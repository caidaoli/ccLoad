function showAddModal() {
  editingChannelId = null;
  currentChannelKeyCooldowns = [];
  
  document.getElementById('modalTitle').textContent = '添加渠道';
  document.getElementById('channelForm').reset();
  document.getElementById('channelEnabled').checked = true;
  document.querySelector('input[name="channelType"][value="anthropic"]').checked = true;
  document.querySelector('input[name="keyStrategy"][value="sequential"]').checked = true;

  redirectTableData = [];
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

  document.getElementById('modalTitle').textContent = '编辑渠道';
  document.getElementById('channelName').value = channel.name;
  document.getElementById('channelUrl').value = channel.url;

  let apiKeys = [];
  try {
    apiKeys = (await fetchDataWithAuth(`/admin/channels/${id}/keys`)) || [];
  } catch (e) {
    console.error('获取API Keys失败', e);
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
  document.getElementById('channelModels').value = channel.models.join(',');
  document.getElementById('channelEnabled').checked = channel.enabled;

  const modelRedirects = channel.model_redirects || {};
  redirectTableData = jsonToRedirectTable(modelRedirects);
  renderRedirectTable();

  document.getElementById('channelModal').classList.add('show');
}

function closeModal() {
  document.getElementById('channelModal').classList.remove('show');
  editingChannelId = null;
}

async function saveChannel(event) {
  event.preventDefault();

  const validKeys = inlineKeyTableData.filter(k => k && k.trim());
  if (validKeys.length === 0) {
    alert('请至少添加一个有效的API Key');
    return;
  }

  document.getElementById('channelApiKey').value = validKeys.join(',');

  const modelRedirects = redirectTableToJSON();

  const channelType = document.querySelector('input[name="channelType"]:checked')?.value || 'anthropic';
  const keyStrategy = document.querySelector('input[name="keyStrategy"]:checked')?.value || 'sequential';

  const formData = {
    name: document.getElementById('channelName').value.trim(),
    url: document.getElementById('channelUrl').value.trim(),
    api_key: validKeys.join(','),
    channel_type: channelType,
    key_strategy: keyStrategy,
    priority: parseInt(document.getElementById('channelPriority').value) || 0,
    models: document.getElementById('channelModels').value.split(',').map(m => m.trim()).filter(m => m),
    model_redirects: modelRedirects,
    enabled: document.getElementById('channelEnabled').checked
  };

  if (!formData.name || !formData.url || !formData.api_key || formData.models.length === 0) {
    if (window.showError) window.showError('请填写所有必填字段');
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

    if (!resp.success) throw new Error(resp.error || '保存失败');

    closeModal();
    clearChannelsCache();
    await loadChannels(filters.channelType);
    if (window.showSuccess) window.showSuccess(editingChannelId ? '渠道已更新' : '渠道已添加');
  } catch (e) {
    console.error('保存渠道失败', e);
    if (window.showError) window.showError('保存失败: ' + e.message);
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

    if (!resp.success) throw new Error(resp.error || '删除失败');

    closeDeleteModal();
    clearChannelsCache();
    await loadChannels(filters.channelType);
    if (window.showSuccess) window.showSuccess('渠道已删除');
  } catch (e) {
    console.error('删除渠道失败', e);
    if (window.showError) window.showError('删除失败: ' + e.message);
  }
}

async function toggleChannel(id, enabled) {
  try {
    const resp = await fetchAPIWithAuth(`/admin/channels/${id}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ enabled })
    });
    if (!resp.success) throw new Error(resp.error || '操作失败');
    clearChannelsCache();
    await loadChannels(filters.channelType);
    if (window.showSuccess) window.showSuccess(enabled ? '渠道已启用' : '渠道已禁用');
  } catch (e) {
    console.error('切换失败', e);
    if (window.showError) window.showError('操作失败');
  }
}

async function copyChannel(id, name) {
  const channel = channels.find(c => c.id === id);
  if (!channel) return;

  const copiedName = generateCopyName(name);

  editingChannelId = null;
  currentChannelKeyCooldowns = [];
  document.getElementById('modalTitle').textContent = '复制渠道';
  document.getElementById('channelName').value = copiedName;
  document.getElementById('channelUrl').value = channel.url;

  let apiKeys = [];
  try {
    apiKeys = (await fetchDataWithAuth(`/admin/channels/${id}/keys`)) || [];
  } catch (e) {
    console.error('获取API Keys失败', e);
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
  document.getElementById('channelModels').value = channel.models.join(',');
  document.getElementById('channelEnabled').checked = true;

  const modelRedirects = channel.model_redirects || {};
  redirectTableData = jsonToRedirectTable(modelRedirects);
  renderRedirectTable();

  document.getElementById('channelModal').classList.add('show');
}

function generateCopyName(originalName) {
  const copyPattern = /^(.+?)(?:\s*-\s*复制(?:\s*(\d+))?)?$/;
  const match = originalName.match(copyPattern);

  if (!match) {
    return originalName + ' - 复制';
  }

  const baseName = match[1];
  const copyNumber = match[2] ? parseInt(match[2]) + 1 : 1;

  const proposedName = copyNumber === 1 ? `${baseName} - 复制` : `${baseName} - 复制 ${copyNumber}`;

  const existingNames = channels.map(c => c.name.toLowerCase());
  if (existingNames.includes(proposedName.toLowerCase())) {
    return generateCopyName(proposedName);
  }

  return proposedName;
}

function addRedirectRow() {
  redirectTableData.push({ from: '', to: '' });
  renderRedirectTable();
  
  setTimeout(() => {
    const tbody = document.getElementById('redirectTableBody');
    const lastRow = tbody.lastElementChild;
    if (lastRow) {
      const firstInput = lastRow.querySelector('input');
      if (firstInput) firstInput.focus();
    }
  }, 50);
}

function deleteRedirectRow(index) {
  redirectTableData.splice(index, 1);
  renderRedirectTable();
}

function updateRedirectRow(index, field, value) {
  if (redirectTableData[index]) {
    redirectTableData[index][field] = value.trim();
  }
}

/**
 * 使用模板引擎创建重定向行元素
 * @param {Object} redirect - 重定向数据
 * @param {number} index - 索引
 * @returns {HTMLElement|null} 表格行元素
 */
function createRedirectRow(redirect, index) {
  const rowData = {
    index: index,
    from: redirect.from || '',
    to: redirect.to || ''
  };

  const row = TemplateEngine.render('tpl-redirect-row', rowData);
  if (!row) {
    // 降级：模板不存在时使用原有方式
    console.warn('[Channels] Template tpl-redirect-row not found, using legacy rendering');
    return createRedirectRowLegacy(redirect, index);
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
      updateRedirectRow(index, 'from', fromInput.value);
      return;
    }

    const toInput = e.target.closest('.redirect-to-input');
    if (toInput) {
      const index = parseInt(toInput.dataset.index);
      updateRedirectRow(index, 'to', toInput.value);
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

function renderRedirectTable() {
  const tbody = document.getElementById('redirectTableBody');
  const countSpan = document.getElementById('redirectCount');

  const validCount = redirectTableData.filter(r => r.from && r.to).length;
  countSpan.textContent = validCount;

  // 初始化事件委托（仅一次）
  initRedirectTableEventDelegation();

  if (redirectTableData.length === 0) {
    const emptyRow = TemplateEngine.render('tpl-redirect-empty', {
      message: '暂无重定向规则，点击"添加"按钮创建'
    });
    if (emptyRow) {
      tbody.innerHTML = '';
      tbody.appendChild(emptyRow);
    } else {
      // 降级：模板不存在时使用简单HTML
      tbody.innerHTML = '<tr><td colspan="3" style="padding: 20px; text-align: center; color: var(--neutral-500);">暂无重定向规则，点击"添加"按钮创建</td></tr>';
    }
    return;
  }

  // 使用DocumentFragment优化批量DOM操作
  const fragment = document.createDocumentFragment();
  redirectTableData.forEach((redirect, index) => {
    const row = createRedirectRow(redirect, index);
    if (row) fragment.appendChild(row);
  });

  tbody.innerHTML = '';
  tbody.appendChild(fragment);
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
      window.showError('请先填写API URL');
    } else {
      alert('请先填写API URL');
    }
    return;
  }

  if (!firstValidKey) {
    if (window.showError) {
      window.showError('请至少添加一个API Key');
    } else {
      alert('请至少添加一个API Key');
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

  const modelsTextarea = document.getElementById('channelModels');
  const originalValue = modelsTextarea.value;
  const originalPlaceholder = modelsTextarea.placeholder;

  modelsTextarea.disabled = true;
  modelsTextarea.placeholder = '正在获取模型列表...';

  try {
    const response = await fetchAPIWithAuth(endpoint, fetchOptions);
    if (!response.success) throw new Error(response.error || '获取模型列表失败');
    const data = response.data || {};

    if (!data.models || data.models.length === 0) {
      throw new Error('未获取到任何模型');
    }

    const existingModels = originalValue.split(',').map(m => m.trim()).filter(m => m);
    const allModels = [...new Set([...existingModels, ...data.models])];

    modelsTextarea.value = allModels.join(',');

    const source = data.source === 'api' ? '从API获取' : '预定义列表';
    if (window.showSuccess) {
      window.showSuccess(`成功获取 ${data.models.length} 个模型 (${source})`);
    } else {
      alert(`成功获取 ${data.models.length} 个模型 (${source})`);
    }

  } catch (error) {
    console.error('获取模型列表失败', error);

    modelsTextarea.value = originalValue;

    if (window.showError) {
      window.showError('获取模型列表失败: ' + error.message);
    } else {
      alert('获取模型列表失败: ' + error.message);
    }
  } finally {
    modelsTextarea.disabled = false;
    modelsTextarea.placeholder = originalPlaceholder;
  }
}

function clearAllModels() {
  if (confirm('确定要清除所有模型吗？此操作不可恢复！')) {
    const modelsTextarea = document.getElementById('channelModels');
    modelsTextarea.value = '';
    modelsTextarea.focus();
  }
}
