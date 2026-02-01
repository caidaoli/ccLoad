package model

import (
	"testing"
	"time"
)

func TestHashToken(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  string
	}{
		{
			name:  "标准令牌",
			token: "sk-ant-1234567890abcdef",
			want:  "8a6d3b9c7f2e1a5d4c8b7a6f5e4d3c2b1a9f8e7d6c5b4a3f2e1d0c9b8a7f6e5d", // SHA256哈希
		},
		{
			name:  "空字符串",
			token: "",
			want:  "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", // 空字符串的SHA256
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := HashToken(tt.token)
			// 验证是否为64字符的十六进制字符串
			if len(got) != 64 {
				t.Errorf("HashToken() 返回长度 = %v, 期望 64", len(got))
			}
			// 每次调用应返回相同的哈希值
			got2 := HashToken(tt.token)
			if got != got2 {
				t.Errorf("HashToken() 不一致: %v != %v", got, got2)
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

func TestHashToken_Consistency(t *testing.T) {
	// 验证相同输入总是产生相同输出
	token := "sk-ant-test-token-12345" //nolint:gosec // G101: 测试用假凭证
	hash1 := HashToken(token)
	hash2 := HashToken(token)
	hash3 := HashToken(token)

	if hash1 != hash2 || hash2 != hash3 {
		t.Errorf("HashToken() 不一致: %v, %v, %v", hash1, hash2, hash3)
	}

	// 验证不同输入产生不同输出
	differentToken := "sk-ant-test-token-54321" //nolint:gosec // G101: 测试用假凭证
	differentHash := HashToken(differentToken)

	if hash1 == differentHash {
		t.Errorf("HashToken() 不同输入产生相同哈希值")
	}
}
