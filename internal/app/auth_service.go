package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// AuthService 认证和授权服务
// 职责：处理所有认证和授权相关的业务逻辑
// - Token 认证（管理界面动态令牌）
// - API 认证（数据库驱动的访问令牌）
// - 登录/登出处理
// - 速率限制（防暴力破解）
//
// 遵循 SRP 原则：仅负责认证授权，不涉及代理、日志、管理 API
type AuthService struct {
	// Token 认证（管理界面使用的动态 Token）
	// [INFO] 安全修复：存储SHA256哈希而非明文(2025-12)
	passwordHash []byte                      // 管理员密码bcrypt哈希
	validTokens  map[string]model.WebSession // WebSessionTokenHash → 会话身份
	tokensMux    sync.RWMutex                // 并发保护

	// API 认证（代理 API 使用的数据库令牌）
	// [FIX] 2025-12: 存储过期时间而非bool，支持懒惰过期校验
	authTokens          map[string]int64                   // Token哈希 → 过期时间(Unix毫秒，0=永不过期)
	authTokenIDs        map[string]int64                   // Token哈希 → Token ID 映射（用于日志记录，2025-12新增）
	authTokenHashes     map[int64]string                   // Token ID → Token哈希（Web会话绑定代理身份）
	authTokenModels     map[string][]string                // Token哈希 → 允许的模型列表（2026-01新增）
	authTokenChannels   map[string]tokenChannelRestriction // Token哈希 → 渠道限制（模式+ID列表）
	authTokenCostLimits map[string]tokenCostLimit          // Token哈希 → 费用限额状态（仅限额>0的令牌）
	authTokenMaxConns   map[string]int                     // Token哈希 → 最大并发请求数（0=无限制）
	authTokenActiveReqs map[string]int                     // Token哈希 → 当前进行中请求数
	authTokensMux       sync.RWMutex                       // 并发保护（支持热更新）

	// 数据库依赖（用于热更新令牌）
	store storage.Store

	// 速率限制（防暴力破解）
	loginRateLimiter       *util.LoginRateLimiter
	apiTokenSessionLimiter *apiTokenSessionLimiter

	// 异步更新 last_used_at（受控 worker，避免 goroutine 泄漏）
	lastUsedCh chan string    // tokenHash 更新队列
	done       chan struct{}  // 关闭信号
	wg         sync.WaitGroup // 优雅关闭
	// [FIX] 2025-12：保证 Close 幂等性，防止重复关闭 channel 导致 panic
	closeOnce sync.Once
}

type tokenCostLimit struct {
	usedMicroUSD  int64
	limitMicroUSD int64
}

// tokenChannelRestriction 缓存单个 token 的渠道限制（列表为空表示无限制，不入缓存）。
type tokenChannelRestriction struct {
	Mode string
	IDs  []int64
}

var authPasswordHashCost = bcrypt.DefaultCost

// NewAuthService 创建认证服务实例
// 初始化时自动从数据库加载 API 访问令牌和 Web 会话。
func NewAuthService(
	password string,
	loginRateLimiter *util.LoginRateLimiter,
	store storage.Store,
) *AuthService {
	// 密码bcrypt哈希（安全存储）
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), authPasswordHashCost)
	if err != nil {
		log.Fatalf("[FATAL] 密码哈希失败: %v", err)
	}

	s := &AuthService{
		passwordHash:           passwordHash,
		validTokens:            make(map[string]model.WebSession),
		authTokens:             make(map[string]int64),
		authTokenIDs:           make(map[string]int64),
		authTokenHashes:        make(map[int64]string),
		authTokenModels:        make(map[string][]string),
		authTokenChannels:      make(map[string]tokenChannelRestriction),
		authTokenCostLimits:    make(map[string]tokenCostLimit),
		authTokenMaxConns:      make(map[string]int),
		authTokenActiveReqs:    make(map[string]int),
		loginRateLimiter:       loginRateLimiter,
		apiTokenSessionLimiter: newAPITokenSessionLimiter(nil),
		store:                  store,
		lastUsedCh:             make(chan string, 256), // 带缓冲，避免阻塞请求
		done:                   make(chan struct{}),
	}

	// 启动 last_used_at 更新 worker
	s.wg.Add(1)
	go s.lastUsedWorker()

	// 从数据库加载API访问令牌
	if err := s.ReloadAuthTokens(); err != nil {
		log.Printf("[WARN]  初始化时加载API令牌失败: %v", err)
	}

	// 从数据库加载 Web 会话（支持重启后保持登录）
	if err := s.loadSessionsFromDB(); err != nil {
		log.Printf("[WARN]  初始化时加载管理员会话失败: %v", err)
	}

	return s
}

// loadSessionsFromDB 从数据库加载角色化 Web 会话。
func (s *AuthService) loadSessionsFromDB() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sessions, err := s.store.LoadWebSessions(ctx)
	if err != nil {
		return err
	}

	s.tokensMux.Lock()
	for tokenHash, session := range sessions {
		s.validTokens[tokenHash] = session
	}
	s.tokensMux.Unlock()

	if len(sessions) > 0 {
		log.Printf("[INFO] 已恢复 %d 个 Web 会话（重启后保持登录）", len(sessions))
	}
	return nil
}

// lastUsedWorker 处理 last_used_at 更新的后台 worker
func (s *AuthService) lastUsedWorker() {
	defer s.wg.Done()
	for {
		select {
		case <-s.done:
			return
		case tokenHash := <-s.lastUsedCh:
			// [FIX] P0-4: WithTimeout 的 cancel 必须在每次循环内执行，不能在循环里 defer 到 goroutine 退出。
			func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()

				_ = s.store.UpdateTokenLastUsed(ctx, tokenHash, time.Now())
			}()
		}
	}
}

// Close 优雅关闭 AuthService（幂等，可安全多次调用）
func (s *AuthService) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.wg.Wait()
	})
}

// ============================================================================
// Token 生成和验证（内部方法）
// ============================================================================

// generateToken 生成安全Token（64字符十六进制）
func (s *AuthService) generateToken() (string, error) {
	b := make([]byte, config.TokenRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto/rand failed: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// isValidToken 验证Token有效性（检查过期时间）
// [INFO] 安全修复：通过tokenHash查询(2025-12)
func (s *AuthService) isValidToken(token string) bool {
	_, ok := s.webSession(token)
	return ok
}

func (s *AuthService) webSession(token string) (model.WebSession, bool) {
	tokenHash := model.HashToken(token)

	s.tokensMux.RLock()
	session, exists := s.validTokens[tokenHash]
	s.tokensMux.RUnlock()

	if !exists {
		return model.WebSession{}, false
	}

	if time.Now().After(session.ExpiresAt) {
		// 同步删除过期Token（避免goroutine泄漏）
		// 原因：map删除操作非常快（O(1)），无需异步，异步反而导致goroutine泄漏
		s.tokensMux.Lock()
		delete(s.validTokens, tokenHash)
		s.tokensMux.Unlock()
		return model.WebSession{}, false
	}

	validIdentity := session.Role == model.WebRoleAdmin && session.AuthTokenID == 0
	if session.Role == model.WebRoleAPIToken {
		validIdentity = s.isActiveAuthTokenID(session.AuthTokenID)
	}
	if !validIdentity {
		if session.AuthTokenID > 0 && s.store != nil {
			_ = s.revokeWebSessions([]int64{session.AuthTokenID})
		} else {
			s.tokensMux.Lock()
			delete(s.validTokens, tokenHash)
			s.tokensMux.Unlock()
		}
		return model.WebSession{}, false
	}

	return session, true
}

func (s *AuthService) isActiveAuthTokenID(tokenID int64) bool {
	if tokenID <= 0 {
		return false
	}
	s.authTokensMux.RLock()
	tokenHash, ok := s.authTokenHashes[tokenID]
	if !ok {
		s.authTokensMux.RUnlock()
		return false
	}
	expiresAt, ok := s.authTokens[tokenHash]
	s.authTokensMux.RUnlock()
	return ok && (expiresAt <= 0 || time.Now().UnixMilli() <= expiresAt)
}

// CleanExpiredTokens 清理过期Token（定期任务）
// 公开方法，供 Server 的后台协程调用
func (s *AuthService) CleanExpiredTokens() {
	now := time.Now()
	s.apiTokenSessionLimiter.cleanup()

	// 使用快照模式避免长时间持锁
	s.tokensMux.RLock()
	toDelete := make([]string, 0, len(s.validTokens)/10)
	for tokenHash, session := range s.validTokens {
		if now.After(session.ExpiresAt) {
			toDelete = append(toDelete, tokenHash)
		}
	}
	s.tokensMux.RUnlock()

	// 批量删除内存中的过期Token
	if len(toDelete) > 0 {
		s.tokensMux.Lock()
		for _, tokenHash := range toDelete {
			if session, exists := s.validTokens[tokenHash]; exists && now.After(session.ExpiresAt) {
				delete(s.validTokens, tokenHash)
			}
		}
		s.tokensMux.Unlock()
	}

	// 同时清理数据库中的过期会话
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.CleanExpiredWebSessions(ctx); err != nil {
		log.Printf("[WARN]  清理数据库过期会话失败: %v", err)
	}
}

// ============================================================================
// 认证中间件
// ============================================================================

// RequireWebAuth accepts administrator and API-token web sessions.
func (s *AuthService) RequireWebAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 从 Authorization 头获取Token
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" {
			const prefix = "Bearer "
			if strings.HasPrefix(authHeader, prefix) {
				token := strings.TrimPrefix(authHeader, prefix)

				if session, ok := s.webSession(token); ok {
					c.Set(webIdentityContextKey, WebIdentity{Role: session.Role, AuthTokenID: session.AuthTokenID})
					c.Next()
					return
				}
			}
		}

		// 未授权
		RespondErrorMsg(c, http.StatusUnauthorized, "未授权访问，请先登录")
		c.Abort()
	}
}

// RequireAdminAuth accepts only administrator web sessions.
func (s *AuthService) RequireAdminAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		token, ok := strings.CutPrefix(authHeader, "Bearer ")
		if !ok {
			RespondErrorMsg(c, http.StatusUnauthorized, "未授权访问，请先登录")
			c.Abort()
			return
		}
		session, ok := s.webSession(token)
		if !ok {
			RespondErrorMsg(c, http.StatusUnauthorized, "未授权访问，请先登录")
			c.Abort()
			return
		}
		if session.Role != model.WebRoleAdmin {
			RespondErrorMsg(c, http.StatusForbidden, "管理员权限不足")
			c.Abort()
			return
		}
		c.Set(webIdentityContextKey, WebIdentity{Role: session.Role})
		c.Next()
	}
}

// RequireWebAPITokenProxyAuth attaches the backing API-token identity to a
// token-scoped web request before it enters the normal proxy handler.
func (s *AuthService) RequireWebAPITokenProxyAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		identity, ok := WebIdentityFromContext(c)
		if !ok || identity.Role != model.WebRoleAPIToken || identity.AuthTokenID <= 0 {
			RespondErrorMsg(c, http.StatusForbidden, "API Token 会话权限不足")
			c.Abort()
			return
		}

		s.authTokensMux.RLock()
		tokenHash, exists := s.authTokenHashes[identity.AuthTokenID]
		expiresAt := s.authTokens[tokenHash]
		s.authTokensMux.RUnlock()
		if !exists || (expiresAt > 0 && time.Now().UnixMilli() > expiresAt) {
			RespondErrorMsg(c, http.StatusUnauthorized, "API Token 已失效")
			c.Abort()
			return
		}

		releaseTokenSlot, activeConns, maxConns, acquired := s.prepareAPIIdentity(c, tokenHash, identity.AuthTokenID)
		if !acquired {
			RespondErrorWithData(c, http.StatusTooManyRequests, "Token concurrency limit exceeded", gin.H{
				"message": fmt.Sprintf("Token concurrency limit exceeded: %d active of %d limit", activeConns, maxConns),
				"type":    "rate_limit_error",
				"code":    "token_concurrency_exceeded",
			})
			c.Abort()
			return
		}
		defer releaseTokenSlot()

		c.Next()
	}
}

func (s *AuthService) prepareAPIIdentity(c *gin.Context, tokenHash string, tokenID int64) (func(), int, int, bool) {
	release, activeConns, maxConns, ok := s.acquireTokenConcurrencySlot(tokenHash)
	if !ok {
		return nil, activeConns, maxConns, false
	}
	c.Set("token_hash", tokenHash)
	if tokenID > 0 {
		c.Set("token_id", tokenID)
	}
	select {
	case s.lastUsedCh <- tokenHash:
	default:
	}
	return release, activeConns, maxConns, true
}

func (s *AuthService) resolveAuthToken(token string) (tokenHash string, expiresAt, tokenID int64, exists bool) {
	s.authTokensMux.RLock()
	defer s.authTokensMux.RUnlock()

	tokenHash = token
	expiresAt, exists = s.authTokens[tokenHash]
	if !exists {
		tokenHash = model.HashToken(token)
		expiresAt, exists = s.authTokens[tokenHash]
	}
	if exists {
		tokenID = s.authTokenIDs[tokenHash]
	}
	return tokenHash, expiresAt, tokenID, exists
}

// RequireAPIAuth API 认证中间件（代理 API 使用）
// [FIX] 2025-12: 添加过期时间校验，支持懒惰剔除过期令牌
func (s *AuthService) RequireAPIAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 未配置认证令牌时，默认全部返回 401（不允许公开访问）
		s.authTokensMux.RLock()
		tokenCount := len(s.authTokens)
		s.authTokensMux.RUnlock()

		if tokenCount == 0 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing authorization"})
			c.Abort()
			return
		}

		var token string
		var tokenFound bool

		// 检查 Authorization 头（Bearer token）
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" {
			const prefix = "Bearer "
			if strings.HasPrefix(authHeader, prefix) {
				token = strings.TrimPrefix(authHeader, prefix)
				tokenFound = true
			}
		}

		// 检查 X-API-Key 头
		if !tokenFound {
			apiKey := c.GetHeader("X-API-Key")
			if apiKey != "" {
				token = apiKey
				tokenFound = true
			}
		}

		// 检查 x-goog-api-key 头（Google API格式）
		if !tokenFound {
			googAPIKey := c.GetHeader("x-goog-api-key")
			if googAPIKey != "" {
				token = googAPIKey
				tokenFound = true
			}
		}

		// 检查 URL 查询参数 key（Gemini API格式：?key=xxx）
		if !tokenFound {
			queryKey := c.Query("key")
			if queryKey != "" {
				token = queryKey
				tokenFound = true
			}
		}

		if !tokenFound {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing authorization"})
			c.Abort()
			return
		}

		// 双路径验证：先尝试直接匹配（客户端发送的是hash值），再尝试SHA256匹配（客户端发送的是明文）
		tokenHash, expiresAt, tokenID, exists := s.resolveAuthToken(token)

		if !exists {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing authorization"})
			c.Abort()
			return
		}

		// [FIX] 过期校验：expiresAt > 0 表示有过期时间，检查是否已过期
		if expiresAt > 0 && time.Now().UnixMilli() > expiresAt {
			// 懒惰剔除：过期时从内存中移除（避免下次还要检查）
			s.authTokensMux.Lock()
			delete(s.authTokens, tokenHash)
			delete(s.authTokenIDs, tokenHash)
			delete(s.authTokenHashes, tokenID)
			delete(s.authTokenModels, tokenHash)
			delete(s.authTokenChannels, tokenHash)
			delete(s.authTokenCostLimits, tokenHash)
			delete(s.authTokenMaxConns, tokenHash)
			s.authTokensMux.Unlock()
			if tokenID > 0 {
				if err := s.revokeWebSessions([]int64{tokenID}); err != nil {
					log.Printf("[WARN] 撤销过期 API Token 的 Web 会话失败: %v", err)
				}
			}

			c.JSON(http.StatusUnauthorized, gin.H{"error": "token expired"})
			c.Abort()
			return
		}

		releaseTokenSlot, activeConns, maxConns, acquired := s.prepareAPIIdentity(c, tokenHash, tokenID)
		if !acquired {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": gin.H{
					"message": fmt.Sprintf("Token concurrency limit exceeded: %d active of %d limit", activeConns, maxConns),
					"type":    "rate_limit_error",
					"code":    "token_concurrency_exceeded",
				},
			})
			c.Abort()
			return
		}
		defer releaseTokenSlot()

		c.Next()
	}
}

// ============================================================================
// 登录/登出处理
// ============================================================================

// HandleLogin 处理登录请求
// 集成登录速率限制，防暴力破解
func (s *AuthService) HandleLogin(c *gin.Context) {
	clientIP := c.ClientIP()

	var req struct {
		Mode     model.WebRole `json:"mode" binding:"required"`
		Password string        `json:"password"`
		Token    string        `json:"token"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "Invalid request format")
		return
	}

	var session model.WebSession
	switch req.Mode {
	case model.WebRoleAdmin:
		adminRateKey := "admin:" + clientIP
		if !s.loginRateLimiter.AllowAttempt(adminRateKey) {
			lockoutTime := s.loginRateLimiter.GetLockoutTime(adminRateKey)
			RespondErrorWithData(c, http.StatusTooManyRequests, "Too many failed login attempts", gin.H{
				"message":         fmt.Sprintf("Account locked for %d seconds. Please try again later.", lockoutTime),
				"lockout_seconds": lockoutTime,
			})
			return
		}
		if err := bcrypt.CompareHashAndPassword(s.passwordHash, []byte(req.Password)); err != nil {
			attemptCount := s.loginRateLimiter.GetAttemptCount(adminRateKey)
			log.Printf("[WARN] 登录失败: IP=%s, 尝试次数=%d/5", clientIP, attemptCount)
			RespondErrorMsg(c, http.StatusUnauthorized, "Invalid credentials")
			return
		}
		s.loginRateLimiter.RecordSuccess(adminRateKey)
		session.Role = model.WebRoleAdmin
	case model.WebRoleAPIToken:
		_, expiresAt, tokenID, exists := s.resolveAuthToken(strings.TrimSpace(req.Token))
		if !exists || tokenID <= 0 || (expiresAt > 0 && time.Now().UnixMilli() > expiresAt) {
			RespondErrorMsg(c, http.StatusUnauthorized, "Invalid credentials")
			return
		}
		allowed, retryAfter := s.apiTokenSessionLimiter.allow(tokenID)
		if !allowed {
			retryAfterSeconds := int((retryAfter + time.Second - 1) / time.Second)
			c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSeconds))
			RespondErrorWithData(c, http.StatusTooManyRequests, "Too many API Token web sessions", gin.H{
				"message":             fmt.Sprintf("API Token web session limit exceeded. Please retry in %d seconds.", retryAfterSeconds),
				"retry_after_seconds": retryAfterSeconds,
			})
			return
		}
		session.Role = model.WebRoleAPIToken
		session.AuthTokenID = tokenID
	default:
		RespondErrorMsg(c, http.StatusBadRequest, "Invalid request format")
		return
	}

	// 生成Token
	token, err := s.generateToken()
	if err != nil {
		log.Printf("[ERROR] 令牌生成失败: %v", err)
		RespondErrorMsg(c, http.StatusInternalServerError, "internal error")
		return
	}
	expiry := time.Now().Add(config.TokenExpiry)
	session.ExpiresAt = expiry

	// [INFO] 安全修复：存储tokenHash而非明文(2025-12)
	tokenHash := model.HashToken(token)

	// 存储TokenHash到内存
	s.tokensMux.Lock()
	session.TokenHash = tokenHash
	s.validTokens[tokenHash] = session
	s.tokensMux.Unlock()

	// [INFO] 修复：同步写入数据库（SQLite本地写入极快，微秒级，无需异步）
	// 原因：异步goroutine未受控，关机时可能写入已关闭的连接
	// [FIX] P0-4: 使用 defer cancel() 防止 context 泄漏
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := s.store.CreateWebSession(ctx, token, session); err != nil {
		log.Printf("[WARN] 保存 Web 会话到数据库失败: %v", err)
		// 注意：内存中的token仍然有效，下次重启会丢失此会话
	}

	log.Printf("[INFO] 登录成功: IP=%s", clientIP)

	// 返回明文Token给客户端（前端存储到localStorage）
	RespondJSON(c, http.StatusOK, gin.H{
		"token":     token,                             // 明文token返回给客户端
		"expiresIn": int(config.TokenExpiry.Seconds()), // 秒数
		"role":      session.Role,
	})
}

// HandleLogout 处理登出请求
func (s *AuthService) HandleLogout(c *gin.Context) {
	// 从Authorization头提取Token
	authHeader := c.GetHeader("Authorization")
	const prefix = "Bearer "
	if after, ok := strings.CutPrefix(authHeader, prefix); ok {
		token := after

		// [INFO] 安全修复：计算tokenHash删除(2025-12)
		tokenHash := model.HashToken(token)

		// 删除内存中的TokenHash
		s.tokensMux.Lock()
		delete(s.validTokens, tokenHash)
		s.tokensMux.Unlock()

		// [INFO] 修复：同步删除数据库中的会话（SQLite本地删除极快，微秒级，无需异步）
		// 原因：异步goroutine未受控，关机时可能写入已关闭的连接
		// [FIX] P0-4: 使用 defer cancel() 防止 context 泄漏
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		if err := s.store.DeleteWebSession(ctx, token); err != nil {
			log.Printf("[WARN]  删除数据库会话失败: %v", err)
		}
	}

	RespondJSON(c, http.StatusOK, gin.H{"message": "已登出"})
}

// ============================================================================
// API令牌热更新
// ============================================================================

// ReloadAuthTokens 从数据库重新加载API访问令牌
// 用于CRUD操作后立即生效，无需重启服务
// [FIX] 2025-12: 同时加载过期时间，支持懒惰过期校验
func (s *AuthService) ReloadAuthTokens() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tokens, err := s.store.ListActiveAuthTokens(ctx)
	if err != nil {
		return fmt.Errorf("reload auth tokens: %w", err)
	}

	// 构建新的令牌映射（存储过期时间而非bool）
	newTokens := make(map[string]int64, len(tokens))
	newTokenIDs := make(map[string]int64, len(tokens))
	newTokenHashes := make(map[int64]string, len(tokens))
	newTokenModels := make(map[string][]string, len(tokens))
	newTokenChannels := make(map[string]tokenChannelRestriction, len(tokens))
	newTokenCostLimits := make(map[string]tokenCostLimit, len(tokens))
	newTokenMaxConns := make(map[string]int, len(tokens))
	for _, t := range tokens {
		if err := t.ValidateUsageLimits(); err != nil {
			return fmt.Errorf("invalid auth token %d: %w", t.ID, err)
		}
		// ExpiresAt: nil → 0 (永不过期), *int64 → Unix毫秒
		var expiresAt int64
		if t.ExpiresAt != nil {
			expiresAt = *t.ExpiresAt
		}
		newTokens[t.Token] = expiresAt
		newTokenIDs[t.Token] = t.ID
		newTokenHashes[t.ID] = t.Token
		// 只有有限制时才存储（节省内存）
		if len(t.AllowedModels) > 0 {
			newTokenModels[t.Token] = t.AllowedModels
		}
		if len(t.AllowedChannelIDs) > 0 {
			newTokenChannels[t.Token] = tokenChannelRestriction{
				Mode: model.NormalizeChannelRestrictionMode(t.ChannelRestrictionMode),
				IDs:  t.AllowedChannelIDs,
			}
		}
		// 费用限额：只为“有限额”的令牌维护状态（避免无谓内存占用）
		limitMicro := t.CostLimitMicroUSD
		if limitMicro > 0 {
			newTokenCostLimits[t.Token] = tokenCostLimit{
				usedMicroUSD:  t.CostUsedMicroUSD,
				limitMicroUSD: limitMicro,
			}
		}
		if t.MaxConcurrency > 0 {
			newTokenMaxConns[t.Token] = t.MaxConcurrency
		}
	}

	// 原子替换（避免读写竞争）
	s.authTokensMux.Lock()
	revokedTokenIDs := make([]int64, 0)
	now := time.Now().UnixMilli()
	for tokenID, oldHash := range s.authTokenHashes {
		newHash, active := newTokenHashes[tokenID]
		oldExpiresAt := s.authTokens[oldHash]
		if !active || newHash != oldHash || (oldExpiresAt > 0 && now > oldExpiresAt) {
			revokedTokenIDs = append(revokedTokenIDs, tokenID)
		}
	}
	// [FIX] P0-1: 防止 DB 滞后值覆盖内存实时累加。
	// AddCostToCache 只更新内存，DB 由 UpdateTokenStats 异步落盘；reload 读到的 DB used
	// 可能落后于内存累加。内存累加恒 ≥ 已落盘值，故取 max 保留未落盘的记账，避免限额被绕过。
	// （管理员清零额度应走专门接口同步清内存，不依赖 reload 路径。）
	for tok, lim := range newTokenCostLimits {
		if old, ok := s.authTokenCostLimits[tok]; ok && old.usedMicroUSD > lim.usedMicroUSD {
			lim.usedMicroUSD = old.usedMicroUSD
			newTokenCostLimits[tok] = lim
		}
	}
	s.authTokens = newTokens
	s.authTokenIDs = newTokenIDs
	s.authTokenHashes = newTokenHashes
	s.authTokenModels = newTokenModels
	s.authTokenChannels = newTokenChannels
	s.authTokenCostLimits = newTokenCostLimits
	s.authTokenMaxConns = newTokenMaxConns
	s.authTokensMux.Unlock()
	if err := s.revokeWebSessions(revokedTokenIDs); err != nil {
		return fmt.Errorf("revoke web sessions: %w", err)
	}

	return nil
}

func (s *AuthService) revokeWebSessions(tokenIDs []int64) error {
	if len(tokenIDs) == 0 {
		return nil
	}
	revoked := make(map[int64]struct{}, len(tokenIDs))
	for _, tokenID := range tokenIDs {
		revoked[tokenID] = struct{}{}
	}

	s.tokensMux.Lock()
	for tokenHash, session := range s.validTokens {
		if _, ok := revoked[session.AuthTokenID]; ok {
			delete(s.validTokens, tokenHash)
		}
	}
	s.tokensMux.Unlock()

	if s.store == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, tokenID := range tokenIDs {
		if err := s.store.DeleteWebSessionsByAuthTokenID(ctx, tokenID); err != nil {
			return err
		}
	}
	return nil
}

func (s *AuthService) getAllowedModelSet(tokenHash string) (map[string]struct{}, bool) {
	s.authTokensMux.RLock()
	allowedModels, hasRestriction := s.authTokenModels[tokenHash]
	s.authTokensMux.RUnlock()

	if !hasRestriction || len(allowedModels) == 0 {
		return nil, false
	}

	allowedSet := make(map[string]struct{}, len(allowedModels))
	for _, model := range allowedModels {
		allowedSet[strings.ToLower(model)] = struct{}{}
	}
	return allowedSet, true
}

// FilterAllowedModels 按 token 的模型限制过滤候选模型列表。
// 无限制时原样返回，保持“模型列表可见性”和“实际请求可用性”使用同一套规则。
func (s *AuthService) FilterAllowedModels(tokenHash string, models []string) []string {
	allowedSet, hasRestriction := s.getAllowedModelSet(tokenHash)
	if !hasRestriction || len(models) == 0 {
		return models
	}

	filtered := make([]string, 0, len(models))
	for _, model := range models {
		if _, ok := allowedSet[strings.ToLower(model)]; ok {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

// IsModelAllowed 检查令牌是否允许访问指定模型
// 如果令牌没有模型限制，返回 true
func (s *AuthService) IsModelAllowed(tokenHash, model string) bool {
	allowedSet, hasRestriction := s.getAllowedModelSet(tokenHash)
	if !hasRestriction {
		return true // 无限制
	}
	_, ok := allowedSet[strings.ToLower(model)]
	return ok
}

func (s *AuthService) getChannelRestriction(tokenHash string) (tokenChannelRestriction, bool) {
	s.authTokensMux.RLock()
	restriction, hasRestriction := s.authTokenChannels[tokenHash]
	s.authTokensMux.RUnlock()

	if !hasRestriction || len(restriction.IDs) == 0 {
		return tokenChannelRestriction{}, false
	}
	restriction.Mode = model.NormalizeChannelRestrictionMode(restriction.Mode)
	return restriction, true
}

func channelIDSet(ids []int64) map[int64]struct{} {
	set := make(map[int64]struct{}, len(ids))
	for _, channelID := range ids {
		set[channelID] = struct{}{}
	}
	return set
}

// FilterAllowedChannels 按 token 的渠道限制过滤候选渠道。
// 返回值 restricted 表示该 token 是否启用了渠道限制。
func (s *AuthService) FilterAllowedChannels(tokenHash string, channels []*model.Config) ([]*model.Config, bool) {
	restriction, hasRestriction := s.getChannelRestriction(tokenHash)
	if !hasRestriction || len(channels) == 0 {
		return channels, hasRestriction
	}

	idSet := channelIDSet(restriction.IDs)
	deny := restriction.Mode == model.ChannelRestrictionModeDeny
	filtered := make([]*model.Config, 0, len(channels))
	for _, cfg := range channels {
		if cfg == nil {
			continue
		}
		_, inList := idSet[cfg.ID]
		if deny {
			if !inList {
				filtered = append(filtered, cfg)
			}
			continue
		}
		if inList {
			filtered = append(filtered, cfg)
		}
	}
	return filtered, true
}

// IsChannelAllowed 检查令牌是否允许访问指定渠道
// 如果令牌没有渠道限制，返回 true
func (s *AuthService) IsChannelAllowed(tokenHash string, channelID int64) bool {
	restriction, hasRestriction := s.getChannelRestriction(tokenHash)
	if !hasRestriction {
		return true
	}
	inList := false
	for _, id := range restriction.IDs {
		if id == channelID {
			inList = true
			break
		}
	}
	if restriction.Mode == model.ChannelRestrictionModeDeny {
		return !inList
	}
	return inList
}

func (s *AuthService) acquireTokenConcurrencySlot(tokenHash string) (release func(), active, limit int, ok bool) {
	if tokenHash == "" {
		return func() {}, 0, 0, true
	}

	s.authTokensMux.Lock()
	if s.authTokenActiveReqs == nil {
		s.authTokenActiveReqs = make(map[string]int)
	}

	current := s.authTokenActiveReqs[tokenHash]
	active = current + 1
	s.authTokenActiveReqs[tokenHash] = active
	limit = s.authTokenMaxConns[tokenHash]
	if limit > 0 && active > limit {
		if current <= 0 {
			delete(s.authTokenActiveReqs, tokenHash)
		} else {
			s.authTokenActiveReqs[tokenHash] = current
		}
		s.authTokensMux.Unlock()
		return nil, current, limit, false
	}
	s.authTokensMux.Unlock()

	return func() {
		s.authTokensMux.Lock()
		current := s.authTokenActiveReqs[tokenHash]
		if current <= 1 {
			delete(s.authTokenActiveReqs, tokenHash)
		} else {
			s.authTokenActiveReqs[tokenHash] = current - 1
		}
		s.authTokensMux.Unlock()
	}, active, limit, true
}

// IsCostLimitExceeded 检查令牌是否超过费用限额（微美元，整数比较）
// 若令牌无限额/未启用限额：exceeded=false 且 used/limit=0
func (s *AuthService) IsCostLimitExceeded(tokenHash string) (usedMicroUSD, limitMicroUSD int64, exceeded bool) {
	s.authTokensMux.RLock()
	v, ok := s.authTokenCostLimits[tokenHash]
	s.authTokensMux.RUnlock()

	if !ok || v.limitMicroUSD <= 0 {
		return 0, 0, false
	}

	return v.usedMicroUSD, v.limitMicroUSD, v.usedMicroUSD >= v.limitMicroUSD
}

// AddCostToCache 原子更新令牌的已消耗费用缓存
// 仅更新内存缓存，数据库更新由 UpdateTokenStats 异步处理
func (s *AuthService) AddCostToCache(tokenHash string, deltaMicroUSD int64) {
	if deltaMicroUSD <= 0 {
		return
	}

	s.authTokensMux.Lock()
	v, ok := s.authTokenCostLimits[tokenHash]
	if ok && v.limitMicroUSD > 0 {
		v.usedMicroUSD += deltaMicroUSD
		s.authTokenCostLimits[tokenHash] = v
	}
	s.authTokensMux.Unlock()
}
