package app

import (
	"bufio"
	"context"
	"strings"
	"time"

	"ccLoad/internal/model"

	"github.com/bytedance/sonic"
)

const (
	modelVerificationVerdictConsistent = "consistent"
	modelVerificationVerdictMismatch   = "mismatch"
	modelVerificationVerdictUnverified = "unverified"

	modelVerificationSourceUnknown         = "unknown"
	modelVerificationSourceLikelyWebBridge = "likely_web_bridge"

	modelVerificationCatalogTimeout = 6 * time.Second
	maxModelVerificationBodyBytes   = 1024 * 1024
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

func (s *Server) attachModelVerification(
	ctx context.Context,
	upstreamProtocol, claimedModel, effectiveModel, baseURL, apiKey string,
	result map[string]any,
) {
	if result == nil {
		return
	}

	verification := newModelVerification(claimedModel, effectiveModel, result)
	if success, _ := result["success"].(bool); success && determineSource(upstreamProtocol) == "api" {
		verification.enrichCatalog(ctx, upstreamProtocol, baseURL, apiKey)
	}
	result["model_verification"] = verification
}

func newModelVerification(claimedModel, effectiveModel string, result map[string]any) *modelVerification {
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
	switch {
	case verification.ReportedModel == "":
		verification.addEvidence("response_model_missing")
	case sameModelName(verification.ReportedModel, effectiveModel):
		verification.Verdict = modelVerificationVerdictConsistent
		verification.EvidenceConfidence = 50
		verification.addEvidence("response_model_matches_effective")
	default:
		verification.Verdict = modelVerificationVerdictMismatch
		verification.EvidenceConfidence = 90
		verification.addEvidence("response_model_mismatch")
	}

	if hasLikelyWebBridgeMarker(result) {
		verification.Source = modelVerificationSourceLikelyWebBridge
		verification.SourceConfidence = 90
		verification.addEvidence("chatgpt_web_bridge_marker")
	}

	return verification
}

func (v *modelVerification) enrichCatalog(ctx context.Context, upstreamProtocol, baseURL, apiKey string) {
	v.Catalog.Attempted = true
	catalogCtx, cancel := context.WithTimeout(ctx, modelVerificationCatalogTimeout)
	defer cancel()

	response, err := fetchModelsForConfig(catalogCtx, upstreamProtocol, model.StripExactUpstreamURLMarker(baseURL), apiKey)
	if err != nil {
		v.Catalog.ErrorCode = "unavailable"
		v.addEvidence("model_catalog_unavailable")
		return
	}
	if response == nil || response.Source != "api" {
		v.Catalog.ErrorCode = "unsupported"
		v.addEvidence("model_catalog_unsupported")
		return
	}

	v.Catalog.Available = true
	v.Catalog.Source = response.Source
	v.Catalog.ModelCount = len(response.Models)
	v.Catalog.EffectiveModelListed = modelCatalogContains(response.Models, v.EffectiveModel)
	v.Catalog.ReportedModelListed = v.ReportedModel != "" && modelCatalogContains(response.Models, v.ReportedModel)

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

func (v *modelVerification) addEvidence(code string) {
	for _, evidence := range v.Evidence {
		if evidence.Code == code {
			return
		}
	}
	v.Evidence = append(v.Evidence, modelVerificationEvidence{Code: code})
}

func modelCatalogContains(entries []model.ModelEntry, target string) bool {
	for _, entry := range entries {
		if sameModelName(entry.Model, target) || sameModelName(entry.RedirectModel, target) {
			return true
		}
	}
	return false
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
