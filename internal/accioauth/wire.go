package accioauth

import (
	"bytes"
	"context"
	"crypto/md5" // The ADK request marker is MD5(request_id), not a signature.
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

/**
 * @brief Accio 会话字段及其客户端别名，首项为原生字段名和最高优先级。
 */
var sessionFields = [][]string{
	{"session_key", "sessionKey", "session_id", "sessionId"},
	{"conversation_id", "conversationId"},
	{"conversation_name", "conversationName"},
}

/**
 * @brief 将逻辑 Chat 请求转换为 Accio ADK 报文，保留客户端提供的会话标识。
 * @param ctx 出站上下文；取消时中止网络请求。
 * @param c 已验证账号凭证。
 * @param body 逻辑 Chat 请求 JSON。
 * @param model 目录声明的模型及能力。
 * @return 可重放的原生 HTTP 请求；失败时为 nil，成功后调用方须关闭正文。
 * @return 凭证、请求字段或模型能力非法，以及 JSON 编码或 URL 构造错误。
 * @note 每次生成独立请求和消息 ID；ADK 始终返回 SSE。
 */
func (s *Service) BuildChatRequest(ctx context.Context, c *Credential, body []byte, model Model) (*http.Request, error) {
	if c == nil || c.AccessToken == "" || model.ID == "" {
		return nil, errors.New("accio: credential and model name are required")
	}
	var input map[string]any
	if err := json.Unmarshal(body, &input); err != nil || input == nil {
		return nil, errors.New("accio: invalid Chat request JSON")
	}
	if err := validateChatOptions(input); err != nil {
		return nil, err
	}
	contents, system, err := chatContents(input["messages"], model.SupportsImages)
	if err != nil {
		return nil, err
	}
	id := uuid.NewString()
	properties := map[string]any{"normalized_response": "true"}
	payload := map[string]any{
		"model": model.ID, "token": c.AccessToken, "tenant": "accio-agent", "iai_tag": "phoenix-desktop",
		"empid": "", "request_id": id, "message_id": "msg-" + id,
		"contents": contents, "system_instruction": system, "properties": properties,
	}
	for _, fields := range sessionFields {
		for _, key := range fields {
			value := input[key]
			if value == nil {
				continue
			}
			text, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("accio: %s must be a string", key)
			}
			if text = strings.TrimSpace(text); text != "" {
				payload[fields[0]] = text
				break
			}
		}
	}
	if err := chatTools(input["tools"], payload); err != nil {
		return nil, err
	}
	for _, key := range []string{"temperature", "top_p"} {
		if value, exists := input[key]; exists && value != nil {
			if _, ok := value.(float64); !ok {
				return nil, fmt.Errorf("accio: %s must be a number", key)
			}
			payload[key] = value
		}
	}
	limit := input["max_completion_tokens"]
	if limit == nil {
		limit = input["max_tokens"]
	}
	if limit != nil {
		number, ok := limit.(float64)
		if !ok || number <= 0 || number != float64(int64(number)) {
			return nil, errors.New("accio: token limit must be a positive integer")
		}
		payload["max_output_tokens"] = limit
	}
	if stop := input["stop"]; stop != nil {
		switch value := stop.(type) {
		case string:
			payload["stop_sequences"] = []string{value}
		case []any:
			for _, item := range value {
				if _, ok := item.(string); !ok {
					return nil, errors.New("accio: stop must contain strings")
				}
			}
			payload["stop_sequences"] = value
		default:
			return nil, errors.New("accio: stop must be a string or array")
		}
	}
	if effort, err := chatEffort(input, model.Efforts); err != nil {
		return nil, err
	} else if effort != "" {
		payload["include_thoughts"] = true
		switch strings.ToLower(strings.TrimSpace(model.Protocol)) {
		case "responses", "openai":
			properties["reasoning_effort"] = effort
		default:
			payload["reasoning_effort"] = effort
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("accio request: %w", err)
	}
	base := strings.TrimRight(s.BaseURL, "/")
	if base == "" {
		base = c.GatewayURL()
	}
	digest := md5.Sum([]byte(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/api/adk/llm/generateContent?sg_k=%x", base, digest), bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	if c.Region != "cn" && c.Region != "global" {
		return nil, errors.New("accio: invalid region")
	}
	req.Header = requestHeaders(c)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("version", accioAppVersion)
	req.Header.Set("appKey", "OneChat")
	if c.SGCookie != "" {
		req.Header.Set("sgcookie", c.SGCookie)
	}
	deviceID := c.DeviceID
	if deviceID == "" && c.UserID != "" {
		// OAuth and previously imported credentials may lack a desktop ID.
		// Keep their client identity stable across requests, refreshes and restarts.
		digest := sha256.Sum256([]byte("accio/" + c.Region + "/" + c.UserID))
		deviceID = fmt.Sprintf("desktop-%x", digest[:16])
	}
	if deviceID != "" {
		req.Header.Set("utdid", deviceID)
	}
	return req, nil
}

/**
 * @brief 补回跨协议转换遗漏的 Accio 会话字段，供代理和管理测试在请求体规则之前使用。
 * @param body 转换后的有效 JSON 请求。
 * @param source 客户端原始 JSON 请求。
 * @return 带会话字段的请求；已有任一同组字段时保持该组原值。
 * @return JSON 字段写入失败时返回错误。
 * @note 仅复制原值；类型与空白处理由 BuildChatRequest 在最终出站边界执行。
 */
func PreserveSessionFields(body, source []byte) ([]byte, error) {
	current, original := gjson.ParseBytes(body), gjson.ParseBytes(source)
	for _, fields := range sessionFields {
		present := false
		for _, key := range fields {
			if current.Get(key).Exists() {
				present = true
				break
			}
		}
		if present {
			continue
		}
		for _, key := range fields {
			if value := original.Get(key); value.Exists() {
				var err error
				body, err = sjson.SetRawBytes(body, key, []byte(value.Raw))
				if err != nil {
					return nil, err
				}
			}
		}
	}
	return body, nil
}

func validateChatOptions(input map[string]any) error {
	for _, key := range []string{"functions", "function_call", "audio", "prediction", "logit_bias", "logprobs", "top_logprobs", "seed"} {
		if input[key] != nil {
			return fmt.Errorf("accio: unsupported option %s", key)
		}
	}
	if parallel := input["parallel_tool_calls"]; parallel != nil && parallel != true {
		return errors.New("accio: disabling parallel tool calls is unsupported")
	}
	if n := input["n"]; n != nil && n != float64(1) {
		return errors.New("accio: only n=1 is supported")
	}
	if choice := input["tool_choice"]; choice != nil && choice != "auto" {
		return errors.New("accio: only automatic tool choice is supported")
	}
	for _, key := range []string{"frequency_penalty", "presence_penalty"} {
		if input[key] != nil && input[key] != float64(0) {
			return fmt.Errorf("accio: unsupported option %s", key)
		}
	}
	if format := input["response_format"]; format != nil {
		value, ok := format.(map[string]any)
		if !ok || value["type"] != "text" {
			return errors.New("accio: structured response formats are unsupported")
		}
	}
	if modes := input["modalities"]; modes != nil {
		value, ok := modes.([]any)
		if !ok || len(value) != 1 || value[0] != "text" {
			return errors.New("accio: only text output is supported")
		}
	}
	return nil
}

func chatContents(raw any, images bool) ([]any, string, error) {
	messages, ok := raw.([]any)
	if !ok || len(messages) == 0 {
		return nil, "", errors.New("accio: messages must be a nonempty array")
	}
	contents := make([]any, 0, len(messages))
	var system []string
	names := map[string]string{}
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			return nil, "", errors.New("accio: invalid message")
		}
		role := wireText(message["role"])
		if message["refusal"] != nil && message["refusal"] != "" || message["audio"] != nil || message["function_call"] != nil {
			return nil, "", errors.New("accio: unsupported message content")
		}
		parts, err := chatParts(message["content"], images)
		if err != nil {
			return nil, "", err
		}
		switch role {
		case "system", "developer":
			for _, part := range parts {
				text, ok := part.(map[string]any)["text"].(string)
				if !ok {
					return nil, "", errors.New("accio: system messages must contain text")
				}
				system = append(system, text)
			}
			continue
		case "assistant":
			role = "model"
			if reasoning := message["reasoning_content"]; reasoning != nil {
				text, ok := reasoning.(string)
				if !ok {
					return nil, "", errors.New("accio: invalid reasoning content")
				}
				if text != "" {
					parts = append([]any{map[string]any{"thought": true, "text": text}}, parts...)
				}
			}
			if rawCalls := message["tool_calls"]; rawCalls != nil {
				calls, ok := rawCalls.([]any)
				if !ok {
					return nil, "", errors.New("accio: invalid tool calls")
				}
				for _, rawCall := range calls {
					call, ok := rawCall.(map[string]any)
					if !ok || call["type"] != "function" {
						return nil, "", errors.New("accio: only function tool calls are supported")
					}
					function, _ := call["function"].(map[string]any)
					name, id, args := wireText(function["name"]), wireText(call["id"]), wireText(function["arguments"])
					if name == "" || id == "" || !json.Valid([]byte(args)) {
						return nil, "", errors.New("accio: tool calls require id, name and valid JSON arguments")
					}
					names[id] = name
					part := map[string]any{"function_call": map[string]any{"id": id, "name": name, "args_json": args}}
					if extra, ok := call["extra_content"].(map[string]any); ok {
						if google, ok := extra["google"].(map[string]any); ok && google["thought_signature"] != nil {
							part["thought_signature"] = google["thought_signature"]
						}
					}
					parts = append(parts, part)
				}
			}
		case "tool":
			id := wireText(message["tool_call_id"])
			name := wireText(message["name"])
			if name == "" {
				name = names[id]
			}
			if id == "" || name == "" {
				return nil, "", errors.New("accio: tool result has no matching function call")
			}
			var text []string
			for _, part := range parts {
				value, ok := part.(map[string]any)["text"].(string)
				if !ok {
					return nil, "", errors.New("accio: only text tool results are supported")
				}
				text = append(text, value)
			}
			isError, _ := message["is_error"].(bool)
			response, err := json.Marshal(map[string]any{"content": strings.Join(text, ""), "is_error": isError})
			if err != nil {
				return nil, "", err
			}
			parts = []any{map[string]any{"function_response": map[string]any{"id": id, "name": name, "response_json": string(response)}}}
		case "user":
		default:
			return nil, "", fmt.Errorf("accio: unsupported message role %q", role)
		}
		if len(parts) > 0 {
			contents = append(contents, map[string]any{"role": role, "parts": parts})
		}
	}
	return contents, strings.Join(system, "\n\n"), nil
}

func chatParts(raw any, images bool) ([]any, error) {
	if raw == nil {
		return nil, nil
	}
	if text, ok := raw.(string); ok {
		return []any{map[string]any{"text": text}}, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, errors.New("accio: content must be text or an array")
	}
	parts := make([]any, 0, len(items))
	for _, item := range items {
		block, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("accio: invalid content block")
		}
		switch block["type"] {
		case "text", "input_text", "output_text":
			text, ok := block["text"].(string)
			if !ok {
				return nil, errors.New("accio: text block requires text")
			}
			parts = append(parts, map[string]any{"text": text})
		case "image_url", "input_image":
			if !images {
				return nil, errors.New("accio: model does not support images")
			}
			rawURL := block["image_url"]
			if rawURL == nil {
				rawURL = block["url"]
			}
			if object, ok := rawURL.(map[string]any); ok {
				rawURL = object["url"]
			}
			imageURL := wireText(rawURL)
			if after, ok0 := strings.CutPrefix(imageURL, "data:"); ok0 {
				meta, data, ok := strings.Cut(after, ",")
				mime, encoding, _ := strings.Cut(meta, ";")
				if !ok || encoding != "base64" || !strings.HasPrefix(mime, "image/") {
					return nil, errors.New("accio: image data must be a base64 image URI")
				}
				if _, err := base64.StdEncoding.DecodeString(data); err != nil {
					return nil, errors.New("accio: invalid base64 image")
				}
				parts = append(parts, map[string]any{"inline_data": map[string]any{"mime_type": mime, "data": data}})
			} else {
				parsed, err := url.Parse(imageURL)
				if err != nil || parsed.Host == "" || parsed.Scheme != "https" && parsed.Scheme != "http" {
					return nil, errors.New("accio: invalid image URL")
				}
				parts = append(parts, map[string]any{"file_data": map[string]any{"mime_type": "image/png", "file_uri": imageURL}})
			}
		default:
			return nil, fmt.Errorf("accio: unsupported content type %q", block["type"])
		}
	}
	return parts, nil
}

func chatTools(raw any, payload map[string]any) error {
	if raw == nil {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		return errors.New("accio: tools must be an array")
	}
	tools := make([]any, 0, len(items))
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok || tool["type"] != "function" {
			return errors.New("accio: only function tools are supported")
		}
		function, _ := tool["function"].(map[string]any)
		name := wireText(function["name"])
		if name == "" {
			return errors.New("accio: function tool requires a name")
		}
		if function["strict"] == true {
			return errors.New("accio: strict tool schemas are unsupported")
		}
		parameters := function["parameters"]
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if _, ok := parameters.(map[string]any); !ok {
			return errors.New("accio: function parameters must be an object")
		}
		encoded, err := json.Marshal(parameters)
		if err != nil {
			return err
		}
		tools = append(tools, map[string]any{"name": name, "description": wireText(function["description"]), "parameters_json": string(encoded)})
	}
	if len(tools) > 0 {
		payload["tools"] = tools
		payload["tool_config"] = `{"functionCallingConfig":{"streamFunctionCallArguments":true}}`
	}
	return nil
}

func chatEffort(input map[string]any, available []string) (string, error) {
	raw := input["reasoning_effort"]
	if reasoning, ok := input["reasoning"].(map[string]any); raw == nil && ok {
		raw = reasoning["effort"]
	}
	if raw == nil {
		return "", nil
	}
	wanted, ok := raw.(string)
	if !ok {
		return "", errors.New("accio: reasoning effort must be a string")
	}
	wanted = strings.ToLower(strings.TrimSpace(wanted))
	if wanted == "" || wanted == "none" || wanted == "off" || wanted == "disabled" {
		return "", nil
	}
	ranks := map[string]int{"minimal": 0, "low": 1, "medium": 2, "high": 3, "xhigh": 4, "max": 5}
	rank, ok := ranks[wanted]
	if !ok {
		return "", fmt.Errorf("accio: invalid reasoning effort %q", wanted)
	}
	if len(available) == 0 {
		return "", errors.New("accio: model does not declare reasoning efforts")
	}
	best, distance := available[0], 100
	for _, effort := range available {
		if candidate, ok := ranks[strings.ToLower(effort)]; ok {
			delta := candidate - rank
			if delta < 0 {
				delta = -delta
			}
			if delta < distance {
				best, distance = effort, delta
			}
		}
	}
	return best, nil
}

func wireText(value any) string { text, _ := value.(string); return text }
