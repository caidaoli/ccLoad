const CODEX_OAUTH_POLL_INTERVAL_MS = 1000;
const CODEX_OAUTH_MAX_POLLS = 300;
let activeCodexOAuthFlow = null;
let codexOAuthStopPromise = null;
let currentOAuthCredentialJSON = '';
let currentOAuthCredential = null;
let currentOAuthCredentialInfo = null;
let currentOAuthCredentialView = 'decoded';
let oauthLoginDialogTrigger = null;
let oauthCredentialImportDialogTrigger = null;
const codexUsageStateByChannelID = new Map();
const OAUTH_PROVIDER_CONFIGS = Object.freeze({
  codex: Object.freeze({
    provider: 'codex', label: 'Codex', i18n: 'channels.codex',
    callbackPlaceholder: 'http://localhost:1455/auth/callback?code=...&state=...'
  }),
  antigravity: Object.freeze({
    provider: 'antigravity', label: 'Antigravity', i18n: 'channels.antigravity',
    callbackPlaceholder: 'http://localhost:51121/oauth-callback?code=...&state=...'
  })
});

function formatCodexPlanBadgeText(planType, subscriptionActiveUntil) {
  const plan = String(planType || '').trim();
  if (!plan) return '';
  const date = String(subscriptionActiveUntil || '').trim().match(/^(\d{4}-\d{2}-\d{2})/);
  return date ? `${plan} · ${date[1]}` : plan;
}

function buildOAuthCredentialView() {
  if (!currentOAuthCredential) return null;
  if (currentOAuthCredentialView !== 'decoded' || !currentOAuthCredentialInfo) {
    return currentOAuthCredential;
  }
  return { ...currentOAuthCredential, id_token: currentOAuthCredentialInfo };
}

function updateOAuthCredentialViewControls() {
  document.querySelectorAll('[data-codex-credential-view]').forEach(button => {
    const active = button.dataset.codexCredentialView === currentOAuthCredentialView;
    button.classList.toggle('active', active);
    button.setAttribute('aria-pressed', String(active));
  });
}

function renderCurrentOAuthCredential() {
  const content = document.getElementById('codexCredentialContent');
  const displayedCredential = buildOAuthCredentialView();
  currentOAuthCredentialJSON = displayedCredential ? JSON.stringify(displayedCredential, null, 2) : '';
  updateOAuthCredentialViewControls();
  if (!content) return;

  content.removeAttribute?.('data-highlighted');
  content.classList?.remove('hljs');
  content.textContent = currentOAuthCredentialJSON;
  if (!currentOAuthCredentialJSON || typeof window === 'undefined' || !window.hljs?.highlightElement) return;

  try {
    content.classList?.add('language-json');
    window.hljs.highlightElement(content);
  } catch (error) {
    console.warn('Failed to highlight Codex credential JSON', error);
  }
}

function renderOAuthCredential(credential, credentialInfo = null, view = 'decoded') {
  currentOAuthCredential = credential || null;
  currentOAuthCredentialInfo = credentialInfo || null;
  currentOAuthCredentialView = view === 'raw' ? 'raw' : 'decoded';
  renderCurrentOAuthCredential();
}

function setOAuthCredentialView(view) {
  currentOAuthCredentialView = view === 'raw' ? 'raw' : 'decoded';
  renderCurrentOAuthCredential();
}

async function copyOAuthCredential(copier = window.copyToClipboard) {
  if (!currentOAuthCredentialJSON) throw new Error('OAuth credential is empty');
  if (typeof copier !== 'function') throw new Error('Clipboard is unavailable');
  await copier(currentOAuthCredentialJSON);
}

function applyChannelAuthEditorMode(
  authType,
  credential = null,
  channel = null,
  credentialInfo = null,
  credentialView = 'decoded'
) {
  const codexOAuth = authType === 'codex_oauth';
  const oauth = codexOAuth || authType === 'antigravity_oauth';
  const notice = document.getElementById('codexCredentialReadOnlyNotice');
  const keyHeader = document.getElementById('channelAPIKeyHeader');
  const keyTable = document.getElementById('channelAPIKeyTable');
  const hiddenKey = document.getElementById('channelApiKey');
  const importButton = document.getElementById('importKeysBtn');
  const batchDeleteButton = document.getElementById('batchDeleteKeysBtn');
  const selectAll = document.getElementById('selectAllKeys');
  const credentialTab = document.getElementById('codexCredentialTab');
  const planBadge = document.getElementById('channelCodexPlanBadge');
  const planType = codexOAuth ? String(credential?.plan_type || channel?.codex_plan_type || '').trim() : '';
  const planBadgeText = codexOAuth
    ? formatCodexPlanBadgeText(planType, channel?.codex_subscription_active_until)
    : '';
  if (notice) notice.hidden = !oauth;
  if (planBadge) {
    planBadge.textContent = planBadgeText;
    planBadge.hidden = !planBadgeText;
  }
  if (keyHeader) keyHeader.hidden = false;
  if (keyTable) keyTable.hidden = false;
  if (hiddenKey) {
    hiddenKey.required = !oauth;
    if (oauth) hiddenKey.value = '';
  }
  if (importButton) importButton.disabled = oauth;
  if (batchDeleteButton) batchDeleteButton.disabled = oauth;
  if (selectAll) selectAll.disabled = oauth;
  if (credentialTab) credentialTab.hidden = !oauth;
  renderOAuthCredential(
    oauth ? credential : null,
    codexOAuth ? credentialInfo : null,
    credentialView
  );

  document.querySelectorAll('input[name="keyStrategy"]').forEach(input => {
    input.disabled = oauth;
  });
  document.querySelectorAll('#inlineKeyTableBody .inline-key-input').forEach(input => {
    input.readOnly = oauth;
  });
  document.querySelectorAll('#inlineKeyTableBody .inline-key-note-input').forEach(input => {
    input.readOnly = oauth;
  });
  document.querySelectorAll('#inlineKeyTableBody [data-action="delete"], #inlineKeyTableBody [data-action="toggle-disabled"]').forEach(button => {
    button.hidden = false;
    button.disabled = oauth;
  });
  document.querySelectorAll('#inlineKeyTableBody .inline-key-row').forEach(row => {
    row.draggable = !oauth;
  });
}

function oauthProviderConfig(provider = 'codex') {
  return OAUTH_PROVIDER_CONFIGS[provider] || OAUTH_PROVIDER_CONFIGS.codex;
}

function setCodexAuthStatus(message, kind = '') {
  const status = document.getElementById('codexAuthStatus');
  if (!status) return;
  status.textContent = message || '';
  status.hidden = !message;
  status.dataset.kind = kind;
}

function codexOAuthDelay(ms) {
  return new Promise(resolve => setTimeout(resolve, ms));
}

function setCodexOAuthDialogStatus(message, kind = '') {
  const status = document.getElementById('oauthLoginDialogStatus');
  if (!status) return;
  status.textContent = message || '';
  status.hidden = !message;
  status.dataset.kind = kind;
}

function setOAuthCredentialImportStatus(message, kind = '') {
  const status = document.getElementById('oauthCredentialImportStatus');
  if (!status) return;
  status.textContent = message || '';
  status.hidden = !message;
  status.dataset.kind = kind;
}

function openOAuthLoginDialog(trigger = null) {
  const dialog = document.getElementById('oauthLoginDialog');
  const providerSelect = document.getElementById('oauthProviderSelect');
  const authorizeButton = document.getElementById('oauthAuthorizeButton');
  const sessionFields = document.getElementById('oauthSessionFields');
  const authorizationURL = document.getElementById('oauthAuthorizationURL');
  const openLink = document.getElementById('oauthOpenLink');
  const callbackURL = document.getElementById('oauthCallbackURL');
  if (!dialog || !providerSelect || !authorizeButton || !sessionFields || !authorizationURL || !openLink || !callbackURL) {
    return false;
  }

  oauthLoginDialogTrigger = trigger;
  providerSelect.value = 'codex';
  providerSelect.disabled = false;
  authorizeButton.disabled = false;
  sessionFields.hidden = true;
  authorizationURL.value = '';
  openLink.removeAttribute?.('href');
  callbackURL.value = '';
  callbackURL.removeAttribute?.('aria-invalid');
  setCodexAuthStatus('');
  setCodexOAuthDialogStatus('');
  if (!dialog.open && typeof dialog.showModal === 'function') dialog.showModal();
  providerSelect.focus?.();
  return true;
}

function closeOAuthLoginDialogElement() {
  const dialog = document.getElementById('oauthLoginDialog');
  if (dialog?.open) dialog.close();
  const trigger = oauthLoginDialogTrigger;
  oauthLoginDialogTrigger = null;
  trigger?.focus?.();
}

function openOAuthCredentialImportDialog(trigger = null) {
  const dialog = document.getElementById('oauthCredentialImportDialog');
  const providerSelect = document.getElementById('oauthImportProviderSelect');
  const priorityIncrementSelect = document.getElementById('oauthImportPriorityIncrement');
  const input = document.getElementById('oauthCredentialImportInput');
  if (!dialog || !providerSelect || !priorityIncrementSelect || !input) return false;

  oauthCredentialImportDialogTrigger = trigger;
  providerSelect.value = 'auto';
  providerSelect.disabled = false;
  priorityIncrementSelect.value = '10';
  priorityIncrementSelect.disabled = false;
  input.value = '';
  input.removeAttribute?.('aria-invalid');
  setCodexAuthStatus('');
  setOAuthCredentialImportStatus('');
  if (!dialog.open && typeof dialog.showModal === 'function') dialog.showModal();
  providerSelect.focus?.();
  return true;
}

function closeOAuthCredentialImportDialog() {
  const dialog = document.getElementById('oauthCredentialImportDialog');
  if (dialog?.open) dialog.close();
  const trigger = oauthCredentialImportDialogTrigger;
  oauthCredentialImportDialogTrigger = null;
  trigger?.focus?.();
}

function showOAuthSession(session, provider = 'codex') {
  if (!session?.url || !session?.state) return false;
  const config = oauthProviderConfig(provider);
  const dialog = document.getElementById('oauthLoginDialog');
  const providerSelect = document.getElementById('oauthProviderSelect');
  const sessionFields = document.getElementById('oauthSessionFields');
  const sessionDescription = document.getElementById('oauthSessionDescription');
  const authorizationURL = document.getElementById('oauthAuthorizationURL');
  const openLink = document.getElementById('oauthOpenLink');
  const callbackURL = document.getElementById('oauthCallbackURL');
  if (!dialog || !providerSelect || !sessionFields || !authorizationURL || !openLink || !callbackURL) return false;

  providerSelect.value = config.provider;
  providerSelect.disabled = true;
  sessionFields.hidden = false;
  if (sessionDescription) sessionDescription.textContent = window.t('channels.oauth.sessionDescription');
  callbackURL.placeholder = config.callbackPlaceholder;
  authorizationURL.value = session.url;
  openLink.href = session.url;
  callbackURL.value = '';
  callbackURL.removeAttribute?.('aria-invalid');
  setCodexOAuthDialogStatus('');
  if (!dialog.open && typeof dialog.showModal === 'function') dialog.showModal();
  authorizationURL.focus?.();
  authorizationURL.select?.();
  return true;
}

async function copyCodexOAuthLink(url, copier = window.copyToClipboard) {
  const authorizationURL = String(url || '').trim();
  if (!authorizationURL) throw new Error('OAuth authorization URL is empty');
  if (typeof copier !== 'function') throw new Error('Clipboard is unavailable');
  await copier(authorizationURL);
}

async function submitOAuthCallback(provider, callbackURL, fetcher = fetchDataWithAuth) {
  const config = oauthProviderConfig(provider);
  const normalizedURL = String(callbackURL || '').trim();
  if (!normalizedURL) throw new Error(`${config.label} OAuth callback URL is required`);
  return fetcher(`/admin/${config.provider}/oauth/callback`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ callback_url: normalizedURL })
  });
}

async function submitCodexOAuthCallback(callbackURL, fetcher = fetchDataWithAuth) {
  return submitOAuthCallback('codex', callbackURL, fetcher);
}

async function submitAntigravityOAuthCallback(callbackURL, fetcher = fetchDataWithAuth) {
  return submitOAuthCallback('antigravity', callbackURL, fetcher);
}

async function cancelOAuth(provider, state, fetcher = fetchDataWithAuth) {
  const config = oauthProviderConfig(provider);
  const normalizedState = String(state || '').trim();
  if (!normalizedState) throw new Error(`${config.label} OAuth state is required`);
  return fetcher(`/admin/${config.provider}/oauth/cancel`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ state: normalizedState })
  });
}

async function cancelCodexOAuth(state, fetcher = fetchDataWithAuth) {
  return cancelOAuth('codex', state, fetcher);
}

async function cancelAntigravityOAuth(state, fetcher = fetchDataWithAuth) {
  return cancelOAuth('antigravity', state, fetcher);
}

async function pollOAuthStatus(provider, state, options = {}) {
  const config = oauthProviderConfig(provider);
  const fetchStatus = options.fetchStatus || (url => fetchDataWithAuth(url));
  const delay = options.delay || codexOAuthDelay;
  const maxPolls = options.maxPolls || CODEX_OAUTH_MAX_POLLS;
  const interval = options.interval ?? CODEX_OAUTH_POLL_INTERVAL_MS;
  for (let attempt = 0; attempt < maxPolls; attempt++) {
    const status = await fetchStatus(`/admin/${config.provider}/oauth/status?state=${encodeURIComponent(state)}`);
    if (status?.status === 'complete') return status;
    if (status?.status === 'cancelled') throw new Error(window.t(`${config.i18n}.oauthCancelled`));
    if (status?.status === 'error') throw new Error(status.error || window.t(`${config.i18n}.oauthFailed`));
    await delay(interval);
  }
  throw new Error(window.t(`${config.i18n}.oauthTimedOut`));
}

async function pollCodexOAuthStatus(state, options = {}) {
  return pollOAuthStatus('codex', state, options);
}

async function pollAntigravityOAuthStatus(state, options = {}) {
  return pollOAuthStatus('antigravity', state, options);
}

async function startOAuth(provider, button) {
  const config = oauthProviderConfig(provider);
  let resolveReady;
  let rejectReady;
  const ready = new Promise((resolve, reject) => {
    resolveReady = resolve;
    rejectReady = reject;
  });
  ready.catch(() => {});
  const flow = { state: '', provider: config.provider, button, cancelling: false, ready, readySettled: false };
  activeCodexOAuthFlow = flow;
  try {
    if (button) button.disabled = true;
    setCodexAuthStatus(window.t(`${config.i18n}.oauthStarting`));
    const session = await fetchDataWithAuth(`/admin/${config.provider}/oauth/start`, { method: 'POST' });
    if (!session?.url || !session?.state) throw new Error(window.t(`${config.i18n}.oauthFailed`));
    flow.state = session.state;
    flow.readySettled = true;
    resolveReady(session.state);
    if (flow.cancelling) return null;
    if (!showOAuthSession(session, config.provider)) throw new Error(window.t(`${config.i18n}.oauthFailed`));
    setCodexAuthStatus(window.t(`${config.i18n}.oauthWaiting`));
    setCodexOAuthDialogStatus(window.t(`${config.i18n}.oauthWaiting`));
    const result = await pollOAuthStatus(config.provider, session.state);
    if (flow.cancelling || activeCodexOAuthFlow !== flow) return null;
    closeOAuthLoginDialogElement();
    setCodexAuthStatus(window.t(`${config.i18n}.oauthComplete`), 'success');
    if (window.showSuccess) window.showSuccess(window.t(`${config.i18n}.oauthComplete`));
    await reloadChannelsList();
    return result;
  } catch (error) {
    if (!flow.readySettled) {
      flow.readySettled = true;
      rejectReady(error);
    }
    if (flow.cancelling) return null;
    const message = error?.message || window.t(`${config.i18n}.oauthFailed`);
    setCodexAuthStatus(message, 'error');
    setCodexOAuthDialogStatus(message, 'error');
    if (window.showError) window.showError(message);
    return null;
  } finally {
    if (activeCodexOAuthFlow === flow) {
      activeCodexOAuthFlow = null;
      if (button) button.disabled = false;
    }
  }
}

async function stopActiveCodexOAuth(options = {}) {
  const closeDialog = options.closeDialog !== false;
  if (codexOAuthStopPromise) return codexOAuthStopPromise;

  const operation = (async () => {
    const flow = activeCodexOAuthFlow;
    if (flow) {
      flow.cancelling = true;
      setCodexOAuthDialogStatus(window.t('channels.oauth.cancelling'));
      if (!flow.state && flow.ready) {
        try {
          await flow.ready;
        } catch {
          if (closeDialog) {
            closeOAuthLoginDialogElement();
          }
          return;
        }
      }
      try {
        await cancelOAuth(flow.provider, flow.state);
      } catch (error) {
        flow.cancelling = false;
        throw error;
      }
      if (activeCodexOAuthFlow === flow) activeCodexOAuthFlow = null;
      if (flow.button) flow.button.disabled = false;
    }
    if (closeDialog) {
      closeOAuthLoginDialogElement();
      setCodexAuthStatus('');
      setCodexOAuthDialogStatus('');
    }
  })();

  codexOAuthStopPromise = operation;
  try {
    return await operation;
  } finally {
    if (codexOAuthStopPromise === operation) codexOAuthStopPromise = null;
  }
}

async function closeOAuthLoginDialog() {
  try {
    await stopActiveCodexOAuth({ closeDialog: true });
  } catch (error) {
    setCodexOAuthDialogStatus(error?.message || window.t('channels.oauth.cancelFailed'), 'error');
  }
}

async function restartOAuth(provider, button) {
  const config = oauthProviderConfig(provider);
  try {
    if (button) button.disabled = true;
    await stopActiveCodexOAuth({ closeDialog: false });
    setCodexOAuthDialogStatus(window.t(`${config.i18n}.oauthRestarting`));
    const authorizeButton = document.getElementById('oauthAuthorizeButton');
    const completion = startOAuth(config.provider, authorizeButton);
    const newFlow = activeCodexOAuthFlow;
    if (newFlow?.ready) await newFlow.ready;
    void completion;
  } catch (error) {
    setCodexOAuthDialogStatus(error?.message || window.t(`${config.i18n}.oauthCancelFailed`), 'error');
  } finally {
    if (button) button.disabled = false;
  }
}

async function importOAuthCredentials(
  files,
  button,
  fetcher = fetchDataWithAuth,
  provider = 'auto',
  priorityIncrement = 10
) {
  const selectedFiles = Array.from(files || []).filter(Boolean);
  if (selectedFiles.length === 0) return null;
  const formData = new FormData();
  selectedFiles.forEach(file => formData.append('files', file));
  formData.append('provider', provider);
  formData.append('priority_increment', String(priorityIncrement));
  try {
    if (button) button.disabled = true;
    const importingMessage = window.t('channels.oauth.importing', { count: selectedFiles.length });
    setCodexAuthStatus(importingMessage);
    setOAuthCredentialImportStatus(importingMessage);
    const result = await fetcher('/admin/oauth/credentials/import', {
      method: 'POST',
      body: formData
    });
    const created = Number(result?.created) || 0;
    const skipped = Number(result?.skipped) || 0;
    const failed = Number(result?.failed) || 0;
    const message = window.t('channels.oauth.importSummary', { created, skipped, failed });
    const kind = failed > 0 ? 'error' : 'success';
    setCodexAuthStatus(message, kind);
    setOAuthCredentialImportStatus(message, kind);
    if (failed > 0) {
      if (window.showError) window.showError(message);
    } else if (window.showSuccess) {
      window.showSuccess(message);
    }
    if (created > 0) await reloadChannelsList();
    return result;
  } catch (error) {
    const message = error?.message || window.t('channels.oauth.importFailed');
    setCodexAuthStatus(message, 'error');
    setOAuthCredentialImportStatus(message, 'error');
    if (window.showError) window.showError(message);
    return null;
  } finally {
    if (button) button.disabled = false;
  }
}

async function refreshOAuthCredential(channelID, fetcher = fetchDataWithAuth, authType = 'codex_oauth') {
  const numericID = Number(channelID);
  const antigravity = authType === 'antigravity_oauth';
  if (!Number.isInteger(numericID) || numericID <= 0) {
    throw new Error(`A saved ${antigravity ? 'Antigravity' : 'Codex'} channel is required`);
  }
  const resource = antigravity ? 'antigravity-credential' : 'codex-credential';
  return fetcher(`/admin/channels/${numericID}/${resource}/refresh`, { method: 'POST' });
}

function getCodexUsageState(channelID) {
  const numericID = Number(channelID);
  if (!Number.isInteger(numericID) || numericID <= 0) return null;
  return codexUsageStateByChannelID.get(numericID) || null;
}

function rerenderCodexUsage() {
  if (typeof filterChannels === 'function') filterChannels();
}

async function refreshCodexUsage(channelID, fetcher = fetchDataWithAuth) {
  const numericID = Number(channelID);
  if (!Number.isInteger(numericID) || numericID <= 0) {
    throw new Error('A saved Codex channel is required');
  }
  codexUsageStateByChannelID.set(numericID, { status: 'loading' });
  rerenderCodexUsage();
  try {
    const result = await fetcher(`/admin/channels/${numericID}/codex-usage`, { method: 'POST' });
    if (!result || !Array.isArray(result.windows)) {
      throw new Error(window.t('channels.codex.usageInvalid'));
    }
    codexUsageStateByChannelID.set(numericID, { status: 'ready', data: result });
    rerenderCodexUsage();
    return result;
  } catch (error) {
    const message = error?.message || window.t('channels.codex.usageFailed');
    codexUsageStateByChannelID.set(numericID, { status: 'error', error: message });
    rerenderCodexUsage();
    throw error;
  }
}

function setupOAuthActions() {
  const loginButton = document.getElementById('oauthLoginBtn');
  const loginDialog = document.getElementById('oauthLoginDialog');
  const loginForm = document.getElementById('oauthLoginForm');
  const providerSelect = document.getElementById('oauthProviderSelect');
  const authorizeButton = document.getElementById('oauthAuthorizeButton');
  const sessionFields = document.getElementById('oauthSessionFields');
  const copyButton = document.getElementById('oauthCopyLink');
  const restartButton = document.getElementById('oauthRestart');
  const authorizationURL = document.getElementById('oauthAuthorizationURL');
  const callbackForm = document.getElementById('oauthCallbackForm');
  const callbackURL = document.getElementById('oauthCallbackURL');
  const callbackButton = document.getElementById('oauthSubmitCallback');
  const importButton = document.getElementById('oauthCredentialImportBtn');
  const importDialog = document.getElementById('oauthCredentialImportDialog');
  const importForm = document.getElementById('oauthCredentialImportForm');
  const importProviderSelect = document.getElementById('oauthImportProviderSelect');
  const importPriorityIncrementSelect = document.getElementById('oauthImportPriorityIncrement');
  const importInput = document.getElementById('oauthCredentialImportInput');
  const importSubmitButton = document.getElementById('oauthCredentialImportSubmit');
  const credentialCopyButton = document.getElementById('codexCredentialCopyButton');
  const credentialRefreshButton = document.getElementById('codexCredentialRefreshButton');

  if (loginButton && !loginButton.dataset.bound) {
    loginButton.addEventListener('click', () => openOAuthLoginDialog(loginButton));
    loginButton.dataset.bound = '1';
  }
  if (loginForm && providerSelect && authorizeButton && !loginForm.dataset.bound) {
    loginForm.addEventListener('submit', async event => {
      event.preventDefault();
      if (activeCodexOAuthFlow) return;
      providerSelect.disabled = true;
      await startOAuth(oauthProviderConfig(providerSelect.value).provider, authorizeButton);
      if (loginDialog?.open && sessionFields?.hidden) providerSelect.disabled = false;
    });
    loginForm.dataset.bound = '1';
  }
  if (copyButton && authorizationURL && !copyButton.dataset.bound) {
    copyButton.addEventListener('click', async () => {
      try {
        await copyCodexOAuthLink(authorizationURL.value);
        setCodexOAuthDialogStatus(window.t('channels.oauth.linkCopied'), 'success');
      } catch (error) {
        setCodexOAuthDialogStatus(error?.message || window.t('channels.oauth.copyFailed'), 'error');
      }
    });
    copyButton.dataset.bound = '1';
  }
  if (restartButton && !restartButton.dataset.bound) {
    restartButton.addEventListener('click', () => restartOAuth(
      activeCodexOAuthFlow?.provider || oauthProviderConfig(providerSelect?.value).provider,
      restartButton
    ));
    restartButton.dataset.bound = '1';
  }
  if (callbackForm && callbackURL && !callbackForm.dataset.bound) {
    callbackForm.addEventListener('submit', async event => {
      event.preventDefault();
      const value = callbackURL.value.trim();
      if (!value) {
        callbackURL.setAttribute('aria-invalid', 'true');
        callbackURL.focus();
        setCodexOAuthDialogStatus(window.t('channels.oauth.callbackRequired'), 'error');
        return;
      }
      callbackURL.removeAttribute('aria-invalid');
      try {
        if (callbackButton) callbackButton.disabled = true;
        setCodexOAuthDialogStatus(window.t('channels.oauth.callbackSubmitting'));
        const provider = activeCodexOAuthFlow?.provider || oauthProviderConfig(providerSelect?.value).provider;
        await submitOAuthCallback(provider, value);
        setCodexOAuthDialogStatus(window.t('channels.oauth.callbackAccepted'), 'success');
      } catch (error) {
        callbackURL.setAttribute('aria-invalid', 'true');
        callbackURL.focus();
        const config = oauthProviderConfig(activeCodexOAuthFlow?.provider || providerSelect?.value);
        setCodexOAuthDialogStatus(error?.message || window.t(`${config.i18n}.oauthFailed`), 'error');
      } finally {
        if (callbackButton) callbackButton.disabled = false;
      }
    });
    callbackForm.dataset.bound = '1';
  }
  document.querySelectorAll('[data-action="close-oauth-login"]').forEach(closeButton => {
    if (closeButton.dataset.bound) return;
    closeButton.addEventListener('click', () => closeOAuthLoginDialog());
    closeButton.dataset.bound = '1';
  });
  if (loginDialog && !loginDialog.dataset.cancelBound) {
    loginDialog.addEventListener('cancel', event => {
      event.preventDefault();
      void closeOAuthLoginDialog();
    });
    loginDialog.dataset.cancelBound = '1';
  }
  if (importButton && !importButton.dataset.bound) {
    importButton.addEventListener('click', () => openOAuthCredentialImportDialog(importButton));
    importButton.dataset.bound = '1';
  }
  if (importForm && importProviderSelect && importPriorityIncrementSelect && importInput && importSubmitButton && !importForm.dataset.bound) {
    importForm.addEventListener('submit', async event => {
      event.preventDefault();
      importProviderSelect.disabled = true;
      importPriorityIncrementSelect.disabled = true;
      const result = await importOAuthCredentials(
        importInput.files,
        importSubmitButton,
        fetchDataWithAuth,
        importProviderSelect.value,
        Number(importPriorityIncrementSelect.value)
      );
      importProviderSelect.disabled = false;
      importPriorityIncrementSelect.disabled = false;
      if (result) closeOAuthCredentialImportDialog();
    });
    importForm.dataset.bound = '1';
  }
  document.querySelectorAll('[data-action="close-oauth-import"]').forEach(closeButton => {
    if (closeButton.dataset.bound) return;
    closeButton.addEventListener('click', () => closeOAuthCredentialImportDialog());
    closeButton.dataset.bound = '1';
  });
  if (importDialog && !importDialog.dataset.cancelBound) {
    importDialog.addEventListener('cancel', event => {
      event.preventDefault();
      closeOAuthCredentialImportDialog();
    });
    importDialog.dataset.cancelBound = '1';
  }
  if (credentialCopyButton && !credentialCopyButton.dataset.bound) {
    credentialCopyButton.addEventListener('click', async () => {
      try {
        await copyOAuthCredential();
        if (window.showSuccess) window.showSuccess(window.t('channels.codex.credentialCopied'));
      } catch (error) {
        const message = error?.message || window.t('channels.codex.credentialCopyFailed');
        if (window.showError) window.showError(message);
      }
    });
    credentialCopyButton.dataset.bound = '1';
  }
  document.querySelectorAll('[data-codex-credential-view]').forEach(viewButton => {
    if (viewButton.dataset.bound) return;
    viewButton.addEventListener('click', () => setOAuthCredentialView(viewButton.dataset.codexCredentialView));
    viewButton.dataset.bound = '1';
  });
  if (credentialRefreshButton && !credentialRefreshButton.dataset.bound) {
    credentialRefreshButton.addEventListener('click', async () => {
      const previousView = currentOAuthCredentialView;
      try {
        credentialRefreshButton.disabled = true;
        const authType = editingChannelAuthType === 'antigravity_oauth' ? 'antigravity_oauth' : 'codex_oauth';
        const antigravity = authType === 'antigravity_oauth';
        const credentialI18n = antigravity ? 'channels.antigravity' : 'channels.codex';
        const result = await refreshOAuthCredential(editingChannelId, fetchDataWithAuth, authType);
        const credential = result?.oauth_credential;
        if (!credential?.access_token) throw new Error(window.t(`${credentialI18n}.credentialRefreshInvalid`));

        if (typeof setInlineKeyTableDataFromAPI === 'function' && typeof renderInlineKeyTable === 'function') {
          setInlineKeyTableDataFromAPI([{
            channel_id: editingChannelId,
            key_index: 0,
            api_key: credential.access_token,
            note: antigravity ? 'Antigravity OAuth AT' : 'Codex OAuth AT',
            key_strategy: 'sequential'
          }]);
          inlineKeyVisible = true;
          renderInlineKeyTable();
        }
        applyChannelAuthEditorMode(authType, credential, result, result.oauth_credential_info, previousView);
        await reloadChannelsList();
        if (window.showSuccess) window.showSuccess(window.t(`${credentialI18n}.credentialRefreshed`));
      } catch (error) {
        const credentialI18n = editingChannelAuthType === 'antigravity_oauth' ? 'channels.antigravity' : 'channels.codex';
        const message = error?.message || window.t(`${credentialI18n}.credentialRefreshFailed`);
        if (window.showError) window.showError(message);
      } finally {
        credentialRefreshButton.disabled = false;
      }
    });
    credentialRefreshButton.dataset.bound = '1';
  }
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    applyChannelAuthEditorMode,
    cancelAntigravityOAuth,
    cancelCodexOAuth,
    copyOAuthCredential,
    copyCodexOAuthLink,
    formatCodexPlanBadgeText,
    getCodexUsageState,
    importOAuthCredentials,
    openOAuthCredentialImportDialog,
    openOAuthLoginDialog,
    pollAntigravityOAuthStatus,
    pollCodexOAuthStatus,
    refreshOAuthCredential,
    refreshCodexUsage,
    renderOAuthCredential,
    setOAuthCredentialView,
    setupOAuthActions,
    showOAuthSession,
    submitAntigravityOAuthCallback,
    submitCodexOAuthCallback
  };
}
