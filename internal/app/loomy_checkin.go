package app

import (
	"context"
	"errors"
	"log"
	"math"
	"net/http"
	"time"

	"ccLoad/internal/loomyauth"
	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

/**
 * @brief 固定 Loomy 业务日与签到时点使用北京时间 UTC+8。
 */
var loomyLocation = time.FixedZone("UTC+8", 8*60*60)

/**
 * @brief 分别记录每日签到、新手任务与额度刷新结果，保留部分成功。
 * @details 附属任务或余额失败以告警表达，不撤销已经确认的每日签到成功。
 */
type loomyCheckinResult struct {
	Status     string             `json:"status"`             ///< 每日签到状态：success、already_checked 或 failed。
	Day        string             `json:"day,omitempty"`      ///< 上游确认的 YYYY-MM-DD 业务日，必要时按北京时间补齐。
	Onboarding string             `json:"onboarding"`         ///< 新手任务状态：not_attempted、pending、completed 或 skipped。
	Usage      *oauthUsageSummary `json:"usage,omitempty"`    ///< 公共额度刷新结果，nil 表示本次未取得快照。
	Warnings   []string           `json:"warnings,omitempty"` ///< 部分失败或持久化失败的说明，不撤销已确认的每日成功。
}

/**
 * @brief 判断当前业务日尚未签到且已经到达允许自动尝试的时间。
 * @param credential 非 nil 的已解析凭证，包含最近签到日与下次自动尝试时点。
 * @param now 用于判断业务日期与调度时点的绝对时间。
 * @return 北京时间达到 00:01、今天未确认签到且未早于下次尝试时间时返回 true。
 */
func loomyCheckinDue(credential *loomyauth.Credential, now time.Time) bool {
	local := now.In(loomyLocation)
	return (local.Hour() > 0 || local.Minute() >= 1) &&
		credential.LastCheckinDay != local.Format("2006-01-02") && !now.Before(credential.NextCheckinAttemptAt)
}

/**
 * @brief 选择严格晚于当前时刻的下一个北京时间签到时点。
 * @param now 用于判断业务日期与调度时点的绝对时间。
 * @return 返回当日 00:01、08:01 或次日 00:01 中严格晚于 now 的最早时点。
 * @details 08:01 后失败等待次日首签，不按扫描频率反复请求。
 */
func nextLoomyCheckinAttempt(now time.Time) time.Time {
	local := now.In(loomyLocation)
	for _, hour := range []int{0, 8} {
		next := time.Date(local.Year(), local.Month(), local.Day(), hour, 1, 0, 0, loomyLocation)
		if now.Before(next) {
			return next
		}
	}
	return time.Date(local.Year(), local.Month(), local.Day()+1, 0, 1, 0, 0, loomyLocation)
}

/**
 * @brief 独立调度 Loomy 签到，避免慢批次推迟其他提供商的签到扫描。
 * @note 启动前须计入服务等待组；启动时补偿扫描，此后每分钟检查，随服务上下文取消并等待本批工作结束后退出。
 * @details 同一循环串行等待整批任务，防止分钟扫描重叠；使用扫描时的实际时间判断到期，避免慢批次结束后沿用过期的 ticker 时间。
 */
func (s *Server) loomyCheckinLoop() {
	defer s.wg.Done()
	ctx := s.baseCtx
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(channelManagementScheduleInterval)
	defer ticker.Stop()
	s.runDueLoomyCheckins(ctx, time.Now())
	for {
		select {
		case <-s.shutdownCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runDueLoomyCheckins(ctx, time.Now())
		}
	}
}

/**
 * @brief 筛选启用且到期的 Loomy 渠道，复用公共签到工作队列。
 * @param ctx 本次操作的取消与截止时间上下文。
 * @param now 用于判断业务日期与调度时点的绝对时间。
 * @note 调用与手动签到相同的业务路径，并在账号锁内重验资格；产生签到审计，错误记日志。
 */
func (s *Server) runDueLoomyCheckins(ctx context.Context, now time.Time) {
	if s.loomyService == nil {
		return
	}
	configs, err := s.store.ListConfigs(ctx)
	if err != nil {
		log.Printf("[WARN] Loomy 签到扫描失败: %v", err)
		return
	}
	targets := make([]*model.Config, 0, len(configs))
	for _, cfg := range configs {
		if !cfg.UsesLoomySession() || !cfg.Enabled {
			continue
		}
		credential, err := loomyauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil || !loomyCheckinDue(credential, now) {
			continue
		}
		targets = append(targets, cfg)
	}
	s.runChannelCheckinJobs(ctx, targets, func(ctx context.Context, cfg *model.Config) {
		result, err := s.checkInLoomy(ctx, cfg, true)
		if result != nil {
			s.auditLoomyCheckin(ctx, cfg, result)
		}
		if err != nil && ctx.Err() == nil {
			log.Printf("[WARN] Loomy 签到失败 channel=%d: %v", cfg.ID, err)
		}
	})
}

/**
 * @brief 执行手动 Loomy 签到并返回每日领取及附属任务的独立状态。
 * @param c 携带路由参数、管理员身份和响应写入器的 HTTP 上下文。
 * @note 手动操作不受自动计划时点限制；已有结果时返回结果与告警，附属失败不撤销每日成功。
 */
func (s *Server) HandleLoomyCheckin(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondErrorMsg(c, 400, "invalid channel id")
		return
	}
	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondError(c, 404, errOAuthUsageChannelNotFound)
		return
	}
	if !cfg.UsesLoomySession() {
		RespondError(c, 409, errOAuthUsageUnsupported)
		return
	}
	result, err := s.checkInLoomy(c.Request.Context(), cfg, false)
	if result == nil {
		RespondError(c, oauthUsageHTTPStatus(err), err)
		return
	}
	s.auditLoomyCheckin(c.Request.Context(), cfg, result)
	RespondJSON(c, http.StatusOK, result)
}

/**
 * @brief 在账号锁内完成每日签到、首次任务检查及公共额度刷新。
 * @param ctx 本次操作的取消与截止时间上下文。
 * @param source 用于确定账号锁与渠道 ID 的原始配置，锁内会重新读取。
 * @param scheduled 是否自动调度；true 时再次校验启用状态、业务日及下次尝试时间。
 * @return 签到与附属任务结果；资格失效或尚未发起签到即失败时为 nil，远程操作后失败仍保留结果。
 * @return 凭证、账号锁、存储或每日签到失败时返回错误；会话失效可用 errors.Is 检查 loomyauth.ErrSessionExpired；正常完成或自动任务已无资格时为 nil。
 * @note 总时限 90 秒；远程签到前在内存预留下一固定时点，请求返回后持久化退避或成功状态。每日成功、任务完成标记和额度更新分别保留。
 */
func (s *Server) checkInLoomy(ctx context.Context, source *model.Config, scheduled bool) (*loomyCheckinResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	credential, err := loomyauth.ParseCredential([]byte(source.OAuthCredential))
	if err != nil {
		return nil, err
	}
	release, err := s.acquireLoomyAccount(ctx, credential.UserID)
	if err != nil {
		return nil, err
	}
	defer release()
	cfg, err := s.store.GetConfig(ctx, source.ID)
	if err != nil {
		return nil, err
	}
	current, err := loomyauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil, err
	}
	if !cfg.UsesLoomySession() || current.UserID != credential.UserID {
		return nil, errors.New("loomy account changed")
	}
	now := time.Now()
	if scheduled {
		if !cfg.Enabled || !loomyCheckinDue(current, now) {
			return nil, nil
		}
		if retry, ok := s.loomyState.retryAfter.Load(cfg.ID); ok && now.Before(retry.(time.Time)) {
			return nil, nil
		}
	}
	service, err := s.loomyClient(cfg)
	if err != nil {
		return nil, err
	}
	result := &loomyCheckinResult{Status: "failed", Onboarding: "not_attempted"}
	// 写库失败时也保留本进程的下次计划时间，防止扫描器重复签到。
	nextAttempt := nextLoomyCheckinAttempt(now)
	s.loomyState.retryAfter.Store(cfg.ID, nextAttempt)
	daily, err := service.DailyCheckin(ctx, current.Session)
	if err != nil {
		result.Warnings = append(result.Warnings, err.Error())
		s.handleLoomyRejection(ctx, cfg, err)
		_, saveErr := s.mergeLoomyCredential(ctx, cfg, func(c *loomyauth.Credential) { c.NextCheckinAttemptAt = nextAttempt })
		if saveErr != nil {
			result.Warnings = append(result.Warnings, "签到重试时间保存失败")
		}
		return result, err
	}
	result.Status = "success"
	if daily.AlreadyProcessed {
		result.Status = "already_checked"
	}
	day := daily.Day
	if day == "" {
		day = now.In(loomyLocation).Format("2006-01-02")
	}
	// 使用上游确认的业务日期，浏览器不得用响应到达时的日期替代。
	result.Day = day
	cfg, err = s.mergeLoomyCredential(ctx, cfg, func(c *loomyauth.Credential) {
		if day > c.LastCheckinDay {
			c.LastCheckinDay = day
		}
		// 每日额度只接受当次签到的实测值，次日不得沿用昨天的分母。
		if day > c.DailyCycleDate {
			c.DailyCycleDate = day
			c.DailyQuota = nil
		}
		if day == c.DailyCycleDate && daily.DailyQuota != nil && *daily.DailyQuota > 0 {
			c.DailyQuota = daily.DailyQuota
		}
		c.NextCheckinAttemptAt = nextAttempt
	})
	if err != nil {
		result.Warnings = append(result.Warnings, "每日签到已成功，但本地状态保存失败")
		return result, err
	}
	if current.OnboardingCompletedAt != "" {
		result.Onboarding = "skipped"
	} else {
		result.Onboarding = "pending"
		complete, taskErr := completeLoomyOnboarding(ctx, service, current.Session)
		if taskErr != nil {
			result.Warnings = append(result.Warnings, "新手任务待处理: "+taskErr.Error())
			if errors.Is(taskErr, loomyauth.ErrSessionExpired) {
				s.handleLoomyRejection(ctx, cfg, taskErr)
				return result, nil
			}
		}
		if complete {
			next, saveErr := s.mergeLoomyCredential(ctx, cfg, func(c *loomyauth.Credential) {
				if c.OnboardingCompletedAt == "" {
					c.OnboardingCompletedAt = time.Now().UTC().Format(time.RFC3339)
				}
			})
			if saveErr != nil {
				result.Warnings = append(result.Warnings, "新手任务已完成，但完成标记保存失败")
			} else {
				cfg = next
				result.Onboarding = "completed"
			}
		}
	}
	// 与手动/批量额度刷新共用超时、采样和 CAS，附属失败只记告警。
	usage, err := s.refreshOAuthUsage(ctx, cfg.ID)
	if err != nil {
		result.Warnings = append(result.Warnings, "余额刷新失败: "+err.Error())
	} else {
		result.Usage = usage
	}
	return result, nil
}

/**
 * @brief 检查八个已知新手任务，仅提交未完成任务并回读确认。
 * @param ctx 本次操作的取消与截止时间上下文。
 * @param service 已配置账号代理策略的协议服务。
 * @param session 经过上游验证的当前账号会话。
 * @return 全部已知任务经回读确认完成时为 true，未完成或失败时为 false。
 * @return 任务未完成、查询或提交失败时返回错误；会话失效可用 errors.Is 检查 loomyauth.ErrSessionExpired，完成时为 nil。
 * @note 可能领取任务积分；不确定的提交结果先回读，确定会话失效立即停止，调用方仅在全部完成后持久化永久标记。
 */
func completeLoomyOnboarding(ctx context.Context, service *loomyauth.Service, session string) (bool, error) {
	tasks, err := service.FetchTasks(ctx, session)
	if err != nil {
		return false, err
	}
	var taskErrors []error
	for _, key := range loomyauth.OnboardingKeys() {
		if tasks[key] {
			continue
		}
		err := service.CompleteTask(ctx, session, key)
		if errors.Is(err, loomyauth.ErrSessionExpired) {
			return false, err
		}
		if err != nil {
			taskErrors = append(taskErrors, err)
			if _, business := errors.AsType[*loomyauth.APIError](err); !business {
				// Unknown outcome: read back before doing any further side effects.
				tasks, err = service.FetchTasks(ctx, session)
				if err != nil {
					return false, err
				}
			}
		}
	}
	tasks, err = service.FetchTasks(ctx, session)
	if err != nil {
		return false, err
	}
	for _, done := range tasks {
		if !done {
			return false, errors.Join(append(taskErrors, errors.New("部分新手任务尚未完成，下次签到重试"))...)
		}
	}
	return true, nil
}

/**
 * @brief 只读获取原生积分，并以当前业务日的可靠数据推导每日总额。
 * @param ctx 本次操作的取消与截止时间上下文。
 * @param cfg 本次操作使用的渠道配置及私有凭证快照。
 * @return 额度快照；非认证流水失败仍保留余额并添加告警，凭证或余额读取失败时为 nil。
 * @return 凭证、客户端或余额请求失败时返回错误；会话失效可用 errors.Is 检查 loomyauth.ErrSessionExpired，成功或仅流水推导失败时为 nil。
 * @note 不触发签到或任务；明确会话失效会按快照尝试禁用渠道。
 * @details 总额优先取当日签到实测，其次当日缓存，最后完整流水；跨北京日界放弃分母，永久积分不虚构总额。
 */
func (s *Server) requestLoomyUsage(ctx context.Context, cfg *model.Config) (*oauthUsageSummary, error) {
	credential, err := loomyauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil, err
	}
	service, err := s.loomyClient(cfg)
	if err != nil {
		return nil, err
	}
	day := time.Now().In(loomyLocation).Format("2006-01-02")
	balance, err := service.FetchBalance(ctx, credential.Session)
	if err != nil {
		s.handleLoomyRejection(ctx, cfg, err)
		return nil, err
	}
	summary := &oauthUsageSummary{Provider: loomyauth.ChannelType, Windows: []oauthUsageWindow{}, LoomyCredits: balance}
	quota := credential.DailyQuota
	if credential.DailyCycleDate != day || quota == nil || *quota <= 0 {
		quota = nil
		// 流水推导结果随余额快照持久化；当天后续刷新无需再次翻页。
		previous, _, _ := persistedOAuthUsage(credential.OAuthUsage, loomyauth.ChannelType)
		if previous != nil && previous.LoomyCredits != nil && previous.LoomyCredits.DailyCycleDate == day {
			quota = previous.LoomyCredits.DailyQuota
		}
	}
	if quota == nil && balance.Daily != nil {
		consumed, foundToday, ledgerErr := service.FetchDailyConsumed(ctx, credential.Session, day)
		if errors.Is(ledgerErr, loomyauth.ErrSessionExpired) {
			s.handleLoomyRejection(ctx, cfg, ledgerErr)
			return nil, ledgerErr
		}
		if ledgerErr == nil && (foundToday || *balance.Daily > 0) {
			value := math.Max(0, *balance.Daily) + consumed
			if value > 0 {
				quota = &value
			}
		} else if ledgerErr != nil {
			summary.Warnings = append(summary.Warnings, "每日积分总额暂不可用")
		}
	}
	// 读取跨越北京日界线时放弃本次分母，不能把两个业务日混算。
	if time.Now().In(loomyLocation).Format("2006-01-02") == day && quota != nil && *quota > 0 && balance.Daily != nil {
		used := math.Max(0, *quota-*balance.Daily)
		balance.DailyQuota, balance.DailyConsumed, balance.DailyCycleDate = quota, &used, day
	}
	return summary, nil
}

/**
 * @brief 将 Loomy 每日签到、任务状态和告警写入公共审计日志。
 * @param ctx 本次操作的取消与截止时间上下文。
 * @param cfg 本次操作使用的渠道配置及私有凭证快照。
 * @param result 非 nil 的本次签到结果。
 * @note 失败只记录日志，不改变已返回的业务状态。
 */
func (s *Server) auditLoomyCheckin(ctx context.Context, cfg *model.Config, result *loomyCheckinResult) {
	status := http.StatusOK
	if result.Status == "failed" {
		status = http.StatusBadGateway
	}
	if err := s.writeChannelCheckinAuditLog(ctx, cfg, time.Now(), status, struct {
		Profile             string `json:"profile"` ///< 审计记录所属的提供商标识。
		*loomyCheckinResult        ///< 内嵌签到结果，使审计状态与管理接口返回保持一致。
	}{loomyauth.ChannelType, result}); err != nil {
		log.Printf("[WARN] Loomy 签到日志保存失败 channel=%d: %v", cfg.ID, err)
	}
}
