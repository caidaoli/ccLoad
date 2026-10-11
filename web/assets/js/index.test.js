const test = require('node:test');
const assert = require('node:assert/strict');

const ServiceHealth = require('./service-health.js');

test('service health request preserves the dashboard date range exactly', () => {
  const request = ServiceHealth.buildRequest(
    'range=custom&start_time=1785456000000&end_time=1785542399000',
    24
  );
  const params = new URLSearchParams(request.query);

  assert.equal(params.get('range'), 'custom');
  assert.equal(params.get('start_time'), '1785456000000');
  assert.equal(params.get('end_time'), '1785542399000');
  assert.equal(params.get('bucket_min'), '15');
  assert.equal(request.bucketMinutes, 15);

  const monthRequest = ServiceHealth.buildRequest('range=last_month', 31 * 24);
  assert.equal(monthRequest.bucketMinutes, 120);
});

test('service health normalizes only the metric buckets returned for the selected range', () => {
  const startMs = Date.UTC(2026, 6, 31, 0, 0);
  const bucketMs = 15 * 60 * 1000;
  const metrics = [
    { ts: new Date(startMs).toISOString(), success: 19, error: 1 },
    { ts: new Date(startMs + bucketMs).toISOString(), success: 4, error: 1, rate_limited: 1 },
    { ts: new Date(startMs + 2 * bucketMs).toISOString(), success: 1, error: 4, rate_limited: 2 },
    { ts: 'invalid', success: 100, error: 0 }
  ];

  const model = ServiceHealth.buildModel(metrics, 15);

  assert.equal(model.bucketMs, bucketMs);
  assert.equal(model.points.length, 3);
  assert.deepEqual(model.points.map(point => point.state), [
    'healthy',
    'warning',
    'critical'
  ]);
  assert.deepEqual(model.points.map(point => point.rate), [0.95, 0.8, 0.2]);
  assert.equal(model.success, 24);
  assert.equal(model.error, 6);
  assert.deepEqual(model.points.map(point => point.rateLimited), [0, 1, 2]);
  assert.equal(model.rateLimited, 3);
  assert.equal(model.rate, 0.8);
  assert.equal(model.state, 'warning');
});

test('service health pads an in-progress range with unknown buckets up to its last bucket', () => {
  const now = new Date(2026, 9, 14, 10, 7).getTime(); // 周三
  const dayStart = new Date(2026, 9, 14).getTime();
  const bucketMs = 15 * 60 * 1000;
  const endOfToday = ServiceHealth.rangeEndMs('today', null, now);
  assert.equal(endOfToday, new Date(2026, 9, 15).getTime() - 1);
  assert.equal(ServiceHealth.rangeEndMs('this_week', null, now), new Date(2026, 9, 19).getTime() - 1);
  assert.equal(ServiceHealth.rangeEndMs('this_month', null, now), new Date(2026, 10, 1).getTime() - 1);
  assert.equal(ServiceHealth.rangeEndMs('custom', { startMs: dayStart, endMs: now + 3600000 }, now), now + 3600000);
  assert.equal(ServiceHealth.rangeEndMs('yesterday', null, now), null);

  const metrics = [
    { ts: dayStart, success: 10, error: 0 },
    { ts: dayStart + 40 * bucketMs, success: 9, error: 1 }
  ];
  const model = ServiceHealth.buildModel(metrics, 15, endOfToday);

  assert.equal(model.points.length, 96);
  assert.equal(model.points.at(-1).ts, new Date(2026, 9, 14, 23, 45).getTime());
  assert.equal(model.points[40].state, 'warning');
  assert.ok(model.points.slice(41).every(point => point.state === 'unknown' && point.rate === null && point.pending));
  assert.ok(model.points.slice(0, 41).every(point => !point.pending));
  assert.equal(model.success, 19);
  assert.equal(model.error, 1);

  assert.equal(ServiceHealth.buildModel(metrics, 15).points.length, 41);
  assert.deepEqual(ServiceHealth.buildModel([], 15, endOfToday).points, []);
});

test('service health treats empty and malformed metrics as unknown instead of healthy', () => {
  const model = ServiceHealth.buildModel([
    { ts: 'invalid', success: -1, error: 'bad' }
  ], 15);

  assert.deepEqual(model.points, []);
  assert.equal(model.rate, null);
  assert.equal(model.state, 'unknown');
});

test('overview totals take request counts from the summary and sum cost and output tokens across protocols', () => {
  const { buildOverviewTotals } = require('./index.js');
  const totals = buildOverviewTotals({
    total_requests: 251,
    success_requests: 250,
    error_requests: 1,
    by_client_protocol: {
      gemini: { total_requests: 231, total_cost: 1.75, effective_cost: 0.875, total_output_tokens: 44500 },
      openai: { total_requests: 20, total_cost: 0.009, total_output_tokens: 673 },
      anthropic: { total_requests: 0, effective_cost: 0 }
    }
  });

  assert.equal(totals.requests, 251);
  assert.equal(totals.success, 250);
  assert.equal(totals.error, 1);
  assert.equal(totals.rate, 250 / 251);
  assert.ok(Math.abs(totals.cost - 1.759) < 1e-9);
  // 缺少 effective_cost 的协议按 total_cost 计入
  assert.ok(Math.abs(totals.effectiveCost - 0.884) < 1e-9);
  assert.equal(totals.outputTokens, 45173);

  assert.deepEqual(buildOverviewTotals({}), {
    requests: 0, success: 0, error: 0, rate: null, cost: 0, effectiveCost: 0, outputTokens: 0
  });
});

test('dashboard time range restored from URL/storage falls back to today when invalid', () => {
  const { resolveIndexTimeRange } = require('./index.js');

  assert.deepEqual(resolveIndexTimeRange({ range: 'this_week' }), { range: 'this_week', customRange: null });
  assert.deepEqual(
    resolveIndexTimeRange({ range: 'custom', customStartTime: '1785456000000', customEndTime: '1785542399000' }),
    { range: 'custom', customRange: { startMs: 1785456000000, endMs: 1785542399000 } }
  );
  // 自定义区间缺失或倒置、未知范围值都回落到本日
  assert.deepEqual(resolveIndexTimeRange({ range: 'custom', customStartTime: '', customEndTime: '' }), { range: 'today', customRange: null });
  assert.deepEqual(resolveIndexTimeRange({ range: 'custom', customStartTime: '200', customEndTime: '100' }), { range: 'today', customRange: null });
  assert.deepEqual(resolveIndexTimeRange({ range: 'last_year' }), { range: 'today', customRange: null });
  for (const range of ['last_1h', 'last_6h', 'last_24h']) {
    assert.deepEqual(resolveIndexTimeRange({ range }), { range: 'today', customRange: null });
  }
});


test('rolling date presets retain exact durations and calendar presets', () => {
  const vm = require('node:vm');
  const fs = require('node:fs');
  const window = {};
  vm.runInNewContext(fs.readFileSync(require.resolve('./date-range-selector.js'), 'utf8'), { window });
  for (const hours of [1, 6, 24]) {
    const range = `last_${hours}h`;
    assert.equal(window.getRangeHours(range), hours);
    assert.equal(window.getDateRangePresets().some(preset => preset.value === range), true);
    assert.equal(new URLSearchParams(window.buildDateRangeQuery(range)).get('range'), range);
  }
  assert.equal(window.getRangeHours('last_week'), 168);
  assert.equal(window.getRangeHours('last_month'), 720);
});
