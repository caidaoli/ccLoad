const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const uiSource = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');
const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');
const modelTestSource = fs.readFileSync(path.join(__dirname, 'model-test.js'), 'utf8');
const tokensSource = fs.readFileSync(path.join(__dirname, 'tokens.js'), 'utf8');
const channelsInitSource = fs.readFileSync(path.join(__dirname, 'channels-init.js'), 'utf8');

function extractCommonUiHelpers(source) {
  const startMarker = '// 公共工具函数（DRY原则：消除重复代码）';
  const endMarker = '// 通用可搜索下拉选择框组件 (SearchableCombobox)';
  const start = source.indexOf(startMarker);
  const end = source.indexOf(endMarker);

  assert.notEqual(start, -1, '找不到 ui.js 公共工具函数区块起点');
  assert.notEqual(end, -1, '找不到 ui.js 公共工具函数区块终点');

  return source.slice(start, end);
}

function loadUiCommonHelpers() {
  const body = { dataset: {} };
  const sandbox = {
    console,
    document: { body },
    window: {}
  };

  vm.createContext(sandbox);
  vm.runInContext(extractCommonUiHelpers(uiSource), sandbox);
  return {
    body,
    window: sandbox.window
  };
}

function createRoot() {
  const listeners = new Map();
  return {
    listeners,
    addEventListener(type, handler) {
      listeners.set(type, handler);
    }
  };
}

function createTarget(selector, dataset, props = {}) {
  return {
    dataset,
    ...props,
    closest(currentSelector) {
      return currentSelector === selector ? this : null;
    }
  };
}

test('ui.js 暴露共享的页面动作委托 helper，并按事件类型分发 data-* 动作', () => {
  const { body, window } = loadUiCommonHelpers();
  const root = createRoot();
  const calls = [];

  assert.equal(typeof window.initDelegatedActions, 'function');

  const initialized = window.initDelegatedActions({
    root,
    boundElement: body,
    boundKey: 'pageActionsBound',
    click: {
      open: (target) => calls.push(['click', target.dataset.action])
    },
    change: {
      toggle: (target) => calls.push(['change', target.dataset.changeAction, target.checked])
    },
    input: {
      filter: (target) => calls.push(['input', target.dataset.inputAction, target.value])
    }
  });

  assert.equal(initialized, true);
  assert.equal(body.dataset.pageActionsBound, '1');

  root.listeners.get('click')({
    target: createTarget('[data-action]', { action: 'open' })
  });
  root.listeners.get('change')({
    target: createTarget('[data-change-action]', { changeAction: 'toggle' }, { checked: true })
  });
  root.listeners.get('input')({
    target: createTarget('[data-input-action]', { inputAction: 'filter' }, { value: 'claude' })
  });

  assert.deepEqual(calls, [
    ['click', 'open'],
    ['change', 'toggle', true],
    ['input', 'filter', 'claude']
  ]);
});

test('ui.js 的共享页面动作委托 helper 会阻止同一 boundKey 重复绑定', () => {
  const { body, window } = loadUiCommonHelpers();
  const root = createRoot();

  assert.equal(window.initDelegatedActions({
    root,
    boundElement: body,
    boundKey: 'pageActionsBound',
    click: { open: () => {} }
  }), true);

  assert.equal(window.initDelegatedActions({
    root,
    boundElement: body,
    boundKey: 'pageActionsBound',
    click: { close: () => {} }
  }), false);

  assert.equal(root.listeners.size, 1);
});

test('logs.js、model-test.js、tokens.js 和 channels-init.js 通过共享 helper 注册页面动作', () => {
  assert.match(logsSource, /window\.initDelegatedActions\(\{/);
  assert.match(logsSource, /boundKey:\s*'logsPageActionsBound'/);
  assert.match(logsSource, /'toggle-response':\s*\(actionTarget\)\s*=>/);

  assert.match(modelTestSource, /window\.initDelegatedActions\(\{/);
  assert.match(modelTestSource, /boundKey:\s*'modelTestActionsBound'/);
  assert.match(modelTestSource, /'set-test-mode':\s*\(actionTarget\)\s*=> setTestMode\(actionTarget\.dataset\.mode \|\| ''\)/);

  assert.match(tokensSource, /window\.initDelegatedActions\(\{/);
  assert.match(tokensSource, /boundKey:\s*'tokensPageActionsBound'/);
  assert.match(tokensSource, /'remove-allowed-model':\s*\(actionTarget\)\s*=>/);
  assert.match(tokensSource, /'toggle-allowed-model':\s*\(actionTarget\)\s*=>/);

  assert.match(channelsInitSource, /window\.initDelegatedActions\(\{/);
  assert.match(channelsInitSource, /boundKey:\s*'channelsPageActionsBound'/);
  assert.match(channelsInitSource, /'toggle-response':\s*\(actionTarget\)\s*=>/);
});
