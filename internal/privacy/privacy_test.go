package privacy_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"ccLoad/internal/privacy"

	"github.com/tidwall/gjson"
)

func enabled() privacy.Config {
	c := privacy.DefaultConfig()
	c.Enabled = true
	c.Personal = true
	return c
}

func newSession(t *testing.T, e *privacy.Engine, scope string, cfg privacy.Config) *privacy.Session {
	t.Helper()
	p, err := privacy.NewPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e.Session(scope, p)
}

/**
 * @brief 同一时钟刻度内完成的文本和 JSON 扫描也应有计时，停用时保持未扫描
 * @param t 测试上下文
 */
func TestMaskDurationRecordsScansWithinClockTick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		text := newSession(t, privacy.NewEngine(), "text", enabled())
		if _, err := text.MaskText("ordinary text"); err != nil {
			t.Fatal(err)
		}
		if text.MaskDuration() <= 0 {
			t.Fatal("completed text scan reported no scan time")
		}
		object := newSession(t, privacy.NewEngine(), "json", enabled())
		if _, err := object.MaskJSON([]byte(`{"content":"ordinary text"}`)); err != nil {
			t.Fatal(err)
		}
		if object.MaskDuration() <= 0 {
			t.Fatal("completed JSON scan reported no scan time")
		}
		disabled := newSession(t, privacy.NewEngine(), "disabled", privacy.DefaultConfig())
		if _, err := disabled.MaskText("ordinary text"); err != nil {
			t.Fatal(err)
		}
		if _, err := disabled.MaskJSON([]byte(`{}`)); err != nil {
			t.Fatal(err)
		}
		if disabled.MaskDuration() != 0 {
			t.Fatal("disabled masking reported scan time")
		}
	})
}

func TestDefaultAndConfig(t *testing.T) {
	defaultConfig, err := privacy.ParseConfig("")
	if err != nil || defaultConfig.Enabled || !defaultConfig.Secrets || defaultConfig.Personal || defaultConfig.Kinds["ip"] || !defaultConfig.Kinds["ssn"] {
		t.Fatalf("unexpected defaults: %+v, %v", defaultConfig, err)
	}
	body := []byte(`{ "content": "password=hello123world", "n": 12345678901234567890 }`)
	s := newSession(t, privacy.NewEngine(), "default", defaultConfig)
	out, err := s.MaskJSON(body)
	if err != nil || string(out) != string(body) {
		t.Fatalf("disabled config changed request: %s, %v", out, err)
	}
	for _, raw := range []string{
		`null`, `[]`, `{"enabled":"yes"}`, `{"Enabled":true}`, `{"rules":[{"prefix":"abc","regex":null}]}`, `{"rules":[{"Prefix":"abc"}]}`, `{"words":["line1\nline2"]}`, `{"enabled":null}`, `{"wat":true}`,
		`{"kinds":{"unknown":true}}`, `{"kinds":{"ip":null}}`,
		`{"rules":[{"regex":"["}]}`, `{"rules":[{"regex":"a*"}]}`,
		`{"rules":[{"prefix":"a"}]}`, `{"rules":[{"prefix":"abc","regex":"a"}]}`,
		`{"rules":[{"kind":"1A","regex":"abc"}]}`, `{"words":[null]}`,
		`{"rules":[{"regex":"a","extra":"secretvalue"}]}`,
	} {
		if _, err := privacy.ParseConfig(raw); err == nil {
			t.Errorf("accepted invalid config %s", raw)
		} else if strings.Contains(err.Error(), "secretvalue") {
			t.Error("configuration error leaked a value")
		}
	}
	// Errors name the 1-based rule so the settings dialog can point at it.
	if _, err := privacy.ParseConfig(`{"rules":[{"prefix":"abc"},{"regex":"("}]}`); err == nil || !strings.Contains(err.Error(), "rule 2 ") {
		t.Errorf("rule error does not name the rule: %v", err)
	}
	c, err := privacy.ParseConfig(`{"enabled":true,"kinds":{"ip":true},"rules":[{"kind":"RELAY","prefix":"relay_"}]}`)
	if err != nil || !c.Enabled || !c.Secrets || !c.Kinds["ssn"] || !c.Kinds["ip"] || c.Rules[0].Kind != "RELAY" {
		t.Fatalf("partial defaults not merged: %+v %v", c, err)
	}
	tooMany := privacy.DefaultConfig()
	tooMany.Words = make([]string, 257)
	for i := range tooMany.Words {
		tooMany.Words[i] = "word"
	}
	raw, _ := json.Marshal(tooMany)
	if _, err := privacy.ParseConfig(string(raw)); err == nil {
		t.Error("accepted too many words")
	}
}

func TestBuiltinRules(t *testing.T) {
	cases := []struct{ name, text, secret string }{
		{"private", "-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----", "-----BEGIN"},
		{"openai", "sk-proj-ABcDef012345678901234567890123456", "sk-proj-"},
		{"relay", "oc_sk_ABcDef012345678901234567890123456", "oc_sk_"},
		{"relay-letters", "oc_sk_AbCDefGHijKLmnOPqrSTuvWX", "oc_sk_"},
		{"ark", "ark-ABcDef012345678901234567890123456", "ark-"},
		{"github", "ghp_" + strings.Repeat("A9", 20), "ghp_"},
		{"cloudflare-user", "cfut_" + strings.Repeat("A9", 20), "cfut_"},
		{"cloudflare-account", "cfat_" + strings.Repeat("A9", 20), "cfat_"},
		{"cloudflare-global", "cfk_" + strings.Repeat("A9", 20), "cfk_"},
		{"cloudflare-oauth", "cfoat_" + strings.Repeat("A9", 20), "cfoat_"},
		{"cloudflare-origin", "v1.0-" + strings.Repeat("c", 16) + "-" + strings.Repeat("d", 40), "v1.0-"},
		{"cloudflare-tunnel", base64.StdEncoding.EncodeToString([]byte(`{"a":"account-for-test","t":"tunnel-for-test","s":"synthetic-secret"}`)), "eyJhIjoi"},
		{"cloudflare-header", "X-Auth-Key: " + strings.Repeat("c", 37), strings.Repeat("c", 37)},
		{"cloudflare-origin-header", "X-Auth-User-Service-Key: " + strings.Repeat("d", 40), strings.Repeat("d", 40)},
		{"email-in-pdf", "send invoices/lea" + "@acme.com.PDF", "@acme.com"},
		{"email-in-archive", "backup lea" + "@acme.com.tar.gz", "@acme.com"},
		{"aws", "AKIAIOSFODNN7EXAMPLE", "AKIA"},
		{"jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcDEF0123456789", "eyJ"},
		{"url", "postgres://alice:s3cr3tPass@db.local/db", "s3cr3tPass"},
		{"assignment", "API_KEY=AbC123dEf456GhI789", "AbC123"},
		{"header", "Authorization: Bearer opaque-token-value", "opaque-token-value"},
		{"cookie", "Cookie: session=opaquevalue123", "opaquevalue123"},
		{"letters", "secret: CorrectHorseBatteryStaple", "CorrectHorseBatteryStaple"},
		{"chinese", "我的密码是 786kih", "786kih"},
		{"traditional", "密碼：hunter2", "hunter2"},
		{"japanese", "パスワードは abc12345", "abc12345"},
		{"korean", "비밀번호는 abc12345", "abc12345"},
		{"numeric-key", "密钥为12231241", "12231241"},
		{"email", "Reach alice@mycompany.com please", "alice@mycompany.com"},
		{"phone", "手机号 13800138000", "13800138000"},
		{"id", "11010519491231002X", "11010519491231002X"},
		{"bank", "4111111111111111", "4111111111111111"},
		{"ssn", "321-54-9876", "321-54-9876"},
		{"passport", "Passport No: E12345678", "E12345678"},
		{"iban", "DE89370400440532013000", "DE89370400440532013000"},
		{"iban-spaced", "BE68 5390 0754 7034 THEN", "BE68 5390 0754 7034"},
		{"birthday", "生日：1990年5月12日", "1990年5月12日"},
		{"mac", "a4:5e:60:12:3b:4c", "a4:5e:60:12:3b:4c"},
		{"serial", "Serial number: C02X9AB12345", "C02X9AB12345"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSession(t, privacy.NewEngine(), "test", enabled())
			out, err := s.MaskText(tc.text)
			if err != nil || strings.Contains(out, tc.secret) || !strings.Contains(out, "{{CCLOAD_") {
				t.Fatalf("did not mask %s: %q (%v)", tc.name, out, err)
			}
			if restored := s.RestoreText(out); restored != tc.text {
				t.Fatalf("restored %q, want %q", restored, tc.text)
			}
		})
	}
}

func TestFalsePositives(t *testing.T) {
	for _, text := range []string{
		"password=${PASSWORD}", "PRIVATE_KEY_PATH=~/.ssh/id_ed25519", "access_token = get_access_token_v2",
		"UI_PASSWORD= UI_ADDR=127.0.0.1:39117", "密码： --listen=127.0.0.1:39117",
		`access_token_expires_at: "2026-11-05T12:34:56.123+08:00"`, "令牌是 2026-11-05T12:34:56Z",
		"icon@2x.jpg icon@2x.zip logo@final.pdf lib@base.json", "cfut_" + strings.Repeat("c", 39), strings.Repeat("c", 64),
		base64.StdEncoding.EncodeToString([]byte(`{"a":"account-for-test","t":"ordinary data without a credential"}`)),
		"max_tokens=max_tokens_v3", "令牌是 300000 毫秒", "token.go:L643-669", `"jsonwebtoken": "9.2.0-beta1"`,
		"订单号 13800138000", "order_id=13800138000", "9f13800138000abc",
		"123-45-6789", "999-00-0000", "4111111111111112", "110105194912310021",
		"DOB: 1990-02-30", "release 1990-05-12", "00:00:00:00:00:00", "aa:bb:cc:dd:ee:ff",
		"DE89370400440532013001", "user@package.notarealtld", "sk-this-is-a-public-documentation-example",
		"{ key: 'blocked_requests', format: 'number' }", "labelKey: 'settings.runtimeMetrics.metric.privacyEnabled',", `"key": "dictionaryEntriesTotal"`,
	} {
		s := newSession(t, privacy.NewEngine(), "test", enabled())
		out, err := s.MaskText(text)
		if err != nil || out != text {
			t.Errorf("false positive %q -> %q (%v)", text, out, err)
		}
	}
}

func TestOptionalKindsAndWords(t *testing.T) {
	cfg := enabled()
	text := "/Users/alicechen/project alicechen@work-machine:~$ 45.76.12.34 s3://company-prod-archive"
	s := newSession(t, privacy.NewEngine(), "test", cfg)
	out, err := s.MaskText(text)
	if err != nil || out != text {
		t.Fatalf("optional categories enabled by default: %q %v", out, err)
	}
	for _, kind := range []string{"home", "userhost", "ip", "bucket"} {
		cfg.Kinds[kind] = true
	}
	s = newSession(t, privacy.NewEngine(), "test", cfg)
	out, err = s.MaskText(text)
	for _, value := range []string{"alicechen", "work-machine", "45.76.12.34", "company-prod-archive"} {
		if err != nil || strings.Contains(out, value) {
			t.Errorf("optional category did not mask %s: %q %v", value, out, err)
		}
	}
	for _, text := range []string{"10.0.0.1", "127.0.0.1", "203.0.113.42", "8.8.8.8", "version 45.76.12.34", "git@github.com:~/", "s3://my-bucket"} {
		fresh := newSession(t, privacy.NewEngine(), "test", cfg)
		out, err := fresh.MaskText(text)
		if err != nil || out != text {
			t.Errorf("optional false positive %q -> %q %v", text, out, err)
		}
	}
	cfg.Words = []string{"Acme", "项目甲"}
	cfg.WordsAnyCase, cfg.WordsWhole = true, true
	cfg.Allow = []string{"ACME"}
	s = newSession(t, privacy.NewEngine(), "test", cfg)
	out, err = s.MaskText("acme acmeOther 项目甲 项目甲乙")
	if err != nil || !strings.HasPrefix(out, "acme acmeOther {{CCLOAD_WORD_") || !strings.HasSuffix(out, " 项目甲乙") {
		t.Fatalf("word/allow mismatch: %q %v", out, err)
	}
	cfg.Rules = []privacy.Rule{{Kind: "RELAY", Prefix: "relay_"}, {Kind: "LOCAL", Regex: `local=([A-Z0-9]{8})`}}
	s = newSession(t, privacy.NewEngine(), "test", cfg)
	out, err = s.MaskText("relay_ABCDEF123456 local=ABCD1234")
	if err != nil || strings.Contains(out, "ABCDEF123456") || !strings.Contains(out, "local={{CCLOAD_LOCAL_") {
		t.Fatalf("custom rules mismatch %q %v", out, err)
	}
}

func TestScopeStabilitySnapshotAndConcurrentUse(t *testing.T) {
	e := privacy.NewEngine()
	cfg := enabled()
	cfg.Words = []string{"ProjectSecret"}
	s := newSession(t, e, "token-a", cfg)
	cfg.Words[0] = "changed"
	cfg.Kinds["ssn"] = false
	first, err := s.MaskText("ProjectSecret password=S3cr3tPass123")
	if err != nil || strings.Contains(first, "ProjectSecret") {
		t.Fatal("session did not snapshot configuration")
	}
	same := newSession(t, e, "token-a", enabled())
	second, _ := same.MaskText("password=S3cr3tPass123")
	if !strings.Contains(first, strings.TrimPrefix(second, "password=")) {
		t.Fatal("same scope changed placeholder")
	}
	if same.RestoreText(first) != "ProjectSecret password=S3cr3tPass123" {
		t.Fatal("new session lost prior mapping")
	}
	other := newSession(t, e, "token-b", enabled())
	third, _ := other.MaskText("password=S3cr3tPass123")
	if third == second || other.RestoreText(first) != first {
		t.Fatal("scope mappings leaked")
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			out, err := same.MaskText("password=S3cr3tPass123")
			if err != nil || out != second || same.RestoreText(out) != "password=S3cr3tPass123" {
				t.Error("concurrent mapping failed")
			}
		})
	}
	wg.Wait()
	// Mapping is reused when a later message no longer labels the secret.
	out, err := same.MaskText("use S3cr3tPass123 again")
	if err != nil || strings.Contains(out, "S3cr3tPass123") {
		t.Fatalf("known secret leaked %q %v", out, err)
	}
}

func TestJSONProtocolBoundaryAndRestoreEscaping(t *testing.T) {
	cfg := enabled()
	cfg.Words = []string{"my_model", "my_function", "enum_value", "secretname"}
	s := newSession(t, privacy.NewEngine(), "test", cfg)
	body := []byte(`{"model":"my_model","messages":[{"role":"user","content":"alice@company.com my_model"},{"role":"assistant","tool_calls":[{"id":"my_function","type":"function","function":{"name":"my_function","arguments":"{\"name\":\"secretname\",\"api_key\":\"opaque-key-value\",\"mode\":\"enum_value\"}"}}]}],"tools":[{"type":"function","function":{"name":"my_function","description":"alice@company.com","parameters":{"type":"object","properties":{"secretname":{"type":"string","enum":["enum_value"],"description":"alice@company.com"}}}}}],"signature":"alice@company.com","encrypted_content":"alice@company.com","image_url":{"url":"https://img/alice@company.com"},"n":12345678901234567890}`)
	out, err := s.MaskJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["model"] != "my_model" || doc["signature"] != "alice@company.com" || doc["encrypted_content"] != "alice@company.com" {
		t.Fatal("protocol fields changed")
	}
	tools := doc["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if tools["name"] != "my_function" || tools["description"] == "alice@company.com" {
		t.Fatal("tool definition handling failed")
	}
	props := tools["parameters"].(map[string]any)["properties"].(map[string]any)
	if props["secretname"].(map[string]any)["enum"].([]any)[0] != "enum_value" {
		t.Fatal("schema enum changed")
	}
	messages := doc["messages"].([]any)
	fn := messages[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	var args map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatal(err)
	}
	if fn["name"] != "my_function" || args["name"] == "secretname" || args["api_key"] == "opaque-key-value" {
		t.Fatal("tool arguments were not masked")
	}
	restored, err := s.RestoreJSON(out)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(body, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(restored, &got); err != nil {
		t.Fatal(err)
	}
	// Embedded JSON string formatting is not part of the tool contract.
	var wantDoc, gotDoc map[string]any
	_ = json.Unmarshal(body, &wantDoc)
	_ = json.Unmarshal(restored, &gotDoc)
	normalizeArguments(t, wantDoc)
	normalizeArguments(t, gotDoc)
	want, got = wantDoc, gotDoc
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if string(wantJSON) != string(gotJSON) {
		t.Fatalf("JSON restore changed content: %s", restored)
	}
	if !strings.Contains(string(out), "12345678901234567890") {
		t.Fatal("numeric literal rounded")
	}

	secret := "a\"b\\c\nvalue"
	cfg = enabled()
	cfg.Rules = []privacy.Rule{{Kind: "SECRET", Regex: `(?s)secret=(.+)`}}
	s = newSession(t, privacy.NewEngine(), "escaping", cfg)
	maskedWithLabel, err := s.MaskText("secret=" + secret)
	masked := strings.TrimPrefix(maskedWithLabel, "secret=")
	if err != nil {
		t.Fatal(err)
	}
	inner, _ := json.Marshal(map[string]string{"value": masked})
	outer, _ := json.Marshal(map[string]string{"arguments": string(inner)})
	restored, err = s.RestoreJSON(outer)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]string
	if err := json.Unmarshal(restored, &response); err != nil {
		t.Fatal(err)
	}
	var nested map[string]string
	if err := json.Unmarshal([]byte(response["arguments"]), &nested); err != nil || nested["value"] != secret {
		t.Fatalf("unsafe arguments restoration %s %v", restored, err)
	}
	fragment, _ := json.Marshal(map[string]string{"type": "response.function_call_arguments.delta", "delta": masked})
	restored, err = s.RestoreJSON(fragment)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(restored, &response); err != nil {
		t.Fatal(err)
	}
	assembled := `{"value":"` + response["delta"] + `"}`
	if err := json.Unmarshal([]byte(assembled), &nested); err != nil || nested["value"] != secret {
		t.Fatalf("unsafe fragment restoration %s %v", assembled, err)
	}
	unchanged := []byte(`{ "arguments": "{\"value\":\"already restored\"}", "n": 1e999 }`)
	nochange, err := s.RestoreJSON(unchanged)
	if err != nil || string(nochange) != string(unchanged) {
		t.Fatal("unmodified JSON reencoded")
	}
	if _, err := s.MaskJSON([]byte(`{"broken":`)); err == nil {
		t.Fatal("invalid JSON sent as plaintext")
	}
}

func TestCapacityFailsClosed(t *testing.T) {
	cfg := enabled()
	cfg.Rules = []privacy.Rule{{Kind: "CUSTOM", Prefix: "local_"}}
	s := newSession(t, privacy.NewEngine(), "limit", cfg)
	var body strings.Builder
	for i := range 4097 {
		fmt.Fprintf(&body, "local_%08d ", i)
	}
	out, err := s.MaskText(body.String())
	if err == nil || out != "" {
		t.Fatal("capacity exhaustion returned plaintext")
	}
}

func normalizeArguments(t *testing.T, value any) {
	t.Helper()
	switch item := value.(type) {
	case map[string]any:
		for key, v := range item {
			if key == "arguments" {
				if text, ok := v.(string); ok {
					var nested any
					if err := json.Unmarshal([]byte(text), &nested); err != nil {
						t.Fatal(err)
					}
					item[key] = nested
				}
			} else {
				normalizeArguments(t, v)
			}
		}
	case []any:
		for _, v := range item {
			normalizeArguments(t, v)
		}
	}
}

/**
 * @brief 空赋值后继续按下一个字段名判定，带等号的真实密钥仍可还原。
 * @param t 测试上下文。
 */
func TestEmptyAssignmentFollowedBySecret(t *testing.T) {
	for _, prefix := range []string{"UI_PASSWORD= UI_TOKEN=", "DB_PASS= DB_KEY=", "密码： token=", "token: ", "token= FIELD= token="} {
		const value = "syntheticAB123=="
		session := newSession(t, privacy.NewEngine(), "field", enabled())
		masked, err := session.MaskText(prefix + value)
		if err != nil || !strings.HasPrefix(masked, prefix+"{{CCLOAD_") || session.RestoreText(masked) != prefix+value {
			t.Errorf("%q: %s, %v", prefix, masked, err)
		}
	}
	for _, text := range []string{"password: hunter2=Xq9zLm", "PASSWORD: ROOT=Zx81kLp9"} {
		session := newSession(t, privacy.NewEngine(), "field", enabled())
		masked, err := session.MaskText(text)
		value := text[strings.Index(text, " ")+1:]
		if err != nil || strings.Contains(masked, value) || session.RestoreText(masked) != text {
			t.Errorf("%q: %s, %v", text, masked, err)
		}
	}
}

/**
 * @brief 工具负载内按用户数据脱敏，调用信封保真；到期时间不是凭据。
 * @param t 测试上下文。
 */
func TestToolCallPrivacyBoundaries(t *testing.T) {
	const marker = "synthetic-marker"
	const expiry = "2026-11-05T12:34:56.123+08:00"
	cfg := enabled()
	cfg.Words = []string{marker}
	for _, tc := range []struct{ event, field string }{
		{"mcp_tool_use", "input"}, {"server_tool_use", "input"}, {"function_call", "arguments"}, {"mcp_call", "arguments"},
		{"custom_tool_call", "input"}, {"local_shell_call", "action"}, {"shell_call", "action"}, {"computer_call", "action"}, {"apply_patch_call", "operation"},
	} {
		t.Run(tc.event, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{"type": tc.event, "id": marker, "name": marker, "server_name": marker, tc.field: map[string]string{
				"data": marker, "customer_id": marker, "access_token_expires_at": expiry, "password": "ordinaryvalue",
			}})
			if err != nil {
				t.Fatal(err)
			}
			session := newSession(t, privacy.NewEngine(), "tool", cfg)
			masked, err := session.MaskJSON(body)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"id", "name", "server_name"} {
				if gjson.GetBytes(masked, field).Str != marker {
					t.Errorf("protocol field %s changed", field)
				}
			}
			for _, field := range []string{"data", "customer_id", "password"} {
				if !strings.HasPrefix(gjson.GetBytes(masked, tc.field+"."+field).Str, "{{CCLOAD_") {
					t.Errorf("payload field %s was not masked", field)
				}
			}
			if gjson.GetBytes(masked, tc.field+".access_token_expires_at").Str != expiry {
				t.Error("expiry was masked")
			}
			restored, err := session.RestoreJSON(masked)
			if err != nil || string(restored) != string(body) {
				t.Fatalf("tool round trip: %s, %v", restored, err)
			}
		})
	}
	for _, tc := range []struct{ event, field string }{{"function_call", "arguments"}, {"function_call_output", "output"}} {
		payload := `{"token_expires_at":"` + expiry + `","token":"2026-11-05T12NotAnExpiry"}`
		body, err := json.Marshal(map[string]string{"type": tc.event, tc.field: payload})
		if err != nil {
			t.Fatal(err)
		}
		session := newSession(t, privacy.NewEngine(), "embedded", cfg)
		masked, err := session.MaskJSON(body)
		inner := gjson.GetBytes(masked, tc.field).Str
		if err != nil || gjson.Get(inner, "token_expires_at").Str != expiry || !strings.HasPrefix(gjson.Get(inner, "token").Str, "{{CCLOAD_") {
			t.Errorf("%s: %s, %v", tc.field, inner, err)
		}
	}
}

func TestOtherProtocolToolPayloads(t *testing.T) {
	for _, body := range []string{
		`{"input":"password=Hello123secret","content":[{"type":"tool_use","id":"tool-1","name":"get_credential","input":{"name":"alice@company.com","password":"opaque-credential-value"}}]}`,
		`{"contents":[{"role":"model","parts":[{"functionCall":{"name":"get_credential","args":{"name":"alice@company.com","password":"opaque-credential-value"}}},{"functionResponse":{"name":"get_credential","response":{"password":"opaque-credential-value"}}},{"text":"alice@company.com","thoughtSignature":"alice@company.com"}]}]}`,
	} {
		s := newSession(t, privacy.NewEngine(), "protocol", enabled())
		out, err := s.MaskJSON([]byte(body))
		if err != nil || strings.Contains(string(out), "opaque-credential-value") || strings.Contains(string(out), "Hello123secret") {
			t.Fatalf("tool payload leak: %s %v", out, err)
		}
		if !strings.Contains(string(out), "get_credential") {
			t.Fatal("tool name changed")
		}
		restored, err := s.RestoreJSON(out)
		if err != nil {
			t.Fatal(err)
		}
		var want, got any
		if err := json.Unmarshal([]byte(body), &want); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(restored, &got); err != nil {
			t.Fatal(err)
		}
		a, _ := json.Marshal(want)
		b, _ := json.Marshal(got)
		if string(a) != string(b) {
			t.Fatalf("protocol round trip mismatch %s", restored)
		}
	}
}

func TestToolResultEmbeddedJSONDecodesSensitiveValues(t *testing.T) {
	protocols := []struct{ name, template, path string }{
		{"responses", `{"input":[{"type":"function_call_output","call_id":"call-1","output":%s}]}`, "input.0.output"},
		{"responses custom", `{"input":[{"type":"custom_tool_call_output","call_id":"call-1","output":%s}]}`, "input.0.output"},
		{"chat", `{"messages":[{"role":"tool","tool_call_id":"call-1","content":%s}]}`, "messages.0.content"},
		{"chat text part", `{"messages":[{"role":"tool","tool_call_id":"call-1","content":[{"type":"text","text":%s}]}]}`, "messages.0.content.0.text"},
		{"anthropic", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":%s}]}]}`, "messages.0.content.0.content"},
		{"anthropic text part", `{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":[{"type":"text","text":%s}]}]}]}`, "messages.0.content.0.content.0.text"},
		{"gemini", `{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"result":%s}}}]}]}`, "contents.0.parts.0.functionResponse.response.result"},
		{"gemini snake", `{"contents":[{"role":"user","parts":[{"function_response":{"name":"lookup","response":{"output":%s}}}]}]}`, "contents.0.parts.0.function_response.response.output"},
	}
	secrets := []struct{ name, wire, value string }{
		{"word", `alice\u0040mycompany.com`, "alice@mycompany.com"},
		{"api key", `\u0073k-proj-ABcDef012345678901234567890123456`, "sk-proj-ABcDef012345678901234567890123456"},
		{"pem", `-----BEGIN PRIVATE KEY-----\nabc\"quoted\\value\n-----END PRIVATE KEY-----`, "-----BEGIN PRIVATE KEY-----\nabc\"quoted\\value\n-----END PRIVATE KEY-----"},
		{"surrogate pair", `before\ud83d\ude3cafter`, "before😼after"},
		{"unpaired surrogate", `\ud800\u0061lice\u0040mycompany.com`, "�alice@mycompany.com"},
		{"escaping", `a\"b\\c\nvalue`, "a\"b\\c\nvalue"},
	}
	for _, protocol := range protocols {
		for _, secret := range secrets {
			t.Run(protocol.name+"/"+secret.name, func(t *testing.T) {
				cfg := privacy.DefaultConfig()
				cfg.Enabled = true
				cfg.Secrets = secret.name == "api key" || secret.name == "pem"
				if !cfg.Secrets {
					if secret.name == "escaping" {
						cfg.Rules = []privacy.Rule{{Kind: "WORD", Regex: regexp.QuoteMeta(secret.value)}}
					} else {
						cfg.Words = []string{secret.value}
					}
				}
				s := newSession(t, privacy.NewEngine(), "tool-result", cfg)
				payload := `{ "type": "tool_search_output", "value": "` + secret.wire + `", "number": 12345678901234567890, "safe": "café \/ <ok>" }`
				quoted, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				body := fmt.Appendf(nil, protocol.template, quoted)
				masked, err := s.MaskJSON(body)
				if err != nil || !json.Valid(masked) {
					t.Fatalf("invalid masked JSON: %s %v", masked, err)
				}
				inner := gjson.GetBytes(masked, protocol.path).String()
				if !json.Valid([]byte(inner)) {
					t.Fatalf("invalid embedded JSON: %s", inner)
				}
				token := gjson.Get(inner, "value").String()
				if !strings.HasPrefix(token, "{{CCLOAD_") || s.RestoreText(token) != secret.value {
					t.Fatalf("decoded value was not masked/restorable: %q", token)
				}
				reply, _ := json.Marshal(map[string]string{"content": token, "arguments": `{"value":"` + token + `"}`})
				restoredReply, err := s.RestoreJSON(reply)
				if err != nil || gjson.GetBytes(restoredReply, "content").String() != secret.value {
					t.Fatalf("ordinary reply did not restore decoded value: %s %v", restoredReply, err)
				}
				arguments := gjson.GetBytes(restoredReply, "arguments").String()
				if !json.Valid([]byte(arguments)) || gjson.Get(arguments, "value").String() != secret.value {
					t.Fatalf("arguments did not restore decoded value safely: %s", restoredReply)
				}
				if gjson.Get(inner, "number").Raw != "12345678901234567890" || gjson.Get(inner, "safe").String() != "café / <ok>" {
					t.Fatalf("unrelated tool data changed: %s", inner)
				}
				if again, err := s.MaskJSON(masked); err != nil || string(again) != string(masked) {
					t.Fatalf("repeated masking changed placeholders: %s %v", again, err)
				}
				restored, err := s.RestoreJSON(masked)
				if err != nil {
					t.Fatal(err)
				}
				restoredText := gjson.GetBytes(restored, protocol.path).String()
				if !json.Valid([]byte(restoredText)) || gjson.Get(restoredText, "value").String() != secret.value {
					t.Fatalf("embedded value restored with wrong escaping: %s", restored)
				}
			})
		}
	}
}

func TestToolResultEmbeddedJSONRetainsRawTextRules(t *testing.T) {
	for _, tc := range []struct {
		name, payload, regex string
		removed              []string
	}{
		{"number", `{"count":12345678,"value":"alice\u0040mycompany.com","secret":"a\"b","existing":"{{CCLOAD_WORD_0123456789abcdef}}"}`, `12345678`, []string{"12345678"}},
		{"key", `{"private_key_name":"safe","value":"alice\u0040mycompany.com"}`, `private_key_name`, []string{"private_key_name"}},
		{"cross syntax", `{"private":"confidential","count":123,"value":"alice\u0040mycompany.com"}`, `"private":"confidential","count":123`, []string{"confidential", `"count"`}},
		{"cross string boundary", `{"first":"alpha","second":"beta","value":"alice\u0040mycompany.com","secret":"a\"b"}`, `alpha","second":"be`, []string{"alpha", "beta"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := privacy.DefaultConfig()
			cfg.Enabled, cfg.Secrets = true, false
			cfg.Words = []string{"alice@mycompany.com", `a"b`}
			cfg.Rules = []privacy.Rule{{Kind: "RAW", Regex: tc.regex}}
			s := newSession(t, privacy.NewEngine(), "mixed", cfg)
			quoted, _ := json.Marshal(tc.payload)
			body := fmt.Appendf(nil, `{"input":[{"type":"function_call_output","output":%s}]}`, quoted)
			masked, err := s.MaskJSON(body)
			if err != nil || !json.Valid(masked) {
				t.Fatalf("invalid outer JSON: %s %v", masked, err)
			}
			text := gjson.GetBytes(masked, "input.0.output").String()
			if !strings.Contains(text, "{{CCLOAD_RAW_") || !strings.Contains(text, "{{CCLOAD_WORD_") || strings.Contains(text, `alice\u0040mycompany.com`) {
				t.Fatalf("raw and decoded rules did not both mask: %s", text)
			}
			if again, err := s.MaskJSON(masked); err != nil || string(again) != string(masked) {
				t.Fatalf("repeated masking changed placeholders: %s %v", again, err)
			}
			visible := regexp.MustCompile(`\{\{CCLOAD_[A-Z_]+_[a-f0-9]{16}\}\}`).ReplaceAllString(text, "")
			for _, value := range tc.removed {
				if strings.Contains(visible, value) {
					t.Fatalf("raw match %q survived: %s", value, text)
				}
			}
			restored, err := s.RestoreJSON(masked)
			if err != nil {
				t.Fatal(err)
			}
			var want, got any
			if err := json.Unmarshal([]byte(tc.payload), &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(gjson.GetBytes(restored, "input.0.output").String()), &got); err != nil {
				t.Fatalf("mixed tool result did not restore valid JSON: %s %v", restored, err)
			}
			wantJSON, _ := json.Marshal(want)
			gotJSON, _ := json.Marshal(got)
			if string(wantJSON) != string(gotJSON) {
				t.Fatalf("mixed tool result content changed: %s", restored)
			}
		})
	}
	t.Run("raw and decoded duplicates share match budget", func(t *testing.T) {
		cfg := privacy.DefaultConfig()
		cfg.Enabled, cfg.Secrets = true, false
		cfg.Words = []string{"privatevalue"}
		s := newSession(t, privacy.NewEngine(), "repeated", cfg)
		payload := `[` + strings.Repeat(`"privatevalue",`, 32768) + `"privatevalue"]`
		quoted, _ := json.Marshal(payload)
		body := fmt.Appendf(nil, `{"input":[{"type":"function_call_output","output":%s}]}`, quoted)
		masked, err := s.MaskJSON(body)
		if err != nil || strings.Contains(string(masked), "privatevalue") || strings.Count(string(masked), "{{CCLOAD_WORD_") != 32769 {
			t.Fatalf("duplicate raw/decoded candidates exhausted match budget: %v", err)
		}
	})
	t.Run("raw syntax and decoded strings stay distinguishable", func(t *testing.T) {
		cfg := privacy.DefaultConfig()
		cfg.Enabled, cfg.Secrets = true, false
		cfg.Rules = []privacy.Rule{{Kind: "RAW", Regex: `alpha","second":"be`}}
		s := newSession(t, privacy.NewEngine(), "same-scope", cfg)
		inputs := []string{`{"first":"alpha","second":"beta"}`, `{"first":"alpha\",\"second\":\"beta"}`}
		var previous string
		for _, input := range inputs {
			quoted, _ := json.Marshal(input)
			body := fmt.Appendf(nil, `{"input":[{"type":"function_call_output","output":%s}]}`, quoted)
			masked, err := s.MaskJSON(body)
			if err != nil {
				t.Fatal(err)
			}
			text := gjson.GetBytes(masked, "input.0.output").String()
			if text == input || text == previous {
				t.Fatalf("distinct documents collapsed to the same masked text: %s", text)
			}
			previous = text
			restored, err := s.RestoreJSON(masked)
			if err != nil || gjson.GetBytes(restored, "input.0.output").String() != input {
				t.Fatalf("tool result did not restore its original structure: %s %v", restored, err)
			}
		}
	})
}

func TestToolResultJSONDecodingPreservesOtherProtocolFields(t *testing.T) {
	cfg := privacy.DefaultConfig()
	cfg.Enabled, cfg.Secrets = true, false
	cfg.Words = []string{"alice@mycompany.com", "CCLOAD"}
	s := newSession(t, privacy.NewEngine(), "boundaries", cfg)
	text := `{"value":"alice\u0040mycompany.com"}`
	quoted, _ := json.Marshal(text)
	body := fmt.Appendf(nil, `{"messages":[{"role":"user","content":%s}],"text":%s,"output":%s,"content":[{"type":"tool_result","tool_use_id":"alice@mycompany.com","content":[{"type":"image","source":{"type":"base64","data":"alice@mycompany.com"}},{"type":"text","text":%s}]}]}`, quoted, quoted, quoted, quoted)
	masked, err := s.MaskJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"messages.0.content", "text", "output"} {
		if gjson.GetBytes(masked, path).String() != text {
			t.Fatalf("ordinary text %s acquired JSON decoding: %s", path, masked)
		}
	}
	for _, path := range []string{"content.0.tool_use_id", "content.0.content.0.source.data"} {
		if gjson.GetBytes(masked, path).String() != "alice@mycompany.com" {
			t.Fatalf("tool result protocol field %s changed: %s", path, masked)
		}
	}
	if inner := gjson.GetBytes(masked, "content.0.content.1.text").String(); gjson.Get(inner, "value").String() == "alice@mycompany.com" || !strings.Contains(inner, "{{CCLOAD_WORD_") {
		t.Fatalf("tool result text was not decoded: %s", inner)
	}
	// Even escaped existing placeholders stay protected from a custom word.
	placeholder := `{"value":"\u007b\u007bCCLOAD_WORD_0123456789abcdef\u007d\u007d"}`
	quoted, _ = json.Marshal(placeholder)
	protectedBody := fmt.Appendf(nil, `{"input":[{"type":"function_call_output","output":%s}]}`, quoted)
	if out, err := s.MaskJSON(protectedBody); err != nil || string(out) != string(protectedBody) {
		t.Fatalf("escaped existing placeholder changed: %s %v", out, err)
	}
}

func TestToolResultJSONInvalidUTF8UsesDecodedOffsets(t *testing.T) {
	for _, tc := range []struct {
		name, key, wire, regex, want string
		secrets                      bool
	}{
		{"decoded word", "value", "\xff" + `alice\u0040mycompany.com`, "", "alice@mycompany.com", false},
		{"raw rule after invalid bytes", "value", "\xe2\x82" + `\u0061lice\u0040mycompany.com`, `u0061lice`, "alice", false},
		{"secret field", "api_key", "\xff" + `secret\u0040mycompany.com`, "", "�secret@mycompany.com", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := privacy.DefaultConfig()
			cfg.Enabled, cfg.Secrets = true, tc.secrets
			if tc.regex != "" {
				cfg.Rules = []privacy.Rule{{Kind: "RAW", Regex: tc.regex}}
			} else if !tc.secrets {
				cfg.Words = []string{tc.want}
			}
			s := newSession(t, privacy.NewEngine(), "invalid-utf8", cfg)
			payload := `{"` + tc.key + `":"` + tc.wire + `"}`
			// encoding/json accepts malformed UTF-8 in otherwise valid JSON,
			// replacing each invalid byte with U+FFFD while decoding strings.
			quoted := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(payload)
			body := []byte(`{"input":[{"type":"function_call_output","output":"` + quoted + `"}]}`)
			if !json.Valid(body) {
				t.Fatal("fixture is not valid JSON")
			}
			masked, err := s.MaskJSON(body)
			if err != nil || !json.Valid(masked) {
				t.Fatalf("MaskJSON = %q, %v", masked, err)
			}
			inner := gjson.GetBytes(masked, "input.0.output").String()
			value := gjson.Get(inner, tc.key).String()
			token := regexp.MustCompile(`\{\{CCLOAD_[A-Z_]+_[a-f0-9]{16}\}\}`).FindString(value)
			if token == "" || s.RestoreText(token) != tc.want {
				t.Fatalf("decoded secret = %q, want %q (masked %q)", s.RestoreText(token), tc.want, inner)
			}
		})
	}
}

func TestToolResultJSONCacheKeepsScopeValuesDynamic(t *testing.T) {
	cfg := privacy.DefaultConfig()
	cfg.Enabled = true
	policy, err := privacy.NewPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	engine := privacy.NewEngine()
	first, second := engine.Session("first", policy), engine.Session("second", policy)
	secret := "PrivateValue01234567"
	if _, err := first.MaskText("password=" + secret); err != nil {
		t.Fatal(err)
	}
	payload := `{"value":"PrivateValue\u00301234567","safe":"` + strings.Repeat("ordinary ", 40) + `"}`
	quoted, _ := json.Marshal(payload)
	body := fmt.Appendf(nil, `{"input":[{"type":"function_call_output","output":%s}]}`, quoted)
	assertMasked := func(s *privacy.Session, want bool) {
		t.Helper()
		masked, err := s.MaskJSON(body)
		if err != nil {
			t.Fatal(err)
		}
		inner := gjson.GetBytes(masked, "input.0.output").String()
		value := gjson.Get(inner, "value").String()
		if strings.HasPrefix(value, "{{CCLOAD_") != want {
			t.Fatalf("scope-specific masking = %q, want masked %v", value, want)
		}
		if want && s.RestoreText(value) != secret {
			t.Fatalf("cached hit restored %q, want %q", s.RestoreText(value), secret)
		}
	}
	assertMasked(first, true)
	assertMasked(second, false)
	if _, err := second.MaskText("password=" + secret); err != nil {
		t.Fatal(err)
	}
	assertMasked(second, true)
}

func TestToolResultJSONOverlappingRulesRetainEligibleHits(t *testing.T) {
	cfg := privacy.DefaultConfig()
	cfg.Enabled = true
	cfg.Rules = []privacy.Rule{{Kind: "CUSTOM", Regex: "bravo charlie"}, {Kind: "CUSTOM", Regex: "charlie delta"}}
	s := newSession(t, privacy.NewEngine(), "overlap", cfg)
	if _, err := s.MaskJSON([]byte(`{"arguments":{"api_key":"alpha bravo"}}`)); err != nil {
		t.Fatal(err)
	}
	payload := `{"value":"alpha bravo charlie delta","safe":"` + strings.Repeat("ordinary ", 40) + `"}`
	quoted, _ := json.Marshal(payload)
	body := fmt.Appendf(nil, `{"input":[{"type":"function_call_output","output":%s}]}`, quoted)
	for range 2 { // the second pass reuses policy candidates from the cache
		masked, err := s.MaskJSON(body)
		if err != nil {
			t.Fatal(err)
		}
		inner := gjson.GetBytes(masked, "input.0.output").String()
		value := gjson.Get(inner, "value").String()
		if strings.Count(value, "{{CCLOAD_") != 2 || strings.Contains(value, "charlie") || strings.Contains(value, "delta") {
			t.Fatalf("eligible overlapping rule was lost: %q", value)
		}
		restored, err := s.RestoreJSON(masked)
		if err != nil || gjson.Get(gjson.GetBytes(restored, "input.0.output").String(), "value").String() != "alpha bravo charlie delta" {
			t.Fatalf("overlapping matches did not restore: %s %v", restored, err)
		}
	}
}

func BenchmarkToolResultJSONMask(b *testing.B) {
	var payload strings.Builder
	payload.WriteByte('[')
	for i := range 4000 {
		if i > 0 {
			payload.WriteByte(',')
		}
		fmt.Fprintf(&payload, `{"name":"record-%04d","description":"%s","status":"complete"}`, i, strings.Repeat("ordinary record ", 9))
	}
	payload.WriteByte(']')
	quoted, _ := json.Marshal(payload.String())
	body := fmt.Appendf(nil, `{"input":[{"type":"function_call_output","output":%s}]}`, quoted)
	cfg := privacy.DefaultConfig()
	cfg.Enabled = true
	policy, err := privacy.NewPolicy(cfg)
	if err != nil {
		b.Fatal(err)
	}
	for _, known := range []bool{false, true} {
		b.Run(fmt.Sprintf("known=%t", known), func(b *testing.B) {
			s := privacy.NewEngine().Session("tool-benchmark", policy)
			if known {
				if _, err := s.MaskText("password=PrivateValue01234567"); err != nil {
					b.Fatal(err)
				}
			}
			if _, err := s.MaskJSON(body); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := s.MaskJSON(body); err != nil {
					b.Fatal(err)
				}
			}
		})
	}

}

func TestRestoreJSONWithFieldsSkipsAlreadyRestoredValues(t *testing.T) {
	s := newSession(t, privacy.NewEngine(), "restored-fields", enabled()).ObserveRestoration()
	missing := "{{CCLOAD_SECRET_0123456789abcdef}}"
	body := []byte(`{"content":"` + missing + `","other":"` + missing + `"}`)
	index := gjson.GetBytes(body, "content").Index
	replacement := `already "restored" ` + missing
	restored, err := s.RestoreJSONWithFields(body, map[int]string{index: replacement})
	if err != nil || !json.Valid(restored) {
		t.Fatalf("RestoreJSONWithFields = %s, %v", restored, err)
	}
	if gjson.GetBytes(restored, "content").String() != replacement || gjson.GetBytes(restored, "other").String() != missing || s.UnrestoredTokens() != 1 {
		t.Fatalf("already restored value was processed twice: %s, missing %d", restored, s.UnrestoredTokens())
	}
	unchanged := []byte(`{ "content": "already\u0020restored" }`)
	index = gjson.GetBytes(unchanged, "content").Index
	if restored, err := s.RestoreJSONWithFields(unchanged, map[int]string{index: "already restored"}); err != nil || string(restored) != string(unchanged) {
		t.Fatalf("unchanged replacement reencoded the field: %s, %v", restored, err)
	}
	if _, err := s.RestoreJSONWithFields([]byte(`{"broken":`), map[int]string{0: "ignored"}); err == nil {
		t.Fatal("accepted invalid JSON")
	}
}

func TestToolResultRawStringMatchesRespectEscapesAndAllowList(t *testing.T) {
	for _, tc := range []struct{ name, wire, regex, value string }{
		{"unicode", `prefix\u0040suffix`, `u0040`, "@"},
		{"newline", `prefix\nsecret`, `nsecret`, "\nsecret"},
		{"surrogate pair", `prefix\ud83d\ude3csuffix`, `ud83d`, "😼"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := privacy.DefaultConfig()
			cfg.Enabled, cfg.Secrets = true, false
			cfg.Rules = []privacy.Rule{{Kind: "RAW", Regex: tc.regex}}
			s := newSession(t, privacy.NewEngine(), "raw-escapes", cfg)
			quoted, _ := json.Marshal(`{"value":"` + tc.wire + `"}`)
			body := fmt.Appendf(nil, `{"input":[{"type":"function_call_output","output":%s}]}`, quoted)
			masked, err := s.MaskJSON(body)
			if err != nil {
				t.Fatal(err)
			}
			inner := gjson.GetBytes(masked, "input.0.output").String()
			if !json.Valid([]byte(inner)) {
				t.Fatalf("raw match split a JSON escape: %s", inner)
			}
			value := gjson.Get(inner, "value").String()
			token := strings.TrimSuffix(strings.TrimPrefix(value, "prefix"), "suffix")
			if !strings.HasPrefix(token, "{{CCLOAD_RAW_") || s.RestoreText(token) != tc.value {
				t.Fatalf("raw string match did not register decoded value: %q", token)
			}
			cfg.Allow = []string{tc.value}
			// Allow entries cannot contain newlines; the ordinary escaped value
			// and surrogate cases exercise decoded allow-list handling.
			if tc.name != "newline" {
				allowed := newSession(t, privacy.NewEngine(), "allowed", cfg)
				if out, err := allowed.MaskJSON(body); err != nil || string(out) != string(body) {
					t.Fatalf("decoded allow-list value changed: %s %v", out, err)
				}
			}
		})
	}
}

func TestKnownPhoneRespectsIdentifierContext(t *testing.T) {
	s := newSession(t, privacy.NewEngine(), "phone", enabled())
	if _, err := s.MaskText("手机号 13800138000"); err != nil {
		t.Fatal(err)
	}
	out, err := s.MaskText("order_id=13800138000")
	if err != nil || out != "order_id=13800138000" {
		t.Fatalf("known phone overrode identifier context: %q %v", out, err)
	}
}

// gjson pairs an unpaired surrogate escape with the next \u escape, dropping
// that character; upstream decoders keep it.
func TestMessageTextDecodesUnpairedSurrogatesLikeEncodingJSON(t *testing.T) {
	for _, tc := range []struct{ name, wire, want string }{
		{"lone word", `x \ud800\u0073ecret`, "x �{{WORD}}"},
		{"with another hit", `password=hello123world \udc00\u0073ecret`, "password={{SECRET}} �{{WORD}}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := enabled()
			cfg.Words = []string{"secret"}
			s := newSession(t, privacy.NewEngine(), "surrogate", cfg)
			body := []byte(`{"messages":[{"role":"user","content":"` + tc.wire + `"}]}`)
			masked, err := s.MaskJSON(body)
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Messages []struct{ Content string } `json:"messages"`
			}
			if err := json.Unmarshal(masked, &got); err != nil {
				t.Fatal(err)
			}
			content := got.Messages[0].Content
			if shape := regexp.MustCompile(`\{\{CCLOAD_([A-Z]+)_[a-f0-9]{16}\}\}`).ReplaceAllString(content, "{{$1}}"); shape != tc.want {
				t.Fatalf("upstream content = %q, want %q", content, tc.want)
			}
			var original string
			if err := json.Unmarshal([]byte(`"`+tc.wire+`"`), &original); err != nil {
				t.Fatal(err)
			}
			if restored := s.RestoreText(content); restored != original {
				t.Fatalf("restored %q, want %q", restored, original)
			}
		})
	}
}

// A known value displaced by an overlapping detection must not hide a later
// known value that overlaps it.
func TestKnownValuesKeepOverlappingCandidates(t *testing.T) {
	s := newSession(t, privacy.NewEngine(), "overlap", enabled())
	learn := `{"messages":[{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{\"password\":\"alpha bravo\",\"token\":\"bravo charlie\"}"}}]}]}`
	if _, err := s.MaskJSON([]byte(learn)); err != nil {
		t.Fatal(err)
	}
	out, err := s.MaskText("password=Z9pQ7mV3-alpha bravo charlie")
	if err != nil || strings.Contains(out, "bravo") || strings.Contains(out, "charlie") {
		t.Fatalf("known value left in clear: %q %v", out, err)
	}
	if restored := s.RestoreText(out); restored != "password=Z9pQ7mV3-alpha bravo charlie" {
		t.Fatalf("restored %q", restored)
	}
}

func TestCustomRulesIndependentAndRemovedRulesDoNotRemask(t *testing.T) {
	e := privacy.NewEngine()
	cfg := privacy.DefaultConfig()
	cfg.Enabled, cfg.Secrets = true, false
	cfg.Rules = []privacy.Rule{{Kind: "RELAY", Prefix: "relay_"}}
	first := newSession(t, e, "scope", cfg)
	// Long enough for the detection cache, which must not outlive its policy.
	text := "relay_ABCDEF123456 " + strings.Repeat("filler ", 64)
	masked, err := first.MaskText(text)
	if err != nil || masked == text || !strings.Contains(masked, "{{CCLOAD_RELAY_") {
		t.Fatalf("custom rule depended on Secrets: %q %v", masked, err)
	}
	cfg.Rules = []privacy.Rule{{Kind: "OTHER", Prefix: "other_"}}
	reconfigured := newSession(t, e, "scope", cfg)
	out, err := reconfigured.MaskText(text)
	if err != nil || out != text {
		t.Fatalf("removed custom rule kept masking: %q %v", out, err)
	}
	if restored := reconfigured.RestoreText(masked); restored != text {
		t.Fatal("removing a custom rule destroyed the previous response mapping")
	}
}

/**
 * @brief 客户端可读签名思考，重放仅撤销展示过的替换且不依赖字典
 * @param t 测试上下文
 */
func TestSignedReasoningRoundTrip(t *testing.T) {
	cfg := enabled()
	cfg.Words = []string{"sample-value"}
	s := newSession(t, privacy.NewEngine(), "owner", cfg)
	token, err := s.MaskText("sample-value")
	if err != nil || token == "sample-value" {
		t.Fatalf("MaskText = %q, %v", token, err)
	}
	rawText := "前缀 sample-value / " + token + " / " + token
	visible := strings.ReplaceAll(rawText, token, "sample-value")
	for _, tc := range []struct{ kind, text, sig string }{
		{"thinking", "thinking", "signature"},
		{"reasoning.text", "text", "signature"},
		{"reasoning.summary", "summary", "signature"},
		{"", "text", "thoughtSignature"},
		{"", "text", "thought_signature"},
	} {
		t.Run(tc.kind+tc.sig, func(t *testing.T) {
			const vendorSig = "dmVuZG9y"
			block := map[string]any{tc.text: rawText, tc.sig: vendorSig}
			if tc.kind == "" {
				block["thought"] = true
			} else {
				block["type"] = tc.kind
			}
			wire, err := json.Marshal(block)
			if err != nil {
				t.Fatal(err)
			}
			// JSON escapes are valid even inside a placeholder or signature.
			escaped := strings.ReplaceAll(string(wire), "{{", `\u007b{`)
			local, err := s.RestoreJSON([]byte(escaped))
			if err != nil || gjson.GetBytes(local, tc.text).String() != visible {
				t.Fatalf("visible reasoning = %s, %v", local, err)
			}
			sig := gjson.GetBytes(local, tc.sig).String()
			if sig == vendorSig {
				t.Fatal("missing replay information")
			}
			if tc.kind == "" {
				// Vertex can return an array of complete Gemini response envelopes.
				response := fmt.Appendf(nil, `[{"candidates":[{"content":{"role":"model","parts":[%s]}}]}]`, escaped)
				restored, err := s.RestoreJSON(response)
				part := gjson.GetBytes(restored, "0.candidates.0.content.parts.0")
				if err != nil || part.Raw != string(local) {
					t.Fatalf("Gemini array did not preserve signed replay: %s, %v", restored, err)
				}
				var disabled *privacy.Session
				replayed, err := disabled.MaskJSON(fmt.Appendf(nil, `{"contents":[{"role":"model","parts":[%s]}]}`, part.Raw))
				part = gjson.GetBytes(replayed, "contents.0.parts.0")
				if err != nil || part.Get(tc.text).Str != rawText || part.Get(tc.sig).Str != vendorSig {
					t.Fatalf("Gemini array reasoning did not replay: %s, %v", replayed, err)
				}
			}
			local = []byte(strings.Replace(string(local), sig, fmt.Sprintf(`\u%04x%s`, sig[0], sig[1:]), 1))
			off := cfg
			off.Enabled = false
			for name, replayer := range map[string]*privacy.Session{
				"same": s, "restarted": newSession(t, privacy.NewEngine(), "owner", cfg),
				"disabled": newSession(t, privacy.NewEngine(), "owner", off), "absent": nil,
			} {
				out, err := replayer.MaskJSON(local)
				if err != nil || gjson.GetBytes(out, tc.text).String() != rawText || gjson.GetBytes(out, tc.sig).String() != vendorSig {
					t.Fatalf("%s replay = %s, %v", name, out, err)
				}
				out, err = replayer.MaskJSON(wire)
				if err != nil || string(out) != string(wire) {
					t.Fatalf("%s changed original signed block: %s, %v", name, out, err)
				}
			}
		})
	}
}

/**
 * @brief 验证 Claude 转 Chat 后从 text 展示和回放签名思考
 * @param t 测试上下文
 */
func TestSignedChatThinkingTextRoundTrip(t *testing.T) {
	cfg := enabled()
	cfg.Words = []string{"private thought sample"}
	session := newSession(t, privacy.NewEngine(), "chat", cfg)
	const text = "想 private thought sample"
	masked, err := session.MaskText(text)
	if err != nil || masked == text {
		t.Fatalf("MaskText did not replace the value: %v", err)
	}
	wire := fmt.Appendf(nil, `{"choices":[{"message":{"reasoning":[{"type":"thinking","text":%q,"signature":"cHJvdmlkZXI="}]}}]}`, masked)
	visible, err := session.RestoreJSON(wire)
	if err != nil {
		t.Fatal(err)
	}
	thinking := gjson.GetBytes(visible, "choices.0.message.reasoning.0")
	if thinking.Get("text").Str != text || thinking.Get("signature").Str == "cHJvdmlkZXI=" {
		t.Fatalf("visible Chat reasoning = %s", thinking.Raw)
	}
	request := []byte(`{"messages":[{"role":"assistant","reasoning":[` + thinking.Raw + `]}]}`)
	var disabled *privacy.Session
	replay, err := disabled.MaskJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	thinking = gjson.GetBytes(replay, "messages.0.reasoning.0")
	if thinking.Get("text").Str != masked || thinking.Get("signature").Str != "cHJvdmlkZXI=" {
		t.Fatalf("upstream Chat reasoning = %s", thinking.Raw)
	}
}

/**
 * @brief 在同一签名下还原多段 Responses 正文，回放不依赖对象键顺序或字典
 * @param t 测试上下文
 */
func TestSignedResponsesReasoningRoundTrip(t *testing.T) {
	const secret, vendor = "private thought sample", "cHJvdmlkZXI="
	cfg := enabled()
	cfg.Words = []string{secret}
	session := newSession(t, privacy.NewEngine(), "responses", cfg)
	token, err := session.MaskText(secret)
	if err != nil || token == secret {
		t.Fatalf("MaskText did not replace the value: %v", err)
	}
	first, second, summary := "想 "+secret+" / "+token, token+" 继续 "+token, "摘要 "+token
	wire := fmt.Appendf(nil, `{"output":[{"id":"r1","type":"reasoning","content":[{"type":"reasoning_text","text":%q},{"type":"reasoning_text","text":%q}],"signature":%q,"encrypted_content":%q,"summary":[{"type":"summary_text","text":%q},{"type":"summary_text","text":"unchanged"}],"note":%q}]}`, first, second, vendor, token, summary, token)
	visible, err := session.RestoreJSON(wire)
	if err != nil {
		t.Fatal(err)
	}
	item := gjson.GetBytes(visible, "output.0")
	fields := map[string]string{"content.0.text": "想 " + secret + " / " + secret,
		"content.1.text": secret + " 继续 " + secret, "summary.0.text": "摘要 " + secret,
		"summary.1.text": "unchanged", "encrypted_content": token, "note": secret}
	for path, want := range fields {
		if got := item.Get(path).Str; got != want {
			t.Fatalf("display %s = %q, want %q", path, got, want)
		}
	}
	sig := item.Get("signature").Str
	if sig == vendor {
		t.Fatal("signature did not carry replay information")
	}
	request := fmt.Appendf(nil, `{"input":[{"summary":[{"text":%q,"type":"summary_text"},{"text":"unchanged","type":"summary_text"}],"type":"reasoning","note":%q,"signature":%q,"content":[{"text":%q,"type":"reasoning_text"},{"text":%q,"type":"reasoning_text"}],"encrypted_content":%q,"id":"r1"}]}`,
		fields["summary.0.text"], secret, sig, fields["content.0.text"], fields["content.1.text"], token)
	restarted := newSession(t, privacy.NewEngine(), "responses", cfg)
	cfg.Enabled = false
	for name, replayer := range map[string]*privacy.Session{
		"restarted": restarted, "disabled": newSession(t, privacy.NewEngine(), "responses", cfg), "absent": nil,
	} {
		t.Run(name, func(t *testing.T) {
			replay, err := replayer.MaskJSON(request)
			if err != nil {
				t.Fatal(err)
			}
			item := gjson.GetBytes(replay, "input.0")
			fields := map[string]string{"content.0.text": first, "content.1.text": second,
				"summary.0.text": summary, "summary.1.text": "unchanged", "signature": vendor, "encrypted_content": token}
			for path, want := range fields {
				if got := item.Get(path).Str; got != want {
					t.Fatalf("replay %s = %q, want %q", path, got, want)
				}
			}
			if note := item.Get("note").Str; (note != secret) != (name == "restarted") {
				t.Fatalf("ordinary sibling did not follow privacy policy: %q", note)
			}
		})
	}
	missing := fmt.Appendf(nil, `{"input":[{"type":"reasoning","signature":%q,"content":[{"text":%q},{"text":%q}]}]}`, sig, fields["content.0.text"], fields["content.1.text"])
	if _, err := restarted.MaskJSON(missing); !errors.Is(err, privacy.ErrInvalidReasoningSignature) {
		t.Fatalf("replay without a required field = %v", err)
	}

	// The HTTP execution session stores the Responses output array by itself.
	history, err := session.RestoreJSON([]byte(gjson.GetBytes(wire, "output").Raw))
	if err != nil || gjson.GetBytes(history, "0").Raw != item.Raw {
		t.Fatalf("restored history array changed the reasoning item: %s, %v", history, err)
	}
	var disabled *privacy.Session
	replayed, err := disabled.MaskJSON(fmt.Appendf(nil, `{"input":%s}`, history))
	if got := gjson.GetBytes(replayed, "input.0"); err != nil || got.Get("signature").Str != vendor ||
		got.Get("content.0.text").Str != first || got.Get("content.1.text").Str != second || got.Get("summary.0.text").Str != summary {
		t.Fatalf("replayed history array changed signed reasoning: %s, %v", replayed, err)
	}
}

/**
 * @brief 加密推理的载体保持不透明，同对象的普通文字仍参与脱敏和还原
 * @param t 测试上下文
 */
func TestEncryptedReasoningPreservesCarrierAndScansSiblings(t *testing.T) {
	const secret = "private thought sample"
	cfg := enabled()
	cfg.Words = []string{secret}
	session := newSession(t, privacy.NewEngine(), "encrypted", cfg)
	token, err := session.MaskText(secret)
	if err != nil || token == secret {
		t.Fatalf("MaskText did not replace the value: %v", err)
	}
	request := fmt.Appendf(nil, `{"metadata":{"type":"reasoning.encrypted","text":%q,"signature":%q,"nested":{"value":%q},"content":[{"type":"thinking","text":%q,"signature":%q}]},"messages":[{"reasoning_details":[{"type":"reasoning.encrypted","data":%q,"encrypted_content":%q,"text":%q}]}]}`,
		secret, token, secret, secret, token, token, token, secret)
	masked, err := session.MaskJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"metadata.text", "metadata.nested.value", "metadata.signature", "metadata.content.0.text", "metadata.content.0.signature",
		"messages.0.reasoning_details.0.data", "messages.0.reasoning_details.0.encrypted_content", "messages.0.reasoning_details.0.text"} {
		if got := gjson.GetBytes(masked, path).Str; got != token {
			t.Fatalf("masked %s = %q, want %q", path, got, token)
		}
	}
	restored, err := session.RestoreJSON(masked)
	if err != nil || string(restored) != string(request) {
		t.Fatalf("restored encrypted reasoning = %s, %v", restored, err)
	}
}

/**
 * @brief 限制同一签名覆盖的替换数量及明文扩展，超限时拒绝生成封套
 * @param t 测试上下文
 */
func TestSignedReasoningRestoreRejectsCapacityOverflow(t *testing.T) {
	session := newSession(t, privacy.NewEngine(), "capacity", enabled())
	masked, err := session.MaskText("password=private-reasoning-sample")
	token := strings.TrimPrefix(masked, "password=")
	if err != nil || !strings.HasPrefix(token, "{{CCLOAD_") {
		t.Fatalf("MaskText did not produce a placeholder: %v", err)
	}
	first, second := strings.Repeat(token, privacy.MaxReasoningEdits/2), strings.Repeat(token, privacy.MaxReasoningEdits/2+1)
	tooMany := fmt.Appendf(nil, `{"type":"reasoning","signature":"abcd","content":[{"text":%q}],"summary":[{"text":%q}]}`, first, second)
	if _, err := session.RestoreJSON(tooMany); !errors.Is(err, privacy.ErrInvalidReasoningSignature) {
		t.Fatalf("restoring too many edits = %v", err)
	}
	const secretBytes = 64 << 10
	longSecret, err := session.MaskJSON(fmt.Appendf(nil, `{"arguments":{"password":%q}}`, strings.Repeat("q", secretBytes)))
	token = gjson.GetBytes(longSecret, "arguments.password").Str
	if err != nil || !strings.HasPrefix(token, "{{CCLOAD_") {
		t.Fatalf("MaskJSON did not produce a placeholder: %v", err)
	}
	tooLong := fmt.Appendf(nil, `{"type":"thinking","signature":"abcd","thinking":%q}`, strings.Repeat(token, (64<<20)/secretBytes+1))
	if _, err := session.RestoreJSON(tooLong); !errors.Is(err, privacy.ErrInvalidReasoningSignature) {
		t.Fatalf("restoring too much visible text = %v", err)
	}
	block := strings.Repeat(token, (32<<20)/secretBytes+1)
	tooLong = fmt.Appendf(nil, `{"content":[{"type":"thinking","signature":"abcd","thinking":%q},{"type":"thinking","signature":"abcd","thinking":%q}]}`, block, block)
	if _, err := session.RestoreJSON(tooLong); !errors.Is(err, privacy.ErrInvalidReasoningSignature) {
		t.Fatalf("restoring too much visible text across blocks = %v", err)
	}
}

func TestHasMappingsAndDisabledPassthrough(t *testing.T) {
	e := privacy.NewEngine()
	cfg := enabled()
	s := newSession(t, e, "scope", cfg)
	if e.HasMappings("scope") || e.HasMappings("other") {
		t.Fatal("new engine has mappings")
	}
	if _, err := s.MaskText("password=ExistingSecret123"); err != nil {
		t.Fatal(err)
	}
	if !e.HasMappings("scope") || e.HasMappings("other") {
		t.Fatal("mapping scope check failed")
	}
	cfg.Enabled = false
	disabled := newSession(t, e, "scope", cfg)
	if disabled.Enabled() || !s.Enabled() {
		t.Fatal("Enabled did not reflect the policy")
	}
	for _, body := range [][]byte{nil, []byte("not JSON"), []byte("[" + strings.Repeat("[", 150) + "0" + strings.Repeat("]", 151)), []byte(`{ "content": "password=AnotherSecret456" }`)} {
		out, err := disabled.MaskJSON(body)
		if err != nil || string(out) != string(body) {
			t.Fatal("disabled passthrough contract changed")
		}
	}
}

func TestMaskJSONRewritesOnlyChangedStrings(t *testing.T) {
	s := newSession(t, privacy.NewEngine(), "bytes", enabled())
	placeholder, err := s.MaskText("alice@company.com")
	if err != nil || placeholder == "alice@company.com" {
		t.Fatalf("email not masked: %q %v", placeholder, err)
	}
	body := `{ "z" : "mail alice@company.com", "a": 1e999, "m": {"b": 2.50, "a": "café \/ <ok>"} }`
	out, err := s.MaskJSON([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(body, "alice@company.com", placeholder, 1); string(out) != want {
		t.Fatalf("MaskJSON = %s, want %s", out, want)
	}
	if restored, err := s.RestoreJSON(out); err != nil || string(restored) != body {
		t.Fatalf("RestoreJSON = %s %v", restored, err)
	}
}

func TestDictionaryEvictsIdleValuesAndFailsClosedWhenFull(t *testing.T) {
	e := privacy.NewEngine()
	now := time.Unix(1_700_000_000, 0)
	privacy.SetClock(e, func() time.Time { return now })
	cfg := enabled()
	cfg.Rules = []privacy.Rule{{Kind: "CUSTOM", Prefix: "local_"}}
	s := newSession(t, e, "scope", cfg)
	old, err := s.MaskText("local_oldvalue1")
	if err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	for i := range 4095 {
		fmt.Fprintf(&body, "local_%08d ", i)
	}
	if _, err := s.MaskText(body.String()); err != nil {
		t.Fatal(err)
	}
	// Every mapping may still be needed by an in-flight reply.
	if out, err := s.MaskText("local_newvalue1"); err == nil || out != "" {
		t.Fatalf("full dictionary returned %q %v", out, err)
	}
	now = now.Add(90 * time.Minute)
	if _, err := s.MaskText(body.String()); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.MaskText("local_newvalue2")
	if err != nil || !strings.HasPrefix(fresh, "{{CCLOAD_CUSTOM_") {
		t.Fatalf("idle value was not evicted: %q %v", fresh, err)
	}
	if s.RestoreText(old) != old || s.RestoreText(fresh) != "local_newvalue2" {
		t.Fatal("eviction kept the idle value or lost the new one")
	}
	now = now.Add(25 * time.Hour)
	if e.HasMappings("scope") {
		t.Fatal("expired mappings still reported")
	}
}

func TestAnyCaseWordsFollowUnicodeFolding(t *testing.T) {
	cfg := enabled()
	cfg.Words = []string{"Acme", "kelvin", "Café"}
	cfg.WordsAnyCase = true
	s := newSession(t, privacy.NewEngine(), "words", cfg)
	text := "ACME acme Kelvin KELVIN CAFÉ café" // the first "Kelvin" starts with U+212A KELVIN SIGN
	out, err := s.MaskText(text)
	if err != nil || strings.Count(out, "{{CCLOAD_WORD_") != 6 {
		t.Fatalf("any-case words = %q %v", out, err)
	}
	if s.RestoreText(out) != text {
		t.Fatal("word round trip failed")
	}
}

// Rules only search text holding one of their required literals, and
// line-bound rules only those lines. These cases sit where that shortcut could
// miss a match or find an extra one.
func TestRulesMatchOnLaterLinesFoldedLabelsAndAnchors(t *testing.T) {
	cfg := enabled()
	cfg.Rules = []privacy.Rule{{Kind: "START", Regex: `^start\d+`}, {Kind: "BLOCK", Regex: `(?s)BEGIN.+?END`}}
	filler := strings.Repeat("plain line without labels\n", 50)
	cases := []struct {
		name, text   string
		masked, kept []string
	}{
		{"later lines", filler + "api_key = AbC123dEf456GhI789\nnext\nmail bob@mycompany.com\n" + filler + "密码：hunter2x",
			[]string{"AbC123dEf456GhI789", "bob@mycompany.com", "hunter2x"}, []string{filler + "api_key = ", "\nnext\nmail "}},
		{"long s label", "pa\u017F\u017Fword = Hunter2Hunter2", []string{"Hunter2Hunter2"}, nil},
		{"kelvin label", "API_\u212AEY = AbC123dEf456GhI789", []string{"AbC123dEf456GhI789"}, nil},
		{"text start anchor", "start1 start2\nstart3", []string{"start1"}, []string{"start2", "start3"}},
		{"multiline custom rule", "x BEGIN\nmiddle\nEND y", []string{"BEGIN\nmiddle\nEND"}, []string{"x ", " y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSession(t, privacy.NewEngine(), "rules", cfg)
			out, err := s.MaskText(tc.text)
			if err != nil {
				t.Fatal(err)
			}
			for _, value := range tc.masked {
				if strings.Contains(out, value) {
					t.Errorf("did not mask %q: %q", value, out)
				}
			}
			for _, value := range tc.kept {
				if !strings.Contains(out, value) {
					t.Errorf("changed %q: %q", value, out)
				}
			}
			if restored := s.RestoreText(out); restored != tc.text {
				t.Fatalf("restored %q, want %q", restored, tc.text)
			}
		})
	}
}

func TestToolPayloadProtocolNamedFieldsAreUserData(t *testing.T) {
	const secret = "sk-proj-ABCDEFabcdef012345678901234567890"
	payload := map[string]any{}
	for _, key := range []string{"signature", "encrypted_content", "thoughtSignature", "thought_signature", "name", "model", "id", "user_id", "image_url", "input_audio", "url"} {
		payload[key] = secret
	}
	payload["source"] = map[string]string{"type": "base64", "data": secret}
	payload["audio"] = map[string]string{"data": secret}
	payload["binary"] = "data:image/png;base64," + secret
	body := map[string]any{
		"signature": secret, "encrypted_content": secret, "name": secret, "model": secret, "id": secret,
		"image_url": map[string]string{"url": secret},
		"source":    map[string]string{"type": "base64", "data": secret},
		"content":   []any{map[string]any{"type": "tool_use", "name": "read", "id": "tool-1", "input": payload}},
		"input":     []any{map[string]any{"type": "function_call_output", "call_id": "tool-1", "output": payload}},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	s := newSession(t, privacy.NewEngine(), "payload", enabled())
	masked, err := s.MaskJSON(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(masked, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"signature", "encrypted_content", "name", "model", "id"} {
		if doc[key] != secret {
			t.Errorf("outer protocol field %s changed", key)
		}
	}
	if doc["image_url"].(map[string]any)["url"] != secret || doc["source"].(map[string]any)["data"] != secret {
		t.Fatal("outer binary/image protocol fields changed")
	}
	toolInput := doc["content"].([]any)[0].(map[string]any)["input"].(map[string]any)
	toolOutput := doc["input"].([]any)[0].(map[string]any)["output"].(map[string]any)
	for _, data := range []map[string]any{toolInput, toolOutput} {
		for key := range payload {
			if key == "binary" {
				continue
			}
			value, err := json.Marshal(data[key])
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(value), secret) {
				t.Errorf("tool data field %s escaped masking: %s", key, value)
			}
		}
		if data["binary"] != "data:image/png;base64,"+secret {
			t.Fatal("data URI changed inside tool payload")
		}
	}
	restored, err := s.RestoreJSON(masked)
	if err != nil || string(restored) != string(encoded) {
		t.Fatalf("tool payload did not restore: %s %v", restored, err)
	}
}

func TestPreviewForcesEnabledAndListsDistinctHits(t *testing.T) {
	cfg := privacy.DefaultConfig() // Enabled is false; Preview must still mask.
	cfg.Secrets = false
	cfg.Words = []string{"Project Falcon"}
	cfg.Rules = []privacy.Rule{{Kind: "RELAY", Prefix: "relay_"}}
	cfg.Allow = []string{"relay_ALLOWED123"}
	literal := "{{CCLOAD_RELAY_0123456789abcdef}}"
	text := "relay_ABCDEF123456 Project Falcon relay_ABCDEF123456 relay_ALLOWED123 " + literal
	result, err := privacy.Preview(cfg, text)
	if err != nil {
		t.Fatal(err)
	}
	masked, hits := result.Masked, result.Hits
	if len(hits) != 2 || hits[0].Kind != "RELAY" || hits[0].Value != "relay_ABCDEF123456" || hits[1].Kind != "WORD" || hits[1].Value != "Project Falcon" {
		t.Fatalf("unexpected hits: %+v", hits)
	}
	want := hits[0].Placeholder + " " + hits[1].Placeholder + " " + hits[0].Placeholder + " relay_ALLOWED123 " + literal
	if masked != want {
		t.Fatalf("masked = %q, want %q", masked, want)
	}

	// Each preview uses its own engine key, so placeholders never repeat.
	if again, _ := privacy.Preview(cfg, text); again.Masked == masked {
		t.Fatal("preview reused an engine")
	}

	clean, err := privacy.Preview(cfg, "nothing sensitive")
	if err != nil || clean.Masked != "nothing sensitive" || len(clean.Hits) != 0 {
		t.Fatalf("clean preview: %+v %v", clean, err)
	}
	if _, err := privacy.Preview(privacy.Config{Rules: []privacy.Rule{{Kind: "bad"}}}, "x"); err == nil {
		t.Fatal("invalid config previewed")
	}
}

func TestCountSentGroupsDistinctSentPlaceholders(t *testing.T) {
	e := privacy.NewEngine()
	cfg := enabled()
	cfg.Words = []string{"Orion"}
	cfg.Rules = []privacy.Rule{{Kind: "EMAIL", Regex: "ticket#[a-z]+"}}
	s := newSession(t, e, "scope", cfg)
	masked, err := s.MaskText("sk-proj-ABcDef012345678901234567890123456 alice@company.com 00:1A:2B:3C:4D:5E Orion ticket#alpha")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := newSession(t, e, "other", cfg).MaskText("alice@mycompany.com")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(masked + masked + foreign + "{{CCLOAD_EMAIL_0123456789abcdef}}")
	want := privacy.Counts{Secret: 1, Personal: 2, Word: 1, Custom: 1}
	if got := s.CountSent(body); got != want || got.Total() != 5 {
		t.Fatalf("CountSent = %+v total %d, want %+v; masked %q", got, got.Total(), want, masked)
	}
	if got := s.CountSent([]byte(masked)); got != want {
		t.Fatalf("repeated CountSent = %+v, want %+v", got, want)
	}
	if got := s.SentCounts(); got != want {
		t.Fatalf("SentCounts after repeat = %+v, want %+v", got, want)
	}
	extra, err := s.MaskText("alice@mycompany.com")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.CountSent([]byte(extra)); got != (privacy.Counts{Personal: 1}) {
		t.Fatalf("CountSent(extra) = %+v", got)
	}
	if got, want := s.SentCounts(), (privacy.Counts{Secret: 1, Personal: 3, Word: 1, Custom: 1}); got != want {
		t.Fatalf("SentCounts union = %+v, want %+v", got, want)
	}

	cfg.Enabled = false
	disabled := newSession(t, e, "scope", cfg)
	if got := disabled.CountSent(body); got != (privacy.Counts{}) || disabled.SentCounts() != (privacy.Counts{}) {
		t.Fatalf("disabled session counted %+v / %+v", got, disabled.SentCounts())
	}
	var nilSession *privacy.Session
	if got := nilSession.CountSent(body); got != (privacy.Counts{}) || nilSession.SentCounts() != (privacy.Counts{}) {
		t.Fatalf("nil session counted %+v", got)
	}
}

func TestRestoredTokensCountsObservedOccurrences(t *testing.T) {
	s := newSession(t, privacy.NewEngine(), "scope", enabled())
	p, err := s.MaskText("alice@company.com")
	if err != nil {
		t.Fatal(err)
	}
	observer := s.ObserveRestoration()
	observer.RestoreText(p + " " + p + " {{CCLOAD_broken")
	if observer.RestoredTokens() != 2 || observer.UnrestoredTokens() != 1 {
		t.Fatalf("restored %d unrestored %d, want 2 and 1", observer.RestoredTokens(), observer.UnrestoredTokens())
	}
	s.RestoreText(p)
	if s.RestoredTokens() != 0 {
		t.Fatalf("internal restore counted %d", s.RestoredTokens())
	}
}

func TestEngineStatsReportsLiveDictionary(t *testing.T) {
	e := privacy.NewEngine()
	now := time.Now()
	privacy.SetClock(e, func() time.Time { return now })
	if scopes, entries, size := e.Stats(); scopes != 0 || entries != 0 || size != 0 {
		t.Fatalf("new engine Stats = %d %d %d", scopes, entries, size)
	}
	for _, scope := range []string{"a", "b"} {
		if _, err := newSession(t, e, scope, enabled()).MaskText("alice@company.com"); err != nil {
			t.Fatal(err)
		}
	}
	if scopes, entries, size := e.Stats(); scopes != 2 || entries != 2 || size <= 0 {
		t.Fatalf("Stats = %d %d %d, want 2 2 >0", scopes, entries, size)
	}
	privacy.SetClock(e, func() time.Time { return now.Add(25 * time.Hour) })
	if scopes, entries, size := e.Stats(); scopes != 0 || entries != 0 || size != 0 {
		t.Fatalf("expired Stats = %d %d %d", scopes, entries, size)
	}
}

/**
 * @brief 按字节还原签名文本，并保留厂商分片拼接与标准 base64 往返
 * @param t 测试上下文
 */
func TestReasoningSignatureReplay(t *testing.T) {
	const (
		vendor  = "Y2xhdWRlIHNpZ25hdHVyZQ=="
		first   = "{{CCLOAD_WORD_0123456789abcdef}}"
		second  = "{{CCLOAD_PASSWORD_fedcba9876543210}}"
		secret  = "凭据-abc"
		visible = "想了 " + secret + " 再看 z。"
	)
	edits := []privacy.ReasoningEdit{
		{Offset: len("想了 "), Length: len(secret), Placeholder: first},
		{Offset: len("想了 " + secret + " 再看 "), Length: 1, Placeholder: second},
	}
	wrapped, err := privacy.WrapReasoningSignature(vendor[:7], edits)
	if err != nil {
		t.Fatal(err)
	}
	wrapped += vendor[7:]
	decoded, err := base64.StdEncoding.DecodeString(wrapped)
	if err != nil || strings.Contains(string(decoded), secret) {
		t.Fatalf("invalid base64 or visible secret in capsule: %v", err)
	}
	gotSignature, gotText, err := privacy.UnwrapReasoningSignature(base64.StdEncoding.EncodeToString(decoded), visible)
	if want := "想了 " + first + " 再看 " + second + "。"; err != nil || gotSignature != vendor || gotText != want {
		t.Fatalf("unexpected replay: signature=%q, text=%q, err=%v", gotSignature, gotText, err)
	}
	unchanged, err := privacy.WrapReasoningSignature(vendor, nil)
	if err != nil || unchanged != vendor {
		t.Fatalf("signature without edits changed: %q, %v", unchanged, err)
	}
	gotSignature, gotText, err = privacy.UnwrapReasoningSignature(vendor, visible)
	if err != nil || gotSignature != vendor || gotText != visible {
		t.Fatalf("native signed block changed: signature=%q, text=%q, err=%v", gotSignature, gotText, err)
	}
}

/**
 * @brief 跨签名块的占位符按原始分片重放，包括可见正文为空的续片
 * @param t 测试上下文
 */
func TestReasoningSignatureReplaysPartialPlaceholders(t *testing.T) {
	const token = "{{CCLOAD_WORD_0123456789abcdef}}"
	for _, part := range []struct {
		signature string
		visible   string
		text      string
		edit      privacy.ReasoningEdit
	}{
		{"first", "想凭据", "想" + token[:9], privacy.ReasoningEdit{Offset: len("想"), Length: len("凭据"), Placeholder: token, TokenEnd: 9}},
		{"middle", "", token[9:21], privacy.ReasoningEdit{Placeholder: token, TokenStart: 9, TokenEnd: 21}},
		{"last", "结束", token[21:] + "结束", privacy.ReasoningEdit{Placeholder: token, TokenStart: 21, TokenEnd: len(token)}},
		{"suffix", "先", "先" + token[21:], privacy.ReasoningEdit{Offset: len("先"), Placeholder: token, TokenStart: 21, TokenEnd: len(token)}},
	} {
		wrapped, err := privacy.WrapReasoningSignature(part.signature, []privacy.ReasoningEdit{part.edit})
		if err != nil {
			t.Fatal(err)
		}
		signature, text, err := privacy.UnwrapReasoningSignature(wrapped, part.visible)
		if err != nil || signature != part.signature || text != part.text {
			t.Fatalf("unexpected partial replay: signature=%q, text=%q, err=%v", signature, text, err)
		}
	}
}

/**
 * @brief 封套损坏、包含越界编辑或试图替换进普通字符串时拒绝重放
 * @param t 测试上下文
 */
func TestReasoningSignatureRejectsInvalidReplay(t *testing.T) {
	const (
		placeholder = "{{CCLOAD_WORD_0123456789abcdef}}"
		token       = `"` + placeholder + `"`
	)
	wrapPayload := func(payload, signature string) string {
		for len(payload)%3 != 0 {
			payload += " "
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(payload))
		return fmt.Sprintf("%s%08x%s%s", privacy.ReasoningSignaturePrefix, len(encoded), encoded, signature)
	}
	for name, signature := range map[string]string{
		"truncated length":       privacy.ReasoningSignaturePrefix + "001",
		"overlong patch":         privacy.ReasoningSignaturePrefix + "fffffffcAAAAabcd",
		"invalid base64":         privacy.ReasoningSignaturePrefix + "00000004....abcd",
		"missing vendor":         wrapPayload(`[{"offset":0,"length":3,"placeholder":`+token+`}]`, ""),
		"trailing data":          wrapPayload(`[{"offset":0,"length":3,"placeholder":`+token+`}][]`, "abcd"),
		"unknown field":          wrapPayload(`[{"offset":0,"length":3,"placeholder":`+token+`,"text":"extra"}]`, "abcd"),
		"no edits":               wrapPayload(`[]`, "abcd"),
		"out of range":           wrapPayload(`[{"offset":0,"length":5,"placeholder":`+token+`}]`, "abcd"),
		"negative":               wrapPayload(`[{"offset":-1,"length":1,"placeholder":`+token+`}]`, "abcd"),
		"token prefix":           wrapPayload(`[{"offset":0,"length":3,"placeholder":"x{{CCLOAD_WORD_0123456789abcdef}}"}]`, "abcd"),
		"overlap":                wrapPayload(`[{"offset":0,"length":4,"placeholder":`+token+`},{"offset":3,"length":1,"placeholder":`+token+`}]`, "abcd"),
		"split rune":             wrapPayload(`[{"offset":1,"length":3,"placeholder":`+token+`}]`, "abcd"),
		"insert within rune":     wrapPayload(`[{"offset":1,"length":0,"placeholder":`+token+`,"token_end":2}]`, "abcd"),
		"insert full token":      wrapPayload(`[{"offset":4,"length":0,"placeholder":`+token+`}]`, "abcd"),
		"explicit full token":    wrapPayload(fmt.Sprintf(`[{"offset":4,"length":0,"placeholder":%s,"token_end":%d}]`, token, len(placeholder)), "abcd"),
		"negative token start":   wrapPayload(`[{"offset":0,"length":3,"placeholder":`+token+`,"token_start":-1,"token_end":2}]`, "abcd"),
		"empty token segment":    wrapPayload(`[{"offset":0,"length":3,"placeholder":`+token+`,"token_start":5,"token_end":5}]`, "abcd"),
		"token end out of range": wrapPayload(fmt.Sprintf(`[{"offset":0,"length":3,"placeholder":%s,"token_end":%d}]`, token, len(placeholder)+1), "abcd"),
	} {
		t.Run(name, func(t *testing.T) {
			gotSignature, gotText, err := privacy.UnwrapReasoningSignature(signature, "想a")
			if !errors.Is(err, privacy.ErrInvalidReasoningSignature) || gotSignature != "" || gotText != "" {
				t.Fatalf("malformed replay accepted: signature=%q, text=%q, err=%v", gotSignature, gotText, err)
			}
		})
	}
	if _, err := privacy.WrapReasoningSignature("abcd", []privacy.ReasoningEdit{{
		Offset: (64 << 20) - 1, Length: 1, Placeholder: "{{CCLOAD_WORD_0123456789abcdef}}",
	}}); !errors.Is(err, privacy.ErrInvalidReasoningSignature) {
		t.Fatalf("oversized reconstructed text accepted: %v", err)
	}
	if _, err := privacy.WrapReasoningSignature("abcd", []privacy.ReasoningEdit{{
		Offset: (64 << 20) - 1, Placeholder: "{{CCLOAD_WORD_0123456789abcdef}}", TokenEnd: 1,
	}}); err != nil {
		t.Fatalf("partial replay within text limit rejected: %v", err)
	}
}
