package app

import (
	"testing"
)

func TestValidateChannelBaseURLAllowsLocalAndPrivateHosts(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"http://localhost:8080":          "http://localhost:8080",
		"http://localhost.:8080":         "http://localhost.:8080",
		"http://127.0.0.1:8080":          "http://127.0.0.1:8080",
		"http://127.0.0.1.":              "http://127.0.0.1.",
		"http://127.1":                   "http://127.1",
		"http://2130706433":              "http://2130706433",
		"http://017700000001":            "http://017700000001",
		"http://10.0.0.1":                "http://10.0.0.1",
		"http://172.16.0.1":              "http://172.16.0.1",
		"http://192.168.1.1":             "http://192.168.1.1",
		"http://169.254.169.254/latest":  "http://169.254.169.254/latest",
		"http://[::1]:8080":              "http://[::1]:8080",
		"http://[::ffff:127.0.0.1]:8080": "http://[::ffff:127.0.0.1]:8080",
		"http://[fc00::1]":               "http://[fc00::1]",
		"http://[fe80::1%25lo0]":         "http://[fe80::1%lo0]",
	}

	for raw, want := range tests {
		t.Run(raw, func(t *testing.T) {
			got, err := validateChannelBaseURL(raw)
			if err != nil {
				t.Fatalf("validateChannelBaseURL(%q) error = %v", raw, err)
			}
			if got != want {
				t.Fatalf("validateChannelBaseURL(%q) = %q, want %q", raw, got, want)
			}
		})
	}
}

func TestValidateChannelBaseURLAllowsPublicHost(t *testing.T) {
	t.Parallel()

	got, err := validateChannelBaseURL("https://api.example.com/openai/")
	if err != nil {
		t.Fatalf("validateChannelBaseURL() error = %v", err)
	}
	if got != "https://api.example.com/openai" {
		t.Fatalf("normalized URL = %q, want public host with trimmed path", got)
	}
}
