package sql_test

import (
	"context"
	"strconv"
	"testing"

	"ccLoad/internal/model"
)

func TestSystemSettings_GetSetting(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "settings.db")

	ctx := context.Background()

	// 测试获取存在的设置项（数据库迁移会插入默认值）
	setting, err := store.GetSetting(ctx, "log_retention_days")
	if err != nil {
		t.Fatalf("get existing setting: %v", err)
	}
	if setting.Key != "log_retention_days" {
		t.Errorf("unexpected key: got %q, want %q", setting.Key, "log_retention_days")
	}
	if setting.Value != "7" {
		t.Errorf("unexpected value: got %q, want %q", setting.Value, "7")
	}
	if setting.ValueType != "int" {
		t.Errorf("unexpected value_type: got %q, want %q", setting.ValueType, "int")
	}
	if v, err := strconv.Atoi(setting.Value); err != nil || v <= 0 {
		t.Errorf("unexpected value: got %q, want positive int", setting.Value)
	}

	// 测试获取不存在的设置项
	_, err = store.GetSetting(ctx, "non_existent_key")
	if err != model.ErrSettingNotFound {
		t.Errorf("expected ErrSettingNotFound, got: %v", err)
	}
}

func TestSystemSettings_ListAllSettings(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "settings.db")

	ctx := context.Background()

	// 获取所有设置项
	settings, err := store.ListAllSettings(ctx)
	if err != nil {
		t.Fatalf("list all settings: %v", err)
	}

	// 验证结果按 key 排序
	for i := 1; i < len(settings); i++ {
		if settings[i-1].Key > settings[i].Key {
			t.Errorf("settings not sorted: %q > %q", settings[i-1].Key, settings[i].Key)
		}
	}

	// 验证包含已知的默认设置 key（但不把具体默认值当成稳定契约）
	found := false
	for _, s := range settings {
		if s.Key == "max_key_retries" {
			found = true
			if s.ValueType != "int" {
				t.Errorf("unexpected max_key_retries value_type: got %q, want %q", s.ValueType, "int")
			}
			if v, err := strconv.Atoi(s.Value); err != nil || v <= 0 {
				t.Errorf("unexpected max_key_retries value: got %q, want positive int", s.Value)
			}
			break
		}
	}
	if !found {
		t.Error("expected to find max_key_retries in settings")
	}
}

func TestSystemSettings_UpdateSetting(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "settings.db")

	ctx := context.Background()

	// 更新存在的设置项
	if err := store.UpdateSetting(ctx, "log_retention_days", "30"); err != nil {
		t.Fatalf("update existing setting: %v", err)
	}

	// 验证更新成功
	setting, err := store.GetSetting(ctx, "log_retention_days")
	if err != nil {
		t.Fatalf("get updated setting: %v", err)
	}
	if setting.Value != "30" {
		t.Errorf("unexpected value after update: got %q, want %q", setting.Value, "30")
	}
	if setting.UpdatedAt == 0 {
		t.Error("expected updated_at to be set")
	}

	// 更新不存在的设置项
	err = store.UpdateSetting(ctx, "non_existent_key", "value")
	if err != model.ErrSettingNotFound {
		t.Errorf("expected ErrSettingNotFound, got: %v", err)
	}
}

func TestSystemSettings_BatchUpdateSettings(t *testing.T) {
	t.Parallel()

	store := newTestStore(t, "settings.db")

	ctx := context.Background()

	// 批量更新多个设置项
	updates := map[string]string{
		"log_retention_days": "14",
		"max_key_retries":    "5",
	}
	if err := store.BatchUpdateSettings(ctx, updates); err != nil {
		t.Fatalf("batch update settings: %v", err)
	}

	// 验证更新成功
	setting1, err := store.GetSetting(ctx, "log_retention_days")
	if err != nil {
		t.Fatalf("get log_retention_days: %v", err)
	}
	if setting1.Value != "14" {
		t.Errorf("log_retention_days: got %q, want %q", setting1.Value, "14")
	}

	setting2, err := store.GetSetting(ctx, "max_key_retries")
	if err != nil {
		t.Fatalf("get max_key_retries: %v", err)
	}
	if setting2.Value != "5" {
		t.Errorf("max_key_retries: got %q, want %q", setting2.Value, "5")
	}

	// 批量更新包含不存在的 key 时应回滚
	badUpdates := map[string]string{
		"log_retention_days": "100",
		"non_existent_key":   "value",
	}
	err = store.BatchUpdateSettings(ctx, badUpdates)
	if err == nil {
		t.Error("expected error for non-existent key in batch update")
	}

	// 验证事务回滚：log_retention_days 应保持原值
	setting1, err = store.GetSetting(ctx, "log_retention_days")
	if err != nil {
		t.Fatalf("get log_retention_days after rollback: %v", err)
	}
	if setting1.Value != "14" {
		t.Errorf("log_retention_days should be unchanged after rollback: got %q, want %q", setting1.Value, "14")
	}
}
