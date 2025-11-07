package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
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
	password    string               // 管理员密码
	validTokens map[string]time.Time // Token → 过期时间
	tokensMux   sync.RWMutex         // 并发保护

	// API 认证（代理 API 使用的数据库令牌）
	authTokens    map[string]bool // 数据库令牌集合（SHA256哈希）
	authTokensMux sync.RWMutex    // 并发保护（支持热更新）

	// 数据库依赖（用于热更新令牌）
	store storage.Store

	// 速率限制（防暴力破解）
	loginRateLimiter *util.LoginRateLimiter
}

// NewAuthService 创建认证服务实例
// 初始化时自动从数据库加载API访问令牌
func NewAuthService(
	password string,
	loginRateLimiter *util.LoginRateLimiter,
	store storage.Store,
) *AuthService {
	s := &AuthService{
		password:         password,
		validTokens:      make(map[string]time.Time),
		authTokens:       make(map[string]bool),
		loginRateLimiter: loginRateLimiter,
		store:            store,
	}

	// 从数据库加载API访问令牌
	if err := s.ReloadAuthTokens(); err != nil {
		util.SafePrintf("⚠️  初始化时加载API令牌失败: %v", err)
	}

	return s
}

// ============================================================================
// Token 生成和验证（内部方法）
// ============================================================================

// generateToken 生成安全Token（64字符十六进制）
func (s *AuthService) generateToken() string {
	b := make([]byte, config.TokenRandomBytes)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// isValidToken 验证Token有效性（检查过期时间）
func (s *AuthService) isValidToken(token string) bool {
	s.tokensMux.RLock()
	expiry, exists := s.validTokens[token]
	s.tokensMux.RUnlock()

	if !exists {
		return false
	}

	// 检查是否过期
	if time.Now().After(expiry) {
		// 同步删除过期Token（避免goroutine泄漏）
		// 原因：map删除操作非常快（O(1)），无需异步，异步反而导致goroutine泄漏
		s.tokensMux.Lock()
		delete(s.validTokens, token)
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
	for token, expiry := range s.validTokens {
		if now.After(expiry) {
			toDelete = append(toDelete, token)
		}
	}
	s.tokensMux.RUnlock()

	// 批量删除过期Token
	if len(toDelete) > 0 {
		s.tokensMux.Lock()
		for _, token := range toDelete {
			if expiry, exists := s.validTokens[token]; exists && now.After(expiry) {
				delete(s.validTokens, token)
			}
		}
		s.tokensMux.Unlock()
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
		c.JSON(http.StatusUnauthorized, gin.H{"error": "未授权访问，请先登录"})
		c.Abort()
	}
}

// RequireAPIAuth API 认证中间件（代理 API 使用）
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
			googApiKey := c.GetHeader("x-goog-api-key")
			if googApiKey != "" {
				token = googApiKey
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
		isValid := s.authTokens[tokenHash]
		s.authTokensMux.RUnlock()

		if isValid {
			// 异步更新last_used_at（不阻塞请求）
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_ = s.store.UpdateTokenLastUsed(ctx, tokenHash, time.Now())
			}()

			c.Next()
			return
		}

		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing authorization"})
		c.Abort()
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
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":           "Too many failed login attempts",
			"message":         fmt.Sprintf("Account locked for %d seconds. Please try again later.", lockoutTime),
			"lockout_seconds": lockoutTime,
		})
		return
	}

	var req struct {
		Password string `json:"password" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request format"})
		return
	}

	// 验证密码
	if req.Password != s.password {
		// 记录失败尝试（速率限制器已在AllowAttempt中增加计数）
		attemptCount := s.loginRateLimiter.GetAttemptCount(clientIP)
		util.SafePrintf("⚠️  登录失败: IP=%s, 尝试次数=%d/5", clientIP, attemptCount)

		c.JSON(http.StatusUnauthorized, gin.H{
			"error":              "Invalid password",
			"remaining_attempts": 5 - attemptCount,
		})
		return
	}

	// 密码正确，重置速率限制
	s.loginRateLimiter.RecordSuccess(clientIP)

	// 生成Token
	token := s.generateToken()

	// 存储Token到内存
	s.tokensMux.Lock()
	s.validTokens[token] = time.Now().Add(config.HoursToDuration(config.TokenExpiryHours))
	s.tokensMux.Unlock()

	util.SafePrintf("✅ 登录成功: IP=%s", clientIP)

	// 返回Token给客户端（前端存储到localStorage）
	c.JSON(http.StatusOK, gin.H{
		"status":    "success",
		"token":     token,
		"expiresIn": config.TokenExpiryHours * 3600, // 秒数
	})
}

// HandleLogout 处理登出请求
func (s *AuthService) HandleLogout(c *gin.Context) {
	// 从Authorization头提取Token
	authHeader := c.GetHeader("Authorization")
	const prefix = "Bearer "
	if after, ok := strings.CutPrefix(authHeader, prefix); ok {
		token := after

		// 删除服务器端Token
		s.tokensMux.Lock()
		delete(s.validTokens, token)
		s.tokensMux.Unlock()
	}

	c.JSON(http.StatusOK, gin.H{"status": "success", "message": "已登出"})
}

// ============================================================================
// API令牌热更新
// ============================================================================

// ReloadAuthTokens 从数据库重新加载API访问令牌
// 用于CRUD操作后立即生效，无需重启服务
func (s *AuthService) ReloadAuthTokens() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	tokens, err := s.store.ListActiveAuthTokens(ctx)
	if err != nil {
		return fmt.Errorf("reload auth tokens: %w", err)
	}

	// 构建新的令牌映射
	newTokens := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		newTokens[t.Token] = true
	}

	// 原子替换（避免读写竞争）
	s.authTokensMux.Lock()
	s.authTokens = newTokens
	s.authTokensMux.Unlock()

	util.SafePrintf("🔄 API令牌已热更新（%d个有效令牌）", len(newTokens))
	return nil
}
