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

func TestCatalogUsesRegionalHostInsteadOfJobInferenceHost(t *testing.T) {
	for _, region := range []string{"global", "cn"} {
		t.Run(region, func(t *testing.T) {
			catalogHost := "api3.qoder.sh"
			inferenceHost := "api2.qoder.sh"
			if region == "cn" {
				catalogHost, inferenceHost = "gateway.qoder.com.cn", "gateway.qoder.com.cn"
			}
			service := &Service{Client: &http.Client{Transport: qoderRoundTripper(func(request *http.Request) (*http.Response, error) {
				if request.URL.Host != catalogHost {
					t.Errorf("catalog host %s, want %s", request.URL.Host, catalogHost)
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"chat":[{"key":"key","display_name":"Model"}]}`))}, nil
			})}}
			credential := &Credential{Region: region, UserID: "u", AccessToken: "jt-token", MachineID: "m"}
			models, err := service.FetchModels(context.Background(), credential)
			if err != nil {
				t.Fatal(err)
			}
			request, err := service.BuildChatRequest(context.Background(), credential, []byte(`{"messages":[{"role":"user","content":"hello"}]}`), models[0])
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

func TestFetchModelsUsesSignedCatalogAndNativeKeys(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertCOSYRequest(t, r, nil)
		if r.Method != "GET" || r.URL.Path != "/algo/api/v2/model/list" || r.URL.Query().Get("Encode") != "1" {
			t.Errorf("wrong catalog target: %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"chat":[{"key":"qfmodel","display_name":"Qwen 3.8 Flash","is_vl":true,"is_reasoning":true,"enable":false,"source":"system","thinking_config":{"enabled":{"efforts":{"low":{},"medium":{},"xhigh":{}}}}},{"key":"duplicate","display_name":"qwen3.8flash"},{"key":"other","display_name":"Other Model"},{"key":"","display_name":"invalid"}]}`)
	}))
	defer server.Close()
	s := &Service{Client: server.Client(), BaseURL: server.URL}
	models, err := s.FetchModels(context.Background(), &Credential{UserID: "u1", AccessToken: "dt-secret", MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0].ID != "qwen3.8flash" || models[0].Key != "qfmodel" || !models[0].SupportsReasoning || !models[0].SupportsImages || len(models[0].Efforts) != 3 {
		t.Fatalf("invalid catalog: %+v", models)
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
			_, err := s.FetchModels(context.Background(), &Credential{UserID: "u", AccessToken: "token", MachineID: "m"})
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
