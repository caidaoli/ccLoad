// Filter channels based on current filters
let filteredChannels = []; // 存储筛选后的渠道列表
let modelFilterOptions = [];
let modelFilterActiveIndex = -1;
let modelFilterOutsideHandler = null;

function getModelAllLabel() {
  return (window.t && window.t('channels.modelAll')) || '所有模型';
}

function modelFilterValueFromInputValue(inputValue) {
  const labelAll = getModelAllLabel();
  const value = (inputValue || '').trim();
  if (!value || value === labelAll) return 'all';
  return value;
}

function modelFilterInputValueFromFilterValue(filterValue) {
  if (!filterValue || filterValue === 'all') return getModelAllLabel();
  return filterValue;
}

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

  const modelFilterInput = document.getElementById('modelFilter');
  if (modelFilterInput && modelFilterInput.dataset.pickActive !== '1') {
    modelFilterInput.value = modelFilterInputValueFromFilterValue(filters.model);
  }

  if (typeof window.__renderModelFilterDropdown === 'function') {
    window.__renderModelFilterDropdown();
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

  let syncModelFilterBeforeApply = () => {
    const modelFilterEl = document.getElementById('modelFilter');
    if (!modelFilterEl) return;
    filters.model = modelFilterValueFromInputValue(modelFilterEl.value);
    if (filters.model === 'all') {
      modelFilterEl.value = modelFilterInputValueFromFilterValue('all');
    }
  };

  const modelFilterInput = document.getElementById('modelFilter');
  if (modelFilterInput) {
    const modelFilterDropdown = document.getElementById('modelFilterDropdown');
    const modelFilterWrapper = modelFilterInput.closest('.filter-combobox-wrapper');
    const modelFilterDropdownHome = modelFilterDropdown ? modelFilterDropdown.parentElement : null;
    let modelFilterRepositionHandler = null;

    function clearOutsideHandler() {
      if (!modelFilterOutsideHandler) return;
      document.removeEventListener('mousedown', modelFilterOutsideHandler, true);
      modelFilterOutsideHandler = null;
    }

    function clearRepositionHandler() {
      if (!modelFilterRepositionHandler) return;
      window.removeEventListener('resize', modelFilterRepositionHandler, true);
      window.removeEventListener('scroll', modelFilterRepositionHandler, true);
      modelFilterRepositionHandler = null;
    }

    function closeModelDropdown() {
      if (!modelFilterDropdown) return;
      modelFilterDropdown.style.display = 'none';
      modelFilterDropdown.dataset.open = '0';
      modelFilterActiveIndex = -1;
      clearOutsideHandler();
      clearRepositionHandler();

      if (modelFilterDropdownHome && modelFilterDropdown.parentElement !== modelFilterDropdownHome) {
        modelFilterDropdownHome.appendChild(modelFilterDropdown);
      }
    }

    function beginModelPick() {
      if (!modelFilterDropdown || !modelFilterWrapper) return;
      if (modelFilterInput.dataset.pickActive === '1') return;
      modelFilterInput.dataset.pickActive = '1';
      modelFilterInput.dataset.prevInputValue = modelFilterInput.value;
      modelFilterInput.dataset.prevFiltersModel = filters.model;
      modelFilterInput.value = '';
      modelFilterActiveIndex = -1;
    }

    function cancelModelPick() {
      if (modelFilterInput.dataset.pickActive !== '1') {
        closeModelDropdown();
        return;
      }

      const prevInputValue = modelFilterInput.dataset.prevInputValue ?? '';
      const prevFiltersModel = modelFilterInput.dataset.prevFiltersModel ?? 'all';

      modelFilterInput.value = prevInputValue;
      filters.model = prevFiltersModel;
      if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
      filterChannels();

      delete modelFilterInput.dataset.pickActive;
      delete modelFilterInput.dataset.prevInputValue;
      delete modelFilterInput.dataset.prevFiltersModel;

      closeModelDropdown();
    }

    function commitModelFilterValue(value) {
      filters.model = value;
      modelFilterInput.value = modelFilterInputValueFromFilterValue(value);
      if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
      filterChannels();

      delete modelFilterInput.dataset.pickActive;
      delete modelFilterInput.dataset.prevInputValue;
      delete modelFilterInput.dataset.prevFiltersModel;

      closeModelDropdown();
    }

    function commitModelFilterFromInput() {
      const typed = modelFilterInput.value.trim();
      if (modelFilterInput.dataset.pickActive === '1' && !typed) {
        cancelModelPick();
        return;
      }
      commitModelFilterValue(modelFilterValueFromInputValue(typed));
    }

    function getDropdownItems() {
      const keyword = modelFilterInput.value.trim().toLowerCase();
      const allLabel = getModelAllLabel();
      const models = keyword
        ? modelFilterOptions.filter(m => String(m).toLowerCase().includes(keyword))
        : modelFilterOptions;

      return [{ value: 'all', label: allLabel }].concat(models.map(m => ({ value: m, label: m })));
    }

    function renderModelDropdown() {
      if (!modelFilterDropdown || modelFilterDropdown.dataset.open !== '1') return;

      const items = getDropdownItems();
      modelFilterDropdown.innerHTML = '';

      if (modelFilterActiveIndex >= items.length) modelFilterActiveIndex = items.length - 1;
      if (modelFilterActiveIndex < -1) modelFilterActiveIndex = -1;

      items.forEach((item, idx) => {
        const row = document.createElement('div');
        row.className = 'filter-dropdown-item';
        row.setAttribute('role', 'option');
        row.dataset.value = item.value;
        row.dataset.index = String(idx);
        row.textContent = item.label;

        if (item.value === filters.model) row.classList.add('selected');
        if (idx === modelFilterActiveIndex) row.classList.add('active');

        row.addEventListener('mousedown', (e) => {
          e.preventDefault();
          e.stopPropagation();
          commitModelFilterValue(item.value);
        });

        modelFilterDropdown.appendChild(row);
      });
    }

    window.__renderModelFilterDropdown = renderModelDropdown;

    function positionModelDropdown() {
      if (!modelFilterDropdown || modelFilterDropdown.dataset.open !== '1') return;
      const rect = modelFilterInput.getBoundingClientRect();
      const margin = 6;

      modelFilterDropdown.style.left = `${Math.round(rect.left)}px`;
      modelFilterDropdown.style.width = `${Math.round(rect.width)}px`;

      // Prefer below; flip above if not enough space.
      modelFilterDropdown.style.top = `${Math.round(rect.bottom + margin)}px`;
      const dropdownHeight = modelFilterDropdown.offsetHeight || 0;
      const viewportBottom = window.innerHeight || 0;
      if (dropdownHeight && rect.bottom + margin + dropdownHeight > viewportBottom && rect.top - margin - dropdownHeight >= 0) {
        modelFilterDropdown.style.top = `${Math.round(rect.top - margin - dropdownHeight)}px`;
      }
    }

    function openModelDropdown() {
      if (!modelFilterDropdown || !modelFilterWrapper) return;
      if (modelFilterDropdownHome && modelFilterDropdown.parentElement !== document.body) {
        document.body.appendChild(modelFilterDropdown);
      }
      modelFilterDropdown.style.display = 'block';
      modelFilterDropdown.dataset.open = '1';
      renderModelDropdown();
      positionModelDropdown();

      clearOutsideHandler();
      modelFilterOutsideHandler = (e) => {
        const target = e.target;
        if (!modelFilterWrapper.contains(target) && !modelFilterDropdown.contains(target)) {
          cancelModelPick();
        }
      };
      document.addEventListener('mousedown', modelFilterOutsideHandler, true);

      clearRepositionHandler();
      modelFilterRepositionHandler = () => positionModelDropdown();
      window.addEventListener('resize', modelFilterRepositionHandler, true);
      window.addEventListener('scroll', modelFilterRepositionHandler, true);
    }

    function moveActive(delta) {
      const items = getDropdownItems();
      if (items.length <= 0) return;
      if (modelFilterActiveIndex === -1) {
        modelFilterActiveIndex = 0;
      } else {
        modelFilterActiveIndex = Math.max(0, Math.min(items.length - 1, modelFilterActiveIndex + delta));
      }
      renderModelDropdown();
    }

    syncModelFilterBeforeApply = () => {
      if (modelFilterDropdown && modelFilterDropdown.dataset.open === '1') {
        if (modelFilterInput.dataset.pickActive === '1' && !modelFilterInput.value.trim()) {
          cancelModelPick();
          return;
        }
      }
      filters.model = modelFilterValueFromInputValue(modelFilterInput.value);
      modelFilterInput.value = modelFilterInputValueFromFilterValue(filters.model);
    };

    modelFilterInput.addEventListener('mousedown', () => {
      beginModelPick();
      openModelDropdown();
    });

    modelFilterInput.addEventListener('input', () => {
      if (modelFilterDropdown && modelFilterDropdown.dataset.open === '1') {
        modelFilterActiveIndex = -1;
        renderModelDropdown();
      }
    });

    modelFilterInput.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') {
        if (modelFilterDropdown && modelFilterDropdown.dataset.open === '1') {
          e.preventDefault();
          cancelModelPick();
        }
        return;
      }

      if (e.key === 'ArrowDown') {
        e.preventDefault();
        if (modelFilterDropdown && modelFilterDropdown.dataset.open !== '1') {
          beginModelPick();
          openModelDropdown();
          return;
        }
        moveActive(1);
        return;
      }

      if (e.key === 'ArrowUp') {
        e.preventDefault();
        if (modelFilterDropdown && modelFilterDropdown.dataset.open !== '1') {
          beginModelPick();
          openModelDropdown();
          return;
        }
        moveActive(-1);
        return;
      }

      if (e.key === 'Enter') {
        e.preventDefault();
        if (modelFilterDropdown && modelFilterDropdown.dataset.open === '1') {
          const items = getDropdownItems();
          if (modelFilterActiveIndex >= 0 && modelFilterActiveIndex < items.length) {
            commitModelFilterValue(items[modelFilterActiveIndex].value);
            return;
          }
        }
        commitModelFilterFromInput();
      }
    });

    modelFilterInput.addEventListener('blur', () => {
      if (modelFilterDropdown && modelFilterDropdown.dataset.open === '1') {
        cancelModelPick();
      }
    });
  }

  // 筛选按钮：手动触发筛选
  document.getElementById('btn_filter').addEventListener('click', () => {
    // 收集当前输入框的值
    filters.search = document.getElementById('searchInput').value;
    filters.id = document.getElementById('idFilter').value;
    syncModelFilterBeforeApply();

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
          syncModelFilterBeforeApply();
          if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
          filterChannels();
        }
      });
    }
  });
}
