package util

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// ModelCatalogSchemaVersion 是持久化归一化目录的版本。
const ModelCatalogSchemaVersion = 1

// ModelsDevOfficialProviders 是允许覆盖内置官方价格的 models.dev provider。
var ModelsDevOfficialProviders = []string{
	"openai", "anthropic", "google", "xai", "deepseek", "alibaba",
	"zai", "minimax", "moonshotai", "xiaomi", "mistral",
}

var modelsDevProviderPriority = func() map[string]int {
	priority := make(map[string]int, len(ModelsDevOfficialProviders))
	for index, provider := range ModelsDevOfficialProviders {
		priority[provider] = index
	}
	return priority
}()

// ModelCatalogEntry 是一个可计费模型的归一化官方元数据和价格。
type ModelCatalogEntry struct {
	ID               string       `json:"id"`
	Provider         string       `json:"provider"`
	ReleaseDate      string       `json:"release_date,omitempty"`
	LastUpdated      string       `json:"last_updated,omitempty"`
	Status           string       `json:"status,omitempty"`
	OutputModalities []string     `json:"output_modalities,omitempty"`
	Pricing          ModelPricing `json:"pricing"`
}

// ModelCatalogSnapshot 是可持久化的、已归一化的模型目录。
type ModelCatalogSnapshot struct {
	Version       int                 `json:"version"`
	Source        string              `json:"source"`
	ETag          string              `json:"etag,omitempty"`
	FetchedAt     time.Time           `json:"fetched_at"`
	SkippedModels int                 `json:"skipped_models,omitempty"`
	Models        []ModelCatalogEntry `json:"models"`
}

type modelsDevProvider struct {
	ID     string                    `json:"id"`
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	ID          string              `json:"id"`
	ReleaseDate string              `json:"release_date"`
	LastUpdated string              `json:"last_updated"`
	Status      string              `json:"status"`
	Modalities  modelsDevModalities `json:"modalities"`
	Cost        *modelsDevCost      `json:"cost"`
}

type modelsDevModalities struct {
	Output []string `json:"output"`
}

type modelsDevCost struct {
	Input     *float64            `json:"input"`
	Output    *float64            `json:"output"`
	CacheRead *float64            `json:"cache_read"`
	Tiers     []modelsDevCostTier `json:"tiers"`
}

type modelsDevCostTier struct {
	Input     *float64       `json:"input"`
	Output    *float64       `json:"output"`
	CacheRead *float64       `json:"cache_read"`
	Tier      *modelsDevTier `json:"tier"`
}

type modelsDevTier struct {
	Type string `json:"type"`
	Size int    `json:"size"`
}

// ParseModelsDevCatalog 将 models.dev 原始 JSON 归一化为可安装的官方目录。
func ParseModelsDevCatalog(r io.Reader, etag string, fetchedAt time.Time) (*ModelCatalogSnapshot, error) {
	decoder := json.NewDecoder(r)
	var providers map[string]modelsDevProvider
	if err := decoder.Decode(&providers); err != nil {
		return nil, fmt.Errorf("decode models.dev catalog: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode models.dev catalog: multiple JSON values")
		}
		return nil, fmt.Errorf("decode models.dev catalog trailing data: %w", err)
	}

	modelsByID := make(map[string]ModelCatalogEntry)
	skippedModels := 0
	for _, provider := range modelsDevOfficialProvidersInPriorityOrder() {
		rawProvider, ok := providers[provider]
		if !ok {
			return nil, fmt.Errorf("models.dev catalog is missing allowlisted provider %q", provider)
		}
		if strings.ToLower(strings.TrimSpace(rawProvider.ID)) != provider {
			return nil, fmt.Errorf("models.dev catalog provider %q has mismatched id %q", provider, rawProvider.ID)
		}

		modelKeys := make([]string, 0, len(rawProvider.Models))
		for key := range rawProvider.Models {
			modelKeys = append(modelKeys, key)
		}
		sort.Strings(modelKeys)
		validModels := 0
		for _, key := range modelKeys {
			entry, ok := normalizeModelsDevModel(provider, rawProvider.Models[key])
			if !ok {
				skippedModels++
				continue
			}
			validModels++
			if _, exists := modelsByID[entry.ID]; !exists {
				modelsByID[entry.ID] = entry
			}
		}
		if validModels == 0 {
			return nil, fmt.Errorf("models.dev catalog provider %q contains no valid token-priced models", provider)
		}
	}
	if len(modelsByID) == 0 {
		return nil, fmt.Errorf("models.dev catalog contains no valid official models")
	}

	models := make([]ModelCatalogEntry, 0, len(modelsByID))
	for _, entry := range modelsByID {
		models = append(models, entry)
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].ID < models[j].ID
	})

	return &ModelCatalogSnapshot{
		Version:       ModelCatalogSchemaVersion,
		Source:        "models.dev",
		ETag:          etag,
		FetchedAt:     fetchedAt,
		SkippedModels: skippedModels,
		Models:        models,
	}, nil
}

func modelsDevOfficialProvidersInPriorityOrder() []string {
	providers := make([]string, 0, len(modelsDevProviderPriority))
	for provider := range modelsDevProviderPriority {
		providers = append(providers, provider)
	}
	sort.Slice(providers, func(i, j int) bool {
		return modelsDevProviderPriority[providers[i]] < modelsDevProviderPriority[providers[j]]
	})
	return providers
}

func normalizeModelsDevModel(provider string, raw modelsDevModel) (ModelCatalogEntry, bool) {
	id := strings.TrimSpace(raw.ID)
	prefix := provider + "/"
	if strings.HasPrefix(strings.ToLower(id), prefix) {
		id = id[len(prefix):]
	}
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" || raw.Cost == nil || raw.Cost.Input == nil || raw.Cost.Output == nil ||
		!validCatalogPrice(*raw.Cost.Input) || !validCatalogPrice(*raw.Cost.Output) {
		return ModelCatalogEntry{}, false
	}

	pricing := ModelPricing{
		InputPrice:  *raw.Cost.Input,
		OutputPrice: *raw.Cost.Output,
	}
	if raw.Cost.CacheRead != nil {
		if !validCatalogPrice(*raw.Cost.CacheRead) {
			return ModelCatalogEntry{}, false
		}
		pricing.CacheReadPrice = *raw.Cost.CacheRead
		pricing.HasCacheReadPrice = true
	}

	tiers, ok := normalizeModelsDevContextTiers(raw.Cost, pricing)
	if !ok {
		return ModelCatalogEntry{}, false
	}
	pricing.TokenPricingTiers = tiers
	pricing.CacheReadCountsTowardTier = provider == "openai" && len(tiers) > 0

	return ModelCatalogEntry{
		ID:               id,
		Provider:         provider,
		ReleaseDate:      raw.ReleaseDate,
		LastUpdated:      raw.LastUpdated,
		Status:           raw.Status,
		OutputModalities: append([]string(nil), raw.Modalities.Output...),
		Pricing:          pricing,
	}, true
}

func normalizeModelsDevContextTiers(cost *modelsDevCost, base ModelPricing) ([]TokenPricingTier, bool) {
	type contextTier struct {
		size    int
		pricing TokenPricingTier
	}

	contextTiers := make([]contextTier, 0, len(cost.Tiers))
	for _, raw := range cost.Tiers {
		if raw.Tier == nil || raw.Tier.Type != "context" {
			continue
		}
		if raw.Tier.Size <= 0 || raw.Input == nil || raw.Output == nil ||
			!validCatalogPrice(*raw.Input) || !validCatalogPrice(*raw.Output) {
			return nil, false
		}
		tier := TokenPricingTier{
			InputPrice:  *raw.Input,
			OutputPrice: *raw.Output,
		}
		if raw.CacheRead != nil {
			if !validCatalogPrice(*raw.CacheRead) {
				return nil, false
			}
			tier.CacheReadPrice = *raw.CacheRead
			tier.HasCacheReadPrice = true
		}
		contextTiers = append(contextTiers, contextTier{size: raw.Tier.Size, pricing: tier})
	}
	if len(contextTiers) == 0 {
		return nil, true
	}

	sort.Slice(contextTiers, func(i, j int) bool {
		return contextTiers[i].size < contextTiers[j].size
	})
	for index := 1; index < len(contextTiers); index++ {
		if contextTiers[index-1].size == contextTiers[index].size {
			return nil, false
		}
	}

	tiers := make([]TokenPricingTier, 0, len(contextTiers)+1)
	tiers = append(tiers, TokenPricingTier{
		MaxInputTokens:    contextTiers[0].size,
		InputPrice:        base.InputPrice,
		OutputPrice:       base.OutputPrice,
		CacheReadPrice:    base.CacheReadPrice,
		HasCacheReadPrice: base.HasCacheReadPrice,
	})
	for index, contextTier := range contextTiers {
		tier := contextTier.pricing
		if index+1 < len(contextTiers) {
			tier.MaxInputTokens = contextTiers[index+1].size
		}
		tiers = append(tiers, tier)
	}
	return tiers, true
}

func validCatalogPrice(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

// Model 返回指定模型的副本，避免调用方修改快照内部 slice。
func (s *ModelCatalogSnapshot) Model(id string) (ModelCatalogEntry, bool) {
	if s == nil {
		return ModelCatalogEntry{}, false
	}
	id = strings.ToLower(strings.TrimSpace(id))
	for _, entry := range s.Models {
		if entry.ID == id {
			return cloneModelCatalogEntry(entry), true
		}
	}
	return ModelCatalogEntry{}, false
}

func cloneModelCatalogEntry(entry ModelCatalogEntry) ModelCatalogEntry {
	entry.OutputModalities = append([]string(nil), entry.OutputModalities...)
	entry.Pricing = cloneModelPricing(entry.Pricing)
	return entry
}

// InstallModelCatalog 验证并原子安装一份归一化模型目录。
func InstallModelCatalog(snapshot *ModelCatalogSnapshot, source string) error {
	if snapshot == nil {
		return fmt.Errorf("model catalog is nil")
	}
	if snapshot.Version != ModelCatalogSchemaVersion {
		return fmt.Errorf("unsupported model catalog version %d", snapshot.Version)
	}
	source = strings.TrimSpace(source)
	if source == "" {
		return fmt.Errorf("model catalog source is empty")
	}
	if len(snapshot.Models) == 0 {
		return fmt.Errorf("model catalog contains no models")
	}
	if snapshot.SkippedModels < 0 {
		return fmt.Errorf("model catalog has negative skipped model count")
	}

	installed := &ModelCatalogSnapshot{
		Version:       snapshot.Version,
		Source:        source,
		ETag:          snapshot.ETag,
		FetchedAt:     snapshot.FetchedAt,
		SkippedModels: snapshot.SkippedModels,
		Models:        make([]ModelCatalogEntry, 0, len(snapshot.Models)),
	}
	seen := make(map[string]struct{}, len(snapshot.Models))
	for _, entry := range snapshot.Models {
		entry = cloneModelCatalogEntry(entry)
		entry.ID = strings.ToLower(strings.TrimSpace(entry.ID))
		entry.Provider = strings.ToLower(strings.TrimSpace(entry.Provider))
		if err := validateModelCatalogEntry(entry); err != nil {
			return err
		}
		if _, exists := seen[entry.ID]; exists {
			return fmt.Errorf("duplicate model catalog id %q", entry.ID)
		}
		seen[entry.ID] = struct{}{}
		installed.Models = append(installed.Models, entry)
	}
	sort.Slice(installed.Models, func(i, j int) bool {
		return installed.Models[i].ID < installed.Models[j].ID
	})

	activeModelPricing.Store(buildModelPricingSnapshot(installed))
	return nil
}

func validateModelCatalogEntry(entry ModelCatalogEntry) error {
	if entry.ID == "" {
		return fmt.Errorf("model catalog contains empty id")
	}
	if _, ok := modelsDevProviderPriority[entry.Provider]; !ok {
		return fmt.Errorf("model catalog model %q has unsupported provider %q", entry.ID, entry.Provider)
	}
	if !validCatalogPrice(entry.Pricing.InputPrice) || !validCatalogPrice(entry.Pricing.OutputPrice) ||
		!validCatalogPrice(entry.Pricing.CacheReadPrice) || !validCatalogPrice(entry.Pricing.CacheReadPriceHigh) ||
		!validCatalogPrice(entry.Pricing.InputPriceHigh) || !validCatalogPrice(entry.Pricing.OutputPriceHigh) ||
		!validCatalogPrice(entry.Pricing.FixedCostPerRequest) {
		return fmt.Errorf("model catalog model %q has invalid price", entry.ID)
	}
	for index, tier := range entry.Pricing.TokenPricingTiers {
		if tier.MaxInputTokens < 0 || !validCatalogPrice(tier.InputPrice) || !validCatalogPrice(tier.OutputPrice) || !validCatalogPrice(tier.CacheReadPrice) {
			return fmt.Errorf("model catalog model %q has invalid tier", entry.ID)
		}
		if tier.MaxInputTokens == 0 && index != len(entry.Pricing.TokenPricingTiers)-1 {
			return fmt.Errorf("model catalog model %q has non-final open tier", entry.ID)
		}
		if index > 0 && tier.MaxInputTokens != 0 && tier.MaxInputTokens <= entry.Pricing.TokenPricingTiers[index-1].MaxInputTokens {
			return fmt.Errorf("model catalog model %q has unordered tiers", entry.ID)
		}
	}
	return nil
}

// RestoreEmbeddedModelCatalog 丢弃远端目录并恢复编译期定价表。
func RestoreEmbeddedModelCatalog() {
	activeModelPricing.Store(buildModelPricingSnapshot(nil))
}

// CurrentModelCatalogETag 返回当前已安装远端目录的 ETag。
func CurrentModelCatalogETag() string {
	snapshot := activeModelPricing.Load()
	if snapshot == nil {
		return ""
	}
	return snapshot.remoteETag
}

// ModelCatalogSummary 是已安装目录的同步观测元数据。
type ModelCatalogSummary struct {
	ModelCount        int
	ProviderCount     int
	SkippedModelCount int
	ETag              string
}

// CurrentModelCatalogSummary 返回当前不可变目录快照的统计信息。
func CurrentModelCatalogSummary() ModelCatalogSummary {
	snapshot := activeModelPricing.Load()
	if snapshot == nil {
		return ModelCatalogSummary{}
	}

	providers := make(map[string]struct{}, len(snapshot.metadata))
	for _, entry := range snapshot.metadata {
		providers[entry.Provider] = struct{}{}
	}
	return ModelCatalogSummary{
		ModelCount:        len(snapshot.metadata),
		ProviderCount:     len(providers),
		SkippedModelCount: snapshot.remoteSkippedModels,
		ETag:              snapshot.remoteETag,
	}
}
