package util_test

import (
	"math"
	"testing"

	"ccLoad/internal/util"
)

func TestCalculateCostWithPriceReplacesGlobalPricing(t *testing.T) {
	t.Cleanup(func() { _ = util.InstallCustomModelPricing(nil) })
	if err := util.InstallCustomModelPricingJSON(`{"gpt-4o": {"input_price": 7, "output_price": 11}}`); err != nil {
		t.Fatal(err)
	}
	channelPrice := &util.CustomModelPrice{InputPrice: new(1.0), OutputPrice: new(2.0)}

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
		{"invalid falls back", "gpt-4o", &util.CustomModelPrice{InputPrice: new(-1.0), OutputPrice: new(2.0)}, 18},
	} {
		if got := util.CalculateCostDetailedWithPrice(tc.model, tc.price, 1_000_000, 1_000_000, 0, 0, 0); math.Abs(got-tc.want) > 1e-12 {
			t.Errorf("%s: cost=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCalculateCostWithPriceKeepsSystemTierSemantics(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	price := &util.CustomModelPrice{
		InputPrice: new(1.0), OutputPrice: new(1.0), InputPriceHigh: new(2.0), OutputPriceHigh: new(2.0),
	}
	// gpt-5.6 的缓存读计入 272K 分档：200K 输入 + 100K 缓存读必须走高档价。
	got := util.CalculateCostDetailedWithPrice("gpt-5.6", price, 200_000, 0, 100_000, 0, 0)
	if want := 200_000*2.0/1e6 + 100_000*2.0*0.1/1e6; math.Abs(got-want) > 1e-12 {
		t.Fatalf("high-tier cost=%v, want %v", got, want)
	}
}

func TestCalculateStandardCostBreakdownWithPriceAppliesFastMode(t *testing.T) {
	price := &util.CustomModelPrice{InputPrice: new(2.0), OutputPrice: new(10.0)}
	// 渠道输入、输出及缓存读写均叠加 fast 倍率。
	fast := util.CalculateStandardCostBreakdownWithPrice("claude-opus-5", "fast", util.OpenAIRateCardAPI, price, 1_000_000, 1_000_000, 1_000_000, 1_000_000, 1_000_000, 0, 0)
	if math.Abs(fast.Total-37.4) > 1e-12 || fast.ServiceTierMultiplier != 2 {
		t.Fatalf("fast breakdown=%#v, want total 37.4 with multiplier 2", fast)
	}
}

func TestCustomModelPriceDistinguishesExplicitZero(t *testing.T) {
	withZero := &util.CustomModelPrice{InputPrice: new(1.0), CacheReadPrice: new(0.0)}
	if withZero.Equal(&util.CustomModelPrice{InputPrice: new(1.0)}) {
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
		{"input and output", util.CustomModelPrice{InputPrice: new(1.0), OutputPrice: new(0.0)}, true},
		{"cache only", util.CustomModelPrice{CacheReadPrice: new(0.1)}, false},
		{"missing output", util.CustomModelPrice{InputPrice: new(1.0), CacheWritePrice: new(1.25)}, false},
		{"full high context", util.CustomModelPrice{
			InputPrice: new(1.0), OutputPrice: new(2.0), InputPriceHigh: new(2.0), OutputPriceHigh: new(4.0),
		}, true},
		// 任一高上下文价格都要求高上下文输入/输出成对填写，与渠道价格编辑器一致。
		{"high cache only", util.CustomModelPrice{
			InputPrice: new(1.0), OutputPrice: new(2.0), CacheReadPriceHigh: new(0.2),
		}, false},
		{"missing high output", util.CustomModelPrice{
			InputPrice: new(1.0), OutputPrice: new(2.0), InputPriceHigh: new(2.0),
		}, false},
	} {
		if _, err := tc.price.ModelPricing(); (err == nil) != tc.ok {
			t.Errorf("%s: err=%v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

/**
 * @brief 验证 OpenAI 独立 TTL 价格、旧平价/零价及服务档位统一缩放。
 * @param t 测试上下文。
 */
func TestOpenAICacheWriteTTLCost(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	for _, tc := range []struct {
		name               string
		price              *util.CustomModelPrice
		tier               string
		input, short, long int
		want               float64
	}{
		{"catalog", nil, "", 0, 0, 100_000, 0.025},
		{"mixed", nil, "", 0, 50_000, 50_000, 0.01875},
		{"fast", nil, "fast", 0, 0, 100_000, 0.05},
		{"flex", nil, "flex", 0, 0, 100_000, 0.0125},
		{"custom implicit", &util.CustomModelPrice{InputPrice: new(1.0), OutputPrice: new(0.0)}, "", 0, 500_000, 500_000, 1.875},
		{"old flat", &util.CustomModelPrice{InputPrice: new(1.0), OutputPrice: new(0.0), CacheWritePrice: new(3.0)}, "", 0, 0, 1_000_000, 3},
		{"explicit zero", &util.CustomModelPrice{InputPrice: new(1.0), OutputPrice: new(0.0), CacheWritePrice: new(3.0), CacheWrite12hPrice: new(0.0)}, "", 0, 0, 1_000_000, 0},
		{"free short input fast long", &util.CustomModelPrice{InputPrice: new(0.0), OutputPrice: new(0.0), CacheWritePrice: new(0.0), CacheWrite12hPrice: new(2.0)}, "fast", 0, 0, 1_000_000, 4},
		{"high context", &util.CustomModelPrice{InputPrice: new(1.0), OutputPrice: new(0.0), InputPriceHigh: new(2.0), OutputPriceHigh: new(0.0)}, "", 0, 0, 1_000_000, 5},
		{"old high flat", &util.CustomModelPrice{InputPrice: new(1.0), OutputPrice: new(0.0), InputPriceHigh: new(2.0), OutputPriceHigh: new(0.0), CacheWritePriceHigh: new(7.0)}, "", 0, 0, 1_000_000, 7},
		{"high explicit free", &util.CustomModelPrice{InputPrice: new(1.0), OutputPrice: new(0.0), InputPriceHigh: new(2.0), OutputPriceHigh: new(0.0), CacheWrite12hPriceHigh: new(0.0)}, "", 0, 0, 1_000_000, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := util.CalculateStandardCostBreakdownWithPrice("gpt-6-luna", tc.tier, util.OpenAIRateCardAPI, tc.price, tc.input, 0, 0, 0, 0, tc.short, tc.long)
			if math.Abs(got.Total-tc.want) > 1e-12 || got.CacheWrite.Quantity != tc.short+tc.long {
				t.Fatalf("breakdown=%+v want=%v", got, tc.want)
			}
		})
	}
	if err := util.InstallCustomModelPricingJSON(`{"gpt-6-luna":{"input_price":1,"output_price":0,"cache_write_price":0,"cache_write_12h_price":2}}`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = util.InstallCustomModelPricing(nil) })
	got := util.CalculateStandardCostBreakdownWithPrice("gpt-6-luna", "fast", util.OpenAIRateCardAPI, nil, 0, 0, 0, 0, 0, 0, 1_000_000)
	if got.Total != 4 {
		t.Fatalf("global custom TTL=%+v", got)
	}
}

/**
 * @brief 验证编辑器预填保存不会改变隐式、平价及显式零价的缓存写入费用。
 * @param t 测试上下文。
 */
func TestOpenAICacheWriteTTLEffectivePricingRoundTrip(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	t.Cleanup(func() { _ = util.InstallCustomModelPricing(nil) })
	for _, tc := range []struct {
		name, model, global string
	}{
		{"system implicit", "gpt-6-luna", ""},
		{"custom implicit", "gpt-6-luna", `{"input_price":1,"output_price":0,"input_price_high":2,"output_price_high":0}`},
		{"explicit base", "gpt-6-luna", `{"input_price":1,"output_price":0,"input_price_high":2,"output_price_high":0,"cache_write_12h_price":3}`},
		{"explicit free base", "gpt-6-luna", `{"input_price":1,"output_price":0,"input_price_high":2,"output_price_high":0,"cache_write_12h_price":0}`},
		{"legacy flat", "gpt-6-luna", `{"input_price":1,"output_price":0,"input_price_high":2,"output_price_high":0,"cache_write_price":3}`},
		{"legacy free", "gpt-6-luna", `{"input_price":1,"output_price":0,"input_price_high":2,"output_price_high":0,"cache_write_price":0}`},
		{"explicit free high", "gpt-6-luna", `{"input_price":1,"output_price":0,"input_price_high":2,"output_price_high":0,"cache_write_12h_price_high":0}`},
		{"relay implicit", "relay-private-model", `{"input_price":1,"output_price":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := util.InstallCustomModelPricing(nil); err != nil {
				t.Fatal(err)
			}
			if tc.global != "" {
				if err := util.InstallCustomModelPricingJSON(`{"` + tc.model + `":` + tc.global + `}`); err != nil {
					t.Fatal(err)
				}
			}
			effective, ok := util.LookupEffectiveModelPricing(tc.model)
			if !ok || !effective.HasCacheWrite12hPrice {
				t.Fatalf("effective pricing missing: %+v, found=%v", effective, ok)
			}
			price := &util.CustomModelPrice{
				InputPrice: new(effective.InputPrice), OutputPrice: new(effective.OutputPrice),
				CacheReadPrice: new(effective.CacheReadPrice), CacheWritePrice: new(effective.CacheWritePrice),
				CacheWrite12hPrice: new(effective.CacheWrite12hPrice),
			}
			if effective.HasInputPriceHigh || effective.InputPriceHigh > 0 {
				price.InputPriceHigh, price.OutputPriceHigh = new(effective.InputPriceHigh), new(effective.OutputPriceHigh)
				price.CacheReadPriceHigh, price.CacheWritePriceHigh = new(effective.CacheReadPriceHigh), new(effective.CacheWritePriceHigh)
				price.CacheWrite12hPriceHigh = new(effective.CacheWrite12hPriceHigh)
			}
			if _, err := price.ModelPricing(); err != nil {
				t.Fatal(err)
			}
			for _, input := range []int{0, 300_000} {
				before := util.CalculateStandardCostBreakdownWithPrice(tc.model, "", util.OpenAIRateCardAPI, nil, input, 0, 0, 0, 0, 100_000, 100_000)
				after := util.CalculateStandardCostBreakdownWithPrice(tc.model, "", util.OpenAIRateCardAPI, price, input, 0, 0, 0, 0, 100_000, 100_000)
				if math.Abs(before.Total-after.Total) > 1e-12 {
					t.Fatalf("input=%d cost changed from %+v to %+v after prefill: %+v", input, before, after, price)
				}
			}
		})
	}
}
