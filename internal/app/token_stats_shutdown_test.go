package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func TestUpdateTokenStatsDuringShutdown(t *testing.T) {
	store, err := storage.CreateSQLiteStore(":memory:", nil)
	if err != nil {
		t.Fatalf("CreateSQLiteStore failed: %v", err)
	}

	srv := NewServer(store)

	ctx := context.Background()
	tokenHash := strings.Repeat("a", 64)
	if err := store.CreateAuthToken(ctx, &model.AuthToken{
		Token:       tokenHash,
		Description: "test",
		IsActive:    true,
	}); err != nil {
		t.Fatalf("CreateAuthToken failed: %v", err)
	}

	// 阻塞wg.Wait，避免Shutdown过快走到store.Close，从而与“在途请求结束后写入统计”的场景失真
	blockCh := make(chan struct{})
	srv.wg.Add(1)
	go func() {
		defer srv.wg.Done()
		<-blockCh
	}()

	shutdownErrCh := make(chan error, 1)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		shutdownErrCh <- srv.Shutdown(shutdownCtx)
	}()
	defer func() {
		close(blockCh)
		<-shutdownErrCh
	}()

	// 等待Shutdown进入“shutting down”状态
	deadline := time.Now().Add(1 * time.Second)
	for !srv.isShuttingDown.Load() {
		if time.Now().After(deadline) {
			t.Fatal("server did not enter shutting down state in time")
		}
		time.Sleep(1 * time.Millisecond)
	}

	// 模拟：shutdown开始后，一个在途请求完成并尝试写入计费/用量统计
	srv.updateTokenStatsAsync(tokenHash, true, 1.23, false, &fwResult{
		FirstByteTime:            0.2,
		InputTokens:              10,
		OutputTokens:             20,
		CacheReadInputTokens:     5,
		CacheCreationInputTokens: 3,
	}, "gpt-5.1-codex")

	got, err := store.GetAuthTokenByValue(ctx, tokenHash)
	if err != nil {
		t.Fatalf("GetAuthTokenByValue failed: %v", err)
	}
	if got.SuccessCount != 1 {
		t.Fatalf("SuccessCount = %d, want %d", got.SuccessCount, 1)
	}
	if got.PromptTokensTotal != 10 {
		t.Fatalf("PromptTokensTotal = %d, want %d", got.PromptTokensTotal, 10)
	}
	if got.CompletionTokensTotal != 20 {
		t.Fatalf("CompletionTokensTotal = %d, want %d", got.CompletionTokensTotal, 20)
	}
	if got.CacheReadTokensTotal != 5 {
		t.Fatalf("CacheReadTokensTotal = %d, want %d", got.CacheReadTokensTotal, 5)
	}
	if got.CacheCreationTokensTotal != 3 {
		t.Fatalf("CacheCreationTokensTotal = %d, want %d", got.CacheCreationTokensTotal, 3)
	}
	if got.TotalCostUSD <= 0 {
		t.Fatalf("TotalCostUSD = %f, want > 0", got.TotalCostUSD)
	}
}
