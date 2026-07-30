const test = require('node:test');
const assert = require('node:assert/strict');

const { selectFirstEnabledInlineKey } = require('./channels-keys.js');

function installFetchModelsGlobals({ rows, states, onFetch, onError, onWarning, channelType = 'openai' }) {
	const globals = {
		window: {
			t: key => key,
      showError: onError,
      showWarning: onWarning
    },
    document: {
      querySelector: () => ({ value: channelType })
    },
    getValidInlineURLs: () => ['https://upstream.test'],
    getInlineKeyRows: () => rows,
    currentChannelKeyCooldowns: states,
    selectFirstEnabledInlineKey,
    fetchAPIWithAuth: onFetch,
    alert: onError,
    console: { ...console, error: () => {} }
  };
  const previous = new Map();
  for (const [name, value] of Object.entries(globals)) {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  }
  return () => {
    for (const [name, descriptor] of previous) {
      if (descriptor) Object.defineProperty(global, name, descriptor);
      else delete global[name];
    }
  };
}

function loadChannelsModals() {
  const modulePath = require.resolve('./channels-modals.js');
  delete require.cache[modulePath];
  return require(modulePath);
}

function loadFetchModelsFromAPI() {
  return loadChannelsModals().fetchModelsFromAPI;
}

function installCommonModelsGlobals(initialRows = []) {
  const rows = initialRows.map(row => ({ ...row }));
  const notifications = [];
  let dirty = false;
  let renders = 0;
  const globals = {
    window: {
      t: (key, params) => ({ key, params }),
      showSuccess: message => notifications.push({ type: 'success', message }),
      showWarning: message => notifications.push({ type: 'warning', message })
    },
    redirectTableData: rows,
    renderRedirectTable: () => { renders++; },
    markChannelFormDirty: () => { dirty = true; },
    alert: () => {}
  };
  const previous = new Map();
  for (const [name, value] of Object.entries(globals)) {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  }
  return {
    rows,
    notifications,
    get dirty() { return dirty; },
    get renders() { return renders; },
    restore() {
      for (const [name, descriptor] of previous) {
        if (descriptor) Object.defineProperty(global, name, descriptor);
        else delete global[name];
      }
    }
  };
}

function installWebsocketProbeGlobals({
  supported,
  initialChecked,
  urls = ['https://upstream.test'],
  urlConfigs = urls.map(url => ({ url, exact: false, protocols: [] })),
  rows = [{ api_key: 'sk-probe' }],
  urlStats = {},
  keyStates = []
}) {
  const checkbox = { checked: initialChecked };
  const button = { disabled: false, innerHTML: '检测' };
  const proxyInput = { value: 'socks5://proxy.test:1080' };
  const notifications = [];
  const requests = [];
  let dirty = false;
	const globals = {
		window: {
			t: key => key,
      showNotification: (message, type) => notifications.push({ message, type }),
      collectCustomRulesForSubmit: () => ({
        headers: [{ action: 'override', name: 'X-Probe', value: '1' }]
      })
    },
    document: {
      querySelector: () => ({ value: 'codex' }),
      getElementById: id => ({
        channelWebsockets: checkbox,
        channelProxyURL: proxyInput
      })[id] || null
    },
    getValidInlineURLConfigs: () => urlConfigs,
    runtimeInlineURL: entry => entry.exact ? `${entry.url}#` : entry.url,
    getInlineKeyRows: () => rows,
    urlStatsMap: urlStats,
    currentChannelKeyCooldowns: keyStates,
    selectFirstEnabledInlineKey,
    fetchDataWithAuth: async (url, options) => {
      requests.push({ url, body: JSON.parse(options.body) });
      return { supported, error: supported ? '' : '426 Upgrade Required' };
    },
    markChannelFormDirty: () => { dirty = true; },
    alert: () => {}
  };
  const previous = new Map();
  for (const [name, value] of Object.entries(globals)) {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  }
  return {
    button,
    checkbox,
    notifications,
    get request() { return requests.at(-1) || null; },
    requests,
    get dirty() { return dirty; },
    restore() {
      for (const [name, descriptor] of previous) {
        if (descriptor) Object.defineProperty(global, name, descriptor);
        else delete global[name];
      }
    }
  };
}

test('WebSocket probe skips disabled URLs and keys and checks every enabled URL', async () => {
  const fixture = installWebsocketProbeGlobals({
    supported: true,
    initialChecked: false,
    urls: [
      'https://disabled-upstream.test',
      'https://anthropic-only.test',
      'https://enabled-a.test',
      'https://enabled-b.test'
    ],
    urlConfigs: [
      { url: 'https://disabled-upstream.test', exact: false, protocols: ['codex'] },
      { url: 'https://anthropic-only.test', exact: false, protocols: ['anthropic'] },
      { url: 'https://enabled-a.test', exact: false, protocols: ['codex'] },
      { url: 'https://enabled-b.test', exact: false, protocols: [] }
    ],
    rows: [
      { api_key: 'disabled-key' },
      { api_key: 'enabled-key-a' },
      { api_key: 'enabled-key-b' }
    ],
    urlStats: {
      'https://disabled-upstream.test': { disabled: true }
    },
    keyStates: [{ key_index: 0, disabled: true }]
  });

  try {
    const { detectChannelWebsocketSupport } = loadChannelsModals();
    const supported = await detectChannelWebsocketSupport(fixture.button);

    assert.equal(supported, true);
    assert.equal(fixture.checkbox.checked, true);
    assert.deepEqual(
      fixture.requests.map(request => ({
        url: request.body.url,
        api_key: request.body.api_key
      })),
      [
        { url: 'https://enabled-a.test', api_key: 'enabled-key-a' },
        { url: 'https://enabled-b.test', api_key: 'enabled-key-b' }
      ]
    );
  } finally {
    fixture.restore();
  }
});

for (const testCase of [
  {
    name: 'WebSocket probe selects the option when upstream supports it',
    supported: true,
    initialChecked: false,
    expectedNotification: 'channels.websocketsProbeSupported',
    expectedType: 'success'
  },
  {
    name: 'WebSocket probe clears the option when upstream rejects it',
    supported: false,
    initialChecked: true,
    expectedNotification: 'channels.websocketsProbeUnsupported',
    expectedType: 'warning'
  }
]) {
  test(testCase.name, async () => {
    const fixture = installWebsocketProbeGlobals(testCase);
    try {
      const { detectChannelWebsocketSupport } = loadChannelsModals();
      const supported = await detectChannelWebsocketSupport(fixture.button);

      assert.equal(supported, testCase.supported);
      assert.equal(fixture.checkbox.checked, testCase.supported);
      assert.equal(fixture.dirty, true);
      assert.equal(fixture.button.disabled, false);
      assert.equal(fixture.button.innerHTML, '检测');
      assert.deepEqual(fixture.notifications, [{
        message: testCase.expectedNotification,
        type: testCase.expectedType
      }]);
      assert.equal(fixture.request.url, '/admin/channels/websocket-probe');
      assert.deepEqual(fixture.request.body, {
        url: 'https://upstream.test',
        api_key: 'sk-probe',
        proxy_url: 'socks5://proxy.test:1080',
        custom_request_rules: {
          headers: [{ action: 'override', name: 'X-Probe', value: '1' }]
        }
      });
    } finally {
      fixture.restore();
    }
  });
}

test('common models add every selected type and ignore existing names case-insensitively', () => {
  const rows = [
    { model: 'GPT-5.4', redirect_model: 'custom-upstream-model' }
  ];

  const restore = installCommonModelsGlobals();
  try {
    const { addCommonModelsToRows } = loadChannelsModals();
    const result = addCommonModelsToRows(rows, ['anthropic', 'codex', 'anthropic']);

    assert.deepEqual(result, { addedCount: 9, hasSupportedTypes: true });
    assert.equal(rows.length, 10);
    assert.equal(rows.filter(row => row.model.toLowerCase() === 'gpt-5.4').length, 1);
    assert.ok(rows.some(row => row.model === 'claude-opus-4-8'));
    assert.ok(rows.some(row => row.model === 'gpt-5.6-terra'));
  } finally {
    restore.restore();
  }
});

test('common models require at least one supported type', () => {
  const fixture = installCommonModelsGlobals();

  try {
    const { addCommonModels } = loadChannelsModals();
    assert.equal(addCommonModels([]), 0);
    assert.equal(fixture.rows.length, 0);
    assert.equal(fixture.dirty, false);
    assert.equal(fixture.renders, 0);
    assert.deepEqual(fixture.notifications, [{
      type: 'warning',
      message: { key: 'channels.selectCommonModelType', params: undefined }
    }]);
  } finally {
    fixture.restore();
  }
});

test('fetchModelsFromAPI sends the first enabled API key', async () => {
  let requestBody;
  const restore = installFetchModelsGlobals({
    rows: [{ api_key: 'disabled-key' }, { api_key: 'enabled-key' }],
    states: [
      { key_index: 0, disabled: true },
      { key_index: 1, disabled: false }
    ],
    onFetch: async (_url, options) => {
      requestBody = JSON.parse(options.body);
      return { success: false, error: 'stop after request capture' };
    },
    onError: () => {}
  });

  try {
    await loadFetchModelsFromAPI()();
  } finally {
    restore();
  }

  assert.equal(requestBody.api_key, 'enabled-key');
});

test('fetchModelsFromAPI rejects a channel whose keys are all disabled', async () => {
  let fetchCalled = false;
  let shownError = '';
  const restore = installFetchModelsGlobals({
    rows: [{ api_key: 'disabled-key' }],
    states: [{ key_index: 0, disabled: true }],
    onFetch: async () => {
      fetchCalled = true;
      return {};
    },
    onError: message => { shownError = message; }
  });

  try {
    await loadFetchModelsFromAPI()();
  } finally {
    restore();
  }

  assert.equal(fetchCalled, false);
  assert.equal(shownError, 'channels.addAtLeastOneEnabledKey');
});
