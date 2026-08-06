const test = require('node:test');
const assert = require('node:assert/strict');

test('returning via reload or bfcache restores channel name search with the other filters', async () => {
  const previousGlobals = new Map();
  const setGlobal = (key, value) => {
    previousGlobals.set(key, Object.getOwnPropertyDescriptor(global, key));
    Object.defineProperty(global, key, { configurable: true, writable: true, value });
  };

  const savedState = {
    status: 'enabled',
    authType: 'codex_oauth',
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
      authType: 'codex_oauth',
      model: 'claude-opus-5',
      modelExact: true
    });
    assert.equal(elements.searchInput.value, 'any');
    assert.equal(elements.channelAuthTypeFilter.value, 'codex_oauth');
    assert.deepEqual(JSON.parse(storage.get('channels.filters')), {
      status: 'enabled',
      authType: 'codex_oauth',
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
