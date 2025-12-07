package mysql

import (
	"ccLoad/internal/model"
	"context"
	"fmt"
)

// fetchChannelNamesBatch 批量查询渠道名称
func (s *MySQLStore) fetchChannelNamesBatch(ctx context.Context, channelIDs map[int64]bool) (map[int64]string, error) {
	if len(channelIDs) == 0 {
		return make(map[int64]string), nil
	}

	rows, err := s.db.QueryContext(ctx, "SELECT id, name FROM channels")
	if err != nil {
		return nil, fmt.Errorf("query all channel names: %w", err)
	}
	defer rows.Close()

	channelNames := make(map[int64]string, len(channelIDs))
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			continue
		}
		if channelIDs[id] {
			channelNames[id] = name
		}
	}

	return channelNames, nil
}

// fetchChannelIDsByNameFilter 根据精确/模糊名称获取渠道ID集合
func (s *MySQLStore) fetchChannelIDsByNameFilter(ctx context.Context, exact string, like string) ([]int64, error) {
	var (
		query string
		args  []any
	)
	if exact != "" {
		query = "SELECT id FROM channels WHERE name = ?"
		args = []any{exact}
	} else if like != "" {
		query = "SELECT id FROM channels WHERE name LIKE ?"
		args = []any{"%" + like + "%"}
	} else {
		return nil, nil
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query channel ids by name: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan channel id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// fetchChannelIDsByType 根据渠道类型获取渠道ID集合
func (s *MySQLStore) fetchChannelIDsByType(ctx context.Context, channelType string) ([]int64, error) {
	if channelType == "" {
		return nil, nil
	}

	query := "SELECT id FROM channels WHERE channel_type = ?"
	rows, err := s.db.QueryContext(ctx, query, channelType)
	if err != nil {
		return nil, fmt.Errorf("query channel ids by type: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan channel id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// applyChannelFilter 应用渠道类型或名称过滤
func (s *MySQLStore) applyChannelFilter(ctx context.Context, qb *QueryBuilder, filter *model.LogFilter) (bool, bool, error) {
	if filter == nil {
		return false, false, nil
	}

	if filter.ChannelType != "" {
		ids, err := s.fetchChannelIDsByType(ctx, filter.ChannelType)
		if err != nil {
			return false, false, err
		}
		if len(ids) == 0 {
			return true, true, nil
		}
		vals := make([]any, 0, len(ids))
		for _, id := range ids {
			vals = append(vals, id)
		}
		qb.WhereIn("channel_id", vals)
		return true, false, nil
	}

	if filter.ChannelName != "" || filter.ChannelNameLike != "" {
		ids, err := s.fetchChannelIDsByNameFilter(ctx, filter.ChannelName, filter.ChannelNameLike)
		if err != nil {
			return false, false, err
		}
		if len(ids) == 0 {
			return true, true, nil
		}
		vals := make([]any, 0, len(ids))
		for _, id := range ids {
			vals = append(vals, id)
		}
		qb.WhereIn("channel_id", vals)
		return true, false, nil
	}

	return false, false, nil
}
