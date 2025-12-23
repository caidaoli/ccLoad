package sql

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"ccLoad/internal/model"
)

// LoadChannelsFromRedis 从Redis恢复渠道数据到SQL (启动时数据库恢复机制)
// [INFO] 修复（2025-10-10）：完整恢复渠道和API Keys，解决Redis恢复后缺少Keys的问题
// [FIX] 2025-12：添加 nil 防御，避免测试中使用 nil redisSync 时 panic
func (s *SQLStore) LoadChannelsFromRedis(ctx context.Context) error {
	if s.redisSync == nil || !s.redisSync.IsEnabled() {
		return nil
	}

	// 从Redis加载所有渠道配置（含API Keys）
	channelsWithKeys, err := s.redisSync.LoadChannelsWithKeysFromRedis(ctx)
	if err != nil {
		return fmt.Errorf("load from redis: %w", err)
	}

	// ============================================================================
	// 恢复channels和API Keys (仅当有数据时执行)
	// ============================================================================
	if len(channelsWithKeys) > 0 {
		// 使用事务高阶函数，确保数据一致性（ACID原则 + DRY原则）
		nowUnix := timeToUnix(time.Now())
		successCount := 0
		totalKeysRestored := 0

		err = s.WithTransaction(ctx, func(tx *sql.Tx) error {
			for _, cwk := range channelsWithKeys {
				config := cwk.Config
				channelType := config.GetChannelType() // 强制使用默认值anthropic

				// 1. 恢复渠道基本配置到channels表（不含 models/model_redirects）
				result, err := tx.ExecContext(ctx, `
				REPLACE INTO channels(
					name, url, priority, channel_type,
					enabled, cooldown_until, cooldown_duration_ms, created_at, updated_at
				)
				VALUES(?, ?, ?, ?, ?, 0, 0, ?, ?)
			`, config.Name, config.URL, config.Priority, channelType,
					boolToInt(config.Enabled), nowUnix, nowUnix)

				if err != nil {
					log.Printf("Warning: failed to restore channel %s: %v", config.Name, err)
					continue
				}

				// 获取渠道ID（对于新插入或更新的记录）
				var channelID int64
				if config.ID > 0 {
					channelID = config.ID
				} else {
					channelID, _ = result.LastInsertId()
				}

				// 查询实际的渠道ID（因为REPLACE INTO可能使用name匹配）
				err = tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE name = ?`, config.Name).Scan(&channelID)
				if err != nil {
					log.Printf("Warning: failed to get channel ID for %s: %v", config.Name, err)
					continue
				}

				// 1.5 恢复模型条目到channel_models表
				if len(config.ModelEntries) > 0 {
					// 先删除旧记录
					_, _ = tx.ExecContext(ctx, `DELETE FROM channel_models WHERE channel_id = ?`, channelID)
					// 插入所有模型条目
					for _, entry := range config.ModelEntries {
						_, err := tx.ExecContext(ctx, `
							INSERT INTO channel_models (channel_id, model, redirect_model) VALUES (?, ?, ?)
						`, channelID, entry.Model, entry.RedirectModel)
						if err != nil {
							log.Printf("Warning: failed to restore model %s for channel %d: %v", entry.Model, channelID, err)
						}
					}
				}

				// 2. 恢复API Keys到api_keys表
				if len(cwk.APIKeys) > 0 {
					// 先删除该渠道的所有旧Keys（避免冲突）
					_, err := tx.ExecContext(ctx, `DELETE FROM api_keys WHERE channel_id = ?`, channelID)
					if err != nil {
						log.Printf("Warning: failed to clear old API keys for channel %d: %v", channelID, err)
					}

					// 插入所有API Keys
					for _, key := range cwk.APIKeys {
						_, err := tx.ExecContext(ctx, `
						INSERT INTO api_keys (channel_id, key_index, api_key, key_strategy,
						                      cooldown_until, cooldown_duration_ms, created_at, updated_at)
						VALUES (?, ?, ?, ?, ?, ?, ?, ?)
					`, channelID, key.KeyIndex, key.APIKey, key.KeyStrategy,
							key.CooldownUntil, key.CooldownDurationMs, nowUnix, nowUnix)

						if err != nil {
							log.Printf("Warning: failed to restore API key %d for channel %d: %v", key.KeyIndex, channelID, err)
							continue
						}
						totalKeysRestored++
					}
				}

				successCount++
			}
			return nil
		})

		if err != nil {
			return err
		}

		log.Printf("Successfully restored %d/%d channels and %d API Keys from Redis",
			successCount, len(channelsWithKeys), totalKeysRestored)
	} else {
		log.Print("No channels found in Redis")
	}

	// ============================================================================
	// 恢复auth_tokens表 (新增 2025-11)
	// 注意: 即使没有channels，也要尝试恢复auth_tokens
	// ============================================================================
	tokensRestored, err := s.loadAuthTokensFromRedis(ctx)
	if err != nil {
		return fmt.Errorf("failed to restore auth tokens from Redis: %w", err)
	}
	if tokensRestored > 0 {
		log.Printf("Successfully restored %d auth tokens from Redis", tokensRestored)
	}

	return nil
}

// syncAllChannelsToRedis 将所有渠道同步到Redis (批量同步，初始化时使用)
// [INFO] 修复（2025-10-10）：完整同步渠道配置和API Keys，解决Redis恢复后缺少Keys的问题
// [FIX] 2025-12：允许同步空集合，避免删除所有渠道后Redis保留旧数据导致恢复时复活
func (s *SQLStore) syncAllChannelsToRedis(ctx context.Context) error {
	if !s.redisSync.IsEnabled() {
		return nil
	}

	// 1. 查询所有渠道配置
	configs, err := s.ListConfigs(ctx)
	if err != nil {
		return fmt.Errorf("list configs: %w", err)
	}

	// 空集合也需要同步（确保删除操作能正确同步到Redis）
	if len(configs) == 0 {
		log.Print("Syncing empty channel list to Redis")
		return s.redisSync.SyncAllChannelsWithKeys(ctx, []*model.ChannelWithKeys{})
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

		// 转换为非指针切片（避免额外内存分配）
		apiKeys := make([]model.APIKey, len(keys))
		for i, k := range keys {
			apiKeys[i] = *k
		}

		channelsWithKeys = append(channelsWithKeys, &model.ChannelWithKeys{
			Config:  config,
			APIKeys: apiKeys,
		})
	}

	// 3. 规范化所有Config对象的默认值（确保Redis中数据完整性）
	normalizeChannelsWithKeys(channelsWithKeys)

	// 4. 同步到Redis
	if err := s.redisSync.SyncAllChannelsWithKeys(ctx, channelsWithKeys); err != nil {
		return fmt.Errorf("sync to redis: %w", err)
	}

	return nil
}

// redisSyncWorker 异步Redis同步worker（后台goroutine）
// 支持细粒度同步：根据 pendingSyncTypes 选择性执行同步操作
// [FIX] P0-3: 使用 defer wg.Done() 确保资源释放，即使 panic 也能保证
func (s *SQLStore) redisSyncWorker() {
	// 无条件释放 WaitGroup，避免 Close() 死锁
	defer s.wg.Done()

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
			// 原子读取并清零待同步类型
			pending := syncType(s.pendingSyncTypes.Swap(0))
			if pending == 0 {
				continue // 无待同步任务
			}

			// 执行同步操作，支持重试
			syncErr := s.doSyncWithRetry(ctx, pending, retryBackoff)
			if syncErr != nil {
				// 所有重试都失败，记录致命错误
				log.Printf("[ERROR] Redis同步失败（已重试%d次）: %v", len(retryBackoff), syncErr)
				log.Print("[WARN]  服务重启后可能丢失渠道配置，请检查Redis连接或手动备份数据库")
			}

		case <-s.done:
			// 优雅关闭：先取消context，阻止重试
			cancel()
			// 直接检查 pending 位（不依赖 channel 是否有信号）
			// syncCh 只是唤醒机制，pending 才是任务存在性判据
			if pending := syncType(s.pendingSyncTypes.Swap(0)); pending != 0 {
				// 有未完成的同步任务，执行最终全量同步
				shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = s.doSyncByType(shutdownCtx, syncAll) // 关闭时全量同步
				shutdownCancel()
			}
			// defer wg.Done() 会在 return 后自动执行，无需手动调用
			return
		}
	}
}

// doSyncWithRetry 带重试机制的同步操作
// syncType: 位标记，指定需要同步的数据类型
func (s *SQLStore) doSyncWithRetry(ctx context.Context, syncType syncType, retryBackoff []time.Duration) error {
	const syncTimeout = 30 * time.Second // 单次同步操作超时
	var lastErr error

	// 首次尝试（带超时）
	syncCtx, cancel := context.WithTimeout(ctx, syncTimeout)
	err := s.doSyncByType(syncCtx, syncType)
	cancel()
	if err == nil {
		return nil // 成功
	}
	lastErr = err
	log.Printf("[WARN]  Redis同步失败（将自动重试）: %v", err)

	// 重试逻辑
	for attempt := 0; attempt < len(retryBackoff); attempt++ {
		// 可取消的退避等待（支持优雅关闭）
		select {
		case <-time.After(retryBackoff[attempt]):
			// 正常等待完成
		case <-ctx.Done():
			return ctx.Err() // 关闭时立即退出，不阻塞
		}

		// 重试同步（带超时）
		syncCtx, cancel := context.WithTimeout(ctx, syncTimeout)
		err := s.doSyncByType(syncCtx, syncType)
		cancel()
		if err == nil {
			log.Printf("[INFO] Redis同步恢复成功（第%d次重试）", attempt+1)
			return nil // 成功
		}
		lastErr = err
		log.Printf("[WARN]  Redis同步重试失败（第%d次）: %v", attempt+1, err)
	}

	// 所有重试都失败
	return fmt.Errorf("all %d retries failed: %w", len(retryBackoff), lastErr)
}

// triggerAsyncSync 触发异步Redis同步（非阻塞）
// syncType: 指定需要同步的数据类型（位标记，可组合）
func (s *SQLStore) triggerAsyncSync(syncType syncType) {
	if syncType == 0 {
		return // 短路：无需同步
	}
	if s.redisSync == nil || !s.redisSync.IsEnabled() {
		return
	}

	// 原子合并同步类型（支持多个请求的类型合并）
	s.pendingSyncTypes.Or(uint32(syncType))

	// 非阻塞发送（如果channel已满则跳过，避免阻塞主流程）
	select {
	case s.syncCh <- struct{}{}:
		// 成功发送信号
	default:
		// channel已有待处理任务，跳过（去重）
	}
}

// doSyncByType 根据类型执行同步操作（worker内部调用）
// syncType: 位标记，指定需要同步的数据类型
func (s *SQLStore) doSyncByType(ctx context.Context, syncType syncType) error {
	// 1. 同步 channels 和 API Keys
	if syncType&syncChannels != 0 {
		if err := s.syncAllChannelsToRedis(ctx); err != nil {
			return fmt.Errorf("sync channels: %w", err)
		}
	}

	// 2. 同步 auth_tokens
	if syncType&syncAuthTokens != 0 {
		if err := s.syncAuthTokensToRedis(ctx); err != nil {
			return fmt.Errorf("sync auth tokens: %w", err)
		}
	}

	return nil
}

// syncAuthTokensToRedis 同步所有AuthToken到Redis (内部方法)
// [INFO] 新增（2025-11）：完整同步认证令牌表
// [FIX] 2025-12：允许同步空集合，避免删除最后一个token后Redis保留旧数据导致恢复时复活已吊销token
func (s *SQLStore) syncAuthTokensToRedis(ctx context.Context) error {
	if !s.redisSync.IsEnabled() {
		return nil
	}

	// 读取所有令牌（包括过期和禁用的，确保完整备份）
	tokens, err := s.ListAuthTokens(ctx)
	if err != nil {
		return fmt.Errorf("list auth tokens: %w", err)
	}

	// 同步到Redis（包括空集合，确保删除操作能正确同步）
	if err := s.redisSync.SyncAllAuthTokens(ctx, tokens); err != nil {
		return err
	}

	log.Printf("[INFO] Successfully synced %d auth tokens to Redis", len(tokens))
	return nil
}

// loadAuthTokensFromRedis 从Redis恢复所有AuthToken到SQL (内部方法)
// [INFO] 新增（2025-11）：支持auth_tokens表的灾难恢复
// 返回: 成功恢复的令牌数量
func (s *SQLStore) loadAuthTokensFromRedis(ctx context.Context) (int, error) {
	if !s.redisSync.IsEnabled() {
		return 0, nil
	}

	// 从Redis加载所有令牌
	tokens, err := s.redisSync.LoadAuthTokensFromRedis(ctx)
	if err != nil {
		return 0, err
	}

	if len(tokens) == 0 {
		log.Print("No auth tokens found in Redis to restore")
		return 0, nil
	}

	// 使用REPLACE INTO批量恢复（包含所有字段）
	restoredCount := 0
	for _, token := range tokens {
		var expiresAt, lastUsedAt any
		if token.ExpiresAt != nil {
			expiresAt = *token.ExpiresAt
		}
		if token.LastUsedAt != nil {
			lastUsedAt = *token.LastUsedAt
		}

		_, err := s.db.ExecContext(ctx, `
			REPLACE INTO auth_tokens
			(id, token, description, created_at, expires_at, last_used_at, is_active,
			 success_count, failure_count, stream_avg_ttfb, non_stream_avg_rt,
			 stream_count, non_stream_count, prompt_tokens_total, completion_tokens_total,
			 cache_read_tokens_total, cache_creation_tokens_total, total_cost_usd)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, token.ID, token.Token, token.Description, token.CreatedAt.UnixMilli(),
			expiresAt, lastUsedAt, token.IsActive,
			token.SuccessCount, token.FailureCount, token.StreamAvgTTFB, token.NonStreamAvgRT,
			token.StreamCount, token.NonStreamCount, token.PromptTokensTotal, token.CompletionTokensTotal,
			token.CacheReadTokensTotal, token.CacheCreationTokensTotal, token.TotalCostUSD)

		if err != nil {
			log.Printf("Warning: failed to restore auth token %d: %v", token.ID, err)
			continue
		}
		restoredCount++
	}

	return restoredCount, nil
}

// normalizeChannelsWithKeys 规范化ChannelWithKeys对象的默认值（2025-10-10新增）
// 确保Redis序列化时所有字段完整，支持API Keys的完整同步
func normalizeChannelsWithKeys(channelsWithKeys []*model.ChannelWithKeys) {
	for _, cwk := range channelsWithKeys {
		// 规范化Config部分
		if cwk.Config.ChannelType == "" {
			cwk.Config.ChannelType = "anthropic"
		}
		// ModelEntries 不需要额外规范化，空切片即为默认值

		// 规范化APIKeys部分：确保key_strategy默认值
		for i := range cwk.APIKeys {
			if cwk.APIKeys[i].KeyStrategy == "" {
				cwk.APIKeys[i].KeyStrategy = model.KeyStrategySequential
			}
		}
	}
}

// CheckChannelsEmpty 检查channels表是否为空
func (s *SQLStore) CheckChannelsEmpty(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM channels`).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check channels count: %w", err)
	}
	return count == 0, nil
}

// CheckAuthTokensEmpty 检查auth_tokens表是否为空
// [FIX] P1-4: 新增方法，支持独立检查 auth_tokens 恢复需求
func (s *SQLStore) CheckAuthTokensEmpty(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_tokens`).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check auth_tokens count: %w", err)
	}
	return count == 0, nil
}
