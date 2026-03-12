const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'logs.html'), 'utf8');
const script = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');

test('logs 页分页与测试弹窗控件不再使用内联事件', () => {
  assert.doesNotMatch(html, /onclick="(?:firstLogsPage|prevLogsPage|nextLogsPage|lastLogsPage|closeTestKeyModal|runKeyTest)\(\)"/);
  assert.doesNotMatch(html, /onkeydown="if\(event\.key==='Enter'\) jumpToPage\(\)"/);
  assert.match(html, /data-action="first-logs-page"/);
  assert.match(html, /data-action="prev-logs-page"/);
  assert.match(html, /data-action="next-logs-page"/);
  assert.match(html, /data-action="last-logs-page"/);
  assert.match(html, /data-action="close-test-key-modal"/);
  assert.match(html, /data-action="run-key-test"/);
});

test('logs.js 使用集中绑定处理分页、弹窗和响应折叠', () => {
  assert.match(script, /function initLogsPageActions\(\)/);
  assert.match(script, /window\.initDelegatedActions\(\{/);
  assert.match(script, /boundKey:\s*'logsPageActionsBound'/);
  assert.match(script, /'first-logs-page':\s*\(\)\s*=> firstLogsPage\(\)/);
  assert.match(script, /'prev-logs-page':\s*\(\)\s*=> prevLogsPage\(\)/);
  assert.match(script, /'next-logs-page':\s*\(\)\s*=> nextLogsPage\(\)/);
  assert.match(script, /'last-logs-page':\s*\(\)\s*=> lastLogsPage\(\)/);
  assert.match(script, /'close-test-key-modal':\s*\(\)\s*=> closeTestKeyModal\(\)/);
  assert.match(script, /'run-key-test':\s*\(\)\s*=> runKeyTest\(\)/);
  assert.match(script, /'toggle-response':\s*\(actionTarget\)\s*=>/);
  assert.match(script, /const responseTarget = actionTarget\.dataset\.responseTarget;/);
  assert.match(script, /const jumpPageInput = document\.getElementById\('logs_jump_page'\);/);
  assert.match(script, /jumpPageInput\.addEventListener\('keydown',/);
  assert.match(script, /data-action="toggle-response"/);
  assert.doesNotMatch(script, /onclick="toggleResponse/);
  assert.match(script, /initLogsPageActions\(\);/);
});
