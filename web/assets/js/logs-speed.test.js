const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'logs.html'), 'utf8');
const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');
const zhLocale = fs.readFileSync(path.join(__dirname, '..', 'locales', 'zh-CN.js'), 'utf8');
const enLocale = fs.readFileSync(path.join(__dirname, '..', 'locales', 'en.js'), 'utf8');

function extractFunction(source, name) {
  const pattern = new RegExp(`function ${name}\\([^)]*\\) \\{[\\s\\S]*?\\n\\}`, 'm');
  const match = source.match(pattern);
  assert.ok(match, `缺少函数 ${name}`);
  return match[0];
}

test('日志页表头新增速度列并补齐中英文本地化', () => {
  assert.match(html, /<th data-i18n="logs\.colSpeed">速度\(tok\/s\)<\/th>/);
  assert.match(zhLocale, /'logs\.colSpeed': '速度\(tok\/s\)'/);
  assert.match(enLocale, /'logs\.colSpeed': 'Speed \(tok\/s\)'/);
});

test('日志页移动端标签与表格渲染包含速度列', () => {
  const getLogMobileLabelsSource = extractFunction(logsSource, 'getLogMobileLabels');
  const renderLogsSource = extractFunction(logsSource, 'renderLogs');
  const renderActiveRequestsSource = extractFunction(logsSource, 'renderActiveRequests');

  assert.match(getLogMobileLabelsSource, /speed:\s*escapeHtml\(t\('logs\.colSpeed'\)\)/);
  assert.match(renderLogsSource, /class="logs-col-speed/);
  assert.match(renderLogsSource, /data-mobile-label="\$\{logMobileLabels\.speed\}"/);
  assert.match(renderActiveRequestsSource, /class="logs-col-speed/);
  assert.match(renderActiveRequestsSource, /data-mobile-label="\$\{logMobileLabels\.speed\}"/);
});

test('日志页速度计算沿用令牌明细中的 tok/s 语义', () => {
  const calculateLogSpeed = vm.runInNewContext(
    `(${extractFunction(logsSource, 'calculateLogSpeed')})`,
    {}
  );

  assert.equal(
    calculateLogSpeed({
      is_streaming: true,
      output_tokens: 957,
      duration: 21.0,
      first_byte_time: 3.2
    }),
    53.764044943820224
  );

  assert.equal(
    calculateLogSpeed({
      is_streaming: false,
      output_tokens: 736,
      duration: 17.0,
      first_byte_time: 2.9
    }),
    43.294117647058826
  );

  assert.equal(
    calculateLogSpeed({
      is_streaming: true,
      output_tokens: 100,
      duration: 3,
      first_byte_time: 3
    }),
    null
  );

  assert.equal(
    calculateLogSpeed({
      is_streaming: true,
      output_tokens: 0,
      duration: 12,
      first_byte_time: 2
    }),
    null
  );
});

test('日志页速度单元格只显示数值不重复单位', () => {
  assert.doesNotMatch(
    logsSource,
    /speedDisplay[^]*tok\/s/,
    '速度列单元格不应重复拼接 tok/s'
  );
});

test('日志页速度单元格保留一位小数', () => {
  assert.match(
    logsSource,
    /speedDisplay[^]*logSpeed\.toFixed\(1\)/,
    '速度列应使用一位小数格式渲染'
  );
});
