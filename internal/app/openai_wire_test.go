package app

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
)

const openAIChatCompletionOK = `{"id":"chatcmpl_1","object":"chat.completion","model":"gpt-5.4","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

func TestProxy_OpenAIChatMaxCompletionTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		model      string
		rejectOnce bool
		wantBodies int
		wantMax    string
	}{
		{name: "reasoning family renamed before send", model: "azure/gpt-5.4", wantBodies: 1, wantMax: "max_completion_tokens"},
		{name: "o series renamed before send", model: "o3-mini", wantBodies: 1, wantMax: "max_completion_tokens"},
		{name: "other models keep max_tokens", model: "gpt-4o", wantBodies: 1, wantMax: "max_tokens"},
		{name: "unknown model renamed after rejection", model: "custom-reasoner", rejectOnce: true, wantBodies: 2, wantMax: "max_completion_tokens"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var bodies [][]byte
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				bodies = append(bodies, body)
				first := len(bodies) == 1
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if test.rejectOnce && first {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.","type":"invalid_request_error","param":"max_tokens","code":"unsupported_parameter"}}`)
					return
				}
				_, _ = io.WriteString(w, openAIChatCompletionOK)
			}))
			defer upstream.Close()

			env := setupProxyTestEnv(t, []testChannel{{
				name: "openai-max-tokens", upstreamProtocol: "openai", models: test.model,
			}}, map[int]string{0: upstream.URL})
			w := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
				"model":      test.model,
				"max_tokens": 128,
				"messages":   []map[string]string{{"role": "user", "content": "hi"}},
			}, map[string]string{"anthropic-version": "2023-06-01"})
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			mu.Lock()
			defer mu.Unlock()
			if len(bodies) != test.wantBodies {
				t.Fatalf("upstream requests=%d, want %d", len(bodies), test.wantBodies)
			}
			final := bodies[len(bodies)-1]
			other := "max_tokens"
			if test.wantMax == "max_tokens" {
				other = "max_completion_tokens"
			}
			if gjson.GetBytes(final, test.wantMax).Int() != 128 || gjson.GetBytes(final, other).Exists() {
				t.Fatalf("final upstream body=%s, want only %s=128", final, test.wantMax)
			}
		})
	}
}

func TestPrepareTranslatedUpstreamBodyMaxTokensRespectsBodyRules(t *testing.T) {
	t.Parallel()
	cfg := &model.Config{CustomRequestRules: &model.CustomRequestRules{Body: []model.CustomBodyRule{
		{Action: model.RuleActionOverride, Path: "max_tokens", Value: json.RawMessage(`64`)},
	}}}
	headers := http.Header{"Content-Type": []string{"application/json"}}
	body := []byte(`{"model":"gpt-5.4","max_tokens":128,"max_completion_tokens":256,"messages":[]}`)

	got, _, err := (&Server{}).prepareTranslatedUpstreamBody(
		cfg, protocol.OpenAI, "/v1/chat/completions", "gpt-5.4", body, body,
		"", headers, false, nil, false, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	// 已有 max_completion_tokens 为准；渠道规则仍能显式写回 max_tokens。
	if gjson.GetBytes(got, "max_completion_tokens").Int() != 256 || gjson.GetBytes(got, "max_tokens").Int() != 64 {
		t.Fatalf("body=%s, want max_completion_tokens=256 and rule max_tokens=64", got)
	}
}
