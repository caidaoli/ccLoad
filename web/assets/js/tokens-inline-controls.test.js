const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'tokens.html'), 'utf8');
const script = fs.readFileSync(path.join(__dirname, 'tokens.js'), 'utf8');
test('tokens 页静态控件不再使用 HTML 内联事件', () => {
  assert.doesNotMatch(html, /\s(?:onclick|onchange|oninput)=/);
});

test('tokens 页静态控件改为 data-action/data-change-action/data-input-action 标记', () => {
  assert.match(html, /data-action="show-create-modal"/);
  assert.match(html, /data-action="close-create-modal"/);
  assert.match(html, /data-action="create-token"/);
  assert.match(html, /data-change-action="toggle-custom-expiry"/);
  assert.match(html, /data-action="close-token-result-modal"/);
  assert.match(html, /data-action="copy-token-result"/);
  assert.match(html, /data-action="close-edit-modal"/);
  assert.match(html, /data-action="update-token"/);
  assert.match(html, /data-action="show-model-select-modal"/);
  assert.match(html, /data-action="show-model-import-modal"/);
  assert.match(html, /data-action="batch-delete-allowed-models"/);
  assert.match(html, /data-change-action="toggle-select-all-allowed-models"/);
  assert.match(html, /data-change-action="toggle-edit-custom-expiry"/);
  assert.match(html, /data-action="close-model-select-modal"/);
  assert.match(html, /data-input-action="filter-available-models"/);
  assert.match(html, /data-change-action="toggle-select-all-models"/);
  assert.match(html, /data-action="confirm-model-selection"/);
  assert.match(html, /data-action="close-model-import-modal"/);
  assert.match(html, /data-input-action="update-model-import-preview"/);
  assert.match(html, /data-action="confirm-model-import"/);
});

test('tokens.js 通过委托处理页面控件和动态 allowed-model 行', () => {
  assert.match(script, /window\.initPageBootstrap\(\{/);
  assert.match(script, /topbarKey:\s*'tokens'/);
  assert.match(script, /function initPageActionDelegation\(\)/);
  assert.match(script, /window\.initDelegatedActions\(\{/);
  assert.match(script, /boundKey:\s*'tokensPageActionsBound'/);
  assert.match(script, /'show-create-modal':\s*\(\)\s*=> showCreateModal\(\)/);
  assert.match(script, /'create-token':\s*\(\)\s*=> createToken\(\)/);
  assert.match(script, /'show-model-select-modal':\s*\(\)\s*=> showModelSelectModal\(\)/);
  assert.match(script, /'confirm-model-import':\s*\(\)\s*=> confirmModelImport\(\)/);
  assert.match(script, /'toggle-custom-expiry':\s*\(actionTarget\)\s*=>/);
  assert.match(script, /'toggle-edit-custom-expiry':\s*\(actionTarget\)\s*=>/);
  assert.match(script, /'toggle-select-all-allowed-models':\s*\(actionTarget\)\s*=> toggleSelectAllAllowedModels\(actionTarget\.checked\)/);
  assert.match(script, /'toggle-select-all-models':\s*\(actionTarget\)\s*=> toggleSelectAllModels\(actionTarget\.checked\)/);
  assert.match(script, /'toggle-allowed-model':\s*\(actionTarget\)\s*=>/);
  assert.match(script, /'filter-available-models':\s*\(actionTarget\)\s*=> filterAvailableModels\(actionTarget\.value\)/);
  assert.match(script, /'update-model-import-preview':\s*\(\)\s*=> updateModelImportPreview\(\)/);
  assert.match(script, /data-change-action="toggle-allowed-model"/);
  assert.match(script, /data-action="remove-allowed-model"/);
  assert.doesNotMatch(script, /onchange="toggleAllowedModelSelection/);
  assert.doesNotMatch(script, /onclick="removeAllowedModel/);
  assert.match(script, /initPageActionDelegation\(\);/);
});
