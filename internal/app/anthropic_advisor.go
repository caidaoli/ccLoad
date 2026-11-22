package app

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/anthropicauth"

	"github.com/tidwall/gjson"
)

/**
 * @brief 保存 advisor 调用归属及已证实的凭证轮换关系，每个服务实例共用 4096 项上限。
 * @note 零值可用，锁保护归属检查、首次登记及淘汰。
 */
type anthropicAdvisorOwners struct {
	mu      sync.Mutex                 ///< 保护下述索引与使用顺序。
	entries map[[32]byte]*list.Element ///< 工具调用或凭证轮换的摘要索引。
	recent  list.List                  ///< 最近使用的归属排在队首。
}

/**
 * @brief 同一条已确认归属或轮换关系的有效期随使用续期。
 */
type advisorOwnerEntry struct {
	key     [32]byte  ///< 调用 ID 或轮换来源凭证的摘要索引。
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
 * @brief 从 Anthropic OAuth 凭证派生 advisor 身份。
 * @param credential 当前尝试使用的凭证。
 * @return 已知时取账号身份，否则取实际访问及刷新令牌的摘要。
 */
func anthropicAdvisorOAuthOwner(credential *anthropicauth.Credential) [32]byte {
	identity := anthropicAccountIdentity(credential)
	if identity == "" {
		identity = "token:" + credential.AccessToken + "\x00" + credential.RefreshToken
	}
	return sha256.Sum256([]byte("anthropic-oauth\x00" + identity))
}

/**
 * @brief 在 OAuth 刷新证实同一凭证时关联历史归属与新身份。
 * @param previous 合并前的账号资料。
 * @param refreshed 合并后实际应用的账号资料。
 * @note 同一 refresh grant 能证明缺少账号资料时的连续性。已知账号发生冲突时保持隔离。
 */
func (s *anthropicAdvisorOwners) linkRefresh(previous, refreshed *anthropicauth.Credential) {
	if previous.AccountUUID != "" || previous.EmailAddress != "" && !strings.EqualFold(previous.EmailAddress, refreshed.EmailAddress) {
		return
	}
	prior := anthropicAdvisorOAuthOwner(previous)
	next := anthropicAdvisorOAuthOwner(refreshed)
	if prior == next {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	resolved := s.resolveOwner(prior, now)
	if previous.EmailAddress != "" && resolved != prior {
		return // 已证实的 UUID 归属不随另一份邮箱凭证迁移。
	}
	next = s.resolveOwner(next, now)
	if resolved != next {
		s.remember(advisorRefreshKey(resolved), next, now)
	}
}

/**
 * @brief 为刷新映射生成与工具调用索引隔离的查找键。
 * @param owner 刷新前的凭证身份。
 * @return 轮换来源索引。
 */
func advisorRefreshKey(owner [32]byte) [32]byte {
	return sha256.Sum256(append([]byte("refresh\x00"), owner[:]...))
}

/**
 * @brief 沿已确认的 OAuth 刷新找到当前归属。
 * @param owner 请求或历史原来使用的身份。
 * @param now 本次检查时间。
 * @return 最近一次仍有效的凭证身份。
 * @note 调用者持有归属表的锁。
 */
func (s *anthropicAdvisorOwners) resolveOwner(owner [32]byte, now time.Time) [32]byte {
	for {
		entry := s.lookup(advisorRefreshKey(owner), now)
		if entry == nil {
			return owner
		}
		owner = entry.owner
	}
}

/**
 * @brief 查找有效记录并续期，删除已过期记录。
 * @param key 工具调用或凭证轮换的索引。
 * @param now 本次访问时间。
 * @return 有效记录，未命中返回 nil。
 * @note 调用者持有归属表的锁。
 */
func (s *anthropicAdvisorOwners) lookup(key [32]byte, now time.Time) *advisorOwnerEntry {
	elem := s.entries[key]
	if elem == nil {
		return nil
	}
	entry := elem.Value.(*advisorOwnerEntry)
	if now.Before(entry.expires) {
		entry.expires = now.Add(24 * time.Hour)
		s.recent.MoveToFront(elem)
		return entry
	}
	delete(s.entries, key)
	s.recent.Remove(elem)
	return nil
}

/**
 * @brief 登记归属或轮换关系，达到上限时淘汰最久未使用记录。
 * @param key 要登记的索引，调用者已确认不存在有效记录。
 * @param owner 工具归属或刷新后的凭证身份。
 * @param now 本次登记时间。
 * @note 调用者持有归属表的锁。
 */
func (s *anthropicAdvisorOwners) remember(key, owner [32]byte, now time.Time) {
	if s.entries == nil {
		s.entries = make(map[[32]byte]*list.Element)
	}
	if len(s.entries) == 4096 {
		oldest := s.recent.Back()
		delete(s.entries, oldest.Value.(*advisorOwnerEntry).key)
		s.recent.Remove(oldest)
	}
	s.entries[key] = s.recent.PushFront(&advisorOwnerEntry{key: key, owner: owner, expires: now.Add(24 * time.Hour)})
}

/**
 * @brief 查询指定调用的凭证归属，可在收到上游成功响应后登记未知调用。
 * @param id 非空 advisor 工具调用 ID。
 * @param accepted 是否已从本次上游得到归属证据。
 * @return 已登记给其他凭证时为 true；未知或本凭证签发时为 false。
 * @note 并发首次登记以先到者为准，不保存凭证或工具 ID 原文。
 */
func (a *anthropicAdvisorAttempt) foreign(id string, accepted bool) bool {
	key := sha256.Sum256([]byte("tool\x00" + a.caller + "\x00" + id))
	s := a.store
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	owner := s.resolveOwner(a.owner, now)
	if entry := s.lookup(key, now); entry != nil {
		entry.owner = s.resolveOwner(entry.owner, now)
		return entry.owner != owner
	}
	if accepted {
		s.remember(key, owner, now)
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
