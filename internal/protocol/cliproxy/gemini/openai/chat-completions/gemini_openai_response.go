// Package openai provides response translation functionality for Gemini to OpenAI API compatibility.
// This package handles the conversion of Gemini API responses into OpenAI Chat Completions-compatible
// JSON format, transforming streaming events and non-streaming responses into the format
// expected by OpenAI API clients. It supports both streaming and non-streaming modes,
// handling text content, tool calls, reasoning content, and usage metadata appropriately.
package chat_completions

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	translatorcommon "ccLoad/internal/protocol/cliproxy/common"
	"ccLoad/internal/protocol/cliproxy/util"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// convertGeminiResponseToOpenAIChatParams holds parameters for response conversion.
type convertGeminiResponseToOpenAIChatParams struct {
	translatorcommon.ApplyPatchErrorState
	UnixTimestamp int64
	// FunctionIndex tracks tool call indices per candidate index to support multiple candidates.
	FunctionIndex            map[int]int
	SawToolCall              map[int]bool
	SawFinishReason          map[int]bool
	SawDone                  bool
	UpstreamFinishReason     map[int]string
	ModelVersion             string
	ResponseID               string
	PendingUsageMetadata     []byte
	SanitizedNameMap         map[string]string
	SanitizedNameMapResolved bool
	ActiveToolCall           map[int]*translatorcommon.VertexStreamingToolCall
	HasIncompleteToolCall    map[int]bool
}

// functionCallIDCounter provides a process-wide unique counter for function call identifiers.
var functionCallIDCounter uint64

// ConvertGeminiResponseToOpenAI translates a single chunk of a streaming response from the
// Gemini API format to the OpenAI Chat Completions streaming format.
// It processes various Gemini event types and transforms them into OpenAI-compatible JSON responses.
// The function handles text content, tool calls, reasoning content, and usage metadata, outputting
// responses that match the OpenAI API format. It supports incremental updates for streaming responses.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Gemini API
//   - param: A pointer to a parameter object for maintaining state between calls
//
// Returns:
//   - [][]byte: A slice of OpenAI-compatible JSON responses
func ConvertGeminiResponseToOpenAI(_ context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	if param == nil {
		var local any
		param = &local
	}
	// Initialize parameters if nil.
	if *param == nil {
		*param = &convertGeminiResponseToOpenAIChatParams{
			UnixTimestamp:            0,
			FunctionIndex:            make(map[int]int),
			SawToolCall:              make(map[int]bool),
			SawFinishReason:          make(map[int]bool),
			UpstreamFinishReason:     make(map[int]string),
			SanitizedNameMap:         util.SanitizedToolNameMap(originalRequestRawJSON),
			SanitizedNameMapResolved: true,
		}
	}

	// Ensure the Map is initialized (handling cases where param might be reused from older context).
	p := (*param).(*convertGeminiResponseToOpenAIChatParams)
	if p.FunctionIndex == nil {
		p.FunctionIndex = make(map[int]int)
	}
	if p.SawToolCall == nil {
		p.SawToolCall = make(map[int]bool)
	}
	if p.SawFinishReason == nil {
		p.SawFinishReason = make(map[int]bool)
	}
	if p.UpstreamFinishReason == nil {
		p.UpstreamFinishReason = make(map[int]string)
	}
	if p.ActiveToolCall == nil {
		p.ActiveToolCall = make(map[int]*translatorcommon.VertexStreamingToolCall)
	}
	if p.HasIncompleteToolCall == nil {
		p.HasIncompleteToolCall = make(map[int]bool)
	}
	if !p.SanitizedNameMapResolved {
		if p.SanitizedNameMap == nil {
			p.SanitizedNameMap = util.SanitizedToolNameMap(originalRequestRawJSON)
		}
		p.SanitizedNameMapResolved = true
	}

	if p.ToolInputError() != nil {
		return nil
	}

	if bytes.HasPrefix(rawJSON, []byte("data:")) {
		rawJSON = bytes.TrimSpace(rawJSON[5:])
	}

	if bytes.Equal(rawJSON, []byte("[DONE]")) {
		if p.SawDone {
			return nil
		}
		p.FinalizeToolInput()
		if p.ToolInputError() != nil {
			return nil
		}
		p.SawDone = true
		var indices []int
		for idx, reason := range p.UpstreamFinishReason {
			if reason != "" && !p.SawFinishReason[idx] && p.ActiveToolCall[idx] == nil && !p.HasIncompleteToolCall[idx] {
				indices = append(indices, idx)
			}
		}
		sort.Ints(indices)
		var doneChunks [][]byte
		for _, idx := range indices {
			p.SawFinishReason[idx] = true
			finishReason, nativeFinishReason := resolveGeminiOpenAIFinishReason(p.UpstreamFinishReason[idx], p.SawToolCall[idx])
			template := []byte(`{"id":"","object":"chat.completion.chunk","created":0,"model":"model","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":null,"native_finish_reason":null}]}`)
			template, _ = sjson.SetBytes(template, "created", p.UnixTimestamp)
			if p.ModelVersion != "" {
				template, _ = sjson.SetBytes(template, "model", p.ModelVersion)
			}
			if p.ResponseID != "" {
				template, _ = sjson.SetBytes(template, "id", p.ResponseID)
			}
			if len(p.PendingUsageMetadata) > 0 {
				template = applyGeminiOpenAIUsageMetadata(template, gjson.ParseBytes(p.PendingUsageMetadata))
			}
			template, _ = sjson.SetBytes(template, "choices.0.index", idx)
			template, _ = sjson.SetBytes(template, "choices.0.finish_reason", finishReason)
			template, _ = sjson.SetBytes(template, "choices.0.native_finish_reason", nativeFinishReason)
			doneChunks = append(doneChunks, template)
		}
		return append(doneChunks, []byte("[DONE]"))
	}

	// Initialize the OpenAI SSE base template.
	// We use a base template and clone it for each candidate to support multiple candidates.
	baseTemplate := []byte(`{"id":"chatcmpl-proxy","object":"chat.completion.chunk","created":0,"model":"","choices":[{"index":0,"delta":{"role":null,"content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":null,"native_finish_reason":null}]}`)
	baseTemplate, _ = sjson.SetBytes(baseTemplate, "model", modelName)

	// Extract and set the model version.
	if modelName != "" {
		p.ModelVersion = modelName
	} else if modelVersionResult := gjson.GetBytes(rawJSON, "modelVersion"); modelVersionResult.Exists() {
		p.ModelVersion = modelVersionResult.String()
	}
	baseTemplate, _ = sjson.SetBytes(baseTemplate, "model", p.ModelVersion)

	// Extract and set the creation timestamp.
	if createTimeResult := gjson.GetBytes(rawJSON, "createTime"); createTimeResult.Exists() {
		t, err := time.Parse(time.RFC3339Nano, createTimeResult.String())
		if err == nil {
			p.UnixTimestamp = t.Unix()
		}
		baseTemplate, _ = sjson.SetBytes(baseTemplate, "created", p.UnixTimestamp)
	} else {
		baseTemplate, _ = sjson.SetBytes(baseTemplate, "created", p.UnixTimestamp)
	}

	// Extract and set the response ID.
	if responseIDResult := gjson.GetBytes(rawJSON, "responseId"); responseIDResult.Exists() {
		p.ResponseID = responseIDResult.String()
		baseTemplate, _ = sjson.SetBytes(baseTemplate, "id", p.ResponseID)
	} else if p.ResponseID != "" {
		baseTemplate, _ = sjson.SetBytes(baseTemplate, "id", p.ResponseID)
	}

	// Extract and set usage metadata (token counts).
	// Usage is applied to the base template so it appears in the chunks.
	if usageResult := gjson.GetBytes(rawJSON, "usageMetadata"); usageResult.Exists() {
		baseTemplate = applyGeminiOpenAIUsageMetadata(baseTemplate, usageResult)
		p.PendingUsageMetadata = nil
	} else if pendingUsage := gjson.GetBytes(rawJSON, "cpaUsageMetadata"); pendingUsage.Exists() {
		p.PendingUsageMetadata = append(p.PendingUsageMetadata[:0], pendingUsage.Raw...)
	}

	var responseStrings [][]byte
	candidates := gjson.GetBytes(rawJSON, "candidates")

	// Iterate over all candidates to support candidate_count > 1.
	if candidates.IsArray() {
		candidates.ForEach(func(_, candidate gjson.Result) bool {
			// Clone the template for the current candidate.
			template := append([]byte(nil), baseTemplate...)

			// Set the specific index for this candidate.
			candidateIndex := int(candidate.Get("index").Int())
			template, _ = sjson.SetBytes(template, "choices.0.index", candidateIndex)

			if finishReasonResult := candidate.Get("finishReason"); finishReasonResult.Exists() {
				p.UpstreamFinishReason[candidateIndex] = strings.ToUpper(finishReasonResult.String())
			}

			partsResult := candidate.Get("content.parts")
			assistantRoleSet := false
			sawNoopFunctionCall := false
			sawCompleteFunctionCall := false
			setAssistantRole := func() {
				if assistantRoleSet {
					return
				}
				template, _ = sjson.SetBytes(template, "choices.0.delta.role", "assistant")
				assistantRoleSet = true
			}

			if partsResult.IsArray() {
				partResults := partsResult.Array()
				for i := 0; i < len(partResults); i++ {
					partResult := partResults[i]
					partTextResult := partResult.Get("text")
					functionCallResult := partResult.Get("functionCall")
					inlineDataResult := partResult.Get("inlineData")
					if !inlineDataResult.Exists() {
						inlineDataResult = partResult.Get("inline_data")
					}
					thoughtSignatureResult := partResult.Get("thoughtSignature")
					if !thoughtSignatureResult.Exists() {
						thoughtSignatureResult = partResult.Get("thought_signature")
					}

					// Speech-to-text models (gemini-3.5-transcribe) deliver the
					// transcript in an audioTranscription part instead of text.
					audioTranscriptionResult := partResult.Get("audioTranscription")
					if audioTranscriptionResult.Exists() && !partTextResult.Exists() {
						partTextResult = audioTranscriptionResult.Get("text")
					}

					hasThoughtSignature := thoughtSignatureResult.Exists() && thoughtSignatureResult.String() != ""
					hasContentPayload := partTextResult.Exists() || functionCallResult.Exists() || inlineDataResult.Exists()

					if active := p.ActiveToolCall[candidateIndex]; active != nil && (partTextResult.String() != "" || (functionCallResult.Exists() && !active.CanContinueWith(functionCallResult))) {
						p.SetToolInputError(fmt.Errorf("upstream Gemini content or call identity interrupted incomplete tool arguments"))
						p.HasIncompleteToolCall[candidateIndex] = true
						p.ActiveToolCall[candidateIndex] = nil
						break
					}

					// Skip pure thoughtSignature parts but keep any actual payload in the same part.
					if hasThoughtSignature && !hasContentPayload {
						continue
					}

					if partTextResult.Exists() {
						text := partTextResult.String()
						if p.ActiveToolCall[candidateIndex] != nil {
							sawNoopFunctionCall = true
							continue
						}
						setAssistantRole()
						// Handle text content, distinguishing between regular content and reasoning/thoughts.
						if partResult.Get("thought").Bool() {
							template, _ = sjson.SetBytes(template, "choices.0.delta.reasoning_content", text)
						} else {
							template, _ = sjson.SetBytes(template, "choices.0.delta.content", text)
						}
					} else if functionCallResult.Exists() {
						// Handle function call content.
						if active := p.ActiveToolCall[candidateIndex]; active != nil && active.CanContinueWith(functionCallResult) {
							p.SawToolCall[candidateIndex] = true
							delta, done := active.Assembler.PushFunctionCall(functionCallResult)
							if err := active.Assembler.Err(); err != nil {
								p.SetToolInputError(err)
								p.HasIncompleteToolCall[candidateIndex] = true
								p.ActiveToolCall[candidateIndex] = nil
								sawNoopFunctionCall = true
								break
							}
							if done {
								p.ActiveToolCall[candidateIndex] = nil
							}
							if delta != "" {
								toolCallsResult := gjson.GetBytes(template, "choices.0.delta.tool_calls")
								if !toolCallsResult.Exists() || !toolCallsResult.IsArray() {
									template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls", []byte(`[]`))
								}
								functionCallTemplate := []byte(`{"index":0,"function":{"arguments":""}}`)
								functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "index", active.Index)
								functionCallTemplate, _ = translatorcommon.SetStringWithoutHTMLEscape(functionCallTemplate, "function.arguments", delta)
								setAssistantRole()
								template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls.-1", functionCallTemplate)
							} else {
								sawNoopFunctionCall = true
							}
						} else if translatorcommon.IsVertexStreamingFunctionCall(functionCallResult) {
							rawName := strings.TrimSpace(functionCallResult.Get("name").String())
							if rawName == "" {
								p.HasIncompleteToolCall[candidateIndex] = true
								sawNoopFunctionCall = true
								continue
							}
							fcName := util.RestoreSanitizedToolName(p.SanitizedNameMap, rawName)
							upstreamID := translatorcommon.VertexFunctionCallID(functionCallResult)
							fcID := upstreamID
							if fcID == "" {
								fcID = fmt.Sprintf("%s-%d-%d", fcName, time.Now().UnixNano(), atomic.AddUint64(&functionCallIDCounter, 1))
							}
							functionCallIndex := p.FunctionIndex[candidateIndex]
							active := translatorcommon.NewVertexStreamingToolCall(rawName, fcName, upstreamID, fcID, functionCallIndex)
							delta, done := active.Assembler.PushFunctionCall(functionCallResult)
							if err := active.Assembler.Err(); err != nil {
								p.SetToolInputError(err)
								p.HasIncompleteToolCall[candidateIndex] = true
								sawNoopFunctionCall = true
								break
							}
							p.SawToolCall[candidateIndex] = true
							p.FunctionIndex[candidateIndex]++
							if !done {
								p.ActiveToolCall[candidateIndex] = active
							}

							toolCallsResult := gjson.GetBytes(template, "choices.0.delta.tool_calls")
							if !toolCallsResult.Exists() || !toolCallsResult.IsArray() {
								template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls", []byte(`[]`))
							}

							functionCallTemplate := []byte(`{"id":"","index":0,"type":"function","function":{"name":"","arguments":""}}`)
							functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "id", fcID)
							functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "index", functionCallIndex)
							functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "function.name", fcName)
							functionCallTemplate, _ = translatorcommon.SetStringWithoutHTMLEscape(functionCallTemplate, "function.arguments", delta)
							setAssistantRole()
							template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls.-1", functionCallTemplate)
						} else {
							rawName := strings.TrimSpace(functionCallResult.Get("name").String())
							if rawName == "" && !functionCallResult.Get("args").Exists() {
								sawNoopFunctionCall = true
								continue
							}
							p.SawToolCall[candidateIndex] = true
							sawCompleteFunctionCall = true
							toolCallsResult := gjson.GetBytes(template, "choices.0.delta.tool_calls")

							// Retrieve the function index for this specific candidate.
							functionCallIndex := p.FunctionIndex[candidateIndex]
							p.FunctionIndex[candidateIndex]++

							if !toolCallsResult.Exists() || !toolCallsResult.IsArray() {
								template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls", []byte(`[]`))
							}

							functionCallTemplate := []byte(`{"id":"","index":0,"type":"function","function":{"name":"","arguments":""}}`)
							fcName := util.RestoreSanitizedToolName(p.SanitizedNameMap, rawName)
							fcID := translatorcommon.VertexFunctionCallID(functionCallResult)
							if fcID == "" {
								fcID = fmt.Sprintf("%s-%d-%d", fcName, time.Now().UnixNano(), atomic.AddUint64(&functionCallIDCounter, 1))
							}
							functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "id", fcID)
							functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "index", functionCallIndex)
							functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "function.name", fcName)
							if fcArgsResult := functionCallResult.Get("args"); fcArgsResult.Exists() {
								functionCallTemplate, _ = sjson.SetBytes(functionCallTemplate, "function.arguments", fcArgsResult.Raw)
							}
							setAssistantRole()
							template, _ = sjson.SetRawBytes(template, "choices.0.delta.tool_calls.-1", functionCallTemplate)
						}
					} else if inlineDataResult.Exists() {
						if p.ActiveToolCall[candidateIndex] != nil {
							p.HasIncompleteToolCall[candidateIndex] = true
							p.ActiveToolCall[candidateIndex] = nil
						}
						data := inlineDataResult.Get("data").String()
						if data == "" {
							continue
						}
						mimeType := inlineDataResult.Get("mimeType").String()
						if mimeType == "" {
							mimeType = inlineDataResult.Get("mime_type").String()
						}
						if mimeType == "" {
							mimeType = "image/png"
						}
						imageURL := fmt.Sprintf("data:%s;base64,%s", mimeType, data)
						imagesResult := gjson.GetBytes(template, "choices.0.delta.images")
						if !imagesResult.Exists() || !imagesResult.IsArray() {
							template, _ = sjson.SetRawBytes(template, "choices.0.delta.images", []byte(`[]`))
						}
						imageIndex := len(gjson.GetBytes(template, "choices.0.delta.images").Array())
						imagePayload := []byte(`{"type":"image_url","image_url":{"url":""}}`)
						imagePayload, _ = sjson.SetBytes(imagePayload, "index", imageIndex)
						imagePayload, _ = sjson.SetBytes(imagePayload, "image_url.url", imageURL)
						setAssistantRole()
						template, _ = sjson.SetRawBytes(template, "choices.0.delta.images.-1", imagePayload)
					}
				}
			}

			if p.ToolInputError() != nil {
				return false
			}
			upstreamFinishReason := p.UpstreamFinishReason[candidateIndex]
			sawToolCall := p.SawToolCall[candidateIndex]
			usageExists := gjson.GetBytes(rawJSON, "usageMetadata").Exists()
			isFinalChunk := upstreamFinishReason != "" && (usageExists || sawCompleteFunctionCall) && !p.SawFinishReason[candidateIndex] && p.ActiveToolCall[candidateIndex] == nil && !p.HasIncompleteToolCall[candidateIndex] && p.ToolInputError() == nil

			if isFinalChunk {
				p.SawFinishReason[candidateIndex] = true
				finishReason, nativeFinishReason := resolveGeminiOpenAIFinishReason(upstreamFinishReason, sawToolCall)
				template, _ = sjson.SetBytes(template, "choices.0.finish_reason", finishReason)
				template, _ = sjson.SetBytes(template, "choices.0.native_finish_reason", nativeFinishReason)
			}

			if sawNoopFunctionCall && !assistantRoleSet && !isFinalChunk && !usageExists {
				return true
			}

			responseStrings = append(responseStrings, template)
			return true // continue loop
		})
	} else {
		// If there are no candidates (e.g., a pure usageMetadata chunk), return the usage chunk if present.
		if gjson.GetBytes(rawJSON, "usageMetadata").Exists() && len(responseStrings) == 0 {
			chunk := append([]byte(nil), baseTemplate...)
			if upstreamFinishReason := p.UpstreamFinishReason[0]; upstreamFinishReason != "" && !p.SawFinishReason[0] && p.ActiveToolCall[0] == nil && !p.HasIncompleteToolCall[0] {
				p.SawFinishReason[0] = true
				finishReason, nativeFinishReason := resolveGeminiOpenAIFinishReason(upstreamFinishReason, p.SawToolCall[0])
				chunk, _ = sjson.SetBytes(chunk, "choices.0.finish_reason", finishReason)
				chunk, _ = sjson.SetBytes(chunk, "choices.0.native_finish_reason", nativeFinishReason)
			}
			responseStrings = append(responseStrings, chunk)
		}
	}

	return responseStrings
}

// ConvertGeminiResponseToOpenAINonStream converts a non-streaming Gemini response to a non-streaming OpenAI response.
// This function processes the complete Gemini response and transforms it into a single OpenAI-compatible
// JSON response. It handles message content, tool calls, reasoning content, and usage metadata, combining all
// the information into a single response that matches the OpenAI API format.
//
// Parameters:
//   - ctx: The context for the request, used for cancellation and timeout handling
//   - modelName: The name of the model being used for the response (unused in current implementation)
//   - rawJSON: The raw JSON response from the Gemini API
//   - param: A pointer to a parameter object for the conversion (unused in current implementation)
//
// Returns:
//   - []byte: An OpenAI-compatible JSON response containing all message content and metadata
func ConvertGeminiResponseToOpenAINonStream(_ context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, _ *any) []byte {
	sanitizedNameMap := util.SanitizedToolNameMap(originalRequestRawJSON)
	var unixTimestamp int64
	// Initialize template with an empty choices array to support multiple candidates.
	template := []byte(`{"id":"chatcmpl-proxy","object":"chat.completion","created":0,"model":"","choices":[]}`)
	template, _ = sjson.SetBytes(template, "model", modelName)

	if modelVersionResult := gjson.GetBytes(rawJSON, "modelVersion"); modelName == "" && modelVersionResult.Exists() {
		template, _ = sjson.SetBytes(template, "model", modelVersionResult.String())
	}

	if createTimeResult := gjson.GetBytes(rawJSON, "createTime"); createTimeResult.Exists() {
		t, err := time.Parse(time.RFC3339Nano, createTimeResult.String())
		if err == nil {
			unixTimestamp = t.Unix()
		}
		template, _ = sjson.SetBytes(template, "created", unixTimestamp)
	} else {
		template, _ = sjson.SetBytes(template, "created", unixTimestamp)
	}

	if responseIDResult := gjson.GetBytes(rawJSON, "responseId"); responseIDResult.Exists() {
		template, _ = sjson.SetBytes(template, "id", responseIDResult.String())
	}

	if usageResult := gjson.GetBytes(rawJSON, "usageMetadata"); usageResult.Exists() {
		template, _ = sjson.SetBytes(template, "usage.completion_tokens", usageResult.Get("candidatesTokenCount").Int()+usageResult.Get("thoughtsTokenCount").Int())
		if totalTokenCountResult := usageResult.Get("totalTokenCount"); totalTokenCountResult.Exists() {
			template, _ = sjson.SetBytes(template, "usage.total_tokens", totalTokenCountResult.Int())
		}
		promptTokenCount := usageResult.Get("promptTokenCount").Int()
		thoughtsTokenCount := usageResult.Get("thoughtsTokenCount").Int()
		cachedTokenCount := usageResult.Get("cachedContentTokenCount").Int()
		template, _ = sjson.SetBytes(template, "usage.prompt_tokens", promptTokenCount)
		if thoughtsTokenCount > 0 {
			template, _ = sjson.SetBytes(template, "usage.completion_tokens_details.reasoning_tokens", thoughtsTokenCount)
		}
		// Include cached token count if present (indicates prompt caching is working)
		if cachedTokenCount > 0 {
			var err error
			template, err = sjson.SetBytes(template, "usage.prompt_tokens_details.cached_tokens", cachedTokenCount)
			if err != nil {
				log.Printf("gemini openai response: failed to set cached_tokens in non-streaming: %v", err)
			}
		}
	}

	// Process the main content part of the response for all candidates.
	candidates := gjson.GetBytes(rawJSON, "candidates")
	if candidates.IsArray() {
		var choicesList [][]byte
		candidates.ForEach(func(_, candidate gjson.Result) bool {
			// Construct a single Choice object.
			choiceTemplate := []byte(`{"index":0,"message":{"role":"assistant","content":null,"reasoning_content":null,"tool_calls":null},"finish_reason":null,"native_finish_reason":null}`)

			// Set the index for this choice.
			choiceTemplate, _ = sjson.SetBytes(choiceTemplate, "index", candidate.Get("index").Int())

			// Set finish reason.
			finishReason := "stop"
			if finishReasonResult := candidate.Get("finishReason"); finishReasonResult.Exists() {
				finishReason, _ = resolveGeminiOpenAIFinishReason(finishReasonResult.String(), false)
				choiceTemplate, _ = sjson.SetBytes(choiceTemplate, "native_finish_reason", strings.ToLower(finishReasonResult.String()))
			}
			choiceTemplate, _ = sjson.SetBytes(choiceTemplate, "finish_reason", finishReason)

			partsResult := candidate.Get("content.parts")
			hasFunctionCall := false
			if partsResult.IsArray() {
				partsResults := partsResult.Array()
				var toolCalls [][]byte
				var images [][]byte
				var textContent strings.Builder
				var reasoningContent strings.Builder
				hasTextContent := false
				hasReasoningContent := false

				for i := 0; i < len(partsResults); i++ {
					partResult := partsResults[i]
					partTextResult := partResult.Get("text")
					functionCallResult := partResult.Get("functionCall")
					inlineDataResult := partResult.Get("inlineData")
					if !inlineDataResult.Exists() {
						inlineDataResult = partResult.Get("inline_data")
					}

					// Speech-to-text models (gemini-3.5-transcribe) deliver the
					// transcript in an audioTranscription part instead of text.
					audioTranscriptionResult := partResult.Get("audioTranscription")
					if audioTranscriptionResult.Exists() && !partTextResult.Exists() {
						partTextResult = audioTranscriptionResult.Get("text")
					}

					if partTextResult.Exists() {
						// Append text content, distinguishing between regular content and reasoning.
						if partResult.Get("thought").Bool() {
							hasReasoningContent = true
							reasoningContent.WriteString(partTextResult.String())
						} else {
							hasTextContent = true
							textContent.WriteString(partTextResult.String())
						}
					} else if functionCallResult.Exists() {
						// Append function call content to the tool_calls array.
						hasFunctionCall = true
						functionCallItemTemplate := []byte(`{"id":"","type":"function","function":{"name":"","arguments":""}}`)
						fcName := util.RestoreSanitizedToolName(sanitizedNameMap, functionCallResult.Get("name").String())
						functionCallID := translatorcommon.VertexFunctionCallID(functionCallResult)
						if functionCallID == "" {
							functionCallID = fmt.Sprintf("%s-%d-%d", fcName, time.Now().UnixNano(), atomic.AddUint64(&functionCallIDCounter, 1))
						}
						functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "id", functionCallID)
						functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "function.name", fcName)
						if fcArgsResult := functionCallResult.Get("args"); fcArgsResult.Exists() {
							functionCallItemTemplate, _ = sjson.SetBytes(functionCallItemTemplate, "function.arguments", fcArgsResult.Raw)
						}
						toolCalls = append(toolCalls, functionCallItemTemplate)
					} else if inlineDataResult.Exists() {
						data := inlineDataResult.Get("data").String()
						if data != "" {
							mimeType := inlineDataResult.Get("mimeType").String()
							if mimeType == "" {
								mimeType = inlineDataResult.Get("mime_type").String()
							}
							if mimeType == "" {
								mimeType = "image/png"
							}
							imageURL := fmt.Sprintf("data:%s;base64,%s", mimeType, data)
							imagePayload := []byte(`{"type":"image_url","image_url":{"url":""}}`)
							imagePayload, _ = sjson.SetBytes(imagePayload, "index", len(images))
							imagePayload, _ = sjson.SetBytes(imagePayload, "image_url.url", imageURL)
							images = append(images, imagePayload)
						}
					}
				}

				if hasTextContent {
					choiceTemplate, _ = sjson.SetBytes(choiceTemplate, "message.content", textContent.String())
				}
				if hasReasoningContent {
					choiceTemplate, _ = sjson.SetBytes(choiceTemplate, "message.reasoning_content", reasoningContent.String())
				}
				if len(toolCalls) > 0 {
					choiceTemplate, _ = sjson.SetRawBytes(choiceTemplate, "message.tool_calls", translatorcommon.JoinRawArray(toolCalls))
				}
				if len(images) > 0 {
					choiceTemplate, _ = sjson.SetRawBytes(choiceTemplate, "message.images", translatorcommon.JoinRawArray(images))
				}
			}

			if hasFunctionCall {
				choiceTemplate, _ = sjson.SetBytes(choiceTemplate, "finish_reason", "tool_calls")
				choiceTemplate, _ = sjson.SetBytes(choiceTemplate, "native_finish_reason", "tool_calls")
			}

			// Append the constructed choice to the main choices array.
			choicesList = append(choicesList, choiceTemplate)
			return true
		})
		if len(choicesList) > 0 {
			template = translatorcommon.SetRawArrayItems(template, "choices", choicesList)
		}
	}

	return template
}

func resolveGeminiOpenAIFinishReason(upstreamFinishReason string, sawToolCall bool) (finishReason, nativeFinishReason string) {
	switch {
	case sawToolCall:
		finishReason = "tool_calls"
	case upstreamFinishReason == "MAX_TOKENS":
		finishReason = "length"
	case isGeminiContentFilterFinishReason(upstreamFinishReason):
		finishReason = "content_filter"
	default:
		finishReason = "stop"
	}
	nativeFinishReason = "stop"
	if upstreamFinishReason != "" {
		nativeFinishReason = strings.ToLower(upstreamFinishReason)
	}
	return finishReason, nativeFinishReason
}

func applyGeminiOpenAIUsageMetadata(template []byte, usageResult gjson.Result) []byte {
	cachedTokenCount := usageResult.Get("cachedContentTokenCount").Int()
	template, _ = sjson.SetBytes(template, "usage.completion_tokens", usageResult.Get("candidatesTokenCount").Int()+usageResult.Get("thoughtsTokenCount").Int())
	if totalTokenCountResult := usageResult.Get("totalTokenCount"); totalTokenCountResult.Exists() {
		template, _ = sjson.SetBytes(template, "usage.total_tokens", totalTokenCountResult.Int())
	}
	promptTokenCount := usageResult.Get("promptTokenCount").Int()
	thoughtsTokenCount := usageResult.Get("thoughtsTokenCount").Int()
	template, _ = sjson.SetBytes(template, "usage.prompt_tokens", promptTokenCount)
	if thoughtsTokenCount > 0 {
		template, _ = sjson.SetBytes(template, "usage.completion_tokens_details.reasoning_tokens", thoughtsTokenCount)
	}
	if cachedTokenCount > 0 {
		var err error
		template, err = sjson.SetBytes(template, "usage.prompt_tokens_details.cached_tokens", cachedTokenCount)
		if err != nil {
			log.Printf("gemini openai response: failed to set cached_tokens in streaming: %v", err)
		}
	}
	return template
}

func isGeminiContentFilterFinishReason(reason string) bool {
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY", "MALFORMED_RESPONSE", "MALFORMED_FUNCTION_CALL":
		return true
	default:
		return false
	}
}

// FinalizeToolInput reports invalid or truncated Vertex tool arguments at EOF.
func (p *convertGeminiResponseToOpenAIChatParams) FinalizeToolInput() [][]byte {
	if p.ToolInputError() != nil {
		return nil
	}
	for idx, active := range p.ActiveToolCall {
		if active != nil {
			p.SetToolInputError(fmt.Errorf("upstream Gemini function call in candidate %d ended before complete arguments", idx))
			return nil
		}
	}
	for idx, incomplete := range p.HasIncompleteToolCall {
		if incomplete {
			p.SetToolInputError(fmt.Errorf("upstream Gemini function call in candidate %d has invalid or incomplete arguments", idx))
			return nil
		}
	}
	return nil
}
