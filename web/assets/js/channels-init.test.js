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
    querySelectorAll: () => [],
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
    ['searchInput', {}]
  ]);
  let authCombobox;
  let loadedAuthType = '';

  setGlobal('window', { t: key => key });
  setGlobal('document', { getElementById: id => elements.get(id) || null });
  setGlobal('filters', { status: 'all', authType: 'all', model: 'all', search: '' });
  setGlobal('channelsCurrentPage', 4);
  setGlobal('allAvailableModels', []);
  setGlobal('allAvailableChannelNames', []);
  setGlobal('channelAuthTypeFilterLabel', value => value);
  setGlobal('getChannelAuthTypeOptions', () => []);
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

test('本地重排与服务端排序同一规则：排序键优先，平局按优先级降序、名称升序', () => {
  const { compareChannelsForList } = require('./channels-filters.js');
  const fixtures = [
    { id: 1, name: 'charlie', priority: 30, enabled: true },
    { id: 2, name: 'alpha', priority: 10, enabled: false },
    { id: 3, name: 'delta', priority: 20, enabled: true },
    { id: 4, name: 'bravo', priority: 20, enabled: false }
  ];
  const order = sort => fixtures.slice().sort((a, b) => compareChannelsForList(a, b, sort)).map(c => c.name);

  assert.deepEqual(order({ key: 'priority', order: 'desc' }), ['charlie', 'bravo', 'delta', 'alpha']);
  assert.deepEqual(order({ key: 'priority', order: 'asc' }), ['alpha', 'bravo', 'delta', 'charlie']);
  assert.deepEqual(order({ key: 'name', order: 'desc' }), ['delta', 'charlie', 'bravo', 'alpha']);
  assert.deepEqual(order({ key: 'enabled', order: 'desc' }), ['charlie', 'delta', 'bravo', 'alpha']);
  assert.deepEqual(order({ key: 'enabled', order: 'asc' }), ['bravo', 'alpha', 'charlie', 'delta']);
  // 健康度模式下以 effective_priority 为准
  assert.deepEqual(
    [{ id: 1, name: 'a', priority: 10, effective_priority: 5 }, { id: 2, name: 'b', priority: 1, effective_priority: 8 }]
      .sort((a, b) => compareChannelsForList(a, b, { key: 'priority', order: 'desc' })).map(c => c.name),
    ['b', 'a']
  );
});
