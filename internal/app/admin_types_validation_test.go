package app

import (
	"strings"
	"testing"
)

// TestChannelRequestValidation_ChannelType 测试 channel_type 白名单校验
func TestChannelRequestValidation_ChannelType(t *testing.T) {
	tests := []struct {
		name        string
		channelType string
		wantErr     bool
		wantNormalized string
	}{
		{
			name:        "空值应该通过（使用默认值）",
			channelType: "",
			wantErr:     false,
			wantNormalized: "",
		},
		{
			name:        "anthropic 小写应该通过",
			channelType: "anthropic",
			wantErr:     false,
			wantNormalized: "anthropic",
		},
		{
			name:        "Anthropic 大写应该标准化为小写",
			channelType: "Anthropic",
			wantErr:     false,
			wantNormalized: "anthropic",
		},
		{
			name:        "openai 应该通过",
			channelType: "openai",
			wantErr:     false,
			wantNormalized: "openai",
		},
		{
			name:        "gemini 应该通过",
			channelType: "gemini",
			wantErr:     false,
			wantNormalized: "gemini",
		},
		{
			name:        "codex 应该通过",
			channelType: "codex",
			wantErr:     false,
			wantNormalized: "codex",
		},
		{
			name:        "带空格的 anthropic 应该 trim 并通过",
			channelType: "  anthropic  ",
			wantErr:     false,
			wantNormalized: "anthropic",
		},
		{
			name:        "非法值应该拒绝",
			channelType: "invalid_type",
			wantErr:     true,
		},
		{
			name:        "垃圾值应该拒绝",
			channelType: "xyz123",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &ChannelRequest{
				Name:        "test-channel",
				APIKey:      "test-key",
				URL:         "https://example.com",
				Models:      []string{"model-1"},
				ChannelType: tt.channelType,
			}

			err := req.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && req.ChannelType != tt.wantNormalized {
				t.Errorf("channel_type 标准化失败: got %q, want %q", req.ChannelType, tt.wantNormalized)
			}

			if tt.wantErr && err != nil {
				if !strings.Contains(err.Error(), "invalid channel_type") {
					t.Errorf("错误信息应该包含 'invalid channel_type', got: %v", err)
				}
			}
		})
	}
}

// TestChannelRequestValidation_KeyStrategy 测试 key_strategy 白名单校验
func TestChannelRequestValidation_KeyStrategy(t *testing.T) {
	tests := []struct {
		name        string
		keyStrategy string
		wantErr     bool
		wantNormalized string
	}{
		{
			name:        "空值应该通过（使用默认值）",
			keyStrategy: "",
			wantErr:     false,
			wantNormalized: "",
		},
		{
			name:        "sequential 小写应该通过",
			keyStrategy: "sequential",
			wantErr:     false,
			wantNormalized: "sequential",
		},
		{
			name:        "Sequential 大写应该标准化为小写",
			keyStrategy: "Sequential",
			wantErr:     false,
			wantNormalized: "sequential",
		},
		{
			name:        "round_robin 应该通过",
			keyStrategy: "round_robin",
			wantErr:     false,
			wantNormalized: "round_robin",
		},
		{
			name:        "ROUND_ROBIN 大写应该标准化为小写",
			keyStrategy: "ROUND_ROBIN",
			wantErr:     false,
			wantNormalized: "round_robin",
		},
		{
			name:        "带空格的 sequential 应该 trim 并通过",
			keyStrategy: "  sequential  ",
			wantErr:     false,
			wantNormalized: "sequential",
		},
		{
			name:        "非法值应该拒绝",
			keyStrategy: "invalid_strategy",
			wantErr:     true,
		},
		{
			name:        "垃圾值应该拒绝",
			keyStrategy: "random",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &ChannelRequest{
				Name:        "test-channel",
				APIKey:      "test-key",
				URL:         "https://example.com",
				Models:      []string{"model-1"},
				KeyStrategy: tt.keyStrategy,
			}

			err := req.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && req.KeyStrategy != tt.wantNormalized {
				t.Errorf("key_strategy 标准化失败: got %q, want %q", req.KeyStrategy, tt.wantNormalized)
			}

			if tt.wantErr && err != nil {
				if !strings.Contains(err.Error(), "invalid key_strategy") {
					t.Errorf("错误信息应该包含 'invalid key_strategy', got: %v", err)
				}
			}
		})
	}
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
				Models:      []string{"model-1"},
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
				Models:      []string{"model-1"},
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
				Models:      []string{"model-1"},
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
				Models:      []string{"model-1"},
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
