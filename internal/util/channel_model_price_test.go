package util_test

import (
	"math"
	"testing"

	"ccLoad/internal/util"
)

func priceOf(value float64) *float64 { return &value }

func TestCalculateCostWithPriceReplacesGlobalPricing(t *testing.T) {
	t.Cleanup(func() { _ = util.InstallCustomModelPricing(nil) })
	if err := util.InstallCustomModelPricingJSON(`{"gpt-4o": {"input_price": 7, "output_price": 11}}`); err != nil {
		t.Fatal(err)
	}
	channelPrice := &util.CustomModelPrice{InputPrice: priceOf(1), OutputPrice: priceOf(2)}

	for _, tc := range []struct {
		name  string
		model string
		price *util.CustomModelPrice
		want  float64
	}{
		{"channel price wins", "gpt-4o", channelPrice, 3},
		{"nil keeps global custom", "gpt-4o", nil, 18},
		{"unknown model priced by channel", "relay-private-model", channelPrice, 3},
		// 绕过校验的脏价格回退全局价格，而不是按负数或 0 计费。
		{"invalid falls back", "gpt-4o", &util.CustomModelPrice{InputPrice: priceOf(-1), OutputPrice: priceOf(2)}, 18},
	} {
		if got := util.CalculateCostDetailedWithPrice(tc.model, tc.price, 1_000_000, 1_000_000, 0, 0, 0); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("%s: cost=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCalculateCostWithPriceKeepsSystemTierSemantics(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	price := &util.CustomModelPrice{
		InputPrice: priceOf(1), OutputPrice: priceOf(1), InputPriceHigh: priceOf(2), OutputPriceHigh: priceOf(2),
	}
	// gpt-5.6 的缓存读计入 272K 分档：200K 输入 + 100K 缓存读必须走高档价。
	got := util.CalculateCostDetailedWithPrice("gpt-5.6", price, 200_000, 0, 100_000, 0, 0)
	if want := 200_000*2.0/1e6 + 100_000*2.0*0.1/1e6; math.Abs(got-want) > 1e-12 {
		t.Fatalf("high-tier cost=%v, want %v", got, want)
	}
}

func TestCalculateStandardCostBreakdownWithPriceAppliesFastMode(t *testing.T) {
	price := &util.CustomModelPrice{InputPrice: priceOf(2), OutputPrice: priceOf(10)}
	// input/output 按渠道价翻倍，缓存读仍按渠道基础价。
	fast := util.CalculateStandardCostBreakdownWithPrice("claude-opus-5", "fast", price, 1_000_000, 1_000_000, 1_000_000, 0, 0)
	if math.Abs(fast.Total-24.2) > 1e-12 || fast.ServiceTierMultiplier != 2 {
		t.Fatalf("fast breakdown=%#v, want total 24.2 with multiplier 2", fast)
	}
}

func TestCustomModelPriceDistinguishesExplicitZero(t *testing.T) {
	withZero := &util.CustomModelPrice{InputPrice: priceOf(1), CacheReadPrice: priceOf(0)}
	if withZero.Equal(&util.CustomModelPrice{InputPrice: priceOf(1)}) {
		t.Fatal("explicit zero cache price must differ from an unset one")
	}
	clone := withZero.Clone()
	*clone.InputPrice = 5
	if !(&util.CustomModelPrice{}).IsEmpty() || *withZero.InputPrice != 1 {
		t.Fatal("empty detection or clone isolation is broken")
	}
}

func TestCustomModelPriceRequiresInputAndOutput(t *testing.T) {
	for _, tc := range []struct {
		name  string
		price util.CustomModelPrice
		ok    bool
	}{
		{"input and output", util.CustomModelPrice{InputPrice: priceOf(1), OutputPrice: priceOf(0)}, true},
		{"cache only", util.CustomModelPrice{CacheReadPrice: priceOf(0.1)}, false},
		{"missing output", util.CustomModelPrice{InputPrice: priceOf(1), CacheWritePrice: priceOf(1.25)}, false},
		{"full high context", util.CustomModelPrice{
			InputPrice: priceOf(1), OutputPrice: priceOf(2), InputPriceHigh: priceOf(2), OutputPriceHigh: priceOf(4),
		}, true},
		// 任一高上下文价格都要求高上下文输入/输出成对填写，与渠道价格编辑器一致。
		{"high cache only", util.CustomModelPrice{
			InputPrice: priceOf(1), OutputPrice: priceOf(2), CacheReadPriceHigh: priceOf(0.2),
		}, false},
		{"missing high output", util.CustomModelPrice{
			InputPrice: priceOf(1), OutputPrice: priceOf(2), InputPriceHigh: priceOf(2),
		}, false},
	} {
		if _, err := tc.price.ModelPricing(); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}
