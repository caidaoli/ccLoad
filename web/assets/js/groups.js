(function () {
  const state = {
    groups: [],
    modelOptions: [],
    editingGroup: null,
    selectedItems: [],
    modelSearch: '',
    draggingIndex: null
  };

  function i18nText(key, fallback, params) {
    if (typeof window.t === 'function') {
      return window.t(key, params);
    }
    return fallback || key;
  }

  function escapeText(value) {
    if (typeof window.escapeHtml === 'function') {
      return window.escapeHtml(value);
    }
    return String(value || '');
  }

  function normalizeText(value) {
    return String(value || '').trim();
  }

  function normalizeMode(mode) {
    const numeric = Number(mode);
    return Number.isFinite(numeric) && numeric >= 1 && numeric <= 4 ? numeric : 3;
  }

  function normalizeSeconds(value) {
    const numeric = Number(value);
    if (!Number.isFinite(numeric) || numeric <= 0) {
      return 0;
    }
    return Math.max(0, Math.floor(numeric));
  }

  function modeLabel(mode) {
    const labels = {
      1: i18nText('groups.mode.roundRobin', '轮询'),
      2: i18nText('groups.mode.random', '随机'),
      3: i18nText('groups.mode.failover', '故障转移'),
      4: i18nText('groups.mode.weighted', '加权')
    };
    return labels[normalizeMode(mode)] || labels[3];
  }

  function modelOptionKey(channelId, modelName) {
    return `${Number(channelId) || 0}::${String(modelName || '')}`;
  }

  function groupItemKey(item) {
    return modelOptionKey(item && item.channel_id, item && item.model_name);
  }

  function parseModelOptionKey(value) {
    const parts = String(value || '').split('::');
    if (parts.length < 2) {
      return { channel_id: 0, model_name: '' };
    }
    return {
      channel_id: Number(parts.shift()) || 0,
      model_name: parts.join('::')
    };
  }

  function matchesGroupName(modelName, groupName) {
    const modelKey = normalizeText(modelName).toLowerCase();
    const groupKey = normalizeText(groupName).toLowerCase();
    return Boolean(modelKey && groupKey && modelKey.includes(groupKey));
  }

  function compileMatchRegex(pattern) {
    const source = normalizeText(pattern);
    if (!source) {
      return null;
    }

    const inlineMatch = source.match(/^\(\?([ims]+)\)([\s\S]+)$/);
    if (inlineMatch) {
      const flags = Array.from(new Set(inlineMatch[1].split('').filter((flag) => /[ims]/.test(flag)))).join('');
      return new RegExp(inlineMatch[2], flags);
    }

    return new RegExp(source);
  }

  function cloneGroupItem(item) {
    return {
      id: Number(item && item.id) || 0,
      channel_id: Number(item && item.channel_id) || 0,
      channel_name: normalizeText(item && item.channel_name),
      model_name: normalizeText(item && item.model_name),
      priority: Number(item && item.priority) || 0,
      weight: Math.max(1, Number(item && item.weight) || 1)
    };
  }

  function sortGroupItems(items) {
    return items.slice().sort((left, right) => {
      const priorityDiff = (Number(left.priority) || 0) - (Number(right.priority) || 0);
      if (priorityDiff !== 0) {
        return priorityDiff;
      }
      return groupItemKey(left).localeCompare(groupItemKey(right));
    });
  }

  function normalizeModelOption(option) {
    return {
      channel_id: Number(option && option.channel_id) || 0,
      channel_name: normalizeText(option && option.channel_name),
      model_name: normalizeText(option && option.model_name)
    };
  }

  function findModelOption(channelId, modelName) {
    const optionKey = modelOptionKey(channelId, modelName);
    return state.modelOptions.find((option) => modelOptionKey(option.channel_id, option.model_name) === optionKey) || null;
  }

  function resolveChannelName(channelId, modelName, fallbackName) {
    const option = findModelOption(channelId, modelName);
    if (option && option.channel_name) {
      return option.channel_name;
    }
    const fallback = normalizeText(fallbackName);
    if (fallback) {
      return fallback;
    }
    return `#${Number(channelId) || 0}`;
  }

  function normalizeGroup(group) {
    const items = sortGroupItems(Array.isArray(group && group.items) ? group.items : []).map((item, index) => {
      const draft = cloneGroupItem(item);
      draft.channel_name = resolveChannelName(draft.channel_id, draft.model_name, draft.channel_name);
      draft.priority = index + 1;
      return draft;
    });

    return {
      id: Number(group && group.id) || 0,
      name: normalizeText(group && group.name),
      mode: normalizeMode(group && group.mode),
      match_regex: normalizeText(group && group.match_regex),
      first_token_time_out: normalizeSeconds(group && group.first_token_time_out),
      session_keep_time: normalizeSeconds(group && group.session_keep_time),
      items,
      created_at: group && group.created_at,
      updated_at: group && group.updated_at
    };
  }

  function getFormElements() {
    return {
      modal: document.getElementById('groupModal'),
      title: document.getElementById('groupModalTitle'),
      form: document.getElementById('groupForm'),
      nameInput: document.getElementById('groupName'),
      modeInput: document.getElementById('groupMode'),
      modeButtons: document.getElementById('groupModeButtons'),
      matchRegexInput: document.getElementById('groupMatchRegex'),
      matchRegexHelp: document.querySelector('[data-i18n="groups.hint.matchRegex"]'),
      firstTokenTimeoutInput: document.getElementById('groupFirstTokenTimeout'),
      sessionKeepTimeInput: document.getElementById('groupSessionKeepTime'),
      searchInput: document.getElementById('groupModelSearch'),
      autoAddButton: document.getElementById('groupAutoAddBtn'),
      pickerList: document.getElementById('groupPickerList'),
      clearButton: document.getElementById('groupClearBtn'),
      selectedList: document.getElementById('groupSelectedList'),
      selectedSummary: document.getElementById('groupSelectedSummary'),
      selectedCount: document.getElementById('groupSelectedCount')
    };
  }

  function syncSelectedPriorities() {
    state.selectedItems = state.selectedItems.map((item, index) => ({
      ...cloneGroupItem(item),
      priority: index + 1
    }));
  }

  function renderModeButtons() {
    const { modeButtons, modeInput } = getFormElements();
    if (!modeButtons) {
      return;
    }

    const activeMode = String(normalizeMode(modeInput && modeInput.value));
    modeButtons.querySelectorAll('[data-action="select-group-mode"]').forEach((button) => {
      const isActive = button.dataset.mode === activeMode;
      button.classList.toggle('is-active', isActive);
      button.setAttribute('aria-pressed', isActive ? 'true' : 'false');
    });
  }

  function isWeightedMode() {
    const { modeInput } = getFormElements();
    return normalizeMode(modeInput && modeInput.value) === 4;
  }

  function selectGroupMode(modeValue) {
    const { modeInput } = getFormElements();
    if (modeInput) {
      modeInput.value = String(normalizeMode(modeValue));
    }
    renderModeButtons();
    renderSelectedSummary();
    renderSelectedList();
  }

  function updateMatchRegexHint(errorMessage) {
    const { matchRegexHelp } = getFormElements();
    if (!matchRegexHelp) {
      return;
    }

    if (errorMessage) {
      matchRegexHelp.textContent = i18nText('groups.messages.invalidRegex', '正则无效：{error}', { error: errorMessage });
      matchRegexHelp.style.color = 'var(--error-500)';
      return;
    }

    matchRegexHelp.textContent = i18nText('groups.hint.matchRegex', '填了正则就按正则自动匹配，不填就按模型名模糊匹配。');
    matchRegexHelp.style.color = '';
  }

  function validateMatchRegexInput() {
    const { matchRegexInput } = getFormElements();
    const value = normalizeText(matchRegexInput && matchRegexInput.value);
    if (!value) {
      updateMatchRegexHint('');
      return { ok: true, regex: null, value: '' };
    }

    try {
      const regex = compileMatchRegex(value);
      updateMatchRegexHint('');
      return { ok: true, regex, value };
    } catch (error) {
      const message = error && error.message ? error.message : i18nText('common.error', '错误');
      updateMatchRegexHint(message);
      return { ok: false, regex: null, value, error: message };
    }
  }

  function getAutoMatchCandidates() {
    const { nameInput } = getFormElements();
    const name = normalizeText(nameInput && nameInput.value);
    const regexResult = validateMatchRegexInput();
    if (!regexResult.ok) {
      return { error: i18nText('groups.messages.invalidRegex', '正则无效：{error}', { error: regexResult.error }) };
    }

    if (regexResult.regex) {
      return {
        candidates: state.modelOptions.filter((option) => regexResult.regex.test(option.model_name))
      };
    }

    if (!name) {
      return { candidates: [] };
    }

    return {
      candidates: state.modelOptions.filter((option) => matchesGroupName(option.model_name, name))
    };
  }

  function getAutoAddDisabled() {
    if (state.modelOptions.length === 0) {
      return true;
    }

    const matchResult = getAutoMatchCandidates();
    if (matchResult.error) {
      return true;
    }

    const candidates = Array.isArray(matchResult.candidates) ? matchResult.candidates : [];
    if (candidates.length === 0) {
      return true;
    }

    const selectedKeys = new Set(state.selectedItems.map((item) => groupItemKey(item)));
    return candidates.every((option) => selectedKeys.has(modelOptionKey(option.channel_id, option.model_name)));
  }

  function renderActionStates() {
    const { autoAddButton, clearButton } = getFormElements();
    if (autoAddButton) {
      const autoAddDisabled = getAutoAddDisabled();
      autoAddButton.disabled = autoAddDisabled;
      autoAddButton.setAttribute('aria-disabled', autoAddDisabled ? 'true' : 'false');
    }
    if (clearButton) {
      const clearDisabled = state.selectedItems.length === 0;
      clearButton.disabled = clearDisabled;
      clearButton.setAttribute('aria-disabled', clearDisabled ? 'true' : 'false');
    }
  }

  function renderSelectedSummary() {
    const { selectedSummary, selectedCount } = getFormElements();
    if (selectedCount) {
      selectedCount.textContent = `(${state.selectedItems.length})`;
    }

    if (!selectedSummary) {
      return;
    }

    const priorityHint = i18nText('groups.hint.priorityAuto', '右边顺序就是优先级，第 1 个最先尝试。');
    const weightedHint = isWeightedMode()
      ? i18nText('groups.hint.weightedMode', '加权模式下，权重越大越容易被选中。')
      : '';
    selectedSummary.textContent = weightedHint
      ? `${priorityHint} ${weightedHint}`
      : priorityHint;
  }

  function renderPickerList() {
    const { pickerList, searchInput } = getFormElements();
    if (!pickerList) {
      return;
    }

    const searchKeyword = normalizeText((searchInput && searchInput.value) || state.modelSearch).toLowerCase();
    const selectedKeys = new Set(state.selectedItems.map((item) => groupItemKey(item)));
    const channels = new Map();

    state.modelOptions.forEach((option) => {
      if (!channels.has(option.channel_id)) {
        channels.set(option.channel_id, {
          channel_id: option.channel_id,
          channel_name: option.channel_name,
          items: []
        });
      }
      channels.get(option.channel_id).items.push(option);
    });

    const channelBlocks = Array.from(channels.values())
      .sort((left, right) => left.channel_id - right.channel_id)
      .map((channel) => {
        const items = channel.items
          .slice()
          .sort((left, right) => left.model_name.localeCompare(right.model_name))
          .filter((option) => {
            if (!searchKeyword) {
              return true;
            }
            return option.channel_name.toLowerCase().includes(searchKeyword)
              || option.model_name.toLowerCase().includes(searchKeyword);
          });

        if (items.length === 0) {
          return '';
        }

        const buttons = items.map((option) => {
          const selected = selectedKeys.has(modelOptionKey(option.channel_id, option.model_name));
          return `
            <button
              type="button"
              class="groups-model-button${selected ? ' is-selected' : ''}"
              data-action="add-group-item"
              data-option-key="${escapeText(modelOptionKey(option.channel_id, option.model_name))}"
              ${selected ? 'disabled' : ''}>
              <span class="groups-model-main">
                <span class="groups-model-name">${escapeText(option.model_name)}</span>
                <span class="groups-model-channel">${escapeText(option.channel_name || `#${option.channel_id}`)}</span>
              </span>
              <span class="stat-badge">${selected ? '✓' : '+'}</span>
            </button>
          `;
        }).join('');

        const selectedCount = items.filter((option) => selectedKeys.has(modelOptionKey(option.channel_id, option.model_name))).length;
        const availableCount = items.length - selectedCount;
        return `
          <details class="groups-channel-block" open>
            <summary class="groups-channel-summary">
              <span>${escapeText(channel.channel_name || `#${channel.channel_id}`)}</span>
              <span class="groups-channel-count">${availableCount}/${items.length}</span>
            </summary>
            <div class="groups-channel-models">${buttons}</div>
          </details>
        `;
      })
      .filter(Boolean);

    if (channelBlocks.length === 0) {
      pickerList.innerHTML = `
        <div class="groups-selected-empty">
          ${escapeText(i18nText('groups.messages.noSearchMatch', '没有匹配到模型'))}
        </div>
      `;
      return;
    }

    pickerList.innerHTML = channelBlocks.join('');
  }

  function renderSelectedList() {
    const { selectedList } = getFormElements();
    if (!selectedList) {
      return;
    }

    syncSelectedPriorities();

    if (state.selectedItems.length === 0) {
      selectedList.innerHTML = `
        <div class="groups-selected-empty">
          ${escapeText(i18nText('groups.messages.noSelectedModels', '右边还没有模型成员，先从左边点几个模型进来。'))}
        </div>
      `;
      return;
    }

    const weighted = isWeightedMode();
    selectedList.innerHTML = state.selectedItems.map((item, index) => `
      <article class="groups-selected-item${state.draggingIndex === index ? ' is-dragging' : ''}" data-group-item-index="${index}" draggable="true">
        <div class="groups-selected-controls">
          <span class="groups-priority-badge">${index + 1}</span>
          <span class="groups-drag-handle" aria-hidden="true">⋮⋮</span>
          <div class="groups-selected-main">
            <h4 class="groups-selected-name">${escapeText(item.model_name)}</h4>
            <div class="groups-selected-channel">${escapeText(resolveChannelName(item.channel_id, item.model_name, item.channel_name))}</div>
          </div>
          <div class="groups-selected-actions">
            ${weighted ? `
              <input
                type="number"
                class="form-input groups-weight-input"
                min="1"
                step="1"
                value="${Math.max(1, Number(item.weight) || 1)}"
                data-change-action="update-group-item-weight"
                data-index="${index}" />
            ` : ''}
            <button
              type="button"
              class="btn btn-secondary btn-sm"
              data-action="delete-group-item"
              data-index="${index}">
              ${escapeText(i18nText('groups.actions.removeItem', '删除成员'))}
            </button>
          </div>
        </div>
      </article>
    `).join('');
  }

  function renderEditor() {
    renderModeButtons();
    renderActionStates();
    renderSelectedSummary();
    renderSelectedList();
    renderPickerList();
  }

  function formatMetaBadge(label, value) {
    return `
      <span class="groups-meta-badge">
        <strong>${escapeText(label)}</strong>
        <span>${escapeText(value)}</span>
      </span>
    `;
  }

  function renderGroupCards() {
    const container = document.getElementById('groups-container');
    if (!container) {
      return;
    }

    if (!Array.isArray(state.groups) || state.groups.length === 0) {
      container.innerHTML = `
        <div class="glass-card" style="padding: var(--space-6);">
          <div class="page-subtitle">${escapeText(i18nText('groups.empty', '还没有模型'))}</div>
        </div>
      `;
      return;
    }

    container.innerHTML = state.groups.map((group) => {
      const items = sortGroupItems(Array.isArray(group.items) ? group.items : []);
      const badges = [
        group.match_regex
          ? formatMetaBadge(i18nText('groups.field.matchRegex', '匹配正则'), group.match_regex)
          : formatMetaBadge(i18nText('groups.field.matchRegex', '匹配正则'), i18nText('groups.meta.matchByName', '按模型名模糊匹配'))
      ];

      if (group.first_token_time_out > 0) {
        badges.push(formatMetaBadge(
          i18nText('groups.field.firstTokenTimeout', '首包超时'),
          `${group.first_token_time_out}${i18nText('common.seconds', '秒')}`
        ));
      }

      if (group.session_keep_time > 0) {
        badges.push(formatMetaBadge(
          i18nText('groups.field.sessionKeepTime', '会话保持'),
          `${group.session_keep_time}${i18nText('common.seconds', '秒')}`
        ));
      }

      const rows = items.length > 0
        ? items.map((item, index) => `
            <tr>
              <td>${escapeText(resolveChannelName(item.channel_id, item.model_name, item.channel_name))}</td>
              <td><span class="model-tag">${escapeText(item.model_name)}</span></td>
              <td>${index + 1}</td>
              <td>${Math.max(1, Number(item.weight) || 1)}</td>
            </tr>
          `).join('')
        : `<tr><td colspan="4" style="color: var(--neutral-500);">${escapeText(i18nText('groups.noItems', '暂无成员'))}</td></tr>`;

      return `
        <article class="glass-card" style="padding: var(--space-6);">
          <div style="display: flex; justify-content: space-between; gap: 16px; align-items: flex-start; margin-bottom: 20px; flex-wrap: wrap;">
            <div>
              <div style="display: flex; gap: 10px; align-items: center; flex-wrap: wrap;">
                <h3 style="margin: 0; font-size: 1.25rem;">${escapeText(group.name)}</h3>
                <span class="stat-badge">${escapeText(modeLabel(group.mode))}</span>
              </div>
              <p class="page-subtitle" style="margin: 8px 0 0 0;">
                ${escapeText(i18nText('groups.members', '{count} 个成员', { count: items.length }))}
              </p>
              <div class="groups-badges">${badges.join('')}</div>
            </div>
            <div style="display: flex; gap: 8px; flex-wrap: wrap;">
              <button type="button" class="btn btn-secondary btn-sm" data-action="edit-group" data-group-id="${group.id}">
                ${escapeText(i18nText('common.edit', '编辑'))}
              </button>
              <button type="button" class="btn btn-secondary btn-sm" data-action="delete-group" data-group-id="${group.id}">
                ${escapeText(i18nText('common.delete', '删除'))}
              </button>
            </div>
          </div>
          <div class="table-container">
            <table class="modern-table">
              <thead>
                <tr>
                  <th>${escapeText(i18nText('groups.channel', '渠道'))}</th>
                  <th>${escapeText(i18nText('groups.model', '真实模型'))}</th>
                  <th>${escapeText(i18nText('groups.priority', '优先级'))}</th>
                  <th>${escapeText(i18nText('groups.weight', '权重'))}</th>
                </tr>
              </thead>
              <tbody>${rows}</tbody>
            </table>
          </div>
        </article>
      `;
    }).join('');
  }

  function showModal() {
    const { modal } = getFormElements();
    if (modal) {
      modal.classList.add('show');
    }
  }

  function closeModal() {
    const {
      modal,
      form,
      title,
      nameInput,
      modeInput,
      matchRegexInput,
      firstTokenTimeoutInput,
      sessionKeepTimeInput,
      searchInput
    } = getFormElements();

    state.editingGroup = null;
    state.selectedItems = [];
    state.modelSearch = '';
    state.draggingIndex = null;

    if (form) {
      form.reset();
    }
    if (title) {
      title.textContent = i18nText('groups.modal.createTitle', '添加模型');
    }
    if (nameInput) {
      nameInput.value = '';
    }
    if (modeInput) {
      modeInput.value = '3';
    }
    if (matchRegexInput) {
      matchRegexInput.value = '';
    }
    if (firstTokenTimeoutInput) {
      firstTokenTimeoutInput.value = '0';
    }
    if (sessionKeepTimeInput) {
      sessionKeepTimeInput.value = '0';
    }
    if (searchInput) {
      searchInput.value = '';
    }
    updateMatchRegexHint('');
    renderEditor();
    if (modal) {
      modal.classList.remove('show');
    }
  }

  function openCreateModal() {
    closeModal();
    showModal();
  }

  function openEditModal(groupId) {
    const group = state.groups.find((entry) => String(entry.id) === String(groupId));
    if (!group) {
      return;
    }

    const {
      title,
      nameInput,
      modeInput,
      matchRegexInput,
      firstTokenTimeoutInput,
      sessionKeepTimeInput,
      searchInput
    } = getFormElements();

    state.editingGroup = group;
    state.selectedItems = sortGroupItems(group.items || []).map((item) => cloneGroupItem(item));
    state.modelSearch = '';
    state.draggingIndex = null;

    if (title) {
      title.textContent = i18nText('groups.modal.editTitle', '编辑模型');
    }
    if (nameInput) {
      nameInput.value = group.name || '';
    }
    if (modeInput) {
      modeInput.value = String(normalizeMode(group.mode));
    }
    if (matchRegexInput) {
      matchRegexInput.value = group.match_regex || '';
    }
    if (firstTokenTimeoutInput) {
      firstTokenTimeoutInput.value = String(group.first_token_time_out || 0);
    }
    if (sessionKeepTimeInput) {
      sessionKeepTimeInput.value = String(group.session_keep_time || 0);
    }
    if (searchInput) {
      searchInput.value = '';
    }

    validateMatchRegexInput();
    renderEditor();
    showModal();
  }

  function addGroupItem(optionKeyValue) {
    const parsed = parseModelOptionKey(optionKeyValue);
    const option = findModelOption(parsed.channel_id, parsed.model_name);
    if (!option) {
      return;
    }

    const key = modelOptionKey(option.channel_id, option.model_name);
    if (state.selectedItems.some((item) => groupItemKey(item) === key)) {
      return;
    }

    state.selectedItems.push({
      id: 0,
      channel_id: option.channel_id,
      channel_name: option.channel_name,
      model_name: option.model_name,
      priority: state.selectedItems.length + 1,
      weight: 1
    });
    renderEditor();
  }

  function autoAddGroupItems() {
    if (getAutoAddDisabled()) {
      return;
    }

    const matchResult = getAutoMatchCandidates();
    const selectedKeys = new Set(state.selectedItems.map((item) => groupItemKey(item)));
    const candidates = Array.isArray(matchResult.candidates) ? matchResult.candidates : [];
    const toAdd = candidates.filter((option) => !selectedKeys.has(modelOptionKey(option.channel_id, option.model_name)));

    toAdd.forEach((option) => {
      state.selectedItems.push({
        id: 0,
        channel_id: option.channel_id,
        channel_name: option.channel_name,
        model_name: option.model_name,
        priority: state.selectedItems.length + 1,
        weight: 1
      });
    });

    renderEditor();
  }

  function clearGroupItems() {
    state.draggingIndex = null;
    state.selectedItems = [];
    renderEditor();
  }

  function removeGroupItem(indexValue) {
    const index = Number(indexValue);
    if (!Number.isInteger(index) || index < 0 || index >= state.selectedItems.length) {
      return;
    }
    state.draggingIndex = null;
    state.selectedItems.splice(index, 1);
    renderEditor();
  }

  function reorderSelectedItems(fromIndexValue, rawInsertIndexValue) {
    const fromIndex = Number(fromIndexValue);
    const rawInsertIndex = Number(rawInsertIndexValue);
    if (!Number.isInteger(fromIndex) || fromIndex < 0 || fromIndex >= state.selectedItems.length) {
      return;
    }

    const nextItems = state.selectedItems.slice();
    const [movedItem] = nextItems.splice(fromIndex, 1);
    let insertIndex = Number.isFinite(rawInsertIndex) ? rawInsertIndex : state.selectedItems.length;
    insertIndex = Math.max(0, Math.min(insertIndex, state.selectedItems.length));
    if (insertIndex > fromIndex) {
      insertIndex -= 1;
    }
    insertIndex = Math.max(0, Math.min(insertIndex, nextItems.length));
    nextItems.splice(insertIndex, 0, movedItem);
    state.selectedItems = nextItems;
    state.draggingIndex = null;
    renderEditor();
  }

  function updateGroupItemWeight(target) {
    const index = Number(target && target.dataset && target.dataset.index);
    if (!Number.isInteger(index) || index < 0 || index >= state.selectedItems.length) {
      return;
    }
    state.selectedItems[index].weight = Math.max(1, Number(target.value) || 1);
  }

  function buildPreparedItems() {
    syncSelectedPriorities();
    return state.selectedItems.map((item, index) => ({
      ...cloneGroupItem(item),
      priority: index + 1
    }));
  }

  function buildCreatePayload(values) {
    return {
      name: values.name,
      mode: values.mode,
      match_regex: values.match_regex,
      first_token_time_out: values.first_token_time_out,
      session_keep_time: values.session_keep_time,
      items: values.items.map((item) => ({
        channel_id: item.channel_id,
        model_name: item.model_name,
        priority: item.priority,
        weight: item.weight
      }))
    };
  }

  function buildUpdatePayload(values) {
    const originalItems = Array.isArray(state.editingGroup && state.editingGroup.items)
      ? state.editingGroup.items
      : [];
    const keptIds = new Set(values.items.filter((item) => item.id > 0).map((item) => item.id));

    return {
      name: values.name,
      mode: values.mode,
      match_regex: values.match_regex,
      first_token_time_out: values.first_token_time_out,
      session_keep_time: values.session_keep_time,
      items_to_add: values.items
        .filter((item) => item.id <= 0)
        .map((item) => ({
          channel_id: item.channel_id,
          model_name: item.model_name,
          priority: item.priority,
          weight: item.weight
        })),
      items_to_update: values.items
        .filter((item) => item.id > 0)
        .map((item) => ({
          id: item.id,
          channel_id: item.channel_id,
          model_name: item.model_name,
          priority: item.priority,
          weight: item.weight
        })),
      items_to_delete: originalItems
        .filter((item) => Number(item.id) > 0 && !keptIds.has(Number(item.id)))
        .map((item) => Number(item.id))
    };
  }

  function readSaveValues() {
    const {
      nameInput,
      modeInput,
      matchRegexInput,
      firstTokenTimeoutInput,
      sessionKeepTimeInput
    } = getFormElements();

    const name = normalizeText(nameInput && nameInput.value);
    if (!name) {
      throw new Error(i18nText('groups.validation.nameRequired', '模型名不能为空'));
    }

    const regexResult = validateMatchRegexInput();
    if (!regexResult.ok) {
      throw new Error(i18nText('groups.messages.invalidRegex', '正则无效：{error}', { error: regexResult.error }));
    }

    const items = buildPreparedItems();
    if (items.length === 0) {
      throw new Error(i18nText('groups.validation.itemRequired', '请先补全模型成员'));
    }

    return {
      name,
      mode: normalizeMode(modeInput && modeInput.value),
      match_regex: normalizeText(matchRegexInput && matchRegexInput.value),
      first_token_time_out: normalizeSeconds(firstTokenTimeoutInput && firstTokenTimeoutInput.value),
      session_keep_time: normalizeSeconds(sessionKeepTimeInput && sessionKeepTimeInput.value),
      items
    };
  }

  async function submitGroupForm(event) {
    event.preventDefault();

    try {
      const values = readSaveValues();
      const isEditing = Boolean(state.editingGroup && state.editingGroup.id);
      const request = isEditing
        ? {
            url: `/admin/groups/${state.editingGroup.id}`,
            options: {
              method: 'PUT',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify(buildUpdatePayload(values))
            },
            successMessage: i18nText('groups.messages.updated', '模型已更新')
          }
        : {
            url: '/admin/groups',
            options: {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify(buildCreatePayload(values))
            },
            successMessage: i18nText('groups.messages.created', '模型已创建')
          };

      await fetchDataWithAuth(request.url, request.options);
      closeModal();
      await loadGroups();
      if (typeof window.showSuccess === 'function') {
        window.showSuccess(request.successMessage);
      }
    } catch (error) {
      if (typeof window.showError === 'function') {
        window.showError(error && error.message ? error.message : i18nText('groups.messages.saveFailed', '保存模型失败'));
      }
    }
  }

  async function deleteGroup(groupId) {
    const group = state.groups.find((entry) => String(entry.id) === String(groupId));
    if (!group) {
      return;
    }

    const confirmed = window.confirm(i18nText('groups.confirmDelete', '确定删除模型 “{name}” 吗？', { name: group.name }));
    if (!confirmed) {
      return;
    }

    try {
      await fetchDataWithAuth(`/admin/groups/${group.id}`, { method: 'DELETE' });
      await loadGroups();
      if (typeof window.showSuccess === 'function') {
        window.showSuccess(i18nText('groups.messages.deleted', '模型已删除'));
      }
    } catch (error) {
      if (typeof window.showError === 'function') {
        window.showError(error && error.message ? error.message : i18nText('groups.messages.deleteFailed', '删除模型失败'));
      }
    }
  }

  async function loadGroups() {
    const [groups, modelOptions] = await Promise.all([
      fetchDataWithAuth('/admin/groups'),
      fetchDataWithAuth('/admin/groups/model-options')
    ]);

    state.modelOptions = Array.isArray(modelOptions)
      ? modelOptions.map(normalizeModelOption).filter((option) => option.channel_id > 0 && option.model_name)
      : [];
    state.groups = Array.isArray(groups) ? groups.map(normalizeGroup) : [];

    renderGroupCards();
    renderEditor();
  }

  function bindModalForm() {
    const {
      form,
      modal,
      nameInput,
      searchInput,
      matchRegexInput,
      selectedList
    } = getFormElements();

    if (form && !form.dataset.groupFormBound) {
      form.addEventListener('submit', submitGroupForm);
      form.dataset.groupFormBound = '1';
    }

    if (modal && !modal.dataset.groupModalBound) {
      modal.addEventListener('click', (event) => {
        if (event.target === modal) {
          closeModal();
        }
      });
      modal.dataset.groupModalBound = '1';
    }

    if (searchInput && !searchInput.dataset.groupSearchBound) {
      searchInput.addEventListener('input', () => {
        state.modelSearch = searchInput.value || '';
        renderPickerList();
      });
      searchInput.dataset.groupSearchBound = '1';
    }

    if (matchRegexInput && !matchRegexInput.dataset.groupRegexBound) {
      matchRegexInput.addEventListener('input', () => {
        validateMatchRegexInput();
        renderActionStates();
      });
      matchRegexInput.dataset.groupRegexBound = '1';
    }

    if (nameInput && !nameInput.dataset.groupNameBound) {
      nameInput.addEventListener('input', () => {
        renderActionStates();
      });
      nameInput.dataset.groupNameBound = '1';
    }

    if (selectedList && !selectedList.dataset.groupDragBound) {
      selectedList.addEventListener('dragstart', (event) => {
        const target = event.target instanceof Element ? event.target : null;
        const item = target ? target.closest('[data-group-item-index]') : null;
        if (!item) {
          return;
        }

        state.draggingIndex = Number(item.dataset.groupItemIndex);
        item.classList.add('is-dragging');

        if (event.dataTransfer) {
          event.dataTransfer.effectAllowed = 'move';
          event.dataTransfer.setData('text/plain', String(state.draggingIndex));
        }
      });

      selectedList.addEventListener('dragover', (event) => {
        if (!Number.isInteger(state.draggingIndex)) {
          return;
        }
        event.preventDefault();
        if (event.dataTransfer) {
          event.dataTransfer.dropEffect = 'move';
        }
      });

      selectedList.addEventListener('drop', (event) => {
        if (!Number.isInteger(state.draggingIndex)) {
          return;
        }

        event.preventDefault();
        const target = event.target instanceof Element ? event.target : null;
        const item = target ? target.closest('[data-group-item-index]') : null;
        if (!item) {
          reorderSelectedItems(state.draggingIndex, state.selectedItems.length);
          return;
        }

        const targetIndex = Number(item.dataset.groupItemIndex);
        const rect = item.getBoundingClientRect();
        const placeAfter = event.clientY > rect.top + (rect.height / 2);
        const rawInsertIndex = placeAfter ? targetIndex + 1 : targetIndex;
        reorderSelectedItems(state.draggingIndex, rawInsertIndex);
      });

      selectedList.addEventListener('dragend', () => {
        state.draggingIndex = null;
        selectedList.querySelectorAll('.groups-selected-item.is-dragging').forEach((item) => {
          item.classList.remove('is-dragging');
        });
      });

      selectedList.dataset.groupDragBound = '1';
    }

    if (!document.body.dataset.groupLocaleBound) {
      window.addEventListener('localechange', () => {
        validateMatchRegexInput();
        renderGroupCards();
        renderEditor();
      });
      document.body.dataset.groupLocaleBound = '1';
    }
  }

  function initGroupsPageActions() {
    if (typeof window.initDelegatedActions !== 'function') {
      return;
    }

    window.initDelegatedActions({
      root: document,
      boundElement: document.body,
      boundKey: 'groupsPageActionsBound',
      click: {
        'show-add-group': () => openCreateModal(),
        'edit-group': (target) => openEditModal(target.dataset.groupId),
        'delete-group': (target) => {
          void deleteGroup(target.dataset.groupId);
        },
        'close-group-modal': () => closeModal(),
        'select-group-mode': (target) => selectGroupMode(target.dataset.mode),
        'add-group-item': (target) => addGroupItem(target.dataset.optionKey),
        'auto-add-group-items': () => autoAddGroupItems(),
        'clear-group-items': () => clearGroupItems(),
        'delete-group-item': (target) => removeGroupItem(target.dataset.index)
      },
      change: {
        'update-group-item-weight': (target) => updateGroupItemWeight(target)
      }
    });
  }

  window.initPageBootstrap({
    topbarKey: 'groups',
    run: async () => {
      bindModalForm();
      initGroupsPageActions();
      try {
        await loadGroups();
      } catch (error) {
        renderGroupCards();
        renderEditor();
        if (typeof window.showError === 'function') {
          window.showError(error && error.message ? error.message : i18nText('groups.messages.loadFailed', '加载模型失败'));
        }
      }
    }
  });
})();
