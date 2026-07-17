package common

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const maxAnthropicResponseSize = 50 * 1024 * 1024

// NormalizeAnthropicResponse returns one canonical Anthropic message response.
// Anthropic-compatible upstreams may return either the documented message JSON
// or a complete SSE transcript even when the client requested a non-streaming
// response. Both wire forms must feed the same downstream conversion path.
func NormalizeAnthropicResponse(raw []byte) ([]byte, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty anthropic response")
	}

	if json.Valid(raw) {
		message, err := decodeJSONObject(raw)
		if err != nil {
			return nil, err
		}
		if err := validateAnthropicMessage(message); err != nil {
			return nil, err
		}
		return json.Marshal(message)
	}

	payloads, err := anthropicSSEPayloads(raw)
	if err != nil {
		return nil, err
	}
	message, err := aggregateAnthropicSSE(payloads)
	if err != nil {
		return nil, err
	}
	if err := validateAnthropicMessage(message); err != nil {
		return nil, err
	}
	return json.Marshal(message)
}

func decodeJSONObject(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("decode anthropic response: %w", err)
	}
	if value == nil {
		return nil, fmt.Errorf("anthropic response must be an object")
	}
	return value, nil
}

func validateAnthropicMessage(message map[string]any) error {
	if messageType, _ := message["type"].(string); messageType != "message" {
		return fmt.Errorf("anthropic response type %q is not a message", messageType)
	}
	if role, _ := message["role"].(string); role != "assistant" {
		return fmt.Errorf("anthropic response role %q is not assistant", role)
	}
	content, ok := message["content"].([]any)
	if !ok {
		return fmt.Errorf("anthropic response content must be an array")
	}
	for index, rawBlock := range content {
		block, ok := rawBlock.(map[string]any)
		if !ok {
			return fmt.Errorf("anthropic response content block %d must be an object", index)
		}
		blockType, _ := block["type"].(string)
		switch blockType {
		case "text", "thinking", "redacted_thinking":
		case "tool_use":
			if input, exists := block["input"]; exists {
				if _, ok := input.(map[string]any); !ok {
					return fmt.Errorf("anthropic tool_use block %d input must be an object", index)
				}
			}
		default:
			return fmt.Errorf("unsupported anthropic response block type %q", blockType)
		}
	}
	return nil
}

func anthropicSSEPayloads(raw []byte) ([][]byte, error) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), maxAnthropicResponseSize)

	var payloads [][]byte
	var dataLines [][]byte
	flush := func() {
		if len(dataLines) == 0 {
			return
		}
		payload := bytes.Join(dataLines, []byte{'\n'})
		payloads = append(payloads, append([]byte(nil), payload...))
		dataLines = dataLines[:0]
	}

	for scanner.Scan() {
		line := bytes.TrimSuffix(scanner.Bytes(), []byte{'\r'})
		if len(line) == 0 {
			flush()
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			dataLines = append(dataLines, bytes.TrimSpace(line[len("data:"):]))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read anthropic SSE response: %w", err)
	}
	flush()
	if len(payloads) == 0 {
		return nil, fmt.Errorf("anthropic SSE response has no data events")
	}
	return payloads, nil
}

func aggregateAnthropicSSE(payloads [][]byte) (map[string]any, error) {
	var message map[string]any
	blocks := make(map[int]map[string]any)
	toolArguments := make(map[int]*strings.Builder)

	for _, payload := range payloads {
		if bytes.Equal(bytes.TrimSpace(payload), []byte("[DONE]")) {
			continue
		}
		if !json.Valid(payload) {
			return nil, fmt.Errorf("invalid anthropic SSE data: %s", strings.TrimSpace(string(payload)))
		}
		event, err := decodeJSONObject(payload)
		if err != nil {
			return nil, err
		}
		eventType, _ := event["type"].(string)
		switch eventType {
		case "message_start":
			started, ok := event["message"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("anthropic message_start has no message")
			}
			message = cloneJSONObject(started)
			message["type"] = "message"
			message["role"] = "assistant"
		case "content_block_start":
			index, err := jsonIndex(event["index"])
			if err != nil {
				return nil, fmt.Errorf("anthropic content_block_start: %w", err)
			}
			block, ok := event["content_block"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("anthropic content_block_start %d has no content_block", index)
			}
			blocks[index] = cloneJSONObject(block)
		case "content_block_delta":
			index, err := jsonIndex(event["index"])
			if err != nil {
				return nil, fmt.Errorf("anthropic content_block_delta: %w", err)
			}
			block := blocks[index]
			if block == nil {
				return nil, fmt.Errorf("anthropic content_block_delta %d precedes content_block_start", index)
			}
			delta, ok := event["delta"].(map[string]any)
			if !ok {
				return nil, fmt.Errorf("anthropic content_block_delta %d has no delta", index)
			}
			deltaType, _ := delta["type"].(string)
			switch deltaType {
			case "text_delta":
				appendString(block, "text", delta["text"])
			case "thinking_delta":
				appendString(block, "thinking", delta["thinking"])
			case "signature_delta":
				appendString(block, "signature", delta["signature"])
			case "input_json_delta":
				builder := toolArguments[index]
				if builder == nil {
					builder = &strings.Builder{}
					toolArguments[index] = builder
				}
				partial, ok := delta["partial_json"].(string)
				if !ok {
					return nil, fmt.Errorf("anthropic input_json_delta %d has no partial_json", index)
				}
				builder.WriteString(partial)
			case "citations_delta":
				citation, ok := delta["citation"].(map[string]any)
				if !ok {
					return nil, fmt.Errorf("anthropic citations_delta %d has no citation", index)
				}
				citations, _ := block["citations"].([]any)
				block["citations"] = append(citations, citation)
			default:
				return nil, fmt.Errorf("unsupported anthropic response delta type %q", deltaType)
			}
		case "content_block_stop":
			index, err := jsonIndex(event["index"])
			if err != nil {
				return nil, fmt.Errorf("anthropic content_block_stop: %w", err)
			}
			if err := finishAnthropicToolInput(blocks[index], toolArguments[index]); err != nil {
				return nil, fmt.Errorf("anthropic tool_use block %d: %w", index, err)
			}
		case "message_delta":
			if message == nil {
				return nil, fmt.Errorf("anthropic message_delta precedes message_start")
			}
			if delta, ok := event["delta"].(map[string]any); ok {
				if stopReason, exists := delta["stop_reason"]; exists {
					message["stop_reason"] = stopReason
				}
				if stopSequence, exists := delta["stop_sequence"]; exists {
					message["stop_sequence"] = stopSequence
				}
			}
			if usage, ok := event["usage"].(map[string]any); ok {
				current, _ := message["usage"].(map[string]any)
				if current == nil {
					current = make(map[string]any)
				}
				for key, value := range usage {
					current[key] = value
				}
				message["usage"] = current
			}
		case "message_stop", "ping":
		case "error":
			return nil, fmt.Errorf("anthropic SSE error: %v", event["error"])
		default:
			return nil, fmt.Errorf("unsupported anthropic SSE event type %q", eventType)
		}
	}

	if message == nil {
		return nil, fmt.Errorf("anthropic SSE response has no message_start")
	}
	indices := make([]int, 0, len(blocks))
	for index := range blocks {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	content := make([]any, 0, len(indices))
	for _, index := range indices {
		if err := finishAnthropicToolInput(blocks[index], toolArguments[index]); err != nil {
			return nil, fmt.Errorf("anthropic tool_use block %d: %w", index, err)
		}
		content = append(content, blocks[index])
	}
	message["content"] = content
	return message, nil
}

func finishAnthropicToolInput(block map[string]any, arguments *strings.Builder) error {
	if block == nil || block["type"] != "tool_use" {
		return nil
	}
	if arguments == nil || strings.TrimSpace(arguments.String()) == "" {
		if _, exists := block["input"]; !exists {
			block["input"] = map[string]any{}
		}
		return nil
	}
	input, err := decodeJSONObject([]byte(arguments.String()))
	if err != nil {
		return fmt.Errorf("decode input JSON: %w", err)
	}
	block["input"] = input
	return nil
}

func jsonIndex(value any) (int, error) {
	switch number := value.(type) {
	case json.Number:
		index, err := number.Int64()
		if err != nil || index < 0 {
			return 0, fmt.Errorf("invalid content block index %q", number)
		}
		return int(index), nil
	case float64:
		index := int(number)
		if number != float64(index) || index < 0 {
			return 0, fmt.Errorf("invalid content block index %v", number)
		}
		return index, nil
	default:
		return 0, fmt.Errorf("missing content block index")
	}
}

func appendString(target map[string]any, key string, value any) {
	part, _ := value.(string)
	current, _ := target[key].(string)
	target[key] = current + part
}

func cloneJSONObject(value map[string]any) map[string]any {
	clone := make(map[string]any, len(value))
	for key, item := range value {
		clone[key] = item
	}
	return clone
}
