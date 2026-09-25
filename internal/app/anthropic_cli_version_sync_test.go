package app

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnthropicCLIVersionSyncRefreshAndCache(t *testing.T) {
	// The version is process-wide; this test stays sequential and restores it.
	anthropicRuntimeCLIVersion.Store(nil)
	t.Cleanup(func() { anthropicRuntimeCLIVersion.Store(nil) })
	for _, testCase := range []struct {
		name        string
		status      int
		body        string
		wantErr     bool
		wantVersion string
	}{
		{name: "newer", status: 200, body: `{"tag_name":"v2.1.999"}`, wantVersion: "2.1.999"},
		{name: "older does not downgrade", status: 200, body: `{"tag_name":"v2.1.300"}`, wantVersion: "2.1.999"},
		{name: "below floor", status: 200, body: `{"tag_name":"v2.1.200"}`, wantErr: true, wantVersion: "2.1.999"},
		{name: "http error", status: 503, body: `{}`, wantErr: true, wantVersion: "2.1.999"},
		{name: "invalid json", status: 200, body: `{`, wantErr: true, wantVersion: "2.1.999"},
		{name: "oversized", status: 200, body: strings.Repeat("x", anthropicCLIVersionMaxBodyBytes+1), wantErr: true, wantVersion: "2.1.999"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != anthropicCLIVersionSyncURL {
					t.Fatalf("unexpected URL: %s", r.URL)
				}
				return &http.Response{StatusCode: testCase.status, Status: http.StatusText(testCase.status),
					Body: io.NopCloser(strings.NewReader(testCase.body))}, nil
			})}
			err := refreshAnthropicCLIVersion(context.Background(), client)
			if (err != nil) != testCase.wantErr {
				t.Fatalf("err=%v, wantErr=%v", err, testCase.wantErr)
			}
			if got := anthropicEffectiveCLIVersion(); got != testCase.wantVersion {
				t.Fatalf("version=%q, want %q", got, testCase.wantVersion)
			}
		})
	}

	cachePath := filepath.Join(t.TempDir(), "nested", "cli-version.json")
	if err := saveAnthropicCLIVersionCache(cachePath, anthropicEffectiveCLIVersion()); err != nil {
		t.Fatal(err)
	}
	anthropicRuntimeCLIVersion.Store(nil)
	if err := loadAnthropicCLIVersionCache(cachePath); err != nil {
		t.Fatal(err)
	}
	if got := anthropicEffectiveCLIVersion(); got != "2.1.999" {
		t.Fatalf("cached version=%q", got)
	}
}

func TestAnthropicCLIVersionSyncCanDisableNetwork(t *testing.T) {
	anthropicRuntimeCLIVersion.Store(nil)
	t.Cleanup(func() { anthropicRuntimeCLIVersion.Store(nil) })
	cachePath := filepath.Join(t.TempDir(), "version.json")
	if err := saveAnthropicCLIVersionCache(cachePath, "2.1.999"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CCLOAD_ANTHROPIC_CLI_VERSION_CACHE", cachePath)
	t.Setenv("CCLOAD_ANTHROPIC_CLI_VERSION_SYNC", "false")
	server := &Server{}
	server.StartAnthropicCLIVersionSync()
	server.wg.Wait()
	if got := anthropicEffectiveCLIVersion(); got != "2.1.999" {
		t.Fatalf("disabled sync did not use cache: %q", got)
	}
}
