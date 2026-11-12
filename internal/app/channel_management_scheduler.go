package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"ccLoad/internal/codebuddyauth"
	"ccLoad/internal/model"
)

const (
	channelManagementScheduleInterval = time.Minute
	channelManagementScheduleWorkers  = 4
	codeBuddyCheckinTimeout           = 60 * time.Second
)

/**
 * @brief 在服务生命周期内定期调度管理账户及 CodeBuddy 签到。
 * @note 启动时补偿扫描，此后每分钟扫描，服务取消时退出并释放等待组；CodeBuddy 仅在固定时段切换时触发。
 */
func (s *Server) managementCheckinLoop() {
	defer s.wg.Done()

	ctx := s.baseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.runDueManagementCheckins(ctx, time.Now()); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("[WARN] 管理账户每日签到补偿扫描失败: %v", err)
	}
	// Domestic CodeBuddy OAuth accounts use the provider's fixed 09:00/21:00
	// local check-in windows. Initialize the current slot without replaying it
	// on every server restart; the next slot transition performs the check-in.
	codeBuddySlot := codeBuddyCheckinSlot(time.Now())
	// Qoder check-in is always on; its catch-up runs on the first tick rather
	// than during boot, so startup never races channel setup.
	qoderLastScan := time.Now().Add(-15 * time.Minute)

	ticker := time.NewTicker(channelManagementScheduleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.shutdownCh:
			return
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if now.Sub(qoderLastScan) >= 15*time.Minute {
				s.runDueQoderCheckins(ctx)
				qoderLastScan = now
			}
			if err := s.runDueManagementCheckins(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("[WARN] 管理账户每日签到扫描失败: %v", err)
			}
			if slot := codeBuddyCheckinSlot(now); slot != "" && slot != codeBuddySlot {
				s.runDueCodeBuddyCheckins(ctx)
				codeBuddySlot = slot
			}
		}
	}
}

/**
 * @brief 扫描可签到的国内 CodeBuddy 渠道并通过公共工作队列执行。
 * @param ctx 本次操作的取消与截止时间上下文。
 * @note 由固定时段调度器调用；会产生上游签到和审计记录，扫描失败仅记录日志。
 */
func (s *Server) runDueCodeBuddyCheckins(ctx context.Context) {
	if s == nil || s.store == nil || s.codeBuddyService == nil || s.codeBuddyCredentials == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	configs, err := s.store.ListConfigs(ctx)
	if err != nil {
		log.Printf("[WARN] CodeBuddy 签到扫描失败: %v", err)
		return
	}
	targets := make([]*model.Config, 0, len(configs))
	for _, cfg := range configs {
		if cfg != nil && cfg.Enabled && cfg.UsesCodeBuddyOAuth() && !isInternationalCodeBuddyConfig(cfg) {
			targets = append(targets, cfg)
		}
	}
	s.runChannelCheckinJobs(ctx, targets, s.runCodeBuddyCheckin)
}

/**
 * @brief 复用签到派发逻辑，以每批固定并发上限执行渠道任务。
 * @param ctx 本次操作的取消与截止时间上下文。
 * @param targets 本轮待处理的渠道快照列表，空列表直接返回。
 * @param run 执行单个渠道签到的回调，会被多个工作协程并发调用。
 * @note 每次调用独立创建最多四个 worker；取消后停止派发并等待已开始的工作结束，回调需响应上下文取消。
 */
func (s *Server) runChannelCheckinJobs(ctx context.Context, targets []*model.Config, run func(context.Context, *model.Config)) {
	if len(targets) == 0 || ctx.Err() != nil {
		return
	}
	jobs := make(chan *model.Config)
	workers := min(channelManagementScheduleWorkers, len(targets))
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for cfg := range jobs {
				if ctx.Err() == nil {
					run(ctx, cfg)
				}
			}
		}()
	}
	for _, cfg := range targets {
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

type codeBuddyCheckinResult struct {
	Status string             `json:"status"`
	Usage  *oauthUsageSummary `json:"usage"`
}

func (s *Server) checkInCodeBuddy(ctx context.Context, cfg *model.Config) (*codeBuddyCheckinResult, error) {
	if cfg == nil || !cfg.UsesCodeBuddyOAuth() {
		return nil, errOAuthUsageUnsupported
	}
	if s == nil || s.codeBuddyService == nil || s.codeBuddyCredentials == nil {
		return nil, errCodeBuddyUsageManagerUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	operationCtx, cancel := context.WithTimeout(ctx, codeBuddyCheckinTimeout)
	defer cancel()
	cred, err := s.codeBuddyCredentials.credential(operationCtx, cfg, false, "")
	if err != nil {
		return nil, err
	}
	if !cred.SupportsDailyCheckin() {
		return nil, errOAuthUsageUnsupported
	}
	service := *s.codeBuddyService
	service.Client = s.getClientForChannel(cfg)
	checkinErr := service.DailyCheckin(operationCtx, cred)
	status := "success"
	if codebuddyauth.IsAlreadyCheckedIn(checkinErr) {
		status = "already_checked"
	}
	summary, err := requestCodeBuddyUsage(operationCtx, &service, cred)
	if err != nil {
		if checkinErr != nil && status != "already_checked" {
			return nil, errors.Join(fmt.Errorf("CodeBuddy check-in failed: %w", checkinErr), err)
		}
		return nil, err
	}
	requestedAt := time.Now().UTC()
	sampledAt := requestedAt
	summary, err = s.persistOAuthUsage(operationCtx, cfg, summary, requestedAt, sampledAt)
	if err != nil {
		return nil, errOAuthUsagePersistFailed
	}
	result := &codeBuddyCheckinResult{Status: status, Usage: summary}
	if checkinErr != nil && status != "already_checked" {
		return result, fmt.Errorf("CodeBuddy check-in failed: %w", checkinErr)
	}
	return result, nil
}

func isInternationalCodeBuddyConfig(cfg *model.Config) bool {
	if cfg == nil || !cfg.UsesCodeBuddyOAuth() {
		return false
	}
	credential, err := codebuddyauth.ParseCredential([]byte(cfg.OAuthCredential))
	return err == nil && credential.IsInternational()
}

func (s *Server) runCodeBuddyCheckin(ctx context.Context, cfg *model.Config) {
	if cfg == nil || ctx == nil || ctx.Err() != nil {
		return
	}
	result, err := s.checkInCodeBuddy(ctx, cfg)
	if err != nil && ctx.Err() == nil {
		log.Printf("[WARN] CodeBuddy 签到失败 channel=%d: %v", cfg.ID, err)
	}
	if ctx.Err() != nil {
		return
	}
	if auditErr := s.addCodeBuddyCheckinAuditLog(ctx, cfg, result, err); auditErr != nil {
		log.Printf("[WARN] CodeBuddy 签到审计日志写入失败（channel=%d）: %v", cfg.ID, auditErr)
	}
	if result != nil && result.Usage != nil && result.Usage.CodeBuddyCredits != nil {
		log.Printf("[INFO] CodeBuddy 余额 channel=%d remain=%g", cfg.ID, result.Usage.CodeBuddyCredits.Remain)
	}
}

/**
 * @brief 把 CodeBuddy 签到状态和积分归一为公共签到审计记录。
 * @param ctx 本次操作的取消与截止时间上下文。
 * @param cfg 本次操作使用的渠道配置及私有凭证快照。
 * @param result CodeBuddy 签到结果，可为 nil。
 * @param operationErr 业务操作错误；成功时为 nil。
 * @return 编码或写库失败返回错误，成功返回 nil。
 * @note 产生签到日志，不改变签到业务结果。
 */
func (s *Server) addCodeBuddyCheckinAuditLog(
	ctx context.Context,
	cfg *model.Config,
	result *codeBuddyCheckinResult,
	operationErr error,
) error {
	if s == nil || s.store == nil || cfg == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	status := ""
	statusCode := http.StatusOK
	var balance *channelCheckinAuditBalance
	if result != nil {
		status = result.Status
		if result.Usage != nil && result.Usage.CodeBuddyCredits != nil {
			balance = &channelCheckinAuditBalance{
				Remaining: result.Usage.CodeBuddyCredits.Remain,
				Unit:      "credits",
			}
		}
	}
	if operationErr != nil || status == "" {
		status = "failed"
		statusCode = http.StatusBadGateway
	}

	return s.writeChannelCheckinAuditLog(ctx, cfg, time.Now(), statusCode, channelCheckinAuditMessage{
		Profile: codebuddyauth.ChannelType,
		Status:  status,
		Balance: balance,
	})
}

func codeBuddyCheckinSlot(now time.Time) string {
	if now.Hour() < 9 {
		return ""
	}
	slot := "09"
	if now.Hour() >= 21 {
		slot = "21"
	}
	return now.Format("2006-01-02") + "-" + slot
}

/**
 * @brief 按管理账户的每日计划领取执行权并执行到期签到。
 * @param ctx 本次操作的取消与截止时间上下文。
 * @param now 用于判断业务日期与调度时点的绝对时间。
 * @return 扫描、领取执行权等失败返回错误；无到期任务或执行调度完成返回 nil。
 * @note 使用业务日领取机制抑制重复调度；取消后等待 worker 结束，再用独立的五秒上下文释放本轮未完成 claim。渠道启用状态不阻止管理账户签到。
 */
func (s *Server) runDueManagementCheckins(ctx context.Context, now time.Time) error {
	if s == nil || s.store == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	configs, err := s.store.ListConfigs(ctx)
	if err != nil {
		return err
	}

	// 和 isManagementCheckinDue 用同一个日历：day 是 CAS claim 的幂等键，
	// 两处若按不同时区取日期，claim 的日子和判定的日子会错位。
	day := now.Format("2006-01-02")
	targets := make([]*model.Config, 0, len(configs))
	pending := make(map[int64]*model.Config)
	var pendingMu sync.Mutex
	var scanErr error
	for _, cfg := range configs {
		if ctx.Err() != nil {
			scanErr = ctx.Err()
			break
		}
		if !supportsChannelManagement(cfg) {
			continue
		}
		envelope, parseErr := model.ParseChannelManagementEnvelope(cfg.OAuthCredential)
		if parseErr != nil || !isManagementCheckinDue(envelope, now) {
			continue
		}
		claimed, claimErr := s.claimManagementCheckinDay(ctx, cfg, day, now)
		if claimErr != nil {
			if scanErr == nil {
				scanErr = claimErr
			}
			log.Printf("[WARN] 管理账户每日签到渠道 claim 失败（channel=%d）: %v", cfg.ID, claimErr)
			continue
		}
		if claimed != nil {
			targets = append(targets, claimed)
			pending[claimed.ID] = claimed
		}
	}
	s.runChannelCheckinJobs(ctx, targets, func(ctx context.Context, cfg *model.Config) {
		s.runManagementCheckinJob(ctx, cfg.ID)
		if ctx.Err() == nil {
			// 普通业务失败也算当天已经尝试，只有调度取消才恢复执行资格。
			pendingMu.Lock()
			delete(pending, cfg.ID)
			pendingMu.Unlock()
		}
	})
	if ctx.Err() != nil && len(pending) > 0 {
		// worker 已全部退出，清理只恢复调度字段；下轮签到会先回读上游状态。
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		for _, claimed := range pending {
			if err := s.releaseManagementCheckinDay(cleanupCtx, claimed, day); err != nil {
				log.Printf("[WARN] 取消后恢复管理账户签到资格失败 channel=%d: %v", claimed.ID, err)
				scanErr = errors.Join(scanErr, err)
			}
		}
	}
	return errors.Join(scanErr, ctx.Err())
}

/**
 * @brief 通过 CAS 领取当天执行权并保留实际获胜快照，供取消时恢复。
 * @param ctx 领取执行权的上下文。
 * @param cfg 扫描得到的渠道快照，可为 nil。
 * @param day 与到期判定使用同一时区的业务日期。
 * @param now 本轮到期判定时间。
 * @return 成功 claim 前的渠道快照；不满足条件或并发领取失败时为 nil。
 * @return 编码、读写存储或上下文错误；未领取且没有存储错误时为 nil。
 * @note 最多两次 CAS，成功后持久化 LastScheduledDay，不修改传入快照。
 */
func (s *Server) claimManagementCheckinDay(
	ctx context.Context,
	cfg *model.Config,
	day string,
	now time.Time,
) (*model.Config, error) {
	if cfg == nil {
		return nil, nil
	}
	current := cfg
	// A failed CAS is re-read and re-evaluated. Two attempts are enough to
	// observe the winner of a concurrent claim while avoiding a retry loop on a
	// permanently failing store.
	for attempt := range 2 {
		envelope, err := model.ParseChannelManagementEnvelope(current.OAuthCredential)
		if err != nil || !supportsChannelManagement(current) || !isManagementCheckinDue(envelope, now) {
			return nil, nil
		}
		next := *envelope
		next.State.LastScheduledDay = day
		nextRaw, err := next.Marshal()
		if err != nil {
			return nil, err
		}
		updated, err := s.store.CompareAndSwapChannelManagement(ctx, current.ID, current.OAuthCredential, nextRaw)
		if err != nil {
			return nil, err
		}
		if updated {
			return current, nil
		}
		if attempt == 1 {
			return nil, nil
		}
		current, err = s.store.GetConfig(ctx, current.ID)
		if err != nil {
			return nil, err
		}
	}
	return nil, nil
}

/**
 * @brief 恢复被取消的当日调度资格，保留并发更新的管理账户状态。
 * @param ctx 独立且有截止时间的取消收尾上下文。
 * @param claimed 实际成功 claim 前的渠道快照。
 * @param day 本轮持久化的调度日期。
 * @return 读取、解析、编码、写库或三次 CAS 冲突耗尽时返回错误；恢复成功或身份、日期已变化时为 nil。
 * @note 仅恢复 LastScheduledDay；调用前必须等待本轮 worker 全部结束。
 */
func (s *Server) releaseManagementCheckinDay(ctx context.Context, claimed *model.Config, day string) error {
	source, err := model.ParseChannelManagementEnvelope(claimed.OAuthCredential)
	if err != nil {
		return err
	}
	for range 3 {
		current, err := s.store.GetConfig(ctx, claimed.ID)
		if err != nil {
			return err
		}
		if current == nil || !supportsChannelManagement(current) {
			return nil
		}
		envelope, err := model.ParseChannelManagementEnvelope(current.OAuthCredential)
		if err != nil {
			return err
		}
		if !sameChannelManagementIdentity(source, envelope) || envelope.State.LastScheduledDay != day {
			return nil
		}
		envelope.State.LastScheduledDay = source.State.LastScheduledDay
		next, err := envelope.Marshal()
		if err != nil {
			return err
		}
		updated, err := s.store.CompareAndSwapChannelManagement(ctx, current.ID, current.OAuthCredential, next)
		if err != nil || updated {
			return err
		}
	}
	return errors.New("management check-in claim release CAS contention")
}

func (s *Server) runManagementCheckinJob(ctx context.Context, channelID int64) {
	cfg, err := s.store.GetConfig(ctx, channelID)
	if err != nil {
		if ctx.Err() == nil {
			s.writeManagementCheckinAudit(ctx, &model.Config{ID: channelID}, nil, err)
		}
		return
	}
	if cfg == nil {
		return
	}
	envelope, parseErr := model.ParseChannelManagementEnvelope(cfg.OAuthCredential)
	if parseErr != nil {
		s.writeManagementCheckinAudit(ctx, cfg, nil, parseErr)
		return
	}
	if !supportsChannelManagement(cfg) ||
		(envelope.Profile != model.ChannelManagementProfileNewAPI && envelope.Profile != model.ChannelManagementProfileSub2APIPro) ||
		!envelope.Settings.DailyCheckinEnabled {
		s.writeManagementCheckinAudit(ctx, cfg, &channelCheckinResult{Status: newAPICheckinSkippedDisabled}, nil)
		return
	}
	result, checkinErr := s.channelManagement.CheckIn(ctx, channelID)
	if ctx.Err() != nil {
		return
	}
	s.writeManagementCheckinAudit(ctx, cfg, result, checkinErr)
}

func (s *Server) writeManagementCheckinAudit(ctx context.Context, cfg *model.Config, result *channelCheckinResult, err error) {
	if s == nil || s.store == nil || cfg == nil {
		return
	}
	auditCtx := ctx
	if auditCtx == nil || auditCtx.Err() != nil {
		auditCtx = context.Background()
	}
	if auditErr := s.addChannelCheckinAuditLog(auditCtx, cfg, result, err); auditErr != nil {
		log.Printf("[WARN] 管理账户每日签到审计日志写入失败（channel=%d）: %v", cfg.ID, auditErr)
	}
}

// isManagementCheckinDue interprets the configured HH:MM in the server's local
// calendar and intentionally ignores manual check-in state.
//
// 时区取自 now 自身而不是包级 time.Local：调用方传的 now 来自 ticker（即
// time.Now()），两者在生产中是同一个 Location。读全局会让这个本可以是纯函数的
// 判定挂上进程级依赖，测试为了控制时区只能去写 time.Local，那是真实的数据竞争
// ——同包里任何并行测试的 time.Now() 都在读它。
func isManagementCheckinDue(envelope *model.ChannelManagementEnvelope, now time.Time) bool {
	if envelope == nil || !envelope.Settings.DailyCheckinEnabled ||
		(envelope.Profile != model.ChannelManagementProfileNewAPI && envelope.Profile != model.ChannelManagementProfileSub2APIPro) {
		return false
	}
	location := now.Location()
	if envelope.State.LastScheduledDay == now.Format("2006-01-02") {
		return false
	}
	scheduledClock, err := time.ParseInLocation("15:04", envelope.Settings.DailyCheckinTime, location)
	if err != nil {
		return false
	}
	scheduled := time.Date(
		now.Year(), now.Month(), now.Day(),
		scheduledClock.Hour(), scheduledClock.Minute(), 0, 0, location,
	)
	return !now.Before(scheduled)
}
