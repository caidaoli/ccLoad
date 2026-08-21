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
// protobuf target; the CLI ask mode is the same path cursor2Oauth uses.
//
// Tool calling is out of scope: --mode ask is text-only, client `tools` are
// ignored, and responses never contain tool_use / function_call blocks.
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
	prompt := cursorauth.ExtractPrompt(body)
	if prompt == "" {
		return cursorClientErrorResult(cfg, http.StatusBadRequest, "cursor prompt is required"), nil
	}
	requested := cursorauth.RequestModelID(body)
	if requested == "" {
		requested = reqCtx.originalModel
	}
	modelID := cursorauth.ResolveModel(requested, cursorauth.ParseClientThinking(body))
	runner := s.cursorRunner
	if runner == nil {
		runner = cursorauth.NewCLIRunner()
	}

	started := time.Now()
	events, err := runner.Run(ctx, credential, modelID, prompt)
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
	msgID := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	var out bytes.Buffer
	full := ""
	firstByte := time.Duration(0)
	wroteHeader := false
	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	writeStream := func(chunk string) {
		if !streaming {
			return
		}
		if !wroteHeader {
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
		if chunk == "" {
			return
		}
		if format == "anthropic" {
			_, _ = w.Write(cursorAnthropicDelta(chunk))
		} else {
			_, _ = w.Write(cursorOpenAIChunk(msgID, modelID, chunk, ""))
		}
		flush()
	}

	var runErr error
	for event := range events {
		if firstByte == 0 && (event.Delta != "" || event.Done) {
			firstByte = time.Since(started)
		}
		if event.Delta != "" {
			full = event.Text
			out.WriteString(event.Delta)
			writeStream(event.Delta)
		} else if event.Text != "" {
			full = event.Text
		}
		if event.Err != nil {
			runErr = event.Err
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

	var responseBody []byte
	var header http.Header
	if streaming {
		if !wroteHeader {
			disableResponseWriteTimeout(w, "Cursor CLI stream")
			header = w.Header()
			header.Set("Content-Type", "text/event-stream")
			header.Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			wroteHeader = true
			if format == "anthropic" {
				_, _ = w.Write(cursorAnthropicStart(msgID, modelID))
			}
			if full != "" {
				writeStream(full)
			}
		}
		if format == "anthropic" {
			_, _ = w.Write(cursorAnthropicStop())
		} else {
			_, _ = w.Write(cursorOpenAIChunk(msgID, modelID, "", "stop"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}
		flush()
		responseBody = []byte(full)
		header = w.Header()
	} else {
		if format == "anthropic" {
			responseBody = cursorAnthropicMessage(msgID, modelID, full)
			header = make(http.Header)
			header.Set("Content-Type", "application/json")
		} else {
			responseBody = cursorOpenAIMessage(msgID, modelID, full)
			header = make(http.Header)
			header.Set("Content-Type", "application/json")
		}
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

func cursorAnthropicStop() []byte {
	stop, _ := json.Marshal(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 0, "input_tokens": 0},
	})
	return []byte("event: message_delta\ndata: " + string(stop) + "\n\n")
}

func cursorAnthropicMessage(id, modelID, text string) []byte {
	body, _ := json.Marshal(map[string]any{
		"id": id, "type": "message", "role": "assistant",
		"content": []any{map[string]any{"type": "text", "text": text}},
		"model":   modelID, "stop_reason": "end_turn", "stop_sequence": nil,
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

func cursorOpenAIMessage(id, modelID, text string) []byte {
	body, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-" + id, "object": "chat.completion",
		"created": time.Now().Unix(), "model": modelID,
		"choices": []any{map[string]any{
			"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	})
	return body
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
