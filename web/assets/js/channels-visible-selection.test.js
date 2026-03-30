const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, 'channels-modals.js'), 'utf8');
const zhLocaleSource = fs.readFileSync(path.join(__dirname, '..', 'locales', 'zh-CN.js'), 'utf8');
const enLocaleSource = fs.readFileSync(path.join(__dirname, '..', 'locales', 'en.js'), 'utf8');

function createElement() {
  const attrs = new Map();
  const classes = new Map();

  return {
    textContent: '',
    title: '',
    disabled: false,
    checked: false,
    indeterminate: false,
    classList: {
      toggle(className, enabled) {
        classes.set(className, Boolean(enabled));
      },
      contains(className) {
        return classes.get(className) === true;
      }
    },
    setAttribute(name, value) {
      attrs.set(name, value);
    },
    getAttribute(name) {
      return attrs.get(name);
    }
  };
}

function loadSelectionSandbox(overrides = {}) {
  const elements = new Map();
  let filterCalls = 0;

  const sandbox = {
    window: {
      t(key, params = {}) {
        const translations = {
          'channels.batchSelectedCount': `${params.count} selected`,
          'channels.batchSelectVisible': 'Select All',
          'channels.batchDeselectVisible': 'Deselect All'
        };
        return translations[key] || key;
      }
    },
    document: {
      getElementById(id) {
        return elements.get(id) || null;
      }
    },
    selectedChannelIds: new Set(),
    channels: [],
    filteredChannels: [],
    normalizeSelectedChannelID(value) {
      return String(value);
    },
    filterChannels() {
      filterCalls += 1;
    },
    console
  };

  Object.assign(sandbox, overrides);

  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);

  return {
    sandbox,
    elements,
    getFilterCalls() {
      return filterCalls;
    }
  };
}

test('toggleVisibleChannelsSelection 在部分选中时取消当前可见渠道选择', () => {
  const { sandbox, getFilterCalls } = loadSelectionSandbox({
    filteredChannels: [{ id: 1 }, { id: 2 }, { id: 3 }],
    selectedChannelIds: new Set(['1', '4'])
  });

  sandbox.toggleVisibleChannelsSelection();

  assert.deepEqual(Array.from(sandbox.selectedChannelIds).sort(), ['4']);
  assert.equal(getFilterCalls(), 1);
});

test('updateBatchChannelSelectionUI 在仅隐藏渠道被选中时仍显示全选文案', () => {
  const floatingMenu = createElement();
  const summary = createElement();
  const countBadge = createElement();
  const closeBtn = createElement();
  const selectionToggle = createElement();
  const selectionCheckbox = createElement();
  const selectionText = createElement();

  const { sandbox, elements } = loadSelectionSandbox({
    filteredChannels: [{ id: 1 }, { id: 2 }],
    selectedChannelIds: new Set(['99'])
  });

  elements.set('batchFloatingMenu', floatingMenu);
  elements.set('selectedChannelsSummary', summary);
  elements.set('selectedChannelsCountBadge', countBadge);
  elements.set('batchFloatingMenuCloseBtn', closeBtn);
  elements.set('visibleSelectionToggle', selectionToggle);
  elements.set('visibleSelectionCheckbox', selectionCheckbox);
  elements.set('visibleSelectionToggleText', selectionText);

  sandbox.updateBatchChannelSelectionUI();

  assert.equal(selectionText.textContent, 'Select All');
  assert.equal(selectionToggle.title, 'Select All');
  assert.equal(selectionCheckbox.checked, false);
  assert.equal(selectionCheckbox.indeterminate, false);
});

test('channels 可见选择文案包含取消可见项的翻译键', () => {
  assert.match(zhLocaleSource, /'channels\.batchDeselectVisible': '取消全选'/);
  assert.match(enLocaleSource, /'channels\.batchDeselectVisible': 'Deselect All'/);
});
