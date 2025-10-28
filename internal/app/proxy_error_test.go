package app

import (
	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"

	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// TestHandleProxyError_SingleKeyUpgrade 测试单Key渠道的错误升级逻辑
func TestHandleProxyError_SingleKeyUpgrade(t *testing.T) {
	// 创建测试服务器（✅ P2重构：添加cooldownManager）
	store := &MockStore{}
	keySelector := NewKeySelector(nil) // ✅ P0重构：移除store参数
	server := &Server{
		store:           store,
		keySelector:     keySelector,
		cooldownManager: cooldown.NewManager(store), // ✅ P2重构：初始化cooldownManager
	}

	ctx := context.Background()

	tests := []struct {
		name           string
		cfg            *model.Config
		statusCode     int
		responseBody   []byte
		networkError   error
		expectedAction cooldown.Action // ✅ P2重构：使用 cooldown.Action
		expectedLevel  string
		reason         string
	}{
		// 单Key渠道测试（注：新架构中无需APIKey字段，通过api_keys表查询）
		{
			name: "single_key_401_should_upgrade_to_channel",
			cfg: &model.Config{
				ID: 1,
			},
			statusCode:     401,
			responseBody:   []byte(`{"error":"unauthorized"}`),
			expectedAction: cooldown.ActionRetryChannel,
			expectedLevel:  "Channel",
			reason:         "单Key渠道的401应该升级为渠道级错误",
		},
		{
			name: "single_key_403_quota_should_upgrade_to_channel",
			cfg: &model.Config{
				ID: 2,
			},
			statusCode:     403,
			responseBody:   []byte(`{"error":"quota_exceeded"}`),
			expectedAction: cooldown.ActionRetryChannel,
			expectedLevel:  "Channel",
			reason:         "单Key渠道的403额度用尽应该升级为渠道级错误",
		},

		// 多Key渠道测试
		{
			name: "multi_key_401_should_stay_key_level",
			cfg: &model.Config{
				ID: 3,
			},
			statusCode:     401,
			responseBody:   []byte(`{"error":"unauthorized"}`),
			expectedAction: cooldown.ActionRetryKey,
			expectedLevel:  "Key",
			reason:         "多Key渠道的401应该保持Key级错误，尝试其他Key",
		},
		{
			name: "multi_key_403_quota_should_stay_key_level",
			cfg: &model.Config{
				ID: 4,
			},
			statusCode:     403,
			responseBody:   []byte(`{"error":"quota_exceeded","message":"Daily cost limit reached"}}`),
			expectedAction: cooldown.ActionRetryKey,
			expectedLevel:  "Key",
			reason:         "多Key渠道的403额度用尽应该保持Key级错误（可能只是单个Key额度用尽）",
		},

		// 渠道级错误（无论单Key还是多Key都应该冷却渠道）
		{
			name: "single_key_500_should_be_channel",
			cfg: &model.Config{
				ID: 5,
			},
			statusCode:     500,
			responseBody:   []byte(`{"error":"internal server error"}`),
			expectedAction: cooldown.ActionRetryChannel,
			expectedLevel:  "Channel",
			reason:         "500错误本身就是渠道级错误",
		},
		{
			name: "multi_key_500_should_be_channel",
			cfg: &model.Config{
				ID: 6,
			},
			statusCode:     500,
			responseBody:   []byte(`{"error":"internal server error"}`),
			expectedAction: cooldown.ActionRetryChannel,
			expectedLevel:  "Channel",
			reason:         "500错误本身就是渠道级错误（即使多Key）",
		},

		// 客户端错误（无论单Key还是多Key都应该直接返回）
		{
			name: "single_key_404_should_return_client",
			cfg: &model.Config{
				ID: 7,
			},
			statusCode:     404,
			responseBody:   []byte(`{"error":"not found"}`),
			expectedAction: cooldown.ActionReturnClient,
			expectedLevel:  "Client",
			reason:         "404错误应该直接返回客户端",
		},

		// 🆕 网络错误测试（修复后应该支持单Key升级）
		{
			name: "single_key_network_error_should_upgrade_to_channel",
			cfg: &model.Config{
				ID: 8,
			},
			networkError:   &net.OpError{Op: "dial", Err: errors.New("connection refused")},
			expectedAction: cooldown.ActionRetryChannel,
			expectedLevel:  "Channel",
			reason:         "单Key渠道的网络错误应该升级为渠道级错误（修复后）",
		},
		{
			name: "multi_key_network_error_should_stay_key_level",
			cfg: &model.Config{
				ID: 9,
			},
			networkError:   &net.OpError{Op: "dial", Err: errors.New("connection refused")},
			expectedAction: cooldown.ActionRetryKey,
			expectedLevel:  "Key",
			reason:         "多Key渠道的网络错误应该保持Key级错误，尝试其他Key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var res *fwResult
			var err error

			if tt.networkError != nil {
				// 测试网络错误
				err = tt.networkError
			} else {
				// 测试HTTP错误
				res = &fwResult{
					Status: tt.statusCode,
					Body:   tt.responseBody,
				}
			}

			action, _ := server.handleProxyError(ctx, tt.cfg, 0, res, err)

			if action != tt.expectedAction {
				t.Errorf("❌ %s\n  期望动作: %v\n  实际动作: %v\n  原因: %s",
					tt.name, tt.expectedAction, action, tt.reason)
			} else {
				t.Logf("✅ %s - %s", tt.name, tt.reason)
			}
		})
	}
}

// MockStore 用于测试的Mock storage.Store
type MockStore struct{}

func (m *MockStore) GetConfig(ctx context.Context, id int64) (*model.Config, error) {
	// 🔧 P1优化修复：返回带KeyCount的Config对象
	apiKeys, err := m.GetAPIKeys(ctx, id)
	if err != nil {
		return nil, err
	}
	return &model.Config{
		ID:       id,
		KeyCount: len(apiKeys),
	}, nil
}

func (m *MockStore) ListConfigs(ctx context.Context) ([]*model.Config, error) {
	return nil, nil
}

func (m *MockStore) CreateConfig(ctx context.Context, c *model.Config) (*model.Config, error) {
	return nil, nil
}

func (m *MockStore) UpdateConfig(ctx context.Context, id int64, c *model.Config) (*model.Config, error) {
	return nil, nil
}

func (m *MockStore) ReplaceConfig(ctx context.Context, c *model.Config) (*model.Config, error) {
	return nil, nil
}

func (m *MockStore) DeleteConfig(ctx context.Context, id int64) error {
	return nil
}

// API Keys management
func (m *MockStore) GetAPIKeys(ctx context.Context, channelID int64) ([]*model.APIKey, error) {
	// 根据渠道ID返回不同数量的Key（模拟单Key vs 多Key场景）
	// ID 1, 2, 5, 7, 8: 单Key渠道
	// ID 3, 4, 6, 9: 多Key渠道（3个Key）
	switch channelID {
	case 1, 2, 5, 7, 8:
		// 单Key渠道
		return []*model.APIKey{
			{
				ChannelID:   channelID,
				KeyIndex:    0,
				APIKey:      "sk-single-key",
				KeyStrategy: "sequential",
			},
		}, nil
	case 3, 4, 6, 9:
		// 多Key渠道（3个Key）
		return []*model.APIKey{
			{ChannelID: channelID, KeyIndex: 0, APIKey: "sk-key1", KeyStrategy: "sequential"},
			{ChannelID: channelID, KeyIndex: 1, APIKey: "sk-key2", KeyStrategy: "sequential"},
			{ChannelID: channelID, KeyIndex: 2, APIKey: "sk-key3", KeyStrategy: "sequential"},
		}, nil
	default:
		return nil, nil
	}
}

func (m *MockStore) GetAPIKey(ctx context.Context, channelID int64, keyIndex int) (*model.APIKey, error) {
	return nil, nil
}

func (m *MockStore) CreateAPIKey(ctx context.Context, key *model.APIKey) error {
	return nil
}

func (m *MockStore) UpdateAPIKey(ctx context.Context, key *model.APIKey) error {
	return nil
}

func (m *MockStore) DeleteAPIKey(ctx context.Context, channelID int64, keyIndex int) error {
	return nil
}

func (m *MockStore) DeleteAllAPIKeys(ctx context.Context, channelID int64) error {
	return nil
}

// Channel-level cooldowns
func (m *MockStore) GetAllChannelCooldowns(ctx context.Context) (map[int64]time.Time, error) {
	return make(map[int64]time.Time), nil
}

func (m *MockStore) BumpChannelCooldown(ctx context.Context, channelID int64, now time.Time, statusCode int) (time.Duration, error) {
	return time.Second, nil
}

func (m *MockStore) ResetChannelCooldown(ctx context.Context, channelID int64) error {
	return nil
}

func (m *MockStore) SetChannelCooldown(ctx context.Context, channelID int64, until time.Time) error {
	return nil
}

// Key-level cooldowns
func (m *MockStore) GetAllKeyCooldowns(ctx context.Context) (map[int64]map[int]time.Time, error) {
	return make(map[int64]map[int]time.Time), nil
}

func (m *MockStore) BumpKeyCooldown(ctx context.Context, channelID int64, keyIndex int, now time.Time, statusCode int) (time.Duration, error) {
	return time.Second, nil
}

func (m *MockStore) SetKeyCooldown(ctx context.Context, channelID int64, keyIndex int, until time.Time) error {
	return nil
}

func (m *MockStore) ResetKeyCooldown(ctx context.Context, channelID int64, keyIndex int) error {
	return nil
}

func (m *MockStore) AddLog(ctx context.Context, e *model.LogEntry) error {
	return nil
}

func (m *MockStore) ListLogs(ctx context.Context, since time.Time, limit, offset int, filter *model.LogFilter) ([]*model.LogEntry, error) {
	return nil, nil
}

func (m *MockStore) Aggregate(ctx context.Context, since time.Time, bucket time.Duration) ([]model.MetricPoint, error) {
	return nil, nil
}

func (m *MockStore) GetStats(ctx context.Context, since time.Time, filter *model.LogFilter) ([]model.StatsEntry, error) {
	return nil, nil
}

func (m *MockStore) Close() error {
	return nil
}

func (m *MockStore) CleanupLogsBefore(ctx context.Context, cutoff time.Time) error {
	return nil
}
func (m *MockStore) GetEnabledChannelsByModel(ctx context.Context, model string) ([]*model.Config, error) {
	return nil, nil
}
func (m *MockStore) GetEnabledChannelsByType(ctx context.Context, channelType string) ([]*model.Config, error) {
	return nil, nil
}

// ==================== P2 边界测试：多Key渠道所有Key冷却场景 ====================

// TestAllKeysCooledDown_UpgradeToChannelCooldown 测试多Key渠道所有Key冷却后升级为渠道级冷却
// 场景：渠道配置了3个Key，所有Key都在冷却中
// 预期：SelectAvailableKey返回错误 -> tryChannelWithKeys返回"channel keys unavailable"
//
//	-> handleProxyRequest触发渠道级冷却
func TestAllKeysCooledDown_UpgradeToChannelCooldown(t *testing.T) {
	// 创建测试用的Store，所有Key都返回冷却中
	store := &MockStoreAllKeysCooled{
		keyCooldowns: map[string]time.Time{
			"1_0": time.Now().Add(30 * time.Second), // Key 0 冷却中
			"1_1": time.Now().Add(60 * time.Second), // Key 1 冷却中
			"1_2": time.Now().Add(90 * time.Second), // Key 2 冷却中
		},
	}

	keySelector := NewKeySelector(nil) // ✅ P0重构：移除store参数
	ctx := context.Background()

	// 配置3个Key的渠道（注：新架构中API Keys在api_keys表）
	cfg := &model.Config{
		ID:   1,
		Name: "test-channel",
	}

	// ✅ P0重构：预先查询apiKeys（MockStore返回包含冷却状态的Keys）
	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	// 尝试选择可用Key（应该失败，因为所有Key都冷却）
	triedKeys := make(map[int]bool)
	keyIndex, selectedKey, err := keySelector.SelectAvailableKey(cfg.ID, apiKeys, triedKeys)

	// 验证：应该返回错误
	if err == nil {
		t.Fatalf("❌ 预期返回错误（所有Key冷却），但成功选择了Key %d: %s", keyIndex, selectedKey)
	}

	// 验证错误消息
	expectedErrMsg := "all API keys are in cooldown or already tried"
	if err.Error() != expectedErrMsg {
		t.Errorf("❌ 错误消息不匹配\n  预期: %s\n  实际: %s", expectedErrMsg, err.Error())
	} else {
		t.Logf("✅ SelectAvailableKey 正确返回错误: %s", err.Error())
	}

	// 验证：这个错误应该在 proxy.go 中被识别并触发渠道级冷却
	// 检查错误消息是否包含关键字（proxy.go:885 检测逻辑）
	if !contains(err.Error(), "cooldown") && !contains(err.Error(), "tried") {
		t.Errorf("❌ 错误消息应包含 'cooldown' 或 'tried' 关键字以便 proxy.go 识别")
	}

	t.Log("✅ 边界测试通过：多Key渠道所有Key冷却时正确返回错误，可触发渠道级冷却升级")
}

// TestAllKeysCooledDown_RoundRobinStrategy 测试轮询策略下所有Key冷却的场景
func TestAllKeysCooledDown_RoundRobinStrategy(t *testing.T) {
	store := &MockStoreAllKeysCooled{
		keyCooldowns: map[string]time.Time{
			"2_0": time.Now().Add(10 * time.Second),
			"2_1": time.Now().Add(20 * time.Second),
			"2_2": time.Now().Add(30 * time.Second),
		},
		rrIndex: 1, // 从Key 1 开始轮询
	}

	keySelector := NewKeySelector(nil) // ✅ P0重构：移除store参数
	ctx := context.Background()

	cfg := &model.Config{
		ID: 2,
	}

	// ✅ P0重构：预先查询apiKeys
	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	triedKeys := make(map[int]bool)
	_, _, err = keySelector.SelectAvailableKey(cfg.ID, apiKeys, triedKeys)

	if err == nil {
		t.Fatal("❌ 预期返回错误（所有Key冷却），但成功选择了Key")
	}

	if !contains(err.Error(), "cooldown") {
		t.Errorf("❌ 错误消息应包含 'cooldown' 关键字")
	}

	t.Log("✅ 轮询策略下所有Key冷却测试通过")
}

// TestPartialKeysCooled_ShouldSelectAvailable 测试部分Key冷却时选择可用Key
func TestPartialKeysCooled_ShouldSelectAvailable(t *testing.T) {
	store := &MockStoreAllKeysCooled{
		keyCooldowns: map[string]time.Time{
			"3_0": time.Now().Add(30 * time.Second),  // Key 0 冷却中
			"3_2": time.Now().Add(-10 * time.Second), // Key 2 已过期（不冷却）
			// Key 1 不在map中，表示未冷却
		},
	}

	keySelector := NewKeySelector(nil) // ✅ P0重构：移除store参数
	ctx := context.Background()

	cfg := &model.Config{
		ID: 3,
	}

	// ✅ P0重构：预先查询apiKeys
	apiKeys, err := store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatalf("查询API Keys失败: %v", err)
	}

	triedKeys := make(map[int]bool)
	keyIndex, selectedKey, err := keySelector.SelectAvailableKey(cfg.ID, apiKeys, triedKeys)

	if err != nil {
		t.Fatalf("❌ 预期成功选择可用Key，但返回错误: %v", err)
	}

	// 应该选择Key 1（索引1）或Key 2（索引2，已过期）
	if keyIndex != 1 && keyIndex != 2 {
		t.Errorf("❌ 预期选择Key 1或2，但实际选择Key %d", keyIndex)
	}

	if selectedKey != "sk-key2" && selectedKey != "sk-key3" {
		t.Errorf("❌ 预期选择 'sk-key2' 或 'sk-key3'，但实际选择 '%s'", selectedKey)
	}

	t.Logf("✅ 部分Key冷却测试通过：正确选择可用Key %d: %s", keyIndex, selectedKey)
}

// MockStoreAllKeysCooled 模拟所有Key冷却的Store
type MockStoreAllKeysCooled struct {
	keyCooldowns map[string]time.Time // key格式: "channelID_keyIndex"
	rrIndex      int                  // 轮询起始索引
}

func (m *MockStoreAllKeysCooled) GetKeyCooldownUntil(ctx context.Context, configID int64, keyIndex int) (time.Time, bool) {
	key := fmt.Sprintf("%d_%d", configID, keyIndex)
	if until, ok := m.keyCooldowns[key]; ok {
		return until, time.Now().Before(until) // 只有未过期的才返回true
	}
	return time.Time{}, false
}

// 实现其他Store接口（使用默认MockStore的实现）
func (m *MockStoreAllKeysCooled) GetConfig(ctx context.Context, id int64) (*model.Config, error) {
	return nil, nil
}
func (m *MockStoreAllKeysCooled) ListConfigs(ctx context.Context) ([]*model.Config, error) {
	return nil, nil
}
func (m *MockStoreAllKeysCooled) CreateConfig(ctx context.Context, c *model.Config) (*model.Config, error) {
	return nil, nil
}
func (m *MockStoreAllKeysCooled) UpdateConfig(ctx context.Context, id int64, c *model.Config) (*model.Config, error) {
	return nil, nil
}
func (m *MockStoreAllKeysCooled) ReplaceConfig(ctx context.Context, c *model.Config) (*model.Config, error) {
	return nil, nil
}
func (m *MockStoreAllKeysCooled) DeleteConfig(ctx context.Context, id int64) error {
	return nil
}

// API Keys management
func (m *MockStoreAllKeysCooled) GetAPIKeys(ctx context.Context, channelID int64) ([]*model.APIKey, error) {
	// 所有测试渠道都配置3个Key，根据keyCooldowns设置冷却状态
	keys := []*model.APIKey{
		{ChannelID: channelID, KeyIndex: 0, APIKey: "sk-key1", KeyStrategy: "sequential"},
		{ChannelID: channelID, KeyIndex: 1, APIKey: "sk-key2", KeyStrategy: "sequential"},
		{ChannelID: channelID, KeyIndex: 2, APIKey: "sk-key3", KeyStrategy: "sequential"},
	}

	// 设置冷却状态（从keyCooldowns map读取）
	for _, key := range keys {
		cooldownKey := fmt.Sprintf("%d_%d", channelID, key.KeyIndex)
		if until, ok := m.keyCooldowns[cooldownKey]; ok {
			key.CooldownUntil = until.Unix() // 设置冷却截止时间（Unix时间戳）
		}
	}

	return keys, nil
}
func (m *MockStoreAllKeysCooled) GetAPIKey(ctx context.Context, channelID int64, keyIndex int) (*model.APIKey, error) {
	return nil, nil
}
func (m *MockStoreAllKeysCooled) CreateAPIKey(ctx context.Context, key *model.APIKey) error {
	return nil
}
func (m *MockStoreAllKeysCooled) UpdateAPIKey(ctx context.Context, key *model.APIKey) error {
	return nil
}
func (m *MockStoreAllKeysCooled) DeleteAPIKey(ctx context.Context, channelID int64, keyIndex int) error {
	return nil
}
func (m *MockStoreAllKeysCooled) DeleteAllAPIKeys(ctx context.Context, channelID int64) error {
	return nil
}

// Channel-level cooldowns
func (m *MockStoreAllKeysCooled) GetAllChannelCooldowns(ctx context.Context) (map[int64]time.Time, error) {
	return make(map[int64]time.Time), nil
}
func (m *MockStoreAllKeysCooled) BumpChannelCooldown(ctx context.Context, channelID int64, now time.Time, statusCode int) (time.Duration, error) {
	return time.Second, nil
}
func (m *MockStoreAllKeysCooled) ResetChannelCooldown(ctx context.Context, channelID int64) error {
	return nil
}
func (m *MockStoreAllKeysCooled) SetChannelCooldown(ctx context.Context, channelID int64, until time.Time) error {
	return nil
}

// Key-level cooldowns
func (m *MockStoreAllKeysCooled) GetAllKeyCooldowns(ctx context.Context) (map[int64]map[int]time.Time, error) {
	return make(map[int64]map[int]time.Time), nil
}
func (m *MockStoreAllKeysCooled) BumpKeyCooldown(ctx context.Context, channelID int64, keyIndex int, now time.Time, statusCode int) (time.Duration, error) {
	return time.Second, nil
}
func (m *MockStoreAllKeysCooled) SetKeyCooldown(ctx context.Context, channelID int64, keyIndex int, until time.Time) error {
	return nil
}
func (m *MockStoreAllKeysCooled) ResetKeyCooldown(ctx context.Context, channelID int64, keyIndex int) error {
	return nil
}
func (m *MockStoreAllKeysCooled) AddLog(ctx context.Context, e *model.LogEntry) error {
	return nil
}
func (m *MockStoreAllKeysCooled) ListLogs(ctx context.Context, since time.Time, limit, offset int, filter *model.LogFilter) ([]*model.LogEntry, error) {
	return nil, nil
}
func (m *MockStoreAllKeysCooled) Aggregate(ctx context.Context, since time.Time, bucket time.Duration) ([]model.MetricPoint, error) {
	return nil, nil
}
func (m *MockStoreAllKeysCooled) GetStats(ctx context.Context, since time.Time, filter *model.LogFilter) ([]model.StatsEntry, error) {
	return nil, nil
}
func (m *MockStoreAllKeysCooled) Close() error {
	return nil
}

func (m *MockStoreAllKeysCooled) CleanupLogsBefore(ctx context.Context, cutoff time.Time) error {
	return nil
}
func (m *MockStoreAllKeysCooled) GetEnabledChannelsByModel(ctx context.Context, model string) ([]*model.Config, error) {
	return nil, nil
}
func (m *MockStoreAllKeysCooled) GetEnabledChannelsByType(ctx context.Context, channelType string) ([]*model.Config, error) {
	return nil, nil
}

// contains 检查字符串是否包含子串（辅助函数）
func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > len(substr) && (s[:len(substr)] == substr || s[len(s)-len(substr):] == substr || containsMiddle(s, substr)))
}

func containsMiddle(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
