package storage

import (
	"fmt"
	"testing"

	"ccLoad/internal/model"
)

var benchmarkConfigsSink []*model.Config

func benchmarkChannelConfigs(count int) []*model.Config {
	configs := make([]*model.Config, count)
	for i := range configs {
		models := make([]model.ModelEntry, 12)
		for j := range models {
			models[j] = model.ModelEntry{Model: fmt.Sprintf("model-%d", j)}
		}
		configs[i] = &model.Config{
			ID:           int64(i + 1),
			Name:         fmt.Sprintf("channel-%d", i+1),
			AuthType:     model.AuthTypeAntigravityOAuth,
			URLs:         model.ChannelURLs{{URL: "https://daily.example.com", Protocols: []string{"gemini"}}, {URL: "https://prod.example.com", Protocols: []string{"gemini"}}},
			Priority:     10,
			Enabled:      true,
			KeyCount:     1,
			ModelEntries: models,
		}
	}
	return configs
}

func BenchmarkDeepCopyConfigs3000(b *testing.B) {
	configs := benchmarkChannelConfigs(3000)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkConfigsSink = deepCopyConfigs(configs)
	}
}

func BenchmarkReadOnlyConfigSnapshot3000(b *testing.B) {
	configs := benchmarkChannelConfigs(3000)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		benchmarkConfigsSink = copyConfigPointers(configs)
	}
}
