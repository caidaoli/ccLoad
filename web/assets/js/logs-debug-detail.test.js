const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');

function extractFunction(source, name) {
  const start = source.indexOf(`function ${name}(`);
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
    renderLogSourceBadge() {
      return '';
    },
    escapeHtml(value) {
      return String(value ?? '');
    },
    t(key, params = {}) {
      const dict = {
        'logs.debugSettingRetentionMinutes': `${params.minutes} 分钟`
      };
      return dict[key] || key;
    }
  };

  vm.createContext(sandbox);
  vm.runInContext(`
${extractFunction(logsSource, 'canInspectDebugLog')}
${extractFunction(logsSource, 'buildLogMessageContent')}
this.__logsDebugDetailTest = {
  canInspectDebugLog,
  buildLogMessageContent
};
`, sandbox);

  return sandbox.__logsDebugDetailTest;
}

test('系统日志不应渲染为可点击的 debug 详情入口', () => {
  const helpers = createHelpers();

  assert.equal(helpers.canInspectDebugLog({ channel_id: 0 }), false);
  assert.equal(helpers.canInspectDebugLog({ channel_id: 12 }), true);
  assert.doesNotMatch(
    helpers.buildLogMessageContent({ id: 1, channel_id: 0, message: '系统汇总日志' }),
    /debug-log-link/
  );
  assert.match(
    helpers.buildLogMessageContent({ id: 2, channel_id: 12, message: 'upstream status 500' }),
    /class="debug-log-link has-upstream-detail"/
  );
});
