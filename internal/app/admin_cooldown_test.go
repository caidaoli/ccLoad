package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"ccLoad/internal/model"
)

/**
 * @brief 草稿测试按成功响应回答文本匹配，保持显式状态及正文条件与代理一致
 * @param t 当前测试上下文
 */
func TestHandleCooldownDetectionTestSuccessAnswer(t *testing.T) {
	srv := newInMemoryServer(t)
	for _, tc := range []struct {
		name     string ///< 样本场景
		body     string ///< 上游原始响应样本
		statuses []int  ///< 显式配置的 HTTP 状态码
		pattern  string ///< 正文匹配表达式
		want     bool   ///< 是否应命中并生成可执行冷却决策
	}{
		{"sse", "data: {\"choices\":[{\"delta\":{\"content\":\"余额不够\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"这次调用。\"}}]}\n\ndata: [DONE]\n\n", []int{200}, "^余额不够这次调用", true},
		{"json", `{"choices":[{"message":{"content":"余额不够这次调用。"}}]}`, []int{200}, "^余额不够这次调用", true},
		{"plain", "余额不够这次调用。", []int{200}, "^余额不够这次调用", true},
		{"legacy_without_status", "余额不够这次调用。", nil, "^余额不够这次调用", false},
		{"status_only", "余额不够这次调用。", []int{200}, "", false},
		{"normal_answer", `{"choices":[{"message":{"content":"这是正常回答。"}}]}`, []int{200}, "^余额不够这次调用", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{
				"status_code": 200, "error_body": tc.body,
				"cooldown_detection_rules": &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
					Enabled: true, Name: "Success answer", StatusCodes: tc.statuses, MessagePattern: tc.pattern,
					Scope: model.CooldownScopeKey, Mode: model.CooldownModeFixed, CooldownSeconds: 60,
				}}},
			}
			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/cooldown-detection/test", payload))
			srv.HandleCooldownDetectionTest(c)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			var result cooldownDetectionTestResponse
			mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &result)
			if result.Matched != tc.want || result.Actionable != tc.want {
				t.Fatalf("evaluation=%+v, want matched/actionable=%t", result, tc.want)
			}
			if tc.want && (result.Message != "余额不够这次调用。" || result.Scope != model.CooldownScopeKey) {
				t.Fatalf("unexpected extracted message or scope: %+v", result)
			}
		})
	}
}

func TestHandleCooldownDetectionTestEvaluatesDraftWithoutPersistingCooldown(t *testing.T) {
	srv := newInMemoryServer(t)
	ctx := context.Background()
	created, err := srv.store.CreateConfig(ctx, &model.Config{
		ID:           1,
		Name:         "cooldown-detection-test",
		URLs:         model.ChannelURLs{{URL: "https://api.example.com"}},
		Priority:     1,
		ModelEntries: []model.ModelEntry{{Model: "test-model"}},
		Enabled:      true,
	})
	if err != nil {
		t.Fatalf("CreateConfig() error = %v", err)
	}
	originalUntil := time.Now().Add(5 * time.Minute).Truncate(time.Second)
	if err := srv.store.SetChannelCooldown(ctx, created.ID, originalUntil); err != nil {
		t.Fatalf("SetChannelCooldown() error = %v", err)
	}

	payload := map[string]any{
		"status_code": 200,
		"error_body":  `2026-07-24T15:30:00+08:00 [WARN] upstream status 406: {"error":{"code":"maintenance","message":"planned maintenance"}}`,
		"cooldown_detection_rules": map[string]any{
			"rules": []map[string]any{{
				"enabled":          true,
				"name":             "Maintenance",
				"priority":         9,
				"status_codes":     []int{406},
				"message_pattern":  "planned maintenance",
				"scope":            model.CooldownScopeChannel,
				"mode":             model.CooldownModeFixed,
				"cooldown_seconds": 120,
			}},
		},
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/cooldown-detection/test", payload))
	srv.HandleCooldownDetectionTest(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var data struct {
		Code              string     `json:"code"`
		StatusCode        int        `json:"status_code"`
		ParsedLog         bool       `json:"parsed_log"`
		Matched           bool       `json:"matched"`
		Actionable        bool       `json:"actionable"`
		Priority          *int       `json:"priority"`
		CooldownUntil     *time.Time `json:"cooldown_until"`
		FallbackToBuiltin bool       `json:"fallback_to_builtin"`
	}
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &data)
	if data.Code != "MAINTENANCE" || !data.Matched || !data.Actionable || data.Priority == nil || *data.Priority != 0 {
		t.Fatalf("unexpected rule test result: %+v", data)
	}
	if data.StatusCode != http.StatusNotAcceptable || !data.ParsedLog {
		t.Fatalf("normalized test input = (status=%d, parsed_log=%t), want (406, true)", data.StatusCode, data.ParsedLog)
	}
	if data.CooldownUntil == nil || !data.CooldownUntil.After(time.Now()) || data.FallbackToBuiltin {
		t.Fatalf("unexpected cooldown result: %+v", data)
	}

	updated, err := srv.store.GetConfig(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}
	if updated.CooldownUntil != originalUntil.Unix() {
		t.Fatalf("test endpoint changed channel cooldown: got %d, want %d", updated.CooldownUntil, originalUntil.Unix())
	}
}

func TestHandleCooldownDetectionTestKeepsExplicitStatusForRawBody(t *testing.T) {
	srv := newInMemoryServer(t)
	payload := map[string]any{
		"status_code": http.StatusOK,
		"error_body":  `{"error":{"message":"soft quota error: upstream status 999: is provider text"}}`,
		"cooldown_detection_rules": map[string]any{
			"rules": []map[string]any{{
				"enabled":          true,
				"name":             "HTTP 200 soft error",
				"priority":         0,
				"status_codes":     []int{http.StatusOK},
				"message_pattern":  "soft quota error",
				"scope":            model.CooldownScopeChannel,
				"mode":             model.CooldownModeFixed,
				"cooldown_seconds": 60,
			}},
		},
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/cooldown-detection/test", payload))
	srv.HandleCooldownDetectionTest(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var data struct {
		StatusCode int  `json:"status_code"`
		ParsedLog  bool `json:"parsed_log"`
		Actionable bool `json:"actionable"`
	}
	mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &data)
	if data.StatusCode != http.StatusOK || data.ParsedLog || !data.Actionable {
		t.Fatalf("unexpected raw body result: %+v", data)
	}
}

func TestHandleCooldownDetectionTestUsesEffectiveChannelRules(t *testing.T) {
	globalRules := &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
		Enabled:         true,
		Name:            "Global maintenance",
		Priority:        0,
		StatusCodes:     []int{http.StatusNotAcceptable},
		MessagePattern:  "planned maintenance",
		Scope:           model.CooldownScopeChannel,
		Mode:            model.CooldownModeFixed,
		CooldownSeconds: 120,
	}}}

	tests := []struct {
		name                string
		rulesSource         string
		channelRules        any
		wantSource          string
		wantActionable      bool
		wantBuiltinFallback bool
	}{
		{
			name:                "empty channel rules inherit global rules",
			rulesSource:         cooldownDetectionRulesSourceChannel,
			channelRules:        nil,
			wantSource:          cooldownDetectionRulesSourceGlobal,
			wantActionable:      true,
			wantBuiltinFallback: false,
		},
		{
			name:        "channel rules replace global rules",
			rulesSource: cooldownDetectionRulesSourceChannel,
			channelRules: map[string]any{"rules": []map[string]any{{
				"enabled": true, "name": "Local teapot", "priority": 0,
				"status_codes": []int{http.StatusTeapot}, "scope": model.CooldownScopeChannel,
				"mode": model.CooldownModeFixed, "cooldown_seconds": 60,
			}}},
			wantSource:          cooldownDetectionRulesSourceChannel,
			wantActionable:      false,
			wantBuiltinFallback: true,
		},
		{
			name:                "global draft does not inherit saved global rules",
			rulesSource:         cooldownDetectionRulesSourceGlobal,
			channelRules:        nil,
			wantSource:          cooldownDetectionRulesSourceGlobal,
			wantActionable:      false,
			wantBuiltinFallback: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newInMemoryServer(t)
			srv.globalCooldownDetectionRules = globalRules
			payload := map[string]any{
				"rules_source":             tt.rulesSource,
				"status_code":              http.StatusNotAcceptable,
				"error_body":               `{"error":{"message":"planned maintenance"}}`,
				"cooldown_detection_rules": tt.channelRules,
			}
			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/cooldown-detection/test", payload))
			srv.HandleCooldownDetectionTest(c)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusOK, w.Body.String())
			}

			var data struct {
				RulesSource       string `json:"rules_source"`
				Actionable        bool   `json:"actionable"`
				FallbackToBuiltin bool   `json:"fallback_to_builtin"`
			}
			mustUnmarshalAPIResponseData(t, w.Body.Bytes(), &data)
			if data.RulesSource != tt.wantSource || data.Actionable != tt.wantActionable || data.FallbackToBuiltin != tt.wantBuiltinFallback {
				t.Fatalf("unexpected effective rules result: %+v", data)
			}
		})
	}
}

func TestHandleCooldownDetectionTestRejectsInvalidRulesSource(t *testing.T) {
	srv := newInMemoryServer(t)
	payload := map[string]any{
		"rules_source": "unknown",
		"status_code":  http.StatusTooManyRequests,
		"error_body":   `{"error":{"message":"rate limited"}}`,
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/cooldown-detection/test", payload))
	srv.HandleCooldownDetectionTest(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusBadRequest, w.Body.String())
	}
}

func TestHandleCooldownDetectionTestRejectsMalformedStandardLog(t *testing.T) {
	tests := []struct {
		name string
		log  string
	}{
		{name: "invalid status", log: `upstream status 999: {"error":{"message":"bad"}}`},
		{name: "empty body", log: "upstream status 200:   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newInMemoryServer(t)
			payload := map[string]any{
				"status_code": http.StatusOK,
				"error_body":  tt.log,
				"cooldown_detection_rules": map[string]any{
					"rules": []map[string]any{{
						"enabled": true, "name": "Status only", "priority": 0,
						"status_codes": []int{http.StatusOK}, "scope": model.CooldownScopeChannel,
						"mode": model.CooldownModeFixed, "cooldown_seconds": 60,
					}},
				},
			}
			c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels/cooldown-detection/test", payload))
			srv.HandleCooldownDetectionTest(c)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusBadRequest, w.Body.String())
			}
		})
	}
}
