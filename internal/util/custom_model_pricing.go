package util

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
)

const (
	// MaxCustomModelPricingJSONBytes bounds the persisted setting before parsing.
	MaxCustomModelPricingJSONBytes = 1 << 20
	// MaxCustomModelPricingModels prevents an unbounded pricing/prefix snapshot.
	MaxCustomModelPricingModels = 512
)

var customModelIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]*$`)

// CustomModelPrice 是自定义价格的对外契约（美元/百万 Token）：全局 model_custom_pricing
// 的单个模型条目与渠道模型条目的 pricing 共用。nil 字段表示未填写，与显式 0 语义不同。
type CustomModelPrice struct {
	InputPrice          *float64 `json:"input_price,omitempty"`
	OutputPrice         *float64 `json:"output_price,omitempty"`
	CacheReadPrice      *float64 `json:"cache_read_price,omitempty"`
	CacheReadPriceHigh  *float64 `json:"cache_read_price_high,omitempty"`
	CacheWritePrice     *float64 `json:"cache_write_price,omitempty"`
	CacheWritePriceHigh *float64 `json:"cache_write_price_high,omitempty"`
	InputPriceHigh      *float64 `json:"input_price_high,omitempty"`
	OutputPriceHigh     *float64 `json:"output_price_high,omitempty"`
}

func (p *CustomModelPrice) fields() [8]*float64 {
	return [8]*float64{
		p.InputPrice, p.OutputPrice, p.CacheReadPrice, p.CacheReadPriceHigh,
		p.CacheWritePrice, p.CacheWritePriceHigh, p.InputPriceHigh, p.OutputPriceHigh,
	}
}

// IsEmpty reports whether no price field is set.
func (p *CustomModelPrice) IsEmpty() bool {
	if p == nil {
		return true
	}
	for _, value := range p.fields() {
		if value != nil {
			return false
		}
	}
	return true
}

// Clone returns an independent copy.
func (p *CustomModelPrice) Clone() *CustomModelPrice {
	if p == nil {
		return nil
	}
	clonePrice := func(value *float64) *float64 {
		if value == nil {
			return nil
		}
		copied := *value
		return &copied
	}
	return &CustomModelPrice{
		InputPrice:          clonePrice(p.InputPrice),
		OutputPrice:         clonePrice(p.OutputPrice),
		CacheReadPrice:      clonePrice(p.CacheReadPrice),
		CacheReadPriceHigh:  clonePrice(p.CacheReadPriceHigh),
		CacheWritePrice:     clonePrice(p.CacheWritePrice),
		CacheWritePriceHigh: clonePrice(p.CacheWritePriceHigh),
		InputPriceHigh:      clonePrice(p.InputPriceHigh),
		OutputPriceHigh:     clonePrice(p.OutputPriceHigh),
	}
}

// Equal compares field presence and values.
func (p *CustomModelPrice) Equal(other *CustomModelPrice) bool {
	if p.IsEmpty() || other.IsEmpty() {
		return p.IsEmpty() == other.IsEmpty()
	}
	left, right := p.fields(), other.fields()
	for i := range left {
		if (left[i] == nil) != (right[i] == nil) || (left[i] != nil && *left[i] != *right[i]) {
			return false
		}
	}
	return true
}

// ModelPricing validates the prices and converts them to the calculator representation.
func (p *CustomModelPrice) ModelPricing() (ModelPricing, error) {
	if p == nil {
		return ModelPricing{}, fmt.Errorf("pricing is empty")
	}
	return normalizeCustomModelPricing(*p)
}

// ParseCustomModelPricing parses and normalizes the model_custom_pricing setting.
// An empty object clears all overrides. The returned map is detached from the input.
func ParseCustomModelPricing(value string) (map[string]ModelPricing, error) {
	if len(value) > MaxCustomModelPricingJSONBytes {
		return nil, fmt.Errorf("custom pricing JSON exceeds %d bytes", MaxCustomModelPricingJSONBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(value)))
	first, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("decode custom pricing JSON: %w", err)
	}
	start, ok := first.(json.Delim)
	if !ok || start != '{' {
		return nil, fmt.Errorf("custom pricing must be a JSON object")
	}
	raw := make(map[string]json.RawMessage)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, fmt.Errorf("decode custom pricing model id: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("custom pricing model id must be a string")
		}
		if _, exists := raw[key]; exists {
			return nil, fmt.Errorf("duplicate custom pricing model id %q", key)
		}
		var objectData json.RawMessage
		if err := decoder.Decode(&objectData); err != nil {
			return nil, fmt.Errorf("decode custom pricing model %q: %w", key, err)
		}
		raw[key] = objectData
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		if err != nil {
			return nil, fmt.Errorf("decode custom pricing JSON object: %w", err)
		}
		return nil, fmt.Errorf("custom pricing must be a JSON object")
	}
	if err := decoder.Decode(&struct{}{}); err == nil {
		return nil, fmt.Errorf("custom pricing JSON contains multiple values")
	} else if err != io.EOF {
		return nil, fmt.Errorf("decode custom pricing JSON trailing data: %w", err)
	}
	if len(raw) > MaxCustomModelPricingModels {
		return nil, fmt.Errorf("custom pricing contains more than %d models", MaxCustomModelPricingModels)
	}

	result := make(map[string]ModelPricing, len(raw))
	for rawID, objectData := range raw {
		id := strings.ToLower(strings.TrimSpace(rawID))
		if !customModelIDPattern.MatchString(id) || strings.ContainsAny(id, " \t\r\n") {
			return nil, fmt.Errorf("invalid custom pricing model id %q", rawID)
		}
		if _, exists := result[id]; exists {
			return nil, fmt.Errorf("duplicate custom pricing model id after normalization %q", id)
		}
		if bytes.Equal(bytes.TrimSpace(objectData), []byte("null")) {
			return nil, fmt.Errorf("model %q must be a JSON object", id)
		}
		if err := validateCustomPricingTarget(id); err != nil {
			return nil, err
		}
		if err := validateCustomPricingFieldNames(objectData); err != nil {
			return nil, fmt.Errorf("model %q: %w", id, err)
		}
		if err := rejectCustomPricingNulls(objectData); err != nil {
			return nil, fmt.Errorf("model %q: %w", id, err)
		}
		objectDecoder := json.NewDecoder(bytes.NewReader(objectData))
		objectDecoder.DisallowUnknownFields()
		var object CustomModelPrice
		if err := objectDecoder.Decode(&object); err != nil {
			return nil, fmt.Errorf("model %q must be a JSON object: %w", id, err)
		}
		if err := objectDecoder.Decode(&struct{}{}); err != io.EOF {
			return nil, fmt.Errorf("model %q contains trailing JSON data", id)
		}
		pricing, err := normalizeCustomModelPricing(object)
		if err != nil {
			return nil, fmt.Errorf("model %q: %w", id, err)
		}
		result[id] = pricing
	}
	return result, nil
}

// validateCustomPricingTarget 拒绝覆盖系统分层定价的模型。
// 自定义价格只能表达基础价与高上下文两档，无法表达系统分层表；
// 一旦覆盖，系统分层（Qwen 全系价格只存在分层里）会被静默丢弃导致计费归零或降档。
func validateCustomPricingTarget(id string) error {
	snapshot := activeModelPricing.Load()
	if snapshot == nil {
		return nil
	}
	pricing, ok := lookupModelPricingWithFallback(
		snapshot.systemPricing, snapshot.systemAliases, snapshot.systemPrefixBuckets, id,
	)
	if ok && len(pricing.TokenPricingTiers) > 0 {
		return fmt.Errorf("model %q uses system token-tier pricing and cannot be overridden", id)
	}
	return nil
}

// ParseModelCustomPricingJSON is a descriptive alias kept for callers that use
// the setting name in their integration code.
func ParseModelCustomPricingJSON(value string) (map[string]ModelPricing, error) {
	return ParseCustomModelPricing(value)
}

func normalizeCustomModelPricing(raw CustomModelPrice) (ModelPricing, error) {
	pricing := ModelPricing{}
	assignPrice := func(name string, value *float64, dst *float64) error {
		if value == nil {
			return nil
		}
		if err := validateCustomPrice(name, *value); err != nil {
			return err
		}
		*dst = *value
		return nil
	}
	for _, field := range []struct {
		name  string
		value *float64
		dst   *float64
	}{
		{"input_price", raw.InputPrice, &pricing.InputPrice},
		{"output_price", raw.OutputPrice, &pricing.OutputPrice},
		{"cache_read_price", raw.CacheReadPrice, &pricing.CacheReadPrice},
		{"cache_read_price_high", raw.CacheReadPriceHigh, &pricing.CacheReadPriceHigh},
		{"cache_write_price", raw.CacheWritePrice, &pricing.CacheWritePrice},
		{"cache_write_price_high", raw.CacheWritePriceHigh, &pricing.CacheWritePriceHigh},
		{"input_price_high", raw.InputPriceHigh, &pricing.InputPriceHigh},
		{"output_price_high", raw.OutputPriceHigh, &pricing.OutputPriceHigh},
	} {
		if err := assignPrice(field.name, field.value, field.dst); err != nil {
			return ModelPricing{}, err
		}
	}
	pricing.HasInputPriceHigh = raw.InputPriceHigh != nil
	// 缓存价没有独立的启用开关：填了（含显式 0）就按填的值算，
	// 留空则按基础价 × 系列倍率回退。
	if raw.CacheReadPrice != nil {
		pricing.HasCacheReadPrice = true
	}
	if raw.CacheReadPriceHigh != nil {
		pricing.HasCacheReadPriceHigh = true
	}
	if raw.CacheWritePrice != nil {
		pricing.HasCacheWritePrice = true
	}
	if raw.CacheWritePriceHigh != nil {
		pricing.HasCacheWritePriceHigh = true
	}
	return pricing, nil
}

func validateCustomPrice(name string, value float64) error {
	if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("%s must be finite and non-negative", name)
	}
	return nil
}

func rejectCustomPricingNulls(data []byte) error {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("invalid JSON value: %w", err)
	}
	var walk func(any) error
	walk = func(current any) error {
		switch typed := current.(type) {
		case nil:
			return fmt.Errorf("null values are not allowed")
		case []any:
			for _, item := range typed {
				if err := walk(item); err != nil {
					return err
				}
			}
		case map[string]any:
			for _, item := range typed {
				if err := walk(item); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(value)
}

func validateCustomPricingFieldNames(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("model pricing must be an object: %w", err)
	}
	allowed := map[string]struct{}{
		"input_price": {}, "output_price": {}, "cache_read_price": {}, "cache_read_price_high": {},
		"cache_write_price": {}, "cache_write_price_high": {},
		"input_price_high": {}, "output_price_high": {},
	}
	for name := range fields {
		if _, ok := allowed[name]; !ok {
			return fmt.Errorf("unknown field %q", name)
		}
	}
	return nil
}

// InstallCustomModelPricing atomically installs a complete replacement map.
func InstallCustomModelPricing(pricing map[string]ModelPricing) error {
	normalized := make(map[string]ModelPricing, len(pricing))
	if len(pricing) > MaxCustomModelPricingModels {
		return fmt.Errorf("custom pricing contains more than %d models", MaxCustomModelPricingModels)
	}
	for rawID, entry := range pricing {
		id := strings.ToLower(strings.TrimSpace(rawID))
		if !customModelIDPattern.MatchString(id) || strings.ContainsAny(id, " \t\r\n") {
			return fmt.Errorf("invalid custom pricing model id %q", rawID)
		}
		if _, exists := normalized[id]; exists {
			return fmt.Errorf("duplicate custom pricing model id after normalization %q", id)
		}
		if err := validateCustomModelPricing(entry); err != nil {
			return fmt.Errorf("model %q: %w", id, err)
		}
		if err := validateCustomPricingTarget(id); err != nil {
			return err
		}
		normalized[id] = cloneModelPricing(entry)
	}

	modelPricingStateMu.Lock()
	activeCustomModelPricing = normalized
	activeModelPricing.Store(buildModelPricingSnapshot(installedModelCatalog, activeCustomModelPricing))
	modelPricingStateMu.Unlock()
	return nil
}

// InstallCustomModelPricingJSON parses and atomically installs the setting value.
func InstallCustomModelPricingJSON(value string) error {
	pricing, err := ParseCustomModelPricing(value)
	if err != nil {
		return err
	}
	return InstallCustomModelPricing(pricing)
}

// InstallModelCustomPricingJSON is an alias matching the setting name.
func InstallModelCustomPricingJSON(value string) error { return InstallCustomModelPricingJSON(value) }

func validateCustomModelPricing(pricing ModelPricing) error {
	// 固定按次费率只由系统目录维护（图像模型）；自定义价格不提供该字段，
	// 若放行会让程序化调用塞进一个前端无法编辑、却覆盖系统图像计费的值。
	if pricing.FixedCostPerRequest != 0 {
		return fmt.Errorf("fixed_cost_per_request is not configurable")
	}
	for name, value := range map[string]float64{
		"input_price": pricing.InputPrice, "output_price": pricing.OutputPrice,
		"cache_read_price": pricing.CacheReadPrice, "cache_read_price_high": pricing.CacheReadPriceHigh,
		"cache_write_price": pricing.CacheWritePrice, "cache_write_price_high": pricing.CacheWritePriceHigh,
		"input_price_high": pricing.InputPriceHigh, "output_price_high": pricing.OutputPriceHigh,
	} {
		if err := validateCustomPrice(name, value); err != nil {
			return err
		}
	}
	return nil
}

// CurrentCustomModelPricing returns a detached copy for diagnostics/tests.
func CurrentCustomModelPricing() map[string]ModelPricing {
	modelPricingStateMu.Lock()
	defer modelPricingStateMu.Unlock()
	result := make(map[string]ModelPricing, len(activeCustomModelPricing))
	for id, pricing := range activeCustomModelPricing {
		result[id] = cloneModelPricing(pricing)
	}
	return result
}
