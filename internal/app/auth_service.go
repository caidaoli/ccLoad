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
	passwordHash []byte               // 管理员密码bcrypt哈希
	validTokens  map[string]time.Time // TokenHash → 过期时间
	tokensMux    sync.RWMutex         // 并发保护

	// API 认证（代理 API 使用的数据库令牌）
	// [FIX] 2025-12: 存储过期时间而非bool，支持懒惰过期校验
	authTokens          map[string]int64          // Token哈希 → 过期时间(Unix毫秒，0=永不过期)
	authTokenIDs        map[string]int64          // Token哈希 → Token ID 映射（用于日志记录，2025-12新增）
	authTokenModels     map[string][]string       // Token哈希 → 允许的模型列表（2026-01新增）
	authTokenCostLimits map[string]tokenCostLimit // Token哈希 → 费用限额状态（仅限额>0的令牌）
	authTokensMux       sync.RWMutex              // 并发保护（支持热更新）

	// 数据库依赖（用于热更新令牌）
	store storage.Store

	// 速率限制（防暴力破解）
	loginRateLimiter *util.LoginRateLimiter

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

// NewAuthService 创建认证服务实例
// 初始化时自动从数据库加载API访问令牌和管理员会话
func NewAuthService(
	password string,
	loginRateLimiter *util.LoginRateLimiter,
	store storage.Store,
) *AuthService {
	// 密码bcrypt哈希（安全存储）
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("FATAL: failed to hash password: %v", err)
	}

	s := &AuthService{
		passwordHash:        passwordHash,
		validTokens:         make(map[string]time.Time),
		authTokens:          make(map[string]int64),
		authTokenIDs:        make(map[string]int64),
		authTokenCostLimits: make(map[string]tokenCostLimit),
		loginRateLimiter:    loginRateLimiter,
		store:               store,
		lastUsedCh:          make(chan string, 256), // 带缓冲，避免阻塞请求
		done:                make(chan struct{}),
	}

	// 启动 last_used_at 更新 worker
	s.wg.Add(1)
	go s.lastUsedWorker()

	// 从数据库加载API访问令牌
	if err := s.ReloadAuthTokens(); err != nil {
		log.Printf("[WARN]  初始化时加载API令牌失败: %v", err)
	}

	// 从数据库加载管理员会话（支持重启后保持登录）
	if err := s.loadSessionsFromDB(); err != nil {
		log.Printf("[WARN]  初始化时加载管理员会话失败: %v", err)
	}

	return s
}

// loadSessionsFromDB 从数据库加载管理员会话
// [INFO] 安全修复：加载tokenHash→expiry映射(2025-12)
func (s *AuthService) loadSessionsFromDB() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sessions, err := s.store.LoadAllSessions(ctx)
	if err != nil {
		return err
	}

	s.tokensMux.Lock()
	for tokenHash, expiry := range sessions {
		s.validTokens[tokenHash] = expiry
	}
	s.tokensMux.Unlock()

	if len(sessions) > 0 {
		log.Printf("[INFO] 已恢复 %d 个管理员会话（重启后保持登录）", len(sessions))
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
	tokenHash := model.HashToken(token)

	s.tokensMux.RLock()
	expiry, exists := s.validTokens[tokenHash]
	s.tokensMux.RUnlock()

	if !exists {
		return false
	}

	// 检查是否过期
	if time.Now().After(expiry) {
		// 同步删除过期Token（避免goroutine泄漏）
		// 原因：map删除操作非常快（O(1)），无需异步，异步反而导致goroutine泄漏
		s.tokensMux.Lock()
		delete(s.validTokens, tokenHash)
		s.tokensMux.Unlock()
		return false
	}

	return true
}

// CleanExpiredTokens 清理过期Token（定期任务）
// 公开方法，供 Server 的后台协程调用
func (s *AuthService) CleanExpiredTokens() {
	now := time.Now()

	// 使用快照模式避免长时间持锁
	s.tokensMux.RLock()
	toDelete := make([]string, 0, len(s.validTokens)/10)
	for tokenHash, expiry := range s.validTokens {
		if now.After(expiry) {
			toDelete = append(toDelete, tokenHash)
		}
	}
	s.tokensMux.RUnlock()

	// 批量删除内存中的过期Token
	if len(toDelete) > 0 {
		s.tokensMux.Lock()
		for _, tokenHash := range toDelete {
			if expiry, exists := s.validTokens[tokenHash]; exists && now.After(expiry) {
				delete(s.validTokens, tokenHash)
			}
		}
		s.tokensMux.Unlock()
	}

	// 同时清理数据库中的过期会话
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.CleanExpiredSessions(ctx); err != nil {
		log.Printf("[WARN]  清理数据库过期会话失败: %v", err)
	}
}

// ============================================================================
// 认证中间件
// ============================================================================

// RequireTokenAuth Token 认证中间件（管理界面使用）
func (s *AuthService) RequireTokenAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 从 Authorization 头获取Token
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" {
			const prefix = "Bearer "
			if strings.HasPrefix(authHeader, prefix) {
				token := strings.TrimPrefix(authHeader, prefix)

				// 检查动态Token（登录生成的24小时Token）
				if s.isValidToken(token) {
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

		// 计算令牌哈希并验证
		tokenHash := model.HashToken(token)

		s.authTokensMux.RLock()
		expiresAt, exists := s.authTokens[tokenHash]
		tokenID, hasTokenID := s.authTokenIDs[tokenHash]
		s.authTokensMux.RUnlock()

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
			s.authTokensMux.Unlock()

			c.JSON(http.StatusUnauthorized, gin.H{"error": "token expired"})
			c.Abort()
			return
		}

		// 将tokenHash和tokenID存储到context，供后续统计使用（2025-11新增tokenHash, 2025-12新增tokenID）
		c.Set("token_hash", tokenHash)
		if hasTokenID {
			c.Set("token_id", tokenID)
		}

		// 异步更新last_used_at（发送到受控worker，不阻塞请求）
		select {
		case s.lastUsedCh <- tokenHash:
		default:
			// channel满时丢弃，避免阻塞（last_used_at非关键数据）
		}

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

	// 检查速率限制
	if !s.loginRateLimiter.AllowAttempt(clientIP) {
		lockoutTime := s.loginRateLimiter.GetLockoutTime(clientIP)
		RespondErrorWithData(c, http.StatusTooManyRequests, "Too many failed login attempts", gin.H{
			"message":         fmt.Sprintf("Account locked for %d seconds. Please try again later.", lockoutTime),
			"lockout_seconds": lockoutTime,
		})
		return
	}

	var req struct {
		Password string `json:"password" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "Invalid request format")
		return
	}

	// 验证密码（bcrypt安全比较）
	if err := bcrypt.CompareHashAndPassword(s.passwordHash, []byte(req.Password)); err != nil {
		// 记录失败尝试（速率限制器已在AllowAttempt中增加计数）
		attemptCount := s.loginRateLimiter.GetAttemptCount(clientIP)
		log.Printf("[WARN]  登录失败: IP=%s, 尝试次数=%d/5", clientIP, attemptCount)

		// [SECURITY] 不返回剩余尝试次数，避免攻击者推断速率限制状态
		RespondErrorMsg(c, http.StatusUnauthorized, "Invalid password")
		return
	}

	// 密码正确，重置速率限制
	s.loginRateLimiter.RecordSuccess(clientIP)

	// 生成Token
	token, err := s.generateToken()
	if err != nil {
		log.Printf("ERROR: token generation failed: %v", err)
		RespondErrorMsg(c, http.StatusInternalServerError, "internal error")
		return
	}
	expiry := time.Now().Add(config.TokenExpiry)

	// [INFO] 安全修复：存储tokenHash而非明文(2025-12)
	tokenHash := model.HashToken(token)

	// 存储TokenHash到内存
	s.tokensMux.Lock()
	s.validTokens[tokenHash] = expiry
	s.tokensMux.Unlock()

	// [INFO] 修复：同步写入数据库（SQLite本地写入极快，微秒级，无需异步）
	// 原因：异步goroutine未受控，关机时可能写入已关闭的连接
	// [FIX] P0-4: 使用 defer cancel() 防止 context 泄漏
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := s.store.CreateAdminSession(ctx, token, expiry); err != nil {
		log.Printf("[WARN]  保存管理员会话到数据库失败: %v", err)
		// 注意：内存中的token仍然有效，下次重启会丢失此会话
	}

	log.Printf("[INFO] 登录成功: IP=%s", clientIP)

	// 返回明文Token给客户端（前端存储到localStorage）
	RespondJSON(c, http.StatusOK, gin.H{
		"token":     token,                             // 明文token返回给客户端
		"expiresIn": int(config.TokenExpiry.Seconds()), // 秒数
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

		if err := s.store.DeleteAdminSession(ctx, token); err != nil {
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
	newTokenModels := make(map[string][]string, len(tokens))
	newTokenCostLimits := make(map[string]tokenCostLimit, len(tokens))
	for _, t := range tokens {
		// ExpiresAt: nil → 0 (永不过期), *int64 → Unix毫秒
		var expiresAt int64
		if t.ExpiresAt != nil {
			expiresAt = *t.ExpiresAt
		}
		newTokens[t.Token] = expiresAt
		newTokenIDs[t.Token] = t.ID
		// 只有有限制时才存储（节省内存）
		if len(t.AllowedModels) > 0 {
			newTokenModels[t.Token] = t.AllowedModels
		}
		// 费用限额：只为“有限额”的令牌维护状态（避免无谓内存占用）
		limitMicro := t.CostLimitMicroUSD
		if limitMicro > 0 {
			newTokenCostLimits[t.Token] = tokenCostLimit{
				usedMicroUSD:  t.CostUsedMicroUSD,
				limitMicroUSD: limitMicro,
			}
		}
	}

	// 原子替换（避免读写竞争）
	s.authTokensMux.Lock()
	s.authTokens = newTokens
	s.authTokenIDs = newTokenIDs
	s.authTokenModels = newTokenModels
	s.authTokenCostLimits = newTokenCostLimits
	s.authTokensMux.Unlock()

	return nil
}

// IsModelAllowed 检查令牌是否允许访问指定模型
// 如果令牌没有模型限制，返回 true
func (s *AuthService) IsModelAllowed(tokenHash, model string) bool {
	s.authTokensMux.RLock()
	allowedModels, hasRestriction := s.authTokenModels[tokenHash]
	s.authTokensMux.RUnlock()

	if !hasRestriction {
		return true // 无限制
	}

	for _, m := range allowedModels {
		if strings.EqualFold(m, model) {
			return true
		}
	}
	return false
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
