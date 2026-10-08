package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestOAuthQuotaOutputHTTP(t *testing.T) {
	for _, auth := range []string{model.AuthTypeCodexOAuth, model.AuthTypeAnthropicOAuth, model.AuthTypeAntigravityOAuth, model.AuthTypeAPIKey} {
		for _, enabled := range []bool{false, true} {
			t.Run(auth+map[bool]string{false: "/off", true: "/on"}[enabled], func(t *testing.T) {
				body := `{"id":"resp_1","rate_limits":{"primary":{"used_percent":42}},"credits":{"balance":10},"usage":{"input_tokens":12,"output_tokens":3},"metadata":{"credits":"caller tag","rate_limits":"caller setting"},"output":[{"text":"rate_limits credits","credits":123}]}`
				headers := http.Header{"Content-Type": {"application/json"}, "X-Codex-Primary-Used-Percent": {"42"}, "Anthropic-Ratelimit-Unified-5h-Utilization": {"0.42"}, "X-Codex-Turn-State": {"sticky"}, "X-Request-Id": {"req_1"}}
				resp := &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(body))}
				rec := httptest.NewRecorder()
				req := &requestContext{ctx: context.Background(), startTime: time.Now()}
				cfg := &model.Config{AuthType: auth, OAuthQuotaPassthrough: enabled}
				result, _, err := (&Server{}).handleResponse(req, resp, rec, "codex", cfg, "", nil)
				if err != nil {
					t.Fatal(err)
				}
				hidden := (auth == model.AuthTypeCodexOAuth || auth == model.AuthTypeAnthropicOAuth) && !enabled
				var got map[string]json.RawMessage
				if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if (got["rate_limits"] == nil) != hidden || (got["credits"] == nil) != hidden {
					t.Fatalf("quota exposure: %s", rec.Body.String())
				}
				if (rec.Header().Get("X-Codex-Primary-Used-Percent") == "") != hidden || (rec.Header().Get("Anthropic-Ratelimit-Unified-5h-Utilization") == "") != hidden {
					t.Fatalf("headers: %v", rec.Header())
				}
				if rec.Header().Get("X-Codex-Turn-State") != "sticky" || result.InputTokens != 12 || result.OutputTokens != 3 {
					t.Fatalf("routing/usage changed: %+v %v", result, rec.Header())
				}
				if result.Header.Get("X-Codex-Primary-Used-Percent") != "42" {
					t.Fatal("internal header was filtered")
				}
				var metadata map[string]string
				if err := json.Unmarshal(got["metadata"], &metadata); err != nil || metadata["credits"] != "caller tag" || metadata["rate_limits"] != "caller setting" {
					t.Fatalf("caller metadata changed: %s", got["metadata"])
				}
				var output []map[string]any
				if err := json.Unmarshal(got["output"], &output); err != nil {
					t.Fatal(err)
				}
				if output[0]["credits"] != float64(123) {
					t.Fatal("model output was modified")
				}
			})
		}
	}
}

func TestOAuthQuotaOutputSSEPreservesAccounting(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[enabled], func(t *testing.T) {
			body := "event: codex.rate_limits\ndata: {\"type\":\"codex.rate_limits\",\"credits\":{\"has_credits\":true},\"rate_limits\":{\"primary\":{\"used_percent\":100}}}\n\n" +
				"event: response.metadata\ndata: {\"type\":\"response.metadata\",\"headers\":{\"x-codex-secondary-reset-at\":\"123\",\"x-codex-turn-state\":\"sticky\"}}\n\n" +
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[],\"usage\":{\"input_tokens\":12,\"output_tokens\":3}}}\n\n"
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(&oneByteQuotaReader{data: []byte(body)})}
			req := &requestContext{ctx: context.Background(), startTime: time.Now(), isStreaming: true}
			rec := httptest.NewRecorder()
			result, _, err := (&Server{}).handleResponse(req, resp, rec, "codex", &model.Config{AuthType: model.AuthTypeCodexOAuth, OAuthQuotaPassthrough: enabled}, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if !result.CodexHasCredits || result.InputTokens != 12 || result.OutputTokens != 3 {
				t.Fatalf("internal accounting lost: %+v", result)
			}
			types := map[string]map[string]any{}
			for _, frame := range strings.Split(rec.Body.String(), "\n\n") {
				_, data := parseSSEEventChunk([]byte(frame))
				if len(data) == 0 {
					continue
				}
				var event map[string]any
				if err := json.Unmarshal(data, &event); err != nil {
					t.Fatal(err)
				}
				types[event["type"].(string)] = event
			}
			if (types["codex.rate_limits"] != nil) != enabled {
				t.Fatalf("quota event exposure: %s", rec.Body.String())
			}
			metadata := types["response.metadata"]["headers"].(map[string]any)
			if (metadata["x-codex-secondary-reset-at"] != nil) != enabled || metadata["x-codex-turn-state"] != "sticky" {
				t.Fatalf("metadata: %v", metadata)
			}
			if types["response.output_text.delta"]["delta"] != "hello" || types["response.completed"] == nil {
				t.Fatal("normal stream altered")
			}
		})
	}
}

type oneByteQuotaReader struct{ data []byte }

func (r *oneByteQuotaReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

// A plan-only response must not bypass the fast marker check, and caller-owned
// fields with the same name must remain intact.
func TestOAuthQuotaOutputPlanType(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[enabled], func(t *testing.T) {
			body := `{"id":"resp_plan","plan_type":"plus","metadata":{"plan_type":"caller tag"},"output":[{"plan_type":"model output"}]}`
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
			rec := httptest.NewRecorder()
			req := &requestContext{ctx: context.Background(), startTime: time.Now()}
			_, _, err := (&Server{}).handleResponse(req, resp, rec, "codex", &model.Config{AuthType: model.AuthTypeCodexOAuth, OAuthQuotaPassthrough: enabled}, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				ID       string              `json:"id"`
				PlanType *string             `json:"plan_type"`
				Metadata map[string]string   `json:"metadata"`
				Output   []map[string]string `json:"output"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if (got.PlanType != nil) != enabled || (got.PlanType != nil && *got.PlanType != "plus") {
				t.Fatalf("unexpected plan exposure with passthrough=%t: %s", enabled, rec.Body.String())
			}
			if got.ID != "resp_plan" || got.Metadata["plan_type"] != "caller tag" || len(got.Output) != 1 || got.Output[0]["plan_type"] != "model output" {
				t.Fatalf("caller content changed: %s", rec.Body.String())
			}
		})
	}
}
