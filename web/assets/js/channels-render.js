function inlineCooldownBadge(c) {
  const ms = c.cooldown_remaining_ms || 0;
  if (!ms || ms <= 0) return '';
  const text = humanizeMS(ms);
  return ` <span style="color: #dc2626; font-size: 0.875rem; font-weight: 500; background: linear-gradient(135deg, #fee2e2 0%, #fecaca 100%); padding: 2px 8px; border-radius: 4px; border: 1px solid #fca5a5;">⚠️ 冷却中·${text}</span>`;
}

function renderChannelStatsInline(stats, cache, channelType) {
  if (!stats) {
    return `<span class="channel-stat-badge" style="margin-left: 6px; color: var(--neutral-500);">统计: --</span>`;
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
    `<span class="channel-stat-badge" style="color: var(--neutral-800);"><strong>${rangeLabel}调用</strong> ${callText}</span>`,
    `<span class="channel-stat-badge" style="color: ${successRateColor};"><strong>率</strong> ${successRateText}</span>`,
    `<span class="channel-stat-badge" style="color: var(--primary-700);"><strong>首字</strong> ${avgFirstByteText}</span>`,
    `<span class="channel-stat-badge" style="color: var(--neutral-800);"><strong>In</strong> ${inputTokensText}</span>`,
    `<span class="channel-stat-badge" style="color: var(--neutral-800);"><strong>Out</strong> ${outputTokensText}</span>`
  ];

  const supportsCaching = channelType === 'anthropic' || channelType === 'codex';
  if (supportsCaching) {
    parts.push(
      `<span class="channel-stat-badge" style="color: var(--success-600); background: var(--success-50); border-color: var(--success-100);"><strong>缓存读</strong> ${cacheReadText}</span>`,
      `<span class="channel-stat-badge" style="color: var(--primary-700); background: var(--primary-50); border-color: var(--primary-100);"><strong>缓存建</strong> ${cacheCreationText}</span>`
    );
  }

  parts.push(
    `<span class="channel-stat-badge" style="color: var(--warning-700); background: var(--warning-50); border-color: var(--warning-100);"><strong>成本</strong> ${costDisplay}</span>`
  );

  return parts.join(' ');
}

function renderChannels(channelsToRender = channels) {
  const el = document.getElementById('channels-container');
  if (!channelsToRender || channelsToRender.length === 0) {
    el.innerHTML = '<div class="glass-card">暂无符合条件的渠道</div>';
    return;
  }
  el.innerHTML = channelsToRender.map(c => {
    const isCooldown = c.cooldown_remaining_ms > 0;
    const cardClasses = ['glass-card'];

    if (isCooldown) {
      cardClasses.push('channel-card-cooldown');
    }
    if (!c.enabled) {
      cardClasses.push('channel-disabled');
    }

    const channelTypeLabels = {
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
    const channelTypeRaw = (c.channel_type || '').toLowerCase();
    const channelTypeInfo = channelTypeLabels[channelTypeRaw || 'anthropic'] || channelTypeLabels['anthropic'];
    const channelTypeBadge = `<span style="background: ${channelTypeInfo.bgColor}; color: ${channelTypeInfo.color}; padding: 3px 10px; border-radius: 6px; font-size: 0.75rem; font-weight: 700; margin-left: 8px; border: 1.5px solid ${channelTypeInfo.borderColor}; letter-spacing: 0.025em; text-transform: uppercase;">${channelTypeInfo.text}</span>`;

    const stats = channelStatsById[c.id] || null;
    const successCount = stats ? stats.success : null;
    const errorCount = stats ? stats.error : null;
    const totalCount = stats ? stats.total : null;
    const successRateText = formatSuccessRate(successCount, totalCount);
    const avgFirstByteText = formatAvgFirstByte(stats ? stats.avgFirstByteTimeSeconds : null);
    const inputTokensText = formatMetricNumber(stats ? stats.totalInputTokens : null);
    const outputTokensText = formatMetricNumber(stats ? stats.totalOutputTokens : null);
    const cacheReadText = formatMetricNumber(stats ? stats.totalCacheReadInputTokens : null);
    const cacheCreationText = formatMetricNumber(stats ? stats.totalCacheCreationInputTokens : null);
    const costDisplay = formatCostValue(stats ? stats.totalCost : null);
    const showStatsInline = true;
    const statsInline = showStatsInline && stats
      ? renderChannelStatsInline(stats, {
          successRateText,
          avgFirstByteText,
          inputTokensText,
          outputTokensText,
          cacheReadText,
          cacheCreationText,
          costDisplay
        }, channelTypeRaw)
      : '';

    return `
      <div class="${cardClasses.join(' ')}" id="channel-${c.id}">
        <div class="flex justify-between items-center">
          <div style="flex: 1;">
            <div class="section-title">${escapeHtml(c.name)} ${channelTypeBadge} <span style="color: var(--neutral-500); font-size: 0.875rem; font-weight: 400;">(ID: ${c.id})</span> <span style="color: var(--neutral-600); font-size: 1rem; font-weight: 400; display: inline-block; max-width: 1100px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; vertical-align: bottom;" title="${Array.isArray(c.models) ? c.models.join(', ') : ''}">模型: ${Array.isArray(c.models) ? c.models.join(', ') : ''}</span></div>
            <div class="text-sm" style="color: var(--neutral-600); margin-top: 4px;">
              <div class="channel-meta-line">
                <span>URL: ${escapeHtml(c.url)} | 优先级: ${c.priority} | ${c.enabled ? '已启用' : '已禁用'}${inlineCooldownBadge(c)}</span>
                ${statsInline ? `<span class="channel-stats-inline">${statsInline}</span>` : ''}
              </div>
            </div>
          </div>
          <div class="channel-actions">
            <button class="btn-icon" onclick="editChannel(${c.id})" title="编辑">编辑</button>
            <button class="btn-icon" onclick="testChannel(${c.id}, '${escapeHtml(c.name)}')" title="测试API Key">测试</button>
            <button class="btn-icon" onclick="toggleChannel(${c.id}, ${!c.enabled})">${c.enabled ? '禁用' : '启用'}</button>
            <button class="btn-icon" onclick="copyChannel(${c.id}, '${escapeHtml(c.name)}')" title="复制渠道">复制</button>
            <button class="btn-icon btn-danger" onclick="deleteChannel(${c.id}, '${escapeHtml(c.name)}')" title="删除">删除</button>
          </div>
        </div>
      </div>
    `;
  }).join('');
}
