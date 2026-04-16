const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const uiSource = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');
const channelsTestSource = fs.readFileSync(path.join(__dirname, 'channels-test.js'), 'utf8');
const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');
const modelTestSource = fs.readFileSync(path.join(__dirname, 'model-test.js'), 'utf8');
const sharedCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'styles.css'), 'utf8');

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

function loadSharedHelpers() {
  const sandbox = {
    console,
    document: {
      body: {},
      createElement() {
        return { style: {}, select() {} };
      }
    },
    navigator: {},
    window: {}
  };

  vm.createContext(sandbox);
  vm.runInContext(extractSharedUiHelpers(uiSource), sandbox);
  return sandbox.window;
}

test('ui.js 暴露共享上游详情高亮 helper，并高亮请求首行、Header 和 JSON token', () => {
  const window = loadSharedHelpers();

  assert.equal(typeof window.renderUpstreamCodeBlock, 'function');

  const html = window.renderUpstreamCodeBlock(
    [
      'POST https://example.com/v1/messages',
      'Content-Type: application/json',
      'X-Retry: 3',
      '',
      '{',
      '  "model": "qwen3-coder-flash",',
      '  "max_tokens": 32000,',
      '  "stream": true,',
      '  "metadata": null',
      '}'
    ].join('\n'),
    'request'
  );

  assert.match(html, /upstream-token upstream-token--method/);
  assert.match(html, /upstream-token upstream-token--url/);
  assert.match(html, /upstream-token upstream-token--header-key/);
  assert.match(html, /upstream-token upstream-token--json-key/);
  assert.match(html, /upstream-token upstream-token--json-string/);
  assert.match(html, /upstream-token upstream-token--json-number/);
  assert.match(html, /upstream-token upstream-token--json-boolean/);
  assert.match(html, /upstream-token upstream-token--json-null/);
});

test('ui.js 高亮响应状态码时按状态类别输出不同 class', () => {
  const window = loadSharedHelpers();
  const okHtml = window.renderUpstreamCodeBlock('HTTP 204', 'response');
  const clientErrHtml = window.renderUpstreamCodeBlock('HTTP 429', 'response');
  const serverErrHtml = window.renderUpstreamCodeBlock('HTTP 503', 'response');

  assert.match(okHtml, /upstream-token upstream-token--status-success/);
  assert.match(clientErrHtml, /upstream-token upstream-token--status-client-error/);
  assert.match(serverErrHtml, /upstream-token upstream-token--status-server-error/);
});

test('channels-test.js、logs.js 和 model-test.js 复用共享上游详情高亮 helper', () => {
  assert.match(channelsTestSource, /window\.setHighlightedCodeContent\(/);
  assert.match(logsSource, /window\.setHighlightedCodeContent\(/);
  assert.match(modelTestSource, /window\.setHighlightedCodeContent\(/);
});

test('共享样式为上游详情 token 提供颜色类', () => {
  assert.match(sharedCss, /\.upstream-token--method(?:\s*,|\s*\{)/);
  assert.match(sharedCss, /\.upstream-token--url\s*\{/);
  assert.match(sharedCss, /\.upstream-token--header-key\s*\{/);
  assert.match(sharedCss, /\.upstream-token--json-key\s*\{/);
  assert.match(sharedCss, /\.upstream-token--json-string\s*\{/);
  assert.match(sharedCss, /\.upstream-token--json-number\s*\{/);
  assert.match(sharedCss, /\.upstream-token--json-boolean\s*\{/);
  assert.match(sharedCss, /\.upstream-token--json-null\s*\{/);
  assert.match(sharedCss, /\.upstream-token--sse-field\s*\{/);
  assert.match(sharedCss, /\.upstream-token--sse-event-name\s*\{/);
  assert.match(sharedCss, /\.upstream-token--sse-comment\s*\{/);
});

test('ui.js 高亮 SSE 响应体中的事件名与 JSON 数据', () => {
  const window = loadSharedHelpers();

  const html = window.renderUpstreamCodeBlock(
    [
      'HTTP 200',
      'Content-Type: text/event-stream;charset=UTF-8',
      '',
      'event:response.created',
      'data:{"type":"response.created","id":"resp_001"}',
      '',
      ':keep-alive',
      'data:[DONE]'
    ].join('\n'),
    'response'
  );

  assert.match(html, /upstream-token--status-success/);
  assert.match(html, /upstream-token--sse-field/);
  assert.match(html, /upstream-token--sse-event-name/);
  assert.match(html, /upstream-token--json-key/);
  assert.match(html, /upstream-token--json-string/);
  assert.match(html, /upstream-token--sse-comment/);
});
