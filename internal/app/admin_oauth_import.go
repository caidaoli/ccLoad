package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/codexauth"
	"ccLoad/internal/xaiauth"

	"github.com/gin-gonic/gin"
)

const (
	oauthCredentialProviderAuto       = "auto"
	oauthCredentialUnknownTypeMessage = "credential type could not be determined"
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
	cleanup                func()
}

type oauthCredentialImportEvent struct {
	Event     string                       `json:"event"`
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
	case codexauth.ChannelType, antigravityauth.ChannelType, xaiauth.ChannelType:
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
		case codexauth.ChannelType, antigravityauth.ChannelType, xaiauth.ChannelType:
			return normalized, nil
		case "":
			// Empty and omitted types use the same field-based inference.
		default:
			return "", nil
		}
	}

	codexFields := hasAnyJSONField(fields, "account_id", "plan_type")
	antigravityFields := hasAnyJSONField(fields, "project_id", "timestamp")
	xaiFields := hasStrongXAIImportMarker(fields)
	providerCount := 0
	for _, matched := range []bool{codexFields, antigravityFields, xaiFields} {
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
	return xaiauth.ChannelType, nil
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
	defer batch.close()
	summary, _ := s.runOAuthCredentialImport(c, batch, nil)
	RespondJSON(c, http.StatusOK, summary)
}

// HandleImportOAuthCredentialsStream imports OAuth credentials while emitting
// one server-sent event before and after each sorted credential.
func (s *Server) HandleImportOAuthCredentialsStream(c *gin.Context) {
	batch, status, err := s.prepareOAuthCredentialImport(c, "")
	if err != nil {
		RespondError(c, status, err)
		return
	}
	defer batch.close()

	c.Header("Content-Type", "text/event-stream; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Status(http.StatusOK)
	start := oauthCredentialImportEvent{Event: "start", Total: len(batch.Files)}
	if writeOAuthCredentialImportEvent(c, start) != nil {
		return
	}
	observer := func(event oauthCredentialImportEvent) bool {
		return writeOAuthCredentialImportEvent(c, event) == nil
	}
	summary, completed := s.runOAuthCredentialImport(c, batch, observer)
	if !completed {
		return
	}
	_ = writeOAuthCredentialImportEvent(c, oauthCredentialImportEvent{
		Event:     "complete",
		Processed: len(summary.Results),
		Total:     len(batch.Files),
		Created:   summary.Created,
		Skipped:   summary.Skipped,
		Failed:    summary.Failed,
	})
}

func (s *Server) prepareOAuthCredentialImport(c *gin.Context, forcedProvider string) (*oauthCredentialImportBatch, int, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxOAuthCredentialImportRequestBytes)
	form, err := c.MultipartForm()
	if err != nil {
		return nil, http.StatusBadRequest, errors.New("credential files are required")
	}
	cleanup := func() { _ = form.RemoveAll() }
	files := form.File["files"]
	if len(files) == 0 {
		cleanup()
		return nil, http.StatusBadRequest, errors.New("credential files are required")
	}

	providerValue := forcedProvider
	if providerValue == "" {
		providerValue = firstMultipartValue(form.Value["provider"])
	}
	provider, err := normalizeOAuthCredentialProvider(providerValue)
	if err != nil {
		cleanup()
		return nil, http.StatusBadRequest, err
	}
	priorityIncrement, err := parseOAuthPriorityIncrement(firstMultipartValue(form.Value["priority_increment"]))
	if err != nil {
		cleanup()
		return nil, http.StatusBadRequest, err
	}
	credentialFiles := expandOAuthCredentialUploads(files)
	if provider == oauthCredentialProviderAuto || provider == xaiauth.ChannelType {
		credentialFiles = expandXAICredentialContainers(credentialFiles, provider)
	}
	nextPriorityByProvider := map[string]int{
		codexauth.ChannelType:       0,
		antigravityauth.ChannelType: 0,
		xaiauth.ChannelType:         0,
	}
	if priorityIncrement > 0 {
		configs, listErr := s.store.ListConfigs(c.Request.Context())
		if listErr != nil {
			cleanup()
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
		cleanup:                cleanup,
	}, 0, nil
}

func (b *oauthCredentialImportBatch) close() {
	if b != nil && b.cleanup != nil {
		b.cleanup()
		b.cleanup = nil
	}
}

func (s *Server) runOAuthCredentialImport(
	c *gin.Context,
	batch *oauthCredentialImportBatch,
	observer oauthCredentialImportObserver,
) (oauthCredentialImportSummary, bool) {
	summary := oauthCredentialImportSummary{Results: make([]oauthCredentialImportResult, 0, len(batch.Files))}
	completed := true
	for _, file := range batch.Files {
		if c.Request.Context().Err() != nil {
			completed = false
			break
		}
		if observer != nil && !observer(oauthCredentialImportEvent{
			Event:     "processing",
			Processed: len(summary.Results),
			Total:     len(batch.Files),
			Created:   summary.Created,
			Skipped:   summary.Skipped,
			Failed:    summary.Failed,
			FileName:  file.FileName,
		}) {
			completed = false
			break
		}

		result := s.runOAuthCredentialImportFile(c, batch, file)
		appendOAuthCredentialImportResult(&summary, result)
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
				completed = false
				break
			}
		}
	}
	if summary.Created > 0 {
		s.InvalidateChannelListCache()
	}
	return summary, completed
}

func (s *Server) runOAuthCredentialImportFile(
	c *gin.Context,
	batch *oauthCredentialImportBatch,
	file oauthCredentialImportFile,
) oauthCredentialImportResult {
	result := oauthCredentialImportResult{FileName: file.FileName}
	if file.Err != nil {
		result.Status, result.Error = "failed", file.Err.Error()
		return result
	}

	credentialProvider := batch.Provider
	if credentialProvider == oauthCredentialProviderAuto {
		detectedProvider, err := detectOAuthCredentialProvider(file.Raw)
		if err != nil {
			result.Status, result.Error = "failed", err.Error()
			return result
		}
		if detectedProvider == "" {
			result.Status, result.Error = "skipped", oauthCredentialUnknownTypeMessage
			return result
		}
		credentialProvider = detectedProvider
	}

	channelName, created, err := s.importOAuthCredential(
		c,
		credentialProvider,
		file.Raw,
		batch.NextPriorityByProvider[credentialProvider],
	)
	switch {
	case errors.Is(err, errOAuthCredentialUnusable), errors.Is(err, antigravityauth.ErrCredentialUnusable):
		result.Status, result.Error = "skipped", err.Error()
	case err != nil:
		result.Status, result.Error = "failed", err.Error()
	case created:
		result.Status, result.ChannelName = "created", channelName
		batch.NextPriorityByProvider[credentialProvider] += batch.PriorityIncrement
	default:
		result.Status, result.ChannelName = "skipped", channelName
	}
	return result
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
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event.Event, raw); err != nil {
		return err
	}
	c.Writer.Flush()
	return nil
}

func firstMultipartValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func (s *Server) importOAuthCredential(c *gin.Context, provider string, raw []byte, priority int) (string, bool, error) {
	switch provider {
	case codexauth.ChannelType:
		credential, err := codexauth.ParseCredential(raw)
		if err != nil {
			return "", false, err
		}
		if existingName, exists, err := s.findExistingOAuthChannelName(c.Request.Context(), codexChannelBaseName(credential)); err != nil {
			return "", false, fmt.Errorf("list channels for Codex credential: %w", err)
		} else if exists {
			return existingName, false, nil
		}
		credential, err = s.completeImportedCodexCredential(c.Request.Context(), credential)
		if err != nil {
			return "", false, err
		}
		return createImportedCodexChannel(c.Request.Context(), s.store, credential, priority)
	case antigravityauth.ChannelType:
		credential, err := antigravityauth.ParseCredential(raw)
		if err != nil {
			return "", false, err
		}
		if existingName, exists, findErr := s.findExistingOAuthChannelName(c.Request.Context(), antigravityChannelBaseName(credential)); findErr != nil {
			return "", false, fmt.Errorf("list channels for Antigravity credential: %w", findErr)
		} else if exists {
			return existingName, false, nil
		}
		if s.antigravityService == nil {
			return "", false, errors.New("antigravity credential completion is unavailable")
		}
		credential, err = s.antigravityService.CompleteCredential(c.Request.Context(), credential)
		if err != nil {
			return "", false, err
		}
		return createImportedAntigravityChannel(c.Request.Context(), s.store, credential, priority)
	case xaiauth.ChannelType:
		credential, err := xaiauth.ParseCredential(raw)
		if err != nil {
			return "", false, err
		}
		if s.client == nil {
			return "", false, errors.New("xAI credential completion is unavailable")
		}
		credential, err = completeXAICredential(
			c.Request.Context(), xaiauth.NewService(s.client), s.client, credential, xaiauth.CLIBaseURL,
		)
		if err != nil {
			return "", false, err
		}
		return createImportedXAIChannel(c.Request.Context(), s.store, credential, priority)
	default:
		return "", false, fmt.Errorf("unsupported credential provider %q", provider)
	}
}

func expandXAICredentialContainers(files []oauthCredentialImportFile, provider string) []oauthCredentialImportFile {
	expanded := make([]oauthCredentialImportFile, 0, len(files))
	for _, file := range files {
		if file.Err != nil {
			expanded = append(expanded, file)
			continue
		}
		fields, err := decodeOAuthCredentialFields(file.Raw)
		if err != nil {
			expanded = append(expanded, file)
			continue
		}
		rawContainer, ok := fields["credentials"]
		if !ok {
			expanded = append(expanded, file)
			continue
		}
		var container map[string]json.RawMessage
		if json.Unmarshal(rawContainer, &container) != nil || len(container) == 0 {
			expanded = append(expanded, failedOAuthCredentialImportFile(file.FileName, errors.New("xAI credentials must be a non-empty object")))
			continue
		}
		keys := make([]string, 0, len(container))
		for key := range container {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		if provider == oauthCredentialProviderAuto {
			xaiContainer := true
			for _, key := range keys {
				detected, detectErr := detectOAuthCredentialProvider(container[key])
				if detectErr != nil || detected != xaiauth.ChannelType {
					xaiContainer = false
					break
				}
			}
			if !xaiContainer {
				expanded = append(expanded, file)
				continue
			}
		}
		for index, key := range keys {
			item := newOAuthCredentialImportFile(
				fmt.Sprintf("%s#%d", file.FileName, index+1),
				file.SortName+"\x00"+key,
				container[key],
			)
			expanded = append(expanded, item)
		}
	}
	sortOAuthCredentialImportFiles(expanded)
	return expanded
}

// findExistingOAuthChannelName is a cheap preflight before remote credential
// validation. Channel creation repeats the check to close the concurrent-create race.
func (s *Server) findExistingOAuthChannelName(ctx context.Context, name string) (string, bool, error) {
	configs, err := s.store.ListConfigs(ctx)
	if err != nil {
		return "", false, err
	}
	for _, cfg := range configs {
		if cfg != nil && strings.EqualFold(strings.TrimSpace(cfg.Name), name) {
			return cfg.Name, true, nil
		}
	}
	return "", false, nil
}

func (s *Server) completeImportedCodexCredential(ctx context.Context, credential *codexauth.Credential) (*codexauth.Credential, error) {
	if credential == nil {
		return nil, errors.New("codex credential is nil")
	}
	service := s.codexService
	if service == nil && s.client != nil {
		service = codexauth.NewService(s.client)
	}
	if service == nil || service.Client == nil {
		return nil, errors.New("codex credential validation is unavailable")
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
		return nil, fmt.Errorf("%w: Codex refresh failed", errOAuthCredentialUnusable)
	}
	merged, err := credential.MergeRefresh(refreshed)
	if err != nil {
		return nil, fmt.Errorf("%w: Codex refresh response was invalid", errOAuthCredentialUnusable)
	}
	accepted, probeErr := probeCodexAccessToken(ctx, service.Client, merged)
	if probeErr != nil || !accepted {
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
	return false, fmt.Errorf("validate Codex access token: %w", err)
}
