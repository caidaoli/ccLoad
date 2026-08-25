package app

import (
	"strings"
	"testing"

	"ccLoad/internal/model"
)

// API Key 渠道行携带任何凭据封套都必须被拒绝，绝不落库。
func TestCSVImportRejectsAPIKeyRowCarryingCredential(t *testing.T) {
	const secret = "must-not-persist"
	columns := map[string]int{
		"name": 0, "api_key": 1, "urls": 2, "models": 3, "auth_type": 4,
		"oauth_credential": 5,
	}
	for _, tc := range []struct {
		name       string
		credential string
	}{
		{"management envelope", `{"profile":"sub2api","base_url":"https://panel.example.com","access_token":"` + secret + `"}`},
		{"oauth credential", `{"refresh_token":"` + secret + `"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channel, errMessage, skipped := (&Server{}).parseChannelImportRow(
				[]string{
					"managed input", "sk-imported", `[{"url":"https://api.example.com"}]`, "gpt-5",
					model.AuthTypeAPIKey, tc.credential,
				}, columns, 2, false, false, false, false, false, false, false,
				nil, nil, nil, nil, nil, nil,
			)
			if !skipped || channel != nil {
				t.Fatalf("API Key row carrying a credential was accepted: channel=%#v skipped=%v", channel, skipped)
			}
			if errMessage != "第2行 API Key 渠道不能包含 OAuth 凭证" {
				t.Fatalf("error message = %q", errMessage)
			}
			if strings.Contains(errMessage, secret) {
				t.Fatalf("import error leaked the credential: %q", errMessage)
			}
		})
	}
}
