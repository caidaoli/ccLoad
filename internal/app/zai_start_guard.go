package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ccLoad/internal/util"
	"ccLoad/internal/zaiauth"

	"github.com/tidwall/gjson"
)

/**
 * @brief Trial 独占的进程内保护；付费与控制面不读取此状态。
 */
type zaiStartGuard struct {
	mu         sync.Mutex                    ///< 保护活动账号、容量和暂停窗口。
	accounts   map[[32]byte]*zaiStartAccount ///< 活动池；空闲成功账号仍保留席位。
	requests   []*zaiStartRequest            ///< 运行及等待的逻辑请求，按准入顺序排列，最多三个等待。
	changed    chan struct{}                 ///< 容量、资格或暂停变化时关闭并重新建立。
	switching  *zaiStartRequest              ///< 发起排空的等待请求；取消后撤销切换。
	target     [32]byte                      ///< 本轮排空后接替的真实账号。
	cooldowns  map[[32]byte]zaiTrialPause    ///< 仅体验账号的暂停窗口。
	authorized map[[32]byte][32]byte         ///< 各账号最新落库 JWT 摘要，阻止旧请求迟到的认证错误。
	policy     *util.CooldownPolicy          ///< 与存储同一启动配置构建的通用时间策略。
	duration   time.Duration                 ///< 出口组上一次冷却持续时间。
	until      time.Time                     ///< 所有 Trial 暂停截止时间。
	now        func() time.Time              ///< 可替换时钟；nil 使用真实时间。
}

/**
 * @brief 一个真实账号的活动席位，退役后须等所有流结束才能让出。
 */
type zaiStartAccount struct {
	inflight int  ///< 此账号的在途逻辑请求数。
	retired  bool ///< 已确认失效，停止接收新请求。
	draining bool ///< 正在为合法备用排空，停止新准入。
}

/**
 * @brief 跨渠道和内部重试共享的一次逻辑请求身份。
 */
type zaiStartRequest struct {
	sends      atomic.Uint64                   ///< 发送边界调用次数，阻止已尝试候选重新排队。
	departures atomic.Uint64                   ///< 转入付费次数，路由协程据此丢弃此前的排队候选。
	routing    bool                            ///< 普通代理由外层选路管理等待与清理；创建后不可变。
	waiting    map[[32]byte]bool               ///< 等待的真实账号集合；仅由 guard.mu 保护。
	pending    []zaiStartPending               ///< 本轮尚未发送的渠道；仅路由协程访问。
	models     map[int64]modelRoutingSelection ///< 等待重试固定原模型行；仅路由协程访问。
}

/**
 * @brief Trial 排队的最长等待时间；不限制已开始的正常生成流。
 */
const zaiStartQueueWait = time.Minute

/**
 * @brief 获取当前容量变化通知，必须先订阅再检查准入以免丢失唤醒。
 * @return 下次容量、资格或暂停变化时关闭的通道。
 * @note 并发安全；不持锁等待。
 */
func (g *zaiStartGuard) notification() <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.changed == nil {
		g.changed = make(chan struct{})
	}
	return g.changed
}

/**
 * @brief 唤醒等待请求重新核对资格与容量。
 * @note 调用方必须持有 mu。
 */
func (g *zaiStartGuard) notifyLocked() {
	if g.changed != nil {
		close(g.changed)
		g.changed = nil
	}
}

/**
 * @brief 归还等待名额并撤销该请求发起的排空。
 * @param logical 本次逻辑请求；nil 或已归还时无操作。
 * @note 调用方必须持有 mu；不释放仍由响应体持有的账号槽位。
 */
func (g *zaiStartGuard) finishLocked(logical *zaiStartRequest) {
	if g.switching == logical {
		g.switching = nil
		for _, account := range g.accounts {
			account.draining = false
		}
		g.notifyLocked()
	}
	if i := slices.Index(g.requests, logical); i >= 0 {
		g.requests = slices.Delete(g.requests, i, i+1)
		logical.waiting = nil
		g.notifyLocked()
	}
}

/**
 * @brief 请求结束或离开 Trial 时立即归还逻辑容量。
 * @param logical 逻辑请求；nil 或重复调用无副作用。
 * @note 并发安全，账号槽仍由实际响应的 Close 释放。
 */
func (g *zaiStartGuard) finish(logical *zaiStartRequest) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.finishLocked(logical)
}

/**
 * @brief 只在 ZAI 尝试上下文中传递逻辑请求身份。
 */
type zaiStartRequestKey struct{}

/**
 * @brief 实际体验发送时间，管理成功处理用它保护在途期间的新冷却。
 */
type zaiTrialStartedKey struct{}

/**
 * @brief 获取保护状态使用的时钟。
 * @return 当前时间。
 */
func (g *zaiStartGuard) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

/**
 * @brief 合并重复渠道中的同一真实账号，不把代理出口或令牌刷新当成新账号。
 * @param c 已归一的 ZAI 凭证。
 * @return 不含明文凭证的站点和账号摘要；缺身份时退回凭证摘要。
 */
func zaiStartIdentity(c *zaiauth.Credential) [32]byte {
	identity := "user:" + c.UserID
	if c.UserID == "" {
		identity = "email:" + strings.ToLower(strings.TrimSpace(c.Email))
		if c.Email == "" {
			identity = "key:" + c.APIKey
			if c.APIKey == "" {
				identity = "jwt:" + c.JWTToken
			}
		}
	}
	return sha256.Sum256([]byte(string(c.Site) + "\x00" + identity))
}

/**
 * @brief 退役失效账号，保留仍在途的席位直到流关闭。
 * @param key 实际账号摘要。
 * @note 并发安全，不修改渠道或付费冷却。
 */
func (g *zaiStartGuard) retire(key [32]byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if account := g.accounts[key]; account != nil {
		account.retired = true
		if account.inflight == 0 {
			delete(g.accounts, key)
		}
		g.notifyLocked()
	}
}

/**
 * @brief 判断账号是否已确认没有任何可用体验模型，单个付费模型无体验不代表账号失效。
 * @param plans 套餐快照；nil 或查询失败不作为失效依据，登录被拒绝视为失效。
 * @return 已确认全部体验不可用或登录被拒绝时为 true，否则为 false。
 */
func zaiStartPlansUnavailable(plans *zaiauth.Plans) bool {
	if plans == nil {
		return false
	}
	if plans.JWTRejected {
		return true
	}
	if !plans.GiftKnown {
		return false
	}
	for _, gift := range plans.Gifts {
		for _, name := range gift.Models {
			if _, available := plans.GiftModel(name); available {
				return false
			}
		}
	}
	return true
}

/**
 * @brief 根据最新账号资格清理活动池并预留 Trial 容量，防止旧渠道缓存重新激活失效账号。
 * @param req 本次 Trial 请求。
 * @param key 实际账号摘要。
 * @param name 当前 Trial 模型名。
 * @return 幂等释放函数；拒绝时为 nil。
 * @return 资格失效、模型冷却、模型额度耗尽、暂停或 reserve 的本地响应，否则 nil。
 * @return 请求取消时返回上下文错误。
 * @note 读取失败或套餐资格未知时保留活动池；模型冷却只拒绝当前模型，不退役账号；
 *       管理检测与独立发送不受渠道停用、冷却或时段约束，只按余额资格准入。
 */
func (s *Server) acquireZAIStart(req *http.Request, key [32]byte, name string) (func(), *http.Response, error) {
	if s.channelCache == nil {
		return s.zaiStart.acquire(req, key, nil)
	}
	configs, err := s.channelCache.GetEnabledChannelsSnapshotByModel(req.Context(), "*")
	if err != nil {
		return s.zaiStart.acquire(req, key, nil)
	}
	cooldowns, err := s.getAllModelCooldowns(req.Context())
	if err != nil {
		return s.zaiStart.acquire(req, key, nil)
	}
	channelCooldowns, err := s.getAllChannelCooldowns(req.Context())
	if err != nil {
		return s.zaiStart.acquire(req, key, nil)
	}
	now := s.zaiStart.clock()
	// 固定 plans → guard 锁序；采样读取到准入之间不解锁，旧快照不能延后覆盖新资格。
	s.zaiPlans.mu.Lock()
	defer s.zaiPlans.mu.Unlock()
	usable := make(map[[32]byte]bool)
	latestPlans := make(map[[32]byte]zaiPlanEntry)
	// 余额是账号事实，不依赖采样渠道当前能否选路；否则停用/冷却会让重复渠道的旧余额复活。
	for _, entry := range s.zaiPlans.entries {
		if entry.giftStale || !now.Before(entry.expires) || !entry.plans.GiftKnown {
			continue
		}
		previous, exists := latestPlans[entry.account]
		if !exists || entry.plans.GiftObservation > previous.plans.GiftObservation {
			latestPlans[entry.account] = entry
		}
	}
	var coolingUntil time.Time
	for _, cfg := range configs {
		if !cfg.UsesZAIOAuth() || !cfg.Enabled || channelCooldowns[cfg.ID].After(now) || !cfg.IsAvailableAt(now) {
			continue
		}
		credential, parseErr := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr == nil && credential.JWTToken != "" && !zaiauth.JWTExpired(credential.JWTToken, now) {
			identity := zaiStartIdentity(credential)
			usable[identity] = true
			// 模型冷却已传播到同账号全部重复渠道，任一命中即可拒绝本模型。
			if until := cooldowns[cfg.ID][zaiTrialModelKey(name)]; identity == key && until.After(now) && until.After(coolingUntil) {
				coolingUntil = until
			}
		}
	}
	// 管理检测可指定停用、冷却或时段外的渠道；其资格只由余额决定，不受选路状态约束。
	if logical, _ := req.Context().Value(zaiStartRequestKey{}).(*zaiStartRequest); !logical.routing {
		usable[key] = true
	}
	for identity, entry := range latestPlans {
		if zaiStartPlansUnavailable(entry.plans) {
			delete(usable, identity)
		}
	}
	// 模型冷却与单模型余额耗尽都只拒绝当前模型，不退役仍可服务其他模型的账号。
	if !coolingUntil.IsZero() {
		return nil, zaiCoolingError(req, "zcode_trial_cooling", "ZCode 该模型体验入口暂时冷却，请稍后重试", coolingUntil.Sub(now)), nil
	}
	if entry, ok := latestPlans[key]; ok {
		if _, available := entry.plans.GiftModel(name); !available && !zaiStartPlansUnavailable(entry.plans) {
			return nil, zaiPlanError(req, http.StatusTooManyRequests, "zcode_trial_error", "ZCode 该模型体验额度已耗尽"), nil
		}
	}
	return s.zaiStart.acquire(req, key, usable)
}

/**
 * @brief 单个默认出口组只准入一个账号的三个在途请求，另保留三个等待名额。
 * @param req 带逻辑身份的体验请求。
 * @param key 实际账号摘要。
 * @param usable 最新账号资格；nil 表示未知，不能据此退役。
 * @return 幂等释放函数，拒绝时为 nil。
 * @return 带本地来源标记的等待、暂停或容量响应；成功为 nil。
 * @return 请求取消时返回上下文错误。
 * @note 不持锁等待；响应体关闭前持续占用在途槽位。
 */
func (g *zaiStartGuard) acquire(req *http.Request, key [32]byte, usable map[[32]byte]bool) (func(), *http.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := req.Context().Err(); err != nil {
		return nil, nil, err
	}
	if usable != nil {
		for activeKey, account := range g.accounts {
			if !usable[activeKey] {
				changed := !account.retired || account.inflight == 0
				account.retired = true
				if account.inflight == 0 {
					delete(g.accounts, activeKey)
				}
				if changed {
					g.notifyLocked()
				}
			}
		}
	}
	if resp := g.heldLocked(req); resp != nil {
		return nil, resp, nil
	}
	if pause := g.cooldowns[key]; pause.until.After(g.clock()) {
		return nil, zaiCoolingError(req, "zcode_trial_account_cooling", "ZCode 体验账号暂时冷却，请稍后重试", pause.until.Sub(g.clock())), nil
	}
	if usable != nil && !usable[key] {
		return nil, zaiPlanError(req, 429, "zcode_trial_unavailable", "ZCode 账号体验资格已失效，请刷新套餐或稍后重试"), nil
	}
	logical, _ := req.Context().Value(zaiStartRequestKey{}).(*zaiStartRequest)
	account := g.accounts[key]
	blocked := account == nil && len(g.accounts) != 0 || account != nil && (account.retired || account.draining || account.inflight >= 3)
	blocked = blocked || g.switching != nil && (g.switching != logical || g.target != key)
	for _, earlier := range g.requests {
		if earlier == logical {
			break
		}
		blocked = blocked || earlier.waiting[key]
	}
	if blocked {
		if !slices.Contains(g.requests, logical) {
			if len(g.requests) >= 3 {
				resp := zaiPlanError(req, 429, "zcode_trial_capacity", "ZCode 体验等待队列已满，请稍后重试")
				resp.Header.Set("Retry-After", "1")
				return nil, resp, nil
			}
			g.requests = append(g.requests, logical)
		}
		if logical.waiting == nil {
			logical.waiting = make(map[[32]byte]bool)
		}
		logical.waiting[key] = true
		return nil, zaiPlanError(req, 429, "zcode_trial_wait", "ZCode 体验正在等待容量或旧账号排空"), nil
	}
	g.finishLocked(logical)
	logical.waiting = nil
	if account == nil {
		if g.accounts == nil {
			g.accounts = make(map[[32]byte]*zaiStartAccount)
		}
		account = &zaiStartAccount{}
		g.accounts[key] = account
	}
	account.inflight++
	return sync.OnceFunc(func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		account.inflight--
		if (account.retired || account.draining) && account.inflight == 0 {
			delete(g.accounts, key)
		}
		g.notifyLocked()
	}), nil, nil
}

/**
 * @brief 完整遍历合法候选仍不能发送后，才为指定备用账号排空。
 * @param logical 持有等待名额的逻辑请求。
 * @param key 原请求合法候选中的备用账号。
 * @note 并发安全；不抢占既有切换，取消由 finishLocked 撤销。
 */
func (g *zaiStartGuard) switchAccount(logical *zaiStartRequest, key [32]byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.switching != nil || !slices.Contains(g.requests, logical) {
		return
	}
	// 切换所有权遵循同账号 FIFO，否则后排所有者会和前排等待互相阻塞。
	for _, earlier := range g.requests {
		if earlier == logical {
			break
		}
		if earlier.waiting[key] {
			return
		}
	}
	if _, active := g.accounts[key]; active {
		return
	}
	// 活动账号仍能服务当前请求时等待其空槽，避免满载即切换健康账号。
	for activeKey, account := range g.accounts {
		if !account.retired && logical.waiting[activeKey] {
			return
		}
	}
	if g.cooldowns[key].until.After(g.clock()) || g.until.After(g.clock()) {
		return
	}
	g.switching, g.target = logical, key
	for activeKey, account := range g.accounts {
		account.draining = true
		if account.inflight == 0 {
			delete(g.accounts, activeKey)
		}
	}
	g.notifyLocked()
}

/**
 * @brief 管理测试和独立发送也共用 Trial 名额；普通代理先返回等待状态以尝试其他合法账号。
 * @param req 已确定为 Trial 的请求，必须携带逻辑身份。
 * @param key 真实账号摘要。
 * @param name 实际请求模型。
 * @return 准入成功的幂等释放函数，未准入为 nil。
 * @return 本地拒绝或排队超时响应，无拒绝为 nil。
 * @return 调用取消时返回上下文错误。
 * @note 等待不持有套餐锁或公共 RPM/发送槽。
 */
func (s *Server) waitZAIStart(req *http.Request, key [32]byte, name string) (func(), *http.Response, error) {
	logical, _ := req.Context().Value(zaiStartRequestKey{}).(*zaiStartRequest)
	deadline := time.NewTimer(zaiStartQueueWait)
	defer deadline.Stop()
	for {
		changed := s.zaiStart.notification()
		release, rejected, err := s.acquireZAIStart(req, key, name)
		if err != nil || rejected == nil || logical.routing || zaiLocalPlanKind(rejected) != "zcode_trial_wait" {
			return release, rejected, err
		}
		_ = rejected.Body.Close()
		s.zaiStart.switchAccount(logical, key)
		select {
		case <-req.Context().Done():
			return nil, nil, req.Context().Err()
		case <-deadline.C:
			if err := req.Context().Err(); err != nil {
				return nil, nil, err
			}
			resp := zaiPlanError(req, 429, "zcode_trial_queue_timeout", "ZCode 体验等待空闲账号超时，请稍后重试")
			resp.Header.Set("Retry-After", "1")
			return nil, resp, nil
		case <-changed:
		}
	}
}

/**
 * @brief 生成暂停响应，绝不把本地 429 当成上游额度耗尽。
 * @param req 当前请求。
 * @return 带 Retry-After 的本地 429；未暂停返回 nil。
 * @note 调用方必须持有 mu。
 */
func (g *zaiStartGuard) heldLocked(req *http.Request) *http.Response {
	remaining := g.until.Sub(g.clock())
	if remaining <= 0 {
		return nil
	}
	return zaiCoolingError(req, "zcode_trial_paused", "ZCode 体验出口因明确风控暂时暂停，请稍后重试", remaining)
}

/**
 * @brief 返回真实剩余冷却时间，向上取整为秒，不修改窗口。
 * @param req 当前请求。
 * @param kind 本地错误类型。
 * @param message 客户端错误说明。
 * @param remaining 剩余冷却时间；已过期时返回零秒。
 * @return 带 Retry-After 的本地 429，调用方负责关闭响应体。
 */
func zaiCoolingError(req *http.Request, kind, message string, remaining time.Duration) *http.Response {
	resp := zaiPlanError(req, http.StatusTooManyRequests, kind, message)
	seconds := max(remaining/time.Second, 0)
	if remaining > 0 && remaining%time.Second != 0 {
		seconds++
	}
	resp.Header.Set("Retry-After", strconv.FormatInt(int64(seconds), 10))
	return resp
}

/**
 * @brief ZAI 体验账号的暂停历史，不被公共渠道读取。
 */
type zaiTrialPause struct {
	until       time.Time     ///< 暂停截止时间。
	duration    time.Duration ///< 上次策略持续时间。
	authFailure bool          ///< 仅认证失败的窗口可被重新授权清除。
}

/**
 * @brief 新 JWT 授权成功后解除同账号认证暂停，保留模型及风控窗口。
 * @param credential 已合并落库的凭证；必须包含本次授权的 JWT。
 * @note 并发安全；保留在途计数及排空状态，唤醒等待请求重新检查资格。
 */
func (g *zaiStartGuard) reauthorize(credential *zaiauth.Credential) {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := zaiStartIdentity(credential)
	if g.authorized == nil {
		g.authorized = make(map[[32]byte][32]byte)
	}
	// 即使当前没有冷却，也要隔离已经发出但尚未返回的旧 JWT。
	g.authorized[key] = sha256.Sum256([]byte(credential.JWTToken))
	if !g.cooldowns[key].authFailure {
		return
	}
	delete(g.cooldowns, key)
	if account := g.accounts[key]; account != nil {
		account.retired = false
	}
	g.notifyLocked()
}

/**
 * @brief 以共享时间算法登记账号或默认出口暂停，同一有效窗口不重复递增。
 * @param credential 实际发送时的凭证快照，供账号归因与认证代次核对。
 * @param decision 已分类的账号或出口组失败；无明确截止时间时使用当前策略。
 * @return 接受失败时为 true；旧 JWT 的迟到认证错误返回 false，不修改任何窗口。
 * @note 并发安全；暂停只约束 Trial，既有正常流继续运行。
 */
func (g *zaiStartGuard) cool(credential *zaiauth.Credential, decision zaiTrialDecision) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := zaiStartIdentity(credential)
	if latest, exists := g.authorized[key]; decision.authFailure && exists && latest != sha256.Sum256([]byte(credential.JWTToken)) {
		return false
	}
	now := g.clock()
	group, until, status := decision.scope == "group", decision.Until, decision.status
	previous := g.cooldowns[key]
	if group {
		previous = zaiTrialPause{until: g.until, duration: g.duration}
	}
	if previous.until.After(now) {
		// 在途请求又确认了账号风控时，不能让重登清除仍有效的风控窗口。
		if !group && !decision.authFailure && previous.authFailure {
			previous.authFailure = false
			g.cooldowns[key] = previous
		}
		return true
	}
	if until.IsZero() {
		policy := g.policy
		if policy == nil {
			policy = util.NewCooldownPolicy(util.CooldownSettings{})
		}
		until = now.Add(policy.CalculateBackoffDuration(previous.duration.Milliseconds(), previous.until, now, &status))
	}
	if group {
		g.until, g.duration = until, until.Sub(now)
	} else {
		if g.cooldowns == nil {
			g.cooldowns = make(map[[32]byte]zaiTrialPause)
		}
		g.cooldowns[key] = zaiTrialPause{until: until, duration: until.Sub(now), authFailure: decision.authFailure}
		if account := g.accounts[key]; account != nil {
			account.retired = true
			if account.inflight == 0 {
				delete(g.accounts, key)
			}
		}
	}
	g.notifyLocked()
	return true
}

/**
 * @brief 取现有请求预算的最早截止时间，不创建或延长任何计时器。
 * @param ctx 发送上下文，包含调用方及非流式截止时间。
 * @param started 原计时器开始时间。
 * @param durations 已启用的超时时长；非正值忽略。
 * @return 最早截止时间；完全未限时返回零。
 */
func zaiRetryDeadline(ctx context.Context, started time.Time, durations ...time.Duration) time.Time {
	deadline, _ := ctx.Deadline()
	for _, duration := range durations {
		if until := started.Add(duration); duration > 0 && (deadline.IsZero() || until.Before(deadline)) {
			deadline = until
		}
	}
	return deadline
}

/**
 * @brief 持原 Trial 槽执行最多三次明确 busy 重试，正常流开始后绝不重放。
 * @param req 已冻结请求体和归因头的 Trial 请求。
 * @param send 公共发送器，每次实际发送独立占用渠道 RPM/并发。
 * @param retryAllowed 复核账号、出口及模型保护窗口，不重新准入。
 * @return 最终响应；调用方须关闭 Body。
 * @return 已归一的上游错误体，正常响应或本地暂停为空。
 * @return 读取、发送或取消错误，保留原始错误身份。
 * @note 优先等待 Retry-After，否则使用 1/2 秒加不足一秒的抖动；超出既有预算时返回最后一次真实错误。
 */
func (g *zaiStartGuard) send(req *http.Request, send func(*http.Request) (*http.Response, error), retryAllowed func() bool) (*http.Response, []byte, error) {
	req = req.WithContext(context.WithValue(req.Context(), zaiTrialStartedKey{}, time.Now()))
	var last *http.Response
	var raw []byte
	var retryAt time.Time
	for attempt := range 3 {
		if attempt > 0 {
			if err := waitForChannelURLRetry(req.Context(), time.Until(retryAt)); err != nil {
				return nil, nil, err
			}
			allowed := retryAllowed()
			if err := req.Context().Err(); err != nil {
				return nil, nil, err
			}
			if !allowed {
				return last, raw, nil
			}
		}
		resp, body, err := g.sendOnce(req, send)
		// 验证与明确风控优先于消息中的 busy 字样，不能进入并发退避循环。
		if code := zaiErrorCode(body); code == "3007" || code == "3012" {
			return resp, body, err
		}
		if err != nil && last != nil && (errors.Is(err, ErrChannelRPMExceeded) || errors.Is(err, ErrChannelConcurrencyExceeded)) {
			zaiState(req).capacityAbort = true
			return last, raw, nil
		}
		if err != nil || resp == nil || resp.StatusCode != http.StatusTooManyRequests || !zaiConcurrencyError(body) {
			return resp, body, err
		}
		now := time.Now()
		if until, ok := util.ParseRetryAfter(resp.Header, now); ok {
			// 锚定绝对时间，已等待的时间不能在最终冷却时再计算一遍。
			if until.Nanosecond() != 0 {
				until = until.Truncate(time.Second).Add(time.Second)
			}
			retryAt = until
			resp.Header.Set("Retry-After", until.UTC().Format(http.TimeFormat))
		} else {
			retryAt = now.Add(time.Duration(attempt+1)*time.Second + time.Duration(rand.Int64N(int64(time.Second))))
		}
		if attempt == 2 {
			return resp, body, nil
		}
		deadline, _ := req.Context().Deadline()
		if state := zaiState(req); state != nil && !state.retryDeadline.IsZero() && (deadline.IsZero() || state.retryDeadline.Before(deadline)) {
			deadline = state.retryDeadline
		}
		if !deadline.IsZero() && !retryAt.Before(deadline) {
			return resp, body, nil
		}
		// 释放本次公共容量；最终回放与已关闭网络流完全独立。
		_ = resp.Body.Close()
		if sent, _ := resp.Request.Context().Value(zaiSendAttemptKey{}).(*zaiSendAttempt); sent != nil && sent.timeout != nil {
			sent.timeout.cancelAll()
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		last, raw = resp, body
	}
	return last, raw, nil
}

/**
 * @brief 发送一次冻结 wire 并有界探测首个业务错误，不停止共享首字节预算。
 * @param req 带首次 Trial 时间的请求。
 * @param send 负责公共容量的发送器。
 * @return 响应，调用方负责 Close。
 * @return 可用于分类的完整错误片段。
 * @return 原始读取、传输或取消错误。
 */
func (g *zaiStartGuard) sendOnce(req *http.Request, send func(*http.Request) (*http.Response, error)) (*http.Response, []byte, error) {
	body, err := zaiRequestBody(req)
	if err != nil {
		return nil, nil, err
	}
	if err := req.Context().Err(); err != nil {
		return nil, nil, err
	}
	outbound := cloneRequestWithBody(req.Clone(req.Context()), body)
	// 各次尝试拥有自己的请求与 timer 记录，修复基准仍只读共享。
	if previous, _ := req.Context().Value(zaiSendAttemptKey{}).(*zaiSendAttempt); previous != nil {
		attempt := &zaiSendAttempt{repairBody: previous.repairBody}
		outbound = outbound.WithContext(context.WithValue(outbound.Context(), zaiSendAttemptKey{}, attempt))
	}
	resp, sendErr := send(outbound)
	if sendErr != nil || resp == nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil, nil, sendErr
	}
	// 在探测期间也响应取消；只关闭稳定的原始 Body，避免与解压/回放包装竞争。
	base := &onceCloseReadCloser{ReadCloser: resp.Body}
	stop := context.AfterFunc(req.Context(), func() { _ = base.Close() })
	resp.Body = &releaseOnCloseReadCloser{ReadCloser: base, release: func() { stop() }}
	sendErr = decodeAnthropicResponse(resp)
	if sendErr == nil {
		sendErr = probeZAIError(resp, gjson.GetBytes(body, "stream").Bool())
	}
	var raw []byte
	if sendErr == nil && resp.StatusCode >= 400 && resp.Body != nil {
		raw, sendErr = io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body = replayResponseProbe(resp.Body, raw, sendErr)
	}
	if req.Context().Err() != nil {
		sendErr = req.Context().Err()
	} else if resp.Request != nil && resp.Request.Context().Err() != nil {
		sendErr = resp.Request.Context().Err()
	}
	if sendErr != nil {
		_ = resp.Body.Close()
		return nil, nil, sendErr
	}
	return resp, raw, nil
}
