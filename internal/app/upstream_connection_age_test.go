package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

type closeIdleTrackingRoundTripper struct {
	mu     sync.Mutex
	closes int
}

func (t *closeIdleTrackingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (t *closeIdleTrackingRoundTripper) CloseIdleConnections() {
	t.mu.Lock()
	t.closes++
	t.mu.Unlock()
}

func (t *closeIdleTrackingRoundTripper) closeCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closes
}

func TestServerUsesStandardHTTP11OnlyForAntigravity(t *testing.T) {
	type observedRequest struct {
		path     string
		protocol int
	}
	observed := make(chan observedRequest, 4)
	closed := make(chan struct{}, 2)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- observedRequest{path: r.URL.Path, protocol: r.ProtoMajor}
		_, _ = io.WriteString(w, "ok")
	}))
	upstream.EnableHTTP2 = true
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	upstream.StartTLS()
	t.Cleanup(upstream.Close)

	base := buildHTTPTransport(true)
	maxAge := 100 * time.Millisecond
	server := &Server{
		client:            newUpstreamHTTPClient(base, maxAge),
		antigravityClient: newAntigravityHTTPClient(base, maxAge),
	}
	t.Cleanup(func() {
		closeUpstreamHTTPClient(server.client)
		closeUpstreamHTTPClient(server.antigravityClient)
	})

	request := func(config *model.Config, path string) httptrace.GotConnInfo {
		t.Helper()
		gotConn := make(chan httptrace.GotConnInfo, 1)
		req, err := http.NewRequestWithContext(
			context.Background(), http.MethodPost, upstream.URL+path, bytes.NewBufferString(`{"request":{}}`),
		)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { gotConn <- info },
		}))
		resp, err := server.getClientForChannel(config).Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		if err = resp.Body.Close(); err != nil {
			t.Fatalf("close %s response: %v", path, err)
		}
		return <-gotConn
	}

	antigravity := &model.Config{AuthType: model.AuthTypeAntigravityOAuth}
	first := request(antigravity, "/antigravity-first")
	second := request(antigravity, "/antigravity-second")
	if !second.Reused || first.Conn != second.Conn {
		t.Fatalf("Antigravity HTTP/1.1 connection was not reused: reused=%t same=%t", second.Reused, first.Conn == second.Conn)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Antigravity HTTP/1.1 connection was not closed after max age")
	}
	third := request(antigravity, "/antigravity-third")
	request(&model.Config{}, "/default")

	if third.Reused || third.Conn == first.Conn {
		t.Fatalf("Antigravity reused an aged connection: reused=%t same=%t", third.Reused, third.Conn == first.Conn)
	}

	protocols := make(map[string]int, 4)
	for range 4 {
		got := <-observed
		protocols[got.path] = got.protocol
	}
	for _, path := range []string{"/antigravity-first", "/antigravity-second", "/antigravity-third"} {
		if got := protocols[path]; got != 1 {
			t.Fatalf("%s protocol = HTTP/%d, want HTTP/1.1", path, got)
		}
	}
	if got := protocols["/default"]; got != 2 {
		t.Fatalf("default protocol = HTTP/%d, want HTTP/2", got)
	}
}

func TestServerIsolatesAntigravityHTTP11PoolByRefreshCredential(t *testing.T) {
	server := &Server{
		client:            http.DefaultClient,
		antigravityClient: newAntigravityHTTPClient(buildHTTPTransport(true), 0),
	}
	t.Cleanup(func() {
		closeUpstreamHTTPClient(server.antigravityClient)
		if server.antigravityTransports != nil {
			server.antigravityTransports.closeAll()
		}
	})

	credential := func(refresh, access string) string {
		return fmt.Sprintf(`{"type":"antigravity","access_token":%q,"refresh_token":%q,"expired":"2099-01-01T00:00:00Z"}`, access, refresh)
	}
	first := &model.Config{ID: 1, AuthType: model.AuthTypeAntigravityOAuth, OAuthCredential: credential("refresh-a", "access-a")}
	rotated := first.Clone()
	rotated.OAuthCredential = credential("refresh-a", "access-b")
	second := &model.Config{ID: 2, AuthType: model.AuthTypeAntigravityOAuth, OAuthCredential: credential("refresh-b", "access-c")}

	firstClient := server.getClientForChannel(first)
	rotatedClient := server.getClientForChannel(rotated)
	secondClient := server.getClientForChannel(second)
	if firstClient != rotatedClient {
		t.Fatal("access-token rotation should retain the refresh-token scoped client")
	}
	if firstClient == secondClient {
		t.Fatal("different Antigravity refresh credentials must not share an HTTP client")
	}
	if got := server.antigravityTransports.order.Len(); got != 2 {
		t.Fatalf("cache entries=%d, want 2", got)
	}
}

func TestServerDoesNotReuseAntigravityConnectionAcrossCredentials(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(upstream.Close)
	server := &Server{
		client:            http.DefaultClient,
		antigravityClient: newAntigravityHTTPClient(buildHTTPTransport(false), 0),
	}
	t.Cleanup(func() {
		closeUpstreamHTTPClient(server.antigravityClient)
		if server.antigravityTransports != nil {
			server.antigravityTransports.closeAll()
		}
	})
	credential := func(refresh string) string {
		return fmt.Sprintf(`{"type":"antigravity","access_token":"access","refresh_token":%q,"expired":"2099-01-01T00:00:00Z"}`, refresh)
	}
	firstCredential := &model.Config{ID: 1, AuthType: model.AuthTypeAntigravityOAuth, OAuthCredential: credential("refresh-a")}
	secondCredential := &model.Config{ID: 2, AuthType: model.AuthTypeAntigravityOAuth, OAuthCredential: credential("refresh-b")}

	request := func(cfg *model.Config) httptrace.GotConnInfo {
		t.Helper()
		gotConn := make(chan httptrace.GotConnInfo, 1)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, upstream.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { gotConn <- info },
		}))
		resp, err := server.getClientForChannel(cfg).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return <-gotConn
	}

	first := request(firstCredential)
	reused := request(firstCredential)
	other := request(secondCredential)
	if !reused.Reused || reused.Conn != first.Conn {
		t.Fatalf("same credential did not reuse its HTTP/1.1 connection: reused=%t same=%t", reused.Reused, reused.Conn == first.Conn)
	}
	if other.Conn == first.Conn {
		t.Fatal("different Antigravity credentials reused the same physical connection")
	}
}

func TestAntigravityHTTPClientCacheIsBounded(t *testing.T) {
	cache := newAntigravityHTTPClientCache(2)
	trackers := make([]*closeIdleTrackingRoundTripper, 0, 3)
	for _, scope := range []string{"a", "b", "c"} {
		client, err := cache.getOrCreate(upstreamHTTPClientCacheKey{credentialScope: scope}, func() (*http.Client, error) {
			tracker := &closeIdleTrackingRoundTripper{}
			trackers = append(trackers, tracker)
			return &http.Client{Transport: tracker}, nil
		})
		if err != nil || client == nil {
			t.Fatalf("create %s: client=%v err=%v", scope, client, err)
		}
	}
	if got := cache.order.Len(); got != 2 {
		t.Fatalf("cache entries=%d, want bounded size 2", got)
	}
	if _, ok := cache.entries[upstreamHTTPClientCacheKey{credentialScope: "a"}]; ok {
		t.Fatal("least-recently-used client was not evicted")
	}
	if got := trackers[0].closeCount(); got != 1 {
		t.Fatalf("evicted client CloseIdleConnections calls=%d, want 1", got)
	}
	agingCache := newAntigravityHTTPClientCache(1)
	agingClient, err := agingCache.getOrCreate(upstreamHTTPClientCacheKey{credentialScope: "aging"}, func() (*http.Client, error) {
		return newAntigravityHTTPClient(buildHTTPTransport(false), time.Hour), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	agingTransport, ok := agingClient.Transport.(*upstreamConnectionAgeTransport)
	if !ok {
		t.Fatalf("aging transport type=%T", agingClient.Transport)
	}
	if _, err = agingCache.getOrCreate(upstreamHTTPClientCacheKey{credentialScope: "evict-aging"}, func() (*http.Client, error) {
		return &http.Client{Transport: &closeIdleTrackingRoundTripper{}}, nil
	}); err != nil {
		t.Fatal(err)
	}
	agingTransport.mu.Lock()
	agingClosed := agingTransport.closed
	agingTransport.mu.Unlock()
	if !agingClosed {
		t.Fatal("LRU eviction did not stop the connection-age transport lifecycle")
	}
	agingCache.closeAll()
	cache.closeAll()
	if _, err := cache.getOrCreate(upstreamHTTPClientCacheKey{credentialScope: "after-close"}, func() (*http.Client, error) {
		return &http.Client{Transport: &closeIdleTrackingRoundTripper{}}, nil
	}); !errors.Is(err, errAntigravityHTTPClientCacheClosed) {
		t.Fatalf("closed cache accepted a new pool: %v", err)
	}

	closingCache := newAntigravityHTTPClientCache(1)
	builderStarted := make(chan struct{})
	releaseBuilder := make(chan struct{})
	buildResult := make(chan error, 1)
	inFlightTracker := &closeIdleTrackingRoundTripper{}
	go func() {
		_, err := closingCache.getOrCreate(upstreamHTTPClientCacheKey{credentialScope: "in-flight"}, func() (*http.Client, error) {
			close(builderStarted)
			<-releaseBuilder
			return &http.Client{Transport: inFlightTracker}, nil
		})
		buildResult <- err
	}()
	<-builderStarted
	closingCache.closeAll()
	close(releaseBuilder)
	if err := <-buildResult; !errors.Is(err, errAntigravityHTTPClientCacheClosed) {
		t.Fatalf("in-flight builder resurrected a closed cache: %v", err)
	}
	if got := inFlightTracker.closeCount(); got != 1 {
		t.Fatalf("discarded in-flight pool close calls=%d, want 1", got)
	}
}

func TestAntigravityPoolLimitsMatchNativeReuseProfile(t *testing.T) {
	base := &http.Transport{MaxIdleConns: 20, MaxIdleConnsPerHost: 20, IdleConnTimeout: 90 * time.Second}
	client := newAntigravityHTTPClient(base, 0)
	t.Cleanup(func() { closeUpstreamHTTPClient(client) })
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.MaxIdleConnsPerHost < antigravityMaxIdleConnsPerHost ||
		transport.MaxIdleConns < transport.MaxIdleConnsPerHost ||
		transport.IdleConnTimeout < antigravityIdleConnTimeout {
		t.Fatalf("Antigravity pool limits = idle %d per-host %d timeout %v",
			transport.MaxIdleConns, transport.MaxIdleConnsPerHost, transport.IdleConnTimeout)
	}
	if base.MaxIdleConnsPerHost != 20 || base.IdleConnTimeout != 90*time.Second {
		t.Fatal("Antigravity pool tuning mutated the shared base transport")
	}
	wide := &http.Transport{MaxIdleConns: 512, MaxIdleConnsPerHost: 256, IdleConnTimeout: time.Hour}
	applyAntigravityPoolLimits(wide)
	if wide.MaxIdleConns != 512 || wide.MaxIdleConnsPerHost != 256 || wide.IdleConnTimeout != time.Hour {
		t.Fatalf("wider operator pool was narrowed: %#v", wide)
	}
	unlimited := &http.Transport{MaxIdleConns: 0, IdleConnTimeout: 0}
	applyAntigravityPoolLimits(unlimited)
	if unlimited.MaxIdleConns != 0 || unlimited.IdleConnTimeout != 0 || unlimited.MaxIdleConnsPerHost != antigravityMaxIdleConnsPerHost {
		t.Fatalf("unlimited sentinels/default per-host were mishandled: %#v", unlimited)
	}
	disabled := &http.Transport{MaxIdleConnsPerHost: -1}
	applyAntigravityPoolLimits(disabled)
	if disabled.MaxIdleConnsPerHost != -1 {
		t.Fatalf("disabled idle pooling was overwritten: %#v", disabled)
	}
}

func TestServerSeparatesAntigravityHTTP11FromDefaultClientThroughSameProxy(t *testing.T) {
	type observedRequest struct {
		path     string
		protocol int
	}
	observed := make(chan observedRequest, 2)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed <- observedRequest{path: r.URL.Path, protocol: r.ProtoMajor}
		_, _ = io.WriteString(w, "ok")
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	t.Cleanup(upstream.Close)

	connectTargets := make(chan string, 2)
	proxyAuthorizations := make(chan string, 2)
	proxyErrors := make(chan error, 2)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			proxyErrors <- fmt.Errorf("proxy method = %s, want CONNECT", r.Method)
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		connectTargets <- r.Host
		proxyAuthorizations <- r.Header.Get("Proxy-Authorization")
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			proxyErrors <- fmt.Errorf("proxy response writer cannot hijack")
			return
		}
		clientConn, buffered, err := hijacker.Hijack()
		if err != nil {
			proxyErrors <- fmt.Errorf("hijack proxy connection: %w", err)
			return
		}
		upstreamConn, err := net.Dial("tcp", upstream.Listener.Addr().String())
		if err != nil {
			_ = clientConn.Close()
			proxyErrors <- fmt.Errorf("dial test upstream: %w", err)
			return
		}
		if _, err = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
			_ = clientConn.Close()
			_ = upstreamConn.Close()
			proxyErrors <- fmt.Errorf("write CONNECT response: %w", err)
			return
		}
		if err = buffered.Flush(); err != nil {
			_ = clientConn.Close()
			_ = upstreamConn.Close()
			proxyErrors <- fmt.Errorf("flush CONNECT response: %w", err)
			return
		}
		go func() {
			_, _ = io.Copy(upstreamConn, buffered)
			_ = upstreamConn.Close()
		}()
		_, _ = io.Copy(clientConn, upstreamConn)
		_ = clientConn.Close()
	}))
	t.Cleanup(proxy.Close)

	server := &Server{skipTLSVerify: true}
	t.Cleanup(func() {
		server.proxyTransports.Range(func(_, value any) bool {
			closeUpstreamHTTPClient(value.(*http.Client))
			return true
		})
	})
	proxyURL := strings.Replace(proxy.URL, "http://", "http://proxy:secret@", 1)
	request := func(config *model.Config, path string) {
		t.Helper()
		req, err := http.NewRequestWithContext(
			context.Background(), http.MethodPost, "https://upstream.example"+path, bytes.NewBufferString(`{"request":{}}`),
		)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := server.getClientForChannel(config).Do(req)
		if err != nil {
			t.Fatalf("POST %s through proxy: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		if err = resp.Body.Close(); err != nil {
			t.Fatalf("close %s response: %v", path, err)
		}
	}

	request(&model.Config{ID: 1, ProxyURL: proxyURL}, "/default")
	request(&model.Config{ID: 2, AuthType: model.AuthTypeAntigravityOAuth, ProxyURL: proxyURL}, "/antigravity")

	protocols := make(map[string]int, 2)
	for range 2 {
		got := <-observed
		protocols[got.path] = got.protocol
	}
	if got := protocols["/default"]; got != 2 {
		t.Fatalf("default proxied protocol = HTTP/%d, want HTTP/2", got)
	}
	if got := protocols["/antigravity"]; got != 1 {
		t.Fatalf("Antigravity proxied protocol = HTTP/%d, want HTTP/1.1", got)
	}
	for range 2 {
		if got := <-connectTargets; got != "upstream.example:443" {
			t.Fatalf("CONNECT target = %q, want upstream.example:443", got)
		}
		if got := <-proxyAuthorizations; got != "Basic cHJveHk6c2VjcmV0" {
			t.Fatalf("Proxy-Authorization = %q, want proxy credentials", got)
		}
	}
	select {
	case err := <-proxyErrors:
		t.Fatal(err)
	default:
	}
}

func TestUpstreamHTTPTransportClosesIdleConnectionAtMaxAge(t *testing.T) {
	closed := make(chan struct{}, 2)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	server.StartTLS()
	defer server.Close()

	transport := newUpstreamConnectionAgeTransport(buildHTTPTransport(true), 50*time.Millisecond)
	client := &http.Client{Transport: transport}
	t.Cleanup(transport.Close)

	doRequest := func() httptrace.GotConnInfo {
		t.Helper()
		gotConn := make(chan httptrace.GotConnInfo, 1)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { gotConn <- info },
		}))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		if _, err = io.Copy(io.Discard, resp.Body); err != nil {
			_ = resp.Body.Close()
			t.Fatalf("read response: %v", err)
		}
		if err = resp.Body.Close(); err != nil {
			t.Fatalf("close response: %v", err)
		}
		return <-gotConn
	}

	first := doRequest()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("idle upstream connection was not closed after max age")
	}
	second := doRequest()
	if first.Conn == second.Conn || second.Reused {
		t.Fatalf("aged connection was reused: same=%t reused=%t", first.Conn == second.Conn, second.Reused)
	}
}

func TestUpstreamHTTPTransportClosesIdleCodexUTLSConnectionAtMaxAge(t *testing.T) {
	closed := make(chan struct{}, 2)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	server.EnableHTTP2 = true
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	server.StartTLS()
	defer server.Close()

	base := buildHTTPTransport(true)
	dialer := &net.Dialer{}
	base.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, server.Listener.Addr().String())
	}
	transport := newUpstreamConnectionAgeTransport(base, 50*time.Millisecond)
	client := &http.Client{Transport: transport}
	t.Cleanup(transport.Close)

	doRequest := func() {
		t.Helper()
		req, err := http.NewRequestWithContext(
			context.Background(),
			http.MethodGet,
			"https://chatgpt.com/backend-api/codex/responses",
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		if err = resp.Body.Close(); err != nil {
			t.Fatalf("close response: %v", err)
		}
	}

	doRequest()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("idle Codex uTLS connection was not closed after max age")
	}
	doRequest()
}

func TestNewServerAppliesUpstreamConnectionMaxAgeToHTTP(t *testing.T) {
	t.Setenv("CCLOAD_PASS", "upstream-connection-age-test-password")

	store, err := storage.CreateSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("CreateSQLiteStore: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err = store.UpdateSetting(ctx, "upstream_connection_reuse_limit_seconds", "1"); err != nil {
		_ = store.Close()
		t.Fatalf("UpdateSetting: %v", err)
	}
	server := NewServer(store)
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutdownCancel()
		if errShutdown := server.Shutdown(shutdownCtx); errShutdown != nil {
			t.Errorf("Shutdown: %v", errShutdown)
		}
	})

	closed := make(chan struct{}, 1)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	upstream.Start()
	defer upstream.Close()

	resp, err := server.client.Get(upstream.URL)
	if err != nil {
		t.Fatalf("GET upstream: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if err = resp.Body.Close(); err != nil {
		t.Fatalf("close response: %v", err)
	}

	select {
	case <-closed:
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("NewServer HTTP client did not apply upstream connection max age")
	}
}

func TestUpstreamHTTPTransportDrainsActiveResponsesAndRotatesNewRequests(t *testing.T) {
	for _, enableHTTP2 := range []bool{false, true} {
		name := "http1"
		if enableHTTP2 {
			name = "http2"
		}
		t.Run(name, func(t *testing.T) {
			releaseSlow := make(chan struct{})
			var releaseOnce sync.Once
			t.Cleanup(func() { releaseOnce.Do(func() { close(releaseSlow) }) })
			protocols := make(chan int, 2)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				protocols <- r.ProtoMajor
				if r.URL.Path == "/slow" {
					w.WriteHeader(http.StatusOK)
					if flusher, ok := w.(http.Flusher); ok {
						flusher.Flush()
					}
					<-releaseSlow
				}
				_, _ = io.WriteString(w, r.URL.Path)
			}))
			server.EnableHTTP2 = enableHTTP2
			server.StartTLS()
			defer server.Close()

			transport := newUpstreamConnectionAgeTransport(buildHTTPTransport(true), 50*time.Millisecond)
			client := &http.Client{Transport: transport}
			t.Cleanup(transport.Close)

			requestWithTrace := func(path string) (*http.Response, httptrace.GotConnInfo) {
				t.Helper()
				gotConn := make(chan httptrace.GotConnInfo, 1)
				req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
					GotConn: func(info httptrace.GotConnInfo) { gotConn <- info },
				}))
				resp, err := client.Do(req)
				if err != nil {
					t.Fatalf("GET %s: %v", path, err)
				}
				return resp, <-gotConn
			}

			slowResp, slowConn := requestWithTrace("/slow")
			// Let the max-age timer retire the generation while /slow remains active.
			<-time.After(150 * time.Millisecond)
			fastResp, fastConn := requestWithTrace("/fast")
			fastBody, err := io.ReadAll(fastResp.Body)
			if err != nil {
				_ = fastResp.Body.Close()
				t.Fatalf("read fast response: %v", err)
			}
			_ = fastResp.Body.Close()
			if string(fastBody) != "/fast" {
				t.Fatalf("fast body=%q, want /fast", fastBody)
			}
			if slowConn.Conn == fastConn.Conn || fastConn.Reused {
				t.Fatalf("new request used retired connection: same=%t reused=%t", slowConn.Conn == fastConn.Conn, fastConn.Reused)
			}

			releaseOnce.Do(func() { close(releaseSlow) })
			slowBody, err := io.ReadAll(slowResp.Body)
			if err != nil {
				_ = slowResp.Body.Close()
				t.Fatalf("active response was interrupted: %v", err)
			}
			_ = slowResp.Body.Close()
			if string(slowBody) != "/slow" {
				t.Fatalf("slow body=%q, want /slow", slowBody)
			}

			wantProtocol := 1
			if enableHTTP2 {
				wantProtocol = 2
			}
			for range 2 {
				if got := <-protocols; got != wantProtocol {
					t.Fatalf("protocol=%d, want HTTP/%d", got, wantProtocol)
				}
			}
		})
	}
}

func TestServerAppliesUpstreamConnectionMaxAgeToChannelProxy(t *testing.T) {
	closed := make(chan struct{}, 2)
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "proxied")
	}))
	proxy.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	proxy.Start()
	defer proxy.Close()

	server := &Server{upstreamConnectionMaxAge: 50 * time.Millisecond}
	config := &model.Config{ID: 1, ProxyURL: proxy.URL}
	client := server.getClientForChannel(config)
	t.Cleanup(func() {
		server.proxyTransports.Range(func(_, value any) bool {
			closeUpstreamHTTPClient(value.(*http.Client))
			return true
		})
	})

	resp, err := client.Get("http://upstream.invalid/test")
	if err != nil {
		t.Fatalf("GET through channel proxy: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || string(body) != "proxied" {
		t.Fatalf("proxy response body=%q err=%v", body, err)
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("channel proxy connection was not closed after max age")
	}
}
