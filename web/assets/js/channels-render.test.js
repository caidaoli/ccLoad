const test = require('node:test');
const assert = require('node:assert/strict');

const {
  buildChannelRuntimeStatusHtml,
  buildOAuthPlanBadge,
  buildOAuthUsageStatusHtml,
  formatCooldownRecoveryTime
} = require('./channels-render.js');

const translations = {
  'channels.status.secondsUntilRecovery': '{count}秒后恢复',
  'channels.status.minutesUntilRecovery': '{count}分钟后恢复',
  'channels.status.hoursMinutesUntilRecovery': '{hours}小时{minutes}分后恢复'
};

test('冷却超过一小时后按小时和分钟显示', () => {
  const previousWindow = global.window;
  global.window = {
    t(key, values) {
      return translations[key].replace(/\{(\w+)\}/g, (_, name) => values[name]);
    }
  };

  try {
    assert.equal(formatCooldownRecoveryTime(59 * 60_000), '59分钟后恢复');
    assert.equal(formatCooldownRecoveryTime(60 * 60_000), '1小时0分后恢复');
    assert.equal(formatCooldownRecoveryTime((60 * 60_000) + 1), '1小时1分后恢复');
    assert.equal(formatCooldownRecoveryTime(2990 * 60_000), '49小时50分后恢复');
  } finally {
    global.window = previousWindow;
  }
});

test('Antigravity OAuth 渠道在状态列提供额度刷新操作', () => {
  const previousWindow = global.window;
  const previousGetUsageState = global.getOAuthUsageState;
  const previousReadOnly = global.isTokenChannelsReadOnly;
  global.window = { t: key => key === 'channels.oauth.usageRefresh' ? '刷新额度' : key };
  global.getOAuthUsageState = () => null;
  global.isTokenChannelsReadOnly = () => false;

  try {
    const html = buildOAuthUsageStatusHtml({ id: 25, auth_type: 'antigravity_oauth' });
    assert.match(html, /data-action="refresh-oauth-usage"/);
    assert.match(html, /data-channel-id="25"/);
    assert.match(html, />刷新额度<\/button>/);
    assert.equal(buildOAuthUsageStatusHtml({ id: 26, auth_type: 'api_key' }), '');
  } finally {
    global.window = previousWindow;
    global.getOAuthUsageState = previousGetUsageState;
    global.isTokenChannelsReadOnly = previousReadOnly;
  }
});

test('OAuth 额度就绪后只显示额度而不显示最后成功时间', () => {
  const previousWindow = global.window;
  const previousGetUsageState = global.getOAuthUsageState;
  const previousReadOnly = global.isTokenChannelsReadOnly;
  global.window = {
    t(key, values = {}) {
      if (key === 'channels.lastSuccess.minutesAgo') return `${values.count}分钟前`;
      if (key === 'channels.oauth.usageWeekly') return '每周';
      if (key === 'channels.oauth.usageRemaining') return `${values.label}剩余${values.percent}%`;
      if (key === 'channels.oauth.usageRefresh') return '刷新额度';
      return key;
    }
  };
  global.getOAuthUsageState = () => ({
    status: 'ready',
    data: {
      windows: [{
        limit_name: 'Gemini Models',
        limit_window_seconds: 7 * 24 * 60 * 60,
        remaining_percent: 90
      }]
    }
  });
  global.isTokenChannelsReadOnly = () => false;

  try {
    const html = buildChannelRuntimeStatusHtml(
      { id: 25, auth_type: 'antigravity_oauth' },
      { lastSuccessAt: Date.now() - 19 * 60_000 }
    );
    assert.match(html, /role="progressbar"/);
    assert.doesNotMatch(html, /19分钟前/);
  } finally {
    global.window = previousWindow;
    global.getOAuthUsageState = previousGetUsageState;
    global.isTokenChannelsReadOnly = previousReadOnly;
  }
});

test('OAuth 计划徽标支持 Antigravity paidTier 并转义内容', () => {
  assert.match(
    buildOAuthPlanBadge({ auth_type: 'antigravity_oauth', antigravity_paid_tier: 'Google AI <Pro>' }),
    /Google AI &lt;Pro&gt;/
  );
  assert.equal(buildOAuthPlanBadge({ auth_type: 'api_key', antigravity_paid_tier: 'Google AI Pro' }), '');
});
