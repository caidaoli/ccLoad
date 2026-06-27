const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const uiSource = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');

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
