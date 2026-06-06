package builtin

import "strings"

func parseSSEEventBlock(raw string) (eventType string, data string) {
	lines := strings.Split(raw, "\n")
	dataLines := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	data = strings.TrimSpace(strings.Join(dataLines, ""))
	return eventType, data
}

func parseSSEEventBlockOrRaw(raw string) (eventType string, data string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	eventType, data = parseSSEEventBlock(raw)
	if data != "" || hasSSEField(raw) {
		return eventType, data
	}
	return "", raw
}

func hasSSEField(raw string) bool {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimLeft(strings.TrimRight(line, "\r"), " \t")
		if strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "data:") {
			return true
		}
	}
	return false
}

func isCodexResponseEventType(value string) bool {
	return strings.HasPrefix(value, "response.")
}
