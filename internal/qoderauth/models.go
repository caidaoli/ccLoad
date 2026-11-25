package qoderauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Model keeps the catalog's public name separate from its native routing key.
type Model struct {
	ID                string          `json:"id"`
	Key               string          `json:"key"`
	Config            json.RawMessage `json:"config"`
	SupportsReasoning bool            `json:"supports_reasoning"`
	SupportsImages    bool            `json:"supports_images"`
	Efforts           []string        `json:"efforts,omitempty"`
}

// Catalog describes enabled and disabled models from an account listing.
/**
 * @brief 上游目录的可用模型与显式禁用模型，用于区分额度耗尽和过期登录视图
 */
type Catalog struct {
	Models   []Model ///< 当前可用模型
	Disabled []Model ///< 上游显式关闭的模型
	FreeOnly bool    ///< 所有收费模型关闭且有免费模型可用
}

// APIError carries a normalized upstream error and explicit retry parameters.
/**
 * @brief Qoder HTTP 或流内业务错误，携带已确认的重试参数
 */
type APIError struct {
	StatusCode int    `json:"status"`
	Type       string `json:"type"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	RetryAfter int    `json:"retry_after,omitempty"` ///< 上游建议的等待秒数
	MaxTokens  int    `json:"max_tokens,omitempty"`  ///< 上游明确拒绝的输出上限
}

func (e *APIError) Error() string { return e.Message }

/**
 * @brief 提取上游明确给出的输出 token 上限
 */
var outputRange = regexp.MustCompile(`(?i)range of max_tokens should be \[\s*1\s*,\s*([0-9]+)\s*\]`)

/**
 * @brief 识别明确的登录过期提示
 */
var loginExpiredText = regexp.MustCompile(`(?i)\b(?:login(?: or access token)?|access token) expired\b`)

/**
 * @brief 识别明确的配额或余额不足提示
 */
var quotaExhaustedText = regexp.MustCompile(`(?i)\b(insufficient (quota|credits?|balance)|not enough credits?|quota (exceeded|exhausted)|credit balance exhausted|credits? (are )?(exhausted|used up)|insufficient_quota|no_quota|quota_exceed(ed)?|exceed_quota|credit_balance_exhausted)\b`)

/**
 * @brief 解码 Qoder 的整数错误号，兼容 JSON 数字和数字字符串
 * @param value 上游错误号字段
 * @return 整数错误号，缺失或无法解码时为 0
 */
func qoderErrorNumber(value any) int {
	switch number := value.(type) {
	case float64:
		if n := int(number); float64(n) == number {
			return n
		}
	case string:
		if n, err := strconv.Atoi(number); err == nil {
			return n
		}
	}
	return 0
}

/**
 * @brief 读取排队等待秒数，正小数向上取整
 * @param value 上游排队时间的 JSON 数字或数字字符串
 * @return 正的整秒数，无有效时间时为 0
 */
func qoderQueueWaitSeconds(value any) int {
	var seconds float64
	switch number := value.(type) {
	case float64:
		seconds = number
	case string:
		seconds, _ = strconv.ParseFloat(strings.TrimSpace(number), 64)
	}
	seconds = math.Ceil(seconds)
	if seconds > 0 && seconds < math.Ldexp(1, strconv.IntSize-1) {
		return int(seconds)
	}
	return 0
}

/**
 * @brief 从外向内读取错误信封的显式 HTTP 状态
 * @param value 当前错误信封，允许 JSON 字符串包裹
 * @param depth 当前信封嵌套层级
 * @return HTTP 4xx/5xx 状态，无显式状态时为 0
 */
func qoderEmbeddedErrorStatus(value any, depth int) int {
	if depth > 6 {
		return 0
	}
	if text, ok := value.(string); ok {
		if json.Unmarshal([]byte(text), &value) != nil {
			return 0
		}
	}
	envelope, ok := value.(map[string]any)
	if !ok {
		return 0
	}
	for _, key := range [...]string{"statusCodeValue", "statusCode", "status"} {
		if n := qoderErrorNumber(envelope[key]); n >= 400 && n <= 599 {
			return n
		}
	}
	for _, key := range [...]string{"body", "error"} {
		if n := qoderEmbeddedErrorStatus(envelope[key], depth+1); n != 0 {
			return n
		}
	}
	switch n := qoderErrorNumber(envelope["code"]); n {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests:
		return n
	}
	return 0
}

// ClassifyError maps Qoder business failures before considering HTTP status.
/**
 * @brief 优先按业务码区分排队、额度、请求错误和认证失败
 * @param status HTTP 状态或信封状态
 * @param body 上游响应 JSON
 * @return 规范化的 OpenAI 错误及明确的重试参数
 * @details 等待提示优先用 retryAfterSeconds，其次 waitTime。同字段按父级在前、同级键排序读取。
 */
func ClassifyError(status int, body []byte) *APIError {
	var root any
	jsonErr := json.Unmarshal(body, &root)
	codes := map[int]bool{}
	queued := false
	retryAfter, waitTime := 0, 0
	message := ""
	quota, loginExpired := false, false
	scanText := func(text string) {
		loginExpired = loginExpired || loginExpiredText.MatchString(text)
		quota = quota || quotaExhaustedText.MatchString(text)
	}
	if jsonErr != nil {
		scanText(string(body))
	}
	var visit func(any, int)
	visit = func(value any, depth int) {
		if depth > 6 {
			return
		}
		switch value := value.(type) {
		case string:
			var nested any
			if json.Unmarshal([]byte(value), &nested) == nil {
				visit(nested, depth+1)
			} else if depth == 0 {
				scanText(value)
			}
		case []any:
			for _, child := range value {
				visit(child, depth+1)
			}
		case map[string]any:
			unavailable, hasQueueType := false, false
			keys := slices.Sorted(maps.Keys(value))
			for _, field := range keys {
				key, child := strings.ToLower(field), value[field]
				switch key {
				case "code", "errorcode", "statuscodevalue", "statuscode", "status":
					codes[qoderErrorNumber(child)] = true
					if text, ok := child.(string); ok {
						scanText(text)
					}
				case "pricingurl":
					if url, ok := child.(string); ok && url != "" {
						quota = true
					}
				case "retryafterseconds":
					if retryAfter == 0 {
						retryAfter = qoderQueueWaitSeconds(child)
					}
				case "waittime":
					if waitTime == 0 {
						waitTime = qoderQueueWaitSeconds(child)
					}
				case "serviceavailable":
					unavailable = unavailable || child == false
				case "queuetype":
					if name, ok := child.(string); ok && strings.TrimSpace(name) != "" {
						hasQueueType = true
					}
				case "isqueued":
					queued = queued || child == true
				case "body":
					if text, ok := child.(string); ok && !json.Valid([]byte(text)) {
						scanText(text)
					}
				case "message", "msg", "error", "type":
					if text, ok := child.(string); ok && text != "" {
						scanText(text)
						if key == "message" || key == "msg" {
							message = text
						}
					}
				}
			}
			queued = queued || unavailable && hasQueueType
			for _, key := range keys {
				visit(value[key], depth+1)
			}
		}
	}
	visit(root, 0)
	if status < 400 || status > 599 {
		if embedded := qoderEmbeddedErrorStatus(root, 0); embedded != 0 {
			status = embedded
		}
	}
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if len(message) > 1000 {
		message = string([]rune(message)[:min(len([]rune(message)), 1000)])
	}
	if message == "" {
		message = http.StatusText(status)
	}
	result := &APIError{StatusCode: status, Type: "upstream_error", Code: "upstream_error", Message: message}
	lower := strings.ToLower(string(body))
	maxTokens := 0
	if match := outputRange.FindStringSubmatch(lower); len(match) == 2 {
		maxTokens, _ = strconv.Atoi(match[1])
	}
	switch {
	case codes[10605] || queued:
		result.StatusCode, result.Type, result.Code = http.StatusServiceUnavailable, "server_error", "service_unavailable"
		result.RetryAfter = retryAfter
		if result.RetryAfter == 0 {
			result.RetryAfter = waitTime
		}
		if result.RetryAfter == 0 {
			result.RetryAfter = 30
		}
	case codes[110] || codes[112] || codes[113] || codes[114] || codes[115] || codes[116] || codes[117] || codes[118] || codes[119] || codes[122]:
		result.StatusCode, result.Type, result.Code = http.StatusTooManyRequests, "insufficient_quota", "insufficient_quota"
	case codes[406] || codes[416] || codes[430] || codes[80411] || codes[80412] || codes[100400] || codes[100401] || codes[100403]:
		result.StatusCode, result.Type, result.Code = http.StatusBadRequest, "invalid_request_error", "invalid_request"
	case maxTokens > 0:
		result.StatusCode, result.Type, result.Code = http.StatusBadRequest, "invalid_request_error", "max_tokens_exceeded"
		result.MaxTokens = maxTokens
	case strings.Contains(lower, "error in upstream response"):
		result.StatusCode, result.Type, result.Code = http.StatusBadGateway, "server_error", "upstream_error"
	case codes[105] || status == http.StatusUnauthorized || loginExpired:
		result.StatusCode, result.Type, result.Code = http.StatusUnauthorized, "authentication_error", "authentication_error"
	case quota:
		result.StatusCode, result.Type, result.Code = http.StatusTooManyRequests, "insufficient_quota", "insufficient_quota"
	case status == http.StatusTooManyRequests || strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests"):
		result.StatusCode, result.Type, result.Code = http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded"
	case status == http.StatusForbidden:
		result.Type, result.Code = "permission_error", "permission_denied"
	case status < 400 || status > 599:
		result.StatusCode = http.StatusBadGateway
	}
	return result
}

// FetchCatalog reads the account listing, prioritizing the App scene.
/**
 * @brief 获取账号的 App 优先目录，不使用静态模型回退
 * @param ctx 网络请求上下文
 * @param c 已验证的账号凭证
 * @return 可用模型、禁用模型及免费视图状态；失败时为 nil
 * @return 网络、目录格式或上游业务错误
 */
func (s *Service) FetchCatalog(ctx context.Context, c *Credential) (*Catalog, error) {
	if c == nil {
		return nil, errors.New("qoder: credential is required")
	}

	base := s.BaseURL
	if base == "" {
		base = "https://api3.qoder.sh"
		if c.Region == "cn" {
			base = "https://gateway.qoder.com.cn"
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/algo/api/v2/model/list?Encode=1", nil)
	if err != nil {
		return nil, err
	}
	if err := cosySign(req, c, nil); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	const limit = 8 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > limit {
		return nil, errors.New("qoder: model catalog exceeds 8 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, ClassifyError(response.StatusCode, body)
	}
	var catalog struct {
		Status int               `json:"statusCodeValue"`
		Code   int               `json:"code"`
		Body   json.RawMessage   `json:"body"`
		Chat   []json.RawMessage `json:"chat"`
		App    []json.RawMessage `json:"app"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("qoder model catalog: %w", err)
	}
	if (catalog.Status != 0 && catalog.Status != 200) || (catalog.Code != 0 && catalog.Code != 200) {
		return nil, ClassifyError(catalog.Status, body)
	}
	if len(catalog.App) == 0 && len(catalog.Chat) == 0 && len(catalog.Body) != 0 {
		inner := catalog.Body
		var text string
		if json.Unmarshal(inner, &text) == nil {
			inner = []byte(text)
		}
		if err := json.Unmarshal(inner, &catalog); err != nil {
			return nil, fmt.Errorf("qoder model catalog body: %w", err)
		}
		if (catalog.Status != 0 && catalog.Status != 200) || (catalog.Code != 0 && catalog.Code != 200) {
			return nil, ClassifyError(catalog.Status, body)
		}
	}
	result := &Catalog{}
	seenKeys := map[string]bool{}
	seenNames := map[string]bool{}
	paidOff, paidOn, freeOn := false, false, false
	for _, raw := range append(catalog.App, catalog.Chat...) {
		var entry struct {
			Key       string `json:"key"`
			Name      string `json:"display_name"`
			Enabled   bool   `json:"enable"`
			Reasoning bool   `json:"is_reasoning"`
			Images    bool   `json:"is_vl"`
			Thinking  struct {
				Enabled *struct {
					Efforts map[string]any `json:"efforts"`
				} `json:"enabled"`
			} `json:"thinking_config"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, fmt.Errorf("qoder model entry: %w", err)
		}
		key := strings.TrimSpace(entry.Key)
		if key == "" || key == "auto" || key == "default" || seenKeys[key] {
			continue
		}
		seenKeys[key] = true
		id := cosyModelID(entry.Name)
		if id == "" || seenNames[id] {
			id = key
		}
		seenNames[id] = true
		model := Model{ID: id, Key: key, Config: raw, SupportsReasoning: entry.Reasoning || entry.Thinking.Enabled != nil, SupportsImages: entry.Images}
		if entry.Thinking.Enabled != nil {
			for effort := range entry.Thinking.Enabled.Efforts {
				model.Efforts = append(model.Efforts, effort)
			}
		}
		sort.Strings(model.Efforts)
		free := freeModel(raw)
		if entry.Enabled {
			result.Models = append(result.Models, model)
			if free {
				freeOn = true
			} else {
				paidOn = true
			}
		} else {
			result.Disabled = append(result.Disabled, model)
			if !free {
				paidOff = true
			}
		}
	}
	if len(result.Models) == 0 && len(result.Disabled) == 0 {
		return nil, errors.New("qoder: upstream returned no valid models")
	}
	result.FreeOnly = paidOff && !paidOn && freeOn
	return result, nil
}

/**
 * @brief 区分目录中的真正免费模型和限时零价折扣
 * @param raw 已验证的目录条目 JSON
 * @return 当前价格为零且非付费折扣活动时为 true
 */
func freeModel(raw json.RawMessage) bool {
	var entry struct {
		Price               *float64        `json:"price_factor"`
		CamelPrice          *float64        `json:"priceFactor"`
		Free                bool            `json:"is_free"`
		CamelFree           bool            `json:"isFree"`
		Promotion           json.RawMessage `json:"promotion"`
		MisspelledPromotion json.RawMessage `json:"prommotion"`
	}
	_ = json.Unmarshal(raw, &entry)
	price := entry.Price
	if price == nil {
		price = entry.CamelPrice
	}
	if price == nil {
		return entry.Free || entry.CamelFree
	}
	if *price != 0 {
		return false
	}
	promotion := entry.Promotion
	if len(promotion) == 0 {
		promotion = entry.MisspelledPromotion
	}
	var discount struct {
		Active      bool    `json:"active"`
		Before      float64 `json:"before_promotion_price_factor"`
		CamelBefore float64 `json:"beforePromotionPriceFactor"`
	}
	_ = json.Unmarshal(promotion, &discount)
	return !discount.Active || (discount.Before <= 0 && discount.CamelBefore <= 0)
}
