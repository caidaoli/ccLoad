package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/codexauth"

	"github.com/gin-gonic/gin"
)

const (
	oauthCredentialProviderAuto       = "auto"
	maxOAuthCredentialImportBytes     = 1 << 20
	oauthCredentialUnknownTypeMessage = "credential type could not be determined"
)

type oauthCredentialImportResult struct {
	FileName    string `json:"file_name"`
	ChannelName string `json:"channel_name,omitempty"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
}

type oauthCredentialImportSummary struct {
	Created int                           `json:"created"`
	Skipped int                           `json:"skipped"`
	Failed  int                           `json:"failed"`
	Results []oauthCredentialImportResult `json:"results"`
}

func readOAuthCredentialFile(file *multipart.FileHeader) ([]byte, error) {
	if file == nil || file.Size <= 0 || file.Size > maxOAuthCredentialImportBytes {
		return nil, errors.New("credential file size is invalid")
	}
	opened, err := file.Open()
	if err != nil {
		return nil, errors.New("open credential file failed")
	}
	raw, readErr := io.ReadAll(io.LimitReader(opened, maxOAuthCredentialImportBytes+1))
	closeErr := opened.Close()
	if readErr != nil || len(raw) > maxOAuthCredentialImportBytes {
		return nil, errors.New("failed to read credential")
	}
	if closeErr != nil {
		return nil, errors.New("close credential file failed")
	}
	return raw, nil
}

func normalizeOAuthCredentialProvider(provider string) (string, error) {
	switch normalized := strings.ToLower(strings.TrimSpace(provider)); normalized {
	case "", oauthCredentialProviderAuto:
		return oauthCredentialProviderAuto, nil
	case codexauth.ChannelType, antigravityauth.ChannelType:
		return normalized, nil
	default:
		return "", fmt.Errorf("unsupported credential provider %q", normalized)
	}
}

func parseOAuthPriorityIncrement(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	increment, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, errors.New("priority_increment must be one of 0, 10, 20, or 50")
	}
	switch increment {
	case 0, 10, 20, 50:
		return increment, nil
	default:
		return 0, errors.New("priority_increment must be one of 0, 10, 20, or 50")
	}
}

func detectOAuthCredentialProvider(raw []byte) (string, error) {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if err := decoder.Decode(&fields); err != nil {
		return "", fmt.Errorf("decode credential: %w", err)
	}
	if fields == nil {
		return "", errors.New("credential must be a JSON object")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", errors.New("credential contains trailing JSON")
	}

	if rawType, exists := fields["type"]; exists {
		var credentialType string
		if err := json.Unmarshal(rawType, &credentialType); err != nil {
			return "", errors.New("credential type must be a string")
		}
		switch normalized := strings.ToLower(strings.TrimSpace(credentialType)); normalized {
		case codexauth.ChannelType, antigravityauth.ChannelType:
			return normalized, nil
		case "":
			// Empty and omitted types use the same field-based inference.
		default:
			return "", nil
		}
	}

	codexFields := hasAnyJSONField(fields, "id_token", "account_id", "plan_type", "last_refresh")
	antigravityFields := hasAnyJSONField(fields, "project_id", "expires_in", "timestamp")
	if codexFields == antigravityFields {
		return "", nil
	}
	if codexFields {
		return codexauth.ChannelType, nil
	}
	return antigravityauth.ChannelType, nil
}

func hasAnyJSONField(fields map[string]json.RawMessage, names ...string) bool {
	for _, name := range names {
		if _, exists := fields[name]; exists {
			return true
		}
	}
	return false
}

// HandleImportOAuthCredentials imports mixed OAuth credential files. The
// provider form field defaults to automatic detection.
func (s *Server) HandleImportOAuthCredentials(c *gin.Context) {
	s.handleImportOAuthCredentials(c, "")
}

func (s *Server) handleImportOAuthCredentials(c *gin.Context, forcedProvider string) {
	form, err := c.MultipartForm()
	if err != nil {
		RespondErrorMsg(c, http.StatusBadRequest, "credential files are required")
		return
	}
	files := form.File["files"]
	if len(files) == 0 {
		RespondErrorMsg(c, http.StatusBadRequest, "credential files are required")
		return
	}

	providerValue := forcedProvider
	if providerValue == "" {
		providerValue = firstMultipartValue(form.Value["provider"])
	}
	provider, err := normalizeOAuthCredentialProvider(providerValue)
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}
	priorityIncrement, err := parseOAuthPriorityIncrement(firstMultipartValue(form.Value["priority_increment"]))
	if err != nil {
		RespondError(c, http.StatusBadRequest, err)
		return
	}

	summary := oauthCredentialImportSummary{Results: make([]oauthCredentialImportResult, 0, len(files))}
	nextPriority := 0
	for _, file := range files {
		fileName := ""
		if file != nil {
			fileName = file.Filename
		}
		result := oauthCredentialImportResult{FileName: fileName}
		raw, readErr := readOAuthCredentialFile(file)
		if readErr != nil {
			result.Status, result.Error = "failed", readErr.Error()
			summary.Failed++
			summary.Results = append(summary.Results, result)
			continue
		}

		credentialProvider := provider
		if credentialProvider == oauthCredentialProviderAuto {
			credentialProvider, err = detectOAuthCredentialProvider(raw)
			if err != nil {
				result.Status, result.Error = "failed", err.Error()
				summary.Failed++
				summary.Results = append(summary.Results, result)
				continue
			}
			if credentialProvider == "" {
				result.Status, result.Error = "skipped", oauthCredentialUnknownTypeMessage
				summary.Skipped++
				summary.Results = append(summary.Results, result)
				continue
			}
		}

		channelName, created, importErr := s.importOAuthCredential(c, credentialProvider, raw, nextPriority)
		switch {
		case importErr != nil:
			result.Status, result.Error = "failed", importErr.Error()
			summary.Failed++
		case created:
			result.Status, result.ChannelName = "created", channelName
			summary.Created++
			nextPriority += priorityIncrement
		default:
			result.Status, result.ChannelName = "skipped", channelName
			summary.Skipped++
		}
		summary.Results = append(summary.Results, result)
	}
	if summary.Created > 0 {
		s.InvalidateChannelListCache()
	}
	RespondJSON(c, http.StatusOK, summary)
}

func firstMultipartValue(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func (s *Server) importOAuthCredential(c *gin.Context, provider string, raw []byte, priority int) (string, bool, error) {
	switch provider {
	case codexauth.ChannelType:
		credential, err := codexauth.ParseCredential(raw)
		if err != nil {
			return "", false, err
		}
		return createImportedCodexChannel(c.Request.Context(), s.store, credential, priority)
	case antigravityauth.ChannelType:
		credential, err := antigravityauth.ParseCredential(raw)
		if err == nil && (credential.Email == "" || credential.ProjectID == "") {
			if s.antigravityService == nil {
				return "", false, errors.New("antigravity credential completion is unavailable")
			}
			credential, err = s.antigravityService.CompleteCredential(c.Request.Context(), credential)
		}
		if err != nil {
			return "", false, err
		}
		return createImportedAntigravityChannel(c.Request.Context(), s.store, credential, priority)
	default:
		return "", false, fmt.Errorf("unsupported credential provider %q", provider)
	}
}
