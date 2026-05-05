const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const protocolSource = fs.readFileSync(path.join(__dirname, 'channels-protocols.js'), 'utf8');
const modalsSource = fs.readFileSync(path.join(__dirname, 'channels-modals.js'), 'utf8');

function createDeferred() {
  let resolve;
  let reject;
  const promise = new Promise((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

function createToggleHarness() {
  const fetchRequest = createDeferred();
  const filterSnapshots = [];
  const successMessages = [];
  let reloadCalls = 0;

  const sandbox = {
    console,
    channels: [{ id: 7, name: 'slow-channel', enabled: false }],
    filteredChannels: [{ id: 7, name: 'slow-channel', enabled: false }],
    filters: { channelType: 'all', status: 'all' },
    clearChannelsCache() {},
    filterChannels() {
      filterSnapshots.push(sandbox.channels.map(channel => ({
        id: channel.id,
        enabled: channel.enabled
      })));
    },
    reloadChannelsList: async () => {
      reloadCalls += 1;
      throw new Error('toggleChannel must not reload channel list');
    },
    fetchAPIWithAuth: async () => {
      await fetchRequest.promise;
      return { success: true };
    },
    window: {
      t(key) {
        return key;
      },
      showSuccess(message) {
        successMessages.push(message);
      },
      showError(message) {
        throw new Error(message);
      }
    }
  };

  vm.createContext(sandbox);
  vm.runInContext(`${protocolSource}\n${modalsSource}\nthis.__toggleUXTest = { toggleChannel };`, sandbox);

  return {
    api: sandbox.__toggleUXTest,
    sandbox,
    fetchRequest,
    filterSnapshots,
    successMessages,
    getReloadCalls() {
      return reloadCalls;
    }
  };
}

test('toggleChannel 点击后立即更新本地开关状态，不等待网络和整表刷新', async () => {
  const harness = createToggleHarness();

  const togglePromise = harness.api.toggleChannel(7, true);

  assert.equal(harness.sandbox.channels[0].enabled, true);
  assert.deepEqual(harness.filterSnapshots, [[{ id: 7, enabled: true }]]);
  assert.deepEqual(harness.successMessages, []);

  harness.fetchRequest.resolve();
  await new Promise(resolve => setImmediate(resolve));

  assert.deepEqual(harness.successMessages, ['channels.channelEnabled']);
  await togglePromise;
  assert.equal(harness.getReloadCalls(), 0);
});

test('toggleChannel 在状态筛选下只更新本地列表，不重新拉 channels 或 filter-options', async () => {
  const harness = createToggleHarness();
  harness.sandbox.channels = [{ id: 7, name: 'enabled-channel', enabled: true }];
  harness.sandbox.filteredChannels = harness.sandbox.channels.slice();
  harness.sandbox.filters.status = 'enabled';

  const togglePromise = harness.api.toggleChannel(7, false);

  assert.deepEqual(harness.sandbox.channels, []);
  assert.deepEqual(harness.filterSnapshots, [[]]);

  harness.fetchRequest.resolve();
  await togglePromise;

  assert.deepEqual(harness.successMessages, ['channels.channelDisabled']);
  assert.equal(harness.getReloadCalls(), 0);
});
