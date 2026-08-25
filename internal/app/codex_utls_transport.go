package app

import (
	"bufio"
	"context"
	cryptotls "crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"time"

	"ccLoad/internal/config"
	"ccLoad/internal/httpwire"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// The protected-host routing and Chrome ClientHello profile follow
// CLIProxyAPI's MIT-licensed uTLS transport (copyright Router-For.ME), pinned
// by this repository at 2b9a9d23d2226efdea25d9710f217208cb52ff8b.
const (
	codexUTLSH2MaxConnsPerHost = 8
	codexUTLSH2DegradeInitial  = 2 * time.Minute
	codexUTLSH2DegradeMax      = 30 * time.Minute
)

type codexUTLSRoundTripper struct {
	fallback   *http.Transport
	h2         *http2.Transport
	h1         *http.Transport
	anthropic  *http.Transport
	standardH1 *http.Transport

	degradeMu sync.Mutex
	degraded  map[string]h2DegradeState
}

type h2DegradeState struct {
	till time.Time
	ttl  time.Duration
}

func newCodexUTLSRoundTripper(base *http.Transport) *codexUTLSRoundTripper {
	if base == nil {
		base = http.DefaultTransport.(*http.Transport).Clone()
	}
	return &codexUTLSRoundTripper{
		fallback:   base,
		h2:         newCodexUTLSH2Transport(base),
		h1:         newCodexUTLSHTTP11Transport(base),
		anthropic:  newAnthropicNodeHTTP11Transport(base),
		standardH1: newStandardHTTP11Transport(base),
		degraded:   make(map[string]h2DegradeState, 2),
	}
}

type chromeUTLSContextKey struct{}

func withChromeUTLS(req *http.Request) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), chromeUTLSContextKey{}, struct{}{}))
}

func (t *codexUTLSRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if isChromeUTLSRequest(req) {
		return t.roundTripProtected(req)
	}
	if isAnthropicNodeUTLSRequest(req) {
		return t.anthropic.RoundTrip(req)
	}
	return t.fallback.RoundTrip(req)
}

func isChromeUTLSRequest(req *http.Request) bool {
	if req == nil || req.URL == nil || req.URL.Scheme != "https" {
		return false
	}
	if req.Context().Value(chromeUTLSContextKey{}) != nil {
		return true
	}
	host := req.URL.Hostname()
	return strings.EqualFold(host, "chatgpt.com") || strings.EqualFold(host, "claude.ai") ||
		strings.EqualFold(host, "platform.claude.com")
}

func isAnthropicNodeUTLSRequest(req *http.Request) bool {
	return req != nil && req.URL != nil && req.URL.Scheme == "https" &&
		strings.EqualFold(req.URL.Hostname(), "api.anthropic.com")
}

func (t *codexUTLSRoundTripper) roundTripProtected(req *http.Request) (*http.Response, error) {
	host := strings.ToLower(req.URL.Hostname())
	transports := []struct {
		transport http.RoundTripper
		h2        bool
	}{
		{transport: t.h2, h2: true},
		{transport: t.h1},
		{transport: t.standardH1},
	}

	var lastErr error
	sent := 0
	for _, candidate := range transports {
		if candidate.h2 && t.h2Degraded(host) {
			continue
		}
		attempt, err := requestForCodexUTLSAttempt(req, sent)
		if err != nil {
			return nil, err
		}
		resp, roundTripErr := candidate.transport.RoundTrip(attempt)
		sent++
		if candidate.h2 {
			if resp != nil {
				t.recordH2Success(host)
			} else if roundTripErr != nil && req.Context().Err() == nil {
				t.recordH2Failure(host)
			}
		}
		if resp != nil {
			return resp, roundTripErr
		}
		if roundTripErr != nil {
			lastErr = roundTripErr
			if req.Context().Err() != nil {
				return nil, req.Context().Err()
			}
			continue
		}
		return nil, nil
	}
	return nil, lastErr
}

func requestForCodexUTLSAttempt(req *http.Request, attempt int) (*http.Request, error) {
	if attempt == 0 || req.Body == nil || req.Body == http.NoBody {
		return req, nil
	}
	if req.GetBody == nil {
		return nil, errors.New("codex uTLS fallback requires a replayable request body")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, fmt.Errorf("rewind Codex uTLS request body: %w", err)
	}
	cloned := req.Clone(req.Context())
	cloned.Body = body
	return cloned, nil
}

func (t *codexUTLSRoundTripper) h2Degraded(host string) bool {
	t.degradeMu.Lock()
	defer t.degradeMu.Unlock()
	return time.Now().Before(t.degraded[host].till)
}

func (t *codexUTLSRoundTripper) recordH2Failure(host string) {
	t.degradeMu.Lock()
	defer t.degradeMu.Unlock()
	state := t.degraded[host]
	ttl := codexUTLSH2DegradeInitial
	if state.ttl > 0 {
		ttl = min(state.ttl*2, codexUTLSH2DegradeMax)
	}
	t.degraded[host] = h2DegradeState{ttl: ttl, till: time.Now().Add(ttl)}
}

func (t *codexUTLSRoundTripper) recordH2Success(host string) {
	t.degradeMu.Lock()
	delete(t.degraded, host)
	t.degradeMu.Unlock()
}

func (t *codexUTLSRoundTripper) CloseIdleConnections() {
	if t == nil {
		return
	}
	t.h2.CloseIdleConnections()
	t.h1.CloseIdleConnections()
	t.anthropic.CloseIdleConnections()
	t.standardH1.CloseIdleConnections()
	t.fallback.CloseIdleConnections()
}

func newCodexUTLSH2Transport(base *http.Transport) *http2.Transport {
	dialer := newCodexUTLSDialer(base)
	maxConns := codexUTLSH2MaxConnsPerHost
	if base.MaxConnsPerHost > 0 {
		maxConns = min(maxConns, base.MaxConnsPerHost)
	}
	limiter := newCodexUTLSConnectionLimiter(maxConns)
	return &http2.Transport{
		StrictMaxConcurrentStreams: false,
		TLSClientConfig:            cloneTLSConfig(base.TLSClientConfig, []string{"h2"}),
		DisableCompression:         base.DisableCompression,
		IdleConnTimeout:            base.IdleConnTimeout,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *cryptotls.Config) (net.Conn, error) {
			release, err := limiter.acquire(ctx, addr)
			if err != nil {
				return nil, err
			}
			rawConn, err := dialer(ctx, network, addr)
			if err != nil {
				release()
				return nil, err
			}
			rawConn = &releaseOnCloseConn{Conn: rawConn, release: release}
			return dialCodexUTLS(ctx, rawConn, addr, base, []string{"h2"}, false)
		},
	}
}

func newCodexUTLSHTTP11Transport(base *http.Transport) *http.Transport {
	dialer := newCodexUTLSDialer(base)
	transport := base.Clone()
	transport.Proxy = nil
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = make(map[string]func(string, *cryptotls.Conn) http.RoundTripper)
	transport.TLSClientConfig = cloneTLSConfig(base.TLSClientConfig, []string{"http/1.1"})
	transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		rawConn, err := dialer(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return dialCodexUTLS(ctx, rawConn, addr, base, []string{"http/1.1"}, true)
	}
	return transport
}

func newAnthropicNodeHTTP11Transport(base *http.Transport) *http.Transport {
	dialer := newCodexUTLSDialer(base)
	transport := base.Clone()
	transport.Proxy = nil
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = make(map[string]func(string, *cryptotls.Conn) http.RoundTripper)
	transport.TLSClientConfig = cloneTLSConfig(base.TLSClientConfig, []string{"http/1.1"})
	transport.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		rawConn, err := dialer(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return dialAnthropicNodeUTLS(ctx, rawConn, addr, base)
	}
	return transport
}

func newStandardHTTP11Transport(base *http.Transport) *http.Transport {
	transport := base.Clone()
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = make(map[string]func(string, *cryptotls.Conn) http.RoundTripper)
	transport.TLSClientConfig = cloneTLSConfig(base.TLSClientConfig, []string{"http/1.1"})
	return transport
}

func cloneTLSConfig(base *cryptotls.Config, nextProtos []string) *cryptotls.Config {
	if base == nil {
		base = &cryptotls.Config{MinVersion: cryptotls.VersionTLS12}
	} else {
		base = base.Clone()
	}
	base.NextProtos = append([]string(nil), nextProtos...)
	return base
}

func dialCodexUTLS(
	ctx context.Context,
	rawConn net.Conn,
	addr string,
	base *http.Transport,
	alpn []string,
	dropApplicationSettings bool,
) (net.Conn, error) {
	spec, err := codexChromeClientHelloSpec(alpn, dropApplicationSettings)
	if err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("build Codex uTLS ClientHello: %w", err)
	}
	return dialCustomUTLS(ctx, rawConn, addr, base, alpn, spec, "codex")
}

func dialAnthropicNodeUTLS(
	ctx context.Context,
	rawConn net.Conn,
	addr string,
	base *http.Transport,
) (net.Conn, error) {
	conn, err := dialCustomUTLS(
		ctx, rawConn, addr, base, []string{"http/1.1"}, anthropicNodeClientHelloSpec(), "anthropic",
	)
	if err != nil {
		return nil, err
	}
	return httpwire.NewOrderedRequestConn(conn, anthropicClaudeCodeHeaderOrder), nil
}

var anthropicClaudeCodeMessagesHeaderOrder = []string{
	"Accept",
	"Authorization",
	"Content-Type",
	"User-Agent",
	"X-Claude-Code-Session-Id",
	"X-Stainless-Arch",
	"X-Stainless-Lang",
	"X-Stainless-OS",
	"X-Stainless-Package-Version",
	"X-Stainless-Retry-Count",
	"X-Stainless-Runtime",
	"X-Stainless-Runtime-Version",
	"X-Stainless-Timeout",
	"anthropic-beta",
	"anthropic-dangerous-direct-browser-access",
	"anthropic-version",
	"x-app",
	"x-client-request-id",
	"Connection",
	"Host",
	"Accept-Encoding",
	"Content-Length",
}

func anthropicClaudeCodeHeaderOrder(_, requestTarget string) []string {
	requestPath := requestTarget
	if query := strings.IndexByte(requestPath, '?'); query >= 0 {
		requestPath = requestPath[:query]
	}
	requestPath = strings.TrimSuffix(requestPath, "/")
	if requestPath == "/v1/messages" || requestPath == "/messages" {
		return anthropicClaudeCodeMessagesHeaderOrder
	}
	return nil
}

func dialCustomUTLS(
	ctx context.Context,
	rawConn net.Conn,
	addr string,
	base *http.Transport,
	alpn []string,
	spec utls.ClientHelloSpec,
	provider string,
) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("split %s uTLS address: %w", provider, err)
	}
	utlsConfig := &utls.Config{
		ServerName: host,
		NextProtos: append([]string(nil), alpn...),
		MinVersion: utls.VersionTLS12,
	}
	if base.TLSClientConfig != nil {
		if base.TLSClientConfig.ServerName != "" {
			utlsConfig.ServerName = base.TLSClientConfig.ServerName
		}
		utlsConfig.InsecureSkipVerify = base.TLSClientConfig.InsecureSkipVerify //nolint:gosec // 继承显式上游 TLS 配置
		utlsConfig.RootCAs = base.TLSClientConfig.RootCAs
		utlsConfig.MinVersion = base.TLSClientConfig.MinVersion
		utlsConfig.MaxVersion = base.TLSClientConfig.MaxVersion
		utlsConfig.VerifyPeerCertificate = base.TLSClientConfig.VerifyPeerCertificate
	}
	tlsConn := utls.UClient(rawConn, utlsConfig, utls.HelloCustom)
	if err = tlsConn.ApplyPreset(&spec); err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("apply %s uTLS ClientHello: %w", provider, err)
	}
	handshakeCtx := ctx
	cancel := func() {}
	if base.TLSHandshakeTimeout > 0 {
		handshakeCtx, cancel = context.WithTimeout(ctx, base.TLSHandshakeTimeout)
	}
	defer cancel()
	if err = tlsConn.HandshakeContext(handshakeCtx); err != nil {
		_ = tlsConn.Close()
		return nil, fmt.Errorf("%s uTLS handshake: %w", provider, err)
	}
	if len(alpn) == 1 && tlsConn.ConnectionState().NegotiatedProtocol != alpn[0] {
		negotiated := tlsConn.ConnectionState().NegotiatedProtocol
		_ = tlsConn.Close()
		return nil, fmt.Errorf("%s uTLS negotiated ALPN %q, want %q", provider, negotiated, alpn[0])
	}
	return tlsConn, nil
}

// anthropicNodeClientHelloSpec is sub2api's built-in Node.js 24.x profile.
// It is used there when TLS fingerprinting is enabled without an account profile.
func anthropicNodeClientHelloSpec() utls.ClientHelloSpec {
	return utls.ClientHelloSpec{
		CipherSuites: []uint16{
			0x1301, 0x1302, 0x1303,
			0xc02b, 0xc02f, 0xc02c, 0xc030,
			0xcca9, 0xcca8,
			0xc009, 0xc013, 0xc00a, 0xc014,
			0x009c, 0x009d, 0x002f, 0x0035,
		},
		CompressionMethods: []uint8{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.GREASEEncryptedClientHelloExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519, utls.CurveP256, utls.CurveP384}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601, 0x0201,
			}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{uint8(utls.PskModeDHE)}},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
		},
		TLSVersMax: utls.VersionTLS13,
		TLSVersMin: utls.VersionTLS10,
	}
}

func codexChromeClientHelloSpec(alpn []string, dropApplicationSettings bool) (utls.ClientHelloSpec, error) {
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
	if err != nil {
		return utls.ClientHelloSpec{}, err
	}
	extensions := spec.Extensions[:0]
	alpnSet := false
	for _, extension := range spec.Extensions {
		switch extension.(type) {
		case *utls.ALPNExtension:
			extensions = append(extensions, &utls.ALPNExtension{AlpnProtocols: append([]string(nil), alpn...)})
			alpnSet = true
		case *utls.ApplicationSettingsExtension, *utls.ApplicationSettingsExtensionNew:
			if !dropApplicationSettings {
				extensions = append(extensions, extension)
			}
		default:
			extensions = append(extensions, extension)
		}
	}
	if !alpnSet {
		extensions = append(extensions, &utls.ALPNExtension{AlpnProtocols: append([]string(nil), alpn...)})
	}
	spec.Extensions = extensions
	return spec, nil
}

type codexUTLSConnectionLimiter struct {
	mu       sync.Mutex
	capacity int
	slots    map[string]chan struct{}
}

func newCodexUTLSConnectionLimiter(capacity int) *codexUTLSConnectionLimiter {
	return &codexUTLSConnectionLimiter{capacity: capacity, slots: make(map[string]chan struct{})}
}

func (l *codexUTLSConnectionLimiter) acquire(ctx context.Context, addr string) (func(), error) {
	l.mu.Lock()
	slots := l.slots[addr]
	if slots == nil {
		slots = make(chan struct{}, l.capacity)
		l.slots[addr] = slots
	}
	l.mu.Unlock()

	select {
	case slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-slots }) }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type releaseOnCloseConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *releaseOnCloseConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

type codexUTLSDialContext func(context.Context, string, string) (net.Conn, error)

func newCodexUTLSDialer(base *http.Transport) codexUTLSDialContext {
	baseDial := base.DialContext
	if baseDial == nil {
		dialer := &net.Dialer{Timeout: config.HTTPDialTimeout, KeepAlive: config.HTTPKeepAliveInterval}
		baseDial = dialer.DialContext
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if base.Proxy == nil {
			return baseDial(ctx, network, addr)
		}
		proxyURL, err := base.Proxy(&http.Request{URL: &neturl.URL{Scheme: "https", Host: addr}})
		if err != nil {
			return nil, fmt.Errorf("resolve Codex uTLS proxy: %w", err)
		}
		if proxyURL == nil {
			return baseDial(ctx, network, addr)
		}
		switch strings.ToLower(proxyURL.Scheme) {
		case "http", "https":
			return dialCodexHTTPProxy(ctx, baseDial, network, proxyURL, addr, base)
		case "socks5", "socks5h":
			dial, err := newSOCKS5Dialer(proxyURL)
			if err != nil {
				return nil, err
			}
			return dial(ctx, network, addr)
		default:
			return nil, fmt.Errorf("unsupported Codex uTLS proxy scheme %q", proxyURL.Scheme)
		}
	}
}

func dialCodexHTTPProxy(
	ctx context.Context,
	dial codexUTLSDialContext,
	network string,
	proxyURL *neturl.URL,
	targetAddr string,
	base *http.Transport,
) (net.Conn, error) {
	conn, err := dial(ctx, network, proxyDialAddress(proxyURL))
	if err != nil {
		return nil, fmt.Errorf("dial Codex HTTP proxy: %w", err)
	}
	cancelConn := conn
	cancelDone := make(chan struct{})
	stopCancel := context.AfterFunc(ctx, func() {
		_ = cancelConn.Close()
		close(cancelDone)
	})
	defer func() {
		if !stopCancel() {
			<-cancelDone
		}
	}()

	if strings.EqualFold(proxyURL.Scheme, "https") {
		proxyTLS := cloneTLSConfig(base.TLSClientConfig, nil)
		proxyTLS.ServerName = proxyURL.Hostname()
		proxyConn := cryptotls.Client(conn, proxyTLS)
		handshakeCtx := ctx
		cancel := func() {}
		if base.TLSHandshakeTimeout > 0 {
			handshakeCtx, cancel = context.WithTimeout(ctx, base.TLSHandshakeTimeout)
		}
		err = proxyConn.HandshakeContext(handshakeCtx)
		cancel()
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("codex HTTPS proxy handshake: %w", err)
		}
		conn = proxyConn
	}

	connectReq := (&http.Request{
		Method: http.MethodConnect,
		URL:    &neturl.URL{Host: targetAddr},
		Host:   targetAddr,
		Header: make(http.Header),
	}).WithContext(ctx)
	if proxyURL.User != nil {
		username := proxyURL.User.Username()
		password, _ := proxyURL.User.Password()
		credential := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
		connectReq.Header.Set("Proxy-Authorization", "Basic "+credential)
	}
	if err = connectReq.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write Codex proxy CONNECT: %w", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, connectReq)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read Codex proxy CONNECT: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("codex proxy CONNECT returned %s", resp.Status)
	}
	if err = ctx.Err(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if reader.Buffered() > 0 {
		return &bufferedProxyConn{Conn: conn, reader: reader}, nil
	}
	return conn, nil
}

func proxyDialAddress(proxyURL *neturl.URL) string {
	port := proxyURL.Port()
	if port == "" {
		port = "80"
		if strings.EqualFold(proxyURL.Scheme, "https") {
			port = "443"
		}
	}
	return net.JoinHostPort(proxyURL.Hostname(), port)
}

type bufferedProxyConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedProxyConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
