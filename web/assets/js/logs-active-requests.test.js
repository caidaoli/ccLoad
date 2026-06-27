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

test('进行中请求令牌列按 token_id 显示令牌描述', () => {
  const buildActiveRequestTokenDescDisplay = vm.runInNewContext(
    `(${extractFunction(logsSource, 'buildActiveRequestTokenDescDisplay')})`,
    {
      authTokens: [{ id: 7, description: 'Ops <Main>' }],
      escapeHtml: (value) => String(value)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;')
    }
  );

  assert.equal(
    buildActiveRequestTokenDescDisplay({ token_id: 7 }),
    '<span title="Ops &lt;Main&gt;">Ops.in&gt;</span>'
  );
  assert.equal(
    buildActiveRequestTokenDescDisplay({ token_id: 8 }),
    '<span title="Token #8">Token #8</span>'
  );
  assert.equal(buildActiveRequestTokenDescDisplay({ token_id: 0 }), '');
});
