const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'logs.html'), 'utf8');
const css = fs.readFileSync(path.join(__dirname, '..', 'css', 'logs.css'), 'utf8');
const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');
const pageFiltersSource = fs.readFileSync(path.join(__dirname, 'page-filters.js'), 'utf8');

function renderLogsFilters() {
  const sandbox = {
    console,
    window: {},
    document: {
      querySelectorAll() {
        return [];
      }
    }
  };

  vm.createContext(sandbox);
  vm.runInContext(pageFiltersSource, sandbox);
  return sandbox.window.PageFilters.renderLayout('logs');
}

test('日志页底部分页使用专用紧凑样式类', () => {
  assert.match(html, /class="pagination-controls\s+logs-pagination-controls"/);
  assert.match(html, /class="pagination-info\s+logs-pagination-info"/);
  assert.match(html, /id="logs_jump_page"[\s\S]*class="logs-jump-input"/);
});

test('日志页跳转输入框显式锁定浅色背景和文字颜色', () => {
  const styleBlockMatch = css.match(/\.logs-jump-input\s*\{[^}]+\}/);
  assert.ok(styleBlockMatch, '缺少 .logs-jump-input 样式');

  const styleBlock = styleBlockMatch[0];
  assert.match(styleBlock, /background:\s*rgba\(255,\s*255,\s*255,\s*0\.9\)/);
  assert.match(styleBlock, /color:\s*var\(--neutral-900\)/);
  assert.match(styleBlock, /color-scheme:\s*light/);
});

test('日志页分页信息区收紧按钮间距', () => {
  const controlsMatch = css.match(/\.logs-pagination-controls\s*\{[^}]+\}/);
  const infoMatch = css.match(/\.logs-pagination-info\s*\{[^}]+\}/);
  assert.ok(controlsMatch, '缺少 .logs-pagination-controls 样式');
  assert.ok(infoMatch, '缺少 .logs-pagination-info 样式');

  assert.match(controlsMatch[0], /gap:\s*var\(--space-1\)/);
  assert.match(infoMatch[0], /margin:\s*0\s+var\(--space-2\)/);
});

test('日志页窄屏分页覆盖全局纵向堆叠规则', () => {
  const mobileMatch = css.match(/@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.logs-pagination-controls\s*\{[\s\S]*?flex-direction:\s*row;[\s\S]*?\.logs-pagination-info\s*\{[\s\S]*?width:\s*100%;[\s\S]*?margin:\s*0;[\s\S]*?\.logs-pagination-separator\s*\{[\s\S]*?display:\s*none;/);
  assert.ok(mobileMatch, '缺少日志页窄屏分页覆盖样式');
});

test('日志页顶部筛选栏通过共享渲染器输出页面专用布局类', () => {
  const filtersHtml = renderLogsFilters();
  assert.match(html, /data-page-filters="logs"/);
  assert.match(filtersHtml, /class="filter-controls\s+logs-filter-controls"/);
  assert.match(filtersHtml, /class="filter-group\s+logs-filter-group"/);
  assert.match(filtersHtml, /class="filter-info\s+logs-filter-info"/);
  assert.match(filtersHtml, /class="logs-filter-summary-row"/);
  assert.match(filtersHtml, /<div class="filter-actions filter-actions--page logs-filter-actions">[\s\S]*id="btn_filter"/);
});

test('日志页为来源筛选和来源 badge 预留 DOM/CSS 契约', () => {
  const filtersHtml = renderLogsFilters();
  assert.match(filtersHtml, /id="f_log_source"/);
  assert.match(logsSource, /log-source-badge/);
  assert.match(css, /\.log-source-badge\s*\{/);
});

test('日志页桌面筛选摘要行摊平到共享 flex 容器，避免挤压筛选控件', () => {
  const desktopCss = css.split(/@media\s*\(max-width:\s*768px\)/)[0];
  const summaryMatch = desktopCss.match(/\.logs-filter-summary-row\s*\{[^}]+\}/);
  assert.ok(summaryMatch, '缺少日志页桌面筛选摘要行样式');

  assert.match(summaryMatch[0], /display:\s*contents/);
});

test('日志页窄屏筛选栏压缩标签和按钮布局', () => {
  const mobileMatch = css.match(/@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.logs-filter-group\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*72px\s+minmax\(0,\s*1fr\);[\s\S]*?flex:\s*none;[\s\S]*?\.logs-filter-summary-row\s*\{[\s\S]*?grid-template-columns:\s*minmax\(0,\s*1fr\)\s+auto;[\s\S]*?\.logs-filter-summary-row\s+\.logs-filter-info\s*\{[\s\S]*?width:\s*auto;[\s\S]*?\.logs-filter-summary-row\s+\.logs-filter-actions\s*\{[\s\S]*?width:\s*auto;[\s\S]*?\.logs-filter-summary-row\s+\.logs-filter-actions\s+\.btn\s*\{[\s\S]*?width:\s*auto;/);
  assert.ok(mobileMatch, '缺少日志页窄屏筛选栏压缩样式');
});

test('日志页分页按钮使用更紧凑的内边距', () => {
  const desktopCss = css.split(/@media\s*\(max-width:\s*768px\)/)[0];
  const compactBtnMatch = desktopCss.match(/\.logs-pagination-controls\s+\.btn-sm\s*\{[^}]+\}/);
  assert.ok(compactBtnMatch, '缺少日志页分页按钮紧凑样式');

  const styleBlock = compactBtnMatch[0];
  assert.match(styleBlock, /padding:\s*2px/);
});

test('日志页分页信息文案降低字重，仅页码数字保持强调', () => {
  const infoMatch = css.match(/\.logs-pagination-info\s*\{[^}]+\}/);
  const currentMatch = css.match(/\.logs-pagination-info\s+#logs_current_page2,\s*\.logs-pagination-info\s+#logs_total_pages2\s*\{[^}]+\}/);
  assert.ok(infoMatch, '缺少 .logs-pagination-info 样式');
  assert.ok(currentMatch, '缺少页码数字强调样式');

  assert.match(infoMatch[0], /font-weight:\s*var\(--font-normal\)/);
  assert.match(infoMatch[0], /color:\s*var\(--neutral-700\)/);
  assert.match(currentMatch[0], /font-weight:\s*var\(--font-semibold\)/);
});

test('日志页分页按钮图标缩小到 14px', () => {
  const iconMatch = css.match(/\.logs-pagination-controls\s+svg\s*\{[^}]+\}/);
  assert.ok(iconMatch, '缺少日志页分页图标样式');

  const styleBlock = iconMatch[0];
  assert.match(styleBlock, /width:\s*14px/);
  assert.match(styleBlock, /height:\s*14px/);
});

test('日志页分页按钮、文案和跳转输入框使用统一字号', () => {
  const btnMatch = css.match(/\.logs-pagination-controls\s+\.btn-sm\s*\{[^}]+\}/);
  const infoMatch = css.match(/\.logs-pagination-info\s*\{[^}]+\}/);
  const inputMatch = css.match(/\.logs-jump-input\s*\{[^}]+\}/);
  assert.ok(btnMatch, '缺少日志页分页按钮样式');
  assert.ok(infoMatch, '缺少日志页分页文案样式');
  assert.ok(inputMatch, '缺少日志页跳转输入框样式');

  assert.match(btnMatch[0], /font-size:\s*var\(--text-sm\)/);
  assert.match(infoMatch[0], /font-size:\s*var\(--text-sm\)/);
  assert.match(inputMatch[0], /font-size:\s*var\(--text-sm\)/);
});

test('日志页分页数字使用等宽数字并预留最小宽度', () => {
  const infoMatch = css.match(/\.logs-pagination-info\s*\{[^}]+\}/);
  const numberMatch = css.match(/\.logs-pagination-info\s+#logs_current_page2,\s*\.logs-pagination-info\s+#logs_total_pages2\s*\{[^}]+\}/);
  assert.ok(infoMatch, '缺少 .logs-pagination-info 样式');
  assert.ok(numberMatch, '缺少分页数字样式');

  assert.match(infoMatch[0], /font-variant-numeric:\s*tabular-nums/);
  assert.match(numberMatch[0], /display:\s*inline-block/);
  assert.match(numberMatch[0], /min-width:\s*3ch/);
});


test('日志页桌面筛选组设置基准宽度避免互相挤压', () => {
  const groupMatch = css.match(/\.logs-filter-group\s*\{[^}]+\}/);
  assert.ok(groupMatch, '缺少 .logs-filter-group 样式');

  const styleBlock = groupMatch[0];
  assert.match(styleBlock, /flex:\s*1\s+1\s+180px/);
});

test('日志页范围、渠道ID、令牌筛选在桌面端使用专用组宽和控件宽度', () => {
  const rangeGroupMatch = css.match(/\.logs-filter-group--range\s*\{[^}]+\}/);
  const channelIdGroupMatch = css.match(/\.logs-filter-group--channel-id\s*\{[^}]+\}/);
  const tokenGroupMatch = css.match(/\.logs-filter-group--token\s*\{[^}]+\}/);
  const rangeControlMatch = css.match(/\.logs-filter-control--range\s*\{[^}]+\}/);
  const channelIdControlMatch = css.match(/\.logs-filter-control--channel-id\s*\{[^}]+\}/);
  const tokenControlMatch = css.match(/\.logs-filter-control--token\s*\{[^}]+\}/);

  assert.ok(rangeGroupMatch, '缺少 .logs-filter-group--range 样式');
  assert.ok(channelIdGroupMatch, '缺少 .logs-filter-group--channel-id 样式');
  assert.ok(tokenGroupMatch, '缺少 .logs-filter-group--token 样式');
  assert.ok(rangeControlMatch, '缺少 .logs-filter-control--range 样式');
  assert.ok(channelIdControlMatch, '缺少 .logs-filter-control--channel-id 样式');
  assert.ok(tokenControlMatch, '缺少 .logs-filter-control--token 样式');

  assert.match(rangeGroupMatch[0], /flex:\s*0\s+1\s+116px/);
  assert.match(channelIdGroupMatch[0], /flex:\s*0\s+1\s+134px/);
  assert.match(tokenGroupMatch[0], /flex:\s*0\s+1\s+134px/);
  assert.match(rangeControlMatch[0], /max-width:\s*80px/);
  assert.match(channelIdControlMatch[0], /max-width:\s*72px/);
  assert.match(tokenControlMatch[0], /max-width:\s*100px/);
});

test('日志页筛选输入控件允许在 flex 布局中收缩', () => {
  const controlMatch = css.match(/\.logs-filter-group\s+\.filter-input,\s*\.logs-filter-group\s+\.filter-select\s*\{[^}]+\}/);
  assert.ok(controlMatch, '缺少日志页筛选控件收缩样式');

  const styleBlock = controlMatch[0];
  assert.match(styleBlock, /min-width:\s*0/);
  assert.match(styleBlock, /width:\s*100%/);
});

test('日志页为 IP 和 API Key 提供共享等宽文本样式类', () => {
  const monoMatch = css.match(/\.logs-mono-text\s*\{[^}]+\}/);
  assert.ok(monoMatch, '缺少 .logs-mono-text 样式');

  const styleBlock = monoMatch[0];
  assert.match(styleBlock, /font-family:\s*var\(--font-family-mono\)/);
  assert.match(styleBlock, /font-size:\s*0\.85em/);
  assert.match(styleBlock, /color:\s*var\(--neutral-600\)/);
});

test('进行中请求复用日志表格列类名和共享字体类', () => {
  const activeMatch = logsSource.match(/function renderActiveRequests\(activeRequests\)\s*\{[\s\S]*?\n\}/);
  assert.ok(activeMatch, '缺少 renderActiveRequests');

  const activeSource = activeMatch[0];
  assert.match(activeSource, /class="logs-col-time"/);
  assert.match(activeSource, /class="logs-col-ip logs-mono-text"/);
  assert.match(activeSource, /class="logs-col-api-key"/);
  assert.match(activeSource, /class="logs-col-channel"/);
  assert.match(activeSource, /class="logs-col-model"/);
  assert.match(activeSource, /class="logs-col-status"/);
  assert.match(activeSource, /class="logs-col-timing"/);
  assert.match(activeSource, /class="logs-col-message"/);
  assert.match(activeSource, /data-mobile-label="\$\{logMobileLabels\.ip\}"/);
  assert.doesNotMatch(activeSource, /font-family:\s*monospace/);
});

test('普通日志渲染也使用共享等宽字体类而不是内联 monospace', () => {
  const renderMatch = logsSource.match(/function renderLogs\(data\)\s*\{[\s\S]*?\n\}/);
  assert.ok(renderMatch, '缺少 renderLogs');

  const renderSource = renderMatch[0];
  assert.match(renderSource, /class="logs-col-ip logs-mono-text"/);
  assert.match(renderSource, /class="logs-api-key-text logs-mono-text"/);
  assert.doesNotMatch(renderSource, /font-family:\s*monospace/);
});
