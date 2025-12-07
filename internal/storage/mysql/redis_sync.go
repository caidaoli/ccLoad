package mysql

import (
	"context"
	"fmt"
	"log"
	"time"

	"ccLoad/internal/model"
)

// redisSyncWorker 异步Redis同步worker（后台goroutine）
func (s *MySQLStore) redisSyncWorker() {
	// 使用可取消的context，支持优雅关闭
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 指数退避重试配置
	retryBackoff := []time.Duration{
		1 * time.Second,  // 第1次重试：1秒后
		5 * time.Second,  // 第2次重试：5秒后
		15 * time.Second, // 第3次重试：15秒后
	}

	for {
		select {
		case <-s.syncCh:
			// 执行同步操作，支持重试
			syncErr := s.doSyncAllChannelsWithRetry(ctx, retryBackoff)
			if syncErr != nil {
				// 所有重试都失败，记录致命错误
				log.Printf("❌ 严重错误: Redis同步失败（已重试%d次）: %v", len(retryBackoff), syncErr)
				log.Print("   警告: 服务重启后可能丢失渠道配置，请检查Redis连接或手动备份数据库")
			}

		case <-s.done:
			// 优雅关闭：先取消context，然后处理最后一个任务（如果有）
			cancel()
			select {
			case <-s.syncCh:
				// 关闭时不重试，快速同步一次即可
				// 创建新的超时context，避免使用已取消的context
				shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = s.doSyncAllChannels(shutdownCtx)
				shutdownCancel()
			default:
			}
			return
		}
	}
}

// doSyncAllChannelsWithRetry 带重试机制的同步操作
func (s *MySQLStore) doSyncAllChannelsWithRetry(ctx context.Context, retryBackoff []time.Duration) error {
	var lastErr error

	// 首次尝试
	if err := s.doSyncAllChannels(ctx); err == nil {
		return nil // 成功
	} else {
		lastErr = err
		log.Printf("⚠️  Redis同步失败（将自动重试）: %v", err)
	}

	// 重试逻辑
	for attempt := 0; attempt < len(retryBackoff); attempt++ {
		// 等待退避时间
		time.Sleep(retryBackoff[attempt])

		// 重试同步
		if err := s.doSyncAllChannels(ctx); err == nil {
			log.Printf("✅ Redis同步恢复成功（第%d次重试）", attempt+1)
			return nil // 成功
		} else {
			lastErr = err
			log.Printf("⚠️  Redis同步重试失败（第%d次）: %v", attempt+1, err)
		}
	}

	// 所有重试都失败
	return fmt.Errorf("all %d retries failed: %w", len(retryBackoff), lastErr)
}

// triggerAsyncSync 触发异步Redis同步（非阻塞）
func (s *MySQLStore) triggerAsyncSync() {
	if s.redisSync == nil || !s.redisSync.IsEnabled() {
		return
	}

	// 非阻塞发送（如果channel已满则跳过，避免阻塞主流程）
	select {
	case s.syncCh <- struct{}{}:
		// 成功发送信号
	default:
		// channel已有待处理任务，跳过（去重）
	}
}

// doSyncAllChannels 实际执行同步操作（worker内部调用）
func (s *MySQLStore) doSyncAllChannels(ctx context.Context) error {
	// 1. 同步channels和API Keys
	if err := s.SyncAllChannelsToRedis(ctx); err != nil {
		return fmt.Errorf("sync channels: %w", err)
	}

	// 2. 同步auth_tokens
	if err := s.syncAuthTokensToRedis(ctx); err != nil {
		return fmt.Errorf("sync auth tokens: %w", err)
	}

	return nil
}

// SyncAllChannelsToRedis 将所有渠道同步到Redis
func (s *MySQLStore) SyncAllChannelsToRedis(ctx context.Context) error {
	if !s.redisSync.IsEnabled() {
		return nil
	}

	// 1. 查询所有渠道配置
	configs, err := s.ListConfigs(ctx)
	if err != nil {
		return fmt.Errorf("list configs: %w", err)
	}

	if len(configs) == 0 {
		log.Print("No channels to sync to Redis")
		return nil
	}

	// 2. 为每个渠道查询API Keys，构建完整数据结构
	channelsWithKeys := make([]*model.ChannelWithKeys, 0, len(configs))
	for _, config := range configs {
		// 查询该渠道的所有API Keys
		keys, err := s.GetAPIKeys(ctx, config.ID)
		if err != nil {
			log.Printf("Warning: failed to get API keys for channel %d: %v", config.ID, err)
			keys = []*model.APIKey{} // 降级处理：渠道没有Keys继续同步
		}

		// 转换为非指针切片
		apiKeys := make([]model.APIKey, len(keys))
		for i, k := range keys {
			apiKeys[i] = *k
		}

		channelsWithKeys = append(channelsWithKeys, &model.ChannelWithKeys{
			Config:  config,
			APIKeys: apiKeys,
		})
	}

	// 3. 同步到Redis
	if err := s.redisSync.SyncAllChannelsWithKeys(ctx, channelsWithKeys); err != nil {
		return fmt.Errorf("sync to redis: %w", err)
	}

	return nil
}

// syncAuthTokensToRedis 同步所有AuthToken到Redis
func (s *MySQLStore) syncAuthTokensToRedis(ctx context.Context) error {
	if !s.redisSync.IsEnabled() {
		return nil
	}

	// 读取所有令牌
	tokens, err := s.ListAuthTokens(ctx)
	if err != nil {
		return fmt.Errorf("list auth tokens: %w", err)
	}

	log.Printf("Syncing %d auth tokens to Redis...", len(tokens))

	// 同步到Redis
	if err := s.redisSync.SyncAllAuthTokens(ctx, tokens); err != nil {
		return err
	}

	if len(tokens) > 0 {
		log.Printf("✅ Successfully synced %d auth tokens to Redis", len(tokens))
	}

	return nil
}
