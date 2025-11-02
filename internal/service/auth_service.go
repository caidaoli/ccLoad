package service

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/util"

	"github.com/gin-gonic/gin"
)

// AuthService 认证和授权服务
// 职责：处理所有认证和授权相关的业务逻辑
// - Token 认证（动态令牌）
// - API 认证（静态令牌）
// - 登录/登出处理
// - 速率限制（防暴力破解）
//
// 遵循 SRP 原则：仅负责认证授权，不涉及代理、日志、管理 API
type AuthService struct {
	// Token 认证（管理界面使用的动态 Token）
	password    string               // 管理员密码
	validTokens map[string]time.Time // Token → 过期时间
	tokensMux   sync.RWMutex         // 并发保护

	// API 认证（代理 API 使用的静态 Token）
	authTokens map[string]bool // 静态认证令牌集合

	// 速率限制（防暴力破解）
	loginRateLimiter *util.LoginRateLimiter
}

// NewAuthService 创建认证服务实例
func NewAuthService(
	password string,
	authTokens map[string]bool,
	loginRateLimiter *util.LoginRateLimiter,
) *AuthService {
	return &AuthService{
		password:         password,
		validTokens:      make(map[string]time.Time),
		authTokens:       authTokens,
		loginRateLimiter: loginRateLimiter,
	}
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
		// 优先从 Authorization 头获取Token
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

				// 检查静态Token（CCLOAD_AUTH配置的永久Token）
				if len(s.authTokens) > 0 && s.authTokens[token] {
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
		if len(s.authTokens) == 0 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or missing authorization"})
			c.Abort()
			return
		}

		// 检查 Authorization 头（Bearer token）
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" {
			const prefix = "Bearer "
			if strings.HasPrefix(authHeader, prefix) {
				token := strings.TrimPrefix(authHeader, prefix)
				if s.authTokens[token] {
					c.Next()
					return
				}
			}
		}

		// 检查 X-API-Key 头
		apiKey := c.GetHeader("X-API-Key")
		if apiKey != "" && s.authTokens[apiKey] {
			c.Next()
			return
		}

		// 检查 x-goog-api-key 头（Google API格式）
		googApiKey := c.GetHeader("x-goog-api-key")
		if googApiKey != "" && s.authTokens[googApiKey] {
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
