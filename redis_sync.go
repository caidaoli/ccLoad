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
	client   *redis.Client
	enabled  bool
	hashKey  string // Redis Hash key: "ccload:channels"
	timeout  time.Duration
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
		hashKey: "ccload:channels",
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

// SyncChannelCreate 同步渠道创建操作 (SRP: 单一职责 - 创建同步)
func (rs *RedisSync) SyncChannelCreate(ctx context.Context, config *Config) error {
	if !rs.enabled {
		return nil
	}

	return rs.syncChannel(ctx, config)
}

// SyncChannelUpdate 同步渠道更新操作 (SRP: 单一职责 - 更新同步)
func (rs *RedisSync) SyncChannelUpdate(ctx context.Context, config *Config) error {
	if !rs.enabled {
		return nil
	}

	return rs.syncChannel(ctx, config)
}

// SyncChannelDelete 同步渠道删除操作 (SRP: 单一职责 - 删除同步)
func (rs *RedisSync) SyncChannelDelete(ctx context.Context, name string) error {
	if !rs.enabled {
		return nil
	}

	ctxWithTimeout, cancel := context.WithTimeout(ctx, rs.timeout)
	defer cancel()

	return rs.client.HDel(ctxWithTimeout, rs.hashKey, name).Err()
}

// syncChannel 统一的渠道同步逻辑 (DRY: 消除重复逻辑)
func (rs *RedisSync) syncChannel(ctx context.Context, config *Config) error {
	ctxWithTimeout, cancel := context.WithTimeout(ctx, rs.timeout)
	defer cancel()

	// 序列化Config为JSON (KISS: 简单的JSON存储)
	data, err := sonic.Marshal(config)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	// 使用渠道名称作为Hash field，确保唯一性
	return rs.client.HSet(ctxWithTimeout, rs.hashKey, config.Name, data).Err()
}

// LoadChannelsFromRedis 从Redis加载所有渠道配置 (启动恢复机制)
func (rs *RedisSync) LoadChannelsFromRedis(ctx context.Context) ([]*Config, error) {
	if !rs.enabled {
		return nil, nil
	}

	ctxWithTimeout, cancel := context.WithTimeout(ctx, rs.timeout*2) // 加载操作允许更长超时
	defer cancel()

	// 获取Hash中的所有字段和值
	result, err := rs.client.HGetAll(ctxWithTimeout, rs.hashKey).Result()
	if err != nil {
		if err == redis.Nil {
			return []*Config{}, nil // 空数据不是错误
		}
		return nil, fmt.Errorf("redis hgetall failed: %w", err)
	}

	if len(result) == 0 {
		return []*Config{}, nil
	}

	// 解析JSON数据为Config对象
	configs := make([]*Config, 0, len(result))
	for name, data := range result {
		var config Config
		if err := sonic.Unmarshal([]byte(data), &config); err != nil {
			fmt.Printf("Warning: failed to unmarshal config for %s: %v\n", name, err)
			continue
		}

		// 确保名称一致性
		config.Name = name
		configs = append(configs, &config)
	}

	return configs, nil
}

// SyncAllChannels 批量同步所有渠道到Redis (批量操作优化性能)
func (rs *RedisSync) SyncAllChannels(ctx context.Context, configs []*Config) error {
	if !rs.enabled || len(configs) == 0 {
		return nil
	}

	ctxWithTimeout, cancel := context.WithTimeout(ctx, rs.timeout*2)
	defer cancel()

	// 使用Pipeline批量操作提升性能
	pipe := rs.client.Pipeline()

	// 先清空现有数据
	pipe.Del(ctxWithTimeout, rs.hashKey)

	// 批量设置所有渠道
	for _, config := range configs {
		data, err := sonic.Marshal(config)
		if err != nil {
			return fmt.Errorf("marshal config %s: %w", config.Name, err)
		}
		pipe.HSet(ctxWithTimeout, rs.hashKey, config.Name, data)
	}

	// 执行Pipeline
	_, err := pipe.Exec(ctxWithTimeout)
	return err
}

// GetChannelCount 获取Redis中的渠道数量 (用于健康检查和监控)
func (rs *RedisSync) GetChannelCount(ctx context.Context) (int64, error) {
	if !rs.enabled {
		return 0, nil
	}

	ctxWithTimeout, cancel := context.WithTimeout(ctx, rs.timeout)
	defer cancel()

	return rs.client.HLen(ctxWithTimeout, rs.hashKey).Result()
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