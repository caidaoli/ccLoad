package privacy_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"ccLoad/internal/privacy"
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
		{"ark", "ark-ABcDef012345678901234567890123456", "ark-"},
		{"github", "ghp_" + strings.Repeat("A9", 20), "ghp_"},
		{"aws", "AKIAIOSFODNN7EXAMPLE", "AKIA"},
		{"jwt", "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcDEF0123456789", "eyJ"},
		{"url", "postgres://alice:s3cr3tPass@db.local/db", "s3cr3tPass"},
		{"assignment", "API_KEY=AbC123dEf456GhI789", "AbC123"},
		{"header", "Authorization: Bearer opaque-token-value", "opaque-token-value"},
		{"cookie", "Cookie: session=opaquevalue123", "opaquevalue123"},
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
		"max_tokens=max_tokens_v3", "令牌是 300000 毫秒", "token.go:L643-669", `"jsonwebtoken": "9.2.0-beta1"`,
		"订单号 13800138000", "order_id=13800138000", "9f13800138000abc",
		"123-45-6789", "999-00-0000", "4111111111111112", "110105194912310021",
		"DOB: 1990-02-30", "release 1990-05-12", "00:00:00:00:00:00", "aa:bb:cc:dd:ee:ff",
		"DE89370400440532013001", "user@package.notarealtld", "sk-this-is-a-public-documentation-example",
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
	for i := 0; i < 16; i++ {
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
	for i := 0; i < 4097; i++ {
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

func TestSignedReasoningPassesThroughUnchanged(t *testing.T) {
	for _, protocol := range []string{"anthropic", "gemini", "gemini-snake"} {
		t.Run(protocol, func(t *testing.T) {
			e := privacy.NewEngine()
			s := newSession(t, e, "owner", enabled())
			secret := "S3cr3tSignedValue123"
			masked, err := s.MaskText("password=" + secret)
			if err != nil {
				t.Fatal(err)
			}
			token := strings.TrimPrefix(masked, "password=")
			reasoning := "remember " + token + " and 我的密码是 FreshSecret987"
			var wire []byte
			if protocol == "anthropic" {
				wire, err = json.Marshal(map[string]any{"content": []any{
					map[string]any{"type": "thinking", "thinking": reasoning, "signature": "vendor-signature"},
					map[string]any{"type": "text", "text": token},
				}})
			} else {
				key := "thoughtSignature"
				if protocol == "gemini-snake" {
					key = "thought_signature"
				}
				wire, err = json.Marshal(map[string]any{"contents": []any{map[string]any{"role": "model", "parts": []any{
					map[string]any{"text": reasoning, "thought": true, key: "vendor-signature"},
					map[string]any{"text": token},
				}}}})
			}
			if err != nil {
				t.Fatal(err)
			}
			// The vendor signed reasoning it produced from masked input. The
			// client must replay exactly that, so the reasoning keeps its
			// placeholders while the answer next to it is restored.
			local, err := s.RestoreJSON(wire)
			if err != nil {
				t.Fatal(err)
			}
			want := strings.Replace(string(wire), `"text":"`+token+`"`, `"text":"`+secret+`"`, 1)
			if string(local) != want {
				t.Fatalf("restore = %s, want %s", local, want)
			}
			// Replays reach the vendor byte-for-byte, even after a restart lost
			// the dictionary or masking was switched off.
			cfg := enabled()
			cfg.Enabled = false
			for name, replayer := range map[string]*privacy.Session{
				"same":      s,
				"restarted": newSession(t, privacy.NewEngine(), "owner", enabled()),
				"disabled":  newSession(t, e, "owner", cfg),
			} {
				out, err := replayer.MaskJSON(wire)
				if err != nil || string(out) != string(wire) {
					t.Fatalf("%s replay changed signed reasoning: %s %v", name, out, err)
				}
			}
		})
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
	for i := 0; i < 4095; i++ {
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
	masked, hits, err := privacy.Preview(cfg, text)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Kind != "RELAY" || hits[0].Value != "relay_ABCDEF123456" || hits[1].Kind != "WORD" || hits[1].Value != "Project Falcon" {
		t.Fatalf("unexpected hits: %+v", hits)
	}
	want := hits[0].Placeholder + " " + hits[1].Placeholder + " " + hits[0].Placeholder + " relay_ALLOWED123 " + literal
	if masked != want {
		t.Fatalf("masked = %q, want %q", masked, want)
	}

	// Each preview uses its own engine key, so placeholders never repeat.
	if again, _, _ := privacy.Preview(cfg, text); again == masked {
		t.Fatal("preview reused an engine")
	}

	clean, none, err := privacy.Preview(cfg, "nothing sensitive")
	if err != nil || clean != "nothing sensitive" || len(none) != 0 {
		t.Fatalf("clean preview: %q %+v %v", clean, none, err)
	}
	if _, _, err := privacy.Preview(privacy.Config{Rules: []privacy.Rule{{Kind: "bad"}}}, "x"); err == nil {
		t.Fatal("invalid config previewed")
	}
}
