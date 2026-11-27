package cursorauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"
)

/**
 * @brief 渠道私有存储的 Cursor 凭证和模型目录元数据。
 * @details AccessToken 用于控制面 RPC。Cursor 未提供该会话的公开刷新接口，失效时用 APIKey 重新换取。
 */
type Credential struct {
	Type           string              `json:"type"`                      ///< 凭证类型。
	AccessToken    string              `json:"access_token"`              ///< Cursor CLI 会话 JWT。
	RefreshToken   string              `json:"refresh_token,omitempty"`   ///< CLI 持久化的配套刷新令牌。
	APIKey         string              `json:"api_key,omitempty"`         ///< User API Key，用于 SDK 调用及会话重铸。
	UserID         string              `json:"user_id,omitempty"`         ///< Cursor 用户 ID。
	Email          string              `json:"email,omitempty"`           ///< Cursor 账号邮箱。
	Name           string              `json:"name,omitempty"`            ///< Cursor 账号显示名。
	LastRefresh    string              `json:"last_refresh,omitempty"`    ///< 以 RFC 3339 UTC 记录的上次会话刷新时间。
	OAuthUsage     json.RawMessage     `json:"oauth_usage,omitempty"`     ///< 最近一次采集的当前周期用量。
	ContextWindows map[string]int      `json:"context_windows,omitempty"` ///< 同一 API Key 最近确认的模型窗口，以 token 计。
	EffortLevels   map[string][]string `json:"effort_levels,omitempty"`   ///< 同一 API Key 最近确认的思考档位；空列表为无档位，缺失为未知。
	DefaultEfforts map[string]string   `json:"default_efforts,omitempty"` ///< 同一账号目录明确标记的默认思考档位。
}

// Identity is the account this credential belongs to.
type Identity struct {
	UserID string
	Email  string
}

// ParseCredential decodes and normalizes one persisted Cursor credential.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, errors.New("cursor credential is empty")
	}
	if len(raw) > maxCredentialSize {
		return nil, fmt.Errorf("cursor credential exceeds %d bytes", maxCredentialSize)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	var credential Credential
	if err := decoder.Decode(&credential); err != nil {
		return nil, fmt.Errorf("decode cursor credential: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("cursor credential contains trailing JSON")
	}
	if err := credential.Normalize(); err != nil {
		return nil, err
	}
	return &credential, nil
}

// Normalize validates and canonicalizes a credential in place.
func (c *Credential) Normalize() error {
	if c == nil {
		return errors.New("cursor credential is nil")
	}
	c.Type = strings.ToLower(strings.TrimSpace(c.Type))
	if c.Type == "" || c.Type == "cursor-cli" {
		c.Type = ChannelType
	}
	if c.Type != ChannelType {
		return fmt.Errorf("unsupported cursor credential type %q", c.Type)
	}
	c.AccessToken = strings.TrimSpace(c.AccessToken)
	c.RefreshToken = strings.TrimSpace(c.RefreshToken)
	c.APIKey = strings.TrimSpace(c.APIKey)
	c.UserID = strings.TrimSpace(c.UserID)
	c.Email = strings.TrimSpace(c.Email)
	c.Name = strings.TrimSpace(c.Name)
	c.LastRefresh = strings.TrimSpace(c.LastRefresh)
	if c.AccessToken == "" {
		return errors.New("cursor credential is missing access_token")
	}
	if strings.ContainsAny(c.AccessToken, " \r\n\t") {
		return errors.New("cursor credential has an invalid access_token")
	}
	if c.LastRefresh != "" {
		parsed, err := time.Parse(time.RFC3339, c.LastRefresh)
		if err != nil {
			return errors.New("cursor credential has invalid last_refresh")
		}
		c.LastRefresh = parsed.UTC().Format(time.RFC3339)
	}
	return nil
}

// Identity returns the account identity used to deduplicate channels.
func (c *Credential) Identity() Identity {
	if c == nil {
		return Identity{}
	}
	return Identity{UserID: strings.TrimSpace(c.UserID), Email: strings.TrimSpace(c.Email)}
}

// IsZero reports whether an identity can match another credential at all.
func (i Identity) IsZero() bool {
	return strings.TrimSpace(i.UserID) == "" && strings.TrimSpace(i.Email) == ""
}

// Matches reports whether two identities describe the same Cursor account.
func (i Identity) Matches(other Identity) bool {
	if i.UserID != "" && other.UserID != "" {
		return i.UserID == other.UserID
	}
	return i.Email != "" && other.Email != "" && strings.EqualFold(i.Email, other.Email)
}

/**
 * @brief 合并刷新结果，保留该次刷新未覆盖的身份和目录信息。
 * @param refreshed 刷新后的凭证；nil 表示没有刷新结果。
 * @return 规范化后的新凭证；失败时为 nil。
 * @return 任一凭证为空或合并后凭证无效时返回错误。
 * @details 只有同一 API Key 的刷新可以沿用目录；新快照同时替换窗口与档位。
 */
func (c *Credential) MergeRefresh(refreshed *Credential) (*Credential, error) {
	if c == nil || refreshed == nil {
		return nil, errors.New("cursor refresh credential is nil")
	}
	merged := *refreshed
	preserve := func(target *string, previous string) {
		if strings.TrimSpace(*target) == "" {
			*target = previous
		}
	}
	preserve(&merged.RefreshToken, c.RefreshToken)
	preserve(&merged.APIKey, c.APIKey)
	preserve(&merged.UserID, c.UserID)
	preserve(&merged.Email, c.Email)
	preserve(&merged.Name, c.Name)
	merged.OAuthUsage = c.OAuthUsage
	if err := merged.Normalize(); err != nil {
		return nil, err
	}
	if merged.ContextWindows == nil && merged.EffortLevels == nil && merged.DefaultEfforts == nil &&
		merged.APIKey != "" && merged.APIKey == strings.TrimSpace(c.APIKey) {
		merged.ContextWindows = c.ContextWindows
		merged.EffortLevels = c.EffortLevels
		merged.DefaultEfforts = c.DefaultEfforts
	}
	return merged.Clone(), nil
}

/**
 * @brief 复制 Cursor 凭证、额度以及目录快照，避免调用方修改共享状态。
 * @return 独立凭证副本；源凭证为 nil 时返回 nil。
 */
func (c *Credential) Clone() *Credential {
	if c == nil {
		return nil
	}
	clone := *c
	clone.OAuthUsage = slices.Clone(c.OAuthUsage)
	clone.ContextWindows = maps.Clone(c.ContextWindows)
	clone.EffortLevels = maps.Clone(c.EffortLevels)
	clone.DefaultEfforts = maps.Clone(c.DefaultEfforts)
	for id, levels := range clone.EffortLevels {
		clone.EffortLevels[id] = slices.Clone(levels)
	}
	return &clone
}

/**
 * @brief 编码可由 ParseCredential 读取的私有凭证。
 * @return 存入数据库的规范化 JSON；失败时为空字符串。
 * @return 凭证无效、编码失败或超出大小限制时返回错误。
 * @note 规范化时修改凭证。
 */
func (c *Credential) JSON() (string, error) {
	if err := c.Normalize(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode cursor credential: %w", err)
	}
	if len(raw) > maxCredentialSize {
		return "", fmt.Errorf("cursor credential exceeds %d bytes", maxCredentialSize)
	}
	return string(raw), nil
}
