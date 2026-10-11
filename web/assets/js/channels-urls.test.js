const test = require('node:test');
const assert = require('node:assert/strict');

const {
  createURLRow,
  normalizeInlineURLConfigs,
  runtimeInlineURL,
  testInlineURL
} = require('./channels-urls.js');

test('channel URL configs serialize one selected protocol or automatic detection', () => {
  const configs = normalizeInlineURLConfigs([
    {
      url: ' https://upstream.test/v1/messages ',
      exact: true,
      protocols: ['CODEX', 'openai', 'codex']
    },
    {
      url: 'https://automatic.test',
      exact: false,
      protocols: []
    }
  ]);

  assert.deepEqual(configs, [
    {
      url: 'https://upstream.test/v1/messages',
      exact: true,
      protocols: ['codex']
    },
    {
      url: 'https://automatic.test',
      exact: false,
      protocols: []
    }
  ]);
  assert.equal(runtimeInlineURL(configs[0]), 'https://upstream.test/v1/messages#');
  assert.equal(runtimeInlineURL(configs[1]), 'https://automatic.test');
});

test('URL row echoes the persisted exact flag in its checkbox', () => {
  const exactCheckbox = { checked: false };
  const selectionCheckbox = { checked: false };
  const lastCell = { querySelector: () => null };
  const row = {
    querySelector(selector) {
      if (selector === '.url-checkbox') return selectionCheckbox;
      if (selector === '.inline-url-exact-checkbox') return exactCheckbox;
      throw new Error(`Unexpected selector: ${selector}`);
    },
    querySelectorAll() {
      return [lastCell];
    },
    insertBefore() {}
  };
  const globals = {
    TemplateEngine: { render: () => row },
    document: {
      createElement() {
        return {
          setAttribute() {}
        };
      }
    },
    formatURLLatency: () => '-',
    formatURLRequests: () => '-',
    formatURLStatus: () => '-',
    inlineURLTableData: [{ url: '', exact: true, protocols: [] }],
    selectedURLIndices: new Set(),
    urlStatsMap: {},
    window: { t: key => key }
  };
  const previousGlobals = new Map(
    Object.keys(globals).map(key => [key, global[key]])
  );

  Object.assign(global, globals);
  try {
    const renderedRow = createURLRow(0);

    assert.equal(
      renderedRow.querySelector('.inline-url-exact-checkbox').checked,
      true
    );
  } finally {
    previousGlobals.forEach((value, key) => {
      if (value === undefined) delete global[key];
      else global[key] = value;
    });
  }
});

test('URL testing submits the native System One protocol without streaming', async t => {
  const requests = [];
  const globals = {
    editingChannelId: 7,
    editingChannelAuthType: 'api_key',
    redirectTableData: [{ model: 'test-model' }],
    inlineURLTableData: [],
    getValidInlineKeyRows: () => [{ api_key: 'test-key' }],
    fetchDataWithAuth: async (url, options) => {
      requests.push({ url, method: options.method, body: JSON.parse(options.body) });
      return { success: true };
    },
    refreshKeyCooldownStatus: async () => {},
    window: { t: key => key, showNotification: () => {} }
  };
  const previousGlobals = new Map(
    Object.keys(globals).map(key => [key, Object.getOwnPropertyDescriptor(global, key)])
  );

  Object.assign(global, globals);
  try {
    for (const [authType, protocols] of [
      ['system_one', []], ['system_one', ['openai']], ['api_key', []], ['api_key', ['codex']], ['api_key', ['gemini']]
    ]) {
      await t.test(`${authType}: ${protocols.join(', ') || 'automatic'}`, async () => {
        requests.length = 0;
        global.editingChannelAuthType = authType;
        global.inlineURLTableData = [{ url: 'https://upstream.test/v1', exact: true, protocols }];
        await testInlineURL(0, { innerHTML: 'Test', disabled: false });

        const systemOne = authType === 'system_one';
        assert.deepEqual(requests, [{
          url: '/admin/channels/7/test-url',
          method: 'POST',
          body: {
            model: 'test-model',
            stream: !systemOne,
            content: 'test',
            ...(systemOne ? {} : { client_protocol: 'anthropic' }),
            key_index: 0,
            base_url: 'https://upstream.test/v1#'
          }
        }]);
      });
    }
  } finally {
    previousGlobals.forEach((descriptor, key) => {
      if (descriptor) Object.defineProperty(global, key, descriptor);
      else delete global[key];
    });
  }
});
