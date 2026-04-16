package protocol_test

import (
	"encoding/json"
	"strings"
	"testing"
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
