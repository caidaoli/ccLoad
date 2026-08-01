const test = require('node:test');
const assert = require('node:assert/strict');

function createClassList() {
  const values = new Set();
  return {
    add: (...names) => names.forEach(name => values.add(name)),
    remove: (...names) => names.forEach(name => values.delete(name)),
    contains: name => values.has(name)
  };
}

function installTestChannelGlobals() {
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
    channels: [{
      id: 7,
      models: [
        { model: 'first-model' },
        { model: 'requested-model' }
      ]
    }],
    testingChannelId: null,
    testingClientProtocol: 'anthropic',
    defaultTestContent: 'hello',
    fetchDataWithAuth: async () => [],
    maskKey: value => value
  };
  const previous = new Map();
  for (const [name, value] of Object.entries(globals)) {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  }
  return {
    elements,
    restore() {
      for (const [name, descriptor] of previous) {
        if (descriptor) Object.defineProperty(global, name, descriptor);
        else delete global[name];
      }
    }
  };
}

test('渠道测试可以预选指定模型', async () => {
  const fixture = installTestChannelGlobals();
  const modulePath = require.resolve('./channels-test.js');
  delete require.cache[modulePath];

  try {
    const { testChannel } = require(modulePath);
    const opened = await testChannel(7, 'test-channel', 'requested-model');

    assert.equal(opened, true);
    assert.equal(fixture.elements.get('testModelSelect').value, 'requested-model');
    assert.equal(fixture.elements.get('testModal').classList.contains('show'), true);
  } finally {
    delete require.cache[modulePath];
    fixture.restore();
  }
});
