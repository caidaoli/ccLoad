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

async function getTargetChannel() {
  const params = new URLSearchParams(location.search);
  const channelId = params.get('id');
  if (!channelId) return null;

  try {
    return await fetchDataWithAuth(`/admin/channels/${channelId}`);
  } catch (e) {
    console.error('Failed to get target channel:', e);
    return null;
  }
}

const CHANNELS_FILTER_KEY = 'channels.filters';

function saveChannelsFilters() {
  try {
    localStorage.setItem(CHANNELS_FILTER_KEY, JSON.stringify({
      channelType: filters.channelType,
      status: filters.status,
      model: filters.model,
      modelExact: filters.modelExact,
      search: filters.search,
      searchExact: filters.searchExact,
      page: channelsCurrentPage
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

function resetChannelSearchFilter() {
  filters.search = '';
  filters.searchExact = false;
  channelsCurrentPage = 1;
  if (typeof channelNameCombobox !== 'undefined' && channelNameCombobox) {
    channelNameCombobox.setValue('', getChannelNameAllLabel());
  } else {
    const searchInputEl = document.getElementById('searchInput');
    if (searchInputEl) {
      const allLabel = (window.t && window.t('channels.channelNameAll')) || '所有渠道';
      searchInputEl.value = allLabel;
    }
  }
}

function updateChannelsPagination() {
  const currentPageEl = document.getElementById('channels_current_page');
  const totalPagesEl = document.getElementById('channels_total_pages');
  const firstBtn = document.getElementById('channels_first_page');
  const prevBtn = document.getElementById('channels_prev_page');
  const nextBtn = document.getElementById('channels_next_page');
  const lastBtn = document.getElementById('channels_last_page');

  if (currentPageEl) currentPageEl.textContent = String(channelsCurrentPage);
  if (totalPagesEl) totalPagesEl.textContent = String(channelsTotalPages);

  const disablePrev = channelsCurrentPage <= 1;
  const disableNext = channelsCurrentPage >= channelsTotalPages;
  if (firstBtn) firstBtn.disabled = disablePrev;
  if (prevBtn) prevBtn.disabled = disablePrev;
  if (nextBtn) nextBtn.disabled = disableNext;
  if (lastBtn) lastBtn.disabled = disableNext;
}

function firstChannelsPage() {
  if (channelsCurrentPage <= 1) return;
  channelsCurrentPage = 1;
  saveChannelsFilters();
  loadChannels(filters.channelType);
}

function prevChannelsPage() {
  if (channelsCurrentPage <= 1) return;
  channelsCurrentPage--;
  saveChannelsFilters();
  loadChannels(filters.channelType);
}

function nextChannelsPage() {
  if (channelsCurrentPage >= channelsTotalPages) return;
  channelsCurrentPage++;
  saveChannelsFilters();
  loadChannels(filters.channelType);
}

function lastChannelsPage() {
  if (channelsCurrentPage >= channelsTotalPages) return;
  channelsCurrentPage = channelsTotalPages;
  saveChannelsFilters();
  loadChannels(filters.channelType);
}

function jumpChannelsPage() {
  const input = document.getElementById('channels_jump_page');
  if (!input) return;
  const page = parseInt(input.value, 10);
  if (!Number.isFinite(page) || page < 1 || page > channelsTotalPages) {
    input.value = '';
    return;
  }
  if (page !== channelsCurrentPage) {
    channelsCurrentPage = page;
    saveChannelsFilters();
    loadChannels(filters.channelType);
  }
  input.value = '';
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
        'first-channels-page': () => firstChannelsPage(),
        'prev-channels-page': () => prevChannelsPage(),
        'next-channels-page': () => nextChannelsPage(),
        'last-channels-page': () => lastChannelsPage(),
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

  const jumpPageInput = document.getElementById('channels_jump_page');
  if (jumpPageInput && !jumpPageInput.dataset.bound) {
    jumpPageInput.addEventListener('keydown', (event) => {
      if (event.key === 'Enter') {
        jumpChannelsPage();
      }
    });
    jumpPageInput.dataset.bound = '1';
  }

  // 每页显示数量选择器
  const pageSizeSelect = document.getElementById('channels_page_size');
  if (pageSizeSelect && !pageSizeSelect.dataset.bound) {
    pageSizeSelect.value = String(channelsPageSize);
    pageSizeSelect.addEventListener('change', (event) => {
      const newSize = parseInt(event.target.value, 10);
      if (newSize > 0) {
        channelsPageSize = newSize;
        localStorage.setItem('channels.pageSize', String(newSize));
        channelsCurrentPage = 1;
        saveChannelsFilters();
        loadChannels(filters.channelType);
      }
    });
    pageSizeSelect.dataset.bound = '1';
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

    const savedFilters = loadChannelsFilters();
    channelsCurrentPage = Math.max(1, parseInt(savedFilters?.page, 10) || 1);
    const targetChannel = await getTargetChannel();
    const targetChannelType = targetChannel?.channel_type || null;
    const initialType = targetChannelType || (savedFilters?.channelType) || 'all';

    filters.channelType = initialType;
    const urlChannelId = new URLSearchParams(location.search).get('id');
    if (urlChannelId) {
      filters.status = 'all';
      filters.model = 'all';
      filters.modelExact = false;
      filters.search = targetChannel?.name || '';
      filters.searchExact = Boolean(filters.search);
      channelsCurrentPage = 1;
      document.getElementById('statusFilter').value = 'all';
      if (typeof modelFilterCombobox !== 'undefined' && modelFilterCombobox) {
        modelFilterCombobox.setValue('all', modelFilterInputValueFromFilterValue('all'));
      } else {
        const modelFilterEl = document.getElementById('modelFilter');
        if (modelFilterEl) modelFilterEl.value = modelFilterInputValueFromFilterValue('all');
      }
      const searchInputEl = document.getElementById('searchInput');
      if (searchInputEl) {
        const allLabel = (window.t && window.t('channels.channelNameAll')) || '所有渠道';
        searchInputEl.value = filters.search || allLabel;
      }
    } else if (savedFilters) {
      filters.status = savedFilters.status || 'all';
      filters.model = savedFilters.model || 'all';
      filters.modelExact = filters.model !== 'all' && savedFilters.modelExact !== false;
      filters.search = savedFilters.search || '';
      filters.searchExact = savedFilters.searchExact === true;
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
      saveChannelsFilters();
    }

    await window.initChannelTypeFilter('channelTypeFilter', initialType, (type) => {
      filters.channelType = type;
      filters.model = 'all';
      filters.modelExact = false;
      filters.search = '';
      filters.searchExact = false;
      channelsCurrentPage = 1;
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
      loadChannelsFilterOptions(type, filters.status);
      loadChannels(type);
    });

    await loadDefaultTestContent();
    await loadChannelStatsRange();

    await Promise.all([
      loadChannelsFilterOptions(initialType, filters.status),
      loadChannels(initialType)
    ]);
    await loadChannelStats();
    highlightFromHash();
    window.addEventListener('hashchange', highlightFromHash);

    window.i18n.onLocaleChange(() => {
      renderChannels();
      updateModelOptions();
      updateChannelsPagination();
    });

    // 自动刷新（system_settings.auto_refresh_interval_seconds，0=禁用）
    // 通过 .modal.show 检测跳过编辑/批量/排序等对话框打开期间的刷新，避免丢失未保存内容
    if (typeof window.createAutoRefresh === 'function') {
      window.createAutoRefresh({
        load: () => Promise.all([
          loadChannels(filters.channelType || 'all'),
          loadChannelStats()
        ])
      }).init();
    }
  }
});

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    const customRulesModal = document.getElementById('customRulesModal');
    const modelImportModal = document.getElementById('modelImportModal');
    const keyImportModal = document.getElementById('keyImportModal');
    const keyExportModal = document.getElementById('keyExportModal');
    const sortModal = document.getElementById('sortModal');
    const deleteModal = document.getElementById('deleteModal');
    const testModal = document.getElementById('testModal');
    const channelModal = document.getElementById('channelModal');

    if (customRulesModal && customRulesModal.classList.contains('show')) {
      closeCustomRulesModal();
    } else if (modelImportModal && modelImportModal.classList.contains('show')) {
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

window.addEventListener('pageshow', async (event) => {
  const urlChannelId = new URLSearchParams(location.search).get('id');
  if (!event.persisted || urlChannelId) return;

  resetChannelSearchFilter();
  if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
  await loadChannels(filters.channelType || 'all');
});
