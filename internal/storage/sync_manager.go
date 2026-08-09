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

	// 第二步：首次启动也执行与后续启动相同的日志增量导入。
	if err := sm.RestoreLogsOnStartup(ctx, logDays); err != nil {
		// 日志恢复失败不阻止启动；下一次启动会再次尝试。
		log.Printf("[WARN] 日志导入失败: %v（历史日志可能不完整）", err)
	}

	log.Printf("[INFO] 数据恢复完成，总耗时: %v", time.Since(start))
	return nil
}

// RestoreLogsOnStartup 从主库增量补齐 SQLite 日志尾部。
// SQLite 日志为空时按 logDays 限制首次导入窗口；0 表示明确跳过日志导入。
func (sm *SyncManager) RestoreLogsOnStartup(ctx context.Context, logDays int) error {
	if logDays == 0 {
		return nil
	}

	start := time.Now()
	if err := sm.importLogsAfterSQLiteTail(ctx, logDays); err != nil {
		return err
	}
	log.Printf("[INFO] 日志导入完成，耗时: %v", time.Since(start))
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

// importLogsAfterSQLiteTail 从主库增量导入 SQLite 尾部之后的日志。
// 两个数据库的 logs.id 不在同一命名空间，只能使用日志时间确定增量边界。
func (sm *SyncManager) importLogsAfterSQLiteTail(ctx context.Context, days int) error {
	var windowStart int64
	if days < 0 {
		windowStart = 0
	} else {
		windowStart = time.Now().AddDate(0, 0, -days).UnixMilli()
	}

	var sqliteLastTime int64
	if err := sm.sqlite.QueryRowContext(ctx, "SELECT COALESCE(MAX(time), 0) FROM logs").Scan(&sqliteLastTime); err != nil {
		return fmt.Errorf("查询 SQLite 最后日志时间失败: %w", err)
	}

	query := "SELECT * FROM logs WHERE time >= ? ORDER BY time ASC, id ASC"
	startTime := windowStart
	if sqliteLastTime > 0 {
		startTime = sqliteLastTime
		query = "SELECT * FROM logs WHERE time > ? ORDER BY time ASC, id ASC"
		log.Printf("[INFO] 准备导入 SQLite 最后日志时间之后的数据: %d", sqliteLastTime)
	} else if days < 0 {
		log.Print("[INFO] SQLite 日志为空，准备全量导入 logs...")
	} else {
		log.Printf("[INFO] SQLite 日志为空，准备导入最近 %d 天的 logs...", days)
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
		return fmt.Errorf("logs 表无共同列，无法导入")
	}

	rows, err := sm.primary.QueryContext(ctx, query, startTime)
	if err != nil {
		return fmt.Errorf("查询主库增量日志失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	tx, err := sm.sqlite.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启 SQLite 日志导入事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	colNames := strings.Join(commonCols, ", ")
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(commonCols)), ",")
	insertQuery := fmt.Sprintf("INSERT INTO logs (%s) VALUES (%s)", colNames, placeholders) //nolint:gosec // G201: 列名来自数据库 schema 交集
	stmt, err := tx.PrepareContext(ctx, insertQuery)
	if err != nil {
		return fmt.Errorf("准备 SQLite 日志导入语句失败: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	totalImported := 0
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
			return fmt.Errorf("插入 SQLite 增量日志失败: %w", err)
		}
		totalImported++
		if totalImported%50000 == 0 {
			log.Printf("[INFO] 已导入 %d 条日志...", totalImported)
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("读取主库增量日志失败: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 SQLite 增量日志失败: %w", err)
	}

	log.Printf("[INFO] 日志增量导入完成，共 %d 条（%d/%d 列，不复制 id）", totalImported, len(commonCols), len(sourceCols))
	return nil
}
