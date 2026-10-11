const test = require('node:test');
const assert = require('node:assert/strict');
const { buildRequestPayload } = require('./model-test-route-preview.js');

test('route preview payload carries only the ordinary generation contract', () => {
  assert.deepEqual(buildRequestPayload({ model: ' claude-test ', clientProtocol: 'anthropic', authTokenID: '', multimodal: false }), {
    model: 'claude-test', client_protocol: 'anthropic', multimodal: false
  });
  assert.deepEqual(buildRequestPayload({ model: 'test', clientProtocol: 'codex', authTokenID: '7', multimodal: true }), {
    model: 'test', client_protocol: 'codex', multimodal: true, auth_token_id: 7
  });
  for (const values of [
    { model: '', clientProtocol: 'openai' },
    { model: 'test', clientProtocol: 'unsupported' },
    { model: 'test', clientProtocol: 'gemini', authTokenID: '-1' }
  ]) assert.throws(() => buildRequestPayload(values));
});

class Element {
  constructor(tag = 'div') {
    this.tagName = tag.toUpperCase();
    this.children = [];
    this.listeners = new Map();
    this.attributes = new Map();
    this.value = '';
    this.disabled = false;
    this.checked = false;
    this.content = '';
  }
  set textContent(value) { this.content = String(value); this.children = []; }
  get textContent() { return this.content + this.children.map(child => child.textContent).join(''); }
  set innerHTML(value) { throw new Error('Preview must render untrusted values as text'); }
  appendChild(child) { this.children.push(child); return child; }
  replaceChildren(...children) { this.children = children; this.content = ''; }
  setAttribute(name, value) { this.attributes.set(name, value); }
  addEventListener(name, fn) { this.listeners.set(name, fn); }
  dispatch(name, event) { return this.listeners.get(name)?.(event); }
  focus() { this.focused = true; }
  get options() { return this.children.filter(child => child.tagName === 'OPTION'); }
}

function setup(fetchDataWithAuth, getModelOptions = () => ['test-model', 'denied-model']) {
  const elements = new Map();
  const ids = ['routePreviewPanel', 'routePreviewProtocol', 'routePreviewToken', 'routePreviewModel',
    'routePreviewMultimodal', 'routePreviewForm', 'routePreviewRun', 'routePreviewStatus', 'routePreviewResults'];
  ids.forEach(id => elements.set(id, new Element(id.includes('Token') || id.includes('Protocol') || id.includes('Model') ? 'select' : 'div')));
  let localeCallback;
  const enhanced = [];
  const openedChannels = [];
  const fakeWindow = {
    document: { getElementById: id => elements.get(id), createElement: tag => new Element(tag) },
    fetchDataWithAuth,
    openLogChannelEditor: id => openedChannels.push(id),
    enhanceNativeSelects: scope => enhanced.push(scope),
    i18n: { onLocaleChange(callback) { localeCallback = callback; } }
  };
  const modulePath = require.resolve('./model-test-route-preview.js');
  const previousWindow = global.window;
  delete require.cache[modulePath];
  global.window = fakeWindow;
  let api;
  try { api = require(modulePath); } finally {
    delete require.cache[modulePath];
    if (previousWindow === undefined) delete global.window;
    else global.window = previousWindow;
  }
  const translate = (key, fallback, params = {}) => Object.entries(params).reduce((value, [name, replacement]) => value.replace(`{${name}}`, replacement), fallback);
  api.init({ t: translate, protocolLabel: protocol => protocol.toUpperCase(), getModelOptions });
  elements.get('routePreviewProtocol').value = 'codex';
  return { api, elements, enhanced, openedChannels, changeLocale: () => localeCallback() };
}

function descendants(node, tag) {
  return node.children.flatMap(child => [ ...(child.tagName === tag.toUpperCase() ? [child] : []), ...descendants(child, tag) ]);
}

test('preview form uses token selection, submits once, and renders untrusted responses as text', async () => {
  const requests = [];
  let resolvePreview;
  const response = {
    model: '<img src=x onerror=alert(1)>', model_denied: false,
    candidates: [
      { id: 5, name: '<script>first</script>', priority: 10, effective_priority: 9.09, priority_group: 9, available_key_count: 2, same_tier_rotation: true },
      { id: 6, name: 'second', priority: 10, effective_priority: 9.01, priority_group: 9, available_key_count: 1, same_tier_rotation: true },
      { id: 7, name: 'fallback', priority: 8, effective_priority: 8, priority_group: 8, available_key_count: 0, cooldown_fallback: true }
    ],
    excluded: [{ id: 8, name: '<svg onload=alert(1)>', priority: 3, reason: 'channel_cooldown', until: '2026-10-11T12:00:00Z' }],
    limitations: ['live_rpm_concurrency', '<script>unknown</script>']
  };
  const { api, elements, enhanced, openedChannels, changeLocale } = setup(async (url, options) => {
    requests.push({ url, options });
    if (url === '/admin/auth-tokens') return { tokens: [
      { id: 7, description: '<b>token</b>', is_active: true },
      { id: 8, description: 'expired', is_active: true, is_expired: true }
    ] };
    return new Promise(resolve => { resolvePreview = resolve; });
  });
  assert.equal(enhanced.length, 1);
  await api.open();
  const tokens = elements.get('routePreviewToken');
  assert.equal(tokens.options[1].textContent, '<b>token</b> (#7)');
  assert.equal(tokens.options[2].disabled, true);
  tokens.value = '7';
  elements.get('routePreviewModel').value = 'test-model';
  elements.get('routePreviewMultimodal').checked = true;
  let prevented = false;
  const running = elements.get('routePreviewForm').dispatch('submit', { preventDefault() { prevented = true; } });
  assert.equal(prevented, true);
  assert.equal(elements.get('routePreviewRun').disabled, true);
  await api.run();
  assert.equal(requests.filter(request => request.url === '/admin/route-preview').length, 1);
  assert.equal(requests[1].options.method, 'POST');
  assert.deepEqual(JSON.parse(requests[1].options.body), {
    model: 'test-model', client_protocol: 'codex', auth_token_id: 7, multimodal: true
  });
  resolvePreview(response);
  await running;
  assert.equal(elements.get('routePreviewRun').disabled, false);
  const results = elements.get('routePreviewResults');
  assert.ok(!results.textContent.includes(response.model));
  assert.ok(results.textContent.includes(response.excluded[0].name));
  assert.ok(results.textContent.includes(response.limitations[1]));
  const rows = descendants(results, 'tbody').flatMap(body => body.children);
  const links = rows.map(row => descendants(row.children[0], 'button')[0]);
  assert.deepEqual(links.map(link => link.textContent), ['<script>first</script>', 'second', 'fallback', '<svg onload=alert(1)>']);
  links[0].dispatch('click');
  links[3].dispatch('click');
  assert.deepEqual(openedChannels, [5, 8]);
  // 静态优先级始终显示；动态优先级仅在偏离静态值时追加
  assert.deepEqual(rows.map(row => row.children[1].textContent), ['109.1', '109', '8', '3']);
  assert.equal(descendants(results, 'script').length, 0);
  assert.equal(descendants(results, 'img').length, 0);
  assert.equal(descendants(results, 'svg').length, 0);
  changeLocale();
  assert.equal(descendants(results, 'tbody').flatMap(body => body.children).length, 4);
});

test('model denial is a successful preview and failed requests restore submission', async () => {
  let shouldFail = false;
  const { api, elements } = setup(async url => {
    if (url === '/admin/auth-tokens') return { tokens: [] };
    if (shouldFail) throw new Error('<img src=x>');
    return { model: 'denied-model', model_denied: true, candidates: [], excluded: [], limitations: [] };
  });
  await api.open();
  elements.get('routePreviewModel').value = 'denied-model';
  await api.run();
  assert.ok(elements.get('routePreviewResults').textContent.includes('此令牌无权访问该模型。'));
  shouldFail = true;
  await api.run();
  assert.ok(elements.get('routePreviewStatus').textContent.includes('<img src=x>'));
  assert.equal(elements.get('routePreviewRun').disabled, false);
  assert.ok(!elements.get('routePreviewResults').textContent.includes('此令牌无权访问该模型。'));
});

test('failed token loading blocks unrestricted submission and can recover on reopening', async () => {
  let shouldFail = true;
  const { api, elements } = setup(async () => {
    if (shouldFail) throw new Error('load failed');
    return { tokens: [] };
  });
  await api.open();
  assert.equal(elements.get('routePreviewRun').disabled, true);
  assert.ok(elements.get('routePreviewStatus').textContent.includes('load failed'));
  shouldFail = false;
  await api.open();
  assert.equal(elements.get('routePreviewRun').disabled, false);
});

test('model choices preserve valid selection, replace removed models, and block empty submission', async () => {
  let models = ['alpha', '<img src=x>', 'beta'];
  const payloads = [];
  const { api, elements } = setup(async (url, options) => {
    if (url === '/admin/auth-tokens') return { tokens: [] };
    payloads.push(JSON.parse(options.body));
    return { model: payloads.at(-1).model, candidates: [], excluded: [], limitations: [] };
  }, () => models);
  await api.open();
  const select = elements.get('routePreviewModel');
  assert.deepEqual(select.options.map(option => option.value), models);
  assert.equal(select.options[1].textContent, '<img src=x>');
  assert.equal(descendants(select, 'img').length, 0);
  select.value = 'beta';
  await api.run();
  assert.equal(payloads.at(-1).model, 'beta');

  models = ['beta', 'gamma'];
  api.refreshModels();
  await api.run();
  assert.equal(payloads.at(-1).model, 'beta');

  models = ['gamma'];
  api.refreshModels();
  await api.run();
  assert.equal(payloads.at(-1).model, 'gamma');

  models = [];
  api.refreshModels();
  const count = payloads.length;
  await api.run();
  assert.equal(payloads.length, count);
  assert.equal(select.disabled, true);
  assert.equal(elements.get('routePreviewRun').disabled, true);

  models = ['delta'];
  await api.open();
  await api.run();
  assert.equal(payloads.at(-1).model, 'delta');
  assert.equal(select.disabled, false);
});
