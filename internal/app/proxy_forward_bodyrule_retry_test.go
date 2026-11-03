package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
)

// 数组索引 BodyRule 在加密内容修复重试时不能再次删除已移位的历史条目。
func TestProxy_ArrayIndexBodyRuleNotReappliedOnEncryptedRetry(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusBadRequest, http.StatusBadGateway} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()

			const errBody = `{"error":{"message":"Encrypted function output content could not be decrypted or decoded.","type":"invalid_request_error","param":"","code":"invalid_encrypted_content"}}`

			rules := &model.CustomRequestRules{Body: []model.CustomBodyRule{{
				Action: model.RuleActionRemove,
				Path:   "input.1",
			}}}

			var attempts atomic.Int32
			var bodies [][]byte
			env := setupProxyTestEnv(t, []testChannel{
				{name: "codex-rule-retry", upstreamProtocol: "codex", models: "gpt-5.5", apiKey: "sk-codex", customRequestRules: rules},
			}, map[int]string{0: "https://codex-upstream.example.com"})

			env.server.client = &http.Client{
				Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					body, _ := io.ReadAll(r.Body)
					bodies = append(bodies, body)
					if attempts.Add(1) == 1 {
						return &http.Response{
							StatusCode: status,
							Header:     http.Header{"Content-Type": []string{"application/json"}},
							Body:       io.NopCloser(bytes.NewReader([]byte(errBody))),
						}, nil
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(bytes.NewReader([]byte(
							`{"id":"resp-1","object":"response","status":"completed","model":"gpt-5.5","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))),
					}, nil
				}),
			}

			w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model":  "gpt-5.5",
				"stream": false,
				"input": []map[string]any{
					{"type": "reasoning", "summary": []any{}, "encrypted_content": "bad-cipher"},
					{"type": "message", "role": "system", "content": []map[string]any{{"type": "input_text", "text": "DROP-ME"}}},
					{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "KEEP-ME"}}},
					{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "KEEP-ME-2"}}},
				},
			}, nil)

			if w.Code != http.StatusOK {
				t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
			}
			if len(bodies) != 2 {
				t.Fatalf("attempts=%d, expected 2", len(bodies))
			}

			firstInput := gjson.GetBytes(bodies[0], "input").Array()
			if len(firstInput) != 3 || firstInput[0].Get("type").String() != "reasoning" ||
				firstInput[1].Get("content.0.text").String() != "KEEP-ME" || firstInput[2].Get("content.0.text").String() != "KEEP-ME-2" {
				t.Fatalf("first request did not remove only DROP-ME: %s", bodies[0])
			}
			retryInput := gjson.GetBytes(bodies[1], "input").Array()
			if len(retryInput) != 2 || retryInput[0].Get("content.0.text").String() != "KEEP-ME" ||
				retryInput[1].Get("content.0.text").String() != "KEEP-ME-2" {
				t.Fatalf("retry must preserve both messages without reapplying the rule: %s", bodies[1])
			}
		})
	}
}

// TestCodexOAuthRetryPreservesFinalBodyRules verifies that a retry replay does
// not rerun the OAuth finalizer after channel rules have removed or overridden
// fields that the finalizer normally synthesizes.
func TestCodexOAuthRetryPreservesFinalBodyRules(t *testing.T) {
	t.Parallel()

	cfg := &model.Config{
		AuthType: model.AuthTypeCodexOAuth,
		CustomRequestRules: &model.CustomRequestRules{Body: []model.CustomBodyRule{
			{Action: model.RuleActionRemove, Path: "instructions"},
			{Action: model.RuleActionOverride, Path: "reasoning.effort", Value: json.RawMessage(`"high"`)},
			{Action: model.RuleActionOverride, Path: "parallel_tool_calls", Value: json.RawMessage(`true`)},
		}},
	}
	headers := http.Header{"Content-Type": []string{"application/json"}}
	original := []byte(`{"model":"gpt-5.6-sol","input":[]}`)

	wire, _, err := (&Server{}).prepareTranslatedUpstreamBody(
		cfg, protocol.Codex, "/v1/responses", "", original, original,
		"", headers, false, nil, false, false,
	)
	if err != nil {
		t.Fatalf("initial body finalization: %v", err)
	}
	if gjson.GetBytes(wire, "instructions").Exists() {
		t.Fatalf("initial wire body resurrected removed instructions: %s", wire)
	}
	if got := gjson.GetBytes(wire, "reasoning.effort").String(); got != "high" {
		t.Fatalf("initial reasoning.effort=%q, want high; body=%s", got, wire)
	}
	if !gjson.GetBytes(wire, "parallel_tool_calls").Bool() {
		t.Fatalf("initial parallel_tool_calls was not overridden: %s", wire)
	}

	replayed, _, err := (&Server{}).prepareTranslatedUpstreamBody(
		cfg, protocol.Codex, "/v1/responses", "", wire, wire,
		"", headers, false, nil, true, false,
	)
	if err != nil {
		t.Fatalf("retry body finalization: %v", err)
	}
	if gjson.GetBytes(replayed, "instructions").Exists() {
		t.Fatalf("retry resurrected removed instructions: %s", replayed)
	}
	if got := gjson.GetBytes(replayed, "reasoning.effort").String(); got != "high" {
		t.Fatalf("retry reasoning.effort=%q, want high; body=%s", got, replayed)
	}
	if !gjson.GetBytes(replayed, "parallel_tool_calls").Bool() {
		t.Fatalf("retry lost parallel_tool_calls override: %s", replayed)
	}
}
