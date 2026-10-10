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

function channelModelDisplayGroups(models) {
  const groups = new Map();
  (Array.isArray(models) ? models : []).forEach((entry) => {
    const name = String(entry?.model || entry || '').trim();
    if (!name) return;
    let group = groups.get(name);
    if (!group) {
      group = { model: name, targets: [] };
      groups.set(name, group);
    }
    const redirect = String(entry?.redirect_model || '').trim();
    const target = redirect || name;
    if (!group.targets.includes(target)) group.targets.push(target);
  });
  return [...groups.values()];
}

function formatChannelModelLabel(group) {
  const targets = group.targets.filter((target) => target !== group.model);
  if (targets.length === 0) return group.model;
  const visible = targets.slice(0, 2).join(', ');
  const suffix = targets.length > 2 ? ', ...' : '';
  return `${group.model}(${visible}${suffix})`;
}

// 可见数量由实际行宽决定（fitChannelModelLine）；渲染上限只为控制 DOM 规模，再多一行也放不下。
const CHANNEL_MODEL_CHIP_RENDER_CAP = 40;

// 列表只占一行：放得下的模型做标签，其余折成 +N，完整列表放在 title。
function buildChannelModelLineHtml(models) {
  const groups = channelModelDisplayGroups(models);
  if (groups.length === 0) return '';
  const chips = groups.slice(0, CHANNEL_MODEL_CHIP_RENDER_CAP)
    .map(group => `<span class="ch-model-chip">${escapeChannelRefreshText(formatChannelModelLabel(group))}</span>`);
  const rest = groups.length - chips.length;
  chips.push(`<span class="ch-model-chip ch-model-chip--more"${rest > 0 ? '' : ' hidden'}>+${rest}</span>`);
  return `<div class="ch-model-line" data-total="${groups.length}" title="${escapeChannelRefreshText(formatChannelModelTitle(models))}">${chips.join('')}</div>`;
}

// scrollWidth 是内容自然宽度，不受 flex 收缩影响；再补上边框
function channelModelChipNaturalWidth(chip) {
  return chip.scrollWidth + chip.offsetWidth - chip.clientWidth;
}

// 按行宽决定可见标签数：至少保留第一个（过长时由 CSS 省略），其余折成 +N。
// 所有行先统一读、再统一写：逐行读写交替会让每行触发两次整页强制布局。
function fitChannelModelLineList(lines) {
  const items = lines.map(line => ({
    line,
    available: line.clientWidth,
    more: line.querySelector('.ch-model-chip--more'),
    chips: Array.from(line.querySelectorAll('.ch-model-chip:not(.ch-model-chip--more)'))
  })).filter(item => item.available > 0 && item.more);

  items.forEach(item => {
    item.total = Number(item.line.dataset.total) || item.chips.length;
    item.chips.forEach(chip => { chip.hidden = false; });
    item.more.hidden = false;
    item.more.textContent = `+${item.total}`;
  });

  items.forEach(item => {
    const gap = parseFloat(getComputedStyle(item.line).columnGap) || 0;
    const moreWidth = channelModelChipNaturalWidth(item.more);
    let visible = 0;
    let used = 0;
    for (const chip of item.chips) {
      const next = used + (visible > 0 ? gap : 0) + channelModelChipNaturalWidth(chip);
      const reserve = visible + 1 < item.total ? gap + moreWidth : 0;
      if (visible > 0 && next + reserve > item.available) break;
      used = next;
      visible++;
    }
    item.visible = visible;
  });

  items.forEach(({ chips, more, total, visible }) => {
    chips.forEach((chip, index) => { chip.hidden = index >= visible; });
    more.hidden = visible >= total;
    more.textContent = `+${total - visible}`;
  });
}

let channelModelLineObserver = null;

// 容器宽度变化时重新计算；只在宽度变化时触发，避免高度抖动引起循环
function fitChannelModelLines(container) {
  fitChannelModelLineList(Array.from(container.querySelectorAll('.ch-model-line')));
  if (channelModelLineObserver || typeof ResizeObserver !== 'function') return;
  let lastWidth = container.clientWidth;
  let frame = 0;
  channelModelLineObserver = new ResizeObserver(() => {
    if (container.clientWidth === lastWidth) return;
    lastWidth = container.clientWidth;
    cancelAnimationFrame(frame);
    frame = requestAnimationFrame(() => {
      fitChannelModelLineList(Array.from(container.querySelectorAll('.ch-model-line')));
    });
  });
  channelModelLineObserver.observe(container);
}

function formatChannelModelTitle(models) {
  return channelModelDisplayGroups(models).map((group) => {
    const targets = group.targets.filter((target) => target !== group.model);
    return targets.length > 0 ? `${group.model}(${targets.join(', ')})` : group.model;
  }).join(', ');
}

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

const OAUTH_USAGE_AUTH_TYPES = [
  'codex_oauth',
  'antigravity_oauth',
  'xai_oauth',
  'anthropic_oauth',
  'zai_oauth',
  'cursor_oauth',
  'zed_oauth',
  'codebuddy_oauth'
];

function isOpenCodeGoEndpoint(raw) {
  const text = String(raw || '').trim().replace(/#$/, '');
  if (!text) return false;
  let parsed;
  try {
    parsed = new URL(text);
  } catch (_) {
    return false;
  }
  if (parsed.protocol !== 'https:' || parsed.hostname !== 'opencode.ai' || parsed.port || parsed.username || parsed.password || parsed.hash) {
    return false;
  }
  const path = parsed.pathname || '';
  if (path.split('/').some(segment => segment === '.' || segment === '..')) return false;
  return path === '/zen/go' || path.startsWith('/zen/go/');
}

function isOpenCodeGoChannel(channel) {
  const entries = Array.isArray(channel?.urls) ? channel.urls : [];
  return entries.some(entry => isOpenCodeGoEndpoint(typeof entry === 'string' ? entry : entry?.url));
}

function channelShowsOAuthUsage(channel) {
  return OAUTH_USAGE_AUTH_TYPES.includes(channel?.auth_type) || isOpenCodeGoChannel(channel);
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
      <div class="ch-priority-hint" aria-hidden="true">${escapeChannelRefreshText(window.t('channels.priorityEditHint'))}</div>
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

const CHANNEL_METRIC_ICONS = {
  clock: '<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></svg>',
  bolt: '<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="M13 2 3 14h9l-1 8 10-12h-9z"/></svg>',
  check: '<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><path d="m5 12 5 5L20 7"/></svg>'
};

function channelSuccessRateLevel(rate) {
  if (rate >= 0.95) return 'good';
  if (rate >= 0.8) return 'warn';
  return 'bad';
}

/**
 * 渠道名下方的指标行：首字/总耗时、速度、成功/失败·成功率、消耗。
 * 统计窗口内没有调用时整行不渲染；omitCalls 用于成功率已单独成条的场景。
 */
function buildChannelMetricsHtml(stats, { omitCalls = false } = {}) {
  if (!stats) return '';
  const successCount = Number.isFinite(Number(stats.success)) ? Number(stats.success) : 0;
  const failureCount = Number.isFinite(Number(stats.error)) ? Number(stats.error) : 0;
  const total = successCount + failureCount;
  if (total <= 0) return '';

  const items = [];
  const seconds = window.t('common.seconds');
  const avgFirstByte = stats.avgFirstByteTimeSeconds || 0;
  const avgDuration = stats.avgDurationSeconds || 0;
  if (avgFirstByte > 0 || avgDuration > 0) {
    const values = [];
    if (avgFirstByte > 0) {
      values.push(`<b style="color: ${window.getFirstByteTimingColor(avgFirstByte)};">${avgFirstByte.toFixed(2)}${seconds}</b>`);
    }
    if (avgDuration > 0) {
      values.push(`<b style="color: ${window.getDurationTimingColor(avgDuration)};">${avgDuration.toFixed(2)}${seconds}</b>`);
    }
    const label = avgFirstByte > 0 ? window.t('channels.stats.firstByte') : window.t('stats.tooltipDuration');
    items.push(`<span class="ch-metric" title="${escapeChannelRefreshText(window.t('channels.stats.timingTitle'))}">${CHANNEL_METRIC_ICONS.clock}${escapeChannelRefreshText(label)} ${values.join(' / ')}</span>`);
  }
  if (Number.isFinite(stats.outputTokensPerSecond) && stats.outputTokensPerSecond > 0) {
    items.push(`<span class="ch-metric" title="${escapeChannelRefreshText(window.t('channels.stats.speed'))}">${CHANNEL_METRIC_ICONS.bolt}<b>${stats.outputTokensPerSecond.toFixed(1)}</b> tok/s</span>`);
  }

  const rate = successCount / total;
  if (!omitCalls) items.push(`<span class="ch-metric" title="${escapeChannelRefreshText(window.t('channels.stats.callsTitle'))}">${CHANNEL_METRIC_ICONS.check}<b class="ch-metric__ok">${successCount}</b> / <b class="ch-metric__err">${failureCount}</b> ${escapeChannelRefreshText(window.t('stats.unitTimes'))} · <b class="ch-metric__rate--${channelSuccessRateLevel(rate)}">${(rate * 100).toFixed(1)}%</b></span>`);

  // 缓存行按实际数据显示：协议声明无法判定渠道是否缓存，CodeBuddy 等上游同样声明 openai 且返回缓读。
  const usageLines = [
    `${window.t('channels.stats.input')} ${formatMetricNumber(stats.totalInputTokens)}`,
    `${window.t('channels.stats.output')} ${formatMetricNumber(stats.totalOutputTokens)}`
  ];
  if (stats.totalCacheReadInputTokens > 0) {
    usageLines.push(`${window.t('channels.stats.cacheRead')} ${formatMetricNumber(stats.totalCacheReadInputTokens)}`);
  }
  if (stats.totalCacheCreationInputTokens > 0) {
    usageLines.push(`${window.t('channels.stats.cacheCreate')} ${formatMetricNumber(stats.totalCacheCreationInputTokens)}`);
  }
  const costHtml = buildCostStackHtml(stats.totalCost, stats.effectiveCost, {
    tone: 'warning',
    decimalPlaces: 2,
    inline: true
  });
  const tokenTotal = (Number(stats.totalInputTokens) || 0) + (Number(stats.totalOutputTokens) || 0);
  const usageValue = costHtml || `<b>${formatMetricNumber(tokenTotal)}</b> tok`;
  items.push(`<span class="ch-metric ch-metric--usage" title="${escapeChannelRefreshText(usageLines.join(' · '))}">${usageValue}</span>`);

  return `<div class="ch-metrics">${items.join('')}</div>`;
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
  if (!Number.isFinite(microUSD) || microUSD <= 0) return '';
  return `$${(microUSD / 1_000_000).toFixed(1)}`;
}

function formatOAuthEstimatedTotalCost(standardCostMicroUSD, remainingPercent) {
  const microUSD = Number(standardCostMicroUSD);
  const remaining = Number(remainingPercent);
  if (!Number.isFinite(microUSD) || microUSD <= 0 || !Number.isFinite(remaining) || remaining >= 100) {
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
  if (normalized === 'five_hour' || normalized === 'weekly' || normalized === 'monthly' || normalized === 'rolling') return '';
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

function buildCodeBuddyCheckinButton(channelID, state = {}) {
  const loading = state?.checkin_status === 'loading';
  const disabled = state?.status === 'loading' || state?.reset_status === 'loading';
  const text = loading
    ? window.t('channels.codebuddy.checkinRunning')
    : window.t('channels.codebuddy.checkin');
  return `<button type="button" class="ch-oauth-usage__refresh channel-action-btn" data-action="checkin-codebuddy" data-channel-id="${channelID}"${loading || disabled ? ' disabled' : ''}${loading ? ' aria-busy="true"' : ''}>${escapeChannelRefreshText(text)}</button>`;
}

function buildOAuthUsageToolbar(channel, state = {}, usageLoading = false) {
  const checkinLoading = state?.checkin_status === 'loading';
  const buttons = [buildOAuthUsageRefreshButton(
    channel.id,
    usageLoading,
    checkinLoading || state?.reset_status === 'loading'
  )];
  if (channel?.auth_type === 'codebuddy_oauth' && !channel?.codebuddy_enterprise && !channel?.codebuddy_international) {
    buttons.push(buildCodeBuddyCheckinButton(channel.id, state));
  }
  return `<div class="ch-oauth-usage__toolbar">${buttons.join('')}</div>`;
}

const OAUTH_RESET_CREDIT_ICON = '<svg class="ch-oauth-usage__credit-icon" width="14" height="14" viewBox="0 0 16 16" fill="none" aria-hidden="true" focusable="false"><rect x="1.75" y="3.25" width="12.5" height="9.5" rx="1.75" stroke="currentColor" stroke-width="1.3"/><path d="M1.75 6.25H14.25M4.5 10H7" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/></svg>';

function buildAnthropicResetCreditsHtml(channelID) {
  const state = typeof getAnthropicResetCreditsState === 'function'
    ? getAnthropicResetCreditsState(channelID) : null;
  const loading = state?.status === 'loading';
  const resetting = state?.reset_status === 'loading';
  const message = state?.reset_error || state?.reset_feedback;
  const feedback = message ? `<div class="ch-oauth-usage__credits-summary" role="${state?.reset_status === 'reset' ? 'status' : 'alert'}">${escapeChannelRefreshText(message)}</div>` : '';
  if (state?.status !== 'ready') {
    if (state?.status !== 'error' && !feedback) return '';
    return `<div class="ch-oauth-usage__credits">${state?.status === 'error' ? `<div class="ch-oauth-usage__error" role="alert">${escapeChannelRefreshText(state.error)}</div>` : ''}${feedback}</div>`;
  }
  const data = state.data;
  const credits = data.credits.filter(credit => credit && typeof credit === 'object' &&
    (!credit.expires_at || Date.parse(credit.expires_at) > Date.now()));
  const total = credits.reduce((sum, credit) => sum + Math.max(0, Number(credit.resets_left) || 0), 0);
  if (total <= 0) return feedback ? `<div class="ch-oauth-usage__credits">${feedback}</div>` : '';
  const resetLabel = resetting ? 'resettingQuota' : 'resetQuota';
  const resetButton = `<button type="button" class="ch-oauth-usage__reset-action channel-action-btn" data-action="reset-anthropic-quota" data-channel-id="${channelID}"${loading || resetting ? ' disabled' : ''}${resetting ? ' aria-busy="true"' : ''}>${escapeChannelRefreshText(window.t(`channels.oauth.${resetLabel}`))}</button>`;
  const expiry = credit => {
    const timestamp = Date.parse(String(credit.expires_at || ''));
    return Number.isFinite(timestamp) ? timestamp : Infinity;
  };
  const primary = credits.find(credit => credit.redeemable) ||
    [...credits].sort((left, right) => expiry(left) - expiry(right))[0];
  const available = data.eligible && data.available_count > 0;
  const status = !data.eligible ? 'channels.oauth.anthropicResetIneligible'
    : available ? ''
      : 'channels.oauth.anthropicResetUnavailable';
  const expiryText = primary?.expires_at
    ? window.t('channels.oauth.resetCreditExpires', { time: formatXAIUsageReset(primary.expires_at) })
    : window.t('channels.oauth.resetCreditExpiresUnknown');
  const details = credits.map(credit => {
    const time = credit.expires_at ? formatXAIUsageReset(credit.expires_at) : '';
    return `${String(credit.label || '')}: ${Math.max(0, Number(credit.resets_left) || 0)}${time ? ` · ${window.t('channels.oauth.resetCreditExpires', { time })}` : ''}`;
  }).join('\n');
  const cooldown = data.cooldown_until && Date.parse(data.cooldown_until) > Date.now()
    ? formatXAIUsageReset(data.cooldown_until) : '';
  return `<div class="ch-oauth-usage__credits ch-oauth-usage__credits--reset" role="status">
    <div class="ch-oauth-usage__credits-summary">
      ${OAUTH_RESET_CREDIT_ICON}
      <span class="ch-oauth-usage__credit-count">${escapeChannelRefreshText(window.t('channels.oauth.resetCredits', { count: total }))}</span>
      <span class="ch-oauth-usage__credit-expiry" title="${escapeChannelRefreshText(details)}">${escapeChannelRefreshText(expiryText)}</span>
      ${resetButton}
    </div>
    ${status || cooldown ? `<div class="ch-oauth-usage__credits-summary">${[
      status ? escapeChannelRefreshText(window.t(status)) : '',
      cooldown ? escapeChannelRefreshText(window.t('channels.oauth.anthropicResetCooldown', { time: cooldown })) : ''
    ].filter(Boolean).join(' · ')}</div>` : ''}
    ${feedback}
  </div>`;
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
  // 已购 Credit 的累计标准成本与重置次数同属额度信息，合并到同一行
  const creditCost = formatOAuthAccumulatedCost(data?.quota_cost_usage?.credit_standard_cost_microusd);
  const metaText = [
    expiryText,
    creditCost ? window.t('channels.oauth.codexCreditCostShort', { cost: creditCost }) : ''
  ].filter(Boolean).join(' · ');
  const escapedMetaText = escapeChannelRefreshText(metaText);
  return `<div class="ch-oauth-usage__credits ch-oauth-usage__credits--reset">
    <div class="ch-oauth-usage__credits-summary">
      ${OAUTH_RESET_CREDIT_ICON}
      <span class="ch-oauth-usage__credit-count">${escapeChannelRefreshText(window.t('channels.oauth.resetCredits', { count: availableCount }))}</span>
      <span class="ch-oauth-usage__credit-expiry" title="${escapedMetaText}">${escapedMetaText}</span>
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

function buildCodexPurchasedCreditsHtml(data) {
  const cost = formatOAuthAccumulatedCost(data?.quota_cost_usage?.credit_standard_cost_microusd);
  if (!cost) return '';
  const text = window.t('channels.oauth.codexPurchasedCreditCost', { cost });
  return `<div class="ch-oauth-usage__credits"><div class="ch-oauth-usage__credits-summary">${escapeChannelRefreshText(text)}</div></div>`;
}

function buildAntigravityCreditsHtml(credits) {
  if (!credits || typeof credits.balance !== 'number' || !Number.isFinite(credits.balance)) return '';
  const text = window.t('channels.oauth.antigravityCredits', { balance: credits.balance.toLocaleString() });
  return `<div class="ch-oauth-usage__credits"><div class="ch-oauth-usage__credits-summary">${escapeChannelRefreshText(text)}</div></div>`;
}

// 额度窗口行：左侧名称+金额，右侧主值+时间，悬浮展开明细；percent 为 null 时不画进度条。
function buildOAuthUsageWindowHtml({ tooltipID, label, amount, value, time, detailLines, ariaLabel, percent = null, level = '' }) {
  const id = escapeChannelRefreshText(tooltipID);
  let track = '';
  if (percent !== null) {
    const clamped = Math.min(100, Math.max(0, percent));
    track = `<div class="ch-oauth-usage__track" role="progressbar" aria-label="${escapeChannelRefreshText(ariaLabel)}" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${escapeChannelRefreshText(formatOAuthUsagePercent(clamped))}">
        <span class="ch-oauth-usage__fill ch-oauth-usage__fill--${level || oauthUsageLevel(clamped)}" style="width:${clamped}%"></span>
      </div>`;
  }
  return `<div class="ch-oauth-usage__window">
      <div class="ch-oauth-usage__meta">
        <span class="ch-oauth-usage__summary" tabindex="0" aria-describedby="${id}">
          <span class="ch-oauth-usage__heading">
            <span class="ch-oauth-usage__label">${escapeChannelRefreshText(label)}</span>
            ${amount ? `<span class="ch-oauth-usage__amount">${escapeChannelRefreshText(amount)}</span>` : ''}
          </span>
          <span class="ch-oauth-usage__details">
            ${value ? `<span class="ch-oauth-usage__percent">${escapeChannelRefreshText(value)}</span>` : ''}
            ${time ? `<span class="ch-oauth-usage__reset">${escapeChannelRefreshText(time)}</span>` : ''}
          </span>
          <span id="${id}" class="ch-oauth-usage__tooltip" role="tooltip">
            ${detailLines.filter(Boolean).map(line => `<span class="ch-oauth-usage__tooltip-line">${escapeChannelRefreshText(line)}</span>`).join('')}
          </span>
        </span>
      </div>
      ${track}
    </div>`;
}

function oauthUsageTooltipID(prefix, channelID, index) {
  return `${prefix}-${String(channelID).replace(/[^a-zA-Z0-9_-]/g, '')}-${index}`;
}

function buildCodeBuddyCreditsHtml(credits, channelID) {
  const remain = Number(credits?.remain);
  if (!Number.isFinite(remain)) return '';
  const formatCredits = value => value.toLocaleString(undefined, { maximumFractionDigits: 2 });
  const total = credits?.total;
  const used = credits?.used;
  if (!credits?.unlimited && Number.isFinite(total) && Number.isFinite(used)) {
    const label = window.t('channels.codebuddy.credits');
    const percent = total > 0 ? remain / total * 100 : 0;
    const percentText = formatOAuthUsagePercent(Math.min(100, Math.max(0, percent)));
    const usageText = window.t('channels.management.usage', { used: formatCredits(used), total: formatCredits(total) });
    const availableText = window.t('channels.management.available', { percent: percentText });
    return buildOAuthUsageWindowHtml({
      tooltipID: oauthUsageTooltipID('ch-codebuddy-credits-tooltip', channelID, 0),
      label,
      amount: `${formatCredits(used)}/${formatCredits(total)}`,
      value: formatCredits(Math.max(0, remain)),
      detailLines: [label, usageText, availableText],
      ariaLabel: availableText,
      percent
    });
  }
  const text = credits?.unlimited
    ? window.t('channels.codebuddy.unlimitedCredits')
    : window.t('channels.oauth.codeBuddyCredits', { remain: formatCredits(Math.max(0, remain)) });
  return `<div class="ch-oauth-usage__credits"><div class="ch-oauth-usage__credits-summary">${escapeChannelRefreshText(text)}</div></div>`;
}

const OAUTH_USAGE_VISIBLE_WINDOWS = 2;
// 展开状态跨重渲染保留：列表在刷新额度、筛选后整体重建。
const expandedOAuthUsageChannels = new Set();

function oauthUsageWindowsToggleLabel(expanded, hiddenCount) {
  return expanded
    ? window.t('channels.oauthUsageLessWindows')
    : window.t('channels.oauthUsageMoreWindows', { count: hiddenCount });
}

function buildOAuthUsageRowsHtml(channelID, rows) {
  if (rows.length <= OAUTH_USAGE_VISIBLE_WINDOWS) return rows.join('');
  const hiddenCount = rows.length - OAUTH_USAGE_VISIBLE_WINDOWS;
  const expanded = expandedOAuthUsageChannels.has(Number(channelID));
  return `${rows.slice(0, OAUTH_USAGE_VISIBLE_WINDOWS).join('')}
    <div class="ch-oauth-usage__more"${expanded ? '' : ' hidden'}>${rows.slice(OAUTH_USAGE_VISIBLE_WINDOWS).join('')}</div>
    <button type="button" class="ch-oauth-usage__toggle channel-action-btn" data-action="toggle-oauth-usage-windows" data-channel-id="${channelID}" data-hidden-count="${hiddenCount}" aria-expanded="${expanded}">${escapeChannelRefreshText(oauthUsageWindowsToggleLabel(expanded, hiddenCount))}</button>`;
}

function toggleOAuthUsageWindows(btn) {
  const channelID = Number(btn.dataset.channelId);
  const expanded = !expandedOAuthUsageChannels.has(channelID);
  if (expanded) {
    expandedOAuthUsageChannels.add(channelID);
  } else {
    expandedOAuthUsageChannels.delete(channelID);
  }
  const more = btn.parentElement && btn.parentElement.querySelector('.ch-oauth-usage__more');
  if (more) more.hidden = !expanded;
  btn.setAttribute('aria-expanded', String(expanded));
  btn.textContent = oauthUsageWindowsToggleLabel(expanded, Number(btn.dataset.hiddenCount) || 0);
}

function buildOAuthUsageStatusHtml(channel) {
  if (!channelShowsOAuthUsage(channel) ||
      (typeof isTokenChannelsReadOnly === 'function' && isTokenChannelsReadOnly())) {
    return '';
  }
  const liveState = typeof getOAuthUsageState === 'function' ? getOAuthUsageState(channel.id) : null;
  const state = liveState || (channel?.oauth_usage ? { status: 'ready', data: channel.oauth_usage } : null);
  if (!state) {
    return `<div class="ch-oauth-usage">${buildOAuthUsageToolbar(channel)}${channel?.auth_type === 'anthropic_oauth' ? buildAnthropicResetCreditsHtml(channel.id) : ''}</div>`;
  }
  if (state.status === 'loading') {
    return `<div class="ch-oauth-usage">${buildOAuthUsageToolbar(channel, state, true)}${channel?.auth_type === 'anthropic_oauth' ? buildAnthropicResetCreditsHtml(channel.id) : ''}</div>`;
  }
  if (state.status === 'error') {
    const fallback = window.t('channels.oauth.usageFailed');
    const message = formatOAuthUsageError(state.error) || fallback;
    return `<div class="ch-oauth-usage">
      ${buildOAuthUsageToolbar(channel, state)}
      ${channel?.auth_type === 'anthropic_oauth' ? buildAnthropicResetCreditsHtml(channel.id) : ''}
      <div class="ch-oauth-usage__error" title="${escapeChannelRefreshText(message)}">${escapeChannelRefreshText(message)}</div>
    </div>`;
  }

  const windows = Array.isArray(state.data?.windows) ? state.data.windows : [];
  const isXAI = channel?.auth_type === 'xai_oauth' || state.data?.provider === 'xai';
  const isCodex = channel?.auth_type === 'codex_oauth';
  const isCursor = channel?.auth_type === 'cursor_oauth' || state.data?.provider === 'cursor';
  const isZed = channel?.auth_type === 'zed_oauth' || state.data?.provider === 'zed';
  const isCodeBuddy = channel?.auth_type === 'codebuddy_oauth' || state.data?.provider === 'codebuddy';
  const displayedWindows = isCursor
    ? orderCursorUsageWindows(windows)
    : isCodex
      ? orderCodexUsageWindows(windows)
      : windows;
  const rows = isXAI ? buildXAIUsageRows(state.data) : isCodeBuddy ? [] : displayedWindows.map((windowInfo, windowIndex) => {
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
    return buildOAuthUsageWindowHtml({
      tooltipID: oauthUsageTooltipID('ch-oauth-usage-tooltip', channel.id, windowIndex),
      label,
      amount: compactAmount,
      value: compactRemaining,
      time: resetAt,
      detailLines,
      ariaLabel: window.t('channels.oauth.usageRemaining', { label, percent }),
      percent: remaining
    });
  });
  const notice = isCursor
    ? formatCursorUsageNotice(state.data?.display_message)
    : isZed
      ? formatZedUsageNotice(state.data)
      : String(state.data?.display_message || '').trim();
  const warnings = Array.isArray(state.data?.warnings)
    ? state.data.warnings.filter(Boolean).map(warning => `<li>${escapeChannelRefreshText(warning)}</li>`).join('')
    : '';
  const codeBuddyCheckinNotice = isCodeBuddy && state.checkin_status === 'ready'
    ? window.t(state.checkin_result === 'already_checked'
      ? 'channels.codebuddy.alreadyCheckedIn'
      : 'channels.codebuddy.checkinSuccess')
    : '';
  const codeBuddyCheckinError = isCodeBuddy && state.checkin_status === 'error'
    ? String(state.checkin_error || '').trim()
    : '';
  return `<div class="ch-oauth-usage">
    ${buildOAuthUsageToolbar(channel, state)}
    ${buildOAuthUsageRowsHtml(channel.id, rows)}
    ${channel?.auth_type === 'anthropic_oauth' ? buildAnthropicResetCreditsHtml(channel.id) : ''}
    ${isCodex ? (buildCodexResetCreditsHtml(state.data, state, channel.id) || buildCodexPurchasedCreditsHtml(state.data)) : ''}
    ${channel?.auth_type === 'antigravity_oauth' ? buildAntigravityCreditsHtml(state.data?.credits) : ''}
    ${isCodeBuddy ? buildCodeBuddyCreditsHtml(state.data?.codebuddy_credits, channel.id) : ''}
    ${codeBuddyCheckinNotice ? `<div class="ch-oauth-usage__notice" role="status">${escapeChannelRefreshText(codeBuddyCheckinNotice)}</div>` : ''}
    ${codeBuddyCheckinError ? `<div class="ch-oauth-usage__error" role="status" title="${escapeChannelRefreshText(codeBuddyCheckinError)}">${escapeChannelRefreshText(codeBuddyCheckinError)}</div>` : ''}
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
  return `<button type="button" class="ch-oauth-usage__refresh channel-action-btn" data-action="${action}" data-channel-id="${channelID}"${loading ? ' disabled aria-busy="true"' : ''}>${escapeChannelRefreshText(text)}</button>`;
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
function buildManagementWindowHtml(channelID, index, { label, remaining, used, total, percent, sampledAt }) {
  const hasAmounts = Boolean(used && total);
  const numericPercent = Number(percent);
  const hasUsage = hasAmounts && Number.isFinite(numericPercent);
  const percentText = hasUsage ? formatOAuthUsagePercent(Math.min(100, Math.max(0, numericPercent))) : '';
  const availableText = hasUsage ? window.t('channels.management.available', { percent: percentText }) : '';
  return buildOAuthUsageWindowHtml({
    tooltipID: oauthUsageTooltipID('ch-management-tooltip', channelID, index),
    label,
    // 只有余额时金额紧跟名称，右侧只留采样时间。
    amount: hasAmounts ? `${used}/${total}` : remaining,
    value: hasAmounts ? (remaining || (hasUsage ? `${percentText}%` : '')) : '',
    time: sampledAt,
    detailLines: [
      label,
      hasAmounts ? window.t('channels.management.usage', { used, total }) : '',
      remaining ? window.t('channels.management.remaining', { amount: remaining }) : '',
      availableText,
      sampledAt ? window.t('channels.management.sampledAt', { time: sampledAt }) : ''
    ],
    ariaLabel: availableText,
    percent: hasUsage ? numericPercent : null
  });
}

function buildManagementBalanceRows(channelID, balance) {
  const unit = balance?.unit;
  const remaining = formatManagementAmount(balance?.remaining, unit);
  const used = formatManagementAmount(balance?.used, unit);
  const total = formatManagementAmount(balance?.total, unit);
  const rows = [];
  if (remaining || (used && total)) {
    rows.push(buildManagementWindowHtml(channelID, 0, {
      label: window.t('channels.management.balance'),
      remaining,
      used,
      total,
      percent: balance?.available_percent,
      sampledAt: formatXAIUsageReset(balance?.sampled_at)
    }));
  }
  const subscriptions = Array.isArray(balance?.subscriptions) ? balance.subscriptions : [];
  subscriptions.forEach((entry, index) => {
    const subUsed = formatManagementAmount(entry?.used_usd, unit);
    const subTotal = formatManagementAmount(entry?.limit_usd, unit);
    if (!subUsed || !subTotal) return;
    const name = String(entry?.name || '').trim();
    const windowName = String(entry?.window || '').trim();
    rows.push(buildManagementWindowHtml(channelID, index + 1, {
      label: [name, windowName].filter(Boolean).join(' · ') || window.t('channels.management.subscription'),
      used: subUsed,
      total: subTotal,
      percent: entry?.available_percent
    }));
  });
  return rows;
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
    buttons.push(`<span class="ch-oauth-usage__reset" role="status">${escapeChannelRefreshText(checkedInAt)}</span>`);
  }

  const balanceError = balanceState?.status === 'error' ? String(balanceState.error || '').trim() : '';
  const checkinError = checkinState?.status === 'error' ? String(checkinState.error || '').trim() : '';
  const balanceData = balanceState?.status === 'ready'
    ? balanceState.data?.balance
    : (balanceState ? null : account?.balance);
  const balanceRows = buildManagementBalanceRows(channel.id, balanceData);

  const checkinSummary = checkinStatus === 'already_checked'
    || checkinStatus === 'skipped_disabled'
    ? ''
    : [statusText, rewardText].filter(Boolean).join(' · ');

  return `<div class="ch-oauth-usage">
    <div class="ch-oauth-usage__toolbar">${buttons.join('')}</div>
    ${balanceError ? `<div class="ch-oauth-usage__error" role="status" title="${escapeChannelRefreshText(balanceError)}">${escapeChannelRefreshText(balanceError)}</div>` : ''}
    ${buildOAuthUsageRowsHtml(channel.id, balanceRows)}
    ${checkinSummary ? `<div class="ch-oauth-usage__credits-summary" role="status">${escapeChannelRefreshText(checkinSummary)}</div>` : ''}
    ${checkinError ? `<div class="ch-oauth-usage__error" role="status" title="${escapeChannelRefreshText(checkinError)}">${escapeChannelRefreshText(checkinError)}</div>` : ''}
  </div>`;
}

const COOLDOWN_CLOCK_ICON = '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/></svg>';

// 渠道没有额度可显示时，额度列用成功率条承载调用表现，与额度条同一视觉。
const SUCCESS_RATE_FILL_LEVEL = { good: 'high', warn: 'medium', bad: 'low' };

function buildChannelPerformanceHtml(channelID, stats) {
  const metricsHtml = buildChannelMetricsHtml(stats, { omitCalls: true });
  if (!metricsHtml) return '';
  const success = Number(stats.success) || 0;
  const failure = Number(stats.error) || 0;
  const rate = success / (success + failure);
  const rateText = `${(rate * 100).toFixed(1)}%`;
  const label = window.t('channels.stats.successRateLabel');
  const amount = `${success} / ${failure} ${window.t('stats.unitTimes')}`;
  return `<div class="ch-oauth-usage ch-channel-performance">${buildOAuthUsageWindowHtml({
    tooltipID: oauthUsageTooltipID('ch-performance-tooltip', channelID, 0),
    label,
    amount,
    value: rateText,
    detailLines: [window.t('channels.stats.callsTitle'), `${amount} · ${rateText}`],
    ariaLabel: `${label} ${rateText}`,
    percent: rate * 100,
    level: SUCCESS_RATE_FILL_LEVEL[channelSuccessRateLevel(rate)]
  })}${metricsHtml}</div>`;
}

// 无调用、也无额度时：API 渠道给出配置管理账户的入口（只读身份不显示）。
function buildChannelQuotaHintHtml(channel) {
  const readOnly = typeof isTokenChannelsReadOnly === 'function' && isTokenChannelsReadOnly();
  if (channel?.auth_type !== 'api_key' || readOnly) return '';
  return `<div class="ch-quota-hint"><button type="button" class="ch-oauth-usage__refresh channel-action-btn" data-action="configure-management-account" data-channel-id="${channel.id}">${escapeChannelRefreshText(window.t('channels.status.configureManagement'))}</button></div>`;
}

// quotaHtml：额度区内容（OAuth/管理账户额度，或无额度时的成功率条/配置入口）。
function buildChannelRuntimeStatusHtml(channel, quotaHtml = '') {
  const statuses = [];
  const channelCooldownMS = Number(channel.cooldown_remaining_ms || 0);
  if (channelCooldownMS > 0) {
    const label = escapeChannelRefreshText(window.t('channels.status.channelCooldownLabel'));
    const timeText = escapeChannelRefreshText(formatCooldownRecoveryTime(channelCooldownMS, 'channels.status.daysHoursUntilRecovery'));
    statuses.push(`<div class="ch-runtime-status ch-runtime-status--channel"><span>${label}</span><span class="ch-runtime-status__clock">${COOLDOWN_CLOCK_ICON}</span><span>${timeText}</span></div>`);
  }

  const coolingKeys = (Array.isArray(channel.key_cooldowns) ? channel.key_cooldowns : [])
    .map(key => Number(key?.cooldown_remaining_ms || 0))
    .filter(remainingMS => remainingMS > 0);
  if (coolingKeys.length > 0) {
    const nextRecoveryMS = Math.min(...coolingKeys);
    const text = window.t('channels.status.keyCooldowns', {
      count: coolingKeys.length,
      time: formatCooldownRecoveryTime(nextRecoveryMS, 'channels.status.daysHoursUntilRecovery')
    });
    const label = window.t('channels.status.viewKeyCooldowns', { count: coolingKeys.length });
    statuses.push(`<button type="button" class="ch-runtime-status ch-runtime-status--keys channel-action-btn" data-action="edit-cooling-keys" data-channel-id="${channel.id}" aria-label="${escapeChannelRefreshText(label)}">${escapeChannelRefreshText(text)}</button>`);
  }

  const coolingModels = (Array.isArray(channel.model_cooldowns) ? channel.model_cooldowns : [])
    .map(model => Number(model?.cooldown_remaining_ms || 0))
    .filter(remainingMS => remainingMS > 0);
  if (coolingModels.length > 0) {
    const nextRecoveryMS = Math.min(...coolingModels);
    const text = escapeChannelRefreshText(window.t('channels.status.modelCooldowns', {
      count: coolingModels.length,
      time: formatCooldownRecoveryTime(nextRecoveryMS, 'channels.status.daysHoursUntilRecovery')
    }));
    statuses.push(`<div class="ch-runtime-status ch-runtime-status--models"><span class="ch-runtime-status__clock">${COOLDOWN_CLOCK_ICON}</span><span class="ch-runtime-status__text">${text}</span></div>`);
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

  if (quotaHtml) statuses.push(quotaHtml);

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
  const stats = channelStatsById[channel.id] || null;
  const batchRefreshResult = getBatchRefreshResult(channel.id);

  // 没有额度可显示时，调用表现移到额度列，左侧不再重复。
  const quotaHtml = buildOAuthUsageStatusHtml(channel) + buildManagementAccountStatusHtml(channel);
  const performanceHtml = quotaHtml ? '' : buildChannelPerformanceHtml(channel.id, stats);
  const runtimeStatusHtml = buildChannelRuntimeStatusHtml(
    channel,
    quotaHtml || performanceHtml || buildChannelQuotaHintHtml(channel)
  );
  const lastRequestFailureHtml = buildChannelLastRequestFailureHtml(stats);

  // 行class
  const rowClasses = ['channel-table-row'];
  if (isCooldown) rowClasses.push('channel-card-cooldown');
  if (!channel.enabled) rowClasses.push('channel-card-disabled');
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
    authType: channel.auth_type || 'api_key',
    avatarHtml: window.channelAvatarContentHTML(channel.auth_type || 'api_key', channel.name),
		nameMultiplierBadge: buildCornerMultiplierBadge(channel.cost_multiplier_min, channel.cost_multiplier_max),
    oauthPlanBadge: buildOAuthPlanBadge(channel),
    url: configuredURLs.join('\n'),
    batchRefreshStatusHtml: buildBatchRefreshStatusHtml(batchRefreshResult),
    // 一对多模型显示前两个重定向目标，悬停时显示全部目标。
    modelsHtml: buildChannelModelLineHtml(channel.models),
    metricsHtml: performanceHtml ? '' : buildChannelMetricsHtml(stats),
    effectivePriorityHtml: buildEffectivePriorityHtml(channel),
    runtimeStatusHtml: runtimeStatusHtml,
    lastRequestFailureHtml: lastRequestFailureHtml,
    enabled: channel.enabled,
    toggleTitle: channel.enabled ? window.t('channels.toggleDisable') : window.t('channels.toggleEnable'),
    toggleSwitchClass: channel.enabled ? 'channel-enable-switch--on' : 'channel-enable-switch--off',
    statusCellClass: runtimeStatusHtml ? '' : 'ch-mobile-empty',
    mobileLabelPriority: window.t('channels.table.priority'),
    mobileLabelStatus: window.t('channels.table.quotaStatus'),
    mobileLabelActions: window.t('channels.table.actions')
  };

  const card = TemplateEngine.render('tpl-channel-card', cardData);
  return card;
}

async function editChannelManagementAccount(channelId) {
  await editChannel(channelId);
  if (editingChannelId !== channelId) return;
  if (typeof revealChannelEditorSection === 'function') revealChannelEditorSection('management');
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
    // 只标记脏状态：保存会重渲染列表，输入中途自动保存会夺走焦点并吞掉后续按键
    input.classList.add('is-dirty');
  });

  container.addEventListener('keydown', (e) => {
    const sortHeader = e.target.closest('th[data-sort-key]');
    if (sortHeader && (e.key === 'Enter' || e.key === ' ')) {
      e.preventDefault();
      toggleChannelsSort(sortHeader.dataset.sortKey);
      return;
    }
    const input = e.target.closest('.ch-priority-input');
    if (!input || isTokenChannelsReadOnly()) return;
    if (e.key === 'Enter') {
      e.preventDefault();
      saveInlineChannelPriority(input);
    } else if (e.key === 'Escape') {
      const originalPriority = normalizeInlinePriorityValue(input.dataset.originalPriority, 0);
      input.value = String(originalPriority);
      input.classList.remove('is-dirty');
    }
  });

  container.addEventListener('focusout', (e) => {
    const input = e.target.closest('.ch-priority-input');
    if (!input || isTokenChannelsReadOnly()) return;
    saveInlineChannelPriority(input);
  });

  // 事件委托：处理所有渠道操作按钮
  container.addEventListener('click', (e) => {
    const sortHeader = e.target.closest('th[data-sort-key]');
    if (sortHeader) {
      toggleChannelsSort(sortHeader.dataset.sortKey);
      return;
    }

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
    if (isTokenChannelsReadOnly() && ['edit', 'edit-cooling-keys', 'configure-management-account', 'refresh-oauth-usage', 'reset-anthropic-quota', 'checkin-codebuddy', 'reset-codex-quota', 'refresh-management-balance', 'run-management-checkin', 'test', 'copy', 'delete', 'toggle'].includes(action)) {
      return;
    }
    const channelId = parseInt(btn.dataset.channelId);
    const channelName = btn.dataset.channelName;
    const enabled = btn.dataset.enabled === 'true';

    switch (action) {
      case 'edit':
        editChannel(channelId);
        break;
      case 'toggle-oauth-usage-windows':
        toggleOAuthUsageWindows(btn);
        break;
      case 'edit-cooling-keys':
        editChannelCoolingKeys(channelId);
        break;
      case 'configure-management-account':
        editChannelManagementAccount(channelId);
        break;
      case 'refresh-oauth-usage':
        if (typeof refreshOAuthUsage === 'function') {
          refreshOAuthUsage(channelId).catch(error => {
            if (window.showError) window.showError(error?.message || window.t('channels.oauth.usageFailed'));
          });
        }
        break;
      case 'reset-anthropic-quota':
        if (typeof confirmAnthropicQuotaReset === 'function') {
          confirmAnthropicQuotaReset(channelId).catch(() => {});
        }
        break;
      case 'checkin-codebuddy':
        if (typeof checkInCodeBuddy === 'function') {
          checkInCodeBuddy(channelId).then(result => {
            const key = result?.status === 'already_checked'
              ? 'channels.codebuddy.alreadyCheckedIn'
              : 'channels.codebuddy.checkinSuccess';
            if (window.showSuccess) window.showSuccess(window.t(key));
          }).catch(error => {
            if (window.showError) window.showError(error?.message || window.t('channels.codebuddy.checkinFailed'));
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
          window.showConfirm({
            message: window.t('channels.oauth.resetConfirm', { count, time: expiry }),
            danger: true
          }).then(confirmed => {
            if (!confirmed) return;
            return resetCodexQuota(channelId).then(result => {
              const hasWarnings = Array.isArray(result?.warnings) && result.warnings.length > 0;
              const message = !result?.usage || hasWarnings
                ? window.t('channels.oauth.resetSuccessNeedsRefresh')
                : window.t('channels.oauth.resetSuccess');
              window.showSuccess(message);
            });
          }).catch(error => {
            window.showError(error?.message || window.t('channels.oauth.resetFailed'));
          });
        }
        break;
      case 'test':
        testChannel(channels.find(channel => channel.id === channelId));
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

function buildChannelSortHeader(key, className, label) {
  const active = channelsSort.key === key;
  const ariaSort = active ? (channelsSort.order === 'asc' ? 'ascending' : 'descending') : 'none';
  return `<th class="${className} sortable${active ? ' sorted' : ''}" data-sort-key="${key}"${active ? ` data-sort-order="${channelsSort.order}"` : ''} aria-sort="${ariaSort}" tabindex="0">${label}<span class="sort-indicator" aria-hidden="true"></span></th>`;
}

function renderChannels(channelsToRender = channels) {
  const el = document.getElementById('channels-container');
  if ((!channelsToRender || channelsToRender.length === 0) && typeof channelsLoadFailed !== 'undefined' && channelsLoadFailed) {
    el.innerHTML = `<div class="glass-card channels-load-error" role="alert">
      <p>${window.t('channels.loadChannelsFailed')}</p>
      <button type="button" class="btn btn-secondary btn-sm" data-action="retry-load-channels">${window.t('common.retry')}</button>
    </div>`;
    if (typeof updateBatchChannelSelectionUI === 'function') {
      updateBatchChannelSelectionUI();
    }
    return;
  }
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
      ${buildChannelSortHeader('name', 'ch-col-name', window.t('channels.table.nameAndUrl'))}
      <th class="ch-col-status">${window.t('channels.table.quotaStatus')}</th>
      ${buildChannelSortHeader('priority', 'ch-col-priority', window.t('channels.table.priority'))}
      ${buildChannelSortHeader('enabled', 'ch-col-actions', window.t('channels.table.enabledAndActions'))}
    </tr>
  </thead>`;

  const tbody = document.createElement('tbody');
  channelsToRender.forEach(channel => {
    const row = createChannelCard(channel);
    if (row) tbody.appendChild(row);
  });

  el.innerHTML = `<div class="table-container channel-table-container"><table class="modern-table channel-table">${thead}</table></div>`;
  el.querySelector('table').appendChild(tbody);
  if (window.i18n && window.i18n.translatePage) {
    window.i18n.translatePage(el);
  }
  fitChannelModelLines(el);

  // 模板渲染后设置 checkbox 选中态
  el.querySelectorAll('.channel-select-checkbox').forEach(cb => {
    cb.checked = selectedChannelIds.has(normalizeSelectedChannelID(cb.dataset.channelId));
  });


  if (typeof updateBatchChannelSelectionUI === 'function') {
    updateBatchChannelSelectionUI();
  }
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    buildChannelModelLineHtml,
    buildChannelMetricsHtml,
    formatChannelModelTitle,
    buildChannelRuntimeStatusHtml,
    buildChannelPerformanceHtml,
    buildChannelQuotaHintHtml,
    buildOAuthUsageStatusHtml,
    toggleOAuthUsageWindows,
    buildManagementAccountStatusHtml,
    formatCooldownRecoveryTime,
    isOpenCodeGoChannel,
    channelShowsOAuthUsage
  };
}
