const t = window.t;

let currentLogsPage = 1;
let logsPageSize = 15;
let totalLogsPages = 1;
let totalLogs = 0;
let currentChannelType = 'all'; // 当前选中的渠道类型
let authTokens = []; // 令牌列表
let logsChannelNameCombobox = null; // 渠道名筛选组合框
let logsModelCombobox = null; // 模型筛选组合框
window.logsChannels = []; // 渠道列表（来自 /admin/models）
window.availableLogsModels = []; // 可用模型列表
let logsDefaultTestContent = 'sonnet 4.0的发布日期是什么'; // 默认测试内容（从设置加载）

const ACTIVE_REQUESTS_POLL_INTERVAL_MS = 2000;
let activeRequestsPollTimer = null;
let activeRequestsFetchInFlight = false;
let lastActiveRequestIDs = null; // 上次活跃请求ID集合（后端原始数据，用于检测完成）
let logsLoadInFlight = false;
let logsLoadPending = false;
let logsLoadScheduled = false;

function scheduleLoad() {
  if (logsLoadScheduled) return;
  logsLoadScheduled = true;
  setTimeout(() => {
    logsLoadScheduled = false;
    load(true); // 自动刷新时跳过 loading 状态，避免闪烁
  }, 0);
}

function toUnixMs(value) {
  if (value === undefined || value === null) return null;

  if (typeof value === 'number' && Number.isFinite(value)) {
    // 兼容：秒(10位) / 毫秒(13位)
    if (value > 1e12) return value;
    if (value > 1e9) return value * 1000;
    return value;
  }

  if (typeof value === 'string') {
    if (/^\d+$/.test(value)) {
      const n = parseInt(value, 10);
      if (!Number.isFinite(n)) return null;
      return n > 1e12 ? n : n * 1000;
    }
    const parsed = Date.parse(value);
    return Number.isNaN(parsed) ? null : parsed;
  }

  return null;
}

// 格式化字节数为可读形式（K/M/G）- 使用对数优化
function formatBytes(bytes) {
  if (bytes == null || bytes <= 0) return '';
  const UNITS = ['B', 'K', 'M', 'G'];
  const FACTOR = 1024;
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(FACTOR)), UNITS.length - 1);
  const value = bytes / Math.pow(FACTOR, i);
  return value.toFixed(i > 0 ? 1 : 0) + ' ' + UNITS[i];
}

// IP 地址掩码处理（隐藏最后两段）
function maskIP(ip) {
  if (!ip) return '';
  // 短地址（如 ::1 localhost）无需掩码
  if (ip.length <= 3) return ip;
  // IPv4: 192.168.1.100 -> 192.168.*.*
  if (ip.includes('.')) {
    const parts = ip.split('.');
    if (parts.length === 4) {
      return `${parts[0]}.${parts[1]}.*.*`;
    }
  }
  // IPv6: 简化处理，保留前两段
  if (ip.includes(':')) {
    const parts = ip.split(':');
    if (parts.length >= 2) {
      return `${parts[0]}:${parts[1]}::*`;
    }
  }
  return ip;
}

function clearActiveRequestsRows() {
  document.querySelectorAll('tr.pending-row').forEach(el => el.remove());
}

function buildChannelTrigger(channelId, channelName, baseURL = '') {
  if (!channelId || !channelName) {
    return '<span style="color: var(--neutral-500);">-</span>';
  }

  const channelTooltip = baseURL ? ` title="${escapeHtml(baseURL)}"` : '';
  return `<button type="button" class="channel-link" data-channel-id="${channelId}"${channelTooltip}>${escapeHtml(channelName)} <small>(#${channelId})</small></button>`;
}

function ensureActiveRequestsPollingStarted() {
  if (activeRequestsPollTimer) return;
  activeRequestsPollTimer = setInterval(async () => {
    if (currentLogsPage !== 1) return;
    await fetchActiveRequests();
  }, ACTIVE_REQUESTS_POLL_INTERVAL_MS);
}
// 生成流式标志HTML（公共函数，避免重复）
function getStreamFlagHtml(isStreaming) {
  return isStreaming
    ? '<span class="stream-flag">流</span>'
    : '<span class="stream-flag placeholder">流</span>';
}

function getLogMobileLabels() {
  return {
    time: escapeHtml(t('logs.colTime')),
    ip: escapeHtml(t('logs.colIP')),
    apiKey: escapeHtml(t('logs.colApiKey')),
    channel: escapeHtml(t('logs.colChannel')),
    model: escapeHtml(t('common.model')),
    status: escapeHtml(t('logs.statusCode')),
    timing: escapeHtml(t('logs.colTiming')),
    speed: escapeHtml(t('logs.colSpeed')),
    input: escapeHtml(t('logs.colInput')),
    output: escapeHtml(t('logs.colOutput')),
    cacheRead: escapeHtml(t('logs.colCacheRead')),
    cacheWrite: escapeHtml(t('logs.colCacheWrite')),
    cost: escapeHtml(t('logs.colCost')),
    message: escapeHtml(t('logs.colMessage'))
  };
}

function renderLogSourceBadge(logSource) {
  switch (logSource) {
    case 'scheduled_check':
      return `<span class="log-source-badge log-source-badge--scheduled">${escapeHtml(t('logs.sourceScheduledCheckBadge'))}</span>`;
    case 'manual_test':
      return `<span class="log-source-badge log-source-badge--manual">${escapeHtml(t('logs.sourceManualTestBadge'))}</span>`;
    default:
      return '';
  }
}

function calculateLogSpeed(entry) {
  const outputTokens = Number(entry?.output_tokens);
  const duration = Number(entry?.duration);
  if (!Number.isFinite(outputTokens) || outputTokens <= 0 || !Number.isFinite(duration) || duration <= 0) {
    return null;
  }
  return outputTokens / duration;
}

// 加载默认测试内容（从系统设置）
async function loadDefaultTestContent() {
  try {
    const setting = await fetchDataWithAuth('/admin/settings/channel_test_content');
    if (setting && setting.value) {
      logsDefaultTestContent = setting.value;
    }
  } catch (e) {
    console.warn('加载默认测试内容失败，使用内置默认值', e);
  }
}

async function load(skipLoading = false) {
  if (logsLoadInFlight) {
    logsLoadPending = true;
    return;
  }
  logsLoadInFlight = true;
  try {
    if (!skipLoading) {
      renderLogsLoading();
    }

    const params = buildLogsRequestParams();
    const response = await fetchAPIWithAuth('/admin/logs?' + params.toString());
    if (!response.success) throw new Error(response.error || '无法加载请求日志');

    const data = response.data || [];

    // 精确计算总页数（基于后端返回的count字段）
    if (typeof response.count === 'number') {
      totalLogs = response.count;
      totalLogsPages = Math.ceil(totalLogs / logsPageSize) || 1;
    } else if (Array.isArray(data)) {
      // 降级方案：后端未返回count时使用旧逻辑
      if (data.length === logsPageSize) {
        totalLogsPages = Math.max(currentLogsPage + 1, totalLogsPages);
      } else if (data.length < logsPageSize && currentLogsPage === 1) {
        totalLogsPages = 1;
      } else if (data.length < logsPageSize) {
        totalLogsPages = currentLogsPage;
      }
    }

    updatePagination();

    // 自动刷新时，保存现有 pending 行以避免闪烁
    const pendingRows = skipLoading ? Array.from(document.querySelectorAll('tr.pending-row')) : [];

    renderLogs(data);

    // 立即恢复 pending 行（后续 fetchActiveRequests 会再更新）
    if (skipLoading && pendingRows.length > 0) {
      const tbody = document.getElementById('tbody');
      const firstRow = tbody.firstChild;
      const fragment = document.createDocumentFragment();
      pendingRows.forEach(row => fragment.appendChild(row));
      tbody.insertBefore(fragment, firstRow);
    }

    updateStats(data);

    // 第一页时获取并显示进行中的请求（并开启轮询，做到真正“实时”）
    if (currentLogsPage === 1) {
      ensureActiveRequestsPollingStarted();
      await fetchActiveRequests();
    } else {
      lastActiveRequestIDs = null;
      clearActiveRequestsRows();
    }

  } catch (error) {
    console.error('加载日志失败:', error);
    try { if (window.showError) window.showError('无法加载请求日志'); } catch (_) { }
    renderLogsError();
  } finally {
    logsLoadInFlight = false;
    if (logsLoadPending) {
      logsLoadPending = false;
      scheduleLoad();
    }
  }
}

// 根据当前筛选条件过滤活跃请求
function filterActiveRequests(requests) {
  const channelName = (logsChannelNameCombobox ? logsChannelNameCombobox.getValue() : (document.getElementById('f_name')?.value || '')).trim().toLowerCase();
  const model = (logsModelCombobox ? logsModelCombobox.getValue() : (document.getElementById('f_model')?.value || '')).trim();
  const channelType = (document.getElementById('f_channel_type')?.value || '').trim();
  const tokenId = (document.getElementById('f_auth_token')?.value || '').trim();

  return requests.filter(req => {
    // 渠道名称精确匹配（来自下拉框）或模糊匹配（手动输入）
    if (channelName) {
      const name = (typeof req.channel_name === 'string' ? req.channel_name : '').toLowerCase();
      if (!name.includes(channelName)) return false;
    }
    // 模型精确匹配（来自下拉框选择）
    if (model) {
      if ((req.model || '') !== model) return false;
    }
    // 渠道类型精确匹配（'all' 表示全部，不过滤）
    if (channelType && channelType !== 'all') {
      const reqType = (typeof req.channel_type === 'string' ? req.channel_type : '').toLowerCase();
      if (reqType !== channelType.toLowerCase()) return false;
    }
    // 令牌ID精确匹配
    if (tokenId) {
      if (req.token_id === undefined || req.token_id === null || req.token_id === 0) return false;
      if (String(req.token_id) !== tokenId) return false;
    }
    return true;
  });
}

function shouldSkipActiveRequestsFetch(hours, status, logSource) {
  if (hours && hours !== 'today') return true;
  if (status) return true;
  return logSource !== 'proxy' && logSource !== 'all';
}

// 获取进行中的请求
async function fetchActiveRequests() {
  if (activeRequestsFetchInFlight) return;

  // 优化：当筛选条件不可能匹配进行中请求时，跳过请求
  const hours = (document.getElementById('f_hours')?.value || '').trim();
  const status = (document.getElementById('f_status')?.value || '').trim();
  const logSource = (document.getElementById('f_log_source')?.value || 'proxy').trim();
  // 进行中的请求只存在于"本日"，且没有状态码
  if (shouldSkipActiveRequestsFetch(hours, status, logSource)) {
    clearActiveRequestsRows();
    lastActiveRequestIDs = null;
    return;
  }

  activeRequestsFetchInFlight = true;
  try {
    const response = await fetchAPIWithAuth('/admin/active-requests');
    const rawActiveRequests = (response.success && Array.isArray(response.data)) ? response.data : [];

    // 检测请求完成：用后端原始ID集合判断“消失的ID”，避免筛选条件变化导致误判
    const currentIDs = new Set();
    for (const req of rawActiveRequests) {
      if (req && (req.id !== undefined && req.id !== null)) {
        currentIDs.add(String(req.id));
      }
    }
    if (lastActiveRequestIDs !== null) {
      let hasCompleted = false;
      for (const id of lastActiveRequestIDs) {
        if (!currentIDs.has(id)) {
          hasCompleted = true;
          break;
        }
      }
      if (hasCompleted && currentLogsPage === 1) {
        scheduleLoad();
      }
    }
    lastActiveRequestIDs = currentIDs;

    // 根据当前筛选条件过滤（只影响展示，不影响完成检测）
    const activeRequests = filterActiveRequests(rawActiveRequests);

    renderActiveRequests(activeRequests);
  } catch (e) {
    // 静默失败，不影响主日志显示
  } finally {
    activeRequestsFetchInFlight = false;
  }
}

// 渲染进行中的请求（插入到表格顶部）
function renderActiveRequests(activeRequests) {
  // 移除旧的进行中行
  clearActiveRequestsRows();

  if (!activeRequests || activeRequests.length === 0) return;

  const tbody = document.getElementById('tbody');
  const firstRow = tbody.firstChild;
  const totalCols = getTableColspan();
  const logMobileLabels = getLogMobileLabels();

  // 使用 DocumentFragment 批量构建，减少 DOM 操作
  const fragment = document.createDocumentFragment();

  for (const req of activeRequests) {
    const startMs = toUnixMs(req.start_time);
    const elapsedRaw = startMs ? Math.max(0, (Date.now() - startMs) / 1000) : null;
    const elapsed = elapsedRaw !== null ? elapsedRaw.toFixed(1) : '-';
    const streamFlag = getStreamFlagHtml(req.is_streaming);

    // 耗时显示：流式请求有首字时间则显示 "首字/总耗时" 格式
    let durationDisplay = startMs ? `${elapsed}s...` : '-';
    if (req.is_streaming && req.client_first_byte_time > 0 && startMs) {
      durationDisplay = `${req.client_first_byte_time.toFixed(2)}s/${elapsed}s...`;
    }

    let channelDisplay = '<span style="color: var(--neutral-500);">选择中...</span>';
    if (req.channel_id && req.channel_name) {
      channelDisplay = buildChannelTrigger(req.channel_id, req.channel_name, req.base_url || '');
    }

    // Key显示
    let keyDisplay = '<span style="color: var(--neutral-500);">-</span>';
    if (req.api_key_used) {
      keyDisplay = `<span class="logs-api-key-text logs-mono-text">${escapeHtml(req.api_key_used)}</span>`;
    }

    const bytesInfo = formatBytes(req.bytes_received);
    const hasBytes = !!bytesInfo;
    const infoDisplay = hasBytes ? `已接收 ${bytesInfo}` : '请求处理中...';
    const infoColor = hasBytes ? 'var(--success-600)' : 'var(--neutral-500)';



    const row = document.createElement('tr');
    row.className = 'pending-row';
    if (totalCols < 8) {
      row.innerHTML = `
            <td colspan="${totalCols}">
              <span class="status-pending">进行中</span>
              <span style="margin-left: 8px;">${formatTime(req.start_time)}</span>
              <span class="logs-mono-text" style="margin-left: 8px;" title="${escapeHtml(req.client_ip || '')}">${escapeHtml(maskIP(req.client_ip) || '-')}</span>
              <span style="margin-left: 8px;">${escapeHtml(req.model || '-')}</span>
              <span style="margin-left: 8px;">${durationDisplay} ${streamFlag}</span>
              <span style="margin-left: 8px; color: ${infoColor};">${escapeHtml(infoDisplay)}</span>
            </td>
          `;
    } else {
      row.innerHTML = `
            <td class="logs-col-time" data-mobile-label="${logMobileLabels.time}" style="white-space: nowrap;">${formatTime(req.start_time)}</td>
            <td class="logs-col-ip logs-mono-text" data-mobile-label="${logMobileLabels.ip}" style="white-space: nowrap;" title="${escapeHtml(req.client_ip || '')}">${escapeHtml(maskIP(req.client_ip) || '-')}</td>
            <td class="logs-col-api-key" data-mobile-label="${logMobileLabels.apiKey}" style="text-align: center; white-space: nowrap;">${keyDisplay}</td>
            <td class="logs-col-channel" data-mobile-label="${logMobileLabels.channel}">${channelDisplay}</td>
            <td class="logs-col-model" data-mobile-label="${logMobileLabels.model}"><span class="model-tag">${escapeHtml(req.model || '-')}</span></td>
            <td class="logs-col-status" data-mobile-label="${logMobileLabels.status}"><span class="status-pending">进行中</span></td>
            <td class="logs-col-timing" data-mobile-label="${logMobileLabels.timing}" style="text-align: right; white-space: nowrap;">${durationDisplay} ${streamFlag}</td>
            <td class="logs-col-speed mobile-empty-cell" data-mobile-label="${logMobileLabels.speed}" style="text-align: right; white-space: nowrap;"></td>
            <td class="logs-col-input mobile-empty-cell" data-mobile-label="${logMobileLabels.input}" style="text-align: right; white-space: nowrap;"></td>
            <td class="logs-col-output mobile-empty-cell" data-mobile-label="${logMobileLabels.output}" style="text-align: right; white-space: nowrap;"></td>
            <td class="logs-col-cache-read mobile-empty-cell" data-mobile-label="${logMobileLabels.cacheRead}" style="text-align: right; white-space: nowrap;"></td>
            <td class="logs-col-cache-write mobile-empty-cell" data-mobile-label="${logMobileLabels.cacheWrite}" style="text-align: right; white-space: nowrap;"></td>
            <td class="logs-col-cost mobile-empty-cell" data-mobile-label="${logMobileLabels.cost}" style="text-align: right; white-space: nowrap;"></td>
            <td class="logs-col-message" data-mobile-label="${logMobileLabels.message}"><span style="color: ${infoColor};">${escapeHtml(infoDisplay)}</span></td>
          `;
    }
    fragment.appendChild(row);
  }

  // 一次性插入所有 pending 行
  tbody.insertBefore(fragment, firstRow);
}

// ✅ 动态计算列数（避免硬编码维护成本）
function getTableColspan() {
  const table = document.getElementById('tbody')?.closest('table')
    || document.querySelector('.logs-table');
  const headerCells = table ? table.querySelectorAll('thead th') : [];
  return headerCells.length || 14; // fallback到14列（日志页默认列数）
}

function renderLogsLoading() {
  const tbody = document.getElementById('tbody');
  const colspan = getTableColspan();
  const loadingRow = TemplateEngine.render('tpl-log-loading', { colspan });
  tbody.innerHTML = '';
  if (loadingRow) tbody.appendChild(loadingRow);
}

function renderLogsError() {
  const tbody = document.getElementById('tbody');
  const colspan = getTableColspan();
  const errorRow = TemplateEngine.render('tpl-log-error', { colspan });
  tbody.innerHTML = '';
  if (errorRow) tbody.appendChild(errorRow);
}

function renderLogs(data) {
  const tbody = document.getElementById('tbody');
  const colspan = getTableColspan();
  const logMobileLabels = getLogMobileLabels();

  if (data.length === 0) {
    const emptyRow = TemplateEngine.render('tpl-log-empty', { colspan });
    tbody.innerHTML = '';
    if (emptyRow) tbody.appendChild(emptyRow);
    return;
  }

  // 性能优化：直接拼接 HTML 字符串，避免逐行调用 TemplateEngine.render
  const htmlParts = new Array(data.length);

  for (let i = 0; i < data.length; i++) {
    const entry = data[i];
    // === 预处理数据：构建复杂HTML片段 ===

    // 0. 客户端IP显示（掩码处理，hover显示完整IP）
    const clientIPDisplay = entry.client_ip ?
      `<span title="${escapeHtml(entry.client_ip)}">${escapeHtml(maskIP(entry.client_ip))}</span>` :
      '<span style="color: var(--neutral-400);">-</span>';

    // 1. 渠道信息显示（鼠标移上去时显示URL）
    const configInfo = entry.channel_name ||
      (entry.channel_id ? `渠道 #${entry.channel_id}` :
        (entry.message === 'exhausted backends' ? '系统（所有渠道失败）' :
          entry.message === 'no available upstream (all cooled or none)' ? '系统（无可用渠道）' : '系统'));
    const channelTooltip = entry.base_url ? ` title="${escapeHtml(entry.base_url)}"` : '';
    const configDisplay = entry.channel_id ?
      buildChannelTrigger(entry.channel_id, entry.channel_name || '', entry.base_url || '') :
      `<span style="color: var(--neutral-500);"${channelTooltip}>${escapeHtml(configInfo)}</span>`;

    // 2. 状态码样式
    const statusClass = (entry.status_code >= 200 && entry.status_code < 300) ?
      'status-success' : 'status-error';
    const statusCode = entry.status_code;

    // 3. 模型显示（支持重定向角标）
    let modelDisplay;
    if (entry.model) {
      if (entry.actual_model && entry.actual_model !== entry.model) {
        // 有重定向：显示角标 + tooltip
        modelDisplay = `<span class="model-tag model-redirected" title="请求模型: ${escapeHtml(entry.model)}&#10;实际模型: ${escapeHtml(entry.actual_model)}">
              <span class="model-text">${escapeHtml(entry.model)}</span>
              <sup class="redirect-badge">↪</sup>
            </span>`;
      } else {
        modelDisplay = `<span class="model-tag">${escapeHtml(entry.model)}</span>`;
      }
    } else {
      modelDisplay = '<span style="color: var(--neutral-500);">-</span>';
    }

    // 4. 响应时间显示(流式/非流式)
    const hasDuration = entry.duration !== undefined && entry.duration !== null;
    const durationDisplay = hasDuration ?
      `<span style="color: var(--neutral-700);">${entry.duration.toFixed(2)}</span>` :
      '<span style="color: var(--neutral-500);">-</span>';

    const streamFlag = getStreamFlagHtml(entry.is_streaming);

    let responseTimingDisplay;
    if (entry.is_streaming) {
      const hasFirstByte = entry.first_byte_time !== undefined && entry.first_byte_time !== null;
      const firstByteDisplay = hasFirstByte ?
        `<span class="log-timing-first-byte" style="color: var(--success-600);">${entry.first_byte_time.toFixed(2)}</span>` :
        '<span class="log-timing-first-byte" style="color: var(--neutral-500);">-</span>';
      responseTimingDisplay = `<span class="log-timing-pair">${firstByteDisplay}<span class="log-timing-separator" style="color: var(--neutral-400);">/</span><span class="log-timing-duration">${durationDisplay}</span></span>${streamFlag}`;
    } else {
      responseTimingDisplay = `<span class="log-timing-pair"><span class="log-timing-duration">${durationDisplay}</span></span>${streamFlag}`;
    }

    const logSpeed = calculateLogSpeed(entry);
    const speedDisplay = logSpeed === null
      ? ''
      : `<span class="token-metric-value" style="color: var(--neutral-700);">${logSpeed.toFixed(1)}</span>`;

    // 5. API Key显示(含按钮组)
    let apiKeyDisplay = '';
    if (entry.api_key_used && entry.channel_id && entry.model) {
      const sc = entry.status_code || 0;
      const showTestBtn = sc !== 200;
      const showDeleteBtn = sc === 401 || sc === 403;
      const keyHashAttr = escapeHtml(entry.api_key_hash || '').replace(/"/g, '&quot;');

      const testBtnIcon = `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" xmlns="http://www.w3.org/2000/svg" aria-hidden="true" focusable="false"><path d="M13 2L4 14H11L9 22L20 10H13L13 2Z" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/></svg>`;
      const deleteBtnIcon = `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" xmlns="http://www.w3.org/2000/svg" aria-hidden="true" focusable="false"><path d="M3 6H21" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/><path d="M8 6V4H16V6" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/><path d="M19 6L18 20H6L5 6" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/><path d="M10 11V17" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/><path d="M14 11V17" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/></svg>`;
      let buttons = '';
      if (showTestBtn) {
        buttons += `<button class="test-key-btn" data-action="test" data-channel-id="${entry.channel_id}" data-channel-name="${escapeHtml(entry.channel_name || '').replace(/"/g, '&quot;')}" data-api-key="${escapeHtml(entry.api_key_used).replace(/"/g, '&quot;')}" data-api-key-hash="${keyHashAttr}" data-model="${escapeHtml(entry.model).replace(/"/g, '&quot;')}" title="测试此 API Key">${testBtnIcon}</button>`;
      }
      if (showDeleteBtn) {
        buttons += `<button class="test-key-btn" style="color: var(--error-600);" data-action="delete" data-channel-id="${entry.channel_id}" data-channel-name="${escapeHtml(entry.channel_name || '').replace(/"/g, '&quot;')}" data-api-key="${escapeHtml(entry.api_key_used).replace(/"/g, '&quot;')}" data-api-key-hash="${keyHashAttr}" title="删除此 API Key">${deleteBtnIcon}</button>`;
      }

      apiKeyDisplay = `<div class="logs-api-key-group"><code class="logs-api-key-text logs-mono-text">${escapeHtml(entry.api_key_used)}</code><span class="logs-api-key-actions">${buttons}</span></div>`;
    } else if (entry.api_key_used) {
      apiKeyDisplay = `<code class="logs-api-key-text logs-mono-text">${escapeHtml(entry.api_key_used)}</code>`;
    } else {
      apiKeyDisplay = '<span style="color: var(--neutral-500);">-</span>';
    }

    // 6. Token统计显示(0值为空)
    const tokenValue = (value, color) => {
      if (value === undefined || value === null || value === 0) return '';
      return `<span class="token-metric-value" style="color: ${color};">${value.toLocaleString()}</span>`;
    };
    const inputTokensDisplay = tokenValue(entry.input_tokens, 'var(--neutral-700)');
    const outputTokensDisplay = tokenValue(entry.output_tokens, 'var(--neutral-700)');
    const cacheReadDisplay = tokenValue(entry.cache_read_input_tokens, 'var(--success-600)');

    // 缓存建列
    let cacheCreationDisplay = '';
    const total = entry.cache_creation_input_tokens || 0;
    const cache5m = entry.cache_5m_input_tokens || 0;
    const cache1h = entry.cache_1h_input_tokens || 0;

    if (total > 0) {
      const model = (entry.model || '').toLowerCase();
      const isClaudeOrCodex = model.includes('claude') || model.includes('codex');

      let badge = '';
      if (isClaudeOrCodex && (cache5m > 0 || cache1h > 0)) {
        if (cache5m > 0 && cache1h === 0) {
          badge = ' <sup style="color: var(--primary-500); font-size: 0.75em; font-weight: 600;">5m</sup>';
        } else if (cache1h > 0 && cache5m === 0) {
          badge = ' <sup style="color: var(--warning-600); font-size: 0.75em; font-weight: 600;">1h</sup>';
        } else if (cache5m > 0 && cache1h > 0) {
          badge = ' <sup style="color: var(--primary-500); font-size: 0.75em; font-weight: 600;">5m</sup><sup style="color: var(--warning-600); font-size: 0.75em; font-weight: 600;">+1h</sup>';
        }
      }
      cacheCreationDisplay = `<span class="token-metric-value" style="color: var(--primary-600);">${total.toLocaleString()}${badge}</span>`;
    }

    // 7. 成本显示
    let tierBadge = '';
    if (entry.service_tier === 'priority') {
      tierBadge = ' <sup style="color: var(--error-600); font-size: 0.7em; font-weight: 600;">2x</sup>';
    } else if (entry.service_tier === 'flex') {
      tierBadge = ' <sup style="color: var(--success-600); font-size: 0.7em; font-weight: 600;">0.5x</sup>';
    } else if (entry.service_tier === 'fast') {
      tierBadge = ' <sup style="color: var(--error-600); font-size: 0.7em; font-weight: 600;">\u26A16x</sup>';
    }
    const costDisplay = entry.cost ?
      `<span style="color: var(--warning-600); font-weight: 500;">${formatCost(entry.cost)}${tierBadge}</span>` : '';
    const sourceBadge = renderLogSourceBadge(entry.log_source || 'proxy');
    const messageText = escapeHtml(entry.message || '');
    const messageDisplay = `${sourceBadge}${messageText}`;

    // === 直接拼接行 HTML ===
    htmlParts[i] = `<tr class="mobile-card-row logs-table-row">
          <td class="logs-col-time" data-mobile-label="${logMobileLabels.time}" style="white-space: nowrap;">${formatTime(entry.time)}</td>
          <td class="logs-col-ip logs-mono-text" data-mobile-label="${logMobileLabels.ip}" style="white-space: nowrap;">${clientIPDisplay}</td>
          <td class="logs-col-api-key" data-mobile-label="${logMobileLabels.apiKey}" style="text-align: center; white-space: nowrap;">${apiKeyDisplay}</td>
          <td class="logs-col-channel" data-mobile-label="${logMobileLabels.channel}">${configDisplay}</td>
          <td class="logs-col-model" data-mobile-label="${logMobileLabels.model}">${modelDisplay}</td>
          <td class="logs-col-status" data-mobile-label="${logMobileLabels.status}"><span class="${statusClass}">${statusCode}</span></td>
          <td class="logs-col-timing" data-mobile-label="${logMobileLabels.timing}" style="text-align: right; white-space: nowrap;">${responseTimingDisplay}</td>
          <td class="logs-col-speed${speedDisplay ? '' : ' mobile-empty-cell'}" data-mobile-label="${logMobileLabels.speed}" style="text-align: right; white-space: nowrap;">${speedDisplay}</td>
          <td class="logs-col-input${inputTokensDisplay ? '' : ' mobile-empty-cell'}" data-mobile-label="${logMobileLabels.input}" style="text-align: right; white-space: nowrap;">${inputTokensDisplay}</td>
          <td class="logs-col-output${outputTokensDisplay ? '' : ' mobile-empty-cell'}" data-mobile-label="${logMobileLabels.output}" style="text-align: right; white-space: nowrap;">${outputTokensDisplay}</td>
          <td class="logs-col-cache-read${cacheReadDisplay ? '' : ' mobile-empty-cell'}" data-mobile-label="${logMobileLabels.cacheRead}" style="text-align: right; white-space: nowrap;">${cacheReadDisplay}</td>
          <td class="logs-col-cache-write${cacheCreationDisplay ? '' : ' mobile-empty-cell'}" data-mobile-label="${logMobileLabels.cacheWrite}" style="text-align: right; white-space: nowrap;">${cacheCreationDisplay}</td>
          <td class="logs-col-cost${costDisplay ? '' : ' mobile-empty-cell'}" data-mobile-label="${logMobileLabels.cost}" style="text-align: right; white-space: nowrap;">${costDisplay}</td>
          <td class="logs-col-message${messageDisplay ? '' : ' mobile-empty-cell'}" data-mobile-label="${logMobileLabels.message}" style="max-width: 300px; word-break: break-word;"><span class="debug-log-link has-upstream-detail" data-log-id="${entry.id}">${messageDisplay}</span></td>
        </tr>`;
  }

  // 一次性替换 tbody 内容
  tbody.innerHTML = htmlParts.join('');
}

function updatePagination() {
  // 更新页码显示（只更新底部分页）
  const currentPage2El = document.getElementById('logs_current_page2');
  const totalPages2El = document.getElementById('logs_total_pages2');
  const first2El = document.getElementById('logs_first2');
  const prev2El = document.getElementById('logs_prev2');
  const next2El = document.getElementById('logs_next2');
  const last2El = document.getElementById('logs_last2');
  const jumpPageInput = document.getElementById('logs_jump_page');

  if (currentPage2El) currentPage2El.textContent = currentLogsPage;
  if (totalPages2El) totalPages2El.textContent = totalLogsPages;

  // 更新跳转输入框的max属性
  if (jumpPageInput) {
    jumpPageInput.max = totalLogsPages;
    jumpPageInput.placeholder = `1-${totalLogsPages}`;
  }

  // 更新按钮状态（只更新底部分页）
  const prevDisabled = currentLogsPage <= 1;
  const nextDisabled = currentLogsPage >= totalLogsPages;

  if (first2El) first2El.disabled = prevDisabled;
  if (prev2El) prev2El.disabled = prevDisabled;
  if (next2El) next2El.disabled = nextDisabled;
  if (last2El) last2El.disabled = nextDisabled;
}

function updateStats(data) {
  // 更新筛选器统计信息
  const displayedCountEl = document.getElementById('displayedCount');
  const totalCountEl = document.getElementById('totalCount');

  if (displayedCountEl) displayedCountEl.textContent = data.length;
  if (totalCountEl) totalCountEl.textContent = totalLogs || data.length;
}

function firstLogsPage() {
  if (currentLogsPage > 1) {
    currentLogsPage = 1;
    load();
  }
}

function prevLogsPage() {
  if (currentLogsPage > 1) {
    currentLogsPage--;
    load();
  }
}

function nextLogsPage() {
  if (currentLogsPage < totalLogsPages) {
    currentLogsPage++;
    load();
  }
}

function lastLogsPage() {
  if (currentLogsPage < totalLogsPages) {
    currentLogsPage = totalLogsPages;
    load();
  }
}

function jumpToPage() {
  const jumpPageInput = document.getElementById('logs_jump_page');
  if (!jumpPageInput) return;

  const targetPage = parseInt(jumpPageInput.value);

  // 输入验证
  if (isNaN(targetPage) || targetPage < 1 || targetPage > totalLogsPages) {
    jumpPageInput.value = ''; // 清空无效输入
    if (window.showError) {
      try {
        window.showError(`请输入有效的页码 (1-${totalLogsPages})`);
      } catch (_) { }
    }
    return;
  }

  // 跳转到目标页
  if (targetPage !== currentLogsPage) {
    currentLogsPage = targetPage;
    load();
  }

  // 清空输入框
  jumpPageInput.value = '';
}

function changePageSize() {
  const newPageSize = parseInt(document.getElementById('page_size').value);
  if (newPageSize !== logsPageSize) {
    logsPageSize = newPageSize;
    currentLogsPage = 1;
    totalLogsPages = 1;
    load();
  }
}

function applyFilter() {
  currentLogsPage = 1;
  totalLogsPages = 1;

  window.persistFilterState({
    key: LOGS_FILTER_KEY,
    values: getLogsFilters(),
    search: location.search,
    pathname: location.pathname,
    fields: LOGS_FILTER_FIELDS,
    preserveExistingParams: true
  });
  load();
}

function applyLogsFilterValues(filters) {
  window.applyFilterControlValues(filters, {
    logSource: 'f_log_source',
    status: 'f_status'
  });

  // 渠道名通过 combobox 恢复
  if (logsChannelNameCombobox && filters.channelName !== undefined) {
    logsChannelNameCombobox.setValue(filters.channelName || '', filters.channelName || t('stats.allChannels'));
  }

  // 模型通过 combobox 恢复
  if (logsModelCombobox && filters.model !== undefined) {
    logsModelCombobox.setValue(filters.model || '', filters.model || t('trend.allModels'));
  }

  currentChannelType = filters.channelType || 'all';
  const channelTypeEl = document.getElementById('f_channel_type');
  if (channelTypeEl) channelTypeEl.value = currentChannelType;
}

function getLogSourceFilterElements() {
  const select = document.getElementById('f_log_source');
  if (!select) {
    return { group: null, select: null };
  }

  let group = null;
  if (typeof select.closest === 'function') {
    group = select.closest('.filter-group');
  }
  if (!group) {
    group = select.parentElement || null;
  }

  return { group, select };
}

async function syncLogSourceVisibility() {
  const { group, select } = getLogSourceFilterElements();
  if (!group || !select) return false;

  let scheduledCheckEnabledByConfig = false;
  try {
    const setting = await fetchDataWithAuth('/admin/settings/channel_check_interval_hours');
    const intervalHours = Number(setting && setting.value);
    scheduledCheckEnabledByConfig = Number.isFinite(intervalHours) && intervalHours > 0;
  } catch (error) {
    console.warn('Failed to load channel check interval setting for logs filter', error);
  }

  group.hidden = !scheduledCheckEnabledByConfig;
  if (!scheduledCheckEnabledByConfig) {
    select.value = 'proxy';
  }
  return scheduledCheckEnabledByConfig;
}

async function loadLogsModels(channelType, range) {
  try {
    const params = new URLSearchParams();
    const ct = channelType || currentChannelType || 'all';
    const r = range || document.getElementById('f_hours')?.value || 'today';
    params.set('range', r);
    if (ct && ct !== 'all') params.set('channel_type', ct);
    const resp = await fetchDataWithAuth('/admin/models?' + params.toString()) || {};
    const rawModels = Array.isArray(resp.models) ? resp.models : [];
    const rawChannels = Array.isArray(resp.channels) ? resp.channels : [];

    window.availableLogsModels = [...new Set(rawModels)];
    window.logsChannels = rawChannels;
    if (logsChannelNameCombobox) logsChannelNameCombobox.refresh();
    if (logsModelCombobox) logsModelCombobox.refresh();
  } catch (error) {
    console.error('加载模型列表失败:', error);
  }
}

function initLogsChannelNameCombobox(initialValue) {
  if (typeof window.createSearchableCombobox !== 'function') return;
  if (!document.getElementById('f_name')) return;
  logsChannelNameCombobox = window.createSearchableCombobox({
    inputId: 'f_name',
    dropdownId: 'f_name_dropdown',
    attachMode: true,
    initialValue: initialValue || '',
    initialLabel: initialValue || t('stats.allChannels'),
    getOptions: () => [
      { value: '', label: t('stats.allChannels') },
      ...(window.logsChannels || []).map(ch => ({ value: ch.name, label: ch.name }))
    ],
    onSelect: () => {
      applyFilter();
    }
  });
}

function initLogsModelCombobox(initialValue) {
  if (typeof window.createSearchableCombobox !== 'function') return;
  if (!document.getElementById('f_model')) return;
  logsModelCombobox = window.createSearchableCombobox({
    inputId: 'f_model',
    dropdownId: 'f_model_dropdown',
    attachMode: true,
    initialValue: initialValue || '',
    initialLabel: initialValue || t('trend.allModels'),
    getOptions: () => [
      { value: '', label: t('trend.allModels') },
      ...(window.availableLogsModels || []).map(m => ({ value: m, label: m }))
    ],
    onSelect: () => {
      applyFilter();
    }
  });
}

async function initFilters(restoredFilters) {
  const range = restoredFilters.range || 'today';
  const authToken = restoredFilters.authToken || '';

  window.initSavedDateRangeFilter({
    selectId: 'f_hours',
    defaultValue: 'today',
    restoredValue: range,
    onChange: async () => {
      window.persistFilterState({
        key: LOGS_FILTER_KEY,
        getValues: getLogsFilters
      });
      currentLogsPage = 1;
      await loadLogsModels(currentChannelType);
      load();
    }
  });

  initLogsChannelNameCombobox(restoredFilters.channelName || '');
  initLogsModelCombobox(restoredFilters.model || '');
  applyLogsFilterValues(restoredFilters);
  await syncLogSourceVisibility();

  authTokens = await window.initAuthTokenFilter({
    selectId: 'f_auth_token',
    value: authToken,
    onChange: () => {
      window.persistFilterState({
        key: LOGS_FILTER_KEY,
        getValues: getLogsFilters
      });
      currentLogsPage = 1;
      load();
    }
  });

  await loadLogsModels(currentChannelType, range);

  // 事件监听
  document.getElementById('btn_filter').addEventListener('click', applyFilter);
  document.getElementById('f_log_source')?.addEventListener('change', applyFilter);

  window.bindFilterApplyInputs({
    apply: applyFilter,
    debounceInputIds: ['f_status'],
    enterInputIds: ['f_hours', 'f_status', 'f_auth_token', 'f_channel_type', 'f_log_source']
  });
}

function initLogsPageActions() {
  if (typeof window.initDelegatedActions === 'function') {
    window.initDelegatedActions({
      boundKey: 'logsPageActionsBound',
      click: {
        'first-logs-page': () => firstLogsPage(),
        'prev-logs-page': () => prevLogsPage(),
        'next-logs-page': () => nextLogsPage(),
        'last-logs-page': () => lastLogsPage(),
        'close-test-key-modal': () => closeTestKeyModal(),
        'close-debug-log-modal': () => closeDebugLogModal(),
        'run-key-test': () => runKeyTest(),
        'toggle-response': (actionTarget) => {
          const responseTarget = actionTarget.dataset.responseTarget;
          if (responseTarget && typeof window.toggleResponse === 'function') {
            window.toggleResponse(responseTarget);
          }
        }
      }
    });
  }

  const jumpPageInput = document.getElementById('logs_jump_page');
  if (jumpPageInput && !jumpPageInput.dataset.bound) {
    jumpPageInput.addEventListener('keydown', (event) => {
      if (event.key === 'Enter') {
        jumpToPage();
      }
    });
    jumpPageInput.dataset.bound = '1';
  }
}

// 性能优化：避免 toLocaleString 的开销，使用手动格式化
function formatTime(timeStr) {
  try {
    const ts = toUnixMs(timeStr);
    if (!ts) return '-';

    const d = new Date(ts);
    if (isNaN(d.getTime()) || d.getFullYear() < 2020) {
      return '-';
    }

    // 手动格式化：MM-DD HH:mm:ss
    const M = String(d.getMonth() + 1).padStart(2, '0');
    const D = String(d.getDate()).padStart(2, '0');
    const h = String(d.getHours()).padStart(2, '0');
    const m = String(d.getMinutes()).padStart(2, '0');
    const s = String(d.getSeconds()).padStart(2, '0');
    return `${M}-${D} ${h}:${m}:${s}`;
  } catch (e) {
    return '-';
  }
}

const apiKeyHashCache = new Map();

function maskKeyForCompare(key) {
  if (!key) return '';
  if (key.length <= 8) return '****';
  return `${key.slice(0, 4)}...${key.slice(-4)}`;
}

function findKeyIndexCandidatesByMaskedKey(apiKeys, maskedKey) {
  if (!maskedKey || !apiKeys || !apiKeys.length) return [];
  const target = maskedKey.trim();
  const candidates = [];

  for (const k of apiKeys) {
    const rawKey = (k && (k.api_key || k.key)) || '';
    if (maskKeyForCompare(rawKey) !== target) continue;
    if (k && typeof k.key_index === 'number') {
      candidates.push(k.key_index);
    }
  }

  return candidates;
}

function findUniqueKeyIndexByMaskedKey(apiKeys, maskedKey) {
  const candidates = findKeyIndexCandidatesByMaskedKey(apiKeys, maskedKey);
  if (candidates.length !== 1) {
    return { keyIndex: null, matchCount: candidates.length };
  }

  return { keyIndex: candidates[0], matchCount: 1 };
}

async function sha256Hex(value) {
  if (!value) return '';
  const key = `sha256:${value}`;
  if (apiKeyHashCache.has(key)) {
    return apiKeyHashCache.get(key);
  }

  const canHash = typeof crypto !== 'undefined' && crypto.subtle && typeof TextEncoder !== 'undefined';
  if (!canHash) return '';

  try {
    const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(value));
    const hex = Array.from(new Uint8Array(digest))
      .map(b => b.toString(16).padStart(2, '0'))
      .join('');
    apiKeyHashCache.set(key, hex);
    return hex;
  } catch (err) {
    console.warn('计算 API Key 哈希失败，将回退掩码匹配:', err);
    return '';
  }
}

async function findUniqueKeyIndexByHash(apiKeys, apiKeyHash) {
  if (!apiKeyHash || !apiKeys || !apiKeys.length) {
    return { keyIndex: null, matchCount: 0 };
  }

  const target = apiKeyHash.trim().toLowerCase();
  const candidates = [];

  for (const k of apiKeys) {
    const rawKey = (k && (k.api_key || k.key)) || '';
    if (!rawKey) continue;
    const hashed = await sha256Hex(rawKey);
    if (!hashed || hashed !== target) continue;
    if (k && typeof k.key_index === 'number') {
      candidates.push(k.key_index);
    }
  }

  if (candidates.length !== 1) {
    return { keyIndex: null, matchCount: candidates.length };
  }
  return { keyIndex: candidates[0], matchCount: 1 };
}

async function resolveKeyIndexForLogEntry(apiKeys, maskedKey, apiKeyHash) {
  if (apiKeyHash) {
    const byHash = await findUniqueKeyIndexByHash(apiKeys, apiKeyHash);
    if (byHash.keyIndex !== null || byHash.matchCount > 1) {
      return { ...byHash, method: 'hash' };
    }
  }

  const byMask = findUniqueKeyIndexByMaskedKey(apiKeys, maskedKey);
  return { ...byMask, method: 'mask' };
}

function updateTestKeyIndexInfo(text) {
  const el = document.getElementById('testKeyIndexInfo');
  if (el) el.textContent = text || '';
}

// 注销功能（已由 ui.js 的 onLogout 统一处理）

// localStorage key for logs page filters
const LOGS_FILTER_KEY = 'logs.filters';
const LOGS_FILTER_FIELDS = [
  { key: 'range', queryKeys: ['range'], defaultValue: 'today' },
  { key: 'channelName', queryKeys: ['channel_name_like', 'channel_name'], defaultValue: '' },
  { key: 'model', queryKeys: ['model'], defaultValue: '' },
  { key: 'logSource', queryKeys: ['log_source'], requestKey: 'log_source', defaultValue: 'proxy' },
  { key: 'status', queryKeys: ['status_code'], defaultValue: '' },
  { key: 'authToken', queryKeys: ['auth_token_id'], defaultValue: '' },
  {
    key: 'channelType',
    queryKeys: ['channel_type'],
    defaultValue: 'all',
    includeInQuery(value) {
      return Boolean(value) && value !== 'all';
    },
    includeInRequest(value) {
      return Boolean(value) && value !== 'all';
    }
  }
];

function getLogsFilters() {
  const { group: logSourceGroup, select: logSourceSelect } = getLogSourceFilterElements();
  const logSource = !logSourceSelect || (logSourceGroup && logSourceGroup.hidden)
    ? 'proxy'
    : (logSourceSelect.value || 'proxy').trim();

  return {
    ...window.readFilterControlValues({
      range: { id: 'f_hours', defaultValue: 'today', trim: true },
      status: { id: 'f_status', trim: true },
      authToken: { id: 'f_auth_token', trim: true }
    }),
    model: logsModelCombobox ? logsModelCombobox.getValue() : (document.getElementById('f_model')?.value || '').trim(),
    channelName: logsChannelNameCombobox ? logsChannelNameCombobox.getValue() : (document.getElementById('f_name')?.value || '').trim(),
    logSource,
    channelType: document.getElementById('f_channel_type')?.value || 'all',
  };
}

function buildLogsRequestParams() {
  return window.FilterQuery.buildRequestParams(getLogsFilters(), LOGS_FILTER_FIELDS, {
    baseParams: {
      limit: logsPageSize.toString(),
      offset: ((currentLogsPage - 1) * logsPageSize).toString()
    }
  });
}

// 页面初始化
window.initPageBootstrap({
  topbarKey: 'logs',
  run: async () => {
  initLogsPageActions();

  // 优先从 URL 读取，其次从 localStorage 恢复，默认 all
  const u = new URLSearchParams(location.search);
  const hasUrlParams = u.toString().length > 0;
  const savedFilters = window.FilterState.load(LOGS_FILTER_KEY);
  const restoredFilters = window.FilterState.restore({
    search: location.search,
    savedFilters,
    fields: LOGS_FILTER_FIELDS
  });
  currentChannelType = restoredFilters.channelType || 'all';

  // 并行初始化：渠道类型 + 默认测试内容同时加载（节省一次 RTT）
  await Promise.all([
    window.initChannelTypeFilter('f_channel_type', currentChannelType, async (value) => {
      currentChannelType = value;
      window.persistFilterState({
        key: LOGS_FILTER_KEY,
        getValues: getLogsFilters
      });
      currentLogsPage = 1;
      await loadLogsModels(value);
      load();
    }),
    loadDefaultTestContent()
  ]);

  await initFilters(restoredFilters);

  if (!hasUrlParams && savedFilters) {
    window.persistFilterState({
      values: getLogsFilters(),
      pathname: location.pathname,
      fields: LOGS_FILTER_FIELDS,
      historyMethod: 'replaceState'
    });
  }

  load();

  // 页面可见性变化时暂停/恢复轮询（减少 HF 等高延迟环境的无效请求）
  document.addEventListener('visibilitychange', () => {
    if (document.hidden) {
      if (activeRequestsPollTimer) {
        clearInterval(activeRequestsPollTimer);
        activeRequestsPollTimer = null;
      }
    } else if (currentLogsPage === 1) {
      ensureActiveRequestsPollingStarted();
      fetchActiveRequests();
    }
  });

  // ESC键关闭模态框
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      const debugModal = document.getElementById('debugLogModal');
      if (debugModal && debugModal.classList.contains('show')) {
        closeDebugLogModal();
        return;
      }
      closeTestKeyModal();
    }
  });

  // 事件委托：处理日志表格中的按钮点击
  const tbody = document.getElementById('tbody');
  if (tbody) {
    tbody.addEventListener('click', (e) => {
      // Debug log 查看
      const debugLink = e.target.closest('.debug-log-link[data-log-id]');
      if (debugLink) {
        const logId = parseInt(debugLink.dataset.logId, 10);
        if (Number.isFinite(logId) && logId > 0) {
          showDebugLogModal(logId);
        }
        return;
      }

      const channelBtn = e.target.closest('.channel-link[data-channel-id]');
      if (channelBtn) {
        const channelId = parseInt(channelBtn.dataset.channelId, 10);
        if (Number.isFinite(channelId) && channelId > 0 && typeof openLogChannelEditor === 'function') {
          openLogChannelEditor(channelId);
        }
        return;
      }

      const btn = e.target.closest('.test-key-btn[data-action]');
      if (!btn) return;

      const action = btn.dataset.action;
      const channelId = parseInt(btn.dataset.channelId);
      const channelName = btn.dataset.channelName || '';
      const apiKey = btn.dataset.apiKey || '';
      const apiKeyHash = btn.dataset.apiKeyHash || '';
      const model = btn.dataset.model || '';

      if (action === 'test') {
        testKey(channelId, channelName, apiKey, model, apiKeyHash);
      } else if (action === 'delete') {
        deleteKeyFromLog(channelId, channelName, apiKey, apiKeyHash);
      }
    });
  }
  }
});

// 处理 bfcache（后退/前进缓存）：页面从缓存恢复时重新加载筛选条件
window.addEventListener('pageshow', async function (event) {
  if (event.persisted) {
    // 页面从 bfcache 恢复，重新同步筛选器状态
    const savedFilters = window.FilterState.load(LOGS_FILTER_KEY);
    if (savedFilters) {
      const restoredFilters = window.FilterState.restore({
        search: '',
        savedFilters,
        fields: LOGS_FILTER_FIELDS
      });

      // 重新加载令牌列表并设置值
      authTokens = await window.loadAuthTokensIntoSelect('f_auth_token');
      if (restoredFilters.authToken) {
        document.getElementById('f_auth_token').value = restoredFilters.authToken;
      }

      document.getElementById('f_hours').value = restoredFilters.range || 'today';
      await loadLogsModels(restoredFilters.channelType || 'all', restoredFilters.range || 'today');
      applyLogsFilterValues(restoredFilters);
      await syncLogSourceVisibility();

      // 重新加载数据
      currentLogsPage = 1;
      load();
    }
  }
});

// ========== API Key 测试功能 ==========
let testingKeyData = null;

async function testKey(channelId, channelName, apiKey, model, apiKeyHash = '') {
  testingKeyData = {
    channelId,
    channelName,
    maskedApiKey: apiKey,
    apiKeyHash,
    originalModel: model,
    channelType: null, // 将在异步加载渠道配置后填充
    keyIndex: null
  };

  // 填充模态框基本信息
  document.getElementById('testKeyChannelName').textContent = channelName;
  document.getElementById('testKeyDisplay').textContent = apiKey;
  document.getElementById('testKeyOriginalModel').textContent = model;

  // 重置状态
  resetTestKeyModal();
  updateTestKeyIndexInfo('');

  // 显示模态框
  document.getElementById('testKeyModal').classList.add('show');

  // 异步加载渠道配置以获取支持的模型列表 + Keys 用于 key_index 匹配
  try {
    const [channel, apiKeysRaw] = await Promise.all([
      fetchDataWithAuth(`/admin/channels/${channelId}`),
      fetchDataWithAuth(`/admin/channels/${channelId}/keys`)
    ]);
    const apiKeys = apiKeysRaw || [];

    // ✅ 保存渠道类型,用于后续测试请求
    testingKeyData.channelType = channel.channel_type || 'anthropic';
    const { keyIndex: matchedIndex, matchCount, method } = await resolveKeyIndexForLogEntry(apiKeys, apiKey, apiKeyHash);
    testingKeyData.keyIndex = matchedIndex;
    if (apiKeys.length > 0) {
      updateTestKeyIndexInfo(
        matchedIndex !== null
          ? method === 'hash'
            ? `匹配到 Key #${matchedIndex + 1}（哈希精确匹配），按日志所用Key测试`
            : `匹配到 Key #${matchedIndex + 1}（掩码匹配），按日志所用Key测试`
          : matchCount > 1
            ? method === 'hash'
              ? `匹配到 ${matchCount} 个哈希相同 Key，已回退默认顺序测试`
              : `匹配到 ${matchCount} 个同掩码 Key，为避免误测将按默认顺序测试`
            : '未匹配到日志中的 Key，将按默认顺序测试'
      );
    } else {
      updateTestKeyIndexInfo('未获取到渠道 Key，将按默认顺序测试');
    }

    // 填充模型下拉列表
    const modelSelect = document.getElementById('testKeyModel');
    modelSelect.innerHTML = '';

    if (channel.models && channel.models.length > 0) {
      // channel.models 是 ModelEntry 对象数组，需访问 .model 属性
      channel.models.forEach(m => {
        const modelName = m.model || m; // 兼容字符串和对象
        const option = document.createElement('option');
        option.value = modelName;
        option.textContent = modelName;
        modelSelect.appendChild(option);
      });

      // 如果日志中的模型在支持列表中，则预选；否则选择第一个
      const modelNames = channel.models.map(m => m.model || m);
      if (modelNames.includes(model)) {
        modelSelect.value = model;
      } else {
        modelSelect.value = modelNames[0];
      }
    } else {
      // 没有配置模型，使用日志中的模型
      const option = document.createElement('option');
      option.value = model;
      option.textContent = model;
      modelSelect.appendChild(option);
      modelSelect.value = model;
    }
  } catch (e) {
    console.error('加载渠道配置失败', e);
    // 降级方案：使用日志中的模型
    const modelSelect = document.getElementById('testKeyModel');
    modelSelect.innerHTML = '';
    const option = document.createElement('option');
    option.value = model;
    option.textContent = model;
    modelSelect.appendChild(option);
    modelSelect.value = model;
    updateTestKeyIndexInfo('渠道配置加载失败，将按默认顺序测试');
  }
}

function closeTestKeyModal() {
  document.getElementById('testKeyModal').classList.remove('show');
  testingKeyData = null;
}

function resetTestKeyModal() {
  document.getElementById('testKeyProgress').classList.remove('show');
  document.getElementById('testKeyResult').classList.remove('show', 'success', 'error');
  document.getElementById('runKeyTestBtn').disabled = false;
  document.getElementById('testKeyContent').value = logsDefaultTestContent;
  document.getElementById('testKeyStream').checked = true;
  updateTestKeyIndexInfo('');
  // 重置模型选择框
  const modelSelect = document.getElementById('testKeyModel');
  modelSelect.innerHTML = '<option value="">加载中...</option>';
}

async function runKeyTest() {
  if (!testingKeyData) return;

  const modelSelect = document.getElementById('testKeyModel');
  const contentInput = document.getElementById('testKeyContent');
  const streamCheckbox = document.getElementById('testKeyStream');
  const selectedModel = modelSelect.value;
  const testContent = contentInput.value.trim() || logsDefaultTestContent;
  const streamEnabled = streamCheckbox.checked;

  if (!selectedModel) {
    if (window.showError) window.showError('请选择一个测试模型');
    return;
  }

  // 显示进度
  document.getElementById('testKeyProgress').classList.add('show');
  document.getElementById('testKeyResult').classList.remove('show');
  document.getElementById('runKeyTestBtn').disabled = true;

  try {
    // 构建测试请求（使用用户选择的模型）
    const testRequest = {
      model: selectedModel,
      stream: streamEnabled,
      content: testContent,
      channel_type: testingKeyData.channelType || 'anthropic' // ✅ 添加渠道类型
    };
    if (testingKeyData && testingKeyData.keyIndex !== null && testingKeyData.keyIndex !== undefined) {
      testRequest.key_index = testingKeyData.keyIndex;
    }

    const testResult = await fetchDataWithAuth(`/admin/channels/${testingKeyData.channelId}/test`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(testRequest)
    });

    displayKeyTestResult(testResult || { success: false, error: '空响应' });
  } catch (e) {
    console.error('测试失败', e);
    displayKeyTestResult({
      success: false,
      error: '测试请求失败: ' + e.message
    });
  } finally {
    document.getElementById('testKeyProgress').classList.remove('show');
    document.getElementById('runKeyTestBtn').disabled = false;
  }
}

function displayKeyTestResult(result) {
  const testResultDiv = document.getElementById('testKeyResult');
  const contentDiv = document.getElementById('testKeyResultContent');
  const detailsDiv = document.getElementById('testKeyResultDetails');

  testResultDiv.classList.remove('success', 'error');
  testResultDiv.classList.add('show');

  if (result.success) {
    testResultDiv.classList.add('success');
    contentDiv.innerHTML = `
          <div style="display: flex; align-items: center; gap: 8px;">
            <span style="font-size: 18px;">✅</span>
            <strong>${escapeHtml(result.message || 'API测试成功')}</strong>
          </div>
        `;

    let details = `响应时间: ${result.duration_ms}ms`;
    if (result.status_code) {
      details += ` | 状态码: ${result.status_code}`;
    }

    // 显示响应文本
    if (result.response_text) {
      details += `
            <div style="margin-top: 12px;">
              <h4 style="margin-bottom: 8px; color: var(--neutral-700);">API 响应内容</h4>
              <div style="padding: 12px; background: var(--neutral-50); border-radius: 4px; border: 1px solid var(--neutral-200); color: var(--neutral-700); white-space: pre-wrap; font-family: monospace; font-size: 0.9em; max-height: 300px; overflow-y: auto;">${escapeHtml(result.response_text)}</div>
            </div>
          `;
    }

    // 显示完整API响应
    if (result.api_response) {
      const responseId = 'api-response-' + Date.now();
      details += `
            <div style="margin-top: 12px;">
              <h4 style="margin-bottom: 8px; color: var(--neutral-700);">完整 API 响应</h4>
              <button type="button" class="btn btn-secondary btn-sm" data-action="toggle-response" data-response-target="${responseId}" style="margin-bottom: 8px;">显示/隐藏 JSON</button>
              <div id="${responseId}" style="display: none; padding: 12px; background: var(--neutral-50); border-radius: 4px; border: 1px solid var(--neutral-200); color: var(--neutral-700); white-space: pre-wrap; font-family: monospace; font-size: 0.85em; max-height: 400px; overflow-y: auto;">${escapeHtml(JSON.stringify(result.api_response, null, 2))}</div>
            </div>
          `;
    }

    detailsDiv.innerHTML = details;
  } else {
    testResultDiv.classList.add('error');
    contentDiv.innerHTML = `
          <div style="display: flex; align-items: center; gap: 8px;">
            <span style="font-size: 18px;">❌</span>
            <strong>测试失败</strong>
          </div>
        `;

    let details = `<p style="color: var(--error-600); margin-top: 8px;">${escapeHtml(result.error || '未知错误')}</p>`;

    if (result.status_code) {
      details += `<p style="margin-top: 8px;">状态码: ${result.status_code}</p>`;
    }

    if (result.raw_response) {
      const rawId = 'raw-response-' + Date.now();
      details += `
            <div style="margin-top: 12px;">
              <h4 style="margin-bottom: 8px; color: var(--neutral-700);">原始响应</h4>
              <button type="button" class="btn btn-secondary btn-sm" data-action="toggle-response" data-response-target="${rawId}" style="margin-bottom: 8px;">显示/隐藏</button>
              <div id="${rawId}" style="display: none; padding: 12px; background: var(--neutral-50); border-radius: 4px; border: 1px solid var(--neutral-200); color: var(--error-700); white-space: pre-wrap; font-family: monospace; font-size: 0.85em; max-height: 400px; overflow-y: auto;">${escapeHtml(result.raw_response)}</div>
            </div>
          `;
    }

    detailsDiv.innerHTML = details;
  }
}

// ========== 删除 Key（从日志列表入口） ==========
async function deleteKeyFromLog(channelId, channelName, maskedApiKey, apiKeyHash = '') {
  if (!channelId || !maskedApiKey) return;

  const confirmDel = confirm(`确定删除渠道“${channelName || ('#' + channelId)}”中的此Key (${maskedApiKey}) 吗？`);
  if (!confirmDel) return;

  try {
    // 通过 logs 返回的哈希优先精确匹配 key_index；无哈希时回退掩码匹配
    const apiKeys = await fetchDataWithAuth(`/admin/channels/${channelId}/keys`);
    const { keyIndex, matchCount, method } = await resolveKeyIndexForLogEntry(apiKeys, maskedApiKey, apiKeyHash);
    if (keyIndex === null) {
      if (matchCount > 1) {
        alert(method === 'hash'
          ? '匹配到多个同哈希 Key，为避免误删已阻止操作，请到渠道管理页手动删除。'
          : '匹配到多个同掩码 Key，为避免误删已阻止操作，请到渠道管理页手动删除。');
      } else {
        alert('未能匹配到该Key，请检查渠道配置。');
      }
      return;
    }

    // 删除Key
    const delResult = await fetchDataWithAuth(`/admin/channels/${channelId}/keys/${keyIndex}`, { method: 'DELETE' });

    alert(`已删除 Key #${keyIndex + 1} (${maskedApiKey})`);

    // 如果没有剩余Key，询问是否删除渠道
    if (delResult && delResult.remaining_keys === 0) {
      const delChannel = confirm('该渠道已无可用Key，是否删除整个渠道？');
      if (delChannel) {
        const chResp = await fetchAPIWithAuth(`/admin/channels/${channelId}`, { method: 'DELETE' });
        if (!chResp.success) throw new Error(chResp.error || '删除渠道失败');
        alert('渠道已删除');
      }
    }

    // 刷新日志列表
    load();
  } catch (e) {
    console.error('删除Key失败', e);
    alert(e.message || '删除Key失败');
  }
}

// ============================================================================
// Debug Log Modal
// ============================================================================

function escapeCodeHtml(str) {
  return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

function formatCodeWithLines(text) {
  if (!text) return '';
  const lines = text.split('\n');
  return lines.map(line => `<span class="code-line">${escapeCodeHtml(line)}</span>`).join('');
}

function setCodeContent(elementId, text) {
  const el = document.getElementById(elementId);
  if (!el) return;
  el._rawText = text || '';
  el.innerHTML = formatCodeWithLines(text || '');
}

function formatJsonSafe(str) {
  if (!str) return '';
  try {
    return JSON.stringify(JSON.parse(str), null, 2);
  } catch {
    return str;
  }
}

function formatHeaderLines(headers) {
  if (!headers) return '';
  if (typeof headers === 'string') {
    try { headers = JSON.parse(headers); } catch { return headers; }
  }
  if (typeof headers !== 'object') return '';
  const lines = [];
  for (const [key, value] of Object.entries(headers)) {
    if (Array.isArray(value)) {
      value.forEach(v => lines.push(`${key}: ${v}`));
    } else {
      lines.push(`${key}: ${value}`);
    }
  }
  return lines.join('\n');
}

function composeDebugRawRequest(data) {
  const parts = [];
  const method = data.req_method || 'POST';
  const url = data.req_url || '';
  parts.push(`${method} ${url}`);
  const headers = formatHeaderLines(data.req_headers);
  if (headers) parts.push(headers);
  const body = formatJsonSafe(data.req_body);
  if (body) {
    parts.push('');
    parts.push(body);
  }
  return parts.join('\n');
}

function composeDebugRawResponse(data) {
  const parts = [];
  if (data.resp_status) parts.push('HTTP ' + data.resp_status);
  const headers = formatHeaderLines(data.resp_headers);
  if (headers) parts.push(headers);
  const body = formatJsonSafe(data.resp_body);
  if (body) {
    parts.push('');
    parts.push(body);
  }
  return parts.join('\n');
}

async function showDebugLogModal(logId) {
  const modal = document.getElementById('debugLogModal');
  const loading = document.getElementById('debugLogLoading');
  const error = document.getElementById('debugLogError');
  const content = document.getElementById('debugLogContent');

  loading.style.display = '';
  error.style.display = 'none';
  content.style.display = 'none';
  modal.classList.add('show');

  // Reset tabs
  modal.querySelectorAll('.upstream-tab').forEach(tab => {
    tab.classList.toggle('active', tab.dataset.tab === 'request');
  });
  document.getElementById('debugTabRequest').classList.add('active');
  document.getElementById('debugTabResponse').classList.remove('active');

  try {
    const data = await fetchDataWithAuth(`/admin/debug-logs/${logId}`);
    loading.style.display = 'none';
    content.style.display = 'flex';

    setCodeContent('debugReqRaw', composeDebugRawRequest(data));
    setCodeContent('debugRespRaw', composeDebugRawResponse(data));
  } catch (e) {
    loading.style.display = 'none';
    if (e.message && e.message.includes('404')) {
      error.textContent = t('logs.debugNotFound') || 'Debug日志不存在（未启用或已过期）';
    } else {
      error.textContent = e.message || '加载失败';
    }
    error.style.display = '';
  }
}

function closeDebugLogModal() {
  document.getElementById('debugLogModal').classList.remove('show');
}

// Tab switch + copy button delegation for debug log modal.
// 部分测试桩只提供最小 document API，这里避免在脚本加载阶段就假定完整 DOM 存在。
if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
  document.addEventListener('click', (e) => {
    const tab = e.target.closest('#debugLogModal .upstream-tab');
    if (tab) {
      const target = tab.dataset.tab;
      document.querySelectorAll('#debugLogModal .upstream-tab').forEach(t => t.classList.toggle('active', t === tab));
      document.getElementById('debugTabRequest').classList.toggle('active', target === 'request');
      document.getElementById('debugTabResponse').classList.toggle('active', target === 'response');
      return;
    }

    const copyBtn = e.target.closest('#debugLogModal .upstream-copy-btn');
    if (copyBtn) {
      const targetId = copyBtn.dataset.copyTarget;
      const pre = document.getElementById(targetId);
      if (!pre) return;
      const text = pre._rawText || pre.textContent || '';
      navigator.clipboard.writeText(text).then(() => {
        const orig = copyBtn.textContent;
        copyBtn.textContent = '\u2713';
        copyBtn.classList.add('copied');
        setTimeout(() => { copyBtn.textContent = orig; copyBtn.classList.remove('copied'); }, 1500);
      });
    }
  });
}
