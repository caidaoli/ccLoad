const test = require('node:test');
const assert = require('node:assert/strict');

function loadRenderer() {
  const domElement = () => ({
    style: {},
    dataset: {},
    classList: { add: () => {}, remove: () => {}, toggle: () => {} },
    setAttribute: () => {},
    appendChild: () => {},
    replaceChildren: () => {},
    querySelector: () => null,
    querySelectorAll: () => [],
    select: () => {}
  });

  const globals = {
    console,
    setTimeout,
    clearTimeout,
    CustomEvent: function CustomEvent(type, init) {
      this.type = type;
      this.detail = init && init.detail;
    },
    Image: function Image() {},
    localStorage: {
      getItem: () => null,
      setItem: () => {},
      removeItem: () => {}
    },
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    matchMedia: () => ({ matches: false, addEventListener: () => {} }),
    requestAnimationFrame: (fn) => setTimeout(fn, 0),
    t: (key) => key,
    document: {
      documentElement: {
        dataset: {},
        style: {}
      },
      createElement: domElement,
      createElementNS: () => ({
        setAttribute: () => {},
        classList: { add: () => {} },
        style: {}
      }),
      body: {
        classList: { add: () => {}, remove: () => {}, toggle: () => {} },
        appendChild: () => {},
        removeChild: () => {}
      },
      head: {
        appendChild: () => {}
      },
      addEventListener: () => {},
      querySelector: () => null,
      querySelectorAll: () => [],
      getElementById: () => null,
      execCommand: () => false
    }
  };
  Object.assign(global, globals);
  Object.defineProperty(global, 'location', { value: { href: '' }, configurable: true });
  Object.defineProperty(global, 'navigator', { value: {}, configurable: true });
  global.window = global;
  global.globalThis = global;
  global.window.dispatchEvent = () => {};
  global.hljs = require('./highlight.min.js');

  delete require.cache[require.resolve('./ui.js')];
  const ui = require('./ui.js');

  return ui.renderUpstreamCodeBlock;
}

test('renderUpstreamCodeBlock highlights JSON with highlight.js classes', () => {
  const render = loadRenderer();

  const html = render(JSON.stringify({ text: 'line one\nline two', ok: true }, null, 2), 'json');

  assert.match(html, /hljs-attr/);
  assert.match(html, /hljs-string/);
  assert.match(html, /hljs-literal/);
});

test('renderUpstreamCodeBlock keeps request headers readable and highlights JSON body', () => {
  const render = loadRenderer();
  const request = [
    'POST https://example.test/v1/messages',
    'Content-Type: application/json',
    '',
    JSON.stringify({ model: 'gpt-test', stream: true }, null, 2)
  ].join('\n');

  const html = render(request, 'request');

  assert.match(html, /upstream-token--method/);
  assert.match(html, /upstream-token--header-key/);
  assert.match(html, /hljs-attr/);
  assert.match(html, /hljs-literal/);
});
