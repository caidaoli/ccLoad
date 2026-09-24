package app

import (
	"context"
	"regexp"
	"strings"

	"ccLoad/internal/model"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

var (
	tokenLogURLPattern    = regexp.MustCompile(`https?://[^\s"'<>]+`)
	tokenLogSecretPattern = regexp.MustCompile(`\b(?:sk[-_]|key[-_]|AIza)[A-Za-z0-9._-]+`)
)

type tokenLogChannelMetadata struct {
	APIKeys      []string
	APIKeyHashes map[string]struct{}
}

type tokenLogEntry struct {
	ID                       int64                       `json:"id"`
	Time                     model.JSONTime              `json:"time"`
	ChannelID                int64                       `json:"channel_id,omitempty"`
	ChannelName              string                      `json:"channel_name,omitempty"`
	ClientProtocol           string                      `json:"client_protocol,omitempty"`
	UpstreamProtocol         string                      `json:"upstream_protocol,omitempty"`
	LogSource                string                      `json:"log_source"`
	Model                    string                      `json:"model"`
	ActualModel              string                      `json:"actual_model,omitempty"`
	ResponseModel            string                      `json:"response_model,omitempty"`
	StatusCode               int                         `json:"status_code"`
	Message                  string                      `json:"message"`
	Duration                 float64                     `json:"duration"`
	IsStreaming              bool                        `json:"is_streaming"`
	UpstreamWebsocket        bool                        `json:"upstream_websocket,omitempty"`
	FirstByteTime            float64                     `json:"first_byte_time"`
	ServiceTier              string                      `json:"service_tier,omitempty"`
	ThinkingEffort           string                      `json:"thinking_effort,omitempty"`
	InputTokens              int                         `json:"input_tokens"`
	OutputTokens             int                         `json:"output_tokens"`
	ReasoningTokens          int                         `json:"reasoning_tokens,omitempty"`
	CacheReadInputTokens     int                         `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int                         `json:"cache_creation_input_tokens"`
	Cache5mInputTokens       int                         `json:"cache_5m_input_tokens"`
	Cache1hInputTokens       int                         `json:"cache_1h_input_tokens"`
	Cost                     float64                     `json:"cost"`
	EffectiveCost            float64                     `json:"effective_cost"`
	CostBreakdown            *util.StandardCostBreakdown `json:"cost_breakdown,omitempty"`
}

type dashboardLogEntry struct {
	*model.LogEntry
	CostBreakdown *util.StandardCostBreakdown `json:"cost_breakdown,omitempty"`
}

// logModelPriceFunc 返回当前可确定的渠道价格；无法辨别选中行时不展示重算明细。
type logModelPriceFunc func(entry *model.LogEntry) (*util.CustomModelPrice, bool)

// logModelPrices 为一页日志预加载渠道配置，按渠道当前的模型价格重算成本明细。
// 渠道已删除或读取失败时明细回退全局价格——明细只用于展示，日志成本已在写入时定格。
func (s *Server) logModelPrices(ctx context.Context, logs []*model.LogEntry) logModelPriceFunc {
	configs := make(map[int64]*model.Config)
	for _, entry := range logs {
		if entry == nil || entry.ChannelID <= 0 || entry.Cost <= 0 {
			continue
		}
		if _, loaded := configs[entry.ChannelID]; loaded {
			continue
		}
		cfg, err := s.GetConfig(ctx, entry.ChannelID)
		if err != nil {
			cfg = nil
		}
		configs[entry.ChannelID] = cfg
	}
	return func(entry *model.LogEntry) (*util.CustomModelPrice, bool) {
		cfg := configs[entry.ChannelID]
		if cfg == nil {
			return nil, true
		}
		actual := entry.ActualModel
		if actual == "" {
			actual = model.RoutingModelName(entry.Model)
		}
		logical := s.resolveChannelRoutingModel(cfg, entry.Model)
		var matched *util.CustomModelPrice
		matches := 0
		for _, row := range cfg.ModelEntries {
			if !strings.EqualFold(model.RoutingModelName(row.Model), logical) {
				continue
			}
			selected := modelRoutingSelection{
				logicalModel: logical,
				entry:        row,
				fuzzyMatched: !strings.EqualFold(model.RoutingModelName(entry.Model), logical),
			}
			if strings.EqualFold(s.resolveFinalUpstreamModel(cfg, selected, entry.UpstreamProtocol), actual) {
				if matches > 0 && !matched.Equal(row.Pricing) {
					return nil, false
				}
				matched = row.Pricing
				matches++
			}
		}
		if matches == 0 && cfg.UsesAntigravityOAuth() && strings.EqualFold(actual, antigravityWebSearchFallbackModel) {
			// Web Search replaces the routed model after row selection. Its log retains
			// the selected row's price, which cannot be recovered from this model name.
			return nil, false
		}
		return matched, true
	}
}

func buildLogCostBreakdown(entry *model.LogEntry, modelPrice logModelPriceFunc) *util.StandardCostBreakdown {
	if entry == nil || entry.Cost <= 0 {
		return nil
	}
	var price *util.CustomModelPrice
	if modelPrice != nil {
		var identifiable bool
		price, identifiable = modelPrice(entry)
		if !identifiable {
			return nil
		}
	}
	billingModel := util.ResolveBillingModel(entry.ActualModel, entry.Model)
	cache5mTokens := entry.Cache5mInputTokens
	cache1hTokens := entry.Cache1hInputTokens
	if cache5mTokens+cache1hTokens == 0 && entry.CacheCreationInputTokens > 0 {
		// 旧日志只有缓存创建总量；历史计费语义等同 5m 缓存创建。
		cache5mTokens = entry.CacheCreationInputTokens
	}
	breakdown := util.CalculateStandardCostBreakdownWithPrice(
		billingModel,
		entry.ServiceTier,
		price,
		entry.InputTokens,
		entry.OutputTokens,
		entry.CacheReadInputTokens,
		cache5mTokens,
		cache1hTokens,
	)
	return &breakdown
}

func projectDashboardLogs(logs []*model.LogEntry, modelPrice logModelPriceFunc) []dashboardLogEntry {
	projected := make([]dashboardLogEntry, 0, len(logs))
	for _, entry := range logs {
		if entry == nil {
			continue
		}
		projected = append(projected, dashboardLogEntry{
			LogEntry:      entry,
			CostBreakdown: buildLogCostBreakdown(entry, modelPrice),
		})
	}
	return projected
}

func projectTokenLogs(logs []*model.LogEntry, channels map[int64]tokenLogChannelMetadata, modelPrice logModelPriceFunc) []tokenLogEntry {
	projected := make([]tokenLogEntry, 0, len(logs))
	for _, entry := range logs {
		if entry == nil {
			continue
		}
		multiplier := entry.CostMultiplier
		if multiplier < 0 {
			multiplier = 1
		}
		channel, channelExists := channels[entry.ChannelID]
		message := "[redacted]"
		canSanitize := entry.ChannelID <= 0
		if channelExists && entry.APIKeyHash != "" {
			_, canSanitize = channel.APIKeyHashes[entry.APIKeyHash]
		}
		if canSanitize {
			message = sanitizeTokenLogMessage(entry, channel.APIKeys)
		}
		projected = append(projected, tokenLogEntry{
			ID:                       entry.ID,
			Time:                     entry.Time,
			ChannelID:                entry.ChannelID,
			ChannelName:              entry.ChannelName,
			ClientProtocol:           entry.ClientProtocol,
			UpstreamProtocol:         entry.UpstreamProtocol,
			LogSource:                entry.LogSource,
			Model:                    entry.Model,
			ActualModel:              entry.ActualModel,
			ResponseModel:            entry.ResponseModel,
			StatusCode:               entry.StatusCode,
			Message:                  message,
			Duration:                 entry.Duration,
			IsStreaming:              entry.IsStreaming,
			UpstreamWebsocket:        entry.UpstreamWebsocket,
			FirstByteTime:            entry.FirstByteTime,
			ServiceTier:              entry.ServiceTier,
			ThinkingEffort:           entry.ThinkingEffort,
			InputTokens:              entry.InputTokens,
			OutputTokens:             entry.OutputTokens,
			ReasoningTokens:          entry.ReasoningTokens,
			CacheReadInputTokens:     entry.CacheReadInputTokens,
			CacheCreationInputTokens: entry.CacheCreationInputTokens,
			Cache5mInputTokens:       entry.Cache5mInputTokens,
			Cache1hInputTokens:       entry.Cache1hInputTokens,
			Cost:                     entry.Cost,
			EffectiveCost:            entry.Cost * multiplier,
			CostBreakdown:            buildLogCostBreakdown(entry, modelPrice),
		})
	}
	return projected
}

func sanitizeTokenLogMessage(entry *model.LogEntry, channelAPIKeys []string) string {
	message := entry.Message
	sensitiveValues := []string{
		entry.BaseURL,
		entry.APIKeyUsed,
		entry.APIKeyHash,
		entry.ChannelName,
		entry.ClientIP,
	}
	sensitiveValues = append(sensitiveValues, channelAPIKeys...)
	for _, sensitive := range sensitiveValues {
		if sensitive != "" {
			message = strings.ReplaceAll(message, sensitive, "[redacted]")
		}
	}
	message = tokenLogURLPattern.ReplaceAllString(message, "[redacted]")
	message = tokenLogSecretPattern.ReplaceAllString(message, "[redacted]")
	const maxSummaryRunes = 512
	runes := []rune(message)
	if len(runes) > maxSummaryRunes {
		message = string(runes[:maxSummaryRunes]) + "…"
	}
	return message
}

// ApplyWebIdentityScope forces API-token sessions to their bound log scope.
func ApplyWebIdentityScope(c *gin.Context, filter *model.LogFilter) {
	identity, ok := WebIdentityFromContext(c)
	if !ok || identity.Role != model.WebRoleAPIToken {
		return
	}
	tokenID := identity.AuthTokenID
	if tokenID <= 0 {
		tokenID = 1<<63 - 1
	}
	filter.AuthTokenID = &tokenID
	filter.LogSource = model.LogSourceProxy
	if identity.HideChannels {
		filter.ChannelID = nil
		filter.ChannelName = ""
		filter.ChannelNameLike = ""
	}
}

func hideTokenChannels(c *gin.Context) bool {
	identity, ok := WebIdentityFromContext(c)
	return ok && identity.Role == model.WebRoleAPIToken && identity.HideChannels
}

func isAPITokenWebRequest(c *gin.Context) bool {
	identity, ok := WebIdentityFromContext(c)
	return ok && identity.Role == model.WebRoleAPIToken
}
