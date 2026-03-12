const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'settings.html'), 'utf8');
const script = fs.readFileSync(path.join(__dirname, 'settings.js'), 'utf8');

test('settings 页保存按钮不再使用内联 onclick', () => {
  assert.doesNotMatch(html, /onclick="saveAllSettings\(\)"/);
  assert.match(html, /id="save-all-btn"/);
});

test('settings.js 在脚本中绑定保存按钮点击事件', () => {
  assert.match(script, /window\.initPageBootstrap\(\{/);
  assert.match(script, /topbarKey:\s*'settings'/);
  assert.match(script, /function bindSettingsPageActions\(\)/);
  assert.match(script, /document\.getElementById\('save-all-btn'\)/);
  assert.match(script, /saveAllBtn\.addEventListener\('click',\s*\(\)\s*=>\s*\{\s*saveAllSettings\(\);/s);
  assert.match(script, /bindSettingsPageActions\(\);/);
  assert.match(script, /loadSettings\(\);/);
});
