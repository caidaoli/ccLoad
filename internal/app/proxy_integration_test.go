package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/anthropicauth"
	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/codexauth"
	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	sigcompat "ccLoad/internal/protocol/cliproxy/signature"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"
	"ccLoad/internal/xaiauth"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// ============================================================================
// 代理转发集成测试
// 端到端验证：上游模拟 → Server → gin 路由 → 请求转发 → 响应返回
// ============================================================================

// testChannel 测试用渠道定义
type testChannel struct {
	name                    string
	upstreamProtocol        string
	protocolTransformMode   string
	websockets              bool
	customRequestRules      *model.CustomRequestRules
	cooldownDetectionRules  *model.CooldownDetectionRules
	retryOtherKeysOnFailure bool
	models                  string // 逗号分隔的模型列表
	apiKey                  string
	authType                string
	oauthCredential         string
	priority                int
}

// proxyTestEnv 集成测试环境
type proxyTestEnv struct {
	server *Server
	store  storage.Store
	engine *gin.Engine
}

func TestProxy_SingleURLRecordsRuntimeStats(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "single-url-stats", upstreamProtocol: "openai", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-test",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs: configs=%d err=%v", len(configs), err)
	}
	stats := env.server.urlSelector.GetURLStats(configs[0].ID, configs[0].GetURLs())
	if len(stats) != 1 || stats[0].Requests != 1 || stats[0].LatencyMs <= 0 {
		t.Fatalf("unexpected single URL runtime stats: %+v", stats)
	}
}

func TestProxy_NonResponsesGenerateFieldIsPreserved(t *testing.T) {
	requestBody := make(chan []byte, 1)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestBody <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()
	env := setupProxyTestEnv(t, []testChannel{{
		name: "preserve-generate", upstreamProtocol: "openai", models: "gpt-test", priority: 100,
	}}, map[int]string{0: upstream.URL})

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "gpt-test", "generate": true,
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("non-Responses request status=%d body=%s", response.Code, response.Body.String())
	}
	if !gjson.GetBytes(<-requestBody, "generate").Bool() {
		t.Fatal("non-Responses generate field was stripped before reaching upstream")
	}
}

// setupProxyTestEnv 创建指向 mockUpstream 的完整测试 Server
// 每个渠道的 URL 使用 upstreamURLs map（channelIndex → upstreamURL）
func setupProxyTestEnv(t testing.TB, channels []testChannel, upstreamURLs map[int]string) *proxyTestEnv {
	return setupProxyTestEnvWithSettings(t, channels, upstreamURLs, nil)
}

func setupProxyTestEnvWithSettings(
	t testing.TB,
	channels []testChannel,
	upstreamURLs map[int]string,
	settings map[string]string,
) *proxyTestEnv {
	t.Helper()

	srv := newInMemoryServerWithSettings(t, settings)
	store := srv.store

	ctx := context.Background()

	// 创建渠道和 API Key
	for i, ch := range channels {
		upURL := upstreamURLs[i]
		if upURL == "" {
			t.Fatalf("missing upstream URL for channel %d", i)
		}

		priority := ch.priority
		if priority == 0 {
			priority = 100 - i*10 // 按顺序递减优先级
		}

		upstreamProtocol := ch.upstreamProtocol
		if upstreamProtocol == "" {
			upstreamProtocol = util.ProtocolOpenAI
		}
		transformMode := ch.protocolTransformMode
		if transformMode == "" {
			transformMode = model.ProtocolTransformModeLocal
		}

		// 构建模型列表
		var modelEntries []model.ModelEntry
		for _, m := range strings.Split(ch.models, ",") {
			m = strings.TrimSpace(m)
			if m != "" {
				modelEntries = append(modelEntries, model.ModelEntry{Model: m})
			}
		}

		urls := channelURLsForTest(upURL)
		if transformMode == model.ProtocolTransformModeLocal {
			for urlIndex := range urls {
				urls[urlIndex].Protocols = []string{upstreamProtocol}
			}
		}
		cfg := &model.Config{
			Name:                    ch.name,
			AuthType:                ch.authType,
			OAuthCredential:         ch.oauthCredential,
			URLs:                    urls,
			Websockets:              ch.websockets,
			ProtocolTransformMode:   transformMode,
			CustomRequestRules:      ch.customRequestRules,
			CooldownDetectionRules:  ch.cooldownDetectionRules,
			RetryOtherKeysOnFailure: ch.retryOtherKeysOnFailure,
			Priority:                priority,
			Enabled:                 true,
			ModelEntries:            modelEntries,
		}
		created, err := store.CreateConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("CreateConfig for %s: %v", ch.name, err)
		}

		if created.UsesOAuth() {
			continue
		}

		// 创建 API Key
		apiKey := ch.apiKey
		if apiKey == "" {
			apiKey = fmt.Sprintf("sk-test-%d", i)
		}
		err = store.CreateAPIKeysBatch(ctx, []*model.APIKey{
			{ChannelID: created.ID, KeyIndex: 0, APIKey: apiKey},
		})
		if err != nil {
			t.Fatalf("CreateAPIKeysBatch for %s: %v", ch.name, err)
		}
	}

	injectAPIToken(srv.authService, "test-api-key", 0, 1)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	srv.SetupRoutes(engine)

	return &proxyTestEnv{
		server: srv,
		store:  store,
		engine: engine,
	}
}

func codexProxyTestCredential(t testing.TB, accessToken, refreshToken, accountID string) string {
	t.Helper()
	credential := &codexauth.Credential{
		Type: codexauth.ChannelType, AccessToken: accessToken, RefreshToken: refreshToken,
		AccountID: accountID, Expired: time.Now().UTC().Add(10 * 24 * time.Hour).Format(time.RFC3339),
	}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatalf("Codex credential JSON: %v", err)
	}
	return payload
}

func antigravityProxyTestCredential(t testing.TB, accessToken string) string {
	t.Helper()
	credential := &antigravityauth.Credential{
		Type: antigravityauth.ChannelType, AccessToken: accessToken, RefreshToken: "rt-antigravity",
		Expired: time.Now().UTC().Add(10 * 24 * time.Hour).Format(time.RFC3339),
		Email:   "gravity@example.com", ProjectID: "gravity-project",
	}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatalf("Antigravity credential JSON: %v", err)
	}
	return payload
}

func anthropicProxyTestCredential(t testing.TB, accessToken string) string {
	t.Helper()
	credential := &anthropicauth.Credential{
		Type: anthropicauth.ChannelType, AccessToken: accessToken, RefreshToken: "rt-anthropic",
		Expired:     time.Now().UTC().Add(10 * 24 * time.Hour).Format(time.RFC3339),
		AccountUUID: "account-anthropic", DeviceID: "device-anthropic",
	}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatalf("Anthropic credential JSON: %v", err)
	}
	return payload
}

func TestProxy_OAuthBaseURLSettingsOverrideChannelURLs(t *testing.T) {
	t.Run("API key channel is unaffected", func(t *testing.T) {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chat-api-key","choices":[{"message":{"role":"assistant","content":"channel url"}}]}`)
		}))
		defer upstream.Close()

		env := setupProxyTestEnvWithSettings(t, []testChannel{{
			name: "api-key-unaffected", upstreamProtocol: "openai", models: "gpt-test",
		}}, map[int]string{0: upstream.URL}, map[string]string{
			"CODEX_BASE_URL": "http://127.0.0.1:1/ignored",
		})

		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}, nil)
		if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "choices.0.message.content").String() != "channel url" {
			t.Fatalf("API key channel response=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("Codex", func(t *testing.T) {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/codex/responses" {
				t.Errorf("Codex override path = %q, want /codex/responses", r.URL.Path)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-codex-override","status":"completed","output":[]}}`+"\n\n")
		}))
		defer upstream.Close()

		env := setupProxyTestEnvWithSettings(t, []testChannel{{
			name: "codex-oauth-override", upstreamProtocol: "codex", models: "gpt-test",
			authType:        model.AuthTypeCodexOAuth,
			oauthCredential: codexProxyTestCredential(t, "codex-override-token", "refresh", "account"),
		}}, map[int]string{0: "http://127.0.0.1:1/ignored#"}, map[string]string{
			"CODEX_BASE_URL": upstream.URL + "/codex/responses",
		})

		response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
			"model": "gpt-test", "stream": false, "input": "hello",
		}, nil)
		if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "id").String() != "resp-codex-override" {
			t.Fatalf("Codex override response=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("xAI", func(t *testing.T) {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/responses" {
				t.Errorf("xAI override path = %q, want /v1/responses", r.URL.Path)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-xai-override","status":"completed","output":[]}}`+"\n\n")
		}))
		defer upstream.Close()

		credential := mustXAICredentialJSON(t, &xaiauth.Credential{
			Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "xai-override-token",
			RefreshToken: "refresh", Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
		env := setupProxyTestEnvWithSettings(t, []testChannel{{
			name: "xai-oauth-override", upstreamProtocol: "codex", models: "grok-4.5",
			authType: model.AuthTypeXAIOAuth, oauthCredential: credential,
		}}, map[int]string{0: "http://127.0.0.1:1/ignored"}, map[string]string{
			"XAI_BASE_URL": upstream.URL + "/v1",
		})

		response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
			"model": "grok-4.5", "stream": false, "input": "hello",
		}, nil)
		if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "id").String() != "resp-xai-override" {
			t.Fatalf("xAI override response=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("Anthropic", func(t *testing.T) {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/anthropic/v1/messages" || r.URL.Query().Get("beta") != "true" {
				t.Errorf("Anthropic override URL = %s?%s", r.URL.Path, r.URL.RawQuery)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer anthropic-override-token" {
				t.Errorf("Authorization = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg-override","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"override ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		}))
		defer upstream.Close()

		env := setupProxyTestEnvWithSettings(t, []testChannel{{
			name: "anthropic-oauth-override", upstreamProtocol: "anthropic", models: "claude-test",
			authType:        model.AuthTypeAnthropicOAuth,
			oauthCredential: anthropicProxyTestCredential(t, "anthropic-override-token"),
		}}, map[int]string{0: "http://127.0.0.1:1/ignored"}, map[string]string{
			"ANTHROPIC_BASE_URL": upstream.URL + "/anthropic",
		})

		response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
			"model": "claude-test", "max_tokens": 32,
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}, nil)
		if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "content.0.text").String() != "override ok" {
			t.Fatalf("Anthropic override response=%d body=%s", response.Code, response.Body.String())
		}
	})

	t.Run("Antigravity", func(t *testing.T) {
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1internal:generateContent" {
				t.Errorf("Antigravity override path = %q, want /v1internal:generateContent", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"override ok"}]},"finishReason":"STOP"}]}}`)
		}))
		defer upstream.Close()

		env := setupProxyTestEnvWithSettings(t, []testChannel{{
			name: "antigravity-oauth-override", upstreamProtocol: "gemini", models: "gemini-3-flash",
			authType:        model.AuthTypeAntigravityOAuth,
			oauthCredential: antigravityProxyTestCredential(t, "antigravity-override-token"),
		}}, map[int]string{0: "http://127.0.0.1:1/ignored"}, map[string]string{
			"ANTIGRAVITY_URL": upstream.URL,
		})

		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model":    "gemini-3-flash",
			"messages": []any{map[string]any{"role": "user", "content": "hello"}},
		}, nil)
		if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "choices.0.message.content").String() != "override ok" {
			t.Fatalf("Antigravity override response=%d body=%s", response.Code, response.Body.String())
		}
	})
}

func TestProxy_AntigravityOAuthWrapsGeminiWireAndTranslatesOpenAIResponse(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:generateContent" || r.URL.RawQuery != "" {
			t.Errorf("Antigravity URL = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer at-antigravity" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != antigravityauth.DefaultUserAgent {
			t.Errorf("User-Agent = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		for _, name := range []string{
			"Accept", "Accept-Language", "HTTP-Referer", "Sec-CH-UA", "Sec-CH-UA-Mobile",
			"Sec-CH-UA-Platform", "Sec-Fetch-Dest", "Sec-Fetch-Mode", "Sec-Fetch-Site", "X-Title",
		} {
			if got := r.Header.Get(name); got != "" {
				t.Errorf("unrelated Antigravity header %s = %q", name, got)
			}
		}
		wireBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read Antigravity wire body: %v", err)
		}
		var envelope struct {
			Project     string `json:"project"`
			Model       string `json:"model"`
			UserAgent   string `json:"userAgent"`
			RequestType string `json:"requestType"`
			Request     struct {
				SystemInstruction struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"systemInstruction"`
				Contents []struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"contents"`
			} `json:"request"`
		}
		if err := json.Unmarshal(wireBody, &envelope); err != nil {
			t.Fatalf("decode Antigravity envelope: %v body=%s", err, wireBody)
		}
		if envelope.Project != "gravity-project" || envelope.Model != "gemini-3-flash" || envelope.UserAgent != "antigravity" || envelope.RequestType != "agent" {
			t.Errorf("unexpected Antigravity envelope: %+v", envelope)
		}
		if got := envelope.Request.SystemInstruction.Parts[0].Text; !strings.Contains(got, "You are Antigravity") {
			t.Errorf("identity prompt missing: %q", got)
		}
		if got := envelope.Request.SystemInstruction.Parts[1].Text; got != "You are A\u200BPI p\u200Broxy C\u200Blaude A\u200Bnthropic assistant" {
			t.Errorf("user system prompt = %q", got)
		}
		if got := envelope.Request.Contents[0].Parts[0].Text; got != "mention API proxy Claude Anthropic unchanged" {
			t.Errorf("user content was modified: %q", got)
		}
		if gjson.GetBytes(wireBody, "request.tools.0.functionDeclarations.0.parameters.type").String() != "object" ||
			gjson.GetBytes(wireBody, "request.tools.0.functionDeclarations.0.parametersJsonSchema").Exists() {
			t.Errorf("Antigravity tool schema was not finalized: %s", wireBody)
		}
		if gjson.GetBytes(wireBody, "request.generationConfig.maxOutputTokens").Exists() {
			t.Errorf("Gemini Antigravity request retained maxOutputTokens: %s", wireBody)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"gravity ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5},"modelVersion":"gemini-3-flash"}}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnvWithSettings(t, []testChannel{{
		name: "antigravity-openai", upstreamProtocol: "gemini", models: "gemini-3-flash", priority: 100,
		authType: model.AuthTypeAntigravityOAuth, oauthCredential: antigravityProxyTestCredential(t, "at-antigravity"),
	}}, map[int]string{0: upstream.URL}, nil)

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "gemini-3-flash", "max_tokens": 100,
		"messages": []map[string]string{
			{"role": "system", "content": "You are API proxy Claude Anthropic assistant"},
			{"role": "user", "content": "mention API proxy Claude Anthropic unchanged"},
		},
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": "lookup", "description": "lookup data",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []string{"query"}},
			},
		}},
	}, map[string]string{
		"Accept":             "text/event-stream",
		"Accept-Language":    "zh-CN",
		"HTTP-Referer":       "https://cherry-ai.com",
		"Sec-CH-UA":          `"Chromium";v="146"`,
		"Sec-CH-UA-Mobile":   "?0",
		"Sec-CH-UA-Platform": `"macOS"`,
		"Sec-Fetch-Dest":     "empty",
		"Sec-Fetch-Mode":     "cors",
		"Sec-Fetch-Site":     "cross-site",
		"X-Title":            "Cherry Studio",
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := gjson.Get(response.Body.String(), "choices.0.message.content").String(); got != "gravity ok" {
		t.Fatalf("OpenAI response content=%q body=%s", got, response.Body.String())
	}
}

func TestProxy_AntigravityOAuthHandlesCountTokensLocally(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "antigravity-count-tokens", upstreamProtocol: "gemini", models: "gemini-3-flash", priority: 100,
		authType: model.AuthTypeAntigravityOAuth, oauthCredential: antigravityProxyTestCredential(t, "at-count"),
	}}, map[int]string{0: upstream.URL})

	response := doProxyRequest(t, env.engine, "/v1beta/models/gemini-3-flash:countTokens", map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}},
	}, nil)
	if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "totalTokens").Int() != 0 {
		t.Fatalf("countTokens response=%d body=%s", response.Code, response.Body.String())
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("countTokens reached upstream %d times", upstreamCalls.Load())
	}
}

func TestProxy_AntigravityOAuthUsesWebSearchWireContract(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wireBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read Antigravity Web Search request: %v", err)
		}
		if got := gjson.GetBytes(wireBody, "requestType").String(); got != "web_search" {
			t.Errorf("requestType=%q body=%s", got, wireBody)
		}
		if got := gjson.GetBytes(wireBody, "model").String(); got != antigravityWebSearchFallbackModel {
			t.Errorf("model=%q body=%s", got, wireBody)
		}
		if got := gjson.GetBytes(wireBody, "request.systemInstruction.parts.0.text").String(); !strings.Contains(got, "You are Antigravity") {
			t.Errorf("identity prompt missing: %q", got)
		}
		if got := gjson.GetBytes(wireBody, "request.sessionId").String(); got == "" {
			t.Errorf("Web Search sessionId missing: %s", wireBody)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"search ok"}]},"finishReason":"STOP"}]}}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "antigravity-web-search", upstreamProtocol: "gemini", models: "claude-sonnet-4-6", priority: 100,
		authType: model.AuthTypeAntigravityOAuth, oauthCredential: antigravityProxyTestCredential(t, "at-search"),
	}}, map[int]string{0: upstream.URL})

	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "claude-sonnet-4-6", "max_tokens": 100,
		"messages": []any{map[string]any{"role": "user", "content": "find current docs"}},
		"tools":    []any{map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 5}},
	}, nil)
	if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "content.0.text").String() != "search ok" {
		t.Fatalf("Web Search response=%d body=%s", response.Code, response.Body.String())
	}
}

func TestProxy_AntigravityOAuthRetriesCorruptedToolSignature(t *testing.T) {
	var attempts atomic.Int32
	var bodies [][]byte
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read Antigravity signature request: %v", err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":400,"message":"Corrupted thought signature."}}`)
			return
		}
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"retry ok"}]},"finishReason":"STOP"}]}}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "antigravity-signature", upstreamProtocol: "gemini", models: "gemini-3-flash", priority: 100,
		authType: model.AuthTypeAntigravityOAuth, oauthCredential: antigravityProxyTestCredential(t, "at-signature"),
	}}, map[int]string{0: upstream.URL})

	response := doProxyRequest(t, env.engine, "/v1beta/models/gemini-3-flash:generateContent", map[string]any{
		"systemInstruction": map[string]any{"parts": []any{map[string]any{"text": "API proxy"}}},
		"contents": []any{
			map[string]any{"role": "model", "parts": []any{map[string]any{
				"functionCall":     map[string]any{"name": "lookup", "args": map[string]any{"q": "x"}},
				"thoughtSignature": "stale-signature",
			}}},
			map[string]any{"role": "user", "parts": []any{map[string]any{
				"functionResponse": map[string]any{"name": "lookup", "response": map[string]any{"result": "x"}},
			}}},
		},
	}, nil)
	if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "candidates.0.content.parts.0.text").String() != "retry ok" {
		t.Fatalf("signature retry response=%d body=%s", response.Code, response.Body.String())
	}
	if attempts.Load() != 2 || len(bodies) != 2 {
		t.Fatalf("signature attempts=%d bodies=%d", attempts.Load(), len(bodies))
	}
	if got := gjson.GetBytes(bodies[0], "request.contents.0.parts.0.thoughtSignature").String(); got != "stale-signature" {
		t.Fatalf("first signature=%q body=%s", got, bodies[0])
	}
	if got := gjson.GetBytes(bodies[1], "request.contents.0.parts.0.thoughtSignature").String(); got != sigcompat.GeminiSkipThoughtSignatureValidator {
		t.Fatalf("retry signature=%q body=%s", got, bodies[1])
	}
	if got := gjson.GetBytes(bodies[1], "request.systemInstruction.parts.#").Int(); got != 2 {
		t.Fatalf("identity prompt duplicated on retry: parts=%d body=%s", got, bodies[1])
	}
	if got := gjson.GetBytes(bodies[1], "request.systemInstruction.parts.1.text").String(); got != "A\u200BPI p\u200Broxy" {
		t.Fatalf("zero-width obfuscation was not preserved: %q", got)
	}
}

func TestProxy_AntigravityOAuthDegradesClaudeSignatureHistoryInStages(t *testing.T) {
	var attempts atomic.Int32
	var bodies [][]byte
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read Antigravity Claude signature request: %v", err)
		}
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		if attempts.Add(1) <= 2 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":400,"message":"Expected thinking block with a valid signature."}}`)
			return
		}
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"claude retry ok"}]},"finishReason":"STOP"}]}}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "antigravity-claude-signature", upstreamProtocol: "gemini", models: "claude-sonnet-4-6", priority: 100,
		authType: model.AuthTypeAntigravityOAuth, oauthCredential: antigravityProxyTestCredential(t, "at-claude-signature"),
	}}, map[int]string{0: upstream.URL})

	response := doProxyRequest(t, env.engine, "/v1beta/models/claude-sonnet-4-6:generateContent", map[string]any{
		"contents": []any{
			map[string]any{"role": "model", "parts": []any{
				map[string]any{"text": "old reasoning", "thought": true, "thoughtSignature": "bad-thinking"},
				map[string]any{
					"functionCall":     map[string]any{"name": "lookup", "args": map[string]any{"q": "x"}},
					"thoughtSignature": "bad-tool",
				},
			}},
			map[string]any{"role": "user", "parts": []any{map[string]any{
				"functionResponse": map[string]any{"name": "lookup", "response": map[string]any{"result": "x"}},
			}}},
		},
		"generationConfig": map[string]any{"thinkingConfig": map[string]any{"includeThoughts": true}},
	}, nil)
	if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "candidates.0.content.parts.0.text").String() != "claude retry ok" {
		t.Fatalf("Claude signature retry response=%d body=%s", response.Code, response.Body.String())
	}
	if attempts.Load() != 3 || len(bodies) != 3 {
		t.Fatalf("Claude signature attempts=%d bodies=%d", attempts.Load(), len(bodies))
	}
	if !gjson.GetBytes(bodies[0], "request.contents.0.parts.0.thought").Bool() {
		t.Fatalf("first request lost thinking history: %s", bodies[0])
	}
	if gjson.GetBytes(bodies[1], "request.contents.0.parts.0.thought").Exists() ||
		gjson.GetBytes(bodies[1], "request.generationConfig.thinkingConfig").Exists() {
		t.Fatalf("thinking-only retry retained thinking state: %s", bodies[1])
	}
	if !gjson.GetBytes(bodies[1], "request.contents.0.parts.1.functionCall").Exists() {
		t.Fatalf("thinking-only retry removed tool history too early: %s", bodies[1])
	}
	if bytes.Contains(bodies[2], []byte(`"functionCall"`)) || bytes.Contains(bodies[2], []byte(`"functionResponse"`)) {
		t.Fatalf("tool retry retained signature-sensitive tool history: %s", bodies[2])
	}
	if !bytes.Contains(bodies[2], []byte(`(tool_use)`)) || !bytes.Contains(bodies[2], []byte(`(tool_result)`)) {
		t.Fatalf("tool retry did not preserve history as text: %s", bodies[2])
	}
}

func TestProxy_AntigravityOAuthClampsAnthropicThinkingLevelOnWire(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wireBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read Antigravity wire body: %v", err)
		}
		if got := gjson.GetBytes(wireBody, "request.generationConfig.thinkingConfig.thinkingLevel").String(); got != "low" {
			t.Errorf("Antigravity thinkingLevel=%q, want low; body=%s", got, wireBody)
		}
		if !gjson.GetBytes(wireBody, "request.generationConfig.thinkingConfig.includeThoughts").Bool() {
			t.Errorf("Antigravity includeThoughts=false; body=%s", wireBody)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"gravity thinking ok"}]},"finishReason":"STOP"}]}}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "antigravity-anthropic-thinking", upstreamProtocol: "gemini", models: "gemini-3.1-pro-low", priority: 100,
		authType: model.AuthTypeAntigravityOAuth, oauthCredential: antigravityProxyTestCredential(t, "at-thinking"),
	}}, map[int]string{0: upstream.URL})

	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "gemini-3.1-pro-low", "max_tokens": 100,
		"messages":      []any{map[string]any{"role": "user", "content": "think"}},
		"thinking":      map[string]any{"type": "adaptive"},
		"output_config": map[string]any{"effort": "minimal"},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := gjson.Get(response.Body.String(), "content.0.text").String(); got != "gravity thinking ok" {
		t.Fatalf("Anthropic response content=%q body=%s", got, response.Body.String())
	}
}

func TestProxy_AntigravityOAuthBaseURLFallbackConditions(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		body         string
		networkError bool
		wantFallback bool
	}{
		{name: "network error", networkError: true, wantFallback: true},
		{name: "404", status: http.StatusNotFound, body: `{"error":{"code":404,"message":"not found"}}`, wantFallback: true},
		{name: "429", status: http.StatusTooManyRequests, body: `{"error":{"code":429,"message":"rate limited"}}`, wantFallback: true},
		{name: "503 no capacity", status: http.StatusServiceUnavailable, body: `{"error":{"code":503,"message":"No capacity available for model claude-sonnet-4-6 on the server"}}`, wantFallback: true},
		{name: "400", status: http.StatusBadRequest, body: `{"error":{"code":400,"message":"bad request"}}`},
		{name: "405", status: http.StatusMethodNotAllowed, body: `{"error":{"code":405,"message":"method not allowed"}}`},
		{name: "500", status: http.StatusInternalServerError, body: `{"error":{"code":500,"message":"internal error"}}`},
		{name: "500 protocol conversion", status: http.StatusInternalServerError, body: `{"error":{"code":"convert_request_failed","message":"not implemented"}}`},
		{name: "502", status: http.StatusBadGateway, body: `{"error":{"code":502,"message":"bad gateway"}}`},
		{name: "ordinary 503", status: http.StatusServiceUnavailable, body: `{"error":{"code":503,"message":"service unavailable"}}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var firstCalls atomic.Int32
			var fallbackCalls atomic.Int32
			first := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				firstCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fallbackCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"fallback ok"}]},"finishReason":"STOP"}]}}`)
			}))

			firstURL := first.URL
			if tc.networkError {
				firstURL = "http://antigravity-network-failure.invalid"
			}
			env := setupProxyTestEnv(t, []testChannel{{
				name: "antigravity-fallback", upstreamProtocol: "gemini", models: "claude-sonnet-4-6", priority: 100,
				authType: model.AuthTypeAntigravityOAuth, oauthCredential: antigravityProxyTestCredential(t, "at-fallback"),
			}}, map[int]string{0: firstURL + "\n" + fallback.URL})

			configs, err := env.store.ListConfigs(context.Background())
			if err != nil || len(configs) != 1 {
				t.Fatalf("ListConfigs = (%d, %v)", len(configs), err)
			}
			// Make the pre-fix weighted selector deterministic. Antigravity fallback
			// order itself must ignore this runtime preference.
			env.server.urlSelector.CooldownURL(configs[0].ID, fallback.URL)
			if tc.networkError {
				env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Host == "antigravity-network-failure.invalid" {
						firstCalls.Add(1)
						return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
					}
					return dispatchTestHTTPRequest(req)
				})}
			}

			response := doProxyRequest(t, env.engine, "/v1beta/models/claude-sonnet-4-6:generateContent", map[string]any{
				"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}},
			}, nil)
			if firstCalls.Load() != 1 {
				t.Fatalf("first URL calls=%d, want 1", firstCalls.Load())
			}
			if tc.wantFallback {
				if response.Code != http.StatusOK || fallbackCalls.Load() != 1 ||
					gjson.Get(response.Body.String(), "candidates.0.content.parts.0.text").String() != "fallback ok" {
					t.Fatalf("fallback response=%d calls=%d body=%s", response.Code, fallbackCalls.Load(), response.Body.String())
				}
				return
			}
			if response.Code != tc.status || fallbackCalls.Load() != 0 {
				t.Fatalf("non-fallback response=%d fallback calls=%d body=%s", response.Code, fallbackCalls.Load(), response.Body.String())
			}
		})
	}
}

func TestProxy_AntigravityOAuthUsesDefaultBaseURLFallbackOrder(t *testing.T) {
	var mu sync.Mutex
	requestBaseURLs := make([]string, 0, 3)
	env := setupProxyTestEnv(t, []testChannel{{
		name: "antigravity-default-fallback", upstreamProtocol: "gemini", models: "claude-sonnet-4-6", priority: 100,
		authType: model.AuthTypeAntigravityOAuth, oauthCredential: antigravityProxyTestCredential(t, "at-default-fallback"),
	}}, map[int]string{0: antigravityDailyBaseURL + "\n" + antigravityProdBaseURL})
	env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		baseURL := req.URL.Scheme + "://" + req.URL.Host
		mu.Lock()
		requestBaseURLs = append(requestBaseURLs, baseURL)
		mu.Unlock()
		status := http.StatusOK
		body := `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"sandbox ok"}]},"finishReason":"STOP"}]}}`
		switch baseURL {
		case antigravityDailyBaseURL:
			status = http.StatusServiceUnavailable
			body = `{"error":{"code":503,"message":"No capacity available for model claude-sonnet-4-6 on the server"}}`
		case antigravityProdBaseURL:
			status = http.StatusTooManyRequests
			body = `{"error":{"code":429,"message":"rate limited"}}`
		case antigravitySandboxDailyBaseURLForTest:
		default:
			t.Fatalf("unexpected Antigravity base URL: %s", baseURL)
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}

	response := doProxyRequest(t, env.engine, "/v1beta/models/claude-sonnet-4-6:generateContent", map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}},
	}, nil)
	if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "candidates.0.content.parts.0.text").String() != "sandbox ok" {
		t.Fatalf("response=%d body=%s", response.Code, response.Body.String())
	}
	want := []string{antigravityDailyBaseURL, antigravityProdBaseURL, antigravitySandboxDailyBaseURLForTest}
	mu.Lock()
	got := append([]string(nil), requestBaseURLs...)
	mu.Unlock()
	if !slices.Equal(got, want) {
		t.Fatalf("Antigravity base URL order=%v, want %v", got, want)
	}
}

func TestProxy_AntigravityOAuthModelCapacityExhaustionBecomes429AfterThreeBaseURLs(t *testing.T) {
	var mu sync.Mutex
	requestBaseURLs := make([]string, 0, antigravityModelCapacityAttempts)
	requestTimes := make([]time.Time, 0, antigravityModelCapacityAttempts)
	env := setupProxyTestEnv(t, []testChannel{{
		name: "antigravity-capacity", upstreamProtocol: "gemini", models: "claude-opus-4-6-thinking", priority: 100,
		authType: model.AuthTypeAntigravityOAuth, oauthCredential: antigravityProxyTestCredential(t, "at-capacity"),
	}}, map[int]string{0: antigravityDailyBaseURL + "\n" + antigravityProdBaseURL})
	env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		requestBaseURLs = append(requestBaseURLs, req.URL.Scheme+"://"+req.URL.Host)
		requestTimes = append(requestTimes, time.Now())
		mu.Unlock()
		body := `{"error":{"code":503,"message":"No capacity available for model claude-opus-4-6-thinking on the server","status":"UNAVAILABLE","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"MODEL_CAPACITY_EXHAUSTED","domain":"cloudcode-pa.googleapis.com","metadata":{"error_number":"2010","model":"claude-opus-4-6-thinking"}}]}}`
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}

	response := doProxyRequest(t, env.engine, "/v1beta/models/claude-opus-4-6-thinking:generateContent", map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}},
	}, nil)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429; body=%s", response.Code, response.Body.String())
	}

	mu.Lock()
	gotURLs := append([]string(nil), requestBaseURLs...)
	gotTimes := append([]time.Time(nil), requestTimes...)
	mu.Unlock()
	wantURLs := []string{antigravityDailyBaseURL, antigravityProdBaseURL, antigravitySandboxDailyBaseURLForTest}
	if !slices.Equal(gotURLs, wantURLs) {
		t.Fatalf("Antigravity capacity attempts=%v, want %v", gotURLs, wantURLs)
	}
	for i := 1; i < len(gotTimes); i++ {
		if delay := gotTimes[i].Sub(gotTimes[i-1]); delay < antigravityBaseURLFallbackDelay {
			t.Fatalf("Antigravity fallback delay[%d]=%v, want >= %v", i, delay, antigravityBaseURLFallbackDelay)
		}
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs=(%d, %v)", len(configs), err)
	}
	cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if until := cooldowns[configs[0].ID]["claude-opus-4-6-thinking"]; !until.After(time.Now()) {
		t.Fatalf("capacity exhaustion did not cool model: %v", cooldowns)
	}
}

func TestProxy_AntigravityOAuthCapacityCountResetsAfterDifferentError(t *testing.T) {
	baseURLs := []string{
		"https://capacity-1.test",
		"https://not-found.test",
		"https://capacity-2.test",
		"https://capacity-3.test",
	}
	var calls atomic.Int32
	env := setupProxyTestEnv(t, []testChannel{{
		name: "antigravity-capacity-reset", upstreamProtocol: "gemini", models: "claude-opus-4-6-thinking", priority: 100,
		authType: model.AuthTypeAntigravityOAuth, oauthCredential: antigravityProxyTestCredential(t, "at-capacity-reset"),
	}}, map[int]string{0: strings.Join(baseURLs, "\n")})
	env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		status := http.StatusServiceUnavailable
		body := `{"error":{"code":503,"message":"No capacity available for model claude-opus-4-6-thinking on the server","status":"UNAVAILABLE","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"MODEL_CAPACITY_EXHAUSTED","domain":"cloudcode-pa.googleapis.com","metadata":{"error_number":"2010","model":"claude-opus-4-6-thinking"}}]}}`
		if call == 2 {
			status = http.StatusNotFound
			body = `{"error":{"code":404,"message":"not found"}}`
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}

	response := doProxyRequest(t, env.engine, "/v1beta/models/claude-opus-4-6-thinking:generateContent", map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}},
	}, nil)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503 after interrupted capacity sequence; body=%s", response.Code, response.Body.String())
	}
	if got := calls.Load(); got != int32(len(baseURLs)) {
		t.Fatalf("attempts=%d, want %d", got, len(baseURLs))
	}
}

func TestProxy_AntigravityOAuthUnwrapsStreamingGeminiResponse(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:streamGenerateContent" || r.URL.RawQuery != "alt=sse" {
			t.Errorf("Antigravity stream URL = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("Accept"); got != "" {
			t.Errorf("Accept = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"stream ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":2,"totalTokenCount":3}}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "antigravity-gemini-stream", upstreamProtocol: "gemini", models: "gemini-3-flash", priority: 100,
		authType: model.AuthTypeAntigravityOAuth, oauthCredential: antigravityProxyTestCredential(t, "at-stream"),
	}}, map[int]string{0: upstream.URL})

	response := doProxyRequest(t, env.engine, "/v1beta/models/gemini-3-flash:streamGenerateContent?alt=sse", map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"text":"stream ok"`) || strings.Contains(response.Body.String(), `"response"`) {
		t.Fatalf("unexpected Gemini SSE body: %s", response.Body.String())
	}
}

func TestProxy_AntigravityOAuthRefreshesAfterUnauthorized(t *testing.T) {
	var upstreamAttempts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := upstreamAttempts.Add(1)
		if attempt == 1 {
			if got := r.Header.Get("Authorization"); got != "Bearer at-old" {
				t.Errorf("first Authorization = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":401,"message":"expired"}}`)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer at-new" {
			t.Errorf("refreshed Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"refreshed"}]},"finishReason":"STOP"}]}}`)
	}))
	defer upstream.Close()

	var refreshes atomic.Int32
	var paidTierRefreshes atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1internal:loadCodeAssist" {
			paidTierRefreshes.Add(1)
			if got := r.Header.Get("Authorization"); got != "Bearer at-new" {
				t.Errorf("loadCodeAssist Authorization = %q", got)
			}
			_, _ = io.WriteString(w, `{"paidTier":{"id":"g1-pro-tier","name":"Google AI Pro"}}`)
			return
		}
		refreshes.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "rt-antigravity" {
			t.Errorf("refresh form = %v", r.Form)
		}
		_, _ = io.WriteString(w, `{"access_token":"at-new","expires_in":3600}`)
	}))
	defer tokenServer.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "antigravity-refresh", upstreamProtocol: "gemini", models: "gemini-3-flash", priority: 100,
		authType: model.AuthTypeAntigravityOAuth, oauthCredential: antigravityProxyTestCredential(t, "at-old"),
	}}, map[int]string{0: upstream.URL})
	service := antigravityauth.NewService(tokenServer.Client())
	service.TokenURL = tokenServer.URL
	service.DailyAPIBaseURL = tokenServer.URL
	env.server.antigravityCredentials.service = service
	env.server.antigravityCredentials.clientFor = func(*model.Config) *http.Client { return tokenServer.Client() }

	response := doProxyRequest(t, env.engine, "/v1beta/models/gemini-3-flash:generateContent", map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}}},
	}, nil)
	if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "candidates.0.content.parts.0.text").String() != "refreshed" {
		t.Fatalf("response=%d body=%s", response.Code, response.Body.String())
	}
	if upstreamAttempts.Load() != 2 || refreshes.Load() != 1 || paidTierRefreshes.Load() != 1 {
		t.Fatalf("upstream attempts=%d refreshes=%d paid tier refreshes=%d", upstreamAttempts.Load(), refreshes.Load(), paidTierRefreshes.Load())
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 || !strings.Contains(configs[0].OAuthCredential, `"access_token":"at-new"`) {
		t.Fatalf("persisted channel=%#v err=%v", configs, err)
	}
	persistedCredential, err := antigravityauth.ParseCredential([]byte(configs[0].OAuthCredential))
	if err != nil || persistedCredential.PaidTier == nil || persistedCredential.PaidTier.DisplayName() != "Google AI Pro" {
		t.Fatalf("persisted paid tier = (%#v, %v)", persistedCredential, err)
	}
}

func TestProxy_CodexOAuthChannelRefreshes401AndReassemblesNonStream(t *testing.T) {
	var upstreamAttempts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := upstreamAttempts.Add(1)
		wireBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read Codex wire body: %v", err)
		}
		if !gjson.GetBytes(wireBody, "stream").Bool() {
			t.Errorf("Codex OAuth wire stream must be true: %s", wireBody)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "account-proxy" {
			t.Errorf("ChatGPT-Account-ID = %q", got)
		}
		if attempt == 1 {
			if got := r.Header.Get("Authorization"); got != "Bearer at-old" {
				t.Errorf("first Authorization = %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"expired"}}`)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer at-new" {
			t.Errorf("refreshed Authorization = %q", got)
		}
		w.Header().Set("X-Codex-Plan-Type", "pro")
		w.Header().Set("X-Codex-Primary-Used-Percent", "6")
		w.Header().Set("X-Codex-Primary-Window-Minutes", "10080")
		w.Header().Set("X-Codex-Primary-Reset-At", "1786851417")
		w.Header().Set("X-Codex-Secondary-Used-Percent", "0")
		w.Header().Set("X-Codex-Secondary-Window-Minutes", "0")
		w.Header().Set("X-Codex-Bengalfox-Limit-Name", "GPT-5.3-Codex-Spark")
		w.Header().Set("X-Codex-Bengalfox-Primary-Used-Percent", "0")
		w.Header().Set("X-Codex-Bengalfox-Primary-Window-Minutes", "10080")
		w.Header().Set("X-Codex-Bengalfox-Primary-Reset-At", "1786876398000")
		w.Header().Set("X-Codex-Bengalfox-Secondary-Used-Percent", "0")
		w.Header().Set("X-Codex-Bengalfox-Secondary-Window-Minutes", "0")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-oauth","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`+"\n\n")
	}))
	defer upstream.Close()

	var refreshes atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		_, _ = io.WriteString(w, `{"access_token":"at-new","refresh_token":"rt-new","expires_in":604800}`)
	}))
	defer tokenServer.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "codex-oauth-http", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
		authType:        model.AuthTypeCodexOAuth,
		oauthCredential: codexProxyTestCredential(t, "at-old", "rt-old", "account-proxy"),
	}}, map[int]string{0: upstream.URL})
	service := codexauth.NewService(tokenServer.Client())
	service.TokenURL = tokenServer.URL
	env.server.codexCredentials.service = service
	env.server.codexCredentials.clientFor = func(*model.Config) *http.Client { return tokenServer.Client() }

	response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-test", "stream": false, "input": "hello",
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := gjson.Get(response.Body.String(), "id").String(); got != "resp-oauth" {
		t.Fatalf("response id=%q body=%s", got, response.Body.String())
	}
	if upstreamAttempts.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("upstream attempts=%d refreshes=%d", upstreamAttempts.Load(), refreshes.Load())
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 || !strings.Contains(configs[0].OAuthCredential, `"access_token":"at-new"`) {
		t.Fatalf("persisted refreshed channel=%#v err=%v", configs, err)
	}
	persistedCredential, err := codexauth.ParseCredential([]byte(configs[0].OAuthCredential))
	if err != nil || persistedCredential.PassiveUsage == nil ||
		len(persistedCredential.PassiveUsage.Windows) != 2 {
		t.Fatalf("persisted Codex quota = (%#v, %v)", persistedCredential, err)
	}
	windows := persistedCredential.PassiveUsage.Windows
	if windows[0].LimitName != "codex" || windows[0].UsedPercent != 6 ||
		windows[0].LimitWindowSeconds != 7*24*60*60 || windows[0].ResetAt != 1786851417 ||
		windows[1].LimitName != "GPT-5.3-Codex-Spark" || windows[1].UsedPercent != 0 ||
		windows[1].LimitWindowSeconds != 7*24*60*60 || windows[1].ResetAt != 1786876398 {
		t.Fatalf("persisted Codex quota windows = %#v", windows)
	}

	adminCtx, adminResponse := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
	env.server.HandleChannels(adminCtx)
	list := mustParseAPIResponse[[]ChannelWithCooldown](t, adminResponse.Body.Bytes())
	if len(list.Data) != 1 || list.Data[0].OAuthUsage == nil || len(list.Data[0].OAuthUsage.Windows) != 2 ||
		list.Data[0].OAuthUsage.Windows[0].RemainingPercent != 94 ||
		list.Data[0].OAuthUsage.Windows[1].RemainingPercent != 100 {
		t.Fatalf("channel Codex quota = %+v", list.Data)
	}
}

func TestProxy_CodexOAuthQuotaMergesIndependentResponseGroups(t *testing.T) {
	var calls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		call := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Primary-Used-Percent", strconv.Itoa(5+int(call)))
		w.Header().Set("X-Codex-Primary-Window-Minutes", "10080")
		w.Header().Set("X-Codex-Primary-Reset-At", "1786851417")
		if call == 1 {
			w.Header().Set("X-Codex-Bengalfox-Limit-Name", "GPT-5.3-Codex-Spark")
			w.Header().Set("X-Codex-Bengalfox-Primary-Used-Percent", "25")
			w.Header().Set("X-Codex-Bengalfox-Primary-Window-Minutes", "10080")
			w.Header().Set("X-Codex-Bengalfox-Primary-Reset-At", "1786876398")
		}
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-quota","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "codex-quota-merge", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
		authType:        model.AuthTypeCodexOAuth,
		oauthCredential: codexProxyTestCredential(t, "at-quota", "rt-quota", "account-quota"),
	}}, map[int]string{0: upstream.URL})
	for _, input := range []string{"first", "second"} {
		response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
			"model": "gpt-test", "stream": false, "input": input,
		}, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("%s response status=%d body=%s", input, response.Code, response.Body.String())
		}
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("list merged quota channel = (%#v, %v)", configs, err)
	}
	persisted, err := codexauth.ParseCredential([]byte(configs[0].OAuthCredential))
	if err != nil || persisted.PassiveUsage == nil || len(persisted.PassiveUsage.Windows) != 2 {
		t.Fatalf("merged Codex quota = (%#v, %v)", persisted, err)
	}
	if persisted.PassiveUsage.Windows[0].UsedPercent != 7 ||
		persisted.PassiveUsage.Windows[1].LimitName != "GPT-5.3-Codex-Spark" ||
		persisted.PassiveUsage.Windows[1].UsedPercent != 25 {
		t.Fatalf("independent quota groups were overwritten: %#v", persisted.PassiveUsage.Windows)
	}
}

func TestProxy_XAIOAuthZeroKeyFinalizesWireAndReassemblesNonStream(t *testing.T) {
	t.Parallel()

	var gotConversationID string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("upstream path = %q, want /v1/responses", r.URL.Path)
		}
		wireBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read xAI wire body: %v", err)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer xai-access" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get(xaiauth.CLITokenAuthHeader); got != xaiauth.CLITokenAuthValue {
			t.Errorf("%s = %q", xaiauth.CLITokenAuthHeader, got)
		}
		for name, want := range map[string]string{
			xaiauth.CLIClientVersionHeader: xaiauth.CLIClientVersion,
			"User-Agent":                   xaiauth.CLIUserAgent,
			xaiauth.CLIClientModeHeader:    xaiauth.CLIClientMode,
		} {
			if got := r.Header.Get(name); got != want {
				t.Errorf("%s = %q, want %q", name, got, want)
			}
		}
		if got := r.Header.Get("Accept"); got != "application/json, text/event-stream" {
			t.Errorf("Accept = %q", got)
		}
		gotConversationID = r.Header.Get("x-grok-conv-id")
		if gotConversationID == "" || gjson.GetBytes(wireBody, "prompt_cache_key").String() != gotConversationID {
			t.Errorf("conversation identity mismatch header=%q body=%s", gotConversationID, wireBody)
		}
		if !gjson.GetBytes(wireBody, "stream").Bool() || gjson.GetBytes(wireBody, "model").String() != "grok-4.5" {
			t.Errorf("xAI required body fields missing: %s", wireBody)
		}
		tools := gjson.GetBytes(wireBody, "tools").Array()
		switch gjson.GetBytes(wireBody, "tool_choice").String() {
		case "":
			if len(tools) != 1 || tools[0].Get("type").String() != "web_search" {
				t.Errorf("xAI CLI tools = %s, want one web_search", gjson.GetBytes(wireBody, "tools").Raw)
			}
		case "auto":
			if len(tools) != 1 || tools[0].Get("type").String() != "web_search" ||
				tools[0].Get("search_context_size").String() != "low" {
				t.Errorf("explicit xAI search tool was not preserved: %s", gjson.GetBytes(wireBody, "tools").Raw)
			}
		default:
			t.Errorf("xAI tool_choice = %q, want absent or preserved auto", gjson.GetBytes(wireBody, "tool_choice").String())
		}
		for _, field := range []string{"previous_response_id", "prompt_cache_retention", "safety_identifier", "stream_options"} {
			if gjson.GetBytes(wireBody, field).Exists() {
				t.Errorf("forbidden field %s survived: %s", field, wireBody)
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-xai","object":"response","status":"completed","model":"grok-4.5","output":[],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`+"\n\n")
	}))
	defer upstream.Close()

	credential := mustXAICredentialJSON(t, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "xai-access", RefreshToken: "xai-refresh",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	rules := &model.CustomRequestRules{
		Headers: []model.CustomHeaderRule{
			{Action: model.RuleActionOverride, Name: xaiauth.CLIClientVersionHeader, Value: "client-override"},
			{Action: model.RuleActionOverride, Name: "x-grok-conv-id", Value: "client-conversation"},
			{Action: model.RuleActionOverride, Name: "User-Agent", Value: "client-agent"},
			{Action: model.RuleActionOverride, Name: "Accept", Value: "application/json"},
		},
		Body: []model.CustomBodyRule{
			{Action: model.RuleActionOverride, Path: "stream", Value: json.RawMessage(`false`)},
			{Action: model.RuleActionOverride, Path: "prompt_cache_key", Value: json.RawMessage(`"client-conversation"`)},
			{Action: model.RuleActionOverride, Path: "previous_response_id", Value: json.RawMessage(`"client-previous"`)},
		},
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "xai-oauth-http", upstreamProtocol: "codex", models: "grok-4.5", priority: 100,
		authType: model.AuthTypeXAIOAuth, oauthCredential: credential, customRequestRules: rules,
	}}, map[int]string{0: xaiauth.CLIBaseURL})
	env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL.Scheme = "http"
		clone.URL.Host = upstream.host
		return dispatchTestHTTPRequest(clone)
	})}
	env.server.xaiCredentials = newXAICredentialManager(env.store, env.server.getClientForChannel, nil)

	response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "grok-4.5", "stream": false, "input": "hello",
		"previous_response_id": "resp-old", "prompt_cache_retention": "24h",
		"safety_identifier": "client", "stream_options": map[string]any{"include_usage": true},
	}, map[string]string{"Session-Id": "session", "Thread-Id": "parent"})
	if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "id").String() != "resp-xai" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	explicitSearchResponse := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "grok-4.5", "stream": false, "input": "current price",
		"tools":       []any{map[string]any{"type": "web_search", "search_context_size": "low"}},
		"tool_choice": "auto", "parallel_tool_calls": true,
	}, map[string]string{"Session-Id": "session", "Thread-Id": "parent"})
	if explicitSearchResponse.Code != http.StatusOK || gjson.Get(explicitSearchResponse.Body.String(), "id").String() != "resp-xai" {
		t.Fatalf("explicit search status=%d body=%s", explicitSearchResponse.Code, explicitSearchResponse.Body.String())
	}
	if gotConversationID == "" {
		t.Fatal("xAI conversation identity was not sent")
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs = %#v, %v", configs, err)
	}
	keys, err := env.store.GetAPIKeys(context.Background(), configs[0].ID)
	if err != nil || len(keys) != 0 {
		t.Fatalf("xAI OAuth channel keys = %#v, %v", keys, err)
	}
}

func TestProxy_XAIOAuthRefreshReplayBoundary(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantRefresh bool
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":{"type":"authentication_error","code":"invalid_token"}}`, wantRefresh: true},
		{name: "structured bad credential", status: http.StatusForbidden, body: `{"code":"invalid_token"}`, wantRefresh: true},
		{name: "ordinary forbidden", status: http.StatusForbidden, body: `{"error":{"message":"forbidden"}}`},
		{name: "entitlement forbidden", status: http.StatusForbidden, body: `{"code":"subscription_required"}`},
		{name: "quota", status: http.StatusTooManyRequests, body: `{"code":"quota_exceeded"}`},
		{name: "upstream failure", status: http.StatusBadGateway, body: `{"error":{"message":"bad gateway"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamAttempts atomic.Int32
			var firstExecutionID string
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempt := upstreamAttempts.Add(1)
				wantToken := "xai-old"
				if attempt == 2 {
					wantToken = "xai-new"
				}
				if got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); got != wantToken {
					t.Errorf("attempt %d access token = %q, want %q", attempt, got, wantToken)
				}
				wireBody, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read attempt %d body: %v", attempt, err)
				}
				executionID := r.Header.Get("x-grok-conv-id")
				if executionID == "" || gjson.GetBytes(wireBody, "prompt_cache_key").String() != executionID {
					t.Errorf("attempt %d execution identity header=%q body=%s", attempt, executionID, wireBody)
				}
				if attempt == 1 {
					firstExecutionID = executionID
				} else if executionID != firstExecutionID {
					t.Errorf("retry execution identity changed: first=%q retry=%q", firstExecutionID, executionID)
				}
				if tt.wantRefresh && attempt == 2 {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-refreshed","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer upstream.Close()

			credential := mustXAICredentialJSON(t, &xaiauth.Credential{
				Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "xai-old", RefreshToken: "xai-refresh",
				Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			})
			env := setupProxyTestEnv(t, []testChannel{{
				name: "xai-replay", upstreamProtocol: "codex", models: "grok-4.5", priority: 100,
				authType: model.AuthTypeXAIOAuth, oauthCredential: credential,
			}}, map[int]string{0: upstream.URL + "/v1"})

			var refreshes atomic.Int32
			refreshClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				refreshes.Add(1)
				if req.URL.String() != xaiauth.TokenURL {
					t.Fatalf("refresh URL = %s", req.URL)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"access_token":"xai-new","refresh_token":"xai-refresh-new","expires_in":3600}`)),
					Request:    req,
				}, nil
			})}
			env.server.xaiCredentials = newXAICredentialManager(
				env.store, func(*model.Config) *http.Client { return refreshClient }, func(int64) {
					env.server.InvalidateChannelListCache()
				},
			)

			response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model": "grok-4.5", "stream": false, "input": "hello",
			}, nil)
			wantAttempts := int32(1)
			wantRefreshes := int32(0)
			if tt.wantRefresh {
				wantAttempts = 2
				wantRefreshes = 1
				if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "id").String() != "resp-refreshed" {
					t.Fatalf("response=%d body=%s", response.Code, response.Body.String())
				}
			}
			if got := upstreamAttempts.Load(); got != wantAttempts {
				t.Fatalf("upstream attempts=%d, want %d", got, wantAttempts)
			}
			if got := refreshes.Load(); got != wantRefreshes {
				t.Fatalf("refreshes=%d, want %d", got, wantRefreshes)
			}
		})
	}
}

func TestProxy_XAIOAuthDoesNotReplayAfterCommittedSemanticOutput(t *testing.T) {
	var upstreamAttempts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamAttempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		largeDelta := strings.Repeat("x", SSEBufferSize)
		_, _ = fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", largeDelta)
		_, _ = io.WriteString(w, "event: error\n"+`data: {"type":"error","error":{"type":"authentication_error","code":"invalid_token","message":"expired"}}`+"\n\n")
	}))
	defer upstream.Close()

	credential := mustXAICredentialJSON(t, &xaiauth.Credential{
		Type: xaiauth.ChannelType, AuthKind: "oauth", AccessToken: "committed-access", RefreshToken: "committed-refresh",
		Expired: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	})
	env := setupProxyTestEnv(t, []testChannel{{
		name: "xai-committed", upstreamProtocol: "codex", models: "grok-4.5", priority: 100,
		authType: model.AuthTypeXAIOAuth, oauthCredential: credential,
	}}, map[int]string{0: upstream.URL + "/v1"})
	var refreshes atomic.Int32
	env.server.xaiCredentials = newXAICredentialManager(env.store, func(*model.Config) *http.Client {
		refreshes.Add(1)
		return http.DefaultClient
	}, nil)

	response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "grok-4.5", "stream": true, "input": "hello",
	}, nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"type":"response.output_text.delta"`) {
		t.Fatalf("response=%d body=%s", response.Code, response.Body.String())
	}
	if upstreamAttempts.Load() != 1 || refreshes.Load() != 0 {
		t.Fatalf("upstream attempts=%d refreshes=%d, want 1/0", upstreamAttempts.Load(), refreshes.Load())
	}
}

func TestProxy_CodexOAuthNonStreamingOpenAIClientReassemblesAndTranslates(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wireBody, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read Codex wire body: %v", err)
		}
		if !gjson.GetBytes(wireBody, "stream").Bool() {
			t.Errorf("Codex OAuth wire stream must be true: %s", wireBody)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", got)
		}
		_, _ = io.WriteString(w, "event: response.output_item.done\n")
		_, _ = io.WriteString(w, `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg-openai","status":"completed","role":"assistant","content":[{"type":"output_text","text":"translated non-stream"}]}}`+"\n\n")
		_, _ = io.WriteString(w, "event: response.completed\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp-openai","object":"response","status":"completed","model":"gpt-test","output":[],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "codex-oauth-openai-client", upstreamProtocol: "codex", models: "gpt-test", priority: 100,
		authType:        model.AuthTypeCodexOAuth,
		oauthCredential: codexProxyTestCredential(t, "at-openai", "rt-openai", "account-openai"),
	}}, map[int]string{0: upstream.URL})

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "gpt-test",
		"messages": []map[string]string{
			{"role": "user", "content": "hello"},
		},
		"stream": false,
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if got := gjson.Get(response.Body.String(), "choices.0.message.content").String(); got != "translated non-stream" {
		t.Fatalf("OpenAI response content=%q body=%s", got, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "data:") || strings.Contains(response.Body.String(), "response.completed") {
		t.Fatalf("Codex SSE leaked to non-stream OpenAI client: %s", response.Body.String())
	}
}

func TestProxy_CodexOAuthUsageLimitCoolsActualModel(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Codex-Primary-Used-Percent", "100")
		w.Header().Set("X-Codex-Primary-Window-Minutes", "10080")
		w.Header().Set("X-Codex-Primary-Reset-After-Seconds", "7260")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"plus","resets_in_seconds":7260}}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "codex-oauth-usage-limit", upstreamProtocol: "codex", models: "gpt-5.4-mini,gpt-5.4", priority: 100,
		authType:        model.AuthTypeCodexOAuth,
		oauthCredential: codexProxyTestCredential(t, "at-usage-limit", "rt-usage-limit", "account-usage-limit"),
	}}, map[int]string{0: upstream.URL})

	before := time.Now()
	_ = doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":  "gpt-5.4-mini",
		"input":  "hello",
		"stream": false,
	}, nil)

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs: configs=%d err=%v", len(configs), err)
	}
	cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("get model cooldowns: %v", err)
	}
	until := cooldowns[configs[0].ID]["gpt-5.4-mini"]
	duration := until.Sub(before)
	if duration < 7250*time.Second || duration > 7270*time.Second {
		t.Fatalf("model cooldown duration=%v, want about 7260s", duration)
	}
	if _, exists := cooldowns[configs[0].ID]["gpt-5.4"]; exists {
		t.Fatal("unaffected Codex model must not be cooled")
	}
	persistedCredential, err := codexauth.ParseCredential([]byte(configs[0].OAuthCredential))
	if err != nil || persistedCredential.PassiveUsage == nil || len(persistedCredential.PassiveUsage.Windows) != 1 ||
		persistedCredential.PassiveUsage.Windows[0].UsedPercent != 100 ||
		persistedCredential.PassiveUsage.Windows[0].LimitWindowSeconds != 7*24*60*60 ||
		persistedCredential.PassiveUsage.Windows[0].ResetAt < before.Add(7250*time.Second).Unix() {
		t.Fatalf("HTTP 429 Codex quota = (%#v, %v)", persistedCredential, err)
	}
}

func TestProxy_RetryOtherKeysOnFailure(t *testing.T) {
	for _, tt := range []struct {
		name                    string
		retryOtherKeysOnFailure bool
		wantPrimaryAttempts     int64
		wantFallbackAttempts    int64
	}{
		{
			name:                 "disabled keeps channel failover behavior",
			wantPrimaryAttempts:  1,
			wantFallbackAttempts: 1,
		},
		{
			name:                    "enabled tries another key before another channel",
			retryOtherKeysOnFailure: true,
			wantPrimaryAttempts:     2,
			wantFallbackAttempts:    0,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var primaryAttempts atomic.Int64
			var fallbackAttempts atomic.Int64
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryAttempts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				if r.Header.Get("Authorization") == "Bearer sk-provider-a" {
					w.WriteHeader(http.StatusBadGateway)
					_, _ = w.Write([]byte(`{"error":{"message":"provider A unavailable"}}`))
					return
				}
				if r.Header.Get("Authorization") != "Bearer sk-provider-b" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				_, _ = w.Write([]byte(`{"id":"provider-b","choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			}))
			defer upstream.Close()
			fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fallbackAttempts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"fallback","choices":[{"message":{"content":"fallback"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			}))
			defer fallback.Close()

			env := setupProxyTestEnv(t, []testChannel{
				{name: "relay", models: "gpt-test", apiKey: "sk-provider-a", priority: 100, retryOtherKeysOnFailure: tt.retryOtherKeysOnFailure},
				{name: "fallback", models: "gpt-test", priority: 50},
			}, map[int]string{0: upstream.URL, 1: fallback.URL})
			if err := env.store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{{
				ChannelID: 1, KeyIndex: 1, APIKey: "sk-provider-b", KeyStrategy: model.KeyStrategySequential,
			}}); err != nil {
				t.Fatalf("create secondary key: %v", err)
			}

			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "gpt-test", "messages": []map[string]string{{"role": "user", "content": "hi"}},
			}, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if got := primaryAttempts.Load(); got != tt.wantPrimaryAttempts {
				t.Fatalf("primary attempts=%d, want %d", got, tt.wantPrimaryAttempts)
			}
			if got := fallbackAttempts.Load(); got != tt.wantFallbackAttempts {
				t.Fatalf("fallback attempts=%d, want %d", got, tt.wantFallbackAttempts)
			}

			if tt.retryOtherKeysOnFailure {
				keys, err := env.store.GetAPIKeys(context.Background(), 1)
				if err != nil {
					t.Fatalf("get relay keys: %v", err)
				}
				if len(keys) != 2 || keys[0].CooldownUntil <= time.Now().Unix() {
					t.Fatalf("failed provider key should be cooled, keys=%+v", keys)
				}
				cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
				if err != nil {
					t.Fatalf("get model cooldowns: %v", err)
				}
				if len(cooldowns[1]) != 0 {
					t.Fatalf("key-fallback mode must not cool the model, got %+v", cooldowns[1])
				}
			}
		})
	}
}

func TestProxy_RetryOtherKeysSessionAffinity(t *testing.T) {
	var phase atomic.Int32
	var keyACalls, keyBCalls, fallbackCalls atomic.Int64

	primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var calls *atomic.Int64
		switch r.Header.Get("Authorization") {
		case "Bearer sk-provider-a":
			calls = &keyACalls
		case "Bearer sk-provider-b":
			calls = &keyBCalls
		default:
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		calls.Add(1)

		currentPhase := phase.Load()
		failed := currentPhase == 4 ||
			(currentPhase == 0 || currentPhase == 3) && r.Header.Get("Authorization") == "Bearer sk-provider-a"
		if failed {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"provider unavailable"}}`))
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp-primary","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	fallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp-fallback","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))

	env := setupProxyTestEnv(t, []testChannel{
		{
			name: "relay", upstreamProtocol: "codex", models: "gpt-test",
			apiKey: "sk-provider-a", priority: 100, retryOtherKeysOnFailure: true,
		},
		{
			name: "fallback", upstreamProtocol: "codex", models: "gpt-test",
			priority: 50, retryOtherKeysOnFailure: true,
		},
	}, map[int]string{0: primary.URL, 1: fallback.URL})
	if err := env.store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{{
		ChannelID: 1, KeyIndex: 1, APIKey: "sk-provider-b", KeyStrategy: model.KeyStrategySequential,
	}}); err != nil {
		t.Fatalf("create relay keys: %v", err)
	}
	env.server.maxKeyRetries = 1

	request := func(input string) *httptest.ResponseRecorder {
		return doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
			"model": "gpt-test", "stream": true, "input": input,
		}, map[string]string{"Session-Id": "sticky-key-session"})
	}
	resetRelay := func() {
		for keyIndex := range 2 {
			if err := env.store.ResetKeyCooldown(context.Background(), 1, keyIndex); err != nil {
				t.Fatalf("reset relay key %d: %v", keyIndex, err)
			}
		}
		if err := env.store.ResetChannelCooldown(context.Background(), 1); err != nil {
			t.Fatalf("reset relay channel: %v", err)
		}
		env.server.invalidateChannelRelatedCache(1)
	}
	assertCalls := func(wantA, wantB, wantFallback int64) {
		t.Helper()
		if keyACalls.Load() != wantA || keyBCalls.Load() != wantB || fallbackCalls.Load() != wantFallback {
			t.Fatalf("calls a/b/fallback=%d/%d/%d, want %d/%d/%d",
				keyACalls.Load(), keyBCalls.Load(), fallbackCalls.Load(), wantA, wantB, wantFallback)
		}
	}

	if response := request("one"); response.Code != http.StatusOK {
		t.Fatalf("first response status=%d body=%s", response.Code, response.Body.String())
	}
	assertCalls(1, 1, 0)

	// Make the fallback globally preferable. Session affinity must still keep the
	// established relay first while it has an available Key.
	fallbackCfg, err := env.store.GetConfig(context.Background(), 2)
	if err != nil {
		t.Fatalf("get fallback config: %v", err)
	}
	fallbackCfg.Priority = 200
	if _, err := env.store.UpdateConfig(context.Background(), fallbackCfg.ID, fallbackCfg); err != nil {
		t.Fatalf("raise fallback priority: %v", err)
	}
	env.server.InvalidateChannelListCache()

	phase.Store(1)
	if response := request("two"); response.Code != http.StatusOK {
		t.Fatalf("cooled-key response status=%d body=%s", response.Code, response.Body.String())
	}
	assertCalls(1, 2, 0)

	// Once A recovers, sequential selection must fail back to it instead of
	// pinning the last successful Key B.
	if err := env.store.ResetKeyCooldown(context.Background(), 1, 0); err != nil {
		t.Fatalf("recover relay key A: %v", err)
	}
	env.server.invalidateChannelRelatedCache(1)

	phase.Store(2)
	if response := request("three"); response.Code != http.StatusOK {
		t.Fatalf("recovered-key response status=%d body=%s", response.Code, response.Body.String())
	}
	assertCalls(2, 2, 0)

	phase.Store(3)
	if response := request("four"); response.Code != http.StatusOK {
		t.Fatalf("recooled-key response status=%d body=%s", response.Code, response.Body.String())
	}
	assertCalls(3, 3, 0)

	phase.Store(1)
	if response := request("five"); response.Code != http.StatusOK {
		t.Fatalf("second cooled-key response status=%d body=%s", response.Code, response.Body.String())
	}
	assertCalls(3, 4, 0)

	resetRelay()
	phase.Store(4)
	if response := request("six"); response.Code != http.StatusOK {
		t.Fatalf("channel fallback response status=%d body=%s", response.Code, response.Body.String())
	}
	assertCalls(4, 5, 1)

	// The preferred relay is now fully cooled, so the next turn must stay on the
	// fallback without probing relay Keys early.
	if response := request("seven"); response.Code != http.StatusOK {
		t.Fatalf("cooled-channel response status=%d body=%s", response.Code, response.Body.String())
	}
	assertCalls(4, 5, 2)

	// A successful fallback must not replace the first successful channel. Once
	// the relay recovers it wins again despite the fallback's higher priority.
	resetRelay()
	phase.Store(5)
	if response := request("eight"); response.Code != http.StatusOK {
		t.Fatalf("recovered-channel response status=%d body=%s", response.Code, response.Body.String())
	}
	assertCalls(5, 5, 2)
}

func TestProxy_GlobalCooldownDetectionRulesFallbackAndChannelOverride(t *testing.T) {
	globalRules := `{"rules":[{"enabled":true,"name":"Global maintenance","priority":0,"status_codes":[406],"message_pattern":"planned maintenance","scope":"channel","mode":"fixed","cooldown_seconds":120}]}`

	t.Run("channel without rules inherits global rules", func(t *testing.T) {
		var fallbackCalls atomic.Int64
		upstreamFail := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotAcceptable)
			_, _ = w.Write([]byte(`{"error":{"message":"planned maintenance"}}`))
		}))
		defer upstreamFail.Close()
		upstreamFallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fallbackCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"global-fallback","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		}))
		defer upstreamFallback.Close()

		env := setupProxyTestEnvWithSettings(t, []testChannel{
			{name: "inherits-global", models: "gpt-4", priority: 100},
			{name: "fallback", models: "gpt-4", priority: 50},
		}, map[int]string{0: upstreamFail.URL, 1: upstreamFallback.URL}, map[string]string{
			"global_cooldown_detection_rules": globalRules,
		})

		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model": "gpt-4", "messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, nil)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "global-fallback") {
			t.Fatalf("status=%d body=%s, want fallback success", response.Code, response.Body.String())
		}
		if got := fallbackCalls.Load(); got != 1 {
			t.Fatalf("fallback calls=%d, want 1", got)
		}
	})

	t.Run("channel rules replace global rules", func(t *testing.T) {
		var fallbackCalls atomic.Int64
		upstreamFail := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotAcceptable)
			_, _ = w.Write([]byte(`{"error":{"message":"planned maintenance"}}`))
		}))
		defer upstreamFail.Close()
		upstreamFallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			fallbackCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"unexpected-fallback","choices":[]}`))
		}))
		defer upstreamFallback.Close()

		env := setupProxyTestEnvWithSettings(t, []testChannel{
			{
				name: "overrides-global", models: "gpt-4", priority: 100,
				cooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
					Enabled: true, Name: "Local teapot", Priority: 0, StatusCodes: []int{http.StatusTeapot},
					Scope: model.CooldownScopeChannel, Mode: model.CooldownModeFixed, CooldownSeconds: 60,
				}}},
			},
			{name: "fallback", models: "gpt-4", priority: 50},
		}, map[int]string{0: upstreamFail.URL, 1: upstreamFallback.URL}, map[string]string{
			"global_cooldown_detection_rules": globalRules,
		})

		response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model": "gpt-4", "messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, nil)
		if response.Code != http.StatusNotAcceptable {
			t.Fatalf("status=%d body=%s, want channel rule override to preserve 406", response.Code, response.Body.String())
		}
		if got := fallbackCalls.Load(); got != 0 {
			t.Fatalf("fallback calls=%d, want 0", got)
		}
	})
}

// doProxyRequest 发送代理请求并返回响应
func doProxyRequest(t testing.TB, engine *gin.Engine, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req := httptest.NewRequest(http.MethodPost, path, bodyReader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-api-key") // 默认 token

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	w := httptest.NewRecorder()
	engine.ServeHTTP(w, req)
	return w
}

func createDashboardSession(t testing.TB, env *proxyTestEnv, plainToken string, authToken *model.AuthToken) string {
	t.Helper()
	authToken.Token = model.HashToken(plainToken)
	authToken.CreatedAt = time.Now()
	authToken.IsActive = true
	if err := env.store.CreateAuthToken(context.Background(), authToken); err != nil {
		t.Fatalf("CreateAuthToken failed: %v", err)
	}
	if err := env.server.authService.ReloadAuthTokens(); err != nil {
		t.Fatalf("ReloadAuthTokens failed: %v", err)
	}

	w := doProxyRequest(t, env.engine, "/login", map[string]any{
		"mode":  model.WebRoleAPIToken,
		"token": plainToken,
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard login status=%d body=%s", w.Code, w.Body.String())
	}
	var data struct {
		Token string `json:"token"`
	}
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &data)
	if data.Token == "" || data.Token == plainToken {
		t.Fatalf("dashboard login returned invalid web session token %q", data.Token)
	}
	return data.Token
}

// ============================================================================
// P0: 代理转发核心链路测试
// ============================================================================

func TestProxy_Success_NonStreaming(t *testing.T) {
	t.Parallel()

	// 模拟上游：返回 200 + JSON
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// 验证响应透传
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp["id"] != "chatcmpl-1" {
		t.Fatalf("expected id=chatcmpl-1, got %v", resp["id"])
	}
}

func TestProxy_ModelCooldownSSEErrorReturns429(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "event: error\n")
		_, _ = fmt.Fprint(w, "data: "+`{"type":"error","error":{"code":"model_cooldown","message":"model temporarily unavailable","model":"gpt-5.5"}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "sse-model-cooldown", models: "gpt-5.5"},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-5.5",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
		"stream":   true,
	}, nil)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429; body=%s", w.Code, w.Body.String())
	}
}

// OpenAI Responses 的 rate limit 失败终态：HTTP 200 + event:response.failed，
// error 嵌在 response.error。漏判会把限流当成功 200 返回。
func TestProxy_ResponseFailedSSERateLimitReturns429(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "event: response.failed\n")
		_, _ = fmt.Fprint(w, "data: "+`{"type":"response.failed","response":{"id":"resp_5ca0fb7943504d6a93576c7fb7e3a760","object":"response","model":"gpt-5.6-sol","status":"failed","output":[],"error":{"code":"rate_limit_exceeded","message":"Upstream rate limit exceeded, please retry later"}}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "sse-response-failed", models: "gpt-5.6-sol"},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-5.6-sol",
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
		"stream":   true,
	}, nil)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d, want 429; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "rate_limit_exceeded") &&
		!strings.Contains(w.Body.String(), "rate limit") &&
		!strings.Contains(strings.ToLower(w.Body.String()), "rate") {
		// 至少不能再当成功 200 空响应；body 内容允许被包装，但状态码必须是 429
		t.Logf("response body: %s", w.Body.String())
	}
}

func TestProxy_ModelCooldownUsesCustomRuleFinalModelKey(t *testing.T) {
	const finalModel = "shared-upstream-model"

	var primaryHits atomic.Int64
	primary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode primary request: %v", err)
		} else if body["model"] != finalModel {
			t.Errorf("primary model=%v, want %s", body["model"], finalModel)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"model_cooldown","message":"model temporarily unavailable","model":"shared-upstream-model","reset_seconds":300}}`))
	}))
	defer primary.Close()

	secondary := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-ok","object":"response","status":"completed","model":"fallback-model","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer secondary.Close()

	rules := &model.CustomRequestRules{Body: []model.CustomBodyRule{{
		Action: model.RuleActionOverride,
		Path:   "model",
		Value:  json.RawMessage(`"shared-upstream-model"`),
	}}}
	env := setupProxyTestEnv(t, []testChannel{
		{
			name:               "custom-rule-primary",
			upstreamProtocol:   util.ProtocolCodex,
			customRequestRules: rules,
			models:             "external-model-a,external-model-b",
			priority:           100,
		},
		{
			name:             "fallback-secondary",
			upstreamProtocol: util.ProtocolCodex,
			models:           "external-model-a,external-model-b",
			priority:         50,
		},
	}, map[int]string{0: primary.URL, 1: secondary.URL})

	request := func(modelName string) {
		t.Helper()
		w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
			"model":  modelName,
			"input":  "hello",
			"stream": false,
		}, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("model=%s status=%d, want 200; body=%s", modelName, w.Code, w.Body.String())
		}
	}

	request("external-model-a")

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("list configs: %v", err)
	}
	var primaryID int64
	for _, cfg := range configs {
		if cfg.Name == "custom-rule-primary" {
			primaryID = cfg.ID
			break
		}
	}
	if primaryID == 0 {
		t.Fatal("primary channel not found")
	}
	cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("get model cooldowns: %v", err)
	}
	if until := cooldowns[primaryID][finalModel]; !until.After(time.Now()) {
		t.Fatalf("final model cooldown=%s, want active cooldown", until.Format(time.RFC3339))
	}
	if _, exists := cooldowns[primaryID]["external-model-a"]; exists {
		t.Fatal("external alias must not be used as model cooldown key")
	}
	channelCooldowns, err := env.store.GetAllChannelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("get channel cooldowns: %v", err)
	}
	if until := channelCooldowns[primaryID]; until.After(time.Now()) {
		t.Fatalf("channel cooldown=%s, want model-only cooldown because another protocol path may resolve differently", until.Format(time.RFC3339))
	}

	request("external-model-b")
	if got := primaryHits.Load(); got != 1 {
		t.Fatalf("primary hits=%d, want 1 after shared final model cooldown", got)
	}
}

func TestProxy_CrossProtocolTranslationDropsClientQuery(t *testing.T) {
	var upstreamPath string
	var upstreamQuery string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		upstreamQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","model":"shared-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "drop-cross-protocol-query", upstreamProtocol: util.ProtocolOpenAI, models: "shared-model",
	}}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/messages?beta=true&prompt_cache_key=client-cache", map[string]any{
		"model":      "shared-model",
		"max_tokens": 16,
		"messages": []map[string]any{{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": "hi"}},
		}},
	}, map[string]string{"anthropic-version": "2023-06-01"})

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
	}
	if upstreamPath != "/v1/chat/completions" {
		t.Fatalf("upstream path=%q, want /v1/chat/completions", upstreamPath)
	}
	if upstreamQuery != "" {
		t.Fatalf("cross-protocol upstream query=%q, want empty", upstreamQuery)
	}
}

func TestProxy_AlphaSearchPassthroughWithRestrictedToken(t *testing.T) {
	var upstreamHits atomic.Int64
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("upstream method=%q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/alpha/search" {
			t.Errorf("upstream path=%q, want /v1/alpha/search", r.URL.Path)
		}
		if got := r.URL.Query().Get("scope"); got != "repo" {
			t.Errorf("upstream scope=%q, want repo", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode upstream body: %v", err)
		} else if body["query"] != "codegraph" {
			t.Errorf("upstream query=%v, want codegraph", body["query"])
		}
		if _, exists := body["prompt_cache_key"]; exists {
			t.Errorf("upstream body contains prompt_cache_key: %v", body)
		}
		if _, exists := body["prompt_cache_retention"]; exists {
			t.Errorf("upstream body contains prompt_cache_retention: %v", body)
		}
		if got := r.Header.Get("Session_id"); got != "" {
			t.Errorf("upstream Session_id=%q, want empty", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "native-codex", upstreamProtocol: util.ProtocolCodex, models: "gpt-5"},
	}, map[int]string{0: upstream.URL})

	plainToken := "sk-alpha-restricted"
	authToken := &model.AuthToken{
		Token:         model.HashToken(plainToken),
		Description:   "alpha search restricted token",
		CreatedAt:     time.Now(),
		IsActive:      true,
		AllowedModels: []string{"gpt-5"},
	}
	if err := env.store.CreateAuthToken(context.Background(), authToken); err != nil {
		t.Fatalf("CreateAuthToken failed: %v", err)
	}
	if err := env.server.authService.ReloadAuthTokens(); err != nil {
		t.Fatalf("ReloadAuthTokens failed: %v", err)
	}

	w := doProxyRequest(t, env.engine, "/v1/alpha/search?scope=repo", map[string]any{
		"query":                  "codegraph",
		"prompt_cache_key":       "responses-cache-key",
		"prompt_cache_retention": "24h",
	}, map[string]string{"Authorization": "Bearer " + plainToken})

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("upstream hits=%d, want 1", upstreamHits.Load())
	}
	// alpha/search 无 request model：日志 model 记 search_call，按次 $0.01
	entry := waitForProxyLog(t, env, util.BillingModelSearchCall)
	if entry.AuthTokenID != authToken.ID {
		t.Fatalf("AuthTokenID=%d, want %d", entry.AuthTokenID, authToken.ID)
	}
	if entry.Cost != 0.01 {
		t.Fatalf("Cost=%v, want 0.01", entry.Cost)
	}

	blocked := doProxyRequest(t, env.engine, "/v1/alpha/search", map[string]any{
		"model": "blocked-model",
		"query": "codegraph",
	}, map[string]string{"Authorization": "Bearer " + plainToken})
	if blocked.Code != http.StatusForbidden {
		t.Fatalf("explicit blocked model status=%d, want 403: %s", blocked.Code, blocked.Body.String())
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("blocked model reached upstream, hits=%d", upstreamHits.Load())
	}
}

func TestProxy_AlphaSearchUnsupportedFallsBackToEmptyResult(t *testing.T) {
	var upstreamHits atomic.Int64
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid URL (POST /v1/alpha/search)"}}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "alpha-search-failure", upstreamProtocol: util.ProtocolCodex, protocolTransformMode: model.ProtocolTransformModeAuto, models: "gpt-5"},
	}, map[int]string{0: upstream.URL})

	request := func() *httptest.ResponseRecorder {
		return doProxyRequest(t, env.engine, "/v1/alpha/search", map[string]any{
			"query": "codegraph",
		}, nil)
	}

	w := request()
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
	}
	var fallback struct {
		EncryptedOutput *string `json:"encrypted_output"`
		Output          string  `json:"output"`
		Results         []any   `json:"results"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &fallback)
	if fallback.EncryptedOutput != nil || fallback.Output != "" || len(fallback.Results) != 0 {
		t.Fatalf("unexpected empty search fallback: %+v", fallback)
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs: configs=%d err=%v", len(configs), err)
	}
	if got := env.server.costCache.Get(configs[0].ID); got != 0 {
		t.Fatalf("failed request cached cost=%v, want 0", got)
	}
	logs, err := env.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 20, 0, &model.LogFilter{LogSource: model.LogSourceProxy})
	if err != nil {
		t.Fatalf("ListLogs: %v", err)
	}
	if len(logs) != 0 {
		t.Fatalf("capability miss wrote proxy logs: %+v", logs)
	}
	cooldowns, err := env.store.GetAllChannelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns failed: %v", err)
	}
	if until := cooldowns[configs[0].ID]; until.After(time.Now()) {
		t.Fatalf("alpha search capability miss cooled channel until %v", until)
	}

	w = request()
	if w.Code != http.StatusOK {
		t.Fatalf("cached fallback status=%d, want 200: %s", w.Code, w.Body.String())
	}
	if got := upstreamHits.Load(); got != 1 {
		t.Fatalf("unsupported alpha search upstream hits=%d, want 1", got)
	}
}

func TestProxy_AlphaSearchUnsupportedFallsBackToNextChannel(t *testing.T) {
	var unsupportedHits atomic.Int64
	unsupported := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		unsupportedHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid URL (POST /v1/alpha/search)"}}`))
	}))
	defer unsupported.Close()

	var supportedHits atomic.Int64
	supported := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		supportedHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"encrypted_output":null,"output":"search result","results":[]}`))
	}))
	defer supported.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "alpha-search-unsupported", upstreamProtocol: util.ProtocolCodex, protocolTransformMode: model.ProtocolTransformModeAuto, models: "gpt-5", priority: 100},
		{name: "alpha-search-supported", upstreamProtocol: util.ProtocolCodex, protocolTransformMode: model.ProtocolTransformModeAuto, models: "gpt-5", priority: 90},
	}, map[int]string{0: unsupported.URL, 1: supported.URL})

	request := func() *httptest.ResponseRecorder {
		return doProxyRequest(t, env.engine, "/v1/alpha/search", map[string]any{
			"query": "codegraph",
		}, nil)
	}
	for i := 0; i < 2; i++ {
		w := request()
		if w.Code != http.StatusOK {
			t.Fatalf("request %d status=%d, want 200: %s", i+1, w.Code, w.Body.String())
		}
		var response struct {
			Output string `json:"output"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &response)
		if response.Output != "search result" {
			t.Fatalf("request %d output=%q, want search result", i+1, response.Output)
		}
	}
	if got := unsupportedHits.Load(); got != 1 {
		t.Fatalf("unsupported upstream hits=%d, want 1", got)
	}
	if got := supportedHits.Load(); got != 2 {
		t.Fatalf("supported upstream hits=%d, want 2", got)
	}
}

func TestProxy_AlphaSearchExactURLRouting(t *testing.T) {
	t.Run("responses exact URL cannot shadow native search", func(t *testing.T) {
		var wrongHits atomic.Int64
		wrong := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			wrongHits.Add(1)
			w.WriteHeader(http.StatusBadRequest)
		}))
		defer wrong.Close()

		var nativeHits atomic.Int64
		native := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			nativeHits.Add(1)
			if r.URL.Path != "/v1/alpha/search" {
				t.Errorf("native upstream path=%q, want /v1/alpha/search", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer native.Close()

		env := setupProxyTestEnv(t, []testChannel{
			{
				name:             "responses-exact",
				upstreamProtocol: util.ProtocolCodex,
				models:           "gpt-5",
				priority:         100,
			},
			{
				name:             "native-search",
				upstreamProtocol: util.ProtocolCodex,
				models:           "gpt-5",
				priority:         90,
			},
		}, map[int]string{
			0: wrong.URL + "/v1/responses#",
			1: native.URL,
		})

		w := doProxyRequest(t, env.engine, "/v1/alpha/search", map[string]any{
			"query": "codegraph",
		}, nil)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
		}
		if wrongHits.Load() != 0 || nativeHits.Load() != 1 {
			t.Fatalf("upstream hits wrong=%d native=%d, want 0/1", wrongHits.Load(), nativeHits.Load())
		}
	})

	t.Run("matching exact URL remains eligible", func(t *testing.T) {
		var upstreamHits atomic.Int64
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upstreamHits.Add(1)
			if r.URL.Path != "/v1/alpha/search" {
				t.Errorf("upstream path=%q, want /v1/alpha/search", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer upstream.Close()

		env := setupProxyTestEnv(t, []testChannel{{
			name:             "alpha-search-exact",
			upstreamProtocol: util.ProtocolCodex,
			models:           "gpt-5",
		}}, map[int]string{0: upstream.URL + "/v1/alpha/search#"})

		w := doProxyRequest(t, env.engine, "/v1/alpha/search", map[string]any{
			"query": "codegraph",
		}, nil)

		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
		}
		if upstreamHits.Load() != 1 {
			t.Fatalf("upstream hits=%d, want 1", upstreamHits.Load())
		}
	})
}

func TestDashboardProxy_UsesBoundTokenAndStreams(t *testing.T) {
	var upstreamHits atomic.Int64
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("upstream path=%q, want /v1/chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"dashboard\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "dashboard-stream", models: "gpt-dashboard", apiKey: "sk-dashboard-upstream"},
	}, map[int]string{0: upstream.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs len=%d err=%v", len(configs), err)
	}
	authToken := &model.AuthToken{
		Description:       "dashboard stream owner",
		AllowedModels:     []string{"gpt-dashboard"},
		AllowedChannelIDs: []int64{configs[0].ID},
	}
	webSession := createDashboardSession(t, env, "sk-dashboard-stream-owner", authToken)

	w := doProxyRequest(t, env.engine, "/dashboard/v1/chat/completions", map[string]any{
		"model":    "gpt-dashboard",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, map[string]string{"Authorization": "Bearer " + webSession})
	if w.Code != http.StatusOK {
		t.Fatalf("dashboard proxy status=%d body=%s", w.Code, w.Body.String())
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("upstream hits=%d, want 1", upstreamHits.Load())
	}
	if body := w.Body.String(); !strings.Contains(body, "dashboard") || !strings.Contains(body, "[DONE]") {
		t.Fatalf("unexpected dashboard SSE body: %s", body)
	}

	entry := waitForProxyLog(t, env, "gpt-dashboard")
	if entry.AuthTokenID != authToken.ID {
		t.Fatalf("AuthTokenID=%d, want %d", entry.AuthTokenID, authToken.ID)
	}
	if entry.ChannelID != configs[0].ID {
		t.Fatalf("ChannelID=%d, want %d", entry.ChannelID, configs[0].ID)
	}
}

func TestDashboardProxy_EnforcesModelAndChannelRestrictions(t *testing.T) {
	t.Run("model", func(t *testing.T) {
		var hits atomic.Int64
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"must-not-run"}`))
		}))
		defer upstream.Close()

		env := setupProxyTestEnv(t, []testChannel{
			{name: "model-restricted", models: "allowed-model,blocked-model"},
		}, map[int]string{0: upstream.URL})
		webSession := createDashboardSession(t, env, "sk-dashboard-model-owner", &model.AuthToken{
			Description:   "model restricted",
			AllowedModels: []string{"allowed-model"},
		})

		w := doProxyRequest(t, env.engine, "/dashboard/v1/chat/completions", map[string]any{
			"model":    "blocked-model",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, map[string]string{"Authorization": "Bearer " + webSession})
		if w.Code != http.StatusForbidden {
			t.Fatalf("status=%d, want 403: %s", w.Code, w.Body.String())
		}
		if hits.Load() != 0 {
			t.Fatalf("disallowed model reached upstream %d times", hits.Load())
		}
	})

	t.Run("channel", func(t *testing.T) {
		var blockedHits atomic.Int64
		blocked := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			blockedHits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer blocked.Close()
		var allowedHits atomic.Int64
		allowed := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			allowedHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"allowed-channel","choices":[{"message":{"content":"ok"}}]}`))
		}))
		defer allowed.Close()

		env := setupProxyTestEnv(t, []testChannel{
			{name: "blocked-channel", models: "gpt-dashboard", priority: 100},
			{name: "allowed-channel", models: "gpt-dashboard", priority: 90},
		}, map[int]string{0: blocked.URL, 1: allowed.URL})
		configs, err := env.store.ListConfigs(context.Background())
		if err != nil {
			t.Fatalf("ListConfigs failed: %v", err)
		}
		var allowedChannelID int64
		for _, cfg := range configs {
			if cfg.Name == "allowed-channel" {
				allowedChannelID = cfg.ID
			}
		}
		if allowedChannelID == 0 {
			t.Fatal("allowed channel not found")
		}
		webSession := createDashboardSession(t, env, "sk-dashboard-channel-owner", &model.AuthToken{
			Description:       "channel restricted",
			AllowedModels:     []string{"gpt-dashboard"},
			AllowedChannelIDs: []int64{allowedChannelID},
		})

		w := doProxyRequest(t, env.engine, "/dashboard/v1/chat/completions", map[string]any{
			"model":    "gpt-dashboard",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, map[string]string{"Authorization": "Bearer " + webSession})
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
		}
		if blockedHits.Load() != 0 || allowedHits.Load() != 1 {
			t.Fatalf("upstream hits blocked=%d allowed=%d, want 0/1", blockedHits.Load(), allowedHits.Load())
		}
	})
}

func TestDashboardProxy_RejectsRevokedToken(t *testing.T) {
	var hits atomic.Int64
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "revoked-dashboard", models: "gpt-dashboard"},
	}, map[int]string{0: upstream.URL})
	authToken := &model.AuthToken{Description: "revoked dashboard owner"}
	webSession := createDashboardSession(t, env, "sk-dashboard-revoked-owner", authToken)
	authToken.IsActive = false
	if err := env.store.UpdateAuthToken(context.Background(), authToken); err != nil {
		t.Fatalf("UpdateAuthToken failed: %v", err)
	}
	if err := env.server.authService.ReloadAuthTokens(); err != nil {
		t.Fatalf("ReloadAuthTokens failed: %v", err)
	}

	w := doProxyRequest(t, env.engine, "/dashboard/v1/chat/completions", map[string]any{
		"model":    "gpt-dashboard",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, map[string]string{"Authorization": "Bearer " + webSession})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401: %s", w.Code, w.Body.String())
	}
	if hits.Load() != 0 {
		t.Fatalf("revoked dashboard session reached upstream %d times", hits.Load())
	}
}

func TestProxy_NoAvailableUpstreamLogKeepsAuthTokenID(t *testing.T) {
	srv := newInMemoryServer(t)
	injectAPIToken(srv.authService, "test-api-key", 0, 77)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	srv.SetupRoutes(engine)
	env := &proxyTestEnv{server: srv, store: srv.store, engine: engine}

	w := doProxyRequest(t, engine, "/v1/chat/completions", map[string]any{
		"model":    "no-upstream-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}

	entry := waitForProxyLog(t, env, "no-upstream-model")
	if entry.AuthTokenID != 77 {
		t.Fatalf("AuthTokenID=%d, want 77", entry.AuthTokenID)
	}
}

func TestProxy_LogsAnthropicBudgetAsThinkingEffort(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"mimo-v2.5","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "fufu-thinking", models: "mimo-v2.5", apiKey: "sk-fufu-thinking", upstreamProtocol: util.ProtocolAnthropic},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model":      "mimo-v2.5",
		"max_tokens": 32000,
		"thinking": map[string]any{
			"type":          "enabled",
			"budget_tokens": 31999,
			"display":       "summarized",
		},
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]string{{
				"type": "text",
				"text": "hi",
			}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	entry := waitForProxyLog(t, env, "mimo-v2.5")
	if entry.ThinkingEffort != "high" {
		t.Fatalf("ThinkingEffort=%q, want high", entry.ThinkingEffort)
	}
}

func TestProxy_LogsThinkingEffortFromRequestAndJSONResponseOverride(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","thinking":{"level":"high"},"choices":[{"message":{"content":"hello"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "json-thinking", models: "gpt-4", apiKey: "sk-json-thinking"},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":            "gpt-4",
		"reasoning_effort": "low",
		"messages":         []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	entry := waitForProxyLog(t, env, "gpt-4")
	if entry.ThinkingEffort != "high" {
		t.Fatalf("ThinkingEffort=%q, want high", entry.ThinkingEffort)
	}
}

func TestProxy_LogsThinkingEffortFromRequestAndSSEOverride(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`event: response.created` + "\n" + `data: {"type":"response.created","response":{"reasoning":{"effort":"medium"}}}`,
			`event: response.completed` + "\n" + `data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5}}}`,
		}
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "%s\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "sse-thinking", models: "gpt-5-codex", apiKey: "sk-sse-thinking", upstreamProtocol: util.ProtocolCodex},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":     "gpt-5-codex",
		"stream":    true,
		"reasoning": map[string]any{"effort": "high"},
		"input":     "hi",
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	entry := waitForProxyLog(t, env, "gpt-5-codex")
	if entry.ThinkingEffort != "medium" {
		t.Fatalf("ThinkingEffort=%q, want medium", entry.ThinkingEffort)
	}
}

func TestProxy_CodexPriorityRequestBillsFastModeWithoutResponseTier(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp-fast","status":"completed","model":"gpt-5.6","output":[],"usage":{"input_tokens":1000,"output_tokens":1000,"total_tokens":2000}}}`+"\n\n")
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "codex-fast", models: "gpt-5.6", upstreamProtocol: util.ProtocolCodex},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":        "gpt-5.6",
		"stream":       true,
		"service_tier": "priority",
		"input":        "hi",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200: %s", w.Code, w.Body.String())
	}

	entry := waitForProxyLog(t, env, "gpt-5.6")
	if entry.ServiceTier != "priority" {
		t.Fatalf("ServiceTier=%q, want priority", entry.ServiceTier)
	}
	wantCost := util.CalculateCostDetailed("gpt-5.6", 1000, 1000, 0, 0, 0) * 2.5
	if !floatEquals(entry.Cost, wantCost) {
		t.Fatalf("Cost=%v, want fast-mode cost %v", entry.Cost, wantCost)
	}
}

func waitForProxyLog(t testing.TB, env *proxyTestEnv, modelName string) *model.LogEntry {
	t.Helper()

	ctx := context.Background()
	since := time.Now().Add(-time.Minute)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logs, err := env.store.ListLogs(ctx, since, 20, 0, &model.LogFilter{LogSource: model.LogSourceProxy})
		if err != nil {
			t.Fatalf("ListLogs failed: %v", err)
		}
		for _, entry := range logs {
			if entry.Model == modelName {
				return entry
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("proxy log for model %q not found within deadline", modelName)
	return nil
}

func TestProxy_SkipsChannelAfterRPMLimitExceeded(t *testing.T) {
	t.Parallel()

	var firstHits atomic.Int64
	firstUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-limited","choices":[{"message":{"content":"limited"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer firstUpstream.Close()

	var fallbackHits atomic.Int64
	fallbackUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-fallback","choices":[{"message":{"content":"fallback"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer fallbackUpstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "limited", models: "gpt-4", apiKey: "sk-limited", priority: 100},
		{name: "fallback", models: "gpt-4", apiKey: "sk-fallback", priority: 90},
	}, map[int]string{0: firstUpstream.URL, 1: fallbackUpstream.URL})

	ctx := context.Background()
	cfgs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	for _, cfg := range cfgs {
		if cfg.Name != "limited" {
			continue
		}
		cfg.RPMLimit = 1
		if _, err := env.store.UpdateConfig(ctx, cfg.ID, cfg); err != nil {
			t.Fatalf("UpdateConfig failed: %v", err)
		}
	}
	env.server.InvalidateChannelListCache()

	requestBody := map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}

	first := doProxyRequest(t, env.engine, "/v1/chat/completions", requestBody, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first request status=%d body=%s", first.Code, first.Body.String())
	}

	second := doProxyRequest(t, env.engine, "/v1/chat/completions", requestBody, nil)
	if second.Code != http.StatusOK {
		t.Fatalf("second request status=%d body=%s", second.Code, second.Body.String())
	}
	if firstHits.Load() != 1 {
		t.Fatalf("limited upstream hits=%d, want 1", firstHits.Load())
	}
	if fallbackHits.Load() != 1 {
		t.Fatalf("fallback upstream hits=%d, want 1", fallbackHits.Load())
	}
	if !strings.Contains(second.Body.String(), "from-fallback") {
		t.Fatalf("second response should come from fallback, got %s", second.Body.String())
	}
}

func TestProxy_SkipsChannelAfterConcurrencyLimitExceeded(t *testing.T) {
	t.Parallel()

	var limitedHits atomic.Int64
	limitedUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limitedHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-limited","choices":[{"message":{"content":"limited"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer limitedUpstream.Close()

	var fallbackHits atomic.Int64
	fallbackUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-fallback","choices":[{"message":{"content":"fallback"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer fallbackUpstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "limited", models: "gpt-4", apiKey: "sk-limited", priority: 100},
		{name: "fallback", models: "gpt-4", apiKey: "sk-fallback", priority: 90},
	}, map[int]string{0: limitedUpstream.URL, 1: fallbackUpstream.URL})

	ctx := context.Background()
	cfgs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	var limitedID int64
	for _, cfg := range cfgs {
		if cfg.Name != "limited" {
			continue
		}
		cfg.MaxConcurrency = 1
		limitedID = cfg.ID
		if _, err := env.store.UpdateConfig(ctx, cfg.ID, cfg); err != nil {
			t.Fatalf("UpdateConfig failed: %v", err)
		}
	}
	if limitedID == 0 {
		t.Fatal("limited channel not found")
	}
	env.server.InvalidateChannelListCache()

	release, _, _, ok := env.server.channelConcurrencyLimiter.acquire(limitedID, 1)
	if !ok {
		t.Fatal("pre-acquire limited channel slot failed")
	}
	defer release()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("request status=%d body=%s", w.Code, w.Body.String())
	}
	if limitedHits.Load() != 0 {
		t.Fatalf("limited upstream hits=%d, want 0", limitedHits.Load())
	}
	if fallbackHits.Load() != 1 {
		t.Fatalf("fallback upstream hits=%d, want 1", fallbackHits.Load())
	}
	if !strings.Contains(w.Body.String(), "from-fallback") {
		t.Fatalf("response should come from fallback, got %s", w.Body.String())
	}
}

func TestProxy_AllCooledFallback_UsesCooledKey(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer sk-cooled" {
			t.Fatalf("expected cooled key to be used, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-fallback","choices":[{"message":{"content":"fallback"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "cooled-key-channel", models: "gpt-4", apiKey: "sk-cooled"},
	}, map[int]string{0: upstream.URL})

	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	if err := env.store.SetKeyCooldown(ctx, configs[0].ID, 0, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("SetKeyCooldown failed: %v", err)
	}
	env.server.invalidateChannelRelatedCache(configs[0].ID)

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from all-cooled fallback, got %d: %s", w.Code, w.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("expected upstream to be called once, got %d", calls.Load())
	}
}

func TestProxy_Success_NonStreaming_OpenAIToGeminiTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", upstreamProtocol: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1beta/models/gemini-2.5-pro:generateContent", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(bytes.NewReader([]byte(`{"candidates":[{"content":{"parts":[{"text":"hello from gemini"}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":4,"totalTokenCount":11},"modelVersion":"gemini-2.5-pro"}`))),
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gemini-2.5-pro",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Fatalf("expected transformed Gemini path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"contents"`)) {
		t.Fatalf("expected Gemini request body, got %s", gotBody)
	}

	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "hello from gemini" {
		t.Fatalf("unexpected translated response: %s", w.Body.String())
	}
}

func TestProxy_LocalTransformRejectsHTMLSuccessResponse(t *testing.T) {
	t.Parallel()

	const maintenancePage = `<!DOCTYPE html><html lang="zh-CN"><head><title>维护中</title></head><body><h1>正在进行系统维护</h1></body></html>`
	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-html", upstreamProtocol: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})
	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/html; charset=utf-8"},
				},
				Body: io.NopCloser(strings.NewReader(maintenancePage)),
			}, nil
		}),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs failed: configs=%d err=%v", len(configs), err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()
	env.server.configService.mu.Lock()
	env.server.configService.cache["debug_log_enabled"] = &model.SystemSetting{Key: "debug_log_enabled", Value: "true"}
	env.server.configService.mu.Unlock()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gemini-2.5-pro",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("HTTP 200 HTML upstream should become 502, got %d: %s", w.Code, w.Body.String())
	}
	if strings.Contains(strings.ToLower(w.Body.String()), "<!doctype html") {
		t.Fatalf("maintenance page leaked as a successful client response: %s", w.Body.String())
	}

	entry := waitForProxyLog(t, env, "gemini-2.5-pro")
	if entry.StatusCode != http.StatusBadGateway {
		t.Fatalf("persisted status=%d, want 502", entry.StatusCode)
	}
	debugLog, err := env.store.GetDebugLogByLogID(context.Background(), entry.ID)
	if err != nil || debugLog == nil {
		t.Fatalf("GetDebugLogByLogID failed: debug=%+v err=%v", debugLog, err)
	}
	if string(debugLog.RespBody) != maintenancePage {
		t.Fatalf("debug original response=%q, want maintenance page", debugLog.RespBody)
	}
	if len(debugLog.TranslatedRespBody) != 0 {
		t.Fatalf("invalid HTML response must not have translated content: %s", debugLog.TranslatedRespBody)
	}
}

func TestProxy_Success_NonStreaming_AnthropicToGeminiTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", upstreamProtocol: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1beta/models/gemini-2.5-pro:generateContent", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(bytes.NewReader([]byte(`{"candidates":[{"content":{"parts":[{"text":"hello from gemini"}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":4,"totalTokenCount":11},"modelVersion":"gemini-2.5-pro"}`))),
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "gemini-2.5-pro",
		"messages": []map[string]any{{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": "hi"}},
		}},
	}, map[string]string{"anthropic-version": "2023-06-01"})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Fatalf("expected transformed Gemini path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"contents"`)) {
		t.Fatalf("expected Gemini request body, got %s", gotBody)
	}

	var resp struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Type != "message" || resp.Role != "assistant" || len(resp.Content) != 1 || resp.Content[0].Text != "hello from gemini" {
		t.Fatalf("unexpected translated anthropic response: %s", w.Body.String())
	}
}

func TestProxy_Success_NonStreaming_CodexToGeminiTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", upstreamProtocol: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1beta/models/gemini-2.5-pro:generateContent", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(bytes.NewReader([]byte(`{"candidates":[{"content":{"parts":[{"text":"hello from gemini"}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":4,"totalTokenCount":11},"modelVersion":"gemini-2.5-pro"}`))),
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gemini-2.5-pro",
		"input": []map[string]any{{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "hi"}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Fatalf("expected transformed Gemini path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"contents"`)) {
		t.Fatalf("expected Gemini request body, got %s", gotBody)
	}

	var resp struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Object != "response" || resp.Status != "completed" || len(resp.Output) != 1 || len(resp.Output[0].Content) != 1 || resp.Output[0].Content[0].Text != "hello from gemini" {
		t.Fatalf("unexpected translated codex response: %s", w.Body.String())
	}
}

func TestProxy_Success_Streaming(t *testing.T) {
	t.Parallel()

	// 模拟上游：返回 200 + SSE 流
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
			`data: {"choices":[{"delta":{"content":" World"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
			`data: [DONE]`,
		}
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "%s\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// 验证 SSE 内容被透传
	body := w.Body.String()
	if !strings.Contains(body, "Hello") {
		t.Fatalf("expected SSE to contain 'Hello', body: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("expected SSE to contain '[DONE]', body: %s", body)
	}
}

func TestProxy_Success_Streaming_OpenAIToGeminiTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte
	rawUpstreamBody := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}]}}]}\n\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" World\"}]}}]}\n\ndata: [DONE]\n\n"

	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", upstreamProtocol: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1beta/models/gemini-2.5-pro:streamGenerateContent", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			body := bytes.NewBufferString(rawUpstreamBody)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type":     []string{"text/event-stream"},
					"X-Upstream-Trace": []string{"stream-response"},
				},
				Body: io.NopCloser(body),
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()
	env.server.configService.mu.Lock()
	env.server.configService.cache["debug_log_enabled"] = &model.SystemSetting{Key: "debug_log_enabled", Value: "true"}
	env.server.configService.mu.Unlock()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gemini-2.5-pro",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, map[string]string{"X-Client-Trace": "stream-request"})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:streamGenerateContent" {
		t.Fatalf("expected transformed Gemini stream path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"contents"`)) {
		t.Fatalf("expected Gemini request body, got %s", gotBody)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"chat.completion.chunk"`) {
		t.Fatalf("expected OpenAI stream chunk, got %s", body)
	}
	if !strings.Contains(body, `"content":"Hello"`) || !strings.Contains(body, `"content":" World"`) {
		t.Fatalf("expected translated content chunks, got %s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected done marker, got %s", body)
	}

	entry := waitForProxyLog(t, env, "gemini-2.5-pro")
	debugLog, err := env.store.GetDebugLogByLogID(context.Background(), entry.ID)
	if err != nil {
		t.Fatalf("GetDebugLogByLogID failed: %v", err)
	}
	if debugLog == nil || !debugLog.ProtocolTransformed {
		t.Fatalf("expected persisted protocol transform debug log, got %+v", debugLog)
	}
	if got := gjson.GetBytes(debugLog.OriginalReqBody, "messages.0.content").String(); got != "hi" {
		t.Fatalf("original request content=%q, want hi; body=%s", got, debugLog.OriginalReqBody)
	}
	if debugLog.OriginalReqURL != "/v1/chat/completions" {
		t.Fatalf("original request URL=%q, want /v1/chat/completions", debugLog.OriginalReqURL)
	}
	if got := gjson.Get(debugLog.OriginalReqHeaders, "X-Client-Trace").String(); got != "stream-request" {
		t.Fatalf("original request header=%q, want stream-request; headers=%s", got, debugLog.OriginalReqHeaders)
	}
	if string(debugLog.RespBody) != rawUpstreamBody {
		t.Fatalf("original response body mismatch:\ngot=%s\nwant=%s", debugLog.RespBody, rawUpstreamBody)
	}
	if string(debugLog.TranslatedRespBody) != w.Body.String() {
		t.Fatalf("translated response should match client body:\ndebug=%s\nclient=%s", debugLog.TranslatedRespBody, w.Body.String())
	}
	if debugLog.TranslatedRespStatus != http.StatusOK {
		t.Fatalf("translated response status=%d, want 200", debugLog.TranslatedRespStatus)
	}
	if got := gjson.Get(debugLog.TranslatedRespHeaders, "Content-Type").String(); got != "text/event-stream" {
		t.Fatalf("translated response content type=%q, want text/event-stream; headers=%s", got, debugLog.TranslatedRespHeaders)
	}
	if got := gjson.Get(debugLog.TranslatedRespHeaders, "X-Upstream-Trace").String(); got != "stream-response" {
		t.Fatalf("translated response trace header=%q, want stream-response; headers=%s", got, debugLog.TranslatedRespHeaders)
	}
}

func TestProxy_Success_Streaming_AnthropicToGeminiTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", upstreamProtocol: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1beta/models/gemini-2.5-pro:streamGenerateContent", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			body := bytes.NewBufferString("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}]}}]}\n\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" World\"}]}}]}\n\ndata: [DONE]\n\n")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
				},
				Body: io.NopCloser(body),
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model":  "gemini-2.5-pro",
		"stream": true,
		"messages": []map[string]any{{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": "hi"}},
		}},
	}, map[string]string{"anthropic-version": "2023-06-01"})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:streamGenerateContent" {
		t.Fatalf("expected transformed Gemini stream path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"contents"`)) {
		t.Fatalf("expected Gemini request body, got %s", gotBody)
	}
	body := w.Body.String()
	if !strings.Contains(body, "event: message_start") || !strings.Contains(body, "event: content_block_delta") || !strings.Contains(body, `"text":"Hello"`) {
		t.Fatalf("expected anthropic stream events, got %s", body)
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Fatalf("expected anthropic message_stop event, got %s", body)
	}
}

func TestProxy_Success_Streaming_CodexToGeminiTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", upstreamProtocol: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1beta/models/gemini-2.5-pro:streamGenerateContent", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			body := bytes.NewBufferString("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}]}}]}\n\ndata: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" World\"}]}}]}\n\ndata: [DONE]\n\n")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
				},
				Body: io.NopCloser(body),
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":  "gemini-2.5-pro",
		"stream": true,
		"input": []map[string]any{{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "hi"}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:streamGenerateContent" {
		t.Fatalf("expected transformed Gemini stream path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"contents"`)) {
		t.Fatalf("expected Gemini request body, got %s", gotBody)
	}
	body := w.Body.String()
	if !strings.Contains(body, "event: response.output_text.delta") || !strings.Contains(body, `"delta":"Hello"`) {
		t.Fatalf("expected codex delta event, got %s", body)
	}
	if !strings.Contains(body, "event: response.completed") {
		t.Fatalf("expected codex completed event, got %s", body)
	}
}

func TestProxy_AutomaticProtocolFallback_OpenAIToAnthropic(t *testing.T) {
	t.Parallel()

	var gotPaths []string
	var gotBodies [][]byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "anthropic-ch", upstreamProtocol: "anthropic", protocolTransformMode: model.ProtocolTransformModeAuto, models: "claude-3-5-sonnet", apiKey: "sk-ant"},
	}, map[int]string{0: "https://anthropic-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			gotPaths = append(gotPaths, r.URL.Path)
			gotBodies = append(gotBodies, body)
			if r.URL.Path == "/v1/chat/completions" {
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Header:     http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
					Body:       io.NopCloser(bytes.NewReader([]byte("404: Not Found (DEPLOYMENT_NOT_FOUND)\n\nThe requested deployment does not exist."))),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(bytes.NewReader([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello from anthropic"}],"model":"claude-3-5-sonnet","stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":4}}`))),
			}, nil
		}),
	}

	request := func() {
		t.Helper()
		w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model":    "claude-3-5-sonnet",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, nil)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}

		var resp struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal response: %v", err)
		}
		if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "hello from anthropic" {
			t.Fatalf("unexpected translated response: %s", w.Body.String())
		}
	}

	request()
	request()

	if got := strings.Join(gotPaths, ","); got != "/v1/chat/completions,/v1/messages,/v1/messages" {
		t.Fatalf("upstream paths=%s, want native OpenAI then cached Anthropic fallback", got)
	}
	if !bytes.Contains(gotBodies[0], []byte(`"messages"`)) || bytes.Contains(gotBodies[0], []byte(`"text":"hi"`)) {
		t.Fatalf("native request is not OpenAI: %s", gotBodies[0])
	}
	for i, body := range gotBodies[1:] {
		if !bytes.Contains(body, []byte(`"messages"`)) || !bytes.Contains(body, []byte(`"text":"hi"`)) {
			t.Fatalf("fallback request body %d is not Anthropic: %s", i+1, body)
		}
	}
	entry := waitForProxyLog(t, env, "claude-3-5-sonnet")
	if entry.ClientProtocol != string(protocol.OpenAI) {
		t.Fatalf("client_protocol=%q, want openai client despite Anthropic fallback", entry.ClientProtocol)
	}
}

func TestProxy_URLProtocolsSkipNativeProbeAndUseDeclaredChannelProtocol(t *testing.T) {
	var paths []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"unexpected native probe"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-3-5-sonnet","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "declared-anthropic", upstreamProtocol: "anthropic", models: "claude-3-5-sonnet",
	}}, map[int]string{0: upstream.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs: configs=%d err=%v", len(configs), err)
	}
	configs[0].URLs[0].Protocols = []string{"anthropic"}
	if _, err := env.store.UpdateConfig(context.Background(), configs[0].ID, configs[0]); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "claude-3-5-sonnet", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s paths=%v", w.Code, w.Body.String(), paths)
	}
	if !slices.Equal(paths, []string{"/v1/messages"}) {
		t.Fatalf("paths=%v, want one direct local request", paths)
	}
}

func TestProxy_URLProtocolsSkipIncompatibleEndpointWithoutCoolingChannel(t *testing.T) {
	var incompatibleHits atomic.Int64
	incompatible := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		incompatibleHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer incompatible.Close()

	var compatibleHits atomic.Int64
	compatible := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		compatibleHits.Add(1)
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("compatible path=%q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","embedding":[0.1],"index":0}],"model":"shared-model","usage":{"prompt_tokens":1,"total_tokens":1}}`)
	}))
	defer compatible.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "incompatible", upstreamProtocol: "anthropic", models: "shared-model", priority: 100},
		{name: "compatible", upstreamProtocol: "openai", models: "shared-model", priority: 90},
	}, map[int]string{0: incompatible.URL, 1: compatible.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 2 {
		t.Fatalf("ListConfigs: configs=%d err=%v", len(configs), err)
	}
	for _, cfg := range configs {
		switch cfg.Name {
		case "incompatible":
			cfg.URLs[0].Protocols = []string{"codex"}
		case "compatible":
			cfg.URLs[0].Protocols = []string{"openai"}
		}
		if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
			t.Fatalf("UpdateConfig(%s): %v", cfg.Name, err)
		}
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/embeddings", map[string]any{
		"model": "shared-model", "input": "hi",
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if incompatibleHits.Load() != 0 || compatibleHits.Load() != 1 {
		t.Fatalf("hits incompatible=%d compatible=%d, want 0/1", incompatibleHits.Load(), compatibleHits.Load())
	}
	cooldowns, err := env.store.GetAllChannelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	for _, cfg := range configs {
		if cfg.Name == "incompatible" && cooldowns[cfg.ID].After(time.Now()) {
			t.Fatalf("incompatible declared URL cooled channel until %v", cooldowns[cfg.ID])
		}
	}
}

func TestProxy_AutomaticProtocolFallback_AllClientProtocolsCacheLearnedPath(t *testing.T) {
	const modelName = "shared-model"
	tests := []struct {
		name             string
		clientPath       string
		requestBody      map[string]any
		headers          map[string]string
		upstreamProtocol string
		localPath        string
		wantPaths        string
		missingStatus    int
		upstreamBody     string
	}{
		{
			name: "OpenAI", clientPath: "/v1/chat/completions", upstreamProtocol: "anthropic", localPath: "/v1/messages",
			requestBody:  map[string]any{"model": modelName, "messages": []map[string]string{{"role": "user", "content": "hi"}}},
			upstreamBody: `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"shared-model","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			wantPaths:    "/v1/chat/completions,/v1/messages,/v1/messages",
		},
		{
			name: "Anthropic", clientPath: "/v1/messages", upstreamProtocol: "openai", localPath: "/v1/chat/completions",
			missingStatus: http.StatusMethodNotAllowed,
			requestBody:   map[string]any{"model": modelName, "messages": []map[string]string{{"role": "user", "content": "hi"}}},
			headers:       map[string]string{"anthropic-version": "2023-06-01"},
			upstreamBody:  `{"id":"chatcmpl_1","object":"chat.completion","model":"shared-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			wantPaths:     "/v1/messages,/v1/chat/completions,/v1/chat/completions",
		},
		{
			name: "Codex", clientPath: "/v1/responses", upstreamProtocol: "openai", localPath: "/v1/chat/completions",
			requestBody: map[string]any{"model": modelName, "input": []map[string]any{{
				"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "hi"}},
			}}},
			upstreamBody: `{"id":"chatcmpl_1","object":"chat.completion","model":"shared-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			wantPaths:    "/v1/responses,/v1/chat/completions,/v1/chat/completions",
		},
		{
			name: "Gemini", clientPath: "/v1beta/models/shared-model:generateContent", upstreamProtocol: "openai", localPath: "/v1/chat/completions",
			requestBody:  map[string]any{"contents": []map[string]any{{"role": "user", "parts": []map[string]string{{"text": "hi"}}}}},
			upstreamBody: `{"id":"chatcmpl_1","object":"chat.completion","model":"shared-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
			wantPaths:    "/v1beta/models/shared-model:generateContent,/v1/chat/completions,/v1/chat/completions",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			missingStatus := tc.missingStatus
			if missingStatus == 0 {
				missingStatus = http.StatusNotFound
			}
			var paths []string
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path != tc.localPath {
					w.WriteHeader(missingStatus)
					_, _ = io.WriteString(w, fmt.Sprintf(`{"error":{"message":"Invalid URL (POST %s)"}}`, r.URL.Path))
					return
				}
				_, _ = io.WriteString(w, tc.upstreamBody)
			}))
			defer upstream.Close()
			env := setupProxyTestEnv(t, []testChannel{{
				name: strings.ToLower(tc.name) + "-fallback", upstreamProtocol: tc.upstreamProtocol,
				protocolTransformMode: model.ProtocolTransformModeAuto, models: modelName,
			}}, map[int]string{0: upstream.URL})

			for i := 0; i < 2; i++ {
				w := doProxyRequest(t, env.engine, tc.clientPath, tc.requestBody, tc.headers)
				if w.Code != http.StatusOK {
					t.Fatalf("request %d status=%d body=%s", i+1, w.Code, w.Body.String())
				}
			}
			if got := strings.Join(paths, ","); got != tc.wantPaths {
				t.Fatalf("paths=%s want=%s", got, tc.wantPaths)
			}
		})
	}
}

func TestProxy_ProtocolTransformModeStrict(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		wantPath   string
		wantStatus int
	}{
		{name: "upstream never falls back", mode: model.ProtocolTransformModeUpstream, wantPath: "/v1/chat/completions", wantStatus: http.StatusNotFound},
		{name: "local translates immediately", mode: model.ProtocolTransformModeLocal, wantPath: "/v1/messages", wantStatus: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var paths []string
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/v1/chat/completions" {
					w.WriteHeader(http.StatusNotFound)
					_, _ = io.WriteString(w, `{"error":{"message":"endpoint not found"}}`)
					return
				}
				_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-3-5-sonnet","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			defer upstream.Close()

			env := setupProxyTestEnv(t, []testChannel{{
				name: "strict-mode", upstreamProtocol: "anthropic", protocolTransformMode: tt.mode, models: "claude-3-5-sonnet",
			}}, map[int]string{0: upstream.URL})

			w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "claude-3-5-sonnet", "messages": []map[string]string{{"role": "user", "content": "hi"}},
			}, nil)
			if w.Code != tt.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tt.wantStatus, w.Body.String())
			}
			if !slices.Equal(paths, []string{tt.wantPath}) {
				t.Fatalf("paths=%v, want exactly [%s]", paths, tt.wantPath)
			}
		})
	}
}

func TestProxy_LocalModeUsesDeclaredProtocolOrder(t *testing.T) {
	var paths []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/responses":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"endpoint not found"}}`)
		case "/v1/messages":
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"shared-model","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "local-declared-order", upstreamProtocol: "gemini",
		protocolTransformMode: model.ProtocolTransformModeLocal, models: "shared-model",
	}}, map[int]string{0: upstream.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs: configs=%d err=%v", len(configs), err)
	}
	configs[0].URLs[0].Protocols = []string{"codex", "anthropic"}
	if _, err := env.store.UpdateConfig(context.Background(), configs[0].ID, configs[0]); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "shared-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !slices.Equal(paths, []string{"/v1/responses", "/v1/messages"}) {
		t.Fatalf("paths=%v, want declared order [codex anthropic]", paths)
	}
}

func TestProxy_LocalModeUsesFixedOrderWhenAllURLsAreAutomatic(t *testing.T) {
	var paths []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1beta/models/shared-model:generateContent" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"endpoint not found"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2},"modelVersion":"shared-model"}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "local-automatic-order", upstreamProtocol: "openai",
		protocolTransformMode: model.ProtocolTransformModeLocal, models: "shared-model",
	}}, map[int]string{0: upstream.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs: configs=%d err=%v", len(configs), err)
	}
	configs[0].URLs[0].Protocols = nil
	if _, err := env.store.UpdateConfig(context.Background(), configs[0].ID, configs[0]); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "shared-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s paths=%v", w.Code, w.Body.String(), paths)
	}
	want := []string{
		"/v1/messages",
		"/v1/responses",
		"/v1/chat/completions",
		"/v1beta/models/shared-model:generateContent",
	}
	if !slices.Equal(paths, want) {
		t.Fatalf("paths=%v, want local fallback order=%v", paths, want)
	}
}

func TestProxy_LocalModePrioritizesDeclaredURLs(t *testing.T) {
	var automaticHits atomic.Int64
	automatic := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		automaticHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer automatic.Close()

	var declaredHits atomic.Int64
	declared := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		declaredHits.Add(1)
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("declared path=%q, want /v1/messages", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"shared-model","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer declared.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "local-declared-url-first", upstreamProtocol: "gemini",
		protocolTransformMode: model.ProtocolTransformModeLocal, models: "shared-model",
	}}, map[int]string{0: automatic.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs: configs=%d err=%v", len(configs), err)
	}
	configs[0].URLs = model.ChannelURLs{
		{URL: automatic.URL},
		{URL: declared.URL, Protocols: []string{"anthropic"}},
	}
	updated, err := env.store.UpdateConfig(context.Background(), configs[0].ID, configs[0])
	if err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	env.server.InvalidateChannelListCache()
	env.server.urlSelector.RecordLatency(updated.ID, automatic.URL, time.Millisecond)
	env.server.urlSelector.RecordLatency(updated.ID, declared.URL, time.Second)

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "shared-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if automaticHits.Load() != 0 || declaredHits.Load() != 1 {
		t.Fatalf("hits automatic=%d declared=%d, want 0/1", automaticHits.Load(), declaredHits.Load())
	}
}

func TestProxy_AutoModePrioritizesAutomaticURLBeforeDeclaredConversion(t *testing.T) {
	var automaticPathsMu sync.Mutex
	var automaticPaths []string
	automatic := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		automaticPathsMu.Lock()
		automaticPaths = append(automaticPaths, r.URL.Path)
		automaticPathsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"endpoint not found"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl_auto","object":"chat.completion","model":"shared-model","choices":[{"index":0,"message":{"role":"assistant","content":"direct"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer automatic.Close()

	var declaredHits atomic.Int64
	declared := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		declaredHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"converted"}],"model":"shared-model","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer declared.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "auto-original-protocol-first", upstreamProtocol: util.ProtocolAnthropic,
		protocolTransformMode: model.ProtocolTransformModeAuto, models: "shared-model",
	}}, map[int]string{0: automatic.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs: configs=%d err=%v", len(configs), err)
	}
	configs[0].URLs = model.ChannelURLs{
		{URL: declared.URL, Protocols: []string{"anthropic"}},
		{URL: automatic.URL},
	}
	if _, err := env.store.UpdateConfig(context.Background(), configs[0].ID, configs[0]); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	env.server.InvalidateChannelListCache()
	// 固定配置顺序，证明 auto 模式会主动把自动检测 URL 提到转换 URL 之前。
	env.server.urlSelector = nil

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "shared-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	automaticPathsMu.Lock()
	gotAutomaticPaths := append([]string(nil), automaticPaths...)
	automaticPathsMu.Unlock()
	if !slices.Equal(gotAutomaticPaths, []string{"/v1/chat/completions"}) {
		t.Fatalf("automatic paths=%v, want original client protocol first", gotAutomaticPaths)
	}
	if got := declaredHits.Load(); got != 0 {
		t.Fatalf("declared conversion URL hits=%d, want 0 when automatic URL accepts original protocol", got)
	}
}

func TestProxy_LocalModeUsesDeclaredProtocolForAutomaticBackupURL(t *testing.T) {
	var declaredPaths []string
	declared := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		declaredPaths = append(declaredPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"endpoint not found"}}`)
	}))
	defer declared.Close()

	var backupPaths []string
	backup := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupPaths = append(backupPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"endpoint not found"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","model":"shared-model","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer backup.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "local-declared-protocol-backup", upstreamProtocol: "gemini",
		protocolTransformMode: model.ProtocolTransformModeLocal, models: "shared-model",
	}}, map[int]string{0: backup.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs: configs=%d err=%v", len(configs), err)
	}
	configs[0].URLs = model.ChannelURLs{
		{URL: declared.URL, Protocols: []string{"codex"}},
		{URL: backup.URL},
	}
	if _, err := env.store.UpdateConfig(context.Background(), configs[0].ID, configs[0]); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "shared-model", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !slices.Equal(declaredPaths, []string{"/v1/responses"}) {
		t.Fatalf("declared paths=%v, want codex only", declaredPaths)
	}
	if !slices.Equal(backupPaths, []string{"/v1/responses"}) {
		t.Fatalf("backup paths=%v, want declared codex protocol", backupPaths)
	}
}

func TestProxy_AutomaticProtocolFallback_CacheIsolatedByURL(t *testing.T) {
	var pathsA, pathsB []string
	newUpstream := func(paths *[]string) *testHTTPServer {
		return newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*paths = append(*paths, r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/v1/chat/completions" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"error":{"message":"Invalid URL (POST /v1/chat/completions)"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-3-5-sonnet","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		}))
	}
	upstreamA := newUpstream(&pathsA)
	defer upstreamA.Close()
	upstreamB := newUpstream(&pathsB)
	defer upstreamB.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "multi-url-anthropic", upstreamProtocol: "anthropic",
		protocolTransformMode: model.ProtocolTransformModeAuto, models: "claude-3-5-sonnet",
	}}, map[int]string{0: upstreamA.URL + "\n" + upstreamB.URL})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs: configs=%d err=%v", len(configs), err)
	}
	channelID := configs[0].ID
	env.server.urlSelector.DisableURL(channelID, upstreamB.URL)

	request := func() {
		t.Helper()
		w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model": "claude-3-5-sonnet", "messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	request()
	request()
	env.server.urlSelector.EnableURL(channelID, upstreamB.URL)
	env.server.urlSelector.DisableURL(channelID, upstreamA.URL)
	request()

	if got := strings.Join(pathsA, ","); got != "/v1/chat/completions,/v1/messages,/v1/messages" {
		t.Fatalf("URL A paths=%s, want native discovery then cached Anthropic", got)
	}
	if got := strings.Join(pathsB, ","); got != "/v1/chat/completions,/v1/messages" {
		t.Fatalf("URL B paths=%s, want independent native discovery", got)
	}
}

func TestProxy_AutomaticProtocolFallback_CacheIsolatedByRequestFamily(t *testing.T) {
	var paths []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/chat/completions":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"Invalid URL (POST /v1/chat/completions)"}}`)
		case "/v1/messages":
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-3-5-sonnet","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		case "/v1/embeddings":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","embedding":[0.1],"index":0}],"model":"claude-3-5-sonnet","usage":{"prompt_tokens":1,"total_tokens":1}}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer upstream.Close()
	env := setupProxyTestEnv(t, []testChannel{{
		name: "family-anthropic", upstreamProtocol: "anthropic",
		protocolTransformMode: model.ProtocolTransformModeAuto, models: "claude-3-5-sonnet",
	}}, map[int]string{0: upstream.URL})

	chat := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "claude-3-5-sonnet", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if chat.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", chat.Code, chat.Body.String())
	}
	embeddings := doProxyRequest(t, env.engine, "/v1/embeddings", map[string]any{
		"model": "claude-3-5-sonnet", "input": "hi",
	}, nil)
	if embeddings.Code != http.StatusOK {
		t.Fatalf("embeddings status=%d body=%s", embeddings.Code, embeddings.Body.String())
	}
	chat = doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "claude-3-5-sonnet", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if chat.Code != http.StatusOK {
		t.Fatalf("cached chat status=%d body=%s", chat.Code, chat.Body.String())
	}

	if got := strings.Join(paths, ","); got != "/v1/chat/completions,/v1/messages,/v1/embeddings,/v1/messages" {
		t.Fatalf("upstream paths=%s, want Chat cache isolated from Embeddings", got)
	}
}

func TestProxy_AutomaticProtocolFallback_DoesNotTranslateOrdinaryErrors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantStatus int
	}{
		{name: "model 404", status: http.StatusNotFound, body: `{"error":{"message":"model claude-3-5-sonnet not found","type":"invalid_request_error","code":"model_not_found"}}`, wantStatus: http.StatusNotFound},
		{name: "unauthorized", status: http.StatusUnauthorized, body: `{"error":{"message":"unauthorized"}}`, wantStatus: http.StatusUnauthorized},
		{name: "forbidden", status: http.StatusForbidden, body: `{"error":{"message":"forbidden"}}`, wantStatus: http.StatusForbidden},
		{name: "rate limited", status: http.StatusTooManyRequests, body: `{"error":{"message":"rate limited"}}`, wantStatus: http.StatusTooManyRequests},
		{name: "server error", status: http.StatusInternalServerError, body: `{"error":{"message":"upstream failed"}}`, wantStatus: http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var paths []string
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			env := setupProxyTestEnv(t, []testChannel{{
				name: "error-anthropic", upstreamProtocol: "anthropic",
				protocolTransformMode: model.ProtocolTransformModeAuto, models: "claude-3-5-sonnet",
			}}, map[int]string{0: upstream.URL})

			w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
				"model": "claude-3-5-sonnet", "messages": []map[string]string{{"role": "user", "content": "hi"}},
			}, nil)
			if w.Code != tc.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
			if got := strings.Join(paths, ","); got != "/v1/chat/completions" {
				t.Fatalf("upstream paths=%s, ordinary error must stop protocol fallback", got)
			}
		})
	}
}

func TestProxy_AutomaticProtocolFallback_UnsupportedAnthropicBeta(t *testing.T) {
	var paths []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/messages" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"尚未验证或不支持的 anthropic-beta：claude-code-20250219"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "unsupported-anthropic-beta", protocolTransformMode: model.ProtocolTransformModeAuto, models: "deepseek-v4-flash",
	}}, map[int]string{0: upstream.URL})

	request := func() {
		t.Helper()
		w := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
			"model":      "deepseek-v4-flash",
			"max_tokens": 128,
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		}, map[string]string{
			"anthropic-version": "2023-06-01",
			"anthropic-beta":    "claude-code-20250219",
		})
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if got := gjson.GetBytes(w.Body.Bytes(), "content.0.text").String(); got != "ok" {
			t.Fatalf("translated Anthropic response text=%q body=%s", got, w.Body.String())
		}
	}

	request()
	request()

	if got := strings.Join(paths, ","); got != "/v1/messages,/v1/chat/completions,/v1/chat/completions" {
		t.Fatalf("upstream paths=%s, want native Anthropic then cached OpenAI", got)
	}

	env.server.InvalidateChannelListCache()
	request()
	if got := strings.Join(paths, ","); got != "/v1/messages,/v1/chat/completions,/v1/chat/completions,/v1/messages,/v1/chat/completions" {
		t.Fatalf("upstream paths=%s, want channel cache invalidation to probe Anthropic again", got)
	}
}

func TestProxy_AutomaticProtocolFallback_ResponsesModelNotSupported(t *testing.T) {
	var paths []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/responses" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"当前模型不支持 Responses API：deepseek-v4-flash","type":"invalid_request_error","param":null,"code":"RESPONSES_MODEL_NOT_SUPPORTED"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","model":"deepseek-v4-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "responses-model-not-supported", protocolTransformMode: model.ProtocolTransformModeAuto, models: "deepseek-v4-flash",
	}}, map[int]string{0: upstream.URL})

	request := func() {
		t.Helper()
		w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
			"model": "deepseek-v4-flash",
			"input": []map[string]any{{
				"type": "message", "role": "user",
				"content": []map[string]string{{"type": "input_text", "text": "hi"}},
			}},
		}, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		if got := gjson.GetBytes(w.Body.Bytes(), "output.0.content.0.text").String(); got != "ok" {
			t.Fatalf("translated Codex response text=%q body=%s", got, w.Body.String())
		}
	}

	request()
	request()

	if got := strings.Join(paths, ","); got != "/v1/responses,/v1/chat/completions,/v1/chat/completions" {
		t.Fatalf("upstream paths=%s, want client Codex then cached OpenAI", got)
	}
}

func TestProxy_AutomaticProtocolFallback_ConvertRequestNotImplemented(t *testing.T) {
	var paths []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/chat/completions":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"Invalid URL (POST /v1/chat/completions)"}}`)
			return
		case "/v1/messages":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"not implemented (request id: req_test)","type":"new_api_error","param":"","code":"convert_request_failed"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_test","object":"response","status":"completed","model":"claude-4.5-haiku","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "codex-to-openai", upstreamProtocol: "openai",
		protocolTransformMode: model.ProtocolTransformModeAuto, models: "claude-4.5-haiku",
	}}, map[int]string{0: upstream.URL})

	request := func() {
		t.Helper()
		w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model":    "claude-4.5-haiku",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}

	request()
	request()

	if got := strings.Join(paths, ","); got != "/v1/chat/completions,/v1/messages,/v1/responses,/v1/responses" {
		t.Fatalf("upstream paths=%s, want client OpenAI failure, Anthropic failure, then cached Codex", got)
	}
}

func TestProxy_AutomaticProtocolFallback_SkipsUnrepresentableTransforms(t *testing.T) {
	var paths []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.6-sol","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "codex-unrepresentable-transform", upstreamProtocol: "codex",
		protocolTransformMode: model.ProtocolTransformModeAuto, models: "gpt-5.6-sol",
	}}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-5.6-sol",
		"input": []map[string]any{{
			"type":              "compaction",
			"encrypted_content": "opaque",
		}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !slices.Equal(paths, []string{"/v1/responses"}) {
		t.Fatalf("paths=%v, want native Codex after local transform rejection", paths)
	}
}

func TestProxy_AutomaticProtocolFallback_ExactURLTranslatesDirectly(t *testing.T) {
	var paths []string
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"claude-3-5-sonnet","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	env := setupProxyTestEnv(t, []testChannel{{
		name: "exact-anthropic", upstreamProtocol: "anthropic",
		protocolTransformMode: model.ProtocolTransformModeAuto, models: "claude-3-5-sonnet",
	}}, map[int]string{0: upstream.URL + "/v1/messages#"})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "claude-3-5-sonnet", "messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if got := strings.Join(paths, ","); got != "/v1/messages" {
		t.Fatalf("exact URL paths=%s, want one direct local request", got)
	}
}

func TestProxy_NonStreamingOpenAIToAnthropic_TranslatesUnexpectedSSE(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "anthropic-ch", upstreamProtocol: "anthropic", models: "mimo-v2.5", apiKey: "sk-ant"},
	}, map[int]string{0: "https://anthropic-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1/messages", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			body := bytes.NewBufferString(
				"event: message_start\n" +
					"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"mimo-v2.5\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
					"event: content_block_start\n" +
					"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
					"event: content_block_delta\n" +
					"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"think\"}}\n\n" +
					"event: content_block_stop\n" +
					"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
					"event: content_block_start\n" +
					"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
					"event: content_block_delta\n" +
					"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
					"event: message_delta\n" +
					"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n" +
					"event: message_stop\n" +
					"data: {\"type\":\"message_stop\"}\n\n",
			)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(body),
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "mimo-v2.5",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("expected anthropic messages path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"stream":false`)) {
		t.Fatalf("expected upstream request to preserve stream=false, got %s", gotBody)
	}
	if strings.Contains(w.Body.String(), "event:") {
		t.Fatalf("expected OpenAI JSON response, got raw SSE: %s", w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}

	var resp struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Object != "chat.completion" || len(resp.Choices) != 1 {
		t.Fatalf("unexpected translated response: %s", w.Body.String())
	}
	if resp.Choices[0].Message.Content != "hello" || resp.Choices[0].Message.ReasoningContent != "think" {
		t.Fatalf("unexpected translated message: %s", w.Body.String())
	}
}

func TestProxy_NonStreamingOpenAIClientNormalizesAnthropicJSONFromUpstreamMode(t *testing.T) {
	t.Parallel()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "anthropic-upstream-mode", upstreamProtocol: "anthropic", protocolTransformMode: model.ProtocolTransformModeUpstream, models: "mimo-v2.5", apiKey: "sk-ant"},
	}, map[int]string{0: "https://anthropic-upstream.example.com"})

	var gotPath string
	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"think"},{"type":"text","text":"hello"}],"model":"mimo-v2.5","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5}}`,
				))),
			}, nil
		}),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "mimo-v2.5",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("expected upstream mode to keep OpenAI path, got %s", gotPath)
	}
	body := w.Body.String()
	if strings.Contains(body, `"type":"message"`) || strings.Contains(body, `"stop_reason"`) {
		t.Fatalf("expected OpenAI JSON, got raw Anthropic JSON: %s", body)
	}
	if !strings.Contains(body, `"object":"chat.completion"`) || !strings.Contains(body, `"content":"hello"`) || !strings.Contains(body, `"reasoning_content":"think"`) {
		t.Fatalf("unexpected normalized OpenAI JSON: %s", body)
	}
}

func TestProxy_NonStreamingAnthropicClientNormalizesOpenAIJSONFromUpstreamMode(t *testing.T) {
	t.Parallel()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "openai-upstream-mode", upstreamProtocol: "openai", protocolTransformMode: model.ProtocolTransformModeUpstream, models: "gpt-4o", apiKey: "sk-oai"},
	}, map[int]string{0: "https://openai-upstream.example.com"})

	var gotPath string
	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"id":"chatcmpl_1","object":"chat.completion","created":0,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}`,
				))),
			}, nil
		}),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "gpt-4o",
		"messages": []map[string]any{{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": "hi"}},
		}},
	}, map[string]string{"anthropic-version": "2023-06-01"})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("expected upstream mode to keep Anthropic path, got %s", gotPath)
	}
	body := w.Body.String()
	if strings.Contains(body, `"chat.completion"`) || strings.Contains(body, `"choices"`) {
		t.Fatalf("expected Anthropic JSON, got raw OpenAI JSON: %s", body)
	}
	if !strings.Contains(body, `"type":"message"`) || !strings.Contains(body, `"text":"hello"`) || !strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Fatalf("unexpected normalized Anthropic JSON: %s", body)
	}
}

func TestProxy_OpenAIShapedBodyOnAnthropicPathIsRejected(t *testing.T) {
	t.Parallel()

	called := false

	env := setupProxyTestEnv(t, []testChannel{
		{name: "anthropic-ch", upstreamProtocol: "anthropic", models: "mimo-v2.5", apiKey: "sk-ant"},
	}, map[int]string{0: "https://token-plan-cn.example.com/anthropic"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			called = true
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"model":"mimo-v2.5","stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":4}}`,
				))),
			}, nil
		}),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/messages?beta=true", map[string]any{
		"model": "mimo-v2.5",
		"messages": []map[string]string{
			{"role": "user", "content": "hello"},
		},
		"max_tokens":       4096,
		"response_format":  map[string]string{"type": "json_object"},
		"stream_options":   map[string]bool{"include_usage": true},
		"prompt_cache_key": "cache-key-1",
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("upstream should not be called for mismatched client protocol body")
	}
	if !strings.Contains(w.Body.String(), "OpenAI chat completions") {
		t.Fatalf("expected protocol mismatch error, got %s", w.Body.String())
	}
}

func TestProxy_OpenAIShapedBodyOnGeminiPathIsRejected(t *testing.T) {
	t.Parallel()

	called := false

	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", upstreamProtocol: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			called = true
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":4,"totalTokenCount":11},"modelVersion":"gemini-2.5-pro"}`,
				))),
			}, nil
		}),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1beta/models/gemini-2.5-pro:generateContent", map[string]any{
		"model": "gemini-2.5-pro",
		"messages": []map[string]string{
			{"role": "user", "content": "hello"},
		},
		"response_format":  map[string]string{"type": "json_object"},
		"stream_options":   map[string]bool{"include_usage": true},
		"prompt_cache_key": "cache-key-1",
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("upstream should not be called for mismatched client protocol body")
	}
	if !strings.Contains(w.Body.String(), "OpenAI chat completions") {
		t.Fatalf("expected protocol mismatch error, got %s", w.Body.String())
	}
}

func TestProxy_AutomaticProtocolFallback_UsesNativeProtocolFirst(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotAuth string
	var gotAPIKey string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "anthropic-ch", upstreamProtocol: "anthropic", protocolTransformMode: model.ProtocolTransformModeAuto, models: "gpt-4o", apiKey: "sk-openai-upstream"},
	}, map[int]string{0: "https://openai-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			gotAPIKey = r.Header.Get("x-api-key")
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"id":"chatcmpl-upstream","object":"chat.completion","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"openai native"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
				))),
			}, nil
		}),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()
	candidates, err := env.server.selectCandidatesByModelAndClientProtocol(context.Background(), "gpt-4o", "openai")
	if err != nil {
		t.Fatalf("selectCandidatesByModelAndClientProtocol failed: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}
	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4o",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("expected native OpenAI upstream path, got %s", gotPath)
	}
	if gotAuth != "Bearer sk-openai-upstream" {
		t.Fatalf("expected bearer auth header, got %q", gotAuth)
	}
	if gotAPIKey != "sk-openai-upstream" {
		t.Fatalf("expected configured x-api-key header, got %q", gotAPIKey)
	}
	if !bytes.Contains(gotBody, []byte(`"messages"`)) {
		t.Fatalf("expected OpenAI request body, got %s", gotBody)
	}

	var resp struct {
		Object string `json:"object"`
		Model  string `json:"model"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Object != "chat.completion" || resp.Model != "gpt-4o" {
		t.Fatalf("expected response translated back to OpenAI, got %+v", resp)
	}
}

func TestProxy_Success_Streaming_OpenAIToAnthropicTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "anthropic-ch", upstreamProtocol: "anthropic", models: "claude-3-5-sonnet", apiKey: "sk-ant"},
	}, map[int]string{0: "https://anthropic-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1/messages", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			body := bytes.NewBufferString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-3-5-sonnet\",\"stop_reason\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"Hello\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\" World\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
				},
				Body: io.NopCloser(body),
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "claude-3-5-sonnet",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("expected anthropic messages path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"messages"`)) || !bytes.Contains(gotBody, []byte(`"text":"hi"`)) {
		t.Fatalf("expected anthropic request body, got %s", gotBody)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"chat.completion.chunk"`) || !strings.Contains(body, `"content":"Hello"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("unexpected translated stream: %s", body)
	}
}

func TestProxy_StreamingOpenAIClientNormalizesAnthropicSSEFromUpstreamMode(t *testing.T) {
	t.Parallel()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "anthropic-upstream-mode", upstreamProtocol: "anthropic", protocolTransformMode: model.ProtocolTransformModeUpstream, models: "mimo-v2.5", apiKey: "sk-ant"},
	}, map[int]string{0: "https://anthropic-upstream.example.com"})

	var gotPath string
	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			body := bytes.NewBufferString(
				"event: message_start\n" +
					"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"mimo-v2.5\",\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
					"event: content_block_start\n" +
					"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
					"event: content_block_delta\n" +
					"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"think\"}}\n\n" +
					"event: content_block_stop\n" +
					"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
					"event: message_delta\n" +
					"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n" +
					"event: message_stop\n" +
					"data: {\"type\":\"message_stop\"}\n\n",
			)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(body),
			}, nil
		}),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "mimo-v2.5",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("expected upstream mode to keep OpenAI path, got %s", gotPath)
	}
	body := w.Body.String()
	if strings.Contains(body, "event: content_block_delta") || strings.Contains(body, `"type":"thinking_delta"`) {
		t.Fatalf("expected OpenAI SSE, got raw Anthropic SSE: %s", body)
	}
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) || !strings.Contains(body, `"reasoning_content":"think"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("unexpected normalized OpenAI SSE: %s", body)
	}
}

func TestProxy_StreamingAnthropicClientNormalizesOpenAISSEFromUpstreamMode(t *testing.T) {
	t.Parallel()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "openai-upstream-mode", upstreamProtocol: "openai", protocolTransformMode: model.ProtocolTransformModeUpstream, models: "gpt-4o", apiKey: "sk-oai"},
	}, map[int]string{0: "https://openai-upstream.example.com"})

	var gotPath string
	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			body := bytes.NewBufferString(
				"data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n" +
					"data: [DONE]\n\n",
			)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(body),
			}, nil
		}),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model":  "gpt-4o",
		"stream": true,
		"messages": []map[string]any{{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": "hi"}},
		}},
	}, map[string]string{"anthropic-version": "2023-06-01"})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("expected upstream mode to keep Anthropic path, got %s", gotPath)
	}
	body := w.Body.String()
	if strings.Contains(body, `"chat.completion.chunk"`) || strings.Contains(body, `"choices"`) {
		t.Fatalf("expected Anthropic SSE, got raw OpenAI SSE: %s", body)
	}
	if !strings.Contains(body, "event: message_start") ||
		!strings.Contains(body, `"type":"text_delta"`) ||
		!strings.Contains(body, `"text":"Hello"`) ||
		!strings.Contains(body, "event: message_stop") {
		t.Fatalf("unexpected normalized Anthropic SSE: %s", body)
	}
}

func TestProxy_StreamingGeminiClientNormalizesOpenAISSEFromUpstreamMode(t *testing.T) {
	t.Parallel()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "openai-upstream-mode", upstreamProtocol: "openai", protocolTransformMode: model.ProtocolTransformModeUpstream, models: "gpt-4o", apiKey: "sk-oai"},
	}, map[int]string{0: "https://openai-upstream.example.com"})

	var gotPath string
	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			body := bytes.NewBufferString(
				"data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\n" +
					"data: [DONE]\n\n",
			)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(body),
			}, nil
		}),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1beta/models/gpt-4o:streamGenerateContent", map[string]any{
		"contents": []map[string]any{{
			"role":  "user",
			"parts": []map[string]any{{"text": "hi"}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/gpt-4o:streamGenerateContent" {
		t.Fatalf("expected upstream mode to keep Gemini path, got %s", gotPath)
	}
	body := w.Body.String()
	if strings.Contains(body, `"chat.completion.chunk"`) || strings.Contains(body, `"choices"`) {
		t.Fatalf("expected Gemini SSE, got raw OpenAI SSE: %s", body)
	}
	if !strings.Contains(body, `"text":"Hello"`) || !strings.Contains(body, `"finishReason":"STOP"`) {
		t.Fatalf("unexpected normalized Gemini SSE: %s", body)
	}
}

func TestProxy_Success_NonStreaming_CodexToAnthropicTransform(t *testing.T) {
	t.Parallel()

	runCodexNonStreamingLocalTransform(t, codexNonStreamingLocalTransformCase{
		channelName:      "anthropic-ch",
		upstreamProtocol: "anthropic",
		modelName:        "claude-3-5-sonnet",
		apiKey:           "sk-ant",
		upstreamURL:      "https://anthropic-upstream.example.com",
		upstreamBody:     `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello from anthropic"}],"model":"claude-3-5-sonnet","stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":4}}`,
		wantPath:         "/v1/messages",
		wantRequestText:  "hi",
		wantText:         "hello from anthropic",
	})
}

type codexNonStreamingLocalTransformCase struct {
	channelName      string
	upstreamProtocol string
	modelName        string
	apiKey           string
	upstreamURL      string
	upstreamBody     string
	wantPath         string
	wantRequestText  string
	wantText         string
}

func runCodexNonStreamingLocalTransform(t *testing.T, tc codexNonStreamingLocalTransformCase) {
	t.Helper()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: tc.channelName, upstreamProtocol: tc.upstreamProtocol, models: tc.modelName, apiKey: tc.apiKey},
	}, map[int]string{0: tc.upstreamURL})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath(tc.wantPath, roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(bytes.NewReader([]byte(tc.upstreamBody))),
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": tc.modelName,
		"input": []map[string]any{{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "hi"}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != tc.wantPath {
		t.Fatalf("expected upstream path %s, got %s", tc.wantPath, gotPath)
	}
	assertChatRequestUserText(t, gotBody, tc.wantRequestText)
	assertCodexResponseText(t, w.Body.Bytes(), tc.wantText)
}

func assertChatRequestUserText(t *testing.T, body []byte, want string) {
	t.Helper()

	var request struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("unmarshal chat request: %v; body=%s", err, body)
	}
	for _, message := range request.Messages {
		if message.Role != "user" {
			continue
		}
		switch content := message.Content.(type) {
		case string:
			if content == want {
				return
			}
		case []any:
			for _, rawBlock := range content {
				block, _ := rawBlock.(map[string]any)
				if text, _ := block["text"].(string); text == want {
					return
				}
			}
		}
	}
	t.Fatalf("expected user text %q in chat request, got %s", want, body)
}

func assertCodexResponseText(t *testing.T, body []byte, want string) {
	t.Helper()

	var resp struct {
		Object string `json:"object"`
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Object != "response" || len(resp.Output) != 1 || len(resp.Output[0].Content) != 1 || resp.Output[0].Content[0].Text != want {
		t.Fatalf("unexpected translated codex response: %s", body)
	}
}

func TestProxy_Success_NonStreaming_CodexBareMessageToAnthropicTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "anthropic-ch", upstreamProtocol: "anthropic", models: "claude-3-5-sonnet", apiKey: "sk-ant"},
	}, map[int]string{0: "https://anthropic-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1/messages", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(bytes.NewReader([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello from anthropic"}],"model":"claude-3-5-sonnet","stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":4}}`))),
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "claude-3-5-sonnet",
		"input": []map[string]any{{
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "hi"}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("expected anthropic messages path, got %s", gotPath)
	}
	assertChatRequestUserText(t, gotBody, "hi")
}

func TestProxy_Success_Streaming_CodexToAnthropicTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "anthropic-ch", upstreamProtocol: "anthropic", models: "claude-3-5-sonnet", apiKey: "sk-ant"},
	}, map[int]string{0: "https://anthropic-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1/messages", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			body := bytes.NewBufferString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"claude-3-5-sonnet\",\"stop_reason\":null,\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"Hello\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\" World\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/event-stream"},
				},
				Body: io.NopCloser(body),
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":  "claude-3-5-sonnet",
		"stream": true,
		"input": []map[string]any{{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "hi"}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("expected anthropic messages path, got %s", gotPath)
	}
	assertChatRequestUserText(t, gotBody, "hi")
	body := w.Body.String()
	if !strings.Contains(body, "event: response.output_text.delta") || !strings.Contains(body, `"delta":"Hello"`) {
		t.Fatalf("expected codex stream delta event, got %s", body)
	}
	if !strings.Contains(body, "event: response.completed") {
		t.Fatalf("expected codex completed event, got %s", body)
	}
}

func TestProxy_Success_NonStreaming_OpenAIToCodexTransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	var gotBody []byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "codex-ch", upstreamProtocol: "codex", models: "gpt-5-codex", apiKey: "sk-codex"},
	}, map[int]string{0: "https://codex-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1/responses", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5-codex","output":[{"type":"reasoning","content":[],"encrypted_content":"internal-codex-payload"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello from codex"}]}],"usage":{"input_tokens":7,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens":4,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":11}}`,
				))),
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-5-codex",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("expected codex responses path, got %s", gotPath)
	}
	if !bytes.Contains(gotBody, []byte(`"type":"input_text"`)) {
		t.Fatalf("expected codex request body, got %s", gotBody)
	}

	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "hello from codex" {
		t.Fatalf("unexpected translated response: %s", w.Body.String())
	}
	if reasoning := gjson.GetBytes(w.Body.Bytes(), "choices.0.message.reasoning"); reasoning.Exists() {
		t.Fatalf("Codex encrypted reasoning leaked into OpenAI response: %s", w.Body.String())
	}
	cachedCreation := gjson.GetBytes(w.Body.Bytes(), "usage.prompt_tokens_details.cached_creation_tokens")
	if !cachedCreation.Exists() || cachedCreation.Int() != 0 {
		t.Fatalf("cached_creation_tokens = %s, want explicit 0: %s", cachedCreation.Raw, w.Body.String())
	}
	if legacy := gjson.GetBytes(w.Body.Bytes(), "usage.cache_creation_input_tokens"); legacy.Exists() {
		t.Fatalf("legacy cache_creation_input_tokens leaked into OpenAI response: %s", w.Body.String())
	}
}

func TestProxy_CodexInvalidEncryptedContentRetriesWithoutEncryptedInputItems(t *testing.T) {
	t.Parallel()

	const invalidEncryptedContentBody = `{"error":{"message":"The encrypted content could not be verified. Reason: Encrypted content could not be decrypted or parsed.","type":"invalid_request_error","param":"","code":"invalid_encrypted_content"}}`

	var attempts atomic.Int32
	var bodies [][]byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "codex-ch", upstreamProtocol: "codex", models: "gpt-5.5", apiKey: "sk-codex"},
	}, map[int]string{0: "https://codex-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, body)
			if attempts.Add(1) == 1 {
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(bytes.NewReader([]byte(invalidEncryptedContentBody))),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`,
				))),
			}, nil
		}),
	}

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-5.5",
		"input": []map[string]any{
			{"type": "compaction", "encrypted_content": "drop-compaction"},
			{"type": "reasoning", "summary": []any{}, "content": nil, "encrypted_content": "drop-reasoning"},
			{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}},
		},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected retry success, got %d: %s", w.Code, w.Body.String())
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts=%d, want 2", attempts.Load())
	}
	if len(bodies) != 2 {
		t.Fatalf("captured bodies=%d, want 2", len(bodies))
	}
	if !bytes.Contains(bodies[0], []byte(`"type":"reasoning"`)) {
		t.Fatalf("first request should include reasoning item, got %s", bodies[0])
	}
	if bytes.Contains(bodies[1], []byte(`"type":"reasoning"`)) {
		t.Fatalf("retry request should remove reasoning item, got %s", bodies[1])
	}
	if bytes.Contains(bodies[1], []byte(`"encrypted_content"`)) ||
		bytes.Contains(bodies[1], []byte(`"type":"compaction"`)) {
		t.Fatalf("retry request should remove encrypted input items, got %s", bodies[1])
	}
	if !bytes.Contains(bodies[1], []byte(`"type":"message"`)) {
		t.Fatalf("retry request should keep non-encrypted input items, got %s", bodies[1])
	}
}

func TestProxy_Codex400RetriesWithoutThinkingAndLogsStrategy(t *testing.T) {
	t.Parallel()

	const unsupportedThinkingBody = `{"error":{"message":"unsupported parameter: reasoning","type":"invalid_request_error","param":"reasoning","code":"unsupported_parameter"}}`

	var attempts atomic.Int32
	var bodies [][]byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "codex-no-thinking", upstreamProtocol: "codex", models: "gpt-5-codex", apiKey: "sk-codex"},
	}, map[int]string{0: "https://codex-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, body)
			if attempts.Add(1) == 1 {
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(bytes.NewReader([]byte(unsupportedThinkingBody))),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5-codex","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`,
				))),
			}, nil
		}),
	}

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-5-codex",
		"input": []map[string]any{
			{"type": "reasoning", "summary": []any{}, "content": []map[string]any{{"type": "reasoning_text", "text": "drop thinking"}}},
			{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}},
		},
		"reasoning": map[string]any{"effort": "medium", "summary": "auto"},
		"include":   []string{"reasoning.encrypted_content"},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected retry success, got %d: %s", w.Code, w.Body.String())
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts=%d, want 2", attempts.Load())
	}
	if len(bodies) != 2 {
		t.Fatalf("captured bodies=%d, want 2", len(bodies))
	}
	if !bytes.Contains(bodies[0], []byte(`"reasoning"`)) ||
		!bytes.Contains(bodies[0], []byte(`"include"`)) {
		t.Fatalf("first request should include thinking controls, got %s", bodies[0])
	}
	if bytes.Contains(bodies[1], []byte(`"reasoning"`)) ||
		bytes.Contains(bodies[1], []byte(`"include"`)) {
		t.Fatalf("retry request should strip thinking controls, got %s", bodies[1])
	}
	if !bytes.Contains(bodies[1], []byte(`"type":"message"`)) {
		t.Fatalf("retry request should keep message input, got %s", bodies[1])
	}

	ctx := context.Background()
	since := time.Now().Add(-time.Minute)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		logs, err := env.store.ListLogs(ctx, since, 20, 0, &model.LogFilter{LogSource: model.LogSourceProxy})
		if err != nil {
			t.Fatalf("ListLogs failed: %v", err)
		}
		for _, entry := range logs {
			if entry.StatusCode == http.StatusOK && entry.ChannelID != 0 {
				if !strings.Contains(entry.Message, "[strip_codex_thinking]") {
					t.Fatalf("success log message=%q, want retry strategy", entry.Message)
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("expected successful proxy log with retry strategy within deadline")
}

func TestProxy_CodexNormalizesToolSearchArgumentsBeforeForward(t *testing.T) {
	t.Parallel()

	var upstreamBody []byte
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		upstreamBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "codex-normalize", upstreamProtocol: "codex", models: "gpt-5.5", apiKey: "sk-codex"},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-5.5",
		"input": []map[string]any{
			{"type": "tool_search_call", "call_id": "search_valid", "status": "completed", "arguments": `{"query":"codegraph_explore","limit":5}`},
			{"type": "tool_search_call", "call_id": "search_object", "status": "completed", "arguments": map[string]any{"query": "already_object"}},
			{"type": "tool_search_call", "call_id": "search_bad", "status": "completed", "arguments": `{bad json`},
			{"type": "tool_search_call", "call_id": "search_number", "status": "completed", "arguments": 7},
			{"type": "tool_search_output", "call_id": "search_valid", "status": "completed", "tools": []any{}},
			{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}},
		},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var sent struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(upstreamBody, &sent); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}

	foundValid := false
	foundObject := false
	foundBad := false
	foundNumber := false
	foundOutput := false
	for _, item := range sent.Input {
		switch item["call_id"] {
		case "search_valid":
			if item["type"] == "tool_search_output" {
				foundOutput = true
				continue
			}
			args, ok := item["arguments"].(map[string]any)
			if !ok {
				t.Fatalf("tool_search_call arguments type=%T, want object in %s", item["arguments"], upstreamBody)
			}
			if args["query"] != "codegraph_explore" || int(args["limit"].(float64)) != 5 {
				t.Fatalf("unexpected normalized arguments: %#v", args)
			}
			foundValid = true
		case "search_object":
			args, ok := item["arguments"].(map[string]any)
			if !ok || args["query"] != "already_object" {
				t.Fatalf("object arguments should be preserved, got %#v", item["arguments"])
			}
			foundObject = true
		case "search_bad":
			foundBad = true
		case "search_number":
			foundNumber = true
		}
	}
	if !foundValid {
		t.Fatalf("valid tool_search_call missing from upstream body: %s", upstreamBody)
	}
	if !foundObject {
		t.Fatalf("object tool_search_call missing from upstream body: %s", upstreamBody)
	}
	if foundBad {
		t.Fatalf("invalid tool_search_call should be removed from upstream body: %s", upstreamBody)
	}
	if foundNumber {
		t.Fatalf("non-object tool_search_call arguments should be removed from upstream body: %s", upstreamBody)
	}
	if !foundOutput {
		t.Fatalf("tool_search_output without arguments should be preserved: %s", upstreamBody)
	}
}

func TestProxy_AnyrouterCodexStripsToolSearchBeforeForwardThenRetriesEncryptedContent(t *testing.T) {
	t.Parallel()

	const invalidResponsesRequestBody = `{"error":{"message":"invalid codex request (request id: req_test)","type":"new_api_error","param":"","code":"invalid_responses_request"}}`

	var attempts atomic.Int32
	var bodies [][]byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "anyrouter-codex", upstreamProtocol: "codex", models: "gpt-5.5", apiKey: "sk-codex"},
	}, map[int]string{0: "https://codex-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, body)
			switch attempts.Add(1) {
			case 1:
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(bytes.NewReader([]byte(invalidResponsesRequestBody))),
				}, nil
			default:
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(bytes.NewReader([]byte(
						`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`,
					))),
				}, nil
			}
		}),
	}

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-5.5",
		"input": []map[string]any{
			{"type": "reasoning", "summary": []any{}, "encrypted_content": "drop-reasoning"},
			{"type": "tool_search_call", "call_id": "search_1", "status": "completed", "arguments": `{"query":"x"}`},
			{"type": "tool_search_output", "call_id": "search_1", "status": "completed", "tools": []any{}},
			{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}},
		},
		"include": []string{"reasoning.encrypted_content"},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected retry success, got %d: %s", w.Code, w.Body.String())
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts=%d, want 2", attempts.Load())
	}
	if len(bodies) != 2 {
		t.Fatalf("captured bodies=%d, want 2", len(bodies))
	}
	if !bytes.Contains(bodies[0], []byte(`"encrypted_content"`)) ||
		bytes.Contains(bodies[0], []byte(`"type":"tool_search_call"`)) ||
		bytes.Contains(bodies[0], []byte(`"type":"tool_search_output"`)) {
		t.Fatalf("first request should strip tool search history before forwarding, got %s", bodies[0])
	}
	if bytes.Contains(bodies[1], []byte(`"encrypted_content"`)) ||
		bytes.Contains(bodies[1], []byte(`"type":"tool_search_call"`)) ||
		bytes.Contains(bodies[1], []byte(`"type":"tool_search_output"`)) {
		t.Fatalf("second request should remove encrypted content after sanitized anyrouter request fails, got %s", bodies[1])
	}
	if !bytes.Contains(bodies[1], []byte(`"type":"reasoning"`)) ||
		!bytes.Contains(bodies[1], []byte(`"type":"message"`)) {
		t.Fatalf("second request should keep sanitized reasoning summary and messages, got %s", bodies[1])
	}
}

func TestProxy_CodexInvalidEncryptedContentWrappedRequestErrorRetries(t *testing.T) {
	t.Parallel()

	const wrappedInvalidEncryptedContentBody = `{"error":{"message":"all 2 attempts failed: HTTP 400: {\"error\":{\"message\":\"The encrypted content gAAA...fnaA could not be verified. Reason: Encrypted content could not be decrypted or parsed.\",\"type\":\"invalid_request_error\",\"param\":\"\",\"code\":\"invalid_encrypted_content\"}}","type":"request_error"}}`

	var attempts atomic.Int32
	var bodies [][]byte

	env := setupProxyTestEnv(t, []testChannel{
		{name: "codex-ch", upstreamProtocol: "codex", models: "gpt-5.5", apiKey: "sk-codex"},
	}, map[int]string{0: "https://codex-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(r.Body)
			bodies = append(bodies, body)
			if attempts.Add(1) == 1 {
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(bytes.NewReader([]byte(wrappedInvalidEncryptedContentBody))),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`,
				))),
			}, nil
		}),
	}

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-5.5",
		"input": []map[string]any{
			{"type": "reasoning", "summary": []any{}, "content": nil, "encrypted_content": "drop-reasoning"},
			{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}},
		},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected retry success, got %d: %s", w.Code, w.Body.String())
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts=%d, want 2", attempts.Load())
	}
	if len(bodies) != 2 {
		t.Fatalf("captured bodies=%d, want 2", len(bodies))
	}
	if bytes.Contains(bodies[1], []byte(`"type":"reasoning"`)) ||
		bytes.Contains(bodies[1], []byte(`"encrypted_content"`)) {
		t.Fatalf("retry request should remove encrypted thinking state, got %s", bodies[1])
	}
	if !bytes.Contains(bodies[1], []byte(`"type":"message"`)) {
		t.Fatalf("retry request should keep non-encrypted input items, got %s", bodies[1])
	}
}

func TestProxy_CodexInvalidEncryptedContentRetryFailureReturnsUpstreamError(t *testing.T) {
	t.Parallel()

	const firstError = `{"error":{"message":"The encrypted content could not be verified. Reason: Encrypted content could not be decrypted or parsed.","type":"invalid_request_error","param":"","code":"invalid_encrypted_content"}}`
	const secondError = `{"error":{"message":"still invalid after retry","type":"invalid_request_error","code":"invalid_encrypted_content"}}`

	var attempts atomic.Int32

	env := setupProxyTestEnv(t, []testChannel{
		{name: "codex-ch", upstreamProtocol: "codex", models: "gpt-5.5", apiKey: "sk-codex"},
	}, map[int]string{0: "https://codex-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
			body := firstError
			if attempts.Add(1) == 2 {
				body = secondError
			}
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewReader([]byte(body))),
			}, nil
		}),
	}

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gpt-5.5",
		"input": []map[string]any{
			{"type": "reasoning", "summary": []any{}, "content": nil, "encrypted_content": "drop-reasoning"},
			{"type": "message", "role": "user", "content": []map[string]any{{"type": "input_text", "text": "hi"}}},
		},
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected retry failure to return upstream 400, got %d: %s", w.Code, w.Body.String())
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts=%d, want 2", attempts.Load())
	}
	if !strings.Contains(w.Body.String(), "still invalid after retry") {
		t.Fatalf("expected second upstream error body, got %s", w.Body.String())
	}
}

func TestProxy_Success_Streaming_OpenAIToCodexTransformWithoutContentType(t *testing.T) {
	var gotPath string
	upstreamBody := newDataThenBlockReadCloser([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5-codex\",\"usage\":{\"input_tokens\":7,\"output_tokens\":4,\"total_tokens\":11}}}\n\n"), 7)
	defer func() { _ = upstreamBody.Close() }()
	env := setupProxyTestEnv(t, []testChannel{
		{name: "codex-ch", upstreamProtocol: "codex", models: "gpt-5-codex", apiKey: "sk-codex"},
	}, map[int]string{0: "https://codex-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1/responses", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{},
				Body:       upstreamBody,
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model":    "gpt-5-codex",
			"stream":   true,
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, nil)
	}()

	var w *httptest.ResponseRecorder
	select {
	case w = <-done:
	case <-time.After(2 * time.Second):
		_ = upstreamBody.Close()
		<-done
		t.Fatal("translated Responses stream waited for upstream EOF after response.completed")
	}

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("expected codex responses path, got %s", gotPath)
	}
	if got := w.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"chat.completion.chunk"`) || !strings.Contains(body, `"content":"Hello"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("expected openai stream chunk, got %s", body)
	}
	select {
	case <-upstreamBody.closed:
	default:
		t.Fatal("translated stream returned without closing the upstream body")
	}
}

func TestProxy_Success_Streaming_CodexCompletedWithoutEOF(t *testing.T) {
	sse := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5-codex\",\"usage\":{\"input_tokens\":7,\"output_tokens\":4,\"total_tokens\":11}}}\n\n")
	trailing := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"late\"}\n\n")

	for _, contentType := range []string{"text/event-stream", "text/plain; charset=utf-8"} {
		t.Run(contentType, func(t *testing.T) {
			upstreamData := append(append([]byte(nil), sse...), trailing...)
			upstreamBody := newDataThenBlockReadCloser(upstreamData, len(upstreamData))
			defer func() { _ = upstreamBody.Close() }()

			env := setupProxyTestEnv(t, []testChannel{
				{name: "codex-no-eof", upstreamProtocol: "codex", models: "gpt-5-codex", apiKey: "sk-codex"},
			}, map[int]string{0: "https://codex-upstream.example.com"})
			env.server.client = &http.Client{
				Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{contentType}},
						Body:       upstreamBody,
					}, nil
				}),
			}

			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
					"model":  "gpt-5-codex",
					"stream": true,
					"input":  "hi",
				}, nil)
			}()

			var w *httptest.ResponseRecorder
			select {
			case w = <-done:
			case <-time.After(2 * time.Second):
				_ = upstreamBody.Close()
				<-done
				t.Fatal("Responses stream waited for upstream EOF after response.completed")
			}

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
			}
			if w.Body.String() != string(sse) {
				t.Fatalf("forwarded SSE mismatch:\n got: %q\nwant: %q", w.Body.String(), string(sse))
			}
			entry := waitForProxyLog(t, env, "gpt-5-codex")
			if entry.InputTokens != 7 || entry.OutputTokens != 4 {
				t.Fatalf("logged usage=(%d,%d), want (7,4)", entry.InputTokens, entry.OutputTokens)
			}
			select {
			case <-upstreamBody.closed:
			default:
				t.Fatal("stream returned without closing the upstream body")
			}
		})
	}
}

func TestProxy_Success_NonStreaming_CodexToOpenAITransform(t *testing.T) {
	t.Parallel()

	runCodexNonStreamingLocalTransform(t, codexNonStreamingLocalTransformCase{
		channelName:      "openai-ch",
		upstreamProtocol: "openai",
		modelName:        "gpt-4o",
		apiKey:           "sk-oai",
		upstreamURL:      "https://openai-upstream.example.com",
		upstreamBody:     `{"id":"chatcmpl_1","object":"chat.completion","created":0,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hello from openai"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":4,"total_tokens":11}}`,
		wantPath:         "/v1/chat/completions",
		wantRequestText:  "hi",
		wantText:         "hello from openai",
	})
}

func TestProxy_Success_Streaming_CodexToOpenAITransform(t *testing.T) {
	t.Parallel()

	var gotPath string
	env := setupProxyTestEnv(t, []testChannel{
		{name: "openai-ch", upstreamProtocol: "openai", models: "gpt-4o", apiKey: "sk-oai"},
	}, map[int]string{0: "https://openai-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1/chat/completions", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			body := bytes.NewBufferString("data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(body),
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":  "gpt-4o",
		"stream": true,
		"input": []map[string]any{{
			"type":    "message",
			"role":    "user",
			"content": []map[string]string{{"type": "input_text", "text": "hi"}},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("expected openai chat completions path, got %s", gotPath)
	}
	body := w.Body.String()
	if !strings.Contains(body, "event: response.output_text.delta") || !strings.Contains(body, `"delta":"Hello"`) || !strings.Contains(body, "event: response.completed") {
		t.Fatalf("expected codex stream output, got %s", body)
	}
}

func TestProxy_GeminiTransform_UsesResolvedActualModelInUpstreamPath(t *testing.T) {
	t.Parallel()

	var gotPath string

	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", upstreamProtocol: "gemini", models: "alias-model", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	cfg.ModelEntries = []model.ModelEntry{{
		Model:         "alias-model",
		RedirectModel: "gemini-2.5-pro",
	}}
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1beta/models/gemini-2.5-pro:generateContent", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotPath = r.URL.Path
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: io.NopCloser(bytes.NewReader([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2},"modelVersion":"gemini-2.5-pro"}`))),
			}, nil
		})),
	}

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "alias-model",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if gotPath != "/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Fatalf("expected resolved actual model path, got %s", gotPath)
	}

	var resp struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Model != "alias-model" {
		t.Fatalf("expected client-visible response model alias-model, got %s", resp.Model)
	}
}

func TestProxy_Success_Streaming_OpenAIToGeminiTransform_TextPlainSSE(t *testing.T) {
	t.Parallel()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", upstreamProtocol: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1beta/models/gemini-2.5-pro:streamGenerateContent", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			body := bytes.NewBufferString("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hello\"}]}}]}\n\ndata: [DONE]\n\n")
			return &http.Response{
				StatusCode: http.StatusOK,
				Header: http.Header{
					"Content-Type": []string{"text/plain; charset=utf-8"},
				},
				Body: io.NopCloser(body),
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gemini-2.5-pro",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"chat.completion.chunk"`) || !strings.Contains(body, `"content":"Hello"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("unexpected translated text/plain SSE stream: %s", body)
	}
}

func TestProxy_StructuredOpenAIImageTransformHitsUpstream(t *testing.T) {
	t.Parallel()

	var gotBody []byte
	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", upstreamProtocol: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1beta/models/gemini-2.5-pro:generateContent", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5},"modelVersion":"gemini-2.5-pro"}`,
				))),
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "gemini-2.5-pro",
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": "hi"},
				{"type": "image_url", "image_url": map[string]any{"url": "https://example.com/a.png"}},
			},
		}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(gotBody, []byte(`"fileUri":"https://example.com/a.png"`)) {
		t.Fatalf("expected structured image request to reach upstream, got %s", gotBody)
	}
}

func TestProxy_StructuredAnthropicBlocksTransformHitsUpstream(t *testing.T) {
	t.Parallel()

	var gotBody []byte
	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", upstreamProtocol: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1beta/models/gemini-2.5-pro:generateContent", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5},"modelVersion":"gemini-2.5-pro"}`,
				))),
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "gemini-2.5-pro",
		"messages": []map[string]any{
			{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": map[string]any{"query": "go"}}}},
			{"role": "user", "content": []map[string]any{
				{"type": "document", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": "cGRm"}},
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": "done"},
			}},
		},
	}, map[string]string{"anthropic-version": "2023-06-01"})

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(gotBody, []byte(`"functionCall"`)) || !bytes.Contains(gotBody, []byte(`"functionResponse"`)) || !bytes.Contains(gotBody, []byte(`"inlineData"`)) {
		t.Fatalf("expected structured anthropic blocks to reach upstream, got %s", gotBody)
	}
}

func TestProxy_StructuredCodexFunctionFamilyTransformHitsUpstream(t *testing.T) {
	t.Parallel()

	var gotBody []byte
	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", upstreamProtocol: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1beta/models/gemini-2.5-pro:generateContent", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			gotBody, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(bytes.NewReader([]byte(
					`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5},"modelVersion":"gemini-2.5-pro"}`,
				))),
			}, nil
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gemini-2.5-pro",
		"input": []map[string]any{
			{"type": "message", "role": "user", "content": []map[string]any{
				{"type": "input_image", "image_url": "https://example.com/a.png"},
				{"type": "input_file", "file_id": "file_123", "filename": "doc.pdf"},
			}},
			{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": map[string]any{"query": "go"}},
			{"type": "function_call_output", "call_id": "call_1", "output": "done"},
		},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(gotBody, []byte(`"functionCall"`)) || !bytes.Contains(gotBody, []byte(`"functionResponse"`)) || !bytes.Contains(gotBody, []byte(`"fileUri":"https://example.com/a.png"`)) {
		t.Fatalf("expected structured codex request to reach upstream, got %s", gotBody)
	}
}

func TestProxy_UnsupportedStructuredTransformRequestReturns400(t *testing.T) {
	t.Parallel()

	var called bool
	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", upstreamProtocol: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1beta/models/gemini-2.5-pro:generateContent", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			called = true
			return nil, fmt.Errorf("should not hit upstream")
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "gemini-2.5-pro",
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{{
				"type":  "mystery",
				"value": true,
			}},
		}},
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("upstream should not be called for unsupported structured transform request")
	}
}

func TestProxy_UnsupportedStructuredAnthropicTransformRequestReturns400(t *testing.T) {
	t.Parallel()

	var called bool
	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", upstreamProtocol: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1beta/models/gemini-2.5-pro:generateContent", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			called = true
			return nil, fmt.Errorf("should not hit upstream")
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model": "gemini-2.5-pro",
		"messages": []map[string]any{{
			"role": "user",
			"content": []map[string]any{{
				"type":  "mystery",
				"value": true,
			}},
		}},
	}, map[string]string{"anthropic-version": "2023-06-01"})

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("upstream should not be called for unsupported anthropic structured transform request")
	}
}

func TestProxy_UnsupportedStructuredCodexTransformRequestReturns400(t *testing.T) {
	t.Parallel()

	var called bool
	env := setupProxyTestEnv(t, []testChannel{
		{name: "gemini-ch", upstreamProtocol: "gemini", models: "gemini-2.5-pro", apiKey: "sk-gem"},
	}, map[int]string{0: "https://gemini-upstream.example.com"})

	env.server.client = &http.Client{
		Transport: automaticFallbackToPath("/v1beta/models/gemini-2.5-pro:generateContent", roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			called = true
			return nil, fmt.Errorf("should not hit upstream")
		})),
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	cfg := configs[0]
	if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatalf("UpdateConfig failed: %v", err)
	}
	env.server.InvalidateChannelListCache()

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "gemini-2.5-pro",
		"input": []map[string]any{{
			"type":    "message",
			"role":    "user",
			"content": []map[string]any{{"type": "mystery", "value": true}},
		}},
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("upstream should not be called for unsupported codex structured transform request")
	}
}

func TestProxy_ChannelRetry_On503(t *testing.T) {
	t.Parallel()

	// 渠道1：返回 503
	upstream1 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"service unavailable"}`))
	}))
	defer upstream1.Close()

	// 渠道2：返回 200
	upstream2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-ch2","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1-fail", models: "gpt-4", apiKey: "sk-1", priority: 100},
		{name: "ch2-ok", models: "gpt-4", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstream1.URL, 1: upstream2.URL})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (fallback to ch2), got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxy_NonStreamingEmpty200RetriesNextChannel(t *testing.T) {
	t.Parallel()

	var emptyCalls atomic.Int32
	upstreamEmpty := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		emptyCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstreamEmpty.Close()

	var okCalls atomic.Int32
	upstreamOK := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-ch2","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamOK.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-empty", models: "gpt-4", apiKey: "sk-empty", priority: 100},
		{name: "ch-ok", models: "gpt-4", apiKey: "sk-ok", priority: 50},
	}, map[int]string{
		0: upstreamEmpty.URL,
		1: upstreamOK.URL,
	})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after retrying next channel, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "from-ch2") {
		t.Fatalf("expected response from second channel, got body: %s", w.Body.String())
	}
	if got := emptyCalls.Load(); got != 1 {
		t.Fatalf("empty upstream calls=%d, want 1", got)
	}
	if got := okCalls.Load(); got != 1 {
		t.Fatalf("ok upstream calls=%d, want 1", got)
	}
}

func TestProxy_StreamingEmpty200RetriesNextChannel(t *testing.T) {
	t.Parallel()

	var emptyCalls atomic.Int32
	upstreamEmpty := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		emptyCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstreamEmpty.Close()

	var okCalls atomic.Int32
	upstreamOK := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"id\":\"from-ch2\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstreamOK.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-empty-stream", models: "gpt-4", apiKey: "sk-empty", priority: 100},
		{name: "ch-ok-stream", models: "gpt-4", apiKey: "sk-ok", priority: 50},
	}, map[int]string{
		0: upstreamEmpty.URL,
		1: upstreamOK.URL,
	})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after retrying next streaming channel, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "from-ch2") {
		t.Fatalf("expected stream response from second channel, got body: %s", w.Body.String())
	}
	if got := emptyCalls.Load(); got != 1 {
		t.Fatalf("empty upstream calls=%d, want 1", got)
	}
	if got := okCalls.Load(); got != 1 {
		t.Fatalf("ok upstream calls=%d, want 1", got)
	}
}

func TestProxy_StreamingPingOnly200RetriesNextChannel(t *testing.T) {
	t.Parallel()

	var pingCalls atomic.Int32
	upstreamPing := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pingCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "event: ping\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"ping\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstreamPing.Close()

	var okCalls atomic.Int32
	upstreamOK := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "data: {\"id\":\"from-ch2\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstreamOK.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-ping-stream", models: "gpt-4", apiKey: "sk-ping", priority: 100},
		{name: "ch-ok-stream", models: "gpt-4", apiKey: "sk-ok", priority: 50},
	}, map[int]string{
		0: upstreamPing.URL,
		1: upstreamOK.URL,
	})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after retrying next streaming channel, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "from-ch2") {
		t.Fatalf("expected stream response from second channel, got body: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"type":"ping"`) {
		t.Fatalf("expected ping-only response not to leak to client, got body: %s", w.Body.String())
	}
	if got := pingCalls.Load(); got != 1 {
		t.Fatalf("ping upstream calls=%d, want 1", got)
	}
	if got := okCalls.Load(); got != 1 {
		t.Fatalf("ok upstream calls=%d, want 1", got)
	}
}

func TestProxy_MultiURL5xx_SwitchesToNextChannel(t *testing.T) {
	t.Parallel()

	var ch1FailCalls atomic.Int64
	var ch1SecondURLCalls atomic.Int64
	var ch2Calls atomic.Int64

	// 渠道1 URL1: 固定 503
	upstreamFail := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch1FailCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"service unavailable"}`))
	}))
	defer upstreamFail.Close()

	// 渠道1 URL2: 即使可用也不应被尝试（新策略：5xx 直接切渠道）
	upstreamShouldSkip := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch1SecondURLCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-ch1-url2","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamShouldSkip.Close()

	// 渠道2: 正常返回，用于验证“切换到下一个渠道”
	upstreamCh2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch2Calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-ch2","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamCh2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{
			name: "ch-multi-url", models: "gpt-4", apiKey: "sk-1", priority: 100,
			cooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
				Enabled: true, Name: "Provider unavailable", Priority: 0, StatusCodes: []int{http.StatusServiceUnavailable},
				Scope: model.CooldownScopeChannel, Mode: model.CooldownModeFixed, CooldownSeconds: 120,
			}}},
		},
		{name: "ch-fallback", models: "gpt-4", apiKey: "sk-2", priority: 50},
	}, map[int]string{
		0: upstreamFail.URL + "\n" + upstreamShouldSkip.URL,
		1: upstreamCh2.URL,
	})

	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("expected 2 config, got %d", len(configs))
	}

	var channelID int64
	for _, cfg := range configs {
		if cfg.Name == "ch-multi-url" {
			channelID = cfg.ID
			break
		}
	}
	if channelID == 0 {
		t.Fatalf("ch-multi-url not found in configs")
	}

	// 强制渠道1首跳命中失败URL，避免随机首跳影响稳定性
	env.server.urlSelector.CooldownURL(channelID, upstreamShouldSkip.URL)

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "from-ch2") {
		t.Fatalf("expected switch to next channel, got body: %s", w.Body.String())
	}
	ch1Fail := ch1FailCalls.Load()
	ch1Second := ch1SecondURLCalls.Load()
	ch2 := ch2Calls.Load()
	if ch1Fail < 1 {
		t.Fatalf("expected channel1 first URL attempted, got %d", ch1Fail)
	}
	if ch1Second != 0 {
		t.Fatalf("expected channel1 second URL not attempted on 5xx, got %d", ch1Second)
	}
	if ch2 < 1 {
		t.Fatalf("expected next channel attempted, got %d", ch2)
	}
	cooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	until, exists := cooldowns[channelID]
	if !exists {
		t.Fatalf("expected configured channel cooldown to be persisted for channel_id=%d", channelID)
	}
	if remaining := time.Until(until); remaining < 115*time.Second || remaining > 125*time.Second {
		t.Fatalf("configured channel cooldown remaining=%v, want about 2m", remaining)
	}
}

func TestProxy_MultiURLFallbackOn598_DoesNotChannelCooldownEarly(t *testing.T) {
	t.Parallel()

	var failCalls atomic.Int64
	var okCalls atomic.Int64

	// URL1: 首字节超时（598）
	upstreamTimeout := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		failCalls.Add(1)
		time.Sleep(120 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstreamTimeout.Close()

	// URL2: 正常返回
	upstreamOK := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"from-url2\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstreamOK.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-multi-url", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{
		0: upstreamTimeout.URL + "\n" + upstreamOK.URL,
	})

	// 缩短首字节超时，稳定触发 598
	env.server.firstByteTimeout = 50 * time.Millisecond

	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	channelID := configs[0].ID

	// 强制 URL2 进入冷却，确保首跳先打到 timeout URL
	env.server.urlSelector.CooldownURL(channelID, upstreamOK.URL)

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "from-url2") {
		t.Fatalf("expected fallback to url2 on 598, got body: %s", w.Body.String())
	}
	fail := failCalls.Load()
	ok := okCalls.Load()
	if fail < 1 || ok < 1 {
		t.Fatalf("expected both URLs attempted, failCalls=%d okCalls=%d", fail, ok)
	}

	// 关键断言：598 触发多URL内部回退成功后，不应残留渠道级冷却
	cooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	if _, exists := cooldowns[channelID]; exists {
		t.Fatalf("unexpected channel cooldown for multi-url fallback success, channel_id=%d", channelID)
	}
}

func TestProxy_StreamTimeoutDoesNotRetryAfterResponseCommit(t *testing.T) {
	t.Parallel()

	upstreamStarted := make(chan struct{})
	upstreamTimedOut := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(upstreamStarted)
		<-r.Context().Done()
	}))
	defer upstreamTimedOut.Close()

	var fallbackCalls atomic.Int64
	upstreamFallback := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"fallback\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstreamFallback.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "timeout", models: "gpt-4", priority: 100},
		{name: "fallback", models: "gpt-4", priority: 1},
	}, map[int]string{0: upstreamTimedOut.URL, 1: upstreamFallback.URL})
	env.server.streamTimeout = 50 * time.Millisecond

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	select {
	case <-upstreamStarted:
	default:
		t.Fatal("timed-out upstream was not selected first")
	}
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "partial") {
		t.Fatalf("response status=%d body=%q, want committed partial stream", response.Code, response.Body.String())
	}
	if calls := fallbackCalls.Load(); calls != 0 {
		t.Fatalf("fallback calls=%d, want 0 after response commit", calls)
	}
	entry := waitForProxyLog(t, env, "gpt-4")
	if entry.StatusCode != util.StatusStreamIncomplete || entry.UpstreamWebsocket {
		t.Fatalf("proxy log status/ws=%d/%v, want ordinary HTTP 599", entry.StatusCode, entry.UpstreamWebsocket)
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("list configs: %v", err)
	}
	var timeoutChannelID int64
	for _, cfg := range configs {
		if cfg.Name == "timeout" {
			timeoutChannelID = cfg.ID
			break
		}
	}
	cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("get model cooldowns: %v", err)
	}
	if until := cooldowns[timeoutChannelID]["gpt-4"]; timeoutChannelID == 0 || !until.After(time.Now()) {
		t.Fatalf("ordinary HTTP 599 did not cool model: channel=%d cooldowns=%v", timeoutChannelID, cooldowns)
	}
}

func TestProxy_MultiURLFirstAttempt_UsesWeightedRandom(t *testing.T) {
	t.Parallel()

	var fastCalls atomic.Int64
	var slowCalls atomic.Int64

	upstreamFast := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fastCalls.Add(1)
		time.Sleep(5 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-fast","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamFast.Close()

	upstreamSlow := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slowCalls.Add(1)
		time.Sleep(30 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-slow","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamSlow.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-weighted-first", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{
		0: upstreamSlow.URL + "\n" + upstreamFast.URL,
	})

	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	channelID := configs[0].ID

	// 预热EWMA，确保不是“未探索优先”分支
	env.server.urlSelector.RecordLatency(channelID, upstreamFast.URL, 5*time.Millisecond)
	env.server.urlSelector.RecordLatency(channelID, upstreamSlow.URL, 30*time.Millisecond)

	const rounds = 120
	for range rounds {
		w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
			"model":    "gpt-4",
			"messages": []map[string]string{{"role": "user", "content": "hi"}},
		}, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
	}

	fast := fastCalls.Load()
	slow := slowCalls.Load()
	if fast <= slow {
		t.Fatalf("expected weighted random to prefer fast URL, fast=%d slow=%d", fast, slow)
	}
	if slow < 5 {
		t.Fatalf("expected slow URL to be selected sometimes (not deterministic first pick), fast=%d slow=%d", fast, slow)
	}
}

func TestProxy_MultiURLProbeCanceledByShutdown_DoesNotPolluteCooldown(t *testing.T) {
	upstreamA := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-a","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamA.Close()

	upstreamB := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"from-b","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstreamB.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-probe-shutdown", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{
		0: upstreamA.URL + "\n" + upstreamB.URL,
	})

	env.server.urlSelector.probeTimeout = 5 * time.Second
	started := make(chan struct{}, 2)
	env.server.urlSelector.probeDial = func(ctx context.Context, _, _ string) (net.Conn, error) {
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("probe dial did not start in time")
		}
	}

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	channelID := configs[0].ID

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := env.server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		env.server.urlSelector.mu.RLock()
		probingLeft := len(env.server.urlSelector.probing)
		env.server.urlSelector.mu.RUnlock()
		if probingLeft == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected probing markers to be cleared after shutdown, got %d", probingLeft)
		}
		time.Sleep(10 * time.Millisecond)
	}

	for _, u := range []string{upstreamA.URL, upstreamB.URL} {
		if env.server.urlSelector.IsCooledDown(channelID, u) {
			t.Fatalf("expected canceled probe not to cooldown url: %s", u)
		}
	}
}

func TestProxy_KeyRetry_On401(t *testing.T) {
	t.Parallel()

	callCount := 0
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		auth := r.Header.Get("Authorization")
		if strings.Contains(auth, "sk-bad") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"authentication_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer upstream.Close()

	// 创建服务器并使用其 store
	srv := newInMemoryServer(t)
	store := srv.store

	ctx := context.Background()
	cfg := &model.Config{
		Name:         "ch1-multikey",
		URLs:         model.ChannelURLs{{URL: upstream.URL}},
		Priority:     100,
		Enabled:      true,
		ModelEntries: []model.ModelEntry{{Model: "gpt-4"}},
	}
	created, err := store.CreateConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("CreateConfig: %v", err)
	}
	err = store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: created.ID, KeyIndex: 0, APIKey: "sk-bad"},
		{ChannelID: created.ID, KeyIndex: 1, APIKey: "sk-good"},
	})
	if err != nil {
		t.Fatalf("CreateAPIKeysBatch: %v", err)
	}

	injectAPIToken(srv.authService, "test-api-key", 0, 1)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	srv.SetupRoutes(engine)

	w := doProxyRequest(t, engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (key retry to sk-good), got %d: %s", w.Code, w.Body.String())
	}
	if callCount < 2 {
		t.Fatalf("expected at least 2 upstream calls (key retry), got %d", callCount)
	}
}

func TestProxy_AllChannelsExhausted(t *testing.T) {
	t.Parallel()

	callCount1 := 0
	upstream1 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount1++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer upstream1.Close()

	callCount2 := 0
	upstream2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount2++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer upstream2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1", priority: 100},
		{name: "ch2", models: "gpt-4", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstream1.URL, 1: upstream2.URL})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	// 所有渠道失败时应返回最后一个错误状态码
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}
	// 关键行为：必须耗尽所有可用渠道，而不是只尝试第一个就返回（避免“假绿”）。
	if callCount1 < 1 || callCount2 < 1 {
		t.Fatalf("expected to try all channels at least once, got upstream1=%d upstream2=%d", callCount1, callCount2)
	}
}

// TestProxy_SingleChannel5xx_SkipsSummaryLog 验证：模型仅有 1 个渠道时，
// 渠道级失败日志已完整反映失败原因，不再写"系统/exhausted backends"汇总日志。
func TestProxy_SingleChannel5xx_SkipsSummaryLog(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "only-ch", models: "gpt-4", apiKey: "sk-1", priority: 100},
	}, map[int]string{0: upstream.URL})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d: %s", w.Code, w.Body.String())
	}

	// 等待异步日志落盘：至少要看到 1 条渠道级失败日志（ChannelID 非零）
	ctx := context.Background()
	since := time.Now().Add(-time.Minute)
	deadline := time.Now().Add(2 * time.Second)
	var logs []*model.LogEntry
	for time.Now().Before(deadline) {
		got, err := env.store.ListLogs(ctx, since, 20, 0, &model.LogFilter{LogSource: model.LogSourceProxy})
		if err != nil {
			t.Fatalf("ListLogs failed: %v", err)
		}
		hasChannelLog := false
		for _, e := range got {
			if e.ChannelID != 0 {
				hasChannelLog = true
				break
			}
		}
		if hasChannelLog {
			logs = got
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if logs == nil {
		t.Fatalf("expected at least one channel-level proxy log within deadline")
	}

	// 关键断言：不能出现"汇总日志"（ChannelID=0 的 Proxy 日志）
	for _, e := range logs {
		if e.ChannelID == 0 {
			t.Fatalf("unexpected summary log (ChannelID=0, message=%q, status=%d) for single-channel failure",
				e.Message, e.StatusCode)
		}
	}
}

func TestProxy_ClientCancel_Returns499(t *testing.T) {
	t.Parallel()

	// 上游延迟响应
	upstreamStarted := make(chan struct{})
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-upstreamStarted:
			// already closed
		default:
			close(upstreamStarted)
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	// 创建可取消的请求
	ctx, cancel := context.WithCancel(context.Background())
	body, _ := json.Marshal(map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-api-key")

	// 等上游请求真的发出后再取消，避免“还没发出去就 cancel”导致语义漂移
	go func() {
		select {
		case <-upstreamStarted:
		case <-time.After(1 * time.Second):
		}
		cancel()
	}()

	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)

	// 客户端取消应返回 499 或超时相关状态
	if w.Code != StatusClientClosedRequest && w.Code != http.StatusGatewayTimeout {
		t.Fatalf("expected 499 or 504, got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxy_ModelNotAllowed_Returns403(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4,gpt-3.5-turbo", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	// 限制 token 只能使用 gpt-3.5-turbo
	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	env.server.authService.authTokenModels[tokenHash] = []string{"gpt-3.5-turbo"}
	env.server.authService.authTokensMux.Unlock()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxy_ChannelRestriction_DenySkipsListedChannel(t *testing.T) {
	t.Parallel()

	var disallowedHits atomic.Int32
	disallowedUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		disallowedHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"disallowed"}`))
	}))
	defer disallowedUpstream.Close()

	var allowedHits atomic.Int32
	allowedUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowedHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"allowed","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer allowedUpstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "disallowed-high-priority", models: "gpt-4", apiKey: "sk-disallowed", priority: 100},
		{name: "allowed-low-priority", models: "gpt-4", apiKey: "sk-allowed", priority: 10},
	}, map[int]string{0: disallowedUpstream.URL, 1: allowedUpstream.URL})

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	var allowedID int64
	var disallowedID int64
	for _, cfg := range configs {
		switch cfg.Name {
		case "allowed-low-priority":
			allowedID = cfg.ID
		case "disallowed-high-priority":
			disallowedID = cfg.ID
		}
	}
	if allowedID == 0 || disallowedID == 0 {
		t.Fatalf("channel ids not found: allowed=%d disallowed=%d", allowedID, disallowedID)
	}

	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	env.server.authService.authTokenChannels[tokenHash] = mustChannelRestriction(t, model.ChannelRestrictionModeDeny, disallowedID)
	env.server.authService.authTokensMux.Unlock()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if got := allowedHits.Load(); got != 1 {
		t.Fatalf("allowed upstream hits=%d, want 1", got)
	}
	if got := disallowedHits.Load(); got != 0 {
		t.Fatalf("disallowed upstream hits=%d, want 0", got)
	}
}

func TestProxy_ChannelRestriction_Returns403WhenNoAllowedCandidate(t *testing.T) {
	t.Parallel()

	var upstreamHits atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "only-channel", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	env.server.authService.authTokenChannels[tokenHash] = mustChannelRestriction(t, model.ChannelRestrictionModeAllow, 999999)
	env.server.authService.authTokensMux.Unlock()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
	if got := upstreamHits.Load(); got != 0 {
		t.Fatalf("upstream hits=%d, want 0", got)
	}
}

func TestProxy_ChannelRestriction_PreservesNoCandidateResponse(t *testing.T) {
	t.Parallel()

	var upstreamHits atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "only-channel", models: "gpt-3.5-turbo", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatalf("ListConfigs failed: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}

	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	env.server.authService.authTokenChannels[tokenHash] = mustChannelRestriction(t, model.ChannelRestrictionModeAllow, configs[0].ID)
	env.server.authService.authTokensMux.Unlock()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
	if got := upstreamHits.Load(); got != 0 {
		t.Fatalf("upstream hits=%d, want 0", got)
	}
}

func TestProxy_CostLimitExceeded_Returns429(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	// 设置 token 费用已超限
	tokenHash := model.HashToken("test-api-key")
	env.server.authService.authTokensMux.Lock()
	env.server.authService.authTokenCostLimits[tokenHash] = tokenCostLimit{
		usedMicroUSD:  200_000, // $0.20
		limitMicroUSD: 100_000, // $0.10 限额
	}
	env.server.authService.authTokensMux.Unlock()

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", w.Code, w.Body.String())
	}

	// 验证错误包含 cost_limit_exceeded
	body := w.Body.String()
	if !strings.Contains(body, "cost_limit_exceeded") {
		t.Fatalf("expected 'cost_limit_exceeded' in body: %s", body)
	}
}

func TestProxy_NoChannels_Returns503(t *testing.T) {
	t.Parallel()

	// 创建没有渠道的环境
	srv := newInMemoryServer(t)
	injectAPIToken(srv.authService, "test-api-key", 0, 1)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	srv.SetupRoutes(engine)

	w := doProxyRequest(t, engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", w.Code, w.Body.String())
	}
}

func TestProxy_SSEErrorEvent_TriggersCooldown(t *testing.T) {
	t.Parallel()

	// 模拟上游：返回 200 + SSE 但包含 error 事件
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// 先正常发几个 chunk，然后发 error
		// 这里首个 chunk 故意做大于 SSEBufferSize，确保代理已经向客户端提交过响应，
		// 后续 error event 才会落到“只能冷却，不能同请求重试”的路径。
		largeContent := strings.Repeat("Hi", SSEBufferSize)
		chunks := []string{
			fmt.Sprintf(`data: {"choices":[{"delta":{"content":"%s"}}]}`, largeContent),
			`event: error` + "\n" + `data: {"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`,
		}
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "%s\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1", models: "gpt-4", apiKey: "sk-1"},
	}, map[int]string{0: upstream.URL})

	ctx := context.Background()
	// 先拿到渠道ID（避免硬编码）
	var channelID int64
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	for _, cfg := range configs {
		if cfg.Name == "ch1" {
			channelID = cfg.ID
			break
		}
	}
	if channelID == 0 {
		t.Fatalf("channel ch1 not found")
	}

	// 预期：请求前没有渠道冷却（否则测试语义不成立）
	beforeCooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns(before): %v", err)
	}
	if _, exists := beforeCooldowns[channelID]; exists {
		t.Fatalf("expected no channel cooldown before request, but found one for channel_id=%d", channelID)
	}

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	// SSE error 事件的处理：HTTP 状态码已经是 200（头部已发送），
	// 但内部应触发冷却逻辑。测试验证响应不崩溃。
	// 响应仍是 200（因为 header 已发送），但内部会记录冷却
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (header already sent), got %d: %s", w.Code, w.Body.String())
	}

	// 关键断言：SSE error 事件必须触发冷却副作用（单Key渠道会升级为渠道级冷却）。
	afterCooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns(after): %v", err)
	}
	until, exists := afterCooldowns[channelID]
	if !exists {
		t.Fatalf("expected channel cooldown to be set after SSE error event, channel_id=%d", channelID)
	}
	if time.Until(until) <= 0 {
		t.Fatalf("expected channel cooldown until in the future, got %v", until)
	}
}

func TestProxy_SSEErrorRuleMatchesOriginalHTTPStatus(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		largeContent := strings.Repeat("Hi", SSEBufferSize)
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", largeContent)
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = fmt.Fprint(w, "event: error\n")
		_, _ = fmt.Fprint(w, `data: {"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{{
		name: "ch-sse-original-status", models: "gpt-4", apiKey: "sk-1",
		cooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
			Enabled: true, Name: "HTTP 200 soft error", Priority: 0, StatusCodes: []int{http.StatusOK},
			MessagePattern: "rate limit exceeded", Scope: model.CooldownScopeChannel,
			Mode: model.CooldownModeFixed, CooldownSeconds: 90,
		}}},
	}}, map[int]string{0: upstream.URL})

	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("ListConfigs() = (%d, %v), want one channel", len(configs), err)
	}
	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model": "gpt-4", "stream": true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	cooldowns, err := env.store.GetAllChannelCooldowns(context.Background())
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	until, exists := cooldowns[configs[0].ID]
	if !exists {
		t.Fatalf("expected configured HTTP 200 rule to cool channel %d", configs[0].ID)
	}
	if remaining := time.Until(until); remaining < 85*time.Second || remaining > 95*time.Second {
		t.Fatalf("configured channel cooldown remaining=%v, want about 90s", remaining)
	}
}

func TestProxy_SSEFreeTierBudgetExceededCoolsOnlyKeyThenPromotesChannel(t *testing.T) {
	t.Parallel()

	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		largeContent := strings.Repeat("Hi", SSEBufferSize)
		chunks := []string{
			fmt.Sprintf(`data: {"choices":[{"delta":{"content":"%s"}}]}`, largeContent),
			`event: error` + "\n" + `data: {"type":"error","error":{"type":"api_error","message":"403 {\"error\":{\"code\":\"FREE_TIER_BUDGET_EXCEEDED\",\"message\":\"Free tier monthly spend limit exceeded. Please upgrade to a paid plan to continue using this service.\"}}"}}`,
		}
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "%s\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch-free-tier-sse", models: "gpt-4", apiKey: "sk-free-tier"},
	}, map[int]string{0: upstream.URL})

	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatalf("ListConfigs: %v", err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected 1 config, got %d", len(configs))
	}
	channelID := configs[0].ID

	before := time.Now()
	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (header already sent), got %d: %s", w.Code, w.Body.String())
	}

	keyCooldowns, err := env.store.GetAllKeyCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllKeyCooldowns: %v", err)
	}
	channelKeyCooldowns := keyCooldowns[channelID]
	if channelKeyCooldowns == nil {
		t.Fatalf("expected key cooldown for channel_id=%d", channelID)
	}
	cooldownUntil, exists := channelKeyCooldowns[0]
	if !exists {
		t.Fatalf("expected key 0 cooldown for channel_id=%d", channelID)
	}
	duration := cooldownUntil.Sub(before)
	if duration < 29*time.Minute+55*time.Second || duration > 30*time.Minute+5*time.Second {
		t.Fatalf("key cooldown duration=%v, want about 30m", duration)
	}

	channelCooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	channelUntil, exists := channelCooldowns[channelID]
	if !exists || !channelUntil.After(time.Now()) {
		t.Fatalf("single-key channel should be cooled after its only key is cooled")
	}
	if channelUntil.Sub(cooldownUntil).Abs() > time.Second {
		t.Fatalf("channel cooldown=%s, want key recovery time %s", channelUntil, cooldownUntil)
	}
}

func TestProxy_SSEErrorEventBeforeClientOutput_RetriesNextChannel(t *testing.T) {
	t.Parallel()

	var firstCalls atomic.Int32
	upstream1 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = fmt.Fprint(w, "event: error\n")
		_, _ = fmt.Fprint(w, "data: "+`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later.","param":null},"sequence_number":2}`+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream1.Close()

	var secondCalls atomic.Int32
	upstream2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		chunks := []string{
			`data: {"choices":[{"delta":{"content":"from-ch2"}}]}`,
			`data: [DONE]`,
		}
		for _, chunk := range chunks {
			_, _ = fmt.Fprintf(w, "%s\n\n", chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer upstream2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "ch1-overloaded", models: "gpt-4", apiKey: "sk-1", priority: 100},
		{name: "ch2-ok", models: "gpt-4", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstream1.URL, 1: upstream2.URL})

	w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "gpt-4",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 after retrying next channel, got %d: %s", w.Code, w.Body.String())
	}

	body := w.Body.String()
	if !strings.Contains(body, "from-ch2") {
		t.Fatalf("expected response body from second channel, got: %s", body)
	}
	if strings.Contains(body, "server_is_overloaded") {
		t.Fatalf("expected first channel SSE error not to leak to client, body: %s", body)
	}
	if firstCalls.Load() != 1 {
		t.Fatalf("expected first channel to be tried once, got %d", firstCalls.Load())
	}
	if secondCalls.Load() != 1 {
		t.Fatalf("expected second channel to be tried once, got %d", secondCalls.Load())
	}
}

func TestProxy_SSEContextLengthExceededReturns400WithoutRetryOrCooldown(t *testing.T) {
	var firstCalls atomic.Int32
	upstream1 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "event: error\n")
		_, _ = fmt.Fprint(w, "data: "+`{"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again.","param":"input"},"sequence_number":2}`+"\n\n")
	}))
	defer upstream1.Close()

	var secondCalls atomic.Int32
	upstream2 := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: "+`{"type":"response.completed","response":{"id":"resp-unexpected","status":"completed","output":[]}}`+"\n\n")
	}))
	defer upstream2.Close()

	env := setupProxyTestEnv(t, []testChannel{
		{name: "context-too-large", upstreamProtocol: "codex", models: "gpt-test", apiKey: "sk-1", priority: 100},
		{name: "must-not-run", upstreamProtocol: "codex", models: "gpt-test", apiKey: "sk-2", priority: 50},
	}, map[int]string{0: upstream1.URL, 1: upstream2.URL})

	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model":  "gpt-test",
		"stream": true,
		"input":  "long conversation",
	}, nil)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", w.Code, w.Body.String())
	}
	if got := gjson.GetBytes(w.Body.Bytes(), "error.code").String(); got != "context_length_exceeded" {
		t.Fatalf("error.code=%q, want context_length_exceeded; body=%s", got, w.Body.String())
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("upstream calls first=%d second=%d, want 1/0", firstCalls.Load(), secondCalls.Load())
	}

	ctx := context.Background()
	keyCooldowns, err := env.store.GetAllKeyCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllKeyCooldowns: %v", err)
	}
	if len(keyCooldowns) != 0 {
		t.Fatalf("key cooldowns=%v, want none", keyCooldowns)
	}
	modelCooldowns, err := env.store.GetAllModelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllModelCooldowns: %v", err)
	}
	if len(modelCooldowns) != 0 {
		t.Fatalf("model cooldowns=%v, want none", modelCooldowns)
	}
	channelCooldowns, err := env.store.GetAllChannelCooldowns(ctx)
	if err != nil {
		t.Fatalf("GetAllChannelCooldowns: %v", err)
	}
	if len(channelCooldowns) != 0 {
		t.Fatalf("channel cooldowns=%v, want none", channelCooldowns)
	}
}
