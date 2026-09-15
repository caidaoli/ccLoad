package app

import (
	"net/http"
	"strings"

	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

type adminTokenPricingTier struct {
	MaxInputTokens int      `json:"max_input_tokens"`
	InputPrice     float64  `json:"input_price"`
	OutputPrice    float64  `json:"output_price"`
	CacheReadPrice *float64 `json:"cache_read_price,omitempty"`
}

// adminModelPricing keeps the admin API's snake_case contract separate from
// ModelPricing's legacy persisted-cache representation.
type adminModelPricing struct {
	InputPrice          float64                 `json:"input_price"`
	OutputPrice         float64                 `json:"output_price"`
	TokenPricingTiers   []adminTokenPricingTier `json:"token_pricing_tiers,omitempty"`
	CacheReadPrice      *float64                `json:"cache_read_price,omitempty"`
	CacheReadPriceHigh  *float64                `json:"cache_read_price_high,omitempty"`
	CacheWritePrice     *float64                `json:"cache_write_price,omitempty"`
	CacheWritePriceHigh *float64                `json:"cache_write_price_high,omitempty"`
	InputPriceHigh      *float64                `json:"input_price_high,omitempty"`
	OutputPriceHigh     *float64                `json:"output_price_high,omitempty"`
}

func modelPricingForAdmin(pricing util.ModelPricing) adminModelPricing {
	response := adminModelPricing{
		InputPrice:  pricing.InputPrice,
		OutputPrice: pricing.OutputPrice,
	}
	if pricing.HasCacheReadPrice {
		response.CacheReadPrice = pricePointer(pricing.CacheReadPrice)
	}
	if pricing.HasCacheReadPriceHigh || pricing.CacheReadPriceHigh > 0 {
		response.CacheReadPriceHigh = pricePointer(pricing.CacheReadPriceHigh)
	}
	if pricing.HasCacheWritePrice {
		response.CacheWritePrice = pricePointer(pricing.CacheWritePrice)
	}
	if pricing.HasCacheWritePriceHigh || pricing.CacheWritePriceHigh > 0 {
		response.CacheWritePriceHigh = pricePointer(pricing.CacheWritePriceHigh)
	}
	if pricing.InputPriceHigh > 0 || pricing.OutputPriceHigh > 0 {
		response.InputPriceHigh = pricePointer(pricing.InputPriceHigh)
		response.OutputPriceHigh = pricePointer(pricing.OutputPriceHigh)
	}
	if len(pricing.TokenPricingTiers) > 0 {
		response.TokenPricingTiers = make([]adminTokenPricingTier, 0, len(pricing.TokenPricingTiers))
		for _, tier := range pricing.TokenPricingTiers {
			entry := adminTokenPricingTier{
				MaxInputTokens: tier.MaxInputTokens,
				InputPrice:     tier.InputPrice,
				OutputPrice:    tier.OutputPrice,
			}
			if tier.HasCacheReadPrice {
				entry.CacheReadPrice = pricePointer(tier.CacheReadPrice)
			}
			response.TokenPricingTiers = append(response.TokenPricingTiers, entry)
		}
	}
	return response
}

func pricePointer(value float64) *float64 { return &value }

// HandleGetModelPricing 返回系统内置/远端目录中的模型价格，不包含自定义覆盖。
// GET /admin/model-pricing?model=<model-id>
func (s *Server) HandleGetModelPricing(c *gin.Context) {
	modelID := strings.TrimSpace(c.Query("model"))
	if modelID == "" {
		RespondErrorMsg(c, http.StatusBadRequest, "missing model id")
		return
	}

	pricing, found := util.LookupSystemModelPricing(modelID)
	c.Header("Cache-Control", "private, max-age=300")
	RespondJSON(c, http.StatusOK, gin.H{
		"model":   modelID,
		"found":   found,
		"pricing": modelPricingForAdmin(pricing),
	})
}
