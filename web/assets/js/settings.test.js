const assert = require('node:assert/strict');
const test = require('node:test');

const confirmMessage = '保存任意设置后,服务会在约 2 秒后自动重启以生效，是否继续';

function flushAsyncWork() {
  return new Promise((resolve) => setImmediate(resolve));
}

async function loadSettingsPage(t, settings, inputValues) {
  const clickListeners = [];
  const bodyListeners = new Map();
  const multimodalModalListeners = new Map();
  const multimodalModalClasses = new Set();
  let multimodalRows = [];
  const saveButton = {
    dataset: {},
    addEventListener(type, listener) {
      if (type === 'click') clickListeners.push(listener);
    },
    click() {
      for (const listener of clickListeners) listener();
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
  const multimodalRowsContainer = {
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
    ['multimodalFallbackModal', multimodalModal],
    ['multimodalFallbackRows', multimodalRowsContainer],
    ['multimodalFallbackError', multimodalError]
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
      const match = selector.match(/^input\[name="(.+)"\]$/);
      return match ? (radioGroups.get(match[1]) || []) : [];
    },
    querySelector(selector) {
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
    if (!options) return settings;
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
    multimodalApplyButton,
    multimodalError,
    multimodalModal,
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

  const updateGroup = page.renderCalls.find(({ template, data }) => (
    template === 'tpl-setting-group-row' && data.groupId === 'update'
  ));
  assert.ok(updateGroup, '应将自动更新设置放入独立分组');
  assert.match(updateGroup.data.groupNoticeHtml, /role="note"/);

  const settingRows = page.renderCalls.filter(({ template }) => template === 'tpl-setting-row');
  assert.equal(settingRows.length, 2);
  for (const { data } of settingRows) {
    assert.match(data.inputHtml, /\bdisabled\b/);
    assert.equal(data.resetDisabledAttributes, 'disabled');
  }
});
