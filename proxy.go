package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"time"
)

type fwResult struct {
	Status int
	Header http.Header
	Body   []byte         // filled for non-2xx or when needed
	Resp   *http.Response // non-nil only when Status is 2xx to support streaming
}

func (s *Server) forwardOnce(ctx context.Context, cfg *Config, body []byte, hdr http.Header, rawQuery string) (*fwResult, float64, error) {
	startTime := time.Now()

	// Build upstream request (+ ensure beta=true)
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

	resp, err := s.client.Do(req)
	duration := time.Since(startTime).Seconds()

	if err != nil {
		return nil, duration, err
	}
	// If non-2xx, read body for error and close
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rb, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return &fwResult{Status: resp.StatusCode, Header: resp.Header.Clone(), Body: rb, Resp: nil}, duration, nil
	}
	// For success, return resp for streaming
	return &fwResult{Status: resp.StatusCode, Header: resp.Header.Clone(), Resp: resp}, duration, nil
}

// POST /v1/messages
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// Read body bytes (to both parse model and forward unchanged)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	// Parse to extract model (keep raw body for forward)
	var reqModel struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &reqModel); err != nil || reqModel.Model == "" {
		http.Error(w, "invalid JSON or missing model", http.StatusBadRequest)
		return
	}

	// 解析超时
	q := r.URL.Query()
	timeout := parseTimeout(q, r.Header)

	ctx := r.Context()
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	// Build candidate list
	cands, err := s.selectCandidates(ctx, reqModel.Model)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// If no candidates available (all cooled or none support), return 503
	if len(cands) == 0 {
		s.addLogAsync(&LogEntry{Time: time.Now(), Model: reqModel.Model, StatusCode: 503, Message: "no available upstream (all cooled or none)"})
		http.Error(w, "no available upstream (all cooled or none)", http.StatusServiceUnavailable)
		return
	}

	// 代理直连：按候选顺序尝试；成功即流式转发，不主动断开
	var lastStatus int
	var lastBody []byte
	var lastHeader http.Header
	for _, cfg := range cands {
		res, duration, err := s.forwardOnce(ctx, cfg, body, r.Header, r.URL.RawQuery)
		if err != nil {
			// 网络错误：指数退避冷却
			cooldownUntil := time.Now()
			s.cooldownCache.Store(cfg.ID, cooldownUntil)
			_, _ = s.store.BumpCooldownOnError(ctx, cfg.ID, cooldownUntil)
			s.addLogAsync(&LogEntry{Time: time.Now(), Model: reqModel.Model, ChannelID: &cfg.ID, StatusCode: 0, Message: truncateErr(err.Error()), Duration: duration})
			lastStatus = 0
			lastBody = []byte(err.Error())
			lastHeader = nil
			continue
		}
		if res.Status >= 200 && res.Status < 300 && res.Resp != nil {
			// success - stream response transparently
			s.cooldownCache.Delete(cfg.ID)
			_ = s.store.ResetCooldown(ctx, cfg.ID)
			s.addLogAsync(&LogEntry{Time: time.Now(), Model: reqModel.Model, ChannelID: &cfg.ID, StatusCode: res.Status, Message: "ok", Duration: duration})
			for k, vs := range res.Header {
				// avoid hop-by-hop headers
				if strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") {
					continue
				}
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(res.Status)
			// stream copy
			if res.Resp.Body != nil {
				defer res.Resp.Body.Close()
				if fl, ok := w.(http.Flusher); ok {
					buf := make([]byte, 64*1024) // 增大缓冲区到 64KB
					for {
						n, readErr := res.Resp.Body.Read(buf)
						if n > 0 {
							if _, err := w.Write(buf[:n]); err != nil {
								break
							}
							fl.Flush()
						}
						if readErr != nil {
							break
						}
					}
				} else {
					_, _ = io.Copy(w, res.Resp.Body)
				}
			}
			return
		}
		// 非2xx：指数退避冷却并尝试下一个
		cooldownUntil := time.Now()
		s.cooldownCache.Store(cfg.ID, cooldownUntil)
		_, _ = s.store.BumpCooldownOnError(ctx, cfg.ID, cooldownUntil)
		msg := fmt.Sprintf("upstream status %d", res.Status)
		if len(res.Body) > 0 {
			msg = fmt.Sprintf("%s: %s", msg, truncateErr(string(res.Body)))
		}
		s.addLogAsync(&LogEntry{Time: time.Now(), Model: reqModel.Model, ChannelID: &cfg.ID, StatusCode: res.Status, Message: msg, Duration: duration})
		lastStatus = res.Status
		lastBody = res.Body
		lastHeader = res.Header
	}

	// All failed
	s.addLogAsync(&LogEntry{Time: time.Now(), Model: reqModel.Model, StatusCode: 503, Message: "exhausted backends"})
	if lastStatus != 0 {
		// surface last upstream response info
		for k, vs := range lastHeader {
			if strings.EqualFold(k, "Connection") || strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") {
				continue
			}
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write(lastBody)
		return
	}
	http.Error(w, "no upstream available", http.StatusServiceUnavailable)
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
