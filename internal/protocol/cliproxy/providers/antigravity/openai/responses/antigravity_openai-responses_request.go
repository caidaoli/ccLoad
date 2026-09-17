package responses

import (
	"encoding/json"
	"fmt"
	"strings"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"
	coreresponses "ccLoad/internal/protocol/cliproxy/gemini/openai/responses"
	antigravitygemini "ccLoad/internal/protocol/cliproxy/providers/antigravity/gemini"
	sigcompat "ccLoad/internal/protocol/cliproxy/signature"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertOpenAIResponsesRequestToAntigravity converts a Responses request to the Antigravity Gemini envelope.
func ConvertOpenAIResponsesRequestToAntigravity(modelName string, inputRawJSON []byte, stream bool) []byte {
	if WantsWebSearch(inputRawJSON) {
		return buildAntigravityResponsesWebSearchRequest(modelName, inputRawJSON, stream)
	}
	rawJSON := inputRawJSON
	rawJSON = coreresponses.ConvertOpenAIResponsesRequestToGemini(modelName, rawJSON, stream)
	rawJSON = stripAntigravityResponsesGoogleSearch(rawJSON)
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
	if !gjson.ValidBytes(geminiJSON) || !gjson.ParseBytes(geminiJSON).IsObject() {
		return geminiJSON
	}

	reasoningSignatures := antigravityClaudeReasoningSignatures(inputRawJSON)
	if len(reasoningSignatures) == 0 {
		return geminiJSON
	}

	contents := gjson.GetBytes(geminiJSON, "contents")
	if !contents.IsArray() {
		return geminiJSON
	}

	reasoningIndex := 0
	changed := false
	type contentRewrite struct {
		index       int
		deleteParts []int
		setParts    map[int]string
		remove      bool
	}
	rewrites := make([]contentRewrite, 0)
	for contentIndex, content := range contents.Array() {
		if !content.IsObject() {
			continue
		}
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		rewrite := contentRewrite{index: contentIndex, setParts: make(map[int]string)}
		for partIndex, part := range parts.Array() {
			thought := part.Get("thought")
			if !part.IsObject() || thought.Type != gjson.True {
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
				rewrite.deleteParts = append(rewrite.deleteParts, partIndex)
				continue
			}
			if strings.TrimSpace(part.Get("text").String()) == "" {
				changed = true
				logDroppedOpenAIResponsesAntigravityClaudeEmptyReasoning(modelName, contentIndex, partIndex, reasoningIndex-1, reasoningSig)
				rewrite.deleteParts = append(rewrite.deleteParts, partIndex)
				continue
			}

			if part.Get("thoughtSignature").String() != reasoningSig.Signature {
				changed = true
				logNormalizedOpenAIResponsesAntigravityClaudeReasoning(modelName, contentIndex, partIndex, reasoningIndex-1, reasoningSig)
				rewrite.setParts[partIndex] = reasoningSig.Signature
			}
		}
		if len(rewrite.deleteParts) == len(parts.Array()) {
			changed = true
			rewrite.remove = true
		}
		if len(rewrite.deleteParts) > 0 || len(rewrite.setParts) > 0 {
			rewrites = append(rewrites, rewrite)
		}
	}

	if !changed {
		return geminiJSON
	}

	updated := geminiJSON
	for rewriteIndex := len(rewrites) - 1; rewriteIndex >= 0; rewriteIndex-- {
		rewrite := rewrites[rewriteIndex]
		for partIndex, signature := range rewrite.setParts {
			var err error
			updated, err = sjson.SetBytes(updated, fmt.Sprintf("contents.%d.parts.%d.thoughtSignature", rewrite.index, partIndex), signature)
			if err != nil {
				return geminiJSON
			}
		}
		for partIndex := len(rewrite.deleteParts) - 1; partIndex >= 0; partIndex-- {
			var err error
			updated, err = sjson.DeleteBytes(updated, fmt.Sprintf("contents.%d.parts.%d", rewrite.index, rewrite.deleteParts[partIndex]))
			if err != nil {
				return geminiJSON
			}
		}
		if rewrite.remove {
			var err error
			updated, err = sjson.DeleteBytes(updated, fmt.Sprintf("contents.%d", rewrite.index))
			if err != nil {
				return geminiJSON
			}
		}
	}
	return updated
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

const antigravityWebSearchSystemInstruction = "You are a search engine bot. You will be given a query from a user. Your task is to search the web for relevant information that will help the user. You MUST perform a web search. Do not respond or interact with the user, please respond as if they typed the query into a search bar."

// WantsWebSearch selects the dedicated search request; mixed function tools and
// explicit non-search tool choices keep the normal model and tool history.
func WantsWebSearch(payload []byte) bool {
	root := gjson.ParseBytes(payload)
	return coreresponses.HasOnlyResponsesWebSearchTools(root) && coreresponses.AllowsResponsesWebSearchToolChoice(root)
}

func buildAntigravityResponsesWebSearchRequest(model string, payload []byte, stream bool) []byte {
	includedDomains := coreresponses.ExtractResponsesWebSearchAllowedDomains(gjson.ParseBytes(payload))
	rawJSON := coreresponses.ConvertOpenAIResponsesRequestToGemini(model, payload, stream)
	rawJSON = rewriteOpenAIResponsesReasoningForAntigravityClaude(model, payload, rawJSON)
	out := antigravitygemini.ConvertGeminiRequestToAntigravity(model, rawJSON, stream)
	out, _ = sjson.SetBytes(out, "requestType", "web_search")
	out = ensureAntigravityResponsesWebSearchTool(out, includedDomains)
	out = ensureAntigravityResponsesWebSearchSystemInstruction(out)
	return out
}

func ensureAntigravityResponsesWebSearchTool(payload []byte, includedDomains []string) []byte {
	googleSearchTool := []byte(`{"googleSearch":{"enhancedContent":{"imageSearch":{"maxResultCount":5}}}}`)
	if len(includedDomains) > 0 {
		if domainsJSON, errMarshal := json.Marshal(includedDomains); errMarshal == nil {
			googleSearchTool, _ = sjson.SetRawBytes(googleSearchTool, "googleSearch.includedDomains", domainsJSON)
		}
	}

	tools := gjson.GetBytes(payload, "request.tools")
	if !tools.IsArray() {
		payload, _ = sjson.SetRawBytes(payload, "request.tools", translatorcommon.JoinRawArray([][]byte{googleSearchTool}))
		return payload
	}

	replaced := false
	filtered := make([][]byte, 0, len(tools.Array()))
	for _, tool := range tools.Array() {
		if tool.Get("googleSearch").Exists() {
			if !replaced {
				filtered = append(filtered, googleSearchTool)
				replaced = true
			}
			continue
		}
		filtered = append(filtered, []byte(tool.Raw))
	}
	if !replaced {
		filtered = append([][]byte{googleSearchTool}, filtered...)
	}
	payload, _ = sjson.SetRawBytes(payload, "request.tools", translatorcommon.JoinRawArray(filtered))
	return payload
}

func ensureAntigravityResponsesWebSearchSystemInstruction(payload []byte) []byte {
	searchPart := []byte(`{"text":""}`)
	searchPart, _ = sjson.SetBytes(searchPart, "text", antigravityWebSearchSystemInstruction)

	sys := gjson.GetBytes(payload, "request.systemInstruction")
	if !sys.Exists() {
		instr := []byte(`{"role":"user","parts":[]}`)
		instr, _ = sjson.SetRawBytes(instr, "parts", translatorcommon.JoinRawArray([][]byte{searchPart}))
		payload, _ = sjson.SetRawBytes(payload, "request.systemInstruction", instr)
		return payload
	}

	var parts [][]byte
	alreadyPresent := false
	if sys.Get("parts").IsArray() {
		for _, part := range sys.Get("parts").Array() {
			if part.Get("text").String() == antigravityWebSearchSystemInstruction {
				alreadyPresent = true
			}
			parts = append(parts, []byte(part.Raw))
		}
	}
	if !alreadyPresent {
		parts = append(parts, searchPart)
	}
	payload, _ = sjson.SetRawBytes(payload, "request.systemInstruction.parts", translatorcommon.JoinRawArray(parts))
	return payload
}

func stripAntigravityResponsesGoogleSearch(payload []byte) []byte {
	for _, path := range []string{"tools", "request.tools"} {
		tools := gjson.GetBytes(payload, path)
		if !tools.IsArray() {
			continue
		}
		var filtered [][]byte
		hasGoogleSearch := false
		for _, tool := range tools.Array() {
			if tool.Get("googleSearch").Exists() {
				hasGoogleSearch = true
				continue
			}
			filtered = append(filtered, []byte(tool.Raw))
		}
		if hasGoogleSearch {
			if len(filtered) == 0 {
				payload, _ = sjson.DeleteBytes(payload, path)
			} else {
				payload, _ = sjson.SetRawBytes(payload, path, translatorcommon.JoinRawArray(filtered))
			}
		}
	}
	return payload
}
