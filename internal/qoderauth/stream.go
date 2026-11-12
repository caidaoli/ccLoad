package qoderauth

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// NormalizeStream unwraps native Qoder SSE and recognizes declared tools.
/**
 * @brief NormalizeStream unwraps native Qoder SSE on demand and recognizes declared tools.
 * Closing it closes the network response directly, including during a concurrent read.
 * @param source Native upstream response body.
 * @param tools Declared Chat function schemas.
 * @return Lazy normalized Chat SSE reader.
 */
func NormalizeStream(source io.ReadCloser, tools []map[string]any) io.ReadCloser {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	return &qoderStream{source: source, scanner: scanner, tools: tools, choices: map[int]*qoderChoice{}}
}

/**
 * @brief qoderStream maintains independent reasoning and tool state for each choice.
 */
type qoderStream struct {
	source   io.ReadCloser
	scanner  *bufio.Scanner
	pending  []byte
	ended    bool
	choices  map[int]*qoderChoice
	identity map[string]any
	usage    map[string]any
	tools    []map[string]any
	said     bool
}

/**
 * @brief qoderChoice tracks native calls and buffered text belonging to one choice.
 */
type qoderChoice struct {
	finished       bool
	deferredFinish string
	role           string
	text           string
	closing        string
	tools          qoderTextTools
	native         map[int]*qoderNativeTool
	begun          []*qoderNativeTool
	nextTool       int
	sawTool        bool
}

/**
 * @brief qoderNativeTool withholds arguments until the upstream supplies a name.
 */
type qoderNativeTool struct {
	id        string
	arguments string
	index     int
	named     bool
}

func (r *qoderStream) Close() error { return r.source.Close() }

/**
 * @brief Read normalizes complete SSE events and surfaces malformed or truncated streams.
 * @param p Destination buffer.
 * @return Bytes copied and any cancellation or terminal read error.
 */
func (r *qoderStream) Read(p []byte) (int, error) {
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
			if errors.Is(err, io.EOF) && r.finished() {
				if err := r.complete(); err != nil {
					r.fail(&APIError{StatusCode: 502, Type: "upstream_error", Code: "invalid_upstream_response", Message: "qoder stream: " + err.Error()})
				}
			} else {
				if errors.Is(err, io.EOF) {
					err = io.ErrUnexpectedEOF
				}
				r.fail(&APIError{StatusCode: 502, Type: "upstream_error", Code: "upstream_stream_error", Message: "qoder stream: " + err.Error()})
			}
			continue
		}
		if err := r.consume(data); err != nil {
			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				apiErr = &APIError{StatusCode: 502, Type: "upstream_error", Code: "invalid_upstream_response", Message: "qoder stream: " + err.Error()}
			}
			r.fail(apiErr)
		}
	}
	if len(r.pending) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *qoderStream) event() ([]byte, error) {
	var lines [][]byte
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
			lines = append(lines, bytes.Clone(data))
		}
	}
	if err := r.scanner.Err(); err != nil {
		return nil, err
	}
	// A final complete JSON event need not have a terminating blank line.
	if len(lines) > 0 {
		return bytes.Join(lines, []byte{'\n'}), nil
	}
	return nil, io.EOF
}

/**
 * @brief consume validates one upstream envelope and normalizes every choice.
 * @param data Complete native SSE event data.
 * @return Invalid upstream data or tool-call error.
 */
func (r *qoderStream) consume(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("[DONE]")) {
		return r.complete()
	}
	if !utf8.Valid(data) {
		return errors.New("invalid UTF-8 in SSE event")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil || envelope == nil {
		return errors.New("invalid SSE envelope JSON")
	}
	if statusJSON, ok := envelope["statusCodeValue"]; ok {
		var status int
		if json.Unmarshal(statusJSON, &status) != nil {
			return errors.New("invalid SSE envelope status")
		}
		if status != 200 {
			return ClassifyError(status, data)
		}
	}
	body, ok := envelope["body"]
	if !ok {
		return errors.New("SSE envelope has no body")
	}
	if bytes.Equal(body, []byte("null")) {
		return nil // Gateway heartbeat, not a completion.
	}
	var text string
	if json.Unmarshal(body, &text) == nil {
		body = []byte(text)
	}
	if bytes.Equal(bytes.TrimSpace(body), []byte("[DONE]")) {
		return r.complete()
	}
	var chunk map[string]any
	if !utf8.Valid(body) || json.Unmarshal(body, &chunk) != nil || chunk == nil {
		return errors.New("invalid SSE body JSON")
	}
	if _, ok := chunk["error"]; ok {
		return ClassifyError(502, body)
	}
	choices, hasChoices := chunk["choices"].([]any)
	_, hasUsage := chunk["usage"].(map[string]any)
	if !hasChoices && !hasUsage {
		return ClassifyError(502, body)
	}
	if !hasChoices {
		chunk["choices"] = []any{}
	}
	if chunk["object"] == nil {
		chunk["object"] = "chat.completion.chunk"
	}
	if r.identity == nil {
		r.identity = map[string]any{}
	}
	for _, key := range []string{"id", "model", "created", "system_fingerprint"} {
		if value, ok := chunk[key]; ok {
			r.identity[key] = value
		}
	}
	frameFinished := false
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			return errors.New("invalid SSE choice")
		}
		index := 0
		if value, ok := choice["index"].(float64); ok {
			if value != float64(int(value)) || value < 0 {
				return errors.New("invalid SSE choice index")
			}
			index = int(value)
		}
		choice["index"] = index
		state := r.choices[index]
		if state == nil {
			state = &qoderChoice{tools: newQoderTextTools(r.tools), native: map[int]*qoderNativeTool{}}
			r.choices[index] = state
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			return errors.New("SSE choice has no delta object")
		}
		if role, _ := delta["role"].(string); role != "" {
			state.role = role
		}
		finish, finished := choice["finish_reason"].(string)
		finished = finished && finish != ""
		if content, ok := delta["content"].(string); ok {
			state.text += content
			delete(delta, "content")
		}
		content, reasoning := state.drain(finished)
		content, textCalls, err := state.tools.feed(content, finished)
		if err != nil {
			return err
		}
		if content != "" {
			delta["content"] = content
		}
		if reasoning != "" {
			existing, _ := delta["reasoning_content"].(string)
			delta["reasoning_content"] = existing + reasoning
		}
		calls := state.textCalls(textCalls)
		if rawCalls, exists := delta["tool_calls"]; exists {
			native, err := state.nativeCalls(rawCalls)
			if err != nil {
				return err
			}
			calls = append(calls, native...)
			delete(delta, "tool_calls")
		}
		if len(calls) > 0 {
			delta["tool_calls"] = calls
		}
		if text, _ := delta["reasoning_content"].(string); content != "" || text != "" || len(calls) > 0 {
			r.said = true
		}
		if finished {
			state.finished = true
			if finish == "stop" || finish == "tool_calls" {
				if state.sawTool {
					choice["finish_reason"] = "tool_calls"
				} else if finish == "tool_calls" {
					choice["finish_reason"] = "stop"
				}
			}
			if !r.said {
				for _, call := range state.begun {
					if !call.named {
						state.deferredFinish = choice["finish_reason"].(string)
						choice["finish_reason"] = nil
						break
					}
				}
			}
			frameFinished = frameFinished || choice["finish_reason"] != nil
		}
	}
	if !r.said && !frameFinished {
		if usage, ok := chunk["usage"].(map[string]any); ok {
			r.usage = usage
		}
		return nil
	}
	for _, raw := range choices {
		choice := raw.(map[string]any)
		state := r.choices[choice["index"].(int)]
		if state.role != "" {
			choice["delta"].(map[string]any)["role"] = state.role
			state.role = ""
		}
	}
	for key, value := range r.identity {
		if _, exists := chunk[key]; !exists {
			chunk[key] = value
		}
	}
	if r.usage != nil {
		if !hasUsage {
			chunk["usage"] = r.usage
		}
		r.usage = nil
	}
	r.frame(chunk)
	return nil
}

func (r *qoderStream) finished() bool {
	if len(r.choices) == 0 {
		return false
	}
	for _, choice := range r.choices {
		if !choice.finished {
			return false
		}
	}
	return true
}

func (r *qoderStream) frame(value any) {
	data, err := json.Marshal(value)
	if err != nil {
		// Values come exclusively from decoded JSON or our fixed error structs.
		r.fail(&APIError{StatusCode: 502, Type: "upstream_error", Code: "invalid_upstream_response", Message: err.Error()})
		return
	}
	r.pending = append(r.pending, []byte("data: ")...)
	r.pending = append(r.pending, data...)
	r.pending = append(r.pending, '\n', '\n')
}

/**
 * @brief fail emits the public SSE error envelope, including upstream retry guidance.
 * @param err Classified upstream failure.
 */
func (r *qoderStream) fail(err *APIError) {
	r.ended = true
	frame := map[string]any{"status": err.StatusCode, "error": err}
	if err.RetryAfter > 0 {
		frame["headers"] = map[string]string{"Retry-After": strconv.Itoa(err.RetryAfter)}
	}
	r.frame(frame)
}

/**
 * @brief complete flushes buffered calls before accepting a real upstream terminal event.
 * @return An incomplete or nameless-only tool-call error.
 */
func (r *qoderStream) complete() error {
	indices := make([]int, 0, len(r.choices))
	for index := range r.choices {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	var choices []any
	nameless := false
	for _, index := range indices {
		state := r.choices[index]
		content, reasoning := state.drain(true)
		content, calls, err := state.tools.feed(content, true)
		if err != nil {
			return err
		}
		delta := map[string]any{}
		if content != "" {
			delta["content"] = content
		}
		if reasoning != "" {
			delta["reasoning_content"] = reasoning
		}
		if len(calls) > 0 {
			delta["tool_calls"] = state.textCalls(calls)
		}
		if len(delta) > 0 {
			r.said = true
		}
		for _, call := range state.begun {
			nameless = nameless || !call.named
		}
		var finish any
		if state.deferredFinish != "" {
			finish = state.deferredFinish
		} else if !state.finished {
			// EOF reaches complete only after every choice has a native finish.
			finish = "stop"
			if state.sawTool {
				finish = "tool_calls"
			}
		}
		if len(delta) > 0 || finish != nil {
			if state.role != "" {
				delta["role"] = state.role
			}
			choices = append(choices, map[string]any{"index": index, "delta": delta, "finish_reason": finish})
		}
	}
	if nameless && !r.said {
		return errors.New("native tool call has no name")
	}
	if len(choices) > 0 || r.usage != nil {
		chunk := r.identity
		if chunk == nil {
			chunk = map[string]any{}
		}
		chunk["object"], chunk["choices"] = "chat.completion.chunk", choices
		if r.usage != nil {
			chunk["usage"] = r.usage
		}
		r.frame(chunk)
	}
	r.pending = append(r.pending, []byte("data: [DONE]\n\n")...)
	r.ended = true
	return nil
}

/**
 * @brief textCalls assigns each extracted call a stable output index and identity.
 * @param calls Complete extracted text calls.
 * @return Chat tool-call deltas with unique indices and IDs.
 */
func (c *qoderChoice) textCalls(calls []qoderTextCall) []any {
	var output []any
	for _, call := range calls {
		output = append(output, map[string]any{"index": c.nextTool, "id": "call_" + uuid.NewString(), "type": "function", "function": map[string]any{"name": call.name, "arguments": call.arguments}})
		c.nextTool++
		c.sawTool = true
	}
	return output
}

/**
 * @brief nativeCalls separates reused indices and never emits unnamed tool fragments.
 * @param raw Decoded upstream tool_calls value.
 * @return Named call deltas and any invalid wire-shape error.
 */
func (c *qoderChoice) nativeCalls(raw any) ([]any, error) {
	calls, ok := raw.([]any)
	if !ok {
		return nil, errors.New("invalid native tool calls")
	}
	var output []any
	for _, rawCall := range calls {
		call, ok := rawCall.(map[string]any)
		if !ok {
			return nil, errors.New("invalid native tool call")
		}
		index := 0
		if value, exists := call["index"]; exists {
			number, ok := value.(float64)
			if !ok || number < 0 || number != float64(int(number)) {
				return nil, errors.New("invalid native tool call index")
			}
			index = int(number)
		}
		id, name, arguments := "", "", ""
		if value, exists := call["id"]; exists {
			id, ok = value.(string)
			if !ok {
				return nil, errors.New("invalid native tool call id")
			}
		}
		if value, exists := call["function"]; exists {
			function, valid := value.(map[string]any)
			if !valid {
				return nil, errors.New("invalid native tool function")
			}
			if value, exists := function["name"]; exists {
				name, ok = value.(string)
				if !ok {
					return nil, errors.New("invalid native tool name")
				}
				name = strings.TrimSpace(name)
			}
			if value, exists := function["arguments"]; exists {
				arguments, ok = value.(string)
				if !ok {
					return nil, errors.New("invalid native tool arguments")
				}
			}
		}
		if id == "" && name == "" && arguments == "" {
			continue
		}
		state := c.native[index]
		if state == nil || (id != "" && state.id != "" && id != state.id) {
			state = &qoderNativeTool{}
			c.native[index] = state
			c.begun = append(c.begun, state)
		}
		if id != "" {
			state.id = id
		}
		if !state.named {
			state.arguments += arguments
			if name == "" {
				continue
			}
			state.named, state.index = true, c.nextTool
			c.nextTool++
			c.sawTool = true
			outputID := state.id
			if outputID == "" {
				outputID = "call_" + uuid.NewString()
			}
			output = append(output, map[string]any{"index": state.index, "id": outputID, "type": "function", "function": map[string]any{"name": name, "arguments": state.arguments}})
			state.arguments = ""
		} else if arguments != "" {
			output = append(output, map[string]any{"index": state.index, "function": map[string]any{"arguments": arguments}})
		}
	}
	return output, nil
}

/**
 * @brief drain keeps only a suffix which may be a tag split across network chunks.
 * Native reasoning_content and all unrelated delta fields pass through intact.
 * @param final Whether buffered tag suffixes must be flushed.
 * @return Ordinary assistant content and extracted reasoning.
 */
func (c *qoderChoice) drain(final bool) (string, string) {
	var content, reasoning strings.Builder
	emit := func(text string) {
		if c.closing == "" {
			content.WriteString(text)
		} else {
			reasoning.WriteString(text)
		}
	}
	for c.text != "" {
		tags := []string{c.closing}
		if c.closing == "" {
			tags = []string{"<think>", "<thinking>", "<reasoning>", "<thought>"}
		}
		at, found := -1, ""
		for _, tag := range tags {
			if pos := strings.Index(c.text, tag); pos >= 0 && (at < 0 || pos < at) {
				at, found = pos, tag
			}
		}
		if at >= 0 {
			emit(c.text[:at])
			c.text = c.text[at+len(found):]
			if c.closing == "" {
				c.closing = "</" + found[1:]
			} else {
				c.closing = ""
			}
			continue
		}
		keep := 0
		if !final {
			for _, tag := range tags {
				for n := 1; n < len(tag) && n <= len(c.text); n++ {
					if strings.HasSuffix(c.text, tag[:n]) {
						keep = max(keep, n)
					}
				}
			}
		}
		emit(c.text[:len(c.text)-keep])
		c.text = c.text[len(c.text)-keep:]
		break
	}
	return content.String(), reasoning.String()
}

var _ io.ReadCloser = (*qoderStream)(nil)
