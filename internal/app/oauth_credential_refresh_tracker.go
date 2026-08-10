package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
)

var errOAuthCredentialRefreshesClosed = errors.New("OAuth credential refreshes are shutting down")

type oauthCredentialRefreshRedirect struct{}

func oauthCredentialRefreshSingleflightKey(channelID int64, accessToken string, tokenRefresh bool) string {
	if accessToken == "" {
		return fmt.Sprintf("channel:%d", channelID)
	}
	kind := "metadata"
	if tokenRefresh {
		kind = "token"
	}
	fingerprint := sha256.Sum256([]byte(accessToken))
	return fmt.Sprintf("channel:%d:%s:%x", channelID, kind, fingerprint[:8])
}

// oauthCredentialRefreshTracker lets request waiters cancel immediately while
// keeping the shared singleflight refresh owned by the Server lifecycle.
type oauthCredentialRefreshTracker struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	closing bool
	wg      sync.WaitGroup
}

func newOAuthCredentialRefreshTracker() *oauthCredentialRefreshTracker {
	ctx, cancel := context.WithCancel(context.Background())
	return &oauthCredentialRefreshTracker{ctx: ctx, cancel: cancel}
}

func (t *oauthCredentialRefreshTracker) begin() (context.Context, func(), error) {
	if t == nil {
		return context.Background(), func() {}, nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing || t.ctx.Err() != nil {
		return nil, nil, errOAuthCredentialRefreshesClosed
	}
	t.wg.Add(1)
	return t.ctx, t.wg.Done, nil
}

func (t *oauthCredentialRefreshTracker) close(ctx context.Context) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	t.closing = true
	t.mu.Unlock()
	done := make(chan struct{})
	go func() {
		t.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		t.cancel()
		return nil
	case <-ctx.Done():
		// Graceful shutdown has exhausted its budget. Abort the remaining
		// refreshes before the Store is closed underneath them.
		t.cancel()
		return ctx.Err()
	}
}
