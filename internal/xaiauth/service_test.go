package xaiauth_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"ccLoad/internal/xaiauth"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

const expectedXAIOAuthScope = "openid profile email offline_access grok-cli:access api:access"

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func response(status int, body string, headers ...http.Header) *http.Response {
	header := make(http.Header)
	if len(headers) > 0 {
		header = headers[0]
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

func TestNewAuthorizationBuildsSub2APCompatiblePKCEURLWithoutNetwork(t *testing.T) {
	t.Parallel()
	requests := 0
	service := xaiauth.NewService(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, nil
	})})
	authorization, err := service.NewAuthorization()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authorization.URL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if requests != 0 || parsed.Scheme+"://"+parsed.Host+parsed.Path != xaiauth.AuthorizeURL {
		t.Fatalf("authorization generation used network or wrong endpoint: requests=%d url=%s", requests, parsed)
	}
	for key, want := range map[string]string{
		"response_type": "code", "client_id": xaiauth.ClientID,
		"redirect_uri": xaiauth.RedirectURI, "scope": expectedXAIOAuthScope,
		"state": authorization.State, "nonce": authorization.Nonce,
		"code_challenge": authorization.CodeChallenge, "code_challenge_method": "S256",
		"plan": "generic", "referrer": "ccload",
	} {
		if got := query.Get(key); got != want {
			t.Errorf("query %s = %q, want %q", key, got, want)
		}
	}
	if authorization.State == "" || authorization.CodeVerifier == "" || authorization.CodeChallenge == "" || authorization.Nonce == "" {
		t.Fatalf("incomplete authorization: %#v", authorization)
	}
}

func TestParseAuthorizationInputAcceptsURLQueryAndBareCode(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"url":   xaiauth.RedirectURI + "?code=url-code&state=url-state",
		"query": "?code=query-code&state=query-state",
		"bare":  "bare-code",
	} {
		t.Run(name, func(t *testing.T) {
			input := xaiauth.ParseAuthorizationInput(raw)
			if input.Code != name+"-code" {
				t.Fatalf("input = %#v", input)
			}
			if name == "bare" && input.RequiresState {
				t.Fatal("bare code unexpectedly requires state")
			}
			if name != "bare" && (!input.RequiresState || input.State != name+"-state") {
				t.Fatalf("input = %#v", input)
			}
		})
	}
}

func TestExchangeCodeUsesPKCEWireContract(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != xaiauth.TokenURL {
			t.Fatalf("unexpected URL: %s", req.URL)
		}
		body, _ := io.ReadAll(req.Body)
		values, _ := url.ParseQuery(string(body))
		for key, want := range map[string]string{
			"grant_type": "authorization_code", "client_id": xaiauth.ClientID,
			"code": "auth-code", "code_verifier": "pkce-verifier", "redirect_uri": xaiauth.RedirectURI,
		} {
			if got := values.Get(key); got != want {
				t.Errorf("form %s = %q, want %q", key, got, want)
			}
		}
		return response(200, `{"access_token":"access","refresh_token":"refresh","expires_in":3600,"token_type":"Bearer"}`), nil
	})}
	credential, err := xaiauth.NewService(client).ExchangeCode(context.Background(), "auth-code", "pkce-verifier")
	if err != nil {
		t.Fatal(err)
	}
	if credential.AccessToken != "access" || credential.RefreshToken != "refresh" || credential.TokenEndpoint != xaiauth.TokenURL {
		t.Fatalf("unexpected credential: %s", credential)
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
