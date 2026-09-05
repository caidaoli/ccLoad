const test = require('node:test');
const assert = require('node:assert/strict');

function createClassList() {
  const values = new Set();
  return {
    add: (...names) => names.forEach(name => values.add(name)),
    remove: (...names) => names.forEach(name => values.delete(name)),
    contains: name => values.has(name),
    toggle(name, force) {
      const enabled = force === undefined ? !values.has(name) : force;
      if (enabled) values.add(name);
      else values.delete(name);
    }
  };
}

function installTestChannelGlobals() {
  const requests = [];
  const updates = [];
  const elements = new Map();
  const makeElement = () => ({
    value: '',
    textContent: '',
    innerHTML: '',
    checked: false,
    disabled: false,
    classList: createClassList(),
    appendChild(child) {
      if (!this.value && !child.disabled) this.value = String(child.value);
    }
  });
  const getElement = id => {
    if (id === 'testUpstreamDetailBtn') return null;
    if (!elements.has(id)) elements.set(id, makeElement());
    return elements.get(id);
  };
  const globals = {
    window: {
      t: key => key,
      ProtocolManager: { renderProtocolSelect: async () => {} }
    },
    document: {
      getElementById: getElement,
      createElement: () => makeElement()
    },
    channels: [],
    testingChannelId: null,
    testingClientProtocol: 'anthropic',
    defaultTestContent: 'hello',
    fetchDataWithAuth: async (url, options) => {
      if (!options) return [];
      requests.push({ url, body: JSON.parse(options.body) });
      return { success: true, status_code: 200, duration_ms: 1 };
    },
    TemplateEngine: { render: () => null },
    handleChannelUpdateSuccess: async update => updates.push(update),
    maskKey: value => value
  };
  const previous = new Map();
  for (const [name, value] of Object.entries(globals)) {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  }
  return {
    elements,
    requests,
    updates,
    restore() {
      for (const [name, descriptor] of previous) {
        if (descriptor) Object.defineProperty(global, name, descriptor);
        else delete global[name];
      }
    }
  };
}

test('渠道测试无需渠道列表即可预选模型、提交请求并刷新当前页面', async () => {
  const fixture = installTestChannelGlobals();
  const modulePath = require.resolve('./channels-test.js');
  delete require.cache[modulePath];

  try {
    const { testChannel, runChannelTest } = require(modulePath);
    const opened = await testChannel({
      id: 7,
      name: 'test-channel',
      models: [{ model: 'first-model' }, { model: 'requested-model' }]
    }, 'requested-model');

    assert.equal(opened, true);
    assert.equal(fixture.elements.get('channelTestModelSelect').value, 'requested-model');
    assert.equal(fixture.elements.get('testModal').classList.contains('show'), true);
    await runChannelTest();
    assert.deepEqual(fixture.requests, [{
      url: '/admin/channels/7/test',
      body: { model: 'requested-model', content: 'hello', stream: false, client_protocol: 'anthropic' }
    }]);
    assert.deepEqual(fixture.updates, [{ savedChannelId: 7 }]);
  } finally {
    delete require.cache[modulePath];
    fixture.restore();
  }
});
