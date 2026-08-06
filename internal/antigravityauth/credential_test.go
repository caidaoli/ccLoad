package antigravityauth

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseCredentialAndRefreshMerge(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	credential, err := ParseCredential([]byte(`{"type":"antigravity","access_token":" at ","refresh_token":" rt ","expires_in":3600,"timestamp":` +
		fmtInt(now.UnixMilli()) + `,"email":" user@example.com ","project_id":" project-1 "}`))
	if err != nil {
		t.Fatalf("ParseCredential: %v", err)
	}
	if credential.AccessToken != "at" || credential.RefreshToken != "rt" || credential.ProjectID != "project-1" {
		t.Fatalf("credential = %#v", credential)
	}
	needsRefresh, err := credential.NeedsRefresh(now, 2*time.Hour)
	if err != nil || !needsRefresh {
		t.Fatalf("NeedsRefresh = (%v, %v)", needsRefresh, err)
	}
	refreshed := &Credential{Type: ChannelType, AccessToken: "new-at", ExpiresIn: 3600, Timestamp: now.UnixMilli(), Expired: now.Add(time.Hour).Format(time.RFC3339)}
	merged, err := credential.MergeRefresh(refreshed)
	if err != nil {
		t.Fatalf("MergeRefresh: %v", err)
	}
	if merged.RefreshToken != "rt" || merged.Email != "user@example.com" || merged.ProjectID != "project-1" {
		t.Fatalf("merged = %#v", merged)
	}
	raw, err := merged.JSON()
	if err != nil || !strings.Contains(raw, `"project_id":"project-1"`) {
		t.Fatalf("JSON = (%s, %v)", raw, err)
	}
}

func TestParseCredentialRejectsInvalidImport(t *testing.T) {
	for _, raw := range []string{
		`{}`,
		`{"type":"codex","access_token":"at","refresh_token":"rt","expired":"2030-01-01T00:00:00Z"}`,
		`{"type":"antigravity","access_token":"at","refresh_token":"rt","expired":"bad"}`,
		`{"type":"antigravity","access_token":"at","refresh_token":"rt","expired":"2030-01-01T00:00:00Z"} {}`,
	} {
		if _, err := ParseCredential([]byte(raw)); err == nil {
			t.Fatalf("ParseCredential(%q) succeeded", raw)
		}
	}
}

func fmtInt(value int64) string {
	return fmt.Sprintf("%d", value)
}
