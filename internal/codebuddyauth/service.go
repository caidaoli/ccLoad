package codebuddyauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Service implements the wire contract from lovingfish/workbuddy-cliproxy,
// commit 7efb280563b8cf2bf62e4295340708bac4ec3d6a.
// Auth/control plane and transport lifecycle are owned by ccLoad.
type Service struct {
	Client  *http.Client
	BaseURL string
	Now     func() time.Time
}

// NewService creates a service using the supplied transport.
func NewService(client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	return &Service{Client: client, BaseURL: BaseURL, Now: time.Now}
}

// APIError retains status and business code without reflecting tokens or upstream bodies.
type APIError struct {
	Status int
	Code   int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("CodeBuddy upstream HTTP %d (code %d)", e.Status, e.Code)
}

// StatusCode exposes the HTTP status to credential rejection handling.
func (e *APIError) StatusCode() int { return e.Status }

// UpstreamResponseBody deliberately excludes sensitive upstream data.
func (e *APIError) UpstreamResponseBody() string { return "{}" }

// ErrCannotRefresh requires reauthorization because no refresh token is available.
var ErrCannotRefresh = errors.New("CodeBuddy credential has no refresh token; authorize again")

// ApplySourceHeaders supplies the CodeBuddy CLI request fingerprint.
func ApplySourceHeaders(h http.Header) {
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("Origin", "https://www.codebuddy.cn")
	h.Set("Referer", "https://www.codebuddy.cn/")
	h.Set("User-Agent", "CLI/2.63.2 CodeBuddy/2.63.2")
}

// ApplyCredentialHeaders adds authentication and account identity headers.
func ApplyCredentialHeaders(h http.Header, c *Credential) {
	ApplySourceHeaders(h)
	h.Set("Authorization", "Bearer "+c.AccessToken)
	for _, item := range [][3]string{{"X-User-Id", "X-No-User-Id", c.UID}, {"X-Enterprise-Id", "X-No-Enterprise-Id", c.EnterpriseID}, {"X-Domain", "X-No-Department-Info", c.Domain}} {
		h.Del(item[0])
		h.Del(item[1])
		if item[2] == "" {
			h.Set(item[1], "1")
		} else {
			h.Set(item[0], item[2])
		}
	}
	h.Del("X-No-Authorization")
	h.Del("X-Refresh-Token")
	if c.RefreshToken != "" {
		h.Set("X-Refresh-Token", c.RefreshToken)
	}
	h.Set("X-Product", "SaaS")
}

func (s *Service) request(ctx context.Context, client *http.Client, method, path string, headers http.Header, body []byte) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(s.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("invalid CodeBuddy endpoint")
	}
	ApplySourceHeaders(req.Header)
	for name, values := range headers {
		req.Header[name] = append([]string(nil), values...)
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("CodeBuddy upstream request failed")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if err != nil {
		return nil, errors.New("read CodeBuddy upstream response failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{Status: resp.StatusCode}
	}
	var envelope struct {
		Code *int            `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if len(raw) > 1<<20 || json.Unmarshal(raw, &envelope) != nil || envelope.Code == nil {
		return nil, errors.New("invalid CodeBuddy upstream response")
	}
	if *envelope.Code != 0 {
		return nil, &APIError{Status: resp.StatusCode, Code: *envelope.Code}
	}
	return envelope.Data, nil
}

// FetchModels reads the account's live cloud product catalog, as CodeBuddy CLI
// 2.148.0 CloudProductProvider does. It never falls back to a compiled catalog.
func (s *Service) FetchModels(ctx context.Context, credential *Credential) ([]string, error) {
	if credential == nil {
		return nil, errors.New("CodeBuddy model discovery requires credentials")
	}
	c := *credential
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	headers := make(http.Header)
	ApplyCredentialHeaders(headers, &c)
	headers.Set("User-Agent", "CLI/2.148.0 CodeBuddy/2.148.0")
	raw, err := s.request(ctx, s.Client, http.MethodGet, "/v3/config", headers, nil)
	if err != nil {
		return nil, err
	}
	var catalog struct {
		Agents []struct {
			Name   string   `json:"name"`
			Models []string `json:"models"`
		} `json:"agents"`
		Models []struct {
			ID       string `json:"id"`
			Disabled bool   `json:"disabled"`
		} `json:"models"`
		AvailableModels []string `json:"availableModels"`
	}
	if json.Unmarshal(raw, &catalog) != nil {
		return nil, errors.New("invalid CodeBuddy model catalog")
	}
	var cliModels []string
	for _, agent := range catalog.Agents {
		if agent.Name == "cli" {
			cliModels = agent.Models
			break
		}
	}
	if len(cliModels) == 0 {
		return nil, errors.New("CodeBuddy model catalog contains no CLI models; check account authorization")
	}
	names := make([]string, 0, len(catalog.Models))
	seen := make(map[string]struct{}, len(catalog.Models))
	for _, entry := range catalog.Models {
		name := strings.TrimSpace(entry.ID)
		if name == "" || entry.Disabled || !slices.Contains(cliModels, name) {
			continue
		}
		if len(catalog.AvailableModels) > 0 && !slices.Contains(catalog.AvailableModels, name) {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, errors.New("CodeBuddy model catalog contains no enabled models; check account authorization")
	}
	return names, nil
}

// Login keeps cookies isolated for the lifetime of a single authorization.
type Login struct {
	State  string
	URL    string
	client *http.Client
}

// Start creates an isolated browser authorization session.
func (s *Service) Start(ctx context.Context) (*Login, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	client := *s.Client
	client.Jar = jar
	raw, err := s.request(ctx, &client, http.MethodPost, "/v2/plugin/auth/state?platform=CLI", nil, []byte("{}"))
	if err != nil {
		return nil, err
	}
	var state struct {
		State string `json:"state"`
		URL   string `json:"authUrl"`
	}
	if json.Unmarshal(raw, &state) != nil || state.State == "" {
		return nil, errors.New("CodeBuddy login response has no state")
	}
	u, err := url.Parse(state.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return nil, errors.New("CodeBuddy login response has invalid URL")
	}
	return &Login{State: state.State, URL: state.URL, client: &client}, nil
}

type tokenData struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

func (s *Service) tokenCredential(raw []byte, current *Credential) (*Credential, error) {
	var tok tokenData
	if json.Unmarshal(raw, &tok) != nil || tok.AccessToken == "" || tok.ExpiresIn <= 0 || tok.ExpiresIn > 10*365*24*60*60 {
		return nil, errors.New("CodeBuddy token response is invalid")
	}
	c := &Credential{}
	if current != nil {
		*c = *current
	}
	c.AccessToken, c.ExpiresAt = tok.AccessToken, s.Now().Unix()+tok.ExpiresIn
	if tok.RefreshToken != "" {
		c.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		c.Domain = tok.Domain
	}
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// Poll returns nil, nil only for the documented pending-login business code.
func (s *Service) Poll(ctx context.Context, login *Login) (*Credential, error) {
	query := url.QueryEscape(login.State)
	raw, err := s.request(ctx, login.client, http.MethodGet, "/v2/plugin/auth/token?state="+query, nil, nil)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == 11217 {
			return nil, nil
		}
		return nil, err
	}
	c, err := s.tokenCredential(raw, nil)
	if err != nil {
		return nil, err
	}
	account, err := s.request(ctx, login.client, http.MethodGet, "/v2/plugin/login/account?state="+query, http.Header{"Authorization": {"Bearer " + c.AccessToken}}, nil)
	if err != nil {
		return nil, err
	}
	var identity struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	if json.Unmarshal(account, &identity) != nil {
		return nil, errors.New("invalid CodeBuddy account response")
	}
	c.UID, c.EnterpriseID, c.Nickname = identity.UID, identity.EnterpriseID, identity.Nickname
	if err := c.Normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// Refresh rotates tokens while retaining account identity and omitted fields.
func (s *Service) Refresh(ctx context.Context, c *Credential) (*Credential, error) {
	if c.RefreshToken == "" {
		return nil, ErrCannotRefresh
	}
	h := http.Header{"X-Refresh-Token": {c.RefreshToken}, "X-Auth-Refresh-Source": {"workbuddy"}}
	if c.EnterpriseID != "" {
		h.Set("X-Enterprise-Id", c.EnterpriseID)
	}
	raw, err := s.request(ctx, s.Client, http.MethodPost, "/v2/plugin/auth/token/refresh", h, nil)
	if err != nil {
		return nil, err
	}
	return s.tokenCredential(raw, c)
}
