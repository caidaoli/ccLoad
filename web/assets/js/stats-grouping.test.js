const test = require('node:test');
const assert = require('node:assert/strict');

const {
  aggregateStatsEntries,
  groupStatsByChannel,
  mergeHealthTimelines
} = require('./stats-grouping.js');

function point(ts, success, error, extra = {}) {
  const total = success + error;
  return { ts, rate: total > 0 ? success / total : 0, success, error, ...extra };
}

const NO_DATA = { ts: 0, rate: -1 };

test('渠道分组按 channel_id 聚合，保持首次出现顺序并保留组内条目顺序', () => {
  const entries = [
    { channel_id: 2, channel_name: 'beta', model: 'm-b1', success: 1, total: 1 },
    { channel_id: 1, channel_name: 'alpha', model: 'm-a1', success: 1, total: 1 },
    { channel_id: 2, channel_name: 'beta', model: 'm-b2', success: 1, total: 1 }
  ];

  const groups = groupStatsByChannel(entries);

  assert.deepEqual(groups.map(group => group.channel_id), [2, 1]);
  assert.deepEqual(groups[0].models.map(entry => entry.model), ['m-b1', 'm-b2']);
  assert.equal(groups[0].channel_name, 'beta');
});

test('渠道汇总：计数、Token、费用相加，倍率后费用缺失时按标准费用计入', () => {
  const [group] = groupStatsByChannel([
    {
      channel_id: 7, channel_name: 'c', channel_auth_type: 'oauth', channel_priority: 5,
      cost_multiplier_min: 0.5, cost_multiplier_max: 0.8,
      model: 'a', success: 8, error: 2, total: 10,
      total_input_tokens: 100, total_output_tokens: 50,
      total_cache_read_input_tokens: 30, total_cache_creation_input_tokens: 5,
      total_cost: 1, effective_cost: 0.5
    },
    {
      channel_id: 7, channel_name: 'c', channel_auth_type: 'oauth', channel_priority: 5,
      cost_multiplier_min: 0.5, cost_multiplier_max: 0.8,
      model: 'b', success: 3, error: 1, total: 4,
      total_input_tokens: 10, total_output_tokens: 20,
      total_cost: 2
    }
  ]);

  assert.equal(group.success, 11);
  assert.equal(group.error, 3);
  assert.equal(group.total, 14);
  assert.equal(group.total_input_tokens, 110);
  assert.equal(group.total_output_tokens, 70);
  assert.equal(group.total_cache_read_input_tokens, 30);
  assert.equal(group.total_cache_creation_input_tokens, 5);
  assert.equal(group.total_cost, 3);
  assert.equal(group.effective_cost, 2.5);
  assert.equal(group.channel_auth_type, 'oauth');
  assert.equal(group.channel_priority, 5);
  assert.equal(group.cost_multiplier_min, 0.5);
  assert.equal(group.cost_multiplier_max, 0.8);
});

test('渠道 RPM：峰值取渠道级 channel_peak_rpm 而非各模型峰值之和，平均与最近相加', () => {
  const [group] = groupStatsByChannel([
    { channel_id: 1, model: 'a', peak_rpm: 2, avg_rpm: 0.5, recent_rpm: 1, channel_peak_rpm: 3 },
    { channel_id: 1, model: 'b', peak_rpm: 2, avg_rpm: 0.25, channel_peak_rpm: 3 }
  ]);

  assert.equal(group.peak_rpm, 3);
  assert.equal(group.avg_rpm, 0.75);
  assert.equal(group.recent_rpm, 1);
});

test('汇总首字/耗时按各自样本数加权，Tok/s 用总输出除以总生成秒数', () => {
  const aggregate = aggregateStatsEntries([
    // 生成秒数 = 5 - 1 = 4，共 3 次成功 → 12 秒
    { success: 3, total_output_tokens: 300, avg_first_byte_time_seconds: 1, avg_duration_seconds: 5, first_byte_count: 3, duration_count: 3 },
    // 耗时减首字不足 1 秒时退回总耗时 2 秒，共 1 次成功 → 2 秒
    { success: 1, total_output_tokens: 40, avg_first_byte_time_seconds: 1.5, avg_duration_seconds: 2, first_byte_count: 1, duration_count: 1 },
    // 非流式为主的模型：成功多但首字样本少，不能按成功数稀释首字均值
    { success: 100, avg_first_byte_time_seconds: 3, avg_duration_seconds: 1, first_byte_count: 2, duration_count: 100 },
    // 无样本不参与加权
    { success: 0, error: 4, avg_first_byte_time_seconds: 9, avg_duration_seconds: 9 }
  ]);

  assert.equal(aggregate.avg_first_byte_time_seconds, (1 * 3 + 1.5 * 1 + 3 * 2) / 6);
  assert.equal(aggregate.avg_duration_seconds, (5 * 3 + 2 * 1 + 1 * 100) / 104);
  assert.equal(aggregate.aggregate_speed, 340 / 14);
});

test('汇总无有效输出时 Tok/s 为空', () => {
  const aggregate = aggregateStatsEntries([{ success: 2, avg_duration_seconds: 3 }]);
  assert.equal(aggregate.aggregate_speed, null);
});

test('健康时间线按桶索引合并：计数相加、成功率重算、无数据桶保持未知', () => {
  const merged = mergeHealthTimelines([
    [point(100, 3, 1, { rate_limited: 1, cost: 0.5, avg_duration: 2 }), NO_DATA, point(300, 0, 2)],
    [point(100, 1, 0, { cost: 0.25, avg_duration: 6 }), NO_DATA, NO_DATA],
    undefined
  ]);

  assert.equal(merged.length, 3);
  assert.equal(merged[0].ts, 100);
  assert.equal(merged[0].success, 4);
  assert.equal(merged[0].error, 1);
  assert.equal(merged[0].rate_limited, 1);
  assert.equal(merged[0].rate, 0.8);
  assert.equal(merged[0].cost, 0.75);
  assert.equal(merged[0].avg_duration, (2 * 3 + 6 * 1) / 4);
  assert.equal(merged[1].rate, -1);
  assert.equal(merged[2].rate, 0);
  assert.equal(merged[2].error, 2);
});

test('只有一条时间线时原样返回，全部缺失时为空', () => {
  const timeline = [point(100, 1, 0)];
  assert.deepEqual(mergeHealthTimelines([undefined, timeline]), timeline);
  assert.equal(mergeHealthTimelines([undefined, []]), null);
});
