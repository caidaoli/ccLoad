package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_foreign_keys=on", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	s := &SQLiteStore{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// migrate 创建数据库表结构
func (s *SQLiteStore) migrate(ctx context.Context) error {
	// 创建 channels 表
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS channels (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			api_key TEXT NOT NULL,
			url TEXT NOT NULL,
			priority INTEGER NOT NULL DEFAULT 0,
			models TEXT NOT NULL,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("create channels table: %w", err)
	}

	// 创建 cooldowns 表
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS cooldowns (
			channel_id INTEGER PRIMARY KEY,
			until TIMESTAMP NOT NULL,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			FOREIGN KEY(channel_id) REFERENCES channels(id) ON DELETE CASCADE
		);
	`); err != nil {
		return fmt.Errorf("create cooldowns table: %w", err)
	}

	// 创建 logs 表
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			time TIMESTAMP NOT NULL,
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

	// 创建 rr (round-robin) 表
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS rr (
			key TEXT PRIMARY KEY,
			idx INTEGER NOT NULL
		);
	`); err != nil {
		return fmt.Errorf("create rr table: %w", err)
	}

	// 创建索引优化查询性能
	if _, err := s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_logs_time ON logs(time);
	`); err != nil {
		return fmt.Errorf("create logs time index: %w", err)
	}

	// 创建渠道名称索引
	if _, err := s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_channels_name ON channels(name);
	`); err != nil {
		return fmt.Errorf("create channels name index: %w", err)
	}

	// 创建日志状态码索引
	if _, err := s.db.ExecContext(ctx, `
		CREATE INDEX IF NOT EXISTS idx_logs_status ON logs(status_code);
	`); err != nil {
		return fmt.Errorf("create logs status index: %w", err)
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

func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

// ---- Store interface impl ----

func (s *SQLiteStore) ListConfigs(ctx context.Context) ([]*Config, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, api_key, url, priority, models, enabled, created_at, updated_at 
		FROM channels
		ORDER BY priority DESC, id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Config
	for rows.Next() {
		var c Config
		var modelsStr string
		var enabledInt int
		if err := rows.Scan(&c.ID, &c.Name, &c.APIKey, &c.URL, &c.Priority,
			&modelsStr, &enabledInt, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.Enabled = enabledInt != 0
		if err := json.Unmarshal([]byte(modelsStr), &c.Models); err != nil {
			c.Models = nil
		}
		cc := c
		out = append(out, &cc)
	}
	return out, nil
}

func (s *SQLiteStore) GetConfig(ctx context.Context, id int64) (*Config, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, api_key, url, priority, models, enabled, created_at, updated_at 
		FROM channels 
		WHERE id = ?
	`, id)

	var c Config
	var modelsStr string
	var enabledInt int
	if err := row.Scan(&c.ID, &c.Name, &c.APIKey, &c.URL, &c.Priority,
		&modelsStr, &enabledInt, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("not found")
		}
		return nil, err
	}
	c.Enabled = enabledInt != 0
	_ = json.Unmarshal([]byte(modelsStr), &c.Models)
	return &c, nil
}

func (s *SQLiteStore) CreateConfig(ctx context.Context, c *Config) (*Config, error) {
	now := time.Now()
	modelsStr, _ := json.Marshal(c.Models)

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO channels(name, api_key, url, priority, models, enabled, created_at, updated_at) 
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
	`, c.Name, c.APIKey, c.URL, c.Priority, string(modelsStr),
		boolToInt(c.Enabled), now, now)

	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetConfig(ctx, id)
}

func (s *SQLiteStore) UpdateConfig(ctx context.Context, id int64, upd *Config) (*Config, error) {
	cur, err := s.GetConfig(ctx, id)
	if err != nil {
		return nil, err
	}

	// 合并更新字段
	if upd.Name != "" {
		cur.Name = upd.Name
	}
	if upd.APIKey != "" {
		cur.APIKey = upd.APIKey
	}
	if upd.URL != "" {
		cur.URL = upd.URL
	}
	if upd.Priority != 0 {
		cur.Priority = upd.Priority
	}
	if upd.Models != nil {
		cur.Models = upd.Models
	}

	// enabled 语义：若仅传 enabled 切换，否则保持
	if upd.Name == "" && upd.APIKey == "" && upd.URL == "" && upd.Priority == 0 && upd.Models == nil {
		cur.Enabled = upd.Enabled
	} else if upd.Enabled != cur.Enabled {
		cur.Enabled = upd.Enabled
	}

	cur.UpdatedAt = time.Now()
	modelsStr, _ := json.Marshal(cur.Models)

	_, err = s.db.ExecContext(ctx, `
		UPDATE channels 
		SET name=?, api_key=?, url=?, priority=?, models=?, enabled=?, updated_at=? 
		WHERE id=?
	`, cur.Name, cur.APIKey, cur.URL, cur.Priority, string(modelsStr),
		boolToInt(cur.Enabled), cur.UpdatedAt, id)

	if err != nil {
		return nil, err
	}
	return s.GetConfig(ctx, id)
}

func (s *SQLiteStore) DeleteConfig(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM channels WHERE id = ?`, id)
	return err
}

func (s *SQLiteStore) GetCooldownUntil(ctx context.Context, configID int64) (time.Time, bool) {
	row := s.db.QueryRowContext(ctx, `SELECT until FROM cooldowns WHERE channel_id = ?`, configID)
	var t time.Time
	if err := row.Scan(&t); err != nil {
		return time.Time{}, false
	}
	return t, true
}

func (s *SQLiteStore) SetCooldown(ctx context.Context, configID int64, until time.Time) error {
	// 计算冷却持续时间
	durMs := int64(0)
	if !until.IsZero() {
		now := time.Now()
		if until.After(now) {
			durMs = int64(until.Sub(now) / time.Millisecond)
		}
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cooldowns(channel_id, until, duration_ms) VALUES(?, ?, ?)
		ON CONFLICT(channel_id) DO UPDATE SET 
			until = excluded.until, 
			duration_ms = excluded.duration_ms
	`, configID, until, durMs)
	return err
}

// BumpCooldownOnError 指数退避：错误翻倍（最小1s，最大30m），成功清零
func (s *SQLiteStore) BumpCooldownOnError(ctx context.Context, configID int64, now time.Time) (time.Duration, error) {
	var until time.Time
	var durMs int64
	err := s.db.QueryRowContext(ctx, `
		SELECT until, COALESCE(duration_ms, 0) 
		FROM cooldowns 
		WHERE channel_id = ?
	`, configID).Scan(&until, &durMs)

	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	prev := time.Duration(durMs) * time.Millisecond
	if prev <= 0 {
		// 如果表里没有记录，但 until 在未来，取其差值；否则从1s开始
		if !until.IsZero() && until.After(now) {
			prev = until.Sub(now)
		} else {
			prev = time.Second
		}
	}

	// 错误一次翻倍
	next := prev * 2
	if next < time.Second {
		next = time.Second
	}
	if next > 30*time.Minute {
		next = 30 * time.Minute
	}

	newUntil := now.Add(next)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO cooldowns(channel_id, until, duration_ms) VALUES(?, ?, ?)
		ON CONFLICT(channel_id) DO UPDATE SET 
			until = excluded.until, 
			duration_ms = excluded.duration_ms
	`, configID, newUntil, int64(next/time.Millisecond))

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
	if e.Time.IsZero() {
		e.Time = time.Now()
	}

	// 在添加新日志前，先清理3天前的日志
	// 清理失败不影响日志记录，静默忽略错误
	_ = s.cleanupOldLogs(ctx)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO logs(time, model, channel_id, status_code, message, duration, is_streaming, first_byte_time) 
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)
	`, e.Time, e.Model, e.ChannelID, e.StatusCode, e.Message, e.Duration, e.IsStreaming, e.FirstByteTime)
	return err
}

// cleanupOldLogs 删除3天前的日志
func (s *SQLiteStore) cleanupOldLogs(ctx context.Context) error {
	cutoff := time.Now().AddDate(0, 0, -3) // 3天前
	_, err := s.db.ExecContext(ctx, `DELETE FROM logs WHERE time < ?`, cutoff)
	return err
}

func (s *SQLiteStore) ListLogs(ctx context.Context, since time.Time, limit, offset int, filter *LogFilter) ([]*LogEntry, error) {
	// 动态构建 WHERE 条件
	where := "l.time >= ?"
	args := []any{since}

	if filter != nil {
		if filter.ChannelID != nil {
			where += " AND l.channel_id = ?"
			args = append(args, *filter.ChannelID)
		}
		if filter.ChannelName != "" {
			where += " AND c.name = ?"
			args = append(args, filter.ChannelName)
		}
		if filter.ChannelNameLike != "" {
			where += " AND c.name LIKE ?"
			args = append(args, "%"+filter.ChannelNameLike+"%")
		}
		if filter.Model != "" {
			where += " AND l.model = ?"
			args = append(args, filter.Model)
		}
		if filter.ModelLike != "" {
			where += " AND l.model LIKE ?"
			args = append(args, "%"+filter.ModelLike+"%")
		}
	}

	args = append(args, limit, offset)
	query := fmt.Sprintf(`
		SELECT l.id, l.time, l.model, l.channel_id, c.name as channel_name, 
		       l.status_code, l.message, l.duration, l.is_streaming, l.first_byte_time
		FROM logs l
		LEFT JOIN channels c ON c.id = l.channel_id
		WHERE %s
		ORDER BY l.time DESC
		LIMIT ? OFFSET ?
	`, where)

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

		if err := rows.Scan(&e.ID, &e.Time, &e.Model, &cfgID, &chName,
			&e.StatusCode, &e.Message, &duration, &isStreamingInt, &firstByteTime); err != nil {
			return nil, err
		}

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
		out = append(out, &e)
	}
	return out, nil
}

func (s *SQLiteStore) Aggregate(ctx context.Context, since time.Time, bucket time.Duration) ([]MetricPoint, error) {
	// 拉取时间范围内的日志，在应用层做桶聚合
	rows, err := s.db.QueryContext(ctx, `
		SELECT time, status_code 
		FROM logs 
		WHERE time >= ?
	`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type pair struct {
		t      time.Time
		status int
	}
	arr := make([]pair, 0, 1024)

	for rows.Next() {
		var t time.Time
		var sc int
		if err := rows.Scan(&t, &sc); err != nil {
			return nil, err
		}
		arr = append(arr, pair{t: t, status: sc})
	}

	// 按时间桶聚合
	mapp := map[int64]*MetricPoint{}
	for _, e := range arr {
		if e.t.Before(since) {
			continue
		}
		ts := e.t.Truncate(bucket)
		key := ts.Unix()
		mp, ok := mapp[key]
		if !ok {
			mp = &MetricPoint{Ts: ts}
			mapp[key] = mp
		}
		if e.status >= 200 && e.status < 300 {
			mp.Success++
		} else {
			mp.Error++
		}
	}

	// 生成完整的时间序列
	out := []MetricPoint{}
	now := time.Now().Truncate(bucket)
	for t := since.Truncate(bucket); !t.After(now); t = t.Add(bucket) {
		key := t.Unix()
		if mp, ok := mapp[key]; ok {
			out = append(out, *mp)
		} else {
			out = append(out, MetricPoint{Ts: t})
		}
	}

	// 保证按时间升序
	sort.Slice(out, func(i, j int) bool {
		return out[i].Ts.Before(out[j].Ts)
	})
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
	err = tx.QueryRowContext(ctx, `SELECT idx FROM rr WHERE key = ?`, key).Scan(&cur)
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

	if _, err := tx.ExecContext(ctx, `UPDATE rr SET idx = ? WHERE key = ?`, next, key); err != nil {
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
	// 构建WHERE条件
	where := "l.time >= ?"
	args := []any{since}

	if filter != nil {
		if filter.ChannelID != nil {
			where += " AND l.channel_id = ?"
			args = append(args, *filter.ChannelID)
		}
		if filter.ChannelName != "" {
			where += " AND c.name = ?"
			args = append(args, filter.ChannelName)
		}
		if filter.ChannelNameLike != "" {
			where += " AND c.name LIKE ?"
			args = append(args, "%"+filter.ChannelNameLike+"%")
		}
		if filter.Model != "" {
			where += " AND l.model = ?"
			args = append(args, filter.Model)
		}
		if filter.ModelLike != "" {
			where += " AND l.model LIKE ?"
			args = append(args, "%"+filter.ModelLike+"%")
		}
	}

	// 统计查询：按channel_name和model分组统计成功/失败次数
	statsQuery := fmt.Sprintf(`
		SELECT 
			COALESCE(c.name, '系统') as channel_name,
			COALESCE(l.model, '') as model,
			SUM(CASE WHEN l.status_code >= 200 AND l.status_code < 300 THEN 1 ELSE 0 END) as success,
			SUM(CASE WHEN l.status_code < 200 OR l.status_code >= 300 THEN 1 ELSE 0 END) as error,
			COUNT(*) as total
		FROM logs l 
		LEFT JOIN channels c ON c.id = l.channel_id 
		WHERE %s
		GROUP BY c.name, l.model 
		ORDER BY channel_name ASC, model ASC
	`, where)

	rows, err := s.db.QueryContext(ctx, statsQuery, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []StatsEntry
	for rows.Next() {
		var entry StatsEntry
		err := rows.Scan(&entry.ChannelName, &entry.Model,
			&entry.Success, &entry.Error, &entry.Total)
		if err != nil {
			return nil, err
		}
		stats = append(stats, entry)
	}

	return stats, nil
}

// 辅助函数
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
