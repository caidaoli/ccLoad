package app

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"ccLoad/internal/codebuddyauth"
	"ccLoad/internal/model"

	"github.com/tidwall/gjson"
)

func TestFinalizeCodeBuddyBodyInjectsSystemStreamOptionsAndDeepSeekThinking(t *testing.T) {
	t.Parallel()
	body, err := finalizeCodeBuddyBody([]byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !gjson.GetBytes(body, "stream").Bool() {
		t.Fatal("stream not forced")
	}
	if !gjson.GetBytes(body, "stream_options.include_usage").Bool() {
		t.Fatal("stream_options.include_usage missing")
	}
	if gjson.GetBytes(body, "messages.0.role").String() != "system" {
		t.Fatal("system prompt not injected")
	}
	if gjson.GetBytes(body, "thinking.type").String() != "enabled" {
		t.Fatal("deepseek thinking not enabled")
	}
	if gjson.GetBytes(body, "reasoning_effort").String() != "high" {
		t.Fatal("deepseek reasoning_effort missing")
	}
}

func TestFinalizeCodeBuddyBodySkipsThinkingForHy3(t *testing.T) {
	t.Parallel()
	body, err := finalizeCodeBuddyBody([]byte(`{"model":"hy3","messages":[{"role":"user","content":"hi"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(body, "thinking").Exists() {
		t.Fatal("hy3 must not get deepseek thinking")
	}
}

func TestInjectCodeBuddyHeadersRewritesCodeBuddyIssuerToWorkBuddy(t *testing.T) {
	t.Parallel()
	token := jwtWithIssuer("https://www.codebuddy.ai/auth/realms/copilot")
	credential, err := (&codebuddyauth.Credential{
		AccessToken:  token,
		RefreshToken: "refresh",
		UID:          "uid",
		BaseURL:      codebuddyauth.InternationalBaseURL,
		Domain:       "www.codebuddy.ai",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "https://www.codebuddy.ai/v2/chat/completions", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if err := injectCodeBuddyHeaders(req, &model.Config{OAuthCredential: credential}, token); err != nil {
		t.Fatal(err)
	}
	if req.URL.Host != "www.workbuddy.ai" {
		t.Fatalf("host=%s, want workbuddy.ai", req.URL.Host)
	}
	if req.Header.Get("X-Domain") != "www.workbuddy.ai" || req.Header.Get("Origin") != codebuddyauth.WorkBuddyBaseURL {
		t.Fatalf("headers=%v", req.Header)
	}
}

func TestRewriteCodeBuddyPublicErrorURLsUsesRequestHost(t *testing.T) {
	t.Parallel()
	body := []byte(`visit https://www.codebuddy.ai/profile/usage`)
	req, err := http.NewRequest(http.MethodPost, "https://www.workbuddy.ai/v2/chat/completions", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := rewriteCodeBuddyPublicErrorURLs(req, body)
	if !strings.Contains(string(got), "https://www.workbuddy.ai/profile/usage") {
		t.Fatalf("got %s", got)
	}
}

func TestInjectCodeBuddyHeadersRewritesWorkBuddyIssuer(t *testing.T) {
	t.Parallel()
	token := jwtWithIssuer("https://www.workbuddy.ai/auth/realms/copilot")
	credential, err := (&codebuddyauth.Credential{
		AccessToken:  token,
		RefreshToken: "refresh",
		UID:          "uid",
		BaseURL:      codebuddyauth.InternationalBaseURL,
		Domain:       "www.codebuddy.ai",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "https://www.codebuddy.ai/v2/chat/completions", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if err := injectCodeBuddyHeaders(req, &model.Config{OAuthCredential: credential}, token); err != nil {
		t.Fatal(err)
	}
	if req.URL.Host != "www.workbuddy.ai" {
		t.Fatalf("host=%s, want workbuddy.ai", req.URL.Host)
	}
	if req.Header.Get("X-Refresh-Token") != "" {
		t.Fatal("refresh token leaked onto chat")
	}
	if req.Header.Get("X-Domain") != "www.workbuddy.ai" || req.Header.Get("Origin") != codebuddyauth.WorkBuddyBaseURL {
		t.Fatalf("headers=%v", req.Header)
	}
	if req.Header.Get("X-CodeBuddy-Request") != "1" {
		t.Fatal("missing X-CodeBuddy-Request")
	}
}

func jwtWithIssuer(iss string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"iss":%q}`, iss))
	return header + "." + payload + ".sig"
}

func TestProxyCodeBuddyResponsesCompactionRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			t.Parallel()
			requests := make(chan []byte, 2)
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read upstream request: %v", err)
					return
				}
				requests <- body
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+`{"id":"chat_compact","model":"hy3","choices":[{"index":0,"delta":{"role":"assistant","content":"Preserve /tmp/code.go; next fix the parser."}}]}`+"\n\n")
				_, _ = io.WriteString(w, "data: "+`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
				_, _ = io.WriteString(w, "data: "+`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18,"prompt_tokens_details":{"cached_tokens":3}}}`+"\n\ndata: [DONE]\n\n")
			}))
			defer upstream.Close()
			credential, err := (&codebuddyauth.Credential{AccessToken: "access"}).JSON()
			if err != nil {
				t.Fatal(err)
			}
			env := setupProxyTestEnv(t, []testChannel{{name: "codebuddy-compaction", upstreamProtocol: "openai", models: "hy3", authType: model.AuthTypeCodeBuddyOAuth, oauthCredential: credential}}, map[int]string{0: upstream.URL + "/v2/chat/completions#"})
			response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model": "hy3", "stream": stream,
				"input": []any{map[string]any{"role": "user", "content": "Fix parser"}, map[string]any{"type": "compaction_trigger"}},
			}, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			compact := gjson.ParseBytes(response.Body.Bytes())
			if stream {
				for _, frame := range strings.Split(response.Body.String(), "\n\n") {
					data := gjson.ParseBytes(sseEventData([]byte(frame)))
					if data.Get("type").String() == "response.completed" {
						compact = data.Get("response")
					}
				}
				if !strings.HasPrefix(response.Header().Get("Content-Type"), "text/event-stream") {
					t.Fatalf("unexpected stream content type %v", response.Header())
				}
			}
			item := compact.Get("output.0")
			if item.Get("type").String() != "compaction" || compact.Get("status").String() != "completed" ||
				compact.Get("usage.input_tokens").Int() != 11 || compact.Get("usage.output_tokens").Int() != 7 ||
				compact.Get("usage.input_tokens_details.cached_tokens").Int() != 3 {
				t.Fatalf("lost compaction or usage: %s", response.Body.String())
			}
			first := <-requests
			if !gjson.GetBytes(first, "stream").Bool() || strings.Contains(string(first), "compaction_trigger") {
				t.Fatalf("bad CodeBuddy summary request: %s", first)
			}
			var replayItem map[string]any
			if err := json.Unmarshal([]byte(item.Raw), &replayItem); err != nil {
				t.Fatal(err)
			}
			replayed := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model": "hy3", "input": []any{replayItem, map[string]any{"role": "user", "content": "continue"}},
			}, nil)
			if replayed.Code != http.StatusOK {
				t.Fatalf("replay status=%d body=%s", replayed.Code, replayed.Body.String())
			}
			next := <-requests
			if !strings.Contains(string(next), "Preserve /tmp/code.go; next fix the parser.") || strings.Contains(string(next), "ccload-compact-v1:") {
				t.Fatalf("capsule was not expanded on replay: %s", next)
			}
		})
	}
}

func TestProxyCodeBuddyResponsesCompactionRequiresRealStop(t *testing.T) {
	for _, ending := range []string{"done-without-stop", "truncated", "length"} {
		t.Run(ending, func(t *testing.T) {
			t.Parallel()
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+`{"id":"partial","model":"hy3","choices":[{"index":0,"delta":{"content":"partial summary"}}]}`+"\n\n")
				if ending == "length" {
					_, _ = io.WriteString(w, "data: "+`{"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`+"\n\n")
				}
				if ending != "truncated" {
					_, _ = io.WriteString(w, "data: [DONE]\n\n")
				}
			}))
			defer upstream.Close()
			credential, err := (&codebuddyauth.Credential{AccessToken: "access"}).JSON()
			if err != nil {
				t.Fatal(err)
			}
			env := setupProxyTestEnv(t, []testChannel{{name: "codebuddy-compaction-stop", upstreamProtocol: "openai", models: "hy3", authType: model.AuthTypeCodeBuddyOAuth, oauthCredential: credential}}, map[int]string{0: upstream.URL + "/v2/chat/completions#"})
			response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model": "hy3", "stream": true,
				"input": []any{map[string]any{"role": "user", "content": "Fix parser"}, map[string]any{"type": "compaction_trigger"}},
			}, nil)
			if response.Code == http.StatusOK || strings.Contains(response.Body.String(), "ccload-compact-v1:") {
				t.Fatalf("unfinished summary reported success: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
