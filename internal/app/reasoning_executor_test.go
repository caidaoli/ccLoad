package app

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

/**
 * @brief 创建按判断模型选路的执行器测试环境
 * @param t 测试上下文
 * @param judge 判断渠道及其模型和凭据
 * @param handler 判断上游响应处理器
 * @return 带独立业务渠道与判断渠道的代理环境
 * @return 判断上游收到请求的原子计数器
 * @note 上游测试服务器随测试结束关闭
 */
func reasoningExecutorEnv(t *testing.T, judge testChannel, handler http.HandlerFunc) (*proxyTestEnv, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(upstream.Close)
	business := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat-1","object":"chat.completion","model":"gpt-5.6","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":1}}`)
	}))
	t.Cleanup(business.Close)
	env := setupProxyTestEnv(t, []testChannel{{name: "business", models: "gpt-5.6", apiKey: "business-key"}, judge}, map[int]string{0: business.URL, 1: upstream.URL})
	env.server.reasoningAuto.Store(&model.ReasoningAutoConfig{Enabled: true, DecisionModel: strings.Split(judge.models, ",")[0], BusinessModels: []string{"gpt-5.6"}, TimeoutMS: 1000})
	return env, calls
}

// editReasoningAuto publishes a modified copy, as saving the setting does.
func editReasoningAuto(s *Server, edit func(*model.ReasoningAutoConfig)) {
	cfg := *s.reasoningAuto.Load()
	edit(&cfg)
	s.reasoningAuto.Store(&cfg)
}

func TestReasoningDecisionSystemOneFixtures(t *testing.T) {
	t.Parallel()
	const usage = `,"usage":{"input_tokens":12,"output_tokens":6}}`
	for _, tc := range []struct {
		name, modelName, response string
		status                    int
		valid                     bool
		unpriced                  bool ///< 判断模型没有内置价格
		effort                    string
		levels                    []string
	}{
		// A judge that answers only the effort question keeps high.
		{name: "typesafe", modelName: "jev-latest", response: `{"model":"jev-1.13.0","answers":{"effort":{"type":"choice","choice":"high","confidence":1,"probabilities":{"low":0,"medium":0,"high":1,"xhigh":0}}}` + usage, status: 200, valid: true},
		{name: "nace", modelName: "drex-latest", response: `{"model":"drex-v1.5","answers":{"effort":{"type":"choice","choice":"high","confidence":0.97,"probabilities":{"low":0,"medium":0.01,"high":0.98,"xhigh":0.01}}}` + usage, status: 200, valid: true},
		// Depth refines high; a max answer stops one level below max.
		{name: "depth max capped at xhigh", modelName: "jev-latest", response: `{"model":"jev-1.14.0","answers":{"effort":{"type":"choice","choice":"high","confidence":0.6},"depth":{"type":"choice","choice":"max","confidence":0.2}}` + usage, status: 200, valid: true, effort: "xhigh", levels: []string{"low", "medium", "high", "xhigh", "max"}},
		{name: "no xhigh skips depth and caps max", modelName: "jev-latest", response: `{"model":"jev-1.14.0","answers":{"effort":{"type":"choice","choice":"max","confidence":0.6},"depth":{"type":"choice","choice":"max"}}` + usage, status: 200, valid: true, levels: []string{"low", "medium", "high", "max"}},
		{name: "depth below high ignored", modelName: "jev-latest", response: `{"model":"jev-1.14.0","answers":{"effort":{"type":"choice","choice":"medium","confidence":0.6},"depth":{"type":"choice","choice":"xhigh"}}` + usage, status: 200, valid: true, effort: "medium"},
		{name: "no depth above high", modelName: "jev-latest", response: `{"model":"jev-1.14.0","answers":{"effort":{"type":"choice","choice":"high"},"depth":{"type":"choice","choice":"xhigh"}}` + usage, status: 200, valid: true, levels: []string{"low", "medium", "high"}},
		// Calibrated models read probability mass: a threshold reached at the
		// reported two-decimal precision promotes, and depth no longer follows its choice.
		{name: "jev mass reaches high", modelName: "jev-latest", response: `{"model":"jev-1.14.0","answers":{"effort":{"type":"choice","choice":"medium","confidence":0.24,"probabilities":{"low":0.1,"medium":0.5,"high":0.36,"xhigh":0.04,"max":0}},"depth":{"type":"choice","choice":"xhigh","probabilities":{"high":0.4,"xhigh":0.55,"max":0.05}}}` + usage, status: 200, valid: true, effort: "high", levels: []string{"low", "medium", "high", "xhigh", "max"}},
		{name: "jev mass reaches xhigh", modelName: "jev-latest", response: `{"model":"jev-1.14.0","answers":{"effort":{"type":"choice","choice":"high","confidence":0.5,"probabilities":{"low":0,"medium":0.2,"high":0.5,"xhigh":0.3,"max":0}},"depth":{"type":"choice","choice":"max","probabilities":{"high":0.35,"xhigh":0.25,"max":0.4}}}` + usage, status: 200, valid: true, effort: "xhigh", levels: []string{"low", "medium", "high", "xhigh", "max"}},
		{name: "d3 lower thresholds", modelName: "vllm-sr/d3-ultra", response: `{"model":"vllm-sr/d3-ultra","answers":{"effort":{"type":"choice","choice":"medium","confidence":0.7,"probabilities":{"low":0.1,"medium":0.68,"high":0.2,"xhigh":0.01,"max":0.01}},"depth":{"type":"choice","choice":"high","probabilities":{"high":0.6,"xhigh":0.35,"max":0.05}}}` + usage, status: 200, valid: true, unpriced: true, effort: "xhigh", levels: []string{"low", "medium", "high", "xhigh", "max"}},
		{name: "uncalibrated model keeps choice", modelName: "drex-latest", response: `{"model":"drex-v2.0","answers":{"effort":{"type":"choice","choice":"medium","confidence":0.5,"probabilities":{"low":0,"medium":0.5,"high":0.5,"xhigh":0}}}` + usage, status: 200, valid: true, effort: "medium"},
		{name: "calibrated probabilities missing level", modelName: "jev-latest", response: `{"answers":{"effort":{"type":"choice","choice":"medium","probabilities":{"low":0.5,"medium":0.5}}}}`, status: 200},
		{name: "missing answer", modelName: "jev-latest", response: `{"answers":{}}`, status: 200},
		{name: "unknown choice", modelName: "drex-latest", response: `{"answers":{"effort":{"type":"choice","choice":"max"}}}`, status: 200},
		{name: "unknown depth choice", modelName: "jev-latest", response: `{"answers":{"effort":{"type":"choice","choice":"high"},"depth":{"type":"choice","choice":"max"}}}`, status: 200},
		{name: "invalid confidence", modelName: "jev-latest", response: `{"answers":{"effort":{"type":"choice","choice":"high","confidence":2}}}`, status: 200},
		{name: "wrong confidence type", modelName: "drex-latest", response: `{"answers":{"effort":{"type":"choice","choice":"high","confidence":"0.9"}}}`, status: 200},
		{name: "authentication", modelName: "jev-latest", response: `{"error":{"type":"authentication_error"}}`, status: 401},
		{name: "billing", modelName: "drex-latest", response: `{"error":{"type":"billing_error"}}`, status: 402},
		{name: "rate limit", modelName: "drex-latest", response: `{"error":{"type":"rate_limit_error"}}`, status: 429},
		{name: "overloaded", modelName: "drex-latest", response: `{"error":{"type":"overloaded_error"}}`, status: 529},
		{name: "non JSON", modelName: "jev-latest", response: `upstream unavailable`, status: 200},
		{name: "oversize", modelName: "drex-latest", response: strings.Repeat("x", reasoningResponseMaxBytes+1), status: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			levels, effort := tc.levels, tc.effort
			if levels == nil {
				levels = reasoningDefaultLevels
			}
			if effort == "" {
				effort = "high"
			}
			// Both questions list their levels in ascending order; depth asks only
			// for the levels from high upward and is omitted without xhigh.
			var depthLevels []string
			if i := slices.Index(levels, "high"); slices.Contains(levels[i:], "xhigh") {
				depthLevels = levels[i:]
			}
			env, calls := reasoningExecutorEnv(t, testChannel{name: "judge", models: tc.modelName, apiKey: "judge-key", authType: model.AuthTypeSystemOne}, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer judge-key" {
					t.Errorf("wire=%s auth=%s", r.URL.Path, r.Header.Get("Authorization"))
				}
				body, _ := io.ReadAll(r.Body)
				keys := func(path string) []string {
					var out []string
					gjson.GetBytes(body, path).ForEach(func(key, _ gjson.Result) bool {
						out = append(out, key.String())
						return true
					})
					return out
				}
				if gjson.GetBytes(body, "questions.effort.type").String() != "choice" || !slices.Equal(keys("questions.effort.criteria"), levels) {
					t.Errorf("invalid effort question=%s", body)
				}
				depth := gjson.GetBytes(body, "questions.depth")
				if depthLevels == nil && depth.Exists() || depthLevels != nil && (depth.Get("type").String() != "choice" || depth.Get("instructions").String() == "" || !slices.Equal(keys("questions.depth.criteria"), depthLevels)) {
					t.Errorf("invalid depth question=%s", body)
				}
				if !strings.Contains(gjson.GetBytes(body, "state").String(), "task data") {
					t.Error("context missing")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.response)
			})
			result, err := env.server.executeReasoningDecision(context.Background(), "task data", levels)
			if (err == nil) != tc.valid {
				t.Fatalf("decision=%+v, error=%v", result, err)
			}
			if tc.valid && (result.effort != effort || result.costUnknown != tc.unpriced || result.usage.InputTokens != 12 || result.usage.OutputTokens != 6) {
				t.Fatalf("decision=%+v", result)
			}
			// The confidence gate reads the effort answer, never the depth answer.
			if want := gjson.Get(tc.response, "answers.effort.confidence"); tc.valid && want.Exists() && (result.confidence == nil || *result.confidence != want.Float()) {
				t.Fatalf("confidence=%v, want effort confidence %v", result.confidence, want.Float())
			}
			if calls.Load() != 1 {
				t.Fatalf("decision calls=%d", calls.Load())
			}
			cold, _ := env.store.GetAllChannelCooldowns(context.Background())
			if _, exists := cold[1]; exists {
				t.Fatal("decision cooled business channel")
			}
			if tc.name == "oversize" {
				return
			}
			entry := waitForDecisionLog(t, env)
			if tc.valid && entry.ResponseModel != gjson.Get(tc.response, "model").String() {
				t.Fatalf("decision log response model=%q", entry.ResponseModel)
			}
			// The failure log keeps the cause and the upstream body.
			if !tc.valid && (!strings.HasPrefix(entry.Message, "reasoning decision failed: ") || !strings.Contains(entry.Message, tc.response)) {
				t.Fatalf("decision log message=%q", entry.Message)
			}
		})
	}
}

func waitForDecisionLog(t *testing.T, env *proxyTestEnv) *model.LogEntry {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		logs, err := env.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 10, 0, &model.LogFilter{ClientProtocol: util.ProtocolSystemOne})
		if err != nil {
			t.Fatal(err)
		}
		if len(logs) == 1 {
			return logs[0]
		}
	}
	t.Fatal("decision log not written")
	return nil
}

// With debug logging on, the decision call keeps the System One request it sent
// and the raw upstream answer (or the transport error) like any proxy attempt.
func TestReasoningDecisionDebugLog(t *testing.T) {
	t.Parallel()
	const answer = `{"model":"jev-1.13.0","answers":{"effort":{"type":"choice","choice":"high","confidence":0.9}},"usage":{"input_tokens":12}}`
	for _, tc := range []struct {
		name         string
		debug, stall bool
	}{
		{name: "decided", debug: true},
		{name: "timeout", debug: true, stall: true},
		{name: "disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, _ := reasoningExecutorEnv(t, testChannel{name: "judge", models: "jev-latest", apiKey: "judge-key", authType: model.AuthTypeSystemOne}, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if tc.stall {
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, answer)
			})
			editReasoningAuto(env.server, func(cfg *model.ReasoningAutoConfig) { cfg.TimeoutMS = 200 })
			if tc.debug {
				env.server.configService.mu.Lock()
				env.server.configService.cache["debug_log_enabled"] = &model.SystemSetting{Key: "debug_log_enabled", Value: "true"}
				env.server.configService.mu.Unlock()
			}
			_, _ = env.server.executeReasoningDecision(context.Background(), "task data", reasoningDefaultLevels)
			entry := waitForDecisionLog(t, env)
			var debug *model.DebugLogEntry
			for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				if debug, _ = env.store.GetDebugLogByLogID(context.Background(), entry.ID); debug != nil || !tc.debug {
					break
				}
			}
			if !tc.debug {
				if debug != nil {
					t.Fatalf("debug disabled but decision debug log written: %+v", debug)
				}
				return
			}
			if debug == nil {
				t.Fatal("decision debug log not written")
			}
			if !strings.HasSuffix(debug.ReqURL, "/v1/systemone") || !strings.Contains(gjson.GetBytes(debug.ReqBody, "state").String(), "task data") {
				t.Fatalf("debug request url=%s body=%s", debug.ReqURL, debug.ReqBody)
			}
			if strings.Contains(debug.ReqHeaders, "judge-key") {
				t.Fatalf("debug headers leak credential: %s", debug.ReqHeaders)
			}
			if tc.stall {
				if debug.UpstreamError == "" || debug.RespStatus != 0 {
					t.Fatalf("timeout debug status=%d error=%q", debug.RespStatus, debug.UpstreamError)
				}
				return
			}
			if debug.RespStatus != http.StatusOK || string(debug.RespBody) != answer {
				t.Fatalf("debug response status=%d body=%s", debug.RespStatus, debug.RespBody)
			}
		})
	}
}

func TestReasoningDecisionTimeoutCancellationAndCapacity(t *testing.T) {
	t.Parallel()
	env, calls := reasoningExecutorEnv(t, testChannel{name: "judge", models: "jev-latest", apiKey: "judge-key", authType: model.AuthTypeSystemOne}, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(500 * time.Millisecond):
		}
	})
	editReasoningAuto(env.server, func(cfg *model.ReasoningAutoConfig) { cfg.TimeoutMS = 100 })
	start := time.Now()
	result, err := env.server.executeReasoningDecision(context.Background(), "task data", reasoningDefaultLevels)
	if err == nil || result.reason != "timeout" || time.Since(start) > time.Second {
		t.Fatalf("timeout result=%+v, error=%v", result, err)
	}
	for range cap(env.server.reasoningDecisionSlots) {
		env.server.reasoningDecisionSlots <- struct{}{}
	}
	start = time.Now()
	result, err = env.server.executeReasoningDecision(context.Background(), "task data", reasoningDefaultLevels)
	if err == nil || result.reason != "queue_full" || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("capacity result=%+v, error=%v", result, err)
	}
	for range cap(env.server.reasoningDecisionSlots) {
		<-env.server.reasoningDecisionSlots
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = env.server.executeReasoningDecision(ctx, "task data", reasoningDefaultLevels)
	if err == nil || calls.Load() != 1 {
		t.Fatalf("cancel error=%v, calls=%d", err, calls.Load())
	}
}

func TestReasoningDecisionMissingUsageWithCustomPrice(t *testing.T) {
	t.Parallel()
	for _, usage := range []string{``, `,"usage":{"input_tokens":"12","output_tokens":-1}`} {
		t.Run(fmt.Sprintf("usage=%q", usage), func(t *testing.T) {
			inputPrice, outputPrice := 1.0, 2.0
			env, calls := reasoningExecutorEnv(t, testChannel{name: "judge", models: "judge", authType: model.AuthTypeSystemOne, apiKey: "judge-key",
				modelEntries: []model.ModelEntry{{Model: "judge", RedirectModel: "jev-latest", Pricing: &util.CustomModelPrice{InputPrice: &inputPrice, OutputPrice: &outputPrice}}}}, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"answers":{"effort":{"type":"choice","choice":"high"}}`+usage+`}`)
			})
			result, err := env.server.executeReasoningDecision(context.Background(), "task data", reasoningDefaultLevels)
			if err != nil || result.effort != "high" || !result.costUnknown || calls.Load() != 1 {
				t.Fatalf("decision=%+v error=%v calls=%d", result, err, calls.Load())
			}
			if cost := env.server.costCache.Get(2); cost != 0 {
				t.Fatalf("unknown usage charged %v", cost)
			}
		})
	}
}

func TestReasoningDecisionRejectsChatChannel(t *testing.T) {
	t.Parallel()
	env, calls := reasoningExecutorEnv(t, testChannel{name: "judge", models: "gpt-5.6", apiKey: "judge-key", upstreamProtocol: "openai"}, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	result, err := env.server.executeReasoningDecision(context.Background(), "task data", reasoningDefaultLevels)
	if err == nil || result.effort != "" || calls.Load() != 0 {
		t.Fatalf("decision=%+v error=%v calls=%d", result, err, calls.Load())
	}
}

/**
 * @brief 判断渠道失败后按模型切换渠道，并独立记录各次尝试的渠道、状态与用量
 * @param t 测试上下文
 */
func TestReasoningDecisionModelFailover(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		status        int
		response      string
		reason        string
		input, output int
	}{
		{name: "unavailable", status: http.StatusServiceUnavailable, response: `{"error":{"message":"unavailable"}}`, reason: "upstream_error"},
		{name: "rate limited", status: http.StatusTooManyRequests, response: `{"error":{"message":"limited"}}`, reason: "rate_limited"},
		{name: "unauthorized", status: http.StatusUnauthorized, response: `{"error":{"message":"unauthorized"}}`, reason: "authentication_failed"},
		{name: "invalid answer", status: http.StatusOK, response: `{"answers":{},"usage":{"input_tokens":7,"output_tokens":2}}`, reason: "invalid_response", input: 7, output: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			first := newReasoningAutoJudge(t)
			first.respond = func(w http.ResponseWriter, _ *http.Request, _ int) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.response)
			}
			second := newReasoningAutoJudge(t, "xhigh")
			business := newReasoningAutoBusiness(t)
			env := setupReasoningAutoTestEnv(t, first.server.URL, []testChannel{
				{name: "business", upstreamProtocol: "openai", models: "gpt-5.6-sol"},
				{name: "second-judge", authType: model.AuthTypeSystemOne, models: "judge", apiKey: "sk-judge", priority: -10},
			}, []string{business.server.URL, second.server.URL})
			reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Analyze concurrent migration failures."), nil)
			assertReasoningAutoEfforts(t, business.snapshot(), "xhigh")
			if len(first.snapshot()) != 1 || len(second.snapshot()) != 1 {
				t.Fatalf("decision attempts: first=%d second=%d", len(first.snapshot()), len(second.snapshot()))
			}
			for _, want := range []struct {
				channelID     int64
				status        int
				input, output int
				decision      string
				reason        string
			}{
				{channelID: 1, status: tc.status, input: tc.input, output: tc.output, decision: "fallback", reason: tc.reason},
				{channelID: 3, status: http.StatusOK, input: 1000, output: 100, decision: "decided"},
			} {
				entry := waitForProxyLogMatching(t, env, func(entry *model.LogEntry) bool {
					return entry.ClientProtocol == "system_one" && entry.ChannelID == want.channelID
				})
				if entry == nil || entry.StatusCode != want.status || entry.Model != "judge" || entry.InputTokens != want.input || entry.OutputTokens != want.output || entry.AuthTokenID != 0 {
					t.Fatalf("channel %d attempt log=%+v", want.channelID, entry)
				}
				if entry.ReasoningDecision == nil || entry.ReasoningDecision.Status != want.decision || entry.ReasoningDecision.Reason != want.reason {
					t.Fatalf("channel %d decision metadata=%+v", want.channelID, entry.ReasoningDecision)
				}
			}
			entry := waitForProxyLog(t, env, "gpt-5.6-sol")
			if entry.ReasoningDecision == nil || entry.ReasoningDecision.Status != "decided" || entry.ReasoningDecision.SuggestedEffort != "xhigh" {
				t.Fatalf("business decision metadata=%+v", entry.ReasoningDecision)
			}
		})
	}
}

/**
 * @brief 判断模型选路跳过不可用渠道及未授权模型行，保留选定行的重定向与定价
 * @param t 测试上下文
 */
func TestReasoningDecisionModelCandidates(t *testing.T) {
	t.Parallel()
	cooled := newReasoningAutoJudge(t)
	var rejectedCalls, selectedCalls atomic.Int32
	rejected := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		rejectedCalls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(rejected.Close)
	selected := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		selectedCalls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer allowed-key" || gjson.GetBytes(body, "model").String() != "judge-v2" {
			t.Errorf("selected decision request: path=%s authorization=%s body=%s", r.URL.Path, r.Header.Get("Authorization"), body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"answers":{"effort":{"type":"choice","choice":"xhigh"}},"usage":{"input_tokens":12,"output_tokens":6}}`)
	}))
	t.Cleanup(selected.Close)
	business := newReasoningAutoBusiness(t)
	inputPrice, outputPrice, unavailablePrice := 1.0, 2.0, 99.0
	env := setupReasoningAutoTestEnv(t, cooled.server.URL, []testChannel{
		{name: "business", upstreamProtocol: "openai", models: "gpt-5.6-sol"},
		{name: "chat-judge", upstreamProtocol: "openai", models: "judge", priority: 300},
		{name: "disabled-judge", authType: model.AuthTypeSystemOne, models: "judge", priority: 200},
		{name: "selected-judge", authType: model.AuthTypeSystemOne, priority: 10, modelEntries: []model.ModelEntry{
			{Model: "judge", RedirectModel: "judge-v1", Pricing: &util.CustomModelPrice{InputPrice: &unavailablePrice, OutputPrice: &unavailablePrice}},
			{Model: "judge", RedirectModel: "judge-v2", Pricing: &util.CustomModelPrice{InputPrice: &inputPrice, OutputPrice: &outputPrice}},
		}},
	}, []string{business.server.URL, rejected.URL, rejected.URL, selected.URL})
	ctx := context.Background()
	if err := env.store.SetChannelCooldown(ctx, 1, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	disabled, err := env.store.GetConfig(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	disabled.Enabled = false
	if _, err := env.store.UpdateConfig(ctx, disabled.ID, disabled); err != nil {
		t.Fatal(err)
	}
	if err := env.store.DeleteAllAPIKeys(ctx, 5); err != nil {
		t.Fatal(err)
	}
	if err := env.store.CreateAPIKeysBatch(ctx, []*model.APIKey{
		{ChannelID: 5, KeyIndex: 0, APIKey: "denied-key", Priority: 100, AllowedModels: []string{"other-model"}},
		{ChannelID: 5, KeyIndex: 1, APIKey: "allowed-key", Priority: -10, AllowedModels: []string{"judge"}, DetectedModels: []string{"judge-v2"}, CostMultiplier: 1},
	}); err != nil {
		t.Fatal(err)
	}
	env.server.InvalidateChannelListCache()
	env.server.InvalidateAPIKeysCache(5)
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("Analyze an unresolved concurrency defect."), nil)
	assertReasoningAutoEfforts(t, business.snapshot(), "xhigh")
	if len(cooled.snapshot()) != 0 || rejectedCalls.Load() != 0 || selectedCalls.Load() != 1 {
		t.Fatalf("candidate calls: cooled=%d rejected=%d selected=%d", len(cooled.snapshot()), rejectedCalls.Load(), selectedCalls.Load())
	}
	entry := waitForDecisionLog(t, env)
	if entry.ChannelID != 5 || entry.Model != "judge" || entry.ActualModel != "judge-v2" {
		t.Fatalf("redirected decision log=%+v", entry)
	}
	if entry.ReasoningDecision == nil || entry.ReasoningDecision.CostUnknown || math.Abs(entry.Cost-0.000024) > 1e-12 {
		t.Fatalf("selected model row pricing: cost=%v decision=%+v", entry.Cost, entry.ReasoningDecision)
	}
}

/**
 * @brief 判断超时或业务请求取消后不再尝试后续渠道
 * @param t 测试上下文
 */
func TestReasoningDecisionFailoverStopsOnContextDone(t *testing.T) {
	t.Parallel()
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled=%v", cancelled), func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			first := newReasoningAutoJudge(t)
			first.respond = func(_ http.ResponseWriter, r *http.Request, _ int) {
				if cancelled {
					cancel()
				}
				<-r.Context().Done()
			}
			second := newReasoningAutoJudge(t)
			business := newReasoningAutoBusiness(t)
			env := setupReasoningAutoTestEnv(t, first.server.URL, []testChannel{
				{name: "business", upstreamProtocol: "openai", models: "gpt-5.6-sol"},
				{name: "second-judge", authType: model.AuthTypeSystemOne, models: "judge", apiKey: "sk-judge", priority: -10},
			}, []string{business.server.URL, second.server.URL})
			editReasoningAuto(env.server, func(cfg *model.ReasoningAutoConfig) { cfg.TimeoutMS = 100 })
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"Analyze concurrent migration failures."}]}`)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer test-api-key")
			response := httptest.NewRecorder()
			start := time.Now()
			env.engine.ServeHTTP(response, req)
			if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
				t.Fatalf("decision exceeded timeout/cancellation budget: %v", elapsed)
			}
			if len(first.snapshot()) != 1 || len(second.snapshot()) != 0 {
				t.Fatalf("decision attempts: first=%d second=%d", len(first.snapshot()), len(second.snapshot()))
			}
			reason := "timeout"
			if cancelled {
				reason = "cancelled"
				if len(business.snapshot()) != 0 {
					t.Fatal("cancelled request reached business upstream")
				}
			} else {
				if response.Code != http.StatusOK || len(business.snapshot()) != 1 {
					t.Fatalf("timeout fallback status=%d business calls=%d", response.Code, len(business.snapshot()))
				}
			}
			entry := waitForDecisionLog(t, env)
			if entry.ReasoningDecision == nil || entry.ReasoningDecision.Reason != reason {
				t.Fatalf("decision metadata=%+v, want %s", entry.ReasoningDecision, reason)
			}
		})
	}
}

func TestReasoningDecisionSystemOneRedirectAndCustomPrice(t *testing.T) {
	t.Parallel()
	inputPrice, outputPrice := 1.0, 2.0
	env, calls := reasoningExecutorEnv(t, testChannel{name: "judge", models: "judge", authType: model.AuthTypeSystemOne, apiKey: "judge-key",
		modelEntries: []model.ModelEntry{{Model: "judge", RedirectModel: "jev-latest", Pricing: &util.CustomModelPrice{InputPrice: &inputPrice, OutputPrice: &outputPrice}}}}, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/systemone" || gjson.GetBytes(body, "model").String() != "jev-latest" {
			t.Errorf("wrong target: %s %s", r.URL.Path, body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"jev-1.13.0","answers":{"effort":{"type":"choice","choice":"high"}},"usage":{"input_tokens":12,"output_tokens":6}}`)
	})
	cfg, err := env.store.GetConfig(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	cfg.URLs[0].URL += "/v1"
	if _, err = env.store.UpdateConfig(context.Background(), 2, cfg); err != nil {
		t.Fatal(err)
	}
	if err = env.store.UpdateAPIKeyCostMultipliers(context.Background(), 2, map[int]float64{0: 1}); err != nil {
		t.Fatal(err)
	}
	result, err := env.server.executeReasoningDecision(context.Background(), "task data", reasoningDefaultLevels)
	if err != nil || result.effort != "high" || result.actualModel != "jev-latest" || result.costUnknown || calls.Load() != 1 {
		t.Fatalf("decision=%+v error=%v calls=%d", result, err, calls.Load())
	}
	if cost := env.server.costCache.Get(2); cost <= 0 {
		t.Fatalf("known custom price not charged: %v", cost)
	}
}

// The database honors context cancellation but would otherwise outlast the judge budget.
type slowReasoningCooldownStore struct{ storage.Store }

func (s slowReasoningCooldownStore) BumpModelCooldown(ctx context.Context, _ int64, _ string, _ time.Time, _ int) (time.Duration, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(600 * time.Millisecond):
		return 0, fmt.Errorf("simulated slow database")
	}
}

/**
 * @brief 冷却持久化耗尽判断预算后业务请求降级，且不再尝试下一判断渠道
 * @param t 测试上下文
 */
func TestReasoningDecisionErrorRespectsTimeoutBudget(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t)
	judge.respond = func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"limited"}}`)
	}
	second := newReasoningAutoJudge(t)
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{
		{name: "business", upstreamProtocol: "openai", models: "gpt-5.6-sol"},
		{name: "second-judge", authType: model.AuthTypeSystemOne, models: "judge", apiKey: "sk-judge", priority: -10},
	}, []string{business.server.URL, second.server.URL})
	editReasoningAuto(env.server, func(cfg *model.ReasoningAutoConfig) { cfg.TimeoutMS = 100 })
	env.server.cooldownManager = cooldown.NewManager(slowReasoningCooldownStore{env.store}, env.server)
	start := time.Now()
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("simple task"), nil)
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Fatalf("timeout_ms=100 but decision error blocked business request for %v", elapsed)
	}
	if len(business.snapshot()) != 1 {
		t.Fatal("business request did not proceed after the decision timeout")
	}
	if len(second.snapshot()) != 0 {
		t.Fatal("decision tried another channel after the cooldown write exhausted its budget")
	}
	decisionLog := waitForDecisionLog(t, env)
	proxyLog := waitForProxyLog(t, env, "gpt-5.6-sol")
	for _, entry := range []*model.LogEntry{decisionLog, proxyLog} {
		if entry.ReasoningDecision == nil || entry.ReasoningDecision.Reason != "timeout" || entry.ReasoningDecision.DurationMS < 90 || entry.ReasoningDecision.DurationMS > 400 {
			t.Fatalf("timeout fallback metadata = %+v", entry.ReasoningDecision)
		}
	}
}

/**
 * @brief 原生决策按重定向后的模型选择协议，提示词模板独立决定评分或双选择题
 * @param t 测试上下文
 */
func TestReasoningDecisionNativeProviders(t *testing.T) {
	t.Parallel()
	const score = `"effort":{"type":"score","score":1.1,"confidence":0.01,"probabilities":{"0":0.1,"1":0.2,"2":0.35,"3":0.35,"4":0}}`
	const choice = `"effort":{"type":"choice","choice":"high","confidence":0.6},"depth":{"type":"choice","choice":"max","confidence":0.1}`
	for _, tc := range []struct {
		name, actualModel, template, basePath, path, questionType, effort, response string
		input, output, cached                                                       int
		unknown                                                                     bool
	}{
		{name: "aliyun root", actualModel: "decision-model-preview", path: "/compatible-mode/v1/systemone", questionType: "score", effort: "high", response: `{"answers":{` + score + `},"usage":{"input_tokens":12}}`, input: 12},
		{name: "gemini version", actualModel: "gemini-decision-1", basePath: "/v1beta", path: "/v1beta/decisions", questionType: "score", effort: "max", response: `{"answers":{"effort":{"type":"score","confidence":0.01,"probabilities":{"0":0.1,"1":0.1,"2":0.1,"3":0.1,"4":0.6}}},"usage":{"total_input_tokens":13,"total_output_tokens":0}}`, input: 13},
		{name: "openai endpoint", actualModel: "gpt-6-luna", basePath: "/v1/decisions", path: "/v1/decisions", questionType: "score", effort: "high", response: `{"model":"gpt-6-luna-served","answers":[{"name":"effort","type":"score","score":0,"confidence":0.01,"probabilities":[{"value":3,"label":"xhigh","probability":0.35},{"value":0,"label":"low","probability":0.1},{"value":4,"label":"max","probability":0},{"value":1,"label":"medium","probability":0.2},{"value":2,"label":"high","probability":0.35}]}],"usage":{"input_tokens":14,"output_tokens":0,"input_tokens_details":{"cached_tokens":8}}}`, input: 6, cached: 8},
		{name: "jev with native template", actualModel: "jev-latest", template: "gemini", basePath: "/v1/systemone", path: "/v1/systemone", questionType: "score", effort: "high", response: `{"answers":{` + score + `},"usage":{"input_tokens":15,"output_tokens":1}}`, input: 15, output: 1},
		{name: "aliyun with jev template", actualModel: "decision-model-preview", template: "jev", basePath: "/compatible-mode/v1", path: "/compatible-mode/v1/systemone", questionType: "choice", effort: "xhigh", response: `{"answers":{` + choice + `},"usage":{"input_tokens":16,"output_tokens":1}}`, input: 16, output: 1},
		{name: "gemini with jev template", actualModel: "gemini-decision-1", template: "jev", basePath: "/v1beta/decisions", path: "/v1beta/decisions", questionType: "choice", effort: "xhigh", response: `{"answers":{` + choice + `},"usage":{"total_input_tokens":17,"total_output_tokens":1}}`, input: 17, output: 1},
		{name: "openai with jev template", actualModel: "gpt-6-luna", template: "jev", basePath: "/v1", path: "/v1/decisions", questionType: "choice", effort: "xhigh", response: `{"answers":[{"name":"effort","type":"choice","choice":"high","confidence":0.6},{"name":"depth","type":"choice","choice":"max","confidence":0.1}],"usage":{"input_tokens":18,"output_tokens":1}}`, input: 18, output: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			levels := []string{"low", "medium", "high", "xhigh", "max"}
			env, calls := reasoningExecutorEnv(t, testChannel{name: "judge", models: "logical-judge", apiKey: "judge-key", authType: model.AuthTypeSystemOne,
				modelEntries: []model.ModelEntry{{Model: "logical-judge", RedirectModel: tc.actualModel, Pricing: channelPrice(1, 2)}}}, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path || r.Method != http.MethodPost {
					t.Errorf("native endpoint=%s %s, want POST %s", r.Method, r.URL.Path, tc.path)
				}
				gemini := strings.HasPrefix(tc.actualModel, "gemini-")
				if gemini && (r.Header.Get("x-goog-api-key") != "judge-key" || r.Header.Get("Authorization") != "") || !gemini && (r.Header.Get("Authorization") != "Bearer judge-key" || r.Header.Get("x-goog-api-key") != "") {
					t.Errorf("native authentication=%v", r.Header)
				}
				body, _ := io.ReadAll(r.Body)
				inputField := "state"
				if gemini || tc.actualModel == "gpt-6-luna" {
					inputField = "input"
				}
				input := gjson.GetBytes(body, inputField)
				if gjson.GetBytes(body, "model").String() != tc.actualModel || input.Type != gjson.String || gjson.Get(input.String(), "conversation").String() != "task data" {
					t.Errorf("native input=%s", body)
				}
				question := gjson.GetBytes(body, "questions.effort")
				if tc.actualModel == "gpt-6-luna" {
					question = gjson.GetBytes(body, "questions.0")
					if question.Get("name").String() != "effort" {
						t.Errorf("native named question=%s", body)
					}
					optionsField, labelField := "levels", "label"
					if tc.questionType == "choice" {
						optionsField, labelField = "choices", "value"
					}
					options := question.Get(optionsField).Array()
					if len(options) != len(levels) {
						t.Errorf("native options=%s", body)
					} else {
						for i, option := range options {
							if option.Get(labelField).String() != levels[i] || option.Get("description").String() == "" {
								t.Errorf("native option=%s", option.Raw)
								break
							}
						}
					}
				} else if tc.questionType == "score" && (!question.Get("criteria").IsArray() || len(question.Get("criteria").Array()) != len(levels)) {
					t.Errorf("native score criteria=%s", body)
				}
				if question.Get("type").String() != tc.questionType || question.Get("instructions").String() == "" {
					t.Errorf("native question=%s", body)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.response)
			})
			editReasoningAuto(env.server, func(cfg *model.ReasoningAutoConfig) { cfg.PromptTemplate = tc.template })
			cfg, err := env.store.GetConfig(context.Background(), 2)
			if err != nil {
				t.Fatal(err)
			}
			cfg.URLs[0].URL += tc.basePath
			if _, err = env.store.UpdateConfig(context.Background(), 2, cfg); err != nil {
				t.Fatal(err)
			}
			env.server.InvalidateChannelListCache()
			result, err := env.server.executeReasoningDecision(context.Background(), "task data", levels)
			if err != nil || result.effort != tc.effort || result.actualModel != tc.actualModel || result.costUnknown != tc.unknown || calls.Load() != 1 {
				t.Fatalf("native decision=%+v error=%v calls=%d", result, err, calls.Load())
			}
			if result.usage.InputTokens != tc.input || result.usage.OutputTokens != tc.output || result.usage.CacheReadInputTokens != tc.cached {
				t.Fatalf("native usage=%+v", result.usage)
			}
			if tc.questionType == "score" && result.confidence != nil || tc.questionType == "choice" && (result.confidence == nil || *result.confidence != 0.6) {
				t.Fatalf("native confidence=%v", result.confidence)
			}
		})
	}
}

/**
 * @brief 大决策请求按提供商实测支持的编码压缩，阿里云与小请求保持明文
 * @param t 测试上下文
 */
func TestReasoningDecisionRequestCompression(t *testing.T) {
	t.Parallel()
	const scoreResponse = `{"answers":{"effort":{"type":"score","score":2,"probabilities":{"0":0,"1":0,"2":1,"3":0,"4":0}}},"usage":{"input_tokens":1}}`
	const openAIResponse = `{"answers":[{"name":"effort","type":"score","score":2,"probabilities":[{"value":0,"label":"low","probability":0},{"value":1,"label":"medium","probability":0},{"value":2,"label":"high","probability":1},{"value":3,"label":"xhigh","probability":0},{"value":4,"label":"max","probability":0}]}],"usage":{"input_tokens":1}}`
	large := strings.Repeat("task data ", 1000)
	for _, tc := range []struct {
		name, actualModel, transcript, encoding, response string
	}{
		{name: "jev zstd", actualModel: "jev-latest", transcript: large, encoding: "zstd", response: scoreResponse},
		{name: "openai zstd", actualModel: "gpt-6-luna", transcript: large, encoding: "zstd", response: openAIResponse},
		{name: "gemini gzip", actualModel: "gemini-decision-1", transcript: large, encoding: "gzip", response: scoreResponse},
		{name: "aliyun plain", actualModel: "decision-model-preview", transcript: large, response: scoreResponse},
		{name: "small plain", actualModel: "jev-latest", transcript: "task data", response: scoreResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, calls := reasoningExecutorEnv(t, testChannel{name: "judge", models: "logical-judge", apiKey: "judge-key", authType: model.AuthTypeSystemOne,
				modelEntries: []model.ModelEntry{{Model: "logical-judge", RedirectModel: tc.actualModel}}}, func(w http.ResponseWriter, r *http.Request) {
				encoding := r.Header.Get("Content-Encoding")
				if encoding != tc.encoding {
					t.Errorf("Content-Encoding=%q, want %q", encoding, tc.encoding)
				}
				// The test transport already decodes zstd request bodies, as chatgpt.com does.
				reader := r.Body
				if encoding == "gzip" {
					var err error
					if reader, err = decodeContentEncodingBody(r.Body, []string{"gzip"}); err != nil {
						t.Errorf("decode body: %v", err)
						return
					}
				}
				body, err := io.ReadAll(reader)
				if err != nil || gjson.GetBytes(body, "model").String() != tc.actualModel || !strings.Contains(string(body), tc.transcript) {
					t.Errorf("decoded body err=%v size=%d", err, len(body))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.response)
			})
			editReasoningAuto(env.server, func(cfg *model.ReasoningAutoConfig) { cfg.PromptTemplate = "gemini" })
			result, err := env.server.executeReasoningDecision(context.Background(), tc.transcript, []string{"low", "medium", "high", "xhigh", "max"})
			if err != nil || result.effort != "high" || calls.Load() != 1 {
				t.Fatalf("decision=%+v error=%v calls=%d", result, err, calls.Load())
			}
		})
	}
}

/**
 * @brief 压缩体被拒时以明文重发；仅明文成功才记住该渠道域名只发明文
 * @param t 测试上下文
 */
func TestReasoningDecisionCompressionFallback(t *testing.T) {
	t.Parallel()
	const response = `{"answers":{"effort":{"type":"score","score":2,"probabilities":{"0":0,"1":0,"2":1,"3":0,"4":0}}},"usage":{"input_tokens":1}}`
	transcript := strings.Repeat("task data ", 1000)
	for _, tc := range []struct {
		name        string
		plainStatus int
		want        []string
	}{
		{name: "plain success remembered", plainStatus: http.StatusOK, want: []string{"zstd", "", ""}},
		{name: "plain failure keeps compression", plainStatus: http.StatusBadRequest, want: []string{"zstd", "", "zstd", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var encodings []string
			env, _ := reasoningExecutorEnv(t, testChannel{name: "judge", models: "jev-latest", apiKey: "judge-key", authType: model.AuthTypeSystemOne}, func(w http.ResponseWriter, r *http.Request) {
				encoding := r.Header.Get("Content-Encoding")
				mu.Lock()
				encodings = append(encodings, encoding)
				mu.Unlock()
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/json")
				if encoding != "" {
					w.WriteHeader(http.StatusUnsupportedMediaType)
					_, _ = io.WriteString(w, `{"error":"unsupported content encoding"}`)
					return
				}
				w.WriteHeader(tc.plainStatus)
				_, _ = io.WriteString(w, response)
			})
			editReasoningAuto(env.server, func(cfg *model.ReasoningAutoConfig) { cfg.PromptTemplate = "gemini" })
			for range 2 {
				result, err := env.server.executeReasoningDecision(context.Background(), transcript, []string{"low", "medium", "high", "xhigh", "max"})
				if tc.plainStatus == http.StatusOK && (err != nil || result.effort != "high") {
					t.Fatalf("decision=%+v error=%v", result, err)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(encodings, tc.want) {
				t.Fatalf("encodings=%q, want %q", encodings, tc.want)
			}
		})
	}
}

/**
 * @brief 原生拒答与损坏概率不能伪造 low，仍保留已发生的用量
 * @param t 测试上下文
 */
func TestReasoningDecisionNativeInvalidAnswers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, actualModel, answer string
	}{
		{name: "refusal", actualModel: "gpt-6-luna", answer: `[{"name":"effort","type":"refusal"}]`},
		{name: "missing probability", actualModel: "decision-model-preview", answer: `{"effort":{"type":"score","probabilities":{"0":0.1,"1":0.2,"2":0.7}}}`},
		{name: "non numeric", actualModel: "decision-model-preview", answer: `{"effort":{"type":"score","probabilities":{"0":"0.1","1":0.2,"2":0.7,"3":0}}}`},
		{name: "negative", actualModel: "decision-model-preview", answer: `{"effort":{"type":"score","probabilities":{"0":-0.1,"1":0.2,"2":0.7,"3":0.2}}}`},
		{name: "above one", actualModel: "decision-model-preview", answer: `{"effort":{"type":"score","probabilities":{"0":1.1,"1":0,"2":0,"3":0}}}`},
		{name: "all zero", actualModel: "decision-model-preview", answer: `{"effort":{"type":"score","score":0,"probabilities":{"0":0,"1":0,"2":0,"3":0}}}`},
		{name: "invalid distribution", actualModel: "decision-model-preview", answer: `{"effort":{"type":"score","probabilities":{"0":1,"1":1,"2":1,"3":1}}}`},
		{name: "unknown level", actualModel: "decision-model-preview", answer: `{"effort":{"type":"score","probabilities":{"0":0.1,"1":0.2,"2":0.7,"other":0}}}`},
		{name: "mismatched label", actualModel: "gpt-6-luna", answer: `[{"name":"effort","type":"score","probabilities":[{"value":0,"label":"xhigh","probability":0.1},{"value":1,"label":"medium","probability":0.1},{"value":2,"label":"high","probability":0.7},{"value":3,"label":"low","probability":0.1}]}]`},
		{name: "duplicate index", actualModel: "gpt-6-luna", answer: `[{"name":"effort","type":"score","probabilities":[{"value":0,"label":"low","probability":0.1},{"value":1,"label":"medium","probability":0.1},{"value":2,"label":"high","probability":0.7},{"value":2,"label":"high","probability":0.1}]}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env, _ := reasoningExecutorEnv(t, testChannel{name: "judge", models: tc.actualModel, authType: model.AuthTypeSystemOne, apiKey: "judge-key"}, func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				_, _ = io.WriteString(w, `{"answers":`+tc.answer+`,"usage":{"input_tokens":12,"output_tokens":0}}`)
			})
			result, err := env.server.executeReasoningDecision(context.Background(), "task data", reasoningDefaultLevels)
			if err == nil || result.effort != "" || result.reason != "invalid_response" || result.usage.InputTokens != 12 {
				t.Fatalf("invalid native decision=%+v error=%v", result, err)
			}
		})
	}
}

/**
 * @brief 判断故障切换沿用一次配置快照，下一次管理测试使用最新自定义提示词
 * @param t 测试上下文
 */
func TestReasoningDecisionPromptSnapshotAndAdminTest(t *testing.T) {
	t.Parallel()
	var env *proxyTestEnv
	var secondCalls atomic.Int32
	first := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		editReasoningAuto(env.server, func(cfg *model.ReasoningAutoConfig) {
			cfg.PromptTemplate = "custom"
			cfg.CustomPrompt = &model.ReasoningPrompt{Instructions: "custom instructions", Criteria: map[string]string{
				"low": "custom low", "medium": "custom medium", "high": "custom high", "xhigh": "custom xhigh", "max": "custom max",
			}}
		})
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(first.Close)
	second := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		question := gjson.GetBytes(body, "questions.0")
		if r.URL.Path != "/v1/decisions" || gjson.GetBytes(body, "model").String() != "gpt-6-luna" {
			t.Errorf("redirected decision path=%s body=%s", r.URL.Path, body)
		}
		w.Header().Set("Content-Type", "application/json")
		if secondCalls.Add(1) == 1 {
			if question.Get("type").String() != "choice" || question.Get("instructions").String() != reasoningDecisionInstructions {
				t.Errorf("decision mixed prompt snapshots: %s", body)
			}
			_, _ = io.WriteString(w, `{"answers":[{"name":"effort","type":"choice","choice":"high","confidence":0.8}],"usage":{"input_tokens":12,"output_tokens":0}}`)
			return
		}
		if question.Get("type").String() != "score" || question.Get("instructions").String() != "custom instructions" || question.Get("levels.0.description").String() != "custom low" || question.Get("levels.3.description").String() != "custom xhigh" {
			t.Errorf("admin test did not use current custom prompt: %s", body)
		}
		_, _ = io.WriteString(w, `{"answers":[{"name":"effort","type":"score","probabilities":[{"value":0,"label":"low","probability":0.1},{"value":1,"label":"medium","probability":0.1},{"value":2,"label":"high","probability":0.1},{"value":3,"label":"xhigh","probability":0.7}]}],"usage":{"input_tokens":12,"output_tokens":0}}`)
	}))
	t.Cleanup(second.Close)
	env = setupReasoningAutoTestEnv(t, first.URL, []testChannel{{name: "second-judge", models: "judge", authType: model.AuthTypeSystemOne,
		modelEntries: []model.ModelEntry{{Model: "judge", RedirectModel: "gpt-6-luna"}}, apiKey: "judge-key", priority: -10}}, []string{second.URL})
	editReasoningAuto(env.server, func(cfg *model.ReasoningAutoConfig) { cfg.PromptTemplate = "jev" })
	result, err := env.server.executeReasoningDecision(context.Background(), "task data", reasoningDefaultLevels)
	if err != nil || result.effort != "high" || secondCalls.Load() != 1 {
		t.Fatalf("snapshot decision=%+v error=%v calls=%d", result, err, secondCalls.Load())
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/2/test", map[string]any{"model": "judge", "content": "task data"}))
	c.Params = gin.Params{{Key: "id", Value: "2"}}
	env.server.HandleChannelTest(c)
	response := mustParseAPIResponse[map[string]any](t, w.Body.Bytes())
	if w.Code != http.StatusOK || response.Data["success"] != true || response.Data["effort"] != "xhigh" || secondCalls.Load() != 2 {
		t.Fatalf("admin native test=%d %s calls=%d", w.Code, w.Body.String(), secondCalls.Load())
	}
}

/**
 * @brief 原生拒答使业务请求沿用原有等级并记录降级，绝不写入伪造的 low
 * @param t 测试上下文
 */
func TestReasoningDecisionNativeRefusalFallback(t *testing.T) {
	t.Parallel()
	judge := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"answers":[{"name":"effort","type":"refusal"}],"usage":{"input_tokens":12,"output_tokens":0}}`)
	}))
	t.Cleanup(judge.Close)
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.URL, []testChannel{{name: "business", upstreamProtocol: "openai", models: "gpt-5.6-sol"}}, []string{business.server.URL})
	cfg, err := env.store.GetConfig(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ModelEntries[0].RedirectModel = "gpt-6-luna"
	if _, err = env.store.UpdateConfig(context.Background(), 1, cfg); err != nil {
		t.Fatal(err)
	}
	env.server.InvalidateChannelListCache()
	body := reasoningAutoChatBody("Investigate an unresolved protocol mismatch.")
	body["reasoning_effort"] = "high"
	reasoningAutoRequest(t, env, "/v1/chat/completions", body, nil)
	assertReasoningAutoEfforts(t, business.snapshot(), "high")
	entry := waitForProxyLog(t, env, "gpt-5.6-sol")
	if entry.ReasoningDecision == nil || entry.ReasoningDecision.Status != "fallback" || entry.ReasoningDecision.Reason != "invalid_response" || entry.ReasoningDecision.SuggestedEffort != "" {
		t.Fatalf("refusal business metadata=%+v", entry.ReasoningDecision)
	}
	decision := waitForDecisionLog(t, env)
	if decision.InputTokens != 12 || decision.ReasoningDecision == nil || decision.ReasoningDecision.Status != "fallback" {
		t.Fatalf("refusal usage log=%+v", decision)
	}
}

/**
 * @brief 内置决策价格在自动判断、管理测试及日志明细中使用同一口径，缓存用量只计一次。
 * @param t 测试上下文。
 */
func TestReasoningDecisionBuiltInBilling(t *testing.T) {
	t.Parallel()
	const score = `"effort":{"type":"score","probabilities":{"0":0.1,"1":0.7,"2":0.1,"3":0.1}}`
	for _, tc := range []struct {
		model, response                    string
		input, read, shortWrite, longWrite int
		cost                               float64
	}{
		{model: "gemini-decision-1", response: `{"answers":{` + score + `},"usage":{"total_input_tokens":1000,"total_output_tokens":0}}`, input: 1000, cost: 0.0001},
		{model: "decision-model-preview", response: `{"answers":{` + score + `},"usage":{"input_tokens":1000}}`, input: 1000, cost: 0},
		{model: "gpt-6-luna", response: `{"answers":[{"type":"score","name":"effort","probabilities":[{"value":0,"label":"low","probability":0.1},{"value":1,"label":"medium","probability":0.7},{"value":2,"label":"high","probability":0.1},{"value":3,"label":"xhigh","probability":0.1}]}],"usage":{"input_tokens":1000,"output_tokens":7,"input_tokens_details":{"cached_tokens":300,"cache_write_tokens":500,"cache_write_tokens_details":{"ttl_30m_tokens":200,"ttl_12h_tokens":300}}}}`, input: 200, read: 300, shortWrite: 200, longWrite: 300, cost: 0.000123},
	} {
		t.Run(tc.model, func(t *testing.T) {
			t.Parallel()
			env, _ := reasoningExecutorEnv(t, testChannel{name: "judge", models: tc.model, apiKey: "key", authType: model.AuthTypeSystemOne}, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.response)
			})
			result, err := env.server.executeReasoningDecision(context.Background(), "settled task", reasoningDefaultLevels)
			if err != nil || result.costUnknown {
				t.Fatalf("decision=%+v error=%v", result, err)
			}
			entry := waitForDecisionLog(t, env)
			if entry.InputTokens != tc.input || entry.CacheReadInputTokens != tc.read || entry.Cache30mInputTokens != tc.shortWrite || entry.Cache12hInputTokens != tc.longWrite || math.Abs(entry.Cost-tc.cost) > 1e-12 || entry.ReasoningDecision.CostUnknown {
				t.Fatalf("decision log=%+v, want cost %v", entry, tc.cost)
			}
			cfg, err := env.store.GetConfig(context.Background(), 2)
			if err != nil {
				t.Fatal(err)
			}
			tested := env.server.testSystemOneChannel(context.Background(), cfg, "key", tc.model, "settled task")
			if tested["success"] != true || tested["cost_unknown"] != false || math.Abs(tested["cost_usd"].(float64)-tc.cost) > 1e-12 {
				t.Fatalf("test=%+v", tested)
			}
			manual := detectionLogFromResult(cfg, model.LogSourceManualTest, tc.model, tc.model, channelTestKeySelection{}, "", "", tested)
			if manual.InputTokens != tc.input || manual.CacheReadInputTokens != tc.read || manual.Cache30mInputTokens != tc.shortWrite || manual.Cache12hInputTokens != tc.longWrite || math.Abs(manual.Cost-tc.cost) > 1e-12 {
				t.Fatalf("manual log=%+v", manual)
			}
			if tc.cost > 0 {
				for _, logged := range []*model.LogEntry{entry, manual} {
					breakdown := buildLogCostBreakdown(logged, nil)
					if breakdown == nil || math.Abs(breakdown.Total-tc.cost) > 1e-12 || breakdown.Output.PricePerMillion != 0 {
						t.Fatalf("breakdown=%+v", breakdown)
					}
				}
			}
		})
	}
}
