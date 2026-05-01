package app

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/protocol"
	"ccLoad/internal/util"

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
	id := util.NewUUIDv4()
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
	return util.NewUUIDv5(util.NameSpaceOID, "ccload:codex:prompt-cache:"+apiKey)
}

// resolveCodexSessionHint 仅在 Codex 上游场景下返回稳定的会话 ID；否则返回空。
//   - Anthropic 客户端：优先 metadata.user_id（model-userID 内存缓存）→ X-Claude-Code-Session-Id 头 → apiKey 确定性 UUID
//   - Codex 客户端：读 body 内已有的 prompt_cache_key（不主动创建）
//   - OpenAI 客户端：基于 apiKey 生成确定性 UUID
//   - 其他协议：返回空
func resolveCodexSessionHint(reqCtx *requestContext, translatedBody []byte, apiKey string, header http.Header) string {
	if reqCtx == nil || runtimeUpstreamProtocol(reqCtx, nil) != string(protocol.Codex) {
		return ""
	}
	switch reqCtx.clientProtocol {
	case protocol.Anthropic:
		if userID := extractAnthropicUserID(reqCtx.originalBody); userID != "" {
			model := strings.TrimSpace(reqCtx.originalModel)
			if model == "" {
				model = "unknown"
			}
			return getOrCreateCodexSessionID(model + "-" + userID)
		}
		if sid := strings.TrimSpace(header.Get("X-Claude-Code-Session-Id")); sid != "" {
			return util.NewUUIDv5(util.NameSpaceOID, "ccload:codex:prompt-cache:session:"+sid)
		}
		return codexSessionIDForOpenAIKey(apiKey)
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
	if readCodexPromptCacheKey(body) != "" {
		return body
	}
	var payload map[string]any
	if err := sonic.Unmarshal(body, &payload); err != nil || payload == nil {
		return body
	}

	encodedID, err := sonic.Marshal(id)
	if err != nil {
		return body
	}
	end := len(body)
	for end > 0 {
		switch body[end-1] {
		case ' ', '\n', '\r', '\t':
			end--
		default:
			goto foundEnd
		}
	}
foundEnd:
	if end == 0 || body[end-1] != '}' {
		return body
	}
	start := 0
	for start < end {
		switch body[start] {
		case ' ', '\n', '\r', '\t':
			start++
		default:
			goto foundStart
		}
	}
foundStart:
	if start >= end || body[start] != '{' {
		return body
	}

	hasFields := len(bytes.TrimSpace(body[start+1:end-1])) > 0
	insertLen := len(`"prompt_cache_key":`) + len(encodedID)
	if hasFields {
		insertLen++
	}
	out := make([]byte, 0, len(body)+insertLen)
	out = append(out, body[:end-1]...)
	if hasFields {
		out = append(out, ',')
	}
	out = append(out, `"prompt_cache_key":`...)
	out = append(out, encodedID...)
	out = append(out, body[end-1:]...)
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

// UUID v4/v5 已统一到 internal/util/uuid_local.go（util.NewUUIDv4 / util.NewUUIDv5 / util.NameSpaceOID）。
