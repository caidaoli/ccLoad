package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/config"
	sqlstore "ccLoad/internal/storage/sql"

	_ "github.com/go-sql-driver/mysql" // MySQL driver
	_ "github.com/jackc/pgx/v5/stdlib" // PostgreSQL driver (name: pgx)
	_ "modernc.org/sqlite"             // SQLite driver
)

// NewStore 根据环境变量创建存储实例（工厂模式）
//
// 模式：
//   - 纯 SQLite：未设置主库 DSN（默认）
//   - 纯 MySQL：仅 CCLOAD_MYSQL
//   - 纯 PostgreSQL：仅 CCLOAD_POSTGRES
//   - 混合（主库 + SQLite 缓存）：主库 DSN + CCLOAD_ENABLE_SQLITE_REPLICA=1
//
// 环境变量：
//   - CCLOAD_MYSQL：MySQL DSN（与 CCLOAD_POSTGRES 互斥）
//   - CCLOAD_POSTGRES：PostgreSQL DSN（URL 或 libpq 关键字串，与 CCLOAD_MYSQL 互斥）
//   - CCLOAD_ENABLE_SQLITE_REPLICA：混合模式开关（1=启用）
//   - SQLITE_PATH：SQLite 数据库路径（默认: data/ccload.db）
//   - CCLOAD_SQLITE_LOG_DAYS：日志恢复天数（默认 7 天，0=不恢复日志，-1=全量）
func NewStore() (Store, error) {
	mysqlDSN := strings.TrimSpace(os.Getenv("CCLOAD_MYSQL"))
	pgDSN := strings.TrimSpace(os.Getenv("CCLOAD_POSTGRES"))

	if mysqlDSN != "" && pgDSN != "" {
		log.Fatal("[FATAL] CCLOAD_MYSQL 与 CCLOAD_POSTGRES 互斥，请只设置其中一个主库 DSN")
	}

	// 场景 1：纯 SQLite 模式（默认）
	if mysqlDSN == "" && pgDSN == "" {
		dbPath := os.Getenv("SQLITE_PATH")
		if dbPath == "" {
			dbPath = resolveSQLitePath()
		}

		store, err := createSQLiteStore(dbPath)
		if err != nil {
			return nil, fmt.Errorf("SQLite 初始化失败: %w", err)
		}
		log.Printf("使用 SQLite 存储（纯模式）: %s", dbPath)
		return store, nil
	}

	enableHybrid := os.Getenv("CCLOAD_ENABLE_SQLITE_REPLICA") == "1"
	primaryName := "PostgreSQL"
	if mysqlDSN != "" {
		primaryName = "MySQL"
	}

	if !enableHybrid {
		var primary *sqlstore.SQLStore
		var err error
		if mysqlDSN != "" {
			primary, err = createMySQLStore(mysqlDSN)
		} else {
			primary, err = createPostgresStore(pgDSN)
		}
		if err != nil {
			return nil, fmt.Errorf("%s 初始化失败: %w", primaryName, err)
		}
		log.Printf("使用 %s 存储（纯模式）", primaryName)
		return primary, nil
	}

	// 混合模式（SQLite 权威库 + 主库异步副本）
	log.Printf("[INFO] 启动混合存储模式（SQLite 权威库 + %s 异步副本）", primaryName)

	sqlitePath := os.Getenv("SQLITE_PATH")
	if sqlitePath == "" {
		sqlitePath = resolveSQLitePath()
	}
	sqliteExisted := sqliteFileHasData(sqlitePath)
	sqlite, err := createSQLiteStore(sqlitePath)
	if err != nil {
		return nil, fmt.Errorf("SQLite 初始化失败: %w", err)
	}
	log.Printf("[INFO] SQLite 权威库已打开: %s", sqlitePath)

	bootstrapCtx, bootstrapCancel := context.WithTimeout(context.Background(), config.StartupMigrationTimeout)
	needsImport, err := prepareHybridBootstrap(bootstrapCtx, sqlite, sqliteExisted)
	bootstrapCancel()
	if err != nil {
		_ = sqlite.Close()
		return nil, fmt.Errorf("SQLite 混合模式状态初始化失败: %w", err)
	}

	logDays := getLogSyncDays()
	var primary *sqlstore.SQLStore
	var initializePrimary primaryStoreInitializer
	if needsImport {
		if mysqlDSN != "" {
			primary, err = createMySQLStore(mysqlDSN)
		} else {
			primary, err = createPostgresStore(pgDSN)
		}
		if err != nil {
			_ = sqlite.Close()
			return nil, fmt.Errorf("%s 初始化失败: %w", primaryName, err)
		}
		syncMgr := NewSyncManager(primary, sqlite)
		restoreCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		if err := syncMgr.RestoreOnStartup(restoreCtx, logDays); err != nil {
			_ = sqlite.Close()
			_ = primary.Close()
			return nil, fmt.Errorf("首次数据导入失败: %w", err)
		}
		if err := completeHybridBootstrap(restoreCtx, sqlite); err != nil {
			_ = sqlite.Close()
			_ = primary.Close()
			return nil, fmt.Errorf("记录首次数据导入状态失败: %w", err)
		}
		log.Printf("[INFO] SQLite 首次初始化已从 %s 导入", primaryName)
	} else {
		if mysqlDSN != "" {
			primary, initializePrimary, err = openMySQLStore(mysqlDSN)
		} else {
			primary, initializePrimary, err = openPostgresStore(pgDSN)
		}
		if err != nil {
			_ = sqlite.Close()
			return nil, fmt.Errorf("打开 %s 异步副本失败: %w", primaryName, err)
		}
		log.Printf("[INFO] 保留现有 SQLite 数据；%s 连接与迁移转入后台", primaryName)
	}

	hybrid := newHybridStore(sqlite, primary, initializePrimary)
	log.Printf("[INFO] 混合存储已启用（主库副本=%s, 首次日志导入天数: %d）", primaryName, logDays)
	return hybrid, nil
}

const (
	hybridBootstrapPending  = "pending"
	hybridBootstrapComplete = "complete"
)

// prepareHybridBootstrap returns true until a fresh SQLite import has completed.
// Existing databases from before the marker was introduced are treated as authoritative.
func prepareHybridBootstrap(ctx context.Context, sqlite *sqlstore.SQLStore, fileExisted bool) (bool, error) {
	var markerTableExisted bool
	var tableCount int
	if err := sqlite.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'hybrid_bootstrap_state'
	`).Scan(&tableCount); err != nil {
		return false, err
	}
	markerTableExisted = tableCount > 0

	var needsImport bool
	err := sqlite.WithTransaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS hybrid_bootstrap_state (
				id INTEGER PRIMARY KEY,
				status VARCHAR(16) NOT NULL,
				updated_at BIGINT NOT NULL
			)
		`); err != nil {
			return err
		}

		var status string
		err := tx.QueryRowContext(ctx, `SELECT status FROM hybrid_bootstrap_state WHERE id = 1`).Scan(&status)
		if err == nil {
			switch status {
			case hybridBootstrapComplete:
				needsImport = false
				return nil
			case hybridBootstrapPending:
				needsImport = true
				return nil
			default:
				return fmt.Errorf("unknown hybrid bootstrap status %q", status)
			}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		// Only an old database that never had the marker is adopted as complete.
		// An existing but empty marker table means a previous bootstrap was interrupted.
		status = hybridBootstrapPending
		needsImport = true
		if fileExisted && !markerTableExisted {
			status = hybridBootstrapComplete
			needsImport = false
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO hybrid_bootstrap_state(id, status, updated_at) VALUES(1, ?, ?)
		`, status, time.Now().Unix())
		return err
	})
	return needsImport, err
}

func completeHybridBootstrap(ctx context.Context, sqlite *sqlstore.SQLStore) error {
	_, err := sqlite.ExecContext(ctx, `
		UPDATE hybrid_bootstrap_state SET status = ?, updated_at = ? WHERE id = 1
	`, hybridBootstrapComplete, time.Now().Unix())
	return err
}

func sqliteFileHasData(path string) bool {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file::memory:") {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

type primaryStoreInitializer func(context.Context) error

// openMySQLStore only validates the DSN and creates the lazy connection pool.
// Ping and migration are returned separately so an initialized hybrid SQLite can start offline.
func openMySQLStore(dsn string) (*sqlstore.SQLStore, primaryStoreInitializer, error) {
	if dsn == "" {
		return nil, nil, fmt.Errorf("MySQL DSN不能为空")
	}

	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("打开MySQL连接失败: %w", err)
	}

	// 连接池配置
	db.SetMaxOpenConns(config.SQLiteMaxOpenConnsFile * 2) // MySQL可以更高并发
	db.SetMaxIdleConns(config.SQLiteMaxIdleConnsFile * 2)
	db.SetConnMaxLifetime(config.SQLiteConnMaxLifetime)

	store := sqlstore.NewSQLStore(db, "mysql")
	initialize := func(ctx context.Context) error {
		pingCtx, pingCancel := context.WithTimeout(ctx, config.StartupDBPingTimeout)
		err := db.PingContext(pingCtx)
		pingCancel()
		if err != nil {
			return fmt.Errorf("MySQL连接测试失败（超时%v）: %w", config.StartupDBPingTimeout, err)
		}
		migrateCtx, migrateCancel := context.WithTimeout(ctx, config.StartupMigrationTimeout)
		err = migrateMySQL(migrateCtx, db)
		migrateCancel()
		if err != nil {
			return fmt.Errorf("MySQL迁移失败（超时%v）: %w", config.StartupMigrationTimeout, err)
		}
		return nil
	}
	return store, initialize, nil
}

// createMySQLStore creates and fully initializes a primary-only or bootstrap store.
func createMySQLStore(dsn string) (*sqlstore.SQLStore, error) {
	store, initialize, err := openMySQLStore(dsn)
	if err != nil {
		return nil, err
	}
	if err := initialize(context.Background()); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

func openPostgresStore(dsn string) (*sqlstore.SQLStore, primaryStoreInitializer, error) {
	if dsn == "" {
		return nil, nil, fmt.Errorf("PostgreSQL DSN不能为空")
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("打开PostgreSQL连接失败: %w", err)
	}

	db.SetMaxOpenConns(config.SQLiteMaxOpenConnsFile * 2)
	db.SetMaxIdleConns(config.SQLiteMaxIdleConnsFile * 2)
	db.SetConnMaxLifetime(config.SQLiteConnMaxLifetime)

	store := sqlstore.NewSQLStore(db, "postgres")
	initialize := func(ctx context.Context) error {
		pingCtx, pingCancel := context.WithTimeout(ctx, config.StartupDBPingTimeout)
		err := db.PingContext(pingCtx)
		pingCancel()
		if err != nil {
			return fmt.Errorf("PostgreSQL连接测试失败（超时%v）: %w", config.StartupDBPingTimeout, err)
		}
		migrateCtx, migrateCancel := context.WithTimeout(ctx, config.StartupMigrationTimeout)
		err = migratePostgres(migrateCtx, db)
		migrateCancel()
		if err != nil {
			return fmt.Errorf("PostgreSQL迁移失败（超时%v）: %w", config.StartupMigrationTimeout, err)
		}
		return nil
	}
	return store, initialize, nil
}

// createPostgresStore 创建并完成 PostgreSQL 初始化。
func createPostgresStore(dsn string) (*sqlstore.SQLStore, error) {
	store, initialize, err := openPostgresStore(dsn)
	if err != nil {
		return nil, err
	}
	if err := initialize(context.Background()); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

// CreatePostgresStoreForTest 直接创建 PostgreSQL 存储实例（测试辅助）
func CreatePostgresStoreForTest(dsn string) (Store, error) {
	s, err := createPostgresStore(dsn)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// CreateSQLiteStore 直接创建 SQLite 存储实例（测试辅助函数）
// 生产代码应使用 NewStore() 工厂函数
// 测试代码可用此函数创建独立的测试数据库
func CreateSQLiteStore(path string) (Store, error) {
	s, err := createSQLiteStore(path)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// CreateMySQLStoreForTest 直接创建 MySQL 存储实例（测试/Benchmark 辅助函数）
// 生产代码应使用 NewStore() 工厂函数
// 测试代码可用此函数创建独立的 MySQL 连接进行性能对比
func CreateMySQLStoreForTest(dsn string) (Store, error) {
	s, err := createMySQLStore(dsn)
	if err != nil {
		return nil, err
	}
	return s, nil
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

	if _, err := db.ExecContext(migrateCtx, "PRAGMA optimize"); err != nil {
		log.Printf("[WARN] SQLite PRAGMA optimize 失败: %v", err)
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
	log.Printf("[WARN] 警告: 默认路径 %s 不可写", defaultDir)
	log.Printf("[WARN] 数据将存储在临时目录: %s", tmpPath)
	log.Printf("[WARN] 临时目录数据可能在系统重启后丢失！")
	log.Printf("[WARN] 生产环境请设置 SQLITE_PATH 环境变量指定持久化路径")
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
	return fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode=%s&_pragma=wal_autocheckpoint(500)&_loc=Local", path, journalMode)
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
		log.Fatalf("[FATAL] 安全错误: SQLITE_JOURNAL_MODE 环境变量值非法: %q\n"+
			"   允许的值: DELETE, TRUNCATE, PERSIST, MEMORY, WAL, OFF\n"+
			"   当前值: %q\n"+
			"   修复方法:\n"+
			"     - 设置合法值: export SQLITE_JOURNAL_MODE=WAL\n"+
			"     - 或者移除该环境变量，使用默认值 WAL",
			mode, mode)
	}

	return modeUpper
}

// getLogSyncDays 获取日志同步天数配置
// 环境变量 CCLOAD_SQLITE_LOG_DAYS：
//   - -1 = 全量恢复（慎用，启动慢）
//   - 0 = 仅恢复配置表，不恢复日志
//   - 7 = 恢复配置表 + 最近 7 天日志（默认）
func getLogSyncDays() int {
	daysStr := os.Getenv("CCLOAD_SQLITE_LOG_DAYS")
	if daysStr == "" {
		return 7 // 默认 7 天
	}
	days, err := strconv.Atoi(daysStr)
	if err != nil || days < -1 {
		log.Printf("[WARN] 无效的 CCLOAD_SQLITE_LOG_DAYS=%s，使用默认值 7", daysStr)
		return 7
	}
	return days
}
