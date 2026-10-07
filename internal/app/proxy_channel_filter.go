package app

import (
	"net/http"
	"strconv"
	"strings"

	"ccLoad/internal/model"
)

// requestedChannelFilter 描述客户端在请求中显式指定的渠道约束
type requestedChannelFilter struct {
	hasFilter   bool
	channelID   int64
	channelName string
}

// extractRequestedChannelFilter 仅从 HTTP Header 中解析指定的渠道限制。
// 规范 header 形式为小写的：
// - x-ccload-channel-id (或标准大小写 X-CCLoad-Channel-ID)
// - x-ccload-channel    (或标准大小写 X-CCLoad-Channel)
// 优先使用 ID，其次使用 Name（若 Name 传纯数字则兼容作为 ID 处理）。
func extractRequestedChannelFilter(req *http.Request) requestedChannelFilter {
	if req == nil {
		return requestedChannelFilter{}
	}

	// 1. 尝试从 Header 获取 Channel ID
	for _, h := range []string{"x-ccload-channel-id", "X-CCLoad-Channel-ID"} {
		if val := strings.TrimSpace(req.Header.Get(h)); val != "" {
			if id, err := strconv.ParseInt(val, 10, 64); err == nil && id > 0 {
				return requestedChannelFilter{hasFilter: true, channelID: id}
			}
		}
	}

	// 2. 尝试从 Header 获取 Channel Name（若传入纯数字则兼作为 ID 处理）
	for _, h := range []string{"x-ccload-channel", "X-CCLoad-Channel"} {
		if val := strings.TrimSpace(req.Header.Get(h)); val != "" {
			if id, err := strconv.ParseInt(val, 10, 64); err == nil && id > 0 {
				return requestedChannelFilter{hasFilter: true, channelID: id}
			}
			return requestedChannelFilter{hasFilter: true, channelName: val}
		}
	}

	return requestedChannelFilter{}
}

// filterByRequestedChannel 根据客户端请求指定的渠道约束过滤候选渠道
func filterByRequestedChannel(cands []*model.Config, filter requestedChannelFilter) ([]*model.Config, bool) {
	if !filter.hasFilter || len(cands) == 0 {
		return cands, false
	}

	filtered := make([]*model.Config, 0, len(cands))
	for _, cfg := range cands {
		if cfg == nil {
			continue
		}
		if filter.channelID > 0 {
			if cfg.ID == filter.channelID {
				filtered = append(filtered, cfg)
			}
		} else if filter.channelName != "" {
			if cfg.Name == filter.channelName || strings.EqualFold(cfg.Name, filter.channelName) {
				filtered = append(filtered, cfg)
			}
		}
	}

	return filtered, true
}
