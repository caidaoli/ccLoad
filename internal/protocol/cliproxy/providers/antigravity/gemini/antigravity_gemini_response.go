// Package gemini provides request translation functionality for Gemini to Antigravity API compatibility.
// It handles parsing and transforming Gemini API requests into Antigravity API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Gemini API format and Antigravity API's expected format.
package gemini

import (
	"bytes"
	"context"
	"fmt"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"
	"ccLoad/internal/protocol/cliproxy/util"

	"github.com/MoeForge/moego/ojson"
)

type convertAntigravityGeminiParams struct {
	SawResponse     bool
	SawFinishReason bool
	ModelVersion    string
	ResponseID      string
	UsageMetadata   []byte
}

// observe records the metadata required to synthesize a faithful terminal chunk
// when upstream never emits finishReason.
func (p *convertAntigravityGeminiParams) observe(rawJSON []byte) {
	if !p.SawResponse {
		p.SawResponse = translatorcommon.HasAntigravityResponsePayload(rawJSON)
	}
	if !p.SawFinishReason {
		for _, path := range []string{"response.candidates", "candidates"} {
			for _, candidate := range ojson.GetBytes(rawJSON, path).Array() {
				if candidate.Get("finishReason").String() != "" {
					p.SawFinishReason = true
					break
				}
			}
			if p.SawFinishReason {
				break
			}
		}
	}
	if p.ModelVersion == "" {
		p.ModelVersion = firstStringValue(rawJSON, "response.modelVersion", "modelVersion")
	}
	if p.ResponseID == "" {
		p.ResponseID = firstStringValue(rawJSON, "response.responseId", "responseId")
	}
	// Keep the latest usage snapshot. FilterSSEUsageMetadata renames non-terminal
	// usage to cpaUsageMetadata, so both spellings must be tracked.
	for _, path := range []string{"response.usageMetadata", "response.cpaUsageMetadata", "usageMetadata", "cpaUsageMetadata"} {
		if usage := ojson.GetBytes(rawJSON, path); usage.Exists() {
			p.UsageMetadata = append(p.UsageMetadata[:0], usage.Raw...)
			break
		}
	}
}

// syntheticTerminalChunk mirrors the terminal chunk shape observed from upstream:
// a model-role candidate carrying an empty text part, finishReason, and the last
// known usage metadata. Fields are set in upstream key order
// (candidates, usageMetadata, modelVersion, responseId).
func (p *convertAntigravityGeminiParams) syntheticTerminalChunk() []byte {
	chunk := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"STOP"}]}`)
	if len(p.UsageMetadata) > 0 {
		chunk, _ = ojson.SetRawBytes(chunk, "usageMetadata", p.UsageMetadata)
	}
	if p.ModelVersion != "" {
		chunk, _ = ojson.SetBytes(chunk, "modelVersion", p.ModelVersion)
	}
	if p.ResponseID != "" {
		chunk, _ = ojson.SetBytes(chunk, "responseId", p.ResponseID)
	}
	return chunk
}

func firstStringValue(rawJSON []byte, paths ...string) string {
	for _, path := range paths {
		if value := ojson.GetBytes(rawJSON, path).String(); value != "" {
			return value
		}
	}
	return ""
}

// ConvertAntigravityResponseToGemini parses and transforms a Antigravity API request into Gemini API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the Gemini API.
// The function performs the following transformations:
// 1. Extracts the response data from the request
// 2. Handles alternative response formats
// 3. Processes array responses by extracting individual response objects
// 4. Synthesizes finishReason: "STOP" on [DONE] if omitted by upstream
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model to use for the request (unused in current implementation)
//   - rawJSON: The raw JSON request data from the Antigravity API
//   - param: A pointer to a parameter object for the conversion
//
// Returns:
//   - [][]byte: The transformed response data in Gemini API format.
func ConvertAntigravityResponseToGemini(ctx context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	if bytes.HasPrefix(rawJSON, []byte("data:")) {
		rawJSON = bytes.TrimSpace(rawJSON[5:])
	}

	var state *convertAntigravityGeminiParams
	if param != nil {
		if *param == nil {
			*param = &convertAntigravityGeminiParams{}
		}
		if s, ok := (*param).(*convertAntigravityGeminiParams); ok {
			state = s
		}
	}

	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		// Never finalize a stream that produced no response at all: an empty
		// upstream body must stay detectable as a failure rather than be reported
		// as a successful empty completion.
		if state == nil || !state.SawResponse || state.SawFinishReason {
			return [][]byte{}
		}
		state.SawFinishReason = true
		finalChunk := state.syntheticTerminalChunk()
		if alt, ok := ctx.Value("alt").(string); ok && alt != "" {
			finalChunk, _ = ojson.SetRawBytes([]byte("[]"), "-1", finalChunk)
		}
		return [][]byte{finalChunk}
	}

	if state != nil {
		state.observe(rawJSON)
	}

	if alt, ok := ctx.Value("alt").(string); ok {
		var chunk []byte
		if alt == "" {
			responseResult := util.GetGJSONBytesNoCopy(rawJSON, "response")
			if responseResult.Exists() {
				if responseResult.Index > 0 && responseResult.Index+len(responseResult.Raw) <= len(rawJSON) {
					chunk = rawJSON[responseResult.Index : responseResult.Index+len(responseResult.Raw)]
				} else {
					chunk = []byte(responseResult.Raw)
				}
				chunk = restoreUsageMetadata(chunk)
				chunk = restoreGeminiFunctionNames(chunk, util.DisambiguatedToolNameMap(originalRequestRawJSON))
			}
		} else {
			chunkTemplate := []byte("[]")
			responseResult := util.ParseGJSONBytesNoCopy(rawJSON)
			nameMap := util.DisambiguatedToolNameMap(originalRequestRawJSON)
			if responseResult.IsArray() {
				responseResultItems := responseResult.Array()
				for i := 0; i < len(responseResultItems); i++ {
					if response := responseResultItems[i].Get("response"); response.Exists() {
						item := restoreUsageMetadata([]byte(response.Raw))
						item = restoreGeminiFunctionNames(item, nameMap)
						chunkTemplate, _ = ojson.SetRawBytes(chunkTemplate, "-1", item)
					}
				}
			}
			chunk = chunkTemplate
		}
		return [][]byte{chunk}
	}
	return [][]byte{}
}

// ConvertAntigravityResponseToGeminiNonStream converts a non-streaming Antigravity request to a non-streaming Gemini response.
// This function processes the complete Antigravity request and transforms it into a single Gemini-compatible
// JSON response. It extracts the response data from the request and returns it in the expected format.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON request data from the Antigravity API
//   - param: A pointer to a parameter object for the conversion (unused in current implementation)
//
// Returns:
//   - []byte: A Gemini-compatible JSON response containing the response data.
func ConvertAntigravityResponseToGeminiNonStream(_ context.Context, _ string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, _ *any) []byte {
	var chunk []byte
	responseResult := util.GetGJSONBytesNoCopy(rawJSON, "response")
	if responseResult.Exists() {
		if responseResult.Index > 0 && responseResult.Index+len(responseResult.Raw) <= len(rawJSON) {
			chunk = rawJSON[responseResult.Index : responseResult.Index+len(responseResult.Raw)]
		} else {
			chunk = []byte(responseResult.Raw)
		}
		chunk = restoreUsageMetadata(chunk)
		chunk = restoreGeminiFunctionNames(chunk, util.DisambiguatedToolNameMap(originalRequestRawJSON))
	} else {
		chunk = restoreGeminiFunctionNames(rawJSON, util.DisambiguatedToolNameMap(originalRequestRawJSON))
	}
	candidates := util.GetGJSONBytesNoCopy(chunk, "candidates")
	if candidates.IsArray() {
		// Default every candidate, not just the first one, so a multi-candidate
		// response cannot leave some of them unterminated.
		idx := 0
		candidates.ForEach(func(_, candidate ojson.Result) bool {
			if candidate.Get("finishReason").String() == "" {
				chunk, _ = ojson.SetBytes(chunk, fmt.Sprintf("candidates.%d.finishReason", idx), "STOP")
			}
			idx++
			return true
		})
	}
	return chunk
}

func restoreGeminiFunctionNames(chunk []byte, nameMap map[string]string) []byte {
	if len(nameMap) == 0 {
		return chunk
	}
	candidates := ojson.GetBytes(chunk, "candidates")
	for candidateIndex, candidate := range candidates.Array() {
		for partIndex, part := range candidate.Get("content.parts").Array() {
			for _, field := range []string{"functionCall", "functionResponse", "function_call", "function_response"} {
				nameResult := part.Get(field + ".name")
				name := nameResult.String()
				if name == "" {
					continue
				}
				restoredName := util.RestoreSanitizedToolName(nameMap, name)
				if nameResult.Type == ojson.String && restoredName == name {
					continue
				}
				path := fmt.Sprintf("candidates.%d.content.parts.%d.%s.name", candidateIndex, partIndex, field)
				chunk, _ = ojson.SetBytes(chunk, path, restoredName)
			}
		}
	}
	return chunk
}

// GeminiTokenCount builds a Gemini token count response.
func GeminiTokenCount(ctx context.Context, count int64) []byte {
	return translatorcommon.GeminiTokenCountJSON(count)
}

// restoreUsageMetadata renames cpaUsageMetadata back to usageMetadata.
// The executor renames usageMetadata to cpaUsageMetadata in non-terminal chunks
// to preserve usage data while hiding it from clients that don't expect it.
// When returning standard Gemini API format, we must restore the original name.
func restoreUsageMetadata(chunk []byte) []byte {
	if cpaUsage := util.GetGJSONBytesNoCopy(chunk, "cpaUsageMetadata"); cpaUsage.Exists() {
		chunk, _ = ojson.SetRawBytes(chunk, "usageMetadata", []byte(cpaUsage.Raw))
		chunk, _ = ojson.DeleteBytes(chunk, "cpaUsageMetadata")
	}
	return chunk
}
