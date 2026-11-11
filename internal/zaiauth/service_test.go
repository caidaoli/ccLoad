package zaiauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

/**
 * @brief 授权轮询按 HTTP 状态及阶段选择容错，空状态保留 pending、JWT-only ready 可用。
 * @param t 测试上下文。
 */
func TestPollErrorAndStateContract(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		readError bool
		want      PollStatus
		retry     bool
	}{
		{"missing_data", 200, `{"code":0}`, false, PollPending, false},
		{"null_data", 200, `{"code":0,"data":null}`, false, PollPending, false},
		{"empty_status", 200, `{"code":0,"data":{}}`, false, PollPending, false},
		{"jwt_ready", 200, `{"code":0,"data":{"status":"ready","token":"jwt"}}`, false, PollReady, false},
		{"failed", 200, `{"code":0,"data":{"status":"failed"}}`, false, PollFailed, false},
		{"bad_json", 200, `broken`, false, "", true},
		{"non_object", 200, `null`, false, "", true},
		{"bad_data", 200, `{"code":0,"data":[]}`, false, "", true},
		{"business", 200, `{"code":123,"msg":"busy"}`, false, "", true},
		{"bad_ready", 200, `{"code":0,"data":{"status":"ready"}}`, false, "", false},
		{"unknown", 200, `{"code":0,"data":{"status":"other"}}`, false, "", false},
		{"not_found", 404, `broken`, false, "", false},
		{"auth_read", 401, ``, true, "", false},
		{"retry_read", 200, ``, true, "", true},
		{"timeout", 408, `broken`, false, "", true},
		{"limited", 429, `broken`, false, "", true},
		{"server", 503, `broken`, false, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := NewService(&http.Client{Transport: siteTestTransport(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("X-Device-Mid") != "login-device" {
					t.Error("poll device missing")
				}
				var body io.Reader = strings.NewReader(tc.body)
				if tc.readError {
					body = iotest.ErrReader(io.ErrUnexpectedEOF)
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(body), Request: req}, nil
			})})
			result, err := service.Poll(context.Background(), "flow", "token", "login-device")
			if tc.want != "" {
				if err != nil || result.Status != tc.want {
					t.Fatalf("result=%v err=%v", result, err)
				}
				return
			}
			var failure *OAuthError
			if !errors.As(err, &failure) || failure.Retryable() != tc.retry || failure.StatusCode != tc.status {
				t.Fatalf("error=%v", err)
			}
			if tc.status == 404 && !errors.Is(err, ErrOAuthFlowUnavailable) {
				t.Fatal("lost unavailable cause")
			}
		})
	}
}

/**
 * @brief 环境输入只按明确来源包装，locale 的编码后缀和未知值按契约规范。
 * @param t 测试上下文。
 */
func TestStartEnvironmentAndLocale(t *testing.T) {
	for input, want := range map[string]string{"en_US.UTF-8": "en-US", "zh_CN": "zh-CN", "C": "unknown", "": "unknown", "!!!": "unknown"} {
		if got := normalizeClientLocale(input); got != want {
			t.Errorf("locale %q=%q want %q", input, got, want)
		}
	}
	for _, env := range []StartEnvironment{{WorkDir: "/remote/project", Git: "yes"}, {WorkDir: "/remote/project", Git: "no"}, {Git: "yes"}, {WorkDir: "/remote/project", Git: "invalid"}} {
		body, err := PrepareStartBody([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "GLM-5.3", "device", "session", SiteZAI, env)
		if err != nil {
			t.Fatal(err)
		}
		var request struct {
			System []struct {
				Text string `json:"text"`
			} `json:"system"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		text := request.System[2].Text
		if strings.Contains(text, "Primary working directory:") != (env.WorkDir != "") || strings.Contains(text, "Is a git repository:") != (env.WorkDir != "" && (env.Git == "yes" || env.Git == "no")) {
			t.Fatal("environment presence mismatch")
		}
		platform, arch, _ := strings.Cut(ClientPlatform(), "-")
		if !strings.Contains(text, "OS Version: "+platform+" "+hostRelease()+" "+arch) {
			t.Fatal("OS line missing platform/release/arch")
		}
	}
}

/**
 * @brief 在保留真实目标域名的情况下验证站点协议。
 */
type siteTestTransport func(*http.Request) (*http.Response, error)

/**
 * @brief ZCode 体验请求必须使用 IANA 时区，不能发送平台本地名称。
 * @param t 测试上下文。
 */
func TestStartHeadersTimezone(t *testing.T) {
	t.Parallel()
	zone := StartHeaders("jwt", "session").Get("X-Client-Timezone")
	t.Logf("X-Client-Timezone = %q", zone)
	if zone == "unknown" {
		return
	}
	if zone == "" || zone == "Local" {
		t.Fatalf("X-Client-Timezone = %q, want an IANA timezone", zone)
	}
	if _, err := time.LoadLocation(zone); err != nil {
		t.Fatalf("X-Client-Timezone = %q: %v", zone, err)
	}
}

/**
 * @brief ZCode 体验请求必须使用桌面端身份，并按真实用户轮次稳定派生查询 ID。
 * @param t 测试上下文。
 */
func TestStartWireIdentityAndQueryID(t *testing.T) {
	t.Parallel()
	if title := StartHeaders("jwt", "session-1").Get("X-Title"); title != "Z Code@electron" {
		t.Fatalf("X-Title = %q, want Z Code@electron", title)
	}
	first := StartQueryID([]byte(`{"messages":[{"role":"user","content":"hello"}]}`), "session-1")
	if first != "c8a30663-ac0d-412f-8a80-d875944d3ff5" {
		t.Fatalf("first query ID = %q", first)
	}
	toolContinuation := StartQueryID([]byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"tool_use"}]},{"role":"user","content":[{"type":"tool_result","content":"done"}]}]}`), "session-1")
	if toolContinuation != first {
		t.Fatalf("tool continuation changed query ID: %q != %q", toolContinuation, first)
	}
	next := StartQueryID([]byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"reply"},{"role":"user","content":[{"type":"text","text":"next"}]}]}`), "session-1")
	if next == "" || next == first {
		t.Fatalf("next user turn query ID = %q, first = %q", next, first)
	}
	if got := StartQueryID([]byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","content":"done"}]}]}`), "session-1"); got != "" {
		t.Fatalf("tool-only request query ID = %q, want empty", got)
	}
}

/**
 * @brief 执行测试请求处理器。
 * @param r 出站请求。
 * @return 模拟响应。
 * @return 处理器返回的错误。
 */
func (f siteTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

/**
 * @brief 付费和体验查询并发且独立失败，调用方取消仍终止整个查询。
 * @param t 测试上下文。
 */
func TestFetchPlansIndependentSources(t *testing.T) {
	for _, timeoutPaid := range []bool{true, false} {
		t.Run(fmt.Sprintf("paid_timeout=%v", timeoutPaid), func(t *testing.T) {
			paidStarted, giftStarted := make(chan struct{}), make(chan struct{})
			client := &http.Client{Timeout: 100 * time.Millisecond, Transport: siteTestTransport(func(r *http.Request) (*http.Response, error) {
				paid := strings.HasSuffix(r.URL.Path, "/subscription/list")
				other := paidStarted
				if paid {
					close(paidStarted)
					other = giftStarted
				} else {
					close(giftStarted)
				}
				select {
				case <-other:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				if paid == timeoutPaid {
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				body := `{"code":0,"data":{"plans":[],"balances":[]}}`
				if paid {
					body = `{"code":0,"data":[{"status":"VALID","productName":"Pro"}]}`
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})}
			plans, err := NewService(client).FetchPlans(context.Background(), &Credential{APIKey: "key", JWTToken: "jwt"})
			if err != nil || plans == nil || plans.PaidKnown == timeoutPaid || plans.GiftKnown != timeoutPaid || plans.Paid == timeoutPaid || len(plans.Warnings) != 1 {
				t.Fatalf("lost independent source: plans=%+v err=%v", plans, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plans, err := NewService(&http.Client{Transport: siteTestTransport(func(r *http.Request) (*http.Response, error) {
		return nil, r.Context().Err()
	})}).FetchPlans(ctx, &Credential{APIKey: "key", JWTToken: "jwt"})
	if !errors.Is(err, context.Canceled) || plans != nil {
		t.Fatalf("cancellation ignored: plans=%+v err=%v", plans, err)
	}
}

/**
 * @brief 合并查询时按已流逝时间判断套餐边界，未发放权益不会变成真实余额。
 * @param t 测试上下文。
 */
func TestFetchPlansAdvancesGiftTimeWhileWaiting(t *testing.T) {
	giftReturned := make(chan struct{})
	client := &http.Client{Transport: siteTestTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"code":0,"data":{"plans":[{"plan_id":"future","status":"active","starts_at":2001},{"plan_id":"expired","status":"active","ends_at":2001},{"plan_id":"ungranted","status":"active","starts_at":2001}],"balances":[{"plan_id":"future","show_name":"glm-future","remaining_units":10},{"plan_id":"expired","show_name":"glm-expired","remaining_units":10}]}}`
		if strings.HasSuffix(r.URL.Path, "/subscription/list") {
			<-giftReturned
			time.Sleep(1200 * time.Millisecond)
			body = `{"code":0,"data":[]}`
		} else {
			defer close(giftReturned)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Date": []string{time.Unix(2000, 0).UTC().Format(http.TimeFormat)}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	plans, err := NewService(client).FetchPlans(context.Background(), &Credential{APIKey: "key", JWTToken: "jwt"})
	if err != nil || plans == nil || plans.ObservedAt < 2001 || len(plans.Gifts) != 3 {
		t.Fatalf("stale sampling clock: plans=%+v err=%v", plans, err)
	}
	for i, want := range []string{"active", "expired", "ungranted"} {
		if plans.Gifts[i].Status != want {
			t.Fatalf("gift %d status=%s want=%s", i, plans.Gifts[i].Status, want)
		}
	}
	if plans.Gifts[2].Granted || plans.Gifts[2].Remaining != nil {
		t.Fatal("activation invented an issued balance")
	}
	if _, available := plans.GiftModel("glm-future"); !available {
		t.Fatal("issued bucket remained pending")
	}
	if _, available := plans.GiftModel("glm-expired"); available {
		t.Fatal("expired bucket remained available")
	}
}

/**
 * @brief 国内业务使用原始令牌，付费查询使用国内 API，体验仍使用 ZCode。
 * @param t 测试上下文。
 */
func TestBigModelServiceWireContract(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: siteTestTransport(func(r *http.Request) (*http.Response, error) {
		host, auth, body := "open.bigmodel.cn", "key.secret", ""
		switch r.URL.Path {
		case "/api/biz/customer/getCustomerInfo":
			host, auth = "bigmodel.cn", "domestic-access"
			body = `{"code":0,"data":{"userId":"cn-user","organizations":[{"organizationId":"team-only","projects":[{"projectId":"team","projectType":2}]},{"organizationId":"org","projects":[{"projectId":"team","projectName":"默认项目","projectType":"2"},{"projectId":"project","projectType":1}]}]}}`
		case "/api/biz/v1/organization/org/projects/project/api_keys":
			host, auth = "bigmodel.cn", "domestic-access"
			body = `{"code":0,"data":[{"name":"zcode-api-key","apiKey":"key"}]}`
		case "/api/biz/v1/organization/org/projects/project/api_keys/copy/key":
			host, auth = "bigmodel.cn", "domestic-access"
			body = `{"code":0,"data":{"secretKey":"secret"}}`
		case "/api/biz/subscription/list":
			body = `{"code":200,"data":[]}`
		case "/api/v1/zcode-plan/billing/balance":
			host, auth = "zcode.z.ai", "Bearer trial-jwt"
			if r.Header.Get("X-Device-Mid") != "device" {
				t.Errorf("missing device header")
			}
			body = `{"code":0,"data":{"plans":[],"balances":[]}}`
		case "/api/monitor/usage/quota/limit":
			body = `{"code":200,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":25}]}}`
		case "/api/coding/paas/v4/models", "/api/paas/v4/models":
			auth = "Bearer key.secret"
			body = `{"data":[{"id":"glm-cn"}]}`
		case "/api.json":
			host, auth = "models.dev", ""
			body = `{"zhipuai-coding-plan":{"models":{"glm-cn":{"release_date":"2026-01-01"}}},"zai-coding-plan":{"models":{"glm-global":{}}}}`
		default:
			t.Errorf("unexpected request %s", r.URL)
			body = `{"code":1}`
		}
		if r.URL.Host != host || r.Header.Get("Authorization") != auth {
			t.Errorf("wrong site/auth: %s %q", r.URL, r.Header.Get("Authorization"))
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	service := NewService(client).ForSite(SiteBigModel, client)
	ctx := context.Background()
	key, identity, err := service.ResolveCodingPlanAPIKey(ctx, "domestic-access")
	if err != nil || key != "key.secret" || identity.UserID != "cn-user" {
		t.Fatalf("resolve: %q %+v %v", key, identity, err)
	}
	plans, err := service.FetchPlans(ctx, &Credential{Site: SiteBigModel, APIKey: key, JWTToken: "trial-jwt", DeviceID: "device"})
	if err != nil || !plans.PaidKnown || plans.Paid || !plans.GiftKnown {
		t.Fatalf("empty plans: %+v %v", plans, err)
	}
	limits, err := service.FetchQuotaLimits(ctx, key)
	if err != nil || len(limits) != 1 || limits[0].UsedPercent != 25 {
		t.Fatalf("quota: %+v %v", limits, err)
	}
	models, err := service.ListModels(ctx, key)
	if err != nil || len(models) != 1 || models[0] != "glm-cn" {
		t.Fatalf("models: %v %v", models, err)
	}
	models, err = service.ListCommunityModels(ctx)
	if err != nil || len(models) != 1 || models[0] != "glm-cn" {
		t.Fatalf("community: %v %v", models, err)
	}
	for _, site := range []Site{SiteZAI, SiteBigModel} {
		body, err := PrepareStartBody([]byte(`{"model":"glm","messages":[{"role":"user","content":"hello"}]}`), "GLM-5.3", "device", "session", site, StartEnvironment{})
		if err != nil {
			t.Fatal(err)
		}
		var request struct {
			System []struct {
				Text string `json:"text"`
			} `json:"system"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		provider := "zai-api"
		if site == SiteBigModel {
			provider = "bigmodel-api"
		}
		found := false
		for _, part := range request.System {
			if strings.Contains(part.Text, provider) {
				found = true
			}
		}
		if !found {
			t.Errorf("trial system missing provider %s", provider)
		}
	}
}

/**
 * @brief 国内登录设置 provider 与 redirect，轮询不串用国际令牌且允许仅体验 JWT。
 * @param t 测试上下文。
 */
func TestBigModelOAuthContract(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, data, token string
		invalid           bool
	}{
		{"snake", `"bigmodel":{"access_token":"cn"},"zai":{"access_token":"wrong"}`, "cn", false},
		{"camel", `"bigmodel":{"accessToken":"cn"}`, "cn", false},
		{"trial only", `"token":"trial"`, "", false},
		{"wrong site", `"zai":{"access_token":"wrong"}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer poll" {
					t.Errorf("invalid OAuth auth")
				}
				if strings.HasSuffix(r.URL.Path, "/init") {
					var payload map[string]string
					if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload["provider"] != "bigmodel" {
						t.Errorf("provider: %v %v", payload, err)
					}
					_, _ = io.WriteString(w, `{"code":0,"data":{"flow_id":"cn","authorize_url":"https://bigmodel.cn/login?keep=1"}}`)
				} else {
					_, _ = io.WriteString(w, `{"code":0,"data":{"status":"ready",`+tc.data+`}}`)
				}
			}))
			service = service.ForSite(SiteBigModel, nil)
			flow, err := service.InitFlow(context.Background(), "poll", "device")
			if err != nil {
				t.Fatal(err)
			}
			authorize, err := url.Parse(flow.AuthorizeURL)
			if err != nil {
				t.Fatal(err)
			}
			back, err := url.Parse(authorize.Query().Get("redirect"))
			if err != nil || back.Host != "zcode.z.ai" || back.Path != "/app/oauth/login" || back.Query().Get("redirect") != "zcode://oauth/callback" || back.Query().Get("app_version") != AppVersion || authorize.Query().Get("keep") != "1" || authorize.Query().Has("redirect_uri") {
				t.Fatalf("callback %s %v", flow.AuthorizeURL, err)
			}
			result, err := service.Poll(context.Background(), flow.FlowID, "poll", "device")
			if tc.invalid {
				if err == nil {
					t.Fatal("accepted international token")
				}
				return
			}
			if err != nil || result.AccessToken != tc.token || result.Status != PollReady {
				t.Fatalf("poll %+v %v", result, err)
			}
		})
	}
}

func newTestService(t *testing.T, handler http.Handler) (*Service, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	service := NewService(server.Client())
	service.OAuthBaseURL = server.URL + "/api/v1"
	service.BizBaseURL = server.URL
	service.CodingModelsURL = server.URL + "/api/coding/paas/v4/models"
	service.ModelsURL = server.URL + "/api/paas/v4/models"
	service.CommunityURL = server.URL + "/api.json"
	service.QuotaLimitURL = server.URL + "/api/monitor/usage/quota/limit"
	return service, server
}

/**
 * @brief 套餐查询按账号订阅和各余额桶判断，多桶与字符串数值不会丢失。
 * @param t 测试上下文。
 */
func TestFetchPlansSeparatesSubscriptionAndGiftBuckets(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", time.Unix(2000, 0).UTC().Format(http.TimeFormat))
		if r.URL.Path == "/api/biz/subscription/list" {
			_, _ = io.WriteString(w, `{"code":200,"data":[]}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":"0","data":{"server_time":"4000","plans":[{"plan_id":"a","user_plan_id":"one","status":"active","ends_at":"3000"},{"plan_id":"a","user_plan_id":"two","status":"active","ends_at":3000},{"plan_id":"future","status":"active","starts_at":"2500"},{"plan_id":"old","status":"active","ends_at":"1999"}],"balances":[{"plan_id":"a","user_plan_id":"one","capabilities":["model:glm-5.3"],"total_units":"10","used_units":10,"remaining_units":"0"},{"plan_id":"a","user_plan_id":"two","capabilities":["MODEL:glm-5.3"],"remaining_units":5},{"plan_id":"future","show_name":"GLM-4.7","remaining_units":10},{"plan_id":"old","capabilities":["model:old"],"remaining_units":10}]}}`)
	}))
	plans, err := service.FetchPlans(context.Background(), &Credential{APIKey: "key", JWTToken: "jwt", DeviceID: "device"})
	if err != nil {
		t.Fatal(err)
	}
	if plans.ObservedAt != 2000 || plans.Paid || !plans.PaidKnown || !plans.GiftKnown || len(plans.Gifts) != 4 {
		t.Fatalf("plans=%+v", plans)
	}
	if raw, ok := plans.GiftModel("GLM-5.3-Trial"); !ok || raw != "glm-5.3" {
		t.Fatalf("available bucket was hidden: %q %v", raw, ok)
	}
	for _, name := range []string{"GLM-4.7", "old"} {
		if _, ok := plans.GiftModel(name); ok {
			t.Fatalf("inactive gift %s was usable", name)
		}
	}
}

/**
 * @brief 体验套餐没有余额桶时仍可查询，空套餐与无效响应不能混为一谈。
 * @param t 测试上下文。
 */
func TestFetchPlansWithoutBalanceBuckets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		data   string
		known  bool
		status string
	}{
		{"pending empty balances", `{"plans":[{"plan_id":"future","name":"Trial","status":"active","starts_at":2500,"ends_at":3000}],"balances":[]}`, true, "pending"},
		{"pending missing balances", `{"plans":[{"plan_id":"future","name":"Trial","status":"active","starts_at":2500,"ends_at":3000}]}`, true, "pending"},
		{"pending null balances", `{"plans":[{"plan_id":"future","name":"Trial","status":"active","starts_at":2500,"ends_at":3000}],"balances":null}`, true, "pending"},
		{"unmatched instance", `{"plans":[{"plan_id":"a","user_plan_id":"future","name":"Trial","status":"active","starts_at":2500}],"balances":[{"plan_id":"a","user_plan_id":"other","show_name":"GLM-4.7","remaining_units":10}]}`, true, "pending"},
		{"active without quota", `{"plans":[{"plan_id":"a","name":"Trial","status":"active"}],"balances":[]}`, true, "ungranted"},
		{"disabled future quota", `{"plans":[{"plan_id":"a","name":"Trial","status":"disabled","starts_at":2500}],"balances":[]}`, true, "inactive"},
		{"expired without quota", `{"plans":[{"plan_id":"a","name":"Trial","status":"active","ends_at":1999}],"balances":[]}`, true, "expired"},
		{"inactive without quota", `{"plans":[{"plan_id":"a","name":"Trial","status":"disabled"}],"balances":[]}`, true, "inactive"},
		{"empty plans", `{"plans":[],"balances":[]}`, true, ""},
		{"missing plans", `{"balances":[]}`, false, ""},
		{"invalid balances", `{"plans":[],"balances":{}}`, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Date", time.Unix(2000, 0).UTC().Format(http.TimeFormat))
				_, _ = io.WriteString(w, `{"code":0,"data":`+tc.data+`}`)
			}))
			plans, err := service.FetchPlans(context.Background(), &Credential{JWTToken: "jwt"})
			if err != nil {
				t.Fatal(err)
			}
			if plans.PaidKnown || plans.GiftKnown != tc.known || (len(plans.Warnings) == 1) != tc.known {
				t.Fatalf("query state = %+v", plans)
			}
			if tc.status == "" {
				if len(plans.Gifts) != 0 {
					t.Fatalf("unexpected gifts: %+v", plans.Gifts)
				}
				return
			}
			if len(plans.Gifts) != 1 {
				t.Fatalf("missing plan: %+v", plans.Gifts)
			}
			gift := plans.Gifts[0]
			if gift.Name != "Trial" || gift.Status != tc.status || gift.Total != nil || gift.Used != nil || gift.Remaining != nil || len(gift.Models) != 0 {
				t.Fatalf("plan without quota = %+v", gift)
			}
			if tc.status == "pending" && gift.StartsAt != 2500 {
				t.Fatalf("start time lost: %+v", gift)
			}
			if _, usable := plans.GiftModel("GLM-4.7"); usable {
				t.Fatal("plan without a model balance must not authorize inference")
			}
		})
	}
}

/**
 * @brief 套餐已开始时仍以模型权益 effective_at 判断生效，余额未发放不能用于调用。
 * @param t 测试上下文。
 */
func TestFetchPlansEntitlementEffectiveAt(t *testing.T) {
	t.Parallel()
	const startsAt int64 = 1793876604
	const effectiveAt int64 = 1793890800
	for _, tc := range []struct {
		name   string
		now    int64
		start  int64
		bucket bool
		status string
		usable bool
	}{
		{"real pending response without bucket", 1793887788, startsAt, false, "pending", false},
		{"bucket granted before effective time", effectiveAt - 1, startsAt, true, "pending", false},
		{"effective boundary with bucket", effectiveAt, startsAt, true, "active", true},
		{"effective boundary without bucket", effectiveAt, startsAt, false, "ungranted", false},
		{"entitlement time takes precedence", effectiveAt, effectiveAt + 60, true, "active", true},
		{"expired entitlement", 1793926800, startsAt, true, "expired", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			balances := []any{}
			if tc.bucket {
				balances = append(balances, map[string]any{"plan_id": "trial", "user_plan_id": "account-plan", "entitlement_id": "flash", "bucket_id": "bucket", "capabilities": []string{"model:glm-5.3-flash"}, "remaining_units": 200, "total_units": 300, "used_units": 100})
			}
			payload, err := json.Marshal(map[string]any{"code": 0, "data": map[string]any{
				"server_time": tc.now,
				"plans": []any{map[string]any{"plan_id": "trial", "user_plan_id": "account-plan", "name": "ZCODE GLOBAL BUILD", "status": "active", "starts_at": tc.start, "ends_at": 1793926800,
					"entitlements": []any{map[string]any{"entitlement_id": "flash", "show_name": "GLM-5.3-Flash", "capabilities": []string{"model:glm-5.3-flash"}, "unit_type": "token", "grant_units": 300000000, "effective_at": "1793890800"}}}},
				"balances": balances,
			}})
			if err != nil {
				t.Fatal(err)
			}
			service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Date", time.Unix(tc.now, 0).UTC().Format(http.TimeFormat))
				_, _ = w.Write(payload)
			}))
			plans, err := service.FetchPlans(context.Background(), &Credential{JWTToken: "jwt"})
			if err != nil || !plans.GiftKnown || len(plans.Gifts) != 1 {
				t.Fatalf("plans=%+v err=%v", plans, err)
			}
			gift := plans.Gifts[0]
			if gift.PlanKey == "" || gift.EntitlementID != "flash" || gift.UnitType != "token" || gift.GrantUnits == nil || *gift.GrantUnits != 300000000 {
				t.Fatalf("planned quota metadata lost: %+v", gift)
			}
			if gift.Granted != tc.bucket {
				t.Fatalf("granted=%v, want %v", gift.Granted, tc.bucket)
			}
			if gift.StartsAt != effectiveAt || gift.ExpiresAt != 1793926800 || gift.Status != tc.status || gift.DisplayName != "GLM-5.3-Flash" {
				t.Fatalf("entitlement = %+v", gift)
			}
			if tc.bucket && (len(gift.Models) != 1 || gift.Models[0] != "glm-5.3-flash") {
				t.Fatalf("bucket models = %v", gift.Models)
			}
			if !tc.bucket && len(gift.Models) != 0 {
				t.Fatalf("ungranted entitlement must not advertise callable models: %v", gift.Models)
			}
			if !tc.bucket && (gift.Total != nil || gift.Used != nil || gift.Remaining != nil) {
				t.Fatalf("grant_units is not a balance: %+v", gift)
			}
			if _, usable := plans.GiftModel("GLM-5.3-Flash-Trial"); usable != tc.usable {
				t.Fatalf("usable=%v want=%v", usable, tc.usable)
			}
		})
	}
}

/**
 * @brief 同一套餐不同权益独立关联余额和生效时间，已发放桶不隐藏未来权益。
 * @param t 测试上下文。
 */
func TestFetchPlansSeparatesEntitlements(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Date", time.Unix(2000, 0).UTC().Format(http.TimeFormat))
		_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"trial","status":"active","starts_at":1000,"ends_at":4000,"entitlements":[{"entitlement_id":"first","show_name":"GLM-4.7","effective_at":1500},{"entitlement_id":"later","show_name":"GLM-5.3-Flash","effective_at":3000}]}],"balances":[{"plan_id":"trial","entitlement_id":"first","bucket_id":"one","show_name":"GLM-4.7","remaining_units":10}]}}`)
	}))
	plans, err := service.FetchPlans(context.Background(), &Credential{JWTToken: "jwt"})
	if err != nil || !plans.GiftKnown || len(plans.Gifts) != 2 {
		t.Fatalf("plans=%+v err=%v", plans, err)
	}
	if plans.Gifts[0].Status != "active" || plans.Gifts[0].StartsAt != 1500 || plans.Gifts[1].Status != "pending" || plans.Gifts[1].StartsAt != 3000 {
		t.Fatalf("entitlements = %+v", plans.Gifts)
	}
	if _, usable := plans.GiftModel("GLM-4.7"); !usable {
		t.Fatal("active entitlement was hidden")
	}
	if _, usable := plans.GiftModel("GLM-5.3-Flash"); usable {
		t.Fatal("future entitlement was usable")
	}
}

/**
 * @brief 权益缺少 effective_at 时沿用套餐开始时间，未开始的套餐不能用于推理。
 * @param t 测试上下文。
 */
func TestFetchPlansEntitlementWithoutEffectiveAtUsesPlanStart(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Date", time.Unix(2000, 0).UTC().Format(http.TimeFormat))
		_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"trial","status":"active","starts_at":3000,"entitlements":[{"entitlement_id":"first","show_name":"GLM-4.7"}]}],"balances":[{"plan_id":"trial","entitlement_id":"first","bucket_id":"one","show_name":"GLM-4.7","remaining_units":10}]}}`)
	}))
	plans, err := service.FetchPlans(context.Background(), &Credential{JWTToken: "jwt"})
	if err != nil || len(plans.Gifts) != 1 || plans.Gifts[0].StartsAt != 3000 || plans.Gifts[0].Status != "pending" {
		t.Fatalf("plans=%+v err=%v", plans, err)
	}
	if _, usable := plans.GiftModel("GLM-4.7"); usable {
		t.Fatal("plan that has not started authorized inference")
	}
}

/**
 * @brief 体验控制面以 401/403 拒绝 JWT 时提示重新登录，其他失败仍按可重试的查询失败处理。
 * @param t 测试上下文。
 */
func TestFetchPlansReportsRejectedLogin(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status   int
		rejected bool
		warning  string
	}{
		{http.StatusUnauthorized, true, "ZCode 登录已失效，请重新登录"},
		{http.StatusForbidden, true, "ZCode 登录已失效，请重新登录"},
		{http.StatusBadGateway, false, "体验套餐查询失败"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			t.Parallel()
			service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"code":401,"msg":"token revoked"}`)
			}))
			plans, err := service.FetchPlans(context.Background(), &Credential{JWTToken: "jwt"})
			if err != nil || plans.GiftKnown || plans.JWTRejected != tc.rejected || !slices.Contains(plans.Warnings, tc.warning) {
				t.Fatalf("plans=%+v err=%v", plans, err)
			}
		})
	}
}

/**
 * @brief 周期窗口与停用状态限制实际可调用桶，独立桶不能相互覆盖。
 * @param t 测试上下文。
 */
func TestFetchPlansBucketWindows(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Date", time.Unix(2000, 0).UTC().Format(http.TimeFormat))
		_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"active","status":"active","starts_at":1000,"ends_at":5000},{"plan_id":"disabled","status":"disabled","starts_at":1000,"ends_at":1500}],"balances":[{"plan_id":"active","bucket_id":"future","show_name":"future","period_start":"2500","period_end":3000,"remaining_units":10},{"plan_id":"active","bucket_id":"past","show_name":"past","period_start":1000,"period_end":"2000","remaining_units":10},{"plan_id":"active","bucket_id":"current","show_name":"current","period_start":2000,"period_end":3000,"remaining_units":10},{"plan_id":"disabled","bucket_id":"disabled","show_name":"disabled","remaining_units":10}]}}`)
	}))
	plans, err := service.FetchPlans(context.Background(), &Credential{JWTToken: "jwt"})
	if err != nil || !plans.GiftKnown || len(plans.Gifts) != 4 {
		t.Fatalf("plans=%+v err=%v", plans, err)
	}
	for i, status := range []string{"pending", "expired", "active", "inactive"} {
		gift := plans.Gifts[i]
		if gift.Status != status || !gift.Granted {
			t.Fatalf("gift=%+v want=%s", gift, status)
		}
		if _, usable := plans.GiftModel(gift.Models[0]); usable != (status == "active") {
			t.Fatalf("model %s usable=%v", gift.Models[0], usable)
		}
	}
}

/**
 * @brief 同名套餐实例、同模型的独立桶和多模型共享桶保持各自额度与截止时间。
 * @param t 测试上下文。
 */
func TestFetchPlansKeepsPlanInstancesAndBucketsSeparate(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Date", time.Unix(2000, 0).UTC().Format(http.TimeFormat))
		_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[
			{"plan_id":"trial","user_plan_id":"one","name":"Trial","status":"active","ends_at":5000,"entitlements":[{"entitlement_id":"model","effective_at":1000,"unit_type":"token"},{"entitlement_id":"future","effective_at":3000,"grant_units":"300000000","unit_type":"token","show_name":"Future"}]},
			{"plan_id":"trial","user_plan_id":"two","name":"Trial","status":"active","ends_at":6000}],
			"balances":[
			{"plan_id":"trial","user_plan_id":"one","entitlement_id":"model","bucket_id":"shared","capabilities":["model:glm-5.3","model:glm-5.3-flash"],"total_units":100,"used_units":20,"remaining_units":80,"expires_at":4000},
			{"plan_id":"trial","user_plan_id":"one","entitlement_id":"model","bucket_id":"extra","capabilities":["model:glm-5.3"],"total_units":200,"remaining_units":50,"expires_at":4500},
			{"plan_id":"trial","user_plan_id":"two","bucket_id":"shared","capabilities":["model:glm-5.3"],"unit_type":"request","total_units":10,"remaining_units":3,"expires_at":5500}]}}`)
	}))
	plans, err := service.FetchPlans(context.Background(), &Credential{JWTToken: "jwt"})
	if err != nil || !plans.GiftKnown || len(plans.Gifts) != 4 {
		t.Fatalf("plans=%+v err=%v", plans, err)
	}
	shared, extra, future, other := plans.Gifts[0], plans.Gifts[1], plans.Gifts[2], plans.Gifts[3]
	if shared.PlanKey == "" || shared.PlanKey != extra.PlanKey || shared.PlanKey != future.PlanKey || shared.PlanKey == other.PlanKey || shared.ID == other.ID {
		t.Fatalf("plan instances merged: %+v", plans.Gifts)
	}
	if shared.BucketID != "shared" || len(shared.Models) != 2 || shared.Total == nil || *shared.Total != 100 || shared.Remaining == nil || *shared.Remaining != 80 || shared.ExpiresAt != 4000 || shared.UnitType != "token" {
		t.Fatalf("shared bucket changed: %+v", shared)
	}
	if extra.BucketID != "extra" || extra.Total == nil || *extra.Total != 200 || extra.Remaining == nil || *extra.Remaining != 50 || extra.ExpiresAt != 4500 || extra.EntitlementID != shared.EntitlementID {
		t.Fatalf("independent bucket merged: %+v", extra)
	}
	if other.Total == nil || *other.Total != 10 || other.Remaining == nil || *other.Remaining != 3 || other.ExpiresAt != 5500 || other.UnitType != "request" {
		t.Fatalf("other plan quota changed: %+v", other)
	}
	if future.Status != "pending" || future.GrantUnits == nil || *future.GrantUnits != 300000000 || future.Remaining != nil || future.Total != nil || future.Used != nil || len(future.Models) != 0 {
		t.Fatalf("planned grant treated as balance: %+v", future)
	}
}

/**
 * @brief 缺少实例 ID 时只允许唯一套餐归属，歧义桶不得重复分配或用于调用。
 * @param t 测试上下文。
 */
func TestFetchPlansRequiresUniqueBucketOwner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		plans    string
		bucket   string
		assigned int
		warning  bool
	}{
		{"ambiguous product", `[{"plan_id":"a","user_plan_id":"one","status":"active"},{"plan_id":"a","user_plan_id":"two","status":"active"}]`, `{"plan_id":"a"}`, 0, true},
		{"unique product", `[{"plan_id":"a","user_plan_id":"one","status":"active"}]`, `{"plan_id":"a"}`, 1, false},
		{"explicit instance", `[{"plan_id":"a","user_plan_id":"one","status":"active"},{"plan_id":"a","user_plan_id":"two","status":"active"}]`, `{"plan_id":"a","user_plan_id":"two"}`, 1, false},
		{"explicit instance before product fallback", `[{"plan_id":"a","status":"active"},{"plan_id":"a","user_plan_id":"two","status":"active"}]`, `{"plan_id":"a","user_plan_id":"two"}`, 1, false},
		{"ambiguous missing instance identities", `[{"plan_id":"a","status":"active"},{"plan_id":"a","status":"active"}]`, `{"plan_id":"a"}`, 0, true},
		{"unknown instance", `[{"plan_id":"a","user_plan_id":"one","status":"active"}]`, `{"plan_id":"a","user_plan_id":"other"}`, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var bucket map[string]any
			if err := json.Unmarshal([]byte(tc.bucket), &bucket); err != nil {
				t.Fatal(err)
			}
			bucket["remaining_units"], bucket["capabilities"] = 100, []string{"model:glm-5.3"}
			payload, err := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"plans": json.RawMessage(tc.plans), "balances": []any{bucket}}})
			if err != nil {
				t.Fatal(err)
			}
			service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(payload)
			}))
			plans, err := service.FetchPlans(context.Background(), &Credential{JWTToken: "jwt"})
			if err != nil || !plans.GiftKnown {
				t.Fatalf("plans=%+v err=%v", plans, err)
			}
			assigned := 0
			planKeys := make(map[string]bool)
			for _, gift := range plans.Gifts {
				if gift.PlanKey == "" || planKeys[gift.PlanKey] {
					t.Fatalf("distinct plan entries merged: %+v", plans.Gifts)
				}
				planKeys[gift.PlanKey] = true
				if gift.Remaining != nil {
					assigned++
				}
			}
			// JWT-only 查询另有一条付费来源未知提示，不属于桶歧义。
			if assigned != tc.assigned || (len(plans.Warnings) > 1) != tc.warning {
				t.Fatalf("assigned=%d plans=%+v", assigned, plans)
			}
			if _, usable := plans.GiftModel("glm-5.3"); usable != (tc.assigned > 0) {
				t.Fatalf("ambiguous bucket authorized inference: %v", usable)
			}
		})
	}
}

/**
 * @brief 国际站点使用 provider=zai、redirect_uri 和对应的轮询令牌。
 * @param t 测试上下文。
 */
func TestInitFlowAndPollFollowZCodeContract(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer poll-token" ||
			r.Header.Get("User-Agent") != "ZCode/"+AppVersion ||
			r.Header.Get("X-ZCode-App-Version") != AppVersion {
			t.Errorf("unexpected identity headers: %v", r.Header)
		}
		switch r.URL.Path {
		case "/api/v1/oauth/cli/init":
			body, _ := io.ReadAll(r.Body)
			var payload map[string]string
			if json.Unmarshal(body, &payload) != nil || payload["provider"] != "zai" {
				t.Errorf("init body = %s", body)
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"flow_id":"flow-1","poll_token":"poll-token","authorize_url":"https://zcode.z.ai/oauth/x","expires_at":1800000000,"poll_interval_sec":2}}`)
		case "/api/v1/oauth/cli/poll/flow-1":
			_, _ = io.WriteString(w, `{"code":0,"data":{"status":"ready","token":"jwt","user":{"user_id":"u-1","email":"user@example.com","name":"User"},"zai":{"access_token":"zai-access"}}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))

	flow, err := service.InitFlow(context.Background(), "poll-token", "device")
	if err != nil {
		t.Fatalf("InitFlow() error = %v", err)
	}
	authorize, parseErr := url.Parse(flow.AuthorizeURL)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	back, parseErr := url.Parse(authorize.Query().Get("redirect_uri"))
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	if authorize.Host != "zcode.z.ai" || authorize.Path != "/oauth/x" || back.Host != "zcode.z.ai" || back.Path != "/app/oauth/login" || back.Query().Get("redirect") != "zcode://oauth/callback" || back.Query().Get("app_version") != AppVersion {
		t.Fatalf("invalid callback: %s", flow.AuthorizeURL)
	}
	if flow.FlowID != "flow-1" || flow.PollIntervalSec != 2 {
		t.Fatalf("flow = %+v", flow)
	}
	result, err := service.Poll(context.Background(), flow.FlowID, "poll-token", "device")
	if err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if result.Status != PollReady || result.AccessToken != "zai-access" || result.JWTToken != "jwt" ||
		result.Identity.UserID != "u-1" || result.Identity.Email != "user@example.com" {
		t.Fatalf("result = %+v", result)
	}
}

func TestPollReportsPendingAndFailed(t *testing.T) {
	t.Parallel()
	status := "pending"
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"code":0,"data":{"status":"`+status+`"}}`)
	}))
	result, err := service.Poll(context.Background(), "flow-1", "poll-token", "device")
	if err != nil || result.Status != PollPending {
		t.Fatalf("pending poll = %+v err = %v", result, err)
	}
	status = "failed"
	result, err = service.Poll(context.Background(), "flow-1", "poll-token", "device")
	if err != nil || result.Status != PollFailed {
		t.Fatalf("failed poll = %+v err = %v", result, err)
	}
}

// The hosted CLI OAuth endpoint is enabled per ZCode release. An empty 404 must
// surface as an upstream availability fact so the admin UI can offer the
// Coding Plan key path instead of reporting a ccLoad bug.
func TestInitFlowReportsUnavailableOnEmptyNotFound(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	_, err := service.InitFlow(context.Background(), "poll-token", "device")
	if !errors.Is(err, ErrOAuthFlowUnavailable) {
		t.Fatalf("InitFlow() error = %v", err)
	}
}

func TestInitFlowRejectsBusinessErrors(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"code":1001,"msg":"provider disabled"}`)
	}))
	_, err := service.InitFlow(context.Background(), "poll-token", "device")
	if err == nil || !strings.Contains(err.Error(), "provider disabled") {
		t.Fatalf("InitFlow() error = %v", err)
	}
}

func TestResolveCodingPlanAPIKeyReusesZCodeKey(t *testing.T) {
	t.Parallel()
	var createdKeys int
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/auth/z/login":
			body, _ := io.ReadAll(r.Body)
			var payload map[string]string
			if json.Unmarshal(body, &payload) != nil || payload["token"] != "zai-access" {
				t.Errorf("login body = %s", body)
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"access_token":"biz-token"}}`)
		case r.URL.Path == "/api/biz/customer/getCustomerInfo":
			if r.Header.Get("Authorization") != "Bearer biz-token" {
				t.Errorf("customer info authorization = %q", r.Header.Get("Authorization"))
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"userId":"u-1","email":"user@example.com","organizations":[{"organizationId":"org-other","organizationName":"Team","projects":[{"projectId":"p-x","projectName":"Other"}]},{"organizationId":"org-1","organizationName":"默认机构","projects":[{"projectId":"p-other","projectName":"Other"},{"projectId":"p-1","projectName":"默认项目"}]}]}}`)
		case r.URL.Path == "/api/biz/v1/organization/org-1/projects/p-1/api_keys" && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"code":0,"data":[{"name":"other","apiKey":"other-id"},{"name":"zcode-api-key","apiKey":"key-id"}]}`)
		case r.URL.Path == "/api/biz/v1/organization/org-1/projects/p-1/api_keys" && r.Method == http.MethodPost:
			createdKeys++
			_, _ = io.WriteString(w, `{"code":0,"data":{"apiKey":"created-id"}}`)
		case r.URL.Path == "/api/biz/v1/organization/org-1/projects/p-1/api_keys/copy/key-id":
			_, _ = io.WriteString(w, `{"code":0,"data":{"secretKey":"secret"}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))

	apiKey, identity, err := service.ResolveCodingPlanAPIKey(context.Background(), "zai-access")
	if err != nil {
		t.Fatalf("ResolveCodingPlanAPIKey() error = %v", err)
	}
	if apiKey != "key-id.secret" {
		t.Fatalf("api key = %q", apiKey)
	}
	if identity.UserID != "u-1" || identity.Email != "user@example.com" {
		t.Fatalf("identity = %+v", identity)
	}
	if createdKeys != 0 {
		t.Fatalf("existing ZCode key must be reused, created %d", createdKeys)
	}
}

func TestResolveCodingPlanAPIKeyCreatesMissingKey(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/auth/z/login":
			_, _ = io.WriteString(w, `{"code":0,"data":{"accessToken":"biz-token"}}`)
		case r.URL.Path == "/api/biz/customer/getCustomerInfo":
			_, _ = io.WriteString(w, `{"code":0,"data":{"organizations":[{"organizationId":"org-1","organizationName":"Org","projects":[{"projectId":"p-1","projectName":"Project"}]}]}}`)
		case strings.HasSuffix(r.URL.Path, "/api_keys") && r.Method == http.MethodGet:
			_, _ = io.WriteString(w, `{"code":0,"data":[]}`)
		case strings.HasSuffix(r.URL.Path, "/api_keys") && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "zcode-api-key") {
				t.Errorf("create body = %s", body)
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"apiKey":"created-id"}}`)
		case strings.HasSuffix(r.URL.Path, "/copy/created-id"):
			_, _ = io.WriteString(w, `{"code":0,"data":{"secretKey":"created-secret"}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	apiKey, _, err := service.ResolveCodingPlanAPIKey(context.Background(), "zai-access")
	if err != nil {
		t.Fatalf("ResolveCodingPlanAPIKey() error = %v", err)
	}
	if apiKey != "created-id.created-secret" {
		t.Fatalf("api key = %q", apiKey)
	}
}

// The Coding Plan catalog is authoritative and the general API only fills in
// what it misses: models reach the plan before the general API lists them.
func TestListModelsPrefersCodingPlanCatalog(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key.secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/api/coding/paas/v4/models":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"glm-5.3"},{"id":"glm-4.7"}]}`)
		case "/api/paas/v4/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"glm-4.7"},{"id":"glm-4.6"}]}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	models, err := service.ListModels(context.Background(), "key.secret")
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(models) != 3 || models[0] != "glm-5.3" || models[1] != "glm-4.7" || models[2] != "glm-4.6" {
		t.Fatalf("models = %v", models)
	}
}

func TestListModelsSurvivesOneUnavailableCatalog(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/coding/paas/v4/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, `{"code":0,"data":[{"model":"glm-4.7"}]}`)
	}))
	models, err := service.ListModels(context.Background(), "key.secret")
	if err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if len(models) != 1 || models[0] != "glm-4.7" {
		t.Fatalf("models = %v", models)
	}
}

// models.dev is the keyless tier: it tracks the plan lineup (glm-5.3 landed
// there on release day) and answers newest first.
func TestListCommunityModelsReadsCodingPlanProvider(t *testing.T) {
	t.Parallel()
	payload := `{"zai":{"models":{"glm-5.2":{"release_date":"2026-06-13"}}},` +
		`"zai-coding-plan":{"models":{` +
		`"glm-4.7":{"release_date":"2025-12-22"},` +
		`"glm-5.3":{"release_date":"2026-08-14"},` +
		`"glm-5.2":{"release_date":"2026-06-13"}}}}`
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, payload)
	}))
	models, err := service.ListCommunityModels(context.Background())
	if err != nil {
		t.Fatalf("ListCommunityModels() error = %v", err)
	}
	if len(models) != 3 || models[0] != "glm-5.3" || models[1] != "glm-5.2" || models[2] != "glm-4.7" {
		t.Fatalf("models = %v", models)
	}
}

func TestParseCommunityCatalogRejectsMissingProvider(t *testing.T) {
	t.Parallel()
	if _, err := parseCommunityCatalog([]byte(`{"openai":{"models":{"gpt":{}}}}`), CommunityCatalogProvider); err == nil {
		t.Fatal("a catalog without the provider must be an error")
	}
	if _, err := parseCommunityCatalog([]byte(`{"zai-coding-plan":{"models":{}}}`), CommunityCatalogProvider); err == nil {
		t.Fatal("an empty provider must be an error")
	}
}

func TestListModelsReportsRejectedKey(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	if _, err := service.ListModels(context.Background(), "key.secret"); err == nil {
		t.Fatal("ListModels() expected an error")
	}
}

func TestParseModelCatalogAcceptsUpstreamShapes(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"openai list":      `{"object":"list","data":[{"id":"glm-5.3"}]}`,
		"envelope objects": `{"code":0,"data":[{"modelId":"glm-5.3"}]}`,
		"envelope ids":     `{"code":0,"data":["glm-5.3"]}`,
		"bare list":        `[{"id":"glm-5.3"}]`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			models, err := parseModelCatalog([]byte(payload))
			if err != nil || len(models) != 1 || models[0] != "glm-5.3" {
				t.Fatalf("models = %v err = %v", models, err)
			}
		})
	}
	if _, err := parseModelCatalog([]byte(`{"data":[]}`)); err == nil {
		t.Fatal("an empty catalog must be an error")
	}
}

func TestValidateAPIKeyClassifiesRejection(t *testing.T) {
	t.Parallel()
	status := http.StatusOK
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key.secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(status)
	}))
	if err := service.ValidateAPIKey(context.Background(), "key.secret"); err != nil {
		t.Fatalf("ValidateAPIKey() error = %v", err)
	}
	status = http.StatusUnauthorized
	if err := service.ValidateAPIKey(context.Background(), "key.secret"); err == nil {
		t.Fatal("ValidateAPIKey() expected rejection")
	}
}

/**
 * @brief 旧官方付费端点可归一为直连地址，体验与自定义 URL 不被误改。
 * @param t 测试上下文。
 */
func TestNormalizeCodingPlanURL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"https://zcode.z.ai/api/v1/ultra/anthropic", "https://open.bigmodel.cn/api/anthropic"},
		{"https://zcode.z.ai/api/v1/ultra-zai/anthropic/", "https://api.z.ai/api/anthropic/"},
		{"https://zcode.z.ai/api/v1/ultra/anthropic/v1/messages#", "https://open.bigmodel.cn/api/anthropic/v1/messages#"},
		{"https://zcode.z.ai/api/v1/ultra-zai/anthropic/v1/messages?beta=true", "https://api.z.ai/api/anthropic/v1/messages?beta=true"},
		{"https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages", ""},
		{"https://custom.example.com/api/v1/ultra/anthropic", ""},
		{"https://zcode.z.ai:8443/api/v1/ultra/anthropic", ""},
		{"https://zcode.z.ai/api/v1/ultra/anthropic/custom", ""},
		{"https://api.z.ai/api/anthropic", ""},
		{"https://open.bigmodel.cn/api/anthropic", ""},
	} {
		t.Run(tc.in, func(t *testing.T) {
			want := tc.want
			if want == "" {
				want = tc.in
			}
			if got := NormalizeCodingPlanURL(tc.in); got != want {
				t.Fatalf("normalized URL = %q, want %q", got, want)
			}
		})
	}
}

/**
 * @brief 体验请求的缓存断点跳过思考块，保留正文和思考内容。
 * @param t 测试上下文。
 */
func TestStartBodyCacheSkipsThinking(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		cached  int
	}{
		{"text-before-thinking", `[{"type":"text","text":"answer"},{"type":"thinking","thinking":"reason","signature":"sig"}]`, 0},
		{"text-before-redacted", `[{"type":"text","text":"answer"},{"type":"redacted_thinking","data":"opaque"}]`, 0},
		{"only-thinking", `[{"type":"thinking","thinking":"reason","signature":"sig"},{"type":"redacted_thinking","data":"opaque"}]`, -1},
		{"text-last", `[{"type":"thinking","thinking":"reason","signature":"sig"},{"type":"text","text":"answer"}]`, 1},
		{"empty", `[]`, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":` + tc.content + `}]}`)
			wire, err := PrepareStartBody(raw, "GLM-5.3", "device", "session", SiteZAI, StartEnvironment{})
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Messages []struct {
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(wire, &got); err != nil {
				t.Fatal(err)
			}
			var original []map[string]any
			if err := json.Unmarshal([]byte(tc.content), &original); err != nil {
				t.Fatal(err)
			}
			var blocks []map[string]any
			if err := json.Unmarshal(got.Messages[len(got.Messages)-1].Content, &blocks); err != nil {
				t.Fatal(err)
			}
			if len(blocks) != len(original) {
				t.Fatalf("content length=%d want=%d", len(blocks), len(original))
			}
			for i, block := range blocks {
				_, cached := block["cache_control"]
				if cached != (i == tc.cached) {
					t.Fatalf("cache at block %d: %v", i, block)
				}
				for key, value := range original[i] {
					if block[key] != value {
						t.Fatalf("block %d field %s changed", i, key)
					}
				}
			}
		})
	}
}

/**
 * @brief 余额解析按明确剩余量、完整收支、可用量依次归一，零与未知保持区别。
 * @param t 测试上下文。
 */
func TestFetchPlansRemainingFallback(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fields    string
		remaining float64
		known     bool
		usable    bool
	}{
		{"explicit-zero", `"remaining_units":0,"total_units":10,"used_units":2,"available_units":9`, 0, true, false},
		{"explicit-positive", `"remaining_units":"3","total_units":10,"used_units":10`, 3, true, true},
		{"derived", `"total_units":"10","used_units":2,"available_units":9`, 8, true, true},
		{"derived-zero", `"total_units":10,"used_units":"10","available_units":9`, 0, true, false},
		{"overdrawn", `"total_units":10,"used_units":12`, -2, true, false},
		{"available-zero", `"available_units":0`, 0, true, false},
		{"available-positive", `"total_units":10,"available_units":"4"`, 4, true, true},
		{"null-remaining", `"remaining_units":null,"total_units":10,"used_units":2`, 8, true, true},
		{"unknown", `"total_units":10`, 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"trial","status":"active"}],"balances":[{"plan_id":"trial","capabilities":["model:glm-5.3"],`+tc.fields+`}]}}`)
			}))
			plans, err := service.FetchPlans(context.Background(), &Credential{JWTToken: "jwt", DeviceID: "device"})
			if err != nil {
				t.Fatal(err)
			}
			if !plans.GiftKnown || len(plans.Gifts) != 1 {
				t.Fatalf("unexpected plans: %+v", plans)
			}
			remaining := plans.Gifts[0].Remaining
			if (remaining != nil) != tc.known || remaining != nil && *remaining != tc.remaining {
				t.Fatalf("remaining=%v want=%v known=%v", remaining, tc.remaining, tc.known)
			}
			if _, usable := plans.GiftModel("GLM-5.3-Trial"); usable != tc.usable {
				t.Fatalf("usable=%v status=%s", usable, plans.Gifts[0].Status)
			}
		})
	}
}

func TestGeneratePollTokenMatchesZCodeShape(t *testing.T) {
	t.Parallel()
	token, err := GeneratePollToken()
	if err != nil {
		t.Fatalf("GeneratePollToken() error = %v", err)
	}
	if len(token) != pollTokenBytes*2 {
		t.Fatalf("token length = %d", len(token))
	}
	other, _ := GeneratePollToken()
	if token == other {
		t.Fatal("poll tokens must be unguessable")
	}
}
