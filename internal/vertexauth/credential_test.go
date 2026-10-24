package vertexauth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	testKeyOnce sync.Once
	testKey     *rsa.PrivateKey
)

func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	testKeyOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		testKey = key
	})
	return testKey
}

// testPKCS8PEM matches the "PRIVATE KEY" block Google issues in key files.
func testPKCS8PEM(t *testing.T) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(testRSAKey(t))
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func testServiceAccount(t *testing.T, tokenURI string) map[string]any {
	t.Helper()
	account := map[string]any{
		"type":           "service_account",
		"project_id":     "demo-project",
		"private_key_id": "key-1",
		"private_key":    testPKCS8PEM(t),
		"client_email":   "runner@demo-project.iam.gserviceaccount.com",
		"client_id":      "123",
	}
	if tokenURI != "" {
		account["token_uri"] = tokenURI
	}
	return account
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParseCredentialAcceptsRawServiceAccountKey(t *testing.T) {
	credential, err := ParseCredential(mustJSON(t, testServiceAccount(t, "")))
	if err != nil {
		t.Fatalf("ParseCredential: %v", err)
	}
	if credential.Type != ChannelType || credential.ProjectID != "demo-project" ||
		credential.Email != "runner@demo-project.iam.gserviceaccount.com" || credential.Location != DefaultLocation {
		t.Fatalf("unexpected credential: %+v", credential)
	}
	privateKey, _ := credential.ServiceAccount["private_key"].(string)
	if !strings.HasPrefix(privateKey, "-----BEGIN RSA PRIVATE KEY-----") {
		t.Fatalf("private key not normalized to PKCS#1: %q", privateKey[:40])
	}
	if credential.ServiceAccount["client_id"] != "123" {
		t.Fatalf("service account fields must be preserved: %+v", credential.ServiceAccount)
	}
}

func TestParseCredentialRepairsMangledKeyInCLIProxyAuthFile(t *testing.T) {
	account := testServiceAccount(t, "")
	// Terminal paste damage: CRLF, ANSI color codes, and a body without line breaks.
	body := strings.NewReplacer("-----BEGIN PRIVATE KEY-----", "", "-----END PRIVATE KEY-----", "", "\n", "").
		Replace(testPKCS8PEM(t))
	account["private_key"] = "\x1b[32m-----BEGIN PRIVATE KEY-----\r\n" + body + "\r\n-----END PRIVATE KEY-----\x1b[0m\r\n"
	raw := mustJSON(t, map[string]any{
		"type": "vertex", "service_account": account, "project_id": "override-project",
		"email": "ignored@example.com", "location": "europe-west4",
	})

	credential, err := ParseCredential(raw)
	if err != nil {
		t.Fatalf("ParseCredential: %v", err)
	}
	if credential.ProjectID != "override-project" || credential.Location != "europe-west4" {
		t.Fatalf("auth file routing fields lost: %+v", credential)
	}
	if credential.Email != "runner@demo-project.iam.gserviceaccount.com" {
		t.Fatalf("email must come from the service account identity, got %q", credential.Email)
	}
	stored, err := credential.JSON()
	if err != nil {
		t.Fatal(err)
	}
	reparsed, err := ParseCredential([]byte(stored))
	if err != nil {
		t.Fatalf("stored credential must round-trip: %v", err)
	}
	if !reparsed.SameAccount(credential) || reparsed.Location != credential.Location {
		t.Fatalf("round trip changed credential: %+v", reparsed)
	}
}

func TestParseCredentialRejectsInvalidInput(t *testing.T) {
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecDER, err := x509.MarshalPKCS8PrivateKey(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(map[string]any) any{
		"location injection": func(account map[string]any) any {
			return map[string]any{"type": "vertex", "service_account": account, "location": "us-central1.evil.com/x"}
		},
		"project path injection": func(account map[string]any) any {
			account["project_id"] = "demo/../other"
			return account
		},
		"missing client_email": func(account map[string]any) any {
			delete(account, "client_email")
			return account
		},
		"non-RSA key": func(account map[string]any) any {
			account["private_key"] = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecDER}))
			return account
		},
		"other credential type": func(account map[string]any) any {
			account["type"] = "authorized_user"
			return account
		},
		"auth file without service_account": func(map[string]any) any {
			return map[string]any{"type": "vertex", "project_id": "demo-project"}
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCredential(mustJSON(t, build(testServiceAccount(t, "")))); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestPublisherBaseURL(t *testing.T) {
	if got := PublisherBaseURL("p1", "us-central1"); got != "https://us-central1-aiplatform.googleapis.com/v1/projects/p1/locations/us-central1/publishers/google" {
		t.Fatalf("regional base URL = %q", got)
	}
	if got := PublisherBaseURL("p1", GlobalLocation); got != "https://aiplatform.googleapis.com/v1/projects/p1/locations/global/publishers/google" {
		t.Fatalf("global base URL = %q", got)
	}
	if got := PublisherBaseURL("p1", "eu"); got != "https://aiplatform.eu.rep.googleapis.com/v1/projects/p1/locations/eu/publishers/google" {
		t.Fatalf("multi-region base URL = %q", got)
	}
}

func TestMintAccessTokenUsesSignedJWTBearerGrant(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		if got := r.PostForm.Get("grant_type"); got != jwtBearerGrantType {
			t.Errorf("grant_type = %q", got)
		}
		segments := strings.Split(r.PostForm.Get("assertion"), ".")
		if len(segments) != 3 {
			t.Fatalf("assertion is not a JWS: %q", r.PostForm.Get("assertion"))
		}
		var header, claims map[string]any
		decodeSegment(t, segments[0], &header)
		decodeSegment(t, segments[1], &claims)
		if header["alg"] != "RS256" || header["kid"] != "key-1" {
			t.Errorf("unexpected header: %v", header)
		}
		if claims["iss"] != "runner@demo-project.iam.gserviceaccount.com" || claims["aud"] != server.URL+"/token" ||
			claims["scope"] != CloudPlatformScope {
			t.Errorf("unexpected claims: %v", claims)
		}
		if exp, iat := claims["exp"].(float64), claims["iat"].(float64); exp-iat != assertionLifetime.Seconds() {
			t.Errorf("assertion lifetime = %v", exp-iat)
		}
		signature, err := base64.RawURLEncoding.DecodeString(segments[2])
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256([]byte(segments[0] + "." + segments[1]))
		if err := rsa.VerifyPKCS1v15(&testRSAKey(t).PublicKey, crypto.SHA256, digest[:], signature); err != nil {
			t.Errorf("assertion signature invalid: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"ya29.token","expires_in":3599,"token_type":"Bearer"}`))
	}))
	defer server.Close()

	credential, err := ParseCredential(mustJSON(t, testServiceAccount(t, server.URL+"/token")))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	token, err := credential.MintAccessToken(context.Background(), server.Client(), now)
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}
	if token.AccessToken != "ya29.token" || !token.ExpiresAt.Equal(now.Add(3599*time.Second)) {
		t.Fatalf("unexpected token: %+v", token)
	}
}

func TestMintAccessTokenReportsTokenEndpointRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"Invalid JWT Signature."}`))
	}))
	defer server.Close()

	credential, err := ParseCredential(mustJSON(t, testServiceAccount(t, server.URL)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = credential.MintAccessToken(context.Background(), server.Client(), time.Now())
	if err == nil || !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("expected invalid_grant error, got %v", err)
	}
	var endpointFailure interface {
		error
		StatusCode() int
		UpstreamResponseBody() string
	}
	if !errors.As(err, &endpointFailure) || endpointFailure.StatusCode() != http.StatusBadRequest {
		t.Fatalf("token endpoint status was not preserved: %v", err)
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(endpointFailure.UpstreamResponseBody()), &payload); err != nil || payload.Error != "invalid_grant" {
		t.Fatalf("token endpoint response was not preserved: %+v, %v", payload, err)
	}
}

func decodeSegment(t *testing.T, segment string, target any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, target); err != nil {
		t.Fatal(err)
	}
}
