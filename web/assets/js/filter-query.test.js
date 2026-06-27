const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const filterQueryPath = path.join(__dirname, 'filter-query.js');

function loadFilterQueryModule() {
  assert.ok(fs.existsSync(filterQueryPath), '缺少共享请求参数模块 filter-query.js');

  const source = fs.readFileSync(filterQueryPath, 'utf8');
  const sandbox = {
    console,
    URLSearchParams,
    window: {}
  };

  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);
  return sandbox.window;
}

test('共享请求参数模块暴露 buildRequestParams helper', () => {
  const window = loadFilterQueryModule();

  assert.equal(typeof window.FilterQuery, 'object');
  assert.equal(typeof window.FilterQuery.buildRequestParams, 'function');
});

test('共享请求参数模块支持基础参数、requestKey 和 includeInRequest', () => {
  const window = loadFilterQueryModule();
  const params = window.FilterQuery.buildRequestParams(
    {
      range: 'today',
      authToken: '9',
      channelType: 'all',
      trendType: 'rpm',
      channelName: 'demo'
    },
    [
      { key: 'range', queryKeys: ['range'] },
      { key: 'authToken', queryKeys: ['token'], requestKey: 'auth_token_id' },
      {
        key: 'channelType',
        queryKeys: ['channel_type'],
        includeInRequest(value) {
          return Boolean(value) && value !== 'all';
        }
      },
      {
        key: 'trendType',
        queryKeys: ['type'],
        includeInRequest() {
          return false;
        }
      },
      { key: 'channelName', queryKeys: ['channel_name'] }
    ],
    {
      baseParams: {
        bucket_min: 5
      }
    }
  );

  assert.equal(params.get('bucket_min'), '5');
  assert.equal(params.get('range'), 'today');
  assert.equal(params.get('auth_token_id'), '9');
  assert.equal(params.get('channel_name'), 'demo');
  assert.equal(params.has('channel_type'), false);
  assert.equal(params.has('type'), false);
});

test('共享请求参数模块支持按当前值动态选择 requestKey', () => {
  const window = loadFilterQueryModule();
  const params = window.FilterQuery.buildRequestParams(
    {
      model: 'gpt-5.4',
      modelExact: true,
      channelName: '88',
      channelNameExact: false
    },
    [
      {
        key: 'model',
        queryKeys: ['model', 'model_like'],
        requestKey(value, values) {
          return values.modelExact ? 'model' : 'model_like';
        }
      },
      {
        key: 'channelName',
        queryKeys: ['channel_name', 'channel_name_like'],
        requestKey(value, values) {
          return values.channelNameExact ? 'channel_name' : 'channel_name_like';
        }
      }
    ]
  );

  assert.equal(params.get('model'), 'gpt-5.4');
  assert.equal(params.has('model_like'), false);
  assert.equal(params.get('channel_name_like'), '88');
  assert.equal(params.has('channel_name'), false);
});
