package util_test

import (
	"bytes"
	"encoding/json"
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
		"release_date": "2026-07-09",
		"last_updated": "2026-07-10",
		"status":       "active",
		"modalities":   map[string]any{"output": []string{"text"}},
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
	if entry.ReleaseDate != "2026-07-09" || entry.LastUpdated != "2026-07-10" || entry.Status != "active" || !reflect.DeepEqual(entry.OutputModalities, []string{"text"}) {
		t.Fatalf("metadata = %#v", entry)
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
}

func TestParseModelsDevCatalogNonOpenAIContextTierDoesNotCountCacheRead(t *testing.T) {
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
	entry, ok := snapshot.Model("claude-next")
	if !ok {
		t.Fatalf("entry not found: %#v", snapshot)
	}
	if entry.Pricing.CacheReadCountsTowardTier {
		t.Fatalf("non-OpenAI context tier counted cache reads: %#v", entry.Pricing)
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

func validModelsDevFixture(t *testing.T, targetProvider, targetID string, override map[string]any) []byte {
	t.Helper()
	providers := validModelsDevProviders()
	if targetProvider != "" {
		model := validModelsDevModel(targetProvider+"/"+targetID, 1, 2)
		for key, value := range override {
			model[key] = value
		}
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
