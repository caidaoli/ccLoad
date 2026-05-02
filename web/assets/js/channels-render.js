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

const CHANNEL_PRIORITY_MIN = -99999;
const CHANNEL_PRIORITY_MAX = 99999;
let channelPrioritySaveTimers = new Map();

function escapeChannelRefreshText(value) {
  if (value === null || value === undefined) return '';
  return String(value).replace(/[&<>"']/g, c => ({
    '&': '&amp;',
    '<': '&lt;',
    '>': '&gt;',
    '"': '&quot;',
    "'": '&#39;'
  }[c]));
}

function normalizeBatchRefreshChannelID(channelID) {
  if (typeof normalizeSelectedChannelID === 'function') {
    return normalizeSelectedChannelID(channelID);
  }
  const numericID = Number(channelID);
  if (!Number.isFinite(numericID) || numericID <= 0) return '';
  return String(Math.trunc(numericID));
}

function getBatchRefreshResult(channelID) {
  if (typeof batchRefreshResultsByChannelId === 'undefined' || !batchRefreshResultsByChannelId) return null;
  const key = normalizeBatchRefreshChannelID(channelID);
  if (!key) return null;
  return batchRefreshResultsByChannelId.get(key) || null;
}

function buildBatchRefreshResultSummary(result) {
  const fetched = Number.isFinite(Number(result.fetched)) ? Number(result.fetched) : 0;
  const added = Number.isFinite(Number(result.added)) ? Number(result.added) : 0;
  const removed = Number.isFinite(Number(result.removed)) ? Number(result.removed) : 0;
  const total = Number.isFinite(Number(result.total)) ? Number(result.total) : 0;

  switch (result.status) {
    case 'processing':
      return window.t('channels.batchRefreshRowProcessing');
    case 'updated':
      if (result.mode === 'replace') {
        return window.t('channels.batchRefreshRowUpdatedReplace', { fetched, removed, total });
      }
      return window.t('channels.batchRefreshRowUpdatedMerge', { fetched, added, total });
    case 'unchanged':
      return window.t('channels.batchRefreshRowUnchanged', { fetched, total });
    case 'failed':
      return window.t('channels.batchRefreshRowFailed', { error: result.summary || window.t('common.failed') });
    default:
      return '';
  }
}

function buildBatchRefreshStatusHtml(result) {
  if (!result || !result.status) return '';

  const status = result.status;
  const statusLabel = window.t(`channels.batchRefreshStatus.${status}`);
  const summary = buildBatchRefreshResultSummary(result);
  const escapedSummary = escapeChannelRefreshText(summary);
  const escapedTitle = escapeChannelRefreshText(result.detail || summary);
  const channelID = escapeChannelRefreshText(result.channelID || '');

  const statusHtml = `<span class="channel-refresh-result__status">${escapeChannelRefreshText(statusLabel)}</span>`;
  const summaryHtml = `<span class="channel-refresh-result__summary" title="${escapedTitle}">${escapedSummary}</span>`;

  if (status !== 'failed') {
    return `<div class="channel-refresh-result channel-refresh-result--${status}">${statusHtml}${summaryHtml}</div>`;
  }

  const detail = escapeChannelRefreshText(result.detail || result.summary || window.t('common.failed'));
  return `<div class="channel-refresh-result channel-refresh-result--failed">
    <div class="channel-refresh-result__line">
      ${statusHtml}${summaryHtml}
      <details class="channel-refresh-result__detail">
        <summary>${escapeChannelRefreshText(window.t('channels.batchRefreshDetail'))}</summary>
        <pre>${detail}</pre>
      </details>
      <button type="button" class="channel-refresh-result-action" data-action="clear-batch-refresh-result" data-channel-id="${channelID}">${escapeChannelRefreshText(window.t('channels.batchRefreshClear'))}</button>
    </div>
  </div>`;
}

function applyBatchRefreshResultClass(row, result) {
  if (!row) return;
  row.classList.remove(
    'channel-row-refresh-processing',
    'channel-row-refresh-updated',
    'channel-row-refresh-unchanged',
    'channel-row-refresh-failed'
  );
  if (result && result.status) {
    row.classList.add(`channel-row-refresh-${result.status}`);
  }
}

function renderChannelBatchRefreshResult(channelID) {
  const key = normalizeBatchRefreshChannelID(channelID);
  if (!key) return;
  const row = document.getElementById(`channel-${key}`);
  if (!row) return;
  const result = getBatchRefreshResult(key);
  applyBatchRefreshResultClass(row, result);
  const slot = row.querySelector('.ch-refresh-result-slot');
  if (slot) {
    slot.innerHTML = buildBatchRefreshStatusHtml(result);
  }
}

function setBatchRefreshResult(channelID, result) {
  if (typeof batchRefreshResultsByChannelId === 'undefined' || !batchRefreshResultsByChannelId) return;
  const key = normalizeBatchRefreshChannelID(channelID);
  if (!key) return;
  const nextResult = Object.assign({}, result, {
    channelID: key,
    stamp: Date.now()
  });
  batchRefreshResultsByChannelId.set(key, nextResult);
  renderChannelBatchRefreshResult(key);
}

function clearBatchRefreshResult(channelID) {
  if (typeof batchRefreshResultsByChannelId === 'undefined' || !batchRefreshResultsByChannelId) return;
  const key = normalizeBatchRefreshChannelID(channelID);
  if (!key) return;
  batchRefreshResultsByChannelId.delete(key);
  renderChannelBatchRefreshResult(key);
}

function clearAllBatchRefreshResults() {
  if (typeof batchRefreshResultsByChannelId === 'undefined' || !batchRefreshResultsByChannelId || batchRefreshResultsByChannelId.size === 0) {
    return;
  }
  const keys = Array.from(batchRefreshResultsByChannelId.keys());
  batchRefreshResultsByChannelId.clear();
  keys.forEach((key) => {
    renderChannelBatchRefreshResult(key);
  });
}

async function copyChannelLastRequestFailure(btn) {
  const lastRequest = btn && btn.closest ? btn.closest('.ch-last-request') : null;
  const pre = lastRequest && lastRequest.querySelector ? lastRequest.querySelector('.ch-last-request__detail pre') : null;
  const text = pre ? pre.textContent : '';
  if (!text) return;

  try {
    if (window.copyToClipboard) {
      await window.copyToClipboard(text);
    } else if (typeof navigator !== 'undefined' && navigator.clipboard && navigator.clipboard.writeText) {
      await navigator.clipboard.writeText(text);
    } else {
      throw new Error('copy failed');
    }
    const originalText = btn.textContent;
    btn.textContent = window.t('channels.batchRefreshCopied');
    setTimeout(() => { btn.textContent = originalText; }, 1500);
  } catch (error) {
    console.error('Copy last request failure failed', error);
    if (window.showError) window.showError(window.t('channels.keyCopyFailed'));
  }
}
if (!window.ChannelProtocolConfig) {
  throw new Error('ChannelProtocolConfig helper is required before channels-render.js');
}

function buildEffectivePriorityHtml(channel) {
  const basePriority = channel.priority;
  const priorityLabel = window.t('channels.table.priority');
  const healthLabel = window.t('channels.stats.healthScoreLabel');
  const channelId = Number(channel.id) || 0;
  const escapedPriorityLabel = escapeChannelRefreshText(priorityLabel);
  const basePriorityValue = normalizeInlinePriorityValue(basePriority, 0);
  const baseRow = buildPriorityEditorRow(channelId, basePriorityValue, escapedPriorityLabel);

  if (channel.effective_priority === undefined || channel.effective_priority === null) {
    const title = `${priorityLabel}: ${basePriority}`;
    const rows = [baseRow];
    return `<div class="ch-priority-stack" title="${title.replace(/"/g, '&quot;')}">${rows.join('')}</div>`;
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

  const rows = [baseRow];
  if (!isConsistent) {
    rows.push(buildPriorityRow('ch-priority-health', healthValueClass, effPriority));
  }
  return `<div class="ch-priority-stack" title="${title.replace(/"/g, '&quot;')}">${rows.join('')}</div>`;
}

function normalizeInlinePriorityValue(value, fallback) {
  const fallbackValue = Number.isFinite(Number(fallback)) ? Number(fallback) : 0;
  const num = Number(value);
  if (!Number.isFinite(num)) return Math.trunc(fallbackValue);
  return Math.max(CHANNEL_PRIORITY_MIN, Math.min(CHANNEL_PRIORITY_MAX, Math.trunc(num)));
}

function buildPriorityEditorRow(channelId, priority, priorityLabel) {
  const disabledAttr = channelId > 0 ? '' : ' disabled';
  return `<div class="ch-priority-row ch-priority-base">
    <div class="ch-priority-editor-wrap" data-channel-id="${channelId}">
      <div class="ch-priority-editor">
        <input class="ch-priority-input" type="number" min="${CHANNEL_PRIORITY_MIN}" max="${CHANNEL_PRIORITY_MAX}" step="1" value="${priority}" data-channel-id="${channelId}" data-original-priority="${priority}" aria-label="${priorityLabel}"${disabledAttr}>
      </div>
    </div>
  </div>`;
}

function setInlinePrioritySaving(input, saving) {
  const editorWrap = input && input.closest ? input.closest('.ch-priority-editor-wrap') : null;
  if (!editorWrap) return;
  editorWrap.classList.toggle('is-saving', saving);
  editorWrap.querySelectorAll('button, input').forEach((el) => {
    el.disabled = saving;
  });
}

function updateLocalChannelPriority(channelId, priority) {
  const updateList = (list) => {
    if (!Array.isArray(list)) return;
    list.forEach((channel) => {
      if (Number(channel && channel.id) !== channelId) return;
      const oldPriority = normalizeInlinePriorityValue(channel.priority, 0);
      if (channel.effective_priority !== undefined && channel.effective_priority !== null) {
        const effectiveOffset = Number(channel.effective_priority) - oldPriority;
        if (Number.isFinite(effectiveOffset)) {
          channel.effective_priority = priority + effectiveOffset;
        }
      }
      channel.priority = priority;
    });
  };
  if (typeof channels !== 'undefined') updateList(channels);
  if (typeof filteredChannels !== 'undefined') updateList(filteredChannels);
}

async function saveInlineChannelPriority(input) {
  if (!input) return;
  const channelId = Number(input.dataset.channelId);
  if (!Number.isFinite(channelId) || channelId <= 0) return;

  const originalPriority = normalizeInlinePriorityValue(input.dataset.originalPriority, 0);
  const nextPriority = normalizeInlinePriorityValue(input.value, originalPriority);
  input.value = String(nextPriority);
  if (nextPriority === originalPriority) {
    input.classList.remove('is-dirty');
    return;
  }

  input.dataset.originalPriority = String(nextPriority);

  try {
    setInlinePrioritySaving(input, true);
    await fetchDataWithAuth('/admin/channels/batch-priority', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json'
      },
      body: JSON.stringify({ updates: [{ id: channelId, priority: nextPriority }] })
    });

    input.classList.remove('is-dirty');
    updateLocalChannelPriority(channelId, nextPriority);
    if (typeof clearChannelsCache === 'function') clearChannelsCache();
    if (typeof filterChannels === 'function') filterChannels();
    if (window.showSuccess) window.showSuccess(window.t('channels.priorityUpdateSuccess'));
  } catch (error) {
    console.error('Update channel priority failed:', error);
    input.dataset.originalPriority = String(originalPriority);
    input.value = String(originalPriority);
    input.classList.remove('is-dirty');
    if (window.showError) {
      window.showError(error.message || window.t('channels.priorityUpdateFailed'));
    }
  } finally {
    setInlinePrioritySaving(input, false);
  }
}

function queueInlineChannelPrioritySave(input, delay = 500) {
  if (!input) return;
  const channelId = Number(input.dataset.channelId);
  if (!Number.isFinite(channelId) || channelId <= 0) return;
  input.classList.add('is-dirty');
  const existingTimer = channelPrioritySaveTimers.get(channelId);
  if (existingTimer) clearTimeout(existingTimer);
  const timer = setTimeout(() => {
    channelPrioritySaveTimers.delete(channelId);
    saveInlineChannelPriority(input);
  }, delay);
  channelPrioritySaveTimers.set(channelId, timer);
}

function flushInlineChannelPrioritySave(input) {
  if (!input) return;
  const channelId = Number(input.dataset.channelId);
  const existingTimer = channelPrioritySaveTimers.get(channelId);
  if (existingTimer) {
    clearTimeout(existingTimer);
    channelPrioritySaveTimers.delete(channelId);
  }
  return saveInlineChannelPriority(input);
}

function inlineCooldownBadge(c) {
  const ms = c.cooldown_remaining_ms || 0;
  if (!ms || ms <= 0) return '';
  const text = humanizeMS(ms);
  return `<span style="display: inline-flex; align-items: center; color: #dc2626; font-size: 0.68rem; font-weight: 600; line-height: 1; background: linear-gradient(135deg, #fee2e2 0%, #fecaca 100%); padding: 1px 6px; border-radius: 4px; border: 1px solid #fca5a5; vertical-align: middle;">${window.t('channels.cooldownBadge', { time: text })}</span>`;
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

function buildInlineNameBadgeStyle({ background, color, borderColor, borderStyle = 'solid' }) {
  return [
    'display: inline-flex',
    'align-items: center',
    `background: ${background}`,
    `color: ${color}`,
    'padding: 2px 6px',
    'border-radius: 999px',
    'font-size: 0.68rem',
    'font-weight: 600',
    `border: 1px ${borderStyle} ${borderColor}`,
    'line-height: 1'
  ].join('; ');
}

/**
 * 生成渠道类型徽章HTML
 * @param {string} channelType - 渠道类型
 * @returns {string} 徽章HTML
 */
function buildChannelTypeBadge(channelType) {
  const config = getChannelTypeConfig(channelType);
  const badgeStyle = buildInlineNameBadgeStyle({
    background: config.bgColor,
    color: config.color,
    borderColor: config.borderColor
  });
  return `<span style="${badgeStyle}">${config.text}</span>`;
}

function getProtocolTransformBadgeLabel(protocol) {
  const labels = {
    anthropic: ['channels.protocolBadgeAnthropic', 'Claude'],
    codex: ['channels.protocolTransformCodex', 'Codex'],
    openai: ['channels.protocolTransformOpenAI', 'OpenAI'],
    gemini: ['channels.protocolTransformGemini', 'Gemini']
  };
  const [translationKey, fallback] = labels[protocol] || [];
  if (!translationKey) return protocol;
  if (window.t) {
    const translated = window.t(translationKey);
    if (translated && translated !== translationKey) {
      return translated;
    }
  }
  return fallback;
}

function normalizeProtocolTransformsForDisplay(channelType, protocolTransforms) {
  return window.ChannelProtocolConfig.normalizeProtocolTransformsForChannel(channelType, protocolTransforms);
}

function buildProtocolTransformBadges(channelType, protocolTransforms) {
  const transforms = normalizeProtocolTransformsForDisplay(channelType, protocolTransforms);
  if (transforms.length === 0) return '';

  const translatedPrefix = window.t ? window.t('channels.modal.protocolTransforms') : '';
  const titlePrefix = translatedPrefix && translatedPrefix !== 'channels.modal.protocolTransforms'
    ? translatedPrefix
    : 'Additional Protocol Transforms';

  const protocolBadgeStyle = buildInlineNameBadgeStyle({
    background: '#fff7ed',
    color: '#9a3412',
    borderColor: '#fdba74',
    borderStyle: 'dashed'
  });

  return `<span style="display: inline-flex; align-items: center; gap: 4px; flex-wrap: wrap; margin-left: 6px; vertical-align: middle;">${transforms.map((protocol) => `<span title="${titlePrefix}: ${getProtocolTransformBadgeLabel(protocol)}" style="${protocolBadgeStyle}">${getProtocolTransformBadgeLabel(protocol)}</span>`).join('')}</span>`;
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

function formatChannelRelativeTime(timestampMs, nowMs = Date.now()) {
  const ts = Number(timestampMs);
  if (!Number.isFinite(ts) || ts <= 0) return '';

  const seconds = Math.max(1, Math.floor((nowMs - ts) / 1000));
  if (seconds < 60) {
    return window.t('channels.lastSuccess.secondsAgo', { count: seconds });
  }

  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) {
    return window.t('channels.lastSuccess.minutesAgo', { count: minutes });
  }

  const hours = Math.floor(minutes / 60);
  if (hours < 24) {
    return window.t('channels.lastSuccess.hoursAgo', { count: hours });
  }

  const days = Math.floor(hours / 24);
  return window.t('channels.lastSuccess.daysAgo', { count: days });
}

function buildChannelLastSuccessHtml(stats) {
  if (!stats) {
    return '';
  }

  const lastSuccessAt = Number(stats.lastSuccessAt || 0);
  const lastRequestAt = Number(stats.lastRequestAt || 0);
  const status = Number(stats.lastRequestStatus);
  const hasRequest = lastRequestAt > 0 && Number.isFinite(status) && status > 0;

  if (lastSuccessAt > 0) {
    return `<div class="ch-last-status ch-last-status--ok">${escapeChannelRefreshText(formatChannelRelativeTime(lastSuccessAt))}</div>`;
  }

  if (hasRequest) {
    return `<div class="ch-last-status ch-last-status--empty">${escapeChannelRefreshText(window.t('channels.lastSuccess.never'))}</div>`;
  }

  return '';
}

function buildChannelLastRequestFailureHtml(stats) {
  if (!stats) return '';

  const lastSuccessAt = Number(stats.lastSuccessAt || 0);
  const lastRequestAt = Number(stats.lastRequestAt || 0);
  const status = Number(stats.lastRequestStatus);
  const hasRequest = lastRequestAt > 0 && Number.isFinite(status) && status > 0;
  const requestFailed = hasRequest && (status < 200 || status >= 300) && status !== 499;
  if (!requestFailed) return '';

  const statusText = escapeChannelRefreshText(window.t('channels.lastSuccess.failedStatus', { status }));
  const relativeTime = formatChannelRelativeTime(lastRequestAt);
  const timeText = escapeChannelRefreshText(window.t('channels.lastSuccess.failedAt', { time: relativeTime }));
  const message = String(stats.lastRequestMessage || window.t('channels.lastSuccess.failedNoMessage'));
  const escapedMessage = escapeChannelRefreshText(message);
  return `<div class="ch-last-request">
    <span class="ch-last-request__state">${statusText}</span>
    <span class="ch-last-request__time">${timeText}</span>
    <details class="ch-last-request__detail">
      <summary>${escapeChannelRefreshText(window.t('channels.lastSuccess.detail'))}</summary>
      <div class="ch-last-request__panel">
        <pre>${escapedMessage}</pre>
        <button type="button" class="ch-last-request__copy" data-action="copy-last-request-failure">${escapeChannelRefreshText(window.t('common.copy'))}</button>
      </div>
    </details>
  </div>`;
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
  const batchRefreshResult = getBatchRefreshResult(channel.id);

  // 预计算统计数据
  const statsCache = stats ? {
    inputTokensText: formatMetricNumber(stats.totalInputTokens),
    outputTokensText: formatMetricNumber(stats.totalOutputTokens),
    cacheReadText: formatMetricNumber(stats.totalCacheReadInputTokens),
    cacheCreationTokens: stats.totalCacheCreationInputTokens || 0,
    cacheCreationText: formatMetricNumber(stats.totalCacheCreationInputTokens),
    costInfo: getCostDisplayInfo(stats.totalCost, stats.effectiveCost)
  } : null;

  // 模型文本
  const modelsText = Array.isArray(channel.models)
    ? channel.models.map(m => m.model || m).join(', ')
    : '';

  const durationHtml = buildChannelTimingHtml(stats);
  const lastSuccessHtml = buildChannelLastSuccessHtml(stats);

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
    costHtml = buildCostStackHtml(stats.totalCost, stats.effectiveCost, { tone: 'success' });
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
  const lastRequestFailureHtml = buildChannelLastRequestFailureHtml(stats);
  if (batchRefreshResult && batchRefreshResult.status) {
    rowClasses.push(`channel-row-refresh-${batchRefreshResult.status}`);
  }

  // 准备模板数据
  const cardData = {
    rowClasses: rowClasses.join(' '),
    id: channel.id,
    name: channel.name,
    nameMultiplierBadge: buildCornerMultiplierBadge(channel.cost_multiplier),
    typeBadge: buildChannelTypeBadge(channelTypeRaw),
    protocolTransformBadges: buildProtocolTransformBadges(channelTypeRaw, channel.protocol_transforms),
    url: channel.url,
    batchRefreshStatusHtml: buildBatchRefreshStatusHtml(batchRefreshResult),
    modelsText: modelsText,
    priority: channel.priority,
    effectivePriorityHtml: buildEffectivePriorityHtml(channel),
    cooldownBadge: inlineCooldownBadge(channel),
    durationHtml: durationHtml,
    usageHtml: usageHtml,
    costHtml: costHtml,
    lastSuccessHtml: lastSuccessHtml,
    lastRequestFailureHtml: lastRequestFailureHtml,
    healthHtml: healthHtml,
    enabled: channel.enabled,
    toggleTitle: channel.enabled ? window.t('channels.toggleDisable') : window.t('channels.toggleEnable'),
    toggleSwitchClass: channel.enabled ? 'channel-enable-switch--on' : 'channel-enable-switch--off',
    durationCellClass: durationHtml ? '' : 'ch-mobile-empty',
    usageCellClass: usageHtml ? '' : 'ch-mobile-empty',
    costCellClass: costHtml ? '' : 'ch-mobile-empty',
    mobileLabelModels: window.t('channels.table.models'),
    mobileLabelPriority: window.t('channels.table.priority'),
    mobileLabelDuration: window.t('channels.table.duration'),
    mobileLabelUsage: window.t('channels.table.usage'),
    mobileLabelCost: window.t('channels.stats.cost'),
    mobileLabelLastSuccess: window.t('channels.table.lastSuccess'),
    mobileLabelEnabled: window.t('channels.table.enabled'),
    mobileLabelActions: window.t('channels.table.actions')
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
    const headerCheckbox = e.target.closest('#visibleSelectionCheckbox');
    if (headerCheckbox) {
      toggleVisibleChannelsSelection();
      return;
    }

    const checkbox = e.target.closest('.channel-select-checkbox');

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

  container.addEventListener('input', (e) => {
    const input = e.target.closest('.ch-priority-input');
    if (!input) return;
    queueInlineChannelPrioritySave(input);
  });

  container.addEventListener('keydown', (e) => {
    const input = e.target.closest('.ch-priority-input');
    if (!input) return;
    if (e.key === 'Enter') {
      e.preventDefault();
      flushInlineChannelPrioritySave(input);
    } else if (e.key === 'Escape') {
      const originalPriority = normalizeInlinePriorityValue(input.dataset.originalPriority, 0);
      input.value = String(originalPriority);
      input.classList.remove('is-dirty');
    }
  });

  container.addEventListener('focusout', (e) => {
    const input = e.target.closest('.ch-priority-input');
    if (!input) return;
    flushInlineChannelPrioritySave(input);
  });

  // 事件委托：处理所有渠道操作按钮
  container.addEventListener('click', (e) => {
    const lastRequestCopyBtn = e.target.closest('.ch-last-request__copy');
    if (lastRequestCopyBtn) {
      copyChannelLastRequestFailure(lastRequestCopyBtn);
      return;
    }

    const refreshResultBtn = e.target.closest('.channel-refresh-result-action');
    if (refreshResultBtn) {
      const channelId = parseInt(refreshResultBtn.dataset.channelId, 10);
      switch (refreshResultBtn.dataset.action) {
        case 'clear-batch-refresh-result':
          clearBatchRefreshResult(channelId);
          break;
      }
      return;
    }

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

  // 点击 details 外部时自动关闭（仅注册一次）
  if (!document._chLastRequestDetailListener) {
    document._chLastRequestDetailListener = true;
    document.addEventListener('click', (e) => {
      if (e.target.closest('.ch-last-request__detail')) return;
      document.querySelectorAll('.ch-last-request__detail[open]').forEach((d) => {
        d.removeAttribute('open');
      });
    }, true);
  }
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
      <th class="ch-col-last-success">${window.t('channels.table.lastSuccess')}</th>
      <th class="ch-col-enabled">${window.t('channels.table.enabled')}</th>
      <th class="ch-col-actions">${window.t('channels.table.actions')}</th>
    </tr>
  </thead>`;

  const tbody = document.createElement('tbody');
  channelsToRender.forEach(channel => {
    const row = createChannelCard(channel);
    if (row) tbody.appendChild(row);
  });

  el.innerHTML = `<div class="table-container channel-table-container"><table class="modern-table channel-table">${thead}</table></div>`;
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
