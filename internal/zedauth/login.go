package zedauth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// Login owns the ephemeral RSA key used by one native Zed sign-in session.
type Login struct {
	privateKey *rsa.PrivateKey
	publicKey  string
}

// NativeCallback contains the account identity and decrypted native credential.
type NativeCallback struct {
	UserID           string
	NativeCredential json.RawMessage
}

// NewLogin creates a native Zed login session.
func NewLogin() (*Login, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate zed login key: %w", err)
	}
	publicDER := x509.MarshalPKCS1PublicKey(&privateKey.PublicKey)
	return &Login{
		privateKey: privateKey,
		publicKey:  base64.RawURLEncoding.EncodeToString(publicDER),
	}, nil
}

// AuthorizationURL returns the Zed native sign-in URL for the loopback callback.
func (l *Login) AuthorizationURL(port int) (string, error) {
	if l == nil || l.privateKey == nil || l.publicKey == "" || port < 1 || port > 65535 {
		return "", errors.New("zed login callback port is invalid")
	}
	query := url.Values{}
	query.Set("native_app_port", strconv.Itoa(port))
	query.Set("native_app_public_key", l.publicKey)
	return NativeSignInURL + "?" + query.Encode(), nil
}

// ParseCallbackURL decrypts a callback and attaches the installation identity.
func (l *Login) ParseCallbackURL(rawURL, systemID string) (*Credential, error) {
	callback, err := l.DecryptCallbackURL(rawURL)
	if err != nil {
		return nil, err
	}
	return NewCredential(callback.UserID, systemID, callback.NativeCredential)
}

// DecryptCallbackURL validates and decrypts a native Zed callback URL.
func (l *Login) DecryptCallbackURL(rawURL string) (*NativeCallback, error) {
	if l == nil || l.privateKey == nil {
		return nil, errors.New("zed login key is unavailable")
	}
	callbackURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !callbackURL.IsAbs() || !strings.EqualFold(callbackURL.Scheme, "http") {
		return nil, errors.New("invalid Zed callback URL")
	}
	host := callbackURL.Hostname()
	isLoopback := strings.EqualFold(host, "localhost")
	if !isLoopback {
		ip := net.ParseIP(host)
		isLoopback = ip != nil && ip.IsLoopback()
	}
	if !isLoopback || callbackURL.Path != "/" {
		return nil, errors.New("zed callback URL must use a loopback host and root path")
	}
	userID := strings.TrimSpace(callbackURL.Query().Get("user_id"))
	encrypted := strings.TrimSpace(callbackURL.Query().Get("access_token"))
	if userID == "" || encrypted == "" {
		return nil, errors.New("zed callback is missing user_id or access_token")
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(encrypted, "="))
	if err != nil {
		return nil, errors.New("zed callback access_token is not valid base64url")
	}
	plaintext, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, l.privateKey, ciphertext, nil)
	if err != nil {
		return nil, errors.New("decrypt Zed callback access_token failed")
	}
	if !json.Valid(plaintext) {
		return nil, errors.New("decrypted Zed credential is not valid JSON")
	}
	return &NativeCallback{UserID: userID, NativeCredential: append(json.RawMessage(nil), plaintext...)}, nil
}
