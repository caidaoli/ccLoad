// Package claude provides request translation functionality for Anthropic to OpenAI API.
// It handles parsing and transforming Anthropic API requests into OpenAI Chat Completions API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Anthropic API format and OpenAI API's expected format.
package claude

import (
	"encoding/base64"
	"strings"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"
	sigcompat "ccLoad/internal/protocol/cliproxy/signature"
	"ccLoad/internal/protocol/cliproxy/thinking"
	"ccLoad/internal/protocol/cliproxy/util"

	"github.com/MoeForge/moego/ojson"
)

// ConvertClaudeRequestToOpenAI parses and transforms an Anthropic API request into OpenAI Chat Completions API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the OpenAI API.
func ConvertClaudeRequestToOpenAI(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	return convertClaudeRequestToOpenAI(modelName, inputRawJSON, stream, false)

}

// ConvertClaudeRequestToOpenAIWithCompat preserves assistant thinking text
// for configured compatibility endpoints.
func ConvertClaudeRequestToOpenAIWithCompat(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	return convertClaudeRequestToOpenAI(modelName, inputRawJSON, stream, true)
}

func convertClaudeRequestToOpenAI(modelName string, inputRawJSON []byte, stream bool, preserveThinkingBlocks bool) ([]byte, error) {
	// The internal commentary marker must be restored as assistant text before
	// any thinking handling, so it is never dropped or mapped to reasoning.
	rawJSON, _ := sigcompat.RestoreCodexCommentaryMarkers(inputRawJSON)
	var drops translatorcommon.UserTurnDrops
	// Base OpenAI Chat Completions API template
	out := []byte(`{"model":"","messages":[]}`)

	root := ojson.ParseBytes(rawJSON)

	// Model mapping
	out, _ = ojson.SetBytes(out, "model", modelName)

	// Max tokens
	if maxTokens := root.Get("max_tokens"); maxTokens.Exists() {
		out, _ = ojson.SetBytes(out, "max_tokens", maxTokens.Int())
	}

	// Temperature
	if temp := root.Get("temperature"); temp.Exists() {
		out, _ = ojson.SetBytes(out, "temperature", temp.Float())
	} else if topP := root.Get("top_p"); topP.Exists() { // Top P
		out, _ = ojson.SetBytes(out, "top_p", topP.Float())
	}

	// Stop sequences -> stop
	if stopSequences := root.Get("stop_sequences"); stopSequences.Exists() {
		if stopSequences.IsArray() {
			var stops []string
			stopSequences.ForEach(func(_, value ojson.Result) bool {
				stops = append(stops, value.String())
				return true
			})
			if len(stops) > 0 {
				out, _ = ojson.SetBytes(out, "stop", stops)
			}
		}
	}

	// Stream
	out, _ = ojson.SetBytes(out, "stream", stream)

	// Thinking: Convert Claude thinking.budget_tokens and output_config.effort to OpenAI reasoning_effort
	effort := ""
	if v := root.Get("output_config.effort"); v.Exists() && v.Type == ojson.String {
		effort = strings.ToLower(strings.TrimSpace(v.String()))
	}
	if thinkingConfig := root.Get("thinking"); thinkingConfig.Exists() && thinkingConfig.IsObject() {
		if thinkingType := thinkingConfig.Get("type"); thinkingType.Exists() {
			switch thinkingType.String() {
			case "enabled":
				if budgetTokens := thinkingConfig.Get("budget_tokens"); budgetTokens.Exists() {
					budget := int(budgetTokens.Int())
					if mappedEffort, ok := thinking.ConvertBudgetToModelLevel(budget, modelName, "openai"); ok {
						out, _ = ojson.SetBytes(out, "reasoning_effort", mappedEffort)
					}
				} else if effort != "" {
					// Some Claude-compatible clients pair manual thinking with output_config.effort.
					// Preserve that explicit level when there is no legacy token budget to map.
					out, _ = ojson.SetBytes(out, "reasoning_effort", effort)
				} else {
					// No budget_tokens or explicit effort specified; preserve the enabled-thinking default.
					if mappedEffort, ok := thinking.ConvertBudgetToLevel(-1); ok && mappedEffort != "" {
						out, _ = ojson.SetBytes(out, "reasoning_effort", mappedEffort)
					}
				}
			case "adaptive", "auto":
				// Adaptive thinking can carry an explicit effort in output_config.effort (Claude 4.6).
				// Pass through directly; ApplyThinking handles clamping to target model's levels.
				if effort != "" {
					out, _ = ojson.SetBytes(out, "reasoning_effort", effort)
				} else {
					out, _ = ojson.SetBytes(out, "reasoning_effort", string(thinking.LevelXHigh))
				}
			case "disabled":
				if mappedEffort, ok := thinking.ConvertBudgetToLevel(0); ok && mappedEffort != "" {
					out, _ = ojson.SetBytes(out, "reasoning_effort", mappedEffort)
				}
			}
		}
	} else if !thinkingConfig.Exists() && effort != "" {
		out, _ = ojson.SetBytes(out, "reasoning_effort", effort)
	}

	reasoningActive := isClaudeRequestReasoningActive(ojson.GetBytes(out, "reasoning_effort"), modelName, root.Get("model").String())

	// Process messages and system.
	messageCapacity := root.Get("messages.#").Int()
	if root.Get("system").Exists() {
		messageCapacity++
	}
	messageItems := translatorcommon.NewRawArrayItems(messageCapacity)

	// Handle system message first.
	systemContentItems := make([][]byte, 0, 2)
	appendSystemContent := func(content ojson.Result) {
		if !content.Exists() {
			return
		}
		if content.Type == ojson.String {
			if content.String() == "" || util.IsClaudeCodeAttributionSystemText(content.String()) {
				return
			}
			oldSystem := []byte(`{"type":"text","text":""}`)
			oldSystem, _ = ojson.SetBytes(oldSystem, "text", content.String())
			systemContentItems = append(systemContentItems, oldSystem)
			return
		}
		if content.IsArray() {
			content.ForEach(func(_, item ojson.Result) bool {
				systemContentItems = append(systemContentItems, convertClaudeContentParts(item, nil)...)
				return true
			})
		}
	}

	if system := root.Get("system"); system.Exists() {
		appendSystemContent(system)
	}
	// Only add system message if it has content.
	if len(systemContentItems) > 0 {
		systemMessage := []byte(`{"role":"system","content":[]}`)
		systemMessage, _ = ojson.SetRawBytes(systemMessage, "content", translatorcommon.JoinRawArray(systemContentItems))
		messageItems = append(messageItems, systemMessage)
	}

	// Process Anthropic messages
	var midConversationToolDefs []ojson.Result
	if messages := root.Get("messages"); messages.Exists() && messages.IsArray() {
		var pendingToolUseIDs []string
		var pendingSystemReminders [][]byte
		toolNameByID := make(map[string]string)

		messages.ForEach(func(_, message ojson.Result) bool {
			role := message.Get("role").String()
			contentResult := message.Get("content")
			if role == "system" {
				if reminderText, ok := translatorcommon.ClaudeMessageSystemReminderText(contentResult); ok {
					msgJSON := []byte(`{"role":"user","content":[{"type":"text","text":""}]}`)
					msgJSON, _ = ojson.SetBytes(msgJSON, "content.0.text", reminderText)
					if len(pendingToolUseIDs) > 0 {
						pendingSystemReminders = append(pendingSystemReminders, msgJSON)
					} else {
						messageItems = append(messageItems, msgJSON)
					}
				}
				if contentResult.IsArray() {
					contentResult.ForEach(func(_, part ojson.Result) bool {
						if part.Get("type").String() != "tool_addition" {
							return true
						}
						toolSpec := part.Get("tool")
						if toolSpec.Get("type").String() == "tool_definition" || toolSpec.Get("definition").IsObject() {
							if def := toolSpec.Get("definition"); def.IsObject() && def.Get("name").String() != "" {
								midConversationToolDefs = append(midConversationToolDefs, def)
							}
						}
						return true
					})
				}
				return true
			}

			// Handle content
			if contentResult.Exists() && contentResult.IsArray() {
				if role == "user" && len(pendingToolUseIDs) > 0 {
					contentResult = translatorcommon.AlignClaudeToolResults(contentResult, pendingToolUseIDs)
				}
				pendingToolUseIDs = nil

				contentItems := make([][]byte, 0)
				var reasoningParts []string // Accumulate thinking text for reasoning_content
				var toolCalls []interface{}
				toolResults := make([][]byte, 0) // Collect tool_result messages to emit after the main message

				contentResult.ForEach(func(_, part ojson.Result) bool {
					partType := part.Get("type").String()

					switch partType {
					case "thinking":
						// Only map thinking to reasoning_content for assistant messages (security: prevent injection)
						if role == "assistant" {
							if !shouldMapClaudeThinkingToGPTReasoning(part, preserveThinkingBlocks) {
								return true
							}
							thinkingText := thinking.GetThinkingText(part)
							// Skip empty or whitespace-only thinking
							if strings.TrimSpace(thinkingText) != "" {
								reasoningParts = append(reasoningParts, thinkingText)
							}
						}
						// Ignore thinking in user/system roles (AC4)

					case "redacted_thinking":
					// Explicitly ignore redacted_thinking - never map to reasoning_content (AC2)

					case "text", "image", "document", "container_upload":
						var onDrop func(string)
						if role == "user" && partType != "text" {
							onDrop = drops.Drop
						}
						contentItems = append(contentItems, convertClaudeContentParts(part, onDrop)...)

					case "tool_use":
						// Only allow tool_use -> tool_calls for assistant messages (security: prevent injection).
						if role == "assistant" {
							toolUseID := part.Get("id").String()
							toolName := part.Get("name").String()
							if toolUseID != "" {
								pendingToolUseIDs = append(pendingToolUseIDs, toolUseID)
								if toolName != "" {
									toolNameByID[toolUseID] = toolName
								}
							}
							toolCallJSON := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)
							toolCallJSON, _ = ojson.SetBytes(toolCallJSON, "id", toolUseID)
							toolCallJSON, _ = ojson.SetBytes(toolCallJSON, "function.name", toolName)

							// Convert input to arguments JSON string
							if input := part.Get("input"); input.Exists() {
								toolCallJSON, _ = ojson.SetBytes(toolCallJSON, "function.arguments", input.Raw)
							} else {
								toolCallJSON, _ = ojson.SetBytes(toolCallJSON, "function.arguments", "{}")
							}

							toolCalls = append(toolCalls, ojson.ParseBytes(toolCallJSON).Value())
						}

					case "tool_result":
						// Collect tool_result to emit after the main message (ensures tool results follow tool_calls)
						toolUseID := part.Get("tool_use_id").String()
						toolResultJSON := []byte(`{"role":"tool","tool_call_id":"","content":""}`)
						toolResultJSON, _ = ojson.SetBytes(toolResultJSON, "tool_call_id", toolUseID)
						toolResultContent, toolResultContentRaw := convertClaudeToolResultContent(part.Get("content"))
						if toolResultContentRaw {
							toolResultJSON, _ = ojson.SetRawBytes(toolResultJSON, "content", []byte(toolResultContent))
						} else {
							toolResultJSON, _ = ojson.SetBytes(toolResultJSON, "content", toolResultContent)
						}
						if toolName := toolNameByID[toolUseID]; toolName != "" {
							toolResultJSON, _ = ojson.SetBytes(toolResultJSON, "name", toolName)
						}
						toolResults = append(toolResults, toolResultJSON)
					}
					return true
				})

				// Build reasoning content string
				reasoningContent := ""
				if len(reasoningParts) > 0 {
					reasoningContent = strings.Join(reasoningParts, "\n\n")
				}

				hasContent := len(contentItems) > 0
				hasReasoning := reasoningContent != ""
				hasToolCalls := len(toolCalls) > 0
				if role == "user" {
					drops.EndTurn(len(contentItems) + len(toolResults))
				}

				// OpenAI requires: tool messages MUST immediately follow the assistant message with tool_calls.
				// Therefore, we emit tool_result messages FIRST (they respond to the previous assistant's tool_calls),
				// then emit any queued system reminders, then emit the current message's content.
				messageItems = append(messageItems, toolResults...)

				if len(pendingSystemReminders) > 0 {
					messageItems = append(messageItems, pendingSystemReminders...)
					pendingSystemReminders = nil
				}

				// For assistant messages: emit a single unified message with content, tool_calls, and reasoning_content
				// This avoids splitting into multiple assistant messages which breaks OpenAI tool-call adjacency
				if role == "assistant" {
					if hasContent || hasReasoning || hasToolCalls {
						msgJSON := []byte(`{"role":"assistant"}`)

						// Add content (as array if we have items, empty string if reasoning-only)
						if hasContent {
							msgJSON, _ = ojson.SetRawBytes(msgJSON, "content", translatorcommon.JoinRawArray(contentItems))
						} else {
							// Ensure content field exists for OpenAI compatibility
							msgJSON, _ = ojson.SetBytes(msgJSON, "content", "")
						}

						// Add reasoning_content if present
						if hasReasoning {
							msgJSON, _ = ojson.SetBytes(msgJSON, "reasoning_content", reasoningContent)
						} else if hasToolCalls && preserveThinkingBlocks && reasoningActive {
							// Assistant turn with tool_calls but no replayable thinking text.
							// Compat thinking upstreams (DeepSeek, Moonshot/Kimi) require
							// reasoning_content to be present on such messages when reasoning is active.
							msgJSON, _ = ojson.SetBytes(msgJSON, "reasoning_content", " ")
						}

						// Add tool_calls if present (in same message as content)
						if hasToolCalls {
							msgJSON, _ = ojson.SetBytes(msgJSON, "tool_calls", toolCalls)
						}

						messageItems = append(messageItems, msgJSON)
					}
				} else {
					// For non-assistant roles: emit content message if we have content
					// If the message only contains tool_results (no text/image), we still processed them above
					if hasContent {
						msgJSON := []byte(`{"role":""}`)
						msgJSON, _ = ojson.SetBytes(msgJSON, "role", role)

						msgJSON, _ = ojson.SetRawBytes(msgJSON, "content", translatorcommon.JoinRawArray(contentItems))
						messageItems = append(messageItems, msgJSON)
					}
				}

			} else if contentResult.Exists() && contentResult.Type == ojson.String {
				// Simple string content
				msgJSON := []byte(`{"role":"","content":""}`)
				msgJSON, _ = ojson.SetBytes(msgJSON, "role", role)
				msgJSON, _ = ojson.SetBytes(msgJSON, "content", contentResult.String())
				messageItems = append(messageItems, msgJSON)
			}

			return true
		})
		if len(pendingSystemReminders) > 0 {
			messageItems = append(messageItems, pendingSystemReminders...)
		}
	}

	// Set messages.
	if len(messageItems) > 0 {
		messageItems = translatorcommon.AlignOpenAIToolCallMessages(messageItems)
		out = translatorcommon.SetRawArrayItems(out, "messages", messageItems)
	}

	// Process tools - convert Anthropic tools (and mid-conversation tool_addition definitions) to OpenAI functions
	var toolItems [][]byte
	var toolIndexByName map[string]int
	if len(midConversationToolDefs) > 0 {
		toolIndexByName = make(map[string]int)
	}
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		tools.ForEach(func(_, tool ojson.Result) bool {
			openAIToolJSON := convertClaudeToolToOpenAI(tool)
			if toolIndexByName != nil {
				if name := tool.Get("name").String(); name != "" {
					toolIndexByName[name] = len(toolItems)
				}
			}
			toolItems = append(toolItems, openAIToolJSON)
			return true
		})
	}
	for _, def := range midConversationToolDefs {
		name := def.Get("name").String()
		converted := convertClaudeToolToOpenAI(def)
		if idx, exists := toolIndexByName[name]; exists {
			toolItems[idx] = converted
		} else {
			toolIndexByName[name] = len(toolItems)
			toolItems = append(toolItems, converted)
		}
	}
	if len(toolItems) > 0 {
		out, _ = ojson.SetRawBytes(out, "tools", translatorcommon.JoinRawArray(toolItems))
	}

	// Tool choice mapping - convert Anthropic tool_choice to OpenAI format
	if toolChoice := root.Get("tool_choice"); toolChoice.Exists() && toolChoice.Type != ojson.Null {
		choiceType := toolChoice.Get("type").String()
		if choiceType == "" && toolChoice.Type == ojson.String {
			choiceType = toolChoice.String()
		}
		switch choiceType {
		case "auto":
			out, _ = ojson.SetBytes(out, "tool_choice", "auto")
		case "any":
			out, _ = ojson.SetBytes(out, "tool_choice", "required")
		case "none":
			out, _ = ojson.SetBytes(out, "tool_choice", "none")
		case "tool":
			// Specific tool choice
			toolName := toolChoice.Get("name").String()
			if toolName != "" {
				toolChoiceJSON := []byte(`{"type":"function","function":{"name":""}}`)
				toolChoiceJSON, _ = ojson.SetBytes(toolChoiceJSON, "function.name", toolName)
				out, _ = ojson.SetRawBytes(out, "tool_choice", toolChoiceJSON)
			} else {
				out, _ = ojson.SetBytes(out, "tool_choice", "none")
			}
		default:
			// Fail closed: unrecognized tool_choice values must not turn into permission
			out, _ = ojson.SetBytes(out, "tool_choice", "none")
		}

		if disableParallel := toolChoice.Get("disable_parallel_tool_use"); disableParallel.Type == ojson.True {
			out, _ = ojson.SetBytes(out, "parallel_tool_calls", false)
		}
	}

	// Handle user parameter (for tracking)
	if user := root.Get("user"); user.Exists() {
		out, _ = ojson.SetBytes(out, "user", user.String())
	}

	return out, drops.Err()
}

func convertClaudeToolToOpenAI(tool ojson.Result) []byte {
	openAIToolJSON := []byte(`{"type":"function","function":{"name":"","description":""}}`)
	openAIToolJSON, _ = ojson.SetBytes(openAIToolJSON, "function.name", tool.Get("name").String())
	openAIToolJSON, _ = ojson.SetBytes(openAIToolJSON, "function.description", tool.Get("description").String())

	// Convert Anthropic input_schema to OpenAI function parameters
	if inputSchema := tool.Get("input_schema"); inputSchema.Exists() && inputSchema.Type != ojson.Null {
		openAIToolJSON, _ = ojson.SetBytes(openAIToolJSON, "function.parameters", normalizeObjectSchemaProperties(inputSchema.Value()))
	} else {
		openAIToolJSON, _ = ojson.SetRawBytes(openAIToolJSON, "function.parameters", []byte(`{"type":"object","properties":{}}`))
	}
	return openAIToolJSON
}

func normalizeObjectSchemaProperties(schema any) any {
	switch value := schema.(type) {
	case bool:
		// JSON Schema boolean subschemas (true/false) are valid, but strict OpenAPI 3.0
		// upstream validators reject boolean subschemas.
		// Normalize `true` (accept anything) to an empty object schema `{}`.
		// Preserve `false` (reject all) to avoid turning rejection constraints into open schemas.
		if value {
			return map[string]any{}
		}
		return value
	case map[string]any:
		if schemaType, ok := value["type"].(string); ok && schemaType == "object" {
			if _, ok := value["properties"]; !ok {
				value["properties"] = map[string]any{}
			}
		}
		if patternVal, ok := value["pattern"].(string); ok && util.HasUnsupportedUnicodePropertyEscape(patternVal) {
			delete(value, "pattern")
		}

		// Inspect regex keys under patternProperties
		if patternProps, ok := value["patternProperties"].(map[string]any); ok {
			for patternKey, subSchema := range patternProps {
				if util.HasUnsupportedUnicodePropertyEscape(patternKey) {
					delete(patternProps, patternKey)
				} else {
					patternProps[patternKey] = normalizeObjectSchemaProperties(subSchema)
				}
			}
		}

		for _, mapKey := range util.SchemaMapKeywords {
			if mapKey == "patternProperties" {
				continue
			}
			if subMap, ok := value[mapKey].(map[string]any); ok {
				for subKey, subSchema := range subMap {
					subMap[subKey] = normalizeObjectSchemaProperties(subSchema)
				}
			}
		}

		for _, valKey := range util.SchemaValueKeywords {
			if val, exists := value[valKey]; exists {
				switch sub := val.(type) {
				case bool:
					// Normalize boolean subschemas (e.g. items: true), but preserve boolean
					// additionalProperties (false/true) required by OpenAI structured outputs.
					if valKey != "additionalProperties" && sub {
						value[valKey] = map[string]any{}
					}
				case map[string]any:
					value[valKey] = normalizeObjectSchemaProperties(sub)
				case []any:
					for i, item := range sub {
						sub[i] = normalizeObjectSchemaProperties(item)
					}
				}
			}
		}
		return value
	case []any:
		for i, child := range value {
			value[i] = normalizeObjectSchemaProperties(child)
		}
		return value
	default:
		return schema
	}
}

func isClaudeRequestReasoningActive(effortRes ojson.Result, modelName, payloadModel string) bool {
	if active, ok := isThinkingSuffixActive(modelName); ok {
		return active
	}
	if active, ok := isThinkingSuffixActive(payloadModel); ok {
		return active
	}
	if !effortRes.Exists() {
		return false
	}
	resolvedEffort := strings.ToLower(strings.TrimSpace(effortRes.String()))
	return resolvedEffort != "" && resolvedEffort != string(thinking.LevelNone) && resolvedEffort != "0" && resolvedEffort != "false"
}

func isThinkingSuffixActive(model string) (bool, bool) {
	suffix := thinking.ParseSuffix(strings.TrimSpace(model))
	if !suffix.HasSuffix {
		return false, false
	}
	if mode, ok := thinking.ParseSpecialSuffix(suffix.RawSuffix); ok {
		return mode != thinking.ModeNone, true
	}
	if budget, ok := thinking.ParseNumericSuffix(suffix.RawSuffix); ok {
		return budget > 0, true
	}
	if level, ok := thinking.ParseLevelSuffix(suffix.RawSuffix); ok {
		return level != "" && level != thinking.LevelNone, true
	}
	return false, false
}

func shouldMapClaudeThinkingToGPTReasoning(part ojson.Result, preserveThinkingBlocks ...bool) bool {
	preserveThinking := len(preserveThinkingBlocks) > 0 && preserveThinkingBlocks[0]
	if preserveThinking {
		return true
	}

	signature := part.Get("signature")
	if !signature.Exists() || strings.TrimSpace(signature.String()) == "" {
		return true
	}
	_, ok := sigcompat.CompatibleSignatureForProvider(sigcompat.SignatureProviderGPT, signature.String())
	return ok
}

func convertClaudeContentPart(part ojson.Result) (string, bool) {
	partType := part.Get("type").String()

	switch partType {
	case "text":
		text := part.Get("text").String()
		if strings.TrimSpace(text) == "" || util.IsClaudeCodeAttributionSystemText(text) {
			return "", false
		}
		textContent := []byte(`{"type":"text","text":""}`)
		textContent, _ = ojson.SetBytes(textContent, "text", text)
		return string(textContent), true

	case "image":
		var imageURL string

		if source := part.Get("source"); source.Exists() {
			sourceType := source.Get("type").String()
			switch sourceType {
			case "base64":
				mediaType := source.Get("media_type").String()
				if mediaType == "" {
					mediaType = "application/octet-stream"
				}
				data := source.Get("data").String()
				if data != "" {
					imageURL = "data:" + mediaType + ";base64," + data
				}
			case "url":
				imageURL = source.Get("url").String()
			}
		}

		if imageURL == "" {
			imageURL = part.Get("url").String()
		}

		if imageURL == "" {
			return "", false
		}

		imageContent := []byte(`{"type":"image_url","image_url":{"url":""}}`)
		imageContent, _ = ojson.SetBytes(imageContent, "image_url.url", imageURL)

		return string(imageContent), true

	case "document", "container_upload":
		return convertClaudeFilePartToOpenAI(part)

	default:
		return "", false
	}
}

// convertClaudeContentParts converts a Claude content block into OpenAI parts.
// Text-source and content-source documents expand into text and image parts.
// onDrop, when set, reports the block (or a nested image) that cannot be sent.
func convertClaudeContentParts(part ojson.Result, onDrop func(string)) [][]byte {
	if part.Get("type").String() == "document" {
		if docText, ok := translatorcommon.ClaudeDocumentTextBlock(part); ok {
			return [][]byte{openAITextPart(docText)}
		}
		var items [][]byte
		var onImageDrop func()
		if onDrop != nil {
			onImageDrop = func() { onDrop("image") }
		}
		if translatorcommon.ExpandClaudeContentDocument(
			part,
			func(text string) { items = append(items, openAITextPart(text)) },
			func(img ojson.Result) ([]byte, bool) {
				contentItem, ok := convertClaudeContentPart(img)
				return []byte(contentItem), ok
			},
			func(img []byte) { items = append(items, img) },
			onImageDrop,
		) {
			return items
		}
	}
	if contentItem, ok := convertClaudeContentPart(part); ok {
		return [][]byte{[]byte(contentItem)}
	}
	if onDrop != nil {
		onDrop(part.Get("type").String())
	}
	return nil
}

func openAITextPart(text string) []byte {
	textContent := []byte(`{"type":"text","text":""}`)
	textContent, _ = ojson.SetBytes(textContent, "text", text)
	return textContent
}

// toolResultUnsupportedDocumentPlaceholder avoids dumping raw base64 JSON into
// tool text when a document block cannot be converted into an OpenAI part.
const toolResultUnsupportedDocumentPlaceholder = "[Unsupported document content]"

// convertClaudeFilePartToOpenAI emits an OpenAI file part. Inline base64 bytes
// become a data URL; ccLoad also forwards a url as file_url and a file id as
// file_id. Anything else stays unconverted and the caller reports it.
func convertClaudeFilePartToOpenAI(part ojson.Result) (string, bool) {
	source := part.Get("source")
	fileContent := []byte(`{"type":"file","file":{}}`)
	switch source.Get("type").String() {
	case "base64":
		data, errDecode := base64.StdEncoding.DecodeString(strings.TrimSpace(source.Get("data").String()))
		if errDecode != nil || len(data) == 0 {
			return "", false
		}
		mimeType := source.Get("media_type").String()
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		fileContent, _ = ojson.SetBytes(fileContent, "file.file_data", "data:"+mimeType+";base64,"+base64.StdEncoding.EncodeToString(data))
	case "url":
		url := source.Get("url").String()
		if url == "" {
			return "", false
		}
		fileContent, _ = ojson.SetBytes(fileContent, "file.file_url", url)
	case "file":
		fileID := source.Get("file_id").String()
		if fileID == "" {
			return "", false
		}
		fileContent, _ = ojson.SetBytes(fileContent, "file.file_id", fileID)
	default:
		return "", false
	}
	filename := part.Get("title").String()
	if filename == "" {
		filename = part.Get("filename").String()
	}
	if filename != "" {
		fileContent, _ = ojson.SetBytes(fileContent, "file.filename", filename)
	}
	return string(fileContent), true
}

func convertClaudeToolResultContent(content ojson.Result) (string, bool) {
	if !content.Exists() {
		return "", false
	}

	if content.Type == ojson.String {
		return content.String(), false
	}

	if content.IsArray() {
		var parts []string
		contentItems := make([][]byte, 0, 4)
		hasStructuredPart := false
		content.ForEach(func(_, item ojson.Result) bool {
			switch {
			case item.Type == ojson.String:
				text := item.String()
				parts = append(parts, text)
				contentItems = append(contentItems, openAITextPart(text))
			case item.IsObject() && item.Get("type").String() == "text":
				text := item.Get("text").String()
				parts = append(parts, text)
				contentItems = append(contentItems, openAITextPart(text))
			case item.IsObject() && item.Get("type").String() == "document":
				docItems := convertClaudeContentParts(item, nil)
				if len(docItems) == 0 {
					docItems = [][]byte{openAITextPart(toolResultUnsupportedDocumentPlaceholder)}
				}
				for _, docItem := range docItems {
					if ojson.GetBytes(docItem, "type").String() == "text" {
						parts = append(parts, ojson.GetBytes(docItem, "text").String())
					} else {
						hasStructuredPart = true
					}
				}
				contentItems = append(contentItems, docItems...)
			case item.IsObject() && item.Get("type").String() == "image":
				contentItem, ok := convertClaudeContentPart(item)
				if ok {
					contentItems = append(contentItems, []byte(contentItem))
					hasStructuredPart = true
				} else {
					parts = append(parts, item.Raw)
				}
			case item.IsObject() && item.Get("text").Exists() && item.Get("text").Type == ojson.String:
				parts = append(parts, item.Get("text").String())
			default:
				parts = append(parts, item.Raw)
			}
			return true
		})

		if hasStructuredPart {
			return string(translatorcommon.JoinRawArray(contentItems)), true
		}

		joined := strings.Join(parts, "\n\n")
		if strings.TrimSpace(joined) != "" {
			return joined, false
		}
		return content.Raw, false
	}

	if content.IsObject() {
		if blockType := content.Get("type").String(); blockType == "image" || blockType == "document" {
			return convertClaudeToolResultContent(ojson.Parse("[" + content.Raw + "]"))
		}
		if text := content.Get("text"); text.Exists() && text.Type == ojson.String {
			return text.String(), false
		}
		return content.Raw, false
	}

	return content.Raw, false
}
