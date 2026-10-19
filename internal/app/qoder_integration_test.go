package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/qoderauth"
	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
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
			return qoderIntegrationResponse(r, 200, `{"chat":[{"key":"native-key","display_name":"Qwen Test"}]}`), nil
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
	qoderIntegrationClient(s, func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/me/campaigns"):
			return qoderIntegrationResponse(r, 200, `{"campaigns":[{"campaignId":"daily","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE"}]}`), nil
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/me/campaigns/daily/claim"):
			claims.Add(1)
			return qoderIntegrationResponse(r, 200, `{"status":"CLAIMED"}`), nil
		}
		return qoderIntegrationResponse(r, 404, `{}`), nil
	})
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
	if err != nil || persisted.LastCheckinCampaign != "daily" || persisted.LastCheckinAt < qoderCheckinWindow(time.Now()).Unix() {
		t.Fatalf("scheduled check-in not persisted: %+v %v", persisted, err)
	}
}

func TestQoderCheckinWindowAtTenInUTCPlusEight(t *testing.T) {
	for _, tt := range []struct{ now, want string }{
		{"2026-10-19T01:59:59Z", "2026-10-18T02:00:00Z"},
		{"2026-10-19T02:00:00Z", "2026-10-19T02:00:00Z"},
		{"2026-10-19T02:00:01Z", "2026-10-19T02:00:00Z"},
		{"2026-11-01T01:59:59Z", "2026-10-31T02:00:00Z"},
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
		if got := qoderCheckinWindow(now); !got.Equal(want) {
			t.Errorf("window(%s)=%s want %s", now, got, want)
		}
	}
}

func qoderIntegrationEvent(value any) string {
	body, _ := json.Marshal(value)
	envelope, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": string(body)})
	return "data: " + string(envelope) + "\n\n"
}

func TestQoderProxyStreamAndNonStreamKeepToolsAndUsage(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			credential := qoderIntegrationCredential(t, "cn", "dt-proxy-secret")
			// The stored mixed-case name must still resolve against the lowercase catalog ID "qwentest".
			env := setupProxyTestEnv(t, []testChannel{{name: "qoder", authType: model.AuthTypeQoderOAuth, oauthCredential: qoderIntegrationJSON(t, credential), models: "QwenTest", upstreamProtocol: "openai"}}, map[int]string{0: "https://gateway.qoder.com.cn/v1/chat/completions"})
			var chatCalls atomic.Int32
			qoderIntegrationClient(env.server, func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/algo/api/v2/model/list" {
					return qoderIntegrationResponse(r, 200, `{"chat":[{"key":"native-key","display_name":"Qwen Test"}]}`), nil
				}
				chatCalls.Add(1)
				if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer COSY.") {
					t.Error("chat was not signed")
				}
				body := qoderIntegrationEvent(map[string]any{"id": "chat-qoder", "model": "native-key", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hello", "tool_calls": []any{map[string]any{"index": 0, "id": "call-1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{\"q\":"}}}}, "finish_reason": nil}}})
				body += qoderIntegrationEvent(map[string]any{"id": "chat-qoder", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"arguments": "1}"}}}}, "finish_reason": "tool_calls"}}})
				body += qoderIntegrationEvent(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 7, "total_tokens": 17, "prompt_tokens_details": map[string]any{"cached_tokens": 3}}})
				body += "data: {\"statusCodeValue\":200,\"body\":\"[DONE]\"}\n\n"
				response := qoderIntegrationResponse(r, 200, body)
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

func TestQoderProxyRefreshesNativeUnauthorizedOnce(t *testing.T) {
	credential := qoderIntegrationCredential(t, "cn", "dt-rejected-secret")
	env := setupProxyTestEnv(t, []testChannel{{name: "qoder-refresh", authType: model.AuthTypeQoderOAuth, oauthCredential: qoderIntegrationJSON(t, credential), models: "QwenTest", upstreamProtocol: "openai"}}, map[int]string{0: "https://gateway.qoder.com.cn/v1/chat/completions"})
	var chats, refreshes atomic.Int32
	qoderIntegrationClient(env.server, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/algo/api/v2/model/list":
			return qoderIntegrationResponse(r, 200, `{"chat":[{"key":"native-key","display_name":"Qwen Test"}]}`), nil
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

func TestQoderProxyDoesNotReplayAfterCommittedOutput(t *testing.T) {
	for _, failure := range []string{"truncated", "unauthorized"} {
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
					return qoderIntegrationResponse(r, 200, `{"chat":[{"key":"native-key","display_name":"Qwen Test"}]}`), nil
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
				if failure == "unauthorized" {
					body += `data: {"statusCodeValue":403,"body":{"code":105,"message":"expired"}}` + "\n\n"
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
