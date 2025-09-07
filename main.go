package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
)

func main() {
	// 优先读取.env文件
	if err := godotenv.Load(); err != nil {
		log.Printf("No .env file found: %v", err)
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
	log.Printf("using sqlite store: %s", dbPath)
	var store Store = s

	// 渠道仅从 SQLite 管理与读取；不再从本地文件初始化。

	srv := NewServer(store)
	mux := http.NewServeMux()
	srv.routes(mux)
	
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
	if err := http.ListenAndServe(addr, logRequest(mux)); err != nil {
		log.Fatal(err)
	}
}
