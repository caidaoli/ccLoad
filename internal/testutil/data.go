package testutil

import (
	"context"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

// CreateTestChannel 创建测试渠道
func CreateTestChannel(t testing.TB, store storage.Store, name string) *model.Config {
	t.Helper()

	cfg := &model.Config{
		Name:     name,
		URL:      "https://api.example.com",
		Priority: 10,
		ModelEntries: []model.ModelEntry{
			{Model: "test-model", RedirectModel: ""},
		},
		Enabled:     true,
		ChannelType: "anthropic",
	}

	created, err := store.CreateConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	return created
}

// CreateTestChannelWithType 创建指定类型的测试渠道
func CreateTestChannelWithType(t testing.TB, store storage.Store, name, channelType string, models []string) *model.Config {
	t.Helper()

	entries := make([]model.ModelEntry, len(models))
	for i, m := range models {
		entries[i] = model.ModelEntry{Model: m}
	}

	cfg := &model.Config{
		Name:         name,
		URL:          "https://api.example.com",
		Priority:     10,
		ModelEntries: entries,
		Enabled:      true,
		ChannelType:  channelType,
	}

	created, err := store.CreateConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("创建测试渠道失败: %v", err)
	}

	return created
}

// CreateTestAPIKey 创建测试 API Key
func CreateTestAPIKey(t testing.TB, store storage.Store, channelID int64, keyIndex int) {
	t.Helper()

	now := time.Now()
	if err := store.CreateAPIKeysBatch(context.Background(), []*model.APIKey{
		{
			ChannelID:   channelID,
			KeyIndex:    keyIndex,
			APIKey:      "sk-test-key",
			KeyStrategy: model.KeyStrategySequential,
			CreatedAt:   model.JSONTime{Time: now},
			UpdatedAt:   model.JSONTime{Time: now},
		},
	}); err != nil {
		t.Fatalf("创建测试 API Key 失败: %v", err)
	}
}

// CreateTestAPIKeys 批量创建测试 API Key
func CreateTestAPIKeys(t testing.TB, store storage.Store, channelID int64, count int) {
	t.Helper()

	now := time.Now()
	keys := make([]*model.APIKey, count)
	for i := range count {
		keys[i] = &model.APIKey{
			ChannelID:   channelID,
			KeyIndex:    i,
			APIKey:      "sk-test-key-" + string(rune('0'+i)),
			KeyStrategy: model.KeyStrategySequential,
			CreatedAt:   model.JSONTime{Time: now},
			UpdatedAt:   model.JSONTime{Time: now},
		}
	}

	if err := store.CreateAPIKeysBatch(context.Background(), keys); err != nil {
		t.Fatalf("批量创建测试 API Key 失败: %v", err)
	}
}

// CountAPIKeys 计算所有渠道的 API Key 总数
func CountAPIKeys(allKeys map[int64][]*model.APIKey) int {
	total := 0
	for _, keys := range allKeys {
		total += len(keys)
	}
	return total
}

// CreateTestAuthToken 创建测试 Auth Token
func CreateTestAuthToken(t testing.TB, store storage.Store, token string) *model.AuthToken {
	t.Helper()

	authToken := &model.AuthToken{
		Token:       token,
		Description: "test-token",
		IsActive:    true,
	}

	if err := store.CreateAuthToken(context.Background(), authToken); err != nil {
		t.Fatalf("创建测试 Auth Token 失败: %v", err)
	}

	return authToken
}
