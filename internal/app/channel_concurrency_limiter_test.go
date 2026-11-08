package app

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"ccLoad/internal/model"
)

func TestChannelConcurrencyLimiterRejectsUntilRelease(t *testing.T) {
	t.Parallel()

	limiter := newChannelConcurrencyLimiter()

	release, active, limit, ok := limiter.acquire(7, 1)
	if !ok || active != 1 || limit != 1 {
		t.Fatalf("first acquire got active=%d limit=%d ok=%v, want 1,1,true", active, limit, ok)
	}

	_, active, limit, ok = limiter.acquire(7, 1)
	if ok || active != 1 || limit != 1 {
		t.Fatalf("second acquire got active=%d limit=%d ok=%v, want 1,1,false", active, limit, ok)
	}

	release()

	release, active, limit, ok = limiter.acquire(7, 1)
	if !ok || active != 1 || limit != 1 {
		t.Fatalf("after release got active=%d limit=%d ok=%v, want 1,1,true", active, limit, ok)
	}
	release()
}

func TestDoUpstreamRequestHoldsChannelConcurrencyUntilBodyClosed(t *testing.T) {
	t.Parallel()

	unblock := make(chan struct{})
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-unblock
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	s := &Server{
		client:                    newTestHTTPClient(),
		channelConcurrencyLimiter: newChannelConcurrencyLimiter(),
	}
	cfg := &model.Config{ID: 42, MaxConcurrency: 1}

	firstReq, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatalf("new first request: %v", err)
	}
	firstResp, err := s.doUpstreamRequest(cfg, firstReq)
	if err != nil {
		t.Fatalf("first request failed: %v", err)
	}

	secondReq, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatalf("new second request: %v", err)
	}
	secondResp, err := s.doUpstreamRequest(cfg, secondReq)
	if secondResp != nil {
		_ = secondResp.Body.Close()
	}
	if !errors.Is(err, ErrChannelConcurrencyExceeded) {
		t.Fatalf("second request error=%v, want ErrChannelConcurrencyExceeded", err)
	}

	close(unblock)
	if _, err := io.ReadAll(firstResp.Body); err != nil {
		t.Fatalf("read first response: %v", err)
	}
	if err := firstResp.Body.Close(); err != nil {
		t.Fatalf("close first response: %v", err)
	}

	thirdReq, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatalf("new third request: %v", err)
	}
	thirdResp, err := s.doUpstreamRequest(cfg, thirdReq)
	if err != nil {
		t.Fatalf("third request after release failed: %v", err)
	}
	_ = thirdResp.Body.Close()
}

/**
 * @brief ZAI 接入不得改变普通渠道的预留顺序、取消错误优先级及 RPM 消耗。
 */
func TestDoUpstreamRequestNonZAICancellationReservation(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "ok") }))
	for _, tc := range []struct {
		name             string
		concurrency, rpm int
		first            bool
		want             error
	}{
		{"concurrency-before-cancel", 1, 0, true, ErrChannelConcurrencyExceeded},
		{"rpm-before-cancel", 0, 1, true, ErrChannelRPMExceeded},
		{"cancel-consumes-reserved-rpm", 0, 1, false, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{client: newTestHTTPClient(), channelConcurrencyLimiter: newChannelConcurrencyLimiter(), channelRPMLimiter: newRPMLimiter(nil)}
			cfg := &model.Config{ID: 42, MaxConcurrency: tc.concurrency, RPMLimit: tc.rpm}
			if tc.first {
				req, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := s.doUpstreamRequest(cfg, req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := s.doUpstreamRequest(cfg, req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v, want %v", err, tc.want)
			}
			if !tc.first {
				next, err := http.NewRequest(http.MethodGet, upstream.URL, nil)
				if err != nil {
					t.Fatal(err)
				}
				resp, err := s.doUpstreamRequest(cfg, next)
				if resp != nil {
					_ = resp.Body.Close()
				}
				if !errors.Is(err, ErrChannelRPMExceeded) {
					t.Fatalf("cancelled reservation no longer consumes RPM: %v", err)
				}
			}
		})
	}
}

/**
 * @brief 普通渠道保留 HTTP 客户端记录的最终重定向请求身份。
 */
func TestDoUpstreamRequestNonZAIRedirectRequestIdentity(t *testing.T) {
	upstream := newTestHTTPServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusSeeOther)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	s := &Server{client: newTestHTTPClient()}
	req, err := http.NewRequest(http.MethodPost, upstream.URL+"/start", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := s.doUpstreamRequest(&model.Config{ID: 42}, req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Request == nil || resp.Request.URL.Path != "/final" || resp.Request.Method != http.MethodGet {
		t.Fatalf("final request identity overwritten: %+v", resp.Request)
	}
}
