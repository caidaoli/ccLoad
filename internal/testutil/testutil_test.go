package testutil_test

import (
	"bytes"
	"net/http"
	"testing"

	"ccLoad/internal/testutil"
)

// typed nil 传入 io.Reader 参数是真实陷阱：接口值非 nil 但底层指针为 nil。
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
