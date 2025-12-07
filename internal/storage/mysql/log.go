package mysql

import (
	"context"
	"database/sql"
	"log"
	"time"

	"ccLoad/internal/model"
)

func maskAPIKey(key string) string {
	if len(key) <= 8 {
		return key
	}
	return key[:4] + "..." + key[len(key)-4:]
}

func (s *MySQLStore) AddLog(ctx context.Context, e *model.LogEntry) error {
	if e.Time.Time.IsZero() {
		e.Time = model.JSONTime{Time: time.Now()}
	}

	cleanTime := e.Time.Time.Round(0)
	timeMs := cleanTime.UnixMilli()

	maskedKey := e.APIKeyUsed
	if maskedKey != "" {
		maskedKey = maskAPIKey(maskedKey)
	}

	query := `
		INSERT INTO logs(time, model, channel_id, status_code, message, duration, is_streaming, first_byte_time, api_key_used,
			input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens, cost)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	_, err := s.db.ExecContext(ctx, query, timeMs, e.Model, e.ChannelID, e.StatusCode, e.Message, e.Duration, e.IsStreaming, e.FirstByteTime, maskedKey,
		e.InputTokens, e.OutputTokens, e.CacheReadInputTokens, e.CacheCreationInputTokens, e.Cost)
	return err
}

// BatchAddLogs 批量写入日志（单事务+预编译语句，提升刷盘性能）
func (s *MySQLStore) BatchAddLogs(ctx context.Context, logs []*model.LogEntry) error {
	if len(logs) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO logs(time, model, channel_id, status_code, message, duration, is_streaming, first_byte_time, api_key_used,
			input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens, cost)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, e := range logs {
		t := e.Time.Time
		if t.IsZero() {
			t = time.Now()
		}
		cleanTime := t.Round(0)
		timeMs := cleanTime.UnixMilli()

		maskedKey := e.APIKeyUsed
		if maskedKey != "" {
			maskedKey = maskAPIKey(maskedKey)
		}

		if _, err := stmt.ExecContext(ctx,
			timeMs,
			e.Model,
			e.ChannelID,
			e.StatusCode,
			e.Message,
			e.Duration,
			e.IsStreaming,
			e.FirstByteTime,
			maskedKey,
			e.InputTokens,
			e.OutputTokens,
			e.CacheReadInputTokens,
			e.CacheCreationInputTokens,
			e.Cost,
		); err != nil {
			log.Printf("⚠️  批量写入日志失败: %v", err)
		}
	}

	return tx.Commit()
}

func (s *MySQLStore) ListLogs(ctx context.Context, since time.Time, limit, offset int, filter *model.LogFilter) ([]*model.LogEntry, error) {
	baseQuery := `
		SELECT id, time, model, channel_id, status_code, message, duration, is_streaming, first_byte_time, api_key_used,
			input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens, cost
		FROM logs`

	sinceMs := since.UnixMilli()

	qb := NewQueryBuilder(baseQuery).Where("time >= ?", sinceMs)

	if filter != nil && (filter.ChannelName != "" || filter.ChannelNameLike != "") {
		ids, err := s.fetchChannelIDsByNameFilter(ctx, filter.ChannelName, filter.ChannelNameLike)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return []*model.LogEntry{}, nil
		}
		vals := make([]any, 0, len(ids))
		for _, id := range ids {
			vals = append(vals, id)
		}
		qb.WhereIn("channel_id", vals)
	}

	qb.ApplyFilter(filter)

	suffix := "ORDER BY time DESC LIMIT ? OFFSET ?"
	query, args := qb.BuildWithSuffix(suffix)
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*model.LogEntry{}
	channelIDsToFetch := make(map[int64]bool)

	for rows.Next() {
		var e model.LogEntry
		var cfgID sql.NullInt64
		var duration sql.NullFloat64
		var isStreamingInt int
		var firstByteTime sql.NullFloat64
		var timeMs int64
		var apiKeyUsed sql.NullString
		var inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens sql.NullInt64
		var cost sql.NullFloat64

		if err := rows.Scan(&e.ID, &timeMs, &e.Model, &cfgID,
			&e.StatusCode, &e.Message, &duration, &isStreamingInt, &firstByteTime, &apiKeyUsed,
			&inputTokens, &outputTokens, &cacheReadTokens, &cacheCreationTokens, &cost); err != nil {
			return nil, err
		}

		e.Time = model.JSONTime{Time: time.UnixMilli(timeMs)}

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
		if inputTokens.Valid {
			val := int(inputTokens.Int64)
			e.InputTokens = &val
		}
		if outputTokens.Valid {
			val := int(outputTokens.Int64)
			e.OutputTokens = &val
		}
		if cacheReadTokens.Valid {
			val := int(cacheReadTokens.Int64)
			e.CacheReadInputTokens = &val
		}
		if cacheCreationTokens.Valid {
			val := int(cacheCreationTokens.Int64)
			e.CacheCreationInputTokens = &val
		}
		if cost.Valid {
			e.Cost = &cost.Float64
		}
		out = append(out, &e)
	}

	if len(channelIDsToFetch) > 0 {
		channelNames, err := s.fetchChannelNamesBatch(ctx, channelIDsToFetch)
		if err != nil {
			log.Printf("⚠️  批量查询渠道名称失败: %v", err)
			channelNames = make(map[int64]string)
		}
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

func (s *MySQLStore) CountLogs(ctx context.Context, since time.Time, filter *model.LogFilter) (int, error) {
	baseQuery := `SELECT COUNT(*) FROM logs`
	sinceMs := since.UnixMilli()

	qb := NewQueryBuilder(baseQuery).Where("time >= ?", sinceMs)

	if filter != nil && (filter.ChannelName != "" || filter.ChannelNameLike != "") {
		ids, err := s.fetchChannelIDsByNameFilter(ctx, filter.ChannelName, filter.ChannelNameLike)
		if err != nil {
			return 0, err
		}
		if len(ids) == 0 {
			return 0, nil
		}
		vals := make([]any, 0, len(ids))
		for _, id := range ids {
			vals = append(vals, id)
		}
		qb.WhereIn("channel_id", vals)
	}

	qb.ApplyFilter(filter)

	query, args := qb.Build()
	var count int
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}

func (s *MySQLStore) ListLogsRange(ctx context.Context, since, until time.Time, limit, offset int, filter *model.LogFilter) ([]*model.LogEntry, error) {
	baseQuery := `
		SELECT id, time, model, channel_id, status_code, message, duration, is_streaming, first_byte_time, api_key_used,
			input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens, cost
		FROM logs`

	sinceMs := since.UnixMilli()
	untilMs := until.UnixMilli()

	qb := NewQueryBuilder(baseQuery).
		Where("time >= ?", sinceMs).
		Where("time <= ?", untilMs)

	if filter != nil && (filter.ChannelName != "" || filter.ChannelNameLike != "") {
		ids, err := s.fetchChannelIDsByNameFilter(ctx, filter.ChannelName, filter.ChannelNameLike)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return []*model.LogEntry{}, nil
		}
		vals := make([]any, 0, len(ids))
		for _, id := range ids {
			vals = append(vals, id)
		}
		qb.WhereIn("channel_id", vals)
	}

	qb.ApplyFilter(filter)

	suffix := "ORDER BY time DESC LIMIT ? OFFSET ?"
	query, args := qb.BuildWithSuffix(suffix)
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*model.LogEntry{}
	channelIDsToFetch := make(map[int64]bool)

	for rows.Next() {
		var e model.LogEntry
		var cfgID sql.NullInt64
		var duration sql.NullFloat64
		var isStreamingInt int
		var firstByteTime sql.NullFloat64
		var timeMs int64
		var apiKeyUsed sql.NullString
		var inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens sql.NullInt64
		var cost sql.NullFloat64

		if err := rows.Scan(&e.ID, &timeMs, &e.Model, &cfgID,
			&e.StatusCode, &e.Message, &duration, &isStreamingInt, &firstByteTime, &apiKeyUsed,
			&inputTokens, &outputTokens, &cacheReadTokens, &cacheCreationTokens, &cost); err != nil {
			return nil, err
		}

		e.Time = model.JSONTime{Time: time.UnixMilli(timeMs)}

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
		if inputTokens.Valid {
			val := int(inputTokens.Int64)
			e.InputTokens = &val
		}
		if outputTokens.Valid {
			val := int(outputTokens.Int64)
			e.OutputTokens = &val
		}
		if cacheReadTokens.Valid {
			val := int(cacheReadTokens.Int64)
			e.CacheReadInputTokens = &val
		}
		if cacheCreationTokens.Valid {
			val := int(cacheCreationTokens.Int64)
			e.CacheCreationInputTokens = &val
		}
		if cost.Valid {
			e.Cost = &cost.Float64
		}
		out = append(out, &e)
	}

	if len(channelIDsToFetch) > 0 {
		channelNames, err := s.fetchChannelNamesBatch(ctx, channelIDsToFetch)
		if err != nil {
			log.Printf("⚠️  批量查询渠道名称失败: %v", err)
			channelNames = make(map[int64]string)
		}
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

func (s *MySQLStore) CountLogsRange(ctx context.Context, since, until time.Time, filter *model.LogFilter) (int, error) {
	baseQuery := `SELECT COUNT(*) FROM logs`
	sinceMs := since.UnixMilli()
	untilMs := until.UnixMilli()

	qb := NewQueryBuilder(baseQuery).
		Where("time >= ?", sinceMs).
		Where("time <= ?", untilMs)

	if filter != nil && (filter.ChannelName != "" || filter.ChannelNameLike != "") {
		ids, err := s.fetchChannelIDsByNameFilter(ctx, filter.ChannelName, filter.ChannelNameLike)
		if err != nil {
			return 0, err
		}
		if len(ids) == 0 {
			return 0, nil
		}
		vals := make([]any, 0, len(ids))
		for _, id := range ids {
			vals = append(vals, id)
		}
		qb.WhereIn("channel_id", vals)
	}

	qb.ApplyFilter(filter)

	query, args := qb.Build()
	var count int
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}
