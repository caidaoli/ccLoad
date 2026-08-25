package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/cursorauth"
	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
	"ccLoad/internal/model"
	"ccLoad/internal/util"
)

type cursorRefreshOnceRunner struct {
	calls            int
	accessTokens     []string
	outputThenReject bool
}

func (r *cursorRefreshOnceRunner) Run(
	_ context.Context,
	credential *cursorauth.Credential,
	_ cursorauth.Request,
) (<-chan cursorauth.Event, error) {
	r.calls++
	r.accessTokens = append(r.accessTokens, credential.AccessToken)
	events := make(chan cursorauth.Event, 2)
	if r.calls == 1 {
		if r.outputThenReject {
			events <- cursorauth.Event{Delta: "partial", Text: "partial"}
		}
		events <- cursorauth.Event{Done: true, Err: &cursorauth.BridgeError{
			SDKCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNAUTHORIZED,
			Message: "expired Cursor session",
		}}
	} else {
		events <- cursorauth.Event{Delta: "ok after refresh", Text: "ok after refresh", Done: true}
	}
	close(events)
	return events, nil
}

func TestProxy_CursorOAuthUsesCLIInsteadOfHTTP(t *testing.T) {
	t.Parallel()

	upstreamHits := 0
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()

	credentialJSON, err := (&cursorauth.Credential{AccessToken: "tok", Email: "user@example.com"}).JSON()
	if err != nil {
		t.Fatalf("encode cursor credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-cli", upstreamProtocol: "anthropic", models: "claude-sonnet-5",
		authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: upstream.URL})
	runner := &fakeCursorRunner{text: "ok from cli", usage: &cursorauth.Usage{
		InputTokens: 11, OutputTokens: 7, CacheReadTokens: 5, CacheWriteTokens: 3,
		TotalTokens: 26, ReasoningTokens: 2,
	}}
	env.server.cursorRunner = runner

	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model":      "claude-sonnet-5",
		"max_tokens": 16,
		"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
	}, map[string]string{"anthropic-version": "2023-06-01"})
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}
	if upstreamHits != 0 {
		t.Fatalf("Cursor OAuth must not HTTP-forward, hits = %d", upstreamHits)
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	content, _ := payload["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("body = %s", response.Body.String())
	}
	block, _ := content[0].(map[string]any)
	if block["text"] != "ok from cli" {
		t.Fatalf("text = %#v", block["text"])
	}
	if runner.model != "claude-sonnet-5" {
		t.Fatalf("model = %q", runner.model)
	}
	entry := waitForProxyLog(t, env, "claude-sonnet-5")
	if entry.InputTokens != 11 || entry.OutputTokens != 7 || entry.CacheReadInputTokens != 5 ||
		entry.CacheCreationInputTokens != 3 || entry.Cache5mInputTokens != 3 || entry.ReasoningTokens != 2 {
		t.Fatalf("logged usage = in:%d out:%d cache_read:%d cache_write:%d cache_5m:%d reasoning:%d",
			entry.InputTokens, entry.OutputTokens, entry.CacheReadInputTokens,
			entry.CacheCreationInputTokens, entry.Cache5mInputTokens, entry.ReasoningTokens)
	}
}

func TestProxy_CursorOAuthRemintsRejectedCredentialAndRetries(t *testing.T) {
	t.Parallel()
	credentialJSON, err := (&cursorauth.Credential{
		APIKey: "cursor-user-api-key", AccessToken: "cursor-access-old", RefreshToken: "cursor-refresh-old",
	}).JSON()
	if err != nil {
		t.Fatalf("encode Cursor credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-refresh", upstreamProtocol: "openai", models: "grok-4.6",
		authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://unused.invalid"})
	exchangeCalls := 0
	env.server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		exchangeCalls++
		if request.URL.Path != cursorauth.ExchangeAPIKeyPath {
			t.Fatalf("refresh path = %s", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); got != "Bearer cursor-user-api-key" {
			t.Fatalf("refresh authorization = %q", got)
		}
		return cursorTestHTTPResponse(
			request, http.StatusOK,
			`{"accessToken":"cursor-access-new","refreshToken":"cursor-refresh-new"}`,
		), nil
	})}
	env.server.cursorCredentials = newCursorCredentialManager(
		env.store, env.server.getClientForChannel, env.server.invalidateChannelRelatedCache,
	)
	runner := &cursorRefreshOnceRunner{}
	env.server.cursorRunner = runner

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "grok-4.6", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "ok after refresh") {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}
	if exchangeCalls != 1 || runner.calls != 2 ||
		len(runner.accessTokens) != 2 || runner.accessTokens[0] != "cursor-access-old" ||
		runner.accessTokens[1] != "cursor-access-new" {
		t.Fatalf("refresh calls=%d runner calls=%d access tokens=%v", exchangeCalls, runner.calls, runner.accessTokens)
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs() = (%d, %v)", len(configs), err)
	}
	persisted, err := cursorauth.ParseCredential([]byte(configs[0].OAuthCredential))
	if err != nil || persisted.AccessToken != "cursor-access-new" || persisted.RefreshToken != "cursor-refresh-new" {
		t.Fatalf("persisted credential = (%+v, %v)", persisted, err)
	}
}

func TestProxy_CursorOAuthDoesNotReplayStreamAfterOutput(t *testing.T) {
	t.Parallel()
	credentialJSON, err := (&cursorauth.Credential{
		APIKey: "cursor-user-api-key", AccessToken: "cursor-access-old",
	}).JSON()
	if err != nil {
		t.Fatalf("encode Cursor credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-no-stream-replay", upstreamProtocol: "openai", models: "grok-4.6",
		authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://unused.invalid"})
	exchangeCalls := 0
	env.server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
		exchangeCalls++
		return cursorTestHTTPResponse(
			request, http.StatusOK,
			`{"accessToken":"cursor-access-new","refreshToken":"cursor-refresh-new"}`,
		), nil
	})}
	env.server.cursorCredentials = newCursorCredentialManager(
		env.store, env.server.getClientForChannel, env.server.invalidateChannelRelatedCache,
	)
	runner := &cursorRefreshOnceRunner{outputThenReject: true}
	env.server.cursorRunner = runner

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "grok-4.6", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "partial") ||
		!strings.Contains(response.Body.String(), "expired Cursor session") {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}
	if exchangeCalls != 1 || runner.calls != 1 {
		t.Fatalf("stream refresh/replay mismatch: refresh calls=%d runner calls=%d", exchangeCalls, runner.calls)
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs() = (%d, %v)", len(configs), err)
	}
	persisted, err := cursorauth.ParseCredential([]byte(configs[0].OAuthCredential))
	if err != nil || persisted.AccessToken != "cursor-access-new" {
		t.Fatalf("persisted credential = (%+v, %v)", persisted, err)
	}
}

func TestProxy_CursorOAuthReplayDoesNotDuplicateUsageLog(t *testing.T) {
	t.Parallel()
	credentialJSON, err := (&cursorauth.Credential{
		APIKey: "cursor-user-api-key", AccessToken: "cursor-access-token", Email: "user@example.com",
	}).JSON()
	if err != nil {
		t.Fatalf("encode cursor credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-replay", upstreamProtocol: "openai", models: "grok-4.6",
		authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://unused.invalid"})
	env.server.cursorRunner = &fakeCursorRunner{
		text: "final", replayed: true, usage: &cursorauth.Usage{InputTokens: 100, OutputTokens: 20},
	}

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "grok-4.6",
		"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function",
				"function": map[string]any{"name": "lookup", "arguments": `{}`},
			}}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "value"},
		},
	}, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "final") {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}
	time.Sleep(50 * time.Millisecond)
	logs, err := env.store.ListLogs(
		context.Background(), time.Now().Add(-time.Minute), 10, 0,
		&model.LogFilter{Model: "grok-4.6", LogSource: model.LogSourceProxy},
	)
	if err != nil {
		t.Fatalf("GetLogs() error = %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("replayed request wrote %d duplicate logs", len(logs))
	}
}

func TestProxy_CursorOAuthToolTurnReturnsEstimatedUsageWithoutBilling(t *testing.T) {
	t.Parallel()
	credentialJSON, err := (&cursorauth.Credential{
		APIKey: "cursor-user-api-key", AccessToken: "cursor-access-token", Email: "user@example.com",
	}).JSON()
	if err != nil {
		t.Fatalf("encode cursor credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-tool-usage", upstreamProtocol: "openai", models: "grok-4.6",
		authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://unused.invalid"})
	runner := &fakeCursorRunner{
		toolCalls:          []cursorauth.ToolCall{{ID: "call_lookup", Name: "lookup", Arguments: json.RawMessage(`{}`)}},
		toolUsage:          &cursorauth.Usage{InputTokens: 100, TotalTokens: 100},
		toolUsageEstimated: true,
	}
	env.server.cursorRunner = runner

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "grok-4.6", "messages": []any{map[string]any{"role": "user", "content": "lookup"}},
		"tools": []any{map[string]any{
			"type": "function", "function": map[string]any{
				"name": "lookup", "parameters": map[string]any{"type": "object"},
			},
		}},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v body=%s", err, response.Body.String())
	}
	if payload.Usage.PromptTokens <= 0 || payload.Usage.CompletionTokens != 0 || payload.Usage.TotalTokens != payload.Usage.PromptTokens {
		t.Fatalf("tool response usage = %+v", payload.Usage)
	}
	if runner.request.InputTokenEstimate <= 0 {
		t.Fatalf("runner request is missing the input-token estimate: %+v", runner.request)
	}

	entry := waitForProxyLog(t, env, "grok-4.6")
	if entry.InputTokens != 0 || entry.OutputTokens != 0 || entry.CacheReadInputTokens != 0 {
		t.Fatalf("estimated intermediate usage must not be billed: %+v", entry)
	}
}

func TestProxy_CursorOAuthResumedTurnDoesNotExposeCumulativeRunUsage(t *testing.T) {
	t.Parallel()
	credentialJSON, err := (&cursorauth.Credential{
		APIKey: "cursor-user-api-key", AccessToken: "cursor-access-token", Email: "user@example.com",
	}).JSON()
	if err != nil {
		t.Fatalf("encode cursor credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-resume-usage", upstreamProtocol: "openai", models: "grok-4.6",
		authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://unused.invalid"})
	env.server.cursorRunner = &fakeCursorRunner{
		text: "final answer", usage: &cursorauth.Usage{
			InputTokens: 900_000, OutputTokens: 5_000, CacheReadTokens: 800_000, TotalTokens: 1_705_000,
		},
	}

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "grok-4.6",
		"messages": []any{
			map[string]any{"role": "user", "content": "find it"},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function",
				"function": map[string]any{"name": "lookup", "arguments": `{}`},
			}}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "result"},
		},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}
	var payload struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v body=%s", err, response.Body.String())
	}
	if payload.Usage.PromptTokens <= 0 || payload.Usage.PromptTokens >= 900_000 ||
		payload.Usage.CompletionTokens <= 0 || payload.Usage.TotalTokens != payload.Usage.PromptTokens+payload.Usage.CompletionTokens {
		t.Fatalf("resumed response exposed invalid usage: %+v", payload.Usage)
	}

	entry := waitForProxyLog(t, env, "grok-4.6")
	if entry.InputTokens != 900_000 || entry.OutputTokens != 5_000 || entry.CacheReadInputTokens != 800_000 {
		t.Fatalf("terminal cumulative usage was not logged for billing: %+v", entry)
	}
}

func TestProxy_CursorOAuthFirstByteTimeoutPersistsActualDuration(t *testing.T) {
	credentialJSON, err := (&cursorauth.Credential{
		APIKey: "cursor-user-api-key", AccessToken: "cursor-access-token", Email: "user@example.com",
	}).JSON()
	if err != nil {
		t.Fatalf("encode cursor credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-timeout", upstreamProtocol: "openai", models: "composer-2.5",
		authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://unused.invalid"})
	env.server.firstByteTimeout = 20 * time.Millisecond
	env.server.streamTimeout = 0
	env.server.cursorRunner = &blockingCursorRunner{started: make(chan struct{}), release: make(chan struct{})}

	started := time.Now()
	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "composer-2.5", "messages": []any{
			map[string]any{"role": "user", "content": "hello"},
		},
		"stream": true,
	}, nil)
	elapsed := time.Since(started)
	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}
	entry := waitForProxyLog(t, env, "composer-2.5")
	if entry.StatusCode != util.StatusFirstByteTimeout || !entry.IsStreaming {
		t.Fatalf("log status = %d streaming = %v", entry.StatusCode, entry.IsStreaming)
	}
	if entry.FirstByteTime != 0 {
		t.Fatalf("first byte = %.3f, timeout emitted no client content", entry.FirstByteTime)
	}
	if elapsed >= time.Second || entry.Duration <= 0 || entry.Duration >= 1 {
		t.Fatalf("elapsed = %v logged duration = %.3f", elapsed, entry.Duration)
	}
	if !strings.Contains(entry.Message, "upstream first byte timeout") {
		t.Fatalf("log message = %q", entry.Message)
	}
}

func TestProxy_CursorOAuthSupportsResponses(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "non-stream"
		if streaming {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			upstreamHits := 0
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamHits++
				w.WriteHeader(http.StatusTeapot)
			}))
			defer upstream.Close()

			credentialJSON, err := (&cursorauth.Credential{
				APIKey: "cursor-user-api-key", AccessToken: "cursor-access-token", Email: "user@example.com",
			}).JSON()
			if err != nil {
				t.Fatalf("encode cursor credential: %v", err)
			}
			env := setupProxyTestEnv(t, []testChannel{{
				name: "cursor-responses", upstreamProtocol: "openai", models: "composer-2.5",
				authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
			}}, map[int]string{0: upstream.URL})
			if streaming {
				env.server.configService.cache["debug_log_enabled"] = &model.SystemSetting{
					Key: "debug_log_enabled", Value: "true",
				}
			}
			runner := &fakeCursorRunner{text: "MacBook M5 answer", usage: &cursorauth.Usage{
				InputTokens: 11, OutputTokens: 7, CacheReadTokens: 5, CacheWriteTokens: 3,
			}}
			env.server.cursorRunner = runner

			response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model": "composer-2.5",
				"input": []any{
					map[string]any{"role": "system", "content": "使用中文回复"},
					map[string]any{"role": "user", "content": []any{
						map[string]any{"type": "input_text", "text": "macbook m5有几款\n\n"},
					}},
				},
				"temperature": "[undefined]", "top_p": "[undefined]",
				"max_output_tokens": "[undefined]", "instructions": "[undefined]",
				"tools": "[undefined]", "tool_choice": "[undefined]", "stream": streaming,
			}, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
			}
			if upstreamHits != 0 {
				t.Fatalf("Cursor OAuth must not HTTP-forward, hits = %d", upstreamHits)
			}
			if !strings.Contains(runner.prompt, "使用中文回复") || !strings.Contains(runner.prompt, "macbook m5有几款") {
				t.Fatalf("translated prompt lost Responses messages: %q", runner.prompt)
			}
			if !strings.Contains(runner.prompt, "[undefined]") {
				t.Fatalf("caller-provided prompt content was rewritten: %q", runner.prompt)
			}
			if streaming {
				entry := waitForProxyLog(t, env, "composer-2.5")
				debugLog, err := env.store.GetDebugLogByLogID(context.Background(), entry.ID)
				if err != nil || debugLog == nil {
					t.Fatalf("load Cursor Responses debug log: debug=%+v err=%v", debugLog, err)
				}
				if !debugLog.ProtocolTransformed || !bytes.Equal(debugLog.TranslatedRespBody, response.Body.Bytes()) {
					t.Fatalf("translated debug response must match client body:\ndebug=%s\nclient=%s",
						debugLog.TranslatedRespBody, response.Body.Bytes())
				}
			}

			if streaming {
				var delta, completed bool
				for _, block := range bytes.Split(response.Body.Bytes(), []byte("\n\n")) {
					eventType, data := parseSSEEventChunk(block)
					payload, ok := decodeSSEPayload(data)
					if !ok {
						continue
					}
					switch eventType {
					case "response.output_text.delta":
						delta = payload["delta"] == "MacBook M5 answer"
					case "response.completed":
						completed = true
					}
				}
				if !delta || !completed {
					t.Fatalf("invalid Responses stream: %s", response.Body.String())
				}
				return
			}

			var payload struct {
				Object string `json:"object"`
				Output []struct {
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"output"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode Responses body: %v body=%s", err, response.Body.String())
			}
			if payload.Object != "response" || len(payload.Output) == 0 ||
				len(payload.Output[0].Content) == 0 || payload.Output[0].Content[0].Text != "MacBook M5 answer" {
				t.Fatalf("invalid Responses body: %s", response.Body.String())
			}
		})
	}
}

func TestProxy_CursorOAuthResponsesEmitsNativeToolCalls(t *testing.T) {
	t.Parallel()

	credentialJSON, err := (&cursorauth.Credential{
		APIKey: "cursor-user-api-key", AccessToken: "cursor-access-token", Email: "user@example.com",
	}).JSON()
	if err != nil {
		t.Fatalf("encode cursor credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "cursor-responses-tools", upstreamProtocol: "openai", models: "composer-2.5",
		authType: model.AuthTypeCursorOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: "https://unused.invalid"})
	env.server.cursorRunner = &fakeCursorRunner{
		text: "Checking.",
		toolCalls: []cursorauth.ToolCall{
			{ID: "call_readme", Name: "read", Arguments: json.RawMessage(`{"path":"README.md"}`)},
			{ID: "call_claude", Name: "read", Arguments: json.RawMessage(`{"path":"CLAUDE.md"}`)},
		},
	}

	response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "composer-2.5",
		"input": []any{map[string]any{"role": "user", "content": "read the project overview"}},
		"tools": []any{map[string]any{
			"type": "function", "name": "read", "description": "read a file",
			"parameters": map[string]any{
				"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required": []any{"path"},
			},
		}},
		"tool_choice": "auto", "stream": true,
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}

	var outputText strings.Builder
	var functionNames, functionArguments []string
	for _, block := range bytes.Split(response.Body.Bytes(), []byte("\n\n")) {
		eventType, data := parseSSEEventChunk(block)
		payload, ok := decodeSSEPayload(data)
		if !ok {
			continue
		}
		switch eventType {
		case "response.output_text.delta":
			delta, _ := payload["delta"].(string)
			outputText.WriteString(delta)
		case "response.output_item.done":
			item, _ := payload["item"].(map[string]any)
			if item["type"] == "function_call" {
				name, _ := item["name"].(string)
				arguments, _ := item["arguments"].(string)
				functionNames = append(functionNames, name)
				functionArguments = append(functionArguments, arguments)
			}
		}
	}
	if outputText.String() != "Checking." {
		t.Fatalf("output text leaked tool framing: %q", outputText.String())
	}
	if len(functionNames) != 2 || len(functionArguments) != 2 {
		t.Fatalf("function calls = %q %q", functionNames, functionArguments)
	}
	for i, wantPath := range []string{"README.md", "CLAUDE.md"} {
		var arguments map[string]any
		if functionNames[i] != "read" || json.Unmarshal([]byte(functionArguments[i]), &arguments) != nil ||
			arguments["path"] != wantPath {
			t.Fatalf("function call %d = %q %q", i, functionNames[i], functionArguments[i])
		}
	}
}
