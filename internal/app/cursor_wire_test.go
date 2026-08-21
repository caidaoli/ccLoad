package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

type fakeCursorRunner struct {
	model  string
	prompt string
	text   string
	err    error
}

func (r *fakeCursorRunner) Run(_ context.Context, _ *cursorauth.Credential, model, prompt string) (<-chan cursorauth.Event, error) {
	r.model = model
	r.prompt = prompt
	if r.err != nil {
		return nil, r.err
	}
	events := make(chan cursorauth.Event, 2)
	events <- cursorauth.Event{Delta: r.text, Text: r.text}
	events <- cursorauth.Event{Text: r.text, Done: true}
	close(events)
	return events, nil
}

func TestForwardCursorAgentWritesAnthropicMessage(t *testing.T) {
	t.Parallel()
	runner := &fakeCursorRunner{text: "hello"}
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
	var payload map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &payload) != nil || payload["type"] != "message" {
		t.Fatalf("body = %s", rec.Body.String())
	}
	if runner.model != "claude-sonnet-5-high" {
		t.Fatalf("model = %q", runner.model)
	}
	if !strings.Contains(runner.prompt, "hi") {
		t.Fatalf("prompt = %q", runner.prompt)
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

func TestTryCursorOAuthChannelSkipsUnsupportedFamilies(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	cfg := &model.Config{ID: 3, AuthType: model.AuthTypeCursorOAuth}
	result, err := srv.tryCursorOAuthChannel(context.Background(), cfg, &proxyRequestContext{
		requestPath: "/v1/responses",
	}, httptest.NewRecorder())
	if err != nil || result == nil || !result.protocolCapabilityMissing {
		t.Fatalf("result = %+v err = %v", result, err)
	}
}

func TestCursorUsageSnapshotPersistsOnCredential(t *testing.T) {
	t.Parallel()
	cfg := newCursorOAuthChannel("Cursor-test", `{"type":"cursor","access_token":"tok"}`, []string{"claude-sonnet-5"})
	state, err := parseOAuthUsageCredentialState(cfg)
	if err != nil {
		t.Fatalf("parseOAuthUsageCredentialState() error = %v", err)
	}
	if state.provider != cursorauth.ChannelType || state.authType != model.AuthTypeCursorOAuth {
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

func TestForwardCursorAgentMapsAnthropicToolCalls(t *testing.T) {
	t.Parallel()
	runner := &fakeCursorRunner{text: "one sec\n<cc_tool_call>\n{\"name\":\"bash\",\"arguments\":{\"cmd\":\"ls\"}}\n</cc_tool_call>\n"}
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
	if !strings.Contains(runner.prompt, "<cc_tool_call>") || !strings.Contains(runner.prompt, "bash") {
		t.Fatalf("prompt missing tool catalog: %q", runner.prompt)
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
