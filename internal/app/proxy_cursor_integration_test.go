package app

import (
	"encoding/json"
	"net/http"
	"testing"

	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
)

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
	runner := &fakeCursorRunner{text: "ok from cli"}
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
	if runner.model != "claude-sonnet-5-thinking-high" {
		t.Fatalf("model = %q", runner.model)
	}
}
