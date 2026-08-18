package zaiauth

import (
	"context"
	"io"
	"net/http"
	"testing"
)

func TestFetchQuotaLimitsReadsMonitorEnvelope(t *testing.T) {
	t.Parallel()
	payload := `{"code":200,"msg":"","success":true,"data":{"limits":[` +
		`{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":42.5,"nextResetTime":1787040000000},` +
		`{"type":"TOKENS_LIMIT","unit":6,"number":1,"percentage":11,"nextResetTime":1787500000000},` +
		`{"type":"MCP_LIMIT","percentage":3,"nextResetTime":0}]}}`
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key.secret" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-ZCode-App-Version") != AppVersion {
			t.Errorf("identity headers missing: %v", r.Header)
		}
		_, _ = io.WriteString(w, payload)
	}))
	limits, err := service.FetchQuotaLimits(context.Background(), "key.secret")
	if err != nil {
		t.Fatalf("FetchQuotaLimits() error = %v", err)
	}
	if len(limits) != 3 {
		t.Fatalf("limits = %+v", limits)
	}
	if limits[0].Name() != "five_hour" || limits[0].Kind() != QuotaLimitTokens ||
		limits[0].WindowSeconds() != 5*3600 || limits[0].UsedPercent != 42.5 {
		t.Fatalf("five hour window = %+v", limits[0])
	}
	if limits[1].Name() != "weekly" || limits[1].WindowSeconds() != 7*86400 {
		t.Fatalf("weekly window = %+v", limits[1])
	}
	// Non-token windows keep their upstream name and report no duration.
	if limits[2].Name() != "mcp_limit" || limits[2].Kind() != QuotaLimitOther ||
		limits[2].WindowSeconds() != 0 {
		t.Fatalf("mcp window = %+v", limits[2])
	}
}

// The monitor endpoint answers HTTP 200 even for a rejected key, so the
// envelope has to decide success.
func TestFetchQuotaLimitsRejectsBusinessFailure(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"code":1000,"msg":"Authentication Failed","success":false}`)
	}))
	_, err := service.FetchQuotaLimits(context.Background(), "key.secret")
	if err == nil {
		t.Fatal("FetchQuotaLimits() expected an error")
	}
}

func TestParseQuotaLimitsRequiresWindows(t *testing.T) {
	t.Parallel()
	if _, err := parseQuotaLimits([]byte(`{"success":true,"data":{"limits":[]}}`)); err == nil {
		t.Fatal("an empty limits array must be an error")
	}
	if _, err := parseQuotaLimits([]byte(`not json`)); err == nil {
		t.Fatal("invalid JSON must be an error")
	}
	// A bare success code with no explicit success flag still parses.
	limits, err := parseQuotaLimits([]byte(`{"code":0,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":1}]}}`))
	if err != nil || len(limits) != 1 {
		t.Fatalf("limits = %+v err = %v", limits, err)
	}
}
