package app

import (
	"net/http"
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

const unsupportedProtocolCapabilityTTL = 10 * time.Minute

// shouldCacheProtocolUnsupported 只缓存稳定的端点级不支持。
// 400/403/500 与本地转换失败都可能由具体请求形态触发；把它们扩大成整个请求族
// 不支持，会让一次 tool_result 等特殊请求错误地屏蔽后续普通请求。
func shouldCacheProtocolUnsupported(statusCode int) bool {
	return statusCode == http.StatusNotFound || statusCode == http.StatusMethodNotAllowed
}

// protocolUnsupported 是能力缓存的哨兵值：已探测且确认该 URL 不支持当前请求族。
// 与「无缓存条目」（尚未探测，get 返回 known=false）区分开。
const protocolUnsupported protocol.Protocol = ""

var automaticFallbackProtocolOrder = [...]protocol.Protocol{
	protocol.OpenAI,
	protocol.Anthropic,
	protocol.Codex,
	protocol.Gemini,
}

var localFallbackProtocolOrder = [...]protocol.Protocol{
	protocol.Anthropic,
	protocol.Codex,
	protocol.OpenAI,
	protocol.Gemini,
}

func supportsProtocolCandidate(client, upstream protocol.Protocol, family protocol.RequestFamily) bool {
	return client == upstream || protocol.SupportsTransformFamily(client, upstream, family)
}

func configCanUseUpstreamProtocol(cfg *model.Config, upstream protocol.Protocol) bool {
	if cfg == nil || !protocol.IsValid(upstream) {
		return false
	}
	for _, entry := range cfg.URLs {
		if entry.SupportsProtocol(string(upstream)) {
			return true
		}
	}
	return false
}

func localUpstreamProtocolOrder(urls model.ChannelURLs) []protocol.Protocol {
	seen := make(map[protocol.Protocol]struct{}, len(localFallbackProtocolOrder))
	ordered := make([]protocol.Protocol, 0, len(localFallbackProtocolOrder))
	for _, entry := range urls {
		for _, configured := range entry.Protocols {
			candidate := protocol.Protocol(configured)
			if !protocol.IsValid(candidate) {
				continue
			}
			if _, exists := seen[candidate]; exists {
				continue
			}
			seen[candidate] = struct{}{}
			ordered = append(ordered, candidate)
		}
	}
	if len(ordered) > 0 {
		return ordered
	}
	return append([]protocol.Protocol(nil), localFallbackProtocolOrder[:]...)
}

func prioritizeProtocolCandidate(candidates []protocol.Protocol, preferred protocol.Protocol) []protocol.Protocol {
	for idx, candidate := range candidates {
		if candidate != preferred || idx == 0 {
			continue
		}
		ordered := make([]protocol.Protocol, 0, len(candidates))
		ordered = append(ordered, candidate)
		ordered = append(ordered, candidates[:idx]...)
		ordered = append(ordered, candidates[idx+1:]...)
		return ordered
	}
	return candidates
}

func protocolCandidatesForURL(
	entry model.ChannelURL,
	transformMode string,
	clientProtocol protocol.Protocol,
	requestFamily protocol.RequestFamily,
	localProtocolOrder []protocol.Protocol,
) (candidates []protocol.Protocol, declared bool) {
	declared = !entry.UsesAutomaticProtocolDetection()
	appendIfSupported := func(upstream protocol.Protocol) {
		if entry.SupportsProtocol(string(upstream)) &&
			supportsProtocolCandidate(clientProtocol, upstream, requestFamily) {
			candidates = append(candidates, upstream)
		}
	}

	switch transformMode {
	case model.ProtocolTransformModeUpstream:
		if protocol.IsValid(clientProtocol) {
			appendIfSupported(clientProtocol)
		}
	case model.ProtocolTransformModeLocal:
		if declared {
			for _, configured := range entry.Protocols {
				appendIfSupported(protocol.Protocol(configured))
			}
		} else {
			for _, upstream := range localProtocolOrder {
				appendIfSupported(upstream)
			}
		}
	default:
		if protocol.IsValid(clientProtocol) {
			appendIfSupported(clientProtocol)
		}
		for _, upstream := range automaticFallbackProtocolOrder {
			if upstream == clientProtocol {
				continue
			}
			appendIfSupported(upstream)
		}
		if declared && len(candidates) > 1 {
			candidates = candidates[:1]
		}
	}
	return candidates, declared
}

func channelTestRequestFamily(client protocol.Protocol) protocol.RequestFamily {
	switch client {
	case protocol.Anthropic:
		return protocol.RequestFamilyMessages
	case protocol.OpenAI:
		return protocol.RequestFamilyChatCompletions
	case protocol.Codex:
		return protocol.RequestFamilyResponses
	case protocol.Gemini:
		return protocol.RequestFamilyGenerateContent
	default:
		return protocol.RequestFamilyUnknown
	}
}

type protocolCapabilityKey struct {
	channelID      int64
	baseURL        string
	clientProtocol protocol.Protocol
	requestFamily  protocol.RequestFamily
}

type protocolCapabilityEntry struct {
	upstream   protocol.Protocol
	retryAfter time.Time
}

type protocolCapabilityCache struct {
	mu      sync.Mutex
	entries map[protocolCapabilityKey]protocolCapabilityEntry
}

type protocolProbeRetrySummary struct {
	count   int
	retryAt time.Time
}

// get 返回已学习的上游协议。成功条目不过期；known=false 表示未探测，或“不支持”
// 条目已到重试时间；known=true 且 upstream==protocolUnsupported 表示暂时确认不支持。
func (c *protocolCapabilityCache) get(key protocolCapabilityKey) (upstream protocol.Protocol, known bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return protocolUnsupported, false
	}
	if entry.upstream == protocolUnsupported && !time.Now().Before(entry.retryAfter) {
		delete(c.entries, key)
		return protocolUnsupported, false
	}
	return entry.upstream, true
}

func (c *protocolCapabilityCache) set(key protocolCapabilityKey, upstream protocol.Protocol) {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[protocolCapabilityKey]protocolCapabilityEntry)
	}
	// 成功能力属于稳定的 URL 配置，保留到进程重启或渠道配置变更；只有“不支持”
	// 哨兵需要定期重试。顺手清掉过期哨兵，避免未再次查询的 key 长期残留。
	for k, entry := range c.entries {
		if entry.upstream == protocolUnsupported && !entry.retryAfter.After(now) {
			delete(c.entries, k)
		}
	}
	entry := protocolCapabilityEntry{upstream: upstream}
	if upstream == protocolUnsupported {
		entry.retryAfter = now.Add(unsupportedProtocolCapabilityTTL)
	}
	c.entries[key] = entry
}

// unsupportedRetrySummaries 返回各渠道仍在等待重探的协议能力哨兵。
// 成功能力不属于临时状态；过期哨兵在生成快照时一并清理。
func (c *protocolCapabilityCache) unsupportedRetrySummaries(now time.Time) map[int64]protocolProbeRetrySummary {
	c.mu.Lock()
	defer c.mu.Unlock()

	summaries := make(map[int64]protocolProbeRetrySummary)
	for key, entry := range c.entries {
		if entry.upstream != protocolUnsupported {
			continue
		}
		if !entry.retryAfter.After(now) {
			delete(c.entries, key)
			continue
		}
		summary := summaries[key.channelID]
		summary.count++
		if summary.retryAt.IsZero() || entry.retryAfter.Before(summary.retryAt) {
			summary.retryAt = entry.retryAfter
		}
		summaries[key.channelID] = summary
	}
	return summaries
}

func (c *protocolCapabilityCache) clear() {
	c.mu.Lock()
	clear(c.entries)
	c.mu.Unlock()
}
