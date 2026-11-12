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
	if errCompress := compressCodexOAuthResponsesBody(cfg, req); errCompress != nil {
		release()
		return nil, errCompress
	}
	if errCompress := compressAnthropicFirstPartyBody(req); errCompress != nil {
		release()
		return nil, errCompress
	}
	reasoningBusinessSending(req.Context(), reasoningBody)
	resp, err := s.getClientForChannel(cfg).Do(req)
	if err != nil {
		release()
		return nil, err
	}
	if resp == nil || resp.Body == nil {
		release()
		return resp, nil
	}
	if cfg.UsesAccioOAuth() {
		modelName, _ := req.Context().Value(accioModelContextKey{}).(string)
		if normalizeErr := normalizeAccioResponse(resp, modelName); normalizeErr != nil {
			_ = resp.Body.Close()
			release()
			return nil, normalizeErr
		}
	}
	if cfg.UsesQoderOAuth() {
		tools, _ := req.Context().Value(qoderToolsContextKey{}).([]map[string]any)
		if normalizeErr := normalizeQoderResponse(resp, tools); normalizeErr != nil {
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
