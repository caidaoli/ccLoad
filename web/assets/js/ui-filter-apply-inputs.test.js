const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const uiSource = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');
const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');
const statsSource = fs.readFileSync(path.join(__dirname, 'stats.js'), 'utf8');
const trendSource = fs.readFileSync(path.join(__dirname, 'trend.js'), 'utf8');

function extractCommonUiHelpers(source) {
  const startMarker = '// 公共工具函数（DRY原则：消除重复代码）';
  const endMarker = '// 通用可搜索下拉选择框组件 (SearchableCombobox)';
  const start = source.indexOf(startMarker);
  const end = source.indexOf(endMarker);

  assert.notEqual(start, -1, '找不到 ui.js 公共工具函数区块起点');
  assert.notEqual(end, -1, '找不到 ui.js 公共工具函数区块终点');

  return source.slice(start, end);
}

function createElement() {
  const listeners = new Map();
  return {
    listeners,
    value: '',
    addEventListener(type, handler) {
      listeners.set(type, handler);
    }
  };
}

function loadUiCommonHelpers(ids = []) {
  const elements = new Map(ids.map((id) => [id, createElement()]));
  const timeouts = [];
  const sandbox = {
    console,
    document: {
      getElementById(id) {
        return elements.get(id) || null;
      }
    },
    setTimeout(fn) {
      timeouts.push(fn);
      fn();
      return timeouts.length;
    },
    clearTimeout() {},
    window: {}
  };

  vm.createContext(sandbox);
  vm.runInContext(extractCommonUiHelpers(uiSource), sandbox);
  return {
    window: sandbox.window,
    elements
  };
}

test('ui.js 暴露共享的筛选输入绑定 helper，并按配置绑定 input/Enter 事件', () => {
  const { window, elements } = loadUiCommonHelpers(['f_id', 'f_name', 'f_hours']);
  let applyCount = 0;

  assert.equal(typeof window.bindFilterApplyInputs, 'function');

  window.bindFilterApplyInputs({
    apply() {
      applyCount += 1;
    },
    debounceInputIds: ['f_id', 'f_name'],
    enterInputIds: ['f_hours', 'f_name']
  });

  elements.get('f_id').listeners.get('input')({});
  elements.get('f_hours').listeners.get('keydown')({ key: 'Escape' });
  elements.get('f_name').listeners.get('keydown')({ key: 'Enter' });

  assert.equal(applyCount, 2);
});

test('ui.js 暴露共享的日期范围/令牌筛选初始化 helper', async () => {
  const { window, elements } = loadUiCommonHelpers(['f_hours', 'f_auth_token']);
  const rangeCalls = [];
  const tokenCalls = [];
  let rangeChangeCount = 0;
  let tokenChangeCount = 0;

  window.initDateRangeSelector = (selectId, defaultValue, onChange) => {
    rangeCalls.push({ selectId, defaultValue });
    elements.get(selectId).listeners.set('change', onChange);
  };
  window.loadAuthTokensIntoSelect = async (selectId, opts) => {
    tokenCalls.push({ selectId, opts });
    return [{ id: '13', name: 'Token 13' }];
  };

  assert.equal(typeof window.initSavedDateRangeFilter, 'function');
  assert.equal(typeof window.initAuthTokenFilter, 'function');

  window.initSavedDateRangeFilter({
    selectId: 'f_hours',
    defaultValue: 'today',
    restoredValue: '7d',
    onChange() {
      rangeChangeCount += 1;
    }
  });

  const tokens = await window.initAuthTokenFilter({
    selectId: 'f_auth_token',
    value: '13',
    loadOptions: { tokenPrefix: 'Token #' },
    onChange() {
      tokenChangeCount += 1;
    }
  });

  assert.deepEqual(rangeCalls, [{ selectId: 'f_hours', defaultValue: 'today' }]);
  assert.equal(elements.get('f_hours').value, '7d');
  elements.get('f_hours').listeners.get('change')();
  assert.equal(rangeChangeCount, 1);

  assert.deepEqual(tokenCalls, [{ selectId: 'f_auth_token', opts: { tokenPrefix: 'Token #' } }]);
  assert.deepEqual(tokens, [{ id: '13', name: 'Token 13' }]);
  assert.equal(elements.get('f_auth_token').value, '13');
  elements.get('f_auth_token').listeners.get('change')();
  assert.equal(tokenChangeCount, 1);
});

test('ui.js 暴露共享的筛选字段读写 helper', () => {
  const { window, elements } = loadUiCommonHelpers(['f_id', 'f_name', 'f_model', 'f_status']);
  elements.get('f_id').value = ' 42 ';
  elements.get('f_name').value = ' demo-name ';
  elements.get('f_model').value = 'gpt-5';

  assert.equal(typeof window.readFilterControlValues, 'function');
  assert.equal(typeof window.applyFilterControlValues, 'function');

  const values = window.readFilterControlValues({
    channelId: { id: 'f_id', trim: true },
    channelName: { id: 'f_name', trim: true },
    model: 'f_model',
    status: { id: 'f_status', defaultValue: '' }
  });

  assert.deepEqual(JSON.parse(JSON.stringify(values)), {
    channelId: '42',
    channelName: 'demo-name',
    model: 'gpt-5',
    status: ''
  });

  window.applyFilterControlValues(
    {
      channelId: '7',
      channelName: 'new-name',
      model: 'claude',
      status: '200'
    },
    {
      channelId: 'f_id',
      channelName: 'f_name',
      model: 'f_model',
      status: 'f_status'
    }
  );

  assert.equal(elements.get('f_id').value, '7');
  assert.equal(elements.get('f_name').value, 'new-name');
  assert.equal(elements.get('f_model').value, 'claude');
  assert.equal(elements.get('f_status').value, '200');
});

test('logs.js、stats.js 和 trend.js 使用共享筛选输入绑定 helper，logs/stats 使用共享筛选控件初始化与字段读写 helper', () => {
  assert.match(logsSource, /bindFilterApplyInputs\(\{/);
  assert.match(statsSource, /bindFilterApplyInputs\(\{/);
  assert.match(logsSource, /initSavedDateRangeFilter\(\{/);
  assert.match(statsSource, /initSavedDateRangeFilter\(\{/);
  assert.match(trendSource, /initSavedDateRangeFilter\(\{/);
  assert.match(logsSource, /initAuthTokenFilter\(\{/);
  assert.match(statsSource, /initAuthTokenFilter\(\{/);
  assert.match(trendSource, /initAuthTokenFilter\(\{/);
  assert.match(logsSource, /readFilterControlValues\(/);
  assert.match(statsSource, /readFilterControlValues\(/);
  assert.match(logsSource, /applyFilterControlValues\(/);
  // stats/trend 渠道/模型筛选已迁移至 combobox，通过 createSearchableCombobox 初始化
  assert.match(statsSource, /createSearchableCombobox\(/);
  assert.match(trendSource, /createSearchableCombobox\(/);
  assert.match(logsSource, /trim:\s*true/);
  assert.match(statsSource, /trim:\s*true/);
  assert.match(logsSource, /getValues:\s*getLogsFilters|values:\s*getLogsFilters\(\)/);
  assert.match(statsSource, /getValues:\s*getStatsFilters|values:\s*getStatsFilters\(\)/);
});
