package app

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

// LogsBootstrapResponse 聚合 logs 页首屏所需的所有数据，减少 RTT
type LogsBootstrapResponse struct {
	ChannelTestContent        string                `json:"channel_test_content"`
	LogChannelClickAction     string                `json:"log_channel_click_action"`
	ChannelCheckIntervalHours float64               `json:"channel_check_interval_hours"`
	AuthTokens                []*model.AuthToken    `json:"auth_tokens"`
	Models                    []string              `json:"models"`
	Channels                  []model.ChannelNameID `json:"channels"`
}

// HandleLogsBootstrap 聚合 logs 页首屏 5 个独立请求，并发组装后一次性返回
// GET /admin/logs/bootstrap
func (s *Server) HandleLogsBootstrap(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if isAPITokenWebRequest(c) {
		s.handleTokenLogsBootstrap(ctx, c)
		return
	}

	// 解析时间范围（与 HandleGetModels 保持一致）
	params := ParsePaginationParams(c)
	if params.Range == "" {
		params.Range = "this_month"
	}
	since, until := params.GetTimeRange()
	channelType := c.Query("channel_type")
	logFilter := &model.LogFilter{LogSource: model.LogSourceProxy}

	var (
		resp     LogsBootstrapResponse
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)

	setErr := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		mu.Unlock()
	}

	// goroutine 1: channel_test_content
	wg.Go(func() {
		setting, err := s.configService.GetSettingFresh(ctx, "channel_test_content")
		if err != nil && !errors.Is(err, model.ErrSettingNotFound) {
			setErr(err)
			return
		}
		if setting != nil {
			mu.Lock()
			resp.ChannelTestContent = setting.Value
			mu.Unlock()
		}
	})

	// goroutine 2: log_channel_click_action
	wg.Go(func() {
		setting, err := s.configService.GetSettingFresh(ctx, "log_channel_click_action")
		if err != nil && !errors.Is(err, model.ErrSettingNotFound) {
			setErr(err)
			return
		}
		if setting != nil {
			mu.Lock()
			resp.LogChannelClickAction = setting.Value
			mu.Unlock()
		}
	})

	// goroutine 3: channel_check_interval_hours
	wg.Go(func() {
		setting, err := s.configService.GetSettingFresh(ctx, "channel_check_interval_hours")
		if err != nil && !errors.Is(err, model.ErrSettingNotFound) {
			setErr(err)
			return
		}
		if setting != nil {
			n, _ := strconv.ParseFloat(setting.Value, 64)
			mu.Lock()
			resp.ChannelCheckIntervalHours = n
			mu.Unlock()
		}
	})

	// goroutine 4: auth tokens
	wg.Go(func() {
		tokens, err := s.store.ListAuthTokens(ctx)
		if err != nil {
			setErr(err)
			return
		}
		if tokens == nil {
			tokens = make([]*model.AuthToken, 0)
		}
		mu.Lock()
		resp.AuthTokens = tokens
		mu.Unlock()
	})

	// goroutine 5: distinct models + channels（顺序调用，共享同一 goroutine）
	wg.Go(func() {
		models, err := s.store.GetDistinctModels(ctx, since, until, channelType, logFilter)
		if err != nil {
			setErr(err)
			return
		}
		channels, err := s.store.GetDistinctChannels(ctx, since, until, channelType, logFilter)
		if err != nil {
			setErr(err)
			return
		}
		if models == nil {
			models = make([]string, 0)
		}
		if channels == nil {
			channels = make([]model.ChannelNameID, 0)
		}
		mu.Lock()
		resp.Models = models
		resp.Channels = channels
		mu.Unlock()
	})

	wg.Wait()

	if firstErr != nil {
		RespondError(c, http.StatusInternalServerError, firstErr)
		return
	}

	// 确保非 null
	if resp.AuthTokens == nil {
		resp.AuthTokens = make([]*model.AuthToken, 0)
	}
	if resp.Models == nil {
		resp.Models = make([]string, 0)
	}
	if resp.Channels == nil {
		resp.Channels = make([]model.ChannelNameID, 0)
	}

	RespondJSON(c, http.StatusOK, resp)
}

func (s *Server) handleTokenLogsBootstrap(ctx context.Context, c *gin.Context) {
	params := ParsePaginationParams(c)
	if params.Range == "" {
		params.Range = "this_month"
	}
	since, until := params.GetTimeRange()
	filter := BuildLogFilter(c)
	filter.LogSource = model.LogSourceProxy
	models, err := s.store.GetDistinctModels(ctx, since, until, "", &filter)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	if models == nil {
		models = make([]string, 0)
	}
	channels, err := s.store.GetDistinctChannels(ctx, since, until, "", &filter)
	if err != nil {
		RespondError(c, http.StatusInternalServerError, err)
		return
	}
	if channels == nil {
		channels = make([]model.ChannelNameID, 0)
	}
	RespondJSON(c, http.StatusOK, LogsBootstrapResponse{
		AuthTokens: make([]*model.AuthToken, 0),
		Models:     models,
		Channels:   channels,
	})
}
