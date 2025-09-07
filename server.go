package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

type Server struct {
	store    Store
	client   *http.Client
	password string
	sessions map[string]time.Time // sessionID -> expireTime
	sessMux  sync.RWMutex
}

func NewServer(store Store) *Server {
	password := os.Getenv("CCLOAD_PASS")
	if password == "" {
		password = "admin" // 默认密码，生产环境应该设置环境变量
	}
	return &Server{
		store:    store,
		client:   &http.Client{Timeout: 0},
		password: password,
		sessions: make(map[string]time.Time),
	}
}

// helper: write JSON
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func parseInt64Param(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

// 生成随机session ID
func (s *Server) generateSessionID() string {
	bytes := make([]byte, 32)
	rand.Read(bytes)
	return hex.EncodeToString(bytes)
}

// 创建session
func (s *Server) createSession() string {
	s.sessMux.Lock()
	defer s.sessMux.Unlock()

	sessionID := s.generateSessionID()
	// session有效期24小时
	s.sessions[sessionID] = time.Now().Add(24 * time.Hour)
	return sessionID
}

// 验证session
func (s *Server) validateSession(sessionID string) bool {
	s.sessMux.RLock()
	defer s.sessMux.RUnlock()

	expireTime, exists := s.sessions[sessionID]
	if !exists {
		return false
	}

	if time.Now().After(expireTime) {
		// session已过期，删除它
		s.sessMux.RUnlock()
		s.sessMux.Lock()
		delete(s.sessions, sessionID)
		s.sessMux.Unlock()
		s.sessMux.RLock()
		return false
	}

	return true
}

// 清理过期session
func (s *Server) cleanExpiredSessions() {
	s.sessMux.Lock()
	defer s.sessMux.Unlock()

	now := time.Now()
	for sessionID, expireTime := range s.sessions {
		if now.After(expireTime) {
			delete(s.sessions, sessionID)
		}
	}
}

// 身份验证中间件
func (s *Server) requireAuth(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 检查cookie中的session
		cookie, err := r.Cookie("ccload_session")
		if err != nil || !s.validateSession(cookie.Value) {
			// 未登录，重定向到登录页面
			loginURL := "/web/login.html?redirect=" + r.URL.Path
			if r.URL.RawQuery != "" {
				loginURL += "%3F" + r.URL.RawQuery // 编码查询参数
			}
			http.Redirect(w, r, loginURL, http.StatusFound)
			return
		}
		handler(w, r)
	}
}

// 登录处理程序
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Password string `json:"password"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request format"})
		return
	}

	if req.Password != s.password {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Invalid password"})
		return
	}

	// 密码正确，创建session
	sessionID := s.createSession()

	// 设置cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "ccload_session",
		Value:    sessionID,
		Path:     "/",
		MaxAge:   24 * 60 * 60, // 24小时
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})

	writeJSON(w, http.StatusOK, map[string]string{"status": "success"})
}

// 登出处理程序
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// 清除cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "ccload_session",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})

	// 清除服务器端session
	cookie, err := r.Cookie("ccload_session")
	if err == nil {
		s.sessMux.Lock()
		delete(s.sessions, cookie.Value)
		s.sessMux.Unlock()
	}

	http.Redirect(w, r, "/web/login.html", http.StatusFound)
}

// routes
func (s *Server) routes(mux *http.ServeMux) {
	// 公开访问的API（代理服务）
	mux.HandleFunc("/v1/messages", s.handleMessages)

	// 公开访问的API（基础统计）
	mux.HandleFunc("/public/summary", s.handlePublicSummary)

	// 登录相关（公开访问）
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.handleLogout)

	// 需要身份验证的admin APIs
	mux.HandleFunc("/admin/channels", s.requireAuth(s.handleChannels))
	mux.HandleFunc("/admin/channels/", s.requireAuth(s.handleChannelByID))
	mux.HandleFunc("/admin/errors", s.requireAuth(s.handleErrors))
	mux.HandleFunc("/admin/metrics", s.requireAuth(s.handleMetrics))
	mux.HandleFunc("/admin/stats", s.requireAuth(s.handleStats))

	// 静态文件服务（需要验证的页面会通过中间件处理）
	mux.HandleFunc("/web/", s.handleWebFiles)

	// 默认首页重定向
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/web/index.html", http.StatusFound)
	})

	// 启动session清理goroutine
	go s.sessionCleanupLoop()
}

// 处理web静态文件，对管理页面进行身份验证
func (s *Server) handleWebFiles(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// 需要身份验证的页面
	authRequiredPages := []string{
		"/web/channels.html",
		"/web/errors.html",
		"/web/stats.html",
	}

	needsAuth := false
	for _, page := range authRequiredPages {
		if path == page {
			needsAuth = true
			break
		}
	}

	if needsAuth {
		// 检查身份验证
		cookie, err := r.Cookie("ccload_session")
		if err != nil || !s.validateSession(cookie.Value) {
			loginURL := "/web/login.html?redirect=" + r.URL.Path
			if r.URL.RawQuery != "" {
				loginURL += "%3F" + r.URL.RawQuery
			}
			http.Redirect(w, r, loginURL, http.StatusFound)
			return
		}
	}

	// 提供静态文件服务
	fs := http.FileServer(http.Dir("web"))
	http.StripPrefix("/web/", fs).ServeHTTP(w, r)
}

// session清理循环
func (s *Server) sessionCleanupLoop() {
	ticker := time.NewTicker(1 * time.Hour) // 每小时清理一次
	defer ticker.Stop()

	for range ticker.C {
		s.cleanExpiredSessions()
	}
}
