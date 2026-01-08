package app

import (
	"strconv"
	"strings"
	"time"

	modelpkg "ccLoad/internal/model"
)

// configSupportsModel 检查渠道是否支持指定模型
func (s *Server) configSupportsModel(cfg *modelpkg.Config, model string) bool {
	if model == "*" {
		return true
	}
	return cfg.SupportsModel(model)
}

// configSupportsModelWithDateFallback 检查渠道是否支持指定模型（含日期后缀回退和模糊匹配）
//
// 匹配策略（按优先级）：
// 1. 精确匹配：cfg.SupportsModel(model)
// 2. 日期后缀回退（需启用 model_lookup_strip_date_suffix）：
//   - 请求带日期 → 无日期：claude-3-5-sonnet-20241022 → claude-3-5-sonnet
//   - 请求无日期 → 带日期：claude-sonnet-4-5 → claude-sonnet-4-5-20250929
//
// 3. 模糊匹配（需启用 model_fuzzy_match）：sonnet → claude-sonnet-4-5-20250929
func (s *Server) configSupportsModelWithDateFallback(cfg *modelpkg.Config, model string) bool {
	if s.configSupportsModel(cfg, model) {
		return true
	}
	if model == "*" {
		return false
	}

	// 日期后缀回退
	if s.modelLookupStripDateSuffix {
		// 请求带日期：claude-3-5-sonnet-20241022 -> claude-3-5-sonnet
		if stripped, ok := stripTrailingYYYYMMDD(model); ok && stripped != model {
			if cfg.SupportsModel(stripped) {
				return true
			}
		}

		// 请求无日期：claude-sonnet-4-5 -> claude-sonnet-4-5-20250929
		for _, entry := range cfg.ModelEntries {
			if entry.Model == "" {
				continue
			}
			if stripped, ok := stripTrailingYYYYMMDD(entry.Model); ok && stripped == model {
				return true
			}
		}
	}

	// 模糊匹配：sonnet -> claude-sonnet-4-5-20250929
	if s.modelFuzzyMatch {
		if _, ok := cfg.FuzzyMatchModel(model); ok {
			return true
		}
	}

	return false
}

// stripTrailingYYYYMMDD 剥离模型名末尾的 YYYYMMDD 日期后缀
//
// 示例：
//   - claude-3-5-sonnet-20241022 → claude-3-5-sonnet, true
//   - claude-sonnet-4-5 → claude-sonnet-4-5, false
//
// 验证规则：
//   - 后缀必须是8位数字
//   - 年份：2000-2100
//   - 月份：1-12
//   - 日期：1-当月最大天数
func stripTrailingYYYYMMDD(model string) (string, bool) {
	dash := strings.LastIndexByte(model, '-')
	if dash < 0 {
		return model, false
	}
	suffix := model[dash+1:]
	if len(suffix) != 8 {
		return model, false
	}
	for i := 0; i < len(suffix); i++ {
		if suffix[i] < '0' || suffix[i] > '9' {
			return model, false
		}
	}
	year, _ := strconv.Atoi(suffix[:4])
	month, _ := strconv.Atoi(suffix[4:6])
	day, _ := strconv.Atoi(suffix[6:8])
	if year < 2000 || year > 2100 {
		return model, false
	}
	if month < 1 || month > 12 {
		return model, false
	}
	lastDay := time.Date(year, time.Month(month)+1, 0, 0, 0, 0, 0, time.UTC).Day()
	if day < 1 || day > lastDay {
		return model, false
	}
	return model[:dash], true
}
