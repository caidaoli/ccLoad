const test = require('node:test');
const assert = require('node:assert/strict');

test('CSV export sends current channel filters without pagination', async () => {
  const NativeURL = global.URL;
  const previous = new Map();
  const setGlobal = (name, value) => {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  };
  let requestedURL = '';
  let clicked = false;
  const button = { disabled: false };

  setGlobal('window', { t: key => key, showSuccess() {} });
  setGlobal('buildChannelsListParams', () => new URLSearchParams({
    search: 'needle',
    status: 'enabled',
    auth_type: 'codex_oauth',
    model: 'grok-4.5',
    limit: '20',
    offset: '40'
  }));
  setGlobal('fetchWithAuth', async (url) => {
    requestedURL = url;
    return { ok: true, blob: async () => ({}) };
  });
  setGlobal('formatTimestampForFilename', () => '20260808-162810');
  setGlobal('document', {
    body: { appendChild() {}, removeChild() {} },
    createElement: () => ({ click() { clicked = true; } })
  });
  setGlobal('URL', class extends NativeURL {
    static createObjectURL() { return 'blob:test'; }
    static revokeObjectURL() {}
  });

  try {
    delete require.cache[require.resolve('./channels-import-export.js')];
    const { exportChannelsCSV } = require('./channels-import-export.js');
    await exportChannelsCSV(button);

    const parsed = new NativeURL(requestedURL, 'http://localhost');
    assert.equal(parsed.pathname, '/admin/channels/export');
    assert.deepEqual(Object.fromEntries(parsed.searchParams), {
      search: 'needle',
      status: 'enabled',
      auth_type: 'codex_oauth',
      model: 'grok-4.5'
    });
    assert.equal(clicked, true);
    assert.equal(button.disabled, false);
  } finally {
    delete require.cache[require.resolve('./channels-import-export.js')];
    for (const [name, descriptor] of previous) {
      if (descriptor) Object.defineProperty(global, name, descriptor);
      else delete global[name];
    }
  }
});
