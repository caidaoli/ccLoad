package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"

	"github.com/tidwall/gjson"
)

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
	env.server.reasoningAuto = model.ReasoningAutoConfig{Enabled: true, DecisionChannelID: 2, DecisionModel: strings.Split(judge.models, ",")[0], BusinessModels: []string{"gpt-5.6"}, TimeoutMS: 1000}
	return env, calls
}

func TestReasoningDecisionSystemOneFixtures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, modelName, response string
		status                    int
		valid                     bool
	}{
		{"typesafe", "jev-latest", `{"model":"jev-1.13.0","answers":{"effort":{"type":"choice","choice":"high","confidence":1,"probabilities":{"low":0,"medium":0,"high":1,"xhigh":0}}},"usage":{"input_tokens":12,"output_tokens":6}}`, 200, true},
		{"nace", "drex-latest", `{"model":"drex-v1.5","answers":{"effort":{"type":"choice","choice":"high","confidence":0.97,"probabilities":{"low":0,"medium":0.01,"high":0.98,"xhigh":0.01}}},"usage":{"input_tokens":12,"output_tokens":6}}`, 200, true},
		{"missing answer", "jev-latest", `{"answers":{}}`, 200, false},
		{"unknown choice", "drex-latest", `{"answers":{"effort":{"type":"choice","choice":"max"}}}`, 200, false},
		{"invalid confidence", "jev-latest", `{"answers":{"effort":{"type":"choice","choice":"high","confidence":2}}}`, 200, false},
		{"wrong confidence type", "drex-latest", `{"answers":{"effort":{"type":"choice","choice":"high","confidence":"0.9"}}}`, 200, false},
		{"authentication", "jev-latest", `{"error":{"type":"authentication_error"}}`, 401, false},
		{"billing", "drex-latest", `{"error":{"type":"billing_error"}}`, 402, false},
		{"rate limit", "drex-latest", `{"error":{"type":"rate_limit_error"}}`, 429, false},
		{"overloaded", "drex-latest", `{"error":{"type":"overloaded_error"}}`, 529, false},
		{"non JSON", "jev-latest", `upstream unavailable`, 200, false},
		{"oversize", "drex-latest", strings.Repeat("x", reasoningResponseMaxBytes+1), 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, calls := reasoningExecutorEnv(t, testChannel{name: "judge", models: tc.modelName, apiKey: "judge-key", authType: model.AuthTypeSystemOne}, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer judge-key" {
					t.Errorf("wire=%s auth=%s", r.URL.Path, r.Header.Get("Authorization"))
				}
				body, _ := io.ReadAll(r.Body)
				if gjson.GetBytes(body, "questions.effort.type").String() != "choice" || len(gjson.GetBytes(body, "questions.effort.criteria").Map()) != 4 {
					t.Errorf("invalid question=%s", body)
				}
				if !strings.Contains(gjson.GetBytes(body, "state").String(), "task data") {
					t.Error("context missing")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.response)
			})
			result, err := env.server.executeReasoningDecision(context.Background(), "task data", reasoningDefaultLevels)
			if (err == nil) != tc.valid {
				t.Fatalf("decision=%+v, error=%v", result, err)
			}
			if tc.valid && (result.effort != "high" || result.costUnknown || result.usage.InputTokens != 12 || result.usage.OutputTokens != 6) {
				t.Fatalf("decision=%+v", result)
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
			env.server.reasoningAuto.TimeoutMS = 200
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
	env.server.reasoningAuto.TimeoutMS = 100
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

func TestReasoningDecisionErrorRespectsTimeoutBudget(t *testing.T) {
	t.Parallel()
	judge := newReasoningAutoJudge(t)
	judge.respond = func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"limited"}}`)
	}
	business := newReasoningAutoBusiness(t)
	env := setupReasoningAutoTestEnv(t, judge.server.URL, []testChannel{{
		name: "business", upstreamProtocol: "openai", models: "gpt-5.6-sol",
	}}, []string{business.server.URL})
	env.server.reasoningAuto.TimeoutMS = 100
	env.server.cooldownManager = cooldown.NewManager(slowReasoningCooldownStore{env.store}, env.server)
	start := time.Now()
	reasoningAutoRequest(t, env, "/v1/chat/completions", reasoningAutoChatBody("simple task"), nil)
	if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
		t.Fatalf("timeout_ms=100 but decision error blocked business request for %v", elapsed)
	}
	if len(business.snapshot()) != 1 {
		t.Fatal("business request did not proceed after the decision timeout")
	}
	decisionLog := waitForDecisionLog(t, env)
	proxyLog := waitForProxyLog(t, env, "gpt-5.6-sol")
	for _, entry := range []*model.LogEntry{decisionLog, proxyLog} {
		if entry.ReasoningDecision == nil || entry.ReasoningDecision.Reason != "timeout" || entry.ReasoningDecision.DurationMS < 90 || entry.ReasoningDecision.DurationMS > 400 {
			t.Fatalf("timeout fallback metadata = %+v", entry.ReasoningDecision)
		}
	}
}
