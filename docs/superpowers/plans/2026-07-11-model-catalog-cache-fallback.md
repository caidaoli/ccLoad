# Model Catalog Cache Fallback Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the default model catalog cache fall back to the system temporary directory when `data/` is not writable, while keeping explicit cache paths strict.

**Architecture:** Resolve the cache path once when model catalog synchronization starts. Keep the resolver inside `internal/app/model_catalog_sync.go`: explicit `CCLOAD_MODEL_CATALOG_CACHE` values are returned unchanged, while the default path probes `data/` and falls back to `${TMPDIR}/ccload/model-catalog.json` when necessary. `ModelCatalogSyncer` continues sharing the resolved path between cache loading and synchronization.

**Tech Stack:** Go, standard library `os`/`path/filepath`, existing `testing` package, Sonic build tag.

## Global Constraints

- Use `-tags sonic` for Go tests.
- Extend `internal/app/model_catalog_sync_test.go`; do not create a new test file.
- Only the default cache path may fall back. An explicit `CCLOAD_MODEL_CATALOG_CACHE` must never fall back.
- Preserve the existing behavior where a cache persistence failure does not roll back an already installed in-memory catalog.
- Do not change the catalog JSON schema, download flow, ETag behavior, or synchronization interval.

---

### Task 1: Resolve the default model catalog cache path safely

**Files:**
- Modify: `internal/app/model_catalog_sync.go:85-90`
- Test: `internal/app/model_catalog_sync_test.go`

**Interfaces:**
- Consumes: `CCLOAD_MODEL_CATALOG_CACHE`, `os.TempDir()`, existing `modelCatalogCachePath() string` callers.
- Produces: unchanged `modelCatalogCachePath() string` signature and private `isModelCatalogCacheDirWritable(dir string) bool` helper.

- [ ] **Step 1: Add failing path-resolution tests**

Append to `internal/app/model_catalog_sync_test.go`:

```go
func TestModelCatalogCachePathDefaultAndFallback(t *testing.T) {
	t.Setenv("CCLOAD_MODEL_CATALOG_CACHE", "")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd failed: %v", err)
	}
	t.Chdir(t.TempDir())
	t.Cleanup(func() {
		if err := os.Chdir(wd); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	if got, want := modelCatalogCachePath(), filepath.Join("data", "model-catalog.json"); got != want {
		t.Fatalf("modelCatalogCachePath() = %q, want %q", got, want)
	}

	if err := os.RemoveAll("data"); err != nil {
		t.Fatalf("remove data directory: %v", err)
	}
	if err := os.WriteFile("data", []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block data directory: %v", err)
	}

	if got, want := modelCatalogCachePath(), filepath.Join(os.TempDir(), "ccload", "model-catalog.json"); got != want {
		t.Fatalf("fallback modelCatalogCachePath() = %q, want %q", got, want)
	}
}

func TestModelCatalogCachePathExplicitPathNeverFallsBack(t *testing.T) {
	blockedParent := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blockedParent, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block explicit parent directory: %v", err)
	}
	explicitPath := filepath.Join(blockedParent, "model-catalog.json")
	t.Setenv("CCLOAD_MODEL_CATALOG_CACHE", explicitPath)

	if got := modelCatalogCachePath(); got != explicitPath {
		t.Fatalf("modelCatalogCachePath() = %q, want explicit path %q", got, explicitPath)
	}
}
```

- [ ] **Step 2: Run the focused tests and verify the fallback test fails**

Run:

```bash
go test -tags sonic ./internal/app -run 'TestModelCatalogCachePath(DefaultAndFallback|ExplicitPathNeverFallsBack)$' -count=1
```

Expected: `TestModelCatalogCachePathDefaultAndFallback` fails because the current resolver still returns `data/model-catalog.json` when `data` is a regular file.

- [ ] **Step 3: Implement the minimal resolver and write probe**

Replace `modelCatalogCachePath` and add the helper in `internal/app/model_catalog_sync.go`:

```go
func modelCatalogCachePath() string {
	if path := strings.TrimSpace(os.Getenv("CCLOAD_MODEL_CATALOG_CACHE")); path != "" {
		return path
	}

	const defaultDir = "data"
	defaultPath := filepath.Join(defaultDir, "model-catalog.json")
	if isModelCatalogCacheDirWritable(defaultDir) {
		return defaultPath
	}
	if err := os.MkdirAll(defaultDir, 0o755); err == nil && isModelCatalogCacheDirWritable(defaultDir) {
		return defaultPath
	}

	tmpPath := filepath.Join(os.TempDir(), "ccload", "model-catalog.json")
	log.Printf("[WARN] 模型目录缓存默认路径 %s 不可写，降级到临时路径 %s；系统重启后缓存可能丢失", defaultPath, tmpPath)
	return tmpPath
}

func isModelCatalogCacheDirWritable(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}

	probe, err := os.CreateTemp(dir, ".model-catalog-write-test-*")
	if err != nil {
		return false
	}
	probePath := probe.Name()
	closeErr := probe.Close()
	removeErr := os.Remove(probePath)
	return closeErr == nil && removeErr == nil
}
```

- [ ] **Step 4: Format and rerun the focused tests**

Run:

```bash
gofmt -w internal/app/model_catalog_sync.go internal/app/model_catalog_sync_test.go
go test -tags sonic ./internal/app -run 'TestModelCatalogCachePath(DefaultAndFallback|ExplicitPathNeverFallsBack)$' -count=1
```

Expected: both tests pass.

- [ ] **Step 5: Run the existing model catalog regression tests**

Run:

```bash
go test -tags sonic ./internal/app -run 'TestModelCatalog' -count=1
```

Expected: all model catalog tests pass, including the existing persistence-error behavior.

- [ ] **Step 6: Run repository verification**

Run:

```bash
go test -tags sonic ./internal/...
git diff --check
```

Expected: all internal tests pass and `git diff --check` prints no output.

- [ ] **Step 7: Commit the implementation**

```bash
git add internal/app/model_catalog_sync.go internal/app/model_catalog_sync_test.go
git commit -m "fix: fall back model catalog cache path"
```
