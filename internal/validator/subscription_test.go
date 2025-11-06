package validator

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ccLoad/internal/model"
)

// TestSubscriptionValidator_ShouldValidate 测试渠道名称匹配逻辑
func TestSubscriptionValidator_ShouldValidate(t *testing.T) {
	tests := []struct {
		name        string
		channelName string
		enabled     bool
		expected    bool
	}{
		{
			name:        "启用+88code前缀(小写)",
			channelName: "88code-test",
			enabled:     true,
			expected:    true,
		},
		{
			name:        "启用+88code前缀(大写)",
			channelName: "88Code-prod",
			enabled:     true,
			expected:    true,
		},
		{
			name:        "启用+88CODE前缀(全大写)",
			channelName: "88CODE_backup",
			enabled:     true,
			expected:    true,
		},
		{
			name:        "启用+非88code前缀",
			channelName: "anthropic-test",
			enabled:     true,
			expected:    false,
		},
		{
			name:        "禁用+88code前缀",
			channelName: "88code-test",
			enabled:     false,
			expected:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := NewSubscriptionValidator(tt.enabled)
			cfg := &model.Config{Name: tt.channelName}

			result := v.ShouldValidate(cfg)
			if result != tt.expected {
				t.Errorf("ShouldValidate() = %v, expected %v", result, tt.expected)
			}
		})
	}
}

// TestSubscriptionValidator_Validate_FreeSubscription 测试免费套餐验证
func TestSubscriptionValidator_Validate_FreeSubscription(t *testing.T) {
	// Mock 88code API返回FREE套餐
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 验证请求方法
		if r.Method != http.MethodPost {
			t.Errorf("Expected POST method, got %s", r.Method)
		}

		// 验证请求头
		if r.Header.Get("Authorization") != "Bearer test-key-123" {
			t.Errorf("Unexpected Authorization header: %s", r.Header.Get("Authorization"))
		}

		// 返回FREE套餐
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"subscriptionName": "FREE",
		})
	}))
	defer server.Close()

	v := NewSubscriptionValidator(true)
	v.apiURL = server.URL // 覆盖API URL为测试服务器

	cfg := &model.Config{
		ID:   1,
		Name: "88code-test",
	}

	available, reason, err := v.Validate(context.Background(), cfg, "test-key-123")

	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if !available {
		t.Errorf("Validate() available = false, expected true for FREE subscription")
	}
	if reason != "" {
		t.Errorf("Validate() reason = %q, expected empty for FREE subscription", reason)
	}
}

// TestSubscriptionValidator_Validate_ProSubscription 测试付费套餐验证
func TestSubscriptionValidator_Validate_ProSubscription(t *testing.T) {
	// Mock 88code API返回PRO套餐
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 验证请求方法
		if r.Method != http.MethodPost {
			t.Errorf("Expected POST method, got %s", r.Method)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"subscriptionName": "PRO",
		})
	}))
	defer server.Close()

	v := NewSubscriptionValidator(true)
	v.apiURL = server.URL

	cfg := &model.Config{
		ID:   1,
		Name: "88code-test",
	}

	available, reason, err := v.Validate(context.Background(), cfg, "test-key-123")

	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if available {
		t.Errorf("Validate() available = true, expected false for PRO subscription")
	}
	if reason == "" {
		t.Errorf("Validate() reason is empty, expected non-empty for PRO subscription")
	}
	if reason != "subscription=PRO (not FREE)" {
		t.Errorf("Validate() reason = %q, expected \"subscription=PRO (not FREE)\"", reason)
	}
}

// TestSubscriptionValidator_Validate_CaseInsensitive 测试大小写不敏感
func TestSubscriptionValidator_Validate_CaseInsensitive(t *testing.T) {
	subscriptions := []string{"free", "Free", "FREE", "FrEe"}

	for _, sub := range subscriptions {
		t.Run("subscription="+sub, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// 验证请求方法
				if r.Method != http.MethodPost {
					t.Errorf("Expected POST method, got %s", r.Method)
				}

				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]string{
					"subscriptionName": sub,
				})
			}))
			defer server.Close()

			v := NewSubscriptionValidator(true)
			v.apiURL = server.URL

			cfg := &model.Config{ID: 1, Name: "88code-test"}

			available, _, err := v.Validate(context.Background(), cfg, "test-key")

			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if !available {
				t.Errorf("Validate() available = false for %q, expected true (case-insensitive)", sub)
			}
		})
	}
}

// TestSubscriptionValidator_Validate_APIError 测试API错误处理
func TestSubscriptionValidator_Validate_APIError(t *testing.T) {
	tests := []struct {
		name           string
		statusCode     int
		responseBody   string
		expectError    bool
		errorSubstring string
	}{
		{
			name:           "401 Unauthorized",
			statusCode:     401,
			responseBody:   `{"error": "Invalid API key"}`,
			expectError:    true,
			errorSubstring: "unexpected status code 401",
		},
		{
			name:           "500 Internal Server Error",
			statusCode:     500,
			responseBody:   "Internal Server Error",
			expectError:    true,
			errorSubstring: "unexpected status code 500",
		},
		{
			name:           "Invalid JSON",
			statusCode:     200,
			responseBody:   `{invalid json`,
			expectError:    true,
			errorSubstring: "failed to decode response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// 验证请求方法
				if r.Method != http.MethodPost {
					t.Errorf("Expected POST method, got %s", r.Method)
				}

				w.WriteHeader(tt.statusCode)
				w.Write([]byte(tt.responseBody))
			}))
			defer server.Close()

			v := NewSubscriptionValidator(true)
			v.apiURL = server.URL

			cfg := &model.Config{ID: 1, Name: "88code-test"}

			_, _, err := v.Validate(context.Background(), cfg, "test-key")

			if tt.expectError && err == nil {
				t.Errorf("Validate() expected error, got nil")
			}
			if tt.expectError && err != nil {
				if tt.errorSubstring != "" && !contains(err.Error(), tt.errorSubstring) {
					t.Errorf("Validate() error = %v, expected to contain %q", err, tt.errorSubstring)
				}
			}
			if !tt.expectError && err != nil {
				t.Errorf("Validate() unexpected error: %v", err)
			}
		})
	}
}

// TestSubscriptionValidator_Cache 测试缓存机制
func TestSubscriptionValidator_Cache(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 验证请求方法
		if r.Method != http.MethodPost {
			t.Errorf("Expected POST method, got %s", r.Method)
		}

		callCount++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"subscriptionName": "FREE",
		})
	}))
	defer server.Close()

	v := NewSubscriptionValidator(true)
	v.apiURL = server.URL
	v.SetCacheTTL(10 * time.Second) // 设置长TTL便于测试

	cfg := &model.Config{ID: 1, Name: "88code-test"}

	// 第一次调用:应该触发API请求
	_, _, err := v.Validate(context.Background(), cfg, "test-key")
	if err != nil {
		t.Fatalf("First Validate() error = %v", err)
	}

	if callCount != 1 {
		t.Errorf("Expected 1 API call after first validation, got %d", callCount)
	}

	// 第二次调用:应该使用缓存,不触发API请求
	_, _, err = v.Validate(context.Background(), cfg, "test-key")
	if err != nil {
		t.Fatalf("Second Validate() error = %v", err)
	}

	if callCount != 1 {
		t.Errorf("Expected 1 API call after second validation (cached), got %d", callCount)
	}

	// 清空缓存后再次调用:应该重新触发API请求
	v.ClearCache()

	_, _, err = v.Validate(context.Background(), cfg, "test-key")
	if err != nil {
		t.Fatalf("Third Validate() error = %v", err)
	}

	if callCount != 2 {
		t.Errorf("Expected 2 API calls after cache clear, got %d", callCount)
	}
}

// TestSubscriptionValidator_CacheExpiry 测试缓存过期
func TestSubscriptionValidator_CacheExpiry(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 验证请求方法
		if r.Method != http.MethodPost {
			t.Errorf("Expected POST method, got %s", r.Method)
		}

		callCount++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"subscriptionName": "FREE",
		})
	}))
	defer server.Close()

	v := NewSubscriptionValidator(true)
	v.apiURL = server.URL
	v.SetCacheTTL(100 * time.Millisecond) // 短TTL用于测试过期

	cfg := &model.Config{ID: 1, Name: "88code-test"}

	// 第一次调用
	v.Validate(context.Background(), cfg, "test-key")

	// 等待缓存过期
	time.Sleep(150 * time.Millisecond)

	// 再次调用:缓存已过期,应该重新请求
	v.Validate(context.Background(), cfg, "test-key")

	if callCount != 2 {
		t.Errorf("Expected 2 API calls after cache expiry, got %d", callCount)
	}
}

// TestSubscriptionValidator_Timeout 测试超时控制
func TestSubscriptionValidator_Timeout(t *testing.T) {
	// Mock一个慢速服务器
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 验证请求方法
		if r.Method != http.MethodPost {
			t.Errorf("Expected POST method, got %s", r.Method)
		}

		time.Sleep(2 * time.Second) // 模拟慢响应
		json.NewEncoder(w).Encode(map[string]string{
			"subscriptionName": "FREE",
		})
	}))
	defer server.Close()

	v := NewSubscriptionValidator(true)
	v.apiURL = server.URL
	v.SetAPITimeout(500 * time.Millisecond) // 设置短超时

	cfg := &model.Config{ID: 1, Name: "88code-test"}

	_, _, err := v.Validate(context.Background(), cfg, "test-key")

	if err == nil {
		t.Errorf("Validate() expected timeout error, got nil")
	}
	if err != nil && !contains(err.Error(), "context deadline exceeded") {
		t.Errorf("Validate() error = %v, expected context deadline exceeded", err)
	}
}

// contains 检查字符串是否包含子串
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || len(s) > len(substr)*2 && containsMiddle(s, substr)))
}

func containsMiddle(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
