function showAddModal() {
  editingChannelId = null;
  currentChannelKeyCooldowns = [];

  document.getElementById('modalTitle').textContent = window.t('channels.addChannel');
  document.getElementById('channelForm').reset();
  document.getElementById('channelEnabled').checked = true;
  document.querySelector('input[name="channelType"][value="anthropic"]').checked = true;
  document.querySelector('input[name="keyStrategy"][value="sequential"]').checked = true;

  redirectTableData = [];
  selectedModelIndices.clear();
  currentModelFilter = '';
  const modelFilterInput = document.getElementById('modelFilterInput');
  if (modelFilterInput) modelFilterInput.value = '';
  renderRedirectTable();

  inlineKeyTableData = [''];
  inlineKeyVisible = true;
  document.getElementById('inlineEyeIcon').style.display = 'none';
  document.getElementById('inlineEyeOffIcon').style.display = 'block';
  renderInlineKeyTable();

  document.getElementById('channelModal').classList.add('show');
}

async function editChannel(id) {
  const channel = channels.find(c => c.id === id);
  if (!channel) return;

  editingChannelId = id;

  document.getElementById('modalTitle').textContent = window.t('channels.editChannel');
  document.getElementById('channelName').value = channel.name;
  document.getElementById('channelUrl').value = channel.url;

  let apiKeys = [];
  try {
    apiKeys = (await fetchDataWithAuth(`/admin/channels/${id}/keys`)) || [];
  } catch (e) {
    console.error('Failed to fetch API Keys', e);
  }

  const now = Date.now();
  currentChannelKeyCooldowns = apiKeys.map((apiKey, index) => {
    const cooldownUntilMs = (apiKey.cooldown_until || 0) * 1000;
    const remainingMs = Math.max(0, cooldownUntilMs - now);
    return {
      key_index: index,
      cooldown_remaining_ms: remainingMs
    };
  });

  inlineKeyTableData = apiKeys.map(k => k.api_key || k);
  if (inlineKeyTableData.length === 0) {
    inlineKeyTableData = [''];
    currentChannelKeyCooldowns = [];
  }

  inlineKeyVisible = true;
  document.getElementById('inlineEyeIcon').style.display = 'none';
  document.getElementById('inlineEyeOffIcon').style.display = 'block';
  renderInlineKeyTable();

  const channelType = channel.channel_type || 'anthropic';
  await window.ChannelTypeManager.renderChannelTypeRadios('channelTypeRadios', channelType);
  const keyStrategy = channel.key_strategy || 'sequential';
  const strategyRadio = document.querySelector(`input[name="keyStrategy"][value="${keyStrategy}"]`);
  if (strategyRadio) {
    strategyRadio.checked = true;
  }
  document.getElementById('channelPriority').value = channel.priority;
  document.getElementById('channelDailyCostLimit').value = channel.daily_cost_limit || 0;
  document.getElementById('channelEnabled').checked = channel.enabled;

  // 加载模型配置（新格式：models是 {model, redirect_model} 数组）
  redirectTableData = (channel.models || []).map(m => ({
    model: m.model || '',
    redirect_model: m.redirect_model || ''
  }));
  selectedModelIndices.clear();
  currentModelFilter = '';
  const modelFilterInput = document.getElementById('modelFilterInput');
  if (modelFilterInput) modelFilterInput.value = '';
  renderRedirectTable();

  document.getElementById('channelModal').classList.add('show');
}

function closeModal() {
  if (channelFormDirty && !confirm(window.t('channels.unsavedChanges'))) {
    return;
  }
  document.getElementById('channelModal').classList.remove('show');
  editingChannelId = null;
  resetChannelFormDirty();
}

async function saveChannel(event) {
  event.preventDefault();

  const validKeys = inlineKeyTableData.filter(k => k && k.trim());
  if (validKeys.length === 0) {
    alert(window.t('channels.atLeastOneKey'));
    return;
  }

  document.getElementById('channelApiKey').value = validKeys.join(',');

  // 构建模型配置（新格式：models 数组）
  const models = redirectTableData
    .filter(r => r.model && r.model.trim())
    .map(r => ({
      model: r.model.trim(),
      redirect_model: (r.redirect_model || '').trim()
    }));

  const channelType = document.querySelector('input[name="channelType"]:checked')?.value || 'anthropic';
  const keyStrategy = document.querySelector('input[name="keyStrategy"]:checked')?.value || 'sequential';

  const formData = {
    name: document.getElementById('channelName').value.trim(),
    url: document.getElementById('channelUrl').value.trim(),
    api_key: validKeys.join(','),
    channel_type: channelType,
    key_strategy: keyStrategy,
    priority: parseInt(document.getElementById('channelPriority').value) || 0,
    daily_cost_limit: parseFloat(document.getElementById('channelDailyCostLimit').value) || 0,
    models: models,
    enabled: document.getElementById('channelEnabled').checked
  };

  if (!formData.name || !formData.url || !formData.api_key || formData.models.length === 0) {
    if (window.showError) window.showError(window.t('channels.fillAllRequired'));
    return;
  }

  try {
    const resp = editingChannelId
      ? await fetchAPIWithAuth(`/admin/channels/${editingChannelId}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(formData)
        })
      : await fetchAPIWithAuth('/admin/channels', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(formData)
        });

    if (!resp.success) throw new Error(resp.error || window.t('channels.msg.saveFailed'));

    const isNewChannel = !editingChannelId;
    const newChannelType = formData.channel_type;

    resetChannelFormDirty(); // 保存成功，重置dirty状态（避免closeModal弹确认框）
    closeModal();
    clearChannelsCache();

    // 新增渠道时，如果类型与当前筛选器不匹配，切换到新渠道的类型
    if (isNewChannel && filters.channelType !== 'all' && filters.channelType !== newChannelType) {
      filters.channelType = newChannelType;
      const typeFilter = document.getElementById('channelTypeFilter');
      if (typeFilter) typeFilter.value = newChannelType;
      if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
    }

    await loadChannels(filters.channelType);
    if (window.showSuccess) window.showSuccess(isNewChannel ? window.t('channels.channelAdded') : window.t('channels.channelUpdated'));
  } catch (e) {
    console.error('Save channel failed', e);
    if (window.showError) window.showError(window.t('channels.saveFailed', { error: e.message }));
  }
}

function deleteChannel(id, name) {
  deletingChannelId = id;
  document.getElementById('deleteChannelName').textContent = name;
  document.getElementById('deleteModal').classList.add('show');
}

function closeDeleteModal() {
  document.getElementById('deleteModal').classList.remove('show');
  deletingChannelId = null;
}

async function confirmDelete() {
  if (!deletingChannelId) return;

  try {
    const resp = await fetchAPIWithAuth(`/admin/channels/${deletingChannelId}`, {
      method: 'DELETE'
    });

    if (!resp.success) throw new Error(resp.error || window.t('common.failed'));

    closeDeleteModal();
    clearChannelsCache();
    await loadChannels(filters.channelType);
    if (window.showSuccess) window.showSuccess(window.t('channels.channelDeleted'));
  } catch (e) {
    console.error('Delete channel failed', e);
    if (window.showError) window.showError(window.t('channels.saveFailed', { error: e.message }));
  }
}

async function toggleChannel(id, enabled) {
  try {
    const resp = await fetchAPIWithAuth(`/admin/channels/${id}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ enabled })
    });
    if (!resp.success) throw new Error(resp.error || window.t('common.failed'));
    clearChannelsCache();
    await loadChannels(filters.channelType);
    if (window.showSuccess) window.showSuccess(enabled ? window.t('channels.channelEnabled') : window.t('channels.channelDisabled'));
  } catch (e) {
    console.error('Toggle failed', e);
    if (window.showError) window.showError(window.t('common.failed'));
  }
}

async function copyChannel(id, name) {
  const channel = channels.find(c => c.id === id);
  if (!channel) return;

  const copiedName = generateCopyName(name);

  editingChannelId = null;
  currentChannelKeyCooldowns = [];
  document.getElementById('modalTitle').textContent = window.t('channels.copyChannel');
  document.getElementById('channelName').value = copiedName;
  document.getElementById('channelUrl').value = channel.url;

  let apiKeys = [];
  try {
    apiKeys = (await fetchDataWithAuth(`/admin/channels/${id}/keys`)) || [];
  } catch (e) {
    console.error('Failed to fetch API Keys', e);
  }

  inlineKeyTableData = apiKeys.map(k => k.api_key || k);
  if (inlineKeyTableData.length === 0) {
    inlineKeyTableData = [''];
  }

  inlineKeyVisible = true;
  document.getElementById('inlineEyeIcon').style.display = 'none';
  document.getElementById('inlineEyeOffIcon').style.display = 'block';
  renderInlineKeyTable();

  const channelType = channel.channel_type || 'anthropic';
  const radioButton = document.querySelector(`input[name="channelType"][value="${channelType}"]`);
  if (radioButton) {
    radioButton.checked = true;
  }
  const keyStrategy = channel.key_strategy || 'sequential';
  const strategyRadio = document.querySelector(`input[name="keyStrategy"][value="${keyStrategy}"]`);
  if (strategyRadio) {
    strategyRadio.checked = true;
  }
  document.getElementById('channelPriority').value = channel.priority;
  document.getElementById('channelDailyCostLimit').value = channel.daily_cost_limit || 0;
  document.getElementById('channelEnabled').checked = true;

  // 加载模型配置（新格式：models是 {model, redirect_model} 数组）
  redirectTableData = (channel.models || []).map(m => ({
    model: m.model || '',
    redirect_model: m.redirect_model || ''
  }));
  selectedModelIndices.clear();
  currentModelFilter = '';
  const modelFilterInput = document.getElementById('modelFilterInput');
  if (modelFilterInput) modelFilterInput.value = '';
  renderRedirectTable();

  document.getElementById('channelModal').classList.add('show');
}

function generateCopyName(originalName) {
  const suffix = window.t('channels.copySuffix');
  // 匹配带有 " - 复制" 或 " - Copy" 后缀的名称
  const copyPattern = new RegExp(`^(.+?)(?:\\s*-\\s*${suffix}(?:\\s*(\\d+))?)?$`);
  const match = originalName.match(copyPattern);

  if (!match) {
    return originalName + ' - ' + suffix;
  }

  const baseName = match[1];
  const copyNumber = match[2] ? parseInt(match[2]) + 1 : 1;

  const proposedName = copyNumber === 1 ? `${baseName} - ${suffix}` : `${baseName} - ${suffix} ${copyNumber}`;

  const existingNames = channels.map(c => c.name.toLowerCase());
  if (existingNames.includes(proposedName.toLowerCase())) {
    return generateCopyName(proposedName);
  }

  return proposedName;
}

// 解析模型输入，支持逗号和换行分隔
// 支持格式：model 或 model:redirect 或 model->redirect
// 返回 [{model, redirect_model}] 数组
function parseModels(input) {
  const entries = input
    .split(/[,\n]/)
    .map(m => m.trim())
    .filter(m => m);

  const seen = new Set();
  const result = [];

  for (const entry of entries) {
    // 支持 model:redirect 或 model->redirect 格式
    const match = entry.match(/^([^:->]+)(?:[:->]+(.+))?$/);
    if (!match) continue;

    const model = match[1].trim();
    const redirect = match[2] ? match[2].trim() : model;

    if (model && !seen.has(model)) {
      seen.add(model);
      result.push({ model, redirect_model: redirect });
    }
  }

  return result;
}

function addRedirectRow() {
  openModelImportModal();
}

function openModelImportModal() {
  document.getElementById('modelImportTextarea').value = '';
  document.getElementById('modelImportPreviewContent').style.display = 'none';
  document.getElementById('modelImportModal').classList.add('show');
  setTimeout(() => document.getElementById('modelImportTextarea').focus(), 100);
}

function closeModelImportModal() {
  document.getElementById('modelImportModal').classList.remove('show');
}

function setupModelImportPreview() {
  const textarea = document.getElementById('modelImportTextarea');
  if (!textarea) return;

  textarea.addEventListener('input', () => {
    const input = textarea.value.trim();
    const previewContent = document.getElementById('modelImportPreviewContent');
    const countSpan = document.getElementById('modelImportCount');

    if (input) {
      const models = parseModels(input);
      if (models.length > 0) {
        countSpan.textContent = models.length;
        previewContent.style.display = 'block';
      } else {
        previewContent.style.display = 'none';
      }
    } else {
      previewContent.style.display = 'none';
    }
  });
}

function confirmModelImport() {
  const textarea = document.getElementById('modelImportTextarea');
  const input = textarea.value.trim();

  if (!input) {
    window.showNotification(window.t('channels.enterModelName'), 'warning');
    return;
  }

  const newModels = parseModels(input);
  if (newModels.length === 0) {
    window.showNotification(window.t('channels.noValidModelParsed'), 'warning');
    return;
  }

  // 获取现有模型名称用于去重
  const existingModels = new Set(redirectTableData.map(r => r.model));
  let addedCount = 0;

  newModels.forEach(entry => {
    if (!existingModels.has(entry.model)) {
      redirectTableData.push({ model: entry.model, redirect_model: entry.redirect_model });
      existingModels.add(entry.model);
      addedCount++;
    }
  });

  renderRedirectTable();
  closeModelImportModal();

  if (addedCount > 0) {
    const duplicateCount = newModels.length - addedCount;
    const msg = duplicateCount > 0
      ? window.t('channels.modelAddedWithDuplicates', { added: addedCount, duplicates: duplicateCount })
      : window.t('channels.modelAddedSuccess', { added: addedCount });
    window.showNotification(msg, 'success');
  } else {
    window.showNotification(window.t('channels.allModelsExist'), 'info');
  }
}

function deleteRedirectRow(index) {
  redirectTableData.splice(index, 1);
  // 更新选中状态：删除该索引，并调整后续索引
  const newSelectedIndices = new Set();
  selectedModelIndices.forEach(i => {
    if (i < index) {
      newSelectedIndices.add(i);
    } else if (i > index) {
      newSelectedIndices.add(i - 1);
    }
  });
  selectedModelIndices.clear();
  newSelectedIndices.forEach(i => selectedModelIndices.add(i));
  renderRedirectTable();
}

function updateRedirectRow(index, field, value) {
  if (redirectTableData[index]) {
    redirectTableData[index][field] = value.trim();

    // 当模型名称变化时，更新重定向目标的 placeholder
    if (field === 'model') {
      const tbody = document.getElementById('redirectTableBody');
      const row = tbody?.children[index];
      if (row) {
        const toInput = row.querySelector('.redirect-to-input');
        if (toInput) {
          toInput.placeholder = value.trim() || window.t('channels.leaveEmptyNoRedirect');
        }
      }
    }
  }
}

/**
 * 使用模板引擎创建重定向行元素
 * @param {Object} redirect - 重定向数据
 * @param {number} index - 索引
 * @returns {HTMLElement|null} 表格行元素
 */
function createRedirectRow(redirect, index) {
  const modelName = redirect.model || '';
  const rowData = {
    index: index,
    displayIndex: index + 1,
    from: modelName,
    to: redirect.redirect_model || '',
    toPlaceholder: modelName || window.t('channels.leaveEmptyNoRedirect')
  };

  const row = TemplateEngine.render('tpl-redirect-row', rowData);
  if (!row) {
    // 降级：模板不存在时使用原有方式
    console.warn('[Channels] Template tpl-redirect-row not found, using legacy rendering');
    return createRedirectRowLegacy(redirect, index);
  }

  // 设置复选框选中状态
  const checkbox = row.querySelector('.model-checkbox');
  if (checkbox) {
    checkbox.checked = selectedModelIndices.has(index);
  }

  return row;
}

/**
 * 初始化重定向表格事件委托 (替代inline onchange/onclick)
 */
function initRedirectTableEventDelegation() {
  const tbody = document.getElementById('redirectTableBody');
  if (!tbody || tbody.dataset.delegated) return;

  tbody.dataset.delegated = 'true';

  // 处理输入框变更
  tbody.addEventListener('change', (e) => {
    const fromInput = e.target.closest('.redirect-from-input');
    if (fromInput) {
      const index = parseInt(fromInput.dataset.index);
      updateRedirectRow(index, 'model', fromInput.value);
      return;
    }

    const toInput = e.target.closest('.redirect-to-input');
    if (toInput) {
      const index = parseInt(toInput.dataset.index);
      updateRedirectRow(index, 'redirect_model', toInput.value);
    }
  });

  // 处理删除按钮点击
  tbody.addEventListener('click', (e) => {
    const deleteBtn = e.target.closest('.redirect-delete-btn');
    if (deleteBtn) {
      const index = parseInt(deleteBtn.dataset.index);
      deleteRedirectRow(index);
    }
  });

  // 处理删除按钮悬停样式
  tbody.addEventListener('mouseover', (e) => {
    const btn = e.target.closest('.redirect-delete-btn');
    if (btn) {
      btn.style.background = 'var(--error-50)';
      btn.style.borderColor = 'var(--error-500)';
    }
  });

  tbody.addEventListener('mouseout', (e) => {
    const btn = e.target.closest('.redirect-delete-btn');
    if (btn) {
      btn.style.background = 'white';
      btn.style.borderColor = 'var(--error-300)';
    }
  });
}

/**
 * 获取筛选后的模型索引列表
 */
function getVisibleModelIndices() {
  if (!currentModelFilter) {
    return redirectTableData.map((_, index) => index);
  }
  const keyword = currentModelFilter.toLowerCase();
  return redirectTableData
    .map((item, index) => {
      const model = (item.model || '').toLowerCase();
      const redirect = (item.redirect_model || '').toLowerCase();
      if (model.includes(keyword) || redirect.includes(keyword)) {
        return index;
      }
      return null;
    })
    .filter(index => index !== null);
}

/**
 * 按关键字筛选模型
 */
function filterModelsByKeyword(keyword) {
  currentModelFilter = (keyword || '').trim();
  renderRedirectTable();
}

function renderRedirectTable() {
  const tbody = document.getElementById('redirectTableBody');
  const countSpan = document.getElementById('redirectCount');

  // 计数所有有效模型（只要有模型名称就算）
  const validCount = redirectTableData.filter(r => r.model && r.model.trim()).length;
  countSpan.textContent = validCount;

  // 初始化事件委托（仅一次）
  initRedirectTableEventDelegation();

  if (redirectTableData.length === 0) {
    const emptyRow = TemplateEngine.render('tpl-redirect-empty', {
      message: window.t('channels.noModelConfig')
    });
    if (emptyRow) {
      tbody.innerHTML = '';
      tbody.appendChild(emptyRow);
    } else {
      // 降级：模板不存在时使用简单HTML
      tbody.innerHTML = `<tr><td colspan="4" style="padding: 20px; text-align: center; color: var(--neutral-500);">${window.t('channels.noModelConfig')}</td></tr>`;
    }
    return;
  }

  // 获取筛选后的索引
  const visibleIndices = getVisibleModelIndices();

  if (visibleIndices.length === 0) {
    tbody.innerHTML = `<tr><td colspan="4" style="padding: 20px; text-align: center; color: var(--neutral-500);">${window.t('channels.noMatchingModels')}</td></tr>`;
    return;
  }

  // 使用DocumentFragment优化批量DOM操作
  const fragment = document.createDocumentFragment();
  visibleIndices.forEach(index => {
    const row = createRedirectRow(redirectTableData[index], index);
    if (row) fragment.appendChild(row);
  });

  tbody.innerHTML = '';
  tbody.appendChild(fragment);

  // 更新全选复选框和批量删除按钮状态
  updateSelectAllModelsCheckbox();
  updateModelBatchDeleteButton();

  // Translate dynamically rendered elements
  if (window.i18n && window.i18n.translatePage) {
    window.i18n.translatePage();
  }
}

// ===== 模型多选删除相关函数 =====

/**
 * 切换单个模型的选中状态
 */
function toggleModelSelection(index, checked) {
  if (checked) {
    selectedModelIndices.add(index);
  } else {
    selectedModelIndices.delete(index);
  }
  updateModelBatchDeleteButton();
  updateSelectAllModelsCheckbox();
}

/**
 * 全选/取消全选模型（仅操作当前可见的模型）
 */
function toggleSelectAllModels(checked) {
  const visibleIndices = getVisibleModelIndices();

  if (checked) {
    visibleIndices.forEach(index => selectedModelIndices.add(index));
  } else {
    visibleIndices.forEach(index => selectedModelIndices.delete(index));
  }

  updateModelBatchDeleteButton();
  renderRedirectTable();
}

/**
 * 更新批量删除按钮状态
 */
function updateModelBatchDeleteButton() {
  const btn = document.getElementById('batchDeleteModelsBtn');
  if (!btn) return;

  const count = selectedModelIndices.size;
  const textSpan = btn.querySelector('span');

  if (count > 0) {
    btn.disabled = false;
    if (textSpan) textSpan.textContent = window.t('channels.deleteSelectedCount', { count });
    btn.style.cursor = 'pointer';
    btn.style.opacity = '1';
    btn.style.background = 'linear-gradient(135deg, #fef2f2 0%, #fecaca 100%)';
    btn.style.borderColor = '#fca5a5';
    btn.style.color = '#dc2626';
  } else {
    btn.disabled = true;
    if (textSpan) textSpan.textContent = window.t('channels.deleteSelected');
    btn.style.cursor = '';
    btn.style.opacity = '0.5';
    btn.style.background = '';
    btn.style.borderColor = '';
    btn.style.color = '';
  }
}

/**
 * 更新全选复选框状态（基于当前可见的模型）
 */
function updateSelectAllModelsCheckbox() {
  const checkbox = document.getElementById('selectAllModels');
  if (!checkbox) return;

  const visibleIndices = getVisibleModelIndices();
  const visibleCount = visibleIndices.length;
  const selectedVisibleCount = visibleIndices.filter(i => selectedModelIndices.has(i)).length;

  if (visibleCount === 0) {
    checkbox.checked = false;
    checkbox.indeterminate = false;
  } else if (selectedVisibleCount === visibleCount) {
    checkbox.checked = true;
    checkbox.indeterminate = false;
  } else if (selectedVisibleCount > 0) {
    checkbox.checked = false;
    checkbox.indeterminate = true;
  } else {
    checkbox.checked = false;
    checkbox.indeterminate = false;
  }
}

/**
 * 批量删除选中的模型
 */
function batchDeleteSelectedModels() {
  const count = selectedModelIndices.size;
  if (count === 0) return;

  if (!confirm(window.t('channels.confirmBatchDeleteModels', { count }))) {
    return;
  }

  const tableContainer = document.querySelector('#redirectTableBody').closest('.inline-table-container');
  const scrollTop = tableContainer ? tableContainer.scrollTop : 0;

  // 从大到小排序，确保删除时索引不会错位
  const indicesToDelete = Array.from(selectedModelIndices).sort((a, b) => b - a);

  indicesToDelete.forEach(index => {
    redirectTableData.splice(index, 1);
  });

  selectedModelIndices.clear();
  updateModelBatchDeleteButton();

  renderRedirectTable();

  setTimeout(() => {
    if (tableContainer) {
      tableContainer.scrollTop = Math.min(scrollTop, tableContainer.scrollHeight - tableContainer.clientHeight);
    }
  }, 50);
}

function redirectTableToJSON() {
  const result = {};
  redirectTableData.forEach(redirect => {
    if (redirect.from && redirect.to) {
      result[redirect.from] = redirect.to;
    }
  });
  return result;
}

function jsonToRedirectTable(json) {
  if (!json || typeof json !== 'object') return [];
  return Object.entries(json).map(([from, to]) => ({ from, to }));
}

async function fetchModelsFromAPI() {
  const channelUrl = document.getElementById('channelUrl').value.trim();
  const channelType = document.querySelector('input[name="channelType"]:checked')?.value || 'anthropic';
  const firstValidKey = inlineKeyTableData
    .map(key => (key || '').trim())
    .filter(Boolean)[0];

  if (!channelUrl) {
    if (window.showError) {
      window.showError(window.t('channels.fillApiUrlFirst'));
    } else {
      alert(window.t('channels.fillApiUrlFirst'));
    }
    return;
  }

  if (!firstValidKey) {
    if (window.showError) {
      window.showError(window.t('channels.addAtLeastOneKey'));
    } else {
      alert(window.t('channels.addAtLeastOneKey'));
    }
    return;
  }

  const endpoint = '/admin/channels/models/fetch';
  const fetchOptions = {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      channel_type: channelType,
      url: channelUrl,
      api_key: firstValidKey
    })
  };

  try {
    const response = await fetchAPIWithAuth(endpoint, fetchOptions);
    if (!response.success) throw new Error(response.error || window.t('channels.fetchModelsFailed', { error: '' }));
    const data = response.data || {};

    if (!data.models || data.models.length === 0) {
      throw new Error(window.t('channels.noModelsFromApi'));
    }

    // 获取现有模型名称集合
    const existingModels = new Set(redirectTableData.map(r => r.model).filter(Boolean));

    // 添加新模型（不重复）- data.models 现在是 ModelEntry 数组
    let addedCount = 0;
    for (const entry of data.models) {
      const modelName = typeof entry === 'string' ? entry : entry.model;
      if (modelName && !existingModels.has(modelName)) {
        // 使用返回的 redirect_model，如果没有则使用 model
        const redirectModel = (typeof entry === 'object' && entry.redirect_model) ? entry.redirect_model : modelName;
        redirectTableData.push({ model: modelName, redirect_model: redirectModel });
        addedCount++;
      }
    }

    renderRedirectTable();

    const source = data.source === 'api' ? window.t('channels.fetchModelsSource.api') : window.t('channels.fetchModelsSource.predefined');
    if (window.showSuccess) {
      window.showSuccess(window.t('channels.fetchModelsSuccess', { added: addedCount, source, total: data.models.length }));
    } else {
      alert(window.t('channels.fetchModelsSuccess', { added: addedCount, source, total: data.models.length }));
    }

  } catch (error) {
    console.error('Fetch models failed', error);

    if (window.showError) {
      window.showError(window.t('channels.fetchModelsFailed', { error: error.message }));
    } else {
      alert(window.t('channels.fetchModelsFailed', { error: error.message }));
    }
  }
}

// 常用模型配置
const COMMON_MODELS = {
  anthropic: [
    'claude-sonnet-4-5-20250929',
    'claude-haiku-4-5-20251001',
    'claude-opus-4-5-20251101'
  ],
  codex: [
    'gpt-5.1',
    'gpt-5.1-codex',
    'gpt-5.1-codex-max',
    'gpt-5.2',
    'gpt-5.2-codex'
  ],
  gemini: [
    'gemini-2.5-flash',
    'gemini-2.5-pro',
    'gemini-2.5-flash-lite'
  ]
};

function addCommonModels() {
  const channelType = document.querySelector('input[name="channelType"]:checked')?.value || 'anthropic';
  const commonModels = COMMON_MODELS[channelType];

  if (!commonModels || commonModels.length === 0) {
    if (window.showWarning) {
      window.showWarning(window.t('channels.noPresetModels', { type: channelType }));
    } else {
      alert(window.t('channels.noPresetModels', { type: channelType }));
    }
    return;
  }

  // 获取现有模型名称集合
  const existingModels = new Set(redirectTableData.map(r => r.model).filter(Boolean));

  // 添加常用模型（不重复）
  let addedCount = 0;
  for (const modelName of commonModels) {
    if (!existingModels.has(modelName)) {
      redirectTableData.push({ model: modelName, redirect_model: '' });
      addedCount++;
    }
  }

  renderRedirectTable();

  if (window.showSuccess) {
    window.showSuccess(window.t('channels.addedCommonModels', { count: addedCount }));
  }
}
