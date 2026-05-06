const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');

function extractFunction(source, name) {
  const signature = `function ${name}(`;
  const start = source.indexOf(signature);
  assert.notEqual(start, -1, `缺少函数 ${name}`);

  const bodyStart = source.indexOf('{', start);
  assert.notEqual(bodyStart, -1, `函数 ${name} 缺少起始大括号`);

  let depth = 0;
  for (let i = bodyStart; i < source.length; i++) {
    const ch = source[i];
    if (ch === '{') depth++;
    if (ch === '}') {
      depth--;
      if (depth === 0) {
        return source.slice(start, i + 1);
      }
    }
  }

  assert.fail(`函数 ${name} 大括号未闭合`);
}

function createHelpers() {
  const sandbox = {
    escapeHtml(value) {
      return String(value ?? '');
    },
    formatBytes(bytes) {
      if (bytes == null || bytes <= 0) return '';
      const units = ['B', 'K', 'M', 'G'];
      const factor = 1024;
      const i = Math.min(Math.floor(Math.log(bytes) / Math.log(factor)), units.length - 1);
      const value = bytes / Math.pow(factor, i);
      return value.toFixed(i > 0 ? 1 : 0) + ' ' + units[i];
    },
    t(key) {
      return key === 'logs.debugLogTitle' ? 'Debug Log' : key;
    }
  };

  vm.createContext(sandbox);
  vm.runInContext(`
${extractFunction(logsSource, 'buildActiveRequestInfoContent')}
this.__logsActiveRequestDebugTest = { buildActiveRequestInfoContent };
`, sandbox);

  return sandbox.__logsActiveRequestDebugTest;
}

test('实时请求信息列在 debug 可用时渲染为可点击入口', () => {
  const helpers = createHelpers();

  const html = helpers.buildActiveRequestInfoContent({
    id: 7,
    bytes_received: 0,
    debug_log_available: true
  });

  assert.match(html, /class="debug-log-link has-upstream-detail"/);
  assert.match(html, /data-active-request-id="7"/);
  assert.match(html, /请求处理中\.\.\./);
});

test('实时请求信息列在 debug 不可用时保持普通文本', () => {
  const helpers = createHelpers();

  const html = helpers.buildActiveRequestInfoContent({
    id: 7,
    bytes_received: 1024,
    debug_log_available: false
  });

  assert.doesNotMatch(html, /debug-log-link/);
  assert.match(html, /已接收 1\.0 K/);
});

test('实时请求 debug 入口使用 active request debug modal', () => {
  assert.match(logsSource, /debug-log-link\[data-active-request-id\]/);
  assert.match(logsSource, /showActiveDebugLogModal\(activeRequestId\)/);
});
