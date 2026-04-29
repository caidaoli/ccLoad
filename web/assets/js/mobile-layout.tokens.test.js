const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const tokensCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'tokens.css'), 'utf8');
const tokensHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'tokens.html'), 'utf8');
const tokensScript = fs.readFileSync(path.join(__dirname, 'tokens.js'), 'utf8');

test('tokens 页为手机卡片布局补齐模板标签和按钮布局', () => {
  assert.match(tokensScript, /table\.className\s*=\s*'mobile-card-table tokens-table'/);
  assert.doesNotMatch(tokensCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?min-width:\s*980px;/);
  assert.match(tokensHtml, /<template id="tpl-token-row">[\s\S]*?class="mobile-card-row token-card-row"/);
  assert.match(tokensHtml, /class="[^"]*tokens-col-description[^"]*"[^>]*data-mobile-label="\{\{mobileLabelDescription\}\}"/);
  assert.match(tokensHtml, /class="[^"]*tokens-col-concurrency[^"]*"[^>]*data-mobile-label="\{\{mobileLabelConcurrency\}\}"/);
  assert.match(tokensHtml, /class="[^"]*tokens-col-actions[^"]*"[^>]*data-mobile-label="\{\{mobileLabelActions\}\}"/);
  assert.match(tokensScript, /mobileLabelDescription:\s*t\('tokens\.table\.description'\)/);
  assert.match(tokensScript, /mobileLabelConcurrency:\s*t\('tokens\.table\.concurrency'\)/);
  assert.match(tokensScript, /mobileLabelActions:\s*t\('tokens\.table\.actions'\)/);
  assert.match(tokensCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.tokens-table\s+\.tokens-col-description,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-token,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-token-usage,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-last-used,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-actions\s*\{[\s\S]*?grid-column:\s*1\s*\/\s*-1;/);
  assert.match(tokensCss, /\.token-row-actions\s*\{[\s\S]*?justify-content:\s*center;[\s\S]*?flex-wrap:\s*nowrap;/);
});

test('tokens 页手机卡片将统计标签和值压缩为左右同行', () => {
  assert.match(tokensCss, /\.tokens-table\s+\.tokens-col-calls,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-success-rate,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-rpm,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-token-usage,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-cost,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-concurrency,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-stream,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-non-stream,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-last-used\s*\{[\s\S]*?display:\s*flex\s*!important;[\s\S]*?align-items:\s*center;[\s\S]*?justify-content:\s*space-between;/);
  assert.match(tokensCss, /\.tokens-table\s+\.tokens-col-calls::before,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-success-rate::before,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-rpm::before,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-token-usage::before,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-cost::before,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-concurrency::before,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-stream::before,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-non-stream::before,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-last-used::before\s*\{[\s\S]*?width:\s*auto\s*!important;[\s\S]*?margin-bottom:\s*0\s*!important;/);
});

test('tokens 页手机卡片将描述令牌和调用次数压缩为单行主信息', () => {
  assert.match(tokensCss, /\.tokens-table\s+\.tokens-col-description,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-token\s*\{[\s\S]*?display:\s*flex\s*!important;[\s\S]*?align-items:\s*center;/);
  assert.match(tokensCss, /\.tokens-table\s+\.tokens-col-description::before,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-token::before\s*\{[\s\S]*?width:\s*auto\s*!important;[\s\S]*?margin-bottom:\s*0\s*!important;/);
  assert.match(tokensScript, /function\s+buildCallsHtml[\s\S]*?class="token-call-stats"/);
  assert.match(tokensScript, /token-call-badge token-call-badge--success/);
  assert.match(tokensScript, /token-call-badge token-call-badge--failure/);
  assert.match(tokensScript, /token-call-icon token-call-icon--success/);
  assert.match(tokensScript, /token-call-icon token-call-icon--failure/);
  assert.doesNotMatch(tokensScript, /let html = '<div style="display: inline-flex; align-items: center; justify-content: flex-end; gap: 4px; flex-wrap: wrap;">'/);
});

test('tokens 页手机卡片将 token 用量压成紧凑二维指标块', () => {
  assert.match(tokensScript, /function\s+buildTokensHtml[\s\S]*?class=\"token-usage-metrics\"/);
  assert.match(tokensScript, /pushUsageItem\('input'/);
  assert.match(tokensScript, /pushUsageItem\('output'/);
  assert.match(tokensScript, /pushUsageItem\('cache-read'/);
  assert.match(tokensScript, /pushUsageItem\('cache-create'/);
  assert.match(tokensScript, /class=\"token-usage-label\"/);
  assert.match(tokensScript, /class=\"token-usage-value\"/);
  assert.match(tokensCss, /\.token-usage-metrics\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*repeat\(2,\s*minmax\(0,\s*max-content\)\);/);
  assert.match(tokensCss, /\.token-usage-item\s*\{[\s\S]*?display:\s*inline-flex;[\s\S]*?align-items:\s*center;[\s\S]*?justify-content:\s*space-between;/);
  assert.match(tokensCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.tokens-table\s+\.tokens-col-token-usage\s+>\s+\.token-usage-metrics\s*\{[\s\S]*?justify-content:\s*flex-end;[\s\S]*?grid-template-columns:\s*repeat\(2,\s*minmax\(0,\s*max-content\)\);/);
});

test('tokens 页总费用使用调用统计同款 warning 两行成本组件', () => {
  assert.match(tokensScript, /function\s+buildCostHtml[\s\S]*?buildCostStackHtml\(totalCostUsd,\s*effectiveCostUsd,\s*\{\s*tone:\s*'warning'\s*\}\)/);
  assert.match(tokensCss, /\.token-cost\s*\{[\s\S]*?display:\s*flex;[\s\S]*?flex-direction:\s*column;/);
  assert.match(tokensCss, /\.token-cost\s+\.cost-stack\s*\{[\s\S]*?align-items:\s*center;[\s\S]*?text-align:\s*center;/);
  assert.doesNotMatch(tokensCss, /\.token-cost-value\s*\{[^}]*color:\s*var\(--success-700\);/);
});

test('tokens 页调用次数和 token 用量指标退化为纯文字样式', () => {
  assert.doesNotMatch(tokensCss, /\.token-call-badge\s*\{[^}]*\bborder\s*:/);
  assert.doesNotMatch(tokensCss, /\.token-call-badge--success\s*\{[^}]*\bborder-color\s*:/);
  assert.doesNotMatch(tokensCss, /\.token-call-badge--failure\s*\{[^}]*\bborder-color\s*:/);
  assert.match(tokensCss, /\.token-call-badge\s*\{[^}]*\bpadding\s*:\s*0;/);
  assert.match(tokensCss, /\.token-call-badge\s*\{[^}]*\bborder-radius\s*:\s*0;/);
  assert.match(tokensCss, /\.token-call-badge\s*\{[^}]*\bbackground\s*:\s*transparent;/);
  assert.doesNotMatch(tokensCss, /\.token-call-badge--success\s*\{[^}]*\bbackground\s*:/);
  assert.doesNotMatch(tokensCss, /\.token-call-badge--failure\s*\{[^}]*\bbackground\s*:/);
  assert.doesNotMatch(tokensCss, /\.token-usage-item\s*\{[^}]*\bborder\s*:/);
  assert.match(tokensCss, /\.token-usage-item\s*\{[^}]*\bpadding\s*:\s*0;/);
  assert.match(tokensCss, /\.token-usage-item\s*\{[^}]*\bborder-radius\s*:\s*0;/);
  assert.match(tokensCss, /\.token-usage-item\s*\{[^}]*\bbackground\s*:\s*transparent;/);
  assert.doesNotMatch(tokensCss, /\.token-usage-item--input\s*\{[^}]*\bborder-color\s*:/);
  assert.doesNotMatch(tokensCss, /\.token-usage-item--output\s*\{[^}]*\bborder-color\s*:/);
  assert.doesNotMatch(tokensCss, /\.token-usage-item--cache-read\s*\{[^}]*\bborder-color\s*:/);
  assert.doesNotMatch(tokensCss, /\.token-usage-item--cache-create\s*\{[^}]*\bborder-color\s*:/);
  assert.doesNotMatch(tokensCss, /\.token-usage-item--input\s*\{[^}]*\bbackground\s*:/);
  assert.doesNotMatch(tokensCss, /\.token-usage-item--output\s*\{[^}]*\bbackground\s*:/);
  assert.doesNotMatch(tokensCss, /\.token-usage-item--cache-read\s*\{[^}]*\bbackground\s*:/);
  assert.doesNotMatch(tokensCss, /\.token-usage-item--cache-create\s*\{[^}]*\bbackground\s*:/);
});

test('tokens 弹窗模型限制表为手机布局补齐类名、标签和按钮重排', () => {
  assert.match(tokensHtml, /<table class="inline-table mobile-inline-table allowed-models-table">/);
  assert.match(tokensScript, /class="mobile-inline-row allowed-model-row"/);
  assert.match(tokensScript, /class="allowed-model-col-name" data-mobile-label="\$\{mobileLabelModelName\}"/);
  assert.match(tokensScript, /class="allowed-model-col-actions" data-mobile-label="\$\{mobileLabelActions\}"/);
  assert.match(tokensScript, /class="allowed-models-empty-row"/);
  assert.match(tokensScript, /class="allowed-models-empty-cell"/);
  assert.match(tokensScript, /class="allowed-model-remove-btn btn btn-secondary btn-sm"/);
  assert.match(tokensScript, /const mobileLabelModelName = t\('tokens\.modelName'\)/);
  assert.match(tokensScript, /const mobileLabelActions = t\('tokens\.table\.actions'\)/);
  assert.doesNotMatch(tokensHtml, /class="[^"]*token-edit-models-table[^"]*"[^>]*style=/);
  assert.doesNotMatch(tokensScript, /allowed-model-col-(?:select|name|actions)[^>]*style=/);
  assert.match(tokensCss, /\.allowed-models-table\s+tbody\s+\.mobile-inline-row\s*\{[\s\S]*?grid-template-columns:\s*auto\s+minmax\(0,\s*1fr\)\s+auto;[\s\S]*?align-items:\s*center;/);
  assert.match(tokensCss, /\.allowed-models-table\s+tbody\s+\.mobile-inline-row\s+td\.allowed-model-col-name\s*\{[\s\S]*?grid-column:\s*auto;[\s\S]*?white-space:\s*nowrap;[\s\S]*?overflow-x:\s*auto;/);
  assert.match(tokensCss, /\.allowed-models-table\s+tbody\s+\.mobile-inline-row\s+td\.allowed-model-col-actions\s*\{[\s\S]*?grid-column:\s*auto;[\s\S]*?justify-content:\s*flex-end;/);
  assert.match(tokensCss, /\.allowed-models-table\s+tbody\s+\.mobile-inline-row\s+td\.allowed-model-col-name::before,\s*[\r\n\s]*\.allowed-models-table\s+tbody\s+\.mobile-inline-row\s+td\.allowed-model-col-actions::before\s*\{[\s\S]*?content:\s*none;/);
});

test('tokens 编辑令牌弹窗按基础信息、配额信息、模型限制三段组织', () => {
  assert.match(tokensHtml, /class="modal-body token-edit-body token-edit-layout"/);
  assert.match(tokensHtml, /<section class="token-edit-section token-edit-section--basic" data-token-edit-section="basic">[\s\S]*?class="token-edit-section-title"[^>]*>基础信息<\/h3>[\s\S]*?token-edit-field--description[\s\S]*?token-edit-field--expiry/);
  assert.match(tokensHtml, /<section class="token-edit-section token-edit-section--quota" data-token-edit-section="quota">[\s\S]*?class="token-edit-section-title"[^>]*>配额信息<\/h3>[\s\S]*?token-edit-field--cost[\s\S]*?token-edit-active-row/);
  assert.match(tokensHtml, /<section class="token-edit-section token-edit-section--channels token-edit-channels-section" data-token-edit-section="channels">[\s\S]*?class="token-edit-section-title token-edit-channels-title"[\s\S]*?token-edit-channels-actions/);
  assert.match(tokensHtml, /<section class="token-edit-section token-edit-section--models token-edit-models-section" data-token-edit-section="models">[\s\S]*?class="token-edit-section-title token-edit-models-title"[\s\S]*?token-edit-models-actions/);

  assert.match(tokensCss, /\.token-edit-section\s*\{[\s\S]*?display:\s*flex;[\s\S]*?flex-direction:\s*column;/);
  assert.match(tokensCss, /\.token-edit-section-title\s*\{[\s\S]*?font-size:\s*13px;[\s\S]*?text-transform:\s*uppercase;/);
  assert.match(tokensCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.token-edit-body\s*\{[\s\S]*?gap:\s*12px;/);
  assert.match(tokensCss, /\.token-edit-field\s*\{[\s\S]*?display:\s*flex;[\s\S]*?align-items:\s*center;/);
  assert.match(tokensCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.token-edit-field\s*\{[\s\S]*?flex-direction:\s*row;[\s\S]*?align-items:\s*center;/);
  assert.match(tokensCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.token-edit-field\s+\.form-label\s*\{[\s\S]*?flex:\s*0\s+0\s+60px;[\s\S]*?min-width:\s*60px;[\s\S]*?white-space:\s*nowrap;/);
  assert.match(tokensCss, /\.token-edit-field\s+\.form-label,\s*[\r\n\s]*\.token-edit-active-row\s+label,\s*[\r\n\s]*\.token-edit-models-title\s*\{[\s\S]*?margin-bottom:\s*0;/);
  assert.match(tokensHtml, /class="form-row-inline__content token-limit-control token-edit-cost-control"[\s\S]*?class="token-limit-input-line token-edit-cost-row"[\s\S]*?id="editCostLimitUSD"[\s\S]*?class="token-limit-meta token-edit-cost-meta"[\s\S]*?token-limit-prefix-slot token-limit-prefix-slot--empty[\s\S]*?id="editCostUsedDisplay"/);
  assert.match(tokensCss, /\.token-limit-control\s*\{[\s\S]*?flex-direction:\s*column;[\s\S]*?gap:\s*5px;/);
  assert.match(tokensCss, /\.token-limit-input-line\s*\{[\s\S]*?display:\s*grid;[\s\S]*?width:\s*100%;/);
  assert.match(tokensCss, /\.token-limit-meta\s*\{[\s\S]*?justify-content:\s*space-between;[\s\S]*?flex-wrap:\s*wrap;/);
  assert.match(tokensCss, /\.token-edit-cost-meta\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*14px\s+minmax\(0,\s*1fr\)\s+max-content;/);
  assert.match(tokensCss, /\.token-edit-cost-used\s*\{[\s\S]*?grid-column:\s*2;/);
  assert.match(tokensCss, /\.token-edit-channels-actions,\s*[\r\n\s]*\.token-edit-models-actions\s*\{[\s\S]*?display:\s*flex;[\s\S]*?align-items:\s*center;[\s\S]*?flex-wrap:\s*nowrap;[\s\S]*?overflow-x:\s*auto;/);
  assert.match(tokensCss, /\.token-edit-channels-actions\s+\.btn,\s*[\r\n\s]*\.token-edit-models-actions\s+\.btn\s*\{[\s\S]*?flex:\s*0\s+0\s+auto;/);
});

test('tokens 编辑令牌弹窗显示当前 token 值', () => {
  assert.match(tokensHtml, /id="editTokenValue"[^>]*readonly/);
  assert.match(tokensHtml, /token-edit-field--token/);
  assert.match(tokensScript, /document\.getElementById\('editTokenValue'\)\.value = token\.token \|\| '';/);
  assert.match(tokensScript, /document\.getElementById\('editTokenValue'\)\.value = '';/);
});

test('tokens 模型选择和导入弹窗把热点内联样式迁到 tokens.css', () => {
  assert.doesNotMatch(tokensHtml, /id="selectAllContainer"[^>]*style=/);
  assert.doesNotMatch(tokensHtml, /id="modelImportModal"[\s\S]*?id="tokenModelImportPreview"[^>]*style=/);
  assert.doesNotMatch(tokensHtml, /id="tokenModelImportTextarea"[^>]*style=/);
  assert.doesNotMatch(tokensHtml, /<template id="tpl-token-row">[\s\S]*?style=/);
  assert.doesNotMatch(tokensScript, /class="model-option-item"[\s\S]*?style=/);
  assert.doesNotMatch(tokensScript, /class="model-option-checkbox"[\s\S]*?style=/);
  assert.match(tokensCss, /#selectAllContainer\s*\{[\s\S]*?padding:\s*8px 12px;[\s\S]*?background:\s*var\(--neutral-50\);/);
  assert.match(tokensCss, /\.model-option-item\s*\{[\s\S]*?display:\s*flex;[\s\S]*?align-items:\s*center;[\s\S]*?padding:\s*8px 12px;/);
  assert.match(tokensCss, /\.model-import-textarea\s*\{[\s\S]*?min-height:\s*160px;[\s\S]*?font-family:/);
  assert.match(tokensCss, /\.token-model-import-preview\s*\{[\s\S]*?background:\s*var\(--success-50\);/);
  assert.match(tokensCss, /\.token-row-meta\s*\{[\s\S]*?font-size:\s*12px;[\s\S]*?color:\s*var\(--neutral-500\);/);
  assert.match(tokensCss, /\.token-row-action-btn\s*\{[\s\S]*?width:\s*28px;[\s\S]*?height:\s*28px;[\s\S]*?padding:\s*0;/);
});

test('tokens 页移除关键固定高度与控件宽度硬编码', () => {
  assert.doesNotMatch(tokensHtml, /id="editModal"[\s\S]*?style="height:\s*680px/);
  assert.doesNotMatch(tokensHtml, /id="tokenDescription"[^>]*style=/);
});
