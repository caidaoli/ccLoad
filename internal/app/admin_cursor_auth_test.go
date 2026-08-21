package app

import (
	"context"
	"testing"

	"ccLoad/internal/cursorauth"
	"ccLoad/internal/model"
	"ccLoad/internal/testutil"
)

func TestNewCursorOAuthChannelUsesCLIOrigin(t *testing.T) {
	t.Parallel()
	channel := newCursorOAuthChannel("Cursor-user@example.com", `{"type":"cursor","access_token":"tok"}`, nil)
	if channel.AuthType != model.AuthTypeCursorOAuth || !channel.Enabled || channel.CostMultiplier != 1 {
		t.Fatalf("channel = %+v", channel)
	}
	if len(channel.URLs) != 1 || channel.URLs[0].URL != cursorauth.APIBaseURL {
		t.Fatalf("urls = %+v", channel.URLs)
	}
	if len(channel.URLs[0].Protocols) != 2 {
		t.Fatalf("protocols = %+v", channel.URLs[0].Protocols)
	}
	if channel.ProtocolTransformMode != model.ProtocolTransformModeLocal {
		t.Fatalf("protocol transform mode = %q", channel.ProtocolTransformMode)
	}
	if len(channel.ModelEntries) != len(cursorauth.DefaultModels) {
		t.Fatalf("models = %+v", channel.ModelEntries)
	}
}

func TestCreateOrUpdateCursorChannelReauthorizesSameAccount(t *testing.T) {
	store, cleanup := testutil.SetupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	first := &cursorauth.Credential{AccessToken: "tok-1", RefreshToken: "ref-1", Email: "user@example.com", UserID: "auth-1"}
	created, isNew, err := createOrUpdateCursorChannel(ctx, store, first, nil)
	if err != nil || !isNew {
		t.Fatalf("createOrUpdateCursorChannel() created=%v err=%v", isNew, err)
	}
	if created.Name != "Cursor-user@example.com" {
		t.Fatalf("name = %q", created.Name)
	}

	second := &cursorauth.Credential{AccessToken: "tok-2", Email: "USER@example.com"}
	updated, isNew, err := createOrUpdateCursorChannel(ctx, store, second, nil)
	if err != nil || isNew {
		t.Fatalf("reauthorization created=%v err=%v", isNew, err)
	}
	if updated.ID != created.ID {
		t.Fatalf("id changed: %d -> %d", created.ID, updated.ID)
	}
	credential, err := cursorauth.ParseCredential([]byte(updated.OAuthCredential))
	if err != nil || credential.AccessToken != "tok-2" || credential.RefreshToken != "ref-1" {
		t.Fatalf("credential = %+v err = %v", credential, err)
	}
}
