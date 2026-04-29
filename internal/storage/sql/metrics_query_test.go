package sql

import (
	"strings"
	"testing"

	"ccLoad/internal/model"
)

func TestBuildLatestChannelSuccessQuery_UsesIndexedSeek(t *testing.T) {
	query, args := buildLatestChannelSuccessQuery(map[int][]int{
		3: {0},
		1: {1},
	}, &model.LogFilter{LogSource: model.LogSourceProxy})

	upperQuery := strings.ToUpper(query)
	if strings.Contains(upperQuery, "ROW_NUMBER()") || strings.Contains(upperQuery, "OVER (") {
		t.Fatalf("latest success query must not rank all historical rows:\n%s", query)
	}
	if !strings.Contains(upperQuery, "ORDER BY TIME DESC, ID DESC LIMIT 1") {
		t.Fatalf("latest success query must seek the latest indexed row per channel:\n%s", query)
	}
	if !strings.Contains(query, "channel_id = scope.channel_id") {
		t.Fatalf("latest success query must be correlated by channel scope:\n%s", query)
	}
	if len(args) != 3 {
		t.Fatalf("args=%v, want two scope channel ids plus log_source", args)
	}
}

func TestBuildLatestEntrySuccessQuery_UsesIndexedSeek(t *testing.T) {
	query, args := buildLatestEntrySuccessQuery(map[statsRequestKey]int{
		{channelID: 3, model: "gpt-4o"}: 0,
		{channelID: 1, model: "claude"}: 1,
	}, &model.LogFilter{ModelLike: "gpt", LogSource: model.LogSourceProxy})

	upperQuery := strings.ToUpper(query)
	if strings.Contains(upperQuery, "ROW_NUMBER()") || strings.Contains(upperQuery, "OVER (") {
		t.Fatalf("latest entry success query must not rank all historical rows:\n%s", query)
	}
	if !strings.Contains(upperQuery, "ORDER BY TIME DESC, ID DESC LIMIT 1") {
		t.Fatalf("latest entry success query must seek the latest indexed row per channel/model:\n%s", query)
	}
	if !strings.Contains(query, "COALESCE(model, '') = scope.model") {
		t.Fatalf("latest entry success query must be correlated by channel/model scope:\n%s", query)
	}
	if len(args) != 6 {
		t.Fatalf("args=%v, want two channel/model pairs plus model_like and log_source", args)
	}
}
