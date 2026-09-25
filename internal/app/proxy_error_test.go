package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"
)

// Test_HandleProxyError_Basic 基础错误处理测试(不依赖数据库)
func Test_HandleProxyError_Basic(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		statusCode     int
		expectedAction cooldown.Action
	}{
		{
			name:           "context canceled",
			err:            context.Canceled,
			expectedAction: cooldown.ActionReturnClient,
		},
		{
			name:           "connection refused",
			err:            errors.New("connection refused"),
			expectedAction: cooldown.ActionRetryChannel,
		},
		{
			name:           "401 unauthorized - Key级",
			statusCode:     401,
			expectedAction: cooldown.ActionRetryKey,
		},
		{
			name:           "500 server error",
			statusCode:     500,
			expectedAction: cooldown.ActionRetryModel,
		},
		{
			name:           "404 not found - 渠道级",
			statusCode:     404,
			expectedAction: cooldown.ActionRetryChannel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newInMemoryServer(t)

			ctx := context.Background()
			cfg := &model.Config{
				ID:       1,
				Name:     "test",
				URLs:     model.ChannelURLs{{URL: "http://test.example.com"}},
				Priority: 1,
				Enabled:  true,
			}

			var res *fwResult
			var err error

			if tt.statusCode > 0 {
				res = &fwResult{
					Status: tt.statusCode,
					Body:   []byte(`{"error": "test"}`),
					Header: make(http.Header),
				}
			} else {
				err = tt.err
			}

			var action cooldown.Action
			if err != nil {
				reqCtx := &proxyRequestContext{
					originalModel: "test-model",
				}
				_, action = srv.handleNetworkError(ctx, cfg, 0, "test-model", "test-key", 0, "", 0.1, err, nil, reqCtx, false)
			} else {
				action = srv.applyCooldownDecision(ctx, cfg, cooldownInputForModel(httpErrorInput(cfg.ID, 0, res), "test-model"))
			}

			if action != tt.expectedAction {
				t.Errorf("期望 action=%v, 实际=%v", tt.expectedAction, action)
			}
		})
	}
}

type failingTokenStatsStore struct {
	storage.Store
	err error
}

func (s *failingTokenStatsStore) UpdateTokenStats(
	context.Context,
	string,
	model.TokenStatsOutcome,
	float64,
	bool,
	float64,
	int64,
	int64,
	int64,
	int64,
	float64,
	float64,
	time.Time,
) error {
	return s.err
}

func TestApplyTokenStatsUpdateAddsCostToCacheWhenStoreFails(t *testing.T) {
	const tokenHash = "limited-token"

	auth := newTestAuthService(t)
	auth.authTokenCostLimits[tokenHash] = tokenCostLimit{
		usedMicroUSD:  0,
		limitMicroUSD: 1000,
	}

	srv := &Server{
		store:       &failingTokenStatsStore{err: errors.New("database down")},
		authService: auth,
	}

	srv.applyTokenStatsUpdate(tokenStatsUpdate{
		tokenHash:      tokenHash,
		completedAt:    time.Now(),
		outcome:        model.TokenStatsSuccess,
		costUSD:        0.0002,
		costMultiplier: 1,
	})

	used, limit, exceeded := auth.IsCostLimitExceeded(tokenHash)
	if want := util.USDToMicroUSD(0.0002); used != want {
		t.Fatalf("used=%d, want %d; DB failure must not block in-memory cost accounting", used, want)
	}
	if limit != 1000 || exceeded {
		t.Fatalf("limit/exceeded=(%d,%v), want (1000,false)", limit, exceeded)
	}
}

// Test_HandleNetworkError_Basic 基础网络错误处理测试
func Test_HandleNetworkError_Basic(t *testing.T) {
	srv := newInMemoryServer(t)

	ctx := context.Background()
	cfg := &model.Config{
		ID:       1,
		Name:     "test",
		URLs:     model.ChannelURLs{{URL: "http://test.example.com"}},
		Priority: 1,
		Enabled:  true,
		ModelEntries: []model.ModelEntry{
			{Model: "test-model"},
			{Model: "other-model"},
		},
	}

	// 创建测试用的请求上下文
	reqCtx := &proxyRequestContext{
		originalModel: "test-model",
		tokenID:       0,
		clientIP:      "",
	}

	t.Run("context canceled returns client error", func(t *testing.T) {
		result, action := srv.handleNetworkError(
			ctx, cfg, 0, "test-model", "test-key", 0, "", 0.1, context.Canceled, nil, reqCtx, false,
		)

		if result == nil {
			t.Error("期望返回错误结果")
		}
		if action != cooldown.ActionReturnClient {
			t.Errorf("期望 action=ActionReturnClient, 实际=%v", action)
		}
	})

	t.Run("network error switches channel", func(t *testing.T) {
		result, action := srv.handleNetworkError(
			ctx, cfg, 0, "test-model", "test-key", 0, "", 0.1, errors.New("connection refused"), nil, reqCtx, false,
		)

		if result == nil {
			t.Error("期望返回错误结果")
		}
		if result != nil && result.status != http.StatusBadGateway {
			t.Errorf("期望 status=502, 实际=%d", result.status)
		}
		if action != cooldown.ActionRetryChannel {
			t.Errorf("期望 action=ActionRetryChannel, 实际=%v", action)
		}
	})

	modelScopedErrors := []struct {
		name string
		err  error
	}{
		{name: "first byte timeout", err: fmt.Errorf("wrap: %w", util.ErrUpstreamFirstByteTimeout)},
		{name: "connection reset", err: errors.New("read: connection reset by peer")},
		{name: "http2 body closed", err: errors.New("http2: response body closed")},
		{name: "http2 stream error", err: errors.New("stream error: stream ID 7; INTERNAL_ERROR")},
		{name: "empty response", err: fmt.Errorf("probe failed: %w", util.ErrUpstreamEmptyResponse)},
		{name: "deadline exceeded", err: context.DeadlineExceeded},
		{name: "connection timeout", err: errors.New("upstream connection timeout")},
	}
	for _, tt := range modelScopedErrors {
		t.Run(tt.name+" cools model", func(t *testing.T) {
			result, action := srv.handleNetworkError(
				ctx, cfg, 0, "test-model", "test-key", 0, "", 0.1, tt.err, nil, reqCtx, false,
			)

			if result == nil {
				t.Fatal("期望返回错误结果")
			}
			if action != cooldown.ActionRetryModel {
				t.Fatalf("action=%v, want ActionRetryModel", action)
			}
		})
	}
}

// Test_HandleProxySuccess_Basic 基础成功处理测试
func Test_HandleProxySuccess_Basic(t *testing.T) {
	srv := newInMemoryServer(t)

	ctx := context.Background()
	cfg := &model.Config{
		ID:       1,
		Name:     "test",
		URLs:     model.ChannelURLs{{URL: "http://test.example.com"}},
		Priority: 1,
		Enabled:  true,
	}

	res := &fwResult{
		Status:        200,
		Body:          []byte(`{"content": "success"}`),
		Header:        make(http.Header),
		FirstByteTime: 0.05,
	}

	// 创建测试用的请求上下文（新增参数，2025-11）
	reqCtx := &proxyRequestContext{
		tokenHash: "", // 测试环境无需Token统计
	}

	result, action := srv.handleProxySuccess(
		ctx, cfg, 0, "test-model", "test-key", res, 0.1, reqCtx,
	)

	if result == nil {
		t.Fatal("期望返回成功结果")
	}
	if result.status != 200 {
		t.Errorf("期望 status=200, 实际=%d", result.status)
	}
	if !result.succeeded {
		t.Error("期望 succeeded=true")
	}
	if action != cooldown.ActionReturnClient {
		t.Errorf("期望 action=ActionReturnClient, 实际=%v", action)
	}
}

// Test_HandleProxyError_499 测试499状态码处理
func Test_HandleProxyError_499(t *testing.T) {
	srv := newInMemoryServer(t)

	ctx := context.Background()
	cfg := &model.Config{
		ID:       1,
		Name:     "test",
		URLs:     model.ChannelURLs{{URL: "http://test.example.com"}},
		Priority: 1,
		Enabled:  true,
		ModelEntries: []model.ModelEntry{
			{Model: "test-model"},
			{Model: "other-model"},
		},
	}

	t.Run("upstream 499 triggers model retry", func(t *testing.T) {
		res := &fwResult{
			Status: 499,
			Body:   []byte(`{"error": "client closed request"}`),
			Header: make(http.Header),
		}
		input := cooldownInputForModel(httpErrorInput(cfg.ID, 0, res), "test-model")
		action := srv.applyCooldownDecision(ctx, cfg, input)

		if action != cooldown.ActionRetryModel {
			t.Errorf("期望 action=ActionRetryModel, 实际=%v", action)
		}
	})

	t.Run("client canceled returns to client", func(t *testing.T) {
		reqCtx := &proxyRequestContext{
			originalModel: "test-model",
		}
		_, action := srv.handleNetworkError(ctx, cfg, 0, "test-model", "test-key", 0, "", 0.1, context.Canceled, nil, reqCtx, false)

		if action != cooldown.ActionReturnClient {
			t.Errorf("期望 action=ActionReturnClient, 实际=%v", action)
		}
	})
}

// Test_HandleNetworkError_499_PreservesTokenStats 测试 499 场景下 token 统计被保留
// [FIX] 2025-12: 修复流式响应中途取消时 token 统计丢失的问题
func Test_HandleNetworkError_499_PreservesTokenStats(t *testing.T) {
	srv := newInMemoryServer(t)

	ctx := context.Background()
	cfg := &model.Config{
		ID:       1,
		Name:     "test",
		URLs:     model.ChannelURLs{{URL: "http://test.example.com"}},
		Priority: 1,
		Enabled:  true,
	}

	// 模拟流式响应中途取消的场景：已解析到 token 统计
	res := &fwResult{
		Status:                   200,
		InputTokens:              100,
		OutputTokens:             50,
		CacheReadInputTokens:     200,
		CacheCreationInputTokens: 30,
		FirstByteTime:            0.1,
	}

	// 创建带有 tokenHash 的请求上下文
	tokenHash := "test-token-hash-499"
	reqCtx := &proxyRequestContext{
		tokenHash:   tokenHash,
		isStreaming: true,
	}

	// 调用 handleNetworkError，传入 res 和 reqCtx
	result, action := srv.handleNetworkError(
		ctx, cfg, 0, "claude-sonnet-4-5", "test-key", 0, "", 0.5, context.Canceled, res, reqCtx, false,
	)

	// 验证返回值正确
	if result == nil {
		t.Error("期望返回错误结果")
	}
	if result != nil && !result.isClientCanceled {
		t.Error("期望 isClientCanceled=true")
	}
	if action != cooldown.ActionReturnClient {
		t.Errorf("期望 action=ActionReturnClient, 实际=%v", action)
	}

	// 验证 hasConsumedTokens 函数
	if !hasConsumedTokens(res) {
		t.Error("hasConsumedTokens 应返回 true")
	}
	if hasConsumedTokens(nil) {
		t.Error("hasConsumedTokens(nil) 应返回 false")
	}
	if hasConsumedTokens(&fwResult{}) {
		t.Error("hasConsumedTokens(空结果) 应返回 false")
	}
}

func TestCooldownWriteContext_DetachesCancelButPreservesValues(t *testing.T) {
	type ctxKey string

	const key ctxKey = "k"
	baseCtx := context.WithValue(context.Background(), key, "v")
	canceledCtx, cancel := context.WithCancel(baseCtx)
	cancel()

	ctx, cancel := cooldownWriteContext(canceledCtx)
	defer cancel()

	select {
	case <-ctx.Done():
		t.Fatalf("cooldownWriteContext 不应立即继承取消信号: err=%v", ctx.Err())
	default:
	}

	if got := ctx.Value(key); got != "v" {
		t.Fatalf("cooldownWriteContext 应保留 ctx.Value: got=%v", got)
	}
}

// Exercises the proxy boundary, persisted audit and prepared deferred decision together.
func TestProxyJevAnalysis(t *testing.T) {
	offsetReset := time.Now().In(time.FixedZone("UTC+8", 8*3600)).Add(24 * time.Hour).Format("2006-01-02 15:04:05")
	cases := []struct {
		name                            string
		status                          int
		body, category, reset, outcome  string
		confidence                      float64
		resetAfter                      time.Duration
		want                            cooldown.Action
		calls                           int
		configured, disabled, committed bool
	}{
		{name: "unknown", status: 404, body: `{"error":{"message":"unrecognized failure sk-upstream-secret"},"messages":["private conversation"]}`, category: "channel", confidence: 1, want: cooldown.ActionRetryChannel, calls: 1},
		{name: "5xx classification", status: 500, body: `{"error":{"message":"unknown"}}`, category: "channel", confidence: 1, want: cooldown.ActionRetryChannel, calls: 1},
		{name: "request", status: 597, body: `{"error":{"type":"unrecognized","message":"invalid shape"}}`, category: "request", confidence: 1, want: cooldown.ActionReturnClient, calls: 1},
		{name: "oauth credential", status: 401, body: `{"error":{"message":"unrecognized rejection"}}`, category: "credential", confidence: 1, want: cooldown.ActionRetryChannel, calls: 1},
		{name: "low confidence", status: 404, body: `{"error":{"message":"unrecognized"}}`, category: "request", confidence: 0.9, want: cooldown.ActionRetryChannel, calls: 1, outcome: "low_confidence"},
		{name: "invalid choice", status: 404, body: `{"error":{"message":"unrecognized"}}`, category: "delete_credential", confidence: 1, want: cooldown.ActionRetryChannel, calls: 1, outcome: "invalid_choice"},
		{name: "configured priority", status: 404, body: `{"error":{"message":"unrecognized"}}`, category: "request", confidence: 1, want: cooldown.ActionRetryModel, calls: 0, configured: true},
		{name: "disabled", status: 404, body: `{"error":{"message":"unrecognized"}}`, want: cooldown.ActionRetryChannel, disabled: true},
		{name: "known context limit", status: 400, body: `{"error":{"code":"context_length_exceeded","message":"maximum context length exceeded"}}`, want: cooldown.ActionReturnClient},
		{name: "time only", status: 429, body: `{"error":{"message":"retry in 2 hours; previous reset 2020-01-01T00:00:00Z"}}`, reset: "2 hours", confidence: 1, resetAfter: 2 * time.Hour, want: cooldown.ActionRetryModel, calls: 1},
		{name: "compound duration", status: 429, body: `{"error":{"message":"Daily free limit reached. Try again in 2h 18m"}}`, reset: "2h 18m", confidence: 1, resetAfter: 2*time.Hour + 18*time.Minute, want: cooldown.ActionRetryModel, calls: 1},
		{name: "compact compound duration", status: 429, body: `{"error":{"message":"Daily free limit reached. Try again in 2h18m"}}`, reset: "2h18m", confidence: 1, resetAfter: 2*time.Hour + 18*time.Minute, want: cooldown.ActionRetryModel, calls: 1},
		{name: "ambiguous timezone", status: 429, body: `{"error":{"message":"reset 2099-01-01 12:00:00"}}`, reset: "2099-01-01 12:00:00", confidence: 1, want: cooldown.ActionRetryModel, calls: 1, outcome: "no_valid_reset"},
		{name: "explicit UTC offset", status: 429, body: fmt.Sprintf(`{"error":{"message":"reset %s UTC+8"}}`, offsetReset), reset: offsetReset + " UTC+8", confidence: 1, want: cooldown.ActionRetryModel, calls: 1},
		{name: "past reset", status: 429, body: `{"error":{"message":"reset 2020-01-01T00:00:00Z"}}`, reset: "2020-01-01T00:00:00Z", confidence: 1, want: cooldown.ActionRetryModel, calls: 1, outcome: "no_valid_reset"},
		{name: "reset overflow", status: 429, body: `{"error":{"message":"retry in 999999999999999999999999 hours"}}`, reset: "999999999999999999999999 hours", confidence: 1, want: cooldown.ActionRetryModel, calls: 1, outcome: "no_valid_reset"},
		{name: "committed SSE", status: 597, body: `{"error":{"type":"new_failure","message":"model temporarily unavailable"}}`, category: "model", confidence: 1, want: cooldown.ActionReturnClient, calls: 1, committed: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			jevAdmission.Lock()
			jevAdmission.starts = nil
			jevAdmission.Unlock()
			srv := newInMemoryServerWithSettings(t, map[string]string{config.TypeSafeEnabledSettingKey: strconv.FormatBool(!tc.disabled), config.TypeSafeAPIKeySettingKey: "test-typesafe-secret", "debug_log_enabled": "false"})
			ctx := context.Background()
			cfg, err := srv.store.CreateConfig(ctx, &model.Config{Name: "jev-test", Enabled: true, URLs: model.ChannelURLs{{URL: "https://upstream.invalid"}}, ModelEntries: []model.ModelEntry{{Model: "test-model"}, {Model: "other-model"}}})
			if err != nil {
				t.Fatal(err)
			}
			if tc.configured {
				cfg.CooldownDetectionRules = &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{Enabled: true, Name: "test rule", Priority: 1, StatusCodes: []int{404}, Scope: "model", Mode: "fixed", CooldownSeconds: 20}}}
			}
			calls := 0
			srv.jevClient = &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.String() != jevEndpoint || request.Header.Get("Authorization") != "Bearer test-typesafe-secret" {
					t.Errorf("invalid endpoint/auth")
				}
				raw, err := io.ReadAll(request.Body)
				if err != nil {
					return nil, err
				}
				if strings.Contains(string(raw), "sk-upstream-secret") || strings.Contains(string(raw), "private conversation") {
					t.Errorf("sensitive request: %s", raw)
				}
				var sent jevRequest
				if err := json.Unmarshal(raw, &sent); err != nil {
					t.Fatal(err)
				}
				if sent.Model != "jev-latest" {
					t.Errorf("model=%s", sent.Model)
				}
				answers := map[string]jevAnswer{}
				for key, question := range sent.Questions {
					choice := tc.category
					if key == "reset" {
						if _, ok := sent.Questions["category"]; ok && tc.status == 429 {
							t.Error("known classification sent for replacement")
						}
						choice = "none"
						for _, candidate := range sent.State.Candidates {
							if candidate.Value == tc.reset {
								choice = candidate.ID
							}
						}
					}
					probabilities := map[string]float64{}
					for option := range question.Criteria {
						probabilities[option] = 0
					}
					probabilities[choice] = 1
					answers[key] = jevAnswer{Type: "choice", Choice: choice, Probabilities: probabilities, Confidence: tc.confidence}
				}
				reply := jevResponse{Model: "jev-1.13.0", Answers: answers}
				reply.Usage.InputTokens = 23
				data, err := json.Marshal(reply)
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data)), Header: make(http.Header)}, nil
			})}
			received := time.Now().Add(-time.Second)
			res := &fwResult{Status: tc.status, Body: []byte(tc.body), ResponseCommitted: tc.committed, errorReceivedAt: received}
			reqCtx := &proxyRequestContext{originalModel: "test-model", channelStartTime: received, attemptStartTime: received}
			result, action := srv.handleCommittedAwareProxyError(ctx, cfg, cooldown.NoKeyIndex, "test-model", "sk-upstream-secret", res, 0.1, reqCtx, nil, true)
			if action != tc.want {
				t.Fatalf("action=%v want=%v", action, tc.want)
			}
			if result.deferredCooldown != nil {
				_ = srv.decideCooldownAction(ctx, cfg, *result.deferredCooldown)
				_ = srv.cooldownManager.CanFallbackToOtherKey(*result.deferredCooldown)
				_ = srv.applyCooldownDecision(ctx, cfg, *result.deferredCooldown)
			}
			if calls != tc.calls {
				t.Fatalf("calls=%d want=%d", calls, tc.calls)
			}
			if calls == 0 {
				return
			}
			var logs []*model.LogEntry
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				logs, err = srv.store.ListLogs(ctx, received.Add(-time.Minute), 10, 0, &model.LogFilter{LogSource: model.LogSourceAll})
				if err != nil {
					t.Fatal(err)
				}
				if len(logs) >= 2 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			var audit jevAudit
			proxyMessage := ""
			auditCount := 0
			for _, entry := range logs {
				if entry.LogSource == model.LogSourceJev {
					auditCount++
					if err := json.Unmarshal([]byte(entry.Message), &audit); err != nil {
						t.Fatal(err)
					}
					if entry.InputTokens != 23 || entry.ResponseModel != "jev-1.13.0" {
						t.Fatalf("audit fields: %+v", entry)
					}
					wantCost := util.CalculateCostDetailed("jev-latest", 23, 0, 0, 0, 0)
					if math.Abs(entry.Cost-wantCost) > 1e-12 {
						t.Fatalf("audit cost = %.12f, want %.12f", entry.Cost, wantCost)
					}
					debug, debugErr := srv.store.GetDebugLogByLogID(ctx, entry.ID)
					if debugErr != nil || debug == nil || debug.ReqURL != jevEndpoint || !strings.Contains(string(debug.RespBody), audit.CallID) {
						t.Fatalf("audit debug data missing: debug=%+v err=%v", debug, debugErr)
					}
				} else {
					proxyMessage = entry.Message
				}
			}
			if auditCount != 1 || audit.CallID == "" || !strings.Contains(proxyMessage, audit.CallID) {
				t.Fatalf("audit=%+v proxy=%q count=%d", audit, proxyMessage, auditCount)
			}
			for key, value := range audit.Adopted {
				if !strings.Contains(proxyMessage, key+"="+value) {
					t.Fatalf("proxy log omitted Jev adopted result %s=%s: %q", key, value, proxyMessage)
				}
			}
			for key, value := range audit.Fallback {
				if !strings.Contains(proxyMessage, "fallback."+key+"="+value) {
					t.Fatalf("proxy log omitted Jev fallback result %s=%s: %q", key, value, proxyMessage)
				}
			}
			if tc.outcome != "" {
				data, _ := json.Marshal(audit.Fallback)
				if !strings.Contains(string(data), tc.outcome) {
					t.Fatalf("fallback=%s", data)
				}
			}
			if tc.name == "explicit UTC offset" && audit.Adopted["reset"] == "" {
				t.Fatalf("explicit timezone reset not adopted: %+v", audit)
			}
			if duration := tc.resetAfter; duration > 0 {
				cooldowns, err := srv.store.GetAllModelCooldowns(ctx)
				if err != nil || cooldowns[cfg.ID]["test-model"].Sub(received.Add(duration)).Abs() > time.Second {
					t.Fatalf("precise reset was not persisted: %v err=%v", cooldowns, err)
				}
				until, err := time.Parse(time.RFC3339Nano, audit.Adopted["reset"])
				if err != nil || !until.Equal(received.Add(duration)) {
					t.Fatalf("reset=%v err=%v", until, err)
				}
			}
		})
	}
}

func TestProxyJevBudgetAndCancellation(t *testing.T) {
	jevAdmission.Lock()
	jevAdmission.starts = nil
	jevAdmission.Unlock()
	srv := newInMemoryServerWithSettings(t, map[string]string{config.TypeSafeEnabledSettingKey: "true", config.TypeSafeAPIKeySettingKey: "test-key"})
	calls := 0
	srv.jevClient = &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	cfg := &model.Config{ID: 1, ModelEntries: []model.ModelEntry{{Model: "m"}}}
	reqCtx := &proxyRequestContext{jevWait: jevWaitBudget - 40*time.Millisecond}
	res := &fwResult{Status: 500, Body: []byte(`{"error":{"message":"unknown"}}`)}
	input := cooldownInputForModel(httpErrorInput(1, 0, res), "m")
	start := time.Now()
	prepared := srv.prepareJevError(context.Background(), cfg, reqCtx, res, input, "")
	if time.Since(start) > time.Second || calls != 1 {
		t.Fatalf("elapsed=%v calls=%d", time.Since(start), calls)
	}
	_ = srv.decideCooldownAction(context.Background(), cfg, prepared)
	second := &fwResult{Status: 500, Body: res.Body}
	_ = srv.prepareJevError(context.Background(), cfg, reqCtx, second, input, "")
	if calls != 1 || second.jevNote != "skipped:budget_exhausted" {
		t.Fatalf("calls=%d note=%s", calls, second.jevNote)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = srv.prepareJevError(ctx, cfg, &proxyRequestContext{}, &fwResult{Status: 500}, input, "")
	if calls != 1 {
		t.Fatal("canceled request called TypeSafe")
	}
	// Database persistence retains its own full budget even after remote timeout/cancellation.
	writeCtx, writeCancel := cooldownWriteContext(ctx)
	defer writeCancel()
	deadline, ok := writeCtx.Deadline()
	if !ok || time.Until(deadline) < 2*time.Second || writeCtx.Err() != nil {
		t.Fatal("database budget inherited analysis cancellation")
	}
}

func TestProxyJevAdmissionAndFailureAudits(t *testing.T) {
	jevAdmission.Lock()
	jevAdmission.starts = nil
	jevAdmission.Unlock()
	srv := newInMemoryServerWithSettings(t, map[string]string{config.TypeSafeEnabledSettingKey: "true", config.TypeSafeAPIKeySettingKey: "typesafe-key"})
	entered := make(chan struct{}, 10)
	release := make(chan struct{})
	srv.jevClient = &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		<-release
		return &http.Response{StatusCode: 502, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"Bearer typesafe-key"}`))}, nil
	})}
	cfg := &model.Config{ID: 1}
	run := func() *fwResult {
		res := &fwResult{Status: 500, Body: []byte(`{"error":{"message":"unknown"}}`)}
		_ = srv.prepareJevError(context.Background(), cfg, &proxyRequestContext{}, res, httpErrorInput(1, 0, res), "")
		return res
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); run() }()
	}
	for range 8 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("calls did not start")
		}
	}
	if res := run(); res.jevNote != "skipped:capacity" {
		t.Fatalf("concurrent admission=%q", res.jevNote)
	}
	close(release)
	wg.Wait()
	run()
	run()
	if res := run(); res.jevNote != "skipped:capacity" {
		t.Fatalf("rate admission=%q", res.jevNote)
	}
	deadline := time.Now().Add(2 * time.Second)
	var logs []*model.LogEntry
	for time.Now().Before(deadline) {
		var err error
		logs, err = srv.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 20, 0, &model.LogFilter{LogSource: model.LogSourceJev})
		if err != nil {
			t.Fatal(err)
		}
		if len(logs) == 10 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(logs) != 10 {
		t.Fatalf("actual call audits=%d want=10", len(logs))
	}
	for _, entry := range logs {
		var audit jevAudit
		if err := json.Unmarshal([]byte(entry.Message), &audit); err != nil {
			t.Fatal(err)
		}
		if audit.Fallback["call"] != "http_error" || strings.Contains(entry.Message, "typesafe-key") {
			t.Fatalf("invalid error audit=%s", entry.Message)
		}
	}
}

func TestProxyJevInvalidResponseAudits(t *testing.T) {
	for _, tc := range []struct {
		name, body, reason string
		truncated          bool
	}{
		{"malformed", `{"answers":`, "invalid_response", false},
		{"oversized", strings.Repeat("\x00", jevMaxResponseBytes+100), "incomplete_response", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jevAdmission.Lock()
			jevAdmission.starts = nil
			jevAdmission.Unlock()
			srv := newInMemoryServerWithSettings(t, map[string]string{config.TypeSafeEnabledSettingKey: "true", config.TypeSafeAPIKeySettingKey: "test-typesafe-secret"})
			srv.jevClient = &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			res := &fwResult{Status: 404, Body: []byte(`{"error":{"message":"unknown"}}`)}
			cfg := &model.Config{ID: 1}
			input := srv.prepareJevError(context.Background(), cfg, &proxyRequestContext{}, res, httpErrorInput(1, 0, res), "")
			if action := srv.decideCooldownAction(context.Background(), cfg, input); action != cooldown.ActionRetryChannel {
				t.Fatalf("invalid output changed action: %v", action)
			}
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				entries, err := srv.store.ListLogs(context.Background(), time.Now().Add(-time.Minute), 10, 0, &model.LogFilter{LogSource: model.LogSourceJev})
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) == 0 {
					time.Sleep(10 * time.Millisecond)
					continue
				}
				var audit jevAudit
				if err := json.Unmarshal([]byte(entries[0].Message), &audit); err != nil {
					t.Fatal(err)
				}
				if len(entries[0].Message) > jevMaxAuditBytes || audit.Fallback["call"] != tc.reason || audit.Truncated != tc.truncated {
					t.Fatalf("audit size=%d fallback=%v truncated=%v", len(entries[0].Message), audit.Fallback, audit.Truncated)
				}
				return
			}
			t.Fatal("actual invalid-response call missing audit")
		})
	}
}
