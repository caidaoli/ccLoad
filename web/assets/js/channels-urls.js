// URL 表格管理（与 API Key 表格一致的交互模式）
const INLINE_EXACT_URL_MARKER = '#';
const INLINE_URL_PROTOCOLS = ['anthropic', 'codex', 'openai', 'gemini'];
const inlineURLProtocolComboboxes = new Map();

function getInlineURLProtocolOptions() {
  return [
    { value: '', label: window.t('channels.urlProtocolAuto') },
    { value: 'anthropic', label: 'Anthropic' },
    { value: 'codex', label: 'Codex' },
    { value: 'openai', label: 'OpenAI' },
    { value: 'gemini', label: 'Gemini' }
  ];
}

function getInlineURLProtocolLabel(value) {
  return getInlineURLProtocolOptions().find(option => option.value === value)?.label || value;
}

function emptyInlineURLConfig() {
  return { url: '', exact: false, protocols: [] };
}

function normalizeInlineURLConfig(value) {
  const source = value && typeof value === 'object' ? value : {};
  const selectedProtocols = new Set(
    (Array.isArray(source.protocols) ? source.protocols : [])
      .map(protocol => String(protocol || '').trim().toLowerCase())
  );
  const selectedProtocol = Array.from(selectedProtocols).find(protocol => INLINE_URL_PROTOCOLS.includes(protocol));
  return {
    url: String(source.url || '').trim(),
    exact: Boolean(source.exact),
    protocols: selectedProtocol ? [selectedProtocol] : []
  };
}

function normalizeInlineURLConfigs(values) {
  if (!Array.isArray(values)) return [];
  return values
    .map(normalizeInlineURLConfig)
    .filter(entry => entry.url);
}

function runtimeInlineURL(value) {
  const entry = normalizeInlineURLConfig(value);
  if (!entry.url) return '';
  return entry.exact ? `${entry.url}${INLINE_EXACT_URL_MARKER}` : entry.url;
}

function getValidInlineURLConfigs() {
  return normalizeInlineURLConfigs(inlineURLTableData);
}

function getValidInlineURLs() {
  return getValidInlineURLConfigs()
    .map(runtimeInlineURL)
    .filter(Boolean);
}

function updateInlineURLCount() {
  const countEl = document.getElementById('inlineUrlCount');
  if (!countEl) return;
  countEl.textContent = inlineURLTableData.length;
}

function updateURLBatchDeleteButton() {
  const btn = document.getElementById('batchDeleteUrlsBtn');
  if (!btn) return;

  const count = selectedURLIndices.size;
  btn.disabled = count === 0;
  btn.style.opacity = count === 0 ? '0.5' : '1';

  const textEl = btn.querySelector('span');
  if (textEl) {
    textEl.textContent = count > 0
      ? window.t('channels.deleteSelectedCount', { count })
      : window.t('channels.deleteSelected');
  }
}

function updateSelectAllURLsCheckbox() {
  const checkbox = document.getElementById('selectAllURLs');
  if (!checkbox) return;

  const total = inlineURLTableData.length;
  const selected = selectedURLIndices.size;

  if (total === 0 || selected === 0) {
    checkbox.checked = false;
    checkbox.indeterminate = false;
    return;
  }

  if (selected === total) {
    checkbox.checked = true;
    checkbox.indeterminate = false;
    return;
  }

  checkbox.checked = false;
  checkbox.indeterminate = true;
}

function createURLRow(index) {
  const entry = normalizeInlineURLConfig(inlineURLTableData[index]);
  inlineURLTableData[index] = entry;
  const tplData = {
    index: index,
    displayIndex: index + 1,
    url: entry.url,
    exactURLChecked: entry.exact ? 'checked' : '',
    mobileLabelUrl: window.t('channels.tableApiUrl'),
    mobileLabelProtocols: window.t('channels.urlProtocols'),
    mobileLabelExactURL: window.t('channels.fullUrl'),
    mobileLabelActions: window.t('common.actions')
  };

  const row = TemplateEngine.render('tpl-url-row', tplData);
  if (!row) return null;

  const checkbox = row.querySelector('.url-checkbox');
  if (checkbox && selectedURLIndices.has(index)) {
    checkbox.checked = true;
  }

  // 单 URL 与多 URL 使用同一列结构，避免添加/删除 URL 时表格跳变。
  const url = runtimeInlineURL(entry);
  const stat = urlStatsMap[url];
  const actionsTd = row.querySelectorAll('td');
  const lastTd = actionsTd[actionsTd.length - 1]; // actions列

  const statusTd = document.createElement('td');
  statusTd.className = 'inline-url-cell-center inline-url-col-status';
  statusTd.setAttribute('data-mobile-label', window.t('common.status'));
  statusTd.innerHTML = formatURLStatus(stat);

  const latencyTd = document.createElement('td');
  latencyTd.className = 'inline-url-cell-center inline-url-cell-metric inline-url-col-latency';
  latencyTd.setAttribute('data-mobile-label', window.t('stats.latency'));
  latencyTd.textContent = formatURLLatency(stat);

  const requestsTd = document.createElement('td');
  requestsTd.className = 'inline-url-cell-center inline-url-cell-metric inline-url-col-requests';
  requestsTd.setAttribute('data-mobile-label', window.t('channels.urlRequests'));
  requestsTd.innerHTML = formatURLRequests(stat);

  if (url) {
    const isDisabled = stat && stat.disabled;
    const toggleBtn = document.createElement('button');
    toggleBtn.type = 'button';
    // 与同行其他按钮（测试/删除）保持中性灰色风格，状态仅由图标形状与 tooltip 表达
    toggleBtn.className = 'inline-url-toggle-btn';
    toggleBtn.style.cssText = 'width: 26px; height: 26px; border-radius: 6px; border: 1px solid var(--surface-border-strong); background: var(--surface-bg-strong); color: var(--neutral-500); cursor: pointer; transition: color 0.2s, border-color 0.2s, background-color 0.2s; display: inline-flex; align-items: center; justify-content: center; padding: 0;';
    toggleBtn.title = isDisabled ? window.t('channels.urlEnable') : window.t('channels.urlDisable');
    toggleBtn.innerHTML = isDisabled
      ? '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><circle cx="12" cy="12" r="10"/><line x1="4.93" y1="4.93" x2="19.07" y2="19.07"/></svg>'
      : '<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M22 11.08V12a10 10 0 1 1-5.93-9.14"/><polyline points="22 4 12 14.01 9 11.01"/></svg>';
    toggleBtn.dataset.url = url;
    toggleBtn.dataset.disabled = isDisabled ? '1' : '0';
    toggleBtn.addEventListener('click', () => toggleURLDisabled(toggleBtn));
    // 合并到 actions 容器内部，作为首个按钮，与 Key 表 actions 容器结构保持一致
    const actionsContainer = lastTd.querySelector('.inline-url-actions');
    if (actionsContainer) {
      actionsContainer.insertBefore(toggleBtn, actionsContainer.firstChild);
    }
  }

  row.insertBefore(statusTd, lastTd);
  row.insertBefore(latencyTd, lastTd);
  row.insertBefore(requestsTd, lastTd);

  return row;
}

function initInlineURLTableEventDelegation() {
  const tbody = document.getElementById('inlineUrlTableBody');
  if (!tbody || tbody.dataset.delegated) return;

  tbody.dataset.delegated = 'true';

  tbody.addEventListener('change', (e) => {
    const checkbox = e.target.closest('.url-checkbox');
    if (checkbox) {
      const index = parseInt(checkbox.dataset.index, 10);
      toggleURLSelection(index, checkbox.checked);
      return;
    }

    const input = e.target.closest('.inline-url-input');
    if (input) {
      const index = parseInt(input.dataset.index, 10);
      updateInlineURL(index, input.value);
      return;
    }

    const exactCheckbox = e.target.closest('.inline-url-exact-checkbox');
    if (exactCheckbox) {
      const index = parseInt(exactCheckbox.dataset.index, 10);
      updateInlineURLExact(index, exactCheckbox.checked);
      return;
    }

  });

  tbody.addEventListener('click', (e) => {
    const testBtn = e.target.closest('.inline-url-test-btn');
    if (testBtn) {
      const index = parseInt(testBtn.dataset.index, 10);
      testInlineURL(index, testBtn);
      return;
    }

    const deleteBtn = e.target.closest('.inline-url-delete-btn');
    if (deleteBtn) {
      const index = parseInt(deleteBtn.dataset.index, 10);
      deleteInlineURL(index);
    }
  });
}

function renderInlineURLTable() {
  const tbody = document.getElementById('inlineUrlTableBody');
  if (!tbody) return;

  if (inlineURLTableData.length === 0) {
    inlineURLTableData = [emptyInlineURLConfig()];
  }

  initInlineURLTableEventDelegation();
	updateInlineURLCount();
  updateURLStatsHeader();

  destroyInlineURLProtocolComboboxes();
  tbody.innerHTML = '';
  inlineURLTableData.forEach((_, index) => {
    const row = createURLRow(index);
    if (!row) return;
    tbody.appendChild(row);
    initInlineURLProtocolCombobox(index);
  });

  updateSelectAllURLsCheckbox();
  updateURLBatchDeleteButton();
}

function setInlineURLTableData(urls) {
  inlineURLTableData = Array.isArray(urls)
    ? urls.map(normalizeInlineURLConfig)
    : [];
  if (inlineURLTableData.length === 0) {
    inlineURLTableData = [emptyInlineURLConfig()];
  }
  selectedURLIndices.clear();
  urlStatsMap = {};
  renderInlineURLTable();
}

function addInlineURL() {
  const newIndex = inlineURLTableData.length;
  inlineURLTableData.push(emptyInlineURLConfig());
  renderInlineURLTable();
  markChannelFormDirty();

  setTimeout(() => {
    const input = document.querySelector(`.inline-url-input[data-index="${newIndex}"]`);
    if (input) input.focus();
  }, 0);
}

function updateInlineURL(index, value) {
  const entry = normalizeInlineURLConfig(inlineURLTableData[index]);
  const nextURL = String(value || '').trim();
  if (entry.url === nextURL) return;

  entry.url = nextURL;
	inlineURLTableData[index] = entry;
  if (typeof scheduleChannelDuplicateHintCheck === 'function') {
    scheduleChannelDuplicateHintCheck();
  }
  markChannelFormDirty();
}

function updateInlineURLExact(index, checked) {
  const entry = normalizeInlineURLConfig(inlineURLTableData[index]);
  if (entry.exact === checked) return;

  entry.exact = checked;
	inlineURLTableData[index] = entry;
  if (typeof scheduleChannelDuplicateHintCheck === 'function') {
    scheduleChannelDuplicateHintCheck();
  }
  markChannelFormDirty();
}

function updateInlineURLProtocol(index, protocol) {
  const entry = normalizeInlineURLConfig(inlineURLTableData[index]);
  const normalizedProtocol = String(protocol || '').trim().toLowerCase();
  if (normalizedProtocol && !INLINE_URL_PROTOCOLS.includes(normalizedProtocol)) return;

  const nextProtocols = normalizedProtocol ? [normalizedProtocol] : [];
  if (entry.protocols.length === nextProtocols.length && entry.protocols.every((value, i) => value === nextProtocols[i])) return;

  entry.protocols = nextProtocols;
  inlineURLTableData[index] = entry;
  markChannelFormDirty();
}

function initInlineURLProtocolCombobox(index) {
  const entry = normalizeInlineURLConfig(inlineURLTableData[index]);
  const selectedProtocol = entry.protocols[0] || '';
  const combobox = createSearchableCombobox({
    container: `inlineURLProtocolSelectContainer-${index}`,
    inputId: `inlineURLProtocolInput-${index}`,
    dropdownId: `inlineURLProtocolDropdown-${index}`,
    minWidth: 0,
    getOptions: getInlineURLProtocolOptions,
    initialValue: selectedProtocol,
    initialLabel: getInlineURLProtocolLabel(selectedProtocol),
    onSelect: (value) => updateInlineURLProtocol(index, value)
  });
  if (combobox) inlineURLProtocolComboboxes.set(index, combobox);
}

function destroyInlineURLProtocolComboboxes() {
  inlineURLProtocolComboboxes.forEach(combobox => combobox?.destroy());
  inlineURLProtocolComboboxes.clear();
}

if (typeof window !== 'undefined' && typeof window.addEventListener === 'function') {
  window.addEventListener('localechange', () => {
    inlineURLProtocolComboboxes.forEach((combobox, index) => {
      const entry = normalizeInlineURLConfig(inlineURLTableData[index]);
      const selectedProtocol = entry.protocols[0] || '';
      combobox.setValue(selectedProtocol, getInlineURLProtocolLabel(selectedProtocol));
      combobox.refresh();
    });
  });
}

function toggleURLSelection(index, checked) {
  if (checked) {
    selectedURLIndices.add(index);
  } else {
    selectedURLIndices.delete(index);
  }

  updateSelectAllURLsCheckbox();
  updateURLBatchDeleteButton();
}

function toggleSelectAllURLs(checked) {
  if (checked) {
    inlineURLTableData.forEach((_, index) => selectedURLIndices.add(index));
  } else {
    selectedURLIndices.clear();
  }

  renderInlineURLTable();
}

function deleteInlineURL(index) {
  if (index < 0 || index >= inlineURLTableData.length) return;

  if (inlineURLTableData.length === 1) {
    inlineURLTableData[0] = emptyInlineURLConfig();
    selectedURLIndices.clear();
    renderInlineURLTable();
    if (typeof scheduleChannelDuplicateHintCheck === 'function') {
      scheduleChannelDuplicateHintCheck();
    }
    markChannelFormDirty();
    return;
  }

  inlineURLTableData.splice(index, 1);

  const nextSelected = new Set();
  selectedURLIndices.forEach(i => {
    if (i < index) {
      nextSelected.add(i);
    } else if (i > index) {
      nextSelected.add(i - 1);
    }
  });
  selectedURLIndices = nextSelected;

  renderInlineURLTable();
  if (typeof scheduleChannelDuplicateHintCheck === 'function') {
    scheduleChannelDuplicateHintCheck();
  }
  markChannelFormDirty();
}

function batchDeleteSelectedURLs() {
  const count = selectedURLIndices.size;
  if (count === 0) return;

  if (!confirm(window.t('channels.confirmBatchDeleteUrls', { count }))) {
    return;
  }

  const indices = Array.from(selectedURLIndices).sort((a, b) => b - a);
  indices.forEach(index => {
    inlineURLTableData.splice(index, 1);
  });

  if (inlineURLTableData.length === 0) {
    inlineURLTableData = [emptyInlineURLConfig()];
  }

  selectedURLIndices.clear();
  renderInlineURLTable();
  if (typeof scheduleChannelDuplicateHintCheck === 'function') {
    scheduleChannelDuplicateHintCheck();
  }
  markChannelFormDirty();
}

async function testInlineURL(index, buttonElement) {
  if (!editingChannelId) {
    alert(window.t('channels.cannotGetChannelId'));
    return;
  }

  const models = redirectTableData
    .map(r => r.model)
    .filter(m => m && m.trim());
  if (models.length === 0) {
    alert(window.t('channels.configModelsFirst'));
    return;
  }

  const firstModel = models[0];
  const url = runtimeInlineURL(inlineURLTableData[index]);
  if (!url) {
    alert(window.t('channels.fillApiUrlFirst'));
    return;
  }

  const firstKey = (getValidInlineKeyRows()[0] || {}).api_key || '';
  if (!firstKey) {
    alert(window.t('channels.emptyKeyCannotTest'));
    return;
  }

  if (!buttonElement) return;
  const originalHTML = buttonElement.innerHTML;
  buttonElement.disabled = true;
  buttonElement.innerHTML = '<span style="font-size: 10px;">⏳</span>';

  try {
    const testResult = await fetchDataWithAuth(`/admin/channels/${editingChannelId}/test-url`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        model: firstModel,
        stream: true,
        content: 'test',
        client_protocol: 'anthropic',
        key_index: 0,
        base_url: url
      })
    });

    await refreshKeyCooldownStatus();

    if (testResult.success) {
      window.showNotification(window.t('channels.urlTestSuccess', { index: index + 1 }), 'success');
    } else {
      const errorMsg = testResult.error || window.t('common.failed');
      window.showNotification(window.t('channels.urlTestFailed', { index: index + 1, error: errorMsg }), 'error');
    }
  } catch (error) {
    console.error('URL test failed', error);
    window.showNotification(window.t('channels.urlTestRequestFailed', { index: index + 1, error: error.message }), 'error');
  } finally {
    buttonElement.disabled = false;
    buttonElement.innerHTML = originalHTML;
  }
}

// === URL 实时状态 ===

async function fetchURLStats(channelId) {
  if (!channelId) return;
  try {
    const stats = await fetchDataWithAuth(`/admin/channels/${channelId}/url-stats`);
    urlStatsMap = {};
    if (Array.isArray(stats)) {
      for (const s of stats) {
        urlStatsMap[s.url] = s;
      }
    }
    renderInlineURLTable();
  } catch (e) {
    console.error('Failed to fetch URL stats', e);
  }
}

function formatURLStatus(stat) {
  if (!stat) {
    return '<span class="inline-url-status-placeholder">--</span>';
  }
  if (stat.disabled) {
    return '<span class="inline-url-status-badge inline-url-status-badge--disabled">'
      + '<span class="inline-url-status-dot inline-url-status-dot--disabled"></span>'
      + `${window.t('channels.urlStatusDisabled')}</span>`;
  }
  if (stat.cooled_down) {
    const remain = humanizeMS(stat.cooldown_remain_ms);
    return `<span class="inline-url-status-badge inline-url-status-badge--cooldown" title="${window.t('channels.urlStatusCooldown')} ${remain}">`
      + '<span class="inline-url-status-dot inline-url-status-dot--cooldown"></span>'
      + `${remain}</span>`;
  }
  if (stat.latency_ms < 0) {
    return '<span class="inline-url-status-badge inline-url-status-badge--unknown">'
      + '<span class="inline-url-status-dot inline-url-status-dot--unknown"></span>'
      + `${window.t('channels.urlStatusUnknown')}</span>`;
  }
  return '<span class="inline-url-status-badge inline-url-status-badge--ok">'
    + '<span class="inline-url-status-dot inline-url-status-dot--ok"></span>'
    + `${window.t('channels.urlStatusNormal')}</span>`;
}

function formatURLLatency(stat) {
  if (!stat || stat.latency_ms < 0) return '--';
  const ms = Math.round(stat.latency_ms);
  if (ms < 1000) return ms + 'ms';
  return (ms / 1000).toFixed(1) + 's';
}

function formatURLRequests(stat) {
  if (!stat) return '--';
  const s = stat.requests || 0;
  const f = stat.failures || 0;
  if (s === 0 && f === 0) return '--';
  if (f === 0) return `<span style="color: #16A34A;">${s}</span>`;
  return `<span style="color: #16A34A;">${s}</span><span style="color: var(--neutral-300); margin: 0 2px;">/</span><span style="color: #DC2626;">${f}</span>`;
}

function updateURLStatsHeader() {
  const thead = document.querySelector('#inlineUrlTableBody')?.closest('table')?.querySelector('thead tr');
  if (!thead) return;

  // 移除已有的统计列头
  thead.querySelectorAll('.url-stats-th').forEach(el => el.remove());

  const actionsTh = thead.querySelector('th:last-child');

  const statusTh = document.createElement('th');
  statusTh.className = 'url-stats-th inline-url-col-status';
  statusTh.textContent = window.t('channels.urlStatus');

  const latencyTh = document.createElement('th');
  latencyTh.className = 'url-stats-th inline-url-col-latency';
  latencyTh.textContent = window.t('channels.urlLatency');

  const requestsTh = document.createElement('th');
  requestsTh.className = 'url-stats-th inline-url-col-requests';
  requestsTh.textContent = window.t('channels.urlRequests');

  thead.insertBefore(statusTh, actionsTh);
  thead.insertBefore(latencyTh, actionsTh);
  thead.insertBefore(requestsTh, actionsTh);
}

async function toggleURLDisabled(btn) {
  if (!editingChannelId) return;
  const url = btn.dataset.url;
  const isCurrentlyDisabled = btn.dataset.disabled === '1';
  const endpoint = isCurrentlyDisabled ? 'url-enable' : 'url-disable';

  btn.disabled = true;
  try {
    await fetchDataWithAuth(`/admin/channels/${editingChannelId}/${endpoint}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ url })
    });
    // 先更新本地状态，避免按钮反馈依赖额外网络往返。
    const newDisabled = !isCurrentlyDisabled;
    if (!urlStatsMap[url]) {
      urlStatsMap[url] = { url, latency_ms: -1, cooled_down: false, cooldown_remain_ms: 0, requests: 0, failures: 0, disabled: newDisabled };
    } else {
      urlStatsMap[url].disabled = newDisabled;
    }
    renderInlineURLTable();
  } catch (e) {
    console.error('Toggle URL failed', e);
    window.showNotification(e.message, 'error');
  } finally {
    btn.disabled = false;
  }
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    fetchURLStats,
    normalizeInlineURLConfig,
    normalizeInlineURLConfigs,
    runtimeInlineURL
  };
}
