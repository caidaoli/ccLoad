package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/cursorauth"
	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/util"
)

type fakeCursorRunner struct {
	model     string
	prompt    string
	text      string
	toolCalls []cursorauth.ToolCall
	err       error
	eventErr  error
	models    []string
	apiKey    string
	usage     *cursorauth.Usage
	raw       [][]byte
	request   cursorauth.Request
}

type failingCursorResponseWriter struct {
	header http.Header
}

type blockingCursorRunner struct {
	started chan struct{}
	release chan struct{}
}

type synchronousBlockingCursorRunner struct{}

func (synchronousBlockingCursorRunner) Run(
	ctx context.Context,
	_ *cursorauth.Credential,
	_ cursorauth.Request,
) (<-chan cursorauth.Event, error) {
	<-ctx.Done()
	return nil, context.Cause(ctx)
}

func (r *blockingCursorRunner) Run(
	ctx context.Context,
	_ *cursorauth.Credential,
	_ cursorauth.Request,
) (<-chan cursorauth.Event, error) {
	close(r.started)
	events := make(chan cursorauth.Event, 2)
	go func() {
		defer close(events)
		select {
		case <-ctx.Done():
			events <- cursorauth.Event{Done: true, Err: context.Cause(ctx)}
		case <-r.release:
			events <- cursorauth.Event{Delta: "hello", Text: "hello"}
			events <- cursorauth.Event{Text: "hello", Done: true}
		}
	}()
	return events, nil
}

func (w *failingCursorResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (*failingCursorResponseWriter) WriteHeader(int) {}

func (*failingCursorResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("broken pipe")
}

func (r *fakeCursorRunner) Run(_ context.Context, _ *cursorauth.Credential, request cursorauth.Request) (<-chan cursorauth.Event, error) {
	r.request = request
	r.model = request.Model
	r.prompt = request.Prompt
	if r.err != nil {
		return nil, r.err
	}
	raw := r.raw
	if len(raw) == 0 {
		payload, _ := json.Marshal(map[string]any{
			"sdk_message": map[string]any{
				"type": "assistant", "message": map[string]any{"text": r.text},
			},
		})
		raw = [][]byte{payload}
	}
	events := make(chan cursorauth.Event, len(raw)+len(r.toolCalls)+2)
	for _, payload := range raw {
		events <- cursorauth.Event{RawResponse: append([]byte(nil), payload...)}
	}
	if r.eventErr != nil {
		events <- cursorauth.Event{Text: r.text, Done: true, Err: r.eventErr, Usage: r.usage}
		close(events)
		return events, nil
	}
	if r.text != "" {
		events <- cursorauth.Event{Delta: r.text, Text: r.text}
	}
	for i := range r.toolCalls {
		call := r.toolCalls[i]
		events <- cursorauth.Event{Text: r.text, ToolCall: &call}
	}
	events <- cursorauth.Event{Text: r.text, Done: true, Usage: r.usage}
	close(events)
	return events, nil
}

func (r *fakeCursorRunner) ListModels(_ context.Context, apiKey string) ([]string, error) {
	r.apiKey = apiKey
	if r.err != nil {
		return nil, r.err
	}
	return append([]string(nil), r.models...), nil
}

func TestForwardCursorAgentWritesAnthropicMessage(t *testing.T) {
	t.Parallel()
	runner := &fakeCursorRunner{text: "hello", usage: &cursorauth.Usage{
		InputTokens: 11, OutputTokens: 7, CacheReadTokens: 5, CacheWriteTokens: 3,
		TotalTokens: 26, ReasoningTokens: 2,
	}}
	srv := newInMemoryServer(t)
	srv.cursorRunner = runner
	cfg := &model.Config{ID: 9, Name: "Cursor-test", AuthType: model.AuthTypeCursorOAuth}
	reqCtx := &proxyRequestContext{
		originalModel:  "claude-sonnet-5",
		clientProtocol: protocol.Anthropic,
		requestPath:    "/v1/messages",
		body:           []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"disabled"}}`),
	}
	rec := httptest.NewRecorder()
	result, err := srv.forwardCursorAgent(context.Background(), cfg, &cursorauth.Credential{AccessToken: "tok"}, reqCtx, rec)
	if err != nil || result == nil || !result.succeeded {
		t.Fatalf("result = %+v err = %v", result, err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Type  string `json:"type"`
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			CacheReadTokens    int `json:"cache_read_input_tokens"`
			CacheCreationToken int `json:"cache_creation_input_tokens"`
			ReasoningTokens    int `json:"reasoning_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &payload) != nil || payload.Type != "message" {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if payload.Usage.InputTokens != 11 || payload.Usage.OutputTokens != 7 ||
		payload.Usage.CacheReadTokens != 5 || payload.Usage.CacheCreationToken != 3 ||
		payload.Usage.ReasoningTokens != 2 {
		t.Fatalf("usage = %+v body = %s", payload.Usage, rec.Body.String())
	}
	if runner.model != "claude-sonnet-5" {
		t.Fatalf("model = %q", runner.model)
	}
	if !strings.Contains(runner.prompt, "hi") {
		t.Fatalf("prompt = %q", runner.prompt)
	}
}

func TestForwardCursorAgentAppearsInActiveRequestsEndpoint(t *testing.T) {
	t.Parallel()
	runner := &blockingCursorRunner{started: make(chan struct{}), release: make(chan struct{})}
	srv := newInMemoryServer(t)
	srv.cursorRunner = runner
	cfg := &model.Config{
		ID: 19, Name: "Cursor-active", AuthType: model.AuthTypeCursorOAuth, CostMultiplier: 1.25,
	}
	reqCtx := &proxyRequestContext{
		originalModel: "composer-2.5", clientProtocol: protocol.OpenAI,
		requestPath: "/v1/chat/completions", clientIP: "1.2.3.4", tokenID: 7,
		body: []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}]}`),
	}

	type outcome struct {
		result *proxyResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := srv.forwardCursorAgent(
			context.Background(), cfg, &cursorauth.Credential{APIKey: "cursor-user-api-key"}, reqCtx,
			httptest.NewRecorder(),
		)
		done <- outcome{result: result, err: err}
	}()

	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("Cursor runner did not start")
	}
	activeContext, activeWriter := newTestContext(t, newRequest(http.MethodGet, "/admin/active-requests", nil))
	srv.HandleActiveRequests(activeContext)
	var activeResponse activeRequestsResponse
	mustUnmarshalJSON(t, activeWriter.Body.Bytes(), &activeResponse)
	if activeResponse.Count != 1 || len(activeResponse.Data) != 1 {
		t.Fatalf("active requests response = %+v, want one Cursor request", activeResponse)
	}
	request := activeResponse.Data[0]
	if request.Model != "composer-2.5" || request.ClientIP != "1.2.3.4" ||
		request.ChannelID != 19 || request.ChannelName != "Cursor-active" ||
		request.ClientProtocol != string(protocol.OpenAI) || request.UpstreamProtocol != "cursor-sdk-bridge" ||
		request.TokenID != 7 || request.BaseURL != "http://cursor-sdk-bridge/sdk.v1.SdkAgentService/CreateAgent+Send" ||
		request.CostMultiplier != 1.25 || request.UpstreamStatus != activeRequestStatusRequesting {
		t.Fatalf("active request = %+v", request)
	}
	if request.APIKeyUsed == "" || request.APIKeyUsed == "cursor-user-api-key" {
		t.Fatalf("masked API key = %q", request.APIKeyUsed)
	}

	close(runner.release)
	select {
	case got := <-done:
		if got.err != nil || got.result == nil || !got.result.succeeded {
			t.Fatalf("result = %+v err = %v", got.result, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("Cursor request did not finish")
	}
	finishedContext, finishedWriter := newTestContext(t, newRequest(http.MethodGet, "/admin/active-requests", nil))
	srv.HandleActiveRequests(finishedContext)
	var finishedResponse activeRequestsResponse
	mustUnmarshalJSON(t, finishedWriter.Body.Bytes(), &finishedResponse)
	if finishedResponse.Count != 0 || len(finishedResponse.Data) != 0 {
		t.Fatalf("finished Cursor request leaked from active endpoint: %+v", finishedResponse)
	}
}

func TestForwardCursorAgentHonorsConfiguredTimeouts(t *testing.T) {
	const timeout = 20 * time.Millisecond
	tests := []struct {
		name        string
		streaming   bool
		synchronous bool
		firstByte   time.Duration
		streamTotal time.Duration
		nonStream   time.Duration
		wantStatus  int
		wantError   string
	}{
		{
			name: "stream first byte", streaming: true, firstByte: timeout,
			wantStatus: util.StatusFirstByteTimeout, wantError: "upstream first byte timeout",
		},
		{
			name: "stream first byte during synchronous runner", streaming: true, synchronous: true,
			firstByte: timeout, wantStatus: util.StatusFirstByteTimeout, wantError: "upstream first byte timeout",
		},
		{
			name: "stream total", streaming: true, streamTotal: timeout,
			wantStatus: util.StatusStreamIncomplete, wantError: "upstream stream timeout",
		},
		{
			name: "non-stream total", nonStream: timeout,
			wantStatus: http.StatusGatewayTimeout, wantError: "upstream timeout",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var runner cursorauth.Runner = &blockingCursorRunner{
				started: make(chan struct{}), release: make(chan struct{}),
			}
			if test.synchronous {
				runner = synchronousBlockingCursorRunner{}
			}
			srv := newInMemoryServer(t)
			srv.cursorRunner = runner
			srv.firstByteTimeout = test.firstByte
			srv.streamTimeout = test.streamTotal
			srv.nonStreamTimeout = test.nonStream
			body := []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}]}`)
			if test.streaming {
				body = []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}],"stream":true}`)
			}
			reqCtx := &proxyRequestContext{
				originalModel: "composer-2.5", clientProtocol: protocol.OpenAI,
				requestPath: "/v1/chat/completions", body: body,
				isStreaming: test.streaming, skipProxyLog: true,
			}

			started := time.Now()
			result, err := srv.forwardCursorAgent(
				context.Background(),
				&model.Config{ID: 21, Name: "Cursor-timeout", AuthType: model.AuthTypeCursorOAuth},
				&cursorauth.Credential{APIKey: "cursor-user-api-key"},
				reqCtx,
				httptest.NewRecorder(),
			)
			elapsed := time.Since(started)
			if err != nil {
				t.Fatalf("forwardCursorAgent() error = %v", err)
			}
			if result == nil || result.succeeded || result.status != test.wantStatus {
				t.Fatalf("result = %+v, want failed status %d", result, test.wantStatus)
			}
			if !strings.Contains(string(result.body), test.wantError) {
				t.Fatalf("body = %s, want %q", result.body, test.wantError)
			}
			if elapsed >= time.Second || result.duration <= 0 || result.duration >= 1 {
				t.Fatalf("elapsed = %v result.duration = %.3f, timeout was not enforced", elapsed, result.duration)
			}
			if result.firstByteTime != 0 {
				t.Fatalf("firstByteTime = %.3f, failed request produced no client byte", result.firstByteTime)
			}
		})
	}
}

func TestForwardCursorAgentDebugPreservesRawSDKEvents(t *testing.T) {
	t.Parallel()
	raw := [][]byte{
		[]byte(`{"sdk_message":{"type":"assistant","message":{"part":"one"}}}`),
		[]byte(`{"result":{"run_id":"run-1","result":{"result":"final"}}}`),
		[]byte(`{"done":{"run_id":"run-1"}}`),
	}
	runner := &fakeCursorRunner{text: "final", raw: raw}
	srv := newInMemoryServer(t)
	srv.cursorRunner = runner
	srv.configService.cache["debug_log_enabled"] = &model.SystemSetting{
		Key: "debug_log_enabled", Value: "true",
	}
	cfg := &model.Config{ID: 20, Name: "Cursor-debug", AuthType: model.AuthTypeCursorOAuth}
	reqCtx := &proxyRequestContext{
		originalModel: "composer-2.5", clientProtocol: protocol.OpenAI,
		requestPath: "/v1/chat/completions", skipProxyLog: true,
		body: []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}]}`),
	}

	result, err := srv.forwardCursorAgent(
		context.Background(), cfg, &cursorauth.Credential{APIKey: "cursor-user-api-key"}, reqCtx,
		httptest.NewRecorder(),
	)
	if err != nil || result == nil || !result.succeeded {
		t.Fatalf("result = %+v err = %v", result, err)
	}
	if reqCtx.debugData == nil {
		t.Fatal("Cursor debug data is missing")
	}
	want := string(raw[0]) + "\n" + string(raw[1]) + "\n" + string(raw[2]) + "\n"
	if got := string(reqCtx.debugData.RespBody); got != want {
		t.Fatalf("raw debug response = %q, want %q", got, want)
	}
	if !strings.Contains(reqCtx.debugData.RespHeaders, "application/x-ndjson") {
		t.Fatalf("raw debug response headers = %s", reqCtx.debugData.RespHeaders)
	}
}

func TestForwardCursorAgentMapsOpenAIUsageWithoutDoubleCountingCache(t *testing.T) {
	t.Parallel()
	runner := &fakeCursorRunner{text: "hello", usage: &cursorauth.Usage{
		InputTokens: 11, OutputTokens: 7, CacheReadTokens: 5, CacheWriteTokens: 3,
		TotalTokens: 26, ReasoningTokens: 2,
	}}
	srv := newInMemoryServer(t)
	srv.cursorRunner = runner
	cfg := &model.Config{ID: 10, Name: "Cursor-test", AuthType: model.AuthTypeCursorOAuth}
	reqCtx := &proxyRequestContext{
		originalModel: "gpt-5.6-sol", clientProtocol: protocol.OpenAI,
		requestPath: "/v1/chat/completions",
		body:        []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hi"}]}`),
	}
	rec := httptest.NewRecorder()
	result, err := srv.forwardCursorAgent(
		context.Background(), cfg, &cursorauth.Credential{AccessToken: "tok"}, reqCtx, rec,
	)
	if err != nil || result == nil || !result.succeeded {
		t.Fatalf("result = %+v err = %v", result, err)
	}
	var payload struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
			PromptDetails    struct {
				CachedTokens        int `json:"cached_tokens"`
				CacheCreationTokens int `json:"cached_creation_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v body = %s", err, rec.Body.String())
	}
	if payload.Usage.PromptTokens != 19 || payload.Usage.CompletionTokens != 7 ||
		payload.Usage.TotalTokens != 26 || payload.Usage.PromptDetails.CachedTokens != 5 ||
		payload.Usage.PromptDetails.CacheCreationTokens != 3 ||
		payload.Usage.CompletionDetails.ReasoningTokens != 2 {
		t.Fatalf("usage = %+v body = %s", payload.Usage, rec.Body.String())
	}
}

func TestForwardCursorAgentStreamsCacheUsage(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		protocol      protocol.Protocol
		path          string
		wantFragments []string
		wantStop      bool
	}{
		{
			name: "anthropic", protocol: protocol.Anthropic, path: "/v1/messages",
			wantFragments: []string{`"input_tokens":11`, `"output_tokens":7`, `"cache_read_input_tokens":5`, `"cache_creation_input_tokens":3`},
			wantStop:      true,
		},
		{
			name: "openai", protocol: protocol.OpenAI, path: "/v1/chat/completions",
			wantFragments: []string{`"prompt_tokens":19`, `"completion_tokens":7`, `"cached_tokens":5`, `"cached_creation_tokens":3`},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeCursorRunner{text: "hello", usage: &cursorauth.Usage{
				InputTokens: 11, OutputTokens: 7, CacheReadTokens: 5, CacheWriteTokens: 3, TotalTokens: 26,
			}}
			srv := newInMemoryServer(t)
			srv.cursorRunner = runner
			cfg := &model.Config{ID: 11, Name: "Cursor-test", AuthType: model.AuthTypeCursorOAuth}
			reqCtx := &proxyRequestContext{
				originalModel: "model-1", clientProtocol: test.protocol, requestPath: test.path,
				body:        []byte(`{"model":"model-1","messages":[{"role":"user","content":"hi"}],"stream":true}`),
				isStreaming: true,
			}
			rec := httptest.NewRecorder()
			result, err := srv.forwardCursorAgent(
				context.Background(), cfg, &cursorauth.Credential{AccessToken: "tok"}, reqCtx, rec,
			)
			if err != nil || result == nil || !result.succeeded {
				t.Fatalf("result = %+v err = %v", result, err)
			}
			for _, fragment := range test.wantFragments {
				if !strings.Contains(rec.Body.String(), fragment) {
					t.Fatalf("stream missing %s: %s", fragment, rec.Body.String())
				}
			}
			if test.wantStop {
				body := rec.Body.String()
				deltaAt := strings.Index(body, "event: message_delta")
				stopAt := strings.Index(body, "event: message_stop")
				if deltaAt < 0 || stopAt <= deltaAt || strings.Count(body, "event: message_stop") != 1 {
					t.Fatalf("Anthropic stream terminal order is invalid: %s", body)
				}
			}
		})
	}
}

func TestForwardCursorAgentReportsMissingCLI(t *testing.T) {
	t.Parallel()
	srv := newInMemoryServer(t)
	srv.cursorRunner = &fakeCursorRunner{err: cursorauth.ErrAgentMissing}
	cfg := &model.Config{ID: 9, AuthType: model.AuthTypeCursorOAuth}
	reqCtx := &proxyRequestContext{
		originalModel: "claude-sonnet-5", clientProtocol: protocol.OpenAI,
		requestPath: "/v1/chat/completions",
		body:        []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`),
	}
	rec := httptest.NewRecorder()
	result, err := srv.forwardCursorAgent(context.Background(), cfg, &cursorauth.Credential{AccessToken: "tok"}, reqCtx, rec)
	if err != nil || result == nil || result.succeeded || result.status != http.StatusServiceUnavailable {
		t.Fatalf("result = %+v err = %v", result, err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("uncommitted error must not write: %d %s", rec.Code, rec.Body.String())
	}
}

func TestForwardCursorAgentRejectsCredentialOnlyForStructuredUnauthorized(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		err    error
		status int
	}{
		{name: "plain text is adapter error", err: errors.New("cursor is not authenticated"), status: http.StatusBadGateway},
		{name: "structured unauthorized", err: &cursorauth.BridgeError{
			SDKCode: sdkv1.SdkErrorCode_SDK_ERROR_CODE_UNAUTHORIZED, Message: "bad key",
		}, status: http.StatusUnauthorized},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := newInMemoryServer(t)
			srv.cursorRunner = &fakeCursorRunner{eventErr: test.err}
			cfg := &model.Config{ID: 9, AuthType: model.AuthTypeCursorOAuth}
			reqCtx := &proxyRequestContext{
				originalModel: "claude-sonnet-5", clientProtocol: protocol.OpenAI,
				requestPath: "/v1/chat/completions",
				body:        []byte(`{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}]}`),
			}
			result, err := srv.forwardCursorAgent(
				context.Background(), cfg, &cursorauth.Credential{APIKey: "key"}, reqCtx, httptest.NewRecorder(),
			)
			if err != nil || result.status != test.status {
				t.Fatalf("result=%+v err=%v, want status %d", result, err, test.status)
			}
		})
	}
}

func TestTryCursorOAuthChannelSkipsUnsupportedFamilies(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	cfg := &model.Config{ID: 3, AuthType: model.AuthTypeCursorOAuth}
	result, err := srv.tryCursorOAuthChannel(context.Background(), cfg, &proxyRequestContext{
		requestPath: "/v1beta/models/test:generateContent",
	}, httptest.NewRecorder())
	if err != nil || result == nil || !result.protocolCapabilityMissing {
		t.Fatalf("result = %+v err = %v", result, err)
	}
}

func TestForwardCursorResponsesTreatsWriteFailureAsClientDisconnect(t *testing.T) {
	t.Parallel()
	srv := newInMemoryServer(t)
	srv.cursorRunner = &fakeCursorRunner{text: "hello"}
	originalBody := []byte(`{"model":"composer-2.5","input":[{"role":"user","content":"hello"}],"stream":true}`)
	translatedBody, err := srv.protocolRegistry.TranslateRequest(
		protocol.Codex, protocol.OpenAI, "composer-2.5", originalBody, true,
	)
	if err != nil {
		t.Fatalf("translate request: %v", err)
	}
	result, err := srv.forwardCursorAgent(
		context.Background(),
		&model.Config{ID: 12, AuthType: model.AuthTypeCursorOAuth},
		&cursorauth.Credential{APIKey: "key"},
		&proxyRequestContext{
			originalModel: "composer-2.5", clientProtocol: protocol.Codex,
			requestPath: "/v1/responses", body: originalBody, translatedBody: translatedBody,
			isStreaming: true, skipProxyLog: true,
		},
		&failingCursorResponseWriter{},
	)
	if err != nil || result == nil || result.status != StatusClientClosedRequest || !result.isClientCanceled {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCursorUsageSnapshotPersistsOnCredential(t *testing.T) {
	t.Parallel()
	cfg := newCursorOAuthChannel("Cursor-test", `{"type":"cursor","access_token":"tok"}`, []string{"claude-sonnet-5"})
	state, err := parseOAuthUsageCredentialState(cfg)
	if err != nil {
		t.Fatalf("parseOAuthUsageCredentialState() error = %v", err)
	}
	if state.provider != cursorauth.ChannelType || state.authType != model.AuthTypeCursorOAuth || state.tracksQuotaCost {
		t.Fatalf("state = %+v", state)
	}
	snapshot := []byte(`{"requested_at":"2026-08-18T00:00:00Z","sampled_at":"2026-08-18T00:00:01Z",` +
		`"summary":{"provider":"cursor","windows":[{"limit_name":"included","kind":"spend","used_percent":90.07,` +
		`"remaining_percent":9.93,"limit_window_seconds":2678399,"reset_at":1789181874}]}}`)
	payload, err := state.encode(snapshot, nil)
	if err != nil {
		t.Fatalf("encode() error = %v", err)
	}
	stored, err := cursorauth.ParseCredential([]byte(payload))
	if err != nil {
		t.Fatalf("ParseCredential() error = %v", err)
	}
	usage, _, _ := persistedOAuthUsage(stored.OAuthUsage, cursorauth.ChannelType)
	if usage == nil || len(usage.Windows) != 1 || usage.Windows[0].LimitName != "included" {
		t.Fatalf("persisted usage = %+v", usage)
	}
}

func TestNormalizeCursorUsageKeepsLimitMessageOffWarnings(t *testing.T) {
	t.Parallel()
	summary, err := normalizeCursorUsage(&cursorauth.PeriodUsage{
		PlanType:       "user",
		DisplayMessage: "You've hit your usage limit",
		Windows: []cursorauth.QuotaWindow{{
			Name: "api", Kind: "spend", UsedPercent: 100, RemainingPercent: 0,
			LimitWindowSeconds: 2678400, ResetAt: 1789181874,
		}},
	})
	if err != nil {
		t.Fatalf("normalizeCursorUsage() error = %v", err)
	}
	if summary.DisplayMessage != "You've hit your usage limit" || len(summary.Warnings) != 0 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.Windows[0].StandardCostMicroUSD != nil {
		t.Fatalf("cursor windows must not carry standard cost: %+v", summary.Windows[0])
	}
}

func TestForwardCursorAgentMapsAnthropicToolCalls(t *testing.T) {
	t.Parallel()
	runner := &fakeCursorRunner{
		text: "one sec",
		toolCalls: []cursorauth.ToolCall{{
			ID: "call_bash", Name: "bash", Arguments: json.RawMessage(`{"cmd":"ls"}`),
		}},
	}
	srv := newInMemoryServer(t)
	srv.cursorRunner = runner
	cfg := &model.Config{ID: 9, AuthType: model.AuthTypeCursorOAuth}
	reqCtx := &proxyRequestContext{
		originalModel: "claude-sonnet-5", clientProtocol: protocol.Anthropic,
		requestPath: "/v1/messages",
		body: []byte(`{
			"model":"claude-sonnet-5",
			"tools":[{"name":"bash","input_schema":{"type":"object"}}],
			"messages":[{"role":"user","content":"list files"}],
			"thinking":{"type":"disabled"}
		}`),
	}
	rec := httptest.NewRecorder()
	result, err := srv.forwardCursorAgent(context.Background(), cfg, &cursorauth.Credential{AccessToken: "tok"}, reqCtx, rec)
	if err != nil || result == nil || !result.succeeded {
		t.Fatalf("result = %+v err = %v", result, err)
	}
	if runner.prompt != "user: list files" || len(runner.request.Tools) != 1 ||
		runner.request.Tools[0].Name != "bash" {
		t.Fatalf("tools were not kept structured: request=%+v", runner.request)
	}
	var payload struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &payload) != nil {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if payload.StopReason != "tool_use" || len(payload.Content) != 2 ||
		payload.Content[0].Type != "text" || payload.Content[0].Text != "one sec" ||
		payload.Content[1].Type != "tool_use" || payload.Content[1].Name != "bash" {
		t.Fatalf("payload = %+v body = %s", payload, rec.Body.String())
	}
}
