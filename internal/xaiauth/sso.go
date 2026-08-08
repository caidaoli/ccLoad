package xaiauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

// ErrSSOUnauthorized reports that an imported xAI SSO cookie is not authenticated.
var ErrSSOUnauthorized = errors.New("xAI SSO unauthorized")

type ssoFlow struct {
	client *http.Client
	jar    http.CookieJar
}

// ConvertSSO exchanges an in-memory xAI SSO cookie for an OAuth credential.
func (s *Service) ConvertSSO(ctx context.Context, cookie string) (*Credential, error) {
	cookie = normalizeSSOCookie(cookie)
	if cookie == "" {
		return nil, ErrSSOUnauthorized
	}
	if ctx == nil {
		ctx = context.Background()
	}
	conversionCtx, cancel := context.WithTimeout(ctx, SSOConversionTimeout)
	defer cancel()
	client := *s.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client.Jar = nil
	jar, err := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	if err != nil {
		return nil, errors.New("create xAI SSO cookie jar")
	}
	accountsURL, _ := url.Parse(SSOAccountsURL)
	jar.SetCookies(accountsURL, []*http.Cookie{
		{Name: "sso", Value: cookie, Domain: ".x.ai", Path: "/", Secure: true, HttpOnly: true},
		{Name: "sso-rw", Value: cookie, Domain: ".x.ai", Path: "/", Secure: true, HttpOnly: true},
	})
	flow := &ssoFlow{client: &client, jar: jar}
	status, finalURL, _, err := flow.do(conversionCtx, http.MethodGet, SSOAccountsURL, nil, true)
	if err != nil {
		return nil, err
	}
	if status == http.StatusUnauthorized || strings.Contains(finalURL, "sign-in") || strings.Contains(finalURL, "sign-up") {
		return nil, ErrSSOUnauthorized
	}
	if status < 200 || status >= 400 {
		return nil, fmt.Errorf("validate xAI SSO: HTTP %d", status)
	}
	status, _, body, err := flow.do(conversionCtx, http.MethodPost, SSODeviceURL, url.Values{"client_id": {ClientID}, "scope": {SSOScope}}, false)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("start xAI SSO device flow: HTTP %d", status)
	}
	var device struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		Interval                int    `json:"interval"`
		ExpiresIn               int    `json:"expires_in"`
	}
	if json.Unmarshal(body, &device) != nil || strings.TrimSpace(device.DeviceCode) == "" || strings.TrimSpace(device.UserCode) == "" {
		return nil, errors.New("xAI SSO device response is incomplete")
	}
	if device.ExpiresIn <= 0 {
		return nil, errors.New("xAI SSO device response has invalid expires_in")
	}
	deviceDeadline := time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)
	verificationURL, err := validateAuthURL(device.VerificationURIComplete)
	if err != nil {
		return nil, errors.New("xAI SSO verification URL has an untrusted origin")
	}
	status, _, _, err = flow.do(conversionCtx, http.MethodGet, verificationURL, nil, false)
	if err != nil || status < 200 || status >= 400 {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("open xAI SSO verification: HTTP %d", status)
	}
	status, finalURL, _, err = flow.do(conversionCtx, http.MethodPost, SSOVerifyURL, url.Values{"user_code": {device.UserCode}}, false)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 400 || !strings.Contains(finalURL, "consent") {
		return nil, errors.New("xAI SSO verification did not reach consent")
	}
	status, finalURL, _, err = flow.do(conversionCtx, http.MethodPost, SSOApproveURL, url.Values{"user_code": {device.UserCode}, "action": {"allow"}, "principal_type": {"User"}, "principal_id": {""}}, false)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 400 || !strings.Contains(finalURL, "done") {
		return nil, errors.New("xAI SSO approval did not complete")
	}
	interval := time.Duration(device.Interval) * time.Second
	if interval <= 0 {
		interval = defaultPollInterval
	}
	deadline, deviceOwnsDeadline := ssoPollDeadline(conversionCtx, deviceDeadline)
	pollCtx, pollCancel := context.WithDeadline(conversionCtx, deadline)
	defer pollCancel()
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, ssoPollDeadlineError(pollCtx, deviceOwnsDeadline)
		}
		wait := interval
		if wait > remaining {
			wait = remaining
		}
		if err := sleepContext(pollCtx, wait); err != nil {
			return nil, ssoPollDeadlineError(pollCtx, deviceOwnsDeadline)
		}
		if !time.Now().Before(deadline) {
			return nil, ssoPollDeadlineError(pollCtx, deviceOwnsDeadline)
		}
		status, _, tokenBody, err := flow.do(pollCtx, http.MethodPost, SSOTokenURL, url.Values{"grant_type": {DeviceCodeGrantType}, "client_id": {ClientID}, "device_code": {device.DeviceCode}}, false)
		if err != nil {
			if pollCtx.Err() != nil {
				return nil, ssoPollDeadlineError(pollCtx, deviceOwnsDeadline)
			}
			return nil, err
		}
		credential, code, err := credentialFromTokenBody(tokenBody, status)
		if err == nil {
			return credential, nil
		}
		switch code {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "access_denied":
			return nil, ErrAccessDenied
		case "expired_token":
			return nil, ErrDeviceExpired
		default:
			return nil, err
		}
	}
}

func ssoPollDeadline(ctx context.Context, deviceDeadline time.Time) (time.Time, bool) {
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(SSOConversionTimeout)
	}
	if deviceDeadline.IsZero() {
		return deadline, false
	}
	if deviceDeadline.Before(deadline) {
		return deviceDeadline, true
	}
	return deadline, false
}

func ssoPollDeadlineError(ctx context.Context, deviceOwnsDeadline bool) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return context.Canceled
	}
	if deviceOwnsDeadline {
		return ErrDeviceExpired
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return context.DeadlineExceeded
}

func (f *ssoFlow) do(ctx context.Context, method, endpoint string, form url.Values, allowAccounts bool) (int, string, []byte, error) {
	currentURL := endpoint
	currentMethod := method
	currentForm := form
	for redirects := 0; redirects <= 8; redirects++ {
		if !trustedSSOURL(currentURL, allowAccounts) {
			return 0, "", nil, errors.New("xAI SSO URL has an untrusted origin")
		}
		var requestBody io.Reader
		if currentForm != nil {
			requestBody = strings.NewReader(currentForm.Encode())
		}
		req, err := http.NewRequestWithContext(ctx, currentMethod, currentURL, requestBody)
		if err != nil {
			return 0, "", nil, errors.New("build xAI SSO request")
		}
		req.Header.Set("Accept", "application/json, text/html;q=0.9, */*;q=0.8")
		req.Header.Set("User-Agent", "Mozilla/5.0")
		for _, cookie := range f.jar.Cookies(req.URL) {
			req.AddCookie(cookie)
		}
		if currentForm != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := f.client.Do(req)
		if err != nil {
			return 0, "", nil, fmt.Errorf("xAI SSO request failed: %w", err)
		}
		f.captureCookies(req.URL, resp)
		data, readErr := io.ReadAll(io.LimitReader(resp.Body, MaxSSOResponseBytes+1))
		_ = resp.Body.Close()
		if readErr != nil {
			return resp.StatusCode, currentURL, nil, errors.New("read xAI SSO response")
		}
		if len(data) > MaxSSOResponseBytes {
			return resp.StatusCode, currentURL, nil, errors.New("xAI SSO response exceeds 2 MiB")
		}
		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			return resp.StatusCode, currentURL, data, nil
		}
		location := strings.TrimSpace(resp.Header.Get("Location"))
		if location == "" {
			return resp.StatusCode, currentURL, nil, errors.New("xAI SSO redirect has no location")
		}
		base, _ := url.Parse(currentURL)
		next, err := url.Parse(location)
		if err != nil {
			return resp.StatusCode, currentURL, nil, errors.New("xAI SSO redirect is invalid")
		}
		currentURL = base.ResolveReference(next).String()
		if !trustedSSOURL(currentURL, allowAccounts) {
			return resp.StatusCode, currentURL, nil, errors.New("xAI SSO redirected to an untrusted origin")
		}
		if resp.StatusCode == http.StatusSeeOther || ((resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound) && currentMethod != http.MethodGet && currentMethod != http.MethodHead) {
			currentMethod = http.MethodGet
			currentForm = nil
		}
	}
	return 0, currentURL, nil, errors.New("xAI SSO redirected too many times")
}

func trustedSSOURL(raw string, allowAccounts bool) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Fragment != "" {
		return false
	}
	if parsed.Host == "auth.x.ai" {
		return true
	}
	return allowAccounts && parsed.Host == "accounts.x.ai"
}

func (f *ssoFlow) captureCookies(requestURL *url.URL, response *http.Response) {
	cookies := response.Cookies()
	accepted := cookies[:0]
	for _, cookie := range cookies {
		name, value := strings.TrimSpace(cookie.Name), strings.TrimSpace(cookie.Value)
		if name == "" || len(name) > 128 || len(value) > 16384 || strings.ContainsAny(name+value, "\r\n\x00") {
			continue
		}
		accepted = append(accepted, cookie)
	}
	f.jar.SetCookies(requestURL, accepted)
}

func normalizeSSOCookie(value string) string {
	value = strings.NewReplacer("\r", "", "\n", "", "\x00", "").Replace(strings.TrimSpace(value))
	if strings.HasPrefix(strings.ToLower(value), "cookie:") {
		value = strings.TrimSpace(value[len("cookie:"):])
	}
	for _, part := range strings.Split(value, ";") {
		name, token, found := strings.Cut(strings.TrimSpace(part), "=")
		if found && (strings.EqualFold(strings.TrimSpace(name), "sso") || strings.EqualFold(strings.TrimSpace(name), "sso-rw")) {
			return strings.TrimSpace(token)
		}
	}
	if token, _, found := strings.Cut(value, ";"); found {
		return strings.TrimSpace(token)
	}
	return value
}

func credentialFromTokenBody(body []byte, status int) (*Credential, string, error) {
	var token struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
		Error        string `json:"error"`
	}
	if json.Unmarshal(body, &token) != nil {
		return nil, "", errors.New("decode xAI SSO token response")
	}
	code := safeOAuthErrorCode(token.Error)
	if status < 200 || status >= 300 || code != "" {
		if code == "" {
			code = "http_error"
		}
		return nil, code, fmt.Errorf("xAI SSO token endpoint returned HTTP %d (%s)", status, code)
	}
	if strings.TrimSpace(token.AccessToken) == "" || strings.TrimSpace(token.RefreshToken) == "" || token.ExpiresIn <= 0 {
		return nil, "", errors.New("xAI SSO token response is incomplete")
	}
	now := time.Now().UTC()
	credential := &Credential{Type: ChannelType, AuthKind: "oauth", AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, IDToken: token.IDToken, TokenType: token.TokenType, ExpiresIn: token.ExpiresIn, LastRefresh: now.Format(time.RFC3339), Expired: now.Add(time.Duration(token.ExpiresIn) * time.Second).Format(time.RFC3339), TokenEndpoint: TokenURL, ClientID: ClientID, Scope: token.Scope}
	identity := credential.Identity()
	credential.Email, credential.Subject = identity.Email, identity.Subject
	return credential, "", nil
}
