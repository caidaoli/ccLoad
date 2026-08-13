const test = require('node:test');
const assert = require('node:assert/strict');

function withGlobals(overrides, run) {
  const previous = new Map();
  for (const [name, value] of Object.entries(overrides)) {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  }
  return (async () => {
    try {
      delete require.cache[require.resolve('./channels-import-export.js')];
      return await run(require('./channels-import-export.js'));
    } finally {
      delete require.cache[require.resolve('./channels-import-export.js')];
      for (const [name, descriptor] of previous) {
        if (descriptor) Object.defineProperty(global, name, descriptor);
        else delete global[name];
      }
    }
  })();
}

test('CSV export requests only the selected channel ids', async () => {
  const NativeURL = global.URL;
  let requestedURL = '';
  let clicked = false;

  await withGlobals({
    window: { t: key => key, showSuccess() {} },
    getSelectedChannelIDs: () => [12, 7, 5],
    fetchWithAuth: async (url) => {
      requestedURL = url;
      return { ok: true, blob: async () => ({}) };
    },
    formatTimestampForFilename: () => '20260808-162810',
    document: {
      body: { appendChild() {}, removeChild() {} },
      createElement: () => ({ click() { clicked = true; } }),
      getElementById: () => null
    },
    URL: class extends NativeURL {
      static createObjectURL() { return 'blob:test'; }
      static revokeObjectURL() {}
    }
  }, async ({ exportSelectedChannelsCSV }) => {
    await exportSelectedChannelsCSV();

    const parsed = new NativeURL(requestedURL, 'http://localhost');
    assert.equal(parsed.pathname, '/admin/channels/export');
    assert.deepEqual(Object.fromEntries(parsed.searchParams), { ids: '12,7,5' });
    assert.equal(clicked, true);
  });
});

test('CSV export aborts without hitting the API when nothing is selected', async () => {
  let requested = false;
  let warned = '';

  await withGlobals({
    window: { t: key => key, showWarning(msg) { warned = msg; } },
    getSelectedChannelIDs: () => [],
    fetchWithAuth: async () => {
      requested = true;
      return { ok: true, blob: async () => ({}) };
    },
    document: { getElementById: () => null }
  }, async ({ exportSelectedChannelsCSV }) => {
    await exportSelectedChannelsCSV();

    assert.equal(requested, false);
    assert.equal(warned, 'channels.batchNoSelection');
  });
});
