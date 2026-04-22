const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');

test('活跃请求渠道显示函数 buildActiveRequestChannelDisplay 存在', () => {
  assert.match(logsSource, /function buildActiveRequestChannelDisplay\(req\)/);
});

test('活跃请求渠道显示在无渠道时返回选择中提示', () => {
  assert.match(logsSource, /if \(!req\.channel_id \|\| !req\.channel_name\) \{[\s\S]*?return '<span style="color: var\(--neutral-500\);">选择中\.\.\.<\/span>';/);
});

test('活跃请求渠道显示在倍率为1时不显示角标', () => {
  assert.match(logsSource, /const multiplier = Number\(req\.cost_multiplier\);/);
  assert.match(logsSource, /if \(!Number\.isFinite\(multiplier\) \|\| multiplier < 0 \|\| Math\.abs\(multiplier - 1\) < 1e-9\) \{[\s\S]*?return channelHtml;/);
});

test('活跃请求渠道显示在倍率非1时显示角标', () => {
  assert.match(logsSource, /const multiplierText = `\$\{Number\(multiplier\.toFixed\(2\)\)\.toString\(\)\}x`;/);
  assert.match(logsSource, /return `<span class="log-channel-cell">\$\{channelHtml\}<sup class="log-channel-multiplier-badge">\$\{multiplierText\}<\/sup><\/span>`;/);
});

test('活跃请求渠道显示在倍率为0时保留 0x 角标', () => {
  assert.match(logsSource, /const multiplier = Number\(req\.cost_multiplier\);/);
  assert.doesNotMatch(logsSource, /if \(!\(multiplier > 0\) \|\| Math\.abs\(multiplier - 1\) < 1e-9\) \{/);
  assert.match(logsSource, /const multiplierText = `\$\{Number\(multiplier\.toFixed\(2\)\)\.toString\(\)\}x`;/);
});

test('renderActiveRequests 使用 buildActiveRequestChannelDisplay 构建渠道显示', () => {
  assert.match(logsSource, /function renderActiveRequests\(activeRequests\) \{[\s\S]*?const channelDisplay = buildActiveRequestChannelDisplay\(req\);/);
});

test('活跃请求渲染不再直接调用 buildChannelTrigger', () => {
  const renderActiveRequestsMatch = logsSource.match(/function renderActiveRequests\(activeRequests\) \{[\s\S]*?\n\}/);
  assert.ok(renderActiveRequestsMatch, '未找到 renderActiveRequests 函数');
  const renderActiveRequestsBody = renderActiveRequestsMatch[0];
  assert.doesNotMatch(renderActiveRequestsBody, /channelDisplay = buildChannelTrigger/);
});
