# Channel TTFB Priority Scoring — Design Spec

**Date:** 2026-07-15  
**Status:** Approved for planning  
**Workspace:** `/root/docker/ccLoad-src`

---

## 1. Goal

Extend channel selection so **effective priority** can penalize channels that are slower than peers on first-byte time (TTFB), while keeping:

- manual `Priority` as the primary control
- existing success-rate health penalty
- same-priority Key-count smooth weighted RR
- multi-URL `1/TTFB_EWMA` selection unchanged

---

## 2. Locked decisions

| Topic | Choice |
|-------|--------|
| Scope | Channel-level effective priority only |
| URL-level | Unchanged (`URLSelector`: explore-first, weight `1/latency`) |
| TTFB form | Relative to **median** of same-request candidate channels |
| Reward fast channels? | **No** (v1 only penalizes slower-than-median) |
| Absolute ms thresholds | **No** (model/protocol baselines differ too much) |
| Default | **Off** (`enable_ttfb_score=false`) |
| Coupling | Works only when `enable_health_score=true` (shares health window/cache path) |
| Failures in TTFB avg | **Exclude** failed/timeout requests; success-rate penalty already covers reliability |

---

## 3. Formula (plain text)

```text
P_eff = P_base - Penalty_fail - Penalty_ttfb
```

### Failure penalty (existing)

```text
Penalty_fail = (1 - success_rate) * W_fail * conf_fail
conf_fail    = min(1, request_samples / min_confident_sample)
W_fail default = 100
min_confident_sample default = 20
```

### TTFB penalty (new)

```text
s = L_ch / L_med

Penalty_ttfb = clamp(s - 1, 0, S_max) * W_ttfb * conf_ttfb

conf_ttfb = min(1, ttfb_samples / ttfb_min_confident_sample)
```

Definitions:

| Symbol | Meaning |
|--------|---------|
| `P_base` | Channel `Priority` |
| `L_ch` | Channel avg first-byte seconds in window (successful proxy logs with `first_byte_time > 0`) |
| `L_med` | Median of `L_ch` among **current route candidates** that have enough TTFB samples |
| `s` | Relative slowness vs median |
| `S_max` | Cap on `(s-1)`, default `2.0` (treat at most 3x slower) |
| `W_ttfb` | `ttfb_penalty_weight`, default `20` |
| `ttfb_min_confident_sample` | default `10` |

`clamp(x, 0, S_max)`:

- if channel is as fast or faster than median → penalty `0`
- if slower → proportional penalty up to `S_max * W_ttfb` at full confidence

### When Penalty_ttfb is forced to 0

1. `enable_ttfb_score=false`
2. `enable_health_score=false`
3. Fewer than **2** candidate channels have valid `L_ch` with `ttfb_samples > 0`
4. This channel has `ttfb_samples == 0`
5. `L_med <= 0` (defensive)

---

## 4. Worked defaults

```text
P_eff =
  Priority
  - (1 - success_rate) * 100 * min(1, N_req / 20)
  - min(max(L_ch / L_med - 1, 0), 2) * 20 * min(1, N_ttfb / 10)
```

Examples at `Priority=100`, full confidence, perfect success rate:

| Relative TTFB | Penalty_ttfb | P_eff |
|---------------|--------------|-------|
| = median | 0 | 100 |
| 1.5x median | 10 | 90 |
| 2x median | 20 | 80 |
| >= 3x median | 40 | 60 |

---

## 5. Selection pipeline (after change)

```text
1. Filter available channels (model, protocol, cooldown, token ACL, cost, RPM, concurrency)
2. If health score disabled:
     sort by Priority, same-priority Key RR   # unchanged
3. If health score enabled:
     for each channel compute P_eff (fail ± ttfb)
     sort by P_eff desc
     bucket by effPriorityBucket (0.1)
     same-bucket Key RR
4. For chosen channel multi-URL: existing URLSelector
```

`L_med` is computed on the **filtered candidate list for this request** (same model/protocol pool), not global all-channels, so Claude vs Gemini baselines do not mix.

---

## 6. Data model

### Extend `ChannelHealthStats`

```go
type ChannelHealthStats struct {
	SuccessRate         float64
	SampleCount         int64
	AvgFirstByteSeconds float64 // 0 if none
	FirstByteSampleCount int64
}
```

### Aggregation source

Reuse health window (`health_score_window_minutes`):

- success rate: existing `GetChannelSuccessRates` semantics
- TTFB: average of `logs.first_byte_time` where:
  - `log_source = proxy`
  - `status_code` in 2xx
  - `first_byte_time > 0`
  - within window
  - group by `channel_id`

Prefer extending the same SQL used by success rates to also return TTFB aggregates (one query / one cache refresh).

### Cache

`HealthCache` already snapshots success rates on an interval. Extend snapshot to include TTFB fields. No per-request DB query.

---

## 7. Config / settings

| Key | Type | Default | Notes |
|-----|------|---------|-------|
| `enable_ttfb_score` | bool | `false` | Requires health score path |
| `ttfb_penalty_weight` | int | `20` | `W_ttfb` |
| `ttfb_max_slow_ratio` | float/string | `2.0` | `S_max` on `(s-1)` |
| `ttfb_min_confident_sample` | int | `10` | Full penalty sample threshold |

Window/update interval reuse:

- `health_score_window_minutes`
- `health_score_update_interval`

Validation:

- weights >= 0
- samples >= 1
- max_slow_ratio >= 0 (0 disables ttfb penalty magnitude even if enabled)

Admin settings API / UI: add toggles next to existing health-score settings if those exist; otherwise settings keys only in v1 is acceptable if UI already generic for system settings.

---

## 8. Code map

| Area | Change |
|------|--------|
| `internal/model/health.go` | stats + config fields, defaults |
| `internal/storage/...` success-rate query | add TTFB avg/count |
| `internal/app/health_cache.go` | carry new fields |
| `internal/app/selector_balancer.go` | `calculateEffectivePriority` + median helper |
| `internal/app/server.go` `loadHealthScoreConfig` | load new settings |
| `internal/storage/migrate.go` / settings seed | register defaults |
| tests | formula unit + selection integration |

---

## 9. Non-goals (v1)

- Rewarding faster-than-median channels
- Absolute TTFB thresholds (e.g. always penalize >800ms)
- Using multi-URL EWMA as channel TTFB (logs first)
- Replacing Key-count RR inside buckets with TTFB weights
- Cross-model global median
- Persisting P_eff to DB

---

## 10. Tests

1. Unit: pure formula table (fast/median/slow/cap/low-sample/zero-med candidates)
2. Unit: median of odd/even candidate sets
3. Unit: ttfb disabled → identical to fail-only priority
4. Unit: health disabled → no ttfb path
5. Integration: two channels same Priority; slower TTFB gets lower P_eff and is tried later when health+ttfb on
6. Storage: success-rate query returns first-byte aggregates for successful logs only

---

## 11. Rollout

1. Ship code with defaults off
2. Enable `enable_health_score` if not already
3. Enable `enable_ttfb_score` on staging / one model pool
4. Watch channel order vs dashboard avg first-byte
5. Tune `ttfb_penalty_weight` (start 20; raise only if slow channels still steal traffic)

---

## 12. Spec self-review

- No TBD for v1 formula/params
- Compatible with existing Priority and fail penalty
- Scope limited to channel effective priority + stats aggregation
