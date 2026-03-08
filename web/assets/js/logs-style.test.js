const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'logs.html'), 'utf8');
const css = fs.readFileSync(path.join(__dirname, '..', 'css', 'logs.css'), 'utf8');

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

test('日志页顶部筛选栏使用页面专用布局类', () => {
  assert.match(html, /class="filter-controls\s+logs-filter-controls"/);
  assert.match(html, /class="filter-group\s+logs-filter-group"/);
  assert.match(html, /class="filter-info\s+logs-filter-info"/);
  assert.match(html, /<div class="logs-filter-actions">[\s\S]*id="btn_filter"/);
});

test('日志页窄屏筛选栏压缩标签和按钮布局', () => {
  const mobileMatch = css.match(/@media\s*\(max-width:\s*768px\)\s*\{[\s\S]*?\.logs-filter-group\s*\{[\s\S]*?display:\s*grid;[\s\S]*?grid-template-columns:\s*72px\s+minmax\(0,\s*1fr\);[\s\S]*?\.logs-filter-info\s*\{[\s\S]*?width:\s*100%;[\s\S]*?\.logs-filter-actions\s*\{[\s\S]*?width:\s*100%;[\s\S]*?\.logs-filter-actions\s+\.btn\s*\{[\s\S]*?width:\s*100%;/);
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

test('日志页筛选输入控件允许在 flex 布局中收缩', () => {
  const controlMatch = css.match(/\.logs-filter-group\s+\.filter-input,\s*\.logs-filter-group\s+\.filter-select\s*\{[^}]+\}/);
  assert.ok(controlMatch, '缺少日志页筛选控件收缩样式');

  const styleBlock = controlMatch[0];
  assert.match(styleBlock, /min-width:\s*0/);
  assert.match(styleBlock, /width:\s*100%/);
});
