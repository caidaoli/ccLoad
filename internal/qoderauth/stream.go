package qoderauth

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"unicode/utf8"
)

// NormalizeStream unwraps native Qoder SSE on demand. Closing it closes the
// network response directly, including while another goroutine is reading.
func NormalizeStream(source io.ReadCloser) io.ReadCloser {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	return &qoderStream{source: source, scanner: scanner, choices: map[int]*qoderChoice{}}
}

type qoderStream struct {
	source   io.ReadCloser
	scanner  *bufio.Scanner
	pending  []byte
	ended    bool
	choices  map[int]*qoderChoice
	identity map[string]any
}

type qoderChoice struct {
	finished bool
	text     string
	closing  string
}

func (r *qoderStream) Close() error { return r.source.Close() }

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
				r.complete()
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

func (r *qoderStream) consume(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("[DONE]")) {
		r.complete()
		return nil
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
		r.complete()
		return nil
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
			state = &qoderChoice{}
			r.choices[index] = state
		}
		delta, ok := choice["delta"].(map[string]any)
		if !ok {
			return errors.New("SSE choice has no delta object")
		}
		finish, finished := choice["finish_reason"].(string)
		finished = finished && finish != ""
		if content, ok := delta["content"].(string); ok {
			state.text += content
			delete(delta, "content")
		}
		content, reasoning := state.drain(finished)
		if content != "" {
			delta["content"] = content
		}
		if reasoning != "" {
			existing, _ := delta["reasoning_content"].(string)
			delta["reasoning_content"] = existing + reasoning
		}
		if finished {
			state.finished = true
		}
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

func (r *qoderStream) fail(err *APIError) {
	r.ended = true
	r.frame(map[string]any{"status": err.StatusCode, "error": err})
}

func (r *qoderStream) complete() {
	indices := make([]int, 0, len(r.choices))
	for index := range r.choices {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	var choices []any
	for _, index := range indices {
		content, reasoning := r.choices[index].drain(true)
		delta := map[string]any{}
		if content != "" {
			delta["content"] = content
		}
		if reasoning != "" {
			delta["reasoning_content"] = reasoning
		}
		if len(delta) > 0 {
			choices = append(choices, map[string]any{"index": index, "delta": delta, "finish_reason": nil})
		}
	}
	if len(choices) > 0 {
		chunk := r.identity
		if chunk == nil {
			chunk = map[string]any{}
		}
		chunk["object"], chunk["choices"] = "chat.completion.chunk", choices
		r.frame(chunk)
	}
	r.pending = append(r.pending, []byte("data: [DONE]\n\n")...)
	r.ended = true
}

// drain keeps only a suffix which may be a tag split across network chunks.
// Native reasoning_content and all unrelated delta fields pass through intact.
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
