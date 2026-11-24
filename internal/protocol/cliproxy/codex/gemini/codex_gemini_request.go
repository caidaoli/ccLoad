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

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
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

	root := gjson.ParseBytes(rawJSON)
	inputItems := translatorcommon.NewRawArrayItems(root.Get("contents.#").Int())

	// Pre-compute tool name shortening map from declared functionDeclarations
	shortMap := map[string]string{}
	if tools := root.Get("tools"); tools.IsArray() {
		var names []string
		tarr := tools.Array()
		for i := 0; i < len(tarr); i++ {
			fns := tarr[i].Get("functionDeclarations")
			if !fns.IsArray() {
				continue
			}
			for _, fn := range fns.Array() {
				if v := fn.Get("name"); v.Exists() {
					names = append(names, v.String())
				}
			}
		}
		if len(names) > 0 {
			shortMap = buildShortNameMap(names)
		}
	}

	// helper for generating paired call IDs in the form: call_gemini_<seq>
	// Gemini uses sequential pairing across possibly multiple in-flight
	// functionCalls, so we keep a FIFO queue of generated call IDs and
	// consume them in order when functionResponses arrive.
	var pendingCallIDs []string
	callCounter := 0
	emittedThoughtSignatures := map[string]bool{}
	signatureTargetModel := translatorcommon.ResolveCodexReplayTargetModel(modelName, inputRawJSON)

	getGeminiCallID := func(value gjson.Result) string {
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
	out, _ = sjson.SetBytes(out, "model", modelName)
	if serviceTier := normalizeGeminiCodexServiceTier(root.Get("service_tier")); serviceTier != "" {
		out, _ = sjson.SetBytes(out, "service_tier", serviceTier)
	}

	// System instruction maps to the native Responses instructions field.
	sysParts := root.Get("system_instruction.parts")
	if !sysParts.Exists() {
		sysParts = root.Get("systemInstruction.parts")
	}
	if sysParts.IsArray() {
		var instructions strings.Builder
		arr := sysParts.Array()
		for i := 0; i < len(arr); i++ {
			p := arr[i]
			if translatorcommon.IsGeminiThoughtPart(p) {
				continue
			}
			if t := p.Get("text"); t.Exists() {
				if instructions.Len() > 0 {
					instructions.WriteByte('\n')
				}
				instructions.WriteString(t.String())
			}
		}
		if instructions.Len() > 0 {
			out, _ = sjson.SetBytes(out, "instructions", instructions.String())
		}
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
			msg, _ = sjson.SetRawBytes(msg, "content", translatorcommon.JoinRawArray(buildCodexGeminiToolAudioRelayItems(pendingToolAudioRelay)))
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
							reasoningItem, _ = sjson.SetBytes(reasoningItem, "encrypted_content", normalizedSignature)
							summaryParts := pendingThoughtSummaryParts
							pendingThoughtSummaryParts = nil
							if translatorcommon.IsGeminiThoughtPart(p) {
								if thoughtText := p.Get("text").String(); thoughtText != "" {
									summaryPart := []byte(`{"type":"summary_text","text":""}`)
									summaryPart, _ = sjson.SetBytes(summaryPart, "text", thoughtText)
									summaryParts = append(summaryParts, summaryPart)
								}
							}
							if len(summaryParts) > 0 {
								reasoningItem, _ = sjson.SetRawBytes(reasoningItem, "summary", translatorcommon.JoinRawArray(summaryParts))
							}
							inputItems = append(inputItems, reasoningItem)
							emittedThoughtSignatures[normalizedSignature] = true
						} else {
							pendingThoughtSummaryParts = nil
						}
					} else if translatorcommon.IsGeminiThoughtPart(p) {
						if thoughtText := p.Get("text").String(); thoughtText != "" {
							summaryPart := []byte(`{"type":"summary_text","text":""}`)
							summaryPart, _ = sjson.SetBytes(summaryPart, "text", thoughtText)
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
					part, _ = sjson.SetBytes(part, "type", partType)
					part, _ = sjson.SetBytes(part, "text", t.String())
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
					fn := []byte(`{"type":"function_call"}`)
					if name := fc.Get("name"); name.Exists() {
						n := name.String()
						if short, ok := shortMap[n]; ok {
							n = short
						} else {
							n = shortenNameIfNeeded(n)
						}
						fn, _ = sjson.SetBytes(fn, "name", n)
					}
					if args := fc.Get("args"); args.Exists() {
						fn, _ = sjson.SetBytes(fn, "arguments", args.Raw)
					}
					// Reuse gateway-provided IDs when present, otherwise generate one for pairing.
					id := getGeminiCallID(fc)
					if id == "" {
						callCounter++
						id = fmt.Sprintf("call_gemini_%016d", callCounter)
					}
					fn, _ = sjson.SetBytes(fn, "call_id", id)
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
					fno, _ = sjson.SetBytes(fno, "call_id", id)
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

	// Tools mapping: Gemini functionDeclarations -> Codex tools
	tools := root.Get("tools")
	if tools.IsArray() {
		var toolItems [][]byte
		out, _ = sjson.SetBytes(out, "tool_choice", "auto")
		tarr := tools.Array()
		for i := 0; i < len(tarr); i++ {
			td := tarr[i]
			fns := td.Get("functionDeclarations")
			if !fns.IsArray() {
				continue
			}
			farr := fns.Array()
			for j := 0; j < len(farr); j++ {
				fn := farr[j]
				tool := []byte(`{}`)
				tool, _ = sjson.SetBytes(tool, "type", "function")
				if v := fn.Get("name"); v.Exists() {
					name := v.String()
					if short, ok := shortMap[name]; ok {
						name = short
					} else {
						name = shortenNameIfNeeded(name)
					}
					tool, _ = sjson.SetBytes(tool, "name", name)
				}
				if v := fn.Get("description"); v.Exists() {
					tool, _ = sjson.SetBytes(tool, "description", v.String())
				}
				parameters := fn.Get("parameters")
				if !parameters.Exists() {
					parameters = fn.Get("parametersJsonSchema")
				}
				if parameters.Exists() {
					tool, _ = sjson.SetRawBytes(tool, "parameters", cleanGeminiCodexToolParameters(parameters))
				}
				tool, _ = sjson.SetBytes(tool, "strict", false)
				toolItems = append(toolItems, tool)
			}
		}
		out, _ = sjson.SetRawBytes(out, "tools", translatorcommon.JoinRawArray(toolItems))
	}

	// Fixed flags aligning with Codex expectations
	out, _ = sjson.SetBytes(out, "parallel_tool_calls", true)
	out = setCodexToolChoiceFromGeminiToolConfig(out, root.Get("toolConfig.functionCallingConfig"))
	genConfig := root.Get("generationConfig")
	if !genConfig.Exists() {
		genConfig = root.Get("generation_config")
	}
	for _, v := range []gjson.Result{
		genConfig.Get("maxOutputTokens"),
		genConfig.Get("max_output_tokens"),
		root.Get("maxOutputTokens"),
		root.Get("max_output_tokens"),
		root.Get("max_tokens"),
	} {
		if v.Type == gjson.Number {
			out, _ = sjson.SetBytes(out, "max_output_tokens", v.Int())
			break
		}
	}

	// Convert Gemini thinkingConfig to Codex reasoning.effort.
	// Note: Google official Python SDK sends snake_case fields (thinking_level/thinking_budget).
	effortSet := false
	if genConfig.Exists() {
		thinkingLevel := genConfig.Get("thinkingLevel")
		if !thinkingLevel.Exists() {
			thinkingLevel = genConfig.Get("thinking_level")
		}
		if thinkingLevel.Exists() {
			effort := strings.ToLower(strings.TrimSpace(thinkingLevel.String()))
			if effort != "" {
				out, _ = sjson.SetBytes(out, "reasoning.effort", effort)
				effortSet = true
			}
		} else if thinkingConfig := genConfig.Get("thinkingConfig"); thinkingConfig.Exists() && thinkingConfig.IsObject() {
			thinkingLevel := thinkingConfig.Get("thinkingLevel")
			if !thinkingLevel.Exists() {
				thinkingLevel = thinkingConfig.Get("thinking_level")
			}
			if thinkingLevel.Exists() {
				effort := strings.ToLower(strings.TrimSpace(thinkingLevel.String()))
				if effort != "" {
					out, _ = sjson.SetBytes(out, "reasoning.effort", effort)
					effortSet = true
				}
			} else {
				thinkingBudget := thinkingConfig.Get("thinkingBudget")
				if !thinkingBudget.Exists() {
					thinkingBudget = thinkingConfig.Get("thinking_budget")
				}
				if thinkingBudget.Exists() {
					if effort, ok := thinking.ConvertBudgetToModelLevel(int(thinkingBudget.Int()), modelName, "codex"); ok {
						out, _ = sjson.SetBytes(out, "reasoning.effort", effort)
						effortSet = true
					}
				}
			}
		}
	}
	if !effortSet {
		// No thinking config, set default effort
		out, _ = sjson.SetBytes(out, "reasoning.effort", "medium")
	}
	// OpenAI documents reasoning summaries as explicit opt-in output. ccLoad
	// excludes upstream's runtime summary applier, so the converter itself
	// keeps requesting summaries to preserve visible reasoning downstream.
	out, _ = sjson.SetBytes(out, "reasoning.summary", "auto")
	out, _ = sjson.SetBytes(out, "stream", stream)
	out, _ = sjson.SetBytes(out, "store", false)
	out, _ = sjson.SetBytes(out, "include", []string{"reasoning.encrypted_content"})

	if input := gjson.GetBytes(out, "input"); input.IsArray() && len(input.Array()) == 0 {
		out, _ = sjson.DeleteBytes(out, "input")
	}
	return out, nil
}

// geminiPartThoughtSignature extracts a part's thought signature under either
// the camelCase or snake_case field spelling.
func geminiPartThoughtSignature(part gjson.Result) string {
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

func setCodexToolChoiceFromGeminiToolConfig(out []byte, functionCallingConfig gjson.Result) []byte {
	if !functionCallingConfig.Exists() {
		return out
	}
	mode := functionCallingConfig.Get("mode").String()
	switch mode {
	case "NONE":
		out, _ = sjson.SetBytes(out, "tool_choice", "none")
	case "AUTO":
		current := gjson.GetBytes(out, "tool_choice")
		if current.Type != gjson.String || current.String() != "auto" {
			out, _ = sjson.SetBytes(out, "tool_choice", "auto")
		}
	case "ANY":
		allowedNames := functionCallingConfig.Get("allowedFunctionNames")
		allowedNameItems := allowedNames.Array()
		if allowedNames.IsArray() && len(allowedNameItems) == 1 {
			choice := []byte(`{"type":"function","name":""}`)
			choice, _ = sjson.SetBytes(choice, "name", shortenNameIfNeeded(allowedNameItems[0].String()))
			out, _ = sjson.SetRawBytes(out, "tool_choice", choice)
		} else {
			out, _ = sjson.SetBytes(out, "tool_choice", "required")
		}
	}
	return out
}

func cleanGeminiCodexToolParameters(parameters gjson.Result) []byte {
	cleaned := []byte(util.NormalizeGeminiToolSchema(parameters.Raw))
	if parameters.Get("$schema").Exists() {
		cleaned, _ = sjson.DeleteBytes(cleaned, "$schema")
	}
	if additionalProperties := parameters.Get("additionalProperties"); additionalProperties.Type != gjson.False {
		cleaned, _ = sjson.SetBytes(cleaned, "additionalProperties", false)
	}
	return cleaned
}

func codexMessageWithPart(role string, part []byte) []byte {
	msg := []byte(`{"type":"message","role":"","content":[]}`)
	msg, _ = sjson.SetBytes(msg, "role", role)
	msg, _ = sjson.SetRawBytes(msg, "content", translatorcommon.JoinRawArray([][]byte{part}))
	return msg
}

func normalizeGeminiCodexServiceTier(serviceTier gjson.Result) string {
	if !serviceTier.Exists() || serviceTier.Type != gjson.String {
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
	part, _ = sjson.SetBytes(part, "text", text)
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
func isEmptyGeminiCodexResponseObject(v gjson.Result) bool {
	if !v.Exists() {
		return true
	}
	if v.IsObject() {
		empty := true
		v.ForEach(func(_, _ gjson.Result) bool {
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
func geminiCodexResponseHasOnlyResultKey(responseField gjson.Result) bool {
	if !responseField.IsObject() {
		return false
	}
	hasResult := false
	onlyResult := true
	responseField.ForEach(func(key, _ gjson.Result) bool {
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
func codexFunctionCallOutputFromGemini(fr gjson.Result) ([]byte, [][]byte) {
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
	fr.Get("parts").ForEach(func(_, nested gjson.Result) bool {
		if t := nested.Get("text"); t.Exists() {
			if strings.TrimSpace(t.String()) != "" {
				nestedItems = append(nestedItems, codexInputTextPart(t.String()))
			}
			return true
		}
		if contentPart, ok := codexContentPartFromGeminiInlineData(nested); ok {
			if gjson.GetBytes(contentPart, "type").String() == "input_audio" {
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
		fno, _ = sjson.SetRawBytes(fno, "output", translatorcommon.JoinRawArray(outputItems))
		return fno, relayedAudioItems
	}

	switch {
	case resultOnly:
		fno, _ = sjson.SetBytes(fno, "output", res.String())
	case !isEmptyGeminiCodexResponseObject(resp):
		fno, _ = sjson.SetBytes(fno, "output", resp.Raw)
	case len(relayedAudioItems) > 0:
		fno, _ = sjson.SetBytes(fno, "output", "[Tool returned audio content; the audio follows in the next user message.]")
	case resp.Exists():
		fno, _ = sjson.SetBytes(fno, "output", resp.Raw)
	}
	return fno, relayedAudioItems
}

func codexContentPartFromGeminiInlineData(part gjson.Result) ([]byte, bool) {
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
		contentPart, _ = sjson.SetBytes(contentPart, "image_url", fmt.Sprintf("data:%s;base64,%s", mimeType, data))
		return contentPart, true
	case strings.HasPrefix(lowerMimeType, "audio/"):
		contentPart := []byte(`{"type":"input_audio","input_audio":{"data":"","format":""}}`)
		contentPart, _ = sjson.SetBytes(contentPart, "input_audio.data", data)
		contentPart, _ = sjson.SetBytes(contentPart, "input_audio.format", codexInputAudioFormatFromMIME(mimeType))
		return contentPart, true
	default:
		fileData := data
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(fileData)), "data:") {
			fileData = fmt.Sprintf("data:%s;base64,%s", mimeType, data)
		}
		contentPart := []byte(`{"type":"input_file","file_data":"","filename":""}`)
		contentPart, _ = sjson.SetBytes(contentPart, "file_data", fileData)
		contentPart, _ = sjson.SetBytes(contentPart, "filename", codexFileNameFromMIME(mimeType))
		return contentPart, true
	}
}

func codexContentPartFromGeminiFileData(part gjson.Result) ([]byte, bool) {
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
		contentPart, _ = sjson.SetBytes(contentPart, "image_url", fileURI)
		return contentPart, true
	}
	if strings.HasPrefix(lowerMimeType, "video/") || strings.HasPrefix(lowerMimeType, "application/") || strings.HasPrefix(lowerMimeType, "text/") {
		contentPart := []byte(`{"type":"input_file","file_url":"","filename":""}`)
		contentPart, _ = sjson.SetBytes(contentPart, "file_url", fileURI)
		contentPart, _ = sjson.SetBytes(contentPart, "filename", codexFileNameFromMIME(mimeType))
		return contentPart, true
	}
	fileInfo := "File: " + fileURI
	if mimeType != "" {
		fileInfo += " (Type: " + mimeType + ")"
	}
	contentPart := []byte(`{"type":"input_text","text":""}`)
	contentPart, _ = sjson.SetBytes(contentPart, "text", fileInfo)
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
