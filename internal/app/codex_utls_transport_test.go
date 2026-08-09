package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestUpstreamHTTPClientUsesChromeUTLSForProtectedWebOrigins(t *testing.T) {
	for _, targetURL := range []string{
		"https://chatgpt.com/backend-api/codex/responses",
		"https://claude.ai/api/organizations",
		"https://platform.claude.com/v1/oauth/token",
	} {
		t.Run(targetURL, func(t *testing.T) {
			protocol := make(chan int, 1)
			upstream, captured := newCapturedTLSServer(t, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				protocol <- r.ProtoMajor
				_, _ = io.WriteString(w, "ok")
			}))

			base := buildHTTPTransport(true)
			dialer := &net.Dialer{}
			base.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, upstream.Listener.Addr().String())
			}
			client := newUpstreamHTTPClient(base, 0)
			t.Cleanup(func() { closeUpstreamHTTPClient(client) })

			req, err := http.NewRequestWithContext(
				context.Background(), http.MethodPost, targetURL, bytes.NewBufferString(`{"input":"hello"}`),
			)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("send request: %v", err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			if err = resp.Body.Close(); err != nil {
				t.Fatalf("close response: %v", err)
			}

			if got := <-protocol; got != 2 {
				t.Fatalf("upstream protocol = HTTP/%d, want HTTP/2", got)
			}
			if !clientHelloHasGREASECipherSuite(captured.Bytes()) {
				t.Fatal("protected origin TLS ClientHello has no GREASE cipher suite; Chrome uTLS fingerprint was not used")
			}
		})
	}
}

func TestUpstreamHTTPClientUsesNodeUTLSHTTP11ForAnthropicAPI(t *testing.T) {
	protocol := make(chan int, 1)
	upstream, captured := newCapturedTLSServer(t, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocol <- r.ProtoMajor
		_, _ = io.WriteString(w, "ok")
	}))

	base := buildHTTPTransport(true)
	dialer := &net.Dialer{}
	base.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	client := newUpstreamHTTPClient(base, 0)
	t.Cleanup(func() { closeUpstreamHTTPClient(client) })

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{"messages":[]}`),
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	if got := <-protocol; got != 1 {
		t.Fatalf("Anthropic protocol = HTTP/%d, want HTTP/1.1", got)
	}
	if clientHelloHasGREASECipherSuite(captured.Bytes()) {
		t.Fatal("Anthropic API used the Chrome cipher profile instead of the Node.js profile")
	}
}

func TestProtectedWebOriginsIsolateHTTP2Fallback(t *testing.T) {
	chatGPTProtocol := make(chan int, 1)
	chatGPTUpstream, _ := newCapturedTLSServer(t, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chatGPTProtocol <- r.ProtoMajor
		_, _ = io.WriteString(w, "ok")
	}))
	claudeProtocol := make(chan int, 1)
	claudeUpstream, _ := newCapturedTLSServer(t, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claudeProtocol <- r.ProtoMajor
		_, _ = io.WriteString(w, "ok")
	}))

	base := buildHTTPTransport(true)
	dialer := &net.Dialer{}
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		target := chatGPTUpstream.Listener.Addr().String()
		if strings.HasPrefix(address, "claude.ai:") {
			target = claudeUpstream.Listener.Addr().String()
		}
		return dialer.DialContext(ctx, network, target)
	}
	client := newUpstreamHTTPClient(base, 0)
	t.Cleanup(func() { closeUpstreamHTTPClient(client) })

	for _, targetURL := range []string{
		"https://chatgpt.com/backend-api/codex/responses",
		"https://claude.ai/api/organizations",
	} {
		request, err := http.NewRequestWithContext(
			context.Background(), http.MethodPost, targetURL, bytes.NewBufferString(`{"input":"hello"}`),
		)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatalf("send %s: %v", targetURL, err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}

	if got := <-chatGPTProtocol; got != 1 {
		t.Fatalf("ChatGPT fallback protocol = HTTP/%d, want HTTP/1", got)
	}
	if got := <-claudeProtocol; got != 2 {
		t.Fatalf("Claude protocol after ChatGPT fallback = HTTP/%d, want isolated HTTP/2", got)
	}
}

func TestUpstreamHTTPClientFallsBackToUTLSHTTP11WithBodyReplay(t *testing.T) {
	type receivedRequest struct {
		protocol int
		body     string
	}
	received := make(chan receivedRequest, 1)
	upstream, _ := newCapturedTLSServer(t, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		received <- receivedRequest{protocol: r.ProtoMajor, body: string(body)}
		_, _ = io.WriteString(w, "ok")
	}))

	base := buildHTTPTransport(true)
	dialer := &net.Dialer{}
	base.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	client := newUpstreamHTTPClient(base, 0)
	t.Cleanup(func() { closeUpstreamHTTPClient(client) })

	wantBody := `{"input":"fallback"}`
	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		"https://chatgpt.com/backend-api/codex/responses",
		strings.NewReader(wantBody),
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	got := <-received
	if got.protocol != 1 {
		t.Fatalf("fallback protocol = HTTP/%d, want HTTP/1.1", got.protocol)
	}
	if got.body != wantBody {
		t.Fatalf("fallback body = %q, want %q", got.body, wantBody)
	}
}

func TestUpstreamHTTPClientCancellationDoesNotDegradeUTLSHTTP2(t *testing.T) {
	started := make(chan struct{}, 1)
	protocol := make(chan int, 1)
	upstream, _ := newCapturedTLSServer(t, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cancel" {
			started <- struct{}{}
			<-r.Context().Done()
			return
		}
		protocol <- r.ProtoMajor
		_, _ = io.WriteString(w, "ok")
	}))

	base := buildHTTPTransport(true)
	dialer := &net.Dialer{}
	base.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, upstream.Listener.Addr().String())
	}
	client := newUpstreamHTTPClient(base, 0)
	t.Cleanup(func() { closeUpstreamHTTPClient(client) })

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/cancel", nil)
	if err != nil {
		t.Fatalf("build canceled request: %v", err)
	}
	requestErr := make(chan error, 1)
	go func() {
		resp, errDo := client.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		requestErr <- errDo
	}()
	<-started
	cancel()
	if err = <-requestErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request error = %v, want context.Canceled", err)
	}

	resp, err := client.Get("https://chatgpt.com/next")
	if err != nil {
		t.Fatalf("send request after cancellation: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if got := <-protocol; got != 2 {
		t.Fatalf("protocol after cancellation = HTTP/%d, want HTTP/2", got)
	}
}

func TestUpstreamHTTPClientUsesUTLSThroughHTTPProxy(t *testing.T) {
	protocol := make(chan int, 1)
	upstream, captured := newCapturedTLSServer(t, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		protocol <- r.ProtoMajor
		_, _ = io.WriteString(w, "proxied")
	}))
	connectTarget := make(chan string, 1)
	proxyAuthorization := make(chan string, 1)
	proxyErrors := make(chan error, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			proxyErrors <- fmt.Errorf("proxy method = %s, want CONNECT", r.Method)
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		connectTarget <- r.Host
		proxyAuthorization <- r.Header.Get("Proxy-Authorization")
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			proxyErrors <- errors.New("proxy response writer cannot hijack")
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
	defer proxy.Close()

	proxyURL := strings.Replace(proxy.URL, "http://", "http://codex:secret@", 1)
	base, err := buildChannelProxyTransport(proxyURL, true)
	if err != nil {
		t.Fatalf("build channel proxy transport: %v", err)
	}
	client := newUpstreamHTTPClient(base, 0)
	defer closeUpstreamHTTPClient(client)
	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		"https://chatgpt.com/backend-api/codex/responses",
		strings.NewReader(`{"input":"proxy"}`),
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("send proxied request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	select {
	case err = <-proxyErrors:
		t.Fatal(err)
	default:
	}
	if got := <-connectTarget; got != "chatgpt.com:443" {
		t.Fatalf("CONNECT target = %q, want chatgpt.com:443", got)
	}
	if got := <-proxyAuthorization; got != "Basic Y29kZXg6c2VjcmV0" {
		t.Fatalf("Proxy-Authorization = %q, want channel proxy basic credentials", got)
	}
	if got := <-protocol; got != 2 {
		t.Fatalf("proxied upstream protocol = HTTP/%d, want HTTP/2", got)
	}
	if !clientHelloHasGREASECipherSuite(captured.Bytes()) {
		t.Fatal("proxied ChatGPT TLS ClientHello has no GREASE cipher suite")
	}
}

func newCapturedTLSServer(
	t *testing.T,
	enableHTTP2 bool,
	handler http.Handler,
) (*httptest.Server, *clientHelloCapture) {
	t.Helper()
	captured := &clientHelloCapture{}
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.EnableHTTP2 = enableHTTP2
	server.Listener = &clientHelloCaptureListener{Listener: server.Listener, capture: captured}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, captured
}

type clientHelloCapture struct {
	mu  sync.Mutex
	raw []byte
}

func (c *clientHelloCapture) append(raw []byte) {
	c.mu.Lock()
	c.raw = append(c.raw, raw...)
	c.mu.Unlock()
}

func (c *clientHelloCapture) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.raw)
}

type clientHelloCaptureListener struct {
	net.Listener
	capture *clientHelloCapture
}

func (l *clientHelloCaptureListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &clientHelloCaptureConn{Conn: conn, capture: l.capture}, nil
}

type clientHelloCaptureConn struct {
	net.Conn
	capture *clientHelloCapture
}

func (c *clientHelloCaptureConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.capture.append(p[:n])
	}
	return n, err
}

func clientHelloHasGREASECipherSuite(raw []byte) bool {
	if len(raw) < 5 || raw[0] != 22 {
		return false
	}
	recordLength := int(raw[3])<<8 | int(raw[4])
	if recordLength > len(raw)-5 {
		return false
	}
	handshake := raw[5 : 5+recordLength]
	if len(handshake) < 39 || handshake[0] != 1 {
		return false
	}
	offset := 4 + 2 + 32
	sessionIDLength := int(handshake[offset])
	offset += 1 + sessionIDLength
	if offset+2 > len(handshake) {
		return false
	}
	cipherSuitesLength := int(handshake[offset])<<8 | int(handshake[offset+1])
	offset += 2
	if cipherSuitesLength%2 != 0 || offset+cipherSuitesLength > len(handshake) {
		return false
	}
	for i := offset; i < offset+cipherSuitesLength; i += 2 {
		cipherSuite := uint16(handshake[i])<<8 | uint16(handshake[i+1])
		if cipherSuite&0x0f0f == 0x0a0a {
			return true
		}
	}
	return false
}
