package app

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"slices"
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
				}, columns, 2, false, false, false, false, false, false, false, false, false, false, false,
				nil, nil, nil, nil, nil, nil, nil, nil, nil,
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
		}, columns, 2, false, false, false, false, false, false, false, true, false, false, false,
		nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)
	if skipped || errMessage != "" {
		t.Fatalf("blank multiplier row rejected: skipped=%v error=%q", skipped, errMessage)
	}
	if channel == nil || len(channel.APIKeys) != 1 || channel.APIKeys[0].CostMultiplier != 1 {
		t.Fatalf("imported keys=%#v, want one key with multiplier 1", channel)
	}
}

func TestCSVImportDailyScheduleValidation(t *testing.T) {
	columns := map[string]int{"name": 0, "api_key": 1, "urls": 2, "models": 3, "scheduled_check_interval_minutes": 4, "scheduled_check_start_time": 5, "scheduled_check_enabled": 6}
	for _, tc := range []struct {
		minutes, start, enabled string
		valid                   bool
	}{
		{"30", "08:30", "true", true}, {"1440", "23:59", "false", true},
		{"0", "08:30", "true", false}, {"1.5", "08:30", "true", false},
		{"1441", "08:30", "true", false}, {"30", "8:30", "true", false},
		{"30", "24:00", "true", false}, {"30", "08:30", "invalid", false},
	} {
		channel, message, skipped := (&Server{}).parseChannelImportRow(
			[]string{"schedule", "sk-test", `[{"url":"https://example.com"}]`, "gpt-5", tc.minutes, tc.start, tc.enabled},
			columns, 2, true, false, false, false, false, false, false, false, false, false, false,
			nil, nil, nil, nil, nil, nil, nil, nil, nil,
		)
		if skipped == tc.valid {
			t.Fatalf("row %v: skipped=%v, error=%q", tc, skipped, message)
		}
		if tc.valid && (channel.Config.ScheduledCheckStartTime != tc.start || channel.Config.ScheduledCheckEnabled != (tc.enabled == "true")) {
			t.Fatalf("row lost configuration: %+v", channel.Config)
		}
		if !tc.valid && !strings.Contains(message, "第2行") {
			t.Fatalf("error lacks row: %q", message)
		}
	}
}

func TestAdminAPI_CSVImportModelScopeSources(t *testing.T) {
	for _, tc := range []struct {
		name, key, allowed, detected, empty, auto string
		wantAuto, wantSkipped, priorManual        bool
	}{
		{name: "omitted scopes preserve source", key: "sk-existing", wantAuto: true},
		{name: "unchanged scope preserves source", key: "sk-existing", allowed: `[["gpt-5"]]`, detected: `[["upstream-gpt"]]`, empty: `[false]`, wantAuto: true},
		{name: "manual range clears source", key: "sk-existing", allowed: `[["claude-sonnet"]]`},
		{name: "manual empty range clears source", key: "sk-existing", allowed: `[[]]`, empty: `[true]`},
		{name: "manual detection clears source", key: "sk-existing", detected: `[["new-upstream"]]`},
		{name: "explicit auto applies", key: "sk-existing", allowed: `[["claude-sonnet"]]`, auto: `[true]`, wantAuto: true},
		{name: "explicit manual applies", key: "sk-existing", auto: `[false]`},
		{name: "new key defaults manual", key: "sk-new"},
		{name: "legacy manual detection stays manual", key: "sk-existing", priorManual: true},
		{name: "invalid boolean rejected", key: "sk-existing", auto: `["true"]`, wantSkipped: true},
		{name: "wrong count rejected", key: "sk-existing", auto: `[]`, wantSkipped: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newInMemoryServer(t)
			ctx := context.Background()
			cfg, err := server.store.CreateConfig(ctx, &model.Config{
				Name: "scope", URLs: model.ChannelURLs{{URL: "https://example.com"}},
				ModelEntries: []model.ModelEntry{{Model: "gpt-5"}, {Model: "claude-sonnet"}}, Enabled: true,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := server.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
				ChannelID: cfg.ID, APIKey: "sk-existing", AllowedModels: []string{"gpt-5"},
				DetectedModels: []string{"upstream-gpt"}, ModelScopeAuto: !tc.priorManual, CostMultiplier: 1,
			}}); err != nil {
				t.Fatal(err)
			}
			header := []string{"name", "api_key", "urls", "models"}
			row := []string{"scope", tc.key, `[{"url":"https://example.com"}]`, "gpt-5,claude-sonnet"}
			for _, field := range []struct{ column, value string }{
				{"api_key_allowed_models", tc.allowed}, {"api_key_detected_models", tc.detected},
				{"api_key_model_scope_empty", tc.empty}, {"api_key_model_scope_auto", tc.auto},
			} {
				if field.value != "" {
					header = append(header, field.column)
					row = append(row, field.value)
				}
			}
			var csvBody bytes.Buffer
			csvWriter := csv.NewWriter(&csvBody)
			if err := csvWriter.WriteAll([][]string{header, row}); err != nil {
				t.Fatal(err)
			}
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, err := writer.CreateFormFile("file", "scopes.csv")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := part.Write(csvBody.Bytes()); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			request := newRequest(http.MethodPost, "/admin/channels/import", &body)
			request.Header.Set("Content-Type", writer.FormDataContentType())
			importC, importW := newTestContext(t, request)
			server.HandleImportChannelsCSV(importC)
			if importW.Code != http.StatusOK {
				t.Fatalf("import: status=%d body=%s", importW.Code, importW.Body.String())
			}
			var response struct {
				Data ChannelImportSummary `json:"data"`
			}
			if err := json.Unmarshal(importW.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if (response.Data.Skipped == 1) != tc.wantSkipped {
				t.Fatalf("unexpected summary: %+v", response.Data)
			}
			if tc.wantSkipped {
				if len(response.Data.Errors) != 1 || !strings.Contains(response.Data.Errors[0], "api_key_model_scope_auto") {
					t.Fatalf("unexpected rejection: %+v", response.Data)
				}
				return
			}
			if response.Data.Updated != 1 || response.Data.Created != 0 || len(response.Data.Errors) != 0 {
				t.Fatalf("unexpected successful summary: %+v", response.Data)
			}
			keys, err := server.store.GetAPIKeys(ctx, cfg.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(keys) != 1 || keys[0].ModelScopeAuto != tc.wantAuto {
				t.Fatalf("imported scope source, want %v: %+v", tc.wantAuto, keys)
			}
		})
	}
}

func TestAdminAPI_CSVImportMissingModelScopeSourcePreservesExisting(t *testing.T) {
	server := newInMemoryServer(t)
	ctx := context.Background()
	cfg, err := server.store.CreateConfig(ctx, &model.Config{
		Name: "scope source", URLs: model.ChannelURLs{{URL: "https://example.com"}},
		ModelEntries: []model.ModelEntry{{Model: "gpt-5"}}, Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.store.CreateAPIKeysBatch(ctx, []*model.APIKey{{
		ChannelID: cfg.ID, APIKey: "sk-auto", AllowedModels: []string{"gpt-5"},
		DetectedModels: []string{"upstream-gpt"}, ModelScopeAuto: true, CostMultiplier: 1,
	}}); err != nil {
		t.Fatal(err)
	}
	exportC, exportW := newTestContext(t, newRequest(http.MethodGet, "/admin/channels/export", nil))
	server.HandleExportChannelsCSV(exportC)
	if exportW.Code != http.StatusOK {
		t.Fatalf("export: status=%d body=%s", exportW.Code, exportW.Body.String())
	}
	records, err := csv.NewReader(bytes.NewReader(exportW.Body.Bytes())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	index := slices.Index(records[0], "api_key_model_scope_auto")
	if index < 0 {
		t.Fatal("export missing model scope source")
	}
	var legacyCSV bytes.Buffer
	csvWriter := csv.NewWriter(&legacyCSV)
	for _, record := range records {
		if err := csvWriter.Write(slices.Delete(record, index, index+1)); err != nil {
			t.Fatal(err)
		}
	}
	csvWriter.Flush()
	if err := csvWriter.Error(); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "legacy-scopes.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(legacyCSV.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := newRequest(http.MethodPost, "/admin/channels/import", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	importC, importW := newTestContext(t, request)
	server.HandleImportChannelsCSV(importC)
	if importW.Code != http.StatusOK {
		t.Fatalf("import: status=%d body=%s", importW.Code, importW.Body.String())
	}
	var response struct {
		Data ChannelImportSummary `json:"data"`
	}
	if err := json.Unmarshal(importW.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Data.Updated != 1 || response.Data.Skipped != 0 || len(response.Data.Errors) != 0 {
		t.Fatalf("unexpected successful summary: %+v", response.Data)
	}
	keys, err := server.store.GetAPIKeys(ctx, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || !keys[0].ModelScopeAuto {
		t.Fatalf("legacy import lost automatic scope source: %+v", keys)
	}
}
