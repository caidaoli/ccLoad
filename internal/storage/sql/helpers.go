package sql

import (
	"ccLoad/internal/model"
	"context"
	"fmt"
	"time"
)

// ChannelInfo 渠道基本信息（用于批量查询）
type ChannelInfo struct {
	Name     string
	Priority int
}

// fetchChannelInfoBatch 批量查询渠道信息（名称+优先级）
// 性能提升：N+1查询 → 1次全表查询 + 内存过滤（100渠道场景提升50-100倍）
// 设计原则（KISS）：渠道总数<1000，全表扫描比IN子查询更简单、更快
// 输入：渠道ID集合 map[int64]bool
// 输出：ID→渠道信息映射 map[int64]ChannelInfo
func (s *SQLStore) fetchChannelInfoBatch(ctx context.Context, channelIDs map[int64]bool) (map[int64]ChannelInfo, error) {
	if len(channelIDs) == 0 {
		return make(map[int64]ChannelInfo), nil
	}

	// 查询所有渠道（全表扫描，渠道数<1000时比IN子查询更快）
	// 优势：固定SQL（查询计划缓存）、无动态参数绑定、代码简单
	rows, err := s.db.QueryContext(ctx, "SELECT id, name, priority FROM channels")
	if err != nil {
		return nil, fmt.Errorf("query all channel info: %w", err)
	}
	defer rows.Close()

	// 解析并过滤需要的渠道（内存过滤，O(N)但N<1000）
	channelInfos := make(map[int64]ChannelInfo, len(channelIDs))
	for rows.Next() {
		var id int64
		var name string
		var priority int
		if err := rows.Scan(&id, &name, &priority); err != nil {
			continue // 跳过扫描错误的行
		}
		// 只保留需要的渠道
		if channelIDs[id] {
			channelInfos[id] = ChannelInfo{Name: name, Priority: priority}
		}
	}

	return channelInfos, nil
}

// fetchChannelNamesBatch 批量查询渠道名称（兼容旧接口）
// 输入：渠道ID集合 map[int64]bool
// 输出：ID→名称映射 map[int64]string
func (s *SQLStore) fetchChannelNamesBatch(ctx context.Context, channelIDs map[int64]bool) (map[int64]string, error) {
	infos, err := s.fetchChannelInfoBatch(ctx, channelIDs)
	if err != nil {
		return nil, err
	}
	names := make(map[int64]string, len(infos))
	for id, info := range infos {
		names[id] = info.Name
	}
	return names, nil
}

// fetchChannelIDsByNameFilter 根据精确/模糊名称获取渠道ID集合
func (s *SQLStore) fetchChannelIDsByNameFilter(ctx context.Context, exact string, like string) ([]int64, error) {
	// 构建查询
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
// 目的：避免跨库JOIN，先解析为ID再过滤logs
func (s *SQLStore) fetchChannelIDsByType(ctx context.Context, channelType string) ([]int64, error) {
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

// applyChannelFilter 应用渠道类型或名称过滤（优先级：ChannelType > ChannelName/Like）
// 返回值：是否应用了过滤、是否为空结果、错误
func (s *SQLStore) applyChannelFilter(ctx context.Context, qb *QueryBuilder, filter *model.LogFilter) (bool, bool, error) {
	if filter == nil {
		return false, false, nil
	}

	var candidateIDs []int64
	hasTypeFilter := filter.ChannelType != ""
	hasNameFilter := filter.ChannelName != "" || filter.ChannelNameLike != ""

	// 按渠道类型过滤
	if hasTypeFilter {
		ids, err := s.fetchChannelIDsByType(ctx, filter.ChannelType)
		if err != nil {
			return false, false, err
		}
		if len(ids) == 0 {
			return true, true, nil // 应用了过滤，结果为空
		}
		candidateIDs = ids
	}

	// 按渠道名称过滤
	if hasNameFilter {
		ids, err := s.fetchChannelIDsByNameFilter(ctx, filter.ChannelName, filter.ChannelNameLike)
		if err != nil {
			return false, false, err
		}
		if len(ids) == 0 {
			return true, true, nil // 应用了过滤，结果为空
		}

		if hasTypeFilter {
			// 取交集：同时满足类型和名称条件
			candidateIDs = intersectIDs(candidateIDs, ids)
			if len(candidateIDs) == 0 {
				return true, true, nil
			}
		} else {
			candidateIDs = ids
		}
	}

	// 应用过滤条件
	if len(candidateIDs) > 0 {
		vals := make([]any, 0, len(candidateIDs))
		for _, id := range candidateIDs {
			vals = append(vals, id)
		}
		qb.WhereIn("channel_id", vals)
		return true, false, nil
	}

	return false, false, nil
}

// intersectIDs 计算两个ID切片的交集
func intersectIDs(a, b []int64) []int64 {
	set := make(map[int64]bool, len(a))
	for _, id := range a {
		set[id] = true
	}
	var result []int64
	for _, id := range b {
		if set[id] {
			result = append(result, id)
		}
	}
	return result
}

// timeToUnix 将时间转换为Unix时间戳（秒）
// SQLite和MySQL都存储为BIGINT类型的Unix时间戳
func timeToUnix(t time.Time) int64 {
	return t.Unix()
}

// unixToTime 将Unix时间戳转换为时间
func unixToTime(ts int64) time.Time {
	return time.Unix(ts, 0)
}

// boolToInt 将布尔值转换为整数
// SQLite和MySQL都使用 1=true, 0=false
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
