const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'stats.html'), 'utf8');
const script = fs.readFileSync(path.join(__dirname, 'stats.js'), 'utf8');

test('stats.js 点击渠道和模型跳转日志页时使用精确匹配参数', () => {
  assert.match(script, /const channelLink = e\.target\.closest\('\.channel-link\[data-channel-name\]'\);/);
  assert.match(script, /const channelName = channelLink\.dataset\.channelName;/);
  assert.match(script, /const params = buildStatsLogLinkParams\(\{ channel_name: channelName \}\);/);
  assert.doesNotMatch(script, /logs\.html\?channel_id=/);

  assert.match(script, /const modelLink = e\.target\.closest\('\.model-link\[data-model\]'\);/);
  assert.match(script, /const channelName = modelLink\.dataset\.channelName;/);
  assert.match(script, /const params = buildStatsLogLinkParams\(\{\s*channel_name: channelName,\s*model\s*\}\);/);
  assert.doesNotMatch(script, /params\.set\('model_like', model\);/);
});

test('stats.html 和 stats.js 使用渠道名属性承载日志跳转上下文', () => {
  assert.match(html, /class="config-name channel-link" data-channel-name="\{\{channelNameAttr\}\}"/);
  assert.match(script, /data-channel-name="\$\{escapeHtml\(entry\.channel_name\)\}"/);
  assert.doesNotMatch(script, /data-channel-id="\$\{entry\.channel_id \|\| ''\}"/);
});
