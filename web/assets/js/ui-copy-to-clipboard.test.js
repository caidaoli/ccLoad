const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const uiSource = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');

function extractSharedUiHelpers(source) {
  const startMarker = '// 跨页面共享工具函数';
  const endMarker = 'window.initTimeRangeSelector = initTimeRangeSelector;';
  const start = source.indexOf(startMarker);
  const end = source.indexOf(endMarker);

  assert.notEqual(start, -1, '找不到共享工具函数区块起点');
  assert.notEqual(end, -1, '找不到共享工具函数区块导出语句');

  const iifeEnd = source.indexOf('})();', end);
  assert.notEqual(iifeEnd, -1, '找不到共享工具函数区块结束');

  return source.slice(start, iifeEnd + 4);
}

function loadClipboardHelper({ navigator, execCommandResult = true } = {}) {
  const appended = [];
  const removed = [];
  const body = {
    appendChild(node) {
      appended.push(node);
    },
    removeChild(node) {
      removed.push(node);
    }
  };

  const document = {
    body,
    createElement(tag) {
      assert.equal(tag, 'textarea');
      return {
        value: '',
        style: {},
        selectCalled: false,
        select() {
          this.selectCalled = true;
        }
      };
    },
    execCommand(command) {
      assert.equal(command, 'copy');
      return execCommandResult;
    }
  };

  const sandbox = {
    console,
    document,
    navigator: navigator || {},
    window: {}
  };

  vm.createContext(sandbox);
  vm.runInContext(extractSharedUiHelpers(uiSource), sandbox);

  return {
    appended,
    removed,
    document,
    copyToClipboard: sandbox.window.copyToClipboard
  };
}

test('copyToClipboard 在缺少 Clipboard API 时走 execCommand 降级', async () => {
  const { appended, removed, copyToClipboard } = loadClipboardHelper();

  await assert.doesNotReject(() => copyToClipboard('hash-value'));
  assert.equal(appended.length, 1);
  assert.equal(removed.length, 1);
  assert.equal(appended[0], removed[0]);
  assert.equal(appended[0].value, 'hash-value');
  assert.equal(appended[0].selectCalled, true);
});
