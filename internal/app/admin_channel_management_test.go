package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"

	"github.com/gin-gonic/gin"
)

const managementAccountSecret = "management-private-token-never-serialize"

func validManagedChannelPayload(name string) map[string]any {
	return map[string]any{
		"name":      name,
		"auth_type": model.AuthTypeAPIKey,
		"api_key":   "sk-admin-channel-key",
		"urls":      []map[string]any{{"url": "https://api.example.com"}},
		"models":    []map[string]any{{"model": "gpt-test"}},
		"enabled":   true,
		"management_account": map[string]any{
			"profile":      model.ChannelManagementProfileSub2API,
			"base_url":     "https://panel.example.com/",
			"access_token": managementAccountSecret,
		},
	}
}

func createManagedChannelThroughHandler(t *testing.T, server *Server, name string) *model.Config {
	t.Helper()
	payload := validManagedChannelPayload(name)
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels", payload))
	server.handleCreateChannel(c)
	if w.Code != http.StatusCreated {
		t.Fatalf("create managed channel status=%d body=%s", w.Code, w.Body.String())
	}
	var response APIResponse[model.Config]
	mustUnmarshalJSON(t, w.Body.Bytes(), &response)
	stored, err := server.store.GetConfig(context.Background(), response.Data.ID)
	if err != nil {
		t.Fatalf("GetConfig(%d): %v", response.Data.ID, err)
	}
	return stored
}

func TestChannelManagementCreatePersistsPrivateEnvelope(t *testing.T) {
	server := newInMemoryServer(t)
	stored := createManagedChannelThroughHandler(t, server, "managed-create")
	if stored.OAuthCredential == "" {
		t.Fatal("management account was not persisted")
	}
	envelope, err := model.ParseChannelManagementEnvelope(stored.OAuthCredential)
	if err != nil {
		t.Fatalf("ParseChannelManagementEnvelope: %v", err)
	}
	if envelope.Profile != model.ChannelManagementProfileSub2API ||
		envelope.Settings.BaseURL != "https://panel.example.com" ||
		envelope.Settings.AccessToken != managementAccountSecret {
		t.Fatalf("persisted envelope = %#v", envelope)
	}
}

func TestChannelManagementCreateRejectsCredentialFieldsByPresence(t *testing.T) {
	fields := []string{"oauth_credential", "credential", "access_token"}
	values := []any{"", "top-level-private", nil}
	for _, field := range fields {
		for _, value := range values {
			t.Run(fmt.Sprintf("%s=%v", field, value), func(t *testing.T) {
				server := newInMemoryServer(t)
				payload := validManagedChannelPayload("reject-create-" + field)
				payload[field] = value
				c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels", payload))
				server.handleCreateChannel(c)
				if w.Code != http.StatusConflict {
					t.Fatalf("status=%d body=%s, want 409", w.Code, w.Body.String())
				}
				if strings.Contains(w.Body.String(), managementAccountSecret) || strings.Contains(w.Body.String(), "top-level-private") {
					t.Fatalf("credential leaked in error: %s", w.Body.String())
				}
			})
		}
	}
}

func TestChannelManagementOAuthCreateAndUpdateRejectManagementAccount(t *testing.T) {
	server := newInMemoryServer(t)

	create := validManagedChannelPayload("oauth-create-reject")
	create["auth_type"] = model.AuthTypeCodexOAuth
	delete(create, "api_key")
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels", create))
	server.handleCreateChannel(c)
	if w.Code != http.StatusConflict {
		t.Fatalf("OAuth create status=%d body=%s, want 409", w.Code, w.Body.String())
	}

	oauth, err := server.store.CreateConfig(context.Background(), &model.Config{
		Name: "oauth-update-reject", AuthType: model.AuthTypeCodexOAuth,
		OAuthCredential: `{"type":"codex","access_token":"oauth-access","refresh_token":"oauth-refresh","expired":"2030-01-01T00:00:00Z"}`,
		URLs:            model.ChannelURLs{{URL: "https://chatgpt.com/backend-api/codex/responses", Exact: true, Protocols: []string{"codex"}}},
		ModelEntries:    []model.ModelEntry{{Model: "gpt-test"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	update := validManagedChannelPayload(oauth.Name)
	update["auth_type"] = model.AuthTypeCodexOAuth
	delete(update, "api_key")
	c, w = newTestContext(t, newJSONRequest(t, http.MethodPut, fmt.Sprintf("/admin/channels/%d", oauth.ID), update))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(oauth.ID)}}
	server.HandleChannelByID(c)
	if w.Code != http.StatusConflict {
		t.Fatalf("OAuth update status=%d body=%s, want 409", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), managementAccountSecret) || strings.Contains(w.Body.String(), "oauth-access") {
		t.Fatalf("credential leaked in OAuth rejection: %s", w.Body.String())
	}
}

func TestChannelManagementUpdateRejectsEffectiveOAuthAuthType(t *testing.T) {
	server := newInMemoryServer(t)
	stored := createManagedChannelThroughHandler(t, server, "managed-auth-transition")
	payload := validManagedChannelPayload(stored.Name)
	payload["auth_type"] = " CODEX_OAUTH "
	payload["management_account"] = map[string]any{
		"profile":      model.ChannelManagementProfileSub2API,
		"base_url":     "https://replacement.example.com",
		"access_token": "replacement-secret",
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, fmt.Sprintf("/admin/channels/%d", stored.ID), payload))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(stored.ID)}}
	server.HandleChannelByID(c)
	if w.Code != http.StatusConflict {
		t.Fatalf("auth transition status=%d body=%s, want 409", w.Code, w.Body.String())
	}
	persisted, err := server.store.GetConfig(context.Background(), stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.GetAuthType() != model.AuthTypeAPIKey || !strings.Contains(persisted.OAuthCredential, managementAccountSecret) {
		t.Fatalf("rejected auth transition changed persisted channel: %#v", persisted)
	}
	for _, secret := range []string{managementAccountSecret, "replacement-secret"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("auth transition rejection leaked %q: %s", secret, w.Body.String())
		}
	}
}

func TestChannelManagementUpdateRejectsUnknownAuthType(t *testing.T) {
	server := newInMemoryServer(t)
	stored := createManagedChannelThroughHandler(t, server, "managed-auth-typo")
	payload := validManagedChannelPayload(stored.Name)
	payload["auth_type"] = "codex_oautn"
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, fmt.Sprintf("/admin/channels/%d", stored.ID), payload))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(stored.ID)}}
	server.HandleChannelByID(c)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid auth_type") {
		t.Fatalf("unknown auth type status=%d body=%s, want 400 invalid auth_type", w.Code, w.Body.String())
	}
	persisted, err := server.store.GetConfig(context.Background(), stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.GetAuthType() != model.AuthTypeAPIKey || persisted.OAuthCredential == "" {
		t.Fatalf("unknown auth type changed persisted channel: %#v", persisted)
	}
}

func TestChannelManagementUpdatePreservesOrClearsCredentialByProfile(t *testing.T) {
	server := newInMemoryServer(t)
	stored := createManagedChannelThroughHandler(t, server, "managed-update")

	update := validManagedChannelPayload(stored.Name)
	update["management_account"] = map[string]any{
		"profile":      model.ChannelManagementProfileSub2API,
		"base_url":     "https://panel.example.com/",
		"access_token": "",
	}
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, fmt.Sprintf("/admin/channels/%d", stored.ID), update))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(stored.ID)}}
	server.HandleChannelByID(c)
	if w.Code != http.StatusOK {
		t.Fatalf("same-profile update status=%d body=%s", w.Code, w.Body.String())
	}
	stored, err := server.store.GetConfig(context.Background(), stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := model.ParseChannelManagementEnvelope(stored.OAuthCredential)
	if err != nil || envelope.Settings.AccessToken != managementAccountSecret {
		t.Fatalf("empty token did not preserve credential: envelope=%#v err=%v", envelope, err)
	}

	update["management_account"] = map[string]any{"profile": ""}
	c, w = newTestContext(t, newJSONRequest(t, http.MethodPut, fmt.Sprintf("/admin/channels/%d", stored.ID), update))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(stored.ID)}}
	server.HandleChannelByID(c)
	if w.Code != http.StatusOK {
		t.Fatalf("disable update status=%d body=%s", w.Code, w.Body.String())
	}
	stored, err = server.store.GetConfig(context.Background(), stored.ID)
	if err != nil || stored.OAuthCredential != "" {
		t.Fatalf("profile disable did not clear management envelope: credential=%q err=%v", stored.OAuthCredential, err)
	}
}

func TestChannelManagementUpdateRejectsCredentialFieldsByPresence(t *testing.T) {
	for _, field := range []string{"oauth_credential", "credential", "access_token"} {
		for _, value := range []any{"", "top-level-private", nil} {
			t.Run(fmt.Sprintf("%s=%v", field, value), func(t *testing.T) {
				server := newInMemoryServer(t)
				stored := createManagedChannelThroughHandler(t, server, "reject-update-"+field)
				payload := validManagedChannelPayload(stored.Name)
				payload[field] = value
				c, w := newTestContext(t, newJSONRequest(t, http.MethodPut, fmt.Sprintf("/admin/channels/%d", stored.ID), payload))
				c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(stored.ID)}}
				server.HandleChannelByID(c)
				if w.Code != http.StatusConflict {
					t.Fatalf("status=%d body=%s, want 409", w.Code, w.Body.String())
				}
				if strings.Contains(w.Body.String(), managementAccountSecret) || strings.Contains(w.Body.String(), "top-level-private") {
					t.Fatalf("credential leaked in error: %s", w.Body.String())
				}
			})
		}
	}
}

func TestChannelManagementResponsesLimitCredentialsToEditor(t *testing.T) {
	server := newInMemoryServer(t)
	stored := createManagedChannelThroughHandler(t, server, "managed-views")

	requests := []struct {
		name string
		call func() string
	}{
		{name: "list", call: func() string {
			c, w := newTestContext(t, newRequest(http.MethodGet, "/admin/channels", nil))
			server.HandleChannels(c)
			return w.Body.String()
		}},
		{name: "detail", call: func() string {
			c, w := newTestContext(t, newRequest(http.MethodGet, fmt.Sprintf("/admin/channels/%d", stored.ID), nil))
			c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(stored.ID)}}
			server.HandleChannelByID(c)
			return w.Body.String()
		}},
		{name: "editor", call: func() string {
			c, w := newTestContext(t, newRequest(http.MethodGet, fmt.Sprintf("/admin/channels/%d/editor", stored.ID), nil))
			c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(stored.ID)}}
			server.HandleChannelEditor(c)
			return w.Body.String()
		}},
	}
	for _, request := range requests {
		t.Run(request.name, func(t *testing.T) {
			body := request.call()
			if !strings.Contains(body, `"management_account"`) || !strings.Contains(body, `"credential_configured":true`) {
				t.Fatalf("redacted management view missing: %s", body)
			}
			if request.name == "editor" {
				if !strings.Contains(body, `"access_token":"`+managementAccountSecret+`"`) {
					t.Fatalf("editor management credential missing: %s", body)
				}
				return
			}
			for _, secret := range []string{managementAccountSecret, `"channel_management"`, `"access_token"`, `"settings"`} {
				if strings.Contains(body, secret) {
					t.Fatalf("%s response leaked %q: %s", request.name, secret, body)
				}
			}
		})
	}
}

func TestChannelManagementEditorExposesCredentialsOnlyInEditor(t *testing.T) {
	server := newInMemoryServer(t)
	stored := createManagedChannelThroughHandler(t, server, "managed-oauth-guards")

	editorCtx, editorW := newTestContext(t, newRequest(http.MethodGet, fmt.Sprintf("/admin/channels/%d/editor", stored.ID), nil))
	editorCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprint(stored.ID)}}
	server.HandleChannelEditor(editorCtx)
	if editorW.Code != http.StatusOK || strings.Contains(editorW.Body.String(), `"oauth_credential"`) ||
		!strings.Contains(editorW.Body.String(), `"access_token":"`+managementAccountSecret+`"`) {
		t.Fatalf("editor credential response invalid: status=%d body=%s", editorW.Code, editorW.Body.String())
	}

	keysCtx, keysW := newTestContext(t, newRequest(http.MethodGet, fmt.Sprintf("/admin/channels/%d/keys", stored.ID), nil))
	keysCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprint(stored.ID)}}
	server.HandleChannelKeys(keysCtx)
	if keysW.Code != http.StatusOK || strings.Contains(keysW.Body.String(), managementAccountSecret) || strings.Contains(keysW.Body.String(), `"channel_management"`) {
		t.Fatalf("key copy surface exposed private envelope: status=%d body=%s", keysW.Code, keysW.Body.String())
	}

	refreshCtx, refreshW := newTestContext(t, newRequest(http.MethodPost, fmt.Sprintf("/admin/channels/%d/codex-credential/refresh", stored.ID), nil))
	refreshCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprint(stored.ID)}}
	server.HandleRefreshCodexCredential(refreshCtx)
	if refreshW.Code != http.StatusConflict || strings.Contains(refreshW.Body.String(), managementAccountSecret) || strings.Contains(refreshW.Body.String(), `"channel_management"`) {
		t.Fatalf("refresh guard status=%d body=%s", refreshW.Code, refreshW.Body.String())
	}
}

func TestChannelManagementEditorExposesSavedUserID(t *testing.T) {
	server := newInMemoryServer(t)
	userID := int64(42)
	stored := seedManagementEnvelope(t, server, "managed-editor-user-id", &model.ChannelManagementEnvelope{
		Kind: model.ChannelManagementKind, Version: model.ChannelManagementVersion,
		Profile: model.ChannelManagementProfileNewAPI,
		Settings: model.ChannelManagementSettings{
			BaseURL: "https://panel.example.com", AccessToken: managementAccountSecret, UserID: &userID,
		},
	})

	c, w := newTestContext(t, newRequest(http.MethodGet, fmt.Sprintf("/admin/channels/%d/editor", stored.ID), nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(stored.ID)}}
	server.HandleChannelEditor(c)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"user_id":42`) {
		t.Fatalf("editor user ID missing: status=%d body=%s", w.Code, w.Body.String())
	}
}

type managementCreateCASFailureStore struct {
	storage.Store
}

func (s *managementCreateCASFailureStore) CompareAndSwapChannelManagement(context.Context, int64, string, string) (bool, error) {
	return false, errors.New("forced management CAS failure")
}

func TestChannelManagementCreateRollsBackConfigBeforeCreatingKeys(t *testing.T) {
	server := newInMemoryServer(t)
	failing := &managementCreateCASFailureStore{Store: server.store}
	server.store = failing
	server.channelManagement = newChannelManagementService(failing, server.getClientForChannel)

	payload := validManagedChannelPayload("managed-create-rollback")
	c, w := newTestContext(t, newJSONRequest(t, http.MethodPost, "/admin/channels", payload))
	server.handleCreateChannel(c)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want 500", w.Code, w.Body.String())
	}
	configs, err := failing.ListConfigs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 0 {
		t.Fatalf("failed management save left config behind: %#v", configs)
	}
	if strings.Contains(w.Body.String(), managementAccountSecret) || strings.Contains(w.Body.String(), "forced management CAS failure") {
		t.Fatalf("unsafe internal error exposed: %s", w.Body.String())
	}
}

func seedManagementEnvelope(t *testing.T, server *Server, name string, envelope *model.ChannelManagementEnvelope) *model.Config {
	t.Helper()
	cfg, err := server.store.CreateConfig(context.Background(), &model.Config{
		Name: name, AuthType: model.AuthTypeAPIKey,
		URLs:         model.ChannelURLs{{URL: "https://api.example.com"}},
		ModelEntries: []model.ModelEntry{{Model: "gpt-test"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := envelope.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	updated, err := server.store.CompareAndSwapChannelManagement(context.Background(), cfg.ID, "", raw)
	if err != nil || !updated {
		t.Fatalf("seed management envelope: updated=%t err=%v", updated, err)
	}
	cfg.OAuthCredential = raw
	return cfg
}

func TestChannelManagementRoutesRegistered(t *testing.T) {
	server := newInMemoryServer(t)
	engine := gin.New()
	server.SetupRoutes(engine)
	routes := make(map[string]struct{})
	for _, route := range engine.Routes() {
		routes[route.Method+" "+route.Path] = struct{}{}
	}
	for _, route := range []string{
		"POST /admin/channels/:id/management-account/balance",
		"POST /admin/channels/:id/management-account/checkin",
	} {
		if _, ok := routes[route]; !ok {
			t.Errorf("route %q is not registered", route)
		}
	}
}

func TestChannelManagementBalanceAndCheckinHandlers(t *testing.T) {
	server := newInMemoryServer(t)
	userID := int64(42)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/user/self" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"quota":1500000,"used_quota":500000}}`))
	}))
	balanceChannel := seedManagementEnvelope(t, server, "balance-handler", &model.ChannelManagementEnvelope{
		Kind: model.ChannelManagementKind, Version: model.ChannelManagementVersion,
		Profile: model.ChannelManagementProfileNewAPI,
		Settings: model.ChannelManagementSettings{
			BaseURL: upstream.URL, AccessToken: managementAccountSecret, UserID: &userID,
		},
	})

	balanceCtx, balanceW := newTestContext(t, newRequest(http.MethodPost, fmt.Sprintf("/admin/channels/%d/management-account/balance", balanceChannel.ID), nil))
	balanceCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprint(balanceChannel.ID)}}
	server.HandleChannelManagementBalance(balanceCtx)
	if balanceW.Code != http.StatusOK {
		t.Fatalf("balance status=%d body=%s", balanceW.Code, balanceW.Body.String())
	}
	if !strings.Contains(balanceW.Body.String(), `"remaining":3`) ||
		!strings.Contains(balanceW.Body.String(), `"credential_configured":true`) {
		t.Fatalf("balance response missing redacted view: %s", balanceW.Body.String())
	}
	if strings.Contains(balanceW.Body.String(), managementAccountSecret) {
		t.Fatalf("balance response leaked credential: %s", balanceW.Body.String())
	}

	checkinChannel := seedManagementEnvelope(t, server, "checkin-handler", &model.ChannelManagementEnvelope{
		Kind: model.ChannelManagementKind, Version: model.ChannelManagementVersion,
		Profile: model.ChannelManagementProfileSub2API,
		Settings: model.ChannelManagementSettings{
			BaseURL: "https://panel.example.com", AccessToken: managementAccountSecret,
		},
	})
	checkinCtx, checkinW := newTestContext(t, newRequest(http.MethodPost, fmt.Sprintf("/admin/channels/%d/management-account/checkin", checkinChannel.ID), nil))
	checkinCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprint(checkinChannel.ID)}}
	server.HandleChannelManagementCheckin(checkinCtx)
	if checkinW.Code != http.StatusOK || !strings.Contains(checkinW.Body.String(), `"status":"unsupported"`) {
		t.Fatalf("checkin status=%d body=%s", checkinW.Code, checkinW.Body.String())
	}
	if strings.Contains(checkinW.Body.String(), managementAccountSecret) {
		t.Fatalf("checkin response leaked credential: %s", checkinW.Body.String())
	}
}

func TestChannelManagementHandlerErrorMappingIsStableAndSafe(t *testing.T) {
	server := newInMemoryServer(t)
	oauth, err := server.store.CreateConfig(context.Background(), &model.Config{
		Name: "oauth-not-management", AuthType: model.AuthTypeCodexOAuth,
		OAuthCredential: `{"type":"codex","access_token":"oauth-error-secret","refresh_token":"oauth-refresh-secret"}`,
		URLs:            model.ChannelURLs{{URL: "https://oauth.example.com"}}, ModelEntries: []model.ModelEntry{{Model: "gpt-test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	unmanaged, err := server.store.CreateConfig(context.Background(), &model.Config{
		Name: "unmanaged-api-key", AuthType: model.AuthTypeAPIKey,
		URLs: model.ChannelURLs{{URL: "https://api.example.com"}}, ModelEntries: []model.ModelEntry{{Model: "gpt-test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	invalidUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":{"quota":"invalid","private":"invalid-body-secret"}}`))
	}))
	userID := int64(9)
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream-body-secret Authorization: Bearer private", http.StatusServiceUnavailable)
	}))
	upstreamWithDetail := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"message":"management token rejected"}`))
	}))
	businessFailureUpstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":false,"message":"quota endpoint denied"}`))
	}))
	upstreamFailure := seedManagementEnvelope(t, server, "upstream-failure", &model.ChannelManagementEnvelope{
		Kind: model.ChannelManagementKind, Version: model.ChannelManagementVersion,
		Profile:  model.ChannelManagementProfileNewAPI,
		Settings: model.ChannelManagementSettings{BaseURL: upstream.URL, AccessToken: managementAccountSecret, UserID: &userID},
	})
	upstreamDetailFailure := seedManagementEnvelope(t, server, "upstream-detail-failure", &model.ChannelManagementEnvelope{
		Kind: model.ChannelManagementKind, Version: model.ChannelManagementVersion,
		Profile:  model.ChannelManagementProfileNewAPI,
		Settings: model.ChannelManagementSettings{BaseURL: upstreamWithDetail.URL, AccessToken: managementAccountSecret, UserID: &userID},
	})
	businessFailure := seedManagementEnvelope(t, server, "business-failure", &model.ChannelManagementEnvelope{
		Kind: model.ChannelManagementKind, Version: model.ChannelManagementVersion,
		Profile:  model.ChannelManagementProfileNewAPI,
		Settings: model.ChannelManagementSettings{BaseURL: businessFailureUpstream.URL, AccessToken: managementAccountSecret, UserID: &userID},
	})
	invalidResponse := seedManagementEnvelope(t, server, "invalid-response", &model.ChannelManagementEnvelope{
		Kind: model.ChannelManagementKind, Version: model.ChannelManagementVersion,
		Profile:  model.ChannelManagementProfileNewAPI,
		Settings: model.ChannelManagementSettings{BaseURL: invalidUpstream.URL, AccessToken: managementAccountSecret, UserID: &userID},
	})

	tests := []struct {
		name       string
		channelID  int64
		param      string
		wantStatus int
		wantError  string
	}{
		{name: "OAuth", channelID: oauth.ID, wantStatus: http.StatusConflict, wantError: "credential_invalid"},
		{name: "unmanaged API key", channelID: unmanaged.ID, wantStatus: http.StatusConflict, wantError: "credential_invalid"},
		{name: "invalid response", channelID: invalidResponse.ID, wantStatus: http.StatusBadGateway, wantError: "invalid_response"},
		{name: "upstream failure", channelID: upstreamFailure.ID, wantStatus: http.StatusBadGateway, wantError: "upstream_error"},
		{name: "upstream detail", channelID: upstreamDetailFailure.ID, wantStatus: http.StatusBadGateway, wantError: "management token rejected"},
		{name: "business failure detail", channelID: businessFailure.ID, wantStatus: http.StatusBadGateway, wantError: "quota endpoint denied"},
		{name: "missing channel", channelID: 999999, wantStatus: http.StatusNotFound, wantError: "credential_invalid"},
		{name: "invalid channel id", param: "invalid", wantStatus: http.StatusBadRequest, wantError: "invalid_response"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			param := test.param
			if param == "" {
				param = fmt.Sprint(test.channelID)
			}
			c, w := newTestContext(t, newRequest(http.MethodPost, "/admin/channels/"+param+"/management-account/balance", nil))
			c.Params = gin.Params{{Key: "id", Value: param}}
			server.HandleChannelManagementBalance(c)
			if w.Code != test.wantStatus {
				t.Fatalf("status=%d body=%s, want %d", w.Code, w.Body.String(), test.wantStatus)
			}
			response := mustParseAPIResponse[any](t, w.Body.Bytes())
			if response.Error != test.wantError {
				t.Fatalf("error=%q body=%s, want %q", response.Error, w.Body.String(), test.wantError)
			}
			for _, secret := range []string{managementAccountSecret, "oauth-error-secret", "oauth-refresh-secret", "invalid-body-secret", "upstream-body-secret", "Authorization", "Bearer private"} {
				if strings.Contains(w.Body.String(), secret) {
					t.Fatalf("safe error leaked %q: %s", secret, w.Body.String())
				}
			}
		})
	}
}

func TestChannelManagementCheckinAuditMatrix(t *testing.T) {
	fixedNow := time.Date(2026, time.August, 25, 9, 30, 0, 0, time.UTC)
	statusEnabled := newAPIStep{
		method: http.MethodGet, target: "https://panel.example.com/api/status",
		status: http.StatusOK, body: `{"success":true,"data":{"checkin_enabled":true}}`,
	}
	monthUnchecked := newAPIStep{
		method: http.MethodGet, target: "https://panel.example.com/api/user/checkin?month=2026-08",
		status: http.StatusOK, body: `{"success":true,"data":{"stats":{"checked_in_today":false}}}`,
	}
	balance := newAPIStep{
		method: http.MethodGet, target: "https://panel.example.com/api/user/self",
		status: http.StatusOK, body: `{"success":true,"data":{"id":42,"quota":750000,"used_quota":250000}}`,
	}
	tests := []struct {
		name           string
		steps          []newAPIStep
		wantHTTPStatus int
		wantStatus     string
		wantLogStatus  int
		wantRemaining  string
	}{
		{
			name: "success",
			steps: []newAPIStep{statusEnabled, monthUnchecked, {
				method: http.MethodPost, target: "https://panel.example.com/api/user/checkin",
				status: http.StatusOK, wrote: true,
				body: `{"success":true,"data":{"quota_awarded":250000,"checkin_date":"2026-08-25"}}`,
			}, balance},
			wantHTTPStatus: http.StatusOK, wantStatus: "success", wantLogStatus: http.StatusOK, wantRemaining: "1.5",
		},
		{
			name: "manual required",
			steps: []newAPIStep{statusEnabled, monthUnchecked, {
				method: http.MethodPost, target: "https://panel.example.com/api/user/checkin",
				status: http.StatusOK, wrote: true,
				body: `{"success":false,"message":"Turnstile raw-audit-body-secret"}`,
			}, monthUnchecked},
			wantHTTPStatus: http.StatusOK, wantStatus: "manual_required", wantLogStatus: http.StatusOK,
		},
		{
			name: "unsupported",
			steps: []newAPIStep{{
				method: http.MethodGet, target: "https://panel.example.com/api/status",
				status: http.StatusNotFound, body: `{"message":"raw-audit-body-secret"}`,
			}},
			wantHTTPStatus: http.StatusOK, wantStatus: "unsupported", wantLogStatus: http.StatusNotFound,
		},
		{
			name: "credential invalid",
			steps: []newAPIStep{{
				method: http.MethodGet, target: "https://panel.example.com/api/status",
				status: http.StatusUnauthorized, body: `{"message":"private-token raw-audit-body-secret"}`,
			}},
			wantHTTPStatus: http.StatusOK, wantStatus: "credential_invalid", wantLogStatus: http.StatusUnauthorized,
		},
		{
			name: "uncertain",
			steps: []newAPIStep{statusEnabled, monthUnchecked, {
				method: http.MethodPost, target: "https://panel.example.com/api/user/checkin",
				wrote: true, err: errors.New("raw-audit-body-secret private-token"),
			}, {
				method: http.MethodGet, target: "https://panel.example.com/api/user/checkin?month=2026-08",
				err: errors.New("raw-audit-body-secret"),
			}},
			wantHTTPStatus: http.StatusOK, wantStatus: "uncertain", wantLogStatus: 0,
		},
		{
			name: "infrastructure error",
			steps: []newAPIStep{{
				method: http.MethodGet, target: "https://panel.example.com/api/status",
				err: errors.New("raw-audit-body-secret private-token"),
			}},
			wantHTTPStatus: http.StatusBadGateway, wantLogStatus: 0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newInMemoryServer(t)
			userID := int64(42)
			cfg := seedManagementEnvelope(t, server, "audit-"+test.name, &model.ChannelManagementEnvelope{
				Kind: model.ChannelManagementKind, Version: model.ChannelManagementVersion,
				Profile:  model.ChannelManagementProfileNewAPI,
				Settings: model.ChannelManagementSettings{BaseURL: "https://panel.example.com", AccessToken: "private-token", UserID: &userID},
			})
			script := &newAPIScript{t: t, steps: test.steps}
			server.channelManagement.clientForChannel = func(*model.Config) *http.Client {
				return &http.Client{Transport: script}
			}
			server.channelManagement.now = func() time.Time { return fixedNow }

			c, w := newTestContext(t, newRequest(http.MethodPost, fmt.Sprintf("/admin/channels/%d/management-account/checkin", cfg.ID), nil))
			c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(cfg.ID)}}
			server.HandleChannelManagementCheckin(c)
			if w.Code != test.wantHTTPStatus {
				t.Fatalf("status=%d body=%s, want %d", w.Code, w.Body.String(), test.wantHTTPStatus)
			}
			if test.wantStatus != "" && !strings.Contains(w.Body.String(), `"status":"`+test.wantStatus+`"`) {
				t.Fatalf("response body=%s, want status %q", w.Body.String(), test.wantStatus)
			}
			script.finishedRequests()

			logs, err := server.store.ListLogs(context.Background(), fixedNow.Add(-time.Hour), 10, 0, &model.LogFilter{LogSource: model.LogSourceCheckin})
			if err != nil {
				t.Fatal(err)
			}
			if len(logs) != 1 {
				t.Fatalf("checkin logs=%d, want exactly one: %#v", len(logs), logs)
			}
			entry := logs[0]
			if entry.ChannelID != cfg.ID || entry.LogSource != model.LogSourceCheckin || entry.StatusCode != test.wantLogStatus {
				t.Fatalf("audit entry=%#v, want channel=%d source=checkin status=%d", entry, cfg.ID, test.wantLogStatus)
			}
			var auditMessage struct {
				Profile string `json:"profile"`
				Status  string `json:"status"`
				Balance *struct {
					Remaining float64 `json:"remaining"`
				} `json:"balance"`
			}
			if err := json.Unmarshal([]byte(entry.Message), &auditMessage); err != nil {
				t.Fatalf("invalid audit message: %v", err)
			}
			if auditMessage.Profile != model.ChannelManagementProfileNewAPI {
				t.Fatalf("audit message profile=%q", auditMessage.Profile)
			}
			if test.wantStatus != "" && auditMessage.Status != test.wantStatus {
				t.Fatalf("audit message status=%q, want %q", auditMessage.Status, test.wantStatus)
			}
			if test.wantRemaining != "" && fmt.Sprint(auditMessage.Balance.Remaining) != test.wantRemaining {
				t.Fatalf("audit message balance=%#v, want remaining %s", auditMessage.Balance, test.wantRemaining)
			}
			serialized, err := json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"private-token", "raw-audit-body-secret", "Authorization", "Bearer"} {
				if strings.Contains(string(serialized), secret) {
					t.Fatalf("audit serialized secret %q: %s", secret, serialized)
				}
			}
		})
	}
}

type checkinAddLogFailureStore struct {
	storage.Store
}

func (s *checkinAddLogFailureStore) AddLog(context.Context, *model.LogEntry) error {
	return errors.New("forced audit storage failure")
}

func TestChannelManagementCheckinAddLogFailureOverridesSuccess(t *testing.T) {
	server := newInMemoryServer(t)
	failing := &checkinAddLogFailureStore{Store: server.store}
	server.store = failing
	server.channelManagement.store = failing
	cfg := seedManagementEnvelope(t, server, "audit-storage-failure", &model.ChannelManagementEnvelope{
		Kind: model.ChannelManagementKind, Version: model.ChannelManagementVersion,
		Profile:  model.ChannelManagementProfileSub2API,
		Settings: model.ChannelManagementSettings{BaseURL: "https://panel.example.com", AccessToken: "private-token"},
	})
	c, w := newTestContext(t, newRequest(http.MethodPost, fmt.Sprintf("/admin/channels/%d/management-account/checkin", cfg.ID), nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(cfg.ID)}}
	server.HandleChannelManagementCheckin(c)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s, want audit failure to override checkin result", w.Code, w.Body.String())
	}
	response := mustParseAPIResponse[any](t, w.Body.Bytes())
	if response.Error != "uncertain" {
		t.Fatalf("audit failure error=%q body=%s, want uncertain", response.Error, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "unsupported") || strings.Contains(w.Body.String(), "forced audit storage failure") {
		t.Fatalf("handler falsely reported completion or leaked storage error: %s", w.Body.String())
	}
}

func TestChannelManagementBalanceDoesNotWriteCheckinLog(t *testing.T) {
	server := newInMemoryServer(t)
	userID := int64(42)
	server.channelManagement.clientForChannel = func(*model.Config) *http.Client {
		return &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"success":true,"data":{"quota":750000}}`)), Request: req}, nil
		})}
	}
	cfg := seedManagementEnvelope(t, server, "balance-no-audit", &model.ChannelManagementEnvelope{
		Kind: model.ChannelManagementKind, Version: model.ChannelManagementVersion,
		Profile:  model.ChannelManagementProfileNewAPI,
		Settings: model.ChannelManagementSettings{BaseURL: "https://panel.example.com", AccessToken: "private-token", UserID: &userID},
	})
	c, w := newTestContext(t, newRequest(http.MethodPost, fmt.Sprintf("/admin/channels/%d/management-account/balance", cfg.ID), nil))
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(cfg.ID)}}
	server.HandleChannelManagementBalance(c)
	if w.Code != http.StatusOK {
		t.Fatalf("balance status=%d body=%s", w.Code, w.Body.String())
	}
	logs, err := server.store.ListLogs(context.Background(), time.Now().Add(-time.Hour), 10, 0, &model.LogFilter{LogSource: model.LogSourceCheckin})
	if err != nil || len(logs) != 0 {
		t.Fatalf("balance checkin logs=%#v err=%v, want none", logs, err)
	}
}
