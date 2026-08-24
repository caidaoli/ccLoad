package zedauth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"testing"
)

func TestLoginDecryptsNativeCallback(t *testing.T) {
	login, err := NewLogin()
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := base64.RawURLEncoding.DecodeString(login.publicKey)
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err := x509.ParsePKCS1PublicKey(publicDER)
	if err != nil {
		t.Fatal(err)
	}
	// The native credential is opaque to the client. Zed may change its internal
	// fields; only the complete JSON object is part of the authorization contract.
	native := []byte(`{"github_user_id":123,"github_user_login":"octocat","credential_kind":"native"}`)
	ciphertext, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, publicKey, native, nil)
	if err != nil {
		t.Fatal(err)
	}
	query := url.Values{}
	query.Set("user_id", "user-42")
	query.Set("access_token", base64.RawURLEncoding.EncodeToString(ciphertext))
	credential, err := login.ParseCallbackURL("http://localhost:43123/?"+query.Encode(), "11111111-2222-4333-8444-555555555555")
	if err != nil {
		t.Fatal(err)
	}
	if credential.UserID != "user-42" || credential.GitHubUserID != "123" || credential.GitHubUserLogin != "octocat" {
		t.Fatalf("unexpected identity: %+v", credential)
	}
	if string(credential.NativeCredential) != string(native) {
		t.Fatalf("native credential changed: %s", credential.NativeCredential)
	}
	var persisted map[string]any
	payload, err := credential.JSON()
	if err != nil || json.Unmarshal([]byte(payload), &persisted) != nil {
		t.Fatalf("persist credential: %v", err)
	}
	if _, exposed := persisted["private_key"]; exposed {
		t.Fatal("ephemeral private key must never be persisted")
	}
}

func TestLoginRejectsForeignCallbackHost(t *testing.T) {
	login, err := NewLogin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := login.ParseCallbackURL("https://example.com/?user_id=u&access_token=x", "system"); err == nil {
		t.Fatal("foreign callback host must be rejected")
	}
}
