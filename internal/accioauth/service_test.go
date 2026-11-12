package accioauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func assertAccioRequestHeaders(t *testing.T, r *http.Request, region, accept string) {
	t.Helper()
	os := runtime.GOOS
	if os == "darwin" {
		os = "macos"
	}
	for key, want := range map[string]string{
		"User-Agent": "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36", "Content-Type": "application/json",
		"Accept": accept, "Accept-Language": "en", "x-language": "en",
		"x-platform": "desktop", "x-os": os, "x-app-version": "0.34.4",
		"x-package-region": region, "Authorization": "",
	} {
		if got := r.Header.Get(key); got != want {
			t.Errorf("%s %s: header %s = %q, want %q", r.Method, r.URL.Path, key, got, want)
		}
	}
}

func TestOAuthAuthorizationAndExchange(t *testing.T) {
	for region, host := range map[string]string{"cn": "www.accio-ai.com", "global": "www.accio.com"} {
		t.Run(region, func(t *testing.T) {
			redirectURI := "http://localhost:14567/auth/callback-accio?trace=a%2Fb"
			link, err := AuthorizationLink(region, "state+/", "challenge-_", redirectURI)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(link)
			if err != nil {
				t.Fatal(err)
			}
			if u.Scheme != "https" || u.Host != host || u.Path != "/login" || !reflect.DeepEqual(u.Query(), url.Values{
				"return_url": {redirectURI}, "state": {"state+/"}, "code_challenge": {"challenge-_"},
				"code_challenge_method": {"S256"}, "client_id": {"accio-work"},
			}) {
				t.Fatalf("unexpected authorization URL: %s", link)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertAccioRequestHeaders(t, r, strings.ToUpper(region), "application/json")
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if r.Method != http.MethodPost || r.URL.Path != "/api/oauth/token" || r.URL.RawQuery != "" || r.Header.Get("x-package-region") != strings.ToUpper(region) || r.Header.Get("Authorization") != "" || !reflect.DeepEqual(body, map[string]string{
					"code": "code+/", "codeVerifier": "verifier-_", "clientId": "accio-work", "redirectUri": redirectURI,
				}) {
					t.Errorf("invalid token request: %s %#v", r.URL, body)
				}
				cookieKey := "sgcookie"
				if region == "global" {
					cookieKey = "sgCookie"
				}
				_, _ = fmt.Fprintf(w, `{"success":true,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresAt":1800000000000,%q:"session=login; companion=value"}}`, cookieKey)
			}))
			defer server.Close()
			s := NewService(server.Client())
			s.BaseURL = server.URL
			credential, err := s.ExchangeCode(context.Background(), region, "code+/", "verifier-_", redirectURI)
			if err != nil {
				t.Fatal(err)
			}
			if credential.Region != region || credential.AccessToken != "new-access" || credential.RefreshToken != "new-refresh" || credential.SGCookie != "session=login; companion=value" || credential.ExpiresAt != 1800000000 || credential.UserID != "" {
				t.Fatalf("unexpected credential: %#v", credential)
			}
		})
	}
	if _, err := AuthorizationLink("intl", "state", "challenge", "http://localhost:1234/auth/callback-accio"); err == nil {
		t.Fatal("accepted invalid region")
	}
}

func TestOAuthExchangeRejectsInvalidTokens(t *testing.T) {
	for _, body := range []string{`{"success":false,"code":"401"}`, `{"data":{}}`, `{"data":{"accessToken":"access","expiresAt":"invalid"}}`, `{"data":{"accessToken":"access","refreshToken":"bad token"}}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) }))
			defer server.Close()
			s := NewService(server.Client())
			s.BaseURL = server.URL
			if _, err := s.ExchangeCode(context.Background(), "global", "code", "verifier", "http://localhost:1234/auth/callback-accio"); err == nil {
				t.Fatal("accepted invalid token response")
			}
		})
	}
}

func TestCredentialImport(t *testing.T) {
	for _, raw := range []string{
		`{"region":"cn","access_token":"access","refresh_token":"refresh","expires_at":1800000000000,"user_id":"42","device_id":"desktop-test","sgcookie":"session=value; companion=value"}`,
		`{"region":"cn","accessToken":"access","refreshToken":"refresh","expiresAt":"1800000000000","userId":42,"utdid":"desktop-test","sgCookie":"session=value; companion=value"}`,
		`{"region":"cn","accessToken":"access","refreshToken":"refresh","expiresAt":"2027-01-15T08:00:00Z","userId":"42","deviceId":"desktop-test","sgcookie":"session=value; companion=value","sgCookie":"alias-is-not-selected"}`,
	} {
		c, err := ParseCredential([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if c.DeviceID != "desktop-test" || c.SGCookie != "session=value; companion=value" || c.Type != ChannelType || c.ExpiresAt != 1800000000 || c.UserID != "42" || c.AccessToken != "access" || c.RefreshToken != "refresh" {
			t.Fatalf("unexpected credential: %#v", c)
		}
		encoded, err := c.JSON()
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(encoded), &fields); err != nil {
			t.Fatal(err)
		}
		if fields["device_id"] != "desktop-test" || fields["sgcookie"] != "session=value; companion=value" || fields["access_token"] != "access" || fields["expires_at"] != float64(1800000000) {
			t.Fatalf("invalid canonical JSON: %s", encoded)
		}
		if _, found := fields["accessToken"]; found {
			t.Fatal("official field leaked into canonical format")
		}
		if _, found := fields["sgCookie"]; found {
			t.Fatal("cookie alias leaked into canonical format")
		}
	}
	for _, raw := range []string{`null`, `[]`, `{"accessToken":"access"}`, `{"region":"intl","accessToken":"access"}`, `{"region":"global","type":"qoder","accessToken":"access"}`, `{"region":"cn","accessToken":"a\nb"}`, `{"region":"cn","accessToken":"a","utdid":"bad\r\nheader"}`, `{"region":"cn","accessToken":"a","expiresAt":-1}`, `{"region":"cn","accessToken":"a","expiresAt":"broken"}`} {
		if _, err := ParseCredential([]byte(raw)); err == nil {
			t.Fatalf("accepted invalid credential %s", raw)
		}
	}
	c := &Credential{Region: "global", AccessToken: "access", ExpiresAt: 1800000000}
	if !c.NeedsRefresh(time.Unix(1799999940, 0)) || c.NeedsRefresh(time.Unix(1799999939, 0)) {
		t.Fatal("incorrect one-minute margin")
	}
	c.ExpiresAt = 0
	if c.NeedsRefresh(time.Now()) {
		t.Fatal("unknown expiry should not be fabricated")
	}
	encoded, err := c.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(encoded), &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["sgcookie"]; present {
		t.Fatal("empty sgcookie should be omitted")
	}
	if c.Type != "" {
		t.Fatal("JSON mutated receiver")
	}
}

func TestSGCookieValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		valid bool
	}{
		{"spaces", "session=cookie-secret; companion=value", true},
		{"maximum length", strings.Repeat("c", 4096), true},
		{"empty", "", true},
		{"null", nil, true},
		{"carriage return", "cookie-secret\r", false},
		{"line feed", "\ncookie-secret", false},
		{"tab", "cookie-secret\tvalue", false},
		{"nul", "cookie-secret\x00", false},
		{"del", "cookie-secret\x7f", false},
		{"non ASCII", "cookie-secret中文", false},
		{"too long", strings.Repeat("c", 4097), false},
		{"wrong type", map[string]string{"cookie-secret": "value"}, false},
	} {
		for _, key := range []string{"sgcookie", "sgCookie"} {
			t.Run(tc.name+"/"+key, func(t *testing.T) {
				fields := map[string]any{"region": "global", "accessToken": "access", key: tc.value}
				body, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
				defer server.Close()
				s := NewService(server.Client())
				s.BaseURL = server.URL
				for name, parse := range map[string]func() (*Credential, error){
					"import": func() (*Credential, error) { return ParseCredential(body) },
					"OAuth": func() (*Credential, error) {
						return s.ExchangeCode(context.Background(), "global", "code", "verifier", "http://localhost:1234/auth/callback-accio")
					},
					"refresh": func() (*Credential, error) {
						return s.Refresh(context.Background(), &Credential{Region: "global", AccessToken: "old", RefreshToken: "refresh"})
					},
				} {
					credential, err := parse()
					if !tc.valid {
						if err == nil || strings.Contains(err.Error(), "cookie-secret") {
							t.Fatalf("%s: expected a private validation error, got %v", name, err)
						}
						continue
					}
					if err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					want, _ := tc.value.(string)
					if credential.SGCookie != want {
						t.Errorf("%s: cookie was changed", name)
					}
				}
			})
		}
	}
}

func TestIdentityAndRefresh(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAccioRequestHeaders(t, r, "CN", "application/json")
		calls = append(calls, r.URL.Path)
		if r.Header.Get("x-package-region") != "CN" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected auth headers: %#v", r.Header)
		}
		switch r.URL.Path {
		case "/api/auth/userinfo":
			if r.Method != http.MethodPost {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/json" || !reflect.DeepEqual(body, map[string]string{"accessToken": "old+access"}) {
				t.Error("profile requires a JSON accessToken without query credentials")
			}
			_, _ = fmt.Fprint(w, `{"success":true,"data":{"user":{"id":42,"nickname":"Name","email":"test@example.com"}}}`)
		case "/api/auth/refresh_token":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if r.Method != http.MethodPost || !reflect.DeepEqual(body, map[string]string{"accessToken": "old+access", "refreshToken": "old-refresh"}) {
				t.Errorf("invalid refresh request: %#v", body)
			}
			_, _ = fmt.Fprint(w, `{"code":"200","data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresAt":1800000000000,"userId":"42","sgCookie":"session=rotated; companion=value"}}`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	s := NewService(server.Client())
	s.BaseURL = server.URL
	original := &Credential{Region: "cn", AccessToken: "old+access", RefreshToken: "old-refresh", DeviceID: "desktop-test", SGCookie: "session=original", OAuthUsage: "saved"}
	profile, err := s.CompleteIdentity(context.Background(), original)
	if err != nil {
		t.Fatal(err)
	}
	if profile.UserID != "42" || profile.Name != "Name" || profile.Email != "test@example.com" || profile.SGCookie != "session=original" {
		t.Fatalf("invalid profile: %#v", profile)
	}
	fresh, err := s.Refresh(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.DeviceID != "desktop-test" || fresh.SGCookie != "session=rotated; companion=value" || fresh.AccessToken != "new-access" || fresh.RefreshToken != "new-refresh" || fresh.ExpiresAt != 1800000000 || fresh.OAuthUsage != "saved" {
		t.Fatalf("invalid refreshed credential: %#v", fresh)
	}
	if original.UserID != "" || original.AccessToken != "old+access" || profile.AccessToken != "old+access" || profile.SGCookie != "session=original" {
		t.Fatal("input credential was mutated")
	}
	if len(calls) != 2 {
		t.Fatalf("unexpected requests: %v", calls)
	}
	if _, err := s.Refresh(context.Background(), &Credential{Region: "cn", AccessToken: "a"}); !errors.Is(err, ErrCannotRefresh) {
		t.Fatalf("expected ErrCannotRefresh, got %v", err)
	}
	profile.UserID = "other"
	if _, err := s.CompleteIdentity(context.Background(), profile); err == nil {
		t.Fatal("accepted mismatched identity")
	}
	if _, err := s.Refresh(context.Background(), profile); err == nil {
		t.Fatal("accepted mismatched refreshed identity")
	}
}

func TestRefreshOmittedFields(t *testing.T) {
	for name, body := range map[string]string{
		"omitted":        `{"data":{"accessToken":"new"}}`,
		"empty cookie":   `{"data":{"accessToken":"new","sgcookie":""}}`,
		"empty alias":    `{"data":{"accessToken":"new","sgCookie":""}}`,
		"null cookie":    `{"data":{"accessToken":"new","sgcookie":null}}`,
		"rotated cookie": `{"data":{"accessToken":"new","sgcookie":"session=rotated"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) }))
			defer server.Close()
			s := NewService(server.Client())
			s.BaseURL = server.URL
			c, err := s.Refresh(context.Background(), &Credential{Region: "global", AccessToken: "old", RefreshToken: "refresh", SGCookie: "session=original", ExpiresAt: 1})
			if err != nil {
				t.Fatal(err)
			}
			wantCookie := "session=original"
			if name == "rotated cookie" {
				wantCookie = "session=rotated"
			}
			if c.RefreshToken != "refresh" || c.SGCookie != wantCookie || c.ExpiresAt != 0 {
				t.Fatalf("omitted fields mishandled: %#v", c)
			}
		})
	}
}

func TestCatalogFiltering(t *testing.T) {
	for _, root := range []string{
		`[%s]`, `{"providers":[%s]}`,
	} {
		provider := `{"modelList":[{"modelCode":"opaque-1","modelName":" readable-model ","modelDisplayName":"Readable Name","protocol":"RESPONSES","reasoningEfforts":["low","high"],"contextWindow":100000,"multimodal":true},{"modelCode":"hidden","modelName":"hidden","visible":false},{"modelCode":"video","modelName":"video","visible":null,"supportedOperationList":["VIDEO_GENERATION"]},{"modelCode":"image","modelName":"image","supportedOperationList":["image_generation"]},{"modelCode":"audio","modelName":"audio","supportedOperationList":["AUDIO_GENERATION"]},{"modelCode":"OPAQUE-1","modelName":"readable-model"},{"modelCode":"code-only"},{"modelCode":"opaque-2","modelName":"readable-model"},{"modelName":"native-2","supportedOperationList":["CHAT"]},{}]}`
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assertAccioRequestHeaders(t, r, "GLOBAL", "application/json")
			if r.Method != http.MethodPost || r.URL.Path != "/api/llm/config" || r.Header.Get("x-package-region") != "GLOBAL" {
				t.Error("invalid model request")
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["token"] != "access" || body["supportAutoModel"] != true || len(body) != 2 {
				t.Errorf("invalid model payload: %#v", body)
			}
			_, _ = fmt.Fprintf(w, `{"success":true,"data":%s}`, fmt.Sprintf(root, provider))
		}))
		s := NewService(server.Client())
		s.BaseURL = server.URL
		models, err := s.FetchModels(context.Background(), &Credential{Region: "global", AccessToken: "access"})
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(models) != 2 || models[0].ID != "readable-model" || models[0].Name != "Readable Name" || models[0].Protocol != "responses" || !reflect.DeepEqual(models[0].Efforts, []string{"low", "high"}) || !models[0].SupportsImages || models[0].ContextWindow != 100000 || models[1].ID != "native-2" {
			t.Fatalf("incorrect catalog: %#v", models)
		}
	}
}

func TestUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertAccioRequestHeaders(t, r, "GLOBAL", "application/json")
		if r.Method != http.MethodGet || r.URL.Query().Get("accessToken") != "access" {
			t.Error("invalid usage request")
		}
		switch r.URL.Path {
		case "/api/entitlement/quota":
			_, _ = fmt.Fprint(w, `{"data":{"usagePercent":37.5,"refreshCountdownSeconds":1234,"accessToken":"never-expose"}}`)
		case "/api/entitlement/currentSubscription":
			_, _ = fmt.Fprint(w, `{"data":{"planName":"Pro","status":"active","expireAt":"2027-01-15T08:00:00Z","remainQuota":50,"totalQuota":100,"unit":"credits","accessToken":"never-expose","nested":{"token":"never-expose"}}}`)
		default:
			t.Errorf("unexpected usage path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	s := NewService(server.Client())
	s.BaseURL = server.URL
	usage, err := s.FetchUsage(context.Background(), &Credential{Region: "global", AccessToken: "access"})
	if err != nil {
		t.Fatal(err)
	}
	if usage.UsagePercent != 37.5 || usage.RemainingPercent != 62.5 || usage.Unit != "%" || usage.RefreshCountdownSeconds == nil || *usage.RefreshCountdownSeconds != 1234 {
		t.Fatalf("bad quota: %#v", usage)
	}
	if usage.Subscription == nil || usage.Subscription.PlanName != "Pro" || usage.Subscription.Unit != "credits" || *usage.Subscription.RemainingQuota != 50 || *usage.Subscription.TotalQuota != 100 {
		t.Fatalf("bad subscription: %#v", usage.Subscription)
	}
	raw, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "never-expose") || strings.Contains(string(raw), "USD") {
		t.Fatalf("unexpected usage payload: %s", raw)
	}
}

func TestUsageMissingSubscriptionAndInvalidQuota(t *testing.T) {
	for _, quota := range []string{`{"usagePercent":0}`, `{}`, `{"usagePercent":101}`, `{"usagePercent":-1}`, `{"usagePercent":1,"refreshCountdownSeconds":-1}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "quota") {
				_, _ = fmt.Fprint(w, quota)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		s := NewService(server.Client())
		s.BaseURL = server.URL
		usage, err := s.FetchUsage(context.Background(), &Credential{Region: "global", AccessToken: "a"})
		server.Close()
		if quota == `{"usagePercent":0}` {
			if err != nil || usage.Subscription != nil || usage.RemainingPercent != 100 {
				t.Fatalf("free account quota failed: %#v %v", usage, err)
			}
		} else if err == nil {
			t.Fatalf("accepted invalid quota: %s", quota)
		}
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("transport URL %s", r.URL.String())
}

func TestErrorPrivacy(t *testing.T) {
	for _, tc := range []struct {
		status     int
		body       string
		wantStatus int
	}{
		{401, `{"message":"access-secret refresh-secret https://host/?accessToken=access-secret"}`, 401},
		{200, `{"success":false,"code":"429","message":"access-secret"}`, 429},
		{200, `{"success":false,"code":403}`, 401},
		{403, `{"message":"access-secret"}`, 401},
		{200, `{"success":false,"code":"423","message":"user blocked access-secret"}`, 502},
		{200, `{"success":false,"code":"access-secret"}`, 502},
		{200, `{"code":"500","message":"access-secret"}`, 502},
		{200, `not JSON access-secret`, 0},
		{302, `access-secret`, 302},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://invalid.example/?accessToken=access-secret")
			w.WriteHeader(tc.status)
			_, _ = fmt.Fprint(w, tc.body)
		}))
		s := NewService(server.Client())
		s.BaseURL = server.URL
		_, err := s.CompleteIdentity(context.Background(), &Credential{Region: "global", AccessToken: "access-secret", RefreshToken: "refresh-secret"})
		server.Close()
		if err == nil {
			t.Fatal("expected upstream failure")
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "https://") || strings.Contains(err.Error(), "accessToken=") {
			t.Fatalf("error leaked credentials: %v", err)
		}
		if tc.status == 200 && strings.Contains(tc.body, `"423"`) {
			if !strings.Contains(err.Error(), "POST /api/auth/userinfo (HTTP 200, business code 423)") || strings.Contains(err.Error(), "HTTP 502") {
				t.Fatalf("lost upstream error context: %v", err)
			}
		}
		if tc.wantStatus != 0 {
			var upstream *HTTPError
			if !errors.As(err, &upstream) || upstream.StatusCode != tc.wantStatus {
				t.Fatalf("wrong HTTP error: %v", err)
			}
		}
	}
	s := NewService(&http.Client{Transport: failingTransport{}})
	_, err := s.CompleteIdentity(context.Background(), &Credential{Region: "global", AccessToken: "access-secret"})
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "https://") {
		t.Fatalf("transport error exposed URL: %v", err)
	}
}

func TestEmptyCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"success":true,"data":[]}`)
	}))
	defer server.Close()
	s := NewService(server.Client())
	s.BaseURL = server.URL
	if _, err := s.FetchModels(context.Background(), &Credential{Region: "global", AccessToken: "access"}); err == nil {
		t.Fatal("empty catalog should fail")
	}
}

func TestOptionalSubscriptionFailure(t *testing.T) {
	for _, status := range []int{401, 403, 404, 500} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "quota") {
				_, _ = fmt.Fprint(w, `{"usagePercent":20}`)
			} else {
				w.WriteHeader(status)
			}
		}))
		s := NewService(server.Client())
		s.BaseURL = server.URL
		usage, err := s.FetchUsage(context.Background(), &Credential{Region: "global", AccessToken: "access"})
		server.Close()
		if status == 401 || status == 403 {
			var upstream *HTTPError
			if !errors.As(err, &upstream) || upstream.StatusCode != 401 {
				t.Fatalf("subscription auth rejection must refresh: %v", err)
			}
		} else if err != nil || usage == nil || usage.UsagePercent != 20 || usage.Subscription != nil {
			t.Fatalf("lost valid quota: %#v %v", usage, err)
		}
	}
}
