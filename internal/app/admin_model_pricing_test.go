package app

import (
	"math"
	"net/http"
	"testing"

	"ccLoad/internal/util"
)

func TestHandleGetModelPricingReturnsSystemDefaults(t *testing.T) {
	t.Cleanup(func() {
		_ = util.InstallCustomModelPricing(nil)
		util.RestoreEmbeddedModelCatalog()
	})
	util.RestoreEmbeddedModelCatalog()
	if err := util.InstallCustomModelPricingJSON(`{"gpt-5.4":{"input_price":99,"output_price":99}}`); err != nil {
		t.Fatal(err)
	}

	type payload struct {
		Model   string            `json:"model"`
		Found   bool              `json:"found"`
		Pricing adminModelPricing `json:"pricing"`
	}
	c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/model-pricing?model=gpt-5.4", nil))
	(&Server{}).HandleGetModelPricing(c)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want %d body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	resp := mustParseAPIResponse[payload](t, w.Body.Bytes()).Data
	if resp.Model != "gpt-5.4" || !resp.Found {
		t.Fatalf("response=%#v, want found system pricing", resp)
	}
	if resp.Pricing.InputPrice == 99 || resp.Pricing.OutputPrice == 99 || resp.Pricing.InputPrice <= 0 {
		t.Fatalf("response used custom override: %#v", resp.Pricing)
	}
	if resp.Pricing.CacheWritePrice == nil {
		t.Fatalf("system cache creation defaults missing: %#v", resp.Pricing)
	}
	if math.Abs(*resp.Pricing.CacheWritePrice-resp.Pricing.InputPrice*1.25) > 1e-12 {
		t.Fatalf("cache creation defaults = %#v", resp.Pricing)
	}
	// 高上下文缓存创建价按高上下文输入价展开，编辑器不再暴露 1 小时档。
	if resp.Pricing.CacheWritePriceHigh == nil || resp.Pricing.InputPriceHigh == nil ||
		math.Abs(*resp.Pricing.CacheWritePriceHigh-*resp.Pricing.InputPriceHigh*1.25) > 1e-12 {
		t.Fatalf("high-context cache creation defaults = %#v", resp.Pricing)
	}

	// effective=1 给渠道价格编辑器预填当前实际计费价格，包含全局自定义覆盖。
	c, w = newTestContext(t, newRequest(http.MethodGet, "/admin/model-pricing?model=gpt-5.4&effective=1", nil))
	(&Server{}).HandleGetModelPricing(c)
	effective := mustParseAPIResponse[payload](t, w.Body.Bytes()).Data
	if !effective.Found || effective.Pricing.InputPrice != 99 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("effective response payload=%#v cache=%q, want uncached custom override", effective, w.Header().Get("Cache-Control"))
	}

	c, w = newTestContext(t, newRequest(http.MethodGet, "/admin/model-pricing?model=unknown-custom-model", nil))
	(&Server{}).HandleGetModelPricing(c)
	unknown := mustParseAPIResponse[payload](t, w.Body.Bytes()).Data
	if w.Code != http.StatusOK || unknown.Found {
		t.Fatalf("unknown response status=%d payload=%#v", w.Code, unknown)
	}

	c, w = newTestContext(t, newRequest(http.MethodGet, "/admin/model-pricing", nil))
	(&Server{}).HandleGetModelPricing(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing model status=%d, want %d", w.Code, http.StatusBadRequest)
	}
}
