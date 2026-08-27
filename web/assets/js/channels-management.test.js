const test = require('node:test');
const assert = require('node:assert/strict');

const MANAGEMENT_ELEMENT_IDS = [
  'channelManagementGroup',
  'channelManagementProfile',
  'channelManagementNotice',
  'channelManagementBaseURLField',
  'channelManagementBaseURL',
  'channelManagementBaseURLError',
  'channelManagementTokenField',
  'channelManagementUserIDHint',
  'channelManagementToken',
  'channelManagementTokenHelp',
  'channelManagementTokenHint',
  'channelManagementTokenError',
  'channelManagementUserIDField',
  'channelManagementUserID',
  'channelManagementUserIDError',
  'channelManagementCheckinField',
  'channelManagementDailyCheckinEnabled',
  'channelManagementDailyCheckinTime',
  'channelManagementDailyCheckinTimeError'
];

function createStubElement(id) {
  const attributes = new Map();
  const classes = new Set();
  return {
    id,
    value: '',
    checked: false,
    hidden: false,
    disabled: false,
    textContent: '',
    dataset: {},
    focusCount: 0,
    listeners: new Map(),
    classList: {
      add: (...names) => names.forEach(name => classes.add(name)),
      remove: (...names) => names.forEach(name => classes.delete(name)),
      contains: name => classes.has(name),
      toggle: (name, force) => {
        const next = force === undefined ? !classes.has(name) : Boolean(force);
        if (next) classes.add(name); else classes.delete(name);
        return next;
      }
    },
    setAttribute(name, value) { attributes.set(name, String(value)); },
    getAttribute(name) { return attributes.has(name) ? attributes.get(name) : null; },
    removeAttribute(name) { attributes.delete(name); },
    hasAttribute(name) { return attributes.has(name); },
    addEventListener(type, handler) { this.listeners.set(type, handler); },
    focus() { this.focusCount += 1; }
  };
}

function installManagementDOM() {
  const elements = new Map(MANAGEMENT_ELEMENT_IDS.map(id => [id, createStubElement(id)]));
  const previous = new Map();
  const dirtyCalls = { count: 0 };
  const globals = {
    document: {
      readyState: 'complete',
      getElementById: id => elements.get(id) || null
    },
    window: {
      t: (key, values) => (values
        ? Object.entries(values).reduce((text, [name, value]) => text.replace(`{${name}}`, String(value)), key)
        : key),
      markChannelFormDirty: () => { dirtyCalls.count += 1; }
    }
  };
  for (const [name, value] of Object.entries(globals)) {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  }
  return {
    elements,
    dirtyCalls,
    el: id => elements.get(id),
    restore() {
      for (const [name, descriptor] of previous) {
        if (descriptor) Object.defineProperty(global, name, descriptor);
        else delete global[name];
      }
    }
  };
}

function loadManagementModule() {
  const modulePath = require.resolve('./channels-management.js');
  delete require.cache[modulePath];
  return require(modulePath);
}

function selectProfile(dom, mod, profile) {
  const select = dom.el('channelManagementProfile');
  select.value = profile;
  const changeHandler = select.listeners.get('change');
  if (changeHandler) changeHandler();
  else mod.renderManagementAccountFields(mod.readManagementAccountForm());
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

test('管理账户表单按 profile 显示字段矩阵并标注平台限制', () => {
  const dom = installManagementDOM();
  try {
    const mod = loadManagementModule();

    mod.resetManagementAccountDraft(null, [], 'codex_oauth');
    assert.equal(dom.el('channelManagementGroup').hidden, true, 'OAuth 渠道不显示管理账户组');

    mod.resetManagementAccountDraft(null, ['https://panel.example.com'], 'api_key');
    assert.equal(dom.el('channelManagementGroup').hidden, false);
    assert.equal(dom.el('channelManagementProfile').value, '');
    assert.equal(dom.el('channelManagementBaseURLField').hidden, true);
    assert.equal(dom.el('channelManagementTokenField').hidden, true);
    assert.equal(dom.el('channelManagementUserIDField').hidden, true);
    assert.equal(dom.el('channelManagementCheckinField').hidden, true);
    assert.equal(dom.el('channelManagementNotice').hidden, true);

    selectProfile(dom, mod, 'new_api');
    assert.equal(dom.el('channelManagementBaseURLField').hidden, false);
    assert.equal(dom.el('channelManagementTokenField').hidden, false);
    assert.equal(dom.el('channelManagementUserIDField').hidden, false);
    assert.equal(dom.el('channelManagementCheckinField').hidden, false);
    assert.equal(dom.el('channelManagementNotice').hidden, true);
    assert.equal(dom.el('channelManagementTokenHelp').textContent, 'channels.management.tokenHelpNewAPI');

    selectProfile(dom, mod, 'sub2api');
    assert.equal(dom.el('channelManagementUserIDField').hidden, true, '标准 Sub2API 不接受 user_id');
    assert.equal(dom.el('channelManagementCheckinField').hidden, true, '标准 Sub2API 不显示签到配置');
    assert.equal(dom.el('channelManagementNotice').hidden, false);
    assert.equal(dom.el('channelManagementNotice').textContent, 'channels.management.noticeSub2API');
    assert.equal(dom.el('channelManagementTokenHelp').textContent, 'channels.management.tokenHelpSub2API');

    selectProfile(dom, mod, 'sub2api_pro');
    assert.equal(dom.el('channelManagementUserIDField').hidden, true);
    assert.equal(dom.el('channelManagementCheckinField').hidden, false);
    assert.equal(dom.el('channelManagementNotice').textContent, 'channels.management.noticeSub2APIPro');
  } finally {
    dom.restore();
  }
});

test('首个渠道 URL 只作为初始默认，显式面板地址不被多 URL 覆盖', () => {
  const dom = installManagementDOM();
  try {
    const mod = loadManagementModule();

    mod.resetManagementAccountDraft(null, [
      { url: 'https://first.example.com/v1/messages', exact: true },
      { url: 'https://second.example.com' }
    ], 'api_key');
    assert.equal(dom.el('channelManagementBaseURL').value, 'https://first.example.com');

    mod.resetManagementAccountDraft({
      profile: 'new_api',
      base_url: 'https://panel.example.com',
      credential_configured: true
    }, [
      { url: 'https://first.example.com/v1/messages' },
      { url: 'https://second.example.com' }
    ], 'api_key');
    assert.equal(dom.el('channelManagementBaseURL').value, 'https://panel.example.com');
  } finally {
    dom.restore();
  }
});

test('管理凭据和用户 ID 会回填，关闭 profile 表示清除', () => {
  const dom = installManagementDOM();
  try {
    const mod = loadManagementModule();

    dom.el('channelManagementUserIDHint').hidden = true;
    mod.resetManagementAccountDraft({
      profile: 'new_api',
      base_url: 'https://panel.example.com',
      access_token: 'saved-token',
      user_id: 41,
      user_id_configured: true,
      daily_checkin_enabled: true,
      daily_checkin_time: '09:30',
      credential_configured: true,
      last_checkin_status: 'success'
    }, ['https://upstream.example.com'], 'api_key');

    assert.equal(dom.el('channelManagementToken').value, 'saved-token', '已保存 token 应回填输入框');
    assert.equal(dom.el('channelManagementUserID').value, '41', '已保存 user_id 应回填输入框');
    assert.equal(dom.el('channelManagementTokenHint').hidden, false);
    assert.equal(dom.el('channelManagementDailyCheckinEnabled').checked, true);
    assert.equal(dom.el('channelManagementDailyCheckinTime').value, '09:30');
    assert.equal(
      dom.el('channelManagementUserIDHint').hidden,
      false,
      'DTO 标记已配置 user_id 时应显示提示'
    );

    assert.equal(mod.commitManagementAccountDraft(), true);
    assert.equal(dom.dirtyCalls.count, 1, '提交管理草稿后必须标记渠道编辑器为未保存');
    assert.deepEqual(mod.collectManagementAccountForSubmit(), {
      profile: 'new_api',
      base_url: 'https://panel.example.com',
      access_token: 'saved-token',
      user_id: 41,
      daily_checkin_enabled: true,
      daily_checkin_time: '09:30'
    });

    dom.el('channelManagementToken').value = ' fresh-pat ';
    dom.el('channelManagementUserID').value = '42';
    assert.equal(mod.commitManagementAccountDraft(), true);
    assert.deepEqual(mod.collectManagementAccountForSubmit(), {
      profile: 'new_api',
      base_url: 'https://panel.example.com',
      access_token: 'fresh-pat',
      user_id: 42,
      daily_checkin_enabled: true,
      daily_checkin_time: '09:30'
    });

    selectProfile(dom, mod, 'sub2api');
    dom.el('channelManagementToken').value = 'jwt-token';
    assert.equal(mod.commitManagementAccountDraft(), true);
    assert.deepEqual(mod.collectManagementAccountForSubmit(), {
      profile: 'sub2api',
      base_url: 'https://panel.example.com',
      access_token: 'jwt-token'
    }, '标准 Sub2API 不提交 user_id 与签到字段');

    selectProfile(dom, mod, '');
    assert.equal(mod.commitManagementAccountDraft(), true);
    assert.deepEqual(mod.collectManagementAccountForSubmit(), { profile: '' }, '关闭表示清除');

    mod.resetManagementAccountDraft(null, [], 'codex_oauth');
    assert.equal(mod.collectManagementAccountForSubmit(), null, 'OAuth 渠道不提交 management_account');
  } finally {
    dom.restore();
  }
});

test('草稿校验为缺失与非法输入设置 aria-invalid 并展示关联错误', () => {
  const dom = installManagementDOM();
  try {
    const mod = loadManagementModule();
    mod.resetManagementAccountDraft(null, [], 'api_key');
    selectProfile(dom, mod, 'new_api');

    assert.equal(mod.validateManagementAccountDraft(), false);
    assert.equal(dom.el('channelManagementBaseURL').getAttribute('aria-invalid'), 'true');
    assert.equal(dom.el('channelManagementBaseURLError').hidden, false);
    assert.equal(dom.el('channelManagementBaseURLError').textContent, 'channels.management.errBaseURLRequired');
    assert.equal(dom.el('channelManagementBaseURL').focusCount, 1, '焦点移到首个非法控件');
    assert.equal(dom.el('channelManagementToken').getAttribute('aria-invalid'), 'true');
    assert.equal(dom.el('channelManagementTokenError').textContent, 'channels.management.errTokenRequired');
    assert.equal(mod.commitManagementAccountDraft(), false);

    dom.el('channelManagementBaseURL').value = 'https://panel.example.com/api/v1';
    dom.el('channelManagementToken').value = 'pat';
    assert.equal(mod.validateManagementAccountDraft(), false);
    assert.equal(dom.el('channelManagementBaseURLError').textContent, 'channels.management.errBaseURLInvalid');
    assert.equal(dom.el('channelManagementToken').getAttribute('aria-invalid'), null);
    assert.equal(dom.el('channelManagementTokenError').hidden, true);

    dom.el('channelManagementBaseURL').value = 'https://panel.example.com';
    dom.el('channelManagementUserID').value = '0';
    assert.equal(mod.validateManagementAccountDraft(), false);
    assert.equal(dom.el('channelManagementUserID').getAttribute('aria-invalid'), 'true');
    assert.equal(dom.el('channelManagementUserIDError').textContent, 'channels.management.errUserID');

    dom.el('channelManagementUserID').value = '7';
    dom.el('channelManagementDailyCheckinEnabled').checked = true;
    dom.el('channelManagementDailyCheckinTime').value = '';
    assert.equal(mod.validateManagementAccountDraft(), false);
    assert.equal(dom.el('channelManagementDailyCheckinTime').getAttribute('aria-invalid'), 'true');
    assert.equal(dom.el('channelManagementDailyCheckinTimeError').textContent, 'channels.management.errCheckinTime');

    dom.el('channelManagementDailyCheckinTime').value = '7:5';
    assert.equal(mod.validateManagementAccountDraft(), false);

    dom.el('channelManagementDailyCheckinTime').value = '07:05';
    assert.equal(mod.validateManagementAccountDraft(), true);
    assert.equal(dom.el('channelManagementBaseURL').getAttribute('aria-invalid'), null);
    assert.equal(dom.el('channelManagementDailyCheckinTimeError').hidden, true);
  } finally {
    dom.restore();
  }
});

test('切换到未保存的 profile 后必须重新输入凭据', () => {
  const dom = installManagementDOM();
  try {
    const mod = loadManagementModule();
    mod.resetManagementAccountDraft({
      profile: 'new_api',
      base_url: 'https://panel.example.com',
      access_token: 'saved-token',
      credential_configured: true
    }, [], 'api_key');

    selectProfile(dom, mod, 'sub2api_pro');
    assert.equal(dom.el('channelManagementToken').value, '', '切换 profile 后不得沿用原凭据');
    assert.equal(mod.validateManagementAccountDraft(), false);
    assert.equal(dom.el('channelManagementToken').getAttribute('aria-invalid'), 'true');
    assert.equal(dom.el('channelManagementTokenHint').hidden, true, '换 profile 后不再提示保留旧凭据');
  } finally {
    dom.restore();
  }
});

test('额度刷新与签到使用独立 operation 序列，陈旧响应不覆盖新响应', async () => {
  const dom = installManagementDOM();
  try {
    const mod = loadManagementModule();
    const first = deferred();
    const second = deferred();
    const calls = [];
    const balanceFetcher = url => {
      calls.push(url);
      return calls.length === 1 ? first.promise : second.promise;
    };

    const firstCall = mod.refreshManagementBalance(7, balanceFetcher, { reload: false });
    assert.equal(mod.getManagementBalanceState(7).status, 'loading');
    assert.equal(mod.getManagementCheckinState(7), null, '余额 loading 不影响签到状态');

    const secondCall = mod.refreshManagementBalance(7, balanceFetcher, { reload: false });
    second.resolve({ profile: 'new_api', balance: { remaining: 12.5, unit: 'USD', sampled_at: '2026-08-25T10:00:00Z' } });
    await secondCall;
    assert.equal(mod.getManagementBalanceState(7).data.balance.remaining, 12.5);

    first.resolve({ profile: 'new_api', balance: { remaining: 999, unit: 'USD', sampled_at: '2026-08-25T09:00:00Z' } });
    await firstCall;
    assert.equal(mod.getManagementBalanceState(7).data.balance.remaining, 12.5, '陈旧响应不能覆盖新响应');
    assert.deepEqual(calls, [
      '/admin/channels/7/management-account/balance',
      '/admin/channels/7/management-account/balance'
    ]);

    const checkinDeferred = deferred();
    const checkinCall = mod.runManagementCheckin(7, url => {
      calls.push(url);
      return checkinDeferred.promise;
    }, { reload: false });
    assert.equal(mod.getManagementCheckinState(7).status, 'loading');
    assert.equal(mod.getManagementBalanceState(7).status, 'ready', '签到 loading 不重置余额状态');
    checkinDeferred.resolve({
      status: 'already_checked',
      status_code: 200,
      balance: { remaining: 20, unit: 'USD', sampled_at: '2026-08-25T11:00:00Z' }
    });
    await checkinCall;
    assert.equal(mod.getManagementCheckinState(7).data.status, 'already_checked');
    assert.equal(mod.getManagementBalanceState(7).data.balance.remaining, 20, '签到返回的余额刷新展示');
    assert.equal(calls[2], '/admin/channels/7/management-account/checkin');
  } finally {
    dom.restore();
  }
});

test('额度失败与签到失败各自独立记录错误，不互相清空', async () => {
  const dom = installManagementDOM();
  try {
    const mod = loadManagementModule();

    await assert.rejects(
      () => mod.refreshManagementBalance(9, async () => { throw new Error('upstream 502'); }, { reload: false }),
      /upstream 502/
    );
    assert.deepEqual(mod.getManagementBalanceState(9), { status: 'error', error: 'upstream 502' });
    assert.equal(mod.getManagementCheckinState(9), null);

    await assert.rejects(
      () => mod.runManagementCheckin(9, async () => ({ status_code: 200 }), { reload: false }),
      /channels.management.checkinInvalid/
    );
    assert.equal(mod.getManagementCheckinState(9).status, 'error');
    assert.equal(mod.getManagementBalanceState(9).error, 'upstream 502', '签到失败不清空余额错误');

    await assert.rejects(
      () => mod.refreshManagementBalance(9, async () => ({ profile: 'new_api' }), { reload: false }),
      /channels.management.balanceInvalid/
    );
    assert.equal(mod.getManagementBalanceState(9).error, 'channels.management.balanceInvalid');
  } finally {
    dom.restore();
  }
});
