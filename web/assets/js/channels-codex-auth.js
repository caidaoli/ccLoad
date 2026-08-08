const CODEX_OAUTH_POLL_INTERVAL_MS = 1000;
const CODEX_OAUTH_MAX_POLLS = 300;
const OAUTH_CREDENTIAL_IMPORT_POLL_INTERVAL_MS = 500;
const OAUTH_CREDENTIAL_IMPORT_MAX_NETWORK_ERRORS = 10;
class OAuthCredentialImportResponseError extends Error {
  constructor(message, status = 0) {
    super(message);
    this.status = status;
  }
}
let activeCodexOAuthFlow = null;
let codexOAuthStopPromise = null;
let activeXAIImportFlow = null;
let xaiImportStopPromise = null;
let oauthPagehideBound = false;
let currentOAuthCredentialJSON = '';
let currentOAuthCredential = null;
let currentOAuthCredentialInfo = null;
let currentOAuthCredentialView = 'decoded';
let oauthLoginDialogTrigger = null;
let oauthCredentialImportDialogTrigger = null;
const oauthUsageStateByChannelID = new Map();
const OAUTH_PROVIDER_CONFIGS = Object.freeze({
  codex: Object.freeze({
    provider: 'codex', label: 'Codex', i18n: 'channels.codex',
    callbackPlaceholder: 'http://localhost:1455/auth/callback?code=...&state=...'
  }),
  antigravity: Object.freeze({
    provider: 'antigravity', label: 'Antigravity', i18n: 'channels.antigravity',
    callbackPlaceholder: 'http://localhost:51121/oauth-callback?code=...&state=...'
  }),
  xai: Object.freeze({
    provider: 'xai', label: 'xAI', i18n: 'channels.xai',
    callbackPlaceholder: 'http://127.0.0.1:56121/callback?code=...&state=...'
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
  const xaiOAuth = authType === 'xai_oauth';
  const credentialVisible = codexOAuth || authType === 'antigravity_oauth' || xaiOAuth;
  const oauth = credentialVisible;
  const notice = document.getElementById('codexCredentialReadOnlyNotice');
  const keyHeader = document.getElementById('channelAPIKeyHeader');
  const keyTable = document.getElementById('channelAPIKeyTable');
  const hiddenKey = document.getElementById('channelApiKey');
  const importButton = document.getElementById('importKeysBtn');
  const batchDeleteButton = document.getElementById('batchDeleteKeysBtn');
  const selectAll = document.getElementById('selectAllKeys');
  const credentialTab = document.getElementById('codexCredentialTab');
  const credentialRefreshButton = document.getElementById('codexCredentialRefreshButton');
  const planBadge = document.getElementById('channelCodexPlanBadge');
  const planType = codexOAuth
    ? String(credential?.plan_type || channel?.codex_plan_type || '').trim()
    : String(channel?.xai_subscription_tier || '').trim();
  const planBadgeText = codexOAuth
    ? formatCodexPlanBadgeText(planType, channel?.codex_subscription_active_until)
    : (xaiOAuth ? planType : '');
  if (notice) {
    const noticeKey = xaiOAuth ? 'channels.xai.editorReadOnly' : 'channels.oauthCredentialReadOnly';
    notice.hidden = !oauth;
    notice.setAttribute?.('data-i18n', noticeKey);
    if (oauth && typeof window !== 'undefined' && typeof window.t === 'function') {
      notice.textContent = window.t(noticeKey);
    }
  }
  if (planBadge) {
    planBadge.textContent = planBadgeText;
    planBadge.hidden = !planBadgeText;
  }
  if (keyHeader) keyHeader.hidden = xaiOAuth;
  if (keyTable) keyTable.hidden = xaiOAuth;
  if (hiddenKey) {
    hiddenKey.required = !oauth;
    if (oauth) hiddenKey.value = '';
  }
  if (importButton) importButton.disabled = oauth;
  if (batchDeleteButton) batchDeleteButton.disabled = oauth;
  if (selectAll) selectAll.disabled = oauth;
  if (credentialTab) credentialTab.hidden = !credentialVisible;
  if (credentialRefreshButton) credentialRefreshButton.hidden = xaiOAuth;
  renderOAuthCredential(
    credentialVisible ? credential : null,
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

function codexOAuthDelay(ms, signal = undefined) {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(new DOMException('Aborted', 'AbortError'));
      return;
    }
    const timer = setTimeout(() => {
      signal?.removeEventListener?.('abort', onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(timer);
      reject(new DOMException('Aborted', 'AbortError'));
    };
    signal?.addEventListener?.('abort', onAbort, { once: true });
  });
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

function getOAuthCredentialImportProgressElements(prefix = 'oauthCredentialImport') {
  return {
    container: document.getElementById(`${prefix}Progress`),
    progress: document.getElementById(`${prefix}ProgressBar`),
    counter: document.getElementById(`${prefix}ProgressCounter`),
    detail: document.getElementById(`${prefix}ProgressDetail`),
    counts: document.getElementById(`${prefix}ProgressCounts`),
    errors: document.getElementById(`${prefix}Errors`),
    errorList: document.getElementById(`${prefix}ErrorList`)
  };
}

function resetOAuthCredentialImportProgress(prefix = 'oauthCredentialImport') {
  const { container, progress, counter, detail, counts, errors, errorList } =
    getOAuthCredentialImportProgressElements(prefix);
  if (container) container.hidden = true;
  if (progress) {
    progress.max = 1;
    progress.value = 0;
  }
  if (counter) counter.textContent = '';
  if (detail) detail.textContent = '';
  if (counts) counts.textContent = '';
  if (errors) errors.hidden = true;
  errorList?.replaceChildren();
}

function appendOAuthCredentialImportIssue(result, prefix = 'oauthCredentialImport') {
  if (!result || (result.status !== 'failed' && result.status !== 'skipped')) return;
  const { errors, errorList } = getOAuthCredentialImportProgressElements(prefix);
  if (!errors || !errorList) return;
  const reason = result.error || (result.channel_name
    ? window.t('channels.oauth.progressSkippedExisting', { channel: result.channel_name })
    : window.t('channels.oauth.progressSkippedUnknown'));
  const item = document.createElement('li');
  item.textContent = window.t('channels.oauth.progressErrorItem', {
    file: result.file_name || '',
    error: reason
  });
  errorList.append(item);
  errors.hidden = false;
}

function updateOAuthCredentialImportProgress(event, prefix = 'oauthCredentialImport') {
  if (!event || typeof event !== 'object') return;
  const { container, progress, counter, detail, counts } = getOAuthCredentialImportProgressElements(prefix);
  const total = Math.max(0, Number(event.total) || 0);
  const processed = Math.min(total, Math.max(0, Number(event.processed) || 0));
  const created = Math.max(0, Number(event.created) || 0);
  const skipped = Math.max(0, Number(event.skipped) || 0);
  const failed = Math.max(0, Number(event.failed) || 0);

  if (container) container.hidden = false;
  if (progress) {
    progress.max = Math.max(1, total);
    progress.value = processed;
  }
  if (counter) {
    counter.textContent = window.t('channels.oauth.progressCounter', { processed, total });
  }
  if (counts) {
    counts.textContent = window.t('channels.oauth.progressCounts', { created, skipped, failed });
  }
  if (!detail) return;
  switch (event.event) {
    case 'preparing':
      detail.textContent = window.t('channels.oauth.progressPreparing', { count: event.file_count || 0 });
      break;
    case 'start':
      detail.textContent = window.t('channels.oauth.progressStarting', { total });
      break;
    case 'processing':
      detail.textContent = window.t('channels.oauth.progressProcessing', { file: event.file_name || '' });
      break;
    case 'progress': {
      const resultStatus = event.result?.status || 'failed';
      appendOAuthCredentialImportIssue(event.result, prefix);
      detail.textContent = window.t('channels.oauth.progressProcessed', {
        file: event.result?.file_name || event.file_name || '',
        status: window.t(`channels.oauth.progressStatus.${resultStatus}`)
      });
      break;
    }
    case 'reconnecting':
      detail.textContent = window.t('channels.oauth.progressReconnecting');
      break;
    case 'complete':
      detail.textContent = window.t('channels.oauth.progressComplete');
      break;
    default:
      break;
  }
}

function openOAuthLoginDialog(trigger = null) {
  const dialog = document.getElementById('oauthLoginDialog');
  const providerSelect = document.getElementById('oauthProviderSelect');
  const xaiMethod = document.getElementById('xaiOAuthMethod');
  const xaiCredentialValues = document.getElementById('xaiCredentialValues');
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
  resetXAIOAuthDialog();
  syncOAuthProviderFields();
  setCodexAuthStatus('');
  setCodexOAuthDialogStatus('');
  if (!dialog.open && typeof dialog.showModal === 'function') dialog.showModal();
  providerSelect.focus?.();
  return true;
}

function closeOAuthLoginDialogElement() {
  const dialog = document.getElementById('oauthLoginDialog');
  if (dialog?.open) dialog.close();
  resetXAIOAuthDialog();
  const trigger = oauthLoginDialogTrigger;
  oauthLoginDialogTrigger = null;
  trigger?.focus?.();
}

function resetXAIOAuthDialog() {
  const controls = document.getElementById('xaiOAuthControls');
  const method = document.getElementById('xaiOAuthMethod');
  const secretField = document.getElementById('xaiCredentialSecretField');
  const textarea = document.getElementById('xaiCredentialValues');
  const authorizeButton = document.getElementById('oauthAuthorizeButton');
  if (controls) controls.hidden = true;
  if (method) method.value = 'manual';
  if (secretField) secretField.hidden = true;
  clearXAICredentialSecrets(textarea);
  if (textarea) textarea.required = false;
  resetOAuthCredentialImportProgress('xaiCredentialImport');
  if (authorizeButton) authorizeButton.hidden = false;
  setOAuthAuthorizeButtonLabel('codex', 'manual');
}

function clearXAICredentialSecrets(textarea = document.getElementById('xaiCredentialValues')) {
  if (!textarea) return;
  textarea.value = '';
  textarea.removeAttribute?.('aria-invalid');
}

function syncOAuthProviderFields() {
  const provider = document.getElementById('oauthProviderSelect')?.value || 'codex';
  const method = document.getElementById('xaiOAuthMethod')?.value || 'manual';
  const xai = provider === 'xai';
  const controls = document.getElementById('xaiOAuthControls');
  const secretField = document.getElementById('xaiCredentialSecretField');
  const textarea = document.getElementById('xaiCredentialValues');
  const sessionFields = document.getElementById('oauthSessionFields');
  const authorizeButton = document.getElementById('oauthAuthorizeButton');
  const description = document.getElementById('oauthLoginDialogDescription');
  resetOAuthCredentialImportProgress('xaiCredentialImport');
  if (controls) controls.hidden = !xai;
  if (sessionFields && xai) sessionFields.hidden = true;
  if (secretField) secretField.hidden = !xai || method === 'manual';
  if (textarea) {
    textarea.required = xai && method !== 'manual';
    textarea.setAttribute?.('aria-describedby', 'xaiCredentialSecretHint oauthLoginDialogStatus');
    if (!textarea.required) textarea.removeAttribute?.('aria-invalid');
  }
  if (description) {
    const descriptionKey = xai
      ? (method === 'manual' ? 'channels.xai.manualDescription' : 'channels.xai.importDescription')
      : 'channels.oauth.loginDialogDescription';
    description.setAttribute?.('data-i18n', descriptionKey);
    if (typeof window !== 'undefined' && typeof window.t === 'function') {
      description.textContent = window.t(descriptionKey);
    }
  }
  if (authorizeButton) {
    authorizeButton.hidden = false;
    setOAuthAuthorizeButtonLabel(provider, method, authorizeButton);
  }
}

function setOAuthAuthorizeButtonLabel(provider, method, button = document.getElementById('oauthAuthorizeButton')) {
  if (!button) return;
  const key = provider === 'xai'
    ? (method === 'manual' ? 'channels.xai.generateLink' : 'channels.xai.importSecrets')
    : 'channels.oauth.startAuthorization';
  button.setAttribute?.('data-i18n', key);
  if (typeof window !== 'undefined' && typeof window.t === 'function') button.textContent = window.t(key);
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
  resetOAuthCredentialImportProgress();
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
  const authorizeButton = document.getElementById('oauthAuthorizeButton');
  if (!dialog || !providerSelect || !sessionFields || !authorizationURL || !openLink || !callbackURL) return false;

  providerSelect.value = config.provider;
  providerSelect.disabled = true;
  if (authorizeButton) authorizeButton.hidden = true;
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

async function submitXAIOAuthCallback(callbackURL, fetcher = fetchDataWithAuth) {
  return submitOAuthCallback('xai', callbackURL, fetcher);
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

async function cancelXAIOAuth(state, fetcher = fetchDataWithAuth) {
  return cancelOAuth('xai', state, fetcher);
}

async function submitXAICredentialBatch(method, textarea, button, fetcher = fetchWithAuth, signal = undefined) {
  const normalizedMethod = String(method || '').trim().toLowerCase();
  if (!['refresh_token', 'sso'].includes(normalizedMethod)) {
    throw new Error(window.t('channels.xai.methodInvalid'));
  }
  let secret = String(textarea?.value || '').trim();
  if (!secret) {
    textarea?.setAttribute?.('aria-invalid', 'true');
    textarea?.focus?.();
    throw new Error(window.t('channels.xai.secretRequired'));
  }
  let body = JSON.stringify({ method: normalizedMethod, values: secret, priority_increment: 10 });
  secret = '';
  clearXAICredentialSecrets(textarea);
  if (button) {
    button.disabled = true;
    button.setAttribute?.('aria-busy', 'true');
  }
  try {
    if (typeof document !== 'undefined') resetOAuthCredentialImportProgress('xaiCredentialImport');
    const responsePromise = fetcher('/admin/xai/credentials/import/stream', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream' },
      body,
      signal
    });
    body = '';
    const response = await responsePromise;
    const onEvent = typeof document === 'undefined'
      ? undefined
      : event => updateOAuthCredentialImportProgress(event, 'xaiCredentialImport');
    return await readOAuthCredentialImportStream(response, onEvent);
  } finally {
    if (button) {
      button.disabled = false;
      button.removeAttribute?.('aria-busy');
    }
  }
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

async function pollXAIOAuthStatus(state, options = {}) {
  return pollOAuthStatus('xai', state, options);
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

async function stopActiveXAIImport(options = {}) {
  const closeDialog = options.closeDialog !== false;
  if (xaiImportStopPromise) return xaiImportStopPromise;
  const operation = (async () => {
    const flow = activeXAIImportFlow;
    if (flow) {
      flow.cancelling = true;
      flow.controller?.abort?.();
      if (activeXAIImportFlow === flow) activeXAIImportFlow = null;
      clearXAICredentialSecrets(flow.textarea);
      if (flow.button) flow.button.disabled = false;
    }
    const providerSelect = document.getElementById('oauthProviderSelect');
    if (providerSelect) providerSelect.disabled = false;
    clearXAICredentialSecrets();
    if (closeDialog) {
      closeOAuthLoginDialogElement();
      setCodexAuthStatus('');
      setCodexOAuthDialogStatus('');
    }
  })();
  xaiImportStopPromise = operation;
  try {
    return await operation;
  } finally {
    if (xaiImportStopPromise === operation) xaiImportStopPromise = null;
  }
}

async function stopActiveOAuth(options = {}) {
  await Promise.all([
    stopActiveCodexOAuth({ closeDialog: false }),
    stopActiveXAIImport({ closeDialog: false })
  ]);
  if (options.closeDialog !== false) {
    closeOAuthLoginDialogElement();
    setCodexAuthStatus('');
    setCodexOAuthDialogStatus('');
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
    await stopActiveOAuth({ closeDialog: true });
  } catch (error) {
    setCodexOAuthDialogStatus(error?.message || window.t('channels.oauth.cancelFailed'), 'error');
  }
}

async function restartOAuth(provider, button) {
  const config = oauthProviderConfig(provider);
  try {
    if (button) button.disabled = true;
    await stopActiveOAuth({ closeDialog: false });
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
  fetcher = fetchWithAuth,
  provider = 'auto',
  priorityIncrement = 10,
  delay = codexOAuthDelay
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
    resetOAuthCredentialImportProgress();
    updateOAuthCredentialImportProgress({ event: 'preparing', file_count: selectedFiles.length });
    const response = await fetcher('/admin/oauth/credentials/import/jobs', {
      method: 'POST',
      body: formData
    });
    const started = await parseOAuthCredentialImportResponse(response);
    if (!started?.job_id) throw new Error(window.t('channels.oauth.importFailed'));
    updateOAuthCredentialImportProgress({ event: 'start', total: started.total });
    const result = await pollOAuthCredentialImportJob(
      started.job_id,
      started.total,
      fetcher,
      updateOAuthCredentialImportProgress,
      delay
    );
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
    try {
      await reloadChannelsList();
    } catch (reloadError) {
      console.warn('Failed to reload channels after an interrupted OAuth credential import', reloadError);
    }
    return null;
  } finally {
    if (button) button.disabled = false;
  }
}

async function parseOAuthCredentialImportResponse(response) {
  let payload;
  try {
    payload = JSON.parse(await response.text());
  } catch (_) {
    throw new OAuthCredentialImportResponseError(
      window.t('channels.oauth.importFailed'),
      Number(response?.status) || 0
    );
  }
  if (!response?.ok || !payload?.success) {
    throw new OAuthCredentialImportResponseError(
      payload?.error || window.t('channels.oauth.importFailed'),
      Number(response?.status) || 0
    );
  }
  return payload.data;
}

async function pollOAuthCredentialImportJob(jobID, total, fetcher, onEvent, delay) {
  let cursor = 0;
  let created = 0;
  let skipped = 0;
  let failed = 0;
  let consecutiveNetworkErrors = 0;
  const results = [];

  for (;;) {
    let view;
    try {
      const response = await fetcher(
        `/admin/oauth/credentials/import/jobs/${encodeURIComponent(jobID)}?after=${cursor}`,
        { method: 'GET' }
      );
      view = await parseOAuthCredentialImportResponse(response);
      consecutiveNetworkErrors = 0;
    } catch (error) {
      if (error instanceof OAuthCredentialImportResponseError &&
          error.status !== 429 && error.status < 500) {
        throw error;
      }
      consecutiveNetworkErrors++;
      if (consecutiveNetworkErrors > OAUTH_CREDENTIAL_IMPORT_MAX_NETWORK_ERRORS) {
        throw new Error(window.t('channels.oauth.importProgressUnavailable', { job: jobID }));
      }
      onEvent({ event: 'reconnecting', processed: cursor, total, created, skipped, failed });
      await delay(Math.min(
        OAUTH_CREDENTIAL_IMPORT_POLL_INTERVAL_MS * consecutiveNetworkErrors,
        5000
      ));
      continue;
    }

    for (const result of Array.isArray(view?.results) ? view.results : []) {
      results.push(result);
      cursor++;
      if (result.status === 'created') created++;
      else if (result.status === 'skipped') skipped++;
      else failed++;
      onEvent({
        event: 'progress', processed: cursor, total,
        created, skipped, failed, file_name: result.file_name, result
      });
    }
    const next = Math.max(cursor, Number(view?.next) || 0);
    cursor = next;

    if (view?.status === 'succeeded') {
      const summary = {
        created: Number(view.created) || 0,
        skipped: Number(view.skipped) || 0,
        failed: Number(view.failed) || 0,
        results
      };
      onEvent({ event: 'complete', processed: cursor, total, ...summary });
      return summary;
    }
    if (view?.status && view.status !== 'running') {
      throw new Error(view.error || window.t('channels.oauth.importJobStopped'));
    }
    onEvent({
      event: 'processing', processed: cursor, total,
      created, skipped, failed, file_name: view?.file_name || ''
    });
    await delay(OAUTH_CREDENTIAL_IMPORT_POLL_INTERVAL_MS);
  }
}

async function readOAuthCredentialImportStream(response, onEvent = () => {}) {
  if (!response?.ok) {
    let message = window.t('channels.oauth.importFailed');
    try {
      const payload = JSON.parse(await response.text());
      message = payload?.error || message;
    } catch (_) {
      // Keep the localized fallback for malformed error responses.
    }
    throw new Error(message);
  }

  const results = [];
  let complete = null;
  let buffer = '';
  const consumeBlock = block => {
    const data = block
      .split(/\r?\n/)
      .filter(line => line.startsWith('data:'))
      .map(line => line.slice(5).trimStart())
      .join('\n');
    if (!data) return;
    const event = JSON.parse(data);
    onEvent(event);
    if (event.event === 'progress' && event.result) results.push(event.result);
    if (event.event === 'complete') complete = event;
  };
  const drain = final => {
    for (;;) {
      const boundary = /\r?\n\r?\n/.exec(buffer);
      if (!boundary) break;
      consumeBlock(buffer.slice(0, boundary.index));
      buffer = buffer.slice(boundary.index + boundary[0].length);
    }
    if (final && buffer.trim()) {
      consumeBlock(buffer);
      buffer = '';
    }
  };

  if (response.body?.getReader) {
    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      drain(false);
    }
    buffer += decoder.decode();
  } else {
    buffer = await response.text();
  }
  drain(true);
  if (!complete) throw new Error(window.t('channels.oauth.importStreamIncomplete'));
  return {
    created: Number(complete.created) || 0,
    skipped: Number(complete.skipped) || 0,
    failed: Number(complete.failed) || 0,
    results
  };
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

function getOAuthUsageState(channelID) {
  const numericID = Number(channelID);
  if (!Number.isInteger(numericID) || numericID <= 0) return null;
  return oauthUsageStateByChannelID.get(numericID) || null;
}

function rerenderOAuthUsage() {
  if (typeof filterChannels === 'function') filterChannels();
}

async function refreshOAuthUsage(channelID, fetcher = fetchDataWithAuth, options = {}) {
  const numericID = Number(channelID);
  if (!Number.isInteger(numericID) || numericID <= 0) {
    throw new Error('A saved OAuth channel is required');
  }
  oauthUsageStateByChannelID.set(numericID, { status: 'loading' });
  rerenderOAuthUsage();
  try {
    const result = await fetcher(`/admin/channels/${numericID}/oauth-usage`, { method: 'POST' });
    if (!result || !Array.isArray(result.windows)) {
      throw new Error(window.t('channels.oauth.usageInvalid'));
    }
    oauthUsageStateByChannelID.set(numericID, { status: 'ready', data: result });
    if (options.reload !== false && typeof loadChannels === 'function') {
      await loadChannels();
    } else {
      rerenderOAuthUsage();
    }
    return result;
  } catch (error) {
    const message = error?.message || window.t('channels.oauth.usageFailed');
    oauthUsageStateByChannelID.set(numericID, { status: 'error', error: message });
    rerenderOAuthUsage();
    throw error;
  }
}

async function refreshOAuthUsageBatch(channelIDs, fetcher = fetchDataWithAuth) {
  const ids = Array.from(new Set((channelIDs || [])
    .map(id => Number(id))
    .filter(id => Number.isInteger(id) && id > 0)));
  const summary = { total: ids.length, succeeded: 0, failed: 0 };
  if (ids.length === 0) return summary;

  for (const id of ids) {
    try {
      await refreshOAuthUsage(id, fetcher, { reload: false });
      summary.succeeded++;
    } catch {
      summary.failed++;
    }
  }

  if (typeof loadChannels === 'function') {
    await loadChannels();
  } else {
    rerenderOAuthUsage();
  }
  return summary;
}

async function batchRefreshSelectedOAuthUsage(fetcher = fetchDataWithAuth) {
  const selectedIDs = typeof getSelectedChannelIDs === 'function' ? getSelectedChannelIDs() : [];
  if (selectedIDs.length === 0) {
    if (window.showWarning) window.showWarning(window.t('channels.batchNoSelection'));
    return null;
  }

  const channelList = typeof channels !== 'undefined' && Array.isArray(channels) ? channels : [];
  const eligibleIDs = selectedIDs.filter(id => {
    const channel = channelList.find(item => Number(item.id) === id);
    return channel && ['codex_oauth', 'antigravity_oauth', 'xai_oauth'].includes(channel.auth_type);
  });
  const skipped = selectedIDs.length - eligibleIDs.length;
  if (eligibleIDs.length === 0) {
    if (window.showWarning) window.showWarning(window.t('channels.batchOAuthUsageNoEligible'));
    return { total: selectedIDs.length, succeeded: 0, failed: 0, skipped };
  }

  const button = document.getElementById('batchRefreshOAuthUsageBtn');
  const label = document.getElementById('batchRefreshOAuthUsageLabel');
  const floatingMenu = document.getElementById('batchFloatingMenu');
  if (button) {
    button.disabled = true;
    button.setAttribute('aria-busy', 'true');
  }
  if (floatingMenu) floatingMenu.setAttribute('aria-busy', 'true');
  if (label) {
    label.setAttribute('data-i18n', 'channels.oauth.usageRefreshing');
    label.textContent = window.t('channels.oauth.usageRefreshing');
  }
  if (typeof updateBatchChannelSelectionUI === 'function') {
    updateBatchChannelSelectionUI();
  }

  try {
    const batch = await refreshOAuthUsageBatch(eligibleIDs, fetcher);
    const result = {
      total: selectedIDs.length,
      succeeded: batch.succeeded,
      failed: batch.failed,
      skipped
    };
    const message = window.t('channels.batchOAuthUsageSummary', result);
    if (batch.failed === batch.total) {
      if (window.showError) window.showError(message);
    } else if (batch.failed > 0) {
      if (window.showWarning) window.showWarning(message);
    } else if (window.showSuccess) {
      window.showSuccess(message);
    }
    return result;
  } catch (error) {
    if (window.showError) {
      window.showError(window.t('channels.batchOAuthUsageFailed', {
        error: error?.message || window.t('common.failed')
      }));
    }
    return null;
  } finally {
    if (button) {
      button.removeAttribute('aria-busy');
      button.disabled = false;
    }
    if (floatingMenu) floatingMenu.removeAttribute('aria-busy');
    if (label) {
      label.setAttribute('data-i18n', 'channels.oauth.usageRefresh');
      label.textContent = window.t('channels.oauth.usageRefresh');
    }
    if (typeof updateBatchChannelSelectionUI === 'function') {
      updateBatchChannelSelectionUI();
    }
  }
}

function setupOAuthActions() {
  const loginButton = document.getElementById('oauthLoginBtn');
  const loginDialog = document.getElementById('oauthLoginDialog');
  const loginForm = document.getElementById('oauthLoginForm');
  const providerSelect = document.getElementById('oauthProviderSelect');
  const xaiMethod = document.getElementById('xaiOAuthMethod');
  const xaiCredentialValues = document.getElementById('xaiCredentialValues');
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
  if (providerSelect && typeof providerSelect.addEventListener === 'function' && !providerSelect.dataset?.xaiBound) {
    providerSelect.addEventListener('change', syncOAuthProviderFields);
    if (providerSelect.dataset) providerSelect.dataset.xaiBound = '1';
  }
  if (xaiMethod && !xaiMethod.dataset.bound) {
    xaiMethod.addEventListener('change', syncOAuthProviderFields);
    xaiMethod.dataset.bound = '1';
  }
  if (loginForm && providerSelect && authorizeButton && !loginForm.dataset.bound) {
    loginForm.addEventListener('submit', async event => {
      event.preventDefault();
      if (activeCodexOAuthFlow || activeXAIImportFlow) return;
      providerSelect.disabled = true;
      if (providerSelect.value === 'xai') {
        const method = xaiMethod?.value || 'manual';
        if (method === 'manual') {
          await startOAuth('xai', authorizeButton);
        } else {
          const controller = typeof AbortController === 'function' ? new AbortController() : null;
          const flow = {
            kind: 'import', session: '', button: authorizeButton, textarea: xaiCredentialValues,
            cancelling: false, controller
          };
          activeXAIImportFlow = flow;
          try {
            setCodexOAuthDialogStatus(window.t('channels.xai.importing'));
            const result = await submitXAICredentialBatch(
              method, xaiCredentialValues, authorizeButton, fetchWithAuth, controller?.signal
            );
            if (flow.cancelling || activeXAIImportFlow !== flow) return;
            const message = window.t('channels.oauth.importSummary', result);
            setCodexOAuthDialogStatus(message, result.failed > 0 ? 'error' : 'success');
            if (result.created > 0) await reloadChannelsList();
          } catch (error) {
            if (flow.cancelling || activeXAIImportFlow !== flow) return;
            const message = error?.message || window.t('channels.xai.importFailed');
            setCodexOAuthDialogStatus(message, 'error');
            if (window.showError) window.showError(message);
          } finally {
            if (activeXAIImportFlow === flow) activeXAIImportFlow = null;
          }
        }
      } else {
        await startOAuth(oauthProviderConfig(providerSelect.value).provider, authorizeButton);
      }
      if (loginDialog?.open && sessionFields?.hidden) providerSelect.disabled = false;
    });
    loginForm.dataset.bound = '1';
  }
  for (const [button, input] of [[copyButton, authorizationURL]]) {
    if (!button || !input || button.dataset.bound) continue;
    button.addEventListener('click', async () => {
      try {
        await copyCodexOAuthLink(input.value);
        setCodexOAuthDialogStatus(window.t('channels.oauth.linkCopied'), 'success');
      } catch (error) {
        setCodexOAuthDialogStatus(error?.message || window.t('channels.oauth.copyFailed'), 'error');
      }
    });
    button.dataset.bound = '1';
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
  if (loginDialog && !loginDialog.dataset.overlayBound) {
    loginDialog.addEventListener('click', event => {
      if (event.target === loginDialog) void closeOAuthLoginDialog();
    });
    loginDialog.dataset.overlayBound = '1';
  }
  if (!oauthPagehideBound && typeof window !== 'undefined' && typeof window.addEventListener === 'function') {
    window.addEventListener('pagehide', () => {
      clearXAICredentialSecrets();
      void stopActiveOAuth({ closeDialog: false }).catch(() => {});
    });
    oauthPagehideBound = true;
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
      await importOAuthCredentials(
        importInput.files,
        importSubmitButton,
        fetchWithAuth,
        importProviderSelect.value,
        Number(importPriorityIncrementSelect.value)
      );
      importProviderSelect.disabled = false;
      importPriorityIncrementSelect.disabled = false;
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
    batchRefreshSelectedOAuthUsage,
    cancelAntigravityOAuth,
    cancelCodexOAuth,
    cancelXAIOAuth,
    copyOAuthCredential,
    copyCodexOAuthLink,
    formatCodexPlanBadgeText,
    getOAuthUsageState,
    importOAuthCredentials,
    openOAuthCredentialImportDialog,
    openOAuthLoginDialog,
    pollAntigravityOAuthStatus,
    pollCodexOAuthStatus,
    pollXAIOAuthStatus,
    refreshOAuthCredential,
    refreshOAuthUsage,
    refreshOAuthUsageBatch,
    renderOAuthCredential,
    setOAuthCredentialView,
    setupOAuthActions,
    showOAuthSession,
    submitAntigravityOAuthCallback,
    submitCodexOAuthCallback,
    submitXAIOAuthCallback,
    submitXAICredentialBatch
  };
}
