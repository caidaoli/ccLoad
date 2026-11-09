package app

import (
	"context"
	"crypto/sha256"
	"io"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ccLoad/internal/zaiauth"

	"github.com/tidwall/gjson"
)

/**
 * @brief Trial 独占的进程内保护；付费与控制面不读取此状态。
 */
type zaiStartGuard struct {
	mu       sync.Mutex                                 ///< 保护活动账号、容量和暂停窗口。
	accounts map[[32]byte]*zaiStartAccount              ///< 活动池；空闲成功账号仍保留席位。
	requests []*zaiStartRequest                         ///< 运行及等待的逻辑请求，按准入顺序排列，最多三个。
	changed  chan struct{}                              ///< 容量、资格或暂停变化时关闭并重新建立。
	busy     []time.Time                                ///< 最近一分钟内不同逻辑请求的忙拒绝。
	until    time.Time                                  ///< 所有 Trial 暂停截止时间。
	now      func() time.Time                           ///< 可替换时钟；nil 使用真实时间。
	wait     func(context.Context, time.Duration) error ///< 可替换等待；nil 使用可取消定时器。
}

/**
 * @brief 一个真实账号的活动席位，退役后须等所有流结束才能让出。
 */
type zaiStartAccount struct {
	inflight int  ///< 此账号的在途逻辑请求数。
	retired  bool ///< 失效、额度耗尽或冷却，停止接收新请求。
}

/**
 * @brief 跨渠道和内部重试共享的一次逻辑请求身份。
 */
type zaiStartRequest struct {
	counted    atomic.Bool                     ///< 同一个下游请求最多登记一次忙拒绝。
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
 * @brief 归还逻辑名额并取消其排队，不清除跨渠道忙拒绝去重身份。
 * @param logical 本次逻辑请求；nil 或已归还时无操作。
 * @note 调用方必须持有 mu；不释放仍由响应体持有的账号槽位。
 */
func (g *zaiStartGuard) finishLocked(logical *zaiStartRequest) {
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
 * @param plans 套餐快照；nil 或查询失败不作为失效依据。
 * @return 已确认全部体验不可用时为 true，否则为 false。
 */
func zaiStartPlansUnavailable(plans *zaiauth.Plans) bool {
	if plans == nil || !plans.GiftKnown {
		return false
	}
	for _, gift := range plans.Gifts {
		for _, name := range gift.Models {
			if _, available := plans.GiftModel(name, time.Unix(plans.ObservedAt, 0)); available {
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
 * @return 资格失效、暂停或 reserve 的本地响应，否则 nil。
 * @return 请求取消时返回上下文错误。
 * @note 读取失败或套餐资格未知时保留活动池；不新增上游查询，不修改公共选路状态。
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
	for _, cfg := range configs {
		if !cfg.UsesZAIOAuth() || !cfg.Enabled || channelCooldowns[cfg.ID].After(now) || !cfg.IsAvailableAt(now) || cooldowns[cfg.ID][zaiTrialModelKey(name)].After(now) {
			continue
		}
		credential, parseErr := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr == nil && credential.JWTToken != "" && !zaiauth.JWTExpired(credential.JWTToken, now) {
			key := zaiStartIdentity(credential)
			usable[key] = true
		}
	}
	for key, entry := range latestPlans {
		if zaiStartPlansUnavailable(entry.plans) {
			delete(usable, key)
		}
	}
	// 排队期间余额也可能更新；只拒绝当前模型，不退役仍有其他模型额度的账号。
	if entry, ok := latestPlans[key]; ok {
		if _, available := entry.plans.GiftModel(name, time.Unix(entry.plans.ObservedAt, 0)); !available && !zaiStartPlansUnavailable(entry.plans) {
			return nil, zaiPlanError(req, http.StatusTooManyRequests, "zcode_trial_error", "ZCode 该模型体验额度已耗尽"), nil
		}
	}
	return s.zaiStart.acquire(req, key, usable)
}

/**
 * @brief 原子预留三个逻辑名额之一和单账号串行槽；繁忙只排队，不替换活动账号。
 * @param req 本次 Trial 请求，用于构造本地拒绝。
 * @param key 实际账号摘要。
 * @param usable 本次模型的最新账号资格；nil 表示读取失败，保留活动池。
 * @return 幂等释放函数；拒绝时为 nil。
 * @return 等待、容量满、暂停或资格拒绝的本地响应，否则 nil。
 * @return 请求取消时返回上下文错误。
 * @note req 必须携带 zaiStartRequest；按账号排队，互不相交的账号不互相阻塞。
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
				if !account.retired {
					account.retired = true
					g.notifyLocked()
				}
				if account.inflight == 0 {
					delete(g.accounts, activeKey)
				}
			}
		}
	}
	if resp := g.heldLocked(req); resp != nil {
		return nil, resp, nil
	}
	// 清理活动池不等于禁止再次入池；重复渠道的旧可用缓存也必须受最新资格约束。
	if usable != nil && !usable[key] {
		return nil, zaiPlanError(req, 429, "zcode_trial_unavailable", "ZCode 账号体验资格已失效，请刷新套餐或稍后重试"), nil
	}
	account := g.accounts[key]
	if (account == nil && len(g.accounts) >= 2) || (account != nil && account.retired) {
		return nil, zaiPlanError(req, 429, "zcode_trial_reserve", "ZCode 体验已有两个活动账号，此账号暂作备用"), nil
	}
	logical, _ := req.Context().Value(zaiStartRequestKey{}).(*zaiStartRequest)
	if !slices.Contains(g.requests, logical) {
		if len(g.requests) >= 3 {
			resp := zaiPlanError(req, 429, "zcode_trial_capacity", "ZCode 体验已有三个运行或等待的请求，请稍后重试")
			resp.Header.Set("Retry-After", "1")
			return nil, resp, nil
		}
		g.requests = append(g.requests, logical)
	}
	blocked := account != nil && account.inflight != 0
	for _, earlier := range g.requests {
		if earlier == logical {
			break
		}
		blocked = blocked || earlier.waiting[key]
	}
	if blocked {
		if logical.waiting == nil {
			logical.waiting = make(map[[32]byte]bool)
		}
		logical.waiting[key] = true
		return nil, zaiPlanError(req, 429, "zcode_trial_wait", "ZCode 活动账号繁忙，等待空闲账号"), nil
	}
	if account == nil {
		if g.accounts == nil {
			g.accounts = make(map[[32]byte]*zaiStartAccount)
		}
		account = &zaiStartAccount{}
		g.accounts[key] = account
	}
	logical.waiting = nil
	account.inflight++
	return sync.OnceFunc(func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		account.inflight--
		if account.retired && account.inflight == 0 {
			delete(g.accounts, key)
		}
		if !logical.routing {
			g.finishLocked(logical)
		}
		g.notifyLocked()
	}), nil, nil
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
	resp := zaiPlanError(req, 429, "zcode_trial_paused", "ZCode 体验因并发拒绝或 IP 风控暂时暂停，请稍后重试")
	resp.Header.Set("Retry-After", strconv.Itoa(int((remaining+time.Second-1)/time.Second)))
	return resp
}

/**
 * @brief 读取暂停，或登记一次逻辑忙拒绝及明确的 IP 封禁。
 * @param req 当前请求。
 * @param busy 本逻辑请求首次出现并发忙。
 * @param edge 已确认 405 Aliyun/HTML 封禁页。
 * @return 当前全局暂停的本地响应；未暂停返回 nil。
 * @note 五个请求在六十秒内触发三分钟暂停；暂停期间不延长窗口。
 */
func (g *zaiStartGuard) observe(req *http.Request, busy, edge bool) *http.Response {
	g.mu.Lock()
	defer g.mu.Unlock()
	if resp := g.heldLocked(req); resp != nil {
		return resp
	}
	now := g.clock()
	if busy {
		kept := g.busy[:0]
		for _, hit := range g.busy {
			if hit.After(now.Add(-time.Minute)) {
				kept = append(kept, hit)
			}
		}
		g.busy = append(kept, now)
	}
	if edge || len(g.busy) >= 5 {
		g.until = now.Add(3 * time.Minute)
		g.busy = nil
		g.notifyLocked()
	}
	return g.heldLocked(req)
}

/**
 * @brief 等待带 ±25% 抖动的退避，调用取消立即终止。
 * @param ctx 请求上下文。
 * @param delay 基准等待时长。
 * @return 取消时返回 context 错误，否则 nil。
 */
func (g *zaiStartGuard) pause(ctx context.Context, delay time.Duration) error {
	delay = time.Duration(float64(delay) * (0.75 + rand.Float64()*0.5))
	if g.wait != nil {
		return g.wait(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

/**
 * @brief 在 Trial 发送边界执行退避，正常流开始后绝不重放。
 * @param req 已冻结请求体和归因头的 Trial 请求。
 * @param send 公共发送器，每次实际发送独立占用渠道 RPM/并发。
 * @return 最终响应；调用方须关闭 Body。
 * @return 已归一的上游错误体，正常响应或本地暂停为空。
 * @return 读取、发送或取消错误，保留原始错误身份。
 */
func (g *zaiStartGuard) send(req *http.Request, send func(*http.Request) (*http.Response, error)) (*http.Response, []byte, error) {
	body, err := zaiRequestBody(req)
	if err != nil {
		return nil, nil, err
	}
	logical, _ := req.Context().Value(zaiStartRequestKey{}).(*zaiStartRequest)
	if logical == nil {
		logical = &zaiStartRequest{}
	}
	waits := [...]time.Duration{250, 500, 900, 1600, 2800, 4000}
	for attempt := 0; ; attempt++ {
		if err := req.Context().Err(); err != nil {
			return nil, nil, err
		}
		if held := g.observe(req, false, false); held != nil {
			return held, nil, nil
		}
		resp, sendErr := send(cloneRequestWithBody(req.Clone(req.Context()), body))
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
		busy := resp.StatusCode == http.StatusTooManyRequests && zaiConcurrencyError(raw)
		lower := strings.ToLower(string(raw))
		edge := resp.StatusCode == 405 && (strings.Contains(lower, "errors.aliyun.com") || strings.Contains(lower, "<html"))
		if busy || edge {
			if held := g.observe(req, busy && logical.counted.CompareAndSwap(false, true), edge); held != nil {
				_ = resp.Body.Close()
				return held, nil, nil
			}
		}
		if !busy || attempt >= len(waits) {
			return resp, raw, nil
		}
		// 失败响应先关闭，释放公共容量；逻辑请求仍占 Trial 槽位，防止重试堆积。
		_ = resp.Body.Close()
		if err := g.pause(req.Context(), waits[attempt]*time.Millisecond); err != nil {
			return nil, nil, err
		}
	}
}
