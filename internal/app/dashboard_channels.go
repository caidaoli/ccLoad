package app

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

type dashboardChannelView struct {
	ID                    int64              `json:"id"`
	Name                  string             `json:"name"`
	ChannelType           string             `json:"channel_type"`
	ProtocolTransformMode string             `json:"protocol_transform_mode,omitempty"`
	ProtocolTransforms    []string           `json:"protocol_transforms,omitempty"`
	Priority              int                `json:"priority"`
	Enabled               bool               `json:"enabled"`
	Models                []model.ModelEntry `json:"models"`
	CostMultiplier        float64            `json:"cost_multiplier"`
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
	visible, err := s.store.GetDistinctChannels(c.Request.Context(), since, until, "", &filter)
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
	configs = applyChannelListFilters(configs, c, cooldowns, now)
	total := len(configs)
	configs = paginateChannels(configs, c)
	out := make([]dashboardChannelView, 0, len(configs))
	for _, cfg := range configs {
		view := dashboardChannelView{
			ID:                    cfg.ID,
			Name:                  cfg.Name,
			ChannelType:           cfg.ChannelType,
			ProtocolTransformMode: cfg.ProtocolTransformMode,
			ProtocolTransforms:    append([]string(nil), cfg.ProtocolTransforms...),
			Priority:              cfg.Priority,
			Enabled:               cfg.Enabled,
			Models:                append([]model.ModelEntry(nil), cfg.ModelEntries...),
			CostMultiplier:        cfg.CostMultiplier,
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
		strings.TrimSpace(c.Query("type")),
		strings.TrimSpace(c.Query("status")),
		cooldowns,
		time.Now(),
	)
	RespondJSON(c, http.StatusOK, buildChannelFilterOptions(configs))
}

func filterChannelOptionConfigs(
	cfgs []*model.Config,
	channelType string,
	status string,
	cooldowns map[int64]time.Time,
	now time.Time,
) []*model.Config {
	if channelType != "" && channelType != "all" {
		normalizedChannelType := util.NormalizeChannelType(channelType)
		cfgs = filterConfigs(cfgs, func(cfg *model.Config) bool {
			return channelExposesProtocol(cfg, normalizedChannelType)
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
			until, cooled := cooldowns[cfg.ID]
			return cooled && until.After(now)
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
