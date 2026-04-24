const TEST_MODE_CHANNEL = 'channel';
const TEST_MODE_MODEL = 'model';

let channelsList = [];
let selectedChannel = null;
let selectedModelType = '';
let selectedModelName = '';
let selectedProtocol = '';
let testMode = TEST_MODE_CHANNEL;
let isDeletingModels = false;
let isAddingModels = false;
let isTestingModels = false;

let channelSelectCombobox = null;
let modelSelectCombobox = null;

const headRow = document.getElementById('model-test-head-row');
const tbody = document.getElementById('model-test-tbody');
const toolbar = document.querySelector('.model-test-toolbar');
const channelSelectorLabel = document.getElementById('channelSelectorLabel');
const modelTypeLabel = document.getElementById('modelTypeLabel');
const modelTypeSelect = document.getElementById('testModelType');
const modelSelectorLabel = document.getElementById('modelSelectorLabel');
const protocolTransformContainer = document.getElementById('protocolTransformContainer');
const protocolTransformOptions = document.getElementById('protocolTransformOptions');
const modelSelect = document.getElementById('testModelSelect');
const mobileNameFilterInput = document.getElementById('modelTestMobileNameFilter');

const addModelsModal = document.getElementById('addModelsModal');
const addModelsTextarea = document.getElementById('addModelsTextarea');
const addModelsCloseBtn = document.getElementById('addModelsCloseBtn');
const addModelsCancelBtn = document.getElementById('addModelsCancelBtn');
const addModelsConfirmBtn = document.getElementById('addModelsConfirmBtn');

const deletePreviewModal = document.getElementById('deletePreviewModal');
const deletePreviewContent = document.getElementById('deletePreviewContent');
const deletePreviewProgress = document.getElementById('deletePreviewProgress');
const deletePreviewRuntimeLog = document.getElementById('deletePreviewRuntimeLog');
const deletePreviewCloseBtn = document.getElementById('deletePreviewCloseBtn');
const deletePreviewCancelBtn = document.getElementById('deletePreviewCancelBtn');
const deletePreviewConfirmBtn = document.getElementById('deletePreviewConfirmBtn');

const RESULT_TABLE_COLSPAN_WITH_FIRST_BYTE = 11;
const RESULT_TABLE_COLSPAN_NO_FIRST_BYTE = 10;
const SORT_DIRECTION_ASC = 1;
const SORT_DIRECTION_DESC = -1;
const SORT_DIRECTION_NONE = 0;
const ALL_PROTOCOLS = ['anthropic', 'codex', 'openai', 'gemini'];
let sortState = { key: '', direction: SORT_DIRECTION_NONE };
let nameFilterKeyword = '';

function getFetchModelsBtn() {
  return document.getElementById('fetchModelsBtn');
}

function getAddModelsBtn() {
  return document.getElementById('addModelsBtn');
}

function getDeleteModelsBtn() {
  return document.getElementById('deleteModelsBtn');
}

function getRunTestBtn() {
  return document.getElementById('runTestBtn');
}

const RESPONSE_HEAD_HTML = `
  <th class="table-col-response model-test-response-head" data-sort-key="response">
    <div class="model-test-response-head-inner">
      <div class="model-test-response-head-line">
        <span class="model-test-response-head-label" data-i18n="modelTest.responseContent">响应内容</span>
      </div>
      <div class="model-test-toolbar-section model-test-toolbar-section--actions model-test-head-actions">
        <button id="fetchModelsBtn" type="button" data-action="fetch-and-add-models" class="btn btn-secondary model-test-toolbar-btn" data-i18n="modelTest.fetchModels">获取模型</button>
        <button id="addModelsBtn" type="button" data-action="open-add-models-modal" class="btn btn-secondary model-test-toolbar-btn hidden" data-i18n="modelTest.addModels">添加模型</button>
        <button id="deleteModelsBtn" type="button" data-action="delete-selected-models" class="btn btn-secondary model-test-toolbar-btn model-test-toolbar-btn--danger" data-i18n="modelTest.deleteModels">删除模型</button>
        <button id="runTestBtn" type="button" data-action="run-model-tests" class="btn btn-primary model-test-toolbar-btn" data-i18n="modelTest.startTest">开始测试</button>
      </div>
    </div>
  </th>
`;

const CHANNEL_MODE_HEAD = `
  <th class="table-col-select mobile-card-select-header"><input type="checkbox" id="selectAllCheckbox" data-change-action="toggle-all-models"></th>
  <th class="table-col-name" data-i18n="common.model" data-sort-key="name">模型</th>
  <th class="first-byte-col table-col-duration" data-i18n="modelTest.firstByteDuration" data-sort-key="firstByteDuration">首字</th>
  <th class="table-col-duration" data-i18n="modelTest.totalDuration" data-sort-key="duration">总耗时</th>
  <th class="table-col-metric" data-i18n="common.input" data-sort-key="inputTokens">输入</th>
  <th class="table-col-metric" data-i18n="common.output" data-sort-key="outputTokens">输出</th>
  <th class="table-col-speed" data-i18n="modelTest.speed" data-sort-key="speed">速度(tok/s)</th>
  <th class="table-col-metric" data-i18n="modelTest.cacheRead" data-sort-key="cacheRead">缓读</th>
  <th class="table-col-metric" data-i18n="modelTest.cacheCreate" data-sort-key="cacheCreate">缓建</th>
  <th class="table-col-cost" data-i18n="common.cost" data-sort-key="cost">费用</th>
  ${RESPONSE_HEAD_HTML}
`;

const MODEL_MODE_HEAD = `
  <th class="table-col-select mobile-card-select-header"><input type="checkbox" id="selectAllCheckbox" data-change-action="toggle-all-models"></th>
  <th class="table-col-channel" data-i18n="modelTest.channelName" data-sort-key="name">渠道</th>
  <th class="first-byte-col table-col-duration" data-i18n="modelTest.firstByteDuration" data-sort-key="firstByteDuration">首字</th>
  <th class="table-col-duration" data-i18n="modelTest.totalDuration" data-sort-key="duration">总耗时</th>
  <th class="table-col-metric" data-i18n="common.input" data-sort-key="inputTokens">输入</th>
  <th class="table-col-metric" data-i18n="common.output" data-sort-key="outputTokens">输出</th>
  <th class="table-col-speed" data-i18n="modelTest.speed" data-sort-key="speed">速度(tok/s)</th>
  <th class="table-col-metric" data-i18n="modelTest.cacheRead" data-sort-key="cacheRead">缓读</th>
  <th class="table-col-metric" data-i18n="modelTest.cacheCreate" data-sort-key="cacheCreate">缓建</th>
  <th class="table-col-cost" data-i18n="common.cost" data-sort-key="cost">费用</th>
  ${RESPONSE_HEAD_HTML}
`;

function i18nText(key, fallback, params) {
  const interpolate = (text) => {
    if (!params || typeof text !== 'string') return text;
    let result = text;
    Object.keys(params).forEach((paramKey) => {
      const safeKey = paramKey.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
      result = result.replace(new RegExp(`\\{${safeKey}\\}`, 'g'), String(params[paramKey]));
    });
    return result;
  };

  if (typeof window.t === 'function') {
    const result = window.t(key, params);
    if (result && result !== key) return interpolate(result);
  }
  return interpolate(fallback);
}

function normalizeProtocol(value) {
  return String(value || '').trim().toLowerCase();
}

function protocolLabel(protocol) {
  const labels = {
    anthropic: 'channels.protocolTransformAnthropic',
    codex: 'channels.protocolTransformCodex',
    openai: 'channels.protocolTransformOpenAI',
    gemini: 'channels.protocolTransformGemini'
  };
  const key = labels[protocol] || protocol;
  return i18nText(key, protocol);
}

function formatDurationMs(durationMs) {
  return (typeof durationMs === 'number' && Number.isFinite(durationMs) && durationMs > 0)
    ? `${(durationMs / 1000).toFixed(2)}s`
    : '-';
}

function calculateTestSpeed(data, usage) {
  const outputTokens = Number(
    usage?.output_tokens
      ?? usage?.completion_tokens
      ?? usage?.candidatesTokenCount
  );
  const durationSeconds = Number(data?.duration_ms) / 1000;
  if (!Number.isFinite(outputTokens) || outputTokens <= 0 || !Number.isFinite(durationSeconds) || durationSeconds <= 0) {
    return null;
  }
  return outputTokens / durationSeconds;
}

function parseNumericCellValue(text) {
  const normalized = String(text || '')
    .replace(/[^0-9.+-]/g, '')
    .trim();
  if (!normalized) return null;
  const value = Number.parseFloat(normalized);
  return Number.isFinite(value) ? value : null;
}

function compareSortValues(a, b) {
  const aNil = a === null || a === undefined || a === '';
  const bNil = b === null || b === undefined || b === '';
  if (aNil && bNil) return 0;
  if (aNil) return 1;
  if (bNil) return -1;

  if (typeof a === 'number' && typeof b === 'number') {
    return a - b;
  }
  return String(a).localeCompare(String(b), 'zh-CN', { numeric: true, sensitivity: 'base' });
}

function isFirstByteColumnVisible() {
  const streamEnabled = document.getElementById('streamEnabled');
  return Boolean(streamEnabled?.checked);
}

function getResultTableColspan() {
  return String(isFirstByteColumnVisible() ? RESULT_TABLE_COLSPAN_WITH_FIRST_BYTE : RESULT_TABLE_COLSPAN_NO_FIRST_BYTE);
}

function isDataRowVisible(row) {
  return row.style.display !== 'none';
}

function getVisibleRowCheckboxes() {
  return Array.from(document.querySelectorAll('#model-test-tbody tr'))
    .filter(row => isDataRowVisible(row))
    .map(row => row.querySelector('.row-checkbox'))
    .filter(Boolean);
}

function getRowSelectionKey(row) {
  const channelId = String(row?.dataset?.channelId || '');
  const modelName = String(row?.dataset?.model || '');
  return `${channelId}::${modelName}`;
}

function captureRowSelectionState() {
  const selectionState = new Map();
  Array.from(tbody.querySelectorAll('tr[data-channel-id][data-model]')).forEach((row) => {
    const checkbox = row.querySelector('.row-checkbox');
    if (!checkbox) return;
    selectionState.set(getRowSelectionKey(row), checkbox.checked);
  });
  return selectionState;
}

function restoreRowSelectionState(row, selectionState, fallbackChecked = true) {
  const checkbox = row?.querySelector('.row-checkbox');
  if (!checkbox) return;

  const selectionKey = getRowSelectionKey(row);
  if (selectionState?.has(selectionKey)) {
    checkbox.checked = Boolean(selectionState.get(selectionKey));
    return;
  }

  checkbox.checked = Boolean(fallbackChecked);
}

function getNameFilterPlaceholder() {
  if (testMode === TEST_MODE_MODEL) {
    return i18nText('modelTest.filterChannelPlaceholder', '搜索渠道名称...');
  }
  return i18nText('modelTest.filterModelPlaceholder', '搜索模型名称...');
}

function syncNameFilterInputs() {
  const placeholder = getNameFilterPlaceholder();
  const headerInput = document.getElementById('modelTestNameFilter');

  if (headerInput) {
    headerInput.placeholder = placeholder;
    headerInput.value = nameFilterKeyword;
  }

  if (mobileNameFilterInput) {
    mobileNameFilterInput.placeholder = placeholder;
    mobileNameFilterInput.value = nameFilterKeyword;
  }
}

function setNameFilterKeyword(value) {
  nameFilterKeyword = String(value || '');
  syncNameFilterInputs();
  applyNameFilter();
}

function getResultRowMobileLabels(nameKey, nameFallback) {
  return {
    mobileLabelSelect: '',
    mobileLabelName: i18nText(nameKey, nameFallback),
    mobileLabelFirstByte: i18nText('modelTest.firstByteDuration', '首字'),
    mobileLabelDuration: i18nText('modelTest.totalDuration', '总耗时'),
    mobileLabelInput: i18nText('common.input', '输入'),
    mobileLabelOutput: i18nText('common.output', '输出'),
    mobileLabelSpeed: i18nText('modelTest.speed', '速度(tok/s)'),
    mobileLabelCacheRead: i18nText('modelTest.cacheRead', '缓读'),
    mobileLabelCacheCreate: i18nText('modelTest.cacheCreate', '缓建'),
    mobileLabelCost: i18nText('common.cost', '费用'),
    mobileLabelResponse: i18nText('modelTest.responseContent', '响应内容')
  };
}

function initModelTestActions() {
  if (typeof window.initDelegatedActions !== 'function') return;

  window.initDelegatedActions({
    boundKey: 'modelTestActionsBound',
    click: {
      'set-test-mode': (actionTarget) => setTestMode(actionTarget.dataset.mode || ''),
      'fetch-and-add-models': () => fetchAndAddModels(),
      'open-add-models-modal': () => openAddModelsModal(),
      'delete-selected-models': () => deleteSelectedModels(),
      'run-model-tests': () => runModelTests()
    },
    change: {
      'toggle-all-models': (actionTarget) => toggleAllModels(actionTarget.checked)
    }
  });
}

function renderNameFilterInHeader() {
  const nameTh = headRow.querySelector('th[data-sort-key="name"]');
  if (!nameTh) return;
  const filterWidth = testMode === TEST_MODE_MODEL ? '160px' : '130px';

  let headerLine = nameTh.querySelector('.model-test-name-head-line');
  let label = nameTh.querySelector('.model-test-name-label');
  let input = nameTh.querySelector('#modelTestNameFilter');

  if (!headerLine || !label || !input) {
    const baseLabel = (nameTh.textContent || '').trim();
    nameTh.textContent = '';
    nameTh.style.whiteSpace = 'nowrap';
    nameTh.style.verticalAlign = 'middle';

    headerLine = document.createElement('div');
    headerLine.className = 'model-test-name-head-line';
    headerLine.style.display = 'flex';
    headerLine.style.alignItems = 'center';
    headerLine.style.gap = '6px';
    headerLine.style.width = '100%';

    label = document.createElement('span');
    label.className = 'model-test-name-label';
    label.textContent = baseLabel;
    label.style.flex = '0 0 auto';
    headerLine.appendChild(label);

    input = document.createElement('input');
    input.id = 'modelTestNameFilter';
    input.type = 'text';
    input.autocomplete = 'off';
    input.spellcheck = false;
    input.style.flex = `0 1 ${filterWidth}`;
    input.style.width = filterWidth;
    input.style.maxWidth = '100%';
    input.style.minWidth = '90px';
    input.style.padding = '6px 10px';
    input.style.border = '1px solid var(--color-border)';
    input.style.borderRadius = '6px';
    input.style.background = 'var(--color-bg-secondary)';
    input.style.color = 'var(--color-text)';
    input.style.fontSize = '13px';
    input.addEventListener('click', (event) => event.stopPropagation());
    input.addEventListener('keydown', (event) => event.stopPropagation());
    input.addEventListener('input', () => {
      setNameFilterKeyword(input.value || '');
    });

    headerLine.appendChild(input);
    nameTh.appendChild(headerLine);
  }

  const indicator = nameTh.querySelector('.model-test-sort-indicator');
  if (indicator && indicator.parentElement !== headerLine) {
    headerLine.insertBefore(indicator, input);
  }

  input.style.flex = `0 1 ${filterWidth}`;
  input.style.width = filterWidth;
  syncNameFilterInputs();
}

function applyNameFilter() {
  const keyword = nameFilterKeyword.trim().toLowerCase();
  const rows = Array.from(tbody.querySelectorAll('tr'));
  rows.forEach(row => {
    const checkbox = row.querySelector('.row-checkbox');
    if (!checkbox) return;
    if (!keyword) {
      row.style.display = '';
      return;
    }

    const nameText = (row.children[1]?.textContent || '').trim().toLowerCase();
    row.style.display = nameText.includes(keyword) ? '' : 'none';
  });
  syncSelectAllCheckbox();
}

function getRowSortValue(row, key) {
  switch (key) {
    case 'name':
      return row.children[1]?.textContent?.trim() || '';
    case 'firstByteDuration':
      return parseNumericCellValue(row.querySelector('.first-byte-duration')?.textContent);
    case 'duration':
      return parseNumericCellValue(row.querySelector('.duration')?.textContent);
    case 'inputTokens':
      return parseNumericCellValue(row.querySelector('.input-tokens')?.textContent);
    case 'outputTokens':
      return parseNumericCellValue(row.querySelector('.output-tokens')?.textContent);
    case 'speed':
      return parseNumericCellValue(row.querySelector('.speed')?.textContent);
    case 'cacheRead':
      return parseNumericCellValue(row.querySelector('.cache-read')?.textContent);
    case 'cacheCreate':
      return parseNumericCellValue(row.querySelector('.cache-create')?.textContent);
    case 'cost':
      return parseNumericCellValue(row.querySelector('.cost')?.textContent);
    case 'response':
      return row.querySelector('.response')?.textContent?.trim() || '';
    default:
      return null;
  }
}

function bindSortableHeaders() {
  headRow.querySelectorAll('th[data-sort-key]').forEach(th => {
    let indicator = th.querySelector('.model-test-sort-indicator');
    const headerLine = th.querySelector('.model-test-name-head-line');
    const responseHeadLine = th.querySelector('.model-test-response-head-line');
    const filterInput = th.querySelector('#modelTestNameFilter');

    if (!indicator) {
      indicator = document.createElement('span');
      indicator.className = 'model-test-sort-indicator';
      indicator.style.display = 'inline-block';
      indicator.style.minWidth = '0.7em';
      indicator.style.marginLeft = '2px';
      indicator.style.fontSize = '11px';
      indicator.style.lineHeight = '1';
      indicator.style.verticalAlign = 'middle';
    }

    if (headerLine && filterInput) {
      if (indicator.parentElement !== headerLine || indicator.nextSibling !== filterInput) {
        headerLine.insertBefore(indicator, filterInput);
      }
    } else if (responseHeadLine) {
      if (indicator.parentElement !== responseHeadLine) {
        responseHeadLine.appendChild(indicator);
      }
    } else if (indicator.parentElement !== th) {
      th.appendChild(indicator);
    }

    th.style.cursor = 'pointer';
    th.style.whiteSpace = 'nowrap';
    th.style.verticalAlign = 'middle';
    th.onclick = (event) => {
      const clickTarget = event.target instanceof Element ? event.target : null;
      if (clickTarget?.closest('.model-test-head-actions')) return;

      const key = th.dataset.sortKey || '';
      if (!key) return;

      if (sortState.key !== key) {
        sortState = { key, direction: SORT_DIRECTION_ASC };
      } else if (sortState.direction === SORT_DIRECTION_ASC) {
        sortState = { key, direction: SORT_DIRECTION_DESC };
      } else if (sortState.direction === SORT_DIRECTION_DESC) {
        sortState = { key: '', direction: SORT_DIRECTION_NONE };
      } else {
        sortState = { key, direction: SORT_DIRECTION_ASC };
      }

      applyCurrentSort();
      updateSortIndicators();
    };
  });
}

function updateSortIndicators() {
  headRow.querySelectorAll('th[data-sort-key]').forEach(th => {
    const key = th.dataset.sortKey || '';
    let indicator = th.querySelector('.model-test-sort-indicator');
    if (!indicator) return;

    if (sortState.key !== key || sortState.direction === SORT_DIRECTION_NONE) {
      indicator.textContent = '';
      return;
    }

    if (sortState.direction === SORT_DIRECTION_ASC) {
      indicator.textContent = '↑';
      return;
    }

    indicator.textContent = '↓';
  });
}

function applyCurrentSort() {
  const rows = Array.from(tbody.querySelectorAll('tr'));
  const dataRows = rows.filter(row => !row.querySelector('td[colspan]'));
  if (dataRows.length === 0) return;

  if (!isFirstByteColumnVisible() && sortState.key === 'firstByteDuration') {
    sortState = { key: '', direction: SORT_DIRECTION_NONE };
  }

  if (sortState.direction === SORT_DIRECTION_NONE || !sortState.key) {
    dataRows.sort((a, b) => Number(a.dataset.baseOrder || 0) - Number(b.dataset.baseOrder || 0));
  } else {
    dataRows.sort((a, b) => {
      const av = getRowSortValue(a, sortState.key);
      const bv = getRowSortValue(b, sortState.key);
      const primary = compareSortValues(av, bv) * sortState.direction;
      if (primary !== 0) return primary;
      return Number(a.dataset.baseOrder || 0) - Number(b.dataset.baseOrder || 0);
    });
  }

  const fragment = document.createDocumentFragment();
  dataRows.forEach(row => fragment.appendChild(row));
  tbody.appendChild(fragment);
}

function applyFirstByteVisibility() {
  const visible = isFirstByteColumnVisible();
  headRow.querySelectorAll('.first-byte-col').forEach(cell => {
    cell.style.display = visible ? '' : 'none';
  });
  tbody.querySelectorAll('.first-byte-duration').forEach(cell => {
    cell.style.display = visible ? '' : 'none';
  });

  const emptyCell = tbody.querySelector('tr > td[colspan]');
  if (emptyCell) {
    emptyCell.setAttribute('colspan', getResultTableColspan());
  }

  if (!visible && sortState.key === 'firstByteDuration') {
    sortState = { key: '', direction: SORT_DIRECTION_NONE };
    applyCurrentSort();
    updateSortIndicators();
  }
}

function markRowBaseOrder() {
  Array.from(tbody.querySelectorAll('tr')).forEach((row, index) => {
    if (row.querySelector('td[colspan]')) return;
    row.dataset.baseOrder = String(index);
  });
}

function finalizeTableRender() {
  markRowBaseOrder();
  applyCurrentSort();
  applyNameFilter();
  applyFirstByteVisibility();
}

function getModelName(entry) {
  return (typeof entry === 'string') ? entry : entry?.model;
}

function getChannelType(channel) {
  return normalizeProtocol(channel?.channel_type) || 'anthropic';
}

function channelMatchesModelType(channel, modelType = selectedModelType) {
  const normalizedModelType = normalizeProtocol(modelType);
  if (!normalizedModelType) return true;
  return getChannelType(channel) === normalizedModelType;
}

function getAvailableChannelTypes() {
  return Array.from(new Set(channelsList.map(ch => getChannelType(ch)))).sort((a, b) => a.localeCompare(b));
}

function ensureSelectedModelType() {
  const channelTypes = getAvailableChannelTypes();
  if (!channelTypes.length) {
    selectedModelType = '';
    return;
  }

  if (!selectedModelType || !channelTypes.includes(selectedModelType)) {
    selectedModelType = channelTypes[0];
  }
}

function populateModelTypeSelect() {
  if (!modelTypeSelect) return;

  ensureSelectedModelType();
  const channelTypes = getAvailableChannelTypes();
  modelTypeSelect.innerHTML = channelTypes.map((channelType) => `
    <option value="${channelType}" ${selectedModelType === channelType ? 'selected' : ''}>${protocolLabel(channelType)}</option>
  `).join('');
}

function getSupportedProtocols(channel) {
  const upstreamProtocol = getChannelType(channel);
  if (!ALL_PROTOCOLS.includes(upstreamProtocol)) {
    return [upstreamProtocol];
  }
  return [...ALL_PROTOCOLS];
}

function getExposedProtocols(channel) {
  const upstreamProtocol = getChannelType(channel);
  const protocols = new Set([upstreamProtocol]);
  const transforms = Array.isArray(channel?.protocol_transforms) ? channel.protocol_transforms : [];
  transforms.forEach((protocol) => {
    const normalized = normalizeProtocol(protocol);
    if (!normalized || normalized === upstreamProtocol) return;
    if (!ALL_PROTOCOLS.includes(normalized)) return;
    protocols.add(normalized);
  });
  return Array.from(protocols);
}

function channelExposesProtocol(channel, protocol) {
  return getExposedProtocols(channel).includes(normalizeProtocol(protocol));
}

function channelSupportsProtocol(channel, protocol) {
  return getSupportedProtocols(channel).includes(normalizeProtocol(protocol));
}

function getAllModelsForProtocol(protocol) {
  const normalizedProtocol = normalizeProtocol(protocol);
  const modelSet = new Set();
  channelsList.forEach(ch => {
    const include = channelMatchesModelType(ch) || channelExposesProtocol(ch, normalizedProtocol);
    if (!include) return;
    (ch.models || []).forEach(entry => {
      const modelName = getModelName(entry);
      if (modelName) modelSet.add(modelName);
    });
  });
  return Array.from(modelSet).sort((a, b) => a.localeCompare(b));
}

function ensureSelectedProtocolForCurrentMode() {
  if (testMode === TEST_MODE_CHANNEL && selectedChannel) {
    const supportedProtocols = getSupportedProtocols(selectedChannel);
    if (!selectedProtocol || !supportedProtocols.includes(selectedProtocol)) {
      selectedProtocol = getChannelType(selectedChannel);
    }
    return;
  }

  if (selectedProtocol) return;
  selectedProtocol = selectedModelType || (channelsList[0] ? getChannelType(channelsList[0]) : 'anthropic');
}

function renderProtocolTransformOptions() {
  if (!protocolTransformOptions) return;

  ensureSelectedProtocolForCurrentMode();

  const supported = testMode === TEST_MODE_CHANNEL && selectedChannel
    ? new Set(getSupportedProtocols(selectedChannel))
    : null;

  if (supported && !supported.has(selectedProtocol)) {
    selectedProtocol = getChannelType(selectedChannel);
  }

  protocolTransformOptions.innerHTML = ALL_PROTOCOLS.map((protocol) => {
    const disabled = Boolean(supported) && !supported.has(protocol);
    const checked = selectedProtocol === protocol;
    return `
      <label class="channel-editor-radio-option">
        <input type="radio"
               name="modelTestProtocolTransform"
               value="${protocol}"
               ${checked ? 'checked' : ''}
               ${disabled ? 'disabled' : ''}>
        <span>${protocolLabel(protocol)}</span>
      </label>
    `;
  }).join('');
}

function isModelSupported(channel, modelName) {
  if (!channel || !modelName || !Array.isArray(channel.models)) return false;
  return channel.models.some(entry => getModelName(entry) === modelName);
}

function getChannelsSupportingModel(protocol, modelName) {
  const normalizedProtocol = normalizeProtocol(protocol);
  return channelsList
    // 模型类型只用于缩小模型候选，不应把同模型的转换渠道挡掉。
    .filter(ch => channelExposesProtocol(ch, normalizedProtocol) && isModelSupported(ch, modelName))
    .sort((a, b) => b.priority - a.priority || a.name.localeCompare(b.name));
}

function getModelInputValue() {
  return (modelSelect?.value || '').trim();
}

function setModelInputValue(value) {
  const nextValue = String(value || '').trim();
  if (modelSelectCombobox) {
    modelSelectCombobox.setValue(nextValue, nextValue);
    return;
  }

  if (modelSelect) {
    modelSelect.value = nextValue;
  }
}

function ensureModelSelectCombobox() {
  if (modelSelectCombobox || !modelSelect) return;
  if (typeof window.createSearchableCombobox !== 'function') return;

  modelSelectCombobox = window.createSearchableCombobox({
    attachMode: true,
    inputId: 'testModelSelect',
    dropdownId: 'testModelSelectDropdown',
    initialValue: selectedModelName,
    initialLabel: selectedModelName,
    getOptions: () => {
      const models = getAllModelsForProtocol(selectedProtocol);
      const options = models.map(name => ({ value: name, label: name }));

      const typedModel = getModelInputValue();
      const hasExactMatch = typedModel
        ? options.some(option => String(option.value).toLowerCase() === typedModel.toLowerCase())
        : false;
      const hasFuzzyMatch = typedModel
        ? options.some(option => String(option.label).toLowerCase().includes(typedModel.toLowerCase()) || String(option.value).toLowerCase().includes(typedModel.toLowerCase()))
        : false;

      if (typedModel && !hasExactMatch && !hasFuzzyMatch) {
        options.unshift({ value: typedModel, label: typedModel });
      }

      return options;
    },
    onSelect: (value) => {
      selectedModelName = String(value || '').trim();
      if (testMode === TEST_MODE_MODEL) {
        renderModelModeRows();
      }
    },
    onCancel: () => {
      selectedModelName = getModelInputValue() || selectedModelName;
    }
  });
}

function clearProgress() {
  const progressEl = document.getElementById('testProgress');
  progressEl.textContent = '';
}

function updateHeadByMode() {
  headRow.innerHTML = testMode === TEST_MODE_MODEL ? MODEL_MODE_HEAD : CHANNEL_MODE_HEAD;
  if (window.i18n) {
    window.i18n.translatePage();
  }
  renderNameFilterInHeader();
  bindSortableHeaders();
  updateSortIndicators();
  applyFirstByteVisibility();
}

function syncSelectAllCheckbox() {
  const selectAllCheckbox = document.getElementById('selectAllCheckbox');
  if (!selectAllCheckbox) return;

  const checkboxes = getVisibleRowCheckboxes();
  if (checkboxes.length === 0) {
    selectAllCheckbox.checked = false;
    selectAllCheckbox.indeterminate = false;
    return;
  }

  const checkedCount = checkboxes.filter(cb => cb.checked).length;
  if (checkedCount === 0) {
    selectAllCheckbox.checked = false;
    selectAllCheckbox.indeterminate = false;
    return;
  }

  if (checkedCount === checkboxes.length) {
    selectAllCheckbox.checked = true;
    selectAllCheckbox.indeterminate = false;
    return;
  }

  selectAllCheckbox.checked = false;
  selectAllCheckbox.indeterminate = true;
}

function renderEmptyRow(message) {
  tbody.innerHTML = '';
  const row = TemplateEngine.render('tpl-empty-row', { message, colspan: getResultTableColspan() });
  if (row) tbody.appendChild(row);
  finalizeTableRender();
}

function renderChannelModeRows() {
  if (!selectedChannel) {
    renderEmptyRow(i18nText('modelTest.selectChannelFirst', '请先选择渠道'));
    return;
  }

  const models = selectedChannel.models || [];
  if (models.length === 0) {
    renderEmptyRow(i18nText('modelTest.channelNoModels', '该渠道没有配置模型'));
    return;
  }

  const fragment = document.createDocumentFragment();
  models.forEach(entry => {
    const modelName = getModelName(entry);
    if (!modelName) return;
    const row = TemplateEngine.render('tpl-model-row', {
      model: modelName,
      displayName: modelName,
      channelId: selectedChannel.id,
      ...getResultRowMobileLabels('common.model', '模型')
    });
    if (row) fragment.appendChild(row);
  });

  tbody.innerHTML = '';
  tbody.appendChild(fragment);
  finalizeTableRender();
}

function populateModelSelector() {
  ensureSelectedModelType();
  const models = getAllModelsForProtocol(selectedProtocol);
  const typedModel = getModelInputValue();

  if (models.length === 0) {
    selectedModelName = typedModel || '';
    setModelInputValue(selectedModelName);
    modelSelectCombobox?.refresh();
    return;
  }

  if (typedModel && models.includes(typedModel)) {
    selectedModelName = typedModel;
  } else if (!selectedModelName || !models.includes(selectedModelName)) {
    selectedModelName = models[0];
  }

  setModelInputValue(selectedModelName);
  modelSelectCombobox?.refresh();
}

function renderModelModeRows() {
  const previousSelectionState = captureRowSelectionState();
  ensureSelectedModelType();
  if (!selectedProtocol) {
    renderEmptyRow(i18nText('modelTest.selectProtocolFirst', '请先选择协议转换'));
    return;
  }

  const models = getAllModelsForProtocol(selectedProtocol);
  if (models.length === 0) {
    renderEmptyRow(i18nText('modelTest.noModelForProtocol', '该协议下没有可用模型'));
    return;
  }

  if (!selectedModelName || !models.includes(selectedModelName)) {
    const typedModel = getModelInputValue();
    if (typedModel) {
      selectedModelName = typedModel;
    } else {
      selectedModelName = models[0];
      setModelInputValue(selectedModelName);
    }
  }

  const channels = getChannelsSupportingModel(selectedProtocol, selectedModelName);
  if (channels.length === 0) {
    renderEmptyRow(i18nText('modelTest.noChannelSupportsModel', '没有渠道支持该模型'));
    return;
  }

  const fragment = document.createDocumentFragment();
  channels.forEach(ch => {
    const isEnabled = ch.enabled !== false;
    const channelName = isEnabled
      ? ch.name
      : `${ch.name} [${i18nText('common.disabled', '已禁用')}]`;

    const row = TemplateEngine.render('tpl-channel-row-by-model', {
      channelId: String(ch.id),
      channelName,
      model: selectedModelName,
      ...getResultRowMobileLabels('modelTest.channel', '渠道')
    });

    if (row) {
      const checkbox = row.querySelector('.channel-checkbox');
      if (checkbox) {
        restoreRowSelectionState(row, previousSelectionState, isEnabled);
      }

      if (!isEnabled) {
        row.style.background = 'rgba(148, 163, 184, 0.14)';
        row.style.color = 'var(--color-text-secondary)';
      }
    }

    if (row) fragment.appendChild(row);
  });

  tbody.innerHTML = '';
  tbody.appendChild(fragment);
  finalizeTableRender();
}

function renderRowsByMode() {
  if (testMode === TEST_MODE_MODEL) {
    renderModelModeRows();
  } else {
    renderChannelModeRows();
  }
}

function updateModeUI() {
  const isModelMode = testMode === TEST_MODE_MODEL;
  const fetchModelsBtn = getFetchModelsBtn();
  const addModelsBtn = getAddModelsBtn();
  const deleteModelsBtn = getDeleteModelsBtn();

  const modeTabChannel = document.getElementById('modeTabChannel');
  const modeTabModel = document.getElementById('modeTabModel');
  modeTabChannel.classList.toggle('active', !isModelMode);
  modeTabModel.classList.toggle('active', isModelMode);
  toolbar?.classList.toggle('model-test-toolbar--model-mode', isModelMode);

  channelSelectorLabel.style.display = isModelMode ? 'none' : 'flex';
  if (modelTypeLabel) {
    modelTypeLabel.style.display = isModelMode ? 'flex' : 'none';
    modelTypeLabel.classList.toggle('hidden', !isModelMode);
  }
  if (modelSelectorLabel) {
    modelSelectorLabel.style.display = isModelMode ? 'flex' : 'none';
    modelSelectorLabel.classList.toggle('hidden', !isModelMode);
  }
  if (fetchModelsBtn) {
    fetchModelsBtn.style.display = isModelMode ? 'none' : '';
  }
  if (addModelsBtn) {
    addModelsBtn.classList.toggle('hidden', !isModelMode);
    addModelsBtn.disabled = false;
  }
  if (deleteModelsBtn) {
    deleteModelsBtn.disabled = false;
    deleteModelsBtn.title = isModelMode ? i18nText('modelTest.deleteBySelectionHint', '按勾选记录删除对应渠道中的模型') : '';
  }
  if (isModelMode) {
    populateModelTypeSelect();
  }
  renderProtocolTransformOptions();
}

function getSelectedTargets() {
  const rows = Array.from(document.querySelectorAll('#model-test-tbody tr'));
  return rows
    .map(row => {
      if (!isDataRowVisible(row)) return null;
      const checkbox = row.querySelector('.row-checkbox');
      if (!checkbox || !checkbox.checked) return null;

      if (testMode === TEST_MODE_MODEL) {
        const channelId = parseInt(row.dataset.channelId, 10);
        const channel = channelsList.find(ch => ch.id === channelId);
        if (!channel) return null;
        return {
          row,
          model: selectedModelName,
          channelId: channel.id,
          protocolTransform: selectedProtocol
        };
      }

      if (!selectedChannel) return null;
      return {
        row,
        model: row.dataset.model,
        channelId: selectedChannel.id,
        protocolTransform: selectedProtocol
      };
    })
    .filter(Boolean);
}

function resetRowStatus(row) {
  row.querySelector('.first-byte-duration').textContent = '-';
  row.querySelector('.duration').textContent = '-';
  row.querySelector('.input-tokens').textContent = '-';
  row.querySelector('.output-tokens').textContent = '-';
  row.querySelector('.speed').textContent = '-';
  row.querySelector('.cache-read').textContent = '-';
  row.querySelector('.cache-create').textContent = '-';
  row.querySelector('.cost').textContent = '-';
  row.querySelector('.response').textContent = i18nText('modelTest.waiting', '等待中...');
  row.querySelector('.response').title = '';
  row.style.background = '';
}

function applyTestResultToRow(row, data) {
  row.querySelector('.first-byte-duration').textContent = formatDurationMs(data.first_byte_duration_ms);
  row.querySelector('.duration').textContent = formatDurationMs(data.duration_ms);

  if (data.success) {
    row.style.background = 'rgba(16, 185, 129, 0.1)';
    const apiResp = data.api_response || {};
    const usage = apiResp.usage || apiResp.usageMetadata || data.usage || {};
    const inputTokens = usage.input_tokens || usage.prompt_tokens || usage.promptTokenCount || '-';
    const outputTokens = usage.output_tokens || usage.completion_tokens || usage.candidatesTokenCount || '-';
    const testSpeed = calculateTestSpeed(data, usage);
    const speedDisplay = testSpeed === null
      ? '-'
      : testSpeed.toFixed(1);
    row.querySelector('.input-tokens').textContent = inputTokens;
    row.querySelector('.output-tokens').textContent = outputTokens;
    row.querySelector('.speed').textContent = speedDisplay;
    row.querySelector('.cache-read').textContent = usage.cache_read_input_tokens || usage.cached_tokens || '-';
    row.querySelector('.cache-create').textContent = usage.cache_creation_input_tokens || '-';
    row.querySelector('.cost').textContent = (typeof data.cost_usd === 'number') ? formatCost(data.cost_usd) : '-';

    let respText = data.response_text;
    if (!respText && data.api_response?.choices?.[0]?.message) {
      const msg = data.api_response.choices[0].message;
      respText = msg.content || msg.reasoning_content || msg.reasoning || msg.text;
    }
    // Anthropic format: content is array of {type, text/thinking}
    if (!respText && Array.isArray(data.api_response?.content)) {
      const textBlock = data.api_response.content.find(b => b.type === 'text');
      if (textBlock) respText = textBlock.text;
    }
    const successText = respText || i18nText('common.success', '成功');
    const responseCell = row.querySelector('.response');
    responseCell.textContent = successText;
    responseCell.title = successText;

    if (data.upstream_request_url) {
      row._upstreamData = {
        url: data.upstream_request_url,
        requestHeaders: data.upstream_request_headers,
        requestBody: data.upstream_request_body,
        statusCode: data.status_code,
        responseHeaders: data.response_headers,
        responseBody: data.upstream_response_body || data.raw_response
      };
      responseCell.classList.add('has-upstream-detail');
    }
    return;
  }

  row.style.background = 'rgba(239, 68, 68, 0.1)';
  let errMsg = '';
  const apiError = data.api_error;
  if (apiError && typeof apiError === 'object') {
    if (typeof apiError.error === 'string' && apiError.error.trim()) {
      errMsg = apiError.error.trim();
    } else if (apiError.error && typeof apiError.error === 'object') {
      if (typeof apiError.error.message === 'string' && apiError.error.message.trim()) {
        errMsg = apiError.error.message.trim();
      } else if (typeof apiError.error.error === 'string' && apiError.error.error.trim()) {
        errMsg = apiError.error.error.trim();
      } else if (typeof apiError.error.type === 'string' && apiError.error.type.trim()) {
        errMsg = apiError.error.type.trim();
      }
    } else if (typeof apiError.message === 'string' && apiError.message.trim()) {
      errMsg = apiError.message.trim();
    }
  }
  if (!errMsg) {
    errMsg = data.error || i18nText('modelTest.testFailed', '测试失败');
  }
  const responseCell = row.querySelector('.response');
  responseCell.textContent = errMsg;
  responseCell.title = errMsg;
  row.querySelector('.speed').textContent = '-';
  row.querySelector('.cost').textContent = '-';

  if (data.upstream_request_url) {
    row._upstreamData = {
      url: data.upstream_request_url,
      requestHeaders: data.upstream_request_headers,
      requestBody: data.upstream_request_body,
      statusCode: data.status_code,
      responseHeaders: data.response_headers,
      responseBody: data.upstream_response_body || data.raw_response
    };
    responseCell.classList.add('has-upstream-detail');
  }
}

async function runBatchTests(targets) {
  const streamEnabled = document.getElementById('streamEnabled').checked;
  const content = document.getElementById('modelTestContent').value.trim() || 'hi';
  const concurrency = parseInt(document.getElementById('concurrency').value, 10) || 5;

  targets.forEach(({ row }) => resetRowStatus(row));

  const testOne = async (target) => {
    const { row, model, channelId, protocolTransform } = target;
    const selectedProtocol = protocolTransform;
    row.querySelector('.response').textContent = i18nText('modelTest.testing', '测试中...');

    try {
      const data = await fetchDataWithAuth(`/admin/channels/${channelId}/test`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ model, stream: streamEnabled, content, protocol_transform: selectedProtocol })
      });
      applyTestResultToRow(row, data);
    } catch (e) {
      row.style.background = 'rgba(239, 68, 68, 0.1)';
      row.querySelector('.first-byte-duration').textContent = '-';
      row.querySelector('.duration').textContent = '-';
      row.querySelector('.speed').textContent = '-';
      row.querySelector('.response').textContent = i18nText('modelTest.requestFailed', '请求失败');
      row.querySelector('.response').title = e.message;
      row.querySelector('.cost').textContent = '-';
    }
  };

  const queue = [...targets];
  const workers = Array(Math.min(concurrency, queue.length)).fill(null).map(async () => {
    while (queue.length) {
      const next = queue.shift();
      if (!next) break;
      await testOne(next);
    }
  });

  await Promise.all(workers);

  document.querySelectorAll('#model-test-tbody tr').forEach(row => {
    const checkbox = row.querySelector('.row-checkbox');
    if (!checkbox) return;
    checkbox.checked = row.style.background.includes('239, 68, 68');
  });

  applyCurrentSort();
  syncSelectAllCheckbox();
}

function setRunTestButtonDisabled(disabled) {
  const runTestBtn = getRunTestBtn();
  if (!runTestBtn) return;

  runTestBtn.disabled = disabled;
  runTestBtn.setAttribute('aria-disabled', disabled ? 'true' : 'false');
  runTestBtn.classList.toggle('is-disabled', disabled);

  if (disabled) {
    if (!runTestBtn.dataset.originalText) {
      runTestBtn.dataset.originalText = runTestBtn.textContent || i18nText('modelTest.startTest', '开始测试');
    }
    runTestBtn.textContent = i18nText('modelTest.testing', '测试中...');
    return;
  }

  runTestBtn.textContent = runTestBtn.dataset.originalText || i18nText('modelTest.startTest', '开始测试');
}

async function runModelTests() {
  if (isTestingModels) return;

  if (testMode === TEST_MODE_CHANNEL && !selectedChannel) {
    showError(i18nText('modelTest.selectChannelFirst', '请先选择渠道'));
    return;
  }

  if (testMode === TEST_MODE_MODEL && !selectedModelName) {
    showError(i18nText('modelTest.selectModelFirst', '请先选择模型'));
    return;
  }

  const targets = getSelectedTargets();
  if (targets.length === 0) {
    showError(i18nText('modelTest.selectAtLeastOne', '请至少选择一条记录'));
    return;
  }

  isTestingModels = true;
  clearProgress();
  setRunTestButtonDisabled(true);
  try {
    await runBatchTests(targets);
  } catch (error) {
    console.error('runModelTests failed:', error);
    showError(i18nText('modelTest.testRunFailed', '测试执行失败'));
  } finally {
    isTestingModels = false;
    clearProgress();
    setRunTestButtonDisabled(false);
  }
}

function selectAllModels() {
  getVisibleRowCheckboxes().forEach(cb => {
    cb.checked = true;
  });
  syncSelectAllCheckbox();
}

function deselectAllModels() {
  getVisibleRowCheckboxes().forEach(cb => {
    cb.checked = false;
  });
  syncSelectAllCheckbox();
}

function toggleAllModels(checked) {
  getVisibleRowCheckboxes().forEach(cb => {
    cb.checked = checked;
  });
  syncSelectAllCheckbox();
}

function getSelectedModelsForDelete() {
  if (testMode === TEST_MODE_MODEL) {
    return Array.from(document.querySelectorAll('#model-test-tbody tr[data-channel-id][data-model]'))
      .map(row => {
        if (!isDataRowVisible(row)) return null;
        const checkbox = row.querySelector('.row-checkbox');
        if (!checkbox || !checkbox.checked) return null;

        const channelId = parseInt(row.dataset.channelId, 10);
        if (!Number.isFinite(channelId)) return null;

        return {
          channelId,
          model: row.dataset.model || selectedModelName,
          row
        };
      })
      .filter(Boolean);
  }

  if (!selectedChannel) return [];

  return Array.from(document.querySelectorAll('#model-test-tbody tr[data-model]'))
    .map(row => {
      if (!isDataRowVisible(row)) return null;
      const checkbox = row.querySelector('.model-checkbox');
      if (!checkbox || !checkbox.checked) return null;
      return {
        channelId: selectedChannel.id,
        model: row.dataset.model,
        row
      };
    })
    .filter(Boolean);
}

function ensureDeleteContext() {
  if (testMode === TEST_MODE_CHANNEL && !selectedChannel) {
    showError(i18nText('modelTest.selectChannelFirst', '请先选择渠道'));
    return false;
  }

  return true;
}

function formatDeleteFailDetails(failed, maxItems = 5) {
  const items = failed.map(item => {
    const channel = channelsList.find(ch => ch.id === item.channelId);
    const channelName = channel ? channel.name : i18nText('common.unknown', '未知渠道');
    return `${channelName}(#${item.channelId}): ${item.error}`;
  });

  if (items.length <= maxItems) {
    return items.join('; ');
  }

  const shown = items.slice(0, maxItems);
  const hiddenCount = items.length - maxItems;
  const moreText = i18nText('modelTest.moreFailures', `其余 ${hiddenCount} 条已省略`, { count: hiddenCount });
  return `${shown.join('; ')}; ${moreText}`;
}

function formatDeletePlanPreview(deletePlan, maxChannels = 8, maxModelsPerChannel = 5) {
  const entries = Array.from(deletePlan.entries());
  const lines = [];

  entries.slice(0, maxChannels).forEach(([channelId, modelSet]) => {
    const channel = channelsList.find(ch => ch.id === channelId);
    const channelName = channel ? channel.name : i18nText('common.unknown', '未知渠道');

    const models = Array.from(modelSet);
    const visibleModels = models.slice(0, maxModelsPerChannel);
    const hiddenModelCount = Math.max(0, models.length - visibleModels.length);
    const moreModelsText = hiddenModelCount > 0
      ? ` ${i18nText('modelTest.moreModels', `等${hiddenModelCount}个模型`, { count: hiddenModelCount })}`
      : '';

    lines.push(`- ${channelName}(#${channelId}): ${visibleModels.join(', ')}${moreModelsText}`);
  });

  const hiddenChannelCount = Math.max(0, entries.length - lines.length);
  if (hiddenChannelCount > 0) {
    lines.push(i18nText('modelTest.moreChannels', `其余 ${hiddenChannelCount} 个渠道已省略`, { count: hiddenChannelCount }));
  }

  return lines.join('\n');
}

function showDeletePreviewModal(previewText, onConfirmAsync) {
  return new Promise((resolve) => {
    if (!deletePreviewModal || !deletePreviewContent || !deletePreviewConfirmBtn || !deletePreviewCancelBtn || !deletePreviewCloseBtn || !deletePreviewProgress || !deletePreviewRuntimeLog) {
      resolve(false);
      return;
    }

    deletePreviewContent.textContent = previewText;
    deletePreviewContent.style.display = '';
    deletePreviewProgress.style.display = 'none';
    deletePreviewRuntimeLog.style.display = 'none';
    deletePreviewRuntimeLog.textContent = '';
    deletePreviewModal.classList.add('show');

    let settled = false;
    let busy = false;
    const originalConfirmText = deletePreviewConfirmBtn.textContent;

    const setBusy = (value) => {
      busy = value;
      deletePreviewConfirmBtn.disabled = value;
      deletePreviewCancelBtn.disabled = value;
      deletePreviewCloseBtn.disabled = value;

      deletePreviewConfirmBtn.textContent = value
        ? i18nText('modelTest.deletePreviewProcessing', '删除中...')
        : originalConfirmText;

      if (value) {
        deletePreviewProgress.style.display = '';
        deletePreviewRuntimeLog.style.display = '';
        deletePreviewContent.style.display = 'none';
      } else {
        deletePreviewContent.style.display = '';
      }
    };

    const cleanup = () => {
      setBusy(false);
      deletePreviewModal.classList.remove('show');
      deletePreviewConfirmBtn.removeEventListener('click', onConfirm);
      deletePreviewCancelBtn.removeEventListener('click', onCancel);
      deletePreviewCloseBtn.removeEventListener('click', onCancel);
      deletePreviewModal.removeEventListener('click', onMaskClick);
      document.removeEventListener('keydown', onEsc);
    };

    const finish = (result) => {
      if (settled) return;
      settled = true;
      cleanup();
      resolve(result);
    };

    const onConfirm = async () => {
      if (busy) return;

      if (typeof onConfirmAsync !== 'function') {
        finish(true);
        return;
      }

      setBusy(true);
      try {
        await onConfirmAsync({
          setProgress: (text) => {
            deletePreviewProgress.textContent = text;
          },
          appendLog: (text) => {
            if (!text) return;
            if (deletePreviewRuntimeLog.textContent && deletePreviewRuntimeLog.textContent !== '-') {
              deletePreviewRuntimeLog.textContent += `\n${text}`;
            } else {
              deletePreviewRuntimeLog.textContent = text;
            }
            deletePreviewRuntimeLog.scrollTop = deletePreviewRuntimeLog.scrollHeight;
          }
        });
        finish(true);
      } catch (error) {
        setBusy(false);
        showError(error?.message || i18nText('common.error', '错误'));
      }
    };
    const onCancel = () => {
      if (busy) return;
      finish(false);
    };
    const onMaskClick = (event) => {
      if (busy) return;
      if (event.target === deletePreviewModal) {
        finish(false);
      }
    };
    const onEsc = (event) => {
      if (busy) return;
      if (event.key === 'Escape') {
        finish(false);
      }
    };

    deletePreviewConfirmBtn.addEventListener('click', onConfirm);
    deletePreviewCancelBtn.addEventListener('click', onCancel);
    deletePreviewCloseBtn.addEventListener('click', onCancel);
    deletePreviewModal.addEventListener('click', onMaskClick);
    document.addEventListener('keydown', onEsc);
  });
}

async function executeDeletePlan(deletePlan, progress = null) {
  const failed = [];
  let successCount = 0;
  const totalChannelCount = deletePlan.size;
  let completed = 0;

  const notifyProgress = (text) => {
    if (progress && typeof progress.setProgress === 'function') {
      progress.setProgress(text);
    }
  };

  const appendLog = (text) => {
    if (progress && typeof progress.appendLog === 'function') {
      progress.appendLog(text);
    }
  };

  notifyProgress(i18nText(
    'modelTest.deleteProgressRunning',
    `删除中 0/${totalChannelCount}`,
    { completed: 0, total: totalChannelCount }
  ));

  for (const [channelId, modelSet] of deletePlan.entries()) {
    const models = Array.from(modelSet);
    if (models.length === 0) continue;

    const channel = channelsList.find(ch => ch.id === channelId);
    const channelName = channel ? channel.name : i18nText('common.unknown', '未知渠道');
    appendLog(i18nText('modelTest.deleteProgressChannelStart', `开始处理 ${channelName}(#${channelId})`, {
      channel_name: channelName,
      channel_id: channelId
    }));

    try {
      const resp = await fetchAPIWithAuth(`/admin/channels/${channelId}/models`, {
        method: 'DELETE',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ models })
      });

      if (!resp.success) {
        failed.push({ channelId, error: resp.error || i18nText('common.deleteFailed', '删除失败') });
        appendLog(i18nText('modelTest.deleteProgressChannelFailed', `${channelName}(#${channelId}) 删除失败`, {
          channel_name: channelName,
          channel_id: channelId,
          error: resp.error || i18nText('common.deleteFailed', '删除失败')
        }));
        completed++;
        notifyProgress(i18nText(
          'modelTest.deleteProgressRunning',
          `删除中 ${completed}/${totalChannelCount}`,
          { completed, total: totalChannelCount }
        ));
        continue;
      }

      successCount++;
      if (channel) {
        channel.models = (channel.models || []).filter(entry => !modelSet.has(getModelName(entry)));
      }
      if (selectedChannel && selectedChannel.id === channelId && channel) {
        selectedChannel = channel;
      }
      appendLog(i18nText('modelTest.deleteProgressChannelDone', `${channelName}(#${channelId}) 删除完成`, {
        channel_name: channelName,
        channel_id: channelId
      }));
    } catch (e) {
      failed.push({ channelId, error: e.message || i18nText('common.deleteFailed', '删除失败') });
      appendLog(i18nText('modelTest.deleteProgressChannelFailed', `${channelName}(#${channelId}) 删除失败`, {
        channel_name: channelName,
        channel_id: channelId,
        error: e.message || i18nText('common.deleteFailed', '删除失败')
      }));
    }

    completed++;
    notifyProgress(i18nText(
      'modelTest.deleteProgressRunning',
      `删除中 ${completed}/${totalChannelCount}`,
      { completed, total: totalChannelCount }
    ));
  }

  notifyProgress(i18nText(
    'modelTest.deleteProgressDone',
    `删除完成 ${totalChannelCount}/${totalChannelCount}`,
    { completed: totalChannelCount, total: totalChannelCount }
  ));

  return { failed, successCount, totalChannelCount };
}

function parseBatchModelInput(value) {
  const seen = new Set();
  return String(value || '')
    .split(/[,\n]+/)
    .map(item => item.trim())
    .filter(Boolean)
    .filter((modelName) => {
      const key = modelName.toLowerCase();
      if (seen.has(key)) return false;
      seen.add(key);
      return true;
    });
}

function buildModelEntriesFromNames(modelNames) {
  return modelNames.map(modelName => ({
    model: modelName,
    redirect_model: ''
  }));
}

function appendModelsToChannelCache(channel, modelNames) {
  if (!channel) return 0;
  if (!Array.isArray(channel.models)) {
    channel.models = [];
  }

  const existing = new Set(
    channel.models
      .map(entry => getModelName(entry))
      .filter(Boolean)
      .map(modelName => modelName.toLowerCase())
  );

  let addedCount = 0;
  modelNames.forEach((modelName) => {
    const key = modelName.toLowerCase();
    if (existing.has(key)) return;

    channel.models.push({
      model: modelName,
      redirect_model: ''
    });
    existing.add(key);
    addedCount++;
  });

  return addedCount;
}

function getVisibleChannelTargetsForAdd() {
  if (testMode !== TEST_MODE_MODEL) return [];

  return Array.from(document.querySelectorAll('#model-test-tbody tr[data-channel-id][data-model]'))
    .filter(row => isDataRowVisible(row))
    .map(row => {
      const checkbox = row.querySelector('.row-checkbox');
      if (!checkbox || !checkbox.checked) return null;

      const channelId = parseInt(row.dataset.channelId, 10);
      if (!Number.isFinite(channelId)) return null;

      const channel = channelsList.find(ch => ch.id === channelId);
      if (!channel) return null;

      return { channelId, channel, row };
    })
    .filter(Boolean);
}

function formatAddFailDetails(failed, maxItems = 5) {
  return formatDeleteFailDetails(failed, maxItems);
}

async function executeAddModelsToChannels(modelNames, targets) {
  const failed = [];
  let successCount = 0;
  let addedModelCount = 0;
  const modelEntries = buildModelEntriesFromNames(modelNames);

  for (const target of targets) {
    try {
      const resp = await fetchAPIWithAuth(`/admin/channels/${target.channelId}/models`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ models: modelEntries })
      });

      if (!resp.success) {
        failed.push({
          channelId: target.channelId,
          error: resp.error || i18nText('modelTest.saveModelsFailed', '保存模型失败')
        });
        continue;
      }

      successCount++;
      addedModelCount += appendModelsToChannelCache(target.channel, modelNames);
    } catch (error) {
      failed.push({
        channelId: target.channelId,
        error: error?.message || i18nText('modelTest.saveModelsFailed', '保存模型失败')
      });
    }
  }

  return {
    failed,
    successCount,
    addedModelCount,
    totalChannelCount: targets.length
  };
}

function setAddModelsModalBusy(value) {
  isAddingModels = value;
  if (addModelsTextarea) {
    addModelsTextarea.disabled = value;
  }
  if (addModelsConfirmBtn) {
    if (!addModelsConfirmBtn.dataset.originalText) {
      addModelsConfirmBtn.dataset.originalText = addModelsConfirmBtn.textContent || i18nText('modelTest.addModelsConfirm', '确认添加');
    }
    addModelsConfirmBtn.disabled = value;
    addModelsConfirmBtn.textContent = value
      ? i18nText('modelTest.addModelsProcessing', '添加中...')
      : addModelsConfirmBtn.dataset.originalText;
  }
  if (addModelsCancelBtn) {
    addModelsCancelBtn.disabled = value;
  }
  if (addModelsCloseBtn) {
    addModelsCloseBtn.disabled = value;
  }
}

function closeAddModelsModal() {
  if (isAddingModels) return;
  addModelsModal?.classList.remove('show');
}

function openAddModelsModal() {
  if (testMode !== TEST_MODE_MODEL) return;

  const targets = getVisibleChannelTargetsForAdd();
  if (targets.length === 0) {
    showError(i18nText('modelTest.addModelsNoChannels', '当前表格没有可添加模型的渠道'));
    return;
  }

  if (addModelsTextarea) {
    addModelsTextarea.value = '';
  }
  addModelsModal?.classList.add('show');
  setTimeout(() => addModelsTextarea?.focus(), 0);
}

async function confirmAddModelsFromModal() {
  if (isAddingModels) return;

  const modelNames = parseBatchModelInput(addModelsTextarea?.value || '');
  if (modelNames.length === 0) {
    showError(i18nText('modelTest.addModelsEmpty', '请输入要添加的模型'));
    return;
  }

  const targets = getVisibleChannelTargetsForAdd();
  if (targets.length === 0) {
    showError(i18nText('modelTest.addModelsNoChannels', '当前表格没有可添加模型的渠道'));
    return;
  }

  setAddModelsModalBusy(true);
  try {
    const result = await executeAddModelsToChannels(modelNames, targets);
    setAddModelsModalBusy(false);

    populateModelSelector();
    renderModelModeRows();
    closeAddModelsModal();

    if (result.failed.length === 0) {
      showSuccess(i18nText(
        'modelTest.addSuccessSummary',
        `添加完成：成功 ${result.successCount} 个渠道，失败 0 个渠道`,
        {
          success_channels: result.successCount,
          failed_channels: 0,
          total_channels: result.totalChannelCount,
          added_models: result.addedModelCount
        }
      ));
      return;
    }

    const failDetails = formatAddFailDetails(result.failed);
    if (result.successCount > 0) {
      showError(i18nText(
        'modelTest.addPartialFailed',
        `添加完成：成功 ${result.successCount} 个渠道，失败 ${result.failed.length} 个渠道。失败详情：${failDetails}`,
        {
          success_channels: result.successCount,
          failed_channels: result.failed.length,
          total_channels: result.totalChannelCount,
          added_models: result.addedModelCount,
          details: failDetails
        }
      ));
      return;
    }

    showError(i18nText(
      'modelTest.addAllFailed',
      `添加失败：共 ${result.totalChannelCount} 个渠道，全部失败。失败详情：${failDetails}`,
      {
        failed_channels: result.failed.length,
        total_channels: result.totalChannelCount,
        details: failDetails
      }
    ));
  } catch (error) {
    setAddModelsModalBusy(false);
    showError(error?.message || i18nText('modelTest.saveModelsFailed', '保存模型失败'));
  }
}

async function fetchAndAddModels() {
  if (!selectedChannel) {
    showError(i18nText('modelTest.selectChannelFirst', '请先选择渠道'));
    return;
  }

  const channelType = getChannelType(selectedChannel);
  try {
    const resp = await fetchAPIWithAuth(`/admin/channels/${selectedChannel.id}/models/fetch?channel_type=${channelType}`);
    if (!resp.success || !resp.data?.models) {
      showError(resp.error || i18nText('modelTest.fetchModelsFailed', '获取模型失败'));
      return;
    }

    const existingNames = new Set((selectedChannel.models || []).map(e => getModelName(e)));
    const fetched = resp.data.models;
    const newOnes = fetched.filter(entry => {
      const name = getModelName(entry);
      return name && !existingNames.has(name);
    });

    if (newOnes.length > 0) {
      const saveResp = await fetchAPIWithAuth(`/admin/channels/${selectedChannel.id}/models`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ models: newOnes })
      });
      if (!saveResp.success) throw new Error(saveResp.error || i18nText('modelTest.saveModelsFailed', '保存模型失败'));
    }

    selectedChannel.models = [...(selectedChannel.models || []), ...newOnes];
    renderChannelModeRows();
    showSuccess(i18nText('modelTest.fetchModelsResult', `获取到 ${fetched.length} 个模型，新增 ${newOnes.length} 个`, {
      total: fetched.length,
      added: newOnes.length
    }));
  } catch (e) {
    showError(e.message || i18nText('modelTest.fetchModelsFailed', '获取模型失败'));
  }
}

async function deleteSelectedModels() {
  if (isDeletingModels) return;
  if (!ensureDeleteContext()) return;

  const selected = getSelectedModelsForDelete();
  if (selected.length === 0) {
    showError(i18nText('modelTest.selectModelToDelete', '请先选择要删除的模型'));
    return;
  }

  const deletePlan = new Map();
  selected.forEach(item => {
    if (!deletePlan.has(item.channelId)) {
      deletePlan.set(item.channelId, new Set());
    }
    if (item.model) deletePlan.get(item.channelId).add(item.model);
  });

  const deletePreview = formatDeletePlanPreview(deletePlan);
  const deletePreviewDesc = document.querySelector('#deletePreviewModal [data-i18n="modelTest.deletePreviewDesc"]');
  if (deletePreviewDesc) {
    deletePreviewDesc.textContent = i18nText(
      'modelTest.deletePreviewDescWithCount',
      `将删除 ${selected.length} 条记录，涉及 ${deletePlan.size} 个渠道：`,
      {
        record_count: selected.length,
        channel_count: deletePlan.size
      }
    );
  }
  let deleteResult = null;
  isDeletingModels = true;
  const deleteModelsBtn = getDeleteModelsBtn();
  if (deleteModelsBtn) {
    deleteModelsBtn.disabled = true;
  }

  const confirmed = await showDeletePreviewModal(deletePreview, async (modalProgress) => {
    deleteResult = await executeDeletePlan(deletePlan, modalProgress);
  });

  isDeletingModels = false;
  if (deleteModelsBtn) {
    deleteModelsBtn.disabled = false;
  }

  if (!confirmed) {
    return;
  }
  if (!deleteResult) {
    showError(i18nText('common.error', '错误'));
    return;
  }

  const { failed, successCount, totalChannelCount } = deleteResult;

  const failedChannelIds = new Set(failed.map(f => f.channelId));
  selected.forEach(item => {
    if (failedChannelIds.has(item.channelId)) return;
    item.row.remove();
  });

  if (testMode === TEST_MODE_MODEL) {
    populateModelSelector();
  }

  const hasDataRows = Array.from(tbody.querySelectorAll('tr')).some(r => !r.querySelector('td[colspan]'));
  if (!hasDataRows) {
    renderRowsByMode();
  } else {
    markRowBaseOrder();
    syncSelectAllCheckbox();
  }

  if (failed.length === 0) {
    showSuccess(i18nText(
      'modelTest.deleteSuccessSummary',
      `删除完成：成功 ${successCount} 个渠道，失败 0 个渠道`,
      {
        success_channels: successCount,
        failed_channels: 0,
        total_channels: totalChannelCount
      }
    ));
    return;
  }

  const failDetails = formatDeleteFailDetails(failed);

  if (successCount > 0) {
    showError(i18nText(
      'modelTest.deletePartialFailed',
      `删除完成：成功 ${successCount} 个渠道，失败 ${failed.length} 个渠道。失败详情：${failDetails}`,
      {
        success_channels: successCount,
        failed_channels: failed.length,
        total_channels: totalChannelCount,
        details: failDetails
      }
    ));
    return;
  }

  showError(i18nText(
    'modelTest.deleteAllFailed',
    `删除失败：共 ${totalChannelCount} 个渠道，全部失败。失败详情：${failDetails}`,
    {
      failed_channels: failed.length,
      total_channels: totalChannelCount,
      details: failDetails
    }
  ));
}

async function onChannelChange() {
  if (!selectedChannel) {
    renderProtocolTransformOptions();
    renderEmptyRow(i18nText('modelTest.selectChannelFirst', '请先选择渠道'));
    return;
  }

  selectedProtocol = getChannelType(selectedChannel);
  renderProtocolTransformOptions();
  populateModelTypeSelect();
  populateModelSelector();

  if (testMode === TEST_MODE_CHANNEL) {
    renderChannelModeRows();
    return;
  }

  renderModelModeRows();
}

function renderSearchableChannelSelect() {
  const initialValue = selectedChannel ? String(selectedChannel.id) : '';
  const initialLabel = selectedChannel ? `[${getChannelType(selectedChannel)}] ${selectedChannel.name}` : '';
  channelSelectCombobox = createSearchableCombobox({
    container: 'testChannelSelectContainer',
    inputId: 'testChannelSelect',
    dropdownId: 'testChannelSelectDropdown',
    placeholder: i18nText('modelTest.searchChannel', '搜索渠道...'),
    minWidth: 250,
    initialValue,
    initialLabel,
    getOptions: () => channelsList.map(ch => ({
      value: String(ch.id),
      label: `[${getChannelType(ch)}] ${ch.name}`
    })),
    onSelect: async (value) => {
      const channelId = parseInt(value, 10);
      selectedChannel = channelsList.find(c => c.id === channelId) || null;
      await onChannelChange();
    }
  });
}

async function loadChannels(options = {}) {
  const { preserveSelection = false } = options;
  const preservedChannelId = preserveSelection ? (selectedChannel?.id ?? null) : null;
  const preservedProtocol = preserveSelection ? selectedProtocol : '';
  const preservedModelType = preserveSelection ? selectedModelType : '';
  const preservedModelName = preserveSelection ? selectedModelName : '';

  try {
    const list = (await fetchDataWithAuth('/admin/channels')) || [];
    channelsList = list.sort((a, b) => getChannelType(a).localeCompare(getChannelType(b)) || b.priority - a.priority);

    if (preserveSelection && preservedChannelId !== null) {
      selectedChannel = channelsList.find(c => c.id === preservedChannelId) || null;
    }
    if (preserveSelection) {
      if (preservedModelType) selectedModelType = preservedModelType;
      if (preservedModelName) selectedModelName = preservedModelName;
    }

    renderSearchableChannelSelect();
    ensureSelectedModelType();

    if (preserveSelection && preservedProtocol) {
      selectedProtocol = preservedProtocol;
    } else {
      selectedProtocol = channelsList[0] ? getChannelType(channelsList[0]) : 'anthropic';
    }

    populateModelTypeSelect();
    renderProtocolTransformOptions();
    populateModelSelector();
    renderRowsByMode();
  } catch (e) {
    console.error('加载渠道列表失败:', e);
    showError(i18nText('modelTest.loadChannelsFailed', '加载渠道列表失败'));
  }
}

async function loadDefaultTestContent() {
  try {
    const settings = await fetchDataWithAuth('/admin/settings');
    if (!Array.isArray(settings)) return;

    const setting = settings.find(s => s.key === 'channel_test_content');
    if (!setting) return;

    const input = document.getElementById('modelTestContent');
    input.value = setting.value;
    input.placeholder = '';
  } catch (e) {
    console.error('加载默认测试内容失败:', e);
  }
}

function bindEvents() {
  ensureModelSelectCombobox();
  const streamEnabled = document.getElementById('streamEnabled');
  if (streamEnabled) {
    streamEnabled.addEventListener('change', () => {
      applyFirstByteVisibility();
    });
  }

  protocolTransformOptions?.addEventListener('change', (event) => {
    const target = event.target;
    if (!(target instanceof HTMLInputElement) || target.name !== 'modelTestProtocolTransform') return;
    if (target.disabled) return;

    selectedProtocol = normalizeProtocol(target.value) || selectedProtocol;
    clearProgress();

    if (testMode === TEST_MODE_MODEL) {
      return;
    }

    renderProtocolTransformOptions();
  });

  if (!modelSelectCombobox && modelSelect) {
    modelSelect.addEventListener('change', () => {
      selectedModelName = getModelInputValue();
      if (testMode === TEST_MODE_MODEL) {
        renderModelModeRows();
      }
    });

    modelSelect.addEventListener('input', () => {
      selectedModelName = getModelInputValue();
      if (testMode === TEST_MODE_MODEL) {
        renderModelModeRows();
      }
    });
  }

  if (modelTypeSelect) {
    modelTypeSelect.addEventListener('change', () => {
      selectedModelType = normalizeProtocol(modelTypeSelect.value) || selectedModelType;
      if (selectedModelType) {
        selectedProtocol = selectedModelType;
      }
      clearProgress();
      renderProtocolTransformOptions();
      populateModelSelector();
      if (testMode === TEST_MODE_MODEL) {
        renderModelModeRows();
      }
    });
  }

  if (mobileNameFilterInput) {
    mobileNameFilterInput.addEventListener('input', () => {
      setNameFilterKeyword(mobileNameFilterInput.value || '');
    });
  }

  addModelsConfirmBtn?.addEventListener('click', () => {
    confirmAddModelsFromModal();
  });
  addModelsCancelBtn?.addEventListener('click', () => {
    closeAddModelsModal();
  });
  addModelsCloseBtn?.addEventListener('click', () => {
    closeAddModelsModal();
  });
  addModelsModal?.addEventListener('click', (event) => {
    if (event.target === addModelsModal) {
      closeAddModelsModal();
    }
  });
  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && addModelsModal?.classList.contains('show')) {
      closeAddModelsModal();
    }
  });

  tbody.addEventListener('click', (event) => {
    // Click on response cell to show upstream detail
    const responseCell = event.target.closest('.response');
    if (responseCell) {
      const row = responseCell.closest('tr');
      if (row && row._upstreamData) {
        showUpstreamDetailModal(row._upstreamData);
        return;
      }
    }

    const channelBtn = event.target.closest('.channel-link[data-channel-id]');
    if (!channelBtn) return;

    const channelId = parseInt(channelBtn.dataset.channelId, 10);
    if (Number.isFinite(channelId) && channelId > 0 && typeof openLogChannelEditor === 'function') {
      openLogChannelEditor(channelId);
    }
  });

  tbody.addEventListener('change', (event) => {
    const target = event.target;
    if (!(target instanceof HTMLInputElement)) return;
    if (!target.classList.contains('row-checkbox')) return;
    syncSelectAllCheckbox();
  });
}

function setTestMode(mode) {
  if (mode !== TEST_MODE_CHANNEL && mode !== TEST_MODE_MODEL) return;
  if (testMode === mode) return;

  testMode = mode;
  clearProgress();
  if (testMode === TEST_MODE_CHANNEL && selectedChannel) {
    selectedProtocol = getChannelType(selectedChannel);
  }
  updateHeadByMode();
  updateModeUI();

  if (testMode === TEST_MODE_MODEL) {
    populateModelSelector();
  }

  renderRowsByMode();
}

window.setTestMode = setTestMode;
window.selectAllModels = selectAllModels;
window.deselectAllModels = deselectAllModels;
window.toggleAllModels = toggleAllModels;
window.runModelTests = runModelTests;
window.fetchAndAddModels = fetchAndAddModels;
window.openAddModelsModal = openAddModelsModal;
window.deleteSelectedModels = deleteSelectedModels;

function tryFormatJSON(str) {
  if (!str) return '';
  try {
    return JSON.stringify(JSON.parse(str), null, 2);
  } catch {
    return str;
  }
}

function formatHeaderLines(headers) {
  if (!headers || typeof headers !== 'object') return '';
  headers = window.maskSensitiveHeaders(headers);
  const lines = [];
  for (const [key, value] of Object.entries(headers)) {
    if (Array.isArray(value)) {
      value.forEach(v => lines.push(`${key}: ${v}`));
    } else {
      lines.push(`${key}: ${value}`);
    }
  }
  return lines.join('\n');
}

function composeRawRequest(data) {
  let parts = [];
  if (data.url) parts.push(data.url);
  const headers = formatHeaderLines(data.requestHeaders);
  if (headers) parts.push(headers);
  const body = tryFormatJSON(data.requestBody);
  if (body) {
    parts.push('');
    parts.push(body);
  }
  return parts.join('\n');
}

function composeRawResponse(data) {
  let parts = [];
  if (data.statusCode != null) parts.push('HTTP ' + data.statusCode);
  const headers = formatHeaderLines(data.responseHeaders);
  if (headers) parts.push(headers);
  const body = tryFormatJSON(data.responseBody);
  if (body) {
    parts.push('');
    parts.push(body);
  }
  return parts.join('\n');
}

function showUpstreamDetailModal(data) {
  if (!data) return;

  window.setHighlightedCodeContent('upstreamReqRaw', composeRawRequest(data), 'request');
  window.setHighlightedCodeContent('upstreamRespRaw', composeRawResponse(data), 'response');

  // Reset to Request tab
  const modal = document.getElementById('upstreamDetailModal');
  modal.querySelectorAll('.upstream-tab').forEach(t => t.classList.toggle('active', t.dataset.tab === 'request'));
  document.getElementById('upstreamTabRequest').classList.add('active');
  document.getElementById('upstreamTabResponse').classList.remove('active');
  const copyBtn = modal.querySelector('.upstream-copy-btn--tabs');
  if (copyBtn) copyBtn.dataset.copyTarget = 'upstreamReqRaw';

  modal.classList.add('show');
}

function closeUpstreamDetailModal() {
  document.getElementById('upstreamDetailModal').classList.remove('show');
}

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    const modal = document.getElementById('upstreamDetailModal');
    if (modal && modal.classList.contains('show')) {
      closeUpstreamDetailModal();
    }
  }
});

// Tab switch + copy button delegation for upstream detail modal
document.addEventListener('click', (e) => {
  const tab = e.target.closest('#upstreamDetailModal .upstream-tab');
  if (tab) {
    const target = tab.dataset.tab;
    document.querySelectorAll('#upstreamDetailModal .upstream-tab').forEach(t => t.classList.toggle('active', t === tab));
    document.getElementById('upstreamTabRequest').classList.toggle('active', target === 'request');
    document.getElementById('upstreamTabResponse').classList.toggle('active', target === 'response');
    const copyBtn = document.querySelector('#upstreamDetailModal .upstream-copy-btn--tabs');
    if (copyBtn) {
      copyBtn.dataset.copyTarget = target === 'response' ? 'upstreamRespRaw' : 'upstreamReqRaw';
    }
    return;
  }

  const copyBtn = e.target.closest('#upstreamDetailModal .upstream-copy-btn');
  if (copyBtn) {
    const targetId = copyBtn.dataset.copyTarget;
    const pre = document.getElementById(targetId);
    if (!pre) return;
    const text = pre._rawText || pre.textContent || '';
    navigator.clipboard.writeText(text).then(() => {
      const orig = copyBtn.textContent;
      copyBtn.textContent = '\u2713';
      copyBtn.classList.add('copied');
      setTimeout(() => { copyBtn.textContent = orig; copyBtn.classList.remove('copied'); }, 1500);
    });
  }
});

async function bootstrap() {
  window.ChannelModalHooks = {
    afterSave: async () => {
      await loadChannels({ preserveSelection: true });
    }
  };
  initModelTestActions();
  bindEvents();
  await loadChannels();
  await loadDefaultTestContent();
  updateHeadByMode();
  updateModeUI();
  renderRowsByMode();
}

window.initPageBootstrap({
  topbarKey: 'model-test',
  run: () => {
    bootstrap();
  }
});
