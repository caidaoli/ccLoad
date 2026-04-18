const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const html = fs.readFileSync(path.join(__dirname, '..', '..', 'channels.html'), 'utf8');
const protocolSource = fs.readFileSync(path.join(__dirname, 'channels-protocols.js'), 'utf8');
const source = fs.readFileSync(path.join(__dirname, 'channels-modals.js'), 'utf8');

const CHANNEL_TYPES = ['anthropic', 'codex', 'openai', 'gemini'];
const KEY_STRATEGIES = ['sequential', 'round_robin'];

function createClassList() {
  const names = new Set();
  return {
    add(...tokens) {
      tokens.filter(Boolean).forEach((token) => names.add(token));
    },
    remove(...tokens) {
      tokens.forEach((token) => names.delete(token));
    },
    contains(token) {
      return names.has(token);
    }
  };
}

function createElement(props = {}) {
  const listeners = new Map();
  const attributes = new Map();

  const element = {
    id: props.id || '',
    name: props.name || '',
    type: props.type || '',
    value: props.value || '',
    checked: !!props.checked,
    disabled: !!props.disabled,
    hidden: !!props.hidden,
    dataset: props.dataset || {},
    style: props.style || {},
    textContent: props.textContent || '',
    children: [],
    classList: createClassList(),
    appendChild(child) {
      this.children.push(child);
      return child;
    },
    addEventListener(type, handler) {
      const current = listeners.get(type) || [];
      current.push(handler);
      listeners.set(type, current);
    },
    async dispatchEvent(event) {
      const nextEvent = event || {};
      const type = nextEvent.type;
      if (!type) return;
      nextEvent.target = nextEvent.target || element;
      nextEvent.currentTarget = element;
      if (typeof nextEvent.preventDefault !== 'function') {
        nextEvent.preventDefault = () => {};
      }
      const handlers = listeners.get(type) || [];
      for (const handler of handlers) {
        await handler(nextEvent);
      }
    },
    setAttribute(name, value) {
      attributes.set(name, String(value));
    },
    getAttribute(name) {
      return attributes.get(name);
    },
    reset() {},
    focus() {}
  };

  return element;
}

function createHarness({
  channel = null,
  apiKeys = [{ api_key: 'sk-test' }],
  channelCheckIntervalHours = 24
} = {}) {
  let protocolTransformInputs = [];
  let protocolTransformModeInputs = [];
  const elements = {};
  const radiosByName = new Map();
  const fetchCalls = [];
  let afterSavePayload = null;

  function registerRadio(name, value, checked = false) {
    const radio = createElement({ name, value, type: 'radio', checked });
    if (!radiosByName.has(name)) {
      radiosByName.set(name, []);
    }
    radiosByName.get(name).push(radio);
    return radio;
  }

  function setCheckedRadio(name, value) {
    for (const radio of radiosByName.get(name) || []) {
      radio.checked = radio.value === value;
    }
  }

  function getCheckedRadio(name) {
    return (radiosByName.get(name) || []).find((radio) => radio.checked) || null;
  }

  function getRadio(name, value) {
    return (radiosByName.get(name) || []).find((radio) => radio.value === value) || null;
  }

  function queryInputs(selector) {
    const match = selector.match(/^input\[name="([^"]+)"\](?:\[value="([^"]+)"\])?(?::checked)?$/);
    if (!match) return [];

    const [, name, value] = match;
    const checkedOnly = selector.endsWith(':checked');
    const pool = name === 'protocolTransform'
      ? protocolTransformInputs
      : name === 'protocolTransformMode'
        ? protocolTransformModeInputs
      : radiosByName.get(name) || [];

    return pool.filter((input) => {
      if (value && input.value !== value) return false;
      if (checkedOnly && !input.checked) return false;
      return true;
    });
  }

  function parseProtocolTransformInputs(markup) {
    const inputs = [];
    const regex = /<input type="checkbox"[\s\S]*?name="protocolTransform"[\s\S]*?value="([^"]+)"([\s\S]*?)>/g;
    let match;
    while ((match = regex.exec(markup))) {
      inputs.push(createElement({
        name: 'protocolTransform',
        type: 'checkbox',
        value: match[1],
        checked: /\bchecked\b/.test(match[2]),
        disabled: /\bdisabled\b/.test(match[2])
      }));
    }
    protocolTransformInputs = inputs;
  }

  function parseProtocolTransformModeInputs(markup) {
    const inputs = [];
    const regex = /<input type="radio"[\s\S]*?name="protocolTransformMode"[\s\S]*?value="([^"]+)"([\s\S]*?)>/g;
    let match;
    while ((match = regex.exec(markup))) {
      inputs.push(createElement({
        name: 'protocolTransformMode',
        type: 'radio',
        value: match[1],
        checked: /\bchecked\b/.test(match[2]),
        disabled: /\bdisabled\b/.test(match[2])
      }));
    }
    protocolTransformModeInputs = inputs;
  }

  elements.protocolTransformsContainer = createElement({ id: 'protocolTransformsContainer' });
  Object.defineProperty(elements.protocolTransformsContainer, 'innerHTML', {
    get() {
      return this._innerHTML || '';
    },
    set(value) {
      this._innerHTML = value;
      parseProtocolTransformInputs(String(value || ''));
    }
  });
  elements.protocolTransformModeContainer = createElement({ id: 'protocolTransformModeContainer' });
  Object.defineProperty(elements.protocolTransformModeContainer, 'innerHTML', {
    get() {
      return this._innerHTML || '';
    },
    set(value) {
      this._innerHTML = value;
      parseProtocolTransformModeInputs(String(value || ''));
    }
  });

  elements.channelTypeRadios = createElement({ id: 'channelTypeRadios', dataset: {} });
  elements.channelForm = createElement({ id: 'channelForm', dataset: {} });
  elements.channelName = createElement({ id: 'channelName', value: channel ? channel.name : '协议转换渠道' });
  elements.channelUrl = createElement({ id: 'channelUrl', value: '' });
  elements.channelApiKey = createElement({ id: 'channelApiKey', value: '' });
  elements.channelPriority = createElement({ id: 'channelPriority', value: channel ? String(channel.priority || 0) : '0' });
  elements.channelDailyCostLimit = createElement({ id: 'channelDailyCostLimit', value: channel ? String(channel.daily_cost_limit || 0) : '0' });
  elements.channelEnabled = createElement({ id: 'channelEnabled', type: 'checkbox', checked: channel ? channel.enabled !== false : true });
  elements.channelScheduledCheckEnabled = createElement({ id: 'channelScheduledCheckEnabled', type: 'checkbox', checked: !!(channel && channel.scheduled_check_enabled), dataset: {} });
  elements.channelScheduledCheckModel = createElement({ id: 'channelScheduledCheckModel', value: channel ? channel.scheduled_check_model || '' : '' });
  elements.channelScheduledCheckModelInput = createElement({ id: 'channelScheduledCheckModelInput', value: '' });
  elements.channelScheduledCheckModelDropdown = createElement({ id: 'channelScheduledCheckModelDropdown', dataset: {}, style: {} });
  elements.channelScheduledCheckModelWrapper = createElement({ id: 'channelScheduledCheckModelWrapper', hidden: false });
  elements.channelScheduledCheckEnabledWrapper = createElement({ id: 'channelScheduledCheckEnabledWrapper', hidden: false });
  elements.channelScheduledCheckModelHint = createElement({ id: 'channelScheduledCheckModelHint', textContent: '' });
  elements.channelModal = createElement({ id: 'channelModal' });
  elements.inlineEyeIcon = createElement({ id: 'inlineEyeIcon', style: {} });
  elements.inlineEyeOffIcon = createElement({ id: 'inlineEyeOffIcon', style: {} });
  elements.modelFilterInput = createElement({ id: 'modelFilterInput', value: '' });
  elements.redirectCount = createElement({ id: 'redirectCount', textContent: '0' });
  elements.redirectTableBody = createElement({ id: 'redirectTableBody' });
  elements.selectAllModels = createElement({ id: 'selectAllModels', type: 'checkbox', checked: false });

  CHANNEL_TYPES.forEach((type, index) => {
    registerRadio('channelType', type, index === 0);
  });
  KEY_STRATEGIES.forEach((type, index) => {
    registerRadio('keyStrategy', type, index === 0);
  });

  const sandbox = {
    console,
    alert() {},
    confirm() { return true; },
    editingChannelId: null,
    channelFormDirty: false,
    channels: channel ? [channel] : [],
    currentChannelKeyCooldowns: [],
    redirectTableData: channel && Array.isArray(channel.models)
      ? channel.models.map((model) => ({ model: model.model || '', redirect_model: model.redirect_model || '' }))
      : [{ model: 'claude-3-7-sonnet', redirect_model: '' }],
    selectedModelIndices: new Set(),
    selectedURLIndices: new Set(),
    inlineURLTableData: channel ? String(channel.url || '').split('\n').filter(Boolean) : ['https://api.example.com'],
    inlineKeyTableData: apiKeys.map((key) => key.api_key || key),
    inlineKeyVisible: true,
    currentModelFilter: '',
    deletingChannelRequest: null,
    selectedChannelIds: new Set(),
    filters: { channelType: 'all' },
    resetChannelFormDirty() {
      sandbox.channelFormDirty = false;
    },
    markChannelFormDirty() {
      sandbox.channelFormDirty = true;
    },
    renderInlineKeyTable() {},
    renderInlineURLTable() {},
    renderRedirectTable() {},
    fetchURLStats() {},
    clearChannelsCache() {},
    loadChannels: async () => {},
    saveChannelsFilters() {},
    normalizeSelectedChannelID(value) { return String(value); },
    setInlineURLTableData(value) {
      sandbox.inlineURLTableData = String(value || '').split('\n').filter(Boolean);
    },
    getValidInlineURLs() {
      return sandbox.inlineURLTableData.filter((url) => url && url.trim());
    },
    createSearchableCombobox(config) {
      return {
        setValue(value, label) {
          elements.channelScheduledCheckModel.value = value;
          elements.channelScheduledCheckModelInput.value = label;
        },
        refresh() {},
        getInput() {
          return elements.channelScheduledCheckModelInput;
        }
      };
    },
    fetchDataWithAuth: async (requestPath) => {
      if (requestPath === '/admin/settings/channel_check_interval_hours') {
        return { value: channelCheckIntervalHours };
      }
      if (channel && requestPath === `/admin/channels/${channel.id}/keys`) {
        return apiKeys;
      }
      throw new Error(`unexpected fetchDataWithAuth: ${requestPath}`);
    },
    fetchAPIWithAuth: async (requestPath, options) => {
      fetchCalls.push({ path: requestPath, options });
      return { success: true };
    },
    document: {
      body: {},
      createDocumentFragment() {
        return {
          children: [],
          appendChild(child) {
            this.children.push(child);
          }
        };
      },
      getElementById(id) {
        return elements[id] || null;
      },
      querySelector(selector) {
        return queryInputs(selector)[0] || null;
      },
      querySelectorAll(selector) {
        return queryInputs(selector);
      }
    },
    window: {
      t(key) {
        const labels = {
          'channels.protocolTransformAnthropic': 'Claude Code',
          'channels.protocolBadgeAnthropic': 'Claude',
          'channels.protocolTransformCodex': 'Codex',
          'channels.protocolTransformOpenAI': 'OpenAI',
          'channels.protocolTransformGemini': 'Gemini',
          'channels.protocolTransformNative': '原生',
          'channels.protocolTransformModeLocal': 'ccLoad(实验性)',
          'channels.protocolTransformModeUpstream': '上游',
          'channels.duplicateModelsNotAllowed': 'duplicate models',
          'channels.fillAllRequired': 'fill required',
          'channels.channelAdded': 'added',
          'channels.channelUpdated': 'updated',
          'channels.scheduledCheckModelDefault': '默认首个模型',
          'channels.scheduledCheckModelHint': '仅用于定时检测，留空表示默认首个模型',
          'channels.scheduledCheckModelFallback': '当前检测模型已失效，已回退为默认首个模型',
          'channels.saveFailed': 'save failed',
          'channels.fetchModelsSuccess': 'fetch models success',
          'channels.fetchModelsFailed': 'fetch models failed',
          'channels.addedCommonModels': 'added common models',
          'channels.noPresetModels': 'no preset models',
          'channels.unsavedChanges': 'unsaved changes'
        };
        return labels[key] || key;
      },
      initDelegatedActions() {},
      ChannelTypeManager: {
        async renderChannelTypeRadios(_containerId, currentType) {
          setCheckedRadio('channelType', currentType || 'anthropic');
        }
      },
      ChannelModalHooks: {
        async afterSave(payload) {
          afterSavePayload = payload;
        }
      },
      TemplateEngine: {
        render(_templateId, data = {}) {
          return {
            dataset: { model: data.model || '', redirectModel: data.redirect_model || '' },
            querySelector() { return null; }
          };
        }
      },
      showSuccess() {},
      showError(message) {
        throw new Error(message);
      }
    },
    TemplateEngine: {
      render(_templateId, data = {}) {
        return {
          dataset: { model: data.model || '', redirectModel: data.redirect_model || '' },
          querySelector() { return null; }
        };
      }
    }
  };

  vm.createContext(sandbox);
  vm.runInContext(`${protocolSource}\n${source}\nthis.__protocolTransformsTest = {\n  initChannelEditorActions,\n  renderProtocolTransformOptions,\n  getSelectedProtocolTransforms,\n  editChannel,\n  saveChannel\n};`, sandbox);

  return {
    api: sandbox.__protocolTransformsTest,
    elements,
    fetchCalls,
    getAfterSavePayload: () => afterSavePayload,
    getProtocolTransformInput(value) {
      return protocolTransformInputs.find((input) => input.value === value) || null;
    },
    getProtocolTransformValues() {
      return protocolTransformInputs.map((input) => ({
        value: input.value,
        checked: input.checked,
        disabled: input.disabled
      }));
    },
    getProtocolTransformModeInput(value) {
      return protocolTransformModeInputs.find((input) => input.value === value) || null;
    },
    getRadio,
    setCheckedRadio,
    async changeChannelType(nextType) {
      const target = getRadio('channelType', nextType);
      assert.ok(target, `missing channelType radio: ${nextType}`);
      setCheckedRadio('channelType', nextType);
      await elements.channelTypeRadios.dispatchEvent({ type: 'change', target });
    },
    async submitForm() {
      await elements.channelForm.dispatchEvent({ type: 'submit' });
    }
  };
}

test('channels 编辑弹窗按两行两列展示协议配置', () => {
  assert.match(html, /id="protocolTransformsContainer"/);
  assert.match(html, /id="protocolTransformModeContainer"/);
  assert.match(
    html,
    /class="channel-editor-primary-row"[\s\S]*?class="channel-editor-primary-field channel-editor-primary-field--name"[\s\S]*?data-i18n="channels\.channelName"[\s\S]*?class="channel-editor-primary-field channel-editor-primary-field--type"[\s\S]*?data-i18n="channels\.modal\.upstreamProtocol"[\s\S]*?id="channelTypeRadios"/
  );
  assert.match(
    html,
    /class="channel-editor-primary-row"[\s\S]*?class="channel-editor-primary-field channel-editor-primary-field--transforms"[\s\S]*?data-i18n="channels\.modal\.protocolTransforms"[\s\S]*?id="protocolTransformsContainer"[\s\S]*?class="channel-editor-primary-field channel-editor-primary-field--mode"[\s\S]*?data-i18n="channels\.modal\.protocolTransformMode"[\s\S]*?id="protocolTransformModeContainer"/
  );
});

test('Gemini 协议转换选项在标签后内联渲染实验性提示', () => {
  const harness = createHarness();

  harness.api.renderProtocolTransformOptions('anthropic', ['gemini']);

  assert.match(
    harness.elements.protocolTransformsContainer.innerHTML,
    /value="gemini"[\s\S]*?channel-editor-radio-option-copy--with-hint[\s\S]*?>Gemini<\/span>[\s\S]*?class="channel-editor-radio-hint"[\s\S]*?data-i18n="channels\.modal\.protocolTransformsHint"[\s\S]*?额外暴露协议,不含原生上游协议/
  );
});

test('切换 channel type 后会禁用并剔除原生 protocol transform，仅保留额外协议', async () => {
	const harness = createHarness();
	harness.api.initChannelEditorActions();
  harness.api.renderProtocolTransformOptions('anthropic', ['openai', 'codex']);

  assert.deepEqual(
    harness.getProtocolTransformValues().map((item) => item.value),
    ['anthropic', 'codex', 'openai', 'gemini']
  );
  assert.equal(harness.getProtocolTransformInput('anthropic').disabled, true);
  assert.equal(harness.getProtocolTransformInput('anthropic').checked, false);
  assert.equal(harness.getProtocolTransformInput('openai').checked, true);
  assert.equal(harness.getProtocolTransformInput('codex').checked, true);
  assert.equal(harness.getProtocolTransformInput('gemini').checked, false);

  await harness.changeChannelType('openai');

  assert.deepEqual(
    harness.getProtocolTransformValues().map((item) => item.value),
    ['anthropic', 'codex', 'openai', 'gemini']
  );
  assert.equal(harness.getProtocolTransformInput('openai').disabled, true);
  assert.equal(harness.getProtocolTransformInput('openai').checked, false);
  assert.equal(harness.getProtocolTransformInput('codex').checked, true);
  assert.equal(harness.getProtocolTransformInput('anthropic').checked, false);
  assert.equal(harness.getProtocolTransformInput('gemini').checked, false);
  assert.deepEqual(
    JSON.parse(JSON.stringify(harness.api.getSelectedProtocolTransforms('openai'))),
    ['codex']
	);
});

test('所有渠道类型都渲染完整四协议集合，仅禁用原生协议', () => {
  const harness = createHarness();

  for (const channelType of CHANNEL_TYPES) {
    harness.api.renderProtocolTransformOptions(channelType, ['anthropic', 'codex', 'openai', 'gemini']);
    assert.deepEqual(
      harness.getProtocolTransformValues().map((item) => item.value),
      ['anthropic', 'codex', 'openai', 'gemini']
    );
    for (const protocol of CHANNEL_TYPES) {
      const input = harness.getProtocolTransformInput(protocol);
      assert.ok(input, `missing protocol transform input: ${protocol}`);
      assert.equal(input.disabled, protocol === channelType);
      assert.equal(input.checked, protocol !== channelType);
    }
  }
});

test('编辑渠道时会回填 protocol_transforms，并禁用原生协议选项', async () => {
  const harness = createHarness({
    channel: {
      id: 7,
      name: 'edited-channel',
      url: 'https://api.example.com',
      channel_type: 'gemini',
      protocol_transform_mode: 'upstream',
      protocol_transforms: ['openai', 'anthropic'],
      key_strategy: 'sequential',
      priority: 9,
      daily_cost_limit: 0,
      enabled: true,
      scheduled_check_enabled: false,
      scheduled_check_model: '',
      models: [{ model: 'gpt-5.4', redirect_model: '' }]
    },
    apiKeys: [{ api_key: 'sk-live' }]
  });

  await harness.api.editChannel(7);

  assert.equal(harness.getRadio('channelType', 'gemini').checked, true);
  assert.equal(harness.getProtocolTransformInput('gemini').disabled, true);
  assert.equal(harness.getProtocolTransformInput('gemini').checked, false);
  assert.equal(harness.getProtocolTransformInput('anthropic').checked, true);
  assert.equal(harness.getProtocolTransformInput('openai').checked, true);
  assert.equal(harness.getProtocolTransformModeInput('upstream').checked, true);
  assert.deepEqual(
    harness.getProtocolTransformValues().filter((item) => item.checked).map((item) => item.value).sort(),
    ['anthropic', 'openai']
  );
});

test('保存渠道时 payload 带上 protocol_transforms', async () => {
  const harness = createHarness();
  harness.api.initChannelEditorActions();
  harness.setCheckedRadio('channelType', 'gemini');
  harness.api.renderProtocolTransformOptions('gemini', ['anthropic', 'openai']);
  harness.elements.protocolTransformModeContainer.innerHTML = `
    <label><input type="radio" name="protocolTransformMode" value="local"></label>
    <label><input type="radio" name="protocolTransformMode" value="upstream" checked></label>
  `;

  await harness.submitForm();

  assert.equal(harness.fetchCalls.length, 1);
  const [{ path: requestPath, options }] = harness.fetchCalls;
  assert.equal(requestPath, '/admin/channels');
  assert.equal(options.method, 'POST');

  const payload = JSON.parse(options.body);
  assert.deepEqual(payload.protocol_transforms, ['anthropic', 'openai']);
  assert.equal(payload.protocol_transform_mode, 'upstream');
  assert.equal(payload.channel_type, 'gemini');
  assert.deepEqual(JSON.parse(JSON.stringify(harness.getAfterSavePayload())), {
    isNewChannel: true,
    newChannelType: 'gemini',
    savedChannelId: null,
    response: { success: true }
  });
});
