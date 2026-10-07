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

// requestedChannelUnavailableMessage 指定渠道不存在、不支持当前模型或被 Token 限制时的统一错误
const requestedChannelUnavailableMessage = "requested channel not found or not available for this model/token"

// extractRequestedChannelFilter 从 HTTP Header 解析客户端指定的渠道（Header 名大小写不敏感）：
// - x-ccload-channel-id：渠道数字 ID，优先
// - x-ccload-channel：渠道名称（忽略大小写）；纯数字时同时按 ID 匹配
func extractRequestedChannelFilter(req *http.Request) requestedChannelFilter {
	if req == nil {
		return requestedChannelFilter{}
	}

	if val := strings.TrimSpace(req.Header.Get("x-ccload-channel-id")); val != "" {
		if id, err := strconv.ParseInt(val, 10, 64); err == nil && id > 0 {
			return requestedChannelFilter{hasFilter: true, channelID: id}
		}
	}

	if val := strings.TrimSpace(req.Header.Get("x-ccload-channel")); val != "" {
		filter := requestedChannelFilter{hasFilter: true, channelName: val}
		if id, err := strconv.ParseInt(val, 10, 64); err == nil && id > 0 {
			filter.channelID = id
		}
		return filter
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
		if (filter.channelID > 0 && cfg.ID == filter.channelID) ||
			(filter.channelName != "" && strings.EqualFold(cfg.Name, filter.channelName)) {
			filtered = append(filtered, cfg)
		}
	}

	return filtered, true
}
