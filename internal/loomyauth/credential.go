/**
 * @brief 实现 Loomy 会话、短信身份验证及原生积分协议。
 */
package loomyauth

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

/**
 * @brief 定义 Loomy 官方服务端点与桌面客户端身份。
 */
const (
	ChannelType    = "loomy"                              ///< 额度协议来源，与认证类型 loomy_session 区分。
	BaseURL        = "https://loomyad.xunfei.cn"          ///< 模型、积分及任务控制接口的官方根地址。
	AccountURL     = "https://account.xfinfr.com"         ///< 短信挑战和账号校验接口的官方根地址。
	CompletionsURL = BaseURL + "/api/v1/chat/completions" ///< 完整聊天端点，不得再次拼接路径。
	ClientUA       = "Loomy|Desktop|Electron|macOS"       ///< 与桌面客户端协议匹配的请求身份。
)

/**
 * @brief 保存 Loomy 私有凭证、每日签到和账号级任务状态。
 * @details 每日状态与账号生命周期状态分离；没有已知续期接口，不根据请求中的 expire 估算本地强制过期时间。
 */
type Credential struct {
	Session               string          `json:"session"`                           ///< 上游登录会话，仅存私有凭证并用于认证请求。
	UserID                string          `json:"user_id"`                           ///< 短信响应或管理员 CSV 封套中的账号标识；CSV 模型校验不证明身份。
	LastCheckinDay        string          `json:"last_checkin_day,omitempty"`        ///< 上游确认的最近签到业务日，格式 YYYY-MM-DD；空值表示尚未确认。
	DailyQuota            *float64        `json:"daily_quota,omitempty"`             ///< 当天实测每日积分总额；nil 表示未知，不等同于零。
	DailyCycleDate        string          `json:"daily_cycle_date,omitempty"`        ///< DailyQuota 对应的业务日期，跨日不得沿用。
	OnboardingCompletedAt string          `json:"onboarding_completed_at,omitempty"` ///< 全部已知新手任务完成的 UTC 时间字符串；非空后跳过任务查询。
	NextCheckinAttemptAt  time.Time       `json:"next_checkin_attempt_at,omitzero"`  ///< 下次允许自动签到的绝对时间，持久化以避免重启提前重试。
	OAuthUsage            json.RawMessage `json:"oauth_usage,omitempty"`             ///< 共用 OAuth 额度缓存的原始 JSON，随凭证更新保留。
}

/**
 * @brief 解析并校验私有 Loomy 凭证封套。
 * @param raw 不超过 1 MiB 的凭证 JSON 字节。
 * @return 去除身份首尾空白的凭证；校验失败时为 nil。
 * @return 输入为空、超过 1 MiB、JSON 无效、会话或账号为空、会话含换行时返回错误；成功为 nil。
 * @note 只验证封套格式，不向上游验证身份；输入应来自已经验证的登录或受信存储。
 */
func ParseCredential(raw []byte) (*Credential, error) {
	var c Credential
	if len(raw) == 0 || len(raw) > 1<<20 || json.Unmarshal(raw, &c) != nil {
		return nil, errors.New("invalid Loomy credential")
	}
	c.Session = strings.TrimSpace(c.Session)
	c.UserID = strings.TrimSpace(c.UserID)
	if c.Session == "" || c.UserID == "" || strings.ContainsAny(c.Session, "\r\n") {
		return nil, errors.New("loomy session and verified user identity are required")
	}
	return &c, nil
}

/**
 * @brief 序列化完整私有凭证，保留签到、任务及额度缓存状态。
 * @return 完整凭证 JSON；nil 接收者编码为 null，编码失败时为空字符串。
 * @return 无法编码字段时返回 json.Marshal 的错误；成功为 nil。
 */
func (c *Credential) JSON() (string, error) {
	raw, err := json.Marshal(c)
	return string(raw), err
}
