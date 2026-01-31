// Package main 是 ccLoad 应用入口
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"ccLoad/internal/app"
	"ccLoad/internal/storage"
	"ccLoad/internal/util"
	"ccLoad/internal/version"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

// restartRequested 标记是否需要重启（由设置保存触发）
var restartRequested atomic.Bool

// RequestRestart 请求程序重启（由 admin_settings 调用）
func RequestRestart() {
	restartRequested.Store(true)
}

// execSelf 使用 syscall.Exec 重新执行自身
func execSelf() {
	executable, err := os.Executable()
	if err != nil {
		log.Printf("[ERROR] 获取可执行文件路径失败: %v", err)
		return
	}

	log.Printf("[INFO] 正在重启程序: %s", executable)

	// syscall.Exec 替换当前进程，不会返回
	//nolint:gosec // G204: executable 来自 os.Executable()，用于自重启，安全可控
	if err := syscall.Exec(executable, os.Args, os.Environ()); err != nil {
		log.Printf("[ERROR] 重启失败: %v", err)
	}
}

// defaultTrustedProxies 默认可信代理（私有网段 + 共享地址空间）
var defaultTrustedProxies = []string{
	"10.0.0.0/8",     // Class A 私有 (RFC 1918)
	"172.16.0.0/12",  // Class B 私有 (RFC 1918)
	"192.168.0.0/16", // Class C 私有 (RFC 1918)
	"100.64.0.0/10",  // 共享地址空间 (RFC 6598, 运营商级NAT/CGNAT)
	"127.0.0.0/8",    // Loopback
	"::1/128",        // IPv6 Loopback
}

// getTrustedProxies 获取可信代理配置
// 环境变量 TRUSTED_PROXIES: 逗号分隔的 CIDR，"none" 表示不信任任何代理
// 未设置时返回私有网段默认值
func getTrustedProxies() []string {
	v := os.Getenv("TRUSTED_PROXIES")
	if v == "" {
		return defaultTrustedProxies
	}
	if v == "none" {
		return nil
	}
	var proxies []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			proxies = append(proxies, p)
		}
	}
	if len(proxies) == 0 {
		return nil
	}
	return proxies
}

func main() {
	// 打印启动 Banner
	version.PrintBanner()

	// 启动后台版本检测（每4小时检查GitHub releases）
	version.StartChecker()

	// 优先读取.env文件
	if err := godotenv.Load(); err != nil {
		log.Printf("No .env file found: %v", err)
	}

	// 设置Gin运行模式
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode) // 生产模式
	}

	// 初始化嵌入的静态资源文件系统
	app.SetEmbedFS(WebFS, "web")

	// 使用工厂函数创建存储实例（自动识别MySQL/SQLite）
	store, err := storage.NewStore()
	if err != nil {
		log.Fatalf("存储初始化失败: %v", err)
	}

	// 渠道仅从数据库管理与读取；不再从本地文件初始化。

	srv := app.NewServer(store)

	// 注入重启函数（避免循环依赖）
	// 语义：标记“需要重启”，并发送 SIGTERM 触发优雅关闭；main 在退出前检测标记并 execSelf。
	app.RestartFunc = func() {
		RequestRestart()

		p, err := os.FindProcess(os.Getpid())
		if err != nil {
			log.Printf("[ERROR] Failed to find process: %v", err)
			return
		}
		if err := p.Signal(syscall.SIGTERM); err != nil {
			log.Printf("[ERROR] Failed to send SIGTERM: %v", err)
		}
	}

	// 创建Gin引擎
	r := gin.New()

	// 配置可信代理，防止 X-Forwarded-For 伪造绕过登录限速
	// TRUSTED_PROXIES 环境变量：逗号分隔的 CIDR 列表，设为 "none" 则不信任任何代理
	// 未配置时默认信任私有网段（适用于内网反向代理场景）
	// [FIX] 2025-12: 检查 SetTrustedProxies 返回值，fail-fast 避免静默的信任链缺口
	trustedProxies := getTrustedProxies()
	if trustedProxies == nil {
		if err := r.SetTrustedProxies(nil); err != nil {
			log.Fatalf("[FATAL] 设置可信代理失败: %v", err)
		}
		log.Printf("[CONFIG] 可信代理: 无 (直接暴露)")
	} else {
		if err := r.SetTrustedProxies(trustedProxies); err != nil {
			log.Fatalf("[FATAL] 设置可信代理失败: %v (配置: %v)", err, trustedProxies)
		}
		log.Printf("[CONFIG] 可信代理: %v", trustedProxies)
	}

	// 添加基础中间件
	// GIN_LOG 环境变量控制访问日志：false/0/no/off 关闭，默认开启
	if util.ParseBoolDefault(os.Getenv("GIN_LOG"), true) {
		r.Use(gin.Logger())
	}
	r.Use(gin.Recovery())

	// 注册路由
	srv.SetupRoutes(r)

	// session清理循环在NewServer中已启动，避免重复启动

	addr := ":8080"
	if v := os.Getenv("PORT"); v != "" {
		if !strings.HasPrefix(v, ":") {
			v = ":" + v
		}
		addr = v
	}

	// 使用http.Server支持优雅关闭
	// WriteTimeout 动态计算：确保 >= nonStreamTimeout，避免传输层截断业务层超时
	writeTimeout := srv.GetWriteTimeout()
	httpServer := &http.Server{
		Addr:    addr,
		Handler: r,

		// ✅ 深度防御：传输层超时保护（抵御slowloris等慢速攻击）
		// 即使绕过应用层并发控制，也会在HTTP层被杀死
		ReadHeaderTimeout: 5 * time.Second,   // 防止慢速发送header（slowloris攻击）
		ReadTimeout:       120 * time.Second, // 防止慢速发送body（兼容长请求）
		WriteTimeout:      writeTimeout,      // 动态值，>= nonStreamTimeout
		IdleTimeout:       60 * time.Second,  // 防止keep-alive连接占用fd
	}
	log.Printf("[CONFIG] HTTP WriteTimeout: %v", writeTimeout)

	// 启动HTTP服务器（在goroutine中）
	go func() {
		log.Printf("listening on %s", addr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP服务器启动失败: %v", err)
		}
	}()

	// 监听系统信号，实现优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	// ✅ 停止信号监听,释放signal.Notify创建的后台goroutine
	signal.Stop(quit)
	close(quit)

	log.Println("收到关闭信号，正在优雅关闭服务器...")

	// 设置5秒超时用于HTTP服务器关闭
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 关闭HTTP服务器
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP服务器关闭超时: %v，强制关闭连接", err)
		// 超时后强制关闭，防止streaming连接阻塞退出
		_ = httpServer.Close()
	}

	// 关闭Server后台任务（设置10秒超时）
	taskShutdownCtx, taskCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer taskCancel()

	if err := srv.Shutdown(taskShutdownCtx); err != nil {
		log.Printf("Server后台任务关闭错误: %v", err)
	}

	log.Println("✅ 服务器已优雅关闭")

	// 检查是否需要重启
	if restartRequested.Load() {
		log.Println("🔄 检测到重启请求，正在重启...")
		execSelf()
		// execSelf 不会返回，如果到这里说明重启失败
		log.Println("[ERROR] 重启失败，程序退出")
	}
}
