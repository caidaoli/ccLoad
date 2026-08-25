const test = require('node:test');
const assert = require('node:assert/strict');

test('shared clipboard copy preserves click activation when native Clipboard API is blocked', async () => {
  const previousGlobals = new Map();
  const setGlobal = (key, value) => {
    previousGlobals.set(key, Object.getOwnPropertyDescriptor(global, key));
    Object.defineProperty(global, key, { configurable: true, writable: true, value });
  };
  const noop = () => {};
  const classList = { add: noop, remove: noop, toggle: noop, contains: () => false };
  const element = () => ({
    style: { setProperty: noop },
    dataset: {},
    classList,
    appendChild: noop,
    removeChild: noop,
    replaceChildren: noop,
    setAttribute: noop,
    getAttribute: () => null,
    addEventListener: noop,
    querySelectorAll: () => [],
    querySelector: () => null,
    closest: () => null
  });
  let userActivation = true;
  let selectedText = '';
  const body = element();
  let copyHost = null;
  const modal = {
    ...element(),
    appendChild() { copyHost = 'dialog'; },
    removeChild: noop
  };

  setGlobal('window', {
    location: { pathname: '/', search: '', href: '' },
    addEventListener: noop,
    dispatchEvent: noop,
    matchMedia: () => ({ matches: false, addEventListener: noop })
  });
  setGlobal('localStorage', { getItem: () => null, setItem: noop, removeItem: noop });
  setGlobal('document', {
    addEventListener: noop,
    querySelectorAll: selector => selector === 'dialog[open]' ? [modal] : [],
    querySelector: () => null,
    getElementById: () => null,
    createElement: () => ({
      ...element(),
      value: '',
      select() { selectedText = this.value; }
    }),
    execCommand: (command) => command === 'copy' && userActivation,
    body,
    documentElement: element()
  });
  setGlobal('navigator', {
    clipboard: {
      writeText: async () => {
        userActivation = false;
        throw new Error('clipboard permission denied');
      }
    }
  });
  setGlobal('CustomEvent', function CustomEvent() {});

  try {
    delete require.cache[require.resolve('./ui.js')];
    require('./ui.js');

    await global.window.copyToClipboard('debug request');

    assert.equal(selectedText, 'debug request');
    assert.equal(copyHost, 'dialog');
  } finally {
    delete require.cache[require.resolve('./ui.js')];
    for (const [key, descriptor] of previousGlobals) {
      if (descriptor === undefined) delete global[key];
      else Object.defineProperty(global, key, descriptor);
    }
  }
});

test('debug log copy works when the native Clipboard API is unavailable', async () => {
  const previousWindow = global.window;
  const previousDocument = global.document;
  const previousLocalStorage = Object.getOwnPropertyDescriptor(global, 'localStorage');
  const previousNavigator = Object.getOwnPropertyDescriptor(global, 'navigator');
  const previousSetTimeout = global.setTimeout;
  const listeners = {};
  let copiedText = '';

  const copyButton = {
    dataset: { copyTarget: 'debugReqRaw' },
    textContent: 'Copy',
    classList: {
      add() {},
      remove() {}
    }
  };
  const rawRequest = {
    _rawText: 'POST /v1/messages\n\n{"model":"test"}',
    textContent: 'rendered request'
  };

  global.window = {
    t: (key) => key,
    initPageBootstrap() {},
    addEventListener() {},
    copyToClipboard: async (text) => {
      copiedText = text;
    }
  };
  global.document = {
    addEventListener: (type, handler) => {
      listeners[type] = handler;
    },
    getElementById: (id) => id === 'debugReqRaw' ? rawRequest : null,
    querySelectorAll: () => []
  };
  Object.defineProperty(global, 'localStorage', {
    configurable: true,
    value: { getItem: () => null }
  });
  Object.defineProperty(global, 'navigator', {
    configurable: true,
    value: {}
  });
  global.setTimeout = (handler) => {
    handler();
    return 0;
  };

  try {
    delete require.cache[require.resolve('./logs.js')];
    require('./logs.js');

    listeners.click({
      target: {
        closest: (selector) => selector === '#debugLogModal .upstream-copy-btn' ? copyButton : null
      }
    });
    await Promise.resolve();

    assert.equal(copiedText, rawRequest._rawText);
  } finally {
    delete require.cache[require.resolve('./logs.js')];
    if (previousWindow === undefined) delete global.window;
    else global.window = previousWindow;
    if (previousDocument === undefined) delete global.document;
    else global.document = previousDocument;
    if (previousLocalStorage === undefined) delete global.localStorage;
    else Object.defineProperty(global, 'localStorage', previousLocalStorage);
    if (previousNavigator === undefined) delete global.navigator;
    else Object.defineProperty(global, 'navigator', previousNavigator);
    global.setTimeout = previousSetTimeout;
  }
});

async function withLoadedLogsPage(options, assertions) {
  const previousGlobals = new Map();
  const setGlobal = (key, value) => {
    previousGlobals.set(key, Object.getOwnPropertyDescriptor(global, key));
    Object.defineProperty(global, key, { configurable: true, writable: true, value });
  };
  const {
    isTokenRole = false,
    logSource = 'proxy',
    entries = []
  } = options;
  const windowListeners = {};
  const requestedURLs = [];
  const sourceGroup = { hidden: false };
  const sourceSelect = {
    value: logSource,
    parentElement: sourceGroup,
    closest: (selector) => selector === '.filter-group' ? sourceGroup : null
  };
  const tbody = {
    innerHTML: '',
    appendChild() {},
    closest: () => null,
    insertBefore() {},
    querySelector: () => null,
    querySelectorAll: () => []
  };
  const elements = {
    tbody,
    f_hours: { value: 'today' },
    f_log_source: sourceSelect,
    f_auth_token: { value: '' }
  };
  const restoredFilters = {
    range: 'today',
    authToken: '',
    model: '',
    channelName: '',
    logSource,
    status: ''
  };

  setGlobal('window', {
    t: (key) => key === 'logs.sourceCheckinBadge' ? '签到' : key,
    initPageBootstrap() {},
    addEventListener: (type, handler) => {
      windowListeners[type] = handler;
    },
    FilterState: {
      load: () => restoredFilters,
      restore: () => ({ ...restoredFilters })
    },
    FilterQuery: {
      buildRequestParams: (values, _fields, options) => {
        const params = new URLSearchParams(options.baseParams);
        params.set('log_source', values.logSource);
        return params;
      }
    },
    loadAuthTokensIntoSelect: async () => [],
    applyFilterControlValues: (values) => {
      sourceSelect.value = values.logSource;
    },
    readFilterControlValues: () => ({ range: 'today', clientProtocol: '', authToken: '' }),
    getDurationTimingColor: () => '',
    isAPITokenRole: () => isTokenRole
  });
  setGlobal('document', {
    addEventListener() {},
    getElementById: (id) => elements[id] || null,
    querySelector: () => null,
    querySelectorAll: () => []
  });
  setGlobal('localStorage', { getItem: () => null });
  setGlobal('location', { search: '', pathname: '/web/logs.html' });
  setGlobal('TemplateEngine', { render: () => null });
  setGlobal('escapeHtml', (value) => String(value ?? ''));
  setGlobal('calculateTokenSpeed', () => null);
  setGlobal('fetchDataWithAuth', async (url) => {
    if (url.startsWith('/dashboard/models?')) return { models: [], channels: [] };
    throw new Error(`unexpected fetchDataWithAuth call: ${url}`);
  });
  setGlobal('fetchAPIWithAuth', async (url) => {
    if (!url.startsWith('/dashboard/logs?')) {
      throw new Error(`unexpected fetchAPIWithAuth call: ${url}`);
    }
    requestedURLs.push(url);
    return { success: true, count: entries.length, data: entries };
  });

  try {
    delete require.cache[require.resolve('./logs.js')];
    require('./logs.js');
    await windowListeners.pageshow({ persisted: true });
    await new Promise(resolve => setImmediate(resolve));
    await assertions({ requestedURLs, sourceGroup, sourceSelect, tbody });
  } finally {
    delete require.cache[require.resolve('./logs.js')];
    for (const [key, descriptor] of previousGlobals) {
      if (descriptor === undefined) delete global[key];
      else Object.defineProperty(global, key, descriptor);
    }
  }
}

test('logs filter HTML exposes one standalone checkin source option', () => {
  const previousWindow = global.window;
  const previousDocument = global.document;
  global.window = {};
  global.document = { querySelectorAll: () => [] };

  try {
    delete require.cache[require.resolve('./page-filters.js')];
    require('./page-filters.js');
    const html = global.window.PageFilters.renderLayout('logs');
    const options = [...html.matchAll(/<option value="([^"]+)"[^>]*>([^<]+)<\/option>/g)]
      .map(([, value, label]) => ({ value, label: label.trim() }));

    assert.deepEqual(options.filter(option => option.value === 'checkin'), [
      { value: 'checkin', label: '签到' }
    ]);
  } finally {
    delete require.cache[require.resolve('./page-filters.js')];
    if (previousWindow === undefined) delete global.window;
    else global.window = previousWindow;
    if (previousDocument === undefined) delete global.document;
    else global.document = previousDocument;
  }
});

test('checkin audit logs render a visible localized source label', async () => {
  await withLoadedLogsPage({
    entries: [{
      time: Date.now(),
      model: '',
      status_code: 200,
      duration: 0,
      log_source: 'checkin',
      message: 'audit result'
    }]
  }, ({ tbody }) => {
    assert.match(tbody.innerHTML, />签到</);
  });
});

test('admins can select checkin logs when scheduled model detection is disabled', async () => {
  await withLoadedLogsPage({ logSource: 'checkin' }, ({ sourceGroup, sourceSelect }) => {
    assert.equal(sourceGroup.hidden, false);
    assert.equal(sourceSelect.value, 'checkin');
  });
});

test('API token sessions cannot select management log sources', async () => {
  await withLoadedLogsPage({ isTokenRole: true, logSource: 'checkin' }, ({ sourceGroup, sourceSelect }) => {
    assert.equal(sourceGroup.hidden, true);
    assert.equal(sourceSelect.value, 'proxy');
  });
});

test('checkin log filter is sent unchanged in the dashboard request', async () => {
  await withLoadedLogsPage({ logSource: 'checkin' }, ({ requestedURLs }) => {
    assert.equal(requestedURLs.length, 1);
    const requestURL = new URL(requestedURLs[0], 'http://localhost');
    assert.deepEqual(requestURL.searchParams.getAll('log_source'), ['checkin']);
  });
});

test('model filter options contain request models but not redirected models', async () => {
  await withLoadedLogsPage({
    entries: [{
      time: Date.now(),
      model: 'requested-model',
      actual_model: 'redirected-model',
      status_code: 200,
      duration: 0,
      log_source: 'proxy'
    }]
  }, () => {
    assert.deepEqual(global.window.availableLogsModels, ['requested-model']);
  });
});
