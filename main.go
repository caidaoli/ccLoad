package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
)

func main() {
	// 优先读取.env文件
	if err := godotenv.Load(); err != nil {
		log.Printf("No .env file found: %v", err)
	}

	// 检查是否为Redis测试模式
	if len(os.Args) > 1 && os.Args[1] == "test-redis" {
		testRedisSync()
		return
	}

	// 设置Gin运行模式
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode) // 生产模式
	}

	// 初始化Redis同步客户端 (可选功能)
	redisURL := os.Getenv("REDIS_URL")
	redisSync, err := NewRedisSync(redisURL)
	if err != nil {
		log.Fatalf("Redis初始化失败: %v", err)
	}
	defer redisSync.Close()

	if redisSync.IsEnabled() {
		log.Printf("Redis同步已启用: %s", redisURL)
	} else {
		log.Printf("Redis同步未配置，使用纯SQLite模式")
	}

	// 优先使用 SQLite 存储
	dbPath := os.Getenv("SQLITE_PATH")
	if dbPath == "" {
		dbPath = filepath.Join("data", "ccload.db")
	}

	// 检查数据库文件是否存在 (启动恢复机制的关键判断)
	dbExists := CheckDatabaseExists(dbPath)

	s, err := NewSQLiteStore(dbPath, redisSync)
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
	} else if dbExists && redisSync.IsEnabled() {
		// 数据库存在时，同步当前数据到Redis
		log.Printf("同步现有渠道数据到Redis...")
		if err := s.SyncAllChannelsToRedis(ctx); err != nil {
			log.Printf("同步到Redis失败: %v", err)
		}
	}

	if err := s.Vacuum(ctx); err != nil {
		log.Printf("VACUUM 执行失败: %v", err)
	}
	log.Printf("using sqlite store: %s", dbPath)
	var store Store = s

	// 渠道仅从 SQLite 管理与读取；不再从本地文件初始化。

	srv := NewServer(store)

	// 创建Gin引擎
	r := gin.New()

	// 添加基础中间件
	r.Use(gin.Logger())
	r.Use(gin.Recovery())

	// 注册路由
	srv.setupRoutes(r)

	// 启动 session 清理循环
	go srv.sessionCleanupLoop()

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

// testRedisSync 测试Redis同步功能
func testRedisSync() {
	log.Println("=== Redis同步功能测试 ===")

	// 测试无Redis URL的情况
	log.Println("\n1. 测试无Redis配置的情况:")
	redisSync1, err := NewRedisSync("")
	if err != nil {
		log.Fatalf("NewRedisSync('')应该成功: %v", err)
	}
	log.Printf("   Redis启用状态: %v (应该为false)", redisSync1.IsEnabled())

	// 测试无效Redis URL
	log.Println("\n2. 测试无效Redis URL:")
	_, err = NewRedisSync("invalid-url")
	if err == nil {
		log.Fatal("NewRedisSync('invalid-url')应该失败")
	}
	log.Printf("   预期错误: %v", err)

	// 如果有REDIS_URL环境变量，测试真实连接
	redisURL := os.Getenv("REDIS_URL")
	if redisURL == "" {
		log.Println("\n3. 跳过真实Redis测试 (未设置REDIS_URL环境变量)")
		log.Println("   要测试真实Redis连接，请设置REDIS_URL环境变量")
		log.Println("   例如: export REDIS_URL='redis://localhost:6379'")
		return
	}

	log.Printf("\n3. 测试真实Redis连接: %s", redisURL)
	redisSync, err := NewRedisSync(redisURL)
	if err != nil {
		log.Printf("   Redis连接失败: %v", err)
		log.Println("   这是正常的，如果Redis服务器未运行")
		return
	}
	defer redisSync.Close()

	ctx := context.Background()

	// 测试健康检查
	log.Println("   a) 健康检查:")
	if err := redisSync.HealthCheck(ctx); err != nil {
		log.Printf("      失败: %v", err)
		return
	}
	log.Println("      成功")

	// 测试渠道同步
	log.Println("   b) 测试渠道同步:")
	testConfig := &Config{
		ID:       1,
		Name:     "test-channel",
		APIKey:   "sk-test-key",
		URL:      "https://api.test.com",
		Priority: 10,
		Models:   []string{"claude-3-sonnet-20240229"},
		Enabled:  true,
	}

	// 创建同步
	if err := redisSync.SyncChannelCreate(ctx, testConfig); err != nil {
		log.Printf("      创建同步失败: %v", err)
		return
	}
	log.Println("      创建同步成功")

	// 验证数据
	configs, err := redisSync.LoadChannelsFromRedis(ctx)
	if err != nil {
		log.Printf("      加载失败: %v", err)
		return
	}

	if len(configs) == 0 {
		log.Println("      错误: 未找到同步的渠道")
		return
	}

	found := false
	for _, config := range configs {
		if config.Name == "test-channel" {
			found = true
			log.Printf("      找到渠道: %s (模型: %v)", config.Name, config.Models)
			break
		}
	}

	if !found {
		log.Println("      错误: 未找到test-channel")
		return
	}

	// 更新同步
	testConfig.Priority = 20
	if err := redisSync.SyncChannelUpdate(ctx, testConfig); err != nil {
		log.Printf("      更新同步失败: %v", err)
		return
	}
	log.Println("      更新同步成功")

	// 删除同步
	if err := redisSync.SyncChannelDelete(ctx, "test-channel"); err != nil {
		log.Printf("      删除同步失败: %v", err)
		return
	}
	log.Println("      删除同步成功")

	// 验证删除
	count, err := redisSync.GetChannelCount(ctx)
	if err != nil {
		log.Printf("      获取数量失败: %v", err)
		return
	}
	log.Printf("      Redis中渠道数量: %d", count)

	log.Println("\n=== Redis同步功能测试完成 ===")
}
