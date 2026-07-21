package app

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/util"

	"github.com/bytedance/sonic"
	"golang.org/x/sync/singleflight"
)

const (
	modelVerificationVerdictConsistent = "consistent"
	modelVerificationVerdictMismatch   = "mismatch"
	modelVerificationVerdictUnverified = "unverified"

	modelVerificationSourceUnknown         = "unknown"
	modelVerificationSourceLikelyWebBridge = "likely_web_bridge"

	modelVerificationCatalogTimeout  = 6 * time.Second
	modelVerificationCatalogCacheTTL = 30 * time.Second
	maxModelVerificationBodyBytes    = 1024 * 1024
)

var (
	modelDateVersionSuffix = regexp.MustCompile(`(?:-\d{4}-\d{2}-\d{2}|-\d{8})$`)
	geminiVersionSuffix    = regexp.MustCompile(`-(?:\d{3}|preview(?:-[a-z0-9]+)*|exp(?:-[a-z0-9]+)*|experimental(?:-[a-z0-9]+)*)$`)
)

// modelVerification is evidence collected from one explicit channel test.
// It intentionally does not claim to prove the model behind an intermediary.
type modelVerification struct {
	ClaimedModel       string                      `json:"claimed_model"`
	EffectiveModel     string                      `json:"effective_model"`
	ReportedModel      string                      `json:"reported_model,omitempty"`
	ModelRewritten     bool                        `json:"model_rewritten"`
	Verdict            string                      `json:"verdict"`
	EvidenceConfidence int                         `json:"evidence_confidence"`
	Source             string                      `json:"source"`
	SourceConfidence   int                         `json:"source_confidence"`
	Catalog            modelVerificationCatalog    `json:"catalog"`
	Evidence           []modelVerificationEvidence `json:"evidence"`
	Limitation         string                      `json:"limitation"`
}

type modelVerificationCatalog struct {
	Attempted            bool   `json:"attempted"`
	Available            bool   `json:"available"`
	Source               string `json:"source,omitempty"`
	ModelCount           int    `json:"model_count,omitempty"`
	EffectiveModelListed bool   `json:"effective_model_listed"`
	ReportedModelListed  bool   `json:"reported_model_listed"`
	ErrorCode            string `json:"error_code,omitempty"`
}

type modelVerificationEvidence struct {
	Code string `json:"code"`
}

type modelIdentityRelation uint8

const (
	modelIdentityUnknown modelIdentityRelation = iota
	modelIdentityExact
	modelIdentityProviderAlias
	modelIdentityConflict
)

type modelVersionKind uint8

const (
	modelVersionStable modelVersionKind = iota
	modelVersionAlias
	modelVersionSnapshot
)

type providerModelIdentity struct {
	line    string
	version modelVersionKind
}

type modelVerificationCatalogSnapshot struct {
	source     string
	modelCount int
	models     map[string]struct{}
	errorCode  string
}

type modelVerificationCatalogCacheEntry struct {
	snapshot  modelVerificationCatalogSnapshot
	expiresAt time.Time
}

type modelVerificationCatalogCache struct {
	mu      sync.Mutex
	entries map[string]modelVerificationCatalogCacheEntry
	group   singleflight.Group
}

func (s *Server) attachModelVerification(
	ctx context.Context,
	cfg *model.Config,
	upstreamProtocol, claimedModel, effectiveModel, baseURL, apiKey string,
	result map[string]any,
) {
	if result == nil {
		return
	}

	verification := newModelVerification(upstreamProtocol, claimedModel, effectiveModel, result)
	if success, _ := result["success"].(bool); success && determineSource(upstreamProtocol) == "api" {
		verification.enrichCatalog(ctx, s, cfg, upstreamProtocol, baseURL, apiKey)
	}
	result["model_verification"] = verification
}

func newModelVerification(upstreamProtocol, claimedModel, effectiveModel string, result map[string]any) *modelVerification {
	claimedModel = strings.TrimSpace(claimedModel)
	effectiveModel = strings.TrimSpace(effectiveModel)

	verification := &modelVerification{
		ClaimedModel:   claimedModel,
		EffectiveModel: effectiveModel,
		ModelRewritten: !sameModelName(claimedModel, effectiveModel),
		Verdict:        modelVerificationVerdictUnverified,
		Source:         modelVerificationSourceUnknown,
		Evidence:       make([]modelVerificationEvidence, 0, 4),
		Limitation:     "metadata_is_not_proof",
	}

	if verification.ModelRewritten {
		verification.addEvidence("configured_model_rewrite")
	}

	responseBody := modelVerificationResponseBody(result)
	verification.ReportedModel = extractReportedModel(responseBody)
	switch compareModelIdentity(upstreamProtocol, effectiveModel, verification.ReportedModel) {
	case modelIdentityExact:
		verification.Verdict = modelVerificationVerdictConsistent
		verification.EvidenceConfidence = 50
		verification.addEvidence("response_model_matches_effective")
	case modelIdentityProviderAlias:
		verification.Verdict = modelVerificationVerdictConsistent
		verification.EvidenceConfidence = 50
		verification.addEvidence("response_model_provider_alias")
	case modelIdentityConflict:
		verification.Verdict = modelVerificationVerdictMismatch
		verification.EvidenceConfidence = 90
		verification.addEvidence("response_model_mismatch")
	default:
		if verification.ReportedModel == "" {
			verification.addEvidence("response_model_missing")
		} else {
			verification.addEvidence("response_model_relation_unknown")
		}
	}

	if hasLikelyWebBridgeMarker(result) {
		verification.Source = modelVerificationSourceLikelyWebBridge
		verification.SourceConfidence = 90
		verification.addEvidence("chatgpt_web_bridge_marker")
	}

	return verification
}

func (v *modelVerification) enrichCatalog(
	ctx context.Context,
	s *Server,
	cfg *model.Config,
	upstreamProtocol, baseURL, apiKey string,
) {
	v.Catalog.Attempted = true
	snapshot := s.getModelVerificationCatalog(ctx, cfg, upstreamProtocol, baseURL, apiKey)
	if snapshot.errorCode != "" {
		v.Catalog.ErrorCode = snapshot.errorCode
		v.addEvidence("model_catalog_" + snapshot.errorCode)
		return
	}

	v.Catalog.Available = true
	v.Catalog.Source = snapshot.source
	v.Catalog.ModelCount = snapshot.modelCount
	v.Catalog.EffectiveModelListed = modelCatalogContains(snapshot, upstreamProtocol, v.EffectiveModel)
	v.Catalog.ReportedModelListed = v.ReportedModel != "" && modelCatalogContains(snapshot, upstreamProtocol, v.ReportedModel)

	if v.Catalog.EffectiveModelListed {
		v.addEvidence("effective_model_listed")
		if v.Verdict == modelVerificationVerdictConsistent {
			v.EvidenceConfidence = 60
		}
	} else {
		v.addEvidence("effective_model_not_listed")
	}
	if v.ReportedModel != "" && !v.Catalog.ReportedModelListed {
		v.addEvidence("reported_model_not_listed")
	}
}

func (s *Server) getModelVerificationCatalog(
	ctx context.Context,
	cfg *model.Config,
	upstreamProtocol, baseURL, apiKey string,
) modelVerificationCatalogSnapshot {
	if s == nil || cfg == nil || model.HasExactUpstreamURLMarker(baseURL) || determineSource(upstreamProtocol) != "api" {
		return modelVerificationCatalogSnapshot{errorCode: "unsupported"}
	}

	catalogCtx, cancel := context.WithTimeout(ctx, modelVerificationCatalogTimeout)
	defer cancel()

	cacheKey := modelVerificationCatalogCacheKey(cfg, upstreamProtocol, baseURL, apiKey)
	return s.modelVerificationCatalogCache.getOrFetch(catalogCtx, cacheKey, func(fetchCtx context.Context) modelVerificationCatalogSnapshot {
		return s.fetchModelVerificationCatalog(fetchCtx, cfg, upstreamProtocol, baseURL, apiKey)
	})
}

func (s *Server) fetchModelVerificationCatalog(
	ctx context.Context,
	cfg *model.Config,
	upstreamProtocol, baseURL, apiKey string,
) modelVerificationCatalogSnapshot {
	req, err := util.BuildModelsRequest(ctx, upstreamProtocol, strings.TrimSpace(baseURL), apiKey)
	if err != nil {
		return modelVerificationCatalogSnapshot{errorCode: "unavailable"}
	}

	resp, err := s.doUpstreamRequest(cfg, req)
	if err != nil {
		return modelVerificationCatalogSnapshot{errorCode: modelVerificationCatalogErrorCode(err)}
	}
	if resp == nil || resp.Body == nil {
		return modelVerificationCatalogSnapshot{errorCode: "unavailable"}
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelVerificationBodyBytes+1))
	if err != nil || len(body) > maxModelVerificationBodyBytes || resp.StatusCode != http.StatusOK {
		return modelVerificationCatalogSnapshot{errorCode: "unavailable"}
	}

	modelNames, err := util.ParseModelsResponse(upstreamProtocol, body)
	if err != nil {
		return modelVerificationCatalogSnapshot{errorCode: "unavailable"}
	}

	models := make(map[string]struct{}, len(modelNames))
	for _, modelName := range modelNames {
		if normalized := normalizeCatalogModelName(upstreamProtocol, modelName); normalized != "" {
			models[normalized] = struct{}{}
		}
	}
	return modelVerificationCatalogSnapshot{
		source:     "api",
		modelCount: len(modelNames),
		models:     models,
	}
}

func modelVerificationCatalogErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrChannelRPMExceeded):
		return "rate_limited"
	case errors.Is(err, ErrChannelConcurrencyExceeded):
		return "concurrency_limited"
	default:
		return "unavailable"
	}
}

func modelVerificationCatalogCacheKey(cfg *model.Config, upstreamProtocol, baseURL, apiKey string) string {
	keyHash := sha256.Sum256([]byte(apiKey))
	return strings.Join([]string{
		strconv.FormatInt(cfg.ID, 10),
		util.NormalizeChannelType(upstreamProtocol),
		strings.TrimSpace(baseURL),
		strings.TrimSpace(cfg.ProxyURL),
		hex.EncodeToString(keyHash[:]),
	}, "\x00")
}

func (c *modelVerificationCatalogCache) getOrFetch(
	ctx context.Context,
	key string,
	fetch func(context.Context) modelVerificationCatalogSnapshot,
) modelVerificationCatalogSnapshot {
	if snapshot, ok := c.get(key); ok {
		return snapshot
	}

	resultCh := c.group.DoChan(key, func() (any, error) {
		if snapshot, ok := c.get(key); ok {
			return snapshot, nil
		}

		snapshot := fetch(ctx)
		if ctx.Err() == nil {
			c.put(key, snapshot)
		}
		return snapshot, nil
	})

	select {
	case result := <-resultCh:
		snapshot, ok := result.Val.(modelVerificationCatalogSnapshot)
		if !ok {
			return modelVerificationCatalogSnapshot{errorCode: "unavailable"}
		}
		return snapshot
	case <-ctx.Done():
		return modelVerificationCatalogSnapshot{errorCode: "unavailable"}
	}
}

func (c *modelVerificationCatalogCache) get(key string) (modelVerificationCatalogSnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return modelVerificationCatalogSnapshot{}, false
	}
	if !entry.expiresAt.After(time.Now()) {
		delete(c.entries, key)
		return modelVerificationCatalogSnapshot{}, false
	}
	return entry.snapshot, true
}

func (c *modelVerificationCatalogCache) put(key string, snapshot modelVerificationCatalogSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.entries == nil {
		c.entries = make(map[string]modelVerificationCatalogCacheEntry)
	}
	now := time.Now()
	for cacheKey, entry := range c.entries {
		if !entry.expiresAt.After(now) {
			delete(c.entries, cacheKey)
		}
	}
	c.entries[key] = modelVerificationCatalogCacheEntry{
		snapshot:  snapshot,
		expiresAt: now.Add(modelVerificationCatalogCacheTTL),
	}
}

func modelCatalogContains(snapshot modelVerificationCatalogSnapshot, upstreamProtocol, target string) bool {
	normalizedTarget := normalizeCatalogModelName(upstreamProtocol, target)
	if normalizedTarget == "" {
		return false
	}
	if _, ok := snapshot.models[normalizedTarget]; ok {
		return true
	}

	for modelName := range snapshot.models {
		relation := compareModelIdentity(upstreamProtocol, target, modelName)
		if relation == modelIdentityExact || relation == modelIdentityProviderAlias {
			return true
		}
	}
	return false
}

func compareModelIdentity(upstreamProtocol, effectiveModel, reportedModel string) modelIdentityRelation {
	if sameModelName(effectiveModel, reportedModel) {
		return modelIdentityExact
	}

	effectiveName := normalizeCatalogModelName(upstreamProtocol, effectiveModel)
	reportedName := normalizeCatalogModelName(upstreamProtocol, reportedModel)
	if effectiveName == "" || reportedName == "" {
		return modelIdentityUnknown
	}
	if effectiveName == reportedName {
		return modelIdentityProviderAlias
	}

	effectiveIdentity, effectiveKnown := providerModelIdentityFor(upstreamProtocol, effectiveName)
	reportedIdentity, reportedKnown := providerModelIdentityFor(upstreamProtocol, reportedName)
	if !effectiveKnown || !reportedKnown {
		return modelIdentityUnknown
	}
	if effectiveIdentity.line != reportedIdentity.line {
		return modelIdentityConflict
	}
	if effectiveIdentity.version == modelVersionSnapshot && reportedIdentity.version == modelVersionSnapshot {
		return modelIdentityUnknown
	}
	return modelIdentityProviderAlias
}

func providerModelIdentityFor(upstreamProtocol, modelName string) (providerModelIdentity, bool) {
	name := normalizeCatalogModelName(upstreamProtocol, modelName)
	if name == "" {
		return providerModelIdentity{}, false
	}

	switch util.NormalizeChannelType(upstreamProtocol) {
	case util.ChannelTypeOpenAI, util.ChannelTypeCodex:
		if !isOpenAIModelName(name) {
			return providerModelIdentity{}, false
		}
	case util.ChannelTypeAnthropic:
		if !strings.HasPrefix(name, "claude-") {
			return providerModelIdentity{}, false
		}
	case util.ChannelTypeGemini:
		if !strings.HasPrefix(name, "gemini-") {
			return providerModelIdentity{}, false
		}
	default:
		return providerModelIdentity{}, false
	}

	line, version := splitProviderModelVersion(util.NormalizeChannelType(upstreamProtocol), name)
	if line == "" {
		return providerModelIdentity{}, false
	}
	return providerModelIdentity{line: line, version: version}, true
}

func isOpenAIModelName(name string) bool {
	if strings.HasPrefix(name, "gpt-") || strings.HasPrefix(name, "chatgpt-") {
		return true
	}
	return len(name) >= 2 && name[0] == 'o' && name[1] >= '0' && name[1] <= '9'
}

func splitProviderModelVersion(channelType, modelName string) (string, modelVersionKind) {
	if strings.HasSuffix(modelName, "-latest") {
		return strings.TrimSuffix(modelName, "-latest"), modelVersionAlias
	}
	if channelType == util.ChannelTypeGemini && geminiVersionSuffix.MatchString(modelName) {
		return geminiVersionSuffix.ReplaceAllString(modelName, ""), modelVersionSnapshot
	}
	if modelDateVersionSuffix.MatchString(modelName) {
		return modelDateVersionSuffix.ReplaceAllString(modelName, ""), modelVersionSnapshot
	}
	return modelName, modelVersionStable
}

func normalizeCatalogModelName(upstreamProtocol, modelName string) string {
	normalized := strings.ToLower(strings.TrimSpace(modelName))
	if util.NormalizeChannelType(upstreamProtocol) == util.ChannelTypeGemini {
		normalized = strings.TrimPrefix(normalized, "models/")
	}
	return normalized
}

func (v *modelVerification) addEvidence(code string) {
	for _, evidence := range v.Evidence {
		if evidence.Code == code {
			return
		}
	}
	v.Evidence = append(v.Evidence, modelVerificationEvidence{Code: code})
}

func sameModelName(left, right string) bool {
	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)
	return left != "" && right != "" && strings.EqualFold(left, right)
}

func modelVerificationResponseBody(result map[string]any) string {
	for _, key := range []string{"upstream_response_body", "raw_response"} {
		if body := strings.TrimSpace(getResultString(result, key)); body != "" {
			return limitModelVerificationBody(body)
		}
	}
	if apiResponse, ok := result["api_response"]; ok {
		if body, err := sonic.Marshal(apiResponse); err == nil {
			return limitModelVerificationBody(string(body))
		}
	}
	return ""
}

func limitModelVerificationBody(body string) string {
	if len(body) <= maxModelVerificationBodyBytes {
		return body
	}
	return body[:maxModelVerificationBodyBytes]
}

func extractReportedModel(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}
	if reported := extractReportedModelFromJSON([]byte(body)); reported != "" {
		return reported
	}

	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), maxModelVerificationBodyBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		if reported := extractReportedModelFromJSON([]byte(data)); reported != "" {
			return reported
		}
	}
	return ""
}

func extractReportedModelFromJSON(body []byte) string {
	var payload map[string]any
	if err := sonic.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return reportedModelFromPayload(payload)
}

func reportedModelFromPayload(payload map[string]any) string {
	if modelName := payloadString(payload, "model"); modelName != "" {
		return modelName
	}
	if modelName := payloadString(payload, "modelVersion"); modelName != "" {
		return modelName
	}
	for _, key := range []string{"response", "message"} {
		nested, _ := payload[key].(map[string]any)
		if modelName := payloadString(nested, "model"); modelName != "" {
			return modelName
		}
		if modelName := payloadString(nested, "modelVersion"); modelName != "" {
			return modelName
		}
	}
	return ""
}

func payloadString(payload map[string]any, key string) string {
	if payload == nil {
		return ""
	}
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}

func hasLikelyWebBridgeMarker(result map[string]any) bool {
	candidates := []string{
		getResultString(result, "base_url"),
		getResultString(result, "upstream_request_url"),
		getResultString(result, "upstream_response_body"),
		getResultString(result, "raw_response"),
		getResultString(result, "error"),
	}
	if responseHeaders, ok := result["response_headers"].(map[string]string); ok {
		for key, value := range responseHeaders {
			candidates = append(candidates, key+": "+value)
		}
	}

	for _, candidate := range candidates {
		value := strings.ToLower(candidate)
		if strings.Contains(value, "chatgpt.com/backend-api") ||
			strings.Contains(value, "chat.openai.com/backend-api") ||
			(strings.Contains(value, "chatgpt.com") && strings.Contains(value, "/backend-api/")) {
			return true
		}
	}
	return false
}
