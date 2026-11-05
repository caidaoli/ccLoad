const test = require('node:test');
const assert = require('node:assert/strict');

test('clearing token filters restores today and reloads its statistics', async () => {
  const elements = new Map();
  const document = {
    addEventListener() {}, querySelectorAll() { return []; },
    getElementById(id) {
      if (id === 'tpl-token-expiry-options') return null;
      if (!elements.has(id)) elements.set(id, Object.assign(new EventTarget(), {
        value: '', style: {}, dataset: {}
      }));
      return elements.get(id);
    }
  };
  let bootstrap;
  let actions;
  const requests = [];
  const window = {
    t: key => key,
    i18n: { onLocaleChange() {} },
    initPageBootstrap(config) { bootstrap = config.run; },
    initDelegatedActions(config) { actions = config; },
    initSavedDateRangeFilter(config) {
      const select = document.getElementById(config.selectId);
      select.value = config.defaultValue;
      select.addEventListener('change', () => config.onChange(select.value));
    }
  };
  const globals = {
    window, document,
    fetchDataWithAuth: async url => {
      requests.push(url);
      return { tokens: [] };
    }
  };
  const previous = new Map(Object.keys(globals).map(key => [key, global[key]]));
  const modulePath = require.resolve('./tokens.js');
  delete require.cache[modulePath];
  Object.assign(global, globals);
  try {
    require(modulePath);
    bootstrap();
    await new Promise(resolve => setImmediate(resolve));
    const range = document.getElementById('tokensRange');
    for (const value of ['yesterday', 'all', 'custom']) {
      range.value = value;
      range.dispatchEvent(new Event('change'));
      document.getElementById('tokensStatusFilter').value = 'inactive';
      document.getElementById('tokensSearchInput').value = 'example';
      actions.input['filter-tokens']();
      await new Promise(resolve => setImmediate(resolve));
      requests.length = 0;

      actions.click['clear-token-filters']();
      await new Promise(resolve => setImmediate(resolve));
      assert.equal(range.value, 'today');
      assert.equal(document.getElementById('tokensStatusFilter').value, 'all');
      assert.equal(document.getElementById('tokensSearchInput').value, '');
      assert.deepEqual(requests, ['/admin/auth-tokens?range=today']);
    }
  } finally {
    delete require.cache[modulePath];
    for (const [key, value] of previous) {
      if (value === undefined) delete global[key];
      else global[key] = value;
    }
  }
});
