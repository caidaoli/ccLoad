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

/**
 * @brief 管理端使用的 snake_case 价格契约，包含 OpenAI 12h 独立写入价。
 */
type adminModelPricing struct {
	InputPrice             float64                 `json:"input_price"`
	OutputPrice            float64                 `json:"output_price"`
	TokenPricingTiers      []adminTokenPricingTier `json:"token_pricing_tiers,omitempty"`
	CacheReadPrice         *float64                `json:"cache_read_price,omitempty"`
	CacheReadPriceHigh     *float64                `json:"cache_read_price_high,omitempty"`
	CacheWritePrice        *float64                `json:"cache_write_price,omitempty"`
	CacheWrite12hPrice     *float64                `json:"cache_write_12h_price,omitempty"`
	CacheWrite12hPriceHigh *float64                `json:"cache_write_12h_price_high,omitempty"`
	CacheWritePriceHigh    *float64                `json:"cache_write_price_high,omitempty"`
	InputPriceHigh         *float64                `json:"input_price_high,omitempty"`
	OutputPriceHigh        *float64                `json:"output_price_high,omitempty"`
}

/**
 * @brief 将生效价格序列化为管理 API 契约，保留显式零价的存在状态。
 * @param pricing 模型价格。
 * @return 管理端价格。
 */
func modelPricingForAdmin(pricing util.ModelPricing) adminModelPricing {
	response := adminModelPricing{
		InputPrice:  pricing.InputPrice,
		OutputPrice: pricing.OutputPrice,
	}
	if pricing.HasCacheReadPrice {
		response.CacheReadPrice = new(pricing.CacheReadPrice)
	}
	if pricing.HasCacheReadPriceHigh || pricing.CacheReadPriceHigh > 0 {
		response.CacheReadPriceHigh = new(pricing.CacheReadPriceHigh)
	}
	if pricing.HasCacheWritePrice {
		response.CacheWritePrice = new(pricing.CacheWritePrice)
	}
	if pricing.HasCacheWritePriceHigh || pricing.CacheWritePriceHigh > 0 {
		response.CacheWritePriceHigh = new(pricing.CacheWritePriceHigh)
	}
	if pricing.HasCacheWrite12hPrice {
		response.CacheWrite12hPrice = new(pricing.CacheWrite12hPrice)
	}
	if pricing.HasCacheWrite12hPriceHigh {
		response.CacheWrite12hPriceHigh = new(pricing.CacheWrite12hPriceHigh)
	}
	if pricing.HasInputPriceHigh || pricing.InputPriceHigh > 0 || pricing.OutputPriceHigh > 0 {
		response.InputPriceHigh = new(pricing.InputPriceHigh)
		response.OutputPriceHigh = new(pricing.OutputPriceHigh)
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
				entry.CacheReadPrice = new(tier.CacheReadPrice)
			}
			response.TokenPricingTiers = append(response.TokenPricingTiers, entry)
		}
	}
	return response
}

// HandleGetModelPricing 返回系统内置/远端目录中的模型价格，不包含自定义覆盖。
// effective=1 时返回当前实际计费价格（含全局自定义覆盖），供渠道模型价格预填。
// GET /admin/model-pricing?model=<model-id>[&effective=1]
func (s *Server) HandleGetModelPricing(c *gin.Context) {
	modelID := strings.TrimSpace(c.Query("model"))
	if modelID == "" {
		RespondErrorMsg(c, http.StatusBadRequest, "missing model id")
		return
	}

	lookup, cacheControl := util.LookupSystemModelPricing, "private, max-age=300"
	if c.Query("effective") == "1" {
		// 全局自定义价格保存后立即生效，预填不能读到浏览器缓存里的旧值。
		lookup, cacheControl = util.LookupEffectiveModelPricing, "no-store"
	}
	pricing, found := lookup(modelID)
	c.Header("Cache-Control", cacheControl)
	RespondJSON(c, http.StatusOK, gin.H{
		"model":   modelID,
		"found":   found,
		"pricing": modelPricingForAdmin(pricing),
	})
}
