package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"ccLoad/internal/codebuddyauth"
	"ccLoad/internal/loomyauth"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

/**
 * @brief 在存储边界模拟同账号会话轮换，验证业务状态不会覆盖并发凭证。
 * @note 仅供串行调用的测试使用，两个预算分别控制凭证和额度写入冲突。
 */
type loomyCASConflictStore struct {
	storage.Store              ///< 提供真实持久化和条件更新的底层存储。
	credentialConflicts int    ///< 尚需模拟的凭证写入冲突次数。
	usageConflicts      int    ///< 尚需模拟的额度写入冲突次数。
	concurrentSession   string ///< 最近一次模拟并发更新实际保存的会话，业务写入必须保留。
}

/**
 * @brief 在业务写入前提交一次同账号会话轮换，使待写快照失效。
 * @param ctx 当前存储操作的上下文。
 * @param id 渠道标识。
 * @param authType 条件更新要求的认证类型。
 * @param expected 业务准备替换的旧凭证快照。
 * @param remaining 当前写入类别尚需模拟的冲突次数。
 * @return 已提交并发更新返回 true；无需模拟返回 false；解析或存储失败返回错误。
 */
func (s *loomyCASConflictStore) injectConflict(ctx context.Context, id int64, authType, expected string, remaining *int) (bool, error) {
	if *remaining <= 0 {
		return false, nil
	}
	credential, err := loomyauth.ParseCredential([]byte(expected))
	if err != nil {
		return false, err
	}
	credential.Session += "-renewed"
	raw, err := credential.JSON()
	if err != nil {
		return false, err
	}
	updated, err := s.Store.CompareAndSwapOAuthCredential(ctx, id, authType, expected, raw)
	if err != nil {
		return false, err
	}
	if !updated {
		return false, errors.New("failed to persist concurrent Loomy session")
	}
	*remaining--
	s.concurrentSession = credential.Session
	return true, nil
}

/**
 * @brief 模拟凭证写入遇到并发会话更新，预算耗尽后恢复真实条件更新。
 * @param ctx 当前存储操作的上下文。
 * @param id 渠道标识。
 * @param authType 预期认证类型。
 * @param expected 预期旧凭证快照。
 * @param next 待保存的新凭证快照。
 * @return 条件写入成功返回 true；冲突返回 false；存储失败返回错误。
 */
func (s *loomyCASConflictStore) CompareAndSwapOAuthCredential(ctx context.Context, id int64, authType, expected, next string) (bool, error) {
	if injected, err := s.injectConflict(ctx, id, authType, expected, &s.credentialConflicts); injected || err != nil {
		return false, err
	}
	return s.Store.CompareAndSwapOAuthCredential(ctx, id, authType, expected, next)
}

/**
 * @brief 模拟额度写入遇到并发会话更新，预算耗尽后恢复真实额度条件更新。
 * @param ctx 当前存储操作的上下文。
 * @param id 渠道标识。
 * @param authType 预期认证类型。
 * @param expected 预期旧凭证快照。
 * @param next 待保存的新额度凭证快照。
 * @return 条件写入成功返回 true；冲突返回 false；存储失败返回错误。
 */
func (s *loomyCASConflictStore) CompareAndSwapOAuthUsage(ctx context.Context, id int64, authType, expected, next string) (bool, error) {
	if injected, err := s.injectConflict(ctx, id, authType, expected, &s.usageConflicts); injected || err != nil {
		return false, err
	}
	return s.Store.CompareAndSwapOAuthUsage(ctx, id, authType, expected, next)
}

/**
 * @brief 验证签到接口能跨过短暂写入争用，持续争用时有界结束且保留已成功状态。
 * @param t 当前测试上下文。
 * @details 两类写入均模拟超过原三次上限的冲突，只断言 API 结果、持久状态与上游请求次数。
 */
func TestLoomyCheckinCASContention(t *testing.T) {
	for _, tc := range []struct {
		name                string ///< 业务场景名称。
		credentialConflicts int    ///< 凭证写入受到的连续并发更新次数。
		usageConflicts      int    ///< 额度写入受到的连续并发更新次数。
		wantCheckinSaved    bool   ///< 是否应保存已确认的每日签到。
		wantUsage           bool   ///< 是否应返回并持久化额度。
	}{
		{"temporary conflicts", 4, 4, true, true},
		{"credential contention", 100, 0, false, false},
		{"usage contention", 0, 100, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				base := newManagementSchedulerTestServer(t)
				store := &loomyCASConflictStore{Store: base.store, credentialConflicts: tc.credentialConflicts, usageConflicts: tc.usageConflicts}
				var dailyCalls, balanceCalls atomic.Int32
				day := time.Now().In(loomyLocation).Format("2006-01-02")
				upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/api/v1/points/first-login":
						dailyCalls.Add(1)
						_ = json.NewEncoder(w).Encode(map[string]any{"code": "000000", "data": map[string]any{"alreadyProcessed": false, "dailyQuota": 5000, "dailyCycleDate": day}})
					case "/api/v1/points/balance":
						balanceCalls.Add(1)
						_, _ = w.Write([]byte(`{"code":"000000","data":{"balance":100,"dailyBalance":1200,"availableBalance":1300}}`))
					default:
						t.Errorf("unexpected upstream request %s", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				srv := &Server{store: store, client: newTestHTTPClient(), loomyService: &loomyauth.Service{BaseURL: upstream.URL}}
				credential := &loomyauth.Credential{Session: "session", UserID: "user", OnboardingCompletedAt: time.Now().UTC().Format(time.RFC3339)}
				raw, err := credential.JSON()
				if err != nil {
					t.Fatal(err)
				}
				cfg, err := store.CreateConfig(context.Background(), &model.Config{Name: tc.name, AuthType: model.AuthTypeLoomySession,
					OAuthCredential: raw, Enabled: true, URLs: model.ChannelURLs{{URL: upstream.URL}}})
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				request := newRequest(http.MethodPost, fmt.Sprintf("/admin/channels/%d/loomy-checkin", cfg.ID), nil).WithContext(ctx)
				c, response := newTestContext(t, request)
				c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(cfg.ID)}}
				srv.HandleLoomyCheckin(c)
				if response.Code != http.StatusOK || ctx.Err() != nil {
					t.Fatalf("check-in did not finish within its budget: status=%d err=%v", response.Code, ctx.Err())
				}
				result := mustParseAPIResponse[loomyCheckinResult](t, response.Body.Bytes()).Data
				if result.Status != "success" || (result.Usage != nil) != tc.wantUsage || (len(result.Warnings) == 0) != tc.wantUsage {
					t.Fatalf("unexpected check-in result: %+v", result)
				}
				persisted, err := store.GetConfig(ctx, cfg.ID)
				if err != nil {
					t.Fatal(err)
				}
				saved, err := loomyauth.ParseCredential([]byte(persisted.OAuthCredential))
				if err != nil {
					t.Fatal(err)
				}
				if saved.Session != store.concurrentSession || saved.OnboardingCompletedAt != credential.OnboardingCompletedAt ||
					(saved.LastCheckinDay == day) != tc.wantCheckinSaved || (len(saved.OAuthUsage) > 0) != tc.wantUsage {
					t.Fatal("persistence lost concurrent credentials or confirmed business state")
				}
				wantBalance := int32(0)
				if tc.wantCheckinSaved {
					wantBalance = 1
				}
				if dailyCalls.Load() != 1 || balanceCalls.Load() != wantBalance {
					t.Fatalf("local CAS retries repeated upstream operations: daily=%d balance=%d", dailyCalls.Load(), balanceCalls.Load())
				}
			})
		})
	}
}

/**
 * @brief 验证慢 Loomy 签到不推迟管理账户签到、批次不重叠且服务取消能终止在途请求。
 * @param t 当前测试上下文。
 * @details 使用虚拟时间跨过真实分钟扫描周期，第二个账号在首批执行期间到期，必须等首批完成后才收到签到请求。
 */
func TestLoomyCheckinSchedulerIsolationAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		time.Sleep(12 * time.Hour)
		var loomyCalls, managementPosts, canceled atomic.Int32
		releaseFirst := make(chan struct{})
		upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.URL.Path == "/api/v1/points/first-login":
				if loomyCalls.Add(1) == 1 {
					select {
					case <-releaseFirst:
					case <-r.Context().Done():
						canceled.Add(1)
					}
				} else {
					<-r.Context().Done()
					canceled.Add(1)
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			case r.URL.Path == "/api/status":
				_, _ = w.Write([]byte(`{"success":true,"data":{"checkin_enabled":true}}`))
			case r.URL.Path == "/api/user/checkin" && r.Method == http.MethodPost:
				managementPosts.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
					"quota_awarded": 1, "checkin_date": time.Now().Format("2006-01-02"),
				}})
			case r.URL.Path == "/api/user/checkin":
				_, _ = w.Write([]byte(`{"success":true,"data":{"stats":{"checked_in_today":false}}}`))
			case r.URL.Path == "/api/user/self":
				_, _ = w.Write([]byte(`{"success":true,"data":{"quota":1}}`))
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
				http.NotFound(w, r)
			}
		}))
		srv := newManagementSchedulerTestServer(t)
		srv.client = newTestHTTPClient()
		srv.loomyService = &loomyauth.Service{BaseURL: upstream.URL}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		srv.baseCtx = ctx
		srv.shutdownCh = make(chan struct{})
		for i := range 2 {
			credential := &loomyauth.Credential{Session: "session", UserID: fmt.Sprint(i)}
			if i == 1 {
				credential.NextCheckinAttemptAt = time.Now().Add(30 * time.Second)
			}
			raw, err := credential.JSON()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := srv.store.CreateConfig(ctx, &model.Config{
				Name: fmt.Sprintf("loomy-scheduler-%d", i), AuthType: model.AuthTypeLoomySession,
				OAuthCredential: raw, Enabled: true, URLs: model.ChannelURLs{{URL: upstream.URL}},
			}); err != nil {
				t.Fatal(err)
			}
		}
		envelope := newDueNewAPIEnvelope(upstream.URL)
		envelope.Settings.DailyCheckinTime = time.Now().Add(time.Minute).Format("15:04")
		seedManagementEnvelope(t, srv, "independent-management-checkin", envelope)
		srv.wg.Add(2)
		go srv.loomyCheckinLoop()
		go srv.managementCheckinLoop()
		synctest.Wait()
		if loomyCalls.Load() != 1 || managementPosts.Load() != 0 {
			t.Fatalf("startup calls: loomy=%d management=%d", loomyCalls.Load(), managementPosts.Load())
		}

		time.Sleep(61 * time.Second)
		synctest.Wait()
		if managementPosts.Load() != 1 {
			t.Fatal("slow Loomy check-in delayed the management check-in")
		}
		if loomyCalls.Load() != 1 {
			t.Fatal("a new Loomy batch started before the previous batch finished")
		}

		close(releaseFirst)
		time.Sleep(time.Minute)
		synctest.Wait()
		if loomyCalls.Load() != 2 {
			t.Fatalf("Loomy scanning did not resume after the slow batch: calls=%d", loomyCalls.Load())
		}
		cancel()
		close(srv.shutdownCh)
		synctest.Wait()
		srv.wg.Wait()
		if canceled.Load() != 1 {
			t.Fatalf("in-flight requests canceled=%d, want 1", canceled.Load())
		}
	})
}

/**
 * @brief 验证签到与新手任务状态持久化、失败重试及重新登录后的状态保留。
 * @param t 当前测试上下文。
 */
func TestLoomyCheckinPersistenceRetryAndReauthorization(t *testing.T) {
	var mu sync.Mutex
	tasks := make(map[string]bool)
	for _, key := range loomyauth.OnboardingKeys() {
		tasks[key] = false
	}
	var failTask atomic.Bool
	failTask.Store(true)
	var queries, claims, dailyCalls atomic.Int32
	var failDaily atomic.Bool
	checkinDay := time.Now().In(loomyLocation).Format("2006-01-02")
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/points/first-login":
			dailyCalls.Add(1)
			if failDaily.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "000000", "data": map[string]any{
				"alreadyProcessed": true, "dailyQuota": 5000, "dailyCycleDate": checkinDay,
			}})
		case "/api/v1/onboarding/tasks":
			queries.Add(1)
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"code": "000000", "data": map[string]any{"tasks": tasks}})
		case "/api/v1/onboarding/tasks/complete":
			claims.Add(1)
			var input struct {
				Key string `json:"key"` ///< 提交给上游的新手任务标识。
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			if input.Key == "share_soul" && failTask.Load() {
				_, _ = w.Write([]byte(`{"code":"TASK_FAILED","data":{}}`))
				return
			}
			mu.Lock()
			tasks[input.Key] = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{"code":"000000","data":{"alreadyCompleted":false}}`))
		case "/api/v1/points/balance":
			// A balance failure must not erase a confirmed onboarding marker.
			w.WriteHeader(503)
		case "/api/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"chat","type":"chat"}]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	// Control scheduling explicitly; a full Server also starts a catch-up scan.
	dbPath := filepath.Join(t.TempDir(), "loomy.db")
	store, err := storage.CreateSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	storeOpen := true
	t.Cleanup(func() {
		if storeOpen {
			_ = store.Close()
		}
	})
	srv := &Server{store: store, client: newTestHTTPClient(), loomyService: &loomyauth.Service{BaseURL: upstream.URL}}
	ctx := context.Background()
	cfg, _, err := srv.commitLoomyCredential(ctx, &loomyauth.Credential{Session: "old", UserID: "user"}, "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := srv.checkInLoomy(ctx, cfg, false)
	if err != nil || first.Status != "already_checked" || first.Onboarding != "pending" || first.Day != checkinDay {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	stored, err := srv.store.GetConfig(ctx, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := loomyauth.ParseCredential([]byte(stored.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	if credential.LastCheckinDay == "" || credential.OnboardingCompletedAt != "" {
		t.Fatalf("partial state=%+v", credential)
	}
	if credential.DailyQuota == nil || *credential.DailyQuota != 5000 || credential.DailyCycleDate != checkinDay {
		t.Fatalf("check-in quota was not persisted: %+v", credential)
	}
	failTask.Store(false)
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			result, checkErr := srv.checkInLoomy(ctx, cfg, false)
			if checkErr != nil || result.Onboarding == "pending" {
				t.Errorf("retry=%+v err=%v", result, checkErr)
			}
		})
	}
	wg.Wait()
	if claims.Load() != 9 {
		t.Fatalf("claims=%d, want 8 first attempts plus one retry", claims.Load())
	}
	queryCount := queries.Load()
	updated, created, err := srv.commitLoomyCredential(ctx, &loomyauth.Credential{Session: "new", UserID: "user"}, "")
	if err != nil || created || updated.ID != cfg.ID {
		t.Fatalf("relogin created=%v err=%v", created, err)
	}
	// An old in-flight rejection must not disable the newly authorized session.
	srv.handleLoomyRejection(ctx, cfg, loomyauth.ErrSessionExpired)
	updated, err = srv.store.GetConfig(ctx, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential, err = loomyauth.ParseCredential([]byte(updated.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Enabled || credential.Session != "new" || credential.OnboardingCompletedAt == "" {
		t.Fatalf("relogin state=%+v enabled=%v", credential, updated.Enabled)
	}
	if credential.DailyQuota == nil || *credential.DailyQuota != 5000 || credential.DailyCycleDate != checkinDay {
		t.Fatal("relogin discarded the daily quota")
	}
	usage := &oauthUsageSummary{Provider: loomyauth.ChannelType, Windows: []oauthUsageWindow{}, LoomyCredits: &loomyauth.Balance{}}
	if _, err := srv.persistOAuthUsage(ctx, updated, usage, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	// Reopen the actual SQLite file, so neither memory nor an open connection
	// can supply the completion marker after a process restart.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	storeOpen = false
	reopened, err := storage.CreateSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := &Server{store: reopened, client: srv.client, loomyService: srv.loomyService}
	result, err := restarted.checkInLoomy(ctx, updated, false)
	if err != nil || result.Onboarding != "skipped" || result.Day != checkinDay || queries.Load() != queryCount {
		t.Fatalf("restart=%+v queries=%d err=%v", result, queries.Load(), err)
	}
	// 从管理 API 触发上游失败，确认下次固定时间落入 SQLite，重启后不会提前重试。
	failDaily.Store(true)
	failedCfg, _, err := restarted.commitLoomyCredential(ctx, &loomyauth.Credential{Session: "failed", UserID: "failed-user"}, "")
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.POST("/channels/:id/checkin", restarted.HandleLoomyCheckin)
	response := httptest.NewRecorder()
	startedAt := time.Now()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, fmt.Sprintf("/channels/%d/checkin", failedCfg.ID), nil))
	var body struct {
		Data loomyCheckinResult `json:"data"` ///< 管理 API 封套中的签到业务结果。
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || body.Data.Status != "failed" {
		t.Fatalf("response status=%d result=%+v", response.Code, body.Data)
	}
	persisted, err := reopened.GetConfig(ctx, failedCfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	failedCredential, err := loomyauth.ParseCredential([]byte(persisted.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	local := startedAt.In(loomyLocation)
	wantNext := time.Date(local.Year(), local.Month(), local.Day(), 0, 1, 0, 0, loomyLocation)
	if !startedAt.Before(wantNext) {
		wantNext = wantNext.Add(8 * time.Hour)
	}
	if !startedAt.Before(wantNext) {
		wantNext = wantNext.Add(16 * time.Hour)
	}
	if failedCredential.LastCheckinDay != "" || !failedCredential.NextCheckinAttemptAt.Equal(wantNext) {
		t.Fatalf("failed check-in day=%q next=%s, want %s", failedCredential.LastCheckinDay, failedCredential.NextCheckinAttemptAt, wantNext)
	}
	calls := dailyCalls.Load()
	afterRestart := &Server{store: reopened, client: srv.client, loomyService: srv.loomyService}
	afterRestart.runDueLoomyCheckins(ctx, time.Now())
	if dailyCalls.Load() != calls {
		t.Fatal("restart retried before the persisted check-in time")
	}
}

/**
 * @brief 验证每日积分总额的来源优先级和跨日隔离，不用固定默认额度。
 * @param t 当前测试上下文。
 */
func TestLoomyUsageQuotaSources(t *testing.T) {
	now := time.Now().In(loomyLocation)
	day := now.Format("2006-01-02")
	for _, tc := range []struct {
		name                string  ///< 子用例名称。
		quotaDay            string  ///< 已保存每日总额所属的北京时间日期。
		storedQuota         float64 ///< 已保存的每日积分总额。
		ledgerBody          string  ///< 模拟每日积分流水的 JSON 正文。
		wantQuota, wantUsed float64 ///< wantQuota：期望推导的每日总积分；wantUsed：期望的当日已用积分。
		wantLedger          int32   ///< 期望向上游查询流水的次数。
	}{
		{"check-in wins", day, 5000, "", 5000, 3800, 0},
		{"derive today's quota", "", 0, `{"total":1,"list":[{"direction":"debit","consumeSource":"daily","dailyCycleDate":"` + day + `","pointsActual":800}]}`, 2000, 800, 1},
		{"discard yesterday", now.AddDate(0, 0, -1).Format("2006-01-02"), 5000, `{"total":0,"list":[]}`, 1200, 0, 1},
		{"partial ledger stays unknown", "", 0, `{"total":1,"list":[]}`, 0, 0, 2},
		{"failed ledger retains balance", "", 0, `{}`, 0, 0, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ledgerCalls atomic.Int32
			upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/points/balance":
					_, _ = w.Write([]byte(`{"code":"000000","data":{"balance":15000,"dailyBalance":1200,"availableBalance":16200}}`))
				case "/api/v1/points/records":
					ledgerCalls.Add(1)
					_, _ = fmt.Fprintf(w, `{"code":"000000","data":%s}`, tc.ledgerBody)
				default:
					t.Errorf("read-only usage called %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			store := newCodexAuthTestStore(t)
			srv := &Server{store: store, client: newTestHTTPClient(), loomyService: &loomyauth.Service{BaseURL: upstream.URL}}
			credential := &loomyauth.Credential{Session: "session", UserID: "user", DailyCycleDate: tc.quotaDay}
			if tc.storedQuota > 0 {
				credential.DailyQuota = &tc.storedQuota
			}
			raw, err := credential.JSON()
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := store.CreateConfig(context.Background(), &model.Config{
				Name: "loomy-usage", AuthType: model.AuthTypeLoomySession, OAuthCredential: raw, Enabled: true,
				URLs: model.ChannelURLs{{URL: loomyauth.CompletionsURL, Exact: true}},
			})
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				requestedAt := time.Now()
				usage, err := srv.requestLoomyUsage(context.Background(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				balance := usage.LoomyCredits
				if balance == nil || balance.Daily == nil || *balance.Daily != 1200 || balance.Permanent == nil || *balance.Permanent != 15000 {
					t.Fatalf("lost balance: %+v", balance)
				}
				if tc.wantQuota > 0 {
					if balance.DailyQuota == nil || *balance.DailyQuota != tc.wantQuota || balance.DailyConsumed == nil || *balance.DailyConsumed != tc.wantUsed || balance.DailyCycleDate != day {
						t.Fatalf("wrong daily quota: %+v", balance)
					}
				} else if balance.DailyQuota != nil || balance.DailyConsumed != nil {
					t.Fatalf("fabricated quota: %+v", balance)
				}
				if _, err := srv.persistOAuthUsage(context.Background(), cfg, usage, requestedAt, time.Now()); err != nil {
					t.Fatal(err)
				}
				cfg, err = store.GetConfig(context.Background(), cfg.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if ledgerCalls.Load() != tc.wantLedger {
				t.Fatalf("ledger calls=%d want=%d", ledgerCalls.Load(), tc.wantLedger)
			}
		})
	}
}

/**
 * @brief 验证北京时间 00:01 首签、08:01 补签及已签到跳过的调度边界。
 * @param t 当前测试上下文。
 */
func TestLoomyCheckinSchedule(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 1, 0, 0, loomyLocation)
	morning := now.Add(8 * time.Hour)
	for _, tc := range []struct {
		name string               ///< 子用例名称。
		at   time.Time            ///< 本用例用于调度计算的绝对时刻。
		c    loomyauth.Credential ///< 本用例的最近签到日及下次尝试状态。
		due  bool                 ///< 是否期望到达本次自动签到条件。
	}{
		{"before first check-in", now.Add(-time.Second), loomyauth.Credential{}, false},
		{"due", now, loomyauth.Credential{}, true},
		{"same instant in UTC", now.UTC(), loomyauth.Credential{}, true},
		{"today pending tasks", now, loomyauth.Credential{LastCheckinDay: "2026-11-04"}, false},
		{"tomorrow", now.Add(24 * time.Hour), loomyauth.Credential{LastCheckinDay: "2026-11-04"}, true},
		{"backoff", now, loomyauth.Credential{NextCheckinAttemptAt: now.Add(time.Minute)}, false},
		{"wait for morning retry", morning.Add(-time.Second), loomyauth.Credential{NextCheckinAttemptAt: morning}, false},
		{"morning retry", morning, loomyauth.Credential{NextCheckinAttemptAt: morning}, true},
		{"morning skips checked account", morning, loomyauth.Credential{LastCheckinDay: "2026-11-04"}, false},
		{"startup catches missed retry", morning.Add(time.Hour), loomyauth.Credential{NextCheckinAttemptAt: morning}, true},
		{"wait until tomorrow after retry", morning.Add(time.Hour), loomyauth.Credential{NextCheckinAttemptAt: now.AddDate(0, 0, 1)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := loomyCheckinDue(&tc.c, tc.at); got != tc.due {
				t.Fatalf("due=%v", got)
			}
		})
	}
	// 固定时点边界覆盖 UTC 表示、跨日、跨月及跨年，不依赖机器本地时区。
	for _, tc := range []struct {
		name string    ///< 子用例名称。
		at   time.Time ///< 本用例用于调度计算的绝对时刻。
		want time.Time ///< 期望的下一固定签到时点。
	}{
		{"before first slot", now.Add(-time.Second), now},
		{"first failure waits for morning", now, morning},
		{"same instant in UTC", now.UTC(), morning},
		{"before morning", morning.Add(-time.Second), morning},
		{"morning failure waits for tomorrow", morning, now.AddDate(0, 0, 1)},
		{"late startup failure", morning.Add(time.Hour), now.AddDate(0, 0, 1)},
		{"month boundary", time.Date(2026, 11, 30, 23, 59, 0, 0, loomyLocation), time.Date(2026, 12, 1, 0, 1, 0, 0, loomyLocation)},
		{"year boundary", time.Date(2026, 12, 31, 23, 59, 0, 0, loomyLocation), time.Date(2027, 1, 1, 0, 1, 0, 0, loomyLocation)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextLoomyCheckinAttempt(tc.at); !got.Equal(tc.want) {
				t.Fatalf("next attempt=%s, want %s", got, tc.want)
			}
		})
	}
}

func TestCodeBuddyCheckinSlot(t *testing.T) {
	loc := time.FixedZone("server", 8*60*60)
	cases := []struct {
		name string
		now  time.Time
		want string
	}{
		{name: "before morning", now: time.Date(2026, 9, 11, 8, 59, 0, 0, loc), want: ""},
		{name: "morning", now: time.Date(2026, 9, 11, 9, 0, 0, 0, loc), want: "2026-09-11-09"},
		{name: "before evening", now: time.Date(2026, 9, 11, 20, 59, 0, 0, loc), want: "2026-09-11-09"},
		{name: "evening", now: time.Date(2026, 9, 11, 21, 0, 0, 0, loc), want: "2026-09-11-21"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codeBuddyCheckinSlot(tc.now); got != tc.want {
				t.Fatalf("slot=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestCodeBuddyUsageRefreshAndScheduledCheckin(t *testing.T) {
	var checkins, resources atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin":
			if r.Method != http.MethodPost {
				t.Errorf("check-in method=%s, want POST", r.Method)
			}
			checkins.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case "/v2/billing/meter/get-enterprise-user-usage":
			resources.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit":104.35,"limitNum":2000}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	srv := newInMemoryServer(t)
	srv.codeBuddyService.BaseURL = upstream.URL
	raw, err := (&codebuddyauth.Credential{AccessToken: "access", RefreshToken: "refresh", EnterpriseID: "enterprise"}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	scheduled := newCodeBuddyChannel("scheduled-codebuddy", raw)
	scheduled.CostMultiplier = 1.5
	cfg, err := srv.store.CreateConfig(context.Background(), scheduled)
	if err != nil {
		t.Fatal(err)
	}
	disabled := newCodeBuddyChannel("disabled-codebuddy", raw)
	disabled.Enabled = false
	if _, err := srv.store.CreateConfig(context.Background(), disabled); err != nil {
		t.Fatal(err)
	}

	summary, err := srv.refreshOAuthUsage(context.Background(), cfg.ID)
	if err != nil {
		t.Fatalf("refreshOAuthUsage: %v", err)
	}
	if summary.Provider != codebuddyauth.ChannelType || summary.CodeBuddyCredits == nil || summary.CodeBuddyCredits.Remain != 1895.65 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	stored, err := srv.store.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := codebuddyauth.ParseCredential([]byte(stored.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	persisted, _, _ := persistedOAuthUsage([]byte(credential.OAuthUsage), codebuddyauth.ChannelType)
	if persisted == nil || persisted.CodeBuddyCredits == nil || persisted.CodeBuddyCredits.Remain != 1895.65 ||
		persisted.CodeBuddyCredits.Total == nil || *persisted.CodeBuddyCredits.Total != 2000 ||
		persisted.CodeBuddyCredits.Used == nil || *persisted.CodeBuddyCredits.Used != 104.35 {
		t.Fatalf("persisted summary missing: %+v", persisted)
	}

	srv.runDueCodeBuddyCheckins(context.Background())
	if checkins.Load() != 0 || resources.Load() != 2 {
		t.Fatalf("checkins=%d resources=%d, want 0 and 2", checkins.Load(), resources.Load())
	}
	logs, err := srv.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 10, 0, &model.LogFilter{
		LogSource: model.LogSourceCheckin,
		ChannelID: &cfg.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("CodeBuddy check-in audit count=%d, want 1", len(logs))
	}
	// 签到日志按渠道级倍率快照，零值会被日志页渲染成免费渠道。
	if logs[0].ChannelID != cfg.ID || logs[0].LogSource != model.LogSourceCheckin || logs[0].CostMultiplier != 1.5 {
		t.Fatalf("unexpected CodeBuddy audit entry: %#v", logs[0])
	}
	var audit channelCheckinAuditMessage
	if err := json.Unmarshal([]byte(logs[0].Message), &audit); err != nil {
		t.Fatalf("invalid CodeBuddy audit message: %v", err)
	}
	if audit.Profile != codebuddyauth.ChannelType || audit.Status != "success" ||
		audit.Balance == nil || audit.Balance.Remaining != 1895.65 {
		t.Fatalf("unexpected CodeBuddy audit message: %#v", audit)
	}
}

func TestCodeBuddyScheduledCheckinAuditsFailure(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":10001,"msg":"签到活动未开启或已过期"}`))
		case "/v2/billing/meter/get-user-resource":
			_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":720}]}}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	srv := newInMemoryServer(t)
	srv.codeBuddyService.BaseURL = upstream.URL
	raw, err := (&codebuddyauth.Credential{AccessToken: "access"}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := srv.store.CreateConfig(context.Background(), newCodeBuddyChannel("scheduled-codebuddy-failure", raw))
	if err != nil {
		t.Fatal(err)
	}

	srv.runDueCodeBuddyCheckins(context.Background())
	logs, err := srv.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 10, 0, &model.LogFilter{
		LogSource: model.LogSourceCheckin,
		ChannelID: &cfg.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("CodeBuddy failed check-in audit count=%d, want 1", len(logs))
	}
	if logs[0].StatusCode != http.StatusBadGateway {
		t.Fatalf("CodeBuddy failed check-in status code=%d, want %d", logs[0].StatusCode, http.StatusBadGateway)
	}
	var audit channelCheckinAuditMessage
	if err := json.Unmarshal([]byte(logs[0].Message), &audit); err != nil {
		t.Fatalf("invalid CodeBuddy failure audit message: %v", err)
	}
	if audit.Profile != codebuddyauth.ChannelType || audit.Status != "failed" ||
		audit.Balance == nil || audit.Balance.Remaining != 720 {
		t.Fatalf("unexpected CodeBuddy failure audit message: %#v", audit)
	}
}

func TestHandleCodeBuddyCheckin(t *testing.T) {
	var mode, checkins, resources atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin":
			checkins.Add(1)
			switch mode.Load() {
			case 0:
				_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
			case 1:
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"code":10001,"msg":"今天已签到，请明天再来"}`))
			default:
				_, _ = w.Write([]byte(`{"code":9001,"msg":"private upstream detail"}`))
			}
		case "/v2/billing/meter/get-user-resource":
			resources.Add(1)
			_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":1000,"CycleCapacityRemain":720}]}}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	server := newInMemoryServer(t)
	server.codeBuddyService.BaseURL = upstream.URL
	raw, err := (&codebuddyauth.Credential{AccessToken: "manual-secret"}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	channel, err := server.store.CreateConfig(context.Background(), newCodeBuddyChannel("manual-codebuddy", raw))
	if err != nil {
		t.Fatal(err)
	}

	call := func(channelID int64) (int, []byte) {
		path := fmt.Sprintf("/admin/channels/%d/codebuddy-checkin", channelID)
		c, w := newTestContext(t, newRequest(http.MethodPost, path, nil))
		c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(channelID)}}
		server.HandleCodeBuddyCheckin(c)
		return w.Code, append([]byte(nil), w.Body.Bytes()...)
	}

	status, body := call(channel.ID)
	if status != http.StatusOK {
		t.Fatalf("check-in status=%d body=%s", status, body)
	}
	response := mustParseAPIResponse[codeBuddyCheckinResult](t, body)
	if response.Data.Status != "success" || response.Data.Usage == nil ||
		response.Data.Usage.CodeBuddyCredits == nil || response.Data.Usage.CodeBuddyCredits.Remain != 720 {
		t.Fatalf("check-in response=%+v", response.Data)
	}
	persistedChannel, err := server.store.GetConfig(context.Background(), channel.ID)
	if err != nil {
		t.Fatal(err)
	}
	persistedCredential, err := codebuddyauth.ParseCredential([]byte(persistedChannel.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	persistedUsage, _, _ := persistedOAuthUsage([]byte(persistedCredential.OAuthUsage), codebuddyauth.ChannelType)
	if persistedUsage == nil || persistedUsage.CodeBuddyCredits == nil || persistedUsage.CodeBuddyCredits.Remain != 720 {
		t.Fatalf("manual check-in did not persist balance: %+v", persistedUsage)
	}

	mode.Store(1)
	status, body = call(channel.ID)
	response = mustParseAPIResponse[codeBuddyCheckinResult](t, body)
	if status != http.StatusOK || response.Data.Status != "already_checked" {
		t.Fatalf("already checked status=%d response=%+v", status, response.Data)
	}

	mode.Store(2)
	status, body = call(channel.ID)
	if status != http.StatusBadGateway || strings.Contains(string(body), "private upstream detail") || strings.Contains(string(body), "manual-secret") {
		t.Fatalf("failed check-in status=%d body=%s", status, body)
	}
	if checkins.Load() != 3 || resources.Load() != 3 {
		t.Fatalf("checkins=%d resources=%d, want 3 and 3", checkins.Load(), resources.Load())
	}

	internationalRaw, err := (&codebuddyauth.Credential{
		AccessToken: "international-secret",
		BaseURL:     codebuddyauth.InternationalBaseURL,
	}).JSON()
	if err != nil {
		t.Fatal(err)
	}
	international, err := server.store.CreateConfig(context.Background(), newCodeBuddyChannel("international-codebuddy", internationalRaw))
	if err != nil {
		t.Fatal(err)
	}
	status, _ = call(international.ID)
	if status != http.StatusConflict {
		t.Fatalf("international check-in status=%d, want %d", status, http.StatusConflict)
	}
	if checkins.Load() != 3 || resources.Load() != 3 {
		t.Fatalf("international check-in made upstream requests: checkins=%d resources=%d", checkins.Load(), resources.Load())
	}

	unsupported, err := server.store.CreateConfig(context.Background(), &model.Config{
		Name: "not-codebuddy", AuthType: model.AuthTypeAPIKey, Enabled: true,
		URLs: model.ChannelURLs{{URL: "https://api.example.test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, _ = call(unsupported.ID)
	if status != http.StatusConflict {
		t.Fatalf("unsupported check-in status=%d, want %d", status, http.StatusConflict)
	}

	engine := gin.New()
	server.SetupRoutes(engine)
	found := false
	for _, route := range engine.Routes() {
		if route.Method == http.MethodPost && route.Path == "/admin/channels/:id/codebuddy-checkin" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("CodeBuddy manual check-in route is not registered")
	}
}

func TestManagementCheckinDueUsesServerLocalDateAndTime(t *testing.T) {
	t.Parallel()
	// 时区由 now 自带，不改进程级 time.Local——那会和同包并行测试里的 time.Now() 打架。
	loc := time.FixedZone("server", 8*60*60)
	base := time.Date(2026, 8, 26, 9, 0, 0, 0, loc)
	newEnvelope := func(day string) *model.ChannelManagementEnvelope {
		return &model.ChannelManagementEnvelope{
			Profile:  model.ChannelManagementProfileNewAPI,
			Settings: model.ChannelManagementSettings{DailyCheckinEnabled: true, DailyCheckinTime: "09:00"},
			State:    model.ChannelManagementState{LastScheduledDay: day},
		}
	}
	for name, tc := range map[string]struct {
		now  time.Time
		day  string
		want bool
	}{
		"before time":          {now: base.Add(-time.Minute), want: false},
		"at time":              {now: base, want: true},
		"same local day":       {now: base.Add(time.Hour), day: "2026-08-26", want: false},
		"previous day can run": {now: base, day: "2026-08-25", want: true},
		"local date matters":   {now: time.Date(2026, 8, 26, 1, 0, 0, 0, loc), want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := isManagementCheckinDue(newEnvelope(tc.day), tc.now); got != tc.want {
				t.Fatalf("isManagementCheckinDue()=%v, want %v at %s", got, tc.want, tc.now)
			}
		})
	}
}

func TestManagementCheckinDueSkipsUnsupportedOrDisabledProfiles(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.FixedZone("server", 8*60*60))
	cases := []model.ChannelManagementEnvelope{
		{Profile: model.ChannelManagementProfileSub2API, Settings: model.ChannelManagementSettings{DailyCheckinEnabled: true, DailyCheckinTime: "09:00"}},
		{Profile: model.ChannelManagementProfileNewAPI, Settings: model.ChannelManagementSettings{DailyCheckinTime: "09:00"}},
		{Profile: model.ChannelManagementProfileNewAPI, Settings: model.ChannelManagementSettings{DailyCheckinEnabled: true, DailyCheckinTime: "invalid"}},
	}
	for _, envelope := range cases {
		if isManagementCheckinDue(&envelope, now) {
			t.Fatalf("unsupported/disabled/invalid envelope was due: %#v", envelope)
		}
	}
}

// 调度测试自行推进扫描，不启动会抢先认领任务的后台启动补偿扫描。
func newManagementSchedulerTestServer(t *testing.T) *Server {
	t.Helper()
	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("CreateSQLiteStore: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return &Server{
		store: store,
		channelManagement: newChannelManagementService(store, func(*model.Config) *http.Client {
			return newTestHTTPClient()
		}),
	}
}

func newDueNewAPIEnvelope(baseURL string) *model.ChannelManagementEnvelope {
	return &model.ChannelManagementEnvelope{
		Kind: model.ChannelManagementKind, Version: model.ChannelManagementVersion,
		Profile: model.ChannelManagementProfileNewAPI,
		Settings: model.ChannelManagementSettings{
			BaseURL: baseURL, AccessToken: "test-token",
			DailyCheckinEnabled: true, DailyCheckinTime: "00:00",
		},
	}
}

/**
 * @brief 普通业务失败当天只尝试一次，并保留唯一签到审计。
 * @param t 测试上下文。
 */
func TestManagementCheckinSchedulerExecutesAuditsAndDoesNotRetry(t *testing.T) {
	var posts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/status":
			_, _ = w.Write([]byte(`{"success":true,"data":{"checkin_enabled":true}}`))
		case "/api/user/checkin":
			if r.Method == http.MethodPost {
				posts.Add(1)
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"success":false}`))
				return
			}
			_, _ = w.Write([]byte(`{"success":true,"data":{"stats":{"checked_in_today":false}}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	server := newManagementSchedulerTestServer(t)
	server.channelManagement.now = func() time.Time { return time.Now() }
	cfg := seedManagementEnvelope(t, server, "scheduler-no-retry", newDueNewAPIEnvelope(upstream.URL))

	now := time.Now()
	err := server.runDueManagementCheckins(context.Background(), now)
	if err != nil {
		t.Fatalf("runDueManagementCheckins: %v", err)
	}
	if err := server.runDueManagementCheckins(context.Background(), now); err != nil {
		t.Fatalf("same-day scan after business failure: %v", err)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("POST check-in count=%d, want 1", got)
	}
	logs, err := server.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 10, 0, &model.LogFilter{LogSource: model.LogSourceCheckin, ChannelID: &cfg.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("check-in audit count=%d, want 1", len(logs))
	}
}

func TestManagementCheckinSchedulerExecutesInitiallyDisabledChannel(t *testing.T) {
	var posts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/status":
			_, _ = w.Write([]byte(`{"success":true,"data":{"checkin_enabled":true}}`))
		case r.URL.Path == "/api/user/checkin" && r.Method == http.MethodPost:
			posts.Add(1)
			_, _ = w.Write([]byte(`{"success":true,"data":{"quota_awarded":1,"checkin_date":"2026-08-28"}}`))
		case r.URL.Path == "/api/user/checkin":
			_, _ = w.Write([]byte(`{"success":true,"data":{"stats":{"checked_in_today":false}}}`))
		case r.URL.Path == "/api/user/self":
			_, _ = w.Write([]byte(`{"success":true,"data":{"quota":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	server := newManagementSchedulerTestServer(t)
	cfg := seedManagementEnvelope(t, server, "scheduler-initially-disabled", newDueNewAPIEnvelope(upstream.URL))
	cfg.Enabled = false
	if _, err := server.store.UpdateConfig(context.Background(), cfg.ID, cfg); err != nil {
		t.Fatal(err)
	}

	if err := server.runDueManagementCheckins(context.Background(), time.Now()); err != nil {
		t.Fatalf("runDueManagementCheckins: %v", err)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("disabled channel POST count=%d, want 1", got)
	}
}

func TestManagementCheckinSchedulerMaxFourWorkersAndSameChannelOnce(t *testing.T) {
	var active, maximum, posts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := active.Add(1)
		for {
			seen := maximum.Load()
			if current <= seen || maximum.CompareAndSwap(seen, current) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		defer active.Add(-1)
		switch r.URL.Path {
		case "/api/status":
			_, _ = w.Write([]byte(`{"success":true,"data":{"checkin_enabled":true}}`))
		case "/api/user/checkin":
			if r.Method == http.MethodPost {
				posts.Add(1)
				_, _ = w.Write([]byte(`{"success":true,"data":{"quota_awarded":1,"checkin_date":"2026-08-26"}}`))
			} else {
				_, _ = w.Write([]byte(`{"success":true,"data":{"stats":{"checked_in_today":false}}}`))
			}
		case "/api/user/self":
			_, _ = w.Write([]byte(`{"success":true,"data":{"quota":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	server := newManagementSchedulerTestServer(t)
	var cfgs []*model.Config
	for i := range 6 {
		cfgs = append(cfgs, seedManagementEnvelope(t, server, "scheduler-concurrent-"+string(rune('a'+i)), newDueNewAPIEnvelope(upstream.URL)))
	}
	if err := server.runDueManagementCheckins(context.Background(), time.Now()); err != nil {
		t.Fatalf("runDueManagementCheckins: %v", err)
	}
	if got := maximum.Load(); got > channelManagementScheduleWorkers {
		t.Fatalf("maximum concurrent upstream requests=%d, want <=%d", got, channelManagementScheduleWorkers)
	}
	if got := posts.Load(); got != int32(len(cfgs)) {
		t.Fatalf("POST count=%d, want %d", got, len(cfgs))
	}
	if err := server.runDueManagementCheckins(context.Background(), time.Now()); err != nil {
		t.Fatalf("second runDueManagementCheckins: %v", err)
	}
	if got := posts.Load(); got != int32(len(cfgs)) {
		t.Fatalf("same-day duplicate POST count=%d, want %d", got, len(cfgs))
	}
}

/**
 * @brief 在持久化边界注入调度取消及 CAS 冲突，复用真实 SQLite 行为。
 */
type schedulerStore struct {
	storage.Store                                                     ///< 实际存储。
	mu             sync.Mutex                                         ///< 串行化注入状态。
	conflictID     int64                                              ///< 首次 CAS 冲突的渠道。
	conflicted     bool                                               ///< 是否已注入冲突。
	claimErrorID   int64                                              ///< 写入失败的渠道。
	disableAfterID int64                                              ///< CAS 后禁用的渠道。
	day            string                                             ///< 并发领取的日期。
	beforeCAS      func(context.Context, int64, string, string) error ///< 写入前注入并发更新。
	afterCAS       func()                                             ///< 成功写入后注入取消。
}

/**
 * @brief 在真实 CAS 前后注入测试事件。
 * @param ctx 存储上下文。
 * @param id 渠道 ID。
 * @param expected 待匹配封套。
 * @param next 新封套。
 * @return CAS 成功为 true；冲突或失败为 false。
 * @return 注入或存储失败时的错误；成功或冲突为 nil。
 * @note 持锁执行回调，回调不得递归进入此替身。
 */
func (s *schedulerStore) CompareAndSwapChannelManagement(ctx context.Context, id int64, expected, next string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.beforeCAS != nil {
		if err := s.beforeCAS(ctx, id, expected, next); err != nil {
			return false, err
		}
	}
	if id == s.claimErrorID {
		return false, errors.New("forced claim failure")
	}
	if id == s.conflictID && !s.conflicted {
		s.conflicted = true
		return false, nil
	}
	updated, err := s.Store.CompareAndSwapChannelManagement(ctx, id, expected, next)
	if err == nil && updated && s.afterCAS != nil {
		s.afterCAS()
	}
	if err == nil && updated && id == s.disableAfterID {
		cfg, getErr := s.Store.GetConfig(ctx, id)
		if getErr == nil {
			cfg.Enabled = false
			_, err = s.UpdateConfig(ctx, id, cfg)
		}
	}
	return updated, err
}

func (s *schedulerStore) GetConfig(ctx context.Context, id int64) (*model.Config, error) {
	cfg, err := s.Store.GetConfig(ctx, id)
	if err != nil || cfg == nil || id != s.conflictID || s.day == "" {
		return cfg, err
	}
	envelope, parseErr := model.ParseChannelManagementEnvelope(cfg.OAuthCredential)
	if parseErr != nil {
		return cfg, nil
	}
	envelope.State.LastScheduledDay = s.day
	cfg.OAuthCredential, _ = envelope.Marshal()
	return cfg, nil
}

func TestManagementCheckinSchedulerClaimConflictSkipsUpstream(t *testing.T) {
	var requests atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected upstream request", http.StatusInternalServerError)
	}))
	server := newManagementSchedulerTestServer(t)
	cfg := seedManagementEnvelope(t, server, "scheduler-claim-conflict", newDueNewAPIEnvelope(upstream.URL))
	wrapped := &schedulerStore{Store: server.store, conflictID: cfg.ID, day: time.Now().In(time.Local).Format("2006-01-02")}
	server.store = wrapped
	server.channelManagement.store = wrapped
	if err := server.runDueManagementCheckins(context.Background(), time.Now()); err != nil {
		t.Fatalf("runDueManagementCheckins: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("claim conflict made %d upstream requests, want 0", requests.Load())
	}
}

func TestManagementCheckinSchedulerClaimFailureKeepsEarlierClaims(t *testing.T) {
	var posts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/status":
			_, _ = w.Write([]byte(`{"success":true,"data":{"checkin_enabled":true}}`))
		case r.URL.Path == "/api/user/checkin" && r.Method == http.MethodPost:
			posts.Add(1)
			_, _ = w.Write([]byte(`{"success":true,"data":{"quota_awarded":1,"checkin_date":"2026-08-26"}}`))
		case r.URL.Path == "/api/user/checkin":
			_, _ = w.Write([]byte(`{"success":true,"data":{"stats":{"checked_in_today":false}}}`))
		case r.URL.Path == "/api/user/self":
			_, _ = w.Write([]byte(`{"success":true,"data":{"quota":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	server := newManagementSchedulerTestServer(t)
	first := seedManagementEnvelope(t, server, "scheduler-claim-first", newDueNewAPIEnvelope(upstream.URL))
	second := seedManagementEnvelope(t, server, "scheduler-claim-error", newDueNewAPIEnvelope(upstream.URL))
	wrapped := &schedulerStore{Store: server.store, claimErrorID: second.ID}
	server.store = wrapped
	if err := server.runDueManagementCheckins(context.Background(), time.Now()); err == nil {
		t.Fatal("claim failure returned nil, want scan error")
	}
	_ = first
	if got := posts.Load(); got != 1 {
		t.Fatalf("POST count=%d, want 1 earlier claimed channel to execute", got)
	}
}

func TestManagementCheckinSchedulerRereadsDisabledAndExecutes(t *testing.T) {
	var posts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/status":
			_, _ = w.Write([]byte(`{"success":true,"data":{"checkin_enabled":true}}`))
		case r.URL.Path == "/api/user/checkin" && r.Method == http.MethodPost:
			posts.Add(1)
			_, _ = w.Write([]byte(`{"success":true,"data":{"quota_awarded":1,"checkin_date":"2026-08-28"}}`))
		case r.URL.Path == "/api/user/checkin":
			_, _ = w.Write([]byte(`{"success":true,"data":{"stats":{"checked_in_today":false}}}`))
		case r.URL.Path == "/api/user/self":
			_, _ = w.Write([]byte(`{"success":true,"data":{"quota":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	server := newManagementSchedulerTestServer(t)
	cfg := seedManagementEnvelope(t, server, "scheduler-reread-disabled", newDueNewAPIEnvelope(upstream.URL))
	wrapped := &schedulerStore{Store: server.store, disableAfterID: cfg.ID}
	server.store = wrapped
	server.channelManagement.store = wrapped
	if err := server.runDueManagementCheckins(context.Background(), time.Now()); err != nil {
		t.Fatalf("runDueManagementCheckins: %v", err)
	}
	if got := posts.Load(); got != 1 {
		t.Fatalf("disabled reread POST count=%d, want 1", got)
	}
	logs, err := server.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 10, 0, &model.LogFilter{LogSource: model.LogSourceCheckin, ChannelID: &cfg.ID})
	if err != nil || len(logs) != 1 || !strings.Contains(logs[0].Message, newAPICheckinSuccess) {
		t.Fatalf("disabled audit logs=%#v err=%v", logs, err)
	}
}

/**
 * @brief 取消不产生失败审计，且同日重扫仍能补签所有未完成的管理账户。
 * @param t 当前测试上下文。
 */
func TestManagementCheckinSchedulerCancellationSkipsFalseAuditAndLoopStops(t *testing.T) {
	started := make(chan struct{})
	var firstRequest sync.Once
	var resume atomic.Bool
	var posts atomic.Int32
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/status" && !resume.Load() {
			firstRequest.Do(func() { close(started) })
			<-r.Context().Done()
			return
		}
		switch {
		case r.URL.Path == "/api/status":
			_, _ = io.WriteString(w, `{"success":true,"data":{"checkin_enabled":true}}`)
		case r.URL.Path == "/api/user/checkin" && r.Method == http.MethodPost:
			posts.Add(1)
			_, _ = io.WriteString(w, `{"success":true,"data":{"quota_awarded":1,"checkin_date":"2026-08-26"}}`)
		case r.URL.Path == "/api/user/checkin":
			_, _ = io.WriteString(w, `{"success":true,"data":{"stats":{"checked_in_today":false}}}`)
		case r.URL.Path == "/api/user/self":
			_, _ = io.WriteString(w, `{"success":true,"data":{"quota":1}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	server := newManagementSchedulerTestServer(t)
	/**
	 * @brief 超过 worker 数量，同时覆盖已开始与尚未派发的持久 claim。
	 */
	const channelCount = 6
	for i := range channelCount {
		seedManagementEnvelope(t, server, fmt.Sprintf("scheduler-cancel-%d", i), newDueNewAPIEnvelope(upstream.URL))
	}
	now := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.runDueManagementCheckins(ctx, now) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scheduler error=%v, want context canceled", err)
	}
	logs, err := server.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 10, 0, &model.LogFilter{LogSource: model.LogSourceCheckin})
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 0 {
		t.Fatalf("canceled scheduler wrote %d audit logs, want 0", len(logs))
	}
	resume.Store(true)
	if err := server.runDueManagementCheckins(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != channelCount {
		t.Fatalf("same-day catch-up POST count=%d, want %d", posts.Load(), channelCount)
	}
	if err := server.runDueManagementCheckins(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != channelCount {
		t.Fatalf("completed catch-up repeated POSTs: %d", posts.Load())
	}

	loop := &Server{baseCtx: context.Background(), shutdownCh: make(chan struct{})}
	loop.wg.Add(1)
	loopDone := make(chan struct{})
	go func() { loop.managementCheckinLoop(); close(loopDone) }()
	close(loop.shutdownCh)
	select {
	case <-loopDone:
	case <-time.After(time.Second):
		t.Fatal("managementCheckinLoop did not stop on shutdown")
	}
}

/**
 * @brief 取消恢复只合并本轮调度日期，CAS 冲突不得覆盖并发状态或新账号。
 * @param t 当前测试上下文。
 */
func TestManagementCheckinSchedulerCancellationPreservesConcurrentState(t *testing.T) {
	for _, changeIdentity := range []bool{false, true} {
		t.Run(fmt.Sprintf("identity_changed=%v", changeIdentity), func(t *testing.T) {
			server := newManagementSchedulerTestServer(t)
			now := time.Now()
			yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
			envelope := newDueNewAPIEnvelope("https://unused.example.test")
			envelope.State.LastScheduledDay = yesterday
			cfg := seedManagementEnvelope(t, server, "cancel-before-dispatch", envelope)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wrapped := &schedulerStore{Store: server.store, afterCAS: cancel}
			var changed bool
			wrapped.beforeCAS = func(ctx context.Context, id int64, expected, next string) error {
				candidate, err := model.ParseChannelManagementEnvelope(next)
				if err != nil || changed || candidate.State.LastScheduledDay != yesterday {
					return err
				}
				// 恢复 CAS 前写入另一项状态，使首次 CAS 冲突，验证重读后的字段合并。
				concurrent, err := model.ParseChannelManagementEnvelope(expected)
				if err != nil {
					return err
				}
				concurrent.Settings.DailyCheckinTime = "09:37"
				if changeIdentity {
					concurrent.Settings.AccessToken = "replacement-account"
				}
				raw, err := concurrent.Marshal()
				if err != nil {
					return err
				}
				updated, err := wrapped.Store.CompareAndSwapChannelManagement(ctx, id, expected, raw)
				if err == nil && !updated {
					return errors.New("concurrent test update lost CAS")
				}
				changed = updated
				return err
			}
			server.store = wrapped
			if err := server.runDueManagementCheckins(ctx, now); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel result=%v", err)
			}
			stored, err := server.store.GetConfig(context.Background(), cfg.ID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := model.ParseChannelManagementEnvelope(stored.OAuthCredential)
			if err != nil {
				t.Fatal(err)
			}
			wantDay := yesterday
			wantToken := envelope.Settings.AccessToken
			if changeIdentity {
				wantDay = now.Format("2006-01-02")
				wantToken = "replacement-account"
			}
			if !changed || got.State.LastScheduledDay != wantDay || got.Settings.DailyCheckinTime != "09:37" || got.Settings.AccessToken != wantToken {
				t.Fatalf("restored state: changed=%v day=%s time=%s identity_preserved=%v", changed, got.State.LastScheduledDay, got.Settings.DailyCheckinTime, got.Settings.AccessToken == wantToken)
			}
		})
	}
}

/**
 * @brief 上游已签到但响应期间取消时，同日恢复先回读，不重复提交签到。
 * @param t 当前测试上下文。
 */
func TestManagementCheckinSchedulerCancellationAfterPostRecoversWithoutReplay(t *testing.T) {
	var posts atomic.Int32
	var checked atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/status":
			_, _ = io.WriteString(w, `{"success":true,"data":{"checkin_enabled":true}}`)
		case r.URL.Path == "/api/user/checkin" && r.Method == http.MethodPost:
			// 模拟上游已经提交成功，但调用方未能收到确认。
			posts.Add(1)
			checked.Store(true)
			cancel()
		case r.URL.Path == "/api/user/checkin":
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{"stats": map[string]bool{"checked_in_today": checked.Load()}}})
		case r.URL.Path == "/api/user/self":
			_, _ = io.WriteString(w, `{"success":true,"data":{"quota":1}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	server := newManagementSchedulerTestServer(t)
	cfg := seedManagementEnvelope(t, server, "cancel-after-post", newDueNewAPIEnvelope(upstream.URL))
	now := time.Now()
	if err := server.runDueManagementCheckins(ctx, now); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel result=%v", err)
	}
	if err := server.runDueManagementCheckins(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	stored, err := server.store.GetConfig(context.Background(), cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := model.ParseChannelManagementEnvelope(stored.OAuthCredential)
	if err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 || envelope.State.LastCheckinStatus != newAPICheckinAlreadyChecked || envelope.State.LastScheduledDay != now.Format("2006-01-02") {
		t.Fatalf("recovery posts=%d status=%s scheduled_day=%s", posts.Load(), envelope.State.LastCheckinStatus, envelope.State.LastScheduledDay)
	}
}
