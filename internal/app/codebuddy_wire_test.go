package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

/**
 * @brief 验证 DeepSeek 请求显式关闭推理时，出站请求保留客户端档位。
 * @param t 测试上下文。
 */
func TestFinalizeCodeBuddyBodyPreservesExplicitThinkingOff(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"reasoning_effort", "reasoning.effort"} {
		for _, effort := range []string{"none", "off"} {
			t.Run(path+"/"+effort, func(t *testing.T) {
				input := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`)
				body, err := finalizeCodeBuddyBody(setJSONValue(input, path, effort), nil)
				if err != nil {
					t.Fatal(err)
				}
				if gjson.GetBytes(body, path).String() != effort || gjson.GetBytes(body, "thinking").Exists() {
					t.Fatalf("unexpected DeepSeek request: %s", body)
				}
			})
		}
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

func TestInjectCodeBuddyHeadersKeepsCodeBuddyInternationalHost(t *testing.T) {
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
	if req.URL.Host != "www.codebuddy.ai" {
		t.Fatalf("host=%s, want codebuddy.ai", req.URL.Host)
	}
	if req.Header.Get("X-Domain") != "www.codebuddy.ai" || req.Header.Get("Origin") != codebuddyauth.InternationalBaseURL {
		t.Fatalf("headers=%v", req.Header)
	}
}

func TestInjectCodeBuddyHeadersPreservesConfiguredURL(t *testing.T) {
	t.Parallel()
	token := jwtWithIssuer("https://www.workbuddy.ai/auth/realms/copilot")
	credential, err := (&codebuddyauth.Credential{
		AccessToken:  token,
		RefreshToken: "refresh",
		UID:          "uid",
		BaseURL:      codebuddyauth.WorkBuddyBaseURL,
		Domain:       "www.codebuddy.ai",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{
		"https://www.workbuddy.ai/v2/chat/completions",
		"https://www.codebuddy.ai/v2/chat/completions",
		"http://127.0.0.1:1234/custom/chat?mode=test",
	} {
		t.Run(endpoint, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			host := req.Host
			origin := req.URL.Scheme + "://" + req.URL.Host
			domain := req.URL.Hostname()
			if err := injectCodeBuddyHeaders(req, &model.Config{OAuthCredential: credential}, token); err != nil {
				t.Fatal(err)
			}
			if req.URL.String() != endpoint || req.Host != host {
				t.Fatalf("request rewritten: URL=%s Host=%s", req.URL, req.Host)
			}
			if req.Header.Get("X-Refresh-Token") != "" {
				t.Fatal("refresh token leaked onto chat")
			}
			if req.Header.Get("X-Domain") != domain || req.Header.Get("Origin") != origin || req.Header.Get("Referer") != origin+"/" {
				t.Fatalf("headers disagree with endpoint: %v", req.Header)
			}
			if req.Header.Get("X-CodeBuddy-Request") != "1" {
				t.Fatal("missing X-CodeBuddy-Request")
			}
		})
	}
}

func jwtWithIssuer(iss string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"iss":%q}`, iss))
	return header + "." + payload + ".sig"
}

/**
 * @brief 验证 CodeBuddy 代理出站的角色、输出上限、历史推理兼容及提示词改写范围。
 * @param t 当前测试上下文。
 */
func TestProxyCodeBuddyRequestCompatibility(t *testing.T) {
	for _, arrayContent := range []bool{false, true} {
		t.Run(fmt.Sprintf("array_content=%v", arrayContent), func(t *testing.T) {
			t.Parallel()
			modelName := "hy3"
			if arrayContent {
				modelName = "gpt-5.5"
			}
			flagged := "You are an AI agent powered by DeepSeek Harness\nWeb GUI whose URL the runtime context names. When the user refers\nTokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root\nordinary workspace root /workspace/project"
			compatible := "You are an AI agent, powered by DeepSeek Harness\nWeb GUI whose URL the runtime context names. The interface, when the user refers\nT\u034Fokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root\nordinary workspace root /workspace/project"
			original := flagged + "\nPRIVATE_TOKEN\nYou are Claude Code, Anthropic's official CLI for Claude.\nMain branch (you will usually use this for PRs)"
			wantAssistant := compatible + "\nPRIVATE_TOKEN\nYou are CodeBuddy Code.You are an interactive CLI tool that helps users with software engineering tasks.\nDefault branch (you will usually use this for PRs)"
			wantSystem := compatible + "\nP\u034FRIVATE_TOKEN\nYou are CodeBuddy Code.You are an interactive CLI tool that helps users with software engineering tasks.\nDefault branch (you will usually use this for PRs)"
			content := func(text string) any {
				if !arrayContent {
					return text
				}
				return []any{
					map[string]any{"type": "text", "text": text, "provider_part": "preserved"},
					map[string]any{"text": text},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.invalid/image.png"}, "text": original},
				}
			}
			arguments, err := json.Marshal(map[string]string{"text": original})
			if err != nil {
				t.Fatal(err)
			}
			messages := []map[string]any{
				{"role": "developer", "content": content(original), "name": "first", "provider_message": map[string]any{"opaque": true}},
				{"role": "developer", "content": content(original), "reasoning_content": "preserved", "reasoning_details": []any{map[string]any{"text": "preserved"}}},
				{"role": "system", "content": content(original)},
				{"role": "assistant", "content": content(original), "tool_calls": []any{map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": string(arguments)}}}},
				{"role": "user", "content": content(flagged + "\nPRIVATE_TOKEN"), "reasoning_content": "preserved", "reasoning_details": []any{map[string]any{"text": "preserved"}}},
				{"role": "tool", "tool_call_id": "call-1", "content": content(original)},
			}
			reasoningCases := []struct {
				name   string
				fields map[string]any
				want   string
			}{
				{"content priority", map[string]any{"reasoning_content": " content ", "reasoning_details": []any{map[string]any{"text": "ignored"}}}, " content "},
				{"existing priority", map[string]any{"reasoning": " existing ", "reasoning_content": "ignored", "reasoning_details": []any{map[string]any{"text": "ignored"}}}, " existing "},
				{"empty existing", map[string]any{"reasoning": "", "reasoning_content": "content"}, "content"},
				{"details fallback", map[string]any{"reasoning": 42, "reasoning_content": "", "reasoning_details": []any{map[string]any{"text": "first", "summary": "ignored"}, map[string]any{"text": "", "summary": "second"}, map[string]any{"text": 3, "summary": "third"}, map[string]any{"text": "", "summary": ""}, map[string]any{"text": "fourth"}}}, "firstsecondthirdfourth"},
				{"nonstring content", map[string]any{"reasoning_content": true, "reasoning_details": []any{map[string]any{"summary": "summary"}}}, "summary"},
				{"whitespace existing", map[string]any{"reasoning": " ", "reasoning_content": "ignored"}, " "},
				{"empty sources", map[string]any{"reasoning_content": "", "reasoning_details": []any{}}, ""},
			}
			for _, tc := range reasoningCases {
				message := map[string]any{"role": "assistant", "content": tc.name}
				maps.Copy(message, tc.fields)
				messages = append(messages, message)
			}
			encoded, err := json.Marshal(messages)
			if err != nil {
				t.Fatal(err)
			}
			var wantMessages []map[string]any
			if err := json.Unmarshal(encoded, &wantMessages); err != nil {
				t.Fatal(err)
			}
			wantMessages[0]["role"] = "system"
			wantMessages[0]["content"] = content(wantSystem)
			wantMessages[1]["role"] = "system"
			wantMessages[1]["content"] = content(wantSystem)
			wantMessages[2]["content"] = content(wantSystem)
			wantMessages[3]["content"] = content(wantAssistant)
			for index, tc := range reasoningCases {
				message := wantMessages[6+index]
				delete(message, "reasoning_content")
				delete(message, "reasoning_details")
				if tc.want != "" {
					message["reasoning"] = tc.want
				}
			}
			wantMessages = append([]map[string]any{{
				"role": "system", "content": codeBuddyDefaultSystemPrompt,
			}}, wantMessages...)
			requests := make(chan []byte, 1)
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read upstream request: %v", err)
					return
				}
				requests <- body
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: {\"id\":\"compatibility\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", modelName)
			}))
			defer upstream.Close()
			credential, err := (&codebuddyauth.Credential{AccessToken: "access"}).JSON()
			if err != nil {
				t.Fatal(err)
			}
			env := setupProxyTestEnvWithSettings(t, []testChannel{{name: "codebuddy-compatibility", upstreamProtocol: "openai", models: modelName, authType: model.AuthTypeCodeBuddyOAuth, oauthCredential: credential}}, map[int]string{0: upstream.URL + "/v2/chat/completions#"}, map[string]string{"antigravity_sensitive_words": `["PRIVATE_TOKEN"]`})
			request := map[string]any{
				"model": modelName, "messages": messages, "max_completion_tokens": 128,
				"provider_extension": map[string]any{"text": original, "opaque": true},
				"tools":              []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "description": original, "parameters": map[string]any{"type": "object"}}}},
			}
			wantLimit := int64(128)
			if arrayContent {
				request["max_tokens"] = 64
				wantLimit = 64
			}
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", request, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			body := <-requests
			var actual struct {
				Messages []map[string]any `json:"messages"`
			}
			if err := json.Unmarshal(body, &actual); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual.Messages, wantMessages) {
				t.Errorf("wire messages=%+v, want %+v", actual.Messages, wantMessages)
			}
			wire := gjson.ParseBytes(body)
			if wire.Get("max_tokens").Int() != wantLimit || wire.Get("max_completion_tokens").Exists() {
				t.Errorf("token limits not normalized: %s", body)
			}
			if wire.Get("provider_extension.text").String() != original || !wire.Get("provider_extension.opaque").Bool() || wire.Get("tools.0.function.description").String() != original {
				t.Errorf("provider extension or tool schema changed: %s", body)
			}
		})
	}
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
				for frame := range strings.SplitSeq(response.Body.String(), "\n\n") {
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

/**
 * @brief 验证 CodeBuddy 11128 的结构化错误触发一次修复，并在成功后复用同一系统指令的策略。
 * @param t 当前测试上下文。
 */
func TestProxyCodeBuddyCommaRetryRemembersSuccess(t *testing.T) {
	for _, tc := range []struct {
		name      string ///< 错误载体及错误码类型。
		errorBody string ///< 上游原始错误 JSON。
		sse       bool   ///< 是否通过未提交的 SSE 事件拒绝首发。
		stream    bool   ///< 客户端是否请求流式响应。
	}{
		{name: "HTTP top-level number", errorBody: `{"code":11128,"message":"refused"}`},
		{name: "HTTP top-level string", errorBody: `{"code":"11128","message":"refused"}`, stream: true},
		{name: "HTTP nested number", errorBody: `{"error":{"code":11128,"message":"refused"}}`},
		{name: "HTTP nested string", errorBody: `{"error":{"code":"11128","message":"refused"}}`, stream: true},
		{name: "SSE top-level number", errorBody: `{"code":11128,"message":"refused"}`, sse: true, stream: true},
		{name: "SSE nested string", errorBody: `{"error":{"code":"11128","message":"refused"}}`, sse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, requests := setupCodeBuddyCommaRetryTest(t, nil, func(w http.ResponseWriter, attempt int) {
				if attempt != 1 {
					writeCodeBuddyCommaRetrySuccess(w)
					return
				}
				if tc.sse {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: %s\n\n", tc.errorBody)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, tc.errorBody)
			})
			original := "You are an AI assistant powered by Acme."
			paused := "You are an AI assistant, powered by Acme."
			messages := []map[string]any{{"role": "system", "content": original}, {"role": "user", "content": "hello"}}
			request := map[string]any{"model": "hy3", "messages": messages, "stream": tc.stream}
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", request, nil)
			if response.Code != http.StatusOK || len(requests) != 2 {
				t.Fatalf("status=%d attempts=%d body=%s", response.Code, len(requests), response.Body.String())
			}
			if first := <-requests; gjson.GetBytes(first, "messages.0.content").String() != original {
				t.Fatalf("first attempt changed the identity before rejection: %s", first)
			}
			if retry := <-requests; gjson.GetBytes(retry, "messages.0.content").String() != paused {
				t.Fatalf("retry did not pause the rejected identity: %s", retry)
			}
			messages[1]["content"] = "continue with a different user message"
			response = doProxyRequest(t, env.engine, "/v1/chat/completions", request, nil)
			if response.Code != http.StatusOK || len(requests) != 1 {
				t.Fatalf("cached status=%d attempts=%d body=%s", response.Code, len(requests), response.Body.String())
			}
			if cached := <-requests; gjson.GetBytes(cached, "messages.0.content").String() != paused || gjson.GetBytes(cached, "messages.1.content").String() != messages[1]["content"] {
				t.Fatalf("successful strategy was not reused for the next turn: %s", cached)
			}
		})
	}
}

/**
 * @brief 验证逗号修复仅修改系统与助手文本，重试不重复应用已完成的数组请求体规则。
 * @param t 当前测试上下文。
 */
func TestProxyCodeBuddyCommaRetryPreservesPayloadAndBodyRules(t *testing.T) {
	t.Parallel()
	rules := &model.CustomRequestRules{Body: []model.CustomBodyRule{
		{Action: model.RuleActionRemove, Path: "messages.1"},
		{Action: model.RuleActionRemove, Path: "tools.0"},
	}}
	env, requests := setupCodeBuddyCommaRetryTest(t, rules, func(w http.ResponseWriter, attempt int) {
		if attempt == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":11128,"message":"refused"}}`)
			return
		}
		writeCodeBuddyCommaRetrySuccess(w)
	})
	original := "You are a coding assistant running on Acme."
	paused := "You are a coding assistant, running on Acme."
	arguments, err := json.Marshal(map[string]string{"identity": original})
	if err != nil {
		t.Fatal(err)
	}
	request := map[string]any{
		"model": "hy3",
		"messages": []map[string]any{
			{"role": "system", "content": []map[string]any{
				{"type": "text", "text": original, "provider_part": true},
				{"text": original},
				{"type": "text", "text": paused},
				{"type": "image_url", "image_url": map[string]any{"url": "https://example.invalid/identity.png"}, "text": original},
			}},
			{"role": "user", "content": "removed once"},
			{"role": "assistant", "content": original, "tool_calls": []any{map[string]any{"id": "call-identity", "type": "function", "function": map[string]any{"name": "identity", "arguments": string(arguments)}}}},
			{"role": "tool", "tool_call_id": "call-identity", "content": original},
			{"role": "user", "content": original},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "removed_once", "parameters": map[string]any{"type": "object"}}},
			map[string]any{"type": "function", "function": map[string]any{"name": "identity", "description": original, "parameters": map[string]any{"type": "object", "properties": map[string]any{"identity": map[string]any{"type": "string", "description": original}}}}},
		},
		"provider_extension": map[string]any{"identity": original},
	}
	response := doProxyRequest(t, env.engine, "/v1/chat/completions", request, nil)
	if response.Code != http.StatusOK || len(requests) != 2 {
		t.Fatalf("status=%d attempts=%d body=%s", response.Code, len(requests), response.Body.String())
	}
	first, retry := <-requests, <-requests
	if gjson.GetBytes(first, "messages.#").Int() != 4 || gjson.GetBytes(first, "tools.#").Int() != 1 || gjson.GetBytes(first, "tools.0.function.name").String() != "identity" {
		t.Fatalf("initial body rules did not remove exactly the selected entries: %s", first)
	}
	var before, after map[string]any
	if err := json.Unmarshal(first, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(retry, &after); err != nil {
		t.Fatal(err)
	}
	messages := before["messages"].([]any)
	parts := messages[0].(map[string]any)["content"].([]any)
	parts[0].(map[string]any)["text"] = paused
	parts[1].(map[string]any)["text"] = paused
	messages[1].(map[string]any)["content"] = paused
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("retry changed data beyond eligible identity text: first=%s retry=%s", first, retry)
	}
}

/**
 * @brief 验证其他错误、无可修改文本和已提交的 SSE 错误不会重放请求。
 * @param t 当前测试上下文。
 */
func TestProxyCodeBuddyCommaRetryRejectsUnrelatedFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string ///< 不应触发修复的请求或错误。
		status    int    ///< 上游 HTTP 状态。
		errorBody string ///< 上游错误 JSON。
		identity  string ///< 首条系统消息文本。
		sse       bool   ///< 是否返回 SSE 错误。
		committed bool   ///< 是否在错误前提交实际输出。
	}{
		{name: "other code", status: 400, errorBody: `{"error":{"code":11127,"message":"refused"}}`},
		{name: "code only in message", status: 400, errorBody: `{"error":{"message":"refused 11128"}}`},
		{name: "non-400", status: 422, errorBody: `{"code":11128,"message":"refused"}`},
		{name: "already paused", status: 400, errorBody: `{"code":11128,"message":"refused"}`, identity: "You are an AI assistant, powered by Acme."},
		{name: "no matching identity", status: 400, errorBody: `{"code":11128,"message":"refused"}`, identity: "Answer concisely."},
		{name: "committed SSE", status: 200, errorBody: `{"error":{"code":11128,"message":"refused"}}`, sse: true, committed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, requests := setupCodeBuddyCommaRetryTest(t, nil, func(w http.ResponseWriter, _ int) {
				if tc.sse {
					w.Header().Set("Content-Type", "text/event-stream")
					if tc.committed {
						_, _ = io.WriteString(w, "data: "+`{"id":"partial","model":"hy3","choices":[{"index":0,"delta":{"content":"already sent"}}]}`+"\n\n")
					}
					_, _ = fmt.Fprintf(w, "data: %s\n\n", tc.errorBody)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.errorBody)
			})
			identity := tc.identity
			if identity == "" {
				identity = "You are an AI assistant powered by Acme."
			}
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "hy3", "stream": tc.sse,
				"messages": []map[string]any{{"role": "system", "content": identity}, {"role": "user", "content": "hello"}},
			}, nil)
			if len(requests) != 1 {
				t.Fatalf("unrelated failure replayed %d attempts: status=%d body=%s", len(requests), response.Code, response.Body.String())
			}
			if sent := <-requests; gjson.GetBytes(sent, "messages.0.content").String() != identity {
				t.Fatalf("identity changed before a matching rejection: %s", sent)
			}
			if tc.committed && !strings.Contains(response.Body.String(), "already sent") {
				t.Fatalf("committed content lost: %s", response.Body.String())
			}
		})
	}
}

/**
 * @brief 验证修复再次被拒或流缺失终态时最多重试一次，并且失败不影响下一次请求的首发。
 * @param t 当前测试上下文。
 */
func TestProxyCodeBuddyCommaRetryDoesNotRememberFailure(t *testing.T) {
	for _, ending := range []string{"HTTP rejection", "SSE rejection", "incomplete SSE"} {
		t.Run(ending, func(t *testing.T) {
			t.Parallel()
			env, requests := setupCodeBuddyCommaRetryTest(t, nil, func(w http.ResponseWriter, attempt int) {
				if attempt > 2 {
					writeCodeBuddyCommaRetrySuccess(w)
					return
				}
				if ending == "incomplete SSE" && attempt == 2 {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+`{"id":"partial","model":"hy3","choices":[{"index":0,"delta":{"content":"unfinished"}}]}`+"\n\n")
					return
				}
				if ending == "SSE rejection" && attempt == 2 {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+`{"error":{"code":11128,"message":"still refused"}}`+"\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"code":11128,"message":"refused"}`)
			})
			original := "You are an AI assistant powered by Acme."
			request := map[string]any{"model": "hy3", "messages": []map[string]any{{"role": "system", "content": original}, {"role": "user", "content": "hello"}}}
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", request, nil)
			if (ending != "incomplete SSE" && response.Code != http.StatusBadRequest) || len(requests) != 2 {
				t.Fatalf("failed repair status=%d attempts=%d body=%s", response.Code, len(requests), response.Body.String())
			}
			<-requests
			<-requests
			response = doProxyRequest(t, env.engine, "/v1/chat/completions", request, nil)
			if response.Code != http.StatusOK || len(requests) != 1 {
				t.Fatalf("next request status=%d attempts=%d body=%s", response.Code, len(requests), response.Body.String())
			}
			if sent := <-requests; gjson.GetBytes(sent, "messages.0.content").String() != original {
				t.Fatalf("failed repair polluted the strategy cache: %s", sent)
			}
		})
	}
}

/**
 * @brief 验证成功策略按渠道与上游 origin 隔离，普通 API Key 渠道不执行 CodeBuddy 修复。
 * @param t 当前测试上下文。
 */
func TestProxyCodeBuddyCommaRetryChannelAndOriginScope(t *testing.T) {
	t.Parallel()
	credential, err := (&codebuddyauth.Credential{AccessToken: "access"}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	env := setupProxyTestEnvWithSettings(t, []testChannel{
		{name: "comma-first", upstreamProtocol: "openai", models: "hy3", authType: model.AuthTypeCodeBuddyOAuth, oauthCredential: credential},
		{name: "comma-second", upstreamProtocol: "openai", models: "hy3", authType: model.AuthTypeCodeBuddyOAuth, oauthCredential: credential},
		{name: "ordinary-api-key", upstreamProtocol: "openai", models: "hy3", apiKey: "sk-ordinary"},
	}, map[int]string{
		0: "https://comma-one.example/v2/chat/completions#",
		1: "https://comma-one.example/v2/chat/completions#",
		2: "https://comma-one.example/v2/chat/completions#",
	}, map[string]string{"antigravity_sensitive_words": `[]`})
	var requests [][]byte
	env.server.client = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		requests = append(requests, body)
		if len(requests) == 1 || r.Header.Get("Authorization") == "Bearer sk-ordinary" {
			return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"code":11128,"message":"refused"}`))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + `{"id":"comma-success","model":"hy3","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))}, nil
	})}
	original := "You are an AI agent running in Acme."
	request := map[string]any{"model": "hy3", "messages": []map[string]any{{"role": "system", "content": original}, {"role": "user", "content": "hello"}}}
	for index, step := range []struct {
		channel string ///< 显式选定的渠道名。
		want    string ///< 该渠道首发身份句。
	}{
		{channel: "comma-first", want: original},
		{channel: "comma-second", want: original},
		{channel: "comma-first", want: "You are an AI agent, running in Acme."},
		{channel: "ordinary-api-key", want: original},
	} {
		before := len(requests)
		response := doProxyRequest(t, env.engine, "/v1/chat/completions", request, map[string]string{"X-CCLoad-Channel": step.channel})
		wantAttempts, wantStatus := 1, http.StatusOK
		if index == 0 {
			wantAttempts = 2
		}
		if step.channel == "ordinary-api-key" {
			wantStatus = http.StatusBadRequest
		}
		if response.Code != wantStatus || len(requests)-before != wantAttempts {
			t.Fatalf("channel=%s status=%d attempts=%d body=%s", step.channel, response.Code, len(requests)-before, response.Body.String())
		}
		if gjson.GetBytes(requests[before], "messages.0.content").String() != step.want {
			t.Fatalf("strategy escaped its channel scope: %s", requests[before])
		}
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range configs {
		if cfg.Name == "comma-first" {
			cfg.URLs[0].URL = "https://comma-two.example/v2/chat/completions"
			if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
				t.Fatal(err)
			}
		}
	}
	env.server.InvalidateChannelListCache()
	before := len(requests)
	response := doProxyRequest(t, env.engine, "/v1/chat/completions", request, map[string]string{"X-CCLoad-Channel": "comma-first"})
	if response.Code != http.StatusOK || len(requests)-before != 1 {
		t.Fatalf("new origin status=%d attempts=%d body=%s", response.Code, len(requests)-before, response.Body.String())
	}
	if gjson.GetBytes(requests[before], "messages.0.content").String() != original {
		t.Fatalf("strategy escaped its upstream origin scope: %s", requests[before])
	}
}

/**
 * @brief 验证仅修改助手历史的成功修复不会使相同系统指令下的后续请求主动改写。
 * @param t 当前测试上下文。
 */
func TestProxyCodeBuddyCommaRetryDoesNotRememberAssistantOnly(t *testing.T) {
	t.Parallel()
	env, requests := setupCodeBuddyCommaRetryTest(t, nil, func(w http.ResponseWriter, attempt int) {
		if attempt == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"code":11128,"message":"refused"}`)
			return
		}
		writeCodeBuddyCommaRetrySuccess(w)
	})
	original := "You are the assistant based on Acme."
	request := map[string]any{"model": "hy3", "messages": []map[string]any{
		{"role": "system", "content": "Answer concisely."},
		{"role": "assistant", "content": original},
		{"role": "user", "content": "continue"},
	}}
	response := doProxyRequest(t, env.engine, "/v1/chat/completions", request, nil)
	if response.Code != http.StatusOK || len(requests) != 2 {
		t.Fatalf("status=%d attempts=%d body=%s", response.Code, len(requests), response.Body.String())
	}
	<-requests
	if retry := <-requests; gjson.GetBytes(retry, "messages.1.content").String() != "You are the assistant, based on Acme." {
		t.Fatalf("assistant identity was not repaired: %s", retry)
	}
	response = doProxyRequest(t, env.engine, "/v1/chat/completions", request, nil)
	if response.Code != http.StatusOK || len(requests) != 1 {
		t.Fatalf("next request status=%d attempts=%d body=%s", response.Code, len(requests), response.Body.String())
	}
	if sent := <-requests; gjson.GetBytes(sent, "messages.1.content").String() != original {
		t.Fatalf("assistant-only repair polluted the system strategy cache: %s", sent)
	}
}

/**
 * @brief 验证 Gemini 流转换在上游只有部分输出且无终态时不会记忆逗号修复策略。
 * @param t 当前测试上下文。
 */
func TestProxyCodeBuddyCommaRetryDoesNotRememberIncompleteGeminiStream(t *testing.T) {
	t.Parallel()
	env, requests := setupCodeBuddyCommaRetryTest(t, nil, func(w http.ResponseWriter, attempt int) {
		switch attempt {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"code":11128,"message":"refused"}`)
		case 2:
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: "+`{"id":"partial","model":"hy3","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"unfinished"}}]}`+"\n\n")
		default:
			writeCodeBuddyCommaRetrySuccess(w)
		}
	})
	original := "You are an AI assistant powered by Acme."
	request := map[string]any{
		"systemInstruction": map[string]any{"parts": []map[string]any{{"text": original}}},
		"contents":          []map[string]any{{"role": "user", "parts": []map[string]any{{"text": "hello"}}}},
	}
	response := doProxyRequest(t, env.engine, "/v1beta/models/hy3:streamGenerateContent", request, nil)
	if len(requests) != 2 {
		t.Fatalf("partial stream made %d attempts: status=%d body=%s", len(requests), response.Code, response.Body.String())
	}
	<-requests
	if retry := <-requests; !strings.Contains(gjson.GetBytes(retry, "messages.0.content").String(), "assistant, powered by") {
		t.Fatalf("Gemini retry did not pause the rejected identity: %s", retry)
	}
	response = doProxyRequest(t, env.engine, "/v1beta/models/hy3:streamGenerateContent", request, nil)
	if response.Code != http.StatusOK || len(requests) != 1 {
		t.Fatalf("next Gemini request status=%d attempts=%d body=%s", response.Code, len(requests), response.Body.String())
	}
	if sent := <-requests; !strings.Contains(gjson.GetBytes(sent, "messages.0.content").String(), original) {
		t.Fatalf("unfinished Gemini stream polluted the strategy cache: %s", sent)
	}
}

/**
 * @brief 验证缓存按完整系统消息和实际上游模型隔离。
 * @param t 当前测试上下文。
 */
func TestProxyCodeBuddyCommaRetryCacheScope(t *testing.T) {
	t.Parallel()
	env, requests := setupCodeBuddyCommaRetryTest(t, nil, func(w http.ResponseWriter, attempt int) {
		if attempt == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"code":11128,"message":"refused"}`)
			return
		}
		writeCodeBuddyCommaRetrySuccess(w)
	})
	original := "You are an AI assistant built on Acme."
	for index, step := range []struct {
		model    string           ///< 当前请求的上游模型。
		messages []map[string]any ///< 当前请求的完整消息。
		want     string           ///< 首条系统消息的预期文本。
	}{
		{model: "hy3", messages: []map[string]any{{"role": "system", "content": original}, {"role": "user", "content": "hello"}}, want: original},
		{model: "hy3", messages: []map[string]any{{"role": "system", "content": original}, {"role": "user", "content": "new user input"}}, want: "You are an AI assistant, built on Acme."},
		{model: "hy3", messages: []map[string]any{{"role": "system", "content": original}, {"role": "system", "content": "Additional system instruction."}, {"role": "user", "content": "hello"}}, want: original},
		{model: "hy3", messages: []map[string]any{{"role": "system", "content": original, "name": "changed-system-metadata"}, {"role": "user", "content": "hello"}}, want: original},
		{model: "deepseek-v4.1-flash", messages: []map[string]any{{"role": "system", "content": original}, {"role": "user", "content": "hello"}}, want: original},
	} {
		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": step.model, "messages": step.messages}, nil)
		wantAttempts := 1
		if index == 0 {
			wantAttempts = 2
		}
		if response.Code != http.StatusOK || len(requests) != wantAttempts {
			t.Fatalf("step=%d status=%d attempts=%d body=%s", index, response.Code, len(requests), response.Body.String())
		}
		if sent := <-requests; gjson.GetBytes(sent, "messages.0.content").String() != step.want {
			t.Fatalf("step=%d cache escaped its system/model scope: %s", index, sent)
		}
		if index == 0 {
			<-requests
		}
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "hy3", "messages": []map[string]any{{"role": "system", "content": original}, {"role": "user", "content": "concurrent next turn"}},
			}, nil)
			if response.Code != http.StatusOK {
				t.Errorf("concurrent cached request status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	workers.Wait()
	if len(requests) != 8 {
		t.Fatalf("concurrent requests made %d attempts, want 8", len(requests))
	}
	for range 8 {
		if sent := <-requests; gjson.GetBytes(sent, "messages.0.content").String() != "You are an AI assistant, built on Acme." {
			t.Fatalf("concurrent request lost the remembered strategy: %s", sent)
		}
	}
}

/**
 * @brief 验证上游拒绝对象类型后只强制已声明的指定工具，并原样转发上游结果。
 * @param t 当前测试上下文。
 */
func TestProxyCodeBuddyNamedToolChoice(t *testing.T) {
	t.Parallel()
	type chatRequest struct {
		ToolChoice string `json:"tool_choice"`
	}
	var request chatRequest
	err := json.Unmarshal([]byte(`{"tool_choice":{"type":"function","function":{"name":"lookup"}}}`), &request)
	if err == nil {
		t.Fatal("JSON decoder unexpectedly accepted an object tool_choice as a string")
	}
	typeError := fmt.Sprintf(`{"code":11101,"msg":%q}`, "Unmarshal chat params failed: "+err.Error())
	const retryError = `{"code":11101,"msg":"Invalid tool choice"}`
	for _, tc := range []struct {
		name         string
		toolName     string
		choiceName   string
		upstreamBody string
		acceptRetry  bool
		noToolCall   bool
		wantAttempts int
	}{
		{name: "object accepted", toolName: "lookup", choiceName: "lookup", wantAttempts: 1},
		{name: "string required", toolName: "lookup", choiceName: "lookup", upstreamBody: typeError, acceptRetry: true, wantAttempts: 2},
		{name: "model returned text", toolName: "lookup", choiceName: "lookup", upstreamBody: typeError, acceptRetry: true, noToolCall: true, wantAttempts: 2},
		{name: "retry rejected", toolName: "lookup", choiceName: "lookup", upstreamBody: typeError, wantAttempts: 2},
		{name: "other field", toolName: "lookup", choiceName: "lookup", upstreamBody: strings.Replace(typeError, ".tool_choice", ".model", 1), wantAttempts: 1},
		{name: "nested field", toolName: "lookup", choiceName: "lookup", upstreamBody: strings.Replace(typeError, ".tool_choice", ".metadata.tool_choice", 1), wantAttempts: 1},
		{name: "generic error", toolName: "lookup", choiceName: "lookup", upstreamBody: `{"code":11101,"msg":"Unmarshal chat params failed"}`, wantAttempts: 1},
		{name: "unknown function", toolName: "lookup", choiceName: "missing", upstreamBody: typeError, wantAttempts: 1},
		{name: "missing function name", toolName: "lookup", upstreamBody: typeError, wantAttempts: 1},
		{name: "invalid function name", toolName: "look up", choiceName: "look up", upstreamBody: typeError, wantAttempts: 1},
		{name: "duplicate function name", toolName: "decoy", choiceName: "decoy", upstreamBody: typeError, wantAttempts: 1},
		{name: "function named auto", toolName: "auto", choiceName: "auto", upstreamBody: typeError, acceptRetry: true, wantAttempts: 2},
		{name: "function named none", toolName: "none", choiceName: "none", upstreamBody: typeError, acceptRetry: true, wantAttempts: 2},
		{name: "function named required", toolName: "required", choiceName: "required", upstreamBody: typeError, acceptRetry: true, wantAttempts: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, requests := setupCodeBuddyCommaRetryTest(t, nil, func(w http.ResponseWriter, attempt int) {
				rejected := tc.upstreamBody
				if tc.acceptRetry && attempt == 2 {
					rejected = ""
				} else if attempt == 2 {
					rejected = retryError
				}
				if rejected != "" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, rejected)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				finishReason := "tool_calls"
				delta := fmt.Sprintf(`{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":%q,"arguments":"{\"query\":\"real tool arguments\"}"}}]}`, tc.choiceName)
				if tc.noToolCall {
					finishReason = "stop"
					delta = `{"role":"assistant","content":"upstream returned text"}`
				}
				_, _ = fmt.Fprintf(w, `data: {"id":"chat","model":"hy3","choices":[{"index":0,"delta":%s,"finish_reason":%q}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`+"\n\ndata: [DONE]\n\n", delta, finishReason)
			})
			payload := json.RawMessage(fmt.Sprintf(`{"model":"hy3","messages":[{"role":"user","content":"call the specified tool"}],"tools":[{"type":"function","function":{"name":"decoy","parameters":{"type":"object"}}},{"type":"function","function":{"name":%q,"description":"Lookup tool","strict":true,"parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}}}],"tool_choice":{"type":"function","function":{"name":%q}},"parallel_tool_calls":false}`, tc.toolName, tc.choiceName))
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", payload, nil)
			if len(requests) != tc.wantAttempts {
				t.Fatalf("attempts=%d want=%d, HTTP %d: %s", len(requests), tc.wantAttempts, response.Code, response.Body)
			}
			if tc.upstreamBody == "" || tc.acceptRetry {
				if response.Code != http.StatusOK {
					t.Fatalf("HTTP %d want HTTP 200: %s", response.Code, response.Body)
				}
				message := gjson.GetBytes(response.Body.Bytes(), "choices.0.message")
				if tc.noToolCall {
					if message.Get("content").Str != "upstream returned text" || len(message.Get("tool_calls").Array()) != 0 {
						t.Fatalf("unexpected tool call synthesized from upstream text: %s", response.Body)
					}
				} else {
					call := message.Get("tool_calls.0")
					arguments := gjson.Parse(call.Get("function.arguments").Str)
					if call.Get("function.name").Str != tc.choiceName || call.Get("id").Str != "call_1" || arguments.Get("query").Str != "real tool arguments" {
						t.Fatalf("upstream tool %q or its arguments changed: %s", tc.choiceName, response.Body)
					}
				}
			} else {
				wantError := tc.upstreamBody
				if tc.wantAttempts == 2 {
					wantError = retryError
				}
				got := gjson.ParseBytes(response.Body.Bytes())
				if response.Code != http.StatusBadRequest || got.Get("code").Int() != 11101 || got.Get("msg").Str != gjson.Get(wantError, "msg").Str {
					t.Fatalf("HTTP %d body=%s, want HTTP 400 body=%s", response.Code, response.Body, wantError)
				}
			}
			var first map[string]any
			if err := json.Unmarshal(<-requests, &first); err != nil {
				t.Fatal(err)
			}
			wantObject := map[string]any{"type": "function", "function": map[string]any{"name": tc.choiceName}}
			if !reflect.DeepEqual(first["tool_choice"], wantObject) {
				t.Fatalf("first tool_choice=%#v want=%#v", first["tool_choice"], wantObject)
			}
			var submitted struct {
				Tools []any `json:"tools"`
			}
			if err := json.Unmarshal(payload, &submitted); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(first["tools"], submitted.Tools) {
				t.Fatalf("first tools=%#v want=%#v", first["tools"], submitted.Tools)
			}
			if tc.wantAttempts == 2 {
				var retried map[string]any
				if err := json.Unmarshal(<-requests, &retried); err != nil {
					t.Fatal(err)
				}
				first["tool_choice"] = "required"
				first["tools"] = submitted.Tools[1:]
				if !reflect.DeepEqual(retried, first) {
					t.Fatalf("retried body=%#v want=%#v", retried, first)
				}
			}
			if tc.acceptRetry {
				waitForProxyLog(t, env, "hy3")
				logs, err := env.store.ListLogs(t.Context(), time.Now().Add(-time.Minute), 10, 0, &model.LogFilter{LogSource: model.LogSourceProxy})
				if err != nil {
					t.Fatal(err)
				}
				if len(logs) != 1 || logs[0].StatusCode != http.StatusOK || logs[0].InputTokens != 7 || logs[0].OutputTokens != 3 {
					t.Fatalf("proxy logs=%+v, want one HTTP 200 with 7 input and 3 output tokens", logs)
				}
			}
		})
	}
}

/**
 * @brief 验证过期重置时间回退到 Retry-After，流内头只有有效时才能覆盖真实 HTTP 头。
 * @param t 测试上下文。
 */
func TestProxyCodeBuddyRetryAfterHints(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		sse            bool
		eventHeader    string
		wantEventDelay bool
	}{
		{name: "HTTP JSON"},
		{name: "SSE missing event header", sse: true},
		{name: "SSE invalid event header", sse: true, eventHeader: "; bad upstream hint"},
		{name: "SSE valid event header", sse: true, wantEventDelay: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := time.Now().UTC().Truncate(time.Second)
			httpUntil := now.Add(7 * time.Minute)
			eventUntil := now.Add(11 * time.Minute)
			eventHeader := tc.eventHeader
			want := httpUntil
			if tc.wantEventDelay {
				eventHeader = eventUntil.Format(http.TimeFormat)
				want = eventUntil
			}
			env, _ := setupCodeBuddyCommaRetryTest(t, nil, func(w http.ResponseWriter, _ int) {
				w.Header().Set("Retry-After", httpUntil.Format(http.TimeFormat))
				form := fmt.Sprintf(`{"status":"busy","error":{"code":6003,"reset_at":%d},"headers":{"retry-after":%q}}`, now.Add(-time.Minute).UnixMilli(), eventHeader)
				if tc.sse {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = fmt.Fprintf(w, "data: %s\n\n", form)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, form)
			})
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "hy3", "stream": tc.sse, "messages": []any{map[string]string{"role": "user", "content": "hi"}},
			}, nil)
			if response.Code != http.StatusTooManyRequests {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
			cooldowns, err := env.store.GetAllModelCooldowns(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if got := cooldowns[1]["hy3"]; !got.Equal(want) {
				t.Fatalf("model cooldown=%v, want %v", got, want)
			}
		})
	}
}

/**
 * @brief 验证收到模型繁忙事件后取消已提交流也会写模型冷却，且保留取消状态。
 * @param t 测试上下文。
 */
func TestProxyCodeBuddyCanceledStreamKeepsBusyCooldown(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client := &cancelOnSSEOutput{
		ResponseRecorder: httptest.NewRecorder(), firstOutput: make(chan struct{}),
		cancelOn: []byte("14003"), cancel: cancel,
	}
	until := time.Now().Truncate(time.Second).Add(8 * time.Minute)
	var attempts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+`{"choices":[{"index":0,"delta":{"content":"partial-answer"}}]}`+"\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"code\":14003,\"reset_at\":%d}\n\n", until.UnixMilli())
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
			t.Error("client cancellation did not close the upstream stream")
		}
	}))
	t.Cleanup(upstream.Close)
	credential, err := (&codebuddyauth.Credential{AccessToken: "access"}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "codebuddy", upstreamProtocol: "openai", models: "hy3,other",
		authType: model.AuthTypeCodeBuddyOAuth, oauthCredential: credential,
	}}, map[int]string{0: upstream.URL + "/v2/chat/completions#"})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"hy3","stream":true,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-api-key")
	env.engine.ServeHTTP(client, req)
	if client.Code != 200 || ctx.Err() != context.Canceled || attempts.Load() != 1 {
		t.Fatalf("status=%d client context=%v attempts=%d body=%s", client.Code, ctx.Err(), attempts.Load(), client.Body)
	}
	if log := waitForProxyLog(t, env, "hy3"); log.StatusCode != 499 {
		t.Fatalf("status=%d, want 499", log.StatusCode)
	}
	cooldowns, err := env.store.GetAllModelCooldowns(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := cooldowns[1]["hy3"]; !got.Equal(until) || len(cooldowns[1]) != 1 {
		t.Fatalf("model cooldowns=%v, want hy3 until %v", cooldowns, until)
	}
}

/**
 * @brief 创建 CodeBuddy 的代理测试环境并收集实际出站请求。
 * @param t 当前测试上下文。
 * @param rules 渠道请求体规则；nil 表示无额外规则。
 * @param respond 按从一开始的请求次数生成上游响应。
 * @return 带独立 Server 与策略缓存的代理环境。
 * @return 出站 JSON 队列；每次代理调用返回后可读取对应尝试。
 * @note 上游服务在测试结束时关闭，respond 在 HTTP 服务协程中执行。
 */
func setupCodeBuddyCommaRetryTest(t *testing.T, rules *model.CustomRequestRules, respond func(http.ResponseWriter, int)) (*proxyTestEnv, <-chan []byte) {
	t.Helper()
	requests := make(chan []byte, 16)
	var attempts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		requests <- body
		respond(w, int(attempts.Add(1)))
	}))
	t.Cleanup(upstream.Close)
	credential, err := (&codebuddyauth.Credential{AccessToken: "access"}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	env := setupProxyTestEnvWithSettings(t, []testChannel{{
		name: "codebuddy-comma", upstreamProtocol: "openai", models: "hy3,deepseek-v4.1-flash",
		authType: model.AuthTypeCodeBuddyOAuth, oauthCredential: credential, customRequestRules: rules,
	}}, map[int]string{0: upstream.URL + "/v2/chat/completions#"}, map[string]string{"antigravity_sensitive_words": `[]`})
	return env, requests
}

/**
 * @brief 输出完整成功的 CodeBuddy SSE，供流式和非流式客户端共用。
 * @param w 上游 HTTP 响应写入器。
 */
func writeCodeBuddyCommaRetrySuccess(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "data: "+`{"id":"comma-success","model":"hy3","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`+"\n\ndata: [DONE]\n\n")
}
