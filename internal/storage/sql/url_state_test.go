package sql_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func TestCleanupOrphanedURLStates(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "test_url_state.db"))
	if err != nil {
		t.Fatalf("创建存储失败: %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now()

	// 创建测试渠道
	channel := &model.Config{
		ID:        1,
		Name:      "test-channel",
		URL:       "https://api1.example.com,https://api2.example.com,https://api3.example.com",
		Enabled:   true,
		CreatedAt: model.JSONTime{Time: now},
		UpdatedAt: model.JSONTime{Time: now},
	}
	_, err = store.CreateConfig(ctx, channel)
	if err != nil {
		t.Fatalf("创建渠道失败: %v", err)
	}

	// 插入3条URL禁用状态记录
	urls := []string{
		"https://api1.example.com",
		"https://api2.example.com",
		"https://api3.example.com",
	}
	for _, url := range urls {
		if err := store.SetURLDisabled(ctx, channel.ID, url, true); err != nil {
			t.Fatalf("插入URL状态失败: %v", err)
		}
	}

	t.Run("场景1：移除部分URL", func(t *testing.T) {
		// keepURLs只保留api1，清理api2和api3
		keepURLs := []string{"https://api1.example.com"}

		if err := store.CleanupOrphanedURLStates(ctx, channel.ID, keepURLs); err != nil {
			t.Fatalf("清理失败: %v", err)
		}

		// 验证：只保留api1记录
		disabledURLs, err := store.LoadDisabledURLs(ctx)
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}

		channelURLs := disabledURLs[channel.ID]
		if len(channelURLs) != 1 {
			t.Errorf("期望保留1条记录，实际=%d", len(channelURLs))
		}
		if len(channelURLs) > 0 && channelURLs[0] != "https://api1.example.com" {
			t.Errorf("期望保留api1，实际=%s", channelURLs[0])
		}
	})

	t.Run("场景2：空列表清理全部", func(t *testing.T) {
		// 先恢复3条记录
		for _, url := range urls {
			if err := store.SetURLDisabled(ctx, channel.ID, url, true); err != nil {
				t.Fatalf("恢复记录失败: %v", err)
			}
		}

		// keepURLs为空，清理全部
		keepURLs := []string{}

		if err := store.CleanupOrphanedURLStates(ctx, channel.ID, keepURLs); err != nil {
			t.Fatalf("清理失败: %v", err)
		}

		// 验证：无记录残留
		disabledURLs, err := store.LoadDisabledURLs(ctx)
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}

		channelURLs := disabledURLs[channel.ID]
		if len(channelURLs) != 0 {
			t.Errorf("期望清理全部记录，实际残留=%d", len(channelURLs))
		}
	})

	t.Run("场景3：无变化", func(t *testing.T) {
		// 先恢复2条记录
		if err := store.SetURLDisabled(ctx, channel.ID, "https://api1.example.com", true); err != nil {
			t.Fatalf("恢复记录失败: %v", err)
		}
		if err := store.SetURLDisabled(ctx, channel.ID, "https://api2.example.com", true); err != nil {
			t.Fatalf("恢复记录失败: %v", err)
		}

		// keepURLs包含全部URL，无清理
		keepURLs := []string{"https://api1.example.com", "https://api2.example.com"}

		if err := store.CleanupOrphanedURLStates(ctx, channel.ID, keepURLs); err != nil {
			t.Fatalf("清理失败: %v", err)
		}

		// 验证：2条记录仍然存在
		disabledURLs, err := store.LoadDisabledURLs(ctx)
		if err != nil {
			t.Fatalf("查询失败: %v", err)
		}

		channelURLs := disabledURLs[channel.ID]
		if len(channelURLs) != 2 {
			t.Errorf("期望保留2条记录，实际=%d", len(channelURLs))
		}
	})

	t.Run("场景4：不存在记录", func(t *testing.T) {
		// 清理不存在记录的渠道（清理操作不影响）
		nonExistentChannelID := int64(999)
		keepURLs := []string{"https://api.example.com"}

		if err := store.CleanupOrphanedURLStates(ctx, nonExistentChannelID, keepURLs); err != nil {
			t.Fatalf("清理不存在渠道应该成功: %v", err)
		}
	})
}
