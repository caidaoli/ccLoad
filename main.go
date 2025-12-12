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
	"ccLoad/internal/storage/redis"

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
	if err := syscall.Exec(executable, os.Args, os.Environ()); err != nil {
		log.Printf("[ERROR] 重启失败: %v", err)
	}
}

func main() {
	// 优先读取.env文件
	if err := godotenv.Load(); err != nil {
		log.Printf("No .env file found: %v", err)
	}

	// 设置Gin运行模式
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode) // 生产模式
	}

	// 初始化Redis同步客户端 (可选功能)
	redisURL := os.Getenv("REDIS_URL")
	redisSync, err := redis.NewRedisSync(redisURL)
	if err != nil {
		log.Fatalf("Redis初始化失败: %v", err)
	}
	defer redisSync.Close()

	if redisSync.IsEnabled() {
		log.Printf("Redis同步已启用")
	} else {
		log.Printf("Redis同步未配置")
	}

	// 使用工厂函数创建存储实例（自动识别MySQL/SQLite）
	ctx := context.Background()
	store, err := storage.NewStore(redisSync)
	if err != nil {
		log.Fatalf("存储初始化失败: %v", err)
	}

	// 统一的Redis恢复逻辑（SQLite和MySQL共用）
	if redisSync.IsEnabled() {
		isEmpty, err := store.CheckChannelsEmpty(ctx)
		if err != nil {
			log.Printf("检查数据库状态失败: %v", err)
		} else if isEmpty {
			log.Printf("数据库为空，尝试从Redis恢复数据...")
			if err := store.LoadChannelsFromRedis(ctx); err != nil {
				log.Printf("从Redis恢复失败: %v", err)
			}
		}
	}

	// 启动 Redis 同步 worker（迁移+恢复完成后）
	// 必须在恢复逻辑之后调用，避免空数据覆盖 Redis 备份
	store.StartRedisSync()

	// 渠道仅从数据库管理与读取；不再从本地文件初始化。

	srv := app.NewServer(store)

	// 注入重启函数（避免循环依赖）
	app.RestartFunc = RequestRestart

	// 创建Gin引擎
	r := gin.New()

	// 添加基础中间件
	r.Use(gin.Logger())
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
	httpServer := &http.Server{
		Addr:    addr,
		Handler: r,

		// ✅ 深度防御：传输层超时保护（抵御slowloris等慢速攻击）
		// 即使绕过应用层并发控制，也会在HTTP层被杀死
		ReadHeaderTimeout: 5 * time.Second,   // 防止慢速发送header（slowloris攻击）
		ReadTimeout:       120 * time.Second, // 防止慢速发送body（兼容长请求）
		WriteTimeout:      120 * time.Second, // 防止慢速读取响应（兼容流式输出）
		IdleTimeout:       60 * time.Second,  // 防止keep-alive连接占用fd
	}

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
