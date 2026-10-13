const test = require('node:test');
const assert = require('node:assert/strict');

class FakeClassList {
  constructor(...values) {
    this.values = new Set(values.filter(Boolean));
  }

  add(...values) {
    values.forEach(value => this.values.add(value));
  }

  remove(...values) {
    values.forEach(value => this.values.delete(value));
  }

  contains(value) {
    return this.values.has(value);
  }

  [Symbol.iterator]() {
    return this.values[Symbol.iterator]();
  }
}

class FakeElement {
  constructor(tagName, document) {
    this.tagName = tagName.toUpperCase();
    this.ownerDocument = document;
    this.attributes = new Map();
    this.classList = new FakeClassList();
    this.listeners = new Map();
    this.children = [];
    this.dataset = {};
    this.disabled = false;
    this.isConnected = true;
  }

  append(...children) {
    this.children.push(...children);
    children.forEach(child => { child.parentElement = this; });
  }

  setAttribute(name, value) {
    this.attributes.set(name, String(value));
  }

  getAttribute(name) {
    return this.attributes.get(name) || null;
  }

  removeAttribute(name) {
    this.attributes.delete(name);
  }

  addEventListener(type, listener) {
    if (!this.listeners.has(type)) this.listeners.set(type, []);
    this.listeners.get(type).push(listener);
  }

  dispatchEvent(event) {
    event.target = this;
    for (const listener of this.listeners.get(event.type) || []) listener(event);
    return true;
  }

  focus() {
    this.focused = true;
  }

  remove() {
    this.removed = true;
  }
}

class FakeSelect extends FakeElement {
  constructor(document, options, value) {
    super('select', document);
    this.options = options;
    this._value = value;
    this._disabled = false;
    this.required = true;
    this.validity = { valid: true };
    this.id = 'oauthProviderSelect';
    this.className = 'form-input';
    this.classList = new FakeClassList('form-input');
    this.multiple = false;
    this.events = [];
  }

  get value() {
    return this._value;
  }

  set value(value) {
    const normalized = String(value);
    this._value = this.options.some(option => option.value === normalized) ? normalized : '';
  }

  get selectedIndex() {
    return this.options.findIndex(option => option.value === this._value);
  }

  set selectedIndex(index) {
    this._value = this.options[index]?.value || '';
  }

  get disabled() {
    return this._disabled;
  }

  set disabled(value) {
    this._disabled = Boolean(value);
  }

  after(element) {
    this.enhancedSibling = element;
  }

  matches() {
    return false;
  }

  dispatchEvent(event) {
    this.events.push(event.type);
    return super.dispatchEvent(event);
  }
}

test('native select enhancement preserves semantic values, events and disabled state', () => {
  const previousCombobox = global.createSearchableCombobox;
  const previousEvent = global.Event;
  const label = { textContent: '认证类型', htmlFor: 'oauthProviderSelect' };
  const elements = [];
  const document = {
    defaultView: null,
    createElement(tagName) {
      const element = new FakeElement(tagName, document);
      elements.push(element);
      return element;
    },
    querySelectorAll() {
      return [label];
    }
  };
  class FakeEvent {
    constructor(type, options = {}) {
      this.type = type;
      this.bubbles = options.bubbles;
    }
  }
  document.defaultView = { Event: FakeEvent };

  let comboboxConfig;
  let comboboxValue = '';
  global.Event = FakeEvent;
  global.createSearchableCombobox = (config) => {
    comboboxConfig = config;
    comboboxValue = config.initialValue;
    return {
      setValue(value, displayLabel) {
        comboboxValue = value;
        elements.find(element => element.id === config.inputId).value = displayLabel;
      },
      refresh() {}
    };
  };

  const modulePath = require.resolve('./searchable-select.js');
  delete require.cache[modulePath];
  try {
    const { enhanceNativeSelect } = require(modulePath);
    const select = new FakeSelect(document, [
      { value: 'codex', label: 'Codex', disabled: false },
      { value: 'antigravity', label: 'Antigravity', disabled: false },
      { value: 'loading', label: '加载中', disabled: true },
      { value: 'zed', label: 'Zed', disabled: false },
      { value: 'custom', label: '自定义', disabled: false },
      { value: '__custom_reselect__', label: '自定义', disabled: false, hidden: true }
    ], 'antigravity');

    const instance = enhanceNativeSelect(select);

    assert.ok(instance);
    assert.equal(instance.input.value, 'Antigravity');
    assert.equal(instance.input.getAttribute('aria-labelledby'), label.id);
    assert.equal(label.htmlFor, instance.input.id);
    assert.equal(comboboxConfig.getOptions()[2].disabled, true);
    assert.deepEqual(
      comboboxConfig.getOptions().map(option => option.value),
      ['codex', 'antigravity', 'loading', 'zed', 'custom']
    );

    select.value = 'zed';
    assert.equal(comboboxValue, 'zed');
    assert.equal(instance.input.value, 'Zed');

    comboboxConfig.onSelect('codex', 'Codex');
    assert.equal(select.value, 'codex');
    assert.deepEqual(select.events.slice(-2), ['input', 'change']);

    let reselectedCustom = false;
    select.addEventListener('pointerdown', () => {
      if (select.value === 'custom') {
        reselectedCustom = true;
        select.value = '__custom_reselect__';
      }
    });
    select.value = 'custom';
    select.events.length = 0;
    comboboxConfig.onSelect('custom', '自定义');
    assert.equal(reselectedCustom, true);
    assert.equal(select.value, 'custom');
    assert.deepEqual(select.events, ['pointerdown', 'input', 'change']);

    select.disabled = true;
    assert.equal(instance.input.disabled, true);
    assert.equal(instance.input.getAttribute('aria-disabled'), 'true');
  } finally {
    delete require.cache[modulePath];
    if (previousCombobox === undefined) delete global.createSearchableCombobox;
    else global.createSearchableCombobox = previousCombobox;
    if (previousEvent === undefined) delete global.Event;
    else global.Event = previousEvent;
  }
});

test('detached select is skipped and enhanced once reconnected', () => {
  const previousCombobox = global.createSearchableCombobox;
  const document = {
    defaultView: null,
    createElement(tagName) {
      return new FakeElement(tagName, document);
    },
    querySelectorAll() {
      return [];
    }
  };
  let comboboxCalls = 0;
  global.createSearchableCombobox = (config) => {
    comboboxCalls += 1;
    return {
      setValue() {},
      refresh() {}
    };
  };

  const modulePath = require.resolve('./searchable-select.js');
  delete require.cache[modulePath];
  try {
    const { enhanceNativeSelect } = require(modulePath);
    const select = new FakeSelect(document, [
      { value: 'codex', label: 'Codex', disabled: false }
    ], 'codex');
    select.isConnected = false;

    // 游离节点直接跳过:不创建 combobox、不产生 wrapper
    assert.equal(enhanceNativeSelect(select), null);
    assert.equal(comboboxCalls, 0);
    assert.equal(select.enhancedSibling, undefined);

    // 重新接入文档树后可正常增强
    select.isConnected = true;
    const instance = enhanceNativeSelect(select);
    assert.ok(instance);
    assert.equal(comboboxCalls, 1);
  } finally {
    delete require.cache[modulePath];
    if (previousCombobox === undefined) delete global.createSearchableCombobox;
    else global.createSearchableCombobox = previousCombobox;
  }
});

class FakeBox extends FakeElement {
  constructor(tagName, document) {
    super(tagName, document);
    this.style = {};
    this.className = '';
    this.rect = { top: 0, bottom: 0, left: 0, width: 0 };
  }

  set innerHTML(_) {
    this.children = [];
  }

  appendChild(child) {
    this.append(child);
    return child;
  }

  hasClass(name) {
    return this.className.split(/\s+/).includes(name) || this.classList.contains(name);
  }

  closest(selector) {
    for (let node = this; node; node = node.parentElement) {
      if (node.hasClass?.(selector.replace(/^\./, ''))) return node;
    }
    return null;
  }

  contains(node) {
    for (; node; node = node.parentElement) {
      if (node === this) return true;
    }
    return false;
  }

  querySelectorAll(selector) {
    const name = selector.replace(/^\./, '');
    const found = [];
    const visit = (node) => node.children?.forEach((child) => {
      if (child.hasClass?.(name)) found.push(child);
      visit(child);
    });
    visit(this);
    return found;
  }

  querySelector(selector) {
    return this.querySelectorAll(selector)[0] || null;
  }

  getBoundingClientRect() {
    return this.rect;
  }
}

test('combobox dropdown re-anchors to the input when filtering shrinks an upward list', () => {
  const rowHeight = 30;
  const previousGlobals = new Map();
  const setGlobal = (key, value) => {
    previousGlobals.set(key, Object.getOwnPropertyDescriptor(global, key));
    Object.defineProperty(global, key, { configurable: true, writable: true, value });
  };
  const noop = () => {};
  const document = {
    addEventListener: noop,
    removeEventListener: noop,
    querySelectorAll: () => [],
    querySelector: () => null,
    getElementById: id => elements[id] || null,
    createElement: tagName => new FakeBox(tagName, document),
    createRange: () => ({ selectNodeContents: noop, getBoundingClientRect: () => ({ width: 100 }) })
  };
  document.body = new FakeBox('body', document);
  document.documentElement = new FakeBox('html', document);

  const wrapper = new FakeBox('div', document);
  wrapper.className = 'filter-combobox-wrapper';
  const input = new FakeBox('input', document);
  input.value = '';
  input.placeholder = '';
  // 视口高 600：输入框下方 158px、上方 388px，完整列表放不下只能向上展开
  input.rect = { top: 400, bottom: 430, left: 100, width: 300 };
  const dropdown = new FakeBox('div', document);
  Object.defineProperty(dropdown, 'offsetHeight', {
    get() {
      const natural = this.children.length * rowHeight;
      const max = Number.parseFloat(this.style.maxHeight);
      return Number.isFinite(max) ? Math.min(natural, max) : natural;
    }
  });
  dropdown.offsetWidth = 0;
  dropdown.clientWidth = 0;
  dropdown.scrollTop = 0;
  wrapper.append(input, dropdown);
  const elements = { 'model-input': input, 'model-dropdown': dropdown };

  setGlobal('window', {
    innerWidth: 1000,
    innerHeight: 600,
    location: { pathname: '/', search: '', href: '' },
    addEventListener: noop,
    removeEventListener: noop,
    dispatchEvent: noop,
    matchMedia: () => ({ matches: false, addEventListener: noop })
  });
  setGlobal('localStorage', { getItem: () => null, setItem: noop, removeItem: noop });
  setGlobal('document', document);
  setGlobal('CustomEvent', function CustomEvent() {});

  const uiPath = require.resolve('./ui.js');
  try {
    delete require.cache[uiPath];
    require(uiPath);
    const options = Array.from({ length: 20 }, (_, i) => ({ value: `model-${i}`, label: `model-${i}` }));
    options.push({ value: 'gpt-5.6-luna', label: 'gpt-5.6-luna' }, { value: 'gpt-6-luna', label: 'gpt-6-luna' });
    global.window.createSearchableCombobox({
      attachMode: true,
      inputId: 'model-input',
      dropdownId: 'model-dropdown',
      showAllOptionsOnOpen: true,
      getOptions: () => options
    });

    input.dispatchEvent({ type: 'mousedown' });
    assert.equal(dropdown.style.maxHeight, '388px');
    assert.equal(dropdown.style.top, '6px');

    input.value = 'lu';
    input.dispatchEvent({ type: 'input' });
    assert.equal(dropdown.children.length, 2);
    assert.equal(dropdown.style.maxHeight, '');
    assert.equal(dropdown.style.top, '436px');
  } finally {
    delete require.cache[uiPath];
    for (const [key, descriptor] of previousGlobals) {
      if (descriptor === undefined) delete global[key];
      else Object.defineProperty(global, key, descriptor);
    }
  }
});
