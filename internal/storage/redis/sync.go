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
// [INFO] 修复（2025-10-10）：完整恢复渠道和API Keys，解决Redis恢复后缺少Keys的问题
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
// [INFO] 修复（2025-10-10）：完整同步渠道和API Keys，解决Redis恢复后缺少Keys的问题
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

// ============================================================================
// Auth Tokens Sync - 认证令牌同步 (新增 2025-11)
// ============================================================================

const authTokensKey = "ccload:auth_tokens"

// SyncAllAuthTokens 全量同步所有AuthToken到Redis
// 设计: 使用独立的Redis Key存储,与channels分离
func (rs *RedisSync) SyncAllAuthTokens(ctx context.Context, tokens []*model.AuthToken) error {
	if !rs.enabled {
		return nil
	}

	ctxWithTimeout, cancel := context.WithTimeout(ctx, rs.timeout*2)
	defer cancel()

	// 序列化为JSON数组
	data, err := sonic.Marshal(tokens)
	if err != nil {
		return fmt.Errorf("marshal auth tokens: %w", err)
	}

	// 原子写入Redis (覆盖整个key)
	return rs.client.Set(ctxWithTimeout, authTokensKey, data, 0).Err()
}

// LoadAuthTokensFromRedis 从Redis加载所有AuthToken
// 返回: 空数组表示Redis中无数据,error表示读取失败
func (rs *RedisSync) LoadAuthTokensFromRedis(ctx context.Context) ([]*model.AuthToken, error) {
	if !rs.enabled {
		return nil, nil
	}

	ctxWithTimeout, cancel := context.WithTimeout(ctx, rs.timeout*2)
	defer cancel()

	// 读取JSON数据
	data, err := rs.client.Get(ctxWithTimeout, authTokensKey).Result()
	if err != nil {
		if err == redis.Nil {
			return []*model.AuthToken{}, nil // Key不存在,返回空数组
		}
		return nil, fmt.Errorf("redis get auth tokens: %w", err)
	}

	// 解析JSON
	var tokens []*model.AuthToken
	if err := sonic.Unmarshal([]byte(data), &tokens); err != nil {
		return nil, fmt.Errorf("unmarshal auth tokens: %w", err)
	}

	if tokens == nil {
		return []*model.AuthToken{}, nil
	}

	return tokens, nil
}
