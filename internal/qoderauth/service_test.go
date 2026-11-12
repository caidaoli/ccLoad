package qoderauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func authTestService(t *testing.T, handler http.HandlerFunc) *Service {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	service := NewService(server.Client())
	service.BaseURL = server.URL
	return service
}

func authTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

/**
 * @brief TestCredentialValidationAndRouting covers canonical import and regional hosts.
 */
func TestCredentialValidationAndRouting(t *testing.T) {
	for _, region := range []string{"", "intl", "https://evil.invalid", "CN"} {
		if _, err := ParseCredential([]byte(fmt.Sprintf(`{"region":%q,"access_token":"dt-secret"}`, region))); err == nil {
			t.Errorf("accepted region %q", region)
		}
	}
	for _, raw := range []string{`null`, `{"region":"cn"}`, `{"type":"codex","region":"cn","access_token":"dt-x"}`, `{"region":"cn","access_token":"dt-x|uid|mid"}`, `{"region":"cn","access_token":"dt-x\r\ny"}`} {
		if _, err := ParseCredential([]byte(raw)); err == nil {
			t.Errorf("accepted invalid credential %s", raw)
		}
	}
	c, err := ParseCredential([]byte(`{"region":"global","access_token":"jt-secret","refresh_token":"jrt-secret","expires_at":1800000000000,"auto_checkin":true,"last_checkin_at":1700000000}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Type != ChannelType || c.ExpiresAt != 1800000000 || c.GatewayURL() != "https://api3.qoder.sh" || c.OpenAPIURL() != "https://openapi.qoder.sh" {
		t.Fatalf("incorrect normalized credential: %+v", c)
	}
	raw, err := c.JSON()
	if err != nil {
		t.Fatal(err)
	}
	roundtrip, err := ParseCredential([]byte(raw))
	if err != nil || *roundtrip != *c {
		t.Fatalf("round trip failed: %v", err)
	}
	if !c.NeedsRefresh(time.Unix(1800000000-60, 0)) || c.NeedsRefresh(time.Unix(1800000000-61, 0)) {
		t.Fatal("refresh margin incorrect")
	}
	c.ExpiresAt = 0
	if c.NeedsRefresh(time.Now()) {
		t.Fatal("unknown expiry should not force refresh")
	}
	c.AccessToken = "dt-secret"
	if c.GatewayURL() != "https://api3.qoder.sh" {
		t.Fatal("device token routing incorrect")
	}
	c.Region = "cn"
	if c.GatewayURL() != "https://gateway.qoder.com.cn" || c.OpenAPIURL() != "https://openapi.qoder.com.cn" {
		t.Fatal("CN routing incorrect")
	}
}

func TestExchangePATAndIdentity(t *testing.T) {
	service := authTestService(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/jobToken/exchange":
			if r.Method != http.MethodPost || r.Header.Get("Authorization") != "" {
				t.Error("invalid exchange request")
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["personal_token"] != "pat-secret" {
				t.Errorf("invalid exchange body: %v", err)
			}
			authTestJSON(t, w, map[string]any{"token": "jt-new", "refresh_token": "jrt-new", "expires_at": 1800000000000})
		case "/api/v1/userinfo":
			if r.Header.Get("Authorization") != "Bearer jt-new" {
				t.Error("missing profile bearer")
			}
			authTestJSON(t, w, map[string]string{"id": "user", "name": "name", "email": "test@example.com"})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	c, err := service.ExchangePAT(context.Background(), "cn", "pat-secret")
	if err != nil {
		t.Fatal(err)
	}
	if c.PAT != "pat-secret" || c.RefreshToken != "jrt-new" || c.UserID != "user" || c.MachineID == "" || c.ExpiresAt != 1800000000 {
		t.Fatalf("incomplete exchanged identity: %+v", c)
	}
	c.UserID = "another-user"
	if _, err := service.CompleteIdentity(context.Background(), c); err == nil {
		t.Fatal("accepted mismatched token owner")
	}
}

func TestRefreshTokenFamiliesAndIdentity(t *testing.T) {
	for _, family := range []string{"device", "job", "device refresh with job access"} {
		t.Run(family, func(t *testing.T) {
			c := &Credential{Region: "cn", AccessToken: "dt-old", RefreshToken: "drt-old", UserID: "user", MachineID: "machine", ExpiresAt: 1, OAuthUsage: `{"remaining":9}`, LastCheckinAt: 12, LastCheckinCampaign: "campaign"}
			path := "/api/v1/deviceToken/refresh"
			token := "dt-new"
			refresh := "drt-new"
			switch family {
			case "job":
				c.AccessToken, c.RefreshToken = "jt-old", "jrt-old"
				path, token, refresh = "/api/v1/jobToken/refresh", "jt-new", "jrt-new"
			case "device refresh with job access":
				c.AccessToken = "jt-old"
			}
			original := *c
			service := authTestService(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != path || r.Method != http.MethodPost || r.Header.Get("Authorization") != "" || r.Header.Get("Cosy-Signature") != "" {
					t.Errorf("bad refresh request: %s %s", r.Method, r.URL.Path)
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["refresh_token"] != c.RefreshToken {
					t.Errorf("invalid refresh body: %v", err)
				}
				authTestJSON(t, w, map[string]any{"device_token": token, "refresh_token": refresh, "user_id": "user", "expires_at": 1800000000})
			})
			fresh, err := service.Refresh(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			if *c != original {
				t.Fatal("refresh mutated input")
			}
			if fresh.AccessToken != token || fresh.RefreshToken != refresh || fresh.ExpiresAt != 1800000000 || fresh.LastCheckinAt != 12 || fresh.OAuthUsage != c.OAuthUsage {
				t.Fatalf("refresh dropped credential fields: %+v", fresh)
			}
		})
	}
}

func TestRefreshRejectsInvalidResults(t *testing.T) {
	for _, response := range []string{`{}`, `{"token":"jt-new","user_id":"different"}`, `{"token":"jt-new","refresh_token":"bad|packed"}`} {
		t.Run(response, func(t *testing.T) {
			service := authTestService(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(response)) })
			c := &Credential{Region: "cn", AccessToken: "jt-old", RefreshToken: "jrt-old", UserID: "user"}
			original := *c
			if _, err := service.Refresh(context.Background(), c); err == nil {
				t.Fatal("accepted invalid refresh response")
			}
			if *c != original {
				t.Fatal("mutated credential on failure")
			}
		})
	}
	service := NewService(nil)
	if _, err := service.Refresh(context.Background(), &Credential{Region: "cn", AccessToken: "dt-token"}); !errors.Is(err, ErrCannotRefresh) {
		t.Fatalf("wrong unrefreshable error: %v", err)
	}
}

func TestRefreshMissingRotationRetainsRefreshToken(t *testing.T) {
	service := authTestService(t, func(w http.ResponseWriter, _ *http.Request) { authTestJSON(t, w, map[string]string{"token": "dt-new"}) })
	c, err := service.Refresh(context.Background(), &Credential{Region: "cn", AccessToken: "dt-old", RefreshToken: "drt-old", UserID: "user", ExpiresAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if c.RefreshToken != "drt-old" || c.ExpiresAt != 0 {
		t.Fatalf("incorrect fallback state: %+v", c)
	}
}

/**
 * @brief TestRefreshPATReexchange covers rejected job refresh recovery and owner isolation.
 */
func TestRefreshPATReexchange(t *testing.T) {
	for _, tc := range []struct {
		name           string
		refresh        string
		pat            string
		status         int
		owner          string
		exchangeStatus int
		wantCalls      int
		wantError      bool
	}{
		{"unauthorized", "jrt-old", "pat-secret", 401, "user", 200, 3, false},
		{"forbidden", "jrt-old", "pat-secret", 403, "user", 200, 3, false},
		{"no refresh token", "", "pat-secret", 0, "user", 200, 2, false},
		{"different owner", "jrt-old", "pat-secret", 401, "other-user", 200, 3, true},
		{"expired PAT", "jrt-old", "pat-secret", 401, "user", 403, 2, true},
		{"no PAT", "jrt-old", "", 401, "user", 200, 1, true},
		{"device rejection", "drt-old", "pat-secret", 401, "user", 200, 1, true},
		{"server error", "jrt-old", "pat-secret", 500, "user", 200, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Credential{Region: "global", AccessToken: "jt-old", RefreshToken: tc.refresh, PAT: tc.pat, UserID: "user", MachineID: "machine", Name: "name", Email: "old@example.com", ExpiresAt: 1, OAuthUsage: `{"remaining":9}`, LastCheckinAt: 12, LastCheckinCampaign: "campaign"}
			original := *c
			calls := 0
			service := authTestService(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				switch r.URL.Path {
				case "/api/v1/jobToken/refresh", "/api/v1/deviceToken/refresh":
					w.WriteHeader(tc.status)
				case "/api/v1/jobToken/exchange":
					var body map[string]string
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["personal_token"] != tc.pat || r.Header.Get("Authorization") != "" {
						t.Errorf("invalid PAT exchange request: %v", err)
					}
					w.WriteHeader(tc.exchangeStatus)
					authTestJSON(t, w, map[string]any{"token": "jt-new", "refresh_token": "jrt-new", "expires_at": 1800000000})
				case "/api/v1/userinfo":
					if r.Header.Get("Authorization") != "Bearer jt-new" {
						t.Error("identity did not use the new token")
					}
					authTestJSON(t, w, map[string]string{"id": tc.owner})
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			})
			fresh, err := service.Refresh(t.Context(), c)
			if (err != nil) != tc.wantError || calls != tc.wantCalls {
				t.Fatalf("refresh error=%v calls=%d", err, calls)
			}
			if *c != original {
				t.Fatal("refresh mutated its input")
			}
			if tc.wantError {
				return
			}
			want := original
			want.Type = ChannelType
			want.AccessToken, want.RefreshToken, want.ExpiresAt = "jt-new", "jrt-new", 1800000000
			if *fresh != want {
				t.Fatalf("PAT recovery dropped credential metadata: %+v", fresh)
			}
		})
	}
}

/**
 * @brief TestTokenExpiryFormats verifies upstream dates, epochs and lifetime units.
 */
func TestTokenExpiryFormats(t *testing.T) {
	for _, tc := range []struct {
		name     string
		flow     string
		response string
		want     int64
		relative bool
	}{
		{"date", "job", `{"expires_at":"2027-01-15T08:00:00.123Z"}`, 1800000000, false},
		{"epoch seconds string", "job", `{"expire_time":"1800000000"}`, 1800000000, false},
		{"epoch milliseconds string", "job", `{"expiresAt":"1800000000000"}`, 1800000000, false},
		{"absolute precedes lifetime", "job", `{"expireTime":1800000000000,"expires_in":1}`, 1800000000, false},
		{"past absolute remains past", "job", `{"expire_time":1,"expires_in":3600000}`, 1, false},
		{"job milliseconds", "job", `{"expires_in":3600000}`, 3600, true},
		{"device seconds", "device", `{"expires_in":"3600"}`, 3600, true},
		{"PAT milliseconds and token alias", "pat", `{"expires_in":3600000,"device_token":"jt-new"}`, 3600, true},
		{"PAT access alias", "pat", `{"expires_at":"1800000000","access_token":"jt-new"}`, 1800000000, false},
		{"login seconds", "login", `{"expires_in":3600}`, 3600, true},
		{"invalid absolute uses lifetime", "device", `{"expires_at":"invalid","expires_in":3600}`, 3600, true},
		{"unknown", "job", `{}`, 0, false},
		{"invalid", "job", `{"expires_at":1e100,"expires_in":-1}`, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var response map[string]any
			if err := json.Unmarshal([]byte(tc.response), &response); err != nil {
				t.Fatal(err)
			}
			if _, device := response["device_token"]; !device {
				if _, access := response["access_token"]; !access {
					response["token"] = "token-new"
				}
			}
			service := authTestService(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/userinfo" {
					authTestJSON(t, w, map[string]string{"id": "user"})
					return
				}
				authTestJSON(t, w, response)
			})
			before := time.Now().Unix()
			var fresh *Credential
			var err error
			switch tc.flow {
			case "pat":
				fresh, err = service.ExchangePAT(t.Context(), "global", "pat-secret")
			case "login":
				var session *LoginSession
				session, err = service.StartLogin("global")
				if err == nil {
					fresh, err = service.PollLogin(t.Context(), session)
				}
			default:
				refresh := "jrt-old"
				if tc.flow == "device" {
					refresh = "drt-old"
				}
				fresh, err = service.Refresh(t.Context(), &Credential{Region: "global", AccessToken: "old", RefreshToken: refresh, UserID: "user", ExpiresAt: 1})
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.relative {
				if fresh.ExpiresAt < before+tc.want || fresh.ExpiresAt > time.Now().Unix()+tc.want {
					t.Fatalf("expiry=%d, want now+%d seconds", fresh.ExpiresAt, tc.want)
				}
			} else if fresh.ExpiresAt != tc.want {
				t.Fatalf("expiry=%d, want %d", fresh.ExpiresAt, tc.want)
			}
		})
	}
}

/**
 * @brief TestLoginPKCEAndPending checks edition client IDs and public PKCE fields.
 */
func TestLoginPKCEAndPending(t *testing.T) {
	session, err := StartLogin("global")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(session.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	nonce, err := uuid.Parse(query.Get("nonce"))
	if err != nil || nonce.String() != query.Get("nonce") {
		t.Fatal("authorization nonce must be a canonical UUID")
	}
	digest := sha256.Sum256([]byte(session.verifier))
	if parsed.Host != "qoder.com" || parsed.Path != "/device/selectAccounts" || query.Get("client_id") != "732aef47-9cf2-46a2-95fe-4cebb5d0d1fa" || query.Get("challenge_method") != "S256" || query.Get("challenge") != base64.RawURLEncoding.EncodeToString(digest[:]) || query.Get("nonce") == "" || strings.Contains(session.AuthURL, session.verifier) {
		t.Fatal("invalid public PKCE authorization URL")
	}
	cn, err := StartLogin("cn")
	if err != nil {
		t.Fatal(err)
	}
	cnURL, err := url.Parse(cn.AuthURL)
	if err != nil || cnURL.Host != "qoder.cn" || cnURL.Query().Get("client_id") != "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb" {
		t.Fatalf("invalid CN authorization URL: %s %v", cn.AuthURL, err)
	}
	encoded, err := json.Marshal(session)
	if err != nil || strings.Contains(string(encoded), session.verifier) {
		t.Fatal("session serialization leaked verifier")
	}
	calls := 0
	service := authTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/userinfo" {
			authTestJSON(t, w, map[string]string{"id": "user"})
			return
		}
		if r.URL.Query().Get("verifier") != session.verifier || r.URL.Query().Get("nonce") != query.Get("nonce") {
			t.Error("invalid polling proof")
		}
		calls++
		switch calls {
		case 1:
			w.WriteHeader(http.StatusAccepted)
		case 2:
			w.WriteHeader(http.StatusNotFound)
		default:
			authTestJSON(t, w, map[string]any{"token": "dt-token", "refresh_token": "drt-token", "user_id": "user", "expires_at": 1800000000})
		}
	})
	for range 2 {
		if _, err := service.PollLogin(context.Background(), session); !errors.Is(err, ErrLoginPending) {
			t.Fatalf("wrong pending result: %v", err)
		}
	}
	c, err := service.PollLogin(context.Background(), session)
	if err != nil || c.MachineID != session.machineID || c.Region != "global" {
		t.Fatalf("login failed: %v", err)
	}
	session.expiresAt = time.Now().Add(-time.Second)
	if _, err := service.PollLogin(context.Background(), session); err == nil || calls != 3 {
		t.Fatal("expired session reached upstream")
	}
}

func TestLoginDeviceTokenResponse(t *testing.T) {
	session, err := StartLogin("cn")
	if err != nil {
		t.Fatal(err)
	}
	service := authTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/deviceToken/poll" {
			authTestJSON(t, w, map[string]string{"device_token": "dt-token", "refresh_token": "drt-token", "user_id": "user"})
			return
		}
		authTestJSON(t, w, map[string]string{"id": "user"})
	})
	credential, err := service.PollLogin(context.Background(), session)
	if err != nil || credential.AccessToken != "dt-token" || credential.RefreshToken != "drt-token" {
		t.Fatalf("device-token login failed: %+v %v", credential, err)
	}
}

func TestFetchUsageAggregatesAvailableBuckets(t *testing.T) {
	service := authTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/user/plan" {
			authTestJSON(t, w, map[string]string{"plan_tier_name": "Pro Trial"})
			return
		}
		authTestJSON(t, w, map[string]any{
			"userQuota":          map[string]any{"total": 300, "used": 75.25, "remaining": "224.75", "unit": "credits"},
			"addOnQuota":         map[string]any{"total": 100, "remaining": 99.5, "unit": "credits"},
			"orgResourcePackage": map[string]any{"cap": 999, "remaining": 999, "available": false, "unit": "credits"},
		})
	})
	usage, err := service.FetchUsage(context.Background(), &Credential{Region: "cn", AccessToken: "dt-token"})
	if err != nil {
		t.Fatal(err)
	}
	if usage.Total != 400 || usage.Remaining != 324.25 || usage.Used != 75.75 || usage.Plan != "Pro Trial" || usage.Unit != "credits" || len(usage.Buckets) != 2 {
		t.Fatalf("incorrect usage: %+v", usage)
	}
}

func TestFetchUsageOrgCapAndUnavailablePlan(t *testing.T) {
	service := authTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/user/plan" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		authTestJSON(t, w, map[string]any{"orgResourcePackage": map[string]any{"cap": 50, "used": 12.5, "remaining": 37.5, "available": true, "unit": "credits"}})
	})
	usage, err := service.FetchUsage(context.Background(), &Credential{Region: "global", AccessToken: "dt-token"})
	if err != nil || usage.Total != 50 || usage.Used != 12.5 || usage.Remaining != 37.5 || usage.Plan != "" {
		t.Fatalf("org quota failed: %+v %v", usage, err)
	}
}

func TestFetchUsageRejectsUnknownAndMixedUnits(t *testing.T) {
	for _, response := range []string{`{}`, `{"userQuota":{"total":999}}`, `{"userQuota":{"remaining":-1}}`, `{"userQuota":{"remaining":1,"unit":"credits"},"addOnQuota":{"remaining":2,"unit":"USD"}}`} {
		service := authTestService(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(response)) })
		if _, err := service.FetchUsage(context.Background(), &Credential{Region: "cn", AccessToken: "dt-token"}); err == nil {
			t.Errorf("accepted invalid usage %s", response)
		}
	}
}

func TestCheckinClaimContract(t *testing.T) {
	getCalls := 0
	service := authTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dt-token" || r.Header.Get("Cosy-ClientType") != "10" || r.Header.Get("Cosy-Signature") != "" {
			t.Error("invalid sash headers")
		}
		if r.Method == http.MethodGet {
			getCalls++
			if r.URL.Query().Get("forceRefresh") != "true" {
				t.Error("missing forceRefresh")
			}
			authTestJSON(t, w, map[string]any{"campaigns": []map[string]string{
				{"campaignId": "promo", "actionType": "VIEW_DETAILS", "claimStatus": "CLAIMABLE"},
				{"campaignId": "daily", "actionType": "CLAIM_BENEFIT", "claimStatus": "CLAIMABLE"},
			}})
			return
		}
		if r.URL.Path != authCampaignPath+"/daily/claim" {
			t.Errorf("claimed wrong campaign: %s", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 0 {
			t.Errorf("invalid claim body: %v", err)
		}
		authTestJSON(t, w, map[string]any{"status": "CLAIMED", "benefit": map[string]any{"kind": "CREDITS", "amount": 12.5}})
	})
	result, err := service.Checkin(context.Background(), &Credential{Region: "cn", AccessToken: "dt-token"})
	if err != nil || result.Status != "claimed" || result.CampaignID != "daily" || result.Reward != 12.5 || result.Unit != "credits" || getCalls != 1 {
		t.Fatalf("claim failed: %+v %v", result, err)
	}
}

func TestCheckinUncertainResults(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		response  string
		confirmed bool
		want      string
	}{
		{"unauthorized replay", 401, `{"result":"ALREADY_CLAIMED"}`, false, "error"},
		{"ordinary conflict", 409, `{"result":"CONFLICT"}`, false, "error"},
		{"explicit replay", 409, `{"result":"ALREADY_CLAIMED"}`, false, "already_claimed"},
		{"confirmed after error", 503, `{}`, true, "already_claimed"},
		{"ambiguous success", 200, `{"status":"PENDING"}`, false, "error"},
		{"missing reward", 200, `{"status":"CLAIMED"}`, false, "claimed"},
		{"replayed reward", 200, `{"status":"CLAIMED","replayed":true,"benefit":{"amount":100}}`, false, "already_claimed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lists := 0
			service := authTestService(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.response))
					return
				}
				lists++
				status := "CLAIMABLE"
				if lists > 1 && tc.confirmed {
					status = "CLAIMED"
				}
				authTestJSON(t, w, map[string]any{"campaigns": []map[string]string{{"campaignId": "daily", "actionType": "CLAIM_BENEFIT", "claimStatus": status}}})
			})
			result, err := service.Checkin(context.Background(), &Credential{Region: "cn", AccessToken: "dt-token"})
			if tc.want == "error" {
				if err == nil {
					t.Fatalf("accepted ambiguous claim: %+v", result)
				}
				return
			}
			if err != nil || result.Status != tc.want || result.Reward != 0 {
				t.Fatalf("unexpected claim outcome: %+v %v", result, err)
			}
		})
	}
}

func TestCheckinUnavailableAndAlreadyClaimed(t *testing.T) {
	for _, status := range []string{"CLAIMED", "DISABLED"} {
		service := authTestService(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Error("unnecessary claim")
			}
			authTestJSON(t, w, map[string]any{"campaigns": []map[string]string{{"campaignId": "daily", "actionType": "CLAIM_BENEFIT", "claimStatus": status}}})
		})
		result, err := service.Checkin(context.Background(), &Credential{Region: "cn", AccessToken: "dt-token"})
		expected := "unavailable"
		if status == "CLAIMED" {
			expected = "already_claimed"
		}
		if err != nil || result.Status != expected {
			t.Fatalf("wrong campaign state: %+v %v", result, err)
		}
	}
}

func TestInternationalCheckinRiskDependency(t *testing.T) {
	service := authTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			authTestJSON(t, w, map[string]string{"status": "CLAIMED"})
			return
		}
		campaigns := []map[string]string{}
		if r.Header.Get("Cosy-MachineToken") == "risk-token" {
			if r.Header.Get("Cosy-MachineCode") != "risk-code" || r.Header.Get("Cosy-MachineType") != "risk-type" {
				t.Error("incomplete risk identity")
			}
			campaigns = append(campaigns, map[string]string{"campaignId": "daily", "actionType": "CLAIM_BENEFIT", "claimStatus": "CLAIMABLE"})
		}
		authTestJSON(t, w, map[string]any{"uid": "sash-uid", "campaigns": campaigns})
	})
	credential := &Credential{Region: "global", AccessToken: "dt-token", UserID: "profile-uid"}
	result, err := service.Checkin(context.Background(), credential)
	if err != nil || result.Status != "missing_component" {
		t.Fatalf("missing component falsely succeeded: %+v %v", result, err)
	}
	service.RiskHeaders = func(_ context.Context, c *Credential) (http.Header, error) {
		if c.UserID != "sash-uid" {
			t.Errorf("wrong machine risk UID %q", c.UserID)
		}
		return http.Header{"Cosy-Machinetoken": {"risk-token"}, "Cosy-Machinecode": {"risk-code"}, "Cosy-Machinetype": {"risk-type"}, "Authorization": {"Bearer malicious"}}, nil
	}
	result, err = service.Checkin(context.Background(), credential)
	if err != nil || result.Status != "claimed" {
		t.Fatalf("risk-enabled claim failed: %+v %v", result, err)
	}
	if credential.UserID != "profile-uid" {
		t.Fatal("sash UID overwrote persistent account identity")
	}
	service.RiskHeaders = func(context.Context, *Credential) (http.Header, error) { return http.Header{}, nil }
	result, err = service.Checkin(context.Background(), credential)
	if err != nil || result.Status != "missing_component" {
		t.Fatalf("incomplete risk identity was accepted: %+v %v", result, err)
	}
}

func TestControlPlaneErrorsDoNotExposeTokens(t *testing.T) {
	service := authTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"message":"secret-pat secret-access secret-refresh"}`))
	})
	_, err := service.ExchangePAT(context.Background(), "cn", "secret-pat")
	var upstream *HTTPError
	if !errors.As(err, &upstream) || upstream.StatusCode != 401 || strings.Contains(err.Error(), "secret-") {
		t.Fatalf("unsafe or untyped error: %v", err)
	}
	session, err := StartLogin("cn")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = service.PollLogin(ctx, session)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), session.verifier) {
		t.Fatalf("invalid cancellation error: %v", err)
	}
}
