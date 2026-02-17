package util

import (
	"log"
	"strings"
)

// ============================================================================
// AI API 成本计算器（Claude + OpenAI）
// ============================================================================

// ModelPricing AI模型定价（单位：美元/百万tokens）
type ModelPricing struct {
	InputPrice  float64 // 基础输入token价格（$/1M tokens, ≤200k context for Gemini）
	OutputPrice float64 // 输出token价格（$/1M tokens, ≤200k context for Gemini）

	// 长上下文定价（>200k tokens，Claude/Gemini）
	// 如果为0，表示无分段定价，使用InputPrice/OutputPrice
	InputPriceHigh  float64 // 高上下文输入价格（$/1M tokens, >200k context）
	OutputPriceHigh float64 // 高上下文输出价格（$/1M tokens, >200k context）
}

// basePricing 基础定价表（无重复，每个模型只定义一次）
// 数据来源：
// - Claude: https://docs.claude.com/en/docs/about-claude/pricing
// - OpenAI: https://openai.com/api/pricing/
// - Gemini: https://ai.google.dev/gemini-api/docs/pricing
var basePricing = map[string]ModelPricing{
	// ========== Claude 模型 ==========
	"claude-sonnet-4-5": {
		InputPrice: 3.00, OutputPrice: 15.00,
		InputPriceHigh: 6.00, OutputPriceHigh: 22.50, // >200k context
	},
	"claude-sonnet-4-0": {
		InputPrice: 3.00, OutputPrice: 15.00,
		InputPriceHigh: 6.00, OutputPriceHigh: 22.50, // >200k context
	},
	"claude-haiku-4-5": {InputPrice: 1.00, OutputPrice: 5.00},
	"claude-opus-4-1":  {InputPrice: 15.00, OutputPrice: 75.00},
	"claude-opus-4-0":  {InputPrice: 15.00, OutputPrice: 75.00},
	"claude-opus-4-6": {
		InputPrice: 5.00, OutputPrice: 25.00,
		InputPriceHigh: 10.00, OutputPriceHigh: 37.50, // >200k context
	},
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
	"gpt-5.3":             {InputPrice: 1.75, OutputPrice: 14.00},
	"gpt-5.3-codex":       {InputPrice: 1.75, OutputPrice: 14.00},
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
	"gemini-3-pro": {
		InputPrice: 2.00, OutputPrice: 12.00,
		InputPriceHigh: 4.00, OutputPriceHigh: 18.00,
	},
	"gemini-3-flash": {InputPrice: 0.50, OutputPrice: 3.00},
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
	"glm-5":               {InputPrice: 1.00, OutputPrice: 3.20},
	"glm-5-code":          {InputPrice: 1.20, OutputPrice: 5.00},
	"glm-4.7":             {InputPrice: 0.60, OutputPrice: 2.20},
	"glm-4.7-flashx":      {InputPrice: 0.07, OutputPrice: 0.40},
	"glm-4.7-flash":       {InputPrice: 0.00, OutputPrice: 0.00}, // 免费
	"glm-4.6":             {InputPrice: 0.60, OutputPrice: 2.20},
	"glm-4.6v":            {InputPrice: 0.30, OutputPrice: 0.90},
	"glm-ocr":             {InputPrice: 0.03, OutputPrice: 0.03},
	"glm-4.6v-flashx":     {InputPrice: 0.04, OutputPrice: 0.40},
	"glm-4.6v-flash":      {InputPrice: 0.00, OutputPrice: 0.00}, // 免费
	"glm-4.5":             {InputPrice: 0.60, OutputPrice: 2.20},
	"glm-4.5v":            {InputPrice: 0.60, OutputPrice: 1.80},
	"glm-4.5-x":           {InputPrice: 2.20, OutputPrice: 8.90},
	"glm-4.5-air":         {InputPrice: 0.20, OutputPrice: 1.10},
	"glm-4.5-airx":        {InputPrice: 1.10, OutputPrice: 4.50},
	"glm-4.5-flash":       {InputPrice: 0.00, OutputPrice: 0.00}, // 免费
	"glm-4-32b-0414-128k": {InputPrice: 0.10, OutputPrice: 0.10},

	// ========== Mimo 模型 ==========
	"mimo-v2-flash": {InputPrice: 0.10, OutputPrice: 0.30},

	// ========== Qwen 模型 ==========
	// 来源: https://api.pricepertoken.com/api/provider-pricing-history/?provider=qwen
	"qwen-2-72b-instruct":              {InputPrice: 0.90, OutputPrice: 0.90},
	"qwen-2.5-72b-instruct":            {InputPrice: 0.12, OutputPrice: 0.39},
	"qwen-2.5-72b-instruct:free":       {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen-2.5-7b-instruct":             {InputPrice: 0.04, OutputPrice: 0.10},
	"qwen-2.5-coder-32b-instruct":      {InputPrice: 0.03, OutputPrice: 0.11},
	"qwen-2.5-coder-32b-instruct:free": {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen-2.5-vl-7b-instruct":          {InputPrice: 0.20, OutputPrice: 0.20},
	"qwen-2.5-vl-7b-instruct:free":     {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen-max":                         {InputPrice: 1.60, OutputPrice: 6.40},
	// qwen3.5-plus（按表格：输入分档0.4/1.2，输出分档2.4/7.2）
	"qwen3.5-plus": {
		InputPrice: 0.40, OutputPrice: 2.40,
		InputPriceHigh: 1.20, OutputPriceHigh: 7.20, // >256k input tokens
	},
	"qwen3.5-plus-2026-02-15": {
		InputPrice: 0.40, OutputPrice: 2.40,
		InputPriceHigh: 1.20, OutputPriceHigh: 7.20, // >256k input tokens
	},
	// qwen-plus（按表格 non-thinking 列）
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
	// thinking 版本按表格 thinking 列计费
	"qwen-plus-2025-07-28:thinking": {
		InputPrice: 0.40, OutputPrice: 4.00,
		InputPriceHigh: 1.20, OutputPriceHigh: 12.00, // >256k input tokens
	},
	// 历史无分档版本
	"qwen-plus-2025-07-14":             {InputPrice: 0.40, OutputPrice: 1.20},
	"qwen-plus-2025-04-28":             {InputPrice: 0.40, OutputPrice: 1.20},
	"qwen-plus-2025-01-25":             {InputPrice: 0.40, OutputPrice: 1.20},
	"qwen-turbo":                       {InputPrice: 0.05, OutputPrice: 0.20},
	"qwen-vl-max":                      {InputPrice: 0.80, OutputPrice: 3.20},
	"qwen-vl-plus":                     {InputPrice: 0.21, OutputPrice: 0.63},
	"qwen2.5-coder-7b-instruct":        {InputPrice: 0.03, OutputPrice: 0.09},
	"qwen2.5-vl-32b-instruct":          {InputPrice: 0.05, OutputPrice: 0.22},
	"qwen2.5-vl-32b-instruct:free":     {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen2.5-vl-72b-instruct":          {InputPrice: 0.15, OutputPrice: 0.60},
	"qwen2.5-vl-72b-instruct:free":     {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-14b":                        {InputPrice: 0.05, OutputPrice: 0.22},
	"qwen3-14b:free":                   {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-235b-a22b":                  {InputPrice: 0.30, OutputPrice: 1.20},
	"qwen3-235b-a22b-2507":             {InputPrice: 0.071, OutputPrice: 0.10},
	"qwen3-235b-a22b-2507:free":        {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-235b-a22b-thinking-2507":    {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-235b-a22b:free":             {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-30b-a3b":                    {InputPrice: 0.06, OutputPrice: 0.22},
	"qwen3-30b-a3b-instruct-2507":      {InputPrice: 0.08, OutputPrice: 0.30},
	"qwen3-30b-a3b-thinking-2507":      {InputPrice: 0.051, OutputPrice: 0.30},
	"qwen3-30b-a3b:free":               {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-32b":                        {InputPrice: 0.08, OutputPrice: 0.24},
	"qwen3-4b":                         {InputPrice: 0.0715, OutputPrice: 0.273},
	"qwen3-4b:free":                    {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-8b":                         {InputPrice: 0.05, OutputPrice: 0.40},
	"qwen3-8b:free":                    {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-coder":                      {InputPrice: 0.22, OutputPrice: 1.00},
	"qwen3-coder-30b-a3b-instruct":     {InputPrice: 0.07, OutputPrice: 0.27},
	"qwen3-coder-flash":                {InputPrice: 0.30, OutputPrice: 1.50},
	"qwen3-coder-next":                 {InputPrice: 0.07, OutputPrice: 0.30},
	"qwen3-coder-plus":                 {InputPrice: 1.00, OutputPrice: 5.00},
	"qwen3-coder:exacto":               {InputPrice: 0.22, OutputPrice: 1.80},
	"qwen3-coder:free":                 {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-max":                        {InputPrice: 1.20, OutputPrice: 6.00},
	"qwen3-max-thinking":               {InputPrice: 1.20, OutputPrice: 6.00},
	"qwen3-next-80b-a3b-instruct":      {InputPrice: 0.09, OutputPrice: 0.78},
	"qwen3-next-80b-a3b-instruct:free": {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-next-80b-a3b-thinking":      {InputPrice: 0.15, OutputPrice: 0.30},
	"qwen3-vl-235b-a22b-instruct":      {InputPrice: 0.20, OutputPrice: 0.88},
	"qwen3-vl-235b-a22b-thinking":      {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-vl-30b-a3b-instruct":        {InputPrice: 0.13, OutputPrice: 0.52},
	"qwen3-vl-30b-a3b-thinking":        {InputPrice: 0.00, OutputPrice: 0.00},
	"qwen3-vl-32b-instruct":            {InputPrice: 0.104, OutputPrice: 0.416},
	"qwen3-vl-8b-instruct":             {InputPrice: 0.08, OutputPrice: 0.455},
	"qwen3-vl-8b-thinking":             {InputPrice: 0.117, OutputPrice: 1.365},
	"qwq-32b":                          {InputPrice: 0.15, OutputPrice: 0.25},
	"qwq-32b-preview":                  {InputPrice: 0.20, OutputPrice: 0.20},
	"qwq-32b:free":                     {InputPrice: 0.00, OutputPrice: 0.00},

	// ========== DeepSeek 模型 ==========
	"deepseek-r1-distill-llama-70b": {InputPrice: 0.03, OutputPrice: 0.11},
	"deepseek-r1-0528-qwen3-8b":     {InputPrice: 0.048, OutputPrice: 0.072},
	"deepseek-r1-distill-qwen-14b":  {InputPrice: 0.12, OutputPrice: 0.12},
	"deepseek-r1":                   {InputPrice: 0.30, OutputPrice: 1.20},
	"deepseek-chat":                 {InputPrice: 0.30, OutputPrice: 1.20},
	"deepseek-v3.2-exp":             {InputPrice: 0.25, OutputPrice: 0.38},
	"deepseek-v3.1-terminus":        {InputPrice: 0.21, OutputPrice: 0.79},
	"deepseek-r1-distill-qwen-32b":  {InputPrice: 0.24, OutputPrice: 0.24},
	"deepseek-v3.2":                 {InputPrice: 0.25, OutputPrice: 0.38},
	"deepseek-v3.2-speciale":        {InputPrice: 0.27, OutputPrice: 0.41},
	"deepseek-r1-0528":              {InputPrice: 0.40, OutputPrice: 1.75},
	"deepseek-prover-v2":            {InputPrice: 0.50, OutputPrice: 2.18},

	// ========== xAI Grok 模型 ==========
	// 来源: https://api.pricepertoken.com/api/provider-pricing-history/?provider=xai
	"grok-4.1-fast":      {InputPrice: 0.50, OutputPrice: 1.50},
	"grok-4":             {InputPrice: 3.00, OutputPrice: 15.00},
	"grok-4-fast":        {InputPrice: 0.20, OutputPrice: 0.50},
	"grok-3":             {InputPrice: 3.00, OutputPrice: 15.00},
	"grok-3-beta":        {InputPrice: 3.00, OutputPrice: 15.00},
	"grok-3-mini":        {InputPrice: 0.30, OutputPrice: 0.50},
	"grok-3-mini-beta":   {InputPrice: 0.30, OutputPrice: 0.50},
	"grok-2":             {InputPrice: 2.00, OutputPrice: 10.00},
	"grok-2-1212":        {InputPrice: 2.00, OutputPrice: 10.00},
	"grok-2-vision-1212": {InputPrice: 2.00, OutputPrice: 10.00},
	"grok-2-mini":        {InputPrice: 0.20, OutputPrice: 0.50},
	"grok-code-fast-1":   {InputPrice: 0.20, OutputPrice: 1.50},
	"grok-vision-beta":   {InputPrice: 5.00, OutputPrice: 15.00},

	// ========== MiniMax 模型 ==========
	// 来源: https://api.pricepertoken.com/api/provider-pricing-history/?provider=minimax
	"minimax-01":   {InputPrice: 0.20, OutputPrice: 1.10},
	"minimax-m1":   {InputPrice: 0.30, OutputPrice: 1.65},
	"minimax-m2":   {InputPrice: 0.15, OutputPrice: 0.45},
	"minimax-m2.1": {InputPrice: 0.30, OutputPrice: 1.20},
	"minimax-m2.5": {InputPrice: 0.30, OutputPrice: 1.20},

	// ========== Meta Llama 模型 ==========
	// 来源: https://api.pricepertoken.com/api/provider-pricing-history/?provider=meta-llama
	"llama-3.2-3b-instruct":         {InputPrice: 0.003, OutputPrice: 0.006},
	"llama-3.2-1b-instruct":         {InputPrice: 0.005, OutputPrice: 0.01},
	"llama-3.1-8b-instruct":         {InputPrice: 0.015, OutputPrice: 0.02},
	"llama-guard-3-8b":              {InputPrice: 0.02, OutputPrice: 0.03},
	"llama-3-8b-instruct":           {InputPrice: 0.03, OutputPrice: 0.04},
	"llama-3.3-70b-instruct":        {InputPrice: 0.038, OutputPrice: 0.12},
	"llama-3.2-11b-vision-instruct": {InputPrice: 0.049, OutputPrice: 0.049},
	"llama-guard-4-12b":             {InputPrice: 0.05, OutputPrice: 0.05},
	"llama-4-scout":                 {InputPrice: 0.08, OutputPrice: 0.30},
	"llama-3.1-70b-instruct":        {InputPrice: 0.10, OutputPrice: 0.28},
	"llama-4-maverick":              {InputPrice: 0.15, OutputPrice: 0.50},
	"llama-guard-2-8b":              {InputPrice: 0.20, OutputPrice: 0.20},
	"llama-3-70b-instruct":          {InputPrice: 0.30, OutputPrice: 0.40},
	"llama-3.2-90b-vision-instruct": {InputPrice: 0.35, OutputPrice: 0.40},
	"llama-3.1-405b-instruct":       {InputPrice: 0.80, OutputPrice: 0.80},
	"llama-3.1-405b":                {InputPrice: 2.00, OutputPrice: 2.00},

	// ========== OpenAI OSS 模型 ==========
	// 来源: https://api.pricepertoken.com/api/provider-pricing-history/?provider=openai
	"gpt-oss-20b":           {InputPrice: 0.05, OutputPrice: 0.20},
	"gpt-oss-120b:exacto":   {InputPrice: 0.05, OutputPrice: 0.24},
	"gpt-oss-safeguard-20b": {InputPrice: 0.075, OutputPrice: 0.30},
	"gpt-oss-120b":          {InputPrice: 0.10, OutputPrice: 0.50},
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
	"grok-beta": "grok-3",

	// Qwen 别名（常见命名变体）
	"qwen-3.5-plus":                  "qwen3.5-plus",
	"qwen-3.5-plus-2026-02-15":       "qwen3.5-plus-2026-02-15",
	"qwen-3-32b":                     "qwen3-32b",
	"qwen-3-4b":                      "qwen3-4b",
	"qwen-3-8b":                      "qwen3-8b",
	"qwen-3-14b":                     "qwen3-14b",
	"qwen-3-235b-a22b-instruct-2507": "qwen3-235b-a22b-2507",

	// GLM 别名
	"zai-glm-4.6": "glm-4.6",

	// Meta Llama 别名（Cerebras等平台命名变体）
	"llama3.1-8b":   "llama-3.1-8b-instruct",
	"llama-3.3-70b": "llama-3.3-70b-instruct",
}

// getPricing 获取模型定价（先查别名再查基础表）
func getPricing(model string) (ModelPricing, bool) {
	// 先查别名
	if base, ok := modelAliases[model]; ok {
		model = base
	}
	// 再查基础表
	p, ok := basePricing[model]
	return p, ok
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

	// cacheWrite5mMultiplier 5分钟缓存写入价格倍数（相对于基础input价格）
	// 5m Cache Write = Input Price × 1.25 (25%溢价)
	// 仅适用于Claude模型（OpenAI不支持cache_creation）
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
)

func getTierThresholdForModel(model string) int {
	lowerModel := strings.ToLower(model)
	switch {
	case strings.HasPrefix(lowerModel, "qwen3.5-plus"),
		strings.HasPrefix(lowerModel, "qwen-3.5-plus"),
		strings.HasPrefix(lowerModel, "qwen-plus"):
		return qwenPlusTierThreshold
	default:
		return geminiLongContextThreshold
	}
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
		log.Printf("ERROR: negative tokens detected (model=%s): input=%d output=%d cache_read=%d cache_5m=%d cache_1h=%d",
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

	// 分段定价逻辑（当前用于 Gemini / Qwen Plus 系列）
	// 阈值判断: 仅针对输入侧非缓存token(不包括输出,不包括缓存)
	tierThreshold := getTierThresholdForModel(model)
	useHighPricing := pricing.InputPriceHigh > 0 && inputTokens > tierThreshold

	// 选择适用的价格
	inputPricePerM := pricing.InputPrice
	outputPricePerM := pricing.OutputPrice
	if useHighPricing {
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
		cacheMultiplier := cacheReadMultiplierClaude // Claude全系/Gemini: 10%折扣
		if isOpenAIModel(model) {
			// OpenAI缓存折扣率按模型系列区分（2025-12官方定价）
			cacheMultiplier = getOpenAICacheMultiplier(model)
		} else if isOpusModel(model) {
			cacheMultiplier = cacheReadMultiplierOpus // Opus: 10%折扣
		}
		cacheReadPrice := inputPricePerM * cacheMultiplier
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

// isOpusModel 判断是否为Claude Opus系列模型
// Opus模型缓存定价与Sonnet/Haiku不同：无折扣(100%基础输入价格)
// 参考：https://docs.claude.com/en/docs/about-claude/pricing
func isOpusModel(model string) bool {
	lowerModel := strings.ToLower(model)
	return strings.Contains(lowerModel, "opus")
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

// fuzzyMatchModel 模糊匹配模型名称
// 例如：claude-3-opus-20240229-extended → claude-3-opus
//
//	gpt-4o-2024-12-01 → gpt-4o
func fuzzyMatchModel(model string) (ModelPricing, bool) {
	lowerModel := strings.ToLower(model)

	// 硬编码前缀列表（按优先级和长度排序，更具体的前缀优先）
	// 优点：比动态排序快，可预测，并发安全
	prefixes := []string{
		// Claude模型（按版本降序，具体版本优先，通用兜底在最后）
		"claude-sonnet-4-5", "claude-haiku-4-5", "claude-opus-4-6", "claude-opus-4-5", "claude-opus-4-1",
		"claude-sonnet-4-0", "claude-opus-4-0", "claude-3-7-sonnet",
		"claude-3-5-sonnet", "claude-3-5-haiku",
		"claude-3-opus", "claude-3-sonnet", "claude-3-haiku",
		"claude-opus", "claude-sonnet", "claude-haiku", // 通用兜底

		// Gemini模型（按版本降序，更长的前缀优先）
		"gemini-3-pro", "gemini-3-flash",
		"gemini-2.5-flash-lite", "gemini-2.5-flash", "gemini-2.5-pro",
		"gemini-2.0-flash-lite", "gemini-2.0-flash",
		"gemini-1.5-pro", "gemini-1.5-flash",

		// OpenAI GPT系列（更长的前缀优先，避免gpt-4o-legacy被gpt-4o截断）
		"gpt-5-pro", "gpt-5-nano", "gpt-5-mini", "gpt-5",
		"gpt-4.1-nano", "gpt-4.1-mini", "gpt-4.1",
		"gpt-4o-legacy", "gpt-4o-mini", "gpt-4o", // legacy必须在gpt-4o之前
		"gpt-4-turbo", "gpt-4-32k", "gpt-4",
		"gpt-3.5-legacy", "gpt-3.5-16k", "gpt-3.5-turbo",

		// OpenAI o系列
		"o3-deep-research", "o3-pro", "o3-mini", "o3",
		"o1-pro", "o1-mini", "o1", "o4-mini",

		// OpenAI其他专用模型
		"computer-use-preview", "codex-mini-latest",
		"davinci-002", "babbage-002",

		// 其他厂商
		"mimo-v2-flash",
		"qwen3.5-plus-2026-02-15", "qwen3.5-plus",
		"qwen-plus-2025-12-01", "qwen-plus-2025-09-11", "qwen-plus-2025-07-28:thinking", "qwen-plus-2025-07-28",
		"qwen-plus-2025-07-14", "qwen-plus-2025-04-28", "qwen-plus-2025-01-25", "qwen-plus-latest", "qwen-plus",
		"qwen-turbo", "qwen-max", "qwen-vl-plus", "qwen-vl-max",
		"qwen3-next-80b-a3b-instruct", "qwen3-next-80b-a3b-thinking",
		"qwen3-max-thinking", "qwen3-max",
		"qwen3-30b-a3b-thinking-2507", "qwen3-30b-a3b-instruct-2507", "qwen3-30b-a3b",
		"qwen3-vl-235b-a22b-instruct", "qwen3-vl-235b-a22b-thinking",
		"qwen3-vl-30b-a3b-thinking", "qwen3-vl-30b-a3b-instruct",
		"qwen3-vl-32b-instruct", "qwen3-vl-8b-thinking", "qwen3-vl-8b-instruct", "qwen3-vl",
		"qwen3-235b-a22b-thinking-2507", "qwen3-235b-a22b-2507", "qwen3-235b-a22b",
		"qwen3-14b", "qwen3-32b", "qwen3-8b", "qwen3-4b",
		"qwen3-coder-flash", "qwen3-coder-next", "qwen3-coder-plus", "qwen3-coder:exacto", "qwen3-coder",
		"qwen2.5-coder-7b-instruct", "qwen-2.5-coder-32b-instruct",
		"qwen2.5-vl-72b-instruct", "qwen2.5-vl-32b-instruct", "qwen-2.5-vl-7b-instruct",
		"qwen-2.5-72b-instruct", "qwen-2.5-7b-instruct", "qwen-2-72b-instruct",
		"qwq-32b-preview", "qwq-32b",
		"deepseek-r1-distill-llama-70b", "deepseek-r1-distill-qwen-32b", "deepseek-r1-distill-qwen-14b",
		"deepseek-r1-0528-qwen3-8b", "deepseek-r1-0528", "deepseek-r1",
		"deepseek-v3.2-speciale", "deepseek-v3.2-exp", "deepseek-v3.2", "deepseek-v3.1-terminus",
		"deepseek-chat", "deepseek-prover-v2",

		// xAI Grok模型（长前缀优先）
		"grok-4.1-fast", "grok-4.1", "grok-4-fast", "grok-4",
		"grok-3-mini-beta", "grok-3-mini", "grok-3-beta", "grok-3",
		"grok-2-vision-1212", "grok-2-1212", "grok-2-mini", "grok-2",
		"grok-code-fast-1", "grok-vision-beta",

		// MiniMax模型
		"minimax-m2.5", "minimax-m2.1", "minimax-m2", "minimax-m1", "minimax-01",

		// Meta Llama模型（长前缀优先）
		"llama-3.2-90b-vision-instruct", "llama-3.2-11b-vision-instruct",
		"llama-3.1-405b-instruct", "llama-3.1-405b", "llama-3.1-70b-instruct", "llama-3.1-8b-instruct",
		"llama-3.3-70b-instruct", "llama-3.2-3b-instruct", "llama-3.2-1b-instruct",
		"llama-3-70b-instruct", "llama-3-8b-instruct",
		"llama-guard-4-12b", "llama-guard-3-8b", "llama-guard-2-8b",
		"llama-4-maverick", "llama-4-scout",

		// OpenAI OSS模型
		"gpt-oss-safeguard-20b", "gpt-oss-120b:exacto", "gpt-oss-120b", "gpt-oss-20b",
	}

	for _, prefix := range prefixes {
		if strings.HasPrefix(lowerModel, prefix) {
			if pricing, ok := basePricing[prefix]; ok {
				return pricing, true
			}
		}
	}

	return ModelPricing{}, false
}
