const test = require('node:test');
const assert = require('node:assert/strict');

test('debug log copy works when the native Clipboard API is unavailable', async () => {
  const previousWindow = global.window;
  const previousDocument = global.document;
  const previousLocalStorage = Object.getOwnPropertyDescriptor(global, 'localStorage');
  const previousNavigator = Object.getOwnPropertyDescriptor(global, 'navigator');
  const previousSetTimeout = global.setTimeout;
  const listeners = {};
  let copiedText = '';

  const copyButton = {
    dataset: { copyTarget: 'debugReqRaw' },
    textContent: 'Copy',
    classList: {
      add() {},
      remove() {}
    }
  };
  const rawRequest = {
    _rawText: 'POST /v1/messages\n\n{"model":"test"}',
    textContent: 'rendered request'
  };

  global.window = {
    t: (key) => key,
    initPageBootstrap() {},
    addEventListener() {},
    copyToClipboard: async (text) => {
      copiedText = text;
    }
  };
  global.document = {
    addEventListener: (type, handler) => {
      listeners[type] = handler;
    },
    getElementById: (id) => id === 'debugReqRaw' ? rawRequest : null,
    querySelectorAll: () => []
  };
  Object.defineProperty(global, 'localStorage', {
    configurable: true,
    value: { getItem: () => null }
  });
  Object.defineProperty(global, 'navigator', {
    configurable: true,
    value: {}
  });
  global.setTimeout = (handler) => {
    handler();
    return 0;
  };

  try {
    delete require.cache[require.resolve('./logs.js')];
    require('./logs.js');

    listeners.click({
      target: {
        closest: (selector) => selector === '#debugLogModal .upstream-copy-btn' ? copyButton : null
      }
    });
    await Promise.resolve();

    assert.equal(copiedText, rawRequest._rawText);
  } finally {
    delete require.cache[require.resolve('./logs.js')];
    if (previousWindow === undefined) delete global.window;
    else global.window = previousWindow;
    if (previousDocument === undefined) delete global.document;
    else global.document = previousDocument;
    if (previousLocalStorage === undefined) delete global.localStorage;
    else Object.defineProperty(global, 'localStorage', previousLocalStorage);
    if (previousNavigator === undefined) delete global.navigator;
    else Object.defineProperty(global, 'navigator', previousNavigator);
    global.setTimeout = previousSetTimeout;
  }
});
