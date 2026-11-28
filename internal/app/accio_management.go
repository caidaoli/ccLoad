package app

import (
	"context"
	"errors"
	"log"
	"math"
	"net/http"
	"time"

	"ccLoad/internal/accioauth"
	"ccLoad/internal/model"
)

func (s *Server) requestAccioUsage(ctx context.Context, cfg *model.Config) (*oauthUsageSummary, error) {
	c, err := s.accioCredentials.credential(ctx, cfg, false, "")
	if err != nil {
		return nil, err
	}
	service := *s.accioService
	service.Client = s.getClientForChannel(cfg)
	usage, err := service.FetchUsage(ctx, c)
	var endpointErr *accioauth.HTTPError
	if errors.As(err, &endpointErr) && endpointErr.StatusCode == http.StatusUnauthorized {
		c, err = s.accioCredentials.credential(ctx, cfg, true, c.AccessToken)
		if err == nil {
			usage, err = service.FetchUsage(ctx, c)
		}
	}
	if err != nil {
		return nil, err
	}
	window := oauthUsageWindow{Kind: "accio", LimitName: "Accio", UsedPercent: usage.UsagePercent, RemainingPercent: usage.RemainingPercent}
	window.ResetAt = accioQuotaResetAt(usage.RefreshCountdownSeconds, time.Now())
	result := &oauthUsageSummary{Provider: accioauth.ChannelType, AccioUsage: usage, Windows: []oauthUsageWindow{window}}
	if usage.Subscription != nil {
		result.PlanType = usage.Subscription.PlanName
		result.EntitlementStatus = usage.Subscription.Status
	}
	return result, nil
}

/**
 * @brief 将 Accio 的窗口倒计时换算为可持久化的 Unix 秒。
 * @param countdown 额度查询响应中的秒数，可以缺失。
 * @param sampledAt 收到该响应的时间。
 * @return 窗口截止秒；缺失或无法表示的时间返回零。
 */
func accioQuotaResetAt(countdown *float64, sampledAt time.Time) int64 {
	if countdown == nil {
		return 0
	}
	if *countdown == 0 {
		return sampledAt.Unix()
	}
	seconds := math.Ceil(*countdown + float64(sampledAt.Nanosecond())/float64(time.Second))
	// 冷却会以 RFC3339 时间对外展示，越界的倒计时不能写入。
	if seconds >= float64(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC).Unix()-sampledAt.Unix()) {
		return 0
	}
	return sampledAt.Unix() + int64(seconds)
}

/**
 * @brief 为明确耗尽的模型登记临时冷却，同账号每轮只有首次命中发起后台额度查询。
 * @param ctx 临时冷却写入上下文。
 * @param cfg 实际收到拒绝的渠道和凭证。
 * @param actualModel 实际发送给 Accio 的模型名。
 * @note 后台工作由服务管理其生命周期，不阻塞当前请求的故障转移。
 */
func (s *Server) startAccioQuotaCooldown(ctx context.Context, cfg *model.Config, actualModel string) {
	until, probe, err := s.store.BeginAccioQuotaCooldown(ctx, cfg.ID, cfg.OAuthCredential, actualModel, time.Now())
	if err != nil {
		log.Printf("[WARN] 写入 Accio 临时模型冷却失败: channel=%d model=%s err=%v", cfg.ID, actualModel, err)
		return
	}
	if !until.IsZero() {
		s.invalidateChannelRelatedCache(cfg.ID)
	}
	if !probe {
		return
	}
	probeCtx, done, err := s.oauthCredentialRefreshes.begin()
	if err != nil {
		return
	}
	go func() {
		defer done()
		queryCtx, cancel := context.WithTimeout(probeCtx, oauthUsageTimeout)
		defer cancel()
		if err := s.confirmAccioQuotaCooldown(queryCtx, cfg, actualModel, until); err != nil && queryCtx.Err() == nil {
			log.Printf("[WARN] 核对 Accio 额度窗口失败: channel=%d err=%v", cfg.ID, err)
		}
	}()
}

/**
 * @brief 用同一账号的实时额度窗口校准触发本轮查询的冷却。
 * @param ctx 额度查询和确认写入共用的限时上下文。
 * @param cfg 临时冷却所绑定的渠道与凭证快照。
 * @param actualModel 触发首次查询的模型。
 * @param provisionalUntil 本轮临时截止时间，供存储核对期间是否已清除或更新。
 * @return 凭证解析、额度查询或事务失败时返回错误；窗口未耗尽或旧结果失效时返回 nil。
 */
func (s *Server) confirmAccioQuotaCooldown(ctx context.Context, cfg *model.Config, actualModel string, provisionalUntil time.Time) error {
	credential, err := accioauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return err
	}
	service := *s.accioService
	service.Client = s.getClientForChannel(cfg)
	usage, err := service.FetchQuota(ctx, credential)
	if err != nil || usage.UsagePercent != 100 {
		return err
	}
	now := time.Now()
	reset := accioQuotaResetAt(usage.RefreshCountdownSeconds, now)
	if reset <= now.Unix() {
		return nil
	}
	updated, err := s.store.ConfirmAccioQuotaWindow(ctx, cfg.ID, cfg.OAuthCredential, actualModel, provisionalUntil, time.Unix(reset, 0))
	if updated {
		s.invalidateChannelRelatedCache(cfg.ID)
	}
	return err
}
