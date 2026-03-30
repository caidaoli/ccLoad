const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'stats.html'), 'utf8');
const script = fs.readFileSync(path.join(__dirname, 'stats.js'), 'utf8');
const zhLocale = fs.readFileSync(path.join(__dirname, '..', 'locales', 'zh-CN.js'), 'utf8');
const enLocale = fs.readFileSync(path.join(__dirname, '..', 'locales', 'en.js'), 'utf8');

function extractFunction(source, name) {
  const signature = `function ${name}`;
  const start = source.indexOf(signature);
  assert.ok(start >= 0, `缺少函数 ${name}`);

  const braceStart = source.indexOf('{', start);
  assert.ok(braceStart >= 0, `函数 ${name} 缺少起始大括号`);

  let depth = 0;
  for (let i = braceStart; i < source.length; i++) {
    const char = source[i];
    if (char === '{') depth++;
    if (char === '}') depth--;
    if (depth === 0) {
      return source.slice(start, i + 1);
    }
  }

  assert.fail(`函数 ${name} 大括号未闭合`);
}

test('stats 页表头和模板新增平均速度列并补齐文案', () => {
  assert.match(html, /data-column="avg_speed"[\s\S]*data-i18n="stats\.avgSpeed"/);
  assert.match(html, /class="stats-col-speed [^"]*" data-mobile-label="\{\{mobileLabelSpeed\}\}"/);
  assert.match(zhLocale, /'stats\.avgSpeed': '平均速度\(tok\/s\)'/);
  assert.match(enLocale, /'stats\.avgSpeed': 'Avg Speed \(tok\/s\)'/);
});

test('stats 页平均速度单元格只显示数值不重复单位', () => {
  assert.doesNotMatch(
    script,
    /avgSpeed[^]*tok\/s/,
    '平均速度列单元格不应重复拼接 tok/s'
  );
});

test('stats 页平均速度公式按成功请求数折算总生成时长', () => {
  const calculateAverageSpeed = vm.runInNewContext(
    `(${extractFunction(script, 'calculateAverageSpeed')})`,
    {}
  );

  assert.equal(
    calculateAverageSpeed({
      success: 152,
      total_output_tokens: 97200,
      avg_duration_seconds: 17.99,
      avg_first_byte_time_seconds: 3.43
    }),
    43.919895893580104
  );

  assert.equal(
    calculateAverageSpeed({
      success: 4,
      total_output_tokens: 400,
      avg_duration_seconds: 20,
      avg_first_byte_time_seconds: 0
    }),
    5
  );

  assert.equal(
    calculateAverageSpeed({
      success: 0,
      total_output_tokens: 400,
      avg_duration_seconds: 20,
      avg_first_byte_time_seconds: 0
    }),
    null
  );

  assert.equal(
    calculateAverageSpeed({
      success: 1,
      total_output_tokens: 400,
      avg_duration_seconds: 3,
      avg_first_byte_time_seconds: 3
    }),
    null
  );
});

test('stats 页支持按平均速度排序', () => {
  const context = {
    statsData: {
      stats: [
        {
          channel_name: 'slow',
          success: 10,
          total_output_tokens: 1000,
          avg_duration_seconds: 20,
          avg_first_byte_time_seconds: 5
        },
        {
          channel_name: 'fast',
          success: 10,
          total_output_tokens: 1000,
          avg_duration_seconds: 6,
          avg_first_byte_time_seconds: 1
        },
        {
          channel_name: 'empty',
          success: 0,
          total_output_tokens: 0,
          avg_duration_seconds: 0,
          avg_first_byte_time_seconds: 0
        }
      ]
    },
    sortState: {
      column: 'avg_speed',
      order: 'desc'
    }
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'calculateAverageSpeed')}
    ${extractFunction(script, 'applySorting')}
  `, context);

  context.applySorting();
  assert.deepEqual(
    context.statsData.stats.map((entry) => entry.channel_name),
    ['fast', 'slow', 'empty']
  );
});
