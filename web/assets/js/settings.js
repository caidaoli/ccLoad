// 系统设置页面
const t = window.t;

let originalSettings = {}; // 保存原始值用于比较
let settingDefinitions = new Map();
let runtimeMetricsLoading = false;
let runtimeMetricsPreviousFocus = null;
let runtimeMetricsRefreshTimer = null;
const RUNTIME_METRICS_REFRESH_MS = 3000;
let globalCooldownRulesPreviousFocus = null;

const globalCooldownRulesSettingKey = 'global_cooldown_detection_rules';
const containerImageManagedDisabledReason = 'container_image_managed';
const advancedSettingKeys = new Set([
  globalCooldownRulesSettingKey,
  'auto_refresh_interval_seconds',
  'active_request_title_enabled',
  'model_catalog_sync_interval_hours',
  'model_fuzzy_match'
]);

const byteSettingKeys = new Set([
  'max_body_bytes',
  'max_image_body_bytes',
  'responses_ws_max_transcript_bytes'
]);
const oauthBaseURLSettingKeys = new Set([
  'codex_base_url',
  'xai_base_url',
  'antigravity_url',
  'anthropic_base_url'
]);
const oauthBaseURLPlaceholders = new Map([
  ['CODEX_BASE_URL', 'https://chatgpt.com/backend-api/codex/responses'],
  ['XAI_BASE_URL', 'https://cli-chat-proxy.grok.com/v1'],
  ['ANTIGRAVITY_URL', 'https://daily-cloudcode-pa.googleapis.com'],
  ['ANTHROPIC_BASE_URL', 'https://api.anthropic.com']
]);
const bytesPerMiB = 1024 * 1024;
const maxDurationSeconds = 9223372036;
const maxDurationMinutes = 153722867;
const maxDurationHours = 2562047;

const selectSettingOptions = new Map([
  ['auto_update_channel', [
    { value: 'stable', labelKey: 'settings.updateChannel.stable' },
    { value: 'preview', labelKey: 'settings.updateChannel.preview' }
  ]],
  ['channel_stats_range', [
    { value: 'today', labelKey: 'index.timeRange.today' },
    { value: 'yesterday', labelKey: 'index.timeRange.yesterday' },
    { value: 'day_before_yesterday', labelKey: 'index.timeRange.dayBeforeYesterday' },
    { value: 'this_week', labelKey: 'index.timeRange.thisWeek' },
    { value: 'last_week', labelKey: 'index.timeRange.lastWeek' },
    { value: 'this_month', labelKey: 'index.timeRange.thisMonth' },
    { value: 'last_month', labelKey: 'index.timeRange.lastMonth' }
  ]],
  ['log_channel_click_action', [
    { value: 'edit', labelKey: 'settings.logChannelClickAction.edit' },
    { value: 'navigate', labelKey: 'settings.logChannelClickAction.navigate' }
  ]]
]);

const numericSettingConstraints = new Map([
  ['max_key_retries', { min: 1 }],
  ['max_concurrency', { min: 1 }],
  ['max_body_bytes', { min: 1 / bytesPerMiB }],
  ['max_image_body_bytes', { min: 1 / bytesPerMiB }],
  ['log_retention_days', { min: -1, max: 365 }],
  ['cooldown_auth_seconds', { min: 1, max: maxDurationSeconds }],
  ['cooldown_server_seconds', { min: 1, max: maxDurationSeconds }],
  ['cooldown_timeout_seconds', { min: 1, max: maxDurationSeconds }],
  ['cooldown_rate_limit_seconds', { min: 1, max: maxDurationSeconds }],
  ['cooldown_min_seconds', { min: 1, max: maxDurationSeconds }],
  ['cooldown_max_seconds', { min: 1, max: maxDurationSeconds }],
  ['channel_check_interval_hours', { min: 0, max: maxDurationHours }],
  ['model_catalog_sync_interval_hours', { min: 0, max: maxDurationHours }],
  ['auto_update_interval_hours', { min: 0, max: maxDurationHours }],
  ['success_rate_penalty_weight', { min: 0 }],
  ['health_score_window_minutes', { min: 1, max: maxDurationMinutes }],
  ['health_score_update_interval', { min: 1, max: maxDurationSeconds }],
  ['health_min_confident_sample', { min: 1 }],
  ['ttfb_penalty_weight', { min: 0 }],
  ['ttfb_max_slow_ratio', { min: 0 }],
  ['ttfb_min_confident_sample', { min: 1 }],
  ['debug_log_retention_minutes', { min: 1, max: 1440 }],
  ['auto_refresh_interval_seconds', { min: 0, max: maxDurationSeconds }],
  ['responses_ws_max_sessions', { min: 0 }],
  ['responses_ws_session_ttl_minutes', { min: 0, max: maxDurationMinutes }],
  ['responses_ws_max_transcript_bytes', { min: 0 }],
  ['responses_ws_max_connections', { min: 0 }],
  ['responses_ws_max_connections_per_token', { min: 0 }]
]);

function settingValueForDisplay(key, value) {
  const normalizedValue = String(value ?? '');
  if (!byteSettingKeys.has(key)) return normalizedValue;

  const bytes = Number(normalizedValue);
  return Number.isFinite(bytes) ? String(bytes / bytesPerMiB) : normalizedValue;
}

function settingValueForStorage(key, value) {
  const normalizedValue = String(value ?? '');
  if (!byteSettingKeys.has(key)) return normalizedValue;

  const mebibytes = Number(normalizedValue);
  const bytes = Math.round(mebibytes * bytesPerMiB);
  return Number.isFinite(bytes) ? String(bytes) : normalizedValue;
}

function numericConstraintFor(setting) {
  const configured = numericSettingConstraints.get(setting.key);
  if (configured) return configured;
  if (setting.value_type === 'duration') return { min: 0, max: maxDurationSeconds };
  return null;
}

function numericInputAttributes(setting) {
  const constraint = numericConstraintFor(setting);
  const attributes = ['required'];
  if (constraint?.min !== undefined) attributes.push(`min="${constraint.min}"`);
  if (constraint?.max !== undefined) attributes.push(`max="${constraint.max}"`);
  const acceptsFraction = setting.value_type === 'float' || byteSettingKeys.has(setting.key);
  attributes.push(`step="${acceptsFraction ? 'any' : '1'}"`);
  return attributes.join(' ');
}

function validateSettingInput(setting, value) {
  const normalizedValue = String(value ?? '');
  if (selectSettingOptions.has(setting.key)) {
    const valid = selectSettingOptions.get(setting.key).some((option) => option.value === normalizedValue);
    return valid ? '' : t('settings.validation.selectListed');
  }
  if (setting.key === 'channel_test_content' && normalizedValue.trim() === '') {
    return t('settings.validation.testContentRequired');
  }

  const numeric = setting.value_type === 'int'
    || setting.value_type === 'float'
    || setting.value_type === 'duration'
    || byteSettingKeys.has(setting.key);
  if (!numeric) return '';

  if (normalizedValue.trim() === '') return t('settings.validation.numberRequired');
  const number = Number(normalizedValue);
  if (!Number.isFinite(number)) return t('settings.validation.finiteNumber');
  if (!byteSettingKeys.has(setting.key) && setting.value_type !== 'float' && !Number.isSafeInteger(number)) {
    return t('settings.validation.wholeNumber');
  }

  const constraint = numericConstraintFor(setting);
  if (constraint?.min !== undefined && number < constraint.min) {
    return t('settings.validation.minimum', { value: constraint.min });
  }
  if (constraint?.max !== undefined && number > constraint.max) {
    return t('settings.validation.maximum', { value: constraint.max });
  }
  if (setting.key === 'log_retention_days' && number !== -1 && (number < 1 || number > 365)) {
    return t('settings.validation.logRetention');
  }

  if (byteSettingKeys.has(setting.key)) {
    const bytes = Math.round(number * bytesPerMiB);
    if (!Number.isSafeInteger(bytes)) return t('settings.validation.smallerSize');
    if (number !== 0 && bytes === 0) return t('settings.validation.zeroOrOneByte');
    if (setting.key !== 'responses_ws_max_transcript_bytes' && bytes < 1) {
      return t('settings.validation.oneByteMinimum');
    }
  }
  return '';
}

const runtimeMetricDomains = [
  {
    sourceKey: 'process',
    titleKey: 'settings.runtimeMetrics.group.process',
    descriptionKey: 'settings.runtimeMetrics.processNote',
    metrics: [
      { key: 'uptime_seconds', labelKey: 'settings.runtimeMetrics.metric.uptime', format: 'duration' },
      { key: 'concurrency_slots_in_use', labelKey: 'settings.runtimeMetrics.metric.concurrencySlotsInUse' },
      { key: 'max_concurrency', labelKey: 'settings.runtimeMetrics.metric.maxConcurrency' },
      { key: 'goroutines', labelKey: 'settings.runtimeMetrics.metric.goroutines' }
    ]
  },
  {
    sourceKey: 'process',
    titleKey: 'settings.runtimeMetrics.group.resources',
    descriptionKey: 'settings.runtimeMetrics.resourcesNote',
    metrics: [
      { key: 'cpu_usage_percent', labelKey: 'settings.runtimeMetrics.metric.cpuUsagePercent', format: 'percent' },
      { key: 'cpu_user_seconds', labelKey: 'settings.runtimeMetrics.metric.cpuUserSeconds', format: 'seconds' },
      { key: 'cpu_system_seconds', labelKey: 'settings.runtimeMetrics.metric.cpuSystemSeconds', format: 'seconds' },
      { key: 'rss_bytes', labelKey: 'settings.runtimeMetrics.metric.rssBytes', format: 'bytes', zeroUnavailable: true },
      { key: 'max_rss_bytes', labelKey: 'settings.runtimeMetrics.metric.maxRssBytes', format: 'bytes', zeroUnavailable: true },
      { key: 'heap_alloc_bytes', labelKey: 'settings.runtimeMetrics.metric.heapAllocBytes', format: 'bytes' },
      { key: 'heap_sys_bytes', labelKey: 'settings.runtimeMetrics.metric.heapSysBytes', format: 'bytes' },
      { key: 'gc_count', labelKey: 'settings.runtimeMetrics.metric.gcCount' },
      { key: 'gc_pause_total_ns', labelKey: 'settings.runtimeMetrics.metric.gcPauseTotal', format: 'durationNs' },
      { key: 'gc_cpu_percent', labelKey: 'settings.runtimeMetrics.metric.gcCpuPercent', format: 'percent' }
    ]
  },
  {
    sourceKey: 'http_proxy',
    titleKey: 'settings.runtimeMetrics.group.httpProxy',
    descriptionKey: 'settings.runtimeMetrics.httpProxyNote',
    metrics: [
      { key: 'active_requests', labelKey: 'settings.runtimeMetrics.metric.httpActiveRequests' },
      { key: 'completed_requests', labelKey: 'settings.runtimeMetrics.metric.httpCompletedRequests' },
      { key: 'non_error_responses', labelKey: 'settings.runtimeMetrics.metric.httpNonErrorResponses' },
      { key: 'client_error_responses', labelKey: 'settings.runtimeMetrics.metric.httpClientErrorResponses' },
      { key: 'server_error_responses', labelKey: 'settings.runtimeMetrics.metric.httpServerErrorResponses' },
      { key: 'streaming_requests', labelKey: 'settings.runtimeMetrics.metric.httpStreamingRequests' },
      { key: 'non_streaming_requests', labelKey: 'settings.runtimeMetrics.metric.httpNonStreamingRequests' },
      { key: 'request_body_bytes', labelKey: 'settings.runtimeMetrics.metric.httpRequestBodyBytes', format: 'bytes' },
      { key: 'response_body_bytes', labelKey: 'settings.runtimeMetrics.metric.httpResponseBodyBytes', format: 'bytes' }
    ]
  },
  {
    sourceKey: 'logs',
    titleKey: 'settings.runtimeMetrics.group.logs',
    descriptionKey: 'settings.runtimeMetrics.logsNote',
    metrics: [
      { key: 'backlog_entries', labelKey: 'settings.runtimeMetrics.metric.logBacklogEntries' },
      { key: 'queue_capacity_entries', labelKey: 'settings.runtimeMetrics.metric.logQueueCapacityEntries' },
      { key: 'dropped_entries', labelKey: 'settings.runtimeMetrics.metric.logDroppedEntries' },
      { key: 'persistence_failed_entries', labelKey: 'settings.runtimeMetrics.metric.logPersistenceFailedEntries' }
    ]
  },
  {
    sourceKey: 'storage',
    titleKey: 'settings.runtimeMetrics.group.storage',
    descriptionKey: 'settings.runtimeMetrics.storageNote',
    optional: true,
    metrics: [
      { key: 'primary_sync_pending', labelKey: 'settings.runtimeMetrics.metric.primarySyncPending' },
      { key: 'primary_sync_failures', labelKey: 'settings.runtimeMetrics.metric.primarySyncFailures' },
      { key: 'primary_sync_dropped', labelKey: 'settings.runtimeMetrics.metric.primarySyncDropped' },
      { key: 'sqlite_read_failures', labelKey: 'settings.runtimeMetrics.metric.sqliteReadFailures' },
      { key: 'analytics_reads_primary', labelKey: 'settings.runtimeMetrics.metric.analyticsReadsPrimary', format: 'boolean' },
      { key: 'primary_sync_last_success_unix_ms', labelKey: 'settings.runtimeMetrics.metric.primarySyncLastSuccess', format: 'unixMilliseconds' }
    ]
  }
];

const responsesRuntimeMetricGroups = [
  {
    titleKey: 'settings.runtimeMetrics.group.sessions',
    metrics: [
      { key: 'sessions', labelKey: 'settings.runtimeMetrics.metric.sessions' },
      { key: 'max_sessions', labelKey: 'settings.runtimeMetrics.metric.maxSessions' },
      { key: 'active_attachments', labelKey: 'settings.runtimeMetrics.metric.activeAttachments' }
    ]
  },
  {
    titleKey: 'settings.runtimeMetrics.group.downstream',
    metrics: [
      { key: 'downstream_connections', labelKey: 'settings.runtimeMetrics.metric.downstreamConnections' },
      { key: 'max_downstream_connections', labelKey: 'settings.runtimeMetrics.metric.maxDownstreamConnections' },
      { key: 'max_downstream_connections_per_token', labelKey: 'settings.runtimeMetrics.metric.maxDownstreamConnectionsPerToken' },
      { key: 'rejected_downstream_connections', labelKey: 'settings.runtimeMetrics.metric.rejectedDownstreamConnections' }
    ]
  },
  {
    titleKey: 'settings.runtimeMetrics.group.upstream',
    metrics: [
      { key: 'upstream_connections', labelKey: 'settings.runtimeMetrics.metric.upstreamConnections' },
      { key: 'upstream_handshakes', labelKey: 'settings.runtimeMetrics.metric.upstreamHandshakes' },
      { key: 'upstream_reuses', labelKey: 'settings.runtimeMetrics.metric.upstreamReuses' },
      { key: 'reconnects', labelKey: 'settings.runtimeMetrics.metric.reconnects' },
      { key: 'upstream_heartbeat_failures', labelKey: 'settings.runtimeMetrics.metric.upstreamHeartbeatFailures' },
      { key: 'upstream_queued_read_bytes', labelKey: 'settings.runtimeMetrics.metric.upstreamQueuedReadBytes', format: 'bytes' },
      { key: 'oldest_upstream_connection_seconds', labelKey: 'settings.runtimeMetrics.metric.oldestUpstreamConnection', format: 'duration' }
    ]
  },
  {
    titleKey: 'settings.runtimeMetrics.group.responsesEvents',
    metrics: [
      { key: 'ttl_expired', labelKey: 'settings.runtimeMetrics.metric.ttlExpired' },
      { key: 'capacity_rejected', labelKey: 'settings.runtimeMetrics.metric.capacityRejected' },
      { key: 'budget_rejected', labelKey: 'settings.runtimeMetrics.metric.budgetRejected' },
      { key: 'previous_response_misses', labelKey: 'settings.runtimeMetrics.metric.previousResponseMisses' }
    ]
  }
];

function bindSettingsPageActions() {
  const saveAllBtn = document.getElementById('save-all-btn');
  if (saveAllBtn && !saveAllBtn.dataset.bound) {
    saveAllBtn.addEventListener('click', () => {
      saveAllSettings();
    });
    saveAllBtn.dataset.bound = '1';
  }

  const runtimeMetricsBtn = document.getElementById('runtime-metrics-btn');
  if (runtimeMetricsBtn && !runtimeMetricsBtn.dataset.bound) {
    runtimeMetricsBtn.addEventListener('click', openRuntimeMetricsModal);
    runtimeMetricsBtn.dataset.bound = '1';
  }

  const refreshBtn = document.getElementById('refresh-runtime-metrics-btn');
  if (refreshBtn && !refreshBtn.dataset.bound) {
    refreshBtn.addEventListener('click', loadRuntimeMetrics);
    refreshBtn.dataset.bound = '1';
  }

  document.querySelectorAll('[data-action="close-runtime-metrics"]').forEach((btn) => {
    if (btn.dataset.bound) return;
    btn.addEventListener('click', closeRuntimeMetricsModal);
    btn.dataset.bound = '1';
  });

  const modal = document.getElementById('runtimeMetricsModal');
  if (modal && !modal.dataset.bound) {
    modal.addEventListener('click', (event) => {
      if (event.target === modal) closeRuntimeMetricsModal();
    });
    modal.addEventListener('keydown', (event) => {
      if (event.key === 'Escape') closeRuntimeMetricsModal();
    });
    modal.dataset.bound = '1';
  }

  bindGlobalCooldownRulesModal();
}

function bindGlobalCooldownRulesModal() {
  const modal = document.getElementById('customRulesModal');
  if (!modal || modal.dataset.bound) return;

  modal.addEventListener('click', (event) => {
    if (event.target === modal) {
      closeGlobalCooldownRulesModal();
      return;
    }
    const button = event.target.closest('[data-action]');
    if (!button) return;
    const index = Number(button.dataset.cooldownDetectionIndex);
    switch (button.dataset.action) {
      case 'close-global-cooldown-rules':
        closeGlobalCooldownRulesModal();
        break;
      case 'apply-global-cooldown-rules':
        applyGlobalCooldownRules();
        break;
      case 'add-cooldown-detection-rule':
        window.addCooldownDetectionRule?.();
        break;
      case 'remove-cooldown-detection-rule':
        window.removeCooldownDetectionRule?.(index);
        break;
      case 'move-cooldown-detection-rule':
        window.moveCooldownDetectionRule?.(index, Number(button.dataset.cooldownDetectionDirection));
        break;
      case 'test-cooldown-detection-rules':
        window.testCooldownDetectionRules?.();
        break;
    }
  });
  modal.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') {
      event.preventDefault();
      closeGlobalCooldownRulesModal();
      return;
    }
    if (event.key === 'Tab') trapModalFocus(modal, event);
  });
  modal.dataset.bound = '1';
}

function trapModalFocus(modal, event) {
  const focusable = Array.from(modal.querySelectorAll(
    'button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'
  )).filter((element) => !element.hidden && element.offsetParent !== null);
  if (focusable.length === 0) return;
  const first = focusable[0];
  const last = focusable[focusable.length - 1];
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault();
    first.focus();
  }
}

function parseGlobalCooldownRules(value) {
  try {
    const parsed = JSON.parse(String(value || '{}'));
    return parsed && typeof parsed === 'object' && !Array.isArray(parsed) ? parsed : {};
  } catch (_) {
    return {};
  }
}

function globalCooldownRuleCount(value) {
  const parsed = parseGlobalCooldownRules(value);
  return Array.isArray(parsed.rules) ? parsed.rules.length : 0;
}

function updateGlobalCooldownRulesSummary(value) {
  const summary = document.getElementById('global-cooldown-rules-summary');
  if (!summary) return;
  summary.textContent = t('settings.globalCooldownRules.ruleCount', {
    count: globalCooldownRuleCount(value)
  });
}

function openGlobalCooldownRulesModal(trigger) {
  const modal = document.getElementById('customRulesModal');
  const input = document.getElementById(globalCooldownRulesSettingKey);
  if (!modal || !input || typeof window.resetCooldownDetectionState !== 'function') return;

  globalCooldownRulesPreviousFocus = trigger || document.activeElement;
  window.resetCooldownDetectionState(parseGlobalCooldownRules(input.value));
  window.beginCooldownDetectionDraft?.();
  document.querySelector('.app-container')?.setAttribute('inert', '');
  modal.classList.add('show');
  modal.setAttribute('aria-hidden', 'false');
  modal.querySelector('.close-btn')?.focus();
}

function closeGlobalCooldownRulesModal() {
  const modal = document.getElementById('customRulesModal');
  if (!modal) return;

  window.discardCooldownDetectionDraft?.();
  modal.classList.remove('show');
  modal.setAttribute('aria-hidden', 'true');
  document.querySelector('.app-container')?.removeAttribute('inert');
  if (globalCooldownRulesPreviousFocus?.isConnected) globalCooldownRulesPreviousFocus.focus();
  globalCooldownRulesPreviousFocus = null;
}

function applyGlobalCooldownRules() {
  if (!window.validateCooldownDetectionDraft?.()) return;
  if (!window.commitCooldownDetectionRules?.()) return;

  const input = document.getElementById(globalCooldownRulesSettingKey);
  if (!input) return;
  const payload = window.collectCooldownDetectionRulesForSubmit?.();
  input.value = JSON.stringify(payload || {});
  markChanged(input);
  updateGlobalCooldownRulesSummary(input.value);
  closeGlobalCooldownRulesModal();
}

function openRuntimeMetricsModal() {
  const modal = document.getElementById('runtimeMetricsModal');
  if (!modal) return;

  runtimeMetricsPreviousFocus = document.activeElement;
  modal.classList.add('show');
  modal.setAttribute('aria-hidden', 'false');
  modal.querySelector('.close-btn')?.focus();
  loadRuntimeMetrics();
  if (runtimeMetricsRefreshTimer === null) {
    runtimeMetricsRefreshTimer = setInterval(() => loadRuntimeMetrics({ silent: true }), RUNTIME_METRICS_REFRESH_MS);
  }
}

function closeRuntimeMetricsModal() {
  const modal = document.getElementById('runtimeMetricsModal');
  if (!modal) return;

  if (runtimeMetricsRefreshTimer !== null) {
    clearInterval(runtimeMetricsRefreshTimer);
    runtimeMetricsRefreshTimer = null;
  }
  modal.classList.remove('show');
  modal.setAttribute('aria-hidden', 'true');
  if (runtimeMetricsPreviousFocus?.isConnected) runtimeMetricsPreviousFocus.focus();
  runtimeMetricsPreviousFocus = null;
}

function normalizeRuntimeMetric(value) {
  if (value === null || value === undefined || value === '') return null;
  const numeric = Number(value);
  return Number.isFinite(numeric) && numeric >= 0 ? numeric : null;
}

function runtimeMetricsLocale() {
  return window.i18n?.getLocale?.() || document.documentElement.lang || 'zh-CN';
}

function formatRuntimeInteger(value) {
  const numeric = normalizeRuntimeMetric(value);
  if (numeric === null) return '—';
  return new Intl.NumberFormat(runtimeMetricsLocale(), { maximumFractionDigits: 0 }).format(numeric);
}

function formatRuntimeDecimal(value, maximumFractionDigits = 1) {
  return new Intl.NumberFormat(runtimeMetricsLocale(), { maximumFractionDigits }).format(value);
}

function formatRuntimeBytes(value) {
  const numeric = normalizeRuntimeMetric(value);
  if (numeric === null) return '—';

  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let unitIndex = 0;
  let amount = numeric;
  while (amount >= 1024 && unitIndex < units.length - 1) {
    amount /= 1024;
    unitIndex++;
  }
  const digits = unitIndex === 0 ? 0 : 1;
  return `${formatRuntimeDecimal(amount, digits)} ${units[unitIndex]}`;
}

function formatRuntimeDuration(value) {
  const numeric = normalizeRuntimeMetric(value);
  if (numeric === null) return '—';

  const seconds = Math.round(numeric);
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  if (hours > 0) return t('common.timeHM', { h: hours, m: minutes });
  if (minutes > 0) return t('common.timeMS', { m: minutes, s: seconds % 60 });
  return t('common.timeS', { s: seconds });
}

function formatRuntimeBoolean(value) {
  if (typeof value !== 'boolean') return '—';
  return t(value ? 'common.yes' : 'common.no');
}

function formatRuntimeTimestamp(value) {
  const numeric = normalizeRuntimeMetric(value);
  if (numeric === null || numeric <= 0) return '—';
  const date = new Date(numeric);
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString(runtimeMetricsLocale());
}

function formatRuntimePercent(value) {
  const numeric = normalizeRuntimeMetric(value);
  if (numeric === null) return '—';
  return `${formatRuntimeDecimal(numeric, 1)}%`;
}

function formatRuntimeSeconds(value) {
  const numeric = normalizeRuntimeMetric(value);
  if (numeric === null) return '—';
  if (numeric < 60) return t('common.timeS', { s: formatRuntimeDecimal(numeric, 1) });
  return formatRuntimeDuration(numeric);
}

function formatRuntimeDurationNs(value) {
  const numeric = normalizeRuntimeMetric(value);
  if (numeric === null) return '—';
  if (numeric < 1e9) return `${formatRuntimeDecimal(numeric / 1e6, 1)} ms`;
  return formatRuntimeDuration(numeric / 1e9);
}

function formatRuntimeMetric(metric, stats) {
  if (metric.zeroUnavailable && normalizeRuntimeMetric(stats[metric.key]) === 0) return '—';
  if (metric.format === 'bytes') return formatRuntimeBytes(stats[metric.key]);
  if (metric.format === 'duration') return formatRuntimeDuration(stats[metric.key]);
  if (metric.format === 'seconds') return formatRuntimeSeconds(stats[metric.key]);
  if (metric.format === 'durationNs') return formatRuntimeDurationNs(stats[metric.key]);
  if (metric.format === 'percent') return formatRuntimePercent(stats[metric.key]);
  if (metric.format === 'boolean') return formatRuntimeBoolean(stats[metric.key]);
  if (metric.format === 'unixMilliseconds') return formatRuntimeTimestamp(stats[metric.key]);
  if (metric.format === 'text') {
    const value = stats[metric.key];
    return value === null || value === undefined || String(value).trim() === '' ? '—' : String(value);
  }
  return formatRuntimeInteger(stats[metric.key]);
}

function renderRuntimeMetricCard(metric, stats) {
  return `
    <div class="runtime-metric-card">
      <span class="runtime-metric-label">${escapeHtml(t(metric.labelKey))}</span>
      <strong class="runtime-metric-value">${escapeHtml(formatRuntimeMetric(metric, stats))}</strong>
      <code class="runtime-metric-key">${escapeHtml(metric.key)}</code>
    </div>`;
}

function renderRuntimeMetricDomain(domain, payload) {
  const stats = payload[domain.sourceKey];
  if (!stats || typeof stats !== 'object' || Array.isArray(stats)) {
    return domain.optional ? '' : `
      <section class="runtime-metrics-section">
        <div class="runtime-metrics-section-header">
          <h3>${escapeHtml(t(domain.titleKey))}</h3>
        </div>
        <p class="runtime-metrics-note">${escapeHtml(t('settings.runtimeMetrics.groupUnavailable'))}</p>
      </section>`;
  }
  return `
    <section class="runtime-metrics-section">
      <div class="runtime-metrics-section-header">
        <h3>${escapeHtml(t(domain.titleKey))}</h3>
      </div>
      <p class="runtime-metrics-section-description">${escapeHtml(t(domain.descriptionKey))}</p>
      <div class="runtime-metrics-grid">
        ${domain.metrics.map((metric) => renderRuntimeMetricCard(metric, stats)).join('')}
      </div>
    </section>`;
}

function renderRuntimeMetricGroup(group, stats) {
  return `
    <section class="runtime-metrics-subsection">
      <div class="runtime-metrics-subsection-header">
        <h4>${escapeHtml(t(group.titleKey))}</h4>
      </div>
      <div class="runtime-metrics-grid">
        ${group.metrics.map((metric) => renderRuntimeMetricCard(metric, stats)).join('')}
      </div>
    </section>`;
}

function renderTranscriptUsage(stats) {
  const used = normalizeRuntimeMetric(stats.transcript_bytes);
  const budget = normalizeRuntimeMetric(stats.max_transcript_bytes);
  const percent = used !== null && budget !== null && budget > 0
    ? (used / budget) * 100
    : null;

  let state = 'unavailable';
  if (percent !== null) {
    if (percent > 100) state = 'exceeded';
    else if (percent >= 80) state = 'warning';
    else state = 'normal';
  }

  const stateLabel = t(`settings.runtimeMetrics.transcriptStatus.${state}`);
  const percentLabel = percent === null ? '—' : `${formatRuntimeDecimal(percent, 1)}%`;
  const progressValue = percent === null ? 0 : Math.min(100, Math.max(0, percent));
  const progressAria = percent === null
    ? ''
    : `aria-valuenow="${Math.round(progressValue)}"`;

  return `
    <section class="runtime-transcript-card">
      <div class="runtime-metrics-subsection-header">
        <h4>${escapeHtml(t('settings.runtimeMetrics.group.transcript'))}</h4>
        <span class="runtime-transcript-status runtime-transcript-status--${state}">${escapeHtml(stateLabel)}</span>
      </div>
      <div class="runtime-transcript-summary">
        <strong>${escapeHtml(formatRuntimeBytes(used))} / ${escapeHtml(formatRuntimeBytes(budget))}</strong>
        <span>${escapeHtml(percentLabel)}</span>
      </div>
      <div class="runtime-transcript-progress" role="progressbar" aria-label="${escapeHtml(t('settings.runtimeMetrics.group.transcript'))}" aria-valuemin="0" aria-valuemax="100" ${progressAria}>
        <span class="runtime-transcript-progress-bar runtime-transcript-progress-bar--${state}" style="width: ${progressValue}%"></span>
      </div>
      <div class="runtime-transcript-details">
        <span><b>${escapeHtml(t('settings.runtimeMetrics.metric.transcriptBytes'))}: ${escapeHtml(formatRuntimeBytes(used))}</b><code>transcript_bytes</code></span>
        <span><b>${escapeHtml(t('settings.runtimeMetrics.metric.maxTranscriptBytes'))}: ${escapeHtml(formatRuntimeBytes(budget))}</b><code>max_transcript_bytes</code></span>
      </div>
      <p class="runtime-metrics-note">${escapeHtml(t('settings.runtimeMetrics.transcriptNote'))}</p>
    </section>`;
}

function renderRuntimeMetrics(stats) {
  const content = document.getElementById('runtime-metrics-content');
  if (!content) return;

  const domains = runtimeMetricDomains.map((domain) => renderRuntimeMetricDomain(domain, stats)).join('');
  const responses = stats.responses_websocket;
  let responsesSection = '';
  if (responses && typeof responses === 'object' && !Array.isArray(responses)) {
    const groups = responsesRuntimeMetricGroups.map((group) => renderRuntimeMetricGroup(group, responses)).join('');
    responsesSection = `
    <section class="runtime-metrics-section runtime-responses-section">
      <div class="runtime-metrics-section-header">
        <h3>${escapeHtml(t('settings.runtimeMetrics.group.responses'))}</h3>
      </div>
      <p class="runtime-metrics-section-description">${escapeHtml(t('settings.runtimeMetrics.responsesNote'))}</p>
      ${renderTranscriptUsage(responses)}
      ${groups}
      <p class="runtime-metrics-note runtime-metrics-note--footer">${escapeHtml(t('settings.runtimeMetrics.cumulativeNote'))}</p>
    </section>`;
  }

  content.innerHTML = `
    ${domains}
    ${responsesSection}`;
}

function renderRuntimeMetricsLoading() {
  const content = document.getElementById('runtime-metrics-content');
  if (!content) return;
  content.innerHTML = `
    <div class="runtime-metrics-message">
      <span class="loading-spinner" aria-hidden="true"></span>
      <span>${escapeHtml(t('settings.runtimeMetrics.loading'))}</span>
    </div>`;
}

function renderRuntimeMetricsError(error) {
  const content = document.getElementById('runtime-metrics-content');
  if (!content) return;
  const message = error?.message || t('settings.runtimeMetrics.loadFailed');
  content.innerHTML = `
    <div class="runtime-metrics-message runtime-metrics-message--error" role="alert">
      <strong>${escapeHtml(t('settings.runtimeMetrics.loadFailed'))}</strong>
      <span>${escapeHtml(message)}</span>
    </div>`;
}

async function loadRuntimeMetrics(options) {
  if (runtimeMetricsLoading) return;
  // 手动刷新按钮的 click 事件会把 MouseEvent 传进来,此时 silent 恒为 false
  const silent = options?.silent === true;

  const content = document.getElementById('runtime-metrics-content');
  const refreshBtn = document.getElementById('refresh-runtime-metrics-btn');
  const updatedAt = document.getElementById('runtime-metrics-updated-at');
  runtimeMetricsLoading = true;
  if (content) content.setAttribute('aria-busy', 'true');
  if (!silent) {
    if (refreshBtn) refreshBtn.disabled = true;
    if (updatedAt) updatedAt.textContent = '';
    renderRuntimeMetricsLoading();
  }

  try {
    const data = await fetchDataWithAuth('/admin/runtime-metrics');
    if (!data || typeof data !== 'object' || Array.isArray(data)) {
      throw new Error(t('settings.runtimeMetrics.invalidResponse'));
    }

    renderRuntimeMetrics(data);
    if (updatedAt) {
      const time = new Date().toLocaleString(runtimeMetricsLocale());
      updatedAt.textContent = t('settings.runtimeMetrics.updatedAt', { time });
    }
  } catch (error) {
    console.error('Failed to load runtime metrics:', error);
    renderRuntimeMetricsError(error);
  } finally {
    runtimeMetricsLoading = false;
    if (content) content.setAttribute('aria-busy', 'false');
    if (refreshBtn) refreshBtn.disabled = false;
  }
}

function getSettingGroupInfo(key) {
  const k = String(key || '').toLowerCase();

  const defs = [
    { id: 'advanced', nameKey: 'settings.group.advanced', order: 70, match: () => advancedSettingKeys.has(k) },
    { id: 'channel', nameKey: 'settings.group.channel', order: 10, match: () => k.startsWith('channel_') || k === 'max_key_retries' },
    { id: 'model', nameKey: 'settings.group.model', order: 15, match: () => k.startsWith('model_') },
    { id: 'upstream-connection', nameKey: 'settings.group.upstreamConnection', order: 19, match: () => k === 'upstream_connection_reuse_limit_seconds' || oauthBaseURLSettingKeys.has(k) },
    { id: 'websocket', nameKey: 'settings.group.websocket', order: 25, match: () => k.startsWith('responses_ws_') },
    { id: 'stream-timeout', nameKey: 'settings.group.streamTimeout', order: 20, match: () => k === 'stream_timeout' || k.endsWith('_first_byte_timeout') },
    { id: 'non-stream-timeout', nameKey: 'settings.group.nonStreamTimeout', order: 21, match: () => k === 'non_stream_timeout' || k.endsWith('_non_stream_timeout') },
    { id: 'limits', nameKey: 'settings.group.limits', order: 26, match: () => k === 'max_concurrency' || k.endsWith('_body_bytes') },
    { id: 'health', nameKey: 'settings.group.health', order: 30, match: () => k.includes('health_score') || k.includes('success_rate') || k.includes('penalty_weight') || k.includes('ttfb') || k === 'enable_health_score' || k === 'health_min_confident_sample' },
    { id: 'cooldown', nameKey: 'settings.group.cooldown', order: 40, match: () => k.startsWith('cooldown_') },
    { id: 'log', nameKey: 'settings.group.log', order: 50, match: () => k.startsWith('log_') || k.startsWith('debug_') },
    { id: 'access', nameKey: 'settings.group.access', order: 60, match: () => k.includes('auth_') },
    { id: 'update', nameKey: 'settings.group.update', order: 65, match: () => k.startsWith('auto_update_') },
  ];

  for (const d of defs) {
    if (d.match()) return { ...d, name: t(d.nameKey) };
  }
  return { id: 'other', nameKey: 'settings.group.other', name: t('settings.group.other'), order: 999 };
}

function getSettingOrder(key) {
  const orders = {
    upstream_connection_reuse_limit_seconds: 90,
    codex_base_url: 91,
    xai_base_url: 92,
    antigravity_url: 93,
    anthropic_base_url: 94,
    upstream_first_byte_timeout: 100,
    stream_timeout: 101,
    non_stream_timeout: 102,
    anthropic_first_byte_timeout: 110,
    anthropic_non_stream_timeout: 111,
    codex_first_byte_timeout: 120,
    codex_non_stream_timeout: 121,
    openai_first_byte_timeout: 130,
    openai_non_stream_timeout: 131,
    gemini_first_byte_timeout: 140,
    gemini_non_stream_timeout: 141,
    max_concurrency: 200,
    max_body_bytes: 201,
    max_image_body_bytes: 202,
    cooldown_fallback_enabled: 300,
    cooldown_auth_seconds: 301,
    cooldown_server_seconds: 302,
    cooldown_timeout_seconds: 303,
    cooldown_rate_limit_seconds: 304,
    cooldown_min_seconds: 305,
    cooldown_max_seconds: 306,
    global_cooldown_detection_rules: 700
  };
  const normalizedKey = String(key || '').toLowerCase();
  return orders[normalizedKey] ?? 1000;
}

function groupSettings(settings) {
  const groupsById = new Map();

  for (const s of settings) {
    const g = getSettingGroupInfo(s.key);
    if (!groupsById.has(g.id)) {
      groupsById.set(g.id, { id: g.id, name: g.name, order: g.order, settings: [] });
    }
    groupsById.get(g.id).settings.push(s);
  }

  const groups = Array.from(groupsById.values())
    .sort((a, b) => a.order - b.order || a.name.localeCompare(b.name));

  for (const g of groups) {
    g.settings.sort((a, b) => {
      const orderDiff = getSettingOrder(a.key) - getSettingOrder(b.key);
      if (orderDiff !== 0) return orderDiff;
      return String(a.key).localeCompare(String(b.key));
    });
  }

  return groups;
}

function renderGroupNav(groups) {
  const nav = document.getElementById('settings-group-nav');
  const navSection = document.getElementById('settings-group-nav-section');
  if (!nav) return;

  nav.innerHTML = '';
  const hasMultipleGroups = Array.isArray(groups) && groups.length > 1;
  if (navSection) navSection.hidden = !hasMultipleGroups;
  if (!hasMultipleGroups) return;

  for (let i = 0; i < groups.length; i++) {
    const g = groups[i];
    const btn = document.createElement('button');
    btn.className = 'time-range-btn' + (i === 0 ? ' active' : '');
    btn.textContent = g.name;
    btn.addEventListener('click', () => {
      // 移除所有按钮的 active 状态
      nav.querySelectorAll('.time-range-btn').forEach(b => b.classList.remove('active'));
      btn.classList.add('active');
      // 滚动到对应分组
      const target = document.getElementById(`settings-group-${g.id}`);
      if (target) target.scrollIntoView({ behavior: 'smooth', block: 'start' });
    });
    nav.appendChild(btn);
  }
}

async function loadSettings() {
  try {
    const data = await fetchDataWithAuth('/admin/settings');
    if (!Array.isArray(data)) throw new Error(t('settings.msg.invalidResponse'));
    renderSettings(data);
  } catch (err) {
    console.error('Failed to load settings:', err);
    showError(t('settings.msg.loadFailed') + ': ' + err.message);
  }
}

function renderSettings(settings) {
  const tbody = document.getElementById('settings-tbody');
  originalSettings = {};
  settingDefinitions = new Map(settings.map((setting) => [setting.key, setting]));
  tbody.innerHTML = '';

  // 初始化事件委托（仅一次）
  initSettingsEventDelegation();

  const groups = groupSettings(settings);
  renderGroupNav(groups);

  for (const g of groups) {
    const groupRow = TemplateEngine.render('tpl-setting-group-row', {
      groupId: g.id,
      groupName: g.name,
      groupNoticeHtml: renderSettingGroupNotice(g)
    });
    if (groupRow) tbody.appendChild(groupRow);

    for (const s of g.settings) {
      const displayValue = settingValueForDisplay(s.key, s.value);
      originalSettings[s.key] = displayValue;
      // 优先使用语言包中的描述，若没有则回退到后端返回的描述
      const descKey = `settings.desc.${s.key}`;
      const translatedDesc = t(descKey);
      const description = (translatedDesc !== descKey) ? translatedDesc : s.description;
      const row = TemplateEngine.render('tpl-setting-row', {
        key: s.key,
        description: description,
        inputHtml: renderInput({ ...s, value: displayValue }),
        resetDisabledAttributes: settingDisabledAttributes(s),
        mobileLabelDescription: t('settings.configItem'),
        mobileLabelValue: t('settings.currentValue'),
        mobileLabelActions: t('common.actions')
      });
      if (row) tbody.appendChild(row);
    }
  }
}

function renderSettingGroupNotice(group) {
  const containerManaged = group.id === 'update' && group.settings.some((setting) => (
    setting.editable === false && setting.disabled_reason === containerImageManagedDisabledReason
  ));
  if (!containerManaged) return '';

  return `
    <div class="settings-group-notice" role="note">
      <p>${escapeHtml(t('settings.update.containerManaged'))}</p>
      <ul>
        <li>${escapeHtml(t('settings.update.stableImage'))}: <code>ghcr.io/caidaoli/ccload:latest</code></li>
        <li>${escapeHtml(t('settings.update.betaImage'))}: <code>ghcr.io/caidaoli/ccload:beta</code></li>
      </ul>
      <p>${escapeHtml(t('settings.update.applyImage'))}</p>
      <code class="settings-group-notice-command">docker compose pull &amp;&amp; docker compose up -d</code>
    </div>`;
}

function settingDisabledAttributes(setting) {
  return setting.editable === false ? 'disabled' : '';
}

// 初始化事件委托（替代 inline onclick）
function initSettingsEventDelegation() {
  const tbody = document.getElementById('settings-tbody');
  if (!tbody || tbody.dataset.delegated) return;
  tbody.dataset.delegated = 'true';

  // 重置按钮点击
  tbody.addEventListener('click', (e) => {
    const editGlobalRulesBtn = e.target.closest('[data-action="edit-global-cooldown-rules"]');
    if (editGlobalRulesBtn) {
      openGlobalCooldownRulesModal(editGlobalRulesBtn);
      return;
    }
    const resetBtn = e.target.closest('.setting-reset-btn');
    if (resetBtn) {
      resetSetting(resetBtn.dataset.key);
    }
  });

  // 输入变更
  tbody.addEventListener('change', (e) => {
    const input = e.target.closest('input, select');
    if (input) markChanged(input);
  });
}

function renderInput(setting) {
  const safeKey = escapeHtml(setting.key);
  const safeValue = escapeHtml(setting.value);
  const placeholder = oauthBaseURLPlaceholders.get(setting.key);
  const placeholderAttribute = placeholder ? `placeholder="${escapeHtml(placeholder)}"` : '';
  const wideTextInput = setting.key === 'channel_test_content' || oauthBaseURLPlaceholders.has(setting.key);
  const disabledAttributes = settingDisabledAttributes(setting);
  const numericAttributes = numericInputAttributes(setting);

  if (setting.key === globalCooldownRulesSettingKey) {
    const count = globalCooldownRuleCount(setting.value);
    return `
      <div class="global-cooldown-rules-control">
        <input type="hidden" id="${safeKey}" value="${safeValue}">
        <button type="button" class="btn btn-secondary" data-action="edit-global-cooldown-rules" ${disabledAttributes}>
          ${escapeHtml(t('settings.globalCooldownRules.edit'))}
        </button>
        <span id="global-cooldown-rules-summary" class="global-cooldown-rules-summary">
          ${escapeHtml(t('settings.globalCooldownRules.ruleCount', { count }))}
        </span>
      </div>`;
  }

  const selectOptions = selectSettingOptions.get(setting.key);
  if (selectOptions) {
    const optionsHtml = selectOptions.map(({ value, labelKey }) => (
      `<option value="${value}" ${setting.value === value ? 'selected' : ''}>${escapeHtml(t(labelKey))}</option>`
    )).join('');
    return `
      <select id="${safeKey}" class="settings-input settings-input--select" ${disabledAttributes}>
        ${optionsHtml}
      </select>`;
  }

  if (byteSettingKeys.has(setting.key)) {
    return `<input type="number" id="${safeKey}" value="${safeValue}" class="settings-input settings-input--number" ${numericAttributes} ${disabledAttributes}>`;
  }

  switch (setting.value_type) {
    case 'bool':
      const isTrue = setting.value === 'true' || setting.value === '1';
      return `
        <div class="settings-bool-group">
          <label class="settings-bool-option">
            <input type="radio" name="${safeKey}" value="true" ${isTrue ? 'checked' : ''} ${disabledAttributes}> ${t('common.enable')}
          </label>
          <label class="settings-bool-option">
            <input type="radio" name="${safeKey}" value="false" ${!isTrue ? 'checked' : ''} ${disabledAttributes}> ${t('common.disable')}
          </label>
        </div>`;
    case 'int':
    case 'duration':
      return `<input type="number" id="${safeKey}" value="${safeValue}" class="settings-input settings-input--number" ${numericAttributes} ${disabledAttributes}>`;
    case 'float':
      return `<input type="number" id="${safeKey}" value="${safeValue}" class="settings-input settings-input--number" ${numericAttributes} ${disabledAttributes}>`;
    default:
      return `<input type="text" id="${safeKey}" value="${safeValue}" ${placeholderAttribute} class="settings-input settings-input--text${wideTextInput ? ' settings-input--wide' : ''}" ${disabledAttributes}>`;
  }
}

function markChanged(input) {
  input.removeAttribute?.('aria-invalid');
  const row = input.closest('tr');
  let key, currentValue;

  if (input.type === 'radio') {
    key = input.name;
    const checkedRadio = row.querySelector(`input[name="${key}"]:checked`);
    currentValue = checkedRadio ? checkedRadio.value : '';
  } else {
    key = input.id;
    currentValue = input.value;
  }

  if (currentValue !== originalSettings[key]) {
    row.style.background = 'rgba(59, 130, 246, 0.08)';
  } else {
    row.style.background = '';
  }
}

function setSettingInvalid(key, invalid) {
  const control = getSettingControl(key);
  if (!control) return;
  const inputs = control.radios ? Array.from(control.radios) : [control.input];
  for (const input of inputs) {
    if (invalid) input.setAttribute?.('aria-invalid', 'true');
    else input.removeAttribute?.('aria-invalid');
  }
  if (invalid) control.input?.focus?.();
}

function getSettingControl(key) {
  const input = document.getElementById(key);
  if (input) {
    return {
      input,
      row: input.closest('tr'),
      value: input.value
    };
  }

  const radios = document.querySelectorAll(`input[name="${key}"]`);
  if (radios.length === 0) return null;

  const checkedRadio = document.querySelector(`input[name="${key}"]:checked`);
  return {
    input: radios[0],
    radios,
    row: radios[0].closest('tr'),
    value: checkedRadio ? checkedRadio.value : ''
  };
}

function setSettingControlValue(key, value) {
  const normalizedValue = settingValueForDisplay(key, value);
  const control = getSettingControl(key);

  if (control?.radios) {
    for (const radio of control.radios) {
      radio.checked = radio.value === normalizedValue
        || (normalizedValue === '1' && radio.value === 'true')
        || (normalizedValue === '0' && radio.value === 'false');
    }
  } else if (control?.input) {
    control.input.value = normalizedValue;
  }

  if (key === globalCooldownRulesSettingKey) updateGlobalCooldownRulesSummary(normalizedValue);
  return control;
}

function syncSettingState(key, value) {
  const normalizedValue = settingValueForDisplay(key, value);
  const control = setSettingControlValue(key, value);

  originalSettings[key] = normalizedValue;
  if (control?.row) {
    control.row.style.background = '';
  }
}

async function saveAllSettings() {
  // 收集所有变更
  const updates = {};

  for (const key of Object.keys(originalSettings)) {
    const control = getSettingControl(key);
    if (!control) continue;

    const currentValue = control.value;
    if (currentValue !== originalSettings[key]) {
      const setting = settingDefinitions.get(key);
      const validationError = setting ? validateSettingInput(setting, currentValue) : '';
      if (validationError) {
        setSettingInvalid(key, true);
        showError(t('settings.msg.invalidValue', { key, reason: validationError }));
        return;
      }
      setSettingInvalid(key, false);
      updates[key] = settingValueForStorage(key, currentValue);
    }
  }

  if (Object.keys(updates).length === 0) {
    window.showNotification(t('settings.msg.noChanges'), 'info');
    return;
  }

  if ('cooldown_min_seconds' in updates || 'cooldown_max_seconds' in updates) {
    const minSeconds = Number(getSettingControl('cooldown_min_seconds')?.value);
    const maxSeconds = Number(getSettingControl('cooldown_max_seconds')?.value);
    if (minSeconds > maxSeconds) {
      setSettingInvalid('cooldown_min_seconds', true);
      setSettingInvalid('cooldown_max_seconds', true);
      showError(t('settings.msg.invalidCooldownBounds'));
      return;
    }
  }

  if (!confirm(t('settings.msg.confirmSave'))) return;

  // 使用批量更新接口（单次请求，事务保护）
  try {
    const result = await fetchDataWithAuth('/admin/settings/batch', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(updates)
    });

    for (const [key, value] of Object.entries(updates)) {
      syncSettingState(key, value);
    }

    showSuccess(result?.message || t('settings.msg.savedCount', { count: Object.keys(updates).length }));
  } catch (err) {
    console.error('保存异常:', err);
    showError(t('settings.msg.saveFailed') + ': ' + err.message);
  }
}

function resetSetting(key) {
  const setting = settingDefinitions.get(key);
  if (!setting) {
    showError(t('settings.msg.resetUnavailable', { key }));
    return;
  }

  const control = setSettingControlValue(key, setting.default_value ?? '');
  if (control?.input) markChanged(control.input);
}

window.initPageBootstrap({
  topbarKey: 'settings',
  run: () => {
    bindSettingsPageActions();
    loadSettings();
  }
});
