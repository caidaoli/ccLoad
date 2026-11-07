package qoderauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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

// APIError exposes an HTTP-equivalent status for errors carried inside a 200
// response. It is also the body of a normalized OpenAI SSE error event.
type APIError struct {
	StatusCode int    `json:"status"`
	Type       string `json:"type"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

func (e *APIError) Error() string { return e.Message }

// ClassifyError translates Qoder business codes before considering HTTP status.
// In particular HTTP 403 can mean authentication, quota exhaustion, or queueing.
func ClassifyError(status int, body []byte) *APIError {
	var root any
	_ = json.Unmarshal(body, &root)
	codes := map[int]bool{}
	queued := false
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
	switch {
	case codes[10605] || queued:
		result.StatusCode, result.Type, result.Code = http.StatusServiceUnavailable, "server_error", "service_unavailable"
	case codes[105] || status == http.StatusUnauthorized:
		result.StatusCode, result.Type, result.Code = http.StatusUnauthorized, "authentication_error", "authentication_error"
	case codes[110] || codes[112] || codes[113] || codes[114] || codes[115] || codes[116] || codes[117] || codes[118] || codes[119] || codes[122] || strings.Contains(lower, "insufficient") || strings.Contains(lower, "no_quota") || strings.Contains(lower, "quota_exceed") || strings.Contains(lower, "credit") || strings.Contains(lower, "pricingurl"):
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

// FetchModels obtains the account's current native catalog without a static
// fallback: advertising guessed routing keys would misroute inference.
func (s *Service) FetchModels(ctx context.Context, c *Credential) ([]Model, error) {
	if c == nil {
		return nil, errors.New("qoder: credential is required")
	}
	// Catalog discovery uses api3 even for job tokens; only inference is
	// routed to api2. The regional gateway is shared for Chinese accounts.
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
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("qoder model catalog: %w", err)
	}
	if (catalog.Status != 0 && catalog.Status != 200) || (catalog.Code != 0 && catalog.Code != 200) {
		return nil, ClassifyError(catalog.Status, body)
	}
	if len(catalog.Chat) == 0 && len(catalog.Body) != 0 {
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
	models := make([]Model, 0, len(catalog.Chat))
	seen := map[string]bool{}
	for _, raw := range catalog.Chat {
		var entry struct {
			Key       string `json:"key"`
			Name      string `json:"display_name"`
			Reasoning bool   `json:"is_reasoning"`
			Images    bool   `json:"is_vl"`
			Thinking  struct {
				Enabled struct {
					Efforts map[string]any `json:"efforts"`
				} `json:"enabled"`
			} `json:"thinking_config"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, fmt.Errorf("qoder model entry: %w", err)
		}
		id := cosyModelID(entry.Name)
		if id == "" || strings.TrimSpace(entry.Key) == "" || seen[id] {
			continue
		}
		seen[id] = true
		model := Model{ID: id, Key: entry.Key, Config: raw, SupportsReasoning: entry.Reasoning || len(entry.Thinking.Enabled.Efforts) > 0, SupportsImages: entry.Images}
		for effort := range entry.Thinking.Enabled.Efforts {
			model.Efforts = append(model.Efforts, effort)
		}
		sort.Strings(model.Efforts)
		models = append(models, model)
	}
	if len(models) == 0 {
		return nil, errors.New("qoder: upstream returned no valid chat models")
	}
	return models, nil
}
