package util_test

import (
	"bytes"
	"encoding/json"
	"maps"
	"math"
	"reflect"
	"testing"
	"time"

	"ccLoad/internal/util"
)

func TestParseModelsDevCatalogNormalizesOfficialPrices(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	raw := validModelsDevFixture(t, "openai", "gpt-next", map[string]any{
		"id":           "openai/gpt-next",
		"family":       "gpt",
		"release_date": "2026-07-09",
		"last_updated": "2026-07-10",
		"status":       "active",
		"modalities":   map[string]any{"output": []string{"text"}},
		"reasoning_options": []any{
			map[string]any{"type": "toggle"},
			map[string]any{"type": "effort", "values": []string{"Low", "medium", "xhigh", "low"}},
			map[string]any{"type": "budget_tokens", "min": 0, "max": 262144},
		},
		"cost": map[string]any{
			"input": 2.5, "output": 15.0, "cache_read": 0.25,
			"tiers": []any{map[string]any{
				"input": 5.0, "output": 22.5, "cache_read": 0.5,
				"tier": map[string]any{"type": "context", "size": 272000},
			}},
		},
	})

	snapshot, err := util.ParseModelsDevCatalog(bytes.NewReader(raw), `"etag-1"`, now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != util.ModelCatalogSchemaVersion || snapshot.Source != "models.dev" || snapshot.ETag != `"etag-1"` || !snapshot.FetchedAt.Equal(now) {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	entry, ok := snapshot.Model("gpt-next")
	if !ok || entry.Provider != "openai" {
		t.Fatalf("entry = %#v", entry)
	}
	if entry.ReleaseDate != "2026-07-09" || entry.LastUpdated != "2026-07-10" || entry.Status != "active" || entry.Family != "gpt" || !reflect.DeepEqual(entry.OutputModalities, []string{"text"}) {
		t.Fatalf("metadata = %#v", entry)
	}
	if want := []string{"low", "medium", "xhigh"}; !reflect.DeepEqual(entry.ReasoningEfforts, want) {
		t.Fatalf("reasoning efforts = %#v, want %#v", entry.ReasoningEfforts, want)
	}
	if entry.Pricing.InputPrice != 2.5 || entry.Pricing.OutputPrice != 15 || entry.Pricing.CacheReadPrice != 0.25 || !entry.Pricing.HasCacheReadPrice {
		t.Fatalf("pricing = %#v", entry.Pricing)
	}
	if !entry.Pricing.CacheReadCountsTowardTier {
		t.Fatalf("OpenAI context tier did not count cache reads: %#v", entry.Pricing)
	}
	if len(entry.Pricing.TokenPricingTiers) != 2 {
		t.Fatalf("tiers = %#v", entry.Pricing.TokenPricingTiers)
	}
	base, high := entry.Pricing.TokenPricingTiers[0], entry.Pricing.TokenPricingTiers[1]
	if base.MaxInputTokens != 272000 || base.InputPrice != 2.5 || base.OutputPrice != 15 || base.CacheReadPrice != 0.25 || !base.HasCacheReadPrice {
		t.Fatalf("base tier = %#v", base)
	}
	if high.MaxInputTokens != 0 || high.InputPrice != 5 || high.OutputPrice != 22.5 || high.CacheReadPrice != 0.5 || !high.HasCacheReadPrice {
		t.Fatalf("high tier = %#v", high)
	}

	util.RestoreEmbeddedModelCatalog()
	t.Cleanup(util.RestoreEmbeddedModelCatalog)
	if err := util.InstallModelCatalog(snapshot, "models.dev"); err != nil {
		t.Fatal(err)
	}
	if got, want := util.CalculateCostDetailed("gpt-next", 100_000, 1_000, 200_000, 0, 0), 0.6225; math.Abs(got-want) > 0.000001 {
		t.Fatalf("installed OpenAI tiered cost = %v, want %v", got, want)
	}
	if got, known := util.ModelReasoningEfforts("GPT-Next"); !known || !reflect.DeepEqual(got, []string{"low", "medium", "xhigh"}) {
		t.Fatalf("installed reasoning efforts = %#v known=%v", got, known)
	}
	if got, known := util.ModelReasoningEfforts("openai-model"); got != nil || known {
		t.Fatalf("uncatalogued model = %#v known=%v, want nil unknown", got, known)
	}
}

func TestParseModelsDevCatalogGoogleContextTierDoesNotCountCacheRead(t *testing.T) {
	raw := validModelsDevFixture(t, "google", "gemini-next", map[string]any{
		"cost": map[string]any{
			"input": 3.0, "output": 15.0, "cache_read": 0.3,
			"tiers": []any{map[string]any{
				"input": 6.0, "output": 22.5, "cache_read": 0.6,
				"tier": map[string]any{"type": "context", "size": 200000},
			}},
		},
	})

	snapshot, err := util.ParseModelsDevCatalog(bytes.NewReader(raw), "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := snapshot.Model("gemini-next")
	if !ok {
		t.Fatalf("entry not found: %#v", snapshot)
	}
	if entry.Pricing.CacheReadCountsTowardTier {
		t.Fatalf("Gemini context tier counted cache reads: %#v", entry.Pricing)
	}
}

// Anthropic 长上下文按 input + cache_read + cache_creation 判定 200K：目录同步后仍须如此。
func TestParseModelsDevCatalogAnthropicContextTierCountsPromptCache(t *testing.T) {
	raw := validModelsDevFixture(t, "anthropic", "claude-next", map[string]any{
		"cost": map[string]any{
			"input": 3.0, "output": 15.0, "cache_read": 0.3,
			"tiers": []any{map[string]any{
				"input": 6.0, "output": 22.5, "cache_read": 0.6,
				"tier": map[string]any{"type": "context", "size": 200000},
			}},
		},
	})

	snapshot, err := util.ParseModelsDevCatalog(bytes.NewReader(raw), "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(util.RestoreEmbeddedModelCatalog)
	if err := util.InstallModelCatalog(snapshot, "models.dev"); err != nil {
		t.Fatal(err)
	}
	// 1K 非缓存输入 + 150K 缓存读 + 60K 缓存写 = 211K > 200K，整单走高档价。
	got := util.CalculateCostDetailed("claude-next", 1_000, 1_000, 150_000, 60_000, 0)
	want := (1_000*6.0 + 1_000*22.5 + 150_000*0.6 + 60_000*6.0*1.25) / 1_000_000
	if math.Abs(got-want) > 0.000001 {
		t.Fatalf("installed Anthropic tiered cost = %v, want %v", got, want)
	}
}

func TestParseModelsDevCatalogUsesOfficialProviderPriority(t *testing.T) {
	providers := validModelsDevProviders()
	providers["anthropic"] = modelsDevProvider("anthropic", map[string]any{
		"shared-model": validModelsDevModel("anthropic/shared-model", 3, 15),
	})
	providers["openai"] = modelsDevProvider("openai", map[string]any{
		"shared-model": validModelsDevModel("openai/shared-model", 2, 8),
	})
	raw := marshalModelsDevDocument(t, providers)

	snapshot, err := util.ParseModelsDevCatalog(bytes.NewReader(raw), "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := snapshot.Model("shared-model")
	if !ok || entry.Provider != "openai" || entry.Pricing.InputPrice != 2 {
		t.Fatalf("entry = %#v", entry)
	}
}

func TestParseModelsDevCatalogRejectsMissingProviderAndSkipsInvalidModels(t *testing.T) {
	valid := validModelsDevModel("openai/good-model", 2, 8)
	invalid := validModelsDevModel("openai/bad-model", -1, 8)
	providers := validModelsDevProviders()
	providers["openai"] = modelsDevProvider("openai", map[string]any{
		"good-model": valid,
		"bad-model":  invalid,
	})
	providers["unofficial"] = modelsDevProvider("unofficial", map[string]any{
		"unofficial-model": validModelsDevModel("unofficial/unofficial-model", 1, 1),
	})
	raw := marshalModelsDevDocument(t, providers)

	snapshot, err := util.ParseModelsDevCatalog(bytes.NewReader(raw), "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snapshot.Model("good-model"); !ok {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if _, ok := snapshot.Model("bad-model"); ok {
		t.Fatalf("invalid model was installed: %#v", snapshot)
	}
	if _, ok := snapshot.Model("unofficial-model"); ok {
		t.Fatalf("unofficial provider was installed: %#v", snapshot)
	}

	missingProviders := validModelsDevProviders()
	missingProviders["openai"] = map[string]any{
		"models": map[string]any{"good-model": valid},
	}
	missingProvider := marshalModelsDevDocument(t, missingProviders)
	if _, err := util.ParseModelsDevCatalog(bytes.NewReader(missingProvider), "", time.Time{}); err == nil {
		t.Fatal("missing official provider identity was accepted")
	}
}

func TestParseModelsDevCatalogRejectsIncompleteAllowlistedProvider(t *testing.T) {
	t.Run("missing provider while every other provider is valid", func(t *testing.T) {
		providers := validModelsDevProviders()
		delete(providers, "anthropic")

		if _, err := util.ParseModelsDevCatalog(bytes.NewReader(marshalModelsDevDocument(t, providers)), "", time.Time{}); err == nil {
			t.Fatal("catalog accepted a missing allowlisted provider")
		}
	})

	t.Run("provider with no valid token-priced models", func(t *testing.T) {
		providers := validModelsDevProviders()
		providers["anthropic"] = modelsDevProvider("anthropic", map[string]any{
			"invalid-model": validModelsDevModel("anthropic/invalid-model", -1, 2),
		})

		if _, err := util.ParseModelsDevCatalog(bytes.NewReader(marshalModelsDevDocument(t, providers)), "", time.Time{}); err == nil {
			t.Fatal("catalog accepted an allowlisted provider with no valid models")
		}
	})
}

func TestModelCatalogInstallIsImmutable(t *testing.T) {
	util.RestoreEmbeddedModelCatalog()
	t.Cleanup(util.RestoreEmbeddedModelCatalog)

	snapshot := &util.ModelCatalogSnapshot{
		Version: util.ModelCatalogSchemaVersion,
		ETag:    `"etag-2"`,
		Models: []util.ModelCatalogEntry{
			{
				ID: "gpt-older", Provider: "openai", ReleaseDate: "2026-07-01", OutputModalities: []string{"text"},
				Pricing: util.ModelPricing{InputPrice: 2, OutputPrice: 8},
			},
			{
				ID: "gpt-newer", Provider: "openai", ReleaseDate: "2026-07-09", OutputModalities: []string{"text"},
				Pricing: util.ModelPricing{InputPrice: 3, OutputPrice: 9},
			},
			{
				ID: "gpt-image", Provider: "openai", ReleaseDate: "2026-07-10", OutputModalities: []string{"image"},
				Pricing: util.ModelPricing{InputPrice: 1, OutputPrice: 1},
			},
		},
	}
	if err := util.InstallModelCatalog(snapshot, "models.dev"); err != nil {
		t.Fatal(err)
	}

	snapshot.Models[1].Pricing.InputPrice = 999
	snapshot.Models[1].OutputModalities[0] = "image"
	if got := util.CalculateCostDetailed("gpt-newer", 1_000_000, 0, 0, 0, 0); got != 3 {
		t.Fatalf("catalog changed after caller mutation: cost = %v", got)
	}
}

/**
 * @brief 验证 v1 缓存保留原价格，思考能力保持未知并触发下次完整同步。
 * @param t 测试上下文。
 */
func TestModelCatalogLegacyCachePricingStillDecodes(t *testing.T) {
	t.Cleanup(util.RestoreEmbeddedModelCatalog)
	util.RestoreEmbeddedModelCatalog()

	const legacyCache = `{
  "version": 1,
  "source": "models.dev",
  "etag": "legacy-etag",
  "models": [{
    "id": "legacy-cached-model",
    "provider": "openai",
    "pricing": {
      "InputPrice": 7,
      "OutputPrice": 11,
      "CacheReadPrice": 0.7,
      "HasCacheReadPrice": true
    }
  }]
}`
	var snapshot util.ModelCatalogSnapshot
	if err := json.Unmarshal([]byte(legacyCache), &snapshot); err != nil {
		t.Fatal(err)
	}
	if err := util.InstallModelCatalog(&snapshot, "cache"); err != nil {
		t.Fatal(err)
	}
	if got := util.CalculateCostDetailed("legacy-cached-model", 1_000_000, 1_000_000, 0, 0, 0); got != 18 {
		t.Fatalf("legacy cached model cost = %v, want 18", got)
	}
	if efforts, known := util.ModelReasoningEfforts("legacy-cached-model"); efforts != nil || known {
		t.Fatalf("v1 cache reasoning = %v, %v, want unknown", efforts, known)
	}
	// v1 缓存没有思考等级；丢弃 ETag 才能让下一次同步全量拉取，而不是得到 304。
	if etag := util.CurrentModelCatalogETag(); etag != "" {
		t.Fatalf("legacy cache ETag = %q, want empty", etag)
	}
}

/**
 * @brief 验证只有明确的思考能力信息会否定未声明的等级，旧 v2 缓存可以补齐能力。
 * @param t 测试上下文。
 */
func TestModelCatalogReasoningCapabilityUsesExplicitMetadata(t *testing.T) {
	t.Cleanup(util.RestoreEmbeddedModelCatalog)
	t.Cleanup(func() { _ = util.InstallCustomModelPricing(nil) })
	providers := validModelsDevProviders()
	budget := validModelsDevModel("openai/budget-reasoning", 1, 2)
	budget["reasoning_options"] = []map[string]any{{"type": "budget_tokens", "min": 0, "max": 32_768}}
	noReasoning := validModelsDevModel("openai/no-reasoning", 1, 2)
	noReasoning["reasoning"] = false
	undisclosed := validModelsDevModel("openai/reasoning-unspecified", 1, 2)
	undisclosed["reasoning"] = true
	providers["openai"] = modelsDevProvider("openai", map[string]any{
		"budget-reasoning":      budget,
		"no-reasoning":          noReasoning,
		"reasoning-unspecified": undisclosed,
	})
	snapshot, err := util.ParseModelsDevCatalog(bytes.NewReader(marshalModelsDevDocument(t, providers)), `"etag-v3"`, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// Round trip through the persisted form before installation.
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var cache util.ModelCatalogSnapshot
	if err := json.Unmarshal(data, &cache); err != nil {
		t.Fatal(err)
	}
	if err := util.InstallModelCatalog(&cache, "cache"); err != nil {
		t.Fatal(err)
	}
	if err := util.InstallCustomModelPricing(map[string]util.ModelPricing{
		"private-price-only-model": {InputPrice: 1, OutputPrice: 2},
	}); err != nil {
		t.Fatal(err)
	}
	for id, wantKnown := range map[string]bool{
		"budget-reasoning":         true,
		"no-reasoning":             true,
		"reasoning-unspecified":    false,
		"private-price-only-model": false,
	} {
		if efforts, known := util.ModelReasoningEfforts(id); efforts != nil || known != wantKnown {
			t.Errorf("%s reasoning = %v, %v, want nil, %v", id, efforts, known, wantKnown)
		}
	}
	if etag := util.CurrentModelCatalogETag(); etag != `"etag-v3"` {
		t.Fatalf("v3 cache ETag = %q, want original", etag)
	}

	const previousCache = `{"version":2,"source":"models.dev","etag":"legacy-etag","models":[
		{"id":"cached-effort-model","provider":"openai","reasoning_efforts":["low","high"],"pricing":{"InputPrice":7,"OutputPrice":11}},
		{"id":"cached-price-only-model","provider":"openai","pricing":{"InputPrice":7,"OutputPrice":11}}
	]}`
	var previous util.ModelCatalogSnapshot
	if err := json.Unmarshal([]byte(previousCache), &previous); err != nil {
		t.Fatal(err)
	}
	if err := util.InstallModelCatalog(&previous, "cache"); err != nil {
		t.Fatal(err)
	}
	if efforts, known := util.ModelReasoningEfforts("cached-effort-model"); !known || !reflect.DeepEqual(efforts, []string{"low", "high"}) {
		t.Errorf("v2 cached efforts = %v, %v", efforts, known)
	}
	if efforts, known := util.ModelReasoningEfforts("cached-price-only-model"); efforts != nil || known {
		t.Errorf("v2 cache without reasoning = %v, %v, want unknown", efforts, known)
	}
	if etag := util.CurrentModelCatalogETag(); etag != "" {
		t.Errorf("v2 cache ETag = %q, want empty", etag)
	}
}

func validModelsDevFixture(t *testing.T, targetProvider, targetID string, override map[string]any) []byte {
	t.Helper()
	providers := validModelsDevProviders()
	if targetProvider != "" {
		model := validModelsDevModel(targetProvider+"/"+targetID, 1, 2)
		maps.Copy(model, override)
		providers[targetProvider] = modelsDevProvider(targetProvider, map[string]any{targetID: model})
	}
	return marshalModelsDevDocument(t, providers)
}

func validModelsDevProviders() map[string]any {
	providers := make(map[string]any, len(util.ModelsDevOfficialProviders))
	for _, provider := range util.ModelsDevOfficialProviders {
		id := provider + "-model"
		providers[provider] = modelsDevProvider(provider, map[string]any{
			id: validModelsDevModel(provider+"/"+id, 1, 2),
		})
	}
	return providers
}

func validModelsDevModel(id string, input, output float64) map[string]any {
	return map[string]any{
		"id":         id,
		"modalities": map[string]any{"output": []string{"text"}},
		"cost":       map[string]any{"input": input, "output": output},
	}
}

func modelsDevProvider(id string, models map[string]any) map[string]any {
	return map[string]any{"id": id, "models": models}
}

func marshalModelsDevDocument(t *testing.T, providers map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(providers)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
