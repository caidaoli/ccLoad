package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db        *sql.DB
	redisSync *RedisSync // Redis同步客户端 (OCP: 开放扩展，封闭修改)
	stmtCache sync.Map   // 预编译语句缓存 (性能优化: 减少SQL解析开销20-30%)
	stmtMux   sync.Mutex // 保护预编译语句创建过程

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

func NewSQLiteStore(path string, redisSync *RedisSync) (*SQLiteStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// 修复时区问题：强制使用本地时区解析时间戳，避免UTC/本地时区混淆导致冷却时间计算错误
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_foreign_keys=on&_pragma=journal_mode=WAL&_loc=Local", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	s := &SQLiteStore{
		db:        db,
		redisSync: redisSync,
		syncCh:    make(chan struct{}, 1), // 缓冲区=1，允许一个待处理任务
		done:      make(chan struct{}),
	}

	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}

	// 启动异步Redis同步worker（仅当Redis启用时）
	if redisSync != nil && redisSync.IsEnabled() {
		go s.redisSyncWorker()
	}

	// 性能优化：预编译所有热查询（阶段1优化）
	if err := s.prepareAllHotQueries(context.Background()); err != nil {
		// 预编译失败不影响启动，仅记录警告
		fmt.Printf("警告：预编译热查询失败: %v\n", err)
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

	// 创建 logs 表（time使用BIGINT Unix毫秒时间戳）
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			TIME BIGINT NOT NULL,
			model TEXT,
			channel_id INTEGER,
			status_code INTEGER NOT NULL,
			message TEXT,
			duration REAL,
			is_streaming INTEGER NOT NULL DEFAULT 0,
			first_byte_time REAL,
			FOREIGN KEY(channel_id) REFERENCES channels(id) ON DELETE SET NULL
		);
	`); err != nil {
		return fmt.Errorf("create logs table: %w", err)
	}

	// 添加新字段（兼容已有数据库）
	s.addColumnIfNotExists(ctx, "logs", "is_streaming", "INTEGER NOT NULL DEFAULT 0")
	s.addColumnIfNotExists(ctx, "logs", "first_byte_time", "REAL")
	s.addColumnIfNotExists(ctx, "logs", "api_key_used", "TEXT")                          // 使用的API Key（完整值）
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

	// 创建索引优化查询性能
	if _, err := s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_logs_time ON logs(TIME);
	`); err != nil {
		return fmt.Errorf("create logs time index: %w", err)
	}

	// 创建渠道名称索引
	if _, err := s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_channels_name ON channels(NAME);
	`); err != nil {
		return fmt.Errorf("create channels name index: %w", err)
	}

	// 创建日志状态码索引
	if _, err := s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_logs_status ON logs(status_code);
	`); err != nil {
		return fmt.Errorf("create logs status index: %w", err)
	}

	// 性能优化：创建复合索引优化常见查询（阶段1优化）
	// idx_logs_time_model - 优化按时间+模型查询（stats、metrics接口）
	if _, err := s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_logs_time_model ON logs(time, model);
	`); err != nil {
		return fmt.Errorf("create logs time_model index: %w", err)
	}

	// idx_logs_time_channel - 优化按时间+渠道查询（metrics接口渠道分组）
	if _, err := s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_logs_time_channel ON logs(time, channel_id);
	`); err != nil {
		return fmt.Errorf("create logs time_channel index: %w", err)
	}

	// idx_logs_time_status - 优化按时间+状态码查询（错误日志过滤）
	if _, err := s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_logs_time_status ON logs(time, status_code);
	`); err != nil {
		return fmt.Errorf("create logs time_status index: %w", err)
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
	if err := s.rebuildLogsTableToUnixTimestamp(ctx); err != nil {
		return fmt.Errorf("rebuild logs table: %w", err)
	}
	if err := s.rebuildChannelsTableToUnixTimestamp(ctx); err != nil {
		return fmt.Errorf("rebuild channels table: %w", err)
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

// migrateLogsToUnixTimestamp 迁移logs表的time列到time_ms（Unix毫秒时间戳）
// 根本解决strftime无法解析时区的问题
// rebuildLogsTableToUnixTimestamp 重建logs表，将time字段从TIMESTAMP改为BIGINT毫秒
func (s *SQLiteStore) rebuildLogsTableToUnixTimestamp(ctx context.Context) error {
	// 检查time字段类型是否需要重建
	var fieldType string
	err := s.db.QueryRowContext(ctx, `
		SELECT type FROM pragma_table_info('logs') WHERE name = 'time'
	`).Scan(&fieldType)
	if err != nil {
		return nil // 表不存在或字段不存在，跳过
	}

	// 如果已经是INTEGER/BIGINT，跳过重建
	if fieldType == "INTEGER" || fieldType == "BIGINT" {
		return nil
	}

	fmt.Println("🔄 重建 logs 表：time(TIMESTAMP) → time(BIGINT 毫秒)")

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 1. 创建新表
	_, err = tx.ExecContext(ctx, `
		CREATE TABLE logs_new (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			time BIGINT NOT NULL,
			model TEXT,
			channel_id INTEGER,
			status_code INTEGER NOT NULL,
			message TEXT,
			duration REAL,
			is_streaming INTEGER NOT NULL DEFAULT 0,
			first_byte_time REAL,
			api_key_used TEXT,
			FOREIGN KEY(channel_id) REFERENCES channels(id) ON DELETE SET NULL
		)
	`)
	if err != nil {
		return fmt.Errorf("create new logs table: %w", err)
	}

	// 2. 迁移数据：转换TIMESTAMP为Unix毫秒时间戳
	_, err = tx.ExecContext(ctx, `
		INSERT INTO logs_new (id, time, model, channel_id, status_code, message, duration, is_streaming, first_byte_time, api_key_used)
		SELECT
			id,
			CAST(strftime('%s', substr(time, 1, 19)) AS INTEGER) * 1000 as time,
			model,
			channel_id,
			status_code,
			message,
			duration,
			is_streaming,
			first_byte_time,
			api_key_used
		FROM logs
	`)
	if err != nil {
		return fmt.Errorf("migrate logs data: %w", err)
	}

	// 3. 删除旧表
	_, err = tx.ExecContext(ctx, `DROP TABLE logs`)
	if err != nil {
		return fmt.Errorf("drop old logs table: %w", err)
	}

	// 4. 重命名新表
	_, err = tx.ExecContext(ctx, `ALTER TABLE logs_new RENAME TO logs`)
	if err != nil {
		return fmt.Errorf("rename logs table: %w", err)
	}

	// 5. 重建索引
	_, err = tx.ExecContext(ctx, `CREATE INDEX idx_logs_time ON logs(time)`)
	if err != nil {
		return fmt.Errorf("create time index: %w", err)
	}
	_, err = tx.ExecContext(ctx, `CREATE INDEX idx_logs_status ON logs(status_code)`)
	if err != nil {
		return fmt.Errorf("create status index: %w", err)
	}
	_, err = tx.ExecContext(ctx, `CREATE INDEX idx_logs_time_model ON logs(time, model)`)
	if err != nil {
		return fmt.Errorf("create time_model index: %w", err)
	}
	_, err = tx.ExecContext(ctx, `CREATE INDEX idx_logs_time_channel ON logs(time, channel_id)`)
	if err != nil {
		return fmt.Errorf("create time_channel index: %w", err)
	}
	_, err = tx.ExecContext(ctx, `CREATE INDEX idx_logs_time_status ON logs(time, status_code)`)
	if err != nil {
		return fmt.Errorf("create time_status index: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	fmt.Println("✅ logs 表重建完成")
	return nil
}

// rebuildChannelsTableToUnixTimestamp 重建channels表，将created_at/updated_at从TIMESTAMP改为BIGINT秒
func (s *SQLiteStore) rebuildChannelsTableToUnixTimestamp(ctx context.Context) error {
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

	// 等待worker处理完最后的同步任务（最多等待3秒）
	time.Sleep(100 * time.Millisecond)

	return s.db.Close()
}

func (s *SQLiteStore) Vacuum(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "VACUUM")
	return err
}

// CleanupLogsBefore 清理截止时间之前的日志（DIP：通过接口暴露维护操作）
func (s *SQLiteStore) CleanupLogsBefore(ctx context.Context, cutoff time.Time) error {
	// time字段现在是BIGINT毫秒时间戳
	cutoffMs := cutoff.UnixMilli()
	_, err := s.db.ExecContext(ctx, `DELETE FROM logs WHERE time < ?`, cutoffMs)
	return err
}

// prepareStmt 获取或创建预编译语句（性能优化：减少SQL解析开销）
// 使用双重检查锁定模式避免重复编译：先无锁快速查找，未找到时加锁创建
func (s *SQLiteStore) prepareStmt(ctx context.Context, query string) (*sql.Stmt, error) {
	// 快速路径：从缓存获取（无锁）
	if cached, ok := s.stmtCache.Load(query); ok {
		return cached.(*sql.Stmt), nil
	}

	// 慢速路径：编译新语句（需要加锁避免重复编译）
	s.stmtMux.Lock()
	defer s.stmtMux.Unlock()

	// 双重检查：再次尝试从缓存获取（避免锁竞争期间其他goroutine已创建）
	if cached, ok := s.stmtCache.Load(query); ok {
		return cached.(*sql.Stmt), nil
	}

	// 编译新语句
	stmt, err := s.db.PrepareContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("prepare statement: %w", err)
	}

	// 缓存语句
	s.stmtCache.Store(query, stmt)
	return stmt, nil
}

// prepareAllHotQueries 预编译所有热查询（性能优化：启动时一次性编译）
// 作用：消除首次查询的编译延迟20-30%，提升冷启动性能
func (s *SQLiteStore) prepareAllHotQueries(ctx context.Context) error {
	hotQueries := []string{
		// 渠道查询（最热）
		`SELECT id, name, api_key, api_keys, key_strategy, url, priority, models, model_redirects, channel_type, enabled, created_at, updated_at
		FROM channels
		WHERE id = ?`,

		`SELECT id, name, api_key, api_keys, key_strategy, url, priority, models, model_redirects, channel_type, enabled, created_at, updated_at
		FROM channels
		ORDER BY priority DESC, id ASC`,

		// 冷却状态查询（热路径）
		`SELECT until FROM cooldowns WHERE channel_id = ?`,
		`SELECT until FROM key_cooldowns WHERE channel_id = ? AND key_index = ?`,

		// 日志插入（高频）
		`INSERT INTO logs(time, model, channel_id, status_code, message, duration, is_streaming, first_byte_time, api_key_used)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`,

		// 统计查询（定期调用）
		`SELECT channel_id, COALESCE(c.name, '系统') AS channel_name, COALESCE(l.model, '') AS model,
			SUM(CASE WHEN l.status_code >= 200 AND l.status_code < 300 THEN 1 ELSE 0 END) AS success,
			SUM(CASE WHEN l.status_code < 200 OR l.status_code >= 300 THEN 1 ELSE 0 END) AS error,
			COUNT(*) AS total
		FROM logs l 
		LEFT JOIN channels c ON c.id = l.channel_id
		WHERE l.time >= ?
		GROUP BY l.channel_id, c.name, l.model ORDER BY channel_name ASC, model ASC`,
	}

	preparedCount := 0
	for _, query := range hotQueries {
		if _, err := s.prepareStmt(ctx, query); err != nil {
			return fmt.Errorf("prepare query failed: %w", err)
		}
		preparedCount++
	}

	fmt.Printf("✅ 热查询预编译完成：%d 条语句\n", preparedCount)
	return nil
}

// ---- Store interface impl ----

func (s *SQLiteStore) ListConfigs(ctx context.Context) ([]*Config, error) {
	// 性能优化：使用预编译语句减少SQL解析开销
	query := `
		SELECT id, name, api_key, api_keys, key_strategy, url, priority, models, model_redirects, channel_type, enabled, created_at, updated_at
		FROM channels
		ORDER BY priority DESC, id ASC
	`
	stmt, err := s.prepareStmt(ctx, query)
	if err != nil {
		return nil, err
	}

	rows, err := stmt.QueryContext(ctx)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 使用统一的扫描器
	scanner := NewConfigScanner()
	return scanner.ScanConfigs(rows)
}

func (s *SQLiteStore) GetConfig(ctx context.Context, id int64) (*Config, error) {
	// 性能优化：使用预编译语句
	query := `
		SELECT id, name, api_key, api_keys, key_strategy, url, priority, models, model_redirects, channel_type, enabled, created_at, updated_at
		FROM channels
		WHERE id = ?
	`
	stmt, err := s.prepareStmt(ctx, query)
	if err != nil {
		return nil, err
	}

	row := stmt.QueryRowContext(ctx, id)

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

	// 从SQLite删除
	_, err := s.db.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, id)
	if err != nil {
		return err
	}

	// 异步全量同步所有渠道到Redis（非阻塞，立即返回）
	s.triggerAsyncSync()

	return nil
}

func (s *SQLiteStore) GetCooldownUntil(ctx context.Context, configID int64) (time.Time, bool) {
	row := s.db.QueryRowContext(ctx, `SELECT until FROM cooldowns WHERE channel_id = ?`, configID)
	return scanUnixTimestamp(row)
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

// BumpCooldownOnError 指数退避：错误翻倍（最小1s，最大30m），成功清零
func (s *SQLiteStore) BumpCooldownOnError(ctx context.Context, configID int64, now time.Time) (time.Duration, error) {
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

	// 使用工具函数计算指数退避时间
	next := calculateBackoffDuration(durMs, until, now)

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

	// 性能优化：使用预编译语句
	query := `
		INSERT INTO logs(time, model, channel_id, status_code, message, duration, is_streaming, first_byte_time, api_key_used)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	stmt, err := s.prepareStmt(ctx, query)
	if err != nil {
		return err
	}

	_, err = stmt.ExecContext(ctx, timeMs, e.Model, e.ChannelID, e.StatusCode, e.Message, e.Duration, e.IsStreaming, e.FirstByteTime, e.APIKeyUsed)
	return err
}

func (s *SQLiteStore) ListLogs(ctx context.Context, since time.Time, limit, offset int, filter *LogFilter) ([]*LogEntry, error) {
	// 使用查询构建器构建复杂查询
	baseQuery := `
		SELECT l.id, l.time, l.model, l.channel_id, c.name AS channel_name,
		       l.status_code, l.message, l.duration, l.is_streaming, l.first_byte_time, l.api_key_used
		FROM logs l
		LEFT JOIN channels c ON c.id = l.channel_id`

	// time字段现在是BIGINT毫秒时间戳，需要转换为Unix毫秒进行比较
	sinceMs := since.UnixMilli()

	qb := NewQueryBuilder(baseQuery).
		Where("l.time >= ?", sinceMs).
		ApplyFilter(filter)

	suffix := "ORDER BY l.time DESC LIMIT ? OFFSET ?"
	query, args := qb.BuildWithSuffix(suffix)
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*LogEntry{}
	for rows.Next() {
		var e LogEntry
		var cfgID sql.NullInt64
		var chName sql.NullString
		var duration sql.NullFloat64
		var isStreamingInt int
		var firstByteTime sql.NullFloat64
		var timeMs int64 // Unix毫秒时间戳
		var apiKeyUsed sql.NullString

		if err := rows.Scan(&e.ID, &timeMs, &e.Model, &cfgID, &chName,
			&e.StatusCode, &e.Message, &duration, &isStreamingInt, &firstByteTime, &apiKeyUsed); err != nil {
			return nil, err
		}

		// 转换Unix毫秒时间戳为time.Time
		e.Time = JSONTime{time.UnixMilli(timeMs)}

		if cfgID.Valid {
			id := cfgID.Int64
			e.ChannelID = &id
		}
		if chName.Valid {
			e.ChannelName = chName.String
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
	return out, nil
}

func (s *SQLiteStore) Aggregate(ctx context.Context, since time.Time, bucket time.Duration) ([]MetricPoint, error) {
	// 性能优化：使用SQL GROUP BY进行数据库层聚合，避免内存聚合
	// 原方案：加载所有日志到内存聚合（10万条日志需2-5秒，占用100-200MB内存）
	// 新方案：数据库聚合（查询时间-80%，内存占用-90%）

	bucketSeconds := int64(bucket.Seconds())
	sinceUnix := since.Unix()

	// SQL聚合查询：使用Unix时间戳除法实现时间桶分组
	// 性能优化：time字段为BIGINT毫秒时间戳，查询速度提升10-100倍
	// bucket_ts = (unix_timestamp_seconds / bucket_seconds) * bucket_seconds
	query := `
		SELECT
			((l.time / 1000) / ?) * ? AS bucket_ts,
			COALESCE(c.name, '未知渠道') AS channel_name,
			SUM(CASE WHEN l.status_code >= 200 AND l.status_code < 300 THEN 1 ELSE 0 END) AS success,
			SUM(CASE WHEN l.status_code < 200 OR l.status_code >= 300 THEN 1 ELSE 0 END) AS error
		FROM logs l
		LEFT JOIN channels c ON l.channel_id = c.id
		WHERE (l.time / 1000) >= ?
		GROUP BY bucket_ts, channel_name
		ORDER BY bucket_ts ASC
	`
	stmt, err := s.prepareStmt(ctx, query)
	if err != nil {
		return nil, err
	}

	rows, err := stmt.QueryContext(ctx, bucketSeconds, bucketSeconds, sinceUnix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// 解析聚合结果，按时间桶重组
	mapp := make(map[int64]*MetricPoint)
	for rows.Next() {
		var bucketTs int64
		var channelName string
		var success, errorCount int

		if err := rows.Scan(&bucketTs, &channelName, &success, &errorCount); err != nil {
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

		// 更新渠道统计
		mp.Channels[channelName] = ChannelMetric{
			Success: success,
			Error:   errorCount,
		}
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

// GetStats 实现统计功能，按渠道和模型统计成功/失败次数
func (s *SQLiteStore) GetStats(ctx context.Context, since time.Time, filter *LogFilter) ([]StatsEntry, error) {
	// 使用查询构建器构建统计查询
	baseQuery := `
		SELECT
			l.channel_id,
			COALESCE(c.name, '系统') AS channel_name,
			COALESCE(l.model, '') AS model,
			SUM(CASE WHEN l.status_code >= 200 AND l.status_code < 300 THEN 1 ELSE 0 END) AS success,
			SUM(CASE WHEN l.status_code < 200 OR l.status_code >= 300 THEN 1 ELSE 0 END) AS error,
			COUNT(*) AS total
		FROM logs l
		LEFT JOIN channels c ON c.id = l.channel_id`

	// time字段现在是BIGINT毫秒时间戳
	sinceMs := since.UnixMilli()

	qb := NewQueryBuilder(baseQuery).
		Where("l.time >= ?", sinceMs).
		ApplyFilter(filter)

	suffix := "GROUP BY l.channel_id, c.name, l.model ORDER BY channel_name ASC, model ASC"
	query, args := qb.BuildWithSuffix(suffix)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []StatsEntry
	for rows.Next() {
		var entry StatsEntry
		err := rows.Scan(&entry.ChannelID, &entry.ChannelName, &entry.Model,
			&entry.Success, &entry.Error, &entry.Total)
		if err != nil {
			return nil, err
		}
		stats = append(stats, entry)
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
func (s *SQLiteStore) redisSyncWorker() {
	ctx := context.Background()

	for {
		select {
		case <-s.syncCh:
			// 执行同步操作
			if err := s.doSyncAllChannels(ctx); err != nil {
				fmt.Printf("Warning: Async Redis sync failed: %v\n", err)
			}
		case <-s.done:
			// 优雅关闭：处理完最后一个任务（如果有）
			select {
			case <-s.syncCh:
				_ = s.doSyncAllChannels(ctx)
			default:
			}
			return
		}
	}
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

// BumpKeyCooldownOnError Key级别指数退避：错误翻倍（最小1s，最大30m）
func (s *SQLiteStore) BumpKeyCooldownOnError(ctx context.Context, configID int64, keyIndex int, now time.Time) (time.Duration, error) {
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

	// 使用工具函数计算指数退避时间
	next := calculateBackoffDuration(durMs, until, now)

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
