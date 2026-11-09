package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/testutil"
	"ccLoad/internal/zaiauth"

	"github.com/gin-gonic/gin"
)

/**
 * @brief 账号渠道使用调用方给出的站点地址，并保留实际模型目录。
 * @param t 测试上下文。
 */
func TestNewZAIOAuthChannelUsesSiteEndpoint(t *testing.T) {
	t.Parallel()
	channel := newZAIOAuthChannel("Z.ai-user@example.com", `{"type":"zai","api_key":"key.secret"}`, zaiauth.CodingPlanAPIBaseURL, zaiauth.DefaultModels)
	if channel.AuthType != model.AuthTypeZAIOAuth || !channel.Enabled || channel.CostMultiplier != 1 {
		t.Fatalf("channel = %+v", channel)
	}
	if len(channel.URLs) != 1 || channel.URLs[0].URL != zaiauth.CodingPlanAPIBaseURL {
		t.Fatalf("urls = %+v", channel.URLs)
	}
	if len(channel.URLs[0].Protocols) != 1 || channel.URLs[0].Protocols[0] != "anthropic" {
		t.Fatalf("protocols = %+v", channel.URLs[0].Protocols)
	}
	if channel.ProtocolTransformMode != model.ProtocolTransformModeLocal {
		t.Fatalf("protocol transform mode = %q", channel.ProtocolTransformMode)
	}
	if len(channel.ModelEntries) != len(zaiauth.DefaultModels) {
		t.Fatalf("models = %+v", channel.ModelEntries)
	}
	routed := newZAIOAuthChannel(
		"name", `{"type":"zai","api_key":"key.secret"}`,
		"https://paid.example.com/anthropic", []string{"glm-9.9", "glm-4.7"},
	)
	if routed.URLs[0].URL != "https://paid.example.com/anthropic" {
		t.Fatalf("routed url = %q", routed.URLs[0].URL)
	}
	// A live catalog wins over the built-in lineup.
	if len(routed.ModelEntries) != 2 || routed.ModelEntries[0].Model != "glm-4.7" || routed.ModelEntries[1].Model != "glm-9.9" {
		t.Fatalf("models = %+v", routed.ModelEntries)
	}
}

/**
 * @brief 仅体验账号重新授权追加真实套餐模型，同时保留既有禁用模型与偏好。
 * @param t 测试上下文。
 */
func TestZAIReauthorizationAddsTrialModels(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.zaiService = zaiauth.NewService(server.getClientForChannel(nil))
	server.zaiCredentials = newZAICredentialManager(store, server.getClientForChannel, server.invalidateChannelRelatedCache)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/zcode-plan/billing/balance" {
			_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":"active"}],"balances":[{"plan_id":"gift","capabilities":["model:glm-5.3"],"remaining_units":10}]}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	server.zaiService.OAuthBaseURL = upstream.URL + "/api/v1"
	freeFirst := false
	raw, err := (&zaiauth.Credential{JWTToken: "old-jwt", UserID: "account", FreeFirst: &freeFirst}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	cfg := newZAIOAuthChannel("existing", raw, zaiauth.CodingPlanAPIBaseURL, []string{"GLM-4.7"})
	cfg.ModelEntries[0].Disabled = true
	created, err := store.CreateConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	updated, isNew, err := server.commitZAICredential(context.Background(), &zaiauth.Credential{JWTToken: "new-jwt", UserID: "account"}, oauthLoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if isNew || updated.ID != created.ID || len(updated.ModelEntries) != 3 {
		t.Fatalf("unexpected reauthorization: %+v new=%v", updated, isNew)
	}
	models := make(map[string]model.ModelEntry)
	for _, entry := range updated.ModelEntries {
		models[entry.Model] = entry
	}
	if !models["GLM-4.7"].Disabled || models["GLM-5.3"].Model == "" || models["GLM-5.3-Trial"].Model == "" {
		t.Fatalf("models=%+v", models)
	}
	credential, err := zaiauth.ParseCredential([]byte(updated.OAuthCredential))
	if err != nil || credential.JWTToken != "new-jwt" || credential.FreeFirst == nil || *credential.FreeFirst {
		t.Fatalf("reauthorization lost credential preference: %v", err)
	}
}

/**
 * @brief 已完成上游授权的账号不因无套餐、待发放或套餐查询失败而丢失登录。
 * @param t 测试上下文。
 */
func TestZAIAuthorizationWithoutCallableModels(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"no plan", `{"code":0,"data":{"plans":[],"balances":[]}}`},
		{"pending entitlement", `{"code":0,"data":{"plans":[{"plan_id":"trial","user_plan_id":"account-plan","status":"active","entitlements":[{"entitlement_id":"flash","show_name":"GLM-5.3-Flash","effective_at":4102444800,"grant_units":300000000}]}],"balances":[]}}`},
		{"plan lookup failed", `{"code":500,"message":"unavailable"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, store, cleanup := setupAdminTestServer(t)
			defer cleanup()
			server.zaiService = zaiauth.NewService(server.getClientForChannel(nil))
			server.zaiCredentials = newZAICredentialManager(store, server.getClientForChannel, server.invalidateChannelRelatedCache)
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/zcode-plan/billing/balance" {
					_, _ = io.WriteString(w, tc.body)
					return
				}
				w.WriteHeader(http.StatusNotFound)
			}))
			server.zaiService.OAuthBaseURL = upstream.URL + "/api/v1"
			manager := newZAIOAuthTestManager(t, &zaiOAuthTestUpstream{},
				func(context.Context, zaiOAuthOptions, string) (*zaiauth.Credential, error) {
					return nil, errors.New("no paid key")
				},
				func(ctx context.Context, options zaiOAuthOptions, credential *zaiauth.Credential) (int64, string, error) {
					cfg, _, err := server.commitZAICredential(ctx, credential, options.oauthLoginOptions)
					if err != nil {
						return 0, "", err
					}
					return cfg.ID, cfg.Name, nil
				},
			)
			defer manager.close()
			started, err := manager.start(context.Background(), "admin", zaiOAuthOptions{})
			if err != nil {
				t.Fatal(err)
			}
			status := waitForZAIOAuthStatus(t, manager, "admin", started.State)
			if status.Status != "complete" || status.ChannelID == 0 {
				t.Fatalf("authorized account was rejected: %+v", status)
			}
			cfg, err := store.GetConfig(context.Background(), status.ChannelID)
			if err != nil {
				t.Fatal(err)
			}
			credential, err := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
			if err != nil || credential.JWTToken != "jwt" || credential.UserID != "u-1" {
				t.Fatalf("authorized identity was not persisted: %v", err)
			}
			if cfg.AuthType != model.AuthTypeZAIOAuth || len(cfg.ModelEntries) != 0 {
				t.Fatalf("empty account must not acquire a fabricated catalog: auth=%s models=%v", cfg.AuthType, cfg.ModelEntries)
			}
			payload, err := json.Marshal(ChannelRequest{
				Name: "renamed", AuthType: model.AuthTypeZAIOAuth, URLs: cfg.URLs,
				Enabled: true, Models: []model.ModelEntry{}, ProxyURL: "http://localhost:7890",
				ProtocolTransformMode: model.ProtocolTransformModeLocal,
			})
			if err != nil {
				t.Fatal(err)
			}
			c, w := newTestContext(t, newRequest(http.MethodPut, "/", strings.NewReader(string(payload))))
			server.handleUpdateChannel(c, cfg.ID)
			if w.Code != http.StatusOK {
				t.Fatalf("empty-model channel cannot be edited: status=%d body=%s", w.Code, w.Body.String())
			}
			updated, err := store.GetConfig(context.Background(), cfg.ID)
			if err != nil || updated.Name != "renamed" || updated.ProxyURL != "http://localhost:7890" || updated.OAuthCredential != cfg.OAuthCredential || len(updated.ModelEntries) != 0 {
				t.Fatalf("editing empty-model channel lost configuration: %v", err)
			}

			// 手工导入同样允许空套餐，但未经上游确认的 JWT 不能伪装成已授权账号。
			imported, importErr := server.buildZAICredential(context.Background(), zaiCredentialImportRequest{JWTToken: "import-jwt"}, oauthLoginOptions{})
			if tc.name == "plan lookup failed" {
				if importErr == nil {
					t.Fatal("unverified manual JWT import was accepted")
				}
			} else if importErr != nil || imported.JWTToken != "import-jwt" {
				t.Fatalf("valid manual JWT import requires a callable plan: %v", importErr)
			}
		})
	}
}

/**
 * @brief 同一 JWT 再次导入必须重新验证上游，不能复用上次导入的成功结果。
 * @param t 测试上下文。
 */
func TestZAIRepeatedImportRevalidatesJWT(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.zaiService = zaiauth.NewService(server.getClientForChannel(nil))
	server.zaiCredentials = newZAICredentialManager(store, server.getClientForChannel, server.invalidateChannelRelatedCache)
	var revoked atomic.Bool
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/zcode-plan/billing/balance" {
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if revoked.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"code":401,"msg":"revoked"}`)
			return
		}
		_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[],"balances":[]}}`)
	}))
	server.zaiService.OAuthBaseURL = upstream.URL + "/api/v1"
	for _, valid := range []bool{true, false} {
		revoked.Store(!valid)
		c, response := newTestContext(t, newRequest(http.MethodPost, "/admin/zai/credentials/import", strings.NewReader(`{"jwt_token":"same-import-jwt"}`)))
		server.HandleImportZAICredential(c)
		want := http.StatusOK
		if !valid {
			want = http.StatusBadGateway
		}
		if response.Code != want {
			t.Fatalf("valid=%t status=%d want=%d body=%s", valid, response.Code, want, response.Body.String())
		}
	}
	configs, err := store.ListConfigs(context.Background())
	if err != nil || len(configs) != 1 {
		t.Fatalf("rejected import changed channels: count=%d err=%v", len(configs), err)
	}
}

/**
 * @brief 相同账号重新授权时重新发现模型，不复用保存渠道前的套餐采样。
 * @param t 测试上下文。
 */
func TestZAIReauthorizationUsesLatestPlans(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.zaiService = zaiauth.NewService(server.getClientForChannel(nil))
	server.zaiCredentials = newZAICredentialManager(store, server.getClientForChannel, server.invalidateChannelRelatedCache)
	var nextModel atomic.Bool
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/zcode-plan/billing/balance" {
			t.Errorf("unexpected endpoint: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		name := "glm-5.3"
		if nextModel.Load() {
			name = "glm-5.2"
		}
		_, _ = fmt.Fprintf(w, `{"code":0,"data":{"plans":[{"plan_id":"gift","status":"active"}],"balances":[{"plan_id":"gift","capabilities":["model:%s"],"remaining_units":10}]}}`, name)
	}))
	server.zaiService.OAuthBaseURL = upstream.URL + "/api/v1"
	credential := &zaiauth.Credential{JWTToken: "same-jwt", UserID: "same-user"}
	if err := credential.Normalize(); err != nil {
		t.Fatal(err)
	}
	first, _, err := server.commitZAICredential(context.Background(), credential, oauthLoginOptions{})
	if err != nil {
		t.Fatal(err)
	}
	nextModel.Store(true)
	updated, created, err := server.commitZAICredential(context.Background(), credential, oauthLoginOptions{})
	if err != nil || created || updated.ID != first.ID {
		t.Fatalf("reauthorization: cfg=%+v created=%t err=%v", updated, created, err)
	}
	models := make(map[string]bool)
	for _, entry := range updated.ModelEntries {
		models[entry.Model] = true
	}
	for _, name := range []string{"GLM-5.3", "GLM-5.3-Trial", "GLM-5.2", "GLM-5.2-Trial"} {
		if !models[name] {
			t.Fatalf("reauthorization lost model %s: %v", name, models)
		}
	}
}

/**
 * @brief 一个等待者取消不取消共享套餐查询，后续请求复用同一次结果。
 * @param t 测试上下文。
 */
func TestZAIPlanQuerySurvivesWaiterCancellation(t *testing.T) {
	server, _, cleanup := setupAdminTestServer(t)
	defer cleanup()
	server.zaiService = zaiauth.NewService(server.getClientForChannel(nil))
	started, finish := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-r.Context().Done():
			t.Error("shared query was canceled with its first waiter")
			return
		case <-finish:
		}
		_, _ = io.WriteString(w, `{"code":0,"data":{"plans":[],"balances":[]}}`)
	}))
	server.zaiService.OAuthBaseURL = upstream.URL + "/api/v1"
	cfg := &model.Config{ID: 99}
	credential := &zaiauth.Credential{JWTToken: "trial-jwt"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := server.zaiAccountPlans(ctx, cfg, credential, false); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter: %v", err)
	}
	close(finish)
	nextCtx, nextCancel := context.WithTimeout(context.Background(), time.Second)
	defer nextCancel()
	plans, err := server.zaiAccountPlans(nextCtx, cfg, credential, false)
	if err != nil || !plans.GiftKnown || calls.Load() != 1 {
		t.Fatalf("plans=%+v err=%v calls=%d", plans, err, calls.Load())
	}
}

/**
 * @brief 单字段选项保存不覆盖凭证，刷新额度和重新授权后仍保留设置。
 * @param t 测试上下文。
 */
func TestZAIOptionsPersistsWithoutReplacingCredentials(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	ctx := context.Background()
	raw, err := (&zaiauth.Credential{JWTToken: "original-jwt", APIKey: "key.secret", UserID: "user"}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := store.CreateConfig(ctx, newZAIOAuthChannel("options", raw, zaiauth.CodingPlanAPIBaseURL, []string{"GLM-5.3"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{`{"free_first":false}`, `{"free_first":"false"}`, `{"free_first":true,"jwt_token":"foreign"}`, `{"free_first":null}`} {
		c, w := newTestContext(t, newRequest(http.MethodPatch, "/", strings.NewReader(payload)))
		c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(cfg.ID)}}
		server.HandleZAIOptions(c)
		want := http.StatusBadRequest
		if payload == `{"free_first":false}` {
			want = http.StatusOK
		}
		if w.Code != want {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
	}
	persisted, err := store.GetConfig(ctx, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := parseOAuthUsageCredentialState(persisted)
	if err != nil {
		t.Fatal(err)
	}
	next, err := state.encode(json.RawMessage(`{"provider":"zai"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := zaiauth.ParseCredential([]byte(next))
	if err != nil {
		t.Fatal(err)
	}
	merged, err := credential.MergeRefresh(&zaiauth.Credential{JWTToken: "new-jwt", APIKey: "new-key"})
	if err != nil {
		t.Fatal(err)
	}
	if merged.FreeFirst == nil || *merged.FreeFirst || merged.JWTToken != "new-jwt" || credential.JWTToken != "original-jwt" {
		t.Fatal("preference or credentials were lost")
	}
}

// The routed Coding Plan endpoint carries /v1 in its path, which ordinary
// channels reject. Saving a Z.ai channel from the editor must stay possible.
func TestValidateChannelBaseURLAllowsZAIRoutedEndpoint(t *testing.T) {
	t.Parallel()
	const legacyURL = "https://zcode.z.ai/api/v1/ultra-zai/anthropic"
	normalized, err := validateChannelBaseURL(legacyURL, model.AuthTypeZAIOAuth)
	if err != nil {
		t.Fatalf("validateChannelBaseURL() error = %v", err)
	}
	if normalized != legacyURL {
		t.Fatalf("normalized = %q", normalized)
	}
	if _, err := validateChannelBaseURL(legacyURL, model.AuthTypeAPIKey); err == nil {
		t.Fatal("API key channels must keep rejecting endpoint paths")
	}
}

/**
 * @brief 同站账号重授权更新原渠道，跨站相同账号和 Key 保存为不同渠道。
 * @param t 测试上下文。
 */
func TestCreateOrUpdateZAIChannelReauthorizesSameAccount(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	first := &zaiauth.Credential{APIKey: "key-1.secret", AccessToken: "access-1", Email: "user@example.com", UserID: "u-1"}
	created, isNew, err := createOrUpdateZAIChannel(ctx, store, first, nil, "")
	if err != nil || !isNew {
		t.Fatalf("createOrUpdateZAIChannel() created=%v err=%v", isNew, err)
	}
	if created.Name != "Z.ai-user@example.com" {
		t.Fatalf("name = %q", created.Name)
	}

	// The same account reauthorizing must land on the same channel.
	second := &zaiauth.Credential{APIKey: "key-2.secret", Email: "USER@example.com"}
	updated, isNew, err := createOrUpdateZAIChannel(ctx, store, second, nil, "")
	if err != nil || isNew {
		t.Fatalf("reauthorization created=%v err=%v", isNew, err)
	}
	if updated.ID != created.ID {
		t.Fatalf("channel id = %d, want %d", updated.ID, created.ID)
	}
	credential, err := zaiauth.ParseCredential([]byte(updated.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	if credential.APIKey != "key-2.secret" {
		t.Fatalf("api key = %q, the rotated key must win", credential.APIKey)
	}
	if credential.AccessToken != "access-1" {
		t.Fatalf("access token = %q, the account authorization must survive", credential.AccessToken)
	}

	// A different account gets its own channel.
	other := &zaiauth.Credential{APIKey: "key-3.secret", Email: "other@example.com", UserID: "u-2"}
	otherChannel, isNew, err := createOrUpdateZAIChannel(ctx, store, other, nil, "")
	if err != nil || !isNew {
		t.Fatalf("second account created=%v err=%v", isNew, err)
	}
	if otherChannel.ID == created.ID {
		t.Fatal("a different account must not reuse the channel")
	}
	domestic := *first
	domestic.Site = zaiauth.SiteBigModel
	cn, isNew, err := createOrUpdateZAIChannel(ctx, store, &domestic, nil, "")
	if err != nil || !isNew || cn.ID == created.ID {
		t.Fatalf("cross-site save: %+v %v %v", cn, isNew, err)
	}
	if !strings.HasPrefix(cn.Name, "BigModel-") || len(cn.URLs) != 1 || cn.URLs[0].URL != "https://open.bigmodel.cn/api/anthropic" {
		t.Fatalf("domestic route: %+v", cn)
	}
	domestic.APIKey = "domestic-rotated"
	cnAgain, isNew, err := createOrUpdateZAIChannel(ctx, store, &domestic, nil, "")
	if err != nil || isNew || cnAgain.ID != cn.ID {
		t.Fatalf("domestic reauth: %+v %v %v", cnAgain, isNew, err)
	}
	international, err := store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	internationalCredential, err := zaiauth.ParseCredential([]byte(international.OAuthCredential))
	if err != nil || internationalCredential.Site != zaiauth.SiteZAI || internationalCredential.APIKey != second.APIKey {
		t.Fatalf("international overwritten: %+v %v", internationalCredential, err)
	}
}

// Without an account identity the key itself is the only stable handle.
func TestCreateOrUpdateZAIChannelMatchesAnonymousKey(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	credential := &zaiauth.Credential{APIKey: "key-1.secret"}
	created, isNew, err := createOrUpdateZAIChannel(ctx, store, credential, nil, "")
	if err != nil || !isNew {
		t.Fatalf("createOrUpdateZAIChannel() created=%v err=%v", isNew, err)
	}
	if !strings.HasPrefix(created.Name, "Z.ai-") {
		t.Fatalf("name = %q", created.Name)
	}
	again, isNew, err := createOrUpdateZAIChannel(ctx, store, &zaiauth.Credential{APIKey: "key-1.secret"}, nil, "")
	if err != nil || isNew {
		t.Fatalf("re-import created=%v err=%v", isNew, err)
	}
	if again.ID != created.ID {
		t.Fatal("re-importing the same key must update in place")
	}
}

func TestUniqueZAIChannelNameAvoidsCollisions(t *testing.T) {
	t.Parallel()
	configs := []*model.Config{{Name: "Z.ai-user@example.com"}, {Name: "Z.ai-user@example.com (2)"}}
	if got := uniqueZAIChannelName(configs, "Z.ai-user@example.com"); got != "Z.ai-user@example.com (3)" {
		t.Fatalf("name = %q", got)
	}
	if got := uniqueZAIChannelName(configs, "Z.ai-free"); got != "Z.ai-free" {
		t.Fatalf("name = %q", got)
	}
}

func TestZAICredentialImportRequestValidation(t *testing.T) {
	t.Parallel()
	empty := zaiCredentialImportRequest{}
	if err := empty.Validate(); err == nil {
		t.Fatal("empty import request must be rejected")
	}
	injected := zaiCredentialImportRequest{APIKey: "key with space"}
	if err := injected.Validate(); err == nil {
		t.Fatal("keys containing whitespace must be rejected")
	}
	valid := zaiCredentialImportRequest{APIKey: "  key.secret  "}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if valid.APIKey != "key.secret" {
		t.Fatalf("api key = %q", valid.APIKey)
	}
}

/**
 * @brief 两站强制刷新各用自己的业务认证和域名，并持久化站点。
 * @param t 测试上下文。
 */
func TestHandleRefreshZAICredentialReResolvesAPIKey(t *testing.T) {
	for _, site := range []zaiauth.Site{zaiauth.SiteZAI, zaiauth.SiteBigModel} {
		t.Run(string(site), func(t *testing.T) {
			server, store, cleanup := setupAdminTestServer(t)
			defer cleanup()
			raw, err := (&zaiauth.Credential{
				Site: site, Type: zaiauth.ChannelType, APIKey: "old-id.old-secret",
				AccessToken: "zai-access", Email: "user@example.com", UserID: "u-1",
			}).JSON()
			if err != nil {
				t.Fatal(err)
			}
			channel, err := store.CreateConfig(context.Background(), newZAIOAuthChannel(
				"Z.ai-user@example.com", raw, zaiauth.CodingPlanAPIBaseURL, []string{"glm-5.3"},
			))
			if err != nil {
				t.Fatal(err)
			}
			server.client = &http.Client{Transport: oauthUsageRoundTripper(func(request *http.Request) (*http.Response, error) {
				wantHost := "api.z.ai"
				if site == zaiauth.SiteBigModel {
					wantHost = "bigmodel.cn"
				}
				if request.URL.Host != wantHost {
					t.Errorf("wrong refresh host %s", request.URL.Host)
				}
				switch {
				case request.URL.Path == "/api/auth/z/login":
					if site == zaiauth.SiteBigModel {
						t.Error("domestic token must not be exchanged")
					}
					body, _ := io.ReadAll(request.Body)
					if !strings.Contains(string(body), `"token":"zai-access"`) {
						t.Errorf("login body = %s", body)
					}
					return jsonResponse(request, `{"code":0,"data":{"access_token":"biz-token"}}`)
				case request.URL.Path == "/api/biz/customer/getCustomerInfo":
					wantAuth := "Bearer biz-token"
					if site == zaiauth.SiteBigModel {
						wantAuth = "zai-access"
					}
					if request.Header.Get("Authorization") != wantAuth {
						t.Errorf("customer info authorization = %q", request.Header.Get("Authorization"))
					}
					return jsonResponse(request, `{"code":0,"data":{"userId":"u-1","email":"user@example.com","organizations":[{"organizationId":"org-1","organizationName":"默认机构","projects":[{"projectId":"p-1","projectName":"默认项目"}]}]}}`)
				case request.URL.Path == "/api/biz/v1/organization/org-1/projects/p-1/api_keys" && request.Method == http.MethodGet:
					return jsonResponse(request, `{"code":0,"data":[{"name":"zcode-api-key","apiKey":"key-id"}]}`)
				case request.URL.Path == "/api/biz/v1/organization/org-1/projects/p-1/api_keys/copy/key-id":
					return jsonResponse(request, `{"code":0,"data":{"secretKey":"secret"}}`)
				default:
					t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
					return jsonResponseStatus(request, http.StatusNotFound, `{"code":1,"msg":"not found"}`)
				}
			})}
			server.zaiCredentials = newZAICredentialManager(store, server.getClientForChannel, func(int64) {})

			path := fmt.Sprintf("/admin/channels/%d/zai-credential/refresh", channel.ID)
			c, w := newTestContext(t, newRequest(http.MethodPost, path, nil))
			c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}
			server.HandleRefreshZAICredential(c)

			if w.Code != http.StatusOK {
				t.Fatalf("refresh status=%d body=%s", w.Code, w.Body.String())
			}
			resp := mustParseAPIResponse[struct {
				OAuthCredential zaiauth.Credential `json:"oauth_credential"`
			}](t, w.Body.Bytes())
			if resp.Data.OAuthCredential.APIKey != "key-id.secret" ||
				resp.Data.OAuthCredential.AccessToken != "zai-access" ||
				resp.Data.OAuthCredential.Email != "user@example.com" {
				t.Fatalf("refresh response credential = %#v", resp.Data.OAuthCredential)
			}

			persisted, err := store.GetConfig(context.Background(), channel.ID)
			if err != nil {
				t.Fatal(err)
			}
			parsed, err := zaiauth.ParseCredential([]byte(persisted.OAuthCredential))
			if err != nil || parsed.Site != site || parsed.APIKey != "key-id.secret" || parsed.AccessToken != "zai-access" {
				t.Fatalf("persisted credential = (%#v, %v)", parsed, err)
			}
		})
	}
}

func TestHandleRefreshZAICredentialRejectsKeyOnlyImport(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	raw, err := (&zaiauth.Credential{
		Type: zaiauth.ChannelType, APIKey: "key.secret", Email: "user@example.com",
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	channel, err := store.CreateConfig(context.Background(), newZAIOAuthChannel(
		"Z.ai-key-only", raw, zaiauth.CodingPlanAPIBaseURL, []string{"glm-5.3"},
	))
	if err != nil {
		t.Fatal(err)
	}

	path := fmt.Sprintf("/admin/channels/%d/zai-credential/refresh", channel.ID)
	c, w := newTestContext(t, newRequest(http.MethodPost, path, nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}
	server.HandleRefreshZAICredential(c)

	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "cannot be re-resolved without an account authorization") {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestHandleRefreshZAICredentialRejectsNonZAIChannel(t *testing.T) {
	server, store, cleanup := setupAdminTestServer(t)
	defer cleanup()
	channel, err := store.CreateConfig(context.Background(), &model.Config{
		Name: "api-key", AuthType: model.AuthTypeAPIKey, Enabled: true,
		URLs: model.ChannelURLs{{URL: "https://example.invalid"}}, ModelEntries: []model.ModelEntry{{Model: "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	path := fmt.Sprintf("/admin/channels/%d/zai-credential/refresh", channel.ID)
	c, w := newTestContext(t, newRequest(http.MethodPost, path, nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", channel.ID)}}
	server.HandleRefreshZAICredential(c)

	if w.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "channel does not use Z.ai OAuth") {
		t.Fatalf("body=%s", w.Body.String())
	}
}

/**
 * @brief 混合凭证导入先拒绝非法格式，再独立验证 JWT；拒绝时保留原登录，空套餐仍可导入。
 * @param t 测试上下文。
 */
func TestZAIImportMixedCredentials(t *testing.T) {
	for _, tt := range []struct {
		name      string ///< 导入场景。
		jwt       string ///< 用户提交的 JWT。
		valid     bool   ///< 上游是否接受 JWT。
		malformed bool   ///< 是否应在请求上游前拒绝。
		access    string ///< 不能派生 Key 的账号令牌。
		badKey    bool   ///< 显式 Key 是否无效。
	}{
		{name: "invalid JWT", jwt: "imported-jwt"},
		{name: "empty plans", jwt: "imported-jwt", valid: true},
		{name: "failed derivation preserves explicit key", jwt: "imported-jwt", access: "expired-access", valid: true},
		{name: "failed derivation without JWT preserves explicit key", access: "expired-access", valid: true},
		{name: "valid JWT cannot mask invalid explicit key", jwt: "imported-jwt", access: "expired-access", valid: true, badKey: true},
		{name: "trim outer whitespace", jwt: " \timported-jwt\r\n", valid: true},
		{name: "embedded space", jwt: "imported jwt", malformed: true},
		{name: "embedded carriage return", jwt: "imported\rjwt", malformed: true},
		{name: "embedded newline", jwt: "imported\njwt", malformed: true},
		{name: "embedded tab", jwt: "imported\tjwt", malformed: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newInMemoryServer(t)
			original := `{"api_key":"valid-key","jwt_token":"good-jwt","user_id":"account"}`
			cfg, err := s.store.CreateConfig(context.Background(), newZAIOAuthChannel("existing", original, zaiauth.CodingPlanAPIBaseURL, []string{"GLM-5.3-Trial"}))
			if err != nil {
				t.Fatal(err)
			}
			var upstreamCalls atomic.Int32
			s.client = &http.Client{Transport: oauthUsageRoundTripper(func(r *http.Request) (*http.Response, error) {
				upstreamCalls.Add(1)
				switch {
				case strings.HasSuffix(r.URL.Path, "/models"):
					if tt.badKey {
						return jsonResponseStatus(r, http.StatusUnauthorized, `{"error":"invalid key"}`)
					}
					return jsonResponse(r, `{"data":[]}`)
				case r.URL.Path == "/api/auth/z/login":
					return jsonResponseStatus(r, http.StatusUnauthorized, `{"code":401,"message":"expired access"}`)
				case r.URL.Path == "/api/biz/subscription/list":
					return jsonResponse(r, `{"code":0,"data":[]}`)
				case r.URL.Path == "/api/v1/agent/configs":
					return jsonResponse(r, `{"code":0,"data":{"models":[]}}`)
				case r.URL.Path == "/api.json":
					return jsonResponse(r, `{}`)
				case r.URL.Path == "/api/v1/zcode-plan/billing/balance":
					if !tt.valid {
						return jsonResponseStatus(r, http.StatusUnauthorized, `{"code":401,"message":"invalid JWT"}`)
					}
					return jsonResponse(r, `{"code":0,"data":{"plans":[],"balances":[]}}`)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					return jsonResponseStatus(r, http.StatusNotFound, `{}`)
				}
			})}
			s.zaiService = zaiauth.NewService(s.getClientForChannel(nil))
			body, err := json.Marshal(map[string]string{"api_key": "valid-key", "jwt_token": tt.jwt, "access_token": tt.access})
			if err != nil {
				t.Fatal(err)
			}
			c, response := newTestContext(t, newRequest(http.MethodPost, "/admin/zai/credentials/import", strings.NewReader(string(body))))
			c.Request.Header.Set("Content-Type", "application/json")
			s.HandleImportZAICredential(c)
			stored, err := s.store.GetConfig(context.Background(), cfg.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tt.malformed {
				if response.Code != http.StatusBadRequest || upstreamCalls.Load() != 0 || stored.OAuthCredential != original {
					t.Fatalf("malformed JWT must fail before upstream without changing login: status=%d calls=%d", response.Code, upstreamCalls.Load())
				}
				return
			}
			if !tt.valid || tt.badKey {
				if response.Code == http.StatusOK || stored.OAuthCredential != original {
					t.Fatalf("invalid import accepted or changed login: status=%d", response.Code)
				}
				return
			}
			credential, err := zaiauth.ParseCredential([]byte(stored.OAuthCredential))
			// 不带 JWT 的重新导入保留原有登录 JWT；显式 Key 始终保留。
			wantJWT := "imported-jwt"
			if tt.jwt == "" {
				wantJWT = "good-jwt"
			}
			if err != nil || response.Code != http.StatusOK || credential.JWTToken != wantJWT || credential.APIKey != "valid-key" {
				t.Fatalf("valid empty-plan import rejected: status=%d err=%v body=%s", response.Code, err, response.Body.String())
			}
		})
	}
}

/**
 * @brief 额度回写拒绝换账号前的采样，并保留同账号并发修改的偏好。
 * @param t 测试上下文。
 */
func TestZAIUsageConcurrentCredentialChange(t *testing.T) {
	for _, scenario := range []string{"account", "anonymous", "key", "preference"} {
		t.Run(scenario, func(t *testing.T) {
			s := newInMemoryServer(t)
			original, replacement := `{"jwt_token":"old-jwt","user_id":"account-a"}`, `{"jwt_token":"new-jwt","user_id":"account-b"}`
			switch scenario {
			case "anonymous":
				original, replacement = `{"jwt_token":"old-jwt"}`, `{"jwt_token":"new-jwt"}`
			case "key":
				original, replacement = `{"api_key":"old-key"}`, `{"api_key":"new-key"}`
			case "preference":
				replacement = `{"jwt_token":"old-jwt","user_id":"account-a","free_first":false}`
			}
			cfg, err := s.store.CreateConfig(context.Background(), newZAIOAuthChannel("existing", original, zaiauth.CodingPlanAPIBaseURL, []string{"GLM-5.3-Trial"}))
			if err != nil {
				t.Fatal(err)
			}
			s.client = &http.Client{Transport: oauthUsageRoundTripper(func(r *http.Request) (*http.Response, error) {
				changed, err := s.store.CompareAndSwapOAuthCredential(context.Background(), cfg.ID, model.AuthTypeZAIOAuth, original, replacement)
				if err != nil || !changed {
					t.Errorf("replace credential: changed=%v err=%v", changed, err)
				}
				if scenario == "key" {
					return jsonResponse(r, `{"code":0,"data":{"limits":[{"type":"TOKENS_LIMIT","percentage":20}]}}`)
				}
				return jsonResponse(r, `{"code":0,"data":{"plans":[{"plan_id":"account-a-plan","status":"active"}],"balances":[{"plan_id":"account-a-plan","show_name":"GLM-5.3","remaining_units":99}]}}`)
			})}
			path := fmt.Sprintf("/admin/channels/%d/oauth-usage", cfg.ID)
			c, response := newTestContext(t, newRequest(http.MethodPost, path, nil))
			c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(cfg.ID)}}
			s.HandleOAuthUsage(c)
			stored, err := s.store.GetConfig(context.Background(), cfg.ID)
			if err != nil {
				t.Fatal(err)
			}
			credential, err := zaiauth.ParseCredential([]byte(stored.OAuthCredential))
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "preference" {
				if response.Code == http.StatusOK || stored.OAuthCredential != replacement || len(credential.OAuthUsage) != 0 {
					t.Fatalf("old sample accepted for new identity: status=%d", response.Code)
				}
				return
			}
			if response.Code != http.StatusOK || len(credential.OAuthUsage) == 0 || credential.FreeFirst == nil || *credential.FreeFirst {
				t.Fatalf("same-account preference change lost: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func jsonResponse(request *http.Request, body string) (*http.Response, error) {
	return jsonResponseStatus(request, http.StatusOK, body)
}

func jsonResponseStatus(request *http.Request, status int, body string) (*http.Response, error) {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    request,
	}, nil
}

/**
 * @brief 两站 Key 导入直接保存付费站点 URL，模型发现之外不依赖路由查询。
 * @param t 测试上下文。
 */
func TestZAIImportUsesSiteEndpoint(t *testing.T) {
	for _, site := range []struct{ name, host string }{{"zai", "api.z.ai"}, {"bigmodel", "open.bigmodel.cn"}} {
		t.Run(site.name, func(t *testing.T) {
			s := newInMemoryServer(t)
			s.client = &http.Client{Transport: oauthUsageRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != site.host || !strings.HasSuffix(r.URL.Path, "/models") || r.Header.Get("Authorization") != "Bearer imported-key" {
					t.Errorf("unexpected import request: %s", r.URL)
					return jsonResponseStatus(r, http.StatusNotFound, `{}`)
				}
				return jsonResponse(r, `{"data":[{"id":"GLM-5.3-Flash"}]}`)
			})}
			s.zaiService = zaiauth.NewService(s.client)
			c, response := newTestContext(t, newRequest(http.MethodPost, "/admin/zai/credentials/import", strings.NewReader(fmt.Sprintf(`{"site":%q,"api_key":"imported-key"}`, site.name))))
			c.Request.Header.Set("Content-Type", "application/json")
			s.HandleImportZAICredential(c)
			if response.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			result := mustParseAPIResponse[zaiCredentialImportResponse](t, response.Body.Bytes()).Data
			cfg, err := s.store.GetConfig(context.Background(), result.ChannelID)
			if err != nil {
				t.Fatal(err)
			}
			want := "https://" + site.host + "/api/anthropic"
			if result.UpstreamURL != want || len(cfg.URLs) != 1 || cfg.URLs[0].URL != want {
				t.Fatalf("endpoint=%s URLs=%+v", result.UpstreamURL, cfg.URLs)
			}
		})
	}
}
