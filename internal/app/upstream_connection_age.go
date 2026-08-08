package app

import (
	"errors"
	"net/http"
	"sync"
	"time"
)

var errUpstreamConnectionAgeTransportClosed = errors.New("upstream connection age transport is closed")

// upstreamConnectionAgeTransport rotates complete HTTP transport generations.
// That is the only safe boundary shared by HTTP/1.1 and multiplexed HTTP/2:
// an expired generation receives no new requests, while active responses drain
// before its remaining idle connections are closed.
type upstreamConnectionAgeTransport struct {
	mu       sync.Mutex
	template *http.Transport
	current  *upstreamHTTPTransportGeneration
	maxAge   time.Duration
	closed   bool
}

type upstreamHTTPTransportGeneration struct {
	transport *codexUTLSRoundTripper
	timer     *time.Timer
	active    int
	retired   bool
}

func newUpstreamConnectionAgeTransport(
	base *http.Transport,
	maxAge time.Duration,
) *upstreamConnectionAgeTransport {
	transport := &upstreamConnectionAgeTransport{
		template: base.Clone(),
		maxAge:   maxAge,
	}
	transport.mu.Lock()
	transport.startGenerationLocked(base)
	transport.mu.Unlock()
	return transport
}

func newUpstreamHTTPClient(base *http.Transport, maxAge time.Duration) *http.Client {
	var roundTripper http.RoundTripper = newCodexUTLSRoundTripper(base)
	if maxAge > 0 {
		roundTripper = newUpstreamConnectionAgeTransport(base, maxAge)
	}
	return &http.Client{Transport: roundTripper, Timeout: 0}
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
	generation := &upstreamHTTPTransportGeneration{transport: newCodexUTLSRoundTripper(base)}
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
