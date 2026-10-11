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
    showChannels = false,
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
    t: (key, params) => {
      if (key === 'logs.sourceCheckinBadge') return '签到';
      return params ? `${key} ${Object.values(params).join(' ')}` : key;
    },
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
    isAPITokenRole: () => isTokenRole,
    shouldHideChannels: () => isTokenRole && !showChannels
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
  setGlobal('getCostDisplayInfo', (standardCost, effectiveCost) => ({standardCost, effectiveCost, hasMultiplier: false}));
  setGlobal('formatCost', value => '$' + value);
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

test('channelless count_tokens logs link to their debug-log entry', async () => {
  await withLoadedLogsPage({
    logSource: 'count_tokens',
    entries: [{
      id: 42,
      time: Date.now(),
      model: 'claude-sonnet-5-5',
      status_code: 200,
      duration: 0.2,
      log_source: 'count_tokens'
    }]
  }, ({ tbody }) => {
    assert.match(tbody.innerHTML, /debug-log-link has-upstream-detail[^>]*data-log-id="42"/);
  });
});

test('token channel visibility also controls actual model text and hover', async () => {
  for (const [isTokenRole, showChannels] of [[true, false], [true, true], [false, false]]) {
    for (const modelField of ['actual_model', 'response_model']) {
      await withLoadedLogsPage({
        isTokenRole,
        showChannels,
        entries: [{
          time: Date.now(), model: 'requested-model', [modelField]: 'private-upstream-model',
          thinking_effort: 'high', reasoning_tokens: 123,
          status_code: 200, duration: 0, log_source: 'proxy'
        }]
      }, ({ tbody }) => {
        const html = tbody.innerHTML;
        assert.match(html, /requested-model/);
        if (isTokenRole && !showChannels) {
          assert.doesNotMatch(html, /private-upstream-model|model-actual|logs\.titleActualModel/);
        } else {
          assert.match(html, /logs\.titleActualModel private-upstream-model/);
        }
      });
    }
  }
});

test('small request costs keep significant digits instead of rounding to $0.000', () => {
  const previousGlobals = new Map();
  const setGlobal = (key, value) => {
    previousGlobals.set(key, Object.getOwnPropertyDescriptor(global, key));
    Object.defineProperty(global, key, { configurable: true, writable: true, value });
  };
  const noop = () => {};
  const element = () => ({
    style: { setProperty: noop },
    dataset: {},
    classList: { add: noop, remove: noop, toggle: noop, contains: () => false },
    appendChild: noop,
    setAttribute: noop,
    getAttribute: () => null,
    addEventListener: noop,
    querySelectorAll: () => [],
    querySelector: () => null
  });
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
    createElement: element,
    body: element(),
    documentElement: element()
  });
  setGlobal('CustomEvent', function CustomEvent() {});

  try {
    delete require.cache[require.resolve('./ui.js')];
    require('./ui.js');
    const { formatCost } = global.window;

    assert.equal(formatCost(0.0002), '$0.0002');
    assert.equal(formatCost(0.00012345), '$0.00012');
    assert.equal(formatCost(0.005), '$0.005');
    assert.equal(formatCost(0.0000004), '<$0.000001');
    assert.equal(formatCost(1.23456), '$1.235');
    assert.equal(formatCost(0), '$0');
    assert.equal(formatCost(0.0002, 2), '$0.00');
  } finally {
    delete require.cache[require.resolve('./ui.js')];
    for (const [key, descriptor] of previousGlobals) {
      if (descriptor === undefined) delete global[key];
      else Object.defineProperty(global, key, descriptor);
    }
  }
});

test('reasoning logs show a level change on the model badge and the judgment in the message', async () => {
  const business = (client, sent, extra = {}) => ({time: Date.now(), model: 'business', thinking_effort: sent, status_code: 200, duration: 0, log_source: 'proxy',
    reasoning_decision: {status: 'decided', trigger: 'user_input', client_effort: client, suggested_effort: sent, confidence: 0.6, ...extra}});
  for (const isTokenRole of [false, true]) {
    await withLoadedLogsPage({isTokenRole, entries: [
      {...business('xhigh', 'low', {decision_channel_id: 1507, decision_model: 'private-judge', decision_id: 'corr-1', context_truncated: true}), reasoning_tokens: 72},
      business('', 'high'),
      business('max', 'xhigh'),
      business('medium', 'medium'),
      {time: Date.now(), model: 'judge', status_code: 200, duration: 0, log_source: 'proxy', client_protocol: 'system_one',
        reasoning_decision: {status: 'decided', suggested_effort: 'xhigh', confidence: 0.554, duration_ms: 12, cost_unknown: true}},
      {time: Date.now(), model: 'judge', status_code: 504, duration: 0, log_source: 'proxy', client_protocol: 'system_one',
        reasoning_decision: {status: 'fallback', reason: 'timeout'}}]}, ({tbody}) => {
      const badges = [...tbody.innerHTML.matchAll(/<sup[^>]*>([^<]*)<\/sup>/g)].map(match => match[1]);
      assert.deepEqual(badges, ['xhigh→low 72', 'high', 'max→xhigh', 'medium']);
      assert.doesNotMatch(tbody.innerHTML, /logs\.reasoning\.changed/);
      assert.match(tbody.innerHTML, /logs.reasoning.judged xhigh(logs.reasoning.confidence 0.55)?/);
      assert.match(tbody.innerHTML, /logs.reasoning.failed timeout/);
      assert.match(tbody.innerHTML, /logs.costUnknown/);
      assert.doesNotMatch(tbody.innerHTML, /private-judge|1507|corr-1|truncated|user_input/);
    });
  }
});

test('unknown decision cost does not replace known business request cost', async () => {
  await withLoadedLogsPage({entries: [{time: Date.now(), model: 'business', status_code: 200, duration: 0, cost: 0.25, log_source: 'proxy', reasoning_decision: {cost_unknown: true}}]}, ({tbody}) => {
    assert.doesNotMatch(tbody.innerHTML, /logs.costUnknown/);
    assert.match(tbody.innerHTML, /0.25/);
  });
});
