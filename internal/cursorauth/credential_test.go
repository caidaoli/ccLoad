package cursorauth

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestParseCredentialNormalizesSessionTokens(t *testing.T) {
	t.Parallel()
	credential, err := ParseCredential([]byte(`{"access_token":" tok ","refresh_token":" ref ","email":" User@Example.com ","user_id":"auth-1"}`))
	if err != nil {
		t.Fatalf("ParseCredential() error = %v", err)
	}
	if credential.Type != ChannelType || credential.AccessToken != "tok" || credential.RefreshToken != "ref" {
		t.Fatalf("credential = %+v", credential)
	}
	if credential.Identity().Email != "User@Example.com" || credential.Identity().UserID != "auth-1" {
		t.Fatalf("identity = %+v", credential.Identity())
	}
}

func TestParseCredentialRejectsInvalidPayloads(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"empty":             ``,
		"missing token":     `{"email":"user@example.com"}`,
		"blank token":       `{"access_token":"   "}`,
		"whitespace token":  `{"access_token":"tok en"}`,
		"wrong type":        `{"type":"zai","access_token":"tok"}`,
		"trailing json":     `{"access_token":"tok"} {}`,
		"bad refresh stamp": `{"access_token":"tok","last_refresh":"not-a-time"}`,
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

func TestMergeRefreshKeepsIdentityAndRotatesToken(t *testing.T) {
	t.Parallel()
	current := &Credential{
		Type: ChannelType, AccessToken: "old", RefreshToken: "refresh", APIKey: "key",
		UserID: "u-1", Email: "user@example.com", Name: "User",
		OAuthUsage: json.RawMessage(`{"kept":true}`),
	}
	merged, err := current.MergeRefresh(&Credential{AccessToken: "new"})
	if err != nil {
		t.Fatalf("MergeRefresh() error = %v", err)
	}
	if merged.AccessToken != "new" || merged.RefreshToken != "refresh" || merged.APIKey != "key" ||
		merged.UserID != "u-1" || merged.Email != "user@example.com" ||
		string(merged.OAuthUsage) != `{"kept":true}` {
		t.Fatalf("merged = %+v", merged)
	}
	merged.OAuthUsage[0] = 'X'
	if string(current.OAuthUsage) != `{"kept":true}` {
		t.Fatalf("current usage mutated: %s", current.OAuthUsage)
	}
}

/**
 * @brief 验证模型窗口与思考档位仅在同一 API Key 未提供新快照时沿用。
 * @param t 测试上下文。
 */
func TestMergeRefreshModelCatalog(t *testing.T) {
	t.Parallel()
	prior := map[string]int{"model@300k": 300000}
	latest := map[string]int{"model@1m": 1000000}
	priorLevels := map[string][]string{"model@300k": {"low"}}
	latestLevels := map[string][]string{"model@1m": {"high"}}
	priorDefault := map[string]string{"model@300k": "low"}
	latestDefault := map[string]string{"model@1m": "high"}
	current := &Credential{AccessToken: "old", APIKey: "key", ContextWindows: prior, EffortLevels: priorLevels, DefaultEfforts: priorDefault}
	for _, tc := range []struct {
		name                   string
		apiKey                 string
		windows                map[string]int
		levels                 map[string][]string
		want                   map[string]int
		wantLevels             map[string][]string
		defaults, wantDefaults map[string]string
	}{
		{name: "preserve with inherited key", want: prior, wantLevels: priorLevels, wantDefaults: priorDefault},
		{name: "preserve with explicit key", apiKey: " key ", want: prior, wantLevels: priorLevels, wantDefaults: priorDefault},
		{name: "discard with replaced key", apiKey: "different"},
		{name: "replace with new account snapshot", apiKey: "different", windows: latest, levels: latestLevels, want: latest, wantLevels: latestLevels, defaults: latestDefault, wantDefaults: latestDefault},
		{name: "discard with explicit empty snapshot", windows: map[string]int{}, want: map[string]int{}},
		{name: "replace with capability-only snapshot", levels: latestLevels, wantLevels: latestLevels},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refreshed := &Credential{AccessToken: "new", APIKey: tc.apiKey, ContextWindows: tc.windows, EffortLevels: tc.levels, DefaultEfforts: tc.defaults}
			merged, err := current.MergeRefresh(refreshed)
			if err != nil {
				t.Fatalf("MergeRefresh() error = %v", err)
			}
			if (merged.ContextWindows == nil) != (tc.want == nil) || !maps.Equal(merged.ContextWindows, tc.want) {
				t.Fatalf("merged windows = %#v, want %#v", merged.ContextWindows, tc.want)
			}
			if merged.ContextWindows != nil {
				merged.ContextWindows["other"] = 1
			}
			if current.ContextWindows["other"] != 0 || refreshed.ContextWindows["other"] != 0 {
				t.Fatal("merged catalog shares storage with an input credential")
			}
			if !maps.Equal(merged.DefaultEfforts, tc.wantDefaults) {
				t.Fatalf("merged default efforts = %v, want %v", merged.DefaultEfforts, tc.wantDefaults)
			}
			if merged.DefaultEfforts != nil {
				merged.DefaultEfforts["other"] = "max"
				if current.DefaultEfforts["other"] != "" || refreshed.DefaultEfforts["other"] != "" {
					t.Fatal("merged defaults share storage with an input credential")
				}
			}
			if !maps.EqualFunc(merged.EffortLevels, tc.wantLevels, slices.Equal) {
				t.Fatalf("merged effort levels = %#v, want %#v", merged.EffortLevels, tc.wantLevels)
			}
			for id, levels := range merged.EffortLevels {
				levels[0] = "max"
				if tc.wantLevels[id][0] == "max" {
					t.Fatal("merged effort levels share storage with an input credential")
				}
			}
		})
	}
}

func TestCredentialJSONRoundTrip(t *testing.T) {
	t.Parallel()
	credential := &Credential{
		AccessToken: "tok", Email: "user@example.com", LastRefresh: "2026-01-02T03:04:05+08:00",
		ContextWindows: map[string]int{"model@300k": 300000},
		DefaultEfforts: map[string]string{"model@300k": "high"},
		EffortLevels:   map[string][]string{"model@300k": {"none", "high"}, "opaque": {}},
	}
	payload, err := credential.JSON()
	if err != nil {
		t.Fatalf("JSON() error = %v", err)
	}
	parsed, err := ParseCredential([]byte(payload))
	if err != nil {
		t.Fatalf("ParseCredential() error = %v", err)
	}
	if parsed.LastRefresh != "2026-01-01T19:04:05Z" || parsed.AccessToken != "tok" ||
		!maps.Equal(parsed.ContextWindows, credential.ContextWindows) || !maps.Equal(parsed.DefaultEfforts, credential.DefaultEfforts) ||
		!maps.EqualFunc(parsed.EffortLevels, credential.EffortLevels, slices.Equal) || parsed.EffortLevels["opaque"] == nil {
		t.Fatalf("parsed = %+v", parsed)
	}
}

/**
 * @brief 验证超过凭证读取限制的模型窗口快照不能持久化。
 * @param t 测试上下文。
 */
func TestCredentialJSONRejectsOversizeCatalog(t *testing.T) {
	t.Parallel()
	credential := &Credential{AccessToken: "tok", ContextWindows: map[string]int{strings.Repeat("m", maxCredentialSize): 300000}}
	if _, err := credential.JSON(); err == nil {
		t.Fatal("JSON() accepted a catalog exceeding the credential size limit")
	}
}

func TestIdentityMatchesAccount(t *testing.T) {
	t.Parallel()
	left := Identity{UserID: "auth-1", Email: "a@example.com"}
	if !left.Matches(Identity{UserID: "auth-1", Email: "other@example.com"}) {
		t.Fatal("user id must win")
	}
	if !(Identity{Email: "User@Example.com"}).Matches(Identity{Email: "user@example.com"}) {
		t.Fatal("email match must be case-insensitive")
	}
	if (Identity{}).Matches(Identity{Email: "user@example.com"}) {
		t.Fatal("zero identity must not match")
	}
}
