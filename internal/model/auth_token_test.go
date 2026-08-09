package model

import (
	"testing"
	"time"
)

// HashToken 的输出会持久化进数据库并用于令牌校验，算法一旦变化所有已存令牌立即失效，
// 因此断言固定的 SHA256 向量而非长度或自洽性。
func TestHashToken(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  string
	}{
		{
			name:  "标准令牌",
			token: "sk-ant-1234567890abcdef",
			want:  "e1ff2b6240d21c9b8524ffb591363c1b5c44c297666ae70ae7bafbe7ad83287d",
		},
		{
			name:  "空字符串",
			token: "",
			want:  "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HashToken(tt.token); got != tt.want {
				t.Errorf("HashToken(%q) = %v, 期望 %v", tt.token, got, tt.want)
			}
		})
	}
}

func TestAuthToken_IsExpired(t *testing.T) {
	now := time.Now().UnixMilli()
	past := now - 1000
	future := now + 1000

	tests := []struct {
		name      string
		expiresAt *int64
		want      bool
	}{
		{
			name:      "永不过期(nil)",
			expiresAt: nil,
			want:      false,
		},
		{
			name:      "已过期",
			expiresAt: &past,
			want:      true,
		},
		{
			name:      "未过期",
			expiresAt: &future,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token := &AuthToken{
				ExpiresAt: tt.expiresAt,
			}
			if got := token.IsExpired(); got != tt.want {
				t.Errorf("IsExpired() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAuthToken_IsValid(t *testing.T) {
	now := time.Now().UnixMilli()
	past := now - 1000
	future := now + 1000

	tests := []struct {
		name      string
		isActive  bool
		expiresAt *int64
		want      bool
	}{
		{
			name:      "有效令牌(启用+未过期)",
			isActive:  true,
			expiresAt: &future,
			want:      true,
		},
		{
			name:      "有效令牌(启用+永不过期)",
			isActive:  true,
			expiresAt: nil,
			want:      true,
		},
		{
			name:      "无效令牌(禁用)",
			isActive:  false,
			expiresAt: &future,
			want:      false,
		},
		{
			name:      "无效令牌(已过期)",
			isActive:  true,
			expiresAt: &past,
			want:      false,
		},
		{
			name:      "无效令牌(禁用+已过期)",
			isActive:  false,
			expiresAt: &past,
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token := &AuthToken{
				IsActive:  tt.isActive,
				ExpiresAt: tt.expiresAt,
			}
			if got := token.IsValid(); got != tt.want {
				t.Errorf("IsValid() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMaskToken(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  string
	}{
		{
			name:  "标准令牌",
			token: "sk-ant-1234567890abcdef",
			want:  "sk-a****cdef",
		},
		{
			name:  "短令牌",
			token: "short",
			want:  "****",
		},
		{
			name:  "8字符令牌(边界)",
			token: "12345678",
			want:  "****",
		},
		{
			name:  "9字符令牌",
			token: "123456789",
			want:  "1234****6789",
		},
		{
			name:  "长令牌",
			token: "sk-ant-api03-very-long-token-1234567890abcdefghijklmnopqrstuvwxyz",
			want:  "sk-a****wxyz",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MaskToken(tt.token); got != tt.want {
				t.Errorf("MaskToken() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAuthToken_UpdateLastUsed(t *testing.T) {
	token := &AuthToken{
		ID:         1,
		Token:      "test-token",
		LastUsedAt: nil,
	}

	// 第一次更新
	token.UpdateLastUsed()
	if token.LastUsedAt == nil {
		t.Fatal("UpdateLastUsed() 未设置 LastUsedAt")
	}

	firstTime := *token.LastUsedAt

	// 第二次更新
	token.UpdateLastUsed()
	if token.LastUsedAt == nil {
		t.Fatal("UpdateLastUsed() LastUsedAt 变为 nil")
	}

	secondTime := *token.LastUsedAt

	// 验证时间已更新
	if secondTime <= firstTime {
		t.Errorf("UpdateLastUsed() 时间未更新: %v <= %v", secondTime, firstTime)
	}
}
