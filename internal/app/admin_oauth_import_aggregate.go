package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"ccLoad/internal/xaiauth"
)

const (
	cliProxyAPIAuthBundleType    = "cliproxyapi-auth-bundle"
	cliProxyAPIAuthBundleVersion = 1
	sub2APIDataType              = "sub2api-data"
	sub2APIDataVersion           = 1
	sub2APIMillisCutover         = int64(100_000_000_000)
)

func expandOAuthCredentialContainers(
	files []oauthCredentialImportFile,
	provider string,
) ([]oauthCredentialImportFile, error) {
	budget := oauthCredentialImportBudget{}
	for i := range files {
		if err := budget.reserve(uint64(len(files[i].Raw))); err != nil {
			wipeOAuthCredentialImportFiles(files)
			return nil, err
		}
	}

	expanded := make([]oauthCredentialImportFile, 0, len(files))
	for i := range files {
		file := files[i]
		if file.Err != nil {
			expanded = append(expanded, file)
			continue
		}
		items, handled := expandCLIProxyAPIAuthBundle(file, &budget)
		if !handled {
			items, handled = expandSub2APIData(file, &budget)
		}
		if !handled && (provider == oauthCredentialProviderAuto || provider == xaiauth.ChannelType) {
			items, handled = expandXAICredentialContainer(file, provider, &budget)
		}
		if !handled {
			expanded = append(expanded, file)
			continue
		}
		clear(file.Raw)
		expanded = append(expanded, items...)
	}
	sortOAuthCredentialImportFiles(expanded)
	return expanded, nil
}

func expandCLIProxyAPIAuthBundle(
	file oauthCredentialImportFile,
	budget *oauthCredentialImportBudget,
) ([]oauthCredentialImportFile, bool) {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(file.Raw, &probe) != nil ||
		!strings.EqualFold(strings.TrimSpace(probe.Type), cliProxyAPIAuthBundleType) {
		return nil, false
	}

	var bundle struct {
		Type     string          `json:"type"`
		Version  int             `json:"version"`
		Accounts json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(file.Raw, &bundle); err != nil {
		return aggregateContainerFailure(file, fmt.Errorf("invalid CLIProxyAPI auth bundle: %w", err)), true
	}
	if bundle.Version != cliProxyAPIAuthBundleVersion {
		return aggregateContainerFailure(file, fmt.Errorf("unsupported CLIProxyAPI auth bundle version %d", bundle.Version)), true
	}
	accounts, err := newOAuthAggregateArrayDecoder(bundle.Accounts)
	if err != nil {
		return aggregateContainerFailure(file, fmt.Errorf("invalid CLIProxyAPI auth bundle accounts: %w", err)), true
	}
	if !accounts.More() {
		return aggregateContainerFailure(file, errors.New("CLIProxyAPI auth bundle contains no accounts")), true
	}

	snapshot := beginOAuthCredentialContainerExpansion(file, budget)
	items := make([]oauthCredentialImportFile, 0)
	for index := 0; accounts.More(); index++ {
		var accountRaw json.RawMessage
		if err := accounts.Decode(&accountRaw); err != nil {
			return failOAuthCredentialContainerExpansion(file, items, budget, snapshot, fmt.Errorf("decode auth bundle account: %w", err)), true
		}
		fileName := aggregateChildFileName(file.FileName, index)
		sortName := aggregateChildSortName(file.SortName, index)
		baseName, err := cliProxyAPIAuthBundleAccountName(accountRaw)
		if err != nil {
			item := failedOAuthCredentialImportFile(fileName, err)
			if reserveErr := reserveExpandedOAuthCredentialFile(&item, budget); reserveErr != nil {
				return failOAuthCredentialContainerExpansion(file, items, budget, snapshot, reserveErr), true
			}
			items = append(items, item)
			continue
		}
		payload := append([]byte(nil), bytes.TrimSpace(accountRaw)...)
		item := newOAuthCredentialImportFile(fileName, sortName+"\x00"+baseName, payload)
		if err := reserveExpandedOAuthCredentialFile(&item, budget); err != nil {
			clear(payload)
			return failOAuthCredentialContainerExpansion(file, items, budget, snapshot, err), true
		}
		items = append(items, item)
	}
	if err := finishOAuthAggregateArray(accounts); err != nil {
		return failOAuthCredentialContainerExpansion(file, items, budget, snapshot, err), true
	}
	return items, true
}

func cliProxyAPIAuthBundleAccountName(raw json.RawMessage) (string, error) {
	var account map[string]any
	if err := decodeOAuthAggregateJSON(raw, &account); err != nil || account == nil {
		return "", errors.New("account must be an object")
	}
	provider, _ := account["type"].(string)
	provider = sanitizeOAuthCredentialFileToken(provider)
	if provider == "" {
		return "", errors.New("account has no usable type")
	}
	identity := ""
	for _, field := range []string{"email", "account_id", "chatgpt_account_id", "sub", "account_uuid", "local_account_id", "name"} {
		identity, _ = account[field].(string)
		identity = strings.TrimSpace(identity)
		if identity != "" {
			break
		}
	}
	identity = sanitizeOAuthCredentialFileToken(identity)
	if identity == "" {
		return "", errors.New("account has no usable email, account id, subject, account UUID, local account id, or name")
	}
	return provider + "-" + identity + ".json", nil
}

func expandSub2APIData(
	file oauthCredentialImportFile,
	budget *oauthCredentialImportBudget,
) ([]oauthCredentialImportFile, bool) {
	var probe struct {
		Type     string          `json:"type"`
		Accounts json.RawMessage `json:"accounts"`
	}
	if json.Unmarshal(file.Raw, &probe) != nil {
		return nil, false
	}
	containerType := strings.ToLower(strings.TrimSpace(probe.Type))
	typedContainer := containerType == sub2APIDataType
	if !typedContainer && (containerType != "" || probe.Accounts == nil) {
		return nil, false
	}

	var document struct {
		Type       string          `json:"type"`
		Version    int             `json:"version"`
		ExportedAt json.RawMessage `json:"exported_at"`
		Accounts   json.RawMessage `json:"accounts"`
	}
	if err := decodeOAuthAggregateJSON(file.Raw, &document); err != nil {
		return aggregateContainerFailure(file, fmt.Errorf("invalid Sub2API data: %w", err)), true
	}
	if typedContainer && document.Version != sub2APIDataVersion {
		return aggregateContainerFailure(file, fmt.Errorf("unsupported sub2api-data version %d", document.Version)), true
	}
	accounts, err := newOAuthAggregateArrayDecoder(document.Accounts)
	if err != nil {
		return aggregateContainerFailure(file, fmt.Errorf("invalid sub2api accounts: %w", err)), true
	}
	if !accounts.More() {
		return aggregateContainerFailure(file, errors.New("sub2api data contains no accounts")), true
	}
	exportedAt, err := parseOptionalSub2APITimestamp(document.ExportedAt)
	if err != nil {
		return aggregateContainerFailure(file, fmt.Errorf("invalid sub2api exported_at: %w", err)), true
	}

	snapshot := beginOAuthCredentialContainerExpansion(file, budget)
	items := make([]oauthCredentialImportFile, 0)
	seen := make(map[[sha256.Size]byte]struct{})
	for index := 0; accounts.More(); index++ {
		fileName := aggregateChildFileName(file.FileName, index)
		sortName := aggregateChildSortName(file.SortName, index)
		var rawAccount any
		if err := accounts.Decode(&rawAccount); err != nil {
			return failOAuthCredentialContainerExpansion(file, items, budget, snapshot, fmt.Errorf("decode sub2api account: %w", err)), true
		}
		account, ok := rawAccount.(map[string]any)
		if !ok || account == nil {
			item := failedOAuthCredentialImportFile(fileName, errors.New("account must be an object"))
			if reserveErr := reserveExpandedOAuthCredentialFile(&item, budget); reserveErr != nil {
				return failOAuthCredentialContainerExpansion(file, items, budget, snapshot, reserveErr), true
			}
			items = append(items, item)
			continue
		}
		converted, fingerprint, err := convertSub2APICredential(account, exportedAt)
		if err != nil {
			item := failedOAuthCredentialImportFile(fileName, err)
			if reserveErr := reserveExpandedOAuthCredentialFile(&item, budget); reserveErr != nil {
				return failOAuthCredentialContainerExpansion(file, items, budget, snapshot, reserveErr), true
			}
			items = append(items, item)
			continue
		}
		if _, duplicate := seen[fingerprint]; duplicate {
			item := failedOAuthCredentialImportFile(fileName, errors.New("duplicate credential record"))
			if reserveErr := reserveExpandedOAuthCredentialFile(&item, budget); reserveErr != nil {
				return failOAuthCredentialContainerExpansion(file, items, budget, snapshot, reserveErr), true
			}
			items = append(items, item)
			continue
		}
		seen[fingerprint] = struct{}{}
		payload, err := json.Marshal(converted)
		if err != nil {
			item := failedOAuthCredentialImportFile(fileName, fmt.Errorf("encode converted credential: %w", err))
			if reserveErr := reserveExpandedOAuthCredentialFile(&item, budget); reserveErr != nil {
				return failOAuthCredentialContainerExpansion(file, items, budget, snapshot, reserveErr), true
			}
			items = append(items, item)
			continue
		}
		item := newOAuthCredentialImportFile(fileName, sortName, payload)
		if err := reserveExpandedOAuthCredentialFile(&item, budget); err != nil {
			clear(payload)
			return failOAuthCredentialContainerExpansion(file, items, budget, snapshot, err), true
		}
		items = append(items, item)
	}
	if err := finishOAuthAggregateArray(accounts); err != nil {
		return failOAuthCredentialContainerExpansion(file, items, budget, snapshot, err), true
	}
	return items, true
}

func convertSub2APICredential(
	account map[string]any,
	exportedAt time.Time,
) (map[string]any, [sha256.Size]byte, error) {
	var emptyFingerprint [sha256.Size]byte
	platform := strings.ToLower(sub2APIFirstString(account, []string{"platform"}, []string{"provider"}))
	authType := strings.ToLower(sub2APIFirstString(account, []string{"type"}, []string{"auth_type"}, []string{"authType"}))
	if authType == "" {
		authType = "oauth"
	}
	provider := ""
	switch platform {
	case "openai":
		provider = "codex"
	case "anthropic":
		provider = "anthropic"
	case "grok", "xai":
		provider = xaiauth.ChannelType
	}
	if provider == "" || authType != "oauth" {
		return nil, emptyFingerprint, fmt.Errorf("unsupported account platform/type %q/%q", platform, authType)
	}
	if provider == "codex" && isSub2APIAgentIdentity(account) {
		return nil, emptyFingerprint, errors.New("codex agent identity credential is not supported by ccLoad")
	}

	accessToken := sub2APIFirstString(account,
		[]string{"credentials", "access_token"}, []string{"credentials", "accessToken"},
		[]string{"credential", "access_token"}, []string{"credential", "accessToken"},
		[]string{"tokens", "access_token"}, []string{"tokens", "accessToken"},
		[]string{"access_token"}, []string{"accessToken"},
	)
	if accessToken == "" {
		return nil, emptyFingerprint, errors.New("missing access_token")
	}
	refreshToken := sub2APIFirstString(account,
		[]string{"credentials", "refresh_token"}, []string{"credentials", "refreshToken"},
		[]string{"credential", "refresh_token"}, []string{"credential", "refreshToken"},
		[]string{"tokens", "refresh_token"}, []string{"tokens", "refreshToken"},
		[]string{"refresh_token"}, []string{"refreshToken"},
	)
	if refreshToken == "" {
		return nil, emptyFingerprint, errors.New("missing refresh_token")
	}
	idToken := sub2APIFirstString(account,
		[]string{"credentials", "id_token"}, []string{"credentials", "idToken"},
		[]string{"credential", "id_token"}, []string{"credential", "idToken"},
		[]string{"tokens", "id_token"}, []string{"tokens", "idToken"},
		[]string{"id_token"}, []string{"idToken"},
	)
	accessClaims := parseSub2APIJWTPayload(accessToken)
	idClaims := parseSub2APIJWTPayload(idToken)
	accessAuth := sub2APIObjectAt(accessClaims, "https://api.openai.com/auth")
	idAuth := sub2APIObjectAt(idClaims, "https://api.openai.com/auth")
	accessProfile := sub2APIObjectAt(accessClaims, "https://api.openai.com/profile")

	name := sub2APIFirstString(account, []string{"name"}, []string{"label"})
	email := sub2APIFirstString(account,
		[]string{"credentials", "email"}, []string{"credentials", "email_address"},
		[]string{"credential", "email"}, []string{"credential", "email_address"},
		[]string{"extra", "email"}, []string{"extra", "email_address"},
		[]string{"email"}, []string{"email_address"},
	)
	if email == "" {
		email = firstSub2APIMapString([]map[string]any{idClaims, accessProfile, accessClaims}, "email")
	}
	if email == "" && strings.Contains(name, "@") {
		email = name
	}
	accountID := sub2APIFirstString(account,
		[]string{"credentials", "chatgpt_account_id"}, []string{"credentials", "account_id"}, []string{"credentials", "accountId"},
		[]string{"credential", "chatgpt_account_id"}, []string{"credential", "account_id"}, []string{"credential", "accountId"},
		[]string{"extra", "chatgpt_account_id"}, []string{"extra", "account_id"},
		[]string{"chatgpt_account_id"}, []string{"account_id"}, []string{"accountId"},
	)
	if accountID == "" {
		accountID = firstSub2APIMapString([]map[string]any{accessAuth, idAuth}, "chatgpt_account_id", "account_id")
	}
	accountUUID := sub2APIFirstString(account,
		[]string{"credentials", "account_uuid"}, []string{"credentials", "accountUuid"},
		[]string{"credential", "account_uuid"}, []string{"credential", "accountUuid"},
		[]string{"extra", "account_uuid"}, []string{"extra", "accountUuid"},
		[]string{"account_uuid"}, []string{"accountUuid"},
	)
	subject := sub2APIFirstString(account,
		[]string{"credentials", "sub"}, []string{"credential", "sub"},
		[]string{"extra", "sub"}, []string{"sub"},
	)
	if subject == "" {
		subject = firstSub2APIMapString([]map[string]any{idClaims, accessClaims}, "sub")
	}
	planType := sub2APIFirstString(account,
		[]string{"credentials", "plan_type"}, []string{"credentials", "planType"}, []string{"credentials", "chatgpt_plan_type"},
		[]string{"extra", "plan_type"}, []string{"extra", "chatgpt_plan_type"},
		[]string{"plan_type"}, []string{"planType"}, []string{"chatgpt_plan_type"},
	)
	if planType == "" {
		planType = firstSub2APIMapString([]map[string]any{accessAuth, idAuth}, "chatgpt_plan_type", "plan_type")
	}

	expiresIn, hasExpiresIn, err := sub2APIFirstInteger(account,
		[]string{"credentials", "expires_in"}, []string{"credentials", "expiresIn"},
		[]string{"extra", "expires_in"}, []string{"extra", "expiresIn"},
		[]string{"expires_in"}, []string{"expiresIn"},
	)
	if err != nil || hasExpiresIn && expiresIn < 0 {
		return nil, emptyFingerprint, errors.New("invalid expires_in")
	}
	expiresAt, hasExpiresAt, err := sub2APIFirstTimestamp(account,
		[]string{"credentials", "expires_at"}, []string{"credentials", "expiresAt"}, []string{"credentials", "expired"},
		[]string{"extra", "expires_at"}, []string{"extra", "expiresAt"}, []string{"extra", "expired"},
		[]string{"expires_at"}, []string{"expiresAt"}, []string{"expires"}, []string{"expired"},
	)
	if err != nil {
		return nil, emptyFingerprint, fmt.Errorf("invalid token expiry: %w", err)
	}
	if !hasExpiresAt {
		expiresAt, hasExpiresAt = sub2APIJWTExpiry(accessClaims)
	}
	if !hasExpiresAt {
		return nil, emptyFingerprint, errors.New("missing token expiry")
	}
	lastRefresh, hasLastRefresh, err := sub2APIFirstTimestamp(account,
		[]string{"extra", "last_refresh"}, []string{"extra", "lastRefresh"}, []string{"extra", "last_refreshed_at"},
		[]string{"credentials", "last_refresh"}, []string{"credentials", "lastRefresh"}, []string{"credentials", "last_refreshed_at"},
		[]string{"last_refresh"}, []string{"lastRefresh"}, []string{"last_refreshed_at"},
	)
	if err != nil {
		return nil, emptyFingerprint, fmt.Errorf("invalid last_refresh: %w", err)
	}
	if !hasLastRefresh && hasExpiresIn && expiresIn > 0 {
		lastRefresh, hasLastRefresh = expiresAt.Add(-time.Duration(expiresIn)*time.Second), true
	}
	if !hasLastRefresh && !exportedAt.IsZero() {
		lastRefresh, hasLastRefresh = exportedAt, true
	}
	if !hasLastRefresh {
		lastRefresh = time.Now().UTC()
	}

	identity := email
	if identity == "" && provider == "codex" {
		identity = accountID
	}
	if identity == "" && provider == "anthropic" {
		identity = accountUUID
	}
	if identity == "" && provider == xaiauth.ChannelType {
		identity = subject
	}
	if identity == "" {
		identity = name
	}
	if sanitizeOAuthCredentialFileToken(identity) == "" {
		return nil, emptyFingerprint, errors.New("account has no usable email, account id, account UUID, or name")
	}

	converted := map[string]any{
		"type":          provider,
		"access_token":  accessToken,
		"refresh_token": refreshToken,
		"expired":       expiresAt.UTC().Format(time.RFC3339),
		"last_refresh":  lastRefresh.UTC().Format(time.RFC3339),
	}
	if priority, present, priorityErr := sub2APIFirstInteger(account, []string{"priority"}); priorityErr != nil {
		return nil, emptyFingerprint, errors.New("invalid priority")
	} else if present {
		converted["priority"] = priority
	}
	if idToken != "" {
		converted["id_token"] = idToken
	}
	if provider == "codex" {
		if email != "" {
			converted["email"] = email
		}
		if accountID != "" {
			converted["account_id"] = accountID
		}
		if planType != "" {
			converted["plan_type"] = planType
		}
		return converted, sha256.Sum256([]byte(accessToken + "\x00" + accountID)), nil
	}
	if provider == xaiauth.ChannelType {
		converted["auth_kind"] = "oauth"
		if hasExpiresIn {
			converted["expires_in"] = expiresIn
		}
		for target, paths := range map[string][][]string{
			"token_type": {
				{"credentials", "token_type"}, {"credentials", "tokenType"},
				{"credential", "token_type"}, {"credential", "tokenType"}, {"token_type"}, {"tokenType"},
			},
			"base_url": {
				{"credentials", "base_url"}, {"credentials", "baseURL"},
				{"credential", "base_url"}, {"credential", "baseURL"}, {"base_url"}, {"baseURL"},
			},
			"token_endpoint": {
				{"credentials", "token_endpoint"}, {"credentials", "tokenEndpoint"},
				{"credential", "token_endpoint"}, {"credential", "tokenEndpoint"}, {"token_endpoint"}, {"tokenEndpoint"},
			},
			"client_id": {
				{"credentials", "client_id"}, {"credentials", "clientId"},
				{"credential", "client_id"}, {"credential", "clientId"}, {"client_id"}, {"clientId"},
			},
			"scope": {
				{"credentials", "scope"}, {"credential", "scope"}, {"scope"},
			},
			"team_id": {
				{"credentials", "team_id"}, {"credentials", "teamId"},
				{"credential", "team_id"}, {"credential", "teamId"}, {"team_id"}, {"teamId"},
			},
		} {
			if value := sub2APIFirstString(account, paths...); value != "" {
				converted[target] = value
			}
		}
		if email != "" {
			converted["email"] = email
		}
		if subject != "" {
			converted["sub"] = subject
		}
		return converted, sha256.Sum256([]byte(accessToken + "\x00" + subject)), nil
	}
	if email != "" {
		converted["email_address"] = email
	}
	if accountUUID != "" {
		converted["account_uuid"] = accountUUID
	}
	return converted, sha256.Sum256([]byte(accessToken + "\x00" + accountUUID)), nil
}

func expandXAICredentialContainer(
	file oauthCredentialImportFile,
	provider string,
	budget *oauthCredentialImportBudget,
) ([]oauthCredentialImportFile, bool) {
	fields, err := decodeOAuthCredentialFields(file.Raw)
	if err != nil {
		return nil, false
	}
	rawContainer, ok := fields["credentials"]
	if !ok {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(rawContainer))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return aggregateContainerFailure(file, errors.New("xAI credentials must be a non-empty object")), true
	}
	if !decoder.More() {
		return aggregateContainerFailure(file, errors.New("xAI credentials must be a non-empty object")), true
	}

	snapshot := beginOAuthCredentialContainerExpansion(file, budget)
	items := make([]oauthCredentialImportFile, 0)
	for index := 0; decoder.More(); index++ {
		keyToken, keyErr := decoder.Token()
		key, keyOK := keyToken.(string)
		if keyErr != nil || !keyOK {
			return failOAuthCredentialContainerExpansion(file, items, budget, snapshot, errors.New("xAI credentials object has an invalid key")), true
		}
		var rawCredential json.RawMessage
		if err := decoder.Decode(&rawCredential); err != nil {
			return failOAuthCredentialContainerExpansion(file, items, budget, snapshot, fmt.Errorf("decode xAI credential %q: %w", key, err)), true
		}
		if provider == oauthCredentialProviderAuto {
			detected, detectErr := detectOAuthCredentialProvider(rawCredential)
			if detectErr != nil || detected != xaiauth.ChannelType {
				wipeOAuthCredentialImportFiles(items)
				*budget = snapshot
				return nil, false
			}
		}
		item := newOAuthCredentialImportFile(
			aggregateChildFileName(file.FileName, index),
			file.SortName+"\x00"+key,
			append([]byte(nil), rawCredential...),
		)
		if err := reserveExpandedOAuthCredentialFile(&item, budget); err != nil {
			clear(item.Raw)
			return failOAuthCredentialContainerExpansion(file, items, budget, snapshot, err), true
		}
		items = append(items, item)
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return failOAuthCredentialContainerExpansion(file, items, budget, snapshot, errors.New("xAI credentials object is invalid")), true
	}
	return items, true
}

func reserveExpandedOAuthCredentialFile(
	file *oauthCredentialImportFile,
	budget *oauthCredentialImportBudget,
) error {
	if file.Err == nil && (len(file.Raw) == 0 || len(file.Raw) > maxOAuthCredentialImportBytes) {
		clear(file.Raw)
		file.Raw = nil
		file.Err = errors.New("credential file size is invalid")
	}
	return budget.reserve(uint64(len(file.Raw)))
}

func beginOAuthCredentialContainerExpansion(
	file oauthCredentialImportFile,
	budget *oauthCredentialImportBudget,
) oauthCredentialImportBudget {
	snapshot := *budget
	budget.entries--
	budget.expandedSize -= uint64(len(file.Raw))
	return snapshot
}

func failOAuthCredentialContainerExpansion(
	file oauthCredentialImportFile,
	items []oauthCredentialImportFile,
	budget *oauthCredentialImportBudget,
	snapshot oauthCredentialImportBudget,
	err error,
) []oauthCredentialImportFile {
	wipeOAuthCredentialImportFiles(items)
	*budget = snapshot
	return aggregateContainerFailure(file, err)
}

func wipeOAuthCredentialImportFiles(files []oauthCredentialImportFile) {
	for i := range files {
		clear(files[i].Raw)
		files[i].Raw = nil
	}
}

func aggregateContainerFailure(file oauthCredentialImportFile, err error) []oauthCredentialImportFile {
	return []oauthCredentialImportFile{failedOAuthCredentialImportFile(file.FileName, err)}
}

func aggregateChildFileName(parent string, index int) string {
	return fmt.Sprintf("%s#%d", parent, index+1)
}

func aggregateChildSortName(parent string, index int) string {
	return fmt.Sprintf("%s\x00%08d", parent, index)
}

func newOAuthAggregateArrayDecoder(raw json.RawMessage) (*json.Decoder, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return nil, errors.New("accounts must be an array")
	}
	return decoder, nil
}

func finishOAuthAggregateArray(decoder *json.Decoder) error {
	if decoder == nil {
		return errors.New("accounts array is unavailable")
	}
	token, err := decoder.Token()
	if err != nil || token != json.Delim(']') {
		return errors.New("accounts array is invalid")
	}
	return nil
}

func parseOptionalSub2APITimestamp(raw json.RawMessage) (time.Time, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return time.Time{}, nil
	}
	var value any
	if err := decodeOAuthAggregateJSON(raw, &value); err != nil {
		return time.Time{}, err
	}
	if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
		return time.Time{}, nil
	}
	return parseSub2APITimestamp(value)
}

func decodeOAuthAggregateJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("credential contains trailing JSON")
	}
	return nil
}

func isSub2APIAgentIdentity(account map[string]any) bool {
	authMode := strings.ToLower(sub2APIFirstString(account,
		[]string{"credentials", "auth_mode"}, []string{"credentials", "authMode"},
		[]string{"credential", "auth_mode"}, []string{"credential", "authMode"},
		[]string{"auth_mode"}, []string{"authMode"},
	))
	return authMode == "agentidentity" || authMode == "agent_identity" || authMode == "agent-identity"
}

func sub2APIFirstString(root map[string]any, paths ...[]string) string {
	for _, path := range paths {
		value, _ := sub2APIValueAt(root, path...).(string)
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func sub2APIValueAt(root map[string]any, path ...string) any {
	var current any = root
	for _, part := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[part]
	}
	return current
}

func sub2APIFirstInteger(root map[string]any, paths ...[]string) (int64, bool, error) {
	for _, path := range paths {
		value := sub2APIValueAt(root, path...)
		if value == nil {
			continue
		}
		integer, ok := sub2APIInteger(value)
		if !ok {
			return 0, true, errors.New("value must be an integer")
		}
		return integer, true, nil
	}
	return 0, false, nil
}

func sub2APIInteger(value any) (int64, bool) {
	var raw string
	switch typed := value.(type) {
	case json.Number:
		raw = typed.String()
	case string:
		raw = strings.TrimSpace(typed)
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	default:
		return 0, false
	}
	integer, err := strconv.ParseInt(raw, 10, 64)
	return integer, err == nil
}

func sub2APIFirstTimestamp(root map[string]any, paths ...[]string) (time.Time, bool, error) {
	for _, path := range paths {
		value := sub2APIValueAt(root, path...)
		if value == nil {
			continue
		}
		if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
			continue
		}
		parsed, err := parseSub2APITimestamp(value)
		return parsed, true, err
	}
	return time.Time{}, false, nil
}

func parseSub2APITimestamp(value any) (time.Time, error) {
	if text, ok := value.(string); ok {
		text = strings.TrimSpace(text)
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed.UTC(), nil
		}
		value = json.Number(text)
	}
	integer, ok := sub2APIInteger(value)
	if !ok || integer <= 0 {
		return time.Time{}, errors.New("timestamp must be RFC3339, Unix seconds, or Unix milliseconds")
	}
	if integer >= sub2APIMillisCutover {
		return time.UnixMilli(integer).UTC(), nil
	}
	return time.Unix(integer, 0).UTC(), nil
}

func parseSub2APIJWTPayload(token string) map[string]any {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || parts[1] == "" {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if decodeOAuthAggregateJSON(payload, &claims) != nil {
		return nil
	}
	return claims
}

func sub2APIObjectAt(root map[string]any, key string) map[string]any {
	if root == nil {
		return nil
	}
	object, _ := root[key].(map[string]any)
	return object
}

func firstSub2APIMapString(objects []map[string]any, keys ...string) string {
	for _, object := range objects {
		for _, key := range keys {
			value, _ := object[key].(string)
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	return ""
}

func sub2APIJWTExpiry(claims map[string]any) (time.Time, bool) {
	if claims == nil {
		return time.Time{}, false
	}
	expiresAt, err := parseSub2APITimestamp(claims["exp"])
	return expiresAt, err == nil
}

func sanitizeOAuthCredentialFileToken(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	lastSeparator := false
	for _, character := range value {
		allowed := character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("@._+-", character)
		if allowed {
			builder.WriteRune(character)
			lastSeparator = false
			continue
		}
		if builder.Len() > 0 && !lastSeparator {
			builder.WriteByte('-')
			lastSeparator = true
		}
	}
	return strings.Trim(builder.String(), "-.")
}
