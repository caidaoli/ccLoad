// 系统设置页面
initTopbar('settings');

let originalSettings = {}; // 保存原始值用于比较

function getSettingGroupInfo(key) {
  const k = String(key || '').toLowerCase();

  const defs = [
    { id: 'channel', name: '渠道与测试', order: 10, match: () => k.startsWith('channel_') || k === 'max_key_retries' },
    { id: 'model', name: '模型匹配', order: 15, match: () => k.startsWith('model_') },
    { id: 'timeout', name: '超时', order: 20, match: () => k.includes('timeout') },
    { id: 'health', name: '渠道动态排序', order: 30, match: () => k.includes('health_score') || k.includes('success_rate') || k.includes('penalty_weight') || k === 'enable_health_score' || k === 'health_min_confident_sample' },
    { id: 'cooldown', name: '冷却兜底', order: 40, match: () => k.startsWith('cooldown_') },
    { id: 'log', name: '日志', order: 50, match: () => k.startsWith('log_') },
    { id: 'access', name: '访问控制', order: 60, match: () => k.includes('88code') || k.includes('auth_') },
  ];

  for (const d of defs) {
    if (d.match()) return d;
  }
  return { id: 'other', name: '其他', order: 999 };
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
    g.settings.sort((a, b) => String(a.key).localeCompare(String(b.key)));
  }

  return groups;
}

function renderGroupNav(groups) {
  const nav = document.getElementById('settings-group-nav');
  if (!nav) return;

  nav.innerHTML = '';
  if (!groups || groups.length <= 1) return;

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

  const groups = groupSettings(settings);
  renderGroupNav(groups);

  for (const g of groups) {
    const groupRow = TemplateEngine.render('tpl-setting-group-row', {
      groupId: g.id,
      groupName: g.name
    });
    if (groupRow) tbody.appendChild(groupRow);

    for (const s of g.settings) {
      originalSettings[s.key] = s.value;
      const row = TemplateEngine.render('tpl-setting-row', {
        key: s.key,
        description: s.description,
        inputHtml: renderInput(s)
      });
      if (row) tbody.appendChild(row);
    }
  }
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
      const isTrue = setting.value === 'true' || setting.value === '1';
      return `
        <label style="margin-right: 16px; cursor: pointer;">
          <input type="radio" name="${safeKey}" value="true" ${isTrue ? 'checked' : ''} style="cursor: pointer;"> 启用
        </label>
        <label style="cursor: pointer;">
          <input type="radio" name="${safeKey}" value="false" ${!isTrue ? 'checked' : ''} style="cursor: pointer;"> 禁用
        </label>`;
    case 'int':
    case 'duration':
      return `<input type="number" id="${safeKey}" value="${safeValue}" style="${baseStyle} width: 100px; text-align: right;">`;
    default:
      return `<input type="text" id="${safeKey}" value="${safeValue}" style="${baseStyle} width: 280px;">`;
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

async function saveAllSettings() {
  // 收集所有变更
  const updates = {};
  const needsRestartKeys = [];
  const processedRadioGroups = new Set();

  for (const key of Object.keys(originalSettings)) {
    // 先尝试通过 id 查找（number/text 类型）
    let input = document.getElementById(key);
    let currentValue;

    if (input) {
      currentValue = input.value;
    } else {
      // 尝试通过 name 查找 radio 组（bool 类型）
      if (processedRadioGroups.has(key)) continue;
      const radios = document.querySelectorAll(`input[name="${key}"]`);
      if (radios.length > 0) {
        processedRadioGroups.add(key);
        const checkedRadio = document.querySelector(`input[name="${key}"]:checked`);
        currentValue = checkedRadio ? checkedRadio.value : '';
        input = radios[0]; // 用于获取 row
      } else {
        continue;
      }
    }

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
