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
 * @brief 业务错误优先于 HTTP 认证状态，并保留排队/输出上限信息
 * @param t 测试上下文
 */
func TestQoderClassifyRequestFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body                      string
		httpStatus, status, limit, wait int
	}{
		{"auth", `{"message":"Login expired"}`, 403, 401, 0, 0},
		{"request", `{"code":80411,"message":"refused"}`, 403, 400, 0, 0},
		{"quota", `{"code":112,"message":"exhausted"}`, 401, 429, 0, 0},
		{"upstream", `{"message":"Error in upstream response"}`, 400, 502, 0, 0},
		{"range", `{"message":"Error in upstream response: Range of max_tokens should be [1, 8192]"}`, 400, 400, 8192, 0},
		{"queued", `{"code":10605,"queue":{"isQueued":true,"retryAfterSeconds":12}}`, 403, 503, 0, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ClassifyError(tc.httpStatus, []byte(tc.body))
			if err.StatusCode != tc.status || err.MaxTokens != tc.limit || err.RetryAfter != tc.wait {
				t.Fatalf("got %+v", err)
			}
		})
	}
}
