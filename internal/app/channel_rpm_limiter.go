package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/qoderauth"
)

type rpmReservation struct {
	allowed    bool
	retryAfter time.Duration
}

type channelRPMExceededError struct {
	retryAfter time.Duration
}

func (e *channelRPMExceededError) Error() string {
	return ErrChannelRPMExceeded.Error()
}

func (e *channelRPMExceededError) Unwrap() error {
	return ErrChannelRPMExceeded
}

type rpmLimiter struct {
	mu       sync.Mutex
	requests map[int64][]time.Time
	now      func() time.Time
}

func newRPMLimiter(now func() time.Time) *rpmLimiter {
	if now == nil {
		now = time.Now
	}
	return &rpmLimiter{
		requests: make(map[int64][]time.Time),
		now:      now,
	}
}

func (l *rpmLimiter) Remove(id int64) {
	if l == nil || id <= 0 {
		return
	}
	l.mu.Lock()
	delete(l.requests, id)
	l.mu.Unlock()
}

func (l *rpmLimiter) CleanupExpired() {
	if l == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := l.now().Add(-time.Minute)

	for id, events := range l.requests {
		kept := 0
		for _, ts := range events {
			if ts.After(cutoff) {
				events[kept] = ts
				kept++
			}
		}
		if kept == 0 {
			delete(l.requests, id)
			continue
		}
		l.requests[id] = events[:kept]
	}
}

func (l *rpmLimiter) reserve(id int64, limit int) rpmReservation {
	if l == nil || id <= 0 || limit <= 0 {
		return rpmReservation{allowed: true}
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	cutoff := now.Add(-time.Minute)

	events := l.requests[id]
	kept := 0
	for _, ts := range events {
		if ts.After(cutoff) {
			events[kept] = ts
			kept++
		}
	}
	events = events[:kept]

	if len(events) >= limit {
		// A lower limit may need several events to expire before a slot opens.
		retryAfter := events[len(events)-limit].Add(time.Minute).Sub(now)
		l.requests[id] = events
		return rpmReservation{allowed: false, retryAfter: retryAfter}
	}

	l.requests[id] = append(events, now)
	return rpmReservation{allowed: true}
}

func (s *Server) reserveChannelRPM(cfg *model.Config) rpmReservation {
	if cfg == nil || cfg.RPMLimit <= 0 {
		return rpmReservation{allowed: true}
	}
	if s == nil || s.channelRPMLimiter == nil {
		return rpmReservation{allowed: true}
	}
	return s.channelRPMLimiter.reserve(cfg.ID, cfg.RPMLimit)
}

func (s *Server) reserveUpstreamRequest(cfg *model.Config) (release func(), err error) {
	release, err = s.acquireChannelConcurrencySlot(cfg)
	if err != nil {
		return nil, err
	}

	reservation := s.reserveChannelRPM(cfg)
	if reservation.allowed {
		return release, nil
	}
	release()
	return nil, &channelRPMExceededError{retryAfter: reservation.retryAfter}
}

func (s *Server) waitForUpstreamRequest(ctx context.Context, cfg *model.Config) (func(), error) {
	for {
		release, err := s.waitForChannelConcurrencySlot(ctx, cfg)
		if err != nil {
			return nil, err
		}

		reservation := s.reserveChannelRPM(cfg)
		if reservation.allowed {
			return release, nil
		}
		release()

		timer := time.NewTimer(reservation.retryAfter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func channelRPMRetryAfter(err error) time.Duration {
	if rpmErr, ok := errors.AsType[*channelRPMExceededError](err); ok {
		return rpmErr.retryAfter
	}
	return 0
}

/**
 * @brief 完成本地套餐选择后快速预留容量并发送上游请求。
 * @param cfg 渠道配置与容量限制。
 * @param req 已构造的出站请求，其上下文控制等待和传输取消。
 * @return 上游或本地套餐响应；非 nil 响应体由调用方关闭。
 * @return 套餐准备、容量预留或传输失败时返回错误，成功时为 nil。
 * @note 预留容量在失败时释放，成功时绑定响应体生命周期。
 */
func (s *Server) doUpstreamRequest(cfg *model.Config, req *http.Request) (*http.Response, error) {
	return s.doUpstreamRequestWithSender(cfg, req, func(outbound *http.Request) (*http.Response, error) {
		release, err := s.reserveUpstreamRequest(cfg)
		return s.doReservedUpstreamRequest(cfg, outbound, release, err)
	})
}

/**
 * @brief 使用已预留的请求额度发送上游请求，并将释放责任绑定到响应体。
 * @param cfg 本次操作使用的渠道配置及私有凭证快照。
 * @param req 已构造且带上下文的出站请求。
 * @param release 预留成功后的释放回调，须与 err 配套传入。
 * @param err 预留阶段错误；非 nil 时不得发送请求或调用 release。
 * @return 返回响应与错误；预留、压缩、传输或 Loomy 响应读取失败时返回错误。
 * @note 预留失败直接返回；取得预留后，失败路径释放，成功路径由调用方关闭 Body 释放。
 */
func (s *Server) doReservedUpstreamRequest(cfg *model.Config, req *http.Request, release func(), err error) (*http.Response, error) {
	if err != nil {
		return nil, err
	}
	reasoningBody := reasoningHTTPRequestBody(req)
	if cfg.UsesAccioOAuth() {
		signed, wireErr := s.prepareAccioWire(cfg, req)
		if wireErr != nil {
			release()
			return nil, wireErr
		}
		if req.Body != nil {
			_ = req.Body.Close()
		}
		req = signed
		captureAccioWireRequest(req)
	}
	logicalReq := req
	if cfg.UsesQoderOAuth() {
		signed, wireErr := s.prepareQoderWire(cfg, req)
		if wireErr != nil {
			if req.Body != nil {
				_ = req.Body.Close()
			}
			release()
			if apiErr, ok := errors.AsType[*qoderauth.APIError](wireErr); ok {
				raw, _ := json.Marshal(map[string]any{"error": apiErr, "status": apiErr.StatusCode})
				return &http.Response{StatusCode: apiErr.StatusCode, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw)), Request: req}, nil
			}
			return nil, wireErr
		}
		if req.Body != nil {
			_ = req.Body.Close()
		}
		req = signed
	}
	encoding, target := s.oauthInferenceEncoding(cfg, req)
	var plainReq *http.Request
	if encoding != "" {
		body, readErr := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if readErr != nil {
			release()
			return nil, readErr
		}
		if len(body) > 0 {
			if s.compressionFallbackActive(target) {
				if target.provider == model.AuthTypeQoderOAuth {
					plainReq = logicalReq
				} else if target.provider != "" {
					plainReq = cloneRequestWithBody(req, body)
				}
			}
			compressRequestBody(req, encoding, body)
		} else {
			req.Body = http.NoBody
		}
	}
	if errCompress := compressAnthropicFirstPartyBody(req); errCompress != nil {
		release()
		return nil, errCompress
	}
	reasoningBusinessSending(req.Context(), reasoningBody)
	resp, err := s.sendInferenceRequest(cfg, req)
	verifyPlain := encoding == "" && target.provider != ""
	onRead, _ := req.Context().Value(inferenceProbeReadKey{}).(func())
	rejected, ambiguous := false, false
	if err == nil && plainReq != nil {
		rejected, ambiguous = inferenceCompressionRejected(cfg, resp, onRead)
	}
	if rejected && req.Context().Err() == nil {
		// Both uploads share a concurrency slot. Each actual POST consumes its own RPM.
		if s.reserveChannelRPM(cfg).allowed {
			_ = resp.Body.Close()
			if cfg.UsesQoderOAuth() {
				// Qoder rejects the previous request ID and COSY signature as a duplicate.
				plainReq, err = s.prepareQoderWire(cfg, logicalReq)
			}
			if err == nil {
				resp, err = s.sendInferenceRequest(cfg, plainReq)
			}
			if err == nil && ambiguous {
				if _, plainAmbiguous := inferenceCompressionRejected(cfg, resp, onRead); plainAmbiguous {
					// The plain upload hit the same rejection, so compression was not the cause.
					s.compressionFallbackPaused.Store(target, time.Now().Add(compressionFallbackPause))
				}
			}
			verifyPlain = true
		} else {
			// Leave the real rejection readable. Try plain on the next admitted request.
			s.plainRequestTargets.LoadOrStore(target, false)
		}
	}
	if err != nil {
		release()
		return nil, err
	}
	if verifyPlain {
		observePlainInferenceResponse(cfg, resp, func() { s.plainRequestTargets.Store(target, true) })
	}
	if cfg.UsesAccioOAuth() {
		modelName, _ := req.Context().Value(accioModelContextKey{}).(string)
		if normalizeErr := normalizeAccioResponse(resp, modelName); normalizeErr != nil {
			_ = resp.Body.Close()
			release()
			return nil, normalizeErr
		}
	}
	resp.Body = &releaseOnCloseReadCloser{ReadCloser: resp.Body, release: release}
	if cfg.UsesLoomySession() {
		if err := normalizeLoomyResponse(resp); err != nil {
			return nil, err
		}
	}
	return resp, nil
}

/**
 * @brief 压缩请求遭 Qoder 流内 403、明文重发也同样被拒后，暂停该目标压缩回退的时长。
 * @details 拒绝与编码无关时（如账号被风控），每个请求都回退会使上游调用翻倍；
 * 到期后恢复探测，以便上游此后真的拒收压缩时仍能回退。
 */
const compressionFallbackPause = 10 * time.Minute

/**
 * @brief 判断压缩被拒时是否探测并以明文重发。
 * @param target 压缩回退目标。
 * @return 未暂停或暂停已到期时为 true。
 * @note 并发安全；到期时删除暂停记录。
 */
func (s *Server) compressionFallbackActive(target requestCompressionTarget) bool {
	until, paused := s.compressionFallbackPaused.Load(target)
	if !paused {
		return true
	}
	if time.Now().Before(until.(time.Time)) {
		return false
	}
	s.compressionFallbackPaused.CompareAndDelete(target, until)
	return true
}

/**
 * @brief 发送一次最终请求，并将 Qoder 拒绝及成功流还原为代理可识别的响应。
 * @param cfg 渠道配置。
 * @param req 已签名、按本次尝试选好编码的请求。
 * @return 返回的响应体由调用方关闭。
 * @return 传输或响应归一化失败时返回错误。
 */
func (s *Server) sendInferenceRequest(cfg *model.Config, req *http.Request) (*http.Response, error) {
	resp, err := s.getClientForChannel(cfg).Do(req)
	if err != nil {
		return nil, err
	}
	if cfg.UsesQoderOAuth() {
		tools, _ := req.Context().Value(qoderToolsContextKey{}).([]map[string]any)
		if normalizeErr := normalizeQoderResponse(resp, tools); normalizeErr != nil {
			_ = resp.Body.Close()
			return nil, normalizeErr
		}
	}
	return resp, nil
}
