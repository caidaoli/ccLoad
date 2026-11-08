package zaiauth

import (
	"encoding/json"
	"strings"
	"testing"
)

/**
 * @brief 站点随凭证持久化，未知站点与跨站刷新必须拒绝。
 * @param t 测试上下文。
 */
func TestCredentialSiteIsolation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ input, want Site }{{"", SiteZAI}, {" BIGMODEL ", SiteBigModel}, {SiteZAI, SiteZAI}} {
		credential := &Credential{Site: tc.input, APIKey: "key"}
		raw, err := credential.JSON()
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseCredential([]byte(raw))
		if err != nil || parsed.Site != tc.want {
			t.Fatalf("round trip: %+v %v", parsed, err)
		}
	}
	if _, err := ParseCredential([]byte(`{"site":"unknown","api_key":"key"}`)); err == nil {
		t.Fatal("accepted invalid site")
	}
	preference := false
	domestic := &Credential{Site: SiteBigModel, APIKey: "old", FreeFirst: &preference}
	merged, err := domestic.MergeRefresh(&Credential{Site: SiteBigModel, APIKey: "new"})
	if err != nil || merged.Site != SiteBigModel || merged.APIKey != "new" || merged.FreeFirst == nil || *merged.FreeFirst {
		t.Fatalf("same-site merge: %+v %v", merged, err)
	}
	if _, err := domestic.MergeRefresh(&Credential{Site: SiteZAI, APIKey: "other"}); err == nil {
		t.Fatal("merged different sites")
	}
}

func TestParseCredentialNormalizesAndDerivesDeviceID(t *testing.T) {
	t.Parallel()
	credential, err := ParseCredential([]byte(`{"api_key":" key-id.secret ","email":" User@Example.com ","user_id":"u-1"}`))
	if err != nil {
		t.Fatalf("ParseCredential() error = %v", err)
	}
	if credential.Type != ChannelType || credential.APIKey != "key-id.secret" {
		t.Fatalf("credential = %+v", credential)
	}
	if credential.Identity().Email != "User@Example.com" || credential.Identity().UserID != "u-1" {
		t.Fatalf("identity = %+v", credential.Identity())
	}
	if len(credential.DeviceID) != 36 || strings.Count(credential.DeviceID, "-") != 4 {
		t.Fatalf("device id = %q", credential.DeviceID)
	}
	// The fingerprint must be stable for the same account across restarts.
	if again := DeriveDeviceID(credential.Identity(), credential.APIKey); again != credential.DeviceID {
		t.Fatalf("device id changed: %q != %q", again, credential.DeviceID)
	}
}

func TestParseCredentialRejectsInvalidPayloads(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"empty":          ``,
		"missing key":    `{"email":"user@example.com"}`,
		"blank key":      `{"api_key":"   "}`,
		"whitespace key": `{"api_key":"key id.secret"}`,
		"wrong type":     `{"type":"codex","api_key":"key.secret"}`,
		"trailing json":  `{"api_key":"key.secret"} {}`,
		"bad refresh":    `{"api_key":"key.secret","last_refresh":"not-a-time"}`,
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseCredential([]byte(payload)); err == nil {
				t.Fatalf("ParseCredential(%s) expected error", payload)
			}
		})
	}
}

func TestMergeRefreshKeepsIdentityAndRotatesKey(t *testing.T) {
	t.Parallel()
	current := &Credential{
		Type: ChannelType, APIKey: "old.secret", AccessToken: "access", JWTToken: "jwt",
		UserID: "u-1", Email: "user@example.com", Name: "User", DeviceID: "device-1",
		OAuthUsage: json.RawMessage(`{"kept":true}`),
	}
	merged, err := current.MergeRefresh(&Credential{APIKey: "new.secret"})
	if err != nil {
		t.Fatalf("MergeRefresh() error = %v", err)
	}
	if merged.APIKey != "new.secret" || merged.AccessToken != "access" || merged.JWTToken != "jwt" ||
		merged.UserID != "u-1" || merged.Email != "user@example.com" || merged.DeviceID != "device-1" ||
		string(merged.OAuthUsage) != `{"kept":true}` {
		t.Fatalf("merged = %+v", merged)
	}
	// The merge must not alias the previous credential's usage buffer.
	merged.OAuthUsage[0] = 'X'
	if string(current.OAuthUsage) != `{"kept":true}` {
		t.Fatalf("current usage mutated: %s", current.OAuthUsage)
	}
}

func TestCredentialJSONRoundTrip(t *testing.T) {
	t.Parallel()
	credential := &Credential{APIKey: "key.secret", Email: "user@example.com", LastRefresh: "2026-01-02T03:04:05+08:00"}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatalf("JSON() error = %v", err)
	}
	parsed, err := ParseCredential([]byte(payload))
	if err != nil {
		t.Fatalf("ParseCredential() error = %v", err)
	}
	if parsed.APIKey != "key.secret" || parsed.LastRefresh != "2026-01-01T19:04:05Z" {
		t.Fatalf("parsed = %+v", parsed)
	}
	if strings.Contains(payload, "\n") {
		t.Fatalf("payload = %q", payload)
	}
}

func TestIdentityMatches(t *testing.T) {
	t.Parallel()
	if !(Identity{UserID: "u-1"}).Matches(Identity{UserID: "u-1", Email: "other@example.com"}) {
		t.Fatal("user id must win when both sides have one")
	}
	if (Identity{UserID: "u-1"}).Matches(Identity{UserID: "u-2"}) {
		t.Fatal("different user ids must not match")
	}
	if !(Identity{Email: "User@Example.com"}).Matches(Identity{Email: "user@example.com"}) {
		t.Fatal("email comparison must be case-insensitive")
	}
	if (Identity{}).Matches(Identity{}) || !(Identity{}).IsZero() {
		t.Fatal("empty identities must never match")
	}
}

/**
 * @brief 体验账号无需付费 Key，渠道偏好经过刷新与序列化仍保留。
 * @param t 测试上下文。
 */
func TestTrialCredentialAndPreferenceSurviveRefresh(t *testing.T) {
	t.Parallel()
	current, err := ParseCredential([]byte(`{"jwt_token":"trial-session","user_id":"trial-user","free_first":false}`))
	if err != nil {
		t.Fatal(err)
	}
	merged, err := current.MergeRefresh(&Credential{JWTToken: "new-session"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := merged.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		t.Fatal(err)
	}
	if saved["free_first"] != false || saved["jwt_token"] != "new-session" || saved["device_id"] == "" {
		t.Fatalf("trial credential lost its preference or identity: %v", saved)
	}
}
