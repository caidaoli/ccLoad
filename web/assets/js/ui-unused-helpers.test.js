const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const source = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');

test('ui.js 不再保留未使用的渠道类型搜索/Tab helper', () => {
  assert.doesNotMatch(source, /\brenderSearchableChannelTypeSelect\b/);
  assert.doesNotMatch(source, /\bgetSearchableSelectValue\b/);
  assert.doesNotMatch(source, /\brenderChannelTypeTabs\b/);
});
