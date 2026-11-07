package codexauth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

/**
 * @brief 在权威凭据存储中永久记录刷新 grant 的提交与结果
 * @note claim 必须原子创建；未完成记录不允许回收或按时间重试，回执含私密凭据。
 */
type RefreshJournal interface {
	/**
	 * @brief 在发送 grant 前原子创建未完成记录
	 * @param ctx 存储操作上下文
	 * @param key RefreshGrantKey 返回的 grant 指纹
	 * @return 首次取得提交权时返回 true，已有记录时返回 false
	 * @return 存储失败时返回错误，此时不得发送刷新请求
	 */
	RefreshClaim(ctx context.Context, key string) (bool, error)
	/**
	 * @brief 读取既有 grant 的持久化回执
	 * @param ctx 存储操作上下文
	 * @param key grant 指纹
	 * @return 回执原文；未完成或不存在时返回 nil
	 * @return grant 存在时返回 true，包括未完成的 claim
	 * @return 存储读取失败时返回错误
	 */
	RefreshRead(ctx context.Context, key string) ([]byte, bool, error)
	/**
	 * @brief 将取得提交权的 grant 首次写入最终结果
	 * @param ctx 存储操作上下文
	 * @param key grant 指纹
	 * @param receipt 私密 JSON 回执
	 * @return 写入失败或记录已完成时返回错误
	 */
	RefreshComplete(ctx context.Context, key string, receipt []byte) error
}

/**
 * @brief 标记无法确认是否已消耗的刷新 grant
 * @note 诊断仅暴露失败阶段；原始网络和存储错误通过 Unwrap 保留。
 */
type RefreshOutcomeUnknownError struct {
	stage string ///< 不确定结果产生的阶段
	cause error  ///< 原始错误，可为空
}

/**
 * @brief 返回不含 token 和上游正文的恢复提示
 * @return 刷新结果不确定的阶段和恢复说明
 */
func (e *RefreshOutcomeUnknownError) Error() string {
	return "codex refresh outcome unknown at " + e.stage + "; recover refreshed credentials or sign in again"
}

/**
 * @brief 保留原始错误供取消和存储故障分类
 * @return 原始错误；无底层错误时返回 nil
 */
func (e *RefreshOutcomeUnknownError) Unwrap() error { return e.cause }

/**
 * @brief 判断错误是否包含不可重放的刷新结果
 * @param err 刷新或存储错误
 * @return 包含不确定刷新结果时返回 true
 */
func IsRefreshOutcomeUnknown(err error) bool {
	_, ok := errors.AsType[*RefreshOutcomeUnknownError](err)
	return ok
}

/**
 * @brief 标识回执中已确认被拒绝的 grant
 */
type refreshGrantRejectedError struct{}

/**
 * @brief 返回不含上游正文的永久拒绝说明
 * @return 需要重新授权的提示
 */
func (*refreshGrantRejectedError) Error() string {
	return "codex refresh grant was rejected; sign in again"
}

/**
 * @brief 在不同渠道和重新导入之间标识同一刷新 grant
 * @param clientID OAuth 客户端标识
 * @param refreshToken 已规范化的刷新 token
 * @return SHA256 十六进制指纹，不包含明文凭据
 */
func RefreshGrantKey(clientID, refreshToken string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(clientID+"\x00"+refreshToken)))
}

/**
 * @brief 查询 grant 是否需要恢复或重新授权，不触发网络交换
 * @param ctx 回执查询上下文
 * @param journal 权威刷新记录
 * @param clientID OAuth 客户端标识
 * @param refreshToken 当前凭据的刷新 token
 * @return 未完成、拒绝、不确定或过期的刷新记录返回 true；尚未提交或有有效回执返回 false
 * @return 存储读取失败时返回错误
 */
func RefreshRecoveryRequired(ctx context.Context, journal RefreshJournal, clientID, refreshToken string) (bool, error) {
	_, _, err := readRefreshReceipt(ctx, journal, RefreshGrantKey(clientID, refreshToken), clientID)
	if outcome, ok := errors.AsType[*RefreshOutcomeUnknownError](err); ok && outcome.stage == "receipt_read" {
		return false, err
	}
	return err != nil, nil
}

/**
 * @brief 保存一次刷新交换的私密结果
 */
type refreshReceipt struct {
	Credential *Credential `json:"credential,omitempty"` ///< 成功交换的凭据
	Rejected   bool        `json:"rejected,omitempty"`   ///< 上游已明确拒绝 grant
	Stage      string      `json:"stage,omitempty"`      ///< 不确定结果的阶段
}

/**
 * @brief 复用已完成的轮换结果，禁止提交已有 claim 的 grant
 * @param ctx 回执读取上下文
 * @param journal 权威持久化存储
 * @param key 已存在的 grant 指纹
 * @param clientID OAuth 客户端标识
 * @return 最新未过期成功回执；不确定或永久拒绝时返回 nil
 * @return 起始 grant 存在记录时返回 true
 * @return 存储失败、未完成、损坏、循环或过期的回执返回不确定错误，已拒绝 grant 返回永久错误
 */
func readRefreshReceipt(ctx context.Context, journal RefreshJournal, key, clientID string) (*Credential, bool, error) {
	seen := make(map[string]bool)
	var latest *Credential
	for {
		if seen[key] {
			return nil, true, &RefreshOutcomeUnknownError{stage: "receipt_cycle"}
		}
		seen[key] = true
		raw, exists, err := journal.RefreshRead(ctx, key)
		if err != nil {
			return nil, true, &RefreshOutcomeUnknownError{stage: "receipt_read", cause: err}
		}
		if !exists {
			if latest != nil {
				break
			}
			return nil, false, nil
		}
		if len(raw) == 0 {
			return nil, true, &RefreshOutcomeUnknownError{stage: "pending"}
		}
		var receipt refreshReceipt
		if err := json.Unmarshal(raw, &receipt); err != nil {
			return nil, true, &RefreshOutcomeUnknownError{stage: "receipt_decode", cause: err}
		}
		if receipt.Stage != "" {
			return nil, true, &RefreshOutcomeUnknownError{stage: "recorded_failure"}
		}
		if receipt.Rejected {
			return nil, true, &refreshGrantRejectedError{}
		}
		if receipt.Credential == nil {
			return nil, true, &RefreshOutcomeUnknownError{stage: "receipt_validate"}
		}
		if err := receipt.Credential.Normalize(); err != nil {
			return nil, true, &RefreshOutcomeUnknownError{stage: "receipt_validate", cause: err}
		}
		latest = receipt.Credential
		nextKey := RefreshGrantKey(clientID, latest.RefreshToken)
		if nextKey == key {
			break
		}
		key = nextKey
	}
	expiresAt, err := latest.Expiry()
	if err != nil || !expiresAt.After(time.Now()) {
		return nil, true, &RefreshOutcomeUnknownError{stage: "receipt_expired", cause: err}
	}
	return latest, true, nil
}
