package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"ccLoad/internal/config"
	sqlstore "ccLoad/internal/storage/sql"

	_ "github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"
)

// RedisSync Redis同步接口（与sql.RedisSync保持一致）
type RedisSync = sqlstore.RedisSync

// NewStore 根据环境变量创建存储实例（工厂模式）
// 环境变量 CCLOAD_MYSQL：设置时使用MySQL，否则使用SQLite
// 环境变量 SQLITE_PATH：SQLite数据库路径（默认: data/ccload.db）
//
// 生产代码应使用此函数，测试代码可使用 CreateSQLiteStore() 直接创建
func NewStore(redisSync RedisSync) (Store, error) {
	mysqlDSN := os.Getenv("CCLOAD_MYSQL")
	if mysqlDSN != "" {
		store, err := createMySQLStore(mysqlDSN, redisSync)
		if err != nil {
			return nil, fmt.Errorf("MySQL 初始化失败: %w", err)
		}
		log.Printf("使用 MySQL 存储")
		return store, nil
	}

	// SQLite模式：自动获取路径
	dbPath := os.Getenv("SQLITE_PATH")
	if dbPath == "" {
		dbPath = filepath.Join("data", "ccload.db")
	}

	store, err := CreateSQLiteStore(dbPath, redisSync)
	if err != nil {
		return nil, fmt.Errorf("SQLite 初始化失败: %w", err)
	}
	log.Printf("使用 SQLite 存储: %s", dbPath)
	return store, nil
}

// createMySQLStore 创建 MySQL 存储实例（使用统一的 SQLStore）
func createMySQLStore(dsn string, redisSync RedisSync) (Store, error) {
	// 确保DSN包含必要参数
	if dsn == "" {
		return nil, fmt.Errorf("MySQL DSN不能为空")
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开MySQL连接失败: %w", err)
	}

	// 连接池配置
	db.SetMaxOpenConns(config.SQLiteMaxOpenConnsFile * 2) // MySQL可以更高并发
	db.SetMaxIdleConns(config.SQLiteMaxIdleConnsFile * 2)
	db.SetConnMaxLifetime(config.SQLiteConnMaxLifetime)

	// 测试连接
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("MySQL连接测试失败: %w", err)
	}

	// 创建统一的 SQLStore（不需要方言抽象）
	store := sqlstore.NewSQLStore(db, redisSync)

	// 执行MySQL迁移
	if err := migrateMySQL(context.Background(), db); err != nil {
		db.Close()
		return nil, fmt.Errorf("MySQL迁移失败: %w", err)
	}

	return store, nil
}

// CreateSQLiteStore 直接创建 SQLite 存储实例（测试辅助函数）
// 生产代码应使用 NewStore() 工厂函数
// 测试代码可用此函数创建独立的测试数据库
func CreateSQLiteStore(path string, redisSync RedisSync) (Store, error) {
	// 创建数据目录
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	// 打开SQLite数据库
	dsn := buildSQLiteDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开SQLite失败: %w", err)
	}

	// 连接池配置
	db.SetMaxOpenConns(config.SQLiteMaxOpenConnsFile)
	db.SetMaxIdleConns(config.SQLiteMaxIdleConnsFile)
	db.SetConnMaxLifetime(config.SQLiteConnMaxLifetime)

	// 创建统一的 SQLStore（不需要方言抽象）
	store := sqlstore.NewSQLStore(db, redisSync)

	// 执行SQLite迁移
	if err := migrateSQLite(context.Background(), db); err != nil {
		db.Close()
		return nil, fmt.Errorf("SQLite迁移失败: %w", err)
	}

	return store, nil
}

// buildSQLiteDSN 构建SQLite DSN
func buildSQLiteDSN(path string) string {
	journalMode := validateJournalMode(os.Getenv("SQLITE_JOURNAL_MODE"))
	return fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_foreign_keys=on&_pragma=journal_mode=%s&_loc=Local", path, journalMode)
}

// validateJournalMode 验证SQLITE_JOURNAL_MODE环境变量的合法性（白名单）
func validateJournalMode(mode string) string {
	if mode == "" {
		return "WAL" // 默认安全值
	}

	validModes := map[string]bool{
		"DELETE":   true,
		"TRUNCATE": true,
		"PERSIST":  true,
		"MEMORY":   true,
		"WAL":      true,
		"OFF":      true,
	}

	modeUpper := strings.ToUpper(mode)
	if !validModes[modeUpper] {
		log.Fatalf("❌ 安全错误: SQLITE_JOURNAL_MODE 环境变量值非法: %q\n"+
			"   允许的值: DELETE, TRUNCATE, PERSIST, MEMORY, WAL, OFF\n"+
			"   当前值: %q\n"+
			"   修复方法:\n"+
			"     - 设置合法值: export SQLITE_JOURNAL_MODE=WAL\n"+
			"     - 或者移除该环境变量，使用默认值 WAL",
			mode, mode)
	}

	return modeUpper
}
