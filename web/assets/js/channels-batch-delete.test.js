const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const modalsSource = fs.readFileSync(path.join(__dirname, 'channels-modals.js'), 'utf8');
const html = fs.readFileSync(path.join(__dirname, '..', '..', 'channels.html'), 'utf8');
const zhLocaleSource = fs.readFileSync(path.join(__dirname, '..', 'locales', 'zh-CN.js'), 'utf8');
const enLocaleSource = fs.readFileSync(path.join(__dirname, '..', 'locales', 'en.js'), 'utf8');

function createModalElement() {
  const classSet = new Set();
  return {
    textContent: '',
    disabled: false,
    classList: {
      add(name) {
        classSet.add(name);
      },
      remove(name) {
        classSet.delete(name);
      },
      contains(name) {
        return classSet.has(name);
      }
    }
  };
}

function createBatchDeleteHarness() {
  const elements = new Map([
    ['deleteModal', createModalElement()],
    ['deleteModalMessage', createModalElement()]
  ]);
  const fetchCalls = [];
  const successMessages = [];
  const warningMessages = [];
  const errorMessages = [];
  const loadChannelsCalls = [];
  let clearCacheCalls = 0;

  const translations = {
    'channels.batchNoSelection': '请先选择至少一个渠道',
    'channels.confirmBatchDeleteMsg': '将删除 {count} 个渠道，确认继续吗？',
    'channels.batchDeleteSummary': '批量删除完成：删除 {deleted}，不存在 {notFound}',
    'channels.batchOperationFailed': '批量操作失败: {error}',
    'common.failed': '失败'
  };

  const sandbox = {
    console,
    window: {
      t(key, params = {}) {
        const template = translations[key] || key;
        return template.replace(/\{(\w+)\}/g, (_, name) => String(params[name] ?? ''));
      },
      showSuccess(message) {
        successMessages.push(message);
      },
      showWarning(message) {
        warningMessages.push(message);
      },
      showError(message) {
        errorMessages.push(message);
      }
    },
    document: {
      getElementById(id) {
        return elements.get(id) || null;
      }
    },
    fetchAPIWithAuth: async (url, options = {}) => {
      fetchCalls.push({ url, options });
      return {
        success: true,
        data: {
          deleted: 2,
          not_found_count: 1
        }
      };
    },
    loadChannels: async (type) => {
      loadChannelsCalls.push(type);
    },
    clearChannelsCache() {
      clearCacheCalls += 1;
    },
    filters: {
      channelType: 'all'
    },
    deletingChannelRequest: null,
    selectedChannelIds: new Set(),
    normalizeSelectedChannelID(value) {
      return String(value);
    }
  };

  vm.createContext(sandbox);
  vm.runInContext(modalsSource, sandbox);

  return {
    sandbox,
    elements,
    fetchCalls,
    successMessages,
    warningMessages,
    errorMessages,
    loadChannelsCalls,
    getClearCacheCalls() {
      return clearCacheCalls;
    }
  };
}

test('channels.html 提供批量删除按钮和动态删除提示容器', () => {
  assert.match(html, /id="batchDeleteChannelsBtn"[\s\S]*?data-action="batch-delete-channels"/);
  assert.match(html, /id="deleteModalMessage"/);
});

test('batchDeleteSelectedChannels 复用删除弹窗显示批量删除提示', () => {
  const { sandbox, elements, warningMessages } = createBatchDeleteHarness();
  sandbox.selectedChannelIds = new Set(['1', '2']);

  sandbox.batchDeleteSelectedChannels();

  assert.equal(warningMessages.length, 0);
  assert.equal(elements.get('deleteModal').classList.contains('show'), true);
  assert.equal(elements.get('deleteModalMessage').textContent, '将删除 2 个渠道，确认继续吗？');
});

test('confirmDelete 在批量模式下调用批量删除接口并显示摘要', async () => {
  const { sandbox, fetchCalls, successMessages, loadChannelsCalls, getClearCacheCalls } = createBatchDeleteHarness();
  sandbox.selectedChannelIds = new Set(['1', '2']);

  sandbox.batchDeleteSelectedChannels();
  await sandbox.confirmDelete();

  assert.equal(fetchCalls.length, 1);
  assert.equal(fetchCalls[0].url, '/admin/channels/batch-delete');
  assert.equal(fetchCalls[0].options.method, 'POST');
  assert.equal(fetchCalls[0].options.body, JSON.stringify({ channel_ids: [1, 2] }));
  assert.deepEqual(Array.from(sandbox.selectedChannelIds), []);
  assert.equal(getClearCacheCalls(), 1);
  assert.deepEqual(loadChannelsCalls, ['all']);
  assert.deepEqual(successMessages, ['批量删除完成：删除 2，不存在 1']);
});

test('批量删除文案已添加到中英文语言包', () => {
  assert.match(zhLocaleSource, /'channels\.batchDeleteChannels': '批量删除'/);
  assert.match(zhLocaleSource, /'channels\.confirmBatchDeleteMsg': '将删除 \{count\} 个渠道，确认继续吗？'/);
  assert.match(zhLocaleSource, /'channels\.batchDeleteSummary': '批量删除完成：删除 \{deleted\}，不存在 \{notFound\}'/);
  assert.match(enLocaleSource, /'channels\.batchDeleteChannels': 'Batch Delete'/);
  assert.match(enLocaleSource, /'channels\.confirmBatchDeleteMsg': 'Delete \{count\} selected channels\?'/);
  assert.match(enLocaleSource, /'channels\.batchDeleteSummary': 'Batch delete completed: deleted \{deleted\}, not found \{notFound\}'/);
});
