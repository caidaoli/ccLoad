package app

import (
	"context"
	"net/http"
	"regexp"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func resetCodexSessionCache() {
	codexSessionMu.Lock()
	codexSessionMap = make(map[string]codexSessionEntry)
	codexSessionMu.Unlock()
}

func TestResolveCodexSessionHint_AnthropicWithUserID(t *testing.T) {
	resetCodexSessionCache()

	rc := &requestContext{
		clientProtocol:   protocol.Anthropic,
		upstreamProtocol: protocol.Codex,
		originalModel:    "gpt-5-codex",
		originalBody:     []byte(`{"metadata":{"user_id":"abc123"}}`),
	}
	id1 := resolveCodexSessionHint(rc, nil, "")
	if !uuidPattern.MatchString(id1) {
		t.Fatalf("expected UUID, got %q", id1)
	}
	// 相同 user_id 再次调用应返回同一 UUID（命中缓存）
	id2 := resolveCodexSessionHint(rc, nil, "")
	if id1 != id2 {
		t.Fatalf("expected cached session id, got %q vs %q", id1, id2)
	}
}

func TestResolveCodexSessionHint_AnthropicDifferentModelsOrUsers(t *testing.T) {
	resetCodexSessionCache()

	mkCtx := func(model, userID string) *requestContext {
		return &requestContext{
			clientProtocol:   protocol.Anthropic,
			upstreamProtocol: protocol.Codex,
			originalModel:    model,
			originalBody:     []byte(`{"metadata":{"user_id":"` + userID + `"}}`),
		}
	}
	idA := resolveCodexSessionHint(mkCtx("m1", "u1"), nil, "")
	idB := resolveCodexSessionHint(mkCtx("m1", "u2"), nil, "")
	idC := resolveCodexSessionHint(mkCtx("m2", "u1"), nil, "")
	if idA == idB || idA == idC || idB == idC {
		t.Fatalf("expected distinct UUIDs for distinct buckets; got %s %s %s", idA, idB, idC)
	}
}

func TestResolveCodexSessionHint_AnthropicMissingUserID(t *testing.T) {
	resetCodexSessionCache()

	rc := &requestContext{
		clientProtocol:   protocol.Anthropic,
		upstreamProtocol: protocol.Codex,
		originalBody:     []byte(`{}`),
	}
	if got := resolveCodexSessionHint(rc, nil, ""); got != "" {
		t.Fatalf("expected empty when metadata.user_id missing, got %q", got)
	}
}

func TestResolveCodexSessionHint_CodexPassthrough(t *testing.T) {
	rc := &requestContext{
		clientProtocol:   protocol.Codex,
		upstreamProtocol: protocol.Codex,
	}
	body := []byte(`{"prompt_cache_key":"existing-uuid","model":"gpt-5-codex"}`)
	if got := resolveCodexSessionHint(rc, body, ""); got != "existing-uuid" {
		t.Fatalf("expected passthrough prompt_cache_key, got %q", got)
	}

	if got := resolveCodexSessionHint(rc, []byte(`{"model":"x"}`), ""); got != "" {
		t.Fatalf("expected empty when codex body has no prompt_cache_key, got %q", got)
	}
}

func TestResolveCodexSessionHint_OpenAIDeterministic(t *testing.T) {
	rc := &requestContext{
		clientProtocol:   protocol.OpenAI,
		upstreamProtocol: protocol.Codex,
	}
	a1 := resolveCodexSessionHint(rc, nil, "sk-abc")
	a2 := resolveCodexSessionHint(rc, nil, "sk-abc")
	b := resolveCodexSessionHint(rc, nil, "sk-xyz")
	if a1 == "" || !uuidPattern.MatchString(a1) {
		t.Fatalf("expected UUID for openai key, got %q", a1)
	}
	if a1 != a2 {
		t.Fatalf("expected deterministic UUID for same key, got %q vs %q", a1, a2)
	}
	if a1 == b {
		t.Fatalf("expected different UUIDs for different keys")
	}
	if got := resolveCodexSessionHint(rc, nil, ""); got != "" {
		t.Fatalf("expected empty for empty apiKey, got %q", got)
	}
}

func TestResolveCodexSessionHint_NonCodexUpstream(t *testing.T) {
	rc := &requestContext{
		clientProtocol:   protocol.Anthropic,
		upstreamProtocol: protocol.Anthropic,
		originalBody:     []byte(`{"metadata":{"user_id":"abc"}}`),
	}
	if got := resolveCodexSessionHint(rc, nil, ""); got != "" {
		t.Fatalf("expected empty when upstream is not Codex, got %q", got)
	}
}

func TestInjectCodexPromptCacheKey(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[]}`)
	out := injectCodexPromptCacheKey(body, "deadbeef")
	if readCodexPromptCacheKey(out) != "deadbeef" {
		t.Fatalf("expected prompt_cache_key injected, got %s", out)
	}

	// 已存在非空值时不覆盖
	preset := []byte(`{"prompt_cache_key":"kept","model":"x"}`)
	out2 := injectCodexPromptCacheKey(preset, "new")
	if readCodexPromptCacheKey(out2) != "kept" {
		t.Fatalf("expected existing prompt_cache_key preserved, got %s", out2)
	}

	// 空 body / 空 id / 非 JSON 原样返回
	if got := injectCodexPromptCacheKey(nil, "x"); got != nil {
		t.Fatalf("expected nil body preserved, got %s", got)
	}
	if got := injectCodexPromptCacheKey(body, ""); string(got) != string(body) {
		t.Fatalf("expected body unchanged for empty id")
	}
	raw := []byte(`not json`)
	if got := injectCodexPromptCacheKey(raw, "x"); string(got) != string(raw) {
		t.Fatalf("expected non-json body unchanged")
	}
}

func TestExtractAnthropicUserID(t *testing.T) {
	cases := []struct {
		name string
		body []byte
		want string
	}{
		{"happy", []byte(`{"metadata":{"user_id":"  abc123  "}}`), "abc123"},
		{"missing metadata", []byte(`{"model":"x"}`), ""},
		{"missing user_id", []byte(`{"metadata":{}}`), ""},
		{"empty body", nil, ""},
		{"invalid json", []byte(`not json`), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractAnthropicUserID(tc.body); got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
		})
	}
}

func TestCodexUUIDHelpers(t *testing.T) {
	v4 := newCodexUUIDv4()
	if !uuidPattern.MatchString(v4) {
		t.Fatalf("v4 format invalid: %q", v4)
	}
	if v4[14] != '4' {
		t.Fatalf("v4 version nibble expected '4', got %q", v4)
	}
	if v4[19] != '8' && v4[19] != '9' && v4[19] != 'a' && v4[19] != 'b' {
		t.Fatalf("v4 variant nibble unexpected: %q", v4)
	}

	v5a := newCodexUUIDv5(uuidNameSpaceOID, "foo")
	v5b := newCodexUUIDv5(uuidNameSpaceOID, "foo")
	v5c := newCodexUUIDv5(uuidNameSpaceOID, "bar")
	if v5a != v5b {
		t.Fatalf("v5 not deterministic: %q vs %q", v5a, v5b)
	}
	if v5a == v5c {
		t.Fatalf("v5 collision for different names")
	}
	if !uuidPattern.MatchString(v5a) {
		t.Fatalf("v5 format invalid: %q", v5a)
	}
	if v5a[14] != '5' {
		t.Fatalf("v5 version nibble expected '5', got %q", v5a)
	}
}

func TestBuildProxyRequest_CodexSessionInjection_Anthropic(t *testing.T) {
	resetCodexSessionCache()
	srv := newInMemoryServer(t)

	cfg := &model.Config{
		ID:          1,
		Name:        "codex-ch",
		URL:         "https://api.example.com",
		ChannelType: "openai",
	}

	originalBody := []byte(`{"metadata":{"user_id":"claude-code-user-42"}}`)
	translatedBody := []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)

	reqCtx := &requestContext{
		ctx:              context.Background(),
		startTime:        time.Now(),
		clientProtocol:   protocol.Anthropic,
		upstreamProtocol: protocol.Codex,
		originalModel:    "gpt-5-codex",
		originalBody:     originalBody,
		translatedBody:   translatedBody,
	}

	req, err := srv.buildProxyRequest(
		reqCtx,
		cfg,
		"sk-test",
		http.MethodPost,
		translatedBody,
		http.Header{"Content-Type": []string{"application/json"}},
		"",
		"/v1/responses",
		cfg.URL,
	)
	if err != nil {
		t.Fatalf("buildProxyRequest failed: %v", err)
	}

	sid := req.Header.Get("Session_id")
	if !uuidPattern.MatchString(sid) {
		t.Fatalf("Session_id header missing or invalid: %q", sid)
	}

	bodyReader, _ := req.GetBody()
	defer func() { _ = bodyReader.Close() }()
	buf := make([]byte, 4096)
	n, _ := bodyReader.Read(buf)
	if key := readCodexPromptCacheKey(buf[:n]); key != sid {
		t.Fatalf("expected body prompt_cache_key == Session_id header, got body=%q header=%q", key, sid)
	}
}

func TestBuildProxyRequest_CodexSessionInjection_NonCodexUpstreamSkipped(t *testing.T) {
	resetCodexSessionCache()
	srv := newInMemoryServer(t)

	cfg := &model.Config{
		ID:          1,
		Name:        "anthropic-ch",
		URL:         "https://api.example.com",
		ChannelType: "anthropic",
	}

	reqCtx := &requestContext{
		ctx:              context.Background(),
		startTime:        time.Now(),
		clientProtocol:   protocol.Anthropic,
		upstreamProtocol: protocol.Anthropic,
		originalModel:    "claude-3",
		originalBody:     []byte(`{"metadata":{"user_id":"u1"}}`),
	}

	req, err := srv.buildProxyRequest(
		reqCtx,
		cfg,
		"sk-test",
		http.MethodPost,
		[]byte(`{"model":"claude-3"}`),
		http.Header{"Content-Type": []string{"application/json"}},
		"",
		"/v1/messages",
		cfg.URL,
	)
	if err != nil {
		t.Fatalf("buildProxyRequest failed: %v", err)
	}

	if got := req.Header.Get("Session_id"); got != "" {
		t.Fatalf("expected no Session_id for non-Codex upstream, got %q", got)
	}
}

func TestBuildProxyRequest_CodexSessionInjection_ClientHeaderNotOverwritten(t *testing.T) {
	resetCodexSessionCache()
	srv := newInMemoryServer(t)

	cfg := &model.Config{
		ID:          1,
		Name:        "codex-ch",
		URL:         "https://api.example.com",
		ChannelType: "openai",
	}

	reqCtx := &requestContext{
		ctx:              context.Background(),
		startTime:        time.Now(),
		clientProtocol:   protocol.Codex,
		upstreamProtocol: protocol.Codex,
		originalModel:    "gpt-5-codex",
		originalBody:     []byte(`{"prompt_cache_key":"client-supplied","model":"gpt-5-codex"}`),
	}

	req, err := srv.buildProxyRequest(
		reqCtx,
		cfg,
		"sk-test",
		http.MethodPost,
		[]byte(`{"prompt_cache_key":"client-supplied","model":"gpt-5-codex"}`),
		http.Header{
			"Content-Type": []string{"application/json"},
			"Session_id":   []string{"client-session"},
		},
		"",
		"/v1/responses",
		cfg.URL,
	)
	if err != nil {
		t.Fatalf("buildProxyRequest failed: %v", err)
	}

	if got := req.Header.Get("Session_id"); got != "client-session" {
		t.Fatalf("expected client Session_id preserved, got %q", got)
	}
}
