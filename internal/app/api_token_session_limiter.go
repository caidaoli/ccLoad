package app

import (
	"sync"
	"time"
)

const (
	apiTokenSessionIssueLimit  = 5
	apiTokenSessionIssueWindow = time.Hour
)

// apiTokenSessionLimiter bounds browser-session issuance by API Token identity.
// AuthService's existing hourly cleanup removes inactive token IDs.
type apiTokenSessionLimiter struct {
	mu     sync.Mutex
	issued map[int64][]time.Time
	now    func() time.Time
}

func newAPITokenSessionLimiter(now func() time.Time) *apiTokenSessionLimiter {
	if now == nil {
		now = time.Now
	}
	return &apiTokenSessionLimiter{
		issued: make(map[int64][]time.Time),
		now:    now,
	}
}

func (l *apiTokenSessionLimiter) allow(tokenID int64) (bool, time.Duration) {
	if l == nil || tokenID <= 0 {
		return false, apiTokenSessionIssueWindow
	}

	now := l.now()
	cutoff := now.Add(-apiTokenSessionIssueWindow)

	l.mu.Lock()
	defer l.mu.Unlock()

	events := retainRecentSessionIssues(l.issued[tokenID], cutoff)
	if len(events) >= apiTokenSessionIssueLimit {
		l.issued[tokenID] = events
		retryAfter := events[0].Add(apiTokenSessionIssueWindow).Sub(now)
		if retryAfter < 0 {
			retryAfter = 0
		}
		return false, retryAfter
	}

	l.issued[tokenID] = append(events, now)
	return true, 0
}

func (l *apiTokenSessionLimiter) cleanup() {
	if l == nil {
		return
	}
	cutoff := l.now().Add(-apiTokenSessionIssueWindow)

	l.mu.Lock()
	defer l.mu.Unlock()
	for tokenID, events := range l.issued {
		events = retainRecentSessionIssues(events, cutoff)
		if len(events) == 0 {
			delete(l.issued, tokenID)
			continue
		}
		l.issued[tokenID] = events
	}
}

func retainRecentSessionIssues(events []time.Time, cutoff time.Time) []time.Time {
	kept := 0
	for _, issuedAt := range events {
		if issuedAt.After(cutoff) {
			events[kept] = issuedAt
			kept++
		}
	}
	return events[:kept]
}
