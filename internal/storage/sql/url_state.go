package sql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// urlHash 计算 URL 的 SHA-256 十六进制摘要（用作 channel_url_states 主键的一部分）。
func urlHash(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])
}

// LoadDisabledURLs 加载所有渠道的手动禁用URL列表（启动时回填URLSelector）
func (s *SQLStore) LoadDisabledURLs(ctx context.Context) (map[int64][]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT channel_id, url FROM channel_url_states WHERE disabled = 1`)
	if err != nil {
		return nil, fmt.Errorf("query channel_url_states: %w", err)
	}
	defer func() { _ = rows.Close() }()

	result := make(map[int64][]string)
	for rows.Next() {
		var channelID int64
		var url string
		if err := rows.Scan(&channelID, &url); err != nil {
			return nil, fmt.Errorf("scan channel_url_states: %w", err)
		}
		result[channelID] = append(result[channelID], url)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate channel_url_states: %w", err)
	}
	return result, nil
}

// SetURLDisabled 持久化指定渠道URL的禁用状态
func (s *SQLStore) SetURLDisabled(ctx context.Context, channelID int64, url string, disabled bool) error {
	now := timeToUnix(time.Now())
	disabledInt := 0
	if disabled {
		disabledInt = 1
	}
	hash := urlHash(url)

	var query string
	if s.IsSQLite() {
		query = `
			INSERT INTO channel_url_states (channel_id, url_hash, url, disabled, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(channel_id, url_hash) DO UPDATE SET
				url = excluded.url,
				disabled = excluded.disabled,
				updated_at = excluded.updated_at
		`
	} else {
		query = `
			INSERT INTO channel_url_states (channel_id, url_hash, url, disabled, updated_at)
			VALUES (?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				url = VALUES(url),
				disabled = VALUES(disabled),
				updated_at = VALUES(updated_at)
		`
	}

	if _, err := s.db.ExecContext(ctx, query, channelID, hash, url, disabledInt, now); err != nil {
		return fmt.Errorf("upsert channel_url_states: %w", err)
	}
	return nil
}
