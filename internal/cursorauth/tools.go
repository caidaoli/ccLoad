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

/**
 * @brief 客户端交给暂停中的 Cursor 原生工具回调的执行结果。
 */
type ToolResult struct {
	CallID  string ///< 下游工具调用 ID。
	Output  string ///< 文本或 JSON 输出；含图片的内容通过 Content 保存。
	Content []any  `json:",omitempty"` ///< 含图片的客户端内容块，按原顺序保存。
	IsError bool   ///< 客户端是否报告工具执行失败。
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
 * @brief Cursor Agent 的归属范围，包含网关认证身份和客户端声明的会话标识。
 */
type SessionScope struct {
	ChannelID       int64  ///< 本次选中的持久化渠道 ID。
	TokenID         int64  ///< 通过 ccLoad 认证的调用方令牌记录 ID。
	SessionID       string ///< 客户端声明的 Session-Id。
	ThreadID        string ///< 客户端声明的 Thread-Id。
	ClaudeSessionID string ///< Messages 客户端声明的 Claude Code 会话。
	ClaudeAgentID   string ///< Claude Code 子代理 ID，主代理为空。
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
	Model             string ///< 实际路由模型；仅交付工具结果时可留空以沿用原 Run 的模型。
	Prompt            string
	Images            []Image ///< Prompt 中 `[image]` 占位按序对应的图片
	Effort            string  ///< 客户端请求体声明的思考等级（小写）；模型名后缀优先于该值
	Tools             []Tool  ///< nil 表示省略声明；非 nil 的空列表表示禁用工具。
	ToolChoice        string
	ParallelToolCalls *bool ///< nil 表示续轮继承 Run 的设置，新轮默认允许并行。
	ToolResults       []ToolResult
	SessionScope      SessionScope `json:"-"` ///< 仅由网关设置；缺少渠道或令牌时禁用跨请求恢复。
	OnDetachedUsage   func(Usage)  `json:"-"` ///< 工具会话迟到用量的接收方；nil 表示无接收方。

	transcript         []transcriptEntry ///< 组成 Prompt 的逐条消息，用于普通续轮匹配已停放 Agent
	instructionsDigest string            ///< 当前 system/developer 消息的内容摘要；无有效内容时为空。
	instructionsSet    bool              ///< 区分省略指令与显式清空指令。
	hasToolSteering    bool              ///< 在尾部工具结果处或其后还有额外的用户内容。
	toolChoiceNamed    bool              ///< 选定了工具名，保留与 none、auto 等控制词同名的工具。
	allowedToolNames   []string          ///< allowed_tools 指定的函数名；nil 表示未提供有效列表。
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

/**
 * @brief 判断本轮请求是否显式开放客户端工具。
 * @return 声明了工具且未禁用时为 true。
 */
func (r Request) AllowsTools() bool {
	if !r.toolChoiceNamed {
		switch r.ToolChoice {
		case "none":
			return false
		case "allowed_tools":
			return len(r.Tools) > 0 && len(r.allowedToolNames) > 0
		}
	}
	return len(r.Tools) > 0
}

/**
 * @brief 决定本轮是否只向客户端公开一个工具调用。
 * @param inherited 原 Run 的串行设置，新建 Run 默认为 false。
 * @return 实际串行设置，显式参数优先。
 */
func (r Request) serialTools(inherited bool) bool {
	if r.ParallelToolCalls == nil {
		return inherited
	}
	return !*r.ParallelToolCalls
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
		Model: strings.TrimSpace(asString(raw["model"])),
		Tools: parseClientTools(raw),
	}
	request.ToolChoice, request.toolChoiceNamed = parseToolChoice(raw["tool_choice"], raw["function_call"])
	if parallel, ok := raw["parallel_tool_calls"].(bool); ok {
		request.ParallelToolCalls = &parallel
	} else if choice, ok := raw["tool_choice"].(map[string]any); ok {
		if disabled, ok := choice["disable_parallel_tool_use"].(bool); ok {
			parallel := !disabled
			request.ParallelToolCalls = &parallel
		}
	}
	if request.ToolChoice == "allowed_tools" && !request.toolChoiceNamed {
		choice, _ := raw["tool_choice"].(map[string]any)
		request.allowedToolNames = parseAllowedToolNames(choice)
	}
	messages := requestMessages(raw)
	request.ToolResults, request.hasToolSteering = parseToolResults(messages)
	request.Effort = requestEffort(raw)
	var instructions []transcriptEntry
	var system contentWriter
	if rawSystem, provided := raw["system"]; provided && rawSystem != nil {
		request.instructionsSet = true
		if text := system.content(rawSystem, false); text != "" {
			entry := transcriptEntry{role: "system", body: text, text: text, images: system.images}
			instructions = append(instructions, entry)
			request.transcript = append(request.transcript, entry)
		}
	}
	for _, rawMessage := range messages {
		message, _ := rawMessage.(map[string]any)
		if message == nil {
			continue
		}
		entry, ok := parseTranscriptEntry(message, false)
		if ok {
			request.transcript = append(request.transcript, entry)
		}
		switch entry.role {
		case "system", "developer":
			request.instructionsSet = true
			if ok {
				instructions = append(instructions, entry)
			}
		}
	}
	request.instructionsDigest = transcriptDigest(instructions)
	request.Prompt, request.Images = renderTranscript(request.transcript)
	return request
}

/**
 * @brief 读取用于 Cursor 续轮选路的工具结果，按推理入口相同的尾部规则提取。
 * @param body Anthropic Messages 或 OpenAI Chat 形态的 JSON 请求体。
 * @return 尾部工具结果；非法请求或没有结果时为 nil。
 */
func ParseRequestToolResults(body []byte) []ToolResult {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return nil
	}
	results, _ := parseToolResults(requestMessages(raw))
	return results
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

/**
 * @brief 解析工具选择策略，保留具名函数与控制词的区别。
 * @param choice 当前协议的 tool_choice 字段。
 * @param legacy 旧版 OpenAI function_call 字段。
 * @return 工具名或控制词；省略时为 auto。
 * @return 已选择区分大小写的工具名时为 true。
 */
func parseToolChoice(choice any, legacy any) (string, bool) {
	switch typed := choice.(type) {
	case string:
		value := strings.TrimSpace(typed)
		switch keyword := strings.ToLower(value); keyword {
		case "none", "auto", "required", "any":
			if keyword == "any" {
				return "required", false
			}
			return keyword, false
		default:
			if value != "" {
				return value, true
			}
		}
	case map[string]any:
		kind := strings.ToLower(asString(typed["type"]))
		switch kind {
		case "none", "auto", "allowed_tools":
			return kind, false
		case "any", "required":
			return "required", false
		case "", "function", "tool", "custom":
		default:
			return "", true
		}
		if name := strings.TrimSpace(asString(typed["name"])); name != "" {
			return name, true
		}
		if function, _ := typed["function"].(map[string]any); function != nil {
			if name := strings.TrimSpace(asString(function["name"])); name != "" {
				return name, true
			}
		}
		return "", true
	}
	if legacy != nil {
		switch typed := legacy.(type) {
		case string:
			value := strings.ToLower(strings.TrimSpace(typed))
			if value == "none" {
				return "none", false
			}
		case map[string]any:
			return strings.TrimSpace(asString(typed["name"])), true
		}
	}
	return "auto", false
}

/**
 * @brief 读取 Chat 或 Responses 允许调用的自定义函数列表。
 * @param choice 已解码的 allowed_tools 选择对象。
 * @return 允许的函数名；列表或模式无效、包含非自定义函数时为 nil。
 */
func parseAllowedToolNames(choice map[string]any) []string {
	if nested, ok := choice["allowed_tools"].(map[string]any); ok {
		choice = nested
	}
	switch asString(choice["mode"]) {
	case "auto", "required":
	default:
		return nil
	}
	items, ok := choice["tools"].([]any)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(items))
	for _, item := range items {
		object, _ := item.(map[string]any)
		if asString(object["type"]) != "function" || asString(object["namespace"]) != "" {
			return nil
		}
		if nested, ok := object["function"].(map[string]any); ok {
			object = nested
		}
		name := strings.TrimSpace(asString(object["name"]))
		if name == "" || asString(object["namespace"]) != "" {
			return nil
		}
		names = append(names, name)
	}
	return names
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
		return transcriptEntry{role: role}, false
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
 * @details system 和 developer 消息不会切断同一工具批次。
 */
func parseToolResults(messages []any) ([]ToolResult, bool) {
	var trailing []ToolResult
	firstResult := len(messages)
	for index, message := range slices.Backward(messages) {
		message, _ := message.(map[string]any)
		batch := toolResultsFromMessage(message)
		if len(batch) == 0 {
			switch strings.ToLower(strings.TrimSpace(asString(message["role"]))) {
			case "system", "developer", "user":
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
		id := firstNonEmpty(asString(message["tool_call_id"]), asString(message["call_id"]), asString(message["id"]))
		results = append(results, parseToolResult(id, firstNonNil(message["content"], message["output"]), asBool(message["is_error"])))
	}
	content, _ := message["content"].([]any)
	for _, rawBlock := range content {
		block, _ := rawBlock.(map[string]any)
		if block == nil || !strings.EqualFold(asString(block["type"]), "tool_result") {
			continue
		}
		id := firstNonEmpty(asString(block["tool_use_id"]), asString(block["call_id"]))
		results = append(results, parseToolResult(id, block["content"], asBool(block["is_error"])))
	}
	return results
}

/**
 * @brief 提取工具输出，保留实际包含图片的结构化内容块。
 * @param id 客户端工具调用 ID。
 * @param output 已解码的工具输出。
 * @param isError 客户端是否报告工具执行失败。
 * @return 含图片时保留全部内容块，其余输出沿用文本或 JSON 表示。
 */
func parseToolResult(id string, output any, isError bool) ToolResult {
	result := ToolResult{CallID: id, IsError: isError}
	if blocks, ok := output.([]any); ok && slices.ContainsFunc(blocks, func(value any) bool {
		block, _ := value.(map[string]any)
		_, isImage := imageFromContentBlock(block)
		return isImage
	}) {
		result.Content = blocks
	} else {
		result.Output = stringifyToolOutput(output)
	}
	return result
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
	case "image", "image_url", "input_image":
		image, _ := imageFromContentBlock(block)
		return w.image(image), false
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
 * @brief 解析客户端内容块中的内联图片或远程图片地址。
 * @param block Anthropic 或 OpenAI 内容块。
 * @return 图片源，没有有效字段时为零值。
 * @return 内容块声明了图片时为 true。
 */
func imageFromContentBlock(block map[string]any) (Image, bool) {
	switch strings.ToLower(asString(block["type"])) {
	case "image":
		source, _ := block["source"].(map[string]any)
		return Image{Data: asString(source["data"]), MimeType: asString(source["media_type"]), URL: asString(source["url"])}, true
	case "image_url", "input_image":
		if object, ok := block["image_url"].(map[string]any); ok {
			return imageFromURL(asString(object["url"])), true
		}
		return imageFromURL(asString(block["image_url"])), true
	default:
		return Image{}, false
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
