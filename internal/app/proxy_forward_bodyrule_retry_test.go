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

// TestProxy_ArrayIndexBodyRuleNotReappliedOn400Retry 验证数组索引 BodyRule
// 在 400 自愈重试时不会被二次应用导致 input 元素错位丢失。
func TestProxy_ArrayIndexBodyRuleNotReappliedOn400Retry(t *testing.T) {
	t.Parallel()

	const errBody = `{"error":{"message":"The encrypted content could not be verified. Reason: Encrypted content could not be decrypted or parsed.","type":"invalid_request_error","param":"","code":"invalid_encrypted_content"}}`

	rules := &model.CustomRequestRules{Body: []model.CustomBodyRule{{
		Action: model.RuleActionRemove,
		Path:   "input.0",
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
					StatusCode: http.StatusBadRequest,
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
			{"type": "message", "role": "system", "content": []map[string]any{{"type": "input_text", "text": "DROP-ME"}}},
			{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "KEEP-ME"}}},
			{"type": "reasoning", "summary": []any{}, "encrypted_content": "bad-cipher"},
			{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "KEEP-ME-2"}}},
		},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(bodies) < 2 {
		t.Fatalf("attempts=%d, expected >=2 (first 400, then retry)", len(bodies))
	}

	// 首次请求：input.0 (DROP-ME) 应被 BodyRule 移除。
	if bytes.Contains(bodies[0], []byte("DROP-ME")) {
		t.Errorf("first attempt should not contain DROP-ME after body rule")
	}

	// 重试请求：KEEP-ME 不应被二次删除。
	var retryParsed map[string]any
	if err := json.Unmarshal(bodies[1], &retryParsed); err != nil {
		t.Fatalf("unmarshal retry body: %v", err)
	}
	retryInput, _ := json.Marshal(retryParsed["input"])
	if !bytes.Contains(retryInput, []byte("KEEP-ME")) {
		t.Errorf("retry body lost KEEP-ME due to double body rule application; input=%s", retryInput)
	}
	if bytes.Contains(retryInput, []byte("DROP-ME")) {
		t.Errorf("retry body should not resurrect DROP-ME; input=%s", retryInput)
	}
	if bytes.Contains(retryInput, []byte("bad-cipher")) {
		t.Errorf("retry body should strip encrypted reasoning; input=%s", retryInput)
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

	wire, err := (&Server{}).prepareTranslatedUpstreamBody(
		cfg, protocol.Codex, "/v1/responses", "", original, original,
		"", headers, false, nil, false,
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

	replayed, err := (&Server{}).prepareTranslatedUpstreamBody(
		cfg, protocol.Codex, "/v1/responses", "", wire, wire,
		"", headers, false, nil, true,
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
