package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	antigravityclaude "ccLoad/internal/protocol/cliproxy/providers/antigravity/claude"

	"github.com/tidwall/gjson"
)

const antigravitySchemaTestModel = "gemini-3-pro"

/**
 * @brief 代理出站中和已知身份及精确触发句，保留指令含义、用户消息与工具描述。
 * @param t 测试上下文。
 */
func TestProxyAntigravitySystemIdentity(t *testing.T) {
	t.Parallel()
	const identity = "You are Claude Code, Anthropic's official CLI for Claude."
	for _, tc := range []struct {
		name   string
		prompt string
		want   string
	}{
		{"claude code", identity + "\nKeep all instructions.", "You are an AI agent.\nKeep all instructions."},
		{"claude sdk", "You are a Claude agent, built on Anthropic's Claude Agent SDK.", "You are an AI agent."},
		{"codex", "You are Codex, an agent based on GPT-5.", "You are an AI agent."},
		{"codex coding agent", "You are Codex, a coding agent based on GPT-6.1-sol.\nKeep all instructions.", "You are an AI agent.\nKeep all instructions."},
		{"codex cli", "You are GPT-5.4 running in the Codex CLI.", "You are an AI agent."},
		{"pi", "You are a coding assistant running inside pi, a coding agent harness.\nKeep instructions.", "You are an AI agent.\nKeep instructions."},
		{"pi short", "You are a coding assistant running inside pi.", "You are an AI agent."},
		{"pi different product", "You are a coding assistant running inside pip.", "You are a coding assistant running inside pip."},
		{"deepseek harness", "You are an AI agent powered by DeepSeek Harness.\nKeep instructions.", "You are an AI agent.\nKeep instructions."},
		{"hermes", "You are Hermes Agent, an intelligent AI assistant created by Nous Research.", "You are an AI agent."},
		{"opencode", "You are OpenCode, an interactive CLI agent that helps users with software engineering tasks.", "You're OpenCode, an interactive CLI agent that helps users with software engineering tasks."},
		{"opencode different product", "You are OpenCodeX, an interactive CLI agent.", "You are OpenCodeX, an interactive CLI agent."},
		{"opencode unicode product suffix", "You are OpenCode工具, an interactive CLI agent.", "You are OpenCode工具, an interactive CLI agent."},
		{"opencode whitespace", "\tYOU\n ARE\u00a0OpenCode,\r\nan interactive CLI agent.", "\tYOU're OpenCode,\r\nan interactive CLI agent."},
		{"codex generic cli", "You are a coding agent running in the Codex CLI.", "You're a coding agent running in the Codex CLI."},
		{"sdk whitespace", "\tYou \nare a\tClaude agent,\u00a0built on Anthropic's Claude Agent SDK.\r\n保留\t  原文。", "You are an AI agent.\r\n保留\t  原文。"},
		{"sdk invisible characters", "Y\u200bou are a Cl\u034faude agent, built on Anthropic's Claude Agent SDK.\nKeep instructions.", "You are an AI agent.\nKeep instructions."},
		{"invisible suffix boundary", identity + "\u200bexample", identity + "\u200bexample"},
		{"ordinary harness mention", "Explain DeepSeek Harness and Hermes Agent.", "Explain DeepSeek Harness and Hermes Agent."},
		{
			"security authorization",
			"Refuse malicious requests. Dual-use security tools (C2 frameworks, credential testing, exploit development) require clear authorization context: pentesting engagements, CTF competitions, security research, or defensive use cases.\nKeep all instructions.",
			"Refuse malicious requests. Security tooling, including command-and-control frameworks, credential checks, and exploit development, may be used only with explicit authorization for penetration tests, CTFs, security research, or defensive work.\nKeep all instructions.",
		},
		{
			"omp tool documentation",
			"Read xd://<tool> for full docs + JSON schema before first use.",
			"Before using a tool for the first time, read its full documentation and JSON schema at xd://<tool>.",
		},
		{
			"omp whitespace and uri case",
			"保\u034f留\t  原文🙂\nREAD\t XD://<TOOL>\nFOR  FULL\u00a0DOCS + JSON SCHEMA BEFORE FIRST USE.\r\n结束\u200b。",
			"保\u034f留\t  原文🙂\nBefore using a tool for the first time, read its full documentation and JSON schema at XD://<TOOL>.\r\n结束\u200b。",
		},
		{
			"omp invisible characters",
			"Re\u200bad xd://<tool> for fu\u034fll docs + JSON schema before first use.",
			"Before using a tool for the first time, read its full documentation and JSON schema at xd://<tool>.",
		},
		{
			"different tool documentation",
			"Read xd://example for full docs + JSON schema before first use.",
			"Read xd://example for full docs + JSON schema before first use.",
		},
		{
			"astrbot sessions",
			"Use `astrbot_shell_session` to list, poll, write raw text or complete lines to, interrupt, or terminate those sessions.\nKeep instructions.",
			"Manage these command sessions with `astrbot_shell_session`: list sessions, poll their output, send raw text or complete input lines, interrupt them, or terminate them.\nKeep instructions.",
		},
		{
			"astrbot preserves tool name case",
			"USE `ASTRBOT_SHELL_SESSION` TO LIST,\nPOLL, WRITE RAW TEXT OR COMPLETE LINES TO, INTERRUPT, OR TERMINATE THOSE SESSIONS.",
			"Manage these command sessions with `ASTRBOT_SHELL_SESSION`: list sessions, poll their output, send raw text or complete input lines, interrupt them, or terminate them.",
		},
		{
			"rfc and repeated matches preserve surrounding text",
			"前文\u034f。\nRFC 2119 KEYWORDS:\nMUST, REQUIRED, SHOULD, RECOMMENDED, MAY, OPTIONAL.\n\t中间🙂\nRFC 2119 keywords: MUST, REQUIRED, SHOULD, RECOMMENDED, MAY, OPTIONAL.\n后文。",
			"前文\u034f。\nRFC 2119 KEY WORDS:\nMUST, REQUIRED, SHOULD, RECOMMENDED, MAY, OPTIONAL.\n\t中间🙂\nRFC 2119 key words: MUST, REQUIRED, SHOULD, RECOMMENDED, MAY, OPTIONAL.\n后文。",
		},
		{"tool identifiers", "Use tools.opencode.session_move to move a session.", "Use tools.opencode.session_move to move a session."},
		{"comma continuation", "You are Codex, based on GPT-5, with access to tools.", "You are an AI agent, with access to tools."},
		{"case and whitespace", " \t\nYOU ARE CLAUDE CODE, ANTHROPICS OFFICIAL CLI FOR CLAUDE", "You are an AI agent."},
		{"ordinary mention", "Discuss this sentence: " + identity, "Discuss this sentence: " + identity},
		{"later line", "Keep all instructions.\n" + identity, "Keep all instructions.\n" + identity},
		{"different product", "You are Claude Code, Anthropic's official CLI for ClaudeX.", "You are Claude Code, Anthropic's official CLI for ClaudeX."},
		{"suffix boundary", identity + "example", identity + "example"},
		{"neutral", "You are an AI agent.", "You are an AI agent."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				parts := gjson.GetBytes(body, "request.systemInstruction.parts").Array()
				if len(parts) != 2 || parts[1].Get("text").String() != tc.want {
					t.Errorf("system instruction=%s, want final part %q", gjson.GetBytes(body, "request.systemInstruction"), tc.want)
				}
				if gjson.GetBytes(body, "request.contents.0.parts.0.text").String() != tc.prompt ||
					gjson.GetBytes(body, "request.tools.0.functionDeclarations.0.description").String() != tc.prompt {
					t.Errorf("identity rewrite changed user content or tool description: %s", body)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}}`)
			}))
			env := setupProxyTestEnvWithSettings(t, []testChannel{{
				name: "identity", upstreamProtocol: "gemini", models: "gemini-3-flash",
				authType: model.AuthTypeAntigravityOAuth, oauthCredential: antigravityProxyTestCredential(t, "identity-at"),
			}}, map[int]string{0: upstream.URL}, map[string]string{"antigravity_sensitive_words": `[]`})
			response := doProxyRequest(t, env.engine, "/v1beta/models/gemini-3-flash:generateContent", map[string]any{
				"system_instruction": map[string]any{"parts": []any{map[string]any{"text": tc.prompt}}},
				"contents":           []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": tc.prompt}}}},
				"tools": []any{map[string]any{"functionDeclarations": []any{map[string]any{
					"name": "lookup", "description": tc.prompt, "parameters": map[string]any{"type": "object"},
				}}}},
			}, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

/**
 * @brief 四种客户端协议及字符串系统指令先改写已知触发句，再按配置混淆其他文本。
 * @param t 测试上下文。
 */
func TestProxyAntigravitySystemIdentityAcrossProtocols(t *testing.T) {
	t.Parallel()
	const identity = "You are Claude Code, Anthropic's official CLI for Claude.\nKeep API instructions."
	const prompt = identity + "\n" +
		"Dual-use security tools (C2 frameworks, credential testing, exploit development) require clear authorization context: pentesting engagements, CTF competitions, security research, or defensive use cases.\n" +
		"Read xd://<tool> for full docs + JSON schema before first use.\n" +
		"Use `astrbot_shell_session` to list, poll, write raw text or complete lines to, interrupt, or terminate those sessions."
	const want = "You are an AI agent.\nKeep A\u034FPI instructions.\n" +
		"Security tooling, including command-and-control frameworks, credential checks, and exploit development, may be used only with explicit authorization for penetration tests, CTFs, security research, or defensive work.\n" +
		"Before using a tool for the first time, read its full documentation and JSON schema at xd://<tool>.\n" +
		"Manage these command sessions with `astrbot_shell_session`: list sessions, poll their output, send raw text or complete input lines, interrupt them, or terminate them."
	for _, clientProtocol := range []string{"anthropic", "openai", "codex", "gemini", "gemini existing identity"} {
		t.Run(clientProtocol, func(t *testing.T) {
			t.Parallel()
			var rules *model.CustomRequestRules
			if clientProtocol == "gemini existing identity" {
				value, err := json.Marshal(prompt + "\nYou are Antigravity.")
				if err != nil {
					t.Fatal(err)
				}
				rules = &model.CustomRequestRules{Body: []model.CustomBodyRule{{Action: model.RuleActionOverride, Path: "request.systemInstruction", Value: value}}}
			}
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				instruction := gjson.GetBytes(body, "request.systemInstruction")
				parts := instruction.Get("parts").Array()
				if clientProtocol == "gemini existing identity" {
					if instruction.String() != want+"\nYou are Antigravity." {
						t.Errorf("existing string identity not rewritten: %s", body)
					}
				} else if len(parts) != 2 || parts[1].Get("text").String() != want {
					t.Errorf("identity not neutralized before obfuscation: %s", body)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}}`)
			}))
			env := setupProxyTestEnv(t, []testChannel{{
				name: "identity", upstreamProtocol: "gemini", models: "gemini-3-flash",
				authType: model.AuthTypeAntigravityOAuth, oauthCredential: antigravityProxyTestCredential(t, "identity-at"),
				customRequestRules: rules,
			}}, map[int]string{0: upstream.URL})
			body := map[string]any{"model": "gemini-3-flash"}
			path := "/v1/messages"
			switch clientProtocol {
			case "anthropic":
				body["system"] = prompt
				body["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
				body["max_tokens"] = 128
			case "openai":
				path = "/v1/chat/completions"
				body["messages"] = []any{map[string]any{"role": "system", "content": prompt}, map[string]any{"role": "user", "content": "hello"}}
			case "codex":
				path = "/v1/responses"
				body["input"] = []any{map[string]any{"role": "developer", "content": prompt}, map[string]any{"role": "user", "content": "hello"}}
			case "gemini", "gemini existing identity":
				path = "/v1beta/models/gemini-3-flash:generateContent"
				body["systemInstruction"] = prompt
				body["contents"] = []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}}
			}
			response := doProxyRequest(t, env.engine, path, body, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

/**
 * @brief 验证只有完整 TOS 违规签名会持久停用渠道，HTTP 与流内错误均生效且不暂停其他账号。
 * @param t 测试上下文。
 */
func TestProxyAntigravityTOSViolation(t *testing.T) {
	t.Parallel()
	tosBody := []byte(`{"error":{"code":403,"status":"PERMISSION_DENIED","message":"Account suspended for TOS_VIOLATION","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION","metadata":{"appeal_url":"https://example.com/appeal?id=private","appeal_url_link_text":"Appeal"}}]}}`)
	textFrame := `data: {"response":{"responseId":"r1","candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}],"modelVersion":"gemini-3-flash"}}` + "\n\n"
	completeFrame := `data: {"response":{"responseId":"r1","candidates":[{"content":{"role":"model","parts":[{"text":"complete"}]},"finishReason":"STOP"}],"modelVersion":"gemini-3-flash"}}` + "\n\n"
	for _, tc := range []struct {
		name         string
		body         []byte
		streamPrefix string
		stream       bool
		disabled     bool
	}{
		{name: "http", body: tosBody, disabled: true},
		{name: "first SSE error", body: tosBody, stream: true, disabled: true},
		{name: "midstream SSE error", body: tosBody, stream: true, streamPrefix: textFrame, disabled: true},
		{name: "error after completed stream", body: tosBody, stream: true, streamPrefix: completeFrame},
		{name: "ordinary permission error", body: setJSONValue(tosBody, "error.details.0.reason", "ACCESS_DENIED")},
		{name: "wrong RPC detail type", body: setJSONValue(tosBody, "error.details.0.@type", "type.googleapis.com/google.rpc.BadRequest")},
		{name: "wrong RPC status", body: setJSONValue(tosBody, "error.status", "UNAVAILABLE")},
		{name: "wrong RPC code", body: setJSONValue(tosBody, "error.code", 429)},
		{name: "string RPC code", body: setJSONValue(tosBody, "error.code", "403")},
		{name: "fractional RPC code", body: setJSONValue(tosBody, "error.code", 403.5)},
		{name: "invalid JSON", body: append(append([]byte(nil), tosBody...), []byte("trailing")...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var primaryCalls atomic.Int32
			primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				primaryCalls.Add(1)
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, tc.streamPrefix+"data: "+string(tc.body)+"\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write(tc.body)
			}))
			var fallbackCalls atomic.Int32
			fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fallbackCalls.Add(1)
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, strings.ReplaceAll(completeFrame, "complete", "fallback"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"fallback"}]},"finishReason":"STOP"}],"modelVersion":"gemini-3-flash"}}`)
			}))
			env := setupProxyTestEnv(t, []testChannel{
				{name: "tos-primary", upstreamProtocol: "gemini", models: "gemini-3-flash", priority: 100, authType: model.AuthTypeAntigravityOAuth, oauthCredential: antigravityProxyTestCredential(t, "tos-at")},
				{name: "tos-fallback", upstreamProtocol: "gemini", models: "gemini-3-flash", priority: 10, authType: model.AuthTypeAntigravityOAuth, oauthCredential: string(setJSONValue([]byte(antigravityProxyTestCredential(t, "fallback-at")), "refresh_token", "fallback-rt"))},
			}, map[int]string{0: primary.URL, 1: fallback.URL})
			ctx := context.Background()
			cached, err := env.server.GetEnabledChannelsByModel(ctx, "gemini-3-flash")
			if err != nil || len(cached) != 2 {
				t.Fatalf("initial channels=%d err=%v", len(cached), err)
			}
			request := map[string]any{"model": "gemini-3-flash", "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "stream": tc.stream}
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", request, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("response=%d body=%s", response.Code, response.Body.String())
			}
			configs, err := env.store.ListConfigs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, cfg := range configs {
				if cfg.Name == "tos-primary" {
					if cfg.Enabled == tc.disabled {
						t.Fatalf("primary enabled=%v, want %v", cfg.Enabled, !tc.disabled)
					}
					if tc.disabled && (cfg.CooldownUntil != 0 || cfg.CooldownDurationMs != 0) {
						t.Fatalf("disabled channel retains cooldown: %+v", cfg)
					}
				} else if !cfg.Enabled {
					t.Fatal("TOS violation disabled another account")
				}
			}
			if !tc.disabled {
				return
			}
			cached, err = env.server.GetEnabledChannelsByModel(ctx, "gemini-3-flash")
			if err != nil || len(cached) != 1 || cached[0].Name != "tos-fallback" {
				t.Fatalf("enabled channels after TOS=%v err=%v", cached, err)
			}
			response = doProxyRequest(t, env.engine, "/v1/chat/completions", request, nil)
			if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "fallback") || fallbackCalls.Load() == 0 {
				t.Fatalf("other account unavailable: status=%d body=%s", response.Code, response.Body.String())
			}
			if primaryCalls.Load() != 1 {
				t.Fatalf("disabled account requested %d times", primaryCalls.Load())
			}
		})
	}
}

/**
 * @brief 验证账号验证错误冷却当前渠道，普通请求和 Credits 尝试均可换渠，候选耗尽时保留验证详情。
 * @param t 测试上下文。
 */
func TestProxyAntigravityValidationRequired(t *testing.T) {
	t.Parallel()
	const modelName = "claude-sonnet-4-6"
	const rejection = `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"Verify your account to continue.","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"VALIDATION_REQUIRED","metadata":{"validation_url":"https://example.com/verify"}}]}}`
	const success = `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"backup"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}}`
	for _, tc := range []struct {
		name        string
		stream      bool
		paid        bool
		messageOnly bool
		noBackup    bool
	}{
		{name: "HTTP before credits"},
		{name: "first SSE frame before credits", stream: true},
		{name: "HTTP during credits", paid: true},
		{name: "first SSE frame during credits", stream: true, paid: true},
		{name: "message alone allows fallback", messageOnly: true},
		{name: "no backup preserves verification details", noBackup: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			credential := func(token string, paid bool) string {
				t.Helper()
				creds, err := antigravityauth.ParseCredential([]byte(antigravityProxyTestCredential(t, token)))
				if err != nil {
					t.Fatal(err)
				}
				creds.RefreshToken = "refresh-" + token
				creds.Email = token + "@example.com"
				if paid {
					balance, minimum := 50.0, 1.0
					creds.Credits = &antigravityauth.Credits{Balance: &balance, Minimum: &minimum, SampledAt: time.Now()}
					creds.StandardQuota = map[string]time.Time{modelName: time.Now().Add(time.Hour)}
				}
				raw, err := creds.JSON()
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			var primaryCalls, backupCalls atomic.Int32
			primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryCalls.Add(1)
				var request struct {
					EnabledCreditTypes []string `json:"enabledCreditTypes"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
				}
				if gotPaid := len(request.EnabledCreditTypes) > 0; gotPaid != tc.paid {
					t.Errorf("primary spent credits=%t, want %t", gotPaid, tc.paid)
				}
				w.Header().Set("X-Verification-Request", "trace-id")
				body := rejection
				if tc.messageOnly {
					body = strings.Replace(body, `"reason":"VALIDATION_REQUIRED"`, `"reason":"ACCESS_DENIED"`, 1)
				}
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+body+"\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, body)
			}))
			backup := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				backupCalls.Add(1)
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: "+success+"\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, success)
			}))
			channels := []testChannel{
				{name: "verify", models: modelName, upstreamProtocol: "gemini", priority: 100, authType: model.AuthTypeAntigravityOAuth, oauthCredential: credential("verify", tc.paid)},
				{name: "backup", models: modelName, upstreamProtocol: "gemini", priority: 10, authType: model.AuthTypeAntigravityOAuth, oauthCredential: credential("backup", tc.paid)},
			}
			if tc.noBackup {
				channels = channels[:1]
			}
			env := setupProxyTestEnvWithSettings(t, channels, map[int]string{0: primary.URL, 1: backup.URL}, map[string]string{"cooldown_fallback_enabled": "false"})
			method := "generateContent"
			if tc.stream {
				method = "streamGenerateContent?alt=sse"
			}
			response := doProxyRequest(t, env.engine, "/v1beta/models/"+modelName+":"+method,
				map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}}}, nil)
			wantStatus, wantBackup := http.StatusOK, int32(1)
			if tc.noBackup {
				wantStatus, wantBackup = http.StatusForbidden, 0
				if got := gjson.Get(response.Body.String(), "error.details.0.metadata.validation_url").String(); got != "https://example.com/verify" || response.Header().Get("X-Verification-Request") != "trace-id" {
					t.Fatalf("verification response lost upstream details: headers=%v body=%s", response.Header(), response.Body.String())
				}
			}
			if response.Code != wantStatus || primaryCalls.Load() != 1 || backupCalls.Load() != wantBackup {
				t.Fatalf("response=%d primary=%d backup=%d body=%s", response.Code, primaryCalls.Load(), backupCalls.Load(), response.Body.String())
			}
			configs, err := env.store.ListConfigs(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			for _, cfg := range configs {
				if !cfg.Enabled || (cfg.CooldownUntil > time.Now().Unix()) != (cfg.Name == "verify") {
					t.Fatalf("channel %s enabled=%t cooldown_until=%d", cfg.Name, cfg.Enabled, cfg.CooldownUntil)
				}
			}
		})
	}
}

/**
 * @brief 验证在途验证结果仅冷却仍使用同一 refresh token 的账号，已提交流不重放。
 * @param t 测试上下文。
 */
func TestProxyAntigravityValidationRequiredCredentialReplacement(t *testing.T) {
	t.Parallel()
	const rejection = `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"VALIDATION_REQUIRED"}]}}`
	const textFrame = `data: {"response":{"responseId":"r1","candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}],"modelVersion":"gemini-3-flash"}}` + "\n\n"
	for _, tc := range []struct {
		name           string
		stream         bool
		committed      bool
		replaceAccount bool
	}{
		{name: "HTTP same account refresh"},
		{name: "HTTP different account", replaceAccount: true},
		{name: "first SSE error same account refresh", stream: true},
		{name: "first SSE error different account", stream: true, replaceAccount: true},
		{name: "committed SSE same account refresh", stream: true, committed: true},
		{name: "committed SSE different account", stream: true, committed: true, replaceAccount: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			original := antigravityProxyTestCredential(t, "old-at")
			renewed := string(setJSONValue([]byte(original), "access_token", "new-at"))
			if tc.replaceAccount {
				renewed = string(setJSONValue([]byte(renewed), "refresh_token", "new-account-rt"))
			}
			var env *proxyTestEnv
			var channelID int64
			var calls atomic.Int32
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) != 1 || r.Header.Get("Authorization") != "Bearer old-at" {
					t.Error("verification request was sent again or used the replacement token")
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				updated, err := env.store.CompareAndSwapOAuthCredential(r.Context(), channelID, model.AuthTypeAntigravityOAuth, original, renewed)
				if err != nil || !updated {
					t.Errorf("concurrent credential update=%v err=%v", updated, err)
				}
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					if tc.committed {
						_, _ = io.WriteString(w, textFrame)
					}
					_, _ = io.WriteString(w, "data: "+rejection+"\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, rejection)
			}))
			channel := testChannel{
				name: "validation-race", upstreamProtocol: "gemini", models: "gemini-3-flash",
				authType: model.AuthTypeAntigravityOAuth, oauthCredential: original,
				cooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
					Enabled: true, Name: "verification cooldown", StatusCodes: []int{http.StatusForbidden},
					Scope: model.CooldownScopeChannel, Mode: model.CooldownModeFixed, CooldownSeconds: 240,
				}}},
			}
			env = setupProxyTestEnv(t, []testChannel{channel}, map[int]string{0: upstream.URL})
			configs, err := env.store.ListConfigs(t.Context())
			if err != nil || len(configs) != 1 {
				t.Fatalf("configs=%d err=%v", len(configs), err)
			}
			channelID = configs[0].ID
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "gemini-3-flash", "stream": tc.stream, "messages": []any{map[string]any{"role": "user", "content": "hello"}},
			}, nil)
			wantStatus := http.StatusForbidden
			if tc.committed {
				wantStatus = http.StatusOK
			}
			if response.Code != wantStatus || calls.Load() != 1 {
				t.Fatalf("response=%d upstream calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
			}
			current, err := env.store.GetConfig(t.Context(), channelID)
			if err != nil {
				t.Fatal(err)
			}
			if !current.Enabled || current.OAuthCredential != renewed || (current.CooldownUntil > time.Now().Unix()) == tc.replaceAccount {
				t.Fatalf("enabled=%t credential changed=%t cooldown=%d", current.Enabled, current.OAuthCredential != renewed, current.CooldownUntil)
			}
			if !tc.stream && !tc.replaceAccount {
				if remaining := time.Until(time.Unix(current.CooldownUntil, 0)); remaining < 237*time.Second || remaining > 240*time.Second {
					t.Fatalf("cooldown=%s, want about 240s from configured rule", remaining)
				}
			}
		})
	}
}

/**
 * @brief 验证在途 TOS 结果跨 access token 刷新仍停用原账号，替换为另一账号后不受旧结果影响。
 * @param t 测试上下文。
 */
func TestProxyAntigravityTOSViolationCredentialReplacement(t *testing.T) {
	t.Parallel()
	for _, replaceAccount := range []bool{false, true} {
		name := "same account refresh"
		if replaceAccount {
			name = "different account"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var env *proxyTestEnv
			var channelID int64
			original := antigravityProxyTestCredential(t, "old-at")
			renewed := string(setJSONValue([]byte(original), "access_token", "new-at"))
			if replaceAccount {
				renewed = string(setJSONValue([]byte(renewed), "refresh_token", "new-account-rt"))
			}
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				updated, err := env.store.CompareAndSwapOAuthCredential(context.Background(), channelID, model.AuthTypeAntigravityOAuth, original, renewed)
				if err != nil || !updated {
					t.Errorf("concurrent credential update=%v err=%v", updated, err)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION"}]}}`)
			}))
			env = setupProxyTestEnv(t, []testChannel{{name: "tos-race", upstreamProtocol: "gemini", models: "gemini-3-flash", authType: model.AuthTypeAntigravityOAuth, oauthCredential: original}}, map[int]string{0: upstream.URL})
			configs, err := env.store.ListConfigs(context.Background())
			if err != nil || len(configs) != 1 {
				t.Fatalf("configs=%d err=%v", len(configs), err)
			}
			channelID = configs[0].ID
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "gemini-3-flash", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}, nil)
			if response.Code != http.StatusForbidden {
				t.Fatalf("response=%d body=%s", response.Code, response.Body.String())
			}
			current, err := env.store.GetConfig(context.Background(), channelID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Enabled != replaceAccount || current.OAuthCredential != renewed || current.CooldownUntil != 0 {
				t.Fatalf("concurrent credential result: enabled=%v credential=%s cooldown=%d", current.Enabled, current.OAuthCredential, current.CooldownUntil)
			}
		})
	}
}

/**
 * @brief 验证最终发送的 Antigravity 请求按模型能力保留 xhigh，并兼容旧等级映射。
 * @param t 测试上下文。
 */
func TestAntigravityGeminiWireThinkingLevels(t *testing.T) {
	cfg := &model.Config{AuthType: model.AuthTypeAntigravityOAuth, AntigravityProjectID: "test-project"}
	for _, tc := range []struct {
		model string
		level string
		want  string
	}{
		{"gemini-4-hydrogen-xhigh", " XHIGH ", "xhigh"},
		{"gemini-4-neon-xhigh", "xhigh", "xhigh"},
		{"gemini-3.8-flash-tiered", "xhigh", "high"},
		{"gemini-3-pro", "xhigh", "high"},
		{"gemini-unlisted", "xhigh", "high"},
		{"gemini-4-hydrogen-xhigh", "max", "high"},
		{"gemini-4-hydrogen-xhigh", "minimal", "low"},
	} {
		t.Run(tc.model+"/"+tc.level, func(t *testing.T) {
			for _, path := range []string{"generationConfig.thinkingConfig.thinkingLevel", "generation_config.thinking_config.thinking_level"} {
				body := setJSONValue([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`), path, tc.level)
				got, err := prepareAntigravityRequestBody(cfg, tc.model, body, body, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				outputPath := "request." + strings.Replace(path, "generation_config", "generationConfig", 1)
				if level := gjson.GetBytes(got, outputPath).String(); level != tc.want {
					t.Fatalf("%s=%q, want %q; body=%s", path, level, tc.want, got)
				}
			}
		})
	}
}

func TestAntigravityGeminiWireRejectsVertexSignature(t *testing.T) {
	t.Parallel()
	sig := base64.StdEncoding.EncodeToString(append([]byte{1, 0x8f, 0x3d, 0x6b, 0x5f}, bytes.Repeat([]byte{0x42}, 400)...))
	body := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{}},"thoughtSignature":"` + sig + `"}]}]}`)
	cfg := &model.Config{AuthType: model.AuthTypeAntigravityOAuth, AntigravityProjectID: "test-project"}
	got, err := prepareAntigravityRequestBody(cfg, "gemini-3-pro", body, body, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	part := gjson.GetBytes(got, "request.contents.0.parts.0")
	if part.Get("thoughtSignature").String() != "skip_thought_signature_validator" || part.Get("functionCall.name").String() != "lookup" {
		t.Fatalf("Vertex signature reached Antigravity: %s", got)
	}
}

/**
 * @brief 验证客户端禁用工具后，Antigravity Claude 实际收到的请求保留 NONE。
 * @param t 测试上下文。
 */
func TestProxyAntigravityClaudeToolChoiceNone(t *testing.T) {
	t.Parallel()
	const target = "claude-sonnet-4-6"
	for _, tc := range []struct{ name, path, body, modePath string }{
		{"chat", "/v1/chat/completions", `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":"none"}`, "toolConfig.functionCallingConfig.mode"},
		{"gemini alias", "/v1beta/models/" + target + ":generateContent", `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tool_config":{"function_calling_config":{"mode":" none "}}}`, "tool_config.function_calling_config.mode"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wire, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				request := gjson.GetBytes(wire, "request")
				if mode := request.Get(tc.modePath).String(); mode != "NONE" {
					t.Errorf("%s=%q, want NONE; request=%s", tc.modePath, mode, wire)
				}
				if tc.name == "gemini alias" && request.Get("toolConfig.functionCallingConfig.mode").Exists() {
					t.Errorf("conflicting tool modes: %s", wire)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}}`)
			}))
			t.Cleanup(upstream.Close)
			env := setupProxyTestEnv(t, []testChannel{{
				name: "antigravity", upstreamProtocol: "gemini", models: target, authType: model.AuthTypeAntigravityOAuth,
				oauthCredential: antigravityProxyTestCredential(t, "tool-mode"),
			}}, map[int]string{0: upstream.URL})
			var body map[string]any
			if err := json.Unmarshal([]byte(tc.body), &body); err != nil {
				t.Fatal(err)
			}
			body["model"] = target
			response := doProxyRequest(t, env.engine, tc.path, body, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestPrepareAntigravityRequestBodyClaude55StructuredOutput(t *testing.T) {
	t.Parallel()

	const schema = `{"type":"object","title":"Answer","description":"Final answer","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}`
	body := []byte(`{"request":{"contents":[{"role":"user","parts":[{"text":"hi"}]}],` +
		`"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object"}}]}],` +
		`"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["lookup"]}},` +
		`"generationConfig":{"temperature":0.2,"responseMimeType":"application/json","responseSchema":` + schema + `}}}`)
	cfg := &model.Config{AuthType: model.AuthTypeAntigravityOAuth, AntigravityProjectID: "gravity-project"}
	// 敏感词命中 schema 字段名：提示词必须在混淆之后追加，否则 schema 被插入零宽字符。
	matcher := regexp.MustCompile(`answer`)

	t.Run("claude 5.5 moves schema into the system prompt", func(t *testing.T) {
		t.Parallel()
		got, err := prepareAntigravityRequestBody(cfg, "claude-opus-5-5-high", body, body, http.Header{}, matcher)
		if err != nil {
			t.Fatal(err)
		}
		request := gjson.GetBytes(got, "request")
		if mode := request.Get("toolConfig.functionCallingConfig.mode").String(); mode != "AUTO" {
			t.Fatalf("mode=%q, want AUTO: %s", mode, got)
		}
		for _, path := range []string{"toolConfig.functionCallingConfig.allowedFunctionNames", "generationConfig.responseSchema", "generationConfig.responseMimeType"} {
			if request.Get(path).Exists() {
				t.Fatalf("%s survived: %s", path, got)
			}
		}
		if request.Get("generationConfig.temperature").Float() != 0.2 {
			t.Fatalf("unrelated generationConfig field lost: %s", got)
		}
		parts := request.Get("systemInstruction.parts").Array()
		if len(parts) == 0 {
			t.Fatalf("missing system prompt: %s", got)
		}
		prompt := parts[len(parts)-1].Get("text").String()
		start := strings.IndexByte(prompt, '{')
		if start < 0 {
			t.Fatalf("prompt has no schema: %q", prompt)
		}
		var gotSchema, wantSchema any
		if err := json.NewDecoder(strings.NewReader(prompt[start:])).Decode(&gotSchema); err != nil {
			t.Fatalf("decode prompt schema: %v: %q", err, prompt)
		}
		if err := json.Unmarshal([]byte(schema), &wantSchema); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotSchema, wantSchema) {
			t.Fatalf("prompt schema=%v, want untouched %v", gotSchema, wantSchema)
		}
	})

	t.Run("other claude keeps native structured output", func(t *testing.T) {
		t.Parallel()
		got, err := prepareAntigravityRequestBody(cfg, "claude-sonnet-4-6", body, body, http.Header{}, matcher)
		if err != nil {
			t.Fatal(err)
		}
		if mode := gjson.GetBytes(got, "request.toolConfig.functionCallingConfig.mode").String(); mode != "VALIDATED" {
			t.Fatalf("mode=%q, want VALIDATED: %s", mode, got)
		}
		if !gjson.GetBytes(got, "request.generationConfig.responseSchema").IsObject() {
			t.Fatalf("responseSchema removed: %s", got)
		}
	})
}

/**
 * @brief 验证 Antigravity 出站会话随账号变化，在令牌轮换后稳定，并保留显式 ID。
 * @param t 测试上下文。
 */
func TestProxyAntigravitySessionFollowsAccount(t *testing.T) {
	t.Parallel()
	type attempt struct {
		authorization string
		sessionID     string
	}
	sent := make(chan attempt, 8)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sent <- attempt{r.Header.Get("Authorization"), gjson.GetBytes(wire, "request.sessionId").String()}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}}`)
	}))
	credential := string(setJSONValue([]byte(antigravityProxyTestCredential(t, "original-at")), "email", "original@example.com"))
	env := setupProxyTestEnv(t, []testChannel{{
		name: "antigravity-session", upstreamProtocol: "gemini", models: "gemini-3-flash",
		authType: model.AuthTypeAntigravityOAuth, oauthCredential: credential,
	}}, map[int]string{0: upstream.URL})
	ctx := context.Background()
	channels, err := env.store.ListConfigs(ctx)
	if err != nil || len(channels) != 1 {
		t.Fatalf("load channel: count=%d, err=%v", len(channels), err)
	}
	channelID := channels[0].ID
	replaceCredential := func(accessToken, refreshToken, email string) {
		t.Helper()
		next := setJSONValue([]byte(credential), "access_token", accessToken)
		next = setJSONValue(next, "refresh_token", refreshToken)
		next = setJSONValue(next, "email", email)
		updated, err := env.store.CompareAndSwapOAuthCredential(ctx, channelID, model.AuthTypeAntigravityOAuth, credential, string(next))
		if err != nil || !updated {
			t.Fatalf("replace credential: updated=%v, err=%v", updated, err)
		}
		credential = string(next)
		env.server.antigravityCredentials.invalidate(channelID)
		env.server.InvalidateChannelListCache()
	}
	send := func(headers map[string]string, explicitID, accessToken string) string {
		t.Helper()
		body := map[string]any{
			"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}},
		}
		if explicitID != "" {
			body["sessionId"] = explicitID
		}
		response := doProxyRequest(t, env.engine, "/v1beta/models/gemini-3-flash:generateContent", body, headers)
		if response.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		select {
		case got := <-sent:
			if got.authorization != "Bearer "+accessToken || got.sessionID == "" {
				t.Fatalf("upstream authorization=%q, session ID=%q", got.authorization, got.sessionID)
			}
			return got.sessionID
		default:
			t.Fatal("upstream did not receive a request")
			return ""
		}
	}

	headers := map[string]string{"Session-Id": "-123"}
	original := send(headers, "", "original-at")
	fromContent := send(nil, "", "original-at")
	replaceCredential("rotated-at", "rotated-rt", "ORIGINAL@example.com")
	if got := send(headers, "", "rotated-at"); got != original {
		t.Fatalf("same account after rotation: session ID=%q, want %q", got, original)
	}
	if got := send(nil, "", "rotated-at"); got != fromContent {
		t.Fatalf("same account content session ID=%q, want %q", got, fromContent)
	}
	replaceCredential("other-at", "other-rt", "other@example.com")
	if got := send(headers, "", "other-at"); got == original {
		t.Fatalf("different accounts reused session ID %q", got)
	}
	if got := send(nil, "", "other-at"); got == fromContent {
		t.Fatalf("different accounts reused content session ID %q", got)
	}
	const explicitID = "client-session"
	if got := send(headers, explicitID, "other-at"); got != explicitID {
		t.Fatalf("explicit session ID=%q, want %q", got, explicitID)
	}
}

func TestNormalizeAntigravitySchemasConsolidatesParametersAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantSource string
	}{
		{
			name: "canonical and two aliases coexist",
			input: `{"tools":[{"functionDeclarations":[{
				"name":"fn",
				"parameters":{"type":"object","properties":{"canonical":{"type":"string"}}},
				"parametersJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"parameters_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}]}]}`,
			wantSource: "canonical",
		},
		{
			name: "two aliases coexist",
			input: `{"tools":[{"functionDeclarations":[{
				"name":"fn",
				"parametersJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"parameters_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}]}]}`,
			wantSource: "camel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeAntigravitySchemas([]byte(tt.input), antigravitySchemaTestModel)
			basePath := "tools.0.functionDeclarations.0"
			if !gjson.GetBytes(got, basePath+".parameters").IsObject() {
				t.Fatalf("parameters missing: %s", got)
			}
			if gotProp := gjson.GetBytes(got, basePath+".parameters.properties."+tt.wantSource+".type").String(); gotProp != "string" {
				t.Fatalf("parameters.properties.%s.type = %q, want string; output=%s", tt.wantSource, gotProp, got)
			}
			for _, alias := range []string{"parametersJsonSchema", "parameters_json_schema"} {
				if gjson.GetBytes(got, basePath+"."+alias).Exists() {
					t.Fatalf("%s should be removed; output=%s", alias, got)
				}
			}
		})
	}
}

func TestNormalizeAntigravitySchemasConsolidatesResponseAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantSource string
	}{
		{
			name: "canonical and two aliases coexist",
			input: `{"tools":[{"functionDeclarations":[{
				"name":"fn",
				"response":{"type":"object","properties":{"canonical":{"type":"string"}}},
				"responseJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"response_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}]}]}`,
			wantSource: "canonical",
		},
		{
			name: "two aliases coexist",
			input: `{"tools":[{"functionDeclarations":[{
				"name":"fn",
				"responseJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"response_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}]}]}`,
			wantSource: "camel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeAntigravitySchemas([]byte(tt.input), antigravitySchemaTestModel)
			basePath := "tools.0.functionDeclarations.0"
			if !gjson.GetBytes(got, basePath+".response").IsObject() {
				t.Fatalf("response missing: %s", got)
			}
			if gotProp := gjson.GetBytes(got, basePath+".response.properties."+tt.wantSource+".type").String(); gotProp != "string" {
				t.Fatalf("response.properties.%s.type = %q, want string; output=%s", tt.wantSource, gotProp, got)
			}
			for _, alias := range []string{"responseJsonSchema", "response_json_schema"} {
				if gjson.GetBytes(got, basePath+"."+alias).Exists() {
					t.Fatalf("%s should be removed; output=%s", alias, got)
				}
			}
		})
	}
}

func TestNormalizeAntigravitySchemasConsolidatesGenerationConfigSchemaAliases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		input      string
		wantSource string
	}{
		{
			name: "canonical and two aliases coexist",
			input: `{"generationConfig":{
				"responseSchema":{"type":"object","properties":{"canonical":{"type":"string"}}},
				"responseJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"response_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}}`,
			wantSource: "canonical",
		},
		{
			name: "two aliases coexist",
			input: `{"generationConfig":{
				"responseJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"response_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}}`,
			wantSource: "camel",
		},
		{
			name: "generation_config alias carries schema aliases",
			input: `{"generation_config":{
				"responseJsonSchema":{"type":"object","properties":{"camel":{"type":"string"}}},
				"response_json_schema":{"type":"object","properties":{"snake":{"type":"string"}}}
			}}`,
			wantSource: "camel",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeAntigravitySchemas([]byte(tt.input), antigravitySchemaTestModel)
			if !gjson.GetBytes(got, "generationConfig.responseSchema").IsObject() {
				t.Fatalf("generationConfig.responseSchema missing: %s", got)
			}
			if gotProp := gjson.GetBytes(got, "generationConfig.responseSchema.properties."+tt.wantSource+".type").String(); gotProp != "string" {
				t.Fatalf("generationConfig.responseSchema.properties.%s.type = %q, want string; output=%s", tt.wantSource, gotProp, got)
			}
			for _, alias := range []string{"responseJsonSchema", "response_schema", "response_json_schema"} {
				if gjson.GetBytes(got, "generationConfig."+alias).Exists() {
					t.Fatalf("generationConfig.%s should be removed; output=%s", alias, got)
				}
			}
			if gjson.GetBytes(got, "generation_config").Exists() {
				t.Fatalf("generation_config parent should be removed; output=%s", got)
			}
		})
	}
}

func TestNormalizeAntigravitySchemasFoldsGenerationConfigMixedFields(t *testing.T) {
	t.Parallel()

	input := `{"generation_config":{
		"temperature":0.2,
		"response_json_schema":{"type":"object","properties":{"field":{"type":"string"}}}
	}}`
	got := normalizeAntigravitySchemas([]byte(input), antigravitySchemaTestModel)

	if gjson.GetBytes(got, "generation_config").Exists() {
		t.Fatalf("generation_config parent should be removed; output=%s", got)
	}
	if !gjson.GetBytes(got, "generationConfig").IsObject() {
		t.Fatalf("generationConfig missing: %s", got)
	}
	if gotTemp := gjson.GetBytes(got, "generationConfig.temperature").Float(); gotTemp != 0.2 {
		t.Fatalf("generationConfig.temperature = %v, want 0.2; output=%s", gotTemp, got)
	}
	if !gjson.GetBytes(got, "generationConfig.responseSchema").IsObject() {
		t.Fatalf("generationConfig.responseSchema missing: %s", got)
	}
	if gotProp := gjson.GetBytes(got, "generationConfig.responseSchema.properties.field.type").String(); gotProp != "string" {
		t.Fatalf("generationConfig.responseSchema.properties.field.type = %q, want string; output=%s", gotProp, got)
	}
	for _, alias := range []string{"responseJsonSchema", "response_schema", "response_json_schema"} {
		if gjson.GetBytes(got, "generationConfig."+alias).Exists() {
			t.Fatalf("generationConfig.%s should be removed; output=%s", alias, got)
		}
	}
}

/**
 * @brief 验证原生 Gemini 和 Claude JSON Schema 工具在出站时分别保留可表示的数组约束。
 * @param t 测试上下文。
 */
func TestPrepareAntigravityRequestBodyArraySchemaCarriers(t *testing.T) {
	t.Parallel()
	cfg := &model.Config{AuthType: model.AuthTypeAntigravityOAuth, AntigravityProjectID: "gravity-project"}
	const parameters = `{"type":"object","properties":{"enum":{"type":"string"},"tuple":{"type":"array","items":[{"type":"string"},{"type":"integer"}]},"closed":{"type":"array","prefixItems":[{"type":"boolean"}],"items":false},"open":{"type":"array","items":true}},"required":["enum","tuple","closed","open"]}`

	t.Run("Gemini native schema", func(t *testing.T) {
		t.Parallel()
		source := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parametersJsonSchema":` + parameters + `,"response":{"type":"array","items":false}}]}],"generationConfig":{"responseMimeType":"application/json","responseSchema":{"type":"array","items":false}}}`)
		wire, err := prepareAntigravityRequestBody(cfg, antigravitySchemaTestModel, source, source, http.Header{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		request := gjson.GetBytes(wire, "request")
		tool := request.Get("tools.0.functionDeclarations.0")
		if tool.Get("parameters.properties.enum.type").String() != "string" || tool.Get("parameters.required.0").String() != "enum" {
			t.Fatalf("native tool lost the required enum parameter: %s", tool.Raw)
		}
		items := tool.Get("parameters.properties.tuple.items")
		if !items.IsObject() || items.Get("anyOf.0.type").String() != "string" || items.Get("anyOf.1.type").String() != "integer" {
			t.Fatalf("native tool items should use a Schema object with both branches: %s", tool.Raw)
		}
		for _, schema := range []gjson.Result{tool.Get("response"), request.Get("generationConfig.responseSchema")} {
			if !schema.Get("items").IsObject() || schema.Get("description").String() != "No items allowed" {
				t.Fatalf("native response items should use a Schema object with a hint: %s", schema.Raw)
			}
		}
	})

	for _, tc := range []struct {
		target    string
		carrier   string
		tuplePath string
	}{
		{antigravitySchemaTestModel, "parameters", "properties.tuple.items.anyOf"},
		{"claude-sonnet-4-6", "parametersJsonSchema", "properties.tuple.items"},
	} {
		t.Run("Anthropic to "+tc.target, func(t *testing.T) {
			t.Parallel()
			source := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"lookup","input_schema":` + parameters + `}]}`)
			translated, err := translateAntigravityRequest(protocol.Anthropic, tc.target, source, false)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := prepareAntigravityRequestBody(cfg, tc.target, translated, source, http.Header{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			schema := gjson.GetBytes(wire, "request.tools.0.functionDeclarations.0."+tc.carrier)
			if schema.Get("properties.enum.type").String() != "string" || schema.Get("required.0").String() != "enum" {
				t.Fatalf("schema lost the required enum parameter: %s", schema.Raw)
			}
			tuple := schema.Get(tc.tuplePath)
			if !tuple.IsArray() || tuple.Get("0.type").String() != "string" || tuple.Get("1.type").String() != "integer" {
				t.Fatalf("schema lost the tuple branches: %s", schema.Raw)
			}
			open := schema.Get("properties.open.items")
			if !open.IsObject() || len(open.Map()) != 0 {
				t.Fatalf("open items should remain unrestricted: %s", schema.Raw)
			}
			closed := schema.Get("properties.closed")
			if tc.carrier == "parametersJsonSchema" {
				if closed.Get("items").Type != gjson.False || closed.Get("prefixItems.0.type").String() != "boolean" {
					t.Fatalf("JSON Schema lost the closed tuple: %s", schema.Raw)
				}
			} else if !closed.Get("items").IsObject() || closed.Get("description").String() != "No additional items allowed" {
				t.Fatalf("native schema should project the closed tuple to a Schema object and hint: %s", schema.Raw)
			}
		})
	}
}

// Gemini 的 promptTokenCount 与 totalTokenCount 都**含**缓存命中部分，而 Anthropic 的
// input_tokens 语义是**未命中缓存**的输入，cache_read 与它相加才是总输入。两边口径不同，
// 转换时必须减掉 cached，否则缓存部分被按全价重复计费、缓存命中率也被腰斩。
//
// 取值来自一次真实的 Antigravity 响应：prompt 62277 / cached 62181 / candidates 248，
// 正确的 input_tokens 是 96。上游 CLIProxyAPI 只在流式路径做了这个减法，
// 偏差登记见 protocol/cliproxy/UPSTREAM.md。
const (
	antigravityUsagePromptTokens = 62277
	antigravityUsageCachedTokens = 62181
	antigravityUsageInputTokens  = antigravityUsagePromptTokens - antigravityUsageCachedTokens

	// candidatesTokenCount 缺失，强制走 totalTokenCount 回退分支。
	antigravityUsageNoCandidates = `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":62277,"totalTokenCount":62525,"cachedContentTokenCount":62181},"modelVersion":"claude-sonnet-4-6","responseId":"msg_vrtx_1"}}`
)

func TestTranslateAntigravityResponseNonStreamSubtractsCachedTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        string
		wantOutput int64
	}{
		{
			name:       "candidatesTokenCount present",
			raw:        `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":62277,"candidatesTokenCount":248,"totalTokenCount":62525,"cachedContentTokenCount":62181},"modelVersion":"claude-sonnet-4-6","responseId":"msg_vrtx_1"}}`,
			wantOutput: 248,
		},
		{
			// 回退时 prompt 必须按含 cached 的原值参与减法，否则缓存命中的
			// 输入会被整段算成输出、按输出价计费。
			name:       "candidatesTokenCount missing falls back to totalTokenCount",
			raw:        antigravityUsageNoCandidates,
			wantOutput: 248,
		},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			got, err := translateAntigravityResponseNonStream(
				t.Context(), protocol.Anthropic, "claude-sonnet-4-6",
				nil, []byte(`{"model":"claude-sonnet-4-6"}`), []byte(testCase.raw),
			)
			if err != nil {
				t.Fatal(err)
			}
			assertAntigravityClaudeUsage(t, gjson.GetBytes(got, "usage"), testCase.wantOutput)
		})
	}
}

// 流式终止事件必须给出与非流式一致的 usage 口径。
func TestTranslateAntigravityResponseStreamUsageMatchesNonStream(t *testing.T) {
	t.Parallel()

	state := any(nil)
	chunks, err := translateAntigravityResponseStream(
		t.Context(), protocol.Anthropic, "claude-sonnet-4-6",
		nil, []byte(`{"model":"claude-sonnet-4-6"}`), []byte(antigravityUsageNoCandidates), &state,
	)
	if err != nil {
		t.Fatal(err)
	}

	stream := string(bytes.Join(chunks, nil))
	delta := ""
	for line := range strings.SplitSeq(stream, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if ok && gjson.Get(payload, "type").String() == "message_delta" {
			delta = payload
		}
	}
	if delta == "" {
		t.Fatalf("未找到 message_delta 事件:\n%s", stream)
	}
	assertAntigravityClaudeUsage(t, gjson.Get(delta, "usage"), 248)
}

func assertAntigravityClaudeUsage(t *testing.T, usage gjson.Result, wantOutput int64) {
	t.Helper()
	if got := usage.Get("input_tokens").Int(); got != antigravityUsageInputTokens {
		t.Errorf("input_tokens = %d, want %d (promptTokenCount 未减去 cachedContentTokenCount)",
			got, antigravityUsageInputTokens)
	}
	if got := usage.Get("cache_read_input_tokens").Int(); got != antigravityUsageCachedTokens {
		t.Errorf("cache_read_input_tokens = %d, want %d", got, antigravityUsageCachedTokens)
	}
	if got := usage.Get("output_tokens").Int(); got != wantOutput {
		t.Errorf("output_tokens = %d, want %d (回退计算误把缓存输入算成输出)", got, wantOutput)
	}
}

func TestConvertAntigravityResponseToClaude_ImplicitOpenToolContinuationOrdering(t *testing.T) {
	origRequest := []byte(`{"model":"claude-sonnet-4-6","tools":[{"name":"echo","input_schema":{"type":"object","properties":{}}}]}`)
	translatedRequest := []byte(`{"model":"gemini-3.8-flash-high"}`)

	for _, tc := range []struct {
		name      string
		firstArgs string
		contArgs  string
		want      string
	}{
		{"empty_then_real_args", `{}`, `{"user":"v"}`, `{"user":"v"}`},
		{"placeholder_then_real_args", `{"reason":"thinking"}`, `{"user":"v"}`, `{"user":"v"}`},
		{"real_args_then_empty", `{"user":"v"}`, `{}`, `{"user":"v"}`},
		{"real_args_then_placeholder", `{"user":"v"}`, `{"reason":"done"}`, `{"user":"v"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			firstChunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"echo","args":` + tc.firstArgs + `}}]}}],"modelVersion":"gemini-3.8-flash-high","responseId":"resp-1"}}`)
			continuationChunk := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"args":` + tc.contArgs + `}}]},"finishReason":"STOP"}],"modelVersion":"gemini-3.8-flash-high","responseId":"resp-1"}}`)

			var param any
			streamBytes := bytes.Join(antigravityclaude.ConvertAntigravityResponseToClaude(context.Background(), "claude-sonnet-4-6", origRequest, translatedRequest, firstChunk, &param), nil)
			streamBytes = append(streamBytes, bytes.Join(antigravityclaude.ConvertAntigravityResponseToClaude(context.Background(), "claude-sonnet-4-6", origRequest, translatedRequest, continuationChunk, &param), nil)...)
			streamText := string(streamBytes)
			var deltaPartial strings.Builder
			for line := range strings.SplitSeq(streamText, "\n") {
				if after, ok := strings.CutPrefix(line, "data: "); ok {
					payload := after
					if gjson.Get(payload, "delta.type").String() == "input_json_delta" {
						deltaPartial.WriteString(gjson.Get(payload, "delta.partial_json").String())
					}
				}
			}
			if got := deltaPartial.String(); got != tc.want {
				t.Fatalf("expected concatenated partial_json %s, got %q (full output: %s)", tc.want, got, streamText)
			}
		})
	}
}
