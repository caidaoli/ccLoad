# Model Catalog Sync Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a runtime models.dev-backed official model catalog so ordinary model releases and base-price changes take effect without publishing a new ccLoad version.

**Architecture:** Keep usage normalization and billing rules local. Put price lookup behind an immutable atomic catalog, overlay validated models.dev data on embedded defaults, persist the last good normalized snapshot, and expose protocol-native common models through an authenticated Admin API. A Server-owned worker performs startup and six-hour conditional syncs; the request path never performs network or JSON work.

**Tech Stack:** Go 1.24, `sync/atomic`, standard `net/http` and `encoding/json`, Gin, existing system settings, browser JavaScript, Node test runner.

## Global Constraints

- Use `-tags sonic` for Go tests and builds.
- Add no third-party dependency.
- Fetch only `https://models.dev/api.json`; only `CCLOAD_MODEL_CATALOG_CACHE` can change the cache path.
- Provider priority is exactly `openai`, `anthropic`, `google`, `xai`, `deepseek`, `alibaba`, `zai`, `minimax`, `moonshotai`, `xiaomi`, `mistral`.
- Remote data replaces numeric base prices and context tiers only. Service tier, fast mode, cache-token semantics, image/tool billing, fixed-request billing, and channel multipliers remain local.
- `model_catalog_sync_interval_hours` defaults to `6`; `0` disables network sync but still loads the last good cache.
- Test exported behavior, API contracts, persisted state, lifecycle, and submitted model data. Do not test source text, helper delegation, CSS classes, or wrapper markup.

---

### Task 1: Immutable pricing catalog and models.dev normalization

**Files:**
- Create: `internal/util/model_pricing_catalog.go`
- Create: `internal/util/models_dev_catalog.go`
- Create: `internal/util/models_dev_catalog_test.go` — new external-data contract has no existing owner
- Modify: `internal/util/cost_calculator.go`
- Modify: `internal/util/cost_calculator_test.go`
- Modify: `internal/util/cost_calculator_bench_test.go`

**Interfaces:**
- Produces: `ModelCatalogSnapshot`, `ModelCatalogEntry`, `ParseModelsDevCatalog`, `InstallModelCatalog`, `RestoreEmbeddedModelCatalog`, `CurrentModelCatalogETag`, `CommonCatalogModels`.
- Preserves: existing `CalculateCostDetailed`, `getPricing`, and `fuzzyMatchModel` behavior.

- [ ] **Step 1: Add failing external-catalog contract tests**

Build a fixture containing one valid token-priced model for every allowlisted provider. Override focused entries to verify base prices, cache-read prices, context tiers, metadata, deterministic provider conflict priority, missing-provider rejection, invalid model skipping, and common-model ordering.

```go
func TestParseModelsDevCatalogNormalizesOfficialPrices(t *testing.T) {
	now := time.Date(2026, 7, 10, 6, 0, 0, 0, time.UTC)
	raw := validModelsDevFixture(t, "openai", "gpt-next", map[string]any{
		"id": "gpt-next",
		"release_date": "2026-07-09",
		"last_updated": "2026-07-10",
		"modalities": map[string]any{"output": []string{"text"}},
		"cost": map[string]any{
			"input": 2.5, "output": 15.0, "cache_read": 0.25,
			"tiers": []any{map[string]any{
				"input": 5.0, "output": 22.5, "cache_read": 0.5,
				"tier": map[string]any{"type": "context", "size": 272000},
			}},
		},
	})
	snapshot, err := ParseModelsDevCatalog(bytes.NewReader(raw), `"etag-1"`, now)
	if err != nil { t.Fatal(err) }
	entry, ok := snapshot.Model("gpt-next")
	if !ok || entry.Provider != "openai" { t.Fatalf("entry = %#v", entry) }
	if entry.Pricing.InputPrice != 2.5 || len(entry.Pricing.TokenPricingTiers) != 2 {
		t.Fatalf("pricing = %#v", entry.Pricing)
	}
}
```

- [ ] **Step 2: Confirm the focused tests fail**

```bash
go test -tags sonic ./internal/util -run 'TestParseModelsDevCatalog|TestModelCatalog' -count=1
```

Expected: build failure because the catalog boundary does not exist.

- [ ] **Step 3: Split pricing data from billing rules**

Move `ModelPricing`, `TokenPricingTier`, tier data, `basePricing`, `modelAliases`, `getPricing`, `fuzzyMatchModel`, and prefix indexing from `cost_calculator.go` into `model_pricing_catalog.go`. Keep image pricing and every cost formula in `cost_calculator.go`.

Use one immutable snapshot:

```go
type modelPrefixMatch struct { prefix, target string }

type modelPricingSnapshot struct {
	pricing       map[string]ModelPricing
	prefixBuckets map[byte][]modelPrefixMatch
	metadata      map[string]ModelCatalogEntry
	remoteETag    string
	remoteFetched time.Time
	remoteSource  string
}

var activeModelPricing atomic.Pointer[modelPricingSnapshot]

func init() { activeModelPricing.Store(buildModelPricingSnapshot(nil, "embedded")) }
```

Copy all maps and slices before publication. Overlay remote numeric fields on embedded entries while preserving `CacheReadCountsTowardTier`, fixed costs, and other local behavior flags. Build prefix matches from pricing keys plus alias keys; sort by descending prefix length and ascending prefix text, then bucket by first byte.

- [ ] **Step 4: Implement the normalized external boundary**

```go
const ModelCatalogSchemaVersion = 1

var ModelsDevOfficialProviders = []string{
	"openai", "anthropic", "google", "xai", "deepseek", "alibaba",
	"zai", "minimax", "moonshotai", "xiaomi", "mistral",
}

type ModelCatalogEntry struct {
	ID string `json:"id"`
	Provider string `json:"provider"`
	ReleaseDate string `json:"release_date,omitempty"`
	LastUpdated string `json:"last_updated,omitempty"`
	Status string `json:"status,omitempty"`
	OutputModalities []string `json:"output_modalities,omitempty"`
	Pricing ModelPricing `json:"pricing"`
}

type ModelCatalogSnapshot struct {
	Version int `json:"version"`
	Source string `json:"source"`
	ETag string `json:"etag,omitempty"`
	FetchedAt time.Time `json:"fetched_at"`
	Models []ModelCatalogEntry `json:"models"`
}

func ParseModelsDevCatalog(r io.Reader, etag string, fetchedAt time.Time) (*ModelCatalogSnapshot, error)
func (s *ModelCatalogSnapshot) Model(id string) (ModelCatalogEntry, bool)
func InstallModelCatalog(snapshot *ModelCatalogSnapshot, source string) error
func RestoreEmbeddedModelCatalog()
func CurrentModelCatalogETag() string
func CommonCatalogModels(channelType string, limit int) ([]string, string, time.Time)
```

Represent external prices with pointers so missing differs from zero. Reject empty IDs, missing input/output, negative or non-finite values. Convert sorted context thresholds into internal inclusive tiers: base ends at the first threshold, each remote tier ends at the next threshold, final tier uses `MaxInputTokens=0`.

- [ ] **Step 5: Support explicit cache-read price per tier**

```go
type TokenPricingTier struct {
	MaxInputTokens int
	InputPrice float64
	OutputPrice float64
	CacheReadPrice float64
	HasCacheReadPrice bool
}
```

In `CalculateCostDetailed`, retain the selected tier and use its explicit cache-read price when present. Otherwise execute the existing model-level explicit price or family multiplier unchanged.

- [ ] **Step 6: Add billing regressions through `CalculateCostDetailed`**

Install a remote snapshot and assert exact cost for a new model, an overridden embedded model, and a high-context tier with a distinct cache-read price. Use `t.Cleanup(RestoreEmbeddedModelCatalog)`.

```go
got := CalculateCostDetailed("gpt-next", 1_000_000, 1_000_000, 1_000_000, 0, 0)
if got != 10.2 { t.Fatalf("cost = %v, want 10.2", got) }
```

- [ ] **Step 7: Run util verification**

```bash
go test -tags sonic ./internal/util -count=1
go test -tags sonic ./internal/util -run '^$' -bench FuzzyMatchModel -benchtime=100ms
```

- [ ] **Step 8: Commit Task 1**

```bash
git add internal/util/model_pricing_catalog.go internal/util/models_dev_catalog.go internal/util/models_dev_catalog_test.go internal/util/cost_calculator.go internal/util/cost_calculator_test.go internal/util/cost_calculator_bench_test.go
git commit -m "feat: add runtime model pricing catalog"
```

---

### Task 2: Conditional sync worker and last-good snapshot

**Files:**
- Create: `internal/app/model_catalog_sync.go`
- Create: `internal/app/model_catalog_sync_test.go` — new lifecycle/persistence subsystem has no existing owner
- Modify: `internal/app/server.go`

**Interfaces:**
- Consumes: Task 1 parser/install APIs.
- Produces: `(*Server).StartModelCatalogSync`, bounded HTTP sync, conditional ETag requests, atomic cache-file persistence, cancellation and no-overlap semantics.

- [ ] **Step 1: Add failing HTTP, persistence, and lifecycle tests**

Use `httptest.Server` and `t.TempDir()` to cover 200 update, `If-None-Match` + 304, 16 MiB rejection, timeout/cancellation, invalid JSON retaining the prior catalog, valid cache startup, corrupt cache fallback, interval zero making no request, and concurrent sync attempts returning `skipped`.

```go
result, err := syncer.Sync(context.Background())
if err != nil { t.Fatal(err) }
if result.Status != modelCatalogSyncUpdated { t.Fatalf("status = %q", result.Status) }
if _, err := os.Stat(cachePath); err != nil { t.Fatalf("cache: %v", err) }
```

- [ ] **Step 2: Confirm focused tests fail**

```bash
go test -tags sonic ./internal/app -run TestModelCatalogSync -count=1
```

- [ ] **Step 3: Implement bounded conditional HTTP sync**

```go
const (
	modelsDevCatalogURL = "https://models.dev/api.json"
	modelCatalogRequestTimeout = 15 * time.Second
	modelCatalogMaxBodyBytes = 16 << 20
	defaultModelCatalogSyncHours = 6.0
)

type modelCatalogSyncStatus string
const (
	modelCatalogSyncUpdated modelCatalogSyncStatus = "updated"
	modelCatalogSyncNotModified modelCatalogSyncStatus = "not_modified"
	modelCatalogSyncSkipped modelCatalogSyncStatus = "skipped"
)
```

Set `If-None-Match` from `util.CurrentModelCatalogETag()`. Accept only 200/304. Read through `io.LimitReader(body, modelCatalogMaxBodyBytes+1)`. Parse and install only after complete validation. A failed sync must not mutate the active catalog.

- [ ] **Step 4: Persist only the normalized snapshot**

Resolve the path with:

```go
func modelCatalogCachePath() string {
	if path := strings.TrimSpace(os.Getenv("CCLOAD_MODEL_CATALOG_CACHE")); path != "" { return path }
	return filepath.Join("data", "model-catalog.json")
}
```

Create the directory with `0755`; write a same-directory temp file with `0600`; JSON encode, `Sync`, close, rename, and remove the temp file on failure. Load with a bounded reader, require `Version == util.ModelCatalogSchemaVersion`, and install using source `cache`. Persistence failure logs an error but does not roll back a valid in-memory update.

- [ ] **Step 5: Attach the worker to Server lifecycle without test network calls**

Do not start it inside `NewServer`. Add:

```go
func (s *Server) StartModelCatalogSync() {
	intervalHours := normalizeModelCatalogSyncIntervalHours(
		s.configService.GetFloat("model_catalog_sync_interval_hours", defaultModelCatalogSyncHours),
	)
	syncer := newModelCatalogSyncer(&http.Client{Timeout: modelCatalogRequestTimeout}, modelsDevCatalogURL, modelCatalogCachePath())
	if err := syncer.LoadCache(); err != nil { log.Printf("[WARN] 模型目录缓存加载失败: %v", err) }
	if intervalHours == 0 { return }
	s.wg.Add(1)
	go s.runModelCatalogSyncLoop(syncer, time.Duration(intervalHours*float64(time.Hour)))
}
```

The loop syncs immediately, then selects on `ticker.C` and `s.baseCtx.Done()`. Guard `Sync` with `atomic.Bool`.

- [ ] **Step 6: Run lifecycle and race verification**

```bash
go test -tags sonic ./internal/app -run TestModelCatalogSync -count=1
go test -race -tags sonic ./internal/app -run TestModelCatalogSync -count=1
```

- [ ] **Step 7: Commit Task 2**

```bash
git add internal/app/model_catalog_sync.go internal/app/model_catalog_sync_test.go internal/app/server.go
git commit -m "feat: sync models.dev catalog in background"
```

---

### Task 3: Configuration, startup, and Admin API

**Files:**
- Create: `internal/app/admin_model_catalog.go`
- Modify: `internal/app/admin_models_test.go`
- Modify: `internal/app/admin_settings_validation_test.go`
- Modify: `internal/app/admin_settings.go`
- Modify: `internal/app/server.go`
- Modify: `internal/storage/migrate.go`
- Modify: `internal/storage/migrate_sqlite_test.go`
- Modify: `main.go`
- Modify: `.env.example`

**Interfaces:**
- Consumes: Task 1 common models and Task 2 startup method.
- Produces: `GET /admin/model-catalog/common?channel_type=...` and persisted sync interval configuration.

- [ ] **Step 1: Add failing migration, validation, and handler tests**

Extend existing test files. Assert the migration seeds float value `6`; validation accepts `0`, `0.5`, `6` and rejects negatives; the handler returns parsed JSON with `models`, `source`, and RFC3339 `fetched_at`.

```go
var response struct {
	Models []string `json:"models"`
	Source string `json:"source"`
	FetchedAt string `json:"fetched_at"`
}
if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil { t.Fatal(err) }
if response.Source != "models.dev" || len(response.Models) != 6 { t.Fatalf("response = %#v", response) }
```

- [ ] **Step 2: Confirm focused tests fail**

```bash
go test -tags sonic ./internal/app ./internal/storage -run 'ModelCatalog|CommonModelCatalog' -count=1
```

- [ ] **Step 3: Seed and validate the interval setting**

Add to the existing setting seed list:

```go
{"model_catalog_sync_interval_hours", "6", "float", "模型目录同步间隔(小时,支持小数,0=关闭网络同步,修改后重启生效)", "6"},
```

Apply the same non-negative float validation branch used by `channel_check_interval_hours`.

- [ ] **Step 4: Implement and register the Admin API**

```go
type commonModelCatalogResponse struct {
	Models []string `json:"models"`
	Source string `json:"source"`
	FetchedAt string `json:"fetched_at,omitempty"`
}

func (s *Server) HandleCommonModelCatalog(c *gin.Context) {
	channelType := util.NormalizeChannelType(c.Query("channel_type"))
	if !util.IsValidChannelType(channelType) {
		RespondErrorMsg(c, http.StatusBadRequest, "invalid channel_type")
		return
	}
	models, source, fetchedAt := util.CommonCatalogModels(channelType, 6)
	response := commonModelCatalogResponse{Models: models, Source: source}
	if !fetchedAt.IsZero() { response.FetchedAt = fetchedAt.UTC().Format(time.RFC3339) }
	RespondJSON(c, http.StatusOK, response)
}
```

Register `admin.GET("/model-catalog/common", s.HandleCommonModelCatalog)` beside `/admin/models`.

- [ ] **Step 5: Wire production startup and document cache path**

Call `srv.StartModelCatalogSync()` immediately after `app.NewServer(store)` in `main.go`. Add to `.env.example`:

```dotenv
# models.dev 归一化目录缓存路径（可选，默认: data/model-catalog.json）
CCLOAD_MODEL_CATALOG_CACHE=./data/model-catalog.json
```

- [ ] **Step 6: Run focused tests**

```bash
go test -tags sonic ./internal/app ./internal/storage -run 'ModelCatalog|CommonModelCatalog|Setting' -count=1
```

- [ ] **Step 7: Commit Task 3**

```bash
git add internal/app/admin_model_catalog.go internal/app/admin_models_test.go internal/app/admin_settings_validation_test.go internal/app/admin_settings.go internal/app/server.go internal/storage/migrate.go internal/storage/migrate_sqlite_test.go main.go .env.example
git commit -m "feat: expose synchronized common models"
```

---

### Task 4: Channel editor consumes synchronized common models

**Files:**
- Modify: `web/assets/js/channels-modals.js`
- Modify: `web/assets/js/channels-test.js`

**Interfaces:**
- Consumes: Task 3 Admin endpoint.
- Produces: submitted channel model rows use synchronized IDs, with the current static list as failure fallback.

- [ ] **Step 1: Add failing submitted-data tests**

Extend `channels-test.js` to assert data, not markup:

```javascript
test('mergeCommonModels uses synchronized models and deduplicates case-insensitively', () => {
  const merged = mergeCommonModels(
    [{ model: 'gpt-5.6', redirect_model: '' }],
    ['GPT-5.6', 'gpt-5.7'],
    ['gpt-5.4']
  );
  assert.deepEqual(merged.rows.map(row => row.model), ['gpt-5.6', 'gpt-5.7']);
  assert.equal(merged.added, 1);
});

test('mergeCommonModels uses embedded fallback when synchronized models are empty', () => {
  const merged = mergeCommonModels([], [], ['claude-sonnet-5']);
  assert.deepEqual(merged.rows.map(row => row.model), ['claude-sonnet-5']);
});
```

- [ ] **Step 2: Confirm the Node test fails**

```bash
node --test web/assets/js/channels-test.js
```

- [ ] **Step 3: Extract merging and make `addCommonModels` asynchronous**

```javascript
function mergeCommonModels(rows, fetchedModels, fallbackModels) {
  const source = Array.isArray(fetchedModels) && fetchedModels.length > 0
    ? fetchedModels
    : (Array.isArray(fallbackModels) ? fallbackModels : []);
  const nextRows = rows.map(row => ({ ...row }));
  const existing = new Set(nextRows.map(row => String(row.model || '').trim().toLowerCase()).filter(Boolean));
  let added = 0;
  for (const rawModel of source) {
    const model = String(rawModel || '').trim();
    const key = model.toLowerCase();
    if (!key || existing.has(key)) continue;
    nextRows.push({ model, redirect_model: '' });
    existing.add(key);
    added++;
  }
  return { rows: nextRows, added };
}
```

`addCommonModels` fetches `/admin/model-catalog/common?channel_type=${encodeURIComponent(channelType)}` through `fetchDataWithAuth`, catches failures, passes `response.models` and `COMMON_MODELS[channelType]` to the helper, replaces `redirectTableData`, renders once, and marks dirty only when `added > 0`.

- [ ] **Step 4: Run frontend verification**

```bash
node --test web/assets/js/channels-test.js
make verify-web
```

- [ ] **Step 5: Commit Task 4**

```bash
git add web/assets/js/channels-modals.js web/assets/js/channels-test.js
git commit -m "feat: load common models from runtime catalog"
```

---

### Task 5: Documentation and full verification

**Files:**
- Modify: `README.md`
- Modify: `README.zh-CN.md`

- [ ] **Step 1: Document the setting and fallback semantics**

Add one concise section/table row to both READMEs covering the six-hour default, `0` disable behavior, models.dev source, last-good cache, embedded fallback, and continued use of `cost_multiplier` for channel-specific pricing.

- [ ] **Step 2: Format all changed Go files**

```bash
gofmt -w internal/util/model_pricing_catalog.go internal/util/models_dev_catalog.go internal/util/models_dev_catalog_test.go internal/util/cost_calculator.go internal/util/cost_calculator_test.go internal/util/cost_calculator_bench_test.go internal/app/model_catalog_sync.go internal/app/model_catalog_sync_test.go internal/app/admin_model_catalog.go internal/app/admin_models_test.go internal/app/admin_settings_validation_test.go internal/app/admin_settings.go internal/app/server.go internal/storage/migrate.go internal/storage/migrate_sqlite_test.go main.go
```

- [ ] **Step 3: Run required verification**

```bash
go test -tags sonic ./internal/...
make verify-web
golangci-lint run ./...
make build
```

Expected: every command exits 0 and lint reports zero warnings.

- [ ] **Step 4: Inspect final scope**

```bash
git diff --check
git status --short
git diff --stat HEAD~4..HEAD
```

Confirm no request handler calls models.dev, no raw response is persisted, no provider outside the allowlist is installed, and unrelated files are untouched.

- [ ] **Step 5: Commit documentation**

```bash
git add README.md README.zh-CN.md
git commit -m "docs: explain automatic model catalog sync"
```
