const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const logsScript = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');

function extractFunction(source, name) {
  const pattern = new RegExp(`function ${name}\\([^)]*\\) \\{[\\s\\S]*?\\n\\}`, 'm');
  const match = source.match(pattern);
  assert.ok(match, `缺少函数 ${name}`);
  return match[0];
}

test('日志页进行中请求列数只基于日志表自身，不受渠道弹窗额外表头影响', () => {
  const logsTable = {
    querySelectorAll(selector) {
      assert.equal(selector, 'thead th');
      return new Array(14).fill({});
    }
  };

  const context = {
    document: {
      getElementById(id) {
        assert.equal(id, 'tbody');
        return {
          closest(selector) {
            assert.equal(selector, 'table');
            return logsTable;
          }
        };
      },
      querySelectorAll(selector) {
        if (selector === 'thead th') {
          return new Array(24).fill({});
        }
        return [];
      }
    }
  };

  const getTableColspan = vm.runInNewContext(
    `(${extractFunction(logsScript, 'getTableColspan')})`,
    context
  );

  assert.equal(getTableColspan(), 14);
});
