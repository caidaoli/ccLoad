package xaiauth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/xaiauth"
)

type externalCookieJar struct{}

func (externalCookieJar) Cookies(*url.URL) []*http.Cookie {
	return []*http.Cookie{{Name: "external", Value: "must-not-leak"}}
}

func (externalCookieJar) SetCookies(*url.URL, []*http.Cookie) {}

func TestConvertSSOUsesAccountsThenAuthAndDoesNotLeakCookie(t *testing.T) {
	t.Parallel()
	secret := "sso-super-secret"
	step := 0
	started := time.Now()
	client := &http.Client{Jar: externalCookieJar{}, Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		step++
		if strings.Contains(req.Header.Get("Cookie"), "external=") {
			t.Fatalf("caller cookie jar leaked into isolated SSO flow: %s", req.Header.Get("Cookie"))
		}
		if step == 1 {
			if req.URL.String() != xaiauth.SSOAccountsURL || !strings.Contains(req.Header.Get("Cookie"), secret) {
				t.Fatalf("bad initial request: %s", req.URL)
			}
			return response(200, `{}`, http.Header{"Set-Cookie": []string{"accounts-only=1; Path=/; Secure; HttpOnly"}}), nil
		}
		if req.URL.Host != "auth.x.ai" {
			t.Fatalf("non-auth followup: %s", req.URL)
		}
		if strings.Contains(req.Header.Get("Cookie"), "accounts-only=1") {
			t.Fatalf("accounts host-only cookie leaked to auth: %s", req.Header.Get("Cookie"))
		}
		switch req.URL.Path {
		case "/oauth2/device/code":
			if err := req.ParseForm(); err != nil || req.Form.Get("scope") != xaiauth.SSOScope {
				t.Fatalf("unexpected SSO scope: %q err=%v", req.Form.Get("scope"), err)
			}
			return response(200, `{"device_code":"device","user_code":"CODE","verification_uri_complete":"https://auth.x.ai/activate?user_code=CODE","interval":1,"expires_in":60}`, http.Header{"Set-Cookie": []string{"device-path=1; Path=/oauth2/device; Secure"}}), nil
		case "/activate":
			if strings.Contains(req.Header.Get("Cookie"), "device-path=1") {
				t.Fatalf("path-scoped cookie leaked to activate: %s", req.Header.Get("Cookie"))
			}
			return response(200, `{}`), nil
		case "/oauth2/device/verify":
			return response(302, "", http.Header{"Location": []string{"https://auth.x.ai/consent"}}), nil
		case "/consent":
			return response(200, `{}`), nil
		case "/oauth2/device/approve":
			return response(302, "", http.Header{"Location": []string{"https://auth.x.ai/done"}}), nil
		case "/done":
			return response(200, `{}`), nil
		case "/oauth2/token":
			if elapsed := time.Since(started); elapsed < 900*time.Millisecond {
				t.Fatalf("first SSO token poll ignored server interval: %s", elapsed)
			}
			return response(200, `{"access_token":"access","refresh_token":"refresh","expires_in":3600}`), nil
		default:
			t.Fatalf("unexpected path: %s", req.URL.Path)
			return nil, nil
		}
	})}
	credential, err := xaiauth.NewService(client).ConvertSSO(context.Background(), secret)
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "access" || step < 7 {
		t.Fatalf("unexpected result: %s step=%d", credential.Redacted(), step)
	}
}

func TestConvertSSOPollDeadlineBoundsLongInterval(t *testing.T) {
	t.Parallel()
	started := time.Now()
	tokenRequests := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/":
			return response(200, `{}`), nil
		case "/oauth2/device/code":
			return response(200, `{"device_code":"device","user_code":"CODE","verification_uri_complete":"https://auth.x.ai/activate","interval":3600,"expires_in":1}`), nil
		case "/activate", "/consent", "/done":
			return response(200, `{}`), nil
		case "/oauth2/device/verify":
			return response(302, "", http.Header{"Location": []string{"https://auth.x.ai/consent"}}), nil
		case "/oauth2/device/approve":
			return response(302, "", http.Header{"Location": []string{"https://auth.x.ai/done"}}), nil
		case "/oauth2/token":
			tokenRequests++
			return response(400, `{"error":"authorization_pending"}`), nil
		default:
			t.Fatalf("unexpected path: %s", req.URL.Path)
			return nil, nil
		}
	})}
	_, err := xaiauth.NewService(client).ConvertSSO(context.Background(), "secret")
	if !errors.Is(err, xaiauth.ErrDeviceExpired) || tokenRequests != 0 || time.Since(started) > 2500*time.Millisecond {
		t.Fatalf("SSO device expiry not enforced: elapsed=%s requests=%d err=%v", time.Since(started), tokenRequests, err)
	}
}

func TestConvertSSORejectsInvalidDeviceExpiresIn(t *testing.T) {
	t.Parallel()
	for _, expiresIn := range []int{0, -1} {
		expiresIn := expiresIn
		t.Run(fmt.Sprintf("expires_in_%d", expiresIn), func(t *testing.T) {
			requests := 0
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				switch req.URL.Path {
				case "/":
					return response(200, `{}`), nil
				case "/oauth2/device/code":
					return response(200, fmt.Sprintf(`{"device_code":"device","user_code":"CODE","verification_uri_complete":"https://auth.x.ai/activate","interval":1,"expires_in":%d}`, expiresIn)), nil
				default:
					t.Fatalf("request continued after invalid expires_in: %s", req.URL.Path)
					return nil, nil
				}
			})}
			_, err := xaiauth.NewService(client).ConvertSSO(context.Background(), "secret")
			if err == nil || !strings.Contains(err.Error(), "expires_in") || requests != 2 {
				t.Fatalf("invalid expires_in accepted: requests=%d err=%v", requests, err)
			}
		})
	}
}

func TestConvertSSOPollWaitHonorsCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	tokenRequests := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/":
			return response(200, `{}`), nil
		case "/oauth2/device/code":
			return response(200, `{"device_code":"device","user_code":"CODE","verification_uri_complete":"https://auth.x.ai/activate","interval":3600,"expires_in":60}`), nil
		case "/activate", "/consent":
			return response(200, `{}`), nil
		case "/oauth2/device/verify":
			return response(302, "", http.Header{"Location": []string{"https://auth.x.ai/consent"}}), nil
		case "/oauth2/device/approve":
			return response(302, "", http.Header{"Location": []string{"https://auth.x.ai/done"}}), nil
		case "/done":
			cancel()
			return response(200, `{}`), nil
		case "/oauth2/token":
			tokenRequests++
			return response(200, `{}`), nil
		default:
			t.Fatalf("unexpected path: %s", req.URL.Path)
			return nil, nil
		}
	})}
	_, err := xaiauth.NewService(client).ConvertSSO(ctx, "secret")
	if !errors.Is(err, context.Canceled) || tokenRequests != 0 {
		t.Fatalf("cancel not honored before token poll: requests=%d err=%v", tokenRequests, err)
	}
}

func TestConvertSSOCookiesRespectHostScopeAcrossRedirects(t *testing.T) {
	t.Parallel()
	step := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		step++
		switch step {
		case 1:
			return response(302, "", http.Header{
				"Location":   []string{"https://auth.x.ai/check"},
				"Set-Cookie": []string{"accounts-only=1; Path=/; Secure"},
			}), nil
		case 2:
			if strings.Contains(req.Header.Get("Cookie"), "accounts-only=1") {
				t.Fatalf("accounts cookie leaked to auth: %s", req.Header.Get("Cookie"))
			}
			return response(302, "", http.Header{
				"Location":   []string{"https://accounts.x.ai/back"},
				"Set-Cookie": []string{"auth-only=1; Path=/; Secure"},
			}), nil
		default:
			if strings.Contains(req.Header.Get("Cookie"), "auth-only=1") {
				t.Fatalf("auth cookie leaked to accounts: %s", req.Header.Get("Cookie"))
			}
			if !strings.Contains(req.Header.Get("Cookie"), "accounts-only=1") {
				t.Fatalf("accounts cookie missing on redirect home: %s", req.Header.Get("Cookie"))
			}
			return response(http.StatusUnauthorized, `{}`), nil
		}
	})}
	_, err := xaiauth.NewService(client).ConvertSSO(context.Background(), "secret")
	if !errors.Is(err, xaiauth.ErrSSOUnauthorized) || step != 3 {
		t.Fatalf("unexpected result: step=%d err=%v", step, err)
	}
}

func TestConvertSSORejectsRedirectAndBodyLimitsWithoutLeak(t *testing.T) {
	t.Parallel()
	secret := "sso-secret"
	for name, rt := range map[string]roundTripFunc{
		"redirect": func(*http.Request) (*http.Response, error) {
			return response(302, secret, http.Header{"Location": []string{"https://evil.example/steal"}}), nil
		},
		"body": func(*http.Request) (*http.Response, error) {
			return response(200, strings.Repeat("x", xaiauth.MaxSSOResponseBytes+1)+secret), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := xaiauth.NewService(&http.Client{Transport: rt}).ConvertSSO(context.Background(), secret)
			if err == nil || strings.Contains(err.Error(), secret) {
				t.Fatalf("unsafe/missing error: %v", err)
			}
		})
	}
}

func TestConvertSSORejectsTooManyRedirectsAndPageDrift(t *testing.T) {
	t.Parallel()
	t.Run("redirect limit", func(t *testing.T) {
		requests := 0
		client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			requests++
			return response(http.StatusFound, "", http.Header{"Location": []string{"https://auth.x.ai/loop"}}), nil
		})}
		_, err := xaiauth.NewService(client).ConvertSSO(context.Background(), "secret")
		if err == nil || requests != 9 || !strings.Contains(err.Error(), "too many") {
			t.Fatalf("requests=%d err=%v", requests, err)
		}
	})
	t.Run("consent drift", func(t *testing.T) {
		step := 0
		client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			step++
			switch step {
			case 1:
				return response(200, `{}`), nil
			case 2:
				return response(200, `{"device_code":"device","user_code":"CODE","verification_uri_complete":"https://auth.x.ai/activate","expires_in":60}`), nil
			case 3:
				return response(200, `{}`), nil
			default:
				return response(200, `<html>changed</html>`), nil
			}
		})}
		_, err := xaiauth.NewService(client).ConvertSSO(context.Background(), "secret")
		if err == nil || !strings.Contains(err.Error(), "consent") {
			t.Fatalf("expected page drift rejection, got %v", err)
		}
	})
}

func TestConvertSSOHonorsContext(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := xaiauth.NewService(client).ConvertSSO(ctx, "secret")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestBillingHelpersAndClassifications(t *testing.T) {
	t.Parallel()
	weekly, err := xaiauth.BillingURL("https://cli-chat-proxy.grok.com/v1/", true)
	if err != nil || weekly != "https://cli-chat-proxy.grok.com/v1/billing?format=credits" {
		t.Fatalf("weekly=%q err=%v", weekly, err)
	}
	if _, err := xaiauth.BillingURL("http://evil.example/v1", false); err == nil {
		t.Fatal("expected untrusted billing base rejection")
	}
	custom, err := xaiauth.BillingURL("https://gateway.example/xai/v1", false)
	if err != nil || custom != "https://gateway.example/xai/v1/billing" {
		t.Fatalf("custom=%q err=%v", custom, err)
	}
	req, _ := http.NewRequest(http.MethodGet, weekly, nil)
	xaiauth.ApplyBillingHeaders(req, "access")
	if req.Header.Get("Authorization") != "Bearer access" || req.Header.Get(xaiauth.CLITokenAuthHeader) != xaiauth.CLITokenAuthValue || req.Header.Get(xaiauth.CLIClientVersionHeader) != xaiauth.CLIClientVersion {
		t.Fatalf("headers=%v", req.Header)
	}
	cases := []struct {
		status int
		body   string
		header http.Header
		want   xaiauth.BillingClassification
	}{
		{401, `{}`, nil, xaiauth.BillingBadCredential},
		{403, `{"error":{"type":"authentication_error","code":"invalid_token"}}`, nil, xaiauth.BillingBadCredential},
		{403, `{"error":{"type":"permission_error","code":"subscription_required"}}`, nil, xaiauth.BillingEntitlement},
		{429, `{"error":{"type":"rate_limit_error","code":"quota_exceeded"}}`, nil, xaiauth.BillingQuota},
		{403, `{}`, http.Header{"X-Entitlement-Status": []string{"not_entitled"}}, xaiauth.BillingEntitlement},
		{403, `{}`, http.Header{"Xai-Entitlement-Status": []string{"not_entitled"}}, xaiauth.BillingEntitlement},
		{429, `{}`, http.Header{"X-Ratelimit-Remaining": []string{"0"}}, xaiauth.BillingIndeterminate},
		{429, `{}`, http.Header{"X-Ratelimit-Remaining-Requests": []string{"0"}}, xaiauth.BillingQuota},
		{403, `{"code":"invalid_token","error":"credentials rejected"}`, nil, xaiauth.BillingBadCredential},
		{403, `{"code":"subscription_required","error":"subscription required"}`, nil, xaiauth.BillingEntitlement},
		{429, `{"code":"quota_exceeded","error":"quota exhausted"}`, nil, xaiauth.BillingQuota},
		{403, `{"outer":{"error":{"type":"authentication_error","code":"invalid_token"}}}`, nil, xaiauth.BillingIndeterminate},
		{429, `{"outer":{"type":"rate_limit_error","code":"quota_exceeded"}}`, nil, xaiauth.BillingIndeterminate},
		{403, `{"error":{"type":"ordinary","code":"subscription_required"}}`, nil, xaiauth.BillingIndeterminate},
		{429, `{"error":{"type":"ordinary","code":"quota_exceeded"}}`, nil, xaiauth.BillingIndeterminate},
		{403, `{"error":"forbidden"}`, nil, xaiauth.BillingIndeterminate},
		{429, `not json`, nil, xaiauth.BillingIndeterminate},
	}
	for _, tc := range cases {
		if got := xaiauth.ClassifyBillingResponse(tc.status, tc.header, []byte(tc.body)); got != tc.want {
			t.Errorf("status %d body %s: got %s want %s", tc.status, tc.body, got, tc.want)
		}
	}
}
