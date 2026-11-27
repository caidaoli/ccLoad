package cursorauth

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	sdkv1 "ccLoad/internal/cursorauth/sdkgen/sdk/v1"
	"ccLoad/internal/cursorauth/sdkgen/sdk/v1/sdkv1connect"
)

/**
 * @brief 停放 Agent 的空闲有效期与数量上限。
 */
const (
	sdkParkedAgentTTL   = 30 * time.Minute ///< 超时未续用即删除 Agent
	sdkParkedAgentLimit = 64               ///< 超出时删除最早停放的 Agent
)

/**
 * @brief 一个 Run 正常完成、等待下一普通轮续用的 Agent。
 */
type parkedAgent struct {
	agentID  string
	apiKey   string
	workdir  string
	client   sdkv1connect.SdkAgentServiceClient ///< 创建 Agent 的 Bridge；Bridge 替换后不再可用
	parkedAt time.Time
	timer    *time.Timer ///< 有效期计时器
}

/**
 * @brief Run 正常结束后停放 Agent，供客户端带完整历史的下一轮直接续发。
 * @param session 已完成的会话。
 * @return 已停放时为 true；缺少可信身份或可匹配的回复，或执行器已关闭时为 false，由调用方删除 Agent。
 * @note 持 r.mu；超出上限时删除最早停放的 Agent。
 */
func (r *SDKRunner) parkAgent(session *sdkSession) bool {
	if !session.scope.reusable() {
		return false
	}
	session.eventMu.Lock()
	reply := session.turnText.String()
	prefix := session.transcriptDigest
	session.eventMu.Unlock()
	anchor := replyAnchor(reply)
	if anchor == "" || prefix == "" {
		return false
	}
	key := parkedAgentKey(session.scope, session.apiKey, session.toolsDigest, prefix, anchor)
	entry := &parkedAgent{
		agentID: session.agentID, apiKey: session.apiKey, workdir: session.workdir,
		client: session.client, parkedAt: time.Now(),
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return false
	}
	if r.parked == nil {
		r.parked = make(map[string]*parkedAgent)
	}
	if old := r.parked[key]; old != nil {
		r.deleteParkedAgentLocked(key, old)
	}
	if len(r.parked) >= sdkParkedAgentLimit {
		oldestKey := ""
		var oldest *parkedAgent
		for parkedKey, parked := range r.parked {
			if oldest == nil || parked.parkedAt.Before(oldest.parkedAt) {
				oldestKey, oldest = parkedKey, parked
			}
		}
		r.deleteParkedAgentLocked(oldestKey, oldest)
	}
	entry.timer = time.AfterFunc(sdkParkedAgentTTL, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.parked[key] == entry {
			r.deleteParkedAgentLocked(key, entry)
		}
	})
	r.parked[key] = entry
	return true
}

/**
 * @brief 计算续发普通轮的停放键与可发送增量。
 * @param apiKey 当前请求的 User API Key。
 * @param toolsDigest 当前请求的工具摘要。
 * @param request 当前请求的完整消息与工具结果。
 * @return 停放键；不是普通续轮或增量不兼容时为空。
 * @return 最后一条 assistant 之后的新消息；无停放键时为 nil。
 * @details 历史在最后一条 assistant 处切分：之前的消息须与上一请求完全一致，assistant 纯文本须与 Agent 上一轮回复逐字节一致。
 */
func parkedAgentContinuation(apiKey, toolsDigest string, request Request) (string, []transcriptEntry) {
	if !request.SessionScope.reusable() || len(request.ToolResults) > 0 {
		return "", nil
	}
	entries := request.transcript
	last := -1
	for index, entry := range entries {
		if entry.role == "assistant" {
			last = index
		}
	}
	if last < 0 || last == len(entries)-1 || !entries[last].plainAssistant {
		return "", nil
	}
	tail := entries[last+1:]
	for _, entry := range tail {
		if entry.toolResult || hasURLImage(entry.images) ||
			(entry.role != "user" && entry.role != "system" && entry.role != "developer") {
			return "", nil
		}
	}
	anchor := replyAnchor(entries[last].body)
	if anchor == "" {
		return "", nil
	}
	return parkedAgentKey(request.SessionScope, apiKey, toolsDigest, transcriptDigest(entries[:last]), anchor), tail
}

/**
 * @brief 在完整历史不可发送时判断是否值得准备续用停放 Agent。
 * @param key 由当前消息历史计算的停放键；为空表示无法续用。
 * @return 目前仍有匹配项时为 true；发送前还须重新核对。
 * @note 持 r.mu，仅检查，不占用停放项。
 */
func (r *SDKRunner) hasParkedAgent(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.parked[key] != nil
}

/**
 * @brief 领取匹配的停放 Agent，转交后续 Send。
 * @param ctx 当前请求上下文；取消后不领取。
 * @param key 由当前消息历史计算的停放键。
 * @param client 当前 Bridge Agent 客户端。
 * @return 可用的 Agent ID；无匹配或旧 Bridge 已失效时为空。
 * @return 请求已结束时为取消原因，其他情况为 nil。
 * @note 持 r.mu；取出即移除，Bridge 已替换的 Agent 随旧进程失效。
 */
func (r *SDKRunner) takeParkedAgent(ctx context.Context, key string, client sdkv1connect.SdkAgentServiceClient) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if ctx.Err() != nil {
		return "", context.Cause(ctx)
	}
	entry := r.parked[key]
	if entry == nil {
		return "", nil
	}
	delete(r.parked, key)
	entry.timer.Stop()
	if entry.client != client {
		return "", nil
	}
	return entry.agentID, nil
}

/**
 * @brief 移除并后台删除一个停放 Agent。
 * @param key 停放键。
 * @param entry 停放的 Agent。
 * @note 调用方须持 r.mu 且执行器未关闭或正在关闭；删除计入 active。
 */
func (r *SDKRunner) deleteParkedAgentLocked(key string, entry *parkedAgent) {
	delete(r.parked, key)
	entry.timer.Stop()
	r.active.Go(func() { r.deleteAgent(entry.client, entry.agentID, entry.workdir, entry.apiKey) })
}

/**
 * @brief 关闭时删除全部停放 Agent。
 * @note 调用方须持 r.mu，且与设置 closed 处于同一临界区。
 */
func (r *SDKRunner) deleteParkedAgentsLocked() {
	for key, entry := range r.parked {
		r.deleteParkedAgentLocked(key, entry)
	}
}

/**
 * @brief 计算停放键。
 * @param scope 当前渠道、已认证的调用方与客户端会话归属。
 * @param apiKey User API Key。
 * @param toolsDigest 自定义工具摘要。
 * @param prefix 上一请求完整消息摘要。
 * @param anchor 上一轮回复摘要。
 * @return 不可逆的复合键。
 */
func parkedAgentKey(scope SessionScope, apiKey, toolsDigest, prefix, anchor string) string {
	h := sha256.New()
	writeDigestFields(h, strconv.FormatInt(scope.ChannelID, 10), strconv.FormatInt(scope.TokenID, 10), scope.SessionID, scope.ThreadID, scope.ClaudeSessionID, scope.ClaudeAgentID, apiKey, toolsDigest, prefix, anchor)
	return string(h.Sum(nil))
}

/**
 * @brief 计算回复原文摘要，保留所有空白。
 * @param text 回复正文。
 * @return 摘要；正文只有空白时为空。
 */
func replyAnchor(text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(text))
	return string(sum[:])
}

/**
 * @brief 计算消息序列摘要。
 * @param entries 按对话顺序排列的消息。
 * @return 摘要；无消息时为空。
 */
func transcriptDigest(entries []transcriptEntry) string {
	if len(entries) == 0 {
		return ""
	}
	h := sha256.New()
	for _, entry := range entries {
		writeDigestFields(h, entry.role, entry.body, strconv.Itoa(len(entry.images)))
		for _, image := range entry.images {
			writeDigestFields(h, image.MimeType, image.Data, image.URL)
		}
	}
	return string(h.Sum(nil))
}

/**
 * @brief 计算 Agent 注册的自定义工具摘要。
 * @param tools 工具名到定义的映射。
 * @return 按工具名排序的确定性摘要；无工具时为空。
 * @return 工具定义无法序列化时返回错误。
 */
func customToolsDigest(tools map[string]*sdkv1.CustomToolDefinition) (string, error) {
	if len(tools) == 0 {
		return "", nil
	}
	h := sha256.New()
	marshal := proto.MarshalOptions{Deterministic: true}
	for _, name := range slices.Sorted(maps.Keys(tools)) {
		encoded, err := marshal.Marshal(tools[name])
		if err != nil {
			return "", err
		}
		writeDigestFields(h, name, string(encoded))
	}
	return string(h.Sum(nil)), nil
}

/**
 * @brief 以长度前缀写入摘要字段，避免字段拼接产生歧义。
 * @param h 目标摘要。
 * @param fields 依次写入的字段。
 */
func writeDigestFields(h hash.Hash, fields ...string) {
	var size [8]byte
	for _, field := range fields {
		binary.BigEndian.PutUint64(size[:], uint64(len(field)))
		h.Write(size[:])
		_, _ = io.WriteString(h, field)
	}
}
