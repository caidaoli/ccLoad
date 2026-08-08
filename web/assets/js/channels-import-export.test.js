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

test('CSV import shows indeterminate progress and finishes at completion', async () => {
  const previous = new Map();
  const setGlobal = (name, value) => {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  };
  const container = { hidden: true, dataset: {} };
  const progress = {
    max: 1,
    value: 0,
    indeterminate: false,
    removeAttribute(name) {
      if (name === 'value') this.indeterminate = true;
    }
  };
  const detail = { textContent: '' };
  const elements = {
    csvImportProgress: container,
    csvImportProgressBar: progress,
    csvImportProgressDetail: detail
  };
  const input = { files: [{ name: 'channels.csv' }], value: 'selected' };
  const button = { disabled: false };
  let observedPending = false;
  let reloaded = false;

  setGlobal('window', {
    t: (key, values = {}) => key === 'channels.import.progressPreparing'
      ? `uploading ${values.file}`
      : key,
    showSuccess() {},
    showError() {}
  });
  setGlobal('document', { getElementById: id => elements[id] || null });
  setGlobal('FormData', class { append() {} });
  setGlobal('fetchAPIWithAuth', async () => {
    observedPending = container.hidden === false && progress.indeterminate === true && detail.textContent === 'uploading channels.csv';
    return { success: true, data: { created: 1, updated: 0, skipped: 0 } };
  });
  setGlobal('reloadChannelsList', async () => { reloaded = true; });

  try {
    delete require.cache[require.resolve('./channels-import-export.js')];
    const { handleImportCSV } = require('./channels-import-export.js');
    await handleImportCSV({ target: input }, button);

    assert.equal(observedPending, true);
    assert.equal(container.hidden, false);
    assert.equal(container.dataset.kind, 'success');
    assert.equal(progress.value, 1);
    assert.equal(detail.textContent, 'channels.import.progressComplete');
    assert.equal(reloaded, true);
    assert.equal(button.disabled, false);
    assert.equal(input.value, '');
  } finally {
    delete require.cache[require.resolve('./channels-import-export.js')];
    for (const [name, descriptor] of previous) {
      if (descriptor) Object.defineProperty(global, name, descriptor);
      else delete global[name];
    }
  }
});
