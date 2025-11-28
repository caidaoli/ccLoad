package sqlite

import (
	"context"
	"database/sql"
	"log"
	"time"

	"ccLoad/internal/model"
)

func (s *SQLiteStore) AddLog(ctx context.Context, e *model.LogEntry) error {
	if e.Time.Time.IsZero() {
		e.Time = model.JSONTime{Time: time.Now()}
	}

	// 清理单调时钟信息，确保时间格式标准化
	cleanTime := e.Time.Time.Round(0) // 移除单调时钟部分

	// Unix时间戳：直接存储毫秒级Unix时间戳
	timeMs := cleanTime.UnixMilli()

	// API Key在写入时强制脱敏（2025-10-06）
	// 设计原则：数据库中不应存储完整API Key，避免备份和日志导出时泄露
	maskedKey := e.APIKeyUsed
	if maskedKey != "" {
		maskedKey = maskAPIKey(maskedKey)
	}

	// 直接写入日志数据库（简化预编译语句缓存）
	query := `
		INSERT INTO logs(time, model, channel_id, status_code, message, duration, is_streaming, first_byte_time, api_key_used,
			input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens, cost)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	_, err := s.logDB.ExecContext(ctx, query, timeMs, e.Model, e.ChannelID, e.StatusCode, e.Message, e.Duration, e.IsStreaming, e.FirstByteTime, maskedKey,
		e.InputTokens, e.OutputTokens, e.CacheReadInputTokens, e.CacheCreationInputTokens, e.Cost)
	return err
}

// BatchAddLogs 批量写入日志（单事务+预编译语句，提升刷盘性能）
// OCP：作为扩展方法提供，调用方可通过类型断言优先使用
func (s *SQLiteStore) BatchAddLogs(ctx context.Context, logs []*model.LogEntry) error {
	if len(logs) == 0 {
		return nil
	}

	tx, err := s.logDB.BeginTx(ctx, nil)
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
			return err
		}
	}

	return tx.Commit()
}

func (s *SQLiteStore) ListLogs(ctx context.Context, since time.Time, limit, offset int, filter *model.LogFilter) ([]*model.LogEntry, error) {
	// 使用查询构建器构建复杂查询（从 logDB 查询）
	// 性能优化：批量查询渠道名称消除N+1问题（100渠道场景提升50-100倍）
	baseQuery := `
		SELECT id, time, model, channel_id, status_code, message, duration, is_streaming, first_byte_time, api_key_used,
			input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens, cost
		FROM logs`

	// time字段现在是BIGINT毫秒时间戳，需要转换为Unix毫秒进行比较
	sinceMs := since.UnixMilli()

	qb := NewQueryBuilder(baseQuery).
		Where("time >= ?", sinceMs)

	// 支持按渠道名称过滤（无需跨库JOIN，先解析为渠道ID集合再按channel_id过滤）
	if filter != nil && (filter.ChannelName != "" || filter.ChannelNameLike != "") {
		ids, err := s.fetchChannelIDsByNameFilter(ctx, filter.ChannelName, filter.ChannelNameLike)
		if err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return []*model.LogEntry{}, nil
		}
		// 转换为[]any以用于占位符
		vals := make([]any, 0, len(ids))
		for _, id := range ids {
			vals = append(vals, id)
		}
		qb.WhereIn("channel_id", vals)
	}

	// 其余过滤条件（model等）
	qb.ApplyFilter(filter)

	suffix := "ORDER BY time DESC LIMIT ? OFFSET ?"
	query, args := qb.BuildWithSuffix(suffix)
	args = append(args, limit, offset)

	rows, err := s.logDB.QueryContext(ctx, query, args...)
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
		var timeMs int64 // Unix毫秒时间戳
		var apiKeyUsed sql.NullString
		var inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens sql.NullInt64
		var cost sql.NullFloat64

		if err := rows.Scan(&e.ID, &timeMs, &e.Model, &cfgID,
			&e.StatusCode, &e.Message, &duration, &isStreamingInt, &firstByteTime, &apiKeyUsed,
			&inputTokens, &outputTokens, &cacheReadTokens, &cacheCreationTokens, &cost); err != nil {
			return nil, err
		}

		// 转换Unix毫秒时间戳为time.Time
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
			// 向后兼容：历史数据可能包含明文Key，maskAPIKey是幂等的
			e.APIKeyUsed = maskAPIKey(apiKeyUsed.String)
		}
		// Token统计（2025-11新增）
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
		// 成本（2025-11新增）
		if cost.Valid {
			e.Cost = &cost.Float64
		}
		out = append(out, &e)
	}

	// 批量查询渠道名称
	if len(channelIDsToFetch) > 0 {
		channelNames, err := s.fetchChannelNamesBatch(ctx, channelIDsToFetch)
		if err != nil {
			// 降级处理：查询失败不影响日志返回，仅记录错误
			log.Printf("⚠️  批量查询渠道名称失败: %v", err)
			channelNames = make(map[int64]string)
		}

		// 填充渠道名称
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

// CountLogs 返回符合条件的日志总数（用于分页）
func (s *SQLiteStore) CountLogs(ctx context.Context, since time.Time, filter *model.LogFilter) (int, error) {
	baseQuery := `SELECT COUNT(*) FROM logs`
	sinceMs := since.UnixMilli()

	qb := NewQueryBuilder(baseQuery).
		Where("time >= ?", sinceMs)

	// 支持按渠道名称过滤（与ListLogs保持一致）
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

	// 其余过滤条件（model等）
	qb.ApplyFilter(filter)

	query, args := qb.Build()
	var count int
	err := s.logDB.QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}

// ListLogsRange 查询指定时间范围内的日志（支持精确日期范围如"昨日"）
func (s *SQLiteStore) ListLogsRange(ctx context.Context, since, until time.Time, limit, offset int, filter *model.LogFilter) ([]*model.LogEntry, error) {
	baseQuery := `
		SELECT id, time, model, channel_id, status_code, message, duration, is_streaming, first_byte_time, api_key_used,
			input_tokens, output_tokens, cache_read_input_tokens, cache_creation_input_tokens, cost
		FROM logs`

	sinceMs := since.UnixMilli()
	untilMs := until.UnixMilli()

	qb := NewQueryBuilder(baseQuery).
		Where("time >= ?", sinceMs).
		Where("time <= ?", untilMs)

	// 支持按渠道名称过滤
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

	rows, err := s.logDB.QueryContext(ctx, query, args...)
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

	// 批量查询渠道名称
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

// CountLogsRange 返回指定时间范围内符合条件的日志总数
func (s *SQLiteStore) CountLogsRange(ctx context.Context, since, until time.Time, filter *model.LogFilter) (int, error) {
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
	err := s.logDB.QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}
