package app

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/antigravityauth"
	"ccLoad/internal/util"
)

var errUpstreamConnectionAgeTransportClosed = errors.New("upstream connection age transport is closed")

// upstreamConnectionAgeTransport rotates complete HTTP transport generations.
// That is the only safe boundary shared by HTTP/1.1 and multiplexed HTTP/2:
// an expired generation receives no new requests, while active responses drain
// before its remaining idle connections are closed.
type upstreamConnectionAgeTransport struct {
	mu       sync.Mutex
	template *http.Transport
	factory  upstreamRoundTripperFactory
	current  *upstreamHTTPTransportGeneration
	maxAge   time.Duration
	closed   bool
}

type upstreamHTTPTransportGeneration struct {
	transport upstreamRoundTripper
	timer     *time.Timer
	active    int
	retired   bool
}

type upstreamRoundTripper interface {
	http.RoundTripper
	CloseIdleConnections()
}

type upstreamRoundTripperFactory func(*http.Transport) upstreamRoundTripper

func newUpstreamConnectionAgeTransportWithFactory(
	base *http.Transport,
	maxAge time.Duration,
	factory upstreamRoundTripperFactory,
) *upstreamConnectionAgeTransport {
	transport := &upstreamConnectionAgeTransport{
		template: base.Clone(),
		factory:  factory,
		maxAge:   maxAge,
	}
	transport.mu.Lock()
	transport.startGenerationLocked(base)
	transport.mu.Unlock()
	return transport
}

func newUpstreamHTTPClient(base *http.Transport, maxAge time.Duration) *http.Client {
	return newHTTPClientWithRoundTripperFactory(base, maxAge, newDefaultUpstreamRoundTripper)
}

func newAntigravityHTTPClient(base *http.Transport, maxAge time.Duration) *http.Client {
	return newHTTPClientWithRoundTripperFactory(base.Clone(), maxAge, newAntigravityUpstreamRoundTripper)
}

func newHTTPClientWithRoundTripperFactory(
	base *http.Transport,
	maxAge time.Duration,
	factory upstreamRoundTripperFactory,
) *http.Client {
	// Keep the retirement gate even when connection-age rotation is disabled:
	// an evicted client may still be held by a caller, but must not rebuild a pool.
	roundTripper := newUpstreamConnectionAgeTransportWithFactory(base, maxAge, factory)
	return &http.Client{Transport: roundTripper, Timeout: 0, CheckRedirect: refuseCrossOriginRedirect}
}

// refuseCrossOriginRedirect keeps upstream credentials on the requested origin.
// net/http rebuilds every hop from the original headers and strips only
// Authorization and cookies, so x-api-key, x-goog-api-key and channel custom
// headers would follow a redirect to any host. A cross-origin hop fails as an
// invalid upstream response so the proxy switches channels instead of relaying
// the redirect to the client.
func refuseCrossOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	// Every earlier hop already passed this check, so the first request is the
	// only origin to compare against.
	if !sameURLOrigin(via[0].URL, req.URL) {
		return fmt.Errorf("%w: redirect to another origin %s://%s", util.ErrUpstreamInvalidResponse, req.URL.Scheme, req.URL.Host)
	}
	return nil
}

func sameURLOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) &&
		strings.EqualFold(a.Hostname(), b.Hostname()) &&
		urlOriginPort(a) == urlOriginPort(b)
}

func urlOriginPort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

func newDefaultUpstreamRoundTripper(base *http.Transport) upstreamRoundTripper {
	return newCodexUTLSRoundTripper(base)
}

/**
 * @brief 在同一账号及代理作用域内分配 Antigravity 的 Cloud Code 和 OAuth token 连接池。
 * @param base 要继承代理、TLS 和连接池配置的 Transport。
 * @return 当前连接池代使用的传输。Cloud Code 用无 ALPN 的 HTTP/1.1，OAuth token 由标准库协商协议。
 */
func newAntigravityUpstreamRoundTripper(base *http.Transport) upstreamRoundTripper {
	transport := newStandardHTTP11Transport(base)
	transport.Protocols = nil
	transport.TLSClientConfig.NextProtos = nil
	return &antigravityUpstreamRoundTripper{cloudCode: transport, oauth: base}
}

/**
 * @brief 管理同一连接池代中的 Antigravity 两种传输。
 */
type antigravityUpstreamRoundTripper struct {
	cloudCode *http.Transport ///< Cloud Code 与 Antigravity 元数据请求的连接池。
	oauth     *http.Transport ///< OAuth token 请求的连接池。
}

/**
 * @brief 按服务端点的传输要求发送 Antigravity 请求。
 * @param req 已构造的上游 HTTP 请求。
 * @return 上游 HTTP 响应，发送失败时为 nil。
 * @return 传输或协议协商失败时的错误，原样保留底层错误链。
 */
func (t *antigravityUpstreamRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if antigravityauth.IsTokenRequest(req) {
		return t.oauth.RoundTrip(req)
	}
	return t.cloudCode.RoundTrip(req)
}

/**
 * @brief 关闭当前连接池代的两组空闲连接，活动请求继续完成。
 */
func (t *antigravityUpstreamRoundTripper) CloseIdleConnections() {
	t.cloudCode.CloseIdleConnections()
	t.oauth.CloseIdleConnections()
}

func closeUpstreamHTTPClient(client *http.Client) {
	if client == nil {
		return
	}
	if closer, ok := client.Transport.(interface{ Close() }); ok {
		closer.Close()
		return
	}
	client.CloseIdleConnections()
}

func (t *upstreamConnectionAgeTransport) startGenerationLocked(base *http.Transport) {
	generation := &upstreamHTTPTransportGeneration{transport: t.factory(base)}
	if t.maxAge > 0 {
		generation.timer = time.AfterFunc(t.maxAge, func() {
			t.retire(generation)
		})
	}
	t.current = generation
}

func (t *upstreamConnectionAgeTransport) acquire() (*upstreamHTTPTransportGeneration, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, errUpstreamConnectionAgeTransportClosed
	}
	if t.current == nil {
		t.startGenerationLocked(t.template.Clone())
	}
	t.current.active++
	return t.current, nil
}

func (t *upstreamConnectionAgeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	generation, err := t.acquire()
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	resp, err := generation.transport.RoundTrip(req)
	if err != nil {
		t.release(generation)
		return nil, err
	}
	if resp == nil || resp.Body == nil {
		t.release(generation)
		return resp, nil
	}
	resp.Body = &releaseOnCloseReadCloser{
		ReadCloser: resp.Body,
		release:    func() { t.release(generation) },
	}
	return resp, nil
}

func (t *upstreamConnectionAgeTransport) retire(generation *upstreamHTTPTransportGeneration) {
	t.mu.Lock()
	if generation.retired {
		t.mu.Unlock()
		return
	}
	generation.retired = true
	if t.current == generation {
		t.current = nil
	}
	t.mu.Unlock()

	// CloseIdleConnections never interrupts active HTTP/1.1 requests or HTTP/2
	// streams. release calls it again after the last active response drains.
	generation.transport.CloseIdleConnections()
}

func (t *upstreamConnectionAgeTransport) release(generation *upstreamHTTPTransportGeneration) {
	t.mu.Lock()
	if generation.active > 0 {
		generation.active--
	}
	closeIdle := generation.retired && generation.active == 0
	t.mu.Unlock()
	if closeIdle {
		generation.transport.CloseIdleConnections()
	}
}

func (t *upstreamConnectionAgeTransport) CloseIdleConnections() {
	t.mu.Lock()
	generation := t.current
	t.mu.Unlock()
	if generation != nil {
		generation.transport.CloseIdleConnections()
	}
}

// Close stops future rotations and closes every idle connection owned by the
// current generation. Retired generations already close on retirement/drain.
func (t *upstreamConnectionAgeTransport) Close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	generation := t.current
	t.current = nil
	if generation != nil {
		generation.retired = true
		if generation.timer != nil {
			generation.timer.Stop()
		}
	}
	t.mu.Unlock()
	if generation != nil {
		generation.transport.CloseIdleConnections()
	}
}
