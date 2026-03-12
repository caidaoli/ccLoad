const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const uiSource = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');

function extractInitTimeRangeSelector(source) {
  const match = source.match(/function initTimeRangeSelector\(onRangeChange\) \{[\s\S]*?\n  \}/);
  assert.ok(match, '找不到 initTimeRangeSelector 定义');
  return `${match[0]}\nwindow.initTimeRangeSelector = initTimeRangeSelector;`;
}

function createButton(range) {
  const listeners = new Map();
  const button = {
    dataset: { range },
    classList: {
      _classes: new Set(),
      add(name) {
        this._classes.add(name);
      },
      remove(name) {
        this._classes.delete(name);
      },
      contains(name) {
        return this._classes.has(name);
      }
    },
    addEventListener(type, handler) {
      const handlers = listeners.get(type) || [];
      handlers.push(handler);
      listeners.set(type, handlers);
    },
    removeEventListener(type, handler) {
      const handlers = listeners.get(type) || [];
      listeners.set(type, handlers.filter((item) => item !== handler));
    },
    dispatch(type) {
      const handlers = listeners.get(type) || [];
      handlers.forEach((handler) => handler.call(button));
    }
  };
  return button;
}

function loadInitTimeRangeSelector(buttons) {
  const sandbox = {
    console,
    document: {
      querySelectorAll(selector) {
        assert.equal(selector, '.time-range-btn');
        return buttons;
      }
    },
    window: {}
  };

  vm.createContext(sandbox);
  vm.runInContext(extractInitTimeRangeSelector(uiSource), sandbox);
  return sandbox.window.initTimeRangeSelector;
}

test('initTimeRangeSelector 重复调用时不会重复绑定 click 事件', () => {
  const buttons = [createButton('today'), createButton('yesterday')];
  const initTimeRangeSelector = loadInitTimeRangeSelector(buttons);
  let calls = 0;

  initTimeRangeSelector(() => {
    calls += 1;
  });
  initTimeRangeSelector(() => {
    calls += 1;
  });

  buttons[0].dispatch('click');

  assert.equal(calls, 1);
  assert.equal(buttons[0].classList.contains('active'), true);
  assert.equal(buttons[1].classList.contains('active'), false);
});

test('initTimeRangeSelector 重复调用时使用最新一次的回调', () => {
  const buttons = [createButton('today'), createButton('yesterday')];
  const initTimeRangeSelector = loadInitTimeRangeSelector(buttons);
  let firstCalls = 0;
  let secondCalls = 0;

  initTimeRangeSelector(() => {
    firstCalls += 1;
  });
  initTimeRangeSelector(() => {
    secondCalls += 1;
  });

  buttons[1].dispatch('click');

  assert.equal(firstCalls, 0);
  assert.equal(secondCalls, 1);
  assert.equal(buttons[0].classList.contains('active'), false);
  assert.equal(buttons[1].classList.contains('active'), true);
});
