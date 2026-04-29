const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const stateSource = fs.readFileSync(path.join(__dirname, 'channels-state.js'), 'utf8');
const keysSource = fs.readFileSync(path.join(__dirname, 'channels-keys.js'), 'utf8');

function createHarness(serverKeys) {
  const fetchCalls = [];
  const tableContainer = { scrollTop: 27 };
  const tableBody = {
    closest() {
      return tableContainer;
    }
  };

  const sandbox = {
    console,
    document: {
      querySelector(selector) {
        return selector === '#inlineKeyTableBody' ? tableBody : null;
      },
      getElementById() {
        return null;
      }
    },
    fetchDataWithAuth: async (url) => {
      fetchCalls.push(url);
      return serverKeys;
    },
    localStorage: {
      getItem() {
        return null;
      }
    },
    window: {
      t(key) {
        return key;
      }
    }
  };

  vm.createContext(sandbox);
  vm.runInContext(`${stateSource}
${keysSource}
let __renderCalls = 0;
renderInlineKeyTable = () => {
  __renderCalls += 1;
};
this.__keysRefreshTest = {
  setEditingChannelId(value) {
    editingChannelId = value;
  },
  setInlineKeys(value) {
    inlineKeyTableData = value;
  },
  getInlineKeys() {
    return inlineKeyTableData.slice();
  },
  getCooldowns() {
    return currentChannelKeyCooldowns.map(item => ({ ...item }));
  },
  getRenderCalls() {
    return __renderCalls;
  },
  refreshKeyCooldownStatus
};`, sandbox);

  return {
    api: sandbox.__keysRefreshTest,
    fetchCalls
  };
}

function createSingleKeyTestHarness() {
  const fetchCalls = [];

  const sandbox = {
    alert() {},
    console,
    document: {
      querySelectorAll(selector) {
        if (selector === 'input[name="channelType"]') {
          return [{ checked: true, value: 'openai' }];
        }
        return [];
      }
    },
    fetchDataWithAuth: async (url, options = {}) => {
      fetchCalls.push({
        url,
        body: options.body ? JSON.parse(options.body) : null
      });
      return { success: true };
    },
    localStorage: {
      getItem() {
        return null;
      }
    },
    window: {
      showNotification() {},
      t(key, params = {}) {
        return `${key}:${JSON.stringify(params)}`;
      }
    }
  };

  vm.createContext(sandbox);
  vm.runInContext(`${stateSource}
${keysSource}
let __refreshCalls = 0;
refreshKeyCooldownStatus = async () => {
  __refreshCalls += 1;
};
this.__singleKeyTest = {
  setEditingChannelId(value) {
    editingChannelId = value;
  },
  setInlineKeys(value) {
    inlineKeyTableData = value;
  },
  setModels(value) {
    redirectTableData = value;
  },
  getRefreshCalls() {
    return __refreshCalls;
  },
  testSingleKey
};`, sandbox);

  return {
    api: sandbox.__singleKeyTest,
    fetchCalls
  };
}

test('refreshKeyCooldownStatus 只刷新冷却元数据，不丢弃未保存的新 Key', async () => {
  const { api, fetchCalls } = createHarness([
    { api_key: 'sk-old', cooldown_until: 4102444800 }
  ]);

  api.setEditingChannelId(7);
  api.setInlineKeys(['sk-old', 'sk-new-unsaved']);

  await api.refreshKeyCooldownStatus();

  assert.deepEqual(fetchCalls, ['/admin/channels/7/keys']);
  assert.deepEqual(api.getInlineKeys(), ['sk-old', 'sk-new-unsaved']);
  assert.equal(api.getRenderCalls(), 1);

  const cooldowns = api.getCooldowns();
  assert.equal(cooldowns.find(item => item.key_index === 0).cooldown_remaining_ms > 0, true);
  assert.equal(cooldowns.find(item => item.key_index === 1).cooldown_remaining_ms, 0);
});

test('testSingleKey 使用当前行输入的 API Key 发起测试请求', async () => {
  const { api, fetchCalls } = createSingleKeyTestHarness();
  const testButton = { disabled: false, innerHTML: 'test' };

  api.setEditingChannelId(7);
  api.setModels([{ model: 'gpt-test' }]);
  api.setInlineKeys(['sk-old', 'sk-new-unsaved']);

  await api.testSingleKey(1, testButton);

  assert.equal(fetchCalls.length, 1);
  assert.equal(fetchCalls[0].url, '/admin/channels/7/test');
  assert.equal(fetchCalls[0].body.key_index, 1);
  assert.equal(fetchCalls[0].body.api_key, 'sk-new-unsaved');
  assert.equal(api.getRefreshCalls(), 1);
  assert.equal(testButton.disabled, false);
  assert.equal(testButton.innerHTML, 'test');
});
