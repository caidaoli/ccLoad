package util

import (
	"fmt"
	"strings"
)

// ============================================================================
// Claude API 成本计算器
// ============================================================================

// ModelPricing Claude模型定价（单位：美元/百万tokens）
type ModelPricing struct {
	InputPrice  float64 // 基础输入token价格（$/1M tokens）
	OutputPrice float64 // 输出token价格（$/1M tokens）
}

// claudePricing Claude API完整定价表（2025年11月）
// 数据来源：https://docs.claude.com/en/docs/about-claude/pricing
var claudePricing = map[string]ModelPricing{
	// Claude 4.x 系列（当前最新）
	"claude-sonnet-4-5-20250929": {InputPrice: 3.00, OutputPrice: 15.00},
	"claude-sonnet-4-5":          {InputPrice: 3.00, OutputPrice: 15.00}, // 别名
	"claude-haiku-4-5-20251001":  {InputPrice: 1.00, OutputPrice: 5.00},
	"claude-haiku-4-5":           {InputPrice: 1.00, OutputPrice: 5.00}, // 别名
	"claude-opus-4-1-20250805":   {InputPrice: 15.00, OutputPrice: 75.00},
	"claude-opus-4-1":            {InputPrice: 15.00, OutputPrice: 75.00}, // 别名

	// Claude 4.0 系列（遗留）
	"claude-sonnet-4-20250514":  {InputPrice: 3.00, OutputPrice: 15.00},
	"claude-sonnet-4-0":         {InputPrice: 3.00, OutputPrice: 15.00}, // 别名
	"claude-opus-4-20250514":    {InputPrice: 15.00, OutputPrice: 75.00},
	"claude-opus-4-0":           {InputPrice: 15.00, OutputPrice: 75.00}, // 别名
	"claude-3-7-sonnet-20250219": {InputPrice: 3.00, OutputPrice: 15.00},
	"claude-3-7-sonnet-latest":   {InputPrice: 3.00, OutputPrice: 15.00}, // 别名

	// Claude 3.5 系列（遗留）
	"claude-3-5-sonnet-20241022": {InputPrice: 3.00, OutputPrice: 15.00},
	"claude-3-5-sonnet-20240620": {InputPrice: 3.00, OutputPrice: 15.00},
	"claude-3-5-sonnet-latest":   {InputPrice: 3.00, OutputPrice: 15.00}, // 别名
	"claude-3-5-haiku-20241022":  {InputPrice: 0.80, OutputPrice: 4.00},
	"claude-3-5-haiku-latest":    {InputPrice: 0.80, OutputPrice: 4.00}, // 别名

	// Claude 3.0 系列（遗留）
	"claude-3-opus-20240229":  {InputPrice: 15.00, OutputPrice: 75.00},
	"claude-3-opus-latest":    {InputPrice: 15.00, OutputPrice: 75.00}, // 别名
	"claude-3-sonnet-20240229": {InputPrice: 3.00, OutputPrice: 15.00},
	"claude-3-sonnet-latest":   {InputPrice: 3.00, OutputPrice: 15.00}, // 别名
	"claude-3-haiku-20240307":  {InputPrice: 0.25, OutputPrice: 1.25},
	"claude-3-haiku-latest":    {InputPrice: 0.25, OutputPrice: 1.25}, // 别名

	// 通配符匹配（向后兼容）
	"claude-3-opus":   {InputPrice: 15.00, OutputPrice: 75.00},
	"claude-3-sonnet": {InputPrice: 3.00, OutputPrice: 15.00},
	"claude-3-haiku":  {InputPrice: 0.25, OutputPrice: 1.25},
}

const (
	// cacheReadMultiplier 缓存读取价格倍数（相对于基础input价格）
	// Cache Read = Input Price × 0.1 (90%节省)
	cacheReadMultiplier = 0.1

	// cacheWriteMultiplier 缓存写入价格倍数（相对于基础input价格）
	// Cache Write = Input Price × 1.25 (25%溢价)
	cacheWriteMultiplier = 1.25
)

// CalculateCost 计算单次请求的成本（美元）
// 参数：
//   - model: 模型名称（如"claude-sonnet-4-5-20250929"）
//   - inputTokens: 输入token数量
//   - outputTokens: 输出token数量
//   - cacheReadTokens: 缓存读取token数量
//   - cacheCreationTokens: 缓存创建token数量
//
// 返回：总成本（美元），如果模型未知则返回0.0
func CalculateCost(model string, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int) float64 {
	pricing, ok := claudePricing[model]
	if !ok {
		// 尝试模糊匹配（例如：claude-3-opus-xxx → claude-3-opus）
		pricing, ok = fuzzyMatchModel(model)
		if !ok {
			return 0.0 // 未知模型
		}
	}

	// 成本计算公式（单位：美元）
	// 注意：价格是per 1M tokens，需要除以1,000,000
	cost := 0.0

	// 1. 基础输入token成本
	if inputTokens > 0 {
		cost += float64(inputTokens) * pricing.InputPrice / 1_000_000
	}

	// 2. 输出token成本
	if outputTokens > 0 {
		cost += float64(outputTokens) * pricing.OutputPrice / 1_000_000
	}

	// 3. 缓存读取成本（10%基础价格）
	if cacheReadTokens > 0 {
		cacheReadPrice := pricing.InputPrice * cacheReadMultiplier
		cost += float64(cacheReadTokens) * cacheReadPrice / 1_000_000
	}

	// 4. 缓存创建成本（125%基础价格）
	if cacheCreationTokens > 0 {
		cacheWritePrice := pricing.InputPrice * cacheWriteMultiplier
		cost += float64(cacheCreationTokens) * cacheWritePrice / 1_000_000
	}

	return cost
}

// fuzzyMatchModel 模糊匹配模型名称
// 例如：claude-3-opus-20240229-extended → claude-3-opus
func fuzzyMatchModel(model string) (ModelPricing, bool) {
	lowerModel := strings.ToLower(model)

	// 尝试通用前缀匹配
	prefixes := []string{
		"claude-sonnet-4-5",
		"claude-haiku-4-5",
		"claude-opus-4-1",
		"claude-sonnet-4-0",
		"claude-opus-4-0",
		"claude-3-7-sonnet",
		"claude-3-5-sonnet",
		"claude-3-5-haiku",
		"claude-3-opus",
		"claude-3-sonnet",
		"claude-3-haiku",
	}

	for _, prefix := range prefixes {
		if strings.HasPrefix(lowerModel, prefix) {
			if pricing, ok := claudePricing[prefix]; ok {
				return pricing, true
			}
			// 尝试添加后缀（如-20240229）
			for key, pricing := range claudePricing {
				if strings.HasPrefix(key, prefix) {
					return pricing, true
				}
			}
		}
	}

	return ModelPricing{}, false
}

// FormatCost 格式化成本为美元字符串
// 例如：0.00123 → "$0.00123"
//      0.000005 → "$0.000005"
func FormatCost(cost float64) string {
	if cost == 0 {
		return "$0.00"
	}
	if cost < 0.001 {
		return "$" + formatSmallCost(cost)
	}
	return "$" + formatLargeCost(cost)
}

// formatSmallCost 格式化小额成本（<$0.001）
// 使用科学计数法或保留足够精度
func formatSmallCost(cost float64) string {
	// 小于$0.000001使用科学计数法
	if cost < 0.000001 {
		return formatScientific(cost)
	}
	// 否则保留6位小数
	return formatFixed(cost, 6)
}

// formatLargeCost 格式化正常成本（>=$0.001）
// 保留4位小数（足够显示$0.0001级别）
func formatLargeCost(cost float64) string {
	if cost >= 1.0 {
		return formatFixed(cost, 2) // 大于$1显示2位小数
	}
	return formatFixed(cost, 4) // 否则显示4位小数
}

// formatFixed 格式化为固定小数位（去除尾随0）
func formatFixed(val float64, precision int) string {
	format := "%." + string(rune(precision+'0')) + "f"
	result := ""
	// 简单格式化（Go标准库会自动去除尾随0）
	switch precision {
	case 2:
		result = trimTrailingZeros(val, "%.2f")
	case 4:
		result = trimTrailingZeros(val, "%.4f")
	case 6:
		result = trimTrailingZeros(val, "%.6f")
	default:
		result = trimTrailingZeros(val, format)
	}
	return result
}

// formatScientific 科学计数法格式化
func formatScientific(val float64) string {
	return trimTrailingZeros(val, "%.2e")
}

// trimTrailingZeros 去除尾随零
func trimTrailingZeros(val float64, format string) string {
	s := ""
	switch format {
	case "%.2f":
		s = formatFloat(val, 2)
	case "%.4f":
		s = formatFloat(val, 4)
	case "%.6f":
		s = formatFloat(val, 6)
	case "%.2e":
		s = formatFloatScientific(val)
	}
	// 去除尾随的0和.
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	return s
}

// formatFloat 格式化浮点数（简化版）
func formatFloat(val float64, prec int) string {
	// 简单实现：使用字符串格式化
	var s string
	switch prec {
	case 2:
		s = fmt.Sprintf("%.2f", val)
	case 4:
		s = fmt.Sprintf("%.4f", val)
	case 6:
		s = fmt.Sprintf("%.6f", val)
	}
	return s
}

// formatFloatScientific 科学计数法格式化（简化版）
func formatFloatScientific(val float64) string {
	return fmt.Sprintf("%.2e", val)
}
