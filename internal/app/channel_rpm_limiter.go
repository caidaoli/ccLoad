package app

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"ccLoad/internal/model"
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
	var rpmErr *channelRPMExceededError
	if errors.As(err, &rpmErr) {
		return rpmErr.retryAfter
	}
	return 0
}

func (s *Server) doUpstreamRequest(cfg *model.Config, req *http.Request) (*http.Response, error) {
	release, err := s.reserveUpstreamRequest(cfg)
	return s.doReservedUpstreamRequest(cfg, req, release, err)
}

func (s *Server) doReservedUpstreamRequest(cfg *model.Config, req *http.Request, release func(), err error) (*http.Response, error) {
	if err != nil {
		return nil, err
	}
	reasoningBody := reasoningHTTPRequestBody(req)
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
	resp.Body = &releaseOnCloseReadCloser{ReadCloser: resp.Body, release: release}
	return resp, nil
}
