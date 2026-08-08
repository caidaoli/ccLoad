package xaiauth_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"ccLoad/internal/xaiauth"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

const expectedXAIOAuthScope = "openid profile email offline_access grok-cli:access api:access conversations:read conversations:write workspaces:read workspaces:write"

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func response(status int, body string, headers ...http.Header) *http.Response {
	header := make(http.Header)
	if len(headers) > 0 {
		header = headers[0]
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

func TestDiscoveryAndDeviceRejectNonAuthOrigin(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return response(200, `{"device_authorization_endpoint":"https://evil.example/device","token_endpoint":"https://auth.x.ai/oauth2/token"}`), nil
	})}
	_, err := xaiauth.NewService(client).StartDevice(context.Background())
	if err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("expected origin rejection, got %v", err)
	}
}

func TestOAuthRequestsNeverFollowRedirects(t *testing.T) {
	t.Parallel()
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return response(http.StatusFound, "", http.Header{"Location": []string{"https://evil.example/steal"}}), nil
	})}
	_, err := xaiauth.NewService(client).StartDevice(context.Background())
	if err == nil || requests != 1 {
		t.Fatalf("redirect was followed or accepted: requests=%d err=%v", requests, err)
	}
}

func TestStartDeviceUsesFixedDiscoveryAndValidatesVerificationURLs(t *testing.T) {
	t.Parallel()
	var paths []string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.String())
		switch req.URL.Path {
		case "/.well-known/openid-configuration":
			return response(200, `{"device_authorization_endpoint":"https://auth.x.ai/oauth2/device/code","token_endpoint":"https://auth.x.ai/oauth2/token"}`), nil
		case "/oauth2/device/code":
			body, _ := io.ReadAll(req.Body)
			values, _ := url.ParseQuery(string(body))
			if values.Get("client_id") != xaiauth.ClientID || values.Get("scope") != expectedXAIOAuthScope {
				t.Fatalf("unexpected form: %s", body)
			}
			return response(200, `{"device_code":"device-secret","user_code":"ABCD","verification_uri":"https://auth.x.ai/activate","verification_uri_complete":"https://auth.x.ai/activate?user_code=ABCD","expires_in":600,"interval":2}`), nil
		default:
			t.Fatalf("unexpected request: %s", req.URL)
			return nil, nil
		}
	})}
	device, err := xaiauth.NewService(client).StartDevice(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if device.DeviceCode != "device-secret" || device.TokenEndpoint != xaiauth.TokenURL || device.ExpiresAt.IsZero() || len(paths) != 2 || paths[0] != xaiauth.DiscoveryURL {
		t.Fatalf("unexpected device/discovery: %+v %v", device, paths)
	}
	raw, _ := json.Marshal(device)
	if strings.Contains(string(raw), "device-secret") || strings.Contains(device.Redacted(), "device-secret") {
		t.Fatalf("device code leaked: JSON=%s string=%s", raw, device.Redacted())
	}
}

func TestStartDeviceRejectsInvalidExpiresIn(t *testing.T) {
	t.Parallel()
	for _, expiresIn := range []int{0, -1} {
		client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/.well-known/openid-configuration" {
				return response(200, `{"device_authorization_endpoint":"https://auth.x.ai/oauth2/device/code","token_endpoint":"https://auth.x.ai/oauth2/token"}`), nil
			}
			return response(200, fmt.Sprintf(`{"device_code":"secret","user_code":"CODE","verification_uri":"https://auth.x.ai/activate","expires_in":%d}`, expiresIn)), nil
		})}
		if _, err := xaiauth.NewService(client).StartDevice(context.Background()); err == nil || !strings.Contains(err.Error(), "expires_in") {
			t.Errorf("expires_in=%d accepted: %v", expiresIn, err)
		}
	}
}

func TestPollDeviceDoesNotExtendStartExpiry(t *testing.T) {
	t.Parallel()
	tokenRequests := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/.well-known/openid-configuration":
			return response(200, `{"device_authorization_endpoint":"https://auth.x.ai/oauth2/device/code","token_endpoint":"https://auth.x.ai/oauth2/token"}`), nil
		case "/oauth2/device/code":
			return response(200, `{"device_code":"secret","user_code":"CODE","verification_uri":"https://auth.x.ai/activate","expires_in":1,"interval":1}`), nil
		case "/oauth2/token":
			tokenRequests++
			return response(400, `{"error":"authorization_pending"}`), nil
		default:
			t.Fatalf("unexpected path: %s", req.URL.Path)
			return nil, nil
		}
	})}
	device, err := xaiauth.NewService(client).StartDevice(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	started := time.Now()
	_, err = xaiauth.NewService(client).PollDevice(context.Background(), device)
	if !errors.Is(err, xaiauth.ErrDeviceExpired) || tokenRequests != 0 || time.Since(started) > 300*time.Millisecond {
		t.Fatalf("poll extended start expiry: elapsed=%s requests=%d err=%v", time.Since(started), tokenRequests, err)
	}
}

func TestStartDeviceRejectsUntrustedVerificationURL(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/.well-known/openid-configuration" {
			return response(200, `{"device_authorization_endpoint":"https://auth.x.ai/oauth2/device/code","token_endpoint":"https://auth.x.ai/oauth2/token"}`), nil
		}
		return response(200, `{"device_code":"secret","user_code":"CODE","verification_uri":"https://evil.example/activate","expires_in":600}`), nil
	})}
	_, err := xaiauth.NewService(client).StartDevice(context.Background())
	if err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("expected verification origin rejection, got %v", err)
	}
}

func TestPollDeviceHandlesPendingSlowDownAndCompletes(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	attempt := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		attempt++
		switch attempt {
		case 1:
			return response(400, `{"error":"authorization_pending"}`), nil
		case 2:
			return response(400, `{"error":"slow_down"}`), nil
		default:
			return response(200, `{"access_token":"access","refresh_token":"refresh","expires_in":3600,"token_type":"Bearer"}`), nil
		}
	})}
	device := testDeviceCode(time.Minute, time.Millisecond)
	credential, err := xaiauth.NewService(client).PollDevice(context.Background(), device)
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "access" || credential.RefreshToken != "refresh" || attempt != 3 {
		t.Fatalf("unexpected result: %+v attempts=%d", credential.Redacted(), attempt)
	}
}

func TestPollDeviceCancellationAndTerminalErrors(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := xaiauth.NewService(http.DefaultClient).PollDevice(ctx, testDeviceCode(time.Minute, 0))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled, got %v", err)
	}
	for oauthCode, target := range map[string]error{"access_denied": xaiauth.ErrAccessDenied, "expired_token": xaiauth.ErrDeviceExpired} {
		oauthCode, target := oauthCode, target
		t.Run(oauthCode, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return response(400, `{"error":"`+oauthCode+`"}`), nil
			})}
			_, got := xaiauth.NewService(client).PollDevice(context.Background(), testDeviceCode(time.Minute, time.Millisecond))
			if !errors.Is(got, target) {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestPollDeviceRejectsSuccessfulResponseWithoutRefreshToken(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(200, `{"access_token":"access","expires_in":3600}`), nil
	})}
	_, err := xaiauth.NewService(client).PollDevice(context.Background(), testDeviceCode(time.Minute, time.Millisecond))
	if err == nil || !strings.Contains(err.Error(), "refresh_token") {
		t.Fatalf("expected incomplete credential rejection, got %v", err)
	}
}

func TestPollDeviceBoundsLongIntervalByServerExpiry(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(400, `{"error":"authorization_pending"}`), nil
	})}
	started := time.Now()
	_, err := xaiauth.NewService(client).PollDevice(context.Background(), testDeviceCode(time.Second, time.Hour))
	if !errors.Is(err, xaiauth.ErrDeviceExpired) || time.Since(started) > 2500*time.Millisecond {
		t.Fatalf("server expiry not enforced promptly: elapsed=%s err=%v", time.Since(started), err)
	}
}

func TestPollDeviceAcceptsNilContext(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(400, `{"error":"access_denied"}`), nil
	})}
	//nolint:staticcheck // PollDevice intentionally accepts nil as a public robustness contract.
	_, err := xaiauth.NewService(client).PollDevice(nil, testDeviceCode(time.Second, 0))
	if !errors.Is(err, xaiauth.ErrAccessDenied) {
		t.Fatalf("nil context failed unsafely: %v", err)
	}
}

func testDeviceCode(expiresAfter, interval time.Duration) *xaiauth.DeviceCode {
	return &xaiauth.DeviceCode{
		DeviceCode: "secret", TokenEndpoint: xaiauth.TokenURL,
		ExpiresIn: int(expiresAfter / time.Second), ExpiresAt: time.Now().Add(expiresAfter), Interval: interval,
	}
}

func TestRefreshUsesCredentialClientIDAndPreservesOmittedRefreshToken(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://auth.x.ai/custom/token" {
			t.Fatalf("unexpected URL: %s", req.URL)
		}
		body, _ := io.ReadAll(req.Body)
		values, _ := url.ParseQuery(string(body))
		if values.Get("client_id") != "custom-client" || values.Get("refresh_token") != "old-refresh" {
			t.Fatalf("unexpected refresh form: %s", body)
		}
		return response(200, `{"access_token":"new-access","expires_in":3600}`), nil
	})}
	old := &xaiauth.Credential{AccessToken: "old-access", RefreshToken: "old-refresh", ClientID: "custom-client", TokenEndpoint: "https://auth.x.ai/custom/token", Expired: "2030-01-01T00:00:00Z"}
	got, err := xaiauth.NewService(client).Refresh(context.Background(), old)
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshToken != "old-refresh" || got.ClientID != "custom-client" || got.TokenEndpoint != "https://auth.x.ai/custom/token" || got.AccessToken != "new-access" {
		t.Fatalf("unexpected credential: %+v", got.Redacted())
	}
}

func TestRefreshTokenUsesTrustedEndpointAndPreservesInputToken(t *testing.T) {
	t.Parallel()
	secret := "imported-refresh-secret"
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != xaiauth.TokenURL {
			t.Fatalf("unexpected URL: %s", req.URL)
		}
		body, _ := io.ReadAll(req.Body)
		values, _ := url.ParseQuery(string(body))
		if values.Get("client_id") != "custom-client" || values.Get("refresh_token") != secret {
			t.Fatalf("unexpected refresh form")
		}
		return response(200, `{"access_token":"new-access","expires_in":3600}`), nil
	})}

	got, err := xaiauth.NewService(client).RefreshToken(context.Background(), secret, "custom-client")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "new-access" || got.RefreshToken != secret || got.ClientID != "custom-client" || got.TokenEndpoint != xaiauth.TokenURL {
		t.Fatalf("unexpected credential: %s", got)
	}
}

func TestTokenErrorsNeverEchoSecretsOrBody(t *testing.T) {
	t.Parallel()
	secret := "refresh-super-secret"
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(400, `{"error":"`+secret+`","error_description":"`+secret+`"}`), nil
	})}
	_, err := xaiauth.NewService(client).Refresh(context.Background(), &xaiauth.Credential{RefreshToken: secret})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("unsafe error: %v", err)
	}
}
