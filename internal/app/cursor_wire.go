package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

	"github.com/google/uuid"
)

// tryCursorOAuthChannel runs inference through cursor-agent instead of HTTP
// forwarding. StreamChat is deprecated and AgentService/RunSSE is a moving
// protobuf target; ask mode keeps Cursor from executing host shell/file tools.
//
// Client tools are mapped through the prompt: the model emits <cc_tool_call>
// blocks which are translated to Anthropic tool_use or OpenAI tool_calls. The
// client executes them and sends tool_result / role=tool on the next turn.
func (s *Server) tryCursorOAuthChannel(
	ctx context.Context,
	cfg *model.Config,
	reqCtx *proxyRequestContext,
	w http.ResponseWriter,
) (*proxyResult, error) {
	if reqCtx != nil && !cursorSupportsRequestFamily(reqCtx.requestPath) {
		return &proxyResult{
			status: http.StatusBadRequest, channelID: &cfg.ID, succeeded: false,
			protocolCapabilityMissing: true, nextAction: cooldown.ActionRetryChannel,
			body: []byte(`{"error":{"message":"Cursor OAuth supports Anthropic messages and OpenAI chat completions","type":"invalid_request_error"}}`),
		}, nil
	}
	if s.cursorCredentials == nil {
		return oauthCredentialUnavailableResult(cfg, "Cursor"), nil
	}
	credential, err := s.cursorCredentials.credential(ctx, cfg, false)
	if credential == nil {
		if err != nil {
			s.cooldownRejectedOAuthCredential(ctx, cfg, "Cursor")
		}
		return oauthCredentialUnavailableResult(cfg, "Cursor"), nil
	}
	return s.forwardCursorAgent(ctx, cfg, credential, reqCtx, w)
}

func (s *Server) forwardCursorAgent(
	ctx context.Context,
	cfg *model.Config,
	credential *cursorauth.Credential,
	reqCtx *proxyRequestContext,
	w http.ResponseWriter,
) (*proxyResult, error) {
	body := reqCtx.body
	if len(reqCtx.translatedBody) > 0 {
		body = reqCtx.translatedBody
	}
	request := cursorauth.ParseRequest(body)
	if request.Prompt == "" {
		return cursorClientErrorResult(cfg, http.StatusBadRequest, "cursor prompt is required"), nil
	}
	requested := request.Model
	if requested == "" {
		requested = reqCtx.originalModel
	}
	modelID := cursorauth.ResolveModel(requested, cursorauth.ParseClientThinking(body))
	runner := s.cursorRunner
	if runner == nil {
		runner = cursorauth.NewCLIRunner()
	}

	started := time.Now()
	events, err := runner.Run(ctx, credential, modelID, request.Prompt)
	if err != nil {
		status := http.StatusBadGateway
		action := cooldown.ActionRetryChannel
		if errors.Is(err, cursorauth.ErrAgentMissing) {
			status = http.StatusServiceUnavailable
			action = cooldown.ActionReturnClient
		}
		result := cursorErrorResult(cfg, status, err.Error(), action)
		s.logProxyResult(reqCtx, cfg, modelID, "cursor-oauth", status, time.Since(started).Seconds(), &fwResult{
			Status: status, Body: result.body,
		}, err.Error())
		return result, nil
	}

	format := cursorResponseFormat(reqCtx)
	streaming := reqCtx.isStreaming
	mapTools := request.AllowsTools()
	msgID := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	var out bytes.Buffer
	full := ""
	firstByte := time.Duration(0)
	wroteHeader := false
	streamedPlain := 0
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	ensureStream := func() {
		if !streaming || wroteHeader {
			return
		}
		disableResponseWriteTimeout(w, "Cursor CLI stream")
		header := w.Header()
		header.Set("Content-Type", "text/event-stream")
		header.Set("Cache-Control", "no-cache")
		header.Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		wroteHeader = true
		if format == "anthropic" {
			_, _ = w.Write(cursorAnthropicStart(msgID, modelID))
		}
	}
	writeStream := func(chunk string) {
		if !streaming || chunk == "" {
			return
		}
		ensureStream()
		if format == "anthropic" {
			_, _ = w.Write(cursorAnthropicDelta(chunk))
		} else {
			_, _ = w.Write(cursorOpenAIChunk(msgID, modelID, chunk, ""))
		}
		flush()
	}

	var runErr error
	for event := range events {
		if firstByte == 0 && (event.Delta != "" || event.Done || event.Text != "") {
			firstByte = time.Since(started)
		}
		if event.Text != "" {
			full = event.Text
		} else if event.Delta != "" {
			out.WriteString(event.Delta)
			full = out.String()
		}
		if event.Err != nil {
			runErr = event.Err
		}
		if streaming && !mapTools && event.Delta != "" {
			writeStream(event.Delta)
			streamedPlain += len(event.Delta)
		}
		if streaming && mapTools && full != "" {
			plain, _, incomplete := cursorauth.SplitToolOutput(full)
			if !incomplete && len(plain) > streamedPlain {
				writeStream(plain[streamedPlain:])
				streamedPlain = len(plain)
			}
		}
	}
	duration := time.Since(started).Seconds()
	if runErr != nil {
		status := http.StatusBadGateway
		action := cooldown.ActionRetryChannel
		if strings.Contains(strings.ToLower(runErr.Error()), "not authenticated") {
			status = http.StatusUnauthorized
			s.cooldownRejectedOAuthCredential(ctx, cfg, "Cursor")
		}
		if streaming && wroteHeader {
			_, _ = w.Write([]byte("data: {\"error\":{\"message\":" + jsonString(runErr.Error()) + "}}\n\n"))
			flush()
		}
		result := cursorErrorResult(cfg, status, runErr.Error(), action)
		s.logProxyResult(reqCtx, cfg, modelID, "cursor-oauth", status, duration, &fwResult{
			Status: status, Body: result.body, FirstByteTime: firstByte.Seconds(),
		}, runErr.Error())
		if streaming && wroteHeader {
			// The SSE envelope is already on the wire; the attempt loop must not
			// write a second JSON body.
			result.succeeded = true
			result.nextAction = cooldown.ActionReturnClient
			return result, nil
		}
		return result, nil
	}

	plain := full
	var calls []cursorauth.ToolCall
	if mapTools {
		var incomplete bool
		plain, calls, incomplete = cursorauth.SplitToolOutput(full)
		if incomplete {
			plain = full
			calls = nil
		}
		calls = cursorauth.FilterToolCalls(calls, request.Tools)
		if choice := request.ToolChoice; choice != "" && choice != "auto" && choice != "required" && choice != "none" {
			calls = cursorauth.FilterToolCalls(calls, []cursorauth.Tool{{Name: choice}})
		}
	}

	var responseBody []byte
	var header http.Header
	if streaming {
		if format == "anthropic" {
			ensureStream()
			if streamedPlain == 0 && plain != "" {
				writeStream(plain)
			}
			_, _ = w.Write(cursorAnthropicStreamFinish(calls))
		} else {
			ensureStream()
			if streamedPlain == 0 && plain != "" && len(calls) == 0 {
				writeStream(plain)
			}
			_, _ = w.Write(cursorOpenAIFinish(msgID, modelID, plain, calls, streamedPlain > 0))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}
		flush()
		responseBody = []byte(plain)
		header = w.Header()
	} else {
		if format == "anthropic" {
			responseBody = cursorAnthropicMessage(msgID, modelID, plain, calls)
		} else {
			responseBody = cursorOpenAIMessage(msgID, modelID, plain, calls)
		}
		header = make(http.Header)
		header.Set("Content-Type", "application/json")
		writeResponseWithHeaders(w, http.StatusOK, header, responseBody)
	}

	channelID := cfg.ID
	s.logProxyResult(reqCtx, cfg, modelID, "cursor-oauth", http.StatusOK, duration, &fwResult{
		Status: http.StatusOK, Header: header, Body: responseBody, FirstByteTime: firstByte.Seconds(),
	}, "")
	return &proxyResult{
		status: http.StatusOK, header: header, body: responseBody, channelID: &channelID,
		duration: duration, firstByteTime: firstByte.Seconds(), succeeded: true,
		nextAction: cooldown.ActionReturnClient,
	}, nil
}

func cursorResponseFormat(reqCtx *proxyRequestContext) string {
	if reqCtx == nil {
		return "openai"
	}
	if util.NormalizeProtocol(string(reqCtx.clientProtocol)) == util.ProtocolAnthropic ||
		strings.Contains(reqCtx.requestPath, "/messages") {
		return "anthropic"
	}
	return "openai"
}

func cursorClientErrorResult(cfg *model.Config, status int, message string) *proxyResult {
	return cursorErrorResult(cfg, status, message, cooldown.ActionReturnClient)
}

func cursorErrorResult(cfg *model.Config, status int, message string, action cooldown.Action) *proxyResult {
	channelID := cfg.ID
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": "api_error"},
	})
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	return &proxyResult{
		status: status, header: header, body: body, channelID: &channelID,
		succeeded: false, nextAction: action,
	}
}

func cursorAnthropicStart(id, modelID string) []byte {
	start, _ := json.Marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": id, "type": "message", "role": "assistant", "content": []any{},
			"model": modelID, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	block, _ := json.Marshal(map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	return []byte("event: message_start\ndata: " + string(start) + "\n\n" +
		"event: content_block_start\ndata: " + string(block) + "\n\n")
}

func cursorAnthropicDelta(text string) []byte {
	payload, _ := json.Marshal(map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
	return []byte("event: content_block_delta\ndata: " + string(payload) + "\n\n")
}

func cursorAnthropicStreamFinish(calls []cursorauth.ToolCall) []byte {
	var b bytes.Buffer
	stop, _ := json.Marshal(map[string]any{"type": "content_block_stop", "index": 0})
	b.WriteString("event: content_block_stop\ndata: " + string(stop) + "\n\n")
	for i, call := range calls {
		blockIndex := i + 1
		input := json.RawMessage(`{}`)
		if len(call.Arguments) > 0 {
			input = call.Arguments
		}
		start, _ := json.Marshal(map[string]any{
			"type": "content_block_start", "index": blockIndex,
			"content_block": map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": map[string]any{}},
		})
		delta, _ := json.Marshal(map[string]any{
			"type": "content_block_delta", "index": blockIndex,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(input)},
		})
		blockStop, _ := json.Marshal(map[string]any{"type": "content_block_stop", "index": blockIndex})
		b.WriteString("event: content_block_start\ndata: " + string(start) + "\n\n")
		b.WriteString("event: content_block_delta\ndata: " + string(delta) + "\n\n")
		b.WriteString("event: content_block_stop\ndata: " + string(blockStop) + "\n\n")
	}
	reason := "end_turn"
	if len(calls) > 0 {
		reason = "tool_use"
	}
	messageDelta, _ := json.Marshal(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": reason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 0, "input_tokens": 0},
	})
	b.WriteString("event: message_delta\ndata: " + string(messageDelta) + "\n\n")
	return b.Bytes()
}

func cursorAnthropicMessage(id, modelID, text string, calls []cursorauth.ToolCall) []byte {
	content := make([]any, 0, 1+len(calls))
	if text != "" || len(calls) == 0 {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, call := range calls {
		var input any
		if len(call.Arguments) == 0 || json.Unmarshal(call.Arguments, &input) != nil {
			input = map[string]any{}
		}
		content = append(content, map[string]any{
			"type": "tool_use", "id": call.ID, "name": call.Name, "input": input,
		})
	}
	reason := "end_turn"
	if len(calls) > 0 {
		reason = "tool_use"
	}
	body, _ := json.Marshal(map[string]any{
		"id": id, "type": "message", "role": "assistant",
		"content": content, "model": modelID, "stop_reason": reason, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
	})
	return body
}

func cursorOpenAIChunk(id, modelID, content, finish string) []byte {
	delta := map[string]any{}
	var finishReason any
	if content != "" {
		delta["content"] = content
	}
	if finish != "" {
		finishReason = finish
	}
	payload, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-" + id, "object": "chat.completion.chunk",
		"created": time.Now().Unix(), "model": modelID,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}},
	})
	return []byte("data: " + string(payload) + "\n\n")
}

func cursorOpenAIFinish(id, modelID, text string, calls []cursorauth.ToolCall, textAlreadyStreamed bool) []byte {
	var b bytes.Buffer
	if !textAlreadyStreamed && text != "" && len(calls) == 0 {
		b.Write(cursorOpenAIChunk(id, modelID, text, ""))
	}
	for i, call := range calls {
		args := "{}"
		if len(call.Arguments) > 0 {
			args = string(call.Arguments)
		}
		delta := map[string]any{
			"tool_calls": []any{map[string]any{
				"index": i, "id": openaiToolCallID(call.ID), "type": "function",
				"function": map[string]any{"name": call.Name, "arguments": args},
			}},
		}
		payload, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-" + id, "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": modelID,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
		})
		b.WriteString("data: " + string(payload) + "\n\n")
	}
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
	}
	b.Write(cursorOpenAIChunk(id, modelID, "", finish))
	return b.Bytes()
}

func cursorOpenAIMessage(id, modelID, text string, calls []cursorauth.ToolCall) []byte {
	message := map[string]any{"role": "assistant", "content": text}
	finish := "stop"
	if len(calls) > 0 {
		finish = "tool_calls"
		if text == "" {
			message["content"] = nil
		}
		toolCalls := make([]any, 0, len(calls))
		for _, call := range calls {
			args := "{}"
			if len(call.Arguments) > 0 {
				args = string(call.Arguments)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id": openaiToolCallID(call.ID), "type": "function",
				"function": map[string]any{"name": call.Name, "arguments": args},
			})
		}
		message["tool_calls"] = toolCalls
	}
	body, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-" + id, "object": "chat.completion",
		"created": time.Now().Unix(), "model": modelID,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finish}},
		"usage":   map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	})
	return body
}

func openaiToolCallID(id string) string {
	if strings.HasPrefix(id, "call_") {
		return id
	}
	if strings.HasPrefix(id, "toolu_") {
		return "call_" + strings.TrimPrefix(id, "toolu_")
	}
	return "call_" + id
}

func jsonString(value string) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(raw)
}

func cursorSupportsRequestFamily(path string) bool {
	family := protocol.DetectRequestFamily(path)
	return family == protocol.RequestFamilyMessages || family == protocol.RequestFamilyChatCompletions
}
