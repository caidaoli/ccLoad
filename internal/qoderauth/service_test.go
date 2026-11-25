package qoderauth

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
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

/**
 * @brief Verifies token rotation and account identity across device and job refresh endpoints.
 * @param t Test context.
 */
func TestRefreshTokenFamiliesAndIdentity(t *testing.T) {
	const owner = "9007199254740993"
	for _, tc := range []struct {
		family        string
		tokenKey      string
		ownerKey      string
		originalOwner string
	}{
		{"device", "device_token", "uid", owner},
		{"job", "accessToken", "userId", owner},
		{"device refresh with job access", "deviceToken", "user_id", owner},
		{"device credential without owner", "accessToken", "uid", ""},
	} {
		t.Run(tc.family, func(t *testing.T) {
			c := &Credential{Region: "cn", AccessToken: "dt-old", RefreshToken: "drt-old", UserID: tc.originalOwner, MachineID: "machine", ExpiresAt: 1, OAuthUsage: `{"remaining":9}`, LastCheckinAt: 12, LastCheckinCampaign: "campaign"}
			path := "/api/v1/deviceToken/refresh"
			token := "dt-new"
			refresh := "drt-new"
			switch tc.family {
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
				response := map[string]any{
					tc.tokenKey: token, tc.ownerKey: json.Number(owner),
					"refresh_token": refresh, "expires_at": 1800000000,
				}
				if tc.tokenKey != "accessToken" {
					response["accessToken"] = "ignored"
				}
				authTestJSON(t, w, response)
			})
			fresh, err := service.Refresh(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			if *c != original {
				t.Fatal("refresh mutated input")
			}
			want := original
			want.Type = ChannelType
			want.UserID = owner
			want.AccessToken, want.RefreshToken, want.ExpiresAt = token, refresh, 1800000000
			if *fresh != want {
				t.Fatalf("refresh dropped credential fields: %+v", fresh)
			}
		})
	}
}

/**
 * @brief Ensures failed refresh responses cannot replace the original credential.
 * @param t Test context.
 */
func TestRefreshRejectsInvalidResults(t *testing.T) {
	for _, response := range []string{
		`{}`,
		`{"token":"jt-new","user_id":"different"}`,
		`{"token":"jt-new","uid":9007199254740992}`,
		`{"token":"jt-new","userId":9007199254740992,"uid":"9007199254740993"}`,
		`{"token":"jt-new","user_id":9007199254740992,"userId":"9007199254740993"}`,
		`{"token":"jt-new","refresh_token":"bad|packed"}`,
	} {
		t.Run(response, func(t *testing.T) {
			service := authTestService(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(response)) })
			c := &Credential{Region: "cn", AccessToken: "jt-old", RefreshToken: "jrt-old", UserID: "9007199254740993"}
			original := *c
			if fresh, err := service.Refresh(context.Background(), c); fresh != nil || err == nil {
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

/**
 * @brief Verifies refresh responses without rotation, identity, or expiry fields.
 * @param t Test context.
 */
func TestRefreshMissingRotationRetainsRefreshToken(t *testing.T) {
	service := authTestService(t, func(w http.ResponseWriter, _ *http.Request) { authTestJSON(t, w, map[string]string{"token": "dt-new"}) })
	c, err := service.Refresh(context.Background(), &Credential{Region: "cn", AccessToken: "dt-old", RefreshToken: "drt-old", UserID: "user", ExpiresAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessToken != "dt-new" || c.RefreshToken != "drt-old" || c.UserID != "user" || c.ExpiresAt != 0 {
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
		{"device refresh milliseconds", "device", `{"expires_in":2591999994}`, 2591999, true},
		{"device refresh absolute precedes milliseconds", "device", `{"expires_at":1800000000,"expires_in":2591999994}`, 1800000000, false},
		{"device refresh max seconds", "device", `{"expires_in":2592000}`, 2592000, true},
		{"device refresh max milliseconds", "device", `{"expires_in":"2592000000"}`, 2592000, true},
		{"device refresh unsupported lifetime", "device", `{"expires_in":2592000001}`, 0, false},
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

/**
 * @brief Verifies device poll aliases against a userinfo response with an independent account ID.
 * @param t Test context.
 */
func TestLoginDeviceTokenResponse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
	}{
		{"device_token", `{"device_token":"dt-token","refresh_token":"drt-token","user_id":"9007199254740993"}`},
		{"access_token", `{"access_token":"dt-token","refreshToken":"drt-token","uid":9007199254740993}`},
		{"accessToken", `{"accessToken":"dt-token","refreshToken":"drt-token","userId":"9007199254740993"}`},
		{"deviceToken", `{"deviceToken":"dt-token","refreshToken":"drt-token"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, err := StartLogin("cn")
			if err != nil {
				t.Fatal(err)
			}
			service := authTestService(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/deviceToken/poll":
					_, _ = io.WriteString(w, tc.response)
				case "/api/v1/userinfo":
					if r.Header.Get("Authorization") != "Bearer dt-token" {
						t.Error("userinfo did not receive the polled device token")
					}
					_, _ = io.WriteString(w, `{"uid":9007199254740993,"name":"verified"}`)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
			})
			credential, err := service.PollLogin(t.Context(), session)
			if err != nil {
				t.Fatal(err)
			}
			if credential.AccessToken != "dt-token" || credential.RefreshToken != "drt-token" ||
				credential.UserID != "9007199254740993" || credential.Name != "verified" {
				t.Fatalf("incorrect device credential: %+v", credential)
			}
		})
	}
}

/**
 * @brief Only temporary errors during the device poll permit a later poll.
 * @param t Test context.
 */
func TestLoginPollTransport(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cause   error
		pending bool
	}{
		{"dropped connection", io.EOF, true},
		{"connection refused", &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, true},
		{"dial permission denied", &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.EACCES)}, false},
		{"Windows connection reset", &net.OpError{Op: "read", Err: os.NewSyscallError("wsarecv", syscall.Errno(10054))}, true},
		{"temporary DNS", &net.DNSError{IsTemporary: true}, true},
		{"timeout", &net.DNSError{IsTimeout: true}, true},
		{"unknown host", &net.DNSError{IsNotFound: true}, false},
		{"invalid certificate", x509.UnknownAuthorityError{}, false},
		{"cancelled", context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, err := StartLogin("global")
			if err != nil {
				t.Fatal(err)
			}
			polls := 0
			service := NewService(&http.Client{Transport: qoderRoundTripper(func(r *http.Request) (*http.Response, error) {
				response := `{"id":"verified"}`
				if r.URL.Path == "/api/v1/deviceToken/poll" {
					polls++
					if polls == 1 {
						return nil, tc.cause
					}
					response = `{"accessToken":"dt-token"}`
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(response))}, nil
			})})
			credential, err := service.PollLogin(t.Context(), session)
			if credential != nil || err == nil || errors.Is(err, ErrLoginPending) != tc.pending {
				t.Fatalf("unexpected first poll result: %+v %v", credential, err)
			}
			if !tc.pending {
				if tc.cause == context.Canceled && !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation: %v", err)
				}
				return
			}
			credential, err = service.PollLogin(t.Context(), session)
			if err != nil || credential == nil || credential.UserID != "verified" || polls != 2 {
				t.Fatalf("follow-up poll failed: %+v %v, polls=%d", credential, err, polls)
			}
		})
	}
}

/**
 * @brief Invalid poll responses and failures after token issuance terminate device login.
 * @param t Test context.
 */
func TestLoginTerminalResponse(t *testing.T) {
	for _, tc := range []struct {
		name           string
		pollStatus     int
		pollBody       string
		userinfoBody   string
		userinfoErr    error
		wantHTTPStatus int
		wantError      string
	}{
		{"forbidden", http.StatusForbidden, `{}`, "", nil, http.StatusForbidden, ""},
		{"rate limited", http.StatusTooManyRequests, `{}`, "", nil, http.StatusTooManyRequests, ""},
		{"unavailable", http.StatusServiceUnavailable, `{}`, "", nil, http.StatusServiceUnavailable, ""},
		{"redirect refused", http.StatusFound, `{}`, "", nil, 0, "qoder transport failed"},
		{"invalid poll JSON", http.StatusOK, "bad JSON", "", nil, 0, "invalid Qoder JSON response"},
		{"missing token", http.StatusOK, `{}`, "", nil, 0, "qoder login response is missing token"},
		{"missing owner", http.StatusOK, `{"token":"dt-token","uid":"user"}`, `{}`, nil, 0, "qoder profile is missing user identity"},
		{"mismatched numeric owner", http.StatusOK, `{"token":"dt-token","uid":9007199254740993}`, `{"userId":9007199254740992}`, nil, 0, "qoder token owner does not match credential user_id"},
		{"userinfo connection dropped", http.StatusOK, `{"token":"dt-token","uid":"user"}`, "", io.EOF, 0, "qoder transport failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, err := StartLogin("global")
			if err != nil {
				t.Fatal(err)
			}
			service := NewService(&http.Client{
				Transport: qoderRoundTripper(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/api/v1/deviceToken/poll" {
						return &http.Response{
							StatusCode: tc.pollStatus,
							Header:     http.Header{"Location": {"https://elsewhere.invalid/device"}},
							Body:       io.NopCloser(strings.NewReader(tc.pollBody)),
						}, nil
					}
					if tc.userinfoErr != nil {
						return nil, tc.userinfoErr
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tc.userinfoBody))}, nil
				}),
				CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return io.EOF },
			})
			credential, err := service.PollLogin(t.Context(), session)
			if credential != nil || err == nil || errors.Is(err, ErrLoginPending) {
				t.Fatalf("expected terminal login failure: %+v %v", credential, err)
			}
			if tc.wantHTTPStatus != 0 {
				var httpErr *HTTPError
				if !errors.As(err, &httpErr) || httpErr.StatusCode != tc.wantHTTPStatus {
					t.Fatalf("expected HTTP %d, got %v", tc.wantHTTPStatus, err)
				}
			} else if err.Error() != tc.wantError {
				t.Fatalf("expected %q, got %v", tc.wantError, err)
			}
		})
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

/**
 * @brief 签到从包装清单选择本期通用每日额度，并解码包装领取结果。
 * @param t 测试上下文。
 */
func TestCheckinClaimContract(t *testing.T) {
	getCalls := 0
	var postAt atomic.Int64
	service := authTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer dt-token" || r.Header.Get("Cosy-ClientType") != "10" || r.Header.Get("Cosy-Signature") != "" {
			t.Error("invalid sash headers")
		}
		if r.Method == http.MethodGet {
			getCalls++
			if r.URL.Query().Get("forceRefresh") != "true" {
				t.Error("missing forceRefresh")
			}
			now := time.Now()
			_, _ = fmt.Fprintf(w, `{"data":{"campaigns":[
				{"campaignId":"promo","actionType":"VIEW_DETAILS","claimStatus":"CLAIMABLE"},
				{"campaignId":"sonus","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","title":"Sonus daily credits","benefit":{"kind":"CREDITS"}},
				{"campaignId":"scoped","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","title":"Daily credits","benefit":{"kind":"CREDITS","modelScope":{"modelSeries":{"key":"QODER_SOTA"}}}},
				{"campaignId":"not-credits","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","title":"Daily reward","benefit":{"kind":"COUPON"}},
				{"campaignId":"invalid-date","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","title":"Daily credits","endAt":"invalid","benefit":{"kind":"CREDITS"}},
				{"campaignId":"expired","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","title":"Daily credits","endAt":%q,"benefit":{"kind":"CREDITS"}},
				{"campaignId":"future","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","title":"Daily credits","startAt":%d,"benefit":{"kind":"CREDITS"}},
				{"campaignId":"daily","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","placements":[{"type":"USAGE","content":{"zh":{"title":"每日通用额度"}}}],"benefit":{"kind":"CREDITS","modelScope":{"modelSeries":{"key":"ALL_MODELS"}}}}
			]}}`, now.Add(-time.Minute).Format(time.RFC3339Nano), now.Add(time.Hour).UnixMilli())
			return
		}
		if r.URL.Path != authCampaignPath+"/daily/claim" {
			t.Errorf("claimed wrong campaign: %s", r.URL.Path)
		}
		postAt.Store(time.Now().UnixNano())
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 0 {
			t.Errorf("invalid claim body: %v", err)
		}
		_, _ = w.Write([]byte(`{"data":{"status":"CLAIMED","benefit":{"kind":"CREDITS","amount":12.5}}}`))
	})
	result, err := service.Checkin(context.Background(), &Credential{Region: "cn", AccessToken: "dt-token"})
	if err != nil || result.Status != "claimed" || result.CampaignID != "daily" || result.Reward != 12.5 || result.Unit != "credits" || getCalls != 1 {
		t.Fatalf("claim failed: %+v %v", result, err)
	}
	if result.SelectedAt.IsZero() || result.SelectedAt.UnixNano() > postAt.Load() {
		t.Fatalf("selection time %v should precede the claim request", result.SelectedAt)
	}
}

/**
 * @brief 领取状态不明确时仅认可对应活动的确认结果。
 * @param t 测试上下文。
 */
func TestCheckinUncertainResults(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		response     string
		confirmation string
		want         string
	}{
		{"unauthorized replay", 401, `{"result":"ALREADY_CLAIMED"}`, "", "error"},
		{"ordinary conflict", 409, `{"result":"CONFLICT"}`, "", "error"},
		{"explicit replay", 409, `{"data":{"result":"ALREADY_CLAIMED"}}`, "", "already_claimed"},
		{"confirmed after error", 503, `{}`, `{"campaignId":"daily","claimStatus":"CLAIMED"}`, "already_claimed"},
		{"different campaign", 503, `{}`, `{"campaignId":"other","claimStatus":"CLAIMED"}`, "error"},
		{"historical confirmation", 503, `{}`, `{"campaignId":"daily","claimStatus":"CLAIMED","claimedAt":"2025-11-25T02:06:00Z"}`, "error"},
		{"ambiguous success", 200, `{"status":"PENDING"}`, "", "error"},
		{"missing reward", 200, `{"status":"CLAIMED"}`, "", "claimed"},
		{"replayed reward", 200, `{"status":"CLAIMED","replayed":true,"benefit":{"amount":100}}`, "", "already_claimed"},
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
				if lists > 1 && tc.confirmation != "" {
					_, _ = fmt.Fprintf(w, `{"campaigns":[%s]}`, tc.confirmation)
					return
				}
				_, _ = w.Write([]byte(`{"campaigns":[{"campaignId":"daily","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","title":"Daily credits","benefit":{"kind":"CREDITS"}}]}`))
			})
			result, err := service.Checkin(context.Background(), &Credential{Region: "cn", AccessToken: "dt-token"})
			if tc.want == "error" {
				if err == nil {
					t.Fatalf("accepted ambiguous claim: %+v", result)
				}
				return
			}
			if err != nil || result.Status != tc.want || result.Reward != 0 || result.SelectedAt.IsZero() {
				t.Fatalf("unexpected claim outcome: %+v %v", result, err)
			}
		})
	}
}

/**
 * @brief 奖励促销、历史签到及未启用的签到不满足本期签到。
 * @param t 测试上下文。
 */
func TestCheckinUnavailableAndAlreadyClaimed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		row    func(time.Time) string
		status string
	}{
		{"disabled", func(time.Time) string { return `"claimStatus":"DISABLED","title":"Daily Credits"` }, "unavailable"},
		{"claimed promotion", func(time.Time) string { return `"claimStatus":"CLAIMED","title":"Welcome Credits"` }, "unavailable"},
		{"unknown claim period", func(time.Time) string { return `"claimStatus":"CLAIMED","title":"Daily Credits"` }, "unavailable"},
		{"historical claim time", func(window time.Time) string {
			return fmt.Sprintf(`"claimStatus":"CLAIMED","title":"Daily Credits","claimedAt":%q`, window.Add(-time.Minute).Format(time.RFC3339Nano))
		}, "unavailable"},
		{"historical campaign start", func(window time.Time) string {
			return fmt.Sprintf(`"claimStatus":"CLAIMED","title":"Daily Credits","startAt":%q`, fmt.Sprint(window.Add(-24*time.Hour).UnixMilli()))
		}, "unavailable"},
		{"current claim", func(window time.Time) string {
			return fmt.Sprintf(`"claimStatus":"CLAIMED","title":"Daily Credits","claimedAt":%q`, window.Format(time.RFC3339Nano))
		}, "already_claimed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := authTestService(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("unnecessary claim")
				}
				_, _ = fmt.Fprintf(w, `{"campaigns":[{"campaignId":"daily","actionType":"CLAIM_BENEFIT","benefit":{"kind":"CREDITS"},%s}]}`, tc.row(CheckinWindow(time.Now())))
			})
			result, err := service.Checkin(context.Background(), &Credential{Region: "cn", AccessToken: "dt-token"})
			if err != nil || result.Status != tc.status || result.SelectedAt.IsZero() != (tc.status == "unavailable") {
				t.Fatalf("wrong campaign state: %+v %v", result, err)
			}
		})
	}
}

/**
 * @brief 仅有已知旧版奖励特征且无标题的活动可以用旧版规则识别。
 * @param t 测试上下文。
 */
func TestCheckinLegacyDailyCredits(t *testing.T) {
	service := authTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if r.URL.Path != authCampaignPath+"/legacy/claim" {
				t.Errorf("claimed wrong campaign: %s", r.URL.Path)
			}
			_, _ = w.Write([]byte(`{"status":"CLAIMED"}`))
			return
		}
		window := CheckinWindow(time.Now())
		end := window.Add(24 * time.Hour)
		_, _ = fmt.Fprintf(w, `{"data":[
			{"campaignId":"promo","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","title":"Welcome Credits","startAt":%d,"endAt":%d,"benefit":{"kind":"CREDITS","amount":100,"validity":{"mode":"RELATIVE_DAYS","days":30}}},
			{"campaignId":"long","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","startAt":%d,"endAt":%d,"benefit":{"kind":"CREDITS","amount":100,"validity":{"mode":"RELATIVE_DAYS","days":30}}},
			{"campaignId":"legacy","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","startAt":%d,"endAt":%d,"benefit":{"kind":"CREDITS","amount":100,"validity":{"days":30}}}
		]}`, window.Unix(), end.Unix(), window.Unix(), end.Add(time.Hour).Unix(), window.Unix(), end.Unix())
	})
	result, err := service.Checkin(context.Background(), &Credential{Region: "cn", AccessToken: "dt-token"})
	if err != nil || result.Status != "claimed" || result.CampaignID != "legacy" {
		t.Fatalf("legacy claim failed: %+v %v", result, err)
	}
}

/**
 * @brief 国际版按 Sash UID 获取风控身份，忽略旧窗口活动并领取当前每日活动。
 * @param t 测试上下文。
 */
func TestInternationalCheckinRiskDependency(t *testing.T) {
	service := authTestService(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if r.URL.Path != authCampaignPath+"/daily/claim" || r.Header.Get("Authorization") != "Bearer dt-token" {
				t.Errorf("invalid international claim: %s", r.URL.Path)
			}
			authTestJSON(t, w, map[string]string{"status": "CLAIMED"})
			return
		}
		if r.Header.Get("Cosy-MachineToken") == "risk-token" {
			if r.Header.Get("Cosy-MachineCode") != "risk-code" || r.Header.Get("Cosy-MachineType") != "risk-type" {
				t.Error("incomplete risk identity")
			}
			window := CheckinWindow(time.Now())
			_, _ = fmt.Fprintf(w, `{"data":{"uid":"sash-uid","campaigns":[
				{"campaignId":"unknown","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","title":"Daily Credits","benefit":{"kind":"CREDITS"}},
				{"campaignId":"stale","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","title":"Daily Credits","startAt":%d,"benefit":{"kind":"CREDITS"}},
				{"campaignId":"daily","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE","title":"Daily Credits","startAt":%d,"benefit":{"kind":"CREDITS"}}
			]}}`, window.Add(-24*time.Hour).Unix(), window.Unix())
			return
		}
		_, _ = w.Write([]byte(`{"data":{"uid":"sash-uid","campaigns":[]}}`))
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
