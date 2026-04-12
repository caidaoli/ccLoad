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
