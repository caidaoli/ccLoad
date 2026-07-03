// URL 表格管理（与 API Key 表格一致的交互模式）
const INLINE_EXACT_URL_MARKER = '#';

function isExactInlineURL(url) {
  return String(url || '').trim().endsWith(INLINE_EXACT_URL_MARKER);
}

function stripInlineExactURLMarker(url) {
  const value = String(url || '').trim();
  if (!value.endsWith(INLINE_EXACT_URL_MARKER)) return value;
  return value.slice(0, -INLINE_EXACT_URL_MARKER.length).trim();
}

function withInlineExactURLMarker(url, exact) {
  const cleanURL = stripInlineExactURLMarker(url);
  if (!cleanURL) return '';
  return exact ? `${cleanURL}${INLINE_EXACT_URL_MARKER}` : cleanURL;
}

function normalizeInlineURLValue(url) {
  return withInlineExactURLMarker(url, isExactInlineURL(url));
}

function parseChannelURLs(input) {
  if (!input || !input.trim()) return [];

  return input
    .split('\n')
    .map(normalizeInlineURLValue)
    .filter(Boolean);
}

function getValidInlineURLs() {
  return inlineURLTableData
    .map(normalizeInlineURLValue)
    .filter(Boolean);
}

function syncInlineURLInput() {
  const hiddenInput = document.getElementById('channelUrl');
  if (!hiddenInput) return;
  hiddenInput.value = getValidInlineURLs().join('\n');
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

function shouldShowURLExtras() {
  return inlineURLTableData.length > 1;
}

function createURLRow(index) {
  const rawURL = inlineURLTableData[index] || '';
  const tplData = {
    index: index,
    displayIndex: index + 1,
    url: stripInlineExactURLMarker(rawURL),
    exactURLChecked: isExactInlineURL(rawURL) ? 'checked' : '',
    mobileLabelUrl: window.t('channels.tableApiUrl'),
    mobileLabelExactURL: window.t('channels.fullUrl'),
    mobileLabelActions: window.t('common.actions')
  };

  const row = TemplateEngine.render('tpl-url-row', tplData);
  if (!row) return null;

  const checkbox = row.querySelector('.url-checkbox');
  if (checkbox && selectedURLIndices.has(index)) {
    checkbox.checked = true;
  }

  // 多URL已保存渠道：注入统计列和禁用按钮
  if (shouldShowURLExtras()) {
    const url = normalizeInlineURLValue(inlineURLTableData[index]);
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
      toggleBtn.style.cssText = 'width: 26px; height: 26px; border-radius: 6px; border: 1px solid var(--surface-border-strong); background: var(--surface-bg-strong); color: var(--neutral-500); cursor: pointer; transition: all 0.2s; display: inline-flex; align-items: center; justify-content: center; padding: 0;';
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
  }

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
    inlineURLTableData = [''];
  }

  initInlineURLTableEventDelegation();
  updateInlineURLCount();
  syncInlineURLInput();
  if (typeof syncProtocolTransformModeForURLs === 'function') {
    syncProtocolTransformModeForURLs();
  }
  updateURLStatsHeader();

  tbody.innerHTML = '';
  inlineURLTableData.forEach((_, index) => {
    const row = createURLRow(index);
    if (row) tbody.appendChild(row);
  });

  updateSelectAllURLsCheckbox();
  updateURLBatchDeleteButton();
}

function setInlineURLTableData(rawURL) {
  inlineURLTableData = parseChannelURLs(rawURL);
  if (inlineURLTableData.length === 0) {
    inlineURLTableData = [''];
  }
  selectedURLIndices.clear();
  urlStatsMap = {};
  renderInlineURLTable();
}

function addInlineURL() {
  const newIndex = inlineURLTableData.length;
  inlineURLTableData.push('');
  renderInlineURLTable();
  markChannelFormDirty();

  setTimeout(() => {
    const input = document.querySelector(`.inline-url-input[data-index="${newIndex}"]`);
    if (input) input.focus();
  }, 0);
}

function updateInlineURL(index, value) {
  const keepExactURL = isExactInlineURL(inlineURLTableData[index]) || isExactInlineURL(value);
  const nextValue = withInlineExactURLMarker(value, keepExactURL);
  if (inlineURLTableData[index] === nextValue) return;

  inlineURLTableData[index] = nextValue;
  syncInlineURLInput();
  if (typeof syncProtocolTransformModeForURLs === 'function') {
    syncProtocolTransformModeForURLs();
  }
  if (typeof scheduleChannelDuplicateHintCheck === 'function') {
    scheduleChannelDuplicateHintCheck();
  }
  markChannelFormDirty();

  if (isExactInlineURL(value)) {
    renderInlineURLTable();
  }
}

function updateInlineURLExact(index, checked) {
  const cleanURL = stripInlineExactURLMarker(inlineURLTableData[index]);
  const nextValue = cleanURL ? withInlineExactURLMarker(cleanURL, checked) : (checked ? INLINE_EXACT_URL_MARKER : '');
  if (inlineURLTableData[index] === nextValue) return;

  inlineURLTableData[index] = nextValue;
  syncInlineURLInput();
  if (typeof syncProtocolTransformModeForURLs === 'function') {
    syncProtocolTransformModeForURLs();
  }
  if (typeof scheduleChannelDuplicateHintCheck === 'function') {
    scheduleChannelDuplicateHintCheck();
  }
  markChannelFormDirty();
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
    inlineURLTableData[0] = '';
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
    inlineURLTableData = [''];
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
  const url = normalizeInlineURLValue(inlineURLTableData[index]);
  if (!url) {
    alert(window.t('channels.fillApiUrlFirst'));
    return;
  }

  const firstKey = (getValidInlineKeyRows()[0] || {}).api_key || '';
  if (!firstKey) {
    alert(window.t('channels.emptyKeyCannotTest'));
    return;
  }

  const channelTypeRadios = document.querySelectorAll('input[name="channelType"]');
  let channelType = 'anthropic';
  for (const radio of channelTypeRadios) {
    if (radio.checked) {
      channelType = radio.value.toLowerCase();
      break;
    }
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
        channel_type: channelType,
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

function hasURLStats() {
  return Object.keys(urlStatsMap).length > 0;
}

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
    if (hasURLStats() || shouldShowURLExtras()) {
      renderInlineURLTable();
    }
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

  if (!shouldShowURLExtras()) return;

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
    // 本地更新状态，避免依赖 fetchURLStats（单URL渠道后端返回空数组）
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
