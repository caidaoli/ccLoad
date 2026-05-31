package app

import (
	"errors"
	"io"
	"sync"

	"ccLoad/internal/model"
)

type channelConcurrencyExceededError struct {
	active int
	limit  int
}

func (e *channelConcurrencyExceededError) Error() string {
	return ErrChannelConcurrencyExceeded.Error()
}

func (e *channelConcurrencyExceededError) Unwrap() error {
	return ErrChannelConcurrencyExceeded
}

type channelConcurrencyLimiter struct {
	mu     sync.Mutex
	active map[int64]int
}

func newChannelConcurrencyLimiter() *channelConcurrencyLimiter {
	return &channelConcurrencyLimiter{
		active: make(map[int64]int),
	}
}

func (l *channelConcurrencyLimiter) acquire(channelID int64, limit int) (release func(), active, max int, ok bool) {
	if l == nil || channelID <= 0 || limit <= 0 {
		return func() {}, 0, 0, true
	}

	l.mu.Lock()
	current := l.active[channelID]
	if current >= limit {
		l.mu.Unlock()
		return nil, current, limit, false
	}
	next := current + 1
	l.active[channelID] = next
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			current := l.active[channelID]
			if current <= 1 {
				delete(l.active, channelID)
				return
			}
			l.active[channelID] = current - 1
		})
	}, next, limit, true
}

func (s *Server) acquireChannelConcurrencySlot(cfg *model.Config) (release func(), err error) {
	if cfg == nil || cfg.MaxConcurrency <= 0 {
		return func() {}, nil
	}
	if s == nil || s.channelConcurrencyLimiter == nil {
		return func() {}, nil
	}

	release, active, limit, ok := s.channelConcurrencyLimiter.acquire(cfg.ID, cfg.MaxConcurrency)
	if ok {
		return release, nil
	}
	return nil, &channelConcurrencyExceededError{active: active, limit: limit}
}

type releaseOnCloseReadCloser struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (rc *releaseOnCloseReadCloser) Close() error {
	var closeErr error
	rc.once.Do(func() {
		closeErr = rc.ReadCloser.Close()
		if rc.release != nil {
			rc.release()
		}
	})
	return closeErr
}

func channelConcurrencyLimit(err error) (active, limit int, ok bool) {
	var concurrencyErr *channelConcurrencyExceededError
	if errors.As(err, &concurrencyErr) {
		return concurrencyErr.active, concurrencyErr.limit, true
	}
	return 0, 0, false
}
