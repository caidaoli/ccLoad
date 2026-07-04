package util

import "testing"

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
