package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"ccLoad/internal/model"
	"ccLoad/internal/protocol"
	"ccLoad/internal/zaiauth"
)

func newZAITestChannel() *model.Config {
	return &model.Config{
		ID: 7, Name: "Z.ai-test", AuthType: model.AuthTypeZAIOAuth,
		OAuthCredential: `{"type":"zai","api_key":"key-id.secret","user_id":"u-1","email":"user@example.com"}`,
		URLs:            model.ChannelURLs{{URL: zaiauth.CodingPlanProxyBaseURL, Protocols: []string{"anthropic"}}},
		ZAIDeviceID:     "11111111-2222-4333-8444-555555555555",
	}
}

func TestZAICodingPlanRequestDetection(t *testing.T) {
	t.Parallel()
	cfg := newZAITestChannel()
	if !isZAICodingPlanRequest(cfg, protocol.Anthropic, "/v1/messages") {
		t.Fatal("Anthropic messages requests on a Z.ai channel must use the Coding Plan contract")
	}
	if isZAICodingPlanRequest(cfg, protocol.Codex, "/v1/responses") {
		t.Fatal("non-Anthropic upstreams must not use the Coding Plan contract")
	}
	if isZAICodingPlanRequest(&model.Config{AuthType: model.AuthTypeAPIKey}, protocol.Anthropic, "/v1/messages") {
		t.Fatal("API key channels must not use the Coding Plan contract")
	}
}

func TestFinalizeZAICodingPlanBodyStampsZCodeFingerprint(t *testing.T) {
	t.Parallel()
	cfg := newZAITestChannel()
	body := []byte(`{"model":"glm-4.7","messages":[{"role":"user","content":"hello"}]}`)
	finalized, err := finalizeZAICodingPlanBody(body, cfg)
	if err != nil {
		t.Fatalf("finalizeZAICodingPlanBody() error = %v", err)
	}
	identity := decodeZAIRequestIdentity(t, finalized)
	if identity.DeviceID != cfg.ZAIDeviceID {
		t.Fatalf("device id = %q", identity.DeviceID)
	}
	if identity.AccountUUID != "" {
		t.Fatalf("account uuid = %q, ZCode always sends an empty value", identity.AccountUUID)
	}
	if identity.SessionID == "" {
		t.Fatal("session id must be present")
	}
	// The same conversation must keep one session identifier.
	repeat, err := finalizeZAICodingPlanBody(body, cfg)
	if err != nil {
		t.Fatalf("finalizeZAICodingPlanBody() error = %v", err)
	}
	if decodeZAIRequestIdentity(t, repeat).SessionID != identity.SessionID {
		t.Fatal("session id must be stable for the same conversation")
	}
}

// A client fingerprint (Claude Code's, for instance) must never reach z.ai.
func TestFinalizeZAICodingPlanBodyReplacesForeignFingerprint(t *testing.T) {
	t.Parallel()
	cfg := newZAITestChannel()
	body := []byte(`{"model":"glm-4.7","metadata":{"user_id":"{\"device_id\":\"other-device\",\"session_id\":\"3f2504e0-4f89-41d3-9a0c-0305e82c3301\"}","keep":"me"},"messages":[]}`)
	finalized, err := finalizeZAICodingPlanBody(body, cfg)
	if err != nil {
		t.Fatalf("finalizeZAICodingPlanBody() error = %v", err)
	}
	identity := decodeZAIRequestIdentity(t, finalized)
	if identity.DeviceID != cfg.ZAIDeviceID {
		t.Fatalf("device id = %q", identity.DeviceID)
	}
	// The caller's own session identifier is preserved: only the device changes.
	if identity.SessionID != "3f2504e0-4f89-41d3-9a0c-0305e82c3301" {
		t.Fatalf("session id = %q", identity.SessionID)
	}
	var request map[string]any
	if err := json.Unmarshal(finalized, &request); err != nil {
		t.Fatal(err)
	}
	metadata, _ := request["metadata"].(map[string]any)
	if metadata["keep"] != "me" {
		t.Fatalf("unrelated metadata was dropped: %v", metadata)
	}
}

func TestFinalizeZAICodingPlanBodyRejectsInvalidJSON(t *testing.T) {
	t.Parallel()
	if _, err := finalizeZAICodingPlanBody([]byte("not json"), newZAITestChannel()); err == nil {
		t.Fatal("finalizeZAICodingPlanBody() expected an error")
	}
}

func TestInjectZAICodingPlanHeadersReplicatesZCodeIdentity(t *testing.T) {
	t.Parallel()
	cfg := newZAITestChannel()
	request, err := http.NewRequest(http.MethodPost, zaiauth.CodingPlanProxyBaseURL+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Headers a downstream client may have sent, including a foreign credential.
	request.Header.Set("Authorization", "Bearer downstream-token")
	request.Header.Set("X-Api-Key", "downstream-key")
	request.Header.Set("X-Stainless-Lang", "js")
	incoming := http.Header{"Accept": []string{"text/event-stream"}}

	injectZAICodingPlanHeaders(request, cfg, "key-id.secret", []byte(`{"messages":[]}`), incoming)

	if anthropicHeaderValue(request.Header, "Authorization") != "" {
		t.Fatal("ZCode authenticates with x-api-key only")
	}
	if got := anthropicHeaderValue(request.Header, "x-api-key"); got != "key-id.secret" {
		t.Fatalf("x-api-key = %q", got)
	}
	if anthropicHeaderValue(request.Header, "X-Stainless-Lang") != "" {
		t.Fatal("foreign client headers must not survive the rebuild")
	}
	if got := anthropicHeaderValue(request.Header, "Accept"); got != "text/event-stream" {
		t.Fatalf("Accept = %q, streaming clients must keep their Accept", got)
	}
	if got := anthropicHeaderValue(request.Header, "anthropic-version"); got != "2023-06-01" {
		t.Fatalf("anthropic-version = %q", got)
	}
	for _, entry := range zaiauth.SourceHeaders() {
		if got := anthropicHeaderValue(request.Header, entry[0]); got != entry[1] {
			t.Fatalf("%s = %q, want %q", entry[0], got, entry[1])
		}
	}
	if anthropicHeaderValue(request.Header, "x-request-id") == "" ||
		anthropicHeaderValue(request.Header, "x-zcode-trace-id") == "" ||
		anthropicHeaderValue(request.Header, "x-session-id") == "" {
		t.Fatalf("attribution headers missing: %v", request.Header)
	}
	// Header names must reach the wire in ZCode's own casing.
	if !hasRawHeaderName(request.Header, "X-ZCode-App-Version") {
		t.Fatalf("header casing lost: %v", request.Header)
	}
}

func hasRawHeaderName(headers http.Header, name string) bool {
	for existing := range headers {
		if existing == name {
			return true
		}
	}
	return false
}

func TestInjectZAICodingPlanHeadersKeepsClientAnthropicVersion(t *testing.T) {
	t.Parallel()
	request, err := http.NewRequest(http.MethodPost, zaiauth.CodingPlanProxyBaseURL+"/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	incoming := http.Header{"Anthropic-Version": []string{"2024-01-01"}}
	injectZAICodingPlanHeaders(request, newZAITestChannel(), "key-id.secret", []byte(`{}`), incoming)
	if got := anthropicHeaderValue(request.Header, "anthropic-version"); got != "2024-01-01" {
		t.Fatalf("anthropic-version = %q", got)
	}
}

// The device fingerprint falls back to the persisted credential when the
// runtime config was not enriched (admin test paths, for example).
func TestZAIDeviceIDFallsBackToCredential(t *testing.T) {
	t.Parallel()
	cfg := newZAITestChannel()
	cfg.ZAIDeviceID = ""
	credential, err := zaiauth.ParseCredential([]byte(cfg.OAuthCredential))
	if err != nil {
		t.Fatal(err)
	}
	if got := zaiDeviceID(cfg); got != credential.DeviceID || got == "" {
		t.Fatalf("device id = %q, want %q", got, credential.DeviceID)
	}
}

func TestZAICredentialRejectedOnlyOnUnauthorized(t *testing.T) {
	t.Parallel()
	if !zaiCredentialRejected(http.StatusUnauthorized) {
		t.Fatal("401 must re-resolve the Coding Plan key")
	}
	for _, status := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		if zaiCredentialRejected(status) {
			t.Fatalf("status %d must not re-resolve the Coding Plan key", status)
		}
	}
}

func decodeZAIRequestIdentity(t *testing.T, body []byte) zaiRequestIdentity {
	t.Helper()
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode finalized body: %v", err)
	}
	metadata, _ := request["metadata"].(map[string]any)
	raw, _ := metadata["user_id"].(string)
	if strings.TrimSpace(raw) == "" {
		t.Fatalf("metadata.user_id missing: %s", body)
	}
	var identity zaiRequestIdentity
	if err := json.Unmarshal([]byte(raw), &identity); err != nil {
		t.Fatalf("decode metadata.user_id: %v", err)
	}
	// ZCode's field order is part of the opaque string contract.
	if !strings.HasPrefix(raw, `{"device_id":`) || !strings.Contains(raw, `"account_uuid":""`) {
		t.Fatalf("metadata.user_id shape = %s", raw)
	}
	return identity
}

func TestNormalizeZAIUsageProjectsCodingPlanWindows(t *testing.T) {
	t.Parallel()
	summary, err := normalizeZAIUsage([]zaiauth.QuotaLimit{
		{Type: "TOKENS_LIMIT", Unit: 3, Number: 5, UsedPercent: 42.5, ResetAtMillis: 1787040000000},
		{Type: "TOKENS_LIMIT", Unit: 6, Number: 1, UsedPercent: 120, ResetAtMillis: 0},
		{Type: "MCP_LIMIT", UsedPercent: -1},
	})
	if err != nil {
		t.Fatalf("normalizeZAIUsage() error = %v", err)
	}
	if summary.Provider != zaiauth.ChannelType || summary.PlanType != zaiCodingPlanName {
		t.Fatalf("summary = %+v", summary)
	}
	if len(summary.Windows) != 3 {
		t.Fatalf("windows = %+v", summary.Windows)
	}
	five := summary.Windows[0]
	if five.LimitName != "five_hour" || five.Kind != string(zaiauth.QuotaLimitTokens) ||
		five.UsedPercent != 42.5 || five.RemainingPercent != 57.5 ||
		five.LimitWindowSeconds != 5*3600 || five.ResetAt != 1787040000 {
		t.Fatalf("five hour window = %+v", five)
	}
	// Upstream percentages are clamped: a window is never over 100 or below 0.
	if summary.Windows[1].UsedPercent != 100 || summary.Windows[1].RemainingPercent != 0 {
		t.Fatalf("weekly window = %+v", summary.Windows[1])
	}
	if summary.Windows[2].UsedPercent != 0 || summary.Windows[2].ResetAt != 0 {
		t.Fatalf("mcp window = %+v", summary.Windows[2])
	}
}

func TestNormalizeZAIUsageRejectsEmptyQuota(t *testing.T) {
	t.Parallel()
	if _, err := normalizeZAIUsage(nil); err == nil {
		t.Fatal("normalizeZAIUsage() expected an error")
	}
}

// The usage snapshot must round-trip through the credential so the channel list
// can render it without another upstream call.
func TestZAIUsageSnapshotPersistsOnCredential(t *testing.T) {
	t.Parallel()
	cfg := newZAITestChannel()
	state, err := parseOAuthUsageCredentialState(cfg)
	if err != nil {
		t.Fatalf("parseOAuthUsageCredentialState() error = %v", err)
	}
	if state.provider != zaiauth.ChannelType || state.authType != model.AuthTypeZAIOAuth || state.tracksQuotaCost {
		t.Fatalf("state = %+v", state)
	}
	snapshot := []byte(`{"requested_at":"2026-08-18T00:00:00Z","sampled_at":"2026-08-18T00:00:01Z",` +
		`"summary":{"provider":"zai","windows":[{"limit_name":"five_hour","kind":"tokens","used_percent":42.5,` +
		`"remaining_percent":57.5,"limit_window_seconds":18000,"reset_at":1787040000}]}}`)
	payload, err := state.encode(snapshot, nil)
	if err != nil {
		t.Fatalf("encode() error = %v", err)
	}
	stored, err := zaiauth.ParseCredential([]byte(payload))
	if err != nil {
		t.Fatalf("ParseCredential() error = %v", err)
	}
	usage, _, _ := persistedOAuthUsage(stored.OAuthUsage, zaiauth.ChannelType)
	if usage == nil || len(usage.Windows) != 1 || usage.Windows[0].LimitName != "five_hour" {
		t.Fatalf("persisted usage = %+v", usage)
	}
}
