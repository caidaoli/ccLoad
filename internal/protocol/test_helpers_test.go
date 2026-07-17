package protocol_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ccLoad/internal/protocol"
	"ccLoad/internal/protocol/builtin"
)

type sseEvent struct {
	Event   string
	RawData string
	Data    map[string]any
}

func parseSSEEvents(t *testing.T, stream string) []sseEvent {
	t.Helper()

	blocks := strings.Split(stream, "\n\n")
	events := make([]sseEvent, 0, len(blocks))
	for _, block := range blocks {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}

		var event sseEvent
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event.Event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				event.RawData = strings.TrimPrefix(line, "data: ")
			}
		}

		if event.RawData != "" && event.RawData != "[DONE]" {
			if err := json.Unmarshal([]byte(event.RawData), &event.Data); err != nil {
				t.Fatalf("unmarshal SSE payload %q: %v", event.RawData, err)
			}
		}

		events = append(events, event)
	}

	return events
}

func mustSSEEventData(t *testing.T, stream, eventName string) map[string]any {
	t.Helper()
	for _, event := range parseSSEEvents(t, stream) {
		if event.Event == eventName {
			return event.Data
		}
	}
	t.Fatalf("missing SSE event %q in:\n%s", eventName, stream)
	return nil
}

func translateResponseStreamChunks(t *testing.T, reg *protocol.Registry, source, target protocol.Protocol, model string, chunks ...string) string {
	t.Helper()

	var state any
	var allOutput bytes.Buffer
	for _, chunk := range chunks {
		out, err := reg.TranslateResponseStream(context.Background(), source, target, model, nil, nil, []byte(chunk), &state)
		if err != nil {
			t.Fatalf("TranslateResponseStream failed: %v", err)
		}
		for _, b := range out {
			allOutput.Write(b)
		}
	}
	return allOutput.String()
}

func assertAnthropicStreamTextTranslation(t *testing.T, source protocol.Protocol, model string, rawReq, translatedReq []byte, textChunk, doneChunk, wantText string) {
	t.Helper()

	reg := protocol.NewRegistry()
	builtin.Register(reg)

	var state any
	chunks, err := reg.TranslateResponseStream(context.Background(), source, protocol.Anthropic, model, rawReq, translatedReq, []byte(textChunk), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream failed: %v", err)
	}
	joined := string(bytes.Join(chunks, nil))
	if !strings.Contains(joined, "event: message_start") || !strings.Contains(joined, "event: content_block_delta") || !strings.Contains(joined, `"text":"`+wantText+`"`) {
		t.Fatalf("unexpected translated stream chunks: %#v", chunks)
	}

	done, err := reg.TranslateResponseStream(context.Background(), source, protocol.Anthropic, model, rawReq, translatedReq, []byte(doneChunk), &state)
	if err != nil {
		t.Fatalf("TranslateResponseStream done failed: %v", err)
	}
	doneJoined := string(bytes.Join(done, nil))
	if !strings.Contains(doneJoined, "event: message_delta") || !strings.Contains(doneJoined, "event: message_stop") {
		t.Fatalf("unexpected anthropic done chunks: %#v", done)
	}
}

func mustJSONMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal JSON payload: %v\nraw=%s", err, raw)
	}
	return payload
}

func mustMap(t *testing.T, value any) map[string]any {
	t.Helper()

	out, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T (%#v)", value, value)
	}
	return out
}

func mustSlice(t *testing.T, value any) []any {
	t.Helper()

	out, ok := value.([]any)
	if !ok {
		t.Fatalf("expected []any, got %T (%#v)", value, value)
	}
	return out
}

func mustString(t *testing.T, value any) string {
	t.Helper()

	out, ok := value.(string)
	if !ok {
		t.Fatalf("expected string, got %T (%#v)", value, value)
	}
	return out
}

func mustInt(t *testing.T, value any) int {
	t.Helper()

	switch n := value.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		t.Fatalf("expected numeric value, got %T (%#v)", value, value)
		return 0
	}
}

func protocolTestContentText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		var text strings.Builder
		for _, rawPart := range value {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			if partText, ok := part["text"].(string); ok {
				text.WriteString(partText)
			}
		}
		return text.String()
	default:
		return ""
	}
}
