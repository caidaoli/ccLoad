package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"ccLoad/internal/model"
)

/**
 * @brief 凭证比较和冷却写入在同一事务内完成，旧账号响应不能污染新账号。
 * @param ctx 数据库操作上下文。
 * @param channelID 冷却所属渠道 ID。
 * @param expectedAuthType 发起请求时的 OAuth 认证类型，会先归一化。
 * @param expectedCredential 发起请求时的完整凭证 JSON 快照。
 * @param name 模型冷却键，不得为空。
 * @param until 冷却截止时间。
 * @return 身份匹配且成功写入时为 true；渠道不存在或身份变化时为 false。
 * @return 身份或模型输入非法、事务失败时返回错误；成功或身份不匹配时为 nil。
 * @note 事务内锁定渠道身份后比较凭证并写入冷却，防止重授权竞争。
 */
func (s *SQLStore) SetModelCooldownIfOAuthCredential(ctx context.Context, channelID int64, expectedAuthType, expectedCredential, name string, until time.Time) (bool, error) {
	authType := model.NormalizeAuthType(expectedAuthType)
	if authType == "" || model.UsesAPIKeyCredentials(authType) || strings.TrimSpace(expectedCredential) == "" {
		return false, errors.New("invalid expected OAuth identity")
	}
	query, args, err := s.modelCooldownUpsert(channelID, name, until)
	if err != nil {
		return false, err
	}
	matched := false
	err = s.WithTransaction(ctx, func(tx *sql.Tx) error {
		matched = false
		currentAuthType, currentCredential, err := s.loadOAuthCredentialForUpdate(ctx, tx, channelID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if currentAuthType != authType || currentCredential != expectedCredential {
			return nil
		}
		if _, err := s.execTx(ctx, tx, query, args...); err != nil {
			return fmt.Errorf("set model cooldown: %w", err)
		}
		matched = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return matched, nil
}
