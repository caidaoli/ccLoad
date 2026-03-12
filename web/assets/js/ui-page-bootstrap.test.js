const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const uiSource = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');
const indexSource = fs.readFileSync(path.join(__dirname, 'index.js'), 'utf8');
const channelsInitSource = fs.readFileSync(path.join(__dirname, 'channels-init.js'), 'utf8');
const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');
const statsSource = fs.readFileSync(path.join(__dirname, 'stats.js'), 'utf8');
const trendSource = fs.readFileSync(path.join(__dirname, 'trend.js'), 'utf8');
const tokensSource = fs.readFileSync(path.join(__dirname, 'tokens.js'), 'utf8');
const settingsSource = fs.readFileSync(path.join(__dirname, 'settings.js'), 'utf8');
const modelTestSource = fs.readFileSync(path.join(__dirname, 'model-test.js'), 'utf8');

function extractCommonUiHelpers(source) {
  const startMarker = '// 公共工具函数（DRY原则：消除重复代码）';
  const endMarker = '// 通用可搜索下拉选择框组件 (SearchableCombobox)';
  const start = source.indexOf(startMarker);
  const end = source.indexOf(endMarker);

  assert.notEqual(start, -1, '找不到 ui.js 公共工具函数区块起点');
  assert.notEqual(end, -1, '找不到 ui.js 公共工具函数区块终点');

  return source.slice(start, end);
}

function loadUiCommonHelpers({ readyState = 'loading' } = {}) {
  const listeners = new Map();
  const body = { dataset: {} };
  const sandbox = {
    console,
    document: {
      body,
      readyState,
      addEventListener(type, handler) {
        listeners.set(type, handler);
      }
    },
    window: {},
    setTimeout,
    clearTimeout
  };

  vm.createContext(sandbox);
  vm.runInContext(extractCommonUiHelpers(uiSource), sandbox);
  return {
    body,
    listeners,
    window: sandbox.window
  };
}

test('ui.js 暴露共享页面 bootstrap helper，并在 DOM 就绪后按顺序执行 translate/topbar/run', async () => {
  const { listeners, window } = loadUiCommonHelpers({ readyState: 'loading' });
  const calls = [];

  window.i18n = {
    translatePage() {
      calls.push('translate');
    }
  };
  window.initTopbar = (key) => {
    calls.push(`topbar:${key}`);
  };

  assert.equal(typeof window.initPageBootstrap, 'function');

  window.initPageBootstrap({
    topbarKey: 'logs',
    run: () => {
      calls.push('run');
    }
  });

  assert.equal(calls.length, 0);
  assert.equal(typeof listeners.get('DOMContentLoaded'), 'function');

  listeners.get('DOMContentLoaded')();
  await Promise.resolve();

  assert.deepEqual(calls, ['translate', 'topbar:logs', 'run']);
});

test('ui.js 的共享页面 bootstrap helper 在 DOM 已就绪时立即执行', async () => {
  const { window } = loadUiCommonHelpers({ readyState: 'complete' });
  const calls = [];

  window.i18n = {
    translatePage() {
      calls.push('translate');
    }
  };
  window.initTopbar = (key) => {
    calls.push(`topbar:${key}`);
  };

  window.initPageBootstrap({
    topbarKey: 'stats',
    run: () => {
      calls.push('run');
    }
  });

  await Promise.resolve();
  assert.deepEqual(calls, ['translate', 'topbar:stats', 'run']);
});

test('关键页面通过共享 bootstrap helper 初始化，而不是各自直接绑定 DOMContentLoaded 样板', () => {
  const pages = [
    { name: 'index', source: indexSource, topbarKey: 'index' },
    { name: 'channels', source: channelsInitSource, topbarKey: 'channels' },
    { name: 'logs', source: logsSource, topbarKey: 'logs' },
    { name: 'stats', source: statsSource, topbarKey: 'stats' },
    { name: 'trend', source: trendSource, topbarKey: 'trend' },
    { name: 'tokens', source: tokensSource, topbarKey: 'tokens' },
    { name: 'settings', source: settingsSource, topbarKey: 'settings' },
    { name: 'model-test', source: modelTestSource, topbarKey: 'model-test' }
  ];

  pages.forEach(({ name, source, topbarKey }) => {
    assert.match(source, /window\.initPageBootstrap\(\{/);
    assert.match(source, new RegExp(`topbarKey:\\s*'${topbarKey}'`));
    assert.doesNotMatch(source, /document\.addEventListener\('DOMContentLoaded'/);
    assert.doesNotMatch(source, new RegExp(`if \\(window\\.initTopbar\\) initTopbar\\('${topbarKey}'\\);`));
    assert.doesNotMatch(source, /if \(window\.i18n\) window\.i18n\.translatePage\(\);/);
  });
});
