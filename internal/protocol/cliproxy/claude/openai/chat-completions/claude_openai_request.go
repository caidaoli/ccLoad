// Package openai provides request translation functionality for OpenAI to Claude Code API compatibility.
// It handles parsing and transforming OpenAI Chat Completions API requests into Claude Code API format,
// extracting model information, system instructions, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between OpenAI API format and Claude Code API's expected format.
package chat_completions

import (
	"strings"

	"ccLoad/internal/protocol/cliproxy/common"
	"ccLoad/internal/protocol/cliproxy/registry"
	"ccLoad/internal/protocol/cliproxy/thinking"
	"ccLoad/internal/protocol/cliproxy/util"

	"github.com/MoeForge/moego/ojson"
)

const (
	defaultClaudeChatMaxTokens = 32000
	defaultFableChatMaxTokens  = 64000
)

// ConvertOpenAIRequestToClaude parses and transforms an OpenAI Chat Completions API request into Claude Code API format.
// It extracts the model name, system instruction, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the Claude Code API.
// The function performs comprehensive transformation including:
// 1. Model name mapping and parameter extraction (max_tokens, top_p, etc.)
// 2. Message content conversion from OpenAI to Claude Code format
// 3. Tool call and tool result handling with proper ID mapping
// 4. Image data conversion from OpenAI data URLs to Claude Code base64 format
// 5. Stop sequence and streaming configuration handling
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data from the OpenAI API
//   - stream: A boolean indicating if the request is for a streaming response
//
// Returns:
//   - []byte: The transformed request data in Claude Code API format
func ConvertOpenAIRequestToClaude(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	return convertOpenAIRequestToClaude(modelName, inputRawJSON, stream, false)

}

// ConvertOpenAIRequestToClaudeWithCompat preserves assistant reasoning content
// as an unsigned thinking block for configured compatibility endpoints.
func ConvertOpenAIRequestToClaudeWithCompat(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	return convertOpenAIRequestToClaude(modelName, inputRawJSON, stream, true)
}

func convertOpenAIRequestToClaude(modelName string, inputRawJSON []byte, stream, preserveEmptyThinkingBlocks bool) ([]byte, error) {
	rawJSON := inputRawJSON
	var drops common.UserTurnDrops

	userID := common.DeriveClaudeUserID(rawJSON)

	// Base Claude Code API template with default max_tokens value
	out := []byte(`{"model":"","max_tokens":32000,"messages":[],"metadata":{}}`)
	out, _ = ojson.SetBytes(out, "metadata.user_id", userID)
	out, _ = ojson.SetBytes(out, "max_tokens", defaultClaudeChatMaxTokensForModel(modelName))

	root := ojson.ParseBytes(rawJSON)

	// Convert OpenAI reasoning_effort to Claude thinking config.
	if v := root.Get("reasoning_effort"); v.Exists() {
		effort := strings.ToLower(strings.TrimSpace(v.String()))
		if effort != "" {
			mi := registry.LookupModelInfo(modelName, "claude")
			supportsAdaptive := mi != nil && mi.Thinking != nil && len(mi.Thinking.Levels) > 0
			supportsMax := supportsAdaptive && thinking.HasLevel(mi.Thinking.Levels, string(thinking.LevelMax))

			// Claude 4.6 supports adaptive thinking with output_config.effort.
			// MapToClaudeEffort normalizes levels (e.g. minimal→low, xhigh→high) to avoid
			// validation errors since validate treats same-provider unsupported levels as errors.
			if supportsAdaptive {
				switch effort {
				case "none":
					out, _ = ojson.SetBytes(out, "thinking.type", "disabled")
					out, _ = ojson.DeleteBytes(out, "thinking.budget_tokens")
					out, _ = ojson.DeleteBytes(out, "output_config.effort")
				case "auto":
					out, _ = ojson.SetBytes(out, "thinking.type", "adaptive")
					out, _ = ojson.DeleteBytes(out, "thinking.budget_tokens")
					out, _ = ojson.DeleteBytes(out, "output_config.effort")
				default:
					if !thinking.HasLevel(mi.Thinking.Levels, effort) {
						if mapped, ok := thinking.MapToClaudeEffort(effort, supportsMax); ok {
							effort = mapped
						}
					}
					out, _ = ojson.SetBytes(out, "thinking.type", "adaptive")
					out, _ = ojson.DeleteBytes(out, "thinking.budget_tokens")
					out, _ = ojson.SetBytes(out, "output_config.effort", effort)
				}
			} else {
				// Legacy/manual thinking (budget_tokens).
				budget, ok := thinking.ConvertLevelToBudget(effort)
				if ok {
					switch budget {
					case 0:
						out, _ = ojson.SetBytes(out, "thinking.type", "disabled")
					case -1:
						out, _ = ojson.SetBytes(out, "thinking.type", "enabled")
					default:
						if budget > 0 {
							out, _ = ojson.SetBytes(out, "thinking.type", "enabled")
							out, _ = ojson.SetBytes(out, "thinking.budget_tokens", budget)
						}
					}
				}
			}
		}
	}

	// Model mapping to specify which Claude Code model to use
	out, _ = ojson.SetBytes(out, "model", modelName)

	// Max tokens configuration with fallback to default value.
	// OpenAI Chat Completions deprecated max_tokens in favor of
	// max_completion_tokens, so accept either spelling.
	if maxTokens := root.Get("max_tokens"); maxTokens.Type == ojson.Number && maxTokens.Int() > 0 {
		out, _ = ojson.SetBytes(out, "max_tokens", maxTokens.Int())
	} else if maxCompletion := root.Get("max_completion_tokens"); maxCompletion.Type == ojson.Number && maxCompletion.Int() > 0 {
		out, _ = ojson.SetBytes(out, "max_tokens", maxCompletion.Int())
	}

	// Top P setting for nucleus sampling.
	if topP := root.Get("top_p"); topP.Type == ojson.Number {
		out, _ = ojson.SetBytes(out, "top_p", topP.Float())
	}

	// Stop sequences configuration for custom termination conditions
	if stop := root.Get("stop"); stop.Exists() && stop.Type != ojson.Null {
		if stop.IsArray() {
			var stopSequences []string
			stop.ForEach(func(_, value ojson.Result) bool {
				if value.Type == ojson.String && value.String() != "" {
					stopSequences = append(stopSequences, value.String())
				}
				return true
			})
			if len(stopSequences) > 0 {
				out, _ = ojson.SetBytes(out, "stop_sequences", stopSequences)
			}
		} else if stop.Type == ojson.String && stop.String() != "" {
			out, _ = ojson.SetBytes(out, "stop_sequences", []string{stop.String()})
		}
	}

	// Stream configuration to enable or disable streaming responses
	out, _ = ojson.SetBytes(out, "stream", stream)

	systemBlocks := make([][]byte, 0)
	messageBlocks := make([][]byte, 0)

	// Process messages and transform them to Claude Code format
	if messages := root.Get("messages"); messages.Exists() && messages.IsArray() {
		lastToolMessage := map[string]ojson.Result{}
		messages.ForEach(func(_, message ojson.Result) bool {
			if message.Get("role").String() == "tool" {
				rawID := message.Get("tool_call_id").String()
				if rawID != "" {
					lastToolMessage[rawID] = message
				}
			}
			return true
		})
		emittedToolResults := map[string]struct{}{}

		messageAccumulator := common.NewClaudeMessageAccumulator(int(root.Get("messages.#").Int()))
		messages.ForEach(func(_, message ojson.Result) bool {
			role := message.Get("role").String()
			contentResult := message.Get("content")

			switch role {
			// Developer messages rank with system messages in OpenAI's instruction
			// hierarchy, so both become top-level Claude system blocks. Dropping the
			// developer role, as this translator used to, silently removed operator
			// instructions from the upstream request.
			case "system", "developer":
				systemStart := len(systemBlocks)
				if contentResult.Exists() && contentResult.Type == ojson.String && strings.TrimSpace(contentResult.String()) != "" {
					textPart := []byte(`{"type":"text","text":""}`)
					textPart, _ = ojson.SetBytes(textPart, "text", contentResult.String())
					textPart = common.AttachCacheControl(textPart, message)
					systemBlocks = append(systemBlocks, textPart)
				} else if contentResult.Exists() && contentResult.IsArray() {
					contentResult.ForEach(func(_, part ojson.Result) bool {
						if part.Get("type").String() == "text" {
							txt := part.Get("text").String()
							if strings.TrimSpace(txt) == "" {
								if part.Get("cache_control").Exists() && len(systemBlocks) > systemStart {
									prevIdx := len(systemBlocks) - 1
									if !ojson.GetBytes(systemBlocks[prevIdx], "cache_control").Exists() {
										systemBlocks[prevIdx] = common.AttachCacheControl(systemBlocks[prevIdx], part)
									}
								}
								return true
							}
							textPart := []byte(`{"type":"text","text":""}`)
							textPart, _ = ojson.SetBytes(textPart, "text", txt)
							textPart = common.AttachCacheControl(textPart, part)
							systemBlocks = append(systemBlocks, textPart)
						}
						return true
					})
					// Message-level cache_control applies to the last system block from this message.
					if message.Get("cache_control").Exists() && len(systemBlocks) > systemStart {
						lastIdx := len(systemBlocks) - 1
						if !ojson.GetBytes(systemBlocks[lastIdx], "cache_control").Exists() {
							systemBlocks[lastIdx] = common.AttachCacheControl(systemBlocks[lastIdx], message)
						}
					}
				}
			case "user", "assistant":
				contentBlocks := make([][]byte, 0, 4)
				if preserveEmptyThinkingBlocks && role == "assistant" {
					if reasoningContent := message.Get("reasoning_content"); reasoningContent.Type == ojson.String && strings.TrimSpace(reasoningContent.String()) != "" {
						part := []byte(`{"type":"thinking","thinking":"","signature":""}`)
						part, _ = ojson.SetBytes(part, "thinking", reasoningContent.String())
						contentBlocks = append(contentBlocks, part)
					}
				}

				// Handle content based on its type
				if contentResult.Exists() && contentResult.Type == ojson.String && strings.TrimSpace(contentResult.String()) != "" {
					part := []byte(`{"type":"text","text":""}`)
					part, _ = ojson.SetBytes(part, "text", contentResult.String())
					contentBlocks = append(contentBlocks, part)
				} else if contentResult.Exists() && contentResult.IsArray() {
					contentResult.ForEach(func(_, part ojson.Result) bool {
						if claudePart := convertOpenAIContentPartToClaudePart(part); claudePart != "" {
							contentBlocks = append(contentBlocks, []byte(claudePart))
						} else if partType := part.Get("type").String(); partType == "text" {
							if part.Get("cache_control").Exists() && len(contentBlocks) > 0 {
								prevIdx := len(contentBlocks) - 1
								if !ojson.GetBytes(contentBlocks[prevIdx], "cache_control").Exists() {
									contentBlocks[prevIdx] = common.AttachCacheControl(contentBlocks[prevIdx], part)
								}
							}
						} else if role == "user" && (partType == "file" || partType == "input_audio") {
							drops.Drop(partType)
						}
						return true
					})
				}

				if role == "user" {
					drops.EndTurn(len(contentBlocks))
				}

				// Handle tool calls (for assistant messages)
				if toolCalls := message.Get("tool_calls"); toolCalls.Exists() && toolCalls.IsArray() && role == "assistant" {
					toolCalls.ForEach(func(_, toolCall ojson.Result) bool {
						if toolCall.Get("type").String() == "function" {
							toolCallID := toolCall.Get("id").String()
							if toolCallID == "" {
								toolCallID = common.GenerateClaudeToolCallID()
							}
							toolCallID = util.SanitizeClaudeToolID(toolCallID)

							function := toolCall.Get("function")
							toolUse := []byte(`{"type":"tool_use","id":"","name":"","input":{}}`)
							toolUse, _ = ojson.SetBytes(toolUse, "id", toolCallID)
							toolUse, _ = ojson.SetBytes(toolUse, "name", util.SanitizeClaudeFunctionName(function.Get("name").String()))

							// Parse arguments for the tool call
							if args := function.Get("arguments"); args.Exists() {
								argsStr := args.String()
								if argsStr != "" && ojson.Valid(argsStr) {
									argsJSON := ojson.Parse(argsStr)
									if argsJSON.IsObject() {
										toolUse, _ = ojson.SetRawBytes(toolUse, "input", []byte(argsJSON.Raw))
									} else {
										toolUse, _ = ojson.SetRawBytes(toolUse, "input", []byte("{}"))
									}
								} else {
									toolUse, _ = ojson.SetRawBytes(toolUse, "input", []byte("{}"))
								}
							} else {
								toolUse, _ = ojson.SetRawBytes(toolUse, "input", []byte("{}"))
							}

							contentBlocks = append(contentBlocks, toolUse)
						}
						return true
					})
				}

				msg := []byte(`{"role":"","content":[]}`)
				msg, _ = ojson.SetBytes(msg, "role", role)
				msg, _ = ojson.SetRawBytes(msg, "content", common.JoinRawArray(contentBlocks))
				msg = common.AttachMessageCacheControl(msg, message)
				messageAccumulator.Append(msg)

			case "tool":
				// Handle tool result messages conversion
				rawID := message.Get("tool_call_id").String()
				toolCallID := util.SanitizeClaudeToolID(rawID)
				if rawID != "" {
					if _, exists := emittedToolResults[rawID]; exists {
						return true
					}
					emittedToolResults[rawID] = struct{}{}
				}

				targetMsg := message
				if rawID != "" {
					if lastMsg, exists := lastToolMessage[rawID]; exists {
						targetMsg = lastMsg
					}
				}
				toolContentResult := targetMsg.Get("content")

				msg := []byte(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"","content":""}]}`)
				msg, _ = ojson.SetBytes(msg, "content.0.tool_use_id", toolCallID)
				toolResultContent, toolResultContentRaw := convertOpenAIToolResultContent(toolContentResult)
				if toolResultContentRaw {
					msg, _ = ojson.SetRawBytes(msg, "content.0.content", []byte(toolResultContent))
				} else {
					msg, _ = ojson.SetBytes(msg, "content.0.content", toolResultContent)
				}
				// Anthropic rejects cache_control inside tool_result.content, so
				// part-level or message-level cache_control is hoisted onto the
				// tool_result block itself.
				msg = common.AttachToolMessageCacheControl(msg, targetMsg)
				messageAccumulator.Append(msg)
			}
			return true
		})

		messageBlocks = messageAccumulator.Messages()
	}

	responseFormat := root.Get("response_format")
	formatInstruction := common.BuildClaudeStructuredOutputInstruction(responseFormat)
	if schema, ok := common.ClaudeOutputFormatSchema(modelName, responseFormat); ok {
		out, _ = ojson.SetBytes(out, "output_config.format.type", "json_schema")
		out, _ = ojson.SetRawBytes(out, "output_config.format.schema", []byte(schema.Raw))
		formatInstruction = common.BuildClaudeOutputFormatDescription(responseFormat)
	}
	if formatInstruction != "" {
		systemBlock := []byte(`{"type":"text","text":""}`)
		systemBlock, _ = ojson.SetBytes(systemBlock, "text", formatInstruction)
		systemBlocks = append(systemBlocks, systemBlock)
	}

	// Decided per user turn, so neither a system prompt nor the blank turn added below hides an emptied turn.
	nothingLeftErr := drops.Err()

	// Preserve a minimal conversational turn for system-only inputs.
	// Claude payloads with top-level system instructions but no messages are risky for downstream validation.
	if len(messageBlocks) == 0 && len(systemBlocks) > 0 {
		fallbackMsg := []byte(`{"role":"user","content":[]}`)
		fallbackMsg, _ = ojson.SetRawBytes(fallbackMsg, "content", common.JoinRawArray(systemBlocks))
		messageBlocks = append(messageBlocks, fallbackMsg)
		systemBlocks = nil
	}

	if len(systemBlocks) > 0 {
		out, _ = ojson.SetRawBytes(out, "system", common.JoinRawArray(systemBlocks))
	}
	if len(messageBlocks) > 0 {
		out = common.SetRawArrayItems(out, "messages", messageBlocks)
	}

	// Tools mapping: OpenAI tools -> Claude Code tools
	allowedToolNames := make(map[string]struct{})
	isAllowedTools := false
	allowedMode := "auto"
	if toolChoice := root.Get("tool_choice"); toolChoice.Exists() && toolChoice.IsObject() && toolChoice.Get("type").String() == "allowed_tools" {
		isAllowedTools = true
		toolList := toolChoice.Get("allowed_tools.tools").Array()
		if len(toolList) == 0 {
			toolList = toolChoice.Get("tools").Array()
		}
		for _, t := range toolList {
			fnName := strings.TrimSpace(t.Get("function.name").String())
			if fnName == "" {
				fnName = strings.TrimSpace(t.Get("name").String())
			}
			if fnName != "" {
				allowedToolNames[fnName] = struct{}{}
				allowedToolNames[util.SanitizeClaudeFunctionName(fnName)] = struct{}{}
			}
		}
		modeVal := strings.ToLower(strings.TrimSpace(toolChoice.Get("allowed_tools.mode").String()))
		if modeVal == "" {
			modeVal = strings.ToLower(strings.TrimSpace(toolChoice.Get("mode").String()))
		}
		if modeVal != "" {
			allowedMode = modeVal
		}
	}

	var anthropicTools [][]byte
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() && len(tools.Array()) > 0 {
		tools.ForEach(func(_, tool ojson.Result) bool {
			if tool.Get("type").String() == "function" {
				function := tool.Get("function")
				fnName := function.Get("name").String()
				sanitizedFnName := util.SanitizeClaudeFunctionName(fnName)
				if isAllowedTools {
					if _, ok := allowedToolNames[fnName]; !ok {
						if _, okSanitized := allowedToolNames[sanitizedFnName]; !okSanitized {
							return true
						}
					}
				}
				anthropicTool := []byte(`{"name":"","description":""}`)
				anthropicTool, _ = ojson.SetBytes(anthropicTool, "name", sanitizedFnName)
				anthropicTool, _ = ojson.SetBytes(anthropicTool, "description", function.Get("description").String())

				// Convert parameters schema for the tool
				if parameters := function.Get("parameters"); parameters.Exists() {
					anthropicTool, _ = ojson.SetRawBytes(anthropicTool, "input_schema", util.NormalizeClaudeToolInputSchema([]byte(parameters.Raw)))
				} else if parameters := function.Get("parametersJsonSchema"); parameters.Exists() {
					anthropicTool, _ = ojson.SetRawBytes(anthropicTool, "input_schema", util.NormalizeClaudeToolInputSchema([]byte(parameters.Raw)))
				} else {
					anthropicTool, _ = ojson.SetRawBytes(anthropicTool, "input_schema", util.NormalizeClaudeToolInputSchema(nil))
				}
				anthropicTool = common.AttachCacheControl(anthropicTool, tool)
				if !ojson.GetBytes(anthropicTool, "cache_control").Exists() {
					anthropicTool = common.AttachCacheControl(anthropicTool, function)
				}
				strict := function.Get("strict")
				if !strict.Exists() {
					strict = tool.Get("strict")
				}
				switch strict.Type {
				case ojson.True:
					anthropicTool, _ = ojson.SetBytes(anthropicTool, "strict", true)
				case ojson.False:
					anthropicTool, _ = ojson.SetBytes(anthropicTool, "strict", false)
				}

				anthropicTools = append(anthropicTools, anthropicTool)
			}
			return true
		})

		if len(anthropicTools) > 0 {
			out, _ = ojson.SetRawBytes(out, "tools", common.JoinRawArray(anthropicTools))
		} else {
			out, _ = ojson.DeleteBytes(out, "tools")
		}
	}

	// Tool choice mapping from OpenAI format to Claude Code format
	if isAllowedTools {
		if len(anthropicTools) == 0 {
			out, _ = ojson.SetRawBytes(out, "tool_choice", []byte(`{"type":"none"}`))
		} else if allowedMode == "required" {
			out, _ = ojson.SetRawBytes(out, "tool_choice", []byte(`{"type":"any"}`))
		} else {
			out, _ = ojson.SetRawBytes(out, "tool_choice", []byte(`{"type":"auto"}`))
		}
	} else if toolChoice := root.Get("tool_choice"); toolChoice.Exists() && toolChoice.Type != ojson.Null {
		switch toolChoice.Type {
		case ojson.String:
			choice := toolChoice.String()
			switch choice {
			case "none":
				out, _ = ojson.SetRawBytes(out, "tool_choice", []byte(`{"type":"none"}`))
			case "auto":
				out, _ = ojson.SetRawBytes(out, "tool_choice", []byte(`{"type":"auto"}`))
			case "required":
				out, _ = ojson.SetRawBytes(out, "tool_choice", []byte(`{"type":"any"}`))
			}
		case ojson.JSON:
			choiceType := toolChoice.Get("type").String()
			switch choiceType {
			case "none":
				out, _ = ojson.SetRawBytes(out, "tool_choice", []byte(`{"type":"none"}`))
			case "auto":
				out, _ = ojson.SetRawBytes(out, "tool_choice", []byte(`{"type":"auto"}`))
			case "required", "any":
				out, _ = ojson.SetRawBytes(out, "tool_choice", []byte(`{"type":"any"}`))
			case "function":
				functionName := toolChoice.Get("function.name").String()
				if functionName == "" {
					functionName = toolChoice.Get("name").String()
				}
				if functionName != "" {
					toolChoiceJSON := []byte(`{"type":"tool","name":""}`)
					toolChoiceJSON, _ = ojson.SetBytes(toolChoiceJSON, "name", util.SanitizeClaudeFunctionName(functionName))
					out, _ = ojson.SetRawBytes(out, "tool_choice", toolChoiceJSON)
				} else {
					out, _ = ojson.SetRawBytes(out, "tool_choice", []byte(`{"type":"none"}`))
				}
			}
		default:
		}
	}

	if parallelToolCalls := root.Get("parallel_tool_calls"); parallelToolCalls.Type == ojson.False {
		if ojson.GetBytes(out, "tool_choice").Exists() {
			if ojson.GetBytes(out, "tool_choice.type").String() != "none" {
				out, _ = ojson.SetBytes(out, "tool_choice.disable_parallel_tool_use", true)
			}
		} else if ojson.GetBytes(out, "tools").Exists() {
			out, _ = ojson.SetRawBytes(out, "tool_choice", []byte(`{"type":"auto","disable_parallel_tool_use":true}`))
		}
	}

	return out, nothingLeftErr
}

func convertOpenAIContentPartToClaudePartRaw(part ojson.Result) []byte {
	switch part.Get("type").String() {
	case "text":
		txt := part.Get("text").String()
		if strings.TrimSpace(txt) == "" {
			return nil
		}
		textPart := []byte(`{"type":"text","text":""}`)
		textPart, _ = ojson.SetBytes(textPart, "text", txt)
		return textPart

	case "image_url":
		return []byte(convertOpenAIImageURLToClaudePart(part.Get("image_url.url").String()))

	case "file":
		fileData := part.Get("file.file_data").String()
		if strings.HasPrefix(fileData, "data:") {
			semicolonIdx := strings.Index(fileData, ";")
			commaIdx := strings.Index(fileData, ",")
			if semicolonIdx != -1 && commaIdx != -1 && commaIdx > semicolonIdx {
				mediaType := strings.TrimPrefix(fileData[:semicolonIdx], "data:")
				data := fileData[commaIdx+1:]
				docPart := []byte(`{"type":"document","source":{"type":"base64","media_type":"","data":""}}`)
				docPart, _ = ojson.SetBytes(docPart, "source.media_type", mediaType)
				docPart, _ = ojson.SetBytes(docPart, "source.data", data)
				return docPart
			}
		}
	}
	return nil
}

func convertOpenAIContentPartToClaudePart(part ojson.Result) string {
	claudePart := convertOpenAIContentPartToClaudePartRaw(part)
	if len(claudePart) == 0 {
		return ""
	}
	return string(common.AttachCacheControl(claudePart, part))
}

func convertOpenAIImageURLToClaudePart(imageURL string) string {
	if imageURL == "" {
		return ""
	}

	if strings.HasPrefix(imageURL, "data:") {
		parts := strings.SplitN(imageURL, ",", 2)
		if len(parts) != 2 {
			return ""
		}

		mediaTypePart := strings.SplitN(parts[0], ";", 2)[0]
		mediaType := strings.TrimPrefix(mediaTypePart, "data:")
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}

		imagePart := []byte(`{"type":"image","source":{"type":"base64","media_type":"","data":""}}`)
		imagePart, _ = ojson.SetBytes(imagePart, "source.media_type", mediaType)
		imagePart, _ = ojson.SetBytes(imagePart, "source.data", parts[1])
		return string(imagePart)
	}

	imagePart := []byte(`{"type":"image","source":{"type":"url","url":""}}`)
	imagePart, _ = ojson.SetBytes(imagePart, "source.url", imageURL)
	return string(imagePart)
}

func convertOpenAIToolResultContent(content ojson.Result) (string, bool) {
	if !content.Exists() {
		return "", false
	}

	if content.Type == ojson.String {
		return content.String(), false
	}

	if content.IsArray() {
		claudeParts := make([][]byte, 0, 4)
		droppedEmptyText := false
		content.ForEach(func(_, part ojson.Result) bool {
			if part.Type == ojson.String {
				if strings.TrimSpace(part.String()) == "" {
					droppedEmptyText = true
					return true
				}
				textPart := []byte(`{"type":"text","text":""}`)
				textPart, _ = ojson.SetBytes(textPart, "text", part.String())
				claudeParts = append(claudeParts, textPart)
				return true
			}

			if claudePart := convertOpenAIContentPartToClaudePartRaw(part); len(claudePart) > 0 {
				claudeParts = append(claudeParts, claudePart)
			} else if part.Get("type").String() == "text" {
				droppedEmptyText = true
			}
			return true
		})

		if len(claudeParts) > 0 || len(content.Array()) == 0 {
			return string(common.JoinRawArray(claudeParts)), true
		}
		if droppedEmptyText {
			return "", false
		}

		return content.Raw, false
	}

	if content.IsObject() {
		if claudePart := convertOpenAIContentPartToClaudePartRaw(content); len(claudePart) > 0 {
			return string(common.JoinRawArray([][]byte{claudePart})), true
		}
		return content.Raw, false
	}

	return content.Raw, false
}

func defaultClaudeChatMaxTokensForModel(modelName string) int {
	normalized := strings.ToLower(strings.TrimSpace(modelName))
	maxTokens := defaultClaudeChatMaxTokens
	if strings.Contains(normalized, "fable") {
		maxTokens = defaultFableChatMaxTokens
	}
	info := registry.LookupModelInfo(modelName, "claude")
	if info == nil || info.MaxCompletionTokens <= 0 {
		return maxTokens
	}
	// Fable keeps its conservative omitted-field ceiling. Every other registered
	// model uses its output limit, including when that limit is above the
	// historical 32000 default.
	if strings.Contains(normalized, "fable") && info.MaxCompletionTokens >= maxTokens {
		return maxTokens
	}
	return info.MaxCompletionTokens
}
