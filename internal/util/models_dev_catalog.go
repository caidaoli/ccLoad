package util

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

/**
 * @brief 已分别保存思考能力是否明确及各候选等级的目录缓存版本。
 */
const ModelCatalogSchemaVersion = 3

/**
 * @brief 不含思考等级字段的旧版缓存格式
 * @details 仍可安装价格；安装时丢弃 ETag，使下一次同步全量拉取等级数据。
 */
const modelCatalogSchemaVersionWithoutReasoning = 1

/**
 * @brief 只保留已声明等级、无法区分预算型和缺少能力信息的旧版缓存格式。
 */
const modelCatalogSchemaVersionWithEfforts = 2

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

/**
 * @brief 可计费模型的归一化价格和已声明能力。
 */
type ModelCatalogEntry struct {
	ID               string       `json:"id"`
	Provider         string       `json:"provider"`
	Family           string       `json:"family,omitempty"`
	ReleaseDate      string       `json:"release_date,omitempty"`
	LastUpdated      string       `json:"last_updated,omitempty"`
	Status           string       `json:"status,omitempty"`
	OutputModalities []string     `json:"output_modalities,omitempty"`
	ReasoningEfforts []string     `json:"reasoning_efforts,omitempty"` ///< models.dev 声明的思考等级，保持上游顺序
	ReasoningKnown   bool         `json:"reasoning_known,omitempty"`   ///< 上游显式声明思考选项或明确不支持思考
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

/**
 * @brief models.dev 模型的计费与能力输入字段。
 */
type modelsDevModel struct {
	ID          string              `json:"id"`
	Family      string              `json:"family"`
	ReleaseDate string              `json:"release_date"`
	LastUpdated string              `json:"last_updated"`
	Status      string              `json:"status"`
	Modalities  modelsDevModalities `json:"modalities"`
	Cost        *modelsDevCost      `json:"cost"`
	Reasoning   *bool               `json:"reasoning"`         ///< nil 表示未声明是否支持思考。
	Options     []modelsDevOption   `json:"reasoning_options"` ///< nil 表示上游未提供选项列表。
}

/**
 * @brief models.dev 的单个推理参数选项
 */
type modelsDevOption struct {
	Type   string   `json:"type"`   ///< 选项类型；只读取 effort
	Values []string `json:"values"` ///< effort 可选等级
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

/**
 * @brief 归一化官方模型价格及可独立核实的思考能力。
 * @param provider 已通过白名单验证的提供商 ID。
 * @param raw 上游模型的价格及元数据。
 * @return 模型目录条目；模型 ID 或价格无效时为零值。
 * @return 有可安装的模型条目时为 true。
 */
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
	// OpenAI 与 Anthropic 的 context tier 按完整 prompt（含缓存）判定；其他 provider 只看非缓存输入。
	pricing.CacheReadCountsTowardTier = (provider == "openai" || provider == "anthropic") && len(tiers) > 0

	return ModelCatalogEntry{
		ID:               id,
		Provider:         provider,
		Family:           strings.ToLower(strings.TrimSpace(raw.Family)),
		ReleaseDate:      raw.ReleaseDate,
		LastUpdated:      raw.LastUpdated,
		Status:           raw.Status,
		OutputModalities: append([]string(nil), raw.Modalities.Output...),
		ReasoningEfforts: modelsDevReasoningEfforts(raw.Options),
		ReasoningKnown:   raw.Options != nil || raw.Reasoning != nil && !*raw.Reasoning,
		Pricing:          pricing,
	}, true
}

/**
 * @brief 提取 models.dev effort 选项的等级
 * @param options 模型的 reasoning_options
 * @return 小写去重后的等级；没有 effort 选项时返回 nil
 */
func modelsDevReasoningEfforts(options []modelsDevOption) []string {
	var efforts []string
	for _, option := range options {
		if option.Type != "effort" {
			continue
		}
		for _, value := range option.Values {
			value = strings.ToLower(strings.TrimSpace(value))
			if value != "" && !slices.Contains(efforts, value) {
				efforts = append(efforts, value)
			}
		}
	}
	return efforts
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
	id = strings.ToLower(strings.TrimSpace(id))
	for _, entry := range s.Models {
		if entry.ID == id {
			return cloneModelCatalogEntry(entry), true
		}
	}
	return ModelCatalogEntry{}, false
}

// ModelOutputModalities returns known output modalities for an exact model ID.
// An absent entry or empty modalities means the catalog cannot classify it.
func ModelOutputModalities(id string) ([]string, bool) {
	snapshot := activeModelPricing.Load()
	if snapshot == nil {
		return nil, false
	}
	entry, ok := snapshot.metadata[strings.ToLower(strings.TrimSpace(id))]
	if !ok || len(entry.OutputModalities) == 0 {
		return nil, false
	}
	return append([]string(nil), entry.OutputModalities...), true
}

/**
 * @brief 查询 models.dev 声明的模型思考等级
 * @param id 模型 ID，大小写无关
 * @return 等级副本；目录未安装、未收录或未声明等级时返回 nil
 * @return 是否有明确的思考能力信息；仅有价格或其他元数据时为 false
 */
func ModelReasoningEfforts(id string) ([]string, bool) {
	snapshot := activeModelPricing.Load()
	if snapshot == nil {
		return nil, false
	}
	entry := snapshot.metadata[strings.ToLower(strings.TrimSpace(id))]
	if len(entry.ReasoningEfforts) == 0 {
		return nil, entry.ReasoningKnown
	}
	return slices.Clone(entry.ReasoningEfforts), true
}

// ModelFamily returns the known model family for an exact model ID.
func ModelFamily(id string) string {
	snapshot := activeModelPricing.Load()
	if snapshot == nil {
		return ""
	}
	return snapshot.metadata[strings.ToLower(strings.TrimSpace(id))].Family
}

func cloneModelCatalogEntry(entry ModelCatalogEntry) ModelCatalogEntry {
	entry.OutputModalities = append([]string(nil), entry.OutputModalities...)
	entry.ReasoningEfforts = append([]string(nil), entry.ReasoningEfforts...)
	entry.Pricing = cloneModelPricing(entry.Pricing)
	return entry
}

/**
 * @brief 验证并原子安装一份模型目录；未声明能力的旧快照仍可提供价格。
 * @param snapshot 来自同步或本地缓存的目录快照。
 * @param source 表示缓存或远程来源的非空标识。
 * @return 快照或来源无效时返回错误；成功时为 nil。
 * @note 并发安全；旧格式的 ETag 不会用于下一次同步。
 */
func InstallModelCatalog(snapshot *ModelCatalogSnapshot, source string) error {
	if snapshot == nil {
		return fmt.Errorf("model catalog is nil")
	}
	etag := snapshot.ETag
	switch snapshot.Version {
	case ModelCatalogSchemaVersion:
	case modelCatalogSchemaVersionWithoutReasoning, modelCatalogSchemaVersionWithEfforts:
		// 旧格式仍可提供价格及已有等级，下次同步须补齐能力信息。
		etag = ""
	default:
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
		Version:       ModelCatalogSchemaVersion,
		Source:        source,
		ETag:          etag,
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

	modelPricingStateMu.Lock()
	installedModelCatalog = installed
	activeModelPricing.Store(buildModelPricingSnapshot(installedModelCatalog, activeCustomModelPricing))
	modelPricingStateMu.Unlock()
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

// RestoreEmbeddedModelCatalog 丢弃远端目录并恢复编译期定价表；保留当前自定义价格覆盖。
func RestoreEmbeddedModelCatalog() {
	modelPricingStateMu.Lock()
	installedModelCatalog = nil
	activeModelPricing.Store(buildModelPricingSnapshot(nil, activeCustomModelPricing))
	modelPricingStateMu.Unlock()
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
