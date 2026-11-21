package app

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"sync"
	"time"

	"github.com/tidwall/gjson"
)

/**
 * @brief 保存上游已签发或接受的 advisor 调用 ID 及对应凭证的摘要；每个服务实例最多保留 4096 项。
 * @note 零值可用，锁保护归属检查、首次登记及淘汰。
 */
type anthropicAdvisorOwners struct {
	mu      sync.Mutex                 ///< 保护下述索引与使用顺序。
	entries map[[32]byte]*list.Element ///< 调用方与工具 ID 的摘要索引。
	recent  list.List                  ///< 最近使用的归属排在队首。
}

/**
 * @brief 同一条已确认归属的有效期随使用续期。
 */
type advisorOwnerEntry struct {
	key     [32]byte  ///< 调用方与工具 ID 的摘要。
	owner   [32]byte  ///< 实际上游凭证的稳定身份摘要。
	expires time.Time ///< 过期后允许重新识别归属。
}

/**
 * @brief 将请求的 advisor 历史与本次已选凭证绑定，跨渠道重试重新建立。
 */
type anthropicAdvisorAttempt struct {
	store   *anthropicAdvisorOwners ///< 服务实例共享的归属表。
	caller  string                  ///< 已认证的下游 token 摘要。
	owner   [32]byte                ///< 本次出站凭证的稳定身份摘要。
	pending []string                ///< 最终请求携带的历史 ID，经上游接受后才能登记。
}

/**
 * @brief 查询指定调用的凭证归属，可在收到上游成功响应后登记未知调用。
 * @param id 非空 advisor 工具调用 ID。
 * @param accepted 是否已从本次上游得到归属证据。
 * @return 已登记给其他凭证时为 true；未知或本凭证签发时为 false。
 * @note 并发首次登记以先到者为准，不保存凭证或工具 ID 原文。
 */
func (a *anthropicAdvisorAttempt) foreign(id string, accepted bool) bool {
	key := sha256.Sum256([]byte(a.caller + "\x00" + id))
	s := a.store
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	expires := now.Add(24 * time.Hour)
	if elem := s.entries[key]; elem != nil {
		entry := elem.Value.(*advisorOwnerEntry)
		if now.Before(entry.expires) {
			entry.expires = expires
			s.recent.MoveToFront(elem)
			return entry.owner != a.owner
		}
		delete(s.entries, key)
		s.recent.Remove(elem)
	}
	if accepted {
		if s.entries == nil {
			s.entries = make(map[[32]byte]*list.Element)
		}
		if len(s.entries) == 4096 {
			oldest := s.recent.Back()
			delete(s.entries, oldest.Value.(*advisorOwnerEntry).key)
			s.recent.Remove(oldest)
		}
		s.entries[key] = s.recent.PushFront(&advisorOwnerEntry{key: key, owner: a.owner, expires: expires})
	}
	return false
}

/**
 * @brief 提取直接或嵌套加密 advisor 结果的调用 ID。
 * @param block Anthropic 内容块。
 * @return advisor 工具调用 ID；普通工具块或没有 ID 时为空。
 */
func advisorBlockID(block gjson.Result) string {
	switch block.Get("type").Str {
	case "server_tool_use":
		if block.Get("name").Str == "advisor" {
			return block.Get("id").Str
		}
	case "advisor_tool_result", "advisor_redacted_result":
		return block.Get("tool_use_id").Str
	case "tool_result":
		for _, part := range block.Get("content").Array() {
			if part.Get("type").Str == "advisor_redacted_result" {
				return block.Get("tool_use_id").Str
			}
		}
	}
	return ""
}

/**
 * @brief 快速跳过不含 advisor 块的正文；带 Unicode 转义的输入交由 JSON 解析器判定。
 * @param body 已验证的请求正文。
 * @return 正文可能含有 advisor 标识时为 true。
 */
func mayContainAdvisor(body []byte) bool {
	return bytes.Contains(body, []byte("advisor")) || bytes.Contains(body, []byte(`\u`))
}

/**
 * @brief 清理属于其他凭证的 advisor 历史，保留普通工具调用与它的结果配对。
 * @param body 本次凭证即将发送的 Messages 或 count_tokens 正文。
 * @return 按原顺序保留的消息；没有外来块时原样返回。
 */
func (a *anthropicAdvisorAttempt) stripForeign(body []byte) []byte {
	if !mayContainAdvisor(body) {
		return body
	}
	messages := gjson.GetBytes(body, "messages").Array()
	ordinary := make(map[string]bool)
	for _, msg := range messages {
		for _, block := range msg.Get("content").Array() {
			if block.Get("type").Str == "tool_use" {
				ordinary[block.Get("id").Str] = true
			}
		}
	}
	var changed bool
	kept := make([]string, 0, len(messages))
	for _, msg := range messages {
		content := msg.Get("content")
		blocks := content.Array()
		var parts []string
		removed := false
		for _, block := range blocks {
			id := advisorBlockID(block)
			if id == "" || !a.foreign(id, false) {
				parts = append(parts, block.Raw)
				continue
			}
			removed = true
			if block.Get("type").Str == "tool_result" && ordinary[id] {
				var rest []string
				for _, nested := range block.Get("content").Array() {
					if nested.Get("type").Str != "advisor_redacted_result" {
						rest = append(rest, nested.Raw)
					}
				}
				replacement := `""`
				if len(rest) > 0 {
					replacement = joinJSONRaw(rest)
				}
				parts = append(parts, string(setJSONRaw([]byte(block.Raw), "content", replacement)))
			}
		}
		if !removed {
			kept = append(kept, msg.Raw)
			continue
		}
		changed = true
		if len(parts) > 0 {
			kept = append(kept, string(setJSONRaw([]byte(msg.Raw), "content", joinJSONRaw(parts))))
		}
	}
	if !changed {
		return body
	}
	return setJSONRaw(body, "messages", joinJSONRaw(kept))
}

/**
 * @brief 收集实际出站正文中的 advisor 历史，渠道规则移除的内容不会建立归属。
 * @param body 规则及签名处理后的请求正文。
 */
func (a *anthropicAdvisorAttempt) sent(body []byte) {
	if !mayContainAdvisor(body) {
		return
	}
	for _, msg := range gjson.GetBytes(body, "messages").Array() {
		for _, block := range msg.Get("content").Array() {
			if id := advisorBlockID(block); id != "" {
				a.pending = append(a.pending, id)
			}
		}
	}
}

/**
 * @brief 从完整上游 JSON 中识别已签发的 advisor 块及被接受的历史。
 * @param data 成功响应的原始负载。
 */
func (a *anthropicAdvisorAttempt) observeJSON(data []byte) {
	if a != nil && (len(a.pending) > 0 || mayContainAdvisor(data)) && gjson.ValidBytes(data) {
		a.observe(gjson.ParseBytes(data))
	}
}

/**
 * @brief 在成功的 Messages JSON 或 SSE 事件中登记实际接受的历史与新签发的 advisor ID。
 * @param root 上游原始 JSON 对象或已解析的 SSE data。
 */
func (a *anthropicAdvisorAttempt) observe(root gjson.Result) {
	var content gjson.Result
	switch root.Get("type").Str {
	case "message_start":
		content = root.Get("message.content")
	case "message":
		content = root.Get("content")
	case "content_block_start":
		if id := advisorBlockID(root.Get("content_block")); id != "" {
			a.foreign(id, true)
		}
		return
	default:
		return
	}
	for _, id := range a.pending {
		a.foreign(id, true)
	}
	a.pending = nil
	for _, block := range content.Array() {
		if id := advisorBlockID(block); id != "" {
			a.foreign(id, true)
		}
	}
}
