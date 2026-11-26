// Package gemini provides request translation functionality for Gemini to Claude Code API compatibility.
// It handles parsing and transforming Gemini API requests into Claude Code API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Gemini API format and Claude Code API's expected format.
package gemini

import (
	"fmt"
	"slices"
	"strings"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"
	"ccLoad/internal/protocol/cliproxy/registry"
	"ccLoad/internal/protocol/cliproxy/thinking"
	"ccLoad/internal/protocol/cliproxy/util"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertGeminiRequestToClaude parses and transforms a Gemini API request into Claude Code API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the Claude Code API.
// The function performs comprehensive transformation including:
// 1. Model name mapping and generation configuration extraction
// 2. System instruction conversion to Claude Code format
// 3. Message content conversion with proper role mapping
// 4. Tool call and tool result handling with FIFO queue for ID matching
// 5. Image and file data conversion to Claude Code base64 format
// 6. Tool declaration and tool choice configuration mapping
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the Gemini API
//   - stream: A boolean indicating if the request is for a streaming response
//
// Returns:
//   - []byte: The transformed request data in Claude Code API format
func ConvertGeminiRequestToClaude(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	return convertGeminiRequestToClaude(modelName, inputRawJSON, stream)

}

// convertGeminiRequestToClaude also reports a user turn that was left empty
// because its only attachment has no Claude equivalent.
func convertGeminiRequestToClaude(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	rawJSON := inputRawJSON
	var drops translatorcommon.UserTurnDrops
	var toolOutputErr error

	userID := translatorcommon.DeriveClaudeUserID(rawJSON)

	// Base Claude message payload
	out := []byte(`{"model":"","max_tokens":32000,"messages":[],"metadata":{}}`)
	out, _ = sjson.SetBytes(out, "metadata.user_id", userID)

	root := gjson.ParseBytes(rawJSON)
	messageAccumulator := translatorcommon.NewClaudeMessageAccumulator(int(root.Get("contents.#").Int()) + 1)

	getGeminiToolID := func(value gjson.Result) string {
		if toolID := strings.TrimSpace(value.Get("id").String()); toolID != "" {
			return toolID
		}
		return strings.TrimSpace(value.Get("call_id").String())
	}

	removePendingToolID := func(ids []string, toolID string) []string {
		if toolID == "" {
			return ids
		}
		for idx, pendingID := range ids {
			if pendingID == toolID {
				return append(ids[:idx], ids[idx+1:]...)
			}
		}
		return ids
	}

	// FIFO queue to store tool call IDs for matching with tool results
	// Gemini uses sequential pairing across possibly multiple in-flight
	// functionCalls, so we keep a FIFO queue of generated tool IDs and
	// consume them in order when functionResponses arrive.
	var pendingToolIDs []string
	toolCallCounter := 0

	// Model mapping to specify which Claude Code model to use
	out, _ = sjson.SetBytes(out, "model", modelName)
	if serviceTier := root.Get("service_tier"); serviceTier.Exists() && serviceTier.Type == gjson.String {
		out, _ = sjson.SetBytes(out, "service_tier", serviceTier.String())
	}

	genConfig := translatorcommon.GeminiConfigObject(root, "generationConfig", "generation_config")
	if maxTokens, ok := translatorcommon.GeminiConfigInt(genConfig, 1, "maxOutputTokens", "max_output_tokens"); ok {
		out, _ = sjson.SetBytes(out, "max_tokens", maxTokens)
	}
	// Temperature is omitted for Claude requests.
	if topP := translatorcommon.GeminiConfigNumber(genConfig, "topP", "top_p"); topP.Exists() {
		out, _ = sjson.SetBytes(out, "top_p", topP.Float())
	}
	if topK, ok := translatorcommon.GeminiConfigInt(genConfig, 0, "topK", "top_k"); ok {
		out, _ = sjson.SetBytes(out, "top_k", topK)
	}
	if stops := translatorcommon.GeminiConfigStrings(genConfig, "stopSequences", "stop_sequences"); len(stops) > 0 {
		out, _ = sjson.SetBytes(out, "stop_sequences", stops)
	}

	level, value, hasBudget := translatorcommon.GeminiThinkingConfig(root)
	if level != "" {
		model := registry.LookupModelInfo(util.ResolveClaudeThinkingModelAlias(strings.TrimSpace(modelName)), "claude")
		switch {
		case level == "none":
			out, _ = sjson.SetBytes(out, "thinking.type", "disabled")
		case model != nil && model.Thinking == nil:
			// The model does not support thinking.
		case model != nil && len(model.Thinking.Levels) == 0:
			if budget, ok := thinking.ConvertLevelToBudget(level); ok && budget > 0 {
				minBudget := model.Thinking.Min
				maxTokens := gjson.GetBytes(out, "max_tokens").Int()
				if maxTokens <= int64(minBudget) {
					return nil, fmt.Errorf("max output tokens must exceed %d for thinking level %q on %s", minBudget, level, modelName)
				}
				budget = max(budget, minBudget)
				if model.Thinking.Max > 0 {
					budget = min(budget, model.Thinking.Max)
				}
				budget = int(min(int64(budget), maxTokens-1))
				out, _ = sjson.SetBytes(out, "thinking.type", "enabled")
				out, _ = sjson.SetBytes(out, "thinking.budget_tokens", budget)
			}
		default:
			var levels []string
			if model != nil {
				levels = model.Thinking.Levels
			}
			out, _ = sjson.SetBytes(out, "thinking.type", "adaptive")
			if level != "auto" || model == nil || !model.Thinking.DynamicAllowed {
				if !thinking.HasLevel(levels, level) {
					supportsMax := model == nil || thinking.HasLevel(levels, "max")
					if mapped, ok := thinking.MapToClaudeEffort(level, supportsMax); ok {
						level = mapped
					}
				}
				out, _ = sjson.SetBytes(out, "output_config.effort", level)
			}
		}
	} else if hasBudget {
		budget := int(value)
		mi := registry.LookupModelInfo(util.ResolveClaudeThinkingModelAlias(strings.TrimSpace(modelName)), "claude")
		supportsAdaptive := mi != nil && mi.Thinking != nil && len(mi.Thinking.Levels) > 0
		switch {
		case budget == 0:
			out, _ = sjson.SetBytes(out, "thinking.type", "disabled")
		case supportsAdaptive:
			if budget == -1 && mi.Thinking.DynamicAllowed {
				out, _ = sjson.SetBytes(out, "thinking.type", "adaptive")
			} else if level, ok := thinking.ConvertBudgetToLevel(budget); ok {
				if level == string(thinking.LevelMax) {
					level = thinking.ClampEffortToLevels(level, mi.Thinking.Levels)
				} else if !thinking.HasLevel(mi.Thinking.Levels, level) {
					supportsMax := thinking.HasLevel(mi.Thinking.Levels, string(thinking.LevelMax))
					if mapped, ok := thinking.MapToClaudeEffort(level, supportsMax); ok {
						level = mapped
					}
				}
				out, _ = sjson.SetBytes(out, "thinking.type", "adaptive")
				out, _ = sjson.SetBytes(out, "output_config.effort", level)
			}
		case budget != -1 || mi == nil:
			out, _ = sjson.SetBytes(out, "thinking.type", "enabled")
			if budget != -1 {
				out, _ = sjson.SetBytes(out, "thinking.budget_tokens", budget)
			}
		}
	}

	// Keep the Gemini system instruction in its established Claude preamble turn.
	if text := translatorcommon.GeminiSystemInstructionText(root, "systemInstruction", "system_instruction"); text != "" {
		systemMessage := []byte(`{"role":"user","content":[{"type":"text","text":""}]}`)
		systemMessage, _ = sjson.SetBytes(systemMessage, "content.0.text", text)
		messageAccumulator.Append(systemMessage)
		messageAccumulator.Flush()
	}
	format, outputInstruction, err := translatorcommon.GeminiResponseFormat(root, false)
	if err != nil {
		return nil, err
	}
	if format != nil {
		responseFormat := gjson.ParseBytes(format)
		if schema, ok := translatorcommon.ClaudeOutputFormatSchema(modelName, responseFormat); ok && supportsNativeClaudeGeminiOutputSchema(schema) {
			out, _ = sjson.SetBytes(out, "output_config.format.type", "json_schema")
			out, _ = sjson.SetRawBytes(out, "output_config.format.schema", []byte(schema.Raw))
		} else {
			outputInstruction = translatorcommon.BuildClaudeStructuredOutputInstruction(responseFormat)
		}
	}
	if outputInstruction != "" {
		out, _ = sjson.SetRawBytes(out, "system", []byte(`[{"type":"text","text":""}]`))
		out, _ = sjson.SetBytes(out, "system.0.text", outputInstruction)
	}

	// Contents conversion to messages with proper role mapping
	if contents := root.Get("contents"); contents.Exists() && contents.IsArray() {
		contents.ForEach(func(_, content gjson.Result) bool {
			parts := content.Get("parts")
			role := translatorcommon.GeminiMessageRole(content.Get("role").String(), parts)
			hasFunctionCall, hasFunctionResponse := false, false
			for _, part := range parts.Array() {
				hasFunctionCall = hasFunctionCall || translatorcommon.GeminiPartFunctionCall(part).Exists()
				hasFunctionResponse = hasFunctionResponse || translatorcommon.GeminiPartFunctionResponse(part).Exists()
			}
			// Some Gemini clients label tool results as model turns; Claude requires user turns.
			if hasFunctionResponse && !hasFunctionCall {
				role = "user"
			}

			contentItems := make([][]byte, 0, 4)
			// Counts what this turn sends; whitespace-only text is forwarded but never sendable.
			sendable := 0
			flushContent := func() {
				if role != "assistant" {
					drops.EndTurn(sendable)
				}
				if len(contentItems) > 0 {
					msg := []byte(`{"role":"","content":[]}`)
					msg, _ = sjson.SetBytes(msg, "role", role)
					msg, _ = sjson.SetRawBytes(msg, "content", translatorcommon.JoinRawArray(contentItems))
					messageAccumulator.Append(msg)
				}
				contentItems = contentItems[:0]
				sendable = 0
			}
			// Split mixed function calls and results without losing their order or IDs.
			switchRole := func(nextRole string) {
				if role != nextRole {
					flushContent()
					role = nextRole
				}
			}
			if parts.Exists() && parts.IsArray() {
				parts.ForEach(func(_, part gjson.Result) bool {
					if translatorcommon.IsGeminiThoughtPart(part) {
						return true
					}

					// Text content conversion
					if text := part.Get("text"); text.Exists() {
						textContent := []byte(`{"type":"text","text":""}`)
						textContent, _ = sjson.SetBytes(textContent, "text", text.String())
						contentItems = append(contentItems, textContent)
						if strings.TrimSpace(text.String()) != "" {
							sendable++
						}
						return true
					}

					// Function call (from model/assistant) conversion to tool use
					if fc := translatorcommon.GeminiPartFunctionCall(part); fc.Exists() && (role == "assistant" || hasFunctionResponse) {
						switchRole("assistant")
						toolUse := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)

						// Reuse gateway-provided IDs when present, otherwise generate one for pairing.
						toolID := getGeminiToolID(fc)
						if toolID == "" {
							toolCallCounter++
							toolID = fmt.Sprintf("toolu_gemini_%016d", toolCallCounter)
						}
						pendingToolIDs = append(pendingToolIDs, toolID)
						toolUse, _ = sjson.SetBytes(toolUse, "id", toolID)

						if name := fc.Get("name"); name.Exists() {
							toolUse, _ = sjson.SetBytes(toolUse, "name", util.SanitizeClaudeFunctionName(name.String()))
						}
						if args := fc.Get("args"); args.Exists() && args.IsObject() {
							toolUse, _ = sjson.SetRawBytes(toolUse, "input", []byte(args.Raw))
						}
						contentItems = append(contentItems, toolUse)
						sendable++
						return true
					}

					// Function response (from user) conversion to tool result
					if fr := translatorcommon.GeminiPartFunctionResponse(part); fr.Exists() {
						switchRole("user")
						toolResult := []byte(`{"type":"tool_result","tool_use_id":"","content":""}`)

						// Attach the oldest queued tool_id to pair the response
						// with its call. If the queue is empty, generate a new id.
						var toolID string
						if customID := getGeminiToolID(fr); customID != "" {
							toolID = customID
							pendingToolIDs = removePendingToolID(pendingToolIDs, toolID)
						} else if len(pendingToolIDs) > 0 {
							toolID = pendingToolIDs[0]
							// Pop the first element from the queue
							pendingToolIDs = pendingToolIDs[1:]
						} else {
							// Fallback: generate new ID if no pending tool_use found
							toolCallCounter++
							toolID = fmt.Sprintf("toolu_gemini_%016d", toolCallCounter)
						}
						toolResult, _ = sjson.SetBytes(toolResult, "tool_use_id", toolID)

						var errTool error
						toolResult, errTool = setClaudeToolResultContentFromGemini(toolResult, fr, &drops)
						if errTool != nil && toolOutputErr == nil {
							toolOutputErr = errTool
						}
						contentItems = append(contentItems, toolResult)
						sendable++
						return true
					}

					// Inline data conversion to Claude Code content format. Only model content
					// keeps a placeholder for media Claude cannot read; a user attachment is
					// never replaced by text, so it is recorded as dropped instead.
					if inlineData := geminiClaudeInlineData(part); inlineData.Exists() {
						if contentPart, ok := claudeContentPartFromGeminiInlineData(inlineData, role == "assistant"); ok {
							contentItems = append(contentItems, contentPart)
							sendable++
						} else if role != "assistant" {
							drops.Drop("inlineData")
						}
						return true
					}

					// File data conversion to Claude Code content format
					if fileData := geminiClaudeFileData(part); fileData.Exists() {
						if contentPart, ok := claudeContentPartFromGeminiFileData(fileData); ok {
							contentItems = append(contentItems, contentPart)
							sendable++
						} else if role != "assistant" {
							drops.Drop("fileData")
						}
						return true
					}

					return true
				})
			}
			flushContent()

			return true
		})
	}
	messages := messageAccumulator.Messages()
	orderGeminiClaudeToolResults(messages)
	out = translatorcommon.SetRawArrayItems(out, "messages", messages)

	declarations := translatorcommon.GeminiFunctionDeclarations(root)
	mode, allowedNames, err := translatorcommon.GeminiFunctionCallingOptions(root, declarations)
	if err != nil {
		return nil, err
	}
	var tools [][]byte
	for _, declaration := range declarations {
		name := declaration.Get("name")
		if len(allowedNames) > 0 && !slices.Contains(allowedNames, name.String()) {
			continue
		}
		tool := []byte(`{"name":"","description":"","input_schema":{"type":"object","properties":{}}}`)
		if name.Exists() {
			tool, _ = sjson.SetBytes(tool, "name", util.SanitizeClaudeFunctionName(name.String()))
		}
		if description := declaration.Get("description"); description.Exists() {
			tool, _ = sjson.SetBytes(tool, "description", description.String())
		}
		if parameters := translatorcommon.GeminiConfigObject(declaration, "parameters", "parametersJsonSchema", "parameters_json_schema"); parameters.Exists() {
			tool, _ = sjson.SetRawBytes(tool, "input_schema", normalizeClaudeToolSchema(parameters))
		}
		tools = append(tools, tool)
	}
	if len(tools) > 0 {
		out, _ = sjson.SetRawBytes(out, "tools", translatorcommon.JoinRawArray(tools))
	}

	switch mode {
	case "AUTO":
		out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"auto"}`))
	case "NONE":
		out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"none"}`))
	case "ANY":
		if len(allowedNames) == 1 {
			choice := []byte(`{"type":"tool","name":""}`)
			choice, _ = sjson.SetBytes(choice, "name", util.SanitizeClaudeFunctionName(allowedNames[0]))
			out, _ = sjson.SetRawBytes(out, "tool_choice", choice)
		} else {
			out, _ = sjson.SetRawBytes(out, "tool_choice", []byte(`{"type":"any"}`))
		}
	}

	// Stream setting configuration
	out, _ = sjson.SetBytes(out, "stream", stream)

	if err := drops.Err(); err != nil {
		return out, err
	}
	return out, toolOutputErr
}

// supportsNativeClaudeGeminiOutputSchema recognizes closed schemas that Claude
// can enforce directly. Other schemas are sent intact in the system instruction.
func supportsNativeClaudeGeminiOutputSchema(schema gjson.Result) bool {
	if !schema.IsObject() {
		return false
	}
	kind := schema.Get("type").Str
	switch kind {
	case "object":
		if schema.Get("additionalProperties").Type != gjson.False {
			return false
		}
	case "array", "string", "integer", "number", "boolean", "null":
	case "":
		if !schema.Get("anyOf").IsArray() {
			return false
		}
	default:
		return false
	}

	supported := true
	schema.ForEach(func(key, value gjson.Result) bool {
		switch key.Str {
		case "title", "description":
			supported = value.Type == gjson.String
		case "type", "default":
			supported = kind != ""
		case "additionalProperties":
			supported = kind == "object"
		case "required":
			supported = kind == "object" && value.IsArray() && !slices.ContainsFunc(value.Array(), func(item gjson.Result) bool {
				return item.Type != gjson.String
			})
		case "properties":
			supported = kind == "object" && value.IsObject() && allNativeClaudeGeminiOutputSchemasSupported(value)
		case "items":
			supported = kind == "array" && supportsNativeClaudeGeminiOutputSchema(value)
		case "anyOf":
			supported = kind == "" && value.IsArray() && value.Get("#").Int() != 0 && allNativeClaudeGeminiOutputSchemasSupported(value)
		case "const":
			supported = kind != "" && !value.IsObject() && !value.IsArray()
		case "enum":
			supported = kind != "" && value.IsArray() && value.Get("#").Int() != 0 && !slices.ContainsFunc(value.Array(), func(item gjson.Result) bool {
				return item.IsObject() || item.IsArray()
			})
		case "minItems":
			supported = kind == "array" && value.Type == gjson.Number && (value.Num == 0 || value.Num == 1)
		case "format":
			supported = kind == "string" && slices.Contains([]string{"date-time", "time", "date", "duration", "email", "hostname", "uri", "ipv4", "ipv6", "uuid"}, value.Str)
		default:
			supported = false
		}
		return supported
	})
	return supported
}

func allNativeClaudeGeminiOutputSchemasSupported(schemas gjson.Result) bool {
	supported := true
	schemas.ForEach(func(_, schema gjson.Result) bool {
		supported = supportsNativeClaudeGeminiOutputSchema(schema)
		return supported
	})
	return supported
}

// orderGeminiClaudeToolResults keeps tool results before other user content,
// including when adjacent Gemini user turns were merged into one Claude turn.
func orderGeminiClaudeToolResults(messages [][]byte) {
	for i, message := range messages {
		root := gjson.ParseBytes(message)
		if root.Get("role").String() != "user" {
			continue
		}
		var results, other [][]byte
		hasOther, needsReorder := false, false
		root.Get("content").ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "tool_result" {
				results = append(results, []byte(part.Raw))
				needsReorder = needsReorder || hasOther
			} else {
				other = append(other, []byte(part.Raw))
				hasOther = true
			}
			return true
		})
		if needsReorder {
			messages[i], _ = sjson.SetRawBytes(message, "content", translatorcommon.JoinRawArray(append(results, other...)))
		}
	}
}

func normalizeClaudeToolSchema(parameters gjson.Result) []byte {
	cleaned := []byte(util.NormalizeGeminiToolSchema(parameters.Raw))
	if parameters.Get("additionalProperties").Type != gjson.False {
		cleaned, _ = sjson.SetBytes(cleaned, "additionalProperties", false)
	}
	const schema = "http://json-schema.org/draft-07/schema#"
	currentSchema := parameters.Get("$schema")
	if currentSchema.Type != gjson.String || currentSchema.String() != schema {
		cleaned, _ = sjson.SetBytes(cleaned, "$schema", schema)
	}
	return cleaned
}

func geminiClaudeInlineData(part gjson.Result) gjson.Result {
	inlineData := part.Get("inlineData")
	if inlineData.Exists() {
		return inlineData
	}
	return part.Get("inline_data")
}

func geminiClaudeFileData(part gjson.Result) gjson.Result {
	fileData := part.Get("fileData")
	if fileData.Exists() {
		return fileData
	}
	return part.Get("file_data")
}

// claudeContentPartFromGeminiInlineData maps inline bytes onto a Claude block.
// Media that Claude cannot read becomes placeholder text only when
// keepPlaceholder is set; otherwise it reports false so the caller can refuse it.
func claudeContentPartFromGeminiInlineData(inlineData gjson.Result, keepPlaceholder bool) ([]byte, bool) {
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
		imageContent := []byte(`{"type":"image","source":{"type":"base64","media_type":"","data":""}}`)
		imageContent, _ = sjson.SetBytes(imageContent, "source.media_type", mimeType)
		imageContent, _ = sjson.SetBytes(imageContent, "source.data", data)
		return imageContent, true
	case strings.HasPrefix(lowerMimeType, "application/"), strings.HasPrefix(lowerMimeType, "text/"):
		documentContent := []byte(`{"type":"document","source":{"type":"base64","media_type":"","data":""}}`)
		documentContent, _ = sjson.SetBytes(documentContent, "source.media_type", mimeType)
		documentContent, _ = sjson.SetBytes(documentContent, "source.data", data)
		return documentContent, true
	default:
		if !keepPlaceholder {
			return nil, false
		}
		return claudeTextContentPart(fmt.Sprintf("Media content: inline data (Type: %s)", mimeType)), true
	}
}

func claudeContentPartFromGeminiFileData(fileData gjson.Result) ([]byte, bool) {
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
	switch {
	case strings.HasPrefix(lowerMimeType, "image/"):
		imageContent := []byte(`{"type":"image","source":{"type":"url","url":""}}`)
		imageContent, _ = sjson.SetBytes(imageContent, "source.url", fileURI)
		return imageContent, true
	case strings.HasPrefix(lowerMimeType, "application/"), strings.HasPrefix(lowerMimeType, "text/"):
		documentContent := []byte(`{"type":"document","source":{"type":"url","url":""}}`)
		documentContent, _ = sjson.SetBytes(documentContent, "source.url", fileURI)
		if mimeType != "" {
			documentContent, _ = sjson.SetBytes(documentContent, "source.media_type", mimeType)
		}
		return documentContent, true
	default:
		fileInfo := "File: " + fileURI
		if mimeType != "" {
			fileInfo += " (Type: " + mimeType + ")"
		}
		return claudeTextContentPart(fileInfo), true
	}
}

func claudeTextContentPart(text string) []byte {
	textContent := []byte(`{"type":"text","text":""}`)
	textContent, _ = sjson.SetBytes(textContent, "text", text)
	return textContent
}

/**
 * @brief 判断 Gemini functionResponse.response 是否没有可转发内容
 * @param v response 字段
 * @return 字段缺失、空对象或空白原文时返回 true
 */
func isEmptyGeminiClaudeResponseObject(v gjson.Result) bool {
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
func geminiClaudeResponseHasOnlyResultKey(responseField gjson.Result) bool {
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
 * @brief 把 Gemini functionResponse 的结果与 parts 媒体写入 Claude tool_result.content
 * @param toolResult 待写入的 tool_result 块
 * @param fr Gemini functionResponse 节点
 * @param drops 当前用户轮次的丢弃记录；Claude 无法表示的嵌套媒体记入其中
 * @return 写入 content 后的 tool_result
 * @return 嵌套媒体全部无法表示且没有任何文本结果时返回 *translatorcommon.UnsupportedPartError
 * @details 有嵌套文本或媒体时 content 为块数组：首块是 response 文本（只有 result 键时取其值，
 *          否则取整个 response 原文），随后按序追加嵌套块；没有嵌套块时保持字符串 content。
 */
func setClaudeToolResultContentFromGemini(toolResult []byte, fr gjson.Result, drops *translatorcommon.UserTurnDrops) ([]byte, error) {
	resultField := fr.Get("response.result")
	responseField := fr.Get("response")
	resultOnly := resultField.Exists() && geminiClaudeResponseHasOnlyResultKey(responseField)
	hasBaseText := false
	baseText := ""
	if resultOnly {
		baseText = resultField.String()
		hasBaseText = strings.TrimSpace(baseText) != ""
	} else if !isEmptyGeminiClaudeResponseObject(responseField) {
		baseText = responseField.Raw
		hasBaseText = true
	}

	var nestedBlocks [][]byte
	droppedType := ""
	fr.Get("parts").ForEach(func(_, nested gjson.Result) bool {
		if text := nested.Get("text"); text.Exists() {
			if strings.TrimSpace(text.String()) != "" {
				nestedBlocks = append(nestedBlocks, claudeTextContentPart(text.String()))
			}
			return true
		}
		partType := ""
		var contentPart []byte
		ok := false
		if inlineData := geminiClaudeInlineData(nested); inlineData.Exists() {
			partType = "inlineData"
			contentPart, ok = claudeContentPartFromGeminiInlineData(inlineData, false)
		} else if fileData := geminiClaudeFileData(nested); fileData.Exists() {
			partType = "fileData"
			contentPart, ok = claudeContentPartFromGeminiFileData(fileData)
		} else {
			return true
		}
		if ok {
			nestedBlocks = append(nestedBlocks, contentPart)
			return true
		}
		if droppedType == "" {
			droppedType = partType
		}
		drops.Drop(partType)
		return true
	})

	if len(nestedBlocks) > 0 {
		blocks := make([][]byte, 0, len(nestedBlocks)+1)
		if hasBaseText {
			blocks = append(blocks, claudeTextContentPart(baseText))
		}
		blocks = append(blocks, nestedBlocks...)
		toolResult, _ = sjson.SetRawBytes(toolResult, "content", translatorcommon.JoinRawArray(blocks))
		return toolResult, nil
	}

	if resultOnly {
		toolResult, _ = sjson.SetBytes(toolResult, "content", resultField.String())
	} else if responseField.Exists() {
		toolResult, _ = sjson.SetBytes(toolResult, "content", responseField.Raw)
	}
	if droppedType != "" && !hasBaseText {
		return toolResult, &translatorcommon.UnsupportedPartError{Type: droppedType}
	}
	return toolResult, nil
}
