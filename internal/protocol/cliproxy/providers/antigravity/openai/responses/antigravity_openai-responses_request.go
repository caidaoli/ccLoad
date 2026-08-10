package responses

import (
	"encoding/json"
	"strings"

	coreresponses "ccLoad/internal/protocol/cliproxy/gemini/openai/responses"
	antigravitygemini "ccLoad/internal/protocol/cliproxy/providers/antigravity/gemini"
	sigcompat "ccLoad/internal/protocol/cliproxy/signature"

	"github.com/tidwall/gjson"
)

// ConvertOpenAIResponsesRequestToAntigravity converts a Responses request to the Antigravity Gemini envelope.
func ConvertOpenAIResponsesRequestToAntigravity(modelName string, inputRawJSON []byte, stream bool) []byte {
	rawJSON := inputRawJSON
	rawJSON = coreresponses.ConvertOpenAIResponsesRequestToGemini(modelName, rawJSON, stream)
	rawJSON = rewriteOpenAIResponsesReasoningForAntigravityClaude(modelName, inputRawJSON, rawJSON)
	return antigravitygemini.ConvertGeminiRequestToAntigravity(modelName, rawJSON, stream)
}

type antigravityClaudeReasoningSignature struct {
	Signature        string
	HasRawSignature  bool
	RawSignatureLen  int
	DetectedProvider sigcompat.SignatureProvider
}

func rewriteOpenAIResponsesReasoningForAntigravityClaude(modelName string, inputRawJSON, geminiJSON []byte) []byte {
	if sigcompat.SignatureProviderFromModelName(modelName) != sigcompat.SignatureProviderClaude {
		return geminiJSON
	}

	reasoningSignatures := antigravityClaudeReasoningSignatures(inputRawJSON)
	if len(reasoningSignatures) == 0 {
		return geminiJSON
	}

	var root map[string]any
	if err := json.Unmarshal(geminiJSON, &root); err != nil {
		return geminiJSON
	}

	contents, ok := root["contents"].([]any)
	if !ok {
		return geminiJSON
	}

	reasoningIndex := 0
	changed := false
	rewrittenContents := make([]any, 0, len(contents))
	for contentIndex, contentValue := range contents {
		content, ok := contentValue.(map[string]any)
		if !ok {
			rewrittenContents = append(rewrittenContents, contentValue)
			continue
		}

		parts, ok := content["parts"].([]any)
		if !ok {
			rewrittenContents = append(rewrittenContents, content)
			continue
		}

		rewrittenParts := make([]any, 0, len(parts))
		for partIndex, partValue := range parts {
			part, ok := partValue.(map[string]any)
			if !ok || part["thought"] != true {
				rewrittenParts = append(rewrittenParts, partValue)
				continue
			}

			var reasoningSig antigravityClaudeReasoningSignature
			if reasoningIndex < len(reasoningSignatures) {
				reasoningSig = reasoningSignatures[reasoningIndex]
			}
			reasoningIndex++

			if reasoningSig.Signature == "" {
				changed = true
				logDroppedOpenAIResponsesAntigravityClaudeReasoning(modelName, contentIndex, partIndex, reasoningIndex-1, reasoningSig)
				continue
			}
			if text, _ := part["text"].(string); strings.TrimSpace(text) == "" {
				changed = true
				logDroppedOpenAIResponsesAntigravityClaudeEmptyReasoning(modelName, contentIndex, partIndex, reasoningIndex-1, reasoningSig)
				continue
			}

			if currentSignature, _ := part["thoughtSignature"].(string); currentSignature != reasoningSig.Signature {
				changed = true
				logNormalizedOpenAIResponsesAntigravityClaudeReasoning(modelName, contentIndex, partIndex, reasoningIndex-1, reasoningSig)
			}
			part["thoughtSignature"] = reasoningSig.Signature
			rewrittenParts = append(rewrittenParts, part)
		}

		if len(rewrittenParts) == 0 {
			changed = true
			continue
		}
		content["parts"] = rewrittenParts
		rewrittenContents = append(rewrittenContents, content)
	}

	if !changed {
		return geminiJSON
	}

	root["contents"] = rewrittenContents
	out, err := json.Marshal(root)
	if err != nil {
		return geminiJSON
	}
	return out
}

func antigravityClaudeReasoningSignatures(inputRawJSON []byte) []antigravityClaudeReasoningSignature {
	input := gjson.GetBytes(inputRawJSON, "input")
	if !input.IsArray() {
		return nil
	}

	signatures := make([]antigravityClaudeReasoningSignature, 0)
	input.ForEach(func(_, item gjson.Result) bool {
		itemType := item.Get("type").String()
		if itemType == "" && item.Get("role").Exists() {
			itemType = "message"
		}
		if itemType != "reasoning" {
			return true
		}

		rawSignatureResult := item.Get("encrypted_content")
		rawSignature := rawSignatureResult.String()
		signature, ok := sigcompat.CompatibleAntigravityClaudeThinkingSignature(rawSignature)
		reasoningSignature := antigravityClaudeReasoningSignature{
			HasRawSignature:  rawSignatureResult.Exists(),
			RawSignatureLen:  len(rawSignature),
			DetectedProvider: sigcompat.SignatureProviderUnknown,
		}
		if rawSignature != "" {
			reasoningSignature.DetectedProvider = sigcompat.DetectSignatureProviderForBlock(rawSignature, sigcompat.SignatureBlockKindClaudeThinking)
		}
		if ok {
			reasoningSignature.Signature = signature
		}
		signatures = append(signatures, reasoningSignature)
		return true
	})
	return signatures
}

func logDroppedOpenAIResponsesAntigravityClaudeReasoning(modelName string, contentIndex, partIndex, reasoningIndex int, sig antigravityClaudeReasoningSignature) {
	_, _, _, _, _ = modelName, contentIndex, partIndex, reasoningIndex, sig
}

func logDroppedOpenAIResponsesAntigravityClaudeEmptyReasoning(modelName string, contentIndex, partIndex, reasoningIndex int, sig antigravityClaudeReasoningSignature) {
	_, _, _, _, _ = modelName, contentIndex, partIndex, reasoningIndex, sig
}

func logNormalizedOpenAIResponsesAntigravityClaudeReasoning(modelName string, contentIndex, partIndex, reasoningIndex int, sig antigravityClaudeReasoningSignature) {
	_, _, _, _, _ = modelName, contentIndex, partIndex, reasoningIndex, sig
}
