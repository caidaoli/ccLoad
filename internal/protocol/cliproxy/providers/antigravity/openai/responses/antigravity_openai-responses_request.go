package responses

import (
	"encoding/json"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"
	coreresponses "ccLoad/internal/protocol/cliproxy/gemini/openai/responses"
	antigravitygemini "ccLoad/internal/protocol/cliproxy/providers/antigravity/gemini"

	"github.com/MoeForge/moego/ojson"
)

// ConvertOpenAIResponsesRequestToAntigravity converts a Responses request to the Antigravity Gemini envelope.
func ConvertOpenAIResponsesRequestToAntigravity(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	if WantsWebSearch(inputRawJSON) {
		return buildAntigravityResponsesWebSearchRequest(modelName, inputRawJSON, stream)
	}
	rawJSON, errConvert := coreresponses.ConvertOpenAIResponsesRequestToGeminiForAntigravity(modelName, inputRawJSON)
	rawJSON = stripAntigravityResponsesGoogleSearch(rawJSON)
	out, _ := antigravitygemini.ConvertGeminiRequestToAntigravity(modelName, rawJSON, stream)
	return out, errConvert
}

const antigravityWebSearchSystemInstruction = "You are a search engine bot. You will be given a query from a user. Your task is to search the web for relevant information that will help the user. You MUST perform a web search. Do not respond or interact with the user, please respond as if they typed the query into a search bar."

// WantsWebSearch selects the dedicated search request; mixed function tools and
// explicit non-search tool choices keep the normal model and tool history.
func WantsWebSearch(payload []byte) bool {
	root := ojson.ParseBytes(payload)
	return coreresponses.HasOnlyResponsesWebSearchTools(root) && coreresponses.AllowsResponsesWebSearchToolChoice(root)
}

func buildAntigravityResponsesWebSearchRequest(model string, payload []byte, stream bool) ([]byte, error) {
	includedDomains := coreresponses.ExtractResponsesWebSearchAllowedDomains(ojson.ParseBytes(payload))
	rawJSON, errConvert := coreresponses.ConvertOpenAIResponsesRequestToGeminiForAntigravity(model, payload)
	rawJSON = stripAntigravityServerSideToolInvocations(rawJSON)
	out, _ := antigravitygemini.ConvertGeminiRequestToAntigravity(model, rawJSON, stream)
	out, _ = ojson.SetBytes(out, "requestType", "web_search")
	out = ensureAntigravityResponsesWebSearchTool(out, includedDomains)
	out = ensureAntigravityResponsesWebSearchSystemInstruction(out)
	return out, errConvert
}

func ensureAntigravityResponsesWebSearchTool(payload []byte, includedDomains []string) []byte {
	googleSearchTool := []byte(`{"googleSearch":{"enhancedContent":{"imageSearch":{"maxResultCount":5}}}}`)
	if len(includedDomains) > 0 {
		if domainsJSON, errMarshal := json.Marshal(includedDomains); errMarshal == nil {
			googleSearchTool, _ = ojson.SetRawBytes(googleSearchTool, "googleSearch.includedDomains", domainsJSON)
		}
	}

	tools := ojson.GetBytes(payload, "request.tools")
	if !tools.IsArray() {
		payload, _ = ojson.SetRawBytes(payload, "request.tools", translatorcommon.JoinRawArray([][]byte{googleSearchTool}))
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
	payload, _ = ojson.SetRawBytes(payload, "request.tools", translatorcommon.JoinRawArray(filtered))
	return payload
}

func ensureAntigravityResponsesWebSearchSystemInstruction(payload []byte) []byte {
	searchPart := []byte(`{"text":""}`)
	searchPart, _ = ojson.SetBytes(searchPart, "text", antigravityWebSearchSystemInstruction)

	sys := ojson.GetBytes(payload, "request.systemInstruction")
	if !sys.Exists() {
		instr := []byte(`{"role":"user","parts":[]}`)
		instr, _ = ojson.SetRawBytes(instr, "parts", translatorcommon.JoinRawArray([][]byte{searchPart}))
		payload, _ = ojson.SetRawBytes(payload, "request.systemInstruction", instr)
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
	payload, _ = ojson.SetRawBytes(payload, "request.systemInstruction.parts", translatorcommon.JoinRawArray(parts))
	return payload
}

func stripAntigravityResponsesGoogleSearch(payload []byte) []byte {
	for _, path := range []string{"tools", "request.tools"} {
		tools := ojson.GetBytes(payload, path)
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
				payload, _ = ojson.DeleteBytes(payload, path)
			} else {
				payload, _ = ojson.SetRawBytes(payload, path, translatorcommon.JoinRawArray(filtered))
			}
		}
	}
	return stripAntigravityServerSideToolInvocations(payload)
}

// stripAntigravityServerSideToolInvocations removes includeServerSideToolInvocations
// injected by the shared Gemini Responses translator because Antigravity does not
// support the includeServerSideToolInvocations parameter.
func stripAntigravityServerSideToolInvocations(payload []byte) []byte {
	for _, toolConfigPath := range []string{"toolConfig", "request.toolConfig"} {
		flagPath := toolConfigPath + ".includeServerSideToolInvocations"
		if !ojson.GetBytes(payload, flagPath).Exists() {
			continue
		}
		payload, _ = ojson.DeleteBytes(payload, flagPath)
		if tc := ojson.GetBytes(payload, toolConfigPath); tc.IsObject() && len(tc.Map()) == 0 {
			payload, _ = ojson.DeleteBytes(payload, toolConfigPath)
		}
	}
	return payload
}
