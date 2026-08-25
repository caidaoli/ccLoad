package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

const (
	channelManagementOperationTimeout = 30 * time.Second
	managementResponseBodyLimit       = 64 * 1024
)

var (
	errChannelManagementNotConfigured       = errors.New("channel_management_not_configured")
	errChannelManagementProviderUnavailable = errors.New("channel_management_provider_unavailable")
	errInvalidManagementRequest             = errors.New("invalid_request")
	errManagementRequestFailed              = errors.New("management_request_failed")
	errInvalidManagementResponse            = errors.New("invalid_response")
)

type channelManagementInput struct {
	Profile             string `json:"profile"`
	BaseURL             string `json:"base_url"`
	AccessToken         string `json:"access_token,omitempty"`
	UserID              *int64 `json:"user_id,omitempty"`
	DailyCheckinEnabled bool   `json:"daily_checkin_enabled,omitempty"`
	DailyCheckinTime    string `json:"daily_checkin_time,omitempty"`
}

type channelManagementBalanceView struct {
	Remaining        float64                                       `json:"remaining"`
	Unit             string                                        `json:"unit"`
	Used             *float64                                      `json:"used,omitempty"`
	Total            *float64                                      `json:"total,omitempty"`
	AvailablePercent *float64                                      `json:"available_percent,omitempty"`
	Subscriptions    []model.ChannelManagementSubscriptionSnapshot `json:"subscriptions,omitempty"`
	SampledAt        time.Time                                     `json:"sampled_at"`
}

type channelManagementView struct {
	Profile              string                        `json:"profile"`
	BaseURL              string                        `json:"base_url"`
	UserIDConfigured     bool                          `json:"user_id_configured"`
	DailyCheckinEnabled  bool                          `json:"daily_checkin_enabled"`
	DailyCheckinTime     string                        `json:"daily_checkin_time,omitempty"`
	CredentialConfigured bool                          `json:"credential_configured"`
	LastCheckinStatus    string                        `json:"last_checkin_status,omitempty"`
	LastCheckinAt        *time.Time                    `json:"last_checkin_at,omitempty"`
	Balance              *channelManagementBalanceView `json:"balance,omitempty"`
}

type channelCheckinResult struct {
	Status      string                        `json:"status"`
	StatusCode  int                           `json:"status_code"`
	Reward      *float64                      `json:"reward,omitempty"`
	Balance     *channelManagementBalanceView `json:"balance,omitempty"`
	CheckedInAt *time.Time                    `json:"checked_in_at,omitempty"`
}

type managementHTTPResult struct {
	StatusCode   int
	Body         []byte
	WroteRequest bool
}

type channelManagementService struct {
	store            storage.Store
	clientForChannel func(*model.Config) *http.Client
	now              func() time.Time
	operationTimeout time.Duration
	gates            sync.Map
}

func newChannelManagementService(
	store storage.Store,
	clientForChannel func(*model.Config) *http.Client,
) *channelManagementService {
	return &channelManagementService{
		store:            store,
		clientForChannel: clientForChannel,
		now:              time.Now,
		operationTimeout: channelManagementOperationTimeout,
	}
}

func (s *channelManagementService) View(cfg *model.Config) (*channelManagementView, error) {
	if cfg == nil {
		return nil, errChannelManagementNotConfigured
	}
	if strings.TrimSpace(cfg.OAuthCredential) == "" {
		return &channelManagementView{}, nil
	}
	envelope, err := model.ParseChannelManagementEnvelope(cfg.OAuthCredential)
	if err != nil {
		return nil, errInvalidManagementResponse
	}
	return channelManagementViewFromEnvelope(envelope), nil
}

func (s *channelManagementService) SaveSettings(
	ctx context.Context,
	cfg *model.Config,
	input *channelManagementInput,
) (*channelManagementView, error) {
	if s == nil || s.store == nil || cfg == nil || input == nil || cfg.AuthType != model.AuthTypeAPIKey {
		return nil, errInvalidManagementRequest
	}
	operationCtx, cancel := context.WithTimeout(ctx, s.operationTimeout)
	defer cancel()
	release, err := s.acquireChannel(operationCtx, cfg.ID)
	if err != nil {
		return nil, err
	}
	defer release()

	current := cfg.Clone()
	for {
		nextEnvelope, nextRaw, mergeErr := mergeChannelManagementSettings(current.OAuthCredential, input)
		if mergeErr != nil {
			return nil, mergeErr
		}
		updated, casErr := s.store.CompareAndSwapChannelManagement(
			operationCtx, current.ID, current.OAuthCredential, nextRaw,
		)
		if casErr != nil {
			return nil, casErr
		}
		if updated {
			current.OAuthCredential = nextRaw
			if nextEnvelope == nil {
				return &channelManagementView{}, nil
			}
			return channelManagementViewFromEnvelope(nextEnvelope), nil
		}
		current, err = s.store.GetConfig(operationCtx, current.ID)
		if err != nil {
			return nil, err
		}
		if current.AuthType != model.AuthTypeAPIKey {
			return nil, errInvalidManagementRequest
		}
	}
}

func (s *channelManagementService) RefreshBalance(
	ctx context.Context,
	channelID int64,
) (*channelManagementView, error) {
	operationCtx, cancel := context.WithTimeout(ctx, s.operationTimeout)
	defer cancel()
	release, err := s.acquireChannel(operationCtx, channelID)
	if err != nil {
		return nil, err
	}
	defer release()
	cfg, envelope, err := s.loadChannelManagement(operationCtx, channelID)
	if err != nil {
		return nil, err
	}
	var snapshot *model.ChannelManagementBalanceSnapshot
	switch envelope.Profile {
	case model.ChannelManagementProfileNewAPI:
		snapshot, _, err = s.refreshNewAPIBalance(operationCtx, cfg, envelope)
	case model.ChannelManagementProfileSub2API, model.ChannelManagementProfileSub2APIPro:
		snapshot, _, err = s.refreshSub2APIBalance(operationCtx, cfg, envelope)
	default:
		return nil, errChannelManagementProviderUnavailable
	}
	if err != nil {
		return nil, err
	}
	persisted, err := s.persistChannelManagementState(
		operationCtx,
		cfg,
		envelope,
		func(state *model.ChannelManagementState) {
			state.LastBalance = snapshot
		},
	)
	if err != nil {
		return nil, err
	}
	return channelManagementViewFromEnvelope(persisted), nil
}

func (s *channelManagementService) CheckIn(
	ctx context.Context,
	channelID int64,
) (*channelCheckinResult, error) {
	operationCtx, cancel := context.WithTimeout(ctx, s.operationTimeout)
	defer cancel()
	release, err := s.acquireChannel(operationCtx, channelID)
	if err != nil {
		return nil, err
	}
	defer release()
	cfg, envelope, err := s.loadChannelManagement(operationCtx, channelID)
	if err != nil {
		return nil, err
	}
	var result *channelCheckinResult
	var snapshot *model.ChannelManagementBalanceSnapshot
	switch envelope.Profile {
	case model.ChannelManagementProfileNewAPI:
		result, snapshot, err = s.checkInNewAPI(operationCtx, cfg, envelope)
	case model.ChannelManagementProfileSub2API:
		result = newAPICheckinResult(newAPICheckinUnsupported, 0, s.now())
	case model.ChannelManagementProfileSub2APIPro:
		result, snapshot, err = s.checkInSub2APIPro(operationCtx, cfg, envelope)
	default:
		return nil, errChannelManagementProviderUnavailable
	}
	if err != nil {
		return nil, err
	}
	_, err = s.persistChannelManagementState(
		operationCtx,
		cfg,
		envelope,
		func(state *model.ChannelManagementState) {
			state.LastCheckinStatus = result.Status
			state.LastCheckinAt = result.CheckedInAt
			if snapshot != nil {
				state.LastBalance = snapshot
			}
		},
	)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *channelManagementService) loadChannelManagement(
	ctx context.Context,
	channelID int64,
) (*model.Config, *model.ChannelManagementEnvelope, error) {
	if s == nil || s.store == nil || channelID <= 0 {
		return nil, nil, errInvalidManagementRequest
	}
	cfg, err := s.store.GetConfig(ctx, channelID)
	if err != nil {
		return nil, nil, err
	}
	if cfg.AuthType != model.AuthTypeAPIKey || strings.TrimSpace(cfg.OAuthCredential) == "" {
		return nil, nil, errChannelManagementNotConfigured
	}
	envelope, err := model.ParseChannelManagementEnvelope(cfg.OAuthCredential)
	if err != nil {
		return nil, nil, errInvalidManagementResponse
	}
	return cfg, envelope, nil
}
func (s *channelManagementService) persistChannelManagementState(
	ctx context.Context,
	cfg *model.Config,
	source *model.ChannelManagementEnvelope,
	merge func(*model.ChannelManagementState),
) (*model.ChannelManagementEnvelope, error) {
	if cfg == nil || source == nil || merge == nil {
		return nil, errInvalidManagementRequest
	}
	currentCfg := cfg
	current := source
	for {
		if !sameChannelManagementIdentity(source, current) {
			return nil, errInvalidManagementRequest
		}
		merge(&current.State)
		nextRaw, err := current.Marshal()
		if err != nil {
			return nil, errInvalidManagementResponse
		}
		updated, err := s.store.CompareAndSwapChannelManagement(
			ctx,
			currentCfg.ID,
			currentCfg.OAuthCredential,
			nextRaw,
		)
		if err != nil {
			return nil, err
		}
		if updated {
			return current, nil
		}
		currentCfg, current, err = s.loadChannelManagement(ctx, currentCfg.ID)
		if err != nil {
			return nil, err
		}
	}
}

func (s *channelManagementService) acquireChannel(ctx context.Context, channelID int64) (func(), error) {
	if s == nil || channelID <= 0 {
		return nil, errInvalidManagementRequest
	}
	gateValue, ok := s.gates.Load(channelID)
	if !ok {
		newGate := make(chan struct{}, 1)
		gateValue, _ = s.gates.LoadOrStore(channelID, newGate)
	}
	gate := gateValue.(chan struct{})
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func mergeChannelManagementSettings(
	currentRaw string,
	input *channelManagementInput,
) (*model.ChannelManagementEnvelope, string, error) {
	if input == nil {
		return nil, "", errInvalidManagementRequest
	}
	profile := strings.TrimSpace(input.Profile)
	if profile == "" {
		return nil, "", nil
	}

	var current *model.ChannelManagementEnvelope
	var err error
	if strings.TrimSpace(currentRaw) != "" {
		current, err = model.ParseChannelManagementEnvelope(currentRaw)
		if err != nil {
			return nil, "", errInvalidManagementResponse
		}
	}
	accessToken := input.AccessToken
	if strings.TrimSpace(accessToken) == "" {
		if current == nil || current.Profile != profile {
			return nil, "", errInvalidManagementRequest
		}
		accessToken = current.Settings.AccessToken
	}

	next := &model.ChannelManagementEnvelope{
		Kind:    model.ChannelManagementKind,
		Version: model.ChannelManagementVersion,
		Profile: profile,
		Settings: model.ChannelManagementSettings{
			BaseURL:             input.BaseURL,
			AccessToken:         accessToken,
			UserID:              input.UserID,
			DailyCheckinEnabled: input.DailyCheckinEnabled,
			DailyCheckinTime:    input.DailyCheckinTime,
		},
	}
	if err = next.Validate(); err != nil {
		return nil, "", fmt.Errorf("%w: %v", errInvalidManagementRequest, err)
	}
	if input.UserID == nil && sameChannelManagementIdentityWithoutUserID(current, next) &&
		current.Settings.UserID != nil {
		userID := *current.Settings.UserID
		next.Settings.UserID = &userID
	}
	if sameChannelManagementIdentity(current, next) {
		next.State = current.State
	}
	nextRaw, err := next.Marshal()
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", errInvalidManagementRequest, err)
	}
	return next, nextRaw, nil
}

func sameChannelManagementIdentityWithoutUserID(
	current *model.ChannelManagementEnvelope,
	next *model.ChannelManagementEnvelope,
) bool {
	return current != nil && next != nil &&
		current.Profile == next.Profile &&
		current.Settings.BaseURL == next.Settings.BaseURL &&
		current.Settings.AccessToken == next.Settings.AccessToken
}

func sameChannelManagementIdentity(
	current *model.ChannelManagementEnvelope,
	next *model.ChannelManagementEnvelope,
) bool {
	return sameChannelManagementIdentityWithoutUserID(current, next) &&
		equalChannelManagementUserID(current.Settings.UserID, next.Settings.UserID)
}

func equalChannelManagementUserID(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func channelManagementViewFromEnvelope(envelope *model.ChannelManagementEnvelope) *channelManagementView {
	if envelope == nil {
		return &channelManagementView{}
	}
	return &channelManagementView{
		Profile:              envelope.Profile,
		BaseURL:              envelope.Settings.BaseURL,
		UserIDConfigured:     envelope.Settings.UserID != nil,
		DailyCheckinEnabled:  envelope.Settings.DailyCheckinEnabled,
		DailyCheckinTime:     envelope.Settings.DailyCheckinTime,
		CredentialConfigured: strings.TrimSpace(envelope.Settings.AccessToken) != "",
		LastCheckinStatus:    envelope.State.LastCheckinStatus,
		LastCheckinAt:        envelope.State.LastCheckinAt,
		Balance:              channelManagementBalanceViewFromSnapshot(envelope.State.LastBalance),
	}
}

func channelManagementBalanceViewFromSnapshot(
	snapshot *model.ChannelManagementBalanceSnapshot,
) *channelManagementBalanceView {
	if snapshot == nil {
		return nil
	}
	view := &channelManagementBalanceView{
		Unit:          "USD",
		Subscriptions: append([]model.ChannelManagementSubscriptionSnapshot(nil), snapshot.Subscriptions...),
		SampledAt:     snapshot.SampledAt,
	}
	if snapshot.BalanceUSD != nil {
		view.Remaining = *snapshot.BalanceUSD
		return view
	}
	if snapshot.RemainingRaw == nil || snapshot.Divisor <= 0 {
		return nil
	}
	divisor := float64(snapshot.Divisor)
	view.Remaining = float64(*snapshot.RemainingRaw) / divisor
	if snapshot.UsedRaw != nil {
		used := float64(*snapshot.UsedRaw) / divisor
		view.Used = &used
	}
	if snapshot.TotalRaw != nil {
		total := float64(*snapshot.TotalRaw) / divisor
		view.Total = &total
		if total > 0 {
			available := view.Remaining / total * 100
			view.AvailablePercent = &available
		}
	}
	return view
}

func (s *channelManagementService) doManagementRequest(
	ctx context.Context,
	cfg *model.Config,
	method string,
	target string,
	accessToken string,
	body []byte,
	extraHeaders ...http.Header,
) (*managementHTTPResult, error) {
	client, err := s.managementClient(cfg)
	if err != nil || strings.TrimSpace(accessToken) == "" {
		return nil, errInvalidManagementRequest
	}
	request, err := http.NewRequestWithContext(ctx, method, target, nil)
	if err != nil {
		return nil, errInvalidManagementRequest
	}
	if body != nil {
		request.Body = io.NopCloser(bytes.NewReader(body))
		request.ContentLength = int64(len(body))
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	for _, headers := range extraHeaders {
		for name, values := range headers {
			request.Header[name] = append([]string(nil), values...)
		}
	}
	if strings.EqualFold(request.URL.Scheme, "https") {
		request = withChromeUTLS(request)
	}

	var wroteRequest atomic.Bool
	if method == http.MethodPost {
		trace := &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) {
			wroteRequest.Store(true)
		}}
		request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	}

	response, requestErr := client.Do(request)
	result := &managementHTTPResult{WroteRequest: wroteRequest.Load()}
	if requestErr != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		return result, errManagementRequestFailed
	}
	defer func() {
		_ = response.Body.Close()
	}()
	result.StatusCode = response.StatusCode
	limitedBody, readErr := io.ReadAll(io.LimitReader(response.Body, managementResponseBodyLimit+1))
	result.WroteRequest = wroteRequest.Load()
	if readErr != nil || len(limitedBody) > managementResponseBodyLimit {
		return result, errInvalidManagementResponse
	}
	result.Body = limitedBody
	return result, nil
}

func (s *channelManagementService) managementClient(cfg *model.Config) (*http.Client, error) {
	if s == nil || s.clientForChannel == nil || cfg == nil {
		return nil, errInvalidManagementRequest
	}
	base := s.clientForChannel(cfg)
	if base == nil {
		return nil, errInvalidManagementRequest
	}
	client := *base
	client.Timeout = 0
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &client, nil
}
