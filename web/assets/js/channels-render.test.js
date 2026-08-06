const test = require('node:test');
const assert = require('node:assert/strict');

const { buildOAuthUsageStatusHtml, formatCooldownRecoveryTime } = require('./channels-render.js');

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
