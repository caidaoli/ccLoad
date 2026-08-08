package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"

	"golang.org/x/sync/singleflight"
)

const (
	codexCredentialRefreshLead = 5 * time.Minute
	codexUserAgent             = "codex-tui/0.146.0 (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; 0.146.0)"
)

var codexHTTPForwardHeaders = []string{
	"X-Codex-Beta-Features",
	"Version",
	"X-Codex-Turn-Metadata",
	"X-Client-Request-Id",
	"User-Agent",
	"Session_id",
	"Session-Id",
	"Originator",
}

type codexCredentialManager struct {
	mu               sync.RWMutex
	entries          map[int64]*codexauth.Credential
	refreshes        singleflight.Group
	service          *codexauth.Service
	store            storage.Store
	clientFor        func(*model.Config) *http.Client
	invalidateConfig func(int64)
	now              func() time.Time
}

func newCodexCredentialManager(
	service *codexauth.Service,
	store storage.Store,
	clientFor func(*model.Config) *http.Client,
	invalidate func(int64),
) *codexCredentialManager {
	return &codexCredentialManager{
		entries: make(map[int64]*codexauth.Credential), service: service,
		store: store, clientFor: clientFor, invalidateConfig: invalidate, now: time.Now,
	}
}

func (m *codexCredentialManager) credential(ctx context.Context, cfg *model.Config, forceRefresh bool) (*codexauth.Credential, error) {
	if m == nil || m.service == nil || m.store == nil || cfg == nil || !cfg.UsesCodexOAuth() {
		return nil, errors.New("codex credential manager is unavailable")
	}
	credential, err := m.cachedOrParse(cfg)
	if err != nil {
		return nil, err
	}
	needsRefresh, err := credential.NeedsRefresh(m.now(), codexCredentialRefreshLead)
	if err != nil {
		return nil, err
	}
	if !forceRefresh && !needsRefresh {
		return cloneCodexCredential(credential), nil
	}
	forcedAccessToken := credential.AccessToken
	forceRequested := forceRefresh

	value, err, _ := m.refreshes.Do(fmt.Sprintf("channel:%d", cfg.ID), func() (any, error) {
		refreshCtx := context.Background()
		if ctx != nil {
			refreshCtx = context.WithoutCancel(ctx)
		}
		currentCfg, getErr := m.store.GetConfig(refreshCtx, cfg.ID)
		if getErr != nil {
			return nil, fmt.Errorf("reload Codex credential before refresh: %w", getErr)
		}
		previousModelPlanType := ""
		for attempt := 0; attempt < 2; attempt++ {
			current, parseErr := codexauth.ParseCredential([]byte(currentCfg.OAuthCredential))
			if parseErr != nil {
				return nil, fmt.Errorf("parse Codex credential for channel %d: %w", currentCfg.ID, parseErr)
			}
			refreshNeeded, refreshErr := current.NeedsRefresh(m.now(), codexCredentialRefreshLead)
			if refreshErr != nil {
				return nil, refreshErr
			}
			forceCurrent := forceRequested && current.AccessToken == forcedAccessToken
			if !forceCurrent && !refreshNeeded {
				winner, reconcileErr := applyCodexWinnerModelState(
					refreshCtx, m.store, currentCfg, previousModelPlanType, current,
				)
				if reconcileErr != nil {
					return nil, reconcileErr
				}
				m.cache(currentCfg.ID, winner)
				return cloneCodexCredential(winner), nil
			}

			service := *m.service
			if m.clientFor != nil {
				service.Client = m.clientFor(currentCfg)
			}
			refreshed, refreshErr := service.Refresh(refreshCtx, current.RefreshToken)
			if refreshErr != nil {
				return nil, fmt.Errorf("refresh Codex credential for channel %d: %w", currentCfg.ID, refreshErr)
			}
			merged, mergeErr := current.MergeRefresh(refreshed)
			if mergeErr != nil {
				return nil, mergeErr
			}
			payload, encodeErr := merged.JSON()
			if encodeErr != nil {
				return nil, encodeErr
			}
			updated, updateErr := m.store.CompareAndSwapOAuthCredential(
				refreshCtx, currentCfg.ID, model.AuthTypeCodexOAuth, currentCfg.OAuthCredential, payload,
			)
			if updateErr != nil {
				return nil, updateErr
			}
			if !updated {
				winner, getErr := m.store.GetConfig(refreshCtx, currentCfg.ID)
				if getErr != nil {
					return nil, fmt.Errorf("reload Codex credential after concurrent refresh: %w", getErr)
				}
				if attempt == 1 {
					return nil, errors.New("codex credential changed during refresh retry")
				}
				previousModelPlanType = merged.PlanType
				currentCfg = winner
				continue
			}

			persisted, persistErr := persistCodexModelState(
				refreshCtx, m.store, currentCfg, current.PlanType, merged, payload,
			)
			if persistErr != nil {
				return nil, persistErr
			}
			if m.invalidateConfig != nil {
				m.invalidateConfig(currentCfg.ID)
			}
			m.cache(currentCfg.ID, persisted)
			return cloneCodexCredential(persisted), nil
		}
		return nil, errors.New("codex credential refresh retry exhausted")
	})
	if err != nil {
		return nil, err
	}
	return value.(*codexauth.Credential), nil
}

func (m *codexCredentialManager) cache(channelID int64, credential *codexauth.Credential) {
	m.mu.Lock()
	m.entries[channelID] = cloneCodexCredential(credential)
	m.mu.Unlock()
}

func (m *codexCredentialManager) cachedOrParse(cfg *model.Config) (*codexauth.Credential, error) {
	m.mu.RLock()
	credential := m.entries[cfg.ID]
	m.mu.RUnlock()
	if credential != nil {
		return cloneCodexCredential(credential), nil
	}
	parsed, err := codexauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil, fmt.Errorf("parse Codex credential for channel %d: %w", cfg.ID, err)
	}
	m.mu.Lock()
	if existing := m.entries[cfg.ID]; existing != nil {
		parsed = cloneCodexCredential(existing)
	} else {
		m.entries[cfg.ID] = cloneCodexCredential(parsed)
	}
	m.mu.Unlock()
	return parsed, nil
}

func (m *codexCredentialManager) invalidate(channelID int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	delete(m.entries, channelID)
	m.mu.Unlock()
}

func cloneCodexCredential(credential *codexauth.Credential) *codexauth.Credential {
	if credential == nil {
		return nil
	}
	clone := *credential
	return &clone
}

func copyCodexHTTPHeaders(dst, src http.Header) {
	if dst == nil {
		return
	}
	for _, name := range codexHTTPForwardHeaders {
		if value := strings.TrimSpace(src.Get(name)); value != "" {
			dst.Set(name, value)
		}
	}
}

func injectCodexHeaders(req *http.Request, cfg *model.Config, apiKey string, streaming bool) {
	if req == nil || cfg == nil {
		return
	}
	token := apiKey
	if cfg.UsesCodexOAuth() {
		token = cfg.CodexAccessToken
	}
	req.Header.Del("X-Api-Key")
	req.Header.Del("x-goog-api-key")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if streaming {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("Connection", "Keep-Alive")
	req.Header.Set("User-Agent", codexUserAgent)
	req.Header.Set("Originator", "codex-tui")
	if cfg.UsesCodexOAuth() && req.Header.Get("Session_id") == "" && req.Header.Get("Session-Id") == "" {
		req.Header.Set("Session_id", util.NewUUIDv4())
	}
	if cfg.UsesCodexOAuth() && cfg.CodexAccountID != "" {
		req.Header.Set("ChatGPT-Account-ID", cfg.CodexAccountID)
	} else {
		req.Header.Del("ChatGPT-Account-ID")
	}
}
