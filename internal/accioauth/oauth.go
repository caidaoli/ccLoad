package accioauth

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

const oauthClientID = "accio-work"

func loginURL(region string) (string, error) {
	switch region {
	case "cn":
		return "https://www.accio-ai.com/login", nil
	case "global":
		return "https://www.accio.com/login", nil
	default:
		return "", errors.New("accio region must be cn or global")
	}
}

// AuthorizationLink creates the edition-specific PKCE login URL.
// redirectURI must also be passed unchanged to ExchangeCode.
func AuthorizationLink(region, state, challenge, redirectURI string) (string, error) {
	base, err := loginURL(region)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(state) == "" || strings.TrimSpace(challenge) == "" || strings.TrimSpace(redirectURI) == "" {
		return "", errors.New("accio OAuth state, challenge and redirect URI are required")
	}
	query := url.Values{
		"return_url": {redirectURI}, "state": {state}, "code_challenge": {challenge},
		"code_challenge_method": {"S256"}, "client_id": {oauthClientID},
	}
	return base + "?" + query.Encode(), nil
}

// ExchangeCode exchanges a one-use code without exposing the PKCE verifier.
func (s *Service) ExchangeCode(ctx context.Context, region, code, verifier, redirectURI string) (*Credential, error) {
	if _, err := loginURL(region); err != nil {
		return nil, err
	}
	if strings.TrimSpace(code) == "" || strings.TrimSpace(verifier) == "" || strings.TrimSpace(redirectURI) == "" {
		return nil, errors.New("accio OAuth code, verifier and redirect URI are required")
	}
	data, err := s.authDo(ctx, &Credential{Region: region}, http.MethodPost, "/api/oauth/token", map[string]string{
		"code": code, "codeVerifier": verifier, "clientId": oauthClientID, "redirectUri": redirectURI,
	})
	if err != nil {
		return nil, err
	}
	data = authData(data)
	c := &Credential{
		Region: region, AccessToken: authText(data, "accessToken"), RefreshToken: authText(data, "refreshToken"),
	}
	c.ExpiresAt, err = authExpiry(data["expiresAt"])
	if err != nil {
		return nil, err
	}
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	return c, nil
}
