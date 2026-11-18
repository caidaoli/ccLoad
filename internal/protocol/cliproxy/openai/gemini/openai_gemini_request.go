// Package gemini provides request translation functionality for Gemini to OpenAI API.
// It handles parsing and transforming Gemini API requests into OpenAI Chat Completions API format,
// extracting model information, generation config, message contents, and tool declarations.
// The package performs JSON data transformation to ensure compatibility
// between Gemini API format and OpenAI API's expected format.
package gemini

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"
	"ccLoad/internal/protocol/cliproxy/thinking"
	"ccLoad/internal/protocol/cliproxy/util"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ConvertGeminiRequestToOpenAI parses and transforms a Gemini API request into OpenAI Chat Completions API format.
// It extracts the model name, generation config, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the OpenAI API.
func ConvertGeminiRequestToOpenAI(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	rawJSON := inputRawJSON
	// Base OpenAI Chat Completions API template
	out := []byte(`{"model":"","messages":[]}`)

	root := gjson.ParseBytes(rawJSON)

	// Model mapping
	out, _ = sjson.SetBytes(out, "model", modelName)

	// Generation config mapping
	if genConfig := root.Get("generationConfig"); genConfig.Exists() {
		// Temperature
		if temp := genConfig.Get("temperature"); temp.Exists() {
			out, _ = sjson.SetBytes(out, "temperature", temp.Float())
		}

		// Max tokens
		if maxTokens := genConfig.Get("maxOutputTokens"); maxTokens.Exists() {
			out, _ = sjson.SetBytes(out, "max_tokens", maxTokens.Int())
		}

		// Top P
		if topP := genConfig.Get("topP"); topP.Exists() {
			out, _ = sjson.SetBytes(out, "top_p", topP.Float())
		}

		// Top K (OpenAI doesn't have direct equivalent, but we can map it)
		if topK := genConfig.Get("topK"); topK.Exists() {
			// Store as custom parameter for potential use
			out, _ = sjson.SetBytes(out, "top_k", topK.Int())
		}

		// Stop sequences
		if stopSequences := genConfig.Get("stopSequences"); stopSequences.Exists() && stopSequences.IsArray() {
			var stops []string
			stopSequences.ForEach(func(_, value gjson.Result) bool {
				stops = append(stops, value.String())
				return true
			})
			if len(stops) > 0 {
				out, _ = sjson.SetBytes(out, "stop", stops)
			}
		}

		// Candidate count (OpenAI 'n' parameter)
		if candidateCount := genConfig.Get("candidateCount"); candidateCount.Exists() {
			out, _ = sjson.SetBytes(out, "n", candidateCount.Int())
		}

		if responseModalities := genConfig.Get("responseModalities"); responseModalities.Exists() && responseModalities.IsArray() {
			var modalities []string
			responseModalities.ForEach(func(_, value gjson.Result) bool {
				switch strings.ToLower(strings.TrimSpace(value.String())) {
				case "text":
					modalities = append(modalities, "text")
				case "image":
					modalities = append(modalities, "image")
				case "audio":
					modalities = append(modalities, "audio")
				}
				return true
			})
			if len(modalities) > 0 {
				out, _ = sjson.SetBytes(out, "modalities", modalities)
			}
		}

		// Map Gemini thinkingConfig to OpenAI reasoning_effort.
		// Always perform conversion to support allowCompat models that may not be in registry.
		// Note: Google official Python SDK sends snake_case fields (thinking_level/thinking_budget).
		if thinkingConfig := genConfig.Get("thinkingConfig"); thinkingConfig.Exists() && thinkingConfig.IsObject() {
			thinkingLevel := thinkingConfig.Get("thinkingLevel")
			if !thinkingLevel.Exists() {
				thinkingLevel = thinkingConfig.Get("thinking_level")
			}
			if thinkingLevel.Exists() {
				effort := strings.ToLower(strings.TrimSpace(thinkingLevel.String()))
				if effort != "" {
					out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
				}
			} else {
				thinkingBudget := thinkingConfig.Get("thinkingBudget")
				if !thinkingBudget.Exists() {
					thinkingBudget = thinkingConfig.Get("thinking_budget")
				}
				if thinkingBudget.Exists() {
					if effort, ok := thinking.ConvertBudgetToLevel(int(thinkingBudget.Int())); ok {
						out, _ = sjson.SetBytes(out, "reasoning_effort", effort)
					}
				}
			}
		}
	}

	// Stream parameter
	out, _ = sjson.SetBytes(out, "stream", stream)
	if serviceTier := root.Get("service_tier"); serviceTier.Exists() && serviceTier.Type == gjson.String {
		out, _ = sjson.SetBytes(out, "service_tier", serviceTier.String())
	}

	// Process contents (Gemini messages) -> OpenAI messages
	messageCapacity := root.Get("contents.#").Int()
	if root.Get("systemInstruction").Exists() || root.Get("system_instruction").Exists() {
		messageCapacity++
	}
	messageItems := translatorcommon.NewRawArrayItems(messageCapacity)
	toolCallIDsByName := make(map[string][]string)

	// System instruction -> OpenAI system message
	// Gemini may provide `systemInstruction` or `system_instruction`; support both keys.
	systemInstruction := root.Get("systemInstruction")
	if !systemInstruction.Exists() {
		systemInstruction = root.Get("system_instruction")
	}
	if systemInstruction.Exists() {
		parts := systemInstruction.Get("parts")
		contentItems := make([][]byte, 0, 2)

		if parts.Exists() && parts.IsArray() {
			parts.ForEach(func(_, part gjson.Result) bool {
				if translatorcommon.IsGeminiThoughtPart(part) {
					return true
				}

				// Handle text parts
				if text := part.Get("text"); text.Exists() {
					contentPart := []byte(`{"type":"text","text":""}`)
					contentPart, _ = sjson.SetBytes(contentPart, "text", text.String())
					contentItems = append(contentItems, contentPart)
				}

				// Handle inline data (e.g., images)
				if contentPart, ok := openAIContentPartFromGeminiInlineData(part); ok {
					contentItems = append(contentItems, contentPart)
				}
				if contentPart, ok := openAIContentPartFromGeminiFileData(part); ok {
					contentItems = append(contentItems, contentPart)
				}
				return true
			})
		}

		if len(contentItems) > 0 {
			msg := []byte(`{"role":"system","content":[]}`)
			msg, _ = sjson.SetRawBytes(msg, "content", translatorcommon.JoinRawArray(contentItems))
			messageItems = append(messageItems, msg)
		}
	}

	if contents := root.Get("contents"); contents.Exists() && contents.IsArray() {
		msgIdx := 0
		// OpenAI tool messages cannot carry image/file parts, and parallel tool
		// messages must stay contiguous. Media from functionResponse.parts and user
		// content sent before the batch is fully answered wait in these buffers.
		pendingTurnCallCount := 0
		var pendingToolRelayMedia []openAIGeminiToolRelayGroup
		var pendingUserContentItems [][]byte
		var pendingUserTextBuilder strings.Builder
		pendingUserOnlyText := true
		resetPendingUser := func() {
			pendingToolRelayMedia = nil
			pendingUserContentItems = nil
			pendingUserTextBuilder.Reset()
			pendingUserOnlyText = true
		}
		flushPendingUserAndRelay := func() {
			if len(pendingToolRelayMedia) == 0 && len(pendingUserContentItems) == 0 {
				return
			}
			relayMsg := []byte(`{"role":"user","content":""}`)
			switch {
			case len(pendingToolRelayMedia) > 0:
				relayItems := buildOpenAIGeminiToolRelayItems(pendingToolRelayMedia)
				relayItems = append(relayItems, pendingUserContentItems...)
				relayMsg, _ = sjson.SetRawBytes(relayMsg, "content", translatorcommon.JoinRawArray(relayItems))
			case pendingUserOnlyText:
				relayMsg, _ = sjson.SetBytes(relayMsg, "content", pendingUserTextBuilder.String())
			default:
				relayMsg, _ = sjson.SetRawBytes(relayMsg, "content", translatorcommon.JoinRawArray(pendingUserContentItems))
			}
			messageItems = append(messageItems, relayMsg)
			resetPendingUser()
		}
		contents.ForEach(func(_, content gjson.Result) bool {
			parts := content.Get("parts")
			role := translatorcommon.GeminiMessageRole(content.Get("role").String(), parts)
			hasTurnFR := translatorcommon.ContentHasGeminiFunctionResponse([]byte(content.Raw))
			if !hasTurnFR && (len(pendingToolRelayMedia) > 0 || len(pendingUserContentItems) > 0) {
				pendingTurnCallCount = 0
				if role != "user" {
					flushPendingUserAndRelay()
				}
			}
			if role == "assistant" && !hasTurnFR {
				pendingTurnCallCount = 0
			}

			msg := []byte(`{"role":"","content":""}`)
			msg, _ = sjson.SetBytes(msg, "role", role)

			var textBuilder strings.Builder
			contentItems := make([][]byte, 0, 4)
			onlyTextContent := true
			toolCallItems := make([][]byte, 0, 2)
			droppedThought := false
			hasFunctionResponse := false

			if parts.Exists() && parts.IsArray() {
				partIdx := 0
				parts.ForEach(func(_, part gjson.Result) bool {
					currentPartIdx := partIdx
					partIdx++

					if translatorcommon.IsGeminiThoughtPart(part) {
						droppedThought = true
						return true
					}

					// Handle text parts
					if text := part.Get("text"); text.Exists() {
						formattedText := text.String()
						textBuilder.WriteString(formattedText)
						contentPart := []byte(`{"type":"text","text":""}`)
						contentPart, _ = sjson.SetBytes(contentPart, "text", formattedText)
						contentItems = append(contentItems, contentPart)
					}

					// Handle inline data (e.g., images)
					if contentPart, ok := openAIContentPartFromGeminiInlineData(part); ok {
						onlyTextContent = false
						contentItems = append(contentItems, contentPart)
					}
					if contentPart, ok := openAIContentPartFromGeminiFileData(part); ok {
						onlyTextContent = false
						contentItems = append(contentItems, contentPart)
					}

					// Handle function calls (Gemini) -> tool calls (OpenAI)
					if functionCall := translatorcommon.GeminiPartFunctionCall(part); functionCall.Exists() {
						funcName := functionCall.Get("name").String()
						argsRaw := ""
						if args := functionCall.Get("args"); args.Exists() {
							argsRaw = args.Raw
						}
						toolCallID := explicitGeminiToolID(functionCall)
						if toolCallID == "" {
							toolCallID = deterministicToolCallID("call", msgIdx, currentPartIdx, funcName, argsRaw)
						}
						toolCallIDsByName[funcName] = append(toolCallIDsByName[funcName], toolCallID)

						toolCall := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)
						toolCall, _ = sjson.SetBytes(toolCall, "id", toolCallID)
						toolCall, _ = sjson.SetBytes(toolCall, "function.name", funcName)

						// Convert args to arguments JSON string
						if argsRaw != "" {
							toolCall, _ = sjson.SetBytes(toolCall, "function.arguments", argsRaw)
						} else {
							toolCall, _ = sjson.SetBytes(toolCall, "function.arguments", "{}")
						}
						if thoughtSig := geminiFunctionCallThoughtSignature(part, functionCall); thoughtSig != "" {
							toolCall, _ = sjson.SetBytes(toolCall, "extra_content.google.thought_signature", thoughtSig)
						}

						toolCallItems = append(toolCallItems, toolCall)
						pendingTurnCallCount++
					}

					// Handle function responses (Gemini) -> tool role messages (OpenAI)
					if functionResponse := translatorcommon.GeminiPartFunctionResponse(part); functionResponse.Exists() {
						if pendingTurnCallCount > 0 {
							pendingTurnCallCount--
						}
						funcName := functionResponse.Get("name").String()
						// Create tool message for function response
						toolMsg := []byte(`{"role":"tool","tool_call_id":"","content":""}`)

						responseRaw := ""
						responseNonEmpty := false
						// Convert response.content to JSON string
						if response := functionResponse.Get("response"); response.Exists() {
							if contentField := response.Get("content"); contentField.Exists() {
								responseRaw = contentField.Raw
								responseNonEmpty = !isEmptyGeminiOpenAIResponseValue(contentField)
							} else {
								responseRaw = response.Raw
								responseNonEmpty = !isEmptyGeminiOpenAIResponseValue(response)
							}
						}

						var frTextParts []string
						var thisResponseMedia [][]byte
						functionResponse.Get("parts").ForEach(func(_, nested gjson.Result) bool {
							if text := nested.Get("text"); text.Exists() {
								if strings.TrimSpace(text.String()) != "" {
									frTextParts = append(frTextParts, text.String())
								}
								return true
							}
							if contentPart, ok := openAIContentPartFromGeminiInlineData(nested); ok {
								thisResponseMedia = append(thisResponseMedia, contentPart)
							} else if contentPart, ok := openAIContentPartFromGeminiFileData(nested); ok {
								thisResponseMedia = append(thisResponseMedia, contentPart)
							}
							return true
						})
						toolContent := responseRaw
						if len(thisResponseMedia) > 0 || len(frTextParts) > 0 {
							var textSegments []string
							if responseNonEmpty {
								textSegments = append(textSegments, responseRaw)
							}
							textSegments = append(textSegments, frTextParts...)
							if joined := strings.Join(textSegments, "\n\n"); strings.TrimSpace(joined) != "" {
								toolContent = joined
							} else if len(thisResponseMedia) > 0 {
								toolContent = geminiToolResultRelayPlaceholder(thisResponseMedia)
							}
						}
						if toolContent != "" || functionResponse.Get("response").Exists() {
							toolMsg, _ = sjson.SetBytes(toolMsg, "content", toolContent)
						}

						if toolCallID := explicitGeminiToolID(functionResponse); toolCallID != "" {
							toolMsg, _ = sjson.SetBytes(toolMsg, "tool_call_id", toolCallID)
							if queue := toolCallIDsByName[funcName]; len(queue) > 0 {
								for i, id := range queue {
									if id == toolCallID {
										toolCallIDsByName[funcName] = append(queue[:i], queue[i+1:]...)
										break
									}
								}
							}
						} else if queue := toolCallIDsByName[funcName]; len(queue) > 0 {
							toolCallID := queue[0]
							toolCallIDsByName[funcName] = queue[1:]
							toolMsg, _ = sjson.SetBytes(toolMsg, "tool_call_id", toolCallID)
						} else {
							fallbackID := deterministicToolCallID("response", msgIdx, currentPartIdx, funcName, responseRaw)
							toolMsg, _ = sjson.SetBytes(toolMsg, "tool_call_id", fallbackID)
						}

						if len(thisResponseMedia) > 0 {
							pendingToolRelayMedia = append(pendingToolRelayMedia, openAIGeminiToolRelayGroup{
								toolName: funcName,
								callID:   gjson.GetBytes(toolMsg, "tool_call_id").String(),
								items:    thisResponseMedia,
							})
						}

						messageItems = append(messageItems, toolMsg)
						hasFunctionResponse = true
					}

					return true
				})
			}

			if hasTurnFR && pendingTurnCallCount > 0 {
				// The batch is still being answered: hold user content behind the tool messages.
				if role == "user" && len(contentItems) > 0 {
					pendingUserContentItems = append(pendingUserContentItems, contentItems...)
					pendingUserTextBuilder.WriteString(textBuilder.String())
					pendingUserOnlyText = pendingUserOnlyText && onlyTextContent
					contentItems = contentItems[:0]
					textBuilder.Reset()
				}
			} else if len(pendingToolRelayMedia) > 0 || len(pendingUserContentItems) > 0 {
				if role == "user" && len(contentItems) > 0 {
					var prefixItems [][]byte
					if len(pendingToolRelayMedia) > 0 {
						prefixItems = buildOpenAIGeminiToolRelayItems(pendingToolRelayMedia)
						onlyTextContent = false
					}
					if len(pendingUserContentItems) > 0 {
						prefixItems = append(prefixItems, pendingUserContentItems...)
						if len(pendingToolRelayMedia) == 0 && pendingUserOnlyText && onlyTextContent {
							combined := pendingUserTextBuilder.String() + textBuilder.String()
							textBuilder.Reset()
							textBuilder.WriteString(combined)
						} else {
							onlyTextContent = false
						}
					}
					contentItems = append(prefixItems, contentItems...)
					resetPendingUser()
				} else {
					flushPendingUserAndRelay()
				}
			}

			// Set content
			if len(contentItems) > 0 {
				if onlyTextContent {
					msg, _ = sjson.SetBytes(msg, "content", textBuilder.String())
				} else {
					msg, _ = sjson.SetRawBytes(msg, "content", translatorcommon.JoinRawArray(contentItems))
				}
			}

			// Set tool calls if any.
			if len(toolCallItems) > 0 {
				msg, _ = sjson.SetRawBytes(msg, "tool_calls", translatorcommon.JoinRawArray(toolCallItems))
			}

			if (droppedThought || hasFunctionResponse) && len(contentItems) == 0 && len(toolCallItems) == 0 {
				msgIdx++
				return true
			}

			messageItems = append(messageItems, msg)
			msgIdx++
			return true
		})
		flushPendingUserAndRelay()
	}
	out = translatorcommon.SetRawArrayItems(out, "messages", messageItems)

	// Tools mapping: Gemini tools -> OpenAI tools
	if tools := root.Get("tools"); tools.Exists() && tools.IsArray() {
		var toolItems [][]byte
		tools.ForEach(func(_, tool gjson.Result) bool {
			if functionDeclarations := tool.Get("functionDeclarations"); functionDeclarations.Exists() && functionDeclarations.IsArray() {
				functionDeclarations.ForEach(func(_, funcDecl gjson.Result) bool {
					openAITool := []byte(`{"type":"function","function":{"name":"","description":""}}`)
					openAITool, _ = sjson.SetBytes(openAITool, "function.name", funcDecl.Get("name").String())
					openAITool, _ = sjson.SetBytes(openAITool, "function.description", funcDecl.Get("description").String())

					// Convert parameters schema
					if parameters := funcDecl.Get("parameters"); parameters.Exists() {
						normalizedParams := util.NormalizeJSONSchemaNumericConstraints(parameters.Raw)
						openAITool, _ = sjson.SetRawBytes(openAITool, "function.parameters", []byte(normalizedParams))
					} else if parameters := funcDecl.Get("parametersJsonSchema"); parameters.Exists() {
						normalizedParams := util.NormalizeJSONSchemaNumericConstraints(parameters.Raw)
						openAITool, _ = sjson.SetRawBytes(openAITool, "function.parameters", []byte(normalizedParams))
					}

					toolItems = append(toolItems, openAITool)
					return true
				})
			}
			return true
		})
		if len(toolItems) > 0 {
			out, _ = sjson.SetRawBytes(out, "tools", translatorcommon.JoinRawArray(toolItems))
		}
	}

	// Tool choice mapping (Gemini doesn't have direct equivalent, but we can handle it)
	if toolConfig := root.Get("toolConfig"); toolConfig.Exists() {
		if functionCallingConfig := toolConfig.Get("functionCallingConfig"); functionCallingConfig.Exists() {
			mode := functionCallingConfig.Get("mode").String()
			allowedNames := functionCallingConfig.Get("allowedFunctionNames")
			switch mode {
			case "NONE":
				out, _ = sjson.SetBytes(out, "tool_choice", "none")
			case "AUTO":
				out, _ = sjson.SetBytes(out, "tool_choice", "auto")
			case "ANY":
				allowedNameItems := allowedNames.Array()
				if allowedNames.IsArray() && len(allowedNameItems) == 1 {
					choice := []byte(`{"type":"function","function":{"name":""}}`)
					choice, _ = sjson.SetBytes(choice, "function.name", allowedNameItems[0].String())
					out, _ = sjson.SetRawBytes(out, "tool_choice", choice)
				} else {
					out, _ = sjson.SetBytes(out, "tool_choice", "required")
				}
			}
		}
	}

	return out, nil
}

func deterministicToolCallID(kind string, msgIdx, partIdx int, name, payload string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%s|%s", kind, msgIdx, partIdx, name, payload)))
	return "call_" + hex.EncodeToString(sum[:12])
}

func geminiFunctionCallThoughtSignature(part, functionCall gjson.Result) string {
	for _, sig := range []gjson.Result{
		part.Get("thoughtSignature"),
		part.Get("thought_signature"),
		functionCall.Get("thoughtSignature"),
		functionCall.Get("thought_signature"),
		part.Get("extra_content.google.thought_signature"),
	} {
		if sig.Exists() && sig.String() != "" {
			return sig.String()
		}
	}
	return ""
}

func explicitGeminiToolID(node gjson.Result) string {
	if id := strings.TrimSpace(node.Get("id").String()); id != "" {
		return id
	}
	if callID := strings.TrimSpace(node.Get("call_id").String()); callID != "" {
		return callID
	}
	return strings.TrimSpace(node.Get("callId").String())
}

func openAIContentPartFromGeminiInlineData(part gjson.Result) ([]byte, bool) {
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
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}
	data := inlineData.Get("data").String()
	if data == "" {
		return nil, false
	}
	dataURL := fmt.Sprintf("data:%s;base64,%s", mimeType, data)
	lowerMimeType := strings.ToLower(mimeType)
	switch {
	case strings.HasPrefix(lowerMimeType, "image/"):
		contentPart := []byte(`{"type":"image_url","image_url":{"url":""}}`)
		contentPart, _ = sjson.SetBytes(contentPart, "image_url.url", dataURL)
		return contentPart, true
	case strings.HasPrefix(lowerMimeType, "audio/"):
		contentPart := []byte(`{"type":"input_audio","input_audio":{"data":"","format":""}}`)
		contentPart, _ = sjson.SetBytes(contentPart, "input_audio.data", data)
		contentPart, _ = sjson.SetBytes(contentPart, "input_audio.format", openAIInputAudioFormatFromMIME(mimeType))
		return contentPart, true
	case strings.HasPrefix(lowerMimeType, "video/"):
		contentPart := []byte(`{"type":"video_url","video_url":{"url":""}}`)
		contentPart, _ = sjson.SetBytes(contentPart, "video_url.url", dataURL)
		return contentPart, true
	default:
		fileData := dataURL
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(data)), "data:") {
			fileData = data
		}
		contentPart := []byte(`{"type":"file","file":{"filename":"","file_data":""}}`)
		contentPart, _ = sjson.SetBytes(contentPart, "file.filename", openAIFileNameFromMIME(mimeType))
		contentPart, _ = sjson.SetBytes(contentPart, "file.file_data", fileData)
		return contentPart, true
	}
}

func openAIContentPartFromGeminiFileData(part gjson.Result) ([]byte, bool) {
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
		contentPart := []byte(`{"type":"image_url","image_url":{"url":""}}`)
		contentPart, _ = sjson.SetBytes(contentPart, "image_url.url", fileURI)
		return contentPart, true
	}
	if strings.HasPrefix(lowerMimeType, "video/") {
		contentPart := []byte(`{"type":"video_url","video_url":{"url":""}}`)
		contentPart, _ = sjson.SetBytes(contentPart, "video_url.url", fileURI)
		return contentPart, true
	}
	if strings.HasPrefix(lowerMimeType, "application/") || strings.HasPrefix(lowerMimeType, "text/") {
		contentPart := []byte(`{"type":"file","file":{"filename":"","file_url":""}}`)
		contentPart, _ = sjson.SetBytes(contentPart, "file.filename", openAIFileNameFromMIME(mimeType))
		contentPart, _ = sjson.SetBytes(contentPart, "file.file_url", fileURI)
		return contentPart, true
	}
	fileInfo := "File: " + fileURI
	if mimeType != "" {
		fileInfo += " (Type: " + mimeType + ")"
	}
	contentPart := []byte(`{"type":"text","text":""}`)
	contentPart, _ = sjson.SetBytes(contentPart, "text", fileInfo)
	return contentPart, true
}

func openAIInputAudioFormatFromMIME(mimeType string) string {
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

func openAIFileNameFromMIME(mimeType string) string {
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

/**
 * @brief 归类转发的工具媒体
 * @param items OpenAI Chat 内容块
 * @return hasImage 含 image_url
 * @return hasFile 含 file、input_audio 或 video_url
 */
func classifyGeminiRelayedToolMedia(items [][]byte) (hasImage, hasFile bool) {
	for _, item := range items {
		switch gjson.GetBytes(item, "type").String() {
		case "image_url":
			hasImage = true
		case "file", "input_audio", "video_url":
			hasFile = true
		}
	}
	return hasImage, hasFile
}

/**
 * @brief 工具只返回媒体时 tool 消息使用的占位文本
 * @param items 随后由用户消息转发的媒体块
 * @return 指明媒体将在下一条用户消息出现的说明
 */
func geminiToolResultRelayPlaceholder(items [][]byte) string {
	hasImage, hasFile := classifyGeminiRelayedToolMedia(items)
	switch {
	case hasFile && !hasImage:
		return "[Tool returned document content; the documents follow in the next user message.]"
	case hasFile && hasImage:
		return "[Tool returned media content; the attachments follow in the next user message.]"
	default:
		return "[Tool returned image content; the images follow in the next user message.]"
	}
}

/**
 * @brief 判断 Gemini functionResponse 的响应值是否没有可转发内容
 * @param v response 或 response.content 字段
 * @return 字段缺失、空对象或空白原文时返回 true
 */
func isEmptyGeminiOpenAIResponseValue(v gjson.Result) bool {
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
 * @brief 一次工具调用返回、需改由用户消息转发的媒体
 */
type openAIGeminiToolRelayGroup struct {
	toolName string   ///< 工具名，用于转发说明中的归属
	callID   string   ///< 已配对的 tool_call_id
	items    [][]byte ///< OpenAI Chat 媒体内容块
}

/**
 * @brief 构建工具媒体转发用户消息的内容块
 * @param groups 按工具消息顺序收集的媒体组
 * @return 首块为归属说明；多组时每组前另加工具标签，避免把媒体误归给其他并行调用
 */
func buildOpenAIGeminiToolRelayItems(groups []openAIGeminiToolRelayGroup) [][]byte {
	textPart := func(text string) []byte {
		part := []byte(`{"type":"text","text":""}`)
		part, _ = sjson.SetBytes(part, "text", text)
		return part
	}
	if len(groups) == 1 {
		g := groups[0]
		kind := geminiToolResultRelayMediaKind(g.items)
		var notice string
		switch {
		case g.toolName != "" && g.callID != "":
			notice = fmt.Sprintf("%s returned by tool %s (%s):", kind, g.toolName, g.callID)
		case g.toolName != "":
			notice = fmt.Sprintf("%s returned by tool %s:", kind, g.toolName)
		case g.callID != "":
			notice = fmt.Sprintf("%s returned by tool call %s:", kind, g.callID)
		default:
			notice = kind + " returned by the preceding tool call(s):"
		}
		relayItems := make([][]byte, 0, len(g.items)+1)
		relayItems = append(relayItems, textPart(notice))
		return append(relayItems, g.items...)
	}
	var allItems [][]byte
	for _, group := range groups {
		allItems = append(allItems, group.items...)
	}
	relayItems := [][]byte{textPart(geminiToolResultRelayMediaKind(allItems) + " returned by the preceding tool call(s):")}
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
		relayItems = append(relayItems, textPart(label))
		relayItems = append(relayItems, group.items...)
	}
	return relayItems
}

/**
 * @brief 转发说明中的媒体类别名
 * @param items 媒体内容块
 * @return Documents、Media 或 Images
 */
func geminiToolResultRelayMediaKind(items [][]byte) string {
	hasImage, hasFile := classifyGeminiRelayedToolMedia(items)
	switch {
	case hasFile && !hasImage:
		return "Documents"
	case hasFile && hasImage:
		return "Media"
	default:
		return "Images"
	}
}
