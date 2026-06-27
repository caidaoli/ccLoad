const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const script = fs.readFileSync(path.join(__dirname, 'model-test.js'), 'utf8');

function extractFunction(source, name) {
  const signature = `function ${name}`;
  let start = source.indexOf(`async ${signature}`);
  if (start < 0) {
    start = source.indexOf(signature);
  }
  assert.ok(start >= 0, `缺少函数 ${name}`);

  const braceStart = source.indexOf('{', start);
  assert.ok(braceStart >= 0, `函数 ${name} 缺少起始大括号`);

  let depth = 0;
  for (let i = braceStart; i < source.length; i++) {
    const char = source[i];
    if (char === '{') depth++;
    if (char === '}') depth--;
    if (depth === 0) {
      return source.slice(start, i + 1);
    }
  }

  assert.fail(`函数 ${name} 大括号未闭合`);
}

function createDomElement(tagName, attrs = {}) {
  const element = {
    tagName: String(tagName || '').toUpperCase(),
    children: [],
    parentElement: null,
    attributes: new Map(),
    style: {},
    _textContent: '',
    className: '',
    id: '',
    value: '',
    placeholder: '',
    addEventListener() {},
    appendChild(child) {
      child.parentElement = this;
      this.children.push(child);
      return child;
    },
    insertBefore(child, reference) {
      child.parentElement = this;
      const index = this.children.indexOf(reference);
      if (index < 0) {
        this.children.push(child);
      } else {
        this.children.splice(index, 0, child);
      }
      return child;
    },
    setAttribute(name, value) {
      this.attributes.set(name, String(value));
      if (name === 'id') this.id = String(value);
      if (name === 'class') this.className = String(value);
    },
    getAttribute(name) {
      return this.attributes.has(name) ? this.attributes.get(name) : null;
    },
    removeAttribute(name) {
      this.attributes.delete(name);
    },
    querySelector(selector) {
      return queryTree(this, selector)[0] || null;
    },
    querySelectorAll(selector) {
      return queryTree(this, selector);
    }
  };

  Object.defineProperty(element, 'textContent', {
    get() {
      return this._textContent + this.children.map(child => child.textContent).join('');
    },
    set(value) {
      this._textContent = String(value || '');
      this.children = [];
    }
  });

  Object.entries(attrs).forEach(([key, value]) => {
    if (key === 'textContent') {
      element.textContent = value;
      return;
    }
    element.setAttribute(key, value);
  });

  return element;
}

function matchesSelector(element, selector) {
  if (selector === '[data-i18n]') {
    return element.getAttribute('data-i18n') !== null;
  }
  if (selector.startsWith('#')) {
    return element.id === selector.slice(1);
  }
  if (selector.startsWith('.')) {
    return String(element.className || '').split(/\s+/).includes(selector.slice(1));
  }
  const attrMatch = selector.match(/^([a-z]+)\[([^=]+)="([^"]+)"\]$/i);
  if (attrMatch) {
    const [, tag, attr, value] = attrMatch;
    return element.tagName === tag.toUpperCase() && element.getAttribute(attr) === value;
  }
  return element.tagName === selector.toUpperCase();
}

function queryTree(root, selector) {
  const result = [];
  const visit = (element) => {
    if (matchesSelector(element, selector)) {
      result.push(element);
    }
    element.children.forEach(visit);
  };
  root.children.forEach(visit);
  return result;
}

function findElementById(root, id) {
  return queryTree(root, `#${id}`)[0] || null;
}

test('i18n.js 暴露的 window.i18nText 对动态删除文案执行插值兜底', () => {
  const i18nScript = fs.readFileSync(path.join(__dirname, 'i18n.js'), 'utf8');
  const sandbox = {
    window: {
      I18N_LOCALES: {
        'zh-CN': {
          'modelTest.deleteSuccessSummary': '删除完成：成功 {success_channels} 个渠道，失败 {failed_channels} 个渠道'
        }
      }
    },
    document: { documentElement: {} },
    navigator: {},
    localStorage: { getItem() { return null; }, setItem() {} },
    console
  };

  vm.runInNewContext(i18nScript, sandbox);

  assert.equal(typeof sandbox.window.i18nText, 'function', 'i18n.js 应当暴露 window.i18nText');
  assert.equal(
    sandbox.window.i18nText(
      'modelTest.deleteSuccessSummary',
      'fallback',
      { success_channels: 2, failed_channels: 0 }
    ),
    '删除完成：成功 2 个渠道，失败 0 个渠道'
  );

  assert.equal(
    sandbox.window.i18nText('missing.key', '回退 {count} 项', { count: 5 }),
    '回退 5 项',
    'i18nText 在 key 缺失时应使用 fallback 并完成插值'
  );
});

test('model-test 思考等级 combobox 使用枚举值而不是显示文本', () => {
  const sandbox = {
    i18nText: (_key, fallback) => fallback
  };

  vm.runInNewContext(`
    let chatThinkingEffort = 'high';
    ${extractFunction(script, 'getChatThinkingOptions')}
    ${extractFunction(script, 'getChatThinkingLabel')}
    ${extractFunction(script, 'getChatThinkingEffort')}

    globalThis.result = {
      values: getChatThinkingOptions().map(option => option.value),
      highLabel: getChatThinkingLabel('high'),
      fallbackLabel: getChatThinkingLabel('unexpected'),
      effort: getChatThinkingEffort()
    };
  `, sandbox);

  assert.deepEqual(Array.from(sandbox.result.values), ['', 'none', 'minimal', 'low', 'medium', 'high']);
  assert.equal(sandbox.result.highLabel, '高');
  assert.equal(sandbox.result.fallbackLabel, '默认');
  assert.equal(sandbox.result.effort, 'high');
});

test('model-test 构建图片消息时文本保持文本、图片转 image_url 块', () => {
  const sandbox = {};
  vm.runInNewContext(`
    ${extractFunction(script, 'buildChatUserContent')}
    globalThis.result = {
      textOnly: buildChatUserContent('hello', []),
      imageOnly: buildChatUserContent('', [{ id: 'img1', dataUrl: 'data:image/png;base64,aW1n', mimeType: 'image/png', name: 'p.png' }]),
      mixed: buildChatUserContent('describe', [{ id: 'img1', dataUrl: 'data:image/png;base64,aW1n', mimeType: 'image/png', name: 'p.png' }])
    };
  `, sandbox);

  assert.equal(sandbox.result.textOnly, 'hello');
  assert.deepEqual(
    JSON.parse(JSON.stringify(sandbox.result.imageOnly)),
    [
      {
        type: 'image_url',
        image_url: {
          url: 'data:image/png;base64,aW1n'
        }
      }
    ]
  );
  assert.deepEqual(
    JSON.parse(JSON.stringify(sandbox.result.mixed)),
    [
      {
        type: 'text',
        text: 'describe'
      },
      {
        type: 'image_url',
        image_url: {
          url: 'data:image/png;base64,aW1n'
        }
      }
    ]
  );
});

test('model-test 对话切换模型后重置为支持该模型的首个渠道', () => {
  const sandbox = {
    chatModel: 'target-model',
    chatChannel: null,
    channelsList: [
      { id: 1, name: 'A', priority: 0, models: [{ model: 'other-model' }] },
      { id: 2, name: 'B', priority: 0, models: [{ model: 'target-model' }] },
      { id: 3, name: 'C', priority: 0, models: [{ model: 'target-model' }] }
    ],
    setValueCalls: [],
    chatChannelCombobox: {
      refreshCalled: 0,
      refresh() {
        this.refreshCalled += 1;
      },
      setValue(value, label) {
        sandbox.setValueCalls.push([value, label]);
      }
    },
    formatModelTestChannelOptionLabel(ch) {
      return ch.name;
    },
    saveChatChannelIdToStorage() {}
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'getModelName')}
    ${extractFunction(script, 'isModelSupported')}
    ${extractFunction(script, 'getChannelsForChatModel')}
    ${extractFunction(script, 'refreshChatChannelsByModel')}

    refreshChatChannelsByModel();
    globalThis.result = {
      chatChannelId: chatChannel ? chatChannel.id : null,
      refreshCalled: chatChannelCombobox.refreshCalled,
      setValueCalls
    };
  `, sandbox);

  assert.equal(sandbox.result.chatChannelId, 2);
  assert.equal(sandbox.result.refreshCalled, 1);
  assert.deepEqual(sandbox.result.setValueCalls, [['2', 'B']]);
});

test('model-test 页按模型测试渠道表在渠道名称后显示可编辑优先级和启用开关', () => {
  const getResultRowMobileLabels = vm.runInNewContext(
    `(${extractFunction(script, 'getResultRowMobileLabels')})`,
    {
      i18nText(key, fallback) {
        return `${key}:${fallback}`;
      }
    }
  );
  assert.equal(
    getResultRowMobileLabels('modelTest.channel', '渠道').mobileLabelPriority,
    'channels.table.priority:优先级'
  );
  assert.equal(
    getResultRowMobileLabels('modelTest.channel', '渠道').mobileLabelEnabled,
    'channels.table.enabled:启用'
  );

  const getResultTableColspan = vm.runInNewContext(`
    const TEST_MODE_MODEL = 'model';
    const RESULT_TABLE_COLSPAN_WITH_FIRST_BYTE = 11;
    const RESULT_TABLE_COLSPAN_NO_FIRST_BYTE = 10;
    const MODEL_MODE_EXTRA_COLSPAN = 2;
    let testMode = TEST_MODE_MODEL;
    function isFirstByteColumnVisible() { return true; }
    (${extractFunction(script, 'getResultTableColspan')})
  `, {});
  assert.equal(getResultTableColspan(), '13');

  const getRowSortValue = vm.runInNewContext(
    `(${extractFunction(script, 'getRowSortValue')})`,
    {
      parseNumericCellValue(text) {
        return Number.parseFloat(String(text));
      }
    }
  );
  const row = {
    children: [],
    querySelector(selector) {
      if (selector === '.ch-priority-input') {
        return { value: '120' };
      }
      if (selector === '.channel-enable-switch') {
        return { dataset: { enabled: 'true' } };
      }
      return null;
    }
  };
  assert.equal(getRowSortValue(row, 'priority'), 120);
  assert.equal(getRowSortValue(row, 'enabled'), 1);

  const disabledRow = {
    children: [],
    querySelector(selector) {
      if (selector === '.channel-enable-switch') {
        return { dataset: { enabled: 'false' } };
      }
      return null;
    }
  };
  assert.equal(getRowSortValue(disabledRow, 'enabled'), 0);
});

test('model-test.js 批量添加模型输入支持逗号换行去空和大小写去重', () => {
  const sandbox = {};
  vm.runInNewContext(`
    ${extractFunction(script, 'parseBatchModelInput')}
  `, sandbox);

  assert.deepEqual(
    Array.from(sandbox.parseBatchModelInput(' gpt-4o,gpt-4o-mini\nGPT-4O\n\n claude-3-5-sonnet ')),
    ['gpt-4o', 'gpt-4o-mini', 'claude-3-5-sonnet']
  );
});

test('model-test.js 批量添加模型只收集当前勾选渠道', () => {
  const rows = [
    { dataset: { channelId: '10' }, checkbox: { checked: true }, hidden: false },
    { dataset: { channelId: '20' }, checkbox: { checked: false }, hidden: false },
    { dataset: { channelId: '30' }, checkbox: { checked: true }, hidden: true }
  ];
  rows.forEach((row) => {
    row.querySelector = (selector) => selector === '.row-checkbox' ? row.checkbox : null;
  });

  const sandbox = {
    TEST_MODE_MODEL: 'model',
    testMode: 'model',
    channelsList: [
      { id: 10, models: [] },
      { id: 20, models: [] },
      { id: 30, models: [] }
    ],
    document: {
      querySelectorAll(selector) {
        assert.equal(selector, '#model-test-tbody tr[data-channel-id][data-model]');
        return rows;
      }
    },
    isDataRowVisible(row) {
      return !row.hidden;
    }
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'getVisibleChannelTargetsForAdd')}
  `, sandbox);

  assert.deepEqual(
    Array.from(sandbox.getVisibleChannelTargetsForAdd().map(target => target.channelId)),
    [10]
  );
});

test('model-test.js 批量添加模型会对当前渠道表格目标逐个保存并更新本地缓存', async () => {
  const calls = [];
  const sandbox = {
    fetchAPIWithAuth: async (url, options) => {
      calls.push({ url, options });
      return { success: true, total: 3 };
    }
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'getModelName')}
    ${extractFunction(script, 'buildModelEntriesFromNames')}
    ${extractFunction(script, 'appendModelsToChannelCache')}
    ${extractFunction(script, 'executeAddModelsToChannels')}
  `, sandbox);

  const channels = [
    { id: 10, models: [{ model: 'existing', redirect_model: '' }] },
    { id: 20, models: ['legacy'] }
  ];

  const result = await sandbox.executeAddModelsToChannels(
    ['gpt-4o', 'existing'],
    [
      { channelId: 10, channel: channels[0] },
      { channelId: 20, channel: channels[1] }
    ]
  );

  assert.deepEqual(
    calls.map(call => call.url),
    ['/admin/channels/10/models', '/admin/channels/20/models']
  );
  assert.deepEqual(JSON.parse(calls[0].options.body), {
    models: [
      { model: 'gpt-4o', redirect_model: '' },
      { model: 'existing', redirect_model: '' }
    ]
  });
  assert.equal(result.successCount, 2);
  assert.deepEqual(Array.from(result.failed), []);
  assert.deepEqual(channels[0].models.map(entry => entry.model || entry), ['existing', 'gpt-4o']);
  assert.deepEqual(channels[1].models.map(entry => entry.model || entry), ['legacy', 'gpt-4o', 'existing']);
});

test('model-test.js 表头模型过滤输入框不被后续全页翻译清掉', () => {
  const root = createDomElement('div');
  const headRow = createDomElement('tr');
  const nameTh = createDomElement('th', {
    'data-sort-key': 'name',
    'data-i18n': 'common.model',
    textContent: '模型'
  });
  headRow.appendChild(nameTh);
  root.appendChild(headRow);

  const sandbox = {
    TEST_MODE_MODEL: 'model',
    testMode: 'channel',
    nameFilterKeyword: '',
    headRow,
    mobileNameFilterInput: null,
    i18nText(key, fallback) {
      return `${key}:${fallback}`;
    },
    document: {
      createElement(tagName) {
        return createDomElement(tagName);
      },
      getElementById(id) {
        return findElementById(root, id);
      }
    }
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'getNameFilterPlaceholder')}
    ${extractFunction(script, 'syncNameFilterInputs')}
    ${extractFunction(script, 'renderNameFilterInHeader')}
  `, sandbox);

  sandbox.renderNameFilterInHeader();
  assert.ok(findElementById(root, 'modelTestNameFilter'), '首次渲染后应存在表头过滤输入框');

  root.querySelectorAll('[data-i18n]').forEach((element) => {
    element.textContent = `translated:${element.getAttribute('data-i18n')}`;
  });

  assert.ok(
    findElementById(root, 'modelTestNameFilter'),
    '渠道编辑器触发 translatePage 后不应删除表头过滤输入框'
  );
});

test('model-test.js 在模型模式重渲染时保留渠道勾选状态', () => {
  const sandbox = {
    tbody: {
      querySelectorAll() {
        return [
          {
            dataset: { channelId: '1', model: 'gpt-4.1' },
            querySelector(selector) {
              if (selector === '.row-checkbox') return { checked: false };
              return null;
            }
          },
          {
            dataset: { channelId: '2', model: 'gpt-4.1' },
            querySelector(selector) {
              if (selector === '.row-checkbox') return { checked: true };
              return null;
            }
          }
        ];
      }
    }
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'getRowSelectionKey')}
    ${extractFunction(script, 'captureRowSelectionState')}
    ${extractFunction(script, 'restoreRowSelectionState')}
  `, sandbox);

  const selectionState = sandbox.captureRowSelectionState();
  const preservedRow = {
    dataset: { channelId: '1', model: 'gpt-4.1' },
    querySelector(selector) {
      if (selector === '.row-checkbox') return this.checkbox;
      return null;
    },
    checkbox: { checked: true }
  };
  sandbox.restoreRowSelectionState(preservedRow, selectionState, true);
  assert.equal(preservedRow.checkbox.checked, false);

  const newRow = {
    dataset: { channelId: '3', model: 'gpt-4.1' },
    querySelector(selector) {
      if (selector === '.row-checkbox') return this.checkbox;
      return null;
    },
    checkbox: { checked: false }
  };
  sandbox.restoreRowSelectionState(newRow, selectionState, true);
  assert.equal(newRow.checkbox.checked, true);
});

test('model-test.js 重载渠道后按行键恢复已有测试结果', () => {
  function createCell(value = '') {
    return {
      textContent: value,
      innerHTML: value,
      title: '',
      dataset: {},
      classList: {
        added: [],
        add(name) { this.added.push(name); },
        remove() {}
      }
    };
  }

  function createRow(channelId, model, responseText) {
    const cells = new Map([
      ['.row-checkbox', { checked: true }],
      ['.first-byte-duration', createCell('1.20s')],
      ['.duration', createCell('2.30s')],
      ['.input-tokens', createCell('10')],
      ['.output-tokens', createCell('5')],
      ['.speed', createCell('4.2')],
      ['.cache-read', createCell('1')],
      ['.cache-create', createCell('2')],
      ['.cost', createCell('$0.001')],
      ['.response', createCell(responseText)]
    ]);
    cells.get('.cost').dataset.sortValue = '0.001';
    cells.get('.response').title = responseText;
    return {
      dataset: { channelId, model },
      style: { background: 'rgba(16, 185, 129, 0.1)', color: '' },
      _upstreamData: { url: 'https://upstream.test' },
      querySelector(selector) {
        return cells.get(selector) || null;
      },
      cells
    };
  }

  let currentRows = [createRow('7', 'gpt-4.1', 'old result')];
  const sandbox = {
    tbody: {
      querySelectorAll(selector) {
        if (selector === 'tr[data-channel-id][data-model]') return currentRows;
        return [];
      }
    }
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'getRowSelectionKey')}
    ${extractFunction(script, 'getModelTestResultCellSelectors')}
    ${extractFunction(script, 'captureModelTestTableState')}
    ${extractFunction(script, 'restoreModelTestTableState')}
  `, sandbox);

  const state = sandbox.captureModelTestTableState();
  const newRow = createRow('7', 'gpt-4.1', '-');
  newRow.cells.get('.row-checkbox').checked = false;
  newRow.style.background = '';
  newRow._upstreamData = null;

  sandbox.restoreModelTestTableState(new Map([['7::gpt-4.1', newRow]]), state);

  assert.equal(newRow.cells.get('.row-checkbox').checked, true);
  assert.equal(newRow.cells.get('.response').textContent, 'old result');
  assert.equal(newRow.cells.get('.response').title, 'old result');
  assert.equal(newRow.cells.get('.duration').textContent, '2.30s');
  assert.equal(newRow.cells.get('.cost').dataset.sortValue, '0.001');
  assert.equal(newRow._upstreamData.url, 'https://upstream.test');
  assert.deepEqual(newRow.cells.get('.response').classList.added, ['has-upstream-detail']);
});

test('按协议测试时模型模式只提供当前类型和协议都可测试的模型', () => {
  const sandbox = {
    ALL_PROTOCOLS: ['anthropic', 'codex', 'openai', 'gemini'],
    selectedModelType: 'anthropic',
    channelsList: [
      { id: 1, name: 'native-anthropic-a', channel_type: 'anthropic', protocol_transforms: [], priority: 10, models: ['claude-4'] },
      { id: 2, name: 'native-openai', channel_type: 'openai', protocol_transforms: [], priority: 5, models: ['gpt-4.1'] },
      { id: 3, name: 'anthropic-openai-transform', channel_type: 'anthropic', protocol_transforms: ['openai'], priority: 3, models: ['claude-3.7'] }
    ]
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'getModelName')}
    ${extractFunction(script, 'normalizeProtocol')}
    ${extractFunction(script, 'getChannelType')}
    ${extractFunction(script, 'getExposedProtocols')}
    ${extractFunction(script, 'getSupportedProtocols')}
    ${extractFunction(script, 'channelExposesProtocol')}
    ${extractFunction(script, 'channelMatchesModelType')}
    ${extractFunction(script, 'isModelSupported')}
    ${extractFunction(script, 'getAllModelsForProtocol')}
    ${extractFunction(script, 'getChannelsSupportingModel')}
  `, sandbox);

  assert.deepEqual(Array.from(sandbox.getAllModelsForProtocol('openai')), ['claude-3.7']);
  assert.deepEqual(Array.from(sandbox.getAllModelsForProtocol('anthropic')), ['claude-3.7', 'claude-4']);
  assert.deepEqual(
    sandbox.getChannelsSupportingModel('openai', 'claude-3.7').map((channel) => channel.id),
    [3]
  );
  sandbox.selectedModelType = 'openai';
  assert.deepEqual(Array.from(sandbox.getAllModelsForProtocol('openai')), ['gpt-4.1']);
  assert.deepEqual(
    sandbox.getChannelsSupportingModel('openai', 'gpt-4.1').map((channel) => channel.id),
    [2]
  );
});

test('按模型测试时已选模型会同时显示原生和协议转换支持的渠道', () => {
  const sandbox = {
    ALL_PROTOCOLS: ['anthropic', 'codex', 'openai', 'gemini'],
    selectedModelType: 'anthropic',
    channelsList: [
      { id: 1, name: 'openai-native', channel_type: 'openai', protocol_transforms: [], priority: 50, models: ['gpt-5.4'] },
      { id: 2, name: 'anthropic-native', channel_type: 'anthropic', protocol_transforms: [], priority: 40, models: ['gpt-5.4'] },
      { id: 3, name: 'codex-anthropic-transform', channel_type: 'codex', protocol_transforms: ['anthropic'], priority: 35, models: ['gpt-5.4'] },
      { id: 4, name: 'openai-anthropic-transform', channel_type: 'openai', protocol_transforms: ['anthropic'], priority: 30, models: ['gpt-5.4'] },
      { id: 5, name: 'openai-other-model', channel_type: 'openai', protocol_transforms: ['anthropic'], priority: 45, models: ['gpt-4.1'] }
    ]
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'getModelName')}
    ${extractFunction(script, 'normalizeProtocol')}
    ${extractFunction(script, 'getChannelType')}
    ${extractFunction(script, 'getExposedProtocols')}
    ${extractFunction(script, 'getSupportedProtocols')}
    ${extractFunction(script, 'channelExposesProtocol')}
    ${extractFunction(script, 'channelMatchesModelType')}
    ${extractFunction(script, 'isModelSupported')}
    ${extractFunction(script, 'getChannelsSupportingModel')}
  `, sandbox);

  assert.deepEqual(
    sandbox.getChannelsSupportingModel('anthropic', 'gpt-5.4').map((channel) => channel.id),
    [2, 3, 4]
  );
});

test('切换类型后会回退到该类型下的首个可用模型', () => {
  const sandbox = {
    TEST_MODE_MODEL: 'model',
    TEST_MODE_CHANNEL: 'channel',
    testMode: 'model',
    selectedProtocol: 'openai',
    selectedModelType: 'openai',
    selectedModelName: 'claude-4',
    channelsList: [
      { id: 1, channel_type: 'anthropic', models: ['claude-4'] },
      { id: 2, channel_type: 'openai', models: ['gpt-4.1', 'gpt-4.1-mini'] }
    ],
    modelSelectCombobox: { refreshCalled: 0, refresh() { this.refreshCalled += 1; } },
    ALL_PROTOCOLS: ['anthropic', 'codex', 'openai', 'gemini']
  };

  vm.runInNewContext(`
    function getModelInputValue() { return ''; }
    function setModelInputValue(value) { globalThis.lastModelValue = value; }
    ${extractFunction(script, 'normalizeProtocol')}
    ${extractFunction(script, 'getModelName')}
    ${extractFunction(script, 'getChannelType')}
    ${extractFunction(script, 'getExposedProtocols')}
    ${extractFunction(script, 'getSupportedProtocols')}
    ${extractFunction(script, 'channelExposesProtocol')}
    ${extractFunction(script, 'channelMatchesModelType')}
    ${extractFunction(script, 'getAvailableChannelTypes')}
    ${extractFunction(script, 'ensureSelectedModelType')}
    ${extractFunction(script, 'getAllModelsForProtocol')}
    ${extractFunction(script, 'populateModelSelector')}
  `, sandbox);

  sandbox.populateModelSelector();
  assert.equal(sandbox.selectedModelName, 'gpt-4.1');
  assert.equal(sandbox.modelSelectCombobox.refreshCalled, 1);
});

test('按模型测试恢复状态时会丢弃当前协议下无法渲染渠道的模型', () => {
  const sandbox = {
    TEST_MODE_MODEL: 'model',
    TEST_MODE_CHANNEL: 'channel',
    testMode: 'model',
    selectedProtocol: 'anthropic',
    selectedModelType: 'codex',
    selectedModelName: 'gpt-5.4',
    channelsList: [
      { id: 1, name: 'codex-native-only', channel_type: 'codex', protocol_transforms: [], priority: 50, models: ['gpt-5.4'] },
      { id: 2, name: 'codex-anthropic-ready', channel_type: 'codex', protocol_transforms: ['anthropic'], priority: 40, models: ['codex-ready'] }
    ],
    modelSelectCombobox: { refreshCalled: 0, refresh() { this.refreshCalled += 1; } },
    lastModelValue: '',
    ALL_PROTOCOLS: ['anthropic', 'codex', 'openai', 'gemini']
  };

  vm.runInNewContext(`
    function getModelInputValue() { return ''; }
    function setModelInputValue(value) { globalThis.lastModelValue = value; }
    ${extractFunction(script, 'normalizeProtocol')}
    ${extractFunction(script, 'getModelName')}
    ${extractFunction(script, 'getChannelType')}
    ${extractFunction(script, 'getExposedProtocols')}
    ${extractFunction(script, 'getSupportedProtocols')}
    ${extractFunction(script, 'channelExposesProtocol')}
    ${extractFunction(script, 'channelMatchesModelType')}
    ${extractFunction(script, 'getAvailableChannelTypes')}
    ${extractFunction(script, 'ensureSelectedModelType')}
    ${extractFunction(script, 'getAllModelsForProtocol')}
    ${extractFunction(script, 'isModelSupported')}
    ${extractFunction(script, 'getChannelsSupportingModel')}
    ${extractFunction(script, 'populateModelSelector')}
  `, sandbox);

  sandbox.populateModelSelector();

  assert.equal(sandbox.selectedModelName, 'codex-ready');
  assert.equal(sandbox.lastModelValue, 'codex-ready');
  assert.deepEqual(
    sandbox.getChannelsSupportingModel('anthropic', sandbox.selectedModelName).map((channel) => channel.id),
    [2]
  );
});

test('按模型测试协议切换导致模型输入框失焦时，同模型提交不重新渲染渠道表格', () => {
  const sandbox = {
    window: {
      createSearchableCombobox(config) {
        sandbox.capturedComboboxConfig = config;
        return { setValue() {}, refresh() {} };
      }
    },
    capturedComboboxConfig: null,
    saveSelectedModelNameToStorage() {}
  };

  vm.runInNewContext(`
    const TEST_MODE_MODEL = 'model';
    let testMode = TEST_MODE_MODEL;
    let selectedModelName = 'claude-4';
    let selectedProtocol = 'anthropic';
    let modelSelectCombobox = null;
    const modelSelect = {};
    let renderCalls = 0;
    function getAllModelsForProtocol() { return ['claude-4']; }
    function getModelInputValue() { return 'claude-4'; }
    function renderModelModeRows() { renderCalls += 1; }
    ${extractFunction(script, 'ensureModelSelectCombobox')}

    ensureModelSelectCombobox();
    capturedComboboxConfig.onSelect('claude-4');
    globalThis.result = { renderCalls, selectedModelName };
  `, sandbox);

  assert.equal(sandbox.result.selectedModelName, 'claude-4');
  assert.equal(sandbox.result.renderCalls, 0);
});

test('按渠道测试时重渲染不会覆盖用户已选的协议转换', () => {
  const sandbox = {
    ALL_PROTOCOLS: ['anthropic', 'codex', 'openai', 'gemini'],
    TEST_MODE_CHANNEL: 'channel',
    TEST_MODE_MODEL: 'model',
    testMode: 'channel',
    selectedProtocol: 'openai',
    selectedChannel: {
      channel_type: 'anthropic',
      protocol_transforms: ['openai']
    },
    channelsList: []
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'normalizeProtocol')}
    ${extractFunction(script, 'getChannelType')}
    ${extractFunction(script, 'getSupportedProtocols')}
    ${extractFunction(script, 'ensureSelectedProtocolForCurrentMode')}
  `, sandbox);

  sandbox.ensureSelectedProtocolForCurrentMode();
  assert.equal(sandbox.selectedProtocol, 'openai');
});

test('按渠道测试时协议选项不再因渠道未配置 protocol_transforms 而禁用', () => {
  const sandbox = {
    TEST_MODE_CHANNEL: 'channel',
    TEST_MODE_MODEL: 'model',
    testMode: 'channel',
    selectedProtocol: 'anthropic',
    selectedChannel: {
      channel_type: 'anthropic',
      protocol_transforms: []
    },
    channelsList: [],
    ALL_PROTOCOLS: ['anthropic', 'codex', 'openai', 'gemini'],
    protocolTransformOptions: { innerHTML: '' },
    protocolLabel(protocol) {
      return protocol;
    }
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'normalizeProtocol')}
    ${extractFunction(script, 'getChannelType')}
    ${extractFunction(script, 'getSupportedProtocols')}
    ${extractFunction(script, 'ensureSelectedProtocolForCurrentMode')}
    ${extractFunction(script, 'renderProtocolTransformOptions')}
  `, sandbox);

  sandbox.renderProtocolTransformOptions();
  assert.doesNotMatch(sandbox.protocolTransformOptions.innerHTML, /\bdisabled\b/);
});

test('按模型测试从按渠道测试返回时恢复模型模式的协议转换', () => {
  const sandbox = {
    TEST_MODE_CHANNEL: 'channel',
    TEST_MODE_MODEL: 'model',
    TEST_MODE_CHAT: 'chat',
    testMode: 'model',
    selectedProtocol: 'codex',
    selectedModelType: 'codex',
    selectedChannel: { channel_type: 'anthropic' },
    calls: []
  };

  vm.runInNewContext(`
    function saveTestModeToStorage(mode) { calls.push(['saveMode', mode]); }
    function clearProgress() {}
    function updateModeUI() {}
    function updateHeadByMode() {}
    function initChatPanel() {}
    function renderProtocolTransformOptions() {}
    function populateModelSelector() { calls.push(['populate', selectedProtocol]); }
    function renderRowsByMode() { calls.push(['render', selectedProtocol]); }
    let selectedModelModeProtocol = '';
    ${extractFunction(script, 'normalizeProtocol')}
    ${extractFunction(script, 'getChannelType')}
    ${extractFunction(script, 'setTestMode')}
  `, sandbox);

  sandbox.setTestMode('channel');
  assert.equal(sandbox.selectedProtocol, 'anthropic');

  sandbox.setTestMode('model');
  assert.equal(sandbox.selectedProtocol, 'codex');
  assert.equal(JSON.stringify(sandbox.calls.slice(-2)), JSON.stringify([['populate', 'codex'], ['render', 'codex']]));
});

test('按模型测试从非模型初始模式进入时使用持久化的模型协议', () => {
  const sandbox = {
    TEST_MODE_CHANNEL: 'channel',
    TEST_MODE_MODEL: 'model',
    TEST_MODE_CHAT: 'chat',
    testMode: 'channel',
    selectedProtocol: 'anthropic',
    selectedModelType: 'codex',
    selectedChannel: { channel_type: 'anthropic' },
    calls: []
  };

  vm.runInNewContext(`
    function saveTestModeToStorage(mode) { calls.push(['saveMode', mode]); }
    function loadSelectedProtocolFromStorage() { return 'codex'; }
    function clearProgress() {}
    function updateModeUI() {}
    function updateHeadByMode() {}
    function initChatPanel() {}
    function renderProtocolTransformOptions() {}
    function populateModelSelector() { calls.push(['populate', selectedProtocol]); }
    function renderRowsByMode() { calls.push(['render', selectedProtocol]); }
    let selectedModelModeProtocol = '';
    ${extractFunction(script, 'normalizeProtocol')}
    ${extractFunction(script, 'getChannelType')}
    ${extractFunction(script, 'setTestMode')}
  `, sandbox);

  sandbox.setTestMode('model');

  assert.equal(sandbox.selectedProtocol, 'codex');
  assert.equal(JSON.stringify(sandbox.calls.slice(-2)), JSON.stringify([['populate', 'codex'], ['render', 'codex']]));
});

test('applyTestResultToRow 在失败时优先展示结构化上游错误而不是泛化状态文案', () => {
  const cells = new Map([
    ['.first-byte-duration', { textContent: '', title: '' }],
    ['.duration', { textContent: '', title: '' }],
    ['.input-tokens', { textContent: '', title: '' }],
    ['.output-tokens', { textContent: '', title: '' }],
    ['.speed', { textContent: '', title: '' }],
    ['.cache-read', { textContent: '', title: '' }],
    ['.cache-create', { textContent: '', title: '' }],
    ['.cost', { textContent: '', title: '' }],
    ['.response', { textContent: '', title: '' }]
  ]);
  const row = {
    style: {},
    querySelector(selector) {
      const cell = cells.get(selector);
      assert.ok(cell, `缺少单元格 ${selector}`);
      return cell;
    }
  };
  const sandbox = {
    formatDurationMs(value) {
      return value ? `${value}ms` : '-';
    },
    formatCost(value) {
      return String(value);
    },
    i18nText(_key, fallback) {
      return fallback;
    }
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'applyTestResultToRow')}
  `, sandbox);

  sandbox.applyTestResultToRow(row, {
    success: false,
    duration_ms: 1503,
    status_code: 429,
    error: 'API返回错误状态: 429 Too Many Requests',
    api_error: {
      error: '由于负载过高，为了尽量保证用户体验，本站已开启限流，当前用户本周无法使用，请下周重试',
      type: 'error'
    }
  });

  assert.equal(
    cells.get('.response').textContent,
    '由于负载过高，为了尽量保证用户体验，本站已开启限流，当前用户本周无法使用，请下周重试'
  );
  assert.equal(
    cells.get('.response').title,
    '由于负载过高，为了尽量保证用户体验，本站已开启限流，当前用户本周无法使用，请下周重试'
  );
  assert.equal(cells.get('.duration').textContent, '1503ms');
  assert.equal(cells.get('.speed').textContent, '-');
  assert.equal(cells.get('.cost').textContent, '-');
});

test('模型测试遇到 RPM 限制时按秒更新等待提示后重试同一行', async () => {
  const responseHistory = [];
  const responseCell = {
    title: '',
    get textContent() {
      return responseHistory[responseHistory.length - 1] || '';
    },
    set textContent(value) {
      responseHistory.push(String(value || ''));
    }
  };
  const row = {
    style: {},
    querySelector(selector) {
      assert.equal(selector, '.response');
      return responseCell;
    }
  };

  const fetchCalls = [];
  const sleepCalls = [];
  const sandbox = {
    fetchDataWithAuth: async (url, options) => {
      fetchCalls.push({ url, options });
      if (fetchCalls.length === 1) {
        return { success: false, rpm_limited: true, retry_after_ms: 3234, error: '渠道已达到RPM限制' };
      }
      return { success: true, response_text: 'ok' };
    },
    i18nText(_key, fallback, vars) {
      return String(fallback).replace('{seconds}', String(vars?.seconds ?? ''));
    }
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'isRPMLimitedTestResult')}
    ${extractFunction(script, 'getRPMRetryDelayMs')}
    ${extractFunction(script, 'markModelTestRPMWait')}
    ${extractFunction(script, 'sleepModelTest')}
    ${extractFunction(script, 'waitModelTestRPMRetry')}
    ${extractFunction(script, 'fetchModelTestWithRPMWait')}
  `, sandbox);
  sandbox.sleepModelTest = async (delayMs) => {
    sleepCalls.push(delayMs);
  };

  const result = await sandbox.fetchModelTestWithRPMWait(
    { row, channelId: 154 },
    { model: 'qwen3-vl-plus', stream: false, content: 'hi', protocol_transform: 'anthropic' }
  );

  assert.equal(result.success, true);
  assert.equal(fetchCalls.length, 2);
  assert.equal(fetchCalls[0].url, '/admin/channels/154/test');
  assert.equal(fetchCalls[1].url, '/admin/channels/154/test');
  assert.deepEqual(sleepCalls, [1000, 1000, 1000, 234]);
  assert.ok(responseHistory.includes('RPM限制，等待 4s 后重试'));
  assert.ok(responseHistory.includes('RPM限制，等待 3s 后重试'));
  assert.ok(responseHistory.includes('RPM限制，等待 2s 后重试'));
  assert.ok(responseHistory.includes('RPM限制，等待 1s 后重试'));
  assert.equal(responseHistory[responseHistory.length - 1], '测试中...');
});
