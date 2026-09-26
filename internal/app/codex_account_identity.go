package app

import (
	"crypto/sha256"
	"net/http"
	"strings"

	"ccLoad/internal/model"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Codex OAuth 账号作用域身份，对齐 sub2api openai_codex_account_identity.go（a3eb7ef3）。
// 客户端的 installation/session/thread/turn/window/request ID 原样经多个上游账号发出时，
// 上游可凭同一 ID 把这些账号关联起来。这里按 ChatGPT 账号把每个 ID 确定性映射为 UUID：
// 同账号同值稳定（prompt 缓存与会话续接不受影响），跨账号不可关联。
// 与 sub2api 不同，映射不区分字段种类：客户端原本相等的字段（prompt_cache_key、
// session_id、Session-Id 等）映射后仍相等。

const codexAccountIdentityVersion = "v1"

const codexTurnMetadataField = "x-codex-turn-metadata"

// codexAccountIdentityFields 同时用作 client_metadata / turn metadata 的键和请求头名，
// 覆盖官方 Codex（rust-v0.157.1）发出的全部会话身份字段。
var codexAccountIdentityFields = []string{
	"installation_id", "x-codex-installation-id",
	"session_id", "session-id",
	"thread_id", "thread-id",
	"turn_id", "turn-id",
	"window_id", "x-codex-window-id", "context_window_id",
	"parent_thread_id", "x-codex-parent-thread-id", "forked_from_thread_id",
	"parent_turn_id", "root_turn_id",
	"x-client-request-id",
}

// codexAccountIdentityNamespace 返回凭证的 ChatGPT 账号命名空间；同一 Team 账号下
// 的不同用户也要彼此隔离。没有账号 ID 时返回空，调用方不做映射。
func codexAccountIdentityNamespace(cfg *model.Config) string {
	if cfg == nil || !cfg.UsesCodexOAuth() {
		return ""
	}
	accountID := strings.TrimSpace(cfg.CodexAccountID)
	if accountID == "" {
		return ""
	}
	if userID := strings.TrimSpace(cfg.CodexUserID); userID != "" {
		return "chatgpt:" + accountID + ":user:" + userID
	}
	return "chatgpt:" + accountID
}

// scopeCodexAccountIdentity 映射单个 ID。官方复合值以 ':' 分隔（窗口 ID
// "<thread_id>:<generation>"、内部会话 prompt_cache_key "<source>:<parent_thread_id>"），
// 只映射其中的 UUID 段，其余段原样保留，映射后仍与对应的 thread_id 相等。
// 不含 UUID 段的值整体映射为 v4 UUID。
func scopeCodexAccountIdentity(namespace, raw string) string {
	segments := strings.Split(raw, ":")
	mapped := false
	for i, segment := range segments {
		if len(segment) != 36 {
			continue
		}
		if id, err := uuid.Parse(segment); err == nil {
			segments[i] = scopeCodexUUID(namespace, id).String()
			mapped = true
		}
	}
	if mapped {
		return strings.Join(segments, ":")
	}
	id := codexAccountIdentityDigest(namespace, raw)
	id[6] = (id[6] & 0x0f) | 0x40
	id[8] = (id[8] & 0x3f) | 0x80
	return id.String()
}

// scopeCodexUUID 保留原 UUID 的版本与变体位；UUIDv7（官方 session/thread/turn ID）
// 还保留 48 位毫秒时间戳，只替换随机位，映射结果仍是时间有序的合法 v7。
func scopeCodexUUID(namespace string, id uuid.UUID) uuid.UUID {
	scoped := codexAccountIdentityDigest(namespace, id.String())
	if id.Version() == 7 {
		copy(scoped[:6], id[:6])
	}
	scoped[6] = (scoped[6] & 0x0f) | (id[6] & 0xf0)
	scoped[8] = (scoped[8] & 0x3f) | (id[8] & 0xc0)
	return scoped
}

func codexAccountIdentityDigest(namespace, raw string) uuid.UUID {
	digest := sha256.Sum256([]byte("ccload:codex-account-identity:" + codexAccountIdentityVersion +
		"\x00" + namespace + "\x00" + raw))
	var id uuid.UUID
	copy(id[:], digest[:len(id)])
	return id
}

// scopeCodexIdentityObject 映射 JSON 对象里的身份字段。用 sjson 原位改写，保留
// 客户端的键序与格式。
func scopeCodexIdentityObject(raw []byte, namespace string, withTurnMetadata bool) ([]byte, bool) {
	changed := false
	for _, field := range codexAccountIdentityFields {
		value := gjson.GetBytes(raw, field)
		if value.Type != gjson.String || strings.TrimSpace(value.String()) == "" {
			continue
		}
		next, err := sjson.SetBytes(raw, field, scopeCodexAccountIdentity(namespace, strings.TrimSpace(value.String())))
		if err == nil {
			raw = next
			changed = true
		}
	}
	if !withTurnMetadata {
		return raw, changed
	}
	if metadata := gjson.GetBytes(raw, codexTurnMetadataField); metadata.Type == gjson.String {
		if scoped, ok := scopeCodexTurnMetadata(metadata.String(), namespace); ok {
			if next, err := sjson.SetBytes(raw, codexTurnMetadataField, scoped); err == nil {
				raw = next
				changed = true
			}
		}
	}
	return raw, changed
}

func scopeCodexTurnMetadata(raw, namespace string) (string, bool) {
	if !gjson.Valid(raw) || !gjson.Parse(raw).IsObject() {
		return raw, false
	}
	scoped, changed := scopeCodexIdentityObject([]byte(raw), namespace, false)
	return string(scoped), changed
}

// scopeCodexAccountIdentityBody 映射 prompt_cache_key 与 client_metadata（含内嵌的
// turn metadata）。大 body 只做两次拼接，不整体反序列化。
func scopeCodexAccountIdentityBody(body []byte, namespace string) []byte {
	if namespace == "" || len(body) == 0 {
		return body
	}
	if clientMetadata := gjson.GetBytes(body, "client_metadata"); clientMetadata.IsObject() {
		if scoped, changed := scopeCodexIdentityObject([]byte(clientMetadata.Raw), namespace, true); changed {
			if next, err := sjson.SetRawBytes(body, "client_metadata", scoped); err == nil {
				body = next
			}
		}
	}
	if promptCacheKey := gjson.GetBytes(body, "prompt_cache_key"); promptCacheKey.Type == gjson.String {
		if raw := strings.TrimSpace(promptCacheKey.String()); raw != "" {
			if next, err := sjson.SetBytes(body, "prompt_cache_key", scopeCodexAccountIdentity(namespace, raw)); err == nil {
				body = next
			}
		}
	}
	return body
}

func scopeCodexAccountIdentityHeaders(h http.Header, namespace string) {
	if h == nil || namespace == "" {
		return
	}
	for _, name := range codexAccountIdentityFields {
		if raw := strings.TrimSpace(h.Get(name)); raw != "" {
			h.Set(name, scopeCodexAccountIdentity(namespace, raw))
		}
	}
	if raw := strings.TrimSpace(h.Get(codexTurnMetadataField)); raw != "" {
		if scoped, ok := scopeCodexTurnMetadata(raw, namespace); ok {
			h.Set(codexTurnMetadataField, scoped)
		}
	}
}
