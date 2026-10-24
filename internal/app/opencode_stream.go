package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"ccLoad/internal/protocol"
	translatorcommon "ccLoad/internal/protocol/cliproxy/common"

	"github.com/tidwall/gjson"
)

// collectOpenCodeCompletion preserves the upstream protocol while joining an
// inference stream into its non-stream JSON response. The original SSE parser
// is returned even on failure for error classification and partial usage.
func collectOpenCodeCompletion(ctx context.Context, reader io.Reader, upstream protocol.Protocol, onEvent func(*sseUsageParser)) ([]byte, *sseUsageParser, error) {
	switch upstream {
	case protocol.OpenAI:
		return collectChatCompletion(ctx, reader, onEvent, nil, "OpenCode")
	case protocol.Codex:
		return collectOpenCodeResponsesCompletion(ctx, reader, onEvent)
	case protocol.Anthropic:
		return collectOpenCodeMessagesCompletion(ctx, reader, onEvent)
	case protocol.Gemini:
		return collectOpenCodeGeminiCompletion(ctx, reader, onEvent)
	default:
		return nil, newSSEUsageParser(string(upstream)), fmt.Errorf("unsupported OpenCode stream protocol %q", upstream)
	}
}

func collectOpenCodeResponsesCompletion(ctx context.Context, reader io.Reader, onEvent func(*sseUsageParser)) ([]byte, *sseUsageParser, error) {
	parser := newSSEUsageParser(string(protocol.Codex))
	collector := newCodexNonStreamCollector(parser)
	total := 0
	err := streamTransformSSEEventsUntil(ctx, reader, discardHTTPResponseWriter{}, nil, func(raw []byte) ([][]byte, error) {
		if len(raw) > maxCodexNonStreamOutputBytes-total {
			return nil, errors.New("OpenCode completion exceeds output limit")
		}
		total += len(raw)
		if err := collector.consume(raw); err != nil {
			return nil, err
		}
		if onEvent != nil {
			onEvent(parser)
		}
		return nil, collector.err
	}, collector.done)
	if err != nil {
		return nil, parser, err
	}
	if parser.GetLastError() != nil {
		return nil, parser, errors.New("OpenCode upstream stream error")
	}
	if len(collector.terminal) == 0 {
		return nil, parser, errors.New("OpenCode stream ended without response.completed or response.incomplete")
	}
	response := gjson.GetBytes(collector.patchedTerminal(), "response")
	if !response.IsObject() {
		return nil, parser, errors.New("OpenCode terminal event is missing response")
	}
	return []byte(response.Raw), parser, nil
}

func collectOpenCodeMessagesCompletion(ctx context.Context, reader io.Reader, onEvent func(*sseUsageParser)) ([]byte, *sseUsageParser, error) {
	parser := newSSEUsageParser(string(protocol.Anthropic))
	var transcript bytes.Buffer
	var stopped bool
	err := streamTransformSSEEventsUntil(ctx, reader, discardHTTPResponseWriter{}, nil, func(raw []byte) ([][]byte, error) {
		if len(raw) > maxCodexNonStreamOutputBytes-transcript.Len() {
			return nil, errors.New("OpenCode completion exceeds output limit")
		}
		transcript.Write(raw)
		if err := parser.Feed(raw); err != nil {
			return nil, err
		}
		if onEvent != nil {
			onEvent(parser)
		}
		if parser.GetLastError() != nil {
			return nil, errors.New("OpenCode upstream stream error")
		}
		eventType, data := parseSSEEventChunk(raw)
		payloadType := gjson.GetBytes(data, "type").String()
		stopped = payloadType == "message_stop" || (payloadType == "" && eventType == "message_stop")
		return nil, nil
	}, func() bool { return stopped })
	if err != nil {
		return nil, parser, err
	}
	// message_delta.stop_reason 已是终态，message_stop 只是收尾。
	if !parser.IsStreamComplete() {
		return nil, parser, errors.New("OpenCode stream ended without stop_reason")
	}
	body, err := translatorcommon.NormalizeAnthropicResponse(transcript.Bytes())
	if err != nil {
		return nil, parser, err
	}
	// The common normalizer merges usage snapshots literally. Compatible
	// upstreams often send placeholder zeros in message_delta; retain the same
	// positive-snapshot and cache-bucket semantics as the live SSE usage parser.
	for _, field := range []struct {
		path  string
		value int
	}{
		{"usage.input_tokens", parser.InputTokens},
		{"usage.output_tokens", parser.OutputTokens},
		{"usage.cache_read_input_tokens", parser.CacheReadInputTokens},
		{"usage.cache_creation_input_tokens", parser.CacheCreationInputTokens},
	} {
		if field.value > 0 {
			body = setJSONRaw(body, field.path, strconv.Itoa(field.value))
		}
	}
	if parser.Cache5mInputTokens+parser.Cache1hInputTokens > 0 {
		body = setJSONRaw(body, "usage.cache_creation.ephemeral_5m_input_tokens", strconv.Itoa(parser.Cache5mInputTokens))
		body = setJSONRaw(body, "usage.cache_creation.ephemeral_1h_input_tokens", strconv.Itoa(parser.Cache1hInputTokens))
	}
	return body, parser, nil
}

type openCodeGeminiCandidate struct {
	role   string
	parts  []openCodeGeminiPart
	fields []string // other candidate members in first-seen order
	values map[string]string
}

// A text part has raw == ""; any other part is kept verbatim.
type openCodeGeminiPart struct {
	raw       string
	text      []byte
	thought   bool
	signature string
}

// collectOpenCodeGeminiCompletion joins streamGenerateContent chunks into one
// generateContent response. Adjacent text parts of one kind merge until a
// thought signature closes them; function calls and other parts stay
// separate. Later chunks win for metadata such as usage and finishReason.
func collectOpenCodeGeminiCompletion(ctx context.Context, reader io.Reader, onEvent func(*sseUsageParser)) ([]byte, *sseUsageParser, error) {
	parser := newSSEUsageParser(string(protocol.Gemini))
	var fields []string
	values := make(map[string]string)
	var order []int64
	candidates := make(map[int64]*openCodeGeminiCandidate)
	finished := false
	total := 0
	err := streamTransformSSEEventsUntil(ctx, reader, discardHTTPResponseWriter{}, nil, func(raw []byte) ([][]byte, error) {
		if len(raw) > maxCodexNonStreamOutputBytes-total {
			return nil, errors.New("OpenCode completion exceeds output limit")
		}
		total += len(raw)
		if err := parser.Feed(raw); err != nil {
			return nil, err
		}
		if onEvent != nil {
			onEvent(parser)
		}
		if parser.GetLastError() != nil {
			return nil, errors.New("OpenCode upstream stream error")
		}
		data := sseEventData(raw)
		if len(data) == 0 {
			return nil, nil
		}
		chunk := gjson.ParseBytes(data)
		if !chunk.IsObject() {
			return nil, errors.New("invalid OpenCode Gemini stream event")
		}
		chunk.ForEach(func(key, value gjson.Result) bool {
			if key.String() != "candidates" {
				fields = appendOpenCodeGeminiField(fields, values, key.String(), value.Raw)
				return true
			}
			for _, item := range value.Array() {
				index := item.Get("index").Int()
				candidate := candidates[index]
				if candidate == nil {
					candidate = &openCodeGeminiCandidate{values: make(map[string]string)}
					candidates[index] = candidate
					order = append(order, index)
				}
				candidate.add(item)
				finished = finished || item.Get("finishReason").String() != ""
			}
			return true
		})
		finished = finished || chunk.Get("promptFeedback.blockReason").String() != ""
		return nil, nil
	}, nil)
	if err != nil {
		return nil, parser, err
	}
	if !finished {
		return nil, parser, errors.New("OpenCode stream ended without finishReason")
	}
	var out bytes.Buffer
	out.WriteString(`{"candidates":[`)
	for i, index := range order {
		if i > 0 {
			out.WriteByte(',')
		}
		candidates[index].renderTo(&out)
	}
	out.WriteByte(']')
	for _, key := range fields {
		out.WriteString("," + jsonEscapedString(key) + ":" + values[key])
	}
	out.WriteByte('}')
	return out.Bytes(), parser, nil
}

func appendOpenCodeGeminiField(fields []string, values map[string]string, key, raw string) []string {
	if _, seen := values[key]; !seen {
		fields = append(fields, key)
	}
	values[key] = raw
	return fields
}

func (c *openCodeGeminiCandidate) add(item gjson.Result) {
	item.ForEach(func(key, value gjson.Result) bool {
		if key.String() != "content" {
			c.fields = appendOpenCodeGeminiField(c.fields, c.values, key.String(), value.Raw)
			return true
		}
		if role := value.Get("role").String(); role != "" && c.role == "" {
			c.role = role
		}
		for _, part := range value.Get("parts").Array() {
			c.addPart(part)
		}
		return true
	})
}

func (c *openCodeGeminiCandidate) addPart(part gjson.Result) {
	text := part.Get("text")
	isText := text.Type == gjson.String
	part.ForEach(func(key, _ gjson.Result) bool {
		switch key.String() {
		case "text", "thought", "thoughtSignature":
			return true
		}
		isText = false
		return false
	})
	if !isText {
		c.parts = append(c.parts, openCodeGeminiPart{raw: part.Raw})
		return
	}
	thought := part.Get("thought").Bool()
	if n := len(c.parts); n == 0 || c.parts[n-1].raw != "" || c.parts[n-1].thought != thought || c.parts[n-1].signature != "" {
		c.parts = append(c.parts, openCodeGeminiPart{thought: thought})
	}
	last := &c.parts[len(c.parts)-1]
	last.text = append(last.text, text.String()...)
	last.signature = part.Get("thoughtSignature").String()
}

func (c *openCodeGeminiCandidate) renderTo(out *bytes.Buffer) {
	role := c.role
	if role == "" {
		role = "model"
	}
	out.WriteString(`{"content":{"role":` + jsonEscapedString(role) + `,"parts":[`)
	for i := range c.parts {
		if i > 0 {
			out.WriteByte(',')
		}
		part := &c.parts[i]
		if part.raw != "" {
			out.WriteString(part.raw)
			continue
		}
		out.WriteString(`{"text":` + jsonEscapedString(string(part.text)))
		if part.thought {
			out.WriteString(`,"thought":true`)
		}
		if part.signature != "" {
			out.WriteString(`,"thoughtSignature":` + jsonEscapedString(part.signature))
		}
		out.WriteByte('}')
	}
	out.WriteString("]}")
	for _, key := range c.fields {
		out.WriteString("," + jsonEscapedString(key) + ":" + c.values[key])
	}
	out.WriteByte('}')
}
