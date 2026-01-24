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

	_ "github.com/go-sql-driver/mysql" // MySQL driver
	_ "modernc.org/sqlite"             // SQLite driver
)

// NewStore 根据环境变量创建存储实例（工厂模式）
// 环境变量 CCLOAD_MYSQL：设置时使用MySQL，否则使用SQLite
// 环境变量 SQLITE_PATH：SQLite数据库路径（默认: data/ccload.db）
func NewStore() (Store, error) {
	var store *sqlstore.SQLStore
	var err error

	mysqlDSN := os.Getenv("CCLOAD_MYSQL")
	if mysqlDSN != "" {
		store, err = createMySQLStore(mysqlDSN)
		if err != nil {
			return nil, fmt.Errorf("MySQL 初始化失败: %w", err)
		}
		log.Printf("使用 MySQL 存储")
	} else {
		// SQLite模式：自动获取路径
		dbPath := os.Getenv("SQLITE_PATH")
		if dbPath == "" {
			dbPath = resolveSQLitePath()
		}

		store, err = createSQLiteStore(dbPath)
		if err != nil {
			return nil, fmt.Errorf("SQLite 初始化失败: %w", err)
		}
		log.Printf("使用 SQLite 存储: %s", dbPath)
	}

	return store, nil
}

// createMySQLStore 创建 MySQL 存储实例（内部函数，返回具体类型以支持生命周期方法调用）
func createMySQLStore(dsn string) (*sqlstore.SQLStore, error) {
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

	// 测试连接（带超时，Fail-Fast）
	pingCtx, pingCancel := context.WithTimeout(context.Background(), config.StartupDBPingTimeout)
	defer pingCancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("MySQL连接测试失败（超时%v）: %w", config.StartupDBPingTimeout, err)
	}

	// 创建统一的 SQLStore
	store := sqlstore.NewSQLStore(db, "mysql")

	// 执行MySQL迁移（带超时）
	migrateCtx, migrateCancel := context.WithTimeout(context.Background(), config.StartupMigrationTimeout)
	defer migrateCancel()
	if err := migrateMySQL(migrateCtx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("MySQL迁移失败（超时%v）: %w", config.StartupMigrationTimeout, err)
	}

	return store, nil
}

// CreateSQLiteStore 直接创建 SQLite 存储实例（测试辅助函数）
// 生产代码应使用 NewStore() 工厂函数
// 测试代码可用此函数创建独立的测试数据库
func CreateSQLiteStore(path string) (Store, error) {
	return createSQLiteStore(path)
}

// createSQLiteStore 内部函数，返回具体类型以支持生命周期方法调用
func createSQLiteStore(path string) (*sqlstore.SQLStore, error) {
	// 创建数据目录
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil { //nolint:gosec // G301: 数据目录需要服务进程可写
		return nil, err
	}

	// 打开SQLite数据库
	dsn := buildSQLiteDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开SQLite失败: %w", err)
	}

	// 连接池配置
	// SQLite 单进程多连接高并发写会触发 BUSY/DEADLOCK，导致冷却等事务更新不可靠。
	// 强制单连接，由 database/sql 串行化所有事务（单写者模式）。
	// 读性能：热读已被缓存层吸收（Channel/APIKey/Cooldown），影响有限。
	// 扩展路径：真有性能问题应切换 MySQL，而非在 SQLite 上堆锁。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(config.SQLiteConnMaxLifetime)

	// 创建统一的 SQLStore
	store := sqlstore.NewSQLStore(db, "sqlite")

	// 执行SQLite迁移（带超时）
	migrateCtx, migrateCancel := context.WithTimeout(context.Background(), config.StartupMigrationTimeout)
	defer migrateCancel()
	if err := migrateSQLite(migrateCtx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("SQLite迁移失败（超时%v）: %w", config.StartupMigrationTimeout, err)
	}

	return store, nil
}

// resolveSQLitePath 解析SQLite数据库路径（未设置SQLITE_PATH时调用）
// 优先使用默认路径 data/ccload.db，如果目录不可写则回退到系统临时目录
func resolveSQLitePath() string {
	defaultDir := "data"
	defaultPath := filepath.Join(defaultDir, "ccload.db")

	// 检查默认目录是否可写
	if isDirWritable(defaultDir) {
		return defaultPath
	}

	// 尝试创建目录后再检查
	if err := os.MkdirAll(defaultDir, 0o750); err == nil {
		if isDirWritable(defaultDir) {
			return defaultPath
		}
	}

	// 回退到系统临时目录
	tmpPath := filepath.Join(os.TempDir(), "ccload", "ccload.db")
	log.Printf("════════════════════════════════════════════════════════════")
	log.Printf("⚠️  警告: 默认路径 %s 不可写", defaultDir)
	log.Printf("⚠️  数据将存储在临时目录: %s", tmpPath)
	log.Printf("⚠️  临时目录数据可能在系统重启后丢失！")
	log.Printf("⚠️  生产环境请设置 SQLITE_PATH 环境变量指定持久化路径")
	log.Printf("════════════════════════════════════════════════════════════")
	return tmpPath
}

// isDirWritable 检查目录是否存在且可写
func isDirWritable(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil {
		return false // 目录不存在
	}
	if !info.IsDir() {
		return false // 不是目录
	}

	// 尝试创建临时文件来验证写权限
	testFile := filepath.Join(dir, ".write_test_"+fmt.Sprintf("%d", os.Getpid()))
	f, err := os.Create(testFile) //nolint:gosec // G304: 临时文件用于测试写权限，路径由程序控制
	if err != nil {
		return false
	}
	_ = f.Close()
	_ = os.Remove(testFile)
	return true
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
