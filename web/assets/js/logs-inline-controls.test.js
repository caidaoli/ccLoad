const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const script = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');

test('logs.js 绑定清空筛选按钮并恢复默认筛选值', () => {
  assert.doesNotMatch(script, /displayedCount/);
  assert.match(script, /function getDefaultLogsFilters\(\)/);
  assert.match(script, /function resetLogsFilters\(\)/);
  assert.match(script, /document\.getElementById\('btn_clear_filters'\)\?\.addEventListener\('click', resetLogsFilters\)/);
  assert.match(script, /window\.FilterState\.restore\(\{\s*search:\s*''[\s\S]*fields:\s*LOGS_FILTER_FIELDS/);
  assert.match(script, /rememberExactLogsFilters\(\{\s*\.\.\.defaults,\s*channelNameExact:\s*false,\s*modelExact:\s*false\s*\}\)/);
  assert.match(script, /historyMethod:\s*'replaceState'/);
});
