package app

import (
	"testing"

	"ccLoad/internal/model"
)

func TestCSVImportIgnoresAPIKeyManagementEnvelope(t *testing.T) {
	columns := map[string]int{
		"name": 0, "api_key": 1, "urls": 2, "models": 3, "auth_type": 4,
		"management_account": 5,
	}
	channel, errMessage, skipped := (&Server{}).parseChannelImportRow(
		[]string{
			"managed input", "sk-imported", `[{"url":"https://api.example.com"}]`, "gpt-5", model.AuthTypeAPIKey,
			`{"profile":"sub2api","base_url":"https://panel.example.com","access_token":"must-not-persist"}`,
		}, columns, 2, false, false, false, false, false, false, false,
		nil, nil, nil, nil, nil, nil,
	)
	if skipped || errMessage != "" || channel == nil || channel.Config == nil {
		t.Fatalf("parse CSV row = channel=%#v error=%q skipped=%v", channel, errMessage, skipped)
	}
	if channel.Config.OAuthCredential != "" {
		t.Fatalf("API Key CSV management envelope was persisted: %q", channel.Config.OAuthCredential)
	}
}
