package app

import (
	"strings"
	"testing"

	"ccLoad/internal/model"
)

type channelRequestFieldCase struct {
	name           string
	input          string
	wantErr        bool
	wantNormalized string
}

func newValidChannelRequest() *ChannelRequest {
	return &ChannelRequest{
		Name:   "test-channel",
		APIKey: "test-key",
		URL:    "https://example.com",
		Models: []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
	}
}

func runChannelRequestFieldValidation(
	t *testing.T,
	cases []channelRequestFieldCase,
	setField func(*ChannelRequest, string),
	getField func(*ChannelRequest) string,
	invalidErrContains string,
) {
	t.Helper()

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			req := newValidChannelRequest()
			setField(req, tt.input)

			err := req.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && getField(req) != tt.wantNormalized {
				t.Errorf("字段标准化失败: got %q, want %q", getField(req), tt.wantNormalized)
			}

			if tt.wantErr && err != nil && invalidErrContains != "" {
				if !strings.Contains(err.Error(), invalidErrContains) {
					t.Errorf("错误信息应该包含 %q, got: %v", invalidErrContains, err)
				}
			}
		})
	}
}

// TestChannelRequestValidation_ChannelType 测试 channel_type 白名单校验
func TestChannelRequestValidation_ChannelType(t *testing.T) {
	tests := []channelRequestFieldCase{
		{
			name:           "空值应该通过（使用默认值）",
			input:          "",
			wantErr:        false,
			wantNormalized: "",
		},
		{
			name:           "anthropic 小写应该通过",
			input:          "anthropic",
			wantErr:        false,
			wantNormalized: "anthropic",
		},
		{
			name:           "Anthropic 大写应该标准化为小写",
			input:          "Anthropic",
			wantErr:        false,
			wantNormalized: "anthropic",
		},
		{
			name:           "openai 应该通过",
			input:          "openai",
			wantErr:        false,
			wantNormalized: "openai",
		},
		{
			name:           "gemini 应该通过",
			input:          "gemini",
			wantErr:        false,
			wantNormalized: "gemini",
		},
		{
			name:           "codex 应该通过",
			input:          "codex",
			wantErr:        false,
			wantNormalized: "codex",
		},
		{
			name:           "带空格的 anthropic 应该 trim 并通过",
			input:          "  anthropic  ",
			wantErr:        false,
			wantNormalized: "anthropic",
		},
		{
			name:    "非法值应该拒绝",
			input:   "invalid_type",
			wantErr: true,
		},
		{
			name:    "垃圾值应该拒绝",
			input:   "xyz123",
			wantErr: true,
		},
	}

	runChannelRequestFieldValidation(
		t,
		tests,
		func(req *ChannelRequest, v string) { req.ChannelType = v },
		func(req *ChannelRequest) string { return req.ChannelType },
		"invalid channel_type",
	)
}

// TestChannelRequestValidation_KeyStrategy 测试 key_strategy 白名单校验
func TestChannelRequestValidation_KeyStrategy(t *testing.T) {
	tests := []channelRequestFieldCase{
		{
			name:           "空值应该通过（使用默认值）",
			input:          "",
			wantErr:        false,
			wantNormalized: "",
		},
		{
			name:           "sequential 小写应该通过",
			input:          "sequential",
			wantErr:        false,
			wantNormalized: "sequential",
		},
		{
			name:           "Sequential 大写应该标准化为小写",
			input:          "Sequential",
			wantErr:        false,
			wantNormalized: "sequential",
		},
		{
			name:           "round_robin 应该通过",
			input:          "round_robin",
			wantErr:        false,
			wantNormalized: "round_robin",
		},
		{
			name:           "ROUND_ROBIN 大写应该标准化为小写",
			input:          "ROUND_ROBIN",
			wantErr:        false,
			wantNormalized: "round_robin",
		},
		{
			name:           "带空格的 sequential 应该 trim 并通过",
			input:          "  sequential  ",
			wantErr:        false,
			wantNormalized: "sequential",
		},
		{
			name:    "非法值应该拒绝",
			input:   "invalid_strategy",
			wantErr: true,
		},
		{
			name:    "垃圾值应该拒绝",
			input:   "random",
			wantErr: true,
		},
	}

	runChannelRequestFieldValidation(
		t,
		tests,
		func(req *ChannelRequest, v string) { req.KeyStrategy = v },
		func(req *ChannelRequest) string { return req.KeyStrategy },
		"invalid key_strategy",
	)
}

// TestChannelRequestValidation_Combined 测试组合场景
func TestChannelRequestValidation_Combined(t *testing.T) {
	tests := []struct {
		name        string
		req         ChannelRequest
		wantErr     bool
		errContains string
	}{
		{
			name: "完全合法的请求",
			req: ChannelRequest{
				Name:        "test-channel",
				APIKey:      "test-key",
				URL:         "https://example.com",
				Models:      []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
				ChannelType: "anthropic",
				KeyStrategy: "round_robin",
			},
			wantErr: false,
		},
		{
			name: "非法 channel_type 应该在第一个被拦截",
			req: ChannelRequest{
				Name:        "test-channel",
				APIKey:      "test-key",
				URL:         "https://example.com",
				Models:      []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
				ChannelType: "invalid",
				KeyStrategy: "sequential",
			},
			wantErr:     true,
			errContains: "invalid channel_type",
		},
		{
			name: "非法 key_strategy 应该被拦截",
			req: ChannelRequest{
				Name:        "test-channel",
				APIKey:      "test-key",
				URL:         "https://example.com",
				Models:      []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
				ChannelType: "anthropic",
				KeyStrategy: "invalid",
			},
			wantErr:     true,
			errContains: "invalid key_strategy",
		},
		{
			name: "同时非法应该报第一个错误（channel_type 在前）",
			req: ChannelRequest{
				Name:        "test-channel",
				APIKey:      "test-key",
				URL:         "https://example.com",
				Models:      []model.ModelEntry{{Model: "model-1", RedirectModel: ""}},
				ChannelType: "invalid_type",
				KeyStrategy: "invalid_strategy",
			},
			wantErr:     true,
			errContains: "invalid channel_type", // channel_type 校验在前
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr && err != nil {
				if !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("错误信息应该包含 %q, got: %v", tt.errContains, err)
				}
			}
		})
	}
}

// TestChannelRequestValidation_DuplicateModels 测试重复模型校验（对应 channel_models 主键约束）
func TestChannelRequestValidation_DuplicateModels(t *testing.T) {
	tests := []struct {
		name   string
		models []model.ModelEntry
	}{
		{
			name: "完全重复模型应该拒绝",
			models: []model.ModelEntry{
				{Model: "gpt-5.2"},
				{Model: "gpt-5.2"},
			},
		},
		{
			name: "大小写差异模型应视为重复并拒绝",
			models: []model.ModelEntry{
				{Model: "GPT-5.2"},
				{Model: "gpt-5.2"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newValidChannelRequest()
			req.Models = tt.models

			err := req.Validate()
			if err == nil {
				t.Fatal("expected duplicate model validation error, got nil")
			}
			if !strings.Contains(err.Error(), "duplicate model") {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
