package main

import "strings"

// normalizeAPIKeys 规范化Config的APIKeys字段
// 设计原则（DRY）：统一处理api_keys字段的序列化准备，确保数据库中不会出现"null"字符串
//
// 处理逻辑：
// 1. 如果APIKeys数组不为空，直接使用
// 2. 如果APIKeys为空但APIKey字段有值，将APIKey分割并填充到APIKeys
// 3. 确保APIKeys至少是空数组[]，而不是nil（避免序列化为"null"）
//
// 调用时机：
// - CreateConfig：创建渠道前
// - UpdateConfig：更新渠道前
// - ReplaceConfig：替换渠道前（CSV导入）
// - handleCreateChannel：API创建渠道前
// - handleUpdateChannel：API更新渠道前
func normalizeAPIKeys(cfg *Config) {
	// 情况1：APIKeys已有值，直接返回
	if len(cfg.APIKeys) > 0 {
		return
	}

	// 情况2：APIKeys为空，但APIKey字段有值
	// 分割APIKey字段（支持逗号分隔的多个Key）
	if cfg.APIKey != "" {
		keys := strings.Split(cfg.APIKey, ",")
		apiKeys := make([]string, 0, len(keys))
		for _, k := range keys {
			trimmed := strings.TrimSpace(k)
			if trimmed != "" {
				apiKeys = append(apiKeys, trimmed)
			}
		}
		// 如果分割后有有效Key，赋值给APIKeys
		if len(apiKeys) > 0 {
			cfg.APIKeys = apiKeys
			return
		}
	}

	// 情况3：APIKeys和APIKey都为空
	// 确保APIKeys至少是空数组，而不是nil（避免序列化为"null"）
	cfg.APIKeys = []string{}
}
