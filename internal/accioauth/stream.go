package accioauth

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// APIError carries the HTTP-equivalent classification of native ADK errors.
type APIError struct {
	StatusCode int    `json:"status"`
	Type       string `json:"type"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

func (e *APIError) Error() string { return e.Message }

// ClassifyError handles HTTP errors and business errors inside HTTP 200 SSE.
func ClassifyError(status int, body []byte) *APIError {
	var root map[string]any
	_ = json.Unmarshal(body, &root)
	message := wireText(streamField(root, "errorMessage", "error_message"))
	if nested, ok := root["error"].(map[string]any); ok {
		root = nested
		message = wireText(root["message"])
	}
	if message == "" {
		message = wireText(root["message"])
	}
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if message == "" {
		message = http.StatusText(status)
	}
	if runes := []rune(message); len(runes) > 1000 {
		message = string(runes[:1000])
	}
	code := streamField(root, "errorCode", "error_code")
	if code == nil {
		code = root["code"]
	}
	businessStatus, _ := strconv.Atoi(fmt.Sprint(code))
	lower := strings.ToLower(string(body))
	result := &APIError{StatusCode: 502, Type: "upstream_error", Code: "accio_upstream_error", Message: message}
	switch {
	case status == 401 || status == 403 || businessStatus == 401 || businessStatus == 403 || containsHint(lower, []string{"unauthorized", "not logged in", "invalid token", "token expired", "invalid_token"}):
		result.StatusCode, result.Type, result.Code = 401, "authentication_error", "authentication_error"
	case status == 429 || businessStatus == 429 || containsHint(lower, []string{"积分", "余额", "额度", "quota", "insufficient", "credit", "exceeded", "limit reached", "rate limit", "too many requests"}):
		result.StatusCode, result.Type, result.Code = 429, "rate_limit_error", "rate_limit_exceeded"
	}
	return result
}

func containsHint(text string, hints []string) bool {
	for _, hint := range hints {
		if strings.Contains(text, hint) {
			return true
		}
	}
	return false
}

// NormalizeStream lazily translates native ADK SSE into OpenAI Chat SSE.
// Close closes the source directly, unblocking an in-flight network read.
func NormalizeStream(source io.ReadCloser, model string) io.ReadCloser {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	return &adkStream{source: source, scanner: scanner, model: model, id: "chatcmpl-" + uuid.NewString(), created: time.Now().Unix()}
}

type adkStream struct {
	source   io.ReadCloser
	scanner  *bufio.Scanner
	pending  []byte
	ended    bool
	terminal bool
	roleSent bool
	id       string
	model    string
	created  int64
	finish   string
	usage    map[string]any
	tools    []*adkTool
}

type adkTool struct {
	id        string
	sourceID  string
	name      string
	arguments string
}

func (r *adkStream) Close() error { return r.source.Close() }

func (r *adkStream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 && !r.ended {
		data, err := r.event()
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				r.ended = true
				return 0, err
			}
			if errors.Is(err, io.EOF) && r.terminal {
				err = r.complete()
			} else {
				if errors.Is(err, io.EOF) {
					err = io.ErrUnexpectedEOF
				}
				err = fmt.Errorf("accio stream: %w", err)
			}
			if err != nil {
				r.fail(err)
			}
			continue
		}
		if err := r.consume(data); err != nil {
			r.fail(err)
		}
	}
	if len(r.pending) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *adkStream) event() ([]byte, error) {
	var lines [][]byte
	size := 0
	for r.scanner.Scan() {
		line := r.scanner.Bytes()
		if len(line) == 0 {
			if len(lines) > 0 {
				return bytes.Join(lines, []byte{'\n'}), nil
			}
			continue
		}
		if data, ok := bytes.CutPrefix(line, []byte("data:")); ok {
			data = bytes.TrimPrefix(data, []byte{' '})
			size += len(data)
			if size > 8<<20 {
				return nil, errors.New("accio SSE event exceeds 8 MiB")
			}
			lines = append(lines, bytes.Clone(data))
		}
	}
	if err := r.scanner.Err(); err != nil {
		return nil, err
	}
	if len(lines) > 0 {
		return bytes.Join(lines, []byte{'\n'}), nil
	}
	return nil, io.EOF
}

func (r *adkStream) consume(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("[DONE]")) || bytes.Equal(data, []byte("[done]")) {
		return r.complete()
	}
	if len(data) == 0 {
		return nil
	}
	var frame map[string]any
	if !utf8.Valid(data) || json.Unmarshal(data, &frame) != nil || frame == nil {
		return errors.New("accio: invalid SSE JSON")
	}
	if code := streamField(frame, "errorCode", "error_code"); code != nil && fmt.Sprint(code) != "" && fmt.Sprint(code) != "0" || wireText(streamField(frame, "errorMessage", "error_message")) != "" || frame["error"] != nil {
		return ClassifyError(200, data)
	}
	if value := streamField(frame, "usageMetadata", "usage_metadata"); value != nil {
		usage, ok := value.(map[string]any)
		if !ok {
			return errors.New("accio: invalid usage metadata")
		}
		var err error
		r.usage, err = adkUsage(usage)
		if err != nil {
			return err
		}
	}
	if raw := frame["content"]; raw != nil {
		content, ok := raw.(map[string]any)
		if !ok {
			return errors.New("accio: invalid SSE content")
		}
		parts, ok := content["parts"].([]any)
		// Empty ADK content can omit parts or encode it as null, including
		// on terminal/usage frames. Still process their completion metadata.
		if !ok && content["parts"] != nil {
			return errors.New("accio: invalid SSE parts")
		}
		if r.terminal && len(parts) > 0 {
			return errors.New("accio: content after terminal frame")
		}
		for _, raw := range parts {
			part, ok := raw.(map[string]any)
			if !ok {
				return errors.New("accio: invalid SSE part")
			}
			if err := r.part(part); err != nil {
				return err
			}
		}
	}
	if reason := wireText(streamField(frame, "finishReason", "finish_reason")); reason != "" {
		var err error
		r.finish, err = adkFinish(reason)
		if err != nil {
			return err
		}
		r.terminal = true
	}
	if complete, _ := streamField(frame, "turnComplete", "turn_complete").(bool); complete {
		r.terminal = true
		return r.complete()
	}
	return nil
}

func (r *adkStream) part(part map[string]any) error {
	for key := range part {
		switch key {
		case "text", "thought", "thoughtSignature", "thought_signature", "functionCall", "function_call":
		default:
			return fmt.Errorf("accio: unsupported SSE part field %q", key)
		}
	}
	signature := streamField(part, "thoughtSignature", "thought_signature")
	if value := streamField(part, "functionCall", "function_call"); value != nil {
		call, ok := value.(map[string]any)
		if !ok {
			return errors.New("accio: invalid function call")
		}
		return r.tool(call, signature)
	}
	if raw, exists := part["text"]; exists {
		text, ok := raw.(string)
		if !ok {
			return errors.New("accio: invalid text part")
		}
		delta := map[string]any{"content": text}
		if thought, _ := part["thought"].(bool); thought {
			delta = map[string]any{"reasoning_content": text}
		}
		if signature != nil {
			delta["extra_content"] = map[string]any{"google": map[string]any{"thought_signature": signature}}
		}
		r.delta(delta)
		return nil
	}
	if signature != nil {
		r.delta(map[string]any{"extra_content": map[string]any{"google": map[string]any{"thought_signature": signature}}})
		return nil
	}
	return errors.New("accio: unsupported SSE content part")
}

func (r *adkStream) tool(call map[string]any, signature any) error {
	id, name := wireText(call["id"]), wireText(call["name"])
	raw := streamField(call, "argsJson", "args_json")
	if raw == nil {
		raw = call["args"]
	}
	args := ""
	if raw != nil {
		if text, ok := raw.(string); ok {
			args = text
		} else {
			encoded, err := json.Marshal(raw)
			if err != nil {
				return err
			}
			args = string(encoded)
		}
	}
	index := -1
	if id != "" {
		for i, tool := range r.tools {
			if tool.sourceID == id {
				index = i
				break
			}
		}
		// ADK can announce an ID after the first argument fragment. Keep the
		// downstream ID stable while binding that later native ID to its call.
		if index < 0 && len(r.tools) > 0 {
			i := len(r.tools) - 1
			tool := r.tools[i]
			if tool.sourceID == "" && (name == "" || name == tool.name) && !json.Valid([]byte(tool.arguments)) {
				index, tool.sourceID = i, id
			}
		}
	} else if len(r.tools) > 0 {
		i := len(r.tools) - 1
		tool := r.tools[i]
		if (name == "" || name == tool.name) && !json.Valid([]byte(tool.arguments)) {
			index = i
		}
	}
	newCall := index < 0
	if newCall {
		if name == "" {
			return errors.New("accio: new tool call has no name")
		}
		index = len(r.tools)
		sourceID := id
		if id == "" {
			id = fmt.Sprintf("call_%d", index)
		}
		r.tools = append(r.tools, &adkTool{id: id, sourceID: sourceID, name: name})
	}
	tool := r.tools[index]
	if name != "" && tool.name != name {
		return errors.New("accio: tool call changed function name")
	}
	tool.arguments += args
	function := map[string]any{"arguments": args}
	output := map[string]any{"index": index, "function": function}
	if newCall {
		output["id"], output["type"], function["name"] = tool.id, "function", tool.name
	}
	if signature != nil {
		output["extra_content"] = map[string]any{"google": map[string]any{"thought_signature": signature}}
	}
	r.delta(map[string]any{"tool_calls": []any{output}})
	return nil
}

func (r *adkStream) delta(delta map[string]any) {
	if !r.roleSent {
		r.roleSent = true
		r.chunk(map[string]any{"role": "assistant", "content": ""}, nil)
	}
	r.chunk(delta, nil)
}

func (r *adkStream) chunk(delta map[string]any, finish any) {
	r.frame(map[string]any{"id": r.id, "object": "chat.completion.chunk", "created": r.created, "model": r.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
}

func (r *adkStream) frame(value any) {
	// All values originate in decoded JSON or scalar protocol fields.
	encoded, _ := json.Marshal(value)
	r.pending = append(r.pending, "data: "...)
	r.pending = append(r.pending, encoded...)
	r.pending = append(r.pending, '\n', '\n')
}

func (r *adkStream) complete() error {
	for _, tool := range r.tools {
		var arguments map[string]any
		if json.Unmarshal([]byte(tool.arguments), &arguments) != nil || arguments == nil {
			return errors.New("accio: incomplete or invalid tool arguments at end of stream")
		}
	}
	if !r.roleSent {
		r.delta(map[string]any{})
	}
	finish := r.finish
	if finish == "" {
		finish = "stop"
		if len(r.tools) > 0 {
			finish = "tool_calls"
		}
	}
	r.chunk(map[string]any{}, finish)
	if r.usage != nil {
		r.frame(map[string]any{"id": r.id, "object": "chat.completion.chunk", "created": r.created, "model": r.model, "choices": []any{}, "usage": r.usage})
	}
	r.pending = append(r.pending, "data: [DONE]\n\n"...)
	r.ended = true
	return nil
}

func (r *adkStream) fail(err error) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		apiErr = &APIError{StatusCode: 502, Type: "upstream_error", Code: "invalid_upstream_response", Message: err.Error()}
	}
	r.frame(map[string]any{"status": apiErr.StatusCode, "error": apiErr})
	r.ended = true
}

func adkUsage(raw map[string]any) (map[string]any, error) {
	counts := make([]int64, 5)
	keys := [][2]string{{"promptTokenCount", "prompt_token_count"}, {"candidatesTokenCount", "candidates_token_count"}, {"thoughtsTokenCount", "thoughts_token_count"}, {"totalTokenCount", "total_token_count"}, {"cachedContentTokenCount", "cached_content_token_count"}}
	for i, key := range keys {
		if value := streamField(raw, key[0], key[1]); value != nil {
			number, ok := value.(float64)
			if !ok || number < 0 || number != float64(int64(number)) {
				return nil, errors.New("accio: invalid usage token count")
			}
			counts[i] = int64(number)
		}
	}
	prompt, completion, thoughts, total, cached := counts[0], counts[1], counts[2], counts[3], counts[4]
	if total == 0 {
		total = prompt + completion + thoughts
	}
	usage := map[string]any{"prompt_tokens": prompt, "completion_tokens": completion + thoughts, "total_tokens": total}
	if thoughts > 0 {
		usage["completion_tokens_details"] = map[string]any{"reasoning_tokens": thoughts}
	}
	if cached > 0 {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
	}
	return usage, nil
}

func streamField(object map[string]any, camel, snake string) any {
	if value := object[camel]; value != nil {
		return value
	}
	return object[snake]
}

func adkFinish(raw string) (string, error) {
	switch strings.ToLower(raw) {
	case "stop", "end_turn", "complete":
		return "stop", nil
	case "max_tokens", "length":
		return "length", nil
	case "tool_calls", "tool_use", "function_call":
		return "tool_calls", nil
	case "content_filter", "safety", "blocked":
		return "content_filter", nil
	default:
		return "", fmt.Errorf("accio: unsupported finish reason %q", raw)
	}
}

var _ io.ReadCloser = (*adkStream)(nil)
