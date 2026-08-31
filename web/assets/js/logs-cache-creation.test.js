const test = require('node:test');
const assert = require('node:assert/strict');

const escapeHtmlStub = (str) => String(str);
global.escapeHtml = escapeHtmlStub;
global.window = {
  t: (key) => key,
  escapeHtml: escapeHtmlStub,
  initPageBootstrap() {},
  addEventListener() {},
};
global.document = { addEventListener() {} };
global.localStorage = { getItem: () => null, setItem() {}, removeItem() {} };

const { getEffectiveCacheCreationTokens } = require('./logs.js');

test('缓存建立显示优先使用后端 aggregate', () => {
  assert.equal(getEffectiveCacheCreationTokens({
    cache_creation_input_tokens: 500,
    cache_5m_input_tokens: 300,
    cache_1h_input_tokens: 200,
  }), 500);
});

test('aggregate 为 0 时使用 5m+1h 明细兜底', () => {
  assert.equal(getEffectiveCacheCreationTokens({
    cache_creation_input_tokens: 0,
    cache_5m_input_tokens: 2613,
    cache_1h_input_tokens: 200,
  }), 2813);
});

test('没有有效缓存建立数据时仍显示为 0', () => {
  assert.equal(getEffectiveCacheCreationTokens({}), 0);
  assert.equal(getEffectiveCacheCreationTokens({
    cache_creation_input_tokens: 0,
    cache_5m_input_tokens: 0,
    cache_1h_input_tokens: 0,
  }), 0);
});
