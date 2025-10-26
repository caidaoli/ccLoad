package util

import "testing"

// BenchmarkDetectChannelTypeFromPath 测试路径检测性能
func BenchmarkDetectChannelTypeFromPath(b *testing.B) {
	testCases := []struct {
		name string
		path string
	}{
		{"Anthropic", "/v1/messages"},
		{"Codex", "/v1/responses"},
		{"OpenAI_Chat", "/v1/chat/completions"},
		{"OpenAI_Embeddings", "/v1/embeddings"},
		{"Gemini", "/v1beta/models/gemini-pro:streamGenerateContent"},
		{"Unknown", "/unknown/path"},
	}

	for _, tc := range testCases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = DetectChannelTypeFromPath(tc.path)
			}
		})
	}
}

// BenchmarkDetectChannelTypeFromPath_Parallel 并发性能测试
func BenchmarkDetectChannelTypeFromPath_Parallel(b *testing.B) {
	path := "/v1/messages"
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_ = DetectChannelTypeFromPath(path)
		}
	})
}

// BenchmarkNormalizeChannelType 测试渠道类型规范化性能
func BenchmarkNormalizeChannelType(b *testing.B) {
	testCases := []struct {
		name  string
		value string
	}{
		{"Lowercase", "anthropic"},
		{"Uppercase", "ANTHROPIC"},
		{"MixedCase", "AnThRoPiC"},
		{"WithSpaces", "  anthropic  "},
		{"Empty", ""},
	}

	for _, tc := range testCases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = NormalizeChannelType(tc.value)
			}
		})
	}
}

// BenchmarkMatchPath 测试路径匹配性能
func BenchmarkMatchPath(b *testing.B) {
	testCases := []struct {
		name      string
		path      string
		patterns  []string
		matchType string
	}{
		{"Prefix_Match", "/v1/messages", []string{"/v1/messages"}, MatchTypePrefix},
		{"Prefix_NoMatch", "/v2/messages", []string{"/v1/messages"}, MatchTypePrefix},
		{"Contains_Match", "/v1beta/models/gemini", []string{"/v1beta/"}, MatchTypeContains},
		{"Contains_NoMatch", "/v1/models/gemini", []string{"/v1beta/"}, MatchTypeContains},
		{"MultiPattern", "/v1/embeddings", []string{"/v1/chat", "/v1/completions", "/v1/embeddings"}, MatchTypePrefix},
	}

	for _, tc := range testCases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = matchPath(tc.path, tc.patterns, tc.matchType)
			}
		})
	}
}
