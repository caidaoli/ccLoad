package app

import (
	"strings"
	"testing"

	"ccLoad/internal/model"
)

func TestCSVImportAPIKeyManagementEnvelopeValidation(t *testing.T) {
	const secret = "must-not-persist"
	columns := map[string]int{
		"name": 0, "api_key": 1, "urls": 2, "models": 3, "auth_type": 4,
		"oauth_credential": 5,
	}
	for _, tc := range []struct {
		name        string
		credential  string
		wantSkipped bool
	}{
		{"management envelope", `{"kind":"channel_management","version":1,"profile":"sub2api","settings":{"base_url":"https://panel.example.com","access_token":"` + secret + `"},"state":{}}`, false},
		{"oauth credential", `{"refresh_token":"` + secret + `"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel, errMessage, skipped := (&Server{}).parseChannelImportRow(
				[]string{
					"managed input", "sk-imported", `[{"url":"https://api.example.com"}]`, "gpt-5",
					model.AuthTypeAPIKey, tc.credential,
				}, columns, 2, false, false, false, false, false, false, false, false,
				nil, nil, nil, nil, nil, nil,
			)
			if skipped != tc.wantSkipped {
				t.Fatalf("skipped=%v, want %v (channel=%#v error=%q)", skipped, tc.wantSkipped, channel, errMessage)
			}
			if tc.wantSkipped {
				if channel != nil || !strings.Contains(errMessage, "管理账号无效") {
					t.Fatalf("unexpected rejected row: channel=%#v error=%q", channel, errMessage)
				}
				if strings.Contains(errMessage, secret) {
					t.Fatalf("import error leaked the credential: %q", errMessage)
				}
				return
			}
			if channel == nil || errMessage != "" || channel.Config.OAuthCredential == "" {
				t.Fatalf("management envelope was not accepted: channel=%#v error=%q", channel, errMessage)
			}
		})
	}
}

func TestCSVImportBlankAPIKeyCostMultiplierDefaultsToOne(t *testing.T) {
	columns := map[string]int{
		"name": 0, "api_key": 1, "urls": 2, "models": 3, "auth_type": 4,
		"api_key_cost_multipliers": 5,
	}

	channel, errMessage, skipped := (&Server{}).parseChannelImportRow(
		[]string{
			"blank multiplier", "sk-imported", `[{"url":"https://api.example.com"}]`, "gpt-5",
			model.AuthTypeAPIKey, "",
		}, columns, 2, false, false, false, false, false, false, true, false,
		nil, nil, nil, nil, nil, nil,
	)
	if skipped || errMessage != "" {
		t.Fatalf("blank multiplier row rejected: skipped=%v error=%q", skipped, errMessage)
	}
	if channel == nil || len(channel.APIKeys) != 1 || channel.APIKeys[0].CostMultiplier != 1 {
		t.Fatalf("imported keys=%#v, want one key with multiplier 1", channel)
	}
}
