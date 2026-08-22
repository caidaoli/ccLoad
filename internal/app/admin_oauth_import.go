package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
	"ccLoad/internal/oauthcost"
	"ccLoad/internal/xaiauth"

	"github.com/gin-gonic/gin"
)

const (
	oauthCredentialProviderAuto       = "auto"
	oauthCredentialUnknownTypeMessage = "credential type could not be determined"
	oauthCredentialImportWorkers      = 8
	codexCredentialProbeAttempts      = 3
	codexCredentialProbeRetryDelay    = 200 * time.Millisecond
)

var errOAuthCredentialUnusable = errors.New("OAuth credential could not obtain a usable access token")

type oauthCredentialImportResult struct {
	FileName    string `json:"file_name"`
	ChannelName string `json:"channel_name,omitempty"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
}

type oauthCredentialImportSummary struct {
	Created int                           `json:"created"`
	Skipped int                           `json:"skipped"`
	Failed  int                           `json:"failed"`
	Results []oauthCredentialImportResult `json:"results"`
}

type oauthCredentialImportBatch struct {
	Files                  []oauthCredentialImportFile
	Provider               string
	PriorityIncrement      int
	NextPriorityByProvider map[string]int
}

type preparedOAuthCredentialImport struct {
	Index       int
	Provider    string
	ChannelName string
	Config      *model.Config
	Result      oauthCredentialImportResult
}

type oauthCredentialImportEvent struct {
	Event     string                       `json:"event"`
	JobID     string                       `json:"job_id,omitempty"`
	Processed int                          `json:"processed"`
	Total     int                          `json:"total"`
	Created   int                          `json:"created"`
	Skipped   int                          `json:"skipped"`
	Failed    int                          `json:"failed"`
	FileName  string                       `json:"file_name,omitempty"`
	Result    *oauthCredentialImportResult `json:"result,omitempty"`
}

type oauthCredentialImportObserver func(oauthCredentialImportEvent) bool

func normalizeOAuthCredentialProvider(provider string) (string, error) {
	switch normalized := strings.ToLower(strings.TrimSpace(provider)); normalized {
	case "", oauthCredentialProviderAuto:
		return oauthCredentialProviderAuto, nil
	case codexauth.ChannelType, antigravityauth.ChannelType, xaiauth.ChannelType, anthropicauth.ChannelType:
		return normalized, nil
	default:
		return "", fmt.Errorf("unsupported credential provider %q", normalized)
	}
}

func parseOAuthPriorityIncrement(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	increment, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, errors.New("priority_increment must be one of 0, 10, 20, or 50")
	}
	switch increment {
	case 0, 10, 20, 50:
		return increment, nil
	default:
		return 0, errors.New("priority_increment must be one of 0, 10, 20, or 50")
	}
}

func decodeOAuthCredentialFields(raw []byte) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if err := decoder.Decode(&fields); err != nil {
		return nil, fmt.Errorf("decode credential: %w", err)
	}
	if fields == nil {
		return nil, errors.New("credential must be a JSON object")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("credential contains trailing JSON")
	}
	return fields, nil
}

func parseOAuthCredentialPriority(raw []byte) (int, error) {
	fields, err := decodeOAuthCredentialFields(raw)
	if err != nil {
		return 0, err
	}
	rawPriority, exists := fields["priority"]
	if !exists || string(rawPriority) == "null" {
		return 0, nil
	}
	var priority int
	if err := json.Unmarshal(rawPriority, &priority); err == nil {
		return priority, nil
	}
	var priorityString string
	if err := json.Unmarshal(rawPriority, &priorityString); err == nil {
		if parsed, parseErr := strconv.Atoi(strings.TrimSpace(priorityString)); parseErr == nil {
			return parsed, nil
		}
	}
	return 0, errors.New("credential priority must be an integer")
}

func detectOAuthCredentialProvider(raw []byte) (string, error) {
	fields, err := decodeOAuthCredentialFields(raw)
	if err != nil {
		return "", err
	}

	if rawType, exists := fields["type"]; exists {
		var credentialType string
		if err := json.Unmarshal(rawType, &credentialType); err != nil {
			return "", errors.New("credential type must be a string")
		}
		switch normalized := strings.ToLower(strings.TrimSpace(credentialType)); normalized {
		case codexauth.ChannelType, antigravityauth.ChannelType, xaiauth.ChannelType, anthropicauth.ChannelType:
			return normalized, nil
		case "claude":
			return anthropicauth.ChannelType, nil
		case "", "oauth":
			// Empty and omitted types use the same field-based inference.
		default:
			return "", nil
		}
	}

	codexFields := hasAnyJSONField(fields, "account_id", "plan_type")
	antigravityFields := hasAnyJSONField(fields, "project_id", "timestamp")
	xaiFields := hasStrongXAIImportMarker(fields)
	anthropicFields := hasStrongAnthropicImportMarker(fields)
	providerCount := 0
	for _, matched := range []bool{codexFields, antigravityFields, xaiFields, anthropicFields} {
		if matched {
			providerCount++
		}
	}
	if providerCount != 1 {
		return "", nil
	}
	if codexFields {
		return codexauth.ChannelType, nil
	}
	if antigravityFields {
		return antigravityauth.ChannelType, nil
	}
	if anthropicFields {
		return anthropicauth.ChannelType, nil
	}
	return xaiauth.ChannelType, nil
}

func hasStrongAnthropicImportMarker(fields map[string]json.RawMessage) bool {
	return hasAnyJSONField(fields, "org_uuid", "account_uuid", "email_address")
}

func hasStrongXAIImportMarker(fields map[string]json.RawMessage) bool {
	readString := func(name string) string {
		var value string
		if raw, ok := fields[name]; ok {
			_ = json.Unmarshal(raw, &value)
		}
		return strings.TrimSpace(value)
	}
	if readString("client_id") == xaiauth.ClientID {
		return true
	}
	if endpoint := readString("token_endpoint"); endpoint != "" {
		parsed, err := url.Parse(endpoint)
		if err == nil && parsed.Scheme == "https" && strings.EqualFold(parsed.Host, "auth.x.ai") && parsed.User == nil {
			return true
		}
	}
	if baseURL := readString("base_url"); baseURL != "" {
		parsed, err := url.Parse(baseURL)
		if err == nil && parsed.Scheme == "https" && parsed.User == nil {
			host := strings.ToLower(parsed.Hostname())
			return host == "cli-chat-proxy.grok.com" || host == "api.x.ai"
		}
	}
	return false
}

func hasAnyJSONField(fields map[string]json.RawMessage, names ...string) bool {
	for _, name := range names {
		if _, exists := fields[name]; exists {
			return true
		}
	}
	return false
}

// HandleImportOAuthCredentials imports mixed OAuth credential files. The
// provider form field defaults to automatic detection.
func (s *Server) HandleImportOAuthCredentials(c *gin.Context) {
	s.handleImportOAuthCredentials(c, "")
}

func (s *Server) handleImportOAuthCredentials(c *gin.Context, forcedProvider string) {
	batch, status, err := s.prepareOAuthCredentialImport(c, forcedProvider)
	if err != nil {
		RespondError(c, status, err)
		return
	}
	defer wipeOAuthCredentialImportBatch(batch)
	summary, _ := s.runOAuthCredentialImport(c.Request.Context(), batch, nil)
	RespondJSON(c, http.StatusOK, summary)
}

func (s *Server) prepareOAuthCredentialImport(c *gin.Context, forcedProvider string) (*oauthCredentialImportBatch, int, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxOAuthCredentialImportRequestBytes)
	reader, err := c.Request.MultipartReader()
	if err != nil {
		return nil, http.StatusBadRequest, errors.New("credential multipart form is required")
	}
	credentialFiles, values, err := parseOAuthCredentialMultipart(reader)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	if len(credentialFiles) == 0 {
		return nil, http.StatusBadRequest, errors.New("credential files are required")
	}

	providerValue := forcedProvider
	if providerValue == "" {
		providerValue = values["provider"]
	}
	provider, err := normalizeOAuthCredentialProvider(providerValue)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	priorityIncrement, err := parseOAuthPriorityIncrement(values["priority_increment"])
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	credentialFiles, err = expandOAuthCredentialContainers(credentialFiles, provider)
	if err != nil {
		return nil, http.StatusBadRequest, err
	}
	nextPriorityByProvider := map[string]int{
		codexauth.ChannelType:       0,
		antigravityauth.ChannelType: 0,
		xaiauth.ChannelType:         0,
		anthropicauth.ChannelType:   0,
	}
	if priorityIncrement > 0 {
		configs, listErr := s.store.ListConfigs(c.Request.Context())
		if listErr != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("list channels for OAuth credential priorities: %w", listErr)
		}
		for _, cfg := range configs {
			if cfg == nil {
				continue
			}
			switch {
			case cfg.UsesCodexOAuth() && cfg.Priority > nextPriorityByProvider[codexauth.ChannelType]:
				nextPriorityByProvider[codexauth.ChannelType] = cfg.Priority
			case cfg.UsesAntigravityOAuth() && cfg.Priority > nextPriorityByProvider[antigravityauth.ChannelType]:
				nextPriorityByProvider[antigravityauth.ChannelType] = cfg.Priority
			case cfg.UsesXAIOAuth() && cfg.Priority > nextPriorityByProvider[xaiauth.ChannelType]:
				nextPriorityByProvider[xaiauth.ChannelType] = cfg.Priority
			case cfg.UsesAnthropicOAuth() && cfg.Priority > nextPriorityByProvider[anthropicauth.ChannelType]:
				nextPriorityByProvider[anthropicauth.ChannelType] = cfg.Priority
			}
		}
		for credentialProvider := range nextPriorityByProvider {
			nextPriorityByProvider[credentialProvider] += priorityIncrement
		}
	}
	return &oauthCredentialImportBatch{
		Files:                  credentialFiles,
		Provider:               provider,
		PriorityIncrement:      priorityIncrement,
		NextPriorityByProvider: nextPriorityByProvider,
	}, 0, nil
}

func (s *Server) runOAuthCredentialImport(
	ctx context.Context,
	batch *oauthCredentialImportBatch,
	observer oauthCredentialImportObserver,
) (oauthCredentialImportSummary, bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.oauthCredentialImportRunMu.Lock()
	defer s.oauthCredentialImportRunMu.Unlock()

	summary := oauthCredentialImportSummary{Results: make([]oauthCredentialImportResult, 0, len(batch.Files))}
	if len(batch.Files) == 0 {
		return summary, true
	}

	configs, err := s.store.ListConfigs(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return summary, false
		}
		completed := s.failOAuthCredentialImportBatch(
			batch,
			observer,
			&summary,
			fmt.Errorf("list channels for OAuth credential import: %w", err),
		)
		return summary, completed
	}
	initialNames := oauthCredentialChannelNames(configs)
	committedNames := make(map[string]string, len(initialNames)+len(batch.Files))
	for normalized, name := range initialNames {
		committedNames[normalized] = name
	}

	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	workerCount := min(oauthCredentialImportWorkers, len(batch.Files))
	jobs := make(chan int, len(batch.Files))
	prepared := make(chan preparedOAuthCredentialImport, workerCount)
	for index := range batch.Files {
		jobs <- index
	}
	close(jobs)

	var workers sync.WaitGroup
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for index := range jobs {
				if workerCtx.Err() != nil {
					return
				}
				result := s.prepareOAuthCredentialImportFile(
					workerCtx,
					batch,
					index,
					initialNames,
				)
				select {
				case prepared <- result:
				case <-workerCtx.Done():
					return
				}
			}
		}()
	}
	go func() {
		workers.Wait()
		close(prepared)
	}()

	next := 0
	pending := make(map[int]preparedOAuthCredentialImport, workerCount)
	stopped := false
	for item := range prepared {
		if stopped {
			continue
		}
		if ctx.Err() != nil {
			stopped = true
			cancelWorkers()
			continue
		}
		pending[item.Index] = item
		for {
			current, ready := pending[next]
			if !ready {
				break
			}
			delete(pending, next)
			if observer != nil && !observer(oauthCredentialImportEvent{
				Event:     "processing",
				Processed: len(summary.Results),
				Total:     len(batch.Files),
				Created:   summary.Created,
				Skipped:   summary.Skipped,
				Failed:    summary.Failed,
				FileName:  current.Result.FileName,
			}) {
				stopped = true
				cancelWorkers()
				break
			}

			result := s.commitPreparedOAuthCredentialImport(ctx, batch, current, committedNames)
			appendOAuthCredentialImportResult(&summary, result)
			next++
			if observer != nil {
				resultCopy := result
				if !observer(oauthCredentialImportEvent{
					Event:     "progress",
					Processed: len(summary.Results),
					Total:     len(batch.Files),
					Created:   summary.Created,
					Skipped:   summary.Skipped,
					Failed:    summary.Failed,
					FileName:  result.FileName,
					Result:    &resultCopy,
				}) {
					stopped = true
					cancelWorkers()
					break
				}
			}
		}
	}
	if summary.Created > 0 {
		s.InvalidateChannelListCache()
	}
	return summary, !stopped && next == len(batch.Files)
}

func (s *Server) prepareOAuthCredentialImportFile(
	ctx context.Context,
	batch *oauthCredentialImportBatch,
	index int,
	existingNames map[string]string,
) preparedOAuthCredentialImport {
	file := batch.Files[index]
	result := oauthCredentialImportResult{FileName: file.FileName}
	prepared := preparedOAuthCredentialImport{Index: index, Result: result}
	if file.Err != nil {
		prepared.Result.Status, prepared.Result.Error = "failed", file.Err.Error()
		return prepared
	}

	credentialProvider := batch.Provider
	if credentialProvider == oauthCredentialProviderAuto {
		detectedProvider, err := detectOAuthCredentialProvider(file.Raw)
		if err != nil {
			prepared.Result.Status, prepared.Result.Error = "failed", err.Error()
			return prepared
		}
		if detectedProvider == "" {
			prepared.Result.Status, prepared.Result.Error = "skipped", oauthCredentialUnknownTypeMessage
			return prepared
		}
		credentialProvider = detectedProvider
	}
	prepared.Provider = credentialProvider

	switch credentialProvider {
	case codexauth.ChannelType:
		credential, err := codexauth.ParseCredential(file.Raw)
		if err != nil {
			prepared.Result.Status, prepared.Result.Error = "failed", err.Error()
			return prepared
		}
		if !credential.IsPersonalAccessToken() {
			if existingName, exists := existingNames[normalizeOAuthCredentialChannelName(codexChannelBaseName(credential))]; exists {
				prepared.Result.Status, prepared.Result.ChannelName = "skipped", existingName
				return prepared
			}
		}
		service := s.codexService
		if service == nil && s.client != nil {
			service = codexauth.NewService(s.client)
		}
		credential, err = completeImportedCodexCredential(ctx, service, credential)
		if err != nil {
			prepared.Result.Status, prepared.Result.Error = "failed", err.Error()
			return prepared
		}
		if existingName, exists := existingNames[normalizeOAuthCredentialChannelName(codexChannelBaseName(credential))]; exists {
			prepared.Result.Status, prepared.Result.ChannelName = "skipped", existingName
			return prepared
		}
		credentialJSON, err := credential.JSON()
		if err != nil {
			prepared.Result.Status, prepared.Result.Error = "failed", err.Error()
			return prepared
		}
		prepared.ChannelName = codexChannelBaseName(credential)
		prepared.Config = newCodexOAuthChannel(prepared.ChannelName, credentialJSON, credential.PlanType)
	case antigravityauth.ChannelType:
		credential, err := antigravityauth.ParseCredential(file.Raw)
		if err != nil {
			prepared.Result.Status, prepared.Result.Error = "failed", err.Error()
			return prepared
		}
		if existingName, exists := existingNames[normalizeOAuthCredentialChannelName(antigravityChannelBaseName(credential))]; exists {
			prepared.Result.Status, prepared.Result.ChannelName = "skipped", existingName
			return prepared
		}
		if s.antigravityService == nil {
			prepared.Result.Status, prepared.Result.Error = "failed", "antigravity credential completion is unavailable"
			return prepared
		}
		credential, err = s.antigravityService.CompleteCredential(ctx, credential)
		if err != nil {
			prepared.Result.Status, prepared.Result.Error = "failed", err.Error()
			return prepared
		}
		credentialJSON, err := credential.JSON()
		if err != nil {
			prepared.Result.Status, prepared.Result.Error = "failed", err.Error()
			return prepared
		}
		prepared.ChannelName = antigravityChannelBaseName(credential)
		prepared.Config = newAntigravityOAuthChannel(prepared.ChannelName, credentialJSON)
	case xaiauth.ChannelType:
		credential, err := xaiauth.ParseCredential(file.Raw)
		if err != nil {
			prepared.Result.Status, prepared.Result.Error = "failed", err.Error()
			return prepared
		}
		if existingName, exists := existingNames[normalizeOAuthCredentialChannelName(xaiChannelBaseName(credential))]; exists {
			prepared.Result.Status, prepared.Result.ChannelName = "skipped", existingName
			return prepared
		}
		if s.client == nil {
			prepared.Result.Status, prepared.Result.Error = "failed", "xAI credential completion is unavailable"
			return prepared
		}
		credential, err = completeXAICredential(
			ctx, xaiauth.NewService(s.client), s.client, credential, xaiauth.CLIBaseURL,
		)
		if err != nil {
			prepared.Result.Status, prepared.Result.Error = "failed", err.Error()
			return prepared
		}
		credentialJSON, err := credential.JSON()
		if err != nil {
			prepared.Result.Status, prepared.Result.Error = "failed", err.Error()
			return prepared
		}
		prepared.ChannelName = xaiChannelBaseName(credential)
		prepared.Config = newXAIOAuthChannel(prepared.ChannelName, credentialJSON)
	case anthropicauth.ChannelType:
		credential, err := anthropicauth.ParseCredential(file.Raw)
		if err != nil {
			prepared.Result.Status, prepared.Result.Error = "failed", err.Error()
			return prepared
		}
		baseName := uniqueAnthropicChannelName(nil, credential)
		if existingName, exists := existingNames[normalizeOAuthCredentialChannelName(baseName)]; exists {
			prepared.Result.Status, prepared.Result.ChannelName = "skipped", existingName
			return prepared
		}
		needsRefresh, err := credential.NeedsRefresh(time.Now(), anthropicauth.CredentialRefreshLead)
		if err != nil {
			prepared.Result.Status, prepared.Result.Error = "failed", err.Error()
			return prepared
		}
		if needsRefresh {
			if s.anthropicService == nil {
				prepared.Result.Status, prepared.Result.Error = "failed", "Anthropic credential refresh is unavailable"
				return prepared
			}
			refreshed, refreshErr := s.anthropicService.Refresh(ctx, credential.RefreshToken)
			if refreshErr != nil {
				prepared.Result.Status, prepared.Result.Error = "failed", refreshErr.Error()
				return prepared
			}
			credential, err = credential.MergeRefresh(refreshed)
			if err != nil {
				prepared.Result.Status, prepared.Result.Error = "failed", err.Error()
				return prepared
			}
		}
		credentialJSON, err := credential.JSON()
		if err != nil {
			prepared.Result.Status, prepared.Result.Error = "failed", err.Error()
			return prepared
		}
		prepared.ChannelName = baseName
		prepared.Config = newAnthropicOAuthChannel(prepared.ChannelName, credentialJSON)
	default:
		prepared.Result.Status = "failed"
		prepared.Result.Error = fmt.Sprintf("unsupported credential provider %q", credentialProvider)
	}
	return prepared
}

func (s *Server) commitPreparedOAuthCredentialImport(
	ctx context.Context,
	batch *oauthCredentialImportBatch,
	prepared preparedOAuthCredentialImport,
	committedNames map[string]string,
) oauthCredentialImportResult {
	if prepared.Result.Status != "" {
		return prepared.Result
	}
	normalizedName := normalizeOAuthCredentialChannelName(prepared.ChannelName)
	if existingName, exists := committedNames[normalizedName]; exists {
		prepared.Result.Status, prepared.Result.ChannelName = "skipped", existingName
		return prepared.Result
	}
	prepared.Config.Priority = batch.NextPriorityByProvider[prepared.Provider]
	created, err := s.store.CreateConfig(ctx, prepared.Config)
	if err != nil {
		prepared.Result.Status = "failed"
		prepared.Result.Error = fmt.Sprintf("create %s channel: %v", oauthCredentialProviderLabel(prepared.Provider), err)
		return prepared.Result
	}
	prepared.Result.Status, prepared.Result.ChannelName = "created", created.Name
	committedNames[normalizedName] = created.Name
	batch.NextPriorityByProvider[prepared.Provider] += batch.PriorityIncrement
	return prepared.Result
}

func oauthCredentialProviderLabel(provider string) string {
	if provider == codexauth.ChannelType {
		return "Codex"
	}
	if provider == antigravityauth.ChannelType {
		return "Antigravity"
	}
	if provider == xaiauth.ChannelType {
		return "xAI"
	}
	if provider == anthropicauth.ChannelType {
		return "Anthropic"
	}
	return provider
}

func oauthCredentialChannelNames(configs []*model.Config) map[string]string {
	names := make(map[string]string, len(configs))
	for _, cfg := range configs {
		if cfg == nil {
			continue
		}
		name := strings.TrimSpace(cfg.Name)
		names[normalizeOAuthCredentialChannelName(name)] = name
	}
	return names
}

func normalizeOAuthCredentialChannelName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func (s *Server) failOAuthCredentialImportBatch(
	batch *oauthCredentialImportBatch,
	observer oauthCredentialImportObserver,
	summary *oauthCredentialImportSummary,
	err error,
) bool {
	for _, file := range batch.Files {
		if observer != nil && !observer(oauthCredentialImportEvent{
			Event:     "processing",
			Processed: len(summary.Results),
			Total:     len(batch.Files),
			Created:   summary.Created,
			Skipped:   summary.Skipped,
			Failed:    summary.Failed,
			FileName:  file.FileName,
		}) {
			return false
		}
		result := oauthCredentialImportResult{FileName: file.FileName, Status: "failed", Error: err.Error()}
		appendOAuthCredentialImportResult(summary, result)
		if observer != nil {
			resultCopy := result
			if !observer(oauthCredentialImportEvent{
				Event:     "progress",
				Processed: len(summary.Results),
				Total:     len(batch.Files),
				Created:   summary.Created,
				Skipped:   summary.Skipped,
				Failed:    summary.Failed,
				FileName:  file.FileName,
				Result:    &resultCopy,
			}) {
				return false
			}
		}
	}
	return true
}

func appendOAuthCredentialImportResult(summary *oauthCredentialImportSummary, result oauthCredentialImportResult) {
	switch result.Status {
	case "created":
		summary.Created++
	case "skipped":
		summary.Skipped++
	default:
		summary.Failed++
	}
	summary.Results = append(summary.Results, result)
}

func writeOAuthCredentialImportEvent(c *gin.Context, event oauthCredentialImportEvent) error {
	return writeSSEEvent(c, event.Event, event)
}

func writeSSEEvent(c *gin.Context, eventName string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", eventName, raw); err != nil {
		return err
	}
	c.Writer.Flush()
	return nil
}

func completeImportedCodexCredential(
	ctx context.Context,
	service *codexauth.Service,
	credential *codexauth.Credential,
) (*codexauth.Credential, error) {
	if credential == nil {
		return nil, errors.New("codex credential is nil")
	}
	if service == nil || service.Client == nil {
		return nil, errors.New("codex credential validation is unavailable")
	}
	if credential.IsPersonalAccessToken() {
		validated, validateErr := service.ValidatePersonalAccessToken(ctx, credential.AccessToken)
		if validateErr != nil {
			return nil, fmt.Errorf("%w: Codex personal access token validation failed: %v", errOAuthCredentialUnusable, validateErr)
		}
		validated.PassiveUsage = codexauth.ClonePassiveUsage(credential.PassiveUsage)
		validated.OAuthUsage = append(json.RawMessage(nil), credential.OAuthUsage...)
		validated.QuotaCostUsage = oauthcost.Clone(credential.QuotaCostUsage)
		validated.QuotaOverdraft = codexauth.CloneQuotaOverdraft(credential.QuotaOverdraft)
		return validated, nil
	}

	needsRefresh, err := credential.NeedsRefresh(time.Now(), codexCredentialRefreshLead)
	if err != nil {
		return nil, err
	}
	if !needsRefresh {
		accepted, probeErr := probeCodexAccessToken(ctx, service.Client, credential)
		if probeErr != nil {
			return nil, probeErr
		}
		if accepted {
			return credential, nil
		}
	}

	refreshed, err := service.Refresh(ctx, credential.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("%w: Codex refresh failed: %v", errOAuthCredentialUnusable, err)
	}
	merged, err := credential.MergeRefresh(refreshed)
	if err != nil {
		return nil, fmt.Errorf("%w: Codex refresh response was invalid: %v", errOAuthCredentialUnusable, err)
	}
	accepted, probeErr := probeCodexAccessToken(ctx, service.Client, merged)
	if probeErr != nil {
		return nil, fmt.Errorf("%w: refreshed Codex access token validation failed: %v", errOAuthCredentialUnusable, probeErr)
	}
	if !accepted {
		return nil, fmt.Errorf("%w: refreshed Codex access token was not accepted", errOAuthCredentialUnusable)
	}
	return merged, nil
}

func probeCodexAccessToken(ctx context.Context, client *http.Client, credential *codexauth.Credential) (bool, error) {
	if client == nil || credential == nil || strings.TrimSpace(credential.AccessToken) == "" {
		return false, errors.New("codex credential validation is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	probeCtx, cancel := context.WithTimeout(ctx, oauthUsageTimeout)
	defer cancel()
	for attempt := 0; attempt < codexCredentialProbeAttempts; attempt++ {
		req, err := newCodexUsageRequest(probeCtx, credential)
		if err != nil {
			return false, errors.New("build Codex credential validation request")
		}
		_, err = executeOAuthUsageRequest(client, req, "Codex")
		if err == nil {
			return true, nil
		}
		var statusErr *oauthUsageHTTPStatusError
		if errors.As(err, &statusErr) && (statusErr.statusCode == http.StatusUnauthorized || statusErr.statusCode == http.StatusForbidden) {
			return false, nil
		}
		if attempt+1 == codexCredentialProbeAttempts || !isTransientOAuthUsageError(err) {
			return false, fmt.Errorf("validate Codex access token: %w", err)
		}
		delay := time.Duration(attempt+1) * codexCredentialProbeRetryDelay
		timer := time.NewTimer(delay)
		select {
		case <-probeCtx.Done():
			timer.Stop()
			return false, fmt.Errorf("validate Codex access token: %w", probeCtx.Err())
		case <-timer.C:
		}
	}
	return false, errors.New("validate Codex access token: retry budget exhausted")
}

func isTransientOAuthUsageError(err error) bool {
	var requestErr *oauthUsageRequestError
	if errors.As(err, &requestErr) {
		return true
	}
	var statusErr *oauthUsageHTTPStatusError
	return errors.As(err, &statusErr) &&
		(statusErr.statusCode == http.StatusTooManyRequests || statusErr.statusCode >= http.StatusInternalServerError)
}
