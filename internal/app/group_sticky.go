package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/model"
)

type groupStickySession struct {
	ChannelID int64
	UpdatedAt time.Time
}

var groupStickySessions sync.Map

func groupStickySessionKey(tokenHash, requestModel string) string {
	return fmt.Sprintf("%s::%s", strings.TrimSpace(tokenHash), strings.TrimSpace(requestModel))
}

func setGroupStickySession(tokenHash, requestModel string, channelID int64) {
	if channelID <= 0 {
		return
	}
	tokenHash = strings.TrimSpace(tokenHash)
	requestModel = strings.TrimSpace(requestModel)
	if tokenHash == "" || requestModel == "" {
		return
	}
	groupStickySessions.Store(groupStickySessionKey(tokenHash, requestModel), groupStickySession{
		ChannelID: channelID,
		UpdatedAt: time.Now(),
	})
}

func getGroupStickySession(ctx context.Context, group *model.Group) (*groupStickySession, bool) {
	if group == nil || group.SessionKeepTime <= 0 {
		return nil, false
	}
	session, ok := groupRouteSessionFromContext(ctx)
	if !ok {
		return nil, false
	}
	key := groupStickySessionKey(session.tokenHash, session.requestModel)
	raw, ok := groupStickySessions.Load(key)
	if !ok {
		return nil, false
	}
	entry, ok := raw.(groupStickySession)
	if !ok || entry.ChannelID <= 0 {
		groupStickySessions.Delete(key)
		return nil, false
	}
	if time.Since(entry.UpdatedAt) > time.Duration(group.SessionKeepTime)*time.Second {
		groupStickySessions.Delete(key)
		return nil, false
	}
	entryCopy := entry
	return &entryCopy, true
}
