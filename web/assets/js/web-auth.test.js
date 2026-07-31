const test = require('node:test');
const assert = require('node:assert/strict');

const WebAuth = require('./web-auth.js');

function memoryStorage() {
  const values = new Map();
  return {
    getItem(key) { return values.has(key) ? values.get(key) : null; },
    setItem(key, value) { values.set(key, String(value)); },
    removeItem(key) { values.delete(key); },
    keys() { return [...values.keys()].sort(); }
  };
}

async function captureChannelReadURLs(role) {
  const urls = [];
  const storage = memoryStorage();
  storage.setItem('ccload_web_role', role);
  const globals = {
    window: { WebAuth, t: key => key, showError: () => {} },
    localStorage: storage,
    filters: { search: 'any', searchExact: false, status: 'enabled', model: 'claude-opus-5', modelExact: true },
    channelsReadURL: (adminPath, dashboardPath) => (
      WebAuth.isAPITokenRole(storage) ? dashboardPath : adminPath
    ),
    channels: [],
    channelsTotalCount: 0,
    channelsTotalPages: 1,
    channelsCurrentPage: 1,
    channelsPageSize: 20,
    allAvailableChannelNames: [],
    allAvailableModels: [],
    channelStatsRange: 'today',
    channelStatsById: {},
    fetchAPIWithAuth: async (url) => {
      urls.push(url);
      return { success: true, data: [{ name: 'anyrouter' }], count: 1 };
    },
    fetchDataWithAuth: async (url) => {
      urls.push(url);
      if (url.includes('filter-options')) {
        return {
          channel_names: ['anyrouter', 'luoqianyi', 'other-channel'],
          models: ['claude-opus-5', 'gpt-5.4']
        };
      }
      return { stats: [], channel_health: {} };
    },
    filterChannels: () => {},
    updateChannelsPagination: () => {},
    updateModelOptions: () => {},
    updateChannelNameOptions: () => {}
  };
  const previous = new Map();
  for (const [name, value] of Object.entries(globals)) {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  }

  const modulePath = require.resolve('./channels-data.js');
  delete require.cache[modulePath];
  try {
    const channelsData = require('./channels-data.js');
    await channelsData.loadChannels('all');
    await channelsData.loadChannelsFilterOptions('enabled');
    await channelsData.loadChannelStats('today');
    return {
      urls,
      channelNames: [...global.allAvailableChannelNames],
      models: [...global.allAvailableModels]
    };
  } finally {
    delete require.cache[modulePath];
    for (const [name, descriptor] of previous) {
      if (descriptor) Object.defineProperty(global, name, descriptor);
      else delete global[name];
    }
  }
}

test('buildLoginPayload separates administrator password and API token', () => {
  assert.deepEqual(WebAuth.buildLoginPayload('admin', 'secret'), {
    mode: 'admin',
    password: 'secret'
  });
  assert.deepEqual(WebAuth.buildLoginPayload('api_token', 'sk-owner'), {
    mode: 'api_token',
    token: 'sk-owner'
  });
});

test('storeWebSession never persists submitted API token', () => {
  const storage = memoryStorage();
  WebAuth.storeWebSession(storage, {
    token: 'random-web-session',
    expiresIn: 3600,
    role: 'api_token'
  }, 1000);

  assert.deepEqual(storage.keys(), [
    'ccload_token',
    'ccload_token_expiry',
    'ccload_web_role'
  ]);
  assert.equal(storage.getItem('ccload_token'), 'random-web-session');
  assert.equal(storage.getItem('ccload_web_role'), 'api_token');
  assert.equal(storage.getItem('ccload_api_token'), null);
});

test('navigation excludes administrative pages for API token role', () => {
  const navKeys = ['index', 'channels', 'tokens', 'stats', 'trend', 'logs', 'model-test', 'settings'];
  assert.deepEqual(WebAuth.filterNavigation(navKeys, 'api_token'), [
    'index', 'stats', 'trend', 'logs'
  ]);
  assert.deepEqual(WebAuth.filterNavigation(navKeys, 'admin'), navKeys);
});

test('channel data uses endpoints allowed for the current web role', async () => {
  const apiTokenResult = await captureChannelReadURLs('api_token');
  const apiTokenURLs = apiTokenResult.urls;
  assert.deepEqual(apiTokenURLs.map(url => url.split('?')[0]), [
    '/dashboard/channels',
    '/dashboard/channels/filter-options',
    '/dashboard/stats'
  ]);
  assert.ok(apiTokenURLs.every(url => !url.includes('auth_token_id=')));

  const adminResult = await captureChannelReadURLs('admin');
  const adminURLs = adminResult.urls;
  assert.deepEqual(adminURLs.map(url => url.split('?')[0]), [
    '/admin/channels',
    '/admin/channels/filter-options',
    '/admin/stats'
  ]);
  for (const url of [...apiTokenURLs, ...adminURLs].filter(value => value.includes('/channels/filter-options'))) {
    const params = new URL(url, 'http://localhost').searchParams;
    assert.equal(params.has('search'), false);
    assert.equal(params.has('status'), false);
    assert.equal(params.has('model'), false);
  }
  assert.deepEqual(adminResult.channelNames, ['anyrouter', 'luoqianyi', 'other-channel']);
  assert.deepEqual(adminResult.models, ['claude-opus-5', 'gpt-5.4']);
});

test('login redirect stays on the current origin', () => {
  const origin = 'https://dashboard.example';
  assert.equal(WebAuth.getSafeRedirectPath('/web/logs.html?range=today#latest', origin), '/web/logs.html?range=today#latest');
  assert.equal(WebAuth.getSafeRedirectPath('https://evil.example/steal', origin), '/web/index.html');
  assert.equal(WebAuth.getSafeRedirectPath('//evil.example/steal', origin), '/web/index.html');
  assert.equal(WebAuth.getSafeRedirectPath('javascript:alert(1)', origin), '/web/index.html');
});
