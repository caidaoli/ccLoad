/**
 * 生成有效优先级显示HTML
 * @param {Object} channel - 渠道数据
 * @returns {string} HTML字符串
 */
function formatHealthScoreDisplay(value) {
  const num = Number(value);
  if (!Number.isFinite(num)) return '';
  const formatted = num.toFixed(1);
  return formatted.endsWith('.0') ? formatted.slice(0, -2) : formatted;
}

function buildPriorityRow(rowClass, valueClass, value) {
  return `<div class="ch-priority-row ${rowClass}"><span class="${valueClass}">${value}</span></div>`;
}

function buildEffectivePriorityHtml(channel) {
  const basePriority = channel.priority;
  const priorityLabel = window.t('channels.table.priority');
  const healthLabel = window.t('channels.stats.healthScoreLabel');

  if (channel.effective_priority === undefined || channel.effective_priority === null) {
    const title = `${priorityLabel}: ${basePriority}`;
    return `<div class="ch-priority-stack" title="${title.replace(/"/g, '&quot;')}">${buildPriorityRow('ch-priority-base', 'ch-priority-value', basePriority)}</div>`;
  }

  const effPriority = formatHealthScoreDisplay(channel.effective_priority);
  const diff = channel.effective_priority - basePriority;
  const isConsistent = Math.abs(diff) < 0.1;

  const successRateText = channel.success_rate !== undefined
    ? window.t('channels.stats.successRate', { rate: (channel.success_rate * 100).toFixed(1) + '%' })
    : '';

  const tooltipParts = [
    `${priorityLabel}: ${basePriority}`,
    `${healthLabel}: ${effPriority}`
  ];
  if (successRateText) {
    tooltipParts.push(successRateText);
  }
  const title = tooltipParts.join(' | ');

  const baseValueClass = isConsistent
    ? 'ch-priority-value ch-priority-base-value'
    : 'ch-priority-value ch-priority-base-value ch-priority-stale';
  const healthValueClass = isConsistent
    ? 'ch-priority-value ch-priority-health-good'
    : 'ch-priority-value ch-priority-health-bad';

  const rows = [buildPriorityRow('ch-priority-base', baseValueClass, basePriority)];
  if (!isConsistent) {
    rows.push(buildPriorityRow('ch-priority-health', healthValueClass, effPriority));
  }

  return `<div class="ch-priority-stack" title="${title.replace(/"/g, '&quot;')}">${rows.join('')}</div>`;
}

function inlineDisabledBadge(enabled) {
  if (enabled !== false) return '';
  return `<span style="display: inline-flex; align-items: center; color: #dc2626; font-size: 0.75rem; font-weight: 600; background: #eef2f7; padding: 1px 6px; border-radius: 4px; border: 1px solid #cbd5e1; vertical-align: middle;">${window.t('channels.statusDisabled')}</span>`;
}

function inlineCooldownBadge(c) {
  const ms = c.cooldown_remaining_ms || 0;
  if (!ms || ms <= 0) return '';
  const text = humanizeMS(ms);
  return `<span style="display: inline-flex; align-items: center; color: #dc2626; font-size: 0.75rem; font-weight: 500; background: linear-gradient(135deg, #fee2e2 0%, #fecaca 100%); padding: 1px 6px; border-radius: 4px; border: 1px solid #fca5a5; vertical-align: middle;">${window.t('channels.cooldownBadge', { time: text })}</span>`;
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
  return `<span style="background: ${config.bgColor}; color: ${config.color}; padding: 2px 8px; border-radius: 4px; font-size: 0.7rem; font-weight: 700; border: 1px solid ${config.borderColor}; letter-spacing: 0.025em; text-transform: uppercase;">${config.text}</span>`;
}

/**
 * 构建渠道健康状态指示器 HTML（参考 stats.js buildHealthIndicator）
 * @param {Array} timeline - health_timeline 数组
 * @param {number} currentRate - 当前成功率 (0-1)
 * @returns {string} HTML字符串
 */
function buildChannelHealthIndicator(timeline, currentRate) {
  if (!timeline || timeline.length === 0) return '';

  const fixedBucketCount = 48;
  const normalizedTimeline = timeline.length >= fixedBucketCount
    ? timeline.slice(-fixedBucketCount)
    : [...Array(fixedBucketCount - timeline.length).fill(null), ...timeline];
  const blocks = new Array(fixedBucketCount);

  for (let i = 0; i < fixedBucketCount; i++) {
    const point = normalizedTimeline[i];
    if (!point || point.rate < 0) {
      blocks[i] = `<span class="health-block unknown" title="${window.t('stats.healthNoData')}"></span>`;
      continue;
    }

    const rate = point.rate;

    const className = rate >= 0.95 ? 'healthy' : rate >= 0.80 ? 'warning' : 'critical';

    const d = new Date(point.ts);
    const timeStr = `${String(d.getMonth() + 1).padStart(2, '0')}/${String(d.getDate()).padStart(2, '0')} ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;

    let title = `${timeStr}\n${window.t('stats.tooltipSuccess')}: ${point.success || 0} / ${window.t('stats.tooltipFailed')}: ${point.error || 0}`;
    if (point.avg_first_byte_time > 0) title += `\n${window.t('stats.tooltipTTFT')}: ${point.avg_first_byte_time.toFixed(2)}s`;
    if (point.avg_duration > 0) title += `\n${window.t('stats.tooltipDuration')}: ${point.avg_duration.toFixed(2)}s`;

    // 简化 title 中内容：只显示关键性能指标
    blocks[i] = `<span class="health-block ${className}" title="${title.replace(/"/g, '&quot;')}"></span>`;
  }

  const ratePercent = (currentRate * 100).toFixed(1);
  const rateColor = currentRate >= 0.95 ? 'var(--success-600)' :
                    currentRate >= 0.80 ? 'var(--warning-600)' : 'var(--error-600)';

  return `<div class="health-indicator"><span class="health-track">${blocks.join('')}</span><span class="health-rate" style="color: ${rateColor}">${ratePercent}%</span></div>`;
}

function buildChannelTimingHtml(stats) {
  if (!stats) return '';

  const avgFirstByte = stats.avgFirstByteTimeSeconds || 0;
  const avgDuration = stats.avgDurationSeconds || 0;
  const successCount = Number.isFinite(Number(stats.success)) ? Number(stats.success) : 0;
  const failureCount = Number.isFinite(Number(stats.error)) ? Number(stats.error) : 0;
  const durationColorBase = avgDuration > 0 ? avgDuration : avgFirstByte;
  const durationColor = (() => {
    if (durationColorBase <= 0) return 'var(--neutral-600)';
    if (durationColorBase <= 5) return 'var(--success-600)';
    if (durationColorBase <= 30) return 'var(--warning-600)';
    return 'var(--error-600)';
  })();

  const rows = [];
  if (avgFirstByte > 0) {
    rows.push(`<div class="ch-timing-row"><span class="ch-timing-label">${window.t('channels.stats.firstByte')}</span><span class="ch-timing-value" style="color: ${durationColor};">${avgFirstByte.toFixed(2)}${window.t('common.seconds')}</span></div>`);
  }
  if (avgDuration > 0) {
    rows.push(`<div class="ch-timing-row"><span class="ch-timing-label">${window.t('stats.tooltipDuration')}</span><span class="ch-timing-value" style="color: ${durationColor};">${avgDuration.toFixed(2)}${window.t('common.seconds')}</span></div>`);
  }
  rows.push(`<div class="ch-timing-row"><span class="ch-timing-label">${window.t('channels.stats.calls')}</span><span class="ch-timing-value"><span style="color: var(--success-600);">${successCount}</span>/<span style="color: var(--error-600);">${failureCount}</span>${window.t('stats.unitTimes')}</span></div>`);

  return rows.length > 0 ? `<div class="ch-timing">${rows.join('')}</div>` : '';
}

/**
 * 使用模板引擎创建渠道表格行
 * @param {Object} channel - 渠道数据
 * @returns {HTMLElement|null} 行元素
 */
function createChannelCard(channel) {
  const isCooldown = channel.cooldown_remaining_ms > 0;
  const channelTypeRaw = (channel.channel_type || '').toLowerCase();
  const stats = channelStatsById[channel.id] || null;

  // 预计算统计数据
  const statsCache = stats ? {
    inputTokensText: formatMetricNumber(stats.totalInputTokens),
    outputTokensText: formatMetricNumber(stats.totalOutputTokens),
    cacheReadText: formatMetricNumber(stats.totalCacheReadInputTokens),
    cacheCreationTokens: stats.totalCacheCreationInputTokens || 0,
    cacheCreationText: formatMetricNumber(stats.totalCacheCreationInputTokens),
    costDisplay: formatCostValue(stats.totalCost)
  } : null;

  // 模型文本
  const modelsText = Array.isArray(channel.models)
    ? channel.models.map(m => m.model || m).join(', ')
    : '';

  const durationHtml = buildChannelTimingHtml(stats);

  // 消耗HTML：仅保留 token 相关消耗项
  let usageHtml = '';
  if (stats && statsCache) {
    const parts = [];
    parts.push(`<div class="ch-usage-row"><span class="ch-usage-label">${window.t('channels.stats.input')}</span><span class="ch-usage-value" style="color: var(--warning-500);">${statsCache.inputTokensText}</span></div>`);
    parts.push(`<div class="ch-usage-row"><span class="ch-usage-label">${window.t('channels.stats.output')}</span><span class="ch-usage-value" style="color: var(--warning-500);">${statsCache.outputTokensText}</span></div>`);
    const supportsCaching = channelTypeRaw === 'anthropic' || channelTypeRaw === 'codex';
    if (supportsCaching) {
      parts.push(`<div class="ch-usage-row"><span class="ch-usage-label">${window.t('channels.stats.cacheRead')}</span><span class="ch-usage-value" style="color: var(--success-500);">${statsCache.cacheReadText}</span></div>`);
      if (statsCache.cacheCreationTokens > 0) {
        parts.push(`<div class="ch-usage-row"><span class="ch-usage-label">${window.t('channels.stats.cacheCreate')}</span><span class="ch-usage-value" style="color: var(--primary-500);">${statsCache.cacheCreationText}</span></div>`);
      }
    }
    usageHtml = `<div class="ch-usage-list">${parts.join('')}</div>`;
  }

  // 成本HTML
  let costHtml = '';
  if (stats && statsCache) {
    costHtml = `<strong style="color: var(--success-600);">${statsCache.costDisplay}</strong>`;
  }

  // 健康指示器
  let healthHtml = '';
  if (stats && stats.healthTimeline && stats.total > 0) {
    const successRate = stats.total > 0 ? stats.success / stats.total : 0;
    healthHtml = buildChannelHealthIndicator(stats.healthTimeline, successRate);
  }

  // 行class
  const rowClasses = ['channel-table-row'];
  if (isCooldown) rowClasses.push('channel-card-cooldown');

  // 准备模板数据
  const cardData = {
    rowClasses: rowClasses.join(' '),
    id: channel.id,
    name: channel.name,
    typeBadge: buildChannelTypeBadge(channelTypeRaw),
    url: channel.url,
    modelsText: modelsText,
    priority: channel.priority,
    effectivePriorityHtml: buildEffectivePriorityHtml(channel),
    disabledBadge: inlineDisabledBadge(channel.enabled),
    cooldownBadge: inlineCooldownBadge(channel),
    durationHtml: durationHtml,
    usageHtml: usageHtml,
    costHtml: costHtml,
    healthHtml: healthHtml,
    enabled: channel.enabled,
    toggleText: channel.enabled ? window.t('common.disable') : window.t('common.enable'),
    toggleTitle: channel.enabled ? window.t('channels.toggleDisable') : window.t('channels.toggleEnable')
  };

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

  // 事件委托：处理渠道多选复选框
  container.addEventListener('change', (e) => {
    const checkbox = e.target.closest('.channel-select-checkbox');
    if (checkbox && checkbox.id === 'visibleSelectionCheckbox') {
      toggleVisibleChannelsSelection();
      return;
    }

    if (!checkbox) return;

    const channelId = normalizeSelectedChannelID(checkbox.dataset.channelId);
    if (!channelId) return;

    if (checkbox.checked) {
      selectedChannelIds.add(channelId);
    } else {
      selectedChannelIds.delete(channelId);
    }

    if (typeof updateBatchChannelSelectionUI === 'function') {
      updateBatchChannelSelectionUI();
    }
  });

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
    if (typeof updateBatchChannelSelectionUI === 'function') {
      updateBatchChannelSelectionUI();
    }
    return;
  }

  // 初始化事件委托（仅一次）
  initChannelEventDelegation();

  // 构建表格
  const thead = `<thead>
    <tr>
      <th class="ch-col-checkbox"><label id="visibleSelectionToggle" class="channel-selection-toggle channel-table-selection-toggle" data-i18n-title="channels.batchSelectVisible" title="全选"><input id="visibleSelectionCheckbox" type="checkbox" data-change-action="toggle-visible-channels-selection"><span id="visibleSelectionToggleText" data-i18n="channels.batchSelectVisible">全选</span></label></th>
      <th class="ch-col-name">${window.t('channels.table.nameAndUrl')}</th>
      <th class="ch-col-models">${window.t('channels.table.models')}</th>
      <th class="ch-col-priority">${window.t('channels.table.priority')}</th>
      <th class="ch-col-duration">${window.t('channels.table.duration')}</th>
      <th class="ch-col-usage">${window.t('channels.table.usage')}</th>
      <th class="ch-col-cost">${window.t('channels.stats.cost')}</th>
      <th class="ch-col-actions">${window.t('channels.table.actions')}</th>
    </tr>
  </thead>`;

  const tbody = document.createElement('tbody');
  channelsToRender.forEach(channel => {
    const row = createChannelCard(channel);
    if (row) tbody.appendChild(row);
  });

  el.innerHTML = `<div class="table-container"><table class="modern-table channel-table">${thead}</table></div>`;
  el.querySelector('table').appendChild(tbody);

  // 模板渲染后设置 checkbox 选中态
  el.querySelectorAll('.channel-select-checkbox').forEach(cb => {
    cb.checked = selectedChannelIds.has(normalizeSelectedChannelID(cb.dataset.channelId));
  });

  // Translate dynamically rendered elements
  if (window.i18n && window.i18n.translatePage) {
    window.i18n.translatePage();
  }

  if (typeof updateBatchChannelSelectionUI === 'function') {
    updateBatchChannelSelectionUI();
  }
}
