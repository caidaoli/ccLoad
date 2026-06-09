const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');
const uiSource = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');

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

test('活动请求单一轮询源：logs.js 订阅 ui.js 推送，不再独立轮询', () => {
  // logs.js 不应再直接请求 /admin/active-requests（debug-log 子路径用模板字符串，不受影响）
  assert.ok(
    !logsSource.includes("fetchAPIWithAuth('/admin/active-requests')"),
    'logs.js 不应再独立轮询 /admin/active-requests，必须订阅 ui.js 推送'
  );
  // logs.js 不应再保留独立轮询的定时器/启动函数
  assert.ok(
    !logsSource.includes('ensureActiveRequestsPollingStarted'),
    'logs.js 不应再保留独立轮询启动函数'
  );
  assert.ok(
    !logsSource.includes('activeRequestsPollTimer'),
    'logs.js 不应再保留独立轮询定时器变量'
  );
  // logs.js 通过 window.onActiveRequestsData 订阅活动请求数据
  assert.ok(
    logsSource.includes('window.onActiveRequestsData(handleActiveRequestsData)'),
    'logs.js 应通过 window.onActiveRequestsData 订阅活动请求数据'
  );
});

test('ui.js 是活动请求唯一轮询源，暴露订阅接口并向订阅者推送数据', () => {
  // ui.js 是 /admin/active-requests 唯一的网络请求点
  assert.ok(
    uiSource.includes("fetchAPIWithAuth('/admin/active-requests')"),
    'ui.js 应是 /admin/active-requests 的唯一轮询源'
  );
  // 暴露订阅接口
  assert.ok(
    uiSource.includes('window.onActiveRequestsData = onActiveRequestsData'),
    'ui.js 应暴露 window.onActiveRequestsData 订阅接口'
  );
  // 轮询后向订阅者列表推送数据
  assert.ok(
    uiSource.includes('_activeDataListeners'),
    'ui.js 应维护订阅者列表并推送活动请求数据'
  );
});
