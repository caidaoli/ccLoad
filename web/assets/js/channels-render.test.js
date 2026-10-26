const test = require('node:test');
const assert = require('node:assert/strict');

const { buildOAuthPlanBadge } = require('./oauth-plan-badge.js');
const {
  buildOAuthUsageStatusHtml,
  toggleOAuthUsageWindows,
  buildChannelHealthHtml,
  buildChannelModelListHtml,
  buildChannelModelPopoverHtml,
  copyChannelModelName,
  buildManagementAccountStatusHtml,
  initChannelEventDelegation,
  isOpenCodeGoChannel,
  isOpenCodeChannel,
  channelShowsOAuthUsage
} = require('./channels-render.js');

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

test('Claude 重置次数中的上游字段不能注入 HTML', () => {
  const previousWindow = global.window;
  const previousResetState = global.getAnthropicResetCreditsState;
  const previousUsageState = global.getOAuthUsageState;
  const previousReadOnly = global.isTokenChannelsReadOnly;
  global.window = { t: (key, values = {}) => `${key}${values.count ?? ''}${values.time ?? ''}` };
  global.getOAuthUsageState = () => null;
  global.getAnthropicResetCreditsState = () => ({
    status: 'ready', cached: true, data: {
      eligible: false, available_count: 0,
      credits: [
        { label: '<img src=x onerror=alert(1)>', resets_left: 2, expires_at: '2027-01-01T00:00:00Z' },
        { label: 'second', resets_left: 1, expires_at: '2027-02-01T00:00:00Z' }
      ]
    }
  });
  global.isTokenChannelsReadOnly = () => false;
  try {
    const html = buildOAuthUsageStatusHtml({ id: 8101, auth_type: 'anthropic_oauth' });
    assert.match(html, /&lt;img src=x onerror=alert\(1\)&gt;/);
    assert.doesNotMatch(html, /<img/);
    const resetButton = html.match(/<button\b[^>]*data-action="reset-anthropic-quota"[^>]*>/);
    assert.ok(resetButton, 'remaining credits expose the reset action');
    assert.doesNotMatch(resetButton[0], /\sdisabled\b/, 'cached credits allow reset without refreshing');
  } finally {
    global.window = previousWindow;
    global.getAnthropicResetCreditsState = previousResetState;
    global.getOAuthUsageState = previousUsageState;
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

test('Codex 额度重置保留操作绑定、禁用状态和错误转义', () => {
  const previousWindow = global.window;
  const previousGetUsageState = global.getOAuthUsageState;
  const previousReadOnly = global.isTokenChannelsReadOnly;
  global.window = { t: key => key };
  let state = {
    status: 'ready',
    data: {
      provider: 'codex',
      windows: [{
        limit_name: 'codex', kind: 'primary', remaining_percent: 25,
        limit_window_seconds: 604800
      }],
      rate_limit_reset_credits: {
        available_count: 2
      }
    }
  };
  global.getOAuthUsageState = () => state;
  global.isTokenChannelsReadOnly = () => false;
  try {
    const html = buildOAuthUsageStatusHtml({ id: 92, auth_type: 'codex_oauth' });
    assert.match(html, /data-action="reset-codex-quota" data-channel-id="92"/);
    assert.doesNotMatch(html, /data-action="reset-codex-quota"[^>]*disabled/);

    state = { ...state, reset_status: 'loading', reset_error: '' };
    const loading = buildOAuthUsageStatusHtml({ id: 92, auth_type: 'codex_oauth' });
    assert.match(loading, /role="progressbar"/);
    assert.match(loading, /data-action="refresh-oauth-usage"[^>]*disabled/);
    assert.match(loading, /data-action="reset-codex-quota"[^>]*disabled aria-busy="true"/);

    state = { ...state, reset_status: 'error', reset_error: '重置失败 <retry>' };
    const failed = buildOAuthUsageStatusHtml({ id: 92, auth_type: 'codex_oauth' });
    assert.match(failed, /重置失败 &lt;retry&gt;/);
    assert.doesNotMatch(failed, /重置失败 <retry>/);
  } finally {
    global.window = previousWindow;
    global.getOAuthUsageState = previousGetUsageState;
    global.isTokenChannelsReadOnly = previousReadOnly;
  }
});

test('CodeBuddy 额度工具栏提供独立的手动签到状态', () => {
  const previousWindow = global.window;
  const previousGetUsageState = global.getOAuthUsageState;
  const previousReadOnly = global.isTokenChannelsReadOnly;
  const state = {
    status: 'ready',
    data: { provider: 'codebuddy', windows: [], codebuddy_credits: { remain: 720 } },
    checkin_status: 'loading'
  };
  global.window = { t: key => key };
  global.getOAuthUsageState = () => state;
  global.isTokenChannelsReadOnly = () => false;
  try {
    let html = buildOAuthUsageStatusHtml({ id: 73, auth_type: 'codebuddy_oauth' });
    assert.match(html, /data-action="checkin-codebuddy" data-channel-id="73" disabled aria-busy="true"/);
    assert.match(html, /data-action="refresh-oauth-usage"[^>]*disabled/);

    html = buildOAuthUsageStatusHtml({ id: 73, auth_type: 'codebuddy_oauth', codebuddy_international: true });
    assert.doesNotMatch(html, /data-action="checkin-codebuddy"/);
  } finally {
    global.window = previousWindow;
    global.getOAuthUsageState = previousGetUsageState;
    global.isTokenChannelsReadOnly = previousReadOnly;
  }
});

test('OpenCode 渠道按官方 URL 和 API Key 认证识别，与名称无关', () => {
  for (const url of [
    'https://opencode.ai/zen', 'https://opencode.ai/zen/go',
    'https://opencode.ai/zen/v1/responses#', 'https://opencode.ai/zen/go/v1/chat/completions',
    'HTTPS://OpenCode.AI/zen', 'https://opencode.ai/%7Aen/go'
  ]) {
    assert.equal(isOpenCodeChannel({ name: 'arbitrary name', auth_type: 'api_key', urls: [{ url }] }), true);
    assert.equal(isOpenCodeChannel({ auth_type: 'codex_oauth', urls: [{ url }] }), false);
  }
  for (const url of [
    'https://example.com/zen', 'https://opencode.ai.example/zen', 'https://opencode.ai/zenith',
    'https://opencode.ai/inference', 'http://opencode.ai/zen', 'https://user@opencode.ai/zen',
    'https://opencode.ai:443/zen', 'https://@opencode.ai/zen', 'https://opencode.ai/zen/go/../x',
    'https://opencode.ai/other/../zen', 'https://opencode.ai/zen/%2E%2E/x', 'https://opencode.ai/zen?x=1'
  ]) {
    assert.equal(isOpenCodeChannel({ name: 'OpenCode-Zen', auth_type: 'api_key', urls: [url] }), false);
  }
});

test('OpenCode Go API Key 渠道显示额度工具栏', () => {
  const previousWindow = global.window;
  const previousGetUsageState = global.getOAuthUsageState;
  const previousReadOnly = global.isTokenChannelsReadOnly;
  global.window = { t: key => key === 'channels.oauth.usageRefresh' ? '刷新额度' : key };
  global.getOAuthUsageState = () => null;
  global.isTokenChannelsReadOnly = () => false;
  try {
    const channel = {
      id: 4,
      auth_type: 'api_key',
      urls: [{ url: 'https://opencode.ai/zen/go' }]
    };
    assert.equal(isOpenCodeGoChannel(channel), true);
    assert.equal(channelShowsOAuthUsage(channel), true);
    assert.equal(isOpenCodeGoChannel({ auth_type: 'api_key', urls: [{ url: 'https://example.com' }] }), false);
    const html = buildOAuthUsageStatusHtml(channel);
    assert.match(html, /data-action="refresh-oauth-usage"/);
    assert.match(html, /data-channel-id="4"/);
  } finally {
    global.window = previousWindow;
    global.getOAuthUsageState = previousGetUsageState;
    global.isTokenChannelsReadOnly = previousReadOnly;
  }
});

test('xAI 额度保留可访问进度并转义上游内容', () => {
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
      quota_cost_usage: {
        windows: [
          { key: 'xai|weekly', window_seconds: 604800, standard_cost_microusd: 3450000 },
          { key: 'xai|monthly', window_seconds: 2592000, standard_cost_microusd: 7800000 }
        ]
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
    assert.match(usage, /Pro &lt;safe&gt;/);
    assert.match(usage, /aria-label="周额度剩余74\.5%"[^>]*aria-valuenow="74\.5"/);
    assert.match(usage, /产品使用 · grok&lt;fast&gt;/);
    assert.match(usage, /Monthly unavailable &lt;retry&gt;/);
  } finally {
    global.window = previousWindow;
    global.getOAuthUsageState = previousGetUsageState;
    global.isTokenChannelsReadOnly = previousReadOnly;
  }
});

function installManagementRenderGlobals({ balanceState = null, checkinState = null, readOnly = false } = {}) {
  const previous = {
    window: global.window,
    getManagementBalanceState: global.getManagementBalanceState,
    getManagementCheckinState: global.getManagementCheckinState,
    isTokenChannelsReadOnly: global.isTokenChannelsReadOnly,
    managementSupportsCheckin: global.managementSupportsCheckin
  };
  global.window = {
    t: (key, values) => (values
      ? Object.entries(values).reduce((text, [name, value]) => text.replace(`{${name}}`, String(value)), key)
      : key)
  };
  global.getManagementBalanceState = () => balanceState;
  global.getManagementCheckinState = () => checkinState;
  global.isTokenChannelsReadOnly = () => readOnly;
  global.managementSupportsCheckin = profile => profile === 'new_api' || profile === 'sub2api_pro';
  return () => {
    global.window = previous.window;
    global.getManagementBalanceState = previous.getManagementBalanceState;
    global.getManagementCheckinState = previous.getManagementCheckinState;
    global.isTokenChannelsReadOnly = previous.isTokenChannelsReadOnly;
    global.managementSupportsCheckin = previous.managementSupportsCheckin;
  };

}

test('管理账户只对已配置凭据的 API Key 渠道渲染动作，签到按 profile 收敛', () => {
  const restore = installManagementRenderGlobals();
  try {
    assert.equal(buildManagementAccountStatusHtml({ id: 3, auth_type: 'codex_oauth' }), '');
    assert.equal(buildManagementAccountStatusHtml({ id: 3, auth_type: 'api_key' }), '');
    assert.equal(buildManagementAccountStatusHtml({
      id: 3,
      auth_type: 'api_key',
      management_account: { profile: 'new_api', credential_configured: false }
    }), '');
    for (const url of ['https://opencode.ai/zen', 'https://opencode.ai/zen/go']) {
      assert.equal(buildManagementAccountStatusHtml({
        id: 3, auth_type: 'api_key', urls: [{ url }],
        management_account: { profile: 'new_api', credential_configured: true }
      }), '', 'OpenCode 不展示管理账户动作，即使已有配置');
    }

    const newAPI = buildManagementAccountStatusHtml({
      id: 3,
      auth_type: 'api_key',
      management_account: { profile: 'new_api', credential_configured: true }
    });
    assert.match(newAPI, /data-action="refresh-management-balance" data-channel-id="3"/);
    assert.match(newAPI, /data-action="run-management-checkin" data-channel-id="3"/);

    const pro = buildManagementAccountStatusHtml({
      id: 4,
      auth_type: 'api_key',
      management_account: { profile: 'sub2api_pro', credential_configured: true }
    });
    assert.match(pro, /data-action="run-management-checkin"/);

    const standard = buildManagementAccountStatusHtml({
      id: 5,
      auth_type: 'api_key',
      management_account: { profile: 'sub2api', credential_configured: true }
    });
    assert.match(standard, /data-action="refresh-management-balance"/);
    assert.doesNotMatch(standard, /data-action="run-management-checkin"/);
  } finally {
    restore();
  }
});

test('只有 used/total/percent 齐备才渲染进度条，缺失用量只显示剩余额度', () => {
  const withUsage = installManagementRenderGlobals({
    balanceState: {
      status: 'ready',
      data: {
        profile: 'new_api',
        balance: {
          remaining: 12.5,
          unit: 'USD',
          used: 7.5,
          total: 20,
          available_percent: 62.5,
          sampled_at: '2026-08-25T10:00:00Z'
        }
      }
    }
  });
  try {
    const html = buildManagementAccountStatusHtml({
      id: 6,
      auth_type: 'api_key',
      management_account: { profile: 'new_api', credential_configured: true }
    });
    assert.match(html, /role="progressbar"/);
    assert.match(html, /aria-valuenow="62\.5"/);
    assert.match(html, /\$12\.50/);
  } finally {
    withUsage();
  }

  const withoutUsage = installManagementRenderGlobals({
    balanceState: {
      status: 'ready',
      data: {
        profile: 'sub2api',
        balance: { remaining: 3.25, unit: 'USD', sampled_at: '2026-08-25T10:00:00Z' }
      }
    }
  });
  try {
    const html = buildManagementAccountStatusHtml({
      id: 7,
      auth_type: 'api_key',
      management_account: { profile: 'sub2api', credential_configured: true }
    });
    assert.match(html, /\$3\.25/);
    assert.doesNotMatch(html, /role="progressbar"/);
    assert.doesNotMatch(html, /aria-valuenow/);
  } finally {
    withoutUsage();
  }

  const persistedBalance = installManagementRenderGlobals();
  try {
    const html = buildManagementAccountStatusHtml({
      id: 71,
      auth_type: 'api_key',
      management_account: {
        profile: 'sub2api',
        credential_configured: true,
        balance: { remaining: 8.75, unit: 'USD', sampled_at: '2026-08-25T10:00:00Z' }
      }
    });
    assert.match(html, /\$8\.75/, '无 live 余额状态时必须回落到 DTO 的持久化余额');
  } finally {
    persistedBalance();
  }
});

test('额度与签到的 loading 与错误互不干扰且带可读文本', () => {
  const loading = installManagementRenderGlobals({
    balanceState: { status: 'loading' },
    checkinState: null
  });
  try {
    const html = buildManagementAccountStatusHtml({
      id: 8,
      auth_type: 'api_key',
      management_account: { profile: 'new_api', credential_configured: true }
    });
    assert.match(html, /role="status"[^>]*>[\s\S]*channels\.management\.refreshingBalance/);
    assert.doesNotMatch(html, /data-action="refresh-management-balance"/, '刷新中不再提供刷新入口');
    assert.doesNotMatch(html, /data-action="run-management-checkin"[^>]*disabled/);
  } finally {
    loading();
  }

  const failed = installManagementRenderGlobals({
    balanceState: { status: 'error', error: 'upstream <502>' },
    checkinState: { status: 'loading' }
  });
  try {
    const html = buildManagementAccountStatusHtml({
      id: 9,
      auth_type: 'api_key',
      management_account: { profile: 'new_api', credential_configured: true }
    });
    const retry = html.match(/<button\b[^>]*data-action="refresh-management-balance"[^>]*>([^<]*)<\/button>/);
    assert.ok(retry, '错误文字本身就是重试刷新入口');
    assert.equal(retry[1], 'upstream &lt;502&gt;');
    assert.doesNotMatch(retry[0], /\sdisabled\b/);
    assert.doesNotMatch(html, /upstream <502>/);
    assert.match(html, /data-action="run-management-checkin"[^>]*disabled[^>]*aria-busy="true"/);
  } finally {
    failed();
  }
});

test('签到状态以文字呈现并回落到持久化结果', () => {
  const live = installManagementRenderGlobals({
    checkinState: { status: 'ready', data: { status: 'manual_required', status_code: 200 } }
  });
  try {
    const html = buildManagementAccountStatusHtml({
      id: 10,
      auth_type: 'api_key',
      management_account: {
        profile: 'new_api',
        credential_configured: true,
        last_checkin_status: 'success'
      }
    });
    assert.match(html, /channels\.management\.status\.manual_required/);
    assert.doesNotMatch(html, /channels\.management\.status\.success/);
  } finally {
    live();
  }

  const persisted = installManagementRenderGlobals();
  try {
    const html = buildManagementAccountStatusHtml({
      id: 11,
      auth_type: 'api_key',
      management_account: {
        profile: 'new_api',
        credential_configured: true,
        last_checkin_status: 'credential_invalid',
        last_checkin_at: '2026-08-25T10:00:00Z'
      }
    });
    assert.match(html, /channels\.management\.status\.credential_invalid/);

    const forbidden = buildManagementAccountStatusHtml({
      id: 13,
      auth_type: 'api_key',
      management_account: {
        profile: 'sub2api_pro',
        credential_configured: true,
        last_checkin_status: 'credential_forbidden'
      }
    });
    assert.match(forbidden, /channels\.management\.status\.credential_forbidden/);

    const disabled = buildManagementAccountStatusHtml({
      id: 15,
      auth_type: 'api_key',
      management_account: {
        profile: 'new_api',
        credential_configured: true,
        last_checkin_status: 'skipped_disabled'
      }
    });
    assert.doesNotMatch(disabled, /channels\.management\.status\.skipped_disabled/);
  } finally {
    persisted();
  }
});

test('只读模式不渲染管理账户动作', () => {
  const restore = installManagementRenderGlobals({ readOnly: true });
  try {
    assert.equal(buildManagementAccountStatusHtml({
      id: 12,
      auth_type: 'api_key',
      management_account: { profile: 'new_api', credential_configured: true }
    }), '');
  } finally {
    restore();
  }
});

test('OAuth 额度超过 2 个窗口时折叠其余窗口，展开状态跨重渲染保留', () => {
  const saved = { window: global.window, state: global.getOAuthUsageState, readOnly: global.isTokenChannelsReadOnly };
  global.window = { t: (key, values = {}) => `${key}${values.count ?? ''}` };
  global.isTokenChannelsReadOnly = () => false;
  const windows = [1, 2, 3, 4].map(i => ({ limit_name: `w${i}`, remaining_percent: 50 }));
  global.getOAuthUsageState = () => ({ status: 'ready', data: { windows } });
  const channel = { id: 77, auth_type: 'codex_oauth' };
  const count = (html, re) => (html.match(re) || []).length;
  try {
    let html = buildOAuthUsageStatusHtml(channel);
    assert.equal(count(html, /role="progressbar"/g), 4);
    const visible = html.slice(0, html.indexOf('ch-oauth-usage__more'));
    assert.equal(count(visible, /role="progressbar"/g), 2);
    assert.match(html, /class="ch-oauth-usage__more" hidden>/);
    assert.match(html, /data-action="toggle-oauth-usage-windows" data-channel-id="77" data-hidden-count="2" aria-expanded="false">channels\.oauthUsageMoreWindows2</);

    const more = { hidden: true };
    const btn = {
      dataset: { channelId: '77', hiddenCount: '2' },
      parentElement: { querySelector: () => more },
      setAttribute(name, value) { this[name] = value; }
    };
    toggleOAuthUsageWindows(btn);
    assert.equal(more.hidden, false);
    assert.equal(btn['aria-expanded'], 'true');

    html = buildOAuthUsageStatusHtml(channel);
    assert.match(html, /class="ch-oauth-usage__more">/);
    assert.match(html, /aria-expanded="true">channels\.oauthUsageLessWindows</);

    toggleOAuthUsageWindows(btn);
    assert.match(buildOAuthUsageStatusHtml(channel), /class="ch-oauth-usage__more" hidden>/);

    global.getOAuthUsageState = () => ({ status: 'ready', data: { windows: windows.slice(0, 2) } });
    assert.doesNotMatch(buildOAuthUsageStatusHtml(channel), /toggle-oauth-usage-windows/);
  } finally {
    global.window = saved.window;
    global.getOAuthUsageState = saved.state;
    global.isTokenChannelsReadOnly = saved.readOnly;
  }
});

test('OAuth 窗口累计成本不足 $0.1 时金额与预估都不显示', () => {
  const saved = { window: global.window, state: global.getOAuthUsageState, readOnly: global.isTokenChannelsReadOnly };
  global.window = { t: (key, values = {}) => [key, ...Object.values(values)].join('|') };
  global.isTokenChannelsReadOnly = () => false;
  const channel = { id: 78, auth_type: 'codex_oauth' };
  const render = window => {
    global.getOAuthUsageState = () => ({ status: 'ready', data: { windows: [{ limit_name: 'codex', ...window }] } });
    return buildOAuthUsageStatusHtml(channel);
  };
  const amountOf = window => render(window).match(/<span class="ch-oauth-usage__amount">([^<]*)<\/span>/)?.[1] ?? null;
  try {
    for (const cost of [400, 30_000, 99_999]) {
      const html = render({ remaining_percent: 50, standard_cost_microusd: cost });
      assert.doesNotMatch(html, /ch-oauth-usage__amount/);
      assert.doesNotMatch(html, /channels\.oauth\.usageDetail(Used|Amount)/, '悬浮明细同样不显示金额');
    }
    assert.equal(amountOf({ remaining_percent: 100, standard_cost_microusd: 100_000 }), 'channels.oauth.usageCompactUsed|$0.1');
    assert.equal(amountOf({ remaining_percent: 50, standard_cost_microusd: 100_000 }), 'channels.oauth.usageCompactAmount|$0.1|$0.2');
    assert.equal(amountOf({ remaining_percent: 41, standard_cost_microusd: 5_697_691 }), 'channels.oauth.usageCompactAmount|$5.7|$9.7');
  } finally {
    global.window = saved.window;
    global.getOAuthUsageState = saved.state;
    global.isTokenChannelsReadOnly = saved.readOnly;
  }
});

test('URL 行模型标签只列去重后的请求模型名并转义，弹层列出传入的模型', () => {
  const chipsOf = html => [...html.matchAll(/<button type="button" class="ch-model-chip" data-model="([^"]*)" title="[^"]*">([^<]*)</g)]
    .map(m => [m[1], m[2]]);
  assert.equal(buildChannelModelListHtml(3, []), '');
  const models = [
    { model: 'gpt-5', redirect_model: '' },
    { model: 'alias', redirect_model: 'a' },
    { model: 'alias', redirect_model: 'b' },
    { model: '<x>', redirect_model: '' }
  ];
  const html = buildChannelModelListHtml(3, models);
  assert.deepEqual(chipsOf(html), [['gpt-5', 'gpt-5'], ['alias', 'alias'], ['&lt;x&gt;', '&lt;x&gt;']]);
  assert.match(html, /<button\b[^>]*aria-expanded="false"[^>]*>\+3<\/button>/);

  assert.deepEqual(chipsOf(buildChannelModelPopoverHtml(['m12', '<y>'])), [['m12', 'm12'], ['&lt;y&gt;', '&lt;y&gt;']]);
});

test('点击模型标签复制请求模型名，失败时提示手动复制', async () => {
  const saved = global.window;
  const copied = [];
  const toasts = [];
  let fail = false;
  global.window = {
    t: (key, values = {}) => `${key}:${values.model ?? ''}`,
    copyToClipboard: text => (fail ? Promise.reject(new Error('denied')) : (copied.push(text), Promise.resolve())),
    showSuccess: msg => toasts.push(['success', msg]),
    showError: msg => toasts.push(['error', msg])
  };
  try {
    await copyChannelModelName({ dataset: { model: 'alias' } });
    fail = true;
    await copyChannelModelName({ dataset: { model: 'gpt-5' } });
    assert.deepEqual(copied, ['alias']);
    assert.deepEqual(toasts, [['success', 'channels.modelCopied:alias'], ['error', 'channels.keyCopyFailed:']]);
  } finally {
    global.window = saved;
  }
});

test('健康度列：最近请求成功显示时间、失败显示错误、499 不显示，无冷却无调用时为空', () => {
  const saved = { window: global.window, cost: global.buildCostStackHtml, metric: global.formatMetricNumber };
  global.window = { t: key => key, getFirstByteTimingLevel: () => 'good', getDurationTimingLevel: () => 'good', getSpeedLevel: () => 'good', getSuccessRateLevel: () => 'good' };
  global.buildCostStackHtml = () => '';
  global.formatMetricNumber = value => String(value || 0);
  try {
    assert.equal(buildChannelHealthHtml({ id: 9 }, { success: 0, error: 0 }), '');

    const stats = { success: 9, error: 1, avgDurationSeconds: 2, totalInputTokens: 10, totalOutputTokens: 5 };
    const lastRequest = status => buildChannelHealthHtml({ id: 9 }, { ...stats, lastRequestAt: Date.now() - 120000, lastRequestStatus: status, lastRequestMessage: '<boom>' });
    assert.match(lastRequest(200), /ch-last-request--ok">channels\.lastSuccess\.succeededAt</);
    assert.doesNotMatch(lastRequest(200), /ch-last-request__detail/);
    const failed = lastRequest(502);
    assert.match(failed, /channels\.lastSuccess\.failedStatus/);
    assert.match(failed, /&lt;boom&gt;/);
    assert.doesNotMatch(lastRequest(499), /ch-last-request/);
  } finally {
    global.window = saved.window;
    global.buildCostStackHtml = saved.cost;
    global.formatMetricNumber = saved.metric;
  }
});

test('点击额度文字先确认，确认后才刷新对应渠道', async () => {
  const names = ['window', 'document', 'isTokenChannelsReadOnly', 'refreshOAuthUsage', 'refreshManagementBalance'];
  const previous = new Map(names.map(name => [name, Object.getOwnPropertyDescriptor(global, name)]));
  const handlers = {};
  const prompts = [];
  const refreshed = [];
  const errors = [];
  let confirmed = false;
  const globals = {
    window: {
      t: key => key,
      showConfirm: async options => { prompts.push(options.message); return confirmed; },
      showError: message => errors.push(message)
    },
    document: {
      getElementById: id => id === 'channels-container'
        ? { dataset: {}, addEventListener: (type, handler) => { handlers[type] = handler; } }
        : null,
      addEventListener() {}
    },
    isTokenChannelsReadOnly: () => false,
    refreshOAuthUsage: async id => { refreshed.push(['oauth', id]); },
    refreshManagementBalance: async id => { refreshed.push(['management', id]); throw new Error('upstream 502'); }
  };
  for (const [name, value] of Object.entries(globals)) {
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  }
  const click = async (action, channelId) => {
    const btn = { dataset: { action, channelId: String(channelId) } };
    handlers.click({ target: { closest: selector => (selector === '.channel-action-btn' ? btn : null) } });
    await new Promise(resolve => setImmediate(resolve));
  };
  try {
    initChannelEventDelegation();

    await click('refresh-oauth-usage', 7);
    assert.deepEqual(prompts, ['channels.quotaRefreshConfirm']);
    assert.deepEqual(refreshed, [], '取消确认不刷新');

    confirmed = true;
    await click('refresh-oauth-usage', 7);
    await click('refresh-management-balance', 8);
    assert.equal(prompts.length, 3);
    assert.deepEqual(refreshed, [['oauth', 7], ['management', 8]]);
    assert.deepEqual(errors, ['upstream 502']);
  } finally {
    for (const [name, descriptor] of previous) {
      if (descriptor) Object.defineProperty(global, name, descriptor);
      else delete global[name];
    }
  }
});
