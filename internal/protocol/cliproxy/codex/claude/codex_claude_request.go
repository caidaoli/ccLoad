// Package claude provides request translation functionality for Claude Code API compatibility.
// It handles parsing and transforming Claude Code API requests into the internal client format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package also performs JSON data cleaning and transformation to ensure compatibility
// between Claude Code API format and the internal client's expected format.
package claude

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"
	sigcompat "ccLoad/internal/protocol/cliproxy/signature"
	"ccLoad/internal/protocol/cliproxy/thinking"
	"ccLoad/internal/protocol/cliproxy/util"

	"github.com/MoeForge/moego/ojson"
)

// ConvertClaudeRequestToCodex parses and transforms a Claude Code API request into the internal client format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the internal client.
// The function performs the following transformations:
// 1. Sets up a template with the model name and empty instructions field
// 2. Processes system messages and converts them to developer input content
// 3. Transforms message contents (text, image, document, tool_use, tool_result) to appropriate formats
// 4. Converts tools declarations to the expected format
// 5. Adds additional configuration parameters for the Codex API
// 6. Maps Claude thinking configuration to Codex reasoning settings
// 7. Maps Claude output_config format to Codex text format
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the Claude Code API
//   - stream: A boolean indicating if the request is for a streaming response (unused in current implementation)
//
// Returns:
//   - []byte: The transformed request data in internal client format
func ConvertClaudeRequestToCodex(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	return convertClaudeRequestToCodex(modelName, inputRawJSON, stream, false)

}

// ConvertClaudeRequestToCodexWithCompat preserves assistant thinking blocks with
// empty or unknown-format signatures for configured compatibility endpoints.
func ConvertClaudeRequestToCodexWithCompat(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	return convertClaudeRequestToCodex(modelName, inputRawJSON, stream, true)
}

func convertClaudeRequestToCodex(modelName string, inputRawJSON []byte, _ bool, preserveEmptyThinkingBlocks bool) ([]byte, error) {
	rawJSON := inputRawJSON
	var drops translatorcommon.UserTurnDrops

	template := []byte(`{"model":"","instructions":"","input":[]}`)

	rootResult := ojson.ParseBytes(rawJSON)
	mayHaveToolAddition := claudePayloadMayContainToolAddition(rawJSON)
	toolNameMap := buildReverseMapFromClaudeOriginalToShortFromResult(rootResult, mayHaveToolAddition)
	signatureTargetModel := translatorcommon.ResolveCodexReplayTargetModel(modelName, rawJSON)
	template, _ = ojson.SetBytes(template, "model", modelName)
	maxOutputTokens := rootResult.Get("max_tokens")
	if !maxOutputTokens.Exists() {
		maxOutputTokens = rootResult.Get("max_output_tokens")
	}
	if maxOutputTokens.Type == ojson.Number {
		template, _ = ojson.SetBytes(template, "max_output_tokens", maxOutputTokens.Int())
	}
	inputItems := translatorcommon.NewRawArrayItems(rootResult.Get("messages.#").Int())
	toolsResult := rootResult.Get("tools")
	var declaredToolsByName map[string]ojson.Result
	declaredToolsIndexed := false
	hasAdditionalTools := false
	emittedToolNames := make(map[string]struct{})
	for _, tool := range toolsResult.Array() {
		if !isClaudeWebSearchToolType(tool.Get("type").String()) && !tool.Get("defer_loading").Bool() {
			emittedToolNames[tool.Get("name").String()] = struct{}{}
		}
	}

	// Codex exposes top-level instructions; keep system text there instead of
	// synthesizing a developer message so system-only requests stay minimal.
	systemsResult := rootResult.Get("system")
	if systemsResult.Exists() {
		var instructions []string

		appendSystemText := func(text string) {
			if text == "" || util.IsClaudeCodeAttributionSystemText(text) {
				return
			}
			instructions = append(instructions, text)
		}

		if systemsResult.Type == ojson.String {
			appendSystemText(systemsResult.String())
		} else if systemsResult.IsArray() {
			systemResults := systemsResult.Array()
			for i := 0; i < len(systemResults); i++ {
				systemResult := systemResults[i]
				if systemResult.Get("type").String() == "text" {
					appendSystemText(systemResult.Get("text").String())
				}
			}
		}

		if len(instructions) > 0 {
			template, _ = ojson.SetBytes(template, "instructions", strings.Join(instructions, "\n"))
		}
	}

	// Process messages and transform their contents to appropriate formats.
	messagesResult := rootResult.Get("messages")
	if messagesResult.IsArray() {
		messageResults := messagesResult.Array()
		var pendingToolUseIDs []string
		var pendingSystemReminders [][]byte

		for i := 0; i < len(messageResults); i++ {
			messageResult := messageResults[i]
			messageRole := messageResult.Get("role").String()
			if messageRole == "system" {
				systemContent := messageResult.Get("content")
				if reminderText, ok := translatorcommon.ClaudeMessageSystemReminderText(systemContent); ok {
					message := []byte(`{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}`)
					message, _ = ojson.SetBytes(message, "content.0.text", reminderText)
					if len(pendingToolUseIDs) > 0 {
						pendingSystemReminders = append(pendingSystemReminders, message)
					} else {
						inputItems = append(inputItems, message)
					}
				}
				if additionalTools, ok := convertClaudeSystemAdditionalTools(systemContent, toolsResult, &declaredToolsByName, &declaredToolsIndexed, toolNameMap, emittedToolNames); ok {
					hasAdditionalTools = true
					if len(pendingToolUseIDs) > 0 {
						pendingSystemReminders = append(pendingSystemReminders, additionalTools)
					} else {
						inputItems = append(inputItems, additionalTools)
					}
				}
				continue
			}

			messageContentsResult := messageResult.Get("content")
			if messageRole == "user" && len(pendingToolUseIDs) > 0 && messageContentsResult.IsArray() {
				messageContentsResult = translatorcommon.AlignClaudeToolResults(messageContentsResult, pendingToolUseIDs)
			}
			pendingToolUseIDs = nil
			contentItems := make([][]byte, 0, 4)
			// Counts only what this turn itself sends, not system reminders flushed beside it.
			// Empty text is forwarded but does not count: it can hide an emptied attachment.
			turnSendable := 0
			bufferedSendable := 0
			var turnDiscoveredTools [][]byte
			remainingToolResults := 0
			if messageContentsResult.IsArray() {
				for _, part := range messageContentsResult.Array() {
					if part.Get("type").String() == "tool_result" {
						remainingToolResults++
					}
				}
			}

			appendDiscoveredToolByReference := func(ref ojson.Result) ([]byte, bool) {
				refField := "tool_name"
				refName := ref.Get(refField).String()
				if refName == "" {
					refField = "name"
					refName = ref.Get(refField).String()
				}
				ensureClaudeDeclaredToolsIndexed(toolsResult, &declaredToolsByName, &declaredToolsIndexed)
				def, exists := declaredToolsByName[refName]
				if !exists || isClaudeWebSearchToolType(def.Get("type").String()) {
					return []byte(ref.Raw), false
				}
				if _, emitted := emittedToolNames[refName]; !emitted {
					emittedToolNames[refName] = struct{}{}
					turnDiscoveredTools = append(turnDiscoveredTools, convertClaudeFunctionToolToCodex(def, toolNameMap))
				}
				refJSON := []byte(ref.Raw)
				if short := toolNameMap[refName]; short != refName {
					refJSON, _ = ojson.SetBytes(refJSON, refField, short)
				}
				return refJSON, true
			}

			flushTurnDiscoveredTools := func() {
				if len(turnDiscoveredTools) == 0 || remainingToolResults > 0 {
					return
				}
				item := []byte(`{"type":"additional_tools","role":"developer","tools":[]}`)
				item, _ = ojson.SetRawBytes(item, "tools", translatorcommon.JoinRawArray(turnDiscoveredTools))
				inputItems = append(inputItems, item)
				turnDiscoveredTools = turnDiscoveredTools[:0]
				hasAdditionalTools = true
				turnSendable++
			}

			flushMessage := func() {
				if len(contentItems) > 0 {
					turnSendable += bufferedSendable
					bufferedSendable = 0
					message := []byte(`{"type":"message","role":""}`)
					message, _ = ojson.SetBytes(message, "role", messageRole)
					message, _ = ojson.SetRawBytes(message, "content", translatorcommon.JoinRawArray(contentItems))
					inputItems = append(inputItems, message)
					contentItems = contentItems[:0]
				}
			}

			appendTextContent := func(text string) {
				partType := "input_text"
				if messageRole == "assistant" {
					partType = "output_text"
				}
				content := []byte(`{"type":"","text":""}`)
				content, _ = ojson.SetBytes(content, "type", partType)
				content, _ = ojson.SetBytes(content, "text", text)
				contentItems = append(contentItems, content)
				if text != "" {
					bufferedSendable++
				}
			}

			appendImageContent := func(content []byte) {
				contentItems = append(contentItems, content)
				bufferedSendable++
			}

			appendFileContent := func(content []byte) {
				contentItems = append(contentItems, content)
				bufferedSendable++
			}

			appendCommentaryContent := func(part ojson.Result) {
				if messageRole != "assistant" {
					return
				}
				text := part.Get("thinking").String()
				if text == "" {
					return
				}
				// A relayed commentary block becomes its own assistant message item
				// carrying phase "commentary", mirroring the Codex output item it
				// came from. The internal marker is consumed here and never
				// forwarded upstream.
				flushMessage()
				content := []byte(`{"type":"output_text","text":""}`)
				content, _ = ojson.SetBytes(content, "text", text)
				message := []byte(`{"type":"message","role":"assistant","phase":"commentary"}`)
				message, _ = ojson.SetRawBytes(message, "content", translatorcommon.JoinRawArray([][]byte{content}))
				inputItems = append(inputItems, message)
			}

			appendReasoningContent := func(part ojson.Result) {
				if messageRole != "assistant" {
					return
				}

				rawSignature := part.Get("signature").String()
				if sigcompat.IsCodexCommentarySignatureMarker(rawSignature) {
					appendCommentaryContent(part)
					return
				}
				signature, ok := translatorcommon.CodexReplaySignatureForTarget(signatureTargetModel, rawSignature)
				if !ok && preserveEmptyThinkingBlocks && part.Get("signature").Type == ojson.String && strings.TrimSpace(rawSignature) != "" &&
					sigcompat.DetectSignatureProviderForBlock(rawSignature, sigcompat.SignatureBlockKindClaudeThinking) == sigcompat.SignatureProviderUnknown {
					signature = rawSignature
					ok = true
				}
				if !ok {
					if preserveEmptyThinkingBlocks && strings.TrimSpace(rawSignature) == "" {
						signature = rawSignature
					} else {
						return
					}
				}

				flushMessage()
				reasoningItem := []byte(`{"type":"reasoning","summary":[],"content":null}`)
				reasoningItem, _ = ojson.SetBytes(reasoningItem, "encrypted_content", signature)
				inputItems = append(inputItems, reasoningItem)
			}

			if messageContentsResult.IsArray() {
				messageContentResults := messageContentsResult.Array()
				for j := 0; j < len(messageContentResults); j++ {
					messageContentResult := messageContentResults[j]
					contentType := messageContentResult.Get("type").String()

					switch contentType {
					case "text":
						flushTurnDiscoveredTools()
						if len(pendingSystemReminders) > 0 {
							inputItems = append(inputItems, pendingSystemReminders...)
							pendingSystemReminders = nil
						}
						appendTextContent(messageContentResult.Get("text").String())
					case "thinking":
						flushTurnDiscoveredTools()
						appendReasoningContent(messageContentResult)
					case "redacted_thinking":
						flushTurnDiscoveredTools()
						if messageRole != "assistant" {
							continue
						}
						encryptedContent := messageContentResult.Get("data").String()
						if encryptedContent == "" {
							continue
						}
						flushMessage()
						reasoningItem := []byte(`{"type":"reasoning","summary":[],"content":null}`)
						reasoningItem, _ = ojson.SetBytes(reasoningItem, "encrypted_content", encryptedContent)
						inputItems = append(inputItems, reasoningItem)
					case "image":
						flushTurnDiscoveredTools()
						if len(pendingSystemReminders) > 0 {
							inputItems = append(inputItems, pendingSystemReminders...)
							pendingSystemReminders = nil
						}
						if imagePart, ok := claudeImageInputPart(messageContentResult.Get("source")); ok {
							appendImageContent(imagePart)
						} else if messageRole == "user" {
							drops.Drop(contentType)
						}
					case "document", "container_upload":
						flushTurnDiscoveredTools()
						if len(pendingSystemReminders) > 0 {
							inputItems = append(inputItems, pendingSystemReminders...)
							pendingSystemReminders = nil
						}
						if contentType == "document" {
							if docText, ok := translatorcommon.ClaudeDocumentTextBlock(messageContentResult); ok {
								appendTextContent(docText)
								break
							}
							var onImageDrop func()
							if messageRole == "user" {
								onImageDrop = func() { drops.Drop("image") }
							}
							if translatorcommon.ExpandClaudeContentDocument(
								messageContentResult,
								appendTextContent,
								func(image ojson.Result) ([]byte, bool) {
									return claudeImageInputPart(image.Get("source"))
								},
								appendImageContent,
								onImageDrop,
							) {
								break
							}
						}
						if filePart, ok := claudeDocumentInputFile(messageContentResult); ok {
							appendFileContent(filePart)
						} else if messageRole == "user" {
							drops.Drop(contentType)
						}
					case "tool_use":
						flushTurnDiscoveredTools()
						flushMessage()
						if id := messageContentResult.Get("id").String(); id != "" {
							pendingToolUseIDs = append(pendingToolUseIDs, id)
						}
						functionCallMessage := []byte(`{"type":"function_call"}`)
						functionCallMessage, _ = ojson.SetBytes(functionCallMessage, "call_id", shortenCodexCallIDIfNeeded(messageContentResult.Get("id").String()))
						{
							name := messageContentResult.Get("name").String()
							if short, ok := toolNameMap[name]; ok {
								name = short
							} else {
								name = shortenNameIfNeeded(name)
							}
							functionCallMessage, _ = ojson.SetBytes(functionCallMessage, "name", name)
						}
						arguments := strings.TrimSpace(messageContentResult.Get("input").Raw)
						if arguments == "" {
							arguments = "{}"
						}
						functionCallMessage, _ = ojson.SetBytes(functionCallMessage, "arguments", arguments)
						inputItems = append(inputItems, functionCallMessage)
					case "tool_reference", "tool_search_tool_result":
						flushMessage()
						if len(pendingSystemReminders) > 0 {
							inputItems = append(inputItems, pendingSystemReminders...)
							pendingSystemReminders = nil
						}
						refs := []ojson.Result{messageContentResult}
						if contentType == "tool_search_tool_result" {
							toolRefs := messageContentResult.Get("content.tool_references")
							if !toolRefs.IsArray() {
								return nil, fmt.Errorf("unsupported Anthropic tool_search_tool_result content: %s", messageContentResult.Get("content").Raw)
							}
							refs = toolRefs.Array()
						}
						for _, ref := range refs {
							if ref.Get("type").String() != "tool_reference" {
								return nil, fmt.Errorf("unsupported Anthropic tool search reference: %s", ref.Raw)
							}
							if _, resolved := appendDiscoveredToolByReference(ref); !resolved {
								return nil, fmt.Errorf("anthropic %s has no matching tool declaration", ref.Get("type").String())
							}
						}
					case "tool_result":
						flushMessage()
						functionCallOutputMessage := []byte(`{"type":"function_call_output"}`)
						functionCallOutputMessage, _ = ojson.SetBytes(functionCallOutputMessage, "call_id", shortenCodexCallIDIfNeeded(messageContentResult.Get("tool_use_id").String()))

						contentResult := messageContentResult.Get("content")
						if contentResult.IsArray() {
							contentResults := contentResult.Array()
							toolResultContentItems := make([][]byte, 0, len(contentResults))
							var rewrittenContent [][]byte
							hasRichContent := false
							for k := 0; k < len(contentResults); k++ {
								part := contentResults[k]
								var refJSON []byte
								toolResultContentType := contentResults[k].Get("type").String()
								switch toolResultContentType {
								case "image":
									if imagePart, ok := claudeImageInputPart(part.Get("source")); ok {
										toolResultContentItems = append(toolResultContentItems, imagePart)
										hasRichContent = true
									}
								case "document":
									toolResultContentItems = append(toolResultContentItems, codexToolResultDocumentParts(part)...)
									hasRichContent = true
								case "text":
									toolResultContent := []byte(`{"type":"input_text","text":""}`)
									toolResultContent, _ = ojson.SetBytes(toolResultContent, "text", contentResults[k].Get("text").String())
									toolResultContentItems = append(toolResultContentItems, toolResultContent)
									hasRichContent = true
								case "tool_reference":
									refJSON, _ = appendDiscoveredToolByReference(part)
									if string(refJSON) != part.Raw && rewrittenContent == nil {
										rewrittenContent = make([][]byte, 0, len(contentResults))
										for _, previous := range contentResults[:k] {
											rewrittenContent = append(rewrittenContent, []byte(previous.Raw))
										}
									}
									toolResultContent := []byte(`{"type":"input_text","text":""}`)
									toolResultContent, _ = ojson.SetBytes(toolResultContent, "text", string(refJSON))
									toolResultContentItems = append(toolResultContentItems, toolResultContent)
								}
								if rewrittenContent != nil {
									if refJSON != nil {
										rewrittenContent = append(rewrittenContent, refJSON)
									} else {
										rewrittenContent = append(rewrittenContent, []byte(part.Raw))
									}
								}
							}
							if hasRichContent {
								functionCallOutputMessage, _ = ojson.SetRawBytes(functionCallOutputMessage, "output", translatorcommon.JoinRawArray(toolResultContentItems))
							} else if rewrittenContent != nil {
								functionCallOutputMessage, _ = ojson.SetBytes(functionCallOutputMessage, "output", string(translatorcommon.JoinRawArray(rewrittenContent)))
							} else {
								functionCallOutputMessage, _ = ojson.SetBytes(functionCallOutputMessage, "output", contentResult.String())
							}
						} else if imagePart, ok := claudeImageInputPart(contentResult.Get("source")); ok && contentResult.Get("type").String() == "image" {
							functionCallOutputMessage, _ = ojson.SetRawBytes(functionCallOutputMessage, "output", translatorcommon.JoinRawArray([][]byte{imagePart}))
						} else if contentResult.IsObject() && contentResult.Get("type").String() == "document" {
							functionCallOutputMessage, _ = ojson.SetRawBytes(functionCallOutputMessage, "output", translatorcommon.JoinRawArray(codexToolResultDocumentParts(contentResult)))
						} else {
							functionCallOutputMessage, _ = ojson.SetBytes(functionCallOutputMessage, "output", messageContentResult.Get("content").String())
						}

						inputItems = append(inputItems, functionCallOutputMessage)
						remainingToolResults--
						turnSendable++
					}
				}
				flushMessage()
				flushTurnDiscoveredTools()
				if len(pendingSystemReminders) > 0 {
					inputItems = append(inputItems, pendingSystemReminders...)
					pendingSystemReminders = nil
				}
			} else if messageContentsResult.Type == ojson.String {
				appendTextContent(messageContentsResult.String())
				flushMessage()
				if len(pendingSystemReminders) > 0 {
					inputItems = append(inputItems, pendingSystemReminders...)
					pendingSystemReminders = nil
				}
			}
			if messageRole == "user" {
				drops.EndTurn(turnSendable)
			}
		}

		if len(pendingSystemReminders) > 0 {
			inputItems = append(inputItems, pendingSystemReminders...)
		}
	}

	// Convert tools declarations to the expected format for the Codex API.
	hasWebSearchTool := false
	var toolItems [][]byte
	if toolsResult.IsArray() || hasAdditionalTools {
		webSearchToolNames := buildClaudeWebSearchToolNameSet(toolsResult)
		template, _ = ojson.SetRawBytes(template, "tool_choice", convertClaudeToolChoiceToCodex(rootResult.Get("tool_choice"), toolNameMap, webSearchToolNames))
	}
	if toolsResult.IsArray() {
		forcedToolName := ""
		if tc := rootResult.Get("tool_choice"); tc.Get("type").String() == "tool" {
			forcedToolName = tc.Get("name").String()
		}
		toolResults := toolsResult.Array()
		toolItems = make([][]byte, 0, len(toolResults))
		for i := 0; i < len(toolResults); i++ {
			toolResult := toolResults[i]
			// Special handling: map Claude web search tool to Codex web_search
			if isClaudeWebSearchToolType(toolResult.Get("type").String()) {
				hasWebSearchTool = true
				toolItems = append(toolItems, convertClaudeWebSearchToolToCodex(toolResult))
				continue
			}
			if toolResult.Get("defer_loading").Bool() {
				name := toolResult.Get("name").String()
				if _, emitted := emittedToolNames[name]; emitted || forcedToolName == "" || name != forcedToolName {
					continue
				}
			}
			toolItems = append(toolItems, convertClaudeFunctionToolToCodex(toolResult, toolNameMap))
		}
	}

	// Default to parallel tool calls unless tool_choice explicitly disables them.
	parallelToolCalls := true
	if disableParallelToolUse := rootResult.Get("tool_choice.disable_parallel_tool_use"); disableParallelToolUse.Exists() {
		parallelToolCalls = !disableParallelToolUse.Bool()
	}

	// Add additional configuration parameters for the Codex API.
	template, _ = ojson.SetBytes(template, "parallel_tool_calls", parallelToolCalls)

	// Convert Claude thinking budgets and effort to reasoning.effort.
	reasoningEffort := "medium"
	effort := ""
	if v := rootResult.Get("output_config.effort"); v.Exists() && v.Type == ojson.String {
		effort = strings.ToLower(strings.TrimSpace(v.String()))
	}
	if thinkingConfig := rootResult.Get("thinking"); thinkingConfig.Exists() && thinkingConfig.IsObject() {
		switch thinkingConfig.Get("type").String() {
		case "enabled":
			if budgetTokens := thinkingConfig.Get("budget_tokens"); budgetTokens.Exists() {
				budget := int(budgetTokens.Int())
				if effort, ok := thinking.ConvertBudgetToModelLevel(budget, modelName, "codex"); ok {
					reasoningEffort = effort
				}
			}
		case "adaptive", "auto":
			// Adaptive thinking can carry an explicit effort in output_config.effort (Claude 4.6).
			// Pass through directly; ApplyThinking handles clamping to target model's levels.
			if effort != "" {
				reasoningEffort = effort
			} else {
				reasoningEffort = string(thinking.LevelXHigh)
			}
		case "disabled":
			if effort, ok := thinking.ConvertBudgetToLevel(0); ok && effort != "" {
				reasoningEffort = effort
			}
		}
	} else if !thinkingConfig.Exists() && effort != "" {
		reasoningEffort = effort
	}
	template, _ = ojson.SetBytes(template, "reasoning.effort", reasoningEffort)
	// OpenAI documents reasoning summaries as explicit opt-in output. Leave
	// reasoning.summary to the source request's canonical summary intent instead
	// of coupling it to reasoning effort.
	serviceTier := normalizeCodexServiceTier(rootResult.Get("service_tier"))
	if speed := rootResult.Get("speed"); speed.Type == ojson.String && speed.String() == "fast" {
		serviceTier = "priority"
	}
	if serviceTier != "" {
		template, _ = ojson.SetBytes(template, "service_tier", serviceTier)
	}
	template, _ = ojson.SetBytes(template, "stream", true)
	template, _ = ojson.SetBytes(template, "store", false)
	includeFields := []string{"reasoning.encrypted_content"}
	if hasWebSearchTool {
		includeFields = append(includeFields, "web_search_call.action.sources")
	}
	template, _ = ojson.SetBytes(template, "include", includeFields)

	// Map Claude output_config.format to Codex Responses text.format.
	if format := rootResult.Get("output_config.format"); format.IsObject() && format.Get("type").String() == "json_schema" && format.Get("schema").IsObject() {
		name := "cli_proxy_structured_output"
		if n := format.Get("name").String(); n != "" {
			name = n
		}
		strict := true
		if s := format.Get("strict"); s.Exists() && s.Type == ojson.False {
			strict = false
		}
		// OpenAI strict mode requires every declared property to be listed
		// in required. Downgrade instead of emitting an unsatisfiable
		// strict schema that strict backends reject with HTTP 400.
		if strict && codexSchemaMissesRequired(format.Get("schema")) {
			strict = false
		}
		translatedFormat := []byte(`{"type":"json_schema","name":"","strict":true,"schema":{}}`)
		translatedFormat, _ = ojson.SetBytes(translatedFormat, "name", name)
		translatedFormat, _ = ojson.SetBytes(translatedFormat, "strict", strict)
		translatedFormat, _ = ojson.SetRawBytes(translatedFormat, "schema", []byte(format.Get("schema").Raw))
		template, _ = ojson.SetRawBytes(template, "text.format", translatedFormat)
	}

	if toolsResult.IsArray() {
		template, _ = ojson.SetRawBytes(template, "tools", translatorcommon.JoinRawArray(toolItems))
	}
	template = translatorcommon.SetRawArrayItems(template, "input", inputItems)
	if len(inputItems) == 0 {
		template, _ = ojson.DeleteBytes(template, "input")
	}

	return template, drops.Err()
}

func normalizeCodexServiceTier(result ojson.Result) string {
	if !result.Exists() || result.Type != ojson.String {
		return ""
	}

	switch strings.ToLower(strings.TrimSpace(result.String())) {
	case "fast", "priority":
		return "priority"
	default:
		return ""
	}
}

// shortenCodexCallIDIfNeeded keeps Claude tool IDs within the OpenAI Responses
// API call_id limit while preserving a stable, low-collision mapping.
func shortenCodexCallIDIfNeeded(id string) string {
	const limit = 64
	if len(id) <= limit {
		return id
	}

	sum := sha256.Sum256([]byte(id))
	suffix := "_" + hex.EncodeToString(sum[:8])
	prefixLen := limit - len(suffix)
	if prefixLen <= 0 {
		return suffix[len(suffix)-limit:]
	}
	return id[:prefixLen] + suffix
}

func isClaudeWebSearchToolType(toolType string) bool {
	return toolType == "web_search_20250305" || toolType == "web_search_20260209"
}

func buildClaudeWebSearchToolNameSet(tools ojson.Result) map[string]struct{} {
	names := map[string]struct{}{}
	if !tools.IsArray() {
		return names
	}

	tools.ForEach(func(_, tool ojson.Result) bool {
		toolType := tool.Get("type").String()
		if !isClaudeWebSearchToolType(toolType) {
			return true
		}

		if name := tool.Get("name").String(); name != "" {
			names[name] = struct{}{}
		}
		return true
	})

	return names
}

func convertClaudeToolChoiceToCodex(toolChoice ojson.Result, toolNameMap map[string]string, webSearchToolNames map[string]struct{}) []byte {
	if !toolChoice.Exists() || toolChoice.Type == ojson.Null {
		return []byte(`"auto"`)
	}

	choiceType := toolChoice.Get("type").String()
	if choiceType == "" && toolChoice.Type == ojson.String {
		choiceType = toolChoice.String()
	}

	switch choiceType {
	case "auto", "":
		return []byte(`"auto"`)
	case "any":
		return []byte(`"required"`)
	case "none":
		return []byte(`"none"`)
	case "tool":
		name := toolChoice.Get("name").String()
		if _, ok := webSearchToolNames[name]; ok {
			return []byte(`{"type":"web_search"}`)
		}
		if short, ok := toolNameMap[name]; ok {
			name = short
		} else {
			name = shortenNameIfNeeded(name)
		}
		if name == "" {
			return []byte(`"auto"`)
		}

		choice := []byte(`{"type":"function","name":""}`)
		choice, _ = ojson.SetBytes(choice, "name", name)
		return choice
	default:
		return []byte(`"auto"`)
	}
}

func convertClaudeWebSearchToolToCodex(tool ojson.Result) []byte {
	out := []byte(`{"type":"web_search"}`)
	if allowedDomains := tool.Get("allowed_domains"); allowedDomains.Exists() && allowedDomains.IsArray() {
		out, _ = ojson.SetRawBytes(out, "filters.allowed_domains", []byte(allowedDomains.Raw))
	}
	if userLocation := tool.Get("user_location"); userLocation.Exists() && userLocation.IsObject() {
		out, _ = ojson.SetRawBytes(out, "user_location", []byte(userLocation.Raw))
	}
	return out
}

// gatewayToolNamePattern matches gateway tool ids of the form
// `[mcp_]<server words>-<hex id...>_<tool>`, e.g.
// `mcp_github-12345678-9abc-def0-1234-56789abcdef0-12345678_add-issue-comment`.
var gatewayToolNamePattern = regexp.MustCompile(`^(?:mcp_)?([a-z][a-z0-9-]*?)-[0-9a-f]{6,}(?:-[0-9a-f]+)*_(.+)$`)

// shortenNameIfNeeded applies a simple shortening rule for a single name.
func shortenNameIfNeeded(name string) string {
	const limit = 64
	if len(name) <= limit {
		return name
	}
	if strings.HasPrefix(name, "mcp__") {
		idx := strings.LastIndex(name, "__")
		if idx > 0 {
			rest := name[idx+2:]
			cand := "mcp__" + rest
			if len(cand) <= limit {
				return cand
			}
			// Plain truncation keeps only the shared server prefix, so every tool of a
			// long-named server collapses to the same 64 characters and the model cannot
			// tell them apart. Keep the server's words and the tool's own name instead.
			if m := gatewayToolNamePattern.FindStringSubmatch(rest); m != nil {
				if readable := "mcp__" + m[1] + "_" + m[2]; len(readable) <= limit {
					return readable
				}
			}
			return cand[:limit]
		}
	}
	return name[:limit]
}

// buildShortNameMap ensures uniqueness of shortened names within a request.
func buildShortNameMap(names []string) map[string]string {
	const limit = 64
	used := map[string]struct{}{}
	m := map[string]string{}

	baseCandidate := shortenNameIfNeeded

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
		if _, exists := m[n]; exists {
			continue
		}
		cand := baseCandidate(n)
		uniq := makeUnique(cand)
		used[uniq] = struct{}{}
		m[n] = uniq
	}
	return m
}

func convertClaudeFunctionToolToCodex(toolResult ojson.Result, toolNameMap map[string]string) []byte {
	tool := []byte(toolResult.Raw)
	if toolResult.Get("type").Type != ojson.String || toolResult.Get("type").String() != "function" {
		tool, _ = ojson.SetBytes(tool, "type", "function")
	}
	// Apply shortened name if needed
	if v := toolResult.Get("name"); v.Exists() {
		originalName := v.String()
		name := originalName
		if short, ok := toolNameMap[name]; ok {
			name = short
		} else {
			name = shortenNameIfNeeded(name)
		}
		if v.Type != ojson.String || name != originalName {
			tool, _ = ojson.SetBytes(tool, "name", name)
		}
	}
	tool, _ = ojson.SetRawBytes(tool, "parameters", []byte(normalizeToolParameters(toolResult.Get("input_schema").Raw)))
	for _, path := range []string{"input_schema", "parameters.$schema", "cache_control", "defer_loading"} {
		if ojson.GetBytes(tool, path).Exists() {
			tool, _ = ojson.DeleteBytes(tool, path)
		}
	}
	if ojson.GetBytes(tool, "strict").Type != ojson.False {
		tool, _ = ojson.SetBytes(tool, "strict", false)
	}
	return tool
}

func ensureClaudeDeclaredToolsIndexed(toolsResult ojson.Result, declaredToolsByName *map[string]ojson.Result, declaredToolsIndexed *bool) {
	if *declaredToolsIndexed {
		return
	}
	*declaredToolsIndexed = true
	if !toolsResult.IsArray() {
		return
	}
	toolArr := toolsResult.Array()
	if *declaredToolsByName == nil {
		*declaredToolsByName = make(map[string]ojson.Result, len(toolArr))
	}
	for i := 0; i < len(toolArr); i++ {
		if name := toolArr[i].Get("name").String(); name != "" {
			if _, exists := (*declaredToolsByName)[name]; !exists {
				(*declaredToolsByName)[name] = toolArr[i]
			}
		}
	}
}

// convertClaudeSystemAdditionalTools converts mid-conversation tool_addition blocks
// inside a role=system message into a Responses additional_tools input item.
func convertClaudeSystemAdditionalTools(content, toolsResult ojson.Result, declaredToolsByName *map[string]ojson.Result, declaredToolsIndexed *bool, toolNameMap map[string]string, emittedToolNames map[string]struct{}) ([]byte, bool) {
	if !content.IsArray() {
		return nil, false
	}
	parts := content.Array()
	var toolItems [][]byte
	var indexByName map[string]int
	for i := 0; i < len(parts); i++ {
		part := parts[i]
		if part.Get("type").String() != "tool_addition" {
			continue
		}
		toolSpec := part.Get("tool")
		var def ojson.Result
		isReference := false
		switch toolSpec.Get("type").String() {
		case "tool_definition":
			def = toolSpec.Get("definition")
			if def.IsObject() {
				if name := def.Get("name").String(); name != "" {
					ensureClaudeDeclaredToolsIndexed(toolsResult, declaredToolsByName, declaredToolsIndexed)
					if *declaredToolsByName == nil {
						*declaredToolsByName = make(map[string]ojson.Result)
					}
					(*declaredToolsByName)[name] = def
				}
			}
		case "tool_reference":
			isReference = true
			ensureClaudeDeclaredToolsIndexed(toolsResult, declaredToolsByName, declaredToolsIndexed)
			refName := toolSpec.Get("name").String()
			if refName == "" {
				refName = toolSpec.Get("tool_name").String()
			}
			if refName != "" && *declaredToolsByName != nil {
				def = (*declaredToolsByName)[refName]
			}
		default:
			if candidate := toolSpec.Get("definition"); candidate.IsObject() {
				def = candidate
				if name := def.Get("name").String(); name != "" {
					ensureClaudeDeclaredToolsIndexed(toolsResult, declaredToolsByName, declaredToolsIndexed)
					if *declaredToolsByName == nil {
						*declaredToolsByName = make(map[string]ojson.Result)
					}
					(*declaredToolsByName)[name] = def
				}
			}
		}
		if !def.IsObject() || isClaudeWebSearchToolType(def.Get("type").String()) {
			continue
		}
		origName := strings.TrimSpace(def.Get("name").String())
		if origName == "" {
			continue
		}
		if _, emitted := emittedToolNames[origName]; isReference && emitted {
			continue
		}
		emittedToolNames[origName] = struct{}{}
		converted := convertClaudeFunctionToolToCodex(def, toolNameMap)
		if indexByName == nil {
			indexByName = make(map[string]int)
		}
		if idx, exists := indexByName[origName]; exists {
			toolItems[idx] = converted
		} else {
			indexByName[origName] = len(toolItems)
			toolItems = append(toolItems, converted)
		}
	}
	if len(toolItems) == 0 {
		return nil, false
	}
	item := []byte(`{"type":"additional_tools","role":"developer","tools":[]}`)
	item, _ = ojson.SetRawBytes(item, "tools", translatorcommon.JoinRawArray(toolItems))
	return item, true
}

func collectClaudeDeclaredToolNames(rootResult ojson.Result, hasDynamicTools bool) []string {
	var names []string
	var deferredNames []string
	deferredSet := make(map[string]struct{})
	tools := rootResult.Get("tools")
	if tools.IsArray() {
		arr := tools.Array()
		names = make([]string, 0, len(arr))
		for i := 0; i < len(arr); i++ {
			n := arr[i].Get("name").String()
			if n == "" {
				continue
			}
			if hasDynamicTools && arr[i].Get("defer_loading").Bool() {
				deferredNames = append(deferredNames, n)
				deferredSet[n] = struct{}{}
				continue
			}
			names = append(names, n)
		}
	}
	if !hasDynamicTools {
		return names
	}
	appendDeferredReference := func(ref ojson.Result) {
		name := ref.Get("tool_name").String()
		if name == "" {
			name = ref.Get("name").String()
		}
		if _, deferred := deferredSet[name]; deferred {
			names = append(names, name)
		}
	}
	for _, msg := range rootResult.Get("messages").Array() {
		content := msg.Get("content")
		if !content.IsArray() {
			continue
		}
		for _, part := range content.Array() {
			switch part.Get("type").String() {
			case "tool_addition":
				if msg.Get("role").String() != "system" {
					continue
				}
				tool := part.Get("tool")
				def := tool.Get("definition")
				if tool.Get("type").String() == "tool_reference" {
					appendDeferredReference(tool)
				} else if def.IsObject() && !isClaudeWebSearchToolType(def.Get("type").String()) {
					if name := def.Get("name").String(); name != "" {
						names = append(names, name)
					}
				}
			case "tool_reference":
				appendDeferredReference(part)
			case "tool_search_tool_result":
				for _, ref := range part.Get("content.tool_references").Array() {
					appendDeferredReference(ref)
				}
			case "tool_result":
				for _, ref := range part.Get("content").Array() {
					if ref.Get("type").String() == "tool_reference" {
						appendDeferredReference(ref)
					}
				}
			}
		}
	}
	return append(names, deferredNames...)
}

func buildReverseMapFromClaudeOriginalToShortFromResult(rootResult ojson.Result, hasToolAddition bool) map[string]string {
	names := collectClaudeDeclaredToolNames(rootResult, hasToolAddition)
	if len(names) == 0 {
		return map[string]string{}
	}
	return buildShortNameMap(names)
}

func claudePayloadMayContainToolAddition(rawJSON []byte) bool {
	return bytes.Contains(rawJSON, []byte("tool_addition")) ||
		bytes.Contains(rawJSON, []byte("defer_loading")) ||
		bytes.Contains(rawJSON, []byte("tool_reference")) ||
		bytes.Contains(rawJSON, []byte(`\u`)) ||
		bytes.Contains(rawJSON, []byte(`\U`))
}

// buildReverseMapFromClaudeOriginalToShort builds original->short map, used to map tool_use names to short.
func buildReverseMapFromClaudeOriginalToShort(original []byte) map[string]string {
	mayHaveToolAddition := claudePayloadMayContainToolAddition(original)
	if !mayHaveToolAddition {
		tools := ojson.GetBytes(original, "tools")
		m := map[string]string{}
		if !tools.IsArray() {
			return m
		}
		var names []string
		arr := tools.Array()
		for i := 0; i < len(arr); i++ {
			n := arr[i].Get("name").String()
			if n != "" {
				names = append(names, n)
			}
		}
		if len(names) > 0 {
			m = buildShortNameMap(names)
		}
		return m
	}
	return buildReverseMapFromClaudeOriginalToShortFromResult(ojson.ParseBytes(original), true)
}

// normalizeToolParameters ensures object schemas contain at least an empty properties map,
// strips dialect keywords ($schema, $id), and drops regex patterns containing unsupported
// Unicode property escapes (\p{...} / \P{...}) that cause upstream schema validation failures.
func normalizeToolParameters(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" || !ojson.Valid(raw) {
		return `{"type":"object","properties":{}}`
	}
	var root map[string]any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if errDecode := decoder.Decode(&root); errDecode != nil || root == nil {
		return `{"type":"object","properties":{}}`
	}

	stripDialectKeywordsFromSchema(root)

	typeVal, typeExists := root["type"]
	isObject := false
	if !typeExists || typeVal == nil || typeVal == "" {
		root["type"] = "object"
		isObject = true
	} else {
		switch t := typeVal.(type) {
		case string:
			if t == "object" {
				isObject = true
			}
		case []any:
			for _, elem := range t {
				if elemStr, ok := elem.(string); ok && elemStr == "object" {
					isObject = true
					break
				}
			}
		}
	}
	if isObject {
		if props, ok := root["properties"]; !ok || props == nil {
			root["properties"] = map[string]any{}
		}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if errEncode := enc.Encode(root); errEncode != nil {
		return `{"type":"object","properties":{}}`
	}
	return strings.TrimSpace(buf.String())
}

func stripDialectKeywordsFromSchema(v any) {
	switch schema := v.(type) {
	case map[string]any:
		delete(schema, "$schema")
		delete(schema, "$id")
		if patternVal, ok := schema["pattern"].(string); ok && util.HasUnsupportedUnicodePropertyEscape(patternVal) {
			delete(schema, "pattern")
		}

		// Inspect regex keys under patternProperties
		if patternProps, ok := schema["patternProperties"].(map[string]any); ok {
			for patternKey, subSchema := range patternProps {
				if util.HasUnsupportedUnicodePropertyEscape(patternKey) {
					delete(patternProps, patternKey)
				} else {
					stripDialectKeywordsFromSchema(subSchema)
				}
			}
		}

		for _, mapKey := range codexSchemaMapKeywords {
			if mapKey == "patternProperties" {
				continue
			}
			if subMap, ok := schema[mapKey].(map[string]any); ok {
				for _, subSchema := range subMap {
					stripDialectKeywordsFromSchema(subSchema)
				}
			}
		}

		for _, valKey := range codexSchemaValueKeywords {
			if val, exists := schema[valKey]; exists {
				switch sub := val.(type) {
				case map[string]any:
					stripDialectKeywordsFromSchema(sub)
				case []any:
					for _, item := range sub {
						stripDialectKeywordsFromSchema(item)
					}
				}
			}
		}
	case []any:
		for _, item := range schema {
			stripDialectKeywordsFromSchema(item)
		}
	}
}

// codexSchemaMapKeywords and codexSchemaValueKeywords reference the unified JSON Schema keywords
// declared in internal/util.
var (
	codexSchemaMapKeywords   = util.SchemaMapKeywords
	codexSchemaValueKeywords = util.SchemaValueKeywords
)

// codexSchemaMissesRequired reports whether a JSON Schema has any declared
// property missing from its sibling required list (recursively). OpenAI
// strict mode rejects such schemas with HTTP 400, so callers downgrade
// text.format.strict instead of emitting an unsatisfiable schema.
func codexSchemaMissesRequired(schema ojson.Result) bool {
	if !schema.IsObject() {
		if schema.IsArray() {
			miss := false
			schema.ForEach(func(_, child ojson.Result) bool {
				if codexSchemaMissesRequired(child) {
					miss = true
					return false
				}
				return true
			})
			return miss
		}
		return false
	}
	if properties := schema.Get("properties"); properties.IsObject() {
		required := schema.Get("required")
		if !required.IsArray() {
			return len(properties.Map()) > 0
		}
		names := make(map[string]struct{}, len(required.Array()))
		for _, item := range required.Array() {
			if item.Type == ojson.String {
				names[item.String()] = struct{}{}
			}
		}
		for name := range properties.Map() {
			if _, ok := names[name]; !ok {
				return true
			}
		}
	}
	for _, keyword := range codexSchemaMapKeywords {
		children := schema.Get(keyword)
		if !children.IsObject() {
			continue
		}
		miss := false
		children.ForEach(func(_, child ojson.Result) bool {
			if codexSchemaMissesRequired(child) {
				miss = true
				return false
			}
			return true
		})
		if miss {
			return true
		}
	}
	for _, keyword := range codexSchemaValueKeywords {
		if child := schema.Get(keyword); child.Exists() && codexSchemaMissesRequired(child) {
			return true
		}
	}
	return false
}

// claudeDocumentInputFile maps a Claude document block onto a Responses
// input_file part. A file id or base64 data is carried as-is (base64 PDF becomes
// a data URL) and an HTTP(S) url becomes file_url. Text and content sources are
// converted by the caller, so they report false here like an empty source.
func claudeDocumentInputFile(part ojson.Result) ([]byte, bool) {
	source := part.Get("source")
	filename := part.Get("title").String()
	if filename == "" {
		filename = source.Get("filename").String()
	}
	content := []byte(`{"type":"input_file"}`)
	switch source.Get("type").String() {
	case "text", "content":
		return nil, false
	case "url":
		fileURL := strings.TrimSpace(source.Get("url").String())
		if !translatorcommon.IsHTTPURL(fileURL) {
			return nil, false
		}
		content, _ = ojson.SetBytes(content, "file_url", fileURL)
	default:
		fileID := source.Get("file_id").String()
		fileData := source.Get("data").String()
		if fileData == "" {
			fileData = source.Get("base64").String()
		}
		mediaType := strings.TrimSpace(source.Get("media_type").String())
		if source.Get("type").String() == "base64" && strings.EqualFold(mediaType, "application/pdf") && fileData != "" {
			fileData = fmt.Sprintf("data:%s;base64,%s", mediaType, fileData)
			if filename == "" {
				filename = "document.pdf"
			}
		}
		switch {
		case fileID != "":
			content, _ = ojson.SetBytes(content, "file_id", fileID)
		case fileData != "":
			content, _ = ojson.SetBytes(content, "file_data", fileData)
		default:
			return nil, false
		}
	}
	if filename != "" {
		content, _ = ojson.SetBytes(content, "filename", filename)
	}
	return content, true
}

// codexUnsupportedDocumentPlaceholder replaces a tool_result document that has
// no Responses representation, so the model still sees that one was returned.
const codexUnsupportedDocumentPlaceholder = "[Unsupported document content]"

func codexInputTextPart(text string) []byte {
	part := []byte(`{"type":"input_text","text":""}`)
	part, _ = ojson.SetBytes(part, "text", text)
	return part
}

// codexToolResultDocumentParts converts a Claude document nested in a
// tool_result into Responses function_call_output content items.
func codexToolResultDocumentParts(part ojson.Result) [][]byte {
	if text, ok := translatorcommon.ClaudeDocumentTextBlock(part); ok {
		return [][]byte{codexInputTextPart(text)}
	}
	var items [][]byte
	if translatorcommon.ExpandClaudeContentDocument(
		part,
		func(text string) { items = append(items, codexInputTextPart(text)) },
		func(image ojson.Result) ([]byte, bool) {
			return claudeImageInputPart(image.Get("source"))
		},
		func(image []byte) { items = append(items, image) },
		nil,
	) {
		return items
	}
	if filePart, ok := claudeDocumentInputFile(part); ok {
		return [][]byte{filePart}
	}
	return [][]byte{codexInputTextPart(codexUnsupportedDocumentPlaceholder)}
}

// claudeImageInputPart maps a Claude image source onto a Responses input_image
// part. A url passes through, base64 bytes become a data URL and a file id is
// carried as file_id. It reports false for a source with none of them.
func claudeImageInputPart(source ojson.Result) ([]byte, bool) {
	if !source.Exists() {
		return nil, false
	}
	part := []byte(`{"type":"input_image"}`)
	data := source.Get("data").String()
	if data == "" {
		data = source.Get("base64").String()
	}
	switch {
	case source.Get("url").String() != "":
		part, _ = ojson.SetBytes(part, "image_url", source.Get("url").String())
	case data != "":
		mediaType := source.Get("media_type").String()
		if mediaType == "" {
			mediaType = source.Get("mime_type").String()
		}
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		part, _ = ojson.SetBytes(part, "image_url", fmt.Sprintf("data:%s;base64,%s", mediaType, data))
	case source.Get("type").String() == "file" && source.Get("file_id").String() != "":
		part, _ = ojson.SetBytes(part, "file_id", source.Get("file_id").String())
	default:
		return nil, false
	}
	return part, true
}
