// Package gemini provides request translation functionality for Gemini to Claude Code API compatibility.
// It handles parsing and transforming Gemini API requests into Claude Code API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Gemini API format and Claude Code API's expected format.
package gemini

import (
	"fmt"
	"math"
	"slices"
	"strings"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"
	"ccLoad/internal/protocol/cliproxy/registry"
	"ccLoad/internal/protocol/cliproxy/thinking"
	"ccLoad/internal/protocol/cliproxy/util"

	"github.com/MoeForge/moego/ojson"
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
	outDoc := ojson.NewDoc([]byte(`{"model":"","max_tokens":32000,"messages":[],"metadata":{}}`))
	_ = outDoc.Set("metadata.user_id", userID)

	root := util.ParseGJSONBytesNoCopy(rawJSON)
	messageAccumulator := translatorcommon.NewClaudeMessageAccumulator(int(root.Get("contents.#").Int()) + 1)

	getGeminiToolID := func(value ojson.Result) string {
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
	_ = outDoc.Set("model", modelName)
	if serviceTier := root.Get("service_tier"); serviceTier.Exists() && serviceTier.Type == ojson.String {
		_ = outDoc.Set("service_tier", serviceTier.String())
	}

	// Generation config extraction from Gemini format
	if genConfig := firstGeminiObject(root, "generationConfig", "generation_config"); genConfig.Exists() {
		// Max output tokens configuration
		if maxTokens, ok := firstGeminiPositiveInt(genConfig, "maxOutputTokens", "max_output_tokens"); ok {
			_ = outDoc.Set("max_tokens", maxTokens)
		}
		// Temperature is intentionally omitted for translated Claude requests (see #4071/#4383).
		// Top P setting for nucleus sampling.
		if topP := firstGeminiNumber(genConfig, "topP", "top_p"); topP.Exists() {
			_ = outDoc.Set("top_p", topP.Float())
		}
		// Top K setting for sampling.
		if topK, ok := firstGeminiNonNegativeInt(genConfig, "topK", "top_k"); ok {
			_ = outDoc.Set("top_k", topK)
		}
		// Stop sequences configuration for custom termination conditions
		if stopSeqs := firstGeminiArray(genConfig, "stopSequences", "stop_sequences"); stopSeqs.Exists() {
			var stopSequences []string
			stopSeqs.ForEach(func(_, value ojson.Result) bool {
				if value.Type == ojson.String {
					stopSequences = append(stopSequences, value.String())
				}
				return true
			})
			if len(stopSequences) > 0 {
				_ = outDoc.Set("stop_sequences", stopSequences)
			}
		}
	}

	// The excluded runtime thinking validation is applied here against the routed model.
	level, value, hasBudget := translatorcommon.GeminiThinkingConfig(root)
	if level != "" {
		model := registry.LookupModelInfo(util.ResolveClaudeThinkingModelAlias(strings.TrimSpace(modelName)), "claude")
		switch {
		case level == "none":
			_ = outDoc.Set("thinking.type", "disabled")
		case model != nil && model.Thinking == nil:
			// The model does not support thinking.
		case model != nil && len(model.Thinking.Levels) == 0:
			if budget, ok := thinking.ConvertLevelToBudget(level); ok && budget > 0 {
				minBudget := model.Thinking.Min
				maxTokens := outDoc.Get("max_tokens").Int()
				if maxTokens <= int64(minBudget) {
					return nil, fmt.Errorf("max output tokens must exceed %d for thinking level %q on %s", minBudget, level, modelName)
				}
				budget = max(budget, minBudget)
				if model.Thinking.Max > 0 {
					budget = min(budget, model.Thinking.Max)
				}
				budget = int(min(int64(budget), maxTokens-1))
				_ = outDoc.Set("thinking.type", "enabled")
				_ = outDoc.Set("thinking.budget_tokens", budget)
			}
		default:
			var levels []string
			if model != nil {
				levels = model.Thinking.Levels
			}
			_ = outDoc.Set("thinking.type", "adaptive")
			if level != "auto" || model == nil || !model.Thinking.DynamicAllowed {
				if !thinking.HasLevel(levels, level) {
					supportsMax := model == nil || thinking.HasLevel(levels, "max")
					if mapped, ok := thinking.MapToClaudeEffort(level, supportsMax); ok {
						level = mapped
					}
				}
				_ = outDoc.Set("output_config.effort", level)
			}
		}
	} else if hasBudget {
		budget := int(value)
		mi := registry.LookupModelInfo(util.ResolveClaudeThinkingModelAlias(strings.TrimSpace(modelName)), "claude")
		supportsAdaptive := mi != nil && mi.Thinking != nil && len(mi.Thinking.Levels) > 0
		switch {
		case budget == 0:
			_ = outDoc.Set("thinking.type", "disabled")
		case supportsAdaptive:
			if budget == -1 && mi.Thinking.DynamicAllowed {
				_ = outDoc.Set("thinking.type", "adaptive")
			} else if level, ok := thinking.ConvertBudgetToLevel(budget); ok {
				if level == string(thinking.LevelMax) {
					level = thinking.ClampEffortToLevels(level, mi.Thinking.Levels)
				} else if !thinking.HasLevel(mi.Thinking.Levels, level) {
					supportsMax := thinking.HasLevel(mi.Thinking.Levels, string(thinking.LevelMax))
					if mapped, ok := thinking.MapToClaudeEffort(level, supportsMax); ok {
						level = mapped
					}
				}
				_ = outDoc.Set("thinking.type", "adaptive")
				_ = outDoc.Set("output_config.effort", level)
			}
		case budget != -1 || mi == nil:
			_ = outDoc.Set("thinking.type", "enabled")
			if budget != -1 {
				_ = outDoc.Set("thinking.budget_tokens", budget)
			}
		}
	}

	// Keep the Gemini system instruction in its established Claude preamble turn.
	if text := translatorcommon.GeminiSystemInstructionText(root, "systemInstruction", "system_instruction"); text != "" {
		systemMessage := []byte(`{"role":"user","content":[{"type":"text","text":""}]}`)
		systemMessage, _ = ojson.SetBytes(systemMessage, "content.0.text", text)
		messageAccumulator.Append(systemMessage)
		messageAccumulator.Flush()
	}
	format, outputInstruction, err := translatorcommon.GeminiResponseFormat(root, false)
	if err != nil {
		return nil, err
	}
	if format != nil {
		responseFormat := util.ParseGJSONBytesNoCopy(format)
		if schema, ok := translatorcommon.ClaudeOutputFormatSchema(modelName, responseFormat); ok && supportsNativeClaudeGeminiOutputSchema(schema) {
			_ = outDoc.Set("output_config.format.type", "json_schema")
			_ = outDoc.SetRaw("output_config.format.schema", schema.Raw)
		} else {
			outputInstruction = translatorcommon.BuildClaudeStructuredOutputInstruction(responseFormat)
		}
	}
	if outputInstruction != "" {
		_ = outDoc.SetRaw("system", `[{"type":"text","text":""}]`)
		_ = outDoc.Set("system.0.text", outputInstruction)
	}

	// Contents conversion to messages with proper role mapping
	if contents := root.Get("contents"); contents.Exists() && contents.IsArray() {
		contents.ForEach(func(_, content ojson.Result) bool {
			parts := content.Get("parts")
			role := translatorcommon.GeminiMessageRole(content.Get("role").String(), parts)
			// Some Gemini clients label tool results as model turns; Claude requires user turns.
			hasFunctionResponse := translatorcommon.GeminiPartsHaveFunctionResponse(parts)
			if hasFunctionResponse && !translatorcommon.GeminiPartsHaveFunctionCall(parts) {
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
					msgDoc := ojson.NewDoc([]byte(`{"role":"","content":[]}`))
					_ = msgDoc.Set("role", role)
					_ = msgDoc.SetRawBytes("content", translatorcommon.JoinRawArray(contentItems))
					messageAccumulator.Append(msgDoc.Bytes())
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
				parts.ForEach(func(_, part ojson.Result) bool {
					if translatorcommon.IsGeminiThoughtPart(part) {
						return true
					}

					// Text content conversion
					if text := part.Get("text"); text.Exists() {
						textContent := []byte(`{"type":"text","text":""}`)
						textContent, _ = ojson.SetBytes(textContent, "text", text.String())
						contentItems = append(contentItems, textContent)
						if strings.TrimSpace(text.String()) != "" {
							sendable++
						}
						return true
					}

					// Function call (from model/assistant) conversion to tool use
					if fc := translatorcommon.GeminiPartFunctionCall(part); fc.Exists() && (role == "assistant" || hasFunctionResponse) {
						switchRole("assistant")
						toolUseDoc := ojson.NewDoc([]byte(`{"type":"tool_use","id":"","name":"","input":{}}`))

						// Reuse gateway-provided IDs when present, otherwise generate one for pairing.
						toolID := getGeminiToolID(fc)
						if toolID == "" {
							toolCallCounter++
							toolID = fmt.Sprintf("toolu_gemini_%016d", toolCallCounter)
						}
						pendingToolIDs = append(pendingToolIDs, toolID)
						_ = toolUseDoc.Set("id", toolID)

						if name := fc.Get("name"); name.Exists() {
							_ = toolUseDoc.Set("name", util.SanitizeClaudeFunctionName(name.String()))
						}
						if args := fc.Get("args"); args.Exists() && args.IsObject() {
							_ = toolUseDoc.SetRaw("input", args.Raw)
						}
						contentItems = append(contentItems, toolUseDoc.Bytes())
						sendable++
						return true
					}

					// Function response (from user) conversion to tool result
					if fr := translatorcommon.GeminiPartFunctionResponse(part); fr.Exists() {
						switchRole("user")
						toolResultDoc := ojson.NewDoc([]byte(`{"type":"tool_result","tool_use_id":"","content":""}`))

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
						_ = toolResultDoc.Set("tool_use_id", toolID)

						// Extract result content and any nested multimodal parts from functionResponse.
						if errTool := setClaudeToolResultContentFromGemini(toolResultDoc, fr, role == "assistant", &drops); errTool != nil && toolOutputErr == nil {
							toolOutputErr = errTool
						}
						contentItems = append(contentItems, toolResultDoc.Bytes())
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
	translatorcommon.SetRawArrayItemsDoc(outDoc, "messages", messages)

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
		toolDoc := ojson.NewDoc([]byte(`{"name":"","description":"","input_schema":{"type":"object","properties":{}}}`))
		if name.Exists() {
			_ = toolDoc.Set("name", util.SanitizeClaudeFunctionName(name.String()))
		}
		if description := declaration.Get("description"); description.Exists() {
			_ = toolDoc.Set("description", description.String())
		}
		if parameters := translatorcommon.GeminiConfigObject(declaration, "parameters", "parametersJsonSchema", "parameters_json_schema"); parameters.Exists() {
			_ = toolDoc.SetRawBytes("input_schema", normalizeClaudeToolSchema(parameters))
		}
		tools = append(tools, toolDoc.Bytes())
	}
	if len(tools) > 0 {
		_ = outDoc.SetRawBytes("tools", translatorcommon.JoinRawArray(tools))
	}

	switch mode {
	case "AUTO":
		_ = outDoc.SetRaw("tool_choice", `{"type":"auto"}`)
	case "NONE":
		_ = outDoc.SetRaw("tool_choice", `{"type":"none"}`)
	case "ANY":
		if len(allowedNames) == 1 {
			choice := []byte(`{"type":"tool","name":""}`)
			choice, _ = ojson.SetBytes(choice, "name", util.SanitizeClaudeFunctionName(allowedNames[0]))
			_ = outDoc.SetRawBytes("tool_choice", choice)
		} else {
			_ = outDoc.SetRaw("tool_choice", `{"type":"any"}`)
		}
	}

	// Stream setting configuration
	_ = outDoc.Set("stream", stream)

	if err := drops.Err(); err != nil {
		return outDoc.Bytes(), err
	}
	return outDoc.Bytes(), toolOutputErr
}

// supportsNativeClaudeGeminiOutputSchema recognizes closed schemas that Claude
// can enforce directly. Other schemas are sent intact in the system instruction.
func supportsNativeClaudeGeminiOutputSchema(schema ojson.Result) bool {
	if !schema.IsObject() {
		return false
	}
	kind := schema.Get("type").Str
	switch kind {
	case "object":
		if schema.Get("additionalProperties").Type != ojson.False {
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
	schema.ForEach(func(key, value ojson.Result) bool {
		switch key.Str {
		case "title", "description":
			supported = value.Type == ojson.String
		case "type", "default":
			supported = kind != ""
		case "additionalProperties":
			supported = kind == "object"
		case "required":
			supported = kind == "object" && value.IsArray() && !slices.ContainsFunc(value.Array(), func(item ojson.Result) bool {
				return item.Type != ojson.String
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
			supported = kind != "" && value.IsArray() && value.Get("#").Int() != 0 && !slices.ContainsFunc(value.Array(), func(item ojson.Result) bool {
				return item.IsObject() || item.IsArray()
			})
		case "minItems":
			supported = kind == "array" && value.Type == ojson.Number && (value.Num == 0 || value.Num == 1)
		case "format":
			supported = kind == "string" && slices.Contains([]string{"date-time", "time", "date", "duration", "email", "hostname", "uri", "ipv4", "ipv6", "uuid"}, value.Str)
		default:
			supported = false
		}
		return supported
	})
	return supported
}

func allNativeClaudeGeminiOutputSchemasSupported(schemas ojson.Result) bool {
	supported := true
	schemas.ForEach(func(_, schema ojson.Result) bool {
		supported = supportsNativeClaudeGeminiOutputSchema(schema)
		return supported
	})
	return supported
}

// orderGeminiClaudeToolResults keeps tool results before other user content,
// including when adjacent Gemini user turns were merged into one Claude turn.
func orderGeminiClaudeToolResults(messages [][]byte) {
	for i, message := range messages {
		root := util.ParseGJSONBytesNoCopy(message)
		if root.Get("role").String() != "user" {
			continue
		}
		var results, other [][]byte
		hasOther, needsReorder := false, false
		root.Get("content").ForEach(func(_, part ojson.Result) bool {
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
			messageDoc := ojson.NewDoc(message)
			_ = messageDoc.SetRawBytes("content", translatorcommon.JoinRawArray(append(results, other...)))
			messages[i] = messageDoc.Bytes()
		}
	}
}

func normalizeClaudeToolSchema(parameters ojson.Result) []byte {
	cleanedDoc := ojson.NewDocString(util.NormalizeGeminiToolSchema(parameters.Raw))
	if parameters.Get("additionalProperties").Type != ojson.False {
		_ = cleanedDoc.Set("additionalProperties", false)
	}
	const schema = "http://json-schema.org/draft-07/schema#"
	currentSchema := parameters.Get("$schema")
	if currentSchema.Type != ojson.String || currentSchema.String() != schema {
		_ = cleanedDoc.Set("$schema", schema)
	}
	return cleanedDoc.Bytes()
}

func firstGeminiObject(parent ojson.Result, keys ...string) ojson.Result {
	for _, key := range keys {
		if value := parent.Get(key); value.Exists() && value.IsObject() {
			return value
		}
	}
	return ojson.Result{}
}

func firstGeminiArray(parent ojson.Result, keys ...string) ojson.Result {
	for _, key := range keys {
		if value := parent.Get(key); value.Exists() && value.IsArray() {
			return value
		}
	}
	return ojson.Result{}
}

func firstGeminiNumber(parent ojson.Result, keys ...string) ojson.Result {
	for _, key := range keys {
		if value := parent.Get(key); value.Exists() && value.Type == ojson.Number {
			return value
		}
	}
	return ojson.Result{}
}

func firstGeminiPositiveInt(parent ojson.Result, keys ...string) (int64, bool) {
	for _, key := range keys {
		if value := parent.Get(key); value.Exists() && value.Type == ojson.Number && value.Int() > 0 {
			return value.Int(), true
		}
	}
	return 0, false
}

func firstGeminiNonNegativeInt(parent ojson.Result, keys ...string) (int64, bool) {
	for _, key := range keys {
		value := parent.Get(key)
		if !value.Exists() || value.Type != ojson.Number {
			continue
		}
		f := value.Float()
		if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || math.Trunc(f) != f || f >= float64(math.MaxInt64) {
			continue
		}
		return int64(f), true
	}
	return 0, false
}

func geminiClaudeInlineData(part ojson.Result) ojson.Result {
	inlineData := part.Get("inlineData")
	if inlineData.Exists() {
		return inlineData
	}
	return part.Get("inline_data")
}

func geminiClaudeFileData(part ojson.Result) ojson.Result {
	fileData := part.Get("fileData")
	if fileData.Exists() {
		return fileData
	}
	return part.Get("file_data")
}

// claudeContentPartFromGeminiInlineData maps inline bytes onto a Claude block.
// Media that Claude cannot read becomes placeholder text only when
// keepPlaceholder is set; otherwise it reports false so the caller can refuse it.
func claudeContentPartFromGeminiInlineData(inlineData ojson.Result, keepPlaceholder bool) ([]byte, bool) {
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
		imageDoc := ojson.NewDoc([]byte(`{"type":"image","source":{"type":"base64","media_type":"","data":""}}`))
		_ = imageDoc.Set("source.media_type", mimeType)
		_ = imageDoc.Set("source.data", data)
		return imageDoc.Bytes(), true
	case strings.HasPrefix(lowerMimeType, "application/"), strings.HasPrefix(lowerMimeType, "text/"):
		docDoc := ojson.NewDoc([]byte(`{"type":"document","source":{"type":"base64","media_type":"","data":""}}`))
		_ = docDoc.Set("source.media_type", mimeType)
		_ = docDoc.Set("source.data", data)
		return docDoc.Bytes(), true
	default:
		if !keepPlaceholder {
			return nil, false
		}
		return claudeTextContentPart(fmt.Sprintf("Media content: inline data (Type: %s)", mimeType)), true
	}
}

func claudeContentPartFromGeminiFileData(fileData ojson.Result) ([]byte, bool) {
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
		imageContent, _ = ojson.SetBytes(imageContent, "source.url", fileURI)
		return imageContent, true
	case strings.HasPrefix(lowerMimeType, "application/"), strings.HasPrefix(lowerMimeType, "text/"):
		docDoc := ojson.NewDoc([]byte(`{"type":"document","source":{"type":"url","url":""}}`))
		_ = docDoc.Set("source.url", fileURI)
		if mimeType != "" {
			_ = docDoc.Set("source.media_type", mimeType)
		}
		return docDoc.Bytes(), true
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
	textContent, _ = ojson.SetBytes(textContent, "text", text)
	return textContent
}

func isEmptyGeminiClaudeResponseObject(v ojson.Result) bool {
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

func geminiClaudeResponseHasOnlyResultKey(responseField ojson.Result) bool {
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

func setClaudeToolResultContentFromGemini(toolResultDoc *ojson.Doc, fr ojson.Result, isAssistant bool, drops *translatorcommon.UserTurnDrops) error {
	resultField := fr.Get("response.result")
	responseField := fr.Get("response")
	hasBaseText := false
	baseText := ""
	if resultField.Exists() && geminiClaudeResponseHasOnlyResultKey(responseField) {
		baseText = resultField.String()
		hasBaseText = strings.TrimSpace(baseText) != ""
	} else if !isEmptyGeminiClaudeResponseObject(responseField) {
		baseText = responseField.Raw
		hasBaseText = true
	}

	var nestedBlocks [][]byte
	droppedType := ""
	if frParts := fr.Get("parts"); frParts.Exists() && frParts.IsArray() {
		frParts.ForEach(func(_, nested ojson.Result) bool {
			if text := nested.Get("text"); text.Exists() {
				if strings.TrimSpace(text.String()) != "" {
					nestedBlocks = append(nestedBlocks, claudeTextContentPart(text.String()))
				}
				return true
			}
			if inlineData := geminiClaudeInlineData(nested); inlineData.Exists() {
				if contentPart, ok := claudeContentPartFromGeminiInlineData(inlineData, isAssistant); ok {
					nestedBlocks = append(nestedBlocks, contentPart)
				} else if !isAssistant {
					if droppedType == "" {
						droppedType = "inlineData"
					}
					if drops != nil {
						drops.Drop("inlineData")
					}
				}
				return true
			}
			if fileData := geminiClaudeFileData(nested); fileData.Exists() {
				if contentPart, ok := claudeContentPartFromGeminiFileData(fileData); ok {
					nestedBlocks = append(nestedBlocks, contentPart)
				} else if !isAssistant {
					if droppedType == "" {
						droppedType = "fileData"
					}
					if drops != nil {
						drops.Drop("fileData")
					}
				}
				return true
			}
			return true
		})
	}

	if len(nestedBlocks) > 0 {
		blocks := make([][]byte, 0, len(nestedBlocks)+1)
		if hasBaseText {
			blocks = append(blocks, claudeTextContentPart(baseText))
		}
		blocks = append(blocks, nestedBlocks...)
		_ = toolResultDoc.SetRawBytes("content", translatorcommon.JoinRawArray(blocks))
		return nil
	}

	if resultField.Exists() && geminiClaudeResponseHasOnlyResultKey(responseField) {
		_ = toolResultDoc.Set("content", resultField.String())
	} else if responseField.Exists() {
		_ = toolResultDoc.Set("content", responseField.Raw)
	}
	if droppedType != "" && !hasBaseText {
		return &translatorcommon.UnsupportedPartError{Type: droppedType}
	}
	return nil
}
