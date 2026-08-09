package storage

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	sqlstore "ccLoad/internal/storage/sql"
)

// SyncManager 负责启动时从主库恢复数据到 SQLite。
//
// 核心职责：
// - 配置表在同一主库快照中全量恢复
// - logs 表按天数恢复主库快照（流式处理，避免内存溢出）
// - 超时由调用方 context 统一约束，配置恢复失败直接阻止启动
//
// 设计原则：
// - KISS：简单的单向数据复制，无复杂一致性
// - Fail-Fast：恢复失败直接退出，不降级
type SyncManager struct {
	primary *sqlstore.SQLStore
	sqlite  *sqlstore.SQLStore
}

// NewSyncManager 创建同步管理器
func NewSyncManager(primary, sqlite *sqlstore.SQLStore) *SyncManager {
	return &SyncManager{
		primary: primary,
		sqlite:  sqlite,
	}
}

// RestoreOnStartup 启动时恢复数据（从主库恢复到 SQLite）
//
// logDays 参数：
//   - -1 = 全量恢复（慎用，启动慢）
//   - 0 = 仅恢复配置表，不恢复 logs
//   - 7 = 恢复配置表 + 最近 7 天 logs
func (sm *SyncManager) RestoreOnStartup(ctx context.Context, logDays int) error {
	start := time.Now()

	// 第一步：恢复配置表（快速，<1 秒）
	configTables := []string{
		"system_settings",
		"channels",
		"channel_models",
		"channel_model_cooldowns",
		"channel_url_states",
		"api_keys",
		"auth_tokens",
		"model_fingerprints",
		"fingerprint_test_results",
	}

	// TiDB 不实现 MySQL 的 READ ONLY 事务选项；恢复代码本身只发 SELECT，
	// 对 MySQL/TiDB 保留 REPEATABLE READ 即可获得一致快照。
	primaryTx, err := sm.primary.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  !sm.primary.IsMySQL(),
	})
	if err != nil {
		return fmt.Errorf("开启主库配置快照失败: %w", err)
	}
	defer func() { _ = primaryTx.Rollback() }()
	sqliteTx, err := sm.sqlite.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启 SQLite 配置恢复事务失败: %w", err)
	}
	defer func() { _ = sqliteTx.Rollback() }()

	log.Printf("[INFO] 开始恢复配置表（共 %d 个表）...", len(configTables))
	for _, table := range configTables {
		if err := sm.restoreTable(ctx, primaryTx, sqliteTx, table); err != nil {
			return fmt.Errorf("恢复表 %s 失败: %w", table, err)
		}
	}
	if err := primaryTx.Commit(); err != nil {
		return fmt.Errorf("提交主库配置快照失败: %w", err)
	}
	if err := sqliteTx.Commit(); err != nil {
		return fmt.Errorf("提交 SQLite 配置恢复事务失败: %w", err)
	}

	log.Printf("[INFO] 配置表恢复完成，耗时: %v", time.Since(start))

	// 第二步：恢复 logs 表（可选，按天数）
	// logDays: -1=全量, 0=不恢复, >0=恢复指定天数
	if logDays != 0 {
		logsStart := time.Now()
		if err := sm.restoreLogsSnapshot(ctx, logDays); err != nil {
			// 日志恢复失败不阻止启动，仅警告
			log.Printf("[WARN] 日志恢复失败: %v（历史日志可能不完整）", err)
		} else {
			log.Printf("[INFO] 日志恢复完成，耗时: %v", time.Since(logsStart))
		}
	}

	log.Printf("[INFO] 数据恢复完成，总耗时: %v", time.Since(start))
	return nil
}

// restoreTable 恢复单表（幂等，DELETE + INSERT）
//
// 关键设计：只恢复 SQLite 和主库都存在的列（交集），避免 schema 不一致时的列数不匹配错误。
// 主库或 SQLite 可能保留历史废弃列，两者不一定完全一致。
type rowsQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func (sm *SyncManager) restoreTable(ctx context.Context, source rowsQueryer, target *sql.Tx, tableName string) error {
	// 1. 获取 SQLite 表的列（目标 schema）
	sqliteCols, err := sm.getTableColumns(ctx, target, tableName)
	if err != nil {
		return fmt.Errorf("获取 SQLite 表列失败: %w", err)
	}
	sqliteColSet := make(map[string]bool, len(sqliteCols))
	for _, col := range sqliteCols {
		sqliteColSet[col] = true
	}

	// 2. 获取主库表的列（源数据）
	sourceCols, err := sm.getTableColumns(ctx, source, tableName)
	if err != nil {
		return fmt.Errorf("获取主库表列失败: %w", err)
	}

	// 3. 计算交集列（只恢复两边都存在的列）
	var commonCols []string
	var sourceColIndices []int // 主库结果集中这些列的索引
	for i, col := range sourceCols {
		if sqliteColSet[col] {
			commonCols = append(commonCols, col)
			sourceColIndices = append(sourceColIndices, i)
		}
	}

	if len(commonCols) == 0 {
		return fmt.Errorf("表 %s 无共同列，无法恢复", tableName)
	}

	// 4. 从主库查询所有列（SELECT * 保持原逻辑）
	query := fmt.Sprintf("SELECT * FROM %s", tableName) //nolint:gosec // G201: 表名来自代码硬编码，非用户输入
	rows, err := source.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("主库查询失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 5. 所有配置表共用一个 SQLite 事务，保证跨表快照原子性。
	deleteQuery := fmt.Sprintf("DELETE FROM %s", tableName) //nolint:gosec // G201: 表名来自代码硬编码
	if _, err := target.ExecContext(ctx, deleteQuery); err != nil {
		return fmt.Errorf("清空 SQLite 表失败: %w", err)
	}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return fmt.Errorf("读取数据失败: %w", err)
		}
		log.Printf("[INFO] 表 %s 已写入恢复事务，共 0 条记录", tableName)
		return nil
	}

	// 6. 逐行写入 SQLite，避免整表数据常驻内存
	// 构建 INSERT 语句（显式列名）
	colNames := strings.Join(commonCols, ", ")
	placeholders := strings.Repeat("?,", len(commonCols))
	placeholders = placeholders[:len(placeholders)-1]                                                // 去掉末尾逗号
	insertQuery := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", tableName, colNames, placeholders) //nolint:gosec // G201: 表名和列名来自代码，非用户输入

	stmt, err := target.PrepareContext(ctx, insertQuery)
	if err != nil {
		return fmt.Errorf("准备插入语句失败: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	totalRestored := 0
	for {
		// 扫描主库所有列
		scanArgs := make([]any, len(sourceCols))
		scanVals := make([]any, len(sourceCols))
		for i := range scanVals {
			scanArgs[i] = &scanVals[i]
		}
		if err := rows.Scan(scanArgs...); err != nil {
			return fmt.Errorf("扫描行失败: %w", err)
		}

		// 只保留交集列的值。MySQL 驱动将 VARCHAR 扫描为 []byte，
		// 需要转为 string，否则 SQLite 会将其绑定为 BLOB。
		record := make([]any, len(commonCols))
		for i, idx := range sourceColIndices {
			val := scanVals[idx]
			if commonCols[i] == "oauth_credential" && val == nil {
				record[i] = ""
				continue
			}
			if b, ok := val.([]byte); ok {
				record[i] = string(b)
			} else {
				record[i] = val
			}
		}

		if _, err := stmt.ExecContext(ctx, record...); err != nil {
			return fmt.Errorf("插入数据失败: %w", err)
		}
		totalRestored++
		if !rows.Next() {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读取数据失败: %w", err)
	}

	log.Printf("[INFO] 表 %s 已写入恢复事务，共 %d 条记录（%d/%d 列）", tableName, totalRestored, len(commonCols), len(sourceCols))
	return nil
}

// getTableColumns 获取表的列名列表
func (sm *SyncManager) getTableColumns(ctx context.Context, store rowsQueryer, tableName string) ([]string, error) {
	// 使用 SELECT * LIMIT 0 获取列信息（跨数据库兼容）
	query := fmt.Sprintf("SELECT * FROM %s LIMIT 0", tableName) //nolint:gosec // G201: 表名来自代码硬编码
	rows, err := store.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return rows.Columns()
}

// restoreLogsSnapshot 使用主库快照重建 SQLite 的日志时间窗口。
//
// 两个数据库独立分配 logs.id，跨库比较 MAX(id) 会同时造成漏行和重复。
// SQLite 是本地副本，恢复时复制主库的业务列但排除 id，由 SQLite 维护自己的本地主键。
// 删除与重建处于同一事务，任何错误都会保留旧快照。
func (sm *SyncManager) restoreLogsSnapshot(ctx context.Context, days int) error {
	var startTime int64
	if days < 0 {
		startTime = 0
		log.Print("[INFO] 准备全量恢复 logs 表快照...")
	} else {
		startTime = time.Now().AddDate(0, 0, -days).UnixMilli()
		log.Printf("[INFO] 准备恢复最近 %d 天的 logs 表快照...", days)
	}

	// 预先计算列映射。id 是每个数据库的本地主键，不属于复制契约。
	sqliteCols, err := sm.getTableColumns(ctx, sm.sqlite, "logs")
	if err != nil {
		return fmt.Errorf("获取 SQLite logs 表列失败: %w", err)
	}
	sqliteColSet := make(map[string]bool, len(sqliteCols))
	for _, col := range sqliteCols {
		sqliteColSet[col] = true
	}

	sourceCols, err := sm.getTableColumns(ctx, sm.primary, "logs")
	if err != nil {
		return fmt.Errorf("获取主库 logs 表列失败: %w", err)
	}

	// 计算交集列
	var commonCols []string
	var sourceColIndices []int
	for i, col := range sourceCols {
		if col != "id" && sqliteColSet[col] {
			commonCols = append(commonCols, col)
			sourceColIndices = append(sourceColIndices, i)
		}
	}

	if len(commonCols) == 0 {
		return fmt.Errorf("logs 表无共同列，无法恢复")
	}

	rows, err := sm.primary.QueryContext(ctx, "SELECT * FROM logs WHERE time >= ?", startTime)
	if err != nil {
		return fmt.Errorf("查询主库日志快照失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	tx, err := sm.sqlite.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启 SQLite 日志恢复事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, "DELETE FROM logs WHERE time >= ?", startTime); err != nil {
		return fmt.Errorf("清理 SQLite 日志窗口失败: %w", err)
	}

	colNames := strings.Join(commonCols, ", ")
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(commonCols)), ",")
	insertQuery := fmt.Sprintf("INSERT INTO logs (%s) VALUES (%s)", colNames, placeholders) //nolint:gosec // G201: 列名来自数据库 schema 交集
	stmt, err := tx.PrepareContext(ctx, insertQuery)
	if err != nil {
		return fmt.Errorf("准备 SQLite 日志恢复语句失败: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	totalRestored := 0
	for rows.Next() {
		scanArgs := make([]any, len(sourceCols))
		scanVals := make([]any, len(sourceCols))
		for i := range scanVals {
			scanArgs[i] = &scanVals[i]
		}

		if err := rows.Scan(scanArgs...); err != nil {
			return fmt.Errorf("扫描主库日志失败: %w", err)
		}

		record := make([]any, len(commonCols))
		for i, idx := range sourceColIndices {
			val := scanVals[idx]
			if b, ok := val.([]byte); ok {
				record[i] = string(b)
			} else {
				record[i] = val
			}
		}
		if _, err := stmt.ExecContext(ctx, record...); err != nil {
			return fmt.Errorf("插入 SQLite 日志快照失败: %w", err)
		}
		totalRestored++
		if totalRestored%50000 == 0 {
			log.Printf("[INFO] 已恢复 %d 条日志...", totalRestored)
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("读取主库日志快照失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 SQLite 日志快照失败: %w", err)
	}

	log.Printf("[INFO] 日志快照恢复完成，共 %d 条（%d/%d 列，不复制 id）", totalRestored, len(commonCols), len(sourceCols))
	return nil
}
