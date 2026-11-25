package qoderauth

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const authCampaignPath = "/sash/api/v1/me/campaigns"

/**
 * @brief 上游确认的 Qoder 签到状态与奖励。
 */
type CheckinResult struct {
	Status     string    `json:"status"`                ///< 领取结果或活动不可用原因。
	CampaignID string    `json:"campaign_id,omitempty"` ///< 已确认的上游活动标识。
	Message    string    `json:"message,omitempty"`     ///< 活动结果说明。
	Unit       string    `json:"unit,omitempty"`        ///< 确认领取的奖励单位。
	Reward     float64   `json:"reward,omitempty"`      ///< 确认领取的奖励数量。
	SelectedAt time.Time `json:"-"`                     ///< 选中已完成活动的时刻，用于归属原签到窗口。
}

/**
 * @brief 将已确认的签到归入选择活动时的窗口，并保留较新的记录。
 * @param c 写入前已重新读取并核对账号身份的渠道凭据。
 * @note 只用于已确认的签到结果；就地更新凭据的签到时间及活动标识。
 */
func (r *CheckinResult) UpdateCredential(c *Credential) {
	selectedAt := r.SelectedAt.Unix()
	if selectedAt > c.LastCheckinAt {
		c.LastCheckinAt = selectedAt
		c.LastCheckinCampaign = r.CampaignID
	}
}

/**
 * @brief 签到判定所用的 Sash 活动字段。
 */
type authCampaign struct {
	CampaignID  string                     `json:"campaignId"`  ///< 领取端点使用的活动标识。
	ActionType  string                     `json:"actionType"`  ///< CLAIM_BENEFIT 表示活动可领取。
	ClaimStatus string                     `json:"claimStatus"` ///< 账号对应的领取状态。
	Title       string                     `json:"title"`       ///< 无本地化展示时的活动标题。
	StartAt     json.RawMessage            `json:"startAt"`     ///< 活动开始时间。
	EndAt       json.RawMessage            `json:"endAt"`       ///< 活动截止时间。
	ClaimedAt   json.RawMessage            `json:"claimedAt"`   ///< 当前账号的领取时间。
	Benefit     map[string]json.RawMessage `json:"benefit"`     ///< 奖励类别、有效期和适用模型。
	Placements  []struct {
		Type    string `json:"type"` ///< 上游客户端展示位置。
		Content map[string]struct {
			Title string `json:"title"` ///< 该语言的活动标题。
		} `json:"content"` ///< 按语言提供的展示内容。
	} `json:"placements"` ///< 活动展示信息。
}

/**
 * @brief 返回包含指定时刻的 Qoder 每日签到窗口起点。
 * @param now 待归入签到窗口的时刻。
 * @return UTC+8 最近一次 10:00，每日活动与调度使用相同的起点。
 */
func CheckinWindow(now time.Time) time.Time {
	local := now.In(time.FixedZone("Qoder", 8*3600))
	start := time.Date(local.Year(), local.Month(), local.Day(), 10, 0, 0, 0, local.Location())
	if local.Before(start) {
		start = start.AddDate(0, 0, -1)
	}
	return start
}

/**
 * @brief 解码活动的绝对时间，兼容 Unix 秒、毫秒与 RFC3339。
 * @param raw 上游 JSON 时间字段。
 * @return 活动时间，字段缺失或无效时为零值。
 * @return 字段缺失或格式有效时为 true。
 */
func campaignTime(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, true
	}
	if value, ok := authNumber(raw); ok && value > 0 && value < math.MaxInt64 {
		if value < 100_000_000_000 {
			value *= 1000
		}
		return time.UnixMilli(int64(value)), true
	}
	var value string
	_ = json.Unmarshal(raw, &value)
	at, err := time.Parse(time.RFC3339Nano, value)
	return at, err == nil
}

/**
 * @brief 识别每日通用 Credits，排除模型专属福利。
 * @param start 活动开始时间，未知时为零值。
 * @param end 活动截止时间，未知时为零值。
 * @return 标题或旧版签到奖励元数据能确认活动时为 true。
 */
func (c *authCampaign) isDaily(start, end time.Time) bool {
	if c.ActionType != "CLAIM_BENEFIT" || authText(c.Benefit, "kind") != "CREDITS" {
		return false
	}
	if raw := c.Benefit["modelScope"]; len(raw) != 0 {
		var scope, series map[string]json.RawMessage
		if json.Unmarshal(raw, &scope) != nil {
			return false
		}
		_ = json.Unmarshal(scope["modelSeries"], &series)
		if len(scope) != 0 && authText(series, "key") != "ALL_MODELS" {
			return false
		}
	}
	titles := []string{c.Title}
	for _, placement := range c.Placements {
		if placement.Type == "POPUP" || placement.Type == "USAGE" {
			titles = append(titles, placement.Content["zh"].Title, placement.Content["en"].Title)
		}
	}
	title := strings.ToLower(strings.TrimSpace(strings.Join(titles, " ")))
	if strings.Contains(title, "sonus") || strings.Contains(title, "sota") {
		return false
	}
	if title != "" {
		return strings.Contains(title, "每日") || strings.Contains(title, "每天") ||
			strings.Contains(title, "daily") || strings.Contains(title, "every day")
	}
	// 旧版签到没有标题，奖励为可在单日窗口内领取的 100 通用 Credits，有效期 30 天。
	if start.IsZero() || !end.After(start) || end.Sub(start) > 24*time.Hour {
		return false
	}
	var validity map[string]json.RawMessage
	_ = json.Unmarshal(c.Benefit["validity"], &validity)
	amount, _ := authNumber(c.Benefit["amount"])
	days, _ := authNumber(validity["days"])
	return amount == 100 && days == 30
}

/**
 * @brief 根据上游提供的时间判断活动是否属于当前签到窗口。
 * @param now 查询活动的当前时刻。
 * @param window 当前签到窗口的起点。
 * @return 活动开始时间，未提供时为零值。
 * @return 活动截止时间，未提供时为零值。
 * @return 已领取活动的领取时间，未提供时为零值。
 * @return 已提供的时间均有效且落在对应窗口时为 true。
 */
func (c *authCampaign) windowTimes(now, window time.Time) (start, end, claimed time.Time, valid bool) {
	start, startOK := campaignTime(c.StartAt)
	end, endOK := campaignTime(c.EndAt)
	if !startOK || !endOK || (!start.IsZero() && (start.Before(window) || start.After(now))) ||
		(!end.IsZero() && !end.After(now)) {
		return start, end, claimed, false
	}
	if c.ClaimStatus == "CLAIMED" {
		claimedAt, ok := campaignTime(c.ClaimedAt)
		return start, end, claimedAt, ok && (claimedAt.IsZero() || (!claimedAt.Before(window) && !claimedAt.After(now)))
	}
	return start, end, claimed, true
}

/**
 * @brief 提取 Sash 活动查询或领取响应的业务字段。
 * @param response HTTP 响应的 JSON 对象。
 * @return data 包装内的对象，或未包装的原对象。
 */
func campaignPayload(response map[string]json.RawMessage) map[string]json.RawMessage {
	var data map[string]json.RawMessage
	if json.Unmarshal(response["data"], &data) == nil && data != nil {
		return data
	}
	return response
}

/**
 * @brief 实时读取账号活动清单及风控组件所需的 Sash UID。
 * @param ctx 请求上下文。
 * @param c 地区及端点所属的凭据。
 * @param headers 请求凭据和已有的风控请求头。
 * @return Sash UID，清单未提供时为空。
 * @return 上游活动清单。
 * @return 请求失败或清单格式无效时返回错误，非 2xx 可匹配 *HTTPError。
 */
func (s *Service) authCampaigns(ctx context.Context, c *Credential, headers http.Header) (string, []authCampaign, error) {
	data, _, err := s.authDo(ctx, http.MethodGet, s.authEndpoint(c, authCampaignPath+"?forceRefresh=true"), nil, headers)
	if err != nil {
		return "", nil, err
	}
	payload := campaignPayload(data)
	raw := payload["campaigns"]
	if raw == nil {
		raw = data["data"]
	}
	var campaigns []authCampaign
	if json.Unmarshal(raw, &campaigns) != nil || campaigns == nil {
		return "", nil, errors.New("invalid Qoder campaigns response")
	}
	uid := authText(payload, "uid")
	if uid == "" {
		uid = authText(data, "uid")
	}
	return uid, campaigns, nil
}

/**
 * @brief 领取本签到窗口的 Qoder 每日通用 Credits。
 * @param ctx 请求上下文。
 * @param credential Qoder 账号及地区凭据。
 * @return 上游确认的领取结果，组件缺失或无每日活动时返回相应状态。
 * @return 凭据无效或上游请求、响应异常时返回错误，非 2xx 可匹配 *HTTPError。
 * @note 不确定领取结果时回查同一活动；国际版使用 Sash UID 获取风控请求头。
 */
func (s *Service) Checkin(ctx context.Context, credential *Credential) (*CheckinResult, error) {
	c, err := authCopy(credential)
	if err != nil {
		return nil, err
	}
	if c.AccessToken == "" {
		return nil, errors.New("qoder access_token is required")
	}
	headers := make(http.Header)
	headers.Set("Authorization", "Bearer "+c.AccessToken)
	headers.Set("Cosy-ClientType", "10")
	headers.Set("Accept", "application/json")
	headers.Set("Accept-Language", "zh-CN")
	headers.Set("User-Agent", "Qoder")
	missing := &CheckinResult{Status: "missing_component", Message: "Qoder international check-in requires the official machine risk component"}
	if c.Region == "global" && s.RiskHeaders == nil {
		return missing, nil
	}
	uid, campaigns, err := s.authCampaigns(ctx, c, headers)
	if err != nil {
		return nil, err
	}
	if c.Region == "global" {
		if uid == "" {
			return nil, errors.New("qoder campaigns response is missing uid")
		}
		riskCredential := *c
		riskCredential.UserID = uid
		risk, riskErr := s.RiskHeaders(ctx, &riskCredential)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if riskErr != nil {
			return missing, nil
		}
		for _, key := range []string{"Cosy-MachineToken", "Cosy-MachineCode", "Cosy-MachineType"} {
			value := strings.TrimSpace(risk.Get(key))
			if value == "" {
				return missing, nil
			}
			headers.Set(key, value)
		}
		_, campaigns, err = s.authCampaigns(ctx, c, headers)
		if err != nil {
			return nil, err
		}
	}
	var target *authCampaign
	var claimed *authCampaign
	now := time.Now()
	window := CheckinWindow(now)
	for i := range campaigns {
		campaign := &campaigns[i]
		start, end, claimedAt, valid := campaign.windowTimes(now, window)
		if !valid || (c.Region == "global" && start.IsZero()) || !campaign.isDaily(start, end) {
			continue
		}
		switch campaign.ClaimStatus {
		case "CLAIMABLE":
			if target == nil {
				target = campaign
			}
		case "CLAIMED":
			if claimed == nil && (!start.IsZero() || !claimedAt.IsZero()) && strings.TrimSpace(campaign.CampaignID) != "" {
				claimed = campaign
			}
		}
	}
	if target == nil {
		if claimed != nil {
			return &CheckinResult{Status: "already_claimed", CampaignID: claimed.CampaignID, SelectedAt: now}, nil
		}
		return &CheckinResult{Status: "unavailable", Message: "No claimable Qoder daily credits campaign"}, nil
	}
	if strings.TrimSpace(target.CampaignID) == "" {
		return nil, errors.New("qoder claimable campaign is missing campaignId")
	}
	result := &CheckinResult{CampaignID: target.CampaignID, SelectedAt: now}
	claim, claimStatus, claimErr := s.authDo(ctx, http.MethodPost, s.authEndpoint(c, authCampaignPath+"/"+url.PathEscape(target.CampaignID)+"/claim"), struct{}{}, headers)
	claim = campaignPayload(claim)
	status := authText(claim, "status")
	if claimErr == nil && status == "CLAIMED" {
		var replayed bool
		_ = json.Unmarshal(claim["replayed"], &replayed)
		result.Status = "claimed"
		if replayed {
			result.Status = "already_claimed"
			return result, nil
		}
		var benefit map[string]json.RawMessage
		if json.Unmarshal(claim["benefit"], &benefit) == nil {
			if reward, ok := authNumber(benefit["amount"]); ok && reward >= 0 {
				result.Reward = reward
			}
			result.Unit = authText(benefit, "kind")
			if result.Unit == "CREDITS" {
				result.Unit = "credits"
			}
		}
		return result, nil
	}
	if (claimErr == nil || claimStatus == http.StatusConflict) && authText(claim, "result") == "ALREADY_CLAIMED" {
		result.Status = "already_claimed"
		return result, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	_, confirmed, verifyErr := s.authCampaigns(ctx, c, headers)
	verifiedAt := time.Now()
	if verifyErr == nil && CheckinWindow(verifiedAt).Equal(window) {
		for i := range confirmed {
			campaign := &confirmed[i]
			if campaign.CampaignID != target.CampaignID || campaign.ClaimStatus != "CLAIMED" {
				continue
			}
			if _, _, _, valid := campaign.windowTimes(verifiedAt, window); valid {
				result.Status = "already_claimed"
				result.Message = "Campaign claim confirmed after an uncertain response"
				return result, nil
			}
		}
	}
	if claimErr != nil {
		return nil, claimErr
	}
	return nil, errors.New("qoder did not confirm the campaign claim")
}
