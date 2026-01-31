package app

import (
	"errors"
	"testing"
)

func TestIsHTTP2StreamCloseError(t *testing.T) {
	if isHTTP2StreamCloseError(nil) {
		t.Fatal("expected false for nil")
	}
	if !isHTTP2StreamCloseError(errors.New("http2: response body closed")) {
		t.Fatal("expected true for response body closed")
	}
	if !isHTTP2StreamCloseError(errors.New("stream error: NO_ERROR")) {
		t.Fatal("expected true for stream error")
	}
	if isHTTP2StreamCloseError(errors.New("other")) {
		t.Fatal("expected false for unrelated error")
	}
}
