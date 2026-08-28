package app

import (
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

type dashboardChannelView struct {
	ID                    int64              `json:"id"`
	Name                  string             `json:"name"`
	URLs                  model.ChannelURLs  `json:"urls"`
	ProtocolTransformMode string             `json:"protocol_transform_mode"`
	Priority              int                `json:"priority"`
	Enabled               bool               `json:"enabled"`
	Models                []model.ModelEntry `json:"models"`
	CostMultiplierMin     float64            `json:"cost_multiplier_min"`
	CostMultiplierMax     float64            `json:"cost_multiplier_max"`
	CooldownRemainingMS   int64              `json:"cooldown_remaining_ms,omitempty"`
}

type channelFilterOptionsResponse struct {
	ChannelNames []string `json:"channel_names"`
	Models       []string `json:"models"`
}

func (s *Server) tokenScopedChannelConfigs(c *gin.Context) ([]*model.Config, map[int64]time.Time, error) {
	params := ParsePaginationParams(c)
	if params.Range == "" {
		params.Range = "today"
	}
	since, until := params.GetTimeRange()
	filter := BuildLogFilter(c)
	filter.LogSource = model.LogSourceProxy
	visible, err := s.store.GetDistinctChannels(c.Request.Context(), since, until, &filter)
	if err != nil {
		return nil, nil, err
	}
	visibleIDs := make(map[int64]struct{}, len(visible))
	for _, channel := range visible {
		visibleIDs[channel.ID] = struct{}{}
	}

	configs, err := s.store.ListConfigs(c.Request.Context())
	if err != nil {
		return nil, nil, err
	}
	scoped := make([]*model.Config, 0, len(visibleIDs))
	for _, cfg := range configs {
		if _, ok := visibleIDs[cfg.ID]; ok {
			scoped = append(scoped, cfg)
		}
	}

	cooldowns, err := s.getAllChannelCooldowns(c.Request.Context())
	if err != nil {
		cooldowns = make(map[int64]time.Time)
	}
	return scoped, cooldowns, nil
}

// HandleDashboardChannels returns the current web session's visible channel configurations.
func (s *Server) HandleDashboardChannels(c *gin.Context) {
	configs, cooldowns, err := s.tokenScopedChannelConfigs(c)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}

	now := time.Now()
	configs = applyChannelListFilters(configs, c, channelCooldownSnapshot{channels: cooldowns}, now)
	total := len(configs)
	configs = paginateChannels(configs, c)

	// 倍率区间批量查询（消除 N+1）；失败降级回渠道级倍率，不阻断列表
	channelIDs := make(map[int64]bool, len(configs))
	for _, cfg := range configs {
		channelIDs[cfg.ID] = true
	}
	channelInfos, err := s.store.FetchChannelInfoBatch(c.Request.Context(), channelIDs)
	if err != nil {
		log.Printf("[WARN] 批量查询渠道倍率区间失败: %v", err)
		channelInfos = make(map[int64]model.ChannelInfo)
	}

	out := make([]dashboardChannelView, 0, len(configs))
	for _, cfg := range configs {
		multiplierMin, multiplierMax := cfg.CostMultiplier, cfg.CostMultiplier
		if multiplierMin < 0 {
			multiplierMin, multiplierMax = 1, 1
		}
		if info, ok := channelInfos[cfg.ID]; ok {
			multiplierMin, multiplierMax = info.CostMultiplierMin, info.CostMultiplierMax
		}
		view := dashboardChannelView{
			ID:                    cfg.ID,
			Name:                  cfg.Name,
			URLs:                  cfg.URLs.Clone(),
			ProtocolTransformMode: cfg.GetProtocolTransformMode(),
			Priority:              cfg.Priority,
			Enabled:               cfg.Enabled,
			Models:                append([]model.ModelEntry(nil), cfg.ModelEntries...),
			CostMultiplierMin:     multiplierMin,
			CostMultiplierMax:     multiplierMax,
		}
		if until, ok := cooldowns[cfg.ID]; ok && until.After(now) {
			view.CooldownRemainingMS = until.Sub(now).Milliseconds()
		}
		out = append(out, view)
	}
	RespondPaginated(c, http.StatusOK, out, total)
}

// HandleDashboardChannelFilterOptions returns filter options for visible dashboard channels.
func (s *Server) HandleDashboardChannelFilterOptions(c *gin.Context) {
	configs, cooldowns, err := s.tokenScopedChannelConfigs(c)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	configs = filterChannelOptionConfigs(
		configs,
		strings.TrimSpace(c.Query("protocol")),
		strings.TrimSpace(c.Query("status")),
		channelCooldownSnapshot{channels: cooldowns},
		time.Now(),
	)
	RespondJSON(c, http.StatusOK, buildChannelFilterOptions(configs))
}

func filterChannelOptionConfigs(
	cfgs []*model.Config,
	configuredProtocol string,
	status string,
	cooldowns channelCooldownSnapshot,
	now time.Time,
) []*model.Config {
	if configuredProtocol != "" && configuredProtocol != "all" {
		cfgs = filterConfigs(cfgs, func(cfg *model.Config) bool {
			return configHasURLProtocol(cfg, configuredProtocol)
		})
	}

	if status == "" || status == "all" {
		return cfgs
	}
	return filterConfigs(cfgs, func(cfg *model.Config) bool {
		switch status {
		case "enabled":
			return cfg.Enabled
		case "disabled":
			return !cfg.Enabled
		case "cooldown":
			return cooldowns.hasActiveCooldown(cfg.ID, now)
		default:
			return false
		}
	})
}

func buildChannelFilterOptions(cfgs []*model.Config) channelFilterOptionsResponse {
	nameSet := make(map[string]struct{}, len(cfgs))
	modelSet := make(map[string]struct{})
	for _, cfg := range cfgs {
		if name := strings.TrimSpace(cfg.Name); name != "" {
			nameSet[name] = struct{}{}
		}
		for _, entry := range cfg.ModelEntries {
			if entry.Model != "" {
				modelSet[entry.Model] = struct{}{}
			}
		}
	}

	channelNames := make([]string, 0, len(nameSet))
	for name := range nameSet {
		channelNames = append(channelNames, name)
	}
	models := make([]string, 0, len(modelSet))
	for name := range modelSet {
		models = append(models, name)
	}

	sort.Strings(channelNames)
	sort.Strings(models)
	return channelFilterOptionsResponse{ChannelNames: channelNames, Models: models}
}
