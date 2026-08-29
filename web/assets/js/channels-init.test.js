const test = require('node:test');
const assert = require('node:assert/strict');

test('channel page size accepts whole numbers from 1 to 1000 and otherwise defaults to 20', () => {
  const previousLocalStorage = Object.getOwnPropertyDescriptor(global, 'localStorage');
  Object.defineProperty(global, 'localStorage', {
    configurable: true,
    writable: true,
    value: { getItem: () => null }
  });

  try {
    delete require.cache[require.resolve('./channels-state.js')];
    const { normalizeChannelsPageSize } = require('./channels-state.js');

    assert.equal(normalizeChannelsPageSize('1'), 1);
    assert.equal(normalizeChannelsPageSize('37'), 37);
    assert.equal(normalizeChannelsPageSize('1000'), 1000);
    for (const value of [null, '', '0', '-1', '1.5', '1001', 'not-a-number']) {
      assert.equal(normalizeChannelsPageSize(value), 20, `value ${String(value)}`);
    }
  } finally {
    delete require.cache[require.resolve('./channels-state.js')];
    if (previousLocalStorage === undefined) delete global.localStorage;
    else Object.defineProperty(global, 'localStorage', previousLocalStorage);
  }
});

test('dirty channel form keeps the save action as save', () => {
  const previousGlobals = new Map();
  const setGlobal = (name, value) => {
    previousGlobals.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  };
  const classes = new Set(['btn-primary']);
  const label = {
    textContent: '',
    setAttribute(name, value) { this[name] = value; }
  };
  const saveButton = {
    classList: {
      add: (...names) => names.forEach(name => classes.add(name)),
      remove: (...names) => names.forEach(name => classes.delete(name)),
      contains: name => classes.has(name)
    }
  };
  setGlobal('localStorage', { getItem: () => null });
  setGlobal('document', {
    getElementById: id => ({ channelSaveBtn: saveButton, channelSaveLabel: label }[id] || null)
  });
  setGlobal('window', {
    t: key => ({ 'common.save': '保存' })[key] || key
  });

  const modulePath = require.resolve('./channels-state.js');
  delete require.cache[modulePath];
  try {
    const mod = require(modulePath);
    mod.resetChannelFormDirty();
    assert.equal(label.textContent, '保存');
    assert.equal(label['data-i18n'], 'common.save');

    mod.markChannelFormDirty();
    assert.equal(label.textContent, '保存 *');
    assert.equal(label['data-i18n'], 'common.save');
  } finally {
    delete require.cache[modulePath];
    for (const [name, descriptor] of previousGlobals) {
      if (descriptor) Object.defineProperty(global, name, descriptor);
      else delete global[name];
    }
  }
});

test('returning via reload or bfcache restores channel name search with the other filters', async () => {
  const previousGlobals = new Map();
  const setGlobal = (key, value) => {
    previousGlobals.set(key, Object.getOwnPropertyDescriptor(global, key));
    Object.defineProperty(global, key, { configurable: true, writable: true, value });
  };

  const savedState = {
    status: 'enabled',
    authType: 'xai_oauth',
    model: 'claude-opus-5',
    modelExact: true,
    search: 'any',
    searchExact: false,
    page: 2
  };
  const storage = new Map([['channels.filters', JSON.stringify(savedState)]]);
  const elements = {
    statusFilter: { value: 'all' },
    channelAuthTypeFilter: { value: '' },
    modelFilter: { value: '' },
    searchInput: { value: '' }
  };
  const windowListeners = {};
  let bootstrap;
  let loadedFilters;

  setGlobal('window', {
    t: (key) => key === 'channels.channelNameAll' ? '所有渠道' : key,
    initPageBootstrap: (config) => { bootstrap = config; },
    addEventListener: (type, handler) => { windowListeners[type] = handler; },
    i18n: { onLocaleChange() {} }
  });
  setGlobal('document', {
    body: { classList: { toggle() {} } },
    addEventListener() {},
    getElementById: (id) => elements[id] || null
  });
  setGlobal('localStorage', {
    getItem: (key) => storage.get(key) ?? null,
    setItem: (key, value) => storage.set(key, String(value))
  });
  setGlobal('location', { search: '', hash: '' });
  setGlobal('filters', {
    search: '',
    searchExact: false,
    status: 'all',
    authType: 'all',
    model: 'all',
    modelExact: false
  });
  setGlobal('channelsCurrentPage', 1);
  setGlobal('channelsPageSize', 20);
  setGlobal('channelStatsRange', 'today');
  setGlobal('isTokenChannelsReadOnly', () => false);
  setGlobal('setupFilterListeners', () => {});
  setGlobal('setupImportExport', () => {});
  setGlobal('setupKeyImportPreview', () => {});
  setGlobal('setupModelImportPreview', () => {});
  setGlobal('ensureProtocolTransformModeCombobox', async () => {});
  setGlobal('loadDefaultTestContent', async () => {});
  setGlobal('loadChannelStatsRange', async () => {});
  setGlobal('loadChannelsFilterOptions', async () => {});
  setGlobal('loadChannels', async () => {
    loadedFilters = { ...global.filters };
  });
  setGlobal('loadChannelStats', async () => {});
  setGlobal('modelFilterInputValueFromFilterValue', (value) => value);
  setGlobal('channelAuthTypeFilterLabel', (value) => value);

  try {
    delete require.cache[require.resolve('./channels-init.js')];
    require('./channels-init.js');
    await bootstrap.run();

    assert.deepEqual(loadedFilters, {
      search: 'any',
      searchExact: false,
      status: 'enabled',
      authType: 'xai_oauth',
      model: 'claude-opus-5',
      modelExact: true
    });
    assert.equal(elements.searchInput.value, 'any');
    assert.equal(elements.channelAuthTypeFilter.value, 'xai_oauth');
    assert.deepEqual(JSON.parse(storage.get('channels.filters')), {
      status: 'enabled',
      authType: 'xai_oauth',
      model: 'claude-opus-5',
      modelExact: true,
      search: 'any',
      searchExact: false,
      page: 2
    });

    await windowListeners.pageshow({ persisted: true });

    assert.equal(loadedFilters.search, 'any');
    assert.equal(elements.searchInput.value, 'any');
    assert.equal(JSON.parse(storage.get('channels.filters')).search, 'any');
  } finally {
    delete require.cache[require.resolve('./channels-init.js')];
    for (const [key, descriptor] of previousGlobals) {
      if (descriptor === undefined) delete global[key];
      else Object.defineProperty(global, key, descriptor);
    }
  }
});

test('selecting the xAI auth type keeps the filter and reloads with it', () => {
  const previousGlobals = new Map();
  const setGlobal = (key, value) => {
    previousGlobals.set(key, Object.getOwnPropertyDescriptor(global, key));
    Object.defineProperty(global, key, { configurable: true, writable: true, value });
  };
  const elements = new Map([
    ['statusFilter', { addEventListener() {} }],
    ['channelAuthTypeFilter', {}],
    ['modelFilter', {}],
    ['searchInput', {}],
    ['btn_filter', { addEventListener() {} }]
  ]);
  let authCombobox;
  let loadedAuthType = '';

  setGlobal('window', { t: key => key });
  setGlobal('document', { getElementById: id => elements.get(id) || null });
  setGlobal('filters', { status: 'all', authType: 'all', model: 'all', search: '' });
  setGlobal('channelsCurrentPage', 4);
  setGlobal('allAvailableModels', []);
  setGlobal('allAvailableChannelNames', []);
  setGlobal('createSearchableCombobox', options => {
    if (options.inputId === 'channelAuthTypeFilter') authCombobox = options;
    return { setValue() {}, refresh() {} };
  });
  setGlobal('saveChannelsFilters', () => {});
  setGlobal('loadChannels', () => { loadedAuthType = global.filters.authType; });

  try {
    delete require.cache[require.resolve('./channels-filters.js')];
    const { setupFilterListeners } = require('./channels-filters.js');
    setupFilterListeners();
    authCombobox.onSelect('xai_oauth');

    assert.equal(global.filters.authType, 'xai_oauth');
    assert.equal(loadedAuthType, 'xai_oauth');
    assert.equal(global.channelsCurrentPage, 1);
  } finally {
    delete require.cache[require.resolve('./channels-filters.js')];
    for (const [key, descriptor] of previousGlobals) {
      if (descriptor === undefined) delete global[key];
      else Object.defineProperty(global, key, descriptor);
    }
  }
});
