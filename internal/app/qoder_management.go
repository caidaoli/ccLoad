package app

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/qoderauth"

	"github.com/gin-gonic/gin"
)

func (s *Server) requestQoderUsage(ctx context.Context, cfg *model.Config) (*oauthUsageSummary, error) {
	credential, err := s.qoderCredentials.credential(ctx, cfg, false, "")
	if err != nil {
		return nil, err
	}
	service := *s.qoderService
	service.Client = s.getClientForChannel(cfg)
	usage, err := service.FetchUsage(ctx, credential)
	var endpointErr *qoderauth.HTTPError
	if errors.As(err, &endpointErr) && endpointErr.StatusCode == 401 {
		credential, err = s.qoderCredentials.credential(ctx, cfg, true, credential.AccessToken)
		if err == nil {
			usage, err = service.FetchUsage(ctx, credential)
		}
	}
	if err != nil {
		return nil, err
	}
	return &oauthUsageSummary{Provider: qoderauth.ChannelType, PlanType: usage.Plan, QoderCredits: usage, Windows: []oauthUsageWindow{}}, nil
}

func (s *Server) updateQoderCredential(ctx context.Context, id int64, identity *qoderauth.Credential, update func(*qoderauth.Credential)) error {
	for range 3 {
		cfg, err := s.store.GetConfig(ctx, id)
		if err != nil {
			return err
		}
		if !cfg.UsesQoderOAuth() {
			return errors.New("qoder channel authentication changed")
		}
		credential, err := qoderauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			return err
		}
		if !qoderIdentityMatches(identity, credential) {
			return errors.New("qoder account changed")
		}
		update(credential)
		payload, err := credential.JSON()
		if err != nil {
			return err
		}
		changed, err := s.store.CompareAndSwapOAuthCredential(ctx, id, model.AuthTypeQoderOAuth, cfg.OAuthCredential, payload)
		if err != nil {
			return err
		}
		if changed {
			s.invalidateChannelRelatedCache(id)
			s.InvalidateChannelListCache()
			return nil
		}
	}
	return errors.New("qoder credential changed concurrently; retry")
}

type qoderCheckinResponse struct {
	*qoderauth.CheckinResult
	Usage *oauthUsageSummary `json:"usage,omitempty"`
}

/**
 * @brief 领取或确认 Qoder 每日签到，并保存对应活动窗口的状态。
 * @param ctx 请求上下文。
 * @param cfg Qoder 渠道及其账号配置。
 * @return 确认的活动状态和可用的余额快照。
 * @return 上游或凭据请求失败时返回错误，保存签到失败时附带已确认的活动状态。
 * @note 同进程同渠道的手动和定时签到串行执行；认证拒绝后可刷新一次。上游确认本窗口无每日活动时记录该窗口，
 *       调度器在同窗口内不再复查。
 */
func (s *Server) checkInQoder(ctx context.Context, cfg *model.Config) (*qoderCheckinResponse, error) {
	// A per-channel lock serializes manual and scheduled claims. Upstream campaign
	// state is authoritative across processes and after an uncertain POST outcome.
	value, _ := s.qoderCheckins.LoadOrStore(cfg.ID, make(chan struct{}, 1))
	gate := value.(chan struct{})
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	credential, err := s.qoderCredentials.credential(ctx, cfg, false, "")
	if err != nil {
		return nil, err
	}
	service := *s.qoderService
	service.Client = s.getClientForChannel(cfg)
	window := qoderauth.CheckinWindow(time.Now())
	claimed, err := service.Checkin(ctx, credential)
	var endpointErr *qoderauth.HTTPError
	if errors.As(err, &endpointErr) && endpointErr.StatusCode == 401 {
		credential, err = s.qoderCredentials.credential(ctx, cfg, true, credential.AccessToken)
		if err == nil {
			claimed, err = service.Checkin(ctx, credential)
		}
	}
	if err != nil {
		return nil, err
	}
	result := &qoderCheckinResponse{CheckinResult: claimed}
	if claimed.Status == "unavailable" {
		s.qoderUnavailableWindows.Store(cfg.ID, window.Unix())
	}
	if claimed.Status == "claimed" || claimed.Status == "already_claimed" {
		if err = s.updateQoderCredential(ctx, cfg.ID, credential, claimed.UpdateCredential); err != nil {
			return result, err
		}
	}
	requestedAt := time.Now()
	summary, usageErr := s.requestQoderUsage(ctx, cfg)
	if usageErr == nil {
		summary, usageErr = s.persistOAuthUsage(ctx, cfg, summary, requestedAt, time.Now())
	}
	if usageErr != nil {
		log.Printf("[WARN] Qoder 签到后余额刷新失败 channel=%d: %v", cfg.ID, usageErr)
	} else {
		result.Usage = summary
	}
	message, _ := json.Marshal(channelCheckinAuditMessage{Profile: "qoder", Status: claimed.Status})
	if logErr := s.store.AddLog(ctx, &model.LogEntry{Time: model.JSONTime{Time: time.Now()}, ChannelID: cfg.ID, StatusCode: 200, LogSource: model.LogSourceCheckin, CostMultiplier: cfg.CostMultiplier, Message: string(message)}); logErr != nil {
		log.Printf("[WARN] Qoder 签到日志写入失败: %v", logErr)
	}
	return result, nil
}

// HandleQoderCheckin claims the current activity and updates the balance snapshot.
func (s *Server) HandleQoderCheckin(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondError(c, 400, err)
		return
	}
	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondErrorMsg(c, 404, "channel not found")
		return
	}
	if !cfg.UsesQoderOAuth() {
		RespondErrorMsg(c, 409, "channel does not use Qoder")
		return
	}
	result, err := s.checkInQoder(c.Request.Context(), cfg)
	if err != nil {
		RespondError(c, 502, err)
		return
	}
	RespondJSON(c, 200, result)
}

// HandleQoderUMIDStatus reports optional international check-in component availability.
func (s *Server) HandleQoderUMIDStatus(c *gin.Context) { RespondJSON(c, 200, s.qoderUMID.Status()) }

// HandleInstallQoderUMID starts installing the verified official component without tying it to this
// request; clients poll the status endpoint for progress and the result.
func (s *Server) HandleInstallQoderUMID(c *gin.Context) {
	s.qoderUMID.Install()
	RespondJSON(c, 200, s.qoderUMID.Status())
}

/**
 * @brief 扫描并签到本窗口尚未完成、且上游未确认无活动的已启用 Qoder 渠道。
 * @param ctx 调度上下文，取消时停止分发。
 * @note 启动最多四个工作协程，等待本轮签到全部退出。
 */
func (s *Server) runDueQoderCheckins(ctx context.Context) {
	if s == nil || s.store == nil {
		return
	}
	configs, err := s.store.ListConfigs(ctx)
	if err != nil {
		log.Printf("[WARN] Qoder 签到扫描失败: %v", err)
		return
	}
	now := time.Now()
	window := qoderauth.CheckinWindow(now)
	jobs := make(chan *model.Config)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for cfg := range jobs {
				if _, err := s.checkInQoder(ctx, cfg); err != nil {
					log.Printf("[WARN] Qoder 签到失败 channel=%d: %v", cfg.ID, err)
				}
			}
		})
	}
	for _, cfg := range configs {
		if !cfg.Enabled || !cfg.UsesQoderOAuth() {
			continue
		}
		credential, err := qoderauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil || credential.LastCheckinAt >= window.Unix() {
			continue
		}
		if settled, _ := s.qoderUnavailableWindows.Load(cfg.ID); settled == window.Unix() {
			continue
		}
		select {
		case jobs <- cfg:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		}
	}
	close(jobs)
	wg.Wait()
}
