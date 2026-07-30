package app

import (
	"sync"
	"time"

	"ccLoad/internal/protocol"
)

const protocolCapabilityTTL = 10 * time.Minute

type protocolCapabilityState uint8

const (
	protocolCapabilityUnknown protocolCapabilityState = iota
	protocolCapabilityNative
	protocolCapabilityLocal
	protocolCapabilityUnsupported
)

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
