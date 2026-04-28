const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const channelsCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'channels.css'), 'utf8');
const channelsHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'channels.html'), 'utf8');
const channelsUrlsScript = fs.readFileSync(path.join(__dirname, 'channels-urls.js'), 'utf8');
const channelsKeysScript = fs.readFileSync(path.join(__dirname, 'channels-keys.js'), 'utf8');

test('channels 页顶部筛选控件不再写死桌面宽度', () => {
  assert.doesNotMatch(channelsHtml, /id="channelTypeFilter"[^>]*style="[^"]*min-width:\s*120px/);
  assert.doesNotMatch(channelsHtml, /id="idFilter"[^>]*style="[^"]*max-width:\s*100px/);
  assert.doesNotMatch(channelsHtml, /id="statusFilter"[^>]*style="[^"]*min-width:\s*100px/);
  assert.doesNotMatch(channelsHtml, /id="modelFilter"[\s\S]*?filter-combobox-wrapper" style="[^"]*min-width:\s*100px/);
  assert.match(channelsHtml, /class="channel-page-hero"[\s\S]*class="channel-page-actions"/);
  assert.match(channelsHtml, /id="exportCsvBtn"[^>]*class="btn btn-secondary channel-page-action-btn"/);
  assert.match(channelsHtml, /id="importCsvBtn"[^>]*class="btn btn-secondary channel-page-action-btn"/);
  assert.match(channelsHtml, /data-action="show-add-modal"[^>]*class="btn btn-primary channel-page-action-btn"/);
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.channel-page-actions\s*\{[\s\S]*?grid-template-columns:\s*repeat\(3,\s*minmax\(0,\s*1fr\)\);[\s\S]*?\.channel-page-action-btn\s*\{[\s\S]*?width:\s*100%;[\s\S]*?white-space:\s*nowrap;/);
});

test('channels 页将数量、排序和筛选归到同一个移动端摘要行', () => {
  assert.match(channelsHtml, /class="channel-filter-summary"[\s\S]*id="filterInfo"[\s\S]*id="btn_sort"[\s\S]*id="btn_filter"/);
});

test('channels 页手机端批量选择浮层拆成头部和两排操作区，关闭按钮固定右上角', () => {
  assert.match(channelsHtml, /id="batchFloatingMenu"[\s\S]*class="channel-batch-float__content"[\s\S]*class="channel-batch-float__header"[\s\S]*class="channel-batch-selection"[\s\S]*class="channel-batch-actions"[\s\S]*id="batchFloatingMenuCloseBtn"/);

  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.channel-batch-float__content\s*\{[^}]*position:\s*relative;[^}]*flex-direction:\s*column;[^}]*align-items:\s*stretch;/);
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.channel-batch-float\s*\{[^}]*width:\s*min\(320px,\s*calc\(100vw - 16px\)\);/);
  assert.match(channelsCss, /\.channel-batch-actions\s*\{[\s\S]*?display:\s*inline-flex;[\s\S]*?gap:\s*10px;/);
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.channel-batch-float__header\s*\{[\s\S]*?width:\s*100%;[\s\S]*?padding-right:\s*34px;/);
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.channel-batch-actions\s*\{[\s\S]*?width:\s*100%;[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*repeat\(2,\s*minmax\(0,\s*1fr\)\);/);
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.channel-batch-action\s*\{[^}]*white-space:\s*nowrap;/);
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.channel-batch-close\s*\{[\s\S]*?position:\s*absolute;[\s\S]*?top:\s*10px;[\s\S]*?right:\s*10px;/);
});

test('channels 页为手机卡片式表格预留移动端标签与样式', () => {
  const templateMatch = channelsHtml.match(/<template id="tpl-channel-card">[\s\S]*?<\/template>/);
  assert.ok(templateMatch, '缺少渠道行模板');
  const template = templateMatch[0];

  assert.match(template, /class="ch-col-models"[^>]*data-mobile-label="\{\{mobileLabelModels\}\}"/);
  assert.match(template, /class="ch-col-priority"[^>]*data-mobile-label="\{\{mobileLabelPriority\}\}"/);
  assert.match(template, /class="ch-col-duration[^"]*"[^>]*data-mobile-label="\{\{mobileLabelDuration\}\}"/);
  assert.match(template, /class="ch-col-usage[^"]*"[^>]*data-mobile-label="\{\{mobileLabelUsage\}\}"/);
  assert.match(template, /class="ch-col-cost[^"]*"[^>]*data-mobile-label="\{\{mobileLabelCost\}\}"/);
  assert.match(template, /class="ch-col-enabled"[^>]*data-mobile-label="\{\{mobileLabelEnabled\}\}"/);

  const mobileCssMatch = channelsCss.match(/@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.channel-table-container\s*\{[\s\S]*?overflow-x:\s*visible;[\s\S]*?\.channel-table\s+thead\s+th:not\(\.ch-col-checkbox\)\s*\{[\s\S]*?display:\s*none;[\s\S]*?\.channel-table\s+tbody\s+tr\s*\{[\s\S]*?display:\s*grid;[\s\S]*?\.channel-table\s+td\[data-mobile-label\]::before\s*\{/);
  assert.ok(mobileCssMatch, '缺少渠道表格手机卡片布局样式');

  assert.match(channelsCss, /\.channel-table\s+\.ch-col-priority\s*\{[^}]*order:\s*10;/);
  assert.match(channelsCss, /\.channel-table\s+\.ch-col-cost\s*\{[^}]*order:\s*11;/);
  assert.match(channelsCss, /\.channel-table\s+\.ch-col-enabled\s*\{[^}]*order:\s*22;/);
  assert.match(channelsCss, /\.channel-table\s+\.ch-col-priority,\s*[\r\n\s]*\.channel-table\s+\.ch-col-cost,\s*[\r\n\s]*\.channel-table\s+\.ch-col-enabled\s*\{[\s\S]*?display:\s*flex;[\s\S]*?justify-content:\s*space-between;/);
  assert.match(channelsCss, /\.channel-table\s+\.ch-col-actions\s*\{[\s\S]*?order:\s*30;[\s\S]*?align-items:\s*center;/);
  assert.match(channelsCss, /\.channel-table\s+td\.ch-col-actions::before\s*\{[\s\S]*?content:\s*none;/);
  assert.match(channelsCss, /\.channel-table\s+\.ch-action-group\s*\{[\s\S]*?justify-content:\s*center;/);
  assert.match(channelsCss, /\.channel-table\s+\.ch-action-group\s*\{[\s\S]*?flex-wrap:\s*nowrap;/);
  assert.match(channelsCss, /\.channel-table\s+\.ch-action-group\s*\{[\s\S]*?overflow-x:\s*auto;/);
  assert.match(channelsCss, /\.channel-table\s+\.channel-refresh-result\s*\{[\s\S]*?width:\s*100%;/);
  assert.match(channelsCss, /\.channel-table\s+\.channel-refresh-result__summary\s*\{[\s\S]*?white-space:\s*normal;/);
});

test('channels 页手机卡片对空统计块做折叠', () => {
  const templateMatch = channelsHtml.match(/<template id="tpl-channel-card">[\s\S]*?<\/template>/);
  assert.ok(templateMatch, '缺少渠道行模板');
  const template = templateMatch[0];

  assert.match(template, /class="ch-col-duration \{\{durationCellClass\}\}"/);
  assert.match(template, /class="ch-col-usage \{\{usageCellClass\}\}"/);
  assert.match(template, /class="ch-col-cost \{\{costCellClass\}\}"/);

  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.channel-table\s+td\.ch-mobile-empty\s*\{[\s\S]*?display:\s*none;/);
});

test('channels 页耗时和消耗列内部指标保持紧凑', () => {
  assert.match(channelsCss, /\.ch-timing\s*\{[\s\S]*?width:\s*max-content;[\s\S]*?max-width:\s*100%;[\s\S]*?margin-left:\s*auto;/);
  assert.match(channelsCss, /\.ch-usage-list\s*\{[\s\S]*?width:\s*max-content;[\s\S]*?max-width:\s*100%;[\s\S]*?margin-left:\s*auto;/);
  assert.match(channelsCss, /\.ch-timing-value,\s*[\r\n\s]*\.ch-usage-value\s*\{[\s\S]*?margin-left:\s*0;/);
});

test('channels 弹窗内联表为手机布局补齐类名、标签和关键重排', () => {
  assert.match(channelsHtml, /<table class="inline-table mobile-inline-table inline-url-table">/);
  assert.match(channelsHtml, /<table class="inline-table mobile-inline-table inline-key-table">/);
  assert.match(channelsHtml, /<table class="inline-table mobile-inline-table redirect-model-table">/);

  assert.match(channelsHtml, /<template id="tpl-url-row">[\s\S]*?class="mobile-inline-row inline-url-row"/);
  assert.match(channelsHtml, /class="inline-url-col-url"[^>]*data-mobile-label="\{\{mobileLabelUrl\}\}"/);
  assert.match(channelsHtml, /class="inline-url-col-actions[^"]*"[^>]*data-mobile-label="\{\{mobileLabelActions\}\}"/);

  assert.match(channelsHtml, /<template id="tpl-key-row">[\s\S]*?class="mobile-inline-row inline-key-row draggable-key-row"/);
  assert.match(channelsHtml, /class="inline-key-col-key"[^>]*data-mobile-label="\{\{mobileLabelKey\}\}"/);
  assert.match(channelsHtml, /class="inline-key-col-status"[^>]*data-mobile-label="\{\{mobileLabelStatus\}\}"/);

  assert.match(channelsHtml, /<template id="tpl-redirect-row">[\s\S]*?class="mobile-inline-row redirect-row"/);
  assert.match(channelsHtml, /class="redirect-col-model"[^>]*data-mobile-label="\{\{mobileLabelModel\}\}"/);
  assert.match(channelsHtml, /class="redirect-col-target"[^>]*data-mobile-label="\{\{mobileLabelTarget\}\}"/);

  assert.match(channelsUrlsScript, /setAttribute\('data-mobile-label', window\.t\('common\.status'\)\)/);
  assert.match(channelsUrlsScript, /setAttribute\('data-mobile-label', window\.t\('stats\.latency'\)\)/);
  assert.match(channelsUrlsScript, /setAttribute\('data-mobile-label', window\.t\('channels\.urlRequests'\)\)/);
  assert.match(channelsKeysScript, /matchMedia\('\(max-width:\s*768px\)'\)\.matches/);

  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s*\{[\s\S]*?grid-template-columns:\s*36px\s+minmax\(0,\s*1fr\)\s+auto\s+auto;[\s\S]*?align-items:\s*center;/);
  assert.match(channelsCss, /\.inline-key-table\s+tbody\s+\.mobile-inline-row\s*\{[\s\S]*?grid-template-columns:\s*36px\s+minmax\(0,\s*1fr\)\s+auto\s+auto;[\s\S]*?align-items:\s*center;/);
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-url\s*\{[\s\S]*?order:\s*2;[\s\S]*?grid-column:\s*2\s*\/\s*4;/);
  assert.match(channelsCss, /\.inline-key-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-key-col-key\s*\{[\s\S]*?order:\s*2;[\s\S]*?grid-column:\s*2\s*\/\s*4;/);
  assert.match(channelsCss, /\.redirect-model-table\s+tbody\s+\.mobile-inline-row\s*\{[\s\S]*?grid-template-columns:\s*36px\s+minmax\(0,\s*1fr\)\s+auto;[\s\S]*?align-items:\s*center;/);
  assert.match(channelsCss, /\.redirect-model-table\s+\.mobile-inline-row\s+\.redirect-col-select\s*\{[\s\S]*?grid-column:\s*1;[\s\S]*?grid-row:\s*1;/);
  assert.match(channelsCss, /\.redirect-model-table\s+\.mobile-inline-row\s+\.redirect-col-model\s*\{[\s\S]*?grid-column:\s*2\s*\/\s*4;[\s\S]*?grid-row:\s*1;/);
  assert.match(channelsCss, /\.redirect-model-table\s+\.mobile-inline-row\s+\.redirect-col-target\s*\{[\s\S]*?grid-column:\s*2\s*\/\s*3;[\s\S]*?grid-row:\s*2;/);
  assert.match(channelsCss, /\.redirect-model-table\s+\.mobile-inline-row\s+\.redirect-col-actions\s*\{[\s\S]*?grid-column:\s*3;[\s\S]*?grid-row:\s*2;[\s\S]*?justify-content:\s*flex-end;[\s\S]*?border-top:\s*none;/);
  assert.match(channelsCss, /\.redirect-model-table\s+\.mobile-inline-row\s+td\.redirect-col-model\[data-mobile-label\]::before,\s*[\r\n\s]*\.redirect-model-table\s+\.mobile-inline-row\s+td\.redirect-col-target\[data-mobile-label\]::before,\s*[\r\n\s]*\.redirect-model-table\s+\.mobile-inline-row\s+td\.redirect-col-actions\[data-mobile-label\]::before\s*\{[\s\S]*?content:\s*none;/);
});

test('channels 编辑弹窗为手机布局补齐结构化骨架和分组重排样式', () => {
  assert.match(channelsHtml, /<div class="modal-content channel-editor-modal">/);
  assert.match(channelsHtml, /<form id="channelForm" class="channel-editor-form">/);
  assert.match(channelsHtml, /class="form-group channel-editor-group channel-editor-group--primary"/);
  assert.match(channelsHtml, /class="channel-editor-primary-row"/);
  assert.match(channelsHtml, /class="channel-editor-primary-field channel-editor-primary-field--name"/);
  assert.match(channelsHtml, /class="channel-editor-primary-field channel-editor-primary-field--type"/);
  assert.match(channelsHtml, /class="channel-editor-primary-field channel-editor-primary-field--mode"/);
  assert.match(channelsHtml, /class="channel-editor-section-header"/);
  assert.match(channelsHtml, /class="[^"]*channel-editor-section-title[^"]*"/);
  assert.match(channelsHtml, /class="[^"]*channel-editor-section-meta[^"]*"/);
  assert.match(channelsHtml, /class="[^"]*channel-editor-section-actions[^"]*"/);
  assert.match(channelsHtml, /class="channel-editor-footer"/);
  assert.match(channelsHtml, /class="channel-editor-footer-actions"/);

  assert.match(channelsCss, /\.channel-editor-modal\s*\{[\s\S]*?max-width:\s*90vw;/);
  assert.match(channelsCss, /\.channel-editor-group--primary\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*minmax\(0,\s*1fr\)\s+minmax\(320px,\s*max-content\);/);
  assert.match(channelsCss, /\.channel-editor-group--primary\s+\.channel-editor-primary-row\s*\{[\s\S]*?display:\s*contents;/);
  assert.match(channelsCss, /\.channel-editor-section-header\s*\{[\s\S]*?display:\s*flex;[\s\S]*?justify-content:\s*space-between;/);
  assert.match(channelsCss, /\.channel-editor-footer-actions\s*\{[\s\S]*?display:\s*flex;[\s\S]*?justify-content:\s*flex-end;/);
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.channel-editor-section-stack\s*\{[\s\S]*?flex:\s*(?:none|0\s+0\s+auto);/);
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.channel-editor-section-actions\s*\{[\s\S]*?flex:\s*(?:none|0\s+0\s+auto);/);
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.channel-editor-modal\s*\{[\s\S]*?width:\s*min\(100%,\s*calc\(100vw - 16px\)\);[\s\S]*?margin:\s*8px;[\s\S]*?padding:\s*16px;[\s\S]*?min-height:\s*calc\(100vh - 16px\);[\s\S]*?\.channel-editor-group--primary\s*\{[\s\S]*?display:\s*block;[\s\S]*?\.channel-editor-group--primary\s+\.channel-editor-primary-row\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*1fr;[\s\S]*?\.channel-editor-section-header\s*\{[\s\S]*?flex-direction:\s*column;[\s\S]*?align-items:\s*stretch;[\s\S]*?\.channel-editor-section-actions\s*\{[\s\S]*?width:\s*100%;[\s\S]*?justify-content:\s*flex-start;/);
});

test('channels 编辑弹窗在手机端将基础字段、按钮条和卡片内容压成单行信息流', () => {
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.channel-editor-primary-field\s*\{[\s\S]*?flex-direction:\s*row;[\s\S]*?align-items:\s*center;/);
  assert.match(channelsCss, /\.channel-editor-form\s*\{[\s\S]*?flex:\s*1\s+1\s+auto;/);
  assert.match(channelsCss, /\.channel-editor-primary-field--type\s+\.channel-editor-radio-group,\s*[\r\n\s]*\.channel-editor-primary-field--mode\s+\.channel-editor-radio-group,\s*[\r\n\s]*\.channel-editor-radio-group--strategy\s*\{[\s\S]*?flex-direction:\s*row;[\s\S]*?flex-wrap:\s*nowrap;[\s\S]*?overflow-x:\s*auto;/);
  assert.match(channelsCss, /\.channel-editor-strategy-row\s*\{[\s\S]*?flex-direction:\s*row;[\s\S]*?align-items:\s*center;[\s\S]*?flex-wrap:\s*nowrap;/);
  assert.match(channelsCss, /\.channel-editor-section-actions\s*\{[\s\S]*?flex-wrap:\s*nowrap;[\s\S]*?overflow-x:\s*auto;/);
  assert.match(channelsCss, /\.channel-editor-section-actions\s+\.channel-editor-action-row\s*\{[\s\S]*?flex-wrap:\s*nowrap;[\s\S]*?overflow-x:\s*auto;/);
  assert.match(channelsCss, /\.channel-editor-section-actions\s+\.btn,\s*[\r\n\s]*\.channel-editor-section-actions\s+\.channel-hover-key-toggle-btn,\s*[\r\n\s]*\.channel-editor-section-actions\s+\.channel-editor-action-row\s+\.btn\s*\{[\s\S]*?flex:\s*0\s+0\s+auto;/);
  assert.match(channelsCss, /\.channel-editor-footer\s*\{[\s\S]*?margin-top:\s*auto;/);
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?#channelModal\s+\.channel-editor-footer\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*minmax\(0,\s*1fr\)\s+auto;/);
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?#channelModal\s+\.channel-editor-checkbox-label\s*\{[\s\S]*?grid-column:\s*1;[\s\S]*?grid-row:\s*1;[\s\S]*?width:\s*auto;/);
  assert.match(channelsCss, /\.channel-editor-footer-fields\s*\{[\s\S]*?grid-column:\s*1\s*\/\s*-1;[\s\S]*?grid-row:\s*2;[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*repeat\(2,\s*minmax\(0,\s*1fr\)\);[\s\S]*?gap:\s*8px\s+12px;/);
  assert.match(channelsCss, /\.channel-editor-inline-field\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*auto\s+minmax\(0,\s*1fr\);[\s\S]*?align-items:\s*center;/);
  assert.match(channelsCss, /\.channel-editor-inline-field\s*>\s*\.form-input,\s*[\r\n\s]*\.channel-editor-inline-field-input\s*\{[\s\S]*?margin-left:\s*0;[\s\S]*?width:\s*auto;/);
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?#channelModal\.modal,\s*[\r\n\s]*#channelModal\s+\.channel-editor-modal\s*\{[\s\S]*?backdrop-filter:\s*none;/);
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?#channelForm\.channel-editor-form\s*\{[\s\S]*?min-height:\s*0;[\s\S]*?padding-bottom:\s*220px;/);
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?#channelModal\s+\.channel-editor-footer\s*\{[\s\S]*?position:\s*fixed;[\s\S]*?left:\s*24px;[\s\S]*?right:\s*24px;[\s\S]*?bottom:\s*24px;[\s\S]*?z-index:\s*120;[\s\S]*?padding:\s*10px\s+12px\s+0;[\s\S]*?border-radius:\s*18px;/);
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?#channelModal\s+\.channel-editor-footer-actions\s*\{[\s\S]*?grid-column:\s*2;[\s\S]*?grid-row:\s*1;[\s\S]*?width:\s*auto;/);
  assert.match(channelsCss, /\.channel-editor-footer-actions\s+\.btn\s*\{[\s\S]*?flex:\s*0\s+0\s+auto;[\s\S]*?min-width:\s*84px;/);
  assert.match(channelsCss, /\.inline-url-table\s+\.mobile-inline-row\s+td\[data-mobile-label\]::before,\s*[\r\n\s]*\.inline-key-table\s+\.mobile-inline-row\s+td\[data-mobile-label\]::before,\s*[\r\n\s]*\.redirect-model-table\s+\.mobile-inline-row\s+td\[data-mobile-label\]::before\s*\{[\s\S]*?display:\s*inline-flex;[\s\S]*?margin:\s*0\s+8px\s+0\s+0;/);
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s*\{[\s\S]*?grid-template-columns:\s*36px\s+minmax\(0,\s*1fr\)\s+auto\s+auto;[\s\S]*?align-items:\s*center;/);
  assert.match(channelsCss, /\.inline-key-table\s+tbody\s+\.mobile-inline-row\s*\{[\s\S]*?grid-template-columns:\s*36px\s+minmax\(0,\s*1fr\)\s+auto\s+auto;[\s\S]*?align-items:\s*center;/);
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-select,\s*[\r\n\s]*\.inline-key-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-key-col-select\s*\{[\s\S]*?grid-column:\s*1;[\s\S]*?grid-row:\s*1;/);
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-url\s*\{[\s\S]*?grid-column:\s*2\s*\/\s*4;[\s\S]*?grid-row:\s*1;/);
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-status\s*\{[\s\S]*?grid-column:\s*2;[\s\S]*?grid-row:\s*2;/);
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-latency\s*\{[\s\S]*?grid-column:\s*3;[\s\S]*?grid-row:\s*2;[\s\S]*?justify-content:\s*flex-end;/);
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-requests\s*\{[\s\S]*?grid-column:\s*4;[\s\S]*?grid-row:\s*2;[\s\S]*?justify-content:\s*flex-end;/);
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-actions\s*\{[\s\S]*?order:\s*3;[\s\S]*?justify-content:\s*flex-end;[\s\S]*?border-top:\s*none;/);
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-actions\s*\{[\s\S]*?grid-column:\s*4;[\s\S]*?grid-row:\s*1;/);
  assert.match(channelsCss, /\.inline-key-table\s+tbody\s+\.mobile-inline-row\s*\{[\s\S]*?padding:\s*10px\s+12px;[\s\S]*?gap:\s*6px\s+8px;/);
  assert.match(channelsCss, /\.inline-key-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-key-col-key\s*\{[\s\S]*?grid-column:\s*2\s*\/\s*3;[\s\S]*?grid-row:\s*1;/);
  assert.match(channelsCss, /\.inline-key-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-key-col-actions\s*\{[\s\S]*?order:\s*3;[\s\S]*?justify-content:\s*flex-end;[\s\S]*?border-top:\s*none;/);
  assert.match(channelsCss, /\.inline-key-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-key-col-actions\s*\{[\s\S]*?grid-column:\s*3;[\s\S]*?grid-row:\s*1;/);
  assert.match(channelsCss, /\.inline-key-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-key-col-status\s*\{[\s\S]*?grid-column:\s*4;[\s\S]*?grid-row:\s*1;[\s\S]*?justify-content:\s*flex-end;[\s\S]*?white-space:\s*nowrap;/);
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-url::before,\s*[\r\n\s]*\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-status::before,\s*[\r\n\s]*\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-latency::before,\s*[\r\n\s]*\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-requests::before,\s*[\r\n\s]*\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-actions::before,\s*[\r\n\s]*\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-toggle::before\s*\{[\s\S]*?content:\s*none;/);
  assert.match(channelsCss, /\.inline-key-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-key-col-key::before,\s*[\r\n\s]*\.inline-key-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-key-col-status::before,\s*[\r\n\s]*\.inline-key-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-key-col-actions::before\s*\{[\s\S]*?content:\s*none;/);
});

test('channels 编辑弹窗手机端 URL 状态退化为纯指示点，延迟和调用指标不再互相挤压', () => {
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-status\s*\{[\s\S]*?grid-column:\s*2;[\s\S]*?grid-row:\s*2;/);
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-latency,\s*[\r\n\s]*\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-requests\s*\{[\s\S]*?min-width:\s*0;/);
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-status\s+\.inline-url-status-badge\s*\{[\s\S]*?padding:\s*0;[\s\S]*?background:\s*transparent;[\s\S]*?font-size:\s*0;/);
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-latency::before,/);
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-requests::before,/);
});
