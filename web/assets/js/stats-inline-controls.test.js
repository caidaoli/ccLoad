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
