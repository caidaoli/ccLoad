const test = require('node:test');
const assert = require('node:assert/strict');

const { selectAvailableInlineKeys, selectFirstEnabledInlineKey } = require('./channels-keys.js');
const { applyURLStats, fetchURLStats } = require('./channels-urls.js');
const ModelEntryParser = require('./model-entry-parser.js');

function installFetchModelsGlobals({ rows, states, onFetch, onError, onWarning, channelId = null, authType = 'api_key' }) {
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
    editingChannelId: channelId,
    editingChannelAuthType: authType,
    selectAvailableInlineKeys,
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

function installBatchProtocolModeGlobals(response) {
  const requests = [];
  const notifications = [];
  let filterSaves = 0;
  let reloads = 0;
  const makeClassList = (initial = []) => {
    const classes = new Set(initial);
    return {
      add: (...names) => names.forEach(name => classes.add(name)),
      remove: (...names) => names.forEach(name => classes.delete(name)),
      contains: name => classes.has(name),
      toggle(name, force) {
        if (force === undefined ? !classes.has(name) : force) classes.add(name);
        else classes.delete(name);
      }
    };
  };
  const appContainer = {
    inert: false,
    setAttribute(name) { if (name === 'inert') this.inert = true; },
    removeAttribute(name) { if (name === 'inert') this.inert = false; }
  };
  const modelImportModeAppend = { value: 'append', checked: true };
  const modelImportModeReplace = { value: 'replace', checked: false };
  const modelImportFormatText = { value: 'text', checked: true };
  const elements = {
    batchProtocolTransformMode: { value: 'local', disabled: false },
    batchApplyProtocolBtn: { disabled: false },
    batchCostMultiplier: {
      value: '0.5',
      disabled: false,
      attributes: new Map(),
      setAttribute(name, value) { this.attributes.set(name, value); },
      focus() {}
    },
    batchApplyCostMultiplierBtn: { disabled: false },
    batchCostMultiplierError: { textContent: '', hidden: true },
    batchImportModelsBtn: { disabled: false },
    batchAdvancedOptions: { open: true },
    batchRefreshOptions: { open: false },
    batchFloatingMenu: {
      inert: false,
      classList: { toggle() {} },
      setAttribute() {}
    },
    selectedChannelsSummary: { textContent: '' },
    selectedChannelsCountBadge: { textContent: '' },
    batchFloatingMenuCloseBtn: { disabled: false },
    modelImportTextarea: {
      value: '',
      disabled: false,
      dataset: {},
      placeholder: '',
      attributes: new Map(),
      setAttribute(name, value) { this.attributes.set(name, value); },
      focus() {}
    },
    modelImportError: { textContent: '', hidden: true },
    modelImportPreviewContent: { classList: makeClassList(['hidden']) },
    modelImportCount: { textContent: '' },
    modelImportModeFieldset: { hidden: true },
    modelImportTitle: { textContent: '', setAttribute() {} },
    modelImportPreviewLabel: { textContent: '', setAttribute() {} },
    modelImportConfirmBtn: { textContent: '', disabled: false, setAttribute() {} },
    modelImportInputLabel: { textContent: '', setAttribute() {} },
    modelImportInputHint: { textContent: '', setAttribute() {} },
    modelImportTextHelp: { hidden: false },
    modelImportJSONHelp: { hidden: true },
    modelImportModal: {
      classList: makeClassList(),
      dataset: {},
      setAttribute() {}
    },
    channelModal: { setAttribute() {}, removeAttribute() {} },
    visibleSelectionCheckbox: { focus() {} }
  };
  const globals = {
    window: {
      t: (key, params) => params ? { key, params } : key,
      showSuccess: message => notifications.push({ type: 'success', message }),
      showError: message => notifications.push({ type: 'error', message }),
      showWarning: message => notifications.push({ type: 'warning', message }),
      ModelEntryParser,
      localStorage: { getItem: () => null, setItem() {} }
    },
    document: {
      activeElement: elements.batchImportModelsBtn,
      getElementById: id => elements[id] || null,
      querySelector: selector => ({
        '.app-container': appContainer,
        'input[name="modelImportMode"][value="append"]': modelImportModeAppend,
        'input[name="modelImportMode"]:checked': modelImportModeReplace.checked ? modelImportModeReplace : modelImportModeAppend,
        'input[name="modelImportFormat"][value="text"]': modelImportFormatText,
        'input[name="modelImportFormat"]:checked': modelImportFormatText
      })[selector] || null
    },
    selectedChannelIds: new Set(['11', '22']),
    filteredChannels: [],
    channels: [],
    fetchAPIWithAuth: async (url, options) => {
      requests.push({ url, options });
      return response;
    },
    saveChannelsFilters: () => { filterSaves++; },
    reloadChannelsList: async () => { reloads++; },
    setTimeout: callback => { callback(); return 1; },
    confirm: () => true,
    console: { ...console, error: () => {} }
  };
  const previous = new Map();
  for (const [name, value] of Object.entries(globals)) {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  }
  return {
    elements,
    notifications,
    requests,
    selectedChannelIds: globals.selectedChannelIds,
    appContainer,
    modelImportModeAppend,
    modelImportModeReplace,
    get filterSaves() { return filterSaves; },
    get reloads() { return reloads; },
    restore() {
      for (const [name, descriptor] of previous) {
        if (descriptor) Object.defineProperty(global, name, descriptor);
        else delete global[name];
      }
    }
  };
}

function installFetchSub2APIRateGlobals({ response, rows, states }) {
  const requests = [];
  const notifications = [];
  let dirty = false;
  const elements = {
    channelCostMultiplier: { value: '0.5' },
    fetchSub2APIRateBtn: {
      disabled: false,
      attributes: new Map(),
      setAttribute(name, value) { this.attributes.set(name, value); },
      removeAttribute(name) { this.attributes.delete(name); }
    },
    fetchSub2APIRateLabel: { textContent: '' }
  };
  const globals = {
    window: {
      t: (key, params) => params ? { key, params } : key,
      showSuccess: message => notifications.push({ type: 'success', message }),
      showError: message => notifications.push({ type: 'error', message })
    },
    document: {
      getElementById: id => elements[id] || null,
      querySelector: () => null
    },
    getValidInlineURLConfigs: () => [{ url: 'https://sub2api.test/v1', exact: false, protocols: ['openai'] }],
    getInlineKeyRows: () => rows,
    currentChannelKeyCooldowns: states,
    selectFirstEnabledInlineKey,
    fetchAPIWithAuth: async (url, options) => {
      requests.push({ url, options });
      return response;
    },
    markChannelFormDirty: () => { dirty = true; },
    alert: message => notifications.push({ type: 'alert', message }),
    console: { ...console, error: () => {} }
  };
  const previous = new Map();
  for (const [name, value] of Object.entries(globals)) {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  }
  return {
    elements,
    notifications,
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

function installEditChannelGlobals(channel, {
  editorError = null,
  editorKeys = [],
  codexCredential = null,
  codexCredentialInfo = null
} = {}) {
  const requests = [];
  const errors = [];
  const authEditorCalls = [];
  let loadedKeys = [];
  const elements = new Map();
  const makeElement = () => {
    const classes = new Set();
    return {
      value: '',
      checked: false,
      disabled: false,
      hidden: false,
      style: {},
      dataset: {},
      classList: {
        add: (...names) => names.forEach(name => classes.add(name)),
        remove: (...names) => names.forEach(name => classes.delete(name)),
        contains: name => classes.has(name)
      },
      setAttribute() {},
      addEventListener() {},
      appendChild() {},
      querySelector: () => null
    };
  };
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
      showError: message => errors.push(message),
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
    editingChannelAuthType: 'api_key',
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
      if (url === `/admin/channels/${channel.id}/editor`) {
        if (editorError) throw editorError;
        return {
          channel,
          keys: editorKeys,
          oauth_credential: codexCredential,
          oauth_credential_info: codexCredentialInfo,
          model_stats: { available: true, items: [] },
          url_stats: {
            available: true,
            items: [{ url: channel.urls[0].url, latency_ms: 125, requests: 1, failures: 0 }]
          },
          features: { scheduled_check_enabled: true }
        };
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
    applyURLStats,
    fetchURLStats,
    urlStatsMap: {},
    renderInlineURLTable() {},
    setInlineKeyTableDataFromAPI(keys) { loadedKeys = keys; },
    renderInlineKeyTable() {},
    applyChannelAuthEditorMode(authType, credential, channel, credentialInfo) {
      authEditorCalls.push({ authType, credential, channel, credentialInfo });
    },
    renderRedirectTable() {},
    resetChannelFormDirty() {},
    syncChannelEditorTableSizing() {},
    scheduleChannelEditorTableSizingSync() {},
    console: { ...console, error() {} }
  };
  const previous = new Map();
  for (const [name, value] of Object.entries(globals)) {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  }
  return {
    errors,
    requests,
    authEditorCalls,
    get loadedKeys() { return loadedKeys; },
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
    editingChannelAuthType: 'api_key',
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

test('editing a channel loads the complete editor state with one request', async () => {
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
    assert.deepEqual(fixture.requests, [`/admin/channels/${channel.id}/editor`]);
    assert.equal(fixture.getElement('quickAddChannelBtn').hidden, false);
  } finally {
    fixture.restore();
  }
});

test('editing a Codex channel loads its AT row and full credential into read-only mode', async () => {
  const channel = {
    id: 75,
    name: 'codex-oauth',
    auth_type: 'codex_oauth',
    urls: [{ url: 'https://chatgpt.com/backend-api/codex/responses', exact: true, protocols: ['codex'] }],
    models: [],
    priority: 0,
    enabled: true,
    protocol_transform_mode: 'upstream'
  };
  const credential = { type: 'codex', access_token: 'at-editor', refresh_token: 'rt-editor' };
  const credentialInfo = { chatgpt_account_id: 'account-editor', plan_type: 'plus' };
  const keys = [{ key_index: 0, api_key: 'at-editor', note: 'Codex OAuth AT' }];
  const fixture = installEditChannelGlobals(channel, {
    editorKeys: keys,
    codexCredential: credential,
    codexCredentialInfo: credentialInfo
  });

  try {
    const { editChannel } = loadChannelsModals();
    await editChannel(channel.id);

    assert.deepEqual(fixture.loadedKeys, keys);
    assert.deepEqual(fixture.authEditorCalls.at(-1), {
      authType: 'codex_oauth',
      credential,
      channel,
      credentialInfo
    });
  } finally {
    fixture.restore();
  }
});

test('editing a channel does not open a partial editor when bootstrap fails', async () => {
  const channel = {
    id: 74,
    urls: [{ url: 'https://failed-bootstrap.test', exact: false, protocols: [] }]
  };
  const fixture = installEditChannelGlobals(channel, { editorError: new Error('database unavailable') });

  try {
    const { editChannel } = loadChannelsModals();
    await editChannel(channel.id);

    assert.deepEqual(fixture.requests, [`/admin/channels/${channel.id}/editor`]);
    assert.deepEqual(fixture.errors, ['channels.loadChannelsFailed']);
    assert.equal(fixture.getElement('channelModal').classList.contains('show'), false);
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

    assert.deepEqual(result, { addedCount: 13, hasSupportedTypes: true });
    assert.equal(rows.length, 14);
    assert.equal(rows.filter(row => row.model.toLowerCase() === 'gpt-5.4').length, 1);
    assert.ok(rows.some(row => row.model === 'claude-opus-4-8'));
    assert.ok(rows.some(row => row.model === 'gpt-5.6-terra'));
    assert.ok(rows.some(row => row.model === 'gpt-5.3-codex-spark'));
    assert.ok(rows.some(row => row.model === 'codex-auto-review'));
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
    { model: 'UPSTREAM-MODEL', redirect_model: 'UPSTREAM-MODEL' },
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

test('fetchModelsFromAPI sends every available API key', async () => {
  let requestBody;
  const restore = installFetchModelsGlobals({
    rows: [
      { api_key: 'disabled-key' },
      { api_key: 'cooling-key' },
      { api_key: 'enabled-key-1' },
      { api_key: 'enabled-key-2' }
    ],
    states: [
      { key_index: 0, disabled: true },
      { key_index: 1, disabled: false, cooldown_remaining_ms: 60_000 },
      { key_index: 2, disabled: false },
      { key_index: 3, disabled: false }
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

  assert.deepEqual(requestBody.api_keys, ['enabled-key-1', 'enabled-key-2']);
  assert.equal(requestBody.api_key, undefined);
  assert.deepEqual(requestBody.urls, [{ url: 'https://upstream.test', exact: false, protocols: ['openai'] }]);
});

test('fetchModelsFromAPI uses the saved Antigravity channel without submitting its OAuth token', async () => {
  const requests = [];
  const restore = installFetchModelsGlobals({
    rows: [{ api_key: 'oauth-access-token-that-must-not-be-submitted' }],
    states: [{ key_index: 0, disabled: false }],
    channelId: 42,
    authType: 'antigravity_oauth',
    onFetch: async (url, options) => {
      requests.push({ url, options });
      return { success: false, error: 'stop after request capture' };
    },
    onError: () => {}
  });

  try {
    await loadFetchModelsFromAPI()();
  } finally {
    restore();
  }

  assert.deepEqual(requests, [{
    url: '/admin/channels/42/models/fetch',
    options: undefined
  }]);
});

test('fetchModelsFromAPI uses the saved Codex channel without submitting its OAuth token', async () => {
  const requests = [];
  const restore = installFetchModelsGlobals({
    rows: [{ api_key: 'codex-access-token-that-must-not-be-submitted' }],
    states: [{ key_index: 0, disabled: false }],
    channelId: 43,
    authType: 'codex_oauth',
    onFetch: async (url, options) => {
      requests.push({ url, options });
      return { success: false, error: 'stop after request capture' };
    },
    onError: () => {}
  });

  try {
    await loadFetchModelsFromAPI()();
  } finally {
    restore();
  }

  assert.deepEqual(requests, [{
    url: '/admin/channels/43/models/fetch',
    options: undefined
  }]);
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

test('fetchSub2APIRate writes the effective multiplier from the first enabled key', async () => {
  const fixture = installFetchSub2APIRateGlobals({
    response: { success: true, data: { effective_rate_multiplier: 1.2 } },
    rows: [{ api_key: 'disabled-key' }, { api_key: 'enabled-key' }],
    states: [
      { key_index: 0, disabled: true },
      { key_index: 1, disabled: false }
    ]
  });

  try {
    const { fetchSub2APIRate } = loadChannelsModals();
    await fetchSub2APIRate();

    assert.equal(fixture.requests.length, 1);
    assert.equal(fixture.requests[0].url, '/admin/channels/billing/fetch');
    assert.deepEqual(JSON.parse(fixture.requests[0].options.body), {
      base_url: 'https://sub2api.test/v1',
      api_key: 'enabled-key'
    });
    assert.equal(fixture.elements.channelCostMultiplier.value, '1.2');
    assert.equal(fixture.elements.fetchSub2APIRateBtn.disabled, false);
    assert.equal(fixture.dirty, true);
    assert.deepEqual(fixture.notifications, [{
      type: 'success',
      message: { key: 'channels.fetchRateSuccess', params: { rate: '1.2' } }
    }]);
  } finally {
    fixture.restore();
  }
});

test('fetchSub2APIRate preserves the input and maps authentication failures', async () => {
  const fixture = installFetchSub2APIRateGlobals({
    response: { success: false, data: { code: 'authentication_error' } },
    rows: [{ api_key: 'invalid-key' }],
    states: [{ key_index: 0, disabled: false }]
  });

  try {
    const { fetchSub2APIRate } = loadChannelsModals();
    await fetchSub2APIRate();

    assert.equal(fixture.elements.channelCostMultiplier.value, '0.5');
    assert.equal(fixture.dirty, false);
    assert.deepEqual(fixture.notifications, [{
      type: 'error',
      message: 'channels.fetchRateError.authentication_error'
    }]);
  } finally {
    fixture.restore();
  }
});

test('batch protocol mode submits selected channel IDs and refreshes the list', async () => {
  const fixture = installBatchProtocolModeGlobals({
    success: true,
    data: { updated: 2, unchanged: 0, not_found_count: 0 }
  });

  try {
    const { batchSetSelectedChannelsProtocolMode } = loadChannelsModals();
    await batchSetSelectedChannelsProtocolMode();

    assert.equal(fixture.requests.length, 1);
    assert.equal(fixture.requests[0].url, '/admin/channels/batch-advanced');
    assert.equal(fixture.requests[0].options.method, 'POST');
    assert.deepEqual(JSON.parse(fixture.requests[0].options.body), {
      channel_ids: [11, 22],
      protocol_transform_mode: 'local'
    });
    assert.equal(fixture.selectedChannelIds.size, 0);
    assert.equal(fixture.filterSaves, 1);
    assert.equal(fixture.reloads, 1);
    assert.deepEqual(fixture.notifications, [{
      type: 'success',
      message: {
        key: 'channels.batchProtocolModeSummary',
        params: {
          mode: 'channels.batchProtocolModeValue.local',
          updated: 2,
          unchanged: 0,
          notFound: 0
        }
      }
    }]);
  } finally {
    fixture.restore();
  }
});

test('batch cost multiplier submits a numeric patch and refreshes the list', async () => {
  const fixture = installBatchProtocolModeGlobals({
    success: true,
    data: { updated: 2, unchanged: 0, not_found_count: 0 }
  });

  try {
    const { batchSetSelectedChannelsCostMultiplier } = loadChannelsModals();
    await batchSetSelectedChannelsCostMultiplier();

    assert.equal(fixture.requests.length, 1);
    assert.equal(fixture.requests[0].url, '/admin/channels/batch-advanced');
    assert.deepEqual(JSON.parse(fixture.requests[0].options.body), {
      channel_ids: [11, 22],
      cost_multiplier: 0.5
    });
    assert.equal(fixture.selectedChannelIds.size, 0);
    assert.equal(fixture.filterSaves, 1);
    assert.equal(fixture.reloads, 1);
    assert.deepEqual(fixture.notifications, [{
      type: 'success',
      message: {
        key: 'channels.batchCostMultiplierSummary',
        params: {
          multiplier: 0.5,
          updated: 2,
          unchanged: 0,
          notFound: 0
        }
      }
    }]);
  } finally {
    fixture.restore();
  }
});

test('batch cost multiplier rejects negative values without sending a request', async () => {
  const fixture = installBatchProtocolModeGlobals({ success: true, data: {} });
  fixture.elements.batchCostMultiplier.value = '-1';

  try {
    const { batchSetSelectedChannelsCostMultiplier } = loadChannelsModals();
    await batchSetSelectedChannelsCostMultiplier();

    assert.equal(fixture.requests.length, 0);
    assert.equal(fixture.selectedChannelIds.size, 2);
    assert.equal(fixture.elements.batchCostMultiplier.attributes.get('aria-invalid'), 'true');
    assert.equal(fixture.elements.batchCostMultiplierError.hidden, false);
    assert.deepEqual(fixture.notifications, []);
  } finally {
    fixture.restore();
  }
});

test('batch cost multiplier rejects an empty value instead of treating it as zero', async () => {
  const fixture = installBatchProtocolModeGlobals({ success: true, data: {} });
  fixture.elements.batchCostMultiplier.value = '';

  try {
    const { batchSetSelectedChannelsCostMultiplier } = loadChannelsModals();
    await batchSetSelectedChannelsCostMultiplier();

    assert.equal(fixture.requests.length, 0);
    assert.equal(fixture.selectedChannelIds.size, 2);
    assert.equal(fixture.elements.batchCostMultiplier.attributes.get('aria-invalid'), 'true');
    assert.equal(fixture.elements.batchCostMultiplierError.hidden, false);
  } finally {
    fixture.restore();
  }
});

test('batch model import parses mappings and submits append mode for selected channels', async () => {
  const fixture = installBatchProtocolModeGlobals({
    success: true,
    data: { updated: 2, unchanged: 0, not_found_count: 0 }
  });

  try {
    const { confirmModelImport, openBatchModelImportModal } = loadChannelsModals();
    openBatchModelImportModal();
    fixture.elements.modelImportTextarea.value = 'request-a|upstream-a\npassthrough';
    await confirmModelImport();

    assert.equal(fixture.requests.length, 1);
    assert.equal(fixture.requests[0].url, '/admin/channels/batch-advanced');
    assert.deepEqual(JSON.parse(fixture.requests[0].options.body), {
      channel_ids: [11, 22],
      model_import_mode: 'append',
      models: [
        { model: 'request-a', redirect_model: 'upstream-a' },
        { model: 'passthrough', redirect_model: '' }
      ]
    });
    assert.equal(fixture.selectedChannelIds.size, 0);
    assert.equal(fixture.filterSaves, 1);
    assert.equal(fixture.reloads, 1);
    assert.equal(fixture.appContainer.inert, false);
    assert.equal(fixture.elements.modelImportModal.classList.contains('show'), false);
    assert.deepEqual(fixture.notifications, [{
      type: 'success',
      message: {
        key: 'channels.batchModelImportSummary',
        params: {
          mode: 'channels.batchModelImportModeValue.append',
          updated: 2,
          unchanged: 0,
          notFound: 0
        }
      }
    }]);
  } finally {
    fixture.restore();
  }
});

test('batch model import submits replace mode after confirmation', async () => {
  const fixture = installBatchProtocolModeGlobals({
    success: true,
    data: { updated: 2, unchanged: 0, not_found_count: 0 }
  });

  try {
    const { confirmModelImport, openBatchModelImportModal } = loadChannelsModals();
    openBatchModelImportModal();
    fixture.modelImportModeAppend.checked = false;
    fixture.modelImportModeReplace.checked = true;
    fixture.elements.modelImportTextarea.value = 'replacement|replacement-upstream';
    await confirmModelImport();

    assert.equal(fixture.requests.length, 1);
    assert.deepEqual(JSON.parse(fixture.requests[0].options.body), {
      channel_ids: [11, 22],
      model_import_mode: 'replace',
      models: [{ model: 'replacement', redirect_model: 'replacement-upstream' }]
    });
    assert.equal(fixture.selectedChannelIds.size, 0);
  } finally {
    fixture.restore();
  }
});

test('batch protocol mode keeps the selection when the request fails', async () => {
  const fixture = installBatchProtocolModeGlobals({ success: false, error: 'database unavailable' });

  try {
    const { batchSetSelectedChannelsProtocolMode } = loadChannelsModals();
    await batchSetSelectedChannelsProtocolMode();

    assert.equal(fixture.selectedChannelIds.size, 2);
    assert.equal(fixture.filterSaves, 0);
    assert.equal(fixture.reloads, 0);
    assert.deepEqual(fixture.notifications, [{
      type: 'error',
      message: {
        key: 'channels.batchOperationFailed',
        params: { error: 'database unavailable' }
      }
    }]);
    assert.equal(fixture.elements.batchApplyProtocolBtn.disabled, false);
    assert.equal(fixture.elements.batchProtocolTransformMode.disabled, false);
  } finally {
    fixture.restore();
  }
});

test('model normalization options synchronize across workflows and persist', () => {
  const storageData = new Map();
  const storage = {
    getItem: key => storageData.get(key) ?? null,
    setItem: (key, value) => storageData.set(key, value)
  };
  const createCheckbox = () => {
    const listeners = new Map();
    return {
      checked: false,
      dataset: {},
      addEventListener(type, listener) { listeners.set(type, listener); },
      dispatchChange() { listeners.get('change')?.(); }
    };
  };
  const lowercaseIDs = [
    'batchRefreshLowercaseModels',
    'quickAddLowercaseModels',
    'modelImportLowercaseModels'
  ];
  const stripPrefixIDs = [
    'batchRefreshStripModelSourcePrefix',
    'quickAddStripModelSourcePrefix',
    'modelImportStripModelSourcePrefix'
  ];
  const createInputs = () => Object.fromEntries(
    [...lowercaseIDs, ...stripPrefixIDs].map(id => [id, createCheckbox()])
  );
  let inputs = createInputs();
  const previousDocument = Object.getOwnPropertyDescriptor(global, 'document');
  Object.defineProperty(global, 'document', {
    configurable: true,
    writable: true,
    value: { getElementById: id => inputs[id] || null }
  });

  try {
    const { initModelNormalizationOptions } = loadChannelsModals();
    initModelNormalizationOptions(storage);
    assert.equal(lowercaseIDs.every(id => inputs[id].checked === false), true);
    assert.equal(stripPrefixIDs.every(id => inputs[id].checked === false), true);

    inputs.quickAddLowercaseModels.checked = true;
    inputs.quickAddLowercaseModels.dispatchChange();
    assert.equal(lowercaseIDs.every(id => inputs[id].checked === true), true);

    inputs.modelImportStripModelSourcePrefix.checked = true;
    inputs.modelImportStripModelSourcePrefix.dispatchChange();
    assert.equal(stripPrefixIDs.every(id => inputs[id].checked === true), true);
    assert.deepEqual(JSON.parse(storageData.get('channels.modelNormalizationOptions')), {
      lowercase_models: true,
      strip_model_source_prefix: true
    });

    inputs = createInputs();
    initModelNormalizationOptions(storage);
    assert.equal(lowercaseIDs.every(id => inputs[id].checked === true), true);
    assert.equal(stripPrefixIDs.every(id => inputs[id].checked === true), true);

    storageData.set('channels.modelNormalizationOptions', '{');
    inputs = createInputs();
    initModelNormalizationOptions(storage);
    assert.equal(lowercaseIDs.every(id => inputs[id].checked === false), true);
    assert.equal(stripPrefixIDs.every(id => inputs[id].checked === false), true);
  } finally {
    if (previousDocument) Object.defineProperty(global, 'document', previousDocument);
    else delete global.document;
  }
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
  }, {
    lowercaseModels: true,
    stripModelSourcePrefix: true
  });

  assert.deepEqual(request, {
    url: '/admin/channels/models/fetch',
    body: {
      urls: [{ url: 'https://gateway.example.com/api', exact: false, protocols: [] }],
      protocol: 'openai',
      api_keys: ['sk-test-secret'],
      lowercase_models: true,
      strip_model_source_prefix: true
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
