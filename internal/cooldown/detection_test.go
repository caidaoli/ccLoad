package cooldown

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
)

func TestNormalizeCooldownDetectionRules_SortsAndRenumbersPriority(t *testing.T) {
	rules := &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{
		{
			Enabled: true, Name: "Status cooldown", Priority: 20, StatusCodes: []int{429, 400},
			Scope: model.CooldownScopeChannel, Mode: model.CooldownModeFixed, CooldownSeconds: 60,
		},
		{
			Enabled: true, Name: "  Quota cooldown  ", Priority: 5, MessagePattern: "quota",
			Scope: model.CooldownScopeKey, Mode: model.CooldownModeFixed, CooldownSeconds: 30,
		},
	}}

	if err := NormalizeCooldownDetectionRules(rules); err != nil {
		t.Fatalf("NormalizeCooldownDetectionRules() error = %v", err)
	}
	if got, want := rules.Rules[0].MessagePattern, "quota"; got != want {
		t.Fatalf("first sorted rule message_pattern = %q, want %q", got, want)
	}
	if got, want := rules.Rules[0].Name, "Quota cooldown"; got != want {
		t.Fatalf("first sorted rule name = %q, want %q", got, want)
	}
	if got, want := rules.Rules[0].Priority, 0; got != want {
		t.Fatalf("first normalized priority = %d, want %d", got, want)
	}
	if got, want := rules.Rules[1].Priority, 1; got != want {
		t.Fatalf("second normalized priority = %d, want %d", got, want)
	}
	if got, want := rules.Rules[1].StatusCodes, []int{400, 429}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("status codes = %#v, want %#v", got, want)
	}
}

func TestNormalizeCooldownDetectionRules_RejectsInvalidDynamicCapture(t *testing.T) {
	rules := &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
		Enabled: true, Name: "Invalid capture", Priority: 0, StatusCodes: []int{429}, MessagePattern: `reset at (?P<until>.+)`,
		Scope: model.CooldownScopeChannel, Mode: model.CooldownModeResetTime,
		TimeCapture: "missing", TimeLayout: "2006-01-02 15:04:05", Timezone: "Asia/Shanghai",
	}}}

	err := NormalizeCooldownDetectionRules(rules)
	if err == nil || !strings.Contains(err.Error(), "time_capture") {
		t.Fatalf("NormalizeCooldownDetectionRules() error = %v, want time_capture validation error", err)
	}
}

func TestEvaluateCooldownDetectionRules_FirstMatchWins(t *testing.T) {
	rules := &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{
		{
			Enabled: true, Name: "Key cooldown", Priority: 0, StatusCodes: []int{429},
			Scope: model.CooldownScopeKey, Mode: model.CooldownModeFixed, CooldownSeconds: 90,
		},
		{
			Enabled: true, Name: "Channel cooldown", Priority: 1, StatusCodes: []int{429}, MessagePattern: "monthly limit",
			Scope: model.CooldownScopeChannel, Mode: model.CooldownModeFixed, CooldownSeconds: 3600,
		},
	}}
	if err := NormalizeCooldownDetectionRules(rules); err != nil {
		t.Fatalf("NormalizeCooldownDetectionRules() error = %v", err)
	}
	now := time.Date(2026, time.July, 23, 9, 0, 0, 0, time.UTC)
	evaluation := EvaluateCooldownDetectionRules(rules, DetectionInput{
		StatusCode: 429,
		ErrorBody:  []byte(`{"error":{"code":"AccountQuotaExceeded","message":"monthly limit"}}`),
	}, now)
	if !evaluation.Actionable || evaluation.Scope != model.CooldownScopeKey || evaluation.Priority != 0 {
		t.Fatalf("evaluation = %+v, want first key rule", evaluation)
	}
	if got, want := evaluation.CooldownUntil, now.Add(90*time.Second); !got.Equal(want) {
		t.Fatalf("CooldownUntil = %v, want %v", got, want)
	}
}

func TestEvaluateCooldownDetectionRules_ParsesConfiguredResetTimeFormats(t *testing.T) {
	now := time.Date(2026, time.July, 23, 9, 0, 0, 0, time.UTC)
	want := now.Add(90 * time.Second)
	tests := []struct {
		name      string
		format    string
		capture   string
		layout    string
		timezone  string
		wantUntil time.Time
	}{
		{
			name:      "datetime",
			format:    model.CooldownTimeFormatDateTime,
			capture:   "2026-07-23 09:01:30",
			layout:    "2006-01-02 15:04:05",
			timezone:  "UTC",
			wantUntil: want,
		},
		{
			name:      "unix seconds",
			format:    model.CooldownTimeFormatUnix,
			capture:   strconv.FormatInt(want.Unix(), 10),
			wantUntil: want,
		},
		{
			name:      "unix milliseconds",
			format:    model.CooldownTimeFormatUnixMilliseconds,
			capture:   strconv.FormatInt(want.UnixMilli(), 10),
			wantUntil: want,
		},
		{
			name:      "duration seconds",
			format:    model.CooldownTimeFormatDurationSeconds,
			capture:   "90",
			wantUntil: want,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules := &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
				Enabled:        true,
				Name:           tt.name,
				Priority:       0,
				StatusCodes:    []int{429},
				MessagePattern: `reset (?P<reset_time>.+)`,
				Scope:          model.CooldownScopeChannel,
				Mode:           model.CooldownModeResetTime,
				TimeCapture:    "reset_time",
				TimeFormat:     tt.format,
				TimeLayout:     tt.layout,
				Timezone:       tt.timezone,
			}}}
			if err := NormalizeCooldownDetectionRules(rules); err != nil {
				t.Fatalf("NormalizeCooldownDetectionRules() error = %v", err)
			}

			evaluation := EvaluateCooldownDetectionRules(rules, DetectionInput{
				StatusCode: 429,
				ErrorBody:  []byte(fmt.Sprintf(`{"error":{"message":"reset %s"}}`, tt.capture)),
			}, now)
			if !evaluation.Actionable {
				t.Fatalf("evaluation = %+v, want actionable", evaluation)
			}
			if got := evaluation.CooldownUntil; !got.Equal(tt.wantUntil) {
				t.Fatalf("CooldownUntil = %v, want %v", got, tt.wantUntil)
			}
		})
	}
}

func TestEvaluateCooldownDetectionRules_ParsesResetTimeAndFallsBackWhenInvalid(t *testing.T) {
	rules := &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
		Enabled: true, Name: "Reset time", Priority: 0, StatusCodes: []int{429},
		MessagePattern: `reset at (?P<reset_time>\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`,
		Scope:          model.CooldownScopeChannel,
		Mode:           model.CooldownModeResetTime,
		TimeCapture:    "reset_time",
		TimeLayout:     "2006-01-02 15:04:05",
		Timezone:       "Asia/Shanghai",
	}}}
	if err := NormalizeCooldownDetectionRules(rules); err != nil {
		t.Fatalf("NormalizeCooldownDetectionRules() error = %v", err)
	}
	if got, want := rules.Rules[0].TimeFormat, model.CooldownTimeFormatDateTime; got != want {
		t.Fatalf("legacy time format = %q, want %q", got, want)
	}
	now := time.Date(2026, time.July, 23, 9, 0, 0, 0, time.UTC)

	valid := EvaluateCooldownDetectionRules(rules, DetectionInput{
		StatusCode: 429,
		ErrorBody:  []byte(`{"error":{"message":"limit will reset at 2026-07-24 08:30:00"}}`),
	}, now)
	if !valid.Actionable {
		t.Fatalf("valid reset evaluation = %+v, want actionable", valid)
	}
	want := time.Date(2026, time.July, 24, 8, 30, 0, 0, time.FixedZone("CST", 8*60*60))
	if !valid.CooldownUntil.Equal(want) {
		t.Fatalf("CooldownUntil = %v, want %v", valid.CooldownUntil, want)
	}
	if valid.Captures["reset_time"] != "2026-07-24 08:30:00" {
		t.Fatalf("captures = %#v", valid.Captures)
	}

	invalid := EvaluateCooldownDetectionRules(rules, DetectionInput{
		StatusCode: 429,
		ErrorBody:  []byte(`{"error":{"message":"limit will reset at 2020-01-01 00:00:00"}}`),
	}, now)
	if !invalid.Matched || invalid.Actionable || invalid.FallbackReason != "reset_time_not_future" {
		t.Fatalf("invalid reset evaluation = %+v, want matched non-actionable fallback", invalid)
	}
}

func TestHandleError_ConfiguredCooldownDetectionOverridesBuiltin(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()
	cfg := createTestChannel(t, store, "configured-detection")
	manager := NewManager(store, nil)

	before := time.Now()
	action := manager.HandleError(ctx, ErrorInput{
		ChannelID:  cfg.ID,
		KeyIndex:   NoKeyIndex,
		StatusCode: 406, // Built-in classification returns directly to the client.
		ErrorBody:  []byte(`{"error":{"code":"maintenance","message":"planned maintenance"}}`),
		CooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
			Enabled: true, Name: "Maintenance", Priority: 0, StatusCodes: []int{406}, MessagePattern: "planned maintenance",
			Scope: model.CooldownScopeChannel, Mode: model.CooldownModeFixed, CooldownSeconds: 120,
		}}},
	})
	if action != ActionRetryChannel {
		t.Fatalf("HandleError() action = %v, want ActionRetryChannel", action)
	}
	updated, err := store.GetConfig(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("GetConfig() error = %v", err)
	}
	until := time.Unix(updated.CooldownUntil, 0)
	if until.Before(before.Add(119*time.Second)) || until.After(time.Now().Add(121*time.Second)) {
		t.Fatalf("channel cooldown until = %v, want approximately two minutes after now", until)
	}
}

func TestHandleError_ConfiguredCooldownDetectionStoresSelectedScope(t *testing.T) {
	tests := []struct {
		name       string
		scope      string
		keyIndex   int
		model      string
		wantAction Action
	}{
		{name: "key", scope: model.CooldownScopeKey, keyIndex: 0, wantAction: ActionRetryKey},
		{name: "model", scope: model.CooldownScopeModel, keyIndex: NoKeyIndex, model: "test-model", wantAction: ActionRetryModel},
		{name: "channel", scope: model.CooldownScopeChannel, keyIndex: NoKeyIndex, wantAction: ActionRetryChannel},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, cleanup := setupTestStore(t)
			defer cleanup()
			ctx := context.Background()
			cfg := createTestChannel(t, store, "configured-scope-"+tt.name)
			if tt.scope == model.CooldownScopeKey {
				if err := store.CreateAPIKeysBatch(ctx, []*model.APIKey{
					{ChannelID: cfg.ID, KeyIndex: 0, APIKey: "key-0", KeyStrategy: model.KeyStrategySequential},
					{ChannelID: cfg.ID, KeyIndex: 1, APIKey: "key-1", KeyStrategy: model.KeyStrategySequential},
				}); err != nil {
					t.Fatalf("CreateAPIKeysBatch() error = %v", err)
				}
			}
			if tt.scope == model.CooldownScopeModel {
				cfg.ModelEntries = append(cfg.ModelEntries, model.ModelEntry{Model: "other-model"})
				if _, err := store.UpdateConfig(ctx, cfg.ID, cfg); err != nil {
					t.Fatalf("UpdateConfig() error = %v", err)
				}
			}

			before := time.Now()
			action := NewManager(store, nil).HandleError(ctx, ErrorInput{
				ChannelID:  cfg.ID,
				KeyIndex:   tt.keyIndex,
				Model:      tt.model,
				StatusCode: 406,
				CooldownDetectionRules: &model.CooldownDetectionRules{Rules: []model.CooldownDetectionRule{{
					Enabled: true, Name: tt.name, Priority: 0, StatusCodes: []int{406},
					Scope: tt.scope, Mode: model.CooldownModeFixed, CooldownSeconds: 120,
				}}},
			})
			if action != tt.wantAction {
				t.Fatalf("HandleError() action = %v, want %v", action, tt.wantAction)
			}

			switch tt.scope {
			case model.CooldownScopeKey:
				key, err := store.GetAPIKey(ctx, cfg.ID, 0)
				if err != nil {
					t.Fatalf("GetAPIKey() error = %v", err)
				}
				if time.Unix(key.CooldownUntil, 0).Before(before.Add(119 * time.Second)) {
					t.Fatalf("key cooldown = %v, want about two minutes after now", time.Unix(key.CooldownUntil, 0))
				}
			case model.CooldownScopeModel:
				cooldowns, err := store.GetAllModelCooldowns(ctx)
				if err != nil {
					t.Fatalf("GetAllModelCooldowns() error = %v", err)
				}
				if until := cooldowns[cfg.ID][tt.model]; until.Before(before.Add(119 * time.Second)) {
					t.Fatalf("model cooldown = %v, want about two minutes after now", until)
				}
			case model.CooldownScopeChannel:
				updated, err := store.GetConfig(ctx, cfg.ID)
				if err != nil {
					t.Fatalf("GetConfig() error = %v", err)
				}
				if until := time.Unix(updated.CooldownUntil, 0); until.Before(before.Add(119 * time.Second)) {
					t.Fatalf("channel cooldown = %v, want about two minutes after now", until)
				}
			}
		})
	}
}
