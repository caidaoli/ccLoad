const test = require('node:test');
const assert = require('node:assert/strict');

const { selectFirstEnabledInlineKey } = require('./channels-keys.js');
const { fetchURLStats } = require('./channels-urls.js');

function installFetchModelsGlobals({ rows, states, onFetch, onError, onWarning }) {
	const globals = {
		window: {
			t: key => key,
      showError: onError,
      showWarning: onWarning
    },
    document: { querySelector: () => null },
    getValidInlineURLConfigs: () => [{ url: 'https://upstream.test', exact: false, protocols: ['openai'] }],
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

function installEditChannelGlobals(channel) {
  const requests = [];
  const elements = new Map();
  const makeElement = () => ({
    value: '',
    checked: false,
    disabled: false,
    hidden: false,
    style: {},
    dataset: {},
    classList: { add() {}, remove() {}, contains() { return false; } },
    setAttribute() {},
    addEventListener() {},
    appendChild() {},
    querySelector: () => null
  });
  const getElement = id => {
    if (id === 'channelScheduledCheckEnabledWrapper' || id === 'channelScheduledCheckModelWrapper') {
      return null;
    }
    if (!elements.has(id)) elements.set(id, makeElement());
    return elements.get(id);
  };
  const globals = {
    window: {
      t: key => key,
      addEventListener() {}
    },
    document: {
      getElementById: getElement,
      querySelector: selector => [
        '#channelModal .channel-editor-body',
        '#inlineUrlTableBody'
      ].includes(selector) ? null : makeElement()
    },
    channels: [],
    editingChannelId: null,
    currentChannelKeyCooldowns: [],
    inlineKeyTableData: [{ api_key: '' }],
    inlineKeyVisible: false,
    inlineURLTableData: channel.urls,
    inlineURLProtocolComboboxes: new Map(),
    selectedURLIndices: new Set(),
    redirectTableData: [],
    selectedModelIndices: new Set(),
    currentModelFilter: '',
    fetchDataWithAuth: async url => {
      requests.push(url);
      if (url === `/admin/channels/${channel.id}`) return channel;
      if (url === `/admin/channels/${channel.id}/keys`) return [];
      if (url === `/admin/channels/${channel.id}/model-stats`) return [];
      if (url === `/admin/channels/${channel.id}/url-stats`) {
        return [{ url: channel.urls[0].url, latency_ms: 125, requests: 1, failures: 0 }];
      }
      throw new Error(`unexpected fetch: ${url}`);
    },
    createSearchableCombobox: () => ({
      setValue() {},
      refresh() {},
      getInput: () => null,
      getValue: () => 'auto'
    }),
    TemplateEngine: { render: () => null },
    clearChannelDuplicateHint() {},
    setInlineURLTableData() {},
    fetchURLStats,
    urlStatsMap: {},
    renderInlineURLTable() {},
    setInlineKeyTableDataFromAPI() {},
    renderInlineKeyTable() {},
    renderRedirectTable() {},
    resetChannelFormDirty() {},
    syncChannelEditorTableSizing() {},
    scheduleChannelEditorTableSizingSync() {}
  };
  const previous = new Map();
  for (const [name, value] of Object.entries(globals)) {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  }
  return {
    requests,
    getElement,
    restore() {
      for (const [name, descriptor] of previous) {
        if (descriptor) Object.defineProperty(global, name, descriptor);
        else delete global[name];
      }
    }
  };
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

function installModelRequestTestGlobals({ dirty = false } = {}) {
  const calls = [];
  const notifications = [];
  const button = {
    disabled: false,
    isConnected: true,
    attributes: new Map(),
    setAttribute(name, value) { this.attributes.set(name, value); },
    removeAttribute(name) { this.attributes.delete(name); }
  };
  const globals = {
    window: {
      t: key => key,
      showWarning: message => notifications.push(message)
    },
    redirectTableData: [{ model: 'requested-model', redirect_model: 'upstream-model', disabled: false }],
    editingChannelId: 7,
    channelFormDirty: dirty,
    channels: [{ id: 7, name: 'test-channel' }],
    testChannel: async (...args) => {
      calls.push({ type: 'open', args });
      return true;
    },
    runChannelTest: async () => { calls.push({ type: 'run' }); }
  };
  const previous = new Map();
  for (const [name, value] of Object.entries(globals)) {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  }
  return {
    button,
    calls,
    notifications,
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

test('editing a single-URL channel loads its URL statistics', async () => {
  const channel = {
    id: 73,
    name: 'single-url',
    urls: [{ url: 'https://single.test', exact: false, protocols: [] }],
    models: [],
    priority: 100,
    enabled: true,
    protocol_transform_mode: 'auto'
  };
  const fixture = installEditChannelGlobals(channel);

  try {
    const { editChannel } = loadChannelsModals();
    await editChannel(channel.id);
    assert.ok(fixture.requests.includes(`/admin/channels/${channel.id}/url-stats`));
    assert.equal(fixture.getElement('quickAddChannelBtn').hidden, true);
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

test('fetched models sort by model name while preserving existing state', () => {
  const { mergeModelRowsWithFetchedModels } = loadChannelsModals();
  const result = mergeModelRowsWithFetchedModels([
    { model: 'z-existing-model', redirect_model: 'upstream-model', disabled: true }
  ], [
    { model: 'z-existing-model', redirect_model: 'ignored-replacement' },
    { model: 'm-new-model', redirect_model: 'new-upstream' },
    { model: 'a-new-model', redirect_model: 'another-upstream' }
  ]);

  assert.deepEqual(result, {
    rows: [
      { model: 'a-new-model', redirect_model: 'another-upstream', disabled: false },
      { model: 'm-new-model', redirect_model: 'new-upstream', disabled: false },
      { model: 'z-existing-model', redirect_model: 'upstream-model', disabled: true }
    ],
    added: 2,
    removed: 0
  });
});

test('model disabled state toggles without changing the model mapping', () => {
  const { toggleModelDisabledState } = loadChannelsModals();
  const rows = [{ model: 'model-a', redirect_model: 'upstream-a', disabled: false }];

  assert.equal(toggleModelDisabledState(rows, 0), true);
  assert.deepEqual(rows, [{ model: 'model-a', redirect_model: 'upstream-a', disabled: true }]);
  assert.equal(toggleModelDisabledState(rows, 0), true);
  assert.deepEqual(rows, [{ model: 'model-a', redirect_model: 'upstream-a', disabled: false }]);
  assert.equal(toggleModelDisabledState(rows, 9), false);
});

test('model row test opens the existing test flow for the current model and runs it', async () => {
  const fixture = installModelRequestTestGlobals();

  try {
    const { testRedirectModel } = loadChannelsModals();
    assert.equal(await testRedirectModel(0, fixture.button), true);
    assert.deepEqual(fixture.calls, [
      { type: 'open', args: [7, 'test-channel', 'requested-model'] },
      { type: 'run' }
    ]);
    assert.equal(fixture.button.disabled, false);
    assert.equal(fixture.button.attributes.has('aria-busy'), false);
  } finally {
    fixture.restore();
  }
});

test('model row test rejects unsaved channel changes', async () => {
  const fixture = installModelRequestTestGlobals({ dirty: true });

  try {
    const { testRedirectModel } = loadChannelsModals();
    assert.equal(await testRedirectModel(0, fixture.button), false);
    assert.deepEqual(fixture.calls, []);
    assert.deepEqual(fixture.notifications, ['channels.saveBeforeModelTest']);
  } finally {
    fixture.restore();
  }
});

test('model submit payload includes disabled state', () => {
  const { collectModelsForSubmit } = loadChannelsModals();
  assert.deepEqual(collectModelsForSubmit([
    { model: '  model-a  ', redirect_model: ' upstream-a ', disabled: true },
    { model: 'model-b', redirect_model: '', disabled: false },
    { model: '   ', disabled: true }
  ]), [
    { model: 'model-a', redirect_model: 'upstream-a', disabled: true },
    { model: 'model-b', redirect_model: '', disabled: false }
  ]);
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
  assert.deepEqual(requestBody.urls, [{ url: 'https://upstream.test', exact: false, protocols: ['openai'] }]);
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

test('quick add parses connection text and only returns setup after model discovery succeeds', async () => {
  const { discoverQuickAddChannelSetup } = loadChannelsModals();
  let request;

  const setup = await discoverQuickAddChannelSetup(`
    export OPENAI_BASE_URL="https://gateway.example.com/api/"
    export OPENAI_API_KEY="sk-test-secret"
  `, async (url, options) => {
    request = { url, body: JSON.parse(options.body) };
    return {
      success: true,
      data: {
        protocol: 'openai',
        models: [
          { model: 'z-model', redirect_model: 'z-upstream' },
          { model: 'a-model', redirect_model: 'a-upstream' }
        ]
      }
    };
  });

  assert.deepEqual(request, {
    url: '/admin/channels/models/fetch',
    body: {
      urls: [{ url: 'https://gateway.example.com/api', exact: false, protocols: [] }],
      protocol: 'openai',
      api_key: 'sk-test-secret'
    }
  });
  assert.deepEqual(setup, {
    url: { url: 'https://gateway.example.com/api', exact: false, protocols: [] },
    key: { api_key: 'sk-test-secret', note: '' },
    models: [
      { model: 'a-model', redirect_model: 'a-upstream', disabled: false },
      { model: 'z-model', redirect_model: 'z-upstream', disabled: false }
    ]
  });
});

test('quick add falls back from OpenAI to Anthropic model discovery', async () => {
  const { discoverQuickAddChannelSetup } = loadChannelsModals();
  const attemptedProtocols = [];

  const setup = await discoverQuickAddChannelSetup(
    'URL=https://gateway.example.com\nAPI_KEY=sk-fallback',
    async (_url, options) => {
      const body = JSON.parse(options.body);
      attemptedProtocols.push(body.protocol);
      if (body.protocol === 'openai') {
        return { success: false, error: 'OpenAI models endpoint is unsupported' };
      }
      return {
        success: true,
        data: {
          protocol: 'anthropic',
          models: [{ model: 'claude-test', redirect_model: 'claude-test' }]
        }
      };
    }
  );

  assert.deepEqual(attemptedProtocols, ['openai', 'anthropic']);
  assert.deepEqual(setup.models, [
    { model: 'claude-test', redirect_model: 'claude-test', disabled: false }
  ]);
});

test('quick add rejects invalid discovery without producing partial setup', async () => {
  const { discoverQuickAddChannelSetup } = loadChannelsModals();

  await assert.rejects(
    discoverQuickAddChannelSetup(
      '{"base_url":"https://gateway.example.com","api_key":"sk-invalid"}',
      async () => ({ success: false, error: 'unauthorized' })
    ),
    /unauthorized/
  );
});

test('quick add parses URL and key labels on one line', () => {
  const { parseQuickAddChannelInfo } = loadChannelsModals();
  assert.deepEqual(
    parseQuickAddChannelInfo('URL: https://gateway.example.com/api  API Key: sk-one-line'),
    { url: 'https://gateway.example.com/api', apiKey: 'sk-one-line' }
  );
});

test('quick add normalizes a versioned API endpoint to the channel base URL', () => {
  const { parseQuickAddChannelInfo } = loadChannelsModals();
  assert.deepEqual(
    parseQuickAddChannelInfo('OPENAI_BASE_URL=https://gateway.example.com/openai/v1\nOPENAI_API_KEY=sk-versioned'),
    { url: 'https://gateway.example.com/openai', apiKey: 'sk-versioned' }
  );
});

test('quick add derives an empty channel name and applies the setup atomically', () => {
  const previous = new Map();
  const redirectBody = {
    dataset: {},
    innerHTML: '',
    addEventListener() {},
    appendChild() {}
  };
  const redirectCount = { textContent: '' };
  const channelNameInput = { value: '   ' };
  const globals = {
    window: { t: key => key },
    document: {
      getElementById: id => ({
        redirectTableBody: redirectBody,
        redirectCount,
        channelName: channelNameInput
      })[id] || null,
      createDocumentFragment: () => ({ appendChild() {} })
    },
    TemplateEngine: { render: () => ({ querySelector: () => null }) },
    inlineURLTableData: [{ url: '', exact: false, protocols: [] }],
    inlineKeyTableData: [{ api_key: '', note: '' }],
    redirectTableData: [{ model: 'stale-model', redirect_model: '', disabled: false }],
    currentModelFilter: '',
    currentChannelKeyCooldowns: [{ key_index: 0, disabled: true }],
    selectedKeyIndices: new Set([0]),
    selectedModelIndices: new Set([0]),
    selectedURLIndices: new Set([0]),
    setInlineURLTableData: urls => { global.inlineURLTableData = urls; },
    setInlineKeyTableDataFromAPI: keys => { global.inlineKeyTableData = keys; },
    renderInlineKeyTable() {},
    syncChannelEditorTableSizing() {},
    scheduleChannelEditorTableSizingSync() {},
    markChannelFormDirty: () => { global.quickAddFormDirty = true; },
    quickAddFormDirty: false
  };
  for (const [name, value] of Object.entries(globals)) {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  }

  try {
    const { applyQuickAddChannelSetup } = loadChannelsModals();
    const setup = {
      url: { url: 'https://gateway.example.com', exact: false, protocols: [] },
      key: { api_key: 'sk-valid', note: '' },
      models: [{ model: 'gpt-test', redirect_model: 'gpt-test', disabled: false }]
    };
    applyQuickAddChannelSetup(setup);

    assert.deepEqual(global.inlineURLTableData, [
      { url: 'https://gateway.example.com', exact: false, protocols: [] }
    ]);
    assert.deepEqual(global.inlineKeyTableData, [{ api_key: 'sk-valid', note: '' }]);
    assert.deepEqual(global.redirectTableData, [
      { model: 'gpt-test', redirect_model: 'gpt-test', disabled: false }
    ]);
    assert.equal(channelNameInput.value, 'gateway.example.com');
    assert.deepEqual(global.currentChannelKeyCooldowns, []);
    assert.equal(global.selectedModelIndices.size, 0);
    assert.equal(global.quickAddFormDirty, true);

    channelNameInput.value = '保留现有名称';
    applyQuickAddChannelSetup({
      ...setup,
      url: { url: 'https://other.example.com', exact: false, protocols: [] }
    });
    assert.equal(channelNameInput.value, '保留现有名称');
  } finally {
    for (const [name, descriptor] of previous) {
      if (descriptor) Object.defineProperty(global, name, descriptor);
      else delete global[name];
    }
  }
});
