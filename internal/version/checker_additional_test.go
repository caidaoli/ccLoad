package version

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type signalReadCloser struct {
	rc      io.ReadCloser
	onClose func()
}

func (s *signalReadCloser) Read(p []byte) (int, error) { return s.rc.Read(p) }

func (s *signalReadCloser) Close() error {
	if s.onClose != nil {
		s.onClose()
		s.onClose = nil
	}
	return s.rc.Close()
}

func httpResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     strconv.Itoa(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}

func latestReleaseResp(t *testing.T, req *http.Request, status int, finalURL string) *http.Response {
	t.Helper()

	resp := httpResp(status, "<html></html>")
	resp.Request = req.Clone(req.Context())
	if finalURL != "" {
		u, err := url.Parse(finalURL)
		if err != nil {
			t.Fatalf("parse finalURL: %v", err)
		}
		resp.Request.URL = u
	}
	return resp
}

func TestChecker_Check_ErrorsAndSuccess(t *testing.T) {
	origVersion := Version
	t.Cleanup(func() { Version = origVersion })

	t.Run("client error", func(t *testing.T) {
		c := &Checker{
			client: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					return nil, errors.New("boom")
				}),
			},
		}
		c.check()
		if c.lastCheck != (time.Time{}) {
			t.Fatalf("expected lastCheck untouched on error, got %v", c.lastCheck)
		}
	})

	t.Run("non-200", func(t *testing.T) {
		c := &Checker{
			client: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					return latestReleaseResp(t, req, http.StatusTooManyRequests, "https://github.com/caidaoli/ccLoad/releases/tag/v1.2.3"), nil
				}),
			},
		}
		c.check()
		if c.latestVersion != "" || c.releaseURL != "" || c.hasUpdate {
			t.Fatalf("expected no state update on non-200, got latest=%q url=%q hasUpdate=%v", c.latestVersion, c.releaseURL, c.hasUpdate)
		}
	})

	t.Run("missing release tag URL", func(t *testing.T) {
		c := &Checker{
			client: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					return latestReleaseResp(t, req, http.StatusOK, "https://github.com/caidaoli/ccLoad/releases/latest"), nil
				}),
			},
		}
		c.check()
		if c.latestVersion != "" || c.releaseURL != "" || c.hasUpdate {
			t.Fatalf("expected no state update on missing tag URL, got latest=%q url=%q hasUpdate=%v", c.latestVersion, c.releaseURL, c.hasUpdate)
		}
	})

	t.Run("success no update", func(t *testing.T) {
		Version = "v1.2.3"
		c := &Checker{
			client: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.Header.Get("Accept") == "" || req.Header.Get("User-Agent") == "" {
						t.Fatalf("expected headers set, got Accept=%q UA=%q", req.Header.Get("Accept"), req.Header.Get("User-Agent"))
					}
					return latestReleaseResp(t, req, http.StatusOK, "https://github.com/caidaoli/ccLoad/releases/tag/v1.2.3"), nil
				}),
			},
		}
		c.check()
		if c.latestVersion != "v1.2.3" || c.releaseURL != "https://github.com/caidaoli/ccLoad/releases/tag/v1.2.3" {
			t.Fatalf("unexpected state: latest=%q url=%q", c.latestVersion, c.releaseURL)
		}
		if c.hasUpdate {
			t.Fatalf("expected hasUpdate=false when versions equal")
		}
		if c.lastCheck.IsZero() {
			t.Fatalf("expected lastCheck set")
		}
	})

	t.Run("success has update", func(t *testing.T) {
		Version = "v1.0.0"
		c := &Checker{
			client: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					return latestReleaseResp(t, req, http.StatusOK, "https://github.com/caidaoli/ccLoad/releases/tag/v2.0.0"), nil
				}),
			},
		}
		c.check()
		if !c.hasUpdate || c.latestVersion != "v2.0.0" || c.releaseURL != "https://github.com/caidaoli/ccLoad/releases/tag/v2.0.0" {
			t.Fatalf("unexpected state: hasUpdate=%v latest=%q url=%q", c.hasUpdate, c.latestVersion, c.releaseURL)
		}
	})

	t.Run("success current version newer", func(t *testing.T) {
		Version = "v2.0.0"
		c := &Checker{
			client: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					return latestReleaseResp(t, req, http.StatusOK, "https://github.com/caidaoli/ccLoad/releases/tag/v1.9.9"), nil
				}),
			},
		}
		c.check()
		if c.hasUpdate {
			t.Fatalf("expected hasUpdate=false when current version is newer")
		}
	})
}

func TestStartChecker_RunsCheckOnce(t *testing.T) {
	origVersion := Version
	origClient := checker.client
	t.Cleanup(func() {
		Version = origVersion
		checker.client = origClient
	})

	Version = "v1.0.0"

	var calls int32
	done := make(chan struct{})
	checker.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			atomic.AddInt32(&calls, 1)
			resp := latestReleaseResp(t, req, http.StatusOK, "https://github.com/caidaoli/ccLoad/releases/tag/v2.0.0")
			resp.Body = &signalReadCloser{
				rc: resp.Body,
				onClose: func() {
					select {
					case <-done:
					default:
						close(done)
					}
				},
			}
			return resp, nil
		}),
	}

	StartChecker()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatalf("expected StartChecker to run check at least once")
	}

	if atomic.LoadInt32(&calls) == 0 {
		t.Fatalf("expected StartChecker to issue at least one HTTP call")
	}
	checker.mu.RLock()
	lastCheck := checker.lastCheck
	checker.mu.RUnlock()
	if lastCheck.IsZero() {
		t.Fatalf("expected lastCheck to be set")
	}
}
