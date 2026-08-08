package app

import (
	"strings"
	"sync"

	"ccLoad/internal/config"
)

const (
	defaultResponsesWebsocketConnectionLimit           = config.DefaultResponsesWebsocketMaxConnections
	defaultResponsesWebsocketConnectionPerSubjectLimit = config.DefaultResponsesWebsocketMaxConnectionsPerToken
	responsesWebsocketMaxConnectionsSetting            = "responses_ws_max_connections"
	responsesWebsocketMaxConnectionsPerTokenSetting    = "responses_ws_max_connections_per_token"
)

type responsesWebsocketConnectionLimit struct {
	scope  string
	active int
	limit  int
}

type responsesWebsocketConnectionStats struct {
	Active        int
	Rejected      uint64
	Max           int
	MaxPerSubject int
}

// responsesWebsocketConnectionLimiter limits physical downstream WebSocket
// connections. Request concurrency is deliberately accounted elsewhere: an
// idle socket consumes a file descriptor and goroutines, not an upstream turn.
type responsesWebsocketConnectionLimiter struct {
	mu              sync.Mutex
	active          int
	activeBySubject map[string]int
	max             int
	maxPerSubject   int
	rejected        uint64
}

func newResponsesWebsocketConnectionLimiter(maxConnections, maxPerSubject int) *responsesWebsocketConnectionLimiter {
	if maxConnections <= 0 {
		maxConnections = defaultResponsesWebsocketConnectionLimit
	}
	if maxPerSubject <= 0 {
		maxPerSubject = defaultResponsesWebsocketConnectionPerSubjectLimit
	}
	return &responsesWebsocketConnectionLimiter{
		activeBySubject: make(map[string]int),
		max:             maxConnections,
		maxPerSubject:   maxPerSubject,
	}
}

func (l *responsesWebsocketConnectionLimiter) acquire(subject string) (func(), *responsesWebsocketConnectionLimit) {
	if l == nil {
		return func() {}, nil
	}
	subject = strings.TrimSpace(subject)
	if subject == "" {
		subject = "<missing>"
	}

	l.mu.Lock()
	if l.max > 0 && l.active >= l.max {
		l.rejected++
		limit := &responsesWebsocketConnectionLimit{scope: "global", active: l.active, limit: l.max}
		l.mu.Unlock()
		return nil, limit
	}
	subjectActive := l.activeBySubject[subject]
	if l.maxPerSubject > 0 && subjectActive >= l.maxPerSubject {
		l.rejected++
		limit := &responsesWebsocketConnectionLimit{
			scope: "token", active: subjectActive, limit: l.maxPerSubject,
		}
		l.mu.Unlock()
		return nil, limit
	}
	l.active++
	l.activeBySubject[subject] = subjectActive + 1
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			if l.active > 0 {
				l.active--
			}
			current := l.activeBySubject[subject]
			if current <= 1 {
				delete(l.activeBySubject, subject)
			} else {
				l.activeBySubject[subject] = current - 1
			}
			l.mu.Unlock()
		})
	}, nil
}

func (l *responsesWebsocketConnectionLimiter) stats() responsesWebsocketConnectionStats {
	if l == nil {
		return responsesWebsocketConnectionStats{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return responsesWebsocketConnectionStats{
		Active:        l.active,
		Rejected:      l.rejected,
		Max:           l.max,
		MaxPerSubject: l.maxPerSubject,
	}
}
