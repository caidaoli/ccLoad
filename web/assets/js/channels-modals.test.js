const test = require('node:test');
const assert = require('node:assert/strict');

const {
  normalizeInlineKeyRow,
  pruneKeyAllowedModels,
  selectAvailableInlineKeys,
  selectModelFetchKeyEntries,
  countConfiguredInlineKeys,
  selectFirstEnabledInlineKey,
  selectModelsForInlineKeyTest,
  openKeyModelScopeModal,
  closeKeyModelScopeModal,
  detectKeyModelScope,
  initKeyModelScopeModalEvents,
  setVisibleKeyModelScopeChecked,
  updateKeyModelScopeSelectionCount,
  canFetchInlineKeyRate,
  toggleKeyDisabled
} = require('./channels-keys.js');
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
    selectModelFetchKeyEntries,
    countConfiguredInlineKeys,
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

test('倍率获取只对定义了受支持管理类型的 API Key 渠道可见', () => {
  const previousWindow = Object.getOwnPropertyDescriptor(global, 'window');
  const previousAuthType = Object.getOwnPropertyDescriptor(global, 'editingChannelAuthType');
  try {
    global.window = { getManagementAccountRateConfig: () => ({ profile: 'new_api' }) };
    global.editingChannelAuthType = 'api_key';
    assert.equal(canFetchInlineKeyRate(), true);

    global.window.getManagementAccountRateConfig = () => null;
    assert.equal(canFetchInlineKeyRate(), false, '未定义管理类型必须隐藏');

    global.window.getManagementAccountRateConfig = () => ({ profile: 'sub2api' });
    global.editingChannelAuthType = 'codex_oauth';
    assert.equal(canFetchInlineKeyRate(), false, 'OAuth 渠道必须隐藏');
  } finally {
    if (previousWindow) Object.defineProperty(global, 'window', previousWindow);
    else delete global.window;
    if (previousAuthType) Object.defineProperty(global, 'editingChannelAuthType', previousAuthType);
    else delete global.editingChannelAuthType;
  }
});

test('inline Key rows preserve and normalize model scopes', () => {
  assert.deepEqual(normalizeInlineKeyRow({
    api_key: ' sk-test ',
    note: ' primary ',
    allowed_models: [' GPT-5 ', 'gpt-5', '', 'Claude-4']
  }), {
    api_key: 'sk-test',
    note: 'primary',
    allowed_models: ['GPT-5', 'Claude-4'],
    cost_multiplier: 1
  });
  assert.deepEqual(normalizeInlineKeyRow('legacy-key').allowed_models, []);
  assert.equal(normalizeInlineKeyRow({ api_key: 'sk-free', cost_multiplier: 0 }).cost_multiplier, 0);
  assert.equal(normalizeInlineKeyRow({ api_key: 'sk-bad', cost_multiplier: -3 }).cost_multiplier, 1);
  assert.equal(normalizeInlineKeyRow({ api_key: 'sk-nan', cost_multiplier: 'abc' }).cost_multiplier, 1);
  assert.deepEqual(selectModelsForInlineKeyTest(
    { api_key: 'sk-wildcard', allowed_models: ['gpt-5'] },
    [{ model: '*', disabled: false }]
  ), ['gpt-5']);
});

test('removing configured models prunes every restricted Key scope', () => {
  const rows = [
    { api_key: 'sk-primary', allowed_models: ['GPT-5', 'claude-opus'] },
    { api_key: 'sk-unrestricted', allowed_models: [] }
  ];

  assert.deepEqual(pruneKeyAllowedModels(rows, [{ model: 'gpt-5' }]), [
    { api_key: 'sk-primary', note: '', allowed_models: ['GPT-5'], cost_multiplier: 1 },
    { api_key: 'sk-unrestricted', note: '', allowed_models: [], cost_multiplier: 1 }
  ]);
  assert.deepEqual(pruneKeyAllowedModels(rows, [{ model: '*' }]), [
    { api_key: 'sk-primary', note: '', allowed_models: ['GPT-5', 'claude-opus'], cost_multiplier: 1 },
    { api_key: 'sk-unrestricted', note: '', allowed_models: [], cost_multiplier: 1 }
  ]);
  assert.deepEqual(pruneKeyAllowedModels(
    [{ api_key: 'sk-thinking', allowed_models: ['gpt-5'] }],
    [{ model: 'gpt-5(max)' }]
  ), [{ api_key: 'sk-thinking', note: '', allowed_models: ['gpt-5'], cost_multiplier: 1 }]);
  assert.deepEqual(pruneKeyAllowedModels(rows, []), [
    { api_key: 'sk-primary', note: '', allowed_models: [], model_scope_empty: true, cost_multiplier: 1 },
    { api_key: 'sk-unrestricted', note: '', allowed_models: [], cost_multiplier: 1 }
  ]);
});

test('single and batch model deletion use the same Key scope reconciliation', () => {
  const globals = {
    redirectTableData: [{ model: 'gpt-5' }, { model: 'claude-opus' }, { model: 'qwen3' }],
    selectedModelIndices: new Set([1, 2]),
    inlineKeyTableData: [{ api_key: 'sk-scoped', allowed_models: ['gpt-5', 'claude-opus', 'qwen3'] }]
  };
  globals.syncInlineKeyModelScopesWithConfiguredModels = () => {
    globals.inlineKeyTableData = pruneKeyAllowedModels(globals.inlineKeyTableData, globals.redirectTableData);
    global.inlineKeyTableData = globals.inlineKeyTableData;
  };

  const previous = new Map();
  for (const [name, value] of Object.entries(globals)) {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  }
  try {
    const { deleteRedirectModelsAtIndices } = loadChannelsModals();
    assert.equal(deleteRedirectModelsAtIndices([1]), true);
    assert.deepEqual(global.redirectTableData, [{ model: 'gpt-5' }, { model: 'qwen3' }]);
    assert.deepEqual(global.inlineKeyTableData[0].allowed_models, ['gpt-5', 'qwen3']);
    assert.deepEqual([...global.selectedModelIndices], [1]);

    assert.equal(deleteRedirectModelsAtIndices([0, 1, 99]), true);
    assert.deepEqual(global.redirectTableData, []);
    assert.deepEqual(global.inlineKeyTableData, [{
      api_key: 'sk-scoped',
      note: '',
      allowed_models: [],
      model_scope_empty: true,
      cost_multiplier: 1
    }]);
    assert.deepEqual([...global.selectedModelIndices], []);
  } finally {
    for (const [name, descriptor] of previous) {
      if (descriptor) Object.defineProperty(global, name, descriptor);
      else delete global[name];
    }
  }
});

test('model discovery preserves duplicate Key rows for exact scope mapping', () => {
  assert.deepEqual(selectModelFetchKeyEntries([
    { api_key: 'same-key' },
    { api_key: 'same-key' }
  ], []), [
    { keyIndex: 0, apiKey: 'same-key' },
    { keyIndex: 1, apiKey: 'same-key' }
  ]);
  assert.equal(countConfiguredInlineKeys([{ api_key: 'same-key' }, { api_key: 'same-key' }]), 2);
});

test('model fetch selection excludes scope-auto-disabled Keys unless allowed', () => {
  const rows = [
    { api_key: 'sk-manual-off' },
    { api_key: 'sk-scope-empty', model_scope_empty: true },
    { api_key: 'sk-normal' }
  ];
  const states = [
    { key_index: 0, disabled: true },
    { key_index: 1, disabled: true },
    { key_index: 2, disabled: false }
  ];
  assert.deepEqual(selectModelFetchKeyEntries(rows, states, true), [
    { keyIndex: 2, apiKey: 'sk-normal' }
  ]);
  assert.deepEqual(selectModelFetchKeyEntries(rows, states, true, true), [
    { keyIndex: 2, apiKey: 'sk-normal' },
    { keyIndex: 1, apiKey: 'sk-scope-empty' }
  ]);
  assert.deepEqual(selectModelFetchKeyEntries([
    { api_key: 'sk-scope-empty', model_scope_empty: true }
  ], [], false), []);
  assert.deepEqual(selectModelFetchKeyEntries([
    { api_key: 'sk-scope-empty', model_scope_empty: true }
  ], [{ key_index: 0, disabled: false }], false), []);
});

test('directly enabling a scope-auto-disabled Key clears the stale editor marker', async () => {
  const calls = [];
  const notifications = [];
  const globals = {
    window: {
      t: key => key,
      showNotification: (...args) => notifications.push(args)
    },
    editingChannelAuthType: 'api_key',
    editingChannelId: 42,
    channelFormDirty: false,
    currentChannelKeyCooldowns: [{ key_index: 0, disabled: true }],
    inlineKeyTableData: [{ api_key: 'scope-empty-key', model_scope_empty: true }],
    console: { ...console, error: () => {} },
    fetchDataWithAuth: async (url, options) => {
      calls.push({ url, options });
      return options ? { ok: true } : [];
    }
  };
  const previous = new Map();
  for (const [name, value] of Object.entries(globals)) {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  }

  try {
    await toggleKeyDisabled(0);
    assert.equal(calls.length, 2);
    assert.equal(calls[0].url, '/admin/channels/42/key-enable');
    assert.equal(JSON.parse(calls[0].options.body).key_index, 0);
    assert.equal(global.inlineKeyTableData[0].model_scope_empty, undefined);
    assert.equal(notifications.at(-1)[1], 'success');
  } finally {
    for (const [name, descriptor] of previous) {
      if (descriptor) Object.defineProperty(global, name, descriptor);
      else delete global[name];
    }
  }
});

test('manually disabling a scope-auto-disabled Key clears the stale editor marker', async () => {
  const calls = [];
  const notifications = [];
  const globals = {
    window: {
      t: key => key,
      showNotification: (...args) => notifications.push(args)
    },
    editingChannelAuthType: 'api_key',
    editingChannelId: 42,
    channelFormDirty: false,
    currentChannelKeyCooldowns: [{ key_index: 0, disabled: false }],
    inlineKeyTableData: [{ api_key: 'scope-empty-key', model_scope_empty: true }],
    console: { ...console, error: () => {} },
    fetchDataWithAuth: async (url, options) => {
      calls.push({ url, options });
      return options ? { ok: true } : [];
    }
  };
  const previous = new Map();
  for (const [name, value] of Object.entries(globals)) {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  }

  try {
    await toggleKeyDisabled(0);
    assert.equal(calls[0].url, '/admin/channels/42/key-disable');
    assert.equal(global.inlineKeyTableData[0].model_scope_empty, undefined);
    assert.equal(notifications.at(-1)[1], 'success');
  } finally {
    for (const [name, descriptor] of previous) {
      if (descriptor) Object.defineProperty(global, name, descriptor);
      else delete global[name];
    }
  }
});

test('fetchModelsFromAPI includes scope-auto-disabled keys for discovery', async () => {
  let requestBody;
  const restore = installFetchModelsGlobals({
    rows: [
      { api_key: 'scope-empty-key', model_scope_empty: true },
      { api_key: 'manual-off-key' },
      { api_key: 'enabled-key' }
    ],
    states: [
      { key_index: 0, disabled: true },
      { key_index: 1, disabled: true },
      { key_index: 2, disabled: false }
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

  assert.deepEqual(requestBody.api_keys, ['enabled-key', 'scope-empty-key']);
  assert.equal(requestBody.per_key, true);
});

test('Key model scope master checkbox reflects and changes visible models', () => {
  const allowAll = { checked: false };
  const toggleAll = { checked: false, indeterminate: false, disabled: false };
  const count = { textContent: '' };
  const status = { textContent: '', classList: { toggle() {} } };
  const list = { setAttribute() {} };
  const labels = [{ hidden: false }, { hidden: true }, { hidden: false }];
  const checkboxes = labels.map((label, index) => ({
    checked: index !== 2,
    disabled: true,
    closest: () => label
  }));
  const previousWindow = Object.getOwnPropertyDescriptor(global, 'window');
  const previousDocument = Object.getOwnPropertyDescriptor(global, 'document');
  Object.defineProperty(global, 'window', {
    configurable: true,
    writable: true,
    value: { t: (_key, values) => `${values.selected}/${values.total}` }
  });
  Object.defineProperty(global, 'document', {
    configurable: true,
    writable: true,
    value: {
      getElementById: id => ({
        keyModelScopeAll: allowAll,
        keyModelScopeToggleAll: toggleAll,
        keyModelScopeSelectionCount: count,
        keyModelScopeStatus: status,
        keyModelScopeList: list
      })[id] || null,
      querySelectorAll: selector => selector.includes('input[name="keyAllowedModel"]') ? checkboxes : []
    }
  });

  try {
    updateKeyModelScopeSelectionCount();
    assert.equal(toggleAll.checked, false);
    assert.equal(toggleAll.indeterminate, true);
    assert.equal(toggleAll.disabled, false);
    assert.equal(count.textContent, '2/3');

    assert.equal(setVisibleKeyModelScopeChecked(false), true);
    assert.deepEqual(checkboxes.map(checkbox => checkbox.checked), [false, true, false]);
    assert.equal(toggleAll.checked, false);
    assert.equal(toggleAll.indeterminate, false);
    assert.equal(count.textContent, '1/3');

    assert.equal(setVisibleKeyModelScopeChecked(true), true);
    assert.deepEqual(checkboxes.map(checkbox => checkbox.checked), [true, true, true]);
    assert.equal(toggleAll.checked, true);
    assert.equal(toggleAll.indeterminate, false);
    assert.equal(count.textContent, '3/3');

    allowAll.checked = true;
    updateKeyModelScopeSelectionCount();
    assert.equal(toggleAll.disabled, true);
  } finally {
    if (previousWindow) Object.defineProperty(global, 'window', previousWindow);
    else delete global.window;
    if (previousDocument) Object.defineProperty(global, 'document', previousDocument);
    else delete global.document;
  }
});

test('Escape closes only the topmost Key model scope modal', () => {
  const makeClassList = initial => {
    const classes = new Set(initial);
    return {
      add: (...names) => names.forEach(name => classes.add(name)),
      remove: (...names) => names.forEach(name => classes.delete(name)),
      contains: name => classes.has(name)
    };
  };
  const keyModelScopeModal = {
    dataset: {},
    classList: makeClassList(['show']),
    querySelectorAll: () => [],
    querySelector: () => null,
    addEventListener() {},
    setAttribute() {}
  };
  const channelModal = {
    classList: makeClassList(['show']),
    removeAttribute() {}
  };
  let keydownHandler;
  const previousDocument = Object.getOwnPropertyDescriptor(global, 'document');
  Object.defineProperty(global, 'document', {
    configurable: true,
    writable: true,
    value: {
      activeElement: null,
      getElementById: id => ({ keyModelScopeModal, channelModal })[id] || null,
      querySelectorAll: () => [],
      addEventListener(type, handler, capture) {
        if (type === 'keydown') {
          assert.equal(capture, true);
          keydownHandler = handler;
        }
      }
    }
  });

  try {
    initKeyModelScopeModalEvents();
    assert.equal(typeof keydownHandler, 'function');
    let prevented = false;
    let stopped = false;
    keydownHandler({
      key: 'Escape',
      preventDefault() { prevented = true; },
      stopPropagation() { stopped = true; }
    });

    assert.equal(keyModelScopeModal.classList.contains('show'), false);
    assert.equal(channelModal.classList.contains('show'), true);
    assert.equal(prevented, true);
    assert.equal(stopped, true);
  } finally {
    if (previousDocument) Object.defineProperty(global, 'document', previousDocument);
    else delete global.document;
  }
});

test('per-Key model detection ignores stale sessions and populates wildcard channel candidates', async () => {
  const checkboxes = [];
  const makeClassList = () => ({
    add() {},
    remove() {},
    toggle() {}
  });
  const list = {
    replaceChildren() { checkboxes.length = 0; },
    appendChild(label) { checkboxes.push(label.children[0]); },
    setAttribute() {}
  };
  const modal = { classList: makeClassList(), setAttribute() {} };
  const allowAll = { checked: true, focus() {} };
  const detectButton = {
    disabled: false,
    attributes: new Map(),
    setAttribute(name, value) { this.attributes.set(name, value); },
    removeAttribute(name) { this.attributes.delete(name); }
  };
  const status = { textContent: '', classList: makeClassList() };
  const elements = {
    keyModelScopeModal: modal,
    keyModelScopeList: list,
    keyModelScopeAll: allowAll,
    keyModelScopeSearch: { value: '' },
    keyModelScopeModalTitle: { textContent: '' },
    keyModelScopeStatus: status,
    detectKeyModelScopeBtn: detectButton,
    channelModal: { setAttribute() {}, removeAttribute() {} }
  };
  const pending = [];
  const globals = {
    window: { t: key => key },
    document: {
      activeElement: { focus() {} },
      getElementById: id => elements[id] || null,
      querySelectorAll: selector => selector === '#keyModelScopeList input[name="keyAllowedModel"]' ? checkboxes : [],
      createElement: tagName => ({
        tagName,
        children: [],
        dataset: {},
        append(...children) { this.children.push(...children); }
      })
    },
    editingChannelAuthType: 'api_key',
    inlineKeyTableData: [{ api_key: 'sk-test', note: '', allowed_models: [] }],
    redirectTableData: [
      { model: 'model-old', redirect_model: '', disabled: false },
      { model: 'model-new', redirect_model: '', disabled: false }
    ],
    getValidInlineURLConfigs: () => [{ url: 'https://upstream.test', exact: false, protocols: [] }],
    fetchAPIWithAuth: () => new Promise(resolve => pending.push(resolve))
  };
  const previous = new Map();
  for (const [name, value] of Object.entries(globals)) {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  }

  try {
    assert.equal(openKeyModelScopeModal(0), true);
    const staleDetection = detectKeyModelScope();
    closeKeyModelScopeModal(false);
    assert.equal(openKeyModelScopeModal(0), true);
    const currentDetection = detectKeyModelScope();

    pending[0]({
      success: true,
      data: { key_models: [{ models: [{ model: 'model-old' }] }] }
    });
    await staleDetection;
    assert.deepEqual(checkboxes.map(checkbox => checkbox.checked), [true, true]);
    assert.equal(detectButton.disabled, true);

    pending[1]({
      success: true,
      data: { key_models: [{ models: [{ model: 'model-new' }] }] }
    });
    await currentDetection;
    assert.deepEqual(checkboxes.map(checkbox => checkbox.checked), [false, true]);
    assert.equal(allowAll.checked, false);
    assert.equal(detectButton.disabled, false);

    closeKeyModelScopeModal(false);
    global.redirectTableData = [{ model: '*', redirect_model: '', disabled: false }];
    assert.equal(openKeyModelScopeModal(0), true);
    assert.deepEqual(checkboxes, []);
    const wildcardDetection = detectKeyModelScope();
    pending[2]({
      success: true,
      data: { key_models: [{ models: [{ model: 'gpt-wildcard' }] }] }
    });
    await wildcardDetection;
    assert.deepEqual(checkboxes.map(checkbox => checkbox.value), ['gpt-wildcard']);
    assert.deepEqual(checkboxes.map(checkbox => checkbox.checked), [true]);
    assert.equal(allowAll.checked, false);
  } finally {
    closeKeyModelScopeModal(false);
    for (const [name, descriptor] of previous) {
      if (descriptor) Object.defineProperty(global, name, descriptor);
      else delete global[name];
    }
  }
});

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
  const makeNumericInput = value => ({
    value,
    disabled: false,
    attributes: new Map(),
    setAttribute(name, attributeValue) { this.attributes.set(name, attributeValue); },
    focus() {}
  });
  const makeButton = () => ({
    disabled: false,
    attributes: new Map(),
    getAttribute(name) { return this.attributes.get(name) || null; },
    setAttribute(name, value) { this.attributes.set(name, value); },
    removeAttribute(name) { this.attributes.delete(name); }
  });
  const elements = {
    batchProtocolTransformMode: { value: 'local', disabled: false },
    batchApplyProtocolBtn: makeButton(),
    batchPriority: makeNumericInput('9999999'),
    batchApplyPriorityBtn: makeButton(),
    batchPriorityError: { textContent: '', hidden: true },
    batchCostMultiplier: makeNumericInput('0.5'),
    batchApplyCostMultiplierBtn: makeButton(),
    batchCostMultiplierError: { textContent: '', hidden: true },
    batchRPMLimit: makeNumericInput('120'),
    batchApplyRPMLimitBtn: makeButton(),
    batchRPMLimitError: { textContent: '', hidden: true },
    batchMaxConcurrency: makeNumericInput('8'),
    batchApplyMaxConcurrencyBtn: makeButton(),
    batchMaxConcurrencyError: { textContent: '', hidden: true },
    batchDailyCostLimit: makeNumericInput('25.5'),
    batchApplyDailyCostLimitBtn: makeButton(),
    batchDailyCostLimitError: { textContent: '', hidden: true },
    batchImportModelsBtn: makeButton(),
    batchClearCooldownsBtn: makeButton(),
    batchAdvancedOptions: { open: true },
    batchFloatingMenu: {
      inert: false,
      attributes: new Map(),
      classList: { toggle() {} },
      getAttribute(name) { return this.attributes.get(name) || null; },
      setAttribute(name, value) { this.attributes.set(name, value); },
      removeAttribute(name) { this.attributes.delete(name); }
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

function installFetchKeyRateGlobals({
  response,
  rows,
  rateConfig = { profile: 'sub2api', base_url: 'https://sub2api.test' }
}) {
  const requests = [];
  const notifications = [];
  const updateCalls = [];
  let dirty = false;
  const makeButton = () => {
    const label = { textContent: '' };
    return {
      disabled: false,
      attributes: new Map(),
      setAttribute(name, value) { this.attributes.set(name, value); },
      removeAttribute(name) { this.attributes.delete(name); },
      querySelector: () => label
    };
  };
  const globals = {
    window: {
      t: (key, params) => params ? { key, params } : key,
      showSuccess: message => notifications.push({ type: 'success', message }),
      showError: message => notifications.push({ type: 'error', message }),
      getManagementAccountRateConfig: () => rateConfig
    },
    document: {
      querySelector: () => null
    },
    getValidInlineURLConfigs: () => [{ url: 'https://sub2api.test/v1', exact: false, protocols: ['openai'] }],
    getInlineKeyValue: i => (rows[i]?.api_key || ''),
    updateInlineKeyCostMultiplier: (index, value) => {
      updateCalls.push({ index, value });
      dirty = true;
    },
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
    notifications,
    requests,
    updateCalls,
    makeButton,
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
    normalizeInlineKeyRow,
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
    document: { getElementById: id => id === 'channelName' ? { value: 'test-channel' } : null },
    channels: [],
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

test('editing an xAI channel loads its full credential into read-only mode and keeps keys empty', async () => {
  const channel = {
    id: 76,
    name: 'xai-oauth',
    auth_type: 'xai_oauth',
    xai_email: 'safe@example.com',
    xai_subscription_tier: 'pro',
    urls: [{ url: 'https://cli-chat-proxy.grok.com/v1', exact: false, protocols: ['codex'] }],
    models: [],
    priority: 0,
    enabled: true,
    protocol_transform_mode: 'local'
  };
  const credential = {
    type: 'xai', auth_kind: 'oauth', access_token: 'xai-at', refresh_token: 'xai-rt', id_token: 'xai-id'
  };
  const fixture = installEditChannelGlobals(channel, {
    editorKeys: [],
    codexCredential: credential,
    codexCredentialInfo: null
  });

  try {
    const { editChannel } = loadChannelsModals();
    await editChannel(channel.id);

    assert.deepEqual(fixture.loadedKeys, []);
    assert.deepEqual(fixture.authEditorCalls.at(-1), {
      authType: 'xai_oauth',
      credential,
      channel,
      credentialInfo: null
    });
  } finally {
    fixture.restore();
  }
});

test('saving an xAI editor preserves xai_oauth and submits no key material', async () => {
  const channel = {
    id: 77,
    name: 'xai-save',
    scheduled_check_enabled: true,
    scheduled_check_interval_minutes: 37,
    scheduled_check_start_time: '08:30',
    auth_type: 'xai_oauth',
    urls: [{ url: 'https://cli-chat-proxy.grok.com/v1', exact: false, protocols: ['codex'] }],
    models: [],
    enabled: true,
    protocol_transform_mode: 'local'
  };
  const fixture = installEditChannelGlobals(channel, { editorKeys: [] });
  const extraGlobals = new Map();
  const setGlobal = (key, value) => {
    extraGlobals.set(key, Object.getOwnPropertyDescriptor(global, key));
    Object.defineProperty(global, key, { configurable: true, writable: true, value });
  };
  let submitted;

  try {
    const { editChannel, saveChannel } = loadChannelsModals();
    await editChannel(channel.id);
    global.redirectTableData.push({ model: 'grok-4', redirect_model: '' });
    fixture.getElement('channelName').value = channel.name;
    fixture.getElement('channelApiKey').value = 'must-be-cleared';
    fixture.getElement('protocolTransformModeValue').value = 'local';
    fixture.getElement('channelEnabled').checked = true;
    for (const id of [
      'channelPriority', 'channelRPMLimit', 'channelMaxConcurrency', 'channelDailyCostLimit',
      'channelScheduledCheckModel', 'channelProxyURL'
    ]) fixture.getElement(id).value = '0';
    setGlobal('getValidInlineURLConfigs', () => channel.urls);
    setGlobal('getValidInlineKeyRows', () => [{ api_key: 'must-not-submit', note: 'secret' }]);
    setGlobal('fetchAPIWithAuth', async (_url, options) => {
      submitted = JSON.parse(options.body);
      return { success: false, error: 'captured' };
    });

    await saveChannel({ preventDefault() {} });
    assert.equal(submitted.auth_type, 'xai_oauth');
    assert.equal(fixture.getElement('channelScheduledCheckIntervalMinutes').value, 37);
    assert.equal(fixture.getElement('channelScheduledCheckStartTime').value, '08:30');
    assert.equal(submitted.scheduled_check_enabled, true);
    assert.equal(submitted.scheduled_check_interval_minutes, 37);
    assert.equal(submitted.scheduled_check_start_time, '08:30');
    assert.equal(submitted.api_key, '');
    assert.deepEqual(submitted.api_keys, []);
    assert.equal(submitted.key_strategy, undefined);
    assert.equal(submitted.oauth_credential, undefined);
    assert.equal(submitted.credential, undefined);
    assert.equal(submitted.access_token, undefined);
    assert.equal(submitted.refresh_token, undefined);
    assert.equal(submitted.id_token, undefined);
  } finally {
    for (const [key, descriptor] of extraGlobals) {
      if (descriptor === undefined) delete global[key];
      else Object.defineProperty(global, key, descriptor);
    }
    fixture.restore();
  }
});

test('saving rejects invalid daily schedules and focuses the field with an inline error', async () => {
  const channel = { id: 80, name: 'invalid-schedule', auth_type: 'api_key', urls: [{ url: 'https://example.com' }], models: [] };
  const fixture = installEditChannelGlobals(channel, { editorKeys: [] });
  try {
    const { editChannel, saveChannel } = loadChannelsModals();
    await editChannel(channel.id);
    for (const [id, value] of [
      ['channelScheduledCheckIntervalMinutes', '0'],
      ['channelScheduledCheckIntervalMinutes', '1.5'],
      ['channelScheduledCheckIntervalMinutes', '1441'],
      ['channelScheduledCheckStartTime', '8:30']
    ]) {
      fixture.getElement('channelScheduledCheckIntervalMinutes').value = '30';
      fixture.getElement('channelScheduledCheckStartTime').value = '08:30';
      const input = fixture.getElement(id);
      input.id = id;
      input.value = value;
      let focused = false;
      input.focus = () => { focused = true; };
      await saveChannel({ preventDefault() {} });
      assert.equal(focused, true);
      assert.equal(fixture.getElement(`${id}Error`).hidden, false);
      assert.ok(fixture.getElement(`${id}Error`).textContent);
    }
    assert.deepEqual(fixture.requests, ['/admin/channels/80/editor']);
  } finally {
    fixture.restore();
  }
});

test('saving an OAuth editor submits the multiplier through the synthetic key row', async () => {
  const channel = {
    id: 79,
    name: 'anth-oauth-save',
    auth_type: 'anthropic_oauth',
    urls: [{ url: 'https://api.anthropic.com', exact: false, protocols: ['anthropic'] }],
    models: [],
    enabled: true,
    protocol_transform_mode: 'local'
  };
  const fixture = installEditChannelGlobals(channel, { editorKeys: [] });
  const extraGlobals = new Map();
  const setGlobal = (key, value) => {
    extraGlobals.set(key, Object.getOwnPropertyDescriptor(global, key));
    Object.defineProperty(global, key, { configurable: true, writable: true, value });
  };
  let submitted;

  try {
    const { editChannel, saveChannel } = loadChannelsModals();
    await editChannel(channel.id);
    global.inlineKeyTableData = [{ api_key: 'sk-ant-masked-credential', cost_multiplier: 3 }];
    global.redirectTableData.push({ model: 'claude-sonnet-5', redirect_model: '' });
    fixture.getElement('channelName').value = channel.name;
    fixture.getElement('channelApiKey').value = '';
    fixture.getElement('protocolTransformModeValue').value = 'local';
    fixture.getElement('channelEnabled').checked = true;
    setGlobal('getValidInlineURLConfigs', () => channel.urls);
    setGlobal('getValidInlineKeyRows', () => []);
    setGlobal('fetchAPIWithAuth', async (_url, options) => {
      submitted = JSON.parse(options.body);
      return { success: false, error: 'captured' };
    });

    await saveChannel({ preventDefault() {} });
    assert.equal(submitted.auth_type, 'anthropic_oauth');
    assert.deepEqual(submitted.api_keys, [{ api_key: 'sk-ant-masked-credential', cost_multiplier: 3 }]);
    assert.equal(submitted.cost_multiplier, undefined);
    assert.equal(submitted.key_strategy, undefined);
  } finally {
    for (const [key, descriptor] of extraGlobals) {
      if (descriptor === undefined) delete global[key];
      else Object.defineProperty(global, key, descriptor);
    }
    fixture.restore();
  }
});

test('saving an API Key channel submits per-Key model scopes', async () => {
  const channel = {
    id: 78,
    name: 'scoped-key-save',
    auth_type: 'api_key',
    urls: [{ url: 'https://api.example.com', exact: false, protocols: ['openai'] }],
    models: [],
    enabled: true,
    protocol_transform_mode: 'auto'
  };
  const fixture = installEditChannelGlobals(channel, { editorKeys: [] });
  const extraGlobals = new Map();
  const setGlobal = (key, value) => {
    extraGlobals.set(key, Object.getOwnPropertyDescriptor(global, key));
    Object.defineProperty(global, key, { configurable: true, writable: true, value });
  };
  let submitted;

  try {
    const { editChannel, saveChannel } = loadChannelsModals();
    await editChannel(channel.id);
    global.redirectTableData.push({ model: 'gpt-5', redirect_model: '' });
    fixture.getElement('channelName').value = channel.name;
    fixture.getElement('channelApiKey').value = 'sk-scoped';
    fixture.getElement('protocolTransformModeValue').value = 'auto';
    fixture.getElement('channelEnabled').checked = true;
    for (const id of [
      'channelPriority', 'channelRPMLimit', 'channelMaxConcurrency', 'channelDailyCostLimit',
      'channelScheduledCheckModel', 'channelProxyURL'
    ]) fixture.getElement(id).value = '0';
    setGlobal('getValidInlineURLConfigs', () => channel.urls);
    setGlobal('getValidInlineKeyRows', () => [
      { api_key: 'sk-scoped', note: 'primary', allowed_models: ['gpt-5'], cost_multiplier: 2 },
      { api_key: 'sk-emptied', note: '', allowed_models: [], model_scope_empty: true, cost_multiplier: 1 }
    ]);
    setGlobal('fetchAPIWithAuth', async (_url, options) => {
      submitted = JSON.parse(options.body);
      return { success: false, error: 'captured' };
    });

    await saveChannel({ preventDefault() {} });
	assert.deepEqual(submitted.api_keys, [
		{ api_key: 'sk-scoped', note: 'primary', allowed_models: ['gpt-5'], model_scope_empty: false, cost_multiplier: 2 },
      { api_key: 'sk-emptied', note: '', allowed_models: [], model_scope_empty: true, cost_multiplier: 1 }
    ]);
  } finally {
    for (const [key, descriptor] of extraGlobals) {
      if (descriptor === undefined) delete global[key];
      else Object.defineProperty(global, key, descriptor);
    }
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

test('model export uses selected rows and falls back to all rows when nothing is selected', () => {
  const { getModelsForExport } = loadChannelsModals();
  const rows = [
    { model: 'gpt-a', redirect_model: 'upstream-a' },
    { model: 'gpt-b', disabled: true },
    { model: 'claude' }
  ];

  assert.deepEqual(getModelsForExport(rows, new Set([1])), [{
    model: 'gpt-b', redirect_model: '', disabled: true
  }]);
  assert.deepEqual(getModelsForExport(rows, new Set()), [
    { model: 'gpt-a', redirect_model: 'upstream-a', disabled: false },
    { model: 'gpt-b', redirect_model: '', disabled: true },
    { model: 'claude', redirect_model: '', disabled: false }
  ]);
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
      { type: 'open', args: [{
        id: 7,
        name: 'test-channel',
        models: [{ model: 'requested-model', redirect_model: 'upstream-model', disabled: false }]
      }, 'requested-model'] },
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
  assert.equal(requestBody.per_key, true);
  assert.deepEqual(requestBody.urls, [{ url: 'https://upstream.test', exact: false, protocols: ['openai'] }]);
});

test('fetchModelsFromAPI uses the earliest recovery key when all enabled keys are cooling', async () => {
  let requestBody;
  const restore = installFetchModelsGlobals({
    rows: [
      { api_key: 'disabled-key' },
      { api_key: 'cooling-late' },
      { api_key: 'cooling-soon' },
      { api_key: 'cooling-soon-higher-index' }
    ],
    states: [
      { key_index: 0, disabled: true },
      { key_index: 1, disabled: false, cooldown_remaining_ms: 60_000 },
      { key_index: 2, disabled: false, cooldown_remaining_ms: 10_000 },
      { key_index: 3, disabled: false, cooldown_remaining_ms: 10_000 }
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

  assert.deepEqual(requestBody.api_keys, ['cooling-soon']);
  assert.equal(requestBody.per_key, true);
  assert.deepEqual(requestBody.urls, [{ url: 'https://upstream.test', exact: false, protocols: ['openai'] }]);
});

test('per-Key discovery proposals map out-of-order results back to original Key rows', () => {
  const { proposeFetchedKeyModelScopes } = loadChannelsModals();
  const result = proposeFetchedKeyModelScopes([
    { api_key: 'sk-a', note: '', allowed_models: [] },
    { api_key: 'sk-b', note: '', allowed_models: [] }
  ], [
    { model: 'logical-a', redirect_model: 'upstream-a' },
    { model: 'common', redirect_model: '' },
    { model: 'logical-b', redirect_model: 'upstream-b' }
  ], [
    { key_index: 1, models: [{ model: 'upstream-b' }, { model: 'common' }] },
    { key_index: 0, models: [{ model: 'upstream-a' }, { model: 'common' }] }
  ], [
    { keyIndex: 0, apiKey: 'sk-a' },
    { keyIndex: 1, apiKey: 'sk-b' }
  ]);

  assert.equal(result.changedCount, 2);
  assert.equal(result.matchedCount, 2);
  assert.equal(result.unmatchedCount, 0);
  assert.equal(result.complete, true);
  assert.deepEqual(result.rows.map(row => row.allowed_models), [
    ['logical-a', 'common'],
    ['common', 'logical-b']
  ]);
});

test('per-Key discovery applies each provider scope independently', () => {
  const { proposeFetchedKeyModelScopes } = loadChannelsModals();
  const result = proposeFetchedKeyModelScopes([
    { api_key: 'micu-codex', allowed_models: [] },
    { api_key: 'micu-claude', allowed_models: [] }
  ], [
    { model: 'gpt-5.4' },
    { model: 'gpt-5.4-mini' },
    { model: 'gpt-5.5' },
    { model: 'gpt-5.6-sol' },
    { model: 'gpt-5.6-terra' }
  ], [
    { key_index: 1, models: [
      { model: 'claude-fable-5' },
      { model: 'claude-haiku-4-5-20251001' },
      { model: 'claude-opus-4-6' },
      { model: 'claude-opus-4-8' },
      { model: 'claude-opus-5' },
      { model: 'claude-sonnet-4-6' },
      { model: 'claude-sonnet-5' }
    ] },
    { key_index: 0, models: [
      { model: 'codex-auto-review' },
      { model: 'gpt-5.3-codex-spark' },
      { model: 'gpt-5.4' },
      { model: 'gpt-5.4-mini' },
      { model: 'gpt-5.5' },
      { model: 'gpt-5.6-sol' },
      { model: 'gpt-5.6-terra' }
    ] }
  ], [
    { keyIndex: 0, apiKey: 'micu-codex' },
    { keyIndex: 1, apiKey: 'micu-claude' }
  ]);

  assert.equal(result.complete, false);
  assert.equal(result.changedCount, 2);
  assert.equal(result.matchedCount, 1);
  assert.equal(result.unmatchedCount, 1);
  assert.equal(result.failedCount, 0);
  assert.deepEqual(result.rows, [
    {
      api_key: 'micu-codex',
      allowed_models: ['gpt-5.4', 'gpt-5.4-mini', 'gpt-5.5', 'gpt-5.6-sol', 'gpt-5.6-terra']
    },
    { api_key: 'micu-claude', allowed_models: [], model_scope_empty: true }
  ]);
});

test('per-Key discovery clears scope-empty marker when a scope is recovered', () => {
  const { proposeFetchedKeyModelScopes } = loadChannelsModals();
  const result = proposeFetchedKeyModelScopes([
    { api_key: 'sk-scope-empty', allowed_models: [], model_scope_empty: true }
  ], [
    { model: 'logical-model', redirect_model: '' }
  ], [
    { key_index: 0, models: [{ model: 'logical-model' }] }
  ], [
    { keyIndex: 0, apiKey: 'sk-scope-empty' }
  ]);

  assert.equal(result.complete, true);
  assert.equal(result.changedCount, 1);
  assert.deepEqual(result.rows, [{
    api_key: 'sk-scope-empty',
    allowed_models: ['logical-model']
  }]);
});

test('per-Key discovery marks failed Keys empty while applying successful Keys', () => {
  const { proposeFetchedKeyModelScopes } = loadChannelsModals();
  const result = proposeFetchedKeyModelScopes([
    { api_key: 'sk-disabled', note: '', allowed_models: ['keep-disabled'] },
    { api_key: 'sk-a', note: '', allowed_models: [] },
    { api_key: 'sk-cooling', note: '', allowed_models: ['keep-cooling'] },
    { api_key: 'sk-b', note: '', allowed_models: [] }
  ], [
    { model: 'logical-a', redirect_model: 'upstream-a' },
    { model: 'common', redirect_model: '' },
    { model: 'logical-b', redirect_model: 'upstream-b' }
  ], [
    { key_index: 1, error: 'invalid api key', models: [] },
    { key_index: 3, models: [{ model: 'upstream-b' }, { model: 'common' }] }
  ], [
    { keyIndex: 1, apiKey: 'sk-a' },
    { keyIndex: 3, apiKey: 'sk-b' }
  ]);

  assert.equal(result.changedCount, 2);
  assert.equal(result.matchedCount, 1);
  assert.equal(result.unmatchedCount, 0);
  assert.equal(result.failedCount, 1);
  assert.equal(result.complete, false);
  assert.deepEqual(result.rows, [
    { api_key: 'sk-disabled', note: '', allowed_models: ['keep-disabled'] },
    { api_key: 'sk-a', note: '', allowed_models: [], model_scope_empty: true },
    { api_key: 'sk-cooling', note: '', allowed_models: ['keep-cooling'] },
    { api_key: 'sk-b', note: '', allowed_models: ['common', 'logical-b'] }
  ]);
});

test('per-Key discovery marks a successful Key with no matching model as empty', () => {
  const { proposeFetchedKeyModelScopes } = loadChannelsModals();
  const result = proposeFetchedKeyModelScopes(
    [{ api_key: 'sk-a', note: '', allowed_models: ['keep-me'] }],
    [{ model: 'configured-model', redirect_model: '' }],
    [{ key_index: 0, models: [{ model: 'unknown-upstream-model' }] }],
    [{ keyIndex: 0, apiKey: 'sk-a' }]
  );

  assert.equal(result.changedCount, 1);
  assert.equal(result.matchedCount, 0);
  assert.equal(result.unmatchedCount, 1);
  assert.equal(result.complete, false);
  assert.deepEqual(result.rows[0], {
    api_key: 'sk-a', note: '', allowed_models: [], model_scope_empty: true
  });
});

test('per-Key discovery keeps skipped manual Keys unchanged', () => {
  const { proposeFetchedKeyModelScopes } = loadChannelsModals();
  const result = proposeFetchedKeyModelScopes([
    { api_key: 'sk-disabled', note: '', allowed_models: ['keep-disabled'] },
    { api_key: 'sk-a', note: '', allowed_models: [] }
  ], [
    { model: 'logical-a', redirect_model: 'upstream-a' }
  ], [
    { key_index: 1, models: [{ model: 'upstream-a' }] }
  ], [
    { keyIndex: 1, apiKey: 'sk-a' }
  ]);

  assert.deepEqual(result.rows.map(row => row.allowed_models), [
    ['keep-disabled'],
    ['logical-a']
  ]);
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

test('fetchKeyRate uses the Sub2API management profile and updates the triggering key row', async () => {
  const fixture = installFetchKeyRateGlobals({
    response: { success: true, data: { effective_rate_multiplier: 1.2 } },
    rows: [{ api_key: 'enabled-key' }]
  });

  try {
    const { fetchKeyRate } = loadChannelsModals();
    const actionBtn = fixture.makeButton();
    await fetchKeyRate(0, actionBtn);

    assert.equal(fixture.requests.length, 1);
    assert.equal(fixture.requests[0].url, '/admin/channels/billing/fetch');
    assert.deepEqual(JSON.parse(fixture.requests[0].options.body), {
      profile: 'sub2api',
      base_url: 'https://sub2api.test',
      api_key: 'enabled-key'
    });
    assert.deepEqual(fixture.updateCalls, [{ index: 0, value: '1.2' }]);
    assert.equal(actionBtn.disabled, false);
    assert.equal(fixture.dirty, true);
    assert.deepEqual(fixture.notifications, [{
      type: 'success',
      message: { key: 'channels.fetchRateSuccess', params: { rate: '1.2' } }
    }]);
  } finally {
    fixture.restore();
  }
});

test('fetchKeyRate sends New API management credentials for group multiplier lookup', async () => {
  const fixture = installFetchKeyRateGlobals({
    response: { success: true, data: { effective_rate_multiplier: 0.75 } },
    rows: [{ api_key: 'new-api-key' }],
    rateConfig: {
      profile: 'new_api',
      base_url: 'https://new-api.test',
      access_token: 'management-pat',
      user_id: 42
    }
  });

  try {
    const { fetchKeyRate } = loadChannelsModals();
    await fetchKeyRate(0, fixture.makeButton());

    assert.deepEqual(JSON.parse(fixture.requests[0].options.body), {
      profile: 'new_api',
      base_url: 'https://new-api.test',
      api_key: 'new-api-key',
      access_token: 'management-pat',
      user_id: 42
    });
    assert.deepEqual(fixture.updateCalls, [{ index: 0, value: '0.75' }]);
  } finally {
    fixture.restore();
  }
});

test('fetchKeyRate maps authentication failures without touching the row', async () => {
  const fixture = installFetchKeyRateGlobals({
    response: { success: false, data: { code: 'authentication_error' } },
    rows: [{ api_key: 'invalid-key' }]
  });

  try {
    const { fetchKeyRate } = loadChannelsModals();
    const actionBtn = fixture.makeButton();
    await fetchKeyRate(0, actionBtn);

    assert.equal(fixture.updateCalls.length, 0);
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

test('batch clear cooldowns submits selected channel IDs and refreshes the list', async () => {
  const fixture = installBatchProtocolModeGlobals({
    success: true,
    data: { cleared: 2, not_found_count: 0 }
  });

  try {
    const { batchClearSelectedChannelCooldowns } = loadChannelsModals();
    await batchClearSelectedChannelCooldowns();

    assert.equal(fixture.requests.length, 1);
    assert.equal(fixture.requests[0].url, '/admin/channels/batch-clear-cooldowns');
    assert.equal(fixture.requests[0].options.method, 'POST');
    assert.deepEqual(JSON.parse(fixture.requests[0].options.body), {
      channel_ids: [11, 22]
    });
    assert.equal(fixture.selectedChannelIds.size, 0);
    assert.equal(fixture.filterSaves, 1);
    assert.equal(fixture.reloads, 1);
    assert.deepEqual(fixture.notifications, [{
      type: 'success',
      message: {
        key: 'channels.batchClearCooldownsSummary',
        params: { cleared: 2, notFound: 0 }
      }
    }]);
    assert.equal(fixture.elements.batchClearCooldownsBtn.getAttribute('aria-busy'), null);
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

for (const testCase of [
  {
    name: 'priority',
    exportName: 'batchSetSelectedChannelsPriority',
    field: 'priority',
    value: 9999999,
    summaryKey: 'channels.batchPrioritySummary'
  },
  {
    name: 'RPM',
    exportName: 'batchSetSelectedChannelsRPMLimit',
    field: 'rpm_limit',
    value: 120,
    summaryKey: 'channels.batchRPMLimitSummary'
  },
  {
    name: 'max concurrency',
    exportName: 'batchSetSelectedChannelsMaxConcurrency',
    field: 'max_concurrency',
    value: 8,
    summaryKey: 'channels.batchMaxConcurrencySummary'
  },
  {
    name: 'daily cost limit',
    exportName: 'batchSetSelectedChannelsDailyCostLimit',
    field: 'daily_cost_limit',
    value: 25.5,
    summaryKey: 'channels.batchDailyCostLimitSummary'
  }
]) {
  test(`batch ${testCase.name} submits only its selected channel patch`, async () => {
    const fixture = installBatchProtocolModeGlobals({
      success: true,
      data: { updated: 2, unchanged: 0, not_found_count: 0 }
    });

    try {
      const handler = loadChannelsModals()[testCase.exportName];
      await handler();

      assert.equal(fixture.requests.length, 1);
      assert.deepEqual(JSON.parse(fixture.requests[0].options.body), {
        channel_ids: [11, 22],
        [testCase.field]: testCase.value
      });
      assert.equal(fixture.selectedChannelIds.size, 0);
      assert.equal(fixture.filterSaves, 1);
      assert.equal(fixture.reloads, 1);
      assert.deepEqual(fixture.notifications, [{
        type: 'success',
        message: {
          key: testCase.summaryKey,
          params: {
            updated: 2,
            unchanged: 0,
            notFound: 0,
            value: testCase.value
          }
        }
      }]);
    } finally {
      fixture.restore();
    }
  });
}

test('batch RPM rejects fractional values without sending a request', async () => {
  const fixture = installBatchProtocolModeGlobals({ success: true, data: {} });
  fixture.elements.batchRPMLimit.value = '1.5';

  try {
    const { batchSetSelectedChannelsRPMLimit } = loadChannelsModals();
    await batchSetSelectedChannelsRPMLimit();

    assert.equal(fixture.requests.length, 0);
    assert.equal(fixture.selectedChannelIds.size, 2);
    assert.equal(fixture.elements.batchRPMLimit.attributes.get('aria-invalid'), 'true');
    assert.equal(fixture.elements.batchRPMLimitError.hidden, false);
  } finally {
    fixture.restore();
  }
});

test('batch priority rejects values outside the editor range', async () => {
  const fixture = installBatchProtocolModeGlobals({ success: true, data: {} });
  fixture.elements.batchPriority.value = '10000000';

  try {
    const { batchSetSelectedChannelsPriority } = loadChannelsModals();
    await batchSetSelectedChannelsPriority();

    assert.equal(fixture.requests.length, 0);
    assert.equal(fixture.selectedChannelIds.size, 2);
    assert.equal(fixture.elements.batchPriority.attributes.get('aria-invalid'), 'true');
    assert.equal(fixture.elements.batchPriorityError.hidden, false);
  } finally {
    fixture.restore();
  }
});

test('batch numeric setting marks the whole selection menu busy until the request finishes', async () => {
  let resolveResponse;
  const pendingResponse = new Promise(resolve => { resolveResponse = resolve; });
  const fixture = installBatchProtocolModeGlobals(pendingResponse);

  try {
    const { batchSetSelectedChannelsRPMLimit } = loadChannelsModals();
    const operation = batchSetSelectedChannelsRPMLimit();
    await Promise.resolve();

    assert.equal(fixture.elements.batchFloatingMenu.getAttribute('aria-busy'), 'true');
    assert.equal(fixture.elements.batchApplyProtocolBtn.disabled, true);
    assert.equal(fixture.elements.batchApplyMaxConcurrencyBtn.disabled, true);
    assert.equal(fixture.elements.batchMaxConcurrency.disabled, true);

    resolveResponse({ success: true, data: { updated: 2, unchanged: 0, not_found_count: 0 } });
    await operation;

    assert.equal(fixture.elements.batchFloatingMenu.getAttribute('aria-busy'), null);
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


test('渠道保存载荷仅在 API Key 渠道携带 management_account，且绝不携带 oauth_credential', () => {
  const previousWindow = Object.getOwnPropertyDescriptor(global, 'window');
  let collected = null;
  Object.defineProperty(global, 'window', {
    configurable: true,
    writable: true,
    value: { collectManagementAccountForSubmit: () => collected }
  });
  try {
    const { applyChannelManagementPayload } = loadChannelsModals();

    const oauthPayload = applyChannelManagementPayload({ name: 'oauth', auth_type: 'codex_oauth' });
    assert.equal('management_account' in oauthPayload, false, 'OAuth 渠道必须完全省略该键');

    collected = { profile: '' };
    const clearedPayload = applyChannelManagementPayload({ name: 'cleared', auth_type: 'api_key' });
    assert.deepEqual(clearedPayload.management_account, { profile: '' });

    collected = {
      profile: 'new_api',
      base_url: 'https://panel.example.com',
      access_token: 'pat',
      user_id: 42,
      daily_checkin_enabled: true,
      daily_checkin_time: '09:30'
    };
    const payload = applyChannelManagementPayload({ name: 'managed', auth_type: 'api_key' });
    assert.deepEqual(payload.management_account, collected);
    for (const forbidden of ['oauth_credential', 'credential', 'access_token']) {
      assert.equal(forbidden in payload, false, `${forbidden} 不能出现在渠道保存载荷`);
    }
  } finally {
    if (previousWindow) Object.defineProperty(global, 'window', previousWindow);
    else delete global.window;
  }
});

test('multi-Key channel asks for confirmation even when a single Key changed', () => {
  const previousWindow = Object.getOwnPropertyDescriptor(global, 'window');
  const prompts = [];
  Object.defineProperty(global, 'window', {
    configurable: true,
    value: {
      t: (key, params) => `${key}:${params?.count ?? ''}`,
      confirm: message => { prompts.push(message); return true; }
    }
  });
  try {
    const { fetchedKeyModelApplyAccepted } = loadChannelsModals();
    assert.equal(fetchedKeyModelApplyAccepted(1), true);
    assert.deepEqual(prompts, ['channels.applyFetchedKeyModelsConfirm:1']);
  } finally {
    if (previousWindow) Object.defineProperty(global, 'window', previousWindow);
    else delete global.window;
  }
});

test('multi Key model scope detection asks for confirmation with the changed count', () => {
  const previousWindow = Object.getOwnPropertyDescriptor(global, 'window');
  const prompts = [];
  Object.defineProperty(global, 'window', {
    configurable: true,
    value: {
      t: (key, params) => `${key}:${params?.count ?? ''}`,
      confirm: message => { prompts.push(message); return true; }
    }
  });
  try {
    const { fetchedKeyModelApplyAccepted } = loadChannelsModals();
    assert.equal(fetchedKeyModelApplyAccepted(2), true);
    assert.deepEqual(prompts, ['channels.applyFetchedKeyModelsConfirm:2']);
  } finally {
    if (previousWindow) Object.defineProperty(global, 'window', previousWindow);
    else delete global.window;
  }
});

test('multi Key model scope detection respects a declined confirm prompt', () => {
  const previousWindow = Object.getOwnPropertyDescriptor(global, 'window');
  Object.defineProperty(global, 'window', {
    configurable: true,
    value: {
      t: key => key,
      confirm: () => false
    }
  });
  try {
    const { fetchedKeyModelApplyAccepted } = loadChannelsModals();
    assert.equal(fetchedKeyModelApplyAccepted(2), false);
  } finally {
    if (previousWindow) Object.defineProperty(global, 'window', previousWindow);
    else delete global.window;
  }
});

test('multi Key model scope detection never applies without a confirm function', () => {
  const previousWindow = Object.getOwnPropertyDescriptor(global, 'window');
  Object.defineProperty(global, 'window', {
    configurable: true,
    value: { t: key => key }
  });
  try {
    const { fetchedKeyModelApplyAccepted } = loadChannelsModals();
    assert.equal(fetchedKeyModelApplyAccepted(2), false);
    assert.equal(fetchedKeyModelApplyAccepted(1), false, '多 Key 渠道无 confirm 时单个 Key 变更也不应用');
  } finally {
    if (previousWindow) Object.defineProperty(global, 'window', previousWindow);
    else delete global.window;
  }
});

test('single-Key channel never applies fetched Key model scopes', () => {
  const previousWindow = Object.getOwnPropertyDescriptor(global, 'window');
  let confirmCalls = 0;
  Object.defineProperty(global, 'window', {
    configurable: true,
    value: {
      t: key => key,
      confirm: () => { confirmCalls++; return true; }
    }
  });
  try {
    const { fetchedKeyModelApplyAccepted } = loadChannelsModals();
    // 单 Key 渠道没有分流需求,不应把模型范围写进唯一 Key,也不弹确认框
    assert.equal(fetchedKeyModelApplyAccepted(1, true), false);
    assert.equal(fetchedKeyModelApplyAccepted(2, true), false);
    assert.equal(confirmCalls, 0);
  } finally {
    if (previousWindow) Object.defineProperty(global, 'window', previousWindow);
    else delete global.window;
  }
});
