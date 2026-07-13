package app

import (
	"regexp"
	"strings"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

var (
	tokenLogURLPattern    = regexp.MustCompile(`https?://[^\s"'<>]+`)
	tokenLogSecretPattern = regexp.MustCompile(`\b(?:sk|key|AIza)[-_][A-Za-z0-9._-]+`)
)

type tokenLogEntry struct {
	ID                       int64          `json:"id"`
	Time                     model.JSONTime `json:"time"`
	Model                    string         `json:"model"`
	ActualModel              string         `json:"actual_model,omitempty"`
	StatusCode               int            `json:"status_code"`
	Message                  string         `json:"message"`
	Duration                 float64        `json:"duration"`
	IsStreaming              bool           `json:"is_streaming"`
	FirstByteTime            float64        `json:"first_byte_time"`
	ServiceTier              string         `json:"service_tier,omitempty"`
	ThinkingEffort           string         `json:"thinking_effort,omitempty"`
	InputTokens              int            `json:"input_tokens"`
	OutputTokens             int            `json:"output_tokens"`
	ReasoningTokens          int            `json:"reasoning_tokens,omitempty"`
	CacheReadInputTokens     int            `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int            `json:"cache_creation_input_tokens"`
	Cache5mInputTokens       int            `json:"cache_5m_input_tokens"`
	Cache1hInputTokens       int            `json:"cache_1h_input_tokens"`
	Cost                     float64        `json:"cost"`
	EffectiveCost            float64        `json:"effective_cost"`
}

func projectTokenLogs(logs []*model.LogEntry) []tokenLogEntry {
	projected := make([]tokenLogEntry, 0, len(logs))
	for _, entry := range logs {
		if entry == nil {
			continue
		}
		multiplier := entry.CostMultiplier
		if multiplier < 0 {
			multiplier = 1
		}
		projected = append(projected, tokenLogEntry{
			ID:                       entry.ID,
			Time:                     entry.Time,
			Model:                    entry.Model,
			ActualModel:              entry.ActualModel,
			StatusCode:               entry.StatusCode,
			Message:                  sanitizeTokenLogMessage(entry),
			Duration:                 entry.Duration,
			IsStreaming:              entry.IsStreaming,
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
		})
	}
	return projected
}

func sanitizeTokenLogMessage(entry *model.LogEntry) string {
	message := entry.Message
	for _, sensitive := range []string{
		entry.BaseURL,
		entry.APIKeyUsed,
		entry.APIKeyHash,
		entry.ChannelName,
		entry.ClientIP,
	} {
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

type tokenStatsAccumulator struct {
	entry          model.StatsEntry
	firstByteSum   float64
	firstByteCount int
	durationSum    float64
	durationCount  int
}

func aggregateTokenStats(stats []model.StatsEntry) []model.StatsEntry {
	byModel := make(map[string]*tokenStatsAccumulator)
	order := make([]string, 0, len(stats))
	for _, source := range stats {
		key := source.Model
		acc, ok := byModel[key]
		if !ok {
			acc = &tokenStatsAccumulator{entry: model.StatsEntry{
				Model: source.Model,
			}}
			byModel[key] = acc
			order = append(order, key)
		}

		acc.entry.Success += source.Success
		acc.entry.Error += source.Error
		acc.entry.Total += source.Total
		addInt64Ptr(&acc.entry.TotalInputTokens, source.TotalInputTokens)
		addInt64Ptr(&acc.entry.TotalOutputTokens, source.TotalOutputTokens)
		addInt64Ptr(&acc.entry.TotalCacheReadInputTokens, source.TotalCacheReadInputTokens)
		addInt64Ptr(&acc.entry.TotalCacheCreationInputTokens, source.TotalCacheCreationInputTokens)
		addFloat64Ptr(&acc.entry.TotalCost, source.TotalCost)
		addFloat64Ptr(&acc.entry.EffectiveCost, source.EffectiveCost)
		addFloat64Ptr(&acc.entry.PeakRPM, source.PeakRPM)
		addFloat64Ptr(&acc.entry.AvgRPM, source.AvgRPM)
		addFloat64Ptr(&acc.entry.RecentRPM, source.RecentRPM)

		if source.AvgFirstByteTimeSeconds != nil && source.Success > 0 {
			acc.firstByteSum += *source.AvgFirstByteTimeSeconds * float64(source.Success)
			acc.firstByteCount += source.Success
		}
		if source.AvgDurationSeconds != nil && source.Success > 0 {
			acc.durationSum += *source.AvgDurationSeconds * float64(source.Success)
			acc.durationCount += source.Success
		}
		if newerTimestamp(source.LastSuccessAt, acc.entry.LastSuccessAt) {
			acc.entry.LastSuccessAt = source.LastSuccessAt
			acc.entry.LastSuccessID = source.LastSuccessID
		}
		if newerTimestamp(source.LastRequestAt, acc.entry.LastRequestAt) {
			acc.entry.LastRequestAt = source.LastRequestAt
			acc.entry.LastRequestID = source.LastRequestID
			acc.entry.LastRequestStatus = source.LastRequestStatus
		}
	}

	result := make([]model.StatsEntry, 0, len(order))
	for _, key := range order {
		acc := byModel[key]
		if acc.firstByteCount > 0 {
			value := acc.firstByteSum / float64(acc.firstByteCount)
			acc.entry.AvgFirstByteTimeSeconds = &value
		}
		if acc.durationCount > 0 {
			value := acc.durationSum / float64(acc.durationCount)
			acc.entry.AvgDurationSeconds = &value
		}
		result = append(result, acc.entry)
	}
	return result
}

func addInt64Ptr(dst **int64, src *int64) {
	if src == nil {
		return
	}
	if *dst == nil {
		value := int64(0)
		*dst = &value
	}
	**dst += *src
}

func addFloat64Ptr(dst **float64, src *float64) {
	if src == nil {
		return
	}
	if *dst == nil {
		value := float64(0)
		*dst = &value
	}
	**dst += *src
}

func newerTimestamp(candidate, current *int64) bool {
	return candidate != nil && (current == nil || *candidate > *current)
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
}

func isAPITokenWebRequest(c *gin.Context) bool {
	identity, ok := WebIdentityFromContext(c)
	return ok && identity.Role == model.WebRoleAPIToken
}
