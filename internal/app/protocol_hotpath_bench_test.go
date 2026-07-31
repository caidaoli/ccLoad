package app

import (
	"fmt"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

// 协议协商热路径基准：这些函数在每次请求的渠道选择/URL 尝试中执行，
// 量化其成本以守住「选择开销远小于转发耗时」的前提。

func benchProtocolChannelConfig() *model.Config {
	return &model.Config{
		ID:   1,
		Name: "bench",
		URLs: model.ChannelURLs{
			{URL: "https://a.example.com"},
			{URL: "https://b.example.com", Protocols: []string{"anthropic", "openai"}},
			{URL: "https://c.example.com", Protocols: []string{"gemini"}},
			{URL: "https://d.example.com"},
		},
		ModelEntries: []model.ModelEntry{
			{Model: "claude-sonnet-5"},
			{Model: "claude-opus-5"},
			{Model: "gpt-5.4"},
			{Model: "gpt-5.6-terra"},
			{Model: "gemini-3-pro"},
			{Model: "gemini-3-flash"},
			{Model: "qwen-plus"},
			{Model: "deepseek-v4"},
		},
	}
}

func BenchmarkPossibleUpstreamProtocols(b *testing.B) {
	cfg := benchProtocolChannelConfig()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = possibleUpstreamProtocols(cfg, protocol.Anthropic)
	}
}

// BenchmarkPossibleActualModels 覆盖 selector 冷却过滤对单个候选渠道的完整解析链
// （possibleUpstreamProtocols + 每协议 resolveFinalUpstreamModel）。
func BenchmarkPossibleActualModels(b *testing.B) {
	cfg := benchProtocolChannelConfig()
	s := &Server{}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = s.possibleActualModels(cfg, "claude-sonnet-5", "anthropic")
	}
}

func BenchmarkProtocolCandidatesForURL(b *testing.B) {
	cfg := benchProtocolChannelConfig()
	localOrder := localUpstreamProtocolOrder(cfg.URLs)
	b.Run("AutoUndeclared", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = protocolCandidatesForURL(cfg.URLs[0], model.ProtocolTransformModeAuto,
				protocol.Anthropic, protocol.RequestFamilyMessages, localOrder)
		}
	})
	b.Run("AutoDeclared", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_, _ = protocolCandidatesForURL(cfg.URLs[1], model.ProtocolTransformModeAuto,
				protocol.Anthropic, protocol.RequestFamilyMessages, localOrder)
		}
	})
}

func BenchmarkLocalUpstreamProtocolOrder(b *testing.B) {
	cfg := benchProtocolChannelConfig()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = localUpstreamProtocolOrder(cfg.URLs)
	}
}

// BenchmarkProtocolCapabilityCacheSet 量化 set 时机会式全扫清理的成本
// （条目数即渠道×URL×协议组合的现实上限量级）。
func BenchmarkProtocolCapabilityCacheSet(b *testing.B) {
	for _, size := range []int{50, 500} {
		b.Run(fmt.Sprintf("entries=%d", size), func(b *testing.B) {
			cache := &protocolCapabilityCache{}
			for i := 0; i < size; i++ {
				cache.set(protocolCapabilityKey{
					channelID: int64(i), baseURL: fmt.Sprintf("https://u%d.example.com", i),
					clientProtocol: protocol.Anthropic, requestFamily: protocol.RequestFamilyMessages,
				}, protocol.OpenAI)
			}
			key := protocolCapabilityKey{
				channelID: 1, baseURL: "https://u1.example.com",
				clientProtocol: protocol.Anthropic, requestFamily: protocol.RequestFamilyMessages,
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				cache.set(key, protocol.OpenAI)
			}
		})
	}
}

func BenchmarkProtocolCapabilityCacheGet(b *testing.B) {
	cache := &protocolCapabilityCache{}
	key := protocolCapabilityKey{
		channelID: 1, baseURL: "https://u1.example.com",
		clientProtocol: protocol.Anthropic, requestFamily: protocol.RequestFamilyMessages,
	}
	cache.set(key, protocol.OpenAI)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = cache.get(key)
	}
}
