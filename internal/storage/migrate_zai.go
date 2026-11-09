package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"ccLoad/internal/model"
	"ccLoad/internal/zaiauth"
)

/**
 * @brief 付费官方端点与手动禁用状态一并迁移的幂等版本标记。
 */
const zaiDirectURLsMigrationVersion = "v1_zai_direct_coding_plan_urls"

/**
 * @brief 一次性迁移 ZCode 旧官方付费 URL，并保留各地址的手动禁用状态。
 * @param ctx 启动迁移上下文。
 * @param db 已建立 channels 和 channel_url_states 的配置数据库。
 * @param dialect 当前数据库方言。
 * @return 读取、解析或事务提交失败时返回错误，失败不会记录迁移完成。
 * @note 仅修改 zai_oauth 渠道的已知官方地址，凭证、自定义 URL 和其他渠道不变。
 */
func migrateZAICodingPlanURLs(ctx context.Context, db *sql.DB, dialect Dialect) error {
	if hasMigration(ctx, db, zaiDirectURLsMigrationVersion, dialect) {
		return nil
	}
	rows, err := db.QueryContext(ctx, "SELECT id, url FROM channels WHERE auth_type = 'zai_oauth' ORDER BY id")
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	type candidate struct {
		id      int64
		encoded string
		urls    map[string]string
	}
	var candidates []candidate
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		var urls model.ChannelURLs
		if err := json.Unmarshal([]byte(raw), &urls); err != nil {
			return fmt.Errorf("channel %d URLs: %w", id, err)
		}
		changed := make(map[string]string)
		for i := range urls {
			old := urls[i].RuntimeURL()
			urls[i].URL = zaiauth.NormalizeCodingPlanURL(urls[i].URL)
			if next := urls[i].RuntimeURL(); next != old {
				changed[old] = next
			}
		}
		if len(changed) == 0 {
			continue
		}
		// 新旧地址可能同时存在；保留首项顺序并合并协议，自动探测覆盖有限声明。
		merged := make(model.ChannelURLs, 0, len(urls))
		seen := make(map[string]int, len(urls))
		for _, entry := range urls {
			key := entry.RuntimeURL()
			if index, exists := seen[key]; exists {
				if len(merged[index].Protocols) == 0 || len(entry.Protocols) == 0 {
					merged[index].Protocols = nil
				} else {
					for _, protocol := range entry.Protocols {
						if !slices.Contains(merged[index].Protocols, protocol) {
							merged[index].Protocols = append(merged[index].Protocols, protocol)
						}
					}
				}
				continue
			}
			seen[key] = len(merged)
			merged = append(merged, entry)
		}
		urls = merged
		if err := urls.Normalize(); err != nil {
			return fmt.Errorf("channel %d direct URLs: %w", id, err)
		}
		encoded, err := json.Marshal(urls)
		if err != nil {
			return err
		}
		candidates = append(candidates, candidate{id: id, encoded: string(encoded), urls: changed})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, item := range candidates {
		if _, err := tx.ExecContext(ctx, rebindIfPostgres(dialect, "UPDATE channels SET url = ? WHERE id = ?"), item.encoded, item.id); err != nil {
			return err
		}
		for oldURL, newURL := range item.urls {
			var disabled int
			var updatedAt int64
			oldHash := fmt.Sprintf("%x", sha256.Sum256([]byte(oldURL)))
			newHash := fmt.Sprintf("%x", sha256.Sum256([]byte(newURL)))
			err := tx.QueryRowContext(ctx, rebindIfPostgres(dialect,
				"SELECT disabled, updated_at FROM channel_url_states WHERE channel_id = ? AND url_hash = ?"), item.id, oldHash).Scan(&disabled, &updatedAt)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			// 目标状态可能已存在；任一地址被手动禁用时继续禁用，不能因迁移恢复发送。
			query := `INSERT INTO channel_url_states (channel_id, url_hash, url, disabled, updated_at) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT(channel_id, url_hash) DO UPDATE SET
				disabled = CASE WHEN channel_url_states.disabled = 1 OR excluded.disabled = 1 THEN 1 ELSE 0 END`
			if dialect == DialectMySQL {
				query = `INSERT INTO channel_url_states (channel_id, url_hash, url, disabled, updated_at) VALUES (?, ?, ?, ?, ?)
					ON DUPLICATE KEY UPDATE disabled = IF(disabled = 1 OR VALUES(disabled) = 1, 1, 0)`
			}
			if _, err := tx.ExecContext(ctx, rebindIfPostgres(dialect, query), item.id, newHash, newURL, disabled, updatedAt); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, rebindIfPostgres(dialect,
				"DELETE FROM channel_url_states WHERE channel_id = ? AND url_hash = ?"), item.id, oldHash); err != nil {
				return err
			}
		}
	}
	if err := recordMigrationTx(ctx, tx, zaiDirectURLsMigrationVersion, dialect); err != nil {
		return err
	}
	return tx.Commit()
}
