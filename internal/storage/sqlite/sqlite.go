package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
    db    *sql.DB // 主数据库（channels, api_keys, key_rr）
    logDB *sql.DB // 日志数据库（logs）- 拆分以减少锁竞争和简化备份

	// ⚠️ 内存数据库守护连接（2025-10-05 P0修复）
	// 内存模式下，持有一个永不关闭的连接，确保数据库不被销毁
	keeperConn *sql.Conn // 守护连接（仅内存模式使用）

	// 异步Redis同步机制（性能优化: 避免同步等待）
    syncCh chan struct{} // 同步触发信号（无缓冲，去重合并多个请求）
    done   chan struct{} // 优雅关闭信号

    redisSync RedisSync // Redis同步接口（依赖注入，支持测试和扩展）

    // 优雅关闭：等待后台worker
    wg sync.WaitGroup
}

// RedisSync Redis同步接口抽象（依赖倒置原则）
type RedisSync interface {
	IsEnabled() bool
	LoadChannelsWithKeysFromRedis(ctx context.Context) ([]*model.ChannelWithKeys, error)
	SyncAllChannelsWithKeys(ctx context.Context, channels []*model.ChannelWithKeys) error
}

// maskAPIKey 将API Key掩码为 "abcd...klmn" 格式（前4位 + ... + 后4位）
func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return key // 短key直接返回
	}
	return key[:4] + "..." + key[len(key)-4:]
}

// generateLogDBPath 从主数据库路径生成日志数据库路径
// 例如: ./data/ccload.db -> ./data/ccload-log.db
// 已有-log后缀不重复添加: ./data/ccload-log.db -> ./data/ccload-log.db
// 特殊处理: :memory: -> :memory:（内存模式）
func generateLogDBPath(mainDBPath string) string {
	// 检测特殊的内存数据库标识（保持原样）
	if mainDBPath == ":memory:" {
		return ":memory:"
	}

	// 保留原始路径的相对路径前缀（./）
	dir := filepath.Dir(mainDBPath)
	base := filepath.Base(mainDBPath)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)

	// 如果已经有-log后缀，不重复添加
	if strings.HasSuffix(name, "-log") {
		return mainDBPath
	}

	// 构建新路径，保留./前缀
	result := filepath.Join(dir, name+"-log"+ext)

	// filepath.Join会清理./前缀，需要手动恢复
	if strings.HasPrefix(mainDBPath, "./") && !strings.HasPrefix(result, "./") {
		result = "./" + result
	}

	return result
}

// buildMainDBDSN 构建主数据库DSN（支持内存模式）
// 内存模式：CCLOAD_USE_MEMORY_DB=true -> file:ccload_mem_db?mode=memory&cache=shared
// 文件模式：默认 -> file:/path/to/db?_pragma=...
//
// ⚠️ 重要修复（2025-10-05）：
// - 使用命名内存数据库（ccload_mem_db）而非匿名内存数据库（::memory:）
// - 命名数据库的生命周期绑定到进程，而非最后一个连接
// - 即使所有连接关闭，只要进程存活，数据库就保留在内存中
// - 解决了连接池生命周期导致的"no such table"错误
//
// ✅ P0修复（2025-10-06）：环境变量控制Journal模式
// - SQLITE_JOURNAL_MODE: WAL(默认) | DELETE | TRUNCATE | PERSIST | MEMORY | OFF
// - Docker/K8s环境建议使用TRUNCATE避免WAL文件损坏风险
func buildMainDBDSN(path string) string {
	useMemory := os.Getenv("CCLOAD_USE_MEMORY_DB") == "true"

	if useMemory {
		// 内存模式：使用命名内存数据库（关键修复）
		// mode=memory: 显式声明为内存模式
		// cache=shared: 多连接共享同一数据库实例
		// ⚡ 性能：移除WAL（内存模式不需要WAL）
		return "file:ccload_mem_db?mode=memory&cache=shared&_pragma=busy_timeout(5000)&_foreign_keys=on&_loc=Local"
	}

	// ✅ P0安全修复：支持环境变量配置Journal模式
	// 设计原则：生产环境（特别是容器/网络存储）需要灵活控制
	journalMode := os.Getenv("SQLITE_JOURNAL_MODE")
	if journalMode == "" {
		journalMode = "WAL" // 默认本地环境使用WAL（高性能）
	}

	return fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_foreign_keys=on&_pragma=journal_mode=%s&_loc=Local", path, journalMode)
}

// buildLogDBDSN 构建日志数据库DSN（始终使用文件模式）
// 日志库不使用内存模式，确保数据持久性
// ✅ P0修复（2025-10-06）：与主数据库保持一致的Journal模式控制
func buildLogDBDSN(path string) string {
	// 使用与主数据库相同的Journal模式配置
	journalMode := os.Getenv("SQLITE_JOURNAL_MODE")
	if journalMode == "" {
		journalMode = "WAL" // 默认WAL
	}

	return fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode=%s&_loc=Local", path, journalMode)
}

func NewSQLiteStore(path string, redisSync RedisSync) (*SQLiteStore, error) {
	// 检查是否启用内存模式
	useMemory := os.Getenv("CCLOAD_USE_MEMORY_DB") == "true"

	if !useMemory {
		// 文件模式：创建数据目录（内存模式无需创建目录）
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}

	// 打开主数据库（channels, api_keys, key_rr）
	// 使用抽象的DSN构建函数，支持内存/文件模式切换
	dsn := buildMainDBDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// ✅ P2连接池优化（2025-10-06）：根据模式差异化配置
	if useMemory {
		// 内存模式：适度减少连接数（无限并发对内存数据库无益）
		db.SetMaxOpenConns(config.SQLiteMaxOpenConnsMemory)
		db.SetMaxIdleConns(config.SQLiteMaxIdleConnsMemory)
		// 不设置ConnMaxLifetime，连接永不过期（保证数据库始终可用）
	} else {
		// WAL文件模式：严格限制写并发（WAL性能瓶颈）
		db.SetMaxOpenConns(config.SQLiteMaxOpenConnsFile)
		db.SetMaxIdleConns(config.SQLiteMaxIdleConnsFile)
		// 缩短连接生命周期（更快资源回收）
		db.SetConnMaxLifetime(config.MinutesToDuration(config.SQLiteConnMaxLifetimeMinutes))
	}

	// 打开日志数据库（logs）- 始终使用文件模式
	logDBPath := generateLogDBPath(path)
	logDSN := buildLogDBDSN(logDBPath)
	logDB, err := sql.Open("sqlite", logDSN)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open log database: %w", err)
	}
	// ✅ P2日志库优化（2025-10-06）：与主库对齐，降低资源占用
	logDB.SetMaxOpenConns(config.SQLiteMaxOpenConnsFile)
	logDB.SetMaxIdleConns(config.SQLiteMaxIdleConnsFile)
	logDB.SetConnMaxLifetime(config.MinutesToDuration(config.SQLiteConnMaxLifetimeMinutes))

	s := &SQLiteStore{
		db:        db,
		logDB:     logDB,
		redisSync: redisSync,
		syncCh:    make(chan struct{}, 1), // 缓冲区=1，允许一个待处理任务
		done:      make(chan struct{}),
	}

	// ⚠️ 内存数据库守护连接（P0修复 2025-10-05）
	// SQLite内存数据库的特性：当最后一个连接关闭时，数据库被删除
	// 解决方案：持有一个永不关闭的"守护连接"，确保数据库始终存在
	if useMemory {
		// ✅ P0安全检查（2025-10-12）：内存模式强制要求Redis备份
		// 设计原则：防止数据永久丢失，确保故障可恢复
		if redisSync == nil || !redisSync.IsEnabled() {
			_ = db.Close()
			_ = logDB.Close()
			log.Print("❌ 错误：内存模式必须配置Redis同步，否则服务重启后数据将永久丢失")
			log.Print("   解决方案：")
			log.Print("   1. 设置环境变量：REDIS_URL=redis://localhost:6379")
			log.Print("   2. 或禁用内存模式：unset CCLOAD_USE_MEMORY_DB（使用文件模式）")
			return nil, fmt.Errorf("内存模式必须配置Redis同步（REDIS_URL）")
		}

		keeperConn, err := db.Conn(context.Background())
		if err != nil {
			_ = db.Close()
			_ = logDB.Close()
			return nil, fmt.Errorf("创建内存数据库守护连接失败: %w", err)
		}
		s.keeperConn = keeperConn

		// 内存模式提示信息
		log.Print("⚡ 性能优化：主数据库使用内存模式（CCLOAD_USE_MEMORY_DB=true）")
		log.Print("   - 使用命名内存数据库（ccload_mem_db）+ 守护连接机制")
		log.Print("   - 守护连接确保数据库生命周期绑定到服务进程")
		log.Print("   - 连接池无生命周期限制，防止连接过期导致数据库销毁")
		log.Print("   - 渠道配置、冷却状态等热数据存储在内存中")
		log.Print("   - 日志数据仍然持久化到磁盘：", logDBPath)
		log.Print("   ✅ Redis同步已启用：数据自动备份，服务重启后可恢复")
	}

	// 迁移主数据库表结构
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		_ = logDB.Close()
		return nil, err
	}

	// 迁移日志数据库表结构
	if err := s.migrateLogDB(context.Background()); err != nil {
		_ = db.Close()
		_ = logDB.Close()
		return nil, err
	}

    // 启动异步Redis同步worker（仅当Redis启用时）
    if redisSync != nil && redisSync.IsEnabled() {
        s.wg.Add(1)
        go func() {
            defer s.wg.Done()
            s.redisSyncWorker()
        }()
    }

	return s, nil
}

// 确保SQLiteStore实现了storage.Store接口
var _ storage.Store = (*SQLiteStore)(nil)

// IsRedisEnabled 检查Redis同步是否启用（公共方法）
func (s *SQLiteStore) IsRedisEnabled() bool {
	return s.redisSync != nil && s.redisSync.IsEnabled()
}

func (s *SQLiteStore) Close() error {
    // 优雅关闭：通知worker退出
    if s.done != nil {
        close(s.done)
    }

    // 等待worker退出（带超时），避免无谓等待
    waitCh := make(chan struct{})
    go func() {
        s.wg.Wait()
        close(waitCh)
    }()
    select {
    case <-waitCh:
    case <-time.After(time.Duration(config.RedisSyncShutdownTimeoutMs) * time.Millisecond):
        log.Printf("⚠️  Redis同步worker关闭超时（%dms）", config.RedisSyncShutdownTimeoutMs)
    }

	// ⚠️ 内存数据库守护连接：最后关闭（P0修复 2025-10-05）
	// 确保守护连接在所有其他操作完成后才关闭
	// 这样可以保证内存数据库在整个服务生命周期内始终存在
	if s.keeperConn != nil {
		if err := s.keeperConn.Close(); err != nil {
			// 记录错误但不影响后续关闭操作
			log.Printf("⚠️  关闭守护连接失败: %v", err)
		}
	}

	// 关闭数据库连接池
	if err := s.db.Close(); err != nil {
		return err
	}

	// 关闭日志数据库
	return s.logDB.Close()
}

// CleanupLogsBefore 清理截止时间之前的日志（DIP：通过接口暴露维护操作）
func (s *SQLiteStore) CleanupLogsBefore(ctx context.Context, cutoff time.Time) error {
	// time字段现在是BIGINT毫秒时间戳（使用 logDB）
	cutoffMs := cutoff.UnixMilli()
	_, err := s.logDB.ExecContext(ctx, `DELETE FROM logs WHERE time < ?`, cutoffMs)
	return err
}

// CheckDatabaseExists 检查SQLite数据库文件是否存在
func CheckDatabaseExists(dbPath string) bool {
	if _, err := os.Stat(dbPath); err != nil {
		return false
	}
	return true
}

// 辅助函数
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// serializeModels 序列化模型列表为JSON字符串
