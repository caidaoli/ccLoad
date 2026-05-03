package app

import (
	"context"
	"strings"
	"time"
)

type groupRouteContextKey string

const (
	groupRouteSessionContextKey   groupRouteContextKey = "group-route-session"
	groupFirstByteTimeoutContextKey groupRouteContextKey = "group-first-byte-timeout"
)

type groupRouteSession struct {
	tokenHash    string
	requestModel string
}

func withGroupRouteSession(ctx context.Context, tokenHash, requestModel string) context.Context {
	tokenHash = strings.TrimSpace(tokenHash)
	requestModel = strings.TrimSpace(requestModel)
	if tokenHash == "" || requestModel == "" {
		return ctx
	}
	return context.WithValue(ctx, groupRouteSessionContextKey, groupRouteSession{
		tokenHash:    tokenHash,
		requestModel: requestModel,
	})
}

func groupRouteSessionFromContext(ctx context.Context) (groupRouteSession, bool) {
	if ctx == nil {
		return groupRouteSession{}, false
	}
	session, ok := ctx.Value(groupRouteSessionContextKey).(groupRouteSession)
	if !ok || session.tokenHash == "" || session.requestModel == "" {
		return groupRouteSession{}, false
	}
	return session, true
}

func withGroupFirstByteTimeout(ctx context.Context, timeout time.Duration) context.Context {
	if timeout <= 0 {
		return ctx
	}
	return context.WithValue(ctx, groupFirstByteTimeoutContextKey, timeout)
}

func groupFirstByteTimeoutFromContext(ctx context.Context) time.Duration {
	if ctx == nil {
		return 0
	}
	timeout, ok := ctx.Value(groupFirstByteTimeoutContextKey).(time.Duration)
	if !ok || timeout <= 0 {
		return 0
	}
	return timeout
}
