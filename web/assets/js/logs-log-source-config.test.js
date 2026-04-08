const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');
const logsCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'logs.css'), 'utf8');

function createHarness(values, initialValue = 'all') {
  const queue = [...values];
  const fetchCalls = [];
  const group = { hidden: false };
  const select = {
    value: initialValue,
    closest(selector) {
      if (selector === '.filter-group') {
        return group;
      }
      return null;
    }
  };

  const sandbox = {
    console,
    URLSearchParams,
    location: {
      search: '',
      pathname: '/web/logs.html'
    },
    fetchDataWithAuth: async (url) => {
      fetchCalls.push(url);
      if (queue.length === 0) {
        throw new Error('no mock value');
      }
      return { value: queue.shift() };
    },
    document: {
      getElementById(id) {
        if (id === 'f_log_source') return select;
        return null;
      }
    },
    window: {
      initPageBootstrap() {},
      addEventListener() {},
      applyFilterControlValues() {},
      initSavedDateRangeFilter() {},
      initAuthTokenFilter: async () => [],
      bindFilterApplyInputs() {},
      initChannelTypeFilter: async () => {},
      persistFilterState() {},
      FilterState: {
        load() {
          return null;
        },
        restore() {
          return {};
        }
      },
      readFilterControlValues() {
        return {};
      },
      FilterQuery: {
        buildRequestParams() {
          return {};
        }
      }
    }
  };

  vm.createContext(sandbox);
  vm.runInContext(`${logsSource}
this.__logsLogSourceConfigTest = {
  syncLogSourceVisibility
};`, sandbox);

  return {
    group,
    select,
    fetchCalls,
    api: sandbox.__logsLogSourceConfigTest
  };
}

test('日志来源筛选在定时检测关闭时隐藏并回退到 proxy', async () => {
  const harness = createHarness(['0'], 'all');

  const visible = await harness.api.syncLogSourceVisibility();

  assert.equal(visible, false);
  assert.equal(harness.group.hidden, true);
  assert.equal(harness.select.value, 'proxy');
  assert.deepEqual(harness.fetchCalls, ['/admin/settings/channel_check_interval_hours']);
});

test('日志来源筛选在定时检测开启时保持可见且保留当前值', async () => {
  const harness = createHarness(['2'], 'all');

  const visible = await harness.api.syncLogSourceVisibility();

  assert.equal(visible, true);
  assert.equal(harness.group.hidden, false);
  assert.equal(harness.select.value, 'all');
});

test('日志来源筛选 hidden 属性有显式样式兜底，避免被 flex 布局顶掉', () => {
  assert.match(logsCss, /\.logs-filter-group\[hidden\]\s*\{\s*display:\s*none\s*!important;/);
});
