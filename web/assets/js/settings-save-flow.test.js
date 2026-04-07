const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const settingsSource = fs.readFileSync(path.join(__dirname, 'settings.js'), 'utf8');

function createTextInput(initialValue, row) {
  return {
    value: initialValue,
    closest(selector) {
      return selector === 'tr' ? row : null;
    }
  };
}

function createRow() {
  return {
    style: { background: '' },
    querySelector(selector) {
      if (selector === 'td') {
        return { textContent: '[需重启] 示例配置' };
      }
      return null;
    }
  };
}

function createSettingsHarness() {
  const fetchCalls = [];
  const successMessages = [];
  const errorMessages = [];
  const notifications = [];

  const row = createRow();
  const input = createTextInput('new-value', row);

  const document = {
    getElementById(id) {
      if (id === 'demo_setting') return input;
      return null;
    },
    querySelectorAll() {
      return [];
    },
    querySelector() {
      return null;
    }
  };

  const sandbox = {
    console,
    document,
    showSuccess(message) {
      successMessages.push(message);
    },
    showError(message) {
      errorMessages.push(message);
    },
    fetchDataWithAuth: async (url, options = {}) => {
      fetchCalls.push({ url, options });
      if (url === '/admin/settings/batch') {
        return { message: '已保存 1 项配置，程序将在2秒后重启' };
      }
      if (url === '/admin/settings/demo_setting/reset') {
        return {
          message: '配置已重置为默认值，程序将在2秒后重启',
          key: 'demo_setting',
          value: 'default-value'
        };
      }
      if (url === '/admin/settings') {
        throw new Error('NetworkError when attempting to fetch resource.');
      }
      throw new Error(`unexpected url: ${url}`);
    },
    confirm() {
      return true;
    },
    window: {
      t(key, params = {}) {
        const messages = {
          'settings.msg.noChanges': '没有需要保存的更改',
          'settings.msg.savedCount': `已保存 ${params.count ?? ''} 项配置`,
          'settings.msg.restartRequired': '以下配置需要重启服务才能生效',
          'settings.msg.saveFailed': '设置保存失败',
          'settings.msg.loadFailed': '加载配置失败',
          'settings.msg.confirmReset': `确定要重置 "${params.key ?? ''}" 为默认值吗?`,
          'settings.msg.resetSuccess': `配置 ${params.key ?? ''} 已重置为默认值`,
          'settings.msg.resetFailed': '重置配置失败',
          'settings.msg.invalidResponse': '响应格式错误'
        };
        return messages[key] || key;
      },
      showNotification(message, type) {
        notifications.push({ message, type });
      },
      initPageBootstrap() {}
    }
  };

  vm.createContext(sandbox);
  vm.runInContext(`${settingsSource}
this.__settingsTest = {
  saveAllSettings,
  resetSetting,
  setOriginalSettings(value) {
    originalSettings = value;
  },
  getOriginalSettings() {
    return originalSettings;
  }
};`, sandbox);

  return {
    input,
    row,
    fetchCalls,
    successMessages,
    errorMessages,
    notifications,
    settingsTest: sandbox.__settingsTest
  };
}

test('saveAllSettings 在保存成功后不应因服务重启再触发一次 loadSettings', async () => {
  const harness = createSettingsHarness();
  harness.settingsTest.setOriginalSettings({ demo_setting: 'old-value' });

  await harness.settingsTest.saveAllSettings();

  assert.deepEqual(harness.fetchCalls.map(call => call.url), ['/admin/settings/batch']);
  assert.deepEqual(harness.errorMessages, []);
  assert.equal(harness.successMessages.length, 1);
});

test('resetSetting 在重置成功后不应因服务重启再触发一次 loadSettings', async () => {
  const harness = createSettingsHarness();
  harness.settingsTest.setOriginalSettings({ demo_setting: 'new-value' });

  await harness.settingsTest.resetSetting('demo_setting');

  assert.deepEqual(harness.fetchCalls.map(call => call.url), ['/admin/settings/demo_setting/reset']);
  assert.deepEqual(harness.errorMessages, []);
  assert.equal(harness.successMessages.length, 1);
  assert.equal(harness.input.value, 'default-value');
});
