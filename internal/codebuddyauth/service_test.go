package codebuddyauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

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
	for _, raw := range []string{`{}`, `{"access_token":"x\r\ny"}`, `{"access_token":"a","type":"codex"}`, `{"access_token":"a"} {}`} {
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
