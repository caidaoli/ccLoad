package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"ccLoad/internal/protocol"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	clientProtocolContextKey = "ccLoad.clientProtocol"
	clientPathContextKey     = "ccLoad.clientPath"
)

func captureClientRequestMetadata() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(clientProtocolContextKey, detectClientProtocolFromPath(c.Request.URL.Path))
		c.Set(clientPathContextKey, c.Request.URL.Path)
		c.Next()
	}
}

func captureDashboardProxyMetadata() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := strings.TrimPrefix(c.Request.URL.Path, "/dashboard")
		if path == "" {
			path = "/"
		}
		c.Request.URL.Path = path
		c.Set(clientProtocolContextKey, detectClientProtocolFromPath(path))
		c.Set(clientPathContextKey, path)
		c.Next()
	}
}

func clientRequestMetadata(c *gin.Context) (protocol.Protocol, string) {
	clientProtocol, _ := c.Get(clientProtocolContextKey)
	clientPath, _ := c.Get(clientPathContextKey)

	p, _ := clientProtocol.(protocol.Protocol)
	path, _ := clientPath.(string)
	if path == "" {
		path = c.Request.URL.Path
	}
	if p == "" {
		p = detectClientProtocolFromPath(path)
	}
	return p, path
}

func detectClientProtocolFromPath(path string) protocol.Protocol {
	switch protocol.DetectRequestFamily(path) {
	case protocol.RequestFamilyMessages:
		return protocol.Anthropic
	case protocol.RequestFamilyResponses, protocol.RequestFamilyAlphaSearch:
		return protocol.Codex
	case protocol.RequestFamilyChatCompletions,
		protocol.RequestFamilyCompletions,
		protocol.RequestFamilyEmbeddings,
		protocol.RequestFamilyImages:
		return protocol.OpenAI
	case protocol.RequestFamilyGenerateContent:
		return protocol.Gemini
	default:
		return ""
	}
}

func validateClientBodyMatchesProtocol(clientProtocol protocol.Protocol, body []byte) error {
	if clientProtocol == protocol.OpenAI || !looksLikeOpenAIChatCompletionsBody(body) {
		return nil
	}
	return fmt.Errorf("request body looks like OpenAI chat completions but path uses %s protocol", clientProtocol)
}

func sanitizeCodexAlphaSearchBody(body []byte) []byte {
	if !gjson.ParseBytes(body).IsObject() {
		return body
	}

	updated := body
	for _, field := range []string{"prompt_cache_key", "prompt_cache_retention"} {
		if !gjson.GetBytes(updated, field).Exists() {
			continue
		}
		var err error
		updated, err = sjson.DeleteBytes(updated, field)
		if err != nil {
			return body
		}
	}
	return updated
}

// multimodalMarkerWords 是跨协议的多模态特征词。请求体不含任何一个词时
// 纯文本请求零 JSON 解析直接返回；命中才进入 gjson 精确判定。字节短路允许
// 误触发（正文恰好提到 "image"、"file" 之类），但不允许漏掉任何协议的真实标记。
var multimodalMarkerWords = [][]byte{
	[]byte("image"),       // Anthropic image / OpenAI image_url / Codex input_image
	[]byte("document"),    // Anthropic document
	[]byte("video_url"),   // OpenAI video_url
	[]byte("input_audio"), // OpenAI input_audio
	[]byte("file"),        // OpenAI file / Codex input_file / Gemini fileData/file_data
	[]byte("inline"),      // Gemini inlineData / inline_data
}

var (
	anthropicNonTextTypes = map[string]bool{"image": true, "document": true}
	openaiNonTextTypes    = map[string]bool{"image_url": true, "video_url": true, "input_audio": true, "file": true}
	codexNonTextTypes     = map[string]bool{"input_image": true, "input_file": true}
	// Gemini 的 part 没有 type tag，只能按字段存在性判定；camelCase 与 snake_case
	// 两种写法都要试（协议转换器的双向来源都见过）。
	geminiNonTextKeys = []string{"inlineData", "inline_data", "fileData", "file_data"}
)

// requestHasNonTextContent 判断请求体是否含图片/文件等非文本内容。
// 调用方已按 clientProtocol 完成协议校验，这里按协议走各自的字段路径：
//
//   - Anthropic：messages[].content[] 的 type，含 tool_result 嵌套 content[]
//     （Claude Code 截图最常见的位置）；
//   - OpenAI：messages[].content[] 的 type，content 可以是字符串、对象或数组；
//   - Codex：input[].content[] 的 type，input 本身也可能是纯字符串；
//   - Gemini：contents[].parts[] 存在 inlineData/inline_data/fileData/file_data，
//     图片/文档/音视频共用 part 形状，只能按字段存在性判定。
func requestHasNonTextContent(clientProtocol protocol.Protocol, body []byte) bool {
	for _, marker := range multimodalMarkerWords {
		if bytes.Contains(body, marker) {
			switch clientProtocol {
			case protocol.Anthropic:
				if hasNonTextType(body, "messages.#.content.#.type", anthropicNonTextTypes) ||
					hasNonTextType(body, "messages.#.content.#.content.#.type", anthropicNonTextTypes) {
					return true
				}
			case protocol.OpenAI:
				if hasNonTextType(body, "messages.#.content.#.type", openaiNonTextTypes) ||
					hasNonTextType(body, "messages.#.content.type", openaiNonTextTypes) {
					return true
				}
			case protocol.Codex:
				if hasNonTextType(body, "input.#.content.#.type", codexNonTextTypes) ||
					hasNonTextType(body, "input.#.content.type", codexNonTextTypes) {
					return true
				}
			case protocol.Gemini:
				if hasGeminiNonTextPart(body) {
					return true
				}
			}
			// 该 marker 只是候选，某个协议分支未命中不代表整体是纯文本：body 里
			// 可能还包含其它 marker 词，得让外层循环继续匹配下一种 marker。
		}
	}
	return false
}

// forEachJSONValue 递归展开嵌套数组，对每个非数组值调用 visit；visit 返回 true
// 表示命中，立即停止全部遍历并返回 true，全部未命中返回 false。`#` 组装的路径
// （如 messages.#.content.#.type）会产生多层嵌套数组，gjson 的 @flatten 对这些
// 结构不压平，必须自递归。
func forEachJSONValue(result gjson.Result, depth int, visit func(gjson.Result) bool) bool {
	if depth > jsonWalkMaxDepth {
		return visit(result)
	}
	if !result.IsArray() {
		return visit(result)
	}
	found := false
	result.ForEach(func(_, child gjson.Result) bool {
		found = forEachJSONValue(child, depth+1, visit)
		return !found
	})
	return found
}

// hasNonTextType 沿 gjson 路径收集 type 值，命中目标集合立即停止遍历。
func hasNonTextType(body []byte, path string, tags map[string]bool) bool {
	return forEachJSONValue(gjson.GetBytes(body, path), 0, func(value gjson.Result) bool {
		return tags[value.String()]
	})
}

// hasGeminiNonTextPart 逐 part 检查四个字段名。Gemini 的 part 没有 type tag，
// 只能按字段存在性判定；对 `#` 聚合路径做 Exists() 会因 gjson 把空命中返回成
// `[]`/`[[]]`（JSON 数组）而恒真，所以必须落到单个 part 对象上逐个查。
func hasGeminiNonTextPart(body []byte) bool {
	return forEachJSONValue(gjson.GetBytes(body, "contents.#.parts"), 0, func(part gjson.Result) bool {
		for _, key := range geminiNonTextKeys {
			if part.Get(key).Exists() {
				return true
			}
		}
		return false
	})
}

func looksLikeOpenAIChatCompletionsBody(body []byte) bool {
	var root map[string]json.RawMessage
	if err := sonic.Unmarshal(body, &root); err != nil {
		return false
	}
	if _, ok := root["messages"]; !ok {
		return false
	}

	for _, key := range []string{
		"response_format",
		"stream_options",
		"prompt_cache_key",
		"parallel_tool_calls",
		"max_completion_tokens",
		"reasoning_effort",
		"frequency_penalty",
		"presence_penalty",
		"seed",
	} {
		if _, ok := root[key]; ok {
			return true
		}
	}

	if isOpenAITools(root["tools"]) || isOpenAIToolChoice(root["tool_choice"]) {
		return true
	}
	return hasOpenAIMessageOnlyFields(root["messages"])
}

func isOpenAITools(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var tools []map[string]json.RawMessage
	if err := sonic.Unmarshal(raw, &tools); err != nil {
		return false
	}
	for _, tool := range tools {
		if hasRawKey(tool, "function") || rawStringValue(tool["type"]) == "function" {
			return true
		}
	}
	return false
}

func isOpenAIToolChoice(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var choice string
	if err := sonic.Unmarshal(raw, &choice); err == nil {
		return choice == "none" || choice == "auto" || choice == "required"
	}
	var obj map[string]json.RawMessage
	if err := sonic.Unmarshal(raw, &obj); err != nil {
		return false
	}
	return rawStringValue(obj["type"]) == "function" || hasRawKey(obj, "function")
}

func hasOpenAIMessageOnlyFields(raw json.RawMessage) bool {
	var messages []map[string]json.RawMessage
	if err := sonic.Unmarshal(raw, &messages); err != nil {
		return false
	}
	for _, message := range messages {
		switch rawStringValue(message["role"]) {
		case "developer", "tool":
			return true
		}
		for _, key := range []string{"tool_calls", "tool_call_id", "reasoning_content"} {
			if hasRawKey(message, key) {
				return true
			}
		}
	}
	return false
}

func hasRawKey(m map[string]json.RawMessage, key string) bool {
	_, ok := m[key]
	return ok
}

func rawStringValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	_ = sonic.Unmarshal(raw, &value)
	return value
}
