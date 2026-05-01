package util

import "testing"

func BenchmarkFuzzyMatchModel_Hit(b *testing.B) {
	models := []string{
		"claude-sonnet-4-6-20250101",
		"gpt-5.4-preview",
		"gemini-2.5-pro",
		"qwen-plus-1m",
		"deepseek-r1-0528",
		"grok-4-fast",
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		_, _ = fuzzyMatchModel(models[i%len(models)])
		i++
	}
}

func BenchmarkFuzzyMatchModel_Miss(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		_, _ = fuzzyMatchModel("zzz-unknown-model-9999")
	}
}
