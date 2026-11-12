package qoderauth

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" // Qoder's COSY wire signature requires MD5.
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"golang.org/x/net/idna"
)

/**
 * @brief cosyVersion identifies the desktop app's bundled worker runtime.
 */
const cosyVersion = "1.1.69"

/**
 * @brief cosySystemPrompt is the default identity sent by Qoder's agent client.
 */
const cosySystemPrompt = "You are a Qoder agent. Use the instructions below and the tools available to you to assist the user."

// These are public protocol constants distributed with the Qoder client.
const cosyModulus = "c0f22307e5cd362e296bb04470f6de8fbf935ce24e8fcf511a0e2701329769c4a76e499bb938036a52af1eaf818cf79a2600620e3ce87e371d2ca6d85803606a1b3fa5e874643c9ed2db7e85673ef7227fca56e2e7c08f0927609bb896a9f24be1782099a66016a5bfdc3f1ff756bfc9e88d7b5dc5be30bf45a0223a00ebcecf"
const cosyAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"

func (s *Service) wireURL(c *Credential, path string) string {
	base := s.BaseURL
	if base == "" {
		base = c.GatewayURL()
	}
	return strings.TrimRight(base, "/") + path
}

// BuildChatRequest constructs and signs Qoder's native streaming agent request.
/**
 * @brief BuildChatRequest constructs and signs Qoder's native streaming agent request.
 * @param ctx Request cancellation context.
 * @param c Validated Qoder credential.
 * @param body Logical OpenAI Chat request JSON.
 * @param model Selected native catalog model.
 * @return Signed native request, or a request/configuration error.
 */
func (s *Service) BuildChatRequest(ctx context.Context, c *Credential, body []byte, model Model) (*http.Request, error) {
	if c == nil || model.Key == "" {
		return nil, errors.New("qoder: credential and model key are required")
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(body, &input); err != nil {
		return nil, fmt.Errorf("qoder chat request: %w", err)
	}
	var original []map[string]json.RawMessage
	if err := json.Unmarshal(input["messages"], &original); err != nil || len(original) == 0 {
		return nil, errors.New("qoder: messages must be a nonempty array")
	}
	config := map[string]json.RawMessage{}
	if len(model.Config) != 0 {
		if err := json.Unmarshal(model.Config, &config); err != nil {
			return nil, fmt.Errorf("qoder model config: %w", err)
		}
	}
	tools, err := cosyTools(input)
	if err != nil {
		return nil, err
	}
	messages, err := cosyMessages(original)
	if err != nil {
		return nil, err
	}
	system := cosySystemPrompt
	lastUser := ""
	for _, message := range original {
		role := cosyString(message["role"])
		if role == "system" || role == "developer" {
			if text := cosyContentText(message["content"]); text != "" {
				system += "\n\n" + text
			}
		}
		if role == "user" {
			lastUser = cosyContentText(message["content"])
		}
	}
	if cosyString(input["tool_choice"]) == "required" && len(tools) > 0 {
		system += "\nYou must call an available function in this response."
	}
	systemBlocks := []map[string]any{{"type": "text", "text": system}}
	messages = append([]map[string]any{{"role": "system", "content": systemBlocks}}, messages...)
	params := map[string]any{}
	for _, key := range []string{"temperature", "top_p", "stop", "frequency_penalty", "presence_penalty", "seed", "tool_choice", "parallel_tool_calls", "response_format"} {
		if value, ok := input[key]; ok {
			params[key] = value
		}
	}
	budget := 0
	for _, key := range []string{"max_completion_tokens", "max_tokens"} {
		if json.Unmarshal(input[key], &budget) == nil && budget > 0 {
			break
		}
		budget = 0
	}
	limit := budget
	if limit == 0 {
		limit = 32000
	}
	cap := 0
	for _, key := range []string{"max_output_tokens", "maxOutputTokens"} {
		if json.Unmarshal(config[key], &cap) == nil && cap > 0 {
			break
		}
		cap = 0
	}
	if cap == 0 && model.Key == "lmodel" {
		cap = 131072
	}
	if cap > 0 {
		limit = min(limit, cap)
	}
	params["max_tokens"] = limit
	thinking := cosyThinking(input, model, config, budget, params)
	slim := cosyModelConfig(config, model, thinking)
	contextLength, err := cosyContextLength(config, original, tools, limit)
	if err != nil {
		return nil, err
	}
	extra := map[string]any{"context": []any{}, "modelConfig": map[string]any{"key": model.Key, "is_reasoning": thinking}, "originalContent": lastUser}
	if contextLength > 0 {
		params["context_length"] = contextLength
		slim["max_input_tokens"] = contextLength
		extra["ideModelConfigOverride"] = map[string]int{"max_input_tokens": contextLength}
	}
	sessionID := cosySession(input, original)
	task := cosyTaskFor(c, sessionID, original)
	requestID := uuid.NewString()
	envelope := map[string]any{
		"request_id": requestID, "request_set_id": task.ID, "chat_record_id": requestID,
		"session_id": sessionID, "stream": true, "session_type": "app", "agent_id": "agent_common", "task_id": "common",
		"system": systemBlocks, "messages": messages, "parameters": params, "model_config": slim,
		"chat_context": map[string]any{"chatPrompt": "", "imageUrls": nil, "features": []any{}, "text": lastUser, "extra": extra},
		"business":     map[string]any{"product": "app", "version": cosyVersion, "type": "agent", "stage": task.Stage, "id": task.ID, "name": task.Name, "begin_at": task.Begin, "model_key": model.Key},
	}
	if len(tools) > 0 {
		envelope["tools"] = tools
	}
	plain, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("qoder envelope: %w", err)
	}
	encoded := cosyEncode(plain)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.wireURL(c, "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"), bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	if err := cosySign(req, c, encoded); err != nil {
		return nil, err
	}
	req.Header.Del("Cosy-ClientIp")
	req.Header.Del("Cosy-MachineToken")
	req.Header.Del("Cosy-MachineType")
	if host := cosyHostname(); host != "" {
		req.Header.Set("Cosy-MachineHostname", host)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("X-Model-Key", model.Key)
	req.Header.Set("X-Model-Source", slim["source"].(string))
	return req, nil
}

/**
 * @brief cosyString reads a string field from a decoded JSON object.
 * @param raw JSON string field.
 * @return String value, or an empty string for other JSON types.
 */
func cosyString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

/**
 * @brief cosyModelConfig keeps only the fields Qoder's worker sends from a listing.
 * @param config Native catalog entry.
 * @param model Selected native model.
 * @param thinking Whether the request enables thinking.
 * @return The worker-compatible model configuration.
 */
func cosyModelConfig(config map[string]json.RawMessage, model Model, thinking bool) map[string]any {
	name, format, source := cosyString(config["display_name"]), cosyString(config["format"]), cosyString(config["source"])
	if name == "" {
		name = model.Key
	}
	if format == "" {
		format = "openai"
	}
	if source == "" {
		source = "system"
	}
	maximum := 200000
	if raw := config["max_input_tokens"]; len(raw) > 0 {
		var tokens int
		if json.Unmarshal(raw, &tokens) == nil && tokens > 0 {
			maximum = tokens
		}
	}
	var reasoning, images bool
	_ = json.Unmarshal(config["is_reasoning"], &reasoning)
	_ = json.Unmarshal(config["is_vl"], &images)
	out := map[string]any{"key": model.Key, "display_name": name, "format": format, "is_vl": images, "is_reasoning": reasoning && thinking, "api_key": "", "url": "", "source": source, "max_input_tokens": maximum}
	if provider := cosyString(config["outer_provider"]); provider != "" {
		out["outer_provider"] = provider
	} else {
		out["model"] = ""
	}
	return out
}

/**
 * @brief cosyThinking selects the catalog default or closest supported thinking effort.
 * @param input Logical Chat fields.
 * @param model Catalog model capabilities.
 * @param config Native model configuration.
 * @param budget Explicit output budget, or zero.
 * @param params Native generation parameters to update.
 * @return Whether thinking is enabled.
 */
func cosyThinking(input map[string]json.RawMessage, model Model, config map[string]json.RawMessage, budget int, params map[string]any) bool {
	levels := slices.Clone(model.Efforts)
	var declared []string
	if json.Unmarshal(config["reasoning_efforts"], &declared) == nil {
		levels = declared
	}
	var reasoning bool
	_ = json.Unmarshal(config["is_reasoning"], &reasoning)
	thinks, always, defaultEffort := model.SupportsReasoning || reasoning, false, ""
	var tc map[string]json.RawMessage
	if json.Unmarshal(config["thinking_config"], &tc) == nil && tc != nil {
		thinks = len(tc["enabled"]) != 0 && string(tc["enabled"]) != "null"
		always = thinks && (len(tc["disabled"]) == 0 || string(tc["disabled"]) == "null")
		levels = nil
		var enabled struct {
			Efforts map[string]struct {
				Default bool `json:"is_default"`
			} `json:"efforts"`
		}
		if thinks && json.Unmarshal(tc["enabled"], &enabled) == nil {
			for level, entry := range enabled.Efforts {
				levels = append(levels, level)
				if entry.Default {
					defaultEffort = level
				}
			}
		}
	}
	rank := []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}
	slices.SortFunc(levels, func(a, b string) int {
		i, j := slices.Index(rank, a), slices.Index(rank, b)
		if i < 0 {
			i = len(rank)
		}
		if j < 0 {
			j = len(rank)
		}
		if i != j {
			return i - j
		}
		return strings.Compare(a, b)
	})
	var asked any
	for _, key := range []string{"reasoning_effort", "reasoning", "thinking"} {
		if raw := input[key]; len(raw) > 0 && string(raw) != "null" {
			_ = json.Unmarshal(raw, &asked)
			break
		}
	}
	if object, ok := asked.(map[string]any); ok {
		if effort, exists := object["effort"]; exists {
			asked = effort
		} else {
			asked = object["type"]
		}
	}
	want, _ := asked.(string)
	want = strings.ToLower(want)
	if asked == false || want == "off" || want == "disabled" {
		want = "none"
	}
	if !slices.Contains(rank, want) {
		want = ""
	}
	if want == "" && budget > 0 && budget <= 1024 {
		want = "none"
	}
	thinking := thinks && (want != "none" || always)
	params["enable_thinking"] = thinking
	if !thinking || len(levels) == 0 {
		return thinking
	}
	effort := defaultEffort
	if want == "none" {
		effort = levels[0]
	} else if want != "" {
		if want == "ultra" && !slices.Contains(levels, want) {
			want = "max"
		}
		effort = want
		if !slices.Contains(levels, want) {
			at, distance := slices.Index(rank, want), len(rank)
			for _, level := range levels {
				i := slices.Index(rank, level)
				if i <= 0 {
					continue
				}
				delta := i - at
				if delta < 0 {
					delta = -delta
				}
				if delta < distance || delta == distance && i > at {
					effort, distance = level, delta
				}
			}
		}
	}
	if effort != "" {
		params["reasoning_effort"] = effort
	}
	return thinking
}

/**
 * @brief cosyContextLength includes the requested reply when selecting a context tier.
 * @param config Native context configuration.
 * @param messages Original conversation history.
 * @param tools Normalized function declarations.
 * @param reply Requested maximum output tokens.
 * @return Selected context size, or a malformed configuration error.
 */
func cosyContextLength(config map[string]json.RawMessage, messages []map[string]json.RawMessage, tools []map[string]any, reply int) (int, error) {
	type tier struct {
		Tokens       int             `json:"token_count"`
		TokensCamel  int             `json:"tokenCount"`
		Default      json.RawMessage `json:"is_default"`
		DefaultCamel json.RawMessage `json:"isDefault"`
	}
	var current int
	if raw := config["max_input_tokens"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &current); err != nil {
			return 0, fmt.Errorf("qoder current context length: %w", err)
		}
	}
	raw := bytes.TrimSpace(config["context_config"])
	if len(raw) == 0 {
		raw = bytes.TrimSpace(config["contextConfig"])
	}
	var tiers []tier
	if len(raw) != 0 && !bytes.Equal(raw, []byte("null")) {
		if raw[0] == '{' {
			var entries map[string]tier
			if err := json.Unmarshal(raw, &entries); err != nil {
				return 0, fmt.Errorf("qoder context config: %w", err)
			}
			for _, entry := range entries {
				tiers = append(tiers, entry)
			}
		} else if err := json.Unmarshal(raw, &tiers); err != nil {
			return 0, fmt.Errorf("qoder context config: %w", err)
		}
	}
	windows, defaultTokens := []int{}, 0
	for _, entry := range tiers {
		tokens := entry.Tokens
		if tokens == 0 {
			tokens = entry.TokensCamel
		}
		if tokens <= 0 {
			continue
		}
		windows = append(windows, tokens)
		for _, marker := range []json.RawMessage{entry.Default, entry.DefaultCamel} {
			if slices.Contains([]string{"true", "1", `"1"`}, string(marker)) {
				defaultTokens = tokens
			}
		}
	}
	if len(windows) == 0 {
		for _, key := range []string{"available_context_windows", "availableContextWindows"} {
			var available []int
			if json.Unmarshal(config[key], &available) == nil {
				for _, tokens := range available {
					if tokens > 0 {
						windows = append(windows, tokens)
					}
				}
				break
			}
		}
		for _, key := range []string{"default_context_window", "defaultContextWindow"} {
			var tokens int
			if json.Unmarshal(config[key], &tokens) == nil && slices.Contains(windows, tokens) {
				defaultTokens = tokens
				break
			}
		}
	}
	if len(windows) == 0 {
		return current, nil
	}
	slices.Sort(windows)
	base := defaultTokens
	if base == 0 {
		base = current
	}
	if base == 0 {
		base = windows[len(windows)-1]
	}
	payload, err := json.Marshal(map[string]any{"messages": messages, "tools": tools})
	if err != nil {
		return 0, fmt.Errorf("qoder context input: %w", err)
	}
	characters, cjk := 0, 0
	for _, ch := range string(payload) {
		characters++
		if ch >= 0x1100 && ch <= 0x11ff || ch >= 0x2e80 && ch <= 0x9fff || ch >= 0xac00 && ch <= 0xd7af || ch >= 0xf900 && ch <= 0xfaff || ch >= 0xff00 && ch <= 0xffef {
			cjk++
		}
	}
	needed := ((cjk+(characters-cjk+3)/4)*115+99)/100 + reply
	if needed <= base {
		return base, nil
	}
	for _, tokens := range windows {
		if tokens >= needed {
			return tokens, nil
		}
	}
	return windows[len(windows)-1], nil
}

/**
 * @brief cosyContentText joins a message's text blocks without changing their text.
 * @param raw Text or content-block JSON.
 * @return Joined text content.
 */
func cosyContentText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var result strings.Builder
	for _, part := range parts {
		result.WriteString(part.Text)
	}
	return result.String()
}

/**
 * @brief cosyBlocks normalizes supported text and image content at the Chat boundary.
 * @param raw Chat message content JSON.
 * @return Normalized text/image blocks, or unsupported-content error.
 */
func cosyBlocks(raw json.RawMessage) ([]map[string]any, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if text == "" {
			return nil, nil
		}
		return []map[string]any{{"type": "text", "text": text}}, nil
	}
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return nil, errors.New("qoder: message content must be text or an array")
	}
	blocks := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch kind := cosyString(part["type"]); kind {
		case "text":
			var value string
			if json.Unmarshal(part["text"], &value) != nil {
				return nil, errors.New("qoder: text content must be a string")
			}
			blocks = append(blocks, map[string]any{"type": "text", "text": value})
		case "image_url":
			url := cosyString(part["image_url"])
			if url == "" {
				var image struct {
					URL string `json:"url"`
				}
				if json.Unmarshal(part["image_url"], &image) != nil || image.URL == "" {
					return nil, errors.New("qoder: image_url requires a URL")
				}
				url = image.URL
			}
			blocks = append(blocks, map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}})
		default:
			return nil, fmt.Errorf("qoder: unsupported content type %q", kind)
		}
	}
	return blocks, nil
}

/**
 * @brief cosyReasoning retains assistant thinking under Qoder's canonical field.
 * @param message Original assistant message.
 * @return Nonblank thinking text, or an empty string.
 */
func cosyReasoning(message map[string]json.RawMessage) string {
	for _, key := range []string{"reasoning_content", "reasoning"} {
		if text := cosyString(message[key]); strings.TrimSpace(text) != "" {
			return text
		}
	}
	var details []struct {
		Text    string `json:"text"`
		Summary string `json:"summary"`
	}
	_ = json.Unmarshal(message["reasoning_details"], &details)
	var result strings.Builder
	for _, detail := range details {
		if detail.Text != "" {
			result.WriteString(detail.Text)
		} else {
			result.WriteString(detail.Summary)
		}
	}
	if strings.TrimSpace(result.String()) == "" {
		return ""
	}
	return result.String()
}

/**
 * @brief cosyMessages repairs tool-history pairing and marks the last user cache block.
 * @param messages Original Chat conversation.
 * @return Normalized conversation, or an invalid-message error.
 */
func cosyMessages(messages []map[string]json.RawMessage) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(messages))
	pending := []map[string]any{}
	open := []string{}
	kept, names := map[string]bool{}, map[string]string{}
	flush := func() {
		if len(pending) > 0 {
			out = append(out, map[string]any{"role": "user", "content": pending})
			pending = nil
		}
	}
	for i, message := range messages {
		role := cosyString(message["role"])
		switch role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			return nil, fmt.Errorf("qoder: unsupported message role %q", role)
		}
		if (len(message["content"]) == 0 || string(message["content"]) == "null") && role != "assistant" {
			return nil, fmt.Errorf("qoder: %s message requires content", role)
		}
		blocks, err := cosyBlocks(message["content"])
		if err != nil {
			return nil, err
		}
		if role == "system" || role == "developer" {
			continue
		}
		if role == "user" {
			open = nil
		}
		if role != "user" && role != "tool" {
			flush()
		}
		text := cosyContentText(message["content"])
		if role == "tool" {
			images := []map[string]any{}
			for _, block := range blocks {
				if block["type"] == "image_url" {
					images = append(images, block)
				}
			}
			id := strings.TrimSpace(cosyString(message["tool_call_id"]))
			pick := slices.Index(open, id)
			if pick < 0 {
				for k, candidate := range open {
					if !kept[candidate] {
						pick = k
						break
					}
				}
			}
			if pick < 0 {
				if strings.TrimSpace(text) == "" && len(images) == 0 {
					continue
				}
				if len(pending) > 0 {
					pending = append(pending, map[string]any{"type": "text", "text": "\n\n"})
				}
				if strings.TrimSpace(text) != "" {
					pending = append(pending, map[string]any{"type": "text", "text": strings.TrimSpace(text)})
				}
				pending = append(pending, images...)
				continue
			}
			id = open[pick]
			open = slices.Delete(open, pick, pick+1)
			if len(images) > 0 {
				of := "tool call " + id
				name := names[id]
				if name == "" {
					name = cosyString(message["name"])
				}
				if name != "" {
					of = name + " (" + of + ")"
				}
				pending = append(pending, map[string]any{"type": "text", "text": "[From the result of " + of + ":]"})
				pending = append(pending, images...)
				note := "[The tool returned an image; it follows in the next message.]"
				if len(images) != 1 {
					note = fmt.Sprintf("[The tool returned %d images; they follow in the next message.]", len(images))
				}
				if strings.TrimSpace(text) != "" {
					text += "\n\n"
				}
				text += note
			}
			out = append(out, map[string]any{"role": "tool", "tool_call_id": id, "content": text})
			continue
		}
		entry := map[string]any{"role": role}
		if role == "assistant" {
			if thinking := cosyReasoning(message); thinking != "" {
				entry["reasoning_content"] = thinking
			}
			var calls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				} `json:"function"`
			}
			if raw := message["tool_calls"]; len(raw) > 0 && string(raw) != "null" {
				if json.Unmarshal(raw, &calls) != nil {
					return nil, errors.New("qoder: tool_calls must be an array")
				}
			}
			if len(calls) > 0 {
				native := make([]map[string]any, 0, len(calls))
				open, kept = nil, map[string]bool{}
				for k, call := range calls {
					if call.Type != "function" || call.Function.Name == "" {
						return nil, errors.New("qoder: tool_calls require named function calls")
					}
					var arguments string
					if json.Unmarshal(call.Function.Arguments, &arguments) != nil {
						var compact bytes.Buffer
						_ = json.Compact(&compact, call.Function.Arguments)
						arguments = compact.String()
						if arguments == "" || arguments == "null" {
							arguments = "{}"
						}
					}
					id := strings.TrimSpace(call.ID)
					if id == "" {
						digest := sha256.Sum256(fmt.Appendf(nil, "%d\x00%d\x00%s\x00%s", i, k, call.Function.Name, arguments))
						id = "call_" + hex.EncodeToString(digest[:16])
					}
					names[id] = call.Function.Name
					open = append(open, id)
					native = append(native, map[string]any{"id": id, "type": "function", "function": map[string]string{"name": call.Function.Name, "arguments": arguments}})
				}
				for j := i + 1; j < len(messages) && cosyString(messages[j]["role"]) == "tool"; j++ {
					id := strings.TrimSpace(cosyString(messages[j]["tool_call_id"]))
					if slices.Contains(open, id) {
						kept[id] = true
					}
				}
				entry["content"], entry["tool_calls"] = text, native
				out = append(out, entry)
				continue
			}
			blocks = slices.DeleteFunc(blocks, func(block map[string]any) bool { return block["type"] != "text" })
		}
		if len(blocks) == 0 {
			continue
		}
		if role == "user" && len(pending) > 0 {
			blocks = append(pending, blocks...)
			pending = nil
		}
		entry["content"] = blocks
		out = append(out, entry)
		open = nil
	}
	flush()
	for i, message := range out {
		if message["role"] != "user" {
			continue
		}
		blocks := message["content"].([]map[string]any)
		if i == len(out)-1 && len(blocks) > 0 {
			blocks[len(blocks)-1]["cache_control"] = map[string]string{"type": "ephemeral"}
		}
		message["contents"] = blocks
	}
	return out, nil
}

/**
 * @brief cosyTools applies the supported function schema normalization.
 * @param input Logical Chat fields.
 * @return Normalized declarations, or an invalid-tool error.
 */
func cosyTools(input map[string]json.RawMessage) ([]map[string]any, error) {
	var tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Parameters  any    `json:"parameters"`
		} `json:"function"`
	}
	if raw := input["tools"]; len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &tools); err != nil {
			return nil, errors.New("qoder: tools must be an array")
		}
	}
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		if tool.Type != "function" || tool.Function.Name == "" {
			return nil, errors.New("qoder: tools require named functions")
		}
		if cosyString(input["tool_choice"]) == "none" {
			continue
		}
		parameters := tool.Function.Parameters
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		cosyTypedUnions(parameters)
		out = append(out, map[string]any{"type": "function", "function": map[string]any{"name": tool.Function.Name, "description": tool.Function.Description, "parameters": parameters}})
	}
	return out, nil
}

/**
 * @brief cosyTypedUnions types object-only oneOf schemas, including local references.
 * @param root Decoded function parameter schema.
 * @return No value; updates the decoded schema in place.
 */
func cosyTypedUnions(root any) {
	var object func(any, []string) bool
	object = func(value any, seen []string) bool {
		schema, ok := value.(map[string]any)
		if !ok {
			return false
		}
		if kind, exists := schema["type"]; exists {
			return kind == "object"
		}
		ref, ok := schema["$ref"].(string)
		if !ok || !strings.HasPrefix(ref, "#/") || slices.Contains(seen, ref) || len(seen) > 16 {
			return false
		}
		at := root
		for part := range strings.SplitSeq(ref[2:], "/") {
			key := strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
			switch node := at.(type) {
			case map[string]any:
				at = node[key]
			case []any:
				i, err := strconv.Atoi(key)
				if err != nil || i < 0 || i >= len(node) {
					return false
				}
				at = node[i]
			default:
				return false
			}
		}
		return object(at, append(seen, ref))
	}
	var walk func(any, int)
	walk = func(value any, depth int) {
		schema, ok := value.(map[string]any)
		if !ok || depth > 64 {
			return
		}
		if _, typed := schema["type"]; !typed {
			if branches, ok := schema["oneOf"].([]any); ok && len(branches) > 0 {
				allObjects := true
				for _, branch := range branches {
					if !object(branch, nil) {
						allObjects = false
						break
					}
				}
				if allObjects {
					schema["type"] = "object"
				}
			}
		}
		for _, key := range []string{"items", "additionalItems", "unevaluatedItems", "contains", "not", "if", "then", "else", "propertyNames", "additionalProperties", "unevaluatedProperties"} {
			walk(schema[key], depth+1)
		}
		for _, key := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
			if list, ok := schema[key].([]any); ok {
				for _, child := range list {
					walk(child, depth+1)
				}
			}
		}
		for _, key := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
			if entries, ok := schema[key].(map[string]any); ok {
				for _, child := range entries {
					walk(child, depth+1)
				}
			}
		}
	}
	walk(root, 0)
}

/**
 * @brief cosyHostname formats the optional desktop machine hostname for HTTP headers.
 * @return A printable hostname of at most 96 bytes, or an empty string.
 */
func cosyHostname() string {
	name, _ := os.Hostname()
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	safe := func(value string) bool {
		return strings.IndexFunc(value, func(ch rune) bool { return ch < 0x20 || ch > 0x7e }) < 0
	}
	if !safe(name) {
		ascii, err := idna.ToASCII(name)
		if err == nil && safe(ascii) {
			name = ascii
		} else {
			digest := sha256.Sum256([]byte(name))
			name = strings.Trim(strings.Map(func(ch rune) rune {
				if ch < 0x21 || ch > 0x7e {
					return '-'
				}
				return ch
			}, name), "-") + "-" + hex.EncodeToString(digest[:4])
		}
	}
	if len(name) > 96 {
		digest := sha256.Sum256([]byte(name))
		name = strings.TrimRight(name[:87], "- ") + "-" + hex.EncodeToString(digest[:4])
	}
	return name
}

func cosyEncode(plain []byte) []byte {
	src := base64.StdEncoding.EncodeToString(plain)
	n := len(src) / 3
	src = src[len(src)-n:] + src[n:len(src)-n] + src[:n]
	const standard = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	return []byte(strings.Map(func(r rune) rune {
		if r == '=' {
			return '$'
		}
		return rune(cosyAlphabet[strings.IndexRune(standard, r)])
	}, src))
}

/**
 * @brief cosySign authenticates the encoded request with Qoder desktop COSY identity.
 * @param req Native HTTP request.
 * @param c Authenticated account identity.
 * @param body Final encoded request body.
 * @return A signing error, or nil.
 */
func cosySign(req *http.Request, c *Credential, body []byte) error {
	if c == nil || c.UserID == "" || c.AccessToken == "" || c.MachineID == "" {
		return errors.New("qoder: user ID, access token and machine ID are required for COSY")
	}
	randomKey := make([]byte, 8)
	if _, err := rand.Read(randomKey); err != nil {
		return err
	}
	key := []byte(hex.EncodeToString(randomKey))
	identity, err := json.Marshal(map[string]string{"uid": c.UserID, "security_oauth_token": c.AccessToken, "name": c.Name, "aid": "", "email": c.Email})
	if err != nil {
		return err
	}
	padding := aes.BlockSize - len(identity)%aes.BlockSize
	identity = append(identity, bytes.Repeat([]byte{byte(padding)}, padding)...)
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	cipher.NewCBCEncrypter(block, key).CryptBlocks(identity, identity)
	modulus, ok := new(big.Int).SetString(cosyModulus, 16)
	if !ok {
		return errors.New("qoder: invalid COSY public key")
	}
	//nolint:staticcheck // COSY requires PKCS#1 v1.5; OAEP changes the external wire contract.
	wrapped, err := rsa.EncryptPKCS1v15(rand.Reader, &rsa.PublicKey{N: modulus, E: 65537}, key)
	if err != nil {
		return err
	}
	requestID := uuid.NewString()
	payloadJSON, err := json.Marshal(map[string]string{"version": "v1", "requestId": requestID, "info": base64.StdEncoding.EncodeToString(identity), "cosyVersion": cosyVersion, "ideVersion": ""})
	if err != nil {
		return err
	}
	payload, wrappedKey := base64.StdEncoding.EncodeToString(payloadJSON), base64.StdEncoding.EncodeToString(wrapped)
	date := strconv.FormatInt(time.Now().Unix(), 10)
	path := strings.TrimPrefix(req.URL.EscapedPath(), "/algo")
	digest := md5.Sum([]byte(payload + "\n" + wrappedKey + "\n" + date + "\n" + string(body) + "\n" + path))
	bodyHash := md5.Sum(body)
	arch := runtime.GOARCH
	switch arch {
	case "arm64":
		arch = "aarch64"
	case "amd64":
		arch = "x86_64"
	}
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	for name, value := range map[string]string{
		"User-Agent":    "Qoder",
		"Authorization": "Bearer COSY." + payload + "." + hex.EncodeToString(digest[:]),
		"Cosy-Key":      wrappedKey, "Cosy-User": c.UserID, "Cosy-Date": date, "Cosy-Version": cosyVersion,
		"Cosy-Machineid": c.MachineID, "Cosy-Machinetoken": c.MachineID, "Cosy-Machinetype": "5",
		"Cosy-Machineos": arch + "_" + platform, "Cosy-Clienttype": "10", "Cosy-Clientip": c.MachineID,
		"Cosy-Business-Product": "app", "Cosy-Business-Type": "agent", "Cosy-Scene": "app",
		"Cosy-Bodyhash": hex.EncodeToString(bodyHash[:]), "Cosy-Bodylength": strconv.Itoa(len(body)), "Cosy-Sigpath": path,
		"Cosy-Data-Policy": "disagree", "Cosy-Organization-Id": "", "Cosy-Organization-Tags": "", "Login-Version": "v2", "X-Request-Id": requestID,
	} {
		req.Header.Set(name, value)
	}
	return nil
}

// cosyModelID derives the public model name: display name without whitespace, lowercased.
func cosyModelID(display string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, display)
}
