package qoderauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
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

// ClassifyError maps Qoder business failures before considering HTTP status.
/**
 * @brief 优先按业务码区分排队、额度、请求错误和认证失败
 * @param status HTTP 状态或信封状态
 * @param body 上游响应 JSON
 * @return 规范化的 OpenAI 错误及明确的重试参数
 */
func ClassifyError(status int, body []byte) *APIError {
	var root any
	_ = json.Unmarshal(body, &root)
	codes := map[int]bool{}
	queued := false
	retryAfter := 0
	message := ""
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
			}
		case []any:
			for _, child := range value {
				visit(child, depth+1)
			}
		case map[string]any:
			for key, child := range value {
				switch strings.ToLower(key) {
				case "code", "errorcode", "statuscodevalue", "status":
					switch number := child.(type) {
					case float64:
						codes[int(number)] = true
					case string:
						if n, err := strconv.Atoi(number); err == nil {
							codes[n] = true
						}
					}
				case "retryafterseconds":
					if n, ok := child.(float64); ok && n > 0 {
						retryAfter = int(n)
						if float64(retryAfter) < n {
							retryAfter++
						}
					}
				case "serviceavailable":
					if child == false {
						queued = true
					}
				case "isqueued":
					if child == true {
						queued = true
					}
				case "message", "msg":
					if text, ok := child.(string); ok && text != "" {
						message = text
					}
				}
				visit(child, depth+1)
			}
		}
	}
	visit(root, 0)
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
	case codes[105] || status == http.StatusUnauthorized || status == http.StatusForbidden:
		result.StatusCode, result.Type, result.Code = http.StatusUnauthorized, "authentication_error", "authentication_error"
	case strings.Contains(lower, "insufficient") || strings.Contains(lower, "no_quota") || strings.Contains(lower, "quota_exceed") || strings.Contains(lower, "credit") || strings.Contains(lower, "pricingurl"):
		result.StatusCode, result.Type, result.Code = http.StatusTooManyRequests, "insufficient_quota", "insufficient_quota"
	case status == http.StatusTooManyRequests || strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests"):
		result.StatusCode, result.Type, result.Code = http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded"
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
