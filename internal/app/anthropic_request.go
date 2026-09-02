package app

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	cliproxysignature "ccLoad/internal/protocol/cliproxy/signature"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Anthropic 线协议改写全程以 []byte 为唯一表示：gjson 读、sjson 就地写。
//
// 不用 map[string]any 中转不是风格偏好——map 丢键序。上游按 body 形态做 Claude Code
// 指纹识别，一旦把请求解成 map 再整体重编码，所有网关新增的键都会按字母序落位，与
// 原生客户端形态对不上；为了绕开这一点还要额外维护一层「原始字节 vs 目标 map」的
// 差分渲染。就地改写让未触及的字节原样保留，键序问题从根上不存在。

// normalizeAnthropicMessagesBody is the single native Claude body boundary.
// Protocol conversion produces the shape; this function only enforces Anthropic
// wire invariants shared by API-key and OAuth attempts.
func normalizeAnthropicMessagesBody(body []byte) ([]byte, error) {
	if !isAnthropicJSONObject(body) {
		return nil, errors.New("normalize Anthropic request: invalid JSON body")
	}
	return encodeNormalizedAnthropicRequest(body), nil
}

// isAnthropicJSONObject 是所有 sjson 就地改写的准入守卫。守卫必须与消费者同源：
// sjson 对截断输入静默返回损坏结果且 err == nil，所以入口这一次校验是唯一防线。
func isAnthropicJSONObject(body []byte) bool {
	return gjson.ValidBytes(body) && gjson.ParseBytes(body).IsObject()
}

// encodeNormalizedAnthropicRequest 只收尾 Anthropic Messages 的通用 wire 形状。
// 入口守卫由调用方（normalizeAnthropicMessagesBody / 最终化边界）负责，这里三步
// 都不会失败，所以不返回 error。
//
// CCH 不在这里签：它按凭证与上游 origin 条件化，必须由持有这两项上下文的最终发送
// 边界处理，判据见 anthropicCCHSigningEnabled。
func encodeNormalizedAnthropicRequest(body []byte) []byte {
	body = normalizeAnthropicMessagesRequest(body)
	body = orderAnthropicCacheControlWireShape(body)
	body, _ = cliproxysignature.SanitizeClaudeMessagesForClaudeUpstream(
		body, jsonStringValue(gjson.GetBytes(body, "model")),
	)
	return body
}

func normalizeAnthropicMessagesRequest(body []byte) []byte {
	body = normalizeAnthropicToolChoice(body)
	body = normalizeAnthropicThinking(body)
	body = normalizeAnthropicSampling(body)
	body = sanitizeAnthropicOAuthMessages(body)
	if countAnthropicCacheControls(body) == 0 {
		body = ensureAnthropicCacheControls(body)
	}
	body = normalizeAnthropicCacheControlTTL(body)
	return enforceAnthropicCacheControlLimit(body, 4)
}

func validateAnthropicLegacySystemMessages(body []byte) error {
	modelName := jsonStringValue(gjson.GetBytes(body, "model"))
	if !anthropicUsesLegacySystemReminder(modelName) {
		return nil
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return nil
	}
	var failure error
	index := 0
	messages.ForEach(func(_, message gjson.Result) bool {
		current := index
		index++
		if !message.IsObject() {
			return true
		}
		if strings.EqualFold(strings.TrimSpace(jsonStringValue(message.Get("role"))), "system") {
			failure = &anthropicRequestValidationError{message: fmt.Sprintf(
				"Anthropic model %q does not support system messages in messages[%d]", modelName, current,
			)}
			return false
		}
		return true
	})
	return failure
}

type anthropicRequestValidationError struct{ message string }

func (e *anthropicRequestValidationError) Error() string { return e.message }

func countAnthropicCacheControls(body []byte) int {
	count := 0
	forEachAnthropicCacheBlock(body, func(_ string, block gjson.Result) bool {
		if block.Get("cache_control").Exists() {
			count++
		}
		return true
	})
	return count
}

func normalizeAnthropicToolChoice(body []byte) []byte {
	if tools := gjson.GetBytes(body, "tools"); !tools.IsArray() || jsonMemberCount(tools) == 0 {
		body = deleteJSONPath(body, "tool_choice")
	}
	choiceType := strings.ToLower(strings.TrimSpace(jsonStringValue(gjson.GetBytes(body, "tool_choice.type"))))
	if choiceType != "any" && choiceType != "tool" {
		return body
	}
	body = deleteJSONPath(body, "thinking")
	return deleteAnthropicOutputEffort(body)
}

func normalizeAnthropicThinking(body []byte) []byte {
	thinking := gjson.GetBytes(body, "thinking")
	if !thinking.IsObject() {
		return body
	}
	typ := strings.ToLower(strings.TrimSpace(jsonStringValue(thinking.Get("type"))))
	switch typ {
	case "auto":
		body = setJSONRaw(body, "thinking.type", `"adaptive"`)
		typ = "adaptive"
	case "disabled", "off", "none":
		body = deleteJSONPath(body, "thinking")
		return deleteAnthropicOutputEffort(body)
	}
	if typ != "adaptive" {
		return body
	}
	if budget, ok := jsonIntegerValue(gjson.GetBytes(body, "thinking.budget_tokens")); ok && budget > 0 {
		body = setAnthropicOutputEffort(body, anthropicBudgetToEffort(int(budget)))
	}
	return deleteJSONPath(body, "thinking.budget_tokens")
}

func deleteAnthropicOutputEffort(body []byte) []byte {
	if !gjson.GetBytes(body, "output_config").IsObject() {
		return body
	}
	body = deleteJSONPath(body, "output_config.effort")
	if jsonMemberCount(gjson.GetBytes(body, "output_config")) == 0 {
		body = deleteJSONPath(body, "output_config")
	}
	return body
}

func normalizeAnthropicSampling(body []byte) []byte {
	// Claude Code does not forward caller sampling knobs. Keeping both temperature
	// and top_p is invalid, and thinking requests reject top_k as well.
	body = deleteJSONPath(body, "temperature")
	body = deleteJSONPath(body, "top_p")
	switch strings.ToLower(strings.TrimSpace(jsonStringValue(gjson.GetBytes(body, "thinking.type")))) {
	case "enabled", "adaptive", "auto":
		body = deleteJSONPath(body, "top_k")
	}
	return body
}

func ensureAnthropicCacheControls(body []byte) []byte {
	body = ensureAnthropicToolCacheControl(body)
	body = ensureAnthropicSystemCacheControl(body)
	return ensureAnthropicMessageCacheControl(body)
}

func ensureAnthropicToolCacheControl(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	lastEligible := -1
	for index, tool := range tools.Array() {
		if !tool.IsObject() {
			continue
		}
		if tool.Get("cache_control").Exists() {
			return body
		}
		if tool.Get("defer_loading").Type != gjson.True {
			lastEligible = index
		}
	}
	if lastEligible < 0 {
		return body
	}
	return setJSONRaw(body, "tools."+strconv.Itoa(lastEligible)+".cache_control", anthropicEphemeralCacheControl())
}

func ensureAnthropicSystemCacheControl(body []byte) []byte {
	system := gjson.GetBytes(body, "system")
	switch {
	case !system.Exists():
		return body
	case system.Type == gjson.String:
		if strings.TrimSpace(system.String()) == "" {
			return body
		}
		block := anthropicTextBlockRaw(system.String(), anthropicEphemeralCacheControl())
		return setJSONRaw(body, "system", "["+block+"]")
	case system.IsArray():
		last := -1
		for index, block := range system.Array() {
			if !block.IsObject() {
				continue
			}
			if block.Get("cache_control").Exists() {
				return body
			}
			last = index
		}
		if last < 0 {
			return body
		}
		return setJSONRaw(body, "system."+strconv.Itoa(last)+".cache_control", anthropicEphemeralCacheControl())
	default:
		return body
	}
}

func ensureAnthropicMessageCacheControl(body []byte) []byte {
	messages := gjson.GetBytes(body, "messages")
	if !messages.IsArray() {
		return body
	}
	userIndexes := make([]int, 0, 2)
	for index, message := range messages.Array() {
		if !message.IsObject() {
			continue
		}
		if content := message.Get("content"); content.IsArray() {
			for _, block := range content.Array() {
				if block.IsObject() && block.Get("cache_control").Exists() {
					return body
				}
			}
		}
		if jsonStringValue(message.Get("role")) == "user" {
			userIndexes = append(userIndexes, index)
		}
	}
	if len(userIndexes) < 2 {
		return body
	}
	target := "messages." + strconv.Itoa(userIndexes[len(userIndexes)-2])
	content := gjson.GetBytes(body, target+".content")
	switch {
	case content.Type == gjson.String:
		block := anthropicTextBlockRaw(content.String(), anthropicEphemeralCacheControl())
		return setJSONRaw(body, target+".content", "["+block+"]")
	case content.IsArray():
		blocks := content.Array()
		for index := len(blocks) - 1; index >= 0; index-- {
			if blocks[index].IsObject() {
				return setJSONRaw(body, target+".content."+strconv.Itoa(index)+".cache_control",
					anthropicEphemeralCacheControl())
			}
		}
	}
	return body
}

// anthropicEphemeralCacheControl 是 Anthropic 线上默认（5m）缓存断点的原始 JSON。
func anthropicEphemeralCacheControl() string { return `{"type":"ephemeral"}` }

// anthropicTextBlockRaw 按原生键序拼一个 text 块：type → text → cache_control。
// 字符串转义交给 sjson，与 encoding/json 完全一致（`<`、`>`、`&` 同样转义）。
func anthropicTextBlockRaw(text, cacheControlRaw string) string {
	block, err := sjson.Set(`{"type":"text"}`, "text", text)
	if err != nil {
		return `{"type":"text","text":""}`
	}
	if cacheControlRaw == "" {
		return block
	}
	withCache, err := sjson.SetRaw(block, "cache_control", cacheControlRaw)
	if err != nil {
		return block
	}
	return withCache
}

// anthropicTextMessageRaw 按原生键序拼一条纯文本消息：role → content。
func anthropicTextMessageRaw(role, content string) string {
	message, err := sjson.Set(`{"role":""}`, "role", role)
	if err != nil {
		return `{"role":"user","content":""}`
	}
	message, err = sjson.Set(message, "content", content)
	if err != nil {
		return `{"role":"user","content":""}`
	}
	return message
}

func normalizeAnthropicCacheControlTTL(body []byte) []byte {
	seenFiveMinutes := false
	var demoted []string
	forEachAnthropicCacheBlock(body, func(path string, block gjson.Result) bool {
		cache := block.Get("cache_control")
		if !cache.IsObject() {
			if cache.Exists() {
				seenFiveMinutes = true
			}
			return true
		}
		if jsonStringValue(cache.Get("ttl")) != "1h" {
			seenFiveMinutes = true
			return true
		}
		if seenFiveMinutes {
			demoted = append(demoted, path+".cache_control.ttl")
		}
		return true
	})
	for _, path := range demoted {
		body = deleteJSONPath(body, path)
	}
	return body
}

// forEachAnthropicCacheBlock 按 Anthropic 的评估顺序（tools → system → messages）
// 遍历所有可以挂 cache_control 的块，并把每个块的 sjson 路径交给 visit；visit 返回
// false 即停止遍历。
//
// path 参数就是这个遍历器存在的理由：调用方拿到路径才能用 sjson 就地改写，不必把块
// 解成 map 再整体回写——键序正是在那一步丢掉的。遍历读的是入参快照，所以 visit 只
// 收集路径、由调用方在遍历结束后统一改写；只增删对象成员时路径始终有效。
func forEachAnthropicCacheBlock(body []byte, visit func(path string, block gjson.Result) bool) {
	root := gjson.ParseBytes(body)
	visitBlocks := func(container gjson.Result, prefix string) bool {
		if !container.IsArray() {
			return true
		}
		for index, block := range container.Array() {
			if !block.IsObject() {
				continue
			}
			if !visit(prefix+"."+strconv.Itoa(index), block) {
				return false
			}
		}
		return true
	}
	if !visitBlocks(root.Get("tools"), "tools") || !visitBlocks(root.Get("system"), "system") {
		return
	}
	messages := root.Get("messages")
	if !messages.IsArray() {
		return
	}
	for index, message := range messages.Array() {
		if !message.IsObject() {
			continue
		}
		if !visitBlocks(message.Get("content"), "messages."+strconv.Itoa(index)+".content") {
			return
		}
	}
}
