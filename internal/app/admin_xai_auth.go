package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/xaiauth"

	"github.com/gin-gonic/gin"
)

const (
	xaiCredentialProbeTimeout  = 30 * time.Second
	maxXAIBillingResponseBytes = 1 << 20
)

var xaiOAuthDefaultModels = []string{
	"grok-build-0.1",
	"grok-4.5",
	"grok-4.3",
	"grok-4.20-0309-reasoning",
	"grok-4.20-0309-non-reasoning",
	"grok-4.20-multi-agent-0309",
	"grok-3-mini",
	"grok-3-mini-fast",
	"grok-composer-2.5-fast",
}

var xaiChannelCreateMu sync.Mutex

type xaiCredentialBatchRequest struct {
	Method            string `json:"method"`
	Values            string `json:"values"`
	PriorityIncrement int    `json:"priority_increment"`
}

func (r *xaiCredentialBatchRequest) Validate() error {
	r.Method = strings.ToLower(strings.TrimSpace(r.Method))
	if r.Method != "refresh_token" && r.Method != "sso" {
		return errors.New("method must be refresh_token or sso")
	}
	if strings.TrimSpace(r.Values) == "" {
		return errors.New("values are required")
	}
	switch r.PriorityIncrement {
	case 0, 10, 20, 50:
		return nil
	default:
		return errors.New("priority_increment must be one of 0, 10, 20, or 50")
	}
}

type xaiCredentialBatchItem struct {
	index      int
	credential *xaiauth.Credential
	err        error
}

// HandleImportXAICredentialsStream imports refresh tokens or SSO cookies with
// provider-specific bounded concurrency. Secrets are consumed only from the
// request body and are never copied into progress events.
func (s *Server) HandleImportXAICredentialsStream(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxOAuthCredentialImportBytes)
	var request xaiCredentialBatchRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	values := splitXAICredentialBatchValues(request.Values)
	limit, concurrency, itemTimeout := 100, 5, 30*time.Second
	if request.Method == "sso" {
		limit, concurrency, itemTimeout = 10, 3, xaiauth.SSOConversionTimeout
	}
	if len(values) == 0 || len(values) > limit {
		RespondErrorMsg(c, http.StatusBadRequest, fmt.Sprintf("xAI %s import accepts 1..%d items", request.Method, limit))
		return
	}
	if s.client == nil || s.store == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "xAI credential import is unavailable")
		return
	}
	nextPriority, err := s.nextXAICredentialPriority(c.Request.Context(), request.PriorityIncrement)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Status(http.StatusOK)
	if writeOAuthCredentialImportEvent(c, oauthCredentialImportEvent{Event: "start", Total: len(values)}) != nil {
		return
	}

	batchCtx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	service := xaiauth.NewService(s.client)
	jobs := make(chan int)
	results := make(chan xaiCredentialBatchItem, concurrency)
	var workers sync.WaitGroup
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				itemCtx, itemCancel := context.WithTimeout(batchCtx, itemTimeout)
				credential, itemErr := acquireXAICredential(itemCtx, service, request.Method, values[index])
				if itemErr == nil {
					credential, itemErr = completeXAICredential(itemCtx, service, s.client, credential, xaiauth.CLIBaseURL)
				}
				itemCancel()
				select {
				case results <- xaiCredentialBatchItem{index: index, credential: credential, err: itemErr}:
				case <-batchCtx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for index := range values {
			select {
			case jobs <- index:
			case <-batchCtx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(results)
	}()

	summary := oauthCredentialImportSummary{Results: make([]oauthCredentialImportResult, 0, len(values))}
	pending := make(map[int]xaiCredentialBatchItem, concurrency)
	nextIndex := 0
	completed := true
	for nextIndex < len(values) {
		select {
		case <-batchCtx.Done():
			completed = false
			nextIndex = len(values)
		case item, ok := <-results:
			if !ok {
				completed = false
				nextIndex = len(values)
				break
			}
			pending[item.index] = item
			for {
				ready, exists := pending[nextIndex]
				if !exists {
					break
				}
				delete(pending, nextIndex)
				fileName := fmt.Sprintf("#%d", nextIndex+1)
				if writeOAuthCredentialImportEvent(c, oauthCredentialImportEvent{
					Event: "processing", Processed: len(summary.Results), Total: len(values),
					Created: summary.Created, Skipped: summary.Skipped, Failed: summary.Failed, FileName: fileName,
				}) != nil {
					cancel()
					completed = false
					nextIndex = len(values)
					break
				}
				result := s.persistXAICredentialBatchItem(batchCtx, request.Method, ready, fileName, nextPriority)
				if result.Status == "created" {
					nextPriority += request.PriorityIncrement
				}
				appendOAuthCredentialImportResult(&summary, result)
				resultCopy := result
				if writeOAuthCredentialImportEvent(c, oauthCredentialImportEvent{
					Event: "progress", Processed: len(summary.Results), Total: len(values),
					Created: summary.Created, Skipped: summary.Skipped, Failed: summary.Failed,
					FileName: fileName, Result: &resultCopy,
				}) != nil {
					cancel()
					completed = false
					nextIndex = len(values)
					break
				}
				nextIndex++
			}
		}
	}
	if summary.Created > 0 {
		s.InvalidateChannelListCache()
	}
	if completed {
		_ = writeOAuthCredentialImportEvent(c, oauthCredentialImportEvent{
			Event: "complete", Processed: len(summary.Results), Total: len(values),
			Created: summary.Created, Skipped: summary.Skipped, Failed: summary.Failed,
		})
	}
}

func splitXAICredentialBatchValues(raw string) []string {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	values := make([]string, 0, len(lines))
	for _, line := range lines {
		if value := strings.TrimSpace(line); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func acquireXAICredential(ctx context.Context, service *xaiauth.Service, method, value string) (*xaiauth.Credential, error) {
	switch method {
	case "refresh_token":
		return service.RefreshToken(ctx, value, "")
	case "sso":
		return service.ConvertSSO(ctx, value)
	default:
		return nil, errors.New("unsupported xAI credential import method")
	}
}

func (s *Server) nextXAICredentialPriority(ctx context.Context, increment int) (int, error) {
	if increment == 0 {
		return 0, nil
	}
	configs, err := s.store.ListConfigs(ctx)
	if err != nil {
		return 0, fmt.Errorf("list channels for xAI credential priorities: %w", err)
	}
	maximum := 0
	for _, cfg := range configs {
		if cfg != nil && cfg.UsesXAIOAuth() && cfg.Priority > maximum {
			maximum = cfg.Priority
		}
	}
	return maximum + increment, nil
}

func (s *Server) persistXAICredentialBatchItem(
	ctx context.Context,
	method string,
	item xaiCredentialBatchItem,
	fileName string,
	priority int,
) oauthCredentialImportResult {
	result := oauthCredentialImportResult{FileName: fileName}
	if item.err != nil || item.credential == nil {
		result.Status = "failed"
		if method == "sso" {
			result.Error = "xAI SSO import failed"
		} else {
			result.Error = "xAI refresh token import failed"
		}
		return result
	}
	channelName, created, err := createImportedXAIChannel(ctx, s.store, item.credential, priority)
	if err != nil {
		result.Status, result.Error = "failed", "xAI credential persistence failed"
		return result
	}
	result.ChannelName = channelName
	if created {
		result.Status = "created"
	} else {
		result.Status = "skipped"
	}
	return result
}

// completeXAICredential is the single validation boundary used by every xAI
// credential acquisition path. It never persists the credential.
func completeXAICredential(
	ctx context.Context,
	service *xaiauth.Service,
	client *http.Client,
	credential *xaiauth.Credential,
	modelBaseURL string,
) (*xaiauth.Credential, error) {
	if credential == nil {
		return nil, errors.New("complete xAI credential: credential is nil")
	}
	if client == nil {
		return nil, errors.New("complete xAI credential: HTTP client is unavailable")
	}
	if service == nil {
		service = xaiauth.NewService(client)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	completed := *credential
	if err := completed.Normalize(); err != nil {
		return nil, fmt.Errorf("complete xAI credential: %w", err)
	}

	refreshed := false
	needsRefresh, err := completed.NeedsRefresh(time.Now(), xaiauth.RefreshLead)
	if err != nil {
		return nil, fmt.Errorf("complete xAI credential: %w", err)
	}
	if needsRefresh {
		completedPtr, refreshErr := service.Refresh(ctx, &completed)
		if refreshErr != nil {
			return nil, errors.New("complete xAI credential: refresh failed")
		}
		completed = *completedPtr
		refreshed = true
	}

	for {
		classification, metadata, probeErr := probeXAICredentialBilling(ctx, client, &completed, modelBaseURL)
		if probeErr != nil {
			return nil, probeErr
		}
		switch classification {
		case xaiauth.BillingOK, xaiauth.BillingEntitlement, xaiauth.BillingQuota:
			mergeXAIBillingMetadata(&completed, metadata, classification)
			if err := completed.Normalize(); err != nil {
				return nil, fmt.Errorf("complete xAI credential: %w", err)
			}
			return &completed, nil
		case xaiauth.BillingBadCredential:
			if refreshed {
				return nil, errors.New("complete xAI credential: refreshed access token was rejected")
			}
			completedPtr, refreshErr := service.Refresh(ctx, &completed)
			if refreshErr != nil {
				return nil, errors.New("complete xAI credential: refresh failed")
			}
			completed = *completedPtr
			refreshed = true
		case xaiauth.BillingIndeterminate:
			return nil, errors.New("complete xAI credential: billing response was indeterminate")
		default:
			return nil, errors.New("complete xAI credential: unsupported billing response")
		}
	}
}

type xaiBillingMetadata struct {
	SubscriptionTier  string
	EntitlementStatus string
}

func probeXAICredentialBilling(
	ctx context.Context,
	client *http.Client,
	credential *xaiauth.Credential,
	modelBaseURL string,
) (xaiauth.BillingClassification, xaiBillingMetadata, error) {
	if strings.TrimSpace(modelBaseURL) == "" {
		modelBaseURL = xaiauth.CLIBaseURL
	}
	billingURL, err := xaiauth.BillingURL(modelBaseURL, false)
	if err != nil {
		return xaiauth.BillingIndeterminate, xaiBillingMetadata{}, errors.New("complete xAI credential: invalid model base URL")
	}
	probeCtx, cancel := context.WithTimeout(ctx, xaiCredentialProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, billingURL, nil)
	if err != nil {
		return xaiauth.BillingIndeterminate, xaiBillingMetadata{}, errors.New("complete xAI credential: build billing request")
	}
	xaiauth.ApplyBillingHeaders(req, credential.AccessToken)
	resp, err := client.Do(req)
	if err != nil {
		return xaiauth.BillingIndeterminate, xaiBillingMetadata{}, errors.New("complete xAI credential: billing request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxXAIBillingResponseBytes+1))
	if err != nil || len(body) > maxXAIBillingResponseBytes {
		return xaiauth.BillingIndeterminate, xaiBillingMetadata{}, errors.New("complete xAI credential: billing response is invalid")
	}
	classification := xaiauth.ClassifyBillingResponse(resp.StatusCode, resp.Header, body)
	metadata := parseXAIBillingMetadata(body)
	return classification, metadata, nil
}

func parseXAIBillingMetadata(body []byte) xaiBillingMetadata {
	var payload struct {
		SubscriptionTier  string `json:"subscription_tier"`
		EntitlementStatus string `json:"entitlement_status"`
		Subscription      struct {
			Tier string `json:"tier"`
		} `json:"subscription"`
		Entitlement struct {
			Status string `json:"status"`
		} `json:"entitlement"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return xaiBillingMetadata{}
	}
	tier := strings.TrimSpace(payload.SubscriptionTier)
	if tier == "" {
		tier = strings.TrimSpace(payload.Subscription.Tier)
	}
	status := strings.TrimSpace(payload.EntitlementStatus)
	if status == "" {
		status = strings.TrimSpace(payload.Entitlement.Status)
	}
	return xaiBillingMetadata{SubscriptionTier: tier, EntitlementStatus: status}
}

func mergeXAIBillingMetadata(credential *xaiauth.Credential, metadata xaiBillingMetadata, classification xaiauth.BillingClassification) {
	if metadata.SubscriptionTier != "" {
		credential.SubscriptionTier = metadata.SubscriptionTier
	}
	if metadata.EntitlementStatus != "" {
		credential.EntitlementStatus = metadata.EntitlementStatus
	} else if classification == xaiauth.BillingEntitlement || classification == xaiauth.BillingQuota {
		credential.EntitlementStatus = string(classification)
	}
}

func createOrUpdateXAIChannel(ctx context.Context, store storage.Store, credential *xaiauth.Credential) (*model.Config, bool, error) {
	if store == nil || credential == nil {
		return nil, false, errors.New("persist xAI credential: unavailable")
	}
	normalizedCredential := *credential
	credential = &normalizedCredential
	credentialJSON, err := credential.JSON()
	if err != nil {
		return nil, false, err
	}
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("list channels for xAI credential: %w", err)
	}
	identity := credential.Identity()
	if identity.Email != "" || identity.Subject != "" {
		if existing, found, updateErr := updateExistingXAIIdentity(ctx, store, configs, credential, credentialJSON); found || updateErr != nil {
			return existing, false, updateErr
		}
	}

	xaiChannelCreateMu.Lock()
	defer xaiChannelCreateMu.Unlock()
	configs, err = store.ListConfigs(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("reload channels for xAI credential: %w", err)
	}
	if identity.Email != "" || identity.Subject != "" {
		if existing, found, updateErr := updateExistingXAIIdentity(ctx, store, configs, credential, credentialJSON); found || updateErr != nil {
			return existing, false, updateErr
		}
	}
	name := uniqueXAIChannelName(configs, xaiChannelBaseName(credential))
	created, err := store.CreateConfig(ctx, newXAIOAuthChannel(name, credentialJSON))
	if err != nil {
		return nil, false, fmt.Errorf("create xAI channel: %w", err)
	}
	return created, true, nil
}

func updateExistingXAIIdentity(
	ctx context.Context,
	store storage.Store,
	configs []*model.Config,
	credential *xaiauth.Credential,
	credentialJSON string,
) (*model.Config, bool, error) {
	for _, cfg := range configs {
		if cfg == nil || !cfg.UsesXAIOAuth() || strings.TrimSpace(cfg.OAuthCredential) == "" {
			continue
		}
		existing, parseErr := xaiauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil || !sameXAIIdentity(existing, credential) {
			continue
		}
		_, updateErr := store.CompareAndSwapOAuthCredential(
			ctx, cfg.ID, model.AuthTypeXAIOAuth, cfg.OAuthCredential, credentialJSON,
		)
		if updateErr != nil {
			return nil, true, updateErr
		}
		persisted, getErr := store.GetConfig(ctx, cfg.ID)
		return persisted, true, getErr
	}
	return nil, false, nil
}

func createImportedXAIChannel(ctx context.Context, store storage.Store, credential *xaiauth.Credential, priority int) (string, bool, error) {
	if store == nil || credential == nil {
		return "", false, errors.New("persist xAI credential: unavailable")
	}
	normalizedCredential := *credential
	credential = &normalizedCredential
	credentialJSON, err := credential.JSON()
	if err != nil {
		return "", false, err
	}
	xaiChannelCreateMu.Lock()
	defer xaiChannelCreateMu.Unlock()
	configs, err := store.ListConfigs(ctx)
	if err != nil {
		return "", false, fmt.Errorf("list channels for xAI credential: %w", err)
	}
	name := xaiChannelBaseName(credential)
	for _, cfg := range configs {
		if cfg != nil && strings.EqualFold(strings.TrimSpace(cfg.Name), name) {
			return cfg.Name, false, nil
		}
	}
	channel := newXAIOAuthChannel(name, credentialJSON)
	channel.Priority = priority
	created, err := store.CreateConfig(ctx, channel)
	if err != nil {
		return "", false, fmt.Errorf("create xAI channel: %w", err)
	}
	return created.Name, true, nil
}

func sameXAIIdentity(a, b *xaiauth.Credential) bool {
	if a == nil || b == nil {
		return false
	}
	aIdentity, bIdentity := a.Identity(), b.Identity()
	if aIdentity.Subject != "" && bIdentity.Subject != "" {
		return aIdentity.Subject == bIdentity.Subject
	}
	return aIdentity.Email != "" && bIdentity.Email != "" && strings.EqualFold(aIdentity.Email, bIdentity.Email)
}

func xaiChannelBaseName(credential *xaiauth.Credential) string {
	if credential != nil {
		if email := strings.TrimSpace(credential.Identity().Email); email != "" {
			return "xAI-" + email
		}
	}
	return "xAI-OAuth"
}

func uniqueXAIChannelName(configs []*model.Config, base string) string {
	used := make(map[string]struct{}, len(configs))
	for _, cfg := range configs {
		if cfg != nil {
			used[strings.ToLower(strings.TrimSpace(cfg.Name))] = struct{}{}
		}
	}
	if _, exists := used[strings.ToLower(base)]; !exists {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := fmt.Sprintf("%s (%d)", base, suffix)
		if _, exists := used[strings.ToLower(candidate)]; !exists {
			return candidate
		}
	}
}

func newXAIOAuthChannel(name, credentialJSON string) *model.Config {
	models := make([]model.ModelEntry, len(xaiOAuthDefaultModels))
	for i, modelName := range xaiOAuthDefaultModels {
		models[i] = model.ModelEntry{Model: modelName}
	}
	return &model.Config{
		Name: name, AuthType: model.AuthTypeXAIOAuth, OAuthCredential: credentialJSON,
		URLs:                  model.ChannelURLs{{URL: xaiauth.CLIBaseURL, Protocols: []string{"codex"}}},
		ProtocolTransformMode: model.ProtocolTransformModeLocal,
		Priority:              0,
		Enabled:               true,
		CostMultiplier:        1,
		ModelEntries:          models,
	}
}
