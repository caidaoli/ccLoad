const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const filterStateSource = fs.readFileSync(path.join(__dirname, 'filter-state.js'), 'utf8');
const trendSource = `${fs.readFileSync(path.join(__dirname, 'trend.js'), 'utf8')}\nthis.__trendTest = { loadSavedTrendFilters };`;

function createStorage(entries = {}) {
  const storage = new Map(Object.entries(entries));
  return {
    getItem(key) {
      return storage.has(key) ? storage.get(key) : null;
    },
    setItem(key, value) {
      storage.set(key, String(value));
    }
  };
}

function loadTrendStateHarness(entries = {}) {
  const sandbox = {
    console,
    URLSearchParams,
    TextEncoder,
    setInterval() {},
    clearInterval() {},
    history: {
      replaceState() {}
    },
    location: {
      search: '',
      pathname: '/web/trend.html'
    },
    document: {
      addEventListener() {},
      getElementById() {
        return null;
      },
      querySelector() {
        return null;
      },
      querySelectorAll() {
        return [];
      }
    },
    debounce(fn) {
      return fn;
    },
    fetchDataWithAuth: async () => [],
    fetchAPIWithAuthRaw: async () => ({
      payload: { success: true, data: [] },
      res: {
        headers: {
          get() {
            return null;
          }
        }
      }
    }),
    echarts: {
      init() {
        return {
          resize() {},
          setOption() {},
          dispose() {}
        };
      }
    },
    ResizeObserver: class {
      observe() {}
      disconnect() {}
    }
  };

  const localStorage = createStorage(entries);
  sandbox.window = {
    t(key) {
      return key;
    },
    localStorage,
    initPageBootstrap() {},
    getDateRangePresets() {
      return [{ value: 'today' }, { value: 'last_month' }];
    },
    getRangeLabel(value) {
      return value;
    }
  };
  sandbox.localStorage = localStorage;

  vm.createContext(sandbox);
  vm.runInContext(filterStateSource, sandbox);
  vm.runInContext(trendSource, sandbox);
  return sandbox.__trendTest;
}

test('trend 筛选状态优先读取新的聚合存储格式', () => {
  const { loadSavedTrendFilters } = loadTrendStateHarness({
    'trend.filters': JSON.stringify({
      range: 'last_month',
      trendType: 'rpm',
      model: 'gpt-5',
      authToken: '9',
      channelType: 'codex',
      channelId: '12',
      channelName: 'demo'
    }),
    'trend.range': 'today',
    'trend.trendType': 'count'
  });

  assert.deepEqual(
    JSON.parse(JSON.stringify(loadSavedTrendFilters())),
    {
      range: 'last_month',
      trendType: 'rpm',
      model: 'gpt-5',
      authToken: '9',
      channelType: 'codex',
      channelId: '12',
      channelName: 'demo'
    }
  );
});

test('trend 筛选状态在聚合存储缺失时回退旧的分散 localStorage 键', () => {
  const { loadSavedTrendFilters } = loadTrendStateHarness({
    'trend.range': 'last_month',
    'trend.trendType': 'rpm',
    'trend.model': 'gpt-5',
    'trend.authToken': '9',
    'trend.channelType': 'codex',
    'trend.channelId': '12',
    'trend.channelName': 'demo'
  });

  assert.deepEqual(
    JSON.parse(JSON.stringify(loadSavedTrendFilters())),
    {
      range: 'last_month',
      trendType: 'rpm',
      model: 'gpt-5',
      authToken: '9',
      channelType: 'codex',
      channelId: '12',
      channelName: 'demo'
    }
  );
});
