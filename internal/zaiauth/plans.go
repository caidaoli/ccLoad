package zaiauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

/**
 * @brief 体验套餐的独立 Anthropic 入口，不能用付费 Key 鉴权。
 */
const StartPlanBaseURL = OAuthAPIBaseURL + "/zcode-plan/anthropic"

/**
 * @brief 体验余额接收序号；进程内严格递增，避免系统时钟精度不足或回拨导致采样乱序。
 */
var giftObservationSequence atomic.Uint64

/**
 * @brief 一个体验套餐的模型余额，单位保持上游原义。
 */
type GiftBalance struct {
	ID            string   `json:"id"`                       ///< 套餐实例与桶的稳定身份。
	PlanKey       string   `json:"plan_key"`                 ///< 套餐实例分组键，不按名称或模型合并。
	BucketID      string   `json:"bucket_id,omitempty"`      ///< 上游独立余额桶标识。
	EntitlementID string   `json:"entitlement_id,omitempty"` ///< 上游模型权益标识。
	Name          string   `json:"name"`                     ///< 上游套餐名称。
	DisplayName   string   `json:"display_name,omitempty"`   ///< 无余额桶时展示权益名称，不作为可调用模型。
	Models        []string `json:"models"`                   ///< 该桶支持的模型。
	UnitType      string   `json:"unit_type,omitempty"`      ///< 上游额度单位，不将不同单位合并。
	GrantUnits    *float64 `json:"grant_units,omitempty"`    ///< 权益计划发放量，不是当前总额或剩余量。
	Total         *float64 `json:"total,omitempty"`          ///< 总额度，nil 表示未知。
	Used          *float64 `json:"used,omitempty"`           ///< 已用额度，nil 表示未知。
	Remaining     *float64 `json:"remaining,omitempty"`      ///< 剩余额度，nil 表示未知。
	Granted       bool     `json:"granted"`                  ///< 是否已有实际余额桶；生效时间不代表已发放。
	StartsAt      int64    `json:"starts_at,omitempty"`      ///< 权益生效时间；无权益列表时为套餐开始时间，Unix 秒。
	ExpiresAt     int64    `json:"expires_at,omitempty"`     ///< 过期时间，Unix 秒。
	Status        string   `json:"status"`                   ///< active、pending、ungranted、inactive、expired 或 exhausted。
}

/**
 * @brief 同一账号的付费订阅与体验余额；查询失败与空套餐分别表达。
 */
type Plans struct {
	Paid            bool          `json:"paid"`                ///< 存在有效付费订阅。
	PaidKnown       bool          `json:"paid_known"`          ///< 是否成功查询付费订阅。
	PaidName        string        `json:"paid_name,omitempty"` ///< 付费套餐名称。
	Gifts           []GiftBalance `json:"gifts"`               ///< 各体验套餐的余额。
	GiftKnown       bool          `json:"gift_known"`          ///< 是否成功查询体验套餐。
	GiftObservation uint64        `json:"-"`                   ///< 体验余额接收序号，用于跨渠道排序，不受付费查询耗时影响。
	Warnings        []string      `json:"warnings,omitempty"`  ///< 查询失败信息，不包含凭证。
	ObservedAt      int64         `json:"observed_at"`         ///< 上游服务器时间或本地采样时间。
}

/**
 * @brief 判断登录 JWT 是否已到期；无法解析的令牌交给上游验证。
 * @param token ZCode 登录 JWT。
 * @param now 当前时间。
 * @return 已声明 exp 且到期时返回 true。
 */
func JWTExpired(token string, now time.Time) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	return json.Unmarshal(raw, &claims) == nil && claims.Exp > 0 && claims.Exp <= now.Unix()
}

/**
 * @brief 查询账号实际套餐；部分失败保留另一来源，调用者不得将未知解释为空。
 * @param ctx 查询上下文。
 * @param credential 账号认证材料。
 * @return 套餐快照，包含各来源查询状态。
 * @return 无有效输入或上下文取消时返回错误；上游错误记录在 Warnings。
 */
func (s *Service) FetchPlans(ctx context.Context, credential *Credential) (*Plans, error) {
	if credential == nil || s == nil || s.Client == nil {
		return nil, errors.New("z.ai plan service is unavailable")
	}
	p := &Plans{Gifts: []GiftBalance{}, ObservedAt: s.Now().Unix()}
	paid := &Plans{}
	var giftRaw json.RawMessage
	var giftObserved, giftReceived time.Time
	var giftErr error
	var queries sync.WaitGroup
	queries.Go(func() {
		if credential.APIKey != "" {
			// 国内账号业务在 bigmodel.cn，Key 的订阅查询在 open.bigmodel.cn。
			base := s.SubscriptionBaseURL
			if base == "" {
				base = s.BizBaseURL
			}
			raw, _, err := s.planRequest(ctx, strings.TrimRight(base, "/")+"/api/biz/subscription/list", credential.APIKey, "")
			var subscriptions []struct {
				Status      string `json:"status"`
				ProductName string `json:"productName"`
			}
			if err == nil {
				err = json.Unmarshal(raw, &subscriptions)
			}
			if err != nil {
				paid.Warnings = append(paid.Warnings, "付费订阅查询失败")
			} else {
				paid.PaidKnown = true
				for _, sub := range subscriptions {
					if strings.EqualFold(sub.Status, "VALID") {
						paid.Paid = true
						paid.PaidName = sub.ProductName
						break
					}
				}
			}
		} else {
			paid.Warnings = append(paid.Warnings, "缺少 API Key，付费套餐暂无法查询")
		}
	})
	// 两个来源并发且各自限时；某一来源超时不能吞掉另一来源的有效结果。
	queries.Go(func() {
		if credential.JWTToken != "" {
			if JWTExpired(credential.JWTToken, s.Now()) {
				p.Warnings = append(p.Warnings, "ZCode 登录已过期，请重新登录")
			} else {
				giftRaw, giftObserved, giftErr = s.planRequest(ctx, strings.TrimRight(s.OAuthBaseURL, "/")+"/zcode-plan/billing/balance?app_version="+AppVersion, "Bearer "+credential.JWTToken, credential.DeviceID)
				p.GiftObservation = giftObservationSequence.Add(1)
				giftReceived = time.Now()
				p.ObservedAt = s.Now().Unix()
			}
		}
	})
	queries.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !giftReceived.IsZero() {
		if giftErr == nil {
			// 以体验服务器时钟为基准推进等待时间，再判断生效/到期；不伪造余额桶。
			giftErr = p.readGifts(giftRaw, giftObserved, time.Since(giftReceived))
		}
		if giftErr != nil {
			p.Warnings = append(p.Warnings, "体验套餐查询失败")
		} else {
			p.GiftKnown = true
		}
	}
	p.Paid, p.PaidKnown, p.PaidName = paid.Paid, paid.PaidKnown, paid.PaidName
	p.Warnings = append(paid.Warnings, p.Warnings...)
	return p, nil
}

/**
 * @brief 判断付费入口是否可尝试；查询未知时允许有效 Key 继续调用。
 * @param credential 账号凭证。
 * @return 有 Key 且未确认无付费订阅时返回 true。
 */
func (p *Plans) CanUsePaid(credential *Credential) bool {
	return credential != nil && credential.APIKey != "" && (p == nil || !p.PaidKnown || p.Paid)
}

/**
 * @brief 按 ZCode 控制面认证契约读取信封，HTTP 与业务状态都必须成功。
 * @param ctx 请求上下文。
 * @param target 完整接口地址。
 * @param authorization 已带所需前缀的认证值。
 * @param device 体验余额查询的设备 ID，付费查询为空。
 * @return 信封中的 data。
 * @return HTTP Date 指示的服务器时间；缺失或非法时为零值。
 * @return 网络、HTTP、业务状态或响应格式非法时返回错误。
 */
func (s *Service) planRequest(ctx context.Context, target, authorization, device string) (json.RawMessage, time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	ApplySourceHeaders(req.Header)
	req.Header.Set("Authorization", authorization)
	req.Header.Set("Accept", "application/json")
	if device != "" {
		req.Header.Set("X-Device-Mid", device)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, time.Time{}, errors.New("z.ai plan request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil || len(raw) > maxResponseSize {
		return nil, time.Time{}, errors.New("z.ai plan response is unreadable")
	}
	var envelope struct {
		Code    json.RawMessage `json:"code"`
		Success *bool           `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil || !envelopeCodeSucceeded(envelope.Code) || (envelope.Success != nil && !*envelope.Success) || resp.StatusCode < 200 || resp.StatusCode >= 300 || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil, time.Time{}, fmt.Errorf("z.ai plan request rejected (HTTP %d)", resp.StatusCode)
	}
	observed, _ := http.ParseTime(resp.Header.Get("Date"))
	return envelope.Data, observed, nil
}

/**
 * @brief 关联套餐、权益和余额桶；未发放权益保留实际生效时间，额度保持未知。
 * @param raw balance 接口的 data。
 * @param observed HTTP Date，零值时回退到响应 server_time 和本地时间。
 * @param elapsed 体验响应返回后等待另一查询的时长。
 * @return JSON 非法或套餐数组缺失时返回错误。
 */
func (p *Plans) readGifts(raw []byte, observed time.Time, elapsed time.Duration) error {
	type plan struct {
		ID           string      `json:"plan_id"`
		UserID       string      `json:"user_plan_id"`
		Name         string      `json:"name"`
		Status       string      `json:"status"`
		StartsAt     json.Number `json:"starts_at"`
		EndsAt       json.Number `json:"ends_at"`
		Entitlements []struct {
			ID           string       `json:"entitlement_id"`
			ShowName     string       `json:"show_name"`
			Capabilities []string     `json:"capabilities"`
			EffectiveAt  json.Number  `json:"effective_at"`
			UnitType     string       `json:"unit_type"`
			GrantUnits   *json.Number `json:"grant_units"`
		} `json:"entitlements"`
	}
	var body struct {
		ServerTime json.Number `json:"server_time"`
		Plans      []plan      `json:"plans"`
		Balances   []struct {
			ID            string       `json:"plan_id"`
			UserID        string       `json:"user_plan_id"`
			BucketID      string       `json:"bucket_id"`
			EntitlementID string       `json:"entitlement_id"`
			ShowName      string       `json:"show_name"`
			UnitType      string       `json:"unit_type"`
			Capabilities  []string     `json:"capabilities"`
			Total         *json.Number `json:"total_units"`
			Used          *json.Number `json:"used_units"`
			Remaining     *json.Number `json:"remaining_units"`
			ExpiresAt     json.Number  `json:"expires_at"`
			PeriodStart   json.Number  `json:"period_start"`
			PeriodEnd     json.Number  `json:"period_end"`
		} `json:"balances"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	if body.Plans == nil {
		return errors.New("z.ai gift response has no plans")
	}
	if serverTime, _ := body.ServerTime.Int64(); serverTime > 0 {
		p.ObservedAt = serverTime
	}
	if !observed.IsZero() {
		p.ObservedAt = observed.Unix()
	}
	p.ObservedAt += int64(elapsed / time.Second)
	// 每个桶只能属于一个套餐实例；缺实例 ID 的歧义桶不能重复分配额度。
	owners := make([]int, len(body.Balances))
	ambiguous := false
	for i, bucket := range body.Balances {
		owners[i] = -1
		candidates := 0
		// 明确实例匹配优先于缺实例 ID 的商品回退。
		for j, item := range body.Plans {
			if bucket.UserID != "" && item.UserID == bucket.UserID {
				owners[i] = j
				candidates++
			}
		}
		if candidates == 0 {
			for j, item := range body.Plans {
				if item.ID != "" && item.ID == bucket.ID && (bucket.UserID == "" || item.UserID == "") {
					owners[i] = j
					candidates++
				}
			}
		}
		if candidates > 1 {
			owners[i] = -1
			ambiguous = true
		}
	}
	if ambiguous {
		p.Warnings = append(p.Warnings, "部分体验额度无法确认所属套餐")
	}
	for planIndex, item := range body.Plans {
		start, _ := item.StartsAt.Int64()
		end, _ := item.EndsAt.Int64()
		base := GiftBalance{ID: item.ID + "/" + item.UserID, Name: item.Name, Models: []string{}, StartsAt: start, ExpiresAt: end, Status: "active"}
		base.PlanKey = base.ID
		if item.UserID == "" {
			// 缺少实例身份时按响应条目隔离，两个同商品记录也不能合并。
			base.PlanKey = fmt.Sprintf("%s/plan/%d", item.ID, planIndex)
			base.ID = base.PlanKey
		}
		base.setStatus(item.Status, p.ObservedAt)
		entitlements := make([]GiftBalance, len(item.Entitlements))
		granted := make([]bool, len(item.Entitlements))
		for i, entitlement := range item.Entitlements {
			gift := base
			gift.ID = fmt.Sprintf("%s/entitlement/%s/%d", base.ID, entitlement.ID, i)
			gift.EntitlementID = entitlement.ID
			gift.UnitType = entitlement.UnitType
			gift.GrantUnits = planNumber(entitlement.GrantUnits)
			// 套餐已开始不代表模型权益已生效；没有余额桶也必须读取 effective_at。
			effective, _ := entitlement.EffectiveAt.Int64()
			gift.StartsAt = effective
			gift.DisplayName = strings.TrimSpace(entitlement.ShowName)
			if gift.DisplayName == "" {
				gift.DisplayName = strings.Join(planModels(entitlement.Capabilities, ""), ", ")
			}
			gift.setStatus(item.Status, p.ObservedAt)
			entitlements[i] = gift
		}
		matched := false
		for i, bucket := range body.Balances {
			if owners[i] != planIndex {
				continue
			}
			matched = true
			exp, _ := bucket.ExpiresAt.Int64()
			gift := base
			for j, entitlement := range item.Entitlements {
				if entitlement.ID != "" && entitlement.ID == bucket.EntitlementID {
					gift = entitlements[j]
					granted[j] = true
					break
				}
			}
			gift.ID = fmt.Sprintf("%s/bucket/%d", base.ID, i)
			gift.BucketID = bucket.BucketID
			gift.Granted = true
			gift.EntitlementID = bucket.EntitlementID
			if bucket.UnitType != "" {
				gift.UnitType = bucket.UnitType
			}
			gift.Total, gift.Used, gift.Remaining = planNumber(bucket.Total), planNumber(bucket.Used), planNumber(bucket.Remaining)
			if bucket.BucketID != "" {
				gift.ID = base.ID + "/bucket/" + bucket.BucketID
			}
			if exp > 0 && (gift.ExpiresAt == 0 || exp < gift.ExpiresAt) {
				gift.ExpiresAt = exp
			}
			// 周期桶只能在自身窗口内使用，不能仅看套餐和权益的开始/结束时间。
			if periodStart, _ := bucket.PeriodStart.Int64(); periodStart > gift.StartsAt {
				gift.StartsAt = periodStart
			}
			if periodEnd, _ := bucket.PeriodEnd.Int64(); periodEnd > 0 && (gift.ExpiresAt == 0 || periodEnd < gift.ExpiresAt) {
				gift.ExpiresAt = periodEnd
			}
			if models := planModels(bucket.Capabilities, bucket.ShowName); len(models) > 0 {
				gift.Models = models
			}
			gift.setStatus(item.Status, p.ObservedAt)
			p.Gifts = append(p.Gifts, gift)
		}
		for i, gift := range entitlements {
			if granted[i] {
				continue
			}
			// 到了生效时间仍未发放余额时，不凭 grant_units 虚构可调用额度。
			if gift.Status == "active" {
				gift.Status = "ungranted"
			}
			p.Gifts = append(p.Gifts, gift)
		}
		// 套餐存在不代表上游已分配余额桶，不能因此丢掉未来套餐。
		if !matched && len(entitlements) == 0 {
			if base.Status == "active" {
				base.Status = "ungranted"
			}
			p.Gifts = append(p.Gifts, base)
		}
	}
	return nil
}

/**
 * @brief 按套餐失效、权益生效与余额计算状态，未来权益不得用于推理。
 * @param planStatus 上游套餐状态。
 * @param observedAt 本次响应的服务器时间，Unix 秒。
 * @note 修改当前余额行的 Status。
 */
func (g *GiftBalance) setStatus(planStatus string, observedAt int64) {
	planStatus = strings.TrimSpace(planStatus)
	switch {
	case strings.EqualFold(planStatus, "expired"):
		g.Status = "expired"
	case !strings.EqualFold(planStatus, "active"):
		// 上游停用是独立业务状态，不能被本地时间推导覆盖。
		g.Status = "inactive"
	case g.ExpiresAt > 0 && g.ExpiresAt <= observedAt:
		g.Status = "expired"
	case g.StartsAt > observedAt:
		g.Status = "pending"
	case g.Remaining != nil && *g.Remaining <= 0:
		g.Status = "exhausted"
	case g.Remaining == nil && g.Total != nil && g.Used != nil && *g.Used >= *g.Total:
		g.Status = "exhausted"
	default:
		g.Status = "active"
	}
}

/**
 * @brief 从权益或余额桶提取模型，忽略非模型能力。
 * @param capabilities 上游能力列表。
 * @param showName 能力列表未提供模型时使用的上游名称。
 * @return 模型列表；未提供时返回空列表。
 */
func planModels(capabilities []string, showName string) []string {
	models := []string{}
	for _, capability := range capabilities {
		capability = strings.TrimSpace(capability)
		if strings.HasPrefix(strings.ToLower(capability), "model:") {
			capability = capability[6:]
		} else if strings.Contains(capability, ":") {
			continue
		}
		if capability = strings.TrimSpace(capability); capability != "" {
			models = append(models, capability)
		}
	}
	if len(models) == 0 && strings.TrimSpace(showName) != "" {
		models = append(models, strings.TrimSpace(showName))
	}
	return models
}

/**
 * @brief 读取上游 number|string|null 额度，非法数值保持未知。
 * @param value 上游数值。
 * @return 有效数值的独立指针，缺失或非法时为 nil。
 */
func planNumber(value *json.Number) *float64 {
	if value == nil {
		return nil
	}
	n, err := value.Float64()
	if err != nil {
		return nil
	}
	return &n
}

/**
 * @brief 查找模型的体验额度，任一有效桶可用即可使用。
 * @param model 请求模型，允许 -Trial 后缀。
 * @param now 当前时间。
 * @return 上游模型原名；不存在时为空。
 * @return 是否存在尚未到期且未耗尽的额度。
 */
func (p *Plans) GiftModel(model string, now time.Time) (string, bool) {
	name := model
	if strings.HasSuffix(strings.ToLower(name), "-trial") {
		name = name[:len(name)-6]
	}
	found := ""
	for _, gift := range p.Gifts {
		for _, raw := range gift.Models {
			if !strings.EqualFold(raw, name) {
				continue
			}
			if gift.Status != "active" && gift.Status != "exhausted" {
				continue
			}
			found = raw
			if gift.Status == "active" && (gift.ExpiresAt == 0 || gift.ExpiresAt > now.Unix()) {
				return raw, true
			}
		}
	}
	return found, false
}
