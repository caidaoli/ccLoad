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

// Request is one client turn normalized for the Cursor SDK runner.
type Request struct {
	Model       string
	Prompt      string
	Images      []Image ///< Prompt 中 `[image]` 占位按序对应的图片
	Effort      string  ///< 客户端请求体声明的思考等级（小写）；模型名后缀优先于该值
	Tools       []Tool
	ToolChoice  string
	ToolResults []ToolResult
	// InputTokenEstimate is used for client context accounting while a native
	// Cursor run is suspended at a tool callback. It is never billable usage.
	InputTokenEstimate int

	transcript []transcriptEntry ///< 组成 Prompt 的逐条消息，用于普通续轮匹配已停放 Agent
}

/**
 * @brief 展平提示词中的一条消息。
 */
type transcriptEntry struct {
	role       string  ///< 小写角色；顶层 system 为 system
	body       string  ///< 不含角色标签的消息文本
	text       string  ///< 写入 Prompt 的带角色标签文本
	images     []Image ///< 本条消息中的图片，顺序与 body 中的占位一致
	toolResult bool    ///< 是否携带工具结果
}

// AllowsTools reports whether this turn exposes native custom tools.
func (r Request) AllowsTools() bool {
	return r.ToolChoice != "none" && len(r.Tools) > 0
}

// ParseRequest reads an Anthropic Messages or OpenAI chat body into one native
// Cursor SDK turn. Tool definitions and results remain structured.
func ParseRequest(body []byte) Request {
	var raw map[string]any
	if json.Unmarshal(body, &raw) != nil {
		return Request{}
	}
	request := Request{
		Model:       strings.TrimSpace(asString(raw["model"])),
		Tools:       parseClientTools(raw),
		ToolChoice:  parseToolChoice(raw["tool_choice"], raw["function_call"]),
		ToolResults: parseToolResults(raw),
	}
	request.Effort = requestEffort(raw)
	var system contentWriter
	if text := system.content(raw["system"]); text != "" {
		request.transcript = append(request.transcript, transcriptEntry{
			role: "system", body: text, text: text, images: system.images,
		})
	}
	for _, rawMessage := range requestMessages(raw) {
		message, _ := rawMessage.(map[string]any)
		if message == nil {
			continue
		}
		if entry, ok := parseTranscriptEntry(message); ok {
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
 * @details OpenAI chat 使用 `reasoning_effort`，Anthropic 使用 adaptive thinking 的 `output_config.effort`；预算型思考没有对应等级，不做换算。
 */
func requestEffort(raw map[string]any) string {
	effort := asString(raw["reasoning_effort"])
	if effort == "" {
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
	return strings.TrimSpace(strings.Join(parts, "\n\n")), images
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

func parseClientTools(raw map[string]any) []Tool {
	seen := make(map[string]struct{})
	var tools []Tool
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
 * @return 展平后的条目。
 * @return 消息没有可发送的文本或图片时为 false。
 * @details tool/function 角色只输出一次工具结果行，内容不再重复作为正文。
 */
func parseTranscriptEntry(message map[string]any) (transcriptEntry, bool) {
	role := strings.ToLower(strings.TrimSpace(asString(message["role"])))
	var writer contentWriter
	var chunks []string
	if role == "tool" || role == "function" {
		writer.toolResult = true
		id := firstNonEmpty(asString(message["tool_call_id"]), asString(message["id"]))
		chunks = append(chunks, formatToolResultLine(id, asString(message["name"]), writer.content(message["content"])))
	} else if text := writer.content(message["content"]); text != "" {
		chunks = append(chunks, text)
	} else if text := writer.content(message["text"]); text != "" {
		chunks = append(chunks, text)
	}
	if calls, _ := message["tool_calls"].([]any); len(calls) > 0 {
		for _, call := range calls {
			object, _ := call.(map[string]any)
			if object == nil {
				continue
			}
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
	body := strings.TrimSpace(strings.Join(chunks, "\n"))
	if body == "" {
		return transcriptEntry{}, false
	}
	text := body
	if role != "" {
		text = role + ": " + body
	}
	return transcriptEntry{
		role: role, body: body, text: text, images: writer.images, toolResult: writer.toolResult,
	}, true
}

func parseToolResults(raw map[string]any) []ToolResult {
	messages := requestMessages(raw)
	var trailing []ToolResult
	for _, message := range slices.Backward(messages) {
		message, _ := message.(map[string]any)
		batch := toolResultsFromMessage(message)
		if len(batch) == 0 {
			// Cursor clients may append a system context block after the tool
			// result while resuming the same native run. It is metadata, not a
			// new user turn, so keep scanning past it. Other message kinds still
			// terminate the trailing-result window to avoid replaying history.
			if message != nil && strings.EqualFold(asString(message["role"]), "system") {
				continue
			}
			break
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
	return results
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
	return writer.content(value)
}

/**
 * @brief 展平消息内容并按出现顺序收集图片。
 */
type contentWriter struct {
	images     []Image ///< 已替换为 `[image]` 占位的图片
	toolResult bool    ///< 内容中出现过工具结果块
}

func (w *contentWriter) content(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case []any:
		chunks := make([]string, 0, len(typed))
		for _, item := range typed {
			switch block := item.(type) {
			case string:
				if text := strings.TrimSpace(block); text != "" {
					chunks = append(chunks, text)
				}
			case map[string]any:
				if line := w.block(block); line != "" {
					chunks = append(chunks, line)
				}
			}
		}
		return strings.TrimSpace(strings.Join(chunks, "\n"))
	default:
		return strings.TrimSpace(asString(value))
	}
}

func (w *contentWriter) block(block map[string]any) string {
	switch strings.ToLower(asString(block["type"])) {
	case "", "text":
		return strings.TrimSpace(asString(block["text"]))
	case "image":
		source, _ := block["source"].(map[string]any)
		return w.image(Image{
			Data: asString(source["data"]), MimeType: asString(source["media_type"]), URL: asString(source["url"]),
		})
	case "image_url":
		url := asString(block["image_url"])
		if object, ok := block["image_url"].(map[string]any); ok {
			url = asString(object["url"])
		}
		return w.image(imageFromURL(url))
	case "tool_use":
		return formatToolUseLine(asString(block["id"]), asString(block["name"]), rawJSON(block["input"]))
	case "tool_result":
		w.toolResult = true
		return formatToolResultLine(asString(block["tool_use_id"]), "", w.content(block["content"]))
	case "function_call":
		return formatToolUseLine(asString(block["id"]), asString(block["name"]), rawJSON(block["arguments"]))
	default:
		return strings.TrimSpace(asString(block["text"]))
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

func formatToolResultLine(id, name, content string) string {
	line := "[tool_result"
	if id != "" {
		line += " tool_use_id=" + id
	}
	if name != "" {
		line += " name=" + name
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
