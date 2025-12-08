package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"ccLoad/internal/util"
)

// LoadChannelsFromRedis 从Redis恢复所有渠道配置和API Keys
// 与SQLite版本保持一致，支持完整的数据恢复
func (s *MySQLStore) LoadChannelsFromRedis(ctx context.Context) error {
	if !s.redisSync.IsEnabled() {
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
		nowUnix := time.Now().Unix()
		successCount := 0
		totalKeysRestored := 0

		err = s.WithTransaction(ctx, func(tx *sql.Tx) error {
			for _, cwk := range channelsWithKeys {
				config := cwk.Config

				// 标准化数据：确保默认值正确填充
				modelsStr, _ := util.SerializeJSON(config.Models, "[]")
				modelRedirectsStr, _ := util.SerializeJSON(config.ModelRedirects, "{}")
				channelType := config.GetChannelType() // 强制使用默认值anthropic

				// 1. 恢复渠道基本配置到channels表（使用REPLACE INTO）
				result, err := tx.ExecContext(ctx, `
					REPLACE INTO channels(
						name, url, priority, models, model_redirects, channel_type,
						enabled, cooldown_until, cooldown_duration_ms, created_at, updated_at
					)
					VALUES(?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)
				`, config.Name, config.URL, config.Priority,
					modelsStr, modelRedirectsStr, channelType,
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

				// 查询实际的渠道ID（因为REPLACE可能使用name匹配）
				err = tx.QueryRowContext(ctx, `SELECT id FROM channels WHERE name = ?`, config.Name).Scan(&channelID)
				if err != nil {
					log.Printf("Warning: failed to get channel ID for %s: %v", config.Name, err)
					continue
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
	// 恢复auth_tokens表
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

// loadAuthTokensFromRedis 从Redis恢复所有认证令牌
func (s *MySQLStore) loadAuthTokensFromRedis(ctx context.Context) (int, error) {
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

	// 使用REPLACE批量恢复（包含所有字段）
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
			 stream_count, non_stream_count, prompt_tokens_total, completion_tokens_total, total_cost_usd)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, token.ID, token.Token, token.Description, token.CreatedAt.UnixMilli(),
			expiresAt, lastUsedAt, token.IsActive,
			token.SuccessCount, token.FailureCount, token.StreamAvgTTFB, token.NonStreamAvgRT,
			token.StreamCount, token.NonStreamCount, token.PromptTokensTotal, token.CompletionTokensTotal, token.TotalCostUSD)

		if err != nil {
			log.Printf("Warning: failed to restore auth token %d: %v", token.ID, err)
			continue
		}
		restoredCount++
	}

	return restoredCount, nil
}

// CheckChannelsEmpty 检查channels表是否为空
func (s *MySQLStore) CheckChannelsEmpty(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM channels`).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check channels count: %w", err)
	}
	return count == 0, nil
}
