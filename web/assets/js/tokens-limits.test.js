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

test('create and edit forms submit RPM limits and reject invalid limits before a request', async () => {
  const elements = new Map();
  const makeElement = () => ({
    value: '', checked: false, innerHTML: '', style: {}, dataset: {},
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
    addEventListener() {}, setAttribute() {}, removeAttribute() {}, focus() {}
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
  const window = {
    t: key => key,
    i18n: { onLocaleChange() {} },
    initPageBootstrap(config) { bootstrap = config.run; },
    initSavedDateRangeFilter() {},
    initDelegatedActions(config) { actions = config.click; },
    guardUnsavedChanges() { return () => {}; },
    showNotification(message) { notifications.push(message); }
  };
  const fetchDataWithAuth = async (url, options = {}) => {
    requests.push({ url, options });
    return { token: 'new-token', tokens: [] };
  };
  const globals = { window, document, fetchDataWithAuth, TokenExpiry: require('./token-expiry.js') };
  const previous = new Map(Object.keys(globals).map(key => [key, global[key]]));
  const modulePath = require.resolve('./tokens.js');
  delete require.cache[modulePath];
  Object.assign(global, globals);
  try {
    require(modulePath);
    bootstrap();
    await new Promise(resolve => setImmediate(resolve));
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
    await new Promise(resolve => setImmediate(resolve));
  } finally {
    delete require.cache[modulePath];
    for (const [key, value] of previous) {
      if (value === undefined) delete global[key];
      else global[key] = value;
    }
  }
});
