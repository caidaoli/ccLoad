package oauthcost

import (
	"strings"
	"time"
)

// ResetWindows starts counters only for explicitly confirmed quota identities.
// A missing window keeps a durable barrier until its first upstream sample.
func ResetWindows(current *Usage, keys []string, resetAt time.Time) *Usage {
	next := Clone(current)
	if next == nil {
		next = &Usage{}
	}
	if resetAt.IsZero() || resetAt.Before(next.EpochTime()) {
		return next
	}
	if next.WindowResetAtUnixNano == nil {
		next.WindowResetAtUnixNano = make(map[string]int64)
	}
	for _, key := range keys {
		key = strings.TrimSpace(key)
		if key == "" || next.WindowResetAtUnixNano[key] > resetAt.UnixNano() {
			continue
		}
		next.WindowResetAtUnixNano[key] = resetAt.UnixNano()
		for index, window := range next.Windows {
			if window == nil || window.Key != key {
				continue
			}
			reset := Reset(&Usage{Windows: []*Window{window}}, resetAt)
			next.Windows[index] = reset.Windows[0]
		}
	}
	return next
}

func applyWindowResets(usage *Usage) {
	for _, window := range usage.Windows {
		if window == nil {
			continue
		}
		cutoff := usage.WindowResetAtUnixNano[window.Key] / int64(time.Second)
		if cutoff <= 0 {
			continue
		}
		if window.Rollback != nil && window.Rollback.PreviousCountFrom < cutoff {
			window.Rollback = nil
		}
		if window.StartedAt < cutoff {
			window.CountFromAt = max(window.CountFromAt, cutoff)
		}
	}
}
