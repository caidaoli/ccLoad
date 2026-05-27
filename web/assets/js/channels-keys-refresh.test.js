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

function createToggleKeyDisabledHarness() {
  const fetchCalls = [];
  const notifications = [];

  const sandbox = {
    alert() {},
    console,
    document: {
      querySelectorAll() {
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
      showNotification(message, type) {
        notifications.push({ message, type });
      },
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
this.__toggleKeyDisabledTest = {
  setEditingChannelId(value) {
    editingChannelId = value;
  },
  setDirty(value) {
    channelFormDirty = value;
  },
  setCooldowns(value) {
    currentChannelKeyCooldowns = value;
  },
  getRefreshCalls() {
    return __refreshCalls;
  },
  toggleKeyDisabled
};`, sandbox);

  return {
    api: sandbox.__toggleKeyDisabledTest,
    fetchCalls,
    notifications
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

test('toggleKeyDisabled 拒绝在未保存 Key 变更上执行持久化索引切换', async () => {
  const { api, fetchCalls, notifications } = createToggleKeyDisabledHarness();

  api.setEditingChannelId(7);
  api.setDirty(true);
  api.setCooldowns([{ key_index: 1, cooldown_remaining_ms: 0, disabled: false }]);

  await api.toggleKeyDisabled(1);

  assert.deepEqual(fetchCalls, []);
  assert.equal(api.getRefreshCalls(), 0);
  assert.equal(notifications.length, 1);
  assert.equal(notifications[0].type, 'error');
  assert.match(notifications[0].message, /channels\.saveBeforeToggleKeyDisabled/);
});
