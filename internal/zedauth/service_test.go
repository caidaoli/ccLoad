package zedauth

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestUserAgentUsesZedPlatformNames(t *testing.T) {
	os := runtime.GOOS
	if os == "darwin" {
		os = "macos"
	}
	arch := map[string]string{
		"386":     "x86",
		"amd64":   "x86_64",
		"arm64":   "aarch64",
		"loong64": "loongarch64",
		"ppc64":   "powerpc64",
		"ppc64le": "powerpc64",
		"wasm":    "wasm32",
	}[runtime.GOARCH]
	if arch == "" {
		arch = runtime.GOARCH
	}
	want := fmt.Sprintf("Zed/%s (%s; %s)", ZedVersion, os, arch)
	if got := UserAgent(); got != want {
		t.Fatalf("UserAgent() = %q, want %q", got, want)
	}
}

func TestServiceUsesNativeAndLLMAuthenticationContracts(t *testing.T) {
	expiresAt := time.Now().Add(time.Hour).Unix()
	token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, expiresAt))) + ".sig"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("User-Agent"); got != UserAgent() {
			t.Errorf("user-agent = %q, want %q", got, UserAgent())
		}
		switch request.URL.Path {
		case "/client/llm_tokens":
			if request.Method != http.MethodPost || request.Header.Get("Authorization") != `u-1 {"github_user_login":"octocat","access_token":"native"}` {
				t.Errorf("mint request contract: method=%s auth=%q", request.Method, request.Header.Get("Authorization"))
			}
			if request.Header.Get("x-zed-system-id") != "system-1" {
				t.Errorf("system id = %q", request.Header.Get("x-zed-system-id"))
			}
			_, _ = fmt.Fprintf(writer, `{"token":%q}`, token)
		case "/models":
			if request.Header.Get("Authorization") != "Bearer "+token || request.Header.Get("x-zed-version") != ZedVersion {
				t.Errorf("model request headers: %v", request.Header)
			}
			_, _ = writer.Write([]byte(`{"models":[{"id":"gpt-5.6-sol","provider":"open_ai"},{"id":"claude-sonnet-5","provider":"anthropic"},{"id":"gemini-3.5-flash","provider":"google"},{"id":"gpt-disabled","provider":"open_ai","is_disabled":true},{"id":"grok-4","provider":"x_ai"},{"id":"claude-wrong","provider":"open_ai"}]}`))
		case "/client/users/me":
			if strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ") {
				t.Error("usage must use native authorization, not the LLM JWT")
			}
			if request.Header.Get("x-zed-system-id") != "system-1" {
				t.Errorf("current user system id = %q", request.Header.Get("x-zed-system-id"))
			}
			_, _ = writer.Write([]byte(`{"user":{"username":"octocat","github_login":"octocat-gh"},"plan":{"plan_v3":"zed_pro","subscription_period":{"ended_at":"2026-09-01T00:00:00Z"},"usage":{"model_requests":{"used":12,"limit":{"limited":50}}}}}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	service := NewService(server.Client())
	service.LLMTokensURL = server.URL + "/client/llm_tokens"
	service.ModelsURL = server.URL + "/models"
	service.CurrentUserURL = server.URL + "/client/users/me"
	credential, err := NewCredential("u-1", "system-1", []byte(`{"github_user_login":"octocat","access_token":"native"}`))
	if err != nil {
		t.Fatal(err)
	}
	minted, err := service.MintLLMToken(context.Background(), credential)
	if err != nil {
		t.Fatal(err)
	}
	if minted.AccessToken != token || minted.ExpiresAt != expiresAt {
		t.Fatalf("minted credential = %+v", minted)
	}
	models, err := service.FetchModels(context.Background(), minted)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 || models[0] != "gpt-5.6-sol" || models[1] != "claude-sonnet-5" || models[2] != "gemini-3.5-flash" {
		t.Fatalf("models = %v", models)
	}
	account, err := service.FetchAccount(context.Background(), minted)
	if err != nil {
		t.Fatal(err)
	}
	if account.Username != "octocat" || account.GitHubUserLogin != "octocat-gh" {
		t.Fatalf("account = %+v", account)
	}
	usage := &account.Usage
	if usage.PlanType != "zed_pro" || usage.Used == nil || *usage.Used != 12 || usage.Limit == nil || *usage.Limit != 50 {
		t.Fatalf("usage = %+v", usage)
	}
}
