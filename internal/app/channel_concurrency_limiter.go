package app

import (
	"context"
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
	mu      sync.Mutex
	active  map[int64]int
	changed map[int64]chan struct{}
}

func newChannelConcurrencyLimiter() *channelConcurrencyLimiter {
	return &channelConcurrencyLimiter{
		active:  make(map[int64]int),
		changed: make(map[int64]chan struct{}),
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

	return l.releaseFunc(channelID), next, limit, true
}

func (l *channelConcurrencyLimiter) acquireContext(ctx context.Context, channelID int64, limit int) (func(), error) {
	if l == nil || channelID <= 0 || limit <= 0 {
		return func() {}, nil
	}

	for {
		l.mu.Lock()
		current := l.active[channelID]
		if current < limit {
			l.active[channelID] = current + 1
			l.mu.Unlock()
			return l.releaseFunc(channelID), nil
		}
		changed := l.changed[channelID]
		if changed == nil {
			changed = make(chan struct{})
			l.changed[channelID] = changed
		}
		l.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (l *channelConcurrencyLimiter) releaseFunc(channelID int64) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			current := l.active[channelID]
			if current <= 1 {
				delete(l.active, channelID)
			} else {
				l.active[channelID] = current - 1
			}
			if changed := l.changed[channelID]; changed != nil {
				close(changed)
				delete(l.changed, channelID)
			}
			l.mu.Unlock()
		})
	}
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

func (s *Server) waitForChannelConcurrencySlot(ctx context.Context, cfg *model.Config) (func(), error) {
	if cfg == nil || cfg.MaxConcurrency <= 0 {
		return func() {}, nil
	}
	if s == nil || s.channelConcurrencyLimiter == nil {
		return func() {}, nil
	}
	return s.channelConcurrencyLimiter.acquireContext(ctx, cfg.ID, cfg.MaxConcurrency)
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
