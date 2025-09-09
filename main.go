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

	// 设置Gin运行模式
	if os.Getenv("GIN_MODE") == "" {
		gin.SetMode(gin.ReleaseMode) // 生产模式
	}

	// 优先使用 SQLite 存储
	dbPath := os.Getenv("SQLITE_PATH")
	if dbPath == "" {
		dbPath = filepath.Join("data", "ccload.db")
	}
	s, err := NewSQLiteStore(dbPath)
	if err != nil {
		log.Fatalf("sqlite 初始化失败: %v", err)
	}
	if err := s.Vacuum(context.Background()); err != nil {
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
