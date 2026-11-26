package responses

import (
	"context"

	coreresponses "ccLoad/internal/protocol/cliproxy/gemini/openai/responses"

	"github.com/MoeForge/moego/ojson"
)

// ConvertAntigravityResponseToOpenAIResponses converts one Antigravity stream payload to Responses SSE events.
func ConvertAntigravityResponseToOpenAIResponses(ctx context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	responseResult := ojson.GetBytes(rawJSON, "response")
	if responseResult.Exists() {
		rawJSON = []byte(responseResult.Raw)
	}
	return coreresponses.ConvertGeminiResponseToOpenAIResponses(ctx, modelName, originalRequestRawJSON, requestRawJSON, rawJSON, param)
}

// ConvertAntigravityResponseToOpenAIResponsesNonStream converts an Antigravity response to Responses JSON.
func ConvertAntigravityResponseToOpenAIResponsesNonStream(ctx context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []byte {
	responseResult := ojson.GetBytes(rawJSON, "response")
	if responseResult.Exists() {
		rawJSON = []byte(responseResult.Raw)
	}

	requestResult := ojson.GetBytes(originalRequestRawJSON, "request")
	if requestResult.Exists() {
		originalRequestRawJSON = []byte(requestResult.Raw)
	}

	requestResult = ojson.GetBytes(requestRawJSON, "request")
	if requestResult.Exists() {
		requestRawJSON = []byte(requestResult.Raw)
	}

	return coreresponses.ConvertGeminiResponseToOpenAIResponsesNonStream(ctx, modelName, originalRequestRawJSON, requestRawJSON, rawJSON, param)
}
