package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"ccLoad/internal/app"
	"ccLoad/internal/storage"
	"ccLoad/internal/storage/redis"
	"ccLoad/internal/storage/sqlite"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

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
		log.Printf("Redis同步未配置，使用纯SQLite模式")
	}

	// 优先使用 SQLite 存储
	dbPath := os.Getenv("SQLITE_PATH")
	if dbPath == "" {
		dbPath = filepath.Join("data", "ccload.db")
	}

	// 检查数据库文件是否存在 (启动恢复机制的关键判断)
	dbExists := sqlite.CheckDatabaseExists(dbPath)

	s, err := sqlite.NewSQLiteStore(dbPath, redisSync)
	if err != nil {
		log.Fatalf("sqlite 初始化失败: %v", err)
	}

	// 启动时数据恢复逻辑 (KISS原则: 简单的恢复策略)
	ctx := context.Background()
	if !dbExists && redisSync.IsEnabled() {
		log.Printf("数据库文件不存在，尝试从Redis恢复渠道数据...")
		if err := s.LoadChannelsFromRedis(ctx); err != nil {
			log.Printf("从Redis恢复失败: %v", err)
		}
	}
	log.Printf("using sqlite store: %s", dbPath)
	var store storage.Store = s

	// 渠道仅从 SQLite 管理与读取；不再从本地文件初始化。

	srv := app.NewServer(store)

	// ========== 性能优化：启动时预热（可选）==========
	if v := os.Getenv("CCLOAD_ENABLE_WARMUP"); v == "1" || strings.EqualFold(v, "true") {
		// HTTP连接预热（消除首次请求TLS握手10-50ms）
		srv.WarmHTTPConnections(ctx)
		log.Printf("✅ 启动预热已完成")
	}

	// ========== 性能优化结束 ==========

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

	log.Println("收到关闭信号，正在优雅关闭服务器...")

	// 设置5秒超时用于HTTP服务器关闭
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 关闭HTTP服务器
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP服务器关闭错误: %v", err)
	}

	// 关闭Server后台任务（设置10秒超时）
	taskShutdownCtx, taskCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer taskCancel()

	if err := srv.Shutdown(taskShutdownCtx); err != nil {
		log.Printf("Server后台任务关闭错误: %v", err)
	}

	log.Println("✅ 服务器已优雅关闭")
}
