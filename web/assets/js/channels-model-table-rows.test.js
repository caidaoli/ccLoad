const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const stateSource = fs.readFileSync(path.join(__dirname, 'channels-state.js'), 'utf8');
const protocolSource = fs.readFileSync(path.join(__dirname, 'channels-protocols.js'), 'utf8');
const modalsSource = fs.readFileSync(path.join(__dirname, 'channels-modals.js'), 'utf8');
const css = fs.readFileSync(path.join(__dirname, '..', 'css', 'channels.css'), 'utf8');

function createHarness() {
  const styleValues = new Map();
  const modelTableContainer = {
    dataset: {},
    style: {
      setProperty(name, value) {
        styleValues.set(name, value);
      }
    }
  };
  const redirectBody = {
    closest(selector) {
      return selector === '.inline-table-container' ? modelTableContainer : null;
    }
  };

  const sandbox = {
    console,
    document: {
      querySelector(selector) {
        return selector === '#redirectTableBody' ? redirectBody : null;
      }
    },
    localStorage: {
      getItem() {
        return null;
      }
    },
    window: {}
  };
  sandbox.window = sandbox.window || {};

  vm.createContext(sandbox);
  vm.runInContext(`${stateSource}
${protocolSource}
${modalsSource}
this.__modelTableRowsTest = {
  calculateModelTableVisibleRows,
  syncChannelModelTableRows,
  setTableRows(urlCount, keyCount) {
    inlineURLTableData = Array.from({ length: urlCount }, (_, index) => 'url-' + index);
    inlineKeyTableData = Array.from({ length: keyCount }, (_, index) => 'key-' + index);
  }
};`, sandbox);

  return {
    api: sandbox.__modelTableRowsTest,
    styleValues,
    modelTableContainer
  };
}

test('模型表格可见行数随 URL 和 Key 行数自动变化', () => {
  const { api } = createHarness();

  assert.equal(api.calculateModelTableVisibleRows(1, 1), 12);
  assert.equal(api.calculateModelTableVisibleRows(2, 2), 10);
  assert.equal(api.calculateModelTableVisibleRows(3, 4), 7);
  assert.equal(api.calculateModelTableVisibleRows(10, 99), 7);
});

test('同步模型表格行数时写入容器 CSS 变量', () => {
  const { api, styleValues, modelTableContainer } = createHarness();

  api.setTableRows(1, 1);
  api.syncChannelModelTableRows();

  assert.equal(styleValues.get('--channel-model-visible-rows'), '12');
  assert.equal(modelTableContainer.dataset.visibleRows, '12');

  api.setTableRows(3, 4);
  api.syncChannelModelTableRows();

  assert.equal(styleValues.get('--channel-model-visible-rows'), '7');
  assert.equal(modelTableContainer.dataset.visibleRows, '7');
});

test('模型表格高度由动态行数 CSS 变量控制', () => {
  const block = css.match(/#channelModal\s+\.inline-table-container\.tall\s*\{[^}]+\}/);
  assert.ok(block, '缺少模型表格容器样式');
  assert.match(block[0], /--channel-model-visible-rows:\s*8/);
  assert.match(block[0], /max-height:\s*calc\(\s*var\(--channel-editor-table-header-height\)\s*\+\s*\(var\(--channel-model-visible-rows\)\s*\*\s*var\(--channel-editor-table-row-height\)\)\s*\)/);
});
