function highlightFromHash() {
  const m = (location.hash || '').match(/^#channel-(\d+)$/);
  if (!m) return;
  const el = document.getElementById(`channel-${m[1]}`);
  if (!el) return;
  el.scrollIntoView({ behavior: 'smooth', block: 'center' });
  const prev = el.style.boxShadow;
  el.style.transition = 'box-shadow 0.3s ease, background 0.3s ease';
  el.style.boxShadow = '0 0 0 3px rgba(59,130,246,0.35), 0 10px 25px rgba(59,130,246,0.20)';
  el.style.background = 'rgba(59,130,246,0.06)';
  setTimeout(() => {
    el.style.boxShadow = prev || '';
    el.style.background = '';
  }, 1600);
}

// 从URL参数获取目标渠道ID，查询其类型并返回
async function getTargetChannelType() {
  const params = new URLSearchParams(location.search);
  const channelId = params.get('id');
  if (!channelId) return null;

  try {
    const channel = await fetchDataWithAuth(`/admin/channels/${channelId}`);
    return channel.channel_type || 'anthropic';
  } catch (e) {
    console.error('Failed to get channel type:', e);
    return null;
  }
}

// localStorage key for channels page filters
const CHANNELS_FILTER_KEY = 'channels.filters';

function saveChannelsFilters() {
  try {
    localStorage.setItem(CHANNELS_FILTER_KEY, JSON.stringify({
      channelType: filters.channelType,
      status: filters.status,
      model: filters.model,
      search: filters.search,
      id: filters.id
    }));
  } catch (_) {}
}

function loadChannelsFilters() {
  try {
    const saved = localStorage.getItem(CHANNELS_FILTER_KEY);
    if (saved) return JSON.parse(saved);
  } catch (_) {}
  return null;
}

document.addEventListener('DOMContentLoaded', async () => {
  // Translate static elements first
  if (window.i18n && window.i18n.translatePage) {
    window.i18n.translatePage();
  }

  if (window.initTopbar) initTopbar('channels');
  setupFilterListeners();
  setupImportExport();
  setupKeyImportPreview();
  setupModelImportPreview();

  await window.ChannelTypeManager.renderChannelTypeRadios('channelTypeRadios');

  // 优先从 localStorage 恢复，其次检查 URL 参数，最后默认 all
  const savedFilters = loadChannelsFilters();
  const targetChannelType = await getTargetChannelType();
  const initialType = targetChannelType || (savedFilters?.channelType) || 'all';

  filters.channelType = initialType;
  if (savedFilters) {
    filters.status = savedFilters.status || 'all';
    filters.model = savedFilters.model || 'all';
    filters.search = savedFilters.search || '';
    filters.id = savedFilters.id || '';
    document.getElementById('statusFilter').value = filters.status;
    const modelFilterEl = document.getElementById('modelFilter');
    if (modelFilterEl) {
      modelFilterEl.value = (typeof modelFilterInputValueFromFilterValue === 'function')
        ? modelFilterInputValueFromFilterValue(filters.model)
        : (filters.model === 'all' ? window.t('channels.modelAll') : filters.model);
    }
    document.getElementById('searchInput').value = filters.search;
    document.getElementById('idFilter').value = filters.id;
  }

  // 初始化渠道类型筛选器（替换原Tab逻辑）
  await initChannelTypeFilter(initialType);

  await loadDefaultTestContent();
  await loadChannelStatsRange();

  await loadChannels(initialType);
  await loadChannelStats();
  highlightFromHash();
  window.addEventListener('hashchange', highlightFromHash);

  // 监听语言切换事件，重新渲染渠道列表
  window.i18n.onLocaleChange(() => {
    renderChannels();
    updateModelOptions();
  });
});

// 初始化渠道类型筛选器
async function initChannelTypeFilter(initialType) {
  const select = document.getElementById('channelTypeFilter');
  if (!select) return;

  const types = await window.ChannelTypeManager.getChannelTypes();

  // Add "All" option
  select.innerHTML = `<option value="all">${window.t('common.all')}</option>`;
  types.forEach(type => {
    const option = document.createElement('option');
    option.value = type.value;
    option.textContent = type.display_name;
    if (type.value === initialType) {
      option.selected = true;
    }
    select.appendChild(option);
  });

  // 绑定change事件
  select.addEventListener('change', (e) => {
    const type = e.target.value;
    filters.channelType = type;
    filters.model = 'all';
    // 使用通用组件更新模型筛选器
    if (typeof modelFilterCombobox !== 'undefined' && modelFilterCombobox) {
      modelFilterCombobox.setValue('all', modelFilterInputValueFromFilterValue('all'));
    } else {
      const modelFilterEl = document.getElementById('modelFilter');
      if (modelFilterEl) {
        modelFilterEl.value = (typeof modelFilterInputValueFromFilterValue === 'function')
          ? modelFilterInputValueFromFilterValue('all')
          : window.t('channels.modelAll');
      }
    }
    saveChannelsFilters();
    loadChannels(type);
  });
}

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    // 按层级优先关闭最上层模态框
    const modelImportModal = document.getElementById('modelImportModal');
    const keyImportModal = document.getElementById('keyImportModal');
    const keyExportModal = document.getElementById('keyExportModal');
    const sortModal = document.getElementById('sortModal');
    const deleteModal = document.getElementById('deleteModal');
    const testModal = document.getElementById('testModal');
    const channelModal = document.getElementById('channelModal');

    if (modelImportModal && modelImportModal.classList.contains('show')) {
      closeModelImportModal();
    } else if (keyImportModal && keyImportModal.classList.contains('show')) {
      closeKeyImportModal();
    } else if (keyExportModal && keyExportModal.classList.contains('show')) {
      closeKeyExportModal();
    } else if (sortModal && sortModal.classList.contains('show')) {
      closeSortModal();
    } else if (deleteModal && deleteModal.classList.contains('show')) {
      closeDeleteModal();
    } else if (testModal && testModal.classList.contains('show')) {
      closeTestModal();
    } else if (channelModal && channelModal.classList.contains('show')) {
      closeModal();
    }
  }
});
