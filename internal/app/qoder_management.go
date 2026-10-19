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
	for attempt := 0; attempt < 3; attempt++ {
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
	if claimed.Status == "claimed" || claimed.Status == "already_claimed" {
		if err = s.updateQoderCredential(ctx, cfg.ID, credential, func(c *qoderauth.Credential) {
			c.LastCheckinAt = time.Now().Unix()
			c.LastCheckinCampaign = claimed.CampaignID
		}); err != nil {
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

// HandleInstallQoderUMID installs the verified official component on supported hosts.
func (s *Server) HandleInstallQoderUMID(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Minute)
	defer cancel()
	if err := s.qoderUMID.Install(ctx); err != nil {
		RespondError(c, 502, err)
		return
	}
	RespondJSON(c, 200, s.qoderUMID.Status())
}

func qoderCheckinWindow(now time.Time) time.Time {
	local := now.In(time.FixedZone("Qoder", 8*3600))
	start := time.Date(local.Year(), local.Month(), local.Day(), 10, 0, 0, 0, local.Location())
	if local.Before(start) {
		start = start.AddDate(0, 0, -1)
	}
	return start
}

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
	window := qoderCheckinWindow(now)
	jobs := make(chan *model.Config)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for cfg := range jobs {
				if _, err := s.checkInQoder(ctx, cfg); err != nil {
					log.Printf("[WARN] Qoder 签到失败 channel=%d: %v", cfg.ID, err)
				}
			}
		}()
	}
	for _, cfg := range configs {
		if !cfg.Enabled || !cfg.UsesQoderOAuth() {
			continue
		}
		credential, err := qoderauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil || credential.LastCheckinAt >= window.Unix() {
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
