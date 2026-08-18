package app

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/zaiauth"
)

// End-to-end proxy coverage for Z.ai Coding Plan channels: a downstream
// Anthropic request must reach the upstream carrying ZCode's identity, ZCode's
// authentication header and ZCode's device fingerprint.
func TestProxy_ZAICodingPlanReplicatesZCodeWire(t *testing.T) {
	t.Parallel()

	var upstreamHeaders http.Header
	var upstreamBody []byte
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHeaders = r.Header.Clone()
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"glm-4.7","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer upstream.Close()

	credential := &zaiauth.Credential{APIKey: "key-id.secret", Email: "user@example.com", UserID: "u-1"}
	credentialJSON, err := credential.JSON()
	if err != nil {
		t.Fatalf("encode z.ai credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "zai-coding-plan", upstreamProtocol: "anthropic", models: "glm-4.7",
		authType: model.AuthTypeZAIOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: upstream.URL})

	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model":      "glm-4.7",
		"max_tokens": 16,
		"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
	}, map[string]string{"anthropic-version": "2023-06-01"})
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}

	if got := anthropicHeaderValue(upstreamHeaders, "x-api-key"); got != "key-id.secret" {
		t.Fatalf("upstream x-api-key = %q", got)
	}
	if got := anthropicHeaderValue(upstreamHeaders, "Authorization"); got != "" {
		t.Fatalf("upstream Authorization = %q, ZCode sends x-api-key only", got)
	}
	for _, entry := range zaiauth.SourceHeaders() {
		if got := anthropicHeaderValue(upstreamHeaders, entry[0]); got != entry[1] {
			t.Fatalf("upstream %s = %q, want %q", entry[0], got, entry[1])
		}
	}

	var request map[string]any
	if err := json.Unmarshal(upstreamBody, &request); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	metadata, _ := request["metadata"].(map[string]any)
	rawIdentity, _ := metadata["user_id"].(string)
	var identity zaiRequestIdentity
	if err := json.Unmarshal([]byte(rawIdentity), &identity); err != nil {
		t.Fatalf("decode metadata.user_id %q: %v", rawIdentity, err)
	}
	if identity.DeviceID != credential.DeviceID || identity.DeviceID == "" {
		t.Fatalf("device id = %q, want %q", identity.DeviceID, credential.DeviceID)
	}
	if identity.SessionID == "" || identity.AccountUUID != "" {
		t.Fatalf("identity = %+v", identity)
	}
	if !strings.HasPrefix(rawIdentity, `{"device_id":`) {
		t.Fatalf("metadata.user_id shape = %s", rawIdentity)
	}
}

// A Coding Plan channel keeps serving OpenAI-shaped clients: ccLoad translates
// them locally to the Anthropic wire the ZCode endpoint expects.
func TestProxy_ZAICodingPlanTranslatesOpenAIClients(t *testing.T) {
	t.Parallel()

	var upstreamPath string
	var upstreamBody []byte
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"glm-4.7","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer upstream.Close()

	credentialJSON, err := (&zaiauth.Credential{APIKey: "key-id.secret"}).JSON()
	if err != nil {
		t.Fatalf("encode z.ai credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "zai-openai-client", upstreamProtocol: "anthropic", models: "glm-4.7",
		authType: model.AuthTypeZAIOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: upstream.URL})

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "glm-4.7",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}
	if !strings.HasSuffix(upstreamPath, "/v1/messages") {
		t.Fatalf("upstream path = %q", upstreamPath)
	}
	var request map[string]any
	if err := json.Unmarshal(upstreamBody, &request); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	metadata, _ := request["metadata"].(map[string]any)
	if _, ok := metadata["user_id"].(string); !ok {
		t.Fatalf("translated request is missing the ZCode fingerprint: %s", upstreamBody)
	}
}
