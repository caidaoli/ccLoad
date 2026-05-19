const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const uiSource = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');
const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');
const statsSource = fs.readFileSync(path.join(__dirname, 'stats.js'), 'utf8');
const trendSource = fs.readFileSync(path.join(__dirname, 'trend.js'), 'utf8');
const channelsKeysSource = fs.readFileSync(path.join(__dirname, 'channels-keys.js'), 'utf8');
const channelsModalsSource = fs.readFileSync(path.join(__dirname, 'channels-modals.js'), 'utf8');
const indexSource = fs.readFileSync(path.join(__dirname, 'index.js'), 'utf8');
const tokensSource = fs.readFileSync(path.join(__dirname, 'tokens.js'), 'utf8');
const zhLocaleSource = fs.readFileSync(path.join(__dirname, '..', 'locales', 'zh-CN.js'), 'utf8');
const enLocaleSource = fs.readFileSync(path.join(__dirname, '..', 'locales', 'en.js'), 'utf8');

const sharedCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'styles.css'), 'utf8');
const indexHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'index.html'), 'utf8');
const loginHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'login.html'), 'utf8');
const logsHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'logs.html'), 'utf8');
const statsHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'stats.html'), 'utf8');
const trendHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'trend.html'), 'utf8');
const settingsHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'settings.html'), 'utf8');
const modelTestHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'model-test.html'), 'utf8');

function duplicateLocaleKeys(source) {
  const counts = new Map();
  for (const match of source.matchAll(/^\s*'([^']+)'\s*:/gm)) {
    counts.set(match[1], (counts.get(match[1]) || 0) + 1);
  }
  return [...counts.entries()]
    .filter(([, count]) => count > 1)
    .map(([key]) => key);
}

test('ui.js 暴露统一通知和筛选状态持久化 helper', () => {
  assert.match(uiSource, /window\.ensureNotifyHost\s*=\s*ensureNotifyHost/);
  assert.match(uiSource, /window\.showWarning\s*=\s*\(msg\)\s*=>\s*window\.showNotification\(msg,\s*'warning'\)/);
  assert.match(uiSource, /window\.persistFilterState\s*=\s*persistFilterState/);
});

test('locale 文件不能重复定义同一个 key', () => {
  assert.deepEqual(duplicateLocaleKeys(zhLocaleSource), []);
  assert.deepEqual(duplicateLocaleKeys(enLocaleSource), []);
});

test('channels 页面脚本复用统一通知入口，不再引用不存在的 showToast', () => {
  assert.doesNotMatch(channelsKeysSource, /\bshowToast\(/);
  assert.match(channelsKeysSource, /window\.showSuccess\(/);
  assert.match(channelsKeysSource, /window\.showError\(/);
  assert.match(channelsModalsSource, /window\.showWarning\(/);
  assert.match(channelsModalsSource, /window\.ensureNotifyHost\(\)/);
  assert.doesNotMatch(channelsModalsSource, /host\.id\s*=\s*['"]notify-host['"]/);
});

test('index、tokens 通过 bindTimeRangeSelector 复用日期按钮重渲染', () => {
  assert.match(uiSource, /window\.bindTimeRangeSelector\s*=\s*bindTimeRangeSelector/);
  assert.match(indexSource, /window\.bindTimeRangeSelector\(/);
  assert.match(tokensSource, /window\.bindTimeRangeSelector\(/);
  // 不再保留旧的 renderTimeRangeSelector 闭包
  assert.doesNotMatch(indexSource, /const\s+renderTimeRangeSelector\s*=/);
  assert.doesNotMatch(tokensSource, /const\s+renderTimeRangeSelector\s*=/);
  // 不再在页面层重复注册 initTimeRangeSelector
  assert.doesNotMatch(indexSource, /window\.initTimeRangeSelector\(/);
  assert.doesNotMatch(tokensSource, /window\.initTimeRangeSelector\(/);
});

test('logs、stats、trend 通过共享 helper 持久化筛选状态', () => {
  [logsSource, statsSource, trendSource].forEach((source) => {
    assert.match(source, /window\.persistFilterState\(/);
  });

  assert.doesNotMatch(logsSource, /window\.FilterState\.writeHistory\(/);
  assert.doesNotMatch(statsSource, /window\.FilterState\.writeHistory\(/);
  assert.doesNotMatch(trendSource, /window\.FilterState\.writeHistory\(/);
});

test('共享样式层提供吸收内联样式的 utility class', () => {
  assert.match(sharedCss, /\.animate-delay-1\s*\{[^}]*animation-delay:\s*0\.1s;/s);
  assert.match(sharedCss, /\.animate-delay-2\s*\{[^}]*animation-delay:\s*0\.2s;/s);
  assert.match(sharedCss, /\.animate-delay-3\s*\{[^}]*animation-delay:\s*0\.3s;/s);
  assert.match(sharedCss, /\.animate-delay-4\s*\{[^}]*animation-delay:\s*0\.4s;/s);
  assert.match(sharedCss, /\.gap-space-3\s*\{[^}]*gap:\s*var\(--space-3\);/s);
  assert.match(sharedCss, /\.flex-nowrap\s*\{[^}]*flex-wrap:\s*nowrap;/s);
  assert.match(sharedCss, /\.overflow-visible\s*\{[^}]*overflow:\s*visible;/s);
  assert.match(sharedCss, /\.table-head-sticky\s*\{[^}]*position:\s*sticky;[^}]*top:\s*0;[^}]*background:\s*var\(--glass-bg\);[^}]*z-index:\s*1;/s);
  assert.match(sharedCss, /\.truncate-cell\s*\{[^}]*overflow:\s*hidden;[^}]*text-overflow:\s*ellipsis;[^}]*white-space:\s*nowrap;/s);
});

test('代表性页面改用 utility class 而不是重复内联样式', () => {
  assert.match(indexHtml, /class="grid grid-cols-2 animate-slide-up animate-delay-1 gap-space-3"/);
  assert.match(indexHtml, /class="flex index-summary-grid animate-slide-up animate-delay-2 gap-space-3 flex-nowrap"/);
  assert.match(indexHtml, /class="glass-card animate-slide-up animate-delay-3"/);
  assert.doesNotMatch(indexHtml, /style="animation-delay: 0\.1s; gap: var\(--space-3\);"/);
  assert.doesNotMatch(indexHtml, /style="animation-delay: 0\.2s; display: flex; gap: var\(--space-3\); flex-wrap: nowrap;"/);
  assert.doesNotMatch(indexHtml, /style="background: linear-gradient\(135deg,/);
  assert.doesNotMatch(indexHtml, /summary-card-info/);

  assert.match(loginHtml, /id="error-message" class="error-notification hidden"/);
  assert.match(loginHtml, /class="features-showcase animate-slide-up animate-delay-2"/);
  assert.doesNotMatch(loginHtml, /style="display: none;"/);
  assert.doesNotMatch(loginHtml, /style="animation-delay: 0\.2s;"/);

  assert.doesNotMatch(logsHtml, /style="animation-delay: 0\.2s;"/);

  assert.match(statsHtml, /class="glass-card stats-detail-card"/);
  assert.match(statsHtml, /id="stats-chart-view" class="hidden"/);
  assert.doesNotMatch(statsHtml, /style="animation-delay: 0\.3s;"/);
  assert.doesNotMatch(statsHtml, /id="stats-chart-view" style="display: none;"/);
  assert.doesNotMatch(statsHtml, /document\.write\('<style/);
  assert.doesNotMatch(statsHtml, /style="color: var\(--success-400\);"/);

  assert.match(trendHtml, /class="glass-card trend-chart-card"/);
  assert.match(trendHtml, /class="flex items-center trend-chart-toolbar gap-space-3 flex-wrap"/);
  assert.match(trendHtml, /class="channel-filter-dropdown hidden" id="channel-filter-dropdown"/);
  assert.match(trendHtml, /class="chart-error hidden" id="chart-error"/);
  assert.match(trendHtml, /id="chart" class="w-full h-full hidden"/);
  assert.doesNotMatch(trendHtml, /style="animation-delay: 0\.2s;"/);
  assert.doesNotMatch(trendHtml, /style="gap:12px; flex-wrap: wrap;"/);
  assert.doesNotMatch(trendHtml, /id="chart-error" style="display: none;"/);

  assert.match(modelTestHtml, /class="glass-card mt-2 mb-2 overflow-visible"/);
  assert.match(modelTestHtml, /id="modelSelectorLabel" class="model-test-control model-test-control--model hidden"/);
  assert.match(modelTestHtml, /<thead class="table-head-sticky">/);
  assert.match(modelTestHtml, /class="model-test-col-name truncate-cell"/);
  assert.doesNotMatch(logsHtml, /style="padding: var\(--space-2\);"/);
  assert.doesNotMatch(logsHtml, /style="display: block; padding: 8px; background: var\(--neutral-100\);/);
  assert.doesNotMatch(settingsHtml, /style="width: 60%;"/);
  assert.doesNotMatch(modelTestHtml, /<style>/);
  assert.doesNotMatch(modelTestHtml, /style="overflow: visible;"/);
  assert.doesNotMatch(modelTestHtml, /style="display: none;"/);
  assert.doesNotMatch(modelTestHtml, /style="\{\{nameStyle\}\}"/);
  assert.doesNotMatch(modelTestHtml, /style="text-align: center; color: var\(--color-text-secondary\); padding: 40px;"/);
  assert.doesNotMatch(modelTestHtml, /style="position: sticky; top: 0; background: var\(--glass-bg\); z-index: 1;"/);
});
