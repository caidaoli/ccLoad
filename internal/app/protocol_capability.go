package app

import (
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

const protocolCapabilityTTL = 10 * time.Minute

// protocolUnsupported 是能力缓存的哨兵值：已探测且确认该 URL 不支持当前请求族。
// 与「无缓存条目」（尚未探测，get 返回 known=false）区分开。
const protocolUnsupported protocol.Protocol = ""

var automaticFallbackProtocolOrder = [...]protocol.Protocol{
	protocol.Anthropic,
	protocol.OpenAI,
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
	upstream  protocol.Protocol
	expiresAt time.Time
}

type protocolCapabilityCache struct {
	mu      sync.Mutex
	entries map[protocolCapabilityKey]protocolCapabilityEntry
}

// get 返回已学习的上游协议。known=false 表示未探测（无条目或已过期）；
// known=true 且 upstream==protocolUnsupported 表示已确认不支持。
func (c *protocolCapabilityCache) get(key protocolCapabilityKey) (upstream protocol.Protocol, known bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return protocolUnsupported, false
	}
	if !time.Now().Before(entry.expiresAt) {
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
	// 条目只在此处新增，顺手清掉过期项即可保证 map 不随渠道/URL 变更无界增长
	// （get 只惰性删除被再次查询的 key）。规模是渠道×URL×协议组合，全扫成本可忽略。
	for k, entry := range c.entries {
		if !entry.expiresAt.After(now) {
			delete(c.entries, k)
		}
	}
	c.entries[key] = protocolCapabilityEntry{upstream: upstream, expiresAt: now.Add(protocolCapabilityTTL)}
}

func (c *protocolCapabilityCache) clear() {
	c.mu.Lock()
	clear(c.entries)
	c.mu.Unlock()
}
