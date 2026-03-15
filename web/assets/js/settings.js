// 系统设置页面
const t = window.t;

let originalSettings = {}; // 保存原始值用于比较

function bindSettingsPageActions() {
  const saveAllBtn = document.getElementById('save-all-btn');
  if (!saveAllBtn || saveAllBtn.dataset.bound) return;

  saveAllBtn.addEventListener('click', () => {
    saveAllSettings();
  });
  saveAllBtn.dataset.bound = '1';
}

function getSettingGroupInfo(key) {
  const k = String(key || '').toLowerCase();

  const defs = [
    { id: 'channel', nameKey: 'settings.group.channel', order: 10, match: () => k.startsWith('channel_') || k === 'max_key_retries' },
    { id: 'model', nameKey: 'settings.group.model', order: 15, match: () => k.startsWith('model_') },
    { id: 'timeout', nameKey: 'settings.group.timeout', order: 20, match: () => k.includes('timeout') },
    { id: 'health', nameKey: 'settings.group.health', order: 30, match: () => k.includes('health_score') || k.includes('success_rate') || k.includes('penalty_weight') || k === 'enable_health_score' || k === 'health_min_confident_sample' },
    { id: 'cooldown', nameKey: 'settings.group.cooldown', order: 40, match: () => k.startsWith('cooldown_') },
    { id: 'log', nameKey: 'settings.group.log', order: 50, match: () => k.startsWith('log_') },
    { id: 'access', nameKey: 'settings.group.access', order: 60, match: () => k.includes('auth_') },
  ];

  for (const d of defs) {
    if (d.match()) return { ...d, name: t(d.nameKey) };
  }
  return { id: 'other', nameKey: 'settings.group.other', name: t('settings.group.other'), order: 999 };
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
      groupName: g.name
    });
    if (groupRow) tbody.appendChild(groupRow);

    for (const s of g.settings) {
      originalSettings[s.key] = s.value;
      // 优先使用语言包中的描述，若没有则回退到后端返回的描述
      const descKey = `settings.desc.${s.key}`;
      const translatedDesc = t(descKey);
      const description = (translatedDesc !== descKey) ? translatedDesc : s.description;
      const row = TemplateEngine.render('tpl-setting-row', {
        key: s.key,
        description: description,
        inputHtml: renderInput(s),
        mobileLabelDescription: t('settings.configItem'),
        mobileLabelValue: t('settings.currentValue'),
        mobileLabelActions: t('common.actions')
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

  switch (setting.value_type) {
    case 'bool':
      const isTrue = setting.value === 'true' || setting.value === '1';
      return `
        <div class="settings-bool-group">
          <label class="settings-bool-option">
            <input type="radio" name="${safeKey}" value="true" ${isTrue ? 'checked' : ''}> ${t('common.enable')}
          </label>
          <label class="settings-bool-option">
            <input type="radio" name="${safeKey}" value="false" ${!isTrue ? 'checked' : ''}> ${t('common.disable')}
          </label>
        </div>`;
    case 'int':
    case 'duration':
      return `<input type="number" id="${safeKey}" value="${safeValue}" class="settings-input settings-input--number">`;
    default:
      return `<input type="text" id="${safeKey}" value="${safeValue}" class="settings-input settings-input--text">`;
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
    window.showNotification(t('settings.msg.noChanges'), 'info');
    return;
  }

  // 使用批量更新接口（单次请求，事务保护）
  try {
    await fetchDataWithAuth('/admin/settings/batch', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(updates)
    });
    let msg = t('settings.msg.savedCount', { count: Object.keys(updates).length });
    if (needsRestartKeys.length > 0) {
      msg += `\n\n${t('settings.msg.restartRequired')}:\n${needsRestartKeys.join(', ')}`;
    }
    showSuccess(msg);
  } catch (err) {
    console.error('保存异常:', err);
    showError(t('settings.msg.saveFailed') + ': ' + err.message);
  }

  loadSettings();
}

async function resetSetting(key) {
  if (!confirm(t('settings.msg.confirmReset', { key }))) return;

  try {
    await fetchDataWithAuth(`/admin/settings/${key}/reset`, { method: 'POST' });
    showSuccess(t('settings.msg.resetSuccess', { key }));
    loadSettings();
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
