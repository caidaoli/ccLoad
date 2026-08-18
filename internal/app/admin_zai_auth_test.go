package app

import (
	"context"
	"strings"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/testutil"
	"ccLoad/internal/zaiauth"
)

func TestNewZAIOAuthChannelUsesZCodeRoutedEndpoint(t *testing.T) {
	t.Parallel()
	channel := newZAIOAuthChannel("Z.ai-user@example.com", `{"type":"zai","api_key":"key.secret"}`, "", nil)
	if channel.AuthType != model.AuthTypeZAIOAuth || !channel.Enabled || channel.CostMultiplier != 1 {
		t.Fatalf("channel = %+v", channel)
	}
	if len(channel.URLs) != 1 || channel.URLs[0].URL != zaiauth.CodingPlanProxyBaseURL {
		t.Fatalf("urls = %+v", channel.URLs)
	}
	if len(channel.URLs[0].Protocols) != 1 || channel.URLs[0].Protocols[0] != "anthropic" {
		t.Fatalf("protocols = %+v", channel.URLs[0].Protocols)
	}
	if channel.ProtocolTransformMode != model.ProtocolTransformModeLocal {
		t.Fatalf("protocol transform mode = %q", channel.ProtocolTransformMode)
	}
	if len(channel.ModelEntries) != len(zaiauth.DefaultModels) {
		t.Fatalf("models = %+v", channel.ModelEntries)
	}
	routed := newZAIOAuthChannel(
		"name", `{"type":"zai","api_key":"key.secret"}`,
		"https://zcode.z.ai/api/v1/next/anthropic", []string{"glm-9.9", "glm-4.7"},
	)
	if routed.URLs[0].URL != "https://zcode.z.ai/api/v1/next/anthropic" {
		t.Fatalf("routed url = %q", routed.URLs[0].URL)
	}
	// A live catalog wins over the built-in lineup.
	if len(routed.ModelEntries) != 2 || routed.ModelEntries[0].Model != "glm-9.9" {
		t.Fatalf("models = %+v", routed.ModelEntries)
	}
}

// The routed Coding Plan endpoint carries /v1 in its path, which ordinary
// channels reject. Saving a Z.ai channel from the editor must stay possible.
func TestValidateChannelBaseURLAllowsZAIRoutedEndpoint(t *testing.T) {
	t.Parallel()
	normalized, err := validateChannelBaseURL(zaiauth.CodingPlanProxyBaseURL, model.AuthTypeZAIOAuth)
	if err != nil {
		t.Fatalf("validateChannelBaseURL() error = %v", err)
	}
	if normalized != zaiauth.CodingPlanProxyBaseURL {
		t.Fatalf("normalized = %q", normalized)
	}
	if _, err := validateChannelBaseURL(zaiauth.CodingPlanProxyBaseURL, model.AuthTypeAPIKey); err == nil {
		t.Fatal("API key channels must keep rejecting endpoint paths")
	}
}

func TestCreateOrUpdateZAIChannelReauthorizesSameAccount(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	first := &zaiauth.Credential{APIKey: "key-1.secret", AccessToken: "access-1", Email: "user@example.com", UserID: "u-1"}
	created, isNew, err := createOrUpdateZAIChannel(ctx, store, first, "", nil)
	if err != nil || !isNew {
		t.Fatalf("createOrUpdateZAIChannel() created=%v err=%v", isNew, err)
	}
	if created.Name != "Z.ai-user@example.com" {
		t.Fatalf("name = %q", created.Name)
	}

	// The same account reauthorizing must land on the same channel.
	second := &zaiauth.Credential{APIKey: "key-2.secret", Email: "USER@example.com"}
	updated, isNew, err := createOrUpdateZAIChannel(ctx, store, second, "", nil)
	if err != nil || isNew {
		t.Fatalf("reauthorization created=%v err=%v", isNew, err)
	}
	if updated.ID != created.ID {
		t.Fatalf("channel id = %d, want %d", updated.ID, created.ID)
	}
	credential, err := zaiauth.ParseCredential([]byte(updated.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	if credential.APIKey != "key-2.secret" {
		t.Fatalf("api key = %q, the rotated key must win", credential.APIKey)
	}
	if credential.AccessToken != "access-1" {
		t.Fatalf("access token = %q, the account authorization must survive", credential.AccessToken)
	}

	// A different account gets its own channel.
	other := &zaiauth.Credential{APIKey: "key-3.secret", Email: "other@example.com", UserID: "u-2"}
	otherChannel, isNew, err := createOrUpdateZAIChannel(ctx, store, other, "", nil)
	if err != nil || !isNew {
		t.Fatalf("second account created=%v err=%v", isNew, err)
	}
	if otherChannel.ID == created.ID {
		t.Fatal("a different account must not reuse the channel")
	}
}

// Without an account identity the key itself is the only stable handle.
func TestCreateOrUpdateZAIChannelMatchesAnonymousKey(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	credential := &zaiauth.Credential{APIKey: "key-1.secret"}
	created, isNew, err := createOrUpdateZAIChannel(ctx, store, credential, "", nil)
	if err != nil || !isNew {
		t.Fatalf("createOrUpdateZAIChannel() created=%v err=%v", isNew, err)
	}
	if !strings.HasPrefix(created.Name, "Z.ai-") {
		t.Fatalf("name = %q", created.Name)
	}
	again, isNew, err := createOrUpdateZAIChannel(ctx, store, &zaiauth.Credential{APIKey: "key-1.secret"}, "", nil)
	if err != nil || isNew {
		t.Fatalf("re-import created=%v err=%v", isNew, err)
	}
	if again.ID != created.ID {
		t.Fatal("re-importing the same key must update in place")
	}
}

func TestUniqueZAIChannelNameAvoidsCollisions(t *testing.T) {
	t.Parallel()
	configs := []*model.Config{{Name: "Z.ai-user@example.com"}, {Name: "Z.ai-user@example.com (2)"}}
	if got := uniqueZAIChannelName(configs, "Z.ai-user@example.com"); got != "Z.ai-user@example.com (3)" {
		t.Fatalf("name = %q", got)
	}
	if got := uniqueZAIChannelName(configs, "Z.ai-free"); got != "Z.ai-free" {
		t.Fatalf("name = %q", got)
	}
}

func TestZAICredentialImportRequestValidation(t *testing.T) {
	t.Parallel()
	empty := zaiCredentialImportRequest{}
	if err := empty.Validate(); err == nil {
		t.Fatal("empty import request must be rejected")
	}
	injected := zaiCredentialImportRequest{APIKey: "key with space"}
	if err := injected.Validate(); err == nil {
		t.Fatal("keys containing whitespace must be rejected")
	}
	valid := zaiCredentialImportRequest{APIKey: "  key.secret  "}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if valid.APIKey != "key.secret" {
		t.Fatalf("api key = %q", valid.APIKey)
	}
}
