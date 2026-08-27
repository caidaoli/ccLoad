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
    this.disabled = false;
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
    assert.equal(comboboxConfig.attachMode, true);
    assert.equal(comboboxConfig.showAllOptionsOnOpen, true);
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
