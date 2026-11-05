package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"ccLoad/internal/loomyauth"
	"ccLoad/internal/model"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

/**
 * @brief 保存管理员与手机号、代理绑定的一次性短信挑战。
 * @note 仅驻留服务端内存，验证前消费，不把会话或账号身份交给客户端声明。
 */
type loomyLoginChallenge struct {
	owner, phone, msgID, proxyURL string    ///< owner：拥有挑战的管理员会话哈希；phone：用于身份绑定和限流的归一大陆手机号；msgID：上游短信挑战标识，仅服务端保存；proxyURL：发送短信和验证时必须一致的渠道代理。
	expires                       time.Time ///< 挑战失效的绝对时刻。
}

/**
 * @brief 隔离 Loomy 进程内登录挑战、限流与账号串行状态。
 * @details 挑战不持久化；账号签到与任务进度存入私有凭证，内存重试时间仅补充写库失败保护。
 */
type loomyRuntimeState struct {
	loginMu    sync.Mutex                     ///< 保护挑战表与短信限流器初始化。
	logins     map[string]loomyLoginChallenge ///< 按不透明登录 ID 保存的一次性挑战。
	smsLimiter *rpmLimiter                    ///< 按手机号统计滚动一分钟发送预算。
	gates      sync.Map                       ///< 按已验证用户 ID 串行化签到与重新登录。
	retryAfter sync.Map                       ///< 按渠道记录进程内下次允许自动尝试时间，补充写库失败保护。
}

/**
 * @brief 为渠道取得遵循其代理策略的 Loomy 协议客户端副本。
 * @param cfg 本次操作使用的渠道配置及私有凭证快照。
 * @return 遵循渠道代理策略的客户端副本；服务未初始化时为 nil。
 * @return 服务未初始化时返回错误；成功为 nil。
 * @note 只替换副本的 HTTP 客户端，不修改共享协议服务。
 */
func (s *Server) loomyClient(cfg *model.Config) (*loomyauth.Service, error) {
	if s.loomyService == nil {
		return nil, errors.New("loomy service unavailable")
	}
	service := *s.loomyService
	service.Client = s.getClientForChannel(cfg)
	return &service, nil
}

/**
 * @brief 串行化同一 Loomy 账号的签到与重新授权操作。
 * @param ctx 本次操作的取消与截止时间上下文。
 * @param userID 由上游登录接口确认的账号标识。
 * @return 成功时返回必须调用一次的释放函数；等待取消时为 nil。
 * @return 等待取消时返回 context.Canceled 或 context.DeadlineExceeded；成功为 nil。
 * @note 获取后应立即 defer 释放；独立额度更新仍由条件写入保护。
 */
func (s *Server) acquireLoomyAccount(ctx context.Context, userID string) (func(), error) {
	return acquireKeyedGate(ctx, &s.loomyState.gates, userID)
}

/**
 * @brief 创建绑定管理员、归一手机号与代理的短信登录挑战。
 * @param c 携带路由参数、管理员身份和响应写入器的 HTTP 上下文。
 * @note 请求最多 30 秒；挑战有效五分钟且最多保存 128 个，手机号共用滚动一分钟限流，失败不退还限流预算；超限响应 429 与 Retry-After 秒数。
 */
func (s *Server) HandleLoomySendCode(c *gin.Context) {
	owner, ok := xaiAdminSessionHash(c)
	if !ok {
		RespondErrorMsg(c, http.StatusUnauthorized, "administrator session required")
		return
	}
	var input struct {
		Phone    string `json:"phone"`     ///< 待归一并验证的大陆手机号。
		ProxyURL string `json:"proxy_url"` ///< 登录流程绑定的渠道代理地址，可为空。
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if c.ShouldBindJSON(&input) != nil {
		RespondErrorMsg(c, 400, "invalid Loomy login request")
		return
	}
	phone, err := loomyauth.NormalizePhone(input.Phone)
	if err != nil {
		RespondError(c, 400, err)
		return
	}
	phoneID, err := strconv.ParseInt(phone, 10, 64)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	proxy, err := normalizeChannelProxyURL(input.ProxyURL)
	if err != nil {
		RespondError(c, 400, err)
		return
	}
	service, err := s.loomyClient(&model.Config{ProxyURL: proxy})
	if err != nil {
		RespondError(c, 503, err)
		return
	}
	// Reserve before sending so concurrent clicks cannot send unlimited SMS.
	now := time.Now()
	s.loomyState.loginMu.Lock()
	if s.loomyState.logins == nil {
		s.loomyState.logins = make(map[string]loomyLoginChallenge)
	}
	for id, login := range s.loomyState.logins {
		if !login.expires.After(now) {
			delete(s.loomyState.logins, id)
		}
	}
	if len(s.loomyState.logins) >= 128 {
		s.loomyState.loginMu.Unlock()
		RespondErrorMsg(c, 429, "too many Loomy login challenges")
		return
	}
	// 发码按手机号共用滚动一分钟限流；消费挑战或上游失败不能释放发送预算。
	if s.loomyState.smsLimiter == nil {
		s.loomyState.smsLimiter = newRPMLimiter(time.Now)
	}
	s.loomyState.smsLimiter.CleanupExpired()
	reservation := s.loomyState.smsLimiter.reserve(phoneID, 1)
	if !reservation.allowed {
		s.loomyState.loginMu.Unlock()
		c.Header("Retry-After", strconv.Itoa(max(1, int((reservation.retryAfter+time.Second-1)/time.Second))))
		RespondErrorMsg(c, http.StatusTooManyRequests, "请稍后再发送验证码")
		return
	}
	id := uuid.NewString()
	challenge := loomyLoginChallenge{owner: owner, phone: phone, proxyURL: proxy, expires: now.Add(5 * time.Minute)}
	s.loomyState.logins[id] = challenge
	s.loomyState.loginMu.Unlock()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	msgID, err := service.SendCode(ctx, phone)
	s.loomyState.loginMu.Lock()
	if err != nil {
		delete(s.loomyState.logins, id)
	} else {
		challenge.msgID = msgID
		s.loomyState.logins[id] = challenge
	}
	s.loomyState.loginMu.Unlock()
	if err != nil {
		RespondError(c, 502, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"login_id": id})
}

/**
 * @brief 消费当前管理员的一次性挑战并通过上游短信校验建立渠道。
 * @param c 携带路由参数、管理员身份和响应写入器的 HTTP 上下文。
 * @note 最多执行 60 秒；有效挑战在校验前消费，失败也不可复用。仅接受 login_id 和验证码，账号与 session 必须来自上游。
 */
func (s *Server) HandleLoomyLogin(c *gin.Context) {
	owner, ok := xaiAdminSessionHash(c)
	if !ok {
		RespondErrorMsg(c, 401, "administrator session required")
		return
	}
	var input struct {
		LoginID string `json:"login_id"` ///< 服务端一次性短信挑战 ID，不包含上游会话。
		Code    string `json:"code"`     ///< 用户输入的六位短信验证码。
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if c.ShouldBindJSON(&input) != nil {
		RespondErrorMsg(c, 400, "invalid Loomy verification request")
		return
	}
	s.loomyState.loginMu.Lock()
	login, found := s.loomyState.logins[input.LoginID]
	if found && login.owner == owner && login.msgID != "" {
		delete(s.loomyState.logins, input.LoginID)
	} else {
		found = false
	}
	s.loomyState.loginMu.Unlock()
	if !found || !login.expires.After(time.Now()) {
		RespondErrorMsg(c, 400, "验证码会话已失效，请重新发送")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 60*time.Second)
	defer cancel()
	service, err := s.loomyClient(&model.Config{ProxyURL: login.proxyURL})
	if err != nil {
		RespondError(c, 503, err)
		return
	}
	credential, err := service.Login(ctx, login.phone, input.Code, login.msgID)
	if err != nil {
		RespondError(c, 502, err)
		return
	}
	cfg, created, err := s.commitLoomyCredential(ctx, credential, login.proxyURL)
	if err != nil {
		RespondError(c, 502, err)
		return
	}
	RespondJSON(c, http.StatusOK, gin.H{"channel_id": cfg.ID, "channel_name": cfg.Name, "created": created})
}

/**
 * @brief 验证登录凭证可访问模型后创建渠道或更新同账号的登录会话。
 * @param ctx 本次操作的取消与截止时间上下文。
 * @param credential 短信登录接口确认的账号身份与会话。
 * @param proxy 已校验归一的渠道代理地址，空值使用默认传输策略。
 * @return 创建或更新后的渠道；失败时可能为 nil。
 * @return 进入新建渠道分支时为 true，包含新建失败；更新已有渠道或此前失败时为 false。
 * @return 凭证校验、账号锁、读写存储或重新启用失败时返回错误；会话失效可用 errors.Is 检查 loomyauth.ErrSessionExpired，成功为 nil。
 * @note 按账号加锁去重；重新登录保留签到、任务与积分状态，替换会话、更新代理、清理下次尝试时间并重新启用。
 */
func (s *Server) commitLoomyCredential(ctx context.Context, credential *loomyauth.Credential, proxy string) (*model.Config, bool, error) {
	service, err := s.loomyClient(&model.Config{ProxyURL: proxy})
	if err != nil {
		return nil, false, err
	}
	names, err := service.FetchModels(ctx, credential.Session)
	if err != nil {
		return nil, false, err
	}
	release, err := s.acquireLoomyAccount(ctx, credential.UserID)
	if err != nil {
		return nil, false, err
	}
	defer release()
	configs, err := s.store.ListConfigs(ctx)
	if err != nil {
		return nil, false, err
	}
	for _, cfg := range configs {
		if !cfg.UsesLoomySession() {
			continue
		}
		current, parseErr := loomyauth.ParseCredential([]byte(cfg.OAuthCredential))
		if parseErr != nil || current.UserID != credential.UserID {
			continue
		}
		_, err = s.mergeLoomyCredential(ctx, cfg, func(next *loomyauth.Credential) {
			next.Session = credential.Session
			next.NextCheckinAttemptAt = time.Time{}
		})
		if err != nil {
			return nil, false, err
		}
		s.loomyState.retryAfter.Delete(cfg.ID)
		updated, err := reactivateReauthorizedChannel(ctx, s.store, cfg.ID, proxy)
		s.invalidateChannelRelatedCache(cfg.ID)
		s.InvalidateChannelListCache()
		return updated, false, err
	}
	payload, err := credential.JSON()
	if err != nil {
		return nil, false, err
	}
	digest := sha256.Sum256([]byte(credential.UserID))
	cfg := &model.Config{Name: fmt.Sprintf("Loomy-%x", digest[:4]), AuthType: model.AuthTypeLoomySession, OAuthCredential: payload,
		URLs:                  model.ChannelURLs{{URL: loomyauth.CompletionsURL, Exact: true, Protocols: []string{util.ProtocolOpenAI}}},
		ProtocolTransformMode: model.ProtocolTransformModeLocal, Enabled: true, CostMultiplier: 1, ProxyURL: proxy, ModelEntries: oauthModelEntries(names)}
	cfg.Name = uniqueZedChannelName(configs, cfg.Name)
	cfg, err = s.store.CreateConfig(ctx, cfg)
	if err == nil {
		s.InvalidateChannelListCache()
	}
	return cfg, true, err
}

/**
 * @brief 将账号状态变更合并到最新凭证，避免覆盖并发额度或登录更新。
 * @param ctx 本次操作的取消与截止时间上下文。
 * @param source 确定渠道和账号身份的原始配置快照。
 * @param merge 应用到每次新读取凭证上的本地合并回调。
 * @return 保存后的渠道配置；失败时为 nil。
 * @return 身份变化、读取编码失败或上下文结束时返回错误；持续写入冲突返回 errOAuthCASContention，成功为 nil。
 * @note merge 可能执行多次，仅允许修改本地凭证，不得发送远程请求或产生其他外部副作用。
 * @details 写入冲突沿用公共 OAuth 有界退避策略，每次重读后重新校验账号并合并，保留并发更新。
 */
func (s *Server) mergeLoomyCredential(ctx context.Context, source *model.Config, merge func(*loomyauth.Credential)) (*model.Config, error) {
	original, err := loomyauth.ParseCredential([]byte(source.OAuthCredential))
	if err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		current, err := s.store.GetConfig(ctx, source.ID)
		if err != nil {
			return nil, err
		}
		if !current.UsesLoomySession() {
			return nil, errors.New("loomy channel identity changed")
		}
		credential, err := loomyauth.ParseCredential([]byte(current.OAuthCredential))
		if err != nil {
			return nil, err
		}
		if credential.UserID != original.UserID {
			return nil, errors.New("loomy account changed")
		}
		merge(credential)
		payload, err := credential.JSON()
		if err != nil {
			return nil, err
		}
		updated, err := s.store.CompareAndSwapOAuthCredential(ctx, current.ID, model.AuthTypeLoomySession, current.OAuthCredential, payload)
		if err != nil {
			return nil, err
		}
		if updated {
			current.OAuthCredential = payload
			s.invalidateChannelRelatedCache(current.ID)
			s.InvalidateChannelListCache()
			return current, nil
		}
		if err := waitOAuthCASRetry(ctx, attempt); err != nil {
			return nil, err
		}
	}
}

/**
 * @brief 通过 Loomy 会话发现聊天模型并归一到公共模型响应。
 * @param ctx 本次操作的取消与截止时间上下文。
 * @param cfg 本次操作使用的渠道配置及私有凭证快照。
 * @param override 显式协议覆盖，仅接受空值或 OpenAI。
 * @return 归一后的聊天模型目录；失败时为 nil。
 * @return 协议不支持、凭证无效或上游失败时返回错误；会话失效可用 errors.Is 检查 loomyauth.ErrSessionExpired，成功为 nil。
 * @note 明确会话失效时按请求快照处理条件禁用。
 */
func (s *Server) fetchLoomyModels(ctx context.Context, cfg *model.Config, override string) (*FetchModelsResponse, error) {
	if override != "" && util.NormalizeProtocol(override) != util.ProtocolOpenAI {
		return nil, errors.New("loomy model discovery requires openai protocol")
	}
	credential, err := loomyauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		return nil, err
	}
	service, err := s.loomyClient(cfg)
	if err != nil {
		return nil, err
	}
	names, err := service.FetchModels(ctx, credential.Session)
	if err != nil {
		s.handleLoomyRejection(ctx, cfg, err)
		return nil, err
	}
	return &FetchModelsResponse{Models: oauthModelEntries(names), Protocol: util.ProtocolOpenAI, Source: "api"}, nil
}
