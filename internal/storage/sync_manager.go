package storage

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	sqlstore "ccLoad/internal/storage/sql"
)

// SyncManager 负责启动时从 MySQL 恢复数据到 SQLite
//
// 核心职责：
// - 启动时从 MySQL 恢复数据到 SQLite
// - 配置表全量恢复（~500 条数据，<1 秒）
// - logs 表按天数增量恢复（分批处理，避免内存溢出）
// - **无超时机制**：恢复失败直接返回错误，降级到纯 MySQL
//
// 设计原则：
// - KISS：简单的单向数据复制，无复杂一致性
// - Fail-Fast：恢复失败直接退出，不降级
type SyncManager struct {
	mysql  *sqlstore.SQLStore
	sqlite *sqlstore.SQLStore
}

// NewSyncManager 创建同步管理器
func NewSyncManager(mysql, sqlite *sqlstore.SQLStore) *SyncManager {
	return &SyncManager{
		mysql:  mysql,
		sqlite: sqlite,
	}
}

// RestoreOnStartup 启动时恢复数据（从 MySQL 恢复到 SQLite）
//
// logDays 参数：
//   - 0 = 仅恢复配置表，不恢复 logs
//   - 7 = 恢复配置表 + 最近 7 天 logs
//   - 999 = 全量恢复（慎用，启动慢）
func (sm *SyncManager) RestoreOnStartup(ctx context.Context, logDays int) error {
	start := time.Now()

	// 第一步：恢复配置表（快速，<1 秒）
	configTables := []string{
		"system_settings",
		"channels",
		"channel_models",
		"api_keys",
		"auth_tokens",
	}

	log.Printf("[INFO] 开始恢复配置表（共 %d 个表）...", len(configTables))
	for _, table := range configTables {
		if err := sm.restoreTable(ctx, table); err != nil {
			return fmt.Errorf("恢复表 %s 失败: %w", table, err)
		}
	}

	log.Printf("[INFO] 配置表恢复完成，耗时: %v", time.Since(start))

	// 第二步：恢复 logs 表（可选，按天数）
	if logDays > 0 {
		logsStart := time.Now()
		if err := sm.restoreLogsIncremental(ctx, logDays); err != nil {
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
// 配置表数据量限制：最多 10000 行，超过则报错（防止内存溢出）
//
// 关键设计：只恢复 SQLite 和 MySQL 都存在的列（交集），避免 schema 不一致时的列数不匹配错误。
// MySQL 可能有历史遗留列或新增列，SQLite 按最新 schema 创建，两者不一定完全一致。
func (sm *SyncManager) restoreTable(ctx context.Context, tableName string) error {
	const maxConfigRows = 10000 // 配置表最大行数限制

	// 1. 先检查行数，防止内存溢出
	var rowCount int64
	countQuery := fmt.Sprintf("SELECT COUNT(*) FROM %s", tableName) //nolint:gosec // G201: 表名来自代码硬编码
	if err := sm.mysql.QueryRowContext(ctx, countQuery).Scan(&rowCount); err != nil {
		return fmt.Errorf("统计行数失败: %w", err)
	}
	if rowCount > maxConfigRows {
		return fmt.Errorf("表 %s 行数 %d 超过限制 %d，请检查数据或使用分批恢复", tableName, rowCount, maxConfigRows)
	}

	// 2. 获取 SQLite 表的列（目标 schema）
	sqliteCols, err := sm.getTableColumns(ctx, sm.sqlite, tableName)
	if err != nil {
		return fmt.Errorf("获取 SQLite 表列失败: %w", err)
	}
	sqliteColSet := make(map[string]bool, len(sqliteCols))
	for _, col := range sqliteCols {
		sqliteColSet[col] = true
	}

	// 3. 获取 MySQL 表的列（源数据）
	mysqlCols, err := sm.getTableColumns(ctx, sm.mysql, tableName)
	if err != nil {
		return fmt.Errorf("获取 MySQL 表列失败: %w", err)
	}

	// 4. 计算交集列（只恢复两边都存在的列）
	var commonCols []string
	var mysqlColIndices []int // MySQL 结果集中这些列的索引
	for i, col := range mysqlCols {
		if sqliteColSet[col] {
			commonCols = append(commonCols, col)
			mysqlColIndices = append(mysqlColIndices, i)
		}
	}

	if len(commonCols) == 0 {
		return fmt.Errorf("表 %s 无共同列，无法恢复", tableName)
	}

	// 5. 从 MySQL 查询所有列（SELECT * 保持原逻辑）
	query := fmt.Sprintf("SELECT * FROM %s", tableName) //nolint:gosec // G201: 表名来自代码硬编码，非用户输入
	rows, err := sm.mysql.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("MySQL 查询失败: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 6. 读取数据，只提取交集列
	var records [][]any
	for rows.Next() {
		// 扫描 MySQL 所有列
		scanArgs := make([]any, len(mysqlCols))
		scanVals := make([]any, len(mysqlCols))
		for i := range scanVals {
			scanArgs[i] = &scanVals[i]
		}

		if err := rows.Scan(scanArgs...); err != nil {
			return fmt.Errorf("扫描行失败: %w", err)
		}

		// 只保留交集列的值
		record := make([]any, len(commonCols))
		for i, idx := range mysqlColIndices {
			record[i] = scanVals[idx]
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("读取数据失败: %w", err)
	}

	if len(records) == 0 {
		log.Printf("[INFO] 表 %s 为空，跳过恢复", tableName)
		return nil
	}

	// 7. 清空 SQLite 表
	deleteQuery := fmt.Sprintf("DELETE FROM %s", tableName)
	if _, err := sm.sqlite.ExecContext(ctx, deleteQuery); err != nil {
		return fmt.Errorf("清空 SQLite 表失败: %w", err)
	}

	// 8. 批量插入 SQLite（显式指定列名）
	tx, err := sm.sqlite.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 构建 INSERT 语句（显式列名）
	colNames := strings.Join(commonCols, ", ")
	placeholders := strings.Repeat("?,", len(commonCols))
	placeholders = placeholders[:len(placeholders)-1]                                                // 去掉末尾逗号
	insertQuery := fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", tableName, colNames, placeholders) //nolint:gosec // G201: 表名和列名来自代码，非用户输入

	stmt, err := tx.Prepare(insertQuery)
	if err != nil {
		return fmt.Errorf("准备插入语句失败: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, record := range records {
		if _, err := stmt.Exec(record...); err != nil {
			return fmt.Errorf("插入数据失败: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}

	log.Printf("[INFO] 表 %s 恢复完成，共 %d 条记录（%d/%d 列）", tableName, len(records), len(commonCols), len(mysqlCols))
	return nil
}

// getTableColumns 获取表的列名列表
func (sm *SyncManager) getTableColumns(ctx context.Context, store *sqlstore.SQLStore, tableName string) ([]string, error) {
	// 使用 SELECT * LIMIT 0 获取列信息（跨数据库兼容）
	query := fmt.Sprintf("SELECT * FROM %s LIMIT 0", tableName) //nolint:gosec // G201: 表名来自代码硬编码
	rows, err := store.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return rows.Columns()
}

// restoreLogsIncremental 增量恢复 logs 表（最近 N 天）
func (sm *SyncManager) restoreLogsIncremental(ctx context.Context, days int) error {
	var startTime int64
	if days >= 999 {
		startTime = 0 // 全量恢复
		log.Print("[INFO] 准备全量恢复 logs 表（可能耗时较长）...")
	} else {
		startTime = time.Now().AddDate(0, 0, -days).UnixMilli()
		log.Printf("[INFO] 准备恢复最近 %d 天的日志...", days)
	}

	// 计数预估
	var count int64
	countQuery := "SELECT COUNT(*) FROM logs WHERE time >= ?"
	if err := sm.mysql.QueryRowContext(ctx, countQuery, startTime).Scan(&count); err != nil {
		return fmt.Errorf("统计日志数量失败: %w", err)
	}

	if count == 0 {
		log.Print("[INFO] 无日志需要恢复")
		return nil
	}

	log.Printf("[INFO] 预计恢复 %d 条日志", count)

	// 预先计算列映射（只计算一次）
	sqliteCols, err := sm.getTableColumns(ctx, sm.sqlite, "logs")
	if err != nil {
		return fmt.Errorf("获取 SQLite logs 表列失败: %w", err)
	}
	sqliteColSet := make(map[string]bool, len(sqliteCols))
	for _, col := range sqliteCols {
		sqliteColSet[col] = true
	}

	mysqlCols, err := sm.getTableColumns(ctx, sm.mysql, "logs")
	if err != nil {
		return fmt.Errorf("获取 MySQL logs 表列失败: %w", err)
	}

	// 计算交集列
	var commonCols []string
	var mysqlColIndices []int
	for i, col := range mysqlCols {
		if sqliteColSet[col] {
			commonCols = append(commonCols, col)
			mysqlColIndices = append(mysqlColIndices, i)
		}
	}

	if len(commonCols) == 0 {
		return fmt.Errorf("logs 表无共同列，无法恢复")
	}

	// 清空 SQLite logs 表
	if _, err := sm.sqlite.ExecContext(ctx, "DELETE FROM logs"); err != nil {
		return fmt.Errorf("清空 SQLite logs 表失败: %w", err)
	}

	// 分批恢复（避免内存溢出）
	const batchSize = 5000
	offset := 0

	for {
		// 查询一批数据
		query := "SELECT * FROM logs WHERE time >= ? ORDER BY id LIMIT ? OFFSET ?"
		rows, err := sm.mysql.QueryContext(ctx, query, startTime, batchSize, offset)
		if err != nil {
			return fmt.Errorf("查询日志失败: %w", err)
		}

		// 读取批次并插入（传入列映射）
		batchCount, err := sm.insertLogBatch(ctx, rows, len(mysqlCols), commonCols, mysqlColIndices)
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("批量插入日志失败: %w", err)
		}

		if batchCount == 0 {
			break
		}

		offset += batchCount

		// 进度提示
		if offset%50000 == 0 {
			log.Printf("[INFO] 已恢复 %d 条日志...", offset)
		}

		// 如果读取的数量小于批次大小，说明已经读完
		if batchCount < batchSize {
			break
		}
	}

	log.Printf("[INFO] 日志恢复完成，共 %d 条（%d/%d 列）", offset, len(commonCols), len(mysqlCols))
	return nil
}

// insertLogBatch 批量插入日志到 SQLite
// mysqlColCount: MySQL 结果集的列数
// commonCols: 交集列名列表
// mysqlColIndices: 交集列在 MySQL 结果集中的索引
func (sm *SyncManager) insertLogBatch(ctx context.Context, rows interface {
	Next() bool
	Scan(...any) error
}, mysqlColCount int, commonCols []string, mysqlColIndices []int) (int, error) {
	// 读取所有数据到内存，只保留交集列
	var records [][]any
	for rows.Next() {
		// 扫描 MySQL 所有列
		scanArgs := make([]any, mysqlColCount)
		scanVals := make([]any, mysqlColCount)
		for i := range scanVals {
			scanArgs[i] = &scanVals[i]
		}

		if err := rows.Scan(scanArgs...); err != nil {
			return 0, fmt.Errorf("扫描行失败: %w", err)
		}

		// 只保留交集列的值
		record := make([]any, len(commonCols))
		for i, idx := range mysqlColIndices {
			record[i] = scanVals[idx]
		}
		records = append(records, record)
	}

	if len(records) == 0 {
		return 0, nil
	}

	// 批量插入 SQLite
	tx, err := sm.sqlite.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 构建 INSERT 语句（显式列名）
	colNames := strings.Join(commonCols, ", ")
	placeholders := strings.Repeat("?,", len(commonCols))
	placeholders = placeholders[:len(placeholders)-1]                                       // 去掉末尾逗号
	insertQuery := fmt.Sprintf("INSERT INTO logs (%s) VALUES (%s)", colNames, placeholders) //nolint:gosec // G201: 列名来自代码，非用户输入

	stmt, err := tx.Prepare(insertQuery)
	if err != nil {
		return 0, fmt.Errorf("准备插入语句失败: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, record := range records {
		if _, err := stmt.Exec(record...); err != nil {
			return 0, fmt.Errorf("插入数据失败: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交事务失败: %w", err)
	}

	return len(records), nil
}
