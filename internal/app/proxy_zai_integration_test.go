package app

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/testutil"
	"ccLoad/internal/util"
	"ccLoad/internal/zaiauth"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

/**
 * @brief 重复渠道共享最长退避历史，渠道排序和新渠道均不能缩短账号窗口。
 * @param t 测试上下文。
 */
func TestProxy_ZAITrialDuplicateBackoffHistory(t *testing.T) {
	for _, history := range [][2]int{{0, 1}, {1, 0}, {1, 2}, {2, 1}, {1, 1}} {
		t.Run(fmt.Sprintf("history=%v", history), func(t *testing.T) {
			env, configs := newZAIStartHarness(t)
			ctx := context.Background()
			const name = "GLM-5.3-Flash-Trial"
			for i, index := range []int{0, 3} {
				for range history[i] {
					if _, err := env.store.BumpModelCooldown(ctx, configs[index].ID, name, time.Now().Add(-time.Hour), 429); err != nil {
						t.Fatal(err)
					}
				}
			}
			control := env.server.client
			var calls atomic.Int32
			env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(req.URL.Path, "/messages") {
					return control.Do(req)
				}
				calls.Add(1)
				return zaiStartTestResponse(req, 429, `{"error":{"code":"1113","message":"quota exhausted"}}`), nil
			})}
			request := func(index int) *httptest.ResponseRecorder {
				return doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": name, "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "hello"}}}, map[string]string{"X-CCLoad-Channel": configs[index].Name})
			}
			if response := request(3); response.Code == 200 || calls.Load() != 1 {
				t.Fatalf("expected one failed attempt: status=%d calls=%d", response.Code, calls.Load())
			}
			windows, err := env.store.GetAllModelCooldowns(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := time.Minute * time.Duration(1<<max(history[0], history[1]))
			for _, index := range []int{0, 3} {
				remaining := time.Until(windows[configs[index].ID][name])
				if remaining < want-5*time.Second || remaining > want {
					t.Errorf("channel %s: remaining=%v want=%v", configs[index].Name, remaining, want)
				}
				// 持久化窗口也必须实际阻止重复渠道再次发送。
				if response := request(index); response.Code < 400 || calls.Load() != 1 {
					t.Errorf("duplicate bypassed cooldown: status=%d calls=%d", response.Code, calls.Load())
				}
				if history[0] == history[1] {
					// 相同历史无需传播，完整时长不能被剩余时长覆盖。
					duration, err := env.store.BumpModelCooldown(ctx, configs[index].ID, name, time.Now(), 429)
					if err != nil || duration != 2*want {
						t.Errorf("preserved history: duration=%v want=%v err=%v", duration, 2*want, err)
					}
				}
			}
		})
	}
}

/**
 * @brief 旧认证失败与新凭证采样并发时，新采样仍可在控制面故障后复用。
 * @param t 测试上下文。
 */
func TestZAITrialAuthFailurePreservesNewPlanSample(t *testing.T) {
	env := setupProxyTestEnv(t, []testChannel{{name: "auth", upstreamProtocol: "anthropic", models: "GLM-5.3-Flash-Trial", authType: model.AuthTypeZAIOAuth, oauthCredential: `{"jwt_token":"old-jwt","user_id":"account"}`}}, map[int]string{0: "https://zai.test"})
	ctx := context.Background()
	configs, err := env.store.ListConfigs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var queries atomic.Int32
	var unavailable atomic.Bool
	env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		queries.Add(1)
		if unavailable.Load() {
			return zaiStartTestResponse(req, 503, `{"error":{"message":"unavailable"}}`), nil
		}
		return zaiStartTestResponse(req, 200, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":"active"}],"balances":[{"plan_id":"gift","capabilities":["model:glm-5.3-flash"],"remaining_units":10}]}}`), nil
	})}
	accepted, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	// 暂停已接受的旧认证失败，让新凭证的套餐查询与其收尾重叠；只断言查询结果和上游次数。
	env.server.zaiStart.now = func() time.Time {
		close(accepted)
		<-release
		return time.Now()
	}
	failureDone := make(chan struct{})
	go func() {
		env.server.applyZAITrialCooldown(ctx, configs[0], cooldown.ErrorInput{StatusCode: 401, Model: "GLM-5.3-Flash", ErrorBody: []byte(`{"error":{"message":"invalid JWT"}}`)})
		close(failureDone)
	}()
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("old authentication failure did not reach cooldown")
	}
	next := configs[0].Clone()
	next.OAuthCredential = `{"jwt_token":"new-jwt","user_id":"account"}`
	credential, err := zaiauth.ParseCredential([]byte(next.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	queryDone := make(chan error, 1)
	go func() {
		_, err := env.server.zaiAccountPlans(ctx, next, credential, false)
		queryDone <- err
	}()
	// 允许旧实现完成并发采样；正确实现会等待失败收尾。超时只释放同步点，不作为断言。
	queryFinished := false
	select {
	case err := <-queryDone:
		queryFinished = true
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	select {
	case <-failureDone:
	case <-time.After(5 * time.Second):
		t.Fatal("old authentication failure did not finish")
	}
	if !queryFinished {
		select {
		case err := <-queryDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("new credential query did not finish")
		}
	}
	unavailable.Store(true)
	plans, err := env.server.zaiAccountPlans(ctx, next, credential, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, available := plans.GiftModel("GLM-5.3-Flash"); !available || queries.Load() != 1 {
		t.Fatalf("new credential sample invalidated: available=%t queries=%d", available, queries.Load())
	}
}

/**
 * @brief busy 按上游恢复时间发送，超出预算时保留真实错误及明确冷却期限。
 * @param t 测试上下文。
 */
func TestProxy_ZAITrialRetryAfter(t *testing.T) {
	for _, scenario := range []string{"seconds", "date", "final", "budget", "stream_budget", "invalid", "overflow", "past", "capacity_after_hint", "cancel_wait"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// 非整秒开始，覆盖秒数提示转 HTTP 日期时的取整边界。
				time.Sleep(200 * time.Millisecond)
				env := setupProxyTestEnvWithSettings(t, []testChannel{{name: "retry", upstreamProtocol: "anthropic", models: "GLM-5.3-Flash-Trial", authType: model.AuthTypeZAIOAuth, oauthCredential: `{"jwt_token":"jwt","user_id":"account"}`}}, map[int]string{0: "https://zai.test"}, map[string]string{"upstream_first_byte_timeout": "2"})
				configs, err := env.store.ListConfigs(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				configs[0].MaxConcurrency = 1
				if scenario == "capacity_after_hint" {
					configs[0].RPMLimit = 1
				}
				if _, err := env.store.UpdateConfig(context.Background(), configs[0].ID, configs[0]); err != nil {
					t.Fatal(err)
				}
				env.server.InvalidateChannelListCache()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				var sent []time.Time
				env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					if !strings.HasSuffix(req.URL.Path, "/messages") {
						return zaiStartTestResponse(req, 200, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":"active"}],"balances":[{"plan_id":"gift","capabilities":["model:glm-5.3-flash"],"remaining_units":10}]}}`), nil
					}
					sent = append(sent, time.Now())
					if scenario == "cancel_wait" {
						time.AfterFunc(time.Second, cancel)
					}
					if len(sent) > 1 && scenario != "final" && scenario != "budget" {
						return zaiStartTestResponse(req, 200, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), nil
					}
					resp := zaiStartTestResponse(req, 429, `{"code":3010,"msg":"busy"}`)
					hint := "4"
					switch scenario {
					case "date":
						hint = time.Now().Add(4 * time.Second).UTC().Format(http.TimeFormat)
					case "budget":
						hint = "120"
					case "invalid":
						hint = "invalid"
					case "overflow":
						hint = "9223372036854775807"
					case "past":
						hint = time.Now().Add(-time.Second).UTC().Format(http.TimeFormat)
					}
					resp.Header.Set("Retry-After", hint)
					return resp, nil
				})}
				req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(fmt.Sprintf(`{"model":"GLM-5.3-Flash-Trial","stream":%t,"max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, scenario == "stream_budget"))).WithContext(ctx)
				req.Header.Set("Authorization", "Bearer test-api-key")
				req.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				env.engine.ServeHTTP(response, req)
				want := 2
				if scenario == "final" {
					want = 3
				}
				if scenario == "budget" || scenario == "stream_budget" || scenario == "capacity_after_hint" || scenario == "cancel_wait" {
					want = 1
				}
				if len(sent) != want {
					t.Fatalf("sends=%d want=%d status=%d body=%s", len(sent), want, response.Code, response.Body.String())
				}
				for i := 1; i < len(sent); i++ {
					delay := sent[i].Sub(sent[i-1])
					if scenario == "invalid" || scenario == "overflow" || scenario == "past" {
						if delay < time.Second || delay >= 2*time.Second {
							t.Fatalf("fallback delay=%v", delay)
						}
					} else if scenario == "date" {
						if sent[i].Before(sent[i-1].Add(4 * time.Second).Truncate(time.Second)) {
							t.Fatalf("retried before HTTP date: %v", delay)
						}
					} else if delay < 4*time.Second {
						t.Fatalf("retried before Retry-After: %v", delay)
					}
				}
				windows, err := env.store.GetAllModelCooldowns(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				until := windows[configs[0].ID]["GLM-5.3-Flash-Trial"]
				if scenario == "final" || scenario == "budget" || scenario == "stream_budget" {
					seconds := 4
					if scenario == "budget" {
						seconds = 120
					}
					if response.Code != 429 || gjson.GetBytes(response.Body.Bytes(), "code").Int() != 3010 {
						t.Fatalf("lost busy: %d %s", response.Code, response.Body.String())
					}
					if wantUntil := sent[len(sent)-1].Add(time.Duration(seconds)*time.Second + time.Second - time.Nanosecond).Truncate(time.Second); !until.Equal(wantUntil) {
						t.Fatalf("cooldown=%v want=%v", until, wantUntil)
					}
					// 管理直测也必须遵循持久化模型窗口，返回实际剩余时间且不续期。
					for _, elapsed := range []time.Duration{0, 1500 * time.Millisecond} {
						time.Sleep(elapsed)
						local, sendErr := callZAIStart(context.Background(), env.server, configs[0], "GLM-5.3-Flash-Trial", env.server.client.Do)
						if sendErr != nil {
							t.Fatal(sendErr)
						}
						_ = local.Body.Close()
						remaining, parseErr := strconv.Atoi(local.Header.Get("Retry-After"))
						wantRemaining := int((time.Until(until) + time.Second - 1) / time.Second)
						if local.StatusCode != 429 || parseErr != nil || remaining != wantRemaining || len(sent) != want {
							t.Fatalf("local cooldown: status=%d Retry-After=%q want=%d sends=%d", local.StatusCode, local.Header.Get("Retry-After"), wantRemaining, len(sent))
						}
					}
				} else if scenario == "capacity_after_hint" || scenario == "cancel_wait" {
					if until.After(time.Now()) {
						t.Fatalf("elapsed hint/cancel opened cooldown: %v", until)
					}
					if scenario == "capacity_after_hint" && (response.Code != 429 || gjson.GetBytes(response.Body.Bytes(), "code").Int() != 3010) {
						t.Fatalf("lost busy: %d %s", response.Code, response.Body.String())
					}
				} else if response.Code != 200 || !until.IsZero() {
					t.Fatalf("recovery: status=%d cooldown=%v", response.Code, until)
				}
			})
		})
	}
}

/**
 * @brief 手动检测遵守发送预算，批量检测遵守任务预算，流式和非流式均保留真实 busy。
 * @param t 测试上下文。
 */
func TestZAIChannelTestRetryBudget(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, wait := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/wait=%t", stream, wait), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					env := setupProxyTestEnvWithSettings(t, []testChannel{{name: "budget", upstreamProtocol: "anthropic", models: "GLM-5.3-Flash-Trial", authType: model.AuthTypeZAIOAuth, oauthCredential: `{"jwt_token":"jwt","user_id":"account"}`}}, map[int]string{0: "https://zai.test"}, map[string]string{"upstream_first_byte_timeout": "2", "non_stream_timeout": "2"})
					configs, err := env.store.ListConfigs(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					calls := 0
					env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						if !strings.HasSuffix(req.URL.Path, "/messages") {
							return zaiStartTestResponse(req, 200, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":"active"}],"balances":[{"plan_id":"gift","capabilities":["model:glm-5.3-flash"],"remaining_units":10}]}}`), nil
						}
						calls++
						response := zaiStartTestResponse(req, 429, `{"code":3010,"msg":"busy"}`)
						response.Header.Set("Retry-After", "120")
						return response, nil
					})}
					started := time.Now()
					ctx := context.Background()
					if wait {
						var cancel context.CancelFunc
						ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
						defer cancel()
					}
					result := env.server.executeChannelTestWithCooldown(ctx, configs[0], cooldown.NoKeyIndex, "", &testutil.TestChannelRequest{Model: "GLM-5.3-Flash-Trial", Content: "hello", ClientProtocol: "anthropic", Stream: stream, WaitForCapacity: wait}, true)
					if calls != 1 || result["status_code"] != 429 || result["cooldown_action"] != "trial_model_cooldown" || time.Since(started) >= 2*time.Second {
						t.Fatalf("budget result: calls=%d elapsed=%v result=%v", calls, time.Since(started), result)
					}
				})
			})
		}
	}
}

/**
 * @brief 重新授权后旧 JWT 的迟到认证错误不得冻结新 JWT，风控仍然生效。
 * @param t 测试上下文。
 */
func TestProxy_ZAITrialReauthorizationInFlight(t *testing.T) {
	for _, tc := range []struct {
		name         string ///< 回归场景。
		status       int    ///< 旧 JWT 的上游状态。
		admin        bool   ///< 通过管理检测入口发送。
		risk         bool   ///< 旧响应为账号风控，必须保留保护。
		newFailure   bool   ///< 新 JWT 也被拒绝，必须正常冷却。
		csv          bool   ///< 使用 CSV 覆盖授权。
		csvOverwrite bool   ///< CSV 后续行覆盖前行，前行 JWT 不应成为有效授权。
		paid         bool   ///< 普通模型允许既有付费回退。
	}{
		{name: "unauthorized", status: 401},
		{name: "forbidden", status: 403},
		{name: "account-risk", status: 403, risk: true},
		{name: "egress-risk", status: 405},
		{name: "model-failure", status: 500},
		{name: "admin-unauthorized", status: 401, admin: true},
		{name: "new-jwt-failure", status: 401, newFailure: true},
		{name: "csv", status: 401, csv: true},
		{name: "csv-new-jwt-failure", status: 401, csv: true, newFailure: true},
		{name: "csv-overwritten-credential", status: 401, csv: true, csvOverwrite: true},
		{name: "paid-fallback", status: 401, paid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, configs := newZAIStartHarness(t)
			cfg := configs[0]
			started, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			control := env.server.client
			if tc.csv {
				if _, _, err := env.server.commitZAICredential(context.Background(), &zaiauth.Credential{JWTToken: "jwt-a", UserID: "a"}, oauthLoginOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			var calls atomic.Int32
			var paidCalls atomic.Int32
			env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(req.URL.Path, "/messages") {
					return control.Do(req)
				}
				calls.Add(1)
				if !strings.Contains(req.URL.Path, "/zcode-plan/") {
					paidCalls.Add(1)
				}
				if req.Header.Get("Authorization") == "Bearer jwt-a" {
					if calls.Load() == 1 {
						close(started)
					}
					<-release
					body := `{"error":{"message":"invalid JWT"}}`
					if tc.risk {
						body = `{"code":3012,"msg":"unusual activity"}`
					}
					if tc.status == 405 {
						body = `<html>blocked</html>`
					}
					return zaiStartTestResponse(req, tc.status, body), nil
				}
				if tc.newFailure {
					return zaiStartTestResponse(req, 401, `{"error":{"message":"invalid new JWT"}}`), nil
				}
				return zaiStartTestResponse(req, 200, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), nil
			})}
			request := func() *httptest.ResponseRecorder {
				if tc.admin {
					id := strconv.FormatInt(cfg.ID, 10)
					c, response := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+id+"/test", map[string]any{"model": "GLM-5.3-Flash-Trial", "content": "hello", "client_protocol": "anthropic", "stream": false}))
					c.Params = gin.Params{{Key: "id", Value: id}}
					env.server.HandleChannelTest(c)
					return response
				}
				name := "GLM-5.3-Flash-Trial"
				if tc.paid {
					name = "GLM-5.3-Flash"
				}
				return doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": name, "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "hello"}}}, map[string]string{"X-CCLoad-Channel": cfg.Name})
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- request() }()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("old request did not start")
			}
			if tc.csv {
				var csvBody, body bytes.Buffer
				rows := [][]string{
					{"name", "urls", "models", "auth_type", "oauth_credential", "enabled"},
					{cfg.Name, `[{"url":"https://zcode.z.ai/api/v1/ultra-zai/anthropic"}]`, "GLM-5.3-Flash-Trial", model.AuthTypeZAIOAuth, `{"jwt_token":"new-jwt","user_id":"a"}`, "true"},
				}
				if tc.csvOverwrite {
					rows[1][0] = "overwritten"
					last := append([]string(nil), rows[1]...)
					last[4] = `{"jwt_token":"other-jwt","user_id":"other"}`
					rows = append(rows, last)
				}
				if err := csv.NewWriter(&csvBody).WriteAll(rows); err != nil {
					t.Fatal(err)
				}
				writer := multipart.NewWriter(&body)
				part, err := writer.CreateFormFile("file", "zai.csv")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := part.Write(csvBody.Bytes()); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				req := newRequest(http.MethodPost, "/admin/channels/import", &body)
				req.Header.Set("Content-Type", writer.FormDataContentType())
				c, result := newTestContext(t, req)
				env.server.HandleImportChannelsCSV(c)
				if result.Code != 200 || mustParseAPIResponse[ChannelImportSummary](t, result.Body.Bytes()).Data.Updated != 1 {
					t.Fatalf("CSV import: %s", result.Body.String())
				}
			} else {
				updated, created, err := env.server.commitZAICredential(context.Background(), &zaiauth.Credential{JWTToken: "new-jwt", UserID: "a"}, oauthLoginOptions{})
				if err != nil || created || updated == nil {
					t.Fatalf("reauthorization: created=%v err=%v", created, err)
				}
			}
			unblock()
			select {
			case response := <-done:
				if tc.admin {
					if gjson.GetBytes(response.Body.Bytes(), "data.success").Bool() || gjson.GetBytes(response.Body.Bytes(), "data.cooldown_action").String() != "trial_error_no_cooldown" {
						t.Fatalf("stale admin failure: %s", response.Body.String())
					}
				} else if (response.Code == 200) != tc.paid {
					t.Fatalf("old attempt result: status=%d paid=%d", response.Code, paidCalls.Load())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("old request did not finish")
			}
			response := request()
			recovered := !tc.risk && !tc.csvOverwrite && (tc.status == 401 || tc.status == 403)
			wantCalls := int32(1)
			if recovered {
				wantCalls = 2
			}
			if tc.paid {
				wantCalls++
				if paidCalls.Load() != 1 {
					t.Fatalf("paid fallback=%d", paidCalls.Load())
				}
			}
			success := response.Code == 200
			if tc.admin {
				success = gjson.GetBytes(response.Body.Bytes(), "data.success").Bool()
			}
			if success != (recovered && !tc.newFailure) || calls.Load() != wantCalls {
				t.Fatalf("after late failure: status=%d calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
			}
			if tc.newFailure {
				response = request()
				remaining, err := strconv.Atoi(response.Header().Get("Retry-After"))
				if response.Code != 429 || calls.Load() != 2 || err != nil || remaining <= 0 {
					t.Fatalf("new JWT failure not cooled: %d %s", response.Code, response.Body.String())
				}
			}
		})
	}
}

/**
 * @brief 从代理入口验证 busy 恢复、冻结 wire、公共容量释放和容量中止不回付费。
 * @param t 测试上下文。
 */
func TestProxy_ZAIBusyRetryContract(t *testing.T) {
	for _, scenario := range []string{"recover_second", "recover_third", "rpm_abort", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			env, configs := newZAIStartHarness(t)
			cfg := configs[0]
			cfg.MaxConcurrency = 1
			if scenario == "rpm_abort" {
				cfg.RPMLimit = 1
			}
			cfg.CustomRequestRules = &model.CustomRequestRules{Headers: []model.CustomHeaderRule{{Action: model.RuleActionAppend, Name: "X-Retry-Marker", Value: "once"}}}
			if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
				t.Fatal(err)
			}
			env.server.InvalidateChannelListCache()
			control := env.server.client
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var firstBody []byte
			var firstHeader http.Header
			var attempts, paid int
			env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(req.URL.Path, "/messages") {
					return control.Do(req)
				}
				if !strings.Contains(req.URL.Path, "/zcode-plan/") {
					paid++
					return zaiStartTestResponse(req, 500, `{}`), nil
				}
				attempts++
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Error(err)
				}
				if gjson.GetBytes(body, "max_tokens").Int() != 16016 || gjson.GetBytes(body, "thinking.budget_tokens").Int() != 16000 {
					t.Error("busy retry changed default thinking budget or output allowance")
				}
				if attempts == 1 {
					firstBody, firstHeader = body, req.Header.Clone()
				} else if !bytes.Equal(body, firstBody) || !reflect.DeepEqual(req.Header, firstHeader) {
					t.Error("busy retry rebuilt wire")
				}
				if got := anthropicHeaderValue(req.Header, "X-Retry-Marker"); got != "once" {
					t.Errorf("header append=%q", got)
				}
				if scenario == "cancel" {
					cancel()
				}
				if scenario == "recover_second" && attempts == 2 || scenario == "recover_third" && attempts == 3 {
					return zaiStartTestResponse(req, 200, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), nil
				}
				return zaiStartTestResponse(req, 429, `{"code":3010,"msg":"busy"}`), nil
			})}
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"GLM-5.3-Flash","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer test-api-key")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-CCLoad-Channel", cfg.Name)
			response := httptest.NewRecorder()
			env.engine.ServeHTTP(response, req)
			want := 1
			if scenario == "recover_second" {
				want = 2
			}
			if scenario == "recover_third" {
				want = 3
			}
			if attempts != want || paid != 0 {
				t.Fatalf("trial=%d paid=%d", attempts, paid)
			}
			windows, err := env.store.GetAllModelCooldowns(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "rpm_abort" {
				if response.Code != 429 || gjson.GetBytes(response.Body.Bytes(), "code").Int() != 3010 {
					t.Fatalf("lost real busy: %d %s", response.Code, response.Body.String())
				}
				if !windows[cfg.ID]["GLM-5.3-Flash-Trial"].After(time.Now()) {
					t.Fatal("final busy was not cooled")
				}
			} else {
				if len(windows[cfg.ID]) != 0 {
					t.Fatalf("success/cancel cooled: %v", windows)
				}
				if scenario != "cancel" && response.Code != 200 {
					t.Fatalf("response=%d", response.Code)
				}
			}
		})
	}
}

/**
 * @brief 验证公共入口的 Trial 参数、commentary、环境脱敏和会话来源；Paid 保持原归一。
 * @param t 测试上下文。
 */
func TestProxy_ZAIPackageFinalization(t *testing.T) {
	for _, paid := range []bool{false, true} {
		t.Run(fmt.Sprintf("paid=%t", paid), func(t *testing.T) {
			env, configs := newZAIStartHarness(t)
			cfg := configs[0]
			// 管理入口的 identity 约定不能覆盖普通代理显式配置的压缩协商。
			cfg.CustomRequestRules = &model.CustomRequestRules{Headers: []model.CustomHeaderRule{{Action: model.RuleActionOverride, Name: "Accept-Encoding", Value: "gzip"}}}
			if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
				t.Fatal(err)
			}
			env.server.InvalidateChannelListCache()
			if paid {
				previous := cfg.OAuthCredential
				credential, err := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
				if err != nil {
					t.Fatal(err)
				}
				credential.JWTToken = ""
				cfg.OAuthCredential, err = credential.JSON()
				if err != nil {
					t.Fatal(err)
				}
				if changed, err := env.store.CompareAndSwapOAuthCredential(context.Background(), cfg.ID, model.AuthTypeZAIOAuth, previous, cfg.OAuthCredential); err != nil || !changed {
					t.Fatalf("replace test credential: changed=%v err=%v", changed, err)
				}
				env.server.InvalidateChannelListCache()
			}
			if w := batchUpdateSetting(t, env.server, privacySettingKey, `{"enabled":true,"personal":true}`); w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			control := env.server.client
			var captured []byte
			var headers http.Header
			env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(req.URL.Path, "/messages") {
					return control.Do(req)
				}
				captured, _ = io.ReadAll(req.Body)
				headers = req.Header.Clone()
				return zaiStartTestResponse(req, 200, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), nil
			})}
			body := map[string]any{"model": "GLM-5.3-Flash", "max_tokens": 16, "temperature": 0.4, "top_p": 0.7, "top_k": 12, "thinking": map[string]any{"type": "auto", "budget_tokens": 1024}, "tool_choice": map[string]any{"type": "any"}, "tools": []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}}, "messages": []any{map[string]any{"role": "user", "content": "hello"}, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": "progress text", "signature": "cpa:commentary:v1"}, map[string]any{"type": "text", "text": "ready"}}}, map[string]any{"role": "user", "content": "continue"}}}
			// 原生调用方携带的 CCH 必须在付费最终归一后重签。
			if paid {
				body["system"] = []map[string]string{{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.280.abc; cc_entrypoint=cli; cch=00000;"}}
				body["metadata"] = map[string]string{"user_id": `{"device_id":"caller-device","account_uuid":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee","session_id":"11111111-2222-4333-8444-555555555555"}`}
			}
			response := doProxyRequest(t, env.engine, "/v1/messages", body, map[string]string{"X-CCLoad-Channel": cfg.Name, "User-Agent": "claude-cli/2.1.280 (external, cli)", "X-App": "cli", "x-session-id": "11111111-2222-4333-8444-555555555555", "x-magpie-work-dir": url.PathEscape("/workspace/person@example.org"), "x-magpie-work-git": "yes"})
			if response.Code != 200 {
				t.Fatalf("response=%d %s", response.Code, response.Body.String())
			}
			root := gjson.ParseBytes(captured)
			if got := anthropicHeaderValue(headers, "Accept-Encoding"); got != "gzip" {
				t.Fatalf("proxy encoding=%q", got)
			}
			if paid {
				resigned, err := finalizeAnthropicCCH(captured)
				if err != nil || !bytes.Equal(resigned, captured) || strings.Contains(root.Get("system.0.text").String(), "cch=00000;") {
					t.Fatalf("paid CCH is stale: %v", err)
				}
				if root.Get("temperature").Exists() || root.Get("top_p").Exists() || root.Get("thinking").Exists() {
					t.Fatalf("paid normalization: temperature=%s top_p=%s thinking=%s tool_choice=%s", root.Get("temperature").Raw, root.Get("top_p").Raw, root.Get("thinking").Raw, root.Get("tool_choice").Raw)
				}
			} else {
				if root.Get("temperature").Float() != 0.4 || root.Get("top_p").Float() != 0.7 || root.Get("top_k").Int() != 12 || root.Get("thinking.type").String() != "auto" {
					t.Fatal("Trial GLM parameters lost")
				}
				identity := decodeZAIRequestIdentity(t, captured)
				if identity.SessionID != "11111111-2222-4333-8444-555555555555" || anthropicHeaderValue(headers, "x-session-id") != identity.SessionID {
					t.Fatal("explicit session lost")
				}
				commentary := root.Get("messages.2.content.0")
				if commentary.Get("type").String() != "text" || commentary.Get("text").String() != "progress text" || commentary.Get("signature").Exists() {
					t.Fatalf("commentary=%s", commentary.Raw)
				}
				environment := root.Get("system.2.text").String()
				if !strings.Contains(environment, "Primary working directory: /workspace/") || strings.Contains(environment, "person@example.org") || !strings.Contains(environment, "Is a git repository: true") {
					t.Fatal("environment missing or not masked")
				}
			}
			if anthropicHeaderValue(headers, "x-magpie-work-dir") != "" || anthropicHeaderValue(headers, "x-magpie-work-git") != "" {
				t.Fatal("private environment headers leaked")
			}
		})
	}
}

/**
 * @brief 通过真实代理入口验证缺省 high 的预算、显式设置优先级及渠道规则。
 * @param t 测试上下文。
 */
func TestProxy_ZAIThinkingDefaults(t *testing.T) {
	for _, paid := range []bool{false, true} {
		for _, tc := range []struct {
			name      string
			path      string
			extra     string
			maxTokens int64
			thinking  string
			budget    int64
			effort    string
			rules     []model.CustomBodyRule
			repair    bool
			paidOnly  bool
		}{
			{name: "native", extra: `{"temperature":0.4,"top_p":0.7,"top_k":12}`, maxTokens: 20096, thinking: "enabled", budget: 16000, effort: "high"},
			{name: "chat", path: "/v1/chat/completions", maxTokens: 20096, thinking: "enabled", budget: 16000, effort: "high"},
			{name: "responses", path: "/v1/responses", extra: `{"input":"hello","max_output_tokens":4096}`, maxTokens: 20096, thinking: "enabled", budget: 16000, effort: "high"},
			{name: "capped", extra: `{"max_tokens":120000}`, maxTokens: 128000, thinking: "enabled", budget: 16000, effort: "high"},
			{name: "large_integer", extra: `{"max_tokens":9223372036854775807}`, maxTokens: 128000, thinking: "enabled", budget: 16000, effort: "high"},
			{name: "missing_max", maxTokens: 17024, thinking: "enabled", budget: 16000, effort: "high"},
			{name: "invalid_max", extra: `{"max_tokens":0}`, maxTokens: 0},
			{name: "thinking_repair", maxTokens: 20096, repair: true},
			{name: "glm52", extra: `{"model":"GLM-5.2"}`, maxTokens: 14336, thinking: "enabled", budget: 10240, paidOnly: true},
			{name: "glm51_cap", extra: `{"model":"GLM-5.1","max_tokens":60000}`, maxTokens: 64000, thinking: "enabled", budget: 10240, paidOnly: true},
			{name: "glm47_cap", extra: `{"model":"GLM-4.7","max_tokens":130000}`, maxTokens: 131072, thinking: "enabled", budget: 10240, paidOnly: true},
			{name: "glm45_cap", extra: `{"model":"GLM-4.5-Air","max_tokens":98000}`, maxTokens: 98304, thinking: "enabled", budget: 10240, paidOnly: true},
			{name: "vision", extra: `{"model":"GLM-4.6V"}`, maxTokens: 4096, paidOnly: true},
			{name: "unknown", extra: `{"model":"GLM-unknown"}`, maxTokens: 4096, paidOnly: true},
			{name: "disabled", extra: `{"thinking":{"type":"disabled"}}`, maxTokens: 4096, thinking: "disabled"},
			{name: "explicit_budget", extra: `{"thinking":{"type":"enabled","budget_tokens":2048}}`, maxTokens: 4096, thinking: "enabled", budget: 2048},
			{name: "explicit_effort", extra: `{"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}}`, maxTokens: 4096, thinking: "adaptive", effort: "low"},
			{name: "effort_only", extra: `{"output_config":{"effort":"low"}}`, maxTokens: 4096, effort: "low"},
			{name: "null_thinking", extra: `{"thinking":null}`, maxTokens: 4096},
			{name: "suffix_none", extra: `{"model":"GLM-5.3-Flash(none)"}`, maxTokens: 4096},
			{name: "chat_none", path: "/v1/chat/completions", extra: `{"reasoning_effort":"none"}`, maxTokens: 4096, thinking: "disabled"},
			{name: "forced_tool", extra: `{"tool_choice":{"type":"any"},"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`, maxTokens: 4096},
			{name: "rules_override", maxTokens: 8000, thinking: "enabled", budget: 2048, effort: "low", rules: []model.CustomBodyRule{
				{Action: model.RuleActionOverride, Path: "thinking.budget_tokens", Value: json.RawMessage(`2048`)},
				{Action: model.RuleActionOverride, Path: "output_config.effort", Value: json.RawMessage(`"low"`)},
				{Action: model.RuleActionOverride, Path: "max_tokens", Value: json.RawMessage(`8000`)},
			}},
			{name: "rules_remove", maxTokens: 20096, rules: []model.CustomBodyRule{
				{Action: model.RuleActionRemove, Path: "thinking"},
				{Action: model.RuleActionRemove, Path: "output_config.effort"},
			}},
		} {
			if tc.paidOnly && !paid {
				continue // 测试套餐只提供 Flash 的体验权益，其他模型验证付费出站。
			}
			t.Run(fmt.Sprintf("paid=%t/%s", paid, tc.name), func(t *testing.T) {
				env, configs := newZAIStartHarness(t)
				cfg := configs[0]
				if paid {
					previous := cfg.OAuthCredential
					credential, err := zaiauth.ParseCredential([]byte(previous))
					if err != nil {
						t.Fatal(err)
					}
					credential.JWTToken = ""
					cfg.OAuthCredential, err = credential.JSON()
					if err != nil {
						t.Fatal(err)
					}
					if changed, err := env.store.CompareAndSwapOAuthCredential(context.Background(), cfg.ID, model.AuthTypeZAIOAuth, previous, cfg.OAuthCredential); err != nil || !changed {
						t.Fatalf("replace test credential: changed=%v err=%v", changed, err)
					}
				}
				cfg.CustomRequestRules = &model.CustomRequestRules{Body: tc.rules}
				if tc.paidOnly {
					cfg.ModelEntries = append(cfg.ModelEntries, model.ModelEntry{Model: gjson.Get(tc.extra, "model").String()})
				}
				if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
					t.Fatal(err)
				}
				env.server.InvalidateChannelListCache()
				control := env.server.client
				var captured []byte
				calls := 0
				env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					if !strings.HasSuffix(req.URL.Path, "/messages") {
						return control.Do(req)
					}
					var err error
					captured, err = io.ReadAll(req.Body)
					if err != nil {
						t.Error(err)
					}
					if trial := strings.Contains(req.URL.Path, "/zcode-plan/"); trial == paid {
						t.Errorf("unexpected package: %s", req.URL.Path)
					}
					calls++
					if tc.repair && calls == 1 {
						if gjson.GetBytes(captured, "max_tokens").Int() != 20096 || gjson.GetBytes(captured, "thinking.budget_tokens").Int() != 16000 {
							t.Error("first attempt missing default thinking allowance")
						}
						return zaiStartTestResponse(req, 400, `{"error":{"type":"invalid_request_error","message":"thinking blocks are not supported"}}`), nil
					}
					return zaiStartTestResponse(req, 200, `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3-Flash","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), nil
				})}
				body := map[string]any{"model": "GLM-5.3-Flash", "max_tokens": 4096, "messages": []map[string]string{{"role": "user", "content": "hello"}}}
				if tc.repair && !paid {
					body["model"] = "GLM-5.3-Flash-Trial" // 固定体验，验证正文修复而非套餐回退。
				}
				if tc.name == "missing_max" {
					delete(body, "max_tokens")
				}
				if tc.extra != "" {
					decoder := json.NewDecoder(strings.NewReader(tc.extra))
					decoder.UseNumber()
					if err := decoder.Decode(&body); err != nil {
						t.Fatal(err)
					}
				}
				path := tc.path
				if path == "" {
					path = "/v1/messages"
				}
				response := doProxyRequest(t, env.engine, path, body, map[string]string{"X-CCLoad-Channel": cfg.Name})
				if response.Code != 200 {
					t.Fatalf("response=%d %s", response.Code, response.Body.String())
				}
				wantCalls := 1
				if tc.repair {
					wantCalls = 2
				}
				if calls != wantCalls {
					t.Fatalf("sends=%d, want %d", calls, wantCalls)
				}
				root := gjson.ParseBytes(captured)
				wantThinking := tc.thinking
				if paid && wantThinking == "disabled" {
					wantThinking = "" // 付费既有归一会移除 disabled，不能恢复默认 high。
				}
				if root.Get("max_tokens").Int() != tc.maxTokens || root.Get("thinking.type").String() != wantThinking ||
					root.Get("thinking.budget_tokens").Int() != tc.budget || root.Get("output_config.effort").String() != tc.effort {
					t.Fatalf("max_tokens=%s thinking=%s output_config=%s", root.Get("max_tokens").Raw, root.Get("thinking").Raw, root.Get("output_config").Raw)
				}
				if tc.name == "native" && (root.Get("temperature").Exists() || root.Get("top_p").Exists() || root.Get("top_k").Exists()) {
					t.Fatal("default thinking retained incompatible sampling parameters")
				}
			})
		}
	}
}

/**
 * @brief 管理检测与代理使用同一默认 high，表单关闭和模型后缀不能丢失。
 * @param t 测试上下文。
 */
func TestZAIChannelTestThinkingDefaults(t *testing.T) {
	for _, client := range []string{"anthropic", "openai", "codex", "gemini"} {
		for _, mode := range []string{"default", "blank", "none", "normalized_none", "suffix_none", "suffix_zero"} {
			t.Run(client+"/"+mode, func(t *testing.T) {
				env, configs := newZAIStartHarness(t)
				control := env.server.client
				var captured []byte
				env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					if !strings.HasSuffix(req.URL.Path, "/messages") {
						return control.Do(req)
					}
					var err error
					captured, err = io.ReadAll(req.Body)
					if err != nil {
						t.Error(err)
					}
					return zaiStartTestResponse(req, 200, `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3-Flash","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), nil
				})}
				body := map[string]any{"model": "GLM-5.3-Flash", "max_tokens": 4096, "content": "hello", "client_protocol": client, "stream": false}
				switch mode {
				case "blank":
					body["thinking_effort"] = " "
				case "none":
					body["thinking_effort"] = "none"
				case "normalized_none":
					body["thinking_effort"] = " NONE "
				case "suffix_none":
					body["model"] = "GLM-5.3-Flash(none)"
				case "suffix_zero":
					body["model"] = "GLM-5.3-Flash(0)"
				}
				id := fmt.Sprint(configs[0].ID)
				c, response := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+id+"/test", body))
				c.Params = gin.Params{{Key: "id", Value: id}}
				env.server.HandleChannelTest(c)
				if response.Code != 200 || len(captured) == 0 {
					t.Fatalf("response=%d %s", response.Code, response.Body.String())
				}
				root := gjson.ParseBytes(captured)
				if mode == "default" || mode == "blank" {
					if root.Get("thinking.type").String() != "enabled" || root.Get("thinking.budget_tokens").Int() != 16000 || root.Get("output_config.effort").String() != "high" || root.Get("max_tokens").Int() != 20096 {
						t.Fatalf("default: max_tokens=%s thinking=%s effort=%s", root.Get("max_tokens").Raw, root.Get("thinking").Raw, root.Get("output_config.effort").Raw)
					}
				} else if kind := root.Get("thinking.type").String(); kind != "" && kind != "disabled" || root.Get("output_config.effort").Exists() || root.Get("max_tokens").Int() != 4096 {
					t.Fatalf("explicit off: max_tokens=%s thinking=%s effort=%s", root.Get("max_tokens").Raw, root.Get("thinking").Raw, root.Get("output_config.effort").Raw)
				}
			})
		}
	}
}

/**
 * @brief 新增环境文本触发隐私容量保护时阻止 Trial 和付费，并沿原出口计一次阻止。
 * @param t 测试上下文。
 */
func TestProxy_ZAIEnvironmentPrivacyFailure(t *testing.T) {
	env, configs := newZAIStartHarness(t)
	if w := batchUpdateSetting(t, env.server, privacySettingKey, `{"enabled":true,"rules":[{"kind":"TEST","regex":"private-value-[0-9]+"}]}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	control := env.server.client
	var sent atomic.Int32
	env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/messages") {
			sent.Add(1)
			return zaiStartTestResponse(req, 200, `{}`), nil
		}
		return control.Do(req)
	})}
	var cwd strings.Builder
	// 明确匹配独立值，避免邮箱有效性规则让容量边界测试失效。
	for i := range 4200 {
		fmt.Fprintf(&cwd, "private-value-%d ", i)
	}
	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": "GLM-5.3-Flash", "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "hello"}}}, map[string]string{"X-CCLoad-Channel": configs[0].Name, "x-magpie-work-dir": url.PathEscape(cwd.String())})
	if response.Code != 400 || sent.Load() != 0 {
		t.Fatalf("privacy failure response=%d sends=%d", response.Code, sent.Load())
	}
	metrics := readPrivacyRuntime(t, env.server)
	if metrics.BlockedRequests != 1 || metrics.MaskedRequests != 0 {
		t.Fatalf("privacy metrics=%+v", metrics)
	}
	windows, err := env.store.GetAllModelCooldowns(context.Background())
	if err != nil || len(windows) != 0 {
		t.Fatalf("privacy failure cooled: %v %v", windows, err)
	}
}

/**
 * @brief 建立真实套餐查询与可控推理发送器之间的测试边界，含重复账号渠道。
 * @param t 测试上下文。
 * @return 代理测试环境。
 * @return 四个渠道快照，最后一个与第一个属于同一账号。
 */
func newZAIStartHarness(t *testing.T) (*proxyTestEnv, []*model.Config) {
	t.Helper()
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/biz/subscription/list":
			_, _ = io.WriteString(w, `{"code":0,"data":[{"status":"VALID"}]}`)
		case "/api/v1/zcode-plan/billing/balance":
			_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":"active"}],"balances":[{"plan_id":"gift","capabilities":["model:glm-5.3-flash"],"remaining_units":10}]}}`)
		default:
			t.Errorf("unexpected control endpoint: %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	var channels []testChannel
	urls := make(map[int]string)
	for i, account := range []string{"a", "b", "c", "a"} {
		channels = append(channels, testChannel{name: fmt.Sprintf("start-%d", i), upstreamProtocol: "anthropic", models: "GLM-5.3-Flash,GLM-5.3-Flash-Trial", authType: model.AuthTypeZAIOAuth,
			oauthCredential: fmt.Sprintf(`{"api_key":"key-%s.secret","jwt_token":"jwt-%s","user_id":"%s"}`, account, account, account)})
		urls[i] = upstream.URL
	}
	env := setupProxyTestEnv(t, channels, urls)
	env.server.zaiService.OAuthBaseURL, env.server.zaiService.BizBaseURL = upstream.URL+"/api/v1", upstream.URL
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 数据库存储顺序不是测试契约，按已知名称整理渠道。
	ordered := make([]*model.Config, len(configs))
	for _, cfg := range configs {
		for i := range ordered {
			if cfg.Name == fmt.Sprintf("start-%d", i) {
				ordered[i] = cfg
			}
		}
	}
	return env, ordered
}

/**
 * @brief 真实终态失败冷却体验模型，坏请求不冷却，已开始的流不得重放。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartFinalFailureRetiresAccount(t *testing.T) {
	const success = `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3-Flash","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
	const prefix = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"GLM-5.3-Flash\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"
	for _, tc := range []struct {
		name    string
		status  int
		stream  bool
		body    string
		cooling bool
	}{
		{"late-sse-error", 200, true, prefix + "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"backend failed after output\"}}\n\n", true},
		{"incomplete-stream", 200, true, prefix, true},
		{"model-not-found", 404, false, `{"type":"error","error":{"type":"not_found_error","message":"model not found"}}`, true},
		{"bad-request", 400, false, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 215000 tokens > 200000 maximum"}}`, false},
		{"late-concurrency-error", 200, true, prefix + "event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":\"3008\",\"message\":\"concurrency limit exceeded\"}}\n\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, configs := newZAIStartHarness(t)
			controlClient := env.server.client
			var fail atomic.Bool
			var reserveCalls atomic.Int32
			env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(req.URL.Path, "/messages") {
					return controlClient.Do(req)
				}
				account := anthropicHeaderValue(req.Header, "Authorization")
				if account == "Bearer jwt-c" {
					reserveCalls.Add(1)
				}
				if fail.Load() && (account == "Bearer jwt-a" || account == "Bearer jwt-b") {
					resp := zaiStartTestResponse(req, tc.status, tc.body)
					if tc.stream {
						resp.Header.Set("Content-Type", "text/event-stream")
					}
					return resp, nil
				}
				return zaiStartTestResponse(req, http.StatusOK, success), nil
			})}
			request := func(index int, stream bool) *httptest.ResponseRecorder {
				return doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
					"model": "GLM-5.3-Flash-Trial", "stream": stream, "max_tokens": 16,
					"messages": []map[string]string{{"role": "user", "content": "hello"}},
				}, map[string]string{"X-CCLoad-Channel": configs[index].Name})
			}
			for _, index := range []int{0, 1} {
				if response := request(index, false); response.Code != http.StatusOK {
					t.Fatalf("warm account %d: %d %s", index, response.Code, response.Body.String())
				}
			}
			fail.Store(true)
			for _, index := range []int{0, 1} {
				response := request(index, tc.stream)
				if tc.stream {
					var text string
					for event := range strings.SplitSeq(response.Body.String(), "\n\n") {
						_, data := parseSSEEventChunk([]byte(event + "\n\n"))
						text += gjson.GetBytes(data, "delta.text").String()
					}
					if text != "partial" {
						t.Fatalf("account %d did not commit text before failure: %q", index, text)
					}
				}
			}
			response := request(2, false)
			if response.Code != http.StatusOK || reserveCalls.Load() != 1 {
				t.Fatalf("idle account could not switch on explicit demand: %d %s", response.Code, response.Body.String())
			}
			models, err := env.store.GetAllModelCooldowns(context.Background())
			if err != nil || models[configs[0].ID]["GLM-5.3-Flash-Trial"].After(time.Now()) != tc.cooling {
				t.Fatalf("wrong model cooldown: %v %v", models, err)
			}

			cooldowns, err := env.store.GetAllChannelCooldowns(context.Background())
			if err != nil || len(cooldowns) != 0 {
				t.Fatalf("Trial failure must not cool paid channels: %v %v", cooldowns, err)
			}
		})
	}
}

/**
 * @brief CSV 跨站点替换与认证重试竞争时停止旧尝试，后续请求使用完整的新站点配置。
 * @param t 测试上下文。
 */
func TestProxy_ZAICrossSiteReplacementRetry(t *testing.T) {
	for _, tc := range []struct {
		name          string
		oldSite       zaiauth.Site
		newSite       zaiauth.Site
		accountToken  string
		duringRefresh bool
		admin         bool
	}{
		{"before-refresh", zaiauth.SiteZAI, zaiauth.SiteBigModel, "old-access", false, false},
		{"api-key-only", zaiauth.SiteZAI, zaiauth.SiteBigModel, "", false, false},
		{"during-refresh", zaiauth.SiteBigModel, zaiauth.SiteZAI, "old-access", true, false},
		{"admin-before-refresh", zaiauth.SiteZAI, zaiauth.SiteBigModel, "old-access", false, true},
		{"admin-api-key-only", zaiauth.SiteZAI, zaiauth.SiteBigModel, "", false, true},
		{"admin-during-refresh", zaiauth.SiteBigModel, zaiauth.SiteZAI, "old-access", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oldCredential := fmt.Sprintf(`{"site":%q,"api_key":"old.secret","access_token":%q}`, tc.oldSite, tc.accountToken)
			env := setupProxyTestEnv(t, []testChannel{{name: "cross-site", authType: model.AuthTypeZAIOAuth, oauthCredential: oldCredential, models: "GLM-5.3", upstreamProtocol: "anthropic"}}, map[int]string{0: tc.oldSite.CodingPlanBaseURL()})
			ctx := context.Background()
			configs, err := env.store.ListConfigs(ctx)
			if err != nil || len(configs) != 1 {
				t.Fatalf("list configs: %v", err)
			}
			original := configs[0]
			replace := func() error {
				imported := original.Clone()
				imported.ID = 0
				imported.URLs = model.ChannelURLs{{URL: tc.newSite.CodingPlanBaseURL(), Protocols: []string{"anthropic"}}}
				imported.OAuthCredential = fmt.Sprintf(`{"site":%q,"api_key":"replacement.secret"}`, tc.newSite)
				batch := []*model.ChannelWithKeys{{Config: imported}}
				if err := env.server.prepareCSVImportOAuthCredentials(ctx, batch); err != nil {
					return fmt.Errorf("validate CSV: %w", err)
				}
				created, updated, err := env.store.ImportChannelBatch(ctx, batch)
				if err != nil || created != 0 || updated != 1 {
					return fmt.Errorf("import CSV created=%d updated=%d: %v", created, updated, err)
				}
				env.server.invalidateOAuthCredential(imported.ID, model.AuthTypeZAIOAuth)
				env.server.InvalidateChannelListCache()
				return nil
			}
			var sends []string
			env.server.client = &http.Client{Transport: oauthUsageRoundTripper(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/api/paas/v4/models":
					return jsonResponse(req, `{"data":[{"id":"GLM-5.3"}]}`)
				case "/api/biz/customer/getCustomerInfo":
					return jsonResponse(req, `{"code":0,"data":{"userId":"user","organizations":[{"organizationId":"org","projects":[{"projectId":"project"}]}]}}`)
				case "/api/biz/v1/organization/org/projects/project/api_keys":
					return jsonResponse(req, `{"code":0,"data":[{"name":"zcode-api-key","apiKey":"resolved"}]}`)
				case "/api/biz/v1/organization/org/projects/project/api_keys/copy/resolved":
					if err := replace(); err != nil {
						return nil, err
					}
					return jsonResponse(req, `{"code":0,"data":{"secretKey":"secret"}}`)
				}
				if !strings.HasSuffix(req.URL.Path, "/v1/messages") {
					return nil, fmt.Errorf("unexpected request: %s", req.URL)
				}
				key := anthropicHeaderValue(req.Header, "x-api-key")
				sends = append(sends, req.URL.String()+" key="+key)
				if len(sends) == 1 {
					if !tc.duringRefresh {
						if err := replace(); err != nil {
							return nil, err
						}
					}
					return jsonResponseStatus(req, http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`)
				}
				return jsonResponse(req, `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			})}
			request := func() *httptest.ResponseRecorder {
				if tc.admin {
					id := fmt.Sprint(original.ID)
					c, response := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/"+id+"/test", map[string]any{
						"model": "GLM-5.3", "content": "hello", "client_protocol": "anthropic", "stream": false,
					}))
					c.Params = gin.Params{{Key: "id", Value: id}}
					env.server.HandleChannelTest(c)
					return response
				}
				return doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": "GLM-5.3", "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "hello"}}}, nil)
			}
			request()
			if len(sends) != 1 {
				t.Fatalf("old attempt sent replacement credential: %v", sends)
			}
			cooldowns, err := env.store.GetAllChannelCooldowns(ctx)
			if err != nil || len(cooldowns) != 0 {
				t.Fatalf("replacement account was cooled: %v %v", cooldowns, err)
			}
			response := request()
			want := tc.newSite.CodingPlanBaseURL() + "/v1/messages key=replacement.secret"
			if response.Code != http.StatusOK || len(sends) != 2 || sends[1] != want {
				t.Fatalf("new request must use replacement site: status=%d sends=%v body=%s", response.Code, sends, response.Body.String())
			}
		})
	}
}

/**
 * @brief 从公共发送分流边界发起一次逻辑请求，不绕过套餐选择。
 * @param ctx 请求上下文。
 * @param s 服务器。
 * @param cfg 渠道配置。
 * @param name 请求模型，支持显式 Trial 和普通付费模型。
 * @param send 可控实际发送器。
 * @return 最终响应，调用方负责关闭。
 * @return 发送错误。
 */
func callZAIStart(ctx context.Context, s *Server, cfg *model.Config, name string, send func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	req, err := http.NewRequestWithContext(context.WithValue(ctx, zaiPlanRequestKey{}, true), http.MethodPost, "http://paid.invalid/v1/messages", strings.NewReader(fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, name)))
	if err != nil {
		return nil, err
	}
	return s.doUpstreamRequestWithSender(cfg, req, send)
}

/**
 * @brief 构造可关闭的上游响应，保留真实发送请求以检查 Trial 身份。
 * @param req 实际出站请求。
 * @param status 上游状态码。
 * @param body 上游 JSON 或 SSE 正文。
 * @return 尚未关闭的响应。
 */
func zaiStartTestResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}
}

/**
 * @brief 同账号跨重复渠道共享三并发及三等待，付费和其他提供商不受影响。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartCapacity(t *testing.T) {

	env, configs := newZAIStartHarness(t)
	var calls atomic.Int32
	send := func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return zaiStartTestResponse(req, 200, `{}`), nil
	}
	var held []io.ReadCloser
	for _, index := range []int{0, 3, 0} {
		resp, err := callZAIStart(context.Background(), env.server, configs[index], "GLM-5.3-Flash-Trial", send)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("admit: %v %v", resp, err)
		}
		held = append(held, resp.Body)
		defer func() { _ = resp.Body.Close() }()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan int, 4)
	for range 4 {
		go func() {
			resp, err := callZAIStart(ctx, env.server, configs[3], "GLM-5.3-Flash-Trial", send)
			if err != nil {
				done <- 0
				return
			}
			defer func() { _ = resp.Body.Close() }()
			done <- resp.StatusCode
		}()
	}
	select {
	case status := <-done:
		if status != 429 {
			t.Fatalf("queue overflow=%d", status)
		}
	case <-ctx.Done():
		t.Fatal("queue not bounded")
	}
	if calls.Load() != 3 {
		t.Fatalf("duplicate channel bypassed capacity: %d", calls.Load())
	}
	paid, other := configs[0].Clone(), configs[0].Clone()
	paid.OAuthCredential = `{"api_key":"key-a.secret","jwt_token":"jwt-a","user_id":"a","free_first":false}`
	other.AuthType = model.AuthTypeAPIKey
	for _, cfg := range []*model.Config{paid, other} {
		resp, err := callZAIStart(ctx, env.server, cfg, "GLM-5.3-Flash", send)
		if err != nil || resp.StatusCode != 200 || isZAITrialResponse(resp) {
			t.Fatalf("paid/provider isolation: %v %v", resp, err)
		}
		_ = resp.Body.Close()
	}
	_ = held[0].Close()
	_ = held[0].Close()
	for range 3 {
		select {
		case status := <-done:
			if status != 200 {
				t.Fatalf("queued=%d", status)
			}
		case <-ctx.Done():
			t.Fatal("queue failed to wake")
		}
	}
	if calls.Load() != 8 {
		t.Fatalf("real sends=%d want=8", calls.Load())
	}

}

/**
 * @brief 三个独立请求必须在任一响应结束前同时经过完整代理入口到达同账号上游。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartConcurrentRequests(t *testing.T) {
	for _, tc := range []struct {
		name            string
		path            string
		stream          bool
		explicitSession bool
	}{
		{"messages_without_session", "/v1/messages", false, false},
		{"messages_with_session", "/v1/messages", false, true},
		{"streaming_messages", "/v1/messages", true, true},
		{"streaming_responses_threads", "/v1/responses", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, configs := newZAIStartHarness(t)
			// 固定同一真实账号，避免备用账号切换掩盖发送路径是否串行。
			for _, cfg := range configs[1:] {
				cfg.Enabled = false
				if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
					t.Fatal(err)
				}
			}
			env.server.InvalidateChannelListCache()
			control := env.server.client
			arrived := make(chan http.Header, 3)
			release := make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			defer unblock()
			env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(req.URL.Path, "/messages") {
					return control.Do(req)
				}
				arrived <- req.Header.Clone()
				select {
				case <-release:
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
				if tc.stream {
					resp := zaiStartTestResponse(req, http.StatusOK, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"GLM-5.3-Flash\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					resp.Header.Set("Content-Type", "text/event-stream")
					return resp, nil
				}
				return zaiStartTestResponse(req, http.StatusOK, `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3-Flash","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), nil
			})}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			var workers sync.WaitGroup
			defer func() { cancel(); unblock(); workers.Wait() }()
			done := make(chan *httptest.ResponseRecorder, 3)
			// 每个并发请求由 WaitGroup 跟踪，清理时等待全部处理结束。
			for i := range 3 {
				workers.Go(func() {
					body := fmt.Sprintf(`{"model":"GLM-5.3-Flash-Trial","max_tokens":16,"stream":%t,"messages":[{"role":"user","content":"request %d"}]}`, tc.stream, i)
					if tc.path == "/v1/responses" {
						body = fmt.Sprintf(`{"model":"GLM-5.3-Flash-Trial","stream":true,"input":"request %d"}`, i)
					}
					req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body)).WithContext(ctx)
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer test-api-key")
					if tc.explicitSession {
						req.Header.Set("X-Claude-Code-Session-Id", fmt.Sprintf("session-%d", i))
					}
					if tc.path == "/v1/responses" {
						req.Header.Set("Session-Id", "shared-session")
						req.Header.Set("Thread-Id", fmt.Sprintf("thread-%d", i))
					}
					response := httptest.NewRecorder()
					env.engine.ServeHTTP(response, req)
					done <- response
				})
			}
			for i := range 3 {
				select {
				case headers := <-arrived:
					if anthropicHeaderValue(headers, "Authorization") != "Bearer jwt-a" {
						t.Fatal("request used a different account")
					}
				case response := <-done:
					t.Fatalf("request ended before three concurrent upstream sends: %d %s", response.Code, response.Body.String())
				case <-ctx.Done():
					t.Fatalf("only %d requests reached upstream before any response was released", i)
				}
			}
			unblock()
			for range 3 {
				select {
				case response := <-done:
					if response.Code != http.StatusOK {
						t.Errorf("status=%d body=%s", response.Code, response.Body.String())
					}
				case <-ctx.Done():
					t.Fatal("concurrent requests did not finish")
				}
			}
		})
	}
}

/**
 * @brief 备用账号必须等旧账号全部流结束；取消切换后旧账号恢复新准入。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartReserve(t *testing.T) {

	for _, cancelSwitch := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelSwitch), func(t *testing.T) {
			env, configs := newZAIStartHarness(t)
			var held []io.ReadCloser
			send := func(req *http.Request) (*http.Response, error) { return zaiStartTestResponse(req, 200, `{}`), nil }
			for range 2 {
				resp, err := callZAIStart(context.Background(), env.server, configs[0], "GLM-5.3-Flash-Trial", send)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				held = append(held, resp.Body)
				defer func() { _ = resp.Body.Close() }()
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sent := make(chan struct{}, 1)
			done := make(chan error, 1)
			go func() {
				resp, err := callZAIStart(ctx, env.server, configs[1], "GLM-5.3-Flash-Trial", func(req *http.Request) (*http.Response, error) { sent <- struct{}{}; return send(req) })
				if resp != nil {
					_ = resp.Body.Close()
					if resp.StatusCode != 200 {
						err = fmt.Errorf("standby=%d", resp.StatusCode)
					}
				}
				done <- err
			}()
			select {
			case <-sent:
				t.Fatal("standby overlapped active stream")
			case <-time.After(100 * time.Millisecond):
			}
			if cancelSwitch {
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel=%v", err)
				}
				resp, err := callZAIStart(context.Background(), env.server, configs[0], "GLM-5.3-Flash-Trial", send)
				if err != nil || resp.StatusCode != 200 {
					t.Fatalf("canceled drain stranded active: %v %v", resp, err)
				}
				_ = resp.Body.Close()
				return
			}
			_ = held[0].Close()
			select {
			case <-sent:
				t.Fatal("standby started before last stream closed")
			case <-time.After(50 * time.Millisecond):
			}
			_ = held[1].Close()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("drain did not switch")
			}
			select {
			case <-sent:
			default:
				t.Fatal("standby never sent")
			}
		})
	}

}

/**
 * @brief 真实代理选路须跳过备用账号，不向其付费入口发送，也不记录虚假的上游失败。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartReserveRouting(t *testing.T) {
	for _, name := range []string{"GLM-5.3-Flash", "GLM-5.3-Flash-Trial"} {
		t.Run(name, func(t *testing.T) {
			env, configs := newZAIStartHarness(t)
			for _, cfg := range configs[:1] {
				resp, err := callZAIStart(context.Background(), env.server, cfg, "GLM-5.3-Flash-Trial", func(req *http.Request) (*http.Response, error) {
					return zaiStartTestResponse(req, http.StatusOK, `{}`), nil
				})
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
			}
			// 强制普通候选排序先选备用 C，验证发送保护与完整选路/日志链的衔接。
			configs[2].Priority = 100
			if _, err := env.store.UpdateConfig(context.Background(), configs[2].ID, configs[2]); err != nil {
				t.Fatal(err)
			}
			env.server.InvalidateChannelListCache()
			controlClient := env.server.client
			var trial, paid, reserve atomic.Int32
			env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(req.URL.Path, "/messages") {
					return controlClient.Do(req)
				}
				if strings.Contains(req.URL.Path, "/zcode-plan/") {
					trial.Add(1)
				} else {
					paid.Add(1)
				}
				if strings.Contains(anthropicHeaderValue(req.Header, "Authorization"), "jwt-c") || strings.Contains(anthropicHeaderValue(req.Header, "Authorization"), "key-c.") {
					reserve.Add(1)
				}
				return zaiStartTestResponse(req, http.StatusOK, `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3-Flash","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), nil
			})}
			response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": name, "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "hello"}}}, nil)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if reserve.Load() != 0 || paid.Load() != 0 || trial.Load() != 1 {
				t.Errorf("reserve=%d paid=%d trial=%d, want 0/0/1", reserve.Load(), paid.Load(), trial.Load())
			}
			if waitForProxyLogMatching(t, env, func(entry *model.LogEntry) bool { return entry.StatusCode == http.StatusOK }) == nil {
				t.Fatal("missing success log")
			}
			logs, err := env.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 20, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range logs {
				if entry.StatusCode != http.StatusOK || entry.ChannelID == configs[2].ID {
					t.Errorf("reserve selection leaked a proxy log: status=%d message=%s", entry.StatusCode, entry.Message)
				}
			}
		})
	}
}

/**
 * @brief 真实代理排队只唤醒同一活动账号，取消等待不发送也不登记上游故障。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartQueueRouting(t *testing.T) {

	for _, cancelWaiting := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelWaiting), func(t *testing.T) {
			env, configs := newZAIStartHarness(t)
			// 即使备用优先级更高，活动账号满载时仍等待它的空槽。
			configs[2].Priority = 100
			if _, err := env.store.UpdateConfig(context.Background(), configs[2].ID, configs[2]); err != nil {
				t.Fatal(err)
			}
			env.server.InvalidateChannelListCache()

			var held []io.ReadCloser
			for range 3 {
				resp, err := callZAIStart(context.Background(), env.server, configs[0], "GLM-5.3-Flash-Trial", func(req *http.Request) (*http.Response, error) { return zaiStartTestResponse(req, 200, `{}`), nil })
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				held = append(held, resp.Body)
				defer func() { _ = resp.Body.Close() }()
			}
			control := env.server.client
			var inference atomic.Int32
			env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(req.URL.Path, "/messages") {
					return control.Do(req)
				}
				if anthropicHeaderValue(req.Header, "Authorization") != "Bearer jwt-a" {
					t.Error("other account bypassed full active account")
				}
				inference.Add(1)
				return zaiStartTestResponse(req, 200, `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3-Flash","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), nil
			})}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"GLM-5.3-Flash-Trial","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)).WithContext(ctx)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer test-api-key")
				response := httptest.NewRecorder()
				env.engine.ServeHTTP(response, req)
				done <- response
			}()
			select {
			case result := <-done:
				t.Fatalf("queue returned early: %d %s", result.Code, result.Body.String())
			case <-time.After(100 * time.Millisecond):
			}
			if cancelWaiting {
				cancel()
			} else {
				_ = held[0].Close()
			}
			select {
			case result := <-done:
				if cancelWaiting {
					if inference.Load() != 0 {
						t.Fatal("canceled queue sent")
					}
				} else if result.Code != 200 || inference.Load() != 1 {
					t.Fatalf("wake=%d sends=%d %s", result.Code, inference.Load(), result.Body.String())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("queue did not finish")
			}
		})
	}

}

/**
 * @brief 本地并发和 RPM 拒绝不冷却账号或模型，重复渠道可以继续使用。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartBusyAccountRemainsActive(t *testing.T) {

	env, configs := newZAIStartHarness(t)
	for _, localErr := range []error{ErrChannelConcurrencyExceeded, ErrChannelRPMExceeded} {
		resp, err := callZAIStart(context.Background(), env.server, configs[0], "GLM-5.3-Flash-Trial", func(*http.Request) (*http.Response, error) { return nil, localErr })
		if resp != nil {
			_ = resp.Body.Close()
		}
		if !errors.Is(err, localErr) {
			t.Fatalf("local failure=%v", err)
		}
	}
	resp, err := callZAIStart(context.Background(), env.server, configs[3], "GLM-5.3-Flash-Trial", func(req *http.Request) (*http.Response, error) { return zaiStartTestResponse(req, 200, `{}`), nil })
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("local capacity cooled healthy account: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
	if err != nil || len(cooldowns) != 0 {
		t.Fatalf("local failure persisted: %v %v", cooldowns, err)
	}

}

/**
 * @brief Trial 满载只拒绝体验候选，仍可尝试后续付费或普通渠道；真实失败不能回到体验排队。
 * @param t 测试上下文。
 * @note 经真实 HTTP 代理入口验证候选遍历，用响应事件确认队列已满，不读取调度器内部状态。
 */
func TestProxy_ZAIStartCapacityFallback(t *testing.T) {

	for _, ordinary := range []bool{false, true} {
		t.Run(fmt.Sprint(ordinary), func(t *testing.T) {
			env, configs := newZAIStartHarness(t)
			for _, cfg := range configs {
				cfg.Enabled = cfg.ID == configs[0].ID || (cfg.ID == configs[1].ID && !ordinary)
				if cfg.ID == configs[0].ID {
					cfg.Priority = 100
				}
				if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
					t.Fatal(err)
				}
			}
			if ordinary {
				cfg := configs[1].Clone()
				cfg.ID, cfg.Name, cfg.AuthType, cfg.OAuthCredential, cfg.Enabled = 0, "ordinary", model.AuthTypeAPIKey, "", true
				if _, err := env.store.CreateConfigWithAPIKeys(context.Background(), cfg, []*model.APIKey{{APIKey: "ordinary-key"}}); err != nil {
					t.Fatal(err)
				}
			}
			if !ordinary {
				next := `{"api_key":"key-b.secret","jwt_token":"jwt-b","user_id":"b","free_first":false}`
				if ok, err := env.store.CompareAndSwapOAuthCredential(context.Background(), configs[1].ID, configs[1].AuthType, configs[1].OAuthCredential, next); err != nil || !ok {
					t.Fatalf("paid preference: %v %v", ok, err)
				}
			}
			env.server.InvalidateChannelListCache()
			for range 3 {
				resp, err := callZAIStart(context.Background(), env.server, configs[0], "GLM-5.3-Flash-Trial", func(req *http.Request) (*http.Response, error) { return zaiStartTestResponse(req, 200, `{}`), nil })
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
			}
			control := env.server.client
			var trial, paid atomic.Int32
			env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(req.URL.Path, "/messages") {
					return control.Do(req)
				}
				if strings.Contains(req.URL.Path, "/zcode-plan/") {
					trial.Add(1)
				} else {
					paid.Add(1)
				}
				return zaiStartTestResponse(req, 500, `{"type":"error","error":{"type":"api_error","message":"real upstream failure"}}`), nil
			})}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"GLM-5.3-Flash","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer test-api-key")
			result := httptest.NewRecorder()
			env.engine.ServeHTTP(result, req)
			if result.Code != 500 || ctx.Err() != nil || trial.Load() != 0 || paid.Load() != 1 {
				t.Fatalf("fallback lost real failure: status=%d trial=%d paid=%d ctx=%v %s", result.Code, trial.Load(), paid.Load(), ctx.Err(), result.Body.String())
			}
		})
	}

}

/**
 * @brief 付费真实失败不重回此前的体验队列，付费同渠道重试仍保持原有配置。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartPaidFailureDoesNotRequeue(t *testing.T) {
	for _, scenario := range []string{"other_candidate", "same_channel"} {
		t.Run(scenario, func(t *testing.T) {
			env, configs := newZAIStartHarness(t)
			for i, cfg := range configs {
				cfg.Enabled = i == 0 || (scenario == "other_candidate" && i == 1)
				if i == 0 {
					cfg.Priority = 100
					if scenario == "same_channel" {
						cfg.SameChannelRetries = 1
					}
				}

				if i == 1 {
					nextCredential := `{"api_key":"key-b.secret","jwt_token":"jwt-b","user_id":"b","free_first":false}`
					// 普通配置更新不覆盖私有凭证，必须使用凭证 CAS 才能改变付费偏好。
					updated, err := env.store.CompareAndSwapOAuthCredential(context.Background(), cfg.ID, model.AuthTypeZAIOAuth, cfg.OAuthCredential, nextCredential)
					if err != nil || !updated {
						t.Fatalf("set paid preference: updated=%v err=%v", updated, err)
					}
					cfg.OAuthCredential = nextCredential
				}
				if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
					t.Fatal(err)
				}
			}
			env.server.InvalidateChannelListCache()
			held := make(chan *http.Response, 6)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			defer func() {
				for {
					select {
					case resp := <-held:
						_ = resp.Body.Close()
					default:
						return
					}
				}
			}()
			holdAccount := func() error {
				resp, err := callZAIStart(ctx, env.server, configs[0], "GLM-5.3-Flash-Trial", func(req *http.Request) (*http.Response, error) { return zaiStartTestResponse(req, 200, `{}`), nil })
				if err != nil {
					return err
				}
				if resp.StatusCode != 200 {
					_ = resp.Body.Close()
					return fmt.Errorf("hold account: %d", resp.StatusCode)
				}
				held <- resp
				return nil
			}
			if scenario == "other_candidate" {
				for range 3 {
					if err := holdAccount(); err != nil {
						t.Fatal(err)
					}
				}
			}
			controlClient := env.server.client
			var trial, paid atomic.Int32
			env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(req.URL.Path, "/messages") {
					return controlClient.Do(req)
				}
				if strings.Contains(req.URL.Path, "/zcode-plan/") {
					trial.Add(1)
				} else {
					paid.Add(1)
				}
				return zaiStartTestResponse(req, 500, `{"type":"error","error":{"type":"api_error","message":"real upstream failure"}}`), nil
			})}
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"GLM-5.3-Flash","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer test-api-key")
			response := httptest.NewRecorder()
			env.engine.ServeHTTP(response, req)
			if response.Code != 500 || ctx.Err() != nil {
				t.Fatalf("real failure replaced by queue: %d %s ctx=%v", response.Code, response.Body.String(), ctx.Err())
			}
			wantTrial := int32(0)
			if scenario == "same_channel" {
				wantTrial = 1
			}
			wantPaid := int32(1)
			if scenario == "same_channel" {
				wantPaid = 2
			}
			if trial.Load() != wantTrial || paid.Load() != wantPaid {
				t.Fatalf("replayed failure: trial=%d paid=%d", trial.Load(), paid.Load())
			}
		})
	}
}

/**
 * @brief 管理接口中断排队中的当前渠道后，跳过它继续其他候选；无候选时退出并归还容量。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartQueueOperatorAbort(t *testing.T) {
	for _, multiple := range []bool{false, true} {
		t.Run(fmt.Sprint(multiple), func(t *testing.T) {
			env, configs := newZAIStartHarness(t)
			for i, cfg := range configs {
				cfg.Enabled = i == 0 || (multiple && i < 3)
				cfg.Priority = 100 - i
				if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
					t.Fatal(err)
				}
			}
			env.server.InvalidateChannelListCache()
			var held []io.ReadCloser
			for range 3 {
				resp, err := callZAIStart(context.Background(), env.server, configs[0], "GLM-5.3-Flash-Trial", func(req *http.Request) (*http.Response, error) { return zaiStartTestResponse(req, 200, `{}`), nil })
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = resp.Body.Close() }()
				held = append(held, resp.Body)
			}
			admin := gin.New()
			admin.GET("/admin/active-requests", env.server.HandleActiveRequests)
			admin.POST("/admin/active-requests/:request_id/abort", env.server.HandleAbortActiveRequest)
			waitQueued := func(channelID int64) *ActiveRequest {
				t.Helper()
				for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
					response := httptest.NewRecorder()
					admin.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin/active-requests", nil))
					var body activeRequestsResponse
					if json.Unmarshal(response.Body.Bytes(), &body) != nil || !body.Success {
						t.Fatal("invalid active request response")
					}
					for _, active := range body.Data {
						if active.ChannelID == channelID && active.Abortable && active.BaseURL == "" {
							return active
						}
					}
					time.Sleep(10 * time.Millisecond)
				}
				t.Fatal("active request did not identify the queued channel")
				return nil
			}
			controlClient := env.server.client
			sent := make(chan string, 4)
			env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(req.URL.Path, "/messages") {
					return controlClient.Do(req)
				}
				sent <- anthropicHeaderValue(req.Header, "Authorization")
				return zaiStartTestResponse(req, 200, `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3-Flash","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), nil
			})}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan *httptest.ResponseRecorder, 1)
			launch := func() {
				go func() {
					req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"GLM-5.3-Flash-Trial","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)).WithContext(ctx)
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer test-api-key")
					response := httptest.NewRecorder()
					env.engine.ServeHTTP(response, req)
					done <- response
				}()
			}
			result := func(want int) {
				t.Helper()
				select {
				case response := <-done:
					if response.Code != want {
						t.Fatalf("status=%d want=%d: %s", response.Code, want, response.Body.String())
					}
				case <-time.After(3 * time.Second):
					t.Fatal("operator abort did not unblock request")
				}
			}
			launch()
			active := waitQueued(configs[0].ID)
			response := httptest.NewRecorder()
			admin.ServeHTTP(response, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/active-requests/%d/abort", active.ID), nil))
			var body struct {
				Success bool `json:"success"`
				Data    struct {
					Aborted bool `json:"aborted"`
				} `json:"data"`
			}
			if json.Unmarshal(response.Body.Bytes(), &body) != nil || response.Code != 200 || !body.Success || !body.Data.Aborted {
				t.Fatalf("abort response: %s", response.Body.String())
			}
			wantAccount := "Bearer jwt-a"
			if multiple {
				if next := waitQueued(configs[1].ID); next.ID != active.ID {
					t.Error("waiting request identity changed")
				}
				for _, body := range held {
					_ = body.Close()
				}
				result(200)
				wantAccount = "Bearer jwt-b"
			} else {
				result(502)
				for _, body := range held {
					_ = body.Close()
				}
				launch()
				result(200)
			}
			select {
			case account := <-sent:
				if account != wantAccount {
					t.Fatalf("sent to skipped/reserve account: %s", account)
				}
			default:
				t.Fatal("expected upstream was not sent")
			}
			select {
			case account := <-sent:
				t.Fatalf("extra upstream send: %s", account)
			default:
			}
			cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
			if err != nil || len(cooldowns) != 0 {
				t.Fatalf("operator abort wrote cooldowns: %v %v", cooldowns, err)
			}
		})
	}
}

/**
 * @brief 首次繁忙只发送一次并立即冷却同账号模型，后续本地拒绝不续期。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartBusyWindow(t *testing.T) {

	env, configs := newZAIStartHarness(t)
	var sends atomic.Int32
	control := env.server.client
	env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(req.URL.Path, "/messages") {
			return control.Do(req)
		}
		sends.Add(1)
		return zaiStartTestResponse(req, 429, `{"code":3008,"msg":"concurrency limit exceeded"}`), nil
	})}
	request := func() *httptest.ResponseRecorder {
		return doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": "GLM-5.3-Flash-Trial", "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "hello"}}}, map[string]string{"X-CCLoad-Channel": configs[0].Name})
	}
	if result := request(); result.Code != 429 {
		t.Fatalf("busy=%d %s", result.Code, result.Body.String())
	}
	first, err := env.store.GetAllModelCooldowns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	until := first[configs[0].ID]["GLM-5.3-Flash-Trial"]
	if !until.After(time.Now()) || !first[configs[3].ID]["GLM-5.3-Flash-Trial"].Equal(until) {
		t.Fatalf("missing shared cooldown: %v", first)
	}
	_ = request()
	next, err := env.store.GetAllModelCooldowns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sends.Load() != 3 || !next[configs[0].ID]["GLM-5.3-Flash-Trial"].Equal(until) {
		t.Fatalf("busy retried or extended: sends=%d %v", sends.Load(), next)
	}

}

/**
 * @brief 仅 405 HTML/Aliyun 封禁触发全局暂停，普通 405 不扩大影响范围。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartEdgeHold(t *testing.T) {
	for _, tc := range []struct {
		name  string ///< 响应特征。
		body  string ///< 上游 405 原始响应。
		group bool   ///< 是否应阻止其他体验账号使用同一出口。
	}{
		{"ordinary", `{"error":{"message":"method not allowed"}}`, false},
		{"html", `<html>request blocked</html>`, true},
		{"aliyun", `Access denied: https://errors.aliyun.com/`, true},
		{"html_aliyun", `<html>https://errors.aliyun.com/</html>`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, configs := newZAIStartHarness(t)
			control := env.server.client
			var trial, paid atomic.Int32
			env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(req.URL.Path, "/messages") {
					return control.Do(req)
				}
				if !strings.Contains(req.URL.Path, "/zcode-plan/") {
					paid.Add(1)
					return zaiStartTestResponse(req, 200, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), nil
				}
				trial.Add(1)
				return zaiStartTestResponse(req, 405, tc.body), nil
			})}
			request := func(index int, name string) *httptest.ResponseRecorder {
				return doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": name, "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "hello"}}}, map[string]string{"X-CCLoad-Channel": configs[index].Name})
			}
			_ = request(0, "GLM-5.3-Flash-Trial")
			_ = request(1, "GLM-5.3-Flash-Trial")
			want := int32(2)
			if tc.group {
				want = 1
			}
			if trial.Load() != want {
				t.Fatalf("group scope: calls=%d want=%d", trial.Load(), want)
			}
			if result := request(0, "GLM-5.3-Flash"); result.Code != 200 || paid.Load() != 1 {
				t.Fatalf("paid blocked: %d %s", result.Code, result.Body.String())
			}
			channels, err := env.store.GetAllChannelCooldowns(context.Background())
			if err != nil || len(channels) != 0 {
				t.Fatalf("Trial polluted paid channel: %v %v", channels, err)
			}
		})
	}

}

/**
 * @brief 取消三个在途流并重复关闭响应体后，全部并发名额可重新使用。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartCancellation(t *testing.T) {

	env, configs := newZAIStartHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	send := func(req *http.Request) (*http.Response, error) { return zaiStartTestResponse(req, 200, `{}`), nil }
	var held []io.ReadCloser
	for range 3 {
		resp, err := callZAIStart(ctx, env.server, configs[0], "GLM-5.3-Flash-Trial", send)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		held = append(held, resp.Body)
	}
	cancel()
	for _, body := range held {
		_ = body.Close()
		_ = body.Close()
	}
	for range 3 {
		resp, err := callZAIStart(context.Background(), env.server, configs[3], "GLM-5.3-Flash-Trial", send)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("capacity leaked: %v %v", resp, err)
		}
		defer func() { _ = resp.Body.Close() }()
	}

}

/**
 * @brief 明确首帧 busy 可短重试，坏信封及已开始的流不能重放。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartRetryBoundary(t *testing.T) {
	for _, scenario := range []string{"sse_first", "sse_started", "malformed", "unrelated_field"} {
		t.Run(scenario, func(t *testing.T) {
			env, configs := newZAIStartHarness(t)
			now := time.Now()
			env.server.zaiStart.now = func() time.Time { return now }
			calls := 0
			send := func(req *http.Request) (*http.Response, error) {
				calls++
				if calls%2 == 0 {
					return zaiStartTestResponse(req, 200, `{}`), nil
				}
				switch scenario {
				case "sse_first", "sse_started":
					body := "event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":3008,\"message\":\"busy\"}}\n\n"
					if scenario == "sse_started" {
						body = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\"}}\n\n" + body
					}
					resp := zaiStartTestResponse(req, 200, body)
					resp.Header.Set("Content-Type", "text/event-stream")
					return resp, nil
				case "malformed":
					return zaiStartTestResponse(req, 429, `{"code":3008,"msg":"concurrency limit exceeded"`), nil
				case "unrelated_field":
					return zaiStartTestResponse(req, 429, `{"code":1302,"msg":"limited","example":"concurrency limit exceeded"}`), nil
				default:
					return zaiStartTestResponse(req, 429, `{"code":3009,"msg":"busy"}`), nil
				}
			}
			for range 1 {
				resp, err := callZAIStart(context.Background(), env.server, configs[0], "GLM-5.3-Flash-Trial", send)
				if err != nil {
					t.Fatal(err)
				}
				if isZAILocalPlanResponse(resp) {
					t.Fatal("unexpected local pause")
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				now = now.Add(61 * time.Second)
			}
			want := 1
			if scenario == "sse_first" {
				want = 2
			}
			if calls != want {
				t.Fatalf("calls=%d want=%d", calls, want)
			}
		})
	}
}

/**
 * @brief 已冷却或禁用的活动账号，以及尚未发出/最终失败的账号，不能挡住备用账号。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartReserveRecovery(t *testing.T) {
	for _, scenario := range []string{"channel_cooldown", "disabled", "rpm", "server_error", "network_error"} {
		t.Run(scenario, func(t *testing.T) {
			env, configs := newZAIStartHarness(t)
			send := func(req *http.Request) (*http.Response, error) { return zaiStartTestResponse(req, 200, `{}`), nil }
			for _, cfg := range configs[:1] {
				resp, err := callZAIStart(context.Background(), env.server, cfg, "GLM-5.3-Flash-Trial", send)
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
			}
			switch scenario {
			case "channel_cooldown":
				// 同账号两个渠道都冷却；只失效冷却缓存，刻意保留旧渠道列表。
				for _, index := range []int{0, 3} {
					if err := env.store.SetChannelCooldown(context.Background(), configs[index].ID, time.Now().Add(time.Minute)); err != nil {
						t.Fatal(err)
					}
					env.server.invalidateChannelRelatedCache(configs[index].ID)
				}
			case "disabled":
				for _, index := range []int{0, 3} {
					cfg := configs[index].Clone()
					cfg.Enabled = false
					if _, err := env.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
						t.Fatal(err)
					}
				}
				env.server.InvalidateChannelListCache()
			default:
				resp, err := callZAIStart(context.Background(), env.server, configs[0], "GLM-5.3-Flash-Trial", func(req *http.Request) (*http.Response, error) {
					if scenario == "rpm" {
						return nil, ErrChannelRPMExceeded
					}
					if scenario == "network_error" {
						return nil, io.ErrUnexpectedEOF
					}
					return zaiStartTestResponse(req, 503, `{"error":{"message":"unavailable"}}`), nil
				})
				if scenario == "rpm" && !errors.Is(err, ErrChannelRPMExceeded) {
					t.Fatalf("rpm error=%v", err)
				}
				if scenario == "network_error" && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatalf("network error=%v", err)
				}
				if resp != nil {
					_ = resp.Body.Close()
				}
			}
			reserveCalls := 0
			resp, err := callZAIStart(context.Background(), env.server, configs[2], "GLM-5.3-Flash-Trial", func(req *http.Request) (*http.Response, error) { reserveCalls++; return send(req) })
			want := 200

			if err != nil || resp.StatusCode != want {
				t.Fatalf("reserve state: %v %v calls=%d want=%d", resp, err, reserveCalls, want)
			}
			_ = resp.Body.Close()
		})
	}
}

/**
 * @brief 已确认的体验资格失效须让备用账号接替；查询失败保留活动席位，在途流关闭前不晋升。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartPlanEligibility(t *testing.T) {
	for _, refreshOnly := range []bool{false, true} {
		for _, state := range []string{"exhausted", "expired", "unknown", "other_model"} {
			t.Run(fmt.Sprintf("%s/refresh_only=%t", state, refreshOnly), func(t *testing.T) {
				env, configs := newZAIStartHarness(t)
				send := func(req *http.Request) (*http.Response, error) {
					return zaiStartTestResponse(req, 200, `{}`), nil
				}
				first, err := callZAIStart(context.Background(), env.server, configs[0], "GLM-5.3-Flash", send)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = first.Body.Close() }()
				if first.StatusCode != 200 || !isZAITrialResponse(first) {
					t.Fatal("initial account must use Trial")
				}
				// 重复渠道保留旧的有效采样，单个渠道刷新也必须更新真实账号的资格判断。
				duplicateCredential, err := zaiauth.ParseCredential([]byte(configs[3].OAuthCredential))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := env.server.zaiAccountPlans(context.Background(), configs[3], duplicateCredential, false); err != nil {
					t.Fatal(err)
				}
				name := "GLM-5.3-Flash"
				if state == "other_model" {
					name = "GLM-5.3"
				}
				balance := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					remaining, status := 10, "active"
					if r.Header.Get("Authorization") == "Bearer jwt-a" {
						switch state {
						case "exhausted":
							remaining = 0
						case "expired":
							status = "expired"
						case "unknown":
							w.WriteHeader(http.StatusServiceUnavailable)
							_, _ = io.WriteString(w, `{}`)
							return
						}
					}
					giftModel := "glm-5.3-flash"
					if r.Header.Get("Authorization") == "Bearer jwt-c" {
						giftModel = name
					}
					_, _ = fmt.Fprintf(w, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":%q}],"balances":[{"plan_id":"gift","capabilities":[%q],"remaining_units":%d}]}}`, status, "model:"+giftModel, remaining)
				}))
				env.server.zaiService.OAuthBaseURL = balance.URL + "/api/v1"
				if refreshOnly {
					// 模拟仅刷新 A 的一个渠道，不发新的 A 推理请求。
					credential, parseErr := zaiauth.ParseCredential([]byte(configs[0].OAuthCredential))
					if parseErr != nil {
						t.Fatal(parseErr)
					}
					if _, err := env.server.zaiAccountPlans(context.Background(), configs[0], credential, true); err != nil {
						t.Fatal(err)
					}
				} else {
					// 模拟套餐缓存过期后的普通请求，仍有付费资格的 A 应直接走付费。
					env.server.invalidateZAIPlans(configs[0].ID)
					paid, err := callZAIStart(context.Background(), env.server, configs[0], name, send)
					if err != nil {
						t.Fatal(err)
					}
					_ = paid.Body.Close()
					if paid.StatusCode != 200 || isZAITrialResponse(paid) {
						t.Fatal("unavailable Trial should retain paid routing")
					}
				}
				checkReserve := func(want int) {
					t.Helper()
					ctx := context.Background()
					if want == 429 {
						var cancel context.CancelFunc
						ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
						defer cancel()
					}
					resp, err := callZAIStart(ctx, env.server, configs[2], name+"-Trial", send)
					if want == 429 {
						if !errors.Is(err, context.DeadlineExceeded) {
							t.Fatalf("standby overlapped old stream: %v %v", resp, err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = resp.Body.Close() }()
					if resp.StatusCode != want {
						raw, _ := io.ReadAll(resp.Body)
						t.Fatalf("reserve status=%d, want %d: %s", resp.StatusCode, want, raw)
					}
					if want == 429 {
						var payload struct {
							Error struct {
								Type string `json:"type"`
							} `json:"error"`
						}
						if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
							t.Fatal(err)
						}
						if payload.Error.Type != "zcode_trial_reserve" {
							t.Fatalf("unexpected rejection: %s", payload.Error.Type)
						}
					}
				}
				checkReserve(429)
				_ = first.Body.Close()
				want := http.StatusOK

				checkReserve(want)
			})
		}
	}
}

/**
 * @brief 重复渠道的新采样确认体验恢复后，旧耗尽缓存不得再次退役整个账号。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartPlanRecovery(t *testing.T) {
	env, configs := newZAIStartHarness(t)
	var restored atomic.Bool
	balance := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remaining := 10
		if r.Header.Get("Authorization") == "Bearer jwt-a" && !restored.Load() {
			remaining = 0
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":"active"}],"balances":[{"plan_id":"gift","capabilities":["model:glm-5.3-flash"],"remaining_units":%d}]}}`, remaining)
	}))
	env.server.zaiService.OAuthBaseURL = balance.URL + "/api/v1"
	check := func(index int, name string, status int, trial bool) {
		t.Helper()
		resp, err := callZAIStart(context.Background(), env.server, configs[index], name, func(req *http.Request) (*http.Response, error) {
			return zaiStartTestResponse(req, http.StatusOK, `{}`), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != status || isZAITrialResponse(resp) != trial {
			t.Fatalf("channel %d: status=%d trial=%t, want status=%d trial=%t", index, resp.StatusCode, isZAITrialResponse(resp), status, trial)
		}
		if status == http.StatusTooManyRequests {
			var payload struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if payload.Error.Type != "zcode_trial_reserve" {
				t.Fatalf("unexpected rejection: %s", payload.Error.Type)
			}
		}
	}
	check(0, "GLM-5.3-Flash", 200, false)
	restored.Store(true)
	check(3, "GLM-5.3-Flash-Trial", 200, true)
	// A 的旧渠道依然按缓存走付费，但不能移除已由新渠道确认恢复的活动账号。
	check(0, "GLM-5.3-Flash", 200, false)
	check(3, "GLM-5.3-Flash-Trial", 200, true)
}

/**
 * @brief 重复渠道的旧可用缓存不得重新准入耗尽账号；自动模式回付费，显式 Trial 本地拒绝。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartStaleEligibility(t *testing.T) {
	for _, forced := range []bool{false, true} {
		for _, source := range []string{"active", "streaming", "cooled", "disabled"} {
			t.Run(fmt.Sprintf("forced=%t/source=%s", forced, source), func(t *testing.T) {
				keepStream := source == "streaming"
				env, configs := newZAIStartHarness(t)
				send := func(req *http.Request) (*http.Response, error) {
					return zaiStartTestResponse(req, 200, `{}`), nil
				}
				var active *http.Response
				for _, index := range []int{3} {
					resp, err := callZAIStart(context.Background(), env.server, configs[index], "GLM-5.3-Flash-Trial", send)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = resp.Body.Close() }()
					if resp.StatusCode != http.StatusOK {
						t.Fatalf("warmup status=%d", resp.StatusCode)
					}
					if index == 3 && keepStream {
						active = resp
					} else {
						_ = resp.Body.Close()
					}
				}
				balance := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					remaining := 10
					if r.Header.Get("Authorization") == "Bearer jwt-a" {
						remaining = 0
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":"active"}],"balances":[{"plan_id":"gift","capabilities":["model:glm-5.3-flash"],"remaining_units":%d}]}}`, remaining)
				}))
				env.server.zaiService.OAuthBaseURL = balance.URL + "/api/v1"
				credential, err := zaiauth.ParseCredential([]byte(configs[0].OAuthCredential))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := env.server.zaiAccountPlans(context.Background(), configs[0], credential, true); err != nil {
					t.Fatal(err)
				}
				// 渠道退出候选不代表它刚取得的账号余额失真，重复渠道仍应遵守最新事实。
				switch source {
				case "cooled":
					if err := env.store.SetModelCooldown(context.Background(), configs[0].ID, "GLM-5.3-Flash-Trial", time.Now().Add(time.Minute)); err != nil {
						t.Fatal(err)
					}
					env.server.invalidateChannelRelatedCache(configs[0].ID)
				case "disabled":
					configs[0].Enabled = false
					if _, err := env.store.UpdateConfig(context.Background(), configs[0].ID, configs[0]); err != nil {
						t.Fatal(err)
					}
					env.server.InvalidateChannelListCache()
				}
				name, wantStatus, wantCalls := "GLM-5.3-Flash", http.StatusOK, 1
				if forced {
					name, wantStatus, wantCalls = name+"-Trial", http.StatusTooManyRequests, 0
				}
				calls := 0
				resp, err := callZAIStart(context.Background(), env.server, configs[3], name, func(req *http.Request) (*http.Response, error) {
					calls++
					if strings.Contains(req.URL.Path, "/zcode-plan/") {
						t.Error("stale balance sent a Trial request for an exhausted account")
					}
					return send(req)
				})
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
				if resp.StatusCode != wantStatus || calls != wantCalls {
					t.Fatalf("status=%d calls=%d, want status=%d calls=%d", resp.StatusCode, calls, wantStatus, wantCalls)
				}
				checkReserve := func(want int) {
					t.Helper()
					ctx := context.Background()
					if want == 429 {
						var cancel context.CancelFunc
						ctx, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
						defer cancel()
					}
					resp, err := callZAIStart(ctx, env.server, configs[2], "GLM-5.3-Flash-Trial", send)
					if want == 429 {
						if !errors.Is(err, context.DeadlineExceeded) {
							t.Fatalf("standby overlapped: %v %v", resp, err)
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					_ = resp.Body.Close()
					if resp.StatusCode != want {
						t.Fatalf("reserve status=%d, want %d", resp.StatusCode, want)
					}
				}
				if active != nil {
					checkReserve(http.StatusTooManyRequests)
					_ = active.Body.Close()
				}
				checkReserve(http.StatusOK)
			})
		}
	}
}

/**
 * @brief 单个 Trial 模型冷却只拒绝该模型，不能退役仍可服务其他模型的活动账号。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartModelCooldownKeepsAccount(t *testing.T) {

	env, configs := newZAIStartHarness(t)
	balance := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":"active"}],"balances":[{"plan_id":"gift","bucket_id":"flash","capabilities":["model:glm-5.3-flash"],"remaining_units":10},{"plan_id":"gift","bucket_id":"other","capabilities":["model:glm-5.3"],"remaining_units":10}]}}`)
	}))
	env.server.zaiService.OAuthBaseURL = balance.URL + "/api/v1"
	resp, err := callZAIStart(context.Background(), env.server, configs[0], "GLM-5.3-Flash", func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/zcode-plan/") {
			return zaiStartTestResponse(req, 429, `{"code":1113,"msg":"quota exhausted"}`), nil
		}
		return zaiStartTestResponse(req, 200, `{}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	var sends atomic.Int32
	send := func(req *http.Request) (*http.Response, error) {
		sends.Add(1)
		return zaiStartTestResponse(req, 200, `{}`), nil
	}
	resp, err = callZAIStart(context.Background(), env.server, configs[3], "GLM-5.3-Flash-Trial", send)
	if err != nil || resp.StatusCode != 429 || sends.Load() != 0 {
		t.Fatalf("duplicate bypassed model cooling: %v %v sends=%d", resp, err, sends.Load())
	}
	_ = resp.Body.Close()
	resp, err = callZAIStart(context.Background(), env.server, configs[3], "GLM-5.3-Trial", send)
	if err != nil || resp.StatusCode != 200 || sends.Load() != 1 {
		t.Fatalf("other model retired: %v %v sends=%d", resp, err, sends.Load())
	}
	_ = resp.Body.Close()

}

/**
 * @brief 管理检测与独立发送可指定已停用渠道，其 Trial 准入只看账号余额资格。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartStandaloneIgnoresRoutingState(t *testing.T) {
	env, configs := newZAIStartHarness(t)
	configs[1].Enabled = false
	if _, err := env.store.UpdateConfig(context.Background(), configs[1].ID, configs[1]); err != nil {
		t.Fatal(err)
	}
	env.server.InvalidateChannelListCache()
	resp, err := callZAIStart(context.Background(), env.server, configs[1], "GLM-5.3-Flash-Trial", func(req *http.Request) (*http.Response, error) {
		return zaiStartTestResponse(req, http.StatusOK, `{}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || !isZAITrialResponse(resp) {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("disabled channel test rejected locally: status=%d body=%s", resp.StatusCode, raw)
	}
}

/**
 * @brief 真实额度拒绝按账号共享 Trial 冷却，重复渠道不能绕过，付费与其他账号仍可发送。
 * @param t 测试上下文。
 */
func TestProxy_ZAITrialQuotaAcrossDuplicateChannels(t *testing.T) {
	env, configs := newZAIStartHarness(t)
	send := func(req *http.Request) (*http.Response, error) { return zaiStartTestResponse(req, 200, `{}`), nil }
	warm, err := callZAIStart(context.Background(), env.server, configs[3], "GLM-5.3-Flash-Trial", send)
	if err != nil {
		t.Fatal(err)
	}
	_ = warm.Body.Close()
	calls := 0
	resp, err := callZAIStart(context.Background(), env.server, configs[0], "GLM-5.3-Flash", func(req *http.Request) (*http.Response, error) {
		calls++
		if strings.Contains(req.URL.Path, "/zcode-plan/") {
			return zaiStartTestResponse(req, 429, `{"code":1113,"msg":"quota exhausted"}`), nil
		}
		return send(req)
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if calls != 2 || resp.StatusCode != 200 || isZAITrialResponse(resp) {
		t.Fatal("quota rejection did not fall back to paid")
	}
	for _, tc := range []struct {
		index  int
		name   string
		status int
		trial  bool
		calls  int
	}{
		{3, "GLM-5.3-Flash-Trial", 429, false, 0},
		{3, "GLM-5.3-Flash", 200, false, 1},
		{1, "GLM-5.3-Flash-Trial", 200, true, 1},
	} {
		calls = 0
		resp, err := callZAIStart(context.Background(), env.server, configs[tc.index], tc.name, func(req *http.Request) (*http.Response, error) {
			calls++
			if strings.Contains(req.URL.Path, "/zcode-plan/") != tc.trial {
				t.Error("duplicate cooldown crossed the paid/account boundary")
			}
			return send(req)
		})
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.status || calls != tc.calls {
			t.Errorf("channel=%d model=%s status=%d calls=%d", tc.index, tc.name, resp.StatusCode, calls)
		}
	}
	cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{0, 3} {
		if len(cooldowns[configs[index].ID]) != 1 || !cooldowns[configs[index].ID]["GLM-5.3-Flash-Trial"].After(time.Now()) {
			t.Errorf("missing isolated Trial cooldown: channel=%d", index)
		}
	}
}

/**
 * @brief 在途旧账号额度拒绝不能冷却已重新授权的新账号，但仍应保护旧账号的重复渠道。
 * @param t 测试上下文。
 */
func TestProxy_ZAITrialQuotaAfterAccountReplacement(t *testing.T) {
	env, configs := newZAIStartHarness(t)
	resp, err := callZAIStart(context.Background(), env.server, configs[0], "GLM-5.3-Flash", func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/zcode-plan/") {
			updated, err := env.store.CompareAndSwapOAuthCredential(context.Background(), configs[0].ID, model.AuthTypeZAIOAuth, configs[0].OAuthCredential,
				`{"api_key":"key-new.secret","jwt_token":"jwt-new","user_id":"new"}`)
			if err != nil || !updated {
				t.Errorf("replace account: %t %v", updated, err)
			}
			return zaiStartTestResponse(req, 429, `{"code":1113,"msg":"quota exhausted"}`), nil
		}
		return zaiStartTestResponse(req, 200, `{}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(cooldowns[configs[0].ID]) != 0 || !cooldowns[configs[3].ID]["GLM-5.3-Flash-Trial"].After(time.Now()) {
		t.Fatalf("old failure affected new account or missed duplicate: %v", cooldowns)
	}
}

/**
 * @brief 慢付费查询不得把较早的耗尽余额排在较新的恢复余额之后。
 * @param t 测试上下文。
 */
func TestProxy_ZAIStartBalanceObservationOrder(t *testing.T) {
	env, configs := newZAIStartHarness(t)
	var restored, balanceReturned atomic.Bool
	var subscriptions atomic.Int32
	firstBalance := make(chan struct{})
	firstSubscription := make(chan struct{})
	releaseSubscription := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(releaseSubscription) })
	defer unblock()
	balanceSignal := sync.OnceFunc(func() { close(firstBalance) })
	// 从余额接收后的时间观察点同步，不把 HTTP handler 返回当作客户端已经收到余额。
	env.server.zaiService.Now = func() time.Time {
		if balanceReturned.Load() {
			balanceSignal()
		}
		return time.Now()
	}
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/biz/subscription/list" {
			if r.Header.Get("Authorization") == "key-a.secret" && subscriptions.Add(1) == 1 {
				close(firstSubscription)
				select {
				case <-releaseSubscription:
				case <-r.Context().Done():
					return
				}
			}
			_, _ = io.WriteString(w, `{"code":0,"data":[{"status":"VALID"}]}`)
			return
		}
		remaining := 10
		if r.Header.Get("Authorization") == "Bearer jwt-a" && !restored.Load() {
			remaining = 0
			defer balanceReturned.Store(true)
		}
		_, _ = fmt.Fprintf(w, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":"active"}],"balances":[{"plan_id":"gift","capabilities":["model:glm-5.3-flash"],"remaining_units":%d}]}}`, remaining)
	}))
	env.server.zaiService.OAuthBaseURL = upstream.URL + "/api/v1"
	env.server.zaiService.SubscriptionBaseURL = upstream.URL
	credential, err := zaiauth.ParseCredential([]byte(configs[0].OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := env.server.zaiAccountPlans(context.Background(), configs[0], credential, true)
		done <- err
	}()
	select {
	case <-firstSubscription:
	case <-time.After(3 * time.Second):
		t.Fatal("subscription did not start")
	}
	select {
	case <-firstBalance:
	case <-time.After(3 * time.Second):
		t.Fatal("balance was not received")
	}
	restored.Store(true)
	send := func(req *http.Request) (*http.Response, error) {
		return zaiStartTestResponse(req, http.StatusOK, `{}`), nil
	}
	for _, index := range []int{3} {
		resp, err := callZAIStart(context.Background(), env.server, configs[index], "GLM-5.3-Flash-Trial", send)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("initial account %d status=%d", index, resp.StatusCode)
		}
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old query did not finish")
	}
	resp, err := callZAIStart(context.Background(), env.server, configs[3], "GLM-5.3-Flash-Trial", send)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("old exhausted balance displaced recovered account; reserve status=%d", resp.StatusCode)
	}
}

/**
 * @brief 体验错误公开返回调用方消息位置，覆盖套餐选路、JSON 和 SSE，并保留付费错误。
 * @param t 测试上下文。
 */
func TestProxy_ZAIErrorMessagePositions(t *testing.T) {
	for _, scenario := range []string{"preferred", "forced", "only_trial", "paid", "json_200", "sse_first", "sse_late", "injected", "brackets", "prose", "large", "invalid_json"} {
		t.Run(scenario, func(t *testing.T) {
			param := "messages.1.content.0.source.base64.data"
			wantParam := "messages.0.content.0.source.base64.data"
			if scenario == "paid" {
				wantParam = param
			}
			if scenario == "injected" {
				param, wantParam = "messages.0.content.0", "messages.injected_date.content.0"
			}
			if scenario == "brackets" {
				param = "messages[1].content[0].source.base64.data"
			}
			message := param + ": Could not process image"
			wantMessage := wantParam + ": Could not process image"
			if scenario == "prose" {
				message = "messages.1 has an image: Could not process image"
				wantMessage = message
			}
			payload := map[string]any{"type": "error", "error": map[string]string{"type": "invalid_request_error", "param": param, "message": message}}
			if scenario == "large" {
				payload["details"] = strings.Repeat("x", 70*1024)
			}
			errorBody, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "invalid_json" {
				errorBody = errorBody[:len(errorBody)-1]
			}
			var trialCalls, paidCalls atomic.Int32
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/biz/subscription/list":
					_, _ = io.WriteString(w, `{"code":0,"data":[{"status":"VALID"}]}`)
					return
				case "/api/v1/zcode-plan/billing/balance":
					_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"trial","status":"active"}],"balances":[{"plan_id":"trial","show_name":"GLM-5.3","remaining_units":10}]}}`)
					return
				case "/api/v1/zcode-plan/anthropic/v1/messages":
					trialCalls.Add(1)
				case "/v1/messages":
					paidCalls.Add(1)
				default:
					t.Errorf("unexpected endpoint: %s", r.URL.Path)
				}
				if strings.HasPrefix(scenario, "sse_") {
					w.Header().Set("Content-Type", "text/event-stream")
					if scenario == "sse_late" {
						_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"GLM-5.3\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
					}
					_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", errorBody)
					return
				}
				if scenario != "json_200" {
					w.WriteHeader(http.StatusBadRequest)
				}
				_, _ = w.Write(errorBody)
			}))
			credential := &zaiauth.Credential{APIKey: "key.secret", JWTToken: "jwt"}
			name := "GLM-5.3"
			if scenario == "forced" || scenario == "invalid_json" || scenario == "sse_first" || scenario == "large" {
				name += "-Trial"
			}
			if scenario == "only_trial" {
				credential.APIKey = ""
			}
			if scenario == "paid" {
				credential.JWTToken = ""
			}
			raw, err := credential.JSON()
			if err != nil {
				t.Fatal(err)
			}
			env := setupProxyTestEnv(t, []testChannel{{name: "error positions", upstreamProtocol: "anthropic", models: name, authType: model.AuthTypeZAIOAuth, oauthCredential: raw}}, map[int]string{0: upstream.URL})
			env.server.zaiService.OAuthBaseURL = upstream.URL + "/api/v1"
			env.server.zaiService.BizBaseURL = upstream.URL
			response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": name, "max_tokens": 16, "stream": strings.HasPrefix(scenario, "sse_"), "messages": []any{map[string]string{"role": "user", "content": "hello"}}}, nil)
			wantStatus := http.StatusBadRequest
			if scenario == "sse_first" {
				wantStatus = http.StatusBadGateway
			}
			if scenario == "sse_late" {
				wantStatus = http.StatusOK
			}
			if response.Code != wantStatus {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			wantPaid := int32(0)
			if scenario == "paid" {
				wantPaid = 1
			}
			if paidCalls.Load() != wantPaid || trialCalls.Load() != 1-wantPaid {
				t.Fatalf("paid=%d trial=%d", paidCalls.Load(), trialCalls.Load())
			}
			body := response.Body.Bytes()
			if scenario == "invalid_json" {
				if string(body) != string(errorBody) {
					t.Fatalf("invalid JSON changed: %s", body)
				}
				return
			}
			if scenario == "sse_late" {
				var errorEvent []byte
				for len(body) > 0 {
					end := firstSSEEventEnd(body)
					if end < 0 {
						t.Fatalf("incomplete SSE frame: %s", body)
					}
					event, data := parseSSEEventChunk(body[:end])
					if event == "error" {
						errorEvent = data
					}
					body = body[end:]
				}
				body = errorEvent
			}
			if !json.Valid(body) || gjson.GetBytes(body, "error.param").String() != wantParam || gjson.GetBytes(body, "error.message").String() != wantMessage {
				t.Fatalf("incorrect error position: %s", body)
			}
		})
	}
}

/**
 * @brief 旧请求在选项保存后加载凭证，不能改变后续请求实际使用的套餐。
 * @param t 测试上下文。
 */
func TestProxy_ZAIOptionsAfterStaleRequest(t *testing.T) {
	for _, site := range []zaiauth.Site{zaiauth.SiteZAI, zaiauth.SiteBigModel} {
		for _, freeFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/free_first=%t", site, freeFirst), func(t *testing.T) {
				previous := !freeFirst
				raw, err := (&zaiauth.Credential{Site: site, APIKey: "key.secret", JWTToken: "jwt", FreeFirst: &previous}).JSON()
				if err != nil {
					t.Fatal(err)
				}
				env := setupProxyTestEnv(t, []testChannel{{name: "option race", authType: model.AuthTypeZAIOAuth, oauthCredential: raw, models: "GLM-5.3", upstreamProtocol: "anthropic"}}, map[int]string{0: "https://paid.test"})
				ctx := context.Background()
				configs, err := env.store.ListConfigs(ctx)
				if err != nil || len(configs) != 1 {
					t.Fatalf("load channel: %v", err)
				}
				staleCfg := configs[0].Clone()
				path := fmt.Sprintf("/admin/channels/%d/zai-options", staleCfg.ID)
				c, response := newTestContext(t, newRequest(http.MethodPatch, path, strings.NewReader(fmt.Sprintf(`{"free_first":%t}`, freeFirst))))
				c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(staleCfg.ID)}}
				env.server.HandleZAIOptions(c)
				if response.Code != http.StatusOK {
					t.Fatalf("PATCH status=%d body=%s", response.Code, response.Body.String())
				}
				// 恢复一个保存前已选中渠道的请求，构造清缓存之后的旧快照回填。
				if _, err := env.server.zaiCredentials.credential(ctx, staleCfg, false); err != nil {
					t.Fatal(err)
				}
				var paid, trial atomic.Int32
				env.server.client = &http.Client{Transport: oauthUsageRoundTripper(func(r *http.Request) (*http.Response, error) {
					switch r.URL.Path {
					case "/api/biz/subscription/list":
						return jsonResponse(r, `{"code":0,"data":[{"status":"VALID"}]}`)
					case "/api/v1/zcode-plan/billing/balance":
						return jsonResponse(r, `{"code":0,"data":{"plans":[{"plan_id":"trial","status":"active"}],"balances":[{"plan_id":"trial","show_name":"GLM-5.3","remaining_units":10}]}}`)
					case "/api/v1/zcode-plan/anthropic/v1/messages":
						trial.Add(1)
					case "/v1/messages":
						paid.Add(1)
					default:
						return nil, fmt.Errorf("unexpected path %s", r.URL.Path)
					}
					return jsonResponse(r, `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
				})}
				response = doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": "GLM-5.3", "max_tokens": 16, "messages": []any{map[string]string{"role": "user", "content": "hello"}}}, nil)
				wantTrial := int32(0)
				if freeFirst {
					wantTrial = 1
				}
				if response.Code != http.StatusOK || trial.Load() != wantTrial || paid.Load() != 1-wantTrial {
					t.Fatalf("saved free_first=%t: paid=%d trial=%d status=%d body=%s", freeFirst, paid.Load(), trial.Load(), response.Code, response.Body.String())
				}
				latest, err := env.store.GetConfig(ctx, staleCfg.ID)
				if err != nil {
					t.Fatal(err)
				}
				stored, err := zaiauth.ParseCredential([]byte(latest.OAuthCredential))
				if err != nil || stored.FreeFirst == nil || *stored.FreeFirst != freeFirst {
					t.Fatalf("persisted preference mismatch: %v", err)
				}
			})
		}
	}
}

/**
 * @brief 并发认证拒绝共用派生结果时，重试凭证独立且后续请求直接使用新 Key。
 * @param t 测试上下文。
 */
func TestProxy_ZAIConcurrentCredentialRefresh(t *testing.T) {
	const callers = 32
	env := setupProxyTestEnv(t, []testChannel{{name: "refresh race", authType: model.AuthTypeZAIOAuth, oauthCredential: `{"site":"bigmodel","api_key":"old.secret","access_token":"access"}`, models: "GLM-5.3", upstreamProtocol: "anthropic"}}, map[int]string{0: "https://paid.test"})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	allRejected := make(chan struct{})
	var rejected, resolved, accepted atomic.Int32
	env.server.client = &http.Client{Transport: oauthUsageRoundTripper(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/api/biz/customer/getCustomerInfo":
			resolved.Add(1)
			return jsonResponse(r, `{"code":0,"data":{"userId":"user","organizations":[{"organizationId":"org","projects":[{"projectId":"project"}]}]}}`)
		case "/api/biz/v1/organization/org/projects/project/api_keys":
			return jsonResponse(r, `{"code":0,"data":[{"name":"zcode-api-key","apiKey":"new"}]}`)
		case "/api/biz/v1/organization/org/projects/project/api_keys/copy/new":
			return jsonResponse(r, `{"code":0,"data":{"secretKey":"secret"}}`)
		case "/v1/messages":
			switch anthropicHeaderValue(r.Header, "x-api-key") {
			case "old.secret":
				if rejected.Add(1) == callers {
					close(allRejected)
				}
				select {
				case <-allRejected:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return jsonResponseStatus(r, http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`)
			case "new.secret":
				accepted.Add(1)
				return jsonResponse(r, `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			}
		}
		return nil, fmt.Errorf("unexpected refresh request %s", r.URL.Path)
	})}
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"GLM-5.3","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-api-key")
		response := httptest.NewRecorder()
		env.engine.ServeHTTP(response, req)
		return response
	}
	responses := make(chan *httptest.ResponseRecorder, callers)
	for range callers {
		go func() { responses <- request() }()
	}
	for range callers {
		response := <-responses
		if response.Code != http.StatusOK {
			t.Errorf("retry status=%d body=%s", response.Code, response.Body.String())
		}
	}
	if response := request(); response.Code != http.StatusOK {
		t.Fatalf("next request status=%d body=%s", response.Code, response.Body.String())
	}
	if rejected.Load() != callers || accepted.Load() != callers+1 || resolved.Load() != 1 {
		t.Fatalf("rejected=%d accepted=%d key derivations=%d", rejected.Load(), accepted.Load(), resolved.Load())
	}
	configs, err := env.store.ListConfigs(ctx)
	if err != nil || len(configs) != 1 {
		t.Fatalf("reload channel: %v", err)
	}
	stored, err := zaiauth.ParseCredential([]byte(configs[0].OAuthCredential))
	if err != nil || stored.APIKey != "new.secret" || stored.Site != zaiauth.SiteBigModel {
		t.Fatalf("refreshed credential not persisted: %v", err)
	}
}

// End-to-end proxy coverage for Z.ai Coding Plan channels: a downstream
// Anthropic request must reach the upstream carrying ZCode's identity, ZCode's
// authentication header and ZCode's device fingerprint.
func TestProxy_ZAICodingPlanReplicatesZCodeWire(t *testing.T) {
	t.Parallel()

	var upstreamHeaders http.Header
	var upstreamBody []byte
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHeaders = r.Header.Clone()
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"glm-4.7","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer upstream.Close()

	credential := &zaiauth.Credential{APIKey: "key-id.secret", Email: "user@example.com", UserID: "u-1"}
	credentialJSON, err := credential.JSON()
	if err != nil {
		t.Fatalf("encode z.ai credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "zai-coding-plan", upstreamProtocol: "anthropic", models: "glm-4.7",
		authType: model.AuthTypeZAIOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: upstream.URL})

	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{
		"model":      "glm-4.7",
		"max_tokens": 16,
		"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
	}, map[string]string{"anthropic-version": "2023-06-01"})
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}

	if got := anthropicHeaderValue(upstreamHeaders, "x-api-key"); got != "key-id.secret" {
		t.Fatalf("upstream x-api-key = %q", got)
	}
	if got := anthropicHeaderValue(upstreamHeaders, "Authorization"); got != "Bearer key-id.secret" {
		t.Fatalf("upstream Authorization = %q, want the channel's paid Key", got)
	}
	for _, entry := range zaiauth.SourceHeaders() {
		if got := anthropicHeaderValue(upstreamHeaders, entry[0]); got != entry[1] {
			t.Fatalf("upstream %s = %q, want %q", entry[0], got, entry[1])
		}
	}

	var request map[string]any
	if err := json.Unmarshal(upstreamBody, &request); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	metadata, _ := request["metadata"].(map[string]any)
	rawIdentity, _ := metadata["user_id"].(string)
	var identity zaiRequestIdentity
	if err := json.Unmarshal([]byte(rawIdentity), &identity); err != nil {
		t.Fatalf("decode metadata.user_id %q: %v", rawIdentity, err)
	}
	if identity.DeviceID != credential.DeviceID || identity.DeviceID == "" {
		t.Fatalf("device id = %q, want %q", identity.DeviceID, credential.DeviceID)
	}
	if identity.SessionID == "" || identity.AccountUUID != "" {
		t.Fatalf("identity = %+v", identity)
	}
	if !strings.HasPrefix(rawIdentity, `{"device_id":`) {
		t.Fatalf("metadata.user_id shape = %s", rawIdentity)
	}
}

/**
 * @brief 普通模型自动体验失败后，只有额度错误阻断显式 Trial；自动避让和缓存重查分别验证。
 * @param t 测试上下文。
 */
func TestProxy_ZAITrialCooldownBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		blocked    bool
		invalidate bool
	}{
		{"risk", 405, `{"error":{"code":3012,"message":"unusual activity"}}`, true, true},
		{"risk with 429", 429, `{"error":{"code":3012,"message":"unusual activity"}}`, true, true},
		{"risk with 403", 403, `{"error":{"code":3012,"message":"unusual activity"}}`, true, true},
		{"quota", 429, `{"error":{"code":1302,"message":"rate limited"}}`, true, true},
		{"soft risk", 200, `{"code":3012,"msg":"unusual activity"}`, true, true},
		{"nested soft risk", 200, `{"code":0,"error":{"code":"3012","message":"unusual activity"}}`, true, true},
		{"soft quota", 200, `{"code":1113,"msg":"no resource package"}`, true, true},
		{"soft quota limit", 200, `{"error":{"code":"1005","message":"exceed quota limit"}}`, true, true},
		{"quota with client status", 400, `{"type":"error","error":{"type":"invalid_request_error","code":1113,"message":"no resource package"}}`, true, true},
		{"unauthorized", 401, `{"error":{"type":"authentication_error","message":"invalid JWT"}}`, true, true},
		{"forbidden", 403, `{"error":{"type":"permission_error","message":"access denied"}}`, true, true},
		{"server", 500, `{"error":{"type":"api_error","message":"unavailable"}}`, true, false},
		{"concurrency", 429, `{"code":3008,"msg":"concurrency limit exceeded"}`, true, false},
		{"soft concurrency", 200, `{"code":3009,"msg":"concurrency limit exceeded"}`, true, false},
		{"nested concurrency", 429, `{"code":0,"error":{"code":3008,"message":"busy"}}`, true, false},
		{"nested soft concurrency", 200, `{"code":0,"error":{"code":3009,"message":"busy"}}`, true, false},
		{"truncated quota", 429, `{"error":{"code":1302`, true, true},
		{"truncated soft quota", 200, `{"code":1113`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var trialCalls, paidCalls, balanceCalls atomic.Int32
			firstTrial := int32(1)
			if zaiConcurrencyError([]byte(tc.body)) {
				firstTrial = 3
			}
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/biz/subscription/list":
					_, _ = io.WriteString(w, `{"code":0,"data":[{"status":"VALID"}]}`)
					return
				case "/api/v1/zcode-plan/billing/balance":
					balanceCalls.Add(1)
					_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":"active"}],"balances":[{"plan_id":"gift","capabilities":["model:glm-5.3-flash"],"remaining_units":10}]}}`)
					return
				}
				var body struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "GLM-5.3-Flash" {
					t.Errorf("upstream model=%q err=%v", body.Model, err)
				}
				if strings.Contains(r.URL.Path, "/zcode-plan/anthropic/") {
					if trialCalls.Add(1) <= firstTrial {
						w.WriteHeader(tc.status)
						_, _ = io.WriteString(w, tc.body)
						if strings.HasPrefix(tc.name, "truncated") {
							// 发出部分响应后中断传输，确保读取端收到错误而非正常 EOF。
							w.(http.Flusher).Flush()
							panic(http.ErrAbortHandler)
						}
						return
					}
				} else {
					paidCalls.Add(1)
				}
				_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3-Flash","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			env := setupProxyTestEnv(t, []testChannel{{name: "trial boundary", upstreamProtocol: "anthropic", models: "GLM-5.3-Flash,GLM-5.3-Flash-Trial", authType: model.AuthTypeZAIOAuth, oauthCredential: `{"api_key":"key.secret","jwt_token":"jwt"}`}}, map[int]string{0: upstream.URL})
			env.server.zaiService.OAuthBaseURL, env.server.zaiService.BizBaseURL = upstream.URL+"/api/v1", upstream.URL
			request := func(name string) *httptest.ResponseRecorder {
				return doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": name, "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "hello"}}}, nil)
			}
			firstPaid, firstStatus := int32(1), http.StatusOK
			if strings.Contains(tc.name, "risk") {
				// 明确风控停止当前请求，但仍保留账号暂停供后续请求避让。
				firstPaid, firstStatus = 0, tc.status
				if firstStatus == 200 {
					firstStatus = http.StatusForbidden
				}
			}
			if response := request("GLM-5.3-Flash"); response.Code != firstStatus || trialCalls.Load() != firstTrial || paidCalls.Load() != firstPaid {
				t.Fatalf("fallback: status=%d trial=%d paid=%d body=%s", response.Code, trialCalls.Load(), paidCalls.Load(), response.Body.String())
			}
			configs, err := env.store.ListConfigs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			until := cooldowns[configs[0].ID]["GLM-5.3-Flash-Trial"]
			accountFailure := strings.Contains(tc.name, "risk") || tc.status == 401 || tc.status == 403
			if until.After(time.Now()) != (tc.blocked && !accountFailure) {
				t.Fatalf("Trial blocked=%v want=%v", until, tc.blocked)
			}
			if response := request("GLM-5.3-Flash"); response.Code != 200 {
				t.Fatalf("next ordinary request: %d %s", response.Code, response.Body.String())
			}
			wantTrial, wantPaid := firstTrial+1, firstPaid
			if tc.blocked {
				wantTrial, wantPaid = firstTrial, firstPaid+1
			}
			if trialCalls.Load() != wantTrial || paidCalls.Load() != wantPaid {
				t.Fatalf("automatic routing: trial=%d paid=%d want=%d/%d", trialCalls.Load(), paidCalls.Load(), wantTrial, wantPaid)
			}
			response := request("GLM-5.3-Flash-Trial")
			if tc.blocked {
				if response.Code == 200 || trialCalls.Load() != wantTrial {
					t.Fatalf("quota block bypassed: status=%d calls=%d", response.Code, trialCalls.Load())
				}
			} else if response.Code != 200 || trialCalls.Load() != wantTrial+1 || paidCalls.Load() != wantPaid {
				t.Fatalf("explicit Trial blocked by ordinary failure: status=%d trial=%d paid=%d", response.Code, trialCalls.Load(), paidCalls.Load())
			}

		})
	}
}

/**
 * @brief 同账号重新授权恢复 JWT 认证冷却，保留模型、账号风控和出口暂停。
 * @param t 测试上下文。
 */
func TestProxy_ZAITrialReauthorizationCooldown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		recover bool
	}{
		{"unauthorized", 401, `{"error":{"type":"authentication_error","message":"invalid JWT"}}`, true},
		{"forbidden", 403, `{"error":{"type":"permission_error","message":"access denied"}}`, true},
		{"account-risk", 403, `{"code":3012,"msg":"unusual activity"}`, false},
		{"egress-risk", 405, `<html>blocked</html>`, false},
		{"model-failure", 500, `{"error":{"type":"api_error","message":"unavailable"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/zcode-plan/billing/balance" {
					_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":"active"}],"balances":[{"plan_id":"gift","capabilities":["model:glm-5.3-flash","model:glm-4.7"],"remaining_units":10}]}}`)
					return
				}
				if r.URL.Path != "/api/v1/zcode-plan/anthropic/v1/messages" {
					t.Errorf("unexpected endpoint: %s", r.URL.Path)
					w.WriteHeader(500)
					return
				}
				calls.Add(1)
				var body struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Model == "GLM-4.7" {
					w.WriteHeader(500)
					_, _ = io.WriteString(w, `{"error":{"type":"api_error","message":"unavailable"}}`)
					return
				}
				if r.Header.Get("Authorization") == "Bearer old-jwt" {
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
					return
				}
				if r.Header.Get("Authorization") != "Bearer new-jwt" {
					t.Error("reauthorized request did not use the new JWT")
				}
				_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3-Flash","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			env := setupProxyTestEnv(t, []testChannel{{name: "reauthorization", upstreamProtocol: "anthropic", models: "GLM-5.3-Flash-Trial,GLM-4.7-Trial", authType: model.AuthTypeZAIOAuth, oauthCredential: `{"jwt_token":"old-jwt","user_id":"account"}`}}, map[int]string{0: upstream.URL})
			env.server.zaiService.OAuthBaseURL = upstream.URL + "/api/v1"
			request := func(name string) *httptest.ResponseRecorder {
				return doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": name, "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "hello"}}}, nil)
			}
			// 先建立另一个模型的真实故障窗口，授权恢复不得顺带清除它。
			if response := request("GLM-4.7-Trial"); response.Code == 200 || calls.Load() != 1 {
				t.Fatalf("model failure: status=%d calls=%d", response.Code, calls.Load())
			}
			if response := request("GLM-5.3-Flash-Trial"); response.Code == 200 || calls.Load() != 2 {
				t.Fatalf("initial failure: status=%d calls=%d", response.Code, calls.Load())
			}
			if response := request("GLM-5.3-Flash-Trial"); response.Code == 200 || calls.Load() != 2 {
				t.Fatalf("cooldown bypassed: status=%d calls=%d", response.Code, calls.Load())
			} else if tc.name != "model-failure" {
				if seconds, err := strconv.Atoi(response.Header().Get("Retry-After")); err != nil || seconds <= 0 {
					t.Fatalf("missing account/egress retry time: %q", response.Header().Get("Retry-After"))
				}
			}
			updated, created, err := env.server.commitZAICredential(context.Background(), &zaiauth.Credential{JWTToken: "new-jwt", UserID: "account"}, oauthLoginOptions{})
			if err != nil || created || updated == nil {
				t.Fatalf("reauthorization: created=%v err=%v", created, err)
			}
			response := request("GLM-5.3-Flash-Trial")
			wantCalls := int32(2)
			if tc.recover {
				wantCalls++
			}
			if (response.Code == 200) != tc.recover || calls.Load() != wantCalls {
				t.Fatalf("after reauthorization: status=%d calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
			}
			if response := request("GLM-4.7-Trial"); response.Code == 200 || calls.Load() != wantCalls {
				t.Fatalf("reauthorization cleared unrelated model cooldown: status=%d calls=%d", response.Code, calls.Load())
			}
		})
	}
}

/**
 * @brief 验证和明确风控拒绝停止套餐及跨渠道重放，只有风控保留账号暂停。
 * @param t 测试上下文。
 */
func TestProxy_ZAITrialTerminalBusinessErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		code     string
		status   int
		stream   bool
		topLevel bool
		repair   bool
	}{
		{name: "captcha-http", code: "3007", status: 400},
		{name: "captcha-busy-status", code: "3007", status: 429},
		{name: "captcha-soft", code: "3007", status: 200},
		{name: "captcha-sse", code: "3007", status: 200, stream: true},
		{name: "captcha-top-level", code: "3007", status: 200, topLevel: true},
		{name: "captcha-repair", code: "3007", status: 400, repair: true},
		{name: "risk-http", code: "3012", status: 403},
		{name: "risk-busy-status", code: "3012", status: 429},
		{name: "risk-soft", code: "3012", status: 200},
		{name: "risk-sse", code: "3012", status: 200, stream: true},
		{name: "risk-top-level", code: "3012", status: 200, topLevel: true},
		{name: "risk-repair", code: "3012", status: 400, repair: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, configs := newZAIStartHarness(t)
			control := env.server.client
			var calls atomic.Int32
			var firstJWT string
			env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				if !strings.HasSuffix(req.URL.Path, "/messages") {
					return control.Do(req)
				}
				if calls.Add(1) == 1 {
					firstJWT = req.Header.Get("Authorization")
					// 业务码必须优先，不能因消息同时含 busy 文案就进入重试。
					body := `{"code":0,"error":{"code":"` + tc.code + `","message":"concurrency limit exceeded"}}`
					if tc.topLevel {
						body = `{"code":` + tc.code + `,"msg":"concurrency limit exceeded"}`
					} else if tc.repair {
						body = `{"error":{"code":"` + tc.code + `","type":"invalid_request_error","message":"thinking.budget_tokens must be less than max_tokens"}}`
					}
					resp := zaiStartTestResponse(req, tc.status, body)
					if tc.stream {
						resp.Header.Set("Content-Type", "text/event-stream")
						resp.Body = io.NopCloser(strings.NewReader("event: ping\ndata: {\"type\":\"ping\"}\n\nevent: error\ndata: " + body + "\n\n"))
					}
					return resp, nil
				}
				return zaiStartTestResponse(req, 200, `{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), nil
			})}
			body := map[string]any{"model": "GLM-5.3-Flash", "max_tokens": 16, "stream": tc.stream, "messages": []map[string]string{{"role": "user", "content": "hello"}}}
			if tc.repair {
				body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 1024}
			}
			response := doProxyRequest(t, env.engine, "/v1/messages", body, nil)
			wantStatus := tc.status
			if wantStatus == 200 {
				wantStatus = http.StatusBadRequest
				if tc.code == "3012" {
					wantStatus = http.StatusForbidden
				}
			}
			if response.Code != wantStatus || calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
			}
			codePath, messagePath := "error.code", "error.message"
			if tc.topLevel {
				codePath, messagePath = "code", "msg"
			}
			if got := gjson.GetBytes(response.Body.Bytes(), codePath).String(); got != tc.code {
				t.Fatalf("code=%s body=%s", got, response.Body.String())
			}
			if tc.code == "3007" && !strings.Contains(gjson.GetBytes(response.Body.Bytes(), messagePath).String(), "需要人机验证") {
				t.Fatalf("missing verification hint: %s", response.Body.String())
			}
			windows, err := env.store.GetAllModelCooldowns(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, models := range windows {
				for _, until := range models {
					if until.After(time.Now()) {
						t.Fatal("business rejection cooled a model")
					}
				}
			}
			var affected, other *model.Config
			for _, cfg := range configs {
				credential, err := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
				if err != nil {
					t.Fatal(err)
				}
				if "Bearer "+credential.JWTToken == firstJWT {
					affected = cfg
				} else {
					other = cfg
				}
			}
			if affected == nil || other == nil {
				t.Fatal("missing test accounts")
			}
			request := func(cfg *model.Config) *httptest.ResponseRecorder {
				return doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": "GLM-5.3-Flash-Trial", "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "hello"}}}, map[string]string{"X-CCLoad-Channel": cfg.Name})
			}
			response = request(affected)
			if tc.code == "3012" {
				if response.Code == 200 || calls.Load() != 1 {
					t.Fatalf("risk pause lost: status=%d calls=%d", response.Code, calls.Load())
				}
			} else if response.Code != 200 || calls.Load() != 2 {
				t.Fatalf("captcha installed a pause: status=%d calls=%d", response.Code, calls.Load())
			}
			if response = request(other); response.Code != 200 {
				t.Fatalf("unrelated account paused: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

/**
 * @brief 代理与管理检测的在途体验成功不能清除执行期间写入的额度暂停。
 * @param t 测试上下文。
 */
func TestProxy_ZAITrialConcurrentCooldown(t *testing.T) {
	for _, admin := range []bool{false, true} {
		t.Run(fmt.Sprintf("admin=%v", admin), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			var trialCalls atomic.Int32
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/biz/subscription/list":
					_, _ = io.WriteString(w, `{"code":0,"data":[{"status":"VALID"}]}`)
					return
				case "/api/v1/zcode-plan/billing/balance":
					_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":"active"}],"balances":[{"plan_id":"gift","show_name":"GLM-5.3","remaining_units":10}]}}`)
					return
				case "/api/v1/zcode-plan/anthropic/v1/messages":
					if trialCalls.Add(1) == 1 {
						close(started)
						select {
						case <-release:
						case <-r.Context().Done():
							return
						}
					} else {
						w.WriteHeader(http.StatusTooManyRequests)
						_, _ = io.WriteString(w, `{"error":{"code":1113,"message":"quota exhausted"}}`)
						return
					}
				}
				_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			env := setupProxyTestEnv(t, []testChannel{{name: "concurrent trial", upstreamProtocol: "anthropic", models: "GLM-5.3,GLM-5.3-Trial", authType: model.AuthTypeZAIOAuth, oauthCredential: `{"api_key":"key.secret","jwt_token":"jwt"}`}}, map[int]string{0: upstream.URL})
			env.server.zaiService.OAuthBaseURL, env.server.zaiService.BizBaseURL = upstream.URL+"/api/v1", upstream.URL
			configs, err := env.store.ListConfigs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			request := func() *httptest.ResponseRecorder {
				if admin {
					path := fmt.Sprintf("/admin/channels/%d/test", configs[0].ID)
					req := newRequest(http.MethodPost, path, strings.NewReader(`{"model":"GLM-5.3-Trial","content":"hello","client_protocol":"anthropic"}`))
					req.Header.Set("Content-Type", "application/json")
					c, response := newTestContext(t, req)
					c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(configs[0].ID)}}
					env.server.HandleChannelTest(c)
					return response
				}
				return doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": "GLM-5.3-Trial", "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "hello"}}}, nil)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- request() }()
			select {
			case <-started:
			case <-time.After(10 * time.Second):
				t.Fatal("first Trial request did not reach upstream")
			}
			// 第二个真实并发请求失败并写入冷却，第一条在途成功不得清除它。
			_ = request()
			before, err := env.store.GetAllModelCooldowns(context.Background())
			if err != nil || !before[configs[0].ID]["GLM-5.3-Trial"].After(time.Now()) {
				t.Fatalf("quota response did not pause Trial: %v %v", before, err)
			}
			close(release)
			select {
			case response := <-done:
				if response.Code != http.StatusOK || (admin && !gjson.GetBytes(response.Body.Bytes(), "success").Bool()) {
					t.Fatalf("in-flight success failed: %d %s", response.Code, response.Body.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("in-flight success did not finish")
			}
			after, err := env.store.GetAllModelCooldowns(context.Background())
			if err != nil || !after[configs[0].ID]["GLM-5.3-Trial"].Equal(before[configs[0].ID]["GLM-5.3-Trial"]) {
				t.Fatalf("success changed quota pause: before=%v after=%v err=%v", before, after, err)
			}
		})
	}
}

/**
 * @brief 从客户端入口验证套餐选择、认证、请求体隔离和失败后的渠道可用性。
 * @param t 测试上下文。
 */
func TestProxy_ZAITrialRouting(t *testing.T) {
	for _, tc := range []struct {
		name        string
		paid        bool
		key         bool
		freeFirst   bool
		trialModel  bool
		trialStatus int
		softError   bool
		streamError string
		wantTrial   int32
		wantPaid    int32
		wantOK      bool
	}{
		{"free first", true, true, true, false, 200, false, "", 1, 0, true},
		{"paid preferred", true, true, false, false, 200, false, "", 0, 1, true},
		{"quota fallback", true, true, true, false, 429, false, "", 1, 1, true},
		{"auth fallback", true, true, true, false, 401, false, "", 1, 1, true},
		{"risk fallback", true, true, true, false, 405, false, "", 1, 1, true},
		{"business fallback", true, true, true, false, 200, true, "", 1, 1, true},
		{"first SSE error fallback", true, true, true, false, 200, false, "first", 1, 1, true},
		{"empty SSE fallback", true, true, true, false, 200, false, "empty", 1, 1, true},
		{"started SSE never replayed", true, true, true, false, 200, false, "started", 1, 0, true},
		{"mixed SSE never replayed", true, true, true, false, 200, false, "mixed", 1, 0, true},
		{"explicit mixed SSE", true, true, true, true, 200, false, "mixed", 1, 0, true},
		{"explicit SSE no paid fallback", true, true, true, true, 200, false, "first", 1, 0, false},
		{"explicit trial", true, true, false, true, 200, false, "", 1, 0, true},
		{"explicit no paid fallback", true, true, true, true, 429, false, "", 1, 0, false},
		{"explicit server error", true, true, true, true, 500, false, "", 1, 0, false},
		{"explicit method error", true, true, true, true, 405, false, "", 1, 0, false},
		{"explicit endpoint missing", true, true, true, true, 404, false, "", 1, 0, false},
		{"JWT only method error", false, false, true, false, 405, false, "", 1, 0, false},
		{"JWT only endpoint missing", false, false, true, false, 404, false, "", 1, 0, false},
		{"client error no paid replay", true, true, true, false, 400, false, "", 1, 0, false},
		{"explicit client error", true, true, true, true, 400, false, "", 1, 0, false},
		{"JWT rejection no key refresh", false, false, false, false, 401, false, "", 1, 0, false},
		{"key without paid subscription", false, true, false, false, 200, false, "", 1, 0, true},
		{"JWT only", false, false, false, false, 200, false, "", 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var trialCalls, paidCalls atomic.Int32
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/biz/subscription/list":
					if r.Header.Get("Authorization") != "key.secret" {
						t.Error("subscription must use raw API key")
					}
					if tc.paid {
						_, _ = io.WriteString(w, `{"code":0,"data":[{"status":"VALID","productName":"Pro"}]}`)
					} else {
						_, _ = io.WriteString(w, `{"code":0,"data":[]}`)
					}
					return
				case "/api/v1/zcode-plan/billing/balance":
					if r.Header.Get("Authorization") != "Bearer trial-jwt" || r.Header.Get("X-Device-Mid") == "" {
						t.Error("balance authentication missing")
					}
					_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":"active","name":"Start Plan"}],"balances":[{"plan_id":"gift","capabilities":["model:glm-5.3"],"total_units":"10","used_units":1,"remaining_units":"9"}]}}`)
					return
				}
				var body struct {
					Model    string            `json:"model"`
					System   json.RawMessage   `json:"system"`
					Messages []json.RawMessage `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if r.URL.Path == "/api/v1/zcode-plan/anthropic/v1/messages" {
					trialCalls.Add(1)
					if body.Model != "GLM-5.3" || r.Header.Get("Authorization") != "Bearer trial-jwt" || r.Header.Get("x-api-key") != "" || r.URL.RawQuery != "" {
						t.Errorf("invalid trial request: %s %s", r.URL, body.Model)
					}
					// Trial 必须复刻 ZCode Electron 的查询身份，不能退回 CLI 或无轮次标识。
					if r.Header.Get("X-Title") != "Z Code@electron" || anthropicHeaderValue(r.Header, "x-query-id") == "" {
						t.Errorf("invalid trial identity: title=%q query=%q", r.Header.Get("X-Title"), anthropicHeaderValue(r.Header, "x-query-id"))
					}
					var blocks []struct {
						Text string `json:"text"`
					}
					if json.Unmarshal(body.System, &blocks) != nil || len(blocks) != 3 || !strings.Contains(blocks[2].Text, "caller-system") || len(body.Messages) != 2 {
						t.Error("trial body lost caller context or ZCode structure")
					}
					if tc.streamError != "" {
						w.Header().Set("Content-Type", "text/event-stream")
						if tc.streamError == "mixed" {
							_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"GLM-5.3\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\r\nevent: message_stop\r\ndata: {\"type\":\"message_stop\"}\r\n\n")
							return
						}
						_, _ = io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
						if tc.streamError == "empty" {
							return
						}
						if tc.streamError == "started" {
							_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"GLM-5.3\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n")
						}
						_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"trial unavailable\"}}\n\n")
						return
					}
					w.WriteHeader(tc.trialStatus)
					if tc.trialStatus == 400 {
						_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 215000 tokens > 200000 maximum"}}`)
						return
					}
					if tc.softError {
						_, _ = io.WriteString(w, `{"code":1113,"msg":"quota exhausted"}`)
						return
					}
					if tc.trialStatus != 200 {
						_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"trial quota unavailable"}}`)
						return
					}
				} else {
					paidCalls.Add(1)
					if anthropicHeaderValue(r.Header, "x-api-key") != "key.secret" || anthropicHeaderValue(r.Header, "Authorization") != "Bearer key.secret" {
						t.Error("paid authentication was replaced")
					}
					var system []struct {
						Text string `json:"text"`
					}
					if json.Unmarshal(body.System, &system) != nil || len(system) != 1 || system[0].Text != "caller-system" || len(body.Messages) != 1 {
						t.Errorf("trial body leaked into paid request: %s", body.System)
					}
				}
				_, _ = io.WriteString(w, `{"id":"msg_trial","type":"message","role":"assistant","model":"GLM-5.3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
			}))
			credential := &zaiauth.Credential{JWTToken: "trial-jwt", UserID: "trial-user", FreeFirst: &tc.freeFirst}
			if tc.key {
				credential.APIKey = "key.secret"
			}
			raw, err := credential.JSON()
			if err != nil {
				t.Fatal(err)
			}
			env := setupProxyTestEnv(t, []testChannel{{name: "zai-trial", upstreamProtocol: "anthropic", models: "GLM-5.3,GLM-5.3-Trial", authType: model.AuthTypeZAIOAuth, oauthCredential: raw}}, map[int]string{0: upstream.URL})
			env.server.zaiService.OAuthBaseURL = upstream.URL + "/api/v1"
			env.server.zaiService.BizBaseURL = upstream.URL
			name := "GLM-5.3"
			if tc.trialModel {
				name += "-Trial"
			}
			response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": name, "stream": tc.streamError != "", "system": "caller-system", "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}, nil)
			if (response.Code == 200) != tc.wantOK {
				t.Fatalf("response %d: %s", response.Code, response.Body.String())
			}
			if trialCalls.Load() != tc.wantTrial || paidCalls.Load() != tc.wantPaid {
				t.Fatalf("trial=%d paid=%d", trialCalls.Load(), paidCalls.Load())
			}
			configs, err := env.store.ListConfigs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if configs[0].CooldownUntil != 0 || !configs[0].Enabled {
				t.Fatalf("trial failure disabled/ cooled the paid account: %+v", configs[0])
			}
			cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			trialCooling := cooldowns[configs[0].ID]["GLM-5.3-Trial"].After(time.Now())
			if tc.trialStatus >= 400 && trialCooling != (tc.trialStatus == 429 || tc.trialStatus == 500) {
				t.Fatalf("trial cooling=%v status=%d", trialCooling, tc.trialStatus)
			}
			if cooldowns[configs[0].ID]["GLM-5.3"].After(time.Now()) {
				t.Fatal("trial failure cooled paid model")
			}
			if tc.wantPaid == 1 && tc.wantTrial == 1 {
				response = doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": name, "system": "caller-system", "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": "hello again"}}}, nil)
				// 真实体验故障已经冷却，自动模式直接使用仍有效的付费资格。
				wantTrial := int32(1)
				if response.Code != 200 || trialCalls.Load() != wantTrial || paidCalls.Load() != 2 {
					t.Fatalf("trial cooldown ignored: trial=%d paid=%d response=%d", trialCalls.Load(), paidCalls.Load(), response.Code)
				}
			}
			if !tc.key && tc.trialStatus == 401 {
				response = doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": name, "system": "caller-system", "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": "again"}}}, nil)
				after, err := env.store.GetAllModelCooldowns(context.Background())
				if err != nil || response.Code != 429 || trialCalls.Load() != 1 || after[configs[0].ID]["GLM-5.3-Trial"].After(time.Now()) {
					t.Fatalf("JWT-only cooldown bypassed/extended: calls=%d status=%d err=%v", trialCalls.Load(), response.Code, err)
				}
			}
		})
	}
}

/**
 * @brief 本地体验拒绝不消耗 RPM；真实体验发送及付费回退分别计数。
 * @param t 测试上下文。
 */
func TestProxy_ZAIPlanRPMBoundary(t *testing.T) {
	for _, exact := range []bool{false, true} {
		for _, scenario := range []string{"missing", "exhausted", "expired_login", "trial_sent", "trial_success", "fallback"} {
			t.Run(fmt.Sprintf("%s/exact=%t", scenario, exact), func(t *testing.T) {
				var trialCalls, paidCalls atomic.Int32
				upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/api/biz/subscription/list":
						_, _ = io.WriteString(w, `{"code":0,"data":[{"status":"VALID"}]}`)
					case "/api/v1/zcode-plan/billing/balance":
						switch scenario {
						case "missing":
							_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[],"balances":[]}}`)
						case "exhausted":
							_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"trial","status":"active"}],"balances":[{"plan_id":"trial","show_name":"GLM-5.3","remaining_units":0}]}}`)
						default:
							_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"trial","status":"active"}],"balances":[{"plan_id":"trial","show_name":"GLM-5.3","remaining_units":10}]}}`)
						}
					case "/api/v1/zcode-plan/anthropic/v1/messages":
						trialCalls.Add(1)
						if scenario == "trial_success" {
							_, _ = io.WriteString(w, `{"id":"trial","type":"message","role":"assistant","model":"GLM-5.3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
							return
						}
						w.WriteHeader(http.StatusTooManyRequests)
						_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"trial unavailable"}}`)
					case "/v1/messages", "/relay/messages":
						paidCalls.Add(1)
						_, _ = io.WriteString(w, `{"id":"paid","type":"message","role":"assistant","model":"GLM-5.3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
					default:
						t.Errorf("unexpected endpoint: %s", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				freeFirst := scenario == "fallback" || scenario == "trial_success"
				credential := zaiauth.Credential{APIKey: "key.secret", JWTToken: "jwt", FreeFirst: &freeFirst}
				if scenario == "expired_login" {
					credential.JWTToken = "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`)) + ".signature"
				}
				raw, err := credential.JSON()
				if err != nil {
					t.Fatal(err)
				}
				target := upstream.URL
				if exact {
					target += "/relay/messages#"
				}
				env := setupProxyTestEnv(t, []testChannel{{name: "rpm boundary", upstreamProtocol: "anthropic", models: "GLM-5.3,GLM-5.3-Trial", authType: model.AuthTypeZAIOAuth, oauthCredential: raw}}, map[int]string{0: target})
				env.server.zaiService.OAuthBaseURL = upstream.URL + "/api/v1"
				env.server.zaiService.BizBaseURL = upstream.URL
				configs, err := env.store.ListConfigs(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				configs[0].RPMLimit = 1
				if scenario == "fallback" {
					configs[0].RPMLimit = 2
				}
				configs[0].MaxConcurrency = 1
				if _, err := env.store.UpdateConfig(context.Background(), configs[0].ID, configs[0]); err != nil {
					t.Fatal(err)
				}
				env.server.InvalidateChannelListCache()
				body := map[string]any{"model": "GLM-5.3-Trial", "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "hello"}}}
				wantFirst := http.StatusBadRequest
				if scenario == "exhausted" || scenario == "trial_sent" {
					wantFirst = http.StatusTooManyRequests
				}
				if scenario == "fallback" {
					body["model"], wantFirst = "GLM-5.3", http.StatusOK
				}
				if scenario == "trial_success" {
					wantFirst = http.StatusOK
				}
				first := doProxyRequest(t, env.engine, "/v1/messages", body, nil)
				if first.Code != wantFirst {
					t.Fatalf("first status=%d want=%d body=%s", first.Code, wantFirst, first.Body.String())
				}
				wantTrial := int32(0)
				if scenario == "trial_sent" || scenario == "trial_success" || scenario == "fallback" {
					wantTrial = 1
				}
				if trialCalls.Load() != wantTrial {
					t.Fatalf("trial calls=%d want=%d", trialCalls.Load(), wantTrial)
				}
				body["model"] = "GLM-5.3"
				second := doProxyRequest(t, env.engine, "/v1/messages", body, nil)
				wantSecond, wantPaid := http.StatusOK, int32(1)
				if scenario == "trial_sent" || scenario == "trial_success" || scenario == "fallback" {
					wantSecond = http.StatusServiceUnavailable
					if scenario != "fallback" {
						wantPaid = 0
					}
				}
				if second.Code != wantSecond || paidCalls.Load() != wantPaid || trialCalls.Load() != wantTrial {
					t.Fatalf("second status=%d want=%d paid=%d want=%d trial=%d body=%s", second.Code, wantSecond, paidCalls.Load(), wantPaid, trialCalls.Load(), second.Body.String())
				}
				third := doProxyRequest(t, env.engine, "/v1/messages", body, nil)
				if third.Code != http.StatusServiceUnavailable || paidCalls.Load() != wantPaid || trialCalls.Load() != wantTrial {
					t.Fatalf("RPM not enforced: status=%d paid=%d trial=%d body=%s", third.Code, paidCalls.Load(), trialCalls.Load(), third.Body.String())
				}
				if scenario == "trial_success" {
					cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
					if err != nil || len(cooldowns[configs[0].ID]) != 0 {
						t.Fatalf("local RPM rejection cooled model: %v err=%v", cooldowns, err)
					}
				}
			})
		}
	}
}

/**
 * @brief 未生效请求不写故障冷却，生效后的首次请求能够立即调用体验端点。
 * @param t 测试上下文。
 */
func TestProxy_ZAIActivationAfterPendingRequest(t *testing.T) {
	for _, tc := range []struct {
		name    string ///< 调用模型。
		delayed bool   ///< 付费查询是否跨过体验开始时间。
	}{{"GLM-5.3", false}, {"GLM-5.3-Trial", false}, {"GLM-5.3", true}} {
		t.Run(fmt.Sprintf("%s/delayed=%v", tc.name, tc.delayed), func(t *testing.T) {
			name := tc.name
			boundary := time.Now().Add(time.Hour).Unix()
			var activated atomic.Bool
			var inference atomic.Int32
			var giftQueries atomic.Int32
			giftReturned := make(chan struct{})
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/biz/subscription/list" {
					if !activated.Load() {
						<-giftReturned
						time.Sleep(1200 * time.Millisecond)
					}
					_, _ = io.WriteString(w, `{"code":0,"data":[]}`)
					return
				}
				now := boundary - 1
				balances := []any{}
				if activated.Load() {
					now = boundary
					balances = append(balances, map[string]any{"plan_id": "p", "entitlement_id": "e", "bucket_id": "b", "show_name": "GLM-5.3", "total_units": 100, "remaining_units": 100})
				}
				w.Header().Set("Date", time.Unix(now, 0).UTC().Format(http.TimeFormat))
				if r.URL.Path == "/api/v1/zcode-plan/billing/balance" {
					if giftQueries.Add(1) == 1 {
						defer close(giftReturned)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"plans": []any{map[string]any{"plan_id": "p", "status": "active", "starts_at": boundary, "entitlements": []any{map[string]any{"entitlement_id": "e", "effective_at": boundary, "show_name": "GLM-5.3"}}}}, "balances": balances}})
					return
				}
				if r.URL.Path != "/api/v1/zcode-plan/anthropic/v1/messages" {
					t.Errorf("unexpected inference URL: %s", r.URL.Path)
				}
				inference.Add(1)
				_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
			}))
			credential := `{"jwt_token":"jwt"}`
			if tc.delayed {
				credential = `{"api_key":"key","jwt_token":"jwt"}`
			}
			env := setupProxyTestEnv(t, []testChannel{{name: "pending", upstreamProtocol: "anthropic", models: "GLM-5.3,GLM-5.3-Trial", authType: model.AuthTypeZAIOAuth, oauthCredential: credential}}, map[int]string{0: upstream.URL})
			env.server.zaiService.OAuthBaseURL = upstream.URL + "/api/v1"
			env.server.zaiService.BizBaseURL = upstream.URL
			body := map[string]any{"model": name, "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}
			first := doProxyRequest(t, env.engine, "/v1/messages", body, nil)
			if first.Code != http.StatusBadRequest || inference.Load() != 0 {
				t.Fatalf("pending status=%d calls=%d", first.Code, inference.Load())
			}
			configs, err := env.store.ListConfigs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
			if err != nil || len(cooldowns[configs[0].ID]) != 0 {
				t.Fatalf("local rejection left cooldown: %v err=%v", cooldowns, err)
			}
			activated.Store(true)
			if !tc.delayed {
				// 通过额度 API 获取新的上游时间和余额，验证已有冷却不会阻止生效。
				path := fmt.Sprintf("/admin/channels/%d/oauth-usage", configs[0].ID)
				c, response := newTestContext(t, newRequest(http.MethodPost, path, nil))
				c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(configs[0].ID)}}
				env.server.HandleOAuthUsage(c)
				if response.Code != http.StatusOK {
					t.Fatalf("refresh status=%d body=%s", response.Code, response.Body.String())
				}
			}
			// 查询期间跨过生效时间的快照不得缓存；下一次调用直接发现已发放的桶。
			second := doProxyRequest(t, env.engine, "/v1/messages", body, nil)
			if second.Code != http.StatusOK || inference.Load() != 1 {
				t.Fatalf("active plan blocked: status=%d calls=%d body=%s", second.Code, inference.Load(), second.Body.String())
			}
		})
	}
}

/**
 * @brief 付费、体验及回退的 ToolSearch 名称往返一致，JSON/SSE 参数不被字符串替换。
 * @param t 测试上下文。
 */
func TestProxy_ZAIToolSearch(t *testing.T) {
	for _, mode := range []string{"paid", "trial", "fallback", "sse", "sse-plain", "deflate", "bad-deflate", "collision"} {
		t.Run(mode, func(t *testing.T) {
			streaming := strings.HasPrefix(mode, "sse")
			alias := "ToolSearchTool"
			if mode == "collision" {
				alias = "ToolSearch"
			}
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/biz/subscription/list":
					_, _ = io.WriteString(w, `{"code":0,"data":[{"status":"VALID"}]}`)
					return
				case "/api/v1/zcode-plan/billing/balance":
					_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"p","status":"active"}],"balances":[{"plan_id":"p","show_name":"GLM-5.3","remaining_units":10}]}}`)
					return
				}
				var body struct {
					Tools []struct {
						Name         string `json:"name"`
						DeferLoading bool   `json:"defer_loading"`
					} `json:"tools"`
					ToolChoice struct {
						Name string `json:"name"`
					} `json:"tool_choice"`
					Messages []struct {
						Role    string          `json:"role"`
						Content json.RawMessage `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body.ToolChoice.Name != alias || body.Tools[0].Name != alias {
					t.Errorf("search alias not applied: %+v", body)
				}
				if mode != "collision" && (len(body.Tools) != 2 || body.Tools[1].Name != "Bash" || body.Tools[1].DeferLoading) {
					t.Errorf("deferred tools not cleaned: %+v", body.Tools)
				}
				if mode == "collision" && (len(body.Tools) != 4 || !body.Tools[2].DeferLoading) {
					t.Errorf("alias collision changed tools: %+v", body.Tools)
				}
				for _, message := range body.Messages {
					if message.Role != "assistant" {
						continue
					}
					var blocks []struct {
						Name  string            `json:"name"`
						Input map[string]string `json:"input"`
					}
					if err := json.Unmarshal(message.Content, &blocks); err != nil {
						t.Fatal(err)
					}
					if blocks[0].Name != alias || blocks[0].Input["query"] != "ToolSearch" {
						t.Errorf("history/parameters changed: %+v", blocks)
					}
				}
				if mode == "fallback" && strings.Contains(r.URL.Path, "/zcode-plan/anthropic/") {
					w.WriteHeader(429)
					_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error"}}`)
					return
				}
				block := map[string]any{"type": "tool_use", "id": "call_new", "name": alias, "input": map[string]string{"query": "ToolSearchTool"}}
				if mode == "bad-deflate" {
					w.Header().Set("Content-Encoding", "deflate")
					_, _ = io.WriteString(w, "broken compression")
					return
				}
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					if mode == "sse-plain" {
						w.Header().Set("Content-Type", "text/plain")
					}
					_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"GLM-5.3\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n")
					raw, _ := json.Marshal(map[string]any{"type": "content_block_start", "index": 0, "content_block": block})
					_, _ = io.WriteString(w, "event: content_block_start\ndata: "+string(raw)+"\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					return
				}
				var output io.Writer = w
				if mode == "deflate" {
					w.Header().Set("Content-Encoding", "deflate")
					compressed := zlib.NewWriter(w)
					defer func() { _ = compressed.Close() }()
					output = compressed
				}
				_ = json.NewEncoder(output).Encode(map[string]any{"id": "m", "type": "message", "role": "assistant", "model": "GLM-5.3", "content": []any{block}, "stop_reason": "tool_use", "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
			}))
			credential := &zaiauth.Credential{APIKey: "key.secret"}
			if mode == "bad-deflate" {
				credential.APIKey, credential.JWTToken = "", "jwt"
			}
			if mode == "trial" || mode == "fallback" || streaming {
				credential.JWTToken = "jwt"
			}
			raw, err := credential.JSON()
			if err != nil {
				t.Fatal(err)
			}
			env := setupProxyTestEnv(t, []testChannel{{name: "search", upstreamProtocol: "anthropic", models: "GLM-5.3", authType: model.AuthTypeZAIOAuth, oauthCredential: raw}}, map[int]string{0: upstream.URL})
			env.server.zaiService.OAuthBaseURL, env.server.zaiService.BizBaseURL = upstream.URL+"/api/v1", upstream.URL
			tool := func(name string) map[string]any {
				return map[string]any{"name": name, "defer_loading": true, "input_schema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]string{"type": "string"}}}}
			}
			tools := []any{tool("ToolSearch"), tool("DeferredToolPlaceholder"), tool("Bash")}
			if mode == "collision" {
				tools = append(tools, tool("ToolSearchTool"))
			}
			resp := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": "GLM-5.3", "max_tokens": 16, "stream": streaming, "tools": tools, "tool_choice": map[string]string{"type": "tool", "name": "ToolSearch"}, "messages": []any{
				map[string]any{"role": "user", "content": "hello"},
				map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call_old", "name": "ToolSearch", "input": map[string]string{"query": "ToolSearch"}}}},
				map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call_old", "content": "done"}}},
			}}, nil)
			if mode == "bad-deflate" {
				configs, err := env.store.ListConfigs(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
				if err != nil || resp.Code == 200 || !cooldowns[configs[0].ID]["GLM-5.3-Trial"].After(time.Now()) || cooldowns[configs[0].ID]["GLM-5.3"].After(time.Now()) || configs[0].CooldownUntil != 0 {
					t.Fatalf("compression error lost trial identity: status=%d cooldowns=%+v err=%v", resp.Code, cooldowns, err)
				}
				return
			}
			if resp.Code != 200 {
				t.Fatalf("response=%d %s", resp.Code, resp.Body.String())
			}
			var block struct {
				Name  string            `json:"name"`
				Input map[string]string `json:"input"`
			}
			if streaming {
				for event := range strings.SplitSeq(resp.Body.String(), "\n\n") {
					_, payload := parseSSEEventChunk([]byte(event + "\n\n"))
					var frame struct {
						Block json.RawMessage `json:"content_block"`
					}
					if json.Unmarshal(payload, &frame) == nil && len(frame.Block) > 0 {
						if err := json.Unmarshal(frame.Block, &block); err != nil {
							t.Fatal(err)
						}
					}
				}
			} else {
				var message struct {
					Content []json.RawMessage `json:"content"`
				}
				if err := json.Unmarshal(resp.Body.Bytes(), &message); err != nil || len(message.Content) != 1 {
					t.Fatalf("invalid message: %s", resp.Body.String())
				}
				if err := json.Unmarshal(message.Content[0], &block); err != nil {
					t.Fatal(err)
				}
			}
			if block.Name != "ToolSearch" || block.Input["query"] != "ToolSearchTool" {
				t.Fatalf("response identity/arguments corrupted: %+v", block)
			}
		})
	}
}

/**
 * @brief 显式体验请求按实际 wire 下标修复工具历史，重试不付费或重复注入提示词。
 * @param t 测试上下文。
 */
func TestProxy_ZAITrialRepairsHistory(t *testing.T) {
	var calls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/biz/subscription/list":
			_, _ = io.WriteString(w, `{"code":0,"data":[{"status":"VALID"}]}`)
		case "/api/v1/zcode-plan/billing/balance":
			_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"g","status":"active"}],"balances":[{"plan_id":"g","capabilities":["model:GLM-5.3"],"remaining_units":10}]}}`)
		case "/api/v1/zcode-plan/anthropic/v1/messages":
			var body struct {
				Model    string `json:"model"`
				System   []any  `json:"system"`
				Messages []struct {
					Content []struct {
						Type string `json:"type"`
					} `json:"content"`
				} `json:"messages"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Model != "GLM-5.3" || len(body.System) != 3 || len(body.Messages) != 4 {
				t.Errorf("invalid trial retry shape: %+v", body)
			}
			if calls.Add(1) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","param":"messages[2].content[0]","message":"messages.2.content.0: tool_use blocks are not supported"}}`)
				return
			}
			for _, message := range body.Messages {
				for _, block := range message.Content {
					if block.Type == "tool_use" || block.Type == "tool_result" {
						t.Error("rejected history was not repaired")
					}
				}
			}
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"GLM-5.3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
		default:
			t.Errorf("explicit trial reached paid endpoint: %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	raw, err := (&zaiauth.Credential{APIKey: "key.secret", JWTToken: "trial-jwt"}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	env := setupProxyTestEnv(t, []testChannel{{name: "zai-repair", upstreamProtocol: "anthropic", models: "GLM-5.3-Trial", authType: model.AuthTypeZAIOAuth, oauthCredential: raw}}, map[int]string{0: upstream.URL})
	env.server.zaiService.OAuthBaseURL = upstream.URL + "/api/v1"
	env.server.zaiService.BizBaseURL = upstream.URL
	var request map[string]any
	if err := json.Unmarshal([]byte(`{"model":"GLM-5.3-Trial","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]},{"role":"assistant","content":[{"type":"tool_use","id":"tool_1","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_1","content":"found"}]}],"tools":[{"name":"lookup","description":"Look up data","input_schema":{"type":"object","properties":{}}}]}`), &request); err != nil {
		t.Fatal(err)
	}
	response := doProxyRequest(t, env.engine, "/v1/messages", request, nil)
	if response.Code != http.StatusOK || calls.Load() != 2 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, calls.Load(), response.Body.String())
	}
}

/**
 * @brief 套餐快照仍有体验余额时，已过期 JWT 不得阻断有效付费 Key。
 * @param t 测试上下文。
 */
func TestProxy_ZAIExpiredJWTUsesPaid(t *testing.T) {
	var paid atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/biz/subscription/list":
			_, _ = io.WriteString(w, `{"code":0,"data":[{"status":"VALID"}]}`)
		case "/api/v1/zcode-plan/billing/balance":
			_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"g","status":"active"}],"balances":[{"plan_id":"g","capabilities":["model:GLM-5.3"],"remaining_units":10}]}}`)
		case "/api/v1/zcode-plan/anthropic/v1/messages":
			t.Error("expired JWT sent to trial endpoint")
			w.WriteHeader(http.StatusUnauthorized)
		default:
			paid.Add(1)
			_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"GLM-5.3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
		}
	}))
	now := time.Now()
	claims, _ := json.Marshal(map[string]int64{"exp": now.Add(-time.Second).Unix()})
	raw, err := (&zaiauth.Credential{APIKey: "key.secret", JWTToken: "header." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	env := setupProxyTestEnv(t, []testChannel{{name: "zai-expired", upstreamProtocol: "anthropic", models: "GLM-5.3", authType: model.AuthTypeZAIOAuth, oauthCredential: raw}}, map[int]string{0: upstream.URL})
	env.server.zaiService.OAuthBaseURL = upstream.URL + "/api/v1"
	env.server.zaiService.BizBaseURL = upstream.URL
	env.server.zaiService.Now = func() time.Time { return now.Add(-time.Minute) }
	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": "GLM-5.3", "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}, nil)
	if response.Code != http.StatusOK || paid.Load() != 1 {
		t.Fatalf("status=%d paid=%d", response.Code, paid.Load())
	}
}

// A Coding Plan channel keeps serving OpenAI-shaped clients: ccLoad translates
// them locally to the Anthropic wire the ZCode endpoint expects.
func TestProxy_ZAICodingPlanTranslatesOpenAIClients(t *testing.T) {
	t.Parallel()

	var upstreamPath string
	var upstreamBody []byte
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"glm-4.7","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer upstream.Close()

	credentialJSON, err := (&zaiauth.Credential{APIKey: "key-id.secret"}).JSON()
	if err != nil {
		t.Fatalf("encode z.ai credential: %v", err)
	}
	env := setupProxyTestEnv(t, []testChannel{{
		name: "zai-openai-client", upstreamProtocol: "anthropic", models: "glm-4.7",
		authType: model.AuthTypeZAIOAuth, oauthCredential: credentialJSON,
	}}, map[int]string{0: upstream.URL})

	response := doProxyRequest(t, env.engine, "/v1/chat/completions", map[string]any{
		"model":    "glm-4.7",
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("proxy status = %d body = %s", response.Code, response.Body.String())
	}
	if !strings.HasSuffix(upstreamPath, "/v1/messages") {
		t.Fatalf("upstream path = %q", upstreamPath)
	}
	var request map[string]any
	if err := json.Unmarshal(upstreamBody, &request); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	metadata, _ := request["metadata"].(map[string]any)
	if _, ok := metadata["user_id"].(string); !ok {
		t.Fatalf("translated request is missing the ZCode fingerprint: %s", upstreamBody)
	}
}

/**
 * @brief 套餐失效后，在途旧采样不得恢复已被拒绝的体验资格。
 * @param t 测试上下文。
 */
func TestProxy_ZAIInvalidationDuringRefresh(t *testing.T) {
	var balanceCalls, trialCalls, paidCalls atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/biz/subscription/list":
			_, _ = io.WriteString(w, `{"code":0,"data":[{"status":"VALID"}]}`)
		case "/api/v1/zcode-plan/billing/balance":
			n := balanceCalls.Add(1)
			if n == 2 {
				close(started)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			if n >= 3 {
				_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[],"balances":[]}}`)
				return
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":"active"}],"balances":[{"plan_id":"gift","show_name":"GLM-5.3","remaining_units":10}]}}`)
		case "/api/v1/zcode-plan/anthropic/v1/messages":
			if trialCalls.Add(1) >= 2 {
				w.WriteHeader(401)
				_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","message":"invalid JWT"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		case "/api/monitor/usage/quota/limit":
			_, _ = io.WriteString(w, `{"code":0,"data":{"limits":[{"type":"TOKENS_LIMIT","percentage":20}]}}`)
		case "/v1/messages":
			paidCalls.Add(1)
			_, _ = io.WriteString(w, `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		}
	}))
	env := setupProxyTestEnv(t, []testChannel{{name: "refresh race", upstreamProtocol: "anthropic", models: "GLM-5.3,GLM-5.3-Trial", authType: model.AuthTypeZAIOAuth, oauthCredential: `{"api_key":"key.secret","jwt_token":"jwt"}`}}, map[int]string{0: upstream.URL})
	env.server.zaiService.OAuthBaseURL, env.server.zaiService.BizBaseURL = upstream.URL+"/api/v1", upstream.URL
	env.server.zaiService.QuotaLimitURL = upstream.URL + "/api/monitor/usage/quota/limit"
	request := func() {
		response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": "GLM-5.3", "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "hello"}}}, nil)
		if response.Code != 200 {
			t.Fatalf("request failed: %d %s", response.Code, response.Body.String())
		}
	}
	request()
	configs, err := env.store.ListConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path := fmt.Sprintf("/admin/channels/%d/oauth-usage", configs[0].ID)
	c, refresh := newTestContext(t, newRequest(http.MethodPost, path, nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(configs[0].ID)}}
	done := make(chan struct{})
	go func() { env.server.HandleOAuthUsage(c); close(done) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not start")
	}
	request()
	// 旧刷新仍挂起时，新请求必须查询当前代次，不能加入旧查询而等待它返回。
	next := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		next <- doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": "GLM-5.3", "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "hello"}}}, nil)
	}()
	select {
	case response := <-next:
		if response.Code != http.StatusOK {
			t.Fatalf("request after invalidation failed: %d %s", response.Code, response.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("new request joined the invalidated refresh")
	}
	close(release)
	select {
	case <-done:
		if refresh.Code != http.StatusOK {
			t.Fatalf("refresh failed: %d %s", refresh.Code, refresh.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not finish")
	}
	// 管理端也必须收到新资格，不能仅阻止旧查询写缓存却仍返回旧快照。
	var refreshed struct {
		Data struct {
			Plans *zaiauth.Plans `json:"zai_plans"`
		} `json:"data"`
	}
	if err := json.Unmarshal(refresh.Body.Bytes(), &refreshed); err != nil {
		t.Fatal(err)
	}
	if refreshed.Data.Plans == nil || !refreshed.Data.Plans.GiftKnown || len(refreshed.Data.Plans.Gifts) != 0 {
		t.Fatalf("refresh returned invalidated eligibility: %s", refresh.Body.String())
	}
	request()
	if balanceCalls.Load() != 3 || trialCalls.Load() != 2 || paidCalls.Load() != 3 {
		t.Fatalf("stale refresh restored rejected eligibility: balances=%d trial=%d paid=%d", balanceCalls.Load(), trialCalls.Load(), paidCalls.Load())
	}
}

/**
 * @brief 两站的付费请求与体验失败回退均直连站点，体验仍走公共入口且认证材料隔离。
 * @param t 测试上下文。
 */
func TestProxy_ZAISiteEndpoints(t *testing.T) {
	for _, site := range []struct{ name, base, host string }{
		{"zai", "https://api.z.ai/api/anthropic", "api.z.ai"},
		{"bigmodel", "https://open.bigmodel.cn/api/anthropic", "open.bigmodel.cn"},
	} {
		for _, route := range []string{"paid", "trial", "fallback"} {
			t.Run(site.name+"/"+route, func(t *testing.T) {
				freeFirst := route != "paid"
				credential := &zaiauth.Credential{Site: zaiauth.Site(site.name), APIKey: "paid-key", JWTToken: "gift-jwt", FreeFirst: &freeFirst}
				raw, err := credential.JSON()
				if err != nil {
					t.Fatal(err)
				}
				env := setupProxyTestEnv(t, []testChannel{{name: "site endpoints", upstreamProtocol: "anthropic", models: "GLM-5.3-Flash", authType: model.AuthTypeZAIOAuth, oauthCredential: raw}}, map[int]string{0: site.base})
				var paidCalls, trialCalls int
				env.server.client = &http.Client{Transport: oauthUsageRoundTripper(func(r *http.Request) (*http.Response, error) {
					switch r.URL.Path {
					case "/api/biz/subscription/list":
						if r.URL.Host != site.host {
							t.Errorf("subscription host=%s", r.URL.Host)
						}
						return jsonResponse(r, `{"code":0,"data":[{"status":"VALID"}]}`)
					case "/api/v1/zcode-plan/billing/balance":
						return jsonResponse(r, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":"active"}],"balances":[{"plan_id":"gift","show_name":"GLM-5.3-Flash","remaining_units":10}]}}`)
					case "/api/v1/zcode-plan/anthropic/v1/messages":
						trialCalls++
						if r.URL.Host != "zcode.z.ai" || anthropicHeaderValue(r.Header, "Authorization") != "Bearer gift-jwt" || anthropicHeaderValue(r.Header, "x-api-key") != "" {
							t.Errorf("invalid gift URL/auth: %s", r.URL)
						}
						if route == "fallback" {
							return jsonResponseStatus(r, 503, `{"error":{"type":"overloaded_error","message":"unavailable"}}`)
						}
					case "/api/anthropic/v1/messages":
						paidCalls++
						if r.URL.String() != site.base+"/v1/messages" || anthropicHeaderValue(r.Header, "Authorization") != "Bearer paid-key" || anthropicHeaderValue(r.Header, "x-api-key") != "paid-key" {
							t.Errorf("invalid paid URL/auth: %s", r.URL)
						}
						body, err := io.ReadAll(r.Body)
						if err != nil {
							return nil, err
						}
						var request struct {
							Model  string `json:"model"`
							System []struct {
								Text string `json:"text"`
							} `json:"system"`
						}
						if err := json.Unmarshal(body, &request); err != nil || request.Model != "GLM-5.3-Flash" || len(request.System) != 1 || request.System[0].Text != "caller-system" {
							t.Errorf("paid body was changed by gift packaging: %s, err=%v", body, err)
						}
					default:
						return nil, fmt.Errorf("unexpected endpoint: %s", r.URL)
					}
					return jsonResponse(r, `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3-Flash","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
				})}
				env.server.zaiService = zaiauth.NewService(env.server.client)
				response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": "GLM-5.3-Flash", "max_tokens": 16, "system": "caller-system", "messages": []any{map[string]string{"role": "user", "content": "hello"}}}, nil)
				if response.Code != 200 {
					t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
				}
				wantPaid, wantTrial := 1, 1
				if route == "paid" {
					wantTrial = 0
				}
				if route == "trial" {
					wantPaid = 0
				}
				if paidCalls != wantPaid || trialCalls != wantTrial {
					t.Fatalf("paid=%d trial=%d", paidCalls, trialCalls)
				}
			})
		}
	}
}

/**
 * @brief Trial 传输失败仍记录实际发送模型，不把路由别名写入失败日志。
 * @param t 测试上下文。
 */
func TestProxy_ZAITrialTransportErrorModel(t *testing.T) {
	env := setupProxyTestEnv(t, []testChannel{{name: "trial transport", upstreamProtocol: "anthropic", models: "GLM-5.3-Flash-Trial", authType: model.AuthTypeZAIOAuth, oauthCredential: `{"jwt_token":"trial-jwt","user_id":"trial-user"}`}}, map[int]string{0: "https://paid.invalid"})
	var calls atomic.Int32
	env.server.client = &http.Client{Transport: oauthUsageRoundTripper(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/v1/zcode-plan/billing/balance":
			return jsonResponse(req, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":"active"}],"balances":[{"plan_id":"gift","capabilities":["model:glm-5.3-flash"],"remaining_units":10}]}}`)
		case "/api/v1/zcode-plan/anthropic/v1/messages":
			calls.Add(1)
			var body struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.Model != "GLM-5.3-Flash" {
				t.Errorf("wire model=%s err=%v", body.Model, err)
			}
			return nil, io.ErrUnexpectedEOF
		default:
			t.Errorf("unexpected endpoint: %s", req.URL)
			return nil, errors.New("unexpected endpoint")
		}
	})}
	env.server.zaiService = zaiauth.NewService(env.server.client)
	response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": "GLM-5.3-Flash-Trial", "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "hello"}}}, nil)
	if response.Code < http.StatusBadRequest || calls.Load() != 1 {
		t.Fatalf("status=%d sends=%d body=%s", response.Code, calls.Load(), response.Body.String())
	}
	entry := waitForProxyLogMatching(t, env, func(entry *model.LogEntry) bool { return entry.ChannelID != 0 && entry.StatusCode >= 400 })
	if entry == nil || entry.ActualModel != "GLM-5.3-Flash" || entry.Model != "GLM-5.3-Flash-Trial" || entry.Cost != 0 {
		t.Fatalf("incorrect transport failure log: %+v", entry)
	}
}

/**
 * @brief 自动回退与显式体验按真实传输故障选用相同的存储时间策略。
 * @param t 测试上下文。
 * @note 覆盖默认及自定义配置，不实际等待冷却到期。
 */
func TestProxy_ZAITrialTransportCooldownPolicy(t *testing.T) {
	for _, custom := range []bool{false, true} {
		for _, forced := range []bool{false, true} {
			for _, tc := range []struct {
				name     string        ///< 传输故障类型。
				err      error         ///< 实际发送错误。
				initial  time.Duration ///< 默认初始冷却。
				override time.Duration ///< 自定义初始冷却。
			}{
				{"connection_refused", errors.New("connection refused"), 2 * time.Minute, 137 * time.Second},
				{"first_byte_timeout", util.ErrUpstreamFirstByteTimeout, time.Minute, 23 * time.Second},
			} {
				t.Run(fmt.Sprintf("custom=%t/forced=%t/%s", custom, forced, tc.name), func(t *testing.T) {
					env, configs := newZAIStartHarness(t)
					if custom {
						env.store.ConfigureCooldown(util.CooldownSettings{ServerSec: 137, TimeoutSec: 23, MinSec: 1})
					}
					control := env.server.client
					var trial, paid atomic.Int32
					env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
						if !strings.HasSuffix(req.URL.Path, "/messages") {
							return control.Do(req)
						}
						if strings.Contains(req.URL.Path, "/zcode-plan/") {
							trial.Add(1)
							return nil, tc.err
						}
						paid.Add(1)
						return zaiStartTestResponse(req, 200, `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3-Flash","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), nil
					})}
					name := "GLM-5.3-Flash"
					if forced {
						name += "-Trial"
					}
					started := time.Now()
					result := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": name, "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "hello"}}}, map[string]string{"X-CCLoad-Channel": configs[0].Name})
					wantPaid := int32(1)
					if forced {
						wantPaid = 0
					}
					if trial.Load() != 1 || paid.Load() != wantPaid || (result.Code == 200) == forced {
						t.Fatalf("fallback: status=%d trial=%d paid=%d", result.Code, trial.Load(), paid.Load())
					}
					cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					want := tc.initial
					if custom {
						want = tc.override
					}
					until := cooldowns[configs[0].ID]["GLM-5.3-Flash-Trial"]
					// 存储截止时间精度为秒；只允许请求耗时和秒截断造成的偏差。
					if until.Before(started.Add(want-time.Second)) || until.After(time.Now().Add(want)) {
						t.Fatalf("wrong cooldown policy: remaining=%v want=%v", time.Until(until), want)
					}
					if !cooldowns[configs[3].ID]["GLM-5.3-Flash-Trial"].Equal(until) || cooldowns[configs[0].ID]["GLM-5.3-Flash"].After(time.Now()) {
						t.Fatalf("cooldown crossed account or paid boundary: %v", cooldowns)
					}
				})
			}
		}
	}
}

/**
 * @brief 模拟读取冷却失败的存储，条件写入仍可用以暴露错误后的盲写。
 */
type zaiQuotaReadErrorStore struct {
	storage.Store ///< 真实测试存储。
}

/**
 * @brief 冷却读取返回存储错误，禁止调用方将失败解释为空冷却。
 * @param ctx 存储操作上下文。
 * @return 无可用快照，始终 nil。
 * @return 模拟的存储读取错误。
 */
func (s *zaiQuotaReadErrorStore) GetAllModelCooldowns(ctx context.Context) (map[int64]map[string]time.Time, error) {
	return nil, errors.New("model cooldown read unavailable")
}

/**
 * @brief 读取暂停失败时不新建或延长冷却，自动体验仍可回退付费。
 * @param t 测试上下文。
 */
func TestZAIQuotaReadErrorPreservesCooldown(t *testing.T) {
	env, configs := newZAIStartHarness(t)
	cfg := configs[0]
	env.server.store = &zaiQuotaReadErrorStore{Store: env.store}
	until := time.Now().Add(30 * time.Second).Truncate(time.Second)
	var paidCalls int
	resp, err := callZAIStart(context.Background(), env.server, cfg, "GLM-5.3-Flash", func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Path, "/zcode-plan/") {
			// 先通过本地准入，再模拟其他请求刚写入的账号暂停。
			if err := env.store.SetModelCooldown(context.Background(), cfg.ID, "GLM-5.3-Flash-Trial", until); err != nil {
				t.Fatal(err)
			}
			return zaiStartTestResponse(req, 429, `{"code":1113,"msg":"quota exhausted"}`), nil
		}
		paidCalls++
		return zaiStartTestResponse(req, 200, `{}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || paidCalls != 1 || isZAITrialResponse(resp) {
		t.Fatalf("paid fallback: status=%d calls=%d", resp.StatusCode, paidCalls)
	}
	cooldowns, err := env.store.GetAllModelCooldowns(context.Background())
	if err != nil || !cooldowns[cfg.ID]["GLM-5.3-Flash-Trial"].Equal(until) {
		t.Fatalf("read failure extended cooldown: %v err=%v", cooldowns, err)
	}
	if len(cooldowns[configs[3].ID]) != 0 {
		t.Fatalf("read failure created duplicate cooldown: %v", cooldowns[configs[3].ID])
	}
}

/**
 * @brief 在身份核对与实际存储写入之间插入换账号操作，复现旧错误污染新账号。
 */
type zaiQuotaSwapStore struct {
	storage.Store           ///< 真实测试存储。
	target        int64     ///< 要替换账号的渠道。
	beforeWrite   func()    ///< 写入前触发的凭证变更。
	once          sync.Once ///< 只替换一次。
}

/**
 * @brief 验证体验额度失效、账号变更和付费路径之间的隔离。
 */
func (s *zaiQuotaSwapStore) SetModelCooldownIfOAuthCredential(ctx context.Context, id int64, authType, credential, name string, until time.Time) (bool, error) {
	if id == s.target {
		s.once.Do(s.beforeWrite)
	}
	return s.Store.SetModelCooldownIfOAuthCredential(ctx, id, authType, credential, name, until)
}

/**
 * @brief 在原子递增前复现凭证替换竞争。
 * @param ctx 存储上下文。
 * @param id 目标渠道。
 * @param authType 发送时认证类型。
 * @param credential 发送时凭证。
 * @param name 模型键。
 * @param now 故障时间。
 * @param status 上游状态码。
 * @return 冷却截止时间，身份变化为零值。
 * @return 身份匹配且写入为 true。
 * @return 存储错误，否则 nil。
 */
func (s *zaiQuotaSwapStore) BumpModelCooldownIfOAuthCredential(ctx context.Context, id int64, authType, credential, name string, now time.Time, status int) (time.Time, bool, error) {
	if id == s.target {
		s.once.Do(s.beforeWrite)
	}
	return s.Store.BumpModelCooldownIfOAuthCredential(ctx, id, authType, credential, name, now, status)
}

/**
 * @brief 在条件清理前插入凭证变更，验证成功恢复的身份竞争边界。
 * @param ctx 存储上下文。
 * @param id 目标渠道。
 * @param authType 预期认证类型。
 * @param credential 成功请求的凭证快照。
 * @param name 体验模型键。
 * @param cutoff 成功请求发送时间。
 * @return 实际删除旧历史为 true，否则 false。
 * @return 存储错误，否则 nil。
 */
func (s *zaiQuotaSwapStore) ResetExpiredModelCooldownIfOAuthCredential(ctx context.Context, id int64, authType, credential, name string, cutoff time.Time) (bool, error) {
	if id == s.target {
		s.once.Do(s.beforeWrite)
	}
	return s.Store.ResetExpiredModelCooldownIfOAuthCredential(ctx, id, authType, credential, name, cutoff)
}

/**
 * @brief 验证体验额度失效、账号变更和付费路径之间的隔离。
 */
func TestZAIQuotaIdentityWriteRace(t *testing.T) {
	for _, tc := range []struct {
		name, credential string
		status, calls    int
	}{
		{"new-account", `{"api_key":"key-new.secret","jwt_token":"jwt-new","user_id":"new"}`, 200, 1},
		{"same-account-refresh", `{"api_key":"key-a.secret","jwt_token":"jwt-refreshed","user_id":"a"}`, 429, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, configs := newZAIStartHarness(t)
			wrapper := &zaiQuotaSwapStore{Store: env.store, target: configs[0].ID}
			wrapper.beforeWrite = func() {
				replacement := configs[0].Clone()
				replacement.OAuthCredential = tc.credential
				current, err := env.store.GetConfig(context.Background(), replacement.ID)
				if err != nil {
					t.Fatal(err)
				}
				if ok, err := env.store.CompareAndSwapOAuthCredential(context.Background(), replacement.ID, current.AuthType, current.OAuthCredential, replacement.OAuthCredential); err != nil || !ok {
					t.Fatalf("swap failed: %t %v", ok, err)
				}
			}
			env.server.store = wrapper
			resp, err := callZAIStart(context.Background(), env.server, configs[0], "GLM-5.3-Flash", func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Path, "/zcode-plan/") {
					return zaiStartTestResponse(req, 429, `{"code":1113,"msg":"quota exhausted"}`), nil
				}
				return zaiStartTestResponse(req, 200, `{}`), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			replacement, err := env.store.GetConfig(context.Background(), configs[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if replacement.OAuthCredential != tc.credential {
				t.Fatal("credential replacement did not occur")
			}
			env.server.InvalidateChannelListCache()
			calls := 0
			resp, err = callZAIStart(context.Background(), env.server, replacement, "GLM-5.3-Flash-Trial", func(req *http.Request) (*http.Response, error) {
				calls++
				return zaiStartTestResponse(req, 200, `{}`), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.status || calls != tc.calls {
				t.Fatalf("account cooldown isolation: status=%d upstream_calls=%d", resp.StatusCode, calls)
			}
		})
	}
}

/**
 * @brief 验证体验额度失效、账号变更和付费路径之间的隔离。
 */
func TestZAIPaidDuplicateCacheIsolation(t *testing.T) {
	for _, tc := range []struct {
		name             string
		index            int
		quota, freeFirst bool
	}{
		{"cached", 3, false, false},
		{"duplicate-paid", 3, true, false},
		{"source-paid", 0, true, false},
		{"duplicate-auto", 3, true, true},
		{"source-auto", 0, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, configs := newZAIStartHarness(t)
			paidCfg := configs[tc.index].Clone()
			paidCfg.OAuthCredential = `{"api_key":"key-a.secret","jwt_token":"jwt-a","user_id":"a","free_first":false}`
			send := func(req *http.Request) (*http.Response, error) { return zaiStartTestResponse(req, 200, `{}`), nil }
			warm, err := callZAIStart(context.Background(), env.server, paidCfg, "GLM-5.3-Flash", send)
			if err != nil {
				t.Fatal(err)
			}
			_ = warm.Body.Close()
			if isZAITrialResponse(warm) {
				t.Fatal("warmup was not paid")
			}
			if tc.quota {
				resp, err := callZAIStart(context.Background(), env.server, configs[0], "GLM-5.3-Flash", func(req *http.Request) (*http.Response, error) {
					if strings.Contains(req.URL.Path, "/zcode-plan/") {
						return zaiStartTestResponse(req, 429, `{"code":1113,"msg":"quota exhausted"}`), nil
					}
					return send(req)
				})
				if err != nil {
					t.Fatal(err)
				}
				_ = resp.Body.Close()
			}
			if tc.freeFirst {
				paidCfg.OAuthCredential = `{"api_key":"key-a.secret","jwt_token":"jwt-a","user_id":"a","free_first":true}`
			}
			blocking := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
			env.server.zaiService.OAuthBaseURL = blocking.URL + "/api/v1"
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			calls := 0
			resp, err := callZAIStart(ctx, env.server, paidCfg, "GLM-5.3-Flash", func(req *http.Request) (*http.Response, error) { calls++; return send(req) })
			if resp != nil {
				_ = resp.Body.Close()
			}
			if err != nil || calls != 1 {
				t.Fatalf("paid request delayed by duplicate Trial invalidation: calls=%d err=%v", calls, err)
			}
		})
	}
}

/**
 * @brief 验证体验额度失效、账号变更和付费路径之间的隔离。
 */
func TestZAILatestModelEligibility(t *testing.T) {
	env, configs := newZAIStartHarness(t)
	// 两个模型原本都已授权，只更新其中一个模型的余额，不能借重复渠道新增模型资格。
	initial := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":"active"}],"balances":[{"plan_id":"gift","bucket_id":"flash","capabilities":["model:glm-5.3-flash"],"remaining_units":10},{"plan_id":"gift","bucket_id":"other","capabilities":["model:glm-5.3"],"remaining_units":10}]}}`)
	}))
	env.server.zaiService.OAuthBaseURL = initial.URL + "/api/v1"

	send := func(req *http.Request) (*http.Response, error) { return zaiStartTestResponse(req, 200, `{}`), nil }
	resp, err := callZAIStart(context.Background(), env.server, configs[3], "GLM-5.3-Flash-Trial", send)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	balance := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":"active"}],"balances":[{"plan_id":"gift","bucket_id":"flash","capabilities":["model:glm-5.3-flash"],"remaining_units":0},{"plan_id":"gift","bucket_id":"other","capabilities":["model:glm-5.3"],"remaining_units":10}]}}`)
	}))
	env.server.zaiService.OAuthBaseURL = balance.URL + "/api/v1"
	credential, err := zaiauth.ParseCredential([]byte(configs[0].OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.server.zaiAccountPlans(context.Background(), configs[0], credential, true); err != nil {
		t.Fatal(err)
	}
	calls := 0
	resp, err = callZAIStart(context.Background(), env.server, configs[3], "GLM-5.3-Flash-Trial", func(req *http.Request) (*http.Response, error) { calls++; return send(req) })
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if calls != 0 || resp.StatusCode != 429 {
		t.Fatalf("exhausted model reused from stale duplicate: calls=%d status=%d", calls, resp.StatusCode)
	}
	// 部分模型耗尽不得误伤同账号其他仍有额度的模型。
	calls = 0
	resp, err = callZAIStart(context.Background(), env.server, configs[3], "GLM-5.3-Trial", func(req *http.Request) (*http.Response, error) { calls++; return send(req) })
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if calls != 1 || resp.StatusCode != 200 {
		t.Fatalf("other model incorrectly rejected: calls=%d status=%d", calls, resp.StatusCode)
	}
}

/**
 * @brief 代理与管理检测恢复同账号的过期体验退避，不清付费模型状态。
 * @param t 测试上下文。
 */
func TestProxy_ZAITrialSuccessResetsExpiredHistory(t *testing.T) {
	for _, mode := range []string{"proxy", "admin", "admin_wait"} {
		for _, change := range []struct {
			name       string        ///< 恢复过程中的凭证变化。
			credential string        ///< 新凭证；空值表示不变。
			backoff    time.Duration ///< 当前渠道下次失败的初始退避。
		}{
			{"unchanged", "", time.Minute},
			{"refresh", `{"api_key":"key-a.secret","jwt_token":"jwt-refreshed","user_id":"a"}`, time.Minute},
			{"preference", `{"api_key":"key-a.secret","jwt_token":"jwt-a","user_id":"a","free_first":false}`, time.Minute},
			{"new_account", `{"api_key":"key-new.secret","jwt_token":"jwt-new","user_id":"new"}`, 2 * time.Minute},
		} {
			t.Run(mode+"/"+change.name, func(t *testing.T) {
				env, configs := newZAIStartHarness(t)
				ctx := context.Background()
				const name = "GLM-5.3-Flash-Trial"
				past := time.Now().Add(-time.Hour)
				for _, index := range []int{0, 3} {
					if _, err := env.store.BumpModelCooldown(ctx, configs[index].ID, name, past, 429); err != nil {
						t.Fatal(err)
					}
					if err := env.store.SetModelCooldown(ctx, configs[index].ID, "GLM-5.3-Flash", time.Now().Add(time.Hour)); err != nil {
						t.Fatal(err)
					}
				}
				if change.credential != "" {
					// 在读取目标之后、事务校验之前更新凭证，成功结果仍属于原账号。
					wrapper := &zaiQuotaSwapStore{Store: env.store, target: configs[0].ID}
					wrapper.beforeWrite = func() {
						if ok, err := env.store.CompareAndSwapOAuthCredential(ctx, configs[0].ID, configs[0].AuthType, configs[0].OAuthCredential, change.credential); err != nil || !ok {
							t.Fatalf("credential change: matched=%t err=%v", ok, err)
						}
					}
					env.server.store = wrapper
				}
				control := env.server.client
				env.server.client = &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
					if !strings.HasSuffix(req.URL.Path, "/messages") {
						return control.Do(req)
					}
					return zaiStartTestResponse(req, 200, `{"id":"m","type":"message","role":"assistant","model":"GLM-5.3-Flash","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), nil
				})}
				switch mode {
				case "admin_wait":
					result := env.server.executeChannelTestWithCooldown(ctx, configs[0], cooldown.NoKeyIndex, "key-a.secret", &testutil.TestChannelRequest{Model: name, Content: "hello", ClientProtocol: "anthropic", WaitForCapacity: true}, true)
					if result["success"] != true {
						t.Fatalf("wait test failed: %v", result)
					}
				case "admin":
					path := fmt.Sprintf("/admin/channels/%d/test", configs[0].ID)
					req := newJSONRequest(t, http.MethodPost, path, map[string]any{"model": name, "content": "hello", "client_protocol": "anthropic"})
					c, response := newTestContext(t, req)
					c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(configs[0].ID)}}
					env.server.HandleChannelTest(c)
					if !mustParseAPIResponse[map[string]any](t, response.Body.Bytes()).Data["success"].(bool) {
						t.Fatal(response.Body.String())
					}
				default:
					response := doProxyRequest(t, env.engine, "/v1/messages", map[string]any{"model": name, "max_tokens": 16, "messages": []map[string]string{{"role": "user", "content": "hello"}}}, map[string]string{"X-CCLoad-Channel": configs[0].Name})
					if response.Code != 200 {
						t.Fatalf("success=%d %s", response.Code, response.Body.String())
					}
				}
				for _, index := range []int{0, 3} {
					duration, err := env.store.BumpModelCooldown(ctx, configs[index].ID, name, time.Now(), 429)
					want := time.Minute
					if index == 0 {
						want = change.backoff
					}
					if err != nil || duration != want {
						t.Fatalf("recovered backoff=%v want=%v err=%v", duration, want, err)
					}
				}
				models, err := env.store.GetAllModelCooldowns(ctx)
				if err != nil || !models[configs[0].ID]["GLM-5.3-Flash"].After(time.Now()) {
					t.Fatalf("paid state cleared: %v %v", models, err)
				}
			})
		}
	}
}
