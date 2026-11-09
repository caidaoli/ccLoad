package zaiauth

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"
)

/**
 * @brief ZCode 3.17.2 固定提示词；来源和许可见 ZCODE_LICENSE.txt。
 */
//go:embed zcode_prompt.json
var startPromptJSON []byte

/**
 * @brief 返回与 ZCode Node 客户端一致的平台名。
 * @return Node 平台与架构，例如 win32-x64。
 */
func ClientPlatform() string {
	osName, arch := runtime.GOOS, runtime.GOARCH
	if osName == "windows" {
		osName = "win32"
	}
	if arch == "amd64" {
		arch = "x64"
	}
	if arch == "386" {
		arch = "ia32"
	}
	return osName + "-" + arch
}

/**
 * @brief 将官方已知 GLM ID 恢复为 registry 拼写，未知模型保持原值。
 * @param name 余额桶提供的模型名称。
 * @return 实际上游模型名。
 */
func WireModel(name string) string {
	for _, id := range []string{"GLM-5.3", "GLM-5.3-Flash", "GLM-5.2", "GLM-5.2-HighSpeed", "GLM-5.1", "GLM-5-Turbo", "GLM-4.7", "GLM-4.7-Flash"} {
		if strings.EqualFold(name, id) {
			return id
		}
	}
	return name
}

/**
 * @brief 重建体验请求头，不携带付费 Key 或其他客户端的 beta 头。
 * @param jwt ZCode 登录凭证。
 * @param session 对话 ID，与 metadata.user_id 保持一致。
 * @return 独立的上游头集合。
 */
func StartHeaders(jwt, session string) http.Header {
	h := make(http.Header)
	ApplySourceHeaders(h)
	h.Set("Authorization", "Bearer "+jwt)
	h.Set("Content-Type", "application/json")
	h.Set("anthropic-version", "2023-06-01")
	h.Set("User-Agent", "ZCode/"+AppVersion+" ai-sdk/anthropic/3.0.81")
	h.Set("X-Release-Channel", "production")
	h.Set("X-Client-Language", "en-US")
	h.Set("X-Client-Timezone", clientTimezone())
	h.Set("X-Platform", ClientPlatform())
	h.Set("X-Os-Version", hostRelease())
	h.Set("x-session-id", session)
	h.Set("x-zcode-session-type", "main")
	h.Set("x-zcode-query-source", "main_turn")
	return h
}

/**
 * @brief 按 ZCode Electron 的会话和真实用户轮次稳定派生体验查询 ID。
 * @param raw 转换后的 Anthropic JSON；工具结果续轮不增加用户轮次。
 * @param session 对话 ID。
 * @return 与 Magpie 相同的 SHA-256 派生 UUIDv4；没有真实用户输入时为空。
 */
func StartQueryID(raw []byte, session string) string {
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		return ""
	}
	turn := 0
	for _, message := range body.Messages {
		if message.Role != "user" || !zcodeRealUserContent(message.Content) {
			continue
		}
		turn++
	}
	if turn == 0 {
		return ""
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("zcode-query\x00%s\x00%d", session, turn)))
	// ZCode takes the first 128 digest bits, then stamps RFC 4122 version/variant bits.
	digest[6] = digest[6]&0x0f | 0x40
	digest[8] = digest[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", digest[0:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
}

/**
 * @brief 判断 user 内容是否包含非 tool_result 块，语义与 ZCode 的 JavaScript 判定一致。
 * @param content Anthropic message.content。
 * @return 文本非空或数组中存在真实用户块时为 true。
 */
func zcodeRealUserContent(content any) bool {
	switch value := content.(type) {
	case string:
		return strings.TrimSpace(value) != ""
	case []any:
		for _, block := range value {
			if !zcodeTruthy(block) {
				continue
			}
			if object, ok := block.(map[string]any); ok && object["type"] == "tool_result" {
				continue
			}
			return true
		}
	}
	return false
}

/**
 * @brief 复刻 JSON 值在 ZCode JavaScript 数组 some 回调中的真值语义。
 * @param value 已解析的 JSON 值。
 * @return null、false、空字符串和数值零返回 false；对象和数组返回 true。
 */
func zcodeTruthy(value any) bool {
	switch value := value.(type) {
	case nil:
		return false
	case bool:
		return value
	case string:
		return value != ""
	case json.Number:
		number, err := value.Float64()
		return err != nil || number != 0
	case float64:
		return value != 0
	default:
		return true
	}
}

/**
 * @brief 按固定版本构造体验 Messages，保留调用者 system 与历史消息。
 * @param raw 转换后的 Anthropic JSON。
 * @param model 实际上游模型 ID。
 * @param device 账号设备指纹。
 * @param session 本次对话 ID。
 * @param site 已验证的账号站点，用于官方提示词的 provider。
 * @return 独立的体验请求体，不修改付费请求。
 * @return JSON、消息或固定资源无效时返回错误。
 */
func PrepareStartBody(raw []byte, model, device, session string, site Site) ([]byte, error) {
	var p struct {
		CLIPrefix         string            `json:"cliPrefix"`
		StableSections    []string          `json:"stableSections"`
		DynamicBehavior   string            `json:"dynamicBehavior"`
		SessionGuidance   string            `json:"sessionGuidance"`
		ContextManagement string            `json:"contextManagement"`
		PoweredBy         string            `json:"poweredBy"`
		Environment       map[string]string `json:"environment"`
		Context           map[string]string `json:"context"`
	}
	if err := json.Unmarshal(startPromptJSON, &p); err != nil {
		return nil, fmt.Errorf("decode ZCode prompt: %w", err)
	}
	var body map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		return nil, err
	}
	messages, ok := body["messages"].([]any)
	if !ok {
		return nil, fmt.Errorf("ZCode Start Plan requires messages")
	}
	cached := func(text string) map[string]any {
		return map[string]any{"type": "text", "text": text, "cache_control": map[string]string{"type": "ephemeral"}}
	}
	dynamic := []string{p.DynamicBehavior}
	tools, _ := body["tools"].([]any)
	for _, value := range tools {
		if tool, ok := value.(map[string]any); ok {
			delete(tool, "cache_control")
			if tool["name"] == "Skill" {
				dynamic = append(dynamic, p.SessionGuidance)
			}
		}
	}
	e := p.Environment
	platform, _, _ := strings.Cut(ClientPlatform(), "-")
	environment := e["heading"] + "\n" + e["invokedLine"] + "\n - " + e["platformLabel"] + ": " + platform + "\n - " + e["osVersionLabel"] + ": " + hostRelease() + "\n - " + e["untrustedNote"]
	dynamic = append(dynamic, environment, strings.NewReplacer("{provider}", site.config().prompt, "{model}", model).Replace(p.PoweredBy), p.ContextManagement)
	switch own := body["system"].(type) {
	case string:
		if strings.TrimSpace(own) != "" {
			dynamic = append(dynamic, own)
		}
	case []any:
		for _, value := range own {
			if block, ok := value.(map[string]any); ok && block["type"] == "text" {
				if text, ok := block["text"].(string); ok && strings.TrimSpace(text) != "" {
					dynamic = append(dynamic, text)
				}
			}
		}
	}
	body["system"] = []any{cached(p.CLIPrefix), cached(strings.Join(p.StableSections, "\n\n")), cached("\n\n" + strings.Join(dynamic, "\n\n"))}
	date := strings.ReplaceAll(p.Context["currentDateLine"], "{date}", time.Now().Format("2006-01-02"))
	messages = append([]any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "<system-reminder>" + date + "</system-reminder>"}}}}, messages...)
	var last map[string]any
	for _, value := range messages {
		message, ok := value.(map[string]any)
		if !ok || message["role"] == "system" {
			continue
		}
		last = message
		if content, ok := message["content"].([]any); ok {
			for _, value := range content {
				if block, ok := value.(map[string]any); ok {
					delete(block, "cache_control")
				}
			}
		}
	}
	if last != nil {
		switch content := last["content"].(type) {
		case string:
			last["content"] = []any{cached(content)}
		case []any:
			if len(content) > 0 {
				if block, ok := content[len(content)-1].(map[string]any); ok {
					block["cache_control"] = map[string]string{"type": "ephemeral"}
				}
			}
		}
	}
	body["messages"], body["model"] = messages, model
	identity, err := json.Marshal(struct {
		Device  string `json:"device_id"`
		Account string `json:"account_uuid"`
		Session string `json:"session_id"`
	}{Device: device, Session: session})
	if err != nil {
		return nil, err
	}
	metadata, _ := body["metadata"].(map[string]any)
	if metadata == nil {
		metadata = make(map[string]any)
	}
	metadata["user_id"] = string(identity)
	body["metadata"] = metadata
	return json.Marshal(body)
}
