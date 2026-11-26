// Package openai provides request translation functionality for OpenAI to Antigravity API compatibility.
// It converts OpenAI Chat Completions requests into Antigravity compatible JSON using gjson/sjson only.
package chat_completions

import (
	"fmt"
	"strings"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"
	"ccLoad/internal/protocol/cliproxy/gemini/common"
	"ccLoad/internal/protocol/cliproxy/providers/antigravity/gemini"
	"ccLoad/internal/protocol/cliproxy/thinking"
	"ccLoad/internal/protocol/cliproxy/util"

	"github.com/MoeForge/moego/ojson"
)

const antigravityFunctionThoughtSignature = "skip_thought_signature_validator"

// ConvertOpenAIRequestToAntigravity converts an OpenAI Chat Completions request (raw JSON)
// into a complete Antigravity request JSON. All JSON construction uses sjson and lookups use ojson.
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Antigravity API format
func ConvertOpenAIRequestToAntigravity(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	return convertOpenAIRequestToAntigravity(modelName, inputRawJSON, stream)

}

// convertOpenAIRequestToAntigravity also reports a file part Antigravity cannot
// receive when it leaves a user turn with nothing to send.
func convertOpenAIRequestToAntigravity(modelName string, inputRawJSON []byte, _ bool) ([]byte, error) {
	rawJSON := inputRawJSON
	var drops translatorcommon.UserTurnDrops
	functionNameMap := util.SanitizedFunctionNameMap(rawJSON)
	toolChoice := parseAntigravityOpenAIToolChoice(ojson.GetBytes(rawJSON, "tool_choice"))
	// Base envelope (no default thinkingConfig)
	out := []byte(`{"project":"","request":{"contents":[]},"model":"gemini-2.5-pro"}`)

	// Model
	out, _ = ojson.SetBytes(out, "model", modelName)

	// Let user-provided generationConfig pass through
	if genConfig := translatorcommon.GeminiConfigObject(util.ParseGJSONBytesNoCopy(rawJSON), "generationConfig", "generation_config"); genConfig.Exists() {
		out, _ = ojson.SetRawBytes(out, "request.generationConfig", []byte(genConfig.Raw))
	}

	out = applyOpenAIThinkingCompatibilityToAntigravity(out, rawJSON)
	out = thinking.ApplyOpenAIGoogleThinking(out, rawJSON, "request.generationConfig")

	// Temperature/top_p/top_k/max_tokens/max_completion_tokens
	if tr := ojson.GetBytes(rawJSON, "temperature"); tr.Exists() && tr.Type == ojson.Number {
		out, _ = ojson.SetBytes(out, "request.generationConfig.temperature", tr.Num)
	}
	if tpr := ojson.GetBytes(rawJSON, "top_p"); tpr.Exists() && tpr.Type == ojson.Number {
		out, _ = ojson.SetBytes(out, "request.generationConfig.topP", tpr.Num)
	}
	if tkr := ojson.GetBytes(rawJSON, "top_k"); tkr.Exists() && tkr.Type == ojson.Number {
		out, _ = ojson.SetBytes(out, "request.generationConfig.topK", tkr.Num)
	}
	if maxTok := ojson.GetBytes(rawJSON, "max_tokens"); maxTok.Exists() && maxTok.Type == ojson.Number {
		out, _ = ojson.SetBytes(out, "request.generationConfig.maxOutputTokens", maxTok.Num)
	} else if mct := ojson.GetBytes(rawJSON, "max_completion_tokens"); mct.Exists() && mct.Type == ojson.Number {
		out, _ = ojson.SetBytes(out, "request.generationConfig.maxOutputTokens", mct.Num)
	}
	if stops := translatorcommon.ChatStopSequences(ojson.GetBytes(rawJSON, "stop")); len(stops) > 0 {
		out, _ = ojson.SetBytes(out, "request.generationConfig.stopSequences", stops)
	}

	// Map OpenAI response_format to Antigravity structured output settings.
	if responseFormat := ojson.GetBytes(rawJSON, "response_format"); responseFormat.Exists() {
		switch responseFormatType := strings.ToLower(strings.TrimSpace(responseFormat.Get("type").String())); responseFormatType {
		case "json_object", "json_schema":
			for _, schemaKey := range []string{"responseSchema", "responseJsonSchema", "response_schema", "response_json_schema"} {
				out, _ = ojson.DeleteBytes(out, "request.generationConfig."+schemaKey)
			}
			out, _ = ojson.SetBytes(out, "request.generationConfig.responseMimeType", "application/json")
			if responseFormatType == "json_schema" {
				if schema := responseFormat.Get("json_schema.schema"); schema.Exists() {
					out, _ = ojson.SetRawBytes(out, "request.generationConfig.responseSchema", []byte(schema.Raw))
				}
			}
		}
	}

	// Candidate count (OpenAI 'n' parameter)
	if n := ojson.GetBytes(rawJSON, "n"); n.Exists() && n.Type == ojson.Number {
		if val := n.Int(); val > 1 {
			out, _ = ojson.SetBytes(out, "request.generationConfig.candidateCount", val)
		}
	}

	// Map OpenAI modalities -> Antigravity request.generationConfig.responseModalities
	// e.g. "modalities": ["image", "text"] -> ["IMAGE", "TEXT"]
	if mods := ojson.GetBytes(rawJSON, "modalities"); mods.Exists() && mods.IsArray() {
		var responseMods []string
		for _, m := range mods.Array() {
			switch strings.ToLower(m.String()) {
			case "text":
				responseMods = append(responseMods, "TEXT")
			case "image":
				responseMods = append(responseMods, "IMAGE")
			}
		}
		if len(responseMods) > 0 {
			out, _ = ojson.SetBytes(out, "request.generationConfig.responseModalities", responseMods)
		}
	}

	// OpenRouter-style image_config support
	// If the input uses top-level image_config.aspect_ratio, map it into request.generationConfig.imageConfig.aspectRatio.
	if imgCfg := ojson.GetBytes(rawJSON, "image_config"); imgCfg.Exists() && imgCfg.IsObject() {
		if ar := imgCfg.Get("aspect_ratio"); ar.Exists() && ar.Type == ojson.String {
			out, _ = ojson.SetBytes(out, "request.generationConfig.imageConfig.aspectRatio", ar.Str)
		}
		if size := imgCfg.Get("image_size"); size.Exists() && size.Type == ojson.String {
			out, _ = ojson.SetBytes(out, "request.generationConfig.imageConfig.imageSize", size.Str)
		}
	}

	// messages -> systemInstruction + contents
	messages := ojson.GetBytes(rawJSON, "messages")
	if messages.IsArray() {
		arr := messages.Array()
		systemParts := make([][]byte, 0, 2)
		contentItems := make([][]byte, 0, len(arr))
		contentCanMergeWithToolResponse := make([]bool, 0, len(arr))
		appendContent := func(content []byte, canMerge bool) {
			contentItems = append(contentItems, content)
			contentCanMergeWithToolResponse = append(contentCanMergeWithToolResponse, canMerge)
		}

		hasEncounteredConversation := false
		for i := 0; i < len(arr); i++ {
			m := arr[i]
			role := m.Get("role").String()
			content := m.Get("content")

			if (role == "system" || role == "developer") && len(arr) > 1 && !hasEncounteredConversation {
				// system -> request.systemInstruction as a user message style
				if content.Type == ojson.String {
					systemParts = append(systemParts, antigravityOpenAITextPart(content.String()))
				} else if content.IsObject() && content.Get("type").String() == "text" {
					systemParts = append(systemParts, antigravityOpenAITextPart(content.Get("text").String()))
				} else if content.IsArray() {
					for _, contentPart := range content.Array() {
						systemParts = append(systemParts, antigravityOpenAITextPart(contentPart.Get("text").String()))
					}
				}
			} else if role == "user" || role == "system" || role == "developer" {
				hasEncounteredConversation = true
				isDemotedSystem := role == "system" || role == "developer"
				partItems := make([][]byte, 0, 4)
				if content.Type == ojson.String {
					partItems = append(partItems, antigravityOpenAITextPart(antigravityDemotedSystemText(content.String(), isDemotedSystem)))
				} else if content.IsObject() && content.Get("type").String() == "text" {
					partItems = append(partItems, antigravityOpenAITextPart(antigravityDemotedSystemText(content.Get("text").String(), isDemotedSystem)))
				} else if content.IsArray() {
					for _, item := range content.Array() {
						switch item.Get("type").String() {
						case "text":
							if text := item.Get("text").String(); text != "" {
								partItems = append(partItems, antigravityOpenAITextPart(antigravityDemotedSystemText(text, isDemotedSystem)))
							}
						case "image_url":
							// Only a base64 data URL can be inlined; a remote URL has no equivalent here.
							if mimeType, data, ok := translatorcommon.NormalizeOpenAIFileData("", "", item.Get("image_url.url").String()); ok {
								partItems = append(partItems, antigravityOpenAIInlineDataPart(mimeType, data))
							} else {
								drops.Drop("image_url")
							}
						case "video_url":
							if mimeType, data, ok := translatorcommon.NormalizeOpenAIFileData("", "", item.Get("video_url.url").String()); ok {
								partItems = append(partItems, antigravityOpenAIInlineDataPart(mimeType, data))
							} else {
								drops.Drop("video_url")
							}
						case "file":
							filename := item.Get("file.filename").String()
							fileData := item.Get("file.file_data").String()
							if mimeType, data, ok := translatorcommon.NormalizeOpenAIFileData(filename, "", fileData); ok {
								partItems = append(partItems, antigravityOpenAIInlineDataPart(mimeType, data))
							} else {
								drops.Drop("file")
							}
						case "input_audio":
							audioData := item.Get("input_audio.data").String()
							if audioData != "" {
								mimeType := antigravityOpenAIAudioMIMEType(item.Get("input_audio.format").String())
								partItems = append(partItems, antigravityOpenAIInlineDataPart(mimeType, audioData))
							} else {
								drops.Drop("input_audio")
							}
						}
					}
				}
				// Whitespace-only text is forwarded but never keeps an emptied turn alive.
				drops.EndTurn(translatorcommon.CountSendableGeminiParts(partItems))
				if len(partItems) > 0 {
					appendContent(antigravityOpenAIContent("user", partItems), role == "user")
				}
			} else if role == "assistant" {
				hasEncounteredConversation = true
				partItems := make([][]byte, 0, 4)
				if reasoningContent := m.Get("reasoning_content"); reasoningContent.Type == ojson.String && reasoningContent.String() != "" {
					part := antigravityOpenAITextPart(reasoningContent.String())
					part, _ = ojson.SetBytes(part, "thought", true)
					partItems = append(partItems, part)
				}
				if content.Type == ojson.String && content.String() != "" {
					partItems = append(partItems, antigravityOpenAITextPart(content.String()))
				} else if content.IsArray() {
					for _, item := range content.Array() {
						switch item.Get("type").String() {
						case "text":
							if text := item.Get("text").String(); text != "" {
								partItems = append(partItems, antigravityOpenAITextPart(text))
							}
						case "image_url":
							if mimeType, data, ok := translatorcommon.NormalizeOpenAIFileData("", "", item.Get("image_url.url").String()); ok {
								partItems = append(partItems, antigravityOpenAIInlineDataPart(mimeType, data))
							}
						}
					}
				}

				tcs := m.Get("tool_calls")
				if tcs.IsArray() {
					type assistantToolCall struct {
						rawID string
						id    string
						name  string
					}
					toolCalls := make([]assistantToolCall, 0)
					usedToolCallIDs := make(map[string]struct{})
					for _, tc := range tcs.Array() {
						if tc.Get("type").String() != "function" {
							continue
						}
						rawID := tc.Get("id").String()
						baseID := util.SanitizeClaudeToolID(rawID)
						functionID := baseID
						suffix := 1
						for {
							if _, exists := usedToolCallIDs[functionID]; !exists {
								usedToolCallIDs[functionID] = struct{}{}
								break
							}
							functionID = fmt.Sprintf("%s_%d", baseID, suffix)
							suffix++
						}
						functionName := util.MapSanitizedFunctionName(functionNameMap, tc.Get("function.name").String())
						if functionName == "" {
							continue
						}
						part := []byte(`{"functionCall":{"id":"","name":""}}`)
						part, _ = ojson.SetBytes(part, "functionCall.id", functionID)
						part, _ = ojson.SetBytes(part, "functionCall.name", functionName)
						part = translatorcommon.SetGeminiFunctionCallArguments(part, tc.Get("function.arguments").String(), "params")
						part, _ = ojson.SetBytes(part, "thoughtSignature", antigravityFunctionThoughtSignature)
						partItems = append(partItems, part)
						toolCalls = append(toolCalls, assistantToolCall{
							rawID: rawID,
							id:    functionID,
							name:  functionName,
						})
					}
					if len(partItems) > 0 {
						appendContent(antigravityOpenAIContent("model", partItems), false)
					}

					// Collect tool responses scoped to this assistant turn.
					turnToolResponses := map[string]string{}
					for j := i + 1; j < len(arr); j++ {
						nextRole := arr[j].Get("role").String()
						if nextRole == "assistant" {
							break
						}
						if nextRole == "tool" {
							callID := arr[j].Get("tool_call_id").String()
							if callID != "" {
								turnToolResponses[callID] = arr[j].Get("content").String()
							}
						}
					}

					responseParts := make([][]byte, 0, len(toolCalls))
					for _, call := range toolCalls {
						part := []byte(`{"functionResponse":{"id":"","name":""}}`)
						part, _ = ojson.SetBytes(part, "functionResponse.id", call.id)
						part, _ = ojson.SetBytes(part, "functionResponse.name", call.name)
						response := turnToolResponses[call.rawID]
						if response == "" {
							response = "{}"
						}
						// Keep it as a string instead of parsing it into JSON.
						// Parsing it as JSON, similar to reading a JSON file with readFile, may trigger an upstream 400 error.
						part, _ = ojson.SetBytes(part, "functionResponse.response.result", response)
						responseParts = append(responseParts, part)
					}
					if len(responseParts) > 0 {
						appendContent(antigravityOpenAIContent("user", responseParts), false)
					}
				} else if len(partItems) > 0 {
					appendContent(antigravityOpenAIContent("model", partItems), false)
				}
			}
		}
		if len(systemParts) > 0 {
			out, _ = ojson.SetRawBytes(out, "request.systemInstruction", antigravityOpenAIContent("user", systemParts))
		}
		contentItems = mergeAntigravityOpenAIToolResponseUserContents(contentItems, contentCanMergeWithToolResponse)
		out = translatorcommon.SetRawArrayItems(out, "request.contents", contentItems)
	}

	// tools -> request.tools[].functionDeclarations + request.tools[].googleSearch/codeExecution/urlContext passthrough
	tools := ojson.GetBytes(rawJSON, "tools")
	toolResults := tools.Array()
	if tools.IsArray() && len(toolResults) > 0 {
		functionDeclarations := make([][]byte, 0, len(toolResults))
		googleSearchNodes := make([][]byte, 0)
		codeExecutionNodes := make([][]byte, 0)
		urlContextNodes := make([][]byte, 0)
		for _, t := range toolResults {
			if t.Get("type").String() == "function" {
				fn := t.Get("function")
				if fn.IsObject() && toolChoice.allowsFunction(fn.Get("name").String()) {
					fnRaw := fn.Raw
					if fn.Get("parameters").Exists() {
						renamed, errRename := util.RenameKey(fnRaw, "parameters", "parametersJsonSchema")
						if errRename != nil {
							var errSet error
							fnRawBytes, errSet := ojson.SetBytes([]byte(fnRaw), "parametersJsonSchema.type", "object")
							if errSet != nil {
								continue
							}
							fnRaw = string(fnRawBytes)
							fnRawBytes, errSet = ojson.SetRawBytes([]byte(fnRaw), "parametersJsonSchema.properties", []byte(`{}`))
							if errSet != nil {
								continue
							}
							fnRaw = string(fnRawBytes)
						} else {
							fnRaw = renamed
						}
					} else {
						var errSet error
						fnRawBytes, errSet := ojson.SetBytes([]byte(fnRaw), "parametersJsonSchema.type", "object")
						if errSet != nil {
							continue
						}
						fnRaw = string(fnRawBytes)
						fnRawBytes, errSet = ojson.SetRawBytes([]byte(fnRaw), "parametersJsonSchema.properties", []byte(`{}`))
						if errSet != nil {
							continue
						}
						fnRaw = string(fnRawBytes)
					}
					fnRawBytes := []byte(fnRaw)
					nameResult := fn.Get("name")
					originalName := nameResult.String()
					mappedName := util.MapSanitizedFunctionName(functionNameMap, originalName)
					if nameResult.Type != ojson.String || mappedName != originalName {
						fnRawBytes, _ = ojson.SetBytes(fnRawBytes, "name", mappedName)
					}
					if ojson.GetBytes(fnRawBytes, "strict").Exists() {
						fnRawBytes, _ = ojson.DeleteBytes(fnRawBytes, "strict")
					}
					functionDeclarations = append(functionDeclarations, fnRawBytes)
				}
			}
			gs := t.Get("google_search")
			if !gs.Exists() {
				gs = t.Get("googleSearch")
			}
			if toolChoice.allowsNativeTool("google_search") && (gs.Exists() || isAntigravityWebSearchName(t.Get("type").String())) {
				googleToolNode := []byte(`{}`)
				config := gs.Raw
				if !gs.Exists() {
					config = "{}"
				}
				var errSet error
				googleToolNode, errSet = ojson.SetRawBytes(googleToolNode, "googleSearch", []byte(config))
				if errSet != nil {
					continue
				}
				googleSearchNodes = append(googleSearchNodes, googleToolNode)
			}
			if ce := t.Get("code_execution"); ce.Exists() && toolChoice.allowsNativeTool("code_execution") {
				codeToolNode := []byte(`{}`)
				var errSet error
				codeToolNode, errSet = ojson.SetRawBytes(codeToolNode, "codeExecution", []byte(ce.Raw))
				if errSet != nil {
					continue
				}
				codeExecutionNodes = append(codeExecutionNodes, codeToolNode)
			}
			if uc := t.Get("url_context"); uc.Exists() && toolChoice.allowsNativeTool("url_context") {
				urlToolNode := []byte(`{}`)
				var errSet error
				urlToolNode, errSet = ojson.SetRawBytes(urlToolNode, "urlContext", []byte(uc.Raw))
				if errSet != nil {
					continue
				}
				urlContextNodes = append(urlContextNodes, urlToolNode)
			}
		}
		deduplicated := util.DeduplicateFunctionDeclarations(translatorcommon.JoinRawArray(functionDeclarations))
		hasFunction := len(deduplicated) > 2
		if hasFunction || len(googleSearchNodes) > 0 || len(codeExecutionNodes) > 0 || len(urlContextNodes) > 0 {
			toolItems := make([][]byte, 0, 1+len(googleSearchNodes)+len(codeExecutionNodes)+len(urlContextNodes))
			if hasFunction {
				functionToolNode := []byte(`{"functionDeclarations":[]}`)
				functionToolNode, _ = ojson.SetRawBytes(functionToolNode, "functionDeclarations", deduplicated)
				toolItems = append(toolItems, functionToolNode)
			}
			toolItems = append(toolItems, googleSearchNodes...)
			toolItems = append(toolItems, codeExecutionNodes...)
			toolItems = append(toolItems, urlContextNodes...)
			out, _ = ojson.SetRawBytes(out, "request.tools", translatorcommon.JoinRawArray(toolItems))
		}
	}

	out = toolChoice.apply(out, functionNameMap)
	if strings.Contains(strings.ToLower(modelName), "claude") {
		out = gemini.SanitizeAntigravityClaudeGeminiRequestSignatures(modelName, out)
	}
	return common.AttachDefaultSafetySettings(out, "request.safetySettings"), drops.Err()
}

func antigravityOpenAITextPart(text string) []byte {
	part := []byte(`{"text":""}`)
	part, _ = ojson.SetBytes(part, "text", text)
	return part
}

func antigravityOpenAIInlineDataPart(mimeType, data string) []byte {
	part := []byte(`{"inlineData":{"mimeType":"","data":""}}`)
	part, _ = ojson.SetBytes(part, "inlineData.mimeType", mimeType)
	part, _ = ojson.SetBytes(part, "inlineData.data", data)
	return part
}

func mergeAntigravityOpenAIToolResponseUserContents(contents [][]byte, canMerge []bool) [][]byte {
	merged := make([][]byte, 0, len(contents))
	for i := 0; i < len(contents); i++ {
		j := i + 1
		if translatorcommon.ContentHasGeminiFunctionResponse(contents[i]) {
			for j < len(contents) && canMerge[j] {
				j++
			}
		}
		if j == i+1 {
			merged = append(merged, contents[i])
			continue
		}
		parts := make([][]byte, 0, j-i)
		for _, current := range contents[i:j] {
			for _, part := range ojson.GetBytesNoCopy(current, "parts").Array() {
				parts = append(parts, []byte(part.Raw))
			}
		}
		merged = append(merged, antigravityOpenAIContent("user", parts))
		i = j - 1
	}
	return merged
}

func antigravityOpenAIContent(role string, parts [][]byte) []byte {
	content := []byte(`{"role":"","parts":[]}`)
	content, _ = ojson.SetBytes(content, "role", role)
	content, _ = ojson.SetRawBytes(content, "parts", translatorcommon.JoinRawArray(parts))
	return content
}

func antigravityOpenAIAudioMIMEType(format string) string {
	switch format {
	case "mp3":
		return "audio/mpeg"
	case "ogg":
		return "audio/ogg"
	case "flac":
		return "audio/flac"
	case "aac":
		return "audio/aac"
	case "webm":
		return "audio/webm"
	case "pcm16":
		return "audio/pcm"
	case "g711_ulaw", "g711_alaw":
		return "audio/basic"
	case "", "wav":
		return "audio/wav"
	default:
		return "audio/" + format
	}
}

type antigravityOpenAIToolChoice struct {
	mode         string
	name         string
	allowsSearch bool
	functions    map[string]struct{}
	nativeTools  map[string]struct{}
}

func parseAntigravityOpenAIToolChoice(value ojson.Result) antigravityOpenAIToolChoice {
	var choice antigravityOpenAIToolChoice
	kind := value.String()
	if value.IsObject() {
		kind = value.Get("type").String()
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case "":
		choice.allowsSearch = value.Type == ojson.Null || value.Type == ojson.String || value.IsObject()
	case "none":
		choice.mode = "NONE"
	case "auto":
		choice.mode = "AUTO"
		choice.allowsSearch = true
	case "required", "any":
		choice.mode = "ANY"
		choice.allowsSearch = true
	case "function", "tool":
		if value.IsObject() {
			choice.name = strings.TrimSpace(value.Get("function.name").String())
			if choice.name == "" {
				choice.name = strings.TrimSpace(value.Get("name").String())
			}
			choice.mode = "NONE"
			if choice.name != "" {
				choice.mode = "ANY"
			}
			// Claude uses a named tool choice for its typed built-in web search.
			choice.allowsSearch = kind == "tool" && choice.name == "web_search"
		}
	case "allowed_tools":
		if value.IsObject() {
			return parseAntigravityOpenAIAllowedTools(value)
		}
	default:
		choice.allowsSearch = isAntigravityWebSearchName(kind)
	}
	return choice
}

func parseAntigravityOpenAIAllowedTools(value ojson.Result) antigravityOpenAIToolChoice {
	choice := antigravityOpenAIToolChoice{
		mode:        "NONE",
		functions:   make(map[string]struct{}),
		nativeTools: make(map[string]struct{}),
	}
	mode := strings.TrimSpace(value.Get("allowed_tools.mode").String())
	if mode == "" {
		mode = strings.TrimSpace(value.Get("mode").String())
	}
	switch strings.ToLower(mode) {
	case "", "auto":
		choice.mode = "AUTO"
	case "required", "any":
		choice.mode = "ANY"
	}

	tools := value.Get("allowed_tools.tools")
	if !tools.IsArray() || tools.Get("#").Int() == 0 {
		tools = value.Get("tools")
	}
	if !tools.IsArray() {
		return choice
	}
	for _, tool := range tools.Array() {
		kind := strings.ToLower(strings.TrimSpace(tool.Get("type").String()))
		if isAntigravityWebSearchName(kind) || tool.Get("google_search").Exists() || tool.Get("googleSearch").Exists() {
			choice.nativeTools["google_search"] = struct{}{}
		}
		if kind == "code_execution" || tool.Get("code_execution").Exists() {
			choice.nativeTools["code_execution"] = struct{}{}
		}
		if kind == "url_context" || tool.Get("url_context").Exists() {
			choice.nativeTools["url_context"] = struct{}{}
		}
		if kind == "function" || kind == "" {
			name := strings.TrimSpace(tool.Get("function.name").String())
			if name == "" {
				name = strings.TrimSpace(tool.Get("name").String())
			}
			if name != "" {
				choice.functions[name] = struct{}{}
			}
		}
	}
	choice.allowsSearch = choice.allowsNativeTool("google_search")
	return choice
}

func (choice antigravityOpenAIToolChoice) allowsFunction(name string) bool {
	_, allowed := choice.functions[name]
	return choice.functions == nil || allowed
}

func (choice antigravityOpenAIToolChoice) allowsNativeTool(kind string) bool {
	if choice.mode == "NONE" || choice.name != "" {
		return false
	}
	_, allowed := choice.nativeTools[kind]
	return choice.nativeTools == nil || allowed
}

// WantsWebSearch reports whether the effective tools can use Antigravity's
// dedicated search request. App callers also pass native Gemini and Claude tool
// declarations, where there is no OpenAI allowlist to apply.
func WantsWebSearch(payload []byte) bool {
	root := ojson.ParseBytes(payload)
	choice := parseAntigravityOpenAIToolChoice(root.Get("tool_choice"))
	tools := root.Get("tools")
	if !choice.allowsSearch || !tools.IsArray() {
		return false
	}
	hasSearch := false
	for _, tool := range tools.Array() {
		if !tool.IsObject() {
			return false
		}
		typedSearch := isAntigravityWebSearchName(tool.Get("type").String())
		searchConfig := tool.Get("google_search").Exists() || tool.Get("googleSearch").Exists()
		if choice.functions == nil {
			if !typedSearch && (!searchConfig || len(tool.Map()) != 1) {
				return false
			}
		} else {
			if tool.Get("type").String() == "function" && choice.allowsFunction(tool.Get("function.name").String()) {
				return false
			}
			for _, kind := range []string{"code_execution", "url_context"} {
				if tool.Get(kind).Exists() && choice.allowsNativeTool(kind) {
					return false
				}
			}
		}
		hasSearch = hasSearch || typedSearch || searchConfig
	}
	return hasSearch
}

func isAntigravityWebSearchName(value string) bool {
	name := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(name, "web_search") || name == "google_search"
}

func (choice antigravityOpenAIToolChoice) apply(out []byte, functionNameMap map[string]string) []byte {
	mode := choice.mode
	if mode == "" {
		return out
	}

	var allowedNames []string
	if choice.functions != nil && mode != "NONE" {
		hasNativeTools := false
		for _, tool := range ojson.GetBytes(out, "request.tools").Array() {
			declarations := tool.Get("functionDeclarations")
			if !declarations.Exists() {
				hasNativeTools = true
			}
			for _, declaration := range declarations.Array() {
				allowedNames = append(allowedNames, declaration.Get("name").String())
			}
		}
		if len(allowedNames) == 0 {
			if hasNativeTools {
				return out
			}
			mode = "NONE"
		}
	} else if choice.name != "" {
		allowedNames = append(allowedNames, util.MapSanitizedFunctionName(functionNameMap, choice.name))
	}

	out, _ = ojson.SetBytes(out, "request.toolConfig.functionCallingConfig.mode", mode)
	if mode == "ANY" && len(allowedNames) > 0 {
		out, _ = ojson.SetBytes(out, "request.toolConfig.functionCallingConfig.allowedFunctionNames", allowedNames)
	}
	return out
}

func applyOpenAIThinkingCompatibilityToAntigravity(out []byte, rawJSON []byte) []byte {
	out = normalizeAntigravityOpenAIThinkingConfig(out)
	includeThoughts, explicit := antigravityOpenAISummaryIntent(rawJSON)
	if explicit {
		out, _ = ojson.SetBytes(out, "request.generationConfig.thinkingConfig.includeThoughts", includeThoughts)
	}
	return out
}

func antigravityOpenAISummaryIntent(rawJSON []byte) (bool, bool) {
	// Google's extension is authoritative when multiple compatibility aliases are
	// present. Keep this parser local and pure; the upstream thinking package also
	// owns runtime model activation, which does not belong in a wire adapter.
	for _, path := range []string{
		"extra_body.google.thinking_config.include_thoughts",
		"extra_body.google.thinking_config.includeThoughts",
		"extra_body.google.thinkingConfig.include_thoughts",
		"extra_body.google.thinkingConfig.includeThoughts",
		"extra_body.extra_body.google.thinking_config.include_thoughts",
		"extra_body.extra_body.google.thinking_config.includeThoughts",
		"google.thinking_config.include_thoughts",
		"google.thinking_config.includeThoughts",
		"thinking.includeThoughts",
		"thinking.include_thoughts",
		"reasoning.includeThoughts",
		"reasoning.include_thoughts",
		"generationConfig.thinkingConfig.includeThoughts",
		"generationConfig.thinkingConfig.include_thoughts",
		"generation_config.thinking_config.include_thoughts",
		"generation_config.thinking_config.includeThoughts",
	} {
		if value := ojson.GetBytes(rawJSON, path); value.IsBool() {
			return value.Bool(), true
		}
	}
	for _, path := range []string{"reasoning.summary", "reasoning.generate_summary"} {
		value := strings.ToLower(strings.TrimSpace(ojson.GetBytes(rawJSON, path).String()))
		switch value {
		case "none", "disabled":
			return false, true
		case "auto", "concise", "detailed":
			return true, true
		}
	}
	if value := ojson.GetBytes(rawJSON, "reasoning.exclude"); value.IsBool() {
		return !value.Bool(), true
	}
	if value := ojson.GetBytes(rawJSON, "include_reasoning"); value.IsBool() {
		return value.Bool(), true
	}
	if value := ojson.GetBytes(rawJSON, "reasoning.enabled"); value.IsBool() {
		return value.Bool(), true
	}
	effort := strings.ToLower(strings.TrimSpace(ojson.GetBytes(rawJSON, "reasoning_effort").String()))
	if effort == "" {
		return false, false
	}
	return effort != "none", true
}

func normalizeAntigravityOpenAIThinkingConfig(out []byte) []byte {
	for _, prefix := range []string{
		"request.generationConfig.thinking_config",
		"request.generationConfig.thinkingConfig",
	} {
		if sourcePath := prefix + ".includeThoughts"; ojson.GetBytes(out, sourcePath).Exists() {
			includeThoughts := ojson.GetBytes(out, sourcePath)
			out = setAntigravityOpenAIBoolResultIfValid(out, "request.generationConfig.thinkingConfig.includeThoughts", includeThoughts)
			if includeThoughts.Type != ojson.True && includeThoughts.Type != ojson.False {
				out, _ = ojson.DeleteBytes(out, sourcePath)
			}
		}
		if sourcePath := prefix + ".include_thoughts"; ojson.GetBytes(out, sourcePath).Exists() {
			includeThoughts := ojson.GetBytes(out, sourcePath)
			out = setAntigravityOpenAIBoolResultIfValid(out, "request.generationConfig.thinkingConfig.includeThoughts", includeThoughts)
			if includeThoughts.Type != ojson.True && includeThoughts.Type != ojson.False {
				out, _ = ojson.DeleteBytes(out, sourcePath)
			}
		}
		if thinkingLevel := ojson.GetBytes(out, prefix+".thinkingLevel"); thinkingLevel.Exists() {
			out = setAntigravityOpenAIRawIfDifferent(out, "request.generationConfig.thinkingConfig.thinkingLevel", thinkingLevel)
		}
		if thinkingLevel := ojson.GetBytes(out, prefix+".thinking_level"); thinkingLevel.Exists() {
			out = setAntigravityOpenAIRawIfDifferent(out, "request.generationConfig.thinkingConfig.thinkingLevel", thinkingLevel)
		}
		if thinkingBudget := ojson.GetBytes(out, prefix+".thinkingBudget"); thinkingBudget.Exists() {
			out = setAntigravityOpenAIRawIfDifferent(out, "request.generationConfig.thinkingConfig.thinkingBudget", thinkingBudget)
		}
		if thinkingBudget := ojson.GetBytes(out, prefix+".thinking_budget"); thinkingBudget.Exists() {
			out = setAntigravityOpenAIRawIfDifferent(out, "request.generationConfig.thinkingConfig.thinkingBudget", thinkingBudget)
		}
	}

	for _, path := range []string{
		"request.generationConfig.includeThoughts",
		"request.generationConfig.include_thoughts",
	} {
		if includeThoughts := ojson.GetBytes(out, path); includeThoughts.Exists() {
			out = setAntigravityOpenAIBoolResultIfValid(out, "request.generationConfig.thinkingConfig.includeThoughts", includeThoughts)
		}
	}

	for _, path := range []string{
		"request.generationConfig.thinking_config",
		"request.generationConfig.thinkingConfig.include_thoughts",
		"request.generationConfig.thinkingConfig.thinking_level",
		"request.generationConfig.thinkingConfig.thinking_budget",
		"request.generationConfig.includeThoughts",
		"request.generationConfig.include_thoughts",
	} {
		if ojson.GetBytes(out, path).Exists() {
			out, _ = ojson.DeleteBytes(out, path)
		}
	}

	return out
}

func setAntigravityOpenAIBoolResultIfValid(out []byte, path string, value ojson.Result) []byte {
	switch value.Type {
	case ojson.True:
		return setAntigravityOpenAIBoolIfDifferent(out, path, true)
	case ojson.False:
		return setAntigravityOpenAIBoolIfDifferent(out, path, false)
	default:
		return out
	}
}

func setAntigravityOpenAIBoolIfDifferent(out []byte, path string, value bool) []byte {
	current := ojson.GetBytes(out, path)
	if value && current.Type == ojson.True || !value && current.Type == ojson.False {
		return out
	}
	updated, errSet := ojson.SetBytes(out, path, value)
	if errSet != nil {
		return out
	}
	return updated
}

func setAntigravityOpenAIRawIfDifferent(out []byte, path string, value ojson.Result) []byte {
	current := ojson.GetBytes(out, path)
	if current.Exists() && current.Raw == value.Raw {
		return out
	}
	updated, errSet := ojson.SetRawBytes(out, path, []byte(value.Raw))
	if errSet != nil {
		return out
	}
	return updated
}

// antigravityDemotedSystemText wraps a demoted mid-session system or developer
// message in the <system-reminder> envelope so non-Claude upstream models treat it
// as a directive rather than user speech.
func antigravityDemotedSystemText(text string, isDemoted bool) string {
	if !isDemoted || strings.TrimSpace(text) == "" {
		return text
	}
	return translatorcommon.SystemReminderText(text)
}
