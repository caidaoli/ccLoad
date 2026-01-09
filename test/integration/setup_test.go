package integration_test

import (
	"context"
	"testing"

	"ccLoad/internal/storage"
	"ccLoad/internal/testutil"
)

// setupTestStoreWithContext 创建测试用的 Store 和 Context
func setupTestStoreWithContext(t *testing.T) (storage.Store, context.Context, func()) {
	t.Helper()

	store, cleanup := testutil.SetupTestStore(t)
	ctx := context.Background()

	return store, ctx, cleanup
}
