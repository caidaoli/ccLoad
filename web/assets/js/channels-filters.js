// Filter channels based on current filters
let filteredChannels = []; // 存储筛选后的渠道列表

function filterChannels() {
  const filtered = channels.filter(channel => {
    if (filters.search && !channel.name.toLowerCase().includes(filters.search.toLowerCase())) {
      return false;
    }

    if (filters.id) {
      const idStr = filters.id.trim();
      if (idStr) {
        const ids = idStr.split(',').map(id => id.trim()).filter(id => id);
        if (ids.length > 0 && !ids.includes(String(channel.id))) {
          return false;
        }
      }
    }

    if (filters.channelType !== 'all') {
      const channelType = channel.channel_type || 'anthropic';
      if (channelType !== filters.channelType) {
        return false;
      }
    }

    if (filters.status !== 'all') {
      if (filters.status === 'enabled' && !channel.enabled) return false;
      if (filters.status === 'disabled' && channel.enabled) return false;
      if (filters.status === 'cooldown' && !(channel.cooldown_remaining_ms > 0)) return false;
    }

    if (filters.model !== 'all') {
      // 新格式：models 是 {model, redirect_model} 对象数组
      const modelNames = Array.isArray(channel.models)
        ? channel.models.map(m => m.model || m)
        : [];
      if (!modelNames.includes(filters.model)) {
        return false;
      }
    }

    return true;
  });

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

  filteredChannels = filtered; // 保存筛选后的列表供其他模块使用
  renderChannels(filtered);
  updateFilterInfo(filtered.length, channels.length);
}

// Update filter info display
function updateFilterInfo(filtered, total) {
  document.getElementById('filteredCount').textContent = filtered;
  document.getElementById('totalCount').textContent = total;
}

// Update model filter options
function updateModelOptions() {
  const modelSet = new Set();
  channels.forEach(channel => {
    if (Array.isArray(channel.models)) {
      // 新格式：models 是 {model, redirect_model} 对象数组
      channel.models.forEach(m => {
        const modelName = m.model || m;
        if (modelName) modelSet.add(modelName);
      });
    }
  });

  const modelFilter = document.getElementById('modelFilter');
  modelFilter.innerHTML = '<option value="all">所有模型</option>';

  Array.from(modelSet).sort().forEach(model => {
    const option = document.createElement('option');
    option.value = model;
    option.textContent = model;
    modelFilter.appendChild(option);
  });

  // 从 filters.model 恢复选中值（而非 DOM 当前值，因为初始化时选项可能还未加载）
  if (filters.model && filters.model !== 'all') {
    modelFilter.value = filters.model;
  }
}

// Setup filter event listeners
function setupFilterListeners() {
  const searchInput = document.getElementById('searchInput');
  const clearSearchBtn = document.getElementById('clearSearchBtn');

  const debouncedFilter = debounce(() => {
    filters.search = searchInput.value;
    if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
    filterChannels();
    updateClearButton();
  }, 300);

  searchInput.addEventListener('input', debouncedFilter);

  clearSearchBtn.addEventListener('click', () => {
    searchInput.value = '';
    filters.search = '';
    if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
    filterChannels();
    updateClearButton();
    searchInput.focus();
  });

  function updateClearButton() {
    clearSearchBtn.style.opacity = searchInput.value ? '1' : '0';
  }

  const idFilter = document.getElementById('idFilter');
  const debouncedIdFilter = debounce(() => {
    filters.id = idFilter.value;
    if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
    filterChannels();
  }, 300);
  idFilter.addEventListener('input', debouncedIdFilter);

  document.getElementById('statusFilter').addEventListener('change', (e) => {
    filters.status = e.target.value;
    if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
    filterChannels();
  });

  document.getElementById('modelFilter').addEventListener('change', (e) => {
    filters.model = e.target.value;
    if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
    filterChannels();
  });

  // 筛选按钮：手动触发筛选
  document.getElementById('btn_filter').addEventListener('click', () => {
    // 收集当前输入框的值
    filters.search = document.getElementById('searchInput').value;
    filters.id = document.getElementById('idFilter').value;

    // 保存筛选条件
    if (typeof saveChannelsFilters === 'function') saveChannelsFilters();

    // 执行筛选
    filterChannels();
  });

  // 回车键触发筛选
  ['searchInput', 'idFilter'].forEach(id => {
    const el = document.getElementById(id);
    if (el) {
      el.addEventListener('keydown', e => {
        if (e.key === 'Enter') {
          filters.search = document.getElementById('searchInput').value;
          filters.id = document.getElementById('idFilter').value;
          if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
          filterChannels();
        }
      });
    }
  });
}
