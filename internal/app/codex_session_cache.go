package app

import (
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // UUIDv5 per RFC 4122 requires SHA-1，与安全无关
	"fmt"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/protocol"

	"github.com/bytedance/sonic"
)

// Codex Responses API 的 prompt 缓存需要 `prompt_cache_key` 请求体字段与 `Session_id` 请求头配合，
// 仅当稳定分桶时 OpenAI 才能稳定命中缓存。ccLoad 需在 Anthropic/OpenAI 客户端转换到 Codex 上游时补齐，
// 策略参考 CLIProxyAPI internal/runtime/executor/codex_executor.go:cacheHelper。

type codexSessionEntry struct {
	id     string
	expire time.Time
}

const (
	codexSessionTTL             = time.Hour
	codexSessionCleanupInterval = 15 * time.Minute
)

var (
	codexSessionMap  = make(map[string]codexSessionEntry)
	codexSessionMu   sync.RWMutex
	codexSessionOnce sync.Once
)

// getOrCreateCodexSessionID 返回同一 cacheKey 下的稳定 UUID，命中即续期 TTL。
func getOrCreateCodexSessionID(cacheKey string) string {
	if cacheKey == "" {
		return ""
	}
	codexSessionOnce.Do(startCodexSessionCleanup)
	now := time.Now()

	codexSessionMu.Lock()
	defer codexSessionMu.Unlock()
	if entry, ok := codexSessionMap[cacheKey]; ok && entry.id != "" && entry.expire.After(now) {
		entry.expire = now.Add(codexSessionTTL)
		codexSessionMap[cacheKey] = entry
		return entry.id
	}
	id := newCodexUUIDv4()
	codexSessionMap[cacheKey] = codexSessionEntry{id: id, expire: now.Add(codexSessionTTL)}
	return id
}

// codexSessionIDForOpenAIKey 基于 API Key 生成确定性 UUID（v5 + OID namespace）。
// 不同 Key 之间得到不同桶；同一 Key 的连续请求稳定命中同一桶。
func codexSessionIDForOpenAIKey(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	return newCodexUUIDv5(uuidNameSpaceOID, "ccload:codex:prompt-cache:"+apiKey)
}

// resolveCodexSessionHint 仅在 Codex 上游场景下返回稳定的会话 ID；否则返回空。
//   - Anthropic 客户端：读原始 body 的 metadata.user_id，cache key = model-userID
//   - Codex 客户端：读 body 内已有的 prompt_cache_key（不主动创建）
//   - OpenAI 客户端：基于 apiKey 生成确定性 UUID
//   - 其他协议：返回空
func resolveCodexSessionHint(reqCtx *requestContext, translatedBody []byte, apiKey string) string {
	if reqCtx == nil || reqCtx.upstreamProtocol != protocol.Codex {
		return ""
	}
	switch reqCtx.clientProtocol {
	case protocol.Anthropic:
		userID := extractAnthropicUserID(reqCtx.originalBody)
		if userID == "" {
			return ""
		}
		model := strings.TrimSpace(reqCtx.originalModel)
		if model == "" {
			model = "unknown"
		}
		return getOrCreateCodexSessionID(model + "-" + userID)
	case protocol.Codex:
		return readCodexPromptCacheKey(translatedBody)
	case protocol.OpenAI:
		return codexSessionIDForOpenAIKey(apiKey)
	}
	return ""
}

// injectCodexPromptCacheKey 在 body 顶层写入 prompt_cache_key；已有非空值则保留。
// 非 JSON 对象或解析失败时原样返回。
func injectCodexPromptCacheKey(body []byte, id string) []byte {
	if len(body) == 0 || id == "" {
		return body
	}
	var payload map[string]any
	if err := sonic.Unmarshal(body, &payload); err != nil || payload == nil {
		return body
	}
	if existing, ok := payload["prompt_cache_key"].(string); ok && strings.TrimSpace(existing) != "" {
		return body
	}
	payload["prompt_cache_key"] = id
	out, err := sonic.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

func extractAnthropicUserID(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var payload struct {
		Metadata struct {
			UserID string `json:"user_id"`
		} `json:"metadata"`
	}
	if err := sonic.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Metadata.UserID)
}

func readCodexPromptCacheKey(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var payload struct {
		PromptCacheKey string `json:"prompt_cache_key"`
	}
	if err := sonic.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.PromptCacheKey)
}

func startCodexSessionCleanup() {
	go func() {
		t := time.NewTicker(codexSessionCleanupInterval)
		defer t.Stop()
		for range t.C {
			now := time.Now()
			codexSessionMu.Lock()
			for k, v := range codexSessionMap {
				if !v.expire.After(now) {
					delete(codexSessionMap, k)
				}
			}
			codexSessionMu.Unlock()
		}
	}()
}

// ---- 手写 UUID v4/v5（与 internal/protocol/builtin/request_prompt.go:newClaudeMetadataUserID 风格一致，零外部依赖）

// uuidNameSpaceOID 为 RFC 4122 定义的 OID namespace UUID。
var uuidNameSpaceOID = [16]byte{
	0x6b, 0xa7, 0xb8, 0x12, 0x9d, 0xad, 0x11, 0xd1,
	0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8,
}

func newCodexUUIDv4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUIDBytes(b)
}

func newCodexUUIDv5(namespace [16]byte, name string) string {
	h := sha1.New() //nolint:gosec
	h.Write(namespace[:])
	h.Write([]byte(name))
	sum := h.Sum(nil)
	var b [16]byte
	copy(b[:], sum[:16])
	b[6] = (b[6] & 0x0f) | 0x50
	b[8] = (b[8] & 0x3f) | 0x80
	return formatUUIDBytes(b)
}

func formatUUIDBytes(b [16]byte) string {
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
