# GPT-5.6 Tiered Pricing Fix Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 GPT-5.6、Sol、Terra、Luna 在内置和 models.dev 定价路径下，都按非缓存输入加缓存读取输入的总量正确选择 272K 价格档。

**Architecture:** 保留 `CalculateCostDetailed` 的公开签名和解析层的 Token 归一化职责，复用 `ModelPricing.CacheReadCountsTowardTier` 表达“缓存读取参与分档”。内置目录提供完整 GPT-5.6 两档价格，models.dev 的 OpenAI context tier 在归一化及覆盖安装时传播同一语义。

**Tech Stack:** Go、标准库 `testing`、ccLoad 模型目录与成本计算模块。

## Global Constraints

- 所有 Go 测试必须带 `-tags sonic`。
- 不修改 `CalculateCostDetailed(model string, inputTokens, outputTokens, cacheReadTokens, cache5mTokens, cache1hTokens int) float64` 签名。
- GPT-5.6 分档阈值是 272,000 输入 Token，第一档为闭区间，第二档无上限。
- 只让 OpenAI context tier 的缓存读取 Token 参与分档，不改变 Gemini、Qwen、MiMo、GPT-5.4/5.5 的既有语义。
- 复用现有测试文件，不新增测试文件。

---

### Task 1: 内置 GPT-5.6 分段计价

**Files:**
- Modify: `internal/util/cost_calculator_test.go`
- Modify: `internal/util/model_pricing_catalog.go`

**Interfaces:**
- Consumes: `CalculateCostDetailed(...) float64`、`ModelPricing`、`TokenPricingTier`。
- Produces: 内置 `gpt-5.6`、`gpt-5.6-sol`、`gpt-5.6-terra`、`gpt-5.6-luna` 的完整两档价格和缓存参与分档语义。

- [x] **Step 1: 写入内置目录回归测试**

在 `internal/util/cost_calculator_test.go` 扩展 GPT-5.6 测试，覆盖四个模型的 272K 边界、缓存跨阈值和长档缓存写入：

```go
func TestCalculateCost_GPT56TieredPricing(t *testing.T) {
	RestoreEmbeddedModelCatalog()
	t.Cleanup(RestoreEmbeddedModelCatalog)

	testCases := []struct {
		name, model                                      string
		inputTokens, outputTokens, cacheRead, cacheWrite int
		expected                                         float64
	}{
		{name: "sol boundary", model: "gpt-5.6-sol", inputTokens: 272_000, outputTokens: 1_000, expected: 1.39},
		{name: "sol above boundary", model: "gpt-5.6", inputTokens: 272_001, outputTokens: 1_000, expected: 2.76501},
		{name: "terra boundary", model: "gpt-5.6-terra", inputTokens: 272_000, outputTokens: 1_000, expected: 0.695},
		{name: "terra above boundary", model: "gpt-5.6-terra", inputTokens: 272_001, outputTokens: 1_000, expected: 1.382505},
		{name: "luna boundary", model: "gpt-5.6-luna", inputTokens: 272_000, outputTokens: 1_000, expected: 0.278},
		{name: "luna above boundary", model: "gpt-5.6-luna", inputTokens: 272_001, outputTokens: 1_000, expected: 0.553002},
		{name: "sol cache crosses boundary", model: "gpt-5.6-sol", inputTokens: 100_000, outputTokens: 1_000, cacheRead: 200_000, expected: 1.245},
		{name: "terra cache crosses boundary", model: "gpt-5.6-terra", inputTokens: 100_000, outputTokens: 1_000, cacheRead: 200_000, expected: 0.6225},
		{name: "luna cache crosses boundary and write uses high tier", model: "gpt-5.6-luna", inputTokens: 100_000, cacheRead: 200_000, cacheWrite: 1_000, expected: 0.2425},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cost := CalculateCostDetailed(tc.model, tc.inputTokens, tc.outputTokens, tc.cacheRead, tc.cacheWrite, 0)
			if !floatEquals(cost, tc.expected, 0.000001) {
				t.Fatalf("cost = %.6f, expected %.6f", cost, tc.expected)
			}
		})
	}
}
```

- [x] **Step 2: 运行测试并确认当前内置价格失败**

Run: `go test -tags sonic ./internal/util -run TestCalculateCost_GPT56TieredPricing -count=1`

Expected: FAIL；272K 以上和缓存跨阈值场景仍按基础价计算。

- [x] **Step 3: 增加三组可复用 GPT-5.6 tiers 并绑定四个模型**

在 `internal/util/model_pricing_catalog.go` 的包级定价变量中加入：

```go
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
```

把四个内置条目改为显式缓存价、对应 tiers 和 `CacheReadCountsTowardTier: true`；裸 `gpt-5.6` 与 Sol 共享 `gpt56SolTiers`。

- [x] **Step 4: 运行内置目录定向测试**

Run: `go test -tags sonic ./internal/util -run 'TestCalculateCost_(GPT56TieredPricing|OpenAIModels|GPT56CacheWrite)$' -count=1`

Expected: PASS。

- [x] **Step 5: 提交内置目录修复**

```bash
git add internal/util/model_pricing_catalog.go internal/util/cost_calculator_test.go
git commit -m "fix: add GPT-5.6 tiered pricing"
```

### Task 2: models.dev OpenAI context tier 语义

**Files:**
- Modify: `internal/util/models_dev_catalog.go`
- Modify: `internal/util/model_pricing_catalog.go`
- Modify: `internal/util/models_dev_catalog_test.go`

**Interfaces:**
- Consumes: `normalizeModelsDevModel(provider string, raw modelsDevModel) (ModelCatalogEntry, bool)`、`overlayRemotePricing(embedded, remote ModelPricing) ModelPricing`。
- Produces: models.dev OpenAI context tier 的 `CacheReadCountsTowardTier=true`，安装后对远端新增模型和内置模型都生效；非 OpenAI provider 保持 false。

- [x] **Step 1: 扩展 models.dev 公开行为测试**

在 `TestParseModelsDevCatalogNormalizesOfficialPrices` 中断言 OpenAI 条目开启缓存分档，并安装快照验证缓存跨 272K 后使用高档：

```go
if !entry.Pricing.CacheReadCountsTowardTier {
	t.Fatalf("OpenAI context tier did not count cache reads: %#v", entry.Pricing)
}
util.RestoreEmbeddedModelCatalog()
t.Cleanup(util.RestoreEmbeddedModelCatalog)
if err := util.InstallModelCatalog(snapshot, "models.dev"); err != nil {
	t.Fatal(err)
}
if got, want := util.CalculateCostDetailed("gpt-next", 100_000, 1_000, 200_000, 0, 0), 0.6225; got != want {
	t.Fatalf("installed OpenAI tiered cost = %v, want %v", got, want)
}
```

新增非 OpenAI context tier 测试，解析 `anthropic/claude-next` 的相同 tiers，并断言 `CacheReadCountsTowardTier` 保持 false。

- [x] **Step 2: 运行测试并确认远端语义失败**

Run: `go test -tags sonic ./internal/util -run 'TestParseModelsDevCatalog(NormalizesOfficialPrices|NonOpenAIContextTierDoesNotCountCacheRead)$' -count=1`

Expected: FAIL；OpenAI 条目没有启用缓存参与分档，安装覆盖也没有传播该字段。

- [x] **Step 3: 在归一化和远端覆盖边界传播字段**

在 `normalizeModelsDevModel` 得到非空 context tiers 后，仅对 OpenAI 设置字段：

```go
pricing.TokenPricingTiers = tiers
pricing.CacheReadCountsTowardTier = provider == "openai" && len(tiers) > 0
```

在 `overlayRemotePricing` 覆盖远端 tiers 时同步复制该字段：

```go
if len(remote.TokenPricingTiers) > 0 {
	overlay.TokenPricingTiers = append([]TokenPricingTier(nil), remote.TokenPricingTiers...)
	overlay.CacheReadCountsTowardTier = remote.CacheReadCountsTowardTier
	overlay.InputPriceHigh = 0
	overlay.OutputPriceHigh = 0
	overlay.CacheReadPriceHigh = 0
}
```

- [x] **Step 4: 运行 models.dev 与成本计算定向测试**

Run: `go test -tags sonic ./internal/util -run 'Test(ParseModelsDevCatalog|CalculateCost_GPT56)' -count=1`

Expected: PASS。

- [x] **Step 5: 提交远端目录修复**

```bash
git add internal/util/models_dev_catalog.go internal/util/model_pricing_catalog.go internal/util/models_dev_catalog_test.go
git commit -m "fix: preserve OpenAI context tier semantics"
```

### Task 3: 全量验证

**Files:**
- Verify only: `internal/util/...`
- Verify only: `internal/...`

**Interfaces:**
- Consumes: Task 1 和 Task 2 的完整实现。
- Produces: 可交付的回归验证结果。

- [x] **Step 1: 格式化修改的 Go 文件**

Run: `gofmt -w internal/util/model_pricing_catalog.go internal/util/models_dev_catalog.go internal/util/cost_calculator_test.go internal/util/models_dev_catalog_test.go`

Expected: 命令成功且无输出。

- [x] **Step 2: 运行 util 包测试**

Run: `go test -tags sonic ./internal/util/...`

Expected: PASS。

- [x] **Step 3: 运行 internal 全量测试**

Run: `go test -tags sonic ./internal/...`

Expected: PASS。

- [x] **Step 4: 运行静态检查**

Run: `golangci-lint run ./...`

Expected: 零警告、退出码 0。

- [x] **Step 5: 检查最终差异和工作树**

Run: `git diff --check && git status --short --branch`

Expected: `git diff --check` 无输出；工作树只包含计划文档（若尚未提交）或完全干净。
