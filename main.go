package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
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

	// ========== 性能优化：启动时预热（阶段1优化）==========

	// HTTP连接预热（消除首次请求TLS握手10-50ms）
	srv.WarmHTTPConnections(ctx)

	// 等待连接预热完成（最多100ms）
	time.Sleep(100 * time.Millisecond)

	log.Printf("✅ 性能优化启动完成")

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
	log.Printf("listening on %s", addr)
	if err := r.Run(addr); err != nil {
		log.Fatal(err)
	}
}
