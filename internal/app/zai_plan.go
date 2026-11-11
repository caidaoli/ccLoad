package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
	"ccLoad/internal/testutil"
	"ccLoad/internal/util"
	"ccLoad/internal/zaiauth"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/sync/singleflight"
)

/**
 * @brief 按渠道保存短期套餐采样；身份摘要防止旧登录污染新账号。
 */
type zaiPlanCache struct {
	mu          sync.Mutex             ///< 保护 entries 和 generations。
	quotaMu     sync.Mutex             ///< 只串行体验冷却写入，不阻塞套餐查询和正常选路。
	entries     map[int64]zaiPlanEntry ///< 每渠道最多一个采样。
	generations map[int64]uint64       ///< 失效代次，隔离在途查询的回填及结果复用。
	flights     singleflight.Group     ///< 合并相同身份的并发查询。
}

/**
 * @brief 仅识别已确认的体验额度业务码，未知 429 不代表余额耗尽。
 * @param status 实际错误状态，可由 HTTP 200 错误信封或首个 SSE 错误归一得到。
 * @param body 结构化上游错误体。
 * @return 额度不足或额度类限流返回 true；3008/3009/3010 和并发忙返回 false。
 */
func zaiGiftQuotaError(status int, body []byte) bool {
	if status < 200 || !json.Valid(body) {
		return false
	}
	// 非 429 保留内外两层额度码识别，嵌套非额度码不能覆盖外层额度码。
	outerCode := gjson.GetBytes(body, "code").String()
	code := gjson.GetBytes(body, "error.code").String()
	if code == "" {
		code = outerCode
	}
	return code == "1113" || code == "1005" ||
		outerCode == "1113" || outerCode == "1005"
}

/**
 * @brief 识别 ZCode 结构化错误中的账号并发忙，供额度分类和 Trial 退避共用。
 * @param body 上游错误 JSON；只读取错误码与错误消息字段，不扫描任意文本。
 * @return 3008、3009、3010 或明确并发超限消息返回 true；无效 JSON 返回 false。
 * @note 调用方负责核对 HTTP 429；不决定套餐回退、冷却或 JSON/SSE 状态映射。
 */
func zaiConcurrencyError(body []byte) bool {
	if !json.Valid(body) {
		return false
	}
	// 嵌套错误码优先，外层成功码不能掩盖真实并发错误。
	code := gjson.GetBytes(body, "error.code").String()
	if code == "" {
		code = gjson.GetBytes(body, "code").String()
	}
	if code == "3008" || code == "3009" || code == "3010" {
		return true
	}
	for _, path := range []string{"msg", "message", "error.message", "error"} {
		value := gjson.GetBytes(body, path)
		if value.Type == gjson.String && strings.Contains(strings.ToLower(value.String()), "concurrency limit exceeded") {
			return true
		}
	}
	return false
}

/**
 * @brief ZAI 薄适配的决策，仅在体验资源范围内执行公共分类结果。
 */
type zaiTrialDecision struct {
	cooldown.Decision        ///< 通用错误动作及明确截止时间。
	scope             string ///< model、account 或 group；仅 ZAI 解释此范围。
	status            int    ///< 业务码归一后的状态，用于通用时间策略。
	authFailure       bool   ///< JWT 认证拒绝，可由成功重新授权解除；不包含账号风控。
}

/**
 * @brief 读取完整 ZCode 错误的业务码，嵌套错误优先于外层信封。
 * @param body 上游 JSON；截断或非 JSON 不参与业务判定。
 * @return 数字或字符串业务码；没有可识别错误码时为空。
 */
func zaiErrorCode(body []byte) string {
	if !json.Valid(body) {
		return ""
	}
	code := gjson.GetBytes(body, "error.code").String()
	if code == "" {
		code = gjson.GetBytes(body, "code").String()
	}
	return code
}

/**
 * @brief 归一体验业务错误，再使用公共无副作用决策；不写库、不修改调度。
 * @param cfg 实际发送时的渠道快照。
 * @param in 真实体验错误，不含本地拒绝。
 * @return 限定体验范围的动作和截止时间；客户端错误不冷却。
 */
func (s *Server) evaluateZAITrialCooldown(cfg *model.Config, in cooldown.ErrorInput) zaiTrialDecision {
	quota := !in.IsNetworkError && zaiGiftQuotaError(in.StatusCode, in.ErrorBody)
	code := zaiErrorCode(in.ErrorBody)
	if !in.IsNetworkError && code == "3007" {
		// 人机验证不是模型或凭证失效，换号与冷却都无法完成验证。
		var decision zaiTrialDecision
		decision.Action, decision.status = cooldown.ActionReturnClient, in.StatusCode
		return decision
	}
	message := strings.ToLower(gjson.GetBytes(in.ErrorBody, "error.message").String() + " " + gjson.GetBytes(in.ErrorBody, "message").String() + " " + gjson.GetBytes(in.ErrorBody, "msg").String())
	risk := !in.IsNetworkError && (code == "3012" || strings.Contains(message, "unusual activity") || strings.Contains(message, "request has been blocked"))
	if quota || !in.IsNetworkError && zaiConcurrencyError(in.ErrorBody) {
		in.StatusCode = http.StatusTooManyRequests
	}
	if risk {
		in.StatusCode = http.StatusForbidden
	}
	// 内容本身无法处理时不切套餐；宽泛的配置规则也不应把坏请求误冻成账号故障。
	if !in.IsNetworkError && !quota && !risk && in.StatusCode != 404 && in.StatusCode != 405 && isZAIClientRequestError(in.StatusCode, in.Headers, in.ErrorBody) {
		var decision zaiTrialDecision
		decision.Action, decision.status = cooldown.ActionReturnClient, in.StatusCode
		return decision
	}
	in.KeyIndex = cooldown.NoKeyIndex
	in.ModelScoped = in.IsNetworkError
	in = s.completeCooldownInput(cfg, in)
	d := zaiTrialDecision{Decision: s.cooldownManager.Evaluate(in), scope: "model", status: in.StatusCode}
	if !in.IsNetworkError && zaiConcurrencyError(in.ErrorBody) && d.Until.IsZero() {
		// busy 的相对提示在发送边界转成绝对时间；过期日期不再开启新窗口。
		if until, _ := util.ParseRetryAfter(in.Headers, time.Now()); !until.IsZero() {
			d.Until = until
		}
	}
	if d.Action == cooldown.ActionRetryChannel || d.Action == cooldown.ActionRetryKey {
		d.scope = "account"
	}
	if !in.IsNetworkError && in.StatusCode == 405 {
		body := strings.ToLower(string(in.ErrorBody))
		// HTML 封禁页或 Aliyun 专有特征都足以确认出口拒绝，二者无需同时出现。
		if strings.Contains(body, "<html") || strings.Contains(body, "errors.aliyun.com") {
			d.scope = "group"
		}
	}
	if risk {
		d.scope = "account"
	}
	d.authFailure = d.scope == "account" && !risk && (in.StatusCode == http.StatusUnauthorized || in.StatusCode == http.StatusForbidden)
	// 固定体验端点没有协议探测机会，真实 404/405 不能被当成本地配置拒绝。
	if d.Action == cooldown.ActionReturnClient && (risk || in.StatusCode == 404 || in.StatusCode == 405) {
		d.Action = cooldown.ActionRetryModel
	}
	if !in.IsNetworkError && code == "3012" {
		// 明确风控仍暂停账号，但停止当前请求的付费回退及后续渠道重放。
		d.Action = cooldown.ActionReturnClient
	}
	d.Model = zaiTrialModelKey(in.Model)
	return d
}

/**
 * @brief 在真实账号边界执行体验冷却，不使用公共渠道冷却或资源耗尽升级。
 * @param ctx 请求上下文；调用方取消时不写入。
 * @param cfg 实际发送时的渠道及凭证快照。
 * @param in 真实体验失败，网络失败不解释为额度耗尽。
 * @return 后续路由动作；写入失败或旧认证错误不改变原有故障转移。
 * @return 冷却策略接受该失败时为 true；客户端错误、取消、凭证无效或过期认证错误为 false。
 * @note 同账号重复渠道共享一个窗口；CAS 防止旧账号结果污染新账号，付费资格独立。
 */
func (s *Server) applyZAITrialCooldown(ctx context.Context, cfg *model.Config, in cooldown.ErrorInput) (cooldown.Action, bool) {
	d := s.evaluateZAITrialCooldown(cfg, in)
	if (d.Action == cooldown.ActionReturnClient && d.scope != "account") || ctx.Err() != nil {
		return d.Action, false
	}
	credential, err := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return d.Action, false
	}
	identity := zaiStartIdentity(credential)
	if d.scope != "model" {
		// 与套餐查询共用临界区，并保持 plans → guard 锁序；旧失败不能在新采样之后失效缓存。
		s.zaiPlans.mu.Lock()
		defer s.zaiPlans.mu.Unlock()
		if !s.zaiStart.cool(credential, d) {
			return cooldown.ActionRetryModel, false
		}
		if d.scope == "account" {
			s.invalidateZAIGiftPlansLocked(cfg)
		}
		if d.Action == cooldown.ActionReturnClient {
			return d.Action, true
		}
		return cooldown.ActionRetryModel, true
	}
	s.zaiPlans.quotaMu.Lock()
	defer s.zaiPlans.quotaMu.Unlock()
	cooldownCtx, cancel := cooldownWriteContext(ctx)
	defer cancel()
	configs, listErr := s.store.ListConfigs(cooldownCtx)
	if listErr != nil {
		configs = []*model.Config{cfg}
	}
	var targets []*model.Config
	for _, candidate := range configs {
		if !candidate.UsesZAIOAuth() {
			continue
		}
		other, err := zaiauth.ParseCredential([]byte(candidate.OAuthCredential))
		if err == nil && zaiStartIdentity(other) == identity {
			targets = append(targets, candidate)
		}
	}
	cooldowns, err := s.store.GetAllModelCooldowns(cooldownCtx)
	if err != nil {
		log.Printf("[WARN] 读取 ZCode 体验冷却失败，保留已有窗口 (channel=%d): %v", cfg.ID, err)
		return cooldown.ActionRetryModel, true
	}
	now := time.Now()
	var until time.Time
	for _, target := range targets {
		if existing := cooldowns[target.ID][d.Model]; existing.After(now) && (until.IsZero() || existing.After(until)) {
			until = existing
		}
	}
	if until.IsZero() {
		until = d.Until
	}
	// 没有有效窗口时，各渠道的历史各递增一次，再共享最长窗口；新渠道不能重置账号退避。
	if until.IsZero() {
		for _, target := range targets {
			next, matched, err := s.bumpZAITrialCooldown(cooldownCtx, target, d.Model, now, d.status)
			if err != nil {
				log.Printf("[WARN] 更新 ZCode 体验冷却失败 (channel=%d): %v", target.ID, err)
				// 未能读完账号历史时，不能传播可能更短的窗口覆盖其他渠道。
				until = time.Time{}
				break
			}
			if matched {
				if next.After(until) {
					until = next
				}
				if cooldowns[target.ID] == nil {
					cooldowns[target.ID] = make(map[string]time.Time)
				}
				cooldowns[target.ID][d.Model] = next.Truncate(time.Second)
				s.invalidateChannelRelatedCache(target.ID)
			}
		}
	}
	for _, target := range targets {
		if until.After(now) && !cooldowns[target.ID][d.Model].Equal(until.Truncate(time.Second)) {
			// 已有相同窗口不能再 Set：剩余时间会覆盖其完整的指数历史。
			if err := s.setZAITrialCooldown(cooldownCtx, target, d.Model, until); err != nil {
				log.Printf("[WARN] 传播 ZCode 体验冷却失败 (channel=%d): %v", target.ID, err)
			}
		}
		if !in.IsNetworkError && zaiGiftQuotaError(in.StatusCode, in.ErrorBody) {
			s.zaiPlans.mu.Lock()
			s.invalidateZAIGiftPlansLocked(target)
			s.zaiPlans.mu.Unlock()
		}
		s.invalidateChannelRelatedCache(target.ID)
	}
	return cooldown.ActionRetryModel, true
}

/**
 * @brief 同账号凭证刷新可重试条件递增，换账号则停止写入。
 * @param ctx 冷却写入上下文。
 * @param target 同账号渠道快照。
 * @param name Trial 模型冷却键。
 * @param now 本次错误时间。
 * @param status 已归一的状态码。
 * @return 成功写入的截止时间，拒写为零值。
 * @return 身份仍匹配且写入成功为 true，否则为 false。
 * @return 数据库失败或取消时返回错误。
 */
func (s *Server) bumpZAITrialCooldown(ctx context.Context, target *model.Config, name string, now time.Time, status int) (time.Time, bool, error) {
	original, err := zaiauth.ParseCredential([]byte(target.OAuthCredential))
	if err != nil {
		return time.Time{}, false, err
	}
	for attempt := 0; ; attempt++ {
		until, matched, err := s.store.BumpModelCooldownIfOAuthCredential(ctx, target.ID, target.AuthType, target.OAuthCredential, name, now, status)
		if err != nil || matched {
			return until, matched, err
		}
		current, err := s.store.GetConfig(ctx, target.ID)
		if err != nil {
			return time.Time{}, false, err
		}
		credential, err := zaiauth.ParseCredential([]byte(current.OAuthCredential))
		if !current.UsesZAIOAuth() || err != nil || zaiStartIdentity(credential) != zaiStartIdentity(original) {
			return time.Time{}, false, nil
		}
		if err := waitOAuthCASRetry(ctx, attempt); err != nil {
			return time.Time{}, false, err
		}
		target = current
	}
}

/**
 * @brief 真实体验成功后恢复旧退避历史，保留请求开始后的新失败窗口。
 * @param ctx 独立限时的冷却写入上下文。
 * @param cfg 成功请求的凭证快照。
 * @param name 实际体验模型。
 * @param started 成功请求开始时间；零值不执行清理。
 * @note 只清理同真实账号的 Trial 模型，不修改公共渠道或付费冷却。
 */
func (s *Server) recoverZAITrialCooldown(ctx context.Context, cfg *model.Config, name string, started time.Time) {
	if started.IsZero() {
		return
	}
	credential, err := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return
	}
	identity := zaiStartIdentity(credential)
	s.zaiStart.mu.Lock()
	if pause := s.zaiStart.cooldowns[identity]; !pause.until.After(started) {
		delete(s.zaiStart.cooldowns, identity)
	}
	if !s.zaiStart.until.After(started) {
		s.zaiStart.until, s.zaiStart.duration = time.Time{}, 0
	}
	s.zaiStart.mu.Unlock()
	s.zaiPlans.quotaMu.Lock()
	defer s.zaiPlans.quotaMu.Unlock()
	// 活动冷却缓存不包含过期退避历史；直接查询持久化候选，重启后也能恢复。
	modelKey := zaiTrialModelKey(name)
	configs, err := s.store.ListExpiredOAuthModelCooldowns(ctx, model.AuthTypeZAIOAuth, modelKey, started)
	if err != nil {
		log.Printf("[WARN] 读取 ZCode 体验恢复目标失败: %v", err)
		return
	}
	for _, target := range configs {
		if !target.UsesZAIOAuth() {
			continue
		}
		other, err := zaiauth.ParseCredential([]byte(target.OAuthCredential))
		if err != nil || zaiStartIdentity(other) != identity {
			continue
		}
		changed, err := s.resetZAITrialCooldown(ctx, target, modelKey, started)
		if err != nil {
			log.Printf("[WARN] 恢复 ZCode 体验退避失败 (channel=%d): %v", target.ID, err)
		} else if changed {
			s.invalidateChannelRelatedCache(target.ID)
		}
	}
}

/**
 * @brief 同账号凭证更新后重试清理旧历史，换账号或没有可清理历史时停止。
 * @param ctx 独立限时的存储上下文；取消时终止重试。
 * @param target 成功请求所属账号的渠道快照。
 * @param name 体验模型冷却键。
 * @param cutoff 成功请求发送时间，晚于此时的冷却始终保留。
 * @return 实际清除旧历史返回 true，身份变化或无旧历史返回 false。
 * @return 凭证解析、存储、取消或达到 CAS 重试上限时返回错误，否则 nil。
 * @note 每次清理均由存储事务原子校验完整凭证和截止时间。
 */
func (s *Server) resetZAITrialCooldown(ctx context.Context, target *model.Config, name string, cutoff time.Time) (bool, error) {
	original, err := zaiauth.ParseCredential([]byte(target.OAuthCredential))
	if err != nil {
		return false, err
	}
	for attempt := 0; ; attempt++ {
		changed, err := s.store.ResetExpiredModelCooldownIfOAuthCredential(ctx, target.ID, target.AuthType, target.OAuthCredential, name, cutoff)
		if err != nil || changed {
			return changed, err
		}
		current, err := s.store.GetConfig(ctx, target.ID)
		if err != nil {
			return false, err
		}
		// false 既可能是凭证变化，也可能没有过期历史；未变化时不能无限重试。
		if current.AuthType == target.AuthType && current.OAuthCredential == target.OAuthCredential {
			return false, nil
		}
		credential, err := zaiauth.ParseCredential([]byte(current.OAuthCredential))
		if !current.UsesZAIOAuth() || err != nil || zaiStartIdentity(credential) != zaiStartIdentity(original) {
			return false, nil
		}
		if err := waitOAuthCASRetry(ctx, attempt); err != nil {
			return false, err
		}
		target = current
	}
}

/**
 * @brief 不可变套餐采样及其失效边界。
 */
type zaiPlanEntry struct {
	identity  [32]byte       ///< 凭证与出口摘要。
	account   [32]byte       ///< 采样所属真实账号；渠道停用或冷却不抹掉余额事实。
	plans     *zaiauth.Plans ///< 只读采样。
	expires   time.Time      ///< 有效期不跨套餐开始或结束。
	giftStale bool           ///< 体验失效不抹掉仍有效的付费资格。
}

/**
 * @brief 为套餐查询复用渠道出口并保留可注入的控制面配置。
 * @param cfg 渠道配置。
 * @param credential 本次请求实际使用的账号及站点，导入时可能尚未写入渠道。
 * @return 请求专用 Service 副本。
 */
func (s *Server) zaiPlanService(cfg *model.Config, credential *zaiauth.Credential) *zaiauth.Service {
	return s.zaiService.ForSite(credential.Site, s.getClientForChannel(cfg))
}

/**
 * @brief 标识套餐采样使用的身份、认证材料及出口，排除偏好和额度快照。
 * @param cfg 采样使用的渠道出口。
 * @param credential 实际发送上游请求的凭证。
 * @return 可比较的摘要，不含可恢复的凭证原文。
 */
func zaiPlanIdentity(cfg *model.Config, credential *zaiauth.Credential) [32]byte {
	material, _ := json.Marshal([]string{string(credential.Site), credential.APIKey, credential.JWTToken, credential.DeviceID, credential.UserID, credential.Email, cfg.ProxyURL})
	return sha256.Sum256(material)
}

/**
 * @brief 获取短期套餐快照，查询有独立截止时间，取消只影响本次等待。
 * @param ctx 查询上下文。
 * @param cfg 渠道配置及代理出口。
 * @param credential 已解析凭证。
 * @param force 是否跳过已缓存快照。
 * @return 不可变套餐快照。
 * @return 查询或等待取消时返回错误。
 * @note 失效前启动的查询既不回填缓存，也不向等待者返回旧资格。
 * @note ID 为 0 的未保存渠道直接查询，不读写渠道缓存；取消会中止该次查询。
 */
func (s *Server) zaiAccountPlans(ctx context.Context, cfg *model.Config, credential *zaiauth.Credential, force bool) (*zaiauth.Plans, error) {
	if cfg.ID == 0 {
		// 导入验证和保存前模型发现须读取当前上游状态，不占用渠道缓存槽位。
		return s.zaiPlanService(cfg, credential).FetchPlans(ctx, credential)
	}
	cache := &s.zaiPlans
	identity := zaiPlanIdentity(cfg, credential)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		cache.mu.Lock()
		entry, ok := cache.entries[cfg.ID]
		generation := cache.generations[cfg.ID]
		cache.mu.Unlock()
		if !force && ok && !entry.giftStale && entry.identity == identity && time.Now().Before(entry.expires) {
			return entry.plans, nil
		}
		key := fmt.Sprintf("%d/%x/%d", cfg.ID, identity, generation)
		result := cache.flights.DoChan(key, func() (any, error) {
			queryCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
			defer cancel()
			if s.baseCtx != nil {
				stop := context.AfterFunc(s.baseCtx, cancel)
				defer stop()
			}
			queryStarted := time.Now()
			plans, err := s.zaiPlanService(cfg, credential).FetchPlans(queryCtx, credential)
			if err != nil {
				return nil, err
			}
			ttl := 10 * time.Minute
			if len(plans.Warnings) > 0 {
				ttl = time.Minute
			}
			now := time.Now()
			oldestObservation := plans.ObservedAt - int64(now.Sub(queryStarted)/time.Second)
			for _, gift := range plans.Gifts {
				for _, boundary := range []int64{gift.StartsAt, gift.ExpiresAt} {
					// 查询期间跨过权益边界时不缓存，下一次查询须确认上游是否已发放新桶。
					if boundary > oldestObservation && boundary <= plans.ObservedAt {
						ttl = 0
					}
					if remaining := time.Duration(boundary-plans.ObservedAt) * time.Second; boundary > plans.ObservedAt && remaining < ttl {
						ttl = remaining
					}
				}
			}
			cache.mu.Lock()
			if cache.entries == nil {
				cache.entries = make(map[int64]zaiPlanEntry)
			}
			// 失效前启动的查询不能恢复已被上游拒绝的资格。
			if cache.generations[cfg.ID] == generation {
				cache.entries[cfg.ID] = zaiPlanEntry{identity: identity, account: zaiStartIdentity(credential), plans: plans, expires: now.Add(ttl)}
			}
			cache.mu.Unlock()
			return plans, nil
		})
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case result := <-result:
			cache.mu.Lock()
			stale := cache.generations[cfg.ID] != generation
			cache.mu.Unlock()
			if stale {
				// 重新加入当前代次，避免 singleflight 把失效前的结果交给新请求。
				force = false
				continue
			}
			if result.Err != nil {
				return nil, result.Err
			}
			return result.Val.(*zaiauth.Plans), nil
		}
	}
}

/**
 * @brief 失效单个渠道的套餐采样。
 * @param id 渠道 ID。
 */
func (s *Server) invalidateZAIPlans(id int64) {
	s.zaiPlans.mu.Lock()
	if s.zaiPlans.generations == nil {
		s.zaiPlans.generations = make(map[int64]uint64)
	}
	s.zaiPlans.generations[id]++
	delete(s.zaiPlans.entries, id)
	s.zaiPlans.mu.Unlock()
}

/**
 * @brief 凭证并发更新时重新核对真实账号；换账号立即停止，刷新同账号凭证可重试。
 */
func (s *Server) setZAITrialCooldown(ctx context.Context, target *model.Config, name string, until time.Time) error {
	original, err := zaiauth.ParseCredential([]byte(target.OAuthCredential))
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		matched, err := s.store.SetModelCooldownIfOAuthCredential(ctx, target.ID, target.AuthType, target.OAuthCredential, name, until)
		if err != nil || matched {
			return err
		}
		current, err := s.store.GetConfig(ctx, target.ID)
		if err != nil {
			return err
		}
		credential, err := zaiauth.ParseCredential([]byte(current.OAuthCredential))
		if !current.UsesZAIOAuth() || err != nil || zaiStartIdentity(credential) != zaiStartIdentity(original) {
			return nil
		}
		if err := waitOAuthCASRetry(ctx, attempt); err != nil {
			return err
		}
		target = current
	}
}

/**
 * @brief 只失效同账号体验采样，保留付费资格及原有效期，并阻止旧查询回填。
 * @param cfg 失败所属账号的渠道凭证快照。
 * @note 调用方必须持有 zaiPlans.mu；认证失败的代次校验也必须在同一临界区完成。
 */
func (s *Server) invalidateZAIGiftPlansLocked(cfg *model.Config) {
	credential, err := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return
	}
	cache := &s.zaiPlans
	entry, exists := cache.entries[cfg.ID]
	if exists && entry.account != zaiStartIdentity(credential) {
		return
	}
	if cache.generations == nil {
		cache.generations = make(map[int64]uint64)
	}
	cache.generations[cfg.ID]++
	if exists {
		entry.giftStale = true
		cache.entries[cfg.ID] = entry
	}
}

/**
 * @brief 当前渠道的付费资格独立于体验采样失效，过期或身份不匹配必须重新查询。
 */
func (s *Server) zaiCachedPaid(cfg *model.Config, credential *zaiauth.Credential) bool {
	s.zaiPlans.mu.Lock()
	defer s.zaiPlans.mu.Unlock()
	entry, ok := s.zaiPlans.entries[cfg.ID]
	return ok && entry.identity == zaiPlanIdentity(cfg, credential) && time.Now().Before(entry.expires) && entry.plans.CanUsePaid(credential)
}

/**
 * @brief 判断本地显式体验模型别名，不全局剥离以保留路由身份。
 * @param name 模型名称。
 * @return 以 -Trial 结尾时为 true。
 */
func isZAITrialModel(name string) bool {
	return strings.HasSuffix(strings.ToLower(model.RoutingModelName(name)), "-trial")
}

/**
 * @brief 修改渠道体验优先偏好，只合并一个布尔键，不接受客户端凭证。
 * @param c 已认证管理请求。
 * @note CAS 冲突后基于最新凭证重试；成功后失效凭证和列表缓存。
 */
func (s *Server) HandleZAIOptions(c *gin.Context) {
	id, err := ParseInt64Param(c, "id")
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	var input struct {
		FreeFirst *bool `json:"free_first"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || input.FreeFirst == nil {
		RespondErrorMsg(c, http.StatusBadRequest, "free_first must be a boolean")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		RespondErrorMsg(c, http.StatusBadRequest, "unexpected trailing JSON")
		return
	}
	ctx := c.Request.Context()
	for attempt := 0; ; attempt++ {
		cfg, err := s.store.GetConfig(ctx, id)
		if err != nil {
			RespondErrorMsg(c, http.StatusNotFound, "channel not found")
			return
		}
		if !cfg.UsesZAIOAuth() {
			RespondErrorMsg(c, http.StatusConflict, "channel does not use Z.ai")
			return
		}
		credential, err := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
		if err != nil {
			RespondError(c, http.StatusInternalServerError, err)
			return
		}
		credential.FreeFirst = input.FreeFirst
		raw, err := credential.JSON()
		if err != nil {
			RespondError(c, http.StatusInternalServerError, err)
			return
		}
		updated, err := s.store.CompareAndSwapOAuthCredential(ctx, id, model.AuthTypeZAIOAuth, cfg.OAuthCredential, raw)
		if err != nil {
			RespondError(c, http.StatusInternalServerError, err)
			return
		}
		if updated {
			if s.zaiCredentials != nil {
				s.zaiCredentials.invalidate(id)
			}
			s.invalidateChannelRelatedCache(id)
			s.InvalidateChannelListCache()
			RespondJSON(c, http.StatusOK, gin.H{"free_first": *input.FreeFirst})
			return
		}
		if err := waitOAuthCASRetry(ctx, attempt); err != nil {
			RespondError(c, http.StatusConflict, err)
			return
		}
	}
}

/**
 * @brief 在公共 HTTP 发送边界选择套餐；每次实际请求单独占用额度。
 * @param cfg 请求渠道快照，不修改共享配置。
 * @param req 公共转换和规则后的候选请求，Body 必须可重读。
 * @param send 实际发送回调，每次调用独立预留 RPM 和并发并负责释放。
 * @return 最终响应，Request 指向实际发送的请求。
 * @return 本地准备、取消、预留或传输错误。
 * @note 所有本地资格拒绝在调用 send 之前完成，不消耗推理容量；体验最终失败按证据冷却，调用方取消不登记上游故障。
 */
func (s *Server) sendZAIPlanRequest(cfg *model.Config, req *http.Request, send func(*http.Request) (*http.Response, error)) (result *http.Response, resultErr error) {
	credential, err := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil, err
	}
	if zaiState(req) == nil {
		// 独立管理/内部调用也走同一准备边界，不要求测试或调用者预构造付费 wire。
		captureZAISources(req, credential.APIKey, req.Header, nil, anthropicCallerWire{})
	}
	state := zaiState(req)
	logical, _ := req.Context().Value(zaiStartRequestKey{}).(*zaiStartRequest)
	if logical == nil {
		logical = &zaiStartRequest{}
		req = req.WithContext(context.WithValue(req.Context(), zaiStartRequestKey{}, logical))
	}
	originalSend := send
	send = func(outbound *http.Request) (*http.Response, error) {
		logical.sends.Add(1)
		// 取消优先及套餐归因仅用于 ZAI，公共发送保留原有预留和重定向语义。
		if err := outbound.Context().Err(); err != nil {
			return nil, err
		}
		resp, err := originalSend(outbound)
		if !errors.Is(err, ErrChannelRPMExceeded) && !errors.Is(err, ErrChannelConcurrencyExceeded) {
			if attempt, _ := outbound.Context().Value(zaiSendAttemptKey{}).(*zaiSendAttempt); attempt != nil && (!attempt.waitsForCapacity || attempt.req != nil) {
				if attempt.req == nil {
					attempt.req = outbound
				}
				state.last = attempt
			}
		}
		if resp != nil && resp.Body != nil {
			// 管理检测在实际发送时才加超时，归因不能抹掉该上下文。
			actual := outbound
			if state.last != nil && state.last.req != nil {
				actual = state.last.req
			}
			if resp.Request != nil {
				actual = actual.WithContext(resp.Request.Context())
			}
			resp.Request = actual
			if state.last != nil {
				state.last.req = actual
			}
		}
		return resp, err
	}
	// 普通代理跨候选保留排队名额；独立调用把成功请求的名额交给 Body.Close。
	defer func() {
		if !logical.routing && (resultErr != nil || result == nil || result.Body == nil || isZAILocalPlanResponse(result) || !isZAITrialResponse(result)) {
			s.zaiStart.finish(logical)
		}
	}()
	sendPaid := func(paid *http.Request) (*http.Response, error) {
		paid, err := prepareZAIPackageRequest(cfg, paid, credential, "")
		if err != nil {
			return nil, err
		}
		// 已经离开 Trial，不能让付费长流继续占用 Trial 等待名额。
		s.zaiStart.finish(logical)
		logical.departures.Add(1)
		return send(paid)
	}
	body, err := zaiRequestBody(req)
	if err != nil {
		return nil, err
	}
	name := gjson.GetBytes(body, "model").String()
	renamed := zaiSearchNeedsAlias(body)
	if renamed {
		defer func() {
			if resultErr != nil || result == nil || result.Body == nil || result.StatusCode < 200 || result.StatusCode >= 300 {
				return
			}
			if err := decodeAnthropicResponse(result); err != nil {
				result, resultErr = nil, err
				return
			}
			var events io.Reader
			if responseIsSSE(result, gjson.GetBytes(body, "stream").Bool()) {
				events = result.Body
			}
			wrapJSONEventRewrite(result, events, restoreZAISearch)
		}()
	}
	forced := isZAITrialModel(name)
	if credential.JWTToken == "" && !forced {
		return sendPaid(req)
	}
	// 已选付费或体验模型已冷却时，保留的付费资格无需等待体验控制面恢复。
	if !forced && s.zaiCachedPaid(cfg, credential) {
		if credential.FreeFirst != nil && !*credential.FreeFirst {
			return sendPaid(req)
		}
		if cooldowns, err := s.getAllModelCooldowns(req.Context()); err == nil && cooldowns[cfg.ID][zaiTrialModelKey(name)].After(time.Now()) {
			return sendPaid(req)
		}
	}
	plans, err := s.zaiAccountPlans(req.Context(), cfg, credential, false)
	if err != nil {
		// 控制面超时不代表付费 Key 不可用；调用方取消和显式体验请求不重放。
		if req.Context().Err() == nil && !forced && credential.APIKey != "" {
			return sendPaid(req)
		}
		return nil, err
	}
	// 套餐选择只依据本渠道采样；跨渠道最新余额仅在 Trial 准入时否决请求。
	onlyGift := !plans.CanUsePaid(credential)
	raw, available := plans.GiftModel(name)
	useGift := onlyGift || forced || ((credential.FreeFirst == nil || *credential.FreeFirst) && available)
	if useGift {
		// 模型冷却只拒绝当前模型；账号仍可服务其他体验模型，不退役活动席位。
		if cooldowns, err := s.getAllModelCooldowns(req.Context()); err == nil && cooldowns[cfg.ID][zaiTrialModelKey(name)].After(time.Now()) {
			if onlyGift || forced {
				return zaiCoolingError(req, "zcode_trial_cooling", "ZCode 该模型体验入口暂时冷却，请稍后重试", time.Until(cooldowns[cfg.ID][zaiTrialModelKey(name)])), nil
			}
			useGift = false
		}
	}
	if !useGift {
		return sendPaid(req)
	}
	if credential.JWTToken == "" || plans.JWTRejected || zaiauth.JWTExpired(credential.JWTToken, time.Now()) {
		s.zaiStart.retire(zaiStartIdentity(credential))
		if !forced && !onlyGift {
			return sendPaid(req)
		}
		return zaiPlanError(req, http.StatusBadRequest, "zcode_trial_error", "ZCode 登录缺失或已过期，请重新登录"), nil
	}
	if !available {
		status, message := http.StatusBadRequest, "ZCode 没有该模型的有效体验套餐"
		if !plans.GiftKnown {
			status, message = http.StatusBadGateway, "ZCode 体验套餐查询失败，请稍后重试"
		} else if raw != "" {
			status, message = http.StatusTooManyRequests, "ZCode 该模型体验额度已耗尽"
		}
		return zaiPlanError(req, status, "zcode_trial_error", message), nil
	}
	trial, err := prepareZAIPackageRequest(cfg, req, credential, zaiauth.WireModel(raw))
	if err != nil {
		return nil, err
	}
	target := zaiauth.StartPlanBaseURL + "/v1/messages"
	if s.zaiService != nil {
		target = strings.TrimRight(s.zaiService.OAuthBaseURL, "/") + "/zcode-plan/anthropic/v1/messages"
	}
	trial.URL, err = url.Parse(target)
	if err != nil {
		return nil, err
	}
	trial.Host = ""
	// 每次 Trial 准入统一清理失效席位；付费请求无需操作 Trial 活动池。
	release, rejected, err := s.waitZAIStart(trial, zaiStartIdentity(credential), name)
	if err != nil {
		return nil, err
	}
	if rejected != nil {
		kind := zaiLocalPlanKind(rejected)
		// 备用和容量保护是选路决定，不能通过付费请求绕过；资格与风控暂停仍沿用原回退。
		canFallback := kind != "zcode_trial_reserve" && kind != "zcode_trial_wait" &&
			kind != "zcode_trial_capacity" && kind != "zcode_trial_queue_timeout"
		if canFallback && !forced && !onlyGift {
			_ = rejected.Body.Close()
			return sendPaid(req)
		}
		return rejected, nil
	}
	defer func() {
		if resultErr != nil || result == nil || result.Body == nil || !isZAITrialResponse(result) || isZAILocalPlanResponse(result) {
			release()
			return
		}
		// 流完整生命周期计入容量；取消与 Close 共用幂等释放，不能提前放入第四个请求。
		base := &onceCloseReadCloser{ReadCloser: result.Body}
		stop := context.AfterFunc(req.Context(), func() { _ = base.Close(); release() })
		result.Body = &releaseOnCloseReadCloser{ReadCloser: base, release: func() { stop(); release() }}
	}()
	resp, rawError, sendErr := s.zaiStart.send(trial, send, func() bool {
		// 已持有逻辑槽，复核保护窗口而不重新准入或排队。
		s.zaiStart.mu.Lock()
		now := s.zaiStart.clock()
		paused := s.zaiStart.until.After(now) || s.zaiStart.cooldowns[zaiStartIdentity(credential)].until.After(now)
		s.zaiStart.mu.Unlock()
		if paused {
			return false
		}
		windows, readErr := s.getAllModelCooldowns(trial.Context())
		return readErr == nil && !windows[cfg.ID][zaiTrialModelKey(name)].After(now)
	})
	// 容量拒绝尚未发送，不能冷却体验模型或触发付费重放。
	if errors.Is(sendErr, ErrChannelRPMExceeded) || errors.Is(sendErr, ErrChannelConcurrencyExceeded) {
		// 本地暂满不是账号失效，保留活动席位，不能借备用绕过容量约束。
		return nil, sendErr
	}
	if sendErr == nil && resp != nil {
		defer func() {
			// 选路与冷却仍使用原始错误；只有最终体验响应进入统一还原边界。
			if resultErr != nil || result != resp || result.Body == nil {
				return
			}
			var events io.Reader
			if responseIsSSE(result, gjson.GetBytes(body, "stream").Bool()) {
				events = result.Body
			} else if result.StatusCode < http.StatusBadRequest {
				return
			} else {
				// 读取整段错误仍受公共大小限制；超限报读取错误，不能用原索引修复历史。
				result.Body = http.MaxBytesReader(nil, result.Body, int64(config.DefaultMaxBodyBytes))
			}
			// 复用完整 JSON / SSE 事件读取，避免截断探测造成部分错误未映射。
			// 下游诊断捕获同样使用还原后的错误位置；实际请求仍保留注入的日期消息。
			wrapJSONEventRewrite(result, events, zaiRestoreErrorBody)
		}()
	}
	if sendErr != nil {
		// 截断响应属于读取故障，不能用残缺的 429 错误体判定额度耗尽。
		sendErr = &zaiTrialRequestError{error: sendErr, model: zaiauth.WireModel(raw)}
	}
	if canceled := req.Context().Err(); canceled != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, canceled
	}
	if errors.Is(sendErr, context.Canceled) || errors.Is(sendErr, context.DeadlineExceeded) || forced || onlyGift || state.capacityAbort || (sendErr == nil && resp != nil && resp.StatusCode < 400) {
		return resp, sendErr
	}
	// 自动体验即将回付费，外层只能看见付费结果，必须先且仅在此登记体验失败。
	status, _, retry := util.ClassifyError(sendErr)
	if sendErr != nil && !retry {
		return resp, sendErr
	}
	// 连接拒绝等 502 与真实超时使用各自策略，不能把所有传输失败统一记成 597。
	input := cooldownInputForModel(networkErrorInput(cfg.ID, cooldown.NoKeyIndex, status), zaiTrialModelKey(name))
	if sendErr == nil && resp != nil {
		if isZAILocalPlanResponse(resp) {
			return resp, nil
		}
		input = cooldownInputForModel(httpErrorInputFromParts(cfg.ID, cooldown.NoKeyIndex, resp.StatusCode, rawError, resp.Header), zaiTrialModelKey(name))
	}
	if action, _ := s.applyZAITrialCooldown(req.Context(), cfg, input); action == cooldown.ActionReturnClient {
		return resp, sendErr
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	// 付费发送不占用 Trial 容量，不能等外层返回后才释放。
	release()
	// 付费重放使用原始请求，绝不带体验 system 块；此时尚未向客户端提交响应。
	paid := cloneRequestWithBody(req.Clone(req.Context()), body)
	return sendPaid(paid)
}

/**
 * @brief 标记最终体验传输错误和实际出站模型，保留 errors.Is/As 和原始网络分类。
 */
type zaiTrialRequestError struct {
	error        ///< 原始传输错误。
	model string ///< 实际 Trial 出站模型；响应不存在时仍用于日志与计费归因。
}

/**
 * @brief 暴露原始错误供公共取消及网络分类器识别。
 * @return 底层传输错误。
 */
func (e *zaiTrialRequestError) Unwrap() error { return e.error }

/**
 * @brief 获取体验独立冷却身份，避免故障影响同账号付费模型。
 * @param name 路由后的模型名，可已带 Trial 后缀。
 * @return 规范后缀的体验模型名。
 */
func zaiTrialModelKey(name string) string {
	name = model.RoutingModelName(name)
	if isZAITrialModel(name) {
		name = name[:len(name)-6]
	}
	return name + "-Trial"
}

/**
 * @brief 标记尚未发送的本地体验拒绝，不伪造实际请求 URL。
 */
type zaiTrialRequestKey struct{}

/**
 * @brief 判断最终响应属于体验端点或本地体验资格拒绝。
 * @param resp 最终上游响应或本地拒绝。
 * @return 真实体验请求或本地体验拒绝返回 true。
 */
func isZAITrialResponse(resp *http.Response) bool {
	if resp == nil || resp.Request == nil {
		return false
	}
	return isZAILocalPlanResponse(resp) || strings.Contains(resp.Request.URL.Path, "/zcode-plan/anthropic/")
}

/**
 * @brief 识别本地生成的套餐拒绝，不信任上游错误体中的同名类型。
 * @param resp 待分类响应，可为 nil。
 * @return 仅本地拒绝返回 true；不表示推理上游发生故障。
 */
func isZAILocalPlanResponse(resp *http.Response) bool {
	return zaiLocalPlanKind(resp) != ""
}

/**
 * @brief 读取本地拒绝的可信类型，不能从上游 JSON 或响应头伪造等待状态。
 * @param resp 待检查响应，允许 nil。
 * @return 本地类型；真实上游响应返回空字符串。
 */
func zaiLocalPlanKind(resp *http.Response) string {
	if resp == nil || resp.Request == nil {
		return ""
	}
	local, _ := resp.Request.Context().Value(zaiTrialRequestKey{}).(string)
	return local
}

/**
 * @brief 为管理测试保留体验网络错误的分类，取消请求不触发冷却。
 * @param result 测试失败结果，原地补充状态和分类标志。
 * @param testReq 本次实际套餐身份。
 * @param err 原始传输或读取错误。
 */
func annotateZAITrialTestError(result map[string]any, testReq *testutil.TestChannelRequest, err error) {
	if testReq == nil || !testReq.ZAI.Trial || err == nil {
		return
	}
	status, _, retry := util.ClassifyError(err)
	if previous, exists := getResultInt(result["status_code"]); !exists || previous >= 200 && previous < 300 {
		result["status_code"] = status
	}
	result["network_error"] = true
	if !retry {
		result["cooldown_action"] = "client_error_no_cooldown"
	}
}

/**
 * @brief 只接受明确的消息字段路径，避免修改错误说明中的自然语言。
 */
var zaiMessageErrorPath = regexp.MustCompile(`^messages\.[0-9]+(?:\.[A-Za-z0-9_]+)*$`)

/**
 * @brief 在体验响应边界补充验证提示并还原消息错误位置，供客户端和内部修复共同使用。
 * @param raw 完整错误 JSON 或单个 SSE 事件的 JSON 数据。
 * @return 还原后的错误；非错误或无效 JSON 保持原值，注入日期消息使用非数字位置。
 */
func zaiRestoreErrorBody(raw []byte) []byte {
	if !json.Valid(raw) {
		return raw
	}
	if zaiErrorCode(raw) == "3007" {
		// 保留原始业务码与错误正文，明确需要完成验证，避免误导用户刷新凭证。
		const hint = "ZCode 体验请求需要人机验证，请完成验证后重试"
		path, message := "message", ""
		for _, key := range []string{"error.message", "error.msg", "message", "msg"} {
			if value := gjson.GetBytes(raw, key); value.Type == gjson.String {
				path, message = key, value.String()
				break
			}
		}
		if !strings.Contains(message, hint) {
			if next, err := sjson.SetBytes(raw, path, strings.TrimSpace(message+" "+hint)); err == nil {
				raw = next
			}
		}
	}
	if !gjson.GetBytes(raw, "error").IsObject() {
		return raw
	}
	shift := func(path string) string {
		normalized := anthropicErrorPathIndex.ReplaceAllString(strings.TrimSpace(path), ".$1")
		if !zaiMessageErrorPath.MatchString(normalized) {
			return path
		}
		parts := strings.Split(normalized, ".")
		index, err := strconv.Atoi(parts[1])
		if err != nil || index < 0 {
			return path
		}
		// 日期消息不属于调用方；非数字位置既说明来源，也阻止误修首条消息。
		if index == 0 {
			parts[1] = "injected_date"
			return strings.Join(parts, ".")
		}
		parts[1] = strconv.Itoa(index - 1)
		return strings.Join(parts, ".")
	}
	result := bytes.Clone(raw)
	for _, key := range []string{"error.param", "error.message"} {
		value := gjson.GetBytes(raw, key)
		if value.Type != gjson.String {
			continue
		}
		prefix, rest, found := strings.Cut(value.String(), ":")
		adjusted := shift(prefix)
		if adjusted == strings.TrimSpace(prefix) {
			continue
		}
		if found {
			adjusted += ":" + rest
		}
		if next, err := sjson.SetBytes(result, key, adjusted); err == nil {
			result = next
		}
	}
	return result
}

/**
 * @brief 读取可重放的请求体，不消费实际发送体。
 * @param req 已构造的请求。
 * @return 请求 JSON。
 * @return 请求不可重放或读取失败时返回错误。
 */
func zaiRequestBody(req *http.Request) ([]byte, error) {
	if req.GetBody == nil {
		return nil, errors.New("ZCode request is not replayable")
	}
	reader, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	return io.ReadAll(reader)
}

/**
 * @brief 将本地套餐拒绝表示为标准 Anthropic 错误响应。
 * @param req 原始请求。
 * @param status HTTP 状态。
 * @param kind 本地拒绝类型，已有冷却单独标记以避免重复延长。
 * @param message 不含凭证的说明。
 * @return 可由现有代理错误流程消费的响应。
 */
func zaiPlanError(req *http.Request, status int, kind, message string) *http.Response {
	req = req.WithContext(context.WithValue(req.Context(), zaiTrialRequestKey{}, kind))
	raw, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": message}})
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw)), Request: req}
}

/**
 * @brief 检查 HTTP 200 错误信封或首个 SSE 事件，正常响应原样回放。
 * @param resp 体验端点响应。
 * @param streaming 是否预期流式响应，允许探测未声明正确 Content-Type 的 SSE。
 * @return 探测的真实读取错误；正常响应或完整错误信封返回 nil。
 * @note 探测限制为 64 KiB；首个正常业务事件后不再探测或重放。
 */
func probeZAIError(resp *http.Response, streaming bool) error {
	if resp.StatusCode != http.StatusOK || resp.Body == nil {
		return nil
	}
	if responseIsSSE(resp, streaming) {
		return probeZAIStreamError(resp)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		return nil
	}
	buf, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body = replayResponseProbe(resp.Body, buf, readErr)
	if readErr != nil {
		return readErr
	}
	if !json.Valid(buf) {
		return nil
	}
	code := zaiErrorCode(buf)
	if code == "3012" {
		// 成功 HTTP 状态不能掩盖风控拒绝，也不能将其归类为额度耗尽。
		resp.StatusCode = http.StatusForbidden
	} else if code == "3007" {
		resp.StatusCode = http.StatusBadRequest
	} else if zaiGiftQuotaError(resp.StatusCode, buf) || zaiConcurrencyError(buf) {
		resp.StatusCode = http.StatusTooManyRequests
	} else if gjson.GetBytes(buf, "type").String() == "error" {
		resp.StatusCode = http.StatusBadRequest
	}
	return nil
}

/**
 * @brief 只探测首个业务 SSE 事件，跳过心跳，正常事件按原字节回放。
 * @param resp 尚未向客户端提交的响应。
 * @return 首个业务事件前的真实读取错误或 io.ErrUnexpectedEOF；首个完整事件返回 nil。
 * @note 使用请求原有取消和首字节截止；最多缓冲 64 KiB。
 */
func probeZAIStreamError(resp *http.Response) error {
	original := resp.Body
	var prefix []byte
	var readErr error
	chunk := make([]byte, 1024)
	offset := 0
	for len(prefix) < 64*1024 {
		n, err := original.Read(chunk)
		prefix = append(prefix, chunk[:n]...)
		readErr = err
		for offset < len(prefix) {
			rest := prefix[offset:]
			end := firstSSEEventEnd(rest)
			if end < 0 {
				break
			}
			event, data := parseSSEEventChunk(rest[:end])
			offset += end
			if len(data) == 0 || event == "ping" || gjson.GetBytes(data, "type").String() == "ping" {
				continue
			}
			if event == "error" || gjson.GetBytes(data, "type").String() == "error" {
				resp.StatusCode = classifySSEErrorStatus(data)
				// ZCode 的数字业务码不是 HTTP 状态，不能交给公共 SSE 分类器猜测。
				switch zaiErrorCode(data) {
				case "3012":
					resp.StatusCode = http.StatusForbidden
				case "3007":
					resp.StatusCode = http.StatusBadRequest
				default:
					if zaiConcurrencyError(data) {
						resp.StatusCode = http.StatusTooManyRequests
					}
				}
				resp.Header.Set("Content-Type", "application/json")
				resp.Header.Del("Content-Length")
				resp.ContentLength = -1
				resp.Body = readerWithCloser{Reader: bytes.NewReader(data), Closer: original}
				return nil
			}
			resp.Body = replayResponseProbe(original, prefix, readErr)
			return nil
		}
		if err != nil || n == 0 {
			break
		}
	}
	if readErr != nil {
		// 尚无业务事件的断流允许回退；正常首事件已在循环内原样返回。
		resp.Body = replayResponseProbe(original, prefix, readErr)
		if errors.Is(readErr, io.EOF) {
			return io.ErrUnexpectedEOF
		}
		return readErr
	}
	resp.Body = replayResponseProbe(original, prefix, readErr)
	return nil
}

/**
 * @brief 识别不能通过切套餐解决的坏请求；模型不存在仍属于模型故障。
 * @param status 上游状态码。
 * @param headers 上游响应头。
 * @param body 上游错误体。
 * @return 普通客户端错误返回 true，模型范围错误返回 false。
 */
func isZAIClientRequestError(status int, headers http.Header, body []byte) bool {
	classification := util.ClassifyHTTPResponseWithMeta(status, headers, body)
	return classification.Level == util.ErrorLevelClient && !classification.ModelScoped
}

/**
 * @brief 本地资格检查通过后才为每次实际发送预留 RPM 和并发容量。
 * @param cfg 本次请求的渠道配置及凭证快照。
 * @param req 已构造且带上下文的出站请求。
 * @param send 发送回调，负责每次实际发送的容量预留和响应体释放。
 * @return 上游响应或本地套餐拒绝响应；成功响应须关闭 Body。
 * @return 本地准备、取消、容量预留或传输失败时返回错误。
 * @note 套餐控制面查询和本地拒绝不占用推理容量；套餐回退每次重新预留。
 */
func (s *Server) doUpstreamRequestWithSender(cfg *model.Config, req *http.Request, send func(*http.Request) (*http.Response, error)) (*http.Response, error) {
	// 业务身份由 Messages 构造入口标记，完整 URL 不能绕过本地套餐校验。
	if cfg != nil && cfg.UsesZAIOAuth() && req.Method == http.MethodPost && req.Context().Value(zaiPlanRequestKey{}) == true {
		return s.sendZAIPlanRequest(cfg, req, send)
	}
	return send(req)
}

/**
 * @brief 本地ZAI拒绝不记录推理失败，保留必要的公共选路动作。
 */
func zaiLocalPlanFailure(cfg *model.Config, res *fwResult, forceReturnClient bool) *proxyResult {
	if cfg.UsesZAIOAuth() && res.zaiLocalPlanError {
		// 本地准入没有推理调用，不写渠道失败日志、令牌失败统计或健康冷却。
		// 容量只约束 Trial，不能提前结束整个请求并截断后续付费或其他渠道。
		action := cooldown.ActionRetryModel
		if forceReturnClient {
			action = cooldown.ActionReturnClient
		}
		return &proxyResult{status: res.Status, header: res.Header, body: res.Body,
			zaiTrial: res.zaiTrial, zaiLocalPlanKind: res.zaiLocalPlanKind, nextAction: action}
	}
	return nil
}
