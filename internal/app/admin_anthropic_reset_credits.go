package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"ccLoad/internal/anthropicauth"

	"github.com/gin-gonic/gin"
)

var anthropicResetGrantIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)

/**
 * @brief 不含上游标识的持有重置券；限制字段仅保留上游信息，不推断兑换资格
 */
type anthropicResetCredit struct {
	Label            string             `json:"label"`                        ///< 上游显示名称
	ResetsLeft       int                `json:"resets_left"`                  ///< 剩余兑换次数
	StartsAt         *time.Time         `json:"starts_at,omitempty"`          ///< 生效时间；缺失表示未限定
	ExpiresAt        *time.Time         `json:"expires_at,omitempty"`         ///< 到期时间；缺失表示未限定
	Clears           []string           `json:"clears"`                       ///< 上游声明的重置窗口
	PercentUsed      map[string]float64 `json:"percent_used"`                 ///< 上游窗口使用百分比
	Blocking         []string           `json:"blocking"`                     ///< 上游声明的阻碍条件，不作为本地兑换门槛
	UseRequiresLimit *bool              `json:"use_requires_limit,omitempty"` ///< 上游声明的额度限制要求；缺失表示未知
}

/**
 * @brief 持有券及上游元数据快照；实际可否兑换以兑换响应为准
 */
type anthropicResetCredits struct {
	Eligible       *bool                  `json:"eligible,omitempty"`         ///< 上游资格声明；缺失表示未知
	Credits        []anthropicResetCredit `json:"credits"`                    ///< 持有券；有效 next grant 优先，其余按上游顺序
	CooldownUntil  *time.Time             `json:"cooldown_until,omitempty"`   ///< 上游冷却截止时间，不作为本地兑换门槛
	WeeklyResetsAt *time.Time             `json:"weekly_resets_at,omitempty"` ///< 上游周重置时间
	FetchedAt      time.Time              `json:"fetched_at"`                 ///< 查询时间（UTC）
}

/**
 * @brief 上游重置券原始数据；ID 仅用于服务端选券和兑换
 */
type anthropicResetGrant struct {
	ID               string             `json:"id"`                 ///< 上游券标识
	Label            string             `json:"label"`              ///< 上游显示名称
	ResetsLeft       int                `json:"resets_left"`        ///< 剩余兑换次数
	StartsAt         *time.Time         `json:"starts_at"`          ///< 生效时间；缺失表示未限定
	EndsAt           *time.Time         `json:"ends_at"`            ///< 到期时间；缺失表示未限定
	Clears           []string           `json:"clears"`             ///< 上游声明的重置窗口
	Paused           bool               `json:"paused"`             ///< 是否暂停该券
	UseRequiresLimit *bool              `json:"use_requires_limit"` ///< 上游声明的额度限制要求；缺失表示未知
	PercentUsed      map[string]float64 `json:"percent_used"`       ///< 上游窗口使用百分比
	Blocking         []string           `json:"blocking"`           ///< 上游声明的阻碍条件
}

/**
 * @brief 上游重置券集合及选券提示，不以资格或冷却信息阻止兑换
 */
type anthropicResetBlock struct {
	Eligible       *bool                 `json:"eligible"`         ///< 上游资格声明；缺失表示未知
	Grants         []anthropicResetGrant `json:"grants"`           ///< 上游券集合
	NextGrantID    string                `json:"next_grant_id"`    ///< 上游建议优先兑换的券标识
	CooldownUntil  *time.Time            `json:"cooldown_until"`   ///< 上游冷却截止时间
	WeeklyResetsAt *time.Time            `json:"weekly_resets_at"` ///< 上游周重置时间
}

func parseAnthropicResetBlock(body []byte) (*anthropicResetBlock, error) {
	invalid := errors.New("invalid Anthropic reset status")
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil || envelope == nil {
		return nil, invalid
	}
	if _, hasError := envelope["error"]; hasError {
		return nil, invalid
	}
	raw, present := envelope["cedar_ember"]
	if !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var block anthropicResetBlock
	if err := json.Unmarshal(raw, &block); err != nil || block.Grants == nil {
		return nil, errors.New("invalid Anthropic reset grants")
	}
	return &block, nil
}

/**
 * @brief 返回脱敏持有券视图，不派生本地可用次数或可兑换结论
 * @param block 上游券集合；nil 表示上游未提供重置券
 * @param now 查询时间，用于判断券是否已生效或到期
 * @return 持有券视图；无持有券时 credits 为空数组
 */
func projectAnthropicResetCredits(block *anthropicResetBlock, now time.Time) *anthropicResetCredits {
	result := &anthropicResetCredits{Credits: []anthropicResetCredit{}, FetchedAt: now.UTC()}
	if block == nil {
		return result
	}
	result.Eligible = block.Eligible
	result.CooldownUntil = block.CooldownUntil
	result.WeeklyResetsAt = block.WeeklyResetsAt
	for _, grant := range anthropicResetHeldGrants(block, now) {
		percentUsed := map[string]float64{}
		for name, value := range grant.PercentUsed {
			if validOAuthUsedPercent(value) {
				percentUsed[name] = value
			}
		}
		result.Credits = append(result.Credits, anthropicResetCredit{
			Label: grant.Label, ResetsLeft: grant.ResetsLeft, StartsAt: grant.StartsAt, ExpiresAt: grant.EndsAt,
			Clears: grant.Clears, PercentUsed: percentUsed, Blocking: grant.Blocking,
			UseRequiresLimit: grant.UseRequiresLimit,
		})
	}
	return result
}

func anthropicResetGrantHeld(grant anthropicResetGrant, now time.Time) bool {
	return anthropicResetGrantIDPattern.MatchString(grant.ID) && len(grant.Clears) > 0 && grant.ResetsLeft > 0 && !grant.Paused &&
		(grant.StartsAt == nil || !now.Before(*grant.StartsAt)) && (grant.EndsAt == nil || now.Before(*grant.EndsAt))
}

/**
 * @brief 为查询展示和兑换返回相同顺序的持有券，有效 next grant 优先
 * @param block 上游券集合；nil 表示未提供重置券
 * @param now 判断券是否已生效或到期的时间
 * @return 持有券；无持有券时为空，除优先券外保留上游顺序
 */
func anthropicResetHeldGrants(block *anthropicResetBlock, now time.Time) []anthropicResetGrant {
	if block == nil {
		return nil
	}
	held := make([]anthropicResetGrant, 0, len(block.Grants))
	for _, grant := range block.Grants {
		if anthropicResetGrantHeld(grant, now) {
			held = append(held, grant)
		}
	}
	for i, grant := range held {
		if grant.ID == block.NextGrantID {
			copy(held[1:i+1], held[:i])
			held[0] = grant
			break
		}
	}
	return held
}

func requestAnthropicResetCredits(ctx context.Context, client *http.Client, credential *anthropicauth.Credential, baseURL string) (*anthropicResetCredits, error) {
	block, err := requestAnthropicResetBlock(ctx, client, credential, baseURL)
	if err != nil {
		return nil, err
	}
	return projectAnthropicResetCredits(block, time.Now()), nil
}

func requestAnthropicResetBlock(ctx context.Context, client *http.Client, credential *anthropicauth.Credential, baseURL string) (*anthropicResetBlock, error) {
	if client == nil || credential == nil || strings.TrimSpace(credential.AccessToken) == "" {
		return nil, errors.New("anthropic reset request is unavailable")
	}
	if baseURL == "" {
		baseURL = anthropicauth.DefaultUpstreamURL
	}
	targetURL := buildUpstreamURL(baseURL, "/api/oauth/usage", "") + "?cedar_ember=1&skip_spend=1"
	req, err := newAnthropicOAuthMetadataRequest(ctx, targetURL, credential.AccessToken, true)
	if err != nil {
		return nil, errors.New("anthropic reset request is unavailable")
	}
	setAnthropicResetHeaders(req)
	body, err := executeOAuthUsageRequest(client, req, "Anthropic reset")
	if err != nil {
		return nil, err
	}
	return parseAnthropicResetBlock(body)
}

func setAnthropicResetHeaders(req *http.Request) {
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("x-app", "cli")
	req.Header.Set("User-Agent", "claude-cli/"+anthropicCLIVersion+" (external, cli)")
}

// HandleAnthropicResetCredits queries native reset credits without redeeming them.
func (s *Server) HandleAnthropicResetCredits(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, err := ParseInt64Param(c, "id")
	if err != nil || id <= 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel id")
		return
	}
	cfg, err := s.store.GetConfig(c.Request.Context(), id)
	if err != nil {
		RespondErrorMsg(c, http.StatusNotFound, "channel not found")
		return
	}
	if !cfg.UsesAnthropicOAuth() {
		RespondErrorMsg(c, http.StatusBadRequest, "Claude OAuth channel required")
		return
	}
	stored, err := anthropicauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Anthropic credential unavailable")
		return
	}
	if !anthropicResetHasProfileScope(stored.Scope) {
		RespondErrorMsg(c, http.StatusBadRequest, "user:profile scope required")
		return
	}
	if s.anthropicCredentials == nil {
		RespondErrorMsg(c, http.StatusServiceUnavailable, "Anthropic credential manager unavailable")
		return
	}
	requestCtx, cancel := context.WithTimeout(c.Request.Context(), oauthUsageTimeout)
	defer cancel()
	cfg = s.withOAuthBaseURLOverride(cfg)
	credential, err := s.anthropicCredentials.credential(requestCtx, cfg, false)
	if err != nil {
		RespondError(c, http.StatusServiceUnavailable, oauthUsageCredentialRefreshError(err, "Anthropic credential refresh failed"))
		return
	}
	if !anthropicResetHasProfileScope(credential.Scope) {
		RespondErrorMsg(c, http.StatusBadRequest, "user:profile scope required")
		return
	}
	client := s.getClientForChannel(cfg)
	baseURL := anthropicauth.DefaultUpstreamURL
	if urls := cfg.GetURLs(); len(urls) > 0 {
		baseURL = urls[0]
	}
	for attempt := range 2 {
		credits, queryErr := requestAnthropicResetCredits(requestCtx, client, credential, baseURL)
		if queryErr == nil {
			RespondJSON(c, http.StatusOK, credits)
			return
		}
		var statusErr *oauthUsageHTTPStatusError
		if attempt == 0 && errors.As(queryErr, &statusErr) && statusErr.statusCode == http.StatusUnauthorized {
			credential, err = s.anthropicCredentials.credentialAfterUnauthorized(requestCtx, cfg, credential.AccessToken)
			if err != nil {
				RespondError(c, http.StatusServiceUnavailable, oauthUsageCredentialRefreshError(err, "Anthropic credential refresh failed"))
				return
			}
			if !anthropicResetHasProfileScope(credential.Scope) {
				RespondErrorMsg(c, http.StatusBadRequest, "user:profile scope required")
				return
			}
			continue
		}
		RespondError(c, http.StatusBadGateway, queryErr)
		return
	}
}

func anthropicResetHasProfileScope(scope string) bool {
	return slices.Contains(strings.Fields(scope), "user:profile")
}
