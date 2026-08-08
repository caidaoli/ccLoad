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

  setGlobal('window', {
    location: { pathname: '/', search: '', href: '' },
    addEventListener: noop,
    dispatchEvent: noop,
    matchMedia: () => ({ matches: false, addEventListener: noop })
  });
  setGlobal('localStorage', { getItem: () => null, setItem: noop, removeItem: noop });
  setGlobal('document', {
    addEventListener: noop,
    querySelectorAll: () => [],
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

test('model filter options contain request models but not redirected models', async () => {
  const previousGlobals = new Map();
  const setGlobal = (key, value) => {
    previousGlobals.set(key, Object.getOwnPropertyDescriptor(global, key));
    Object.defineProperty(global, key, { configurable: true, writable: true, value });
  };

  const windowListeners = {};
  const tbody = {
    innerHTML: '',
    appendChild() {},
    closest: () => null,
    insertBefore() {},
    querySelector: () => null,
    querySelectorAll: () => []
  };
  const hoursInput = { value: 'today' };

  setGlobal('window', {
    t: (key) => key,
    initPageBootstrap() {},
    addEventListener: (type, handler) => {
      windowListeners[type] = handler;
    },
    FilterState: {
      load: () => ({ range: 'today' }),
      restore: () => ({
        range: 'today',
        authToken: '',
        model: '',
        channelName: '',
        logSource: 'proxy',
        status: ''
      })
    },
    FilterQuery: {
      buildRequestParams: (_values, _fields, options) => new URLSearchParams(options.baseParams)
    },
    loadAuthTokensIntoSelect: async () => [],
    applyFilterControlValues() {},
    readFilterControlValues: () => ({ range: 'today', status: '', authToken: '' }),
    getDurationTimingColor: () => '',
    isAPITokenRole: () => false
  });
  setGlobal('document', {
    addEventListener() {},
    getElementById: (id) => {
      if (id === 'tbody') return tbody;
      if (id === 'f_hours') return hoursInput;
      return null;
    },
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
    return {
      success: true,
      count: 1,
      data: [{
        time: Date.now(),
        model: 'requested-model',
        actual_model: 'redirected-model',
        status_code: 200,
        duration: 0,
        log_source: 'proxy'
      }]
    };
  });

  try {
    delete require.cache[require.resolve('./logs.js')];
    require('./logs.js');

    await windowListeners.pageshow({ persisted: true });
    await new Promise(resolve => setImmediate(resolve));

    assert.deepEqual(global.window.availableLogsModels, ['requested-model']);
  } finally {
    delete require.cache[require.resolve('./logs.js')];
    for (const [key, descriptor] of previousGlobals) {
      if (descriptor === undefined) delete global[key];
      else Object.defineProperty(global, key, descriptor);
    }
  }
});
