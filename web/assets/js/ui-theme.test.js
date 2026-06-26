const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const uiSource = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');
const themeInitSource = fs.readFileSync(path.join(__dirname, 'theme-init.js'), 'utf8');
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
