package util

import (
	"sort"
	"strings"
	"sync/atomic"
)

// BillingModelSearchCall 是 Codex /v1/alpha/search 的按次计费标识。
// 一次成功调用 = 1 search_call = $0.01。
const BillingModelSearchCall = "search_call"

// ModelPricing AI模型定价（单位：美元/百万tokens）
type ModelPricing struct {
	InputPrice         float64 // 基础输入token价格（$/1M tokens, ≤200k context for Gemini）
	OutputPrice        float64 // 输出token价格（$/1M tokens, ≤200k context for Gemini）
	TokenPricingTiers  []TokenPricingTier
	CacheReadPrice     float64 // 显式缓存读取价格（$/1M tokens）
	CacheReadPriceHigh float64 // 高上下文显式缓存读取价格（$/1M tokens）
	HasCacheReadPrice  bool    // 是否使用显式缓存读取价格；false 时按模型系列倍率回退计算

	// 缓存读取 token 是否参与高/低档选择。
	// OpenAI context tiers、MiMo / Grok 等系列按「input + cache_read」总量分档，需置 true；
	// Gemini 长上下文分档只看非缓存 prompt size，缓存读不得推高分档，保持 false。
	CacheReadCountsTowardTier bool

	// 长上下文定价（>200k tokens，Claude/Gemini/xAI）
	// 如果为0，表示无分段定价，使用InputPrice/OutputPrice
	InputPriceHigh  float64 // 高上下文输入价格（$/1M tokens, >200k context）
	OutputPriceHigh float64 // 高上下文输出价格（$/1M tokens, >200k context）

	// 固定按次计费（图像生成等非token计费模型）
	// 如果 > 0，当token成本为0时使用此值作为每次请求成本
	FixedCostPerRequest float64
}

// TokenPricingTier 按输入 token 数选择整次请求的 token 单价。
// MaxInputTokens 为该档的闭区间上限；0 表示无上限。
type TokenPricingTier struct {
	MaxInputTokens    int
	InputPrice        float64
	OutputPrice       float64
	CacheReadPrice    float64
	HasCacheReadPrice bool
}

var (
	gpt56SolTiers = []TokenPricingTier{
		{MaxInputTokens: 272_000, InputPrice: 5.00, OutputPrice: 30.00, CacheReadPrice: 0.50, HasCacheReadPrice: true},
		{InputPrice: 10.00, OutputPrice: 45.00, CacheReadPrice: 1.00, HasCacheReadPrice: true},
	}
	gpt56TerraTiers = []TokenPricingTier{
		{MaxInputTokens: 272_000, InputPrice: 2.50, OutputPrice: 15.00, CacheReadPrice: 0.25, HasCacheReadPrice: true},
		{InputPrice: 5.00, OutputPrice: 22.50, CacheReadPrice: 0.50, HasCacheReadPrice: true},
	}
	gpt56LunaTiers = []TokenPricingTier{
		{MaxInputTokens: 272_000, InputPrice: 1.00, OutputPrice: 6.00, CacheReadPrice: 0.10, HasCacheReadPrice: true},
		{InputPrice: 2.00, OutputPrice: 9.00, CacheReadPrice: 0.20, HasCacheReadPrice: true},
	}
	qwen3MaxTiers = []TokenPricingTier{
		{MaxInputTokens: 32_000, InputPrice: 1.20, OutputPrice: 6.00},
		{MaxInputTokens: 128_000, InputPrice: 2.40, OutputPrice: 12.00},
		{MaxInputTokens: 252_000, InputPrice: 3.00, OutputPrice: 15.00},
	}
	qwenFlashTiers = []TokenPricingTier{
		{MaxInputTokens: 256_000, InputPrice: 0.05, OutputPrice: 0.40},
		{MaxInputTokens: 1_000_000, InputPrice: 0.25, OutputPrice: 2.00},
	}
	qwen3VLPlusTiers = []TokenPricingTier{
		{MaxInputTokens: 32_000, InputPrice: 0.20, OutputPrice: 1.60},
		{MaxInputTokens: 128_000, InputPrice: 0.30, OutputPrice: 2.40},
		{MaxInputTokens: 256_000, InputPrice: 0.60, OutputPrice: 4.80},
	}
	qwen3VLFlashTiers = []TokenPricingTier{
		{MaxInputTokens: 32_000, InputPrice: 0.05, OutputPrice: 0.40},
		{MaxInputTokens: 128_000, InputPrice: 0.075, OutputPrice: 0.60},
		{MaxInputTokens: 256_000, InputPrice: 0.12, OutputPrice: 0.96},
	}
	qwen3CoderPlusTiers = []TokenPricingTier{
		{MaxInputTokens: 32_000, InputPrice: 1.00, OutputPrice: 5.00},
		{MaxInputTokens: 128_000, InputPrice: 1.80, OutputPrice: 9.00},
		{MaxInputTokens: 256_000, InputPrice: 3.00, OutputPrice: 15.00},
		{MaxInputTokens: 1_000_000, InputPrice: 6.00, OutputPrice: 60.00},
	}
	qwen3CoderFlashTiers = []TokenPricingTier{
		{MaxInputTokens: 32_000, InputPrice: 0.30, OutputPrice: 1.50},
		{MaxInputTokens: 128_000, InputPrice: 0.50, OutputPrice: 2.50},
		{MaxInputTokens: 256_000, InputPrice: 0.80, OutputPrice: 4.00},
		{MaxInputTokens: 1_000_000, InputPrice: 1.60, OutputPrice: 9.60},
	}
	qwen3CoderNextTiers = []TokenPricingTier{
		{MaxInputTokens: 32_000, InputPrice: 0.30, OutputPrice: 1.50},
		{MaxInputTokens: 128_000, InputPrice: 0.50, OutputPrice: 2.50},
		{MaxInputTokens: 256_000, InputPrice: 0.80, OutputPrice: 4.00},
	}
	qwen3Coder480BTiers = []TokenPricingTier{
		{MaxInputTokens: 32_000, InputPrice: 1.50, OutputPrice: 7.50},
		{MaxInputTokens: 128_000, InputPrice: 2.70, OutputPrice: 13.50},
		{MaxInputTokens: 200_000, InputPrice: 4.50, OutputPrice: 22.50},
	}
	qwen3Coder30BTiers = []TokenPricingTier{
		{MaxInputTokens: 32_000, InputPrice: 0.45, OutputPrice: 2.25},
		{MaxInputTokens: 128_000, InputPrice: 0.75, OutputPrice: 3.75},
		{MaxInputTokens: 200_000, InputPrice: 1.20, OutputPrice: 6.00},
	}

	grok45Pricing = ModelPricing{
		InputPrice: 2.00, OutputPrice: 6.00, CacheReadPrice: 0.50, HasCacheReadPrice: true,
		InputPriceHigh: 4.00, OutputPriceHigh: 12.00, CacheReadPriceHigh: 1.00,
		CacheReadCountsTowardTier: true,
	}
	grok420Pricing = ModelPricing{
		InputPrice: 1.25, OutputPrice: 2.50, CacheReadPrice: 0.20, HasCacheReadPrice: true,
		InputPriceHigh: 2.50, OutputPriceHigh: 5.00, CacheReadPriceHigh: 0.40,
		CacheReadCountsTowardTier: true,
	}
	grokBuildPricing = ModelPricing{
		InputPrice: 1.00, OutputPrice: 2.00, CacheReadPrice: 0.20, HasCacheReadPrice: true,
		InputPriceHigh: 2.00, OutputPriceHigh: 4.00, CacheReadPriceHigh: 0.40,
		CacheReadCountsTowardTier: true,
	}
)

// basePricing 基础定价表（无重复，每个模型只定义一次）
// 数据来源：
// - Claude: https://docs.claude.com/en/docs/about-claude/pricing
// - OpenAI: https://openai.com/api/pricing/
// - Gemini: https://ai.google.dev/gemini-api/docs/pricing
var basePricing = map[string]ModelPricing{
	// ========== Claude 模型 ==========
	"claude-sonnet-5":   {InputPrice: 3.00, OutputPrice: 15.00}, // 同 claude-sonnet-4-6
	"claude-sonnet-4-6": {InputPrice: 3.00, OutputPrice: 15.00}, // 全1M窗口统一价格
	"claude-sonnet-4-5": {
		InputPrice: 3.00, OutputPrice: 15.00,
		InputPriceHigh: 6.00, OutputPriceHigh: 22.50, // >200k context
	},
	"claude-sonnet-4-0": {
		InputPrice: 3.00, OutputPrice: 15.00,
		InputPriceHigh: 6.00, OutputPriceHigh: 22.50, // >200k context
	},
	"claude-haiku-4-5":  {InputPrice: 1.00, OutputPrice: 5.00},
	"claude-opus-4-1":   {InputPrice: 15.00, OutputPrice: 75.00},
	"claude-opus-4-0":   {InputPrice: 15.00, OutputPrice: 75.00},
	"claude-opus-4-6":   {InputPrice: 5.00, OutputPrice: 25.00},  // 全1M窗口统一价格
	"claude-opus-4-7":   {InputPrice: 5.00, OutputPrice: 25.00},  // 全1M窗口统一价格
	"claude-opus-4-8":   {InputPrice: 5.00, OutputPrice: 25.00},  // 全1M窗口统一价格
	"claude-fable-5":    {InputPrice: 10.00, OutputPrice: 50.00}, // claude-opus-4-8 两倍
	"claude-opus-4-5":   {InputPrice: 5.00, OutputPrice: 25.00},
	"claude-3-7-sonnet": {InputPrice: 3.00, OutputPrice: 15.00},
	"claude-3-5-sonnet": {InputPrice: 3.00, OutputPrice: 15.00},
	"claude-3-5-haiku":  {InputPrice: 0.80, OutputPrice: 4.00},
	"claude-3-opus":     {InputPrice: 15.00, OutputPrice: 75.00},
	"claude-3-sonnet":   {InputPrice: 3.00, OutputPrice: 15.00},
	"claude-3-haiku":    {InputPrice: 0.25, OutputPrice: 1.25},
	// 通用兜底（未来新版本）
	"claude-opus":   {InputPrice: 5.00, OutputPrice: 25.00},
	"claude-sonnet": {InputPrice: 3.00, OutputPrice: 15.00},
	"claude-haiku":  {InputPrice: 1.00, OutputPrice: 5.00},

	// ========== OpenAI GPT-5系列 ==========
	"gpt-5.6": {
		InputPrice: 5.00, OutputPrice: 30.00, CacheReadPrice: 0.50, HasCacheReadPrice: true,
		TokenPricingTiers: gpt56SolTiers, CacheReadCountsTowardTier: true,
	},
	"gpt-5.6-sol": {
		InputPrice: 5.00, OutputPrice: 30.00, CacheReadPrice: 0.50, HasCacheReadPrice: true,
		TokenPricingTiers: gpt56SolTiers, CacheReadCountsTowardTier: true,
	},
	"gpt-5.6-terra": {
		InputPrice: 2.50, OutputPrice: 15.00, CacheReadPrice: 0.25, HasCacheReadPrice: true,
		TokenPricingTiers: gpt56TerraTiers, CacheReadCountsTowardTier: true,
	},
	"gpt-5.6-luna": {
		InputPrice: 1.00, OutputPrice: 6.00, CacheReadPrice: 0.10, HasCacheReadPrice: true,
		TokenPricingTiers: gpt56LunaTiers, CacheReadCountsTowardTier: true,
	},
	"gpt-5.5": {
		InputPrice: 5.00, OutputPrice: 30.00,
		InputPriceHigh: 10.00, OutputPriceHigh: 45.00, // >272K context; 2× gpt-5.4
	},
	"gpt-5.4": {
		InputPrice: 2.50, OutputPrice: 15.00,
		InputPriceHigh: 5.00, OutputPriceHigh: 22.50, // >272K context
	},
	"gpt-5.4-pro": {
		InputPrice: 30.00, OutputPrice: 180.00,
		InputPriceHigh: 60.00, OutputPriceHigh: 270.00, // >272K context
	},
	"gpt-5.4-mini":        {InputPrice: 0.75, OutputPrice: 4.50},
	"gpt-5.4-nano":        {InputPrice: 0.20, OutputPrice: 1.25},
	"gpt-5.3":             {InputPrice: 1.75, OutputPrice: 14.00},
	"gpt-5.3-codex":       {InputPrice: 1.75, OutputPrice: 14.00},
	"gpt-5.3-codex-spark": {InputPrice: 1.75, OutputPrice: 14.00},
	"gpt-5.2":             {InputPrice: 1.75, OutputPrice: 14.00},
	"gpt-5.2-chat-latest": {InputPrice: 1.75, OutputPrice: 14.00},
	"gpt-5.2-pro":         {InputPrice: 21.00, OutputPrice: 168.00},
	"gpt-5.1":             {InputPrice: 1.25, OutputPrice: 10.00},
	"gpt-5.1-chat-latest": {InputPrice: 1.25, OutputPrice: 10.00},
	"gpt-5.1-codex-max":   {InputPrice: 1.25, OutputPrice: 10.00},
	"gpt-5.1-codex":       {InputPrice: 1.25, OutputPrice: 10.00},
	"gpt-5.1-codex-mini":  {InputPrice: 0.25, OutputPrice: 2.00},
	"gpt-5":               {InputPrice: 1.25, OutputPrice: 10.00},
	"gpt-5-chat-latest":   {InputPrice: 1.25, OutputPrice: 10.00},
	"gpt-5-codex":         {InputPrice: 1.25, OutputPrice: 10.00},
	"gpt-5-search-api":    {InputPrice: 1.25, OutputPrice: 10.00},
	"gpt-5-mini":          {InputPrice: 0.25, OutputPrice: 2.00},
	"gpt-5-nano":          {InputPrice: 0.05, OutputPrice: 0.40},
	"gpt-5-pro":           {InputPrice: 15.00, OutputPrice: 120.00},

	// ========== OpenAI GPT-4系列 ==========
	"gpt-4.1":                    {InputPrice: 2.00, OutputPrice: 8.00},
	"gpt-4.1-mini":               {InputPrice: 0.40, OutputPrice: 1.60},
	"gpt-4.1-nano":               {InputPrice: 0.10, OutputPrice: 0.40},
	"gpt-4o":                     {InputPrice: 2.50, OutputPrice: 10.00},
	"gpt-4o-2024-05-13":          {InputPrice: 5.00, OutputPrice: 15.00},
	"gpt-4o-legacy":              {InputPrice: 5.00, OutputPrice: 15.00}, // 旧版模糊匹配
	"gpt-4o-mini":                {InputPrice: 0.15, OutputPrice: 0.60},
	"gpt-4o-search-preview":      {InputPrice: 2.50, OutputPrice: 10.00},
	"gpt-4o-mini-search-preview": {InputPrice: 0.15, OutputPrice: 0.60},
	"gpt-4-turbo":                {InputPrice: 10.00, OutputPrice: 30.00},
	"gpt-4":                      {InputPrice: 30.00, OutputPrice: 60.00},
	"gpt-4-32k":                  {InputPrice: 60.00, OutputPrice: 120.00},
	"gpt-3.5-turbo":              {InputPrice: 0.50, OutputPrice: 1.50},
	"gpt-3.5-legacy":             {InputPrice: 1.50, OutputPrice: 2.00},
	"gpt-3.5-16k":                {InputPrice: 3.00, OutputPrice: 4.00},

	// ========== OpenAI Realtime/Audio ==========
	"gpt-realtime":                 {InputPrice: 4.00, OutputPrice: 16.00},
	"gpt-realtime-mini":            {InputPrice: 0.60, OutputPrice: 2.40},
	"gpt-4o-realtime-preview":      {InputPrice: 5.00, OutputPrice: 20.00},
	"gpt-4o-mini-realtime-preview": {InputPrice: 0.60, OutputPrice: 2.40},
	"gpt-audio":                    {InputPrice: 2.50, OutputPrice: 10.00},
	"gpt-audio-mini":               {InputPrice: 0.60, OutputPrice: 2.40},
	"gpt-4o-audio-preview":         {InputPrice: 2.50, OutputPrice: 10.00},
	"gpt-4o-mini-audio-preview":    {InputPrice: 0.15, OutputPrice: 0.60},

	// ========== OpenAI Image ==========
	"gpt-image-1.5":        {InputPrice: 5.00, OutputPrice: 10.00},
	"chatgpt-image-latest": {InputPrice: 5.00, OutputPrice: 10.00},
	"gpt-image-1":          {InputPrice: 5.00, OutputPrice: 0.00},
	"gpt-image-1-mini":     {InputPrice: 2.00, OutputPrice: 0.00},

	// ========== OpenAI o系列 ==========
	"o1":                    {InputPrice: 15.00, OutputPrice: 60.00},
	"o1-pro":                {InputPrice: 150.00, OutputPrice: 600.00},
	"o1-mini":               {InputPrice: 1.10, OutputPrice: 4.40},
	"o3":                    {InputPrice: 2.00, OutputPrice: 8.00},
	"o3-pro":                {InputPrice: 20.00, OutputPrice: 80.00},
	"o3-mini":               {InputPrice: 1.10, OutputPrice: 4.40},
	"o3-deep-research":      {InputPrice: 10.00, OutputPrice: 40.00},
	"o4-mini":               {InputPrice: 1.10, OutputPrice: 4.40},
	"o4-mini-deep-research": {InputPrice: 2.00, OutputPrice: 8.00},

	// ========== OpenAI 其他 ==========
	"computer-use-preview": {InputPrice: 3.00, OutputPrice: 12.00},
	"codex-mini-latest":    {InputPrice: 1.50, OutputPrice: 6.00},
	"davinci-002":          {InputPrice: 2.00, OutputPrice: 2.00},
	"babbage-002":          {InputPrice: 0.40, OutputPrice: 0.40},

	// ========== Gemini 模型 ==========
	"gemini-3.5-flash": {InputPrice: 1.50, OutputPrice: 9.00, CacheReadPrice: 0.15, HasCacheReadPrice: true},
	"gemini-3-5-flash": {InputPrice: 1.50, OutputPrice: 9.00, CacheReadPrice: 0.15, HasCacheReadPrice: true},
	"gemini-3.1-pro": {
		InputPrice: 2.00, OutputPrice: 12.00, CacheReadPrice: 0.20, HasCacheReadPrice: true,
		InputPriceHigh: 4.00, OutputPriceHigh: 18.00, CacheReadPriceHigh: 0.40,
	},
	"gemini-3-pro": {
		InputPrice: 2.00, OutputPrice: 12.00,
		InputPriceHigh: 4.00, OutputPriceHigh: 18.00,
	},
	"gemini-3-flash":        {InputPrice: 0.50, OutputPrice: 3.00},
	"gemini-3.1-flash-lite": {InputPrice: 0.25, OutputPrice: 1.50},
	"gemini-2.5-pro": {
		InputPrice: 1.25, OutputPrice: 10.00,
		InputPriceHigh: 2.50, OutputPriceHigh: 15.00,
	},
	"gemini-2.5-flash":      {InputPrice: 0.30, OutputPrice: 2.50},
	"gemini-2.5-flash-lite": {InputPrice: 0.10, OutputPrice: 0.40},
	"gemini-2.0-flash":      {InputPrice: 0.10, OutputPrice: 0.40},
	"gemini-2.0-flash-lite": {InputPrice: 0.075, OutputPrice: 0.30},
	"gemini-1.5-pro":        {InputPrice: 1.25, OutputPrice: 5.00},
	"gemini-1.5-flash":      {InputPrice: 0.20, OutputPrice: 0.60},

	// ========== 智谱 GLM 模型 ==========
	// 来源：https://docs.z.ai/guides/overview/pricing
	"glm-5":               {InputPrice: 1.00, OutputPrice: 3.20, CacheReadPrice: 0.20, HasCacheReadPrice: true},
	"glm-5.2":             {InputPrice: 1.40, OutputPrice: 4.40, CacheReadPrice: 0.26, HasCacheReadPrice: true},
	"glm-5.1":             {InputPrice: 1.40, OutputPrice: 4.40, CacheReadPrice: 0.26, HasCacheReadPrice: true},
	"glm-5-turbo":         {InputPrice: 1.20, OutputPrice: 4.00, CacheReadPrice: 0.24, HasCacheReadPrice: true},
	"glm-5-code":          {InputPrice: 1.20, OutputPrice: 5.00, CacheReadPrice: 0.30, HasCacheReadPrice: true},
	"glm-4.7":             {InputPrice: 0.60, OutputPrice: 2.20, CacheReadPrice: 0.11, HasCacheReadPrice: true},
	"glm-4.7-flashx":      {InputPrice: 0.07, OutputPrice: 0.40, CacheReadPrice: 0.01, HasCacheReadPrice: true},
	"glm-4.7-flash":       {InputPrice: 0.00, OutputPrice: 0.00}, // 免费
	"glm-4.6":             {InputPrice: 0.60, OutputPrice: 2.20, CacheReadPrice: 0.11, HasCacheReadPrice: true},
	"glm-4.6v":            {InputPrice: 0.30, OutputPrice: 0.90},
	"glm-ocr":             {InputPrice: 0.03, OutputPrice: 0.03},
	"glm-4.6v-flashx":     {InputPrice: 0.04, OutputPrice: 0.40},
	"glm-4.6v-flash":      {InputPrice: 0.00, OutputPrice: 0.00}, // 免费
	"glm-4.5":             {InputPrice: 0.60, OutputPrice: 2.20, CacheReadPrice: 0.11, HasCacheReadPrice: true},
	"glm-4.5v":            {InputPrice: 0.60, OutputPrice: 1.80},
	"glm-4.5-x":           {InputPrice: 2.20, OutputPrice: 8.90, CacheReadPrice: 0.45, HasCacheReadPrice: true},
	"glm-4.5-air":         {InputPrice: 0.20, OutputPrice: 1.10, CacheReadPrice: 0.03, HasCacheReadPrice: true},
	"glm-4.5-airx":        {InputPrice: 1.10, OutputPrice: 4.50, CacheReadPrice: 0.22, HasCacheReadPrice: true},
	"glm-4.5-flash":       {InputPrice: 0.00, OutputPrice: 0.00}, // 免费
	"glm-4-32b-0414-128k": {InputPrice: 0.10, OutputPrice: 0.10, CacheReadPrice: 0.00, HasCacheReadPrice: true},

	// ========== Mimo 模型 ==========
	// 来源：用户提供的价格表截图（2026-04-29）
	"mimo-v2.5-pro": {
		InputPrice: 1.00, OutputPrice: 3.00, CacheReadPrice: 0.20, HasCacheReadPrice: true,
		InputPriceHigh: 2.00, OutputPriceHigh: 6.00, CacheReadPriceHigh: 0.40, // >256k input tokens
		CacheReadCountsTowardTier: true,
	},
	"mimo-v2-pro": {
		InputPrice: 1.00, OutputPrice: 3.00, CacheReadPrice: 0.20, HasCacheReadPrice: true,
		InputPriceHigh: 2.00, OutputPriceHigh: 6.00, CacheReadPriceHigh: 0.40, // >256k input tokens
		CacheReadCountsTowardTier: true,
	},
	"mimo-v2.5": {
		InputPrice: 0.40, OutputPrice: 2.00, CacheReadPrice: 0.08, HasCacheReadPrice: true,
		InputPriceHigh: 0.80, OutputPriceHigh: 4.00, CacheReadPriceHigh: 0.16, // >256k input tokens
		CacheReadCountsTowardTier: true,
	},
	"mimo-v2-omni":    {InputPrice: 0.40, OutputPrice: 2.00, CacheReadPrice: 0.08, HasCacheReadPrice: true},
	"mimo-v2.5-flash": {InputPrice: 0.10, OutputPrice: 0.30, CacheReadPrice: 0.01, HasCacheReadPrice: true},
	"mimo-v2-flash":   {InputPrice: 0.10, OutputPrice: 0.30, CacheReadPrice: 0.01, HasCacheReadPrice: true},

	// ========== Cerebras 模型 ==========
	// 来源：https://inference-docs.cerebras.ai/resources/pricing
	"zai-glm-4.7":           {InputPrice: 2.25, OutputPrice: 2.75},
	"gemma-4-31b":           {InputPrice: 0.99, OutputPrice: 1.49},
	"cerebras-gpt-oss-120b": {InputPrice: 0.35, OutputPrice: 0.75},

	// ========== Moonshot AI / Kimi 模型 ==========
	// 来源: https://api.pricepertoken.com/api/provider-pricing-history/?provider=moonshotai
	"kimi-dev-72b":                 {InputPrice: 0.29, OutputPrice: 1.15},
	"kimi-dev-72b:free":            {InputPrice: 0.00, OutputPrice: 0.00},
	"kimi-k2":                      {InputPrice: 0.57, OutputPrice: 2.30},
	"kimi-k2-0905":                 {InputPrice: 0.60, OutputPrice: 2.50, CacheReadPrice: 0.50, HasCacheReadPrice: true},
	"kimi-k2-0905:exacto":          {InputPrice: 0.60, OutputPrice: 2.50, CacheReadPrice: 0.15, HasCacheReadPrice: true},
	"kimi-k2-thinking":             {InputPrice: 0.60, OutputPrice: 2.50, CacheReadPrice: 0.15, HasCacheReadPrice: true},
	"kimi-k2.5":                    {InputPrice: 0.40, OutputPrice: 1.90, CacheReadPrice: 0.07, HasCacheReadPrice: true},
	"kimi-k2.6":                    {InputPrice: 0.73, OutputPrice: 3.40, CacheReadPrice: 0.15, HasCacheReadPrice: true},
	"kimi-k2:free":                 {InputPrice: 0.00, OutputPrice: 0.00},
	"kimi-linear-48b-a3b-instruct": {InputPrice: 0.70, OutputPrice: 0.90},
	"kimi-vl-a3b-thinking":         {InputPrice: 0.02, OutputPrice: 0.08},
	"kimi-vl-a3b-thinking:free":    {InputPrice: 0.00, OutputPrice: 0.00},

	// ========== Qwen 模型 ==========
	// 来源: 阿里云 Model Studio 官方价格页 International 部分
	// https://www.alibabacloud.com/help/en/model-studio/model-pricing
	"qwen3-max":            {TokenPricingTiers: qwen3MaxTiers},
	"qwen3-max-2026-01-23": {TokenPricingTiers: qwen3MaxTiers},
	"qwen3-max-2025-09-23": {TokenPricingTiers: qwen3MaxTiers},
	"qwen3-max-preview":    {TokenPricingTiers: qwen3MaxTiers},
	"qwen-max":             {InputPrice: 1.60, OutputPrice: 6.40},
	"qwen-max-latest":      {InputPrice: 1.60, OutputPrice: 6.40},
	"qwen-max-2025-01-25":  {InputPrice: 1.60, OutputPrice: 6.40},
	"qwen3.5-plus": {
		InputPrice: 0.40, OutputPrice: 2.40,
		InputPriceHigh: 0.50, OutputPriceHigh: 3.00, // >256k input tokens
	},
	"qwen3.5-plus-2026-02-15": {
		InputPrice: 0.40, OutputPrice: 2.40,
		InputPriceHigh: 0.50, OutputPriceHigh: 3.00, // >256k input tokens
	},
	"qwen-plus": {
		InputPrice: 0.40, OutputPrice: 1.20,
		InputPriceHigh: 1.20, OutputPriceHigh: 3.60, // >256k input tokens
	},
	"qwen-plus-latest": {
		InputPrice: 0.40, OutputPrice: 1.20,
		InputPriceHigh: 1.20, OutputPriceHigh: 3.60, // >256k input tokens
	},
	"qwen-plus-2025-12-01": {
		InputPrice: 0.40, OutputPrice: 1.20,
		InputPriceHigh: 1.20, OutputPriceHigh: 3.60, // >256k input tokens
	},
	"qwen-plus-2025-09-11": {
		InputPrice: 0.40, OutputPrice: 1.20,
		InputPriceHigh: 1.20, OutputPriceHigh: 3.60, // >256k input tokens
	},
	"qwen-plus-2025-07-28": {
		InputPrice: 0.40, OutputPrice: 1.20,
		InputPriceHigh: 1.20, OutputPriceHigh: 3.60, // >256k input tokens
	},
	"qwen-plus:thinking": {
		InputPrice: 0.40, OutputPrice: 4.00,
		InputPriceHigh: 1.20, OutputPriceHigh: 12.00, // >256k input tokens
	},
	"qwen-plus-latest:thinking": {
		InputPrice: 0.40, OutputPrice: 4.00,
		InputPriceHigh: 1.20, OutputPriceHigh: 12.00, // >256k input tokens
	},
	"qwen-plus-2025-12-01:thinking": {
		InputPrice: 0.40, OutputPrice: 4.00,
		InputPriceHigh: 1.20, OutputPriceHigh: 12.00, // >256k input tokens
	},
	"qwen-plus-2025-09-11:thinking": {
		InputPrice: 0.40, OutputPrice: 4.00,
		InputPriceHigh: 1.20, OutputPriceHigh: 12.00, // >256k input tokens
	},
	"qwen-plus-2025-07-28:thinking": {
		InputPrice: 0.40, OutputPrice: 4.00,
		InputPriceHigh: 1.20, OutputPriceHigh: 12.00, // >256k input tokens
	},
	"qwen-plus-2025-07-14":          {InputPrice: 0.40, OutputPrice: 1.20},
	"qwen-plus-2025-07-14:thinking": {InputPrice: 0.40, OutputPrice: 4.00},
	"qwen-plus-2025-04-28":          {InputPrice: 0.40, OutputPrice: 1.20},
	"qwen-plus-2025-04-28:thinking": {InputPrice: 0.40, OutputPrice: 4.00},
	"qwen-plus-2025-01-25":          {InputPrice: 0.40, OutputPrice: 1.20},
	"qwen3.5-flash":                 {InputPrice: 0.10, OutputPrice: 0.40},
	"qwen3.5-flash-2026-02-23":      {InputPrice: 0.10, OutputPrice: 0.40},
	"qwen-flash":                    {TokenPricingTiers: qwenFlashTiers},
	"qwen-flash-2025-07-28":         {TokenPricingTiers: qwenFlashTiers},
	"qwen-turbo":                    {InputPrice: 0.05, OutputPrice: 0.20},
	"qwen-turbo-latest":             {InputPrice: 0.05, OutputPrice: 0.20},
	"qwen-turbo-2025-04-28":         {InputPrice: 0.05, OutputPrice: 0.20},
	"qwen-turbo-2024-11-01":         {InputPrice: 0.05, OutputPrice: 0.20},
	"qwen-vl-max":                   {InputPrice: 0.80, OutputPrice: 3.20},
	"qwen-vl-max-latest":            {InputPrice: 0.80, OutputPrice: 3.20},
	"qwen-vl-max-2025-08-13":        {InputPrice: 0.80, OutputPrice: 3.20},
	"qwen-vl-max-2025-04-08":        {InputPrice: 0.80, OutputPrice: 3.20},
	"qwen-vl-plus":                  {InputPrice: 0.21, OutputPrice: 0.63},
	"qwen-vl-plus-latest":           {InputPrice: 0.21, OutputPrice: 0.63},
	"qwen-vl-plus-2025-08-15":       {InputPrice: 0.21, OutputPrice: 0.63},
	"qwen-vl-plus-2025-05-07":       {InputPrice: 0.21, OutputPrice: 0.63},
	"qwen-vl-plus-2025-01-25":       {InputPrice: 0.21, OutputPrice: 0.63},
	"qwen3-vl-plus":                 {TokenPricingTiers: qwen3VLPlusTiers},
	"qwen3-vl-plus-2025-12-19":      {TokenPricingTiers: qwen3VLPlusTiers},
	"qwen3-vl-plus-2025-09-23":      {TokenPricingTiers: qwen3VLPlusTiers},
	"qwen3-vl-flash":                {TokenPricingTiers: qwen3VLFlashTiers},
	"qwen3-vl-flash-2026-01-22":     {TokenPricingTiers: qwen3VLFlashTiers},
	"qwen3-vl-flash-2025-10-15":     {TokenPricingTiers: qwen3VLFlashTiers},
	"qwen3-coder-plus":              {TokenPricingTiers: qwen3CoderPlusTiers},
	"qwen3-coder-plus-2025-09-23":   {TokenPricingTiers: qwen3CoderPlusTiers},
	"qwen3-coder-plus-2025-07-22":   {TokenPricingTiers: qwen3CoderPlusTiers},
	"qwen3-coder-flash":             {TokenPricingTiers: qwen3CoderFlashTiers},
	"qwen3-coder-flash-2025-07-28":  {TokenPricingTiers: qwen3CoderFlashTiers},
	"qwen3-coder-next":              {TokenPricingTiers: qwen3CoderNextTiers},
	"qwen3-coder-480b-a35b-instruct": {
		TokenPricingTiers: qwen3Coder480BTiers,
	},
	"qwen3-coder-30b-a3b-instruct": {
		TokenPricingTiers: qwen3Coder30BTiers,
	},
	"qwen3-next-80b-a3b-thinking":   {InputPrice: 0.15, OutputPrice: 1.20},
	"qwen3-next-80b-a3b-instruct":   {InputPrice: 0.15, OutputPrice: 1.20},
	"qwen3-235b-a22b-thinking-2507": {InputPrice: 0.23, OutputPrice: 2.30},
	"qwen3-235b-a22b-instruct-2507": {InputPrice: 0.23, OutputPrice: 0.92},
	"qwen3-235b-a22b-2507":          {InputPrice: 0.23, OutputPrice: 0.92},
	"qwen3-30b-a3b-thinking-2507":   {InputPrice: 0.20, OutputPrice: 2.40},
	"qwen3-30b-a3b-instruct-2507":   {InputPrice: 0.20, OutputPrice: 0.80},
	"qwen3-235b-a22b":               {InputPrice: 0.70, OutputPrice: 2.80},
	"qwen3-235b-a22b:thinking":      {InputPrice: 0.70, OutputPrice: 8.40},
	"qwen3-32b":                     {InputPrice: 0.16, OutputPrice: 0.64},
	"qwen3-30b-a3b":                 {InputPrice: 0.20, OutputPrice: 0.80},
	"qwen3-30b-a3b:thinking":        {InputPrice: 0.20, OutputPrice: 2.40},
	"qwen3-14b":                     {InputPrice: 0.35, OutputPrice: 1.40},
	"qwen3-8b":                      {InputPrice: 0.18, OutputPrice: 0.70},
	"qwen3-4b":                      {InputPrice: 0.11, OutputPrice: 0.42},
	"qwen3-1.7b":                    {InputPrice: 0.11, OutputPrice: 0.42},
	"qwen3-0.6b":                    {InputPrice: 0.11, OutputPrice: 0.42},
	"qwen3.5-397b-a17b":             {InputPrice: 0.60, OutputPrice: 3.60},
	"qwen3.5-122b-a10b":             {InputPrice: 0.40, OutputPrice: 3.20},
	"qwen3.5-27b":                   {InputPrice: 0.30, OutputPrice: 2.40},
	"qwen3.5-35b-a3b":               {InputPrice: 0.25, OutputPrice: 2.00},
	"qwen2.5-14b-instruct-1m":       {InputPrice: 0.805, OutputPrice: 3.22},
	"qwen2.5-7b-instruct-1m":        {InputPrice: 0.368, OutputPrice: 1.47},
	"qwen2.5-72b-instruct":          {InputPrice: 1.40, OutputPrice: 5.60},
	"qwen2.5-32b-instruct":          {InputPrice: 0.70, OutputPrice: 2.80},
	"qwen2.5-14b-instruct":          {InputPrice: 0.35, OutputPrice: 1.40},
	"qwen2.5-7b-instruct":           {InputPrice: 0.175, OutputPrice: 0.70},
	"qwen3-vl-235b-a22b-thinking":   {InputPrice: 0.40, OutputPrice: 4.00},
	"qwen3-vl-235b-a22b-instruct":   {InputPrice: 0.40, OutputPrice: 1.60},
	"qwen3-vl-32b-thinking":         {InputPrice: 0.16, OutputPrice: 0.64},
	"qwen3-vl-32b-instruct":         {InputPrice: 0.16, OutputPrice: 0.64},
	"qwen3-vl-30b-a3b-thinking":     {InputPrice: 0.20, OutputPrice: 2.40},
	"qwen3-vl-30b-a3b-instruct":     {InputPrice: 0.20, OutputPrice: 0.80},
	"qwen3-vl-8b-thinking":          {InputPrice: 0.18, OutputPrice: 2.10},
	"qwen3-vl-8b-instruct":          {InputPrice: 0.18, OutputPrice: 0.70},
	"qwen2.5-vl-72b-instruct":       {InputPrice: 2.80, OutputPrice: 8.40},
	"qwen2.5-vl-32b-instruct":       {InputPrice: 1.40, OutputPrice: 4.20},
	"qwen2.5-vl-7b-instruct":        {InputPrice: 0.35, OutputPrice: 1.05},
	"qwen2.5-vl-3b-instruct":        {InputPrice: 0.21, OutputPrice: 0.63},

	// 第三方/历史变体：官方 International 表无对应条目，保留现有兜底。
	"qwen-2-72b-instruct":              {InputPrice: 0.90, OutputPrice: 0.90},
	"qwen-2.5-72b-instruct:free":       {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen-2.5-coder-32b-instruct":      {InputPrice: 0.03, OutputPrice: 0.11},
	"qwen-2.5-coder-32b-instruct:free": {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen-2.5-vl-7b-instruct:free":     {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen2.5-coder-7b-instruct":        {InputPrice: 0.03, OutputPrice: 0.09},
	"qwen2.5-vl-32b-instruct:free":     {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen2.5-vl-72b-instruct:free":     {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-14b:free":                   {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-235b-a22b-2507:free":        {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-235b-a22b:free":             {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-30b-a3b:free":               {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-4b:free":                    {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-8b:free":                    {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-coder":                      {InputPrice: 0.22, OutputPrice: 1.00},
	"qwen3-coder:exacto":               {InputPrice: 0.22, OutputPrice: 1.80},
	"qwen3-coder:free":                 {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-next-80b-a3b-instruct:free": {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3.6-plus": {
		InputPrice: 0.50, OutputPrice: 3.00,
		InputPriceHigh: 2.00, OutputPriceHigh: 6.00, // legacy >256k input tokens
	},
	"qwen3.6-plus-2026-04-02": {
		InputPrice: 0.50, OutputPrice: 3.00,
		InputPriceHigh: 2.00, OutputPriceHigh: 6.00, // legacy >256k input tokens
	},
	"qwen3.6-plus:free":         {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3.6-plus-preview:free": {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-max-thinking":        {TokenPricingTiers: qwen3MaxTiers},
	"qwq-32b":                   {InputPrice: 0.15, OutputPrice: 0.25},
	"qwq-32b-preview":           {InputPrice: 0.20, OutputPrice: 0.20},
	"qwq-32b:free":              {InputPrice: 0.00, OutputPrice: 0.00},

	// ========== DeepSeek 模型 ==========
	"deepseek-r1-distill-llama-70b": {InputPrice: 0.70, OutputPrice: 0.80},
	"deepseek-r1-distill-llama-8b":  {InputPrice: 0.04, OutputPrice: 0.04},
	"deepseek-r1-0528-qwen3-8b":     {InputPrice: 0.06, OutputPrice: 0.09},
	"deepseek-r1-distill-qwen-14b":  {InputPrice: 0.15, OutputPrice: 0.15},
	"deepseek-r1-distill-qwen-32b":  {InputPrice: 0.29, OutputPrice: 0.29},
	"deepseek-r1-distill-qwen-7b":   {InputPrice: 0.10, OutputPrice: 0.20},
	"deepseek-r1-distill-qwen-1.5b": {InputPrice: 0.18, OutputPrice: 0.18},
	"deepseek-r1":                   {InputPrice: 0.70, OutputPrice: 2.50},
	"deepseek-r1-0528":              {InputPrice: 0.50, OutputPrice: 2.15, CacheReadPrice: 0.35, HasCacheReadPrice: true},
	"deepseek-chat":                 {InputPrice: 0.32, OutputPrice: 0.89},
	"deepseek-chat-v3-0324":         {InputPrice: 0.20, OutputPrice: 0.77, CacheReadPrice: 0.11, HasCacheReadPrice: true},
	"deepseek-chat-v3.1":            {InputPrice: 0.21, OutputPrice: 0.79, CacheReadPrice: 0.13, HasCacheReadPrice: true},
	"deepseek-v3-base":              {InputPrice: 0.20, OutputPrice: 0.80},
	"deepseek-v3.1-base":            {InputPrice: 0.25, OutputPrice: 1.00},
	"deepseek-v3.1-terminus":        {InputPrice: 0.27, OutputPrice: 0.95, CacheReadPrice: 0.13, HasCacheReadPrice: true},
	"deepseek-v3.2":                 {InputPrice: 0.252, OutputPrice: 0.378, CacheReadPrice: 0.0252, HasCacheReadPrice: true},
	"deepseek-v3.2-exp":             {InputPrice: 0.27, OutputPrice: 0.41, CacheReadPrice: 0.27, HasCacheReadPrice: true},
	"deepseek-v3.2-speciale":        {InputPrice: 0.287, OutputPrice: 0.431, CacheReadPrice: 0.058, HasCacheReadPrice: true},
	"deepseek-v4-flash":             {InputPrice: 0.112, OutputPrice: 0.224, CacheReadPrice: 0.0028, HasCacheReadPrice: true},
	"deepseek-v4-pro":               {InputPrice: 0.435, OutputPrice: 0.87, CacheReadPrice: 0.0036, HasCacheReadPrice: true},
	"deepseek-prover-v2":            {InputPrice: 0.50, OutputPrice: 2.18},

	// ========== xAI Grok 模型 ==========
	// 来源: https://docs.x.ai/developers/pricing
	"grok-4.5":                     grok45Pricing,
	"grok-4.3":                     grok420Pricing,
	"grok-4.20":                    grok420Pricing,
	"grok-4.20-0309-reasoning":     grok420Pricing,
	"grok-4.20-0309-non-reasoning": grok420Pricing,
	"grok-4.20-multi-agent-0309":   grok420Pricing,
	"grok-4.20-beta":               grok420Pricing,
	"grok-4.20-multi-agent":        grok420Pricing,
	"grok-4.20-multi-agent-beta":   grok420Pricing,
	"grok-4.1-fast":                {InputPrice: 0.20, OutputPrice: 0.50, CacheReadPrice: 0.05, HasCacheReadPrice: true},
	"grok-4":                       {InputPrice: 3.00, OutputPrice: 15.00, CacheReadPrice: 0.75, HasCacheReadPrice: true},
	"grok-4-fast":                  {InputPrice: 0.20, OutputPrice: 0.50, CacheReadPrice: 0.05, HasCacheReadPrice: true},
	"grok-build-0.1":               grokBuildPricing,
	"grok-3":                       {InputPrice: 3.00, OutputPrice: 15.00, CacheReadPrice: 0.75, HasCacheReadPrice: true},
	"grok-3-beta":                  {InputPrice: 3.00, OutputPrice: 15.00, CacheReadPrice: 0.75, HasCacheReadPrice: true},
	"grok-3-mini":                  {InputPrice: 0.30, OutputPrice: 0.50, CacheReadPrice: 0.075, HasCacheReadPrice: true},
	"grok-3-mini-beta":             {InputPrice: 0.30, OutputPrice: 0.50, CacheReadPrice: 0.075, HasCacheReadPrice: true},
	"grok-2":                       {InputPrice: 2.00, OutputPrice: 10.00},
	"grok-2-1212":                  {InputPrice: 2.00, OutputPrice: 10.00},
	"grok-2-vision-1212":           {InputPrice: 2.00, OutputPrice: 10.00},
	"grok-2-mini":                  {InputPrice: 0.20, OutputPrice: 0.50},
	"grok-code-fast":               grokBuildPricing,
	"grok-code-fast-1":             grokBuildPricing,
	"grok-vision-beta":             {InputPrice: 5.00, OutputPrice: 15.00},

	// xAI Grok 图像生成模型（按张计费，非token计费）
	// 来源: https://docs.x.ai/developers/pricing
	"grok-2-image-1212":          {FixedCostPerRequest: 0.07},
	"grok-imagine-image":         {FixedCostPerRequest: 0.02},
	"grok-imagine-image-quality": {FixedCostPerRequest: 0.05},
	"grok-imagine-image-pro":     {FixedCostPerRequest: 0.07},

	// Codex Alpha Search：按次计费（1 search_call = $0.01）
	BillingModelSearchCall: {FixedCostPerRequest: 0.01},

	// ========== MiniMax 模型 ==========
	// 来源: https://api.pricepertoken.com/api/provider-pricing-history/?provider=minimax
	"minimax-01":     {InputPrice: 0.20, OutputPrice: 1.10},
	"minimax-m1":     {InputPrice: 0.40, OutputPrice: 2.20},
	"minimax-m2":     {InputPrice: 0.255, OutputPrice: 1.00, CacheReadPrice: 0.03, HasCacheReadPrice: true},
	"minimax-m2-her": {InputPrice: 0.30, OutputPrice: 1.20, CacheReadPrice: 0.03, HasCacheReadPrice: true},
	"minimax-m2.1":   {InputPrice: 0.29, OutputPrice: 0.95, CacheReadPrice: 0.03, HasCacheReadPrice: true},
	"minimax-m2.5":   {InputPrice: 0.15, OutputPrice: 0.90, CacheReadPrice: 0.027, HasCacheReadPrice: true},
	"minimax-m2.7":   {InputPrice: 0.279, OutputPrice: 1.20, CacheReadPrice: 0.059, HasCacheReadPrice: true},
	"minimax-m3": {
		InputPrice: 0.30, OutputPrice: 1.20, CacheReadPrice: 0.06, HasCacheReadPrice: true,
		InputPriceHigh: 0.60, OutputPriceHigh: 2.40, CacheReadPriceHigh: 0.12,
	},

	// ========== 美团 LongCat 模型 ==========
	// 来源: https://api.pricepertoken.com/api/provider-pricing-history/?provider=meituan
	"longcat-flash-chat":          {InputPrice: 0.20, OutputPrice: 0.80, CacheReadPrice: 0.20, HasCacheReadPrice: true},
	"longcat-flash-chat:free":     {InputPrice: 0.00, OutputPrice: 0.00},
	"longcat-flash-thinking":      {InputPrice: 0.20, OutputPrice: 0.80},
	"longcat-flash-thinking-2601": {InputPrice: 0.20, OutputPrice: 0.80},
	"longcat-flash-lite":          {InputPrice: 0.00, OutputPrice: 0.00}, // 公测免费
	"longcat-flash-omni-2603":     {InputPrice: 0.20, OutputPrice: 0.80},
	"longcat-flash-chat-2602-exp": {InputPrice: 0.20, OutputPrice: 0.80},

	// ========== Meta Llama 模型 ==========
	// 来源: https://api.pricepertoken.com/api/provider-pricing-history/?provider=meta-llama
	"llama-3.2-3b-instruct":         {InputPrice: 0.0509, OutputPrice: 0.335},
	"llama-3.2-1b-instruct":         {InputPrice: 0.027, OutputPrice: 0.201},
	"llama-3.1-8b-instruct":         {InputPrice: 0.02, OutputPrice: 0.05, CacheReadPrice: 0.025, HasCacheReadPrice: true},
	"llama-guard-3-8b":              {InputPrice: 0.484, OutputPrice: 0.03},
	"llama-3-8b-instruct":           {InputPrice: 0.04, OutputPrice: 0.04},
	"llama-3.3-70b-instruct":        {InputPrice: 0.10, OutputPrice: 0.32, CacheReadPrice: 0.11, HasCacheReadPrice: true},
	"llama-3.2-11b-vision-instruct": {InputPrice: 0.245, OutputPrice: 0.245},
	"llama-guard-4-12b":             {InputPrice: 0.18, OutputPrice: 0.18},
	"llama-4-scout":                 {InputPrice: 0.08, OutputPrice: 0.30},
	"llama-3.1-70b-instruct":        {InputPrice: 0.40, OutputPrice: 0.40, CacheReadPrice: 0.80, HasCacheReadPrice: true},
	"llama-4-maverick":              {InputPrice: 0.15, OutputPrice: 0.60, CacheReadPrice: 0.17, HasCacheReadPrice: true},
	"llama-guard-2-8b":              {InputPrice: 0.20, OutputPrice: 0.20},
	"llama-3-70b-instruct":          {InputPrice: 0.51, OutputPrice: 0.74},
	"llama-3.2-90b-vision-instruct": {InputPrice: 0.35, OutputPrice: 0.40},
	"llama-3.1-405b-instruct":       {InputPrice: 4.00, OutputPrice: 4.00},
	"llama-3.1-405b":                {InputPrice: 4.00, OutputPrice: 4.00},

	// ========== OpenAI OSS 模型 ==========
	// 来源: https://api.pricepertoken.com/api/provider-pricing-history/?provider=openai
	"gpt-oss-20b":           {InputPrice: 0.03, OutputPrice: 0.14, CacheReadPrice: 0.02, HasCacheReadPrice: true},
	"gpt-oss-120b":          {InputPrice: 0.039, OutputPrice: 0.18, CacheReadPrice: 0.055, HasCacheReadPrice: true},
	"gpt-oss-120b:exacto":   {InputPrice: 0.039, OutputPrice: 0.19, CacheReadPrice: 0.04, HasCacheReadPrice: true},
	"gpt-oss-safeguard-20b": {InputPrice: 0.075, OutputPrice: 0.30, CacheReadPrice: 0.037, HasCacheReadPrice: true},
}

// modelAliases 模型别名映射（多对一）
// key: 别名, value: basePricing中的基础模型名
var modelAliases = map[string]string{
	// Claude别名
	"claude-sonnet-4-5-20250929": "claude-sonnet-4-5",
	"claude-haiku-4-5-20251001":  "claude-haiku-4-5",
	"claude-opus-4-1-20250805":   "claude-opus-4-1",
	"claude-sonnet-4-20250514":   "claude-sonnet-4-0",
	"claude-opus-4-20250514":     "claude-opus-4-0",
	"claude-3-7-sonnet-20250219": "claude-3-7-sonnet",
	"claude-3-7-sonnet-latest":   "claude-3-7-sonnet",
	"claude-3-5-sonnet-20241022": "claude-3-5-sonnet",
	"claude-3-5-sonnet-20240620": "claude-3-5-sonnet",
	"claude-3-5-sonnet-latest":   "claude-3-5-sonnet",
	"claude-3-5-haiku-20241022":  "claude-3-5-haiku",
	"claude-3-5-haiku-latest":    "claude-3-5-haiku",
	"claude-3-opus-20240229":     "claude-3-opus",
	"claude-3-opus-latest":       "claude-3-opus",
	"claude-3-sonnet-20240229":   "claude-3-sonnet",
	"claude-3-sonnet-latest":     "claude-3-sonnet",
	"claude-3-haiku-20240307":    "claude-3-haiku",
	"claude-3-haiku-latest":      "claude-3-haiku",

	// OpenAI GPT别名
	"gpt-5.1":                    "gpt-5",
	"gpt-5.1-chat-latest":        "gpt-5",
	"gpt-5-chat-latest":          "gpt-5",
	"gpt-5.1-codex":              "gpt-5",
	"gpt-5-codex":                "gpt-5",
	"gpt-5.1-codex-mini":         "gpt-5-mini",
	"gpt-5-search-api":           "gpt-5",
	"gpt-4o-2024-05-13":          "gpt-4o-legacy",
	"chatgpt-4o-latest":          "gpt-4o-legacy",
	"gpt-4o-mini-search-preview": "gpt-4o-mini",
	"gpt-4o-search-preview":      "gpt-4o",
	"gpt-4-turbo-2024-04-09":     "gpt-4-turbo",
	"gpt-4-0125-preview":         "gpt-4-turbo",
	"gpt-4-1106-preview":         "gpt-4-turbo",
	"gpt-4-1106-vision-preview":  "gpt-4-turbo",
	"gpt-4-0613":                 "gpt-4",
	"gpt-4-0314":                 "gpt-4",
	"gpt-4-32k-0613":             "gpt-4-32k",
	"gpt-3.5-turbo-0125":         "gpt-3.5-turbo",
	"gpt-3.5-turbo-1106":         "gpt-3.5-legacy",
	"gpt-3.5-turbo-0613":         "gpt-3.5-legacy",
	"gpt-3.5-0301":               "gpt-3.5-legacy",
	"gpt-3.5-turbo-instruct":     "gpt-3.5-legacy",
	"gpt-3.5-turbo-16k-0613":     "gpt-3.5-16k",

	// o系列别名
	"o4-mini-deep-research": "o3-deep-research", // 相同定价

	// Gemini Claude 别名（第三方封装）
	"gemini-claude-opus-4-6-thinking":   "claude-opus-4-6",
	"gemini-claude-opus-4-5-thinking":   "claude-opus-4-5",
	"gemini-claude-sonnet-4-5-thinking": "claude-sonnet-4-5",
	"gemini-claude-sonnet-4-5":          "claude-sonnet-4-5",

	// DeepSeek 别名
	"deepseek-v3": "deepseek-chat",

	// xAI 别名
	"grok-4.3-latest":       "grok-4.3",
	"grok-4.5-latest":       "grok-4.5",
	"grok-beta":             "grok-3",
	"grok-build-latest":     "grok-4.5",
	"grok-code-fast":        "grok-build-0.1",
	"grok-code-fast-1":      "grok-build-0.1",
	"grok-code-fast-1-0825": "grok-build-0.1",
	"grok-latest":           "grok-4.3",

	// Qwen 别名（常见命名变体）
	"qwen-3.5-plus":                  "qwen3.5-plus",
	"qwen-3.5-plus-2026-02-15":       "qwen3.5-plus-2026-02-15",
	"qwen-3.6-plus":                  "qwen3.6-plus",
	"qwen-3.6-plus-2026-04-02":       "qwen3.6-plus-2026-04-02",
	"qwen-3-32b":                     "qwen3-32b",
	"qwen-3-4b":                      "qwen3-4b",
	"qwen-3-8b":                      "qwen3-8b",
	"qwen-3-14b":                     "qwen3-14b",
	"qwen-3-235b-a22b-instruct-2507": "qwen3-235b-a22b-instruct-2507",
	"qwen-2.5-72b-instruct":          "qwen2.5-72b-instruct",
	"qwen-2.5-7b-instruct":           "qwen2.5-7b-instruct",
	"qwen-2.5-vl-7b-instruct":        "qwen2.5-vl-7b-instruct",

	// GLM 别名
	"zai-glm-4.6": "glm-4.6",

	// Meta Llama 别名（Cerebras等平台命名变体）
	"llama3.1-8b":   "llama-3.1-8b-instruct",
	"llama-3.3-70b": "llama-3.3-70b-instruct",

	// OpenAI OSS / Ollama 风格 size tag（渠道重定向目标常用冒号）
	"gpt-oss:120b": "gpt-oss-120b",
	"gpt-oss:20b":  "gpt-oss-20b",

	// 渠道短名 / Ollama 冒号重定向 → 官方完整 ID（与 basePricing canonical 对齐）
	// 常见：客户端用 qwen3-vl-235b，上游用 qwen3-vl:235b
	"qwen3-vl-235b":          "qwen3-vl-235b-a22b-instruct",
	"qwen3-vl:235b":          "qwen3-vl-235b-a22b-instruct",
	"qwen3-vl-235b-instruct": "qwen3-vl-235b-a22b-instruct",
	"qwen3-vl:235b-instruct": "qwen3-vl-235b-a22b-instruct",
	"qwen3-vl-235b-thinking": "qwen3-vl-235b-a22b-thinking",
	"qwen3-vl:235b-thinking": "qwen3-vl-235b-a22b-thinking",
	"qwen3-coder-480b":       "qwen3-coder-480b-a35b-instruct",
	"qwen3-coder:480b":       "qwen3-coder-480b-a35b-instruct",
	"qwen3-coder:next":       "qwen3-coder-next",
}

// modelPrefixMatch 将可匹配的前缀解析为实际定价条目。
type modelPrefixMatch struct {
	prefix string
	target string
}

type modelPricingSnapshot struct {
	pricing             map[string]ModelPricing
	aliases             map[string]string
	prefixBuckets       map[byte][]modelPrefixMatch
	metadata            map[string]ModelCatalogEntry
	remoteETag          string
	remoteSkippedModels int
}

var activeModelPricing atomic.Pointer[modelPricingSnapshot]

func init() {
	activeModelPricing.Store(buildModelPricingSnapshot(nil))
}

func buildModelPricingSnapshot(catalog *ModelCatalogSnapshot) *modelPricingSnapshot {
	pricing := make(map[string]ModelPricing, len(basePricing))
	for id, entry := range basePricing {
		pricing[id] = cloneModelPricing(entry)
	}

	aliases := make(map[string]string, len(modelAliases))
	for alias, target := range modelAliases {
		aliases[alias] = target
	}

	metadata := make(map[string]ModelCatalogEntry)
	remoteETag := ""
	remoteSkippedModels := 0
	if catalog != nil {
		remoteETag = catalog.ETag
		remoteSkippedModels = catalog.SkippedModels
		for _, entry := range catalog.Models {
			// 远端精确模型 ID 是当前目录的权威值，不能再被同名本地别名重定向。
			delete(aliases, entry.ID)
			pricing[entry.ID] = overlayRemotePricing(pricing[entry.ID], entry.Pricing)
			metadata[entry.ID] = cloneModelCatalogEntry(entry)
		}
	}

	return &modelPricingSnapshot{
		pricing:             pricing,
		aliases:             aliases,
		prefixBuckets:       buildPrefixBuckets(pricing, aliases),
		metadata:            metadata,
		remoteETag:          remoteETag,
		remoteSkippedModels: remoteSkippedModels,
	}
}

func overlayRemotePricing(embedded, remote ModelPricing) ModelPricing {
	// models.dev 只表达基础 token 单价、显式 cache-read 单价和 context tiers。
	// 先复制内置项，避免远端省略字段时清掉本地计费语义。
	overlay := cloneModelPricing(embedded)
	overlay.InputPrice = remote.InputPrice
	overlay.OutputPrice = remote.OutputPrice
	if remote.HasCacheReadPrice {
		overlay.CacheReadPrice = remote.CacheReadPrice
		overlay.HasCacheReadPrice = true
	}
	if len(remote.TokenPricingTiers) > 0 {
		overlay.TokenPricingTiers = append([]TokenPricingTier(nil), remote.TokenPricingTiers...)
		overlay.CacheReadCountsTowardTier = remote.CacheReadCountsTowardTier
		overlay.InputPriceHigh = 0
		overlay.OutputPriceHigh = 0
		overlay.CacheReadPriceHigh = 0
	}
	return overlay
}

func cloneModelPricing(pricing ModelPricing) ModelPricing {
	pricing.TokenPricingTiers = append([]TokenPricingTier(nil), pricing.TokenPricingTiers...)
	return pricing
}

func buildPrefixBuckets(pricing map[string]ModelPricing, aliases map[string]string) map[byte][]modelPrefixMatch {
	matches := make([]modelPrefixMatch, 0, len(pricing)+len(aliases))
	for id := range pricing {
		matches = append(matches, modelPrefixMatch{prefix: id, target: id})
	}
	for alias, target := range aliases {
		matches = append(matches, modelPrefixMatch{prefix: alias, target: target})
	}
	sort.Slice(matches, func(i, j int) bool {
		if len(matches[i].prefix) != len(matches[j].prefix) {
			return len(matches[i].prefix) > len(matches[j].prefix)
		}
		if matches[i].prefix != matches[j].prefix {
			return matches[i].prefix < matches[j].prefix
		}
		return matches[i].target < matches[j].target
	})

	buckets := make(map[byte][]modelPrefixMatch, 16)
	for _, match := range matches {
		if match.prefix == "" {
			continue
		}
		buckets[match.prefix[0]] = append(buckets[match.prefix[0]], match)
	}
	return buckets
}

// getPricing 获取模型定价（先查别名再查基础表）。
// 支持 Ollama/vLLM 风格 size tag：gpt-oss:120b → gpt-oss-120b（精确冒号条目如 gpt-oss-120b:exacto 优先保留）。
func getPricing(model string) (ModelPricing, bool) {
	snapshot := activeModelPricing.Load()
	if snapshot == nil {
		return ModelPricing{}, false
	}
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return ModelPricing{}, false
	}
	if pricing, ok := lookupPricingInSnapshot(snapshot, model); ok {
		return pricing, true
	}
	// 冒号 size tag → 连字符（仅在精确/别名未命中时）
	if alt := colonSizeTagToHyphen(model); alt != "" {
		return lookupPricingInSnapshot(snapshot, alt)
	}
	return ModelPricing{}, false
}

func lookupPricingInSnapshot(snapshot *modelPricingSnapshot, model string) (ModelPricing, bool) {
	if base, ok := snapshot.aliases[model]; ok {
		model = base
	}
	pricing, ok := snapshot.pricing[model]
	return pricing, ok
}

// colonSizeTagToHyphen 将最后一个 ':' 换成 '-'（gpt-oss:120b → gpt-oss-120b）。
// 已是精确表项的冒号 ID 不会走到这里（getPricing 先精确匹配）。
func colonSizeTagToHyphen(model string) string {
	i := strings.LastIndexByte(model, ':')
	if i <= 0 || i == len(model)-1 {
		return ""
	}
	return model[:i] + "-" + model[i+1:]
}

// fuzzyMatchModel 模糊匹配模型名称。
// 例如：claude-3-opus-20240229-extended → claude-3-opus
//
//	gpt-4o-2024-12-01 → gpt-4o
func fuzzyMatchModel(model string) (ModelPricing, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return ModelPricing{}, false
	}
	if pricing, ok := fuzzyMatchModelRaw(model); ok {
		return pricing, true
	}
	if alt := colonSizeTagToHyphen(model); alt != "" {
		return fuzzyMatchModelRaw(alt)
	}
	return ModelPricing{}, false
}

func fuzzyMatchModelRaw(lowerModel string) (ModelPricing, bool) {
	snapshot := activeModelPricing.Load()
	if snapshot == nil || lowerModel == "" {
		return ModelPricing{}, false
	}
	bucket, ok := snapshot.prefixBuckets[lowerModel[0]]
	if !ok {
		return ModelPricing{}, false
	}
	for _, match := range bucket {
		if strings.HasPrefix(lowerModel, match.prefix) {
			if pricing, ok := snapshot.pricing[match.target]; ok {
				return pricing, true
			}
		}
	}
	return ModelPricing{}, false
}

// HasModelPricing 判断模型是否能解析到定价（精确/别名/冒号归一/前缀模糊）。
func HasModelPricing(model string) bool {
	if _, ok := getPricing(model); ok {
		return true
	}
	_, ok := fuzzyMatchModel(model)
	return ok
}

// ResolveBillingModel 选择用于计费的模型 ID。
// 优先 actual（上游/重定向后，价格可能不同）；若 actual 无定价而 request 有，则回退 request。
// 这样渠道「模型名 → 重定向目标」配置下，可用第一列（有定价的客户端名）覆盖无定价的上游 ID。
func ResolveBillingModel(actual, request string) string {
	actual = strings.TrimSpace(actual)
	request = strings.TrimSpace(request)
	if actual == "" {
		return request
	}
	if HasModelPricing(actual) {
		return actual
	}
	if request != "" && request != actual && HasModelPricing(request) {
		return request
	}
	return actual
}
