package util

import "testing"

// BenchmarkNormalizeProtocol 测试协议规范化性能
func BenchmarkNormalizeProtocol(b *testing.B) {
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
				_ = NormalizeProtocol(tc.value)
			}
		})
	}
}
