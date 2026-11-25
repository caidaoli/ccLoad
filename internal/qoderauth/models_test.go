package qoderauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type qoderRoundTripper func(*http.Request) (*http.Response, error)

func (f qoderRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCatalogAndInferenceUseRegionalHost(t *testing.T) {
	for _, region := range []string{"global", "cn"} {
		t.Run(region, func(t *testing.T) {
			catalogHost := "api3.qoder.sh"
			inferenceHost := "api3.qoder.sh"
			if region == "cn" {
				catalogHost, inferenceHost = "gateway.qoder.com.cn", "gateway.qoder.com.cn"
			}
			service := &Service{Client: &http.Client{Transport: qoderRoundTripper(func(request *http.Request) (*http.Response, error) {
				if request.URL.Host != catalogHost {
					t.Errorf("catalog host %s, want %s", request.URL.Host, catalogHost)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"chat":[{"key":"key","display_name":"Model","enable":true}]}`))}, nil
			})}}
			credential := &Credential{Region: region, UserID: "u", AccessToken: "jt-token", MachineID: "m"}
			models, err := service.FetchCatalog(context.Background(), credential)
			if err != nil {
				t.Fatal(err)
			}
			request, err := service.BuildChatRequest(context.Background(), credential, []byte(`{"messages":[{"role":"user","content":"hello"}]}`), models.Models[0])
			if err != nil {
				t.Fatal(err)
			}
			if request.URL.Host != inferenceHost {
				t.Errorf("inference host %s, want %s", request.URL.Host, inferenceHost)
			}
			if err := request.Body.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

/**
 * @brief App 场景优先、过滤自动/禁用项，并保留重名原生模型
 * @param t 测试上下文
 */
func TestFetchCatalogUsesAppAndNativeKeys(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertCOSYRequest(t, r, nil)
		if r.Method != "GET" || r.URL.Path != "/algo/api/v2/model/list" || r.URL.Query().Get("Encode") != "1" {
			t.Errorf("wrong catalog target: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"app":[{"key":"qfmodel","display_name":"Qwen 3.8 Flash","enable":true,"price_factor":0,"is_vl":true,"thinking_config":{"enabled":{"efforts":{"low":{},"medium":{},"xhigh":{}}}}},{"key":"fast","display_name":"Fast","enable":true}],"chat":[{"key":"qfmodel","display_name":"Old Name","enable":false},{"key":"duplicate","display_name":"qwen3.8flash","enable":true},{"key":"paid","display_name":"Paid","enable":false},{"key":"auto","display_name":"Auto","enable":true},{"key":"default","display_name":"Default","enable":true},{"key":"","display_name":"invalid"}]}`)
	}))
	defer server.Close()
	s := &Service{Client: server.Client(), BaseURL: server.URL}
	catalog, err := s.FetchCatalog(context.Background(), &Credential{UserID: "u1", AccessToken: "dt-secret", MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	models := catalog.Models
	if len(models) != 3 || models[0].ID != "qwen3.8flash" || models[0].Key != "qfmodel" || !models[0].SupportsReasoning || !models[0].SupportsImages || len(models[0].Efforts) != 3 || models[1].Key != "fast" || models[2].ID != "duplicate" {
		t.Fatalf("invalid catalog: %+v", catalog)
	}
	if len(catalog.Disabled) != 1 || catalog.Disabled[0].Key != "paid" || catalog.FreeOnly {
		t.Fatalf("invalid disabled/free view: %+v", catalog)
	}
}

/**
 * @brief 免费视图依据实际价格与促销信息判定，目录接受嵌套信封
 * @param t 测试上下文
 */
func TestFetchCatalogFreeView(t *testing.T) {
	for _, tc := range []struct {
		name, model string
		want        bool
	}{
		{"free", `{"price_factor":0}`, true},
		{"paid", `{"price_factor":1,"is_free":true}`, false},
		{"discount", `{"price_factor":0,"promotion":{"active":true,"before_promotion_price_factor":1}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := strings.TrimSuffix(tc.model, "}") + `,"key":"free","display_name":"Free","enable":true}`
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, `{"statusCodeValue":200,"body":{"app":[`+model+`,{"key":"paid","display_name":"Paid","price_factor":1,"enable":false}]}}`)
			}))
			defer server.Close()
			s := &Service{Client: server.Client(), BaseURL: server.URL}
			catalog, err := s.FetchCatalog(context.Background(), &Credential{UserID: "u", AccessToken: "token", MachineID: "m"})
			if err != nil {
				t.Fatal(err)
			}
			if catalog.FreeOnly != tc.want {
				t.Fatalf("free view=%t want %t", catalog.FreeOnly, tc.want)
			}
		})
	}
}

func TestFetchModelsRejectsInvalidAndBusinessErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body         string
		httpStatus, status int
	}{
		{"auth", `{"statusCodeValue":403,"body":"{\"code\":105}"}`, 200, 401},
		{"quota", `{"code":112,"message":"exhausted"}`, 200, 429},
		{"nested auth", `{"statusCodeValue":200,"body":{"code":105,"message":"expired"}}`, 200, 401},
		{"queue", `{"code":10605}`, 403, 503},
		{"empty", `{"chat":[]}`, 200, 0},
		{"invalid", `{broken}`, 200, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.httpStatus)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			s := &Service{Client: server.Client(), BaseURL: server.URL}
			_, err := s.FetchCatalog(context.Background(), &Credential{UserID: "u", AccessToken: "token", MachineID: "m"})
			if err == nil {
				t.Fatal("expected catalog error")
			}
			if tc.status != 0 {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != tc.status {
					t.Fatalf("got %v, want status %d", err, tc.status)
				}
			}
		})
	}
}

/**
 * @brief 业务错误优先于 HTTP 状态，普通拒绝不视为登录过期
 * @param t 测试上下文
 */
func TestQoderClassifyRequestFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body, code                string
		httpStatus, status, limit, wait int
	}{
		{"auth", `{"message":"Login expired"}`, "authentication_error", 403, 401, 0, 0},
		{"auth wording", `{"error":{"message":"Login or access token expired"}}`, "authentication_error", 403, 401, 0, 0},
		{"business auth", `{"code":"105"}`, "authentication_error", 403, 401, 0, 0},
		{"unavailable flag alone preserves business auth", `{"code":105,"serviceAvailable":false}`, "authentication_error", 403, 401, 0, 0},
		{"blank queue type preserves business auth", `{"code":105,"serviceAvailable":false,"queueType":" "}`, "authentication_error", 403, 401, 0, 0},
		{"queue fields in separate objects preserve business auth", `{"code":105,"body":{"serviceAvailable":false},"details":{"queueType":"p3"}}`, "authentication_error", 403, 401, 0, 0},
		{"transport auth", `{}`, "authentication_error", 401, 401, 0, 0},
		{"embedded auth", `{"statusCodeValue":"401","body":{"message":"unauthorized"}}`, "authentication_error", 200, 401, 0, 0},
		{"embedded auth code", `{"error":{"code":"401"}}`, "authentication_error", 200, 401, 0, 0},
		{"forbidden", `{}`, "permission_denied", 403, 403, 0, 0},
		{"forbidden code in JSON response", `{"code":"403","message":"Forbidden"}`, "permission_denied", 200, 403, 0, 0},
		{"insufficient permission", `{"message":"Insufficient permission to view credit details","pricingUrl":null}`, "permission_denied", 403, 403, 0, 0},
		{"embedded forbidden", `{"statusCodeValue":200,"body":"{\"status\":403}"}`, "permission_denied", 200, 403, 0, 0},
		{"outer status wins", `{"statusCodeValue":403,"body":{"code":401}}`, "permission_denied", 200, 403, 0, 0},
		{"pricing URL", `{"body":{"data":{"pricingUrl":"https://example.com/pricing"}}}`, "insufficient_quota", 403, 429, 0, 0},
		{"explicit quota text", `{"message":"Insufficient credits"}`, "insufficient_quota", 403, 429, 0, 0},
		{"quota text without business code", `{"message":"credits exhausted"}`, "insufficient_quota", 429, 429, 0, 0},
		{"quota in plain body", `{"statusCodeValue":403,"body":"Insufficient credits"}`, "insufficient_quota", 403, 429, 0, 0},
		{"expired login in plain body", `{"statusCodeValue":403,"body":"Login or access token expired"}`, "authentication_error", 403, 401, 0, 0},
		{"explicit quota code in message", `{"message":"insufficient_quota"}`, "insufficient_quota", 403, 429, 0, 0},
		{"explicit quota code", `{"error":{"code":"insufficient_quota"}}`, "insufficient_quota", 403, 429, 0, 0},
		{"embedded rate limit", `{"statusCodeValue":429,"body":{"message":"retry later"}}`, "rate_limit_exceeded", 200, 429, 0, 0},
		{"request", `{"code":80411,"message":"refused"}`, "invalid_request", 403, 400, 0, 0},
		{"quota", `{"code":112,"message":"exhausted"}`, "insufficient_quota", 401, 429, 0, 0},
		{"upstream", `{"message":"Error in upstream response"}`, "upstream_error", 400, 502, 0, 0},
		{"range", `{"message":"Error in upstream response: Range of max_tokens should be [1, 8192]"}`, "max_tokens_exceeded", 400, 400, 8192, 0},
		{"queued", `{"code":10605,"queue":{"isQueued":true,"retryAfterSeconds":12}}`, "service_unavailable", 403, 503, 0, 12},
		{"queue code with no valid hint", `{"code":"10605","retryAfterSeconds":"Infinity","waitTime":-1}`, "service_unavailable", 403, 503, 0, 30},
		{"queued flag with wait time", `{"message":"{\"isQueued\":true,\"waitTime\":\"12.25\"}"}`, "service_unavailable", 403, 503, 0, 13},
		{"unavailable with queue type", `{"body":{"serviceAvailable":false,"queueType":"p3","retryAfterSeconds":0.2}}`, "service_unavailable", 403, 503, 0, 1},
		{"retry seconds take priority across envelopes", `{"isQueued":true,"waitTime":30,"body":{"retryAfterSeconds":"8.5"}}`, "service_unavailable", 403, 503, 0, 9},
		{"outer retry seconds take priority", `{"isQueued":true,"retryAfterSeconds":" 8 ","body":{"retryAfterSeconds":12}}`, "service_unavailable", 403, 503, 0, 8},
		{"invalid retry seconds fall back to positive wait time", `{"isQueued":true,"retryAfterSeconds":"invalid","waitTime":7}`, "service_unavailable", 403, 503, 0, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ClassifyError(tc.httpStatus, []byte(tc.body))
			if err.StatusCode != tc.status || err.Code != tc.code || err.MaxTokens != tc.limit || err.RetryAfter != tc.wait {
				t.Fatalf("got %+v, want status=%d code=%s max_tokens=%d retry_after=%d", *err, tc.status, tc.code, tc.limit, tc.wait)
			}
		})
	}
}
