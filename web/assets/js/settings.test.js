const assert = require('node:assert/strict');
const test = require('node:test');

const confirmMessage = '保存任意设置后,服务会在约 2 秒后自动重启以生效，是否继续';

function flushAsyncWork() {
  return new Promise((resolve) => setImmediate(resolve));
}

async function loadSettingsPage(t, settings, inputValues) {
  const clickListeners = [];
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
    addEventListener() {},
    appendChild() {}
  };
  const inputs = {};
  const elements = new Map([
    ['save-all-btn', saveButton],
    ['settings-tbody', settingsBody]
  ]);
  for (const [key, value] of Object.entries(inputValues)) {
    const row = { style: {} };
    const input = {
      value,
      closest() {
        return row;
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

  global.window = {
    t(key) {
      if (key === 'settings.msg.confirmSave') return confirmMessage;
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
    querySelectorAll() {
      return [];
    }
  };
  global.TemplateEngine = {
    render(template, data) {
      renderCalls.push({ template, data });
      return null;
    }
  };
  global.escapeHtml = (value) => String(value);
  global.showError = (error) => {
    throw error;
  };
  global.showSuccess = () => {};
  global.confirm = (message) => {
    prompts.push(message);
    return allowSave;
  };
  global.fetchDataWithAuth = async (url, options) => {
    requests.push({ url, options });
    if (!options) return settings;
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
    notifications,
    prompts,
    renderCalls,
    requests,
    saveButton,
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

test('固定选项设置使用原生下拉框', async (t) => {
  const page = await loadSettingsPage(t, [
    { key: 'channel_stats_range', value: 'this_week', value_type: 'string', description: '' },
    { key: 'log_channel_click_action', value: 'navigate', value_type: 'string', description: '' }
  ], {});

  const rows = new Map(page.renderCalls
    .filter(({ template }) => template === 'tpl-setting-row')
    .map(({ data }) => [data.key, data.inputHtml]));

  assert.match(rows.get('channel_stats_range'), /<select\b[^>]*id="channel_stats_range"/);
  assert.match(rows.get('channel_stats_range'), /<option value="this_week" selected>/);
  assert.match(rows.get('log_channel_click_action'), /<select\b[^>]*id="log_channel_click_action"/);
  assert.match(rows.get('log_channel_click_action'), /<option value="navigate" selected>/);
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
  assert.match(updateGroup.data.groupNoticeHtml, /settings\.update\.containerManaged/);
  assert.match(updateGroup.data.groupNoticeHtml, /ghcr\.io\/caidaoli\/ccload:latest/);
  assert.match(updateGroup.data.groupNoticeHtml, /ghcr\.io\/caidaoli\/ccload:beta/);
  assert.match(updateGroup.data.groupNoticeHtml, /docker compose pull/);

  const settingRows = page.renderCalls.filter(({ template }) => template === 'tpl-setting-row');
  assert.equal(settingRows.length, 2);
  for (const { data } of settingRows) {
    assert.match(data.inputHtml, /\bdisabled\b/);
    assert.equal(data.resetDisabledAttributes, 'disabled');
  }
});
