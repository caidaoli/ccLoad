package vertexauth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	jwtBearerGrantType = "urn:ietf:params:oauth:grant-type:jwt-bearer"
	assertionLifetime  = time.Hour
	// clockSkewAllowance backdates iat like golang.org/x/oauth2/jws: Google
	// refuses assertions issued in its future.
	clockSkewAllowance  = 10 * time.Second
	maxTokenResponseLen = 1 << 20
)

// Token is a short-lived OAuth access token minted for the service account.
type Token struct {
	AccessToken string
	ExpiresAt   time.Time
}

type tokenEndpointError struct {
	statusCode   int
	responseBody string
}

func (e *tokenEndpointError) Error() string {
	return fmt.Sprintf("vertex token endpoint returned %d: %s", e.statusCode, strings.TrimSpace(e.responseBody))
}

// StatusCode exposes the token endpoint status for credential error classification.
func (e *tokenEndpointError) StatusCode() int { return e.statusCode }

// UpstreamResponseBody returns the bounded token endpoint response body.
func (e *tokenEndpointError) UpstreamResponseBody() string { return e.responseBody }

// MintAccessToken exchanges a self-signed RS256 assertion for a cloud-platform
// access token (RFC 7523 JWT bearer grant), the same flow
// google.CredentialsFromJSON uses for service account keys.
func (c *Credential) MintAccessToken(ctx context.Context, client *http.Client, now time.Time) (*Token, error) {
	if client == nil {
		return nil, errors.New("vertex token client is unavailable")
	}
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	privateKey, err := parseRSAPrivateKey(stringField(c.ServiceAccount, "private_key"))
	if err != nil {
		return nil, err
	}
	tokenURI := c.tokenURI()
	issuedAt := now.Add(-clockSkewAllowance)
	assertion, err := signAssertion(privateKey, stringField(c.ServiceAccount, "private_key_id"), map[string]any{
		"iss":   c.Email,
		"scope": CloudPlatformScope,
		"aud":   tokenURI,
		"iat":   issuedAt.Unix(),
		"exp":   issuedAt.Add(assertionLifetime).Unix(),
	})
	if err != nil {
		return nil, err
	}

	form := url.Values{"grant_type": {jwtBearerGrantType}, "assertion": {assertion}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build vertex token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("vertex token request: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxTokenResponseLen))
	if err != nil {
		return nil, fmt.Errorf("read vertex token response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, &tokenEndpointError{statusCode: response.StatusCode, responseBody: string(body)}
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode vertex token response: %w", err)
	}
	payload.AccessToken = strings.TrimSpace(payload.AccessToken)
	if payload.AccessToken == "" {
		return nil, errors.New("vertex token response missing access_token")
	}
	lifetime := time.Duration(payload.ExpiresIn) * time.Second
	if lifetime <= 0 {
		lifetime = assertionLifetime
	}
	return &Token{AccessToken: payload.AccessToken, ExpiresAt: now.Add(lifetime)}, nil
}

func signAssertion(key *rsa.PrivateKey, keyID string, claims map[string]any) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if keyID != "" {
		header["kid"] = keyID
	}
	encodedHeader, err := encodeSegment(header)
	if err != nil {
		return "", err
	}
	encodedClaims, err := encodeSegment(claims)
	if err != nil {
		return "", err
	}
	signingInput := encodedHeader + "." + encodedClaims
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign vertex assertion: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func encodeSegment(value any) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode vertex assertion: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}
