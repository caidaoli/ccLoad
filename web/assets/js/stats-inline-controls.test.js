const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'stats.html'), 'utf8');
const script = fs.readFileSync(path.join(__dirname, 'stats.js'), 'utf8');

test('stats 页视图切换与排序表头不再使用内联 onclick', () => {
  assert.doesNotMatch(html, /onclick="switchView\('[^']+'\)"/);
  assert.doesNotMatch(html, /onclick="sortTable\('[^']+'\)"/);
  assert.match(html, /id="view-toggle-group"/);
  assert.match(html, /class="sortable" data-column="channel_name"/);
});

test('stats.js 使用集中绑定处理视图切换和排序点击', () => {
  assert.match(script, /function bindStatsStaticControls\(\)/);
  assert.match(script, /document\.getElementById\('view-toggle-group'\)/);
  assert.match(script, /viewToggleGroup\.addEventListener\('click',/);
  assert.match(script, /const viewBtn = e\.target\.closest\('\.view-toggle-btn\[data-view\]'\);/);
  assert.match(script, /switchView\(viewBtn\.dataset\.view\);/);
  assert.match(script, /document\.querySelector\('\.stats-table thead'\)/);
  assert.match(script, /thead\.addEventListener\('click',/);
  assert.match(script, /const sortable = e\.target\.closest\('\.sortable\[data-column\]'\);/);
  assert.match(script, /sortTable\(sortable\.dataset\.column\);/);
  assert.match(script, /bindStatsStaticControls\(\);/);
});

test('stats.js 点击渠道和模型跳转日志页时使用精确匹配参数', () => {
  assert.match(script, /const channelLink = e\.target\.closest\('\.channel-link\[data-channel-name\]'\);/);
  assert.match(script, /const channelName = channelLink\.dataset\.channelName;/);
  assert.match(script, /const params = new URLSearchParams\(\);[\s\S]*params\.set\('channel_name', channelName\);/);
  assert.match(script, /params\.set\('channel_name', channelName\);/);
  assert.doesNotMatch(script, /logs\.html\?channel_id=/);

  assert.match(script, /const modelLink = e\.target\.closest\('\.model-link\[data-model\]'\);/);
  assert.match(script, /const channelName = modelLink\.dataset\.channelName;/);
  assert.match(script, /if \(channelName\) params\.set\('channel_name', channelName\);/);
  assert.match(script, /params\.set\('model', model\);/);
  assert.doesNotMatch(script, /params\.set\('model_like', model\);/);
});

test('stats.html 和 stats.js 使用渠道名属性承载日志跳转上下文', () => {
  assert.match(html, /class="config-name channel-link" data-channel-name="\{\{channelNameAttr\}\}"/);
  assert.match(script, /data-channel-name="\$\{escapeHtml\(entry\.channel_name\)\}"/);
  assert.doesNotMatch(script, /data-channel-id="\$\{entry\.channel_id \|\| ''\}"/);
});
