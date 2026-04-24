// Filter channels based on current filters
let filteredChannels = []; // 存储筛选后的渠道列表
let modelFilterOptions = [];
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

function normalizeModelFilterOption() {
  if (!filters || !filters.model || filters.model === 'all') return false;
  if (modelFilterOptions.includes(filters.model)) return false;

  filters.model = 'all';
  if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
  return true;
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

// Update model filter options
function updateModelOptions() {
  const modelSet = new Set();
  const typeFilter = (filters && filters.channelType) ? filters.channelType : 'all';
  channels.forEach(channel => {
    if (typeFilter !== 'all') {
      const channelType = channel.channel_type || 'anthropic';
      if (channelType !== typeFilter) return;
    }
    if (Array.isArray(channel.models)) {
      // 新格式：models 是 {model, redirect_model} 对象数组
      channel.models.forEach(m => {
        const modelName = m.model || m;
        if (modelName) modelSet.add(modelName);
      });
    }
  });

  modelFilterOptions = Array.from(modelSet).sort();

  normalizeModelFilterOption();

  // 使用通用组件刷新下拉框
  if (modelFilterCombobox) {
    modelFilterCombobox.setValue(filters.model, modelFilterInputValueFromFilterValue(filters.model));
    modelFilterCombobox.refresh();
  } else {
    const modelFilterInput = document.getElementById('modelFilter');
    if (modelFilterInput) {
      modelFilterInput.value = modelFilterInputValueFromFilterValue(filters.model);
    }
  }
}

// 更新渠道名称下拉选项（getOptions 回调动态读取，refresh 触发重算）
function updateChannelNameOptions() {
  if (!channelNameCombobox) return;

  // 检查当前选值是否仍合法
  const currentVal = channelNameCombobox.getValue();
  if (currentVal) {
    const typeFilter = (filters && filters.channelType) ? filters.channelType : 'all';
    const stillExists = channels.some(ch => {
      if (typeFilter !== 'all' && (ch.channel_type || 'anthropic') !== typeFilter) return false;
      return ch.name === currentVal;
    });
    if (!stillExists) {
      channelNameCombobox.setValue('', getChannelNameAllLabel());
      filters.search = '';
      if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
    }
  }

  channelNameCombobox.refresh();
}

// Setup filter event listeners
function setupFilterListeners() {
  document.getElementById('statusFilter').addEventListener('change', (e) => {
    filters.status = e.target.value;
    channelsCurrentPage = 1;
    if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
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
      getOptions: () => {
        const allLabel = getModelAllLabel();
        return [{ value: 'all', label: allLabel }].concat(
          modelFilterOptions.map(m => ({ value: m, label: m }))
        );
      },
      onSelect: (value) => {
        filters.model = value;
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
      getOptions: () => {
        const nameSet = new Set();
        const typeFilter = (filters && filters.channelType) ? filters.channelType : 'all';
        channels.forEach(ch => {
          if (typeFilter !== 'all' && (ch.channel_type || 'anthropic') !== typeFilter) return;
          if (ch.name) nameSet.add(ch.name);
        });
        return [{ value: '', label: allLabel }].concat(
          Array.from(nameSet).sort().map(name => ({ value: name, label: name }))
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
      filters.search = '';
      channelsCurrentPage = 1;
      if (channelNameCombobox) {
        channelNameCombobox.setValue('', getChannelNameAllLabel());
      } else {
        const searchInputEl = document.getElementById('searchInput');
        if (searchInputEl) searchInputEl.value = getChannelNameAllLabel();
      }
      if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
      loadChannels(filters.channelType);
    });
  }
}
