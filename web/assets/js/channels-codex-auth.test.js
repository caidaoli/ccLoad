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
  const previousDocument = global.document;
  global.document = {
    getElementById: id => elements.get(id) || null,
    querySelectorAll: selector => ({
      'input[name="keyStrategy"]': strategyInputs,
      '#inlineKeyTableBody .inline-key-input': [rowKeyInput],
      '#inlineKeyTableBody .inline-key-note-input': [rowNoteInput],
      '#inlineKeyTableBody [data-action="delete"], #inlineKeyTableBody [data-action="toggle-disabled"]': [rowDeleteButton, rowToggleButton],
      '#inlineKeyTableBody .inline-key-row': [row]
    })[selector] || []
  };
	try {
		const credential = { type: 'codex', access_token: 'at-secret', refresh_token: 'rt-secret', plan_type: 'plus' };
		applyChannelAuthEditorMode('codex_oauth', credential, {
		  codex_subscription_active_until: '2030-02-03T04:05:06Z'
		});
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
    assert.equal(elements.get('codexCredentialContent').textContent, JSON.stringify(credential, null, 2));
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
    assert.equal(copiedCredential, JSON.stringify(credential, null, 2));

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
