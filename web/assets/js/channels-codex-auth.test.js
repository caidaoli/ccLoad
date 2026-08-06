const test = require('node:test');
const assert = require('node:assert/strict');

const {
  applyChannelAuthEditorMode,
  pollCodexOAuthStatus,
  showCodexOAuthSession,
  copyCodexOAuthLink,
  copyCodexCredential,
  cancelCodexOAuth,
  formatCodexPlanBadgeText,
  importCodexCredentials,
  getCodexUsageState,
  refreshCodexUsage,
  refreshCodexCredential,
  setCodexCredentialView,
  submitCodexOAuthCallback
} = require('./channels-codex-auth.js');

test('Codex plan badge appends the subscription calendar date', () => {
  assert.equal(formatCodexPlanBadgeText('plus', '2030-02-03T04:05:06Z'), 'plus · 2030-02-03');
  assert.equal(formatCodexPlanBadgeText('free', ''), 'free');
  assert.equal(formatCodexPlanBadgeText('', '2030-02-03T04:05:06Z'), '');
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

test('Codex OAuth session exposes the authorization link for copy and manual callback', async () => {
  const elements = new Map([
    ['codexOAuthDialog', { open: false, showModal() { this.open = true; } }],
    ['codexOAuthAuthorizationURL', { value: '' }],
    ['codexOAuthOpenLink', { href: '' }],
    ['codexOAuthCallbackURL', { value: '', setAttribute() {} }]
  ]);
  const previousDocument = global.document;
  global.document = { getElementById: id => elements.get(id) || null };
  try {
    assert.equal(showCodexOAuthSession({ url: 'https://auth.example/authorize?state=abc', state: 'abc' }), true);
    assert.equal(elements.get('codexOAuthDialog').open, true);
    assert.equal(elements.get('codexOAuthAuthorizationURL').value, 'https://auth.example/authorize?state=abc');
    assert.equal(elements.get('codexOAuthOpenLink').href, 'https://auth.example/authorize?state=abc');
    assert.equal(elements.get('codexOAuthCallbackURL').value, '');

    let copied = '';
    await copyCodexOAuthLink('https://auth.example/authorize?state=abc', async text => { copied = text; });
    assert.equal(copied, 'https://auth.example/authorize?state=abc');
  } finally {
    global.document = previousDocument;
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

test('Codex credential import submits every selected file in one request', async () => {
  const previousFormData = global.FormData;
  const previousDocument = global.document;
  const previousWindow = global.window;
  const previousReload = global.reloadChannelsList;
  class FakeFormData {
    constructor() { this.items = []; }
    append(name, value) { this.items.push([name, value]); }
  }
  global.FormData = FakeFormData;
  global.document = { getElementById: () => null };
  global.window = {
    t: (key, params) => `${key}:${params?.created ?? ''}:${params?.skipped ?? ''}:${params?.failed ?? ''}`,
    showSuccess() {},
    showError() {}
  };
  let reloads = 0;
  global.reloadChannelsList = async () => { reloads++; };
  const files = [{ name: 'one.json' }, { name: 'two.json' }];
  let captured;
  try {
    const result = await importCodexCredentials(files, null, async (url, options) => {
      captured = { url, options };
      return { created: 1, skipped: 1, failed: 0, results: [] };
    });

    assert.equal(result.created, 1);
    assert.equal(captured.url, '/admin/codex/credentials/import');
    assert.equal(captured.options.method, 'POST');
    assert.deepEqual(captured.options.body.items, [['files', files[0]], ['files', files[1]]]);
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
  const response = { codex_credential: { access_token: 'at-new' } };
  const result = await refreshCodexCredential(42, async (url, options) => {
    captured = { url, options };
    return response;
  });

  assert.equal(result, response);
  assert.deepEqual(captured, {
    url: '/admin/channels/42/codex-credential/refresh',
    options: { method: 'POST' }
  });
  await assert.rejects(() => refreshCodexCredential(0, async () => response), /saved Codex channel/);
});

test('Codex usage refresh stores one safe per-channel quota summary', async () => {
  const previousFilterChannels = global.filterChannels;
  let renders = 0;
  let captured;
  global.filterChannels = () => { renders++; };
  try {
    const result = await refreshCodexUsage(42, async (url, options) => {
      captured = { url, options };
      return {
        plan_type: 'pro',
        windows: [{
          limit_name: 'codex', kind: 'primary', used_percent: 29,
          remaining_percent: 71, limit_window_seconds: 604800, reset_at: 1786163635
        }]
      };
    });

    assert.equal(captured.url, '/admin/channels/42/codex-usage');
    assert.equal(captured.options.method, 'POST');
    assert.equal(result.windows[0].remaining_percent, 71);
    assert.deepEqual(getCodexUsageState(42), { status: 'ready', data: result });
    assert.equal(renders, 2);
  } finally {
    global.filterChannels = previousFilterChannels;
  }
});

test('failed Codex usage refresh remains retryable', async () => {
  const previousFilterChannels = global.filterChannels;
  global.filterChannels = () => {};
  try {
    await assert.rejects(
      refreshCodexUsage(43, async () => { throw new Error('quota unavailable'); }),
      /quota unavailable/
    );
    assert.deepEqual(getCodexUsageState(43), { status: 'error', error: 'quota unavailable' });
  } finally {
    global.filterChannels = previousFilterChannels;
  }
});

test('Codex editor shows AT in the normal key area and the full credential read-only', async () => {
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
    await copyCodexCredential(async text => { copiedCredential = text; });
    assert.equal(copiedCredential, JSON.stringify(decodedCredential, null, 2));

    setCodexCredentialView('raw');
    assert.equal(elements.get('codexCredentialContent').textContent, JSON.stringify(credential, null, 2));

    applyChannelAuthEditorMode('api_key');
    assert.equal(elements.get('codexCredentialReadOnlyNotice').hidden, true);
    assert.equal(elements.get('channelAPIKeyHeader').hidden, false);
    assert.equal(elements.get('channelAPIKeyTable').hidden, false);
    assert.equal(elements.get('channelApiKey').required, true);
    assert.equal(elements.get('importKeysBtn').disabled, false);
    assert.equal(elements.get('selectAllKeys').disabled, false);
    assert.equal(elements.get('codexCredentialTab').hidden, true);
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
