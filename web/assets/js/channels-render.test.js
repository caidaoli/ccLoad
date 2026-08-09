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

test('Antigravity 和 Anthropic OAuth 渠道在状态列提供额度刷新操作', () => {
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
    const anthropicHTML = buildOAuthUsageStatusHtml({ id: 27, auth_type: 'anthropic_oauth' });
    assert.match(anthropicHTML, /data-action="refresh-oauth-usage"/);
    assert.match(anthropicHTML, /data-channel-id="27"/);
    assert.equal(buildOAuthUsageStatusHtml({ id: 26, auth_type: 'api_key' }), '');
  } finally {
    global.window = previousWindow;
    global.getOAuthUsageState = previousGetUsageState;
    global.isTokenChannelsReadOnly = previousReadOnly;
  }
});

test('OAuth 额度刷新失败时格式化结构化错误并转义内容', () => {
  const previousWindow = global.window;
  const previousGetUsageState = global.getOAuthUsageState;
  const previousReadOnly = global.isTokenChannelsReadOnly;
  let error = '{"error":"invalid_grant","error_description":"Refresh token not found or invalid"}';
  global.window = {
    t: key => key === 'channels.oauth.usageRefresh' ? '刷新额度' : '额度刷新失败'
  };
  global.getOAuthUsageState = () => ({
    status: 'error',
    error
  });
  global.isTokenChannelsReadOnly = () => false;

  try {
    let html = buildOAuthUsageStatusHtml({ id: 25, auth_type: 'antigravity_oauth' });
    assert.match(html, /Refresh token not found or invalid/);
    assert.doesNotMatch(html, /invalid_grant/);
    assert.doesNotMatch(html, /\{"error"/);
    assert.doesNotMatch(html, /额度刷新失败/);
    assert.match(html, /data-action="refresh-oauth-usage"/);

    error = '{"error":{"type":"invalid_grant","message":"Refresh token <expired>"}}';
    html = buildOAuthUsageStatusHtml({ id: 25, auth_type: 'anthropic_oauth' });
    assert.match(html, /Refresh token &lt;expired&gt;/);
    assert.doesNotMatch(html, /invalid_grant/);
    assert.doesNotMatch(html, /Refresh token <expired>/);

    error = 'network timeout';
    html = buildOAuthUsageStatusHtml({ id: 25, auth_type: 'codex_oauth' });
    assert.match(html, /network timeout/);
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
  const previousGetUsageState = global.getOAuthUsageState;
  global.getOAuthUsageState = () => ({
    status: 'ready',
    data: { plan_type: 'Max 20x <safe>' }
  });
  try {
    assert.match(
      buildOAuthPlanBadge({ auth_type: 'antigravity_oauth', antigravity_paid_tier: 'Google AI <Pro>' }),
      /Google AI &lt;Pro&gt;/
    );
    assert.equal(buildOAuthPlanBadge({ auth_type: 'api_key', antigravity_paid_tier: 'Google AI Pro' }), '');
    assert.match(
      buildOAuthPlanBadge({ id: 27, auth_type: 'anthropic_oauth' }),
      /Max 20x &lt;safe&gt;/
    );
    global.getOAuthUsageState = () => null;
    assert.match(
      buildOAuthPlanBadge({ id: 27, auth_type: 'anthropic_oauth', anthropic_plan_type: 'Pro <stored>' }),
      /Pro &lt;stored&gt;/
    );
  } finally {
    global.getOAuthUsageState = previousGetUsageState;
  }
});

test('xAI 按 Management Center 语义渲染原值额度并转义内容', () => {
  const previousWindow = global.window;
  const previousGetUsageState = global.getOAuthUsageState;
  const previousReadOnly = global.isTokenChannelsReadOnly;
  global.window = {
    t(key, values = {}) {
      return ({
        'channels.oauth.usageRefresh': '刷新额度',
        'channels.oauth.usageWeekly': '周额度',
        'channels.oauth.usageMonthly': '月额度',
        'channels.oauth.usageAvailable': '可用',
        'channels.oauth.usageWarnings': '部分数据不可用',
        'channels.oauth.usageUsed': `已用${values.percent}`,
        'channels.oauth.usageRemaining': `${values.label}剩余${values.percent}%`,
        'channels.oauth.usageProduct': `产品使用 · ${values.product}`,
        'channels.oauth.usageOnDemand': '按量付费',
        'channels.oauth.usageOnDemandDisabled': '未启用',
        'channels.oauth.usageMonthlyCredits': '月度积分',
        'channels.oauth.usageReset': `重置 ${values.time}`
      })[key] || key;
    }
  };
  global.getOAuthUsageState = () => ({
    status: 'ready',
    data: {
      provider: 'xai',
      subscription_tier: 'Pro <safe>',
      xai_billing: {
        weekly_present: true,
        weekly_usage_percent: 25.5,
        weekly_reset_at: '2026-08-08T00:00:00Z',
        product_usage: [{ product: 'grok<fast>', usage_percent: 12.25 }],
        on_demand_cap_cents: 500.25,
        on_demand_used_cents: 125.5,
        monthly_limit_cents: 10000.75,
        included_used_cents: 4000.5,
        monthly_reset_at: '2026-09-01T00:00:00Z',
        monthly_present: true
      },
      warnings: ['Monthly unavailable <retry>']
    }
  });
  global.isTokenChannelsReadOnly = () => false;
  try {
    const badge = buildOAuthPlanBadge({
      auth_type: 'xai_oauth',
      xai_email: 'user<safe>@example.com',
      xai_subscription_tier: 'Pro <safe>',
      xai_entitlement_status: 'active<script>',
      oauth_credential: 'must-not-render',
      api_key: 'must-not-render'
    });
    assert.match(badge, /Pro &lt;safe&gt;/);
    assert.doesNotMatch(badge, /user&lt;safe&gt;@example\.com/);
    assert.doesNotMatch(badge, /active&lt;script&gt;/);
    assert.doesNotMatch(badge, /must-not-render/);

    const usage = buildOAuthUsageStatusHtml({ id: 88, auth_type: 'xai_oauth' });
    assert.match(usage, /周额度/);
    assert.match(usage, /Pro &lt;safe&gt;/);
    assert.match(usage, /已用25\.5%/);
    assert.match(usage, /aria-label="周额度剩余74\.5%"[^>]*aria-valuenow="74\.5"/);
    assert.match(usage, /产品使用 · grok&lt;fast&gt;/);
    assert.match(usage, /已用12\.25%/);
    assert.match(usage, /按量付费/);
    assert.match(usage, /已用25\.09%/);
    assert.match(usage, /US\$1\.26 \/ US\$5\.00/);
    assert.match(usage, /月度积分/);
    assert.match(usage, /40%/);
    assert.match(usage, /US\$40\.01 \/ US\$100\.01/);
    assert.match(usage, /重置/);
    assert.doesNotMatch(usage, /未知/);
    assert.match(usage, /Monthly unavailable &lt;retry&gt;/);
  } finally {
    global.window = previousWindow;
    global.getOAuthUsageState = previousGetUsageState;
    global.isTokenChannelsReadOnly = previousReadOnly;
  }
});

test('xAI 零 cap 和零月额度保留金额且不显示未知', () => {
  const previousWindow = global.window;
  const previousGetUsageState = global.getOAuthUsageState;
  const previousReadOnly = global.isTokenChannelsReadOnly;
  global.window = {
    t(key, values = {}) {
      return ({
        'channels.oauth.usageRefresh': '刷新额度',
        'channels.oauth.usageWeekly': '周额度',
        'channels.oauth.usageUsed': `已用${values.percent}`,
        'channels.oauth.usageOnDemand': '按量付费',
        'channels.oauth.usageOnDemandDisabled': '未启用',
        'channels.oauth.usageMonthlyCredits': '月度积分'
      })[key] || key;
    }
  };
  global.getOAuthUsageState = () => ({
    status: 'ready',
    data: {
      provider: 'xai',
      xai_billing: {
        weekly_present: true,
        weekly_usage_percent: null,
        on_demand_cap_cents: 0,
        on_demand_used_cents: 0,
        monthly_limit_cents: 0,
        included_used_cents: 25,
        monthly_present: true
      }
    }
  });
  global.isTokenChannelsReadOnly = () => false;
  try {
    const usage = buildOAuthUsageStatusHtml({ id: 89, auth_type: 'xai_oauth' });
    assert.match(usage, /周额度/);
    assert.match(usage, /已用--/);
    assert.match(usage, /按量付费/);
    assert.match(usage, /未启用/);
    assert.match(usage, /月度积分/);
    assert.match(usage, /--/);
    assert.match(usage, /US\$0\.25 \/ US\$0\.00/);
    assert.doesNotMatch(usage, /未知/);
  } finally {
    global.window = previousWindow;
    global.getOAuthUsageState = previousGetUsageState;
    global.isTokenChannelsReadOnly = previousReadOnly;
  }
});

test('xAI 只渲染 API 标记实际存在的周期', () => {
  const previousWindow = global.window;
  const previousGetUsageState = global.getOAuthUsageState;
  const previousReadOnly = global.isTokenChannelsReadOnly;
  global.window = {
    t(key, values = {}) {
      return ({
        'channels.oauth.usageRefresh': '刷新额度',
        'channels.oauth.usageWeekly': '周额度',
        'channels.oauth.usageUsed': `已用${values.percent}`,
        'channels.oauth.usageOnDemand': '按量付费',
        'channels.oauth.usageOnDemandDisabled': '未启用',
        'channels.oauth.usageMonthlyCredits': '月度积分'
      })[key] || key;
    }
  };
  let billing = {
    weekly_present: false,
    monthly_present: true,
    monthly_limit_cents: 0,
    included_used_cents: 0,
    on_demand_cap_cents: 0
  };
  global.getOAuthUsageState = () => ({
    status: 'ready',
    data: { provider: 'xai', xai_billing: billing }
  });
  global.isTokenChannelsReadOnly = () => false;
  try {
    const usage = buildOAuthUsageStatusHtml({ id: 90, auth_type: 'xai_oauth' });
    assert.doesNotMatch(usage, /周额度/);
    assert.match(usage, /月度积分/);
    assert.match(usage, /US\$0\.00 \/ US\$0\.00/);

    billing = { weekly_present: true, weekly_usage_percent: 0, monthly_present: false, on_demand_cap_cents: 0 };
    const weeklyUsage = buildOAuthUsageStatusHtml({ id: 91, auth_type: 'xai_oauth' });
    assert.match(weeklyUsage, /周额度/);
    assert.match(weeklyUsage, /已用0%/);
    assert.doesNotMatch(weeklyUsage, /月度积分/);
  } finally {
    global.window = previousWindow;
    global.getOAuthUsageState = previousGetUsageState;
    global.isTokenChannelsReadOnly = previousReadOnly;
  }
});
