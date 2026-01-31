package version

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func httpResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     strconv.Itoa(status),
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
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
					return httpResp(http.StatusTooManyRequests, "{}"), nil
				}),
			},
		}
		c.check()
		if c.latestVersion != "" || c.releaseURL != "" || c.hasUpdate {
			t.Fatalf("expected no state update on non-200, got latest=%q url=%q hasUpdate=%v", c.latestVersion, c.releaseURL, c.hasUpdate)
		}
	})

	t.Run("bad json", func(t *testing.T) {
		c := &Checker{
			client: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					return httpResp(http.StatusOK, "{"), nil
				}),
			},
		}
		c.check()
		if c.latestVersion != "" || c.releaseURL != "" || c.hasUpdate {
			t.Fatalf("expected no state update on bad json, got latest=%q url=%q hasUpdate=%v", c.latestVersion, c.releaseURL, c.hasUpdate)
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
					return httpResp(http.StatusOK, `{"tag_name":"v1.2.3","html_url":"https://example.com/release"}`), nil
				}),
			},
		}
		c.check()
		if c.latestVersion != "v1.2.3" || c.releaseURL != "https://example.com/release" {
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
					return httpResp(http.StatusOK, `{"tag_name":"v2.0.0","html_url":"https://example.com/release2"}`), nil
				}),
			},
		}
		c.check()
		if !c.hasUpdate || c.latestVersion != "v2.0.0" || c.releaseURL != "https://example.com/release2" {
			t.Fatalf("unexpected state: hasUpdate=%v latest=%q url=%q", c.hasUpdate, c.latestVersion, c.releaseURL)
		}
	})
}

func TestStartChecker_RunsCheckOnce(t *testing.T) {
	origVersion := Version
	origClient := checker.client

	Version = "v1.0.0"

	var calls int32
	checker.client = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			atomic.AddInt32(&calls, 1)
			return httpResp(http.StatusOK, `{"tag_name":"v2.0.0","html_url":"https://example.com/release"}`), nil
		}),
	}

	StartChecker()

	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&calls) > 0 {
			checker.mu.RLock()
			done := !checker.lastCheck.IsZero()
			checker.mu.RUnlock()
			if done {
				Version = origVersion
				checker.client = origClient
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 失败时也要恢复全局状态，避免影响其它测试
	Version = origVersion
	checker.client = origClient
	t.Fatalf("expected StartChecker to run check at least once")
}
