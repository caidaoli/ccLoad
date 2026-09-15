package util_test

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"ccLoad/internal/util"
)

func TestParseCustomModelPricingNormalizesAndValidates(t *testing.T) {
	const input = `{
  "  Custom-Model ": {
    "input_price": 2,
  "output_price": 4,
  "cache_read_price": 0.2,
    "cache_write_price": 0.35,
    "cache_write_price_high": 0.7,
    "cache_read_price_high": 0.4,
    "input_price_high": 3,
    "output_price_high": 6
  }
}`

	pricing, err := util.ParseCustomModelPricing(input)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := pricing["custom-model"]
	if !ok {
		t.Fatalf("normalized model id missing: %#v", pricing)
	}
	if entry.InputPrice != 2 || entry.CacheWritePrice != 0.35 || !entry.HasCacheWritePrice || entry.CacheWritePriceHigh != 0.7 {
		t.Fatalf("normalized pricing = %#v", entry)
	}
	if !entry.HasCacheReadPrice || entry.CacheReadPrice != 0.2 || entry.CacheReadPriceHigh != 0.4 || entry.InputPriceHigh != 3 || entry.OutputPriceHigh != 6 {
		t.Fatalf("normalized pricing = %#v", entry)
	}
	if !entry.HasCacheReadPriceHigh || !entry.HasCacheWritePriceHigh {
		t.Fatalf("high-context explicit price presence missing: %#v", entry)
	}

	cases := []string{
		"null",
		"[]",
		`{"model":null}`,
		`{"model":{"input_price":null}}`,
		`{"model":{"unknown":1}}`,
		`{"model":{"Input_Price":1}}`,
		`{"model":{"input_price":-1}}`,
		`{"model":{"input_price":NaN}}`,
		`{"model":{"token_pricing_tiers":[{"max_input_tokens":100,"input_price":1,"output_price":2}]}}`,
		// 已从自定义价格契约移除的字段必须被拒绝，而不是静默忽略。
		`{"model":{"cache_write_price_1h":1}}`,
		`{"model":{"has_cache_write_price_1h":true}}`,
		`{"model":{"has_cache_write_price":true}}`,
		`{"model":{"has_cache_read_price":true}}`,
		`{"model":{"cache_read_counts_toward_tier":true}}`,
		`{"model":{"image_generation":{"image_output_price":1}}}`,
		`{"model":{"image_generation_fallback":{"low":{"1024x1024":1}}}}`,
		`{"model":{"fixed_cost_per_request":0.25}}`,
		`{" Foo ":{},"foo":{}}`,
		`{"model":{},"model":{}}`,
		`{"model":{}} {}`,
	}
	for _, value := range cases {
		t.Run(value, func(t *testing.T) {
			if _, err := util.ParseCustomModelPricing(value); err == nil {
				t.Fatalf("ParseCustomModelPricing(%q) succeeded", value)
			}
		})
	}
}

func TestCustomModelPricingLimits(t *testing.T) {
	tooLarge := `{"model":{"input_price":` + strings.Repeat("1", util.MaxCustomModelPricingJSONBytes) + `}}`
	if _, err := util.ParseCustomModelPricing(tooLarge); err == nil {
		t.Fatal("expected oversized JSON to be rejected")
	}

	models := make(map[string]any, util.MaxCustomModelPricingModels+1)
	for i := 0; i <= util.MaxCustomModelPricingModels; i++ {
		models[fmt.Sprintf("model-%03d", i)] = map[string]float64{}
	}
	data, err := json.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := util.ParseCustomModelPricing(string(data)); err == nil {
		t.Fatal("expected model count limit to be enforced")
	}
}

func TestCustomModelPricingOverridesAndCalculatesAllPriceForms(t *testing.T) {
	t.Cleanup(func() { _ = util.InstallCustomModelPricing(nil) })
	value := `{
  "gpt-4o": {"input_price": 7, "output_price": 11, "cache_read_price": 0.7},
  "custom-long": {"input_price": 1, "output_price": 2, "cache_read_price": 0.1, "input_price_high": 3, "output_price_high": 4, "cache_read_price_high": 0.3},
  "custom-cache-write": {"input_price": 2, "output_price": 0, "cache_write_price": 0.4},
  "custom-high-cache-write": {"input_price": 1, "output_price": 1, "input_price_high": 2, "output_price_high": 2, "cache_write_price": 0.4, "cache_write_price_high": 0.8},
  "custom-implicit-cache": {"input_price": 3, "output_price": 1},
  "custom-prefix": {"input_price": 2, "output_price": 1}
}`
	if err := util.InstallCustomModelPricingJSON(value); err != nil {
		t.Fatal(err)
	}

	if got := util.CalculateCostDetailed("gpt-4o", 1_000_000, 1_000_000, 0, 0, 0); math.Abs(got-18) > 1e-12 {
		t.Fatalf("custom built-in override cost = %v, want 18", got)
	}
	if got := util.CalculateCostDetailed("custom-long", 200_001, 1_000, 1_000, 0, 0); math.Abs(got-0.604303) > 1e-12 {
		t.Fatalf("custom high-context cost = %v, want 0.604303", got)
	}
	if got := util.CalculateCostDetailed("custom-prefix-v2", 1_000_000, 0, 0, 0, 0); math.Abs(got-2) > 1e-12 {
		t.Fatalf("custom prefix cost = %v, want 2", got)
	}
	// 5 分钟档用显式价，1 小时档恒为「基础输入价 × 2」。
	if got := util.CalculateCostDetailed("custom-cache-write", 0, 0, 0, 1_000_000, 1_000_000); math.Abs(got-4.4) > 1e-12 {
		t.Fatalf("custom cache-write price = %v, want 4.4", got)
	}
	// 高上下文档用显式高上下文缓存创建价，1 小时档按「高上下文输入价 × 2」。
	if got := util.CalculateCostDetailed("custom-high-cache-write", 300_000, 0, 0, 1_000_000, 1_000_000); math.Abs(got-5.4) > 1e-12 {
		t.Fatalf("custom high-context cache-write price = %v, want 5.4", got)
	}
	// 未填缓存价时回退到系列倍率，不再需要「使用显式缓存读取价格」开关。
	if got := util.CalculateCostDetailed("custom-implicit-cache", 0, 0, 1_000_000, 0, 0); math.Abs(got-0.3) > 1e-12 {
		t.Fatalf("custom implicit cache-read price = %v, want 0.3", got)
	}
	breakdown := util.CalculateStandardCostBreakdown("custom-cache-write", "", 0, 0, 0, 1_000_000, 0)
	if math.Abs(breakdown.CacheWrite.Cost-0.4) > 1e-12 {
		t.Fatalf("custom cache-write breakdown = %#v", breakdown)
	}

	if err := util.InstallCustomModelPricingJSON("{}"); err != nil {
		t.Fatal(err)
	}
	if got := util.CalculateCostDetailed("custom-prefix-v2", 1_000_000, 0, 0, 0, 0); got != 0 {
		t.Fatalf("cleared custom pricing cost = %v, want 0", got)
	}
}

func TestCustomModelPricingPreservesSystemTierSelection(t *testing.T) {
	t.Cleanup(func() { _ = util.InstallCustomModelPricing(nil) })
	util.RestoreEmbeddedModelCatalog()
	for _, model := range []string{"gpt-5.6-sol", "gpt-5.6-sol-custom", "gemini-2.5-pro"} {
		t.Run(model, func(t *testing.T) {
			if err := util.InstallCustomModelPricing(nil); err != nil {
				t.Fatal(err)
			}
			pricing, ok := util.LookupSystemModelPricing(model)
			if !ok {
				t.Fatal("missing system pricing")
			}
			inputs := []int{172_000, 172_001, 200_000}
			want := make([]float64, len(inputs))
			for i, input := range inputs {
				want[i] = util.CalculateCostDetailed(model, input, 1_000, 100_000, 0, 0)
			}
			data, err := json.Marshal(map[string]any{model: map[string]float64{
				"input_price": pricing.InputPrice, "output_price": pricing.OutputPrice,
				"input_price_high": pricing.InputPriceHigh, "output_price_high": pricing.OutputPriceHigh,
				"cache_read_price": pricing.CacheReadPrice, "cache_read_price_high": pricing.CacheReadPriceHigh,
			}})
			if err != nil {
				t.Fatal(err)
			}
			if err := util.InstallCustomModelPricingJSON(string(data)); err != nil {
				t.Fatal(err)
			}
			for i, input := range inputs {
				if got := util.CalculateCostDetailed(model, input, 1_000, 100_000, 0, 0); math.Abs(got-want[i]) > 1e-12 {
					t.Errorf("input=%d: override cost=%v, system cost=%v", input, got, want[i])
				}
			}
		})
	}
}

func TestCustomModelPricingHonorsExplicitZeroHighInputPrice(t *testing.T) {
	t.Cleanup(func() { _ = util.InstallCustomModelPricing(nil) })
	for _, tc := range []struct {
		name  string
		high  string
		input int
		want  float64
	}{
		{"omitted", "", 300_000, 0.312},
		{"threshold", `,"input_price_high":0`, 200_000, 0.212},
		{"above threshold", `,"input_price_high":0`, 200_001, 0.034},
		{"free high input", `,"input_price_high":0`, 300_000, 0.034},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := `{"custom-zero-input":{"input_price":1,"output_price":2,"output_price_high":4,"cache_read_price_high":0.3` + tc.high + `}}`
			if err := util.InstallCustomModelPricingJSON(value); err != nil {
				t.Fatal(err)
			}
			if got := util.CalculateCostDetailed("custom-zero-input", tc.input, 1_000, 100_000, 0, 0); math.Abs(got-tc.want) > 1e-12 {
				t.Fatalf("cost=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestCustomModelPricingHonorsExplicitZeroHighCachePrices(t *testing.T) {
	t.Cleanup(func() { _ = util.InstallCustomModelPricing(nil) })
	if err := util.InstallCustomModelPricingJSON(`{
  "custom-high-zero": {
    "input_price": 1,
    "input_price_high": 2,
    "cache_read_price": 0.5,
    "cache_read_price_high": 0,
    "cache_write_price": 0.5,
    "cache_write_price_high": 0
  }
}`); err != nil {
		t.Fatal(err)
	}

	if got := util.CalculateCostDetailed("custom-high-zero", 300_000, 0, 1_000_000, 1_000_000, 0); math.Abs(got-0.6) > 1e-12 {
		t.Fatalf("explicit zero high-context cache cost = %v, want 0.6", got)
	}
}

// 图像专项费率只由系统目录维护：自定义价格不再提供图像字段，
// 因此覆盖一个图像模型不得把图像计费打到零。
func TestCustomPricingDoesNotDisableImagePricing(t *testing.T) {
	t.Cleanup(util.RestoreEmbeddedModelCatalog)
	util.RestoreEmbeddedModelCatalog()
	systemTokenCost := util.CalculateImageGenerationToolCost("gpt-image-2", util.ImageGenerationToolUsage{ImageOutputTokens: 1_000_000})
	systemFallbackCost := util.CalculateImageGenerationToolFallbackCost("gpt-image-2", "low", "1024x1024")
	if systemTokenCost <= 0 || systemFallbackCost <= 0 {
		t.Fatalf("system image pricing missing: token=%v fallback=%v", systemTokenCost, systemFallbackCost)
	}

	if err := util.InstallCustomModelPricingJSON(`{"gpt-image-2":{"input_price":1}}`); err != nil {
		t.Fatal(err)
	}
	if got := util.CalculateImageGenerationToolCost("gpt-image-2", util.ImageGenerationToolUsage{ImageOutputTokens: 1_000_000}); got != systemTokenCost {
		t.Fatalf("custom override changed image token cost: got %v, want %v", got, systemTokenCost)
	}
	if got := util.CalculateImageGenerationToolFallbackCost("gpt-image-2", "low", "1024x1024"); got != systemFallbackCost {
		t.Fatalf("custom override changed image fallback cost: got %v, want %v", got, systemFallbackCost)
	}
}

// 系统分层定价模型（Qwen 全系价格只存在分层表里，基础价为 0）不得被自定义覆盖：
// 覆盖会丢掉分层导致计费归零，必须在解析与安装两个入口都拒绝。
func TestCustomModelPricingRejectsSystemTieredModels(t *testing.T) {
	t.Cleanup(func() { _ = util.InstallCustomModelPricing(nil) })
	util.RestoreEmbeddedModelCatalog()

	baseline := util.CalculateCostDetailed("qwen3-max", 1_000_000, 0, 0, 0, 0)
	if baseline <= 0 {
		t.Fatalf("system tiered baseline cost = %v, want > 0", baseline)
	}

	if _, err := util.ParseCustomModelPricing(`{"qwen3-max":{"input_price":1}}`); err == nil {
		t.Fatal("expected parse to reject system tiered model override")
	}
	if _, err := util.ParseCustomModelPricing(`{"qwen3-max-versioned":{"input_price":1}}`); err == nil {
		t.Fatal("expected parse to reject a versioned system tiered model override")
	}
	if err := util.InstallCustomModelPricing(map[string]util.ModelPricing{
		"qwen3-max": {InputPrice: 1},
	}); err == nil {
		t.Fatal("expected install to reject system tiered model override")
	}

	if got := util.CalculateCostDetailed("qwen3-max", 1_000_000, 0, 0, 0, 0); got != baseline {
		t.Fatalf("rejected override changed tiered cost: got %v, want %v", got, baseline)
	}
}

func TestCatalogRefreshDoesNotApplyCustomPricingToNewTieredModel(t *testing.T) {
	t.Cleanup(func() {
		_ = util.InstallCustomModelPricing(nil)
		util.RestoreEmbeddedModelCatalog()
	})
	util.RestoreEmbeddedModelCatalog()

	if err := util.InstallCustomModelPricingJSON(`{"future-tiered-model":{"input_price":1,"output_price":1}}`); err != nil {
		t.Fatal(err)
	}
	if err := util.InstallModelCatalog(&util.ModelCatalogSnapshot{
		Version: util.ModelCatalogSchemaVersion,
		Models: []util.ModelCatalogEntry{{
			ID: "future-tiered-model", Provider: "openai",
			Pricing: util.ModelPricing{TokenPricingTiers: []util.TokenPricingTier{
				{MaxInputTokens: 100_000, InputPrice: 2, OutputPrice: 2},
				{MaxInputTokens: 200_000, InputPrice: 3, OutputPrice: 3},
				{InputPrice: 4, OutputPrice: 4},
			}},
		}},
	}, "models.dev"); err != nil {
		t.Fatal(err)
	}

	if got := util.CalculateCostDetailed("future-tiered-model", 1_000_000, 0, 0, 0, 0); got != 4 {
		t.Fatalf("catalog tier pricing = %v, want 4", got)
	}
}

// 固定按次费率只由系统目录维护（图像模型），自定义价格不提供该字段；
// 直接安装路径也必须拒绝，否则程序化调用能塞进前端无法编辑的覆盖值。
func TestCustomModelPricingRejectsFixedCostPerRequest(t *testing.T) {
	t.Cleanup(func() { _ = util.InstallCustomModelPricing(nil) })

	if err := util.InstallCustomModelPricing(map[string]util.ModelPricing{
		"some-image-model": {FixedCostPerRequest: 0.25},
	}); err == nil {
		t.Fatal("expected install to reject fixed_cost_per_request override")
	}
}

// 两档长上下文模型（基础价 + >272K 高价）必须可以自定义覆盖：
// 它们用高上下文字段表达，能被自定义价格的整份替换完整描述。
func TestCustomModelPricingAllowsTwoTierHighContextModels(t *testing.T) {
	t.Cleanup(func() { _ = util.InstallCustomModelPricing(nil) })
	util.RestoreEmbeddedModelCatalog()

	for _, model := range []string{"gpt-5.6-sol", "gpt-5.6", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra"} {
		if _, err := util.ParseCustomModelPricing(`{"` + model + `":{"input_price":1}}`); err != nil {
			t.Fatalf("expected %s to be overridable, got %v", model, err)
		}
	}

	if err := util.InstallCustomModelPricingJSON(
		`{"gpt-5.6-sol":{"input_price":1,"output_price":2,"input_price_high":3,"output_price_high":4}}`,
	); err != nil {
		t.Fatal(err)
	}
	// 阈值仍是 272K：200k 走基础价，300k 走高上下文价。
	if got := util.CalculateCostDetailed("gpt-5.6-sol", 200_000, 0, 0, 0, 0); math.Abs(got-0.2) > 1e-12 {
		t.Fatalf("custom base cost = %v, want 0.2", got)
	}
	if got := util.CalculateCostDetailed("gpt-5.6-sol", 300_000, 0, 0, 0, 0); math.Abs(got-0.9) > 1e-12 {
		t.Fatalf("custom high-context cost = %v, want 0.9", got)
	}
}

// 远端目录用两档分层表描述长上下文时，必须折叠成高上下文表达再进快照：
// 否则目录同步一次，gpt-5.6-sol 又会变回「不支持自定义覆盖」。
func TestCustomModelPricingSurvivesRemoteTwoTierSync(t *testing.T) {
	t.Cleanup(func() {
		_ = util.InstallCustomModelPricing(nil)
		util.RestoreEmbeddedModelCatalog()
	})
	util.RestoreEmbeddedModelCatalog()

	if err := util.InstallModelCatalog(&util.ModelCatalogSnapshot{
		Version: util.ModelCatalogSchemaVersion,
		Models: []util.ModelCatalogEntry{{
			ID: "gpt-5.6-sol", Provider: "openai",
			Pricing: util.ModelPricing{
				InputPrice: 5.00, OutputPrice: 30.00, CacheReadPrice: 0.50, HasCacheReadPrice: true,
				TokenPricingTiers: []util.TokenPricingTier{
					{MaxInputTokens: 272_000, InputPrice: 5.00, OutputPrice: 30.00, CacheReadPrice: 0.50, HasCacheReadPrice: true},
					{InputPrice: 10.00, OutputPrice: 45.00, CacheReadPrice: 1.00, HasCacheReadPrice: true},
				},
				CacheReadCountsTowardTier: true,
			},
		}},
	}, "models.dev"); err != nil {
		t.Fatal(err)
	}

	if _, err := util.ParseCustomModelPricing(`{"gpt-5.6-sol":{"input_price":1}}`); err != nil {
		t.Fatalf("remote sync made gpt-5.6-sol non-overridable: %v", err)
	}

	// 折叠前后计费必须逐点等价：272K 边界仍是基础价/高价的切换点。
	for _, tc := range []struct {
		tokens int
		want   float64
	}{
		{200_000, 1.0}, {272_000, 1.36}, {272_001, 2.72001}, {500_000, 5.0},
	} {
		got := util.CalculateCostDetailed("gpt-5.6-sol", tc.tokens, 0, 0, 0, 0)
		if math.Abs(got-tc.want) > 1e-12 {
			t.Fatalf("tokens=%d cost=%v, want %v", tc.tokens, got, tc.want)
		}
	}
}

func TestCustomModelPricingSnapshotPreservesCatalogOverlayAndClear(t *testing.T) {
	t.Cleanup(func() {
		_ = util.InstallCustomModelPricing(nil)
		util.RestoreEmbeddedModelCatalog()
	})
	util.RestoreEmbeddedModelCatalog()
	remote := func(sharedPrice float64) *util.ModelCatalogSnapshot {
		return &util.ModelCatalogSnapshot{
			Version: util.ModelCatalogSchemaVersion,
			Models: []util.ModelCatalogEntry{
				{ID: "shared-model", Provider: "openai", Pricing: util.ModelPricing{InputPrice: sharedPrice, OutputPrice: 1}},
				{ID: "remote-only", Provider: "openai", Pricing: util.ModelPricing{InputPrice: 4, OutputPrice: 1}},
			},
		}
	}
	if err := util.InstallModelCatalog(remote(2), "models.dev"); err != nil {
		t.Fatal(err)
	}
	if err := util.InstallCustomModelPricingJSON(`{"shared-model":{"input_price":9,"output_price":1},"custom-only":{"input_price":3,"output_price":1}}`); err != nil {
		t.Fatal(err)
	}
	if pricing, ok := util.LookupSystemModelPricing("shared-model"); !ok || pricing.InputPrice != 2 {
		t.Fatalf("system lookup should ignore custom override: ok=%v pricing=%#v", ok, pricing)
	}
	if got := util.CalculateCostDetailed("shared-model", 1_000_000, 0, 0, 0, 0); got != 9 {
		t.Fatalf("custom did not override remote price: %v", got)
	}
	if got := util.CalculateCostDetailed("remote-only", 1_000_000, 0, 0, 0, 0); got != 4 {
		t.Fatalf("remote price = %v, want 4", got)
	}
	if got := util.CalculateCostDetailed("custom-only", 1_000_000, 0, 0, 0, 0); got != 3 {
		t.Fatalf("unknown custom price = %v, want 3", got)
	}
	if summary := util.CurrentModelCatalogSummary(); summary.ModelCount != 3 || summary.ProviderCount != 2 {
		t.Fatalf("catalog summary = %#v, want remote plus custom models", summary)
	}

	if err := util.InstallModelCatalog(remote(5), "models.dev"); err != nil {
		t.Fatal(err)
	}
	if got := util.CalculateCostDetailed("shared-model", 1_000_000, 0, 0, 0, 0); got != 9 {
		t.Fatalf("catalog refresh lost custom price: %v", got)
	}
	if err := util.InstallCustomModelPricingJSON(`{}`); err != nil {
		t.Fatal(err)
	}
	if got := util.CalculateCostDetailed("shared-model", 1_000_000, 0, 0, 0, 0); got != 5 {
		t.Fatalf("clearing custom pricing did not restore remote price: %v", got)
	}
}
