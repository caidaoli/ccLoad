package app

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sync"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"

	"github.com/tidwall/gjson"
)

/**
 * @brief CodeBuddy 身份句局部修复的日志策略名。
 */
const pauseCodeBuddyIdentityStrategy = "pause_codebuddy_identity"

/**
 * @brief Magpie OPENERS 的身份句形态；已有逗号和跨句文本不匹配。
 */
var codeBuddyIdentityOpener = regexp.MustCompile(`(?i)(\bYou are (?:an?|the)\s[^\n.,]{0,60}?\b(?:agent|assistant))\s+(powered by|running on|running in|built on|based on)\b`)

/**
 * @brief 服务实例内最多保留 256 个已成功的系统提示词修复摘要，按插入顺序淘汰。
 * @note 零值可用，并发安全；不保存提示词原文，不落数据库。
 */
type codeBuddyCommaCache struct {
	mu    sync.Mutex            ///< 保护摘要集合与淘汰顺序。
	keys  map[[32]byte]struct{} ///< 已成功的渠道、上游、模型及系统消息组合。
	order [][32]byte            ///< 首次成功的先后顺序。
}

/**
 * @brief 对命中过往成功策略的请求预先执行同一身份句修复。
 * @param cfg 当前 CodeBuddy 渠道。
 * @param target 实际请求的上游 URL。
 * @param body 已完成规则、角色归一与隐私遮蔽的出站正文。
 * @return 命中时返回局部改写后的正文，否则返回原文。
 */
func (c *codeBuddyCommaCache) apply(cfg *model.Config, target *url.URL, body []byte) []byte {
	key := codeBuddyCommaKey(cfg, target, body)
	c.mu.Lock()
	_, found := c.keys[key]
	c.mu.Unlock()
	if found {
		body, _ = pauseCodeBuddyIdentity(body)
	}
	return body
}

/**
 * @brief 记录一次已成功且修改了系统提示词的修复。
 * @param key 修复前请求的作用域摘要；重复记录不改变淘汰顺序。
 */
func (c *codeBuddyCommaCache) remember(key [32]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, found := c.keys[key]; found {
		return
	}
	if c.keys == nil {
		c.keys = make(map[[32]byte]struct{})
	}
	if len(c.order) == 256 {
		delete(c.keys, c.order[0])
		c.order = append(c.order[:0], c.order[1:]...)
	}
	c.keys[key] = struct{}{}
	c.order = append(c.order, key)
}

/**
 * @brief 对渠道、上游 origin、模型与完整系统消息生成稳定摘要。
 * @param cfg 当前渠道，使用其稳定 ID。
 * @param target 已解析的实际上游 URL，仅使用 scheme 与 host。
 * @param body 已最终化的请求正文。
 * @return 缓存作用域的 SHA-256 摘要。
 */
func codeBuddyCommaKey(cfg *model.Config, target *url.URL, body []byte) [32]byte {
	var systems []json.RawMessage
	for _, message := range gjson.GetBytes(body, "messages").Array() {
		if message.Get("role").String() == "system" {
			systems = append(systems, json.RawMessage(message.Raw))
		}
	}
	raw, _ := json.Marshal([]any{cfg.ID, target.Scheme + "://" + target.Host, gjson.GetBytes(body, "model").String(), systems})
	return sha256.Sum256(raw)
}

/**
 * @brief 仅在系统和助手文本中的身份连接词前插入逗号。
 * @param body 有效的 Chat 请求；用户、工具和非文本内容块不改写。
 * @return 独立改写后的正文；无匹配时返回原文。
 * @return 至少改写一处时为 true；已有逗号不会重复添加。
 */
func pauseCodeBuddyIdentity(body []byte) ([]byte, bool) {
	changed := false
	for i, message := range gjson.GetBytes(body, "messages").Array() {
		role := message.Get("role").String()
		if role != "system" && role != "assistant" {
			continue
		}
		content := message.Get("content")
		rewrite := func(path string, text gjson.Result) {
			if text.Type != gjson.String {
				return
			}
			rewritten := codeBuddyIdentityOpener.ReplaceAllString(text.Str, "$1, $2")
			if rewritten != text.Str {
				body = setJSONValue(body, path, rewritten)
				changed = true
			}
		}
		if content.IsArray() {
			for j, part := range content.Array() {
				if kind := part.Get("type"); kind.Exists() && kind.String() != "text" {
					continue
				}
				rewrite(fmt.Sprintf("messages.%d.content.%d.text", i, j), part.Get("text"))
			}
		} else {
			rewrite(fmt.Sprintf("messages.%d.content", i), content)
		}
	}
	return body, changed
}

/**
 * @brief 识别 CodeBuddy 的结构化渠道调用拒绝码，接受数字和字符串形式。
 * @param body 上游 JSON 错误或已提取的 SSE 错误事件。
 * @return 顶层或 error.code 精确为 11128 时返回 true。
 */
func isCodeBuddyInvocationRefused(body []byte) bool {
	root := gjson.ParseBytes(body)
	return root.Get("code").String() == "11128" || root.Get("error.code").String() == "11128"
}

/**
 * @brief 为未提交给客户端的 CodeBuddy 11128 拒绝生成身份句修复体。
 * @param upstream 当前上游协议。
 * @param cfg 当前渠道，非 CodeBuddy 不处理。
 * @param plan 含实际发送正文的转换计划。
 * @param res 当前发送结果，已提交响应或非 400 不处理。
 * @return 修复后的正文，无匹配时返回 nil。
 * @return 修复策略名，无修复时为空。
 * @return 正文发生改动且允许重试时为 true。
 */
func codeBuddyRetryBodyFor11128(upstream protocol.Protocol, cfg *model.Config, plan protocol.TransformPlan, res *fwResult) ([]byte, string, bool) {
	if !isCodeBuddyChatRequest(cfg, upstream) || res == nil || res.ResponseCommitted {
		return nil, "", false
	}
	errorBody, status := forwardResultErrorPayload(res)
	if status != http.StatusBadRequest || !isCodeBuddyInvocationRefused(errorBody) {
		return nil, "", false
	}
	body, changed := pauseCodeBuddyIdentity(plan.TranslatedBody)
	if !changed {
		return nil, "", false
	}
	return body, pauseCodeBuddyIdentityStrategy, true
}
