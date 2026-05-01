package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// backfillLogsMinuteBucketSQLite 分批回填 logs.minute_bucket（SQLite）
func backfillLogsMinuteBucketSQLite(ctx context.Context, db *sql.DB, batchSize int) error {
	if batchSize <= 0 {
		batchSize = 5_000
	}

	for {
		res, err := db.ExecContext(ctx,
			"UPDATE logs SET minute_bucket = (time / 60000) WHERE id IN (SELECT id FROM logs WHERE minute_bucket = 0 AND time > 0 LIMIT ?)",
			batchSize,
		)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return nil
		}
	}
}

// backfillLogsMinuteBucketMySQL 分批回填 logs.minute_bucket（MySQL）
func backfillLogsMinuteBucketMySQL(ctx context.Context, db *sql.DB, batchSize int) error {
	if batchSize <= 0 {
		batchSize = 10_000
	}

	for {
		res, err := db.ExecContext(ctx,
			"UPDATE logs SET minute_bucket = FLOOR(time / 60000) WHERE minute_bucket = 0 AND time > 0 LIMIT ?",
			batchSize,
		)
		if err != nil {
			return fmt.Errorf("backfill minute_bucket: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return nil
		}
	}
}

// migrateChannelModelsSchema 迁移channel_models表结构
// 版本控制：使用 schema_migrations 表记录已执行的迁移，确保幂等性
// 1. 添加redirect_model字段
// 2. 从channels.models和model_redirects迁移数据到channel_models
// 3. 放宽channels表废弃字段约束(NOT NULL → NULL)，保留兼容性以支持版本回滚
func migrateChannelModelsSchema(ctx context.Context, db *sql.DB, dialect Dialect) error {
	// 检查迁移是否已执行（幂等性保证）
	if applied, err := isMigrationApplied(ctx, db, channelModelsRedirectMigrationVersion); err != nil {
		return fmt.Errorf("check migration status: %w", err)
	} else if applied {
		return nil // 已执行，跳过
	}

	// 第一步：添加redirect_model字段
	if err := ensureChannelModelsRedirectField(ctx, db, dialect); err != nil {
		return err
	}

	// 第二步：从channels.model_redirects迁移数据到channel_models
	if err := migrateModelRedirectsData(ctx, db, dialect); err != nil {
		return err
	}

	// 第三步：放宽channels表废弃字段约束（NOT NULL → NULL）
	if err := relaxDeprecatedChannelFields(ctx, db, dialect); err != nil {
		return err
	}

	// 记录迁移完成
	if err := recordMigration(ctx, db, channelModelsRedirectMigrationVersion, dialect); err != nil {
		log.Printf("[WARN] Failed to record migration %s: %v", channelModelsRedirectMigrationVersion, err)
		// 不阻塞，迁移本身已成功
	}

	return nil
}

// migrateModelRedirectsData 从channels.models和model_redirects迁移数据到channel_models
func migrateModelRedirectsData(ctx context.Context, db *sql.DB, dialect Dialect) error {
	// 检查是否需要迁移
	needMigration, err := needChannelModelsMigration(ctx, db, dialect)
	if err != nil {
		return err
	}
	if !needMigration {
		return nil
	}

	// 查询所有需要迁移的渠道（有models数据）
	// 注意：必须同时查询 models 和 model_redirects
	rows, err := db.QueryContext(ctx,
		"SELECT id, created_at, models, model_redirects FROM channels WHERE models != '' AND models != '[]'")
	if err != nil {
		return fmt.Errorf("query channels for migration: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 收集所有待迁移的数据
	type modelEntry struct {
		channelID     int64
		model         string
		redirectModel string
		createdAt     int64
	}
	var entries []modelEntry
	var channelIDs []int64

	for rows.Next() {
		var channelID int64
		var channelCreatedAt int64
		var modelsJSON, redirectsJSON string
		if err := rows.Scan(&channelID, &channelCreatedAt, &modelsJSON, &redirectsJSON); err != nil {
			return fmt.Errorf("scan channel data: %w", err)
		}

		// [FIX] P2: 解析 models JSON 数组，失败时中断迁移（Fail-Fast）
		models, err := parseModelsForMigration(modelsJSON)
		if err != nil {
			return fmt.Errorf("channel %d: %w", channelID, err)
		}
		if len(models) == 0 {
			continue
		}

		// 只有解析成功才记录 channelID（避免解析失败的渠道被重命名字段后丢失数据）
		channelIDs = append(channelIDs, channelID)

		// 解析 model_redirects JSON 对象
		redirects, _ := parseModelRedirectsForMigration(redirectsJSON)
		if redirects == nil {
			redirects = make(map[string]string)
		}

		baseCreatedAt := channelCreatedAt * 1000
		if baseCreatedAt <= 0 {
			baseCreatedAt = time.Now().UnixMilli()
		}

		// 构建条目：每个模型一条记录
		for i, model := range models {
			entries = append(entries, modelEntry{
				channelID:     channelID,
				model:         model,
				redirectModel: redirects[model], // 如果没有重定向则为空
				createdAt:     baseCreatedAt + int64(i),
			})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// 无数据需要迁移
	if len(channelIDs) == 0 {
		return nil
	}

	// 使用事务批量执行
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 插入或更新 channel_models
	for _, e := range entries {
		var upsertSQL string
		if dialect == DialectMySQL {
			upsertSQL = `INSERT INTO channel_models (channel_id, model, redirect_model, created_at)
				VALUES (?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE redirect_model = VALUES(redirect_model), created_at = VALUES(created_at)`
		} else {
			upsertSQL = `INSERT INTO channel_models (channel_id, model, redirect_model, created_at)
				VALUES (?, ?, ?, ?)
				ON CONFLICT(channel_id, model) DO UPDATE SET redirect_model = excluded.redirect_model, created_at = excluded.created_at`
		}
		if _, err := tx.ExecContext(ctx, upsertSQL, e.channelID, e.model, e.redirectModel, e.createdAt); err != nil {
			return fmt.Errorf("upsert channel_model: %w", err)
		}
	}

	// 数据迁移完成，字段约束放宽在 relaxDeprecatedChannelFields 中处理
	return tx.Commit()
}

func repairLegacyChannelModelOrder(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if hasMigration(ctx, db, channelModelsOrderRepairVersion) {
		return nil
	}

	appliedAt, ok, err := migrationAppliedAt(ctx, db, channelModelsRedirectMigrationVersion)
	if err != nil {
		return err
	}
	if !ok {
		return recordMigration(ctx, db, channelModelsOrderRepairVersion, dialect)
	}

	needRepair, err := needChannelModelsMigration(ctx, db, dialect)
	if err != nil {
		return err
	}
	if !needRepair {
		return recordMigration(ctx, db, channelModelsOrderRepairVersion, dialect)
	}

	rows, err := db.QueryContext(ctx, `
		SELECT id, created_at, models, model_redirects
		FROM channels
		WHERE models IS NOT NULL AND models != '' AND models != '[]' AND updated_at <= ?
	`, appliedAt)
	if err != nil {
		return fmt.Errorf("query legacy channel order candidates: %w", err)
	}

	type legacyOrderCandidate struct {
		channelID        int64
		channelCreatedAt int64
		modelsJSON       string
		redirectsJSON    string
	}
	candidates := make([]legacyOrderCandidate, 0)
	for rows.Next() {
		var candidate legacyOrderCandidate
		if err := rows.Scan(&candidate.channelID, &candidate.channelCreatedAt, &candidate.modelsJSON, &candidate.redirectsJSON); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan legacy channel order candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate legacy channel order candidates: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close legacy channel order candidates: %w", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin legacy order repair tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	updateStmt, err := tx.PrepareContext(ctx, `UPDATE channel_models SET created_at = ? WHERE channel_id = ? AND model = ?`)
	if err != nil {
		return fmt.Errorf("prepare legacy order repair update: %w", err)
	}
	defer func() { _ = updateStmt.Close() }()

	for _, candidate := range candidates {
		desiredOrder, err := parseModelsForMigration(candidate.modelsJSON)
		if err != nil {
			return fmt.Errorf("channel %d: %w", candidate.channelID, err)
		}
		if len(desiredOrder) == 0 {
			continue
		}

		desiredRedirects, err := parseModelRedirectsForMigration(candidate.redirectsJSON)
		if err != nil {
			return fmt.Errorf("channel %d parse model_redirects JSON: %w", candidate.channelID, err)
		}
		if !legacyChannelModelsNeedOrderRepair(ctx, tx, candidate.channelID, desiredOrder, desiredRedirects) {
			continue
		}

		baseCreatedAt := candidate.channelCreatedAt * 1000
		if baseCreatedAt <= 0 {
			baseCreatedAt = appliedAt * 1000
		}
		for i, modelName := range desiredOrder {
			if _, err := updateStmt.ExecContext(ctx, baseCreatedAt+int64(i), candidate.channelID, modelName); err != nil {
				return fmt.Errorf("repair channel %d model order for %s: %w", candidate.channelID, modelName, err)
			}
		}
	}

	if err := recordMigrationTx(ctx, tx, channelModelsOrderRepairVersion, dialect); err != nil {
		return err
	}
	return tx.Commit()
}

func legacyChannelModelsNeedOrderRepair(ctx context.Context, tx *sql.Tx, channelID int64, desiredOrder []string, desiredRedirects map[string]string) bool {
	rows, err := tx.QueryContext(ctx, `
		SELECT model, redirect_model
		FROM channel_models
		WHERE channel_id = ?
		ORDER BY created_at ASC, model ASC
	`, channelID)
	if err != nil {
		return false
	}
	defer func() { _ = rows.Close() }()

	currentOrder := make([]string, 0, len(desiredOrder))
	currentRedirects := make(map[string]string, len(desiredOrder))
	for rows.Next() {
		var modelName, redirectModel string
		if err := rows.Scan(&modelName, &redirectModel); err != nil {
			return false
		}
		currentOrder = append(currentOrder, modelName)
		currentRedirects[modelName] = redirectModel
	}
	if err := rows.Err(); err != nil || len(currentOrder) != len(desiredOrder) {
		return false
	}

	for i, modelName := range desiredOrder {
		currentRedirect, ok := currentRedirects[modelName]
		if !ok {
			return false
		}
		if currentRedirect != desiredRedirects[modelName] {
			return false
		}
		if currentOrder[i] != modelName {
			return true
		}
	}

	return false
}

// needChannelModelsMigration 检查是否需要迁移
// 检查 channels.models 字段是否存在（未被重命名为 _deprecated_models）
func needChannelModelsMigration(ctx context.Context, db *sql.DB, dialect Dialect) (bool, error) {
	if dialect == DialectMySQL {
		// MySQL: 检查 models 字段是否存在
		var count int
		err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='channels' AND COLUMN_NAME='models'",
		).Scan(&count)
		if err != nil {
			return false, fmt.Errorf("check models column: %w", err)
		}
		return count > 0, nil
	}

	// SQLite: 检查 models 字段是否存在
	existingCols, err := sqliteExistingColumns(ctx, db, "channels")
	if err != nil {
		return false, nil // 表不存在或其他错误，视为无需迁移
	}
	return existingCols["models"], nil
}

// parseModelsForMigration 解析 models JSON 数组用于迁移
// [FIX] P2: 解析失败返回错误而非静默忽略，避免数据丢失
func parseModelsForMigration(jsonStr string) ([]string, error) {
	if jsonStr == "" || jsonStr == "[]" {
		return nil, nil
	}
	var models []string
	if err := json.Unmarshal([]byte(jsonStr), &models); err != nil {
		return nil, fmt.Errorf("parse models JSON %q: %w", jsonStr, err)
	}
	return models, nil
}

// parseModelRedirectsForMigration 解析model_redirects JSON用于迁移
func parseModelRedirectsForMigration(jsonStr string) (map[string]string, error) {
	if jsonStr == "" || jsonStr == "{}" {
		return nil, nil
	}
	var redirects map[string]string
	if err := json.Unmarshal([]byte(jsonStr), &redirects); err != nil {
		return nil, fmt.Errorf("parse model_redirects JSON: %w", err)
	}
	return redirects, nil
}

// relaxDeprecatedChannelFields 放宽channels表废弃字段的约束
// 将 models 和 model_redirects 从 NOT NULL 改为允许 NULL
// 这样新版程序 INSERT 时不提供这些字段也不会报错，同时保留字段名以支持版本回滚
func relaxDeprecatedChannelFields(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if dialect == DialectMySQL {
		// MySQL: 使用 MODIFY COLUMN 去除 NOT NULL
		var count int
		err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='channels' AND COLUMN_NAME='models'",
		).Scan(&count)
		if err != nil {
			return fmt.Errorf("check models field: %w", err)
		}
		if count > 0 {
			if _, err := db.ExecContext(ctx,
				"ALTER TABLE channels MODIFY COLUMN models TEXT NULL"); err != nil {
				return fmt.Errorf("modify models column: %w", err)
			}
			log.Printf("[MIGRATE] Modified channels.models: NOT NULL → NULL")
		}

		err = db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='channels' AND COLUMN_NAME='model_redirects'",
		).Scan(&count)
		if err != nil {
			return fmt.Errorf("check model_redirects field: %w", err)
		}
		if count > 0 {
			if _, err := db.ExecContext(ctx,
				"ALTER TABLE channels MODIFY COLUMN model_redirects TEXT NULL"); err != nil {
				return fmt.Errorf("modify model_redirects column: %w", err)
			}
			log.Printf("[MIGRATE] Modified channels.model_redirects: NOT NULL → NULL")
		}
		return nil
	}

	// SQLite: 不支持直接修改列约束，但 TEXT 类型天然允许 NULL
	// SQLite 的 NOT NULL 约束只在显式 INSERT 该列时检查
	// 新版程序 INSERT 语句不包含这些列，SQLite 会使用默认值（NULL）
	return nil
}

func validateAuthTokensAllowedModelsJSON(ctx context.Context, db *sql.DB) error {
	return validateJSONColumn(ctx, db, "auth_tokens", "allowed_models", func(raw string) error {
		var models []string
		return json.Unmarshal([]byte(raw), &models)
	})
}

func validateAuthTokensAllowedChannelIDsJSON(ctx context.Context, db *sql.DB) error {
	return validateJSONColumn(ctx, db, "auth_tokens", "allowed_channel_ids", func(raw string) error {
		var channelIDs []int64
		return json.Unmarshal([]byte(raw), &channelIDs)
	})
}

// validateJSONColumn 校验给定字符串列的非空行均为合法 JSON。
// parser 由调用方提供（决定预期 JSON 类型，例如 []string 或 []int64）。
// 任一行解析失败即返回错误，错误信息包含修复 SQL 提示，禁止脏数据静默放权。
//
// 安全注意：table/col 仅来自内部代码硬编码字面量，不接受外部输入；fmt.Sprintf 拼接是安全的。
func validateJSONColumn(ctx context.Context, db *sql.DB, table, col string, parser func(raw string) error) error {
	//nolint:gosec // G201: table/col 由内部代码控制，非用户输入
	query := fmt.Sprintf("SELECT id, %s FROM %s WHERE %s <> ''", col, table, col)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("query %s.%s: %w", table, col, err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return fmt.Errorf("scan %s.%s: %w", table, col, err)
		}
		// SQLite BLOB 类型亲和性可能导致 WHERE <> '' 过滤失效，显式跳过空字符串
		if raw == "" {
			continue
		}
		if err := parser(raw); err != nil {
			return fmt.Errorf(
				"%s.%s invalid json: id=%d %s=%q: %w (fix: UPDATE %s SET %s='' WHERE id=%d)",
				table, col, id, col, raw, err, table, col, id,
			)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate %s.%s: %w", table, col, err)
	}
	return nil
}

func validateAuthTokensMaxConcurrency(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT id, max_concurrency FROM auth_tokens WHERE max_concurrency < 0")
	if err != nil {
		return fmt.Errorf("query auth_tokens.max_concurrency: %w", err)
	}
	defer func() { _ = rows.Close() }()

	if rows.Next() {
		var id int64
		var maxConcurrency int64
		if err := rows.Scan(&id, &maxConcurrency); err != nil {
			return fmt.Errorf("scan auth_tokens.max_concurrency: %w", err)
		}
		return fmt.Errorf(
			"auth_tokens.max_concurrency must be >= 0: id=%d max_concurrency=%d (fix: UPDATE auth_tokens SET max_concurrency=0 WHERE id=%d)",
			id, maxConcurrency, id,
		)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate auth_tokens.max_concurrency: %w", err)
	}
	return nil
}

// rebuildDebugLogsPrimaryKey 将 debug_logs 旧结构（id 自增主键 + log_id 列）
// 迁移为新结构（log_id 作为主键）。因调试日志保留期极短（默认5分钟），
// 直接 DROP 旧表由后续 CREATE TABLE IF NOT EXISTS 重建即可
const debugLogsPKRebuildVersion = "v1_debug_logs_pk_log_id"

func rebuildDebugLogsPrimaryKey(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if hasMigration(ctx, db, debugLogsPKRebuildVersion) {
		return nil
	}

	// 检查旧表是否存在且包含 id 列（新部署首次创建时跳过 DROP）
	hasLegacy, err := debugLogsHasLegacyIDColumn(ctx, db, dialect)
	if err != nil {
		return err
	}
	if hasLegacy {
		if _, err := db.ExecContext(ctx, "DROP TABLE debug_logs"); err != nil {
			return fmt.Errorf("drop legacy debug_logs: %w", err)
		}
		log.Printf("[MIGRATE] Dropped legacy debug_logs table (id-based PK) for rebuild")
	}

	return recordMigration(ctx, db, debugLogsPKRebuildVersion, dialect)
}

// relaxDebugLogsRespBodyNullable 将 debug_logs.resp_body 从 NOT NULL 放宽为可空
// （部分请求尚未拿到响应体就写入，NOT NULL 约束会导致批量写入失败）。
// 调试日志保留期极短，直接 DROP 重建，不迁移旧数据。
const debugLogsRespBodyNullableVersion = "v2_debug_logs_resp_body_nullable"

func relaxDebugLogsRespBodyNullable(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if hasMigration(ctx, db, debugLogsRespBodyNullableVersion) {
		return nil
	}
	if _, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS debug_logs"); err != nil {
		return fmt.Errorf("drop debug_logs for resp_body relax: %w", err)
	}
	log.Printf("[MIGRATE] Dropped debug_logs table to relax resp_body NOT NULL constraint")
	return recordMigration(ctx, db, debugLogsRespBodyNullableVersion, dialect)
}

func debugLogsHasLegacyIDColumn(ctx context.Context, db *sql.DB, dialect Dialect) (bool, error) {
	if dialect == DialectMySQL {
		var count int
		err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='debug_logs' AND COLUMN_NAME='id'",
		).Scan(&count)
		if err != nil {
			return false, fmt.Errorf("check debug_logs.id existence: %w", err)
		}
		return count > 0, nil
	}

	// SQLite: 表不存在时 PRAGMA 返回空结果集，视为无旧列
	existing, err := sqliteExistingColumns(ctx, db, "debug_logs")
	if err != nil {
		return false, nil
	}
	return existing["id"], nil
}
