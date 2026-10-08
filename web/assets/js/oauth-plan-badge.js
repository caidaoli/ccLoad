// OAuth 订阅计划徽标：渠道管理与模型测试共用，保证同一计划显示同一标签与配色。
function escapeOAuthPlanText(value) {
  return String(value ?? '').replace(/[&<>"']/g, c => ({
    '&': '&amp;',
    '<': '&lt;',
    '>': '&gt;',
    '"': '&quot;',
    "'": '&#39;'
  })[c]);
}

// Codex plan_type → 用户可读标签；未登记的值原样返回。
function codexPlanLabel(rawPlanType) {
  const key = String(rawPlanType || '').trim().toLowerCase().replace(/[\s_-]+/g, '');
  switch (key) {
    case 'free': return 'Free';
    case 'go': return 'Go';
    case 'plus': return 'Plus';
    case 'prolite': return 'Pro 100';
    case 'chatgptpro':
    case 'pro': return 'Pro 200';
    case 'promax': return 'Pro 500';
    case 'team':
    case 'selfservebusinessusagebased': return 'Business';
    case 'selfservebusinessprolite': return 'Business Premium';
    case 'business':
    case 'enterprise':
    case 'ent26':
    case 'enterprisecbpusagebased': return 'Enterprise';
    case 'enterprisecbpautomation': return 'Enterprise (Automation)';
    case 'edu': return 'Edu';
    case 'eduplus': return 'Edu Plus';
    case 'edupro': return 'Edu Pro';
    case 'unknown': return 'Unknown';
    default: return rawPlanType;
  }
}

// 订阅计划标签配色：渠道列表与编辑框共用，保证同一计划显示同一颜色。
function oauthPlanBadgeTone(authType, planType) {
  const plan = String(planType || '').toLowerCase();
  if (authType === 'codex_oauth' && plan.replace(/[^a-z0-9]+/g, '_') === 'self_serve_business_prolite') return 'pro';
  const planTokens = plan.split(/[^a-z0-9]+/).filter(Boolean);
  return ['plus', 'pro', 'team'].find(tier => planTokens.includes(tier)) || '';
}

function buildOAuthPlanBadge(channel) {
  let planType = '';
  if (channel?.auth_type === 'codex_oauth') {
    planType = String(channel.codex_plan_type || '').trim();
  } else if (channel?.auth_type === 'antigravity_oauth') {
    planType = String(channel.antigravity_paid_tier || '').trim();
  } else if (channel?.auth_type === 'xai_oauth') {
    planType = String(channel.xai_subscription_tier || '').trim();
  } else if (channel?.auth_type === 'anthropic_oauth') {
    planType = String(channel.anthropic_plan_type || '').trim();
    const usageState = typeof getOAuthUsageState === 'function'
      ? getOAuthUsageState(channel.id)
      : null;
    if (usageState?.status === 'ready' && String(usageState.data?.plan_type || '').trim()) {
      planType = String(usageState.data.plan_type).trim();
    }
  }
  if (!planType) return '';

  const planTokens = planType.toLowerCase().split(/[^a-z0-9]+/).filter(Boolean);
  if (channel?.auth_type !== 'xai_oauth' && planTokens.includes('free')) return '';

  const planTone = oauthPlanBadgeTone(channel?.auth_type, planType);
  const toneClass = planTone ? ` ch-oauth-plan-badge--${planTone}` : '';
  const displayLabel = channel?.auth_type === 'codex_oauth' ? codexPlanLabel(planType) : planType;
  return `<span class="ch-oauth-plan-badge${toneClass}">${escapeOAuthPlanText(displayLabel)}</span>`;
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    buildOAuthPlanBadge,
    oauthPlanBadgeTone,
    codexPlanLabel
  };
}
