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
        'logs.debugUnavailableTitle': '这条记录没有可查看的 Debug 日志',
        'logs.debugUnavailableHintDisabled': '该请求发生时未开启 Debug 日志，因此没有保存上游原始请求/响应。',
        'logs.debugUnavailableHintExpired': '当前已开启 Debug 日志，更可能是这条记录已超过保留时长被自动清理。',
        'logs.debugUnavailableHintGeneric': '当前无法定位这条请求对应的 Debug 日志，请检查相关设置。',
        'logs.debugUnavailableSettingsTitle': '当前相关设置',
        'settings.desc.debug_log_enabled': '启用Debug日志(记录上游请求/响应原始数据)',
        'settings.desc.debug_log_retention_minutes': 'Debug日志保留时长(分钟,1-1440)',
        'logs.debugSettingEnabledOn': '已开启',
        'logs.debugSettingEnabledOff': '已关闭',
        'logs.debugSettingRetentionMinutes': `${params.minutes} 分钟`
      };
      return dict[key] || key;
    }
  };

  vm.createContext(sandbox);
  vm.runInContext(`
${extractFunction(logsSource, 'canInspectDebugLog')}
${extractFunction(logsSource, 'buildLogMessageContent')}
${extractFunction(logsSource, 'formatDebugSettingValue')}
${extractFunction(logsSource, 'buildDebugLogUnavailableHtml')}
this.__logsDebugDetailTest = {
  canInspectDebugLog,
  buildLogMessageContent,
  formatDebugSettingValue,
  buildDebugLogUnavailableHtml
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

test('无 debug 日志时的提示应展示更友好的原因和相关设置', () => {
  const helpers = createHelpers();

  const html = helpers.buildDebugLogUnavailableHtml({
    debug_log_enabled: { key: 'debug_log_enabled', value: 'false' },
    debug_log_retention_minutes: { key: 'debug_log_retention_minutes', value: '15' }
  });

  assert.match(html, /这条记录没有可查看的 Debug 日志/);
  assert.match(html, /当前相关设置/);
  assert.match(html, /已关闭/);
  assert.match(html, /15 分钟/);
});
