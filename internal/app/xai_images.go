package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

const openAIImagesGenerationsPath = "/v1/images/generations"

var errXAIImagesBridgeUnsupported = errors.New("xAI Images bridge request unsupported")

var xaiImagesToolOptionFields = [...]string{
	"size",
	"quality",
	"background",
	"output_format",
	"output_compression",
	"partial_images",
	"moderation",
}

type xaiImageGenerationOutput struct {
	Type          string `json:"type"`
	Result        string `json:"result"`
	RevisedPrompt string `json:"revised_prompt"`
	OutputFormat  string `json:"output_format"`
	Size          string `json:"size"`
	Background    string `json:"background"`
	Quality       string `json:"quality"`
}

type xaiImagesResponsesResult struct {
	CreatedAt int64                      `json:"created_at"`
	Output    []xaiImageGenerationOutput `json:"output"`
	ToolUsage struct {
		ImageGeneration json.RawMessage `json:"image_gen"`
	} `json:"tool_usage"`
}

type xaiImagesStreamState struct {
	outputByIndex map[int64]xaiImageGenerationOutput
	fallback      []xaiImageGenerationOutput
	completed     *xaiImagesResponsesResult
}

func (s *xaiImagesStreamState) collectOutputItem(index *int64, item xaiImageGenerationOutput) {
	if s == nil || item.Type != "image_generation_call" || strings.TrimSpace(item.Result) == "" {
		return
	}
	if index == nil {
		s.fallback = append(s.fallback, item)
		return
	}
	if s.outputByIndex == nil {
		s.outputByIndex = make(map[int64]xaiImageGenerationOutput)
	}
	s.outputByIndex[*index] = item
}

func (s *xaiImagesStreamState) outputs() []xaiImageGenerationOutput {
	if s == nil {
		return nil
	}
	indexes := make([]int64, 0, len(s.outputByIndex))
	for index := range s.outputByIndex {
		indexes = append(indexes, index)
	}
	sort.Slice(indexes, func(i, j int) bool { return indexes[i] < indexes[j] })
	outputs := make([]xaiImageGenerationOutput, 0, len(indexes)+len(s.fallback))
	for _, index := range indexes {
		outputs = append(outputs, s.outputByIndex[index])
	}
	return append(outputs, s.fallback...)
}

func mergeXAIImageOutputs(existing, collected []xaiImageGenerationOutput) []xaiImageGenerationOutput {
	if len(collected) == 0 {
		return existing
	}
	merged := append([]xaiImageGenerationOutput(nil), existing...)
	seen := make(map[string]struct{}, len(existing))
	for _, output := range existing {
		if output.Type == "image_generation_call" && strings.TrimSpace(output.Result) != "" {
			seen[output.Result] = struct{}{}
		}
	}
	for _, output := range collected {
		if output.Type != "image_generation_call" || strings.TrimSpace(output.Result) == "" {
			continue
		}
		if _, exists := seen[output.Result]; exists {
			continue
		}
		seen[output.Result] = struct{}{}
		merged = append(merged, output)
	}
	return merged
}

func hasXAIImageGenerationOutput(outputs []xaiImageGenerationOutput) bool {
	for _, output := range outputs {
		if output.Type == "image_generation_call" && strings.TrimSpace(output.Result) != "" {
			return true
		}
	}
	return false
}

func xaiImagesCompletedChunks(response xaiImagesResponsesResult, originalRequest []byte) ([][]byte, error) {
	responseFormat, requestOutputFormat := xaiImagesResponseFormats(originalRequest)
	completed := make([][]byte, 0, len(response.Output))
	for _, output := range response.Output {
		if output.Type != "image_generation_call" || strings.TrimSpace(output.Result) == "" {
			continue
		}
		payload := map[string]any{"type": "image_generation.completed"}
		if responseFormat == "url" {
			outputFormat := output.OutputFormat
			if outputFormat == "" {
				outputFormat = requestOutputFormat
			}
			payload["url"] = "data:" + imageMIMEType(outputFormat) + ";base64," + output.Result
		} else {
			payload["b64_json"] = output.Result
		}
		if len(response.ToolUsage.ImageGeneration) > 0 && string(response.ToolUsage.ImageGeneration) != "null" {
			payload["usage"] = response.ToolUsage.ImageGeneration
		}
		chunk, err := encodeXAIImagesSSEEvent("image_generation.completed", payload)
		if err != nil {
			return nil, err
		}
		completed = append(completed, chunk)
	}
	if len(completed) == 0 {
		return nil, errors.New("xAI Responses completed without image_generation_call result")
	}
	return completed, nil
}

func isOpenAIImagesGenerationRequest(method, path string, clientProtocol protocol.Protocol) bool {
	return method == "POST" &&
		clientProtocol == protocol.OpenAI &&
		strings.TrimRight(strings.TrimSpace(path), "/") == openAIImagesGenerationsPath
}

func (s *Server) xaiImagesResponsesModel(cfg *model.Config, reqCtx *proxyRequestContext) (string, bool) {
	if cfg == nil || reqCtx == nil || !cfg.UsesXAIOAuth() ||
		cfg.GetProtocolTransformMode() == model.ProtocolTransformModeUpstream ||
		!isOpenAIImagesGenerationRequest(reqCtx.requestMethod, reqCtx.requestPath, reqCtx.clientProtocol) {
		return "", false
	}
	actualModel := s.resolveFinalUpstreamModel(cfg, reqCtx.originalModel, string(protocol.Codex))
	return actualModel, xaiSupportsImageGeneration(actualModel)
}

func isXAIImagesResponsesPlan(plan protocol.TransformPlan) bool {
	return plan.ClientProtocol == protocol.OpenAI &&
		plan.UpstreamProtocol == protocol.Codex &&
		plan.RequestFamily == protocol.RequestFamilyImages &&
		strings.TrimRight(strings.TrimSpace(plan.OriginalPath), "/") == openAIImagesGenerationsPath
}

func buildXAIImagesResponsesRequest(raw []byte, actualModel string) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var source map[string]any
	if err := decoder.Decode(&source); err != nil {
		return nil, errors.New("invalid Images request JSON")
	}
	if source == nil {
		return nil, errors.New("images request must be a JSON object")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("images request contains trailing JSON")
	}

	prompt, ok := source["prompt"].(string)
	if !ok || strings.TrimSpace(prompt) == "" {
		return nil, errors.New("images request requires a non-empty string prompt")
	}
	if rawStream, exists := source["stream"]; exists {
		if _, ok := rawStream.(bool); !ok {
			return nil, errors.New("images stream must be a boolean")
		}
	}
	if n, exists := source["n"]; exists {
		value, ok := n.(json.Number)
		count, err := value.Int64()
		if !ok || err != nil || count != 1 {
			if ok && err == nil && count > 1 {
				return nil, fmt.Errorf("%w: only n=1 is supported", errXAIImagesBridgeUnsupported)
			}
			return nil, errors.New("images n must be the integer 1 or greater")
		}
	}
	responseFormat := ""
	if rawResponseFormat, exists := source["response_format"]; exists {
		var ok bool
		responseFormat, ok = rawResponseFormat.(string)
		if !ok {
			return nil, errors.New("images response_format must be a string")
		}
	}
	switch strings.ToLower(strings.TrimSpace(responseFormat)) {
	case "", "b64_json", "url":
	default:
		return nil, errors.New("images response_format must be b64_json or url")
	}

	tool := map[string]any{
		"type":   xaiImageGenerationToolType,
		"action": "generate",
	}
	for _, field := range xaiImagesToolOptionFields {
		if value, exists := source[field]; exists {
			tool[field] = value
		}
	}
	payload := map[string]any{
		"instructions": "",
		"input": []any{map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{map[string]any{
				"type": "input_text",
				"text": prompt,
			}},
		}},
		"model":               actualModel,
		"parallel_tool_calls": true,
		"store":               false,
		"stream":              true,
		"tool_choice":         "required",
		"tools":               []any{tool},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode xAI Images Responses request: %w", err)
	}
	return encoded, nil
}

func buildOpenAIImagesResponseFromXAIResponses(responseBody, originalRequest []byte) ([]byte, error) {
	var response xaiImagesResponsesResult
	if err := json.Unmarshal(responseBody, &response); err != nil {
		return nil, errors.New("decode xAI Responses image result")
	}

	responseFormat, requestOutputFormat := xaiImagesResponseFormats(originalRequest)

	createdAt := response.CreatedAt
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	data := make([]map[string]any, 0, len(response.Output))
	metadata := map[string]string{}
	for _, output := range response.Output {
		if output.Type != "image_generation_call" || strings.TrimSpace(output.Result) == "" {
			continue
		}
		item := make(map[string]any, 2)
		if responseFormat == "url" {
			outputFormat := output.OutputFormat
			if outputFormat == "" {
				outputFormat = requestOutputFormat
			}
			item["url"] = "data:" + imageMIMEType(outputFormat) + ";base64," + output.Result
		} else {
			item["b64_json"] = output.Result
		}
		if output.RevisedPrompt != "" {
			item["revised_prompt"] = output.RevisedPrompt
		}
		data = append(data, item)
		if len(metadata) == 0 {
			metadata["background"] = output.Background
			metadata["output_format"] = output.OutputFormat
			metadata["quality"] = output.Quality
			metadata["size"] = output.Size
		}
	}
	if len(data) == 0 {
		return nil, errors.New("xAI Responses completed without image_generation_call result")
	}

	payload := map[string]any{"created": createdAt, "data": data}
	for key, value := range metadata {
		if value != "" {
			payload[key] = value
		}
	}
	if len(response.ToolUsage.ImageGeneration) > 0 && string(response.ToolUsage.ImageGeneration) != "null" {
		payload["usage"] = response.ToolUsage.ImageGeneration
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode OpenAI Images response: %w", err)
	}
	return encoded, nil
}

func xaiImagesResponseFormats(originalRequest []byte) (responseFormat, outputFormat string) {
	var request struct {
		ResponseFormat string `json:"response_format"`
		OutputFormat   string `json:"output_format"`
	}
	_ = json.Unmarshal(originalRequest, &request)
	responseFormat = strings.ToLower(strings.TrimSpace(request.ResponseFormat))
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	return responseFormat, strings.TrimSpace(request.OutputFormat)
}

func encodeXAIImagesSSEEvent(eventName string, payload any) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return []byte("event: " + eventName + "\ndata: " + string(encoded) + "\n\n"), nil
}

func xaiImagesStreamErrorEvent(rawError json.RawMessage, fallback string) ([]byte, error) {
	message := fallback
	var errorValue any
	if len(rawError) > 0 && json.Unmarshal(rawError, &errorValue) == nil {
		switch value := errorValue.(type) {
		case map[string]any:
			if upstreamMessage, _ := value["message"].(string); strings.TrimSpace(upstreamMessage) != "" {
				message = upstreamMessage
			}
		case string:
			if strings.TrimSpace(value) != "" {
				message = value
			}
			errorValue = map[string]any{"type": "upstream_error", "message": message}
		}
	}
	if errorValue == nil {
		errorValue = map[string]any{"type": "upstream_error", "message": message}
	}
	chunk, err := encodeXAIImagesSSEEvent("error", map[string]any{
		"type":  "error",
		"error": errorValue,
	})
	if err != nil {
		return nil, fmt.Errorf("encode Images stream error: %w", err)
	}
	return chunk, errors.New(message)
}

func translateXAIImagesResponsesStreamEvent(rawEvent, originalRequest []byte) (chunks [][]byte, terminal bool, eventErr error) {
	return translateXAIImagesResponsesStreamEventWithState(rawEvent, originalRequest, nil)
}

func translateXAIImagesResponsesStreamEventWithState(
	rawEvent, originalRequest []byte,
	state *xaiImagesStreamState,
) (chunks [][]byte, terminal bool, eventErr error) {
	eventType, data := parseSSEEventChunk(rawEvent)
	if len(data) == 0 || bytes.Equal(data, sseDoneMarker) {
		return nil, false, nil
	}
	if !json.Valid(data) {
		chunk, err := xaiImagesStreamErrorEvent(nil, "invalid xAI Responses SSE data JSON")
		return [][]byte{chunk}, true, err
	}

	var event struct {
		Type              string                   `json:"type"`
		PartialImageB64   string                   `json:"partial_image_b64"`
		PartialImageIndex int64                    `json:"partial_image_index"`
		OutputFormat      string                   `json:"output_format"`
		Error             json.RawMessage          `json:"error"`
		OutputIndex       *int64                   `json:"output_index"`
		Item              xaiImageGenerationOutput `json:"item"`
		Response          struct {
			Error json.RawMessage `json:"error"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		chunk, streamErr := xaiImagesStreamErrorEvent(nil, "decode xAI Responses SSE event")
		return [][]byte{chunk}, true, streamErr
	}
	payloadType := strings.TrimSpace(event.Type)
	if payloadType == "" {
		payloadType = strings.TrimSpace(eventType)
	}
	if eventType == "error" || payloadType == "error" || payloadType == "response.failed" || isErrorPayload(string(data)) {
		rawError := event.Error
		if len(rawError) == 0 || string(rawError) == "null" {
			rawError = event.Response.Error
		}
		chunk, err := xaiImagesStreamErrorEvent(rawError, "xAI Responses image stream failed")
		return [][]byte{chunk}, true, err
	}

	responseFormat, requestOutputFormat := xaiImagesResponseFormats(originalRequest)
	switch payloadType {
	case "response.output_item.done":
		if state == nil {
			return nil, false, nil
		}
		state.collectOutputItem(event.OutputIndex, event.Item)
		if state != nil && state.completed != nil {
			response := *state.completed
			response.Output = mergeXAIImageOutputs(response.Output, state.outputs())
			if hasXAIImageGenerationOutput(response.Output) {
				state.completed = nil
				completed, err := xaiImagesCompletedChunks(response, originalRequest)
				if err != nil {
					chunk, streamErr := xaiImagesStreamErrorEvent(nil, err.Error())
					return [][]byte{chunk}, true, streamErr
				}
				return completed, true, nil
			}
		}
		return nil, false, nil
	case "response.image_generation_call.partial_image":
		b64 := strings.TrimSpace(event.PartialImageB64)
		if b64 == "" {
			return nil, false, nil
		}
		payload := map[string]any{
			"type":                "image_generation.partial_image",
			"partial_image_index": event.PartialImageIndex,
		}
		if responseFormat == "url" {
			outputFormat := strings.TrimSpace(event.OutputFormat)
			if outputFormat == "" {
				outputFormat = requestOutputFormat
			}
			payload["url"] = "data:" + imageMIMEType(outputFormat) + ";base64," + b64
		} else {
			payload["b64_json"] = b64
		}
		chunk, err := encodeXAIImagesSSEEvent("image_generation.partial_image", payload)
		if err != nil {
			return nil, true, err
		}
		return [][]byte{chunk}, false, nil
	case "response.incomplete":
		chunk, err := xaiImagesStreamErrorEvent(nil, "xAI Responses image generation did not complete")
		return [][]byte{chunk}, true, err
	case "response.completed":
		var envelope struct {
			Response xaiImagesResponsesResult `json:"response"`
		}
		if err := json.Unmarshal(data, &envelope); err != nil {
			chunk, streamErr := xaiImagesStreamErrorEvent(nil, "decode completed xAI Responses image event")
			return [][]byte{chunk}, true, streamErr
		}
		var collected []xaiImageGenerationOutput
		if state != nil {
			collected = state.outputs()
		}
		outputs := mergeXAIImageOutputs(envelope.Response.Output, collected)
		if !hasXAIImageGenerationOutput(outputs) {
			if state == nil {
				chunk, err := xaiImagesStreamErrorEvent(nil, "xAI Responses completed without image_generation_call result")
				return [][]byte{chunk}, true, err
			}
			envelope.Response.Output = outputs
			state.completed = &envelope.Response
			return nil, false, nil
		}
		envelope.Response.Output = outputs
		completed, err := xaiImagesCompletedChunks(envelope.Response, originalRequest)
		if err != nil {
			chunk, streamErr := xaiImagesStreamErrorEvent(nil, err.Error())
			return [][]byte{chunk}, true, streamErr
		}
		return completed, true, nil
	default:
		return nil, false, nil
	}
}

func (s *Server) handleXAIImagesResponsesStreamSuccessResponse(
	reqCtx *requestContext,
	resp *http.Response,
	hdrClone http.Header,
	w http.ResponseWriter,
	readStats *streamReadStats,
	observer *ForwardObserver,
) (*fwResult, float64, error) {
	disableResponseWriteTimeout(w, "xAI图片流式")

	deferredWriter := newDeferredResponseWriter(w)
	responseHeader := resp.Header.Clone()
	responseHeader.Set("Content-Type", "text/event-stream")
	responseHeader.Del("Content-Encoding")
	responseHeader.Del("Content-Length")
	filterAndWriteResponseHeaders(deferredWriter, responseHeader)
	deferredWriter.WriteHeader(resp.StatusCode)

	parser := newSSEUsageParser(string(protocol.Codex))
	var state xaiImagesStreamState
	var terminal bool
	var translationErr error
	streamErr := streamTransformSSEEventsUntil(
		reqCtx.ctx,
		resp.Body,
		deferredWriter,
		func(rawEvent []byte) error {
			if err := parser.Feed(rawEvent); err != nil {
				return err
			}
			if shouldMarkUpstreamFirstByte(parser) {
				markFirstStreamResponse(reqCtx, readStats)
			}
			if !deferredWriter.Committed() && parser.GetLastError() != nil {
				return errAbortStreamBeforeWrite
			}
			return nil
		},
		func(rawEvent []byte) ([][]byte, error) {
			chunks, eventTerminal, eventErr := translateXAIImagesResponsesStreamEventWithState(
				rawEvent,
				reqCtx.transformPlan.OriginalBody,
				&state,
			)
			if eventErr != nil && !deferredWriter.Committed() {
				translationErr = eventErr
				return nil, errAbortStreamBeforeWrite
			}
			if len(chunks) > 0 && !deferredWriter.Committed() {
				if err := deferredWriter.Commit(); err != nil {
					return nil, err
				}
				markClientFirstByte(reqCtx, readStats, observer)
			}
			if eventTerminal {
				terminal = true
			}
			if eventErr != nil {
				translationErr = eventErr
			}
			return chunks, nil
		},
		func() bool { return terminal },
	)
	if errors.Is(streamErr, errAbortStreamBeforeWrite) {
		streamErr = nil
	}
	if !terminal && translationErr == nil && parser.GetLastError() == nil && reqCtx.ctx.Err() == nil {
		message := "xAI Responses image stream ended before completion"
		if streamErr != nil && !isClientDisconnectError(streamErr) {
			message = "xAI Responses image stream interrupted: " + streamErr.Error()
		}
		if streamErr == nil || !isClientDisconnectError(streamErr) {
			if deferredWriter.Committed() {
				chunk, terminalErr := xaiImagesStreamErrorEvent(nil, message)
				if _, writeErr := deferredWriter.Write(chunk); writeErr != nil {
					streamErr = writeErr
				} else if flusher, ok := any(deferredWriter).(http.Flusher); ok {
					flusher.Flush()
				}
				translationErr = terminalErr
			} else {
				translationErr = errors.New(message)
			}
		}
	}

	result := &fwResult{
		Status:            resp.StatusCode,
		UpstreamStatus:    resp.StatusCode,
		Header:            hdrClone,
		FirstByteTime:     responseFirstByteSec(reqCtx, readStats),
		BytesReceived:     readStats.totalBytes,
		ResponseCommitted: deferredWriter.Committed(),
	}
	populateFWResultFromUsageParser(result, parser)
	if result.SSEErrorEvent != nil {
		return result, reqCtx.Duration().Seconds(), nil
	}
	if translationErr != nil {
		result.StreamDiagMsg = translationErr.Error()
		return result, reqCtx.Duration().Seconds(), translationErr
	}
	if diagMsg := buildStreamDiagnostics(
		streamErr,
		readStats,
		terminal,
		string(protocol.Codex),
		resp.Header.Get("Content-Type"),
	); diagMsg != "" {
		result.StreamDiagMsg = diagMsg
	} else if terminal && streamErr != nil {
		streamErr = nil
	}
	return result, reqCtx.Duration().Seconds(), streamErr
}

func imageMIMEType(outputFormat string) string {
	switch strings.ToLower(strings.TrimSpace(outputFormat)) {
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return "image/png"
	}
}
