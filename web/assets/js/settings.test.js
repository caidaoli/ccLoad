const assert = require('node:assert/strict');
const test = require('node:test');

const confirmMessage = '保存任意设置后,服务会在约 2 秒后自动重启以生效，是否继续';
const confirmHotMessage = '这些设置保存后立即生效，无需重启服务，是否继续？';

function flushAsyncWork() {
  return new Promise((resolve) => setImmediate(resolve));
}

async function loadSettingsPage(t, settings, inputValues, { filterModels = [], channels = [], reasoningLoadError = null, initialLoadError = null, extraElements = {} } = {}) {
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
    ['customPricingError', customPricingError],
    ...Object.entries(extraElements)
  ]);
  const reasoningListeners = new Map();
  const reasoningClasses = new Set();
  let reasoningFields = [];
  let reasoningModels = [];
  // Parse rendered form controls so tests interact with the same values and disabled states as the browser.
  function reasoningControl(tag) {
    const attribute = name => tag.match(new RegExp(`(?:^|\\s)${name}="([^"]*)"`))?.[1] || '';
    const attributes = new Map();
    return {
      id: attribute('id'), type: attribute('type'), value: attribute('value'),
      checked: /\bchecked\b/.test(tag), disabled: /\bdisabled\b/.test(tag),
      dataset: { reasoningField: attribute('data-reasoning-field') },
      hasAttribute: name => new RegExp(`(?:^|\\s)${name}(?:=|\\s|$)`).test(tag),
      setAttribute: (name, value) => attributes.set(name, String(value)),
      removeAttribute: name => attributes.delete(name),
      getAttribute: name => attributes.get(name) ?? null,
      innerHTML: '',
      get options() {
        return [...this.innerHTML.matchAll(/<option value="([^"]*)"/g)].map(match => ({ value: match[1] }));
      },
      insertAdjacentHTML(_position, html) { this.innerHTML += html; },
      focus() { global.document.activeElement = this; }
    };
  }
  const reasoningButton = {
    dataset: {}, isConnected: true,
    addEventListener(type, listener) { if (type === 'click') this.listener = listener; },
    click() { this.listener?.({ currentTarget: this }); },
    focus() { global.document.activeElement = this; }
  };
  const reasoningModal = {
    dataset: {}, attributes: new Map(),
    classList: {
      add: name => reasoningClasses.add(name), remove: name => reasoningClasses.delete(name),
      contains: name => reasoningClasses.has(name)
    },
    addEventListener: (type, listener) => reasoningListeners.set(type, listener),
    setAttribute(name, value) { this.attributes.set(name, String(value)); },
    querySelector: selector => selector === '.close-btn' ? { focus() {} } : null
  };
  const reasoningContent = {
    set innerHTML(html) {
      reasoningFields = [...html.matchAll(/<(?:input|select)\b([^>]*)>/g)].map(match => reasoningControl(match[1]));
      for (const field of reasoningFields) if (field.id) elements.set(field.id, field);
      elements.set('reasoningAutoEditor', { querySelectorAll: () => [...reasoningFields, ...reasoningModels] });
      elements.set('reasoningBusinessModels', {
        querySelectorAll: () => reasoningModels,
        set innerHTML(rows) {
          reasoningModels = [...rows.matchAll(/<input\b([^>]*)>/g)].map(match => reasoningControl(match[1]));
        }
      });
      elements.set('reasoningModelsEmpty', { hidden: true });
    }
  };
  const reasoningError = { textContent: '', hidden: true };
  elements.set('reasoning-auto-btn', reasoningButton);
  elements.set('reasoningAutoModal', reasoningModal);
  elements.set('reasoningAutoContent', reasoningContent);
  elements.set('reasoningAutoError', reasoningError);
  const privacyListeners = new Map();
  const privacyClasses = new Set();
  let privacyFields = [];
  let privacyRuleFields = [];
  const privacyButton = {
    dataset: {}, isConnected: true,
    addEventListener(type, listener) { if (type === 'click') this.listener = listener; },
    click() { this.listener?.({ currentTarget: this }); },
    focus() { global.document.activeElement = this; }
  };
  const privacyCloseButton = { focus() { global.document.activeElement = this; } };
  const privacyApplyButton = { disabled: false, setAttribute() {}, removeAttribute() {} };
  const privacyModal = {
    dataset: {}, attributes: new Map(),
    classList: {
      add: name => privacyClasses.add(name), remove: name => privacyClasses.delete(name),
      contains: name => privacyClasses.has(name)
    },
    addEventListener: (type, listener) => privacyListeners.set(type, listener),
    setAttribute(name, value) { this.attributes.set(name, String(value)); },
    removeAttribute(name) { this.attributes.delete(name); },
    querySelector: selector => ({ '.close-btn': privacyCloseButton, '[data-action="apply-privacy-redaction"]': privacyApplyButton })[selector] || null
  };
  const classSet = () => {
    const classes = new Set();
    return {
      add: name => classes.add(name), remove: name => classes.delete(name), contains: name => classes.has(name),
      toggle: (name, on) => on ? classes.add(name) : classes.delete(name)
    };
  };
  const privacyTestListeners = new Map();
  const privacyTestModal = {
    attributes: new Map(), classList: classSet(),
    addEventListener: (type, listener) => privacyTestListeners.set(type, listener),
    setAttribute(name, value) { this.attributes.set(name, String(value)); }
  };
  const privacyTestStub = extra => ({
    textContent: '', innerHTML: '', hidden: false, disabled: false, value: '', isConnected: true, attributes: new Map(),
    classList: classSet(),
    setAttribute(name, value) { this.attributes.set(name, String(value)); },
    removeAttribute(name) { this.attributes.delete(name); },
    focus() { global.document.activeElement = this; },
    ...extra
  });
  elements.set('privacyTestModal', privacyTestModal);
  elements.set('privacyTestOpen', privacyTestStub());
  elements.set('privacyTestInput', privacyTestStub({ id: 'privacyTestInput' }));
  elements.set('privacyTestRun', privacyTestStub({ disabled: true }));
  elements.set('privacyTestStatus', privacyTestStub());
  elements.set('privacyTestWarnings', privacyTestStub({ hidden: true }));
  elements.set('privacyRedactionWarnings', privacyTestStub({ hidden: true }));
  // 测试结果区按 DOM 构建：最小元素桩只保留文本、子节点与按标签查询，断言读取用户可见的文本。
  const domElement = tag => ({
    tagName: tag.toUpperCase(), children: [], ownText: '',
    get textContent() { return this.children.length ? this.children.map(child => child.textContent).join('') : this.ownText; },
    set textContent(value) { this.children = []; this.ownText = String(value); },
    setAttribute() {},
    append(...nodes) { this.children.push(...nodes); },
    replaceChildren(...nodes) { this.children = nodes; this.ownText = ''; },
    querySelectorAll(selector) {
      return this.children.flatMap(child => [...(child.tagName === selector.toUpperCase() ? [child] : []), ...child.querySelectorAll(selector)]);
    }
  });
  elements.set('privacyTestResult', Object.assign(domElement('div'), { hidden: true }));
  function privacyControls(html) {
    return [...html.matchAll(/<(input|button)\b([^>]*)>|<textarea\b([^>]*)>([\s\S]*?)<\/textarea>/g)].map(match => {
      const tag = match[2] || match[3];
      const field = reasoningControl(tag);
      for (const name of ['privacy-field', 'privacy-rule', 'privacy-index']) {
        const key = name.replace(/-([a-z])/g, (_match, char) => char.toUpperCase());
        field.dataset[key] = tag.match(new RegExp(`data-${name}="([^"]*)"`))?.[1];
      }
      if (match[3] !== undefined) field.value = match[4] || '';
      return field;
    });
  }
  elements.set('privacy-redaction-btn', privacyButton);
  elements.set('privacyRedactionModal', privacyModal);
  elements.set('privacyRedactionContent', {
    set innerHTML(html) {
      privacyFields = privacyControls(html);
      for (const field of privacyFields) if (field.id) elements.set(field.id, field);
      elements.set('privacyRedactionEditor', { querySelectorAll: () => [...privacyFields, ...privacyRuleFields] });
      elements.set('privacyRuleRows', {
        set innerHTML(rows) { privacyRuleFields = privacyControls(rows); },
        querySelector: () => privacyRuleFields.find(field => field.dataset.privacyRule)
      });
    }
  });
  const privacyError = { textContent: '', hidden: true };
  elements.set('privacyRedactionError', privacyError);
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
  let unsavedGuard = null;
  let allowSave = false;
  let failNextLoad = null;
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
  const privacyPreviews = [];

  global.window = {
    t(key, params = {}) {
      if (key === 'settings.msg.confirmSave') return confirmMessage;
      if (key === 'settings.msg.confirmSaveHot') return confirmHotMessage;
      if (key === 'settings.msg.invalidValue') return `请检查 ${params.name}：${params.reason}`;
      if (key === 'settings.msg.invalidValues') return `${params.count} 项无效：${params.details}`;
      if (key === 'settings.msg.savedRestart') return `需重启：${params.names}`;
      if (key === 'settings.msg.listSeparator') return '、';
      if (key === 'settings.msg.detailSeparator') return '；';
      if (key === 'settings.validation.oauthURLDuplicatedScheme') return `协议头重复，请改为 ${params.url}。`;
      if (key === 'settings.privacyRedaction.hitBuiltin') return `内置规则 · ${params.kind}`;
      if (key === 'settings.privacyRedaction.hitWord') return `词表第 ${params.index} 项`;
      if (key === 'settings.privacyRedaction.hitCustom') return `自定义规则第 ${params.index} 条 · ${params.kind}`;
      if (key === 'settings.privacyRedaction.warningTitle') return '隐私脱敏规则提示（仍可应用和保存）：';
      if (key === 'settings.privacyRedaction.broadPattern') return `自定义规则第 ${params.index} 条匹配范围过宽，可能遮蔽普通文本，请检查。`;
      return key;
    },
    showNotification(message, type) {
      notifications.push({ message, type });
    },
    async showConfirm(options) {
      prompts.push(options.message);
      return allowSave;
    },
    guardUnsavedChanges(isDirty) {
      unsavedGuard = isDirty;
      return () => {};
    },
    initPageBootstrap(config) {
      bootstrap = config;
    },
    highlightPrivacyPlaceholders() {}
  };
  const documentListeners = new Map();
  global.document = {
    activeElement: null,
    addEventListener: (type, listener) => documentListeners.set(type, listener),
    documentElement: { lang: 'zh-CN' },
    createElement: domElement,
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
  global.fetchDataWithAuth = async (url, options) => {
    requests.push({ url, options });
    if (url.startsWith('/admin/model-pricing?')) {
      return new Promise((resolve) => {
        resolveModelPricingRequest = resolve;
      });
    }
    if (url === '/admin/channels' || url === '/admin/channels/filter-options?status=enabled') {
      if (reasoningLoadError) throw new Error(reasoningLoadError);
      return url === '/admin/channels' ? channels : { models: filterModels };
    }
    if (!options) {
      if (failNextLoad) {
        const error = failNextLoad;
        failNextLoad = null;
        throw error;
      }
      return settings;
    }
    if (url === '/admin/privacy-redaction/preview') {
      return new Promise((resolve, reject) => privacyPreviews.push({ body: JSON.parse(options.body), resolve, reject }));
    }
    if (url === '/admin/update/check') {
      if (nextUpdateError) {
        const error = nextUpdateError;
        nextUpdateError = null;
        throw error;
      }
      return nextUpdateResult;
    }
    if (nextSaveError) {
      const error = nextSaveError;
      nextSaveError = null;
      throw error;
    }
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
      'fetchDataWithAuth'
    ]) {
      delete global[name];
    }
  });

  require(settingsModule);
  if (initialLoadError) failNextLoad = new Error(initialLoadError);
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
    settingsBody,
    hasUnsavedChanges: () => unsavedGuard?.(),
    editInput(key, value) {
      const input = inputs[key];
      input.value = value;
      bodyListeners.get('input')?.({ target: { closest: () => input } });
    },
    reasoningModal,
    reasoningError,
    reasoningModels: () => reasoningModels,
    reasoningField: id => elements.get(id),
    privacyModal,
    privacyError,
    privacyButton,
    privacyField: field => privacyFields.find(input => input.dataset.privacyField === field),
    openPrivacy() { privacyButton.click(); },
    privacyApplyButton,
    // 应用需服务端校验：点击后若发出了校验请求，按服务端接受处理。
    async applyPrivacy() {
      const pending = privacyPreviews.length;
      this.privacyAction('apply-privacy-redaction');
      privacyPreviews[pending]?.resolve({ masked: '', hits: [] });
      await flushAsyncWork();
    },
    privacyAction(action, index) {
      const button = { dataset: { action, privacyIndex: String(index) }, closest: () => button };
      privacyListeners.get('click')?.({ target: button });
    },
    editPrivacyField(field, value) {
      const target = privacyFields.find(input => input.dataset.privacyField === field);
      assert.ok(target, field);
      if (target.disabled) return;
      if (target.type === 'checkbox') target.checked = value;
      else target.value = value;
      privacyListeners.get(target.type === 'checkbox' ? 'change' : 'input')?.({ target });
    },
    privacyPreviews,
    privacyTestModal,
    privacyTest: id => elements.get(id),
    privacyTestAction(action) {
      const button = { dataset: { action }, closest: () => button };
      privacyTestListeners.get('click')?.({ target: button });
    },
    editPrivacyTest(value) {
      const target = elements.get('privacyTestInput');
      target.value = value;
      privacyTestListeners.get('input')?.({ target });
    },
    editPrivacyRule(index, field, value) {
      const target = privacyRuleFields.find(input => input.dataset.privacyIndex === String(index) && input.dataset.privacyRule === field);
      assert.ok(target, `${index}:${field}`);
      if (target.disabled) return;
      target.value = value;
      privacyListeners.get('input')?.({ target });
    },
    async openReasoning() {
      reasoningButton.click();
      await flushAsyncWork();
    },
    pressDocumentKey(key, { defaultPrevented = false } = {}) {
      const event = { key, defaultPrevented, preventDefault() { this.defaultPrevented = true; } };
      documentListeners.get('keydown')?.(event);
      return event;
    },
    reasoningAction(action) {
      const button = { dataset: { action }, closest: () => button };
      reasoningListeners.get('click')?.({ target: button });
    },
    editReasoningField(field, value) {
      const target = reasoningFields.find(input => input.dataset.reasoningField === field);
      assert.ok(target, field);
      if (target.disabled) return;
      if (field === 'enabled') target.checked = value;
      else target.value = String(value);
      reasoningListeners.get(field === 'timeout_ms' ? 'input' : 'change')?.({ target });
    },
    selectReasoningModel(model, checked) {
      const target = reasoningModels.find(input => input.value === model);
      assert.ok(target, model);
      if (target.disabled) return;
      target.checked = checked;
      reasoningListeners.get('change')?.({ target });
    },
    toggleReasoningModels(checked) {
      const target = elements.get('reasoningModelsSelectAll');
      if (target.disabled) return;
      target.checked = checked;
      reasoningListeners.get('change')?.({ target });
    },
    filterReasoningModels(value) {
      const target = elements.get('reasoningModelSearch');
      target.value = value;
      reasoningListeners.get('input')?.({ target });
    },
    clickRetryLoad() {
      bodyListeners.get('click')?.({
        target: { closest: (selector) => (selector === '[data-action="retry-load-settings"]' ? { disabled: false } : null) }
      });
    },
    updateButton,
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
  return page.requests.filter(({ url }) => url === '/admin/settings/batch');
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
  const bodyBudgetKey = 'request_body_memory_budget_bytes';
  const page = await loadSettingsPage(t, [
    { key: transcriptKey, value: '134217728', value_type: 'int', description: '' },
    { key: bodyKey, value: '10485760', value_type: 'int', description: '' },
    { key: imageBodyKey, value: '20971520', value_type: 'int', description: '' },
    { key: bodyBudgetKey, value: '268435456', value_type: 'int', description: '' }
  ], {
    [transcriptKey]: '128',
    [bodyKey]: '10',
    [imageBodyKey]: '20',
    [bodyBudgetKey]: '256'
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
  page.inputs[bodyBudgetKey].value = '128.5';
  page.saveButton.click();
  await flushAsyncWork();

  const requests = saveRequests(page);
  assert.equal(requests.length, 1);
  assert.deepEqual(JSON.parse(requests[0].options.body), {
    [transcriptKey]: '268435456',
    [bodyKey]: '12582912',
    [imageBodyKey]: '25165824',
    [bodyBudgetKey]: '134742016'
  });
  assert.equal(page.inputs[transcriptKey].value, '256');
  assert.equal(page.inputs[bodyKey].value, '12');
  assert.equal(page.inputs[imageBodyKey].value, '24');
  assert.equal(page.inputs[bodyBudgetKey].value, '128.5');
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

test('正字节设置拒绝空输入、零、负数及不足一字节的大小', async (t) => {
  for (const key of ['max_body_bytes', 'request_body_memory_budget_bytes']) {
    await t.test(key, async (t) => {
      const page = await loadSettingsPage(t, [{
        key,
        value: '10485760',
        default_value: '10485760',
        value_type: 'int',
        description: ''
      }], { [key]: '10' });
      page.setAllowSave(true);

      for (const [index, value] of ['', '0', '-1', '0.0000001'].entries()) {
        page.inputs[key].value = value;
        page.saveButton.click();
        await flushAsyncWork();
        assert.equal(saveRequests(page).length, 0);
        assert.equal(page.prompts.length, 0);
        assert.equal(page.errors.length, index + 1);
      }
    });
  }
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

test('一次保存报告全部无效设置的可读名称并标记每个输入', async (t) => {
  const page = await loadSettingsPage(t, [
    { key: 'max_key_retries', value: '3', value_type: 'int', description: '单渠道最大Key重试次数' },
    { key: 'max_concurrency', value: '10', value_type: 'int', description: '最大并发请求数' },
    { key: 'log_retention_days', value: '7', value_type: 'int', description: '日志保留天数' }
  ], { max_key_retries: '0', max_concurrency: '0', log_retention_days: '30' });
  page.setAllowSave(true);

  page.saveButton.click();
  await flushAsyncWork();

  assert.equal(saveRequests(page).length, 0);
  assert.equal(page.prompts.length, 0);
  assert.equal(page.errors.length, 1);
  assert.match(page.errors[0], /^2 项无效：/);
  assert.match(page.errors[0], /请检查 单渠道最大Key重试次数：/);
  assert.match(page.errors[0], /请检查 最大并发请求数：/);
  assert.doesNotMatch(page.errors[0], /max_key_retries|max_concurrency/);
  assert.equal(page.inputs.max_key_retries.getAttribute('aria-invalid'), 'true');
  assert.equal(page.inputs.max_concurrency.getAttribute('aria-invalid'), 'true');
  assert.equal(page.inputs.log_retention_days.getAttribute('aria-invalid'), null);
  assert.equal(global.document.activeElement, page.inputs.max_key_retries);

  page.editInput('max_key_retries', '5');
  assert.equal(page.inputs.max_key_retries.getAttribute('aria-invalid'), null);
  assert.equal(page.inputs.max_concurrency.getAttribute('aria-invalid'), 'true');
});

test('混合保存时确认并汇总需重启生效的设置', async (t) => {
  const page = await loadSettingsPage(t, [
    { key: 'max_concurrency', value: '10', value_type: 'int', description: '最大并发请求数' },
    { key: 'privacy_redaction', value: '{}', value_type: 'json' }
  ], { max_concurrency: '20', privacy_redaction: '{}' });
  page.editInput('privacy_redaction', JSON.stringify({ ...privacyDefaults, enabled: true }));
  page.setAllowSave(true);

  page.saveButton.click();
  await flushAsyncWork();

  assert.equal(saveRequests(page).length, 1);
  assert.deepEqual(page.prompts, [confirmMessage]);
  assert.deepEqual(page.successes, ['需重启：最大并发请求数']);
});

test('存在未保存修改时离开页面受保护，保存后解除', async (t) => {
  const page = await loadSettingsPage(t, [
    { key: 'max_concurrency', value: '10', value_type: 'int', description: '' }
  ], { max_concurrency: '10' });
  page.setAllowSave(true);

  assert.equal(page.hasUnsavedChanges(), false);
  page.editInput('max_concurrency', '20');
  assert.equal(page.hasUnsavedChanges(), true);

  page.saveButton.click();
  await flushAsyncWork();
  assert.equal(page.hasUnsavedChanges(), false);
});

test('设置加载失败时提供重试入口并重新加载', async (t) => {
  const page = await loadSettingsPage(t, [
    { key: 'max_concurrency', value: '10', value_type: 'int', description: '' }
  ], { max_concurrency: '10' }, { initialLoadError: '网络错误' });

  assert.match(page.settingsBody.innerHTML, /网络错误/);
  assert.match(page.settingsBody.innerHTML, /data-action="retry-load-settings"/);
  const loadsBefore = page.requests.filter(({ url }) => url === '/admin/settings').length;

  page.clickRetryLoad();
  await flushAsyncWork();

  assert.equal(page.requests.filter(({ url }) => url === '/admin/settings').length, loadsBefore + 1);
});

test('dynamic reasoning setting validates and saves its JSON contract without a restart', async (t) => {
  const initial = JSON.stringify({ enabled: false, decision_model: '', business_models: [], timeout_ms: 2000 });
  const value = JSON.stringify({ enabled: true, decision_model: 'jev-latest', business_models: ['gpt-5'], timeout_ms: 1500 });
  const page = await loadSettingsPage(t, [{key: 'reasoning_auto', value: initial, value_type: 'string'}], {reasoning_auto: initial});
  page.inputs.reasoning_auto.value = value;
  page.setAllowSave(true);
  page.saveButton.click();
  await flushAsyncWork();
  assert.equal(saveRequests(page).length, 1);
  assert.deepEqual(JSON.parse(JSON.parse(saveRequests(page)[0].options.body).reasoning_auto), JSON.parse(value));
  assert.deepEqual(page.prompts, [confirmHotMessage]);
  assert.deepEqual(page.successes, ['settings.msg.savedHot']);
  page.inputs.reasoning_auto.value = JSON.stringify({...JSON.parse(value), timeout_ms: 99});
  page.saveButton.click();
  await flushAsyncWork();
  assert.equal(saveRequests(page).length, 1);
  assert.equal(page.inputs.reasoning_auto.getAttribute('aria-invalid'), 'true');
});

test('invalid persisted reasoning JSON leaves settings reload and repair usable', async (t) => {
  const invalid = JSON.stringify({ enabled: false, business_models: null });
  const page = await loadSettingsPage(t, [
    { key: 'reasoning_auto', value: invalid, value_type: 'json' },
    { key: 'max_concurrency', value: '10', value_type: 'int' }
  ], { reasoning_auto: invalid, max_concurrency: '10' });
  page.clickRetryLoad();
  await flushAsyncWork();
  assert.equal(page.inputs.reasoning_auto.value, invalid);
  assert.equal(page.inputs.max_concurrency.value, '10');
  await page.openReasoning();
  page.reasoningAction('reset-reasoning-auto');
  page.reasoningAction('apply-reasoning-auto');
  page.setAllowSave(true);
  page.saveButton.click();
  await flushAsyncWork();
  assert.equal(saveRequests(page).length, 1);
  assert.deepEqual(JSON.parse(JSON.parse(saveRequests(page)[0].options.body).reasoning_auto), reasoningDefaults);
});

test('persisted reasoning JSON formatting does not mark the page dirty', async (t) => {
  const persisted = '{ "enabled": false, "timeout_ms": 2000 }';
  const rendered = JSON.stringify({ enabled: false, decision_model: '', business_models: [], timeout_ms: 2000 });
  const page = await loadSettingsPage(t, [{ key: 'reasoning_auto', value: persisted, value_type: 'json' }], { reasoning_auto: rendered });
  assert.equal(page.hasUnsavedChanges(), false);
});

test('reasoning editor rejects explicit null and incorrect field types', async (t) => {
  const initial = JSON.stringify({ enabled: false });
  const page = await loadSettingsPage(t, [{ key: 'reasoning_auto', value: initial, value_type: 'json' }], { reasoning_auto: initial });
  page.setAllowSave(true);
  for (const [field, invalid] of Object.entries({ enabled: 'false', decision_model: 0, business_models: {}, timeout_ms: '2000' })) {
    for (const value of [null, invalid]) {
      page.editInput('reasoning_auto', JSON.stringify({ enabled: false, [field]: value }));
      page.saveButton.click();
      await flushAsyncWork();
      assert.equal(saveRequests(page).length, 0, field);
      assert.equal(page.inputs.reasoning_auto.getAttribute('aria-invalid'), 'true', field);
    }
  }
});

test('reasoning business models reject only parseable thinking suffixes and wildcards', async (t) => {
  const initial = JSON.stringify({ enabled: false });
  const page = await loadSettingsPage(t, [{ key: 'reasoning_auto', value: initial, value_type: 'json' }], { reasoning_auto: initial });
  page.setAllowSave(true);
  for (const model of ['gpt-5(high)', 'gpt-5( Auto )', 'gpt-5(8192)', 'gpt-5(none)', 'gpt-*', 'gpt-5?']) {
    page.editInput('reasoning_auto', JSON.stringify({ enabled: false, business_models: [model] }));
    page.saveButton.click();
    await flushAsyncWork();
    assert.equal(saveRequests(page).length, 0, model);
    assert.equal(page.inputs.reasoning_auto.getAttribute('aria-invalid'), 'true', model);
  }
  const accepted = JSON.stringify({ enabled: false, business_models: ['foo(bar)', 'gpt-5(preview)', '(high)'] });
  page.editInput('reasoning_auto', accepted);
  page.saveButton.click();
  await flushAsyncWork();
  assert.equal(saveRequests(page).length, 1);
  assert.deepEqual(JSON.parse(JSON.parse(saveRequests(page)[0].options.body).reasoning_auto), JSON.parse(accepted));
});

const reasoningDefaults = { enabled: false, decision_model: '', business_models: [], timeout_ms: 2000 };
const privacyDefaults = {
  enabled: false, secrets: true, personal: false,
  kinds: { ssn: true, passport: true, iban: true, birthday: true, mac: true, serial: true, home: false, userhost: false, ip: false, bucket: false },
  words: [], words_any_case: false, words_whole: false, rules: [], allow: []
};

function loadPrivacyPage(t, config = {}) {
  const value = typeof config === 'string' ? config : JSON.stringify(config);
  return loadSettingsPage(t, [{ key: 'privacy_redaction', value, value_type: 'json' }], { privacy_redaction: value });
}

test('privacy defaults are disabled and formatting alone does not create changes', async (t) => {
  const page = await loadPrivacyPage(t);
  assert.equal(page.hasUnsavedChanges(), false);
  page.openPrivacy();
  assert.equal(page.privacyField('enabled').checked, false);
  assert.equal(page.privacyField('secrets').checked, true);
  assert.equal(page.privacyField('personal').checked, false);
  for (const [kind, checked] of Object.entries(privacyDefaults.kinds)) {
    assert.equal(page.privacyField('kinds.' + kind).checked, checked, kind);
  }
  await page.applyPrivacy();
  assert.equal(page.hasUnsavedChanges(), false);
  assert.equal(saveRequests(page).length, 0);
});

test('privacy rules with an omitted kind retain the server default without creating changes', async (t) => {
  const page = await loadPrivacyPage(t, { rules: [{ prefix: 'internal-' }] });
  assert.equal(page.hasUnsavedChanges(), false);
  page.openPrivacy();
  assert.equal(page.privacyField('enabled').disabled, false);
  await page.applyPrivacy();
  assert.equal(page.hasUnsavedChanges(), false);
  assert.deepEqual(JSON.parse(page.inputs.privacy_redaction.value).rules, [{ prefix: 'internal-', kind: 'CUSTOM' }]);
});

test('privacy modal applies word lists and rules to the batch save payload with hot reload', async (t) => {
  const page = await loadPrivacyPage(t);
  page.openPrivacy();
  page.editPrivacyField('enabled', true);
  page.editPrivacyField('personal', true);
  page.editPrivacyField('kinds.ip', true);
  page.editPrivacyField('words', ' internal-project \n\n user@example.com ');
  page.editPrivacyField('words_any_case', true);
  page.editPrivacyField('words_whole', true);
  page.editPrivacyField('allow', ' allowed@example.com \n');
  page.privacyAction('add-privacy-rule');
  page.editPrivacyRule(0, 'kind', 'INTERNAL_KEY');
  page.editPrivacyRule(0, 'prefix', 'project-secret-');
  page.privacyAction('add-privacy-rule');
  page.editPrivacyRule(1, 'kind', 'ACCOUNT');
  page.editPrivacyRule(1, 'regex', '(?P<account>ACC[0-9]+)');
  page.privacyAction('add-privacy-rule');
  page.privacyAction('remove-privacy-rule', 2);
  assert.equal(page.hasUnsavedChanges(), false);
  await page.applyPrivacy();
  assert.equal(page.hasUnsavedChanges(), true);
  assert.equal(saveRequests(page).length, 0);
  page.setAllowSave(true);
  page.saveButton.click();
  await flushAsyncWork();
  assert.equal(saveRequests(page).length, 1);
  assert.deepEqual(page.prompts, [confirmHotMessage]);
  assert.deepEqual(JSON.parse(JSON.parse(saveRequests(page)[0].options.body).privacy_redaction), {
    ...privacyDefaults, enabled: true, personal: true, kinds: { ...privacyDefaults.kinds, ip: true },
    words: ['internal-project', 'user@example.com'], words_any_case: true, words_whole: true,
    rules: [{ kind: 'INTERNAL_KEY', prefix: 'project-secret-' }, { kind: 'ACCOUNT', regex: '(?P<account>ACC[0-9]+)' }],
    allow: ['allowed@example.com']
  });
  assert.ok(!page.successes.some(message => message.startsWith('需重启：')));
});

test('privacy cancel and Escape discard draft edits and restore trigger focus', async (t) => {
  const page = await loadPrivacyPage(t);
  const original = page.inputs.privacy_redaction.value;
  for (const cancel of [() => page.privacyAction('close-privacy-redaction'), () => page.pressDocumentKey('Escape')]) {
    page.openPrivacy();
    page.editPrivacyField('enabled', true);
    page.editPrivacyField('words', 'discard-me');
    cancel();
    assert.equal(page.privacyModal.attributes.get('aria-hidden'), 'true');
    assert.equal(global.document.activeElement, page.privacyButton);
    assert.equal(page.inputs.privacy_redaction.value, original);
    assert.equal(page.hasUnsavedChanges(), false);
    page.openPrivacy();
    assert.equal(page.privacyField('words').value, '');
    page.privacyAction('close-privacy-redaction');
  }
});

test('invalid stored privacy remains locked and unchanged until reset is applied', async (t) => {
  const invalid = '{"enabled":true,"kinds":{"unknown":true}}';
  const page = await loadPrivacyPage(t, invalid);
  page.openPrivacy();
  assert.equal(page.privacyField('enabled').disabled, true);
  page.editPrivacyField('enabled', false);
  page.privacyAction('add-privacy-rule');
  page.privacyAction('apply-privacy-redaction');
  assert.equal(page.inputs.privacy_redaction.value, invalid);
  page.privacyAction('reset-privacy-redaction');
  assert.equal(page.privacyField('enabled').disabled, false);
  page.privacyAction('close-privacy-redaction');
  assert.equal(page.inputs.privacy_redaction.value, invalid);
  assert.equal(page.hasUnsavedChanges(), false);
  page.openPrivacy();
  assert.equal(page.privacyField('enabled').disabled, true);
  page.privacyAction('reset-privacy-redaction');
  await page.applyPrivacy();
  assert.deepEqual(JSON.parse(page.inputs.privacy_redaction.value), privacyDefaults);
  assert.equal(page.hasUnsavedChanges(), true);
});

test('saving unrelated settings does not overwrite invalid stored privacy', async (t) => {
  const invalid = '{invalid';
  const page = await loadSettingsPage(t, [
    { key: 'privacy_redaction', value: invalid, value_type: 'json' },
    { key: 'max_concurrency', value: '10', value_type: 'int' }
  ], { privacy_redaction: invalid, max_concurrency: '10' });
  page.setAllowSave(true);
  page.editInput('max_concurrency', '11');
  page.saveButton.click();
  await flushAsyncWork();
  assert.deepEqual(JSON.parse(saveRequests(page)[0].options.body), { max_concurrency: '11' });
  assert.equal(page.inputs.privacy_redaction.value, invalid);
});

test('privacy apply validates the draft with the server and stays open when it is rejected', async (t) => {
  const page = await loadPrivacyPage(t);
  const original = page.inputs.privacy_redaction.value;
  page.openPrivacy();
  page.privacyAction('add-privacy-rule');
  page.editPrivacyRule(0, 'regex', '(');
  page.privacyAction('apply-privacy-redaction');
  page.privacyAction('apply-privacy-redaction');
  assert.equal(page.privacyPreviews.length, 1);
  assert.equal(page.privacyApplyButton.disabled, true);
  const pending = { ...privacyDefaults, rules: [{ kind: 'CUSTOM', regex: '(' }] };
  assert.equal(page.privacyPreviews[0].body.text, '');
  assert.deepEqual(JSON.parse(page.privacyPreviews[0].body.config), pending);
  page.privacyPreviews[0].reject(new Error('privacy rule 1 has an invalid or empty-matching regular expression'));
  await flushAsyncWork();
  assert.equal(page.privacyModal.attributes.get('aria-hidden'), 'false');
  assert.equal(page.privacyError.hidden, false);
  assert.equal(page.privacyError.textContent, 'privacy rule 1 has an invalid or empty-matching regular expression');
  assert.equal(page.privacyApplyButton.disabled, false);
  assert.equal(page.inputs.privacy_redaction.value, original);
  assert.equal(page.hasUnsavedChanges(), false);

  // 留空的类型按 CUSTOM 处理，与服务端一致。
  page.editPrivacyRule(0, 'kind', '  ');
  page.editPrivacyRule(0, 'regex', 'account-[0-9]+');
  await page.applyPrivacy();
  assert.equal(page.privacyModal.attributes.get('aria-hidden'), 'true');
  assert.deepEqual(JSON.parse(page.privacyPreviews[1].body.config).rules, [{ kind: 'CUSTOM', regex: 'account-[0-9]+' }]);
  assert.equal(page.inputs.privacy_redaction.value, page.privacyPreviews[1].body.config);
  assert.equal(page.hasUnsavedChanges(), true);
});

test('privacy apply ignores a server answer that arrives after the dialog was cancelled', async (t) => {
  const page = await loadPrivacyPage(t);
  const original = page.inputs.privacy_redaction.value;
  page.openPrivacy();
  page.editPrivacyField('enabled', true);
  page.privacyAction('apply-privacy-redaction');
  page.privacyAction('close-privacy-redaction');
  page.privacyPreviews[0].resolve({ masked: '', hits: [] });
  await flushAsyncWork();
  assert.equal(page.inputs.privacy_redaction.value, original);
  assert.equal(page.hasUnsavedChanges(), false);
});

test('privacy apply shows broad rule warnings from empty-sample validation without blocking save', async (t) => {
  const page = await loadPrivacyPage(t);
  page.openPrivacy();
  page.privacyAction('add-privacy-rule');
  page.editPrivacyRule(0, 'regex', '[a-z]+');
  page.privacyAction('apply-privacy-redaction');
  assert.equal(page.privacyPreviews[0].body.text, '');
  page.privacyPreviews[0].resolve({ masked: '', hits: [], warnings: [{ rule_index: 0, code: 'broad_pattern' }] });
  await flushAsyncWork();
  const warnings = page.privacyTest('privacyRedactionWarnings');
  assert.equal(warnings.hidden, false);
  assert.equal(warnings.textContent, '隐私脱敏规则提示（仍可应用和保存）：\n自定义规则第 1 条匹配范围过宽，可能遮蔽普通文本，请检查。');
  assert.equal(page.privacyError.hidden, true);
  assert.equal(page.privacyModal.attributes.get('aria-hidden'), 'true');
  assert.equal(page.hasUnsavedChanges(), true);
  assert.deepEqual(JSON.parse(page.inputs.privacy_redaction.value).rules, [{ kind: 'CUSTOM', regex: '[a-z]+' }]);

  page.setAllowSave(true);
  page.saveButton.click();
  await flushAsyncWork();
  assert.deepEqual(JSON.parse(JSON.parse(saveRequests(page)[0].options.body).privacy_redaction).rules,
    [{ kind: 'CUSTOM', regex: '[a-z]+' }]);

  page.openPrivacy();
  page.editPrivacyRule(0, 'regex', 'account-[0-9]+');
  await page.applyPrivacy();
  assert.equal(warnings.hidden, true);
  assert.equal(warnings.textContent, '');
});

test('server save errors preserve the pending privacy configuration for repair', async (t) => {
  const page = await loadPrivacyPage(t);
  page.openPrivacy();
  page.privacyAction('add-privacy-rule');
  page.editPrivacyRule(0, 'kind', 'CUSTOM');
  page.editPrivacyRule(0, 'regex', 'account-[a-z]+');
  await page.applyPrivacy();
  page.setAllowSave(true);
  page.failNextSave('database is locked');
  page.saveButton.click();
  await flushAsyncWork();
  assert.equal(saveRequests(page).length, 1);
  assert.ok(page.errors.some(error => error.includes('database is locked')));
  assert.equal(page.hasUnsavedChanges(), true);
  page.openPrivacy();
  page.editPrivacyRule(0, 'regex', 'account-[0-9]+');
  await page.applyPrivacy();
  page.saveButton.click();
  await flushAsyncWork();
  assert.equal(saveRequests(page).length, 2);
  assert.deepEqual(JSON.parse(JSON.parse(saveRequests(page)[1].options.body).privacy_redaction).rules,
    [{ kind: 'CUSTOM', regex: 'account-[0-9]+' }]);
  assert.equal(page.hasUnsavedChanges(), false);
});

test('privacy rule validation blocks invalid drafts and preserves the pending setting', async (t) => {
  const page = await loadPrivacyPage(t);
  page.openPrivacy();
  const original = page.inputs.privacy_redaction.value;
  page.privacyAction('add-privacy-rule');
  page.editPrivacyRule(0, 'kind', 'TOKEN');
  for (const [prefix, regex] of [['ab', ''], ['x'.repeat(65), ''], ['', 'x'.repeat(301)], ['abc', 'abc'], ['', '']]) {
    page.editPrivacyRule(0, 'prefix', prefix);
    page.editPrivacyRule(0, 'regex', regex);
    page.privacyAction('apply-privacy-redaction');
    assert.equal(page.inputs.privacy_redaction.value, original);
    assert.equal(page.privacyError.hidden, false);
  }
  page.editPrivacyRule(0, 'prefix', 'valid-prefix');
  for (const kind of ['lowercase', '1TOKEN', 'X'.repeat(25), 'WITH-HYPHEN']) {
    page.editPrivacyRule(0, 'kind', kind);
    page.privacyAction('apply-privacy-redaction');
    assert.equal(page.inputs.privacy_redaction.value, original);
  }
  page.editPrivacyRule(0, 'kind', 'TOKEN');
  await page.applyPrivacy();
  assert.deepEqual(JSON.parse(page.inputs.privacy_redaction.value).rules, [{ kind: 'TOKEN', prefix: 'valid-prefix' }]);
});

test('privacy test dialog previews the unapplied draft and reruns it on reopen', async (t) => {
  const page = await loadPrivacyPage(t);
  const original = page.inputs.privacy_redaction.value;
  const testShown = () => page.privacyTestModal.classList.contains('show');
  page.openPrivacy();
  page.privacyAction('add-privacy-rule');
  page.editPrivacyRule(0, 'kind', 'RELAY');
  page.editPrivacyRule(0, 'prefix', 'relay_');
  page.privacyAction('open-privacy-test');
  assert.equal(testShown(), true);
  assert.equal(page.privacyModal.attributes.get('inert'), '');
  assert.equal(page.privacyTest('privacyTestRun').disabled, true);
  assert.equal(page.privacyPreviews.length, 0);

  page.editPrivacyTest('key relay_ABCDEF123456');
  assert.equal(page.privacyTest('privacyTestRun').disabled, false);
  page.privacyTestAction('test-privacy-redaction');
  assert.equal(page.privacyPreviews.length, 1);
  assert.equal(page.privacyPreviews[0].body.text, 'key relay_ABCDEF123456');
  assert.deepEqual(JSON.parse(page.privacyPreviews[0].body.config), { ...privacyDefaults, rules: [{ kind: 'RELAY', prefix: 'relay_' }] });
  const placeholder = '{{CCLOAD_RELAY_0123456789abcdef}}';
  page.privacyPreviews[0].resolve({ masked: 'key ' + placeholder, hits: [{ kind: 'RELAY', source: 'custom', rule_index: 0, value: 'relay_ABCDEF123456', placeholder }] });
  await flushAsyncWork();
  const result = page.privacyTest('privacyTestResult');
  const resultText = () => result.querySelectorAll('pre').map(output => output.textContent);
  const resultHits = () => result.querySelectorAll('li').map(item => item.children.map(cell => cell.textContent));
  assert.equal(result.hidden, false);
  assert.deepEqual(resultText(), ['key ' + placeholder]);
  assert.deepEqual(resultHits(), [['自定义规则第 1 条 · RELAY', 'relay_ABCDEF123456']]);
  assert.equal(page.privacyTest('privacyTestStatus').textContent, 'settings.privacyRedaction.testHits');

  // 改样例让旧结果失效；改之前发出的请求晚到也不显示。
  page.privacyTestAction('test-privacy-redaction');
  page.editPrivacyTest('key other_ABCDEF123456');
  assert.equal(result.hidden, true);
  page.privacyPreviews[1].resolve({ masked: 'stale', hits: [{ kind: 'RELAY', value: 'stale', placeholder: 'stale' }] });
  await flushAsyncWork();
  assert.equal(result.hidden, true);
  assert.equal(page.privacyTest('privacyTestRun').disabled, false);

  // Esc 只关测试弹窗；回到编辑弹窗改规则后重新打开，保留样例并按新草稿自动重跑。
  page.pressDocumentKey('Escape');
  assert.equal(testShown(), false);
  assert.equal(page.privacyModal.attributes.get('aria-hidden'), 'false');
  assert.equal(page.privacyModal.attributes.has('inert'), false);
  page.editPrivacyRule(0, 'prefix', 'other_');
  page.privacyAction('open-privacy-test');
  assert.equal(page.privacyTest('privacyTestInput').value, 'key other_ABCDEF123456');
  assert.equal(page.privacyPreviews.length, 3);
  assert.deepEqual(JSON.parse(page.privacyPreviews[2].body.config).rules, [{ kind: 'RELAY', prefix: 'other_' }]);
  const otherPlaceholder = '{{CCLOAD_RELAY_fedcba9876543210}}';
  page.privacyPreviews[2].resolve({ masked: 'key ' + otherPlaceholder, hits: [{ kind: 'RELAY', source: 'custom', rule_index: 0, value: 'other_ABCDEF123456', placeholder: otherPlaceholder }] });
  await flushAsyncWork();
  assert.deepEqual(resultHits(), [['自定义规则第 1 条 · RELAY', 'other_ABCDEF123456']]);

  // 关闭测试弹窗后，隐藏的输入框与结果区不再保留样例或原值。
  page.privacyTestAction('close-privacy-test');
  assert.equal(page.privacyTest('privacyTestInput').value, '');
  assert.equal(result.hidden, true);
  assert.equal(result.textContent, '');
  page.privacyAction('close-privacy-redaction');
  assert.equal(page.inputs.privacy_redaction.value, original);
  assert.equal(page.hasUnsavedChanges(), false);
  page.openPrivacy();
  page.privacyAction('open-privacy-test');
  assert.equal(page.privacyTest('privacyTestInput').value, '');
  assert.equal(page.privacyPreviews.length, 3);
});

test('privacy test reports invalid drafts and server errors without sending or saving', async (t) => {
  const page = await loadPrivacyPage(t);
  page.openPrivacy();
  page.privacyAction('add-privacy-rule');
  page.editPrivacyRule(0, 'kind', 'lowercase');
  page.editPrivacyRule(0, 'prefix', 'valid-prefix');
  page.privacyAction('open-privacy-test');
  page.editPrivacyTest('sample');
  page.privacyTestAction('test-privacy-redaction');
  const status = page.privacyTest('privacyTestStatus');
  assert.equal(page.privacyPreviews.length, 0);
  assert.ok(status.textContent);
  assert.equal(status.classList.contains('is-error'), true);

  page.privacyTestAction('close-privacy-test');
  page.editPrivacyRule(0, 'kind', 'TOKEN');
  page.privacyAction('open-privacy-test');
  page.privacyPreviews[0].reject(new Error('privacy text exceeds limit'));
  await flushAsyncWork();
  assert.ok(status.textContent.includes('privacy text exceeds limit'));
  assert.equal(status.classList.contains('is-error'), true);
  assert.equal(page.privacyTest('privacyTestResult').hidden, true);
  assert.equal(saveRequests(page).length, 0);
});

test('privacy test identifies builtin, word list and custom rule sources', async (t) => {
  const page = await loadPrivacyPage(t);
  page.openPrivacy();
  page.privacyAction('open-privacy-test');
  page.editPrivacyTest('key first second');
  page.privacyTestAction('test-privacy-redaction');
  page.privacyPreviews[0].resolve({
    masked: 'masked',
    hits: [
      { kind: 'API_KEY', source: 'builtin', value: 'key', placeholder: 'key-placeholder' },
      { kind: 'WORD', source: 'word', rule_index: 1, value: 'first', placeholder: 'word-placeholder' },
      { kind: 'TOKEN', source: 'custom', rule_index: 2, value: 'second', placeholder: 'custom-placeholder' }
    ],
    warnings: []
  });
  await flushAsyncWork();
  const hits = page.privacyTest('privacyTestResult').querySelectorAll('li');
  assert.deepEqual(hits.map(item => item.children.map(cell => cell.textContent)), [
    ['内置规则 · API_KEY', 'key'], ['词表第 2 项', 'first'], ['自定义规则第 3 条 · TOKEN', 'second']
  ]);
  assert.equal(page.privacyTest('privacyTestWarnings').hidden, true);
});

test('privacy test shows broad rule warnings even without hits and clears them with the sample', async (t) => {
  const page = await loadPrivacyPage(t);
  page.openPrivacy();
  page.privacyAction('open-privacy-test');
  page.editPrivacyTest('sample');
  page.privacyTestAction('test-privacy-redaction');
  page.privacyPreviews[0].resolve({ masked: 'sample', hits: [], warnings: [{ rule_index: 1, code: 'broad_pattern' }] });
  await flushAsyncWork();
  const warnings = page.privacyTest('privacyTestWarnings');
  assert.equal(warnings.hidden, false);
  assert.equal(warnings.textContent, '隐私脱敏规则提示（仍可应用和保存）：\n自定义规则第 2 条匹配范围过宽，可能遮蔽普通文本，请检查。');
  assert.equal(page.privacyTest('privacyTestStatus').textContent, 'settings.privacyRedaction.testNoHits');
  assert.equal(page.privacyTest('privacyTestResult').hidden, true);
  page.editPrivacyTest('new sample');
  assert.equal(warnings.hidden, true);
  assert.equal(warnings.textContent, '');
  assert.equal(page.hasUnsavedChanges(), false);
  assert.equal(saveRequests(page).length, 0);
});

test('locked privacy settings disable the test dialog', async (t) => {
  const page = await loadPrivacyPage(t, '{"enabled":true,"kinds":{"unknown":true}}');
  page.openPrivacy();
  assert.equal(page.privacyTest('privacyTestOpen').disabled, true);
  page.privacyAction('open-privacy-test');
  assert.equal(page.privacyTestModal.classList.contains('show'), false);
  page.privacyAction('reset-privacy-redaction');
  assert.equal(page.privacyTest('privacyTestOpen').disabled, false);
});

test('privacy save validates list and rule boundaries even when disabled', async (t) => {
  const page = await loadPrivacyPage(t);
  page.setAllowSave(true);
  const invalidConfigs = [
    { words: Array(257).fill('word') }, { allow: Array(257).fill('allowed') },
    { words: ['x'.repeat(513)] }, { allow: ['x'.repeat(513)] }, { words: ['two\nlines'] },
    { rules: Array(33).fill({ kind: 'TOKEN', prefix: 'abc' }) }, { enabled: null }, { kinds: null }
  ];
  for (const config of invalidConfigs) {
    page.editInput('privacy_redaction', JSON.stringify(config));
    page.saveButton.click();
    await flushAsyncWork();
    assert.equal(saveRequests(page).length, 0);
    assert.equal(page.inputs.privacy_redaction.getAttribute('aria-invalid'), 'true');
  }
  page.editInput('privacy_redaction', JSON.stringify({ words: Array(256).fill('😀'.repeat(512)), allow: ['allowed'] }));
  page.saveButton.click();
  await flushAsyncWork();
  assert.equal(saveRequests(page).length, 1);
});

const reasoningConfigured = { ...reasoningDefaults, enabled: true, decision_model: 'judge', business_models: ['gpt-5'], timeout_ms: 1500 };

async function loadReasoningPage(t, config = reasoningConfigured, options = {}) {
  const value = JSON.stringify(config);
  return loadSettingsPage(t, [{ key: 'reasoning_auto', value, value_type: 'json', default_value: JSON.stringify(reasoningDefaults) }],
    { reasoning_auto: value }, {
      channels: [
        { id: 7, enabled: true, auth_type: 'system_one', name: 'Judge', models: ['judge'] },
        { id: 8, enabled: true, auth_type: 'system_one', name: 'Other', models: ['other-judge'] },
        { id: 9, enabled: true, auth_type: 'api_key', name: 'Chat', models: ['gpt-5'] }
      ],
      filterModels: ['gpt-5', 'claude', 'foo(bar)', 'gpt-5(high)', 'wild-*'], ...options
    });
}

test('reasoning modal selected-count toggle lists only selected models and combines with the name filter', async (t) => {
  const page = await loadReasoningPage(t);
  await page.openReasoning();
  page.selectReasoningModel('claude', true);
  page.selectReasoningModel('gpt-5', true);
  page.reasoningAction('toggle-reasoning-selected');
  assert.deepEqual(page.reasoningModels().map(input => [input.value, input.checked]), [['claude', true], ['gpt-5', true]]);
  page.selectReasoningModel('claude', false);
  page.filterReasoningModels('GPT');
  assert.deepEqual(page.reasoningModels().map(input => input.value), ['gpt-5']);
  page.filterReasoningModels('');
  assert.deepEqual(page.reasoningModels().map(input => input.value), ['gpt-5']);
  page.reasoningAction('toggle-reasoning-selected');
  assert.deepEqual(page.reasoningModels().map(input => input.value), ['claude', 'foo(bar)', 'gpt-5']);
  page.reasoningAction('toggle-reasoning-selected');
  page.reasoningAction('close-reasoning-auto');
  await page.openReasoning();
  assert.deepEqual(page.reasoningModels().map(input => input.value).sort(), ['claude', 'foo(bar)', 'gpt-5']);
});

test('reasoning modal preserves checked models across filters and applies through save-all', async (t) => {
  const page = await loadReasoningPage(t);
  await page.openReasoning();
  assert.deepEqual(page.reasoningModels().map(input => input.value).sort(), ['claude', 'foo(bar)', 'gpt-5']);
  page.selectReasoningModel('claude', true);
  page.filterReasoningModels('GPT');
  assert.deepEqual(page.reasoningModels().map(input => [input.value, input.checked]), [['gpt-5', true]]);
  page.selectReasoningModel('gpt-5', false);
  page.filterReasoningModels('');
  assert.equal(page.reasoningModels().find(input => input.value === 'claude').checked, true);
  assert.equal(page.reasoningModels().find(input => input.value === 'gpt-5').checked, false);
  assert.equal(page.hasUnsavedChanges(), false);
  page.reasoningAction('apply-reasoning-auto');
  assert.equal(saveRequests(page).length, 0);
  assert.equal(page.hasUnsavedChanges(), true);
  page.setAllowSave(true);
  page.saveButton.click();
  await flushAsyncWork();
  const payload = JSON.parse(JSON.parse(saveRequests(page)[0].options.body).reasoning_auto);
  assert.deepEqual(payload, { ...reasoningConfigured, business_models: ['claude'] });
  assert.equal(page.prompts.length, 1);
});

test('reasoning modal cancellation discards edits and restores the persisted draft on reopen', async (t) => {
  const page = await loadReasoningPage(t);
  const original = page.inputs.reasoning_auto.value;
  await page.openReasoning();
  page.editReasoningField('timeout_ms', 3);
  page.selectReasoningModel('claude', true);
  page.reasoningAction('close-reasoning-auto');
  assert.equal(page.inputs.reasoning_auto.value, original);
  assert.equal(page.hasUnsavedChanges(), false);
  await page.openReasoning();
  assert.equal(page.reasoningField('reasoningTimeout').value, '1.5');
  assert.equal(page.reasoningModels().find(input => input.value === 'claude').checked, false);
});

test('Escape closes the reasoning modal even when focus left the modal', async (t) => {
  const page = await loadReasoningPage(t);
  const original = page.inputs.reasoning_auto.value;
  await page.openReasoning();
  page.editReasoningField('timeout_ms', 3);
  page.pressDocumentKey('Escape', { defaultPrevented: true });
  assert.equal(page.reasoningModal.classList.contains('show'), true);
  assert.equal(page.pressDocumentKey('Escape').defaultPrevented, true);
  assert.equal(page.reasoningModal.classList.contains('show'), false);
  assert.equal(page.inputs.reasoning_auto.value, original);
});

test('reasoning decision models combine enabled System One channels and deduplicate enabled models', async (t) => {
  const page = await loadReasoningPage(t, reasoningConfigured, {
    channels: [
      { id: 7, enabled: true, auth_type: 'system_one', models: ['judge', { model: 'shared', disabled: false }, { model: 'disabled', disabled: true }] },
      { id: 8, enabled: true, auth_type: 'system_one', models: [{ model: 'other-judge' }, 'shared', { model: 'judge', disabled: true }] },
      { id: 9, enabled: true, auth_type: 'api_key', models: ['chat'] },
      { id: 10, enabled: false, auth_type: 'system_one', models: ['offline-judge'] }
    ]
  });
  await page.openReasoning();
  const select = page.reasoningField('reasoningDecisionModel');
  assert.deepEqual(select.options.map(option => option.value), ['', 'judge', 'shared', 'other-judge']);
  assert.equal(select.value, 'judge');
});

test('reasoning decision model is required when enabled and can change across channels', async (t) => {
  const page = await loadReasoningPage(t);
  await page.openReasoning();
  page.editReasoningField('decision_model', '');
  page.reasoningAction('apply-reasoning-auto');
  assert.equal(page.hasUnsavedChanges(), false);
  assert.equal(page.reasoningError.hidden, false);
  page.editReasoningField('decision_model', 'other-judge');
  page.reasoningAction('apply-reasoning-auto');
  assert.deepEqual(JSON.parse(page.inputs.reasoning_auto.value), { ...reasoningConfigured, decision_model: 'other-judge' });
});

test('reasoning preserves a stored decision model absent from enabled channels and saves without a channel ID', async (t) => {
  const page = await loadReasoningPage(t, { ...reasoningConfigured, decision_channel_id: 99, decision_model: 'retired-judge' });
  await page.openReasoning();
  const select = page.reasoningField('reasoningDecisionModel');
  assert.deepEqual(select.options.map(option => option.value), ['', 'retired-judge', 'judge', 'other-judge']);
  assert.equal(select.value, 'retired-judge');
  assert.equal(page.hasUnsavedChanges(), false);
  page.editReasoningField('timeout_ms', 3);
  page.reasoningAction('apply-reasoning-auto');
  page.setAllowSave(true);
  page.saveButton.click();
  await flushAsyncWork();
  const saved = JSON.parse(JSON.parse(saveRequests(page)[0].options.body).reasoning_auto);
  assert.deepEqual(saved, { ...reasoningConfigured, decision_model: 'retired-judge', timeout_ms: 3000 });
});

test('invalid reasoning timeout cannot apply a modal draft', async (t) => {
  const page = await loadReasoningPage(t);
  const original = page.inputs.reasoning_auto.value;
  await page.openReasoning();
  for (const timeout of [0.099, 10.001, '']) {
    page.editReasoningField('timeout_ms', timeout);
    page.reasoningAction('apply-reasoning-auto');
    assert.equal(page.inputs.reasoning_auto.value, original);
    assert.equal(page.reasoningError.hidden, false);
    assert.equal(page.reasoningModal.classList.contains('show'), true);
  }
  assert.equal(page.hasUnsavedChanges(), false);
});

test('stored reasoning request models remain selected when absent from available models', async (t) => {
  const config = { ...reasoningConfigured, business_models: ['retired-model'] };
  const page = await loadReasoningPage(t, config);
  await page.openReasoning();
  assert.equal(page.reasoningModels().find(input => input.value === 'retired-model').checked, true);
  page.selectReasoningModel('retired-model', false);
  page.filterReasoningModels('retired');
  assert.deepEqual(page.reasoningModels().map(input => [input.value, input.checked]), [['retired-model', false]]);
  page.filterReasoningModels('');
  assert.equal(page.reasoningModels().find(input => input.value === 'retired-model').checked, false);
  page.selectReasoningModel('retired-model', true);
  page.selectReasoningModel('claude', true);
  page.reasoningAction('apply-reasoning-auto');
  assert.deepEqual(JSON.parse(page.inputs.reasoning_auto.value).business_models, ['retired-model', 'claude']);
});

test('invalid persisted reasoning is locked until reset and reset cancellation preserves original data', async (t) => {
  const config = { ...reasoningConfigured, business_models: ['gpt-5(high)'] };
  const page = await loadReasoningPage(t, config);
  const original = page.inputs.reasoning_auto.value;
  await page.openReasoning();
  assert.equal(page.reasoningField('reasoningTimeout').disabled, true);
  assert.equal(page.reasoningField('reasoningDecisionModel').disabled, true);
  page.editReasoningField('timeout_ms', 3);
  page.reasoningAction('apply-reasoning-auto');
  assert.equal(page.inputs.reasoning_auto.value, original);
  page.reasoningAction('reset-reasoning-auto');
  assert.equal(page.reasoningField('reasoningTimeout').disabled, false);
  assert.equal(page.reasoningField('reasoningDecisionModel').disabled, false);
  page.editReasoningField('timeout_ms', 3);
  assert.equal(page.inputs.reasoning_auto.value, original);
  page.reasoningAction('close-reasoning-auto');
  assert.equal(page.inputs.reasoning_auto.value, original);
  assert.equal(page.hasUnsavedChanges(), false);
  await page.openReasoning();
  assert.equal(page.reasoningField('reasoningTimeout').disabled, true);
  page.reasoningAction('reset-reasoning-auto');
  page.editReasoningField('timeout_ms', 3);
  page.reasoningAction('apply-reasoning-auto');
  assert.deepEqual(JSON.parse(page.inputs.reasoning_auto.value), { ...reasoningDefaults, timeout_ms: 3000 });
});

test('reasoning option load failure preserves the stored decision and selected models', async (t) => {
  const page = await loadReasoningPage(t, reasoningConfigured, { reasoningLoadError: 'offline' });
  await page.openReasoning();
  assert.equal(page.reasoningError.hidden, false);
  assert.equal(page.reasoningField('reasoningDecisionModel').value, 'judge');
  assert.deepEqual(page.reasoningModels().map(input => [input.value, input.checked]), [['gpt-5', true]]);
  page.editReasoningField('timeout_ms', 2.5);
  page.reasoningAction('apply-reasoning-auto');
  assert.deepEqual(JSON.parse(page.inputs.reasoning_auto.value), { ...reasoningConfigured, timeout_ms: 2500 });
});


test('reasoning header checkbox selects visible models and preserves selections outside the filter', async (t) => {
  const page = await loadReasoningPage(t);
  await page.openReasoning();
  const header = page.reasoningField('reasoningModelsSelectAll');
  assert.equal(header.checked, false);
  assert.equal(header.indeterminate, true);
  page.toggleReasoningModels(true);
  assert.equal(header.checked, true);
  assert.equal(header.indeterminate, false);
  assert.ok(page.reasoningModels().every(model => model.checked));
  page.filterReasoningModels('GPT');
  assert.equal(header.checked, true);
  page.toggleReasoningModels(false);
  assert.equal(header.checked, false);
  assert.equal(header.indeterminate, false);
  page.filterReasoningModels('');
  assert.equal(header.indeterminate, true);
  assert.equal(page.reasoningModels().find(model => model.value === 'claude').checked, true);
  page.selectReasoningModel('foo(bar)', false);
  assert.equal(header.indeterminate, true);
  page.selectReasoningModel('claude', false);
  assert.equal(header.indeterminate, false);
  page.toggleReasoningModels(true);
  page.filterReasoningModels('GPT');
  page.toggleReasoningModels(false);
  page.reasoningAction('apply-reasoning-auto');
  page.setAllowSave(true);
  page.saveButton.click();
  await flushAsyncWork();
  const saved = JSON.parse(JSON.parse(saveRequests(page)[0].options.body).reasoning_auto);
  assert.deepEqual(saved.business_models.sort(), ['claude', 'foo(bar)']);
});

test('reasoning header checkbox is disabled for no matches and invalid stored configuration', async (t) => {
  const page = await loadReasoningPage(t, { ...reasoningConfigured, business_models: ['gpt-5(high)'] });
  await page.openReasoning();
  assert.equal(page.reasoningField('reasoningModelsSelectAll').disabled, true);
  page.reasoningAction('reset-reasoning-auto');
  await flushAsyncWork();
  const header = page.reasoningField('reasoningModelsSelectAll');
  assert.equal(header.disabled, false);
  page.filterReasoningModels('no-matches');
  assert.equal(header.disabled, true);
  assert.equal(header.checked, false);
  assert.equal(header.indeterminate, false);
  page.filterReasoningModels('GPT');
  assert.equal(header.disabled, false);
  page.toggleReasoningModels(true);
  assert.equal(header.checked, true);
  page.reasoningAction('close-reasoning-auto');
  assert.equal(page.hasUnsavedChanges(), false);
});
