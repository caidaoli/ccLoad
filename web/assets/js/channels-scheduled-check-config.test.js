const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const protocolSource = fs.readFileSync(path.join(__dirname, 'channels-protocols.js'), 'utf8');
const modalsSource = fs.readFileSync(path.join(__dirname, 'channels-modals.js'), 'utf8');

function createHarness(values) {
  const queue = [...values];
  const fetchCalls = [];
  const wrapper = { hidden: true };

  const sandbox = {
    console,
    document: {
      getElementById(id) {
        if (id === 'channelScheduledCheckEnabledWrapper') return wrapper;
        return null;
      }
    },
    fetchDataWithAuth: async (url) => {
      fetchCalls.push(url);
      if (queue.length === 0) {
        throw new Error('no mock value');
      }
      return { value: queue.shift() };
    },
    window: {}
  };

  vm.createContext(sandbox);
  vm.runInContext(`${protocolSource}
${modalsSource}
this.__channelScheduledCheckTest = {
  syncScheduledCheckVisibility
};`, sandbox);

  return {
    wrapper,
    fetchCalls,
    api: sandbox.__channelScheduledCheckTest
  };
}

test('syncScheduledCheckVisibility 每次打开弹窗都重新读取渠道定时检测配置', async () => {
  const harness = createHarness(['1', '0']);

  const firstVisible = await harness.api.syncScheduledCheckVisibility();
  const secondVisible = await harness.api.syncScheduledCheckVisibility();

  assert.equal(firstVisible, true);
  assert.equal(secondVisible, false);
  assert.equal(harness.wrapper.hidden, true);
  assert.deepEqual(harness.fetchCalls, [
    '/admin/settings/channel_check_interval_hours',
    '/admin/settings/channel_check_interval_hours'
  ]);
});
