const TEST_MODE_CHANNEL = 'channel';
const TEST_MODE_MODEL = 'model';
const TEST_MODE_CHAT = 'chat';

// localStorage keys
const STORAGE_KEY_TEST_MODE = 'ccload_model_test_mode';
const STORAGE_KEY_SELECTED_CHANNEL_ID = 'ccload_model_test_channel_id';
const STORAGE_KEY_SELECTED_MODEL_TYPE = 'ccload_model_test_model_type';
const STORAGE_KEY_SELECTED_MODEL_NAME = 'ccload_model_test_model_name';
const STORAGE_KEY_SELECTED_PROTOCOL = 'ccload_model_test_protocol';
const STORAGE_KEY_STREAM_ENABLED = 'ccload_model_test_stream_enabled';
const STORAGE_KEY_CHAT_MODEL = 'ccload_model_test_chat_model';
const STORAGE_KEY_CHAT_CHANNEL_ID = 'ccload_model_test_chat_channel_id';
const STORAGE_KEY_CHAT_STREAM_ENABLED = 'ccload_model_test_chat_stream_enabled';
const STORAGE_KEY_CHAT_THINKING_EFFORT = 'ccload_model_test_chat_thinking_effort';
const STORAGE_KEY_CHAT_BUILTIN_SEARCH = 'ccload_model_test_chat_builtin_search';
const STORAGE_KEY_CHAT_MESSAGES = 'ccload_model_test_chat_messages';

let channelsList = [];
let selectedChannel = null;
let selectedModelType = '';
let selectedModelName = '';
let selectedProtocol = '';
let selectedModelModeProtocol = '';
let testMode = TEST_MODE_CHANNEL;
let isDeletingModels = false;
let isAddingModels = false;
let isTestingModels = false;

// Chat 模式状态
let chatMessages = [];
let chatMessageSummaries = [];
let chatChannel = null;
let chatModel = '';
let isChatSending = false;
let chatChannelCombobox = null;
let chatModelCombobox = null;
let chatThinkingCombobox = null;
let chatThinkingEffort = '';
let chatPendingImages = [];
let chatAdvancedOptions = {
  systemPrompt: '',
  temperature: null,
  topP: null,
  contextMessages: null,
  maxTokens: null
};

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
const chatToolbar = document.getElementById('chatToolbar');

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
const MODEL_MODE_EXTRA_COLSPAN = 2;
const SORT_DIRECTION_ASC = 1;
const SORT_DIRECTION_DESC = -1;
const SORT_DIRECTION_NONE = 0;
const ALL_PROTOCOLS = ['anthropic', 'codex', 'openai', 'gemini'];
let sortState = { key: '', direction: SORT_DIRECTION_NONE };
let nameFilterKeyword = '';
let upstreamMergedVisible = false;

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
  <th class="table-col-priority" data-i18n="channels.table.priority" data-sort-key="priority">优先级</th>
  <th class="table-col-enabled" data-i18n="channels.table.enabled" data-sort-key="enabled">启用</th>
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

const i18nText = window.i18nText;

const ALLOWED_CHAT_MARKDOWN_TAGS = new Set([
  'A', 'BLOCKQUOTE', 'BR', 'CODE', 'DEL', 'EM', 'HR', 'LI', 'OL', 'P',
  'PRE', 'STRONG', 'TABLE', 'TBODY', 'TD', 'TH', 'THEAD', 'TR', 'UL'
]);
const DROP_CHAT_MARKDOWN_CONTENT_TAGS = new Set(['SCRIPT', 'STYLE', 'IFRAME', 'OBJECT', 'EMBED', 'LINK', 'META']);

function sanitizeChatMarkdownHTML(html) {
  const template = document.createElement('template');
  template.innerHTML = String(html || '');
  sanitizeChatMarkdownNode(template.content);
  return template.innerHTML;
}

function sanitizeChatMarkdownNode(root) {
  Array.from(root.childNodes || []).forEach((node) => {
    if (node.nodeType !== 1) return;

    const tagName = node.tagName.toUpperCase();
    if (DROP_CHAT_MARKDOWN_CONTENT_TAGS.has(tagName)) {
      node.remove();
      return;
    }

    sanitizeChatMarkdownNode(node);
    if (!ALLOWED_CHAT_MARKDOWN_TAGS.has(tagName)) {
      unwrapChatMarkdownElement(node);
      return;
    }

    sanitizeChatMarkdownAttributes(node, tagName);
  });
}

function unwrapChatMarkdownElement(element) {
  const parent = element.parentNode;
  if (!parent) {
    element.remove();
    return;
  }
  while (element.firstChild) {
    parent.insertBefore(element.firstChild, element);
  }
  element.remove();
}

function sanitizeChatMarkdownAttributes(element, tagName) {
  Array.from(element.attributes || []).forEach((attr) => {
    const name = attr.name.toLowerCase();
    const value = attr.value;
    const allowed =
      (tagName === 'A' && ((name === 'href' && isSafeChatMarkdownHref(value)) || name === 'title')) ||
      ((tagName === 'TD' || tagName === 'TH') && name === 'align' && /^(left|center|right)$/i.test(value)) ||
      (tagName === 'CODE' && name === 'class' && /^language-[a-z0-9_-]+$/i.test(value));

    if (!allowed) {
      element.removeAttribute(attr.name);
    }
  });

  if (tagName === 'A' && element.hasAttribute('href')) {
    element.setAttribute('target', '_blank');
    element.setAttribute('rel', 'noopener noreferrer');
  }
}

function isSafeChatMarkdownHref(href) {
  const value = String(href || '').trim();
  if (!value) return false;
  if (value.startsWith('#') || value.startsWith('/')) return true;
  try {
    const parsed = new URL(value, window.location.origin);
    return parsed.protocol === 'http:' || parsed.protocol === 'https:' || parsed.protocol === 'mailto:';
  } catch (_) {
    return false;
  }
}

function renderChatMarkdown(target, markdown, options = {}) {
  if (!target) return;

  const text = String(markdown || '');
  if (typeof window.marked !== 'undefined' && typeof window.marked.parse === 'function') {
    target.innerHTML = sanitizeChatMarkdownHTML(window.marked.parse(text));

    // 对代码块应用语法高亮
    if (typeof window.hljs !== 'undefined') {
      target.querySelectorAll('pre code').forEach((block) => {
        window.hljs.highlightElement(block);
      });
    }
  } else {
    target.textContent = text;
  }

  if (options.cursor) {
    const cursor = document.createElement('span');
    cursor.className = 'chat-cursor';
    target.appendChild(cursor);
  }
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

const MODEL_TEST_PRIORITY_MIN = -99999;
const MODEL_TEST_PRIORITY_MAX = 99999;
let modelTestPrioritySaveTimers = new Map();

function normalizeModelTestPriorityValue(value, fallback) {
  const fallbackValue = Number.isFinite(Number(fallback)) ? Number(fallback) : 0;
  const num = Number(value);
  if (!Number.isFinite(num)) return Math.trunc(fallbackValue);
  return Math.max(MODEL_TEST_PRIORITY_MIN, Math.min(MODEL_TEST_PRIORITY_MAX, Math.trunc(num)));
}

function updateLocalModelTestChannelPriority(channelId, priority) {
  if (!Array.isArray(channelsList)) return;
  channelsList.forEach((ch) => {
    if (Number(ch.id) === channelId) ch.priority = priority;
  });
}

async function saveModelTestInlinePriority(input) {
  if (!input) return;
  const channelId = Number(input.dataset.channelId);
  if (!Number.isFinite(channelId) || channelId <= 0) return;

  const originalPriority = normalizeModelTestPriorityValue(input.dataset.originalPriority, 0);
  const nextPriority = normalizeModelTestPriorityValue(input.value, originalPriority);
  input.value = String(nextPriority);
  if (nextPriority === originalPriority) {
    input.classList.remove('is-dirty');
    return;
  }

  input.dataset.originalPriority = String(nextPriority);
  input.disabled = true;

  try {
    await fetchDataWithAuth('/admin/channels/batch-priority', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ updates: [{ id: channelId, priority: nextPriority }] })
    });
    input.classList.remove('is-dirty');
    updateLocalModelTestChannelPriority(channelId, nextPriority);
  } catch (error) {
    console.error('Update channel priority failed:', error);
    input.dataset.originalPriority = String(originalPriority);
    input.value = String(originalPriority);
    input.classList.remove('is-dirty');
  } finally {
    input.disabled = false;
  }
}

function queueModelTestPrioritySave(input, delay = 1000) {
  if (!input) return;
  const channelId = Number(input.dataset.channelId);
  if (!Number.isFinite(channelId) || channelId <= 0) return;
  input.classList.add('is-dirty');
  const existingTimer = modelTestPrioritySaveTimers.get(channelId);
  if (existingTimer) clearTimeout(existingTimer);
  const timer = setTimeout(() => {
    modelTestPrioritySaveTimers.delete(channelId);
    saveModelTestInlinePriority(input);
  }, delay);
  modelTestPrioritySaveTimers.set(channelId, timer);
}

function flushModelTestPrioritySave(input) {
  if (!input) return;
  const channelId = Number(input.dataset.channelId);
  const existingTimer = modelTestPrioritySaveTimers.get(channelId);
  if (existingTimer) {
    clearTimeout(existingTimer);
    modelTestPrioritySaveTimers.delete(channelId);
  }
  return saveModelTestInlinePriority(input);
}

function updateLocalModelTestChannelEnabled(channelId, enabled) {
  if (!Array.isArray(channelsList)) return;
  channelsList.forEach((ch) => {
    if (Number(ch.id) === channelId) ch.enabled = enabled;
  });
}

function applyModelTestRowEnabledStyle(row, enabled) {
  if (!row) return;
  const btn = row.querySelector('.channel-enable-switch');
  if (btn) {
    btn.dataset.enabled = String(enabled);
    btn.setAttribute('aria-checked', String(enabled));
    btn.classList.toggle('channel-enable-switch--on', enabled);
    btn.classList.toggle('channel-enable-switch--off', !enabled);
    btn.title = enabled ? i18nText('channels.toggleDisable', '禁用') : i18nText('channels.toggleEnable', '启用');
    btn.setAttribute('aria-label', btn.title);
  }
  row.style.background = enabled ? '' : 'rgba(148, 163, 184, 0.14)';
  row.style.color = enabled ? '' : 'var(--color-text-secondary)';
}

async function toggleModelTestChannelEnabled(row, channelId, newEnabled) {
  updateLocalModelTestChannelEnabled(channelId, newEnabled);
  applyModelTestRowEnabledStyle(row, newEnabled);

  try {
    const resp = await fetchAPIWithAuth(`/admin/channels/${channelId}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ enabled: newEnabled })
    });
    if (!resp.success) throw new Error(resp.error || 'failed');
  } catch (e) {
    console.error('Toggle channel enabled failed:', e);
    updateLocalModelTestChannelEnabled(channelId, !newEnabled);
    applyModelTestRowEnabledStyle(row, !newEnabled);
  }
}

function normalizeModelTestCostMultiplier(multiplier) {
  const value = Number(multiplier);
  return Number.isFinite(value) && value >= 0 ? value : 1;
}

function buildModelTestCostDisplay(standardCost, multiplier) {
  const cost = Number(standardCost);
  if (!Number.isFinite(cost) || cost <= 0) return null;

  const effectiveCost = cost * normalizeModelTestCostMultiplier(multiplier);
  if (typeof buildCostStackHtml === 'function') {
    return {
      html: buildCostStackHtml(cost, effectiveCost, { tone: 'warning' }),
      effectiveCost
    };
  }

  const hasMultiplier = Math.abs(effectiveCost - cost) >= 1e-9;
  return {
    html: hasMultiplier ? `${formatCost(cost)}/${formatCost(effectiveCost)}` : formatCost(cost),
    effectiveCost
  };
}

function getRowCostMultiplier(row) {
  const rowMultiplier = row?.dataset?.costMultiplier;
  if (rowMultiplier !== undefined && rowMultiplier !== '') {
    return normalizeModelTestCostMultiplier(rowMultiplier);
  }

  const channelId = String(row?.dataset?.channelId || '');
  const channel = channelsList.find(ch => String(ch.id) === channelId);
  return normalizeModelTestCostMultiplier(channel?.cost_multiplier);
}

function pickPositiveTokenCount(...values) {
  for (const value of values) {
    const tokenCount = Number(value);
    if (Number.isFinite(tokenCount) && tokenCount > 0) {
      return tokenCount;
    }
  }
  return null;
}

function calculateTestSpeed(data, usage) {
  const outputTokens = pickPositiveTokenCount(
    usage?.completion_tokens,
    usage?.output_tokens,
    usage?.candidatesTokenCount
  );
  return calculateTokenSpeed(
    outputTokens,
    Number(data?.duration_ms) / 1000,
    Number(data?.first_byte_duration_ms) / 1000
  );
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
  const baseColspan = isFirstByteColumnVisible() ? RESULT_TABLE_COLSPAN_WITH_FIRST_BYTE : RESULT_TABLE_COLSPAN_NO_FIRST_BYTE;
  const modeColspan = testMode === TEST_MODE_MODEL ? MODEL_MODE_EXTRA_COLSPAN : 0;
  return String(baseColspan + modeColspan);
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

function getModelTestResultCellSelectors() {
  return [
    '.first-byte-duration',
    '.duration',
    '.input-tokens',
    '.output-tokens',
    '.speed',
    '.cache-read',
    '.cache-create',
    '.cost',
    '.response'
  ];
}

function captureModelTestTableState() {
  const tableState = new Map();
  Array.from(tbody.querySelectorAll('tr[data-channel-id][data-model]')).forEach((row) => {
    const selectionKey = getRowSelectionKey(row);
    if (!selectionKey || selectionKey === '::') return;

    const cells = {};
    getModelTestResultCellSelectors().forEach((selector) => {
      const cell = row.querySelector(selector);
      if (!cell) return;

      cells[selector] = {
        textContent: cell.textContent || '',
        innerHTML: cell.innerHTML || '',
        title: cell.title || '',
        sortValue: selector === '.cost' ? cell.dataset?.sortValue : undefined
      };
    });

    const checkbox = row.querySelector('.row-checkbox');
    tableState.set(selectionKey, {
      checked: checkbox ? Boolean(checkbox.checked) : undefined,
      background: row.style?.background || '',
      color: row.style?.color || '',
      cells,
      upstreamData: row._upstreamData ? { ...row._upstreamData } : null
    });
  });
  return tableState;
}

function collectModelTestRowsByKey() {
  const rowsByKey = new Map();
  Array.from(tbody.querySelectorAll('tr[data-channel-id][data-model]')).forEach((row) => {
    const selectionKey = getRowSelectionKey(row);
    if (!selectionKey || selectionKey === '::') return;
    rowsByKey.set(selectionKey, row);
  });
  return rowsByKey;
}

function restoreModelTestTableState(rowsByKey, tableState) {
  if (!tableState || typeof tableState.get !== 'function') return;

  const rows = rowsByKey && typeof rowsByKey.forEach === 'function'
    ? rowsByKey
    : collectModelTestRowsByKey();

  rows.forEach((row, selectionKey) => {
    const savedState = tableState.get(selectionKey);
    if (!savedState) return;

    const checkbox = row.querySelector('.row-checkbox');
    if (checkbox && typeof savedState.checked === 'boolean') {
      checkbox.checked = savedState.checked;
    }

    if (row.style) {
      row.style.background = savedState.background || '';
      row.style.color = savedState.color || '';
    }

    Object.entries(savedState.cells || {}).forEach(([selector, cellState]) => {
      const cell = row.querySelector(selector);
      if (!cell) return;

      if (selector === '.cost') {
        cell.innerHTML = cellState.innerHTML || cellState.textContent || '';
        if (cell.dataset) {
          if (cellState.sortValue !== undefined && cellState.sortValue !== null && cellState.sortValue !== '') {
            cell.dataset.sortValue = String(cellState.sortValue);
          } else {
            delete cell.dataset.sortValue;
          }
        }
      } else {
        cell.textContent = cellState.textContent || '';
      }

      cell.title = cellState.title || '';
    });

    const responseCell = row.querySelector('.response');
    if (savedState.upstreamData) {
      row._upstreamData = { ...savedState.upstreamData };
      responseCell?.classList?.add('has-upstream-detail');
    } else {
      row._upstreamData = null;
      responseCell?.classList?.remove('has-upstream-detail');
    }
  });

  if (typeof syncSelectAllCheckbox === 'function') {
    syncSelectAllCheckbox();
  }
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
    mobileLabelPriority: i18nText('channels.table.priority', '优先级'),
    mobileLabelEnabled: i18nText('channels.table.enabled', '启用'),
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
      'run-model-tests': () => runModelTests(),
      'send-chat-message': () => sendChatMessage(),
      'select-chat-image': () => document.getElementById('chatImageInput')?.click(),
      'toggle-chat-builtin-search': () => toggleChatBuiltinSearch(),
      'open-chat-advanced-options': () => openChatAdvancedOptionsModal(),
      'close-chat-advanced-options': () => closeChatAdvancedOptionsModal(),
      'save-chat-advanced-options': () => saveChatAdvancedOptionsFromModal(),
      'clear-chat': () => clearChat(),
      'retry-chat-message': (actionTarget) => retryChatMessage(actionTarget),
      'edit-chat-message': (actionTarget) => editChatMessage(actionTarget),
      'toggle-chat-export-menu': () => toggleChatExportMenu(),
      'export-chat-md': () => { closeChatExportMenu(); exportChatAsMarkdown(); },
      'export-chat-html': () => { closeChatExportMenu(); exportChatAsHTML(); },
      'remove-chat-image': (actionTarget) => removeChatImage(actionTarget.dataset.imageId)
    },
    change: {
      'toggle-all-models': (actionTarget) => toggleAllModels(actionTarget.checked),
      'add-chat-images': (actionTarget) => addChatImageFiles(actionTarget.files)
    }
  });
}

function renderNameFilterInHeader() {
  const nameTh = headRow.querySelector('th[data-sort-key="name"]');
  if (!nameTh) return;
  const filterWidth = testMode === TEST_MODE_MODEL ? '160px' : '130px';
  const labelI18nKey = nameTh.getAttribute('data-i18n') || '';

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
    if (labelI18nKey) {
      label.setAttribute('data-i18n', labelI18nKey);
    }
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
  } else if (labelI18nKey && label && !label.getAttribute('data-i18n')) {
    label.setAttribute('data-i18n', labelI18nKey);
  }

  if (labelI18nKey) {
    nameTh.removeAttribute('data-i18n');
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
    case 'priority': {
      const priorityInput = row.querySelector('.ch-priority-input');
      return parseNumericCellValue(priorityInput ? priorityInput.value : row.querySelector('.channel-priority')?.textContent);
    }
    case 'enabled': {
      const btn = row.querySelector('.channel-enable-switch');
      return btn?.dataset?.enabled === 'true' ? 1 : 0;
    }
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
      {
        const costCell = row.querySelector('.cost');
        const sortValue = parseNumericCellValue(costCell?.dataset?.sortValue);
        return sortValue ?? parseNumericCellValue(costCell?.textContent);
      }
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

function getAllModelsForProtocol(protocol) {
  const normalizedProtocol = normalizeProtocol(protocol);
  const modelSet = new Set();
  channelsList.forEach(ch => {
    const include = channelMatchesModelType(ch) && channelExposesProtocol(ch, normalizedProtocol);
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

function isExactModelInProtocol(protocol, modelName) {
  if (!modelName) return false;
  const target = String(modelName).trim().toLowerCase();
  if (!target) return false;
  return getAllModelsForProtocol(protocol).some(m => m.toLowerCase() === target);
}

function getChannelModelPairsMatching(protocol, keyword) {
  const trimmed = String(keyword || '').trim().toLowerCase();
  if (!trimmed) return [];
  const normalizedProtocol = normalizeProtocol(protocol);
  const pairs = [];
  channelsList
    .filter(ch => channelExposesProtocol(ch, normalizedProtocol))
    .sort((a, b) => b.priority - a.priority || a.name.localeCompare(b.name))
    .forEach(ch => {
      (ch.models || []).forEach(entry => {
        const name = getModelName(entry);
        if (name && name.toLowerCase().includes(trimmed)) {
          pairs.push({ channel: ch, model: name });
        }
      });
    });
  return pairs;
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
    allowCustomInput: true,
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
      const nextModelName = String(value || '').trim();
      const modelChanged = nextModelName !== selectedModelName;
      selectedModelName = nextModelName;
      saveSelectedModelNameToStorage(selectedModelName);
      if (modelChanged && testMode === TEST_MODE_MODEL) {
        renderModelModeRows();
      }
    },
    onCancel: () => {
      selectedModelName = getModelInputValue() || selectedModelName;
      saveSelectedModelNameToStorage(selectedModelName);
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
      costMultiplier: normalizeModelTestCostMultiplier(selectedChannel.cost_multiplier),
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

  // 输入框有用户输入（含模糊关键字）→ 保留；否则当前选择不在新协议下时回退到首项。
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

  if (!selectedModelName) {
    const typedModel = getModelInputValue();
    if (typedModel) {
      selectedModelName = typedModel;
    } else {
      selectedModelName = models[0];
      setModelInputValue(selectedModelName);
    }
  }

  const isExact = isExactModelInProtocol(selectedProtocol, selectedModelName);
  const pairs = isExact
    ? getChannelsSupportingModel(selectedProtocol, selectedModelName)
        .map(ch => ({ channel: ch, model: selectedModelName }))
    : getChannelModelPairsMatching(selectedProtocol, selectedModelName);

  if (pairs.length === 0) {
    renderEmptyRow(i18nText('modelTest.noChannelSupportsModel', '没有渠道支持该模型'));
    return;
  }

  const fragment = document.createDocumentFragment();
  pairs.forEach(({ channel: ch, model }) => {
    const isEnabled = ch.enabled !== false;
    const baseName = isExact ? ch.name : `${ch.name} · ${model}`;
    const channelName = isEnabled
      ? baseName
      : `${baseName} [${i18nText('common.disabled', '已禁用')}]`;
    const priorityValue = (ch.priority !== null && ch.priority !== undefined && Number.isFinite(Number(ch.priority))) ? Number(ch.priority) : 0;

    const row = TemplateEngine.render('tpl-channel-row-by-model', {
      channelId: String(ch.id),
      channelName,
      channelPriority: String(priorityValue),
      channelEnabled: String(isEnabled),
      toggleSwitchClass: isEnabled ? 'channel-enable-switch--on' : 'channel-enable-switch--off',
      toggleTitle: isEnabled ? i18nText('channels.toggleDisable', '禁用') : i18nText('channels.toggleEnable', '启用'),
      costMultiplier: normalizeModelTestCostMultiplier(ch.cost_multiplier),
      model,
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
  const isChatMode = testMode === TEST_MODE_CHAT;
  const fetchModelsBtn = getFetchModelsBtn();
  const addModelsBtn = getAddModelsBtn();
  const deleteModelsBtn = getDeleteModelsBtn();

  const modeTabChannel = document.getElementById('modeTabChannel');
  const modeTabModel = document.getElementById('modeTabModel');
  const modeTabChat = document.getElementById('modeTabChat');
  modeTabChannel.classList.toggle('active', testMode === TEST_MODE_CHANNEL);
  modeTabModel.classList.toggle('active', isModelMode);
  if (modeTabChat) modeTabChat.classList.toggle('active', isChatMode);

  // chat 模式：隐藏测试工具栏与表格，显示对话面板
  const tableContainer = document.querySelector('.model-test-table-container');
  const chatPanel = document.getElementById('chatPanel');
  const modelTestCard = document.getElementById('modelTestCard');
  if (toolbar) toolbar.style.display = isChatMode ? 'none' : '';
  if (tableContainer) tableContainer.style.display = isChatMode ? 'none' : '';
  chatToolbar?.classList.toggle('hidden', !isChatMode);
  if (chatPanel) chatPanel.classList.toggle('hidden', !isChatMode);
  modelTestCard?.classList.toggle('model-test-card--chat-mode', isChatMode);

  if (isChatMode) return;

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
          model: row.dataset.model || selectedModelName,
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
  const costCell = row.querySelector('.cost');
  costCell.textContent = '-';
  if (costCell.dataset) delete costCell.dataset.sortValue;
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
    const usage = data.usage || apiResp.usage || apiResp.usageMetadata || {};
    const inputTokens = pickPositiveTokenCount(usage.prompt_tokens, usage.input_tokens, usage.promptTokenCount) ?? '-';
    const outputTokens = pickPositiveTokenCount(usage.completion_tokens, usage.output_tokens, usage.candidatesTokenCount) ?? '-';
    const testSpeed = calculateTestSpeed(data, usage);
    const speedDisplay = testSpeed === null
      ? '-'
      : testSpeed.toFixed(1);
    row.querySelector('.input-tokens').textContent = inputTokens;
    row.querySelector('.output-tokens').textContent = outputTokens;
    row.querySelector('.speed').textContent = speedDisplay;
    row.querySelector('.cache-read').textContent = usage.cache_read_input_tokens || usage.cached_tokens || '-';
    row.querySelector('.cache-create').textContent = usage.cache_creation_input_tokens || '-';
    const costCell = row.querySelector('.cost');
    const costDisplay = buildModelTestCostDisplay(data.cost_usd, getRowCostMultiplier(row));
    if (costDisplay) {
      costCell.innerHTML = costDisplay.html;
      if (costCell.dataset) costCell.dataset.sortValue = String(costDisplay.effectiveCost);
    } else {
      costCell.textContent = '-';
      if (costCell.dataset) delete costCell.dataset.sortValue;
    }

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
  const costCell = row.querySelector('.cost');
  costCell.textContent = '-';
  if (costCell.dataset) delete costCell.dataset.sortValue;

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

function isRPMLimitedTestResult(data) {
  return !!data && data.rpm_limited === true;
}

function getRPMRetryDelayMs(data) {
  const delayMs = Number(data?.retry_after_ms);
  if (Number.isFinite(delayMs) && delayMs > 0) {
    return Math.ceil(delayMs);
  }
  return 60 * 1000;
}

function sleepModelTest(delayMs) {
  return new Promise(resolve => setTimeout(resolve, delayMs));
}

function markModelTestRPMWait(row, delayMs) {
  const seconds = Math.max(1, Math.ceil(delayMs / 1000));
  const message = i18nText('modelTest.waitingRpmLimit', 'RPM限制，等待 {seconds}s 后重试', { seconds });
  row.style.background = 'rgba(250, 204, 21, 0.14)';
  const responseCell = row.querySelector('.response');
  responseCell.textContent = message;
  responseCell.title = message;
}

async function waitModelTestRPMRetry(row, delayMs) {
  let remainingMs = Math.max(0, Math.ceil(delayMs));
  if (remainingMs <= 0) return;

  markModelTestRPMWait(row, remainingMs);
  while (remainingMs > 0) {
    const stepMs = Math.min(1000, remainingMs);
    await sleepModelTest(stepMs);
    remainingMs -= stepMs;
    if (remainingMs > 0) {
      markModelTestRPMWait(row, remainingMs);
    }
  }
}

async function fetchModelTestWithRPMWait(target, payload) {
  const { row, channelId } = target;

  for (;;) {
    const data = await fetchDataWithAuth(`/admin/channels/${channelId}/test`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload)
    });
    if (!isRPMLimitedTestResult(data)) {
      return data;
    }

    const delayMs = getRPMRetryDelayMs(data);
    await waitModelTestRPMRetry(row, delayMs);
    row.querySelector('.response').textContent = i18nText('modelTest.testing', '测试中...');
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
      const data = await fetchModelTestWithRPMWait(target, { model, stream: streamEnabled, content, protocol_transform: selectedProtocol });
      applyTestResultToRow(row, data);
    } catch (e) {
      row.style.background = 'rgba(239, 68, 68, 0.1)';
      row.querySelector('.first-byte-duration').textContent = '-';
      row.querySelector('.duration').textContent = '-';
      row.querySelector('.speed').textContent = '-';
      row.querySelector('.response').textContent = i18nText('modelTest.requestFailed', '请求失败');
      row.querySelector('.response').title = e.message;
      const costCell = row.querySelector('.cost');
      costCell.textContent = '-';
      if (costCell.dataset) delete costCell.dataset.sortValue;
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
      ? i18nText('modelTest.moreModels', ` 等，共 ${models.length} 个模型`, { total: models.length })
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

function formatModelTestChannelOptionLabel(ch) {
  if (!ch) return '';
  return `[${getChannelType(ch)}] ${ch.name}`;
}

function getModelTestChannelOptionClass(ch) {
  return ch?.enabled === false ? 'filter-dropdown-item--disabled' : '';
}

function renderSearchableChannelSelect() {
  const initialValue = selectedChannel ? String(selectedChannel.id) : '';
  const initialLabel = selectedChannel ? formatModelTestChannelOptionLabel(selectedChannel) : '';
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
      label: formatModelTestChannelOptionLabel(ch),
      className: getModelTestChannelOptionClass(ch)
    })),
    onSelect: async (value) => {
      const channelId = parseInt(value, 10);
      selectedChannel = channelsList.find(c => c.id === channelId) || null;
      saveSelectedChannelIdToStorage(selectedChannel ? selectedChannel.id : null);
      await onChannelChange();
    }
  });
}

async function loadChannels(options = {}) {
  const { preserveSelection = false, preserveTableState = false } = options;
  const preservedChannelId = preserveSelection ? (selectedChannel?.id ?? null) : null;
  const preservedProtocol = preserveSelection ? selectedProtocol : '';
  const preservedModelType = preserveSelection ? selectedModelType : '';
  const preservedModelName = preserveSelection ? selectedModelName : '';
  const preservedTableState = preserveTableState ? captureModelTestTableState() : null;

  try {
    const list = (await fetchDataWithAuth('/admin/channels')) || [];
    channelsList = list.sort((a, b) => getChannelType(a).localeCompare(getChannelType(b)) || b.priority - a.priority);

    // 恢复选择或从 localStorage 加载
    if (preserveSelection && preservedChannelId !== null) {
      selectedChannel = channelsList.find(c => c.id === preservedChannelId) || null;
    } else if (!preserveSelection) {
      const storedChannelId = loadSelectedChannelIdFromStorage();
      if (storedChannelId !== null) {
        selectedChannel = channelsList.find(c => c.id === storedChannelId) || null;
      }
    }

    if (preserveSelection) {
      if (preservedModelType) selectedModelType = preservedModelType;
      if (preservedModelName) selectedModelName = preservedModelName;
    } else {
      const storedModelType = loadSelectedModelTypeFromStorage();
      const storedModelName = loadSelectedModelNameFromStorage();
      if (storedModelType) selectedModelType = storedModelType;
      if (storedModelName) selectedModelName = storedModelName;
    }

    renderSearchableChannelSelect();
    ensureSelectedModelType();

    if (preserveSelection && preservedProtocol) {
      selectedProtocol = preservedProtocol;
    } else if (!preserveSelection) {
      const storedProtocol = loadSelectedProtocolFromStorage();
      if (storedProtocol) {
        selectedProtocol = storedProtocol;
      } else {
        selectedProtocol = channelsList[0] ? getChannelType(channelsList[0]) : 'anthropic';
      }
    } else {
      selectedProtocol = channelsList[0] ? getChannelType(channelsList[0]) : 'anthropic';
    }
    if (testMode === TEST_MODE_MODEL) {
      selectedModelModeProtocol = selectedProtocol;
    }

    populateModelTypeSelect();
    renderProtocolTransformOptions();
    populateModelSelector();
    renderRowsByMode();

    if (preserveTableState) {
      restoreModelTestTableState(collectModelTestRowsByKey(), preservedTableState);
      applyCurrentSort();
      applyNameFilter();
      applyFirstByteVisibility();
    }
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
      saveStreamEnabledToStorage(streamEnabled.checked);
      applyFirstByteVisibility();
    });
  }

  const chatStreamEnabled = document.getElementById('chatStreamEnabled');
  if (chatStreamEnabled) {
    chatStreamEnabled.addEventListener('change', () => {
      saveChatStreamEnabledToStorage(chatStreamEnabled.checked);
    });
  }

  protocolTransformOptions?.addEventListener('change', (event) => {
    const target = event.target;
    if (!(target instanceof HTMLInputElement) || target.name !== 'modelTestProtocolTransform') return;
    if (target.disabled) return;

    selectedProtocol = normalizeProtocol(target.value) || selectedProtocol;
    if (testMode === TEST_MODE_MODEL) {
      selectedModelModeProtocol = selectedProtocol;
    }
    saveSelectedProtocolToStorage(selectedProtocol);
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
      saveSelectedModelTypeToStorage(selectedModelType);
      if (selectedModelType) {
        selectedProtocol = selectedModelType;
        selectedModelModeProtocol = selectedProtocol;
        saveSelectedProtocolToStorage(selectedProtocol);
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
  const chatAdvancedOptionsModal = document.getElementById('chatAdvancedOptionsModal');
  chatAdvancedOptionsModal?.addEventListener('click', (event) => {
    if (event.target === chatAdvancedOptionsModal) {
      closeChatAdvancedOptionsModal();
    }
  });
  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && addModelsModal?.classList.contains('show')) {
      closeAddModelsModal();
    }
    if (event.key === 'Escape' && chatAdvancedOptionsModal?.classList.contains('show')) {
      closeChatAdvancedOptionsModal();
    }
  });

  tbody.addEventListener('click', (event) => {
    // Enable switch toggle
    const enableSwitch = event.target.closest('.channel-enable-switch');
    if (enableSwitch) {
      const row = enableSwitch.closest('tr');
      const channelId = parseInt(enableSwitch.dataset.channelId, 10);
      if (Number.isFinite(channelId) && channelId > 0 && row) {
        const currentEnabled = enableSwitch.dataset.enabled === 'true';
        toggleModelTestChannelEnabled(row, channelId, !currentEnabled);
      }
      return;
    }

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

  tbody.addEventListener('input', (event) => {
    const input = event.target.closest('.ch-priority-input');
    if (!input) return;
    queueModelTestPrioritySave(input);
  });

  tbody.addEventListener('keydown', (event) => {
    const input = event.target.closest('.ch-priority-input');
    if (!input) return;
    if (event.key === 'Enter') {
      event.preventDefault();
      flushModelTestPrioritySave(input);
    } else if (event.key === 'Escape') {
      const originalPriority = normalizeModelTestPriorityValue(input.dataset.originalPriority, 0);
      input.value = String(originalPriority);
      input.classList.remove('is-dirty');
    }
  });

  tbody.addEventListener('focusout', (event) => {
    const input = event.target.closest('.ch-priority-input');
    if (!input) return;
    flushModelTestPrioritySave(input);
  });

  tbody.addEventListener('change', (event) => {
    const target = event.target;
    if (!(target instanceof HTMLInputElement)) return;
    if (!target.classList.contains('row-checkbox')) return;
    syncSelectAllCheckbox();
  });
}

function setTestMode(mode) {
  if (mode !== TEST_MODE_CHANNEL && mode !== TEST_MODE_MODEL && mode !== TEST_MODE_CHAT) return;
  if (testMode === mode) return;

  const previousMode = testMode;
  if (previousMode === TEST_MODE_MODEL && selectedProtocol) {
    selectedModelModeProtocol = selectedProtocol;
  }

  testMode = mode;
  saveTestModeToStorage(mode);
  clearProgress();

  if (testMode === TEST_MODE_CHAT) {
    updateModeUI();
    initChatPanel();
    return;
  }

  if (testMode === TEST_MODE_CHANNEL && selectedChannel) {
    selectedProtocol = getChannelType(selectedChannel);
  }
  updateHeadByMode();
  updateModeUI();

  if (testMode === TEST_MODE_MODEL) {
    if (!selectedModelModeProtocol) {
      selectedModelModeProtocol = loadSelectedProtocolFromStorage() || selectedProtocol;
    }
    if (selectedModelModeProtocol) {
      selectedProtocol = selectedModelModeProtocol;
      renderProtocolTransformOptions();
    }
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

// ===== localStorage 持久化 =====

function saveTestModeToStorage(mode) {
  try {
    localStorage.setItem(STORAGE_KEY_TEST_MODE, mode);
  } catch (_) { /* ignore */ }
}

function loadTestModeFromStorage() {
  try {
    const mode = localStorage.getItem(STORAGE_KEY_TEST_MODE);
    if (mode === TEST_MODE_CHANNEL || mode === TEST_MODE_MODEL || mode === TEST_MODE_CHAT) {
      return mode;
    }
  } catch (_) { /* ignore */ }
  return TEST_MODE_CHANNEL;
}

function saveSelectedChannelIdToStorage(channelId) {
  try {
    if (channelId !== null && Number.isFinite(Number(channelId))) {
      localStorage.setItem(STORAGE_KEY_SELECTED_CHANNEL_ID, String(channelId));
    } else {
      localStorage.removeItem(STORAGE_KEY_SELECTED_CHANNEL_ID);
    }
  } catch (_) { /* ignore */ }
}

function loadSelectedChannelIdFromStorage() {
  try {
    const value = localStorage.getItem(STORAGE_KEY_SELECTED_CHANNEL_ID);
    if (value) {
      const channelId = parseInt(value, 10);
      if (Number.isFinite(channelId)) return channelId;
    }
  } catch (_) { /* ignore */ }
  return null;
}

function saveSelectedModelTypeToStorage(modelType) {
  try {
    if (modelType) {
      localStorage.setItem(STORAGE_KEY_SELECTED_MODEL_TYPE, modelType);
    } else {
      localStorage.removeItem(STORAGE_KEY_SELECTED_MODEL_TYPE);
    }
  } catch (_) { /* ignore */ }
}

function loadSelectedModelTypeFromStorage() {
  try {
    return localStorage.getItem(STORAGE_KEY_SELECTED_MODEL_TYPE) || '';
  } catch (_) { /* ignore */ }
  return '';
}

function saveSelectedModelNameToStorage(modelName) {
  try {
    if (modelName) {
      localStorage.setItem(STORAGE_KEY_SELECTED_MODEL_NAME, modelName);
    } else {
      localStorage.removeItem(STORAGE_KEY_SELECTED_MODEL_NAME);
    }
  } catch (_) { /* ignore */ }
}

function loadSelectedModelNameFromStorage() {
  try {
    return localStorage.getItem(STORAGE_KEY_SELECTED_MODEL_NAME) || '';
  } catch (_) { /* ignore */ }
  return '';
}

function saveSelectedProtocolToStorage(protocol) {
  try {
    if (protocol) {
      localStorage.setItem(STORAGE_KEY_SELECTED_PROTOCOL, protocol);
    } else {
      localStorage.removeItem(STORAGE_KEY_SELECTED_PROTOCOL);
    }
  } catch (_) { /* ignore */ }
}

function loadSelectedProtocolFromStorage() {
  try {
    return localStorage.getItem(STORAGE_KEY_SELECTED_PROTOCOL) || '';
  } catch (_) { /* ignore */ }
  return '';
}

function saveStreamEnabledToStorage(enabled) {
  try {
    localStorage.setItem(STORAGE_KEY_STREAM_ENABLED, enabled ? '1' : '0');
  } catch (_) { /* ignore */ }
}

function loadStreamEnabledFromStorage() {
  try {
    const value = localStorage.getItem(STORAGE_KEY_STREAM_ENABLED);
    if (value === '0') return false;
    if (value === '1') return true;
  } catch (_) { /* ignore */ }
  return true; // 默认开启流式
}

function saveChatModelToStorage(model) {
  try {
    if (model) {
      localStorage.setItem(STORAGE_KEY_CHAT_MODEL, model);
    } else {
      localStorage.removeItem(STORAGE_KEY_CHAT_MODEL);
    }
  } catch (_) { /* ignore */ }
}

function loadChatModelFromStorage() {
  try {
    return localStorage.getItem(STORAGE_KEY_CHAT_MODEL) || '';
  } catch (_) { /* ignore */ }
  return '';
}

function saveChatChannelIdToStorage(channelId) {
  try {
    if (channelId !== null && Number.isFinite(Number(channelId))) {
      localStorage.setItem(STORAGE_KEY_CHAT_CHANNEL_ID, String(channelId));
    } else {
      localStorage.removeItem(STORAGE_KEY_CHAT_CHANNEL_ID);
    }
  } catch (_) { /* ignore */ }
}

function loadChatChannelIdFromStorage() {
  try {
    const value = localStorage.getItem(STORAGE_KEY_CHAT_CHANNEL_ID);
    if (value) {
      const channelId = parseInt(value, 10);
      if (Number.isFinite(channelId)) return channelId;
    }
  } catch (_) { /* ignore */ }
  return null;
}

function saveChatStreamEnabledToStorage(enabled) {
  try {
    localStorage.setItem(STORAGE_KEY_CHAT_STREAM_ENABLED, enabled ? '1' : '0');
  } catch (_) { /* ignore */ }
}

function loadChatStreamEnabledFromStorage() {
  try {
    const value = localStorage.getItem(STORAGE_KEY_CHAT_STREAM_ENABLED);
    if (value === '0') return false;
    if (value === '1') return true;
  } catch (_) { /* ignore */ }
  return true; // 默认开启流式
}

function saveChatThinkingEffortToStorage(effort) {
  try {
    if (effort) {
      localStorage.setItem(STORAGE_KEY_CHAT_THINKING_EFFORT, effort);
    } else {
      localStorage.removeItem(STORAGE_KEY_CHAT_THINKING_EFFORT);
    }
  } catch (_) { /* ignore */ }
}

function loadChatThinkingEffortFromStorage() {
  try {
    return localStorage.getItem(STORAGE_KEY_CHAT_THINKING_EFFORT) || '';
  } catch (_) { /* ignore */ }
  return '';
}

function saveChatBuiltinSearchToStorage(enabled) {
  try {
    localStorage.setItem(STORAGE_KEY_CHAT_BUILTIN_SEARCH, enabled ? '1' : '0');
  } catch (_) { /* ignore */ }
}

function loadChatBuiltinSearchFromStorage() {
  try {
    const value = localStorage.getItem(STORAGE_KEY_CHAT_BUILTIN_SEARCH);
    if (value === '1') return true;
    if (value === '0') return false;
  } catch (_) { /* ignore */ }
  return false; // 默认关闭内置搜索
}

function getChatAdvancedOptionsAPI() {
  return window.ModelTestAdvancedOptions || null;
}

function normalizeChatAdvancedOptions(options) {
  const api = getChatAdvancedOptionsAPI();
  if (api && typeof api.normalizeOptions === 'function') {
    return api.normalizeOptions(options);
  }
  return {
    systemPrompt: '',
    temperature: null,
    topP: null,
    contextMessages: null,
    maxTokens: null
  };
}

function loadChatAdvancedOptionsFromStorage() {
  const api = getChatAdvancedOptionsAPI();
  if (api && typeof api.loadOptions === 'function') {
    return api.loadOptions(localStorage);
  }
  return normalizeChatAdvancedOptions(null);
}

function saveChatAdvancedOptionsToStorage(options) {
  const api = getChatAdvancedOptionsAPI();
  if (api && typeof api.saveOptions === 'function') {
    return api.saveOptions(localStorage, options);
  }
  return normalizeChatAdvancedOptions(options);
}

function chatAdvancedOptionsEnabled(options = chatAdvancedOptions) {
  const normalized = normalizeChatAdvancedOptions(options);
  return Boolean(
    normalized.systemPrompt ||
    normalized.temperature !== null ||
    normalized.topP !== null ||
    (normalized.contextMessages !== null && normalized.contextMessages > 0) ||
    normalized.maxTokens !== null
  );
}

function updateChatAdvancedOptionsButton() {
  const btn = document.getElementById('chatAdvancedOptionsBtn');
  if (!btn) return;
  btn.classList.toggle('active', chatAdvancedOptionsEnabled());
  btn.setAttribute('aria-pressed', chatAdvancedOptionsEnabled() ? 'true' : 'false');
}

function formatChatAdvancedInputValue(value) {
  return value === null || value === undefined ? '' : String(value);
}

function setChatAdvancedOptionsForm(options) {
  const normalized = normalizeChatAdvancedOptions(options);
  const systemPrompt = document.getElementById('chatAdvancedSystemPrompt');
  const temperature = document.getElementById('chatAdvancedTemperature');
  const topP = document.getElementById('chatAdvancedTopP');
  const contextMessages = document.getElementById('chatAdvancedContextMessages');
  const maxTokens = document.getElementById('chatAdvancedMaxTokens');

  if (systemPrompt) systemPrompt.value = normalized.systemPrompt;
  if (temperature) temperature.value = formatChatAdvancedInputValue(normalized.temperature);
  if (topP) topP.value = formatChatAdvancedInputValue(normalized.topP);
  if (contextMessages) contextMessages.value = formatChatAdvancedInputValue(normalized.contextMessages);
  if (maxTokens) maxTokens.value = formatChatAdvancedInputValue(normalized.maxTokens);
}

function collectChatAdvancedOptionsForm() {
  return normalizeChatAdvancedOptions({
    systemPrompt: document.getElementById('chatAdvancedSystemPrompt')?.value || '',
    temperature: document.getElementById('chatAdvancedTemperature')?.value || '',
    topP: document.getElementById('chatAdvancedTopP')?.value || '',
    contextMessages: document.getElementById('chatAdvancedContextMessages')?.value || '',
    maxTokens: document.getElementById('chatAdvancedMaxTokens')?.value || ''
  });
}

function openChatAdvancedOptionsModal() {
  const modal = document.getElementById('chatAdvancedOptionsModal');
  if (!modal) return;
  setChatAdvancedOptionsForm(chatAdvancedOptions);
  modal.classList.add('show');
  setTimeout(() => document.getElementById('chatAdvancedSystemPrompt')?.focus(), 0);
}

function closeChatAdvancedOptionsModal() {
  document.getElementById('chatAdvancedOptionsModal')?.classList.remove('show');
}

function saveChatAdvancedOptionsFromModal() {
  chatAdvancedOptions = saveChatAdvancedOptionsToStorage(collectChatAdvancedOptionsForm());
  updateChatAdvancedOptionsButton();
  closeChatAdvancedOptionsModal();
}

function saveChatMessagesToStorage() {
  try {
    const data = JSON.stringify({
      messages: chatMessages,
      summaries: chatMessageSummaries.slice(0, chatMessages.length),
      model: chatModel,
      channelId: chatChannel?.id || null,
    });
    localStorage.setItem(STORAGE_KEY_CHAT_MESSAGES, data);
  } catch (_) { /* quota exceeded or serialization error */ }
}

function loadChatMessagesFromStorage() {
  try {
    const raw = localStorage.getItem(STORAGE_KEY_CHAT_MESSAGES);
    if (!raw) return null;
    const data = JSON.parse(raw);
    if (!data || !Array.isArray(data.messages)) return null;
    return data;
  } catch (_) {
    return null;
  }
}

function clearChatMessagesFromStorage() {
  localStorage.removeItem(STORAGE_KEY_CHAT_MESSAGES);
}

function cloneChatSummary(summary) {
  if (!summary || typeof summary !== 'object') return null;
  try {
    return JSON.parse(JSON.stringify(summary));
  } catch (_) {
    return null;
  }
}

function normalizeChatMessageSummaries(summaries, messageCount) {
  return Array.from({ length: messageCount }, (_, index) => cloneChatSummary(Array.isArray(summaries) ? summaries[index] : null));
}

function pushChatMessage(message, summary = null) {
  chatMessages.push(message);
  chatMessageSummaries.push(cloneChatSummary(summary));
  return chatMessages.length - 1;
}

function popChatMessage() {
  chatMessages.pop();
  chatMessageSummaries.pop();
}

function trimChatHistory(length) {
  chatMessages = chatMessages.slice(0, length);
  chatMessageSummaries = chatMessageSummaries.slice(0, length);
}

// ===== Chat 模式实现 =====

function getChatThinkingOptions() {
  return [
    { value: '', label: i18nText('modelTest.chat.thinkingDefault', '默认') },
    { value: 'none', label: i18nText('modelTest.chat.thinkingNone', '关闭') },
    { value: 'minimal', label: i18nText('modelTest.chat.thinkingMinimal', '最少') },
    { value: 'low', label: i18nText('modelTest.chat.thinkingLow', '低') },
    { value: 'medium', label: i18nText('modelTest.chat.thinkingMedium', '中') },
    { value: 'high', label: i18nText('modelTest.chat.thinkingHigh', '高') },
    { value: 'xhigh', label: i18nText('modelTest.chat.thinkingXHighMax', '最高 (xhigh/max)') }
  ];
}

function getChatThinkingLabel(value) {
  const normalized = String(value || '').trim();
  const options = getChatThinkingOptions();
  return (options.find(option => option.value === normalized) || options[0]).label;
}

/**
 * 初始化对话面板：创建渠道/模型 combobox，绑定输入框快捷键。
 * 每次切换到 chat 模式时调用；combobox 只创建一次。
 */
function initChatPanel() {
  if (typeof window.createSearchableCombobox !== 'function') return;

  // 模型 combobox（先选模型，渠道随之过滤）
  if (!chatModelCombobox) {
    chatModelCombobox = window.createSearchableCombobox({
      attachMode: true,
      inputId: 'chatModelSelect',
      dropdownId: 'chatModelSelectDropdown',
      allowCustomInput: true,
      initialValue: chatModel,
      initialLabel: chatModel,
      getOptions: () => getAllChatModelOptions().map(m => ({ value: m, label: m })),
      onSelect: (value) => {
        chatModel = String(value || '').trim();
        saveChatModelToStorage(chatModel);
        refreshChatChannelsByModel();
      },
      onCancel: () => {
        const inputEl = document.getElementById('chatModelSelect');
        if (inputEl) {
          const next = inputEl.value.trim();
          if (next && next !== chatModel) {
            chatModel = next;
            saveChatModelToStorage(chatModel);
            refreshChatChannelsByModel();
          }
        }
      }
    });
  } else {
    chatModelCombobox.refresh();
  }

  // 渠道 combobox（仅显示支持当前模型的渠道）
  if (!chatChannelCombobox) {
    chatChannelCombobox = window.createSearchableCombobox({
      container: 'chatChannelSelectContainer',
      inputId: 'chatChannelSelect',
      dropdownId: 'chatChannelSelectDropdown',
      placeholder: i18nText('modelTest.searchChannel', '搜索渠道...'),
      minWidth: 200,
      initialValue: chatChannel ? String(chatChannel.id) : '',
      initialLabel: chatChannel ? formatModelTestChannelOptionLabel(chatChannel) : '',
      getOptions: () => getChannelsForChatModel().map(ch => ({
        value: String(ch.id),
        label: formatModelTestChannelOptionLabel(ch),
        className: getModelTestChannelOptionClass(ch)
      })),
      onSelect: (value) => {
        const channelId = parseInt(value, 10);
        chatChannel = channelsList.find(c => c.id === channelId) || null;
        saveChatChannelIdToStorage(chatChannel ? chatChannel.id : null);
      }
    });
  } else {
    chatChannelCombobox.refresh();
  }

  // 思考等级 combobox（固定枚举，不允许提交自定义显示文本）
  if (!chatThinkingCombobox) {
    chatThinkingCombobox = window.createSearchableCombobox({
      attachMode: true,
      inputId: 'chatThinkingLevel',
      dropdownId: 'chatThinkingLevelDropdown',
      allowCustomInput: false,
      initialValue: chatThinkingEffort,
      initialLabel: getChatThinkingLabel(chatThinkingEffort),
      getOptions: getChatThinkingOptions,
      onSelect: (value) => {
        chatThinkingEffort = String(value || '').trim();
        saveChatThinkingEffortToStorage(chatThinkingEffort);
      },
      onCancel: () => {
        chatThinkingCombobox?.setValue(chatThinkingEffort, getChatThinkingLabel(chatThinkingEffort));
      }
    });
  } else {
    chatThinkingCombobox.refresh();
    chatThinkingCombobox.setValue(chatThinkingEffort, getChatThinkingLabel(chatThinkingEffort));
  }

  // 初始化默认选择：未选模型时自动选第一个，并联动渠道
  if (!chatModel) {
    const allModels = getAllChatModelOptions();
    if (allModels.length > 0) {
      chatModel = allModels[0];
      chatModelCombobox.setValue(chatModel, chatModel);
      saveChatModelToStorage(chatModel);
    }
  }
  refreshChatChannelsByModel();

  // 输入框快捷键（只绑定一次）
  const chatInput = document.getElementById('chatInput');
  if (chatInput && !chatInput._chatBound) {
    chatInput._chatBound = true;
    chatInput.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        sendChatMessage();
      }
    });
    chatInput.addEventListener('input', autoResizeChatInput);
    chatInput.addEventListener('paste', handleChatPaste);
  }

  // Restore persisted chat messages
  const savedChat = loadChatMessagesFromStorage();
  if (savedChat && savedChat.messages.length > 0) {
    chatMessages = savedChat.messages;
    chatMessageSummaries = normalizeChatMessageSummaries(savedChat.summaries, chatMessages.length);
    if (savedChat.model) {
      chatModel = savedChat.model;
      const chatModelInput = document.getElementById('chatModelSelect');
      if (chatModelInput) chatModelInput.value = chatModel;
    }
    renderChatMessages();
  }
}

/** 收集所有渠道的模型并集（去重 + 字母排序），供 chat 模式模型下拉使用 */
function getAllChatModelOptions() {
  const set = new Set();
  channelsList.forEach(ch => {
    (ch.models || []).forEach(entry => {
      const m = getModelName(entry);
      if (m) set.add(m);
    });
  });
  return Array.from(set).sort((a, b) => a.localeCompare(b));
}

/** 当前 chatModel 下可用的渠道列表；未选模型时返回全部渠道 */
function getChannelsForChatModel() {
  if (!chatModel) return channelsList.slice();
  return channelsList.filter(ch => isModelSupported(ch, chatModel));
}

/** 模型变更后刷新渠道下拉，自动选第一个支持该模型的渠道 */
function refreshChatChannelsByModel() {
  if (!chatChannelCombobox) return;
  chatChannelCombobox.refresh();

  const list = getChannelsForChatModel();
  if (!chatChannel || !list.find(c => c.id === chatChannel.id)) {
    chatChannel = list[0] || null;
    saveChatChannelIdToStorage(chatChannel ? chatChannel.id : null);
  }
  if (chatChannel) {
    chatChannelCombobox.setValue(String(chatChannel.id), formatModelTestChannelOptionLabel(chatChannel));
  } else {
    chatChannelCombobox.setValue('', '');
  }
}

function getChatThinkingEffort() {
  return chatThinkingEffort;
}

function isChatBuiltinSearchEnabled() {
  return document.getElementById('chatBuiltinSearchToggle')?.getAttribute('aria-pressed') === 'true';
}

function toggleChatBuiltinSearch() {
  const toggle = document.getElementById('chatBuiltinSearchToggle');
  if (!toggle) return;
  const enabled = toggle.getAttribute('aria-pressed') === 'true';
  const nextEnabled = !enabled;
  toggle.setAttribute('aria-pressed', nextEnabled ? 'true' : 'false');
  saveChatBuiltinSearchToStorage(nextEnabled);
}

function buildChatUserContent(text, images) {
  const trimmedText = String(text || '').trim();
  const normalizedImages = Array.isArray(images) ? images.filter(image => image && image.dataUrl) : [];
  if (normalizedImages.length === 0) {
    return trimmedText;
  }
  const content = [];
  if (trimmedText) {
    content.push({ type: 'text', text: trimmedText });
  }
  normalizedImages.forEach((image) => {
    content.push({
      type: 'image_url',
      image_url: {
        url: image.dataUrl
      }
    });
  });
  return content;
}

function renderChatImagePreviews() {
  const list = document.getElementById('chatImagePreviewList');
  if (!list) return;
  list.innerHTML = '';
  chatPendingImages.forEach((image) => {
    const item = document.createElement('div');
    item.className = 'chat-image-preview-item';

    const img = document.createElement('img');
    img.className = 'chat-image-preview-thumb';
    img.src = image.dataUrl;
    img.alt = image.name || 'image';

    const removeBtn = document.createElement('button');
    removeBtn.type = 'button';
    removeBtn.className = 'chat-image-preview-remove';
    removeBtn.setAttribute('data-action', 'remove-chat-image');
    removeBtn.setAttribute('data-image-id', image.id);
    removeBtn.setAttribute('aria-label', '删除图片');
    removeBtn.textContent = '×';

    item.appendChild(img);
    item.appendChild(removeBtn);
    list.appendChild(item);
  });
}

function removeChatImage(imageID) {
  const normalized = String(imageID || '').trim();
  if (!normalized) return;
  chatPendingImages = chatPendingImages.filter(image => image.id !== normalized);
  renderChatImagePreviews();
}

function readChatImageFile(file) {
  return new Promise((resolve, reject) => {
    if (!(file instanceof File)) {
      reject(new Error('invalid file'));
      return;
    }
    if (!String(file.type || '').startsWith('image/')) {
      reject(new Error('not image'));
      return;
    }
    const reader = new FileReader();
    reader.onload = () => {
      resolve({
        id: `${Date.now()}-${Math.random().toString(16).slice(2)}`,
        name: file.name || 'image',
        mimeType: file.type || 'image/*',
        dataUrl: String(reader.result || '')
      });
    };
    reader.onerror = () => reject(reader.error || new Error('read failed'));
    reader.readAsDataURL(file);
  });
}

async function addChatImageFiles(files) {
  const list = Array.from(files || []).filter(Boolean);
  if (list.length === 0) return;
  const nextImages = [];
  for (const file of list) {
    try {
      nextImages.push(await readChatImageFile(file));
    } catch (_) {
      continue;
    }
  }
  if (nextImages.length === 0) return;
  chatPendingImages = chatPendingImages.concat(nextImages);
  renderChatImagePreviews();
  const input = document.getElementById('chatImageInput');
  if (input) input.value = '';
}

function handleChatPaste(event) {
  const files = Array.from(event?.clipboardData?.files || []).filter(file => String(file.type || '').startsWith('image/'));
  if (files.length === 0) return;
  event.preventDefault();
  addChatImageFiles(files);
}

function renderChatUserContent(target, content) {
  if (!target) return;
  if (typeof content === 'string') {
    target.textContent = content;
    return;
  }
  if (!Array.isArray(content)) {
    target.textContent = String(content || '');
    return;
  }
  target.textContent = '';
  content.forEach((block) => {
    if (!block || typeof block !== 'object') return;
    if (block.type === 'text') {
      const textEl = document.createElement('div');
      textEl.textContent = String(block.text || '');
      target.appendChild(textEl);
      return;
    }
    if (block.type === 'image_url') {
      const url = String(block.image_url?.url || '');
      if (!url) return;
      const img = document.createElement('img');
      img.className = 'chat-image-preview-thumb';
      img.src = url;
      img.alt = 'user image';
      target.appendChild(img);
    }
  });
}

function cloneChatMessageContent(content) {
  if (typeof content === 'string') return content;
  try {
    return JSON.parse(JSON.stringify(content));
  } catch (_) {
    return extractChatMessageRawText(content);
  }
}

function extractChatMessageImages(content) {
  if (!Array.isArray(content)) return [];
  return content
    .filter(block => block && block.type === 'image_url' && typeof block.image_url?.url === 'string' && block.image_url.url)
    .map((block, index) => ({
      id: `${Date.now()}-${index}-${Math.random().toString(16).slice(2)}`,
      name: `image-${index + 1}`,
      mimeType: 'image/*',
      dataUrl: block.image_url.url
    }));
}

function setChatComposerContent(content) {
  const inputEl = document.getElementById('chatInput');
  if (inputEl) {
    inputEl.value = extractChatMessageRawText(content);
    autoResizeChatInput();
    inputEl.focus();
  }
  chatPendingImages = extractChatMessageImages(content);
  renderChatImagePreviews();
}

function renderChatMessages() {
  const messagesEl = document.getElementById('chatMessages');
  if (!messagesEl) return;
  messagesEl.innerHTML = '';
  chatMessages.forEach((msg, index) => {
    const bubble = appendChatBubble(msg.role, msg.content, index);
    if (msg.role === 'assistant') {
      renderChatBubbleStats(bubble, chatMessageSummaries[index]);
    }
  });
  messagesEl.scrollTop = messagesEl.scrollHeight;
}

function getUserChatMessageFromAction(actionTarget) {
  const bubble = actionTarget?.closest?.('.chat-message');
  const index = Number.parseInt(bubble?.dataset.chatIndex || '', 10);
  if (!Number.isInteger(index) || index < 0 || index >= chatMessages.length) return null;
  const message = chatMessages[index];
  if (!message || message.role !== 'user') return null;
  return { index, message };
}

async function retryChatMessage(actionTarget) {
  if (isChatSending) return;
  const found = getUserChatMessageFromAction(actionTarget);
  if (!found) return;
  if (!chatChannel) {
    showError(i18nText('modelTest.chat.selectChannel', '请先选择渠道'));
    return;
  }
  const currentModel = document.getElementById('chatModelSelect')?.value?.trim() || chatModel;
  if (!currentModel) {
    showError(i18nText('modelTest.chat.selectModel', '请先选择模型'));
    return;
  }

  const content = cloneChatMessageContent(found.message.content);
  trimChatHistory(found.index);
  saveChatMessagesToStorage();
  renderChatMessages();
  setChatComposerContent(content);
  await sendChatMessage();
}

function editChatMessage(actionTarget) {
  if (isChatSending) return;
  const found = getUserChatMessageFromAction(actionTarget);
  if (!found) return;

  const content = cloneChatMessageContent(found.message.content);
  trimChatHistory(found.index);
  saveChatMessagesToStorage();
  renderChatMessages();
  setChatComposerContent(content);
}

/** 发送消息：追加用户消息 → POST /admin/channels/:id/chat (SSE) → 实时渲染 delta */
async function sendChatMessage() {
  if (isChatSending) return;

  const inputEl = document.getElementById('chatInput');
  const content = inputEl?.value?.trim();
  if (!content && chatPendingImages.length === 0) {
    showError(i18nText('modelTest.chat.emptyInput', '请输入消息'));
    return;
  }
  if (!chatChannel) {
    showError(i18nText('modelTest.chat.selectChannel', '请先选择渠道'));
    return;
  }
  const currentModel = document.getElementById('chatModelSelect')?.value?.trim() || chatModel;
  if (!currentModel) {
    showError(i18nText('modelTest.chat.selectModel', '请先选择模型'));
    return;
  }
  chatModel = currentModel;

  const userContent = buildChatUserContent(content, chatPendingImages);
  const userMessageIndex = pushChatMessage({ role: 'user', content: userContent });
  saveChatMessagesToStorage();
  appendChatBubble('user', userContent, userMessageIndex);
  inputEl.value = '';
  chatPendingImages = [];
  renderChatImagePreviews();
  autoResizeChatInput();

  isChatSending = true;
  const sendBtn = document.getElementById('chatSendBtn');
  if (sendBtn) {
    sendBtn.disabled = true;
    sendBtn.textContent = i18nText('modelTest.chat.sending', '发送中...');
  }

  const assistantBubble = appendChatBubble('assistant', '');
  const contentEl = assistantBubble?.querySelector('.chat-message-content');
  renderChatMarkdown(contentEl, '', { cursor: true });

  let accText = '';
  let accThinking = '';
  let assistantSummary = null;
  let hasError = false;

  try {
    const token = localStorage.getItem('ccload_token');
    const chatStreamEnabled = document.getElementById('chatStreamEnabled')?.checked !== false;
    const chatThinkingEffort = getChatThinkingEffort();
    const chatBuiltinSearch = isChatBuiltinSearchEnabled();
    const advancedAPI = getChatAdvancedOptionsAPI();
    const basePayload = {
      model: chatModel,
      stream: chatStreamEnabled,
      thinking_effort: chatThinkingEffort,
      builtin_search: chatBuiltinSearch
    };
    const requestPayload = advancedAPI && typeof advancedAPI.buildChatRequestPayload === 'function'
      ? advancedAPI.buildChatRequestPayload(basePayload, chatMessages, chatAdvancedOptions)
      : { ...basePayload, messages: chatMessages };
    const resp = await fetch(`/admin/channels/${chatChannel.id}/chat`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        ...(token ? { 'Authorization': `Bearer ${token}` } : {}),
      },
      body: JSON.stringify(requestPayload),
    });

    if (!resp.ok || !resp.body) throw new Error(`HTTP ${resp.status}`);

    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buf = '';

    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });

      let idx;
      while ((idx = buf.indexOf('\n\n')) !== -1) {
        const block = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        for (const line of block.split('\n')) {
          if (!line.startsWith('data:')) continue;
          const payload = line.slice(5).trim();
          if (!payload || payload === '[DONE]') continue;
          try {
            const evt = JSON.parse(payload);
            if (evt.error) {
              hasError = true;
              if (contentEl) {
                contentEl.textContent = evt.error;
                assistantBubble?.classList.add('chat-message--error');
              }
              if (assistantBubble) assistantBubble._rawText = String(evt.error || '');
            } else if (typeof evt.thinking_delta === 'string') {
              accThinking += evt.thinking_delta;
              renderChatThinking(assistantBubble, accThinking, true);
              const messagesEl = document.getElementById('chatMessages');
              if (messagesEl) messagesEl.scrollTop = messagesEl.scrollHeight;
            } else if (typeof evt.delta === 'string') {
              accText += evt.delta;
              renderChatMarkdown(contentEl, accText, { cursor: true });
              if (assistantBubble) assistantBubble._rawText = accText;
              const messagesEl = document.getElementById('chatMessages');
              if (messagesEl) messagesEl.scrollTop = messagesEl.scrollHeight;
            } else if (evt.summary) {
              if (assistantBubble) {
                assistantSummary = cloneChatSummary(evt.summary);
                assistantBubble._chatSummary = assistantSummary;
                renderChatBubbleStats(assistantBubble, assistantSummary);
              }
            }
          } catch (_) { /* ignore malformed event */ }
        }
      }
    }

    if (!hasError) {
      renderChatThinking(assistantBubble, accThinking, false);
      renderChatMarkdown(contentEl, accText || '');
      if (assistantBubble) assistantBubble._rawText = accText || '';
      if (accText) {
        const assistantMessageIndex = pushChatMessage({ role: 'assistant', content: accText }, assistantSummary);
        if (assistantBubble) assistantBubble.dataset.chatIndex = String(assistantMessageIndex);
        saveChatMessagesToStorage();
      } else {
        popChatMessage();
      }
    }
  } catch (e) {
    popChatMessage();
    saveChatMessagesToStorage();
    if (contentEl) {
      contentEl.textContent = e.message || i18nText('modelTest.chat.error', '发送失败');
      assistantBubble?.classList.add('chat-message--error');
    }
    if (assistantBubble) assistantBubble._rawText = String(e?.message || i18nText('modelTest.chat.error', '发送失败'));
  } finally {
    contentEl?.querySelector('.chat-cursor')?.remove();
    isChatSending = false;
    if (sendBtn) {
      sendBtn.disabled = false;
      sendBtn.textContent = i18nText('modelTest.chat.send', '发送');
    }
    const messagesEl = document.getElementById('chatMessages');
    if (messagesEl) messagesEl.scrollTop = messagesEl.scrollHeight;
  }
}

/**
 * 提取对话消息的可复制纯文本：
 * - string直接返回
 * - 多模态数组仅拼接 text 块（忽略 image_url）
 */
function extractChatMessageRawText(content) {
  if (typeof content === 'string') return content;
  if (Array.isArray(content)) {
    return content
      .filter(item => item && item.type === 'text' && typeof item.text === 'string')
      .map(item => item.text)
      .join('\n');
  }
  return '';
}

/**
 * 在消息列表中追加一个气泡，返回 bubble 元素。
 * @param {'user'|'assistant'} role
 * @param {string|Array<any>} content
 * @param {number|null} messageIndex
 */
function appendChatBubble(role, content, messageIndex = null) {
  const messagesEl = document.getElementById('chatMessages');
  if (!messagesEl) return null;

  const bubble = document.createElement('div');
  bubble.className = `chat-message chat-message--${role}`;
  if (Number.isInteger(messageIndex) && messageIndex >= 0) {
    bubble.dataset.chatIndex = String(messageIndex);
  }

  const contentEl = document.createElement('div');
  contentEl.className = 'chat-message-content';

  if (content) {
    if (role === 'assistant') {
      renderChatMarkdown(contentEl, content);
    } else {
      renderChatUserContent(contentEl, content);
    }
  }

  bubble.appendChild(contentEl);

  const footerEl = document.createElement('div');
  footerEl.className = 'chat-message-footer';
  const copyLabel = i18nText('common.copy', '复制');
  if (role === 'user') {
    const actionsEl = document.createElement('div');
    actionsEl.className = 'chat-message-footer-actions';
    actionsEl.appendChild(createChatActionButton('retry-chat-message', i18nText('modelTest.chat.refreshMessage', '刷新'), '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 12a9 9 0 1 1-2.64-6.36"></path><path d="M21 3v6h-6"></path></svg>'));
    actionsEl.appendChild(createChatActionButton('edit-chat-message', i18nText('modelTest.chat.editMessage', '修改'), '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 20h9"></path><path d="M16.5 3.5a2.12 2.12 0 0 1 3 3L7 19l-4 1 1-4Z"></path></svg>'));
    actionsEl.appendChild(createChatActionButton('copy-chat-message', copyLabel, '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg>', 'chat-message-copy-btn'));
    footerEl.appendChild(actionsEl);
  } else {
    footerEl.appendChild(createChatActionButton('copy-chat-message', copyLabel, '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg>', 'chat-message-copy-btn'));
    const statsEl = document.createElement('div');
    statsEl.className = 'chat-message-stats';
    statsEl.hidden = true;
    footerEl.appendChild(statsEl);
  }
  bubble.appendChild(footerEl);

  bubble._rawText = extractChatMessageRawText(content);

  messagesEl.appendChild(bubble);
  messagesEl.scrollTop = messagesEl.scrollHeight;
  return bubble;
}

function createChatActionButton(action, label, iconHTML, extraClass = '') {
  const btn = document.createElement('button');
  btn.type = 'button';
  btn.className = ['chat-message-action-btn', extraClass].filter(Boolean).join(' ');
  btn.setAttribute('data-action', action);
  btn.setAttribute('aria-label', label);
  btn.title = label;
  btn.innerHTML = iconHTML;
  return btn;
}

/**
 * 在 assistant 气泡右下方渲染 token 统计信息。
 * @param {HTMLElement} bubble
 * @param {object} summary - { first_byte_ms, duration_ms, input_tokens, output_tokens, cache_read, cache_create, speed, cost_usd }
 */
function renderChatBubbleStats(bubble, summary) {
  if (!bubble || !summary) return;
  let statsEl = bubble.querySelector('.chat-message-stats');

  const parts = [];
  if (summary.first_byte_ms != null && summary.first_byte_ms > 0) {
    parts.push(i18nText('modelTest.chat.statsFirstByte', '首字') + ' ' + formatDurationMs(summary.first_byte_ms));
  }
  if (summary.duration_ms != null && summary.duration_ms > 0) {
    parts.push(i18nText('modelTest.chat.statsDuration', '耗时') + ' ' + formatDurationMs(summary.duration_ms));
  }
  if (summary.input_tokens != null && summary.input_tokens > 0) {
    parts.push(i18nText('common.input', '输入') + ' ' + summary.input_tokens);
  }
  if (summary.output_tokens != null && summary.output_tokens > 0) {
    parts.push(i18nText('common.output', '输出') + ' ' + summary.output_tokens);
  }
  if (summary.cache_read != null && summary.cache_read > 0) {
    parts.push(i18nText('modelTest.cacheRead', '缓读') + ' ' + summary.cache_read);
  }
  if (summary.cache_create != null && summary.cache_create > 0) {
    parts.push(i18nText('modelTest.cacheCreate', '缓建') + ' ' + summary.cache_create);
  }
  if (summary.speed != null && summary.speed > 0) {
    parts.push(summary.speed.toFixed(1) + ' tok/s');
  }
  if (summary.cost_usd != null && summary.cost_usd > 0) {
    parts.push('$' + summary.cost_usd.toFixed(4));
  }

  if (!statsEl) {
    const footerEl = bubble.querySelector('.chat-message-footer') || bubble.appendChild(document.createElement('div'));
    footerEl.classList.add('chat-message-footer');
    statsEl = document.createElement('div');
    statsEl.className = 'chat-message-stats';
    footerEl.appendChild(statsEl);
  }
  if (parts.length === 0) {
    statsEl.hidden = true;
    return;
  }
  statsEl.hidden = false;
  statsEl.textContent = parts.join(' · ');
}

function renderChatThinking(bubble, thinking, streaming = false) {
  if (!bubble) return;
  const text = String(thinking || '').trim();
  let thinkingEl = bubble.querySelector('.chat-thinking');
  if (!text) {
    thinkingEl?.remove();
    return;
  }

  if (!thinkingEl) {
    thinkingEl = document.createElement('details');
    thinkingEl.className = 'chat-thinking';
    thinkingEl.open = true;

    const summary = document.createElement('summary');
    summary.className = 'chat-thinking-summary';
    summary.setAttribute('data-i18n', 'modelTest.chat.thinking');
    summary.textContent = i18nText('modelTest.chat.thinking', '思考');

    const contentEl = document.createElement('div');
    contentEl.className = 'chat-thinking-content';

    thinkingEl.appendChild(summary);
    thinkingEl.appendChild(contentEl);
    bubble.insertBefore(thinkingEl, bubble.firstChild);
  }

  const contentEl = thinkingEl.querySelector('.chat-thinking-content');
  if (contentEl) {
    contentEl.textContent = text;
  }
  thinkingEl.open = streaming;
}

/** 清空对话历史与消息列表 DOM */
function clearChat() {
  chatMessages = [];
  chatMessageSummaries = [];
  chatPendingImages = [];
  clearChatMessagesFromStorage();
  const messagesEl = document.getElementById('chatMessages');
  if (messagesEl) messagesEl.innerHTML = '';
  renderChatImagePreviews();
}

/** 构造导出文件名：chat-YYYYMMDD-HHmmss.<ext> */
function buildChatExportFilename(ext) {
  const d = new Date();
  const pad = (n) => String(n).padStart(2, '0');
  const stamp = `${d.getFullYear()}${pad(d.getMonth() + 1)}${pad(d.getDate())}-${pad(d.getHours())}${pad(d.getMinutes())}${pad(d.getSeconds())}`;
  return `chat-${stamp}.${ext}`;
}

/** 触发浏览器下载 Blob */
function downloadChatBlob(content, filename, mimeType) {
  const blob = new Blob([content], { type: mimeType });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = filename;
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

/** 将 chatMessages 转为 Markdown 文本 */
function buildChatMarkdownText() {
  if (!Array.isArray(chatMessages) || chatMessages.length === 0) return '';
  const channelName = chatChannel?.name || '-';
  const modelName = chatModel || '-';
  const exportTime = new Date().toLocaleString();
  const lines = [];
  lines.push(`# ${i18nText('modelTest.chat.exportTitle', '对话导出')}`);
  lines.push('');
  lines.push(`- ${i18nText('modelTest.channel', '渠道')}: ${channelName}`);
  lines.push(`- ${i18nText('common.model', '模型')}: ${modelName}`);
  lines.push(`- ${i18nText('modelTest.chat.exportTime', '导出时间')}: ${exportTime}`);
  lines.push('');
  lines.push('---');
  lines.push('');

  chatMessages.forEach((msg) => {
    const roleLabel = msg.role === 'user'
      ? i18nText('modelTest.chat.roleUser', '用户')
      : i18nText('modelTest.chat.roleAssistant', '助手');
    lines.push(`## ${roleLabel}`);
    lines.push('');
    if (typeof msg.content === 'string') {
      lines.push(msg.content);
    } else if (Array.isArray(msg.content)) {
      msg.content.forEach((block) => {
        if (!block || typeof block !== 'object') return;
        if (block.type === 'text') {
          lines.push(String(block.text || ''));
        } else if (block.type === 'image_url') {
          const url = String(block.image_url?.url || '');
          if (url) lines.push(`![image](${url})`);
        }
      });
    }
    lines.push('');
  });
  return lines.join('\n');
}

/** 导出对话为 Markdown 文件 */
function exportChatAsMarkdown() {
  const text = buildChatMarkdownText();
  if (!text) {
    if (typeof window.showError === 'function') {
      window.showError(i18nText('modelTest.chat.exportEmpty', '暂无对话内容'));
    }
    return;
  }
  downloadChatBlob(text, buildChatExportFilename('md'), 'text/markdown;charset=utf-8');
}

/** 构造导出 HTML（抓取 #chatMessages 已渲染 DOM + 内联精简样式） */
function buildChatExportHTML() {
  const messagesEl = document.getElementById('chatMessages');
  if (!messagesEl || !messagesEl.innerHTML.trim()) return '';
  const esc = (typeof window.escapeHtml === 'function')
    ? window.escapeHtml
    : (s) => String(s || '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const channelName = esc(chatChannel?.name || '-');
  const modelName = esc(chatModel || '-');
  const exportTime = esc(new Date().toLocaleString());
  const title = esc(i18nText('modelTest.chat.exportTitle', '对话导出'));
  const channelLabel = esc(i18nText('modelTest.channel', '渠道'));
  const modelLabel = esc(i18nText('common.model', '模型'));
  const timeLabel = esc(i18nText('modelTest.chat.exportTime', '导出时间'));

  const css = `:root{color-scheme:light dark}body{font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Hiragino Sans GB","Microsoft YaHei",sans-serif;max-width:880px;margin:0 auto;padding:24px 16px;line-height:1.6;color:#1f2937;background:#f9fafb}header{border-bottom:1px solid #e5e7eb;padding-bottom:12px;margin-bottom:20px}header h1{margin:0 0 8px;font-size:20px}header .meta{font-size:13px;color:#6b7280}header .meta span{margin-right:14px}.chat-message{display:flex;margin:12px 0}.chat-message--user{justify-content:flex-end}.chat-message--assistant{justify-content:flex-start}.chat-message-content{max-width:78%;padding:10px 14px;border-radius:12px;word-break:break-word}.chat-message--user .chat-message-content{background:#3b82f6;color:#fff;border-bottom-right-radius:4px;white-space:pre-wrap}.chat-message--assistant .chat-message-content{background:#fff;color:#1f2937;border:1px solid #e5e7eb;border-bottom-left-radius:4px}.chat-message-footer{display:none}.chat-message--assistant .chat-message-content pre{background:#f3f4f6;border:1px solid #e5e7eb;padding:10px;border-radius:6px;overflow-x:auto;font-size:13px}.chat-message--assistant .chat-message-content code{background:#f3f4f6;padding:0 4px;border-radius:4px;font-family:ui-monospace,"SFMono-Regular","Menlo",monospace;font-size:13px}.chat-message--assistant .chat-message-content pre code{background:transparent;padding:0}.chat-message--assistant .chat-message-content p{margin:6px 0}.chat-message--assistant .chat-message-content table{border-collapse:collapse;margin:8px 0}.chat-message--assistant .chat-message-content th,.chat-message--assistant .chat-message-content td{border:1px solid #e5e7eb;padding:6px 10px}.chat-image-preview-thumb{max-width:240px;max-height:240px;border-radius:8px;margin:4px 0;display:block}.chat-thinking{background:#fef3c7;border:1px solid #fcd34d;border-radius:8px;padding:8px 12px;margin:8px 0;font-size:13px}.chat-thinking-summary{font-weight:600;cursor:pointer}.chat-thinking-content{white-space:pre-wrap;margin-top:6px;color:#78350f}.chat-cursor{display:none}@media (prefers-color-scheme:dark){body{color:#e5e7eb;background:#0f172a}header{border-bottom-color:#1f2937}header .meta{color:#9ca3af}.chat-message--assistant .chat-message-content{background:#1e293b;color:#e5e7eb;border-color:#334155}.chat-message--assistant .chat-message-content pre,.chat-message--assistant .chat-message-content code{background:#0f172a;border-color:#334155}.chat-message--assistant .chat-message-content th,.chat-message--assistant .chat-message-content td{border-color:#334155}.chat-thinking{background:rgba(251,191,36,.1);border-color:#b45309;color:#fcd34d}.chat-thinking-content{color:#fde68a}}@media print{body{background:#fff}}`;

  return `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>${title} - ${modelName}</title>
<style>${css}</style>
</head>
<body>
<header>
  <h1>${title}</h1>
  <div class="meta">
    <span>${channelLabel}: ${channelName}</span>
    <span>${modelLabel}: ${modelName}</span>
    <span>${timeLabel}: ${exportTime}</span>
  </div>
</header>
<main>
${messagesEl.innerHTML}
</main>
</body>
</html>`;
}

/** 导出对话为 HTML 文件 */
function exportChatAsHTML() {
  const html = buildChatExportHTML();
  if (!html) {
    if (typeof window.showError === 'function') {
      window.showError(i18nText('modelTest.chat.exportEmpty', '暂无对话内容'));
    }
    return;
  }
  downloadChatBlob(html, buildChatExportFilename('html'), 'text/html;charset=utf-8');
}

/** 切换导出下拉菜单显隐 */
function toggleChatExportMenu() {
  const dropdown = document.getElementById('chatExportDropdown');
  if (!dropdown) return;
  const isOpen = dropdown.classList.toggle('open');
  const trigger = document.getElementById('chatExportTrigger');
  if (trigger) trigger.setAttribute('aria-expanded', String(isOpen));
}

/** 关闭导出下拉菜单 */
function closeChatExportMenu() {
  const dropdown = document.getElementById('chatExportDropdown');
  if (!dropdown) return;
  dropdown.classList.remove('open');
  const trigger = document.getElementById('chatExportTrigger');
  if (trigger) trigger.setAttribute('aria-expanded', 'false');
}

/** 注册全局监听：外部点击 / Esc 关闭导出菜单（仅绑定一次） */
function initChatExportDropdown() {
  if (window.__chatExportDropdownBound) return;
  window.__chatExportDropdownBound = true;

  document.addEventListener('click', (e) => {
    const dropdown = document.getElementById('chatExportDropdown');
    if (!dropdown || !dropdown.classList.contains('open')) return;
    if (!dropdown.contains(e.target)) closeChatExportMenu();
  });

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') closeChatExportMenu();
  });
}

/** textarea 随内容自动调整高度 */
function autoResizeChatInput() {
  const el = document.getElementById('chatInput');
  if (!el) return;
  el.style.height = 'auto';
  el.style.height = `${Math.min(el.scrollHeight, 200)}px`;
}

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

function composeMergedResponse(data) {
  const raw = String(data.responseBody || '').replace(/\r\n/g, '\n').trim();
  if (!raw) return '';

  const state = {
    reasoning: [],
    text: [],
    functionCalls: [],
    hasReasoningDelta: false,
    hasTextDelta: false,
    hasFunctionCallDelta: false,
    lastFunctionCallIndex: null,
    functionCallDeltaIndexes: new Set()
  };
  const ssePayloads = window.SSEMerge.parsePayloads(raw);
  if (ssePayloads.length > 0) {
    ssePayloads.forEach(payload => window.SSEMerge.collectPayload(payload, state));
  } else {
    try {
      window.SSEMerge.collectPayload(JSON.parse(raw), state);
    } catch {
      return tryFormatJSON(raw);
    }
  }

  const sections = [];
  [state.reasoning, state.text, state.functionCalls].forEach(bucket => {
    const text = bucket.join('').trim();
    if (text) sections.push(text);
  });

  return sections.join('\n\n') || tryFormatJSON(raw);
}

function getMergedRenderMode(text) {
  const trimmed = String(text || '').trim();
  if (!trimmed) return 'text';
  const isJson = (trimmed.startsWith('{') && trimmed.endsWith('}'))
    || (trimmed.startsWith('[') && trimmed.endsWith(']'));
  if (!isJson) return 'text';
  try {
    JSON.parse(trimmed);
    return 'json';
  } catch {
    return 'text';
  }
}

function updateUpstreamResponseActionButtons() {
  const responseActive = !!document.getElementById('upstreamTabResponse')?.classList.contains('active');
  const copyBtn = document.querySelector('#upstreamDetailModal .upstream-copy-btn--tabs');
  if (copyBtn) {
    copyBtn.dataset.copyTarget = responseActive
      ? (upstreamMergedVisible ? 'upstreamRespMerged' : 'upstreamRespRaw')
      : 'upstreamReqRaw';
  }

  const mergeBtn = document.getElementById('upstreamMergeBtn');
  if (mergeBtn) {
    mergeBtn.hidden = !responseActive;
  }
}

function setUpstreamMergedVisible(visible) {
  upstreamMergedVisible = !!visible;

  const raw = document.getElementById('upstreamRespRaw');
  const merged = document.getElementById('upstreamRespMerged');
  if (raw) raw.hidden = upstreamMergedVisible;
  if (merged) merged.hidden = !upstreamMergedVisible;

  const mergeBtn = document.getElementById('upstreamMergeBtn');
  if (mergeBtn) {
    const key = upstreamMergedVisible ? 'logs.debugRaw' : 'logs.debugMerge';
    mergeBtn.classList.toggle('active', upstreamMergedVisible);
    mergeBtn.setAttribute('aria-pressed', upstreamMergedVisible ? 'true' : 'false');
    mergeBtn.dataset.i18n = key;
    mergeBtn.textContent = (typeof i18nText === 'function' ? i18nText(key) : '') || (upstreamMergedVisible ? '原始' : '合并');
  }

  updateUpstreamResponseActionButtons();
}

function showUpstreamDetailModal(data) {
  if (!data) return;

  window.setHighlightedCodeContent('upstreamReqRaw', composeRawRequest(data), 'request');
  window.setHighlightedCodeContent('upstreamRespRaw', composeRawResponse(data), 'response');
  const mergedResponse = composeMergedResponse(data);
  window.setHighlightedCodeContent('upstreamRespMerged', mergedResponse, getMergedRenderMode(mergedResponse));

  // Reset to Request tab
  const modal = document.getElementById('upstreamDetailModal');
  modal.querySelectorAll('.upstream-tab').forEach(t => t.classList.toggle('active', t.dataset.tab === 'request'));
  document.getElementById('upstreamTabRequest').classList.add('active');
  document.getElementById('upstreamTabResponse').classList.remove('active');
  setUpstreamMergedVisible(false);
  updateUpstreamResponseActionButtons();

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

// Tab switch + copy/merge button delegation for upstream detail modal
document.addEventListener('click', (e) => {
  // 对话气泡复制按钮
  const chatCopyBtn = e.target.closest('.chat-message-copy-btn');
  if (chatCopyBtn) {
    e.preventDefault();
    const bubble = chatCopyBtn.closest('.chat-message');
    const text = bubble?._rawText || '';
    if (!text) return;
    navigator.clipboard.writeText(text).then(() => {
      chatCopyBtn.classList.add('chat-message-copy-btn--copied');
      setTimeout(() => chatCopyBtn.classList.remove('chat-message-copy-btn--copied'), 1500);
    }).catch(() => { /* 静默失败 */ });
    return;
  }

  const tab = e.target.closest('#upstreamDetailModal .upstream-tab');
  if (tab) {
    const target = tab.dataset.tab;
    document.querySelectorAll('#upstreamDetailModal .upstream-tab').forEach(t => t.classList.toggle('active', t === tab));
    document.getElementById('upstreamTabRequest').classList.toggle('active', target === 'request');
    document.getElementById('upstreamTabResponse').classList.toggle('active', target === 'response');
    updateUpstreamResponseActionButtons();
    return;
  }

  const mergeBtn = e.target.closest('#upstreamDetailModal [data-action="merge-upstream-response"]');
  if (mergeBtn) {
    setUpstreamMergedVisible(!upstreamMergedVisible);
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
      await loadChannels({ preserveSelection: true, preserveTableState: true });
    }
  };
  initModelTestActions();
  bindEvents();
  initChatExportDropdown();

  // 从 localStorage 恢复测试模式
  testMode = loadTestModeFromStorage();

  // 从 localStorage 恢复流式开关
  const streamEnabled = document.getElementById('streamEnabled');
  if (streamEnabled) {
    streamEnabled.checked = loadStreamEnabledFromStorage();
  }

  // 从 localStorage 恢复对话模式状态
  const chatStreamEnabled = document.getElementById('chatStreamEnabled');
  if (chatStreamEnabled) {
    chatStreamEnabled.checked = loadChatStreamEnabledFromStorage();
  }

  chatModel = loadChatModelFromStorage();
  const storedChatChannelId = loadChatChannelIdFromStorage();
  chatThinkingEffort = loadChatThinkingEffortFromStorage();
  chatAdvancedOptions = loadChatAdvancedOptionsFromStorage();
  updateChatAdvancedOptionsButton();

  const chatBuiltinSearchToggle = document.getElementById('chatBuiltinSearchToggle');
  if (chatBuiltinSearchToggle) {
    const builtinSearchEnabled = loadChatBuiltinSearchFromStorage();
    chatBuiltinSearchToggle.setAttribute('aria-pressed', builtinSearchEnabled ? 'true' : 'false');
  }

  // 并行化：loadChannels 与 loadDefaultTestContent 互不依赖，同时发出
  await Promise.all([
    loadChannels(),
    loadDefaultTestContent()
  ]);

  // 恢复对话渠道选择（必须在 loadChannels 之后）
  if (storedChatChannelId !== null) {
    chatChannel = channelsList.find(c => c.id === storedChatChannelId) || null;
  }
  updateHeadByMode();
  updateModeUI();
  renderRowsByMode();

  // 根据恢复的模式初始化 UI
  const modeTabChannel = document.getElementById('modeTabChannel');
  const modeTabModel = document.getElementById('modeTabModel');
  const modeTabChat = document.getElementById('modeTabChat');
  modeTabChannel?.classList.toggle('active', testMode === TEST_MODE_CHANNEL);
  modeTabModel?.classList.toggle('active', testMode === TEST_MODE_MODEL);
  modeTabChat?.classList.toggle('active', testMode === TEST_MODE_CHAT);

  if (testMode === TEST_MODE_CHAT) {
    initChatPanel();
  }
}

window.initPageBootstrap({
  topbarKey: 'model-test',
  run: () => {
    bootstrap();
  }
});
