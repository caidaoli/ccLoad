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
      search: filters.search
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

function initChannelsPageActions() {
  if (typeof initChannelEditorActions === 'function') {
    initChannelEditorActions();
  }

  if (typeof window.initDelegatedActions === 'function') {
    window.initDelegatedActions({
      boundKey: 'channelsPageActionsBound',
      click: {
        'show-add-modal': () => showAddModal(),
        'batch-enable-channels': () => batchEnableSelectedChannels(),
        'batch-disable-channels': () => batchDisableSelectedChannels(),
        'batch-delete-channels': () => batchDeleteSelectedChannels(),
        'batch-refresh-channels-merge': () => batchRefreshSelectedChannelsMerge(),
        'batch-refresh-channels-replace': () => batchRefreshSelectedChannelsReplace(),
        'clear-selected-channels': () => clearSelectedChannels(),
        'close-test-modal': () => closeTestModal(),
        'run-channel-test': () => runChannelTest(),
        'run-batch-test': () => runBatchTest(),
        'show-upstream-detail': () => showUpstreamDetailModal(),
        'close-upstream-detail': () => closeUpstreamDetailModal(),
        'close-sort-modal': () => closeSortModal(),
        'save-sort-order': () => saveSortOrder(),
        'toggle-response': (actionTarget) => {
          const responseTarget = actionTarget.dataset.responseTarget;
          if (responseTarget && typeof window.toggleResponse === 'function') {
            window.toggleResponse(responseTarget);
          }
        }
      },
      change: {
        'update-test-url': () => updateTestURL()
      }
    });
  }
}

window.initPageBootstrap({
  topbarKey: 'channels',
  run: async () => {
  initChannelsPageActions();
  setupFilterListeners();
  setupImportExport();
  setupKeyImportPreview();
  setupModelImportPreview();
  if (typeof initChannelFormDirtyTracking === 'function') {
    initChannelFormDirtyTracking();
  }
  if (typeof updateBatchChannelSelectionUI === 'function') {
    updateBatchChannelSelectionUI();
  }

  await window.ChannelTypeManager.renderChannelTypeRadios('channelTypeRadios');

  // 优先从 localStorage 恢复，其次检查 URL 参数，最后默认 all
  const savedFilters = loadChannelsFilters();
  const targetChannelType = await getTargetChannelType();
  const initialType = targetChannelType || (savedFilters?.channelType) || 'all';

  filters.channelType = initialType;
  const urlChannelId = new URLSearchParams(location.search).get('id');
  if (urlChannelId) {
    // 从日志等页面跳转过来时，仅按渠道ID过滤，清除其他条件
    filters.status = 'all';
    filters.model = 'all';
    filters.search = '';
    document.getElementById('statusFilter').value = 'all';
    if (typeof modelFilterCombobox !== 'undefined' && modelFilterCombobox) {
      modelFilterCombobox.setValue('all', modelFilterInputValueFromFilterValue('all'));
    } else {
      const modelFilterEl = document.getElementById('modelFilter');
      if (modelFilterEl) modelFilterEl.value = modelFilterInputValueFromFilterValue('all');
    }
    saveChannelsFilters();
  } else if (savedFilters) {
    filters.status = savedFilters.status || 'all';
    filters.model = savedFilters.model || 'all';
    filters.search = savedFilters.search || '';
    document.getElementById('statusFilter').value = filters.status;
    if (typeof modelFilterCombobox !== 'undefined' && modelFilterCombobox) {
      modelFilterCombobox.setValue(filters.model, modelFilterInputValueFromFilterValue(filters.model));
    } else {
      const modelFilterEl = document.getElementById('modelFilter');
      if (modelFilterEl) modelFilterEl.value = modelFilterInputValueFromFilterValue(filters.model);
    }
    if (typeof channelNameCombobox !== 'undefined' && channelNameCombobox) {
      const allLabel = (typeof getChannelNameAllLabel === 'function')
        ? getChannelNameAllLabel()
        : ((window.t && window.t('channels.channelNameAll')) || '所有渠道');
      channelNameCombobox.setValue(filters.search, filters.search || allLabel);
    } else {
      const searchInputEl = document.getElementById('searchInput');
      if (searchInputEl) {
        const allLabel = (window.t && window.t('channels.channelNameAll')) || '所有渠道';
        searchInputEl.value = filters.search || allLabel;
      }
    }
  }

  // 初始化渠道类型筛选器（替换原Tab逻辑）
  await window.initChannelTypeFilter('channelTypeFilter', initialType, (type) => {
    filters.channelType = type;
    filters.model = 'all';
    filters.search = '';
    if (typeof modelFilterCombobox !== 'undefined' && modelFilterCombobox) {
      modelFilterCombobox.setValue('all', modelFilterInputValueFromFilterValue('all'));
    } else {
      const modelFilterEl = document.getElementById('modelFilter');
      if (modelFilterEl) modelFilterEl.value = modelFilterInputValueFromFilterValue('all');
    }
    if (typeof channelNameCombobox !== 'undefined' && channelNameCombobox) {
      channelNameCombobox.setValue('', getChannelNameAllLabel());
    }
    saveChannelsFilters();
    loadChannels(type);
    if (typeof updateChannelNameOptions === 'function') updateChannelNameOptions();
  });

  await loadDefaultTestContent();
  await loadChannelStatsRange();

  await loadChannels(initialType);
  if (typeof updateChannelNameOptions === 'function') updateChannelNameOptions();
  await loadChannelStats();
  highlightFromHash();
  window.addEventListener('hashchange', highlightFromHash);

  // 监听语言切换事件，重新渲染渠道列表
  window.i18n.onLocaleChange(() => {
    renderChannels();
    updateModelOptions();
  });
  }
});

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
