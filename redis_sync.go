package main

import (
	"context"
	"fmt"
	"time"

	"github.com/bytedance/sonic"
	"github.com/redis/go-redis/v9"
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

// LoadChannelsFromRedis 从Redis加载所有渠道配置 (启动恢复机制)
func (rs *RedisSync) LoadChannelsFromRedis(ctx context.Context) ([]*Config, error) {
	if !rs.enabled {
		return nil, nil
	}

	ctxWithTimeout, cancel := context.WithTimeout(ctx, rs.timeout*2) // 加载操作允许更长超时
	defer cancel()

	// 使用GET获取完整的JSON数组
	data, err := rs.client.Get(ctxWithTimeout, rs.key).Result()
	if err != nil {
		if err == redis.Nil {
			return []*Config{}, nil // Key不存在，返回空数组
		}
		return nil, fmt.Errorf("redis get failed: %w", err)
	}

	// 解析JSON数组为Config对象切片
	var configs []*Config
	if err := sonic.Unmarshal([]byte(data), &configs); err != nil {
		return nil, fmt.Errorf("unmarshal channels json: %w", err)
	}

	if configs == nil {
		return []*Config{}, nil
	}

	return configs, nil
}

// SyncAllChannels 全量同步所有渠道到Redis (KISS: 使用简单的SET操作存储完整JSON)
func (rs *RedisSync) SyncAllChannels(ctx context.Context, configs []*Config) error {
	if !rs.enabled {
		return nil
	}

	ctxWithTimeout, cancel := context.WithTimeout(ctx, rs.timeout*2)
	defer cancel()

	// 序列化整个配置数组为JSON
	data, err := sonic.Marshal(configs)
	if err != nil {
		return fmt.Errorf("marshal all configs: %w", err)
	}

	// 使用SET直接覆盖整个key（原子操作，无需DEL）
	return rs.client.Set(ctxWithTimeout, rs.key, data, 0).Err()
}

// GetChannelCount 获取Redis中的渠道数量 (用于健康检查和监控)
func (rs *RedisSync) GetChannelCount(ctx context.Context) (int64, error) {
	if !rs.enabled {
		return 0, nil
	}

	// 加载所有渠道并返回数量
	configs, err := rs.LoadChannelsFromRedis(ctx)
	if err != nil {
		return 0, err
	}

	return int64(len(configs)), nil
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
