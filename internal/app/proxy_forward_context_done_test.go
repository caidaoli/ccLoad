package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ccLoad/internal/cooldown"
	"ccLoad/internal/model"
)

func TestTryChannelWithKeys_ContextCanceled_Returns499(t *testing.T) {
	s := &Server{}
	cfg := &model.Config{ID: 1}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := s.tryChannelWithKeys(ctx, cfg, &proxyRequestContext{}, httptest.NewRecorder())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res == nil {
		t.Fatal("expected result, got nil")
	}
	if res.status != StatusClientClosedRequest {
		t.Fatalf("expected status %d, got %d", StatusClientClosedRequest, res.status)
	}
	if !res.isClientCanceled {
		t.Fatal("expected isClientCanceled=true")
	}
	if res.nextAction != cooldown.ActionReturnClient {
		t.Fatalf("expected nextAction=ActionReturnClient, got %v", res.nextAction)
	}
}

func TestTryChannelWithKeys_ContextDeadlineExceeded_Returns504(t *testing.T) {
	s := &Server{}
	cfg := &model.Config{ID: 1}

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	res, err := s.tryChannelWithKeys(ctx, cfg, &proxyRequestContext{}, httptest.NewRecorder())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res == nil {
		t.Fatal("expected result, got nil")
	}
	if res.status != http.StatusGatewayTimeout {
		t.Fatalf("expected status %d, got %d", http.StatusGatewayTimeout, res.status)
	}
	if res.isClientCanceled {
		t.Fatal("expected isClientCanceled=false")
	}
	if res.nextAction != cooldown.ActionReturnClient {
		t.Fatalf("expected nextAction=ActionReturnClient, got %v", res.nextAction)
	}
}
