// Package gemini provides request translation functionality for Codex to Gemini API compatibility.
// It handles parsing and transforming Codex API requests into Gemini API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Codex API format and Gemini API's expected format.
package gemini

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"
	"ccLoad/internal/protocol/cliproxy/thinking"
	"ccLoad/internal/protocol/cliproxy/util"

	"github.com/MoeForge/moego/ojson"
)

// ConvertGeminiRequestToCodex parses and transforms a Gemini API request into Codex API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the Codex API.
// The function performs comprehensive transformation including:
// 1. Model name mapping and generation configuration extraction
// 2. System instruction conversion to Codex format
// 3. Message content conversion with proper role mapping
// 4. Tool call and tool result handling with FIFO queue for ID matching
// 5. Tool declaration and tool choice configuration mapping
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the Gemini API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in Codex API format
func ConvertGeminiRequestToCodex(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	rawJSON := inputRawJSON
	// Base template
	out := []byte(`{"model":"","instructions":"","input":[]}`)

	root := ojson.ParseBytes(rawJSON)
	inputItems := translatorcommon.NewRawArrayItems(root.Get("contents.#").Int())

	declarations := translatorcommon.GeminiFunctionDeclarations(root)
	mode, allowedNames, err := translatorcommon.GeminiFunctionCallingOptions(root, declarations)
	if err != nil {
		return nil, err
	}
	shortMap := geminiToolShortNames(declarations)

	// helper for generating paired call IDs in the form: call_gemini_<seq>
	// Gemini uses sequential pairing across possibly multiple in-flight
	// functionCalls, so we keep a FIFO queue of generated call IDs and
	// consume them in order when functionResponses arrive.
	var pendingCallIDs []string
	callCounter := 0
	emittedThoughtSignatures := map[string]bool{}
	signatureTargetModel := translatorcommon.ResolveCodexReplayTargetModel(modelName, inputRawJSON)

	getGeminiCallID := func(value ojson.Result) string {
		if callID := strings.TrimSpace(value.Get("id").String()); callID != "" {
			return callID
		}
		return strings.TrimSpace(value.Get("call_id").String())
	}

	removePendingCallID := func(ids []string, callID string) []string {
		if callID == "" {
			return ids
		}
		for idx, pendingID := range ids {
			if pendingID == callID {
				return append(ids[:idx], ids[idx+1:]...)
			}
		}
		return ids
	}

	// Model
	out, _ = ojson.SetBytes(out, "model", modelName)
	if serviceTier := normalizeGeminiCodexServiceTier(root.Get("service_tier")); serviceTier != "" {
		out, _ = ojson.SetBytes(out, "service_tier", serviceTier)
	}

	format, outputInstruction, err := translatorcommon.GeminiResponseFormat(root, true)
	if err != nil {
		return nil, err
	}
	if format != nil {
		out, _ = ojson.SetRawBytes(out, "text.format", format)
	}
	// System instruction maps to the native Responses instructions field.
	instructions := translatorcommon.GeminiSystemInstructionText(root, "system_instruction", "systemInstruction")
	if ojson.GetBytes(format, "type").Str == "json_object" {
		outputInstruction = translatorcommon.GeminiJSONObjectInstruction
	}
	if outputInstruction != "" {
		if instructions != "" {
			instructions += "\n"
		}
		instructions += outputInstruction
	}
	if instructions != "" {
		out, _ = ojson.SetBytes(out, "instructions", instructions)
	}

	// Contents -> messages and function calls/results
	contents := root.Get("contents")
	if contents.IsArray() {
		items := contents.Array()
		// Calls of the latest model turn still awaiting a functionResponse; audio
		// relays wait until the batch is answered so tool outputs stay contiguous.
		pendingTurnCallCount := 0
		var pendingToolAudioRelay []codexGeminiToolAudioGroup
		flushPendingToolAudioRelay := func() {
			if len(pendingToolAudioRelay) == 0 {
				return
			}
			msg := []byte(`{"type":"message","role":"user","content":[]}`)
			msg, _ = ojson.SetRawBytes(msg, "content", translatorcommon.JoinRawArray(buildCodexGeminiToolAudioRelayItems(pendingToolAudioRelay)))
			inputItems = append(inputItems, msg)
			pendingToolAudioRelay = nil
		}
		for i := 0; i < len(items); i++ {
			item := items[i]
			parts := item.Get("parts")
			if !parts.IsArray() {
				continue
			}
			role := translatorcommon.GeminiMessageRole(item.Get("role").String(), parts)
			if !translatorcommon.ContentHasGeminiFunctionResponse([]byte(item.Raw)) {
				pendingTurnCallCount = 0
				flushPendingToolAudioRelay()
			}
			parr := parts.Array()
			var pendingThoughtSummaryParts [][]byte
			for j := 0; j < len(parr); j++ {
				p := parr[j]

				// A thoughtSignature is replayable reasoning state, not visible
				// content. Restore it as a reasoning item only when its detected
				// family matches this request's target model family, so a Muse
				// (or GPT/Grok) signature can never be replayed to a wrong upstream
				// through the Gemini ingress. Unmatched signatures fall through to
				// the existing hidden-thought drop below.
				if role == "assistant" {
					if rawSignature := geminiPartThoughtSignature(p); rawSignature != "" {
						if normalizedSignature, ok := normalizedGeminiThoughtSignatureForCodexTarget(signatureTargetModel, rawSignature); ok && !emittedThoughtSignatures[normalizedSignature] {
							reasoningItem := []byte(`{"type":"reasoning","encrypted_content":"","summary":[]}`)
							reasoningItem, _ = ojson.SetBytes(reasoningItem, "encrypted_content", normalizedSignature)
							summaryParts := pendingThoughtSummaryParts
							pendingThoughtSummaryParts = nil
							if translatorcommon.IsGeminiThoughtPart(p) {
								if thoughtText := p.Get("text").String(); thoughtText != "" {
									summaryPart := []byte(`{"type":"summary_text","text":""}`)
									summaryPart, _ = ojson.SetBytes(summaryPart, "text", thoughtText)
									summaryParts = append(summaryParts, summaryPart)
								}
							}
							if len(summaryParts) > 0 {
								reasoningItem, _ = ojson.SetRawBytes(reasoningItem, "summary", translatorcommon.JoinRawArray(summaryParts))
							}
							inputItems = append(inputItems, reasoningItem)
							emittedThoughtSignatures[normalizedSignature] = true
						} else {
							pendingThoughtSummaryParts = nil
						}
					} else if translatorcommon.IsGeminiThoughtPart(p) {
						if thoughtText := p.Get("text").String(); thoughtText != "" {
							summaryPart := []byte(`{"type":"summary_text","text":""}`)
							summaryPart, _ = ojson.SetBytes(summaryPart, "text", thoughtText)
							pendingThoughtSummaryParts = append(pendingThoughtSummaryParts, summaryPart)
						}
					} else {
						pendingThoughtSummaryParts = nil
					}
				}

				if translatorcommon.IsGeminiThoughtPart(p) {
					continue
				}

				// text part
				if t := p.Get("text"); t.Exists() {
					partType := "input_text"
					if role == "assistant" {
						partType = "output_text"
					}
					part := []byte(`{}`)
					part, _ = ojson.SetBytes(part, "type", partType)
					part, _ = ojson.SetBytes(part, "text", t.String())
					inputItems = append(inputItems, codexMessageWithPart(role, part))
					continue
				}

				if contentPart, ok := codexContentPartFromGeminiInlineData(p); ok {
					inputItems = append(inputItems, codexMessageWithPart(role, contentPart))
					continue
				}

				if contentPart, ok := codexContentPartFromGeminiFileData(p); ok {
					inputItems = append(inputItems, codexMessageWithPart(role, contentPart))
					continue
				}

				// function call from model
				if fc := translatorcommon.GeminiPartFunctionCall(p); fc.Exists() {
					fn := []byte(`{"type":"function_call","arguments":"{}"}`)
					if name := fc.Get("name"); name.Exists() {
						fn, _ = ojson.SetBytes(fn, "name", resolveGeminiToolName(shortMap, name.String()))
					}
					if args := fc.Get("args"); args.IsObject() {
						fn, _ = ojson.SetBytes(fn, "arguments", args.Raw)
					}
					// Reuse gateway-provided IDs when present, otherwise generate one for pairing.
					id := getGeminiCallID(fc)
					if id == "" {
						callCounter++
						id = fmt.Sprintf("call_gemini_%016d", callCounter)
					}
					fn, _ = ojson.SetBytes(fn, "call_id", id)
					pendingCallIDs = append(pendingCallIDs, id)
					inputItems = append(inputItems, fn)
					pendingTurnCallCount++
					continue
				}

				// function response from user
				if fr := translatorcommon.GeminiPartFunctionResponse(p); fr.Exists() {
					if pendingTurnCallCount > 0 {
						pendingTurnCallCount--
					}
					fno, audioItems := codexFunctionCallOutputFromGemini(fr)
					// attach the oldest queued call_id to pair the response
					// with its call. If the queue is empty, generate a new id.
					var id string
					if customID := getGeminiCallID(fr); customID != "" {
						id = customID
						pendingCallIDs = removePendingCallID(pendingCallIDs, id)
					} else if len(pendingCallIDs) > 0 {
						id = pendingCallIDs[0]
						// pop the first element
						pendingCallIDs = pendingCallIDs[1:]
					} else {
						callCounter++
						id = fmt.Sprintf("call_gemini_%016d", callCounter)
					}
					if len(audioItems) > 0 {
						pendingToolAudioRelay = append(pendingToolAudioRelay, codexGeminiToolAudioGroup{
							toolName: fr.Get("name").String(),
							callID:   id,
							items:    audioItems,
						})
					}
					fno, _ = ojson.SetBytes(fno, "call_id", id)
					inputItems = append(inputItems, fno)
					continue
				}
			}
			if pendingTurnCallCount == 0 {
				flushPendingToolAudioRelay()
			}
		}
		flushPendingToolAudioRelay()
	}

	out = translatorcommon.SetRawArrayItems(out, "input", inputItems)

	if root.Get("tools").IsArray() {
		var tools [][]byte
		for _, declaration := range declarations {
			tool := []byte(`{"type":"function"}`)
			if name := declaration.Get("name"); name.Exists() {
				tool, _ = ojson.SetBytes(tool, "name", resolveGeminiToolName(shortMap, name.String()))
			}
			if description := declaration.Get("description"); description.Exists() {
				tool, _ = ojson.SetBytes(tool, "description", description.String())
			}
			if parameters := translatorcommon.GeminiConfigObject(declaration, "parameters", "parametersJsonSchema", "parameters_json_schema"); parameters.Exists() {
				tool, _ = ojson.SetRawBytes(tool, "parameters", cleanGeminiCodexToolParameters(parameters))
			}
			tool, _ = ojson.SetBytes(tool, "strict", false)
			tools = append(tools, tool)
		}
		out, _ = ojson.SetBytes(out, "tool_choice", "auto")
		out, _ = ojson.SetRawBytes(out, "tools", translatorcommon.JoinRawArray(tools))
	}

	// Fixed flags aligning with Codex expectations
	out, _ = ojson.SetBytes(out, "parallel_tool_calls", true)
	out = setCodexToolChoiceFromGeminiToolConfig(out, mode, allowedNames, shortMap)
	genConfig := translatorcommon.GeminiConfigObject(root, "generationConfig", "generation_config")
	maxTokens, hasMaxTokens := translatorcommon.GeminiConfigInt(genConfig, 0, "maxOutputTokens", "max_output_tokens")
	if !hasMaxTokens {
		maxTokens, hasMaxTokens = translatorcommon.GeminiConfigInt(root, 0, "maxOutputTokens", "max_output_tokens", "max_tokens")
	}
	if hasMaxTokens {
		out, _ = ojson.SetBytes(out, "max_output_tokens", maxTokens)
	}

	level, budget, hasBudget := translatorcommon.GeminiThinkingConfig(root)
	effort := translatorcommon.GeminiThinkingLevel(genConfig)
	if effort == "" {
		effort = level
	}
	if effort == "" && hasBudget {
		effort, _ = thinking.ConvertBudgetToModelLevel(int(budget), modelName, "codex")
	}
	if effort == "" {
		effort = "medium"
	}
	if effort != string(thinking.LevelAuto) {
		out, _ = ojson.SetBytes(out, "reasoning.effort", effort)
	}
	// Preserve ccLoad's default reasoning summaries unless Gemini opted out.
	if translatorcommon.GeminiIncludeThoughts(root) != ojson.False {
		out, _ = ojson.SetBytes(out, "reasoning.summary", "auto")
	}
	out, _ = ojson.SetBytes(out, "stream", stream)
	out, _ = ojson.SetBytes(out, "store", false)
	out, _ = ojson.SetBytes(out, "include", []string{"reasoning.encrypted_content"})

	if input := ojson.GetBytes(out, "input"); input.IsArray() && len(input.Array()) == 0 {
		out, _ = ojson.DeleteBytes(out, "input")
	}
	return out, nil
}

// geminiPartThoughtSignature extracts a part's thought signature under either
// the camelCase or snake_case field spelling.
func geminiPartThoughtSignature(part ojson.Result) string {
	if signature := strings.TrimSpace(part.Get("thoughtSignature").String()); signature != "" {
		return signature
	}
	return strings.TrimSpace(part.Get("thought_signature").String())
}

// decodeGeminiCodexThoughtSignature unwraps the carrier written by
// encodeGeminiCodexThoughtSignature. Values without the carrier magic are
// returned unchanged, preserving legacy raw-signature echoes.
func decodeGeminiCodexThoughtSignature(value string) string {
	value = strings.TrimSpace(value)
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil || !strings.HasPrefix(string(decoded), geminiCodexThoughtSignatureCarrierMagic) {
		return value
	}
	return strings.TrimSpace(strings.TrimPrefix(string(decoded), geminiCodexThoughtSignatureCarrierMagic))
}

// normalizedGeminiThoughtSignatureForCodexTarget unwraps and normalizes a
// client-echoed thought signature for the Codex-format upstream serving
// modelName, returning the exact value that may be written as reasoning
// encrypted_content. The carrier magic written by
// encodeGeminiCodexThoughtSignature is stripped first; the family match and
// normalization rules live in translatorcommon.CodexReplaySignatureForTarget.
func normalizedGeminiThoughtSignatureForCodexTarget(modelName, rawSignature string) (string, bool) {
	return translatorcommon.CodexReplaySignatureForTarget(modelName, decodeGeminiCodexThoughtSignature(rawSignature))
}

func setCodexToolChoiceFromGeminiToolConfig(out []byte, mode string, allowedNames []string, shortMap map[string]string) []byte {
	switch mode {
	case "NONE":
		out, _ = ojson.SetBytes(out, "tool_choice", "none")
	case "AUTO":
		out, _ = ojson.SetBytes(out, "tool_choice", "auto")
	case "ANY":
		switch len(allowedNames) {
		case 0:
			out, _ = ojson.SetBytes(out, "tool_choice", "required")
		case 1:
			choice := []byte(`{"type":"function","name":""}`)
			choice, _ = ojson.SetBytes(choice, "name", shortMap[allowedNames[0]])
			out, _ = ojson.SetRawBytes(out, "tool_choice", choice)
		default:
			choice := []byte(`{"type":"allowed_tools","mode":"required","tools":[]}`)
			var tools [][]byte
			for _, name := range allowedNames {
				tool := []byte(`{"type":"function","name":""}`)
				tool, _ = ojson.SetBytes(tool, "name", shortMap[name])
				tools = append(tools, tool)
			}
			choice, _ = ojson.SetRawBytes(choice, "tools", translatorcommon.JoinRawArray(tools))
			out, _ = ojson.SetRawBytes(out, "tool_choice", choice)
		}
	}
	return out
}

func geminiToolShortNames(declarations []ojson.Result) map[string]string {
	var names []string
	for _, declaration := range declarations {
		if name := declaration.Get("name"); name.Exists() {
			names = append(names, name.String())
		}
	}
	return buildShortNameMap(names)
}

func resolveGeminiToolName(shortMap map[string]string, name string) string {
	if short, ok := shortMap[name]; ok {
		return short
	}
	return shortenNameIfNeeded(name)
}

func cleanGeminiCodexToolParameters(parameters ojson.Result) []byte {
	cleaned := []byte(util.NormalizeGeminiToolSchema(parameters.Raw))
	if parameters.Get("$schema").Exists() {
		cleaned, _ = ojson.DeleteBytes(cleaned, "$schema")
	}
	if additionalProperties := parameters.Get("additionalProperties"); additionalProperties.Type != ojson.False {
		cleaned, _ = ojson.SetBytes(cleaned, "additionalProperties", false)
	}
	return cleaned
}

func codexMessageWithPart(role string, part []byte) []byte {
	msg := []byte(`{"type":"message","role":"","content":[]}`)
	msg, _ = ojson.SetBytes(msg, "role", role)
	msg, _ = ojson.SetRawBytes(msg, "content", translatorcommon.JoinRawArray([][]byte{part}))
	return msg
}

func normalizeGeminiCodexServiceTier(serviceTier ojson.Result) string {
	if !serviceTier.Exists() || serviceTier.Type != ojson.String {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(serviceTier.String())) {
	case "priority", "fast":
		return "priority"
	}
	return ""
}

/**
 * @brief 一次工具调用返回、需改由用户消息转发的音频
 */
type codexGeminiToolAudioGroup struct {
	toolName string   ///< 工具名，用于转发说明中的归属
	callID   string   ///< 已配对的 call_id
	items    [][]byte ///< input_audio 内容块
}

/**
 * @brief 把 Gemini 工具文本表示为 Codex input_text 块
 * @param text 文本内容
 * @return input_text 内容块
 */
func codexInputTextPart(text string) []byte {
	part := []byte(`{"type":"input_text","text":""}`)
	part, _ = ojson.SetBytes(part, "text", text)
	return part
}

/**
 * @brief 构建工具音频转发用户消息的内容块
 * @param groups 按工具输出顺序收集的音频组
 * @return 首块为归属说明；多组时每组前另加工具标签，避免把音频误归给其他并行调用
 */
func buildCodexGeminiToolAudioRelayItems(groups []codexGeminiToolAudioGroup) [][]byte {
	if len(groups) == 1 {
		g := groups[0]
		var notice string
		switch {
		case g.toolName != "" && g.callID != "":
			notice = fmt.Sprintf("Audio returned by tool %s (%s):", g.toolName, g.callID)
		case g.toolName != "":
			notice = fmt.Sprintf("Audio returned by tool %s:", g.toolName)
		case g.callID != "":
			notice = fmt.Sprintf("Audio returned by tool call %s:", g.callID)
		default:
			notice = "Audio returned by the preceding tool call(s):"
		}
		relayItems := make([][]byte, 0, len(g.items)+1)
		relayItems = append(relayItems, codexInputTextPart(notice))
		return append(relayItems, g.items...)
	}
	relayItems := [][]byte{codexInputTextPart("Audio returned by the preceding tool call(s):")}
	for _, group := range groups {
		var label string
		switch {
		case group.toolName != "" && group.callID != "":
			label = fmt.Sprintf("Tool %s (%s):", group.toolName, group.callID)
		case group.toolName != "":
			label = fmt.Sprintf("Tool %s:", group.toolName)
		case group.callID != "":
			label = fmt.Sprintf("Tool call %s:", group.callID)
		default:
			label = "Tool:"
		}
		relayItems = append(relayItems, codexInputTextPart(label))
		relayItems = append(relayItems, group.items...)
	}
	return relayItems
}

/**
 * @brief 判断 Gemini functionResponse.response 是否没有可转发内容
 * @param v response 字段
 * @return 字段缺失、空对象或空白原文时返回 true
 */
func isEmptyGeminiCodexResponseObject(v ojson.Result) bool {
	if !v.Exists() {
		return true
	}
	if v.IsObject() {
		empty := true
		v.ForEach(func(_, _ ojson.Result) bool {
			empty = false
			return false
		})
		return empty
	}
	return strings.TrimSpace(v.Raw) == ""
}

/**
 * @brief 判断 response 对象是否只有 result 一个键
 * @param responseField functionResponse.response 字段
 * @return 仅含 result 时返回 true；带 error 等兄弟字段时返回 false，以便整体转发不丢信息
 */
func geminiCodexResponseHasOnlyResultKey(responseField ojson.Result) bool {
	if !responseField.IsObject() {
		return false
	}
	hasResult := false
	onlyResult := true
	responseField.ForEach(func(key, _ ojson.Result) bool {
		if key.String() == "result" {
			hasResult = true
			return true
		}
		onlyResult = false
		return false
	})
	return hasResult && onlyResult
}

/**
 * @brief 把 Gemini functionResponse 转为不含 call_id 的 Codex function_call_output
 * @param fr Gemini functionResponse 节点
 * @return function_call_output 项；有嵌套文本、图片或文件时 output 为内容块数组，否则为字符串
 * @return 需在本批工具输出之后以用户消息转发的 input_audio 块；function_call_output.output
 *         只接受 input_text/input_image/input_file
 */
func codexFunctionCallOutputFromGemini(fr ojson.Result) ([]byte, [][]byte) {
	fno := []byte(`{"type":"function_call_output"}`)
	res := fr.Get("response.result")
	resp := fr.Get("response")
	resultOnly := res.Exists() && geminiCodexResponseHasOnlyResultKey(resp)
	hasBaseText := false
	baseText := ""
	if resultOnly {
		baseText = res.String()
		hasBaseText = strings.TrimSpace(baseText) != ""
	} else if !isEmptyGeminiCodexResponseObject(resp) {
		baseText = resp.Raw
		hasBaseText = true
	}

	var nestedItems, relayedAudioItems [][]byte
	fr.Get("parts").ForEach(func(_, nested ojson.Result) bool {
		if t := nested.Get("text"); t.Exists() {
			if strings.TrimSpace(t.String()) != "" {
				nestedItems = append(nestedItems, codexInputTextPart(t.String()))
			}
			return true
		}
		if contentPart, ok := codexContentPartFromGeminiInlineData(nested); ok {
			if ojson.GetBytes(contentPart, "type").String() == "input_audio" {
				relayedAudioItems = append(relayedAudioItems, contentPart)
			} else {
				nestedItems = append(nestedItems, contentPart)
			}
			return true
		}
		if contentPart, ok := codexContentPartFromGeminiFileData(nested); ok {
			nestedItems = append(nestedItems, contentPart)
		}
		return true
	})

	if len(nestedItems) > 0 {
		outputItems := make([][]byte, 0, len(nestedItems)+1)
		if hasBaseText {
			outputItems = append(outputItems, codexInputTextPart(baseText))
		}
		outputItems = append(outputItems, nestedItems...)
		fno, _ = ojson.SetRawBytes(fno, "output", translatorcommon.JoinRawArray(outputItems))
		return fno, relayedAudioItems
	}

	switch {
	case resultOnly:
		fno, _ = ojson.SetBytes(fno, "output", res.String())
	case !isEmptyGeminiCodexResponseObject(resp):
		fno, _ = ojson.SetBytes(fno, "output", resp.Raw)
	case len(relayedAudioItems) > 0:
		fno, _ = ojson.SetBytes(fno, "output", "[Tool returned audio content; the audio follows in the next user message.]")
	case resp.Exists():
		fno, _ = ojson.SetBytes(fno, "output", resp.Raw)
	}
	return fno, relayedAudioItems
}

func codexContentPartFromGeminiInlineData(part ojson.Result) ([]byte, bool) {
	inlineData := part.Get("inlineData")
	if !inlineData.Exists() {
		inlineData = part.Get("inline_data")
	}
	if !inlineData.Exists() {
		return nil, false
	}
	mimeType := inlineData.Get("mimeType").String()
	if mimeType == "" {
		mimeType = inlineData.Get("mime_type").String()
	}
	data := inlineData.Get("data").String()
	if mimeType == "" || data == "" {
		return nil, false
	}
	lowerMimeType := strings.ToLower(mimeType)
	switch {
	case strings.HasPrefix(lowerMimeType, "image/"):
		contentPart := []byte(`{"type":"input_image","image_url":""}`)
		contentPart, _ = ojson.SetBytes(contentPart, "image_url", fmt.Sprintf("data:%s;base64,%s", mimeType, data))
		return contentPart, true
	case strings.HasPrefix(lowerMimeType, "audio/"):
		contentPart := []byte(`{"type":"input_audio","input_audio":{"data":"","format":""}}`)
		contentPart, _ = ojson.SetBytes(contentPart, "input_audio.data", data)
		contentPart, _ = ojson.SetBytes(contentPart, "input_audio.format", codexInputAudioFormatFromMIME(mimeType))
		return contentPart, true
	default:
		fileData := data
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(fileData)), "data:") {
			fileData = fmt.Sprintf("data:%s;base64,%s", mimeType, data)
		}
		contentPart := []byte(`{"type":"input_file","file_data":"","filename":""}`)
		contentPart, _ = ojson.SetBytes(contentPart, "file_data", fileData)
		contentPart, _ = ojson.SetBytes(contentPart, "filename", codexFileNameFromMIME(mimeType))
		return contentPart, true
	}
}

func codexContentPartFromGeminiFileData(part ojson.Result) ([]byte, bool) {
	fileData := part.Get("fileData")
	if !fileData.Exists() {
		fileData = part.Get("file_data")
	}
	if !fileData.Exists() {
		return nil, false
	}
	fileURI := fileData.Get("fileUri").String()
	if fileURI == "" {
		fileURI = fileData.Get("file_uri").String()
	}
	if fileURI == "" {
		return nil, false
	}
	mimeType := fileData.Get("mimeType").String()
	if mimeType == "" {
		mimeType = fileData.Get("mime_type").String()
	}
	lowerMimeType := strings.ToLower(mimeType)
	if strings.HasPrefix(lowerMimeType, "image/") {
		contentPart := []byte(`{"type":"input_image","image_url":""}`)
		contentPart, _ = ojson.SetBytes(contentPart, "image_url", fileURI)
		return contentPart, true
	}
	if strings.HasPrefix(lowerMimeType, "video/") || strings.HasPrefix(lowerMimeType, "application/") || strings.HasPrefix(lowerMimeType, "text/") {
		contentPart := []byte(`{"type":"input_file","file_url":"","filename":""}`)
		contentPart, _ = ojson.SetBytes(contentPart, "file_url", fileURI)
		contentPart, _ = ojson.SetBytes(contentPart, "filename", codexFileNameFromMIME(mimeType))
		return contentPart, true
	}
	fileInfo := "File: " + fileURI
	if mimeType != "" {
		fileInfo += " (Type: " + mimeType + ")"
	}
	contentPart := []byte(`{"type":"input_text","text":""}`)
	contentPart, _ = ojson.SetBytes(contentPart, "text", fileInfo)
	return contentPart, true
}

func codexInputAudioFormatFromMIME(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "audio/wav", "audio/wave", "audio/x-wav":
		return "wav"
	case "audio/flac":
		return "flac"
	case "audio/opus", "audio/ogg":
		return "opus"
	case "audio/pcm", "audio/l16":
		return "pcm16"
	default:
		return "mp3"
	}
}

func codexFileNameFromMIME(mimeType string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "application/pdf":
		return "document.pdf"
	case "text/plain":
		return "document.txt"
	case "text/csv":
		return "document.csv"
	case "application/json":
		return "document.json"
	case "application/xml", "text/xml":
		return "document.xml"
	default:
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(mimeType)), "video/") {
			return "video"
		}
		return "document"
	}
}

// shortenNameIfNeeded applies the simple shortening rule for a single name.
func shortenNameIfNeeded(name string) string {
	const limit = 64
	if len(name) <= limit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		idx := strings.LastIndex(name, "__")
		if idx > 0 {
			cand := "mcp__" + name[idx+2:]
			if len(cand) > limit {
				return cand[:limit]
			}
			return cand
		}
	}
	return name[:limit]
}

// buildShortNameMap ensures uniqueness of shortened names within a request.
func buildShortNameMap(names []string) map[string]string {
	const limit = 64
	used := map[string]struct{}{}
	m := map[string]string{}

	baseCandidate := func(n string) string {
		if len(n) <= limit {
			return n
		}
		if strings.HasPrefix(n, "mcp__") {
			idx := strings.LastIndex(n, "__")
			if idx > 0 {
				cand := "mcp__" + n[idx+2:]
				if len(cand) > limit {
					cand = cand[:limit]
				}
				return cand
			}
		}
		return n[:limit]
	}

	makeUnique := func(cand string) string {
		if _, ok := used[cand]; !ok {
			return cand
		}
		base := cand
		for i := 1; ; i++ {
			suffix := "_" + strconv.Itoa(i)
			allowed := limit - len(suffix)
			if allowed < 0 {
				allowed = 0
			}
			tmp := base
			if len(tmp) > allowed {
				tmp = tmp[:allowed]
			}
			tmp = tmp + suffix
			if _, ok := used[tmp]; !ok {
				return tmp
			}
		}
	}

	for _, n := range names {
		cand := baseCandidate(n)
		uniq := makeUnique(cand)
		used[uniq] = struct{}{}
		m[n] = uniq
	}
	return m
}
