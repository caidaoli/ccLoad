const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'model-test.html'), 'utf8');
const script = fs.readFileSync(path.join(__dirname, 'model-test.js'), 'utf8');
const sharedCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'styles.css'), 'utf8');

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

test('model-test 页静态控件不再使用内联事件', () => {
  assert.doesNotMatch(html, /onclick="(?:setTestMode|fetchAndAddModels|deleteSelectedModels|runModelTests)\([^"]*\)"/);
  assert.doesNotMatch(html, /onchange="toggleAllModels\(this\.checked\)"/);
  assert.match(html, /data-action="set-test-mode"/);
  assert.doesNotMatch(html, /data-action="select-all-models"/);
  assert.doesNotMatch(html, /data-action="deselect-all-models"/);
  assert.match(html, /data-action="fetch-and-add-models"/);
  assert.match(html, /data-action="open-add-models-modal"/);
  assert.match(html, /data-action="delete-selected-models"/);
  assert.match(html, /data-action="run-model-tests"/);
  assert.match(html, /data-change-action="toggle-all-models"/);
});

test('model-test 页在按模型测试表头提供批量添加模型按钮和弹窗', () => {
  assert.match(html, /id="fetchModelsBtn"[\s\S]*?id="addModelsBtn"[\s\S]*?id="deleteModelsBtn"[\s\S]*?id="runTestBtn"/);
  assert.match(html, /<div id="addModelsModal" class="modal">/);
  assert.match(html, /<textarea[\s\S]*id="addModelsTextarea"[\s\S]*data-i18n-placeholder="modelTest\.addModelsPlaceholder"/);
  assert.match(html, /id="addModelsConfirmBtn"[\s\S]*data-i18n="modelTest\.addModelsConfirm"/);
});

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

test('model-test 页接入日志页同款渠道编辑器桥接并将渠道名渲染为可点击按钮', () => {
  assert.match(html, /<link rel="stylesheet" href="\/web\/assets\/css\/channels\.css\?v=__VERSION__">/);
  assert.match(html, /<script defer src="\/web\/assets\/js\/logs-channel-editor\.js\?v=__VERSION__"><\/script>/);
  assert.match(html, /<button type="button" class="channel-link" data-channel-id="{{channelId}}" title="{{channelName}}">{{channelName}}<\/button>/);
  assert.match(html, /<button type="button" class="channel-link" data-channel-id="{{channelId}}" title="{{model}}">{{displayName}}<\/button>/);
});

test('model-test 页在按模型测试模式下提供类型筛选并保留协议转换容器', () => {
  assert.match(html, /id="modelTypeLabel"/);
  assert.match(html, /id="testModelType"/);
  assert.match(html, /class="model-test-control model-test-control--type hidden"/);
  assert.match(html, /data-i18n="common\.type"/);
  assert.match(html, /id="protocolTransformContainer"/);
  assert.match(html, /id="protocolTransformOptions"/);
  assert.match(html, /data-i18n="modelTest\.protocolTransform"/);
  assert.match(html, /id="modelTypeLabel"[\s\S]*?id="modelSelectorLabel"[\s\S]*?id="protocolTransformContainer"[\s\S]*?id="streamEnabled"[\s\S]*?id="concurrency"[\s\S]*?id="modelTestContent"/);
});

test('model-test.js 使用集中绑定处理页面控件和重渲染表头复选框', () => {
  assert.match(script, /window\.initPageBootstrap\(\{/);
  assert.match(script, /topbarKey:\s*'model-test'/);
  assert.match(script, /function initModelTestActions\(\)/);
  assert.match(script, /window\.initDelegatedActions\(\{/);
  assert.match(script, /boundKey:\s*'modelTestActionsBound'/);
  assert.match(script, /'set-test-mode':\s*\(actionTarget\)\s*=> setTestMode\(actionTarget\.dataset\.mode \|\| ''\)/);
  assert.match(script, /'fetch-and-add-models':\s*\(\)\s*=> fetchAndAddModels\(\)/);
  assert.match(script, /'open-add-models-modal':\s*\(\)\s*=> openAddModelsModal\(\)/);
  assert.match(script, /'delete-selected-models':\s*\(\)\s*=> deleteSelectedModels\(\)/);
  assert.match(script, /'run-model-tests':\s*\(\)\s*=> runModelTests\(\)/);
  assert.match(script, /'toggle-all-models':\s*\(actionTarget\)\s*=> toggleAllModels\(actionTarget\.checked\)/);
  assert.match(script, /data-change-action="toggle-all-models"/);
  assert.doesNotMatch(script, /onchange="toggleAllModels/);
  assert.match(script, /const toolbar = document\.querySelector\('\.model-test-toolbar'\);/);
  assert.match(script, /toolbar\?\.classList\.toggle\('model-test-toolbar--model-mode',\s*isModelMode\)/);
  assert.match(script, /bootstrap\(\);/);
});

test('model-test.js 将表头操作按钮渲染进响应内容列并阻止按钮点击触发表头排序', () => {
  assert.match(script, /const RESPONSE_HEAD_HTML = `[\s\S]*?class="table-col-response model-test-response-head"[\s\S]*?class="model-test-toolbar-section model-test-toolbar-section--actions model-test-head-actions"[\s\S]*?id="fetchModelsBtn"[\s\S]*?id="addModelsBtn"[\s\S]*?id="deleteModelsBtn"[\s\S]*?id="runTestBtn"[\s\S]*?`;/);
  assert.match(script, /const CHANNEL_MODE_HEAD = `[\s\S]*?\$\{RESPONSE_HEAD_HTML\}[\s\S]*?`;/);
  assert.match(script, /const MODEL_MODE_HEAD = `[\s\S]*?\$\{RESPONSE_HEAD_HTML\}[\s\S]*?`;/);
  assert.match(script, /th\.onclick = \(event\) => \{[\s\S]*?closest\('\.model-test-head-actions'\)[\s\S]*?return;/);
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

test('model-test.js 移除测试数量进度文案，只保留按钮自身测试中状态', () => {
  assert.doesNotMatch(script, /testingProgress/);
  assert.doesNotMatch(script, /completedProgress/);
  assert.match(script, /runTestBtn\.textContent = i18nText\('modelTest\.testing', '测试中\.\.\.'\)/);
});

test('model-test.js 两个模式下都把渠道按钮点击委托到编辑弹窗', () => {
  assert.match(script, /const channelBtn = event\.target\.closest\('\.channel-link\[data-channel-id\]'\);/);
  assert.match(script, /if \(!channelBtn\) return;/);
  assert.match(script, /openLogChannelEditor\(channelId\)/);
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

test('model-test.js 渠道编辑器保存后通过 preserveSelection 重新加载渠道并保留选中', () => {
  assert.match(script, /async function loadChannels\(options = \{\}\)/);
  assert.match(script, /const \{ preserveSelection = false \} = options;/);
  assert.match(script, /if \(preserveSelection && preservedChannelId !== null\)/);
  assert.match(script, /window\.ChannelModalHooks = \{[\s\S]*?afterSave:[\s\S]*?loadChannels\(\{ preserveSelection: true \}\)/);
});

test('model-test.js 渠道搜索下拉在重建时通过 initialValue/initialLabel 保持显示当前渠道', () => {
  assert.match(script, /const initialValue = selectedChannel \? String\(selectedChannel\.id\) : '';/);
  assert.match(script, /const initialLabel = selectedChannel \? `\[\$\{getChannelType\(selectedChannel\)\}\] \$\{selectedChannel\.name\}` : '';/);
  assert.match(script, /initialValue,\s*\n\s*initialLabel,/);
});

test('model-test.js 在模型模式重渲染时保留渠道勾选状态', () => {
  assert.match(script, /function getRowSelectionKey\(row\)/);
  assert.match(script, /function captureRowSelectionState\(\)/);
  assert.match(script, /function restoreRowSelectionState\(row,\s*selectionState,\s*fallbackChecked = true\)/);
  assert.match(script, /const previousSelectionState = captureRowSelectionState\(\);[\s\S]*?restoreRowSelectionState\(row,\s*previousSelectionState,\s*isEnabled\);/);

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

test('model-test 页渠道按钮去掉默认按钮边框和底色', () => {
  assert.match(sharedCss, /\.model-test-table\s+\.channel-link\s*\{[\s\S]*?padding:\s*0;[\s\S]*?border:\s*none;[\s\S]*?background:\s*transparent;/);
});

test('按协议测试时模型模式按类型和协议联动模型与渠道', () => {
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
    ${extractFunction(script, 'channelSupportsProtocol')}
    ${extractFunction(script, 'channelMatchesModelType')}
    ${extractFunction(script, 'isModelSupported')}
    ${extractFunction(script, 'getAllModelsForProtocol')}
    ${extractFunction(script, 'getChannelsSupportingModel')}
  `, sandbox);

  assert.deepEqual(Array.from(sandbox.getAllModelsForProtocol('openai')), ['claude-3.7', 'claude-4', 'gpt-4.1']);
  assert.deepEqual(Array.from(sandbox.getAllModelsForProtocol('anthropic')), ['claude-3.7', 'claude-4']);
  assert.deepEqual(
    sandbox.getChannelsSupportingModel('openai', 'claude-3.7').map((channel) => channel.id),
    [3]
  );
  sandbox.selectedModelType = 'openai';
  assert.deepEqual(Array.from(sandbox.getAllModelsForProtocol('openai')), ['claude-3.7', 'gpt-4.1']);
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
    ${extractFunction(script, 'channelSupportsProtocol')}
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
    lastModelValue: '',
    setModelInputValue(value) { globalThis.lastModelValue = value; },
    getModelInputValue() { return ''; },
    ALL_PROTOCOLS: ['anthropic', 'codex', 'openai', 'gemini']
  };

  vm.runInNewContext(`
    ${extractFunction(script, 'normalizeProtocol')}
    ${extractFunction(script, 'getModelName')}
    ${extractFunction(script, 'getChannelType')}
    ${extractFunction(script, 'getExposedProtocols')}
    ${extractFunction(script, 'getSupportedProtocols')}
    ${extractFunction(script, 'channelExposesProtocol')}
    ${extractFunction(script, 'channelSupportsProtocol')}
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

test('model-test.js 开始测试时发送 protocol_transform 而不是 channel_type', () => {
  assert.match(script, /const selectedProtocol = protocolTransform;/);
  assert.match(script, /protocol_transform:\s*selectedProtocol/);
  assert.doesNotMatch(script, /body:\s*JSON\.stringify\(\{[\s\S]*channel_type:\s*channelType[\s\S]*\}\)/);
});

test('切换渠道后协议默认回退到渠道原生协议', () => {
  assert.match(script, /selectedProtocol\s*=\s*getChannelType\(selectedChannel\)/);
});

test('按模型测试切换协议转换时不重新渲染渠道表格', () => {
  const handlerMatch = script.match(
    /protocolTransformOptions\?\.addEventListener\('change',\s*\(event\)\s*=>\s*\{[\s\S]*?\}\);/
  );
  assert.ok(handlerMatch, '未定位到协议转换 change 处理器');
  const handler = handlerMatch[0];
  assert.match(handler, /if\s*\(testMode\s*===\s*TEST_MODE_MODEL\)\s*\{\s*return;\s*\}/);
  assert.doesNotMatch(handler, /populateModelSelector\(\)/);
  assert.doesNotMatch(handler, /renderModelModeRows\(\)/);
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
