package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	neturl "net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
)

type fwResult struct {
	Status        int
	Header        http.Header
	Body          []byte         // filled for non-2xx or when needed
	Resp          *http.Response // non-nil only when Status is 2xx to support streaming
	FirstByteTime float64        // 首字节响应时间（秒）
	Trace         *traceBreakdown
}

type traceBreakdown struct {
	DNS       float64
	Connect   float64
	TLS       float64
	WroteReq  float64
	FirstByte float64
}

// forwardOnceAsync: 异步流式转发，一旦接收到响应头就立即开始转发
func (s *Server) forwardOnceAsync(ctx context.Context, cfg *Config, body []byte, hdr http.Header, rawQuery string, w http.ResponseWriter) (*fwResult, float64, error) {
	startTime := time.Now()

	// HTTP trace for timing breakdown
	var (
		dnsStart, connStart, tlsStart time.Time
		tDNS, tConn, tTLS, tWrote     float64
	)
	trace := &httptrace.ClientTrace{
		DNSStart: func(info httptrace.DNSStartInfo) { dnsStart = time.Now() },
		DNSDone: func(info httptrace.DNSDoneInfo) {
			if !dnsStart.IsZero() {
				tDNS = time.Since(dnsStart).Seconds()
			}
		},
		ConnectStart: func(network, addr string) { connStart = time.Now() },
		ConnectDone: func(network, addr string, err error) {
			if !connStart.IsZero() {
				tConn = time.Since(connStart).Seconds()
			}
		},
		TLSHandshakeStart: func() { tlsStart = time.Now() },
		TLSHandshakeDone: func(cs tls.ConnectionState, err error) {
			if !tlsStart.IsZero() {
				tTLS = time.Since(tlsStart).Seconds()
			}
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) { tWrote = time.Since(startTime).Seconds() },
	}
	ctx = httptrace.WithClientTrace(ctx, trace)

	// Build upstream request
	base := strings.TrimRight(cfg.URL, "/") + "/v1/messages"
	u, err := neturl.Parse(base)
	if err != nil {
		return nil, 0, err
	}
	// merge incoming query as-is
	if rawQuery != "" {
		// Merge existing + incoming query
		tgt := u.Query()
		src, _ := neturl.ParseQuery(rawQuery)
		for k, vs := range src {
			for _, v := range vs {
				tgt.Add(k, v)
			}
		}
		u.RawQuery = tgt.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	// Copy headers but override API key
	for k, vs := range hdr {
		// Skip hop-by-hop and auth
		if strings.EqualFold(k, "Authorization") || strings.EqualFold(k, "X-Api-Key") {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// Upstream 同时发送 x-api-key 与 Authorization: Bearer
	req.Header.Set("x-api-key", cfg.APIKey)
	req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}

	// 异步发送请求，一旦收到响应头立即开始转发
	resp, err := s.client.Do(req)
	if err != nil {
		duration := time.Since(startTime).Seconds()
		return nil, duration, err
	}

	// 记录首字节响应时间（接收到响应头的时间）
	firstByteTime := time.Since(startTime).Seconds()

	// 克隆响应头用于追踪
	hdrClone := resp.Header.Clone()
	if os.Getenv("CCLOAD_TRACE") == "1" {
		hdrClone.Set("X-Proxy-First-Byte", fmt.Sprintf("%.3f", firstByteTime))
		hdrClone.Set("X-Proxy-Timing", fmt.Sprintf("dns=%.3f,conn=%.3f,tls=%.3f,wrote=%.3f,first=%.3f", tDNS, tConn, tTLS, tWrote, firstByteTime))
	}

	// 如果是错误状态，读取错误体后返回
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rb, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			// 记录读取错误，但仍返回可用部分
			s.addLogAsync(&LogEntry{Time: JSONTime{time.Now()}, Message: fmt.Sprintf("error reading upstream body: %v", readErr)})
		}
		_ = resp.Body.Close()
		duration := time.Since(startTime).Seconds()
		return &fwResult{Status: resp.StatusCode, Header: hdrClone, Body: rb, Resp: nil, FirstByteTime: firstByteTime, Trace: &traceBreakdown{DNS: tDNS, Connect: tConn, TLS: tTLS, WroteReq: tWrote, FirstByte: firstByteTime}}, duration, nil
	}

	// 成功响应：立即写入响应头，开始异步流式转发
	for k, vs := range resp.Header {
		// 跳过hop-by-hop头
		if strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// 启动异步流式传输（管道式）
	var streamErr error

	defer resp.Body.Close()

	// 使用小缓冲区实现低延迟传输，支持ctx取消
	buf := make([]byte, 8*1024) // 8KB缓冲区，平衡延迟与系统调用开销
streamLoop:
	for {
		// 尝试上下文取消
		select {
		case <-ctx.Done():
			streamErr = ctx.Err()
			break streamLoop
		default:
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			// 立即写入
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				streamErr = writeErr
				break streamLoop
			}
			// 如果支持flusher，刷新减少延迟
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				streamErr = readErr
			}
			break streamLoop
		}
	}
	// 已统一到上面的循环，支持ctx取消，无需else分支

	// 计算总传输时间（从startTime开始）
	totalDuration := time.Since(startTime).Seconds()

	// 返回结果，包含流传输信息
	return &fwResult{
		Status:        resp.StatusCode,
		Header:        hdrClone,
		Body:          nil, // 流式传输不保存body
		Resp:          nil, // 已经处理完毕
		FirstByteTime: firstByteTime,
		Trace: &traceBreakdown{
			DNS:       tDNS,
			Connect:   tConn,
			TLS:       tTLS,
			WroteReq:  tWrote,
			FirstByte: firstByteTime,
		},
	}, totalDuration, streamErr
}

// POST /v1/messages - Gin版本
func (s *Server) handleMessages(c *gin.Context) {
	// 全量读取再转发，KISS
	all, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read body"})
		return
	}
	_ = c.Request.Body.Close()
	var reqModel struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := sonic.Unmarshal(all, &reqModel); err != nil || reqModel.Model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid JSON or missing model"})
		return
	}

	// 解析超时
	timeout := parseTimeout(c.Request.URL.Query(), c.Request.Header)

	ctx := c.Request.Context()
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	// Build candidate list
	cands, err := s.selectCandidates(ctx, reqModel.Model)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	// If no candidates available (all cooled or none support), return 503
	if len(cands) == 0 {
		s.addLogAsync(&LogEntry{
			Time:        JSONTime{time.Now()},
			Model:       reqModel.Model,
			StatusCode:  503,
			Message:     "no available upstream (all cooled or none)",
			IsStreaming: reqModel.Stream,
		})
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no available upstream (all cooled or none)"})
		return
	}

	// 异步代理：按候选顺序尝试，使用管道式流转发减少延迟
	var lastStatus int
	var lastBody []byte
	var lastHeader http.Header
	for _, cfg := range cands {
		// 首先尝试异步流式转发（适用于成功响应）
		res, duration, err := s.forwardOnceAsync(ctx, cfg, all, c.Request.Header, c.Request.URL.RawQuery, c.Writer)
		if err != nil {
			// 网络错误：指数退避冷却
			now := time.Now()
			cooldownDur, _ := s.store.BumpCooldownOnError(ctx, cfg.ID, now)
			cooldownUntil := now.Add(cooldownDur)
			s.cooldownCache.Store(cfg.ID, cooldownUntil)
			s.addLogAsync(&LogEntry{
				Time:        JSONTime{time.Now()},
				Model:       reqModel.Model,
				ChannelID:   &cfg.ID,
				StatusCode:  0,
				Message:     truncateErr(err.Error()),
				Duration:    duration,
				IsStreaming: reqModel.Stream,
			})
			lastStatus = 0
			lastBody = []byte(err.Error())
			lastHeader = nil
			continue
		}
		if res.Status >= 200 && res.Status < 300 {
			// 异步流式传输成功完成
			s.cooldownCache.Delete(cfg.ID)
			_ = s.store.ResetCooldown(ctx, cfg.ID)

			// 记录成功日志
			logEntry := &LogEntry{
				Time:        JSONTime{time.Now()},
				Model:       reqModel.Model,
				ChannelID:   &cfg.ID,
				StatusCode:  res.Status,
				Duration:    duration,
				Message:     "ok",
				IsStreaming: reqModel.Stream,
			}

			// 流式请求记录首字节响应时间
			if reqModel.Stream {
				logEntry.FirstByteTime = &res.FirstByteTime
			}

			s.addLogAsync(logEntry)
			return // 成功完成，直接返回
		}
		// 非2xx：指数退避冷却并尝试下一个
		now := time.Now()
		cooldownDur, _ := s.store.BumpCooldownOnError(ctx, cfg.ID, now)
		cooldownUntil := now.Add(cooldownDur)
		s.cooldownCache.Store(cfg.ID, cooldownUntil)
		msg := fmt.Sprintf("upstream status %d", res.Status)
		if len(res.Body) > 0 {
			msg = fmt.Sprintf("%s: %s", msg, truncateErr(string(res.Body)))
		}

		// 记录错误日志
		logEntry := &LogEntry{
			Time:        JSONTime{time.Now()},
			Model:       reqModel.Model,
			ChannelID:   &cfg.ID,
			StatusCode:  res.Status,
			Message:     msg,
			Duration:    duration,
			IsStreaming: reqModel.Stream,
		}
		if reqModel.Stream {
			logEntry.FirstByteTime = &res.FirstByteTime
		}
		s.addLogAsync(logEntry)
		lastStatus = res.Status
		lastBody = res.Body
		lastHeader = res.Header
	}

	// All failed
	s.addLogAsync(&LogEntry{
		Time:        JSONTime{time.Now()},
		Model:       reqModel.Model,
		StatusCode:  503,
		Message:     "exhausted backends",
		IsStreaming: reqModel.Stream,
	})
	if lastStatus != 0 {
		// surface last upstream response info
		for k, vs := range lastHeader {
			if strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") {
				continue
			}
			for _, v := range vs {
				c.Header(k, v)
			}
		}
		c.Data(http.StatusServiceUnavailable, "application/json", lastBody)
		return
	}
	c.JSON(http.StatusServiceUnavailable, gin.H{"error": "no upstream available"})
}

func truncateErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 512 {
		return s[:512]
	}
	return s
}

func parseTimeout(q map[string][]string, h http.Header) time.Duration {
	// 优先 query: timeout_ms / timeout_s
	if v := first(q, "timeout_ms"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	if v := first(q, "timeout_s"); v != "" {
		if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
			return time.Duration(sec) * time.Second
		}
	}
	// header 兜底
	if v := h.Get("x-timeout-ms"); v != "" {
		if ms, err := strconv.Atoi(v); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	if v := h.Get("x-timeout-s"); v != "" {
		if sec, err := strconv.Atoi(v); err == nil && sec > 0 {
			return time.Duration(sec) * time.Second
		}
	}
	return 0
}

func first(q map[string][]string, k string) string {
	if vs, ok := q[k]; ok && len(vs) > 0 {
		return vs[0]
	}
	return ""
}
