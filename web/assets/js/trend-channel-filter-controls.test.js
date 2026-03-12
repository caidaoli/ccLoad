const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const trendHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'trend.html'), 'utf8');
const trendScript = fs.readFileSync(path.join(__dirname, 'trend.js'), 'utf8');

test('trend 页渠道筛选控件不再使用内联 onclick', () => {
  assert.doesNotMatch(trendHtml, /onclick="toggleChannelFilter\(\)"/);
  assert.doesNotMatch(trendHtml, /onclick="selectAllChannels\(\)"/);
  assert.doesNotMatch(trendHtml, /onclick="clearAllChannels\(\)"/);
  assert.match(trendHtml, /id="btn-channel-filter-toggle"/);
  assert.match(trendHtml, /id="btn-select-all-channels"/);
  assert.match(trendHtml, /id="btn-clear-all-channels"/);
});

test('trend.js 在脚本中集中绑定渠道筛选控件事件', () => {
  assert.match(trendScript, /function bindChannelFilterControls\(\)/);
  assert.match(trendScript, /document\.getElementById\('btn-channel-filter-toggle'\)/);
  assert.match(trendScript, /channelFilterToggle\.addEventListener\('click',\s*\(\)\s*=>\s*\{\s*toggleChannelFilter\(\);/s);
  assert.match(trendScript, /document\.getElementById\('btn-select-all-channels'\)/);
  assert.match(trendScript, /selectAllBtn\.addEventListener\('click',\s*\(\)\s*=>\s*\{\s*selectAllChannels\(\);/s);
  assert.match(trendScript, /document\.getElementById\('btn-clear-all-channels'\)/);
  assert.match(trendScript, /clearAllBtn\.addEventListener\('click',\s*\(\)\s*=>\s*\{\s*clearAllChannels\(\);/s);
  assert.match(trendScript, /bindChannelFilterControls\(\);/);
});
