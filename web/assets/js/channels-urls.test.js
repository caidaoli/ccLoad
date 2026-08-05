const test = require('node:test');
const assert = require('node:assert/strict');

const {
  createURLRow,
  normalizeInlineURLConfigs,
  runtimeInlineURL
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
    querySelectorAll(selector) {
      assert.equal(selector, 'td');
      return [lastCell];
    },
    insertBefore() {}
  };
  const globals = {
    TemplateEngine: { render: () => row },
    document: {
      createElement(tagName) {
        assert.equal(tagName, 'td');
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
