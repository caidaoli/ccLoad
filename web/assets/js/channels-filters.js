// Filter channels based on current filters
let filteredChannels = []; // 存储筛选后的渠道列表
let modelFilterCombobox = null; // 通用组件实例
let channelNameCombobox = null; // 渠道名筛选组合框实例

function getModelAllLabel() {
  return (window.t && window.t('channels.modelAll')) || '所有模型';
}

function getChannelNameAllLabel() {
  return (window.t && window.t('channels.channelNameAll')) || '所有渠道';
}

function modelFilterInputValueFromFilterValue(filterValue) {
  if (!filterValue || filterValue === 'all') return getModelAllLabel();
  return filterValue;
}

function normalizeChannelFilterValue(value) {
  return String(value || '').trim().toLowerCase();
}

function isExactChannelFilterValue(value, options) {
  const normalizedValue = normalizeChannelFilterValue(value);
  if (!normalizedValue) return false;
  return (Array.isArray(options) ? options : []).some((option) =>
    normalizeChannelFilterValue(option) === normalizedValue
  );
}

function isExactChannelModelFilter(value) {
  if (!value || value === 'all') return false;
  return isExactChannelFilterValue(value, allAvailableModels);
}

function isExactChannelNameFilter(value) {
  return isExactChannelFilterValue(value, allAvailableChannelNames);
}

function filterChannels() {
  const filtered = channels.slice();

  // 排序：优先使用 effective_priority（健康度模式），否则使用 priority
  filtered.sort((a, b) => {
    const prioA = a.effective_priority ?? a.priority;
    const prioB = b.effective_priority ?? b.priority;
    if (prioB !== prioA) {
      return prioB - prioA;
    }
    const typeA = (a.channel_type || 'anthropic').toLowerCase();
    const typeB = (b.channel_type || 'anthropic').toLowerCase();
    if (typeA !== typeB) {
      return typeA.localeCompare(typeB);
    }
    return a.name.localeCompare(b.name);
  });

  filteredChannels = filtered; // 当前页筛选结果（服务端已过滤）
  renderChannels(filtered);
  updateFilterInfo(filtered.length, channelsTotalCount);
}

// Update filter info display
function updateFilterInfo(filtered, total) {
  document.getElementById('filteredCount').textContent = filtered;
  document.getElementById('totalCount').textContent = total;
}

// 刷新模型筛选下拉显示（选项由 getOptions 从 allAvailableModels 动态读取）
function updateModelOptions() {
  if (modelFilterCombobox) {
    modelFilterCombobox.setValue(filters.model, modelFilterInputValueFromFilterValue(filters.model));
    modelFilterCombobox.refresh();
    return;
  }
  const modelFilterInput = document.getElementById('modelFilter');
  if (modelFilterInput) {
    modelFilterInput.value = modelFilterInputValueFromFilterValue(filters.model);
  }
}

// 刷新渠道名称下拉显示（选项由 getOptions 从 allAvailableChannelNames 动态读取）
function updateChannelNameOptions() {
  if (channelNameCombobox) channelNameCombobox.refresh();
}

// Setup filter event listeners
function setupFilterListeners() {
  document.getElementById('statusFilter').addEventListener('change', (e) => {
    filters.status = e.target.value;
    channelsCurrentPage = 1;
    if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
    if (typeof loadChannelsFilterOptions === 'function') {
      loadChannelsFilterOptions(filters.channelType, filters.status);
    }
    loadChannels(filters.channelType);
  });

  // 模型筛选 combobox
  const modelFilterInput = document.getElementById('modelFilter');
  if (modelFilterInput) {
    modelFilterCombobox = createSearchableCombobox({
      attachMode: true,
      inputId: 'modelFilter',
      dropdownId: 'modelFilterDropdown',
      initialValue: filters.model,
      initialLabel: modelFilterInputValueFromFilterValue(filters.model),
      allowCustomInput: true,
      commitEmptyAsFirst: true,
      getOptions: () => {
        const allLabel = getModelAllLabel();
        const models = Array.isArray(allAvailableModels) ? allAvailableModels : [];
        return [{ value: 'all', label: allLabel }].concat(
          models.map(m => ({ value: m, label: m }))
        );
      },
      onSelect: (value) => {
        const raw = String(value || '').trim();
        filters.model = raw || 'all';
        filters.modelExact = isExactChannelModelFilter(value);
        channelsCurrentPage = 1;
        if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
        loadChannels(filters.channelType);
      }
    });
  }

  // 渠道名称筛选 combobox
  const searchInput = document.getElementById('searchInput');
  if (searchInput) {
    const allLabel = getChannelNameAllLabel();
    channelNameCombobox = createSearchableCombobox({
      attachMode: true,
      inputId: 'searchInput',
      dropdownId: 'searchInputDropdown',
      initialValue: filters.search,
      initialLabel: filters.search || allLabel,
      allowCustomInput: true,
      commitEmptyAsFirst: true,
      getOptions: () => {
        // 使用服务端在 search 过滤前冻结的全集，避免选中某渠道名后下拉收敛为单一项
        const names = Array.isArray(allAvailableChannelNames) ? allAvailableChannelNames : [];
        return [{ value: '', label: allLabel }].concat(
          names.map(name => ({ value: name, label: name }))
        );
      },
      onSelect: (value) => {
        const raw = String(value || '').trim();
        const allLabel = String(getChannelNameAllLabel() || '').trim().toLowerCase();
        const normalized = raw.toLowerCase();
        const isAllToken = !raw ||
          normalized === allLabel ||
          normalized === '所有渠道' ||
          normalized === 'all channels';

        filters.search = isAllToken ? '' : raw;
        filters.searchExact = !isAllToken && isExactChannelNameFilter(raw);
        if (isAllToken && channelNameCombobox) {
          channelNameCombobox.setValue('', getChannelNameAllLabel());
        }
        channelsCurrentPage = 1;
        if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
        loadChannels(filters.channelType);
      }
    });
  }

  // 筛选按钮：手动触发筛选
  document.getElementById('btn_filter').addEventListener('click', () => {
    channelsCurrentPage = 1;
    if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
    loadChannels(filters.channelType);
  });

  const clearSearchBtn = document.getElementById('clearSearchBtn');
  if (clearSearchBtn) {
    clearSearchBtn.addEventListener('click', () => {
      // 重置所有筛选条件
      filters.search = '';
      filters.searchExact = false;
      filters.status = 'all';
      filters.model = 'all';
      filters.modelExact = false;
      filters.channelType = 'all';
      channelsCurrentPage = 1;

      // 重置渠道名称 combobox
      if (channelNameCombobox) {
        channelNameCombobox.setValue('', getChannelNameAllLabel());
      } else {
        const searchInputEl = document.getElementById('searchInput');
        if (searchInputEl) searchInputEl.value = getChannelNameAllLabel();
      }

      // 重置模型 combobox
      if (modelFilterCombobox) {
        modelFilterCombobox.setValue('all', getModelAllLabel());
      } else {
        const modelFilterEl = document.getElementById('modelFilter');
        if (modelFilterEl) modelFilterEl.value = getModelAllLabel();
      }

      // 重置状态下拉框
      const statusFilterEl = document.getElementById('statusFilter');
      if (statusFilterEl) statusFilterEl.value = 'all';

      // 重置渠道类型下拉框
      const channelTypeFilterEl = document.getElementById('channelTypeFilter');
      if (channelTypeFilterEl) channelTypeFilterEl.value = 'all';

      if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
      loadChannels(filters.channelType);
    });
  }
}
