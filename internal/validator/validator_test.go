package validator

import (
	"context"
	"errors"
	"testing"

	"ccLoad/internal/model"
)

// mockValidator 测试用Mock验证器
type mockValidator struct {
	shouldValidateFunc func(*model.Config) bool
	validateFunc       func(context.Context, *model.Config, string) (bool, string, error)
}

func (m *mockValidator) ShouldValidate(cfg *model.Config) bool {
	if m.shouldValidateFunc != nil {
		return m.shouldValidateFunc(cfg)
	}
	return true
}

func (m *mockValidator) Validate(ctx context.Context, cfg *model.Config, apiKey string) (bool, string, error) {
	if m.validateFunc != nil {
		return m.validateFunc(ctx, cfg, apiKey)
	}
	return true, "", nil
}

// TestManager_EmptyValidatorList 测试空验证器列表
func TestManager_EmptyValidatorList(t *testing.T) {
	mgr := NewManager()

	if mgr.ValidatorCount() != 0 {
		t.Errorf("ValidatorCount() = %d, expected 0", mgr.ValidatorCount())
	}

	cfg := &model.Config{Name: "test-channel"}
	available, reason := mgr.ValidateChannel(context.Background(), cfg, "test-key")

	if !available {
		t.Errorf("ValidateChannel() available = false with empty validator list, expected true")
	}
	if reason != "" {
		t.Errorf("ValidateChannel() reason = %q, expected empty", reason)
	}
}

// TestManager_AddValidator 测试添加验证器
func TestManager_AddValidator(t *testing.T) {
	mgr := NewManager()

	mgr.AddValidator(&mockValidator{})
	if mgr.ValidatorCount() != 1 {
		t.Errorf("ValidatorCount() = %d, expected 1", mgr.ValidatorCount())
	}

	mgr.AddValidator(&mockValidator{})
	if mgr.ValidatorCount() != 2 {
		t.Errorf("ValidatorCount() = %d, expected 2", mgr.ValidatorCount())
	}
}

// TestManager_ValidateChannel_PassAll 测试所有验证器通过
func TestManager_ValidateChannel_PassAll(t *testing.T) {
	mgr := NewManager()

	// 添加3个都通过的验证器
	for i := 0; i < 3; i++ {
		mgr.AddValidator(&mockValidator{
			validateFunc: func(ctx context.Context, cfg *model.Config, apiKey string) (bool, string, error) {
				return true, "", nil
			},
		})
	}

	cfg := &model.Config{Name: "test-channel"}
	available, reason := mgr.ValidateChannel(context.Background(), cfg, "test-key")

	if !available {
		t.Errorf("ValidateChannel() available = false, expected true when all pass")
	}
	if reason != "" {
		t.Errorf("ValidateChannel() reason = %q, expected empty when all pass", reason)
	}
}

// TestManager_ValidateChannel_FailOne 测试一个验证器失败
func TestManager_ValidateChannel_FailOne(t *testing.T) {
	mgr := NewManager()

	// 第一个验证器通过
	mgr.AddValidator(&mockValidator{
		validateFunc: func(ctx context.Context, cfg *model.Config, apiKey string) (bool, string, error) {
			return true, "", nil
		},
	})

	// 第二个验证器失败
	mgr.AddValidator(&mockValidator{
		validateFunc: func(ctx context.Context, cfg *model.Config, apiKey string) (bool, string, error) {
			return false, "validation failed reason", nil
		},
	})

	// 第三个验证器不应该被执行(责任链中断)
	executed := false
	mgr.AddValidator(&mockValidator{
		validateFunc: func(ctx context.Context, cfg *model.Config, apiKey string) (bool, string, error) {
			executed = true
			return true, "", nil
		},
	})

	cfg := &model.Config{Name: "test-channel"}
	available, reason := mgr.ValidateChannel(context.Background(), cfg, "test-key")

	if available {
		t.Errorf("ValidateChannel() available = true, expected false when one fails")
	}
	if reason != "validation failed reason" {
		t.Errorf("ValidateChannel() reason = %q, expected \"validation failed reason\"", reason)
	}
	if executed {
		t.Errorf("Third validator was executed, expected chain to break after second validator failed")
	}
}

// TestManager_ValidateChannel_ErrorDefensivePolicy 测试错误时的防御性策略
func TestManager_ValidateChannel_ErrorDefensivePolicy(t *testing.T) {
	mgr := NewManager()

	// 第一个验证器返回错误
	mgr.AddValidator(&mockValidator{
		validateFunc: func(ctx context.Context, cfg *model.Config, apiKey string) (bool, string, error) {
			return false, "", errors.New("network error")
		},
	})

	// 第二个验证器正常通过
	secondExecuted := false
	mgr.AddValidator(&mockValidator{
		validateFunc: func(ctx context.Context, cfg *model.Config, apiKey string) (bool, string, error) {
			secondExecuted = true
			return true, "", nil
		},
	})

	cfg := &model.Config{Name: "test-channel"}
	available, reason := mgr.ValidateChannel(context.Background(), cfg, "test-key")

	// 防御性策略:第一个验证器错误时默认允许通过,继续执行后续验证器
	if !available {
		t.Errorf("ValidateChannel() available = false, expected true (defensive policy)")
	}
	if reason != "" {
		t.Errorf("ValidateChannel() reason = %q, expected empty (all pass)", reason)
	}
	if !secondExecuted {
		t.Errorf("Second validator was not executed, expected to continue after first validator error")
	}
}

// TestManager_ValidateChannel_ShouldValidateFilter 测试ShouldValidate过滤
func TestManager_ValidateChannel_ShouldValidateFilter(t *testing.T) {
	mgr := NewManager()

	// 添加一个不需要验证的验证器
	skippedExecuted := false
	mgr.AddValidator(&mockValidator{
		shouldValidateFunc: func(cfg *model.Config) bool {
			return false // 跳过验证
		},
		validateFunc: func(ctx context.Context, cfg *model.Config, apiKey string) (bool, string, error) {
			skippedExecuted = true
			return false, "should not execute", nil
		},
	})

	// 添加一个需要验证的验证器
	executedCount := 0
	mgr.AddValidator(&mockValidator{
		shouldValidateFunc: func(cfg *model.Config) bool {
			return true // 需要验证
		},
		validateFunc: func(ctx context.Context, cfg *model.Config, apiKey string) (bool, string, error) {
			executedCount++
			return true, "", nil
		},
	})

	cfg := &model.Config{Name: "test-channel"}
	available, _ := mgr.ValidateChannel(context.Background(), cfg, "test-key")

	if skippedExecuted {
		t.Errorf("First validator was executed, expected to be skipped by ShouldValidate")
	}
	if executedCount != 1 {
		t.Errorf("Second validator executed %d times, expected 1", executedCount)
	}
	if !available {
		t.Errorf("ValidateChannel() available = false, expected true")
	}
}

// TestManager_ValidateChannel_MultipleValidators 测试多个验证器的组合
func TestManager_ValidateChannel_MultipleValidators(t *testing.T) {
	tests := []struct {
		name    string
		results []struct {
			available bool
			reason    string
			err       error
		}
		expected  bool
		expReason string
	}{
		{
			name: "所有通过",
			results: []struct {
				available bool
				reason    string
				err       error
			}{
				{true, "", nil},
				{true, "", nil},
				{true, "", nil},
			},
			expected:  true,
			expReason: "",
		},
		{
			name: "第一个失败",
			results: []struct {
				available bool
				reason    string
				err       error
			}{
				{false, "first failed", nil},
				{true, "", nil}, // 不应该执行
			},
			expected:  false,
			expReason: "first failed",
		},
		{
			name: "第一个错误,第二个失败",
			results: []struct {
				available bool
				reason    string
				err       error
			}{
				{false, "", errors.New("network error")}, // 错误,继续
				{false, "second failed", nil},            // 失败,中断
			},
			expected:  false,
			expReason: "second failed",
		},
		{
			name: "所有错误",
			results: []struct {
				available bool
				reason    string
				err       error
			}{
				{false, "", errors.New("error1")},
				{false, "", errors.New("error2")},
				{false, "", errors.New("error3")},
			},
			expected:  true, // 所有错误时防御性策略允许通过
			expReason: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := NewManager()

			for _, r := range tt.results {
				result := r // 捕获循环变量
				mgr.AddValidator(&mockValidator{
					validateFunc: func(ctx context.Context, cfg *model.Config, apiKey string) (bool, string, error) {
						return result.available, result.reason, result.err
					},
				})
			}

			cfg := &model.Config{Name: "test-channel"}
			available, reason := mgr.ValidateChannel(context.Background(), cfg, "test-key")

			if available != tt.expected {
				t.Errorf("ValidateChannel() available = %v, expected %v", available, tt.expected)
			}
			if reason != tt.expReason {
				t.Errorf("ValidateChannel() reason = %q, expected %q", reason, tt.expReason)
			}
		})
	}
}
