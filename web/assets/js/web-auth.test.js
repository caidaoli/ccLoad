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
    'index', 'channels', 'stats', 'trend', 'logs', 'model-test'
  ]);
  assert.deepEqual(WebAuth.filterNavigation(navKeys, 'admin'), navKeys);
});

test('login redirect stays on the current origin', () => {
  const origin = 'https://dashboard.example';
  assert.equal(WebAuth.getSafeRedirectPath('/web/logs.html?range=today#latest', origin), '/web/logs.html?range=today#latest');
  assert.equal(WebAuth.getSafeRedirectPath('https://evil.example/steal', origin), '/web/index.html');
  assert.equal(WebAuth.getSafeRedirectPath('//evil.example/steal', origin), '/web/index.html');
  assert.equal(WebAuth.getSafeRedirectPath('javascript:alert(1)', origin), '/web/index.html');
});
