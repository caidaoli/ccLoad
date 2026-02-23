// 模型测试页面
initTopbar('model-test');
if (window.i18n) window.i18n.translatePage();

const TEST_MODE_CHANNEL = 'channel';
const TEST_MODE_MODEL = 'model';

let channelsList = [];
let selectedChannel = null;
let selectedModelName = '';
let testMode = TEST_MODE_CHANNEL;
let isDeletingModels = false;
let isTestingModels = false;

let channelSelectCombobox = null;
let modelSelectCombobox = null;

const headRow = document.getElementById('model-test-head-row');
const tbody = document.getElementById('model-test-tbody');
const channelSelectorLabel = document.getElementById('channelSelectorLabel');
const modelSelectorLabel = document.getElementById('modelSelectorLabel');
const typeSelect = document.getElementById('testChannelType');
const modelSelect = document.getElementById('testModelSelect');
const fetchModelsBtn = document.getElementById('fetchModelsBtn');
const deleteModelsBtn = document.getElementById('deleteModelsBtn');
const runTestBtn = document.getElementById('runTestBtn');

const deletePreviewModal = document.getElementById('deletePreviewModal');
const deletePreviewContent = document.getElementById('deletePreviewContent');
const deletePreviewProgress = document.getElementById('deletePreviewProgress');
const deletePreviewRuntimeLog = document.getElementById('deletePreviewRuntimeLog');
const deletePreviewCloseBtn = document.getElementById('deletePreviewCloseBtn');
const deletePreviewCancelBtn = document.getElementById('deletePreviewCancelBtn');
const deletePreviewConfirmBtn = document.getElementById('deletePreviewConfirmBtn');

const RESULT_TABLE_COLSPAN_WITH_FIRST_BYTE = 10;
const RESULT_TABLE_COLSPAN_NO_FIRST_BYTE = 9;
const SORT_DIRECTION_ASC = 1;
const SORT_DIRECTION_DESC = -1;
const SORT_DIRECTION_NONE = 0;
let sortState = { key: '', direction: SORT_DIRECTION_NONE };
let nameFilterKeyword = '';

const CHANNEL_MODE_HEAD = `
  <th style="width: 30px;"><input type="checkbox" id="selectAllCheckbox" onchange="toggleAllModels(this.checked)"></th>
  <th style="width: 200px;" data-i18n="common.model" data-sort-key="name">模型</th>
  <th class="first-byte-col" style="width: 76px;" data-i18n="modelTest.firstByteDuration" data-sort-key="firstByteDuration">首字</th>
  <th style="width: 76px;" data-i18n="modelTest.totalDuration" data-sort-key="duration">总耗时</th>
  <th style="width: 65px;" data-i18n="common.input" data-sort-key="inputTokens">输入</th>
  <th style="width: 65px;" data-i18n="common.output" data-sort-key="outputTokens">输出</th>
  <th style="width: 65px;" data-i18n="modelTest.cacheRead" data-sort-key="cacheRead">缓读</th>
  <th style="width: 65px;" data-i18n="modelTest.cacheCreate" data-sort-key="cacheCreate">缓建</th>
  <th style="width: 80px;" data-i18n="common.cost" data-sort-key="cost">费用</th>
  <th data-i18n="modelTest.responseContent" data-sort-key="response">响应内容</th>
`;

const MODEL_MODE_HEAD = `
  <th style="width: 30px;"><input type="checkbox" id="selectAllCheckbox" onchange="toggleAllModels(this.checked)"></th>
  <th style="width: 280px;" data-i18n="modelTest.channelName" data-sort-key="name">渠道</th>
  <th class="first-byte-col" style="width: 76px;" data-i18n="modelTest.firstByteDuration" data-sort-key="firstByteDuration">首字</th>
  <th style="width: 76px;" data-i18n="modelTest.totalDuration" data-sort-key="duration">总耗时</th>
  <th style="width: 65px;" data-i18n="common.input" data-sort-key="inputTokens">输入</th>
  <th style="width: 65px;" data-i18n="common.output" data-sort-key="outputTokens">输出</th>
  <th style="width: 65px;" data-i18n="modelTest.cacheRead" data-sort-key="cacheRead">缓读</th>
  <th style="width: 65px;" data-i18n="modelTest.cacheCreate" data-sort-key="cacheCreate">缓建</th>
  <th style="width: 80px;" data-i18n="common.cost" data-sort-key="cost">费用</th>
  <th data-i18n="modelTest.responseContent" data-sort-key="response">响应内容</th>
`;

function i18nText(key, fallback, params) {
  if (typeof window.t === 'function') {
    const result = window.t(key, params);
    if (result && result !== key) return result;
  }
  return fallback;
}

function formatDurationMs(durationMs) {
  return (typeof durationMs === 'number' && Number.isFinite(durationMs) && durationMs > 0)
    ? `${(durationMs / 1000).toFixed(2)}s`
    : '-';
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

function getNameFilterPlaceholder() {
  if (testMode === TEST_MODE_MODEL) {
    return i18nText('modelTest.filterChannelPlaceholder', '搜索渠道名称...');
  }
  return i18nText('modelTest.filterModelPlaceholder', '搜索模型名称...');
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
      nameFilterKeyword = input.value || '';
      applyNameFilter();
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
  input.placeholder = getNameFilterPlaceholder();
  input.value = nameFilterKeyword;
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
    } else if (indicator.parentElement !== th) {
      th.appendChild(indicator);
    }

    th.style.cursor = 'pointer';
    th.style.whiteSpace = 'nowrap';
    th.style.verticalAlign = 'middle';
    th.onclick = () => {
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
  return channel?.channel_type || 'anthropic';
}

function isModelSupported(channel, modelName) {
  if (!channel || !modelName || !Array.isArray(channel.models)) return false;
  return channel.models.some(entry => getModelName(entry) === modelName);
}

function getAllModelsInType(channelType) {
  const modelSet = new Set();
  channelsList.forEach(ch => {
    if (getChannelType(ch) !== channelType) return;
    (ch.models || []).forEach(entry => {
      const modelName = getModelName(entry);
      if (modelName) modelSet.add(modelName);
    });
  });
  return Array.from(modelSet).sort((a, b) => a.localeCompare(b));
}

function getChannelsSupportingModel(channelType, modelName) {
  return channelsList
    .filter(ch => getChannelType(ch) === channelType && isModelSupported(ch, modelName))
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
      const channelType = typeSelect.value;
      const models = getAllModelsInType(channelType);
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
      nameStyle: ''
    });
    if (row) fragment.appendChild(row);
  });

  tbody.innerHTML = '';
  tbody.appendChild(fragment);
  finalizeTableRender();
}

function populateModelSelector() {
  const channelType = typeSelect.value;
  const models = getAllModelsInType(channelType);
  const typedModel = getModelInputValue();

  if (models.length === 0) {
    selectedModelName = typedModel || '';
    setModelInputValue(selectedModelName);
    modelSelectCombobox?.refresh();
    return;
  }

  if (typedModel) {
    selectedModelName = typedModel;
  } else if (!selectedModelName || !models.includes(selectedModelName)) {
    selectedModelName = models[0];
  }

  setModelInputValue(selectedModelName);
  modelSelectCombobox?.refresh();
}

function renderModelModeRows() {
  const channelType = typeSelect.value;
  if (!channelType) {
    renderEmptyRow(i18nText('modelTest.selectTypeFirst', '请先选择渠道类型'));
    return;
  }

  const models = getAllModelsInType(channelType);
  if (models.length === 0) {
    renderEmptyRow(i18nText('modelTest.noModelInType', '该类型下没有可用模型'));
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

  const channels = getChannelsSupportingModel(channelType, selectedModelName);
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
      model: selectedModelName
    });

    if (row) {
      const checkbox = row.querySelector('.channel-checkbox');
      if (checkbox) checkbox.checked = isEnabled;

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

  const modeTabChannel = document.getElementById('modeTabChannel');
  const modeTabModel = document.getElementById('modeTabModel');
  modeTabChannel.classList.toggle('active', !isModelMode);
  modeTabModel.classList.toggle('active', isModelMode);

  channelSelectorLabel.style.display = isModelMode ? 'none' : 'flex';
  modelSelectorLabel.style.display = isModelMode ? 'flex' : 'none';
  fetchModelsBtn.style.display = isModelMode ? 'none' : '';
  deleteModelsBtn.disabled = false;
  deleteModelsBtn.title = isModelMode ? i18nText('modelTest.deleteBySelectionHint', '按勾选记录删除对应渠道中的模型') : '';

  const typeValue = typeSelect.value;
  if (!isModelMode && selectedChannel) {
    typeSelect.value = getChannelType(selectedChannel);
  }
  if (isModelMode && typeValue) {
    typeSelect.value = typeValue;
  }
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
          channelType: typeSelect.value
        };
      }

      if (!selectedChannel) return null;
      return {
        row,
        model: row.dataset.model,
        channelId: selectedChannel.id,
        channelType: typeSelect.value
      };
    })
    .filter(Boolean);
}

function resetRowStatus(row) {
  row.querySelector('.first-byte-duration').textContent = '-';
  row.querySelector('.duration').textContent = '-';
  row.querySelector('.input-tokens').textContent = '-';
  row.querySelector('.output-tokens').textContent = '-';
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
    row.querySelector('.input-tokens').textContent = usage.input_tokens || usage.prompt_tokens || usage.promptTokenCount || '-';
    row.querySelector('.output-tokens').textContent = usage.output_tokens || usage.completion_tokens || usage.candidatesTokenCount || '-';
    row.querySelector('.cache-read').textContent = usage.cache_read_input_tokens || usage.cached_tokens || '-';
    row.querySelector('.cache-create').textContent = usage.cache_creation_input_tokens || '-';
    row.querySelector('.cost').textContent = (typeof data.cost_usd === 'number') ? formatCost(data.cost_usd) : '-';

    let respText = data.response_text;
    if (!respText && data.api_response?.choices?.[0]?.message) {
      const msg = data.api_response.choices[0].message;
      respText = msg.content || msg.reasoning_content || msg.reasoning || msg.text;
    }
    const successText = respText || i18nText('common.success', '成功');
    row.querySelector('.response').textContent = successText;
    row.querySelector('.response').title = successText;
    return;
  }

  row.style.background = 'rgba(239, 68, 68, 0.1)';
  const errMsg = data.error || i18nText('modelTest.testFailed', '测试失败');
  row.querySelector('.response').textContent = errMsg;
  row.querySelector('.response').title = errMsg;
  row.querySelector('.cost').textContent = '-';
}

async function runBatchTests(targets) {
  const progressEl = document.getElementById('testProgress');
  const streamEnabled = document.getElementById('streamEnabled').checked;
  const content = document.getElementById('modelTestContent').value.trim() || 'hi';
  const concurrency = parseInt(document.getElementById('concurrency').value, 10) || 5;

  let completed = 0;
  const total = targets.length;

  targets.forEach(({ row }) => resetRowStatus(row));

  const testOne = async (target) => {
    const { row, model, channelId, channelType } = target;
    row.querySelector('.response').textContent = i18nText('modelTest.testing', '测试中...');

    try {
      const data = await fetchDataWithAuth(`/admin/channels/${channelId}/test`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ model, stream: streamEnabled, content, channel_type: channelType })
      });
      applyTestResultToRow(row, data);
    } catch (e) {
      row.style.background = 'rgba(239, 68, 68, 0.1)';
      row.querySelector('.first-byte-duration').textContent = '-';
      row.querySelector('.duration').textContent = '-';
      row.querySelector('.response').textContent = i18nText('modelTest.requestFailed', '请求失败');
      row.querySelector('.response').title = e.message;
      row.querySelector('.cost').textContent = '-';
    }

    completed++;
    progressEl.textContent = `${i18nText('modelTest.testingProgress', '测试中')} ${completed}/${total}`;
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

  progressEl.textContent = `${i18nText('modelTest.completedProgress', '完成')} ${total}/${total}`;

  document.querySelectorAll('#model-test-tbody tr').forEach(row => {
    const checkbox = row.querySelector('.row-checkbox');
    if (!checkbox) return;
    checkbox.checked = row.style.background.includes('239, 68, 68');
  });

  applyCurrentSort();
  syncSelectAllCheckbox();
}

function setRunTestButtonDisabled(disabled) {
  if (!runTestBtn) return;

  runTestBtn.disabled = disabled;
  runTestBtn.setAttribute('aria-disabled', disabled ? 'true' : 'false');
  runTestBtn.style.pointerEvents = disabled ? 'none' : '';
  runTestBtn.style.cursor = disabled ? 'not-allowed' : '';
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
  setRunTestButtonDisabled(true);
  try {
    await runBatchTests(targets);
  } catch (error) {
    console.error('runModelTests failed:', error);
    showError(i18nText('modelTest.testRunFailed', '测试执行失败'));
  } finally {
    isTestingModels = false;
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

async function fetchAndAddModels() {
  if (!selectedChannel) {
    showError(i18nText('modelTest.selectChannelFirst', '请先选择渠道'));
    return;
  }

  const channelType = typeSelect.value;
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
  deleteModelsBtn.disabled = true;

  const confirmed = await showDeletePreviewModal(deletePreview, async (modalProgress) => {
    deleteResult = await executeDeletePlan(deletePlan, modalProgress);
  });

  isDeletingModels = false;
  deleteModelsBtn.disabled = false;

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
    renderEmptyRow(i18nText('modelTest.selectChannelFirst', '请先选择渠道'));
    return;
  }

  const channelType = getChannelType(selectedChannel);
  await window.ChannelTypeManager.renderChannelTypeSelect('testChannelType', channelType);

  if (testMode === TEST_MODE_CHANNEL) {
    renderChannelModeRows();
  }
}

function renderSearchableChannelSelect() {
  channelSelectCombobox = createSearchableCombobox({
    container: 'testChannelSelectContainer',
    inputId: 'testChannelSelect',
    dropdownId: 'testChannelSelectDropdown',
    placeholder: i18nText('modelTest.searchChannel', '搜索渠道...'),
    minWidth: 250,
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

async function loadChannels() {
  try {
    const list = (await fetchDataWithAuth('/admin/channels')) || [];
    channelsList = list.sort((a, b) => getChannelType(a).localeCompare(getChannelType(b)) || b.priority - a.priority);
    renderSearchableChannelSelect();

    const firstType = channelsList[0] ? getChannelType(channelsList[0]) : 'anthropic';
    await window.ChannelTypeManager.renderChannelTypeSelect('testChannelType', firstType);

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

  typeSelect.addEventListener('change', async () => {
    if (testMode === TEST_MODE_CHANNEL) {
      return;
    }

    populateModelSelector();
    renderModelModeRows();
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
  updateModeUI();
  updateHeadByMode();

  if (testMode === TEST_MODE_MODEL) {
    populateModelSelector();
  } else if (selectedChannel) {
    typeSelect.value = getChannelType(selectedChannel);
  }

  renderRowsByMode();
}

window.setTestMode = setTestMode;
window.selectAllModels = selectAllModels;
window.deselectAllModels = deselectAllModels;
window.toggleAllModels = toggleAllModels;
window.runModelTests = runModelTests;
window.fetchAndAddModels = fetchAndAddModels;
window.deleteSelectedModels = deleteSelectedModels;

async function bootstrap() {
  bindEvents();
  await loadChannels();
  await loadDefaultTestContent();
  updateModeUI();
  updateHeadByMode();
  renderRowsByMode();
}

bootstrap();
