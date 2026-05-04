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

function makeMemoryStorage() {
  const map = new Map();
  return {
    getItem(k) { return map.has(k) ? map.get(k) : null; },
    setItem(k, v) { map.set(k, String(v)); },
    removeItem(k) { map.delete(k); },
    clear() { map.clear(); }
  };
}

function loadAutoRefresh(opts = {}) {
  const settingsValue = opts.settingsValue ?? '0';
  const settingsError = opts.settingsError === true;

  const listeners = new Map();
  const intervals = [];
  let nextIntervalId = 1;
  let nextTimeoutId = 1;

  const state = {
    hidden: Boolean(opts.initialHidden),
    modal: null
  };

  const document = {
    body: { dataset: {} },
    readyState: 'complete',
    addEventListener(type, handler) {
      listeners.set(type, handler);
    },
    removeEventListener(type, handler) {
      if (listeners.get(type) === handler) listeners.delete(type);
    },
    querySelector(selector) {
      if (selector === '.modal.show') return state.modal;
      return null;
    }
  };
  Object.defineProperty(document, 'hidden', {
    get: () => state.hidden,
    configurable: true
  });

  const fetchCalls = [];
  const fetchDataWithAuth = async (url) => {
    fetchCalls.push(url);
    if (settingsError) throw new Error('boom');
    if (url === '/admin/settings') {
      return [{ key: 'auto_refresh_interval_seconds', value: settingsValue }];
    }
    return null;
  };

  const sandbox = {
    console,
    document,
    window: {
      fetchDataWithAuth,
      sessionStorage: opts.sessionStorage ?? makeMemoryStorage()
    },
    setInterval(fn, ms) {
      const id = nextIntervalId++;
      intervals.push({ id, fn, ms });
      return id;
    },
    clearInterval(id) {
      const idx = intervals.findIndex(i => i.id === id);
      if (idx >= 0) intervals.splice(idx, 1);
    },
    setTimeout(fn) {
      // 不真正调度，仅返回一个 id
      return nextTimeoutId++;
    },
    clearTimeout() { /* noop */ },
    Date,
    Number,
    JSON,
    Math,
    Promise,
    Array,
    Object,
    String,
    Error
  };

  vm.createContext(sandbox);
  vm.runInContext(extractCommonUiHelpers(uiSource), sandbox);

  return {
    sandbox,
    listeners,
    intervals,
    fetchCalls,
    state,
    createAutoRefresh: sandbox.window.createAutoRefresh
  };
}

test('createAutoRefresh: 配置 = 0 时不启动 interval, 不注册 visibilitychange', async () => {
  const env = loadAutoRefresh({ settingsValue: '0' });
  let loadCalls = 0;
  const ar = env.createAutoRefresh({ load: () => { loadCalls++; } });

  await ar.init();

  assert.equal(env.intervals.length, 0, '不应注册 setInterval');
  assert.equal(env.listeners.has('visibilitychange'), false, '不应注册 visibilitychange');
  assert.equal(loadCalls, 0);
});

test('createAutoRefresh: 配置 > 0 启动 interval, 触发时调用 load', async () => {
  const env = loadAutoRefresh({ settingsValue: '30' });
  let loadCalls = 0;
  const ar = env.createAutoRefresh({ load: () => { loadCalls++; } });

  await ar.init();

  assert.equal(env.intervals.length, 1, '应注册 1 个 setInterval');
  assert.equal(env.intervals[0].ms, 30000, '间隔应为 30000ms');
  assert.equal(env.listeners.has('visibilitychange'), true);

  // 触发 tick
  env.intervals[0].fn();
  assert.equal(loadCalls, 1);

  env.intervals[0].fn();
  assert.equal(loadCalls, 2);
});

test('createAutoRefresh: 配置 > 0 但 document.hidden=true 时跳过 load', async () => {
  const env = loadAutoRefresh({ settingsValue: '60' });
  let loadCalls = 0;
  const ar = env.createAutoRefresh({ load: () => { loadCalls++; } });

  await ar.init();
  assert.equal(env.intervals.length, 1);

  env.state.hidden = true;
  env.intervals[0].fn();
  assert.equal(loadCalls, 0, 'hidden 状态下不应调用 load');

  env.state.hidden = false;
  env.intervals[0].fn();
  assert.equal(loadCalls, 1);
});

test('createAutoRefresh: .modal.show 存在时跳过 load (保护未保存内容)', async () => {
  const env = loadAutoRefresh({ settingsValue: '30' });
  let loadCalls = 0;
  const ar = env.createAutoRefresh({ load: () => { loadCalls++; } });

  await ar.init();

  env.state.modal = { id: 'editModal' };
  env.intervals[0].fn();
  assert.equal(loadCalls, 0, '存在打开的对话框时应跳过');

  env.state.modal = null;
  env.intervals[0].fn();
  assert.equal(loadCalls, 1);
});

test('createAutoRefresh: visibilitychange 隐藏时停止 interval, 恢复时立即 load 并重启', async () => {
  const env = loadAutoRefresh({ settingsValue: '45' });
  let loadCalls = 0;
  const ar = env.createAutoRefresh({ load: () => { loadCalls++; } });

  await ar.init();
  assert.equal(env.intervals.length, 1);
  const handler = env.listeners.get('visibilitychange');
  assert.equal(typeof handler, 'function');

  // 模拟切到后台
  env.state.hidden = true;
  handler();
  assert.equal(env.intervals.length, 0, '隐藏时应清除 interval');
  assert.equal(loadCalls, 0);

  // 模拟切回前台
  env.state.hidden = false;
  handler();
  assert.equal(loadCalls, 1, '恢复时应立即触发 load');
  assert.equal(env.intervals.length, 1, '恢复时应重启 interval');
  assert.equal(env.intervals[0].ms, 45000);
});

test('createAutoRefresh: 拉取设置失败时不启动', async () => {
  const env = loadAutoRefresh({ settingsError: true });
  let loadCalls = 0;
  const ar = env.createAutoRefresh({ load: () => { loadCalls++; } });

  await ar.init();
  assert.equal(env.intervals.length, 0);
  assert.equal(env.listeners.has('visibilitychange'), false);
  assert.equal(loadCalls, 0);
});

test('createAutoRefresh: 缺少 load 回调时返回空 noop', async () => {
  const env = loadAutoRefresh({ settingsValue: '30' });
  const ar = env.createAutoRefresh({});
  await ar.init();
  assert.equal(env.intervals.length, 0);
  assert.equal(env.fetchCalls.length, 0, '无 load 时不应调用接口');
});

test('createAutoRefresh: stop() 清理 interval 与 visibilitychange 监听', async () => {
  const env = loadAutoRefresh({ settingsValue: '30' });
  const ar = env.createAutoRefresh({ load: () => {} });

  await ar.init();
  assert.equal(env.intervals.length, 1);
  assert.equal(env.listeners.has('visibilitychange'), true);

  ar.stop();
  assert.equal(env.intervals.length, 0);
  assert.equal(env.listeners.has('visibilitychange'), false);
});

test('createAutoRefresh: sessionStorage 60s 缓存命中时不重复请求', async () => {
  const sharedStorage = makeMemoryStorage();
  // 第一次：拉取并写入缓存
  {
    const env = loadAutoRefresh({ settingsValue: '30', sessionStorage: sharedStorage });
    const ar = env.createAutoRefresh({ load: () => {} });
    await ar.init();
    assert.equal(env.fetchCalls.length, 1);
  }
  // 第二次：相同进程的另一次 init，缓存仍在 60s 内，应跳过 /admin/settings
  {
    const env = loadAutoRefresh({ settingsValue: '999', sessionStorage: sharedStorage });
    const ar = env.createAutoRefresh({ load: () => {} });
    await ar.init();
    assert.equal(env.fetchCalls.length, 0, '缓存应阻止再次请求');
    assert.equal(env.intervals.length, 1);
    assert.equal(env.intervals[0].ms, 30000, '应使用缓存的 30s 而非新值 999s');
  }
});
