package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	antigravityReplayMaxEntries = 1024
	antigravityReplayMaxBytes   = 8 << 20
	antigravityReplayItemLimit  = 64 << 10
)

// The server owns this bounded cache. Translators remain pure, and cache misses
// never authorize rewriting or removing the client's conversation history.
type antigravityReplayCache struct {
	mu       sync.Mutex
	entries  map[string]antigravityReplayEntry
	bytes    int
	sequence uint64
	scopes   map[string]*antigravityReplayScope
	now      func() time.Time
}

type antigravityReplayScope struct {
	generation uint64
	active     int
}

type antigravityReplayEntry struct {
	scope    string
	value    string
	expires  time.Time
	sequence uint64
}

type antigravityReplay struct {
	cache      *antigravityReplayCache
	scope      string
	protocol   protocol.Protocol
	scopeState *antigravityReplayScope
	generation uint64
	pending    map[string]string
	blocks     map[int]string
	bytes      int
	done       bool
	disabled   bool
}

func (c *antigravityReplayCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func antigravityReplayHash(parts ...string) string {
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (c *antigravityReplayCache) begin(cfg *model.Config, modelName, baseURL string, headers http.Header, body []byte, client protocol.Protocol) *antigravityReplay {
	if client != protocol.Anthropic && client != protocol.Codex {
		return nil
	}
	session := responsesExecutionSessionID(headers)
	if session == "" {
		session = strings.TrimSpace(headers.Get("X-Claude-Code-Session-Id"))
	}
	if session == "" {
		session = anthropicSessionIDFromBody(body)
	}
	if session == "" {
		for _, path := range []string{"session_id", "metadata.session_id"} {
			if session = gjson.GetBytes(body, path).String(); session != "" {
				break
			}
		}
	}
	// Without a stable caller and conversation identity, a miss is safer than
	// recovering a signature from an unrelated request with identical text.
	caller := headers.Get("Authorization") + "\x00" + headers.Get("X-Api-Key")
	if session == "" || caller == "\x00" {
		return nil
	}
	scope := antigravityReplayHash(fmt.Sprint(cfg.ID), antigravityCredentialPoolScope(cfg), modelName, baseURL, caller, session, string(client))
	c.mu.Lock()
	if c.scopes == nil {
		c.scopes = make(map[string]*antigravityReplayScope)
	}
	state := c.scopes[scope]
	if state == nil {
		state = &antigravityReplayScope{}
		c.scopes[scope] = state
	}
	state.active++
	generation := state.generation
	c.mu.Unlock()
	return &antigravityReplay{cache: c, scope: scope, scopeState: state, protocol: client, generation: generation, pending: make(map[string]string), blocks: make(map[int]string)}
}

func (r *antigravityReplay) close() {
	if r == nil {
		return
	}
	r.cache.mu.Lock()
	defer r.cache.mu.Unlock()
	r.scopeState.active--
	if r.scopeState.active == 0 {
		delete(r.cache.scopes, r.scope)
	}
}

func (r *antigravityReplay) key(kind, text string) string {
	return antigravityReplayHash(r.scope, kind, text)
}

func (r *antigravityReplay) lookup(kind, text string) string {
	c := r.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	key := r.key(kind, text)
	entry, ok := c.entries[key]
	if !ok {
		return ""
	}
	if !c.clock().Before(entry.expires) {
		c.bytes -= len(entry.value)
		delete(c.entries, key)
		return ""
	}
	return entry.value
}

func antigravityReasoningText(item gjson.Result) string {
	var parts []string
	for _, part := range item.Get("summary").Array() {
		if text := part.Get("text").String(); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func antigravityReplayMessageKey(item gjson.Result) string {
	if item.Get("type").String() != "message" || item.Get("role").String() != "assistant" || item.Get("id").String() == "" {
		return ""
	}
	var text strings.Builder
	for _, part := range item.Get("content").Array() {
		text.WriteString(part.Get("text").String())
	}
	if text.Len() == 0 {
		return ""
	}
	return antigravityReplayHash(item.Get("id").String(), text.String())
}

func (r *antigravityReplay) restore(body []byte) []byte {
	if r == nil {
		return body
	}
	if r.protocol == protocol.Anthropic {
		for i, message := range gjson.GetBytes(body, "messages").Array() {
			if message.Get("role").String() != "assistant" {
				continue
			}
			for j, block := range message.Get("content").Array() {
				if block.Get("type").String() != "thinking" || block.Get("signature").String() != "" {
					continue
				}
				if value := r.lookup("thinking", block.Get("thinking").String()); value != "" {
					body = setJSONValue(body, fmt.Sprintf("messages.%d.content.%d.signature", i, j), value)
				}
			}
		}
		return body
	}
	items := gjson.GetBytes(body, "input")
	if !items.IsArray() {
		return body
	}
	input := items.Array()
	output := make([]json.RawMessage, 0, len(input))
	changed := false
	for i, item := range input {
		if item.Get("type").String() == "reasoning" && item.Get("encrypted_content").String() == "" {
			if value := r.lookup("reasoning", antigravityReasoningText(item)); value != "" {
				raw := setJSONValue([]byte(item.Raw), "encrypted_content", value)
				item = gjson.ParseBytes(raw)
				changed = true
			}
		}
		output = append(output, json.RawMessage(item.Raw))
		key := antigravityReplayMessageKey(item)
		if key == "" {
			continue
		}
		// Preserve explicit carriers, including ones we cannot validate. Never
		// silently replace a client's signature or insert a competing carrier.
		if (i > 0 && input[i-1].Get("type").String() == "reasoning") || (i+1 < len(input) && input[i+1].Get("type").String() == "reasoning") {
			continue
		}
		if value := r.lookup("carriers", key); value != "" {
			for _, carrier := range gjson.Parse(value).Array() {
				output = append(output, json.RawMessage(carrier.Raw))
			}
			changed = true
		}
	}
	if changed {
		raw, _ := json.Marshal(output)
		body = setJSONRaw(body, "input", string(raw))
	}
	return body
}

func (r *antigravityReplay) remember(kind, text, value string) {
	if text == "" || value == "" || len(value) > antigravityReplayItemLimit || r.disabled {
		return
	}
	key := r.key(kind, text)
	r.bytes += len(value) - len(r.pending[key])
	if r.bytes > 1<<20 {
		r.disabled = true
		r.pending = nil
		return
	}
	r.pending[key] = value
}

func (r *antigravityReplay) captureJSON(body []byte) {
	if r == nil || r.disabled {
		return
	}
	if r.protocol == protocol.Anthropic {
		for _, block := range gjson.GetBytes(body, "content").Array() {
			if block.Get("type").String() == "thinking" {
				r.remember("thinking", block.Get("thinking").String(), block.Get("signature").String())
			}
		}
	} else {
		items := gjson.GetBytes(body, "output").Array()
		for i, item := range items {
			if item.Get("type").String() == "reasoning" {
				r.remember("reasoning", antigravityReasoningText(item), item.Get("encrypted_content").String())
			}
			key := antigravityReplayMessageKey(item)
			if key == "" {
				continue
			}
			carriers := []json.RawMessage{}
			first := i
			for first > 0 {
				preceding := items[first-1]
				if preceding.Get("type").String() != "reasoning" || len(preceding.Get("summary").Array()) != 0 || !strings.HasPrefix(preceding.Get("encrypted_content").String(), "cpa-gemini-responses-carrier-v1:next:text:") {
					break
				}
				first--
			}
			for j := first; j < i; j++ {
				carrier := items[j]
				signature := strings.Replace(carrier.Get("encrypted_content").String(), "cpa-gemini-responses-carrier-v1:next:text:", "cpa-gemini-responses-carrier-v1:previous:text:", 1)
				carriers = append(carriers, setJSONValue([]byte(carrier.Raw), "encrypted_content", signature))
			}
			for j := i + 1; j < len(items); j++ {
				carrier := items[j]
				if carrier.Get("type").String() != "reasoning" || len(carrier.Get("summary").Array()) != 0 || !strings.HasPrefix(carrier.Get("encrypted_content").String(), "cpa-gemini-responses-carrier-v1:previous:text:") {
					break
				}
				carriers = append(carriers, json.RawMessage(carrier.Raw))
			}
			if len(carriers) > 0 {
				raw, _ := json.Marshal(carriers)
				r.remember("carriers", key, string(raw))
			}
		}
	}
	r.done = true
}

func (r *antigravityReplay) captureStream(chunks [][]byte) {
	if r == nil || r.disabled {
		return
	}
	var events [][]byte
	for _, chunk := range chunks {
		events = append(events, bytes.Split(chunk, []byte("\n\n"))...)
	}
	for _, chunk := range events {
		data := sseEventData(bytes.TrimSpace(chunk))
		event := gjson.ParseBytes(data)
		if r.protocol == protocol.Codex {
			if event.Get("type").String() == "response.completed" {
				r.captureJSON([]byte(event.Get("response").Raw))
			}
			continue
		}
		index := int(event.Get("index").Int())
		switch event.Get("type").String() {
		case "content_block_start":
			block := event.Get("content_block")
			if block.Get("type").String() == "thinking" {
				if len(block.Raw) > antigravityReplayItemLimit || len(r.blocks) >= 64 {
					r.disabled = true
					r.blocks = nil
					r.pending = nil
					return
				}
				r.blocks[index] = block.Raw
			}
		case "content_block_delta":
			block, ok := r.blocks[index]
			if !ok {
				continue
			}
			field := ""
			switch event.Get("delta.type").String() {
			case "thinking_delta":
				field = "thinking"
			case "signature_delta":
				field = "signature"
			}
			if field != "" {
				value := gjson.Get(block, field).String() + event.Get("delta."+field).String()
				if len(value) > antigravityReplayItemLimit {
					r.disabled = true
					r.blocks = nil
					r.pending = nil
					return
				}
				block, _ = sjson.Set(block, field, value)
				r.blocks[index] = block
			}
		case "content_block_stop":
			if block, ok := r.blocks[index]; ok {
				r.remember("thinking", gjson.Get(block, "thinking").String(), gjson.Get(block, "signature").String())
				delete(r.blocks, index)
			}
		case "message_stop":
			r.done = true
		}
	}
}

func (r *antigravityReplay) finish(result *fwResult, err error) {
	if r == nil || result == nil {
		return
	}
	c := r.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	if result.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(gjson.GetBytes(result.Body, "error.message").String()), "signature") {
		r.scopeState.generation++
		for key, entry := range c.entries {
			if entry.scope == r.scope {
				c.bytes -= len(entry.value)
				delete(c.entries, key)
			}
		}
		return
	}
	if err != nil || result.Status < 200 || result.Status >= 300 || result.SSEErrorEvent != nil || !r.done || r.disabled || r.scopeState.generation != r.generation {
		return
	}
	if c.entries == nil {
		c.entries = make(map[string]antigravityReplayEntry)
	}
	now := c.clock()
	for key, entry := range c.entries {
		if !now.Before(entry.expires) {
			c.bytes -= len(entry.value)
			delete(c.entries, key)
		}
	}
	ttl := time.Hour
	if r.protocol == protocol.Anthropic {
		ttl = 3 * time.Hour
	}
	for key, value := range r.pending {
		c.bytes -= len(c.entries[key].value)
		c.sequence++
		c.entries[key] = antigravityReplayEntry{scope: r.scope, value: value, expires: now.Add(ttl), sequence: c.sequence}
		c.bytes += len(value)
	}
	for len(c.entries) > antigravityReplayMaxEntries || c.bytes > antigravityReplayMaxBytes {
		oldestKey := ""
		oldest := ^uint64(0)
		for key, entry := range c.entries {
			if entry.sequence < oldest {
				oldestKey, oldest = key, entry.sequence
			}
		}
		c.bytes -= len(c.entries[oldestKey].value)
		delete(c.entries, oldestKey)
	}
}
