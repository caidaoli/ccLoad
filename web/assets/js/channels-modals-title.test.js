const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, 'channels-modals.js'), 'utf8');

function createTitleElement() {
  const attributes = new Map([
    ['data-i18n', 'channels.modalAddTitle']
  ]);

  return {
    textContent: '添加渠道',
    setAttribute(name, value) {
      attributes.set(name, value);
    },
    getAttribute(name) {
      return attributes.get(name);
    }
  };
}

test('setChannelModalTitle 同步更新弹窗标题文本和国际化键', () => {
  const titleEl = createTitleElement();
  const sandbox = {
    window: {
      t(key) {
        const translations = {
          'channels.addChannel': '添加渠道',
          'channels.editChannel': '编辑渠道',
          'channels.copyChannel': '复制渠道'
        };
        return translations[key] || key;
      }
    },
    document: {
      getElementById(id) {
        assert.equal(id, 'modalTitle');
        return titleEl;
      }
    },
    console
  };

  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);

  assert.equal(typeof sandbox.setChannelModalTitle, 'function');

  sandbox.setChannelModalTitle('channels.editChannel');

  assert.equal(titleEl.textContent, '编辑渠道');
  assert.equal(titleEl.getAttribute('data-i18n'), 'channels.editChannel');
});
