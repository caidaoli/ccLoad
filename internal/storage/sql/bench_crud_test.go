package sql_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"ccLoad/internal/model"
	"ccLoad/internal/storage"
)

func BenchmarkUpdateTokenStats_SQLite(b *testing.B) {
	b.ReportAllocs()

	tmp := b.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "bench.db"))
	if err != nil {
		b.Fatalf("create sqlite store: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	tokenHash := "bench_token_hash"
	if err := store.CreateAuthToken(ctx, &model.AuthToken{
		Token:             tokenHash,
		Description:       "bench",
		CreatedAt:         time.Now(),
		IsActive:          true,
		CostLimitMicroUSD: 0,
	}); err != nil {
		b.Fatalf("create auth token: %v", err)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := store.UpdateTokenStats(ctx, tokenHash, true, 123.0, false, 0, 12, 34, 0, 0, 0.01); err != nil {
			b.Fatalf("update token stats: %v", err)
		}
	}
}

func BenchmarkUpdateTokenStats_SQLite_Parallel(b *testing.B) {
	b.ReportAllocs()

	tmp := b.TempDir()
	store, err := storage.CreateSQLiteStore(filepath.Join(tmp, "bench_parallel.db"))
	if err != nil {
		b.Fatalf("create sqlite store: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	tokenHash := "bench_token_hash_parallel"
	if err := store.CreateAuthToken(ctx, &model.AuthToken{
		Token:             tokenHash,
		Description:       "bench_parallel",
		CreatedAt:         time.Now(),
		IsActive:          true,
		CostLimitMicroUSD: 0,
	}); err != nil {
		b.Fatalf("create auth token: %v", err)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := store.UpdateTokenStats(ctx, tokenHash, true, 123.0, false, 0, 12, 34, 0, 0, 0.01); err != nil {
				b.Fatalf("update token stats: %v", err)
			}
		}
	})
}
