package app

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/model"

	"github.com/gin-gonic/gin"
)

func TestGetReadTimeoutFallsBackToBuiltInDefault(t *testing.T) {
	t.Parallel()
	var unset *Server
	if unset.GetReadTimeout() != config.DefaultHTTPReadTimeout {
		t.Fatalf("nil server read timeout = %v", unset.GetReadTimeout())
	}
	if (&Server{}).GetReadTimeout() != config.DefaultHTTPReadTimeout {
		t.Fatalf("zero value read timeout = %v", (&Server{}).GetReadTimeout())
	}
	configured := &Server{httpReadTimeout: 10 * time.Minute}
	if configured.GetReadTimeout() != 10*time.Minute {
		t.Fatalf("configured read timeout = %v", configured.GetReadTimeout())
	}
}

// The setting is a duration in seconds where 0 means "use the built-in
// default"; a negative value is invalid and must not disable the deadline.
func TestLoadHTTPReadTimeoutNormalizesSetting(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		value string
		want  time.Duration
	}{
		"unset":    {value: "", want: config.DefaultHTTPReadTimeout},
		"zero":     {value: "0", want: config.DefaultHTTPReadTimeout},
		"explicit": {value: "600", want: 10 * time.Minute},
		"negative": {value: "-1", want: config.DefaultHTTPReadTimeout},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cs := newTestConfigService(t, map[string]string{config.HTTPReadTimeoutSettingKey: test.value})
			if got := loadHTTPReadTimeout(cs); got != test.want {
				t.Fatalf("loadHTTPReadTimeout() = %v, want %v", got, test.want)
			}
		})
	}
}

// A slow upload and an oversized upload need different remedies, so they must
// not collapse into the same 400 response.
func TestParseIncomingRequestSeparatesReadTimeoutFromSizeLimit(t *testing.T) {
	t.Parallel()
	limits := newRequestBodyLimits(16, 16)

	oversized := newParseRequestContext(t, strings.NewReader(strings.Repeat("x", 64)))
	_, err := parseIncomingRequest(oversized, limits)
	if !errors.Is(err, errBodyTooLarge) {
		t.Fatalf("oversized body error = %v", err)
	}
	if !strings.Contains(err.Error(), "max_body_bytes") {
		t.Fatalf("oversized body message must name the size settings: %v", err)
	}

	timedOut := newParseRequestContext(t, &errorReader{err: os.ErrDeadlineExceeded})
	_, err = parseIncomingRequest(timedOut, limits)
	if !errors.Is(err, errBodyReadTimeout) {
		t.Fatalf("timed out body error = %v", err)
	}
	if !strings.Contains(err.Error(), config.HTTPReadTimeoutSettingKey) {
		t.Fatalf("timed out message must name the read timeout setting: %v", err)
	}
	if errors.Is(err, errBodyTooLarge) {
		t.Fatal("a read timeout must never be reported as a size limit")
	}

	// A plain I/O failure stays a generic read failure.
	broken := newParseRequestContext(t, &errorReader{err: errors.New("connection reset by peer")})
	_, err = parseIncomingRequest(broken, limits)
	if errors.Is(err, errBodyReadTimeout) || errors.Is(err, errBodyTooLarge) {
		t.Fatalf("generic read failure = %v", err)
	}
}

func TestIsRequestReadTimeoutClassifiesNetErrors(t *testing.T) {
	t.Parallel()
	if !isRequestReadTimeout(os.ErrDeadlineExceeded) {
		t.Fatal("deadline exceeded must be a read timeout")
	}
	if !isRequestReadTimeout(fmt.Errorf("read tcp: %w", timeoutError{})) {
		t.Fatal("a wrapped net.Error timeout must be a read timeout")
	}
	if isRequestReadTimeout(errors.New("connection reset by peer")) {
		t.Fatal("a reset connection is not a read timeout")
	}
	if isRequestReadTimeout(nil) {
		t.Fatal("nil is not a read timeout")
	}
}

type timeoutError struct{}

func (timeoutError) Error() string { return "i/o timeout" }
func (timeoutError) Timeout() bool { return true }

var _ net.Error = timeoutError{}

func (timeoutError) Temporary() bool { return false }

func newTestConfigService(t *testing.T, values map[string]string) *ConfigService {
	t.Helper()
	cs := NewConfigService(nil)
	cs.mu.Lock()
	for key, value := range values {
		if value == "" {
			continue
		}
		cs.cache[key] = &model.SystemSetting{Key: key, Value: value}
	}
	cs.mu.Unlock()
	return cs
}

func newParseRequestContext(t *testing.T, body interface{ Read([]byte) (int, error) }) *gin.Context {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	request.Header.Set("Content-Type", "application/json")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = request
	return ctx
}
