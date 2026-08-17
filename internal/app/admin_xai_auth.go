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
	xaiCredentialProbeTimeout   = 30 * time.Second
	xaiSSOResponseHeaderTimeout = 30 * time.Second
	maxXAIBillingResponseBytes  = 1 << 20

	xaiImageModelDefault = "grok-imagine-image"
	xaiImageModelQuality = "grok-imagine-image-quality"
	xaiImageModel20      = "grok-imagine-image-2.0"
)

var xaiOAuthDefaultModels = []string{
	"grok-build-0.1",
	"grok-4.6",
	"grok-4.5",
	"grok-4.3",
	"grok-4.20-0309-reasoning",
	"grok-4.20-0309-non-reasoning",
	"grok-4.20-multi-agent-0309",
	"grok-3-mini",
	"grok-3-mini-fast",
	"grok-composer-2.5-fast",
	xaiImageModelDefault,
	xaiImageModelQuality,
	xaiImageModel20,
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

type xaiCredentialImportBatch struct {
	Method            string
	Values            []string
	PriorityIncrement int
	NextPriority      int
}

// HandleStartXAICredentialImportJob starts an import owned by the server
// lifecycle. Losing the progress connection must not cancel credential work.
func (s *Server) HandleStartXAICredentialImportJob(c *gin.Context) {
	started, ok := s.startXAICredentialImportJob(c)
	if !ok {
		return
	}
	RespondJSON(c, http.StatusAccepted, started)
}

// HandleImportXAICredentialsStream streams one background import. A broken SSE
// connection stops only observation; the job remains owned by the server.
func (s *Server) HandleImportXAICredentialsStream(c *gin.Context) {
	started, ok := s.startXAICredentialImportJob(c)
	if !ok {
		return
	}
	s.streamOAuthCredentialImportJob(c, started)
}

func (s *Server) startXAICredentialImportJob(c *gin.Context) (oauthCredentialImportJobStart, bool) {
	batch, status, err := s.prepareXAICredentialImport(c)
	if err != nil {
		RespondError(c, status, err)
		return oauthCredentialImportJobStart{}, false
	}
	started, err := s.ensureOAuthCredentialImportJobs().start(
		len(batch.Values),
		func(ctx context.Context, observer oauthCredentialImportObserver) (oauthCredentialImportSummary, bool) {
			defer wipeXAICredentialImportBatch(batch)
			return s.runXAICredentialImport(ctx, batch, observer)
		},
	)
	if err != nil {
		wipeXAICredentialImportBatch(batch)
		status = http.StatusServiceUnavailable
		if errors.Is(err, errOAuthCredentialImportJobsBusy) {
			status = http.StatusTooManyRequests
		}
		RespondError(c, status, err)
		return oauthCredentialImportJobStart{}, false
	}
	return started, true
}

func (s *Server) prepareXAICredentialImport(c *gin.Context) (*xaiCredentialImportBatch, int, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxOAuthCredentialImportBytes)
	var request xaiCredentialBatchRequest
	if err := BindAndValidate(c, &request); err != nil {
		return nil, http.StatusBadRequest, err
	}
	values := splitXAICredentialBatchValues(request.Values)
	request.Values = ""
	if len(values) == 0 {
		return nil, http.StatusBadRequest, errors.New("xAI credential import requires at least 1 item")
	}
	if s.client == nil || s.store == nil {
		return nil, http.StatusServiceUnavailable, errors.New("xAI credential import is unavailable")
	}
	nextPriority, err := s.nextXAICredentialPriority(c.Request.Context(), request.PriorityIncrement)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return &xaiCredentialImportBatch{
		Method:            request.Method,
		Values:            values,
		PriorityIncrement: request.PriorityIncrement,
		NextPriority:      nextPriority,
	}, 0, nil
}

func wipeXAICredentialImportBatch(batch *xaiCredentialImportBatch) {
	if batch == nil {
		return
	}
	for index := range batch.Values {
		batch.Values[index] = ""
	}
	batch.Values = nil
}

func (s *Server) runXAICredentialImport(
	ctx context.Context,
	batch *xaiCredentialImportBatch,
	observer oauthCredentialImportObserver,
) (oauthCredentialImportSummary, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if batch == nil || len(batch.Values) == 0 {
		return oauthCredentialImportSummary{}, true
	}
	concurrency, itemTimeout := 5, 30*time.Second
	if batch.Method == "sso" {
		concurrency, itemTimeout = 3, xaiauth.SSOConversionTimeout
	}

	batchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	service := xaiauth.NewService(s.client)
	ssoClient := s.xaiSSOClient
	if ssoClient == nil {
		ssoClient = s.client
	}
	ssoService := xaiauth.NewService(ssoClient)
	jobs := make(chan int)
	results := make(chan xaiCredentialBatchItem, concurrency)
	var workers sync.WaitGroup
	for range concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				itemCtx, itemCancel := context.WithTimeout(batchCtx, itemTimeout)
				credential, itemErr := acquireXAICredential(itemCtx, service, ssoService, batch.Method, batch.Values[index])
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
		for index := range batch.Values {
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

	summary := oauthCredentialImportSummary{Results: make([]oauthCredentialImportResult, 0, len(batch.Values))}
	pending := make(map[int]xaiCredentialBatchItem, concurrency)
	nextIndex := 0
	stopped := false
	nextPriority := batch.NextPriority
	for nextIndex < len(batch.Values) && !stopped {
		select {
		case <-batchCtx.Done():
			stopped = true
		case item, ok := <-results:
			if !ok {
				stopped = true
				break
			}
			pending[item.index] = item
			for !stopped {
				ready, exists := pending[nextIndex]
				if !exists {
					break
				}
				delete(pending, nextIndex)
				fileName := fmt.Sprintf("#%d", nextIndex+1)
				if observer != nil && !observer(oauthCredentialImportEvent{
					Event: "processing", Processed: len(summary.Results), Total: len(batch.Values),
					Created: summary.Created, Skipped: summary.Skipped, Failed: summary.Failed, FileName: fileName,
				}) {
					cancel()
					stopped = true
					break
				}
				result := s.persistXAICredentialBatchItem(batchCtx, batch.Method, ready, fileName, nextPriority)
				if result.Status == "created" {
					nextPriority += batch.PriorityIncrement
				}
				appendOAuthCredentialImportResult(&summary, result)
				if observer != nil {
					resultCopy := result
					if !observer(oauthCredentialImportEvent{
						Event: "progress", Processed: len(summary.Results), Total: len(batch.Values),
						Created: summary.Created, Skipped: summary.Skipped, Failed: summary.Failed,
						FileName: fileName, Result: &resultCopy,
					}) {
						cancel()
						stopped = true
						break
					}
				}
				nextIndex++
			}
		}
	}
	cancel()
	workers.Wait()
	if summary.Created > 0 {
		s.InvalidateChannelListCache()
	}
	return summary, !stopped && nextIndex == len(batch.Values)
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

func acquireXAICredential(
	ctx context.Context,
	service *xaiauth.Service,
	ssoService *xaiauth.Service,
	method string,
	value string,
) (*xaiauth.Credential, error) {
	switch method {
	case "refresh_token":
		return service.RefreshToken(ctx, value, "")
	case "sso":
		return ssoService.ConvertSSO(ctx, value)
	default:
		return nil, errors.New("unsupported xAI credential import method")
	}
}

func newXAISSOHTTPClient(base *http.Transport) *http.Client {
	transport := newStandardHTTP11Transport(base)
	transport.ResponseHeaderTimeout = xaiSSOResponseHeaderTimeout
	return &http.Client{Transport: transport, Timeout: xaiauth.SSOConversionTimeout}
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
		if method == "sso" && item.err != nil {
			result.Error = item.err.Error()
		} else if method == "sso" {
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
		if existing, found, updateErr := updateExistingXAIIdentity(ctx, store, configs, credential); found || updateErr != nil {
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
		if existing, found, updateErr := updateExistingXAIIdentity(ctx, store, configs, credential); found || updateErr != nil {
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
) (*model.Config, bool, error) {
	for _, cfg := range configs {
		if cfg == nil || !cfg.UsesXAIOAuth() || strings.TrimSpace(cfg.OAuthCredential) == "" {
			continue
		}
		existing, parseErr := xaiauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil || !sameXAIIdentity(existing, credential) {
			continue
		}
		for {
			currentCfg, getErr := store.GetConfig(ctx, cfg.ID)
			if getErr != nil {
				return nil, true, getErr
			}
			current, parseErr := xaiauth.ParseCredential([]byte(currentCfg.OAuthCredential))
			if parseErr != nil || !currentCfg.UsesXAIOAuth() || !sameXAIIdentity(current, credential) {
				return nil, true, errors.New("xAI credential changed identity during reauthorization")
			}
			merged, mergeErr := current.MergeRefresh(credential)
			if mergeErr != nil {
				return nil, true, mergeErr
			}
			mergedJSON, encodeErr := merged.JSON()
			if encodeErr != nil {
				return nil, true, encodeErr
			}
			updated, updateErr := store.CompareAndSwapOAuthCredential(
				ctx, currentCfg.ID, model.AuthTypeXAIOAuth, currentCfg.OAuthCredential, mergedJSON,
			)
			if updateErr != nil {
				return nil, true, updateErr
			}
			if !updated {
				continue
			}
			persisted, getErr := store.GetConfig(ctx, currentCfg.ID)
			return persisted, true, getErr
		}
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
