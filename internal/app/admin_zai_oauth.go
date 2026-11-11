package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/zaiauth"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	zaiOAuthSessionTTL      = 15 * time.Minute
	zaiOAuthTerminalTTL     = 2 * time.Minute
	zaiOAuthJanitorInterval = 30 * time.Second
)

/**
 * @brief ZCode 授权入口的站点和代理选择。
 */
type zaiOAuthStartRequest struct {
	oauthLoginStartRequest              ///< 通用会话代理。
	Site                   zaiauth.Site `json:"site"` ///< 国际版 zai 或国内版 bigmodel。
}

/**
 * @brief 在任何上游请求前验证站点及代理。
 * @return 站点或代理非法时返回错误。
 */
func (r *zaiOAuthStartRequest) Validate() error {
	site, err := zaiauth.NormalizeSite(r.Site)
	if err != nil {
		return err
	}
	r.Site = site
	return r.oauthLoginStartRequest.Validate()
}

/**
 * @brief 授权会话固定的站点及出口，贯穿轮询、Key 派生及持久化。
 */
type zaiOAuthOptions struct {
	oauthLoginOptions              ///< 固定代理配置。
	site              zaiauth.Site ///< 开始授权时确定的账号站点。
	deviceID          string       ///< 本次登录设备，贯穿初始化、轮询及保存。
}

type zaiOAuthStartResponse struct {
	URL    string `json:"url"`
	State  string `json:"state"`
	Status string `json:"status"`
}

type zaiOAuthStatusResponse struct {
	State       string `json:"state"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	ChannelID   int64  `json:"channel_id,omitempty"`
	ChannelName string `json:"channel_name,omitempty"`
}

type zaiOAuthCancelRequest struct {
	State string `json:"state"`
}

func (r *zaiOAuthCancelRequest) Validate() error {
	if strings.TrimSpace(r.State) == "" {
		return errors.New("state is required")
	}
	return nil
}

// zaiOAuthSession 只放需要跨 goroutine 共享、且受 m.mu 保护的会话状态。
// 轮询凭证刻意不在此列：它在 start 里生成后就不再变化，只有轮询 goroutine
// 消费，作为 run 的参数传递即可——它的生命周期就是那个 goroutine 的栈，会话
// 落终态后 map 里本就不再持有它，比"存进来再清空"既少一个数据竞争也更短命。
type zaiOAuthSession struct {
	state            string
	adminSessionHash string
	ctx              context.Context
	cancel           context.CancelFunc
	status           string
	errorMsg         string
	channelID        int64
	channelName      string
	createdAt        time.Time
	finishedAt       time.Time
}

/**
 * @brief 管理两站授权会话；站点和出口在启动时固定，后台轮询不受请求结束影响。
 */
type zaiOAuthManager struct {
	mu       sync.Mutex                                                                         ///< 共享状态锁。
	service  *zaiauth.Service                                                                   ///< 公共控制面服务。
	resolve  func(context.Context, zaiOAuthOptions, string) (*zaiauth.Credential, error)        ///< 付费 Key 派生。
	commit   func(context.Context, zaiOAuthOptions, *zaiauth.Credential) (int64, string, error) ///< 渠道持久化。
	baseCtx  context.Context                                                                    ///< 服务生命周期。
	now      func() time.Time                                                                   ///< 会话时钟。
	sleep    func(context.Context, time.Duration)                                               ///< 可取消轮询间隔。
	sessions map[string]*zaiOAuthSession                                                        ///< 按状态 ID 保存会话。
	// activeByID keeps one live login per administrator.
	activeByID  map[string]string ///< 管理员当前会话。
	stopJanitor chan struct{}     ///< 通知清理协程退出。
	janitorDone chan struct{}     ///< 清理协程完成通知。
	closeOnce   sync.Once         ///< 关闭操作幂等。
	closed      bool              ///< 禁止新建会话。
	pollers     sync.WaitGroup    ///< 等待后台轮询结束。
}

/**
 * @brief 创建 ZCode 授权会话管理器。
 * @param baseCtx 服务生命周期上下文；nil 使用后台上下文。
 * @param service 公共控制面客户端。
 * @param resolve 按所选站点派生付费 Key。
 * @param commit 保存账号的回调；无套餐也可提交。
 * @return 启动了清理协程的管理器，必须 close。
 * @note 启动后台清理协程。
 */
func newZAIOAuthManager(
	baseCtx context.Context,
	service *zaiauth.Service,
	resolve func(context.Context, zaiOAuthOptions, string) (*zaiauth.Credential, error),
	commit func(context.Context, zaiOAuthOptions, *zaiauth.Credential) (int64, string, error),
) *zaiOAuthManager {
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	manager := &zaiOAuthManager{
		service: service, resolve: resolve, commit: commit, baseCtx: baseCtx,
		now: time.Now, sleep: sleepWithContext,
		sessions: make(map[string]*zaiOAuthSession), activeByID: make(map[string]string),
		stopJanitor: make(chan struct{}), janitorDone: make(chan struct{}),
	}
	go manager.runJanitor()
	return manager
}

func sleepWithContext(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
	}
}

/**
 * @brief 为管理员启动固定站点的授权，同一管理员旧会话会被取消。
 * @param ctx 初始化请求上下文。
 * @param adminSessionHash 管理员会话标识。
 * @param options 固定站点及代理出口。
 * @return 授权链接和轮询状态。
 * @return 缺少管理员、服务不可用或上游初始化失败时返回错误。
 * @note 初始化成功后启动后台轮询协程。
 */
func (m *zaiOAuthManager) start(ctx context.Context, adminSessionHash string, options zaiOAuthOptions) (zaiOAuthStartResponse, error) {
	if m == nil || m.service == nil || m.resolve == nil || m.commit == nil {
		return zaiOAuthStartResponse{}, errors.New("z.ai OAuth is unavailable")
	}
	adminSessionHash = strings.TrimSpace(adminSessionHash)
	if adminSessionHash == "" {
		return zaiOAuthStartResponse{}, errors.New("z.ai OAuth requires an administrator session")
	}
	pollToken, err := zaiauth.GeneratePollToken()
	if err != nil {
		return zaiOAuthStartResponse{}, err
	}
	options.deviceID = uuid.NewString()
	service := m.service.ForSite(options.site, options.client)
	flow, err := service.InitFlow(ctx, pollToken, options.deviceID)
	if err != nil {
		return zaiOAuthStartResponse{}, err
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return zaiOAuthStartResponse{}, errors.New("z.ai OAuth is unavailable")
	}
	m.pruneExpiredSessionsLocked()
	if old := m.sessions[m.activeByID[adminSessionHash]]; old != nil && zaiOAuthSessionCancelable(old.status) {
		m.finishSessionLocked(old, "cancelled", "", 0, "")
	}
	sessionCtx, cancel := context.WithCancel(m.baseCtx)
	session := &zaiOAuthSession{
		state: flow.FlowID, adminSessionHash: adminSessionHash,
		ctx: sessionCtx, cancel: cancel, status: "pending", createdAt: m.now(),
	}
	m.sessions[session.state] = session
	m.activeByID[adminSessionHash] = session.state
	m.pollers.Add(1)
	m.mu.Unlock()

	go m.run(session, service, flow, pollToken, options)
	return zaiOAuthStartResponse{URL: flow.AuthorizeURL, State: session.state, Status: "pending"}, nil
}

/**
 * @brief 使用初始化时的站点和出口轮询至授权提交或终止。
 * @param session 受管理器保护的会话。
 * @param service 固定站点的服务副本。
 * @param flow 上游登录信息和期限。
 * @param pollToken 仅用于该登录的轮询令牌。
 * @param options 固定的登录选项。
 * @note 由 start 启动；终止时释放 pollers 计数。
 */
func (m *zaiOAuthManager) run(
	session *zaiOAuthSession,
	service *zaiauth.Service,
	flow *zaiauth.Flow,
	pollToken string,
	options zaiOAuthOptions,
) {
	defer m.pollers.Done()
	interval := zaiauth.PollInterval
	if flow.PollIntervalSec > 0 {
		interval = max(interval, time.Duration(flow.PollIntervalSec)*time.Second)
	}
	deadline := m.now().Add(zaiauth.PollTimeout)
	if flow.ExpiresAt > 0 {
		if expiry := time.Unix(flow.ExpiresAt, 0); expiry.Before(deadline) {
			deadline = expiry
		}
	}

	for m.now().Before(deadline) {
		// 首轮和暂态恢复均先等待；期限只阻止下一轮，不取消已经在途的 poll。
		m.sleep(session.ctx, interval)
		if session.ctx.Err() != nil {
			return
		}
		if !m.now().Before(deadline) {
			break
		}
		result, err := service.Poll(session.ctx, flow.FlowID, pollToken, options.deviceID)
		if err != nil {
			if session.ctx.Err() != nil {
				return
			}
			var pollErr *zaiauth.OAuthError
			if errors.As(err, &pollErr) && pollErr.Retryable() {
				continue
			}
			m.finish(session, "error", err.Error(), 0, "")
			return
		}
		switch result.Status {
		case zaiauth.PollReady:
			m.completeAuthorization(session, result, options)
			return
		case zaiauth.PollFailed:
			m.finish(session, "error", "z.ai rejected the authorization", 0, "")
			return
		}
	}
	if session.ctx.Err() == nil {
		m.finish(session, "error", "z.ai authorization timed out", 0, "")
	}
}

/**
 * @brief 完成登录并提交账号；付费 Key 派生失败时仍允许有效体验 JWT。
 * @param session 待提交的授权会话。
 * @param result 上游确认的登录身份及令牌。
 * @param options 本次登录的代理配置。
 * @note 会话先转为 committing，再于持久化完成后结束，避免重复提交。
 */
func (m *zaiOAuthManager) completeAuthorization(session *zaiOAuthSession, result *zaiauth.PollResult, options zaiOAuthOptions) {
	m.mu.Lock()
	if session.status != "pending" {
		m.mu.Unlock()
		return
	}
	session.status = "committing"
	if m.activeByID[session.adminSessionHash] == session.state {
		delete(m.activeByID, session.adminSessionHash)
	}
	m.mu.Unlock()

	credential := &zaiauth.Credential{}
	var err error
	if result.AccessToken != "" {
		credential, err = m.resolve(session.ctx, options, result.AccessToken)
	}
	if err != nil {
		if result.JWTToken == "" {
			m.finish(session, "error", err.Error(), 0, "")
			return
		}
		// 体验 JWT 仍可完成登录，但付费额度不可用，须留下管理员可排查的线索。
		log.Printf("[WARN] ZCode 付费 Key 派生失败，账号仅保存体验登录: %v", err)
	}
	if credential == nil {
		credential = &zaiauth.Credential{}
	}
	credential.AccessToken = result.AccessToken
	credential.Site = options.site
	credential.DeviceID = options.deviceID
	credential.JWTToken = result.JWTToken
	if credential.UserID == "" {
		credential.UserID = result.Identity.UserID
	}
	if credential.Email == "" {
		credential.Email = result.Identity.Email
	}
	if credential.Name == "" {
		credential.Name = result.Name
	}
	if err := credential.Normalize(); err != nil {
		m.finish(session, "error", err.Error(), 0, "")
		return
	}
	channelID, channelName, err := m.commit(session.ctx, options, credential)
	if err != nil {
		m.finish(session, "error", err.Error(), 0, "")
		return
	}
	log.Printf("[INFO] z.ai Coding Plan 渠道已就绪: channel_id=%d", channelID)
	m.finish(session, "complete", "", channelID, channelName)
}

func (m *zaiOAuthManager) finish(session *zaiOAuthSession, status, errorMsg string, channelID int64, channelName string) {
	m.mu.Lock()
	if session.status != "cancelled" {
		m.finishSessionLocked(session, status, errorMsg, channelID, channelName)
	}
	m.mu.Unlock()
}

func (m *zaiOAuthManager) status(adminSessionHash, state string) (zaiOAuthStatusResponse, bool) {
	if m == nil {
		return zaiOAuthStatusResponse{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneExpiredSessionsLocked()
	session := m.sessions[strings.TrimSpace(state)]
	if session == nil || session.adminSessionHash != strings.TrimSpace(adminSessionHash) {
		return zaiOAuthStatusResponse{}, false
	}
	return m.statusLocked(session), true
}

func (m *zaiOAuthManager) statusLocked(session *zaiOAuthSession) zaiOAuthStatusResponse {
	return zaiOAuthStatusResponse{
		State: session.state, Status: session.status, Error: session.errorMsg,
		ChannelID: session.channelID, ChannelName: session.channelName,
	}
}

func (m *zaiOAuthManager) cancel(adminSessionHash, state string) error {
	if m == nil {
		return errors.New("z.ai OAuth session not found")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	session := m.sessions[strings.TrimSpace(state)]
	if session == nil || session.adminSessionHash != strings.TrimSpace(adminSessionHash) {
		return errors.New("z.ai OAuth session not found")
	}
	if !zaiOAuthSessionCancelable(session.status) {
		return fmt.Errorf("z.ai OAuth session cannot be cancelled while %s", session.status)
	}
	m.finishSessionLocked(session, "cancelled", "", 0, "")
	return nil
}

func zaiOAuthSessionCancelable(status string) bool {
	return status == "pending"
}

func (m *zaiOAuthManager) finishSessionLocked(
	session *zaiOAuthSession,
	status, errorMsg string,
	channelID int64,
	channelName string,
) {
	session.status = status
	session.errorMsg = errorMsg
	session.channelID = channelID
	session.channelName = channelName
	session.finishedAt = m.now()
	session.cancel()
	if m.activeByID[session.adminSessionHash] == session.state {
		delete(m.activeByID, session.adminSessionHash)
	}
}

func (m *zaiOAuthManager) pruneExpiredSessionsLocked() {
	now := m.now()
	for state, session := range m.sessions {
		if session.finishedAt.IsZero() && zaiOAuthSessionCancelable(session.status) &&
			now.Sub(session.createdAt) > zaiOAuthSessionTTL {
			m.finishSessionLocked(session, "error", "z.ai OAuth session expired", 0, "")
		}
		if !session.finishedAt.IsZero() && now.Sub(session.finishedAt) > zaiOAuthTerminalTTL {
			delete(m.sessions, state)
		}
	}
}

func (m *zaiOAuthManager) runJanitor() {
	ticker := time.NewTicker(zaiOAuthJanitorInterval)
	defer func() {
		ticker.Stop()
		close(m.janitorDone)
	}()
	for {
		select {
		case <-ticker.C:
			m.mu.Lock()
			m.pruneExpiredSessionsLocked()
			m.mu.Unlock()
		case <-m.stopJanitor:
			return
		}
	}
}

func (m *zaiOAuthManager) close() {
	if m == nil {
		return
	}
	m.closeOnce.Do(func() {
		close(m.stopJanitor)
		m.mu.Lock()
		m.closed = true
		cancels := make([]context.CancelFunc, 0, len(m.sessions))
		for _, session := range m.sessions {
			cancels = append(cancels, session.cancel)
		}
		m.sessions = make(map[string]*zaiOAuthSession)
		m.activeByID = make(map[string]string)
		m.mu.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
		m.pollers.Wait()
		<-m.janitorDone
	})
}

/**
 * @brief 启动所选国内或国际站点的 ZCode 授权。
 * @param c 管理端 HTTP 上下文。
 */
func (s *Server) HandleStartZAIOAuth(c *gin.Context) {
	adminSessionHash, ok := xaiAdminSessionHash(c)
	if !ok {
		RespondErrorMsg(c, http.StatusUnauthorized, "administrator session is unavailable")
		return
	}
	if s.zaiOAuth == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "z.ai OAuth is unavailable")
		return
	}
	var request zaiOAuthStartRequest
	if !bindOAuthLoginStart(c, &request) {
		return
	}
	options, err := s.oauthLoginOptions(request.oauthLoginStartRequest, false)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	response, err := s.zaiOAuth.start(c.Request.Context(), adminSessionHash, zaiOAuthOptions{oauthLoginOptions: options, site: request.Site})
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, zaiauth.ErrOAuthFlowUnavailable) {
			status = http.StatusServiceUnavailable
		}
		RespondError(c, status, err)
		return
	}
	RespondJSON(c, http.StatusOK, response)
}

// HandleZAIOAuthStatus returns only sessions owned by the current administrator.
func (s *Server) HandleZAIOAuthStatus(c *gin.Context) {
	adminSessionHash, ok := xaiAdminSessionHash(c)
	if !ok || s.zaiOAuth == nil {
		RespondErrorMsg(c, http.StatusNotFound, "z.ai OAuth session not found")
		return
	}
	status, exists := s.zaiOAuth.status(adminSessionHash, c.Query("state"))
	if !exists {
		RespondErrorMsg(c, http.StatusNotFound, "z.ai OAuth session not found")
		return
	}
	RespondJSON(c, http.StatusOK, status)
}

// HandleCancelZAIOAuth cancels one pending administrator-owned session.
func (s *Server) HandleCancelZAIOAuth(c *gin.Context) {
	adminSessionHash, ok := xaiAdminSessionHash(c)
	if !ok || s.zaiOAuth == nil {
		RespondErrorMsg(c, http.StatusNotFound, "z.ai OAuth session not found")
		return
	}
	var request zaiOAuthCancelRequest
	if err := BindAndValidate(c, &request); err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	if err := s.zaiOAuth.cancel(adminSessionHash, request.State); err != nil {
		RespondError(c, http.StatusConflict, err)
		return
	}
	RespondJSON(c, http.StatusOK, zaiOAuthStatusResponse{State: strings.TrimSpace(request.State), Status: "cancelled"})
}
