// Filter channels based on current filters
let filteredChannels = []; // 存储筛选后的渠道列表
let channelAuthTypeFilterCombobox = null; // 认证类型筛选组合框实例
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

function channelListPriority(channel) {
  return Number(channel.effective_priority ?? channel.priority) || 0;
}

function compareChannelText(a, b) {
  const left = String(a ?? '');
  const right = String(b ?? '');
  return left < right ? -1 : left > right ? 1 : 0;
}

// 与服务端 sortChannelList 同一规则：先按排序键，平局依次按优先级降序、名称升序、ID 升序。
// 本地改动（如行内改优先级）后重排当前页，结果与重新请求一致。
function compareChannelsForList(a, b, sort = channelsSort) {
  let result;
  if (sort.key === 'name') {
    result = compareChannelText(a.name, b.name);
  } else if (sort.key === 'enabled') {
    result = Number(Boolean(a.enabled)) - Number(Boolean(b.enabled));
  } else {
    result = channelListPriority(a) - channelListPriority(b);
  }
  if (result !== 0) return sort.order === 'desc' ? -result : result;
  const priorityDiff = channelListPriority(b) - channelListPriority(a);
  if (priorityDiff !== 0) return priorityDiff;
  return compareChannelText(a.name, b.name) || (Number(a.id) - Number(b.id));
}

function toggleChannelsSort(key) {
  const order = channelsSort.key === key
    ? (channelsSort.order === 'asc' ? 'desc' : 'asc')
    : defaultChannelSortOrder(key);
  channelsSort = normalizeChannelsSort({ key, order });
  channelsCurrentPage = 1;
  if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
  loadChannels();
}

function filterChannels() {
  const filtered = channels.slice().sort((a, b) => compareChannelsForList(a, b));

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

function updateChannelAuthTypeOptions() {
  if (!channelAuthTypeFilterCombobox) return;
  channelAuthTypeFilterCombobox.setValue(filters.authType, channelAuthTypeFilterLabel(filters.authType));
  channelAuthTypeFilterCombobox.refresh();
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
    loadChannels();
  });

  const authTypeFilterInput = document.getElementById('channelAuthTypeFilter');
  if (authTypeFilterInput) {
    channelAuthTypeFilterCombobox = createSearchableCombobox({
      attachMode: true,
      inputId: 'channelAuthTypeFilter',
      dropdownId: 'channelAuthTypeFilterDropdown',
      initialValue: filters.authType,
      initialLabel: channelAuthTypeFilterLabel(filters.authType),
      allowCustomInput: false,
      commitEmptyAsFirst: true,
      showAllOptionsOnOpen: true,
      getOptions: getChannelAuthTypeOptions,
      onSelect: (value) => {
        const validValues = new Set(['all', 'codebuddy_oauth', 'api_key', 'codex_oauth', 'antigravity_oauth', 'xai_oauth', 'anthropic_oauth', 'zai_oauth', 'cursor_oauth', 'zed_oauth']);
        filters.authType = validValues.has(value) ? value : 'all';
        channelsCurrentPage = 1;
        if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
        loadChannels();
      }
    });
  }

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
      showAllOptionsOnOpen: true,
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
        loadChannels();
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
      showAllOptionsOnOpen: true,
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
        loadChannels();
      }
    });
  }

  const clearSearchBtn = document.getElementById('clearSearchBtn');
  if (clearSearchBtn) {
    clearSearchBtn.addEventListener('click', () => {
      // 重置所有筛选条件
      filters.search = '';
      filters.searchExact = false;
      filters.status = 'all';
      filters.authType = 'all';
      filters.model = 'all';
      filters.modelExact = false;
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

      if (channelAuthTypeFilterCombobox) {
        channelAuthTypeFilterCombobox.setValue('all', channelAuthTypeFilterLabel('all'));
      } else {
        const authTypeFilterEl = document.getElementById('channelAuthTypeFilter');
        if (authTypeFilterEl) authTypeFilterEl.value = channelAuthTypeFilterLabel('all');
      }

      // 重置状态下拉框
      const statusFilterEl = document.getElementById('statusFilter');
      if (statusFilterEl) statusFilterEl.value = 'all';

      if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
      loadChannels();
    });
  }
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = { setupFilterListeners, compareChannelsForList };
}
