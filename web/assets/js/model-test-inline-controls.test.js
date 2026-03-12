const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'model-test.html'), 'utf8');
const script = fs.readFileSync(path.join(__dirname, 'model-test.js'), 'utf8');

test('model-test 页静态控件不再使用内联事件', () => {
  assert.doesNotMatch(html, /onclick="(?:setTestMode|selectAllModels|deselectAllModels|fetchAndAddModels|deleteSelectedModels|runModelTests)\([^"]*\)"/);
  assert.doesNotMatch(html, /onchange="toggleAllModels\(this\.checked\)"/);
  assert.match(html, /data-action="set-test-mode"/);
  assert.match(html, /data-action="select-all-models"/);
  assert.match(html, /data-action="deselect-all-models"/);
  assert.match(html, /data-action="fetch-and-add-models"/);
  assert.match(html, /data-action="delete-selected-models"/);
  assert.match(html, /data-action="run-model-tests"/);
  assert.match(html, /data-change-action="toggle-all-models"/);
});

test('model-test.js 使用集中绑定处理页面控件和重渲染表头复选框', () => {
  assert.match(script, /window\.initPageBootstrap\(\{/);
  assert.match(script, /topbarKey:\s*'model-test'/);
  assert.match(script, /function initModelTestActions\(\)/);
  assert.match(script, /window\.initDelegatedActions\(\{/);
  assert.match(script, /boundKey:\s*'modelTestActionsBound'/);
  assert.match(script, /'set-test-mode':\s*\(actionTarget\)\s*=> setTestMode\(actionTarget\.dataset\.mode \|\| ''\)/);
  assert.match(script, /'select-all-models':\s*\(\)\s*=> selectAllModels\(\)/);
  assert.match(script, /'deselect-all-models':\s*\(\)\s*=> deselectAllModels\(\)/);
  assert.match(script, /'fetch-and-add-models':\s*\(\)\s*=> fetchAndAddModels\(\)/);
  assert.match(script, /'delete-selected-models':\s*\(\)\s*=> deleteSelectedModels\(\)/);
  assert.match(script, /'run-model-tests':\s*\(\)\s*=> runModelTests\(\)/);
  assert.match(script, /'toggle-all-models':\s*\(actionTarget\)\s*=> toggleAllModels\(actionTarget\.checked\)/);
  assert.match(script, /data-change-action="toggle-all-models"/);
  assert.doesNotMatch(script, /onchange="toggleAllModels/);
  assert.match(script, /bootstrap\(\);/);
});
