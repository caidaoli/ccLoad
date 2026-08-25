const test = require('node:test');
const assert = require('node:assert/strict');

const mod = require('./channels-custom-rules.js');
const {
  validateRulesLocally,
  collectCustomRulesForSubmit,
  resetCustomRulesState,
  cloneRules,
  getState,
  MAX_RULES
} = mod;

test('cloneRules 深拷贝并规范化字段', () => {
  const rules = {
    headers: [{ action: 'OVERRIDE', name: 'X-Foo', value: 'v' }],
    body: [{ action: 'override', path: 'thinking', value: { type: 'adaptive' } }]
  };
  const copy = cloneRules(rules);
  assert.equal(copy.headers[0].action, 'override');
  assert.equal(copy.body[0].value, '{"type":"adaptive"}');
  // 修改副本不影响源
  copy.headers[0].name = 'Y-Bar';
  assert.equal(rules.headers[0].name, 'X-Foo');
});

test('resetCustomRulesState 接受 null 重置为空', () => {
  resetCustomRulesState({ headers: [{ action: 'override', name: 'X', value: 'v' }], body: [] });
  assert.equal(getState().headers.length, 1);
  resetCustomRulesState(null);
  assert.deepEqual(getState(), { headers: [], body: [] });
});

test('validateRulesLocally 接受合法规则', () => {
  const errors = validateRulesLocally({
    headers: [
      { action: 'override', name: 'X-Api-Version', value: '2025-08-07' },
      { action: 'remove', name: 'User-Agent', value: '' }
    ],
    body: [
      { action: 'override', path: 'thinking.budget_tokens', value: '8192' },
      { action: 'remove', path: 'stop_sequences', value: '' }
    ]
  });
  assert.deepEqual(errors, []);
});

test('validateRulesLocally 拒绝空 header 名', () => {
  const errors = validateRulesLocally({
    headers: [{ action: 'override', name: '   ', value: 'v' }],
    body: []
  });
  assert.equal(errors.length, 1);
  assert.match(errors[0], /#1/);
});

test('validateRulesLocally 拒绝 CRLF 注入', () => {
  const errors = validateRulesLocally({
    headers: [{ action: 'override', name: 'X-Foo\r\nInject', value: 'v' }],
    body: []
  });
  assert.equal(errors.length, 1);
});

test('validateRulesLocally 拒绝认证头改写', () => {
  const errors = validateRulesLocally({
    headers: [
      { action: 'override', name: 'Authorization', value: 'Bearer hijack' },
      { action: 'remove', name: 'x-api-key', value: '' }
    ],
    body: []
  });
  assert.equal(errors.length, 2);
});

test('validateRulesLocally 拒绝非法 body path', () => {
  const errors = validateRulesLocally({
    headers: [],
    body: [{ action: 'override', path: 'messages[0].role', value: '"user"' }]
  });
  assert.equal(errors.length, 1);
});

test('validateRulesLocally 要求 body override 值为合法 JSON', () => {
  const errors = validateRulesLocally({
    headers: [],
    body: [{ action: 'override', path: 'thinking', value: 'not json' }]
  });
  assert.equal(errors.length, 1);
});

test('validateRulesLocally 超过上限报错', () => {
  const headers = Array.from({ length: MAX_RULES + 1 }, (_, i) => ({
    action: 'override', name: `X-${i}`, value: 'v'
  }));
  const errors = validateRulesLocally({ headers, body: [] });
  assert.ok(errors.some((e) => /32/.test(e)));
});

test('validateRulesLocally 空输入不报错', () => {
  assert.deepEqual(validateRulesLocally(null), []);
  assert.deepEqual(validateRulesLocally({}), []);
});

test('collectCustomRulesForSubmit 返回 null 当规则全为空', () => {
  resetCustomRulesState(null);
  assert.equal(collectCustomRulesForSubmit(), null);
});

test('collectCustomRulesForSubmit 过滤掉空 name / 非法 JSON', () => {
  resetCustomRulesState({
    headers: [
      { action: 'override', name: '  ', value: 'v' }, // 空 name → 丢弃
      { action: 'remove', name: 'User-Agent', value: '' }
    ],
    body: [
      { action: 'override', path: 'thinking', value: '{"type":"adaptive"}' },
      { action: 'override', path: 'bad', value: 'not json' }, // 非法 JSON → 丢弃
      { action: 'remove', path: '  ', value: '' } // 空 path → 丢弃
    ]
  });
  const payload = collectCustomRulesForSubmit();
  assert.ok(payload);
  assert.equal(payload.headers.length, 1);
  assert.equal(payload.headers[0].name, 'User-Agent');
  assert.ok(!('value' in payload.headers[0]), 'remove 头不应包含 value');
  assert.equal(payload.body.length, 1);
  assert.deepEqual(payload.body[0].value, { type: 'adaptive' });
});

test('collectCustomRulesForSubmit 保留 override 头值（空字符串也允许）', () => {
  resetCustomRulesState({
    headers: [{ action: 'override', name: 'X-Blank', value: '' }],
    body: []
  });
  const payload = collectCustomRulesForSubmit();
  assert.ok(payload);
  assert.equal(payload.headers[0].value, '');
});

test('collectCustomRulesForSubmit remove 头带值表示 token 精确移除', () => {
  resetCustomRulesState({
    headers: [
      { action: 'remove', name: 'Anthropic-Beta', value: 'context-1m-2025-08-07' },
      { action: 'remove', name: 'User-Agent', value: '' }
    ],
    body: []
  });
  const payload = collectCustomRulesForSubmit();
  assert.ok(payload);
  assert.equal(payload.headers.length, 2);
  assert.equal(payload.headers[0].name, 'Anthropic-Beta');
  assert.equal(payload.headers[0].value, 'context-1m-2025-08-07');
  assert.equal(payload.headers[1].name, 'User-Agent');
  assert.ok(!('value' in payload.headers[1]), 'remove + 空值不应包含 value');
});

test('高级设置确定会等待超额设置保存，失败时保持对话框打开', async () => {
  const modulePath = require.resolve('./channels-custom-rules.js');
  const cachedModule = require.cache[modulePath];
  const previousDocument = global.document;
  const previousWindow = global.window;
  let modalClosed = 0;
  let shownError = '';
  const confirmButton = {
    disabled: false,
    attributes: new Set(),
    setAttribute(name) { this.attributes.add(name); },
    removeAttribute(name) { this.attributes.delete(name); }
  };
  const modal = { classList: { remove() { modalClosed++; } } };

  delete require.cache[modulePath];
  global.document = {
    readyState: 'complete',
    getElementById: id => id === 'customRulesModal' ? modal : null,
    querySelector: selector => selector === '[data-action="apply-advanced-settings"]' ? confirmButton : null,
    querySelectorAll: () => []
  };
  global.window = {
    t: key => key,
    saveCodexQuotaOverdraftFromAdvancedSettings: async () => {},
    showError: message => { shownError = message; }
  };
  try {
    const browserModule = require('./channels-custom-rules.js');
    assert.equal(await browserModule.applyAdvancedSettingsFromForm(), true);
    assert.equal(modalClosed, 1);
    assert.equal(confirmButton.disabled, false);
    assert.equal(confirmButton.attributes.has('aria-busy'), false);

    global.window.saveCodexQuotaOverdraftFromAdvancedSettings = async () => {
      throw new Error('credential write failed');
    };
    assert.equal(await browserModule.applyAdvancedSettingsFromForm(), false);
    assert.equal(modalClosed, 1);
    assert.equal(shownError, 'credential write failed');
    assert.equal(confirmButton.disabled, false);
  } finally {
    delete require.cache[modulePath];
    if (cachedModule) require.cache[modulePath] = cachedModule;
    global.document = previousDocument;
    global.window = previousWindow;
  }
});

test('高级设置确定会校验并提交管理账户草稿，校验失败时停留在“其他”页', async () => {
  const modulePath = require.resolve('./channels-custom-rules.js');
  const cachedModule = require.cache[modulePath];
  const previousDocument = global.document;
  const previousWindow = global.window;
  let modalClosed = 0;
  let committed = 0;
  let managementValid = false;
  const confirmButton = {
    disabled: false,
    setAttribute() {},
    removeAttribute() {}
  };
  const modal = { classList: { remove() { modalClosed++; } } };
  const otherPanel = {
    classList: {
      hidden: true,
      toggle(name, force) { if (name === 'hidden') this.hidden = Boolean(force); },
      contains(name) { return name === 'hidden' && this.hidden; }
    }
  };

  delete require.cache[modulePath];
  global.document = {
    readyState: 'complete',
    getElementById: id => {
      if (id === 'customRulesModal') return modal;
      if (id === 'advancedSettingsPanelOther') return otherPanel;
      return null;
    },
    querySelector: selector => selector === '[data-action="apply-advanced-settings"]' ? confirmButton : null,
    querySelectorAll: () => []
  };
  global.window = {
    t: key => key,
    validateManagementAccountDraft: () => managementValid,
    commitManagementAccountDraft: () => {
      if (!managementValid) return false;
      committed++;
      return true;
    }
  };
  try {
    const browserModule = require('./channels-custom-rules.js');

    assert.equal(await browserModule.applyAdvancedSettingsFromForm(), false);
    assert.equal(modalClosed, 0, '管理账户草稿非法时对话框保持打开');
    assert.equal(committed, 0);
    assert.equal(otherPanel.classList.hidden, false, '切换到“其他”页展示错误');

    managementValid = true;
    assert.equal(await browserModule.applyAdvancedSettingsFromForm(), true);
    assert.equal(committed, 1);
    assert.equal(modalClosed, 1);
  } finally {
    delete require.cache[modulePath];
    if (cachedModule) require.cache[modulePath] = cachedModule;
    global.document = previousDocument;
    global.window = previousWindow;
  }
});


test('关闭高级设置会把焦点交还给打开它的按钮', () => {
  const modulePath = require.resolve('./channels-custom-rules.js');
  const cachedModule = require.cache[modulePath];
  const previousDocument = global.document;
  const previousWindow = global.window;
  const classNames = new Set();
  const modal = {
    classList: {
      add(name) { classNames.add(name); },
      remove(name) { classNames.delete(name); },
      contains(name) { return classNames.has(name); }
    }
  };
  const body = { tagName: 'BODY' };
  let openerFocused = 0;
  const opener = { isConnected: true, focus() { openerFocused++; } };
  const insideModal = { isConnected: true, focus() {} };

  delete require.cache[modulePath];
  global.document = {
    readyState: 'complete',
    body,
    activeElement: opener,
    getElementById: id => (id === 'customRulesModal' ? modal : null),
    querySelector: () => null,
    querySelectorAll: () => []
  };
  global.window = { t: key => key };
  try {
    require('./channels-custom-rules.js');
    const { openCustomRulesModal, closeCustomRulesModal } = global.window;

    openCustomRulesModal();
    assert.equal(modal.classList.contains('show'), true);

    // 焦点在弹窗内部时关闭，必须回到触发按钮而不是留在 <body>。
    global.document.activeElement = insideModal;
    closeCustomRulesModal();
    assert.equal(modal.classList.contains('show'), false);
    assert.equal(openerFocused, 1);

    // 再次关闭不应重复抢焦点。
    closeCustomRulesModal();
    assert.equal(openerFocused, 1);

    // 无有效触发元素(焦点在 body)时关闭不得抛错、不得把焦点丢给 body。
    global.document.activeElement = body;
    openCustomRulesModal();
    closeCustomRulesModal();
    assert.equal(openerFocused, 1);
  } finally {
    delete require.cache[modulePath];
    if (cachedModule) require.cache[modulePath] = cachedModule;
    global.document = previousDocument;
    global.window = previousWindow;
  }
});