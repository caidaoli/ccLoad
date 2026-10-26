// Package vertexauth implements Google Vertex AI service-account credentials.
package vertexauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Vertex AI endpoints and credential identity.
const (
	ChannelType = "vertex"
	// GlobalLocation routes to the location-less aiplatform host. It is also the
	// default for credentials without a location, matching CLIProxyAPI.
	GlobalLocation     = "global"
	DefaultLocation    = GlobalLocation
	CloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
	DefaultTokenURI    = "https://oauth2.googleapis.com/token"
	serviceAccountType = "service_account"
	maxCredentialSize  = 64 << 10
)

var (
	locationPattern  = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$`)
	projectIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.:_-]*$`)
)

// Credential is the CLIProxyAPI Vertex auth file: the verbatim service account
// plus the project and location it routes to.
type Credential struct {
	Type           string         `json:"type"`
	ServiceAccount map[string]any `json:"service_account"`
	ProjectID      string         `json:"project_id"`
	Email          string         `json:"email"`
	Location       string         `json:"location,omitempty"`
}

// ParseCredential accepts either a CLIProxyAPI Vertex auth file or a raw Google
// service account key, and returns the normalized Vertex credential.
func ParseCredential(raw []byte) (*Credential, error) {
	if len(raw) == 0 {
		return nil, errors.New("vertex credential is empty")
	}
	if len(raw) > maxCredentialSize {
		return nil, fmt.Errorf("vertex credential exceeds %d bytes", maxCredentialSize)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode vertex credential: %w", err)
	}
	if payload == nil {
		return nil, errors.New("vertex credential must be a JSON object")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("vertex credential contains trailing JSON")
	}

	credential := &Credential{}
	switch credentialType := strings.ToLower(strings.TrimSpace(stringField(payload, "type"))); credentialType {
	case serviceAccountType:
		credential.ServiceAccount = payload
	case ChannelType, "":
		serviceAccount, ok := payload["service_account"].(map[string]any)
		if !ok {
			return nil, errors.New("vertex credential missing service_account")
		}
		credential.ServiceAccount = serviceAccount
		credential.ProjectID = stringField(payload, "project_id")
		credential.Email = stringField(payload, "email")
		credential.Location = stringField(payload, "location")
	default:
		return nil, fmt.Errorf("unsupported vertex credential type %q", credentialType)
	}
	if err := credential.Normalize(); err != nil {
		return nil, err
	}
	return credential, nil
}

// IsCredentialType reports whether a credential file's type field belongs to Vertex.
func IsCredentialType(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case ChannelType, serviceAccountType:
		return true
	default:
		return false
	}
}

// Normalize validates the credential and canonicalizes it in place.
func (c *Credential) Normalize() error {
	if c == nil {
		return errors.New("vertex credential is nil")
	}
	serviceAccount, err := NormalizeServiceAccountMap(c.ServiceAccount)
	if err != nil {
		return err
	}
	if accountType := stringField(serviceAccount, "type"); accountType != "" && accountType != serviceAccountType {
		return fmt.Errorf("unsupported service account type %q", accountType)
	}
	c.Type = ChannelType
	c.ServiceAccount = serviceAccount
	c.ProjectID = strings.TrimSpace(c.ProjectID)
	if c.ProjectID == "" {
		c.ProjectID = stringField(serviceAccount, "project_id")
	}
	if !projectIDPattern.MatchString(c.ProjectID) {
		return fmt.Errorf("vertex credential has invalid project_id %q", c.ProjectID)
	}
	c.Email = stringField(serviceAccount, "client_email")
	if c.Email == "" {
		return errors.New("service account missing client_email")
	}
	c.Location = strings.ToLower(strings.TrimSpace(c.Location))
	if c.Location == "" {
		c.Location = DefaultLocation
	}
	if !locationPattern.MatchString(c.Location) {
		return fmt.Errorf("vertex credential has invalid location %q", c.Location)
	}
	return nil
}

// JSON encodes the credential for channel storage.
func (c *Credential) JSON() (string, error) {
	if err := c.Normalize(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode vertex credential: %w", err)
	}
	return string(payload), nil
}

// SameAccount reports whether both credentials authenticate as the same
// service account in the same project; key rotation keeps the account.
func (c *Credential) SameAccount(other *Credential) bool {
	return c != nil && other != nil &&
		strings.EqualFold(c.Email, other.Email) && c.ProjectID == other.ProjectID
}

// BaseURL is the publisher endpoint new channels route to. Requests append
// /models/{model}:{action}; the API version lives here, not in the request path.
func (c *Credential) BaseURL() string {
	return PublisherBaseURL(c.ProjectID, c.Location)
}

// PublisherBaseURL builds the Vertex publisher endpoint for a project and location.
func PublisherBaseURL(projectID, location string) string {
	var host string
	switch location {
	case GlobalLocation:
		host = "https://aiplatform.googleapis.com"
	case "us", "eu":
		// Multi-region locations are served only by Regional Endpoint (REP) hosts.
		host = "https://aiplatform." + location + ".rep.googleapis.com"
	default:
		host = "https://" + location + "-aiplatform.googleapis.com"
	}
	return host + "/v1/projects/" + projectID + "/locations/" + location + "/publishers/google"
}

func (c *Credential) tokenURI() string {
	if uri := stringField(c.ServiceAccount, "token_uri"); uri != "" {
		return uri
	}
	return DefaultTokenURI
}

func stringField(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return strings.TrimSpace(value)
}
