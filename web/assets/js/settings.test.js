const assert = require('node:assert/strict');
const test = require('node:test');

const confirmMessage = '保存任意设置后,服务会在约 2 秒后自动重启以生效，是否继续';

function flushAsyncWork() {
  return new Promise((resolve) => setImmediate(resolve));
}

async function loadSettingsPage(t, settings, inputValues, { filterModels = [] } = {}) {
  const clickListeners = [];
  const bodyListeners = new Map();
  const multimodalModalListeners = new Map();
  const multimodalModalClasses = new Set();
  const customPricingModalListeners = new Map();
  const customPricingModalClasses = new Set();
  let multimodalRows = [];
  let customPricingRows = [];
  let renderedMultimodalHTML = '';
  let renderedCustomPricingHTML = '';
  const saveButton = {
    dataset: {},
    addEventListener(type, listener) {
      if (type === 'click') clickListeners.push(listener);
    },
    click() {
      for (const listener of clickListeners) listener();
    }
  };
  const updateButton = {
    dataset: { action: 'check-for-updates' },
    disabled: false,
    attributes: new Map(),
    closest(selector) {
      return selector === '[data-action="check-for-updates"]' ? this : null;
    },
    setAttribute(name, value) {
      this.attributes.set(name, String(value));
    },
    getAttribute(name) {
      return this.attributes.get(name) ?? null;
    },
    removeAttribute(name) {
      this.attributes.delete(name);
    }
  };
  const typeSafeButton = {
    ...updateButton, attributes: new Map(),
    closest(selector) { return selector === '[data-action="test-typesafe"]' ? this : null; }
  };
  const settingsBody = {
    dataset: {},
    innerHTML: '',
    addEventListener(type, listener) {
      bodyListeners.set(type, listener);
    },
    appendChild() {}
  };
  const multimodalApplyButton = {
    dataset: { action: 'apply-multimodal-fallback' },
    disabled: false,
    attributes: new Map(),
    click() {
      multimodalModalListeners.get('click')?.({ target: this });
    },
    closest(selector) {
      return selector === '[data-action]' ? this : null;
    },
    setAttribute(name, value) {
      this.attributes.set(name, String(value));
    },
    getAttribute(name) {
      return this.attributes.get(name) ?? null;
    },
    removeAttribute(name) {
      this.attributes.delete(name);
    }
  };
  const multimodalAddButton = {
    dataset: { action: 'add-multimodal-fallback-row' },
    closest(selector) {
      return selector === '[data-action]' ? this : null;
    }
  };
  const multimodalFallbackButton = {
    dataset: {},
    addEventListener(type, listener) {
      if (type === 'click') this.clickListener = listener;
    },
    click() {
      this.clickListener?.({ currentTarget: this });
    }
  };
  const multimodalModal = {
    dataset: {},
    attributes: new Map(),
    classList: {
      add(name) {
        multimodalModalClasses.add(name);
      },
      remove(name) {
        multimodalModalClasses.delete(name);
      },
      contains(name) {
        return multimodalModalClasses.has(name);
      }
    },
    addEventListener(type, listener) {
      multimodalModalListeners.set(type, listener);
    },
    querySelector(selector) {
      if (selector === '[data-action="apply-multimodal-fallback"]') return multimodalApplyButton;
      return null;
    },
    setAttribute(name, value) {
      this.attributes.set(name, String(value));
    }
  };
  const customPricingCloseButton = {
    focus() {
      global.document.activeElement = this;
    }
  };
  const customPricingButton = {
    dataset: {},
    disabled: false,
    addEventListener(type, listener) {
      if (type === 'click') this.clickListener = listener;
    },
    click() {
      this.clickListener?.({ currentTarget: this });
    }
  };
  const customPricingModal = {
    dataset: {},
    attributes: new Map(),
    classList: {
      add(name) {
        customPricingModalClasses.add(name);
      },
      remove(name) {
        customPricingModalClasses.delete(name);
      },
      contains(name) {
        return customPricingModalClasses.has(name);
      }
    },
    addEventListener(type, listener) {
      customPricingModalListeners.set(type, listener);
    },
    querySelector(selector) {
      if (selector === '.close-btn') return customPricingCloseButton;
      return null;
    },
    setAttribute(name, value) {
      this.attributes.set(name, String(value));
    }
  };
  const customPricingRowsContainer = {
    get innerHTML() {
      return renderedCustomPricingHTML;
    },
    set innerHTML(value) {
      renderedCustomPricingHTML = String(value);
    }
  };
  const customPricingSearch = { value: '' };
  const customPricingEmpty = { hidden: true };
  const customPricingError = { textContent: '', hidden: true };
  const customPricingSummary = { textContent: '' };
  const appContainer = {
    attributes: new Map(),
    setAttribute(name, value) {
      this.attributes.set(name, String(value));
    },
    removeAttribute(name) {
      this.attributes.delete(name);
    }
  };
  const multimodalRowsContainer = {
    get innerHTML() {
      return renderedMultimodalHTML;
    },
    set innerHTML(value) {
      renderedMultimodalHTML = String(value);
    },
    querySelectorAll(selector) {
      if (selector !== '.multimodal-fallback-row') return [];
      return multimodalRows.map(({ from, to }) => ({
        querySelector(fieldSelector) {
          if (fieldSelector === 'select[data-field="from"]') return { value: from };
          if (fieldSelector === 'select[data-field="to"]') return { value: to };
          return null;
        }
      }));
    }
  };
  const multimodalError = { textContent: '', hidden: true };
  const inputs = {};
  const rows = {};
  const radioGroups = new Map();
  const elements = new Map([
    ['save-all-btn', saveButton],
    ['settings-tbody', settingsBody],
    ['model-multimodal-fallback-btn', multimodalFallbackButton],
    ['multimodalFallbackModal', multimodalModal],
    ['multimodalFallbackRows', multimodalRowsContainer],
    ['multimodalFallbackError', multimodalError],
    ['model-custom-pricing-btn', customPricingButton],
    ['model-custom-pricing-summary', customPricingSummary],
    ['customPricingModal', customPricingModal],
    ['customPricingRows', customPricingRowsContainer],
    ['customPricingSearch', customPricingSearch],
    ['customPricingEmpty', customPricingEmpty],
    ['customPricingError', customPricingError]
  ]);
  const definitions = new Map(settings.map((setting) => [setting.key, setting]));
  for (const [key, value] of Object.entries(inputValues)) {
    const row = {
      style: {},
      querySelector(selector) {
        if (!selector.endsWith(':checked')) return null;
        return (radioGroups.get(key) || []).find((radio) => radio.checked) || null;
      }
    };
    rows[key] = row;
    if (definitions.get(key)?.value_type === 'bool') {
      const radios = ['true', 'false'].map((radioValue) => ({
        type: 'radio',
        name: key,
        value: radioValue,
        checked: radioValue === value,
        closest() {
          return row;
        }
      }));
      radioGroups.set(key, radios);
      continue;
    }
    const input = {
      dataset: {},
      id: key,
      type: definitions.get(key)?.value_type === 'string' ? 'text' : 'number',
      value,
      attributes: new Map(),
      closest() {
        return row;
      },
      setAttribute(name, attributeValue) {
        this.attributes.set(name, String(attributeValue));
      },
      getAttribute(name) {
        return this.attributes.get(name) ?? null;
      },
      removeAttribute(name) {
        this.attributes.delete(name);
      },
      focus() {
        global.document.activeElement = this;
      }
    };
    inputs[key] = input;
    elements.set(key, input);
  }

  let bootstrap;
  let allowSave = false;
  const prompts = [];
  const notifications = [];
  const requests = [];
  const renderCalls = [];
  const errors = [];
  const successes = [];
  let nextSaveError = null;
  let nextUpdateError = null;
  let nextUpdateResult = { has_update: false, latest_version: 'v1.0.0' };
  let resolveModelPricingRequest = null;

  global.window = {
    t(key, params = {}) {
      if (key === 'settings.msg.confirmSave') return confirmMessage;
      if (key === 'settings.msg.invalidValue') return `请检查 ${params.key}：${params.reason}`;
      if (key === 'settings.validation.oauthURLDuplicatedScheme') return `协议头重复，请改为 ${params.url}。`;
      return key;
    },
    showNotification(message, type) {
      notifications.push({ message, type });
    },
    initPageBootstrap(config) {
      bootstrap = config;
    }
  };
  global.document = {
    activeElement: null,
    documentElement: { lang: 'zh-CN' },
    getElementById(id) {
      return elements.get(id) || null;
    },
    querySelectorAll(selector) {
      if (selector === '#customPricingRows .custom-pricing-model-row') return customPricingRows;
      const match = selector.match(/^input\[name="(.+)"\]$/);
      return match ? (radioGroups.get(match[1]) || []) : [];
    },
    querySelector(selector) {
      if (selector === '.app-container') return appContainer;
      const customRowMatch = selector.match(/^#customPricingRows \.custom-pricing-model-row\[data-model-index="(\d+)"\]$/);
      if (customRowMatch) return customPricingRows[Number(customRowMatch[1])] || null;
      const match = selector.match(/^input\[name="(.+)"\]:checked$/);
      return match ? (radioGroups.get(match[1]) || []).find((radio) => radio.checked) || null : null;
    }
  };
  global.TemplateEngine = {
    render(template, data) {
      renderCalls.push({ template, data });
      return null;
    }
  };
  global.escapeHtml = (value) => String(value);
  global.showError = (error) => { errors.push(error); };
  global.showSuccess = (message) => { successes.push(message); };
  global.confirm = (message) => {
    prompts.push(message);
    return allowSave;
  };
  global.fetchDataWithAuth = async (url, options) => {
    requests.push({ url, options });
    if (url.startsWith('/admin/model-pricing?')) {
      return new Promise((resolve) => {
        resolveModelPricingRequest = resolve;
      });
    }
    if (!options) return settings;
    if (url === '/admin/update/check') {
      if (nextUpdateError) {
        const error = nextUpdateError;
        nextUpdateError = null;
        throw error;
      }
      return nextUpdateResult;
    }
    if (url === '/admin/channels/filter-options?status=enabled') {
      return { models: filterModels };
    }
    if (nextSaveError) {
      const error = nextSaveError;
      nextSaveError = null;
      throw error;
    }
    if (url === '/admin/typesafe/test') return { valid: true };
    return { message: 'saved' };
  };

  const settingsModule = require.resolve('./settings.js');
  t.after(() => {
    delete require.cache[settingsModule];
    for (const name of [
      'window',
      'document',
      'TemplateEngine',
      'escapeHtml',
      'showError',
      'showSuccess',
      'confirm',
      'fetchDataWithAuth'
    ]) {
      delete global[name];
    }
  });

  require(settingsModule);
  bootstrap.run();
  await flushAsyncWork();

  return {
    inputs,
    radioGroups,
    errors,
    successes,
    notifications,
    prompts,
    renderCalls,
    requests,
    saveButton,
    updateButton,
    typeSafeButton,
    clickTypeSafe() { bodyListeners.get('click')?.({ target: typeSafeButton }); },
    clickUpdate() {
      bodyListeners.get('click')?.({ target: updateButton });
    },
    setUpdateResult(result) {
      nextUpdateResult = result;
    },
    failNextUpdate(message) {
      nextUpdateError = new Error(message);
    },
    multimodalApplyButton,
    multimodalRowsHTML: () => renderedMultimodalHTML,
    multimodalError,
    multimodalModal,
    customPricingRowsHTML: () => renderedCustomPricingHTML,
    setCustomPricingRows(entries) {
      customPricingRows = entries.map((entry, index) => {
        const pricing = entry.pricing || {};
        const status = { textContent: '', hidden: true, dataset: {} };
        const priceInputs = new Map([...customPricingFieldsForTest].map((field) => [field, {
          value: Object.prototype.hasOwnProperty.call(pricing, field) ? String(pricing[field]) : ''
        }]));
        let modelRow;
        const modelInput = {
          value: entry.model,
          closest(selector) {
            return selector === '.custom-pricing-model-row' ? modelRow : null;
          },
          matches(selector) {
            return selector === '[data-cp-field="model_id"]';
          },
          focus() {
            global.document.activeElement = this;
          }
        };
        const highRow = {
          dataset: { modelIndex: String(index) },
          classList: { contains: (name) => name === 'custom-pricing-high-row' },
          nextElementSibling: null,
          querySelector(selector) {
            const match = selector.match(/^\[data-cp-field="(.+)"\]$/);
            return match ? priceInputs.get(match[1]) || null : null;
          }
        };
        modelRow = {
          dataset: { modelIndex: String(index) },
          isConnected: true,
          classList: { contains: (name) => name === 'custom-pricing-model-row' },
          nextElementSibling: highRow,
          querySelector(selector) {
            if (selector === '[data-cp-field="model_id"]') return modelInput;
            if (selector === '[data-cp-status]') return status;
            const match = selector.match(/^\[data-cp-field="(.+)"\]$/);
            return match ? priceInputs.get(match[1]) || null : null;
          }
        };
        return modelRow;
      });
    },
    editCustomPricingField(index, field, value) {
      const selector = `[data-cp-field="${field}"]`;
      const row = customPricingRows[index];
      const target = row?.querySelector(selector) || row?.nextElementSibling?.querySelector(selector);
      if (target) target.value = String(value);
    },
    requestCustomPricingDefaults(index) {
      const input = customPricingRows[index]?.querySelector('[data-cp-field="model_id"]');
      customPricingModalListeners.get('focusout')?.({ target: input });
    },
    resolveCustomPricingDefaults(result) {
      resolveModelPricingRequest?.(result);
      resolveModelPricingRequest = null;
    },
    async openCustomPricing() {
      customPricingButton.click();
      await flushAsyncWork();
    },
    async openMultimodal() {
      multimodalFallbackButton.click();
      await flushAsyncWork();
    },
    clickAddMultimodal() {
      multimodalModalListeners.get('click')?.({ target: multimodalAddButton });
    },
    setMultimodalRows(rows) {
      multimodalRows = rows;
    },
    applyMultimodal(rows) {
      multimodalRows = rows;
      multimodalModal.classList.add('show');
      multimodalApplyButton.click();
    },
    failNextSave(message) {
      nextSaveError = new Error(message);
    },
    clickReset(key) {
      const listener = bodyListeners.get('click');
      listener?.({
        target: {
          closest(selector) {
            if (selector === '.setting-reset-btn') return { dataset: { key } };
            return null;
          }
        }
      });
    },
    setAllowSave(value) {
      allowSave = value;
    }
  };
}

const customPricingFieldsForTest = [
  'input_price', 'output_price', 'cache_read_price', 'cache_write_price',
  'input_price_high', 'output_price_high', 'cache_read_price_high', 'cache_write_price_high'
];

function saveRequests(page) {
  return page.requests.filter(({ options }) => options?.method === 'POST');
}

test('保存设置须经用户确认', async (t) => {
  const page = await loadSettingsPage(t, [{
    key: 'sample_setting',
    value: 'old-value',
    value_type: 'string',
    description: ''
  }], {
    sample_setting: 'new-value'
  });

  page.saveButton.click();
  await flushAsyncWork();

  assert.deepEqual(page.prompts, [confirmMessage]);
  assert.equal(saveRequests(page).length, 0);
  assert.equal(page.notifications.length, 0);

  page.setAllowSave(true);
  page.saveButton.click();
  await flushAsyncWork();

  assert.deepEqual(page.prompts, [confirmMessage, confirmMessage]);
  assert.equal(saveRequests(page).length, 1);
});

test('OAuth 地址协议头重复时提示正确值且不提交', async (t) => {
  const key = 'ANTIGRAVITY_URL';
  const page = await loadSettingsPage(t, [{
    key,
    value: '',
    value_type: 'string',
    description: ''
  }], {
    [key]: 'https://https://antigravity.hz-dao.deno.net'
  });
  page.setAllowSave(true);

  page.saveButton.click();
  await flushAsyncWork();

  assert.equal(saveRequests(page).length, 0);
  assert.equal(page.prompts.length, 0);
  assert.deepEqual(page.errors, [
    '请检查 ANTIGRAVITY_URL：协议头重复，请改为 https://antigravity.hz-dao.deno.net。'
  ]);
  assert.equal(page.inputs[key].getAttribute('aria-invalid'), 'true');
  assert.equal(global.document.activeElement, page.inputs[key]);
});

test('字节型设置以 MiB 数值编辑并以字节保存', async (t) => {
  const transcriptKey = 'responses_ws_max_transcript_bytes';
  const bodyKey = 'max_body_bytes';
  const imageBodyKey = 'max_image_body_bytes';
  const page = await loadSettingsPage(t, [
    { key: transcriptKey, value: '134217728', value_type: 'int', description: '' },
    { key: bodyKey, value: '10485760', value_type: 'int', description: '' },
    { key: imageBodyKey, value: '20971520', value_type: 'int', description: '' }
  ], {
    [transcriptKey]: '128',
    [bodyKey]: '10',
    [imageBodyKey]: '20'
  });
  page.setAllowSave(true);

  page.saveButton.click();
  await flushAsyncWork();

  assert.deepEqual(page.prompts, []);
  assert.deepEqual(page.notifications, [{ message: 'settings.msg.noChanges', type: 'info' }]);
  assert.equal(saveRequests(page).length, 0);

  page.inputs[transcriptKey].value = '256';
  page.inputs[bodyKey].value = '12';
  page.inputs[imageBodyKey].value = '24';
  page.saveButton.click();
  await flushAsyncWork();

  const requests = saveRequests(page);
  assert.equal(requests.length, 1);
  assert.deepEqual(JSON.parse(requests[0].options.body), {
    [transcriptKey]: '268435456',
    [bodyKey]: '12582912',
    [imageBodyKey]: '25165824'
  });
  assert.equal(page.inputs[transcriptKey].value, '256');
  assert.equal(page.inputs[bodyKey].value, '12');
  assert.equal(page.inputs[imageBodyKey].value, '24');
});

test('五个 WebSocket 设置允许显式保存 0', async (t) => {
  const settings = [
    { key: 'responses_ws_max_sessions', value: '256', value_type: 'int', description: '' },
    { key: 'responses_ws_session_ttl_minutes', value: '15', value_type: 'int', description: '' },
    { key: 'responses_ws_max_transcript_bytes', value: '268435456', value_type: 'int', description: '' },
    { key: 'responses_ws_max_connections', value: '128', value_type: 'int', description: '' },
    { key: 'responses_ws_max_connections_per_token', value: '64', value_type: 'int', description: '' }
  ];
  const page = await loadSettingsPage(t, settings, {
    responses_ws_max_sessions: '0',
    responses_ws_session_ttl_minutes: '0',
    responses_ws_max_transcript_bytes: '0',
    responses_ws_max_connections: '0',
    responses_ws_max_connections_per_token: '0'
  });
  page.setAllowSave(true);

  page.saveButton.click();
  await flushAsyncWork();

  assert.equal(page.errors.length, 0);
  const requests = saveRequests(page);
  assert.equal(requests.length, 1);
  assert.deepEqual(JSON.parse(requests[0].options.body), {
    responses_ws_max_sessions: '0',
    responses_ws_session_ttl_minutes: '0',
    responses_ws_max_transcript_bytes: '0',
    responses_ws_max_connections: '0',
    responses_ws_max_connections_per_token: '0'
  });
});

test('空字节输入和舍入为零的正数不会提交', async (t) => {
  const key = 'max_body_bytes';
  const page = await loadSettingsPage(t, [{
    key,
    value: '10485760',
    default_value: '10485760',
    value_type: 'int',
    description: ''
  }], { [key]: '10' });
  page.setAllowSave(true);

  page.inputs[key].value = '';
  page.saveButton.click();
  await flushAsyncWork();
  assert.equal(saveRequests(page).length, 0);
  assert.equal(page.prompts.length, 0);
  assert.equal(page.errors.length, 1);

  page.inputs[key].value = '0.0000001';
  page.saveButton.click();
  await flushAsyncWork();
  assert.equal(saveRequests(page).length, 0);
  assert.equal(page.prompts.length, 0);
  assert.equal(page.errors.length, 2);
});

test('WebSocket 恢复默认写入 0，且只在保存所有更改后提交', async (t) => {
  const bytesKey = 'responses_ws_max_transcript_bytes';
  const boolKey = 'debug_log_enabled';
  const websocketSettings = [
    { key: 'responses_ws_max_sessions', value: '32', default_value: '0', value_type: 'int', description: '' },
    { key: 'responses_ws_session_ttl_minutes', value: '30', default_value: '0', value_type: 'int', description: '' },
    { key: bytesKey, value: '134217728', default_value: '0', value_type: 'int', description: '' },
    { key: 'responses_ws_max_connections', value: '64', default_value: '0', value_type: 'int', description: '' },
    { key: 'responses_ws_max_connections_per_token', value: '16', default_value: '0', value_type: 'int', description: '' }
  ];
  const page = await loadSettingsPage(t, [
    ...websocketSettings,
    {
      key: boolKey,
      value: 'true',
      default_value: 'false',
      value_type: 'bool',
      description: ''
    }
  ], {
    responses_ws_max_sessions: '32',
    responses_ws_session_ttl_minutes: '30',
    [bytesKey]: '128',
    responses_ws_max_connections: '64',
    responses_ws_max_connections_per_token: '16',
    [boolKey]: 'true'
  });

  for (const setting of websocketSettings) page.clickReset(setting.key);
  page.clickReset(boolKey);

  for (const setting of websocketSettings) {
    assert.equal(page.inputs[setting.key].value, '0');
  }
  assert.equal(page.radioGroups.get(boolKey).find((radio) => radio.value === 'false').checked, true);
  assert.equal(page.prompts.length, 0);
  assert.equal(saveRequests(page).length, 0);

  page.setAllowSave(true);
  page.saveButton.click();
  await flushAsyncWork();

  const requests = saveRequests(page);
  assert.equal(requests.length, 1);
  assert.deepEqual(JSON.parse(requests[0].options.body), {
    responses_ws_max_sessions: '0',
    responses_ws_session_ttl_minutes: '0',
    [bytesKey]: '0',
    responses_ws_max_connections: '0',
    responses_ws_max_connections_per_token: '0',
    [boolKey]: 'false'
  });
});

test('全局冷却规则通过设置批量保存接口持久化', async (t) => {
  const key = 'global_cooldown_detection_rules';
  const rules = '{"rules":[{"enabled":true,"name":"Maintenance","priority":0,"status_codes":[503],"scope":"channel","mode":"fixed","cooldown_seconds":60}]}';
  const page = await loadSettingsPage(t, [{
    key,
    value: '{}',
    value_type: 'json',
    description: ''
  }], {
    [key]: rules
  });
  page.setAllowSave(true);

  page.saveButton.click();
  await flushAsyncWork();

  const requests = saveRequests(page);
  assert.equal(requests.length, 1);
  assert.deepEqual(JSON.parse(requests[0].options.body), { [key]: rules });
});

test('自定义模型价格重新打开时保留显式零值', async (t) => {
  const value = JSON.stringify({
    'free-cache-model': {
      cache_read_price: 0,
      cache_read_price_high: 0,
      cache_write_price_high: 0
    }
  });
  const page = await loadSettingsPage(t, [{
    key: 'model_custom_pricing',
    value,
    value_type: 'json',
    description: ''
  }], {
    model_custom_pricing: value
  });

  await page.openCustomPricing();

  const html = page.customPricingRowsHTML();
  for (const field of ['cache_read_price', 'cache_read_price_high', 'cache_write_price_high']) {
    assert.match(html, new RegExp(`data-cp-field="${field}"[^>]*value="0"`));
  }
});

test('加载系统默认价格时保留其他行的并发编辑', async (t) => {
  const value = JSON.stringify({
    'target-model': {},
    'other-model': { input_price: 1 }
  });
  const page = await loadSettingsPage(t, [{
    key: 'model_custom_pricing',
    value,
    value_type: 'json',
    description: ''
  }], {
    model_custom_pricing: value
  });

  await page.openCustomPricing();
  page.setCustomPricingRows([
    { model: 'target-model', pricing: {} },
    { model: 'other-model', pricing: { input_price: 1 } }
  ]);
  page.requestCustomPricingDefaults(0);
  page.editCustomPricingField(1, 'input_price', 9);
  page.resolveCustomPricingDefaults({
    found: true,
    pricing: { input_price: 2, output_price: 3 }
  });
  await flushAsyncWork();

  assert.match(
    page.customPricingRowsHTML(),
    /value="other-model"[\s\S]*?data-cp-field="input_price"[^>]*value="9"/
  );
});

test('多模态回退映射在对话框内直接保存，无需再点保存所有更改', async (t) => {
  const key = 'model_multimodal_fallback';
  const mappings = '{"gpt-5.6-luna":"gemini-3-pro"}';
  const page = await loadSettingsPage(t, [{
    key,
    value: '{}',
    value_type: 'json',
    description: ''
  }], {
    [key]: '{}'
  });

  page.applyMultimodal([{ from: 'gpt-5.6-luna', to: 'gemini-3-pro' }]);
  await flushAsyncWork();

  const requests = saveRequests(page);
  assert.equal(requests.length, 1);
  assert.deepEqual(JSON.parse(requests[0].options.body), { [key]: mappings });
  assert.equal(page.prompts.length, 0);
  assert.equal(page.inputs[key].value, mappings);
  assert.equal(page.multimodalModal.classList.contains('show'), false);
  assert.equal(page.multimodalApplyButton.disabled, false);
  assert.equal(page.multimodalApplyButton.getAttribute('aria-busy'), null);

  page.saveButton.click();
  await flushAsyncWork();
  assert.equal(saveRequests(page).length, 1);
});

test('添加映射时保留当前行尚未保存的下拉选择', async (t) => {
  const page = await loadSettingsPage(t, [{
    key: 'model_multimodal_fallback',
    value: '{}',
    value_type: 'json',
    description: ''
  }], {
    model_multimodal_fallback: '{}'
  }, {
    filterModels: ['MiniMax-M2.1', 'glm-5.2-fast-preview', 'qwen3.8-max-0902']
  });

  await page.openMultimodal();
  page.clickAddMultimodal();
  page.setMultimodalRows([{
    from: 'glm-5.2-fast-preview',
    to: 'qwen3.8-max-0902'
  }]);
  page.clickAddMultimodal();

  const html = page.multimodalRowsHTML();
  assert.match(html, /value="glm-5\.2-fast-preview" selected/);
  assert.match(html, /value="qwen3\.8-max-0902" selected/);
});

test('多模态回退映射保存失败时保留对话框和原持久化值', async (t) => {
  const key = 'model_multimodal_fallback';
  const page = await loadSettingsPage(t, [{
    key,
    value: '{}',
    value_type: 'json',
    description: ''
  }], {
    [key]: '{}'
  });
  page.failNextSave('连接中断');

  page.applyMultimodal([{ from: 'gpt-5.6-luna', to: 'gemini-3-pro' }]);
  await flushAsyncWork();

  assert.equal(saveRequests(page).length, 1);
  assert.equal(page.inputs[key].value, '{}');
  assert.equal(page.multimodalModal.classList.contains('show'), true);
  assert.equal(page.multimodalError.hidden, false);
  assert.match(page.multimodalError.textContent, /连接中断/);
  assert.equal(page.multimodalApplyButton.disabled, false);
  assert.equal(page.multimodalApplyButton.getAttribute('aria-busy'), null);
});

test('非容器更新渠道显示手动检测按钮并触发完整更新流程', async (t) => {
  const page = await loadSettingsPage(t, [{
    key: 'auto_update_channel',
    value: 'stable',
    value_type: 'string',
    description: ''
  }], {
    auto_update_channel: 'stable'
  });

  const settingRow = page.renderCalls.find(({ template, data }) => (
    template === 'tpl-setting-row' && data.key === 'auto_update_channel'
  ));
  assert.ok(settingRow);
  assert.match(settingRow.data.inputHtml, /data-action="check-for-updates"/);

  page.setUpdateResult({
    has_update: true,
    latest_version: 'v2.0.0',
    pending_restart: true,
    pending_version: 'v2.0.0'
  });
  page.clickUpdate();
  assert.equal(page.updateButton.disabled, true);
  assert.equal(page.updateButton.getAttribute('aria-busy'), 'true');

  await flushAsyncWork();

  const requests = page.requests.filter(({ url }) => url === '/admin/update/check');
  assert.equal(requests.length, 1);
  assert.equal(requests[0].options.method, 'POST');
  assert.equal(page.successes.length, 1);
  assert.equal(page.successes[0], 'settings.updateCheck.pendingRestart');
  assert.equal(page.updateButton.disabled, false);
  assert.equal(page.updateButton.getAttribute('aria-busy'), null);
});

test('仅发现更新时不提示已开始下载', async (t) => {
  const page = await loadSettingsPage(t, [{
    key: 'auto_update_channel',
    value: 'stable',
    value_type: 'string',
    description: ''
  }], {
    auto_update_channel: 'stable'
  });

  page.setUpdateResult({
    has_update: true,
    latest_version: 'v2.0.0',
    pending_restart: false
  });
  page.clickUpdate();
  await flushAsyncWork();

  assert.equal(page.successes.length, 1);
  assert.equal(page.successes[0], 'settings.updateCheck.found');

  page.setUpdateResult({
    has_update: false,
    latest_version: 'v2.0.0',
    pending_restart: false
  });
  page.clickUpdate();
  await flushAsyncWork();

  assert.equal(page.successes.length, 2);
  assert.equal(page.successes[1], 'settings.updateCheck.upToDate');
});

test('手动检测更新失败时恢复按钮并显示错误', async (t) => {
  const page = await loadSettingsPage(t, [{
    key: 'auto_update_channel',
    value: 'stable',
    value_type: 'string',
    description: ''
  }], {
    auto_update_channel: 'stable'
  });
  page.failNextUpdate('连接中断');

  page.clickUpdate();
  await flushAsyncWork();

  assert.equal(page.requests.filter(({ url }) => url === '/admin/update/check').length, 1);
  assert.equal(page.errors.length, 1);
  assert.match(page.errors[0], /连接中断/);
  assert.equal(page.updateButton.disabled, false);
  assert.equal(page.updateButton.getAttribute('aria-busy'), null);
});

test('容器内禁用更新设置并显示镜像切换说明', async (t) => {
  const page = await loadSettingsPage(t, [
    {
      key: 'auto_update_channel',
      value: 'stable',
      value_type: 'string',
      description: '',
      editable: false,
      disabled_reason: 'container_image_managed'
    },
    {
      key: 'auto_update_interval_hours',
      value: '12',
      value_type: 'int',
      description: '',
      editable: false,
      disabled_reason: 'container_image_managed'
    }
  ], {});

  const settingRows = page.renderCalls.filter(({ template }) => template === 'tpl-setting-row');
  assert.equal(settingRows.length, 2);
  for (const { data } of settingRows) {
    assert.match(data.inputHtml, /\bdisabled\b/);
    assert.equal(data.resetDisabledAttributes, 'disabled');
  }
  const channelRow = settingRows.find(({ data }) => data.key === 'auto_update_channel');
  assert.ok(channelRow);
  assert.doesNotMatch(channelRow.data.inputHtml, /data-action="check-for-updates"/);
});


test('TypeSafe secret is omitted unless changed and explicit reset disables the service', async (t) => {
  const page = await loadSettingsPage(t, [
    { key: 'TypeSafe_api_key', value: '', default_value: '', value_type: 'string', configured: true },
    { key: 'TypeSafe_enabled', value: 'true', default_value: 'false', value_type: 'bool' },
    { key: 'auto_refresh_interval_seconds', value: '10', value_type: 'int' }
  ], { TypeSafe_api_key: '', TypeSafe_enabled: 'true', auto_refresh_interval_seconds: '20' });
  page.setAllowSave(true);
  page.saveButton.click();
  await flushAsyncWork();
  assert.deepEqual(JSON.parse(saveRequests(page)[0].options.body), { auto_refresh_interval_seconds: '20' });
  page.clickReset('TypeSafe_api_key');
  page.saveButton.click();
  await flushAsyncWork();
  assert.deepEqual(JSON.parse(saveRequests(page)[1].options.body), { TypeSafe_api_key: '', TypeSafe_enabled: 'false' });
  page.inputs.TypeSafe_api_key.value = 'replacement-key';
  page.saveButton.click();
  await flushAsyncWork();
  assert.deepEqual(JSON.parse(saveRequests(page)[2].options.body), { TypeSafe_api_key: 'replacement-key' });
  assert.equal(page.inputs.TypeSafe_api_key.value, '');
});

test('TypeSafe test sends entered or saved key without saving and restores button after errors', async (t) => {
  const page = await loadSettingsPage(t, [
    { key: 'TypeSafe_api_key', value: '', default_value: '', value_type: 'string', configured: true }
  ], { TypeSafe_api_key: ' entered-key ' });
  page.clickTypeSafe();
  page.clickTypeSafe();
  assert.equal(page.typeSafeButton.disabled, true);
  await flushAsyncWork();
  const calls = () => page.requests.filter(r => r.url === '/admin/typesafe/test');
  assert.equal(calls().length, 1);
  assert.deepEqual(JSON.parse(calls()[0].options.body), { api_key: 'entered-key' });
  assert.equal(page.inputs.TypeSafe_api_key.value, ' entered-key ');
  page.inputs.TypeSafe_api_key.value = '';
  page.clickTypeSafe();
  await flushAsyncWork();
  assert.deepEqual(JSON.parse(calls()[1].options.body), {});
  page.clickReset('TypeSafe_api_key');
  page.clickTypeSafe();
  await flushAsyncWork();
  assert.deepEqual(JSON.parse(calls()[2].options.body), { api_key: '' });
  assert.equal(page.typeSafeButton.disabled, false);
  assert.equal(page.requests.filter(r => r.url === '/admin/settings/batch').length, 0);
  assert.equal(page.prompts.length, 0);
  assert.equal(page.successes.length, 3);
  page.failNextSave('connection failed');
  page.clickTypeSafe();
  await flushAsyncWork();
  assert.equal(page.typeSafeButton.disabled, false);
  assert.match(page.errors.at(-1), /connection failed/);
});
