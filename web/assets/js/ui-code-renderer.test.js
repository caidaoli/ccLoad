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

function createElement(tag = 'div') {
  const attributes = new Map();
  const classNames = new Set();
  const el = {
    tagName: String(tag).toUpperCase(),
    style: {},
    dataset: {},
    children: [],
    textContent: '',
    innerHTML: '',
    title: '',
    classList: {
      add: (...names) => names.forEach(name => classNames.add(name)),
      remove: (...names) => names.forEach(name => classNames.delete(name)),
      toggle: (name, force) => {
        const enabled = force === undefined ? !classNames.has(name) : Boolean(force);
        if (enabled) classNames.add(name);
        else classNames.delete(name);
        return enabled;
      },
      contains: (name) => classNames.has(name)
    },
    setAttribute: (name, value) => {
      const normalized = String(value);
      attributes.set(name, normalized);
      el[name] = normalized;
      if (name === 'title') el.title = normalized;
    },
    getAttribute: (name) => attributes.get(name) || null,
    removeAttribute: (name) => {
      attributes.delete(name);
      delete el[name];
    },
    appendChild: (child) => {
      if (child && child.parentNode && child.parentNode !== el && typeof child.parentNode.removeChild === 'function') {
        child.parentNode.removeChild(child);
      }
      const existingIndex = el.children.indexOf(child);
      if (existingIndex >= 0) el.children.splice(existingIndex, 1);
      if (child && typeof child === 'object') child.parentNode = el;
      el.children.push(child);
      return child;
    },
    replaceChildren: (...children) => {
      el.children = [];
      children.forEach((child) => el.appendChild(child));
    },
    removeChild: (child) => {
      const index = el.children.indexOf(child);
      if (index >= 0) {
        el.children.splice(index, 1);
        if (child && typeof child === 'object') child.parentNode = null;
      }
      return child;
    },
    remove: () => {
      if (el.parentNode && typeof el.parentNode.removeChild === 'function') {
        el.parentNode.removeChild(el);
      }
    },
    addEventListener: () => {},
    removeEventListener: () => {},
    querySelector: () => null,
    querySelectorAll: () => [],
    closest: () => null,
    select: () => {}
  };
  if (String(tag).toLowerCase() === 'canvas') {
    const commands = [];
    const ctx = {
      clearRect: (...args) => commands.push(['clearRect', args]),
      drawImage: (...args) => commands.push(['drawImage', args.length]),
      beginPath: () => commands.push(['beginPath']),
      arc: (...args) => commands.push(['arc', args]),
      fill: () => commands.push(['fill', ctx.fillStyle]),
      fillText: (...args) => commands.push(['fillText', args]),
      fillStyle: '',
      font: '',
      textAlign: '',
      textBaseline: ''
    };
    el.getContext = () => ctx;
    el.toDataURL = () => `data:image/png;base64,${Buffer.from(JSON.stringify(commands)).toString('base64')}`;
  }
  return el;
}

function installTopbarTestGlobals(activePayloads, options = {}) {
  const intervals = [];
  let intervalID = 0;
  const iconLinks = [];
  const isIconSelector = (selector) => selector === 'link[rel~="icon"]';
  const syncIconLinks = (head) => {
    iconLinks.length = 0;
    head.children.forEach((child) => {
      if (child && typeof child.rel === 'string' && child.rel.split(/\s+/).includes('icon')) {
        iconLinks.push(child);
      }
    });
  };
  const doc = {
    title: '请求日志 - Claude Code & Codex Proxy',
    documentElement: {
      dataset: {},
      style: {}
    },
    createElement,
    createElementNS: createElement,
    createTextNode: (text) => ({ nodeType: 3, textContent: String(text) }),
    body: createElement('body'),
    head: createElement('head'),
    addEventListener: () => {},
    querySelector: (selector) => isIconSelector(selector) ? (iconLinks[0] || null) : null,
    querySelectorAll: (selector) => isIconSelector(selector) ? iconLinks.slice() : [],
    getElementById: () => null,
    execCommand: () => false
  };
  const originalAppendChild = doc.head.appendChild;
  doc.head.appendChild = (child) => {
    const result = originalAppendChild(child);
    syncIconLinks(doc.head);
    return result;
  };
  const originalRemoveChild = doc.head.removeChild;
  doc.head.removeChild = (child) => {
    const result = originalRemoveChild(child);
    syncIconLinks(doc.head);
    return result;
  };

  const svgIcon = createElement('link');
  svgIcon.rel = 'icon';
  svgIcon.href = '/web/favicon.svg';
  svgIcon.type = 'image/svg+xml';
  svgIcon.setAttribute('href', svgIcon.href);
  svgIcon.setAttribute('type', svgIcon.type);
  const icoIcon = createElement('link');
  icoIcon.rel = 'icon';
  icoIcon.href = '/web/favicon.ico';
  icoIcon.type = 'image/x-icon';
  icoIcon.setAttribute('href', icoIcon.href);
  icoIcon.setAttribute('type', icoIcon.type);
  doc.head.appendChild(svgIcon);
  doc.head.appendChild(icoIcon);

  const globals = {
    console,
    setTimeout,
    clearTimeout,
    setInterval: (fn, ms) => {
      const item = { id: ++intervalID, fn, ms, cleared: false };
      intervals.push(item);
      return item.id;
    },
    clearInterval: (id) => {
      const item = intervals.find(entry => entry.id === id);
      if (item) item.cleared = true;
    },
    CustomEvent: function CustomEvent(type, init) {
      this.type = type;
      this.detail = init && init.detail;
    },
    Image: function Image() {
      Object.defineProperty(this, 'src', {
        set(value) {
          this._src = value;
          if (typeof this.onload === 'function') setImmediate(() => this.onload());
        },
        get() {
          return this._src;
        }
      });
    },
    localStorage: {
      getItem: (key) => key === 'ccload_token' ? 'test-token' : null,
      setItem: () => {},
      removeItem: () => {}
    },
    getComputedStyle: () => ({ getPropertyValue: () => '' }),
    matchMedia: () => ({ matches: false, addEventListener: () => {} }),
    requestAnimationFrame: (fn) => setTimeout(fn, 0),
    document: doc,
    fetch: async (url) => {
      if (url === '/public/version') {
        return { json: async () => ({ success: true, data: { version: 'dev' } }) };
      }
      if (url === '/admin/active-requests') {
        const payload = activePayloads.length
          ? activePayloads.shift()
          : { success: true, count: 0, data: [] };
        return {
          status: 200,
          text: async () => JSON.stringify(payload)
        };
      }
      return {
        status: 200,
        text: async () => JSON.stringify({ success: true, data: null })
      };
    }
  };
  globals.t = (key, params) => {
    if (!options.missingActiveTitleKey && key === 'nav.activeRequestsTitle') return `请求中[${params.count}]-`;
    return key;
  };
  Object.assign(global, globals);
  Object.defineProperty(global, 'location', { value: { href: '' }, configurable: true });
  Object.defineProperty(global, 'navigator', { value: {}, configurable: true });
  global.window = global;
  global.globalThis = global;
  global.window.dispatchEvent = () => {};
  global.hljs = require('./highlight.min.js');

  return { doc, intervals };
}

async function loadTopbarWithActiveRequests(activePayloads, options = {}) {
  const ctx = installTopbarTestGlobals(activePayloads, options);
  delete require.cache[require.resolve('./ui.js')];
  require('./ui.js');
  global.initTopbar('logs');
  await new Promise(resolve => setImmediate(resolve));
  await new Promise(resolve => setImmediate(resolve));
  return ctx;
}

test('initTopbar flashes browser title while active requests exist', async () => {
  const ctx = await loadTopbarWithActiveRequests([
    { success: true, count: 2, data: [] }
  ], { missingActiveTitleKey: true });

  const activeTitle = '请求中[2]-请求日志 - Claude Code & Codex Proxy';
  assert.equal(ctx.doc.title, activeTitle);

  const titleTimer = ctx.intervals.find(item => item.ms !== 2000 && !item.cleared);
  assert.ok(titleTimer);
  titleTimer.fn();
  assert.equal(ctx.doc.title, '请求日志 - Claude Code & Codex Proxy');
  titleTimer.fn();
  assert.equal(ctx.doc.title, activeTitle);
});

test('initTopbar redraws favicon badge as a breathing color dot while active requests exist', async () => {
  const ctx = await loadTopbarWithActiveRequests([
    { success: true, count: 2, data: [] }
  ]);

  const links = ctx.doc.querySelectorAll('link[rel~="icon"]');
  assert.equal(links.length, 4);
  assert.equal(links[0].href, '/web/favicon.svg');
  assert.equal(links[1].href, '/web/favicon.ico');
  const dynamicLinks = links.filter((link) => link.getAttribute('data-dynamic-favicon') === '1');
  assert.equal(dynamicLinks.length, 2);
  assert.equal(dynamicLinks[0].rel, 'shortcut icon');
  assert.equal(dynamicLinks[1].rel, 'icon');
  const firstHref = dynamicLinks[1].href;
  assert.match(firstHref, /^data:image\/png;base64,/);
  assert.equal(dynamicLinks[0].type, 'image/png');
  assert.equal(dynamicLinks[1].type, 'image/png');

  const titleTimer = ctx.intervals.find(item => item.ms !== 2000 && !item.cleared);
  assert.ok(titleTimer);
  titleTimer.fn();
  await new Promise(resolve => setImmediate(resolve));

  const nextDynamicLinks = ctx.doc.querySelectorAll('link[rel~="icon"]')
    .filter((link) => link.getAttribute('data-dynamic-favicon') === '1');
  assert.equal(nextDynamicLinks.length, 2);
  assert.match(nextDynamicLinks[1].href, /^data:image\/png;base64,/);
  assert.notEqual(nextDynamicLinks[1].href, firstHref);
});

test('initTopbar restores browser title when active requests finish', async () => {
  const ctx = await loadTopbarWithActiveRequests([
    { success: true, count: 1, data: [] },
    { success: true, count: 0, data: [] }
  ]);

  const pollTimer = ctx.intervals.find(item => item.ms === 2000 && !item.cleared);
  assert.ok(pollTimer);

  pollTimer.fn();
  await new Promise(resolve => setImmediate(resolve));

  assert.equal(ctx.doc.title, '请求日志 - Claude Code & Codex Proxy');
  const titleTimer = ctx.intervals.find(item => item.ms !== 2000);
  assert.equal(titleTimer.cleared, true);
  const links = ctx.doc.querySelectorAll('link[rel~="icon"]');
  assert.equal(links.length, 4);
  const dynamicLinks = links.filter((link) => link.getAttribute('data-dynamic-favicon') === '1');
  assert.equal(dynamicLinks.length, 2);
  assert.equal(dynamicLinks[0].href, '/web/favicon.ico');
  assert.equal(dynamicLinks[1].href, '/web/favicon.ico');
  assert.equal(dynamicLinks[0].type, 'image/x-icon');
  assert.equal(dynamicLinks[1].type, 'image/x-icon');
});
