package app

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

// ConfigService 配置管理服务
// 职责: 启动时从数据库加载配置，提供只读访问
// 配置修改后程序会自动重启，无需热重载
type ConfigService struct {
	store  storage.Store
	cache  map[string]*model.SystemSetting // 启动时加载，运行期间只读
	loaded bool
}

// NewConfigService 创建配置服务
func NewConfigService(store storage.Store) *ConfigService {
	return &ConfigService{
		store: store,
		cache: make(map[string]*model.SystemSetting),
	}
}

// LoadDefaults 启动时从数据库加载配置到内存（只调用一次）
func (cs *ConfigService) LoadDefaults(ctx context.Context) error {
	if cs.loaded {
		return nil
	}

	settings, err := cs.store.ListAllSettings(ctx)
	if err != nil {
		return fmt.Errorf("load settings from db: %w", err)
	}

	for _, s := range settings {
		cs.cache[s.Key] = s
	}
	cs.loaded = true

	log.Printf("[INFO] ConfigService loaded %d settings", len(settings))
	return nil
}

// GetInt 获取整数配置
func (cs *ConfigService) GetInt(key string, defaultValue int) int {
	if setting, ok := cs.cache[key]; ok {
		if intVal, err := strconv.Atoi(setting.Value); err == nil {
			return intVal
		}
	}
	return defaultValue
}

// GetBool 获取布尔配置
func (cs *ConfigService) GetBool(key string, defaultValue bool) bool {
	if setting, ok := cs.cache[key]; ok {
		return setting.Value == "true" || setting.Value == "1"
	}
	return defaultValue
}

// GetString 获取字符串配置
func (cs *ConfigService) GetString(key string, defaultValue string) string {
	if setting, ok := cs.cache[key]; ok {
		return setting.Value
	}
	return defaultValue
}

// GetFloat 获取浮点数配置
func (cs *ConfigService) GetFloat(key string, defaultValue float64) float64 {
	if setting, ok := cs.cache[key]; ok {
		if floatVal, err := strconv.ParseFloat(setting.Value, 64); err == nil {
			return floatVal
		}
	}
	return defaultValue
}

// GetDuration 获取时长配置(秒转Duration)
func (cs *ConfigService) GetDuration(key string, defaultValue time.Duration) time.Duration {
	seconds := cs.GetInt(key, int(defaultValue.Seconds()))
	return time.Duration(seconds) * time.Second
}

// GetIntMin 获取整数配置（带最小值约束）
// 如果值小于 min，记录警告并返回 defaultValue
func (cs *ConfigService) GetIntMin(key string, defaultValue, min int) int {
	val := cs.GetInt(key, defaultValue)
	if val < min {
		log.Printf("[WARN] 无效的 %s=%d（必须 >= %d），已使用默认值 %d", key, val, min, defaultValue)
		return defaultValue
	}
	return val
}

// GetDurationNonNegative 获取非负时长配置
// 如果值为负，记录警告并返回 0（禁用）
func (cs *ConfigService) GetDurationNonNegative(key string, defaultValue time.Duration) time.Duration {
	val := cs.GetDuration(key, defaultValue)
	if val < 0 {
		log.Printf("[WARN] 无效的 %s=%v（必须 >= 0），已设为 0（禁用）", key, val)
		return 0
	}
	return val
}

// GetDurationPositive 获取正时长配置
// 如果值 <= 0，记录警告并返回 defaultValue
func (cs *ConfigService) GetDurationPositive(key string, defaultValue time.Duration) time.Duration {
	val := cs.GetDuration(key, defaultValue)
	if val <= 0 {
		log.Printf("[WARN] 无效的 %s=%v（必须 > 0），已使用默认值 %v", key, val, defaultValue)
		return defaultValue
	}
	return val
}

// GetSetting 获取完整配置对象（用于验证等场景）
func (cs *ConfigService) GetSetting(key string) *model.SystemSetting {
	return cs.cache[key]
}

// UpdateSetting 更新配置（仅写数据库，不更新缓存，因为会重启）
func (cs *ConfigService) UpdateSetting(ctx context.Context, key, value string) error {
	return cs.store.UpdateSetting(ctx, key, value)
}

// ListAllSettings 获取所有配置(用于前端展示)
func (cs *ConfigService) ListAllSettings(ctx context.Context) ([]*model.SystemSetting, error) {
	return cs.store.ListAllSettings(ctx)
}

// BatchUpdateSettings 批量更新配置（仅写数据库，不更新缓存，因为会重启）
func (cs *ConfigService) BatchUpdateSettings(ctx context.Context, updates map[string]string) error {
	return cs.store.BatchUpdateSettings(ctx, updates)
}
