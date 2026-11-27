package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/qoderauth"
	"ccLoad/internal/storage"
	"ccLoad/internal/testutil"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
)

func qoderIntegrationCredential(t *testing.T, region, token string) *qoderauth.Credential {
	t.Helper()
	return &qoderauth.Credential{Type: qoderauth.ChannelType, Region: region, AccessToken: token, RefreshToken: "refresh-secret", UserID: "same-user", MachineID: "machine-secret", ExpiresAt: time.Now().Add(time.Hour).Unix()}
}
func qoderIntegrationJSON(t *testing.T, c *qoderauth.Credential) string {
	t.Helper()
	raw, err := c.JSON()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func qoderIntegrationResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}
func qoderIntegrationClient(s *Server, transport roundTripperFunc) {
	s.client = &http.Client{Transport: transport}
	s.qoderService.Client = s.client
}
func qoderIntegrationContext(t *testing.T, method, path string, body any, id int64) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	c, w := newTestContext(t, newJSONRequest(t, method, path, body))
	if id > 0 {
		c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(id)}}
	}
	return c, w
}
func qoderIntegrationNoSecrets(t *testing.T, body string, secrets ...string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(body, secret) {
			t.Fatalf("response leaked a private credential field: %s", body)
		}
	}
}

func TestQoderImportReauthorizesByRegionAndUser(t *testing.T) {
	s := newInMemoryServer(t)
	var profileCalls, catalogCalls atomic.Int32
	qoderIntegrationClient(s, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/v1/userinfo":
			profileCalls.Add(1)
			return qoderIntegrationResponse(r, 200, `{"id":"same-user","name":"User","email":"same@example.com"}`), nil
		case "/algo/api/v2/model/list":
			catalogCalls.Add(1)
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer COSY.") {
				t.Error("catalog request was not signed")
			}
			return qoderIntegrationResponse(r, 200, `{"chat":[{"key":"native-key","display_name":"Qwen Test","enable":true}]}`), nil
		default:
			t.Errorf("unexpected import request %s", r.URL)
			return qoderIntegrationResponse(r, 404, `{}`), nil
		}
	})
	var ids []int64
	for index, region := range []string{"cn", "cn", "global"} {
		credential := qoderIntegrationCredential(t, region, fmt.Sprintf("dt-secret-%d", index))
		credential.PAT = "pat-secret"
		c, w := qoderIntegrationContext(t, "POST", "/admin/qoder/credentials/import", map[string]any{"credential": credential}, 0)
		s.HandleImportQoderCredential(c)
		if w.Code != 200 {
			t.Fatalf("import status=%d body=%s", w.Code, w.Body.String())
		}
		qoderIntegrationNoSecrets(t, w.Body.String(), credential.AccessToken, credential.RefreshToken, credential.PAT, credential.MachineID)
		var result struct {
			ID      int64 `json:"channel_id"`
			Created bool  `json:"created"`
		}
		mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &result)
		if result.Created != (index != 1) {
			t.Fatalf("created=%t for import %d", result.Created, index)
		}
		ids = append(ids, result.ID)
	}
	if ids[0] != ids[1] || ids[2] == ids[0] {
		t.Fatalf("region/account deduplication IDs=%v", ids)
	}
	configs, err := s.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 2 {
		t.Fatalf("configs=%d err=%v", len(configs), err)
	}
	for _, cfg := range configs {
		if cfg.AuthType != model.AuthTypeQoderOAuth || !cfg.Enabled || cfg.ProtocolTransformMode != model.ProtocolTransformModeLocal || len(cfg.URLs) != 1 || !cfg.URLs[0].Exact || !cfg.SupportsModel("qwentest") {
			t.Fatalf("invalid Qoder channel: %+v", cfg)
		}
	}
	listContext, listResponse := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
	s.HandleChannels(listContext)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("channel list status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}
	qoderIntegrationNoSecrets(t, listResponse.Body.String(), "dt-secret-0", "dt-secret-1", "dt-secret-2", "refresh-secret", "pat-secret", "machine-secret")
	if profileCalls.Load() != 3 || catalogCalls.Load() != 2 {
		t.Fatalf("profile=%d catalog=%d", profileCalls.Load(), catalogCalls.Load())
	}
}

func TestQoderImportRejectsRegionMismatchAndRedactsUpstreamFailure(t *testing.T) {
	s := newInMemoryServer(t)
	var calls atomic.Int32
	qoderIntegrationClient(s, func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		return qoderIntegrationResponse(r, 403, `{"error":"dt-private-access refresh-private-token pat-private-token machine-private"}`), nil
	})
	credential := qoderIntegrationCredential(t, "cn", "dt-private-access")
	c, w := qoderIntegrationContext(t, "POST", "/admin/qoder/credentials/import", map[string]any{"region": "global", "credential": credential}, 0)
	s.HandleImportQoderCredential(c)
	if w.Code != 400 || calls.Load() != 0 {
		t.Fatalf("mismatched region status=%d requests=%d", w.Code, calls.Load())
	}
	c, w = qoderIntegrationContext(t, "POST", "/admin/qoder/credentials/import", map[string]any{"credential": credential}, 0)
	s.HandleImportQoderCredential(c)
	if w.Code != 400 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	qoderIntegrationNoSecrets(t, w.Body.String(), "dt-private-access", "refresh-private-token", "pat-private-token", "machine-private")
	configs, err := s.store.ListConfigs(context.Background())
	if err != nil || len(configs) != 0 {
		t.Fatalf("failed import created channels: %v", err)
	}
}

func TestQoderRefreshConcurrentReauthorizationWins(t *testing.T) {
	for _, upstreamFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(upstreamFailure), func(t *testing.T) {
			s := newInMemoryServer(t)
			credential := qoderIntegrationCredential(t, "cn", "dt-old-secret")
			cfg, err := s.store.CreateConfig(context.Background(), newQoderChannel("refresh", qoderIntegrationJSON(t, credential)))
			if err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			var requests atomic.Int32
			qoderIntegrationClient(s, func(r *http.Request) (*http.Response, error) {
				requests.Add(1)
				once.Do(func() { close(started) })
				select {
				case <-release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				if upstreamFailure {
					return qoderIntegrationResponse(r, 401, `{"error":"expired"}`), nil
				}
				return qoderIntegrationResponse(r, 200, `{"device_token":"dt-refreshed-old-account","refresh_token":"rotated-old-refresh","user_id":"same-user"}`), nil
			})
			const concurrency = 8
			results := make(chan *qoderauth.Credential, concurrency)
			errs := make(chan error, concurrency)
			for range concurrency {
				go func() {
					c, err := s.qoderCredentials.credential(context.Background(), cfg, true, credential.AccessToken)
					results <- c
					errs <- err
				}()
			}
			<-started
			winner := qoderIntegrationCredential(t, "cn", "dt-reauthorized-winner")
			winner.RefreshToken = "reauthorized-refresh"
			if changed, err := s.store.CompareAndSwapOAuthCredential(context.Background(), cfg.ID, model.AuthTypeQoderOAuth, cfg.OAuthCredential, qoderIntegrationJSON(t, winner)); err != nil || !changed {
				t.Fatalf("reauthorization CAS=%t error=%v", changed, err)
			}
			close(release)
			for range concurrency {
				if err := <-errs; err != nil {
					t.Fatal(err)
				}
				if c := <-results; c.AccessToken != winner.AccessToken {
					t.Fatalf("stale refresh won: %+v", c)
				}
			}
			stored, err := s.store.GetConfig(context.Background(), cfg.ID)
			if err != nil {
				t.Fatal(err)
			}
			persisted, err := qoderauth.ParseCredential([]byte(stored.OAuthCredential))
			if err != nil || persisted.AccessToken != winner.AccessToken || persisted.RefreshToken != winner.RefreshToken {
				t.Fatalf("persisted stale refresh: %+v err=%v", persisted, err)
			}
			if requests.Load() != 1 {
				t.Fatalf("refresh requests=%d, want one", requests.Load())
			}
		})
	}
}

func TestQoderScheduledCheckinClaimsOncePerWindowWithoutOptIn(t *testing.T) {
	s := newInMemoryServer(t)
	credential := qoderIntegrationCredential(t, "cn", "dt-scheduled-checkin")
	cfg, err := s.store.CreateConfig(context.Background(), newQoderChannel("scheduled-checkin", qoderIntegrationJSON(t, credential)))
	if err != nil {
		t.Fatal(err)
	}
	var claims atomic.Int32
	dailyAvailable := false
	qoderIntegrationClient(s, func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/me/campaigns"):
			rows := `{"campaignId":"sonus","title":"Sonus Credits","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED","benefit":{"kind":"CREDITS"}}`
			if dailyAvailable {
				rows += `,{"campaignId":"daily","title":"Daily Credits","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","benefit":{"kind":"CREDITS"}}`
			}
			return qoderIntegrationResponse(r, 200, `{"campaigns":[`+rows+`]}`), nil
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/me/campaigns/daily/claim"):
			claims.Add(1)
			return qoderIntegrationResponse(r, 200, `{"status":"CLAIMED"}`), nil
		}
		return qoderIntegrationResponse(r, 404, `{}`), nil
	})
	s.runDueQoderCheckins(context.Background())
	dailyAvailable = true
	for range 2 {
		s.runDueQoderCheckins(context.Background())
	}
	if claims.Load() != 1 {
		t.Fatalf("scheduled claims=%d, want one per window", claims.Load())
	}
	stored, err := s.store.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := qoderauth.ParseCredential([]byte(stored.OAuthCredential))
	if err != nil || persisted.LastCheckinCampaign != "daily" || persisted.LastCheckinAt < qoderauth.CheckinWindow(time.Now()).Unix() {
		t.Fatalf("scheduled check-in not persisted: %+v %v", persisted, err)
	}
}

/**
 * @brief 持久化签到依据活动选择时刻，并保留并发写入的新窗口签到。
 * @param t 测试上下文。
 */
func TestQoderCheckinPersistsSelectedWindow(t *testing.T) {
	s := newInMemoryServer(t)
	ctx := context.Background()
	credential := qoderIntegrationCredential(t, "cn", "dt-window-checkin")
	cfg, err := s.store.CreateConfig(ctx, newQoderChannel("window-checkin", qoderIntegrationJSON(t, credential)))
	if err != nil {
		t.Fatal(err)
	}
	window := time.Date(2026, 10, 19, 2, 0, 0, 0, time.UTC)
	previous := qoderauth.CheckinResult{Status: "claimed", CampaignID: "previous", SelectedAt: window.Add(-time.Second)}
	current := qoderauth.CheckinResult{Status: "already_claimed", CampaignID: "current", SelectedAt: window.Add(time.Second)}
	for _, step := range []struct {
		result qoderauth.CheckinResult
		want   qoderauth.CheckinResult
	}{
		{previous, previous},
		{current, current},
		{previous, current},
	} {
		if err := s.updateQoderCredential(ctx, cfg.ID, credential, step.result.UpdateCredential); err != nil {
			t.Fatal(err)
		}
		stored, err := s.store.GetConfig(ctx, cfg.ID)
		if err != nil {
			t.Fatal(err)
		}
		persisted, err := qoderauth.ParseCredential([]byte(stored.OAuthCredential))
		if err != nil || persisted.LastCheckinAt != step.want.SelectedAt.Unix() || persisted.LastCheckinCampaign != step.want.CampaignID {
			t.Fatalf("persisting %s: got %+v, err=%v, want %s at %v", step.result.CampaignID, persisted, err, step.want.CampaignID, step.want.SelectedAt)
		}
	}
}

func TestQoderCheckinWindowAtTenInUTCPlusEight(t *testing.T) {
	for _, tt := range []struct{ now, want string }{
		{"2026-10-19T01:59:59Z", "2026-10-18T02:00:00Z"},
		{"2026-10-19T02:00:00Z", "2026-10-19T02:00:00Z"},
		{"2026-10-19T02:00:01Z", "2026-10-19T02:00:00Z"},
		{"2027-01-01T01:59:59Z", "2026-12-31T02:00:00Z"},
	} {
		now, err := time.Parse(time.RFC3339, tt.now)
		if err != nil {
			t.Fatal(err)
		}
		want, err := time.Parse(time.RFC3339, tt.want)
		if err != nil {
			t.Fatal(err)
		}
		if got := qoderauth.CheckinWindow(now); !got.Equal(want) {
			t.Errorf("window(%s)=%s want %s", now, got, want)
		}
	}
}

func qoderIntegrationEvent(value any) string {
	body, _ := json.Marshal(value)
	envelope, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": string(body)})
	return "data: " + string(envelope) + "\n\n"
}

/**
 * @brief 验证心跳后的工具调用和用量通过流式及非流式代理返回给客户端。
 * @param t 测试上下文。
 */
func TestQoderProxyStreamAndNonStreamKeepToolsAndUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			credential := qoderIntegrationCredential(t, "cn", "dt-proxy-secret")
			// The stored mixed-case name must still resolve against the lowercase catalog ID "qwentest".
			env := setupProxyTestEnv(t, []testChannel{{name: "qoder", authType: model.AuthTypeQoderOAuth, oauthCredential: qoderIntegrationJSON(t, credential), models: "QwenTest", upstreamProtocol: "openai"}}, map[int]string{0: "https://gateway.qoder.com.cn/v1/chat/completions"})
			var chatCalls atomic.Int32
			qoderIntegrationClient(env.server, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/algo/api/v2/model/list" {
					return qoderIntegrationResponse(r, 200, `{"chat":[{"key":"native-key","display_name":"Qwen Test","enable":true}]}`), nil
				}
				chatCalls.Add(1)
				if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer COSY.") {
					t.Error("chat was not signed")
				}
				body := qoderIntegrationEvent(map[string]any{"id": "chat-qoder", "model": "native-key", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hello", "tool_calls": []any{map[string]any{"index": 0, "id": "call-1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{\"q\":"}}}}, "finish_reason": nil}}})
				body += qoderIntegrationEvent(map[string]any{"id": "chat-qoder", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": "1}"}}}}, "finish_reason": "tool_calls"}}})
				body += qoderIntegrationEvent(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 7, "total_tokens": 17, "prompt_tokens_details": map[string]any{"cached_tokens": 3}}})
				body += "data: {\"statusCodeValue\":200,\"body\":\"[DONE]\"}\n\n"
				response := qoderIntegrationResponse(r, 200, "data: {\"isQueued\":false}\n\n"+body)
				response.Header.Set("Content-Type", "text/event-stream")
				return response, nil
			})
			req := newJSONRequest(t, "POST", "/v1/chat/completions", map[string]any{"model": "QwenTest", "messages": []any{map[string]string{"role": "user", "content": "lookup"}}, "stream": stream, "stream_options": map[string]bool{"include_usage": true}})
			req.Header.Set("Authorization", "Bearer test-api-key")
			w := httptest.NewRecorder()
			env.engine.ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatalf("proxy status=%d body=%s", w.Code, w.Body.String())
			}
			qoderIntegrationNoSecrets(t, w.Body.String(), credential.AccessToken, credential.RefreshToken, credential.MachineID)
			var events []map[string]any
			if stream {
				scanner := bufio.NewScanner(bytes.NewReader(w.Body.Bytes()))
				for scanner.Scan() {
					line := scanner.Text()
					if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
						var event map[string]any
						if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
							t.Fatal(err)
						}
						events = append(events, event)
					}
				}
				if err := scanner.Err(); err != nil {
					t.Fatal(err)
				}
			} else {
				var event map[string]any
				mustUnmarshalJSON(t, w.Body.Bytes(), &event)
				events = append(events, event)
			}
			var content, arguments, name string
			var total float64
			for _, event := range events {
				if usage, ok := event["usage"].(map[string]any); ok {
					total, _ = usage["total_tokens"].(float64)
				}
				choices, _ := event["choices"].([]any)
				for _, raw := range choices {
					choice := raw.(map[string]any)
					message, _ := choice["delta"].(map[string]any)
					if message == nil {
						message, _ = choice["message"].(map[string]any)
					}
					if text, ok := message["content"].(string); ok {
						content += text
					}
					tools, _ := message["tool_calls"].([]any)
					for _, raw := range tools {
						tool := raw.(map[string]any)
						function, _ := tool["function"].(map[string]any)
						if v, ok := function["name"].(string); ok {
							name += v
						}
						if v, ok := function["arguments"].(string); ok {
							arguments += v
						}
					}
				}
			}
			if content != "hello" || arguments != `{"q":1}` || name != "lookup" || total != 17 || chatCalls.Load() != 1 {
				t.Fatalf("content=%q arguments=%q name=%q tokens=%v calls=%d body=%s", content, arguments, name, total, chatCalls.Load(), w.Body.String())
			}
		})
	}
}

/**
 * @brief 验证代理和管理测试先处理 COSY 系统提示词，再压缩已签名的请求。
 * @param t 测试上下文。
 */
func TestQoderSystemSensitiveWordsOnWire(t *testing.T) {
	const messagesJSON = `[
		{"role":"system","content":"Claude API unchanged"},
		{"role":"developer","content":"cLaUdE api unchanged"},
		{"role":"system","content":[{"type":"text","text":"CLAUDE API"},{"type":"text","text":"ordinary policy"}]},
		{"role":"developer","content":[{"type":"text","text":"claude Api"},{"type":"text","text":"ordinary instruction"}]},
		{"role":"user","content":"Claude API"},
		{"role":"assistant","content":"Claude API","tool_calls":[{"id":"call-1","type":"function","function":{"name":"Claude_API","arguments":"{\"text\":\"Claude API\"}"}}]},
		{"role":"tool","tool_call_id":"call-1","content":"Claude API"},
		{"role":"user","content":[{"type":"text","text":"cLaUdE api"}]}
	]`
	const toolsJSON = `[{"type":"function","function":{"name":"Claude_API","description":"Claude API","parameters":{"type":"object","properties":{"text":{"type":"string","description":"Claude API","enum":["Claude API"]}}}}}]`
	for _, path := range []string{"proxy", "admin"} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/enabled=%v", path, enabled), func(t *testing.T) {
				words := `[]`
				if enabled {
					words = `["Claude","API"]`
				}
				credential := qoderIntegrationCredential(t, "cn", "dt-sensitive-word-test")
				rules := &model.CustomRequestRules{Body: []model.CustomBodyRule{
					{Action: model.RuleActionOverride, Path: "messages", Value: json.RawMessage(messagesJSON)},
					{Action: model.RuleActionOverride, Path: "tools", Value: json.RawMessage(toolsJSON)},
				}}
				env := setupProxyTestEnvWithSettings(t, []testChannel{{name: "qoder", authType: model.AuthTypeQoderOAuth, oauthCredential: qoderIntegrationJSON(t, credential), models: "QwenTest", upstreamProtocol: "openai", customRequestRules: rules}}, map[int]string{0: "https://gateway.qoder.com.cn/v1/chat/completions"}, map[string]string{"antigravity_sensitive_words": words})
				var chatCalls atomic.Int32
				qoderIntegrationClient(env.server, func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/algo/api/v2/model/list" {
						if encoding := r.Header.Get("Content-Encoding"); encoding != "" || r.ContentLength != 0 {
							t.Errorf("catalog Content-Encoding = %q, Content-Length = %d", encoding, r.ContentLength)
						}
						return qoderIntegrationResponse(r, 200, `{"chat":[{"key":"native-key","display_name":"Qwen Test","enable":true}]}`), nil
					}
					chatCalls.Add(1)
					if got := r.Header.Get("Content-Encoding"); got != "zstd" {
						t.Fatalf("inference Content-Encoding = %q, want zstd", got)
					}
					decoded := qoderIntegrationDecodeSignedRequest(t, r)
					var actual struct {
						Messages []map[string]any `json:"messages"`
						Tools    []map[string]any `json:"tools"`
					}
					mustUnmarshalJSON(t, decoded, &actual)
					var wantTools []map[string]any
					mustUnmarshalJSON(t, []byte(toolsJSON), &wantTools)
					system := "You are a Qoder agent. Use the instructions below and the tools available to you to assist the user."
					instructions := []string{"Claude API unchanged", "cLaUdE api unchanged", "CLAUDE APIordinary policy", "claude Apiordinary instruction"}
					if enabled {
						instructions = []string{"C\u034flaude A\u034fPI unchanged", "c\u034fLaUdE a\u034fpi unchanged", "C\u034fLAUDE A\u034fPIordinary policy", "c\u034flaude A\u034fpiordinary instruction"}
					}
					wantText := []string{system + "\n\n" + strings.Join(instructions, "\n\n"), "Claude API", "Claude API", "Claude API", "cLaUdE api"}
					wantRoles := []string{"system", "user", "assistant", "tool", "user"}
					if len(actual.Messages) != len(wantText) {
						t.Fatalf("wire messages=%+v", actual.Messages)
					}
					for i, message := range actual.Messages {
						text, _ := message["content"].(string)
						if parts, ok := message["content"].([]any); ok {
							for _, part := range parts {
								value, _ := part.(map[string]any)["text"].(string)
								text += value
							}
						}
						if text != wantText[i] || message["role"] != wantRoles[i] {
							t.Errorf("message %d=%+v want role=%s text=%q", i, message, wantRoles[i], wantText[i])
						}
					}
					calls := actual.Messages[2]["tool_calls"].([]any)
					if calls[0].(map[string]any)["function"].(map[string]any)["arguments"] != `{"text":"Claude API"}` {
						t.Error("tool arguments changed")
					}
					if !reflect.DeepEqual(actual.Tools, wantTools) {
						t.Errorf("wire tools = %+v, want %+v", actual.Tools, wantTools)
					}
					response := qoderIntegrationResponse(r, http.StatusOK, qoderIntegrationEvent(map[string]any{"id": "chat-sensitive", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "ok"}, "finish_reason": "stop"}}})+"data: {\"statusCodeValue\":200,\"body\":\"[DONE]\"}\n\n")
					response.Header.Set("Content-Type", "text/event-stream")
					return response, nil
				})
				if path == "proxy" {
					response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "QwenTest", "messages": []any{map[string]any{"role": "user", "content": "test"}}}, nil)
					if response.Code != http.StatusOK {
						t.Fatalf("proxy status=%d body=%s", response.Code, response.Body.String())
					}
				} else {
					configs, err := env.store.ListConfigs(context.Background())
					if err != nil || len(configs) != 1 {
						t.Fatalf("configs=%d err=%v", len(configs), err)
					}
					result := env.server.testChannelAPI(context.Background(), configs[0], credential.AccessToken, &testutil.TestChannelRequest{Model: "QwenTest", ClientProtocol: "openai", Content: "test"})
					if result["success"] != true {
						t.Fatalf("channel test failed: %+v", result)
					}
				}
				if chatCalls.Load() != 1 {
					t.Fatalf("chat requests=%d, want one", chatCalls.Load())
				}
			})
		}
	}
}

/**
 * @brief 解出 COSY 信封并验证签名和摘要覆盖传输解压后的字节。
 * @param t 测试上下文。
 * @param req 出站推理请求。
 * @return COSY JSON 信封。
 */
func qoderIntegrationDecodeSignedRequest(t *testing.T, req *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if req.ContentLength != int64(len(body)) {
		t.Errorf("inference Content-Length = %d, wire size = %d", req.ContentLength, len(body))
	}
	if req.Header.Get("Content-Encoding") == "zstd" {
		decoder, err := zstd.NewReader(nil)
		if err != nil {
			t.Fatal(err)
		}
		defer decoder.Close()
		body, err = decoder.DecodeAll(body, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	parts := strings.Split(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer COSY."), ".")
	if len(parts) != 2 {
		t.Fatalf("invalid COSY authorization: %q", req.Header.Get("Authorization"))
	}
	path := strings.TrimPrefix(req.URL.Path, "/algo")
	signature := md5.Sum([]byte(parts[0] + "\n" + req.Header.Get("Cosy-Key") + "\n" + req.Header.Get("Cosy-Date") + "\n" + string(body) + "\n" + path))
	if parts[1] != hex.EncodeToString(signature[:]) || req.Header.Get("Cosy-Sigpath") != path {
		t.Error("COSY signature does not cover the encoded body and path")
	}
	bodyHash := md5.Sum(body)
	if req.Header.Get("Cosy-Bodyhash") != hex.EncodeToString(bodyHash[:]) || req.Header.Get("Cosy-Bodylength") != strconv.Itoa(len(body)) {
		t.Error("COSY body digest/length do not match the encoded body")
	}
	const alphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"
	const standard = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	mapped := make([]byte, len(body))
	for i, b := range body {
		if b == '$' {
			mapped[i] = '='
		} else if index := strings.IndexByte(alphabet, b); index >= 0 {
			mapped[i] = standard[index]
		} else {
			t.Fatalf("invalid Qoder encoding byte: %q", b)
		}
	}
	n := len(mapped) / 3
	standardBody := append(bytes.Clone(mapped[len(mapped)-n:]), mapped[n:len(mapped)-n]...)
	standardBody = append(standardBody, mapped[:n]...)
	decoded, err := base64.StdEncoding.DecodeString(string(standardBody))
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

/**
 * @brief Qoder 推理返回压缩拒绝时重新签名并保留推理内容，成功后直接发送明文。
 * @param t 测试上下文。
 */
func TestQoderInferenceCompressionFallback(t *testing.T) {
	const sseForbidden = "data: {\"statusCodeValue\":403,\"body\":{\"message\":\"Forbidden\"}}\n\n"
	const sseSignatureInvalid = `data: {"statusCode":"FORBIDDEN","statusCodeValue":403,"body":"{\"message\":\"signature invalid\"}"}` + "\n\n"
	for _, tc := range []struct {
		name, region, host string
		admin              bool
		rejection          string
		content, finish    string
		wantEncodings      []string
	}{
		{"proxy SSE forbidden", "cn", "gateway.qoder.com.cn", false, sseForbidden, "replayed", "stop", []string{"zstd", "", ""}},
		{"proxy SSE signature invalid", "global", "api3.qoder.sh", false, sseSignatureInvalid, "replayed", "stop", []string{"zstd", "", ""}},
		{"proxy empty length", "global", "api3.qoder.sh", false, sseSignatureInvalid, "", "length", []string{"zstd", "", ""}},
		{"proxy empty content filter", "global", "api3.qoder.sh", false, sseSignatureInvalid, "", "content_filter", []string{"zstd", "", ""}},
		{"admin HTTP 415", "global", "api3.qoder.sh", true, sseForbidden, "replayed", "stop", []string{"zstd", "", "zstd", "", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credential := qoderIntegrationCredential(t, tc.region, "dt-compression")
			env := setupProxyTestEnv(t, []testChannel{{name: "qoder", authType: model.AuthTypeQoderOAuth, oauthCredential: qoderIntegrationJSON(t, credential), models: "QwenTest", upstreamProtocol: "openai"}}, map[int]string{0: "https://" + tc.host + "/v1/chat/completions"})
			var encodings []string
			var compressedBody gjson.Result
			var compressedHeaders http.Header
			qoderIntegrationClient(env.server, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/algo/api/v2/model/list":
					return qoderIntegrationResponse(r, http.StatusOK, `{"chat":[{"key":"native-key","display_name":"Qwen Test","enable":true}]}`), nil
				case "/algo/api/v2/service/pro/sse/agent_chat_generation":
					if r.URL.Host != tc.host {
						t.Errorf("inference host = %q, want %q", r.URL.Host, tc.host)
					}
				default:
					t.Fatalf("unexpected upstream request: %s", r.URL)
				}
				encoding := r.Header.Get("Content-Encoding")
				encodings = append(encodings, encoding)
				wire := gjson.ParseBytes(qoderIntegrationDecodeSignedRequest(t, r))
				if encoding == "zstd" {
					compressedBody = wire
					compressedHeaders = r.Header.Clone()
				} else if compressedHeaders != nil {
					for _, field := range []string{"request_id", "chat_record_id"} {
						if id := wire.Get(field).Str; id == "" || id == compressedBody.Get(field).Str {
							t.Errorf("plain replay did not generate a new COSY %s", field)
						}
					}
					for _, header := range []string{"Authorization", "X-Request-Id"} {
						if id := r.Header.Get(header); id == "" || id == compressedHeaders.Get(header) {
							t.Errorf("plain replay did not renew %s", header)
						}
					}
					for _, field := range []string{"session_id", "task_id", "request_set_id", "business", "model_config", "system", "messages", "parameters", "chat_context"} {
						if result := wire.Get(field); !result.Exists() || result.Raw != compressedBody.Get(field).Raw {
							t.Errorf("plain replay changed COSY %s", field)
						}
					}
					compressedHeaders = nil
				}
				if tc.admin && encoding == "zstd" {
					return qoderIntegrationResponse(r, http.StatusUnsupportedMediaType, `{"message":"Unsupported Content-Encoding"}`), nil
				}
				sseAnswer := fmt.Sprintf(`{"id":"chat-replayed","choices":[{"index":0,"delta":{"content":%q},"finish_reason":%q}]}`, tc.content, tc.finish)
				body := qoderIntegrationEvent(json.RawMessage(sseAnswer)) + "data: {\"statusCodeValue\":200,\"body\":\"[DONE]\"}\n\n"
				if encoding == "zstd" || tc.admin && len(encodings) == 2 {
					body = "data: {\"isQueued\":false}\n\n" + tc.rejection
				}
				response := qoderIntegrationResponse(r, http.StatusOK, body)
				response.Header.Set("Content-Type", "text/event-stream")
				return response, nil
			})
			if tc.admin {
				configs, err := env.store.ListConfigs(context.Background())
				if err != nil || len(configs) != 1 {
					t.Fatalf("configs=%d err=%v", len(configs), err)
				}
				id := configs[0].ID
				for _, wantSuccess := range []bool{false, true, true} {
					c, w := qoderIntegrationContext(t, http.MethodPost, fmt.Sprintf("/admin/channels/%d/test", id), &testutil.TestChannelRequest{Model: "QwenTest", ClientProtocol: "openai", Content: "test"}, id)
					env.server.HandleChannelTest(c)
					success := gjson.GetBytes(w.Body.Bytes(), "data.success")
					if w.Code != http.StatusOK || !success.Exists() || success.Bool() != wantSuccess {
						t.Fatalf("channel test status=%d want success=%t body=%s", w.Code, wantSuccess, w.Body.String())
					}
				}
			} else {
				for range 2 {
					w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "QwenTest", "stream": true, "messages": []any{map[string]string{"role": "user", "content": "test"}}}, nil)
					var content, finish string
					var done int
					for line := range strings.SplitSeq(w.Body.String(), "\n") {
						payload, ok := strings.CutPrefix(line, "data: ")
						if !ok {
							continue
						}
						if payload == "[DONE]" {
							done++
							continue
						}
						if !gjson.Valid(payload) || gjson.Get(payload, "error").Exists() {
							t.Fatalf("unexpected SSE payload: %s", payload)
						}
						content += gjson.Get(payload, "choices.0.delta.content").String()
						if reason := gjson.Get(payload, "choices.0.finish_reason").String(); reason != "" {
							finish = reason
						}
					}
					if w.Code != http.StatusOK || content != tc.content || finish != tc.finish || done != 1 {
						t.Fatalf("proxy status=%d content=%q finish=%q done=%d body=%s", w.Code, content, finish, done, w.Body.String())
					}
				}
			}
			if !reflect.DeepEqual(encodings, tc.wantEncodings) {
				t.Errorf("upstream Content-Encoding sequence = %q, want %q", encodings, tc.wantEncodings)
			}
		})
	}
}

/**
 * @brief 明文重发也被拒时暂停压缩回退，避免每个请求都翻倍发送；到期后恢复回退。
 * @param t 测试上下文。
 */
func TestQoderCompressionFallbackPausesWhenPlainAlsoRejected(t *testing.T) {
	credential := qoderIntegrationCredential(t, "cn", "dt-forbidden-secret")
	env := setupProxyTestEnv(t, []testChannel{{name: "qoder-forbidden", authType: model.AuthTypeQoderOAuth, oauthCredential: qoderIntegrationJSON(t, credential), models: "QwenTest", upstreamProtocol: "openai"}}, map[int]string{0: "https://gateway.qoder.com.cn/v1/chat/completions"})
	var mu sync.Mutex
	var encodings []string
	qoderIntegrationClient(env.server, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/algo/api/v2/model/list" {
			return qoderIntegrationResponse(r, 200, `{"chat":[{"key":"native-key","display_name":"Qwen Test","enable":true}]}`), nil
		}
		mu.Lock()
		encodings = append(encodings, r.Header.Get("Content-Encoding"))
		mu.Unlock()
		response := qoderIntegrationResponse(r, 200, "data: {\"statusCodeValue\":403,\"body\":{\"message\":\"Forbidden\"}}\n\n")
		response.Header.Set("Content-Type", "text/event-stream")
		return response, nil
	})
	send := func() {
		t.Helper()
		req := newJSONRequest(t, "POST", "/v1/chat/completions", map[string]any{"model": "QwenTest", "messages": []any{map[string]string{"role": "user", "content": "hello"}}, "stream": true})
		req.Header.Set("Authorization", "Bearer test-api-key")
		w := httptest.NewRecorder()
		env.engine.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("proxy status=%d body=%s", w.Code, w.Body.String())
		}
	}
	send()
	send()
	send()
	if got, want := strings.Join(encodings, ","), "zstd,,zstd,zstd"; got != want {
		t.Fatalf("upstream Content-Encoding sequence = %q, want %q", got, want)
	}
	env.server.compressionFallbackPaused.Range(func(key, _ any) bool {
		env.server.compressionFallbackPaused.Store(key, time.Now().Add(-time.Second))
		return true
	})
	send()
	if got, want := strings.Join(encodings, ","), "zstd,,zstd,zstd,zstd,"; got != want {
		t.Fatalf("after pause expiry Content-Encoding sequence = %q, want %q", got, want)
	}
}

func TestQoderProxyRefreshesNativeUnauthorizedOnce(t *testing.T) {
	credential := qoderIntegrationCredential(t, "cn", "dt-rejected-secret")
	env := setupProxyTestEnv(t, []testChannel{{name: "qoder-refresh", authType: model.AuthTypeQoderOAuth, oauthCredential: qoderIntegrationJSON(t, credential), models: "QwenTest", upstreamProtocol: "openai"}}, map[int]string{0: "https://gateway.qoder.com.cn/v1/chat/completions"})
	var chats, refreshes atomic.Int32
	qoderIntegrationClient(env.server, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/algo/api/v2/model/list":
			return qoderIntegrationResponse(r, 200, `{"chat":[{"key":"native-key","display_name":"Qwen Test","enable":true}]}`), nil
		case "/api/v1/deviceToken/refresh":
			refreshes.Add(1)
			if r.Header.Get("Authorization") != "" {
				t.Error("refresh leaked old bearer")
			}
			return qoderIntegrationResponse(r, 200, `{"device_token":"dt-recovered-secret","refresh_token":"refresh-recovered-secret","user_id":"same-user"}`), nil
		default:
			attempt := chats.Add(1)
			body := `data: {"statusCodeValue":403,"body":{"code":105,"message":"expired"}}` + "\n\n"
			if attempt > 1 {
				body = qoderIntegrationEvent(map[string]any{"id": "recovered", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "recovered"}, "finish_reason": "stop"}}}) + "data: {\"statusCodeValue\":200,\"body\":\"[DONE]\"}\n\n"
			}
			response := qoderIntegrationResponse(r, 200, body)
			response.Header.Set("Content-Type", "text/event-stream")
			return response, nil
		}
	})
	req := newJSONRequest(t, "POST", "/v1/chat/completions", map[string]any{"model": "QwenTest", "messages": []any{map[string]string{"role": "user", "content": "hello"}}, "stream": false})
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
	qoderIntegrationNoSecrets(t, w.Body.String(), credential.AccessToken, credential.RefreshToken, "dt-recovered-secret", "refresh-recovered-secret")
}

/**
 * @brief 已写入客户端的 Qoder 回复遇到后续上游错误时通过 SSE 报错。
 * @param t 测试上下文。
 */
func TestQoderProxyDoesNotReplayAfterCommittedOutput(t *testing.T) {
	for _, failure := range []string{"truncated", "unauthorized", "forbidden", "output after finish"} {
		t.Run(failure, func(t *testing.T) {
			primary := qoderIntegrationCredential(t, "cn", "dt-primary-secret")
			fallback := qoderIntegrationCredential(t, "cn", "dt-fallback-secret")
			fallback.UserID = "fallback-user"
			env := setupProxyTestEnv(t, []testChannel{
				{name: "qoder-primary", authType: model.AuthTypeQoderOAuth, oauthCredential: qoderIntegrationJSON(t, primary), models: "QwenTest", upstreamProtocol: "openai", sameChannelRetries: 2},
				{name: "qoder-fallback", authType: model.AuthTypeQoderOAuth, oauthCredential: qoderIntegrationJSON(t, fallback), models: "QwenTest", upstreamProtocol: "openai"},
			}, map[int]string{0: "https://gateway.qoder.com.cn/v1/chat/completions", 1: "https://gateway.qoder.com.cn/v1/chat/completions"})
			var primaryChats, fallbackChats, refreshes atomic.Int32
			qoderIntegrationClient(env.server, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/algo/api/v2/model/list" {
					return qoderIntegrationResponse(r, 200, `{"chat":[{"key":"native-key","display_name":"Qwen Test","enable":true}]}`), nil
				}
				if strings.Contains(r.URL.Path, "Token/refresh") {
					refreshes.Add(1)
					return qoderIntegrationResponse(r, 200, `{"device_token":"dt-unexpected-refresh"}`), nil
				}
				if r.Header.Get("Cosy-User") == fallback.UserID {
					fallbackChats.Add(1)
				} else {
					primaryChats.Add(1)
				}
				body := qoderIntegrationEvent(map[string]any{"id": "partial", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "visible partial"}, "finish_reason": nil}}})
				switch failure {
				case "unauthorized":
					body += `data: {"statusCodeValue":403,"body":{"code":105,"message":"expired"}}` + "\n\n"
				case "forbidden":
					if got := r.Header.Get("Content-Encoding"); got != "zstd" {
						t.Errorf("inference Content-Encoding = %q, want zstd", got)
					}
					body += `data: {"statusCodeValue":403,"body":{"message":"Forbidden"}}` + "\n\n"
				case "output after finish":
					body += qoderIntegrationEvent(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
					body += qoderIntegrationEvent(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "late text"}}}})
				}
				response := qoderIntegrationResponse(r, 200, body)
				response.Header.Set("Content-Type", "text/event-stream")
				return response, nil
			})
			req := newJSONRequest(t, "POST", "/v1/chat/completions", map[string]any{"model": "QwenTest", "messages": []any{map[string]string{"role": "user", "content": "hello"}}, "stream": true})
			req.Header.Set("Authorization", "Bearer test-api-key")
			w := httptest.NewRecorder()
			env.engine.ServeHTTP(w, req)
			if w.Code != 200 || primaryChats.Load() != 1 || fallbackChats.Load() != 0 || refreshes.Load() != 0 {
				t.Fatalf("committed output replayed: status=%d primary=%d fallback=%d refresh=%d body=%s", w.Code, primaryChats.Load(), fallbackChats.Load(), refreshes.Load(), w.Body.String())
			}
			var content string
			var sawError, sawDone bool
			scanner := bufio.NewScanner(bytes.NewReader(w.Body.Bytes()))
			for scanner.Scan() {
				line := scanner.Text()
				if line == "data: [DONE]" {
					sawDone = true
					continue
				}
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var event map[string]any
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
					t.Fatal(err)
				}
				if event["error"] != nil {
					sawError = true
				}
				choices, _ := event["choices"].([]any)
				for _, raw := range choices {
					delta, _ := raw.(map[string]any)["delta"].(map[string]any)
					text, _ := delta["content"].(string)
					content += text
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			if content != "visible partial" || !sawError || sawDone {
				t.Fatalf("partial stream content=%q error=%t done=%t body=%s", content, sawError, sawDone, w.Body.String())
			}
		})
	}
}

func TestQoderRefreshPreservesConcurrentMetadataAndRotatedTokens(t *testing.T) {
	s := newInMemoryServer(t)
	credential := qoderIntegrationCredential(t, "cn", "dt-metadata-old")
	cfg, err := s.store.CreateConfig(context.Background(), newQoderChannel("metadata-refresh", qoderIntegrationJSON(t, credential)))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var refreshes atomic.Int32
	qoderIntegrationClient(s, func(r *http.Request) (*http.Response, error) {
		refreshes.Add(1)
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		return qoderIntegrationResponse(r, 200, `{"device_token":"dt-metadata-new","refresh_token":"rotated-metadata-refresh","user_id":"same-user","expires_at":1900000000}`), nil
	})
	result := make(chan *qoderauth.Credential, 1)
	failure := make(chan error, 1)
	go func() {
		c, err := s.qoderCredentials.credential(context.Background(), cfg, true, credential.AccessToken)
		result <- c
		failure <- err
	}()
	<-started
	if err := s.updateQoderCredential(context.Background(), cfg.ID, credential, func(c *qoderauth.Credential) {
		c.OAuthUsage = `{"remaining":42}`
		c.LastCheckinAt = 1770000000
		c.LastCheckinCampaign = "metadata-campaign"
	}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-failure; err != nil {
		t.Fatal(err)
	}
	returned := <-result
	persisted, err := s.store.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := qoderauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []*qoderauth.Credential{returned, stored} {
		if value.AccessToken != "dt-metadata-new" || value.RefreshToken != "rotated-metadata-refresh" || value.ExpiresAt != 1900000000 || value.OAuthUsage != `{"remaining":42}` || value.LastCheckinAt != 1770000000 || value.LastCheckinCampaign != "metadata-campaign" {
			t.Fatalf("refresh lost rotated tokens or concurrent metadata: %+v", value)
		}
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refresh token reused %d times", refreshes.Load())
	}
}

func TestQoderTerminalRefreshDisablesOnlyRejectedCredential(t *testing.T) {
	for _, reauthorize := range []bool{false, true} {
		t.Run(fmt.Sprint(reauthorize), func(t *testing.T) {
			s := newInMemoryServer(t)
			credential := qoderIntegrationCredential(t, "cn", "dt-terminal-old")
			cfg, err := s.store.CreateConfig(context.Background(), newQoderChannel("terminal-refresh", qoderIntegrationJSON(t, credential)))
			if err != nil {
				t.Fatal(err)
			}
			qoderIntegrationClient(s, func(r *http.Request) (*http.Response, error) {
				return qoderIntegrationResponse(r, 401, `{"error":"expired private refresh token"}`), nil
			})
			_, refreshErr := s.qoderCredentials.credential(context.Background(), cfg, true, credential.AccessToken)
			if refreshErr == nil {
				t.Fatal("terminal refresh did not fail")
			}
			// A metadata write after the failure must still allow disabling this token.
			if err = s.updateQoderCredential(context.Background(), cfg.ID, credential, func(c *qoderauth.Credential) { c.OAuthUsage = `{"remaining":19}` }); err != nil {
				t.Fatal(err)
			}
			if reauthorize {
				latest, err := s.store.GetConfig(context.Background(), cfg.ID)
				if err != nil {
					t.Fatal(err)
				}
				winner := qoderIntegrationCredential(t, "cn", "dt-terminal-reauthorized")
				winner.RefreshToken = "terminal-reauthorized-refresh"
				changed, err := s.store.CompareAndSwapOAuthCredential(context.Background(), cfg.ID, model.AuthTypeQoderOAuth, latest.OAuthCredential, qoderIntegrationJSON(t, winner))
				if err != nil || !changed {
					t.Fatalf("reauthorization=%t err=%v", changed, err)
				}
			}
			if !s.disableTerminalOAuthCredential(context.Background(), cfg, "Qoder", refreshErr) {
				t.Fatal("terminal refresh was not handled with a credential snapshot")
			}
			latest, err := s.store.GetConfig(context.Background(), cfg.ID)
			if err != nil {
				t.Fatal(err)
			}
			if latest.Enabled != reauthorize {
				t.Fatalf("enabled=%t, reauthorized=%t", latest.Enabled, reauthorize)
			}
			if reauthorize {
				stored, err := qoderauth.ParseCredential([]byte(latest.OAuthCredential))
				if err != nil || stored.AccessToken != "dt-terminal-reauthorized" {
					t.Fatalf("reauthorized credential changed: %+v %v", stored, err)
				}
			}
		})
	}
}

func TestQoderRefreshRetriesCASAfterMetadataWrite(t *testing.T) {
	credential := qoderIntegrationCredential(t, "cn", "dt-cas-original")
	metadata := *credential
	metadata.OAuthUsage = `{"remaining":73}`
	metadata.LastCheckinCampaign = "cas-campaign"
	winner := &concurrentOAuthWinnerStore{authType: model.AuthTypeQoderOAuth, winnerJSON: qoderIntegrationJSON(t, &metadata)}
	s := newInMemoryServerWithCustomStore(t, func(store storage.Store) storage.Store { winner.Store = store; return winner })
	cfg, err := s.store.CreateConfig(context.Background(), newQoderChannel("cas-refresh", qoderIntegrationJSON(t, credential)))
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	qoderIntegrationClient(s, func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return qoderIntegrationResponse(r, 200, `{"device_token":"dt-cas-rotated","refresh_token":"cas-rotated-refresh","user_id":"same-user"}`), nil
	})
	fresh, err := s.qoderCredentials.credential(context.Background(), cfg, true, credential.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := s.store.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := qoderauth.ParseCredential([]byte(latest.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []*qoderauth.Credential{fresh, stored} {
		if value.AccessToken != "dt-cas-rotated" || value.RefreshToken != "cas-rotated-refresh" || value.OAuthUsage != metadata.OAuthUsage || value.LastCheckinCampaign != metadata.LastCheckinCampaign {
			t.Fatalf("CAS conflict lost tokens or latest metadata: %+v", value)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("CAS conflict repeated one-use refresh: %d", requests.Load())
	}
}

/**
 * @brief 真实代理链路解析文本工具调用，并在工具续轮保留会话和任务身份
 * @param t 测试上下文
 */
func TestQoderProxyTextToolsAndTaskIdentity(t *testing.T) {
	credential := qoderIntegrationCredential(t, "cn", "dt-text-tools")
	env := setupProxyTestEnv(t, []testChannel{{name: "qoder", authType: model.AuthTypeQoderOAuth, oauthCredential: qoderIntegrationJSON(t, credential), models: "QwenTest", upstreamProtocol: "openai"}}, map[int]string{0: "https://gateway.qoder.com.cn/v1/chat/completions"})
	var session, task string
	var chats int
	qoderIntegrationClient(env.server, func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/algo/api/v2/model/list" {
			return qoderIntegrationResponse(r, 200, `{"app":[{"key":"native-key","display_name":"Qwen Test","enable":true}]}`), nil
		}
		chats++
		var wire struct {
			Session  string `json:"session_id"`
			Task     string `json:"request_set_id"`
			Business struct {
				ID, Stage string
				Model     string `json:"model_key"`
			} `json:"business"`
		}
		mustUnmarshalJSON(t, qoderIntegrationDecodeSignedRequest(t, r), &wire)
		if wire.Business.ID != wire.Task || wire.Business.Model != "native-key" {
			t.Errorf("business=%+v", wire.Business)
		}
		if chats == 1 {
			session, task = wire.Session, wire.Task
		} else if session != wire.Session || task != wire.Task || wire.Business.Stage != "processing" {
			t.Errorf("tool continuation changed identity: %+v", wire)
		}
		answer := "<tool_call>{\"name\":\"lookup\",\"arguments\":{\"q\":1}}</tool_call>"
		if chats > 1 {
			answer = "done"
		}
		body := qoderIntegrationEvent(map[string]any{"id": "chat-tools", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": answer}, "finish_reason": "stop"}}})
		return qoderIntegrationResponse(r, 200, body), nil
	})
	messages := []any{map[string]any{"role": "user", "content": "lookup"}}
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "integer"}}}}}}
	for turn := range 2 {
		w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "QwenTest", "messages": messages, "tools": tools, "stream": false}, map[string]string{"Session-Id": "stable-tools"})
		if w.Code != 200 {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		var result struct {
			Choices []struct {
				Message map[string]any `json:"message"`
				Finish  string         `json:"finish_reason"`
			} `json:"choices"`
		}
		mustUnmarshalJSON(t, w.Body.Bytes(), &result)
		if turn == 0 {
			calls, ok := result.Choices[0].Message["tool_calls"].([]any)
			if !ok || len(calls) != 1 || result.Choices[0].Finish != "tool_calls" {
				t.Fatalf("text call not parsed: %s", w.Body.String())
			}
			call := calls[0].(map[string]any)
			var arguments map[string]int
			mustUnmarshalJSON(t, []byte(call["function"].(map[string]any)["arguments"].(string)), &arguments)
			if arguments["q"] != 1 {
				t.Fatalf("arguments=%v", arguments)
			}
			messages = append(messages, result.Choices[0].Message, map[string]any{"role": "tool", "tool_call_id": call["id"], "content": "found"})
		}
	}
	if chats != 2 {
		t.Fatalf("chats=%d", chats)
	}
}

/**
 * @brief 输出上限拒绝只修复一次，HTTP 与流内错误共用修复路径
 * @param t 测试上下文
 */
func TestQoderProxyRepairsOutputLimit(t *testing.T) {
	for _, tc := range []struct {
		name               string
		streamed, nullable bool
		cap                int
	}{
		{"http", false, false, 8192}, {"sse", true, false, 8192}, {"nullable-budget", false, true, 40000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credential := qoderIntegrationCredential(t, "cn", "dt-output-cap")
			env := setupProxyTestEnv(t, []testChannel{{name: "qoder", authType: model.AuthTypeQoderOAuth, oauthCredential: qoderIntegrationJSON(t, credential), models: "QwenTest", upstreamProtocol: "openai"}}, map[int]string{0: "https://gateway.qoder.com.cn/v1/chat/completions"})
			chats := 0
			qoderIntegrationClient(env.server, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/algo/api/v2/model/list" {
					return qoderIntegrationResponse(r, 200, `{"app":[{"key":"native-key","display_name":"Qwen Test","enable":true}]}`), nil
				}
				chats++
				var wire struct {
					Parameters struct {
						Max int `json:"max_tokens"`
					} `json:"parameters"`
				}
				mustUnmarshalJSON(t, qoderIntegrationDecodeSignedRequest(t, r), &wire)
				want := 50000
				if chats > 1 {
					want = tc.cap
				}
				if wire.Parameters.Max != want {
					t.Errorf("attempt %d budget=%d want=%d", chats, wire.Parameters.Max, want)
				}
				if chats == 1 {
					failure := fmt.Sprintf(`{"message":"Error in upstream response: Range of max_tokens should be [1, %d]"}`, tc.cap)
					if tc.streamed {
						return qoderIntegrationResponse(r, 200, "data: {\"statusCodeValue\":400,\"body\":"+failure+"}\n\n"), nil
					}
					return qoderIntegrationResponse(r, 400, failure), nil
				}
				return qoderIntegrationResponse(r, 200, qoderIntegrationEvent(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "ok"}, "finish_reason": "stop"}}})), nil
			})
			request := map[string]any{"model": "QwenTest", "max_tokens": 50000, "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "stream": false}
			if tc.nullable {
				request["max_completion_tokens"] = nil
			}
			w := doProxyRequest(t, env.engine, "/v1/chat/completions", request, nil)
			if w.Code != 200 || chats != 2 {
				t.Fatalf("status=%d chats=%d body=%s", w.Code, chats, w.Body.String())
			}
		})
	}
}

/**
 * @brief 免费视图只在额度未耗尽时刷新，确认耗尽的禁用模型返回 429
 * @param t 测试上下文
 */
func TestQoderProxyRecoversFreeViewAndClassifiesDisabledQuota(t *testing.T) {
	for _, exhausted := range []bool{false, true} {
		t.Run(fmt.Sprint(exhausted), func(t *testing.T) {
			credential := qoderIntegrationCredential(t, "cn", "dt-free-view")
			env := setupProxyTestEnv(t, []testChannel{{name: "qoder", authType: model.AuthTypeQoderOAuth, oauthCredential: qoderIntegrationJSON(t, credential), models: "QwenTest", upstreamProtocol: "openai"}}, map[int]string{0: "https://gateway.qoder.com.cn/v1/chat/completions"})
			catalogs, refreshes, chats := 0, 0, 0
			qoderIntegrationClient(env.server, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/algo/api/v2/model/list":
					catalogs++
					enabled := "false"
					if refreshes > 0 {
						enabled = "true"
					}
					return qoderIntegrationResponse(r, 200, `{"app":[{"key":"free","display_name":"Free","enable":true,"price_factor":0},{"key":"native-key","display_name":"Qwen Test","enable":`+enabled+`,"price_factor":1}]}`), nil
				case "/api/v2/quota/usage":
					remaining := "1"
					if exhausted {
						remaining = "0"
					}
					return qoderIntegrationResponse(r, 200, `{"userQuota":{"total":1,"remaining":`+remaining+`,"unit":"credits"}}`), nil
				case "/api/v2/user/plan":
					return qoderIntegrationResponse(r, 200, `{}`), nil
				case "/api/v1/deviceToken/refresh":
					refreshes++
					return qoderIntegrationResponse(r, 200, `{"token":"dt-free-renewed","refresh_token":"refresh-new","expires_in":3600}`), nil
				default:
					chats++
					return qoderIntegrationResponse(r, 200, qoderIntegrationEvent(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "ok"}, "finish_reason": "stop"}}})), nil
				}
			})
			w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "QwenTest", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}, nil)
			if exhausted {
				if w.Code != 429 || refreshes != 0 || chats != 0 {
					t.Fatalf("status=%d refreshes=%d chats=%d body=%s", w.Code, refreshes, chats, w.Body.String())
				}
			} else if w.Code != 200 || refreshes != 1 || catalogs != 2 || chats != 1 {
				t.Fatalf("status=%d catalogs=%d refreshes=%d chats=%d body=%s", w.Code, catalogs, refreshes, chats, w.Body.String())
			}
		})
	}
}

/**
 * @brief 模型缓存缺项时重取目录，不将新模型误判为请求错误
 * @param t 测试上下文
 */
func TestQoderProxyRefreshesCatalogForNewModel(t *testing.T) {
	for _, initiallyDisabled := range []bool{false, true} {
		t.Run(fmt.Sprint(initiallyDisabled), func(t *testing.T) {
			credential := qoderIntegrationCredential(t, "cn", "dt-new-model")
			env := setupProxyTestEnv(t, []testChannel{{name: "qoder", authType: model.AuthTypeQoderOAuth, oauthCredential: qoderIntegrationJSON(t, credential), models: "First,Second", upstreamProtocol: "openai"}}, map[int]string{0: "https://gateway.qoder.com.cn/v1/chat/completions"})
			catalogs, chats := 0, 0
			qoderIntegrationClient(env.server, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/algo/api/v2/model/list" {
					catalogs++
					entries := `{"key":"one","display_name":"First","enable":true}`
					if catalogs > 1 || initiallyDisabled {
						entries += `,{"key":"two","display_name":"Second","enable":` + strconv.FormatBool(catalogs > 1) + `}`
					}
					return qoderIntegrationResponse(r, 200, `{"app":[`+entries+`]}`), nil
				}
				chats++
				want := "one"
				if chats > 1 {
					want = "two"
				}
				if r.Header.Get("X-Model-Key") != want {
					t.Errorf("model key=%q want=%q", r.Header.Get("X-Model-Key"), want)
				}
				return qoderIntegrationResponse(r, 200, qoderIntegrationEvent(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "ok"}, "finish_reason": "stop"}}})), nil
			})
			for _, name := range []string{"First", "Second"} {
				w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": name, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}, nil)
				if w.Code != 200 {
					t.Fatalf("%s status=%d body=%s", name, w.Code, w.Body.String())
				}
			}
			if catalogs != 2 || chats != 2 {
				t.Fatalf("catalogs=%d chats=%d", catalogs, chats)
			}
		})
	}
}

/**
 * @brief 成功目录中的免费模型不因缺失刷新材料或可选恢复暂时失败而不可用
 * @param t 测试上下文
 */
func TestQoderProxyKeepsEnabledFreeModelsWhenRecoveryUnavailable(t *testing.T) {
	for _, canRefresh := range []bool{false, true} {
		t.Run(fmt.Sprint(canRefresh), func(t *testing.T) {
			credential := qoderIntegrationCredential(t, "cn", "dt-enabled-free")
			if !canRefresh {
				credential.RefreshToken = ""
			}
			env := setupProxyTestEnv(t, []testChannel{{name: "qoder", authType: model.AuthTypeQoderOAuth, oauthCredential: qoderIntegrationJSON(t, credential), models: "Free", upstreamProtocol: "openai"}}, map[int]string{0: "https://gateway.qoder.com.cn/v1/chat/completions"})
			refreshes, chats := 0, 0
			qoderIntegrationClient(env.server, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/algo/api/v2/model/list":
					return qoderIntegrationResponse(r, 200, `{"app":[{"key":"free","display_name":"Free","enable":true,"price_factor":0},{"key":"paid","display_name":"Paid","enable":false,"price_factor":1}]}`), nil
				case "/api/v2/quota/usage":
					return qoderIntegrationResponse(r, 503, `{}`), nil
				case "/api/v1/deviceToken/refresh":
					refreshes++
					return qoderIntegrationResponse(r, 503, `{}`), nil
				default:
					chats++
					return qoderIntegrationResponse(r, 200, qoderIntegrationEvent(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "free answer"}, "finish_reason": "stop"}}})), nil
				}
			})
			for range 2 {
				w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "Free", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}, nil)
				if w.Code != 200 {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
			}
			want := 0
			if canRefresh {
				want = 1
			}
			if refreshes != want || chats != 2 {
				t.Fatalf("refreshes=%d want=%d chats=%d", refreshes, want, chats)
			}
		})
	}
}

/**
 * @brief HTTP 错误、成功状态的 JSON 与首帧 SSE 排队失败均保留上游 Retry-After
 * @param t 测试上下文
 */
func TestQoderProxyQueueKeepsRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name, body, wait string
		transport        int
		stream           bool
	}{
		{"HTTP", `{"code":10605,"queue":{"isQueued":true,"retryAfterSeconds":12}}`, "12", 403, false},
		{"JSON", `{"statusCodeValue":403,"body":{"queue":{"serviceAvailable":false,"queueType":"p3","retryAfterSeconds":"12"}}}`, "12", 200, false},
		{"SSE", `data: {"statusCodeValue":403,"body":{"code":10605,"waitTime":"9"}}` + "\n\n", "9", 200, false},
		{"SSE outer wait", `data: {"statusCodeValue":200,"waitTime":"9","body":{"error":{"code":10605}}}` + "\n\n", "9", 200, false},
		{"bare queue streaming", `data: {"isQueued":true,"waitTime":"9"}` + "\n\n", "9", 200, true},
		{"bare queue aggregated", `data: {"isQueued":true,"waitTime":"9"}` + "\n\n", "9", 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credential := qoderIntegrationCredential(t, "cn", "dt-queue")
			env := setupProxyTestEnv(t, []testChannel{{name: "qoder", authType: model.AuthTypeQoderOAuth, oauthCredential: qoderIntegrationJSON(t, credential), models: "QwenTest", upstreamProtocol: "openai"}}, map[int]string{0: "https://gateway.qoder.com.cn/v1/chat/completions"})
			qoderIntegrationClient(env.server, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/algo/api/v2/model/list" {
					return qoderIntegrationResponse(r, 200, `{"app":[{"key":"native-key","display_name":"Qwen Test","enable":true}]}`), nil
				}
				return qoderIntegrationResponse(r, tc.transport, tc.body), nil
			})
			w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "QwenTest", "stream": tc.stream, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}, nil)
			if w.Code != 503 || w.Header().Get("Retry-After") != tc.wait {
				t.Fatalf("status=%d retry-after=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
			}
		})
	}
}

/**
 * @brief 仅在 JSON 或首帧 SSE 业务错误明确拒绝凭证时触发代理刷新
 * @param t 测试上下文
 */
func TestQoderProxyClassifiesHTTPBusinessErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		transport, want  int
	}{
		{"bare forbidden", `{"message":"Forbidden"}`, "permission_denied", 403, 403},
		{"embedded forbidden", `{"statusCodeValue":403,"body":{"message":"Forbidden"}}`, "permission_denied", 200, 403},
		{"entitlement", `{"message":"Upgrade required","pricingUrl":"https://example.com/pricing"}`, "insufficient_quota", 403, 429},
		{"embedded quota", `{"statusCodeValue":200,"body":"{\"code\":112}"}`, "insufficient_quota", 200, 429},
		{"embedded auth", `{"statusCodeValue":403,"body":{"code":105,"message":"Login expired"}}`, "", 200, 200},
		{"expired token while service unavailable", `{"code":105,"serviceAvailable":false}`, "", 403, 200},
		{"SSE nested auth", `data: {"statusCodeValue":200,"body":{"error":{"status":401,"message":"Unauthorized"}}}` + "\n\n", "", 200, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credential := qoderIntegrationCredential(t, "cn", "dt-http-business-errors")
			env := setupProxyTestEnv(t, []testChannel{{name: "qoder", authType: model.AuthTypeQoderOAuth, oauthCredential: qoderIntegrationJSON(t, credential), models: "QwenTest", upstreamProtocol: "openai"}}, map[int]string{0: "https://gateway.qoder.com.cn/v1/chat/completions"})
			var chats, refreshes int
			qoderIntegrationClient(env.server, func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/algo/api/v2/model/list":
					return qoderIntegrationResponse(r, 200, `{"chat":[{"key":"native-key","display_name":"Qwen Test","enable":true}]}`), nil
				case "/api/v1/deviceToken/refresh":
					refreshes++
					return qoderIntegrationResponse(r, 200, `{"device_token":"dt-rotated","refresh_token":"rotated-refresh","user_id":"same-user"}`), nil
				default:
					chats++
					if chats == 1 {
						return qoderIntegrationResponse(r, tc.transport, tc.body), nil
					}
					return qoderIntegrationResponse(r, 200, qoderIntegrationEvent(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "recovered"}, "finish_reason": "stop"}}})), nil
				}
			})
			w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "QwenTest", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}, nil)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.want, w.Body.String())
			}
			if tc.want == http.StatusOK {
				if chats != 2 || refreshes != 1 || gjson.GetBytes(w.Body.Bytes(), "choices.0.message.content").String() != "recovered" {
					t.Fatalf("chats=%d refreshes=%d body=%s", chats, refreshes, w.Body.String())
				}
			} else if chats != 1 || refreshes != 0 || gjson.GetBytes(w.Body.Bytes(), "error.code").String() != tc.code {
				t.Fatalf("chats=%d refreshes=%d body=%s", chats, refreshes, w.Body.String())
			}
		})
	}
}

/**
 * @brief 纯无名工具回复不会先提交成功终态，流式与非流式均可在输出前报错
 * @param t 测试上下文
 */
func TestQoderProxyRejectsNamelessOnlyToolReply(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			credential := qoderIntegrationCredential(t, "cn", "dt-nameless")
			env := setupProxyTestEnv(t, []testChannel{{name: "qoder", authType: model.AuthTypeQoderOAuth, oauthCredential: qoderIntegrationJSON(t, credential), models: "QwenTest", upstreamProtocol: "openai"}}, map[int]string{0: "https://gateway.qoder.com.cn/v1/chat/completions"})
			qoderIntegrationClient(env.server, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/algo/api/v2/model/list" {
					return qoderIntegrationResponse(r, 200, `{"app":[{"key":"native-key","display_name":"Qwen Test","enable":true}]}`), nil
				}
				body := qoderIntegrationEvent(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}})
				body += qoderIntegrationEvent(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "unnamed", "type": "function", "function": map[string]any{"arguments": "{}"}}}}, "finish_reason": "tool_calls"}}})
				return qoderIntegrationResponse(r, 200, body), nil
			})
			w := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{"model": "QwenTest", "messages": []any{map[string]any{"role": "user", "content": "hello"}}, "stream": stream}, nil)
			if w.Code != 502 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			var result map[string]any
			mustUnmarshalJSON(t, w.Body.Bytes(), &result)
			if result["error"] == nil {
				t.Fatalf("missing error: %s", w.Body.String())
			}
		})
	}
}

/**
 * @brief 验证 Qoder 非流式聚合与 Responses 压缩共存，截断摘要不能生成可回放胶囊。
 * @param t 当前测试上下文。
 */
func TestQoderResponsesCompactionRequiresSuccessfulStop(t *testing.T) {
	for _, ending := range []string{"stop", "length"} {
		t.Run(ending, func(t *testing.T) {
			credential := qoderIntegrationCredential(t, "cn", "dt-compaction")
			env := setupProxyTestEnv(t, []testChannel{{name: "qoder-compaction", authType: model.AuthTypeQoderOAuth, oauthCredential: qoderIntegrationJSON(t, credential), models: "qwentest", upstreamProtocol: "openai"}}, map[int]string{0: "https://gateway.qoder.com.cn/v1/chat/completions"})
			qoderIntegrationClient(env.server, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/algo/api/v2/model/list" {
					return qoderIntegrationResponse(r, 200, `{"chat":[{"key":"native-key","display_name":"Qwen Test","enable":true}]}`), nil
				}
				body := qoderIntegrationEvent(map[string]any{"id": "summary", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "Keep the parser changes."}, "finish_reason": ending}}, "usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 7, "total_tokens": 18}})
				response := qoderIntegrationResponse(r, 200, body)
				response.Header.Set("Content-Type", "text/event-stream")
				return response, nil
			})
			response := doProxyRequest(t, env.engine, "/v1/responses", map[string]any{
				"model": "qwentest", "stream": false,
				"input": []any{map[string]any{"role": "user", "content": "Fix parser"}, map[string]any{"type": "compaction_trigger"}},
			}, nil)
			result := gjson.ParseBytes(response.Body.Bytes())
			if ending == "stop" {
				if response.Code != http.StatusOK || result.Get("status").String() != "completed" || result.Get("output.0.type").String() != "compaction" || result.Get("usage.input_tokens").Int() != 11 || result.Get("usage.output_tokens").Int() != 7 {
					t.Fatalf("lost compaction or usage: status=%d body=%s", response.Code, response.Body.String())
				}
			} else if response.Code == http.StatusOK || result.Get("output.0.type").String() == "compaction" {
				t.Fatalf("truncated summary reported success: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}
