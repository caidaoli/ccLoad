const test = require('node:test');
const assert = require('node:assert/strict');
const { buildRpmLimitPayload } = require('./tokens.js');

test('RPM payload distinguishes omitted limits from explicit disabling', () => {
  assert.deepEqual(buildRpmLimitPayload(), {});
  assert.deepEqual(buildRpmLimitPayload(null), {});
  assert.deepEqual(buildRpmLimitPayload(''), { rpm_limit: 0 });
  assert.deepEqual(buildRpmLimitPayload('0'), { rpm_limit: 0 });
  assert.deepEqual(buildRpmLimitPayload(' 60 '), { rpm_limit: 60 });
  assert.equal(JSON.parse(JSON.stringify(buildRpmLimitPayload('0'))).rpm_limit, 0);
});

test('RPM payload rejects negative, fractional and non-finite limits', () => {
  for (const value of ['-1', '0.5', 'NaN', 'Infinity', 'abc', '9007199254740992']) {
    assert.throws(() => buildRpmLimitPayload(value), RangeError);
  }
});

/**
 * @brief 在假 DOM 中加载令牌页并执行页面交互
 * @param respond 按请求 URL 返回模拟响应
 * @param run 接收 { actions, changes, document, requests, notifications } 的交互脚本；
 *            元素的 listeners 记录 addEventListener 注册的处理器
 */
async function withTokensPage(respond, run) {
  const elements = new Map();
  const makeElement = () => ({
    value: '', checked: false, innerHTML: '', style: {}, dataset: {}, listeners: {},
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
    addEventListener(type, listener) { this.listeners[type] = listener; },
    setAttribute() {}, removeAttribute() {}, focus() {}
  });
  const document = {
    addEventListener() {}, querySelectorAll() { return []; },
    getElementById(id) {
      if (id === 'tpl-token-expiry-options') return null;
      if (!elements.has(id)) elements.set(id, makeElement());
      return elements.get(id);
    }
  };
  const requests = [];
  const notifications = [];
  let bootstrap;
  let actions;
  let changes;
  const window = {
    t: key => key,
    i18n: { onLocaleChange() {} },
    initPageBootstrap(config) { bootstrap = config.run; },
    initSavedDateRangeFilter() {},
    initDelegatedActions(config) { actions = config.click; changes = config.change; },
    guardUnsavedChanges() { return () => {}; },
    showNotification(message) { notifications.push(message); },
    formatCost: String
  };
  const fetchDataWithAuth = async (url, options = {}) => {
    requests.push({ url, options });
    return respond(url);
  };
  const globals = { window, document, fetchDataWithAuth, escapeHtml: String, TokenExpiry: require('./token-expiry.js') };
  const previous = new Map(Object.keys(globals).map(key => [key, global[key]]));
  const modulePath = require.resolve('./tokens.js');
  delete require.cache[modulePath];
  Object.assign(global, globals);
  try {
    require(modulePath);
    bootstrap();
    await new Promise(resolve => setImmediate(resolve));
    await run({ actions, changes, document, requests, notifications });
    await new Promise(resolve => setImmediate(resolve));
  } finally {
    delete require.cache[modulePath];
    for (const [key, value] of previous) {
      if (value === undefined) delete global[key];
      else global[key] = value;
    }
  }
}

test('create and edit forms submit RPM limits and reject invalid limits before a request', async () => {
  await withTokensPage(() => ({ token: 'new-token', tokens: [] }), async ({ actions, document, requests, notifications }) => {
    actions['show-create-modal']();
    document.getElementById('tokenDescription').value = 'test token';
    document.getElementById('tokenRPMLimit').value = '30';
    await actions['create-token']();
    const create = requests.find(request => request.options.method === 'POST');
    assert.equal(create.url, '/admin/auth-tokens');
    assert.equal(JSON.parse(create.options.body).rpm_limit, 30);

    document.getElementById('editTokenId').value = '7';
    document.getElementById('editTokenDescription').value = 'edited token';
    document.getElementById('editTokenExpiry').value = 'never';
    document.getElementById('editRPMLimit').value = '0';
    await actions['update-token']();
    const update = requests.find(request => request.options.method === 'PUT');
    assert.equal(update.url, '/admin/auth-tokens/7');
    assert.equal(JSON.parse(update.options.body).rpm_limit, 0);

    const submitted = requests.filter(request => ['POST', 'PUT'].includes(request.options.method)).length;
    document.getElementById('tokenRPMLimit').value = '-1';
    await actions['create-token']();
    document.getElementById('editRPMLimit').value = '1.5';
    await actions['update-token']();
    assert.equal(requests.filter(request => ['POST', 'PUT'].includes(request.options.method)).length, submitted);
    assert.ok(notifications.includes('tokens.msg.rpmLimitInteger'));
  });
});

test('channel and model pickers offer only enabled channels and models', async () => {
  const channels = [
    { id: 1, name: 'live', enabled: true, models: [{ model: 'live-model' }, { model: 'paused-model', disabled: true }] },
    { id: 2, name: 'off', enabled: false, models: [{ model: 'off-model' }] }
  ];
  const respond = url => (url === '/admin/channels' ? channels : { tokens: [] });
  const pickAll = async ({ actions, changes }, kind) => {
    await actions[`show-${kind}-select-modal`]();
    changes[`toggle-select-all-${kind}s`]({ checked: true });
    actions[`confirm-${kind}-selection`]();
  };
  const update = async (page) => {
    const { actions, document, requests } = page;
    document.getElementById('editTokenId').value = '7';
    document.getElementById('editTokenExpiry').value = 'never';
    await actions['update-token']();
    return JSON.parse(requests.filter(request => request.options.method === 'PUT').at(-1).options.body);
  };

  // Without a channel restriction the model list spans every enabled channel.
  await withTokensPage(respond, async (page) => {
    await pickAll(page, 'model');
    assert.deepEqual((await update(page)).allowed_models, ['live-model']);
  });
  // The channel picker hides disabled channels.
  await withTokensPage(respond, async (page) => {
    await pickAll(page, 'channel');
    assert.deepEqual((await update(page)).allowed_channel_ids, [1]);
  });
  // Saved restrictions on disabled channels and models stay listed and are kept on save.
  const saved = { id: 7, description: 'saved', allowed_models: ['paused-model', 'off-model'], allowed_channel_ids: [2] };
  const respondSaved = url => (url === '/admin/channels' ? channels : { tokens: [saved] });
  await withTokensPage(respondSaved, async (page) => {
    const { document } = page;
    const editButton = { classList: { contains: name => name === 'btn-edit' }, closest: () => ({ dataset: { tokenId: '7' } }) };
    document.getElementById('tokens-container').listeners.click({ target: { closest: () => editButton } });
    assert.match(document.getElementById('allowedModelsList').innerHTML, /paused-model[\s\S]*off-model/);
    assert.match(document.getElementById('allowedChannelsList').innerHTML, /off/);
    await pickAll(page, 'channel');
    await pickAll(page, 'model');
    const body = await update(page);
    assert.deepEqual(body.allowed_models, ['live-model', 'off-model', 'paused-model']);
    assert.deepEqual(body.allowed_channel_ids, [1, 2]);
  });
});
