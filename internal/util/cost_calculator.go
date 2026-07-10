package util

import (
	"log"
	"strings"
)

// ============================================================================
// AI API 成本计算器（Claude + OpenAI）
// ============================================================================

// ImageGenerationToolUsage 是 Responses image_generation 工具返回的 token 用量。
type ImageGenerationToolUsage struct {
	InputTokens       int
	OutputTokens      int
	TextInputTokens   int
	TextCachedTokens  int
	ImageInputTokens  int
	ImageCachedTokens int
	ImageOutputTokens int
}

type imageGenerationToolPricing struct {
	TextInputPrice   float64
	TextCachedPrice  float64
	ImageInputPrice  float64
	ImageCachedPrice float64
	ImageOutputPrice float64
}

type imageGenerationFallbackPricing map[string]map[string]float64

var imageGenerationToolPricingByModel = map[string]imageGenerationToolPricing{
	// 来源: https://openai.com/api/pricing/ (GPT Image 2, per 1M tokens)
	"gpt-image-2": {
		TextInputPrice: 5.00, TextCachedPrice: 1.25,
		ImageInputPrice: 8.00, ImageCachedPrice: 2.00, ImageOutputPrice: 30.00,
	},
}

var imageGenerationFallbackCostByModel = map[string]imageGenerationFallbackPricing{
	// 来源: https://developers.openai.com/api/docs/guides/image-generation#calculating-costs
	"gpt-image-2": {
		"low": {
			"1024x1024": 0.006,
			"1024x1536": 0.005,
			"1536x1024": 0.005,
		},
		"medium": {
			"1024x1024": 0.053,
			"1024x1536": 0.041,
			"1536x1024": 0.041,
		},
		"high": {
			"1024x1024": 0.211,
			"1024x1536": 0.165,
			"1536x1024": 0.165,
		},
	},
}

const (
	// cacheReadMultiplierClaude Claude Sonnet/Haiku 缓存读取价格倍数
	// Cache Read = Input Price × 0.1 (90%节省)
	// 适用于Claude Sonnet/Haiku和Gemini模型
	// 例如：Claude Sonnet input=$3.00/1M → cached=$0.30/1M
	cacheReadMultiplierClaude = 0.1

	// cacheReadMultiplierOpus Claude Opus 缓存读取价格倍数
	// Cache Read = Input Price × 0.1 (90%折扣)
	// 适用于Claude Opus系列模型（Opus 4.5, 4.1, 4.0, 3）
	// 例如：Claude Opus 4.5 input=$5.00/1M → cached=$0.50/1M
	// 参考：https://docs.claude.com/en/docs/about-claude/pricing
	cacheReadMultiplierOpus = 0.1

	// cacheWrite5mMultiplier 缓存写入价格倍数（相对于基础input价格）
	// Cache Write = Input Price × 1.25 (25%溢价)
	// 适用于 Anthropic 5m cache write 和 OpenAI cache_creation_input_tokens。
	// 参考：https://platform.claude.com/docs/en/build-with-claude/prompt-caching
	cacheWrite5mMultiplier = 1.25

	// cacheWrite1hMultiplier 1小时缓存写入价格倍数（相对于基础input价格）
	// 1h Cache Write = Input Price × 2.0 (100%溢价)
	// 仅适用于Claude模型（OpenAI不支持cache_creation）
	// 参考：https://platform.claude.com/docs/en/build-with-claude/prompt-caching
	cacheWrite1hMultiplier = 2.0

	// geminiLongContextThreshold Gemini长上下文阈值（tokens）
	// 超过此阈值的请求将使用InputPriceHigh/OutputPriceHigh定价
	// 参考：https://ai.google.dev/gemini-api/docs/pricing
	geminiLongContextThreshold = 200_000

	// qwenPlusTierThreshold Qwen Plus 系列分档阈值（tokens）
	// 参考用户提供的价格表：0<Tokens<=256K 与 256K<Tokens<=1M
	qwenPlusTierThreshold = 256_000

	// gpt54TierThreshold GPT-5.4 系列分档阈值（tokens）
	// 参考：<=272K 与 >272K context length
	gpt54TierThreshold = 272_000

	// minimaxM3TierThreshold MiniMax-M3 分档阈值（tokens）
	// 参考：<=512K 与 >512K input tokens
	minimaxM3TierThreshold = 512_000
)

func getTierThresholdForModel(model string) int {
	lowerModel := strings.ToLower(model)
	switch {
	case strings.HasPrefix(lowerModel, "gpt-5.5"),
		strings.HasPrefix(lowerModel, "gpt-5.4"):
		return gpt54TierThreshold
	case strings.HasPrefix(lowerModel, "minimax-m3"):
		return minimaxM3TierThreshold
	case strings.HasPrefix(lowerModel, "qwen3.5-plus"),
		strings.HasPrefix(lowerModel, "qwen-3.5-plus"),
		strings.HasPrefix(lowerModel, "qwen3.6-plus"),
		strings.HasPrefix(lowerModel, "qwen-3.6-plus"),
		strings.HasPrefix(lowerModel, "qwen-plus"),
		strings.HasPrefix(lowerModel, "mimo-"):
		return qwenPlusTierThreshold
	default:
		return geminiLongContextThreshold
	}
}

func selectTokenPricingTier(tiers []TokenPricingTier, inputTokens int) TokenPricingTier {
	if len(tiers) == 0 {
		return TokenPricingTier{}
	}
	for _, tier := range tiers {
		if tier.MaxInputTokens == 0 || inputTokens <= tier.MaxInputTokens {
			return tier
		}
	}
	return tiers[len(tiers)-1]
}

// CalculateCostDetailed 计算单次请求的成本（美元）- 详细版本，支持5m和1h缓存分别计费
// 参数：
//   - model: 模型名称（如"claude-sonnet-4-5-20250929"或"gpt-5.1-codex"）
//   - inputTokens: 输入token数量（已归一化为可计费token）
//   - outputTokens: 输出token数量
//   - cacheReadTokens: 缓存读取token数量（Claude: cache_read_input_tokens, OpenAI: cached_tokens）
//   - cache5mTokens: 5分钟缓存创建token数量（Claude: ephemeral_5m_input_tokens）
//   - cache1hTokens: 1小时缓存创建token数量（Claude: ephemeral_1h_input_tokens）
//
// 重要: inputTokens应为"可计费输入token"，由解析层（proxy_sse_parser.go）负责归一化：
//   - OpenAI: 解析层已自动扣除cached_tokens（prompt_tokens - cached_tokens）
//   - Claude/Gemini: 解析层直接返回input_tokens（本身就是非缓存部分）
//
// 设计原则: 平台语义差异在解析层处理，计费层无需关心（SRP原则）
//
// 返回：总成本（美元），如果模型未知则返回0.0
func CalculateCostDetailed(model string, inputTokens, outputTokens, cacheReadTokens, cache5mTokens, cache1hTokens int) float64 {
	// 防御性检查:拒绝负数token
	if inputTokens < 0 || outputTokens < 0 || cacheReadTokens < 0 || cache5mTokens < 0 || cache1hTokens < 0 {
		log.Printf("[ERROR] 检测到负数 token（model=%s）: input=%d output=%d cache_read=%d cache_5m=%d cache_1h=%d",
			model, inputTokens, outputTokens, cacheReadTokens, cache5mTokens, cache1hTokens)
		return 0.0
	}

	pricing, ok := getPricing(model)
	if !ok {
		// 尝试模糊匹配(例如:claude-3-opus-xxx → claude-3-opus)
		pricing, ok = fuzzyMatchModel(model)
		if !ok {
			return 0.0 // 未知模型
		}
	}

	// 成本计算公式(单位:美元)
	// 注意:价格是per 1M tokens,需要除以1,000,000
	cost := 0.0

	// 分段定价逻辑（当前用于 Gemini / Qwen / MiMo / MiniMax-M3 系列）
	// 默认仅按非缓存输入判断；仅 MiMo 这类「input + cache_read 总量分档」的模型
	// （CacheReadCountsTowardTier=true）才把缓存读计入分档。Gemini 长上下文只看
	// 非缓存 prompt size，缓存读不得推高分档（否则 256K 缓存读会误触高档 input 价）。
	tierThreshold := getTierThresholdForModel(model)
	tierInputTokens := inputTokens
	if pricing.CacheReadCountsTowardTier {
		tierInputTokens += cacheReadTokens
	}

	// 选择适用的价格
	inputPricePerM := pricing.InputPrice
	outputPricePerM := pricing.OutputPrice
	selectedTier := TokenPricingTier{}
	hasSelectedTier := false
	useHighPricing := false
	if len(pricing.TokenPricingTiers) > 0 {
		selectedTier = selectTokenPricingTier(pricing.TokenPricingTiers, tierInputTokens)
		hasSelectedTier = true
		inputPricePerM = selectedTier.InputPrice
		outputPricePerM = selectedTier.OutputPrice
	} else if pricing.InputPriceHigh > 0 && tierInputTokens > tierThreshold {
		useHighPricing = true
		inputPricePerM = pricing.InputPriceHigh
		outputPricePerM = pricing.OutputPriceHigh // 分段定价同时影响输入和输出
	}

	// 1. 基础输入token成本（inputTokens已由解析层归一化，无需再处理平台差异）
	if inputTokens > 0 {
		cost += float64(inputTokens) * inputPricePerM / 1_000_000
	}

	// 2. 输出token成本
	if outputTokens > 0 {
		cost += float64(outputTokens) * outputPricePerM / 1_000_000
	}

	// 3. 缓存读取成本（OpenAI按模型系列有不同折扣率）
	if cacheReadTokens > 0 {
		cacheReadPrice := pricing.CacheReadPrice
		if hasSelectedTier && selectedTier.HasCacheReadPrice {
			cacheReadPrice = selectedTier.CacheReadPrice
		} else if !pricing.HasCacheReadPrice {
			cacheMultiplier := cacheReadMultiplierClaude // Claude全系/Gemini: 10%折扣
			if isOpenAIModel(model) {
				// OpenAI缓存折扣率按模型系列区分（2025-12官方定价）
				cacheMultiplier = getOpenAICacheMultiplier(model)
			} else if isOpusModel(model) {
				cacheMultiplier = cacheReadMultiplierOpus // Opus: 10%折扣
			}
			cacheReadPrice = inputPricePerM * cacheMultiplier
		} else if useHighPricing && pricing.CacheReadPriceHigh > 0 {
			cacheReadPrice = pricing.CacheReadPriceHigh
		}
		cost += float64(cacheReadTokens) * cacheReadPrice / 1_000_000
	}

	// 4. 5分钟缓存创建成本(1.25x基础价格,仅Claude支持)
	if cache5mTokens > 0 {
		cache5mWritePrice := inputPricePerM * cacheWrite5mMultiplier
		cost += float64(cache5mTokens) * cache5mWritePrice / 1_000_000
	}

	// 5. 1小时缓存创建成本(2.0x基础价格,仅Claude支持)
	if cache1hTokens > 0 {
		cache1hWritePrice := inputPricePerM * cacheWrite1hMultiplier
		cost += float64(cache1hTokens) * cache1hWritePrice / 1_000_000
	}

	// 6. 固定按次计费（图像生成等非token计费模型）
	// 当token成本为0但模型有固定费用时，使用每次请求成本
	if cost == 0 && pricing.FixedCostPerRequest > 0 {
		return pricing.FixedCostPerRequest
	}

	return cost
}

// CalculateImageGenerationToolCost 计算 Responses image_generation 工具费用。
func CalculateImageGenerationToolCost(model string, usage ImageGenerationToolUsage) float64 {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 ||
		usage.TextInputTokens < 0 || usage.TextCachedTokens < 0 ||
		usage.ImageInputTokens < 0 || usage.ImageCachedTokens < 0 || usage.ImageOutputTokens < 0 {
		return 0
	}

	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		model = "gpt-image-2"
	}
	pricing, ok := imageGenerationToolPricingByModel[model]
	if !ok {
		return 0
	}

	textInput := usage.TextInputTokens
	textCached := usage.TextCachedTokens
	imageInput := usage.ImageInputTokens
	imageCached := usage.ImageCachedTokens
	imageOutput := usage.ImageOutputTokens

	knownInput := textInput + textCached + imageInput + imageCached
	if usage.InputTokens > knownInput {
		imageInput += usage.InputTokens - knownInput
	}
	if imageOutput == 0 && usage.OutputTokens > 0 {
		imageOutput = usage.OutputTokens
	} else if usage.OutputTokens > imageOutput {
		imageOutput += usage.OutputTokens - imageOutput
	}

	return (float64(textInput)*pricing.TextInputPrice +
		float64(textCached)*pricing.TextCachedPrice +
		float64(imageInput)*pricing.ImageInputPrice +
		float64(imageCached)*pricing.ImageCachedPrice +
		float64(imageOutput)*pricing.ImageOutputPrice) / 1_000_000
}

// CalculateImageGenerationToolFallbackCost returns the fixed image output cost
// when OpenAI Responses image_generation succeeds but omits tool_usage.
func CalculateImageGenerationToolFallbackCost(model, quality, size string) float64 {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		model = "gpt-image-2"
	}
	quality = strings.ToLower(strings.TrimSpace(quality))
	size = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(size), " ", ""))

	byQuality, ok := imageGenerationFallbackCostByModel[model]
	if !ok {
		return 0
	}
	bySize, ok := byQuality[quality]
	if !ok {
		return 0
	}
	return bySize[size]
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

// serviceTierModels 列出支持 priority/flex service_tier 的 OpenAI 模型。
// 来源：OpenAI 官方 Pricing 页 Priority 表；GPT-5.6 预览公告明确支持 API priority processing。
// 注意：gpt-5.4-pro 虽在表中出现但价格列为空，不算支持。
var serviceTierModels = map[string]bool{
	"gpt-5.6":           true,
	"gpt-5.6-sol":       true,
	"gpt-5.6-terra":     true,
	"gpt-5.6-luna":      true,
	"gpt-5.5":           true,
	"gpt-5.4":           true,
	"gpt-5.4-mini":      true,
	"gpt-5.4-nano":      true,
	"gpt-5.3-codex":     true,
	"gpt-5.2":           true,
	"gpt-5.2-codex":     true,
	"gpt-5.1":           true,
	"gpt-5.1-codex-max": true,
	"gpt-5.1-codex":     true,
	"gpt-5":             true,
	"gpt-5-mini":        true,
	"gpt-5-codex":       true,
	"gpt-4.1":           true,
	"gpt-4.1-mini":      true,
	"gpt-4.1-nano":      true,
	"gpt-4o":            true,
	"gpt-4o-2024-05-13": true,
	"gpt-4o-mini":       true,
	"o3":                true,
	"o4-mini":           true,
}

// modelSupportsTier 检查模型是否在 service_tier 白名单中。
// 支持日期后缀变体：gpt-5.4-2026-03-01 匹配 gpt-5.4。
// 非日期后缀（如 -pro、-nano）不会误匹配。
func modelSupportsTier(model string) bool {
	m := strings.ToLower(model)
	if serviceTierModels[m] {
		return true
	}
	// 逐段剥离日期后缀（纯数字段），尝试匹配白名单
	for {
		idx := strings.LastIndex(m, "-")
		if idx <= 0 {
			break
		}
		suffix := m[idx+1:]
		if len(suffix) == 0 || suffix[0] < '0' || suffix[0] > '9' {
			break // 非日期后缀，停止
		}
		m = m[:idx]
		if serviceTierModels[m] {
			return true
		}
	}
	return false
}

// OpenAIServiceTierMultiplier 返回 OpenAI service_tier 的费用倍率。
// priority=2x（加钱降延迟）, flex=0.5x（便宜但慢）, fast=2.5x(gpt-5.5)/2x(gpt-5.4), default/""=1x（标准）。
// 仅当响应中携带 service_tier 字段时才生效。
func OpenAIServiceTierMultiplier(model, serviceTier string) float64 {
	if serviceTier == "" || serviceTier == "default" {
		return 1.0
	}
	if !modelSupportsTier(model) {
		return 1.0
	}
	switch serviceTier {
	case "priority":
		return 2.0
	case "flex":
		return 0.5
	case "fast":
		// gpt-5.5 fast = 2.5× base, gpt-5.4 fast = 2× base
		lm := strings.ToLower(model)
		if strings.HasPrefix(lm, "gpt-5.5") {
			return 2.5
		}
		if strings.HasPrefix(lm, "gpt-5.4") {
			return 2.0
		}
		return 1.0
	default:
		return 1.0
	}
}

// isOpusModel 判断是否为Claude Opus系列模型
// Opus模型缓存定价与Sonnet/Haiku不同：无折扣(100%基础输入价格)
// 参考：https://docs.claude.com/en/docs/about-claude/pricing
func isOpusModel(model string) bool {
	lowerModel := strings.ToLower(model)
	return strings.Contains(lowerModel, "opus")
}

// IsFastModeModel 判断模型是否支持 Anthropic fast mode
// 当前仅 claude-opus-4-6 支持 fast mode（2.5x输出速度，独立定价）
func IsFastModeModel(model string) bool {
	lowerModel := strings.ToLower(model)
	return strings.HasPrefix(lowerModel, "claude-opus-4-6")
}

// CalculateFastModeCost 计算 Anthropic fast mode 的独立费用
// Fast mode 的 input/output 使用全上下文统一定价（无 >200K 加价）。
// 缓存倍率（read 0.1 / 5m 1.25 / 1h 2.0）按定义相对「基础 input 价」，
// 故缓存成本基于基础价 $5 而非 fast 价 $30，与标准路径 CalculateCostDetailed 一致。
// 参考: https://docs.anthropic.com/en/docs/about-claude/pricing
func CalculateFastModeCost(inputTokens, outputTokens, cacheReadTokens, cache5mTokens, cache1hTokens int) float64 {
	if inputTokens < 0 || outputTokens < 0 || cacheReadTokens < 0 || cache5mTokens < 0 || cache1hTokens < 0 {
		return 0.0
	}

	// Fast mode 固定价格（全上下文统一，无 >200K 分段）
	const inputPrice = 30.0   // $30/MTok（仅 input/output）
	const outputPrice = 150.0 // $150/MTok
	// 缓存倍率常量相对「基础 input 价」定义，缓存成本须基于基础价而非 fast 价
	const baseInputPrice = 5.0 // claude-opus-4-6 基础 input 价 $5/MTok

	cost := float64(inputTokens)*inputPrice/1e6 + float64(outputTokens)*outputPrice/1e6

	// 缓存成本基于基础 input 价（倍率常量的定义基准）
	if cacheReadTokens > 0 {
		cost += float64(cacheReadTokens) * baseInputPrice * cacheReadMultiplierOpus / 1e6
	}
	if cache5mTokens > 0 {
		cost += float64(cache5mTokens) * baseInputPrice * cacheWrite5mMultiplier / 1e6
	}
	if cache1hTokens > 0 {
		cost += float64(cache1hTokens) * baseInputPrice * cacheWrite1hMultiplier / 1e6
	}

	return cost
}

// getOpenAICacheMultiplier 获取OpenAI模型的缓存价格倍数
// OpenAI缓存定价策略（2025-12官方）：
//   - GPT-5系列: 90%折扣（缓存=$0.125/1M, input=$1.25/1M → 0.1倍）
//   - GPT-4.1/o3/o4系列: 75%折扣（缓存=$0.50/1M, input=$2.00/1M → 0.25倍）
//   - GPT-4o/o1系列: 50%折扣（缓存=$1.25/1M, input=$2.50/1M → 0.5倍）
//
// 参考: https://openai.com/api/pricing/
func getOpenAICacheMultiplier(model string) float64 {
	lowerModel := strings.ToLower(model)

	// GPT-5系列: 90%折扣 (0.1倍)
	if strings.HasPrefix(lowerModel, "gpt-5") {
		return 0.1
	}

	// GPT-4.1系列: 75%折扣 (0.25倍)
	if strings.HasPrefix(lowerModel, "gpt-4.1") {
		return 0.25
	}

	// o3/o4系列（除o3-mini外）: 75%折扣 (0.25倍)
	if strings.HasPrefix(lowerModel, "o3") && !strings.Contains(lowerModel, "mini") {
		return 0.25
	}
	if strings.HasPrefix(lowerModel, "o4") {
		return 0.25
	}

	// codex-mini-latest: 75%折扣 (0.25倍)
	if strings.HasPrefix(lowerModel, "codex-mini") {
		return 0.25
	}

	// GPT-4o系列/o1系列/o3-mini/o1-mini: 50%折扣 (0.5倍)
	// 这是默认值，涵盖:
	//   - gpt-4o, gpt-4o-mini
	//   - o1, o1-mini, o1-pro
	//   - o3-mini
	return 0.5
}
