const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const uiSource = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');
const themeInitSource = fs.readFileSync(path.join(__dirname, 'theme-init.js'), 'utf8');
const sharedCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'styles.css'), 'utf8');
const tokensCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'tokens.css'), 'utf8');
const logsCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'logs.css'), 'utf8');
const channelsCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'channels.css'), 'utf8');
const zhLocale = fs.readFileSync(path.join(__dirname, '..', 'locales', 'zh-CN.js'), 'utf8');
const enLocale = fs.readFileSync(path.join(__dirname, '..', 'locales', 'en.js'), 'utf8');
const htmlFiles = [
  'index.html',
  'channels.html',
  'tokens.html',
  'stats.html',
  'trend.html',
  'logs.html',
  'model-test.html',
  'settings.html',
  'login.html'
].map((file) => ({
  file,
  source: fs.readFileSync(path.join(__dirname, '..', '..', file), 'utf8')
}));

function getRuleBody(source, selector) {
  const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  const match = source.match(new RegExp(`${escaped}\\s*\\{([^}]*)\\}`));
  assert.ok(match, `missing CSS rule: ${selector}`);
  return match[1];
}

test('主题模块支持跟随系统、亮色和暗色三种模式', () => {
  assert.match(uiSource, /THEME_STORAGE_KEY\s*=\s*'ccload_theme'/);
  assert.match(uiSource, /THEME_MODES\s*=\s*\[[^\]]*'system'[^\]]*'light'[^\]]*'dark'[^\]]*\]/s);
  assert.match(uiSource, /document\.documentElement\.dataset\.theme\s*=/);
  assert.match(uiSource, /matchMedia\('\(prefers-color-scheme: dark\)'\)/);
  assert.match(uiSource, /addEventListener\('change',\s*applyStoredTheme\)/);
  assert.match(uiSource, /localStorage\.setItem\(THEME_STORAGE_KEY,\s*mode\)/);
});

test('顶部导航渲染主题下拉菜单并标记当前主题', () => {
  assert.match(uiSource, /function\s+buildThemeSwitcher\(\)/);
  assert.match(uiSource, /class:\s*'theme-switcher'/);
  assert.match(uiSource, /classList\.add\('open'\)/);
  assert.match(uiSource, /classList\.remove\('open'\)/);
  assert.match(uiSource, /data-theme-mode/);
  assert.match(uiSource, /aria-pressed/);
  assert.match(uiSource, /aria-expanded/);
  assert.match(uiSource, /theme\.system/);
  assert.match(uiSource, /theme\.light/);
  assert.match(uiSource, /theme\.dark/);
  assert.match(uiSource, /buildThemeSwitcher\(\)/);
  assert.doesNotMatch(uiSource, /theme-current-label/);
});

test('共享样式提供显式亮色和暗色主题变量与菜单样式', () => {
  assert.match(sharedCss, /html\[data-theme="dark"\]/);
  assert.match(sharedCss, /html\[data-theme="light"\]/);
  assert.match(sharedCss, /@media\s*\(prefers-color-scheme:\s*dark\)[\s\S]*html\[data-theme="system"\]/);
  assert.match(sharedCss, /\.theme-switcher/);
  assert.match(sharedCss, /\.theme-menu/);
  assert.match(sharedCss, /\.theme-switcher\.open\s+\.theme-menu/);
  assert.doesNotMatch(sharedCss, /\.theme-switcher:hover\s+\.theme-menu/);
  assert.doesNotMatch(sharedCss, /\.theme-switcher:focus-within\s+\.theme-menu/);
  assert.match(sharedCss, /\.theme-option\[aria-pressed="true"\]/);
});

test('主题菜单文案覆盖中英文', () => {
  for (const source of [zhLocale, enLocale]) {
    assert.match(source, /'theme\.label'/);
    assert.match(source, /'theme\.system'/);
    assert.match(source, /'theme\.light'/);
    assert.match(source, /'theme\.dark'/);
  }
});

test('令牌、统计和日志列表背景必须使用主题变量', () => {
  const tokenTableRule = getRuleBody(tokensCss, '.token-table');
  const tokenDisplayRule = getRuleBody(tokensCss, '.token-display');
  const tokenDisplayActiveRule = getRuleBody(tokensCss, '.token-display-active');
  const tokenDisplayInactiveRule = getRuleBody(tokensCss, '.token-display-inactive');
  const tokenDisplayExpiredRule = getRuleBody(tokensCss, '.token-display-expired');
  const tokenImportPreviewRule = getRuleBody(tokensCss, '.token-model-import-preview');
  const tokenModalContentRule = getRuleBody(tokensCss, '.modal-content');
  const tokenEditSectionRule = getRuleBody(tokensCss, '.token-edit-section');
  const tokenEditModelsRule = getRuleBody(tokensCss, '.token-edit-section--models');
  const tokenEditChannelsRule = getRuleBody(tokensCss, '.token-edit-section--channels');
  const inlineTableContainerRule = getRuleBody(sharedCss, '.inline-table-container');
  const inlineTableHeadRule = getRuleBody(sharedCss, '.inline-table thead');
  const inlineTableHeaderCellRule = getRuleBody(sharedCss, '.inline-table th');
  const inlineTableCellRule = getRuleBody(sharedCss, '.inline-table td');
  const upstreamPreRule = getRuleBody(sharedCss, '.upstream-pre');
  const modalInlineInputRule = getRuleBody(channelsCss, '.modal-inline-input');
  const modalInlineSelectRule = getRuleBody(channelsCss, '.modal-inline-select');
  const modelSelectRule = getRuleBody(channelsCss, '.model-select');
  const statsTotalRule = getRuleBody(sharedCss, '.stats-table .stats-total-row');
  const logsOddRule = getRuleBody(logsCss, '.logs-table tbody td:nth-child(odd)');
  const logsEvenRule = getRuleBody(logsCss, '.logs-table tbody td:nth-child(even)');

  assert.match(tokenTableRule, /background:\s*var\(--table-bg\)/);
  assert.doesNotMatch(tokenTableRule, /background:\s*(?:white|#fff)\s*;/);
  assert.match(tokenDisplayRule, /background:\s*var\(--surface-bg-muted\)/);
  assert.match(tokenDisplayActiveRule, /background:\s*var\(--token-active-bg\)/);
  assert.match(tokenDisplayInactiveRule, /background:\s*var\(--token-inactive-bg\)/);
  assert.match(tokenDisplayExpiredRule, /background:\s*var\(--token-expired-bg\)/);
  assert.match(tokenImportPreviewRule, /background:\s*var\(--token-import-preview-bg\)/);
  assert.match(tokenModalContentRule, /background-color:\s*var\(--surface-bg-strong\)/);
  assert.match(tokenEditSectionRule, /background:\s*var\(--surface-bg-muted\)/);
  assert.match(tokenEditModelsRule, /background:\s*var\(--surface-bg\)/);
  assert.match(tokenEditChannelsRule, /background:\s*var\(--surface-bg\)/);
  assert.match(inlineTableContainerRule, /background:\s*var\(--table-bg\)/);
  assert.match(inlineTableHeadRule, /background:\s*var\(--surface-bg-muted\)/);
  assert.match(inlineTableHeaderCellRule, /background(?:-color)?:\s*var\(--surface-bg-muted\)/);
  assert.match(inlineTableCellRule, /background:\s*var\(--table-bg\)/);
  assert.match(upstreamPreRule, /background:\s*var\(--surface-bg-muted\)/);
  assert.match(modalInlineInputRule, /background:\s*var\(--field-bg\)\s*!important/);
  assert.match(modalInlineSelectRule, /background:\s*var\(--field-bg\)\s*!important/);
  assert.match(modelSelectRule, /background:\s*var\(--field-bg\)/);
  assert.doesNotMatch(tokenDisplayActiveRule, /background:\s*var\(--success-50\)/);
  assert.doesNotMatch(tokenDisplayExpiredRule, /background:\s*var\(--error-50\)/);
  assert.doesNotMatch(tokenImportPreviewRule, /background:\s*var\(--success-50\)/);
  assert.doesNotMatch(tokenModalContentRule, /background-color:\s*white/);
  assert.doesNotMatch(modalInlineInputRule, /color-scheme:\s*light/);
  assert.doesNotMatch(modalInlineSelectRule, /color-scheme:\s*light/);
  assert.doesNotMatch(modelSelectRule, /color-scheme:\s*light/);
  assert.match(statsTotalRule, /background(?:-color)?:\s*var\(--surface-bg-muted\)/);
  assert.doesNotMatch(statsTotalRule, /background:\s*linear-gradient\(180deg,\s*#eff6ff/);
  assert.match(logsOddRule, /background:\s*var\(--table-bg\)/);
  assert.match(logsEvenRule, /background:\s*var\(--surface-bg-muted\)/);
});

test('暗色主题为可点击详情提供可读颜色', () => {
  assert.match(sharedCss, /html\[data-theme="dark"\]\s+\.has-upstream-detail,\s*[\r\n\s]*html\[data-theme="system"\]\[data-resolved-theme="dark"\]\s+\.has-upstream-detail\s*\{[\s\S]*?color:\s*var\(--primary-300\)/);
});

test('暗色主题为模型测试和日志页渠道链接提供可读颜色', () => {
  assert.match(sharedCss, /html\[data-theme="dark"\]\s+\.model-test-table\s+\.channel-link,\s*[\r\n\s]*html\[data-theme="system"\]\[data-resolved-theme="dark"\]\s+\.model-test-table\s+\.channel-link,\s*[\r\n\s]*html\[data-theme="dark"\]\s+\.logs-table\s+\.channel-link,\s*[\r\n\s]*html\[data-theme="system"\]\[data-resolved-theme="dark"\]\s+\.logs-table\s+\.channel-link\s*\{[\s\S]*?color:\s*var\(--primary-300\)/);
});

test('首页和登录页提示块使用主题变量适配暗色模式', () => {
  const indexTipRule = getRuleBody(sharedCss, '.index-api-tip');
  const securityNoticeRule = getRuleBody(sharedCss, '.security-notice');

  assert.match(indexTipRule, /background:\s*var\(--surface-bg-muted\)/);
  assert.match(indexTipRule, /border:\s*1px\s+solid\s+var\(--surface-border\)/);
  assert.doesNotMatch(indexTipRule, /background:\s*var\(--info-50\)/);

  assert.match(securityNoticeRule, /background:\s*var\(--surface-bg-muted\)/);
  assert.match(securityNoticeRule, /border:\s*1px\s+solid\s+var\(--surface-border\)/);
  assert.doesNotMatch(securityNoticeRule, /background:\s*#F0F9FF/);
});

test('所有页面在样式表加载前同步初始化主题，避免暗色模式白闪', () => {
  for (const { file, source } of htmlFiles) {
    const themeInitIndex = source.indexOf('/web/assets/js/theme-init.js');
    const firstStylesheetIndex = source.indexOf('<link rel="stylesheet"');
    assert.ok(themeInitIndex >= 0, `${file} 缺少 theme-init.js`);
    assert.ok(firstStylesheetIndex >= 0, `${file} 缺少 stylesheet`);
    assert.ok(themeInitIndex < firstStylesheetIndex, `${file} 必须在 CSS 前初始化主题`);
  }
  assert.match(themeInitSource, /style\.backgroundColor\s*=\s*resolvedTheme\s*===\s*'dark'\s*\?\s*'#0f172a'\s*:\s*'#fcfbf9'/);
  assert.match(themeInitSource, /removeProperty\('background-color'\)/);
});
