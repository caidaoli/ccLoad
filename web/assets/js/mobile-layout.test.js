const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const sharedCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'styles.css'), 'utf8');
const channelsCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'channels.css'), 'utf8');
const logsCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'logs.css'), 'utf8');
const tokensCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'tokens.css'), 'utf8');
const channelsHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'channels.html'), 'utf8');
const logsHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'logs.html'), 'utf8');
const statsHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'stats.html'), 'utf8');
const trendHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'trend.html'), 'utf8');
const tokensHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'tokens.html'), 'utf8');
const settingsHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'settings.html'), 'utf8');
const modelTestHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'model-test.html'), 'utf8');
const logsScript = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');
const statsScript = fs.readFileSync(path.join(__dirname, 'stats.js'), 'utf8');
const trendScript = fs.readFileSync(path.join(__dirname, 'trend.js'), 'utf8');
const tokensScript = fs.readFileSync(path.join(__dirname, 'tokens.js'), 'utf8');
const settingsScript = fs.readFileSync(path.join(__dirname, 'settings.js'), 'utf8');
const modelTestScript = fs.readFileSync(path.join(__dirname, 'model-test.js'), 'utf8');
const channelsUrlsScript = fs.readFileSync(path.join(__dirname, 'channels-urls.js'), 'utf8');
const channelsKeysScript = fs.readFileSync(path.join(__dirname, 'channels-keys.js'), 'utf8');
const channelsModalsScript = fs.readFileSync(path.join(__dirname, 'channels-modals.js'), 'utf8');

function getLastRuleBody(css, selector) {
  const escapedSelector = selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const pattern = new RegExp(`${escapedSelector}\\s*\\{([\\s\\S]*?)\\}`, 'g');
  let match = null;
  let lastBody = '';
  while ((match = pattern.exec(css)) !== null) {
    lastBody = match[1];
  }
  return lastBody;
}

test('共享样式在窄屏下压缩顶部导航、时间范围、筛选栏和弹窗', () => {
  assert.match(sharedCss, /--topbar-offset:\s*var\(--topbar-height\)/);
  assert.match(sharedCss, /\.top-layout\s+\.main-content\s*\{[^}]*padding-top:\s*var\(--topbar-offset\)/s);

  const mobileSection = sharedCss.match(/@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\n\}/);
  assert.ok(mobileSection, '缺少共享移动端适配规则');
  const mobileCss = mobileSection[0];

  assert.match(mobileCss, /--topbar-offset:\s*144px;/);
  assert.match(mobileCss, /\.topbar\s*\{[\s\S]*?flex-wrap:\s*wrap;/);
  assert.match(mobileCss, /\.topbar-right\s*\{[\s\S]*?order:\s*2;/);
  assert.match(mobileCss, /\.topnav\s*\{[\s\S]*?order:\s*3;[\s\S]*?flex:\s*0\s+0\s+100%;[\s\S]*?width:\s*100%;[\s\S]*?flex-wrap:\s*wrap;[\s\S]*?overflow-x:\s*visible;[\s\S]*?white-space:\s*normal;/);
  assert.match(mobileCss, /\.topnav-link\s*\{[\s\S]*?font-size:\s*13px;[\s\S]*?padding:\s*6px\s+8px;[\s\S]*?white-space:\s*nowrap;/);
  assert.match(mobileCss, /\.topnav-link\s+svg\s*\{[\s\S]*?width:\s*16px;[\s\S]*?height:\s*16px;/);
  assert.match(sharedCss, /@media\s*\(max-width:\s*480px\)\s*\{[\s\S]*?--topbar-offset:\s*144px;/);
  assert.match(mobileCss, /\.time-range-selector\s*\{[\s\S]*?overflow-x:\s*auto;/);
  assert.match(mobileCss, /\.filter-controls\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*repeat\(2,\s*minmax\(0,\s*1fr\)\);[\s\S]*?align-items:\s*start;/);
  assert.match(mobileCss, /\.filter-group\s*\{[\s\S]*?grid-template-columns:\s*88px\s+minmax\(0,\s*1fr\);[\s\S]*?width:\s*100%;[\s\S]*?min-width:\s*0;/);
  assert.match(mobileCss, /\.filter-controls\s*>\s*\.channel-filter-summary,\s*[\r\n\s]*\.filter-controls\s*>\s*\.logs-filter-summary-row,\s*[\r\n\s]*\.filter-controls\s*>\s*\.stats-filter-summary-row,\s*[\r\n\s]*\.filter-controls\s*>\s*div\[style\*="flex:\s*none"\]\s*\{[\s\S]*?grid-column:\s*1\s*\/\s*-1;/);
  assert.match(mobileCss, /\.modal-content\s*\{[\s\S]*?max-height:\s*calc\(100vh - 24px\);/);
});

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

test('channels 页为手机卡片式表格预留移动端标签与样式', () => {
  const templateMatch = channelsHtml.match(/<template id="tpl-channel-card">[\s\S]*?<\/template>/);
  assert.ok(templateMatch, '缺少渠道行模板');
  const template = templateMatch[0];

  assert.match(template, /class="ch-col-models"[^>]*data-mobile-label="\{\{mobileLabelModels\}\}"/);
  assert.match(template, /class="ch-col-priority"[^>]*data-mobile-label="\{\{mobileLabelPriority\}\}"/);
  assert.match(template, /class="ch-col-duration[^"]*"[^>]*data-mobile-label="\{\{mobileLabelDuration\}\}"/);
  assert.match(template, /class="ch-col-usage[^"]*"[^>]*data-mobile-label="\{\{mobileLabelUsage\}\}"/);
  assert.match(template, /class="ch-col-cost[^"]*"[^>]*data-mobile-label="\{\{mobileLabelCost\}\}"/);

  const mobileCssMatch = channelsCss.match(/@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.channel-table-container\s*\{[\s\S]*?overflow-x:\s*visible;[\s\S]*?\.channel-table\s+thead\s+th:not\(\.ch-col-checkbox\)\s*\{[\s\S]*?display:\s*none;[\s\S]*?\.channel-table\s+tbody\s+tr\s*\{[\s\S]*?display:\s*grid;[\s\S]*?\.channel-table\s+td\[data-mobile-label\]::before\s*\{/);
  assert.ok(mobileCssMatch, '缺少渠道表格手机卡片布局样式');

  assert.match(channelsCss, /\.channel-table\s+\.ch-col-priority\s*\{[^}]*order:\s*10;/);
  assert.match(channelsCss, /\.channel-table\s+\.ch-col-cost\s*\{[^}]*order:\s*11;/);
  assert.match(channelsCss, /\.channel-table\s+\.ch-col-priority,\s*[\r\n\s]*\.channel-table\s+\.ch-col-cost\s*\{[\s\S]*?display:\s*flex;[\s\S]*?justify-content:\s*space-between;/);
  assert.match(channelsCss, /\.channel-table\s+\.ch-col-actions\s*\{[\s\S]*?order:\s*30;[\s\S]*?align-items:\s*center;/);
  assert.match(channelsCss, /\.channel-table\s+td\.ch-col-actions::before\s*\{[\s\S]*?content:\s*none;/);
  assert.match(channelsCss, /\.channel-table\s+\.ch-action-group\s*\{[\s\S]*?justify-content:\s*center;/);
  assert.match(channelsCss, /\.channel-table\s+\.ch-action-group\s*\{[\s\S]*?flex-wrap:\s*nowrap;/);
  assert.match(channelsCss, /\.channel-table\s+\.ch-action-group\s*\{[\s\S]*?overflow-x:\s*auto;/);
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

test('共享样式提供可复用的手机卡片表格骨架', () => {
  assert.match(sharedCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.mobile-card-table\s+thead\s*\{[\s\S]*?display:\s*none;/);
  assert.match(sharedCss, /\.mobile-card-table\s+tbody\s*\{[\s\S]*?display:\s*grid;/);
  assert.match(sharedCss, /\.mobile-card-table\s+tbody\s+\.mobile-card-row\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*repeat\(2,\s*minmax\(0,\s*1fr\)\);/);
  assert.match(sharedCss, /\.mobile-card-table\s+td\[data-mobile-label\]::before\s*\{[\s\S]*?content:\s*attr\(data-mobile-label\);/);
});

test('共享样式为弹窗 inline-table 提供手机卡片骨架', () => {
  assert.match(sharedCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.mobile-inline-table-container\s*\{[\s\S]*?overflow:\s*visible;/);
  assert.match(sharedCss, /\.mobile-inline-table\s+thead\s*\{[\s\S]*?display:\s*none;/);
  assert.match(sharedCss, /\.mobile-inline-table\s+tbody\s+\.mobile-inline-row\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*repeat\(2,\s*minmax\(0,\s*1fr\)\);/);
  assert.match(sharedCss, /\.mobile-inline-table\s+tbody\s+\.mobile-inline-row\s+td\[data-mobile-label\]::before\s*\{[\s\S]*?content:\s*attr\(data-mobile-label\);/);
});

test('logs 页为手机卡片布局补齐类名、标签和重排样式', () => {
  assert.match(logsHtml, /class="modern-table logs-table mobile-card-table"/);
  assert.match(logsScript, /const logMobileLabels = \{/);
  assert.match(logsScript, /class="mobile-card-row logs-table-row"/);
  assert.match(logsScript, /class="logs-col-time"[^`]*data-mobile-label="\$\{logMobileLabels\.time\}"/);
  assert.match(logsScript, /class="logs-col-message[^"]*"[^`]*data-mobile-label="\$\{logMobileLabels\.message\}"/);
  assert.match(logsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.logs-table\s+\.logs-col-time\s*\{[^}]*order:\s*1;/);
  assert.match(logsCss, /\.logs-table\s+\.logs-col-status\s*\{[^}]*order:\s*2;/);
  assert.match(logsCss, /\.logs-table\s+\.logs-col-message\s*\{[\s\S]*?grid-column:\s*1\s*\/\s*-1;/);
  assert.doesNotMatch(logsCss, /\.logs-table\s+\.logs-col-time,\s*[\r\n\s]*\.logs-table\s+\.logs-col-channel,\s*[\r\n\s]*\.logs-table\s+\.logs-col-model,\s*[\r\n\s]*\.logs-table\s+\.logs-col-message\s*\{[\s\S]*?grid-column:\s*1\s*\/\s*-1;/);
});

test('logs 页手机端压缩筛选摘要行并将日志字段改为单行标签布局', () => {
  assert.match(logsCss, /\.logs-filter-summary-row\s*\{[\s\S]*?grid-template-columns:\s*minmax\(0,\s*1fr\)\s+auto;[\s\S]*?align-items:\s*center;/);
  assert.match(logsCss, /\.logs-filter-summary-row\s+\.logs-filter-info\s*\{[\s\S]*?width:\s*auto;[\s\S]*?text-align:\s*left;/);
  assert.match(logsCss, /\.logs-filter-summary-row\s+\.logs-filter-actions\s*\{[\s\S]*?width:\s*auto;/);
  assert.match(logsCss, /\.logs-filter-summary-row\s+\.logs-filter-actions\s+\.btn\s*\{[\s\S]*?width:\s*auto;/);
  assert.match(logsCss, /\.logs-table\s+\.logs-col-time,\s*[\r\n\s]*\.logs-table\s+\.logs-col-status,\s*[\r\n\s]*\.logs-table\s+\.logs-col-channel,\s*[\r\n\s]*\.logs-table\s+\.logs-col-model,\s*[\r\n\s]*\.logs-table\s+\.logs-col-api-key,\s*[\r\n\s]*\.logs-table\s+\.logs-col-ip,\s*[\r\n\s]*\.logs-table\s+\.logs-col-timing,\s*[\r\n\s]*\.logs-table\s+\.logs-col-input,\s*[\r\n\s]*\.logs-table\s+\.logs-col-output,\s*[\r\n\s]*\.logs-table\s+\.logs-col-cache-read,\s*[\r\n\s]*\.logs-table\s+\.logs-col-cache-write,\s*[\r\n\s]*\.logs-table\s+\.logs-col-cost,\s*[\r\n\s]*\.logs-table\s+\.logs-col-message\s*\{[\s\S]*?display:\s*flex\s*!important;[\s\S]*?align-items:\s*center;/);
  assert.match(logsCss, /\.logs-table\s+\.logs-col-time::before,\s*[\r\n\s]*\.logs-table\s+\.logs-col-status::before,\s*[\r\n\s]*\.logs-table\s+\.logs-col-channel::before,\s*[\r\n\s]*\.logs-table\s+\.logs-col-model::before,\s*[\r\n\s]*\.logs-table\s+\.logs-col-api-key::before,\s*[\r\n\s]*\.logs-table\s+\.logs-col-ip::before,\s*[\r\n\s]*\.logs-table\s+\.logs-col-timing::before,\s*[\r\n\s]*\.logs-table\s+\.logs-col-input::before,\s*[\r\n\s]*\.logs-table\s+\.logs-col-output::before,\s*[\r\n\s]*\.logs-table\s+\.logs-col-cache-read::before,\s*[\r\n\s]*\.logs-table\s+\.logs-col-cache-write::before,\s*[\r\n\s]*\.logs-table\s+\.logs-col-cost::before,\s*[\r\n\s]*\.logs-table\s+\.logs-col-message::before\s*\{[\s\S]*?width:\s*auto\s*!important;[\s\S]*?margin-bottom:\s*0\s*!important;/);
});

test('logs 页手机端将首字耗时拆成纵向两行显示', () => {
  assert.match(logsScript, /class="log-timing-pair"/);
  assert.match(logsScript, /class="log-timing-first-byte"/);
  assert.match(logsScript, /class="log-timing-separator"/);
  assert.match(logsScript, /class="log-timing-duration"/);
  assert.match(logsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.logs-table\s+\.logs-col-timing\s+\.log-timing-pair\s*\{[\s\S]*?flex-direction:\s*column;[\s\S]*?align-items:\s*flex-end;/);
  assert.match(logsCss, /\.logs-table\s+\.logs-col-timing\s+\.log-timing-separator\s*\{[\s\S]*?display:\s*none;/);
});

test('stats 页为手机卡片布局补齐模板标签和重排样式', () => {
  assert.match(statsHtml, /class="modern-table stats-table mobile-card-table"/);
  assert.match(statsHtml, /<template id="tpl-stats-row">[\s\S]*?class="mobile-card-row stats-data-row"/);
  assert.match(statsHtml, /class="stats-col-channel"[^>]*data-mobile-label="\{\{mobileLabelChannel\}\}"/);
  assert.match(statsHtml, /class="stats-col-cost"[^>]*data-mobile-label="\{\{mobileLabelCost\}\}"/);
  assert.match(statsHtml, /<template id="tpl-stats-total">[\s\S]*?class="mobile-card-row stats-total-row"/);
  assert.match(statsScript, /mobileLabelChannel:\s*t\('stats\.channelName'\)/);
  assert.match(statsScript, /mobileLabelCost:\s*t\('stats\.costUsd'\)/);
  assert.match(sharedCss, /\.stats-table\s+\.stats-col-channel,\s*[\r\n\s]*\.stats-table\s+\.stats-col-model,\s*[\r\n\s]*\.stats-table\s+\.stats-col-total-label\s*\{[\s\S]*?grid-column:\s*1\s*\/\s*-1;/);
});

test('stats 页手机端压缩筛选摘要行、渠道信息和健康度指示器', () => {
  assert.match(statsHtml, /class="section-title stats-detail-heading/);
  assert.match(statsHtml, /class="stats-detail-heading-main"/);
  assert.match(statsHtml, /class="stats-detail-sort-hint"/);
  assert.match(sharedCss, /\.stats-filter-summary-row\s*\{[\s\S]*?grid-template-columns:\s*minmax\(0,\s*1fr\)\s+auto\s+auto;[\s\S]*?align-items:\s*center;/);
  assert.match(sharedCss, /\.stats-table\s+\.stats-col-channel,\s*[\r\n\s]*\.stats-table\s+\.stats-col-model\s*\{[\s\S]*?display:\s*flex\s*!important;[\s\S]*?align-items:\s*center;/);
  assert.match(sharedCss, /\.stats-table\s+\.stats-col-channel::before,\s*[\r\n\s]*\.stats-table\s+\.stats-col-model::before\s*\{[\s\S]*?width:\s*auto\s*!important;[\s\S]*?margin-bottom:\s*0\s*!important;/);
  assert.match(sharedCss, /\.stats-table\s+\.stats-col-success,\s*[\r\n\s]*\.stats-table\s+\.stats-col-error,\s*[\r\n\s]*\.stats-table\s+\.stats-col-timing,\s*[\r\n\s]*\.stats-table\s+\.stats-col-rpm,\s*[\r\n\s]*\.stats-table\s+\.stats-col-input,\s*[\r\n\s]*\.stats-table\s+\.stats-col-output,\s*[\r\n\s]*\.stats-table\s+\.stats-col-cache-read,\s*[\r\n\s]*\.stats-table\s+\.stats-col-cache-create,\s*[\r\n\s]*\.stats-table\s+\.stats-col-cost\s*\{[\s\S]*?display:\s*flex\s*!important;[\s\S]*?justify-content:\s*space-between;/);
  assert.match(sharedCss, /\.stats-table\s+\.health-indicator\s*\{[\s\S]*?max-width:\s*100%;[\s\S]*?overflow:\s*hidden;/);
  assert.match(sharedCss, /\.stats-table\s+\.health-track\s*\{[\s\S]*?width:\s*min\(100%,\s*335px\);[\s\S]*?min-width:\s*0;[\s\S]*?flex:\s*1\s+1\s+auto;/);
  assert.match(sharedCss, /\.stats-table\s+\.health-rate\s*\{[\s\S]*?display:\s*none;/);
});

test('stats 页将成功率并到成功计数后并收紧 RPM 显示', () => {
  assert.match(statsHtml, /<template id="tpl-stats-row">[\s\S]*?class="stats-col-success"[^>]*>\{\{\{successDisplay\}\}\}<\/td>/);
  assert.match(statsHtml, /<template id="tpl-stats-total">[\s\S]*?class="stats-col-success"[^>]*>\{\{\{successDisplay\}\}\}<\/td>/);
  assert.match(statsScript, /function\s+buildSuccessDisplay[\s\S]*?class="stats-success-inline"[\s\S]*?class="stats-success-separator"/);
  assert.match(statsScript, /function\s+buildCompactRpmDisplay[\s\S]*?stats-rpm-inline[\s\S]*?stats-rpm-separator|function\s+buildCompactRpmDisplay[\s\S]*?stats-rpm-separator[\s\S]*?stats-rpm-inline/);
  assert.match(statsScript, /text\.endsWith\('\.0%'\)\s*\?\s*text\.slice\(0,\s*-3\)\s*\+\s*'%'\s*:\s*text/);
  assert.match(sharedCss, /\.stats-success-inline,\s*[\r\n\s]*\.stats-rpm-inline\s*\{[\s\S]*?display:\s*inline-flex;[\s\S]*?align-items:\s*center;[\s\S]*?gap:\s*2px;/);
});

test('trend 页手机端堆叠头部工具栏并释放图表高度', () => {
  assert.match(trendHtml, /class="flex justify-between items-center mb-6 trend-chart-header"/);
  assert.match(trendHtml, /class="text-xl font-semibold trend-chart-title"/);
  assert.match(trendHtml, /class="flex items-center trend-chart-toolbar"/);
  assert.match(trendScript, /const trendTypeGroup = document\.getElementById\('trend-type-group'\)/);
  assert.match(sharedCss, /body\.trend-page\s+\.main-content\s*\{[\s\S]*?height:\s*auto;/);
  assert.match(sharedCss, /\.trend-chart-header\s*\{[\s\S]*?flex-direction:\s*column;[\s\S]*?align-items:\s*stretch;/);
  assert.match(sharedCss, /\.trend-chart-toolbar\s*\{[\s\S]*?flex-direction:\s*column;[\s\S]*?align-items:\s*stretch\s*!important;[\s\S]*?width:\s*100%;/);
  assert.match(sharedCss, /\.trend-chart-toolbar\s+\.channel-filter-container\s*\{[\s\S]*?width:\s*100%;/);
  assert.match(sharedCss, /\.trend-chart-toolbar\s+#btn-channel-filter-toggle\s*\{[\s\S]*?width:\s*100%;/);
  assert.match(sharedCss, /body\.trend-page\s+\.chart-container\s*\{[\s\S]*?min-height:\s*320px;/);
});

test('tokens 页为手机卡片布局补齐模板标签和按钮布局', () => {
  assert.match(tokensScript, /table\.className\s*=\s*'mobile-card-table tokens-table'/);
  assert.doesNotMatch(tokensCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?min-width:\s*980px;/);
  assert.match(tokensHtml, /<template id="tpl-token-row">[\s\S]*?class="mobile-card-row token-card-row"/);
  assert.match(tokensHtml, /class="tokens-col-description"[^>]*data-mobile-label="\{\{mobileLabelDescription\}\}"/);
  assert.match(tokensHtml, /class="tokens-col-actions"[^>]*data-mobile-label="\{\{mobileLabelActions\}\}"/);
  assert.match(tokensScript, /mobileLabelDescription:\s*t\('tokens\.table\.description'\)/);
  assert.match(tokensScript, /mobileLabelActions:\s*t\('tokens\.table\.actions'\)/);
  assert.match(tokensCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.tokens-table\s+\.tokens-col-description,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-token,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-token-usage,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-last-used,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-actions\s*\{[\s\S]*?grid-column:\s*1\s*\/\s*-1;/);
  assert.match(tokensCss, /\.token-row-actions\s*\{[\s\S]*?justify-content:\s*center;[\s\S]*?flex-wrap:\s*nowrap;/);
});

test('tokens 页手机卡片将统计标签和值压缩为左右同行', () => {
  assert.match(tokensCss, /\.tokens-table\s+\.tokens-col-calls,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-success-rate,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-rpm,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-token-usage,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-cost,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-stream,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-non-stream,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-last-used\s*\{[\s\S]*?display:\s*flex\s*!important;[\s\S]*?align-items:\s*center;[\s\S]*?justify-content:\s*space-between;/);
  assert.match(tokensCss, /\.tokens-table\s+\.tokens-col-calls::before,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-success-rate::before,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-rpm::before,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-token-usage::before,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-cost::before,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-stream::before,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-non-stream::before,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-last-used::before\s*\{[\s\S]*?width:\s*auto\s*!important;[\s\S]*?margin-bottom:\s*0\s*!important;/);
});

test('tokens 页手机卡片将描述令牌和调用次数压缩为单行主信息', () => {
  assert.match(tokensCss, /\.tokens-table\s+\.tokens-col-description,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-token\s*\{[\s\S]*?display:\s*flex\s*!important;[\s\S]*?align-items:\s*center;/);
  assert.match(tokensCss, /\.tokens-table\s+\.tokens-col-description::before,\s*[\r\n\s]*\.tokens-table\s+\.tokens-col-token::before\s*\{[\s\S]*?width:\s*auto\s*!important;[\s\S]*?margin-bottom:\s*0\s*!important;/);
  assert.match(tokensScript, /function\s+buildCallsHtml[\s\S]*?display:\s*inline-flex;[\s\S]*?align-items:\s*center;[\s\S]*?gap:\s*4px;/);
  assert.doesNotMatch(tokensScript, /let html = '<div style="display: flex; flex-direction: column; gap: 4px; align-items: center;">'/);
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
  assert.match(channelsUrlsScript, /setAttribute\('data-mobile-label', window\.t\('common\.requests'\)\)/);
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
  assert.match(channelsHtml, /class="channel-editor-primary-row"/);
  assert.match(channelsHtml, /class="channel-editor-primary-field channel-editor-primary-field--name"/);
  assert.match(channelsHtml, /class="channel-editor-primary-field channel-editor-primary-field--type"/);
  assert.match(channelsHtml, /class="channel-editor-section-header"/);
  assert.match(channelsHtml, /class="[^"]*channel-editor-section-title[^"]*"/);
  assert.match(channelsHtml, /class="[^"]*channel-editor-section-meta[^"]*"/);
  assert.match(channelsHtml, /class="[^"]*channel-editor-section-actions[^"]*"/);
  assert.match(channelsHtml, /class="channel-editor-footer"/);
  assert.match(channelsHtml, /class="channel-editor-footer-actions"/);

  assert.match(channelsCss, /\.channel-editor-modal\s*\{[\s\S]*?max-width:\s*1120px;/);
  assert.match(channelsCss, /\.channel-editor-primary-row\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*minmax\(0,\s*1fr\)\s+minmax\(320px,\s*max-content\);/);
  assert.match(channelsCss, /\.channel-editor-section-header\s*\{[\s\S]*?display:\s*flex;[\s\S]*?justify-content:\s*space-between;/);
  assert.match(channelsCss, /\.channel-editor-footer-actions\s*\{[\s\S]*?display:\s*flex;[\s\S]*?justify-content:\s*flex-end;/);
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.channel-editor-section-stack\s*\{[\s\S]*?flex:\s*(?:none|0\s+0\s+auto);/);
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.channel-editor-section-actions\s*\{[\s\S]*?flex:\s*(?:none|0\s+0\s+auto);/);
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.channel-editor-modal\s*\{[\s\S]*?width:\s*min\(100%,\s*calc\(100vw - 16px\)\);[\s\S]*?margin:\s*8px;[\s\S]*?padding:\s*16px;[\s\S]*?min-height:\s*calc\(100vh - 16px\);[\s\S]*?\.channel-editor-primary-row\s*\{[\s\S]*?grid-template-columns:\s*1fr;[\s\S]*?\.channel-editor-section-header\s*\{[\s\S]*?flex-direction:\s*column;[\s\S]*?align-items:\s*stretch;[\s\S]*?\.channel-editor-section-actions\s*\{[\s\S]*?width:\s*100%;[\s\S]*?justify-content:\s*flex-start;/);
});

test('channels 编辑弹窗在手机端将基础字段、按钮条和卡片内容压成单行信息流', () => {
  assert.match(channelsCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.channel-editor-primary-field\s*\{[\s\S]*?flex-direction:\s*row;[\s\S]*?align-items:\s*center;/);
  assert.match(channelsCss, /\.channel-editor-form\s*\{[\s\S]*?min-height:\s*100%;[\s\S]*?flex:\s*1\s+1\s+auto;/);
  assert.match(channelsCss, /\.channel-editor-primary-field--type\s+\.channel-editor-radio-group,\s*[\r\n\s]*\.channel-editor-radio-group--strategy\s*\{[\s\S]*?flex-direction:\s*row;[\s\S]*?flex-wrap:\s*nowrap;[\s\S]*?overflow-x:\s*auto;/);
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
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-status\s*\{[\s\S]*?grid-column:\s*1\s*\/\s*3;[\s\S]*?grid-row:\s*2;/);
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-latency\s*\{[\s\S]*?grid-column:\s*3;[\s\S]*?grid-row:\s*2;[\s\S]*?justify-content:\s*flex-end;/);
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-requests\s*\{[\s\S]*?grid-column:\s*4;[\s\S]*?grid-row:\s*2;[\s\S]*?justify-content:\s*flex-end;/);
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-actions\s*\{[\s\S]*?order:\s*3;[\s\S]*?justify-content:\s*flex-end;[\s\S]*?border-top:\s*none;/);
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-actions\s*\{[\s\S]*?grid-column:\s*4;[\s\S]*?grid-row:\s*1;/);
  assert.match(channelsCss, /\.inline-key-table\s+tbody\s+\.mobile-inline-row\s*\{[\s\S]*?padding:\s*10px\s+12px;[\s\S]*?gap:\s*6px\s+8px;/);
  assert.match(channelsCss, /\.inline-key-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-key-col-key\s*\{[\s\S]*?grid-column:\s*2\s*\/\s*3;[\s\S]*?grid-row:\s*1;/);
  assert.match(channelsCss, /\.inline-key-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-key-col-actions\s*\{[\s\S]*?order:\s*3;[\s\S]*?justify-content:\s*flex-end;[\s\S]*?border-top:\s*none;/);
  assert.match(channelsCss, /\.inline-key-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-key-col-actions\s*\{[\s\S]*?grid-column:\s*3;[\s\S]*?grid-row:\s*1;/);
  assert.match(channelsCss, /\.inline-key-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-key-col-status\s*\{[\s\S]*?grid-column:\s*4;[\s\S]*?grid-row:\s*1;[\s\S]*?justify-content:\s*flex-end;[\s\S]*?white-space:\s*nowrap;/);
  assert.match(channelsCss, /\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-url::before,\s*[\r\n\s]*\.inline-url-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-url-col-actions::before\s*\{[\s\S]*?content:\s*none;/);
  assert.match(channelsCss, /\.inline-key-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-key-col-key::before,\s*[\r\n\s]*\.inline-key-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-key-col-status::before,\s*[\r\n\s]*\.inline-key-table\s+tbody\s+\.mobile-inline-row\s+td\.inline-key-col-actions::before\s*\{[\s\S]*?content:\s*none;/);
});

test('tokens 弹窗模型限制表为手机布局补齐类名、标签和按钮重排', () => {
  assert.match(tokensHtml, /<table class="inline-table mobile-inline-table allowed-models-table">/);
  assert.match(tokensScript, /class="mobile-inline-row allowed-model-row"/);
  assert.match(tokensScript, /class="allowed-model-col-name" data-mobile-label="\$\{mobileLabelModelName\}"/);
  assert.match(tokensScript, /class="allowed-model-col-actions" data-mobile-label="\$\{mobileLabelActions\}"/);
  assert.match(tokensScript, /const mobileLabelModelName = t\('tokens\.modelName'\)/);
  assert.match(tokensScript, /const mobileLabelActions = t\('tokens\.table\.actions'\)/);
  assert.match(tokensCss, /\.allowed-models-table\s+tbody\s+\.mobile-inline-row\s*\{[\s\S]*?grid-template-columns:\s*auto\s+minmax\(0,\s*1fr\)\s+auto;[\s\S]*?align-items:\s*center;/);
  assert.match(tokensCss, /\.allowed-models-table\s+tbody\s+\.mobile-inline-row\s+td\.allowed-model-col-name\s*\{[\s\S]*?grid-column:\s*auto;[\s\S]*?white-space:\s*nowrap;[\s\S]*?overflow-x:\s*auto;/);
  assert.match(tokensCss, /\.allowed-models-table\s+tbody\s+\.mobile-inline-row\s+td\.allowed-model-col-actions\s*\{[\s\S]*?grid-column:\s*auto;[\s\S]*?justify-content:\s*flex-end;/);
  assert.match(tokensCss, /\.allowed-models-table\s+tbody\s+\.mobile-inline-row\s+td\.allowed-model-col-name::before,\s*[\r\n\s]*\.allowed-models-table\s+tbody\s+\.mobile-inline-row\s+td\.allowed-model-col-actions::before\s*\{[\s\S]*?content:\s*none;/);
});

test('tokens 编辑令牌弹窗在手机端将字段和模型限制操作压成单行信息流', () => {
  assert.match(tokensHtml, /class="modal-body token-edit-body"/);
  assert.match(tokensHtml, /class="form-group form-row-inline token-edit-field token-edit-field--description"/);
  assert.match(tokensHtml, /class="form-group form-row-inline token-edit-field token-edit-field--expiry"/);
  assert.match(tokensHtml, /class="form-group form-row-inline token-edit-field token-edit-field--cost"/);
  assert.match(tokensHtml, /class="[^"]*token-edit-active-row[^"]*"/);
  assert.match(tokensHtml, /class="[^"]*token-edit-models-section[^"]*"/);
  assert.match(tokensHtml, /class="[^"]*token-edit-models-header[^"]*"/);
  assert.match(tokensHtml, /class="[^"]*token-edit-models-actions[^"]*"/);

  assert.match(tokensCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.token-edit-body\s*\{[\s\S]*?gap:\s*12px;/);
  assert.match(tokensCss, /\.token-edit-field\s*\{[\s\S]*?display:\s*flex;[\s\S]*?align-items:\s*center;/);
  assert.match(tokensCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.token-edit-field\s*\{[\s\S]*?flex-direction:\s*row;[\s\S]*?align-items:\s*center;/);
  assert.match(tokensCss, /@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.token-edit-field\s+\.form-label\s*\{[\s\S]*?flex:\s*0\s+0\s+60px;[\s\S]*?min-width:\s*60px;[\s\S]*?white-space:\s*nowrap;/);
  assert.match(tokensCss, /\.token-edit-field\s+\.form-label,\s*[\r\n\s]*\.token-edit-active-row\s+label,\s*[\r\n\s]*\.token-edit-models-title\s*\{[\s\S]*?margin-bottom:\s*0;/);
  assert.match(tokensCss, /\.token-edit-cost-row,\s*[\r\n\s]*\.token-edit-models-actions\s*\{[\s\S]*?display:\s*flex;[\s\S]*?align-items:\s*center;[\s\S]*?flex-wrap:\s*nowrap;[\s\S]*?overflow-x:\s*auto;/);
  assert.match(tokensCss, /\.token-edit-models-actions\s+\.btn,\s*[\r\n\s]*\.token-edit-cost-row\s+#editCostUsedDisplay\s*\{[\s\S]*?flex:\s*0\s+0\s+auto;/);
});

test('settings 页为手机卡片布局补齐模板标签和分组样式', () => {
  assert.match(settingsHtml, /id="settings-group-nav-section" class="mt-2 mb-2 settings-group-nav-section" hidden/);
  assert.match(settingsHtml, /class="time-range-container settings-group-nav-container"/);
  assert.match(settingsHtml, /class="modern-table settings-table mobile-card-table"/);
  assert.match(settingsHtml, /<template id="tpl-setting-row">[\s\S]*?class="mobile-card-row setting-data-row"/);
  assert.match(settingsHtml, /class="setting-col-description"[^>]*data-mobile-label="\{\{mobileLabelDescription\}\}"/);
  assert.match(settingsHtml, /class="setting-col-actions"[^>]*data-mobile-label="\{\{mobileLabelActions\}\}"/);
  assert.match(settingsHtml, /<template id="tpl-setting-group-row">[\s\S]*?class="setting-group-row"/);
  assert.match(settingsScript, /class="settings-bool-group"/);
  assert.match(settingsScript, /mobileLabelDescription:\s*t\('settings\.configItem'\)/);
  assert.match(settingsScript, /mobileLabelActions:\s*t\('common\.actions'\)/);
  assert.match(settingsScript, /const navSection = document\.getElementById\('settings-group-nav-section'\);/);
  assert.match(settingsScript, /const hasMultipleGroups = Array\.isArray\(groups\) && groups\.length > 1;/);
  assert.match(settingsScript, /navSection\.hidden = !hasMultipleGroups;/);
  assert.match(sharedCss, /\.settings-group-nav-section\[hidden\]\s*\{[\s\S]*?display:\s*none\s*!important;/);
  assert.match(sharedCss, /\.settings-table\s+\.setting-data-row\s*\{[\s\S]*?grid-template-columns:\s*minmax\(0,\s*1fr\)\s+auto;/);
  assert.match(sharedCss, /\.settings-table\.mobile-card-table\s+td\.setting-col-description::before,\s*[\r\n\s]*\.settings-table\.mobile-card-table\s+td\.setting-col-value::before\s*\{[\s\S]*?display:\s*inline(?:-block|-flex)?;[\s\S]*?margin:\s*0\s+8px\s+0\s+0;/);
  assert.match(sharedCss, /\.settings-table\.mobile-card-table\s+td\.setting-col-value\s*\{[\s\S]*?display:\s*flex;[\s\S]*?align-items:\s*center;[\s\S]*?flex-wrap:\s*wrap;/);
  assert.match(sharedCss, /\.settings-table\.mobile-card-table\s+td\.setting-col-value\s+\.settings-bool-group\s*\{[\s\S]*?display:\s*flex;[\s\S]*?flex-wrap:\s*nowrap;/);
  assert.match(sharedCss, /\.settings-table\.mobile-card-table\s+td\.setting-col-value\s+\.settings-bool-option\s*\{[\s\S]*?display:\s*inline-flex;[\s\S]*?align-items:\s*center;[\s\S]*?white-space:\s*nowrap;/);
  assert.match(sharedCss, /\.settings-table\.mobile-card-table\s+td\.setting-col-actions\s*\{[\s\S]*?grid-column:\s*2\s*\/\s*3;[\s\S]*?justify-content:\s*flex-end;/);
  assert.match(sharedCss, /\.settings-table\.mobile-card-table\s+td\.setting-col-actions::before\s*\{[\s\S]*?content:\s*none;/);
  assert.match(sharedCss, /\.settings-table\s+\.setting-group-row\s*\{[\s\S]*?grid-column:\s*1\s*\/\s*-1;/);
});

test('model-test 页为手机卡片布局补齐模板标签和重排样式', () => {
  const lastFilterRule = getLastRuleBody(sharedCss, '.model-test-toolbar-section--filters');
  const lastActionsRule = getLastRuleBody(sharedCss, '.model-test-toolbar-section--actions');
  const lastMetaRule = getLastRuleBody(sharedCss, '.model-test-toolbar-section--meta');
  const lastNameFilterRule = getLastRuleBody(sharedCss, '.model-test-control--name-filter');
  const lastTogglesRule = getLastRuleBody(sharedCss, '.model-test-toolbar-toggles');

  assert.match(modelTestHtml, /class="model-test-toolbar-section model-test-toolbar-section--filters"/);
  assert.match(modelTestHtml, /class="model-test-toolbar-section model-test-toolbar-section--actions"/);
  assert.match(modelTestHtml, /class="model-test-toolbar-section model-test-toolbar-section--meta"/);
  assert.match(modelTestHtml, /class="model-test-toolbar-toggles"/);
  assert.match(modelTestHtml, /class="model-test-control model-test-control--type"/);
  assert.match(modelTestHtml, /class="model-test-toolbar-section model-test-toolbar-section--actions"[\s\S]*?id="runTestBtn"/);
  assert.match(modelTestHtml, /class="model-test-toolbar-section model-test-toolbar-section--meta"[\s\S]*?class="model-test-control model-test-control--name-filter"[\s\S]*?class="model-test-toolbar-toggles"/);
  assert.match(modelTestHtml, /id="modelTestMobileNameFilter"/);
  assert.match(modelTestHtml, /class="modern-table model-test-table mobile-card-table"/);
  assert.doesNotMatch(modelTestHtml, /class="modern-table model-test-table mobile-card-table mobile-card-table--selectable"/);
  assert.match(modelTestHtml, /<th class="table-col-select mobile-card-select-header"><input type="checkbox" id="selectAllCheckbox" data-change-action="toggle-all-models"><\/th>/);
  assert.match(modelTestHtml, /<template id="tpl-model-row">[\s\S]*?class="mobile-card-row model-test-row"/);
  assert.match(modelTestHtml, /class="model-test-col-name"[^>]*data-mobile-label="\{\{mobileLabelName\}\}"/);
  assert.match(modelTestHtml, /class="model-test-col-response[^"]*"[^>]*data-mobile-label="\{\{mobileLabelResponse\}\}"/);
  assert.match(modelTestHtml, /<template id="tpl-channel-row-by-model">[\s\S]*?class="mobile-card-row model-test-row"/);
  assert.match(modelTestScript, /const mobileNameFilterInput = document\.getElementById\('modelTestMobileNameFilter'\);/);
  assert.match(modelTestScript, /function syncNameFilterInputs\(\)/);
  assert.match(modelTestScript, /mobileNameFilterInput\.addEventListener\('input',/);
  assert.match(modelTestScript, /getResultRowMobileLabels\('common\.model'/);
  assert.match(modelTestScript, /mobileLabelResponse:\s*i18nText\('modelTest\.responseContent'/);
  assert.match(sharedCss, /\.model-test-toolbar\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*1fr;/);
  assert.match(sharedCss, /\.model-test-toolbar-section--filters\s*\{[\s\S]*?grid-template-columns:\s*minmax\(88px,\s*104px\)\s+minmax\(0,\s*1fr\);/);
  assert.match(sharedCss, /#channelSelectorLabel,\s*[\r\n\s]*#modelSelectorLabel\s*\{[\s\S]*?grid-column:\s*1\s*\/\s*-1;/);
  assert.match(sharedCss, /\.model-test-control--type\s*\{[\s\S]*?grid-column:\s*1\s*\/\s*2;/);
  assert.match(sharedCss, /\.model-test-control--content\s*\{[\s\S]*?grid-column:\s*2\s*\/\s*3;/);
  assert.match(sharedCss, /\.model-test-control--name-filter\s*\{[\s\S]*?display:\s*none;/);
  assert.match(sharedCss, /\.model-test-toolbar-section--actions\s*\{[\s\S]*?display:\s*flex;[\s\S]*?flex-wrap:\s*nowrap;/);
  assert.match(sharedCss, /\.model-test-toolbar-section--meta\s*\{[\s\S]*?display:\s*flex;[\s\S]*?flex-wrap:\s*nowrap;/);
  assert.match(sharedCss, /\.model-test-toolbar-toggles\s*\{[\s\S]*?display:\s*flex;[\s\S]*?flex-wrap:\s*nowrap;/);
  assert.match(sharedCss, /\.model-test-toolbar-section--actions\s+\.model-test-toolbar-btn\s*\{[\s\S]*?flex:\s*1\s+1\s+0;/);
  assert.match(lastFilterRule, /grid-template-columns:\s*minmax\(88px,\s*104px\)\s+minmax\(0,\s*1fr\)/);
  assert.doesNotMatch(lastFilterRule, /grid-template-columns:\s*1fr/);
  assert.match(lastNameFilterRule, /display:\s*flex/);
  assert.match(lastNameFilterRule, /flex:\s*1\s+1\s+auto/);
  assert.match(lastNameFilterRule, /min-width:\s*0/);
  assert.match(lastNameFilterRule, /flex-direction:\s*row/);
  assert.match(lastNameFilterRule, /align-items:\s*center/);
  assert.doesNotMatch(lastNameFilterRule, /grid-column/);
  assert.match(lastActionsRule, /display:\s*flex/);
  assert.match(lastActionsRule, /flex-wrap:\s*nowrap/);
  assert.doesNotMatch(lastActionsRule, /grid-template-columns/);
  assert.match(lastMetaRule, /display:\s*flex/);
  assert.match(lastMetaRule, /flex-wrap:\s*nowrap/);
  assert.doesNotMatch(lastMetaRule, /grid-template-columns/);
  assert.match(lastTogglesRule, /display:\s*flex/);
  assert.match(lastTogglesRule, /flex-wrap:\s*nowrap/);
  assert.match(lastTogglesRule, /flex:\s*0\s+0\s+auto/);
  assert.doesNotMatch(lastTogglesRule, /grid-template-columns/);
  assert.match(sharedCss, /\.model-test-table\s+\.model-test-col-name,\s*[\r\n\s]*\.model-test-table\s+\.model-test-col-response\s*\{[\s\S]*?grid-column:\s*1\s*\/\s*-1;/);
  assert.match(sharedCss, /\.model-test-table\s+\.model-test-col-select\s*\{[\s\S]*?position:\s*absolute;/);
});

test('tokens 和 model-test 页移除关键固定高度与控件宽度硬编码', () => {
  assert.doesNotMatch(tokensHtml, /id="editModal"[\s\S]*?style="height:\s*680px/);
  assert.doesNotMatch(tokensHtml, /id="tokenDescription"[^>]*style=/);
  assert.doesNotMatch(modelTestHtml, /id="modelSelectorLabel"[^>]*min-width:\s*280px/);
  assert.doesNotMatch(modelTestHtml, /id="concurrency"[^>]*style="[^"]*width:\s*50px/);
  assert.doesNotMatch(modelTestHtml, /id="runTestBtn"[^>]*style="[^"]*padding:\s*8px 16px/);
});
