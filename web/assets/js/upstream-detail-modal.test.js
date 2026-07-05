const test = require('node:test');
const assert = require('node:assert/strict');

function loadModule() {
  delete require.cache[require.resolve('./upstream-detail-modal.js')];
  global.maskSensitiveHeaders = (headers) => {
    const out = { ...headers };
    if (out.Authorization) out.Authorization = 'Bear******test';
    return out;
  };
  return require('./upstream-detail-modal.js');
}

test('composeRawRequest matches debug modal raw request format', () => {
  const modal = loadModule();

  const raw = modal.composeRawRequest({
    method: 'POST',
    url: 'https://example.test/v1/chat/completions',
    requestHeaders: {
      Authorization: 'Bearer secret-test',
      'Content-Type': 'application/json'
    },
    requestBody: '{"model":"gpt-test","stream":true}'
  });

  assert.equal(raw, [
    'POST https://example.test/v1/chat/completions',
    'Authorization: Bear******test',
    'Content-Type: application/json',
    '',
    '{',
    '  "model": "gpt-test",',
    '  "stream": true',
    '}'
  ].join('\n'));
});

test('composeRawResponse keeps status, repeated headers, and non-json body', () => {
  const modal = loadModule();

  const raw = modal.composeRawResponse({
    statusCode: 429,
    responseHeaders: {
      'Retry-After': ['60', '120']
    },
    responseBody: 'rate limited'
  });

  assert.equal(raw, [
    'HTTP 429',
    'Retry-After: 60',
    'Retry-After: 120',
    '',
    'rate limited'
  ].join('\n'));
});

test('Escape closes upstream modal and stops stacked modal propagation', () => {
  delete require.cache[require.resolve('./upstream-detail-modal.js')];

  const previousDocument = global.document;
  const listeners = {};
  let visible = true;
  global.document = {
    addEventListener: (type, handler) => {
      listeners[type] = handler;
    },
    getElementById: (id) => {
      if (id !== 'upstreamDetailModal') return null;
      return {
        classList: {
          contains: (name) => name === 'show' && visible,
          remove: (name) => {
            if (name === 'show') visible = false;
          }
        }
      };
    }
  };

  try {
    require('./upstream-detail-modal.js');

    const event = {
      key: 'Escape',
      prevented: false,
      stopped: false,
      preventDefault() {
        this.prevented = true;
      },
      stopImmediatePropagation() {
        this.stopped = true;
      }
    };
    listeners.keydown(event);

    assert.equal(visible, false);
    assert.equal(event.prevented, true);
    assert.equal(event.stopped, true);
  } finally {
    if (previousDocument === undefined) {
      delete global.document;
    } else {
      global.document = previousDocument;
    }
  }
});
