package redis

import (
	"context"
	"fmt"
	"time"

	"github.com/bytedance/sonic"
	"github.com/redis/go-redis/v9"

	"ccLoad/internal/model"
)

// RedisSync 负责Redis同步操作的结构体
type RedisSync struct {
	client  *redis.Client
	enabled bool
	key     string // Redis key: "ccload:channels" (存储完整JSON数组)
	timeout time.Duration
}

// NewRedisSync 创建Redis同步客户端
func NewRedisSync(redisURL string) (*RedisSync, error) {
	if redisURL == "" {
		return &RedisSync{enabled: false}, nil
	}

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}

	// 设置连接池参数优化性能
	opts.PoolSize = 10
	opts.MinIdleConns = 2
	opts.ConnMaxLifetime = 5 * time.Minute
	opts.DialTimeout = 3 * time.Second
	opts.ReadTimeout = 2 * time.Second
	opts.WriteTimeout = 2 * time.Second

	client := redis.NewClient(opts)

	// 测试连接
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping failed: %w", err)
	}

	return &RedisSync{
		client:  client,
		enabled: true,
		key:     "ccload:channels",
		timeout: 2 * time.Second,
	}, nil
}

// Close 关闭Redis连接
func (rs *RedisSync) Close() error {
	if !rs.enabled {
		return nil
	}
	return rs.client.Close()
}

// IsEnabled 检查Redis同步是否启用
func (rs *RedisSync) IsEnabled() bool {
	return rs.enabled
}

// LoadChannelsWithKeysFromRedis 从Redis加载所有渠道（含API Keys）
// ✅ 修复（2025-10-10）：完整恢复渠道和API Keys，解决Redis恢复后缺少Keys的问题
func (rs *RedisSync) LoadChannelsWithKeysFromRedis(ctx context.Context) ([]*model.ChannelWithKeys, error) {
	if !rs.enabled {
		return nil, nil
	}

	ctxWithTimeout, cancel := context.WithTimeout(ctx, rs.timeout*2)
	defer cancel()

	// 使用GET获取完整的JSON数组
	data, err := rs.client.Get(ctxWithTimeout, rs.key).Result()
	if err != nil {
		if err == redis.Nil {
			return []*model.ChannelWithKeys{}, nil // Key不存在，返回空数组
		}
		return nil, fmt.Errorf("redis get failed: %w", err)
	}

	// 解析JSON数组为ChannelWithKeys对象切片
	var channelsWithKeys []*model.ChannelWithKeys
	if err := sonic.Unmarshal([]byte(data), &channelsWithKeys); err != nil {
		return nil, fmt.Errorf("unmarshal channels with keys json: %w", err)
	}

	if channelsWithKeys == nil {
		return []*model.ChannelWithKeys{}, nil
	}

	return channelsWithKeys, nil
}

// SyncAllChannelsWithKeys 全量同步所有渠道（含API Keys）到Redis
// ✅ 修复（2025-10-10）：完整同步渠道和API Keys，解决Redis恢复后缺少Keys的问题
func (rs *RedisSync) SyncAllChannelsWithKeys(ctx context.Context, channelsWithKeys []*model.ChannelWithKeys) error {
	if !rs.enabled {
		return nil
	}

	ctxWithTimeout, cancel := context.WithTimeout(ctx, rs.timeout*2)
	defer cancel()

	// 序列化整个ChannelWithKeys数组为JSON
	data, err := sonic.Marshal(channelsWithKeys)
	if err != nil {
		return fmt.Errorf("marshal channels with keys: %w", err)
	}

	// 使用SET直接覆盖整个key（原子操作）
	return rs.client.Set(ctxWithTimeout, rs.key, data, 0).Err()
}

// GetChannelCount 获取Redis中的渠道数量 (用于健康检查和监控)
// ✅ 修复（2025-10-10）：切换到新API，支持ChannelWithKeys
func (rs *RedisSync) GetChannelCount(ctx context.Context) (int64, error) {
	if !rs.enabled {
		return 0, nil
	}

	// 加载所有渠道（含Keys）并返回数量
	channelsWithKeys, err := rs.LoadChannelsWithKeysFromRedis(ctx)
	if err != nil {
		return 0, err
	}

	return int64(len(channelsWithKeys)), nil
}

// HealthCheck 检查Redis连接状态
func (rs *RedisSync) HealthCheck(ctx context.Context) error {
	if !rs.enabled {
		return nil
	}

	ctxWithTimeout, cancel := context.WithTimeout(ctx, rs.timeout)
	defer cancel()

	return rs.client.Ping(ctxWithTimeout).Err()
}
