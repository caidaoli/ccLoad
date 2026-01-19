/**
 * 生成有效优先级显示HTML
 * @param {Object} channel - 渠道数据
 * @returns {string} HTML字符串
 */
function buildEffectivePriorityHtml(channel) {
  if (channel.effective_priority === undefined || channel.effective_priority === null) {
    return '';
  }

  const effPriority = channel.effective_priority.toFixed(1);
  const basePriority = channel.priority;
  const diff = channel.effective_priority - basePriority;

  // 成功率文本
  const successRateText = channel.success_rate !== undefined
    ? window.t('channels.stats.successRate', { rate: (channel.success_rate * 100).toFixed(1) + '%' })
    : '';

  // 如果有效优先级与基础优先级相同，显示绿色勾号
  if (Math.abs(diff) < 0.1) {
    const title = successRateText ? `${window.t('channels.stats.healthy')} | ${successRateText}` : window.t('channels.stats.healthy');
    return ` <span style="color: #16a34a; font-size: 0.8rem;" title="${title}">(✓${effPriority})</span>`;
  }

  // 有效优先级降低时显示红色
  const color = '#dc2626';
  const arrow = '↓';
  const title = successRateText ? `${window.t('channels.stats.effectivePriority', { priority: effPriority })} | ${successRateText}` : window.t('channels.stats.effectivePriority', { priority: effPriority });

  return ` <span style="color: ${color}; font-size: 0.8rem;" title="${title}">(${arrow}${effPriority})</span>`;
}

function inlineCooldownBadge(c) {
  const ms = c.cooldown_remaining_ms || 0;
  if (!ms || ms <= 0) return '';
  const text = humanizeMS(ms);
  return ` <span style="color: #dc2626; font-size: 0.875rem; font-weight: 500; background: linear-gradient(135deg, #fee2e2 0%, #fecaca 100%); padding: 2px 8px; border-radius: 4px; border: 1px solid #fca5a5;">${window.t('channels.cooldownBadge', { time: text })}</span>`;
}

function renderChannelStatsInline(stats, cache, channelType) {
  if (!stats) {
    return `<span class="channel-stat-badge" style="margin-left: 6px; color: var(--neutral-500);">${window.t('channels.stats.noStats')}</span>`;
  }

  const successRateText = cache?.successRateText || formatSuccessRate(stats.success, stats.total);
  const avgFirstByteText = cache?.avgFirstByteText || formatAvgFirstByte(stats.avgFirstByteTimeSeconds);
  const inputTokensText = cache?.inputTokensText || formatMetricNumber(stats.totalInputTokens);
  const outputTokensText = cache?.outputTokensText || formatMetricNumber(stats.totalOutputTokens);
  const cacheReadText = cache?.cacheReadText || formatMetricNumber(stats.totalCacheReadInputTokens);
  const cacheCreationText = cache?.cacheCreationText || formatMetricNumber(stats.totalCacheCreationInputTokens);
  const costDisplay = cache?.costDisplay || formatCostValue(stats.totalCost);

  const successRateColor = (() => {
    const rateNum = Number(successRateText.replace('%', ''));
    if (!Number.isFinite(rateNum)) return 'var(--neutral-600)';
    if (rateNum >= 95) return 'var(--success-600)';
    if (rateNum < 80) return 'var(--error-500)';
    return 'var(--warning-600)';
  })();

  const callText = `${formatMetricNumber(stats.success)}/${formatMetricNumber(stats.error)}`;
  const rangeLabel = getStatsRangeLabel(channelStatsRange);

  const parts = [
    `<span class="channel-stat-badge" style="color: var(--neutral-800);"><strong>${rangeLabel}${window.t('channels.stats.calls')}</strong> ${callText}</span>`,
    `<span class="channel-stat-badge" style="color: ${successRateColor};"><strong>${window.t('channels.stats.rate')}</strong> ${successRateText}</span>`,
    `<span class="channel-stat-badge" style="color: var(--primary-700);"><strong>${window.t('channels.stats.firstByte')}</strong> ${avgFirstByteText}</span>`,
    `<span class="channel-stat-badge" style="color: var(--neutral-800);"><strong>In</strong> ${inputTokensText}</span>`,
    `<span class="channel-stat-badge" style="color: var(--neutral-800);"><strong>Out</strong> ${outputTokensText}</span>`
  ];

  const supportsCaching = channelType === 'anthropic' || channelType === 'codex';
  if (supportsCaching) {
    parts.push(
      `<span class="channel-stat-badge" style="color: var(--success-600); background: var(--success-50); border-color: var(--success-100);"><strong>${window.t('channels.stats.cacheRead')}</strong> ${cacheReadText}</span>`,
      `<span class="channel-stat-badge" style="color: var(--primary-700); background: var(--primary-50); border-color: var(--primary-100);"><strong>${window.t('channels.stats.cacheCreate')}</strong> ${cacheCreationText}</span>`
    );
  }

  parts.push(
    `<span class="channel-stat-badge" style="color: var(--warning-700); background: var(--warning-50); border-color: var(--warning-100);"><strong>${window.t('channels.stats.cost')}</strong> ${costDisplay}</span>`
  );

  return parts.join(' ');
}

/**
 * 获取渠道类型配置信息
 * @param {string} channelType - 渠道类型
 * @returns {Object} 类型配置
 */
function getChannelTypeConfig(channelType) {
  const configs = {
    'anthropic': {
      text: 'Claude',
      color: '#8b5cf6',
      bgColor: '#f3e8ff',
      borderColor: '#c4b5fd'
    },
    'codex': {
      text: 'Codex',
      color: '#059669',
      bgColor: '#d1fae5',
      borderColor: '#6ee7b7'
    },
    'openai': {
      text: 'OpenAI',
      color: '#10b981',
      bgColor: '#d1fae5',
      borderColor: '#6ee7b7'
    },
    'gemini': {
      text: 'Gemini',
      color: '#2563eb',
      bgColor: '#dbeafe',
      borderColor: '#93c5fd'
    }
  };
  const type = (channelType || '').toLowerCase();
  return configs[type] || configs['anthropic'];
}

/**
 * 生成渠道类型徽章HTML
 * @param {string} channelType - 渠道类型
 * @returns {string} 徽章HTML
 */
function buildChannelTypeBadge(channelType) {
  const config = getChannelTypeConfig(channelType);
  return `<span style="background: ${config.bgColor}; color: ${config.color}; padding: 3px 10px; border-radius: 6px; font-size: 0.75rem; font-weight: 700; margin-left: 8px; border: 1.5px solid ${config.borderColor}; letter-spacing: 0.025em; text-transform: uppercase;">${config.text}</span>`;
}

/**
 * 使用模板引擎创建渠道卡片元素
 * @param {Object} channel - 渠道数据
 * @returns {HTMLElement|null} 卡片元素
 */
function createChannelCard(channel) {
  const isCooldown = channel.cooldown_remaining_ms > 0;
  const cardClasses = ['glass-card'];
  if (isCooldown) cardClasses.push('channel-card-cooldown');
  if (!channel.enabled) cardClasses.push('channel-disabled');

  const channelTypeRaw = (channel.channel_type || '').toLowerCase();
  const stats = channelStatsById[channel.id] || null;

  // 预计算统计数据
  const statsCache = stats ? {
    successRateText: formatSuccessRate(stats.success, stats.total),
    avgFirstByteText: formatAvgFirstByte(stats.avgFirstByteTimeSeconds),
    inputTokensText: formatMetricNumber(stats.totalInputTokens),
    outputTokensText: formatMetricNumber(stats.totalOutputTokens),
    cacheReadText: formatMetricNumber(stats.totalCacheReadInputTokens),
    cacheCreationText: formatMetricNumber(stats.totalCacheCreationInputTokens),
    costDisplay: formatCostValue(stats.totalCost)
  } : null;

  const statsHtml = stats && statsCache
    ? `<span class="channel-stats-inline">${renderChannelStatsInline(stats, statsCache, channelTypeRaw)}</span>`
    : '';

  // 新格式：models 是 {model, redirect_model} 对象数组
  const modelsText = Array.isArray(channel.models)
    ? channel.models.map(m => m.model || m).join(', ')
    : '';

  // 准备模板数据
  const cardData = {
    cardClasses: cardClasses.join(' '),
    id: channel.id,
    name: channel.name,
    typeBadge: buildChannelTypeBadge(channelTypeRaw),
    modelsText: modelsText,
    url: channel.url,
    priority: channel.priority,
    effectivePriorityHtml: buildEffectivePriorityHtml(channel),
    statusText: channel.enabled ? window.t('channels.statusEnabled') : window.t('channels.statusDisabled'),
    cooldownBadge: inlineCooldownBadge(channel),
    statsHtml: statsHtml,
    enabled: channel.enabled,
    toggleText: channel.enabled ? window.t('common.disable') : window.t('common.enable'),
    toggleTitle: channel.enabled ? window.t('channels.toggleDisable') : window.t('channels.toggleEnable')
  };

  // 使用模板引擎渲染
  const card = TemplateEngine.render('tpl-channel-card', cardData);
  return card;
}

/**
 * 初始化渠道卡片事件委托 (替代inline onclick)
 */
function initChannelEventDelegation() {
  const container = document.getElementById('channels-container');
  if (!container || container.dataset.delegated) return;

  container.dataset.delegated = 'true';

  // 事件委托：处理所有渠道操作按钮
  container.addEventListener('click', (e) => {
    const btn = e.target.closest('.channel-action-btn');
    if (!btn) return;

    const action = btn.dataset.action;
    const channelId = parseInt(btn.dataset.channelId);
    const channelName = btn.dataset.channelName;
    const enabled = btn.dataset.enabled === 'true';

    switch (action) {
      case 'edit':
        editChannel(channelId);
        break;
      case 'test':
        testChannel(channelId, channelName);
        break;
      case 'toggle':
        toggleChannel(channelId, !enabled);
        break;
      case 'copy':
        copyChannel(channelId, channelName);
        break;
      case 'delete':
        deleteChannel(channelId, channelName);
        break;
    }
  });
}

function renderChannels(channelsToRender = channels) {
  const el = document.getElementById('channels-container');
  if (!channelsToRender || channelsToRender.length === 0) {
    el.innerHTML = `<div class="glass-card">${window.t('channels.noChannels')}</div>`;
    return;
  }

  // 初始化事件委托（仅一次）
  initChannelEventDelegation();

  // 使用DocumentFragment优化批量DOM操作
  const fragment = document.createDocumentFragment();
  channelsToRender.forEach(channel => {
    const card = createChannelCard(channel);
    if (card) fragment.appendChild(card);
  });

  el.innerHTML = '';
  el.appendChild(fragment);

  // Translate dynamically rendered elements
  if (window.i18n && window.i18n.translatePage) {
    window.i18n.translatePage();
  }
}
