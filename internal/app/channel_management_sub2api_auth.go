package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"ccLoad/internal/model"
)

const (
	sub2APILoginPath          = "/api/v1/auth/login"
	sub2APILogin2FAPath       = "/api/v1/auth/login/2fa"
	sub2APIRefreshPath        = "/api/v1/auth/refresh"
	sub2APIRefreshAhead       = time.Minute
	maxSub2APIEmailLength     = 320
	maxSub2APIPasswordLength  = 4096
	maxSub2APITokenLength     = 32 * 1024
	maxSub2APIExpiresInSecond = 366 * 24 * 60 * 60
)

type sub2APIManagementSession struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	AccountID    int64
}

type sub2APIAuthData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Requires2FA  bool   `json:"requires_2fa"`
	TempToken    string `json:"temp_token"`
	User         *struct {
		ID int64 `json:"id"`
	} `json:"user"`
}

func (s *channelManagementService) resolveChannelManagementInput(
	ctx context.Context,
	cfg *model.Config,
	input *channelManagementInput,
) (*channelManagementInput, error) {
	if input == nil {
		return nil, errInvalidManagementRequest
	}
	resolved := *input
	resolved.Profile = strings.TrimSpace(input.Profile)
	resolved.BaseURL = strings.TrimSpace(input.BaseURL)
	resolved.AccessToken = strings.TrimSpace(input.AccessToken)
	resolved.RefreshToken = strings.TrimSpace(input.RefreshToken)
	resolved.ExpiresAt = cloneTimePointer(input.ExpiresAt)
	resolved.AccountID = cloneInt64Pointer(input.AccountID)
	resolved.Email = strings.TrimSpace(input.Email)
	resolved.Password = input.Password
	resolved.TOTPCode = strings.TrimSpace(input.TOTPCode)
	if resolved.sub2APISession != nil {
		return &resolved, nil
	}

	if resolved.Profile != model.ChannelManagementProfileSub2API && resolved.Profile != model.ChannelManagementProfileSub2APIPro {
		return &resolved, nil
	}
	session, err := sub2APISessionFromInput(&resolved)
	if err != nil {
		return nil, err
	}
	if session != nil {
		resolved.sub2APISession = session
		resolved.AccessToken = ""
		resolved.TOTPCode = ""
		return &resolved, nil
	}
	if resolved.AccessToken != "" {
		return nil, errInvalidManagementRequest
	}
	if canReuseSub2APIManagementSession(cfg, &resolved) {
		resolved.TOTPCode = ""
		return &resolved, nil
	}
	hasLoginInput := resolved.Email != "" || resolved.Password != "" || resolved.TOTPCode != ""
	if !hasLoginInput {
		return &resolved, nil
	}
	if resolved.Email == "" || resolved.Password == "" || len(resolved.Email) > maxSub2APIEmailLength ||
		len(resolved.Password) > maxSub2APIPasswordLength || !validSub2APITOTPCode(resolved.TOTPCode) {
		return nil, errInvalidManagementRequest
	}

	validated := &model.ChannelManagementEnvelope{
		Kind: model.ChannelManagementKind, Version: model.ChannelManagementVersion, Profile: resolved.Profile,
		Settings: model.ChannelManagementSettings{
			BaseURL: resolved.BaseURL, AccessToken: "pending-login", RefreshToken: "pending-refresh",
			ExpiresAt: timePointer(s.now().Add(time.Hour)), AccountID: int64Pointer(1),
			DailyCheckinEnabled: resolved.DailyCheckinEnabled, DailyCheckinTime: resolved.DailyCheckinTime,
		},
	}
	if err := validated.Validate(); err != nil {
		return nil, errInvalidManagementRequest
	}
	resolved.BaseURL = validated.Settings.BaseURL
	session, err = s.loginSub2APIManagementSession(ctx, cfg, resolved.BaseURL, resolved.Email, resolved.Password, resolved.TOTPCode)
	if err != nil {
		return nil, err
	}
	resolved.sub2APISession = session
	resolved.TOTPCode = ""
	return &resolved, nil
}

func canReuseSub2APIManagementSession(cfg *model.Config, input *channelManagementInput) bool {
	if cfg == nil || input == nil || strings.TrimSpace(cfg.OAuthCredential) == "" {
		return false
	}
	current, err := model.ParseChannelManagementEnvelope(cfg.OAuthCredential)
	if err != nil || current == nil || current.Profile != input.Profile {
		return false
	}
	if !strings.EqualFold(current.Settings.BaseURL, strings.TrimSuffix(strings.TrimSpace(input.BaseURL), "/")) {
		return false
	}
	return channelManagementCredentialConfigured(current)
}

func sub2APISessionFromInput(input *channelManagementInput) (*sub2APIManagementSession, error) {
	if input == nil {
		return nil, errInvalidManagementRequest
	}
	hasSession := input.AccessToken != "" || input.RefreshToken != "" || input.ExpiresAt != nil || input.AccountID != nil
	if !hasSession {
		return nil, nil
	}
	if input.AccessToken == "" || input.RefreshToken == "" || input.ExpiresAt == nil ||
		len(input.AccessToken) > maxSub2APITokenLength || len(input.RefreshToken) > maxSub2APITokenLength ||
		input.AccountID == nil || *input.AccountID <= 0 {
		return nil, errInvalidManagementRequest
	}
	return &sub2APIManagementSession{
		AccessToken: input.AccessToken, RefreshToken: input.RefreshToken,
		ExpiresAt: input.ExpiresAt.UTC(), AccountID: *input.AccountID,
	}, nil
}

func (s *channelManagementService) PreviewSub2APILogin(
	ctx context.Context,
	cfg *model.Config,
	input *channelManagementInput,
) (*sub2APIManagementSession, error) {
	if s == nil || input == nil {
		return nil, errInvalidManagementRequest
	}
	profile := strings.TrimSpace(input.Profile)
	if profile != model.ChannelManagementProfileSub2API && profile != model.ChannelManagementProfileSub2APIPro {
		return nil, errInvalidManagementRequest
	}
	email := strings.TrimSpace(input.Email)
	password := input.Password
	totpCode := strings.TrimSpace(input.TOTPCode)
	if email == "" || password == "" || len(email) > maxSub2APIEmailLength ||
		len(password) > maxSub2APIPasswordLength || !validSub2APITOTPCode(totpCode) {
		return nil, errInvalidManagementRequest
	}
	validated := &model.ChannelManagementEnvelope{
		Kind: model.ChannelManagementKind, Version: model.ChannelManagementVersion, Profile: profile,
		Settings: model.ChannelManagementSettings{
			BaseURL: strings.TrimSpace(input.BaseURL), AccessToken: "pending-login", RefreshToken: "pending-refresh",
			ExpiresAt: timePointer(s.now().Add(time.Hour)), AccountID: int64Pointer(1),
		},
	}
	if err := validated.Validate(); err != nil {
		return nil, errInvalidManagementRequest
	}
	operationCtx, cancel := context.WithTimeout(ctx, s.operationTimeout)
	defer cancel()
	return s.loginSub2APIManagementSession(
		operationCtx, cfg, validated.Settings.BaseURL, email, password, totpCode,
	)
}

func validSub2APITOTPCode(code string) bool {
	if code == "" {
		return true
	}
	if len(code) != 6 {
		return false
	}
	for _, char := range code {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func (s *channelManagementService) loginSub2APIManagementSession(
	ctx context.Context,
	cfg *model.Config,
	baseURL string,
	email string,
	password string,
	totpCode string,
) (*sub2APIManagementSession, error) {
	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		return nil, errInvalidManagementRequest
	}
	response, err := s.requestSub2APIAuth(ctx, cfg, strings.TrimRight(baseURL, "/")+sub2APILoginPath, body, password)
	if err != nil {
		return nil, err
	}
	if response.Requires2FA {
		if totpCode == "" {
			return nil, errManagementTwoFactorRequired
		}
		if strings.TrimSpace(response.TempToken) == "" {
			return nil, errInvalidManagementResponse
		}
		body, err = json.Marshal(map[string]string{"temp_token": response.TempToken, "totp_code": totpCode})
		if err != nil {
			return nil, errInvalidManagementRequest
		}
		response, err = s.requestSub2APIAuth(
			ctx, cfg, strings.TrimRight(baseURL, "/")+sub2APILogin2FAPath, body, response.TempToken, totpCode,
		)
		if err != nil {
			return nil, err
		}
	}
	return s.sub2APIManagementSessionFromAuth(response, 0)
}

func (s *channelManagementService) requestSub2APIAuth(
	ctx context.Context,
	cfg *model.Config,
	target string,
	body []byte,
	sensitiveValues ...string,
) (*sub2APIAuthData, error) {
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	result, err := s.doManagementPublicRequest(ctx, cfg, http.MethodPost, target, body, headers)
	if err != nil {
		return nil, err
	}
	if result.StatusCode < http.StatusOK || result.StatusCode >= http.StatusMultipleChoices {
		return nil, withManagementErrorDetail(errManagementRequestFailed, result, sensitiveValues...)
	}
	response, err := decodeSub2APIResponse[sub2APIAuthData](result.Body)
	if err != nil {
		return nil, err
	}
	if response.Code != 0 {
		return nil, withManagementErrorDetail(errInvalidManagementRequest, result, sensitiveValues...)
	}
	return &response.Data, nil
}

func (s *channelManagementService) sub2APIManagementSessionFromAuth(
	response *sub2APIAuthData,
	accountID int64,
) (*sub2APIManagementSession, error) {
	if response == nil || strings.TrimSpace(response.AccessToken) == "" || strings.TrimSpace(response.RefreshToken) == "" ||
		len(response.AccessToken) > maxSub2APITokenLength || len(response.RefreshToken) > maxSub2APITokenLength ||
		response.ExpiresIn <= 0 || response.ExpiresIn > maxSub2APIExpiresInSecond {
		return nil, errInvalidManagementResponse
	}
	if response.User != nil {
		accountID = response.User.ID
	}
	if accountID <= 0 {
		return nil, errInvalidManagementResponse
	}
	return &sub2APIManagementSession{
		AccessToken: strings.TrimSpace(response.AccessToken), RefreshToken: strings.TrimSpace(response.RefreshToken),
		ExpiresAt: s.now().UTC().Add(time.Duration(response.ExpiresIn) * time.Second), AccountID: accountID,
	}, nil
}

func (s *channelManagementService) ensureSub2APIManagementSession(
	ctx context.Context,
	cfg *model.Config,
	envelope *model.ChannelManagementEnvelope,
) (*model.Config, *model.ChannelManagementEnvelope, error) {
	if envelope == nil || (envelope.Profile != model.ChannelManagementProfileSub2API && envelope.Profile != model.ChannelManagementProfileSub2APIPro) {
		return cfg, envelope, nil
	}
	if !channelManagementCredentialConfigured(envelope) {
		return nil, nil, errChannelManagementNotConfigured
	}
	if envelope.Settings.ExpiresAt.After(s.now().Add(sub2APIRefreshAhead)) {
		return cfg, envelope, nil
	}

	body, err := json.Marshal(map[string]string{"refresh_token": envelope.Settings.RefreshToken})
	if err != nil {
		return nil, nil, errInvalidManagementRequest
	}
	response, err := s.requestSub2APIAuth(
		ctx, cfg, strings.TrimRight(envelope.Settings.BaseURL, "/")+sub2APIRefreshPath, body,
		envelope.Settings.RefreshToken,
	)
	if err != nil {
		return nil, nil, err
	}
	session, err := s.sub2APIManagementSessionFromAuth(response, *envelope.Settings.AccountID)
	if err != nil {
		return nil, nil, err
	}
	return s.persistSub2APIManagementSession(ctx, cfg, envelope, session)
}

func (s *channelManagementService) persistSub2APIManagementSession(
	ctx context.Context,
	cfg *model.Config,
	source *model.ChannelManagementEnvelope,
	session *sub2APIManagementSession,
) (*model.Config, *model.ChannelManagementEnvelope, error) {
	if cfg == nil || source == nil || session == nil {
		return nil, nil, errInvalidManagementRequest
	}
	currentCfg := cfg
	current := source
	for {
		if !sameSub2APIManagementAccount(source, current) {
			return nil, nil, errInvalidManagementRequest
		}
		if current.Settings.RefreshToken != source.Settings.RefreshToken {
			if channelManagementCredentialConfigured(current) {
				return currentCfg, current, nil
			}
			return nil, nil, errInvalidManagementResponse
		}

		next := *current
		next.Settings = current.Settings
		next.Settings.AccessToken = session.AccessToken
		next.Settings.RefreshToken = session.RefreshToken
		next.Settings.ExpiresAt = timePointer(session.ExpiresAt)
		next.Settings.AccountID = int64Pointer(session.AccountID)
		nextRaw, err := next.Marshal()
		if err != nil {
			return nil, nil, errInvalidManagementResponse
		}
		updated, err := s.store.CompareAndSwapChannelManagement(ctx, currentCfg.ID, currentCfg.OAuthCredential, nextRaw)
		if err != nil {
			return nil, nil, err
		}
		if updated {
			currentCfg = currentCfg.Clone()
			currentCfg.OAuthCredential = nextRaw
			return currentCfg, &next, nil
		}
		currentCfg, current, err = s.loadChannelManagement(ctx, currentCfg.ID)
		if err != nil {
			return nil, nil, err
		}
	}
}

func sameSub2APIManagementAccount(left, right *model.ChannelManagementEnvelope) bool {
	return left != nil && right != nil && left.Profile == right.Profile &&
		left.Settings.BaseURL == right.Settings.BaseURL &&
		equalChannelManagementUserID(left.Settings.AccountID, right.Settings.AccountID)
}
