package app

import (
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
)

const protocolCapabilityTTL = 10 * time.Minute

type protocolCapabilityState uint8

const (
	protocolCapabilityUnknown protocolCapabilityState = iota
	protocolCapabilityUnsupported
	protocolCapabilityAnthropic
	protocolCapabilityOpenAI
	protocolCapabilityCodex
	protocolCapabilityGemini
)

var automaticFallbackProtocolOrder = [...]protocol.Protocol{
	protocol.Anthropic,
	protocol.OpenAI,
	protocol.Codex,
	protocol.Gemini,
}

func protocolCapabilityFor(upstream protocol.Protocol) protocolCapabilityState {
	switch upstream {
	case protocol.Anthropic:
		return protocolCapabilityAnthropic
	case protocol.OpenAI:
		return protocolCapabilityOpenAI
	case protocol.Codex:
		return protocolCapabilityCodex
	case protocol.Gemini:
		return protocolCapabilityGemini
	default:
		return protocolCapabilityUnsupported
	}
}

func (state protocolCapabilityState) upstreamProtocol() (protocol.Protocol, bool) {
	switch state {
	case protocolCapabilityAnthropic:
		return protocol.Anthropic, true
	case protocolCapabilityOpenAI:
		return protocol.OpenAI, true
	case protocolCapabilityCodex:
		return protocol.Codex, true
	case protocolCapabilityGemini:
		return protocol.Gemini, true
	default:
		return "", false
	}
}

func supportsProtocolCandidate(client, upstream protocol.Protocol, family protocol.RequestFamily) bool {
	return client == upstream || protocol.SupportsTransformFamily(client, upstream, family)
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
	clientProtocol, channelProtocol protocol.Protocol,
	requestFamily protocol.RequestFamily,
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
		if protocolCapabilityFor(clientProtocol) != protocolCapabilityUnsupported {
			appendIfSupported(clientProtocol)
		}
	case model.ProtocolTransformModeLocal:
		appendIfSupported(channelProtocol)
	default:
		if entry.Exact && !declared {
			appendIfSupported(channelProtocol)
			return candidates, false
		}
		if protocolCapabilityFor(clientProtocol) != protocolCapabilityUnsupported {
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
	state     protocolCapabilityState
	expiresAt time.Time
}

type protocolCapabilityCache struct {
	mu      sync.Mutex
	entries map[protocolCapabilityKey]protocolCapabilityEntry
}

func (c *protocolCapabilityCache) get(key protocolCapabilityKey) protocolCapabilityState {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[key]
	if !ok {
		return protocolCapabilityUnknown
	}
	if !time.Now().Before(entry.expiresAt) {
		delete(c.entries, key)
		return protocolCapabilityUnknown
	}
	return entry.state
}

func (c *protocolCapabilityCache) set(key protocolCapabilityKey, state protocolCapabilityState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[protocolCapabilityKey]protocolCapabilityEntry)
	}
	c.entries[key] = protocolCapabilityEntry{state: state, expiresAt: time.Now().Add(protocolCapabilityTTL)}
}

func (c *protocolCapabilityCache) clear() {
	c.mu.Lock()
	clear(c.entries)
	c.mu.Unlock()
}
