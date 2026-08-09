const test = require('node:test');
const assert = require('node:assert/strict');

const {
  applyChannelAuthEditorMode,
  cancelAntigravityOAuth,
  cancelXAIOAuth,
  pollCodexOAuthStatus,
  copyCodexOAuthLink,
  copyOAuthCredential,
  cancelCodexOAuth,
  formatCodexPlanBadgeText,
  importOAuthCredentials,
  pollAntigravityOAuthStatus,
  pollXAIOAuthStatus,
  getOAuthUsageState,
  refreshOAuthUsage,
  refreshOAuthUsageBatch,
  batchRefreshSelectedOAuthUsage,
  refreshOAuthCredential,
  openOAuthCredentialImportDialog,
  openOAuthLoginDialog,
  setOAuthCredentialView,
  setupOAuthActions,
  showOAuthSession,
  submitXAICredentialBatch,
  submitAntigravityOAuthCallback,
  submitCodexOAuthCallback,
  submitXAIOAuthCallback
} = require('./channels-codex-auth.js');

test('xAI manual OAuth helpers use the shared state and callback contract', async () => {
  const requests = [];
  const status = await pollXAIOAuthStatus('xai/state', {
    fetchStatus: async url => {
      requests.push({ url });
      return { status: 'complete', channel_id: 91 };
    },
    delay: async () => {},
    maxPolls: 1
  });
  assert.equal(status.channel_id, 91);
  assert.equal(requests[0].url, '/admin/xai/oauth/status?state=xai%2Fstate');

  await submitXAIOAuthCallback(
    '  http://127.0.0.1:56121/callback?code=code-1&state=state-1  ',
    async (url, options) => {
      requests.push({ url, options });
      return { status: 'complete', state: 'state-1', channel_id: 91 };
    }
  );
  assert.equal(requests[1].url, '/admin/xai/oauth/callback');
  assert.deepEqual(JSON.parse(requests[1].options.body), {
    callback_url: 'http://127.0.0.1:56121/callback?code=code-1&state=state-1'
  });

  await cancelXAIOAuth(' state-2 ', async (url, options) => {
    requests.push({ url, options });
    return { status: 'cancelled', state: 'state-2' };
  });
  assert.equal(requests[2].url, '/admin/xai/oauth/cancel');
  assert.deepEqual(JSON.parse(requests[2].options.body), { state: 'state-2' });
});

test('xAI refresh-token and SSO jobs survive progress read errors and clear submitted secrets', async () => {
  const previousWindow = global.window;
  const storageWrites = [];
  global.window = {
    t: key => key,
    localStorage: { setItem: (...args) => storageWrites.push(args) },
    sessionStorage: { setItem: (...args) => storageWrites.push(args) }
  };
  const makeResponse = data => ({
    ok: true,
    status: 200,
    async text() {
      return JSON.stringify({ success: true, data });
    }
  });
  try {
    for (const [method, secret] of [['refresh_token', 'rt-secret-value'], ['sso', 'sso-secret-value']]) {
      const textarea = { value: secret, removeAttribute() {}, setAttribute() {}, focus() {} };
      let captured;
      let pollAttempts = 0;
      let streamReads = 0;
      const result = await submitXAICredentialBatch(
        method,
        textarea,
        null,
        async (url, options) => {
          assert.equal(textarea.value, '');
          if (options.method === 'POST') {
            captured = { url, options };
            return {
              ok: true,
              body: {
                getReader() {
                  return {
                    async read() {
                      streamReads++;
                      if (streamReads === 1) {
                        return {
                          done: false,
                          value: new TextEncoder().encode(
                            `event: start\ndata: {"event":"start","job_id":"ocij-${method}","total":1}\n\n`
                          )
                        };
                      }
                      throw new Error('Error in input stream');
                    }
                  };
                }
              }
            };
          }
          pollAttempts++;
          return makeResponse({
            job_id: `ocij-${method}`, status: 'succeeded', processed: 1, total: 1,
            created: 1, skipped: 0, failed: 0, results: [], next: 1
          });
        },
        undefined,
        async () => {}
      );
      assert.equal(result.created, 1);
      assert.equal(pollAttempts, 1);
      assert.equal(captured.url, '/admin/xai/credentials/import/stream');
      assert.deepEqual(JSON.parse(captured.options.body), {
        method,
        values: secret,
        priority_increment: 10
      });
      assert.equal(textarea.value, '');
      assert.doesNotMatch(captured.url, /secret/);
    }
    assert.deepEqual(storageWrites, []);
  } finally {
    global.window = previousWindow;
  }
});

test('xAI credential import renders streamed item progress in the OAuth dialog', async () => {
  const makeTarget = properties => ({
    dataset: {}, listeners: {},
    addEventListener(type, listener) { this.listeners[type] = listener; },
    setAttribute(name, value) { this[name] = value; },
    removeAttribute(name) { delete this[name]; },
    ...properties
  });
  const dialog = makeTarget({ open: true, close() { this.open = false; } });
  const form = makeTarget({});
  const provider = makeTarget({ value: 'xai', disabled: false });
  const method = makeTarget({ value: 'sso', disabled: false });
  const textarea = makeTarget({ value: 'cookie-1\ncookie-2', focus() {} });
  const button = makeTarget({ disabled: false });
  const progress = makeTarget({ hidden: true, focus() { this.focused = true; } });
  const errorList = {
    children: [],
    append(item) { this.children.push(item); },
    replaceChildren() { this.children = []; }
  };
  const elements = new Map([
    ['oauthLoginDialog', dialog],
    ['oauthLoginForm', form],
    ['oauthProviderSelect', provider],
    ['xaiOAuthMethod', method],
    ['xaiCredentialValues', textarea],
    ['oauthAuthorizeButton', button],
    ['oauthSessionFields', { hidden: true }],
    ['oauthLoginDialogStatus', { textContent: '', hidden: true, dataset: {} }],
    ['xaiCredentialImportProgress', progress],
    ['xaiCredentialImportProgressBar', { max: 1, value: 0 }],
    ['xaiCredentialImportProgressCounter', { textContent: '' }],
    ['xaiCredentialImportProgressDetail', { textContent: '' }],
    ['xaiCredentialImportProgressCounts', { textContent: '' }],
    ['xaiCredentialImportErrors', { hidden: true }],
    ['xaiCredentialImportErrorList', errorList]
  ]);
  const previous = new Map();
  const setGlobal = (key, value) => {
    previous.set(key, Object.getOwnPropertyDescriptor(global, key));
    Object.defineProperty(global, key, { configurable: true, writable: true, value });
  };
  let reloads = 0;
  setGlobal('document', {
    getElementById: id => elements.get(id) || null,
    querySelectorAll: () => [],
    createElement: () => ({ textContent: '' })
  });
  setGlobal('window', {
    t: (key, values = {}) => `${key}:${JSON.stringify(values)}`,
    showError() {}
  });
  setGlobal('fetchWithAuth', async () => ({
    ok: true,
    body: null,
    async text() {
      return [
        'event: start\ndata: {"event":"start","job_id":"ocij-xai","processed":0,"total":2,"created":0,"skipped":0,"failed":0}',
        'event: processing\ndata: {"event":"processing","processed":0,"total":2,"created":0,"skipped":0,"failed":0,"file_name":"#1"}',
        'event: progress\ndata: {"event":"progress","processed":1,"total":2,"created":1,"skipped":0,"failed":0,"file_name":"#1","result":{"file_name":"#1","status":"created"}}',
        'event: progress\ndata: {"event":"progress","processed":2,"total":2,"created":1,"skipped":0,"failed":1,"file_name":"#2","result":{"file_name":"#2","status":"failed","error":"xAI SSO import failed"}}',
        'event: complete\ndata: {"event":"complete","processed":2,"total":2,"created":1,"skipped":0,"failed":1}'
      ].join('\n\n') + '\n\n';
    }
  }));
  setGlobal('reloadChannelsList', async () => { reloads++; });

  try {
    setupOAuthActions();
    await form.listeners.submit({ preventDefault() {} });

    assert.equal(elements.get('xaiCredentialImportProgress').hidden, false);
    assert.equal(progress.focused, true);
    assert.equal(elements.get('xaiCredentialImportProgressBar').max, 2);
    assert.equal(elements.get('xaiCredentialImportProgressBar').value, 2);
    assert.match(elements.get('xaiCredentialImportProgressCounter').textContent, /"processed":2/);
    assert.match(elements.get('xaiCredentialImportProgressCounts').textContent, /"created":1/);
    assert.match(elements.get('xaiCredentialImportProgressCounts').textContent, /"failed":1/);
    assert.match(elements.get('xaiCredentialImportProgressDetail').textContent, /progressComplete/);
    assert.equal(elements.get('xaiCredentialImportErrors').hidden, false);
    assert.equal(errorList.children.length, 1);
    assert.match(errorList.children[0].textContent, /#2/);
    assert.equal(reloads, 1);
  } finally {
    for (const [key, descriptor] of previous) {
      if (descriptor === undefined) delete global[key];
      else Object.defineProperty(global, key, descriptor);
    }
  }
});

test('closing and pagehide abort active xAI imports and clear browser-held secrets', async () => {
  const makeTarget = properties => ({
    dataset: {}, listeners: {},
    addEventListener(type, listener) { this.listeners[type] = listener; },
    ...properties
  });
  const dialog = makeTarget({ open: true, close() { this.open = false; } });
  const form = makeTarget({});
  const provider = { value: 'xai', disabled: false };
  const method = makeTarget({ value: 'refresh_token' });
  const textarea = { value: 'rt-hanging', removeAttribute() {}, setAttribute() {}, focus() {} };
  const button = { disabled: false, setAttribute() {}, removeAttribute() {} };
  const statusWrites = [];
  const status = { hidden: true, dataset: {} };
  Object.defineProperty(status, 'textContent', {
    set(value) { statusWrites.push(value); }, get() { return statusWrites.at(-1) || ''; }
  });
  const elements = new Map([
    ['oauthLoginDialog', dialog],
    ['oauthLoginForm', form],
    ['oauthProviderSelect', provider],
    ['xaiOAuthMethod', method],
    ['xaiCredentialValues', textarea],
    ['oauthAuthorizeButton', button],
    ['oauthSessionFields', { hidden: true }],
    ['oauthLoginDialogStatus', status]
  ]);
  const previous = new Map();
  const setGlobal = (key, value) => {
    previous.set(key, Object.getOwnPropertyDescriptor(global, key));
    Object.defineProperty(global, key, { configurable: true, writable: true, value });
  };
  const pageListeners = {};
  const signals = [];
  let reloads = 0;
  setGlobal('document', {
    getElementById: id => elements.get(id) || null,
    querySelectorAll: () => []
  });
  setGlobal('window', {
    t: key => key,
    addEventListener: (type, listener) => { pageListeners[type] = listener; },
    showError() {}
  });
  setGlobal('fetchWithAuth', (_url, options) => new Promise((resolve, reject) => {
    signals.push(options.signal);
    options.signal?.addEventListener('abort', () => reject(new Error('aborted')));
  }));
  setGlobal('reloadChannelsList', async () => { reloads++; });

  try {
    setupOAuthActions();
    const firstSubmit = form.listeners.submit({ preventDefault() {} });
    await new Promise(resolve => setImmediate(resolve));
    const beforeCloseWrites = statusWrites.length;
    dialog.listeners.cancel({ preventDefault() {} });
    await firstSubmit;
    assert.equal(signals[0]?.aborted, true);
    assert.equal(reloads, 0);
    assert.equal(statusWrites.length, beforeCloseWrites);

    dialog.open = true;
    textarea.value = 'sso-hanging';
    method.value = 'sso';
    const secondSubmit = form.listeners.submit({ preventDefault() {} });
    await new Promise(resolve => setImmediate(resolve));
    const beforePagehideWrites = statusWrites.length;
    pageListeners.pagehide();
    await secondSubmit;
    assert.equal(signals[1]?.aborted, true);
    assert.equal(statusWrites.length, beforePagehideWrites);
    assert.equal(reloads, 0);
    assert.equal(provider.disabled, false);

    textarea.value = 'unsubmitted-secret';
    pageListeners.pagehide();
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(textarea.value, '');
  } finally {
    for (const [key, descriptor] of previous) {
      if (descriptor === undefined) delete global[key];
      else Object.defineProperty(global, key, descriptor);
    }
  }
});

test('Codex plan badge appends the subscription calendar date', () => {
  assert.equal(formatCodexPlanBadgeText('plus', '2030-02-03T04:05:06Z'), 'plus · 2030-02-03');
  assert.equal(formatCodexPlanBadgeText('free', ''), 'free');
  assert.equal(formatCodexPlanBadgeText('', '2030-02-03T04:05:06Z'), '');
});

test('logs channel editor loads Codex auth before opening a Codex channel', async () => {
  const requiredMarkupIDs = new Set([
    'channelModal',
    'commonModelsModal',
    'keyImportModal',
    'keyExportModal',
    'modelImportModal',
    'customRulesModal',
    'tpl-key-row',
    'tpl-key-empty',
    'tpl-cooldown-badge',
    'tpl-key-normal-status',
    'tpl-key-actions',
    'tpl-url-row',
    'tpl-url-empty',
    'tpl-redirect-row',
    'tpl-redirect-empty'
  ]);
  const elements = new Map();
  for (const id of [
    'codexCredentialReadOnlyNotice',
    'channelAPIKeyHeader',
    'channelAPIKeyTable',
    'channelApiKey',
    'importKeysBtn',
    'batchDeleteKeysBtn',
    'selectAllKeys',
    'codexCredentialTab',
    'channelCodexPlanBadge'
  ]) {
    elements.set(id, { hidden: true, required: true, value: '' });
  }
  elements.set('codexCredentialContent', {
    textContent: '',
    removeAttribute() {},
    classList: { add() {}, remove() {} }
  });

  const scripts = [{ src: 'http://localhost/web/assets/js/logs-channel-editor.js?v=test' }];
  let openedChannelID = null;
  const previous = new Map();
  const installGlobal = (name, value) => {
    previous.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  };

  installGlobal('window', {
    location: { origin: 'http://localhost' },
    t: key => key,
    showError() {}
  });
  installGlobal('document', {
    scripts,
    head: {
      appendChild(script) {
        scripts.push(script);
        const path = new URL(script.src, global.window.location.origin).pathname;
        if (path === '/web/assets/js/channels-codex-auth.js') {
          global.applyChannelAuthEditorMode = applyChannelAuthEditorMode;
        }
        if (path === '/web/assets/js/channels-modals.js') {
          global.editChannel = async id => {
            openedChannelID = id;
            if (typeof global.applyChannelAuthEditorMode === 'function') {
              global.applyChannelAuthEditorMode(
                'codex_oauth',
                { access_token: 'at-from-log-editor', refresh_token: 'rt-secret' },
                { codex_plan_type: 'plus' }
              );
            }
          };
        }
        script.onload();
      }
    },
    createElement: () => ({}),
    getElementById: id => elements.get(id) || (requiredMarkupIDs.has(id) ? {} : null),
    querySelectorAll: () => [],
    addEventListener() {}
  });
  previous.set('applyChannelAuthEditorMode', Object.getOwnPropertyDescriptor(global, 'applyChannelAuthEditorMode'));
  previous.set('editChannel', Object.getOwnPropertyDescriptor(global, 'editChannel'));
  delete global.applyChannelAuthEditorMode;
  delete global.editChannel;

  const modulePath = require.resolve('./logs-channel-editor.js');
  delete require.cache[modulePath];
  try {
    require(modulePath);
    await global.window.openLogChannelEditor(42);

    assert.equal(openedChannelID, 42);
    assert.equal(elements.get('codexCredentialTab').hidden, false);
    assert.match(elements.get('codexCredentialContent').textContent, /at-from-log-editor/);
  } finally {
    delete require.cache[modulePath];
    for (const [name, descriptor] of previous) {
      if (descriptor) Object.defineProperty(global, name, descriptor);
      else delete global[name];
    }
  }
});

test('Codex OAuth status polling waits for completion and encodes state', async () => {
  const requests = [];
  const statuses = [
    { status: 'pending' },
    { status: 'complete', channel_id: 42 }
  ];
  const result = await pollCodexOAuthStatus('state with / symbols', {
    fetchStatus: async url => {
      requests.push(url);
      return statuses.shift();
    },
    delay: async () => {},
    interval: 0,
    maxPolls: 2
  });

  assert.equal(result.channel_id, 42);
  assert.equal(requests.length, 2);
  assert.equal(requests[0], '/admin/codex/oauth/status?state=state%20with%20%2F%20symbols');
});

test('OAuth login dialog requires provider selection before exposing an authorization session', async () => {
  const elements = new Map([
    ['oauthLoginDialog', { open: false, showModal() { this.open = true; } }],
    ['oauthProviderSelect', { value: 'antigravity', disabled: true, focus() { this.focused = true; } }],
    ['oauthAuthorizeButton', { disabled: true, hidden: false }],
    ['oauthSessionFields', { hidden: false }],
    ['oauthAuthorizationURL', { value: 'stale', focus() { this.focused = true; }, select() { this.selected = true; } }],
    ['oauthOpenLink', { href: 'https://stale.example', removeAttribute(name) { if (name === 'href') this.href = ''; } }],
    ['oauthCallbackURL', { value: 'stale', removeAttribute() {} }],
    ['oauthLoginDialogStatus', { textContent: 'stale', hidden: false, dataset: {} }]
  ]);
  const previousDocument = global.document;
  global.document = { getElementById: id => elements.get(id) || null };
  try {
    assert.equal(openOAuthLoginDialog({ focus() {} }), true);
    assert.equal(elements.get('oauthLoginDialog').open, true);
    assert.equal(elements.get('oauthProviderSelect').value, 'codex');
    assert.equal(elements.get('oauthProviderSelect').disabled, false);
    assert.equal(elements.get('oauthProviderSelect').focused, true);
    assert.equal(elements.get('oauthAuthorizeButton').disabled, false);
    assert.equal(elements.get('oauthSessionFields').hidden, true);
    assert.equal(elements.get('oauthAuthorizationURL').value, '');
    assert.equal(elements.get('oauthOpenLink').href, '');

    assert.equal(showOAuthSession({ url: 'https://auth.example/authorize?state=abc', state: 'abc' }, 'antigravity'), true);
    assert.equal(elements.get('oauthProviderSelect').value, 'antigravity');
    assert.equal(elements.get('oauthProviderSelect').disabled, true);
    assert.equal(elements.get('oauthAuthorizeButton').hidden, true);
    assert.equal(elements.get('oauthSessionFields').hidden, false);
    assert.equal(elements.get('oauthAuthorizationURL').value, 'https://auth.example/authorize?state=abc');
    assert.equal(elements.get('oauthOpenLink').href, 'https://auth.example/authorize?state=abc');
    assert.equal(elements.get('oauthCallbackURL').value, '');

    let copied = '';
    await copyCodexOAuthLink('https://auth.example/authorize?state=abc', async text => { copied = text; });
    assert.equal(copied, 'https://auth.example/authorize?state=abc');
  } finally {
    global.document = previousDocument;
  }
});

test('OAuth login toolbar waits for explicit authorization after provider selection', async () => {
  const makeTarget = properties => ({
    dataset: {}, listeners: {},
    addEventListener(type, listener) { this.listeners[type] = listener; },
    ...properties
  });
  const loginButton = makeTarget({ focus() { this.focused = true; } });
  const dialog = makeTarget({
    open: false,
    showModal() { this.open = true; },
    close() { this.open = false; }
  });
  const loginForm = makeTarget({});
  const providerSelect = makeTarget({ value: 'codex', disabled: false, focus() { this.focused = true; } });
  const xaiMethod = makeTarget({ value: 'manual' });
  const authorizeButton = {
    disabled: false, hidden: false, textContent: '',
    setAttribute(name, value) { this[name] = value; }
  };
  const sessionFields = { hidden: false };
  const authorizationURL = { value: '', focus() {}, select() {} };
  const openLink = { href: '', removeAttribute() { this.href = ''; } };
  const callbackURL = { value: '', removeAttribute() {} };
  const dialogDescription = { textContent: '' };
  const secretField = { hidden: false };
  const xaiProgress = { hidden: false };
  const elements = new Map([
    ['oauthLoginBtn', loginButton],
    ['oauthLoginDialog', dialog],
    ['oauthLoginForm', loginForm],
    ['oauthProviderSelect', providerSelect],
    ['xaiOAuthMethod', xaiMethod],
    ['xaiOAuthControls', { hidden: true }],
    ['xaiCredentialSecretField', secretField],
    ['xaiCredentialImportProgress', xaiProgress],
    ['xaiCredentialValues', { value: '', removeAttribute() {}, setAttribute() {} }],
    ['oauthAuthorizeButton', authorizeButton],
    ['oauthSessionFields', sessionFields],
    ['oauthAuthorizationURL', authorizationURL],
    ['oauthOpenLink', openLink],
    ['oauthCallbackURL', callbackURL],
    ['oauthLoginDialogDescription', dialogDescription]
  ]);
  const previousDocument = global.document;
  const previousWindow = global.window;
  const previousFetch = global.fetchDataWithAuth;
  const previousReload = global.reloadChannelsList;
  const requests = [];
  global.document = {
    getElementById: id => elements.get(id) || null,
    querySelectorAll: () => []
  };
  global.window = { t: key => key, showSuccess() {}, showError() {} };
  global.fetchDataWithAuth = async (url) => {
    requests.push(url);
    if (url.endsWith('/oauth/start')) {
      return { url: 'https://accounts.example/authorize', state: 'gravity-state' };
    }
    return { status: 'complete', channel_id: 9 };
  };
  global.reloadChannelsList = async () => {};
  try {
    setupOAuthActions();
    loginButton.listeners.click();

    assert.equal(dialog.open, true);
    assert.equal(providerSelect.focused, true);
    assert.equal(sessionFields.hidden, true);
    assert.deepEqual(requests, []);

    providerSelect.value = 'xai';
    providerSelect.listeners.change();
    assert.equal(secretField.hidden, true);
    assert.equal(authorizeButton.hidden, false);
    assert.equal(authorizeButton.textContent, 'channels.xai.generateLink');
    assert.equal(dialogDescription.textContent, 'channels.xai.manualDescription');
    await loginForm.listeners.submit({ preventDefault() {} });
    assert.deepEqual(requests, [
      '/admin/xai/oauth/start',
      '/admin/xai/oauth/status?state=gravity-state'
    ]);

    openOAuthLoginDialog(loginButton);
    providerSelect.value = 'xai';
    providerSelect.listeners.change();
    xaiMethod.value = 'sso';
    xaiMethod.listeners.change();
    assert.equal(secretField.hidden, false);
    assert.equal(authorizeButton.textContent, 'channels.xai.importSecrets');
    assert.equal(xaiProgress.hidden, true);
    assert.equal(dialogDescription.textContent, 'channels.xai.importDescription');
    openOAuthLoginDialog(loginButton);
    assert.equal(authorizeButton.hidden, false);
    assert.equal(authorizeButton.textContent, 'channels.oauth.startAuthorization');
    assert.equal(dialogDescription.textContent, 'channels.oauth.loginDialogDescription');

    providerSelect.value = 'antigravity';
    await loginForm.listeners.submit({ preventDefault() {} });
    assert.deepEqual(requests, [
      '/admin/xai/oauth/start',
      '/admin/xai/oauth/status?state=gravity-state',
      '/admin/antigravity/oauth/start',
      '/admin/antigravity/oauth/status?state=gravity-state'
    ]);
  } finally {
    global.document = previousDocument;
    global.window = previousWindow;
    global.fetchDataWithAuth = previousFetch;
    global.reloadChannelsList = previousReload;
  }
});

test('OAuth credential import dialog defaults to automatic detection with priority increments of 10', () => {
  const elements = new Map([
    ['oauthCredentialImportDialog', { open: false, showModal() { this.open = true; } }],
    ['oauthImportProviderSelect', { value: 'antigravity', focus() { this.focused = true; } }],
    ['oauthImportPriorityIncrement', { value: '50' }],
    ['oauthCredentialImportInput', { value: 'stale', removeAttribute() {} }],
    ['oauthCredentialImportStatus', { textContent: 'stale', hidden: false, dataset: {} }],
    ['oauthCredentialImportProgress', { hidden: false }],
    ['oauthCredentialImportProgressBar', { max: 9, value: 8 }],
    ['oauthCredentialImportProgressCounter', { textContent: '8 / 9' }],
    ['oauthCredentialImportProgressDetail', { textContent: 'stale' }],
    ['oauthCredentialImportProgressCounts', { textContent: 'stale' }],
    ['oauthCredentialImportErrors', { hidden: false }],
    ['oauthCredentialImportErrorList', {
      children: ['stale'],
      replaceChildren() { this.children = []; }
    }]
  ]);
  const previousDocument = global.document;
  global.document = { getElementById: id => elements.get(id) || null };
  try {
    assert.equal(openOAuthCredentialImportDialog({ focus() {} }), true);
    assert.equal(elements.get('oauthCredentialImportDialog').open, true);
    assert.equal(elements.get('oauthImportProviderSelect').value, 'auto');
    assert.equal(elements.get('oauthImportProviderSelect').focused, true);
    assert.equal(elements.get('oauthImportPriorityIncrement').value, '10');
    assert.equal(elements.get('oauthCredentialImportInput').value, '');
    assert.equal(elements.get('oauthCredentialImportStatus').hidden, true);
    assert.equal(elements.get('oauthCredentialImportProgress').hidden, true);
    assert.equal(elements.get('oauthCredentialImportProgressBar').max, 1);
    assert.equal(elements.get('oauthCredentialImportProgressBar').value, 0);
    assert.equal(elements.get('oauthCredentialImportErrors').hidden, true);
    assert.deepEqual(elements.get('oauthCredentialImportErrorList').children, []);
  } finally {
    global.document = previousDocument;
  }
});

test('completed OAuth credential import keeps the dialog open for result review', async () => {
  const previousDocument = global.document;
  const previousWindow = global.window;
  const previousFormData = global.FormData;
  const previousFetch = global.fetchWithAuth;
  const previousReload = global.reloadChannelsList;
  const makeTarget = properties => ({
    dataset: {}, listeners: {},
    addEventListener(type, listener) { this.listeners[type] = listener; },
    ...properties
  });
  const dialog = makeTarget({
    open: true,
    closeCalls: 0,
    close() { this.open = false; this.closeCalls++; }
  });
  const form = makeTarget({});
  const provider = { value: 'auto', disabled: false };
  const priority = { value: '10', disabled: false };
  const input = { files: [{ name: 'one.json' }] };
  const submit = { disabled: false };
  const elements = new Map([
    ['oauthCredentialImportDialog', dialog],
    ['oauthCredentialImportForm', form],
    ['oauthImportProviderSelect', provider],
    ['oauthImportPriorityIncrement', priority],
    ['oauthCredentialImportInput', input],
    ['oauthCredentialImportSubmit', submit],
    ['oauthCredentialImportStatus', { textContent: '', hidden: true, dataset: {} }],
    ['oauthCredentialImportProgress', { hidden: true }],
    ['oauthCredentialImportProgressBar', { max: 1, value: 0 }],
    ['oauthCredentialImportProgressCounter', { textContent: '' }],
    ['oauthCredentialImportProgressDetail', { textContent: '' }],
    ['oauthCredentialImportProgressCounts', { textContent: '' }],
    ['oauthCredentialImportErrors', { hidden: true }],
    ['oauthCredentialImportErrorList', { replaceChildren() {}, append() {} }]
  ]);
  class FakeFormData { append() {} }
  global.FormData = FakeFormData;
  global.document = {
    getElementById: id => elements.get(id) || null,
    querySelectorAll: () => [],
    createElement: () => ({ textContent: '' })
  };
  global.window = { t: key => key, showSuccess() {}, showError() {} };
  let requestCount = 0;
  global.fetchWithAuth = async () => {
    requestCount++;
    const data = requestCount === 1
      ? { job_id: 'ocij-dialog', total: 1 }
      : {
          job_id: 'ocij-dialog', status: 'succeeded', processed: 1, total: 1,
          created: 1, skipped: 0, failed: 0, next: 1,
          results: [{ file_name: 'one.json', channel_name: 'Codex-one', status: 'created' }]
        };
    return { ok: true, async text() { return JSON.stringify({ success: true, data }); } };
  };
  global.reloadChannelsList = async () => {};
  try {
    setupOAuthActions();
    await form.listeners.submit({ preventDefault() {} });
    assert.equal(dialog.open, true);
    assert.equal(dialog.closeCalls, 0);
  } finally {
    global.document = previousDocument;
    global.window = previousWindow;
    global.FormData = previousFormData;
    global.fetchWithAuth = previousFetch;
    global.reloadChannelsList = previousReload;
  }
});

test('manual Codex OAuth callback submits the complete callback URL as JSON', async () => {
  let captured;
  const result = await submitCodexOAuthCallback(
    '  http://localhost:1455/auth/callback?code=code-1&state=state-1  ',
    async (url, options) => {
      captured = { url, options };
      return { status: 'accepted', state: 'state-1' };
    }
  );

  assert.deepEqual(result, { status: 'accepted', state: 'state-1' });
  assert.equal(captured.url, '/admin/codex/oauth/callback');
  assert.equal(captured.options.method, 'POST');
  assert.deepEqual(JSON.parse(captured.options.body), {
    callback_url: 'http://localhost:1455/auth/callback?code=code-1&state=state-1'
  });
});

test('Codex OAuth cancellation submits the active state as JSON', async () => {
  let captured;
  const result = await cancelCodexOAuth('  state-1  ', async (url, options) => {
    captured = { url, options };
    return { status: 'cancelled', state: 'state-1' };
  });

  assert.deepEqual(result, { status: 'cancelled', state: 'state-1' });
  assert.equal(captured.url, '/admin/codex/oauth/cancel');
  assert.equal(captured.options.method, 'POST');
  assert.deepEqual(JSON.parse(captured.options.body), { state: 'state-1' });
});

test('Antigravity OAuth helpers use the Antigravity admin contract', async () => {
  const requests = [];
  const status = await pollAntigravityOAuthStatus('gravity/state', {
    fetchStatus: async url => {
      requests.push(url);
      return { status: 'complete', channel_id: 9 };
    },
    delay: async () => {},
    maxPolls: 1
  });
  assert.equal(status.channel_id, 9);
  assert.equal(requests[0], '/admin/antigravity/oauth/status?state=gravity%2Fstate');

  await submitAntigravityOAuthCallback('http://localhost:51121/oauth-callback?code=x&state=y', async (url, options) => {
    requests.push(url);
    assert.equal(JSON.parse(options.body).callback_url, 'http://localhost:51121/oauth-callback?code=x&state=y');
    return { status: 'accepted' };
  });
  await cancelAntigravityOAuth('y', async (url, options) => {
    requests.push(url);
    assert.deepEqual(JSON.parse(options.body), { state: 'y' });
    return { status: 'cancelled' };
  });
  assert.deepEqual(requests.slice(1), [
    '/admin/antigravity/oauth/callback',
    '/admin/antigravity/oauth/cancel'
  ]);
});

test('OAuth credential import polls a background job, recovers from network errors, and shows skipped reasons', async () => {
  const previousFormData = global.FormData;
  const previousDocument = global.document;
  const previousWindow = global.window;
  const previousReload = global.reloadChannelsList;
  class FakeFormData {
    constructor() { this.items = []; }
    append(name, value) { this.items.push([name, value]); }
  }
  global.FormData = FakeFormData;
  const elements = new Map([
    ['oauthCredentialImportStatus', { textContent: '', hidden: true, dataset: {} }],
    ['oauthCredentialImportProgress', { hidden: true }],
    ['oauthCredentialImportProgressBar', { max: 1, value: 0 }],
    ['oauthCredentialImportProgressCounter', { textContent: '' }],
    ['oauthCredentialImportProgressDetail', { textContent: '' }],
    ['oauthCredentialImportProgressCounts', { textContent: '' }],
    ['oauthCredentialImportErrors', { hidden: true }],
    ['oauthCredentialImportErrorList', {
      children: [],
      replaceChildren() { this.children = []; },
      append(child) { this.children.push(child); }
    }]
  ]);
  global.document = {
    getElementById: id => elements.get(id) || null,
    createElement: () => ({ textContent: '', dataset: {} })
  };
  global.window = {
    t: (key, params) => `${key}:${Object.values(params || {}).join(':')}`,
    showSuccess() {},
    showError() {}
  };
  let reloads = 0;
  global.reloadChannelsList = async () => { reloads++; };
  const files = [{ name: 'credentials.zip' }];
  const captured = [];
  const jsonResponse = (data, status = 200) => ({
    ok: status >= 200 && status < 300,
    status,
    async text() { return JSON.stringify({ success: status < 400, data, error: status < 400 ? '' : 'request failed' }); }
  });
  let statusRequests = 0;
  try {
    const result = await importOAuthCredentials(files, null, async (url, options) => {
      captured.push({ url, options });
      if (options?.method === 'POST') return jsonResponse({ job_id: 'ocij-1', total: 3 }, 202);
      statusRequests++;
      if (statusRequests === 1) throw new Error('network error');
      if (statusRequests === 2) {
        return jsonResponse({
          job_id: 'ocij-1', status: 'running', processed: 1, total: 3,
          created: 1, skipped: 0, failed: 0, file_name: 'credentials.zip/two.json', next: 1,
          results: [{ file_name: 'credentials.zip/one.json', channel_name: 'Codex-one', status: 'created' }]
        });
      }
      return jsonResponse({
        job_id: 'ocij-1', status: 'succeeded', processed: 3, total: 3,
        created: 1, skipped: 1, failed: 1, next: 3,
        results: [
          { file_name: 'credentials.zip/two.json', status: 'skipped', error: 'credential type could not be determined' },
          { file_name: 'credentials.zip/three.json', status: 'failed', error: 'invalid credential' }
        ]
      });
    }, 'xai', 10, async () => {});

    assert.equal(result.created, 1);
    assert.equal(result.skipped, 1);
    assert.equal(result.failed, 1);
    assert.equal(result.results.length, 3);
    assert.equal(captured[0].url, '/admin/oauth/credentials/import/jobs');
    assert.equal(captured[0].options.method, 'POST');
    assert.deepEqual(captured[0].options.body.items, [
      ['files', files[0]],
      ['provider', 'xai'],
      ['priority_increment', '10']
    ]);
    assert.equal(elements.get('oauthCredentialImportProgress').hidden, false);
    assert.equal(captured[1].url, '/admin/oauth/credentials/import/jobs/ocij-1?after=0');
    assert.equal(captured[2].url, '/admin/oauth/credentials/import/jobs/ocij-1?after=0');
    assert.equal(captured[3].url, '/admin/oauth/credentials/import/jobs/ocij-1?after=1');
    assert.equal(elements.get('oauthCredentialImportProgressBar').max, 3);
    assert.equal(elements.get('oauthCredentialImportProgressBar').value, 3);
    assert.match(elements.get('oauthCredentialImportProgressCounter').textContent, /3/);
    assert.match(elements.get('oauthCredentialImportProgressCounts').textContent, /1/);
    assert.equal(elements.get('oauthCredentialImportErrors').hidden, false);
    assert.equal(elements.get('oauthCredentialImportErrorList').children.length, 2);
    assert.match(elements.get('oauthCredentialImportErrorList').children[0].textContent, /credentials\.zip\/two\.json/);
    assert.match(elements.get('oauthCredentialImportErrorList').children[0].textContent, /credential type could not be determined/);
    assert.match(elements.get('oauthCredentialImportErrorList').children[1].textContent, /invalid credential/);
    assert.equal(reloads, 1);
  } finally {
    global.FormData = previousFormData;
    global.document = previousDocument;
    global.window = previousWindow;
    global.reloadChannelsList = previousReload;
  }
});

test('manual Codex credential refresh targets the saved channel', async () => {
  let captured;
  const response = { oauth_credential: { access_token: 'at-new' } };
  const result = await refreshOAuthCredential(42, async (url, options) => {
    captured = { url, options };
    return response;
  });

  assert.equal(result, response);
  assert.deepEqual(captured, {
    url: '/admin/channels/42/codex-credential/refresh',
    options: { method: 'POST' }
  });
  await assert.rejects(() => refreshOAuthCredential(0, async () => response), /saved Codex channel/);
});

test('manual Antigravity credential refresh targets the saved channel', async () => {
  let captured;
  const response = { oauth_credential: { access_token: 'gravity-at' } };
  const result = await refreshOAuthCredential(42, async (url, options) => {
    captured = { url, options };
    return response;
  }, 'antigravity_oauth');

  assert.equal(result, response);
  assert.deepEqual(captured, {
    url: '/admin/channels/42/antigravity-credential/refresh',
    options: { method: 'POST' }
  });
});

test('OAuth usage refresh stores one safe per-channel quota summary', async () => {
  const previousFilterChannels = global.filterChannels;
  let renders = 0;
  let captured;
  global.filterChannels = () => { renders++; };
  try {
    const result = await refreshOAuthUsage(42, async (url, options) => {
      captured = { url, options };
      return {
        plan_type: 'pro',
        windows: [{
          limit_name: 'codex', kind: 'primary', used_percent: 29,
          remaining_percent: 71, limit_window_seconds: 604800, reset_at: 1786163635
        }]
      };
    });

    assert.equal(captured.url, '/admin/channels/42/oauth-usage');
    assert.equal(captured.options.method, 'POST');
    assert.equal(result.windows[0].remaining_percent, 71);
    assert.deepEqual(getOAuthUsageState(42), { status: 'ready', data: result });
    assert.equal(renders, 2);
  } finally {
    global.filterChannels = previousFilterChannels;
  }
});

test('failed OAuth usage refresh remains retryable', async () => {
  const previousFilterChannels = global.filterChannels;
  global.filterChannels = () => {};
  try {
    await assert.rejects(
      refreshOAuthUsage(43, async () => { throw new Error('quota unavailable'); }),
      /quota unavailable/
    );
    assert.deepEqual(getOAuthUsageState(43), { status: 'error', error: 'quota unavailable' });
  } finally {
    global.filterChannels = previousFilterChannels;
  }
});

test('batch OAuth usage refresh keeps per-channel results and reloads the list once', async () => {
  const previousFilterChannels = global.filterChannels;
  const previousLoadChannels = global.loadChannels;
  const requested = [];
  let reloads = 0;
  global.filterChannels = () => {};
  global.loadChannels = async () => { reloads++; };

  try {
    const summary = await refreshOAuthUsageBatch([51, 52, 53], async (url) => {
      requested.push(url);
      if (url.includes('/52/')) throw new Error('quota unavailable');
      return { windows: [] };
    });

    assert.deepEqual(requested, [
      '/admin/channels/51/oauth-usage',
      '/admin/channels/52/oauth-usage',
      '/admin/channels/53/oauth-usage'
    ]);
    assert.deepEqual(summary, { total: 3, succeeded: 2, failed: 1 });
    assert.equal(getOAuthUsageState(51).status, 'ready');
    assert.deepEqual(getOAuthUsageState(52), { status: 'error', error: 'quota unavailable' });
    assert.equal(getOAuthUsageState(53).status, 'ready');
    assert.equal(reloads, 1);
  } finally {
    global.filterChannels = previousFilterChannels;
    if (previousLoadChannels === undefined) delete global.loadChannels;
    else global.loadChannels = previousLoadChannels;
  }
});

test('selected quota refresh skips non-OAuth channels and reports one batch result', async () => {
  const previousGlobals = new Map();
  const setGlobal = (name, value) => {
    previousGlobals.set(name, Object.getOwnPropertyDescriptor(global, name));
    Object.defineProperty(global, name, { configurable: true, writable: true, value });
  };
  const notices = [];
  const requested = [];
  const attributes = new Map();
  const menuAttributes = new Map();
  const button = {
    disabled: false,
    setAttribute: (name, value) => attributes.set(name, String(value)),
    removeAttribute: name => attributes.delete(name)
  };
  const floatingMenu = {
    setAttribute: (name, value) => menuAttributes.set(name, String(value)),
    removeAttribute: name => menuAttributes.delete(name)
  };
  const label = { textContent: '刷新额度', setAttribute() {} };

  setGlobal('window', {
    t: (key, params) => params ? { key, params } : key,
    showSuccess: message => notices.push({ type: 'success', message }),
    showWarning: message => notices.push({ type: 'warning', message }),
    showError: message => notices.push({ type: 'error', message })
  });
  setGlobal('document', {
    getElementById: id => ({
      batchRefreshOAuthUsageBtn: button,
      batchRefreshOAuthUsageLabel: label,
      batchFloatingMenu: floatingMenu
    })[id] || null
  });
  setGlobal('channels', [
    { id: 61, auth_type: 'codex_oauth' },
    { id: 62, auth_type: 'api_key' },
    { id: 63, auth_type: 'antigravity_oauth' }
  ]);
  setGlobal('getSelectedChannelIDs', () => [61, 62, 63]);
  setGlobal('filterChannels', () => {});
  setGlobal('loadChannels', async () => {});
  setGlobal('updateBatchChannelSelectionUI', () => {});

  try {
    const summary = await batchRefreshSelectedOAuthUsage(async (url) => {
      requested.push(url);
      return { windows: [] };
    });

    assert.deepEqual(requested, [
      '/admin/channels/61/oauth-usage',
      '/admin/channels/63/oauth-usage'
    ]);
    assert.deepEqual(summary, { total: 3, succeeded: 2, failed: 0, skipped: 1 });
    assert.deepEqual(notices, [{
      type: 'success',
      message: {
        key: 'channels.batchOAuthUsageSummary',
        params: { total: 3, succeeded: 2, failed: 0, skipped: 1 }
      }
    }]);
    assert.equal(button.disabled, false);
    assert.equal(attributes.has('aria-busy'), false);
    assert.equal(menuAttributes.has('aria-busy'), false);
    assert.equal(label.textContent, 'channels.oauth.usageRefresh');
  } finally {
    for (const [name, descriptor] of previousGlobals) {
      if (descriptor) Object.defineProperty(global, name, descriptor);
      else delete global[name];
    }
  }
});

test('OAuth editor keeps credentials read-only and applies provider-specific controls', async () => {
  const elements = new Map();
  for (const id of [
    'codexCredentialReadOnlyNotice',
    'channelAPIKeyHeader',
    'channelAPIKeyTable',
    'channelApiKey',
    'importKeysBtn',
    'batchDeleteKeysBtn',
    'selectAllKeys',
    'codexCredentialTab',
    'codexCredentialContent',
    'codexCredentialRefreshButton',
    'channelCodexPlanBadge'
  ]) {
    elements.set(id, { hidden: false, required: true, value: 'must-not-remain' });
  }
  const strategyInputs = [{ disabled: false }, { disabled: false }];
  const rowKeyInput = { readOnly: false };
  const rowNoteInput = { readOnly: false };
  const rowDeleteButton = { hidden: false, disabled: false };
  const rowToggleButton = { hidden: false, disabled: false };
  const row = { draggable: true };
  const viewButtons = ['decoded', 'raw'].map(view => ({
    dataset: { codexCredentialView: view },
    classList: { toggle() {} },
    setAttribute() {}
  }));
  const previousDocument = global.document;
  global.document = {
    getElementById: id => elements.get(id) || null,
    querySelectorAll: selector => ({
      'input[name="keyStrategy"]': strategyInputs,
      '#inlineKeyTableBody .inline-key-input': [rowKeyInput],
      '#inlineKeyTableBody .inline-key-note-input': [rowNoteInput],
      '#inlineKeyTableBody [data-action="delete"], #inlineKeyTableBody [data-action="toggle-disabled"]': [rowDeleteButton, rowToggleButton],
      '#inlineKeyTableBody .inline-key-row': [row],
      '[data-codex-credential-view]': viewButtons
    })[selector] || []
  };
  try {
    const credential = { type: 'codex', access_token: 'at-secret', refresh_token: 'rt-secret', plan_type: 'plus' };
    const credentialInfo = {
      chatgpt_account_id: 'account-1',
      chatgpt_subscription_active_start: '2030-01-03T04:05:06Z',
      chatgpt_subscription_active_until: '2030-02-03T04:05:06Z',
      plan_type: 'plus'
    };
    applyChannelAuthEditorMode('codex_oauth', credential, {
      codex_subscription_active_until: '2030-02-03T04:05:06Z'
    }, credentialInfo);
    assert.equal(elements.get('codexCredentialReadOnlyNotice').hidden, false);
    assert.equal(elements.get('channelAPIKeyHeader').hidden, false);
    assert.equal(elements.get('channelAPIKeyTable').hidden, false);
    assert.equal(elements.get('channelApiKey').required, false);
    assert.equal(elements.get('channelApiKey').value, '');
    assert.equal(elements.get('importKeysBtn').disabled, true);
    assert.equal(elements.get('batchDeleteKeysBtn').disabled, true);
    assert.equal(elements.get('selectAllKeys').disabled, true);
    assert.equal(elements.get('codexCredentialTab').hidden, false);
    assert.equal(elements.get('channelCodexPlanBadge').hidden, false);
    assert.equal(elements.get('channelCodexPlanBadge').textContent, 'plus · 2030-02-03');
    const decodedCredential = { ...credential, id_token: credentialInfo };
    assert.equal(elements.get('codexCredentialContent').textContent, JSON.stringify(decodedCredential, null, 2));
    assert.ok(strategyInputs.every(input => input.disabled));
    assert.equal(rowKeyInput.readOnly, true);
    assert.equal(rowNoteInput.readOnly, true);
    assert.equal(rowDeleteButton.hidden, false);
    assert.equal(rowDeleteButton.disabled, true);
    assert.equal(rowToggleButton.hidden, false);
    assert.equal(rowToggleButton.disabled, true);
    assert.equal(row.draggable, false);

    let copiedCredential = '';
    await copyOAuthCredential(async text => { copiedCredential = text; });
    assert.equal(copiedCredential, JSON.stringify(decodedCredential, null, 2));

    setOAuthCredentialView('raw');
    assert.equal(elements.get('codexCredentialContent').textContent, JSON.stringify(credential, null, 2));

    const antigravityCredential = { type: 'antigravity', access_token: 'gravity-at', refresh_token: 'gravity-rt', project_id: 'project-1' };
    applyChannelAuthEditorMode('antigravity_oauth', antigravityCredential);
    assert.equal(elements.get('codexCredentialReadOnlyNotice').hidden, false);
    assert.equal(elements.get('channelApiKey').required, false);
    assert.equal(elements.get('codexCredentialTab').hidden, false);
    assert.equal(elements.get('channelCodexPlanBadge').hidden, true);
    assert.equal(elements.get('codexCredentialContent').textContent, JSON.stringify(antigravityCredential, null, 2));
    assert.ok(strategyInputs.every(input => input.disabled));

    const xaiCredential = {
      type: 'xai', auth_kind: 'oauth', access_token: 'xai-at', refresh_token: 'xai-rt', id_token: 'xai-id'
    };
    applyChannelAuthEditorMode('xai_oauth', xaiCredential, {
      xai_email: 'safe@example.com',
      xai_subscription_tier: 'supergrok',
      xai_entitlement_status: 'active'
    });
    assert.equal(elements.get('channelAPIKeyHeader').hidden, true);
    assert.equal(elements.get('channelAPIKeyTable').hidden, true);
    assert.equal(elements.get('codexCredentialTab').hidden, false);
    assert.equal(elements.get('codexCredentialRefreshButton').hidden, true);
    assert.equal(elements.get('codexCredentialContent').textContent, JSON.stringify(xaiCredential, null, 2));
    let copiedXAICredential = '';
    await copyOAuthCredential(async text => { copiedXAICredential = text; });
    assert.equal(copiedXAICredential, elements.get('codexCredentialContent').textContent);

    applyChannelAuthEditorMode('api_key');
    assert.equal(elements.get('codexCredentialReadOnlyNotice').hidden, true);
    assert.equal(elements.get('channelAPIKeyHeader').hidden, false);
    assert.equal(elements.get('channelAPIKeyTable').hidden, false);
    assert.equal(elements.get('channelApiKey').required, true);
    assert.equal(elements.get('importKeysBtn').disabled, false);
    assert.equal(elements.get('selectAllKeys').disabled, false);
    assert.equal(elements.get('codexCredentialTab').hidden, true);
    assert.equal(elements.get('codexCredentialRefreshButton').hidden, false);
    assert.equal(elements.get('channelCodexPlanBadge').hidden, true);
    assert.equal(elements.get('channelCodexPlanBadge').textContent, '');
    assert.equal(elements.get('codexCredentialContent').textContent, '');
    assert.ok(strategyInputs.every(input => !input.disabled));
    assert.equal(rowKeyInput.readOnly, false);
    assert.equal(rowNoteInput.readOnly, false);
    assert.equal(rowDeleteButton.hidden, false);
    assert.equal(rowDeleteButton.disabled, false);
    assert.equal(rowToggleButton.hidden, false);
    assert.equal(rowToggleButton.disabled, false);
    assert.equal(row.draggable, true);
  } finally {
    global.document = previousDocument;
  }
});
