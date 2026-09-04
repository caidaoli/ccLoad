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
const CHANNEL_PRIORITY_MAX = 9999999;
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

  const planTone = ['plus', 'pro', 'team'].find(tier => planTokens.includes(tier));
  const toneClass = planTone ? ` ch-oauth-plan-badge--${planTone}` : '';
  return `<span class="ch-oauth-plan-badge${toneClass}">${escapeChannelRefreshText(planType)}</span>`;
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
  const disabledAttr = channelId > 0 && !isTokenChannelsReadOnly() ? '' : ' disabled';
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
  if (!input || isTokenChannelsReadOnly()) return;
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

function queueInlineChannelPrioritySave(input, delay = 1000) {
  if (!input || isTokenChannelsReadOnly()) return;
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
  if (!input || isTokenChannelsReadOnly()) return;
  const channelId = Number(input.dataset.channelId);
  const existingTimer = channelPrioritySaveTimers.get(channelId);
  if (existingTimer) {
    clearTimeout(existingTimer);
    channelPrioritySaveTimers.delete(channelId);
  }
  return saveInlineChannelPriority(input);
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
    const rateLimited = point.rate_limited || 0;
    const realErrors = (point.error || 0) - rateLimited;

    // 配色：所有失败都是限流 → 蓝色；有真实错误 → 按成功率分级(绿/橙/红)
    const className = (realErrors === 0 && rateLimited > 0)
      ? 'rate-limited'
      : rate >= 0.95 ? 'healthy' : rate >= 0.80 ? 'warning' : 'critical';

    const d = new Date(point.ts);
    const timeStr = `${String(d.getMonth() + 1).padStart(2, '0')}/${String(d.getDate()).padStart(2, '0')} ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;

    let title = `${timeStr}\n${window.t('stats.tooltipSuccess')}: ${point.success || 0} / ${window.t('stats.tooltipFailed')}: ${point.error || 0}`;
    if (rateLimited > 0) title += ` (${window.t('stats.tooltipRateLimited')}: ${rateLimited})`;
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
  const firstByteColor = window.getFirstByteTimingColor(avgFirstByte);
  const durationColor = window.getDurationTimingColor(avgDuration);

  const rows = [];
  if (avgFirstByte > 0) {
    rows.push(`<div class="ch-timing-row"><span class="ch-timing-label">${window.t('channels.stats.firstByte')}</span><span class="ch-timing-value" style="color: ${firstByteColor};">${avgFirstByte.toFixed(2)}${window.t('common.seconds')}</span></div>`);
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

function buildChannelLastRequestFailureHtml(stats) {
  if (!stats) return '';

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

function formatRemainingStatusTime(remainingMS, secondsKey, minutesKey, hoursMinutesKey, daysHoursKey) {
  const ms = Math.max(0, Number(remainingMS) || 0);
  if (ms <= 5 * 60 * 1000) {
    return window.t(secondsKey, { count: Math.ceil(ms / 1000) });
  }
  const totalMinutes = Math.ceil(ms / 60000);
  if (ms < 60 * 60 * 1000) {
    return window.t(minutesKey, { count: totalMinutes });
  }
  if (daysHoursKey && ms >= 48 * 60 * 60 * 1000) {
    return window.t(daysHoursKey, {
      days: Math.floor(totalMinutes / (24 * 60)),
      hours: Math.floor((totalMinutes % (24 * 60)) / 60)
    });
  }
  return window.t(hoursMinutesKey, {
    hours: Math.floor(totalMinutes / 60),
    minutes: totalMinutes % 60
  });
}

function formatCooldownRecoveryTime(remainingMS, daysHoursKey) {
  return formatRemainingStatusTime(
    remainingMS,
    'channels.status.secondsUntilRecovery',
    'channels.status.minutesUntilRecovery',
    'channels.status.hoursMinutesUntilRecovery',
    daysHoursKey
  );
}

function formatProtocolProbeRetryTime(remainingMS) {
  return formatRemainingStatusTime(
    remainingMS,
    'channels.status.secondsUntilRetry',
    'channels.status.minutesUntilRetry',
    'channels.status.hoursMinutesUntilRetry'
  );
}

function formatOAuthUsagePercent(value) {
  const percent = Math.min(100, Math.max(0, Number(value) || 0));
  return Number.isInteger(percent) ? String(percent) : percent.toFixed(1).replace(/\.0$/, '');
}

// 累计标准成本按美元显示，与渠道日消费同一形状，无需本地化前缀。
function formatOAuthAccumulatedCost(standardCostMicroUSD) {
  const microUSD = Number(standardCostMicroUSD);
  if (!Number.isFinite(microUSD) || microUSD < 0) return '';
  return `$${(microUSD / 1_000_000).toFixed(1)}`;
}

function formatOAuthEstimatedTotalCost(standardCostMicroUSD, remainingPercent) {
  const microUSD = Number(standardCostMicroUSD);
  const remaining = Number(remainingPercent);
  if (!Number.isFinite(microUSD) || microUSD < 0 || !Number.isFinite(remaining) || remaining >= 100) {
    return '';
  }
  const usedRatio = 1 - Math.min(100, Math.max(0, remaining)) / 100;
  if (usedRatio <= 0) return '';
  const estimatedMicroUSD = microUSD / usedRatio;
  if (!Number.isFinite(estimatedMicroUSD) || estimatedMicroUSD < 0) return '';
  return `$${(estimatedMicroUSD / 1_000_000).toFixed(1)}`;
}

// 累计成本按上游窗口标识（limit_name|kind）取用：同一时长可能对应多个互不相干的窗口。
function oauthAccumulatedCostByKey(quotaCostUsage, key) {
  const windows = Array.isArray(quotaCostUsage?.windows) ? quotaCostUsage.windows : [];
  const match = windows.find(item => item?.key === key);
  return match ? match.standard_cost_microusd : null;
}

function formatOAuthUsageResetAt(resetAt) {
  const timestamp = Number(resetAt);
  if (!Number.isFinite(timestamp) || timestamp <= 0) return '';
  const date = new Date(timestamp * 1000);
  if (Number.isNaN(date.getTime())) return '';
  const pad = value => String(value).padStart(2, '0');
  return `${pad(date.getMonth() + 1)}/${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

function formatOAuthUsageWindowDuration(seconds) {
  const duration = Math.max(0, Number(seconds) || 0);
  const day = 24 * 60 * 60;
  if (duration >= 28 * day && duration <= 31 * day) return window.t('channels.oauth.usageMonthly');
  if (duration === 7 * day) return window.t('channels.oauth.usageWeekly');
  if (duration > 0 && duration % day === 0) {
    return window.t('channels.oauth.usageDays', { count: duration / day });
  }
  if (duration > 0 && duration % (60 * 60) === 0) {
    return window.t('channels.oauth.usageHours', { count: duration / (60 * 60) });
  }
  return window.t('channels.oauth.usageQuota');
}

function isCodexSparkLimitName(limitName) {
  const normalized = String(limitName || '').trim().toLowerCase();
  return normalized === 'codex-spark' || normalized === 'gpt-5.3-codex-spark';
}

function formatCodexSparkUsageLabel(windowInfo) {
  const duration = Math.max(0, Number(windowInfo?.limit_window_seconds) || 0);
  if (duration === 5 * 60 * 60) return window.t('channels.oauth.usageCodexSparkFiveHour');
  if (duration === 7 * 24 * 60 * 60) return window.t('channels.oauth.usageCodexSparkWeekly');
  return '';
}

function orderCodexUsageWindows(windows) {
  const groupOrder = new Map();
  windows.forEach((windowInfo, index) => {
    const key = String(windowInfo?.limit_name || '').trim().toLowerCase() || 'codex';
    if (!groupOrder.has(key)) groupOrder.set(key, index);
  });
  const kindOrder = { primary: 0, secondary: 1 };
  return [...windows].sort((left, right) => {
    const leftName = String(left?.limit_name || '').trim().toLowerCase() || 'codex';
    const rightName = String(right?.limit_name || '').trim().toLowerCase() || 'codex';
    const groupDelta = (groupOrder.get(leftName) ?? 0) - (groupOrder.get(rightName) ?? 0);
    if (groupDelta !== 0) return groupDelta;
    return (kindOrder[String(left?.kind || '').trim().toLowerCase()] ?? 2) -
      (kindOrder[String(right?.kind || '').trim().toLowerCase()] ?? 2);
  });
}

function formatOAuthUsageLimitName(limitName) {
  const normalized = String(limitName || '').trim().toLowerCase();
  if (!normalized || normalized === 'codex') return '';
  if (isCodexSparkLimitName(limitName)) return 'Spark';
  if (normalized === 'gemini models') return 'Gemini';
  // Z.ai 的 token 窗口只有时长有信息量，时长已单独渲染，避免出现「five_hour 5小时」。
  if (normalized === 'five_hour' || normalized === 'weekly') return '';
  if (normalized === 'mcp_limit') return 'MCP';
  if (normalized === 'included') return window.t('channels.cursor.usageMonthlyLimit');
  if (normalized === 'api') return window.t('channels.cursor.usageOtherModels');
  if (normalized === 'auto') return window.t('channels.cursor.usageCursorModels');
  if (normalized === 'claude and gpt models') return 'Claude';
  return String(limitName).trim();
}

function formatOAuthUsageError(value) {
  const raw = String(value || '').trim();
  if (!raw) return '';
  let payload;
  try {
    payload = JSON.parse(raw);
  } catch (_) {
    return raw;
  }
  if (typeof payload === 'string') return payload.trim() || raw;
  if (!payload || typeof payload !== 'object' || Array.isArray(payload)) return raw;

  const nested = payload.error && typeof payload.error === 'object' ? payload.error : null;
  const code = String(
    (typeof payload.error === 'string' ? payload.error : '') ||
    nested?.code || nested?.type || payload.code || payload.type || ''
  ).trim();
  const description = String(
    payload.error_description || nested?.message || payload.message || payload.description || ''
  ).trim();
  return description || code || raw;
}

function oauthUsageLevel(remainingPercent) {
  if (remainingPercent >= 70) return 'high';
  if (remainingPercent >= 30) return 'medium';
  if (remainingPercent > 0) return 'low';
  return 'empty';
}

function orderCursorUsageWindows(windows) {
  const order = { auto: 0, api: 1, included: 2 };
  return [...windows].sort((left, right) => {
    const leftOrder = order[String(left?.limit_name || '').trim().toLowerCase()] ?? 3;
    const rightOrder = order[String(right?.limit_name || '').trim().toLowerCase()] ?? 3;
    return leftOrder - rightOrder;
  });
}

function formatCursorUsageNotice(value) {
  const message = String(value || '').trim();
  if (message.toLowerCase() === "you've hit your usage limit") {
    return '';
  }
  return message;
}

function formatZedUsageNotice(data) {
  switch (String(data?.entitlement_status || '').trim().toLowerCase()) {
    case 'unmetered':
      return window.t('channels.zed.usageUnmetered');
    case 'restricted':
      return window.t('channels.zed.usageRestricted');
    case 'exhausted':
      return window.t('channels.zed.usageExhausted');
    default:
      return '';
  }
}

function buildOAuthUsageRefreshButton(channelID, loading = false, disabled = false) {
  const text = loading
    ? window.t('channels.oauth.usageRefreshing')
    : window.t('channels.oauth.usageRefresh');
  return `<button type="button" class="ch-oauth-usage__refresh channel-action-btn" data-action="refresh-oauth-usage" data-channel-id="${channelID}"${loading || disabled ? ' disabled' : ''}${loading ? ' aria-busy="true"' : ''}>${escapeChannelRefreshText(text)}</button>`;
}

function formatCodexResetCreditExpiry(expiresAt) {
  const date = new Date(String(expiresAt || '').trim());
  if (Number.isNaN(date.getTime()) || date.getTime() <= Date.now()) return null;
  const pad = value => String(value).padStart(2, '0');
  return {
    timestamp: date.getTime(),
    text: `${pad(date.getMonth() + 1)}/${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`
  };
}

function buildCodexResetCreditsHtml(data, state, channelID) {
  const resetCredits = data?.rate_limit_reset_credits || {};
  const rawCount = Number(resetCredits.available_count);
  const normalizedCount = Number.isInteger(rawCount) && rawCount > 0 ? rawCount : 0;
  const hasCreditList = Array.isArray(resetCredits.credits);
  const expiries = (hasCreditList ? resetCredits.credits : [])
    .map(credit => formatCodexResetCreditExpiry(credit?.expires_at))
    .filter(Boolean)
    .sort((left, right) => left.timestamp - right.timestamp);
  const availableCount = hasCreditList ? Math.min(normalizedCount, expiries.length) : normalizedCount;
  if (availableCount <= 0) return '';
  const visibleExpiries = expiries.slice(0, availableCount);
  const earliest = visibleExpiries[0]?.text || '';
  const resetting = state?.reset_status === 'loading';
  const stale = state?.reset_status === 'stale';
  const disabled = resetting || stale;
  const buttonText = resetting
    ? window.t('channels.oauth.resettingQuota')
    : window.t('channels.oauth.resetQuota');
  const resetError = String(state?.reset_error || '').trim();
  const expiryText = visibleExpiries.length > 0
    ? window.t('channels.oauth.resetCreditExpires', {
        time: visibleExpiries.map(expiry => expiry.text).join('、')
      })
    : window.t('channels.oauth.resetCreditExpiresUnknown');
  const escapedExpiryText = escapeChannelRefreshText(expiryText);
  return `<div class="ch-oauth-usage__credits">
    <div class="ch-oauth-usage__credits-summary">
      <span class="ch-oauth-usage__credit-count">${escapeChannelRefreshText(window.t('channels.oauth.resetCredits', { count: availableCount }))}</span>
      <span class="ch-oauth-usage__credit-expiry" title="${escapedExpiryText}">${escapedExpiryText}</span>
      <button type="button" class="ch-oauth-usage__reset-action channel-action-btn" data-action="reset-codex-quota" data-channel-id="${channelID}" data-reset-count="${availableCount}" data-reset-expiry="${escapeChannelRefreshText(earliest)}"${disabled ? ' disabled' : ''}${resetting ? ' aria-busy="true"' : ''}>${escapeChannelRefreshText(buttonText)}</button>
    </div>
    ${resetError ? `<div class="ch-oauth-usage__error" role="status">${escapeChannelRefreshText(resetError)}</div>` : ''}
  </div>`;
}

function formatXAIUsagePercent(value) {
  if (value === null || value === undefined || value === '' || !Number.isFinite(Number(value))) return '--';
  const percent = Math.min(100, Math.max(0, Number(value)));
  return `${Number.isInteger(percent) ? percent : percent.toFixed(2).replace(/0+$/, '').replace(/\.$/, '')}%`;
}

function formatXAIUsageMoney(cents) {
  if (cents === null || cents === undefined || cents === '' || !Number.isFinite(Number(cents))) return '--';
  return `US$${(Math.round(Number(cents)) / 100).toFixed(2)}`;
}

function formatXAIUsageReset(resetAt) {
  const date = new Date(String(resetAt || '').trim());
  if (Number.isNaN(date.getTime())) return '';
  const pad = value => String(value).padStart(2, '0');
  return `${pad(date.getMonth() + 1)}/${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

function xaiUsageNumber(value) {
  if (value === null || value === undefined || value === '') return null;
  const number = Number(value);
  return Number.isFinite(number) ? number : null;
}

function buildXAIUsageInlineRow(label, value) {
  return `<div class="ch-oauth-usage__window">
    <div class="ch-oauth-usage__meta">
      <span class="ch-oauth-usage__label" title="${escapeChannelRefreshText(label)}">${escapeChannelRefreshText(label)}</span>
      <span class="ch-oauth-usage__details">
        <span class="ch-oauth-usage__percent">${escapeChannelRefreshText(value)}</span>
      </span>
    </div>
  </div>`;
}

function buildXAIUsageRow(label, usedPercent, amount, resetAt, accumulatedCostMicroUSD) {
  const percent = formatXAIUsagePercent(usedPercent);
  const reset = formatXAIUsageReset(resetAt);
  const accumulatedCost = formatOAuthAccumulatedCost(accumulatedCostMicroUSD);
  const numericUsed = xaiUsageNumber(usedPercent);
  const remaining = numericUsed !== null ? Math.min(100, Math.max(0, 100 - numericUsed)) : 0;
  const ariaLabel = numericUsed === null
    ? `${label}: ${window.t('channels.oauth.usageUsed', { percent })}`
    : window.t('channels.oauth.usageRemaining', {
      label,
      percent: formatOAuthUsagePercent(remaining)
    });
  return `<div class="ch-oauth-usage__window">
    <div class="ch-oauth-usage__meta">
      <span class="ch-oauth-usage__heading">
        <span class="ch-oauth-usage__label" title="${escapeChannelRefreshText(label)}">${escapeChannelRefreshText(label)}</span>
        ${accumulatedCost ? `<span class="ch-oauth-usage__amount">${escapeChannelRefreshText(accumulatedCost)}</span>` : ''}
      </span>
      <span class="ch-oauth-usage__details">
        <span class="ch-oauth-usage__percent">${escapeChannelRefreshText(window.t('channels.oauth.usageUsed', { percent }))}</span>
        ${amount ? `<span class="ch-oauth-usage__amount">${escapeChannelRefreshText(amount)}</span>` : ''}
        ${reset ? `<span class="ch-oauth-usage__reset">${escapeChannelRefreshText(window.t('channels.oauth.usageReset', { time: reset }))}</span>` : ''}
      </span>
    </div>
    <div class="ch-oauth-usage__track" role="progressbar" aria-label="${escapeChannelRefreshText(ariaLabel)}" aria-valuemin="0" aria-valuemax="100"${numericUsed !== null ? ` aria-valuenow="${remaining}"` : ''}>
      <span class="ch-oauth-usage__fill ch-oauth-usage__fill--${oauthUsageLevel(remaining)}" style="width:${remaining}%"></span>
    </div>
  </div>`;
}

function buildXAIUsageRows(data) {
  const billing = data?.xai_billing || {};
  const rows = [];
  const plan = String(data?.plan_type || data?.subscription_tier || '').trim();
  if (plan) {
    rows.push(buildXAIUsageInlineRow(window.t('channels.oauth.usagePlan'), plan));
  }
  if (billing.weekly_present === true) {
    rows.push(buildXAIUsageRow(
      window.t('channels.oauth.usageWeekly'),
      billing.weekly_usage_percent,
      '',
      billing.weekly_reset_at,
      oauthAccumulatedCostByKey(data?.quota_cost_usage, 'xai|weekly')
    ));
  }
  const products = Array.isArray(billing.product_usage) ? billing.product_usage : [];
  for (const product of products) {
    rows.push(buildXAIUsageRow(
      window.t('channels.oauth.usageProduct', { product: String(product?.product || '') }),
      product?.usage_percent,
      '',
      ''
    ));
  }
  const onDemandCap = xaiUsageNumber(billing.on_demand_cap_cents);
  if (onDemandCap !== null && onDemandCap > 0) {
    const used = xaiUsageNumber(billing.on_demand_used_cents);
    const usedPercent = used !== null ? used * 100 / onDemandCap : null;
    rows.push(buildXAIUsageRow(
      window.t('channels.oauth.usageOnDemand'),
      usedPercent,
      `${formatXAIUsageMoney(billing.on_demand_used_cents)} / ${formatXAIUsageMoney(billing.on_demand_cap_cents)}`,
      ''
    ));
  } else {
    rows.push(buildXAIUsageInlineRow(
      window.t('channels.oauth.usageOnDemand'),
      window.t('channels.oauth.usageOnDemandDisabled')
    ));
  }
  const monthlyLimit = xaiUsageNumber(billing.monthly_limit_cents);
  const includedUsed = xaiUsageNumber(billing.included_used_cents);
  const monthlyPercent = monthlyLimit !== null && monthlyLimit > 0 && includedUsed !== null
    ? includedUsed * 100 / monthlyLimit
    : null;
  if (billing.monthly_present === true) {
    rows.push(buildXAIUsageRow(
      window.t('channels.oauth.usageMonthlyCredits'),
      monthlyPercent,
      `${formatXAIUsageMoney(billing.included_used_cents)} / ${formatXAIUsageMoney(billing.monthly_limit_cents)}`,
      billing.monthly_reset_at,
      oauthAccumulatedCostByKey(data?.quota_cost_usage, 'xai|monthly')
    ));
  }
  return rows;
}

function buildOAuthUsageStatusHtml(channel) {
  if (!['codex_oauth', 'antigravity_oauth', 'xai_oauth', 'anthropic_oauth', 'zai_oauth', 'cursor_oauth', 'zed_oauth'].includes(channel?.auth_type) ||
      (typeof isTokenChannelsReadOnly === 'function' && isTokenChannelsReadOnly())) {
    return '';
  }
  const liveState = typeof getOAuthUsageState === 'function' ? getOAuthUsageState(channel.id) : null;
  const state = liveState || (channel?.oauth_usage ? { status: 'ready', data: channel.oauth_usage } : null);
  if (!state) {
    return `<div class="ch-oauth-usage">${buildOAuthUsageRefreshButton(channel.id)}</div>`;
  }
  if (state.status === 'loading') {
    return `<div class="ch-oauth-usage">${buildOAuthUsageRefreshButton(channel.id, true)}</div>`;
  }
  if (state.status === 'error') {
    const fallback = window.t('channels.oauth.usageFailed');
    const message = formatOAuthUsageError(state.error) || fallback;
    return `<div class="ch-oauth-usage">
      ${buildOAuthUsageRefreshButton(channel.id)}
      <div class="ch-oauth-usage__error" title="${escapeChannelRefreshText(message)}">${escapeChannelRefreshText(message)}</div>
    </div>`;
  }

  const windows = Array.isArray(state.data?.windows) ? state.data.windows : [];
  const isXAI = channel?.auth_type === 'xai_oauth' || state.data?.provider === 'xai';
  const isCodex = channel?.auth_type === 'codex_oauth';
  const isCursor = channel?.auth_type === 'cursor_oauth' || state.data?.provider === 'cursor';
  const isZed = channel?.auth_type === 'zed_oauth' || state.data?.provider === 'zed';
  const displayedWindows = isCursor
    ? orderCursorUsageWindows(windows)
    : isCodex
      ? orderCodexUsageWindows(windows)
      : windows;
  const rows = isXAI ? buildXAIUsageRows(state.data) : displayedWindows.map((windowInfo, windowIndex) => {
    const remaining = Math.min(100, Math.max(0, Number(windowInfo?.remaining_percent) || 0));
    const percent = formatOAuthUsagePercent(remaining);
    const percentWithSymbol = `${percent}%`;
    const duration = isCursor ? '' : formatOAuthUsageWindowDuration(windowInfo?.limit_window_seconds);
    const limitName = formatOAuthUsageLimitName(windowInfo?.limit_name);
    const codexSparkLabel = isCodex && isCodexSparkLimitName(windowInfo?.limit_name)
      ? formatCodexSparkUsageLabel(windowInfo)
      : '';
    // 名称与时长的连接方式交给语言包：中文直接相连，英文才需要空格。
    const label = codexSparkLabel || (limitName
      ? window.t('channels.oauth.usageLabel', { name: limitName, duration })
      : duration);
    const resetAt = formatOAuthUsageResetAt(windowInfo?.reset_at);
    const accumulatedCost = formatOAuthAccumulatedCost(windowInfo?.standard_cost_microusd);
    const estimatedTotalCost = formatOAuthEstimatedTotalCost(windowInfo?.standard_cost_microusd, remaining);
    const compactAmount = accumulatedCost && estimatedTotalCost
      ? window.t('channels.oauth.usageCompactAmount', { used: accumulatedCost, estimated: estimatedTotalCost })
      : accumulatedCost
        ? window.t('channels.oauth.usageCompactUsed', { used: accumulatedCost })
        : '';
    const compactRemaining = window.t('channels.oauth.usageCompactRemaining', { percent });
    const detailAmount = accumulatedCost && estimatedTotalCost
      ? window.t('channels.oauth.usageDetailAmount', { used: accumulatedCost, estimated: estimatedTotalCost })
      : accumulatedCost
        ? window.t('channels.oauth.usageDetailUsed', { used: accumulatedCost })
        : '';
    const detailLines = [
      label,
      detailAmount,
      window.t('channels.oauth.usageDetailRemaining', { percent }),
      resetAt ? window.t('channels.oauth.usageReset', { time: resetAt }) : ''
    ].filter(Boolean);
    const tooltipID = `ch-oauth-usage-tooltip-${String(channel.id).replace(/[^a-zA-Z0-9_-]/g, '')}-${windowIndex}`;
    const ariaLabel = window.t('channels.oauth.usageRemaining', { label, percent });
    return `<div class="ch-oauth-usage__window">
      <div class="ch-oauth-usage__meta">
        <span class="ch-oauth-usage__summary" tabindex="0" aria-describedby="${escapeChannelRefreshText(tooltipID)}">
          <span class="ch-oauth-usage__heading">
            <span class="ch-oauth-usage__label">${escapeChannelRefreshText(label)}</span>
            ${compactAmount ? `<span class="ch-oauth-usage__amount">${escapeChannelRefreshText(compactAmount)}</span>` : ''}
          </span>
          <span class="ch-oauth-usage__details">
            <span class="ch-oauth-usage__percent">${escapeChannelRefreshText(compactRemaining)}</span>
            ${resetAt ? `<span class="ch-oauth-usage__reset">${escapeChannelRefreshText(resetAt)}</span>` : ''}
          </span>
          <span id="${escapeChannelRefreshText(tooltipID)}" class="ch-oauth-usage__tooltip" role="tooltip">
            ${detailLines.map(line => `<span class="ch-oauth-usage__tooltip-line">${escapeChannelRefreshText(line)}</span>`).join('')}
          </span>
        </span>
      </div>
      <div class="ch-oauth-usage__track" role="progressbar" aria-label="${escapeChannelRefreshText(ariaLabel)}" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${escapeChannelRefreshText(percent)}">
        <span class="ch-oauth-usage__fill ch-oauth-usage__fill--${oauthUsageLevel(remaining)}" style="width:${remaining}%"></span>
      </div>
    </div>`;
  });
  const notice = isCursor
    ? formatCursorUsageNotice(state.data?.display_message)
    : isZed
      ? formatZedUsageNotice(state.data)
      : String(state.data?.display_message || '').trim();
  const warnings = Array.isArray(state.data?.warnings)
    ? state.data.warnings.filter(Boolean).map(warning => `<li>${escapeChannelRefreshText(warning)}</li>`).join('')
    : '';
  return `<div class="ch-oauth-usage">
    <div class="ch-oauth-usage__toolbar">${buildOAuthUsageRefreshButton(channel.id, false, state.reset_status === 'loading')}</div>
    ${rows.join('')}
    ${isCodex ? buildCodexResetCreditsHtml(state.data, state, channel.id) : ''}
    ${notice ? `<div class="ch-oauth-usage__notice" role="status">${escapeChannelRefreshText(notice)}</div>` : ''}
    ${warnings ? `<div role="status"><span>${escapeChannelRefreshText(window.t('channels.oauth.usageWarnings'))}</span><ul>${warnings}</ul></div>` : ''}
  </div>`;
}

const MANAGEMENT_ACCOUNT_CHECKIN_STATUSES = [
  'success', 'already_checked', 'manual_required',
  'unsupported', 'credential_invalid', 'credential_forbidden', 'uncertain', 'skipped_disabled'
];

function buildManagementActionButton(action, channelID, labelKey, loadingKey, loading) {
  const text = loading ? window.t(loadingKey) : window.t(labelKey);
  return `<button type="button" class="ch-management__action channel-action-btn" data-action="${action}" data-channel-id="${channelID}"${loading ? ' disabled aria-busy="true"' : ''}>${escapeChannelRefreshText(text)}</button>`;
}

function formatManagementAmount(value, unit) {
  const amount = Number(value);
  if (!Number.isFinite(amount)) return '';
  const normalizedUnit = String(unit || 'USD').trim().toUpperCase();
  const text = amount.toFixed(2);
  return normalizedUnit === 'USD' ? `$${text}` : `${text} ${normalizedUnit}`;
}

function formatManagementCheckinStatus(status) {
  const value = String(status || '').trim();
  if (!value) return '';
  return MANAGEMENT_ACCOUNT_CHECKIN_STATUSES.includes(value)
    ? window.t(`channels.management.status.${value}`)
    : value;
}

// 只有上游同时给出已用、总额和可用百分比才画进度条,缺失用量只展示剩余额度。
function buildManagementUsageBar(usage) {
  const percent = Number(usage?.percent);
  const used = String(usage?.used || '').trim();
  const total = String(usage?.total || '').trim();
  if (!Number.isFinite(percent) || !used || !total) return '';
  const clamped = Math.min(100, Math.max(0, percent));
  const percentText = formatOAuthUsagePercent(clamped);
  const usageText = window.t('channels.management.usage', { used, total });
  const availableText = window.t('channels.management.available', { percent: percentText });
  return `<div class="ch-management__usage">
    <div class="ch-management__usage-meta">
      <span class="ch-management__usage-text" title="${escapeChannelRefreshText(usageText)}">${escapeChannelRefreshText(usageText)}</span>
      <span class="ch-management__usage-percent">${escapeChannelRefreshText(availableText)}</span>
    </div>
    <div class="ch-management__track" role="progressbar" aria-label="${escapeChannelRefreshText(availableText)}" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${escapeChannelRefreshText(percentText)}">
      <span class="ch-management__fill ch-management__fill--${oauthUsageLevel(clamped)}" style="width:${clamped}%"></span>
    </div>
  </div>`;
}

function buildManagementSubscriptionRows(subscriptions, unit) {
  const entries = Array.isArray(subscriptions) ? subscriptions : [];
  return entries.map(entry => {
    const name = String(entry?.name || '').trim();
    const windowName = String(entry?.window || '').trim();
    const label = [name, windowName].filter(Boolean).join(' · ') || window.t('channels.management.subscription');
    const used = formatManagementAmount(entry?.used_usd, unit);
    const total = formatManagementAmount(entry?.limit_usd, unit);
    const bar = buildManagementUsageBar({ percent: entry?.available_percent, used, total });
    const detail = bar || !used || !total
      ? ''
      : `<span class="ch-management__usage-text">${escapeChannelRefreshText(window.t('channels.management.usage', { used, total }))}</span>`;
    if (!bar && !detail) return '';
    return `<div class="ch-management__subscription">
      <span class="ch-management__label" title="${escapeChannelRefreshText(label)}">${escapeChannelRefreshText(label)}</span>
      ${bar}${detail}
    </div>`;
  }).filter(Boolean).join('');
}

function buildManagementBalanceHtml(balance) {
  const unit = balance?.unit;
  const remaining = formatManagementAmount(balance?.remaining, unit);
  const sampledAt = formatXAIUsageReset(balance?.sampled_at);
  const usageBar = buildManagementUsageBar({
    percent: balance?.available_percent,
    used: formatManagementAmount(balance?.used, unit),
    total: formatManagementAmount(balance?.total, unit)
  });
  const subscriptions = buildManagementSubscriptionRows(balance?.subscriptions, unit);
  if (!remaining && !usageBar && !subscriptions) return '';
  const balanceSummary = [
    sampledAt ? `<span class="ch-management__sampled">${escapeChannelRefreshText(window.t('channels.management.sampledAt', { time: sampledAt }))}</span>` : '',
    remaining ? `<div class="ch-management__meta ch-management__meta--remaining">
      <span class="ch-management__label">${escapeChannelRefreshText(window.t('channels.management.remaining'))}</span>
      <span class="ch-management__amount">${escapeChannelRefreshText(remaining)}</span>
    </div>` : ''
  ].filter(Boolean).join('');
  return `<div class="ch-management__balance">
    ${balanceSummary ? `<div class="ch-management__summary">${balanceSummary}</div>` : ''}
    ${usageBar}${subscriptions}
  </div>`;
}

function buildManagementAccountStatusHtml(channel) {
  const account = channel?.management_account;
  const profile = String(account?.profile || '').trim();
  if (channel?.auth_type !== 'api_key' || !profile || account?.credential_configured !== true) return '';
  if (typeof isTokenChannelsReadOnly === 'function' && isTokenChannelsReadOnly()) return '';

  // 额度与签到各自读取独立状态:一个 loading 或失败不会禁用另一个。
  const balanceState = typeof getManagementBalanceState === 'function' ? getManagementBalanceState(channel.id) : null;
  const checkinState = typeof getManagementCheckinState === 'function' ? getManagementCheckinState(channel.id) : null;
  const supportsCheckin = (
    typeof managementSupportsCheckin === 'function' &&
    managementSupportsCheckin(profile)
  );

  // 本次签到结果优先;没有进行中的签到时回落到持久化的最近一次结果。
  const liveStatus = checkinState?.status === 'ready' ? String(checkinState.data?.status || '').trim() : '';
  const savedStatus = String(account?.last_checkin_status || '').trim();
  const checkinStatus = liveStatus || savedStatus;
  const statusText = formatManagementCheckinStatus(checkinStatus);
  const isCompletedCheckin = checkinStatus === 'success' || checkinStatus === 'already_checked';
  const checkedInAt = isCompletedCheckin
    ? formatXAIUsageReset(liveStatus ? checkinState.data?.checked_in_at : account?.last_checkin_at)
    : '';
  const reward = liveStatus ? Number(checkinState.data?.reward) : NaN;
  const rewardText = Number.isFinite(reward) && reward > 0
    ? window.t('channels.management.reward', { amount: formatManagementAmount(reward, balanceState?.data?.balance?.unit) })
    : '';

  const buttons = [buildManagementActionButton(
    'refresh-management-balance', channel.id,
    'channels.management.refreshBalance', 'channels.management.refreshingBalance',
    balanceState?.status === 'loading'
  )];
  if (supportsCheckin) {
    buttons.push(buildManagementActionButton(
      'run-management-checkin', channel.id,
      'channels.management.checkin', 'channels.management.checkinRunning',
      checkinState?.status === 'loading'
    ));
  }

  if (checkedInAt) {
    buttons.push(`<span class="ch-management__checkin-time" role="status">${escapeChannelRefreshText(checkedInAt)}</span>`);
  }

  const balanceError = balanceState?.status === 'error' ? String(balanceState.error || '').trim() : '';
  const checkinError = checkinState?.status === 'error' ? String(checkinState.error || '').trim() : '';
  const balanceData = balanceState?.status === 'ready'
    ? balanceState.data?.balance
    : (balanceState ? null : account?.balance);
  const balanceBody = buildManagementBalanceHtml(balanceData);

  const checkinSummary = checkinStatus === 'already_checked'
    || checkinStatus === 'skipped_disabled'
    ? ''
    : [statusText, rewardText].filter(Boolean).join(' · ');

  return `<div class="ch-management">
    <div class="ch-management__toolbar">${buttons.join('')}</div>
    ${balanceError ? `<div class="ch-management__error" role="status" title="${escapeChannelRefreshText(balanceError)}">${escapeChannelRefreshText(balanceError)}</div>` : ''}
    ${balanceBody}
    ${checkinSummary ? `<div class="ch-management__checkin" role="status">${escapeChannelRefreshText(checkinSummary)}</div>` : ''}
    ${checkinError ? `<div class="ch-management__error" role="status" title="${escapeChannelRefreshText(checkinError)}">${escapeChannelRefreshText(checkinError)}</div>` : ''}
  </div>`;
}

function buildChannelRuntimeStatusHtml(channel) {
  const statuses = [];
  const channelCooldownMS = Number(channel.cooldown_remaining_ms || 0);
  if (channelCooldownMS > 0) {
    const text = window.t('channels.status.channelCooldown', { time: formatCooldownRecoveryTime(channelCooldownMS) });
    statuses.push(`<div class="ch-runtime-status ch-runtime-status--channel">${escapeChannelRefreshText(text)}</div>`);
  }

  const coolingKeys = (Array.isArray(channel.key_cooldowns) ? channel.key_cooldowns : [])
    .map(key => Number(key?.cooldown_remaining_ms || 0))
    .filter(remainingMS => remainingMS > 0);
  if (coolingKeys.length > 0) {
    const nextRecoveryMS = Math.min(...coolingKeys);
    const text = window.t('channels.status.keyCooldowns', {
      count: coolingKeys.length,
      time: formatCooldownRecoveryTime(nextRecoveryMS)
    });
    const label = window.t('channels.status.viewKeyCooldowns', { count: coolingKeys.length });
    statuses.push(`<button type="button" class="ch-runtime-status ch-runtime-status--keys channel-action-btn" data-action="edit-cooling-keys" data-channel-id="${channel.id}" aria-label="${escapeChannelRefreshText(label)}">${escapeChannelRefreshText(text)}</button>`);
  }

  const coolingModels = (Array.isArray(channel.model_cooldowns) ? channel.model_cooldowns : [])
    .map(model => Number(model?.cooldown_remaining_ms || 0))
    .filter(remainingMS => remainingMS > 0);
  if (coolingModels.length > 0) {
    const nextRecoveryMS = Math.min(...coolingModels);
    const text = window.t('channels.status.modelCooldowns', {
      count: coolingModels.length,
      time: formatCooldownRecoveryTime(nextRecoveryMS, 'channels.status.daysHoursUntilRecovery')
    });
    statuses.push(`<div class="ch-runtime-status ch-runtime-status--models">${escapeChannelRefreshText(text)}</div>`);
  }

  const protocolProbeRetryCount = Number(channel.protocol_probe_retry_count || 0);
  const protocolProbeRetryRemainingMS = Number(channel.protocol_probe_retry_remaining_ms || 0);
  if (protocolProbeRetryCount > 0 && protocolProbeRetryRemainingMS > 0) {
    const text = window.t('channels.status.protocolProbeRetries', {
      count: protocolProbeRetryCount,
      time: formatProtocolProbeRetryTime(protocolProbeRetryRemainingMS)
    });
    statuses.push(`<div class="ch-runtime-status ch-runtime-status--protocols">${escapeChannelRefreshText(text)}</div>`);
  }

  const oauthUsageHtml = buildOAuthUsageStatusHtml(channel);
  if (oauthUsageHtml) statuses.push(oauthUsageHtml);

  const managementHtml = buildManagementAccountStatusHtml(channel);
  if (managementHtml) statuses.push(managementHtml);

  return statuses.length > 0
    ? `<div class="ch-runtime-status-list">${statuses.join('')}</div>`
    : '';
}

/**
 * 使用模板引擎创建渠道表格行
 * @param {Object} channel - 渠道数据
 * @returns {HTMLElement|null} 行元素
 */
function createChannelCard(channel) {
  const isCooldown = channel.cooldown_remaining_ms > 0;
  const configuredProtocols = new Set(
    (Array.isArray(channel.urls) ? channel.urls : [])
      .flatMap(entry => Array.isArray(entry?.protocols) ? entry.protocols : [])
      .map(protocol => String(protocol || '').trim().toLowerCase())
  );
  const hasAutoProtocolURL = (Array.isArray(channel.urls) ? channel.urls : [])
    .some(entry => !Array.isArray(entry?.protocols) || entry.protocols.length === 0);
  const stats = channelStatsById[channel.id] || null;
  const batchRefreshResult = getBatchRefreshResult(channel.id);

  // 预计算统计数据
  const statsCache = stats ? {
    inputTokensText: formatMetricNumber(stats.totalInputTokens),
    outputTokensText: formatMetricNumber(stats.totalOutputTokens),
    cacheReadText: formatMetricNumber(stats.totalCacheReadInputTokens),
    cacheCreationTokens: stats.totalCacheCreationInputTokens || 0,
    cacheCreationText: formatMetricNumber(stats.totalCacheCreationInputTokens)
  } : null;

  // 模型文本
  const modelsText = Array.isArray(channel.models)
    ? channel.models.map(m => m.model || m).join(', ')
    : '';

  const durationHtml = buildChannelTimingHtml(stats);
  const runtimeStatusHtml = buildChannelRuntimeStatusHtml(channel);
  const lastRequestFailureHtml = buildChannelLastRequestFailureHtml(stats);

  // 消耗HTML：token 与成本统一放在同一列
  let usageHtml = '';
  if (stats && statsCache) {
    const parts = [];
    parts.push(`<div class="ch-usage-row"><span class="ch-usage-label">${window.t('channels.stats.input')}</span><span class="ch-usage-value" style="color: var(--warning-500);">${statsCache.inputTokensText}</span></div>`);
    parts.push(`<div class="ch-usage-row"><span class="ch-usage-label">${window.t('channels.stats.output')}</span><span class="ch-usage-value" style="color: var(--warning-500);">${statsCache.outputTokensText}</span></div>`);
    const supportsCaching = hasAutoProtocolURL || configuredProtocols.has('anthropic') || configuredProtocols.has('codex');
    if (supportsCaching) {
      parts.push(`<div class="ch-usage-row"><span class="ch-usage-label">${window.t('channels.stats.cacheRead')}</span><span class="ch-usage-value" style="color: var(--success-500);">${statsCache.cacheReadText}</span></div>`);
      if (statsCache.cacheCreationTokens > 0) {
        parts.push(`<div class="ch-usage-row"><span class="ch-usage-label">${window.t('channels.stats.cacheCreate')}</span><span class="ch-usage-value" style="color: var(--primary-500);">${statsCache.cacheCreationText}</span></div>`);
      }
    }
    const costHtml = buildCostStackHtml(stats.totalCost, stats.effectiveCost, {
      tone: 'warning',
      decimalPlaces: 2,
      inline: true
    });
    if (costHtml) {
      parts.push(`<div class="ch-usage-row ch-usage-cost-row" title="${escapeChannelRefreshText(window.t('channels.stats.cost'))}">${costHtml}</div>`);
    }
    usageHtml = `<div class="ch-usage-list">${parts.join('')}</div>`;
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
  if (batchRefreshResult && batchRefreshResult.status) {
    rowClasses.push(`channel-row-refresh-${batchRefreshResult.status}`);
  }

  // 准备模板数据
  const configuredURLs = (Array.isArray(channel.urls) ? channel.urls : [])
    .map(entry => {
      const url = String(entry?.url || '').trim();
      return url && entry?.exact ? `${url}#` : url;
    })
    .filter(Boolean);

  const cardData = {
    rowClasses: rowClasses.join(' '),
    id: channel.id,
		name: channel.name,
		nameMultiplierBadge: buildCornerMultiplierBadge(channel.cost_multiplier_min, channel.cost_multiplier_max),
    oauthPlanBadge: buildOAuthPlanBadge(channel),
    url: configuredURLs.join('\n'),
    batchRefreshStatusHtml: buildBatchRefreshStatusHtml(batchRefreshResult),
    modelsText: modelsText,
    priority: channel.priority,
    effectivePriorityHtml: buildEffectivePriorityHtml(channel),
    durationHtml: durationHtml,
    usageHtml: usageHtml,
    runtimeStatusHtml: runtimeStatusHtml,
    lastRequestFailureHtml: lastRequestFailureHtml,
    healthHtml: healthHtml,
    enabled: channel.enabled,
    toggleTitle: channel.enabled ? window.t('channels.toggleDisable') : window.t('channels.toggleEnable'),
    toggleSwitchClass: channel.enabled ? 'channel-enable-switch--on' : 'channel-enable-switch--off',
    durationCellClass: durationHtml ? '' : 'ch-mobile-empty',
    usageCellClass: usageHtml ? '' : 'ch-mobile-empty',
    lastSuccessCellClass: runtimeStatusHtml ? '' : 'ch-mobile-empty',
    mobileLabelModels: window.t('channels.table.models'),
    mobileLabelPriority: window.t('channels.table.priority'),
    mobileLabelDuration: window.t('channels.table.duration'),
    mobileLabelUsage: window.t('channels.table.usage'),
    mobileLabelLastSuccess: window.t('common.status'),
    mobileLabelEnabled: window.t('channels.table.enabled'),
    mobileLabelActions: window.t('channels.table.actions')
  };

  const card = TemplateEngine.render('tpl-channel-card', cardData);
  return card;
}

async function editChannelCoolingKeys(channelId) {
  await editChannel(channelId);
  if (editingChannelId !== channelId) return;

  const filter = document.getElementById('keyStatusFilter');
  if (filter) filter.value = 'cooldown';
  filterKeysByStatus('cooldown');
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
    if (!input || isTokenChannelsReadOnly()) return;
    queueInlineChannelPrioritySave(input);
  });

  container.addEventListener('keydown', (e) => {
    const input = e.target.closest('.ch-priority-input');
    if (!input || isTokenChannelsReadOnly()) return;
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
    if (!input || isTokenChannelsReadOnly()) return;
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
    if (isTokenChannelsReadOnly() && ['edit', 'edit-cooling-keys', 'refresh-oauth-usage', 'reset-codex-quota', 'refresh-management-balance', 'run-management-checkin', 'test', 'copy', 'delete', 'toggle'].includes(action)) {
      return;
    }
    const channelId = parseInt(btn.dataset.channelId);
    const channelName = btn.dataset.channelName;
    const enabled = btn.dataset.enabled === 'true';

    switch (action) {
      case 'edit':
        editChannel(channelId);
        break;
      case 'edit-cooling-keys':
        editChannelCoolingKeys(channelId);
        break;
      case 'refresh-oauth-usage':
        if (typeof refreshOAuthUsage === 'function') {
          refreshOAuthUsage(channelId).catch(error => {
            if (window.showError) window.showError(error?.message || window.t('channels.oauth.usageFailed'));
          });
        }
        break;
      case 'refresh-management-balance':
        if (typeof refreshManagementBalance === 'function') {
          refreshManagementBalance(channelId).catch(error => {
            if (window.showError) window.showError(error?.message || window.t('channels.management.balanceFailed'));
          });
        }
        break;
      case 'run-management-checkin':
        if (typeof runManagementCheckin === 'function') {
          runManagementCheckin(channelId).catch(error => {
            if (window.showError) window.showError(error?.message || window.t('channels.management.checkinFailed'));
          });
        }
        break;
      case 'reset-codex-quota':
        if (typeof resetCodexQuota === 'function') {
          const count = Math.max(0, Number(btn.dataset.resetCount) || 0);
          const expiry = btn.dataset.resetExpiry || window.t('channels.oauth.resetCreditExpiresUnknown');
          const confirmed = window.confirm(window.t('channels.oauth.resetConfirm', { count, time: expiry }));
          if (!confirmed) break;
          resetCodexQuota(channelId).then(result => {
            const hasWarnings = Array.isArray(result?.warnings) && result.warnings.length > 0;
            const message = !result?.usage || hasWarnings
              ? window.t('channels.oauth.resetSuccessNeedsRefresh')
              : window.t('channels.oauth.resetSuccess');
            if (window.showSuccess) window.showSuccess(message);
          }).catch(error => {
            if (window.showError) window.showError(error?.message || window.t('channels.oauth.resetFailed'));
          });
        }
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
      <th class="ch-col-last-success">${window.t('common.status')}</th>
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

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    buildChannelRuntimeStatusHtml,
    buildOAuthPlanBadge,
    buildOAuthUsageStatusHtml,
    buildManagementAccountStatusHtml,
    formatCooldownRecoveryTime
  };
}
