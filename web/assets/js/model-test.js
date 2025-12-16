// 模型测试页面
initTopbar('model-test');

let channelsList = [];
let selectedChannel = null;
let newModels = new Set(); // 新获取的模型

// 加载默认测试内容
async function loadDefaultTestContent() {
  try {
    const settings = await fetchDataWithAuth('/admin/settings');
    if (!Array.isArray(settings)) return;
    const setting = settings.find(s => s.key === 'channel_test_content');
    if (setting) {
      document.getElementById('modelTestContent').value = setting.value;
      document.getElementById('modelTestContent').placeholder = '';
    }
  } catch (e) {
    console.error('加载默认测试内容失败:', e);
  }
}

// 加载渠道列表
async function loadChannels() {
  try {
    const list = (await fetchDataWithAuth('/admin/channels')) || [];
    channelsList = list.sort((a, b) => a.channel_type.localeCompare(b.channel_type) || b.priority - a.priority);
    const select = document.getElementById('testChannelSelect');
    select.innerHTML = '<option value="">选择...</option>';
    channelsList.forEach(ch => {
      const opt = document.createElement('option');
      opt.value = ch.id;
      opt.textContent = `[${ch.channel_type}] ${ch.name}`;
      select.appendChild(opt);
    });
  } catch (e) {
    console.error('加载渠道列表失败:', e);
    showError('加载渠道列表失败');
  }
}

// 渲染模型列表
function renderModelList() {
  const tbody = document.getElementById('model-test-tbody');
  const models = selectedChannel?.models || [];

  if (models.length === 0) {
    tbody.innerHTML = '';
    const emptyRow = TemplateEngine.render('tpl-empty-row', { message: '该渠道没有配置模型' });
    if (emptyRow) tbody.appendChild(emptyRow);
    return;
  }

  const fragment = document.createDocumentFragment();
  models.forEach(model => {
    const isNew = newModels.has(model);
    const row = TemplateEngine.render('tpl-model-row', {
      model: model,
      displayName: isNew ? `[新] ${model}` : model,
      nameStyle: isNew ? ' color: var(--success-500);' : ''
    });
    if (row) fragment.appendChild(row);
  });

  tbody.innerHTML = '';
  tbody.appendChild(fragment);
  document.getElementById('selectAllCheckbox').checked = true;
}

// 渠道切换
async function onChannelChange() {
  const select = document.getElementById('testChannelSelect');
  const channelId = parseInt(select.value);
  selectedChannel = channelsList.find(c => c.id === channelId) || null;

  if (!selectedChannel) {
    const tbody = document.getElementById('model-test-tbody');
    tbody.innerHTML = '';
    const emptyRow = TemplateEngine.render('tpl-empty-row', { message: '请先选择渠道' });
    if (emptyRow) tbody.appendChild(emptyRow);
    return;
  }

  // 更新渠道类型
  const channelType = selectedChannel.channel_type || 'anthropic';
  await window.ChannelTypeManager.renderChannelTypeSelect('testChannelType', channelType);

  renderModelList();
}

function selectAllModels() {
  document.querySelectorAll('.model-checkbox').forEach(cb => cb.checked = true);
  document.getElementById('selectAllCheckbox').checked = true;
}

function deselectAllModels() {
  document.querySelectorAll('.model-checkbox').forEach(cb => cb.checked = false);
  document.getElementById('selectAllCheckbox').checked = false;
}

function toggleAllModels(checked) {
  document.querySelectorAll('.model-checkbox').forEach(cb => cb.checked = checked);
}

function getSelectedModels() {
  const rows = document.querySelectorAll('#model-test-tbody tr[data-model]');
  const selected = [];
  rows.forEach(row => {
    const cb = row.querySelector('.model-checkbox');
    if (cb?.checked) selected.push({ model: row.dataset.model, row });
  });
  return selected;
}

async function runModelTests() {
  if (!selectedChannel) {
    showError('请先选择渠道');
    return;
  }

  const rows = document.querySelectorAll('#model-test-tbody tr[data-model]');
  const selectedModels = [];
  rows.forEach(row => {
    const cb = row.querySelector('.model-checkbox');
    if (cb && cb.checked) {
      selectedModels.push({ model: row.dataset.model, row });
    }
  });

  if (selectedModels.length === 0) {
    showError('请至少选择一个模型');
    return;
  }

  const content = document.getElementById('modelTestContent').value.trim() || 'hi';
  const channelType = document.getElementById('testChannelType').value;
  const streamEnabled = document.getElementById('streamEnabled').checked;
  const progressEl = document.getElementById('testProgress');
  const runBtn = document.getElementById('runTestBtn');

  runBtn.disabled = true;

  // 重置所有选中行的状态
  selectedModels.forEach(({ row }) => {
    row.querySelector('.duration').textContent = '-';
    row.querySelector('.input-tokens').textContent = '-';
    row.querySelector('.output-tokens').textContent = '-';
    row.querySelector('.cache-read').textContent = '-';
    row.querySelector('.cache-create').textContent = '-';
    row.querySelector('.response').textContent = '等待中...';
    row.querySelector('.response').title = '';
    row.style.background = '';
  });

  let completed = 0;
  const total = selectedModels.length;
  const concurrency = parseInt(document.getElementById('concurrency').value) || 5;

  const testModel = async ({ model, row }) => {
    row.querySelector('.response').textContent = '测试中...';
    try {
      const data = await fetchDataWithAuth(`/admin/channels/${selectedChannel.id}/test`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ model, max_tokens: 512, stream: streamEnabled, content, channel_type: channelType })
      });

      row.querySelector('.duration').textContent = data.duration_ms ? `${data.duration_ms}ms` : '-';

      if (data.success) {
        row.style.background = 'rgba(16, 185, 129, 0.1)';
        const usage = data.api_response?.usage || {};
        row.querySelector('.input-tokens').textContent = usage.input_tokens || usage.prompt_tokens || '-';
        row.querySelector('.output-tokens').textContent = usage.output_tokens || usage.completion_tokens || '-';
        row.querySelector('.cache-read').textContent = usage.cache_read_input_tokens || '-';
        row.querySelector('.cache-create').textContent = usage.cache_creation_input_tokens || '-';

        let respText = data.response_text;
        if (!respText && data.api_response?.choices?.[0]?.message) {
          const msg = data.api_response.choices[0].message;
          respText = msg.content || msg.reasoning_content || msg.reasoning || msg.text;
        }
        row.querySelector('.response').textContent = respText || '成功';
        row.querySelector('.response').title = respText || '成功';
      } else {
        row.style.background = 'rgba(239, 68, 68, 0.1)';
        const errMsg = data.error || '测试失败';
        row.querySelector('.response').textContent = errMsg;
        row.querySelector('.response').title = errMsg;
      }
    } catch (e) {
      row.style.background = 'rgba(239, 68, 68, 0.1)';
      row.querySelector('.duration').textContent = '-';
      row.querySelector('.response').textContent = '请求失败';
      row.querySelector('.response').title = e.message;
    }
    completed++;
    progressEl.textContent = `测试中 ${completed}/${total}`;
  };

  // 并发控制
  const queue = [...selectedModels];
  const workers = Array(Math.min(concurrency, queue.length)).fill(null).map(async () => {
    while (queue.length) await testModel(queue.shift());
  });
  await Promise.all(workers);

  progressEl.textContent = `完成 ${total}/${total}`;
  runBtn.disabled = false;

  // 测试完成后只选中失败的行
  document.querySelectorAll('#model-test-tbody tr[data-model]').forEach(row => {
    const cb = row.querySelector('.model-checkbox');
    cb.checked = row.style.background.includes('239, 68, 68');
  });
  document.getElementById('selectAllCheckbox').checked = false;
}

// 获取并添加模型
async function fetchAndAddModels() {
  if (!selectedChannel) return showError('请先选择渠道');
  const channelType = document.getElementById('testChannelType').value;

  try {
    const resp = await fetchAPIWithAuth(`/admin/channels/${selectedChannel.id}/models/fetch?channel_type=${channelType}`);
    if (resp.success && resp.data && resp.data.models) {
      const existingSet = new Set(selectedChannel.models);
      const fetched = resp.data.models;
      const newOnes = fetched.filter(m => !existingSet.has(m));

      if (newOnes.length > 0) {
        // 保存到后端
        const saveResp = await fetchAPIWithAuth(`/admin/channels/${selectedChannel.id}/models`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ models: newOnes })
        });
        if (!saveResp.success) throw new Error(saveResp.error || '保存模型失败');
        newOnes.forEach(m => newModels.add(m));
      }

      selectedChannel.models = [...new Set([...selectedChannel.models, ...fetched])];
      renderModelList();
      showSuccess(`获取到 ${fetched.length} 个模型，新增 ${newOnes.length} 个`);
    } else {
      showError(resp.error || '获取模型失败');
    }
  } catch (e) {
    showError(e.message || '获取模型失败');
  }
}

// 删除选中的模型
async function deleteSelectedModels() {
  if (!selectedChannel) return showError('请先选择渠道');
  const selected = getSelectedModels();
  if (selected.length === 0) return showError('请先选择要删除的模型');

  if (!confirm(`是否删除选择的 ${selected.map(s => s.model).join(', ')}？`)) return;

  try {
    const resp = await fetchAPIWithAuth(`/admin/channels/${selectedChannel.id}/models`, {
      method: 'DELETE',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ models: selected.map(s => s.model) })
    });
    if (resp.success) {
      selectedChannel.models = selectedChannel.models.filter(m => !selected.some(s => s.model === m));
      selected.forEach(({ row }) => row.remove());
      showSuccess('删除成功');
    } else {
      showError(resp.error || '删除失败');
    }
  } catch (e) {
    showError(e.message || '删除失败');
  }
}

// 页面初始化
loadChannels();
loadDefaultTestContent();
window.ChannelTypeManager.renderChannelTypeSelect('testChannelType', 'anthropic');
