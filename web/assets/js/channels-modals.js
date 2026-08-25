function setChannelModalTitle(i18nKey) {
  const titleEl = document.getElementById('modalTitle');
  if (!titleEl) return;

  titleEl.setAttribute('data-i18n', i18nKey);
  titleEl.textContent = window.t(i18nKey);
}

function normalizeProtocolTransformMode(value) {
  const mode = String(value || '').trim().toLowerCase();
  return ['auto', 'upstream', 'local'].includes(mode) ? mode : 'auto';
}

let protocolTransformModeCombobox = null;
let quickAddChannelTrigger = null;
let quickAddChannelRequestVersion = 0;
let modelImportTrigger = null;
let modelImportTarget = 'channel';

function getProtocolTransformModeOptions() {
  return [
    { value: 'auto', label: window.t('channels.modal.protocolTransformModeAuto') },
    { value: 'upstream', label: window.t('channels.modal.protocolTransformModeUpstream') },
    { value: 'local', label: window.t('channels.modal.protocolTransformModeLocal') }
  ];
}

function getProtocolTransformModeLabel(value) {
  const mode = normalizeProtocolTransformMode(value);
  return getProtocolTransformModeOptions().find(option => option.value === mode)?.label || mode;
}

function getProtocolTransformModeHelp(value) {
  const mode = normalizeProtocolTransformMode(value);
  const keys = {
    auto: 'channels.modal.protocolTransformModeAutoHelp',
    upstream: 'channels.modal.protocolTransformModeUpstreamHelp',
    local: 'channels.modal.protocolTransformModeLocalHelp'
  };
  return window.t(keys[mode]);
}

function setProtocolTransformMode(value) {
  const mode = normalizeProtocolTransformMode(value);
  const hiddenInput = document.getElementById('protocolTransformModeValue');
  if (hiddenInput) hiddenInput.value = mode;
  protocolTransformModeCombobox?.setValue(mode, getProtocolTransformModeLabel(mode));
  const visibleInput = protocolTransformModeCombobox?.getInput();
  if (visibleInput) visibleInput.title = getProtocolTransformModeHelp(mode);
}

function getProtocolTransformMode() {
  const hiddenInput = document.getElementById('protocolTransformModeValue');
  return normalizeProtocolTransformMode(hiddenInput?.value || protocolTransformModeCombobox?.getValue());
}

async function ensureProtocolTransformModeCombobox(transformMode) {
  if (!protocolTransformModeCombobox) {
    protocolTransformModeCombobox = createSearchableCombobox({
      container: 'protocolTransformModeSelectContainer',
      inputId: 'protocolTransformModeInput',
      dropdownId: 'protocolTransformModeDropdown',
      minWidth: 0,
      getOptions: getProtocolTransformModeOptions,
      initialValue: 'auto',
      initialLabel: getProtocolTransformModeLabel('auto'),
      onSelect: (value) => {
        setProtocolTransformMode(value);
        markChannelFormDirty();
      }
    });
  }

  setProtocolTransformMode(transformMode ?? getProtocolTransformMode());
  protocolTransformModeCombobox?.refresh();
}

if (typeof window !== 'undefined' && typeof window.addEventListener === 'function') {
  window.addEventListener('localechange', () => {
    setProtocolTransformMode(getProtocolTransformMode());
    protocolTransformModeCombobox?.refresh();
  });
}

async function syncScheduledCheckVisibility(enabledOverride) {
  const scheduledCheckWrapper = document.getElementById('channelScheduledCheckEnabledWrapper');
  const scheduledCheckModelWrapper = document.getElementById('channelScheduledCheckModelWrapper');
  if (!scheduledCheckWrapper) return false;

  let enabled = enabledOverride === true;
  if (typeof enabledOverride !== 'boolean') {
    try {
      const setting = await fetchDataWithAuth('/admin/settings/channel_check_interval_hours');
      const intervalHours = Number(setting && setting.value);
      enabled = Number.isFinite(intervalHours) && intervalHours > 0;
    } catch (error) {
      console.warn('Failed to load channel check interval setting', error);
    }
  }

  scheduledCheckWrapper.hidden = !enabled;
  if (scheduledCheckModelWrapper) {
    scheduledCheckModelWrapper.hidden = !enabled;
  }
  syncScheduledCheckModelState();
  return enabled;
}

function setScheduledCheckModelHint(i18nKey) {
  const hint = document.getElementById('channelScheduledCheckModelHint');
  if (!hint) return;
  hint.setAttribute('data-i18n', i18nKey);
  hint.textContent = window.t(i18nKey);
}

let scheduledCheckModelCombobox = null;

function getScheduledCheckModelNames() {
  return redirectTableData
    .filter(entry => entry && !entry.disabled)
    .map(entry => (entry.model ? entry.model.trim() : ''))
    .filter(Boolean);
}

function getScheduledCheckModelDefaultLabel() {
  return window.t('channels.scheduledCheckModelDefault');
}

function scheduledCheckModelInputValueFromValue(value) {
  return value || getScheduledCheckModelDefaultLabel();
}

function getScheduledCheckModelOptions() {
  return [{ value: '', label: getScheduledCheckModelDefaultLabel() }].concat(
    getScheduledCheckModelNames().map(modelName => ({ value: modelName, label: modelName }))
  );
}

function ensureScheduledCheckModelCombobox() {
  if (scheduledCheckModelCombobox) return scheduledCheckModelCombobox;

  const hiddenInput = document.getElementById('channelScheduledCheckModel');
  const input = document.getElementById('channelScheduledCheckModelInput');
  const dropdown = document.getElementById('channelScheduledCheckModelDropdown');
  if (!hiddenInput || !input || !dropdown || typeof createSearchableCombobox !== 'function') return null;

  scheduledCheckModelCombobox = createSearchableCombobox({
    attachMode: true,
    inputId: 'channelScheduledCheckModelInput',
    dropdownId: 'channelScheduledCheckModelDropdown',
    initialValue: hiddenInput.value || '',
    initialLabel: scheduledCheckModelInputValueFromValue(hiddenInput.value || ''),
    getOptions: () => getScheduledCheckModelOptions(),
    onSelect: (value) => {
      hiddenInput.value = value;
      setScheduledCheckModelHint('channels.scheduledCheckModelHint');
    }
  });

  return scheduledCheckModelCombobox;
}

function syncScheduledCheckModelState() {
  const wrapper = document.getElementById('channelScheduledCheckModelWrapper');
  const hiddenInput = document.getElementById('channelScheduledCheckModel');
  const input = document.getElementById('channelScheduledCheckModelInput');
  const checkbox = document.getElementById('channelScheduledCheckEnabled');
  if (!wrapper || !hiddenInput || !input || !checkbox) return;

  const modelNames = getScheduledCheckModelNames();
  const currentValue = hiddenInput.value || '';
  const isValid = currentValue === '' || modelNames.includes(currentValue);
  const nextValue = isValid ? currentValue : '';
  hiddenInput.value = nextValue;
  setScheduledCheckModelHint(isValid ? 'channels.scheduledCheckModelHint' : 'channels.scheduledCheckModelFallback');

  const combobox = ensureScheduledCheckModelCombobox();
  const nextLabel = scheduledCheckModelInputValueFromValue(nextValue);
  if (combobox) {
    combobox.setValue(nextValue, nextLabel);
    combobox.refresh();
  } else {
    input.value = nextLabel;
  }

  input.disabled = wrapper.hidden || !checkbox.checked;
}

function setChannelWebsocketChecked(checkbox, checked) {
  if (checkbox.checked === checked) return;
  checkbox.checked = checked;
  if (typeof markChannelFormDirty === 'function') {
    markChannelFormDirty();
  }
}

function getEnabledChannelWebsocketURLs() {
  if (typeof getValidInlineURLConfigs !== 'function' || typeof runtimeInlineURL !== 'function') return [];
  const stats = typeof urlStatsMap !== 'undefined' && urlStatsMap ? urlStatsMap : {};
  return getValidInlineURLConfigs()
    .filter(entry => entry.protocols.length === 0 || entry.protocols.includes('codex'))
    .map(runtimeInlineURL)
    .filter(url => url && !stats[url]?.disabled);
}

function getEnabledChannelWebsocketKeys() {
  if (typeof getInlineKeyRows !== 'function') return [];
  const states = typeof currentChannelKeyCooldowns !== 'undefined' ? currentChannelKeyCooldowns : [];
  const disabledIndices = new Set(
    (Array.isArray(states) ? states : [])
      .filter(state => state && state.disabled)
      .map(state => Number(state.key_index))
  );
  return getInlineKeyRows()
    .map((row, index) => ({
      index,
      apiKey: String(row && typeof row === 'object' ? (row.api_key || '') : (row || '')).trim()
    }))
    .filter(item => item.apiKey && !disabledIndices.has(item.index))
    .map(item => item.apiKey);
}

async function detectChannelWebsocketSupport(button) {
  const checkbox = document.getElementById('channelWebsockets');
  if (!checkbox) return false;

  const baseURLs = getEnabledChannelWebsocketURLs();
  if (baseURLs.length === 0) {
    if (window.showError) window.showError(window.t('channels.fillApiUrlFirst'));
    else alert(window.t('channels.fillApiUrlFirst'));
    return false;
  }
  const apiKeys = getEnabledChannelWebsocketKeys();
  if (apiKeys.length === 0) {
    if (window.showError) window.showError(window.t('channels.addAtLeastOneEnabledKey'));
    else alert(window.t('channels.addAtLeastOneEnabledKey'));
    return false;
  }

  const originalHTML = button?.innerHTML || '';
  if (button) {
    button.disabled = true;
    button.textContent = window.t('channels.websocketsProbing');
  }
  try {
    const customRules = invokeChannelEditorAction('collectCustomRulesForSubmit') || null;
    const proxyURL = (document.getElementById('channelProxyURL')?.value || '').trim();
    let supported = true;
    for (const [index, baseURL] of baseURLs.entries()) {
      const result = await fetchDataWithAuth('/admin/channels/websocket-probe', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          url: baseURL,
          api_key: apiKeys[index % apiKeys.length],
          proxy_url: proxyURL,
          custom_request_rules: customRules
        })
      });
      if (!result || !result.supported) {
        supported = false;
        break;
      }
    }
    setChannelWebsocketChecked(checkbox, supported);
    window.showNotification(
      window.t(supported ? 'channels.websocketsProbeSupported' : 'channels.websocketsProbeUnsupported'),
      supported ? 'success' : 'warning'
    );
    return supported;
  } catch (error) {
    setChannelWebsocketChecked(checkbox, false);
    const message = window.t('channels.websocketsProbeFailed', { error: error.message });
    if (window.showError) window.showError(message);
    else alert(message);
    return false;
  } finally {
    if (button) {
      button.disabled = false;
      button.innerHTML = originalHTML;
    }
  }
}

async function handleChannelSaveSuccess({ isNewChannel, savedChannelId, response }) {
  if (window.ChannelModalHooks && typeof window.ChannelModalHooks.afterSave === 'function') {
    await window.ChannelModalHooks.afterSave({
      isNewChannel,
      savedChannelId,
      response
    });
    return;
  }

  if (isNewChannel) {
    channelsCurrentPage = 1;
  }
  if (typeof saveChannelsFilters === 'function') saveChannelsFilters();

  if (typeof reloadChannelsList === 'function') {
    await reloadChannelsList();
  } else if (typeof loadChannels === 'function') {
    await loadChannels();
  }
}

function invokeChannelEditorAction(actionName, ...args) {
  const action = window[actionName];
  if (typeof action === 'function') {
    return action(...args);
  }
  return undefined;
}

function initChannelEditorActions() {
  if (typeof window.initDelegatedActions === 'function') {
    window.initDelegatedActions({
      root: document.body,
      boundElement: document.body,
      boundKey: 'channelEditorActionsBound',
      click: {
        'close-channel-modal': () => invokeChannelEditorAction('closeModal'),
        'open-quick-add-channel': (actionTarget) => openQuickAddChannelModal(actionTarget),
        'close-quick-add-channel': () => closeQuickAddChannelModal(),
        'add-inline-url': () => invokeChannelEditorAction('addInlineURL'),
        'batch-delete-urls': () => invokeChannelEditorAction('batchDeleteSelectedURLs'),
        'open-key-import-modal': () => invokeChannelEditorAction('openKeyImportModal'),
        'open-key-export-modal': () => invokeChannelEditorAction('openKeyExportModal'),
        'toggle-inline-key-visibility': () => invokeChannelEditorAction('toggleInlineKeyVisibility'),
        'batch-delete-keys': () => invokeChannelEditorAction('batchDeleteSelectedKeys'),
        'add-common-models': (actionTarget) => openCommonModelsModal(actionTarget),
        'close-common-models-modal': () => closeCommonModelsModal(),
        'confirm-common-models': () => confirmCommonModelsSelection(),
        'fetch-models-from-api': () => invokeChannelEditorAction('fetchModelsFromAPI'),
        'fetch-sub2api-rate': () => invokeChannelEditorAction('fetchSub2APIRate'),
        'add-redirect-row': () => invokeChannelEditorAction('addRedirectRow'),
        'export-channel-models': () => invokeChannelEditorAction('exportChannelModels'),
        'open-batch-model-import': () => invokeChannelEditorAction('openBatchModelImportModal'),
        'batch-lowercase-models': () => invokeChannelEditorAction('batchLowercaseSelectedModels'),
        'batch-strip-model-source-prefix': () => invokeChannelEditorAction('batchStripSelectedModelSourcePrefixes'),
        'batch-delete-models': () => invokeChannelEditorAction('batchDeleteSelectedModels'),
        'close-delete-modal': () => invokeChannelEditorAction('closeDeleteModal'),
        'confirm-delete-channel': () => invokeChannelEditorAction('confirmDelete'),
        'close-key-import-modal': () => invokeChannelEditorAction('closeKeyImportModal'),
        'confirm-inline-key-import': () => invokeChannelEditorAction('confirmInlineKeyImport'),
        'close-key-export-modal': () => invokeChannelEditorAction('closeKeyExportModal'),
        'copy-export-keys': () => invokeChannelEditorAction('copyExportKeys'),
        'download-export-keys': () => invokeChannelEditorAction('downloadExportKeys'),
        'close-model-import-modal': () => invokeChannelEditorAction('closeModelImportModal'),
        'confirm-model-import': () => invokeChannelEditorAction('confirmModelImport'),
        'open-custom-rules-modal': () => invokeChannelEditorAction('openCustomRulesModal'),
        'probe-channel-websocket': (actionTarget) => detectChannelWebsocketSupport(actionTarget),
        'close-custom-rules-modal': () => invokeChannelEditorAction('closeCustomRulesModal'),
        'switch-advanced-settings-tab': (actionTarget) => invokeChannelEditorAction('switchAdvancedSettingsTab', actionTarget?.dataset?.advancedSettingsTab || ''),
        'apply-advanced-settings': () => invokeChannelEditorAction('applyAdvancedSettingsFromForm'),
        'add-custom-rule': (actionTarget) => invokeChannelEditorAction('addCustomRule', actionTarget?.dataset?.customRulesTarget || ''),
        'remove-custom-rule': (actionTarget) => invokeChannelEditorAction('removeCustomRule', actionTarget?.dataset?.customRulesTarget || '', Number(actionTarget?.dataset?.customRulesIndex || '-1')),
        'close-custom-rules-help': () => invokeChannelEditorAction('closeCustomRulesHelp'),
        'add-cooldown-detection-rule': () => invokeChannelEditorAction('addCooldownDetectionRule'),
        'remove-cooldown-detection-rule': (actionTarget) => invokeChannelEditorAction('removeCooldownDetectionRule', Number(actionTarget?.dataset?.cooldownDetectionIndex || '-1')),
        'move-cooldown-detection-rule': (actionTarget) => invokeChannelEditorAction('moveCooldownDetectionRule', Number(actionTarget?.dataset?.cooldownDetectionIndex || '-1'), Number(actionTarget?.dataset?.cooldownDetectionDirection || '0')),
        'test-cooldown-detection-rules': () => invokeChannelEditorAction('testCooldownDetectionRules')
      },
      change: {
        'toggle-select-all-urls': (actionTarget) => invokeChannelEditorAction('toggleSelectAllURLs', actionTarget.checked),
        'toggle-select-all-keys': (actionTarget) => invokeChannelEditorAction('toggleSelectAllKeys', actionTarget.checked),
        'filter-keys-by-status': (actionTarget) => invokeChannelEditorAction('filterKeysByStatus', actionTarget.value),
        'toggle-select-all-models': (actionTarget) => invokeChannelEditorAction('toggleSelectAllModels', actionTarget.checked),
        'switch-model-import-format': (actionTarget) => invokeChannelEditorAction('switchModelImportFormat', actionTarget.value),
        'update-export-preview': () => invokeChannelEditorAction('updateExportPreview')
      },
      input: {
        'filter-models-by-keyword': (actionTarget) => invokeChannelEditorAction('filterModelsByKeyword', actionTarget.value)
      }
    });
  }

  const channelForm = document.getElementById('channelForm');
  if (channelForm && !channelForm.dataset.channelFormBound) {
    channelForm.addEventListener('submit', (event) => {
      return saveChannel(event);
    });
    channelForm.dataset.channelFormBound = '1';
  }

  const scheduledCheckCheckbox = document.getElementById('channelScheduledCheckEnabled');
  if (scheduledCheckCheckbox && !scheduledCheckCheckbox.dataset.bound) {
    scheduledCheckCheckbox.addEventListener('change', () => {
      syncScheduledCheckModelState();
    });
    scheduledCheckCheckbox.dataset.bound = '1';
  }

  initCommonModelsModalEvents();
  initQuickAddChannelModalEvents();
  initModelNormalizationOptions();

  const retryOtherKeysCheckbox = document.getElementById('channelRetryOtherKeysOnFailure');
  if (retryOtherKeysCheckbox && !retryOtherKeysCheckbox.dataset.bound) {
    retryOtherKeysCheckbox.addEventListener('change', () => {
      if (typeof markChannelFormDirty === 'function') {
        markChannelFormDirty();
      }
    });
    retryOtherKeysCheckbox.dataset.bound = '1';
  }
  ensureScheduledCheckModelCombobox();
}

function resetModalKeyStatusFilter() {
  if (typeof currentKeyStatusFilter !== 'undefined') currentKeyStatusFilter = 'all';
  const filter = document.getElementById('keyStatusFilter');
  if (filter) filter.value = 'all';
}

async function showAddModal() {
  editingChannelId = null;
  editingChannelAuthType = 'api_key';
  currentChannelKeyCooldowns = [];
  resetModalKeyStatusFilter();
  await syncScheduledCheckVisibility();

  setChannelModalTitle('channels.addChannel');
  document.getElementById('channelForm').reset();
  document.getElementById('channelEnabled').checked = true;
  document.getElementById('channelScheduledCheckEnabled').checked = false;
  const retryOtherKeysCheckbox = document.getElementById('channelRetryOtherKeysOnFailure');
  if (retryOtherKeysCheckbox) retryOtherKeysCheckbox.checked = false;
  const websocketCheckbox = document.getElementById('channelWebsockets');
  if (websocketCheckbox) websocketCheckbox.checked = false;
  document.getElementById('channelScheduledCheckModel').value = '';
	await ensureProtocolTransformModeCombobox('auto');
  document.querySelector('input[name="keyStrategy"][value="sequential"]').checked = true;

  redirectTableData = [];
  selectedModelIndices.clear();
  currentModelFilter = '';
  const modelFilterInput = document.getElementById('modelFilterInput');
  if (modelFilterInput) modelFilterInput.value = '';
  renderRedirectTable();
  syncScheduledCheckModelState();

  inlineURLTableData = [emptyInlineURLConfig()];
  selectedURLIndices.clear();
  urlStatsMap = {};
  renderInlineURLTable();
  clearChannelDuplicateHint();

  inlineKeyTableData = [makeInlineKeyRow()];
  inlineKeyVisible = true;
  document.getElementById('inlineEyeIcon').style.display = 'none';
  document.getElementById('inlineEyeOffIcon').style.display = 'block';
  renderInlineKeyTable();
  if (typeof applyChannelAuthEditorMode === 'function') applyChannelAuthEditorMode(editingChannelAuthType, null);

  invokeChannelEditorAction('resetCustomRulesState', null);
  invokeChannelEditorAction('resetCooldownDetectionState', null);

  resetChannelFormDirty();
  document.getElementById('channelModal').classList.add('show');
  scheduleChannelEditorTableSizingSync();
}

async function editChannel(id) {
  let editorData;
  try {
    editorData = await fetchDataWithAuth(`/admin/channels/${id}/editor`);
  } catch (error) {
    console.error('Failed to fetch channel editor data', error);
    if (window.showError) window.showError(window.t('channels.loadChannelsFailed'));
    return;
  }

  const channel = editorData && editorData.channel;
  const apiKeys = editorData && Array.isArray(editorData.keys) ? editorData.keys : null;
  if (!channel || apiKeys === null) {
    console.error('Invalid channel editor data', editorData);
    if (window.showError) window.showError(window.t('channels.loadChannelsFailed'));
    return;
  }

  const modelStatsData = editorData.model_stats || {};
  const modelStats = modelStatsData.available === false
    ? null
    : new Map((Array.isArray(modelStatsData.items) ? modelStatsData.items : []).map(entry => [
      normalizeModelStatsKey(entry.model),
      entry
    ]));
  const urlStats = editorData.url_stats && Array.isArray(editorData.url_stats.items)
    ? editorData.url_stats.items
    : [];
  const scheduledCheckEnabled = Boolean(
    editorData.features && editorData.features.scheduled_check_enabled
  );

  resetModalKeyStatusFilter();

  const scheduledVisibilityPromise = syncScheduledCheckVisibility(scheduledCheckEnabled);
  const protocolModeRenderPromise = ensureProtocolTransformModeCombobox(channel.protocol_transform_mode);

  editingChannelId = id;
  editingChannelAuthType = ['codex_oauth', 'antigravity_oauth', 'xai_oauth', 'anthropic_oauth', 'zai_oauth', 'cursor_oauth', 'zed_oauth'].includes(channel.auth_type)
    ? channel.auth_type
    : 'api_key';
  clearChannelDuplicateHint();

  setChannelModalTitle('channels.editChannel');
  document.getElementById('channelName').value = channel.name;
  setInlineURLTableData(channel.urls);
  applyURLStats(urlStats);

  await Promise.all([
    scheduledVisibilityPromise,
    protocolModeRenderPromise
  ]);

  const now = Date.now();
  currentChannelKeyCooldowns = apiKeys.map((apiKey, index) => {
    const cooldownUntilMs = (apiKey.cooldown_until || 0) * 1000;
    const remainingMs = Math.max(0, cooldownUntilMs - now);
    return {
      key_index: Number.isInteger(apiKey.key_index) ? apiKey.key_index : index,
      cooldown_remaining_ms: remainingMs,
      disabled: Boolean(apiKey.disabled)
    };
  });

  setInlineKeyTableDataFromAPI(apiKeys);
  if (inlineKeyTableData.length === 1 && !inlineKeyTableData[0].api_key) {
    currentChannelKeyCooldowns = [];
  }

  inlineKeyVisible = true;
  document.getElementById('inlineEyeIcon').style.display = 'none';
  document.getElementById('inlineEyeOffIcon').style.display = 'block';
  renderInlineKeyTable();
  if (typeof applyChannelAuthEditorMode === 'function') {
    applyChannelAuthEditorMode(
      editingChannelAuthType,
      editorData.oauth_credential || null,
      channel,
      editorData.oauth_credential_info || null
    );
  }

  const keyStrategy = channel.key_strategy || 'sequential';
  const strategyRadio = document.querySelector(`input[name="keyStrategy"][value="${keyStrategy}"]`);
  if (strategyRadio) {
    strategyRadio.checked = true;
  }
  document.getElementById('channelPriority').value = channel.priority;
  document.getElementById('channelRPMLimit').value = channel.rpm_limit || 0;
  document.getElementById('channelMaxConcurrency').value = String(channel.max_concurrency || 0);
  document.getElementById('channelDailyCostLimit').value = channel.daily_cost_limit || 0;
  document.getElementById('channelCostMultiplier').value = (Number(channel.cost_multiplier) >= 0 ? Number(channel.cost_multiplier) : 1);
  document.getElementById('channelEnabled').checked = channel.enabled;
  const websocketCheckbox = document.getElementById('channelWebsockets');
  if (websocketCheckbox) websocketCheckbox.checked = !!channel.websockets;
  document.getElementById('channelScheduledCheckEnabled').checked = !!channel.scheduled_check_enabled;
  document.getElementById('channelScheduledCheckModel').value = channel.scheduled_check_model || '';
  const retryOtherKeysCheckbox = document.getElementById('channelRetryOtherKeysOnFailure');
  if (retryOtherKeysCheckbox) retryOtherKeysCheckbox.checked = !!channel.retry_other_keys_on_failure;

  // 加载模型配置（新格式：models是 {model, redirect_model} 数组）
  const modelCooldowns = new Map(
    (channel.model_cooldowns || []).map(cooldown => [cooldown.model, cooldown])
  );
  redirectTableData = (channel.models || []).map(m => {
    const modelName = m.model || '';
    const redirectModel = m.redirect_model || '';
    const actualModel = redirectModel || modelName;
    const cooldown = modelCooldowns.get(actualModel);
    const stats = modelStats?.get(normalizeModelStatsKey(modelName));
    return {
      model: modelName,
      redirect_model: redirectModel,
      disabled: !!m.disabled,
      cooldown_until: cooldown?.cooldown_until || '',
      cooldown_remaining_ms: cooldown?.cooldown_remaining_ms || 0,
      model_stats: stats || null,
      model_stats_unavailable: modelStats === null
    };
  });
  selectedModelIndices.clear();
  currentModelFilter = '';
  const modelFilterInput = document.getElementById('modelFilterInput');
  if (modelFilterInput) modelFilterInput.value = '';
  renderRedirectTable();
  syncScheduledCheckModelState();

  invokeChannelEditorAction('resetCustomRulesState', channel.custom_request_rules || null);
  invokeChannelEditorAction('resetCooldownDetectionState', channel.cooldown_detection_rules || null);

  const proxyUrlInput = document.getElementById('channelProxyURL');
  if (proxyUrlInput) proxyUrlInput.value = channel.proxy_url || '';
  const availableTimeStart = document.getElementById('channelAvailableTimeStart');
  const availableTimeEnd = document.getElementById('channelAvailableTimeEnd');
  if (availableTimeStart) availableTimeStart.value = channel.available_time_start || '';
  if (availableTimeEnd) availableTimeEnd.value = channel.available_time_end || '';

  resetChannelFormDirty();
  document.getElementById('channelModal').classList.add('show');
  scheduleChannelEditorTableSizingSync();
}

function normalizeModelStatsKey(modelName) {
  return String(modelName || '').trim().toLowerCase();
}

function closeModal() {
  if (channelFormDirty && !confirm(window.t('channels.unsavedChanges'))) {
    return;
  }
  document.getElementById('channelModal').classList.remove('show');
  editingChannelId = null;
  clearChannelDuplicateHint();
  resetChannelFormDirty();
}

let channelDuplicateHintTimer = null;
let channelDuplicateHintRequestSeq = 0;

async function checkChannelDuplicate(urls, options = {}) {
  try {
    const resp = await fetchAPIWithAuth('/admin/channels/check-duplicate', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ urls })
    });
    if (!resp.success) return [];
    return Array.isArray(resp.data?.duplicates) ? resp.data.duplicates : [];
  } catch (e) {
    if (!options.silent) {
      console.warn('渠道重复检测失败，已放行:', e);
    }
    return [];
  }
}

function clearChannelDuplicateHint() {
  channelDuplicateHintRequestSeq++;
  const hint = document.getElementById('channelDuplicateHint');
  if (!hint) return;
  hint.hidden = true;
  hint.textContent = '';
}

function renderChannelDuplicateHint(dupes) {
  const hint = document.getElementById('channelDuplicateHint');
  if (!hint) return;

  const names = dupes
    .map(d => (d && d.name ? d.name.trim() : ''))
    .filter(Boolean);
  if (names.length === 0) {
    clearChannelDuplicateHint();
    return;
  }

  const visibleNames = names.slice(0, 3);
  const separator = window.t('channels.duplicateChannelHintSeparator');
  const extraCount = names.length - visibleNames.length;
  const extra = extraCount > 0
    ? window.t('channels.duplicateChannelHintMore', { count: extraCount })
    : '';

  hint.textContent = window.t('channels.duplicateChannelHint', {
    list: visibleNames.join(separator),
    extra
  });
  hint.hidden = false;
}

async function refreshChannelDuplicateHint() {
  if (!document.getElementById('channelDuplicateHint')) return;

  if (editingChannelId) {
    clearChannelDuplicateHint();
    return;
  }

  const validURLConfigs = typeof getValidInlineURLConfigs === 'function' ? getValidInlineURLConfigs() : [];
  if (validURLConfigs.length === 0) {
    clearChannelDuplicateHint();
    return;
  }

  const requestSeq = ++channelDuplicateHintRequestSeq;
  const dupes = await checkChannelDuplicate(validURLConfigs, { silent: true });
  if (requestSeq !== channelDuplicateHintRequestSeq) return;
  if (dupes.length > 0) {
    renderChannelDuplicateHint(dupes);
  } else {
    clearChannelDuplicateHint();
  }
}

function scheduleChannelDuplicateHintCheck() {
  channelDuplicateHintRequestSeq++;
  if (channelDuplicateHintTimer && typeof clearTimeout === 'function') {
    clearTimeout(channelDuplicateHintTimer);
  }
  channelDuplicateHintTimer = null;

  const hint = document.getElementById('channelDuplicateHint');
  if (!hint) return;
  hint.hidden = true;
  hint.textContent = '';

  if (editingChannelId) {
    clearChannelDuplicateHint();
    return;
  }

  const validURLConfigs = typeof getValidInlineURLConfigs === 'function' ? getValidInlineURLConfigs() : [];
  if (validURLConfigs.length === 0) {
    clearChannelDuplicateHint();
    return;
  }

  const run = () => {
    channelDuplicateHintTimer = null;
    return refreshChannelDuplicateHint();
  };
  if (typeof setTimeout === 'function') {
    channelDuplicateHintTimer = setTimeout(run, 350);
  } else {
    run();
  }
}

function confirmDuplicateChannel(dupes) {
  const list = dupes.map(d => {
    const urls = (Array.isArray(d.urls) ? d.urls : [])
      .map(runtimeInlineURL)
      .filter(Boolean);
    return `• ${d.name}\n  ${urls.join('\n  ')}`;
  }).join('\n\n');
  return confirm(window.t('channels.duplicateChannelFound', { list }));
}

function setChannelSavePending(pending) {
  const saveBtn = document.getElementById('channelSaveBtn');
  if (!saveBtn) return;
  saveBtn.disabled = Boolean(pending);
}

function collectModelsForSubmit(rows) {
  return (rows || [])
    .filter(r => r.model && r.model.trim())
    .map(r => ({
      model: r.model.trim(),
      redirect_model: (r.redirect_model || '').trim(),
      disabled: !!r.disabled
    }));
}

async function saveChannel(event) {
  event.preventDefault();

  const cooldownRuleErrors = invokeChannelEditorAction('validateCooldownDetectionRulesForSubmit');
  if (Array.isArray(cooldownRuleErrors) && cooldownRuleErrors.length > 0) {
    const message = `${window.t('channels.cooldownDetection.saveIncomplete', 'Cannot save channel: complete all cooldown detection rules.')} ${cooldownRuleErrors.join(' · ')}`;
    if (window.showError) {
      window.showError(message);
    } else {
      alert(message);
    }
    return;
  }

  const validURLConfigs = getValidInlineURLConfigs();
  if (validURLConfigs.length === 0) {
    alert(window.t('channels.fillApiUrlFirst'));
    return;
  }

  const isOAuth = ['codex_oauth', 'antigravity_oauth', 'xai_oauth', 'anthropic_oauth', 'zai_oauth', 'cursor_oauth', 'zed_oauth'].includes(editingChannelAuthType);
  const validKeyRows = isOAuth ? [] : getValidInlineKeyRows();
  const validKeys = validKeyRows.map(row => row.api_key);
  if (!isOAuth && validKeyRows.length === 0) {
    alert(window.t('channels.atLeastOneKey'));
    return;
  }

  document.getElementById('channelApiKey').value = validKeys.join(',');

  // 构建模型配置（新格式：models 数组）
  const models = collectModelsForSubmit(redirectTableData);
  const seenModels = new Set();
  const duplicateModels = [];
  for (const entry of models) {
    const modelKey = entry.model.toLowerCase();
    if (seenModels.has(modelKey)) {
      duplicateModels.push(entry.model);
      continue;
    }
    seenModels.add(modelKey);
  }
  if (duplicateModels.length > 0) {
    const uniqueDuplicates = [...new Set(duplicateModels)];
    const msg = window.t('channels.duplicateModelsNotAllowed', { models: uniqueDuplicates.join(', ') });
    if (window.showError) {
      window.showError(msg);
    } else {
      alert(msg);
    }
    return;
  }

  const keyStrategy = document.querySelector('input[name="keyStrategy"]:checked')?.value || 'sequential';

  const formData = {
    name: document.getElementById('channelName').value.trim(),
    auth_type: isOAuth ? editingChannelAuthType : 'api_key',
    urls: validURLConfigs,
    api_key: validKeys.join(','),
    api_keys: validKeyRows.map(row => ({
      api_key: row.api_key,
      note: row.note || '',
      allowed_models: Array.isArray(row.allowed_models) ? [...row.allowed_models] : []
    })),
    protocol_transform_mode: getProtocolTransformMode(),
    priority: parseInt(document.getElementById('channelPriority').value) || 0,
    rpm_limit: parseInt(document.getElementById('channelRPMLimit').value) || 0,
    max_concurrency: parseInt(document.getElementById('channelMaxConcurrency').value) || 0,
    daily_cost_limit: parseFloat(document.getElementById('channelDailyCostLimit').value) || 0,
    cost_multiplier: (function () {
      const v = parseFloat(document.getElementById('channelCostMultiplier').value);
      return Number.isFinite(v) && v >= 0 ? v : 1;
    })(),
    models: models,
    enabled: document.getElementById('channelEnabled').checked,
    scheduled_check_enabled: document.getElementById('channelScheduledCheckEnabled').checked,
    websockets: !!document.getElementById('channelWebsockets')?.checked,
    scheduled_check_model: document.getElementById('channelScheduledCheckModel').value.trim(),
    custom_request_rules: invokeChannelEditorAction('collectCustomRulesForSubmit') || null,
    cooldown_detection_rules: invokeChannelEditorAction('collectCooldownDetectionRulesForSubmit') || null,
    proxy_url: (document.getElementById('channelProxyURL')?.value || '').trim(),
    available_time_start: (document.getElementById('channelAvailableTimeStart')?.value || '').trim(),
    available_time_end: (document.getElementById('channelAvailableTimeEnd')?.value || '').trim(),
    retry_other_keys_on_failure: !!document.getElementById('channelRetryOtherKeysOnFailure')?.checked
  };
  if (!isOAuth) formData.key_strategy = keyStrategy;

  if (!formData.name || formData.urls.length === 0 || (!isOAuth && !formData.api_key) || formData.models.length === 0) {
    if (window.showError) window.showError(window.t('channels.fillAllRequired'));
    return;
  }

  setChannelSavePending(true);
  try {
    if (!editingChannelId) {
      const dupes = await checkChannelDuplicate(validURLConfigs);
      if (dupes.length > 0 && !confirmDuplicateChannel(dupes)) return;
    }

    const resp = editingChannelId
      ? await fetchAPIWithAuth(`/admin/channels/${editingChannelId}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(formData)
        })
      : await fetchAPIWithAuth('/admin/channels', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(formData)
        });

    if (!resp.success) throw new Error(resp.error || window.t('channels.msg.saveFailed'));

    const isNewChannel = !editingChannelId;
    const savedChannelId = editingChannelId;

    resetChannelFormDirty(); // 保存成功，重置dirty状态（避免closeModal弹确认框）
    closeModal();
    await handleChannelSaveSuccess({ isNewChannel, savedChannelId, response: resp });
    if (window.showSuccess) window.showSuccess(isNewChannel ? window.t('channels.channelAdded') : window.t('channels.channelUpdated'));
  } catch (e) {
    console.error('Save channel failed', e);
    if (window.showError) window.showError(window.t('channels.saveFailed', { error: e.message }));
  } finally {
    setChannelSavePending(false);
  }
}

function deleteChannel(id, name) {
  deletingChannelRequest = {
    type: 'single',
    channelIDs: [id],
    url: `/admin/channels/${id}`,
    options: {
      method: 'DELETE'
    }
  };
  const messageEl = document.getElementById('deleteModalMessage');
  if (messageEl) {
    messageEl.textContent = window.t('channels.confirmDeleteNamed', { name });
  }
  document.getElementById('deleteModal').classList.add('show');
}

function closeDeleteModal() {
  document.getElementById('deleteModal').classList.remove('show');
  deletingChannelRequest = null;
}

async function confirmDelete() {
  if (!deletingChannelRequest) return;

  try {
    const { channelIDs, options, type, url } = deletingChannelRequest;
    const resp = await fetchAPIWithAuth(url, options);

    if (!resp.success) throw new Error(resp.error || window.t('common.failed'));

    closeDeleteModal();
    channelIDs.forEach((channelID) => {
      selectedChannelIds.delete(normalizeSelectedChannelID(channelID));
    });
    if (type === 'single') {
      channelsCurrentPage = 1;
    }
    if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
    await reloadChannelsList();
    if (window.showSuccess) {
      if (type === 'batch') {
        const data = resp.data || {};
        window.showSuccess(window.t('channels.batchDeleteSummary', {
          deleted: data.deleted || 0,
          notFound: data.not_found_count || 0
        }));
      } else {
        window.showSuccess(window.t('channels.channelDeleted'));
      }
    }
  } catch (e) {
    console.error('Delete channel failed', e);
    if (window.showError) {
      const errorKey = deletingChannelRequest && deletingChannelRequest.type === 'batch'
        ? 'channels.batchOperationFailed'
        : 'channels.saveFailed';
      window.showError(window.t(errorKey, { error: e.message }));
    }
  }
}

function setLocalChannelEnabled(id, enabled) {
  const channelId = Number(id);
  const previous = [];
  const seenState = new Set();
  const removedEntries = [];
  const shouldRemoveFromCurrentList = !channelEnabledMatchesCurrentStatus(enabled);
  const previousTotalCount = typeof channelsTotalCount !== 'undefined' ? channelsTotalCount : null;
  let removedFromChannels = false;

  const updateList = (list) => {
    if (!Array.isArray(list)) return;
    for (let index = list.length - 1; index >= 0; index--) {
      const channel = list[index];
      if (Number(channel && channel.id) !== channelId) continue;
      if (!seenState.has(channel)) {
        seenState.add(channel);
        previous.push({ channel, enabled: channel.enabled });
      }
      channel.enabled = enabled;
      if (shouldRemoveFromCurrentList) {
        removedEntries.push({ list, index, channel });
        if (typeof channels !== 'undefined' && list === channels) {
          removedFromChannels = true;
        }
        list.splice(index, 1);
      }
    }
  };

  if (typeof channels !== 'undefined') updateList(channels);
  if (typeof filteredChannels !== 'undefined') updateList(filteredChannels);
  syncLocalChannelPaginationAfterEnabledChange(removedFromChannels ? -1 : 0);

  return () => {
    previous.forEach((entry) => {
      entry.channel.enabled = entry.enabled;
    });
    removedEntries.slice().reverse().forEach((entry) => {
      if (entry.list.includes(entry.channel)) return;
      entry.list.splice(Math.min(entry.index, entry.list.length), 0, entry.channel);
    });
    if (previousTotalCount !== null) {
      channelsTotalCount = previousTotalCount;
      if (typeof channelsPageSize !== 'undefined') {
        channelsTotalPages = Math.max(1, Math.ceil(channelsTotalCount / channelsPageSize));
      }
    }
  };
}

function channelEnabledMatchesCurrentStatus(enabled) {
  if (typeof filters === 'undefined' || !filters || !filters.status || filters.status === 'all') {
    return true;
  }
  if (filters.status === 'enabled') return enabled === true;
  if (filters.status === 'disabled') return enabled === false;
  return true;
}

function syncLocalChannelPaginationAfterEnabledChange(delta) {
  if (!delta || typeof channelsTotalCount === 'undefined') return;
  channelsTotalCount = Math.max(0, channelsTotalCount + delta);
  if (typeof channelsPageSize !== 'undefined') {
    channelsTotalPages = Math.max(1, Math.ceil(channelsTotalCount / channelsPageSize));
    if (typeof channelsCurrentPage !== 'undefined' && channelsCurrentPage > channelsTotalPages) {
      channelsCurrentPage = channelsTotalPages;
    }
  }
}

function renderLocalChannelsAfterEnabledChange() {
  if (typeof filterChannels === 'function') {
    filterChannels();
  } else if (typeof renderChannels === 'function') {
    renderChannels();
  }
  if (typeof updateChannelsPagination === 'function') {
    updateChannelsPagination();
  }
}

async function toggleChannel(id, enabled) {
  const rollbackLocalChange = setLocalChannelEnabled(id, enabled);
  renderLocalChannelsAfterEnabledChange();

  try {
    const resp = await fetchAPIWithAuth(`/admin/channels/${id}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ enabled })
    });
    if (!resp.success) throw new Error(resp.error || window.t('common.failed'));
    if (window.showSuccess) window.showSuccess(enabled ? window.t('channels.channelEnabled') : window.t('channels.channelDisabled'));
  } catch (e) {
    rollbackLocalChange();
    renderLocalChannelsAfterEnabledChange();
    console.error('Toggle failed', e);
    if (window.showError) window.showError(window.t('common.failed'));
  }
}

function syncSelectedChannelsWithLoadedChannels() {
  const loadedIDs = new Set((channels || [])
    .map(ch => normalizeSelectedChannelID(ch.id))
    .filter(Boolean));
  let changed = false;
  selectedChannelIds.forEach((id) => {
    if (!loadedIDs.has(id)) {
      selectedChannelIds.delete(id);
      changed = true;
    }
  });
  if (changed) {
    updateBatchChannelSelectionUI();
  }
}

function getSelectedChannelIDs() {
  return Array.from(selectedChannelIds)
    .map(id => Number(id))
    .filter(id => Number.isFinite(id) && id > 0);
}

function getVisibleChannelsForSelection() {
  return Array.isArray(filteredChannels) ? filteredChannels : (channels || []);
}

function renderBatchSummary(selectedCount) {
  const marker = '__count_marker__';
  const raw = String(window.t('channels.batchSelectedCount', { count: marker }));
  const text = raw.includes(marker)
    ? raw.replace(marker, '')
    : String(window.t('channels.batchSelectedCount', { count: selectedCount }));
  const compact = text.replace(/\s+/g, ' ').trim();
  if (/[\u4e00-\u9fff]/.test(compact)) {
    return compact.replace(/\s+/g, '');
  }
  return compact;
}

function updateBatchChannelSelectionUI() {
  const selectedCount = getSelectedChannelIDs().length;
  const visibleChannels = getVisibleChannelsForSelection();
  const visibleCount = visibleChannels.length;
  let visibleSelectedCount = 0;
  visibleChannels.forEach((ch) => {
    if (selectedChannelIds.has(normalizeSelectedChannelID(ch.id))) {
      visibleSelectedCount++;
    }
  });

  const floatingMenu = document.getElementById('batchFloatingMenu');
  const batchBusy = floatingMenu?.getAttribute?.('aria-busy') === 'true';
  if (floatingMenu) {
    const visible = selectedCount > 0;
    floatingMenu.classList.toggle('is-visible', visible);
    floatingMenu.setAttribute('aria-hidden', visible ? 'false' : 'true');
    floatingMenu.inert = !visible;
    if (!visible) {
      const advancedOptions = document.getElementById('batchAdvancedOptions');
      if (advancedOptions) advancedOptions.open = false;
    }
  }

  const summary = document.getElementById('selectedChannelsSummary');
  if (summary) {
    summary.textContent = renderBatchSummary(selectedCount);
  }

  const countBadge = document.getElementById('selectedChannelsCountBadge');
  if (countBadge) {
    countBadge.textContent = String(selectedCount);
  }

  const closeBtn = document.getElementById('batchFloatingMenuCloseBtn');
  if (closeBtn) closeBtn.disabled = selectedCount === 0 || batchBusy;

  const selectionToggle = document.getElementById('visibleSelectionToggle');
  const selectionCheckbox = document.getElementById('visibleSelectionCheckbox');
  const selectionText = document.getElementById('visibleSelectionToggleText');
  const selectionI18nKey = visibleSelectedCount > 0
    ? 'channels.batchDeselectVisible'
    : 'channels.batchSelectVisible';
  const selectionLabel = window.t(selectionI18nKey);

  if (selectionText) {
    selectionText.setAttribute('data-i18n', selectionI18nKey);
    selectionText.textContent = selectionLabel;
  }
  if (selectionToggle) {
    selectionToggle.classList.toggle('is-disabled', visibleCount === 0 || batchBusy);
    selectionToggle.setAttribute('data-i18n-title', selectionI18nKey);
    selectionToggle.title = selectionLabel;
  }
  if (selectionCheckbox) {
    selectionCheckbox.disabled = visibleCount === 0 || batchBusy;
    selectionCheckbox.checked = visibleCount > 0 && visibleSelectedCount === visibleCount;
    selectionCheckbox.indeterminate = visibleSelectedCount > 0 && visibleSelectedCount < visibleCount;
  }

  const actionBtnIDs = [
    'batchEnableChannelsBtn',
    'batchDisableChannelsBtn',
    'batchExportChannelsBtn',
    'batchDeleteChannelsBtn',
    'batchRefreshOAuthUsageBtn',
    'batchRefreshMergeBtn',
    'batchRefreshReplaceBtn',
    'batchApplyProtocolBtn',
    'batchApplyPriorityBtn',
    'batchApplyCostMultiplierBtn',
    'batchApplyRPMLimitBtn',
    'batchApplyMaxConcurrencyBtn',
    'batchApplyDailyCostLimitBtn',
    'batchImportModelsBtn',
    'batchClearCooldownsBtn'
  ];
  actionBtnIDs.forEach((id) => {
    const btn = document.getElementById(id);
    if (btn) btn.disabled = selectedCount === 0 || batchBusy || btn.getAttribute?.('aria-busy') === 'true';
  });

  const protocolMode = document.getElementById('batchProtocolTransformMode');
  if (protocolMode) protocolMode.disabled = selectedCount === 0 || batchBusy;
  ['batchPriority', 'batchCostMultiplier', 'batchRPMLimit', 'batchMaxConcurrency', 'batchDailyCostLimit'].forEach((id) => {
    const input = document.getElementById(id);
    if (input) input.disabled = selectedCount === 0 || batchBusy;
  });
}

function setBatchChannelOperationBusy(busy) {
  const floatingMenu = document.getElementById('batchFloatingMenu');
  if (floatingMenu) {
    if (busy) floatingMenu.setAttribute('aria-busy', 'true');
    else floatingMenu.removeAttribute('aria-busy');
  }
  updateBatchChannelSelectionUI();
}

function selectAllVisibleChannels() {
  const visibleChannels = getVisibleChannelsForSelection();

  if (visibleChannels.length === 0) {
    return;
  }

  visibleChannels.forEach((ch) => {
    const channelID = normalizeSelectedChannelID(ch.id);
    if (channelID) {
      selectedChannelIds.add(channelID);
    }
  });
  filterChannels();
}

function toggleVisibleChannelsSelection() {
  const visibleChannels = getVisibleChannelsForSelection();

  if (visibleChannels.length === 0) {
    return;
  }

  const hasSelectedVisibleChannel = visibleChannels.some((ch) => {
    const channelID = normalizeSelectedChannelID(ch.id);
    return channelID && selectedChannelIds.has(channelID);
  });

  if (!hasSelectedVisibleChannel) {
    selectAllVisibleChannels();
    return;
  }

  deselectVisibleChannels();
}

function deselectVisibleChannels() {
  const visibleChannels = getVisibleChannelsForSelection();

  if (visibleChannels.length === 0) {
    return;
  }

  visibleChannels.forEach((ch) => {
    const channelID = normalizeSelectedChannelID(ch.id);
    if (!channelID) return;
    selectedChannelIds.delete(channelID);
  });
  filterChannels();
}

function clearSelectedChannels() {
  if (selectedChannelIds.size === 0) return;
  selectedChannelIds.clear();
  filterChannels();
}

async function batchSetSelectedChannelsEnabled(enabled) {
  const channelIDs = getSelectedChannelIDs();
  if (channelIDs.length === 0) {
    if (window.showWarning) window.showWarning(window.t('channels.batchNoSelection'));
    return;
  }

  setBatchChannelOperationBusy(true);
  try {
    const resp = await fetchAPIWithAuth('/admin/channels/batch-enabled', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ channel_ids: channelIDs, enabled })
    });
    if (!resp.success) throw new Error(resp.error || window.t('common.failed'));

    const data = resp.data || {};
    selectedChannelIds.clear();
    if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
    await reloadChannelsList();

    if (window.showSuccess) {
      window.showSuccess(window.t('channels.batchEnabledSummary', {
        action: enabled ? window.t('common.enable') : window.t('common.disable'),
        updated: data.updated || 0,
        unchanged: data.unchanged || 0,
        notFound: data.not_found_count || 0
      }));
    }
  } catch (e) {
    console.error('Batch set enabled failed', e);
    if (window.showError) window.showError(window.t('channels.batchOperationFailed', { error: e.message }));
  } finally {
    setBatchChannelOperationBusy(false);
  }
}

async function batchClearSelectedChannelCooldowns() {
  const channelIDs = getSelectedChannelIDs();
  if (channelIDs.length === 0) {
    if (window.showWarning) window.showWarning(window.t('channels.batchNoSelection'));
    return;
  }

  const clearButton = document.getElementById('batchClearCooldownsBtn');
  if (clearButton) clearButton.setAttribute('aria-busy', 'true');
  setBatchChannelOperationBusy(true);
  try {
    const resp = await fetchAPIWithAuth('/admin/channels/batch-clear-cooldowns', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ channel_ids: channelIDs })
    });
    if (!resp.success) throw new Error(resp.error || window.t('common.failed'));

    const data = resp.data || {};
    await finishBatchAdvancedUpdate();
    if (window.showSuccess) {
      window.showSuccess(window.t('channels.batchClearCooldownsSummary', {
        cleared: data.cleared || 0,
        notFound: data.not_found_count || 0
      }));
    }
  } catch (e) {
    console.error('Batch clear cooldowns failed', e);
    if (window.showError) window.showError(window.t('channels.batchOperationFailed', { error: e.message }));
  } finally {
    if (clearButton) clearButton.removeAttribute('aria-busy');
    setBatchChannelOperationBusy(false);
  }
}

async function requestBatchAdvancedPatch(patch) {
  const channelIDs = getSelectedChannelIDs();
  if (channelIDs.length === 0) {
    if (window.showWarning) window.showWarning(window.t('channels.batchNoSelection'));
    return null;
  }

  const resp = await fetchAPIWithAuth('/admin/channels/batch-advanced', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ channel_ids: channelIDs, ...patch })
  });
  if (!resp.success) throw new Error(resp.error || window.t('common.failed'));
  return resp.data || {};
}

async function finishBatchAdvancedUpdate() {
  selectedChannelIds.clear();
  if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
  await reloadChannelsList();
}

async function batchSetSelectedChannelsProtocolMode() {
  if (getSelectedChannelIDs().length === 0) {
    if (window.showWarning) window.showWarning(window.t('channels.batchNoSelection'));
    return;
  }

  const modeSelect = document.getElementById('batchProtocolTransformMode');
  const mode = String(modeSelect?.value || '').trim();
  if (!['auto', 'upstream', 'local'].includes(mode)) {
    if (window.showError) window.showError(window.t('channels.batchProtocolModeInvalid'));
    return;
  }

  const applyButton = document.getElementById('batchApplyProtocolBtn');
  if (applyButton) applyButton.setAttribute('aria-busy', 'true');
  setBatchChannelOperationBusy(true);

  try {
    const data = await requestBatchAdvancedPatch({ protocol_transform_mode: mode });
    if (!data) return;
    await finishBatchAdvancedUpdate();

    if (window.showSuccess) {
      window.showSuccess(window.t('channels.batchProtocolModeSummary', {
        mode: window.t(`channels.batchProtocolModeValue.${mode}`),
        updated: data.updated || 0,
        unchanged: data.unchanged || 0,
        notFound: data.not_found_count || 0
      }));
    }
  } catch (e) {
    console.error('Batch set protocol transform mode failed', e);
    if (window.showError) window.showError(window.t('channels.batchOperationFailed', { error: e.message }));
  } finally {
    if (applyButton) applyButton.removeAttribute('aria-busy');
    setBatchChannelOperationBusy(false);
  }
}

const batchNumericSettingConfigs = {
  priority: {
    inputID: 'batchPriority',
    buttonID: 'batchApplyPriorityBtn',
    errorID: 'batchPriorityError',
    invalidKey: 'channels.batchPriorityInvalid',
    summaryKey: 'channels.batchPrioritySummary',
    summaryValueName: 'value',
    integer: true,
    min: -99999,
    max: 9999999
  },
  cost_multiplier: {
    inputID: 'batchCostMultiplier',
    buttonID: 'batchApplyCostMultiplierBtn',
    errorID: 'batchCostMultiplierError',
    invalidKey: 'channels.batchCostMultiplierInvalid',
    summaryKey: 'channels.batchCostMultiplierSummary',
    summaryValueName: 'multiplier',
    integer: false
  },
  rpm_limit: {
    inputID: 'batchRPMLimit',
    buttonID: 'batchApplyRPMLimitBtn',
    errorID: 'batchRPMLimitError',
    invalidKey: 'channels.batchRPMLimitInvalid',
    summaryKey: 'channels.batchRPMLimitSummary',
    summaryValueName: 'value',
    integer: true
  },
  max_concurrency: {
    inputID: 'batchMaxConcurrency',
    buttonID: 'batchApplyMaxConcurrencyBtn',
    errorID: 'batchMaxConcurrencyError',
    invalidKey: 'channels.batchMaxConcurrencyInvalid',
    summaryKey: 'channels.batchMaxConcurrencySummary',
    summaryValueName: 'value',
    integer: true
  },
  daily_cost_limit: {
    inputID: 'batchDailyCostLimit',
    buttonID: 'batchApplyDailyCostLimitBtn',
    errorID: 'batchDailyCostLimitError',
    invalidKey: 'channels.batchDailyCostLimitInvalid',
    summaryKey: 'channels.batchDailyCostLimitSummary',
    summaryValueName: 'value',
    integer: false
  }
};

async function batchSetSelectedChannelsNumericSetting(fieldName) {
  if (getSelectedChannelIDs().length === 0) {
    if (window.showWarning) window.showWarning(window.t('channels.batchNoSelection'));
    return;
  }

  const config = batchNumericSettingConfigs[fieldName];
  if (!config) return;
  const input = document.getElementById(config.inputID);
  const applyButton = document.getElementById(config.buttonID);
  const error = document.getElementById(config.errorID);
  const rawValue = String(input?.value ?? '').trim();
  const value = Number(rawValue);
  const min = config.min ?? 0;
  const max = config.max ?? Number.POSITIVE_INFINITY;
  if (rawValue === '' || !Number.isFinite(value) || value < min || value > max || (config.integer && !Number.isInteger(value))) {
    const message = window.t(config.invalidKey);
    if (input) {
      input.setAttribute('aria-invalid', 'true');
      input.focus();
    }
    if (error) {
      error.textContent = message;
      error.hidden = false;
    }
    return;
  }
  if (error) {
    error.textContent = '';
    error.hidden = true;
  }
  if (input) {
    input.setAttribute('aria-invalid', 'false');
  }
  if (applyButton) applyButton.setAttribute('aria-busy', 'true');
  setBatchChannelOperationBusy(true);

  try {
    const data = await requestBatchAdvancedPatch({ [fieldName]: value });
    if (!data) return;
    await finishBatchAdvancedUpdate();
    if (window.showSuccess) {
      const params = {
        updated: data.updated || 0,
        unchanged: data.unchanged || 0,
        notFound: data.not_found_count || 0
      };
      params[config.summaryValueName] = value;
      window.showSuccess(window.t(config.summaryKey, params));
    }
  } catch (e) {
    console.error(`Batch set ${fieldName} failed`, e);
    if (window.showError) window.showError(window.t('channels.batchOperationFailed', { error: e.message }));
  } finally {
    if (applyButton) applyButton.removeAttribute('aria-busy');
    setBatchChannelOperationBusy(false);
  }
}

async function batchSetSelectedChannelsCostMultiplier() {
  return batchSetSelectedChannelsNumericSetting('cost_multiplier');
}

async function batchSetSelectedChannelsPriority() {
  return batchSetSelectedChannelsNumericSetting('priority');
}

async function batchSetSelectedChannelsRPMLimit() {
  return batchSetSelectedChannelsNumericSetting('rpm_limit');
}

async function batchSetSelectedChannelsMaxConcurrency() {
  return batchSetSelectedChannelsNumericSetting('max_concurrency');
}

async function batchSetSelectedChannelsDailyCostLimit() {
  return batchSetSelectedChannelsNumericSetting('daily_cost_limit');
}

function batchDeleteSelectedChannels() {
  const channelIDs = getSelectedChannelIDs();
  if (channelIDs.length === 0) {
    if (window.showWarning) window.showWarning(window.t('channels.batchNoSelection'));
    return;
  }

  deletingChannelRequest = {
    type: 'batch',
    channelIDs,
    url: '/admin/channels/batch-delete',
    options: {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ channel_ids: channelIDs })
    }
  };
  const messageEl = document.getElementById('deleteModalMessage');
  if (messageEl) {
    messageEl.textContent = window.t('channels.confirmBatchDeleteMsg', { count: channelIDs.length });
  }
  document.getElementById('deleteModal').classList.add('show');
}

function summarizeBatchRefreshError(error) {
  const fallback = window.t('common.failed');
  const text = String(error || fallback).replace(/\s+/g, ' ').trim() || fallback;
  return text.length > 120 ? `${text.slice(0, 117)}...` : text;
}

function buildBatchRefreshFailureDetail(name, error) {
  const errorText = String(error || window.t('common.failed'));
  return [
    `${window.t('common.name')}: ${name}`,
    `${window.t('common.status')}: ${window.t('channels.batchRefreshStatus.failed')}`,
    `${window.t('channels.batchRefreshErrorReason')}:`,
    errorText
  ].join('\n');
}

function buildBatchRefreshResultForItem(channelID, name, item, mode) {
  const status = item && (item.status === 'updated' || item.status === 'unchanged')
    ? item.status
    : 'failed';

  if (status === 'failed') {
    const error = item && item.error ? item.error : window.t('common.failed');
    return {
      status,
      mode,
      summary: summarizeBatchRefreshError(error),
      detail: buildBatchRefreshFailureDetail(name, error)
    };
  }

  return {
    status,
    mode,
    fetched: Number(item.fetched) || 0,
    added: Number(item.added) || 0,
    removed: Number(item.removed) || 0,
    total: Number(item.total) || 0
  };
}

function setBatchRefreshRowResult(channelID, result) {
  if (typeof setBatchRefreshResult === 'function') {
    setBatchRefreshResult(channelID, result);
  }
}

const MODEL_NORMALIZATION_OPTIONS_STORAGE_KEY = 'channels.modelNormalizationOptions';
const MODEL_NORMALIZATION_OPTION_INPUT_IDS = {
  lowercase_models: [
    'batchRefreshLowercaseModels',
    'quickAddLowercaseModels',
    'modelImportLowercaseModels'
  ],
  strip_model_source_prefix: [
    'batchRefreshStripModelSourcePrefix',
    'quickAddStripModelSourcePrefix',
    'modelImportStripModelSourcePrefix'
  ]
};

function getModelNormalizationOptionInputs() {
  return Object.fromEntries(
    Object.entries(MODEL_NORMALIZATION_OPTION_INPUT_IDS).map(([option, ids]) => [
      option,
      ids.map(id => document.getElementById(id)).filter(Boolean)
    ])
  );
}

function resolveModelNormalizationOptionsStorage(storage) {
  if (storage) return storage;
  try {
    return window.localStorage;
  } catch (_) {
    return null;
  }
}

function readModelNormalizationOptions(storage) {
  const fallback = { lowercase_models: false, strip_model_source_prefix: false };
  const target = resolveModelNormalizationOptionsStorage(storage);
  if (!target) return fallback;

  try {
    const saved = JSON.parse(target.getItem(MODEL_NORMALIZATION_OPTIONS_STORAGE_KEY));
    return {
      lowercase_models: saved?.lowercase_models === true,
      strip_model_source_prefix: saved?.strip_model_source_prefix === true
    };
  } catch (_) {
    return fallback;
  }
}

function applyModelNormalizationOptions(options) {
  const inputs = getModelNormalizationOptionInputs();
  Object.entries(inputs).forEach(([option, optionInputs]) => {
    optionInputs.forEach(input => { input.checked = options[option] === true; });
  });
  return options;
}

function saveModelNormalizationOptions(options, storage) {
  const normalized = {
    lowercase_models: options?.lowercase_models === true,
    strip_model_source_prefix: options?.strip_model_source_prefix === true
  };
  const target = resolveModelNormalizationOptionsStorage(storage);
  if (!target) return normalized;

  try {
    target.setItem(MODEL_NORMALIZATION_OPTIONS_STORAGE_KEY, JSON.stringify(normalized));
  } catch (_) { /* 浏览器禁用本地存储时保留当前页面状态 */ }
  return normalized;
}

function syncModelNormalizationOptions(storage) {
  return applyModelNormalizationOptions(readModelNormalizationOptions(storage));
}

function initModelNormalizationOptions(storage) {
  const options = syncModelNormalizationOptions(storage);
  const inputs = getModelNormalizationOptionInputs();

  Object.entries(inputs).forEach(([option, optionInputs]) => {
    optionInputs.forEach(input => {
      if (input.dataset.modelNormalizationOptionsBound === '1') return;
      input.addEventListener('change', () => {
        const next = readModelNormalizationOptions(storage);
        next[option] = input.checked === true;
        applyModelNormalizationOptions(saveModelNormalizationOptions(next, storage));
        updateModelImportPreview();
      });
      input.dataset.modelNormalizationOptionsBound = '1';
    });
  });
  return options;
}

function modelNormalizationOptionsForRequest(storage) {
  const options = readModelNormalizationOptions(storage);
  return {
    lowercaseModels: options.lowercase_models,
    stripModelSourcePrefix: options.strip_model_source_prefix
  };
}

async function batchRefreshSelectedChannels(mode) {
  const channelIDs = getSelectedChannelIDs();
  if (channelIDs.length === 0) {
    if (window.showWarning) window.showWarning(window.t('channels.batchNoSelection'));
    return;
  }

  if (mode === 'replace' && !confirm(window.t('channels.batchRefreshReplaceConfirm', { count: channelIDs.length }))) {
    return;
  }

  const normalizationOptions = readModelNormalizationOptions();
  const lowercaseModels = normalizationOptions.lowercase_models;
  const stripModelSourcePrefix = normalizationOptions.strip_model_source_prefix;

  if (typeof clearAllBatchRefreshResults === 'function') {
    clearAllBatchRefreshResults();
  }

  setBatchChannelOperationBusy(true);
  try {

  const total = channelIDs.length;
  const modeLabel = mode === 'replace' ? window.t('channels.batchModeReplace') : window.t('channels.batchModeMerge');

  // 创建持久化进度通知
  const progressEl = document.createElement('div');
  progressEl.style.cssText = [
    'background: var(--glass-bg)', 'backdrop-filter: blur(16px)',
    'border: 1px solid var(--info-300)', 'border-radius: var(--radius-lg)',
    'padding: var(--space-4) var(--space-6)', 'color: var(--neutral-900)',
    'font-weight: var(--font-medium)', 'max-width: 420px',
    'box-shadow: 0 10px 25px rgba(0,0,0,0.12)', 'pointer-events: auto',
    'opacity: 0', 'transform: translateX(20px)',
    'transition: all var(--duration-normal) var(--timing-function)'
  ].join(';');

  const titleSpan = document.createElement('div');
  titleSpan.style.marginBottom = 'var(--space-2)';
  titleSpan.textContent = window.t('channels.batchRefreshProgress', { current: 0, total, mode: modeLabel });
  progressEl.appendChild(titleSpan);

  const barOuter = document.createElement('div');
  barOuter.style.cssText = 'height:4px;background:var(--neutral-200);border-radius:2px;overflow:hidden;margin-bottom:var(--space-2)';
  const barInner = document.createElement('div');
  barInner.style.cssText = 'height:100%;width:0%;background:var(--primary-500);border-radius:2px;transition:width 0.3s ease';
  barOuter.appendChild(barInner);
  progressEl.appendChild(barOuter);

  const detailSpan = document.createElement('div');
  detailSpan.style.cssText = 'font-size:0.85em;color:var(--neutral-600)';
  progressEl.appendChild(detailSpan);

  const host = typeof window.ensureNotifyHost === 'function'
    ? window.ensureNotifyHost()
    : document.body;
  host.appendChild(progressEl);
  requestAnimationFrame(() => { progressEl.style.opacity = '1'; progressEl.style.transform = 'translateX(0)'; });

  let updated = 0, unchanged = 0, failed = 0;

  for (let i = 0; i < channelIDs.length; i++) {
    const channelID = channelIDs[i];
    const info = channels.find(c => c.id === channelID);
    const name = info ? info.name : `#${channelID}`;

    titleSpan.textContent = window.t('channels.batchRefreshProgress', { current: i, total, mode: modeLabel });
    detailSpan.textContent = window.t('channels.batchRefreshCurrent', { name });
    barInner.style.width = `${(i / total * 100).toFixed(0)}%`;
    setBatchRefreshRowResult(channelID, { status: 'processing', mode });

    try {
      const resp = await fetchAPIWithAuth('/admin/channels/models/refresh-batch', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          channel_ids: [channelID],
          mode,
          lowercase_models: lowercaseModels,
          strip_model_source_prefix: stripModelSourcePrefix
        })
      });

      if (!resp.success) throw new Error(resp.error || window.t('common.failed'));

      const item = ((resp.data || {}).results || [])[0] || {};
      const rowResult = buildBatchRefreshResultForItem(channelID, name, item, mode);
      if (item.status === 'updated') {
        updated++;
      } else if (item.status === 'unchanged') {
        unchanged++;
      } else {
        failed++;
      }
      setBatchRefreshRowResult(channelID, rowResult);
    } catch (e) {
      failed++;
      const errorMessage = e && e.message ? e.message : window.t('common.failed');
      setBatchRefreshRowResult(channelID, {
        status: 'failed',
        mode,
        summary: summarizeBatchRefreshError(errorMessage),
        detail: buildBatchRefreshFailureDetail(name, errorMessage)
      });
    }

    detailSpan.textContent = window.t('channels.batchRefreshCounts', { updated, unchanged, failed });
  }

  // 完成：更新进度条到100%
  barInner.style.width = '100%';
  titleSpan.textContent = window.t('channels.batchRefreshSummary', { mode: modeLabel, updated, unchanged, failed });

  if (failed > 0) {
    progressEl.style.borderColor = 'var(--error-300)';
    detailSpan.textContent = window.t('channels.batchRefreshInlineFailedHint', { failed });
  } else {
    progressEl.style.borderColor = 'var(--success-400)';
    detailSpan.textContent = '';
  }

  // 关闭动画辅助函数
  function dismissProgress() {
    progressEl.style.opacity = '0';
    progressEl.style.transform = 'translateX(20px)';
    setTimeout(() => { if (progressEl.parentNode) progressEl.parentNode.removeChild(progressEl); }, 320);
  }

  // 操作按钮栏：复制 + 关闭
  const actionBar = document.createElement('div');
  actionBar.style.cssText = 'display:flex;justify-content:flex-end;gap:var(--space-2);margin-top:var(--space-3)';

  const closeBtn = document.createElement('button');
  closeBtn.textContent = '✕';
  closeBtn.style.cssText = 'padding:2px 8px;font-size:0.9em;border:1px solid var(--neutral-300);border-radius:var(--radius-md);background:var(--neutral-50);color:var(--neutral-700);cursor:pointer;font-weight:bold';
  closeBtn.onclick = dismissProgress;
  actionBar.appendChild(closeBtn);

  progressEl.appendChild(actionBar);

  setTimeout(dismissProgress, 10000);

  selectedChannelIds.clear();
  if (typeof saveChannelsFilters === 'function') saveChannelsFilters();
  await reloadChannelsList();
  updateBatchChannelSelectionUI();
  } finally {
    setBatchChannelOperationBusy(false);
  }
}

function batchEnableSelectedChannels() {
  return batchSetSelectedChannelsEnabled(true);
}

function batchDisableSelectedChannels() {
  return batchSetSelectedChannelsEnabled(false);
}

function batchRefreshSelectedChannelsMerge() {
  return batchRefreshSelectedChannels('merge');
}

function batchRefreshSelectedChannelsReplace() {
  return batchRefreshSelectedChannels('replace');
}

async function copyChannel(id, name) {
  const channel = channels.find(c => c.id === id);
  if (!channel) return;
  await syncScheduledCheckVisibility();

  const copiedName = generateCopyName(name);

  editingChannelId = null;
  clearChannelDuplicateHint();
  currentChannelKeyCooldowns = [];
  resetModalKeyStatusFilter();
  setChannelModalTitle('channels.copyChannel');
  document.getElementById('channelName').value = copiedName;
  setInlineURLTableData(channel.urls);

  let apiKeys = [];
  try {
    apiKeys = (await fetchDataWithAuth(`/admin/channels/${id}/keys`)) || [];
  } catch (e) {
    console.error('Failed to fetch API Keys', e);
  }

  setInlineKeyTableDataFromAPI(apiKeys);

  inlineKeyVisible = true;
  document.getElementById('inlineEyeIcon').style.display = 'none';
  document.getElementById('inlineEyeOffIcon').style.display = 'block';
  renderInlineKeyTable();

  await ensureProtocolTransformModeCombobox(channel.protocol_transform_mode);
  scheduleChannelDuplicateHintCheck();
  const keyStrategy = channel.key_strategy || 'sequential';
  const strategyRadio = document.querySelector(`input[name="keyStrategy"][value="${keyStrategy}"]`);
  if (strategyRadio) {
    strategyRadio.checked = true;
  }
  document.getElementById('channelPriority').value = channel.priority;
  document.getElementById('channelRPMLimit').value = channel.rpm_limit || 0;
  document.getElementById('channelMaxConcurrency').value = String(channel.max_concurrency || 0);
  document.getElementById('channelDailyCostLimit').value = channel.daily_cost_limit || 0;
  document.getElementById('channelCostMultiplier').value = (Number(channel.cost_multiplier) >= 0 ? Number(channel.cost_multiplier) : 1);
  document.getElementById('channelEnabled').checked = true;
  const websocketCheckbox = document.getElementById('channelWebsockets');
  if (websocketCheckbox) websocketCheckbox.checked = !!channel.websockets;
  document.getElementById('channelScheduledCheckEnabled').checked = !!channel.scheduled_check_enabled;
  document.getElementById('channelScheduledCheckModel').value = channel.scheduled_check_model || '';
  const proxyUrlInput = document.getElementById('channelProxyURL');
  if (proxyUrlInput) proxyUrlInput.value = channel.proxy_url || '';
  const availableTimeStart = document.getElementById('channelAvailableTimeStart');
  const availableTimeEnd = document.getElementById('channelAvailableTimeEnd');
  if (availableTimeStart) availableTimeStart.value = channel.available_time_start || '';
  if (availableTimeEnd) availableTimeEnd.value = channel.available_time_end || '';
  const retryOtherKeysCheckbox = document.getElementById('channelRetryOtherKeysOnFailure');
  if (retryOtherKeysCheckbox) retryOtherKeysCheckbox.checked = !!channel.retry_other_keys_on_failure;

  // 加载模型配置（新格式：models是 {model, redirect_model} 数组）
  redirectTableData = (channel.models || []).map(m => ({
    model: m.model || '',
    redirect_model: m.redirect_model || '',
    disabled: !!m.disabled
  }));
  selectedModelIndices.clear();
  currentModelFilter = '';
  const modelFilterInput = document.getElementById('modelFilterInput');
  if (modelFilterInput) modelFilterInput.value = '';
  renderRedirectTable();
  syncScheduledCheckModelState();

  resetChannelFormDirty();
  document.getElementById('channelModal').classList.add('show');
  scheduleChannelEditorTableSizingSync();
}

function generateCopyName(originalName) {
  const suffix = window.t('channels.copySuffix');
  // 匹配带有 " - 复制" 或 " - Copy" 后缀的名称
  const copyPattern = new RegExp(`^(.+?)(?:\\s*-\\s*${suffix}(?:\\s*(\\d+))?)?$`);
  const match = originalName.match(copyPattern);

  if (!match) {
    return originalName + ' - ' + suffix;
  }

  const baseName = match[1];
  const copyNumber = match[2] ? parseInt(match[2]) + 1 : 1;

  const proposedName = copyNumber === 1 ? `${baseName} - ${suffix}` : `${baseName} - ${suffix} ${copyNumber}`;

  const existingNames = channels.map(c => c.name.toLowerCase());
  if (existingNames.includes(proposedName.toLowerCase())) {
    return generateCopyName(proposedName);
  }

  return proposedName;
}

function getModelImportFormat() {
  return document.querySelector('input[name="modelImportFormat"]:checked')?.value === 'json'
    ? 'json'
    : 'text';
}

function parseModels(input, format = getModelImportFormat()) {
  const entries = format === 'json'
    ? window.ModelEntryParser.parseJSONModelEntries(input)
    : window.ModelEntryParser.parseModelEntries(input);
  const options = readModelNormalizationOptions();
  if (!options.lowercase_models && !options.strip_model_source_prefix) return entries;
  return window.ModelEntryParser.normalizeModelEntries(entries, options);
}

function setModelImportError(message = '') {
  const textarea = document.getElementById('modelImportTextarea');
  const error = document.getElementById('modelImportError');
  const hasError = Boolean(message);
  if (textarea) textarea.setAttribute('aria-invalid', hasError ? 'true' : 'false');
  if (!error) return;
  error.textContent = message;
  error.hidden = !hasError;
}

function modelImportErrorMessage(error) {
  const item = Number.isInteger(error?.index) ? error.index + 1 : 0;
  const keys = {
    invalid_json: 'channels.modelImportJsonInvalid',
    array_required: 'channels.modelImportJsonArrayRequired',
    invalid_entry: 'channels.modelImportJsonEntryInvalid',
    model_required: 'channels.modelImportJsonModelRequired',
    gateway_model_required: 'channels.modelImportJsonGatewayModelRequired',
    invalid_model: 'channels.modelImportJsonModelInvalid',
    invalid_redirect_model: 'channels.modelImportJsonRedirectInvalid',
    invalid_disabled: 'channels.modelImportJsonDisabledInvalid'
  };
  const key = keys[error?.code];
  return key
    ? window.t(key, { item })
    : window.t('channels.modelImportJsonInvalid');
}

function switchModelImportFormat(format) {
  const normalizedFormat = format === 'json' ? 'json' : 'text';
  const textarea = document.getElementById('modelImportTextarea');
  const label = document.getElementById('modelImportInputLabel');
  const hint = document.getElementById('modelImportInputHint');
  const textHelp = document.getElementById('modelImportTextHelp');
  const jsonHelp = document.getElementById('modelImportJSONHelp');
  const formatInput = document.querySelector(`input[name="modelImportFormat"][value="${normalizedFormat}"]`);

  if (formatInput) formatInput.checked = true;
  if (label) {
    const key = normalizedFormat === 'json' ? 'channels.inputModelJSON' : 'channels.inputModelNames';
    label.setAttribute('data-i18n', key);
    label.textContent = window.t(key);
  }
  if (hint) {
    const key = normalizedFormat === 'json' ? 'channels.modelJSONHint' : 'channels.modelSeparatorHint';
    hint.setAttribute('data-i18n', key);
    hint.textContent = window.t(key);
  }
  if (textarea) {
    const key = normalizedFormat === 'json'
      ? 'channels.modelImportJSONPlaceholder'
      : 'channels.modelImportPlaceholder';
    textarea.setAttribute('data-i18n-placeholder', key);
    textarea.placeholder = window.t(key);
  }
  if (textHelp) textHelp.hidden = normalizedFormat === 'json';
  if (jsonHelp) jsonHelp.hidden = normalizedFormat !== 'json';

  setModelImportError();
  updateModelImportPreview();
}

function addRedirectRow() {
  openModelImportModal();
}

function setModelImportText(element, key) {
  if (!element) return;
  element.setAttribute('data-i18n', key);
  element.textContent = window.t(key);
}

function openModelImportModal(target = 'channel') {
  modelImportTarget = target === 'batch' ? 'batch' : 'channel';
  modelImportTrigger = document.activeElement;
  syncModelNormalizationOptions();
  document.getElementById('modelImportTextarea').value = '';
  document.getElementById('modelImportPreviewContent').classList.add('hidden');
  const batchImport = modelImportTarget === 'batch';
  const modeFieldset = document.getElementById('modelImportModeFieldset');
  if (modeFieldset) modeFieldset.hidden = !batchImport;
  const appendMode = document.querySelector('input[name="modelImportMode"][value="append"]');
  if (appendMode) appendMode.checked = true;
  setModelImportText(document.getElementById('modelImportTitle'), batchImport
    ? 'channels.batchImportModelsTitle'
    : 'channels.importModelsTitle');
  setModelImportText(document.getElementById('modelImportPreviewLabel'), batchImport
    ? 'channels.batchParseSuccessModel'
    : 'channels.parseSuccessModel');
  setModelImportText(document.getElementById('modelImportConfirmBtn'), batchImport
    ? 'channels.batchImportModelsConfirm'
    : 'channels.confirmAdd');
  switchModelImportFormat('text');
  const modal = document.getElementById('modelImportModal');
  if (batchImport) {
    document.querySelector('.app-container')?.setAttribute('inert', '');
  } else {
    document.getElementById('channelModal')?.setAttribute('inert', '');
  }
  modal.classList.add('show');
  modal.setAttribute('aria-hidden', 'false');
  setTimeout(() => document.getElementById('modelImportTextarea').focus(), 100);
}

function closeModelImportModal(restoreFocus = true) {
  const modal = document.getElementById('modelImportModal');
  modal.classList.remove('show');
  modal.setAttribute('aria-hidden', 'true');
  document.getElementById('channelModal')?.removeAttribute('inert');
  document.querySelector('.app-container')?.removeAttribute('inert');
  if (restoreFocus) modelImportTrigger?.focus?.();
  modelImportTrigger = null;
  modelImportTarget = 'channel';
}

function openBatchModelImportModal() {
  if (getSelectedChannelIDs().length === 0) {
    if (window.showWarning) window.showWarning(window.t('channels.batchNoSelection'));
    return;
  }
  openModelImportModal('batch');
}

function updateModelImportPreview() {
  const textarea = document.getElementById('modelImportTextarea');
  if (!textarea) return;

  const input = textarea.value.trim();
  const previewContent = document.getElementById('modelImportPreviewContent');
  const countSpan = document.getElementById('modelImportCount');

  setModelImportError();
  if (input) {
    try {
      const models = parseModels(input);
      if (models.length > 0) {
        countSpan.textContent = models.length;
        previewContent.classList.remove('hidden');
      } else {
        previewContent.classList.add('hidden');
      }
    } catch (_) {
      previewContent.classList.add('hidden');
    }
  } else {
    previewContent.classList.add('hidden');
  }
}

function setupModelImportPreview() {
  const textarea = document.getElementById('modelImportTextarea');
  if (!textarea || textarea.dataset.modelImportPreviewBound === '1') return;

  textarea.addEventListener('input', updateModelImportPreview);
  textarea.dataset.modelImportPreviewBound = '1';

  const modal = document.getElementById('modelImportModal');
  if (!modal || modal.dataset.modelImportEventsBound === '1') return;
  modal.addEventListener('click', event => {
    if (event.target === modal) closeModelImportModal();
  });
  document.addEventListener('keydown', event => {
    if (!modal.classList.contains('show')) return;
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      closeModelImportModal();
      return;
    }
    if (event.key !== 'Tab') return;

    const focusable = Array.from(modal.querySelectorAll(
      'button:not([disabled]), input:not([disabled]), textarea:not([disabled])'
    )).filter(element => !element.closest('[hidden]'));
    if (focusable.length === 0) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  }, true);
  modal.dataset.modelImportEventsBound = '1';
}

async function confirmModelImport() {
  const textarea = document.getElementById('modelImportTextarea');
  const input = textarea.value.trim();

  if (!input) {
    setModelImportError(window.t('channels.modelImportInputRequired'));
    textarea.focus();
    return;
  }

  let newModels;
  try {
    newModels = parseModels(input);
    setModelImportError();
  } catch (error) {
    setModelImportError(modelImportErrorMessage(error));
    textarea.focus();
    return;
  }
  if (newModels.length === 0) {
    setModelImportError(window.t('channels.noValidModelParsed'));
    textarea.focus();
    return;
  }

  if (modelImportTarget === 'batch') {
    const mode = document.querySelector('input[name="modelImportMode"]:checked')?.value === 'replace'
      ? 'replace'
      : 'append';
    const channelCount = getSelectedChannelIDs().length;
    if (mode === 'replace' && !confirm(window.t('channels.batchModelImportReplaceConfirm', { count: channelCount }))) {
      return;
    }

    const confirmButton = document.getElementById('modelImportConfirmBtn');
    if (confirmButton) confirmButton.disabled = true;
    textarea.disabled = true;
    try {
      const data = await requestBatchAdvancedPatch({
        model_import_mode: mode,
        models: newModels
      });
      if (!data) return;
      closeModelImportModal(false);
      await finishBatchAdvancedUpdate();
      document.getElementById('visibleSelectionCheckbox')?.focus();
      if (window.showSuccess) {
        window.showSuccess(window.t('channels.batchModelImportSummary', {
          mode: window.t(`channels.batchModelImportModeValue.${mode}`),
          updated: data.updated || 0,
          unchanged: data.unchanged || 0,
          notFound: data.not_found_count || 0
        }));
      }
    } catch (error) {
      console.error('Batch import models failed', error);
      setModelImportError(window.t('channels.batchOperationFailed', { error: error.message }));
    } finally {
      textarea.disabled = false;
      if (confirmButton) confirmButton.disabled = false;
      if (document.getElementById('modelImportModal')?.classList.contains('show')) textarea.focus();
      updateBatchChannelSelectionUI();
    }
    return;
  }

  // 获取现有模型名称用于去重（忽略大小写）
  const existingModels = new Set(
    redirectTableData
      .map(r => (r.model || '').trim().toLowerCase())
      .filter(Boolean)
  );
  let addedCount = 0;

  newModels.forEach(entry => {
    const modelKey = entry.model.toLowerCase();
    if (!existingModels.has(modelKey)) {
      redirectTableData.push({
        model: entry.model,
        redirect_model: entry.redirect_model,
        disabled: !!entry.disabled
      });
      existingModels.add(modelKey);
      addedCount++;
    }
  });

  renderRedirectTable();
  closeModelImportModal();
  if (addedCount > 0) markChannelFormDirty();

  if (addedCount > 0) {
    const duplicateCount = newModels.length - addedCount;
    const msg = duplicateCount > 0
      ? window.t('channels.modelAddedWithDuplicates', { added: addedCount, duplicates: duplicateCount })
      : window.t('channels.modelAddedSuccess', { added: addedCount });
    window.showNotification(msg, 'success');
  } else {
    window.showNotification(window.t('channels.allModelsExist'), 'info');
  }
}

function exportChannelModels() {
  const models = collectModelsForSubmit(redirectTableData);
  const text = window.ModelEntryParser.serializeModelEntries(models);
  if (!text) {
    if (window.showWarning) window.showWarning(window.t('channels.noModelsToExport'));
    return;
  }

  const blob = new Blob([text], { type: 'text/plain;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const link = document.createElement('a');
  link.href = url;
  link.download = 'channel-models.txt';
  document.body.appendChild(link);
  link.click();
  document.body.removeChild(link);
  URL.revokeObjectURL(url);
  if (window.showSuccess) window.showSuccess(window.t('channels.modelsExported', { count: models.length }));
}

function deleteRedirectRow(index) {
  redirectTableData.splice(index, 1);
  // 更新选中状态：删除该索引，并调整后续索引
  const newSelectedIndices = new Set();
  selectedModelIndices.forEach(i => {
    if (i < index) {
      newSelectedIndices.add(i);
    } else if (i > index) {
      newSelectedIndices.add(i - 1);
    }
  });
  selectedModelIndices.clear();
  newSelectedIndices.forEach(i => selectedModelIndices.add(i));
  renderRedirectTable();
  markChannelFormDirty();
}

function updateRedirectRow(index, field, value) {
  if (redirectTableData[index]) {
    const nextValue = value.trim();
    if (redirectTableData[index][field] === nextValue) return;

    redirectTableData[index][field] = nextValue;

    // 用户改动模型配置后，原运行态已不再对应当前行。
    redirectTableData[index].cooldown_until = '';
    redirectTableData[index].cooldown_remaining_ms = 0;
    if (field === 'model') {
      redirectTableData[index].model_stats = null;
      redirectTableData[index].model_stats_unavailable = false;
    }

    // 当模型名称变化时，更新重定向目标的 placeholder
    const tbody = document.getElementById('redirectTableBody');
    const row = tbody?.children[index];
    if (field === 'model' && row) {
      const toInput = row.querySelector('.redirect-to-input');
      if (toInput) {
        toInput.placeholder = nextValue || window.t('channels.leaveEmptyNoRedirect');
      }
    }
    if (row) {
      const statusCell = row.querySelector('.redirect-col-status');
      if (statusCell) {
        renderRedirectModelStatus(statusCell, redirectTableData[index]);
      }
      configureRedirectModelActions(row, redirectTableData[index], index);
    }

    markChannelFormDirty();
  }
}

function toggleModelDisabledState(rows, index) {
  const row = rows?.[index];
  if (!row) return false;
  row.disabled = !row.disabled;
  return true;
}

function toggleRedirectModelDisabled(index) {
  if (!toggleModelDisabledState(redirectTableData, index)) return;
  renderRedirectTable();
  document.querySelector(`#redirectTableBody .redirect-model-toggle-btn[data-index="${index}"]`)?.focus();
  markChannelFormDirty();
}

async function testRedirectModel(index, button) {
  const redirect = redirectTableData[index];
  const modelName = String(redirect?.model || '').trim();
  if (!redirect || !modelName) {
    if (window.showWarning) window.showWarning(window.t('channels.modelNameRequiredForTest'));
    return false;
  }
  if (!editingChannelId || channelFormDirty) {
    if (window.showWarning) window.showWarning(window.t('channels.saveBeforeModelTest'));
    return false;
  }
  if (redirect.disabled) {
    if (window.showWarning) window.showWarning(window.t('channels.enableBeforeModelTest'));
    return false;
  }

  const channel = channels.find(item => item.id === editingChannelId);
  if (!channel) {
    if (window.showError) window.showError(window.t('channels.test.channelNotFound'));
    return false;
  }

  if (button) {
    button.disabled = true;
    button.setAttribute('aria-busy', 'true');
  }
  try {
    const opened = await testChannel(channel.id, channel.name, modelName);
    if (!opened) return false;
    await runChannelTest();
    return true;
  } catch (error) {
    console.error('Model test failed', error);
    if (window.showError) {
      window.showError(window.t('channels.test.requestFailed') + error.message);
    }
    return false;
  } finally {
    if (button?.isConnected) {
      const currentRedirect = redirectTableData[index];
      button.disabled = Boolean(currentRedirect?.disabled) || !String(currentRedirect?.model || '').trim();
      button.removeAttribute('aria-busy');
    }
  }
}

/**
 * 使用模板引擎创建重定向行元素
 * @param {Object} redirect - 重定向数据
 * @param {number} index - 索引
 * @returns {HTMLElement|null} 表格行元素
 */
function createRedirectRow(redirect, index) {
  const modelName = redirect.model || '';
  const rowData = {
    index: index,
    displayIndex: index + 1,
    from: modelName,
    to: redirect.redirect_model || '',
    toPlaceholder: modelName || window.t('channels.leaveEmptyNoRedirect'),
    mobileLabelModel: window.t('channels.modal.modelName'),
    mobileLabelTarget: window.t('channels.modal.redirectTarget'),
    mobileLabelStatus: window.t('common.status')
  };

  const row = TemplateEngine.render('tpl-redirect-row', rowData);
  if (!row) {
    console.error('[Channels] Template tpl-redirect-row not found');
    return null;
  }

  // 设置复选框选中状态
  const checkbox = row.querySelector('.model-checkbox');
  if (checkbox) {
    checkbox.checked = selectedModelIndices.has(index);
  }

  const statusCell = row.querySelector('.redirect-col-status');
  if (statusCell) {
    renderRedirectModelStatus(statusCell, redirect);
  }
  configureRedirectModelActions(row, redirect, index);

  return row;
}

function renderRedirectModelStatus(statusCell, redirect) {
  statusCell.replaceChildren();

  if (redirect.disabled) {
    appendRedirectModelStatus(statusCell, 'disabled', [window.t('channels.modelStatusDisabled')]);
  } else {
    renderActiveRedirectModelStatus(statusCell, redirect);
  }
}

function configureRedirectModelActions(row, redirect, index) {
  const toggleButton = row.querySelector('.redirect-model-toggle-btn');
  if (!toggleButton) return;
  toggleButton.dataset.index = String(index);
  const toggleTitle = redirect.disabled
    ? window.t('channels.enableThisModel')
    : window.t('channels.disableThisModel');
  toggleButton.title = toggleTitle;
  toggleButton.setAttribute('aria-label', toggleTitle);
  toggleButton.innerHTML = redirect.disabled
    ? '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><circle cx="12" cy="12" r="10"/><line x1="4.93" y1="4.93" x2="19.07" y2="19.07"/></svg>'
    : '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M22 11.08V12a10 10 0 1 1-5.93-9.14"/><polyline points="22 4 12 14.01 9 11.01"/></svg>';

  const testButton = row.querySelector('.redirect-model-test-btn');
  if (!testButton) return;
  const modelName = String(redirect.model || '').trim();
  const testTitle = redirect.disabled
    ? window.t('channels.enableBeforeModelTest')
    : modelName
      ? window.t('channels.testThisModel')
      : window.t('channels.modelNameRequiredForTest');
  testButton.dataset.index = String(index);
  testButton.title = testTitle;
  testButton.setAttribute('aria-label', testTitle);
  testButton.disabled = redirect.disabled || !modelName;
}

function renderActiveRedirectModelStatus(statusCell, redirect) {
  const cooldownUntilMS = Date.parse(redirect.cooldown_until || '');
  const responseRemainingMS = Number(redirect.cooldown_remaining_ms || 0);
  const cooldownRemainingMS = Number.isFinite(cooldownUntilMS)
    ? Math.max(0, cooldownUntilMS - Date.now())
    : Math.max(0, responseRemainingMS);
  if (cooldownRemainingMS > 0) {
    const badge = TemplateEngine.render('tpl-cooldown-badge', {
      text: humanizeMS(cooldownRemainingMS)
    });
    if (badge) {
      badge.classList.add('redirect-model-cooldown-badge');
      statusCell.appendChild(badge);
    }
    return;
  }
  renderRedirectModelStats(statusCell, redirect);
}

function formatModelStatsSeconds(value) {
  const seconds = Number(value);
  if (!Number.isFinite(seconds) || seconds <= 0) return '—';
  return `${seconds.toFixed(2).replace(/\.00$/, '').replace(/(\.\d)0$/, '$1')}s`;
}

function appendRedirectModelStatus(statusCell, modifier, lines) {
  const status = document.createElement('div');
  status.className = `redirect-model-status redirect-model-status--${modifier}`;
  for (const text of lines) {
    const line = document.createElement('span');
    line.textContent = text;
    status.appendChild(line);
  }
  statusCell.appendChild(status);
}

function appendRedirectModelPerformance(statusCell, stats) {
  const status = document.createElement('div');
  status.className = 'redirect-model-status redirect-model-status--performance';

  // 第一行：首字/耗时 2s/14.4s
  status.appendChild(buildRedirectModelPerformanceLine(
    window.t('channels.modelTiming'),
    [
      {
        text: formatModelStatsSeconds(stats.avg_first_byte_time_seconds),
        color: window.getFirstByteTimingColor(stats.avg_first_byte_time_seconds)
      },
      {
        text: formatModelStatsSeconds(stats.avg_duration_seconds),
        color: window.getDurationTimingColor(stats.avg_duration_seconds)
      }
    ]
  ));

  // 第二行：调用/失败 成功次数/失败次数
  status.appendChild(buildRedirectModelPerformanceLine(
    window.t('channels.modelCallFail'),
    [
      { text: formatModelStatsCount(stats.success), color: 'var(--success-600)' },
      { text: formatModelStatsCount(stats.error), color: 'var(--error-600)' }
    ]
  ));

  statusCell.appendChild(status);
}

function buildRedirectModelPerformanceLine(labelText, values) {
  const line = document.createElement('span');
  line.className = 'redirect-model-performance-line';

  const label = document.createElement('span');
  label.className = 'redirect-model-performance-label';
  label.textContent = labelText;
  line.appendChild(label);

  values.forEach((item, index) => {
    if (index > 0) {
      const sep = document.createElement('span');
      sep.className = 'redirect-model-performance-sep';
      sep.textContent = '/';
      line.appendChild(sep);
    }
    const value = document.createElement('span');
    value.className = 'redirect-model-performance-value';
    value.textContent = item.text;
    if (item.color) {
      value.style.color = item.color;
    }
    line.appendChild(value);
  });

  return line;
}

function formatModelStatsCount(value) {
  const count = Number(value);
  if (!Number.isFinite(count) || count < 0) return '—';
  return String(Math.trunc(count));
}

function renderRedirectModelStats(statusCell, redirect) {
  const stats = redirect.model_stats;
  if (Number(stats?.success) > 0) {
    appendRedirectModelPerformance(statusCell, stats);
    return;
  }

  appendRedirectModelStatus(statusCell, 'empty', [
    window.t(redirect.model_stats_unavailable
      ? 'channels.modelStatsUnavailable'
      : 'channels.modelNoSamples')
  ]);
}

/**
 * 初始化重定向表格事件委托 (替代inline onchange/onclick)
 */
function initRedirectTableEventDelegation() {
  const tbody = document.getElementById('redirectTableBody');
  if (!tbody || tbody.dataset.delegated) return;

  tbody.dataset.delegated = 'true';

  // 处理输入框变更
  tbody.addEventListener('change', (e) => {
    const checkbox = e.target.closest('.model-checkbox');
    if (checkbox) {
      const index = parseInt(checkbox.dataset.index, 10);
      toggleModelSelection(index, checkbox.checked);
      return;
    }

    const fromInput = e.target.closest('.redirect-from-input');
    if (fromInput) {
      const index = parseInt(fromInput.dataset.index, 10);
      updateRedirectRow(index, 'model', fromInput.value);
      return;
    }

    const toInput = e.target.closest('.redirect-to-input');
    if (toInput) {
      const index = parseInt(toInput.dataset.index, 10);
      updateRedirectRow(index, 'redirect_model', toInput.value);
    }
  });

  // 处理模型操作按钮点击
  tbody.addEventListener('click', (e) => {
    const toggleBtn = e.target.closest('.redirect-model-toggle-btn');
    if (toggleBtn) {
      const index = parseInt(toggleBtn.dataset.index, 10);
      toggleRedirectModelDisabled(index);
      return;
    }

    const testBtn = e.target.closest('.redirect-model-test-btn');
    if (testBtn) {
      const index = parseInt(testBtn.dataset.index, 10);
      void testRedirectModel(index, testBtn);
      return;
    }

    const deleteBtn = e.target.closest('.redirect-delete-btn');
    if (deleteBtn) {
      const index = parseInt(deleteBtn.dataset.index, 10);
      deleteRedirectRow(index);
      return;
    }

    const lowercaseBtn = e.target.closest('.lowercase-btn');
    if (lowercaseBtn) {
      const index = parseInt(lowercaseBtn.dataset.index, 10);
      const row = lowercaseBtn.closest('tr');
      const fromInput = row?.querySelector('.redirect-from-input');
      if (fromInput && fromInput.value) {
        const lowercased = fromInput.value.toLowerCase();
        fromInput.value = lowercased;
        updateRedirectRow(index, 'model', lowercased);
      }
    }
  });
}

/**
 * 获取筛选后的模型索引列表
 */
function getVisibleModelIndices() {
  if (!currentModelFilter) {
    return redirectTableData.map((_, index) => index);
  }
  const keyword = currentModelFilter.toLowerCase();
  return redirectTableData
    .map((item, index) => {
      const model = (item.model || '').toLowerCase();
      const redirect = (item.redirect_model || '').toLowerCase();
      if (model.includes(keyword) || redirect.includes(keyword)) {
        return index;
      }
      return null;
    })
    .filter(index => index !== null);
}

/**
 * 按关键字筛选模型
 */
function filterModelsByKeyword(keyword) {
  currentModelFilter = (keyword || '').trim();
  renderRedirectTable();
}

function renderRedirectTable() {
  const tbody = document.getElementById('redirectTableBody');
  const countSpan = document.getElementById('redirectCount');

  // 计数所有有效模型（只要有模型名称就算）
  const validCount = redirectTableData.filter(r => r.model && r.model.trim()).length;
  countSpan.textContent = validCount;
  syncScheduledCheckModelState();

  // 初始化事件委托（仅一次）
  initRedirectTableEventDelegation();

  if (redirectTableData.length === 0) {
    const emptyRow = TemplateEngine.render('tpl-redirect-empty', {
      message: window.t('channels.noModelConfig')
    });
    if (emptyRow) {
      tbody.innerHTML = '';
      tbody.appendChild(emptyRow);
    } else {
      // 降级：模板不存在时使用简单HTML
      tbody.innerHTML = `<tr><td colspan="4" style="padding: 20px; text-align: center; color: var(--neutral-500);">${window.t('channels.noModelConfig')}</td></tr>`;
    }
    syncChannelEditorTableSizing();
    return;
  }

  // 获取筛选后的索引
  const visibleIndices = getVisibleModelIndices();

  if (visibleIndices.length === 0) {
    tbody.innerHTML = `<tr><td colspan="4" style="padding: 20px; text-align: center; color: var(--neutral-500);">${window.t('channels.noMatchingModels')}</td></tr>`;
    syncChannelEditorTableSizing();
    return;
  }

  // 使用DocumentFragment优化批量DOM操作
  const fragment = document.createDocumentFragment();
  visibleIndices.forEach(index => {
    const row = createRedirectRow(redirectTableData[index], index);
    if (row) fragment.appendChild(row);
  });

  tbody.innerHTML = '';
  tbody.appendChild(fragment);
  syncChannelEditorTableSizing();

  // 更新全选复选框和批量删除按钮状态
  updateSelectAllModelsCheckbox();
  updateModelBatchDeleteButton();

  // Translate dynamically rendered elements
  if (window.i18n && window.i18n.translatePage) {
    window.i18n.translatePage();
  }
}

// ===== 模型多选删除相关函数 =====

/**
 * 切换单个模型的选中状态
 */
function toggleModelSelection(index, checked) {
  if (checked) {
    selectedModelIndices.add(index);
  } else {
    selectedModelIndices.delete(index);
  }
  updateModelBatchDeleteButton();
  updateSelectAllModelsCheckbox();
}

/**
 * 全选/取消全选模型（仅操作当前可见的模型）
 */
function toggleSelectAllModels(checked) {
  const visibleIndices = getVisibleModelIndices();

  if (checked) {
    visibleIndices.forEach(index => selectedModelIndices.add(index));
  } else {
    visibleIndices.forEach(index => selectedModelIndices.delete(index));
  }

  updateModelBatchDeleteButton();
  renderRedirectTable();
}

/**
 * 更新批量删除按钮状态
 */
function updateModelBatchDeleteButton() {
  const deleteBtn = document.getElementById('batchDeleteModelsBtn');
  const lowercaseBtn = document.getElementById('batchLowercaseModelsBtn');
  const stripPrefixBtn = document.getElementById('batchStripModelSourcePrefixBtn');
  const count = selectedModelIndices.size;

  // 更新删除按钮
  if (deleteBtn) {
    const textSpan = deleteBtn.querySelector('span');
    if (count > 0) {
      deleteBtn.disabled = false;
      if (textSpan) textSpan.textContent = window.t('channels.deleteSelectedCount', { count });
      deleteBtn.style.cursor = 'pointer';
      deleteBtn.style.opacity = '1';
      deleteBtn.style.background = 'linear-gradient(135deg, #fef2f2 0%, #fecaca 100%)';
      deleteBtn.style.borderColor = '#fca5a5';
      deleteBtn.style.color = '#dc2626';
    } else {
      deleteBtn.disabled = true;
      if (textSpan) textSpan.textContent = window.t('channels.deleteSelected');
      deleteBtn.style.cursor = '';
      deleteBtn.style.opacity = '0.5';
      deleteBtn.style.background = '';
      deleteBtn.style.borderColor = '';
      deleteBtn.style.color = '';
    }
  }

  [
    [lowercaseBtn, 'channels.lowercaseSelected', 'channels.lowercaseSelectedCount'],
    [stripPrefixBtn, 'channels.stripSourcePrefixSelected', 'channels.stripSourcePrefixSelectedCount']
  ].forEach(([button, emptyLabel, countLabel]) => {
    if (!button) return;
    const textSpan = button.querySelector('span');
    button.disabled = count === 0;
    if (textSpan) {
      textSpan.textContent = count > 0 ? window.t(countLabel, { count }) : window.t(emptyLabel);
    }
    button.style.cursor = count > 0 ? 'pointer' : '';
    button.style.opacity = count > 0 ? '1' : '0.5';
    button.style.background = count > 0 ? 'linear-gradient(135deg, #eff6ff 0%, #bfdbfe 100%)' : '';
    button.style.borderColor = count > 0 ? '#93c5fd' : '';
    button.style.color = count > 0 ? '#2563eb' : '';
  });
}

function normalizeSelectedModels(options) {
  const count = selectedModelIndices.size;
  if (count === 0) return;

  let changedCount = 0;
  selectedModelIndices.forEach(index => {
    const current = redirectTableData[index];
    if (!current) return;
    const normalized = window.ModelEntryParser.normalizeModelEntry(current, options);
    if (!normalized) return;
    if (current.model === normalized.model && current.redirect_model === normalized.redirect_model) return;
    redirectTableData[index] = normalized;
    changedCount++;
  });

  selectedModelIndices.clear();
  updateModelBatchDeleteButton();
  renderRedirectTable();
  if (changedCount > 0) markChannelFormDirty();
}

function batchLowercaseSelectedModels() {
  normalizeSelectedModels({ lowercase_models: true });
}

function batchStripSelectedModelSourcePrefixes() {
  normalizeSelectedModels({ strip_model_source_prefix: true });
}

/**
 * 更新全选复选框状态（基于当前可见的模型）
 */
function updateSelectAllModelsCheckbox() {
  const checkbox = document.getElementById('selectAllModels');
  if (!checkbox) return;

  const visibleIndices = getVisibleModelIndices();
  const visibleCount = visibleIndices.length;
  const selectedVisibleCount = visibleIndices.filter(i => selectedModelIndices.has(i)).length;

  if (visibleCount === 0) {
    checkbox.checked = false;
    checkbox.indeterminate = false;
  } else if (selectedVisibleCount === visibleCount) {
    checkbox.checked = true;
    checkbox.indeterminate = false;
  } else if (selectedVisibleCount > 0) {
    checkbox.checked = false;
    checkbox.indeterminate = true;
  } else {
    checkbox.checked = false;
    checkbox.indeterminate = false;
  }
}

/**
 * 批量删除选中的模型
 */
function batchDeleteSelectedModels() {
  const count = selectedModelIndices.size;
  if (count === 0) return;

  if (!confirm(window.t('channels.confirmBatchDeleteModels', { count }))) {
    return;
  }

  const tableContainer = document.querySelector('#redirectTableBody').closest('.inline-table-container');
  const scrollTop = tableContainer ? tableContainer.scrollTop : 0;

  // 从大到小排序，确保删除时索引不会错位
  const indicesToDelete = Array.from(selectedModelIndices).sort((a, b) => b - a);

  indicesToDelete.forEach(index => {
    redirectTableData.splice(index, 1);
  });

  selectedModelIndices.clear();
  updateModelBatchDeleteButton();

  renderRedirectTable();
  markChannelFormDirty();

  setTimeout(() => {
    if (tableContainer) {
      tableContainer.scrollTop = Math.min(scrollTop, tableContainer.scrollHeight - tableContainer.clientHeight);
    }
  }, 50);
}

function mergeModelRowsWithFetchedModels(currentRows, fetchedModels) {
  const existingModelKeys = new Set();
  const occupiedModelKeys = new Set();
  const rows = [];
  (currentRows || []).forEach(row => {
    const model = (row?.model || '').trim();
    if (!model) return;
    const modelKey = model.toLowerCase();
    if (existingModelKeys.has(modelKey)) return;
    existingModelKeys.add(modelKey);
    occupiedModelKeys.add(modelKey);
    const redirectModel = (row?.redirect_model || '').trim();
    if (redirectModel) occupiedModelKeys.add(redirectModel.toLowerCase());
    rows.push({
      model,
      redirect_model: redirectModel,
      disabled: !!row?.disabled
    });
  });

  let added = 0;
  for (const entry of fetchedModels || []) {
    const modelName = (typeof entry === 'string' ? entry : entry?.model || '').trim();
    if (!modelName) continue;

    const modelKey = modelName.toLowerCase();
    const fetchedRedirect = (typeof entry === 'object' && entry?.redirect_model)
      ? String(entry.redirect_model).trim()
      : modelName;
    const redirectKey = fetchedRedirect.toLowerCase();
    if (occupiedModelKeys.has(modelKey) || occupiedModelKeys.has(redirectKey)) continue;
    occupiedModelKeys.add(modelKey);
    occupiedModelKeys.add(redirectKey);
    rows.push({
      model: modelName,
      redirect_model: fetchedRedirect,
      disabled: false
    });
    added++;
  }

  rows.sort((a, b) => a.model.localeCompare(b.model));

  return { rows, added, removed: 0 };
}

function areModelRowsEqual(left, right) {
  if ((left || []).length !== (right || []).length) return false;
  return (left || []).every((row, index) => {
    const other = right[index] || {};
    return (row.model || '') === (other.model || '') &&
      (row.redirect_model || '') === (other.redirect_model || '') &&
      !!row.disabled === !!other.disabled;
  });
}

function normalizeDetectedModelNames(entries) {
  const seen = new Set();
  const names = [];
  for (const entry of Array.isArray(entries) ? entries : []) {
    for (const value of [entry?.model, entry?.redirect_model]) {
      const name = String(value || '').trim();
      const key = name.toLowerCase();
      if (!name || seen.has(key)) continue;
      seen.add(key);
      names.push(name);
    }
  }
  return names;
}

function detectedChannelModels(modelRows, entries) {
  const detectedNames = normalizeDetectedModelNames(entries);
  const detected = new Set(detectedNames.map(name => name.toLowerCase()));
  const matched = [];
  const seen = new Set();
  for (const row of Array.isArray(modelRows) ? modelRows : []) {
    const logicalModel = String(row?.model || '').trim();
    if (!logicalModel || logicalModel === '*') continue;
    const upstreamModel = String(row?.redirect_model || logicalModel).trim();
    if (!detected.has(logicalModel.toLowerCase()) && !detected.has(upstreamModel.toLowerCase())) continue;
    const key = logicalModel.toLowerCase();
    if (seen.has(key)) continue;
    seen.add(key);
    matched.push(logicalModel);
  }
  const wildcard = (Array.isArray(modelRows) ? modelRows : [])
    .some(row => String(row?.model || '').trim() === '*');
  return matched.length > 0 || !wildcard ? matched : detectedNames;
}

function proposeFetchedKeyModelScopes(keyRows, modelRows, keyModels, requestEntries) {
  const originalRows = (Array.isArray(keyRows) ? keyRows : []).map(row => ({
    ...row,
    allowed_models: Array.isArray(row?.allowed_models) ? [...row.allowed_models] : []
  }));
  const rows = originalRows.map(row => ({ ...row, allowed_models: [...row.allowed_models] }));
  const results = new Map(
    (Array.isArray(keyModels) ? keyModels : [])
      .filter(Boolean)
      .map(result => [Number(result.key_index), result])
  );
  let changedCount = 0;
  let matchedCount = 0;
  let unmatchedCount = 0;
  for (const [requestIndex, requestEntry] of (Array.isArray(requestEntries) ? requestEntries : []).entries()) {
    const result = results.get(requestIndex);
    if (!result || result.error || !Array.isArray(result.models) || result.models.length === 0) continue;
    const rowIndex = Number(requestEntry?.keyIndex);
    const row = rows[rowIndex];
    if (!row || String(row.api_key || '').trim() !== String(requestEntry?.apiKey || '').trim()) continue;
    const allowedModels = detectedChannelModels(modelRows, result.models);
    if (allowedModels.length === 0) {
      unmatchedCount++;
      continue;
    }
    matchedCount++;
    const current = (row.allowed_models || []).map(name => String(name).toLowerCase());
    const next = allowedModels.map(name => name.toLowerCase());
    if (current.length !== next.length || current.some((name, index) => name !== next[index])) {
      row.allowed_models = allowedModels;
      changedCount++;
    }
  }
  const complete = matchedCount === (Array.isArray(requestEntries) ? requestEntries.length : 0);
  return {
    rows: complete ? rows : originalRows,
    changedCount: complete ? changedCount : 0,
    matchedCount,
    unmatchedCount,
    complete
  };
}

function quickAddFieldKind(name) {
  const normalized = String(name || '').trim().toLowerCase();
  const compact = normalized.replace(/[^a-z0-9密钥]/g, '');
  if (compact.includes('url') || compact.includes('endpoint') || compact.endsWith('apibase')) {
    return 'url';
  }
  if (compact.includes('apikey') || compact === 'key' || compact.endsWith('key') ||
      compact.includes('token') || compact.includes('secret') || compact.includes('密钥')) {
    return 'apiKey';
  }
  return '';
}

function cleanQuickAddValue(value) {
  let cleaned = String(value || '').trim();
  cleaned = cleaned.replace(/^[`'\"]+/, '').replace(/[`'\";,]+$/, '').trim();
  return cleaned;
}

function collectQuickAddJSONFields(value, fields) {
  if (!value || typeof value !== 'object') return;
  for (const [name, child] of Object.entries(value)) {
    const kind = quickAddFieldKind(name);
    if (kind && (typeof child === 'string' || typeof child === 'number')) {
      const cleaned = cleanQuickAddValue(child);
      if (cleaned) fields[kind].push(cleaned);
    }
    if (child && typeof child === 'object') collectQuickAddJSONFields(child, fields);
  }
}

function normalizeQuickAddURL(value) {
  const candidate = cleanQuickAddValue(value);
  let parsed;
  try {
    parsed = new URL(candidate);
  } catch (_error) {
    const error = new Error('invalid URL');
    error.code = 'url_invalid';
    throw error;
  }

  if (!['http:', 'https:'].includes(parsed.protocol) || !parsed.host ||
      parsed.username || parsed.password || parsed.search || parsed.hash) {
    const error = new Error('invalid URL');
    error.code = 'url_invalid';
    throw error;
  }

  let path = parsed.pathname.replace(/\/+$/, '');
  const versionPathIndex = path.toLowerCase().indexOf('/v1');
  if (versionPathIndex >= 0) path = path.slice(0, versionPathIndex);
  return `${parsed.protocol}//${parsed.host}${path}`;
}

function parseQuickAddChannelInfo(input) {
  const text = String(input || '').trim();
  if (!text) {
    const error = new Error('connection info is required');
    error.code = 'input_required';
    throw error;
  }

  const fields = { url: [], apiKey: [] };
  try {
    collectQuickAddJSONFields(JSON.parse(text), fields);
  } catch (_error) {
    // 普通文本不是 JSON；继续按环境变量、标签文本解析。
  }

  for (const line of text.split(/\r?\n/)) {
    const assignment = line.match(/^\s*(?:export\s+|set\s+)?([^:=]+?)\s*[:=]\s*(.+?)\s*$/i);
    if (!assignment) continue;
    const kind = quickAddFieldKind(assignment[1]);
    if (!kind) continue;
    const value = cleanQuickAddValue(assignment[2]);
    if (value) fields[kind].push(value);
  }

  const urlMatch = text.match(/https?:\/\/[^\s\"'`<>\uff0c\uff1b]+/i);
  if (urlMatch) fields.url.unshift(cleanQuickAddValue(urlMatch[0].replace(/[).]+$/, '')));

  if (fields.apiKey.length === 0) {
    const labeledKey = text.match(/(?:api[\s_-]*key|token|secret|\u5bc6\u94a5)\s*[:=]\s*(?:\"([^\"]+)\"|'([^']+)'|([^\s,;\]}]+))/i);
    const bearerKey = text.match(/\bbearer\s+([^\s\"',;]+)/i);
    const key = labeledKey
      ? (labeledKey[1] || labeledKey[2] || labeledKey[3])
      : bearerKey?.[1];
    if (key) fields.apiKey.push(cleanQuickAddValue(key));
  }

  if (fields.apiKey.length === 0) {
    const bareCandidates = text.split(/\r?\n/)
      .map(cleanQuickAddValue)
      .filter(value => value && !value.includes('://') && !/\s/.test(value) && value.length >= 8);
    if (bareCandidates.length === 1) fields.apiKey.push(bareCandidates[0]);
  }

  if (fields.url.length === 0) {
    const error = new Error('URL not found');
    error.code = 'url_missing';
    throw error;
  }
  if (fields.apiKey.length === 0) {
    const error = new Error('API key not found');
    error.code = 'key_missing';
    throw error;
  }

  return {
    url: normalizeQuickAddURL(fields.url[0]),
    apiKey: fields.apiKey[0]
  };
}

async function discoverQuickAddChannelSetup(input, request = fetchAPIWithAuth, options = {}) {
  const parsed = parseQuickAddChannelInfo(input);
  const lowercaseModels = options?.lowercaseModels === true;
  const stripModelSourcePrefix = options?.stripModelSourcePrefix === true;
  const failures = [];
  let response;
  for (const protocol of ['openai', 'anthropic']) {
    try {
      const candidate = await request('/admin/channels/models/fetch', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          urls: [{ url: parsed.url, exact: false, protocols: [] }],
          protocol,
          api_keys: [parsed.apiKey],
          lowercase_models: lowercaseModels,
          strip_model_source_prefix: stripModelSourcePrefix
        })
      });
      if (candidate?.success) {
        response = candidate;
        break;
      }
      failures.push(`${protocol}: ${candidate?.error || 'model discovery failed'}`);
    } catch (error) {
      failures.push(`${protocol}: ${error?.message || 'model discovery failed'}`);
    }
  }
  if (!response) throw new Error(failures.join('; '));

  const replacement = mergeModelRowsWithFetchedModels([], response.data?.models || []);
  if (replacement.rows.length === 0) {
    const error = new Error('model list is empty');
    error.code = 'models_missing';
    throw error;
  }

  return {
    // Models API 成功不能证明聊天请求协议；保持自动能力探测。
    url: { url: parsed.url, exact: false, protocols: [] },
    key: { api_key: parsed.apiKey, note: '' },
    models: replacement.rows
  };
}

function applyQuickAddChannelSetup(setup) {
  const channelNameInput = document.getElementById('channelName');
  if (channelNameInput && !channelNameInput.value.trim()) {
    channelNameInput.value = new URL(setup.url.url).host;
  }

  setInlineURLTableData([setup.url]);
  setInlineKeyTableDataFromAPI([setup.key]);
  currentChannelKeyCooldowns = [];
  selectedKeyIndices.clear();
  renderInlineKeyTable();

  redirectTableData = setup.models.map(row => ({
    model: row.model || '',
    redirect_model: row.redirect_model || '',
    disabled: !!row.disabled
  }));
  selectedModelIndices.clear();
  updateModelBatchDeleteButton();
  renderRedirectTable();
  syncScheduledCheckModelState();
  markChannelFormDirty();
  scheduleChannelEditorTableSizingSync();
}

function setQuickAddChannelError(message = '') {
  const input = document.getElementById('quickAddChannelInput');
  const error = document.getElementById('quickAddChannelError');
  if (!input || !error) return;

  const hasError = Boolean(message);
  input.setAttribute('aria-invalid', hasError ? 'true' : 'false');
  error.textContent = message;
  error.hidden = !hasError;
}

function setQuickAddChannelBusy(busy) {
  const input = document.getElementById('quickAddChannelInput');
  const lowercaseModels = document.getElementById('quickAddLowercaseModels');
  const stripModelSourcePrefix = document.getElementById('quickAddStripModelSourcePrefix');
  const button = document.getElementById('quickAddChannelConfirmBtn');
  const status = document.getElementById('quickAddChannelStatus');
  if (input) input.disabled = busy;
  if (lowercaseModels) lowercaseModels.disabled = busy;
  if (stripModelSourcePrefix) stripModelSourcePrefix.disabled = busy;
  if (button) {
    button.disabled = busy;
    if (busy) button.setAttribute('aria-busy', 'true');
    else button.removeAttribute('aria-busy');
  }
  if (status) status.textContent = busy ? window.t('channels.quickAdd.checking') : '';
}

function quickAddChannelErrorMessage(error) {
  const keys = {
    input_required: 'channels.quickAdd.inputRequired',
    url_missing: 'channels.quickAdd.urlMissing',
    key_missing: 'channels.quickAdd.keyMissing',
    url_invalid: 'channels.quickAdd.urlInvalid',
    models_missing: 'channels.quickAdd.modelsMissing'
  };
  if (error?.code && keys[error.code]) return window.t(keys[error.code]);
  return window.t('channels.quickAdd.failed', { error: error?.message || window.t('common.unknown') });
}

function openQuickAddChannelModal(trigger) {
  const modal = document.getElementById('quickAddChannelModal');
  const input = document.getElementById('quickAddChannelInput');
  if (!modal || !input) return false;

  quickAddChannelRequestVersion++;
  quickAddChannelTrigger = trigger || document.activeElement;
  syncModelNormalizationOptions();
  input.value = '';
  setQuickAddChannelError();
  setQuickAddChannelBusy(false);
  document.getElementById('channelModal')?.setAttribute('inert', '');
  modal.classList.add('show');
  modal.setAttribute('aria-hidden', 'false');
  input.focus();
  return true;
}

function closeQuickAddChannelModal() {
  const modal = document.getElementById('quickAddChannelModal');
  if (!modal) return;

  quickAddChannelRequestVersion++;
  setQuickAddChannelBusy(false);
  modal.classList.remove('show');
  modal.setAttribute('aria-hidden', 'true');
  document.getElementById('channelModal')?.removeAttribute('inert');
  quickAddChannelTrigger?.focus?.();
  quickAddChannelTrigger = null;
}

async function confirmQuickAddChannel() {
  const input = document.getElementById('quickAddChannelInput');
  if (!input || input.disabled) return false;

  const requestVersion = ++quickAddChannelRequestVersion;
  setQuickAddChannelError();
  setQuickAddChannelBusy(true);
  try {
    const setup = await discoverQuickAddChannelSetup(
      input.value,
      fetchAPIWithAuth,
      modelNormalizationOptionsForRequest()
    );
    if (requestVersion !== quickAddChannelRequestVersion) return false;

    applyQuickAddChannelSetup(setup);
    const modelCount = setup.models.length;
    closeQuickAddChannelModal();
    if (window.showSuccess) {
      window.showSuccess(window.t('channels.quickAdd.success', { count: modelCount }));
    }
    return true;
  } catch (error) {
    if (requestVersion !== quickAddChannelRequestVersion) return false;
    setQuickAddChannelBusy(false);
    setQuickAddChannelError(quickAddChannelErrorMessage(error));
    input.focus();
    return false;
  }
}

function initQuickAddChannelModalEvents() {
  const modal = document.getElementById('quickAddChannelModal');
  const form = document.getElementById('quickAddChannelForm');
  if (!modal || !form || modal.dataset.bound) return;

  modal.addEventListener('click', event => {
    if (event.target === modal) closeQuickAddChannelModal();
  });
  form.addEventListener('submit', event => {
    event.preventDefault();
    confirmQuickAddChannel();
  });
  document.addEventListener('keydown', event => {
    if (!modal.classList.contains('show')) return;
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      closeQuickAddChannelModal();
      return;
    }
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      confirmQuickAddChannel();
      return;
    }
    if (event.key !== 'Tab') return;

    const focusable = Array.from(modal.querySelectorAll('button:not([disabled]), input:not([disabled]), textarea:not([disabled])'));
    if (focusable.length === 0) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  }, true);
  modal.dataset.bound = '1';
}

async function fetchModelsFromAPI() {
  let endpoint;
  let fetchOptions;
  let modelFetchEntries = [];
  let skippedKeyCount = 0;
  if (['antigravity_oauth', 'codex_oauth', 'xai_oauth', 'anthropic_oauth', 'zai_oauth', 'cursor_oauth', 'zed_oauth'].includes(editingChannelAuthType)) {
    if (!editingChannelId) {
      if (window.showError) window.showError(window.t('channels.saveBeforeModelTest'));
      else alert(window.t('channels.saveBeforeModelTest'));
      return;
    }
    endpoint = `/admin/channels/${editingChannelId}/models/fetch`;
  } else {
    const urls = getValidInlineURLConfigs();
    const channelUrl = urls[0]?.url || '';
    const keyRows = getInlineKeyRows();
    modelFetchEntries = selectModelFetchKeyEntries(keyRows, currentChannelKeyCooldowns);
    const availableKeys = modelFetchEntries.map(entry => entry.apiKey);
    skippedKeyCount = Math.max(
      0,
      countConfiguredInlineKeys(keyRows) - modelFetchEntries.length
    );

    if (!channelUrl) {
      if (window.showError) {
        window.showError(window.t('channels.fillApiUrlFirst'));
      } else {
        alert(window.t('channels.fillApiUrlFirst'));
      }
      return;
    }

    if (availableKeys.length === 0) {
      if (window.showError) {
        window.showError(window.t('channels.addAtLeastOneEnabledKey'));
      } else {
        alert(window.t('channels.addAtLeastOneEnabledKey'));
      }
      return;
    }

    endpoint = '/admin/channels/models/fetch';
    fetchOptions = {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        urls,
        api_keys: availableKeys,
        per_key: true
      })
    };
  }

  try {
    const response = await fetchAPIWithAuth(endpoint, fetchOptions);
    if (!response.success) throw new Error(response.error || window.t('channels.fetchModelsFailed', { error: '' }));
    const data = response.data || {};
    const keyModels = Array.isArray(data.key_models) ? data.key_models : [];
    const failedKeyCount = keyModels.filter(result => result?.error).length;
    let unmatchedKeyCount = 0;

    if (!data.models || data.models.length === 0) {
      const firstKeyError = keyModels.find(result => result?.error)?.error;
      throw new Error(firstKeyError || window.t('channels.noModelsFromApi'));
    }

    const previousRows = redirectTableData.map(row => ({
      model: row.model || '',
      redirect_model: row.redirect_model || '',
      disabled: !!row.disabled
    }));
    const replacement = mergeModelRowsWithFetchedModels(redirectTableData, data.models);
    if (replacement.rows.length === 0) {
      throw new Error(window.t('channels.noModelsFromApi'));
    }

    redirectTableData = replacement.rows;
    selectedModelIndices.clear();
    updateModelBatchDeleteButton();

    renderRedirectTable();
    if (!areModelRowsEqual(previousRows, redirectTableData)) markChannelFormDirty();

    if (modelFetchEntries.length > 0 && keyModels.length > 0) {
      const scopeProposal = proposeFetchedKeyModelScopes(
        getInlineKeyRows(),
        redirectTableData,
        keyModels,
        modelFetchEntries
      );
      const completeScopeDetection = skippedKeyCount === 0 &&
        failedKeyCount === 0 &&
        keyModels.length === modelFetchEntries.length &&
        scopeProposal.complete;
      const shouldApply = completeScopeDetection && scopeProposal.changedCount > 0 &&
        typeof window.confirm === 'function' &&
        window.confirm(window.t('channels.applyFetchedKeyModelsConfirm', { count: scopeProposal.changedCount }));
      if (shouldApply && scopeProposal.changedCount > 0) {
        inlineKeyTableData = scopeProposal.rows;
        renderInlineKeyTable();
        markChannelFormDirty();
        if (window.showSuccess) {
          window.showSuccess(window.t('channels.appliedFetchedKeyModels', { count: scopeProposal.changedCount }));
        }
      }
      unmatchedKeyCount = scopeProposal.unmatchedCount;
    }

    const source = data.source === 'api' ? window.t('channels.fetchModelsSource.api') : window.t('channels.fetchModelsSource.predefined');
    if (window.showSuccess) {
      window.showSuccess(window.t('channels.fetchModelsSuccess', { source, total: redirectTableData.length, added: replacement.added }));
    } else {
      alert(window.t('channels.fetchModelsSuccess', { source, total: redirectTableData.length, added: replacement.added }));
    }

    const warnings = [];
    if (failedKeyCount > 0) {
      warnings.push(window.t('channels.fetchModelsPartialFailed', { failed: failedKeyCount }));
    }
    if (skippedKeyCount > 0) {
      warnings.push(window.t('channels.fetchModelsSkippedKeys', { count: skippedKeyCount }));
    }
    if (unmatchedKeyCount > 0) {
      warnings.push(window.t('channels.fetchModelsUnmatchedKeys', { count: unmatchedKeyCount }));
    }
    if (warnings.length > 0) {
      if (window.showWarning) window.showWarning(warnings.join(' '));
      else alert(warnings.join(' '));
    }

  } catch (error) {
    console.error('Fetch models failed', error);

    if (window.showError) {
      window.showError(window.t('channels.fetchModelsFailed', { error: error.message }));
    } else {
      alert(window.t('channels.fetchModelsFailed', { error: error.message }));
    }
  }
}

function setFetchSub2APIRatePending(pending) {
  const button = document.getElementById('fetchSub2APIRateBtn');
  const label = document.getElementById('fetchSub2APIRateLabel');
  if (button) {
    button.disabled = pending;
    if (pending) button.setAttribute('aria-busy', 'true');
    else button.removeAttribute('aria-busy');
  }
  if (label) {
    label.textContent = window.t(pending ? 'channels.fetchRateLoading' : 'channels.fetchRate');
  }
}

function showSub2APIRateError(code) {
  const knownCodes = new Set([
    'authentication_error',
    'permission_error',
    'not_supported',
    'timeout',
    'invalid_response'
  ]);
  const errorCode = knownCodes.has(code) ? code : 'default';
  const message = window.t(`channels.fetchRateError.${errorCode}`);
  if (window.showError) window.showError(message);
  else alert(message);
}

async function fetchSub2APIRate() {
  const baseURL = getValidInlineURLConfigs()[0]?.url || '';
  const apiKey = selectFirstEnabledInlineKey(getInlineKeyRows(), currentChannelKeyCooldowns);

  if (!baseURL) {
    if (window.showError) window.showError(window.t('channels.fillApiUrlFirst'));
    else alert(window.t('channels.fillApiUrlFirst'));
    return;
  }
  if (!apiKey) {
    if (window.showError) window.showError(window.t('channels.addAtLeastOneEnabledKey'));
    else alert(window.t('channels.addAtLeastOneEnabledKey'));
    return;
  }

  setFetchSub2APIRatePending(true);
  try {
    const response = await fetchAPIWithAuth('/admin/channels/billing/fetch', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ base_url: baseURL, api_key: apiKey })
    });
    if (!response.success) {
      showSub2APIRateError(response.data?.code);
      return;
    }

    const rate = response.data?.effective_rate_multiplier;
    if (typeof rate !== 'number' || !Number.isFinite(rate) || rate < 0) {
      showSub2APIRateError('invalid_response');
      return;
    }

    const input = document.getElementById('channelCostMultiplier');
    if (!input) return;
    const rateText = String(rate);
    const currentRate = Number.parseFloat(input.value);
    input.value = rateText;
    if (!Number.isFinite(currentRate) || currentRate !== rate) markChannelFormDirty();

    const message = window.t('channels.fetchRateSuccess', { rate: rateText });
    if (window.showSuccess) window.showSuccess(message);
    else alert(message);
  } catch (error) {
    console.error('Fetch Sub2API rate failed', error);
    showSub2APIRateError('default');
  } finally {
    setFetchSub2APIRatePending(false);
  }
}

// 常用模型配置
const COMMON_MODELS = {
  anthropic: [
    'claude-haiku-4-5-20251001',
    'claude-opus-4-8',
    'claude-opus-5',
    'claude-fable-5',
    'claude-sonnet-5',
    'claude-sonnet-4-6',
  ],
  codex: [
    'gpt-5.4',
    'gpt-5.4-mini',
    'gpt-5.5',
    'gpt-5.6-sol',
    'gpt-5.6-luna',
    'gpt-5.6-terra',
    'gpt-5.3-codex-spark',
    'codex-auto-review'
  ],
  gemini: [
    'gemini-3.6-flash',
    'gemini-3.5-flash',
    'gemini-2.5-pro',
    'gemini-3.1-flash-lite',
    'gemini-3.1-pro'
  ]
};

let commonModelsTrigger = null;

function getCommonModelsModalCheckboxes() {
  return Array.from(document.querySelectorAll('#commonModelsModal input[name="commonModelType"]'));
}

function openCommonModelsModal(trigger) {
  const modal = document.getElementById('commonModelsModal');
  if (!modal) return false;

  commonModelsTrigger = trigger || document.activeElement;
  const configuredProtocols = new Set(
    getValidInlineURLConfigs().flatMap(entry => Array.isArray(entry.protocols) ? entry.protocols : [])
  );
  const checkboxes = getCommonModelsModalCheckboxes();
  checkboxes.forEach(checkbox => {
    checkbox.checked = configuredProtocols.size > 0
      ? configuredProtocols.has(checkbox.value)
      : checkbox.value === 'anthropic';
  });

  document.getElementById('channelModal')?.setAttribute('inert', '');
  modal.classList.add('show');
  modal.setAttribute('aria-hidden', 'false');
  (checkboxes.find(checkbox => checkbox.checked) || checkboxes[0])?.focus();
  return true;
}

function closeCommonModelsModal() {
  const modal = document.getElementById('commonModelsModal');
  if (!modal) return;

  modal.classList.remove('show');
  modal.setAttribute('aria-hidden', 'true');
  document.getElementById('channelModal')?.removeAttribute('inert');
  if (commonModelsTrigger && typeof commonModelsTrigger.focus === 'function') {
    commonModelsTrigger.focus();
  }
  commonModelsTrigger = null;
}

function getSelectedCommonModelTypes() {
  return getCommonModelsModalCheckboxes()
    .filter(checkbox => checkbox.checked)
    .map(checkbox => checkbox.value);
}

function initCommonModelsModalEvents() {
  const modal = document.getElementById('commonModelsModal');
  if (!modal || modal.dataset.bound) return;

  modal.addEventListener('click', event => {
    if (event.target === modal) closeCommonModelsModal();
  });
  document.addEventListener('keydown', event => {
    if (!modal.classList.contains('show')) return;
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      closeCommonModelsModal();
      return;
    }
    if (event.key !== 'Tab') return;

    const focusable = Array.from(modal.querySelectorAll('button:not([disabled]), input:not([disabled])'));
    if (focusable.length === 0) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  }, true);
  modal.dataset.bound = '1';
}

function addCommonModelsToRows(rows, protocols) {
  const selectedProtocols = Array.from(new Set(
    (Array.isArray(protocols) ? protocols : [])
      .map(protocol => String(protocol || '').trim().toLowerCase())
      .filter(protocol => COMMON_MODELS[protocol])
  ));
  const commonModels = selectedProtocols.flatMap(protocol => COMMON_MODELS[protocol]);
  if (commonModels.length === 0) return { addedCount: 0, hasSupportedTypes: false };

  const existingModels = new Set(
    rows
      .map(r => (r.model || '').trim().toLowerCase())
      .filter(Boolean)
  );

  let addedCount = 0;
  for (const modelName of commonModels) {
    const modelKey = modelName.toLowerCase();
    if (!existingModels.has(modelKey)) {
      rows.push({ model: modelName, redirect_model: '' });
      existingModels.add(modelKey);
      addedCount++;
    }
  }
  return { addedCount, hasSupportedTypes: true };
}

function addCommonModels(protocols) {
  const result = addCommonModelsToRows(redirectTableData, protocols);
  if (!result.hasSupportedTypes) {
    if (window.showWarning) {
      window.showWarning(window.t('channels.selectCommonModelType'));
    } else {
      alert(window.t('channels.selectCommonModelType'));
    }
    return 0;
  }

  renderRedirectTable();
  if (result.addedCount > 0) markChannelFormDirty();

  if (window.showSuccess) {
    window.showSuccess(window.t('channels.addedCommonModels', { count: result.addedCount }));
  }
  return result.addedCount;
}

function confirmCommonModelsSelection() {
  const selectedTypes = getSelectedCommonModelTypes();
  if (selectedTypes.length === 0) {
    if (window.showWarning) {
      window.showWarning(window.t('channels.selectCommonModelType'));
    } else {
      alert(window.t('channels.selectCommonModelType'));
    }
    getCommonModelsModalCheckboxes()[0]?.focus();
    return false;
  }

  addCommonModels(selectedTypes);
  closeCommonModelsModal();
  return true;
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    addCommonModels,
    addCommonModelsToRows,
    applyQuickAddChannelSetup,
    batchClearSelectedChannelCooldowns,
    batchSetSelectedChannelsPriority,
    batchSetSelectedChannelsDailyCostLimit,
    batchSetSelectedChannelsCostMultiplier,
    batchSetSelectedChannelsMaxConcurrency,
    batchSetSelectedChannelsProtocolMode,
    batchSetSelectedChannelsRPMLimit,
    collectModelsForSubmit,
    confirmModelImport,
    detectChannelWebsocketSupport,
    discoverQuickAddChannelSetup,
    editChannel,
    exportChannelModels,
    fetchModelsFromAPI,
    fetchSub2APIRate,
    initModelNormalizationOptions,
    mergeModelRowsWithFetchedModels,
    openBatchModelImportModal,
    parseQuickAddChannelInfo,
    proposeFetchedKeyModelScopes,
    saveChannel,
    testRedirectModel,
    toggleModelDisabledState
  };
}
