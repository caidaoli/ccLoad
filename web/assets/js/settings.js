// 系统设置页面
initTopbar('settings');

let originalSettings = {}; // 保存原始值用于比较

async function loadSettings() {
  try {
    const data = await fetchDataWithAuth('/admin/settings');
    if (!Array.isArray(data)) throw new Error('响应不是数组');
    renderSettings(data);
  } catch (err) {
    console.error('加载配置异常:', err);
    showError('加载配置异常: ' + err.message);
  }
}

function renderSettings(settings) {
  const tbody = document.getElementById('settings-tbody');
  originalSettings = {};
  tbody.innerHTML = '';

  // 初始化事件委托（仅一次）
  initSettingsEventDelegation();

  settings.forEach(s => {
    originalSettings[s.key] = s.value;
    const row = TemplateEngine.render('tpl-setting-row', {
      key: s.key,
      description: s.description,
      inputHtml: renderInput(s)
    });
    if (row) tbody.appendChild(row);
  });
}

// 初始化事件委托（替代 inline onclick）
function initSettingsEventDelegation() {
  const tbody = document.getElementById('settings-tbody');
  if (!tbody || tbody.dataset.delegated) return;
  tbody.dataset.delegated = 'true';

  // 重置按钮点击
  tbody.addEventListener('click', (e) => {
    const resetBtn = e.target.closest('.setting-reset-btn');
    if (resetBtn) {
      resetSetting(resetBtn.dataset.key);
    }
  });

  // 输入变更
  tbody.addEventListener('change', (e) => {
    const input = e.target.closest('input');
    if (input) markChanged(input);
  });
}

function renderInput(setting) {
  const safeKey = escapeHtml(setting.key);
  const safeValue = escapeHtml(setting.value);
  const baseStyle = 'padding: 6px 10px; border: 1px solid var(--color-border); border-radius: 6px; background: var(--color-bg-secondary); color: var(--color-text); font-size: 13px;';

  switch (setting.value_type) {
    case 'bool':
      const checked = setting.value === 'true' || setting.value === '1';
      return `<input type="checkbox" id="${safeKey}" ${checked ? 'checked' : ''} style="width: 18px; height: 18px; cursor: pointer;">`;
    case 'int':
    case 'duration':
      return `<input type="number" id="${safeKey}" value="${safeValue}" style="${baseStyle} width: 100px; text-align: right;">`;
    default:
      return `<input type="text" id="${safeKey}" value="${safeValue}" style="${baseStyle} width: 280px;">`;
  }
}

function markChanged(input) {
  const key = input.id;
  const row = input.closest('tr');

  const currentValue = input.type === 'checkbox' ? (input.checked ? 'true' : 'false') : input.value;
  if (currentValue !== originalSettings[key]) {
    row.style.background = 'rgba(59, 130, 246, 0.08)';
  } else {
    row.style.background = '';
  }
}

async function saveAllSettings() {
  // 收集所有变更
  const updates = {};
  const needsRestartKeys = [];

  for (const key of Object.keys(originalSettings)) {
    const input = document.getElementById(key);
    if (!input) continue;

    const currentValue = input.type === 'checkbox' ? (input.checked ? 'true' : 'false') : input.value;
    if (currentValue !== originalSettings[key]) {
      updates[key] = currentValue;
      // 检查是否需要重启（从 DOM 中读取 description）
      const row = input.closest('tr');
      if (row?.querySelector('td')?.textContent?.includes('[需重启]')) {
        needsRestartKeys.push(key);
      }
    }
  }

  if (Object.keys(updates).length === 0) {
    showInfo('没有需要保存的更改');
    return;
  }

  // 使用批量更新接口（单次请求，事务保护）
  try {
    await fetchDataWithAuth('/admin/settings/batch', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(updates)
    });
    let msg = `已保存 ${Object.keys(updates).length} 项配置`;
    if (needsRestartKeys.length > 0) {
      msg += `\n\n以下配置需要重启服务才能生效:\n${needsRestartKeys.join(', ')}`;
    }
    showSuccess(msg);
  } catch (err) {
    console.error('保存异常:', err);
    showError('保存异常: ' + err.message);
  }

  loadSettings();
}

async function resetSetting(key) {
  if (!confirm(`确定要重置 "${key}" 为默认值吗?`)) return;

  try {
    await fetchDataWithAuth(`/admin/settings/${key}/reset`, { method: 'POST' });
    showSuccess(`配置 ${key} 已重置为默认值`);
    loadSettings();
  } catch (err) {
    console.error('重置异常:', err);
    showError('重置异常: ' + err.message);
  }
}

// showSuccess/showError 已在 ui.js 中定义（toast 通知），无需重复定义
function showInfo(msg) {
  window.showNotification(msg, 'info');
}

// 页面加载时执行
loadSettings();
