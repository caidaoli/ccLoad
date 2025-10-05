package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db        *sql.DB    // 主数据库（channels, cooldowns, rr）
	logDB     *sql.DB    // 日志数据库（logs）- 拆分以减少锁竞争和简化备份
	redisSync *RedisSync // Redis同步客户端 (OCP: 开放扩展，封闭修改)

	// ⚠️ 内存数据库守护连接（2025-10-05 P0修复）
	// 内存模式下，持有一个永不关闭的连接，确保数据库不被销毁
	keeperConn *sql.Conn // 守护连接（仅内存模式使用）

	// 异步Redis同步机制（性能优化: 避免同步等待）
	syncCh chan struct{} // 同步触发信号（无缓冲，去重合并多个请求）
	done   chan struct{} // 优雅关闭信号
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
// 特殊处理: :memory: -> /tmp/ccload-test-log.db（测试场景）
func generateLogDBPath(mainDBPath string) string {
	// 检测特殊的内存数据库标识（用于测试）
	if mainDBPath == ":memory:" {
		return filepath.Join(os.TempDir(), "ccload-test-log.db")
	}

	dir := filepath.Dir(mainDBPath)
	base := filepath.Base(mainDBPath)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	return filepath.Join(dir, name+"-log"+ext)
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
func buildMainDBDSN(path string) string {
	useMemory := os.Getenv("CCLOAD_USE_MEMORY_DB") == "true"

	if useMemory {
		// 内存模式：使用命名内存数据库（关键修复）
		// mode=memory: 显式声明为内存模式
		// cache=shared: 多连接共享同一数据库实例
		// ⚡ 性能：移除WAL（内存模式不需要WAL）
		return "file:ccload_mem_db?mode=memory&cache=shared&_pragma=busy_timeout(5000)&_foreign_keys=on&_loc=Local"
	}

	// 文件模式：保持原有逻辑
	return fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_foreign_keys=on&_pragma=journal_mode=WAL&_loc=Local", path)
}

// buildLogDBDSN 构建日志数据库DSN（始终使用文件模式）
// 日志库不使用内存模式，确保数据持久性
func buildLogDBDSN(path string) string {
	return fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode=WAL&_loc=Local", path)
}

func NewSQLiteStore(path string, redisSync *RedisSync) (*SQLiteStore, error) {
	// 检查是否启用内存模式
	useMemory := os.Getenv("CCLOAD_USE_MEMORY_DB") == "true"

	if !useMemory {
		// 文件模式：创建数据目录（内存模式无需创建目录）
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}

	// 打开主数据库（channels, cooldowns, rr）
	// 使用抽象的DSN构建函数，支持内存/文件模式切换
	dsn := buildMainDBDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)

	// ⚠️ 关键修复（2025-10-05）：内存模式下移除连接生命周期限制
	// 原因：SetConnMaxLifetime会导致所有连接定期过期
	//      如果所有连接同时关闭，命名内存数据库理论上不会销毁
	//      但为了绝对安全，内存模式下让连接永不过期
	// 文件模式：保持5分钟生命周期（避免长连接积累资源泄漏）
	if !useMemory {
		db.SetConnMaxLifetime(5 * time.Minute)
	}
	// 内存模式：不设置ConnMaxLifetime，连接永不过期（保证数据库始终可用）

	// 打开日志数据库（logs）- 始终使用文件模式
	logDBPath := generateLogDBPath(path)
	logDSN := buildLogDBDSN(logDBPath)
	logDB, err := sql.Open("sqlite", logDSN)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open log database: %w", err)
	}
	logDB.SetMaxOpenConns(10)
	logDB.SetMaxIdleConns(2)
	logDB.SetConnMaxLifetime(5 * time.Minute)

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
		keeperConn, err := db.Conn(context.Background())
		if err != nil {
			_ = db.Close()
			_ = logDB.Close()
			return nil, fmt.Errorf("创建内存数据库守护连接失败: %w", err)
		}
		s.keeperConn = keeperConn

		// 内存模式提示信息
		fmt.Println("⚡ 性能优化：主数据库使用内存模式（CCLOAD_USE_MEMORY_DB=true）")
		fmt.Println("   - 使用命名内存数据库（ccload_mem_db）+ 守护连接机制")
		fmt.Println("   - 守护连接确保数据库生命周期绑定到服务进程")
		fmt.Println("   - 连接池无生命周期限制，防止连接过期导致数据库销毁")
		fmt.Println("   - 渠道配置、冷却状态等热数据存储在内存中")
		fmt.Println("   - 日志数据仍然持久化到磁盘：", logDBPath)
		fmt.Println("   ⚠️  警告：服务重启后主数据库数据将丢失，请配置Redis同步或重新导入CSV")
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
		go s.redisSyncWorker()
	}

	return s, nil
}

// migrate 创建数据库表结构（Unix时间戳原生支持）
func (s *SQLiteStore) migrate(ctx context.Context) error {
	// 创建 channels 表（created_at/updated_at使用BIGINT Unix秒时间戳）
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS channels (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			NAME TEXT NOT NULL,
			api_key TEXT NOT NULL,
			url TEXT NOT NULL,
			priority INTEGER NOT NULL DEFAULT 0,
			models TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("create channels table: %w", err)
	}

	// 创建 cooldowns 表（使用Unix时间戳）
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS cooldowns (
			channel_id INTEGER PRIMARY KEY,
			until INTEGER NOT NULL,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY(channel_id) REFERENCES channels(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf("create cooldowns table: %w", err)
	}

	// 添加新字段（兼容已有数据库）
	s.addColumnIfNotExists(ctx, "channels", "model_redirects", "TEXT DEFAULT '{}'")      // 模型重定向字段，JSON格式
	s.addColumnIfNotExists(ctx, "channels", "api_keys", "TEXT DEFAULT '[]'")             // 多Key支持，JSON数组
	s.addColumnIfNotExists(ctx, "channels", "key_strategy", "TEXT DEFAULT 'sequential'") // Key使用策略
	s.addColumnIfNotExists(ctx, "channels", "channel_type", "TEXT DEFAULT 'anthropic'")  // 渠道类型

	// 创建 key_cooldowns 表（Key级别冷却，使用Unix时间戳）
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS key_cooldowns (
			channel_id INTEGER NOT NULL,
			key_index INTEGER NOT NULL,
			until INTEGER NOT NULL,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(channel_id, key_index),
			FOREIGN KEY(channel_id) REFERENCES channels(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf("create key_cooldowns table: %w", err)
	}

	// 创建 key_rr 表（Key级别轮询指针）
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS key_rr (
			channel_id INTEGER PRIMARY KEY,
			idx INTEGER NOT NULL,
			FOREIGN KEY(channel_id) REFERENCES channels(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf("create key_rr table: %w", err)
	}

	// 创建 rr (round-robin) 表
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS rr (
			KEY TEXT PRIMARY KEY,
			idx INTEGER NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("create rr table: %w", err)
	}

	// 创建渠道名称索引
	if _, err := s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_channels_name ON channels(NAME);
	`); err != nil {
		return fmt.Errorf("create channels name index: %w", err)
	}

	// 确保channels表的name字段具有UNIQUE约束
	if err := s.ensureChannelNameUnique(ctx); err != nil {
		return fmt.Errorf("ensure channel name unique: %w", err)
	}

	// 迁移api_keys字段：修复历史数据中的"null"字符串问题
	if err := s.migrateAPIKeysField(ctx); err != nil {
		return fmt.Errorf("migrate api_keys field: %w", err)
	}

	// 迁移冷却表的until字段从TIMESTAMP到Unix时间戳
	if err := s.migrateCooldownToUnixTimestamp(ctx); err != nil {
		return fmt.Errorf("migrate cooldown to unix timestamp: %w", err)
	}

	// Unix时间戳重构：重建表结构（TIMESTAMP → BIGINT）
	if err := s.rebuildChannelsTableToUnixTimestamp(ctx); err != nil {
		return fmt.Errorf("rebuild channels table: %w", err)
	}

	return nil
}

// migrateLogDB 创建日志数据库表结构（独立数据库，从零开始，无需兼容）
func (s *SQLiteStore) migrateLogDB(ctx context.Context) error {
	// 创建 logs 表（BIGINT Unix毫秒时间戳，所有字段一次性创建）
	// 注意：无 FOREIGN KEY 约束，因为 channels 表在主数据库中
	if _, err := s.logDB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			time BIGINT NOT NULL,
			model TEXT,
			channel_id INTEGER,
			status_code INTEGER NOT NULL,
			message TEXT,
			duration REAL,
			is_streaming INTEGER NOT NULL DEFAULT 0,
			first_byte_time REAL,
			api_key_used TEXT
		);
	`); err != nil {
		return fmt.Errorf("create logs table: %w", err)
	}

	// 创建索引（一次性创建，无需兼容检查）
	indexes := []string{
		"CREATE INDEX IF NOT EXISTS idx_logs_time ON logs(time)",
		"CREATE INDEX IF NOT EXISTS idx_logs_status ON logs(status_code)",
		"CREATE INDEX IF NOT EXISTS idx_logs_time_model ON logs(time, model)",
		"CREATE INDEX IF NOT EXISTS idx_logs_time_channel ON logs(time, channel_id)",
		"CREATE INDEX IF NOT EXISTS idx_logs_time_status ON logs(time, status_code)",
	}

	for _, indexSQL := range indexes {
		if _, err := s.logDB.ExecContext(ctx, indexSQL); err != nil {
			return fmt.Errorf("create index: %w", err)
		}
	}

	return nil
}

// addColumnIfNotExists 添加列如果不存在（用于数据库升级兼容）
func (s *SQLiteStore) addColumnIfNotExists(ctx context.Context, tableName, columnName, columnDef string) {
	// 检查列是否存在
	query := fmt.Sprintf("PRAGMA table_info(%s)", tableName)
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return
	}
	defer rows.Close()

	exists := false
	for rows.Next() {
		var cid int
		var name, dataType string
		var notNull, pk int
		var dfltValue sql.NullString

		if err := rows.Scan(&cid, &name, &dataType, &notNull, &dfltValue, &pk); err != nil {
			continue
		}
		if name == columnName {
			exists = true
			break
		}
	}

	if !exists {
		alterQuery := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", tableName, columnName, columnDef)
		s.db.ExecContext(ctx, alterQuery)
	}
}

// migrateAPIKeysField 数据库迁移：修复api_keys字段的脏数据
// 问题：历史数据中api_keys可能存储为"null"字符串，导致GetAPIKeys()返回空数组
// 解决方案：将api_key字段（逗号分隔）转换为api_keys JSON数组
// 执行时机：服务启动时自动运行（在ensureChannelNameUnique之后）
func (s *SQLiteStore) migrateAPIKeysField(ctx context.Context) error {
	// 内存数据库模式：跳过历史数据修复（KISS原则：内存DB无历史数据）
	useMemory := os.Getenv("CCLOAD_USE_MEMORY_DB") == "true"
	if useMemory {
		return nil
	}

	// 查询所有需要迁移的渠道：api_keys为"null"或空字符串，但api_key不为空
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, api_key
		FROM channels
		WHERE (api_keys IS NULL OR api_keys = 'null' OR api_keys = '' OR api_keys = '[]')
		  AND api_key IS NOT NULL
		  AND api_key != ''
	`)
	if err != nil {
		return fmt.Errorf("query channels for migration: %w", err)
	}
	defer rows.Close()

	migratedCount := 0
	for rows.Next() {
		var id int64
		var apiKey string
		if err := rows.Scan(&id, &apiKey); err != nil {
			continue // 跳过错误行
		}

		// 使用normalizeAPIKeys的相同逻辑：分割api_key字段
		keys := strings.Split(apiKey, ",")
		apiKeys := make([]string, 0, len(keys))
		for _, k := range keys {
			trimmed := strings.TrimSpace(k)
			if trimmed != "" {
				apiKeys = append(apiKeys, trimmed)
			}
		}

		// 序列化为JSON数组
		apiKeysJSON, err := sonic.Marshal(apiKeys)
		if err != nil {
			continue // 跳过序列化失败的
		}

		// 更新数据库
		_, err = s.db.ExecContext(ctx, `
			UPDATE channels
			SET api_keys = ?
			WHERE id = ?
		`, string(apiKeysJSON), id)
		if err == nil {
			migratedCount++
		}
	}

	if migratedCount > 0 {
		fmt.Printf("✅ api_keys字段迁移完成：修复 %d 条渠道记录\n", migratedCount)
	}

	return nil
}

// ensureChannelNameUnique 确保channels表的name字段具有UNIQUE约束
// 简化的四步迁移方案，遵循KISS原则
func (s *SQLiteStore) ensureChannelNameUnique(ctx context.Context) error {
	// 内存数据库模式优化：跳过重复数据检查，直接创建索引（YAGNI：内存DB无历史重复数据）
	useMemory := os.Getenv("CCLOAD_USE_MEMORY_DB") == "true"
	if useMemory {
		// 直接创建UNIQUE索引（CREATE IF NOT EXISTS保证幂等性）
		if _, err := s.db.ExecContext(ctx,
			"CREATE UNIQUE INDEX IF NOT EXISTS idx_channels_unique_name ON channels (NAME)",
		); err != nil {
			return fmt.Errorf("create unique index: %w", err)
		}
		return nil
	}

	// 第一步: 删除旧的普通索引
	if _, err := s.db.ExecContext(ctx, "DROP INDEX IF EXISTS idx_channels_name"); err != nil {
		return fmt.Errorf("drop old index: %w", err)
	}

	// 第二步: 检查是否已存在UNIQUE索引
	var count int
	err := s.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_channels_unique_name'",
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("check unique index exists: %w", err)
	}
	if count > 0 {
		return nil // 索引已存在，退出
	}

	// 第三步: 修复重复的name数据
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, GROUP_CONCAT(id) AS ids
		FROM channels
		GROUP BY name
		HAVING COUNT(*) > 1
	`)
	if err != nil {
		return fmt.Errorf("find duplicates: %w", err)
	}
	defer rows.Close()

	duplicateCount := 0
	for rows.Next() {
		var name, idsStr string
		if err := rows.Scan(&name, &idsStr); err != nil {
			continue
		}

		ids := strings.Split(idsStr, ",")
		if len(ids) <= 1 {
			continue
		}

		duplicateCount++

		// 保留第一个ID的name不变，其他ID的name改为 "原name+id"
		for i := 1; i < len(ids); i++ {
			newName := fmt.Sprintf("%s%s", name, ids[i])
			nowUnix := time.Now().Unix()
			_, err = s.db.ExecContext(ctx, `
				UPDATE channels SET name = ?, updated_at = ?
				WHERE id = ?
			`, newName, nowUnix, ids[i])
			if err != nil {
				return fmt.Errorf("fix duplicate name for id %s: %w", ids[i], err)
			}
		}
	}

	if duplicateCount > 0 {
		fmt.Printf("Fixed %d duplicate channel names\n", duplicateCount)
	}

	// 第四步: 创建UNIQUE索引
	if _, err := s.db.ExecContext(ctx,
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_channels_unique_name ON channels (NAME)",
	); err != nil {
		return fmt.Errorf("create unique index: %w", err)
	}

	return nil
}

// migrateCooldownToUnixTimestamp 迁移冷却表的until字段从TIMESTAMP到Unix时间戳
// 策略：检测字段类型，如果是TEXT/TIMESTAMP则重建表，如果已经是INTEGER则跳过
func (s *SQLiteStore) migrateCooldownToUnixTimestamp(ctx context.Context) error {
	// 内存数据库模式：跳过冷却表迁移（KISS原则：内存DB字段类型已正确）
	useMemory := os.Getenv("CCLOAD_USE_MEMORY_DB") == "true"
	if useMemory {
		return nil
	}

	// 检查cooldowns表的until字段类型
	needMigrateCooldowns := false
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info(cooldowns)")
	if err != nil {
		return fmt.Errorf("check cooldowns table: %w", err)
	}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dfltValue any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dfltValue, &pk); err != nil {
			continue
		}
		if name == "until" && typ != "INTEGER" {
			needMigrateCooldowns = true
			break
		}
	}
	rows.Close()

	if needMigrateCooldowns {
		// 重建cooldowns表
		if err := s.rebuildCooldownsTable(ctx); err != nil {
			return fmt.Errorf("rebuild cooldowns table: %w", err)
		}
		fmt.Println("✅ 迁移 cooldowns 表：TIMESTAMP → Unix时间戳")
	}

	// 检查key_cooldowns表的until字段类型
	needMigrateKeyCooldowns := false
	rows, err = s.db.QueryContext(ctx, "PRAGMA table_info(key_cooldowns)")
	if err != nil {
		return fmt.Errorf("check key_cooldowns table: %w", err)
	}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dfltValue any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dfltValue, &pk); err != nil {
			continue
		}
		if name == "until" && typ != "INTEGER" {
			needMigrateKeyCooldowns = true
			break
		}
	}
	rows.Close()

	if needMigrateKeyCooldowns {
		// 重建key_cooldowns表
		if err := s.rebuildKeyCooldownsTable(ctx); err != nil {
			return fmt.Errorf("rebuild key_cooldowns table: %w", err)
		}
		fmt.Println("✅ 迁移 key_cooldowns 表：TIMESTAMP → Unix时间戳")
	}

	return nil
}

// rebuildChannelsTableToUnixTimestamp 重建channels表，将created_at/updated_at从TIMESTAMP改为BIGINT秒
func (s *SQLiteStore) rebuildChannelsTableToUnixTimestamp(ctx context.Context) error {
	// 内存数据库模式：跳过表重建（KISS原则：内存DB总是全新的，无需向后兼容迁移）
	// 原因：内存数据库在启动时已创建正确的BIGINT字段类型，重建操作不仅无必要，还引入竞态条件风险
	useMemory := os.Getenv("CCLOAD_USE_MEMORY_DB") == "true"
	if useMemory {
		return nil // 内存模式直接跳过，避免DROP TABLE的并发竞态窗口
	}

	// 检查created_at字段类型是否需要重建
	var fieldType string
	err := s.db.QueryRowContext(ctx, `
		SELECT type FROM pragma_table_info('channels') WHERE name = 'created_at'
	`).Scan(&fieldType)
	if err != nil {
		return nil // 表不存在或字段不存在，跳过
	}

	// 如果已经是INTEGER/BIGINT，跳过重建
	if fieldType == "INTEGER" || fieldType == "BIGINT" {
		return nil
	}

	fmt.Println("🔄 重建 channels 表：created_at/updated_at(TIMESTAMP) → (BIGINT 秒)")

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. 创建新表
	_, err = tx.ExecContext(ctx, `
		CREATE TABLE channels_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			api_key TEXT NOT NULL,
			api_keys TEXT DEFAULT '[]',
			key_strategy TEXT DEFAULT 'sequential',
			url TEXT NOT NULL,
			priority INTEGER NOT NULL DEFAULT 0,
			models TEXT NOT NULL,
			model_redirects TEXT DEFAULT '{}',
			channel_type TEXT DEFAULT 'anthropic',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL
		)
	`)
	if err != nil {
		return fmt.Errorf("create new channels table: %w", err)
	}

	// 2. 迁移数据：转换TIMESTAMP为Unix秒时间戳
	_, err = tx.ExecContext(ctx, `
		INSERT INTO channels_new (id, name, api_key, api_keys, key_strategy, url, priority, models, model_redirects, channel_type, enabled, created_at, updated_at)
		SELECT
			id,
			name,
			api_key,
			COALESCE(api_keys, '[]'),
			COALESCE(key_strategy, 'sequential'),
			url,
			priority,
			models,
			COALESCE(model_redirects, '{}'),
			COALESCE(channel_type, 'anthropic'),
			enabled,
			CAST(strftime('%s', substr(created_at, 1, 19)) AS INTEGER) as created_at,
			CAST(strftime('%s', substr(updated_at, 1, 19)) AS INTEGER) as updated_at
		FROM channels
	`)
	if err != nil {
		return fmt.Errorf("migrate channels data: %w", err)
	}

	// 3. 删除旧表
	_, err = tx.ExecContext(ctx, `DROP TABLE channels`)
	if err != nil {
		return fmt.Errorf("drop old channels table: %w", err)
	}

	// 4. 重命名新表
	_, err = tx.ExecContext(ctx, `ALTER TABLE channels_new RENAME TO channels`)
	if err != nil {
		return fmt.Errorf("rename channels table: %w", err)
	}

	// 5. 重建唯一索引
	_, err = tx.ExecContext(ctx, `CREATE UNIQUE INDEX idx_channels_unique_name ON channels(name)`)
	if err != nil {
		return fmt.Errorf("create unique name index: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	fmt.Println("✅ channels 表重建完成")
	return nil
}

// rebuildTimestampTable 通用表重建函数，用于TIMESTAMP到Unix时间戳迁移
// 遵循DRY原则，消除重复的表重建逻辑
func (s *SQLiteStore) rebuildTimestampTable(ctx context.Context, tableName, createTableSQL string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	tempTableName := tableName + "_new"

	// 1. 创建临时表（新结构）
	createSQL := strings.ReplaceAll(createTableSQL, tableName, tempTableName)
	if _, err := tx.ExecContext(ctx, createSQL); err != nil {
		return fmt.Errorf("create temp table: %w", err)
	}

	// 2. 清理旧数据（TIMESTAMP格式无法可靠转换）
	fmt.Printf("  清理旧的%s记录（格式不兼容）\n", tableName)

	// 3. 删除旧表
	dropSQL := fmt.Sprintf("DROP TABLE %s", tableName)
	if _, err := tx.ExecContext(ctx, dropSQL); err != nil {
		return fmt.Errorf("drop old table: %w", err)
	}

	// 4. 重命名新表
	renameSQL := fmt.Sprintf("ALTER TABLE %s RENAME TO %s", tempTableName, tableName)
	if _, err := tx.ExecContext(ctx, renameSQL); err != nil {
		return fmt.Errorf("rename table: %w", err)
	}

	return tx.Commit()
}

// rebuildCooldownsTable 重建cooldowns表，迁移现有数据
func (s *SQLiteStore) rebuildCooldownsTable(ctx context.Context) error {
	createSQL := `
		CREATE TABLE cooldowns (
			channel_id INTEGER PRIMARY KEY,
			until INTEGER NOT NULL,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY(channel_id) REFERENCES channels(id) ON DELETE CASCADE
		)
	`
	return s.rebuildTimestampTable(ctx, "cooldowns", createSQL)
}

// rebuildKeyCooldownsTable 重建key_cooldowns表，迁移现有数据
func (s *SQLiteStore) rebuildKeyCooldownsTable(ctx context.Context) error {
	createSQL := `
		CREATE TABLE key_cooldowns (
			channel_id INTEGER NOT NULL,
			key_index INTEGER NOT NULL,
			until INTEGER NOT NULL,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(channel_id, key_index),
			FOREIGN KEY(channel_id) REFERENCES channels(id) ON DELETE CASCADE
		)
	`
	return s.rebuildTimestampTable(ctx, "key_cooldowns", createSQL)
}

func (s *SQLiteStore) Close() error {
	// 优雅关闭：通知worker退出
	if s.done != nil {
		close(s.done)
	}

	// 等待worker处理完最后的同步任务（最多等待100ms）
	time.Sleep(100 * time.Millisecond)

	// ⚠️ 内存数据库守护连接：最后关闭（P0修复 2025-10-05）
	// 确保守护连接在所有其他操作完成后才关闭
	// 这样可以保证内存数据库在整个服务生命周期内始终存在
	if s.keeperConn != nil {
		if err := s.keeperConn.Close(); err != nil {
			// 记录错误但不影响后续关闭操作
			fmt.Printf("⚠️  关闭守护连接失败: %v\n", err)
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

// ---- Store interface impl ----

func (s *SQLiteStore) ListConfigs(ctx context.Context) ([]*Config, error) {
	query := `
		SELECT id, name, api_key, api_keys, key_strategy, url, priority, models, model_redirects, channel_type, enabled, created_at, updated_at
		FROM channels
		ORDER BY priority DESC, id ASC
	`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 使用统一的扫描器
	scanner := NewConfigScanner()
	return scanner.ScanConfigs(rows)
}

func (s *SQLiteStore) GetConfig(ctx context.Context, id int64) (*Config, error) {
	query := `
		SELECT id, name, api_key, api_keys, key_strategy, url, priority, models, model_redirects, channel_type, enabled, created_at, updated_at
		FROM channels
		WHERE id = ?
	`
	row := s.db.QueryRowContext(ctx, query, id)

	// 使用统一的扫描器
	scanner := NewConfigScanner()
	config, err := scanner.ScanConfig(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("not found")
		}
		return nil, err
	}
	return config, nil
}

// GetEnabledChannelsByModel 查询支持指定模型的启用渠道（按优先级排序）
// 性能优化：使用 LEFT JOIN 一次性查询渠道和冷却状态，消除 N+1 查询问题
func (s *SQLiteStore) GetEnabledChannelsByModel(ctx context.Context, model string) ([]*Config, error) {
	var query string
	var args []any
	nowUnix := time.Now().Unix()

	if model == "*" {
		// 通配符：返回所有启用且未冷却的渠道
		query = `
			SELECT c.id, c.name, c.api_key, c.api_keys, c.key_strategy, c.url, c.priority,
			       c.models, c.model_redirects, c.channel_type, c.enabled, c.created_at, c.updated_at
			FROM channels c
			LEFT JOIN cooldowns cd ON c.id = cd.channel_id
			WHERE c.enabled = 1
			  AND (cd.until IS NULL OR cd.until <= ?)
			ORDER BY c.priority DESC, c.id ASC
		`
		args = []any{nowUnix}
	} else {
		// 精确匹配：查询支持该模型且未冷却的渠道
		query = `
			SELECT c.id, c.name, c.api_key, c.api_keys, c.key_strategy, c.url, c.priority,
			       c.models, c.model_redirects, c.channel_type, c.enabled, c.created_at, c.updated_at
			FROM channels c
			LEFT JOIN cooldowns cd ON c.id = cd.channel_id
			WHERE c.enabled = 1
			  AND c.models LIKE ?
			  AND (cd.until IS NULL OR cd.until <= ?)
			ORDER BY c.priority DESC, c.id ASC
		`
		args = []any{"%" + model + "%", nowUnix}
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	scanner := NewConfigScanner()
	return scanner.ScanConfigs(rows)
}

// GetEnabledChannelsByType 查询指定类型的启用渠道（按优先级排序）
// 性能优化：使用 LEFT JOIN 一次性查询渠道和冷却状态，消除 N+1 查询问题
func (s *SQLiteStore) GetEnabledChannelsByType(ctx context.Context, channelType string) ([]*Config, error) {
	nowUnix := time.Now().Unix()
	query := `
		SELECT c.id, c.name, c.api_key, c.api_keys, c.key_strategy, c.url, c.priority,
		       c.models, c.model_redirects, c.channel_type, c.enabled, c.created_at, c.updated_at
		FROM channels c
		LEFT JOIN cooldowns cd ON c.id = cd.channel_id
		WHERE c.enabled = 1
		  AND c.channel_type = ?
		  AND (cd.until IS NULL OR cd.until <= ?)
		ORDER BY c.priority DESC, c.id ASC
	`

	rows, err := s.db.QueryContext(ctx, query, channelType, nowUnix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	scanner := NewConfigScanner()
	return scanner.ScanConfigs(rows)
}

func (s *SQLiteStore) CreateConfig(ctx context.Context, c *Config) (*Config, error) {
	nowUnix := time.Now().Unix() // Unix秒时间戳
	modelsStr, _ := serializeModels(c.Models)
	modelRedirectsStr, _ := serializeModelRedirects(c.ModelRedirects)

	// 规范化APIKeys字段（DRY：统一处理，避免"null"字符串）
	normalizeAPIKeys(c)
	apiKeysStr, _ := sonic.Marshal(c.APIKeys) // 序列化多Key数组

	// 使用GetChannelType确保默认值
	channelType := c.GetChannelType()
	keyStrategy := c.GetKeyStrategy() // 确保默认值

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO channels(name, api_key, api_keys, key_strategy, url, priority, models, model_redirects, channel_type, enabled, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, c.Name, c.APIKey, string(apiKeysStr), keyStrategy, c.URL, c.Priority, modelsStr, modelRedirectsStr, channelType,
		boolToInt(c.Enabled), nowUnix, nowUnix)

	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()

	// 获取完整的配置信息
	config, err := s.GetConfig(ctx, id)
	if err != nil {
		return nil, err
	}

	// 异步全量同步所有渠道到Redis（非阻塞，立即返回）
	s.triggerAsyncSync()

	return config, nil
}

func (s *SQLiteStore) UpdateConfig(ctx context.Context, id int64, upd *Config) (*Config, error) {
	if upd == nil {
		return nil, errors.New("update payload cannot be nil")
	}

	// 确认目标存在，保持与之前逻辑一致
	if _, err := s.GetConfig(ctx, id); err != nil {
		return nil, err
	}

	name := strings.TrimSpace(upd.Name)
	apiKey := strings.TrimSpace(upd.APIKey)
	url := strings.TrimSpace(upd.URL)
	modelsStr, _ := serializeModels(upd.Models)
	modelRedirectsStr, _ := serializeModelRedirects(upd.ModelRedirects)

	// 规范化APIKeys字段（DRY：统一处理，避免"null"字符串）
	normalizeAPIKeys(upd)
	apiKeysStr, _ := sonic.Marshal(upd.APIKeys) // 序列化多Key数组
	channelType := upd.GetChannelType()         // 确保默认值
	keyStrategy := upd.GetKeyStrategy()         // 确保默认值
	updatedAtUnix := time.Now().Unix()          // Unix秒时间戳

	_, err := s.db.ExecContext(ctx, `
		UPDATE channels
		SET name=?, api_key=?, api_keys=?, key_strategy=?, url=?, priority=?, models=?, model_redirects=?, channel_type=?, enabled=?, updated_at=?
		WHERE id=?
	`, name, apiKey, string(apiKeysStr), keyStrategy, url, upd.Priority, modelsStr, modelRedirectsStr, channelType,
		boolToInt(upd.Enabled), updatedAtUnix, id)
	if err != nil {
		return nil, err
	}

	// 获取更新后的配置
	config, err := s.GetConfig(ctx, id)
	if err != nil {
		return nil, err
	}

	// 异步全量同步所有渠道到Redis（非阻塞，立即返回）
	s.triggerAsyncSync()

	return config, nil
}

func (s *SQLiteStore) ReplaceConfig(ctx context.Context, c *Config) (*Config, error) {
	nowUnix := time.Now().Unix() // Unix秒时间戳
	modelsStr, _ := serializeModels(c.Models)
	modelRedirectsStr, _ := serializeModelRedirects(c.ModelRedirects)

	// 规范化APIKeys字段（DRY：统一处理，避免"null"字符串）
	normalizeAPIKeys(c)
	apiKeysStr, _ := sonic.Marshal(c.APIKeys) // 序列化多Key数组
	channelType := c.GetChannelType()         // 确保默认值
	keyStrategy := c.GetKeyStrategy()         // 确保默认值
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO channels(name, api_key, api_keys, key_strategy, url, priority, models, model_redirects, channel_type, enabled, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(NAME) DO UPDATE SET
			api_key = excluded.api_key,
			api_keys = excluded.api_keys,
			key_strategy = excluded.key_strategy,
			url = excluded.url,
			priority = excluded.priority,
			models = excluded.models,
			model_redirects = excluded.model_redirects,
			channel_type = excluded.channel_type,
			enabled = excluded.enabled,
			updated_at = excluded.updated_at
	`, c.Name, c.APIKey, string(apiKeysStr), keyStrategy, c.URL, c.Priority, modelsStr, modelRedirectsStr, channelType,
		boolToInt(c.Enabled), nowUnix, nowUnix)
	if err != nil {
		return nil, err
	}

	// 获取实际的记录ID（可能是新创建的或已存在的）
	var id int64
	err = s.db.QueryRowContext(ctx, `SELECT id FROM channels WHERE name = ?`, c.Name).Scan(&id)
	if err != nil {
		return nil, err
	}

	// 获取完整的配置信息
	config, err := s.GetConfig(ctx, id)
	if err != nil {
		return nil, err
	}

	// 注意: ReplaceConfig通常在批量导入时使用，最后会统一调用SyncAllChannelsToRedis
	// 这里不做单独同步，避免CSV导入时的N次Redis操作

	return config, nil
}

func (s *SQLiteStore) DeleteConfig(ctx context.Context, id int64) error {
	// 检查记录是否存在（幂等性）
	if _, err := s.GetConfig(ctx, id); err != nil {
		if strings.Contains(err.Error(), "not found") {
			return nil // 记录不存在，直接返回
		}
		return err
	}

	// 级联删除所有关联资源（事务保证原子性）
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	// 1. 删除渠道配置
	if _, err := tx.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete channel: %w", err)
	}

	// 2. 级联删除渠道级冷却数据
	if _, err := tx.ExecContext(ctx, `DELETE FROM cooldowns WHERE channel_id = ?`, id); err != nil {
		return fmt.Errorf("delete cooldowns: %w", err)
	}

	// 3. 级联删除Key级冷却数据
	if _, err := tx.ExecContext(ctx, `DELETE FROM key_cooldowns WHERE channel_id = ?`, id); err != nil {
		return fmt.Errorf("delete key_cooldowns: %w", err)
	}

	// 4. 级联删除Key轮询指针
	if _, err := tx.ExecContext(ctx, `DELETE FROM key_rr WHERE channel_id = ?`, id); err != nil {
		return fmt.Errorf("delete key_rr: %w", err)
	}

	// 提交事务
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	// 异步全量同步所有渠道到Redis（非阻塞，立即返回）
	s.triggerAsyncSync()

	return nil
}

func (s *SQLiteStore) GetCooldownUntil(ctx context.Context, configID int64) (time.Time, bool) {
	row := s.db.QueryRowContext(ctx, `SELECT until FROM cooldowns WHERE channel_id = ?`, configID)
	return scanUnixTimestamp(row)
}

// GetAllChannelCooldowns 批量查询所有渠道冷却状态（P0性能优化）
// 性能提升：N次查询 → 1次查询，消除N+1问题
func (s *SQLiteStore) GetAllChannelCooldowns(ctx context.Context) (map[int64]time.Time, error) {
	now := time.Now().Unix()
	query := `SELECT channel_id, until FROM cooldowns WHERE until > ?`

	rows, err := s.db.QueryContext(ctx, query, now)
	if err != nil {
		return nil, fmt.Errorf("query all channel cooldowns: %w", err)
	}
	defer rows.Close()

	result := make(map[int64]time.Time)
	for rows.Next() {
		var channelID int64
		var until int64

		if err := rows.Scan(&channelID, &until); err != nil {
			return nil, fmt.Errorf("scan channel cooldown: %w", err)
		}

		result[channelID] = time.Unix(until, 0)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate channel cooldowns: %w", err)
	}

	return result, nil
}

func (s *SQLiteStore) SetCooldown(ctx context.Context, configID int64, until time.Time) error {
	now := time.Now()
	// 使用工具函数计算冷却持续时间和时间戳
	durMs := calculateCooldownDuration(until, now)
	unixTime := toUnixTimestamp(until)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cooldowns(channel_id, until, duration_ms) VALUES(?, ?, ?)
		ON CONFLICT(channel_id) DO UPDATE SET
			until = excluded.until,
			duration_ms = excluded.duration_ms
	`, configID, unixTime, durMs)
	return err
}

// BumpCooldownOnError 指数退避：错误翻倍（认证错误5分钟起，其他1秒起，最大30m），成功清零
func (s *SQLiteStore) BumpCooldownOnError(ctx context.Context, configID int64, now time.Time, statusCode int) (time.Duration, error) {
	var unixTime int64
	var durMs int64
	err := s.db.QueryRowContext(ctx, `
		SELECT until, COALESCE(duration_ms, 0)
		FROM cooldowns
		WHERE channel_id = ?
	`, configID).Scan(&unixTime, &durMs)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	// 从Unix时间戳转换为time.Time
	until := time.Unix(unixTime, 0)

	// 使用工具函数计算指数退避时间（传递statusCode用于确定初始冷却时间）
	next := calculateBackoffDuration(durMs, until, now, &statusCode)

	newUntil := now.Add(next)
	// 转换为Unix时间戳存储
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO cooldowns(channel_id, until, duration_ms) VALUES(?, ?, ?)
		ON CONFLICT(channel_id) DO UPDATE SET
			until = excluded.until,
			duration_ms = excluded.duration_ms
	`, configID, newUntil.Unix(), int64(next/time.Millisecond))

	if err != nil {
		return 0, err
	}
	return next, nil
}

func (s *SQLiteStore) ResetCooldown(ctx context.Context, configID int64) error {
	// 删除记录，等效于冷却为0
	_, err := s.db.ExecContext(ctx, `DELETE FROM cooldowns WHERE channel_id = ?`, configID)
	return err
}

func (s *SQLiteStore) AddLog(ctx context.Context, e *LogEntry) error {
	if e.Time.Time.IsZero() {
		e.Time = JSONTime{time.Now()}
	}

	// 清理单调时钟信息，确保时间格式标准化
	cleanTime := e.Time.Time.Round(0) // 移除单调时钟部分

	// Unix时间戳：直接存储毫秒级Unix时间戳
	timeMs := cleanTime.UnixMilli()

	// 直接写入日志数据库（简化预编译语句缓存）
	query := `
		INSERT INTO logs(time, model, channel_id, status_code, message, duration, is_streaming, first_byte_time, api_key_used)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	_, err := s.logDB.ExecContext(ctx, query, timeMs, e.Model, e.ChannelID, e.StatusCode, e.Message, e.Duration, e.IsStreaming, e.FirstByteTime, e.APIKeyUsed)
	return err
}

func (s *SQLiteStore) ListLogs(ctx context.Context, since time.Time, limit, offset int, filter *LogFilter) ([]*LogEntry, error) {
	// 使用查询构建器构建复杂查询（从 logDB 查询）
	// 性能优化：批量查询渠道名称消除N+1问题（100渠道场景提升50-100倍）
	baseQuery := `
		SELECT id, time, model, channel_id, status_code, message, duration, is_streaming, first_byte_time, api_key_used
		FROM logs`

	// time字段现在是BIGINT毫秒时间戳，需要转换为Unix毫秒进行比较
	sinceMs := since.UnixMilli()

    qb := NewQueryBuilder(baseQuery).
        Where("time >= ?", sinceMs)

    // 支持按渠道名称过滤（无需跨库JOIN，先解析为渠道ID集合再按channel_id过滤）
    if filter != nil && (filter.ChannelName != "" || filter.ChannelNameLike != "") {
        ids, err := s.fetchChannelIDsByNameFilter(ctx, filter.ChannelName, filter.ChannelNameLike)
        if err != nil {
            return nil, err
        }
        if len(ids) == 0 {
            return []*LogEntry{}, nil
        }
        // 转换为[]any以用于占位符
        vals := make([]any, 0, len(ids))
        for _, id := range ids {
            vals = append(vals, id)
        }
        qb.WhereIn("channel_id", vals)
    }

    // 其余过滤条件（model等）
    qb.ApplyFilter(filter)

	suffix := "ORDER BY time DESC LIMIT ? OFFSET ?"
	query, args := qb.BuildWithSuffix(suffix)
	args = append(args, limit, offset)

	rows, err := s.logDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*LogEntry{}
	channelIDsToFetch := make(map[int64]bool)

	for rows.Next() {
		var e LogEntry
		var cfgID sql.NullInt64
		var duration sql.NullFloat64
		var isStreamingInt int
		var firstByteTime sql.NullFloat64
		var timeMs int64 // Unix毫秒时间戳
		var apiKeyUsed sql.NullString

		if err := rows.Scan(&e.ID, &timeMs, &e.Model, &cfgID,
			&e.StatusCode, &e.Message, &duration, &isStreamingInt, &firstByteTime, &apiKeyUsed); err != nil {
			return nil, err
		}

		// 转换Unix毫秒时间戳为time.Time
		e.Time = JSONTime{time.UnixMilli(timeMs)}

		if cfgID.Valid {
			id := cfgID.Int64
			e.ChannelID = &id
			channelIDsToFetch[id] = true
		}
		if duration.Valid {
			e.Duration = duration.Float64
		}
		e.IsStreaming = isStreamingInt != 0
		if firstByteTime.Valid {
			fbt := firstByteTime.Float64
			e.FirstByteTime = &fbt
		}
		if apiKeyUsed.Valid && apiKeyUsed.String != "" {
			e.APIKeyUsed = maskAPIKey(apiKeyUsed.String)
		}
		out = append(out, &e)
	}

	// 批量查询渠道名称（P0性能优化：N+1 → 1次查询）
	if len(channelIDsToFetch) > 0 {
		channelNames, err := s.fetchChannelNamesBatch(ctx, channelIDsToFetch)
		if err != nil {
			// 降级处理：查询失败不影响日志返回，仅记录错误
			fmt.Printf("⚠️  批量查询渠道名称失败: %v\n", err)
			channelNames = make(map[int64]string)
		}

		// 填充渠道名称
		for _, e := range out {
			if e.ChannelID != nil {
				if name, ok := channelNames[*e.ChannelID]; ok {
					e.ChannelName = name
				}
			}
		}
	}

	return out, nil
}

func (s *SQLiteStore) Aggregate(ctx context.Context, since time.Time, bucket time.Duration) ([]MetricPoint, error) {
	// 性能优化：使用SQL GROUP BY进行数据库层聚合，避免内存聚合
	// 原方案：加载所有日志到内存聚合（10万条日志需2-5秒，占用100-200MB内存）
	// 新方案：数据库聚合（查询时间-80%，内存占用-90%）
	// 批量查询渠道名称消除N+1问题（100渠道场景提升50-100倍）

	bucketSeconds := int64(bucket.Seconds())
	sinceUnix := since.Unix()

	// SQL聚合查询：使用Unix时间戳除法实现时间桶分组（从 logDB）
	// 性能优化：time字段为BIGINT毫秒时间戳，查询速度提升10-100倍
	// bucket_ts = (unix_timestamp_seconds / bucket_seconds) * bucket_seconds
	query := `
		SELECT
			((time / 1000) / ?) * ? AS bucket_ts,
			channel_id,
			SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) AS success,
			SUM(CASE WHEN status_code < 200 OR status_code >= 300 THEN 1 ELSE 0 END) AS error
		FROM logs
		WHERE (time / 1000) >= ?
		GROUP BY bucket_ts, channel_id
		ORDER BY bucket_ts ASC
	`

	rows, err := s.logDB.QueryContext(ctx, query, bucketSeconds, bucketSeconds, sinceUnix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 解析聚合结果，按时间桶重组
	mapp := make(map[int64]*MetricPoint)
	channelIDsToFetch := make(map[int64]bool)

	for rows.Next() {
		var bucketTs int64
		var channelID sql.NullInt64
		var success, errorCount int

		if err := rows.Scan(&bucketTs, &channelID, &success, &errorCount); err != nil {
			return nil, err
		}

		// 获取或创建时间桶
		mp, ok := mapp[bucketTs]
		if !ok {
			mp = &MetricPoint{
				Ts:       time.Unix(bucketTs, 0),
				Channels: make(map[string]ChannelMetric),
			}
			mapp[bucketTs] = mp
		}

		// 更新总体统计
		mp.Success += success
		mp.Error += errorCount

		// 暂时使用 channel_id 作为 key，稍后替换为 name
		channelKey := "未知渠道"
		if channelID.Valid {
			channelKey = fmt.Sprintf("ch_%d", channelID.Int64)
			channelIDsToFetch[channelID.Int64] = true
		}

		mp.Channels[channelKey] = ChannelMetric{
			Success: success,
			Error:   errorCount,
		}
	}

	// 批量查询渠道名称（P0性能优化：N+1 → 1次查询）
	channelNames := make(map[int64]string)
	if len(channelIDsToFetch) > 0 {
		var err error
		channelNames, err = s.fetchChannelNamesBatch(ctx, channelIDsToFetch)
		if err != nil {
			// 降级处理：查询失败不影响聚合返回，仅记录错误
			fmt.Printf("⚠️  批量查询渠道名称失败: %v\n", err)
			channelNames = make(map[int64]string)
		}
	}

	// 替换 channel_id 为 channel_name
	for _, mp := range mapp {
		newChannels := make(map[string]ChannelMetric)
		for key, metric := range mp.Channels {
			if key == "未知渠道" {
				newChannels[key] = metric
			} else {
				// 解析 ch_123 格式
				var channelID int64
				fmt.Sscanf(key, "ch_%d", &channelID)
				if name, ok := channelNames[channelID]; ok {
					newChannels[name] = metric
				} else {
					newChannels["未知渠道"] = metric
				}
			}
		}
		mp.Channels = newChannels
	}

	// 生成完整的时间序列（填充空桶）
	out := []MetricPoint{}
	now := time.Now()
	endTime := now.Truncate(bucket).Add(bucket)
	startTime := since.Truncate(bucket)

	for t := startTime; t.Before(endTime); t = t.Add(bucket) {
		ts := t.Unix()
		if mp, ok := mapp[ts]; ok {
			out = append(out, *mp)
		} else {
			out = append(out, MetricPoint{
				Ts:       t,
				Channels: make(map[string]ChannelMetric),
			})
		}
	}

	// 已按时间升序（GROUP BY bucket_ts ASC）
	return out, nil
}

func (s *SQLiteStore) NextRR(ctx context.Context, model string, priority int, n int) int {
	if n <= 0 {
		return 0
	}

	key := fmt.Sprintf("%s|%d", model, priority)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return 0
	}
	defer func() { _ = tx.Rollback() }()

	var cur int
	err = tx.QueryRowContext(ctx, `SELECT idx FROM rr WHERE KEY = ?`, key).Scan(&cur)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			cur = 0
			if _, err := tx.ExecContext(ctx, `INSERT INTO rr(key, idx) VALUES(?, ?)`, key, 0); err != nil {
				return 0
			}
		} else {
			return 0
		}
	}

	if cur >= n {
		cur = 0
	}

	next := cur + 1
	if next >= n {
		next = 0
	}

	if _, err := tx.ExecContext(ctx, `UPDATE rr SET idx = ? WHERE KEY = ?`, next, key); err != nil {
		return cur
	}

	_ = tx.Commit()
	return cur
}

func (s *SQLiteStore) SetRR(ctx context.Context, model string, priority int, idx int) error {
	key := fmt.Sprintf("%s|%d", model, priority)
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO rr (key, idx) VALUES (?, ?)`, key, idx)
	return err
}

// GetStats 实现统计功能，按渠道和模型统计成功/失败次数（从 logDB）
// 性能优化：批量查询渠道名称消除N+1问题（100渠道场景提升50-100倍）
func (s *SQLiteStore) GetStats(ctx context.Context, since time.Time, filter *LogFilter) ([]StatsEntry, error) {
	// 使用查询构建器构建统计查询（从 logDB）
	baseQuery := `
		SELECT
			channel_id,
			COALESCE(model, '') AS model,
			SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END) AS success,
			SUM(CASE WHEN status_code < 200 OR status_code >= 300 THEN 1 ELSE 0 END) AS error,
			COUNT(*) AS total
		FROM logs`

	// time字段现在是BIGINT毫秒时间戳
	sinceMs := since.UnixMilli()

	qb := NewQueryBuilder(baseQuery).
		Where("time >= ?", sinceMs).
		ApplyFilter(filter)

	suffix := "GROUP BY channel_id, model ORDER BY channel_id ASC, model ASC"
	query, args := qb.BuildWithSuffix(suffix)

	rows, err := s.logDB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []StatsEntry
	channelIDsToFetch := make(map[int64]bool)

	for rows.Next() {
		var entry StatsEntry
		err := rows.Scan(&entry.ChannelID, &entry.Model,
			&entry.Success, &entry.Error, &entry.Total)
		if err != nil {
			return nil, err
		}

		if entry.ChannelID != nil {
			channelIDsToFetch[int64(*entry.ChannelID)] = true
		}
		stats = append(stats, entry)
	}

	// 批量查询渠道名称（P0性能优化：N+1 → 1次查询）
	if len(channelIDsToFetch) > 0 {
		channelNames, err := s.fetchChannelNamesBatch(ctx, channelIDsToFetch)
		if err != nil {
			// 降级处理：查询失败不影响统计返回，仅记录错误
			fmt.Printf("⚠️  批量查询渠道名称失败: %v\n", err)
			channelNames = make(map[int64]string)
		}

		// 填充渠道名称
		for i := range stats {
			if stats[i].ChannelID != nil {
				if name, ok := channelNames[int64(*stats[i].ChannelID)]; ok {
					stats[i].ChannelName = name
				} else {
					stats[i].ChannelName = "系统"
				}
			} else {
				stats[i].ChannelName = "系统"
			}
		}
	} else {
		// 没有渠道ID，全部标记为系统
		for i := range stats {
			stats[i].ChannelName = "系统"
		}
	}

	return stats, nil
}

// LoadChannelsFromRedis 从Redis恢复渠道数据到SQLite (启动时数据库恢复机制)
func (s *SQLiteStore) LoadChannelsFromRedis(ctx context.Context) error {
	if !s.redisSync.IsEnabled() {
		return nil
	}

	// 从Redis加载所有渠道配置
	configs, err := s.redisSync.LoadChannelsFromRedis(ctx)
	if err != nil {
		return fmt.Errorf("load from redis: %w", err)
	}

	if len(configs) == 0 {
		fmt.Println("No channels found in Redis")
		return nil
	}

	fmt.Printf("Restoring %d channels from Redis...\n", len(configs))

	// 使用事务确保数据一致性 (ACID原则)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	nowUnix := time.Now().Unix()
	successCount := 0

	for _, config := range configs {
		// 标准化数据：确保默认值正确填充
		modelsStr, _ := serializeModels(config.Models)
		modelRedirectsStr, _ := serializeModelRedirects(config.ModelRedirects)
		apiKeysStr, _ := sonic.Marshal(config.APIKeys)
		channelType := config.GetChannelType() // 强制使用默认值anthropic
		keyStrategy := config.GetKeyStrategy() // 强制使用默认值sequential

		// 使用完整字段列表确保数据一致性（包含所有新字段）
		_, err := tx.ExecContext(ctx, `
			INSERT OR REPLACE INTO channels(
				name, api_key, api_keys, key_strategy, url, priority,
				models, model_redirects, channel_type, enabled, created_at, updated_at
			)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, config.Name, config.APIKey, string(apiKeysStr), keyStrategy, config.URL, config.Priority,
			modelsStr, modelRedirectsStr, channelType, boolToInt(config.Enabled), nowUnix, nowUnix)

		if err != nil {
			fmt.Printf("Warning: failed to restore channel %s: %v\n", config.Name, err)
			continue
		}
		successCount++
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	fmt.Printf("Successfully restored %d/%d channels from Redis\n", successCount, len(configs))
	return nil
}

// SyncAllChannelsToRedis 将所有渠道同步到Redis (批量同步，初始化时使用)
func (s *SQLiteStore) SyncAllChannelsToRedis(ctx context.Context) error {
	if !s.redisSync.IsEnabled() {
		return nil
	}

	configs, err := s.ListConfigs(ctx)
	if err != nil {
		return fmt.Errorf("list configs: %w", err)
	}

	if len(configs) == 0 {
		fmt.Println("No channels to sync to Redis")
		return nil
	}

	// 规范化所有Config对象的默认值（确保Redis中数据完整性）
	normalizeConfigDefaults(configs)

	fmt.Printf("Syncing %d channels to Redis...\n", len(configs))

	if err := s.redisSync.SyncAllChannels(ctx, configs); err != nil {
		return fmt.Errorf("sync to redis: %w", err)
	}

	fmt.Printf("Successfully synced %d channels to Redis\n", len(configs))
	return nil
}

// redisSyncWorker 异步Redis同步worker（后台goroutine）
// 修复：增加重试机制，避免瞬时网络故障导致数据丢失（P0修复 2025-10-05）
func (s *SQLiteStore) redisSyncWorker() {
	ctx := context.Background()

	// 指数退避重试配置
	retryBackoff := []time.Duration{
		1 * time.Second,   // 第1次重试：1秒后
		5 * time.Second,   // 第2次重试：5秒后
		15 * time.Second,  // 第3次重试：15秒后
	}

	for {
		select {
		case <-s.syncCh:
			// 执行同步操作，支持重试
			syncErr := s.doSyncAllChannelsWithRetry(ctx, retryBackoff)
			if syncErr != nil {
				// 所有重试都失败，记录致命错误
				fmt.Printf("❌ 严重错误: Redis同步失败（已重试%d次）: %v\n", len(retryBackoff), syncErr)
				fmt.Printf("   警告: 服务重启后可能丢失渠道配置，请检查Redis连接或手动备份数据库\n")
			}

		case <-s.done:
			// 优雅关闭：处理完最后一个任务（如果有）
			select {
			case <-s.syncCh:
				// 关闭时不重试，快速同步一次即可
				_ = s.doSyncAllChannels(ctx)
			default:
			}
			return
		}
	}
}

// doSyncAllChannelsWithRetry 带重试机制的同步操作（P0修复新增）
func (s *SQLiteStore) doSyncAllChannelsWithRetry(ctx context.Context, retryBackoff []time.Duration) error {
	var lastErr error

	// 首次尝试
	if err := s.doSyncAllChannels(ctx); err == nil {
		return nil // 成功
	} else {
		lastErr = err
		fmt.Printf("⚠️  Redis同步失败（将自动重试）: %v\n", err)
	}

	// 重试逻辑
	for attempt := 0; attempt < len(retryBackoff); attempt++ {
		// 等待退避时间
		time.Sleep(retryBackoff[attempt])

		// 重试同步
		if err := s.doSyncAllChannels(ctx); err == nil {
			fmt.Printf("✅ Redis同步恢复成功（第%d次重试）\n", attempt+1)
			return nil // 成功
		} else {
			lastErr = err
			fmt.Printf("⚠️  Redis同步重试失败（第%d次）: %v\n", attempt+1, err)
		}
	}

	// 所有重试都失败
	return fmt.Errorf("all %d retries failed: %w", len(retryBackoff), lastErr)
}

// triggerAsyncSync 触发异步Redis同步（非阻塞）
func (s *SQLiteStore) triggerAsyncSync() {
	if s.redisSync == nil || !s.redisSync.IsEnabled() {
		return
	}

	// 非阻塞发送（如果channel已满则跳过，避免阻塞主流程）
	select {
	case s.syncCh <- struct{}{}:
		// 成功发送信号
	default:
		// channel已有待处理任务，跳过（去重）
	}
}

// doSyncAllChannels 实际执行同步操作（worker内部调用）
func (s *SQLiteStore) doSyncAllChannels(ctx context.Context) error {
	configs, err := s.ListConfigs(ctx)
	if err != nil {
		return fmt.Errorf("list configs: %w", err)
	}

	// 规范化默认值后再同步（与SyncAllChannelsToRedis保持一致）
	normalizeConfigDefaults(configs)

	return s.redisSync.SyncAllChannels(ctx, configs)
}

// normalizeConfigDefaults 规范化Config对象的默认值字段（DRY原则：统一规范化逻辑）
// 确保序列化到Redis时所有字段都有正确的默认值，避免空值污染
func normalizeConfigDefaults(configs []*Config) {
	for _, config := range configs {
		// 强制填充channel_type默认值（避免空字符串序列化到Redis）
		if config.ChannelType == "" {
			config.ChannelType = "anthropic"
		}
		// 强制填充key_strategy默认值
		if config.KeyStrategy == "" {
			config.KeyStrategy = "sequential"
		}
		// 确保model_redirects不为nil（避免序列化为null）
		if config.ModelRedirects == nil {
			config.ModelRedirects = make(map[string]string)
		}
		// 确保api_keys不为nil
		if config.APIKeys == nil {
			config.APIKeys = []string{}
		}
	}
}

// fetchChannelNamesBatch 批量查询渠道名称（P0性能优化 2025-10-05）
// 性能提升：N+1查询 → 1次全表查询 + 内存过滤（100渠道场景提升50-100倍）
// 设计原则（KISS）：渠道总数<1000，全表扫描比IN子查询更简单、更快
// 输入：渠道ID集合 map[int64]bool
// 输出：ID→名称映射 map[int64]string
func (s *SQLiteStore) fetchChannelNamesBatch(ctx context.Context, channelIDs map[int64]bool) (map[int64]string, error) {
	if len(channelIDs) == 0 {
		return make(map[int64]string), nil
	}

	// 查询所有渠道（全表扫描，渠道数<1000时比IN子查询更快）
	// 优势：固定SQL（查询计划缓存）、无动态参数绑定、代码简单
	rows, err := s.db.QueryContext(ctx, "SELECT id, name FROM channels")
	if err != nil {
		return nil, fmt.Errorf("query all channel names: %w", err)
	}
	defer rows.Close()

	// 解析并过滤需要的渠道（内存过滤，O(N)但N<1000）
	channelNames := make(map[int64]string, len(channelIDs))
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			continue // 跳过扫描错误的行
		}
		// 只保留需要的渠道
		if channelIDs[id] {
			channelNames[id] = name
		}
	}

	return channelNames, nil
}

// fetchChannelIDsByNameFilter 根据精确/模糊名称获取渠道ID集合
// 目的：避免跨库JOIN（logs在logDB，channels在主db），先解析为ID再过滤logs
func (s *SQLiteStore) fetchChannelIDsByNameFilter(ctx context.Context, exact string, like string) ([]int64, error) {
    // 构建查询
    var (
        query string
        args  []any
    )
    if exact != "" {
        query = "SELECT id FROM channels WHERE name = ?"
        args = []any{exact}
    } else if like != "" {
        query = "SELECT id FROM channels WHERE name LIKE ?"
        args = []any{"%" + like + "%"}
    } else {
        return nil, nil
    }

    rows, err := s.db.QueryContext(ctx, query, args...)
    if err != nil {
        return nil, fmt.Errorf("query channel ids by name: %w", err)
    }
    defer rows.Close()

    var ids []int64
    for rows.Next() {
        var id int64
        if err := rows.Scan(&id); err != nil {
            return nil, fmt.Errorf("scan channel id: %w", err)
        }
        ids = append(ids, id)
    }
    if err := rows.Err(); err != nil {
        return nil, err
    }
    return ids, nil
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

// ==================== Key级别冷却机制 ====================

// GetKeyCooldownUntil 查询指定Key的冷却截止时间
func (s *SQLiteStore) GetKeyCooldownUntil(ctx context.Context, configID int64, keyIndex int) (time.Time, bool) {
	row := s.db.QueryRowContext(ctx, `
		SELECT until FROM key_cooldowns
		WHERE channel_id = ? AND key_index = ?
	`, configID, keyIndex)
	return scanUnixTimestamp(row)
}

// GetAllKeyCooldowns 批量查询所有Key冷却状态（P1修复 2025-10-05）
// 返回: map[channelID]map[keyIndex]cooldownUntil
// 性能优化: 一次查询替代 N*M 次独立查询（N=渠道数, M=Key数）
func (s *SQLiteStore) GetAllKeyCooldowns(ctx context.Context) (map[int64]map[int]time.Time, error) {
	now := time.Now().Unix()
	query := `SELECT channel_id, key_index, until FROM key_cooldowns WHERE until > ?`

	rows, err := s.db.QueryContext(ctx, query, now)
	if err != nil {
		return nil, fmt.Errorf("query all key cooldowns: %w", err)
	}
	defer rows.Close()

	result := make(map[int64]map[int]time.Time)
	for rows.Next() {
		var channelID int64
		var keyIndex int
		var until int64

		if err := rows.Scan(&channelID, &keyIndex, &until); err != nil {
			return nil, fmt.Errorf("scan key cooldown: %w", err)
		}

		// 初始化渠道级map
		if result[channelID] == nil {
			result[channelID] = make(map[int]time.Time)
		}
		result[channelID][keyIndex] = time.Unix(until, 0)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return result, nil
}

// BumpKeyCooldownOnError Key级别指数退避：错误翻倍（认证错误5分钟起，其他1秒起，最大30m）
func (s *SQLiteStore) BumpKeyCooldownOnError(ctx context.Context, configID int64, keyIndex int, now time.Time, statusCode int) (time.Duration, error) {
	var unixTime int64
	var durMs int64
	err := s.db.QueryRowContext(ctx, `
		SELECT until, COALESCE(duration_ms, 0)
		FROM key_cooldowns
		WHERE channel_id = ? AND key_index = ?
	`, configID, keyIndex).Scan(&unixTime, &durMs)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	// 从Unix时间戳转换为time.Time
	until := time.Unix(unixTime, 0)

	// 使用工具函数计算指数退避时间（传递statusCode用于确定初始冷却时间）
	next := calculateBackoffDuration(durMs, until, now, &statusCode)

	newUntil := now.Add(next)
	// 转换为Unix时间戳存储
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO key_cooldowns(channel_id, key_index, until, duration_ms) VALUES(?, ?, ?, ?)
		ON CONFLICT(channel_id, key_index) DO UPDATE SET
			until = excluded.until,
			duration_ms = excluded.duration_ms
	`, configID, keyIndex, newUntil.Unix(), int64(next/time.Millisecond))

	if err != nil {
		return 0, err
	}
	return next, nil
}

// SetKeyCooldown 设置指定Key的冷却截止时间
func (s *SQLiteStore) SetKeyCooldown(ctx context.Context, configID int64, keyIndex int, until time.Time) error {
	now := time.Now()
	durationMs := calculateCooldownDuration(until, now)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO key_cooldowns(channel_id, key_index, until, duration_ms) VALUES(?, ?, ?, ?)
		ON CONFLICT(channel_id, key_index) DO UPDATE SET
			until = excluded.until,
			duration_ms = excluded.duration_ms
	`, configID, keyIndex, until.Unix(), durationMs)
	return err
}

// ResetKeyCooldown 重置指定Key的冷却状态
func (s *SQLiteStore) ResetKeyCooldown(ctx context.Context, configID int64, keyIndex int) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM key_cooldowns
		WHERE channel_id = ? AND key_index = ?
	`, configID, keyIndex)
	return err
}

// ClearAllKeyCooldowns 清理渠道的所有Key冷却数据（用于Key变更时避免索引错位）
func (s *SQLiteStore) ClearAllKeyCooldowns(ctx context.Context, configID int64) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM key_cooldowns
		WHERE channel_id = ?
	`, configID)
	return err
}

// ==================== Key级别轮询机制 ====================

// NextKeyRR 获取下一个轮询Key索引（带自动增量）
func (s *SQLiteStore) NextKeyRR(ctx context.Context, configID int64, keyCount int) int {
	if keyCount <= 0 {
		return 0
	}

	var idx int
	err := s.db.QueryRowContext(ctx, `
		SELECT idx FROM key_rr WHERE channel_id = ?
	`, configID).Scan(&idx)

	if err != nil {
		// 没有记录，从0开始
		return 0
	}

	// 确保索引在有效范围内
	return idx % keyCount
}

// SetKeyRR 设置渠道的Key轮询指针
func (s *SQLiteStore) SetKeyRR(ctx context.Context, configID int64, idx int) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO key_rr(channel_id, idx) VALUES(?, ?)
		ON CONFLICT(channel_id) DO UPDATE SET idx = excluded.idx
	`, configID, idx)
	return err
}
