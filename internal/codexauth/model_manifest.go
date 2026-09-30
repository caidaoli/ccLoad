package codexauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// ServiceTier is the Codex model manifest's advertised processing option.
type ServiceTier struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ManifestModel retains missing/null, empty, and declared service tier lists.
type ManifestModel struct {
	Slug         string          `json:"slug"`
	ServiceTiers json.RawMessage `json:"service_tiers,omitempty"`
}

// ModelManifest binds capabilities to the account and endpoint that supplied them.
type ModelManifest struct {
	AccountID string          `json:"account_id"`
	UserID    string          `json:"user_id,omitempty"`
	PlanType  string          `json:"plan_type,omitempty"`
	Endpoint  string          `json:"endpoint"`
	SampledAt int64           `json:"sampled_at_unix_nano"`
	Models    []ManifestModel `json:"models"`
}

type modelManifestEndpointError struct{ status int }

func (e *modelManifestEndpointError) Error() string {
	return fmt.Sprintf("Codex model manifest returned HTTP %d", e.status)
}

func (e *modelManifestEndpointError) StatusCode() int { return e.status }

// ModelsEndpoint derives the discovery endpoint identity from the configured
// Codex Responses endpoint. The client version is a request parameter, not part
// of the identity, so a client upgrade does not orphan saved snapshots.
func ModelsEndpoint(baseURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u == nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return "", errors.New("invalid Codex model discovery URL")
	}
	u.Path = strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/responses") + "/models"
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

// FetchModelManifest reads account capabilities only during explicit model discovery.
func FetchModelManifest(ctx context.Context, client *http.Client, endpoint string, credential *Credential) ([]ManifestModel, error) {
	if credential == nil || credential.AccessToken == "" || credential.AccountID == "" {
		return nil, errors.New("model discovery: Codex account credential is required")
	}
	target, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("create Codex model discovery request: %w", err)
	}
	query := target.Query()
	query.Set("client_version", DefaultClientVersion)
	target.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create Codex model discovery request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	req.Header.Set("ChatGPT-Account-Id", credential.AccountID)
	req.Header.Set("User-Agent", DefaultUserAgent)
	req.Header.Set("Version", DefaultClientVersion)
	req.Header.Set("Originator", DefaultOriginator)
	req.Header.Set("Accept", "application/json")
	if client == nil {
		client = http.DefaultClient
	}
	requestClient := *client
	requestClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := requestClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request Codex model manifest: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, &modelManifestEndpointError{status: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCredentialSize+1))
	if err != nil {
		return nil, fmt.Errorf("read Codex model manifest: %w", err)
	}
	if len(body) > maxCredentialSize {
		return nil, errors.New("model discovery: Codex model manifest exceeds size limit")
	}
	var manifest struct {
		Models []ManifestModel `json:"models"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, fmt.Errorf("decode Codex model manifest: %w", err)
	}
	if err := validateManifestModels(manifest.Models); err != nil {
		return nil, err
	}
	return manifest.Models, nil
}

func validateManifestModels(models []ManifestModel) error {
	if len(models) == 0 {
		return errors.New("model discovery: Codex model manifest has no models")
	}
	seen := make(map[string]struct{}, len(models))
	for i := range models {
		entry := &models[i]
		entry.Slug = strings.TrimSpace(entry.Slug)
		if entry.Slug == "" {
			return errors.New("model discovery: Codex model manifest has an empty model slug")
		}
		if _, exists := seen[entry.Slug]; exists {
			return fmt.Errorf("model discovery: Codex model manifest repeats model %q", entry.Slug)
		}
		seen[entry.Slug] = struct{}{}
		value := bytes.TrimSpace(entry.ServiceTiers)
		if len(value) == 0 || bytes.Equal(value, []byte("null")) {
			continue
		}
		var tiers []ServiceTier
		if value[0] != '[' || json.Unmarshal(value, &tiers) != nil {
			return fmt.Errorf("model discovery: Codex model %q has invalid service_tiers", entry.Slug)
		}
		for _, tier := range tiers {
			if strings.TrimSpace(tier.ID) == "" {
				return fmt.Errorf("model discovery: Codex model %q has an empty service tier ID", entry.Slug)
			}
		}
		// Canonicalize whitespace so route agreement does not depend on JSON formatting.
		entry.ServiceTiers, _ = json.Marshal(tiers)
	}
	return nil
}

// Matches rejects capability snapshots after an account, plan, or endpoint change.
// Quota epochs are ledger boundaries: a manual quota reset advances them without
// changing capabilities, and identity restarts clear the snapshot explicitly.
func (m *ModelManifest) Matches(credential *Credential, endpoint string) bool {
	if m == nil || credential == nil {
		return false
	}
	return m.AccountID != "" &&
		m.AccountID == credential.AccountID && m.UserID == credential.ChatGPTUserID &&
		strings.EqualFold(m.PlanType, credential.PlanType) && m.Endpoint == endpoint
}

func (c *Credential) normalizeModelManifest() error {
	manifest := c.ModelManifest
	if manifest == nil {
		return nil
	}
	if !manifest.Matches(c, manifest.Endpoint) {
		c.ModelManifest = nil
		return nil
	}
	if manifest.Endpoint == "" || manifest.SampledAt <= 0 {
		return errors.New("model discovery: Codex credential has invalid model manifest provenance")
	}
	return validateManifestModels(manifest.Models)
}

// Clone returns an independent capability snapshot.
func (m *ModelManifest) Clone() *ModelManifest {
	if m == nil {
		return nil
	}
	clone := *m
	clone.Models = make([]ManifestModel, len(m.Models))
	for i, entry := range m.Models {
		clone.Models[i] = entry
		clone.Models[i].ServiceTiers = append(json.RawMessage(nil), entry.ServiceTiers...)
	}
	return &clone
}
