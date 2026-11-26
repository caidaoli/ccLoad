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
	"slices"
	"strings"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"
	"ccLoad/internal/protocol/cliproxy/thinking"
	"ccLoad/internal/protocol/cliproxy/util"

	"github.com/MoeForge/moego/ojson"
)

// ConvertGeminiRequestToOpenAI parses and transforms a Gemini API request into OpenAI Chat Completions API format.
// It extracts the model name, generation config, message contents, and tool declarations
// from the raw JSON request and returns them in the format expected by the OpenAI API.
func ConvertGeminiRequestToOpenAI(modelName string, inputRawJSON []byte, stream bool) ([]byte, error) {
	rawJSON := inputRawJSON
	// Base OpenAI Chat Completions API template
	out := []byte(`{"model":"","messages":[]}`)

	root := ojson.ParseBytes(rawJSON)

	// Model mapping
	out, _ = ojson.SetBytes(out, "model", modelName)

	genConfig := translatorcommon.GeminiConfigObject(root, "generationConfig", "generation_config")
	format, outputInstruction, err := translatorcommon.GeminiResponseFormat(root, true)
	if err != nil {
		return nil, err
	}
	formatType := ojson.GetBytes(format, "type").Str
	if format != nil {
		responseFormat := format
		if formatType == "json_schema" {
			schemaFormat, _ := ojson.DeleteBytes(format, "type")
			responseFormat, _ = ojson.SetRawBytes([]byte(`{"type":"json_schema"}`), "json_schema", schemaFormat)
		}
		out, _ = ojson.SetRawBytes(out, "response_format", responseFormat)
	}
	if temperature := translatorcommon.GeminiConfigNumber(genConfig, "temperature"); temperature.Exists() {
		out, _ = ojson.SetBytes(out, "temperature", temperature.Float())
	}
	if maxTokens, ok := translatorcommon.GeminiConfigInt(genConfig, 1, "maxOutputTokens", "max_output_tokens"); ok {
		out, _ = ojson.SetBytes(out, "max_tokens", maxTokens)
	}
	if topP := translatorcommon.GeminiConfigNumber(genConfig, "topP", "top_p"); topP.Exists() {
		out, _ = ojson.SetBytes(out, "top_p", topP.Float())
	}
	if topK, ok := translatorcommon.GeminiConfigInt(genConfig, 0, "topK", "top_k"); ok {
		out, _ = ojson.SetBytes(out, "top_k", topK)
	}
	if stops := translatorcommon.GeminiConfigStrings(genConfig, "stopSequences", "stop_sequences"); len(stops) > 0 {
		out, _ = ojson.SetBytes(out, "stop", stops)
	}
	if candidates, ok := translatorcommon.GeminiConfigInt(genConfig, 1, "candidateCount", "candidate_count"); ok {
		out, _ = ojson.SetBytes(out, "n", candidates)
	}
	var modalities []string
	for _, value := range translatorcommon.GeminiConfigStrings(genConfig, "responseModalities", "response_modalities") {
		switch modality := strings.ToLower(strings.TrimSpace(value)); modality {
		case "text", "image", "audio":
			modalities = append(modalities, modality)
		}
	}
	if len(modalities) > 0 {
		out, _ = ojson.SetBytes(out, "modalities", modalities)
	}

	effort, budget, hasBudget := translatorcommon.GeminiThinkingConfig(root)
	if effort == "" && hasBudget {
		effort, _ = thinking.ConvertBudgetToModelLevel(int(budget), modelName, "openai")
	}
	if effort != "" && effort != string(thinking.LevelAuto) {
		out, _ = ojson.SetBytes(out, "reasoning_effort", effort)
	}

	// Stream parameter
	out, _ = ojson.SetBytes(out, "stream", stream)
	if serviceTier := root.Get("service_tier"); serviceTier.Exists() && serviceTier.Type == ojson.String {
		out, _ = ojson.SetBytes(out, "service_tier", serviceTier.String())
	}

	// Process contents (Gemini messages) -> OpenAI messages
	messageCapacity := root.Get("contents.#").Int()
	messageItems := translatorcommon.NewRawArrayItems(messageCapacity)
	toolCallIDsByName := make(map[string][]string)

	// System instruction -> OpenAI system message
	// Gemini may provide `systemInstruction` or `system_instruction`; support both keys.
	for _, key := range [...]string{"systemInstruction", "system_instruction"} {
		parts := root.Get(key + ".parts")
		contentItems := make([][]byte, 0, 2)

		if parts.Exists() && parts.IsArray() {
			parts.ForEach(func(_, part ojson.Result) bool {
				if translatorcommon.IsGeminiThoughtPart(part) {
					return true
				}

				// Handle text parts
				if text := part.Get("text"); strings.TrimSpace(text.Str) != "" {
					contentPart := []byte(`{"type":"text","text":""}`)
					contentPart, _ = ojson.SetBytes(contentPart, "text", text.Str)
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
			msg, _ = ojson.SetRawBytes(msg, "content", translatorcommon.JoinRawArray(contentItems))
			messageItems = append(messageItems, msg)
			break
		}
	}
	if formatType == "json_object" {
		outputInstruction = translatorcommon.GeminiJSONObjectInstruction
	}
	if outputInstruction != "" {
		msg := []byte(`{"role":"system","content":""}`)
		msg, _ = ojson.SetBytes(msg, "content", outputInstruction)
		messageItems = append(messageItems, msg)
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
				relayMsg, _ = ojson.SetRawBytes(relayMsg, "content", translatorcommon.JoinRawArray(relayItems))
			case pendingUserOnlyText:
				relayMsg, _ = ojson.SetBytes(relayMsg, "content", pendingUserTextBuilder.String())
			default:
				relayMsg, _ = ojson.SetRawBytes(relayMsg, "content", translatorcommon.JoinRawArray(pendingUserContentItems))
			}
			messageItems = append(messageItems, relayMsg)
			resetPendingUser()
		}
		contents.ForEach(func(_, content ojson.Result) bool {
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
			msg, _ = ojson.SetBytes(msg, "role", role)

			var textBuilder strings.Builder
			contentItems := make([][]byte, 0, 4)
			onlyTextContent := true
			toolCallItems := make([][]byte, 0, 2)
			droppedThought := false
			hasFunctionResponse := false

			if parts.Exists() && parts.IsArray() {
				partIdx := 0
				parts.ForEach(func(_, part ojson.Result) bool {
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
						contentPart, _ = ojson.SetBytes(contentPart, "text", formattedText)
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
						args := functionCall.Get("args")
						toolCallID := explicitGeminiToolID(functionCall)
						if toolCallID == "" {
							// Use the original args to keep previously generated IDs stable.
							toolCallID = deterministicToolCallID("call", msgIdx, currentPartIdx, funcName, args.Raw)
						}
						toolCallIDsByName[funcName] = append(toolCallIDsByName[funcName], toolCallID)

						toolCall := []byte(`{"id":"","type":"function","function":{"name":"","arguments":"{}"}}`)
						toolCall, _ = ojson.SetBytes(toolCall, "id", toolCallID)
						toolCall, _ = ojson.SetBytes(toolCall, "function.name", funcName)

						if args.IsObject() {
							toolCall, _ = ojson.SetBytes(toolCall, "function.arguments", args.Raw)
						}
						if thoughtSig := geminiFunctionCallThoughtSignature(part, functionCall); thoughtSig != "" {
							toolCall, _ = ojson.SetBytes(toolCall, "extra_content.google.thought_signature", thoughtSig)
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
						functionResponse.Get("parts").ForEach(func(_, nested ojson.Result) bool {
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
							toolMsg, _ = ojson.SetBytes(toolMsg, "content", toolContent)
						}

						if toolCallID := explicitGeminiToolID(functionResponse); toolCallID != "" {
							toolMsg, _ = ojson.SetBytes(toolMsg, "tool_call_id", toolCallID)
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
							toolMsg, _ = ojson.SetBytes(toolMsg, "tool_call_id", toolCallID)
						} else {
							fallbackID := deterministicToolCallID("response", msgIdx, currentPartIdx, funcName, responseRaw)
							toolMsg, _ = ojson.SetBytes(toolMsg, "tool_call_id", fallbackID)
						}

						if len(thisResponseMedia) > 0 {
							pendingToolRelayMedia = append(pendingToolRelayMedia, openAIGeminiToolRelayGroup{
								toolName: funcName,
								callID:   ojson.GetBytes(toolMsg, "tool_call_id").String(),
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
					msg, _ = ojson.SetBytes(msg, "content", textBuilder.String())
				} else {
					msg, _ = ojson.SetRawBytes(msg, "content", translatorcommon.JoinRawArray(contentItems))
				}
			}

			// Set tool calls if any.
			if len(toolCallItems) > 0 {
				msg, _ = ojson.SetRawBytes(msg, "tool_calls", translatorcommon.JoinRawArray(toolCallItems))
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

	declarations := translatorcommon.GeminiFunctionDeclarations(root)
	mode, allowedNames, err := translatorcommon.GeminiFunctionCallingOptions(root, declarations)
	if err != nil {
		return nil, err
	}
	var tools [][]byte
	for _, declaration := range declarations {
		name := declaration.Get("name").String()
		if len(allowedNames) > 1 && !slices.Contains(allowedNames, name) {
			continue
		}
		tool := []byte(`{"type":"function","function":{"name":"","description":""}}`)
		tool, _ = ojson.SetBytes(tool, "function.name", name)
		tool, _ = ojson.SetBytes(tool, "function.description", declaration.Get("description").String())
		if parameters := translatorcommon.GeminiConfigObject(declaration, "parameters", "parametersJsonSchema", "parameters_json_schema"); parameters.Exists() {
			tool, _ = ojson.SetRawBytes(tool, "function.parameters", []byte(util.NormalizeGeminiToolSchema(parameters.Raw)))
		}
		tools = append(tools, tool)
	}
	if len(tools) > 0 {
		out, _ = ojson.SetRawBytes(out, "tools", translatorcommon.JoinRawArray(tools))
	}

	switch mode {
	case "NONE":
		out, _ = ojson.SetBytes(out, "tool_choice", "none")
	case "AUTO":
		out, _ = ojson.SetBytes(out, "tool_choice", "auto")
	case "ANY":
		if len(allowedNames) == 1 {
			choice := []byte(`{"type":"function","function":{"name":""}}`)
			choice, _ = ojson.SetBytes(choice, "function.name", allowedNames[0])
			out, _ = ojson.SetRawBytes(out, "tool_choice", choice)
		} else {
			out, _ = ojson.SetBytes(out, "tool_choice", "required")
		}
	}

	return out, nil
}

func deterministicToolCallID(kind string, msgIdx, partIdx int, name, payload string) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%d|%s|%s", kind, msgIdx, partIdx, name, payload)))
	return "call_" + hex.EncodeToString(sum[:12])
}

func geminiFunctionCallThoughtSignature(part, functionCall ojson.Result) string {
	for _, sig := range []ojson.Result{
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

func explicitGeminiToolID(node ojson.Result) string {
	if id := strings.TrimSpace(node.Get("id").String()); id != "" {
		return id
	}
	if callID := strings.TrimSpace(node.Get("call_id").String()); callID != "" {
		return callID
	}
	return strings.TrimSpace(node.Get("callId").String())
}

func openAIContentPartFromGeminiInlineData(part ojson.Result) ([]byte, bool) {
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
		contentPart, _ = ojson.SetBytes(contentPart, "image_url.url", dataURL)
		return contentPart, true
	case strings.HasPrefix(lowerMimeType, "audio/"):
		contentPart := []byte(`{"type":"input_audio","input_audio":{"data":"","format":""}}`)
		contentPart, _ = ojson.SetBytes(contentPart, "input_audio.data", data)
		contentPart, _ = ojson.SetBytes(contentPart, "input_audio.format", openAIInputAudioFormatFromMIME(mimeType))
		return contentPart, true
	case strings.HasPrefix(lowerMimeType, "video/"):
		contentPart := []byte(`{"type":"video_url","video_url":{"url":""}}`)
		contentPart, _ = ojson.SetBytes(contentPart, "video_url.url", dataURL)
		return contentPart, true
	default:
		fileData := dataURL
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(data)), "data:") {
			fileData = data
		}
		contentPart := []byte(`{"type":"file","file":{"filename":"","file_data":""}}`)
		contentPart, _ = ojson.SetBytes(contentPart, "file.filename", openAIFileNameFromMIME(mimeType))
		contentPart, _ = ojson.SetBytes(contentPart, "file.file_data", fileData)
		return contentPart, true
	}
}

func openAIContentPartFromGeminiFileData(part ojson.Result) ([]byte, bool) {
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
		contentPart, _ = ojson.SetBytes(contentPart, "image_url.url", fileURI)
		return contentPart, true
	}
	if strings.HasPrefix(lowerMimeType, "video/") {
		contentPart := []byte(`{"type":"video_url","video_url":{"url":""}}`)
		contentPart, _ = ojson.SetBytes(contentPart, "video_url.url", fileURI)
		return contentPart, true
	}
	if strings.HasPrefix(lowerMimeType, "application/") || strings.HasPrefix(lowerMimeType, "text/") {
		contentPart := []byte(`{"type":"file","file":{"filename":"","file_url":""}}`)
		contentPart, _ = ojson.SetBytes(contentPart, "file.filename", openAIFileNameFromMIME(mimeType))
		contentPart, _ = ojson.SetBytes(contentPart, "file.file_url", fileURI)
		return contentPart, true
	}
	fileInfo := "File: " + fileURI
	if mimeType != "" {
		fileInfo += " (Type: " + mimeType + ")"
	}
	contentPart := []byte(`{"type":"text","text":""}`)
	contentPart, _ = ojson.SetBytes(contentPart, "text", fileInfo)
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
		switch ojson.GetBytes(item, "type").String() {
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
func isEmptyGeminiOpenAIResponseValue(v ojson.Result) bool {
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
		part, _ = ojson.SetBytes(part, "text", text)
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
