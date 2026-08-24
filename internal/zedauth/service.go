package zedauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"
)

// Service calls the Zed account, token, model, and usage endpoints.
type Service struct {
	Client         *http.Client
	LLMTokensURL   string
	ModelsURL      string
	CurrentUserURL string
	Now            func() time.Time
}

// Usage is the numeric portion of Zed account usage when the plan exposes one.
type Usage struct {
	PlanType          string
	Used              *int64
	Limit             *int64
	SubscriptionEnd   string
	UsageBasedBilling bool
	Overdue           bool
	AccountTooYoung   bool
}

// Account is the identity and subscription state returned by /client/users/me.
type Account struct {
	Username        string
	GitHubUserLogin string
	Usage           Usage
}

type responseError struct {
	operation string
	status    int
	body      string
}

func (e *responseError) Error() string {
	return fmt.Sprintf("zed %s returned HTTP %d", e.operation, e.status)
}
func (e *responseError) StatusCode() int              { return e.status }
func (e *responseError) UpstreamResponseBody() string { return e.body }

// NewService creates a Zed service using client or the default HTTP client.
func NewService(client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	return &Service{
		Client: client, LLMTokensURL: LLMTokensURL, ModelsURL: ModelsURL,
		CurrentUserURL: CurrentUserURL, Now: time.Now,
	}
}

// UserAgent returns the native Zed client identity used for account requests.
func UserAgent() string {
	return fmt.Sprintf("Zed/%s (%s; %s)", ZedVersion, zedOS(runtime.GOOS), zedArch(runtime.GOARCH))
}

func zedOS(goos string) string {
	if goos == "darwin" {
		return "macos"
	}
	return goos
}

func zedArch(goarch string) string {
	switch goarch {
	case "386":
		return "x86"
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "loong64":
		return "loongarch64"
	case "ppc64", "ppc64le":
		return "powerpc64"
	case "wasm":
		return "wasm32"
	default:
		return goarch
	}
}

// MintLLMToken exchanges the native credential for a short-lived inference JWT.
func (s *Service) MintLLMToken(ctx context.Context, credential *Credential) (*Credential, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if credential == nil {
		return nil, errors.New("zed credential is required")
	}
	body, err := s.do(ctx, http.MethodPost, s.LLMTokensURL, credential.NativeAuthorization(), credential.SystemID, nil, "LLM token mint")
	if err != nil {
		return nil, err
	}
	var payload struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode zed LLM token response: %w", err)
	}
	payload.Token = strings.TrimSpace(payload.Token)
	expiresAt, err := JWTExpiry(payload.Token)
	if err != nil {
		return nil, err
	}
	refreshed := CloneCredential(credential)
	refreshed.AccessToken = payload.Token
	refreshed.ExpiresAt = expiresAt
	refreshed.LastRefresh = s.now().UTC().Format(time.RFC3339)
	return refreshed, nil
}

// FetchModels returns enabled models whose provider wire is implemented by ccLoad.
func (s *Service) FetchModels(ctx context.Context, credential *Credential) ([]string, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if credential == nil || strings.TrimSpace(credential.AccessToken) == "" {
		return nil, errors.New("zed LLM token is required")
	}
	body, err := s.do(ctx, http.MethodGet, s.ModelsURL, "Bearer "+credential.AccessToken, "", nil, "model catalog")
	if err != nil {
		return nil, err
	}
	var payload struct {
		Models []struct {
			ID         string `json:"id"`
			Provider   string `json:"provider"`
			IsDisabled bool   `json:"is_disabled"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode zed model catalog: %w", err)
	}
	seen := make(map[string]struct{})
	models := make([]string, 0, len(payload.Models))
	for _, entry := range payload.Models {
		id := strings.TrimSpace(entry.ID)
		provider, supported := ProviderForModel(id)
		if id == "" || entry.IsDisabled || !supported || strings.TrimSpace(entry.Provider) != provider {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		models = append(models, id)
	}
	if len(models) == 0 {
		return nil, errors.New("zed model catalog has no supported models")
	}
	return models, nil
}

// FetchAccount reads the authenticated Zed identity and subscription state.
func (s *Service) FetchAccount(ctx context.Context, credential *Credential) (*Account, error) {
	if err := s.validate(); err != nil {
		return nil, err
	}
	if credential == nil {
		return nil, errors.New("zed credential is required")
	}
	body, err := s.do(ctx, http.MethodGet, s.CurrentUserURL, credential.NativeAuthorization(), credential.SystemID, nil, "current user")
	if err != nil {
		return nil, err
	}
	var root struct {
		User struct {
			Username    string `json:"username"`
			GitHubLogin string `json:"github_login"`
		} `json:"user"`
		Plan struct {
			PlanV3                     string `json:"plan_v3"`
			PlanV2                     string `json:"plan_v2"`
			Plan                       string `json:"plan"`
			HasOverdueInvoices         bool   `json:"has_overdue_invoices"`
			IsAccountTooYoung          bool   `json:"is_account_too_young"`
			IsUsageBasedBillingEnabled bool   `json:"is_usage_based_billing_enabled"`
			SubscriptionPeriod         struct {
				EndedAt string `json:"ended_at"`
			} `json:"subscription_period"`
			Usage struct {
				ModelRequests struct {
					Used  *int64          `json:"used"`
					Limit json.RawMessage `json:"limit"`
				} `json:"model_requests"`
			} `json:"usage"`
		} `json:"plan"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("decode zed current user response: %w", err)
	}
	planType := firstNonEmpty(root.Plan.PlanV3, root.Plan.PlanV2, root.Plan.Plan, "unknown")
	account := &Account{
		Username: strings.TrimSpace(root.User.Username), GitHubUserLogin: strings.TrimSpace(root.User.GitHubLogin),
	}
	account.Usage = Usage{
		PlanType: planType, Used: root.Plan.Usage.ModelRequests.Used,
		SubscriptionEnd:   strings.TrimSpace(root.Plan.SubscriptionPeriod.EndedAt),
		UsageBasedBilling: root.Plan.IsUsageBasedBillingEnabled,
		Overdue:           root.Plan.HasOverdueInvoices, AccountTooYoung: root.Plan.IsAccountTooYoung,
	}
	account.Usage.Limit = parseUsageLimit(root.Plan.Usage.ModelRequests.Limit)
	return account, nil
}

// FetchUsage reads numeric model-request usage when Zed exposes a plan limit.
func (s *Service) FetchUsage(ctx context.Context, credential *Credential) (*Usage, error) {
	account, err := s.FetchAccount(ctx, credential)
	if err != nil {
		return nil, err
	}
	return &account.Usage, nil
}

func parseUsageLimit(raw json.RawMessage) *int64 {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var number int64
	if json.Unmarshal(raw, &number) == nil {
		return &number
	}
	var object struct {
		Limited *int64 `json:"limited"`
	}
	if json.Unmarshal(raw, &object) == nil {
		return object.Limited
	}
	return nil
}

func (s *Service) do(ctx context.Context, method, target, authorization, systemID string, payload []byte, operation string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requestCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, method, target, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build zed %s request: %w", operation, err)
	}
	request.Header.Set("Authorization", authorization)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", UserAgent())
	if systemID != "" {
		request.Header.Set("x-zed-system-id", systemID)
	}
	if operation == "model catalog" {
		request.Header.Set("x-zed-version", ZedVersion)
	}
	response, err := s.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("zed %s request: %w", operation, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read zed %s response: %w", operation, err)
	}
	if len(body) > maxResponseSize {
		return nil, fmt.Errorf("zed %s response exceeds %d bytes", operation, maxResponseSize)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, &responseError{operation: operation, status: response.StatusCode, body: strings.TrimSpace(string(body))}
	}
	return body, nil
}

func (s *Service) validate() error {
	if s == nil || s.Client == nil || s.LLMTokensURL == "" || s.ModelsURL == "" || s.CurrentUserURL == "" {
		return errors.New("zed service is unavailable")
	}
	return nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
