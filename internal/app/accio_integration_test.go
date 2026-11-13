package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/accioauth"
	"ccLoad/internal/model"
	"ccLoad/internal/testutil"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const accioTestCatalog = `{"success":true,"data":[{"modelList":[{"modelCode":"accio-test-code","modelName":"accio-test","visible":true,"protocol":"openai"}]}]}`
const accioTestCompletion = "data: {\"content\":{\"parts\":[{\"text\":\"hello\"}]}}\n\ndata: {\"content\":{\"role\":\"model\",\"parts\":null},\"turn_complete\":true,\"finish_reason\":\"STOP\",\"usage_metadata\":{\"prompt_token_count\":10,\"candidates_token_count\":7,\"total_token_count\":17}}\n\n"

func accioOAuthStart(t *testing.T, s *Server, region string) (string, *url.URL) {
	t.Helper()
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/accio/oauth/start", map[string]string{"region": region}))
	s.HandleStartAccioOAuth(c)
	if w.Code != http.StatusOK {
		t.Fatalf("start: %d %s", w.Code, w.Body.String())
	}
	var result struct {
		State string `json:"state"`
		URL   string `json:"url"`
	}
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &result)
	u, err := url.Parse(result.URL)
	if err != nil || result.State == "" {
		t.Fatalf("invalid start response: %+v %v", result, err)
	}
	return result.State, u
}

func accioOAuthWait(t *testing.T, s *Server, state, want string) codexOAuthStatus {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/accio/oauth/status?state="+url.QueryEscape(state), nil))
		s.HandleAccioOAuthStatus(c)
		if w.Code != http.StatusOK {
			t.Fatalf("status: %d %s", w.Code, w.Body.String())
		}
		var result codexOAuthStatus
		mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &result)
		if result.Status == want {
			return result
		}
		if time.Now().After(deadline) || result.Status == "error" || result.Status == "cancelled" || result.Status == "complete" {
			t.Fatalf("wanted %s, got %+v", want, result)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAccioOAuthBrowserLogin(t *testing.T) {
	for _, region := range []string{"cn", "global"} {
		t.Run(region, func(t *testing.T) {
			s := newInMemoryServer(t)
			tokenRequests := make(chan map[string]string, 2)
			accioTestClient(s, func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("x-package-region") != strings.ToUpper(region) || r.Header.Get("Authorization") != "" {
					t.Errorf("invalid OAuth request headers: %#v", r.Header)
				}
				switch r.URL.Path {
				case "/api/oauth/token":
					var body map[string]string
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						return nil, err
					}
					tokenRequests <- body
					return accioTestResponse(r, 200, fmt.Sprintf(`{"data":{"accessToken":"access-%s","refreshToken":"refresh-new","expiresAt":1800000000000}}`, body["code"])), nil
				case "/api/auth/userinfo":
					return accioTestResponse(r, 200, `{"data":{"id":"same-user","name":"User"}}`), nil
				case "/api/llm/config":
					return accioTestResponse(r, 200, accioTestCatalog), nil
				default:
					return nil, fmt.Errorf("unexpected request %s", r.URL.Path)
				}
			})
			var channelID int64
			for attempt := range 2 {
				state, authorization := accioOAuthStart(t, s, region)
				query := authorization.Query()
				callbackURL := query.Get("return_url")
				// Invalid input must not replace the valid pending login.
				c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/accio/oauth/start", map[string]string{"region": "intl"}))
				s.HandleStartAccioOAuth(c)
				if w.Code != http.StatusBadRequest {
					t.Fatalf("invalid region: %d", w.Code)
				}
				accioOAuthWait(t, s, state, "pending")
				code := fmt.Sprintf("code-%d", attempt)
				validCallback := callbackURL + "?code=" + code + "&state=" + url.QueryEscape(state)
				if region == "cn" {
					client := &http.Client{Timeout: 2 * time.Second}
					for _, suffix := range []string{"?code=bad&state=wrong", "?error=access_denied&state=wrong", "?code=bad"} {
						response, err := client.Get(callbackURL + suffix)
						if err != nil {
							t.Fatal(err)
						}
						_ = response.Body.Close()
						if response.StatusCode != http.StatusBadRequest {
							t.Fatalf("invalid callback: %d", response.StatusCode)
						}
					}
					accioOAuthWait(t, s, state, "pending")
					response, err := client.Get(validCallback)
					if err != nil {
						t.Fatal(err)
					}
					_ = response.Body.Close()
					if response.StatusCode != http.StatusOK {
						t.Fatalf("callback: %d", response.StatusCode)
					}
				} else {
					for _, callback := range []string{callbackURL + "?code=bad&state=wrong", validCallback} {
						c, w = newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/accio/oauth/callback", map[string]string{"callback_url": callback}))
						s.HandleSubmitAccioOAuthCallback(c)
						want := http.StatusBadRequest
						if callback == validCallback {
							want = http.StatusOK
							var result codexOAuthStatus
							mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &result)
							if result.State != state || result.Status != "accepted" {
								t.Fatalf("callback result: %+v", result)
							}
						}
						if w.Code != want {
							t.Fatalf("manual callback: %d %s", w.Code, w.Body.String())
						}
					}
				}
				status := accioOAuthWait(t, s, state, "complete")
				request := <-tokenRequests
				challenge := sha256.Sum256([]byte(request["codeVerifier"]))
				if request["redirectUri"] != callbackURL || request["code"] != code || request["clientId"] != "accio-work" || len(request) != 4 || len(request["codeVerifier"]) < 43 || base64.RawURLEncoding.EncodeToString(challenge[:]) != query.Get("code_challenge") {
					t.Fatal("token exchange does not match PKCE authorization")
				}
				if attempt == 0 {
					channelID = status.ChannelID
				} else if status.ChannelID != channelID {
					t.Fatal("reauthorization created a duplicate channel")
				}
				configs, err := s.store.ListConfigs(context.Background())
				if err != nil || len(configs) != 1 {
					t.Fatalf("channels: %d %v", len(configs), err)
				}
				cfg := configs[0]
				credential, err := accioauth.ParseCredential([]byte(cfg.OAuthCredential))
				if err != nil || credential.AccessToken != "access-"+code || credential.Region != region || credential.UserID != "same-user" || credential.ExpiresAt != 1800000000 || !cfg.Enabled {
					t.Fatalf("persisted login: %+v %v", credential, err)
				}
				if attempt == 0 {
					credential.OAuthUsage = `{"saved":true}`
					previousCredential := cfg.OAuthCredential
					cfg.OAuthCredential, err = credential.JSON()
					if err != nil {
						t.Fatal(err)
					}
					if updated, err := s.store.CompareAndSwapOAuthCredential(context.Background(), cfg.ID, model.AuthTypeAccioOAuth, previousCredential, cfg.OAuthCredential); err != nil || !updated {
						t.Fatalf("save usage: %v %v", updated, err)
					}
					cfg.Name, cfg.Enabled = "Custom name", false
					cfg.ModelEntries = []model.ModelEntry{{Model: "custom-model"}}
					if _, err := s.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
						t.Fatal(err)
					}
				} else if cfg.Name != "Custom name" || !cfg.SupportsModel("custom-model") || credential.OAuthUsage != `{"saved":true}` {
					t.Fatal("reauthorization lost channel configuration or usage")
				}
				c, w = newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/accio/oauth/callback", map[string]string{"callback_url": validCallback}))
				s.HandleSubmitAccioOAuthCallback(c)
				if w.Code != http.StatusBadRequest || len(tokenRequests) != 0 {
					t.Fatal("callback replay was accepted")
				}
			}
		})
	}
}

func TestAccioOAuthCancellationAndFailure(t *testing.T) {
	for _, outcome := range []string{"cancel", "denied", "token", "identity"} {
		t.Run(outcome, func(t *testing.T) {
			s := newInMemoryServer(t)
			var exchanges atomic.Int32
			accioTestClient(s, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/api/oauth/token":
					exchanges.Add(1)
					if outcome == "token" {
						return accioTestResponse(r, 400, `{"error":"invalid_grant"}`), nil
					}
					return accioTestResponse(r, 200, `{"accessToken":"access"}`), nil
				case "/api/auth/userinfo":
					return accioTestResponse(r, 401, `{}`), nil
				default:
					return nil, fmt.Errorf("unexpected request %s", r.URL.Path)
				}
			})
			state, authorization := accioOAuthStart(t, s, "global")
			callbackURL := authorization.Query().Get("return_url") + "?state=" + url.QueryEscape(state)
			want, wantExchanges := "error", int32(1)
			if outcome == "cancel" {
				c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/accio/oauth/cancel", map[string]string{"state": state}))
				s.HandleCancelAccioOAuth(c)
				if w.Code != http.StatusOK {
					t.Fatalf("cancel: %d %s", w.Code, w.Body.String())
				}
				want, wantExchanges = "cancelled", 0
			}
			if outcome == "denied" {
				callbackURL += "&error=access_denied"
				wantExchanges = 0
			} else {
				callbackURL += "&code=authorization-code"
			}
			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/accio/oauth/callback", map[string]string{"callback_url": callbackURL}))
			s.HandleSubmitAccioOAuthCallback(c)
			if outcome == "cancel" && w.Code != http.StatusBadRequest || outcome != "cancel" && w.Code != http.StatusOK {
				t.Fatalf("callback: %d %s", w.Code, w.Body.String())
			}
			accioOAuthWait(t, s, state, want)
			configs, err := s.store.ListConfigs(context.Background())
			if err != nil || len(configs) != 0 || exchanges.Load() != wantExchanges {
				t.Fatalf("failure persisted channel: %d %v exchanges=%d", len(configs), err, exchanges.Load())
			}
		})
	}
}

func accioTestCredential(t *testing.T, region, token string) string {
	t.Helper()
	raw, err := (&accioauth.Credential{Type: "accio", Region: region, AccessToken: token, RefreshToken: "refresh-secret", UserID: "same-user", ExpiresAt: time.Now().Add(time.Hour).Unix()}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func accioTestEvents(t *testing.T, raw []byte) []gjson.Result {
	t.Helper()
	var events []gjson.Result
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if !json.Valid([]byte(data)) {
			t.Fatalf("invalid SSE JSON: %s", data)
		}
		events = append(events, gjson.Parse(data))
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func TestAccioReauthorizationUpdatesCatalog(t *testing.T) {
	env := setupProxyTestEnv(t, []testChannel{{name: "accio", authType: model.AuthTypeAccioOAuth, oauthCredential: accioTestCredential(t, "global", "old-token"), models: "accio-test,accio-new", upstreamProtocol: "openai"}}, map[int]string{0: "https://phoenix-gw.alibaba.com/v1/chat/completions"})
	var catalogs, chats atomic.Int32
	accioTestClient(env.server, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/auth/userinfo":
			return accioTestResponse(r, 200, `{"data":{"id":"same-user"}}`), nil
		case "/api/llm/config":
			catalogs.Add(1)
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			catalog := accioTestCatalog
			if body["token"] == "new-token" {
				catalog = `{"data":[{"modelList":[{"modelCode":"accio-new-code","modelName":"accio-new","protocol":"openai"}]}]}`
			}
			return accioTestResponse(r, 200, catalog), nil
		case "/api/adk/llm/generateContent":
			chats.Add(1)
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			if chats.Load() == 2 && (gjson.GetBytes(raw, "model").String() != "accio-new" || gjson.GetBytes(raw, "token").String() != "new-token") {
				t.Errorf("reauthorized wire: %s", raw)
			}
			return accioTestResponse(r, 200, accioTestCompletion), nil
		default:
			return accioTestResponse(r, 404, `{}`), nil
		}
	})
	request := func(name string) *httptest.ResponseRecorder {
		return doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": name, "messages": []any{map[string]string{"role": "user", "content": "hi"}}}, nil)
	}
	if w := request("accio-test"); w.Code != 200 {
		t.Fatalf("warm catalog: %d %s", w.Code, w.Body.String())
	}
	c, w := newTestContext(t, newJSONRequest(t, "POST", "/admin/accio/credentials/import", map[string]any{"region": "global", "credential": map[string]any{"accessToken": "new-token", "refreshToken": "new-refresh"}}))
	env.server.HandleImportAccioCredential(c)
	if w.Code != 200 {
		t.Fatalf("reauthorize: %d %s", w.Code, w.Body.String())
	}
	if w = request("accio-new"); w.Code != 200 {
		t.Fatalf("new model: %d %s", w.Code, w.Body.String())
	}
	if catalogs.Load() != 2 || chats.Load() != 2 {
		t.Fatalf("catalogs=%d chats=%d", catalogs.Load(), chats.Load())
	}
}

func TestAccioBatchImportDeduplicatesRenamedAccount(t *testing.T) {
	s := newInMemoryServer(t)
	credential := accioTestCredential(t, "global", "batch-access")
	cfg, err := s.store.CreateConfig(context.Background(), newAccioChannel("My renamed account", credential))
	if err != nil {
		t.Fatal(err)
	}
	accioTestClient(s, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/auth/userinfo" {
			return accioTestResponse(r, 200, `{"data":{"id":"same-user"}}`), nil
		}
		if r.URL.Path == "/api/llm/config" {
			return accioTestResponse(r, 200, accioTestCatalog), nil
		}
		return accioTestResponse(r, 404, `{}`), nil
	})
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("files", "accio.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(part, credential); err != nil {
		t.Fatal(err)
	}
	if err = writer.WriteField("provider", "auto"); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/oauth/credentials/import", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	w := importOAuthCredentialsViaJob(t, s, req)
	result := mustParseAPIResponse[oauthCredentialImportSummary](t, w.Body.Bytes())
	if result.Data.Created != 0 || result.Data.Skipped != 1 || result.Data.Failed != 0 {
		t.Fatalf("batch import: %s", w.Body.String())
	}
	configs, err := s.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 1 || configs[0].ID != cfg.ID || configs[0].Name != "My renamed account" {
		t.Fatalf("duplicate or renamed channel: %+v", configs)
	}
}

/**
 * @brief 验证四种客户端协议保留 Accio 会话字段，并在转换后拒绝非法字段。
 * @param t 当前测试上下文。
 */
func TestAccioSessionFieldsCrossProtocol(t *testing.T) {
	for _, tc := range []struct {
		name string ///< 客户端协议。
		path string ///< 客户端生成端点。
		body string ///< 最小有效请求。
	}{
		{name: "openai", path: "/v1/chat/completions", body: `{"model":"accio-test","messages":[{"role":"user","content":"hi"}]}`},
		{name: "responses", path: "/v1/responses", body: `{"model":"accio-test","input":"hi"}`},
		{name: "anthropic", path: "/v1/messages", body: `{"model":"accio-test","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "gemini", path: "/v1beta/models/accio-test:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`},
	} {
		for _, invalid := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/invalid=%v", tc.name, invalid), func(t *testing.T) {
				env := setupProxyTestEnv(t, []testChannel{{name: "accio-session", authType: model.AuthTypeAccioOAuth, oauthCredential: accioTestCredential(t, "global", "session-access"), models: "accio-test", upstreamProtocol: "openai"}}, map[int]string{0: "https://phoenix-gw.alibaba.com/v1/chat/completions"})
				var chats atomic.Int32
				accioTestClient(env.server, func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/api/llm/config" {
						return accioTestResponse(r, 200, accioTestCatalog), nil
					}
					chats.Add(1)
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						return nil, err
					}
					for key, want := range map[string]string{"session_key": "session", "conversation_id": "conversation", "conversation_name": "title"} {
						if body[key] != want {
							t.Errorf("native %s = %#v, want %q", key, body[key], want)
						}
					}
					return accioTestResponse(r, 200, accioTestCompletion), nil
				})
				var request map[string]any
				mustUnmarshalJSON(t, []byte(tc.body), &request)
				request["session_key"] = nil
				request["sessionKey"] = " session "
				request["session_id"] = "lower-priority"
				request["conversationId"] = " conversation "
				request["conversationName"] = " title "
				if invalid {
					request["sessionKey"] = 42
				}
				w := doProxyRequest(t, env.engine, tc.path, request, nil)
				wantStatus, wantChats := http.StatusOK, int32(1)
				if invalid {
					wantStatus, wantChats = http.StatusBadRequest, 0
				}
				if w.Code != wantStatus || chats.Load() != wantChats {
					t.Fatalf("session conversion: status=%d chats=%d body=%s", w.Code, chats.Load(), w.Body.String())
				}
			})
		}
	}
}

/**
 * @brief 验证会话字段先透传再应用渠道规则，原生认证刷新重试不恢复已删除字段。
 * @param t 当前测试上下文。
 */
func TestAccioSessionBodyRulesSurviveRefreshRetry(t *testing.T) {
	rules := &model.CustomRequestRules{Body: []model.CustomBodyRule{
		{Action: model.RuleActionOverride, Path: "session_key", Value: json.RawMessage(`"rule-session"`)},
		{Action: model.RuleActionRemove, Path: "conversation_id"},
		{Action: model.RuleActionRemove, Path: "conversation_name"},
	}}
	env := setupProxyTestEnv(t, []testChannel{{name: "accio-session-rules", authType: model.AuthTypeAccioOAuth, oauthCredential: accioTestCredential(t, "global", "old-access"), models: "accio-test", upstreamProtocol: "openai", customRequestRules: rules}}, map[int]string{0: "https://phoenix-gw.alibaba.com/v1/chat/completions"})
	var chats, refreshes atomic.Int32
	accioTestClient(env.server, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/llm/config":
			return accioTestResponse(r, 200, accioTestCatalog), nil
		case "/api/auth/refresh_token":
			refreshes.Add(1)
			return accioTestResponse(r, 200, `{"data":{"accessToken":"new-access","refreshToken":"new-refresh","userId":"same-user"}}`), nil
		default:
			attempt := chats.Add(1)
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return nil, err
			}
			if body["session_key"] != "rule-session" {
				t.Errorf("attempt %d ignored session override: %#v", attempt, body["session_key"])
			}
			for _, field := range []string{"conversation_id", "conversation_name"} {
				if value, exists := body[field]; exists {
					t.Errorf("attempt %d restored removed %s = %#v", attempt, field, value)
				}
			}
			if attempt == 1 {
				return accioTestResponse(r, 200, "data: {\"error_code\":\"401\",\"error_message\":\"token expired\"}\n\n"), nil
			}
			return accioTestResponse(r, 200, accioTestCompletion), nil
		}
	})
	w := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
		"model": "accio-test", "input": "hi", "sessionKey": "client-session", "conversation_id": "removed-conversation", "conversation_name": "removed-title",
	}, nil)
	if w.Code != http.StatusOK || chats.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("refresh retry: status=%d chats=%d refreshes=%d body=%s", w.Code, chats.Load(), refreshes.Load(), w.Body.String())
	}
}

/**
 * @brief 验证分片思考标签及原生思考经四种客户端协议输出，工具与用量保持完整。
 * @param t 当前测试上下文。
 */
func TestAccioCrossProtocolTools(t *testing.T) {
	/**
	 * @brief 将标签前缀拆在 ADK 帧边界，覆盖 think 与 thinking 的前缀歧义。
	 */
	const native = `data: {"content":{"parts":[{"text":"native <think>literal</think> ","thought":true}]}}

data: {"content":{"parts":[{"text":"before <thi"}]}}

data: {"content":{"parts":[{"text":"nk>first </th"}]}}

data: {"content":{"parts":[{"text":"ink>middle <think"}]}}

data: {"content":{"parts":[{"text":"ing>second</thinking>checking"}]}}

data: {"content":{"parts":[{"function_call":{"id":"call-accio","name":"lookup","args_json":"{\"q\":\"test\"}"}}]}}

data: {"turn_complete":true,"finish_reason":"tool_calls","usage_metadata":{"prompt_token_count":10,"candidates_token_count":7,"total_token_count":17}}

`
	for _, client := range []string{"openai", "responses", "anthropic", "gemini"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", client, stream), func(t *testing.T) {
				env := setupProxyTestEnv(t, []testChannel{{name: "accio", authType: model.AuthTypeAccioOAuth, oauthCredential: accioTestCredential(t, "global", "tool-access"), models: "accio-test", upstreamProtocol: "openai"}}, map[int]string{0: "https://phoenix-gw.alibaba.com/v1/chat/completions"})
				var chats atomic.Int32
				accioTestClient(env.server, func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/api/llm/config" {
						return accioTestResponse(r, 200, accioTestCatalog), nil
					}
					chats.Add(1)
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					if gjson.GetBytes(raw, "tools.0.name").String() != "lookup" || gjson.GetBytes(raw, "contents.0.parts.0.text").String() != "hi" {
						t.Errorf("translated ADK request lost tools/content: %s", raw)
					}
					return accioTestResponse(r, 200, native), nil
				})
				path := "/v1/responses"
				request := map[string]any{"model": "accio-test", "stream": stream, "input": "hi", "tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}}}}
				switch client {
				case "openai":
					path = "/v1/chat/completions"
					request = map[string]any{"model": "accio-test", "stream": stream, "messages": []any{map[string]string{"role": "user", "content": "hi"}}, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}}}}}
				case "anthropic":
					path = "/v1/messages"
					request = map[string]any{"model": "accio-test", "stream": stream, "max_tokens": 64, "messages": []any{map[string]string{"role": "user", "content": "hi"}}, "tools": []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}}}}
				case "gemini":
					path = "/v1beta/models/accio-test:generateContent"
					if stream {
						path = "/v1beta/models/accio-test:streamGenerateContent?alt=sse"
					}
					request = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": "hi"}}}}, "tools": []any{map[string]any{"functionDeclarations": []any{map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}}}}}}
				}
				w := doProxyRequest(t, env.engine, path, request, nil)
				if w.Code != 200 || chats.Load() != 1 {
					t.Fatalf("cross-protocol response: %d calls=%d %s", w.Code, chats.Load(), w.Body.String())
				}
				var events []gjson.Result
				if stream {
					events = accioTestEvents(t, w.Body.Bytes())
				} else {
					if !json.Valid(w.Body.Bytes()) {
						t.Fatalf("invalid JSON: %s", w.Body.String())
					}
					events = []gjson.Result{gjson.ParseBytes(w.Body.Bytes())}
				}
				var text, thinking, ordered, args, name, id string
				var input, output int64
				var terminal bool
				for _, event := range events {
					switch client {
					case "openai":
						message := event.Get("choices.0.message")
						if stream {
							message = event.Get("choices.0.delta")
						}
						text += message.Get("content").String()
						thinking += message.Get("reasoning_content").String()
						ordered += message.Get("reasoning_content").String() + message.Get("content").String()
						for _, call := range message.Get("tool_calls").Array() {
							if call.Get("id").Exists() {
								id = call.Get("id").String()
							}
							if call.Get("function.name").Exists() {
								name = call.Get("function.name").String()
							}
							args += call.Get("function.arguments").String()
						}
						if event.Get("usage").Exists() {
							input = event.Get("usage.prompt_tokens").Int()
							output = event.Get("usage.completion_tokens").Int()
						}
						terminal = terminal || event.Get("choices.0.finish_reason").String() == "tool_calls"
					case "responses":
						if stream {
							switch event.Get("type").String() {
							case "response.output_text.delta":
								text += event.Get("delta").String()
								ordered += event.Get("delta").String()
							case "response.reasoning_summary_text.delta":
								thinking += event.Get("delta").String()
								ordered += event.Get("delta").String()
							case "response.function_call_arguments.done":
								args = event.Get("arguments").String()
							case "response.output_item.done":
								if item := event.Get("item"); item.Get("type").String() == "function_call" {
									name = item.Get("name").String()
									id = item.Get("call_id").String()
									args = item.Get("arguments").String()
								}
							case "response.completed":
								terminal = true
								input = event.Get("response.usage.input_tokens").Int()
								output = event.Get("response.usage.output_tokens").Int()
							}
						} else {
							terminal = event.Get("status").String() == "completed"
							input = event.Get("usage.input_tokens").Int()
							output = event.Get("usage.output_tokens").Int()
							for _, item := range event.Get("output").Array() {
								if item.Get("type").String() == "function_call" {
									name = item.Get("name").String()
									id = item.Get("call_id").String()
									args = item.Get("arguments").String()
								} else if item.Get("type").String() == "reasoning" {
									for _, part := range item.Get("summary").Array() {
										thinking += part.Get("text").String()
									}
								} else {
									for _, part := range item.Get("content").Array() {
										text += part.Get("text").String()
									}
								}
							}
						}
					case "anthropic":
						if stream {
							switch event.Get("type").String() {
							case "content_block_start":
								if block := event.Get("content_block"); block.Get("type").String() == "tool_use" {
									name = block.Get("name").String()
									id = block.Get("id").String()
								}
							case "content_block_delta":
								text += event.Get("delta.text").String()
								thinking += event.Get("delta.thinking").String()
								ordered += event.Get("delta.thinking").String() + event.Get("delta.text").String()
								args += event.Get("delta.partial_json").String()
							case "message_start":
								input = event.Get("message.usage.input_tokens").Int()
							case "message_delta":
								if event.Get("usage.input_tokens").Exists() {
									input = event.Get("usage.input_tokens").Int()
								}
								output = event.Get("usage.output_tokens").Int()
								terminal = event.Get("delta.stop_reason").String() == "tool_use"
							}
						} else {
							input = event.Get("usage.input_tokens").Int()
							output = event.Get("usage.output_tokens").Int()
							terminal = event.Get("stop_reason").String() == "tool_use"
							for _, block := range event.Get("content").Array() {
								text += block.Get("text").String()
								thinking += block.Get("thinking").String()
								if block.Get("type").String() == "tool_use" {
									name = block.Get("name").String()
									id = block.Get("id").String()
									args = block.Get("input").Raw
								}
							}
						}
					case "gemini":
						for _, part := range event.Get("candidates.0.content.parts").Array() {
							if part.Get("thought").Bool() {
								thinking += part.Get("text").String()
							} else {
								text += part.Get("text").String()
							}
							ordered += part.Get("text").String()
							if call := part.Get("functionCall"); call.Exists() {
								name = call.Get("name").String()
								id = call.Get("id").String()
								args = call.Get("args").Raw
							}
						}
						if event.Get("candidates.0.finishReason").String() == "STOP" {
							terminal = true
						}
						if event.Get("usageMetadata").Exists() {
							input = event.Get("usageMetadata.promptTokenCount").Int()
							output = event.Get("usageMetadata.candidatesTokenCount").Int()
						}
					}
				}
				if stream && ordered != "native <think>literal</think> before first middle secondchecking" {
					t.Fatalf("stream reordered text/thinking: %q body=%s", ordered, w.Body.String())
				}
				if text != "before middle checking" || thinking != "native <think>literal</think> first second" || name != "lookup" || id == "" || gjson.Get(args, "q").String() != "test" || input != 10 || output != 7 || !terminal {
					t.Fatalf("lost protocol data: text=%q thinking=%q name=%q id=%q args=%s tokens=%d/%d terminal=%v body=%s", text, thinking, name, id, args, input, output, terminal, w.Body.String())
				}
			})
		}
	}
}

func TestAccioCommittedStreamDoesNotReplay(t *testing.T) {
	for _, failure := range []string{"truncated", "unauthorized"} {
		t.Run(failure, func(t *testing.T) {
			fallback, err := accioauth.ParseCredential([]byte(accioTestCredential(t, "global", "fallback-access")))
			if err != nil {
				t.Fatal(err)
			}
			fallback.UserID = "fallback-user"
			fallbackJSON, err := fallback.JSON()
			if err != nil {
				t.Fatal(err)
			}
			env := setupProxyTestEnv(t, []testChannel{
				{name: "primary", authType: model.AuthTypeAccioOAuth, oauthCredential: accioTestCredential(t, "global", "primary-access"), models: "accio-test", upstreamProtocol: "openai"},
				{name: "fallback", authType: model.AuthTypeAccioOAuth, oauthCredential: fallbackJSON, models: "accio-test", upstreamProtocol: "openai"},
			}, map[int]string{0: "https://phoenix-gw.alibaba.com/v1/chat/completions", 1: "https://phoenix-gw.alibaba.com/v1/chat/completions"})
			var primaryChats, fallbackChats, refreshes atomic.Int32
			accioTestClient(env.server, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/api/llm/config":
					return accioTestResponse(r, 200, accioTestCatalog), nil
				case "/api/auth/refresh_token":
					refreshes.Add(1)
					return accioTestResponse(r, 200, `{"data":{"accessToken":"unexpected-refresh"}}`), nil
				default:
					raw, readErr := io.ReadAll(r.Body)
					if readErr != nil {
						return nil, readErr
					}
					if gjson.GetBytes(raw, "token").String() == "fallback-access" {
						fallbackChats.Add(1)
					} else {
						primaryChats.Add(1)
					}
					body := "data: {\"content\":{\"parts\":[{\"text\":\"visible partial\"}]}}\n\n"
					if failure == "unauthorized" {
						body += "data: {\"error_code\":\"401\",\"error_message\":\"token expired\"}\n\n"
					}
					return accioTestResponse(r, 200, body), nil
				}
			})
			w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "accio-test", "messages": []any{map[string]string{"role": "user", "content": "hi"}}, "stream": true}, nil)
			if w.Code != 200 || primaryChats.Load() != 1 || fallbackChats.Load() != 0 || refreshes.Load() != 0 {
				t.Fatalf("committed stream replayed: status=%d primary=%d fallback=%d refresh=%d body=%s", w.Code, primaryChats.Load(), fallbackChats.Load(), refreshes.Load(), w.Body.String())
			}
			var text string
			var sawError, sawFinish bool
			for _, event := range accioTestEvents(t, w.Body.Bytes()) {
				text += event.Get("choices.0.delta.content").String()
				sawError = sawError || event.Get("error").Exists()
				sawFinish = sawFinish || event.Get("choices.0.finish_reason").Type == gjson.String
			}
			if text != "visible partial" || !sawError || sawFinish || strings.Contains(w.Body.String(), "data: [DONE]") {
				t.Fatalf("failed stream marked complete: %s", w.Body.String())
			}
		})
	}
}

func TestAccioRefreshConcurrentReauthorizationWins(t *testing.T) {
	for _, tc := range []struct{ upstreamFailure, cookieOnly bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		t.Run(fmt.Sprintf("failure=%v/cookieOnly=%v", tc.upstreamFailure, tc.cookieOnly), func(t *testing.T) {
			s := newInMemoryServer(t)
			cfg, err := s.store.CreateConfig(context.Background(), newAccioChannel("refresh", accioTestCredential(t, "global", "old-access")))
			if err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{})
			release := make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			var refreshes atomic.Int32
			accioTestClient(s, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/api/auth/userinfo":
					return accioTestResponse(r, 200, `{"data":{"id":"same-user"}}`), nil
				case "/api/auth/refresh_token":
					if refreshes.Add(1) == 1 {
						close(started)
					}
					select {
					case <-release:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
					if tc.upstreamFailure {
						return accioTestResponse(r, 401, `{}`), nil
					}
					return accioTestResponse(r, 200, `{"data":{"accessToken":"stale-refresh-access","refreshToken":"stale-refresh-token","sgcookie":"stale-session-cookie"}}`), nil
				default:
					return accioTestResponse(r, 404, `{}`), nil
				}
			})
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				c, w := newTestContext(t, newRequest(http.MethodPost, fmt.Sprintf("/admin/channels/%d/accio-credential/refresh", cfg.ID), nil))
				c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(cfg.ID)}}
				s.HandleRefreshAccioCredential(c)
				done <- w
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("refresh never started")
			}
			access, refresh := "winner-access", "winner-refresh"
			if tc.cookieOnly {
				access, refresh = "old-access", "refresh-secret"
			}
			c, w := newTestContext(t, newJSONRequest(t, "POST", "/admin/accio/credentials/import", map[string]any{"region": "global", "credential": map[string]any{"accessToken": access, "refreshToken": refresh, "sgcookie": "winner-session-cookie"}}))
			s.HandleImportAccioCredential(c)
			if w.Code != 200 {
				t.Fatalf("reauthorization failed: %d %s", w.Code, w.Body.String())
			}
			close(release)
			select {
			case response := <-done:
				if tc.cookieOnly && tc.upstreamFailure {
					if response.Code == 200 {
						t.Fatal("cookie-only update hid a failed token refresh")
					}
				} else if response.Code != 200 {
					t.Fatalf("refresh did not use new authorization: %d %s", response.Code, response.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("refresh did not complete")
			}
			latest, err := s.store.GetConfig(context.Background(), cfg.ID)
			if err != nil {
				t.Fatal(err)
			}
			credential, err := accioauth.ParseCredential([]byte(latest.OAuthCredential))
			if err != nil {
				t.Fatal(err)
			}
			if tc.cookieOnly && !tc.upstreamFailure {
				access, refresh = "stale-refresh-access", "stale-refresh-token"
			}
			if credential.AccessToken != access || credential.RefreshToken != refresh || credential.SGCookie != "winner-session-cookie" || refreshes.Load() != 1 {
				t.Fatalf("refresh overwrote reauthorization: %#v requests=%d", credential, refreshes.Load())
			}
		})
	}
}

func TestAccioImportRefreshesExpiredCredential(t *testing.T) {
	for _, knownExpiry := range []bool{false, true} {
		t.Run(fmt.Sprintf("known_expiry=%v", knownExpiry), func(t *testing.T) {
			s := newInMemoryServer(t)
			var refreshes, catalogs atomic.Int32
			accioTestClient(s, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/api/auth/userinfo":
					var body map[string]string
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						return nil, err
					}
					if r.Method != http.MethodPost || r.URL.RawQuery != "" {
						t.Error("profile must use POST JSON")
					}
					if body["accessToken"] == "expired-access" {
						return accioTestResponse(r, 401, `{}`), nil
					}
					if body["accessToken"] != "import-fresh-access" {
						t.Errorf("unexpected profile token")
					}
					return accioTestResponse(r, 200, `{"data":{"id":"same-user"}}`), nil
				case "/api/auth/refresh_token":
					refreshes.Add(1)
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					if gjson.GetBytes(raw, "accessToken").String() != "expired-access" || gjson.GetBytes(raw, "refreshToken").String() != "import-refresh" {
						t.Errorf("invalid import refresh body: %s", raw)
					}
					return accioTestResponse(r, 200, `{"data":{"accessToken":"import-fresh-access","refreshToken":"import-fresh-refresh","expiresAt":1900000000000}}`), nil
				case "/api/llm/config":
					catalogs.Add(1)
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					if gjson.GetBytes(raw, "token").String() != "import-fresh-access" {
						t.Errorf("catalog used expired imported token")
					}
					return accioTestResponse(r, 200, accioTestCatalog), nil
				default:
					return accioTestResponse(r, 404, `{}`), nil
				}
			})
			credential := map[string]any{"accessToken": "expired-access", "refreshToken": "import-refresh"}
			if knownExpiry {
				credential["expiresAt"] = time.Now().Add(-time.Hour).UnixMilli()
			}
			c, w := newTestContext(t, newJSONRequest(t, "POST", "/admin/accio/credentials/import", map[string]any{"region": "global", "credential": credential}))
			s.HandleImportAccioCredential(c)
			if w.Code != 200 {
				t.Fatalf("expired credential import: %d %s", w.Code, w.Body.String())
			}
			configs, err := s.store.ListConfigs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(configs) != 1 {
				t.Fatalf("channels=%d", len(configs))
			}
			stored, err := accioauth.ParseCredential([]byte(configs[0].OAuthCredential))
			if err != nil {
				t.Fatal(err)
			}
			if stored.AccessToken != "import-fresh-access" || stored.RefreshToken != "import-fresh-refresh" || stored.ExpiresAt != 1900000000 || refreshes.Load() != 1 || catalogs.Load() != 1 {
				t.Fatalf("import did not persist fresh credentials: %#v refresh=%d catalog=%d", stored, refreshes.Load(), catalogs.Load())
			}
			if strings.Contains(w.Body.String(), "access") || strings.Contains(w.Body.String(), "refresh") {
				t.Fatalf("import leaked credential fields: %s", w.Body.String())
			}
		})
	}
}
func accioTestClient(s *Server, transport roundTripperFunc) {
	s.client = &http.Client{Transport: transport}
	s.accioService.Client = s.client
}
func accioTestResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func TestAccioImportAndUsage(t *testing.T) {
	s := newInMemoryServer(t)
	accioTestClient(s, func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "" {
			t.Error("Accio must not send Bearer authentication")
		}
		switch r.URL.Path {
		case "/api/auth/userinfo":
			if r.Method != http.MethodPost {
				return accioTestResponse(r, http.StatusMethodNotAllowed, `{"status":405,"error":"Method Not Allowed"}`), nil
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return nil, err
			}
			if r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" || !strings.HasPrefix(body["accessToken"], "secret-access-") {
				t.Error("invalid profile JSON request")
			}
			return accioTestResponse(r, 200, `{"success":true,"data":{"id":"same-user","name":"User"}}`), nil
		case "/api/llm/config":
			return accioTestResponse(r, 200, `{"data":[{"modelList":[{"modelCode":"opaque-a","modelName":"accio-test"},{"modelCode":"opaque-b","modelName":"accio-test"}]}]}`), nil
		case "/api/entitlement/quota":
			return accioTestResponse(r, 200, `{"data":{"usagePercent":25,"refreshCountdownSeconds":3600}}`), nil
		case "/api/entitlement/currentSubscription":
			return accioTestResponse(r, 200, `{"data":{"planName":"Pro","status":"active"}}`), nil
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			return accioTestResponse(r, 404, `{}`), nil
		}
	})
	var ids []int64
	for index, region := range []string{"cn", "cn", "global"} {
		imported := map[string]any{"accessToken": fmt.Sprintf("secret-access-%d", index), "refreshToken": "secret-refresh"}
		if index == 0 {
			imported["utdid"] = "imported-device"
			imported["sgCookie"] = "imported-session-cookie"
		}
		c, w := newTestContext(t, newJSONRequest(t, "POST", "/admin/accio/credentials/import", map[string]any{"region": region, "credential": imported}))
		s.HandleImportAccioCredential(c)
		if w.Code != 200 {
			t.Fatalf("import: %d %s", w.Code, w.Body.String())
		}
		var result struct {
			ID      int64 `json:"channel_id"`
			Created bool  `json:"created"`
		}
		mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &result)
		if result.Created != (index != 1) {
			t.Fatalf("created=%v", result.Created)
		}
		ids = append(ids, result.ID)
	}
	if ids[0] != ids[1] || ids[0] == ids[2] {
		t.Fatalf("region identity: %v", ids)
	}
	usage, err := s.refreshOAuthUsage(context.Background(), ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if usage.Provider != "accio" || usage.PlanType != "Pro" || len(usage.Windows) != 1 || usage.Windows[0].UsedPercent != 25 || usage.Windows[0].RemainingPercent != 75 || usage.Windows[0].ResetAt <= time.Now().Unix() {
		t.Fatalf("usage=%+v", usage)
	}
	cfg, err := s.store.GetConfig(context.Background(), ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.ModelEntries, []model.ModelEntry{{Model: "accio-test"}}) {
		t.Fatalf("import must expose unique model names without code redirects: %+v", cfg.ModelEntries)
	}
	credential, err := accioauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SupportsModel("accio-test") || credential.OAuthUsage == "" || cfg.GetAuthType() != model.AuthTypeAccioOAuth {
		t.Fatalf("channel=%+v", cfg)
	}
	if credential.DeviceID != "imported-device" {
		t.Fatal("reauthorization without a device ID lost the imported device identity")
	}
	if credential.SGCookie != "imported-session-cookie" {
		t.Fatal("reauthorization without sgcookie lost the imported session cookie")
	}
	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
	s.HandleChannels(c)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for _, secret := range []string{"secret-access-0", "secret-access-1", "secret-access-2", "secret-refresh", "imported-session-cookie"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("channel list leaked credential")
		}
	}
	// A conflicting explicit region must fail before any account is changed.
	c, w = newTestContext(t, newJSONRequest(t, "POST", "/admin/accio/credentials/import", map[string]any{"region": "global", "credential": map[string]any{"region": "cn", "access_token": "secret"}}))
	s.HandleImportAccioCredential(c)
	if w.Code != 400 {
		t.Fatalf("region mismatch accepted: %d", w.Code)
	}
}

func TestAccioProxyAndAdminWire(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			credential := accioTestCredential(t, "global", "secret-access")
			env := setupProxyTestEnv(t, []testChannel{{name: "accio", authType: model.AuthTypeAccioOAuth, oauthCredential: credential, modelEntries: []model.ModelEntry{{Model: "accio-test"}}, upstreamProtocol: "openai"}}, map[int]string{0: "https://phoenix-gw.alibaba.com/v1/chat/completions"})
			var chats atomic.Int32
			accioTestClient(env.server, func(r *http.Request) (*http.Response, error) {
				userAgent := "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
				for key, want := range map[string]string{"User-Agent": userAgent, "x-platform": "desktop", "x-app-version": "0.34.4", "x-package-region": "GLOBAL", "Accept-Language": "en"} {
					if got := r.Header.Get(key); got != want {
						t.Errorf("%s: header %s = %q, want %q", r.URL.Path, key, got, want)
					}
				}
				if r.URL.Path == "/api/llm/config" {
					return accioTestResponse(r, 200, `{"data":[{"modelList":[{"modelCode":"other-route","modelName":"accio-test","protocol":"openai"},{"modelCode":"opaque-route","modelName":"accio-test","protocol":"openai"}]}]}`), nil
				}
				chats.Add(1)
				var body struct {
					Token    string `json:"token"`
					Model    string `json:"model"`
					Contents []any  `json:"contents"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if r.URL.Path != "/api/adk/llm/generateContent" || body.Token != "secret-access" || body.Model != "accio-test" || len(body.Contents) == 0 || r.Header.Get("Authorization") != "" || r.Header.Get("x-package-region") != "GLOBAL" || r.Header.Get("appKey") == "" {
					t.Fatalf("invalid wire: %+v %s", body, r.URL.Path)
				}
				return accioTestResponse(r, 200, accioTestCompletion), nil
			})
			w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "accio-test", "messages": []any{map[string]string{"role": "user", "content": "hi"}}, "stream": stream, "stream_options": map[string]bool{"include_usage": true}}, nil)
			if w.Code != 200 {
				t.Fatalf("proxy: %d %s", w.Code, w.Body.String())
			}
			var events []map[string]any
			if stream {
				scanner := bufio.NewScanner(w.Body)
				for scanner.Scan() {
					line := scanner.Text()
					if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
						var e map[string]any
						mustUnmarshalJSON(t, []byte(strings.TrimPrefix(line, "data: ")), &e)
						events = append(events, e)
					}
				}
				if err := scanner.Err(); err != nil {
					t.Fatal(err)
				}
			} else {
				var e map[string]any
				mustUnmarshalJSON(t, w.Body.Bytes(), &e)
				events = append(events, e)
			}
			var text string
			var total float64
			for _, e := range events {
				if u, ok := e["usage"].(map[string]any); ok {
					total, _ = u["total_tokens"].(float64)
				}
				if choices, ok := e["choices"].([]any); ok {
					for _, raw := range choices {
						ch := raw.(map[string]any)
						msg, _ := ch["delta"].(map[string]any)
						if msg == nil {
							msg, _ = ch["message"].(map[string]any)
						}
						content, _ := msg["content"].(string)
						text += content
					}
				}
			}
			if text != "hello" || total != 17 {
				t.Fatalf("text=%q tokens=%v", text, total)
			}
			configs, err := env.store.ListConfigs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			result := env.server.testChannelAPI(context.Background(), configs[0], "secret-access", &testutil.TestChannelRequest{Model: "accio-test", ClientProtocol: "openai", Content: "hi", Stream: stream})
			if result["success"] != true {
				t.Fatalf("admin test: %+v", result)
			}
			result = env.server.testChannelAPI(context.Background(), configs[0], "secret-access", &testutil.TestChannelRequest{Model: "opaque-route", ClientProtocol: "openai", Content: "hi", Stream: stream})
			if result["success"] != false || !strings.Contains(fmt.Sprint(result["error"]), "absent from this account's catalog") {
				t.Fatalf("modelCode must not resolve to modelName: %+v", result)
			}
			if chats.Load() != 2 {
				t.Fatalf("chat calls=%d", chats.Load())
			}
		})
	}
}

func TestAccioGatewayAndNonSSEResponse(t *testing.T) {
	for _, tc := range []struct {
		name, contentType, body, punish, code, message string
		status, wantStatus                             int
	}{
		{name: "gateway-200", status: 200, contentType: "text/html;charset=UTF-8", body: `<html>challenge-secret https://challenge.invalid/?token=private</html>`, punish: "1", wantStatus: 502, code: "accio_gateway_blocked", message: "Accio gateway rejected request (Bxpunish: 1)"},
		{name: "gateway-403", status: 403, contentType: "text/html", body: `<html>challenge-secret</html>`, punish: "1", wantStatus: 502, code: "accio_gateway_blocked", message: "Accio gateway rejected request (Bxpunish: 1)"},
		{name: "html-403", status: 403, contentType: "text/html", body: `<html>challenge-secret</html>`, wantStatus: 502, code: "accio_gateway_blocked", message: "Accio gateway rejected request"},
		{name: "html-200", status: 200, contentType: "text/html", body: `<html>challenge-secret</html>`, wantStatus: 502, code: "invalid_upstream_response", message: "Accio returned a non-SSE inference response"},
		{name: "json-rejection", status: 200, contentType: "application/json;charset=UTF-8", body: `{"success":false,"code":423,"message":"challenge-secret https://challenge.invalid/?token=private"}`, wantStatus: 502, code: "accio_upstream_error", message: "Accio upstream rejected request (HTTP 200, business code 423)"},
		{name: "json-quota", status: 200, contentType: "application/json", body: `{"error_code":429,"error_message":"challenge-secret"}`, wantStatus: 429, code: "rate_limit_exceeded", message: "Accio upstream rejected request (HTTP 200, business code 429)"},
		{name: "json-success", status: 200, contentType: "application/json", body: `{"success":true,"code":200,"message":"challenge-secret"}`, wantStatus: 502, code: "invalid_upstream_response", message: "Accio returned a non-SSE inference response"},
		{name: "json-unsafe-code", status: 200, contentType: "application/json", body: `{"success":false,"code":"challenge-secret","message":"challenge-secret"}`, wantStatus: 502, code: "accio_upstream_error", message: "Accio upstream rejected request (HTTP 200)"},
	} {
		for _, admin := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/admin=%v/stream=%v", tc.name, admin, stream), func(t *testing.T) {
					env := setupProxyTestEnvWithSettings(t, []testChannel{{name: "accio", authType: model.AuthTypeAccioOAuth, oauthCredential: accioTestCredential(t, "global", "access-secret"), models: "accio-test", upstreamProtocol: "openai"}}, map[int]string{0: "https://phoenix-gw.alibaba.com/v1/chat/completions"}, map[string]string{"debug_log_enabled": "true"})
					var chats, refreshes atomic.Int32
					accioTestClient(env.server, func(req *http.Request) (*http.Response, error) {
						switch req.URL.Path {
						case "/api/llm/config":
							return accioTestResponse(req, http.StatusOK, accioTestCatalog), nil
						case "/api/auth/refresh_token":
							refreshes.Add(1)
							return accioTestResponse(req, http.StatusUnauthorized, `{}`), nil
						default:
							chats.Add(1)
							resp := accioTestResponse(req, tc.status, tc.body)
							resp.Header.Set("Content-Type", tc.contentType)
							resp.Header.Set("Bxpunish", tc.punish)
							resp.Header.Set("Set-Cookie", "challenge-cookie-secret")
							resp.Header.Set("Location", "https://challenge.invalid/private")
							resp.Header.Set("X-Request-Id", "trace-123")
							return resp, nil
						}
					})
					var response *httptest.ResponseRecorder
					var failure gjson.Result
					if admin {
						configs, err := env.store.ListConfigs(context.Background())
						if err != nil || len(configs) != 1 {
							t.Fatalf("configs=%d err=%v", len(configs), err)
						}
						c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", configs[0].ID), map[string]any{"model": "accio-test", "client_protocol": "openai", "content": "hi", "stream": stream}))
						c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(configs[0].ID)}}
						env.server.HandleChannelTest(c)
						response = w
						data := gjson.GetBytes(w.Body.Bytes(), "data")
						if w.Code != http.StatusOK || data.Get("success").Bool() || data.Get("status_code").Int() != int64(tc.wantStatus) {
							t.Fatalf("admin response=%d %s", w.Code, w.Body.String())
						}
						failure = data.Get("api_error.error")
						if data.Get("response_headers.X-Request-Id").String() != "trace-123" {
							t.Fatal("admin diagnostic lost the upstream trace ID")
						}
					} else {
						response = doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "accio-test", "messages": []any{map[string]string{"role": "user", "content": "hi"}}, "stream": stream}, nil)
						if response.Code != tc.wantStatus {
							t.Fatalf("proxy response=%d %s", response.Code, response.Body.String())
						}
						failure = gjson.GetBytes(response.Body.Bytes(), "error")
					}
					if failure.Get("code").String() != tc.code || failure.Get("message").String() != tc.message || chats.Load() != 1 || refreshes.Load() != 0 {
						t.Fatalf("failure=%s chats=%d refreshes=%d", failure, chats.Load(), refreshes.Load())
					}
					for _, secret := range []string{"challenge-secret", "challenge-cookie-secret", "challenge.invalid", "access-secret", "refresh-secret"} {
						if strings.Contains(response.Body.String(), secret) || strings.Contains(fmt.Sprint(response.Header()), secret) {
							t.Fatal("upstream rejection leaked sensitive diagnostics")
						}
					}
				})
			}
		}
	}
}

/**
 * @brief 非流式管理测试保留 Accio SSE 错误正文和限流分类。
 * @param t 测试上下文。
 */
func TestAccioAdminQuotaExhaustedDetails(t *testing.T) {
	env := setupProxyTestEnv(t, []testChannel{{name: "accio", authType: model.AuthTypeAccioOAuth, oauthCredential: accioTestCredential(t, "global", "access"), models: "accio-test,spare-model", upstreamProtocol: "openai"}}, map[int]string{0: "https://phoenix-gw.alibaba.com/v1/chat/completions"})
	const errorBody = `{"error":{"status":429,"type":"rate_limit_error","code":"rate_limit_exceeded","message":"quota exhausted"},"status":429}`
	accioTestClient(env.server, func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/llm/config" {
			return accioTestResponse(req, http.StatusOK, strings.ReplaceAll(accioTestCatalog, `"protocol":"openai"`, `"protocol":"openai","reasoningEfforts":["low","medium","high"]`)), nil
		}
		return accioTestResponse(req, http.StatusOK, "data: "+errorBody+"\n\n"), nil
	})
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c, response := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", configs[0].ID), map[string]any{"model": "accio-test", "client_protocol": "anthropic", "content": "hi", "stream": false}))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(configs[0].ID)}}
	env.server.HandleChannelTest(c)
	data := gjson.GetBytes(response.Body.Bytes(), "data")
	body := strings.TrimSpace(strings.TrimPrefix(data.Get("upstream_response_body").String(), "data: "))
	if gjson.Get(body, "error.message").String() != "quota exhausted" {
		t.Fatalf("missing upstream error details: %s", data.Raw)
	}
	if data.Get("success").Bool() || data.Get("status_code").Int() != http.StatusOK || data.Get("error").String() != "quota exhausted" || data.Get("api_error.error.status").Int() != 429 || data.Get("cooldown_action").String() != "model_cooldown_applied" {
		t.Fatalf("incorrect quota classification: %s", data.Raw)
	}
}

func TestAccioProxySentinelRateLimit(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			env := setupProxyTestEnv(t, []testChannel{{name: "accio", authType: model.AuthTypeAccioOAuth, oauthCredential: accioTestCredential(t, "global", "access"), models: "accio-test", upstreamProtocol: "openai"}}, map[int]string{0: "https://phoenix-gw.alibaba.com/v1/chat/completions"})
			accioTestClient(env.server, func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/api/llm/config" {
					return accioTestResponse(req, http.StatusOK, accioTestCatalog), nil
				}
				return accioTestResponse(req, http.StatusOK, "data: {\"error_code\":555,\"error_message\":\"blocked by sentinel rate limit\"}\n\n"), nil
			})
			response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "accio-test", "messages": []any{map[string]string{"role": "user", "content": "hi"}}, "stream": stream}, nil)
			if response.Code != http.StatusTooManyRequests || gjson.GetBytes(response.Body.Bytes(), "error.code").String() != "rate_limit_exceeded" {
				t.Fatalf("sentinel rejection=%d %s", response.Code, response.Body.String())
			}
		})
	}
	t.Run("admin-stream", func(t *testing.T) {
		env := setupProxyTestEnv(t, []testChannel{{name: "accio", authType: model.AuthTypeAccioOAuth, oauthCredential: accioTestCredential(t, "global", "access"), models: "accio-test,spare-model", upstreamProtocol: "openai"}}, map[int]string{0: "https://phoenix-gw.alibaba.com/v1/chat/completions"})
		var refreshes atomic.Int32
		accioTestClient(env.server, func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/api/llm/config":
				return accioTestResponse(req, http.StatusOK, accioTestCatalog), nil
			case "/api/auth/refresh_token":
				refreshes.Add(1)
				return accioTestResponse(req, http.StatusUnauthorized, `{}`), nil
			default:
				return accioTestResponse(req, http.StatusOK, "data: {\"error_code\":555,\"error_message\":\"blocked by sentinel rate limit\"}\n\n"), nil
			}
		})
		configs, err := env.store.ListConfigs(context.Background())
		if err != nil || len(configs) != 1 {
			t.Fatalf("configs=%d err=%v", len(configs), err)
		}
		c, response := newTestContext(t, newJSONRequest(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", configs[0].ID), map[string]any{"model": "accio-test", "client_protocol": "openai", "content": "hi", "stream": true}))
		c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(configs[0].ID)}}
		env.server.HandleChannelTest(c)
		data := gjson.GetBytes(response.Body.Bytes(), "data")
		if response.Code != http.StatusOK || data.Get("success").Bool() || data.Get("status_code").Int() != http.StatusOK || data.Get("api_error.error.status").Int() != http.StatusTooManyRequests || data.Get("api_error.status").Int() != http.StatusTooManyRequests {
			t.Fatalf("admin sentinel rejection=%d %s", response.Code, response.Body.String())
		}
		if data.Get("cooldown_action").String() != "model_cooldown_applied" || refreshes.Load() != 0 {
			t.Fatalf("sentinel classification=%s refreshes=%d", data.Get("cooldown_action"), refreshes.Load())
		}
	})
}

func TestAccioSystemSensitiveWordsOnWire(t *testing.T) {
	const messagesJSON = `[
		{"role":"system","content":"Claude Codex codex CODEX API unchanged"},
		{"role":"developer","content":"Claude Codex cLaUdE api unchanged"},
		{"role":"system","content":[{"type":"text","text":"Claude Codex CLAUDE API"},{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude. You are a coding agent running in the Codex CLI."},{"type":"text","text":"ordinary policy"}]},
		{"role":"developer","content":[{"type":"text","text":"claude Api"},{"type":"text","text":"ordinary instruction"}]},
		{"role":"user","content":"Claude Codex API"},
		{"role":"assistant","content":"Claude Codex API","tool_calls":[{"id":"call-1","type":"function","function":{"name":"Claude_API","arguments":"{\"text\":\"Claude Codex API\"}"}}]},
		{"role":"tool","tool_call_id":"call-1","content":"Claude Codex API"},
		{"role":"user","content":[{"type":"text","text":"cLaUdE api"}]},
		{"role":"assistant","content":[{"type":"text","text":"CLAUDE Api"}]}
	]`
	const toolsJSON = `[{"type":"function","function":{"name":"Claude_API","description":"Claude Codex API","parameters":{"type":"object","properties":{"text":{"type":"string","description":"Claude Codex API","enum":["Claude Codex API"]}}}}}]`
	const wantContentsJSON = `[
		{"role":"user","parts":[{"text":"Claude Codex API"}]},
		{"role":"model","parts":[{"text":"Claude Codex API"},{"function_call":{"id":"call-1","name":"Claude_API","args_json":{"text":"Claude Codex API"}}}]},
		{"role":"tool","parts":[{"function_response":{"id":"call-1","name":"Claude_API","response_json":{"content":"Claude Codex API","is_error":false}}}]},
		{"role":"user","parts":[{"text":"cLaUdE api"}]},
		{"role":"model","parts":[{"text":"CLAUDE Api"}]}
	]`
	const wantToolsJSON = `[{"name":"Claude_API","description":"Claude Codex API","parameters_json":{"type":"object","properties":{"text":{"type":"string","description":"Claude Codex API","enum":["Claude Codex API"]}}}}]`
	for _, path := range []string{"proxy", "admin"} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/enabled=%v", path, enabled), func(t *testing.T) {
				words := `[]`
				if enabled {
					words = `["Claude","Codex","Anthropic","API"]`
				}
				rules := &model.CustomRequestRules{Body: []model.CustomBodyRule{
					{Action: model.RuleActionOverride, Path: "messages", Value: json.RawMessage(messagesJSON)},
					{Action: model.RuleActionOverride, Path: "tools", Value: json.RawMessage(toolsJSON)},
				}}
				env := setupProxyTestEnvWithSettings(t, []testChannel{{name: "accio", authType: model.AuthTypeAccioOAuth, oauthCredential: accioTestCredential(t, "global", "sensitive-access"), models: "accio-test", upstreamProtocol: "openai", customRequestRules: rules}}, map[int]string{0: "https://phoenix-gw.alibaba.com/v1/chat/completions"}, map[string]string{"antigravity_sensitive_words": words})
				var chats atomic.Int32
				accioTestClient(env.server, func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/api/llm/config" {
						return accioTestResponse(r, 200, accioTestCatalog), nil
					}
					if r.URL.Path != "/api/adk/llm/generateContent" {
						t.Errorf("unexpected upstream path: %s", r.URL.Path)
						return accioTestResponse(r, 404, `{}`), nil
					}
					chats.Add(1)
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					var actual struct {
						SystemInstruction string           `json:"system_instruction"`
						Contents          []map[string]any `json:"contents"`
						Tools             []map[string]any `json:"tools"`
					}
					mustUnmarshalJSON(t, raw, &actual)
					wantSystem := []string{"Accio Accio codex CODEX API unchanged", "Accio Accio cLaUdE api unchanged", "Accio Accio CLAUDE API", "You are Accio Code, Anthropic's official CLI for Accio. You are a coding agent running in the Accio CLI.", "ordinary policy", "claude Api", "ordinary instruction"}
					if enabled {
						wantSystem[0] = "Accio Accio c\u034fodex C\u034fODEX A\u034fPI unchanged"
						wantSystem[1] = "Accio Accio c\u034fLaUdE a\u034fpi unchanged"
						wantSystem[2] = "Accio Accio C\u034fLAUDE A\u034fPI"
						wantSystem[3] = "You are Accio Code, A\u034fnthropic's official CLI for Accio. You are a coding agent running in the Accio CLI."
						wantSystem[5] = "c\u034flaude A\u034fpi"
					}
					if want := strings.Join(wantSystem, "\n\n"); actual.SystemInstruction != want {
						t.Errorf("wire system_instruction = %q, want %q", actual.SystemInstruction, want)
					}
					if len(actual.Contents) != 5 || len(actual.Tools) != 1 {
						t.Fatalf("unexpected ADK contents/tools: %s", raw)
					}
					// ADK embeds these JSON values as strings; compare their decoded structures.
					decodeJSON := func(path string) any {
						var value any
						mustUnmarshalJSON(t, []byte(gjson.GetBytes(raw, path).String()), &value)
						return value
					}
					actual.Contents[1]["parts"].([]any)[1].(map[string]any)["function_call"].(map[string]any)["args_json"] = decodeJSON("contents.1.parts.1.function_call.args_json")
					actual.Contents[2]["parts"].([]any)[0].(map[string]any)["function_response"].(map[string]any)["response_json"] = decodeJSON("contents.2.parts.0.function_response.response_json")
					actual.Tools[0]["parameters_json"] = decodeJSON("tools.0.parameters_json")
					var wantContents, wantTools []map[string]any
					mustUnmarshalJSON(t, []byte(wantContentsJSON), &wantContents)
					mustUnmarshalJSON(t, []byte(wantToolsJSON), &wantTools)
					if !reflect.DeepEqual(actual.Contents, wantContents) {
						t.Errorf("wire contents = %+v, want %+v", actual.Contents, wantContents)
					}
					if !reflect.DeepEqual(actual.Tools, wantTools) {
						t.Errorf("wire tools = %+v, want %+v", actual.Tools, wantTools)
					}
					return accioTestResponse(r, 200, accioTestCompletion), nil
				})
				if path == "proxy" {
					response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "accio-test", "messages": []any{map[string]string{"role": "user", "content": "test"}}}, nil)
					if response.Code != http.StatusOK {
						t.Fatalf("proxy status=%d body=%s", response.Code, response.Body.String())
					}
				} else {
					configs, err := env.store.ListConfigs(context.Background())
					if err != nil || len(configs) != 1 {
						t.Fatalf("configs=%d err=%v", len(configs), err)
					}
					result := env.server.testChannelAPI(context.Background(), configs[0], "sensitive-access", &testutil.TestChannelRequest{Model: "accio-test", ClientProtocol: "openai", Content: "test"})
					if result["success"] != true {
						t.Fatalf("channel test failed: %+v", result)
					}
				}
				if chats.Load() != 1 {
					t.Fatalf("chat requests=%d, want one", chats.Load())
				}
			})
		}
	}
}

func TestAccioProxyRefreshesNativeUnauthorizedOnce(t *testing.T) {
	credential := accioTestCredential(t, "cn", "dt-rejected-secret")
	env := setupProxyTestEnv(t, []testChannel{{name: "accio-refresh", authType: model.AuthTypeAccioOAuth, oauthCredential: credential, models: "accio-test", upstreamProtocol: "openai"}}, map[int]string{0: "https://phoenix-gw.alibaba.com/v1/chat/completions"})
	var chats, refreshes atomic.Int32
	accioTestClient(env.server, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/llm/config":
			return accioTestResponse(r, 200, accioTestCatalog), nil
		case "/api/auth/refresh_token":
			refreshes.Add(1)
			if r.Header.Get("Authorization") != "" {
				t.Error("refresh leaked old bearer")
			}
			return accioTestResponse(r, 200, `{"success":true,"data":{"accessToken":"dt-recovered-secret","refreshToken":"refresh-recovered-secret","userId":"same-user","sgcookie":"rotated-session-cookie"}}`), nil
		default:
			attempt := chats.Add(1)
			body := `data: {"error_code":"401","error_message":"token expired"}` + "\n\n"
			if attempt > 1 {
				if r.Header.Get("sgcookie") != "rotated-session-cookie" {
					t.Error("retry omitted the rotated session cookie")
				}
				body = strings.ReplaceAll(accioTestCompletion, "hello", "recovered")
			}
			response := accioTestResponse(r, 200, body)
			response.Header.Set("Content-Type", "text/event-stream")
			return response, nil
		}
	})
	req := newJSONRequest(t, "POST", "/v1/chat/completions", map[string]any{"model": "accio-test", "messages": []any{map[string]string{"role": "user", "content": "hello"}}, "stream": false})
	req.Header.Set("Authorization", "Bearer test-api-key")
	w := httptest.NewRecorder()
	env.engine.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("proxy status=%d body=%s chats=%d refreshes=%d", w.Code, w.Body.String(), chats.Load(), refreshes.Load())
	}
	var result struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	mustUnmarshalJSON(t, w.Body.Bytes(), &result)
	if len(result.Choices) != 1 || result.Choices[0].Message.Content != "recovered" || chats.Load() != 2 || refreshes.Load() != 1 {
		t.Fatalf("native auth refresh response=%s chats=%d refreshes=%d", w.Body.String(), chats.Load(), refreshes.Load())
	}
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("channels=%d err=%v", len(configs), err)
	}
	stored, err := accioauth.ParseCredential([]byte(configs[0].OAuthCredential))
	if err != nil || stored.SGCookie != "rotated-session-cookie" {
		t.Fatal("refresh did not persist the rotated session cookie")
	}
	for _, secret := range []string{"dt-rejected-secret", "refresh-secret", "dt-recovered-secret", "refresh-recovered-secret"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("secret leaked")
		}
	}
}
