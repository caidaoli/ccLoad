package cursorauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestService(t *testing.T, handler http.Handler) (*Service, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	service := NewService(server.Client())
	service.APIBaseURL = server.URL
	return service, server
}

func TestExchangeAPIKeyAndIdentity(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case ExchangeAPIKeyPath:
			if r.Header.Get("Authorization") != "Bearer user-key" {
				t.Errorf("authorization = %q", r.Header.Get("Authorization"))
			}
			_, _ = io.WriteString(w, `{"accessToken":"tok","refreshToken":"ref"}`)
		case GetMeRPC:
			if r.Header.Get("connect-protocol-version") != "1" ||
				r.Header.Get("x-cursor-client-type") != ClientType {
				t.Errorf("headers = %v", r.Header)
			}
			_, _ = io.WriteString(w, `{"authId":"auth-1","email":"user@example.com","firstName":"Ada","lastName":"Lovelace"}`)
		default:
			t.Errorf("path = %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	pair, err := service.ExchangeAPIKey(context.Background(), "user-key")
	if err != nil || pair.AccessToken != "tok" {
		t.Fatalf("pair = %+v err = %v", pair, err)
	}
	identity, name, err := service.FetchIdentity(context.Background(), "tok")
	if err != nil || identity.UserID != "auth-1" || identity.Email != "user@example.com" || name != "Ada Lovelace" {
		t.Fatalf("identity = %+v name = %q err = %v", identity, name, err)
	}
}

func TestConnectJSONRejectsUnauthorized(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	if _, _, err := service.FetchIdentity(context.Background(), "tok"); !errors.Is(err, ErrSessionRejected) {
		t.Fatalf("unauthorized identity err = %v", err)
	}
}
