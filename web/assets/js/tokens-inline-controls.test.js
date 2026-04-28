const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'tokens.html'), 'utf8');
const script = fs.readFileSync(path.join(__dirname, 'tokens.js'), 'utf8');
const css = fs.readFileSync(path.join(__dirname, '..', 'css', 'tokens.css'), 'utf8');

function extractFunction(source, name) {
  const signature = `function ${name}`;
  const start = source.indexOf(signature);
  assert.ok(start >= 0, `缺少函数 ${name}`);

  const braceStart = source.indexOf('{', start);
  assert.ok(braceStart >= 0, `函数 ${name} 缺少起始大括号`);

  let depth = 0;
  for (let i = braceStart; i < source.length; i++) {
    const char = source[i];
    if (char === '{') depth++;
    if (char === '}') depth--;
    if (depth === 0) {
      return source.slice(start, i + 1);
    }
  }

  assert.fail(`函数 ${name} 大括号未闭合`);
}
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

test('tokens 页费用和并发上限常驻说明 0 表示无限制', () => {
  assert.match(html, /data-i18n="tokens\.zeroUnlimitedHint">0 表示无限制<\/span>/);
  assert.equal((html.match(/data-i18n="tokens\.zeroUnlimitedHint"/g) || []).length, 4);
  assert.match(html, /id="tokenCostLimitUSD"[\s\S]*?class="token-limit-hint token-limit-hint--inline"[\s\S]*?id="tokenMaxConcurrency"[\s\S]*?class="token-limit-hint token-limit-hint--inline"/);
  assert.match(html, /id="editCostLimitUSD"[\s\S]*?class="token-limit-hint token-limit-hint--inline"[\s\S]*?id="editMaxConcurrency"[\s\S]*?class="token-limit-hint token-limit-hint--inline"/);
});

test('tokens 页费用和并发上限输入框使用一致前缀槽位保持对齐', () => {
  assert.equal((html.match(/class="token-limit-prefix-slot token-limit-prefix-slot--empty"/g) || []).length, 2);
  assert.match(html, /id="tokenCostLimitUSD"[\s\S]*?id="tokenMaxConcurrency"/);
  assert.match(html, /token-cost-prefix token-limit-prefix-slot/);
  assert.match(html, /token-edit-cost-prefix token-limit-prefix-slot/);
  assert.match(css, /\.form-row-inline:has\(>\s*\.token-limit-control\)\s*\{[\s\S]*?align-items:\s*flex-start;/);
  assert.match(css, /\.form-row-inline:has\(>\s*\.token-limit-control\)\s*>\s*\.form-row-inline__label\s*\{[\s\S]*?min-height:\s*36px;/);
  assert.match(css, /\.token-limit-input-line\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*14px\s+minmax\(0,\s*1fr\)\s+max-content;/);
  assert.match(css, /\.token-limit-hint--inline\s*\{[\s\S]*?flex:\s*0\s+0\s+auto;/);
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

test('tokens.js 并发上限输入只接受非负整数且创建更新共用同一解析逻辑', () => {
  const sandbox = {
    t(key) {
      return key;
    }
  };
  vm.runInNewContext(extractFunction(script, 'parseMaxConcurrencyInput'), sandbox);

  assert.deepEqual(sandbox.parseMaxConcurrencyInput(''), { value: 0 });
  assert.deepEqual(sandbox.parseMaxConcurrencyInput('0'), { value: 0 });
  assert.deepEqual(sandbox.parseMaxConcurrencyInput(' 1e2 '), { value: 100 });
  assert.deepEqual(sandbox.parseMaxConcurrencyInput('3'), { value: 3 });
  assert.deepEqual(sandbox.parseMaxConcurrencyInput('1.9'), { error: 'tokens.msg.maxConcurrencyInteger' });
  assert.deepEqual(sandbox.parseMaxConcurrencyInput('-0.5'), { error: 'tokens.msg.maxConcurrencyInteger' });
  assert.deepEqual(sandbox.parseMaxConcurrencyInput('-1'), { error: 'tokens.msg.maxConcurrencyInteger' });
  assert.match(script, /const maxConcurrencyResult = parseMaxConcurrencyInput\(document\.getElementById\('tokenMaxConcurrency'\)\.value\);/);
  assert.match(script, /const maxConcurrencyResult = parseMaxConcurrencyInput\(document\.getElementById\('editMaxConcurrency'\)\.value\);/);
  assert.doesNotMatch(script, /parseInt\(document\.getElementById\('tokenMaxConcurrency'\)\.value,\s*10\)\s*\|\|\s*0/);
  assert.doesNotMatch(script, /parseInt\(document\.getElementById\('editMaxConcurrency'\)\.value,\s*10\)\s*\|\|\s*0/);
});
