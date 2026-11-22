package cursorauth

import (
	"encoding/json"
	"slices"
	"strings"
)

// Tool is one client-advertised function (Anthropic tools[] or OpenAI tools[]).
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolCall is one model-requested client tool invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// ToolResult is one client-executed result for a suspended native Cursor
// custom-tool callback.
type ToolResult struct {
	CallID  string
	Output  string
	IsError bool
}

/**
 * @brief 随提示词转发给 SDK 的一张客户端图片。
 */
type Image struct {
	Data     string ///< base64 图片字节，不含 data URL 前缀；远程图片为空
	MimeType string ///< 内联图片的 MIME 类型
	URL      string ///< 远程图片地址；内联图片为空
}

/**
 * @brief 可跨请求恢复 Cursor Agent 的网关身份，由可信路由与认证结果构成。
 */
type SessionScope struct {
	ChannelID int64 ///< 本次选中的持久化渠道 ID。
	TokenID   int64 ///< 通过 ccLoad 认证的调用方令牌记录 ID。
}

/**
 * @brief 判断请求是否有可用于跨请求恢复的身份。
 * @return 渠道与调用方均可识别时为 true。
 */
func (s SessionScope) reusable() bool {
	return s.ChannelID > 0 && s.TokenID > 0
}

/**
 * @brief Cursor SDK 接收的一轮客户端请求。
 * @details 工具 Run 在无附着请求时结束，才同步调用最后一次成功附着提供的 OnDetachedUsage，至多一次。
 * 回调只接收上游已报告且未交给客户端请求的增量，须独立于 HTTP 请求及其取消生命周期。
 */
type Request struct {
	Model           string ///< 实际路由模型；仅交付工具结果时可留空以沿用原 Run 的模型。
	Prompt          string
	Images          []Image ///< Prompt 中 `[image]` 占位按序对应的图片
	Effort          string  ///< 客户端请求体声明的思考等级（小写）；模型名后缀优先于该值
	Tools           []Tool
	ToolChoice      string
	ToolResults     []ToolResult
	SessionScope    SessionScope `json:"-"` ///< 仅由网关设置；缺少渠道或令牌时禁用跨请求恢复。
	OnDetachedUsage func(Usage)  `json:"-"` ///< 工具会话迟到用量的接收方；nil 表示无接收方。

	transcript      []transcriptEntry ///< 组成 Prompt 的逐条消息，用于普通续轮匹配已停放 Agent
	hasToolSteering bool              ///< 在尾部工具结果处或其后还有额外的用户内容。
}

/**
 * @brief 展平提示词中的一条消息。
 */
type transcriptEntry struct {
	role           string  ///< 小写角色；顶层 system 为 system
	body           string  ///< 不含角色标签的消息文本
	text           string  ///< 写入 Prompt 的带角色标签文本
	images         []Image ///< 本条消息中的图片，顺序与 body 中的占位一致
	toolResult     bool    ///< 是否携带工具结果
	plainAssistant bool    ///< 可与 SDK 回复正文直接匹配的纯文本 assistant 消息
}

// AllowsTools reports whether this turn exposes native custom tools.
func (r Request) AllowsTools() bool {
	return r.ToolChoice != "none" && len(r.Tools) > 0
}

/**
 * @brief 将 Anthropic Messages 或 OpenAI 请求规范化为 Cursor SDK 请求。
 * @param body 客户端 JSON 请求体。
 * @return 完整提示词和结构化工具数据；非法 JSON 返回零值。
 */
func ParseRequest(body []byte) Request {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return Request{}
	}
	request := Request{
		Model:      strings.TrimSpace(asString(raw["model"])),
		Tools:      parseClientTools(raw),
		ToolChoice: parseToolChoice(raw["tool_choice"], raw["function_call"]),
	}
	messages := requestMessages(raw)
	request.ToolResults, request.hasToolSteering = parseToolResults(messages)
	request.Effort = requestEffort(raw)
	var system contentWriter
	if text := system.content(raw["system"], false); text != "" {
		request.transcript = append(request.transcript, transcriptEntry{
			role: "system", body: text, text: text, images: system.images,
		})
	}
	for _, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		if message == nil {
			continue
		}
		if entry, ok := parseTranscriptEntry(message, false); ok {
			request.transcript = append(request.transcript, entry)
		}
	}
	request.Prompt, request.Images = renderTranscript(request.transcript)
	return request
}

/**
 * @brief 读取请求体中显式声明的思考等级。
 * @param raw 已解码的 Anthropic Messages 或 OpenAI chat 请求体。
 * @return 小写思考等级；未声明时为空。
 * @details OpenAI chat 使用 `reasoning_effort`。Anthropic 显式关闭思考映射为 none，否则读取 `output_config.effort`；预算型思考没有对应等级。
 */
func requestEffort(raw map[string]any) string {
	effort := asString(raw["reasoning_effort"])
	if effort == "" {
		thinking, _ := raw["thinking"].(map[string]any)
		if asString(thinking["type"]) == "disabled" {
			return "none"
		}
		config, _ := raw["output_config"].(map[string]any)
		effort = asString(config["effort"])
	}
	return strings.ToLower(strings.TrimSpace(effort))
}

/**
 * @brief 把逐条消息拼成 SDK 提示词并收集其中的图片。
 * @param entries 按对话顺序排列的消息。
 * @return 以空行分隔的提示词。
 * @return 提示词中 `[image]` 占位按序对应的图片；无图片时为 nil。
 */
func renderTranscript(entries []transcriptEntry) (string, []Image) {
	parts := make([]string, 0, len(entries))
	var images []Image
	for _, entry := range entries {
		parts = append(parts, entry.text)
		images = append(images, entry.images...)
	}
	return strings.Join(parts, "\n\n"), images
}

func requestMessages(raw map[string]any) []any {
	if messages, _ := raw["messages"].([]any); len(messages) > 0 {
		return messages
	}
	if input, _ := raw["input"].([]any); len(input) > 0 {
		return input
	}
	return nil
}

/**
 * @brief 规范化客户端的工具声明。
 * @param raw 已解码的请求对象。
 * @return 工具声明；未声明 tools/functions 时为 nil，显式空列表保持非 nil。
 */
func parseClientTools(raw map[string]any) []Tool {
	seen := make(map[string]struct{})
	var tools []Tool
	_, hasTools := raw["tools"]
	_, hasFunctions := raw["functions"]
	if hasTools || hasFunctions {
		tools = []Tool{}
	}
	add := func(tool Tool) {
		tool.Name = strings.TrimSpace(tool.Name)
		if tool.Name == "" {
			return
		}
		if _, exists := seen[tool.Name]; exists {
			return
		}
		seen[tool.Name] = struct{}{}
		if len(bytesTrimSpace(tool.Parameters)) == 0 {
			tool.Parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools = append(tools, tool)
	}
	if entries, _ := raw["tools"].([]any); len(entries) > 0 {
		for _, entry := range entries {
			object, _ := entry.(map[string]any)
			if object == nil {
				continue
			}
			if strings.EqualFold(asString(object["type"]), "function") {
				function, _ := object["function"].(map[string]any)
				if function == nil {
					continue
				}
				add(Tool{
					Name:        asString(function["name"]),
					Description: asString(function["description"]),
					Parameters:  rawJSON(function["parameters"]),
				})
				continue
			}
			schema := rawJSON(object["input_schema"])
			if len(schema) == 0 {
				schema = rawJSON(object["parameters"])
			}
			add(Tool{
				Name:        asString(object["name"]),
				Description: asString(object["description"]),
				Parameters:  schema,
			})
		}
	}
	if entries, _ := raw["functions"].([]any); len(entries) > 0 {
		for _, entry := range entries {
			object, _ := entry.(map[string]any)
			if object == nil {
				continue
			}
			add(Tool{
				Name:        asString(object["name"]),
				Description: asString(object["description"]),
				Parameters:  rawJSON(object["parameters"]),
			})
		}
	}
	return tools
}

func parseToolChoice(choice any, legacy any) string {
	switch typed := choice.(type) {
	case string:
		value := strings.ToLower(strings.TrimSpace(typed))
		switch value {
		case "none", "auto", "required", "any":
			if value == "any" {
				return "required"
			}
			return value
		default:
			if value != "" {
				return value
			}
		}
	case map[string]any:
		if name := strings.TrimSpace(asString(typed["name"])); name != "" {
			return name
		}
		if function, _ := typed["function"].(map[string]any); function != nil {
			if name := strings.TrimSpace(asString(function["name"])); name != "" {
				return name
			}
		}
		switch strings.ToLower(asString(typed["type"])) {
		case "none":
			return "none"
		case "auto":
			return "auto"
		case "any", "required":
			return "required"
		}
	}
	if legacy != nil {
		switch typed := legacy.(type) {
		case string:
			value := strings.ToLower(strings.TrimSpace(typed))
			if value == "none" {
				return "none"
			}
		case map[string]any:
			if name := strings.TrimSpace(asString(typed["name"])); name != "" {
				return name
			}
		}
	}
	return "auto"
}

/**
 * @brief 把一条客户端消息展平为提示词条目。
 * @param message Anthropic Messages 或 OpenAI chat 消息对象。
 * @param skipToolResults 提取与工具结果一并提交的额外用户内容时忽略工具结果块。
 * @return 展平后的条目。
 * @return 消息没有可发送的文本或图片时为 false。
 * @details tool/function 角色只输出一次工具结果行，内容不再重复作为正文。
 * assistant 文本保留原始空白，供普通续轮匹配和新建对话时使用。
 */
func parseTranscriptEntry(message map[string]any, skipToolResults bool) (transcriptEntry, bool) {
	role := strings.ToLower(strings.TrimSpace(asString(message["role"])))
	if strings.EqualFold(asString(message["type"]), "function_call_output") {
		role = "tool"
	}
	assistant := role == "assistant"
	writer := contentWriter{skipToolResults: skipToolResults}
	var chunks []string
	if role == "tool" || role == "function" {
		writer.toolResult = true
		id := firstNonEmpty(asString(message["tool_call_id"]), asString(message["call_id"]), asString(message["id"]))
		chunks = append(chunks, formatToolResultLine(id, asString(message["name"]), writer.content(firstNonNil(message["content"], message["output"]), false), asBool(message["is_error"])))
	} else if text := writer.content(message["content"], assistant); text != "" {
		chunks = append(chunks, text)
	} else if text := writer.content(message["text"], assistant); text != "" {
		chunks = append(chunks, text)
	}
	if calls, _ := message["tool_calls"].([]any); len(calls) > 0 {
		for _, call := range calls {
			object, _ := call.(map[string]any)
			if object == nil {
				continue
			}
			writer.hasNonText = true
			function, _ := object["function"].(map[string]any)
			name := asString(object["name"])
			id := asString(object["id"])
			args := rawJSON(object["arguments"])
			if function != nil {
				if name == "" {
					name = asString(function["name"])
				}
				if len(args) == 0 {
					args = rawJSON(function["arguments"])
				}
			}
			chunks = append(chunks, formatToolUseLine(id, name, args))
		}
	}
	body := strings.Join(chunks, "\n")
	if !assistant {
		body = strings.TrimSpace(body)
	}
	if body == "" {
		return transcriptEntry{}, false
	}
	text := body
	if role != "" {
		text = role + ": " + body
	}
	return transcriptEntry{
		role: role, body: body, text: text, images: writer.images, toolResult: writer.toolResult,
		plainAssistant: assistant && !writer.hasNonText,
	}, true
}

/**
 * @brief 提取当前工具批次及其携带的额外用户内容。
 * @param messages 按对话顺序排列的客户端消息。
 * @return 去重后的尾部工具结果；上一轮 assistant 之前的调用不恢复。
 * @return 从首条结果起还有额外用户内容时为 true。
 * @details system 消息是续接元数据，可以夹在结果和用户消息之间。
 */
func parseToolResults(messages []any) ([]ToolResult, bool) {
	var trailing []ToolResult
	firstResult := len(messages)
	for index, message := range slices.Backward(messages) {
		message, _ := message.(map[string]any)
		batch := toolResultsFromMessage(message)
		if len(batch) == 0 {
			switch strings.ToLower(strings.TrimSpace(asString(message["role"]))) {
			case "system", "user":
				continue
			}
			break
		}
		if slices.ContainsFunc(batch, func(result ToolResult) bool { return result.CallID != "" }) {
			firstResult = index
		}
		trailing = append(batch, trailing...)
	}

	seen := make(map[string]struct{})
	var results []ToolResult
	for _, result := range trailing {
		result.CallID = strings.TrimSpace(result.CallID)
		if result.CallID == "" {
			continue
		}
		if _, exists := seen[result.CallID]; exists {
			continue
		}
		seen[result.CallID] = struct{}{}
		results = append(results, result)
	}
	for _, raw := range messages[firstResult:] {
		message, _ := raw.(map[string]any)
		if strings.EqualFold(strings.TrimSpace(asString(message["role"])), "user") {
			if _, ok := parseTranscriptEntry(message, true); ok {
				return results, true
			}
		}
	}
	return results, false
}

func toolResultsFromMessage(message map[string]any) []ToolResult {
	if message == nil {
		return nil
	}
	var results []ToolResult
	role := strings.ToLower(strings.TrimSpace(asString(message["role"])))
	typeName := strings.ToLower(strings.TrimSpace(asString(message["type"])))
	if role == "tool" || role == "function" || typeName == "function_call_output" {
		results = append(results, ToolResult{
			CallID:  firstNonEmpty(asString(message["tool_call_id"]), asString(message["call_id"]), asString(message["id"])),
			Output:  stringifyToolOutput(firstNonNil(message["content"], message["output"])),
			IsError: asBool(message["is_error"]),
		})
	}
	content, _ := message["content"].([]any)
	for _, rawBlock := range content {
		block, _ := rawBlock.(map[string]any)
		if block == nil || !strings.EqualFold(asString(block["type"]), "tool_result") {
			continue
		}
		results = append(results, ToolResult{
			CallID:  firstNonEmpty(asString(block["tool_use_id"]), asString(block["call_id"])),
			Output:  stringifyToolOutput(block["content"]),
			IsError: asBool(block["is_error"]),
		})
	}
	return results
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func asBool(value any) bool {
	result, _ := value.(bool)
	return result
}

func stringifyToolOutput(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if value == nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return stringifyContent(value)
	}
	return string(encoded)
}

func stringifyContent(value any) string {
	var writer contentWriter
	return writer.content(value, false)
}

/**
 * @brief 展平消息内容并按出现顺序收集图片。
 */
type contentWriter struct {
	images          []Image ///< 已替换为 `[image]` 占位的图片
	toolResult      bool    ///< 内容中出现过工具结果块
	skipToolResults bool    ///< 只提取用户内容，不展开工具结果块
	hasNonText      bool    ///< 内容包含已写出的图片或工具记录
}

/**
 * @brief 将内容展平为提示词文本。
 * @param value 已解码的消息内容。
 * @param preserveText 保留助手文本的全部空白，并直接连接相邻文本块。
 * @return 消息正文；没有可发送内容时为空。
 */
func (w *contentWriter) content(value any, preserveText bool) string {
	blocks, ok := value.([]any)
	if !ok {
		text := asString(value)
		if preserveText {
			return text
		}
		return strings.TrimSpace(text)
	}
	var body strings.Builder
	var previousText bool
	for _, item := range blocks {
		text, isText := asString(item), true
		if block, ok := item.(map[string]any); ok {
			text, isText = w.block(block)
		}
		if isText && !preserveText {
			text = strings.TrimSpace(text)
		}
		if text == "" {
			continue
		}
		if body.Len() > 0 && (!preserveText || !previousText || !isText) {
			body.WriteByte('\n')
		}
		body.WriteString(text)
		w.hasNonText = w.hasNonText || !isText
		previousText = isText
	}
	if preserveText {
		return body.String()
	}
	return strings.TrimSpace(body.String())
}

/**
 * @brief 展平客户端内容块，保留图片位置和工具调用身份。
 * @param block Anthropic 或 OpenAI 消息内容块。
 * @return 该块的文本或图片占位；无可传内容时为空。
 * @return 纯文本块为 true；图片或工具记录为 false。
 */
func (w *contentWriter) block(block map[string]any) (string, bool) {
	switch strings.ToLower(asString(block["type"])) {
	case "image":
		source, _ := block["source"].(map[string]any)
		return w.image(Image{
			Data: asString(source["data"]), MimeType: asString(source["media_type"]), URL: asString(source["url"]),
		}), false
	case "image_url":
		url := asString(block["image_url"])
		if object, ok := block["image_url"].(map[string]any); ok {
			url = asString(object["url"])
		}
		return w.image(imageFromURL(url)), false
	case "tool_use":
		return formatToolUseLine(asString(block["id"]), asString(block["name"]), rawJSON(block["input"])), false
	case "tool_result":
		w.toolResult = true
		if w.skipToolResults {
			return "", false
		}
		return formatToolResultLine(asString(block["tool_use_id"]), "", w.content(block["content"], false), asBool(block["is_error"])), false
	case "function_call":
		return formatToolUseLine(asString(block["id"]), asString(block["name"]), rawJSON(block["arguments"])), false
	default:
		return asString(block["text"]), true
	}
}

/**
 * @brief 记录一张图片并返回其在提示词中的占位。
 * @param image 客户端图片；内联数据与地址都为空时忽略。
 * @return `[image]` 占位；图片无效时为空。
 */
func (w *contentWriter) image(image Image) string {
	if image.Data == "" {
		image.MimeType = ""
	} else {
		image.URL = ""
	}
	if image.Data == "" && image.URL == "" {
		return ""
	}
	w.images = append(w.images, image)
	return "[image]"
}

/**
 * @brief 解析 OpenAI `image_url` 地址。
 * @param url base64 data URL 或远程地址。
 * @return data URL 拆成内联数据与 MIME 类型，其余地址原样作为远程图片。
 */
func imageFromURL(url string) Image {
	url = strings.TrimSpace(url)
	header, data, ok := strings.Cut(url, ",")
	if !ok || !strings.HasPrefix(header, "data:") || !strings.HasSuffix(header, ";base64") {
		return Image{URL: url}
	}
	return Image{Data: data, MimeType: strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")}
}

func formatToolUseLine(id, name string, args json.RawMessage) string {
	line := "[tool_use"
	if id != "" {
		line += " id=" + id
	}
	if name != "" {
		line += " name=" + name
	}
	line += "]"
	if len(bytesTrimSpace(args)) > 0 {
		line += " " + string(args)
	}
	return line
}

/**
 * @brief 将历史工具结果写入 SDK 提示词，保留客户端报告的失败状态。
 * @param id 工具调用标识。
 * @param name 客户端提供的工具名，可为空。
 * @param content 工具输出。
 * @param isError 此工具调用是否执行失败。
 * @return 可用于新建或恢复对话的工具结果行。
 */
func formatToolResultLine(id, name, content string, isError bool) string {
	line := "[tool_result"
	if id != "" {
		line += " tool_use_id=" + id
	}
	if name != "" {
		line += " name=" + name
	}
	if isError {
		line += " is_error=true"
	}
	line += "]"
	if content != "" {
		line += " " + content
	}
	return line
}

func rawJSON(value any) json.RawMessage {
	switch typed := value.(type) {
	case nil:
		return nil
	case json.RawMessage:
		return bytesTrimSpace(typed)
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []byte(typed)
	default:
		raw, err := json.Marshal(typed)
		if err != nil {
			return nil
		}
		return raw
	}
}

func bytesTrimSpace(raw json.RawMessage) json.RawMessage {
	return json.RawMessage(strings.TrimSpace(string(raw)))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
