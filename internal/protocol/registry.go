package protocol

import (
	"context"
	"fmt"
)

type pair struct {
	from Protocol
	to   Protocol
}

// Registry stores the request/response transformers registered for protocol pairs.
type Registry struct {
	requests   map[pair]RequestTransform
	streams    map[pair]ResponseStreamTransform
	nonStreams map[pair]ResponseNonStreamTransform
}

// NewRegistry creates an empty protocol transform registry.
func NewRegistry() *Registry {
	return &Registry{
		requests:   make(map[pair]RequestTransform),
		streams:    make(map[pair]ResponseStreamTransform),
		nonStreams: make(map[pair]ResponseNonStreamTransform),
	}
}

// RegisterRequest registers the request transformer for one protocol pair.
func (r *Registry) RegisterRequest(from, to Protocol, fn RequestTransform) {
	if fn == nil {
		return
	}
	r.requests[pair{from: from, to: to}] = fn
}

// RegisterNonStreamResponse registers the non-stream response transformer for one protocol pair.
func (r *Registry) RegisterNonStreamResponse(from, to Protocol, fn ResponseNonStreamTransform) {
	if fn == nil {
		return
	}
	r.nonStreams[pair{from: from, to: to}] = fn
}

// RegisterStreamResponse registers the streaming response transformer for one protocol pair.
func (r *Registry) RegisterStreamResponse(from, to Protocol, fn ResponseStreamTransform) {
	if fn == nil {
		return
	}
	r.streams[pair{from: from, to: to}] = fn
}

// TranslateRequest converts one request body from a client protocol into the upstream protocol.
func (r *Registry) TranslateRequest(from, to Protocol, model string, rawJSON []byte, stream bool) ([]byte, error) {
	if from == to {
		return rawJSON, nil
	}
	fn, ok := r.requests[pair{from: from, to: to}]
	if !ok {
		return nil, fmt.Errorf("no request transform registered: %s -> %s", from, to)
	}
	return fn(model, rawJSON, stream)
}

// TranslateResponseNonStream converts one upstream non-stream response into the client protocol.
func (r *Registry) TranslateResponseNonStream(ctx context.Context, from, to Protocol, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte) ([]byte, error) {
	if from == to {
		return rawJSON, nil
	}
	fn, ok := r.nonStreams[pair{from: from, to: to}]
	if !ok {
		return nil, fmt.Errorf("no non-stream response transform registered: %s -> %s", from, to)
	}
	return fn(ctx, model, originalRequestRawJSON, requestRawJSON, rawJSON)
}

// TranslateResponseStream converts one upstream streaming event into the client protocol.
func (r *Registry) TranslateResponseStream(ctx context.Context, from, to Protocol, model string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) ([][]byte, error) {
	if from == to {
		return [][]byte{rawJSON}, nil
	}
	fn, ok := r.streams[pair{from: from, to: to}]
	if !ok {
		return nil, fmt.Errorf("no stream response transform registered: %s -> %s", from, to)
	}
	return fn(ctx, model, originalRequestRawJSON, requestRawJSON, rawJSON, param)
}
