package app

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"ccLoad/internal/model"
)

type channelRPMReservation struct {
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

type channelRPMLimiter struct {
	mu       sync.Mutex
	requests map[int64][]time.Time
	now      func() time.Time
}

func newChannelRPMLimiter(now func() time.Time) *channelRPMLimiter {
	if now == nil {
		now = time.Now
	}
	return &channelRPMLimiter{
		requests: make(map[int64][]time.Time),
		now:      now,
	}
}

func (l *channelRPMLimiter) allow(channelID int64, limit int) bool {
	return l.reserve(channelID, limit).allowed
}

func (l *channelRPMLimiter) reserve(channelID int64, limit int) channelRPMReservation {
	if l == nil || channelID <= 0 || limit <= 0 {
		return channelRPMReservation{allowed: true}
	}

	now := l.now()
	cutoff := now.Add(-time.Minute)

	l.mu.Lock()
	defer l.mu.Unlock()

	events := l.requests[channelID]
	kept := 0
	for _, ts := range events {
		if ts.After(cutoff) {
			events[kept] = ts
			kept++
		}
	}
	events = events[:kept]

	if len(events) >= limit {
		retryAfter := time.Minute
		if len(events) > 0 {
			retryAfter = events[0].Add(time.Minute).Sub(now)
			if retryAfter < 0 {
				retryAfter = 0
			}
		}
		if len(events) == 0 {
			delete(l.requests, channelID)
		} else {
			l.requests[channelID] = events
		}
		return channelRPMReservation{allowed: false, retryAfter: retryAfter}
	}

	l.requests[channelID] = append(events, now)
	return channelRPMReservation{allowed: true}
}

func (s *Server) reserveChannelRPM(cfg *model.Config) channelRPMReservation {
	if cfg == nil || cfg.RPMLimit <= 0 {
		return channelRPMReservation{allowed: true}
	}
	if s == nil || s.channelRPMLimiter == nil {
		return channelRPMReservation{allowed: true}
	}
	return s.channelRPMLimiter.reserve(cfg.ID, cfg.RPMLimit)
}

func (s *Server) reserveUpstreamRequest(cfg *model.Config) error {
	reservation := s.reserveChannelRPM(cfg)
	if reservation.allowed {
		return nil
	}
	return &channelRPMExceededError{retryAfter: reservation.retryAfter}
}

func channelRPMRetryAfter(err error) time.Duration {
	var rpmErr *channelRPMExceededError
	if errors.As(err, &rpmErr) {
		return rpmErr.retryAfter
	}
	return 0
}

func (s *Server) doUpstreamRequest(cfg *model.Config, req *http.Request) (*http.Response, error) {
	if err := s.reserveUpstreamRequest(cfg); err != nil {
		return nil, err
	}
	return s.client.Do(req)
}
