package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"ccLoad/internal/codebuddyauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func isCodeBuddyChatRequest(cfg *model.Config, upstream protocol.Protocol) bool {
	return cfg != nil && cfg.UsesCodeBuddyOAuth() && upstream == protocol.OpenAI
}

func prepareCodeBuddyDefaults(body, source []byte) []byte {
	if !strings.HasPrefix(gjson.GetBytes(body, "model").String(), "hy3") || gjson.GetBytes(body, "reasoning_effort").Exists() {
		return body
	}
	for _, key := range []string{"reasoning_effort", "reasoning", "thinking", "generationConfig.thinkingConfig"} {
		if gjson.GetBytes(source, key).Exists() {
			return body
		}
	}
	updated, err := sjson.SetBytes(body, "reasoning_effort", "high")
	if err == nil {
		return updated
	}
	return body
}

func finalizeCodeBuddyBody(body []byte) ([]byte, error) {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil || request == nil {
		return nil, errors.New("invalid CodeBuddy chat request")
	}
	var messages []map[string]json.RawMessage
	if json.Unmarshal(request["messages"], &messages) != nil || len(messages) == 0 {
		return nil, errors.New("CodeBuddy requires chat messages")
	}
	replacer := strings.NewReplacer(
		"You are Claude Code, Anthropic's official CLI for Claude.", "You are Claude Code, Anthropic's official CLI tool for Claude.",
		"Main branch (you will usually use this for PRs)", "Default branch (you will usually use this for PRs)",
	)
	for _, message := range messages {
		// Rewrite only text content, never serialized function arguments or tool results.
		if string(message["role"]) == `"tool"` {
			continue
		}
		var content string
		if json.Unmarshal(message["content"], &content) == nil {
			message["content"], _ = json.Marshal(replacer.Replace(content))
			continue
		}
		var parts []map[string]json.RawMessage
		if json.Unmarshal(message["content"], &parts) == nil && parts != nil {
			for _, part := range parts {
				if json.Unmarshal(part["text"], &content) == nil {
					part["text"], _ = json.Marshal(replacer.Replace(content))
				}
			}
			message["content"], _ = json.Marshal(parts)
		}
	}
	request["messages"], _ = json.Marshal(messages)
	request["stream"] = json.RawMessage("true")
	return json.Marshal(request)
}

func injectCodeBuddyHeaders(req *http.Request, cfg *model.Config, accessToken string) error {
	credential, err := codebuddyauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return err
	}
	credential.AccessToken = accessToken
	req.Header = make(http.Header)
	codebuddyauth.ApplyCredentialHeaders(req.Header, credential)
	applyHeaderRules(req.Header, cfg.HeaderRules())
	// Provider identity is authoritative even if a custom rule targets identity headers.
	identityHeaders := make(http.Header)
	codebuddyauth.ApplyCredentialHeaders(identityHeaders, credential)
	for _, name := range []string{"Authorization", "X-Refresh-Token", "X-User-Id", "X-Enterprise-Id", "X-Domain", "X-No-Authorization", "X-No-User-Id", "X-No-Enterprise-Id", "X-No-Department-Info", "X-Product"} {
		req.Header.Del(name)
		for _, value := range identityHeaders.Values(name) {
			req.Header.Add(name, value)
		}
	}
	return nil
}

func (s *Server) tryCodeBuddyOAuthChannel(ctx context.Context, cfg *model.Config, reqCtx *proxyRequestContext, w http.ResponseWriter) (*proxyResult, error) {
	return s.tryOAuthChannel(ctx, cfg, reqCtx, w, "CodeBuddy", true, func(force bool, rejected string) (*model.Config, string, error) {
		credential, err := s.codeBuddyCredentials.credential(ctx, cfg, force, rejected)
		if credential == nil {
			return cfg, "", err
		}
		runtime := cfg.Clone()
		runtime.OAuthCredential, _ = credential.JSON()
		return runtime, credential.AccessToken, err
	}, func(result *proxyResult) bool {
		return result != nil && !result.succeeded && result.status == http.StatusUnauthorized
	})
}

func normalizeCodeBuddySSEEvent(raw []byte) []byte {
	data := sseEventData(raw)
	if len(data) == 0 || bytes.Equal(data, sseDoneMarker) {
		return raw
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(data, &payload) != nil {
		return raw
	}
	if code := gjson.GetBytes(data, "code"); code.Exists() && code.Int() != 0 {
		errorBody, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "upstream_error", "code": code.Int(), "message": fmt.Sprintf("CodeBuddy upstream business error %d", code.Int())}})
		return append(append([]byte("data: "), errorBody...), '\n', '\n')
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(payload["choices"], &choices) != nil {
		return raw
	}
	for _, choice := range choices {
		var delta map[string]json.RawMessage
		if json.Unmarshal(choice["delta"], &delta) != nil || delta == nil {
			continue
		}
		for name, value := range delta {
			switch string(bytes.TrimSpace(value)) {
			case "null", `""`, "[]", "{}":
				delete(delta, name)
			}
		}
		choice["delta"], _ = json.Marshal(delta)
	}
	payload["choices"], _ = json.Marshal(choices)
	data, err := json.Marshal(payload)
	if err != nil {
		return raw
	}
	return append(append([]byte("data: "), data...), '\n', '\n')
}

type codeBuddyToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type codeBuddyChoice struct {
	role, finish       string
	content, reasoning strings.Builder
	tools              map[int]*codeBuddyToolCall
}

// collectCodeBuddyCompletion shares the application's bounded SSE reader and
// usage parser. It never writes a partial completion to the downstream client.
func collectCodeBuddyCompletion(ctx context.Context, reader io.Reader, onEvent func(*sseUsageParser)) ([]byte, *sseUsageParser, error) {
	parser := newSSEUsageParser("openai")
	choices := make(map[int]*codeBuddyChoice)
	var id, responseModel string
	var created int64
	var usage json.RawMessage
	var done bool
	total := 0
	consume := func(raw []byte) error {
		total += len(raw)
		if total > maxCodexNonStreamOutputBytes {
			return errors.New("CodeBuddy completion exceeds output limit")
		}
		raw = normalizeCodeBuddySSEEvent(raw)
		if err := parser.Feed(raw); err != nil {
			return err
		}
		if onEvent != nil {
			onEvent(parser)
		}
		if parser.GetLastError() != nil {
			return errors.New("CodeBuddy upstream stream error")
		}
		data := sseEventData(raw)
		if len(data) == 0 {
			return nil
		}
		if bytes.Equal(data, sseDoneMarker) {
			done = true
			return nil
		}
		var chunk struct {
			ID      string          `json:"id"`
			Model   string          `json:"model"`
			Created int64           `json:"created"`
			Usage   json.RawMessage `json:"usage"`
			Choices []struct {
				Index  int    `json:"index"`
				Finish string `json:"finish_reason"`
				Delta  struct {
					Role      string              `json:"role"`
					Content   string              `json:"content"`
					Reasoning string              `json:"reasoning_content"`
					ToolCalls []codeBuddyToolCall `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(data, &chunk) != nil {
			return errors.New("invalid CodeBuddy completion event")
		}
		if chunk.ID != "" {
			id = chunk.ID
		}
		if chunk.Model != "" {
			responseModel = chunk.Model
		}
		if chunk.Created != 0 {
			created = chunk.Created
		}
		if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
			usage = bytes.Clone(chunk.Usage)
		}
		for _, part := range chunk.Choices {
			choice := choices[part.Index]
			if choice == nil {
				choice = &codeBuddyChoice{role: "assistant", tools: make(map[int]*codeBuddyToolCall)}
				choices[part.Index] = choice
			}
			if part.Delta.Role != "" {
				choice.role = part.Delta.Role
			}
			choice.content.WriteString(part.Delta.Content)
			choice.reasoning.WriteString(part.Delta.Reasoning)
			if part.Finish != "" {
				choice.finish = part.Finish
			}
			for _, delta := range part.Delta.ToolCalls {
				call := choice.tools[delta.Index]
				if call == nil {
					call = &codeBuddyToolCall{Type: "function"}
					choice.tools[delta.Index] = call
				}
				if delta.ID != "" {
					call.ID = delta.ID
				}
				if delta.Type != "" {
					call.Type = delta.Type
				}
				call.Function.Name += delta.Function.Name
				call.Function.Arguments += delta.Function.Arguments
			}
		}
		return nil
	}
	err := streamTransformSSEEventsUntil(ctx, reader, discardHTTPResponseWriter{}, nil, func(raw []byte) ([][]byte, error) {
		return nil, consume(raw)
	}, func() bool { return done })
	if err != nil {
		return nil, parser, err
	}
	if len(choices) == 0 || !parser.IsStreamComplete() {
		return nil, parser, errors.New("CodeBuddy stream ended without completion")
	}
	indices := make([]int, 0, len(choices))
	for index := range choices {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	output := make([]any, 0, len(choices))
	for _, index := range indices {
		choice := choices[index]
		if choice.finish == "" {
			if !done {
				return nil, parser, errors.New("CodeBuddy choice did not finish")
			}
			choice.finish = "stop"
		}
		message := map[string]any{"role": choice.role, "content": choice.content.String()}
		if choice.reasoning.Len() > 0 {
			message["reasoning_content"] = choice.reasoning.String()
		}
		toolIndices := make([]int, 0, len(choice.tools))
		for index := range choice.tools {
			toolIndices = append(toolIndices, index)
		}
		sort.Ints(toolIndices)
		calls := make([]any, 0, len(toolIndices))
		for _, index := range toolIndices {
			call := choice.tools[index]
			calls = append(calls, map[string]any{"id": call.ID, "type": call.Type, "function": call.Function})
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
		}
		output = append(output, map[string]any{"index": index, "message": message, "finish_reason": choice.finish})
	}
	result := map[string]any{"id": id, "object": "chat.completion", "created": created, "model": responseModel, "choices": output}
	if len(usage) > 0 {
		result["usage"] = usage
	}
	body, err := json.Marshal(result)
	return body, parser, err
}

func (s *Server) handleCodeBuddyNonStream(reqCtx *requestContext, resp *http.Response, headers http.Header, w http.ResponseWriter, stats *streamReadStats) (*fwResult, float64, error) {
	body, parser, err := collectCodeBuddyCompletion(reqCtx.ctx, resp.Body, func(parser *sseUsageParser) {
		if shouldMarkUpstreamFirstByte(parser) {
			markFirstStreamResponse(reqCtx, stats)
		}
	})
	result := &fwResult{Status: resp.StatusCode, UpstreamStatus: resp.StatusCode, Header: headers, FirstByteTime: responseFirstByteSec(reqCtx, stats), BytesReceived: stats.totalBytes}
	populateFWResultFromUsageParser(result, parser)
	if result.SSEErrorEvent != nil {
		return result, reqCtx.Duration().Seconds(), nil
	}
	if err != nil {
		if !isClientDisconnectError(err) {
			result.StreamDiagMsg = err.Error()
		}
		return result, reqCtx.Duration().Seconds(), err
	}
	plan := reqCtx.transformPlan
	if plan.NeedsTransform {
		body, err = s.protocolRegistry.TranslateResponseNonStream(reqCtx.ctx, protocol.OpenAI, plan.ClientProtocol, plan.ResponseModel(), plan.OriginalBody, plan.TranslatedBody, body)
		if err != nil {
			return result, reqCtx.Duration().Seconds(), err
		}
	}
	headers = headers.Clone()
	headers.Set("Content-Type", "application/json")
	headers.Del("Content-Length")
	headers.Del("Content-Encoding")
	disableResponseWriteTimeout(w, "CodeBuddy非流式")
	filterAndWriteResponseHeaders(w, headers)
	w.WriteHeader(resp.StatusCode)
	_, err = w.Write(body)
	result.ResponseCommitted = true
	return result, reqCtx.Duration().Seconds(), err
}
