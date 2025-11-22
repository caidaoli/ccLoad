package util

import (
	"fmt"
	"strings"
)

// ============================================================================
// AI API 成本计算器（Claude + OpenAI）
// ============================================================================

// ModelPricing AI模型定价（单位：美元/百万tokens）
type ModelPricing struct {
	InputPrice  float64 // 基础输入token价格（$/1M tokens, ≤200k context for Gemini）
	OutputPrice float64 // 输出token价格（$/1M tokens, ≤200k context for Gemini）

	// 长上下文定价（Gemini >200k tokens）
	// 如果为0，表示无分段定价，使用InputPrice/OutputPrice
	InputPriceHigh  float64 // 高上下文输入价格（$/1M tokens, >200k context）
	OutputPriceHigh float64 // 高上下文输出价格（$/1M tokens, >200k context）
}

// modelPricing 统一定价表（Claude + OpenAI）
// 数据来源：
// - Claude: https://docs.claude.com/en/docs/about-claude/pricing
// - OpenAI: https://openai.com/api/pricing/ (2025年1月数据)
var modelPricing = map[string]ModelPricing{
	// ========== Claude 模型 ==========
	// Claude 4.x 系列（当前最新）
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

	// ========== OpenAI 模型（Standard层级定价 - 2025年官方）==========
	// 数据来源: https://platform.openai.com/docs/pricing

	// GPT-5 系列
	"gpt-5.1":              {InputPrice: 1.25, OutputPrice: 10.00},  // 缓存: $0.125/1M
	"gpt-5":                {InputPrice: 1.25, OutputPrice: 10.00},  // 缓存: $0.125/1M
	"gpt-5-mini":           {InputPrice: 0.25, OutputPrice: 2.00},   // 缓存: $0.025/1M
	"gpt-5-nano":           {InputPrice: 0.05, OutputPrice: 0.40},   // 缓存: $0.005/1M
	"gpt-5.1-chat-latest":  {InputPrice: 1.25, OutputPrice: 10.00},
	"gpt-5-chat-latest":    {InputPrice: 1.25, OutputPrice: 10.00},
	"gpt-5.1-codex":        {InputPrice: 1.25, OutputPrice: 10.00},  // 缓存: $0.125/1M
	"gpt-5-codex":          {InputPrice: 1.25, OutputPrice: 10.00},
	"gpt-5.1-codex-mini":   {InputPrice: 0.25, OutputPrice: 2.00},
	"gpt-5-pro":            {InputPrice: 15.00, OutputPrice: 120.00}, // 无缓存
	"gpt-5-search-api":     {InputPrice: 1.25, OutputPrice: 10.00},

	// GPT-4.1 系列（新）
	"gpt-4.1":      {InputPrice: 2.00, OutputPrice: 8.00},  // 缓存: $0.50/1M
	"gpt-4.1-mini": {InputPrice: 0.40, OutputPrice: 1.60},  // 缓存: $0.10/1M
	"gpt-4.1-nano": {InputPrice: 0.10, OutputPrice: 0.40},  // 缓存: $0.025/1M

	// GPT-4o 系列
	"gpt-4o":            {InputPrice: 2.50, OutputPrice: 10.00}, // 缓存: $1.25/1M
	"gpt-4o-2024-05-13": {InputPrice: 5.00, OutputPrice: 15.00}, // 无缓存
	"gpt-4o-mini":       {InputPrice: 0.15, OutputPrice: 0.60},  // 缓存: $0.075/1M
	"chatgpt-4o-latest": {InputPrice: 5.00, OutputPrice: 15.00},

	// GPT-4 Turbo 系列（Legacy）
	"gpt-4-turbo":              {InputPrice: 10.00, OutputPrice: 30.00},
	"gpt-4-turbo-2024-04-09":   {InputPrice: 10.00, OutputPrice: 30.00},
	"gpt-4-0125-preview":       {InputPrice: 10.00, OutputPrice: 30.00},
	"gpt-4-1106-preview":       {InputPrice: 10.00, OutputPrice: 30.00},
	"gpt-4-1106-vision-preview": {InputPrice: 10.00, OutputPrice: 30.00},

	// GPT-4 标准系列（Legacy）
	"gpt-4":          {InputPrice: 30.00, OutputPrice: 60.00},
	"gpt-4-0613":     {InputPrice: 30.00, OutputPrice: 60.00},
	"gpt-4-0314":     {InputPrice: 30.00, OutputPrice: 60.00},
	"gpt-4-32k":      {InputPrice: 60.00, OutputPrice: 120.00},
	"gpt-4-32k-0613": {InputPrice: 60.00, OutputPrice: 120.00},

	// GPT-3.5 系列（Legacy）
	"gpt-3.5-turbo":          {InputPrice: 0.50, OutputPrice: 1.50},
	"gpt-3.5-turbo-0125":     {InputPrice: 0.50, OutputPrice: 1.50},
	"gpt-3.5-turbo-1106":     {InputPrice: 1.00, OutputPrice: 2.00},
	"gpt-3.5-turbo-0613":     {InputPrice: 1.50, OutputPrice: 2.00},
	"gpt-3.5-0301":           {InputPrice: 1.50, OutputPrice: 2.00},
	"gpt-3.5-turbo-instruct": {InputPrice: 1.50, OutputPrice: 2.00},
	"gpt-3.5-turbo-16k-0613": {InputPrice: 3.00, OutputPrice: 4.00},

	// o系列（推理模型）
	"o1":                    {InputPrice: 15.00, OutputPrice: 60.00}, // 缓存: $7.50/1M
	"o1-pro":                {InputPrice: 150.00, OutputPrice: 600.00}, // 无缓存
	"o1-mini":               {InputPrice: 1.10, OutputPrice: 4.40},   // 缓存: $0.55/1M
	"o3":                    {InputPrice: 2.00, OutputPrice: 8.00},   // 缓存: $0.50/1M
	"o3-pro":                {InputPrice: 20.00, OutputPrice: 80.00}, // 无缓存
	"o3-mini":               {InputPrice: 1.10, OutputPrice: 4.40},   // 缓存: $0.55/1M
	"o3-deep-research":      {InputPrice: 10.00, OutputPrice: 40.00}, // 缓存: $2.50/1M
	"o4-mini":               {InputPrice: 1.10, OutputPrice: 4.40},   // 缓存: $0.275/1M
	"o4-mini-deep-research": {InputPrice: 2.00, OutputPrice: 8.00},   // 缓存: $0.50/1M

	// 其他专用模型
	"computer-use-preview":       {InputPrice: 3.00, OutputPrice: 12.00},
	"codex-mini-latest":          {InputPrice: 1.50, OutputPrice: 6.00},
	"gpt-4o-mini-search-preview": {InputPrice: 0.15, OutputPrice: 0.60},
	"gpt-4o-search-preview":      {InputPrice: 2.50, OutputPrice: 10.00},
	"davinci-002":                {InputPrice: 2.00, OutputPrice: 2.00},
	"babbage-002":                {InputPrice: 0.40, OutputPrice: 0.40},

	// ========== Gemini 模型（2025年官方定价）==========
	// 数据来源: https://ai.google.dev/gemini-api/docs/pricing
	// 注意：部分模型有分段定价（≤200k vs >200k tokens）

	// Gemini 3.0 系列（分段定价）
	"gemini-3-pro": {
		InputPrice:      2.00,
		OutputPrice:     12.00,
		InputPriceHigh:  4.00,  // >200k context
		OutputPriceHigh: 18.00, // >200k context
	},

	// Gemini 2.5 系列
	"gemini-2.5-pro": {
		InputPrice:      1.25,
		OutputPrice:     10.00,
		InputPriceHigh:  2.50,  // >200k context
		OutputPriceHigh: 15.00, // >200k context
	},
	"gemini-2.5-flash":      {InputPrice: 0.30, OutputPrice: 2.50},  // 无分段
	"gemini-2.5-flash-lite": {InputPrice: 0.10, OutputPrice: 0.40},  // 无分段

	// Gemini 2.0 系列（无分段）
	"gemini-2.0-flash":      {InputPrice: 0.10, OutputPrice: 0.40},
	"gemini-2.0-flash-lite": {InputPrice: 0.075, OutputPrice: 0.30},

	// Gemini 1.5 系列（向后兼容，无分段）
	"gemini-1.5-pro":   {InputPrice: 1.25, OutputPrice: 5.00},
	"gemini-1.5-flash": {InputPrice: 0.20, OutputPrice: 0.60},
}

const (
	// cacheReadMultiplier 缓存读取价格倍数（相对于基础input价格）
	// Cache Read = Input Price × 0.1 (90%节省)
	// 适用于Claude和Gemini模型
	// 例如：Claude Sonnet input=$3.00/1M → cached=$0.30/1M
	cacheReadMultiplierClaude = 0.1

	// cacheReadMultiplierOpenAI OpenAI缓存读取价格倍数
	// Cache Read = Input Price × 0.5 (50%节省)
	// 适用于OpenAI模型
	// 例如：GPT-4o input=$2.50/1M → cached=$1.25/1M
	cacheReadMultiplierOpenAI = 0.5

	// cacheWriteMultiplier 缓存写入价格倍数（相对于基础input价格）
	// Cache Write = Input Price × 1.25 (25%溢价)
	// 仅适用于Claude模型（OpenAI不支持cache_creation）
	cacheWriteMultiplier = 1.25

	// geminiLongContextThreshold Gemini长上下文阈值（tokens）
	// 超过此阈值的请求将使用InputPriceHigh/OutputPriceHigh定价
	// 参考：https://ai.google.dev/gemini-api/docs/pricing
	geminiLongContextThreshold = 200_000
)

// CalculateCost 计算单次请求的成本（美元）
// 参数：
//   - model: 模型名称（如"claude-sonnet-4-5-20250929"或"gpt-5.1-codex"）
//   - inputTokens: 输入token数量
//   - outputTokens: 输出token数量
//   - cacheReadTokens: 缓存读取token数量（Claude: cache_read_input_tokens, OpenAI: cached_tokens）
//   - cacheCreationTokens: 缓存创建token数量（Claude: cache_creation_input_tokens）
//
// 返回：总成本（美元），如果模型未知则返回0.0
func CalculateCost(model string, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens int) float64 {
	pricing, ok := modelPricing[model]
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

	// Gemini长上下文分段定价逻辑
	// 如果模型支持分段定价（InputPriceHigh > 0）且输入侧token数 > 200k，使用高价格
	// 注意：阈值仅针对输入侧（prompt + cache），输出不计入
	totalInputTokens := inputTokens + cacheReadTokens + cacheCreationTokens
	useHighPricing := pricing.InputPriceHigh > 0 && totalInputTokens > geminiLongContextThreshold

	// 选择适用的价格
	inputPricePerM := pricing.InputPrice
	outputPricePerM := pricing.OutputPrice
	if useHighPricing {
		inputPricePerM = pricing.InputPriceHigh
		// 输出价格保持不变，因为Gemini长上下文定价仅影响输入侧
		// outputPricePerM 保持 pricing.OutputPrice
	}

	// 检测是否为OpenAI模型（处理缓存语义差异）
	isOpenAI := isOpenAIModel(model)

	// 计算实际计费的输入token数量
	billableInputTokens := inputTokens
	if isOpenAI && cacheReadTokens > 0 {
		// OpenAI语义：prompt_tokens包含cached_tokens，需要减去避免双计
		// 例如：prompt_tokens=1000, cached_tokens=800
		//      → 实际非缓存部分 = 1000 - 800 = 200
		billableInputTokens = inputTokens - cacheReadTokens
		if billableInputTokens < 0 {
			billableInputTokens = 0 // 防御性处理
		}
	}

	// 1. 基础输入token成本
	if billableInputTokens > 0 {
		cost += float64(billableInputTokens) * inputPricePerM / 1_000_000
	}

	// 2. 输出token成本
	if outputTokens > 0 {
		cost += float64(outputTokens) * outputPricePerM / 1_000_000
	}

	// 3. 缓存读取成本
	if cacheReadTokens > 0 {
		cacheMultiplier := cacheReadMultiplierClaude // Claude/Gemini: 10%折扣
		if isOpenAI {
			cacheMultiplier = cacheReadMultiplierOpenAI // OpenAI: 50%折扣
		}
		cacheReadPrice := inputPricePerM * cacheMultiplier
		cost += float64(cacheReadTokens) * cacheReadPrice / 1_000_000
	}

	// 4. 缓存创建成本（125%基础价格，仅Claude支持）
	if cacheCreationTokens > 0 {
		cacheWritePrice := inputPricePerM * cacheWriteMultiplier
		cost += float64(cacheCreationTokens) * cacheWritePrice / 1_000_000
	}

	return cost
}

// isOpenAIModel 判断是否为OpenAI模型
// OpenAI模型包括：gpt-*, o*, chatgpt-*, davinci-*, babbage-*, computer-use-preview, codex-*
func isOpenAIModel(model string) bool {
	lowerModel := strings.ToLower(model)
	return strings.HasPrefix(lowerModel, "gpt-") ||
		strings.HasPrefix(lowerModel, "o1") ||
		strings.HasPrefix(lowerModel, "o3") ||
		strings.HasPrefix(lowerModel, "o4") ||
		strings.HasPrefix(lowerModel, "chatgpt-") ||
		strings.HasPrefix(lowerModel, "davinci-") ||
		strings.HasPrefix(lowerModel, "babbage-") ||
		strings.HasPrefix(lowerModel, "codex-") ||
		lowerModel == "computer-use-preview"
}

// fuzzyMatchModel 模糊匹配模型名称
// 例如：claude-3-opus-20240229-extended → claude-3-opus
//      gpt-4o-2024-12-01 → gpt-4o
func fuzzyMatchModel(model string) (ModelPricing, bool) {
	lowerModel := strings.ToLower(model)

	// 尝试通用前缀匹配（按优先级排序：更具体的前缀优先）
	prefixes := []string{
		// Claude 模型
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

		// Gemini 模型（按优先级排序：更具体的前缀优先）
		"gemini-3-pro",
		"gemini-2.5-flash-lite",
		"gemini-2.5-flash",
		"gemini-2.5-pro",
		"gemini-2.0-flash-lite",
		"gemini-2.0-flash",
		"gemini-1.5-pro",
		"gemini-1.5-flash",

		// OpenAI 模型（按优先级排序：更具体的前缀优先）
		"gpt-5.1-codex-mini",
		"gpt-5.1-codex",
		"gpt-5.1-chat-latest",
		"gpt-5.1",
		"gpt-5-codex",
		"gpt-5-chat-latest",
		"gpt-5-search-api",
		"gpt-5-pro",
		"gpt-5-nano",
		"gpt-5-mini",
		"gpt-5",
		"gpt-4.1-nano",
		"gpt-4.1-mini",
		"gpt-4.1",
		"chatgpt-4o-latest",
		"gpt-4o-mini-search-preview",
		"gpt-4o-search-preview",
		"gpt-4o-mini",
		"gpt-4o",
		"gpt-4-turbo-vision",
		"gpt-4-turbo-preview",
		"gpt-4-turbo",
		"gpt-4-32k",
		"gpt-4",
		"gpt-3.5-turbo-instruct",
		"gpt-3.5-turbo-16k",
		"gpt-3.5-turbo",
		"o4-mini-deep-research",
		"o4-mini",
		"o3-deep-research",
		"o3-pro",
		"o3-mini",
		"o3",
		"o1-pro",
		"o1-mini",
		"o1",
		"computer-use-preview",
		"codex-mini-latest",
		"davinci-002",
		"babbage-002",
	}

	for _, prefix := range prefixes {
		if strings.HasPrefix(lowerModel, prefix) {
			if pricing, ok := modelPricing[prefix]; ok {
				return pricing, true
			}
			// 尝试查找带日期后缀的版本（如gpt-4o-2024-11-20）
			for key, pricing := range modelPricing {
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
