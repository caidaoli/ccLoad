// 系统设置页面
const t = window.t;

let originalSettings = {}; // 保存原始值用于比较
let runtimeMetricsLoading = false;
let runtimeMetricsPreviousFocus = null;
let globalCooldownRulesPreviousFocus = null;

const globalCooldownRulesSettingKey = 'global_cooldown_detection_rules';
const containerImageManagedDisabledReason = 'container_image_managed';
const advancedSettingKeys = new Set([
  globalCooldownRulesSettingKey,
  'auto_refresh_interval_seconds',
  'model_catalog_sync_interval_hours',
  'model_fuzzy_match'
]);

const byteSettingKeys = new Set([
  'max_body_bytes',
  'max_image_body_bytes',
  'responses_ws_max_transcript_bytes'
]);
const bytesPerM = 1024 * 1024;

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

function settingValueForDisplay(key, value) {
  const normalizedValue = String(value ?? '');
  if (!byteSettingKeys.has(key)) return normalizedValue;

  const bytes = Number(normalizedValue);
  return Number.isFinite(bytes) ? String(bytes / bytesPerM) : normalizedValue;
}

function settingValueForStorage(key, value) {
  const normalizedValue = String(value ?? '');
  if (!byteSettingKeys.has(key)) return normalizedValue;

  const megabytes = Number(normalizedValue);
  const bytes = Math.round(megabytes * bytesPerM);
  return Number.isFinite(bytes) ? String(bytes) : normalizedValue;
}

const runtimeMetricGroups = [
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
}

function closeRuntimeMetricsModal() {
  const modal = document.getElementById('runtimeMetricsModal');
  if (!modal) return;

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

function formatRuntimeMetric(metric, stats) {
  if (metric.format === 'bytes') return formatRuntimeBytes(stats[metric.key]);
  if (metric.format === 'duration') return formatRuntimeDuration(stats[metric.key]);
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
    <section class="runtime-metrics-section runtime-transcript-card">
      <div class="runtime-metrics-section-header">
        <h3>${escapeHtml(t('settings.runtimeMetrics.group.transcript'))}</h3>
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

  const sections = runtimeMetricGroups.map((group) => `
    <section class="runtime-metrics-section">
      <div class="runtime-metrics-section-header">
        <h3>${escapeHtml(t(group.titleKey))}</h3>
      </div>
      <div class="runtime-metrics-grid">
        ${group.metrics.map((metric) => renderRuntimeMetricCard(metric, stats)).join('')}
      </div>
    </section>`).join('');

  content.innerHTML = `
    ${renderTranscriptUsage(stats)}
    ${sections}
    <p class="runtime-metrics-note runtime-metrics-note--footer">${escapeHtml(t('settings.runtimeMetrics.cumulativeNote'))}</p>`;
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

async function loadRuntimeMetrics() {
  if (runtimeMetricsLoading) return;

  const content = document.getElementById('runtime-metrics-content');
  const refreshBtn = document.getElementById('refresh-runtime-metrics-btn');
  const updatedAt = document.getElementById('runtime-metrics-updated-at');
  runtimeMetricsLoading = true;
  if (content) content.setAttribute('aria-busy', 'true');
  if (refreshBtn) refreshBtn.disabled = true;
  if (updatedAt) updatedAt.textContent = '';
  renderRuntimeMetricsLoading();

  try {
    const data = await fetchDataWithAuth('/admin/runtime-metrics');
    const stats = data?.responses_websocket;
    if (!stats || typeof stats !== 'object' || Array.isArray(stats)) {
      throw new Error(t('settings.runtimeMetrics.invalidResponse'));
    }

    renderRuntimeMetrics(stats);
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
    { id: 'upstream-connection', nameKey: 'settings.group.upstreamConnection', order: 19, match: () => k === 'upstream_connection_reuse_limit_seconds' },
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
  const disabledAttributes = settingDisabledAttributes(setting);

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
    return `<input type="number" step="any" id="${safeKey}" value="${safeValue}" class="settings-input settings-input--number" ${disabledAttributes}>`;
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
      return `<input type="number" id="${safeKey}" value="${safeValue}" class="settings-input settings-input--number" ${disabledAttributes}>`;
    case 'float':
      return `<input type="number" step="any" id="${safeKey}" value="${safeValue}" class="settings-input settings-input--number" ${disabledAttributes}>`;
    default:
      return `<input type="text" id="${safeKey}" value="${safeValue}" class="settings-input settings-input--text${setting.key === 'channel_test_content' ? ' settings-input--wide' : ''}" ${disabledAttributes}>`;
  }
}

function markChanged(input) {
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

function syncSettingState(key, value) {
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

  originalSettings[key] = normalizedValue;
  if (control?.row) {
    control.row.style.background = '';
  }
  if (key === globalCooldownRulesSettingKey) updateGlobalCooldownRulesSummary(normalizedValue);
}

async function saveAllSettings() {
  // 收集所有变更
  const updates = {};

  for (const key of Object.keys(originalSettings)) {
    const control = getSettingControl(key);
    if (!control) continue;

    const currentValue = control.value;
    if (currentValue !== originalSettings[key]) {
      updates[key] = settingValueForStorage(key, currentValue);
    }
  }

  if (Object.keys(updates).length === 0) {
    window.showNotification(t('settings.msg.noChanges'), 'info');
    return;
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

async function resetSetting(key) {
  if (!confirm(t('settings.msg.confirmReset', { key }))) return;

  try {
    const result = await fetchDataWithAuth(`/admin/settings/${key}/reset`, { method: 'POST' });
    syncSettingState(key, result?.value ?? '');
    showSuccess(result?.message || t('settings.msg.resetSuccess', { key }));
  } catch (err) {
    console.error('重置异常:', err);
    showError(t('settings.msg.resetFailed') + ': ' + err.message);
  }
}

window.initPageBootstrap({
  topbarKey: 'settings',
  run: () => {
    bindSettingsPageActions();
    loadSettings();
  }
});
