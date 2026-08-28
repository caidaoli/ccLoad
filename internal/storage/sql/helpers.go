package sql

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	"ccLoad/internal/model"
)

// FetchChannelInfoBatch 批量查询渠道信息（名称+优先级+倍率区间）
// 消除 N+1：一次全表查询 + 内存过滤
// 设计原则（KISS）：渠道总数<1000时，全表扫描比动态 IN 子查询更简单
// 输入：渠道ID集合 map[int64]bool
// 输出：ID→渠道信息映射 map[int64]model.ChannelInfo
func (s *SQLStore) FetchChannelInfoBatch(ctx context.Context, channelIDs map[int64]bool) (map[int64]model.ChannelInfo, error) {
	if len(channelIDs) == 0 {
		return make(map[int64]model.ChannelInfo), nil
	}

	// 查询所有渠道（全表扫描，渠道数<1000时比IN子查询更快）
	// 优势：固定SQL（查询计划缓存）、无动态参数绑定、代码简单
	// 倍率按 auth_type 分流：OAuth 渠道取 channels.cost_multiplier；api_key 渠道取未禁用 Key 的 min/max（无启用 Key 回退渠道列）
	rows, err := s.QueryContext(ctx, `
		SELECT
			c.id,
			c.name,
			c.priority,
			c.auth_type,
			COALESCE(c.cost_multiplier, 1),
			MIN(CASE WHEN c.auth_type = 'api_key' AND k.disabled = 0 THEN COALESCE(k.cost_multiplier, 1) END),
			MAX(CASE WHEN c.auth_type = 'api_key' AND k.disabled = 0 THEN COALESCE(k.cost_multiplier, 1) END)
		FROM channels c
		LEFT JOIN api_keys k ON k.channel_id = c.id
		GROUP BY c.id
	`)
	if err != nil {
		return nil, fmt.Errorf("query all channel info: %w", err)
	}
	defer func() { _ = rows.Close() }()

	// 解析并过滤需要的渠道（内存过滤，O(N)但N<1000）
	channelInfos := make(map[int64]model.ChannelInfo, len(channelIDs))
	for rows.Next() {
		var id int64
		var name string
		var priority int
		var authType string
		var channelMultiplier float64
		var keyMin, keyMax sql.NullFloat64
		if err := rows.Scan(&id, &name, &priority, &authType, &channelMultiplier, &keyMin, &keyMax); err != nil {
			log.Printf("[WARN] 扫描渠道信息失败: %v", err)
			continue // 跳过扫描错误的行
		}
		multiplierMin := normalizeCostMultiplier(channelMultiplier)
		multiplierMax := multiplierMin
		if authType == model.AuthTypeAPIKey && keyMin.Valid && keyMax.Valid {
			multiplierMin = normalizeCostMultiplier(keyMin.Float64)
			multiplierMax = normalizeCostMultiplier(keyMax.Float64)
		}
		// 只保留需要的渠道
		if channelIDs[id] {
			channelInfos[id] = model.ChannelInfo{
				Name:              name,
				Priority:          priority,
				CostMultiplierMin: multiplierMin,
				CostMultiplierMax: multiplierMax,
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate channel info rows: %w", err)
	}

	return channelInfos, nil
}

// fetchChannelNamesBatch 批量查询渠道名称（兼容旧接口）
// 输入：渠道ID集合 map[int64]bool
// 输出：ID→名称映射 map[int64]string
func (s *SQLStore) fetchChannelNamesBatch(ctx context.Context, channelIDs map[int64]bool) (map[int64]string, error) {
	infos, err := s.FetchChannelInfoBatch(ctx, channelIDs)
	if err != nil {
		return nil, err
	}
	names := make(map[int64]string, len(infos))
	for id, info := range infos {
		names[id] = info.Name
	}
	return names, nil
}

// fetchAuthTokenDescriptionsBatch 批量查询API令牌描述
func (s *SQLStore) fetchAuthTokenDescriptionsBatch(ctx context.Context, tokenIDs map[int64]bool) (map[int64]string, error) {
	if len(tokenIDs) == 0 {
		return make(map[int64]string), nil
	}

	ids := make([]any, 0, len(tokenIDs))
	placeholders := make([]string, 0, len(tokenIDs))
	for id := range tokenIDs {
		ids = append(ids, id)
		placeholders = append(placeholders, "?")
	}

	query := "SELECT id, description FROM auth_tokens WHERE id IN (" +
		strings.Join(placeholders, ",") + ")"

	rows, err := s.QueryContext(ctx, query, ids...)
	if err != nil {
		return nil, fmt.Errorf("query auth token descriptions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	descriptions := make(map[int64]string, len(tokenIDs))
	for rows.Next() {
		var id int64
		var desc string
		if err := rows.Scan(&id, &desc); err != nil {
			return nil, fmt.Errorf("scan auth token description: %w", err)
		}
		descriptions[id] = desc
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate auth token descriptions: %w", err)
	}
	return descriptions, nil
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

	rows, err := s.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query channel ids by name: %w", err)
	}
	defer func() { _ = rows.Close() }()

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

// applyChannelFilter 将渠道名称过滤解析为渠道 ID。
// 返回值：是否为空结果、错误
// 注意：ChannelID 精确匹配不在此处处理，由 QueryBuilder.ApplyFilter 负责
func (s *SQLStore) applyChannelFilter(ctx context.Context, qb *QueryBuilder, filter *model.LogFilter) (bool, error) {
	channelIDs, isEmpty, err := s.resolveChannelFilter(ctx, filter)
	if err != nil {
		return false, err
	}
	if isEmpty {
		return true, nil
	}
	if len(channelIDs) > 0 {
		vals := make([]any, 0, len(channelIDs))
		for _, id := range channelIDs {
			vals = append(vals, id)
		}
		qb.WhereIn("channel_id", vals)
	}
	return false, nil
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

// normalizeSQLArgs 将领域布尔值转换为三种数据库统一使用的整数布尔值。
// 仅在遇到 bool 时复制参数，避免修改调用方复用的切片。
func normalizeSQLArgs(args []any) []any {
	for i, arg := range args {
		if _, ok := arg.(bool); !ok {
			continue
		}

		normalized := append([]any(nil), args...)
		for j := i; j < len(normalized); j++ {
			value, ok := normalized[j].(bool)
			if !ok {
				continue
			}
			if value {
				normalized[j] = 1
			} else {
				normalized[j] = 0
			}
		}
		return normalized
	}
	return args
}

// normalizeCostMultiplier 规范化成本倍率：负数退化为 1；0 表示免费渠道，保持不变
func normalizeCostMultiplier(m float64) float64 {
	if m < 0 {
		return 1
	}
	return m
}
