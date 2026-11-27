package util

import "strings"

/**
 * @brief Cursor 官方超额参考费率，单位为美元每百万 token。
 * @details https://cursor.com/docs/models-and-pricing
 */
var cursorReferencePricing = map[string]ModelPricing{
	"composer-2.5":      {InputPrice: 0.50, CacheReadPrice: 0.20, OutputPrice: 2.50, HasCacheReadPrice: true},
	"composer-2.5-fast": {InputPrice: 3.00, CacheReadPrice: 0.50, OutputPrice: 15.00, HasCacheReadPrice: true},
	"grok-4.6":          {InputPrice: 2.00, CacheReadPrice: 0.50, OutputPrice: 6.00, HasCacheReadPrice: true},
	"grok-4.6-fast":     {InputPrice: 4.00, CacheReadPrice: 1.00, OutputPrice: 12.00, HasCacheReadPrice: true},
	"grok-4.7": {
		InputPrice: 2.00, CacheReadPrice: 0.50, OutputPrice: 6.00, HasCacheReadPrice: true,
		InputPriceHigh: 4.00, CacheReadPriceHigh: 1.00, OutputPriceHigh: 12.00,
	},
	"grok-4.7-fast": {
		InputPrice: 4.00, CacheReadPrice: 1.00, OutputPrice: 12.00, HasCacheReadPrice: true,
		InputPriceHigh: 6.00, CacheReadPriceHigh: 1.50, OutputPriceHigh: 18.00,
	},
}

/**
 * @brief 识别 Cursor 官方参考价涵盖的模型与速度组合。
 * @param model Cursor SDK 模型 ID 或网关模型别名。
 * @return Cursor 价目名称；其它模型返回空字符串。
 */
func CursorPricingModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	base, fast := strings.CutSuffix(model, "-fast")
	base, slow := strings.CutSuffix(base, "-slow")
	base, _, _ = strings.Cut(base, "@")
	base, nativeFast := strings.CutSuffix(base, "-fast")
	base = strings.TrimSuffix(base, "-slow")
	if fast || (nativeFast && !slow) {
		base += "-fast"
	}
	if _, ok := cursorReferencePricing[base]; ok {
		return base
	}
	return ""
}

/**
 * @brief 计算 Cursor 自有模型的超额参考成本，按有效速度和每轮真实用量选价。
 * @param routedModel 网关原有计费模型，用于解析自定义价。
 * @param model CursorPricingModel 规范化后的 SDK 模型和速度。
 * @param price 渠道价格覆盖；显式价目优先于全局自定义与 Cursor 参考价。
 * @param increments 不重叠的真实用量轮次；缺少边界的终态补差视为一份。
 * @return 各成本分项及合计；空用量的总额为零。
 * @return 命中 Cursor 自有模型时为 true；其它模型沿用原提供商价目。
 */
func CalculateCursorCostBreakdown(routedModel, model string, price *CustomModelPrice, increments []TokenCostUsage) (StandardCostBreakdown, bool) {
	pricing, ok := cursorReferencePricing[model]
	if !ok {
		return StandardCostBreakdown{}, false
	}
	base := strings.TrimSuffix(model, "-fast")
	if override := resolveModelPriceOverride(base, price); override != nil {
		pricing = *override
	} else {
		for _, id := range []string{routedModel, model, base} {
			if custom, ok := getPricing(id); ok && custom.isCustom {
				pricing = custom
				break
			}
		}
	}
	if base == "grok-4.7" {
		pricing.tierThreshold = 256_000
		pricing.CacheReadCountsTowardTier = true
	}
	return calculateIncrementalCostBreakdown(model, pricing, increments), true
}
