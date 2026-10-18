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

test('overview totals take request counts from the summary and sum cost and tokens across protocols', () => {
  const { buildOverviewTotals } = require('./index.js');
  const totals = buildOverviewTotals({
    total_requests: 251,
    success_requests: 250,
    error_requests: 1,
    by_client_protocol: {
      gemini: {
        total_requests: 231,
        total_cost: 1.75,
        effective_cost: 0.875,
        total_input_tokens: 1000,
        total_output_tokens: 44500,
        total_cache_read_tokens: 6000
      },
      openai: {
        total_requests: 20,
        total_cost: 0.009,
        total_input_tokens: 500,
        total_output_tokens: 673,
        total_cache_read_tokens: 2000,
        total_cache_creation_tokens: 500
      },
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
  assert.equal(totals.inputTokens, 1500);
  assert.equal(totals.outputTokens, 45173);
  assert.equal(totals.cacheReadTokens, 8000);
  assert.equal(totals.cacheCreationTokens, 500);
  // input_tokens 已不含缓存读写，四类相加即总量；缓存率以完整 prompt 为分母
  assert.equal(totals.promptTokens, 10000);
  assert.equal(totals.totalTokens, 55173);
  assert.equal(totals.cacheRate, 0.8);

  assert.deepEqual(buildOverviewTotals({}), {
    requests: 0,
    success: 0,
    error: 0,
    rate: null,
    cost: 0,
    effectiveCost: 0,
    inputTokens: 0,
    outputTokens: 0,
    cacheReadTokens: 0,
    cacheCreationTokens: 0,
    promptTokens: 0,
    totalTokens: 0,
    cacheRate: null
  });
});

test('overview spark series merges adjacent metric buckets beyond the point budget and keeps each point time range', () => {
  const { buildSparkSeries } = require('./index.js');
  const metrics = Array.from({ length: 192 }, (_, index) => ({
    ts: new Date(Date.UTC(2026, 9, 14) + index * 15 * 60 * 1000).toISOString(),
    success: 1,
    error: index % 2,
    total_cost: 0.2,
    effective_cost: index % 2 === 0 ? 0.1 : undefined
  }));

  const series = buildSparkSeries(metrics, 15);

  assert.equal(series.requests.length, 96);
  assert.ok(series.requests.every(value => value === 3));
  assert.ok(series.errors.every(value => value === 1));
  // 缺少 effective_cost 的桶按 total_cost 计入
  assert.ok(series.cost.every(value => Math.abs(value - 0.3) < 1e-9));
  // 每个点覆盖合并后的两个 15 分钟桶
  const dayStart = Date.UTC(2026, 9, 14);
  assert.deepEqual([series.starts[0], series.ends[0]], [dayStart, dayStart + 30 * 60 * 1000]);
  assert.deepEqual([series.starts[95], series.ends[95]], [dayStart + 95 * 30 * 60 * 1000, dayStart + 48 * 3600 * 1000]);

  // 不足整组的末尾点只覆盖实际包含的桶
  const uneven = buildSparkSeries(metrics.slice(0, 97), 15);
  assert.equal(uneven.requests.length, 49);
  assert.equal(uneven.ends[48] - uneven.starts[48], 15 * 60 * 1000);

  assert.deepEqual(buildSparkSeries(null, 15), { requests: [], errors: [], cost: [], starts: [], ends: [] });
  assert.deepEqual(buildSparkSeries([{ success: 2, error: 1 }], 15), {
    requests: [3], errors: [1], cost: [0], starts: [null], ends: [null]
  });
});

test('usage ranking reads model and channel totals from the summary and ranks the top entries by metric', () => {
  const { buildSummaryUsageRows, rankUsageRows } = require('./index.js');
  const { model: models, channel: channels } = buildSummaryUsageRows({
    by_model: [
      { model: 'gpt-5.6', total_requests: 30, total_input_tokens: 100, total_output_tokens: 50, total_cache_creation_tokens: 30, total_cost: 1.5 },
      { model: 'gpt-6', total_requests: 6, total_cache_read_tokens: 400, total_cost: 2, effective_cost: 1 }
    ],
    by_channel: [
      { channel_id: 1, channel_name: 'alpha', auth_type: 'codex_oauth', total_requests: 15, total_input_tokens: 100, total_output_tokens: 50, total_cache_read_tokens: 400, total_cost: 3, effective_cost: 2 },
      // 已删除的渠道没有名称与认证类型
      { channel_id: 2, channel_name: '', total_requests: 20, total_cache_creation_tokens: 30, total_cost: 0.5 }
    ]
  });

  assert.deepEqual(channels.map(row => [row.key, row.label, row.authType, row.requests, row.tokens, row.cost]), [
    ['1', 'alpha', 'codex_oauth', 15, 550, 2],
    ['2', '#2', '', 20, 30, 0.5]
  ]);
  assert.deepEqual(models.map(row => [row.key, row.label, row.requests, row.tokens, row.cost]), [
    ['gpt-5.6', 'gpt-5.6', 30, 180, 1.5],
    ['gpt-6', 'gpt-6', 6, 400, 1]
  ]);
  // 隐藏渠道的会话没有 by_channel
  assert.deepEqual(buildSummaryUsageRows({ by_model: null }), { model: [], channel: [] });

  assert.deepEqual(rankUsageRows(channels, 'requests').map(row => [row.key, row.share]), [
    ['2', 20 / 35],
    ['1', 15 / 35]
  ]);
  assert.deepEqual(rankUsageRows(models, 'tokens').map(row => row.key), ['gpt-6', 'gpt-5.6']);

  // 指标为零的行不上榜；前 N 名的占比仍以全部行之和为分母
  const rows = [
    { key: 'a', cost: 6 }, { key: 'b', cost: 0 }, { key: 'c', cost: 3 }, { key: 'd', cost: 1 }
  ];
  assert.deepEqual(rankUsageRows(rows, 'cost', 2).map(row => [row.key, row.share]), [
    ['a', 0.6],
    ['c', 0.3]
  ]);
  assert.deepEqual(rankUsageRows([{ key: 'z', cost: 0 }], 'cost'), []);
});

test('usage ranking rows link to logs filtered by the same date range', () => {
  const { buildSummaryUsageRows, buildUsageLogsHref } = require('./index.js');
  const { model: models, channel: channels } = buildSummaryUsageRows({
    by_model: [{ model: 'claude-sonnet-4-6', total_requests: 1 }],
    by_channel: [
      { channel_id: 1, channel_name: 'alpha & beta', total_requests: 1 },
      { channel_id: 2, channel_name: '', total_requests: 1 }
    ]
  });
  const customRange = 'range=custom&start_time=1785456000000&end_time=1785542399000';
  const logsQuery = href => {
    const url = new URL(href, 'http://localhost');
    assert.equal(url.pathname, '/web/logs.html');
    return Object.fromEntries(url.searchParams);
  };

  assert.deepEqual(logsQuery(buildUsageLogsHref(customRange, models[0].logsFilter)), {
    range: 'custom', start_time: '1785456000000', end_time: '1785542399000', model: 'claude-sonnet-4-6'
  });
  assert.deepEqual(logsQuery(buildUsageLogsHref('range=this_week', channels[0].logsFilter)), {
    range: 'this_week', channel_name: 'alpha & beta'
  });
  assert.deepEqual(logsQuery(buildUsageLogsHref('range=today', { client_protocol: 'codex' })), {
    range: 'today', client_protocol: 'codex'
  });
  // 已删除的渠道无名称可筛，不生成链接
  assert.equal(buildUsageLogsHref('range=today', channels[1].logsFilter), '');
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
