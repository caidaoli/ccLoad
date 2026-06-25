package testutil_test

import (
	"bytes"
	"net/http"
	"testing"

	"ccLoad/internal/testutil"
)

func TestNewRequest_NilBody_DoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("expected no panic, got %v", r)
		}
	}()

	req := testutil.NewRequest(http.MethodGet, "/test", nil)
	if req == nil {
		t.Fatal("request should not be nil")
	}
}

func TestNewRequestReader_TypedNil_DoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("expected no panic, got %v", r)
		}
	}()

	var r *bytes.Reader
	req := testutil.NewRequestReader(http.MethodGet, "/test", r)
	if req == nil {
		t.Fatal("request should not be nil")
	}
}
