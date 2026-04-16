const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const filterStatePath = path.join(__dirname, 'filter-state.js');
const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');
const statsSource = fs.readFileSync(path.join(__dirname, 'stats.js'), 'utf8');
const trendSource = fs.readFileSync(path.join(__dirname, 'trend.js'), 'utf8');
const logsHtmlSource = fs.readFileSync(path.join(__dirname, '..', '..', 'logs.html'), 'utf8');
const statsHtmlSource = fs.readFileSync(path.join(__dirname, '..', '..', 'stats.html'), 'utf8');
const trendHtmlSource = fs.readFileSync(path.join(__dirname, '..', '..', 'trend.html'), 'utf8');

function loadFilterStateModule() {
  assert.ok(fs.existsSync(filterStatePath), '缺少共享筛选状态模块 filter-state.js');

  const source = fs.readFileSync(filterStatePath, 'utf8');
  const storage = new Map();
  const sandbox = {
    console,
    URLSearchParams,
    window: {
      localStorage: {
        setItem(key, value) {
          storage.set(key, String(value));
        },
        getItem(key) {
          return storage.has(key) ? storage.get(key) : null;
        }
      }
    }
  };

  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);
  return { window: sandbox.window, storage };
}

test('共享筛选状态模块暴露持久化和恢复 helper', () => {
  const { window } = loadFilterStateModule();

  assert.equal(typeof window.FilterState, 'object');
  assert.equal(typeof window.FilterState.save, 'function');
  assert.equal(typeof window.FilterState.load, 'function');
  assert.equal(typeof window.FilterState.restore, 'function');
  assert.equal(typeof window.FilterState.buildParams, 'function');
  assert.equal(typeof window.FilterState.mergeParams, 'function');
  assert.equal(typeof window.FilterState.buildRestoreSearch, 'function');
  assert.equal(typeof window.FilterState.buildURL, 'function');
  assert.equal(typeof window.FilterState.writeHistory, 'function');
});

test('共享筛选状态模块支持 URL 优先、本地存储兜底和别名查询', () => {
  const { window } = loadFilterStateModule();
  const fields = [
    { key: 'range', queryKeys: ['range'], defaultValue: 'today' },
    { key: 'channelName', queryKeys: ['channel_name'], defaultValue: '' },
    { key: 'model', queryKeys: ['model'], defaultValue: '' },
    { key: 'channelType', queryKeys: ['channel_type'], defaultValue: 'all' }
  ];

  const restored = window.FilterState.restore({
    search: '?channel_name=from-url&model=gpt-5',
    savedFilters: {
      range: 'last_month',
      channelName: 'saved-name',
      model: 'saved-model',
      channelType: 'codex'
    },
    fields
  });

  assert.deepEqual(
    JSON.parse(JSON.stringify(restored)),
    {
      range: 'today',
      channelName: 'from-url',
      model: 'gpt-5',
      channelType: 'all'
    }
  );

  const restoredFromSaved = window.FilterState.restore({
    search: '',
    savedFilters: {
      range: 'last_month',
      channelName: 'saved-name',
      model: 'saved-model',
      channelType: 'codex'
    },
    fields
  });

  assert.deepEqual(
    JSON.parse(JSON.stringify(restoredFromSaved)),
    {
      range: 'last_month',
      channelName: 'saved-name',
      model: 'saved-model',
      channelType: 'codex'
    }
  );
});

test('共享筛选状态模块在聚合存储缺失时支持 legacy 键兜底', () => {
  const { window, storage } = loadFilterStateModule();
  storage.set('trend.range', 'last_month');
  storage.set('trend.trendType', 'rpm');
  storage.set('trend.model', 'gpt-5');

  const loaded = window.FilterState.load(
    'trend.filters',
    window.localStorage,
    {
      legacyKeyMap: {
        range: 'trend.range',
        trendType: 'trend.trendType',
        model: 'trend.model'
      }
    }
  );

  assert.deepEqual(
    JSON.parse(JSON.stringify(loaded)),
    {
      range: 'last_month',
      trendType: 'rpm',
      model: 'gpt-5'
    }
  );
});

test('共享筛选状态模块可以按字段定义构建查询参数', () => {
  const { window } = loadFilterStateModule();
  const fields = [
    { key: 'range', queryKeys: ['range'] },
    { key: 'channelName', queryKeys: ['channel_name'] },
    { key: 'model', queryKeys: ['model'] },
    {
      key: 'channelType',
      queryKeys: ['channel_type'],
      includeInQuery(value) {
        return Boolean(value) && value !== 'all';
      }
    }
  ];

  const params = window.FilterState.buildParams(
    {
      range: 'today',
      channelName: 'demo',
      model: '',
      channelType: 'all'
    },
    fields
  );

  assert.equal(params.get('range'), 'today');
  assert.equal(params.get('channel_name'), 'demo');
  assert.equal(params.has('model'), false);
  assert.equal(params.has('channel_type'), false);
});

test('共享筛选状态模块可以在保留无关参数的同时合并并清理筛选别名参数', () => {
  const { window } = loadFilterStateModule();
  const fields = [
    { key: 'range', queryKeys: ['range'], defaultValue: 'today' },
    { key: 'channelName', queryKeys: ['channel_name'], defaultValue: '' },
    { key: 'model', queryKeys: ['model'], defaultValue: '' },
    {
      key: 'channelType',
      queryKeys: ['channel_type'],
      defaultValue: 'all',
      includeInQuery(value) {
        return Boolean(value) && value !== 'all';
      }
    }
  ];

  const params = window.FilterState.mergeParams(
    '?page=2&channel_name=old-name&model=gpt-4&channel_type=all&keep=1',
    {
      range: 'today',
      channelName: 'new-name',
      model: '',
      channelType: 'all'
    },
    fields
  );

  assert.equal(params.get('page'), '2');
  assert.equal(params.get('keep'), '1');
  assert.equal(params.get('range'), 'today');
  assert.equal(params.get('channel_name'), 'new-name');
  assert.equal(params.has('model'), false);
  assert.equal(params.has('channel_type'), false);
});

test('共享筛选状态模块可以为无 URL 参数场景构建保存筛选条件回填 search', () => {
  const { window } = loadFilterStateModule();
  const fields = [
    { key: 'range', queryKeys: ['range'], defaultValue: 'today' },
    { key: 'channelName', queryKeys: ['channel_name'], defaultValue: '' },
    { key: 'model', queryKeys: ['model'], defaultValue: '' },
    {
      key: 'channelType',
      queryKeys: ['channel_type'],
      defaultValue: 'all',
      includeInQuery(value) {
        return Boolean(value) && value !== 'all';
      }
    }
  ];

  assert.equal(
    window.FilterState.buildRestoreSearch(
      '',
      {
        range: 'today',
        channelName: 'saved-name',
        model: 'gpt-5',
        channelType: 'all'
      },
      fields
    ),
    '?range=today&channel_name=saved-name&model=gpt-5'
  );

  assert.equal(
    window.FilterState.buildRestoreSearch(
      '?range=today',
      {
        range: 'today',
        channelName: 'saved-name'
      },
      fields
    ),
    ''
  );

  assert.equal(window.FilterState.buildRestoreSearch('', null, fields), '');
});

test('共享筛选状态模块可以构建 merge/replace 两种 URL 写回结果', () => {
  const { window } = loadFilterStateModule();
  const fields = [
    { key: 'range', queryKeys: ['range'] },
    { key: 'channelName', queryKeys: ['channel_name'] },
    {
      key: 'channelType',
      queryKeys: ['channel_type'],
      includeInQuery(value) {
        return Boolean(value) && value !== 'all';
      }
    }
  ];

  assert.equal(
    window.FilterState.buildURL({
      search: '?page=2&channel_name=legacy&keep=1',
      pathname: '/logs',
      values: {
        range: 'today',
        channelName: 'new-name',
        channelType: 'all'
      },
      fields,
      preserveExistingParams: true
    }),
    '?page=2&keep=1&range=today&channel_name=new-name'
  );

  assert.equal(
    window.FilterState.buildURL({
      pathname: '/trend',
      values: {
        range: 'last_month',
        channelName: ''
      },
      fields
    }),
    '?range=last_month'
  );

  assert.equal(
    window.FilterState.buildURL({
      pathname: '/trend',
      values: {
        range: '',
        channelName: ''
      },
      fields
    }),
    '/trend'
  );
});

test('共享筛选状态模块可以按指定 history 方法写回 URL', () => {
  const { window } = loadFilterStateModule();
  const calls = [];
  const history = {
    pushState(...args) {
      calls.push(['pushState', ...args]);
    },
    replaceState(...args) {
      calls.push(['replaceState', ...args]);
    }
  };
  const fields = [
    { key: 'range', queryKeys: ['range'] },
    { key: 'channelName', queryKeys: ['channel_name'] }
  ];

  const pushedURL = window.FilterState.writeHistory({
    search: '?page=3&channel_name=legacy',
    pathname: '/logs',
    values: {
      range: 'today',
      channelName: 'demo'
    },
    fields,
    preserveExistingParams: true,
    history
  });

  const replacedURL = window.FilterState.writeHistory({
    pathname: '/trend',
    values: {
      range: 'last_month',
      channelName: ''
    },
    fields,
    history,
    historyMethod: 'replaceState'
  });

  assert.equal(pushedURL, '?page=3&range=today&channel_name=demo');
  assert.equal(replacedURL, '?range=last_month');
  assert.deepEqual(calls, [
    ['pushState', null, '', '?page=3&range=today&channel_name=demo'],
    ['replaceState', null, '', '?range=last_month']
  ]);
});

test('logs.js、stats.js 和 trend.js 接入共享 helper，而不是各自复制 save/load/history 逻辑', () => {
  assert.match(logsSource, /FilterState\.(save|load|restore|buildParams)/);
  assert.match(statsSource, /FilterState\.(save|load|restore|buildParams)/);
  assert.match(trendSource, /FilterState\.(save|load|restore|buildParams)/);
  assert.doesNotMatch(logsSource, /function saveLogsFilters/);
  assert.doesNotMatch(logsSource, /function loadLogsFilters/);
  assert.doesNotMatch(statsSource, /function saveStatsFilters/);
  assert.doesNotMatch(statsSource, /function loadStatsFilters/);
  assert.match(logsSource, /persistFilterState\(/);
  assert.match(statsSource, /persistFilterState\(/);
  assert.match(trendSource, /persistFilterState\(/);
  assert.doesNotMatch(logsSource, /history\.pushState/);
  assert.doesNotMatch(logsSource, /history\.replaceState/);
  assert.doesNotMatch(statsSource, /history\.pushState/);
  assert.doesNotMatch(statsSource, /history\.replaceState/);
  assert.doesNotMatch(trendSource, /history\.replaceState/);
  assert.doesNotMatch(trendSource, /localStorage\.setItem\('trend\.(channelType|range|trendType|model|authToken|channelId|channelName)'/);
  assert.doesNotMatch(trendSource, /localStorage\.getItem\('trend\.(channelType|range|trendType|model|authToken|channelId|channelName)'/);
});

test('logs.html、stats.html 和 trend.html 在页面脚本前加载共享筛选状态 helper', () => {
  assert.match(
    logsHtmlSource,
    /filter-state\.js\?v=__VERSION__[\s\S]*logs\.js\?v=__VERSION__/
  );
  assert.match(
    statsHtmlSource,
    /filter-state\.js\?v=__VERSION__[\s\S]*stats\.js\?v=__VERSION__/
  );
  assert.match(
    trendHtmlSource,
    /filter-state\.js\?v=__VERSION__[\s\S]*trend\.js\?v=__VERSION__/
  );
});

test('logs.js 的活跃请求筛选对渠道名和模型使用精确匹配', () => {
  assert.match(logsSource, /const name = \(typeof req\.channel_name === 'string' \? req\.channel_name : ''\)\.toLowerCase\(\);/);
  assert.match(logsSource, /if \(name !== channelName\) return false;/);
  assert.match(logsSource, /if \(\(req\.model \|\| ''\) !== model\) return false;/);
  assert.doesNotMatch(logsSource, /name\.includes\(channelName\)/);
});
