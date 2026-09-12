package codebuddyauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type codeBuddyRoundTripper func(*http.Request) (*http.Response, error)

func (f codeBuddyRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestFetchModelsLiveCatalog(t *testing.T) {
	for _, body := range []string{
		`{"code":0,"data":{"agents":[{"name":"cli","models":["future-model","hy3-new","blocked-model"]}],"models":[{"id":" future-model "},{"id":"hy3-new","disabled":false},{"id":"blocked-model","disabled":true,"disabledReason":"unavailable"},{"id":"future-model"},{"id":""}]}}`,
		`{"code":0,"data":{"agents":[{"name":"cli","models":["blocked-model"]}],"models":[{"id":"blocked-model","disabled":true}]}}`,
		`{"code":0,"data":{"models":[]}}`,
		`{"code":0,"data":{"models":null}}`,
		`{"code":0,"data":{"models":{}}}`,
		`{"code":14001,"msg":"secret-access secret-refresh"}`,
	} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v3/config" {
					t.Errorf("invalid catalog request %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer secret-access" || r.Header.Get("X-Refresh-Token") != "secret-refresh" || r.Header.Get("X-User-Id") != "uid" {
					t.Error("missing credential headers")
				}
				_, _ = w.Write([]byte(body))
			}))
			defer upstream.Close()
			service := NewService(upstream.Client())
			service.BaseURL = upstream.URL
			names, err := service.FetchModels(context.Background(), &Credential{AccessToken: "secret-access", RefreshToken: "secret-refresh", UID: "uid"})
			if strings.Contains(body, "future-model") {
				if err != nil || strings.Join(names, ",") != "future-model,hy3-new" {
					t.Fatalf("names=%v err=%v", names, err)
				}
			} else if err == nil {
				t.Fatal("invalid catalog accepted")
			} else if strings.Contains(err.Error(), "secret-") {
				t.Fatal("error leaked credentials")
			}
		})
	}
}

func TestFetchModelsUsesCLIAgent(t *testing.T) {
	for _, tc := range []struct {
		name, selection, want string
		wantError             bool
	}{
		{"cli-scope", `"agents":[{"name":"other","models":["chat-model"]},{"name":"cli","models":["new-model","new-model","alias-model","missing","blocked"]}],`, "new-model", false},
		{"available-intersection", `"agents":[{"name":"cli","models":["new-model","canonical"]}],"availableModels":["canonical","chat-model"],`, "canonical", false},
		{"missing-cli", `"agents":[{"name":"other","models":["new-model"]}],`, "", true},
		{"missing-agents", "", "", true},
		{"empty-available", `"agents":[{"name":"cli","models":["new-model"]}],"availableModels":[],`, "new-model", false},
		{"empty-cli", `"agents":[{"name":"cli","models":[]}],"availableModels":["new-model"],`, "", true},
		{"only-disabled", `"agents":[{"name":"cli","models":["blocked"]}],`, "", true},
		{"only-undefined", `"agents":[{"name":"cli","models":["missing"]}],`, "", true},
		{"no-fixed-exclusions", `"agents":[{"name":"cli","models":["glm-4.6","minimax-m2.5"]}],`, "glm-4.6,minimax-m2.5", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `{"code":0,"data":{%s"models":[{"id":" glm-4.6 "},{"id":"glm-4.6v"},{"id":"glm-4.7"},{"id":"glm-5.0"},{"id":"hunyuan-image-v3.0-art"},{"id":"hy4-preview-x"},{"id":"kimi-k2-thinking"},{"id":"minimax-m2.5"},{"id":"new-model"},{"id":"canonical","aliases":["alias-model"]},{"id":"chat-model","tags":["chat"]},{"id":"glm-5.0-new"},{"id":"blocked","disabled":true}]}}`, tc.selection)
			}))
			defer upstream.Close()
			service := NewService(upstream.Client())
			service.BaseURL = upstream.URL
			names, err := service.FetchModels(context.Background(), &Credential{AccessToken: "access"})
			if (err != nil) != tc.wantError || strings.Join(names, ",") != tc.want {
				t.Fatalf("models=%v err=%v", names, err)
			}
		})
	}
}

func TestBillingCheckinAndUserResource(t *testing.T) {
	t.Parallel()
	var checkins atomic.Int32
	var resources atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method=%s, want POST", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer access" || r.Header.Get("X-Refresh-Token") != "" || r.Header.Get("X-User-Id") != "uid" || r.Header.Get("X-Enterprise-Id") != "" || r.Header.Get("X-Tenant-Id") != "" || r.Header.Get("X-Domain") != "team" {
			t.Error("billing request missing credential headers")
		}
		switch r.URL.Path {
		case "/v2/billing/meter/daily-checkin":
			checkins.Add(1)
			_, _ = fmt.Fprint(w, `{"code":0,"data":{}}`)
		case "/v2/billing/meter/get-user-resource":
			resources.Add(1)
			var request struct {
				PageNumber  int    `json:"PageNumber"`
				PageSize    int    `json:"PageSize"`
				ProductCode string `json:"ProductCode"`
				Status      []int  `json:"Status"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.PageNumber != 1 || request.PageSize != 100 || request.ProductCode != "p_tcaca" || len(request.Status) != 2 {
				t.Errorf("unexpected resource request: %+v, err=%v", request, err)
			}
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":2000,"CycleCapacityRemain":1200,"CycleCapacityUsed":800,"CapacityRemain":9999},{"CycleCapacitySize":0,"CycleCapacityRemain":300,"CycleCapacityUsed":700,"CapacityRemain":9999},{"CycleCapacitySize":0,"CycleCapacityRemain":0,"CycleCapacityUsed":0,"CapacityRemain":100},{"CycleCapacitySize":100,"CycleCapacityRemain":-1,"CycleCapacityUsed":101,"CapacityRemain":500}]}}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	service := NewService(upstream.Client())
	service.BaseURL = upstream.URL
	service.Now = func() time.Time { return time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC) }
	credential := &Credential{AccessToken: "access", RefreshToken: "refresh", UID: "uid", Domain: "team"}
	if err := service.DailyCheckin(context.Background(), credential); err != nil {
		t.Fatalf("DailyCheckin: %v", err)
	}
	usage, err := service.UserResource(context.Background(), credential)
	if err != nil {
		t.Fatalf("UserResource: %v", err)
	}
	if usage.Remain != 1600 || usage.Total != nil || usage.Used != nil {
		t.Fatalf("usage=%+v, want remaining 1600 without incomplete totals", usage)
	}
	if checkins.Load() != 1 || resources.Load() != 1 {
		t.Fatalf("checkins=%d resources=%d", checkins.Load(), resources.Load())
	}
}

func TestPersonalUserResourceTotals(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/billing/meter/get-user-resource" {
			t.Errorf("unexpected personal resource path %s", r.URL.Path)
		}
		_, _ = fmt.Fprint(w, `{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":500,"CycleCapacityRemain":484.5,"CapacitySize":9999},{"CapacitySize":1500,"CapacityRemain":1400}]}}}}`)
	}))
	defer upstream.Close()
	service := NewService(upstream.Client())
	service.BaseURL = upstream.URL
	usage, err := service.UserResource(context.Background(), &Credential{AccessToken: "access"})
	if err != nil {
		t.Fatal(err)
	}
	if usage.Remain != 1884.5 || usage.Total == nil || *usage.Total != 2000 || usage.Used == nil || *usage.Used != 115.5 {
		t.Fatalf("incorrect personal aggregate: %+v", usage)
	}
}

func TestEnterpriseUserResource(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		data      string
		want      float64
		unlimited bool
		wantError bool
	}{
		{name: "fractional remaining credits", data: `{"credit":104.35,"limitNum":2000}`, want: 1895.65},
		{name: "exhausted", data: `{"credit":2001.5,"limitNum":2000}`},
		{name: "zero allowance", data: `{"credit":0,"limitNum":0}`},
		{name: "missing usage", data: `{"limitNum":2000}`, wantError: true},
		{name: "missing limit", data: `{"credit":104.35}`, wantError: true},
		{name: "null response", data: `null`, wantError: true},
		{name: "unlimited allowance", data: `{"credit":123.45,"limitNum":-1}`, unlimited: true},
		{name: "invalid negative allowance", data: `{"credit":0,"limitNum":-0.5}`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v2/billing/meter/get-enterprise-user-usage" {
					t.Errorf("unexpected enterprise request: %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("Authorization") != "Bearer access" || r.Header.Get("X-User-Id") != "uid" || r.Header.Get("X-Enterprise-Id") != "enterprise" || r.Header.Get("X-Refresh-Token") != "" {
					t.Error("incorrect enterprise billing identity headers")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body == nil || len(body) != 0 {
					t.Errorf("expected empty JSON request, got %v, err=%v", body, err)
				}
				_, _ = fmt.Fprintf(w, `{"code":0,"data":%s}`, tc.data)
			}))
			defer upstream.Close()
			service := NewService(upstream.Client())
			service.BaseURL = upstream.URL
			usage, err := service.UserResource(context.Background(), &Credential{AccessToken: "access", RefreshToken: "refresh", UID: "uid", EnterpriseID: "enterprise"})
			if (err != nil) != tc.wantError {
				t.Fatalf("UserResource() error=%v; want error=%v", err, tc.wantError)
			}
			if tc.wantError {
				return
			}
			if usage == nil || usage.Remain != tc.want || usage.Unlimited != tc.unlimited || usage.Used == nil || (usage.Total == nil) != tc.unlimited {
				t.Fatalf("UserResource() = %+v; want remaining %g, unlimited=%v", usage, tc.want, tc.unlimited)
			}
		})
	}
}

func TestEnterpriseDailyCheckinIsSkipped(t *testing.T) {
	t.Parallel()
	service := NewService(nil)
	if err := service.DailyCheckin(context.Background(), &Credential{AccessToken: "access", EnterpriseID: "enterprise"}); err != nil {
		t.Fatalf("enterprise DailyCheckin() = %v", err)
	}
}

func TestInternationalDailyCheckinIsUnsupportedWithoutUpstreamRequest(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	service := NewService(&http.Client{Transport: codeBuddyRoundTripper(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected international check-in request")
	})})
	err := service.DailyCheckin(context.Background(), &Credential{
		AccessToken: "access",
		BaseURL:     InternationalBaseURL,
	})
	if !errors.Is(err, ErrDailyCheckinUnsupported) {
		t.Fatalf("international DailyCheckin() = %v, want ErrDailyCheckinUnsupported", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("international check-in requests=%d, want 0", requests.Load())
	}
}

func TestInternationalOAuthUsesInternationalEndpoint(t *testing.T) {
	t.Parallel()
	var requests []string
	client := &http.Client{Transport: codeBuddyRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.String())
		if r.URL.Host != "www.codebuddy.ai" {
			t.Fatalf("request host=%q, want international host", r.URL.Host)
		}
		if r.Header.Get("Origin") != InternationalBaseURL {
			t.Fatalf("origin=%q, want %q", r.Header.Get("Origin"), InternationalBaseURL)
		}
		var body string
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			body = `{"code":0,"data":{"state":"intl-state","authUrl":"https://www.codebuddy.ai/login"}}`
		case "/v2/plugin/auth/token":
			body = `{"code":0,"data":{"accessToken":"intl-access","refreshToken":"intl-refresh","expiresIn":3600}}`
		case "/v2/plugin/login/account":
			body = `{"code":0,"data":{"uid":"intl-user","nickname":"International"}}`
		case "/v2/plugin/auth/token/refresh":
			body = `{"code":0,"data":{"accessToken":"intl-new-access","refreshToken":"intl-new-refresh","expiresIn":3600}}`
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	service := NewService(client)
	service.Now = func() time.Time { return time.Unix(1000, 0) }
	login, err := service.StartAt(context.Background(), InternationalBaseURL)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := service.Poll(context.Background(), login)
	if err != nil {
		t.Fatal(err)
	}
	if credential.BaseURL != InternationalBaseURL || credential.UID != "intl-user" {
		t.Fatalf("credential=%+v, want international endpoint and identity", credential)
	}
	refreshed, err := service.Refresh(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.BaseURL != InternationalBaseURL || refreshed.AccessToken != "intl-new-access" {
		t.Fatalf("refreshed=%+v, want international endpoint", refreshed)
	}
	if len(requests) != 4 {
		t.Fatalf("requests=%v, want state/token/account/refresh", requests)
	}
}

func TestBillingHTTPErrorRetainsBusinessCode(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"code":10001,"msg":"今日已签到"}`)
	}))
	defer server.Close()
	service := NewService(server.Client())
	service.BaseURL = server.URL
	err := service.DailyCheckin(context.Background(), &Credential{AccessToken: "access"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != 10001 {
		t.Fatalf("error=%v, want business code 10001", err)
	}
}

// This new provider package has no existing test file; exercise its public
// authentication and persistence contracts through a simulated control plane.
func TestLoginRefreshAndCredentialRoundTrip(t *testing.T) {
	t.Parallel()
	var states atomic.Int32
	var tokenPolls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v2/plugin/auth/state":
			if r.Method != http.MethodPost || r.URL.Query().Get("platform") != "CLI" {
				t.Error("invalid login request")
			}
			state := fmt.Sprint(states.Add(1))
			http.SetCookie(w, &http.Cookie{Name: "login", Value: state, Path: "/"})
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": map[string]any{"state": state, "authUrl": "https://www.codebuddy.cn/login"}})
		case "/v2/plugin/auth/token":
			cookie, err := r.Cookie("login")
			if err != nil || cookie.Value != r.URL.Query().Get("state") {
				t.Error("login cookies crossed sessions")
			}
			if tokenPolls.Add(1) == 1 {
				_, _ = fmt.Fprint(w, `{"code":11217}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"accessToken":"access","refreshToken":"refresh","expiresIn":3600,"domain":"team"}}`)
		case "/v2/plugin/login/account":
			if r.Header.Get("Authorization") != "Bearer access" {
				t.Error("account bearer missing")
			}
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"uid":"u1","enterpriseId":"e1","nickname":"Tester"}}`)
		case "/v2/plugin/auth/token/refresh":
			if r.Header.Get("X-Refresh-Token") != "refresh" || r.Header.Get("X-Enterprise-Id") != "e1" {
				t.Error("refresh identity missing")
			}
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"accessToken":"new-access","refreshToken":"rotated","expiresIn":7200}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	s := NewService(upstream.Client())
	s.BaseURL = upstream.URL
	s.Now = func() time.Time { return time.Unix(1000, 0) }
	a, err := s.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.State == b.State {
		t.Fatal("login states reused")
	}
	pending, err := s.Poll(context.Background(), a)
	if err != nil || pending != nil {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	for _, login := range []*Login{a, b} {
		c, err := s.Poll(context.Background(), login)
		if err != nil {
			t.Fatal(err)
		}
		if c.UID != "u1" || c.EnterpriseID != "e1" || c.ExpiresAt != 4600 {
			t.Fatalf("unexpected credential: %+v", c)
		}
		fresh, err := s.Refresh(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.AccessToken != "new-access" || fresh.RefreshToken != "rotated" || fresh.Domain != "team" || fresh.UID != "u1" {
			t.Fatal("refresh lost fields")
		}
		raw, err := fresh.JSON()
		if err != nil {
			t.Fatal(err)
		}
		stored, err := ParseCredential([]byte(raw))
		if err != nil || *stored != *fresh {
			t.Fatalf("round-trip err=%v", err)
		}
	}
}

func TestCredentialImportAndFailureContracts(t *testing.T) {
	t.Parallel()
	c, err := ParseCredential([]byte(`{"auth":{"accessToken":"a","refreshToken":"r","expiresAt":12345,"domain":"d"},"account":{"uid":"u","enterpriseId":"e","nickname":"n"}}`))
	if err != nil || c.UID != "u" || c.RefreshToken != "r" || c.ExpiresAt != 12345 {
		t.Fatalf("legacy import: %v", err)
	}
	for _, raw := range []string{`{}`, `{"access_token":"x\r\ny"}`, `{"access_token":"a","type":"codex"}`, `{"access_token":"a","base_url":"https://evil.example"}`, `{"access_token":"a"} {}`} {
		if _, err := ParseCredential([]byte(raw)); err == nil {
			t.Errorf("accepted invalid credential: %s", raw)
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		_, _ = fmt.Fprint(w, `{"secret":"private-refresh-token"}`)
	}))
	defer upstream.Close()
	s := NewService(upstream.Client())
	s.BaseURL = upstream.URL
	_, err = s.Refresh(context.Background(), c)
	var apiError *APIError
	if !errors.As(err, &apiError) || apiError.StatusCode() != 401 || strings.Contains(err.Error(), "private") {
		t.Fatalf("unsafe/incorrect error %v", err)
	}
	_, err = s.Refresh(context.Background(), &Credential{AccessToken: "a"})
	if !errors.Is(err, ErrCannotRefresh) {
		t.Fatalf("missing refresh: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.Start(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}
