package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/vertexauth"

	"golang.org/x/sync/singleflight"
)

const (
	vertexTokenRefreshLead = 5 * time.Minute
	vertexTokenMintTimeout = 30 * time.Second
)

type vertexTokenEntry struct {
	credential string
	token      vertexauth.Token
}

// vertexCredentialManager mints service account access tokens in memory.
// Tokens are never persisted: an entry is valid only for the exact stored
// credential it was minted from, so replacing the key needs no invalidation.
type vertexCredentialManager struct {
	mu        sync.Mutex
	entries   map[int64]vertexTokenEntry
	mints     singleflight.Group
	clientFor func(*model.Config) *http.Client
	now       func() time.Time
}

func newVertexCredentialManager(clientFor func(*model.Config) *http.Client) *vertexCredentialManager {
	return &vertexCredentialManager{entries: make(map[int64]vertexTokenEntry), clientFor: clientFor, now: time.Now}
}

// accessToken returns a cached token or mints a new one. A non-empty
// rejectedAccessToken forces a re-mint unless another request already replaced
// it. On mint failure a still-valid, non-rejected cached token is returned
// together with the error.
func (m *vertexCredentialManager) accessToken(ctx context.Context, cfg *model.Config, rejectedAccessToken string) (string, error) {
	if m == nil || cfg == nil || !cfg.UsesVertexOAuth() {
		return "", errors.New("vertex credential manager is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	now := m.now()
	cached, ok := m.cached(cfg)
	usable := ok && cached.AccessToken != rejectedAccessToken && cached.ExpiresAt.After(now)
	if usable && (rejectedAccessToken != "" || cached.ExpiresAt.After(now.Add(vertexTokenRefreshLead))) {
		return cached.AccessToken, nil
	}
	fallback := ""
	if usable {
		fallback = cached.AccessToken
	}

	credentialHash := sha256.Sum256([]byte(cfg.OAuthCredential))
	// Different rejected tokens must not share a flight that can return cached tokens.
	key := oauthCredentialRefreshSingleflightKey(cfg.ID, rejectedAccessToken, true) + ":" + hex.EncodeToString(credentialHash[:])
	resultCh := m.mints.DoChan(key, func() (any, error) {
		// Another mint may finish between the outer cache read and DoChan.
		now := m.now()
		cached, ok := m.cached(cfg)
		if ok && cached.AccessToken != rejectedAccessToken && cached.ExpiresAt.After(now) &&
			(rejectedAccessToken != "" || cached.ExpiresAt.After(now.Add(vertexTokenRefreshLead))) {
			return cached.AccessToken, nil
		}
		credential, err := vertexauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return nil, fmt.Errorf("parse Vertex credential for channel %d: %w", cfg.ID, err)
		}
		mintCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), vertexTokenMintTimeout)
		defer cancel()
		var client *http.Client
		if m.clientFor != nil {
			client = m.clientFor(cfg)
		}
		token, err := credential.MintAccessToken(mintCtx, client, m.now())
		if err != nil {
			return nil, fmt.Errorf("mint Vertex access token for channel %d: %w", cfg.ID, err)
		}
		m.mu.Lock()
		m.entries[cfg.ID] = vertexTokenEntry{credential: cfg.OAuthCredential, token: *token}
		m.mu.Unlock()
		return token.AccessToken, nil
	})
	select {
	case result := <-resultCh:
		if result.Err != nil {
			return fallback, result.Err
		}
		return result.Val.(string), nil
	case <-ctx.Done():
		return fallback, ctx.Err()
	}
}

func (m *vertexCredentialManager) cached(cfg *model.Config) (vertexauth.Token, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[cfg.ID]
	if !ok || entry.credential != cfg.OAuthCredential {
		return vertexauth.Token{}, false
	}
	return entry.token, true
}

func (m *vertexCredentialManager) invalidate(channelID int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.entries, channelID)
	m.mu.Unlock()
}
