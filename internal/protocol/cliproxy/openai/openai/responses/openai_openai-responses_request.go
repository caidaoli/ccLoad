package responses

import (
	"strings"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"

	"github.com/MoeForge/moego/ojson"
)

// ConvertOpenAIResponsesRequestToOpenAIChatCompletions converts OpenAI responses format to OpenAI chat completions format.
// It transforms the OpenAI responses API format (with instructions and input array) into the standard
// OpenAI chat completions format (with messages array and system content).
//
// The conversion handles:
// 1. Model name and streaming configuration
// 2. Instructions to system message conversion
// 3. Input array to messages array transformation
// 4. Tool definitions and tool choice conversion
// 5. Function calls and function results handling
// 6. Generation parameters mapping (max_tokens, reasoning, etc.)
//
// Parameters:
//   - modelName: The name of the model to use for the request
//   - rawJSON: The raw JSON request data in OpenAI responses format
//   - stream: A boolean indicating if the request is for a streaming response
//
// Returns:
//   - []byte: The transformed request data in OpenAI chat completions format
func ConvertOpenAIResponsesRequestToOpenAIChatCompletions(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	// ccLoad: reject tool-search modes Chat Completions cannot represent.
	if err := validateOpenAIResponsesRequestForChat(inputRawJSON); err != nil {
		return nil, err
	}
	return convertOpenAIResponsesRequestToOpenAIChatCompletions(modelName, inputRawJSON, stream)
}

// convertOpenAIResponsesRequestToOpenAIChatCompletions also reports a file or
// audio part Chat Completions cannot receive when it leaves a user turn with
// nothing to send.
func convertOpenAIResponsesRequestToOpenAIChatCompletions(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	rawJSON := inputRawJSON
	var drops translatorcommon.UserTurnDrops
	var toolOutputErr error
	// Base OpenAI chat completions template with default values
	out := []byte(`{"model":"","messages":[],"stream":false}`)

	root := ojson.ParseBytes(rawJSON)
	toolIndex := newResponsesToolIndex(root)

	messages := make([][]byte, 0)
	appendMessage := func(message []byte) {
		messages = append(messages, message)
	}

	// Set model name
	out, _ = ojson.SetBytes(out, "model", modelName)

	// Set stream configuration
	out, _ = ojson.SetBytes(out, "stream", stream)

	// Map Responses text format to Chat Completions response format.
	if textFormat := root.Get("text.format"); textFormat.Exists() {
		if responseFormat := convertResponsesTextFormatToChatResponseFormat(textFormat); len(responseFormat) > 0 {
			out, _ = ojson.SetRawBytes(out, "response_format", responseFormat)
		}
	}

	// Map generation parameters from responses format to chat completions format
	if maxTokens := root.Get("max_output_tokens"); maxTokens.Exists() {
		if maxTokens.Raw != "" {
			out, _ = ojson.SetRawBytes(out, "max_tokens", []byte(maxTokens.Raw))
		} else {
			out, _ = ojson.SetBytes(out, "max_tokens", maxTokens.Value())
		}
	}

	// Convert instructions to system message
	if instructions := root.Get("instructions"); instructions.Exists() {
		systemMessage := []byte(`{"role":"system","content":""}`)
		systemMessage, _ = ojson.SetBytes(systemMessage, "content", instructions.String())
		appendMessage(systemMessage)
	}

	duplicateOutputIDs := make(map[string]struct{})

	// Convert input array to messages
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		rawInputArray := toolIndex.shellHistory(input.Array())
		explicitOutputCounts := make(map[string]int)
		missingIDOutputsCount := 0
		for _, item := range rawInputArray {
			itemType := item.Get("type").String()
			if itemType == "function_call_output" || itemType == "custom_tool_call_output" {
				id := translatorcommon.ExtractResponsesCallID(item)
				if id != "" {
					explicitOutputCounts[id]++
				} else {
					missingIDOutputsCount++
				}
			}
		}

		unclaimedCalls := make(map[string]bool)
		for _, item := range rawInputArray {
			itemType := item.Get("type").String()
			if itemType == "function_call" || itemType == "custom_tool_call" {
				id := translatorcommon.ExtractResponsesCallID(item)
				if id != "" && explicitOutputCounts[id] == 0 {
					unclaimedCalls[id] = true
				}
			}
		}

		inputItems := translatorcommon.NormalizeResponsesToolCallOutputs(rawInputArray)
		if missingIDOutputsCount > 1 || (missingIDOutputsCount > 0 && len(unclaimedCalls) > 1) {
			for idx, item := range inputItems {
				itemType := item.Get("type").String()
				if itemType == "function_call_output" || itemType == "custom_tool_call_output" {
					if idx < len(rawInputArray) && translatorcommon.ExtractResponsesCallID(rawInputArray[idx]) == "" {
						raw := []byte(item.Raw)
						raw, _ = ojson.DeleteBytes(raw, "call_id")
						raw, _ = ojson.DeleteBytes(raw, "tool_call_id")
						raw, _ = ojson.DeleteBytes(raw, "callId")
						inputItems[idx] = ojson.ParseBytes(raw)
					}
				}
			}
		}

		hasReasoningInSession := false
		if effortRes := root.Get("reasoning.effort"); effortRes.Exists() {
			effort := strings.ToLower(strings.TrimSpace(effortRes.String()))
			if effort != "" && effort != "none" && effort != "0" && effort != "false" {
				hasReasoningInSession = true
			}
		} else if effortRes := root.Get("reasoning_effort"); effortRes.Exists() {
			effort := strings.ToLower(strings.TrimSpace(effortRes.String()))
			if effort != "" && effort != "none" && effort != "0" && effort != "false" {
				hasReasoningInSession = true
			}
		} else if reasoningObj := root.Get("reasoning"); reasoningObj.Exists() {
			reasoningRaw := strings.ToLower(strings.TrimSpace(reasoningObj.String()))
			if reasoningRaw != "" && reasoningRaw != "none" && reasoningRaw != "false" && reasoningRaw != "{}" {
				hasReasoningInSession = true
			}
		}
		if !hasReasoningInSession {
			for _, item := range rawInputArray {
				itemType := item.Get("type").String()
				if itemType == "reasoning" || item.Get("reasoning_content").Exists() {
					hasReasoningInSession = true
					break
				}
			}
		}

		pendingToolCalls := make([]interface{}, 0)
		pendingToolCallIDs := make([]string, 0)
		pendingReasoningContent := ""
		latestReasoningContent := ""
		awaitingToolOutputs := make(map[string]struct{})
		outputCounts := make(map[string]int)
		mergeableAssistantIndex := -1
		var pendingToolFileParts [][]byte

		fallbackToolReasoning := func() string {
			if latestReasoningContent != "" {
				return latestReasoningContent
			}
			if hasReasoningInSession {
				return "[reasoning unavailable]"
			}
			return ""
		}

		takePendingReasoningContent := func() string {
			reasoningContent := pendingReasoningContent
			pendingReasoningContent = ""
			return reasoningContent
		}
		flushPendingToolCalls := func() {
			if len(pendingToolCalls) == 0 {
				return
			}

			reasoningContent := takePendingReasoningContent()
			mergedIntoAssistant := false
			if mergeableAssistantIndex >= 0 && mergeableAssistantIndex == len(messages)-1 {
				assistantMessage := ojson.ParseBytes(messages[mergeableAssistantIndex])
				if assistantMessage.Get("role").String() == "assistant" && !assistantMessage.Get("tool_calls").Exists() {
					updatedMessage, _ := ojson.SetBytes(messages[mergeableAssistantIndex], "tool_calls", pendingToolCalls)
					combinedReasoning := combineOpenAIResponsesReasoning(assistantMessage.Get("reasoning_content").String(), reasoningContent)
					if combinedReasoning != "" {
						updatedMessage, _ = ojson.SetBytes(updatedMessage, "reasoning_content", combinedReasoning)
						if isUsableResponsesReasoning(combinedReasoning) {
							latestReasoningContent = combinedReasoning
						}
					} else if fallback := fallbackToolReasoning(); fallback != "" {
						updatedMessage, _ = ojson.SetBytes(updatedMessage, "reasoning_content", fallback)
					}
					messages[mergeableAssistantIndex] = updatedMessage
					mergedIntoAssistant = true
				}
			}
			if !mergedIntoAssistant {
				assistantMessage := []byte(`{"role":"assistant","tool_calls":[]}`)
				assistantMessage, _ = ojson.SetBytes(assistantMessage, "tool_calls", pendingToolCalls)
				if reasoningContent != "" {
					assistantMessage, _ = ojson.SetBytes(assistantMessage, "reasoning_content", reasoningContent)
					if isUsableResponsesReasoning(reasoningContent) {
						latestReasoningContent = reasoningContent
					}
				} else if fallback := fallbackToolReasoning(); fallback != "" {
					assistantMessage, _ = ojson.SetBytes(assistantMessage, "reasoning_content", fallback)
				}
				appendMessage(assistantMessage)
			}
			for _, id := range pendingToolCallIDs {
				trimmed := strings.TrimSpace(id)
				if trimmed == "" {
					continue
				}
				awaitingToolOutputs[trimmed] = struct{}{}
			}
			pendingToolCalls = pendingToolCalls[:0]
			pendingToolCallIDs = pendingToolCallIDs[:0]
			mergeableAssistantIndex = -1
		}
		appendRegularMessage := func(message []byte) int {
			appendMessage(message)
			return len(messages) - 1
		}
		// Chat tool messages cannot carry files or (for object outputs) images,
		// so tool output media is relayed in one user message after the whole
		// tool-result group to keep tool_calls answered contiguously.
		flushPendingToolFileParts := func() {
			if len(pendingToolFileParts) == 0 {
				return
			}
			userMessage := translatorcommon.SetRawArrayItems([]byte(`{"role":"user","content":[]}`), "content", pendingToolFileParts)
			appendRegularMessage(userMessage)
			pendingToolFileParts = nil
		}
		appendPendingReasoningMessage := func() {
			reasoningContent := takePendingReasoningContent()
			if reasoningContent == "" {
				return
			}
			if isUsableResponsesReasoning(reasoningContent) {
				latestReasoningContent = reasoningContent
			}
			message := []byte(`{"role":"assistant","content":"","reasoning_content":""}`)
			message, _ = ojson.SetBytes(message, "reasoning_content", reasoningContent)
			appendRegularMessage(message)
		}

		for _, item := range inputItems {
			itemType := item.Get("type").String()
			if itemType == "" && item.Get("role").String() != "" {
				itemType = "message"
			}
			if itemType != "function_call" && itemType != "custom_tool_call" && itemType != "tool_search_call" {
				flushPendingToolCalls()
			}
			if itemType != "function_call_output" && itemType != "custom_tool_call_output" && itemType != "tool_search_output" {
				flushPendingToolFileParts()
			}

			switch itemType {
			case "message", "":
				// Handle regular message conversion
				role := item.Get("role").String()
				// Chat Completions supports developer messages. Preserve their role and
				// position so mid-session instructions retain their original authority.
				// Only real user turns participate in unsupported-content refusal.
				isUserTurn := role == "user"
				mergeableAssistantIndex = -1
				if role != "assistant" {
					appendPendingReasoningMessage()
					latestReasoningContent = ""
				}
				message := []byte(`{"role":"","content":[]}`)
				message, _ = ojson.SetBytes(message, "role", role)

				if content := item.Get("content"); content.Exists() && content.IsArray() {
					var contentItems [][]byte
					// Counts the parts this turn really sends; an empty text part does not.
					turnSendable := 0
					content.ForEach(func(_, contentItem ojson.Result) bool {
						contentType := contentItem.Get("type").String()
						if contentType == "" {
							contentType = "input_text"
						}

						switch contentType {
						case "input_text", "output_text":
							text := contentItem.Get("text").String()
							contentPart := []byte(`{"type":"text","text":""}`)
							contentPart, _ = ojson.SetBytes(contentPart, "text", text)
							contentItems = append(contentItems, contentPart)
							if text != "" {
								turnSendable++
							}
						case "input_video", "video_url":
							contentPart := []byte(`{"type":"video_url","video_url":{}}`)
							videoURL := contentItem.Get("video_url")
							if videoURL.IsObject() {
								contentPart, _ = ojson.SetRawBytes(contentPart, "video_url", []byte(videoURL.Raw))
							} else if videoURL.Exists() {
								contentPart, _ = ojson.SetRawBytes(contentPart, "video_url.url", []byte(videoURL.Raw))
							}
							if processing := contentItem.Get("processing"); processing.Exists() {
								contentPart, _ = ojson.SetRawBytes(contentPart, "video_url.processing", []byte(processing.Raw))
							}
							// Preserve malformed video parts for upstream validation instead of
							// silently turning a video request into a text-only request.
							contentItems = append(contentItems, contentPart)
							turnSendable++
						case "input_image":
							imageURL := contentItem.Get("image_url").String()
							contentPart := []byte(`{"type":"image_url","image_url":{"url":""}}`)
							contentPart, _ = ojson.SetBytes(contentPart, "image_url.url", imageURL)
							if detail, ok := normalizeChatImageDetail(contentItem.Get("detail")); ok && detail != "" {
								contentPart, _ = ojson.SetBytes(contentPart, "image_url.detail", detail)
							}
							contentItems = append(contentItems, contentPart)
							turnSendable++
						case "input_file":
							if contentPart, ok := responsesInputFileToChatPart(contentItem); ok {
								contentItems = append(contentItems, contentPart)
								turnSendable++
							} else if isUserTurn {
								// Chat Completions takes a file id or inline bytes only.
								drops.Drop(contentType)
							}
						case "input_audio":
							if contentPart, ok := responsesInputAudioToChatPart(contentItem); ok {
								contentItems = append(contentItems, contentPart)
								turnSendable++
							} else if isUserTurn {
								drops.Drop(contentType)
							}
						}
						return true
					})
					if isUserTurn {
						drops.EndTurn(turnSendable)
					}
					message = translatorcommon.SetRawArrayItems(message, "content", contentItems)
				} else if content.Type == ojson.String {
					message, _ = ojson.SetBytes(message, "content", content.String())
				}

				if role == "assistant" {
					reasoningContent := combineOpenAIResponsesReasoning(takePendingReasoningContent(), item.Get("reasoning_content").String())
					if reasoningContent != "" {
						message, _ = ojson.SetBytes(message, "reasoning_content", reasoningContent)
						if isUsableResponsesReasoning(reasoningContent) {
							latestReasoningContent = reasoningContent
						}
					}
				}

				messageIndex := appendRegularMessage(message)
				if role == "assistant" {
					mergeableAssistantIndex = messageIndex
				}

			case "reasoning":
				reasoningContent := collectOpenAIResponsesReasoningContent(item)
				pendingReasoningContent = combineOpenAIResponsesReasoning(pendingReasoningContent, reasoningContent)
				if isUsableResponsesReasoning(reasoningContent) {
					latestReasoningContent = reasoningContent
				}

			case "function_call":
				rc := item.Get("reasoning_content").String()
				pendingReasoningContent = combineOpenAIResponsesReasoning(pendingReasoningContent, rc)
				if isUsableResponsesReasoning(rc) {
					latestReasoningContent = rc
				}
				// Buffer consecutive function calls and emit them as one assistant message.
				toolCall := []byte(`{"id":"","type":"function","function":{"name":"","arguments":"{}"}}`)

				if callId := translatorcommon.ExtractResponsesCallID(item); callId != "" {
					toolCall, _ = ojson.SetBytes(toolCall, "id", callId)
				}

				if name := item.Get("name"); name.Exists() {
					functionName := name.String()
					if namespace := strings.TrimSpace(item.Get("namespace").String()); namespace != "" {
						functionName = toolIndex.namespaceName(namespace, functionName)
					} else {
						functionName = toolIndex.canonicalName(functionName)
					}
					toolCall, _ = ojson.SetBytes(toolCall, "function.name", functionName)
				}

				if arguments := item.Get("arguments").String(); strings.TrimSpace(arguments) != "" {
					toolCall, _ = ojson.SetBytes(toolCall, "function.arguments", arguments)
				}
				pendingToolCalls = append(pendingToolCalls, ojson.ParseBytes(toolCall).Value())
				if callID := translatorcommon.ExtractResponsesCallID(item); callID != "" {
					pendingToolCallIDs = append(pendingToolCallIDs, callID)
				}

			case "tool_search_call":
				rc := item.Get("reasoning_content").String()
				pendingReasoningContent = combineOpenAIResponsesReasoning(pendingReasoningContent, rc)
				if isUsableResponsesReasoning(rc) {
					latestReasoningContent = rc
				}
				toolCall := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)
				callID := strings.TrimSpace(item.Get("call_id").String())
				toolCall, _ = ojson.SetBytes(toolCall, "id", callID)
				toolCall, _ = ojson.SetBytes(toolCall, "function.name", responsesChatToolSearchFunctionName)
				if arguments, err := normalizeResponsesToolSearchArguments(item.Get("arguments")); err == nil {
					toolCall, _ = ojson.SetBytes(toolCall, "function.arguments", arguments)
				}
				pendingToolCalls = append(pendingToolCalls, ojson.ParseBytes(toolCall).Value())
				if callID != "" {
					pendingToolCallIDs = append(pendingToolCallIDs, callID)
				}

			case "function_call_output":
				mergeableAssistantIndex = -1
				callID := translatorcommon.ExtractResponsesCallID(item)
				if callID != "" {
					outputCounts[callID]++
					if outputCounts[callID] > 1 {
						duplicateOutputIDs[callID] = struct{}{}
					}
				}
				if _, awaiting := awaitingToolOutputs[callID]; !awaiting {
					// Orphan outputs (empty call_id or no matching assistant
					// tool_calls, e.g. Codex send_message_to_thread cards) must
					// not become tool messages. Emit as user text instead.
					if errStandalone := appendStandaloneResponsesToolOutputAsUser(item.Get("output"), setFunctionCallOutputContent, appendRegularMessage); errStandalone != nil && toolOutputErr == nil {
						toolOutputErr = errStandalone
					}
				} else {
					toolMessage := []byte(`{"role":"tool","tool_call_id":"","content":""}`)
					toolMessage, _ = ojson.SetBytes(toolMessage, "tool_call_id", callID)
					delete(awaitingToolOutputs, callID)
					if output := item.Get("output"); output.Exists() {
						var fileParts [][]byte
						var errToolOut error
						toolMessage, fileParts, errToolOut = setFunctionCallOutputContent(toolMessage, output, true)
						if errToolOut != nil && toolOutputErr == nil {
							toolOutputErr = errToolOut
						}
						pendingToolFileParts = append(pendingToolFileParts, fileParts...)
					}
					appendMessage(toolMessage)
					if len(awaitingToolOutputs) == 0 {
						flushPendingToolFileParts()
					}
				}

			case "tool_search_output":
				mergeableAssistantIndex = -1
				callID := strings.TrimSpace(item.Get("call_id").String())
				toolMessage := []byte(`{"role":"tool","tool_call_id":"","content":""}`)
				toolMessage, _ = ojson.SetBytes(toolMessage, "tool_call_id", callID)
				toolMessage = setToolSearchOutputContent(toolMessage, item.Get("tools"))
				appendMessage(toolMessage)
				if callID != "" {
					delete(awaitingToolOutputs, callID)
				}
				if len(awaitingToolOutputs) == 0 {
					flushPendingToolFileParts()
				}

			case "custom_tool_call":
				rc := item.Get("reasoning_content").String()
				pendingReasoningContent = combineOpenAIResponsesReasoning(pendingReasoningContent, rc)
				if isUsableResponsesReasoning(rc) {
					latestReasoningContent = rc
				}
				// Codex freeform tool call replay: wrap the raw input so it
				// matches the {"input": string} function shape used when
				// converting custom tool definitions.
				toolCall := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)
				toolCall, _ = ojson.SetBytes(toolCall, "id", translatorcommon.ExtractResponsesCallID(item))
				functionName := item.Get("name").String()
				if namespace := item.Get("namespace").String(); namespace != "" {
					functionName = toolIndex.namespaceName(namespace, functionName)
				} else {
					functionName = toolIndex.canonicalName(functionName)
				}
				toolCall, _ = ojson.SetBytes(toolCall, "function.name", functionName)
				wrappedArgs, _ := ojson.SetBytes([]byte(`{"input":""}`), "input", item.Get("input").String())
				toolCall, _ = ojson.SetBytes(toolCall, "function.arguments", string(wrappedArgs))
				pendingToolCalls = append(pendingToolCalls, ojson.ParseBytes(toolCall).Value())
				if callID := translatorcommon.ExtractResponsesCallID(item); callID != "" {
					pendingToolCallIDs = append(pendingToolCallIDs, callID)
				}

			case "custom_tool_call_output":
				mergeableAssistantIndex = -1
				callID := translatorcommon.ExtractResponsesCallID(item)
				if callID != "" {
					outputCounts[callID]++
					if outputCounts[callID] > 1 {
						duplicateOutputIDs[callID] = struct{}{}
					}
				}
				if _, awaiting := awaitingToolOutputs[callID]; !awaiting {
					if errStandalone := appendStandaloneResponsesToolOutputAsUser(item.Get("output"), setCustomToolCallOutputContent, appendRegularMessage); errStandalone != nil && toolOutputErr == nil {
						toolOutputErr = errStandalone
					}
				} else {
					toolMessage := []byte(`{"role":"tool","tool_call_id":"","content":""}`)
					toolMessage, _ = ojson.SetBytes(toolMessage, "tool_call_id", callID)
					delete(awaitingToolOutputs, callID)
					if output := item.Get("output"); output.Exists() {
						var fileParts [][]byte
						var errToolOut error
						toolMessage, fileParts, errToolOut = setCustomToolCallOutputContent(toolMessage, output, true)
						if errToolOut != nil && toolOutputErr == nil {
							toolOutputErr = errToolOut
						}
						pendingToolFileParts = append(pendingToolFileParts, fileParts...)
					}
					appendMessage(toolMessage)
					if len(awaitingToolOutputs) == 0 {
						flushPendingToolFileParts()
					}
				}

			case "compaction":
				// Codex context compaction is encrypted control metadata and has no
				// Chat Completions representation; the OpenAI bridge intentionally drops it.
				mergeableAssistantIndex = -1
			default:
				mergeableAssistantIndex = -1
			}

		}
		flushPendingToolCalls()
		flushPendingToolFileParts()
		appendPendingReasoningMessage()
	} else if input.Type == ojson.String {
		msg := []byte(`{}`)
		msg, _ = ojson.SetBytes(msg, "role", "user")
		msg, _ = ojson.SetBytes(msg, "content", input.String())
		appendMessage(msg)
	}

	if len(messages) > 0 {
		var extraAmbiguous []string
		for id := range duplicateOutputIDs {
			extraAmbiguous = append(extraAmbiguous, id)
		}
		messages = translatorcommon.AlignOpenAIToolCallMessages(messages, extraAmbiguous...)
		out, _ = ojson.SetRawBytes(out, "messages", translatorcommon.JoinRawArray(messages))
	}

	// Convert tools from responses format to chat completions format.
	// Codex Desktop (Responses Lite) delivers tool definitions through an
	// "additional_tools" input item instead of the top-level "tools" field,
	// so merge both sources.
	var chatCompletionsTools []interface{}
	var webSearch ojson.Result
	findWebSearch := func(tools ojson.Result) {
		if !tools.Exists() || !tools.IsArray() {
			return
		}
		tools.ForEach(func(_, tool ojson.Result) bool {
			if strings.HasPrefix(tool.Get("type").String(), "web_search") {
				webSearch = tool
			}
			return true
		})
	}
	findWebSearch(root.Get("tools"))
	if input := root.Get("input"); input.Exists() && input.IsArray() {
		input.ForEach(func(_, item ojson.Result) bool {
			if item.Get("type").String() == "additional_tools" {
				findWebSearch(item.Get("tools"))
			}
			return true
		})
	}
	for _, chatTool := range responsesRequestChatTools(root, toolIndex) {
		chatCompletionsTools = append(chatCompletionsTools, ojson.ParseBytes(chatTool).Value())
	}
	if len(chatCompletionsTools) > 0 {
		out, _ = ojson.SetBytes(out, "tools", chatCompletionsTools)
		if parallelToolCalls := root.Get("parallel_tool_calls"); parallelToolCalls.Exists() {
			out, _ = ojson.SetBytes(out, "parallel_tool_calls", parallelToolCalls.Bool())
		}
		if toolChoice := root.Get("tool_choice"); toolChoice.Exists() {
			out, _ = ojson.SetRawBytes(out, "tool_choice", convertResponsesToolChoiceWithIndex(root, toolIndex))
		}
	}
	if webSearch.Exists() {
		options := []byte(`{}`)
		if size := webSearch.Get("search_context_size"); size.Exists() {
			options, _ = ojson.SetBytes(options, "search_context_size", size.String())
		}
		if location := webSearch.Get("user_location"); location.Exists() && location.IsObject() {
			options, _ = ojson.SetBytes(options, "user_location.type", "approximate")
			if approximate := location.Get("approximate"); approximate.Exists() && approximate.IsObject() {
				options, _ = ojson.SetRawBytes(options, "user_location.approximate", []byte(approximate.Raw))
			} else {
				approximate := []byte(location.Raw)
				approximate, _ = ojson.DeleteBytes(approximate, "type")
				options, _ = ojson.SetRawBytes(options, "user_location.approximate", approximate)
			}
		}
		out, _ = ojson.SetRawBytes(out, "web_search_options", options)
	}

	if reasoningEffort := root.Get("reasoning.effort"); reasoningEffort.Exists() {
		effort := strings.ToLower(strings.TrimSpace(reasoningEffort.String()))
		if effort != "" {
			out, _ = ojson.SetBytes(out, "reasoning_effort", effort)
		}
	}

	if errDrops := drops.Err(); errDrops != nil {
		return out, errDrops
	}
	return out, toolOutputErr
}

// responsesInputFileToChatPart maps a Responses input_file part onto a Chat
// Completions file part. ccLoad also forwards file_url as a wire extension. It
// reports false when the part carries no file id, inline bytes, or url.
func responsesInputFileToChatPart(contentItem ojson.Result) ([]byte, bool) {
	part := []byte(`{"type":"file","file":{}}`)
	sendable := false
	for _, field := range []struct{ source, target string }{
		{"file_id", "file.file_id"},
		{"file_data", "file.file_data"},
		{"file_url", "file.file_url"},
		{"filename", "file.filename"},
	} {
		if value := contentItem.Get(field.source).String(); value != "" {
			part, _ = ojson.SetBytes(part, field.target, value)
			sendable = sendable || field.source != "filename"
		}
	}
	if !sendable {
		return nil, false
	}
	return part, true
}

// responsesInputAudioToChatPart maps a Responses input_audio part onto a Chat
// Completions input_audio part. It reports false when the part has no bytes.
func responsesInputAudioToChatPart(contentItem ojson.Result) ([]byte, bool) {
	audio := contentItem.Get("input_audio")
	data := audio.Get("data").String()
	if data == "" {
		data = contentItem.Get("data").String()
	}
	if data == "" {
		return nil, false
	}
	format := audio.Get("format").String()
	if format == "" {
		format = contentItem.Get("format").String()
	}
	part := []byte(`{"type":"input_audio","input_audio":{"data":""}}`)
	part, _ = ojson.SetBytes(part, "input_audio.data", data)
	if format != "" {
		part, _ = ojson.SetBytes(part, "input_audio.format", format)
	}
	return part, true
}

func convertResponsesToolChoiceWithIndex(root ojson.Result, toolIndex *responsesToolIndex) []byte {
	toolChoice := root.Get("tool_choice")
	if !toolChoice.IsObject() {
		return []byte(toolChoice.Raw)
	}

	choiceType := toolChoice.Get("type").String()
	if choiceType == "shell" {
		if name := toolIndex.shellName(); name != "" {
			converted, _ := ojson.SetBytes([]byte(`{"type":"function","function":{}}`), "function.name", name)
			return converted
		}
	}
	if choiceType == "tool_search" && responsesRequestHasToolSearch(root) {
		converted, _ := ojson.SetBytes([]byte(`{"type":"function","function":{}}`), "function.name", responsesChatToolSearchFunctionName)
		return converted
	}
	if choiceType != "function" && choiceType != "custom" {
		return []byte(toolChoice.Raw)
	}

	name := toolChoice.Get("function.name").String()
	if name == "" {
		name = toolChoice.Get("custom.name").String()
	}
	if name == "" {
		name = toolChoice.Get("name").String()
	}
	if name == "" {
		return []byte(toolChoice.Raw)
	}

	namespace := strings.TrimSpace(toolChoice.Get("namespace").String())
	if namespace == "" {
		namespace = strings.TrimSpace(toolChoice.Get("function.namespace").String())
	}
	if namespace == "" {
		namespace = strings.TrimSpace(toolChoice.Get("custom.namespace").String())
	}
	if namespace != "" {
		name = toolIndex.namespaceName(namespace, name)
	} else {
		name = toolIndex.canonicalName(name)
	}

	converted := []byte(`{"type":"function","function":{"name":""}}`)
	converted, _ = ojson.SetBytes(converted, "function.name", name)
	return converted
}

func convertResponsesTextFormatToChatResponseFormat(textFormat ojson.Result) []byte {
	formatType := textFormat.Get("type").String()
	switch formatType {
	case "text", "json_object":
		responseFormat := []byte(`{"type":""}`)
		responseFormat, _ = ojson.SetBytes(responseFormat, "type", formatType)
		return responseFormat
	case "json_schema":
		responseFormat := []byte(`{"type":"json_schema","json_schema":{}}`)
		for _, field := range []string{"name", "description", "strict"} {
			if value := textFormat.Get(field); value.Exists() {
				responseFormat, _ = ojson.SetBytes(responseFormat, "json_schema."+field, value.Value())
			}
		}
		if schema := textFormat.Get("schema"); schema.Exists() {
			responseFormat, _ = ojson.SetRawBytes(responseFormat, "json_schema.schema", []byte(schema.Raw))
		}
		return responseFormat
	default:
		return nil
	}
}

func appendStandaloneResponsesToolOutputAsUser(output ojson.Result, setContent func([]byte, ojson.Result, bool) ([]byte, [][]byte, error), appendMessage func([]byte) int) error {
	userMessage := []byte(`{"role":"user","content":""}`)
	if output.Exists() {
		var errSet error
		if userMessage, _, errSet = setContent(userMessage, output, false); errSet != nil {
			return errSet
		}
	}
	content := ojson.GetBytes(userMessage, "content")
	if !content.Exists() {
		return nil
	}
	if content.Type == ojson.String && strings.TrimSpace(content.String()) == "" {
		return nil
	}
	if content.IsArray() && !content.Get("0").Exists() {
		return nil
	}
	appendMessage(userMessage)
	return nil
}

// setFunctionCallOutputContent writes a Responses tool output onto a Chat
// message. With separateFiles the message is a tool message: files and media
// from object outputs are returned for a follow-up user message and the tool
// content keeps only text. Unsendable media yields an UnsupportedPartError.
func setFunctionCallOutputContent(toolMessage []byte, output ojson.Result, separateFiles bool) ([]byte, [][]byte, error) {
	structuredContent := output
	if output.Type == ojson.String {
		if !ojson.Valid(output.String()) {
			toolMessage, _ = ojson.SetBytes(toolMessage, "content", output.String())
			return toolMessage, nil, nil
		}
		structuredContent = ojson.Parse(output.String())
	}

	parts := inspectChatToolOutputParts(structuredContent)
	if parts.unsupportedType != "" {
		return toolMessage, nil, &translatorcommon.UnsupportedPartError{Type: parts.unsupportedType}
	}
	if parts.hasFile || (structuredContent.IsObject() && parts.hasImage) {
		if parts.badImageType != "" {
			return toolMessage, nil, &translatorcommon.UnsupportedPartError{Type: parts.badImageType}
		}
		var toolItems [][]byte
		var userMediaItems [][]byte
		var textParts []string
		for _, item := range parts.items {
			switch item.Get("type").String() {
			case "input_file", "file":
				if filePart, ok := chatToolOutputFilePart(item); ok {
					if separateFiles {
						userMediaItems = append(userMediaItems, filePart)
					} else {
						toolItems = append(toolItems, filePart)
					}
				}
			case "image_url", "input_image":
				if _, _, ok := chatToolOutputImageFields(item); ok {
					imagePart := chatToolOutputContentPart(item)
					if separateFiles {
						userMediaItems = append(userMediaItems, imagePart)
					} else {
						toolItems = append(toolItems, imagePart)
					}
				}
			case "text", "input_text", "output_text":
				if item.Get("text").Type == ojson.String {
					textParts = append(textParts, item.Get("text").String())
					toolItems = append(toolItems, chatToolOutputContentPart(item))
					continue
				}
				fallthrough
			default:
				fallback := chatToolOutputFallbackPart(item)
				textParts = append(textParts, ojson.GetBytes(fallback, "text").String())
				toolItems = append(toolItems, fallback)
			}
		}
		if separateFiles {
			if len(textParts) == 0 {
				toolMessage, _ = ojson.SetBytes(toolMessage, "content", "Attached media from tool output follows in the user message.")
			} else {
				toolMessage, _ = ojson.SetBytes(toolMessage, "content", strings.Join(textParts, "\n"))
			}
			return toolMessage, userMediaItems, nil
		}
		return translatorcommon.SetRawArrayItems(toolMessage, "content", toolItems), nil, nil
	}
	if structuredContent.IsObject() && parts.badImageType != "" {
		return toolMessage, nil, &translatorcommon.UnsupportedPartError{Type: parts.badImageType}
	}
	if parts.hasImage && !parts.invalidTextOrImage {
		contentItems := make([][]byte, 0, len(parts.items))
		for _, item := range parts.items {
			contentItems = append(contentItems, chatToolOutputContentPart(item))
		}
		return translatorcommon.SetRawArrayItems(toolMessage, "content", contentItems), nil, nil
	}

	toolMessage, _ = ojson.SetBytes(toolMessage, "content", output.String())
	return toolMessage, nil, nil
}

func setCustomToolCallOutputContent(toolMessage []byte, output ojson.Result, separateFiles bool) ([]byte, [][]byte, error) {
	structuredContent := output
	if output.Type == ojson.String && ojson.Valid(output.String()) {
		structuredContent = ojson.Parse(output.String())
	}
	parts := inspectChatToolOutputParts(structuredContent)
	if parts.unsupportedType != "" {
		return toolMessage, nil, &translatorcommon.UnsupportedPartError{Type: parts.unsupportedType}
	}
	if parts.hasFile || (structuredContent.IsObject() && (parts.hasImage || parts.badImageType != "")) || (parts.hasImage && !parts.invalidTextOrImage) {
		return setFunctionCallOutputContent(toolMessage, output, separateFiles)
	}

	toolMessage, _ = ojson.SetBytes(toolMessage, "content", responsesToolOutputText(output))
	return toolMessage, nil, nil
}

func chatToolOutputContentPart(item ojson.Result) []byte {
	itemType := item.Get("type").String()
	switch itemType {
	case "text", "input_text", "output_text":
		part := []byte(`{"type":"text","text":""}`)
		part, _ = ojson.SetBytes(part, "text", item.Get("text").String())
		return part
	case "image_url", "input_image":
		imageURL, detail, ok := chatToolOutputImageFields(item)
		if !ok {
			return chatToolOutputFallbackPart(item)
		}
		part := []byte(`{"type":"image_url","image_url":{"url":""}}`)
		part, _ = ojson.SetBytes(part, "image_url.url", imageURL)
		if detail != "" {
			part, _ = ojson.SetBytes(part, "image_url.detail", detail)
		}
		return part
	case "input_file", "file":
		if filePart, ok := chatToolOutputFilePart(item); ok {
			return filePart
		}
		return chatToolOutputFallbackPart(item)
	default:
		return chatToolOutputFallbackPart(item)
	}
}

func chatToolOutputFilePart(item ojson.Result) ([]byte, bool) {
	if item.Get("type").String() == "file" && item.Get("file").IsObject() {
		return responsesInputFileToChatPart(item.Get("file"))
	}
	return responsesInputFileToChatPart(item)
}

func isChatToolOutputPartObject(content ojson.Result) bool {
	if !content.IsObject() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(content.Get("type").String())) {
	case "input_text", "output_text", "image_url", "input_image", "input_file", "file", "input_audio", "audio", "input_video", "video":
		return true
	case "text":
		return content.Get("text").Type == ojson.String
	default:
		return false
	}
}

// chatToolOutputParts summarizes a structured tool output; a single content
// part object is treated as a one-element array.
type chatToolOutputParts struct {
	items              []ojson.Result
	hasImage           bool
	hasFile            bool
	invalidTextOrImage bool
	badImageType       string
	unsupportedType    string
}

func inspectChatToolOutputParts(content ojson.Result) chatToolOutputParts {
	var parts chatToolOutputParts
	switch {
	case content.IsArray():
		parts.items = content.Array()
	case isChatToolOutputPartObject(content):
		parts.items = []ojson.Result{content}
	default:
		return parts
	}

	for _, item := range parts.items {
		itemType := item.Get("type")
		if itemType.Type != ojson.String {
			continue
		}
		typeName := itemType.String()
		switch typeName {
		case "text", "input_text", "output_text":
			if item.Get("text").Type != ojson.String {
				parts.invalidTextOrImage = true
			}
		case "image_url", "input_image":
			if _, _, ok := chatToolOutputImageFields(item); !ok {
				parts.invalidTextOrImage = true
				if parts.badImageType == "" {
					parts.badImageType = typeName
				}
			} else {
				parts.hasImage = true
			}
		case "input_file", "file":
			if _, ok := chatToolOutputFilePart(item); ok {
				parts.hasFile = true
			} else if parts.unsupportedType == "" {
				parts.unsupportedType = typeName
			}
		case "input_audio", "audio", "input_video", "video":
			if parts.unsupportedType == "" {
				parts.unsupportedType = typeName
			}
		}
	}
	return parts
}

func chatToolOutputImageFields(item ojson.Result) (imageURL, detail string, ok bool) {
	var imageURLValue ojson.Result
	var detailValue ojson.Result
	switch item.Get("type").String() {
	case "image_url":
		imageURLValue = item.Get("image_url.url")
		detailValue = item.Get("image_url.detail")
	case "input_image":
		imageURLValue = item.Get("image_url")
		detailValue = item.Get("detail")
	default:
		return "", "", false
	}

	if imageURLValue.Type != ojson.String {
		return "", "", false
	}
	imageURL = strings.TrimSpace(imageURLValue.String())
	if imageURL == "" {
		return "", "", false
	}

	detail, ok = normalizeChatImageDetail(detailValue)
	if !ok {
		return "", "", false
	}
	return imageURL, detail, true
}

func normalizeChatImageDetail(detailValue ojson.Result) (string, bool) {
	if !detailValue.Exists() {
		return "", true
	}
	if detailValue.Type != ojson.String {
		return "", false
	}

	normalizedDetail := strings.ToLower(strings.TrimSpace(detailValue.String()))
	switch normalizedDetail {
	case "auto", "low", "high":
		return normalizedDetail, true
	case "original":
		// Chat Completions does not support Codex's original detail value.
		return "high", true
	default:
		return "", true
	}
}

func chatToolOutputFallbackPart(item ojson.Result) []byte {
	text := item.Raw
	if item.Type == ojson.String || text == "" {
		text = item.String()
	}
	part := []byte(`{"type":"text","text":""}`)
	part, _ = ojson.SetBytes(part, "text", text)
	return part
}

func collectOpenAIResponsesReasoningContent(item ojson.Result) string {
	var reasoningText strings.Builder
	if summary := item.Get("summary"); summary.Exists() && summary.IsArray() {
		summary.ForEach(func(_, summaryItem ojson.Result) bool {
			if summaryItem.Get("type").String() != "summary_text" {
				return true
			}
			reasoningText.WriteString(summaryItem.Get("text").String())
			return true
		})
	}
	if reasoningText.Len() == 0 {
		return "[reasoning unavailable]"
	}
	return reasoningText.String()
}

func combineOpenAIResponsesReasoning(existing, incoming string) string {
	existingTrimmed := strings.TrimSpace(existing)
	incomingTrimmed := strings.TrimSpace(incoming)

	switch {
	case existingTrimmed == "":
		return incoming
	case incomingTrimmed == "":
		return existing
	case existingTrimmed == "[reasoning unavailable]":
		return incoming
	case incomingTrimmed == "[reasoning unavailable]", existingTrimmed == incomingTrimmed:
		return existing
	default:
		return existing + "\n\n" + incoming
	}
}

func isUsableResponsesReasoning(reasoning string) bool {
	trimmed := strings.TrimSpace(reasoning)
	return trimmed != "" && trimmed != "[reasoning unavailable]"
}
