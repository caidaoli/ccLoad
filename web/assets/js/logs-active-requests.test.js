const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');

function extractFunction(source, name) {
  const pattern = new RegExp(`function ${name}\\([^)]*\\) \\{[\\s\\S]*?\\n\\}`, 'm');
  const match = source.match(pattern);
  assert.ok(match, `缺少函数 ${name}`);
  return match[0];
}

test('全部日志视图仍会保留进行中请求轮询', () => {
  const shouldSkipActiveRequestsFetch = vm.runInNewContext(
    `(${extractFunction(logsSource, 'shouldSkipActiveRequestsFetch')})`,
    {}
  );

  assert.equal(shouldSkipActiveRequestsFetch('today', '', 'proxy'), false);
  assert.equal(shouldSkipActiveRequestsFetch('today', '', 'all'), false);
  assert.equal(shouldSkipActiveRequestsFetch('today', '', 'detection'), true);
  assert.equal(shouldSkipActiveRequestsFetch('yesterday', '', 'all'), true);
  assert.equal(shouldSkipActiveRequestsFetch('today', '500', 'all'), true);
});
