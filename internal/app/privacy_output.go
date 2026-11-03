package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/privacy"

	"github.com/tidwall/gjson"
)

// privacyResponseWriter restores the client protocol after conversion and usage
// accounting. Only possible placeholder suffixes delay SSE events. An event
// behind a delayed event waits too, so IDs, tool lifecycle and usage keep their
// original order even when independent content channels are interleaved.
type privacyResponseWriter struct {
	http.ResponseWriter
	session     *privacy.Session
	pending     []byte
	frames      []*privacyOutputFrame
	channels    map[string]*privacyOutputChannel
	queuedBytes int
	modeKnown   bool
	streaming   bool
	emitted     bool
	finished    bool
	err         error
}

func newPrivacyResponseWriter(w http.ResponseWriter, session *privacy.Session) *privacyResponseWriter {
	return &privacyResponseWriter{ResponseWriter: w, session: session, channels: make(map[string]*privacyOutputChannel)}
}

func (w *privacyResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *privacyResponseWriter) WriteHeader(status int) {
	if w.session != nil {
		w.Header().Del("Content-Length")
	}
	w.ResponseWriter.WriteHeader(status)
	if status >= http.StatusOK && responseHeaderWriteError(w.ResponseWriter) == nil {
		w.emitted = true
	}
}

func (w *privacyResponseWriter) Write(data []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.finished {
		return 0, io.ErrClosedPipe
	}
	if w.session == nil {
		n, err := w.ResponseWriter.Write(data)
		w.emitted = w.emitted || n > 0
		return n, err
	}
	w.Header().Del("Content-Length")
	w.pending = append(w.pending, data...)
	w.detectMode()
	if w.streaming {
		for {
			end := firstSSEEventEnd(w.pending)
			if end < 0 {
				break
			}
			frame := bytes.Clone(w.pending[:end])
			w.pending = w.pending[end:]
			if w.err = w.addFrame(frame); w.err != nil {
				return 0, w.err
			}
		}
		if len(w.pending)+w.queuedBytes > maxSSEEventBytes {
			w.err = errors.New("privacy response events exceed size limit")
			return 0, w.err
		}
	}
	return len(data), nil
}

func (w *privacyResponseWriter) detectMode() {
	if w.modeKnown {
		return
	}
	contentType := strings.ToLower(w.Header().Get("Content-Type"))
	if strings.Contains(contentType, "text/event-stream") {
		w.streaming, w.modeKnown = true, true
		return
	}
	if strings.Contains(contentType, "json") {
		w.modeKnown = true
		return
	}
	data := bytes.TrimSpace(w.pending)
	if len(data) == 0 {
		return
	}
	// The Responses WebSocket bridge can receive SSE before a content type is
	// set. Do not mistake a Write ending inside "data:" for a JSON response.
	for _, prefix := range []string{"data:", "event:", "id:", "retry:", ":"} {
		if bytes.HasPrefix(data, []byte(prefix)) {
			w.streaming, w.modeKnown = true, true
			return
		}
		if bytes.HasPrefix([]byte(prefix), data) {
			return
		}
	}
	w.modeKnown = true
}

func (w *privacyResponseWriter) Flush() { _ = w.FlushError() }

func (w *privacyResponseWriter) FlushError() error {
	if w.err == nil && w.emitted {
		err := http.NewResponseController(w.ResponseWriter).Flush()
		if errors.Is(err, http.ErrNotSupported) {
			return err
		}
		w.err = err
	}
	return w.err
}

func (w *privacyResponseWriter) SetWriteDeadline(deadline time.Time) error {
	return http.NewResponseController(w.ResponseWriter).SetWriteDeadline(deadline)
}

// Finish is required before inspecting a WebSocket bridge's terminal state.
// It also releases literal/incomplete placeholder prefixes on truncated streams.
func (w *privacyResponseWriter) Finish() error {
	if w.finished {
		return w.err
	}
	w.finished = true
	if w.err != nil || w.session == nil {
		return w.err
	}
	w.detectMode()
	if !w.streaming {
		if len(w.pending) == 0 {
			return nil
		}
		restored, err := w.session.RestoreJSON(w.pending)
		if err != nil && !json.Valid(w.pending) {
			// Plain-text and HTML error bodies are not JSON documents.
			restored, err = []byte(w.session.RestoreText(string(w.pending))), nil
		}
		w.err = err
		if w.err == nil {
			w.err = w.writeOutput(restored)
		}
		w.pending = nil
		return w.err
	}
	if len(w.pending) > 0 {
		frame := bytes.Clone(w.pending)
		w.pending = nil
		w.err = w.addFrame(frame)
	}
	if w.err == nil {
		w.releaseChannels("")
		w.err = w.drainFrames()
	}
	return w.err
}

func (w *privacyResponseWriter) writeOutput(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	w.Header().Del("Content-Length")
	n, err := w.ResponseWriter.Write(data)
	if n > 0 {
		w.emitted = true
	}
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}

type privacyOutputFrame struct {
	wire   []byte
	data   []byte
	fields []*privacyOutputField
	holds  int
}

type privacyOutputField struct {
	frame *privacyOutputFrame
	index int
	raw   string
	value string
	edits []privacyOutputEdit
}

type privacyOutputEdit struct {
	start, end int
	text       string
}

type privacyOutputPiece struct {
	field      *privacyOutputField
	start, end int
}

type privacyOutputChannel struct {
	pending []privacyOutputPiece
	tail    string
	// These are the lexical state of the inner tool JSON before tail. They
	// must span deltas: a fragment need not start at an opening quotation mark.
	quoted, escaped bool
	arguments       bool
}

func (w *privacyResponseWriter) addFrame(wire []byte) error {
	frame := &privacyOutputFrame{wire: wire}
	w.frames = append(w.frames, frame)
	w.queuedBytes += len(wire)
	event, data := parseSSEEventChunk(wire)
	frame.data = data
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		w.releaseChannels("")
	} else if gjson.ValidBytes(data) {
		root := gjson.ParseBytes(data)
		w.collectFields(frame, root)
		w.releaseTerminalChannels(event, root)
	}
	if w.queuedBytes > maxSSEEventBytes {
		return errors.New("privacy response events exceed size limit")
	}
	return w.drainFrames()
}

// collectField reports whether the frame continues channel with new text.
func (w *privacyResponseWriter) collectField(frame *privacyOutputFrame, value gjson.Result, channel string, arguments bool) bool {
	if value.Type != gjson.String {
		return false
	}
	field := &privacyOutputField{frame: frame, index: value.Index, raw: value.Raw, value: value.String()}
	frame.fields = append(frame.fields, field)
	state := w.channels[channel]
	if state == nil {
		state = &privacyOutputChannel{arguments: arguments}
		w.channels[channel] = state
	}
	state.feed(field, w.session)
	return field.value != ""
}

func privacyOutputIndex(value gjson.Result, fallback int) string {
	if value.Exists() {
		return value.String()
	}
	return strconv.Itoa(fallback)
}

// Signed reasoning (Anthropic thinking, Gemini thought parts) keeps its
// placeholders, matching RestoreJSON, so it is not collected.
func (w *privacyResponseWriter) collectFields(frame *privacyOutputFrame, root gjson.Result) {
	for index, choice := range root.Get("choices").Array() {
		channel := "chat/" + privacyOutputIndex(choice.Get("index"), index) + "/"
		delta := choice.Get("delta")
		var active []string
		collect := func(value gjson.Result, key string, arguments bool) {
			if w.collectField(frame, value, channel+key, arguments) {
				active = append(active, channel+key)
			}
		}
		for _, key := range []string{"content", "reasoning_content", "reasoning", "refusal"} {
			collect(delta.Get(key), key, false)
		}
		for partIndex, part := range delta.Get("content").Array() {
			collect(part.Get("text"), "content/"+strconv.Itoa(partIndex), false)
		}
		for toolIndex, tool := range delta.Get("tool_calls").Array() {
			collect(tool.Get("function.arguments"), "tool/"+privacyOutputIndex(tool.Get("index"), toolIndex), true)
		}
		collect(delta.Get("function_call.arguments"), "function", true)
		if len(active) > 0 {
			w.releaseSwitchedChannels(channel, active)
		}
	}
	for candidateIndex, candidate := range root.Get("candidates").Array() {
		// Clients concatenate a candidate's answer parts, so a placeholder may
		// continue in the next part; a thought or call part ends the text.
		channel := "gemini/" + privacyOutputIndex(candidate.Get("index"), candidateIndex) + "/text"
		for _, part := range candidate.Get("content.parts").Array() {
			if text := part.Get("text"); text.Type == gjson.String && !part.Get("thought").Bool() {
				w.collectField(frame, text, channel, false)
			} else {
				w.releaseChannels(channel)
			}
		}
	}
	index := privacyOutputIndex(root.Get("index"), 0)
	switch root.Get("type").String() {
	case "content_block_start":
		w.collectField(frame, root.Get("content_block.text"), "anthropic/"+index+"/text", false)
	case "content_block_delta":
		for _, key := range []string{"text", "partial_json"} {
			w.collectField(frame, root.Get("delta."+key), "anthropic/"+index+"/"+key, key == "partial_json")
		}
	case "response.output_text.delta", "response.refusal.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		event := root.Get("type").String()
		item := root.Get("output_index").String()
		if !root.Get("output_index").Exists() {
			item = root.Get("item_id").String()
		}
		channel := "responses/" + item + "/" + root.Get("content_index").String() + "/" + root.Get("summary_index").String() + "/" + event
		w.collectField(frame, root.Get("delta"), channel, event == "response.function_call_arguments.delta")
	}
}

func (w *privacyResponseWriter) releaseTerminalChannels(event string, root gjson.Result) {
	for index, choice := range root.Get("choices").Array() {
		if finish := choice.Get("finish_reason"); finish.Exists() && finish.Type != gjson.Null {
			w.releaseChannels("chat/" + privacyOutputIndex(choice.Get("index"), index) + "/")
		}
	}
	for index, candidate := range root.Get("candidates").Array() {
		if candidate.Get("finishReason").String() != "" {
			w.releaseChannels("gemini/" + privacyOutputIndex(candidate.Get("index"), index) + "/")
		}
	}
	if kind := root.Get("type").String(); kind != "" {
		event = kind
	}
	switch event {
	case "content_block_stop":
		w.releaseChannels("anthropic/" + privacyOutputIndex(root.Get("index"), 0) + "/")
	case "message_stop", "error", "response.completed", "response.done", "response.incomplete", "response.failed":
		w.releaseChannels("")
	case "response.output_item.done":
		item := root.Get("output_index").String()
		if !root.Get("output_index").Exists() {
			item = root.Get("item.id").String()
		}
		w.releaseChannels("responses/" + item + "/")
	}
}

// releaseSwitchedChannels ends a choice's text channels once the model writes
// elsewhere: a held placeholder prefix there can no longer be completed, and
// would otherwise delay every later event until the choice finishes. Tool
// arguments end with their call.
func (w *privacyResponseWriter) releaseSwitchedChannels(prefix string, active []string) {
	for key, channel := range w.channels {
		if strings.HasPrefix(key, prefix) && !channel.arguments && !slices.Contains(active, key) {
			channel.release()
			delete(w.channels, key)
		}
	}
}

func (w *privacyResponseWriter) releaseChannels(prefix string) {
	for key, channel := range w.channels {
		if strings.HasPrefix(key, prefix) {
			channel.release()
			delete(w.channels, key)
		}
	}
}

func (c *privacyOutputChannel) release() {
	for _, piece := range c.pending {
		piece.field.frame.holds--
	}
	c.pending, c.tail = nil, ""
}

func (c *privacyOutputChannel) feed(field *privacyOutputField, session *privacy.Session) {
	pieces := append(c.pending, privacyOutputPiece{field: field, end: len(field.value)})
	text := c.tail + field.value
	c.release()
	position := 0
	for position < len(text) {
		length, incomplete := privacyOutputPlaceholder(text[position:])
		if incomplete {
			break
		}
		if length > 0 {
			token := text[position : position+length]
			if restored := session.RestoreText(token); restored != token {
				if c.arguments && c.quoted {
					escaped, _ := json.Marshal(restored)
					restored = string(escaped[1 : len(escaped)-1])
				}
				privacyOutputReplace(pieces, position, position+length, restored)
			}
			position += length
			continue
		}
		if c.arguments {
			if c.escaped {
				c.escaped = false
			} else if c.quoted && text[position] == '\\' {
				c.escaped = true
			} else if text[position] == '"' {
				c.quoted = !c.quoted
			}
		}
		position++
	}
	if position < len(text) {
		c.tail = text[position:]
		offset := 0
		for _, piece := range pieces {
			length := piece.end - piece.start
			if offset+length > position {
				if offset < position {
					piece.start += position - offset
				}
				c.pending = append(c.pending, piece)
				piece.field.frame.holds++
			}
			offset += length
		}
	}
}

// A match can cover multiple original deltas. Put the restored value at the
// first position and remove only the matching continuation bytes in later
// fields; ordinary text never moves between events or content channels.
func privacyOutputReplace(pieces []privacyOutputPiece, start, end int, restored string) {
	offset := 0
	for _, piece := range pieces {
		length := piece.end - piece.start
		left, right := max(start, offset), min(end, offset+length)
		if left < right {
			piece.field.edits = append(piece.field.edits, privacyOutputEdit{
				start: piece.start + left - offset,
				end:   piece.start + right - offset,
				text:  restored,
			})
			restored = ""
		}
		offset += length
		if offset >= end {
			return
		}
	}
}

// Return a complete token's length, or whether the entire input could still
// become a token. The bounded grammar avoids holding arbitrary model prose.
func privacyOutputPlaceholder(text string) (int, bool) {
	if len(text) == 0 || text[0] != '{' {
		return 0, false
	}
	const prefix = "{{CCLOAD_"
	incomplete := false
	for kindLength := 1; kindLength <= 24; kindLength++ {
		length := len(prefix) + kindLength + 1 + 16 + 2
		matches := true
		for index := 0; index < min(len(text), length); index++ {
			char := text[index]
			switch {
			case index < len(prefix):
				matches = char == prefix[index]
			case index == len(prefix):
				matches = char >= 'A' && char <= 'Z'
			case index < len(prefix)+kindLength:
				matches = char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_'
			case index == len(prefix)+kindLength:
				matches = char == '_'
			case index < length-2:
				matches = char >= '0' && char <= '9' || char >= 'a' && char <= 'f'
			default:
				matches = char == '}'
			}
			if !matches {
				break
			}
		}
		if matches {
			if len(text) >= length {
				return length, false
			}
			incomplete = true
		}
	}
	return 0, incomplete
}

func (w *privacyResponseWriter) drainFrames() error {
	for len(w.frames) > 0 && w.frames[0].holds == 0 {
		frame := w.frames[0]
		data, err := frame.restore(w.session)
		if err != nil {
			return err
		}
		if err := w.writeOutput(data); err != nil {
			return err
		}
		w.queuedBytes -= len(frame.wire)
		w.frames[0] = nil
		w.frames = w.frames[1:]
	}
	return nil
}

func (f *privacyOutputFrame) restore(session *privacy.Session) ([]byte, error) {
	if !gjson.ValidBytes(f.data) {
		return f.wire, nil
	}
	var patches []privacyOutputEdit
	for _, field := range f.fields {
		if len(field.edits) == 0 {
			continue
		}
		var restored strings.Builder
		offset := 0
		for _, edit := range field.edits {
			restored.WriteString(field.value[offset:edit.start])
			restored.WriteString(edit.text)
			offset = edit.end
		}
		restored.WriteString(field.value[offset:])
		encoded, err := json.Marshal(restored.String())
		if err != nil {
			return nil, fmt.Errorf("encode restored privacy delta: %w", err)
		}
		patches = append(patches, privacyOutputEdit{start: field.index, end: field.index + len(field.raw), text: string(encoded)})
	}
	sort.Slice(patches, func(i, j int) bool { return patches[i].start < patches[j].start })
	var body bytes.Buffer
	offset := 0
	for _, patch := range patches {
		body.Write(f.data[offset:patch.start])
		body.WriteString(patch.text)
		offset = patch.end
	}
	body.Write(f.data[offset:])
	updated, err := session.RestoreJSON(body.Bytes())
	if err != nil {
		return nil, fmt.Errorf("restore privacy response event: %w", err)
	}
	if bytes.Equal(updated, f.data) {
		return f.wire, nil
	}
	return privacyOutputEventData(f.wire, updated), nil
}

// Replace only data lines. Preserve comments, IDs, event names, retry fields and
// CRLF framing. Every newline in the rewritten JSON must still have a data:
// prefix, otherwise SSE would discard the remaining JSON lines.
func privacyOutputEventData(wire, data []byte) []byte {
	var output bytes.Buffer
	wrote := false
	for _, line := range bytes.SplitAfter(wire, []byte{'\n'}) {
		if !bytes.HasPrefix(line, []byte("data:")) {
			output.Write(line)
			continue
		}
		if wrote {
			continue
		}
		wrote = true
		prefix := len("data:")
		if len(line) > prefix && line[prefix] == ' ' {
			prefix++
		}
		ending := "\n"
		if bytes.HasSuffix(line, []byte("\r\n")) {
			ending = "\r\n"
		}
		for index, dataLine := range bytes.Split(data, []byte{'\n'}) {
			if index > 0 {
				output.WriteString(ending)
			}
			output.Write(line[:prefix])
			output.Write(dataLine)
		}
		if bytes.HasSuffix(line, []byte{'\n'}) {
			output.WriteString(ending)
		}
	}
	return output.Bytes()
}
