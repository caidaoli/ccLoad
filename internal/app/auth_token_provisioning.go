package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

const (
	// EnvProvisionedAuthTokens contains comma-separated plaintext API tokens to seed on startup.
	EnvProvisionedAuthTokens = "CCLOAD_API_TOKENS"
	// EnvProvisionedAuthTokensAlias keeps the shorter variable proposed by Docker examples compatible.
	EnvProvisionedAuthTokensAlias = "API_TOKENS"

	authTokenProvisionTimeout = 10 * time.Second
)

type provisionedAuthToken struct {
	PlainToken  string
	Description string
}

// AuthTokenProvisionResult summarizes startup API token provisioning.
type AuthTokenProvisionResult struct {
	Configured int
	Created    int
}

func parseProvisionedAuthTokens(raw string) ([]provisionedAuthToken, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	entries := strings.Split(raw, ",")
	tokens := make([]provisionedAuthToken, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for i, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, fmt.Errorf("empty API token entry at position %d", i+1)
		}

		tokenPart, descPart, hasDescription := strings.Cut(entry, "|")
		if strings.Contains(descPart, "|") {
			return nil, fmt.Errorf("invalid API token entry at position %d: too many separators", i+1)
		}

		plainToken := strings.TrimSpace(tokenPart)
		if plainToken == "" {
			return nil, fmt.Errorf("empty API token at position %d", i+1)
		}
		if _, ok := seen[plainToken]; ok {
			return nil, fmt.Errorf("duplicate API token at position %d", i+1)
		}
		seen[plainToken] = struct{}{}

		description := "Provisioned token " + model.MaskToken(plainToken)
		if hasDescription {
			description = strings.TrimSpace(descPart)
			if description == "" {
				return nil, fmt.Errorf("empty API token description at position %d", i+1)
			}
		}

		tokens = append(tokens, provisionedAuthToken{
			PlainToken:  plainToken,
			Description: description,
		})
	}

	return tokens, nil
}

func provisionedAuthTokensEnvValue() (string, error) {
	primary := strings.TrimSpace(os.Getenv(EnvProvisionedAuthTokens))
	alias := strings.TrimSpace(os.Getenv(EnvProvisionedAuthTokensAlias))
	if primary != "" && alias != "" && primary != alias {
		return "", fmt.Errorf("%s and %s are both set with different values", EnvProvisionedAuthTokens, EnvProvisionedAuthTokensAlias)
	}
	if primary != "" {
		return primary, nil
	}
	return alias, nil
}

// ProvisionAuthTokensFromEnv provisions API tokens from supported environment variables.
func ProvisionAuthTokensFromEnv(ctx context.Context, store storage.Store) (AuthTokenProvisionResult, error) {
	raw, err := provisionedAuthTokensEnvValue()
	if err != nil {
		return AuthTokenProvisionResult{}, err
	}
	return ProvisionAuthTokens(ctx, store, raw)
}

// ProvisionAuthTokens creates missing API tokens from a comma-separated plaintext token list.
func ProvisionAuthTokens(ctx context.Context, store storage.Store, raw string) (AuthTokenProvisionResult, error) {
	tokens, err := parseProvisionedAuthTokens(raw)
	if err != nil {
		return AuthTokenProvisionResult{}, err
	}

	result := AuthTokenProvisionResult{Configured: len(tokens)}
	for i, token := range tokens {
		authToken := &model.AuthToken{
			Token:       model.HashToken(token.PlainToken),
			Description: token.Description,
			CreatedAt:   time.Now(),
			IsActive:    true,
		}
		created, err := store.EnsureAuthToken(ctx, authToken)
		if err != nil {
			return result, fmt.Errorf("provision API token at position %d: %w", i+1, err)
		}
		if created {
			result.Created++
		}
	}

	return result, nil
}
