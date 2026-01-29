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
	// logDays: -1=全量, 0=不恢复, >0=恢复指定天数
	if logDays != 0 {
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
		// 注意：MySQL 驱动将 VARCHAR 扫描为 []byte，需要转换为 string
		// 否则 SQLite 驱动会将 []byte 绑定为 BLOB（类型亲和性问题）
		record := make([]any, len(commonCols))
		for i, idx := range mysqlColIndices {
			val := scanVals[idx]
			// 将 []byte 转为 string（MySQL VARCHAR -> Go string）
			if b, ok := val.([]byte); ok {
				record[i] = string(b)
			} else {
				record[i] = val
			}
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

	// 7. 清空 + 插入必须在同一个事务里，保证原子性
	tx, err := sm.sqlite.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	deleteQuery := fmt.Sprintf("DELETE FROM %s", tableName) //nolint:gosec // G201: 表名来自代码硬编码
	if _, err := tx.ExecContext(ctx, deleteQuery); err != nil {
		return fmt.Errorf("清空 SQLite 表失败: %w", err)
	}

	// 8. 批量插入 SQLite（显式指定列名）
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

// restoreLogsIncremental 增量恢复 logs 表（基于 id 增量同步）
//
// 设计：不删除 SQLite 现有数据，只拉取 id > MAX(sqlite.id) 的新记录
// 优势：
//   - SQLite 为空时（HuggingFace 重启）：MAX(id)=0，等价于全量恢复
//   - SQLite 有数据时（程序重启）：只拉取增量，启动更快
//   - 避免 DELETE 导致的数据丢失风险
func (sm *SyncManager) restoreLogsIncremental(ctx context.Context, days int) error {
	// 1. 获取 SQLite 中最大的 id（为空时返回 0）
	var maxID int64
	if err := sm.sqlite.QueryRowContext(ctx, "SELECT COALESCE(MAX(id), 0) FROM logs").Scan(&maxID); err != nil {
		return fmt.Errorf("获取 SQLite 最大 ID 失败: %w", err)
	}

	// 2. 计算时间范围
	var startTime int64
	if days < 0 {
		startTime = 0 // 全量恢复
		log.Printf("[INFO] 准备增量恢复 logs 表（从 id > %d）...", maxID)
	} else {
		startTime = time.Now().AddDate(0, 0, -days).UnixMilli()
		log.Printf("[INFO] 准备增量恢复最近 %d 天的日志（从 id > %d）...", days, maxID)
	}

	// 3. 统计需要恢复的数量
	var count int64
	countQuery := "SELECT COUNT(*) FROM logs WHERE id > ? AND time >= ?"
	if err := sm.mysql.QueryRowContext(ctx, countQuery, maxID, startTime).Scan(&count); err != nil {
		return fmt.Errorf("统计日志数量失败: %w", err)
	}

	if count == 0 {
		if maxID > 0 {
			log.Print("[INFO] SQLite 日志已是最新，无需恢复")
		} else {
			log.Print("[INFO] MySQL 无日志需要恢复")
		}
		return nil
	}

	log.Printf("[INFO] 预计恢复 %d 条日志", count)

	// 4. 预先计算列映射（只计算一次）
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

	// 5. 分批增量恢复（基于 id 游标，避免 OFFSET 性能问题）
	const batchSize = 5000
	lastID := maxID
	totalRestored := 0

	for {
		// 查询一批数据（id > lastID，无需 OFFSET）
		query := "SELECT * FROM logs WHERE id > ? AND time >= ? ORDER BY id LIMIT ?"
		rows, err := sm.mysql.QueryContext(ctx, query, lastID, startTime, batchSize)
		if err != nil {
			return fmt.Errorf("查询日志失败: %w", err)
		}

		// 读取批次并插入（传入列映射）
		batchCount, batchLastID, err := sm.insertLogBatchWithLastID(ctx, rows, len(mysqlCols), commonCols, mysqlColIndices)
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("批量插入日志失败: %w", err)
		}

		if batchCount == 0 {
			break
		}

		lastID = batchLastID
		totalRestored += batchCount

		// 进度提示
		if totalRestored%50000 == 0 {
			log.Printf("[INFO] 已恢复 %d 条日志...", totalRestored)
		}

		// 如果读取的数量小于批次大小，说明已经读完
		if batchCount < batchSize {
			break
		}
	}

	log.Printf("[INFO] 日志恢复完成，共 %d 条（%d/%d 列）", totalRestored, len(commonCols), len(mysqlCols))
	return nil
}

// insertLogBatchWithLastID 批量插入日志到 SQLite，返回插入数量和最后一条记录的 ID
// mysqlColCount: MySQL 结果集的列数
// commonCols: 交集列名列表
// mysqlColIndices: 交集列在 MySQL 结果集中的索引
func (sm *SyncManager) insertLogBatchWithLastID(ctx context.Context, rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}, mysqlColCount int, commonCols []string, mysqlColIndices []int) (count int, lastID int64, err error) {
	// 找到 id 列在 commonCols 中的索引
	idColIdx := -1
	for i, col := range commonCols {
		if col == "id" {
			idColIdx = i
			break
		}
	}
	if idColIdx < 0 {
		return 0, 0, fmt.Errorf("commonCols 中缺少 id 列")
	}

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
			return 0, 0, fmt.Errorf("扫描行失败: %w", err)
		}

		// 只保留交集列的值
		// 注意：MySQL 驱动将 VARCHAR 扫描为 []byte，需要转换为 string
		// 否则 SQLite 驱动会将 []byte 绑定为 BLOB（类型亲和性问题）
		record := make([]any, len(commonCols))
		for i, idx := range mysqlColIndices {
			val := scanVals[idx]
			if b, ok := val.([]byte); ok {
				record[i] = string(b)
			} else {
				record[i] = val
			}
		}
		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("读取日志失败: %w", err)
	}

	if len(records) == 0 {
		return 0, 0, nil
	}

	// 提取最后一条记录的 ID
	lastRecord := records[len(records)-1]
	switch v := lastRecord[idColIdx].(type) {
	case int64:
		lastID = v
	case int:
		lastID = int64(v)
	default:
		return 0, 0, fmt.Errorf("无法解析 id 列值: %T", v)
	}

	// 批量插入 SQLite
	tx, err := sm.sqlite.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 构建 INSERT 语句（显式列名）
	colNames := strings.Join(commonCols, ", ")
	placeholders := strings.Repeat("?,", len(commonCols))
	placeholders = placeholders[:len(placeholders)-1]                                       // 去掉末尾逗号
	insertQuery := fmt.Sprintf("INSERT INTO logs (%s) VALUES (%s)", colNames, placeholders) //nolint:gosec // G201: 列名来自代码，非用户输入

	stmt, err := tx.Prepare(insertQuery)
	if err != nil {
		return 0, 0, fmt.Errorf("准备插入语句失败: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for _, record := range records {
		if _, err := stmt.Exec(record...); err != nil {
			return 0, 0, fmt.Errorf("插入数据失败: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("提交事务失败: %w", err)
	}

	return len(records), lastID, nil
}
