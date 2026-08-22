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
let activeCodexPersonalAccessTokenFlow = null;
let codexOAuthStopPromise = null;
let activeXAIImportFlow = null;
let xaiImportStopPromise = null;
let activeAnthropicCookieFlow = null;
let activeZAIKeyFlow = null;
let activeCursorImportFlow = null;
let oauthPagehideBound = false;
let activeOAuthCredentialCleanup = null;
let oauthCredentialCleanupModelLoadSequence = 0;
let oauthCredentialCleanupOptions = { models: [] };
let oauthCredentialCleanupDialogTrigger = null;
let currentOAuthCredentialJSON = '';
let currentOAuthCredential = null;
let currentOAuthCredentialInfo = null;
let currentOAuthCredentialView = 'decoded';
let oauthLoginDialogTrigger = null;
let oauthCredentialImportDialogTrigger = null;
const oauthUsageStateByChannelID = new Map();
const oauthUsageOperationByChannelID = new Map();
let oauthUsageOperationSequence = 0;
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
  }),
  anthropic: Object.freeze({
    provider: 'anthropic', label: 'Anthropic', i18n: 'channels.anthropic',
    callbackPlaceholder: 'code#state', authorizationCode: true
  }),
  // Z.ai (ZCode) approves in the browser and ccLoad polls for the result, so
  // there is no callback for the administrator to paste back.
  zai: Object.freeze({
    provider: 'zai', label: 'Z.ai', i18n: 'channels.zai',
    callbackPlaceholder: '', pollOnly: true
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

function renderCodexQuotaOverdraft(credential, visible) {
  const settings = document.getElementById('codexQuotaOverdraftSettings');
  const checkbox = document.getElementById('codexQuotaOverdraftEnabled');
  const requests = document.getElementById('codexQuotaOverdraftRequests');
  const cost = document.getElementById('codexQuotaOverdraftCost');
  const overdraft = credential?.quota_overdraft || {};
  if (settings) settings.hidden = !visible;
  if (checkbox) {
    checkbox.disabled = !visible;
    checkbox.checked = visible && overdraft.enabled === true;
  }
  if (requests) requests.textContent = String(Math.max(0, Number(overdraft.successful_requests) || 0));
  if (cost) {
    const costUSD = Math.max(0, Number(overdraft.cost_microusd) || 0) / 1e6;
    if (costUSD === 0) {
      cost.textContent = '$0';
    } else if (costUSD < 0.001) {
      cost.textContent = `$${costUSD.toFixed(6)}`;
    } else {
      cost.textContent = typeof window !== 'undefined' && typeof window.formatCost === 'function'
        ? window.formatCost(costUSD)
        : `$${costUSD.toFixed(6)}`;
    }
  }
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
  const codexPersonalAccessToken = codexOAuth && credential?.auth_mode === 'personalAccessToken';
  const xaiOAuth = authType === 'xai_oauth';
  const anthropicOAuth = authType === 'anthropic_oauth';
  const zaiOAuth = authType === 'zai_oauth';
  const cursorOAuth = authType === 'cursor_oauth';
  const credentialVisible = codexOAuth || authType === 'antigravity_oauth' || xaiOAuth || anthropicOAuth || zaiOAuth || cursorOAuth;
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
    : (anthropicOAuth
        ? String(credential?.plan_type || channel?.anthropic_plan_type || '').trim()
        : String(channel?.xai_subscription_tier || '').trim());
  const planBadgeText = codexOAuth
    ? formatCodexPlanBadgeText(planType, channel?.codex_subscription_active_until)
    : ((xaiOAuth || anthropicOAuth) ? planType : '');
  if (notice) {
    const noticeKey = xaiOAuth
      ? 'channels.xai.editorReadOnly'
      : (codexPersonalAccessToken ? 'channels.codex.personalAccessTokenReadOnly' : 'channels.oauthCredentialReadOnly');
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
  if (credentialRefreshButton) credentialRefreshButton.hidden = xaiOAuth || zaiOAuth || cursorOAuth || codexPersonalAccessToken;
  renderOAuthCredential(
    credentialVisible ? credential : null,
    codexOAuth ? credentialInfo : null,
    credentialView
  );
  renderCodexQuotaOverdraft(credential, codexOAuth);

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

function setXAICredentialImportView(state, textarea, button) {
  if (typeof document === 'undefined') return;
  const secretField = document.getElementById('xaiCredentialSecretField');
  const method = document.getElementById('xaiOAuthMethod');
  const submitButton = button || document.getElementById('oauthAuthorizeButton');
  const { container, detail } = getOAuthCredentialImportProgressElements('xaiCredentialImport');
  const showingResult = state === 'importing' || state === 'result';

  if (secretField) secretField.hidden = showingResult;
  if (method) method.disabled = state === 'importing';
  if (submitButton) submitButton.hidden = showingResult;
  if (state === 'importing') {
    if (container) container.hidden = false;
    if (detail) detail.textContent = window.t('channels.xai.importing');
    container?.focus?.();
  } else if (state === 'edit') {
    resetOAuthCredentialImportProgress('xaiCredentialImport');
    textarea?.removeAttribute?.('aria-invalid');
  }
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
  const loginActions = document.getElementById('oauthLoginActions');
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
  if (loginActions) loginActions.hidden = false;
  sessionFields.hidden = true;
  authorizationURL.value = '';
  openLink.removeAttribute?.('href');
  callbackURL.value = '';
  callbackURL.removeAttribute?.('aria-invalid');
  resetXAIOAuthDialog();
  resetCodexPersonalAccessTokenDialog();
  resetAnthropicCookieDialog();
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
  resetCodexPersonalAccessTokenDialog();
  resetAnthropicCookieDialog();
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
  if (method) {
    method.value = 'manual';
    method.disabled = false;
  }
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

function resetCodexPersonalAccessTokenDialog() {
  const controls = document.getElementById('codexOAuthControls');
  const method = document.getElementById('codexOAuthMethod');
  const field = document.getElementById('codexPersonalAccessTokenField');
  const input = document.getElementById('codexPersonalAccessToken');
  if (controls) controls.hidden = false;
  if (method) {
    method.value = 'oauth';
    method.disabled = false;
  }
  if (field) field.hidden = true;
  clearCodexPersonalAccessToken(input);
  if (input) input.required = false;
}

function clearCodexPersonalAccessToken(input = document.getElementById('codexPersonalAccessToken')) {
  if (!input) return;
  input.value = '';
  input.removeAttribute?.('aria-invalid');
}

function resetAnthropicCookieDialog() {
  const controls = document.getElementById('anthropicOAuthControls');
  const method = document.getElementById('anthropicOAuthMethod');
  const cookieField = document.getElementById('anthropicCookieField');
  const input = document.getElementById('anthropicSessionKey');
  if (controls) controls.hidden = true;
  if (method) {
    method.value = 'code';
    method.disabled = false;
  }
  if (cookieField) cookieField.hidden = true;
  clearAnthropicCookieSecret(input);
  if (input) input.required = false;
}

function clearAnthropicCookieSecret(input = document.getElementById('anthropicSessionKey')) {
  if (!input) return;
  input.value = '';
  input.removeAttribute?.('aria-invalid');
}

function syncOAuthProviderFields() {
  const provider = document.getElementById('oauthProviderSelect')?.value || 'codex';
  const codexMethod = document.getElementById('codexOAuthMethod')?.value || 'oauth';
  const xaiMethod = document.getElementById('xaiOAuthMethod')?.value || 'manual';
  const anthropicMethod = document.getElementById('anthropicOAuthMethod')?.value || 'code';
  const zaiMethod = document.getElementById('zaiOAuthMethod')?.value || 'oauth';
  const xai = provider === 'xai';
  const anthropic = provider === 'anthropic';
  const codex = provider === 'codex';
  const zai = provider === 'zai';
  const cursor = provider === 'cursor';
  const zaiAPIKey = zai && zaiMethod === 'api_key';
  const cursorAPIKey = cursor;
  const codexPersonalAccessToken = codex && codexMethod === 'personalAccessToken';
  const anthropicCookie = anthropic && anthropicMethod === 'cookie';
  const controls = document.getElementById('xaiOAuthControls');
  const secretField = document.getElementById('xaiCredentialSecretField');
  const textarea = document.getElementById('xaiCredentialValues');
  const anthropicControls = document.getElementById('anthropicOAuthControls');
  const anthropicCookieField = document.getElementById('anthropicCookieField');
  const anthropicSessionKey = document.getElementById('anthropicSessionKey');
  const zaiControls = document.getElementById('zaiOAuthControls');
  const zaiAPIKeyField = document.getElementById('zaiAPIKeyField');
  const zaiAPIKeyInput = document.getElementById('zaiCodingPlanKey');
  const cursorControls = document.getElementById('cursorOAuthControls');
  const cursorAPIKeyField = document.getElementById('cursorAPIKeyField');
  const cursorAPIKeyInput = document.getElementById('cursorUserAPIKey');
  const codexControls = document.getElementById('codexOAuthControls');
  const codexPersonalAccessTokenField = document.getElementById('codexPersonalAccessTokenField');
  const codexPersonalAccessTokenInput = document.getElementById('codexPersonalAccessToken');
  const sessionFields = document.getElementById('oauthSessionFields');
  const authorizeButton = document.getElementById('oauthAuthorizeButton');
  const description = document.getElementById('oauthLoginDialogDescription');
  resetOAuthCredentialImportProgress('xaiCredentialImport');
  if (controls) controls.hidden = !xai;
  if (codexControls) codexControls.hidden = !codex;
  if (anthropicControls) anthropicControls.hidden = !anthropic;
  if (zaiControls) zaiControls.hidden = !zai;
  if (cursorControls) cursorControls.hidden = !cursor;
  if (zaiAPIKeyField) zaiAPIKeyField.hidden = !zaiAPIKey;
  if (zaiAPIKeyInput) {
    zaiAPIKeyInput.required = zaiAPIKey;
    if (!zaiAPIKey) clearZAICodingPlanKey(zaiAPIKeyInput);
  }
  if (cursorAPIKeyField) cursorAPIKeyField.hidden = !cursorAPIKey;
  if (cursorAPIKeyInput) {
    cursorAPIKeyInput.required = cursorAPIKey;
    if (!cursorAPIKey) clearCursorSecret(cursorAPIKeyInput);
  }
  if (sessionFields && (xai || anthropicCookie || codexPersonalAccessToken || zaiAPIKey || cursorAPIKey)) sessionFields.hidden = true;
  if (codexPersonalAccessTokenField) codexPersonalAccessTokenField.hidden = !codexPersonalAccessToken;
  if (codexPersonalAccessTokenInput) {
    codexPersonalAccessTokenInput.required = codexPersonalAccessToken;
    if (!codexPersonalAccessToken) clearCodexPersonalAccessToken(codexPersonalAccessTokenInput);
  }
  if (secretField) secretField.hidden = !xai || xaiMethod === 'manual';
  if (textarea) {
    textarea.required = xai && xaiMethod !== 'manual';
    textarea.setAttribute?.('aria-describedby', 'xaiCredentialSecretHint oauthLoginDialogStatus');
    if (!textarea.required) textarea.removeAttribute?.('aria-invalid');
  }
  if (anthropicCookieField) anthropicCookieField.hidden = !anthropicCookie;
  if (anthropicSessionKey) {
    anthropicSessionKey.required = anthropicCookie;
    if (!anthropicCookie) clearAnthropicCookieSecret(anthropicSessionKey);
  }
  if (description) {
    const descriptionKey = codexPersonalAccessToken
      ? 'channels.codex.personalAccessTokenDescription'
      : cursor
      ? 'channels.cursor.apiKeyDescription'
      : zai
      ? (zaiAPIKey ? 'channels.zai.apiKeyDescription' : 'channels.zai.oauthDescription')
      : xai
      ? (xaiMethod === 'manual' ? 'channels.xai.manualDescription' : 'channels.xai.importDescription')
      : (anthropic
          ? (anthropicCookie ? 'channels.anthropic.cookieDescription' : 'channels.anthropic.codeDescription')
          : 'channels.oauth.loginDialogDescription');
    description.setAttribute?.('data-i18n', descriptionKey);
    if (typeof window !== 'undefined' && typeof window.t === 'function') {
      description.textContent = window.t(descriptionKey);
    }
  }
  if (authorizeButton) {
    authorizeButton.hidden = false;
    const method = codex ? codexMethod : (xai ? xaiMethod : (zai ? zaiMethod : anthropicMethod));
    setOAuthAuthorizeButtonLabel(provider, method, authorizeButton);
  }
}

function setOAuthAuthorizeButtonLabel(provider, method, button = document.getElementById('oauthAuthorizeButton')) {
  if (!button) return;
  const key = provider === 'cursor'
    ? 'channels.cursor.apiKeySubmit'
    : provider === 'zai'
    ? (method === 'api_key' ? 'channels.zai.apiKeySubmit' : 'channels.oauth.startAuthorization')
    : provider === 'xai'
    ? (method === 'manual' ? 'channels.xai.generateLink' : 'channels.xai.importSecrets')
    : (provider === 'codex' && method === 'personalAccessToken'
        ? 'channels.codex.personalAccessTokenSubmit'
        : provider === 'anthropic' && method === 'cookie'
        ? 'channels.anthropic.authorizeWithCookie'
        : 'channels.oauth.startAuthorization');
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
  const callbackLabel = document.getElementById('oauthCallbackLabel');
  const callbackHint = document.getElementById('oauthCallbackHint');
  const callbackButton = document.getElementById('oauthSubmitCallback');
  const authorizeButton = document.getElementById('oauthAuthorizeButton');
  const loginActions = document.getElementById('oauthLoginActions');
  if (!dialog || !providerSelect || !sessionFields || !authorizationURL || !openLink || !callbackURL) return false;

  providerSelect.value = config.provider;
  providerSelect.disabled = true;
  if (loginActions) loginActions.hidden = true;
  else if (authorizeButton) authorizeButton.hidden = true;
  sessionFields.hidden = false;
  const sessionKey = config.pollOnly
    ? `${config.i18n}.sessionDescription`
    : (config.authorizationCode ? 'channels.anthropic.sessionDescription' : 'channels.oauth.sessionDescription');
  if (sessionDescription) sessionDescription.textContent = window.t(sessionKey);
  const callbackKey = config.authorizationCode ? 'channels.anthropic.authorizationCode' : 'channels.oauth.callbackURL';
  const hintKey = config.authorizationCode ? 'channels.anthropic.authorizationCodeHint' : 'channels.oauth.callbackHint';
  const submitKey = config.authorizationCode ? 'channels.anthropic.submitAuthorizationCode' : 'channels.oauth.submitCallback';
  if (callbackLabel) {
    callbackLabel.setAttribute?.('data-i18n', callbackKey);
    callbackLabel.textContent = window.t(callbackKey);
  }
  if (callbackHint) {
    callbackHint.setAttribute?.('data-i18n', hintKey);
    callbackHint.textContent = window.t(hintKey);
  }
  if (callbackButton) {
    callbackButton.setAttribute?.('data-i18n', submitKey);
    callbackButton.textContent = window.t(submitKey);
  }
  const callbackFormElement = document.getElementById('oauthCallbackForm');
  if (callbackFormElement) callbackFormElement.hidden = Boolean(config.pollOnly);
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

async function submitOAuthCallback(provider, callbackURL, fetcher = fetchDataWithAuth, state = '') {
  const config = oauthProviderConfig(provider);
  const normalizedURL = String(callbackURL || '').trim();
  if (!normalizedURL) throw new Error(`${config.label} OAuth ${config.authorizationCode ? 'authorization code' : 'callback URL'} is required`);
  const body = config.authorizationCode
    ? { state: String(state || '').trim(), code: normalizedURL }
    : { callback_url: normalizedURL };
  return fetcher(`/admin/${config.provider}/oauth/callback`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body)
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

async function submitAnthropicOAuthCode(code, state, fetcher = fetchDataWithAuth) {
  return submitOAuthCallback('anthropic', code, fetcher, state);
}

async function submitAnthropicCookieAuth(
  input,
  fetcher = fetchDataWithAuth,
  signal = undefined,
  onProgress = () => {}
) {
  let entries = String(input?.value || '')
    .split(/\r?\n/)
    .map((sessionKey, index) => ({ line: index + 1, sessionKey: sessionKey.trim() }))
    .filter(entry => entry.sessionKey);
  if (entries.length === 0) {
    input?.setAttribute?.('aria-invalid', 'true');
    input?.focus?.();
    throw new Error(window.t('channels.anthropic.cookieRequired'));
  }
  input?.removeAttribute?.('aria-invalid');
  clearAnthropicCookieSecret(input);
  const summary = {
    total: entries.length,
    created: 0,
    updated: 0,
    failed: 0,
    failedLines: [],
    failedDetails: []
  };
  try {
    for (let index = 0; index < entries.length; index++) {
      if (signal?.aborted) {
        const error = new Error('Anthropic Cookie authorization cancelled');
        error.name = 'AbortError';
        throw error;
      }
      const entry = entries[index];
      onProgress({ current: index + 1, total: entries.length });
      let body = JSON.stringify({ session_key: entry.sessionKey });
      try {
        const result = await fetcher('/admin/anthropic/oauth/cookie', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body,
          signal
        });
        if (result?.created === true) summary.created++;
        else summary.updated++;
      } catch (error) {
        if (signal?.aborted || error?.name === 'AbortError') throw error;
        summary.failed++;
        summary.failedLines.push(entry.line);
        const errorMessage = String(error?.message || '').trim();
        summary.failedDetails.push({
          line: entry.line,
          error: errorMessage || window.t('channels.anthropic.cookieFailed')
        });
      } finally {
        body = '';
        entry.sessionKey = '';
      }
    }
    return summary;
  } finally {
    for (const entry of entries) entry.sessionKey = '';
    entries = [];
  }
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

async function cancelAnthropicOAuth(state, fetcher = fetchDataWithAuth) {
  return cancelOAuth('anthropic', state, fetcher);
}

async function cancelZAIOAuth(state, fetcher = fetchDataWithAuth) {
  return cancelOAuth('zai', state, fetcher);
}

// submitZAICodingPlanKey imports a Coding Plan key without a browser round trip.
// The secret is cleared from the page before the request is awaited.
async function submitZAICodingPlanKey(input, fetcher = fetchDataWithAuth, signal = undefined) {
  let apiKey = String(input?.value || '').trim();
  if (!apiKey) {
    input?.setAttribute?.('aria-invalid', 'true');
    input?.focus?.();
    throw new Error(window.t('channels.zai.apiKeyRequired'));
  }
  let body = JSON.stringify({ api_key: apiKey });
  apiKey = '';
  clearZAICodingPlanKey(input);
  try {
    return await fetcher('/admin/zai/credentials/import', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body,
      signal
    });
  } finally {
    body = '';
  }
}

function clearZAICodingPlanKey(input = document.getElementById('zaiCodingPlanKey')) {
  if (!input) return;
  input.value = '';
  input.removeAttribute?.('aria-invalid');
}

function clearCursorSecret(input) {
  if (!input) return;
  input.value = '';
  input.removeAttribute?.('aria-invalid');
}

async function submitCursorCredential(input, fetcher = fetchDataWithAuth, signal = undefined) {
  let secret = String(input?.value || '').trim();
  if (!secret) {
    input?.setAttribute?.('aria-invalid', 'true');
    input?.focus?.();
    throw new Error(window.t('channels.cursor.apiKeyRequired'));
  }
  let body = JSON.stringify({ api_key: secret });
  secret = '';
  clearCursorSecret(input);
  try {
    return await fetcher('/admin/cursor/credentials/import', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body,
      signal
    });
  } finally {
    body = '';
  }
}

async function submitXAICredentialBatch(
  method,
  textarea,
  button,
  fetcher = fetchWithAuth,
  signal = undefined,
  delay = codexOAuthDelay
) {
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
  let completed = false;
  try {
    if (typeof document !== 'undefined') resetOAuthCredentialImportProgress('xaiCredentialImport');
    setXAICredentialImportView('importing', textarea, button);
    const responsePromise = fetcher('/admin/xai/credentials/import/stream', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'text/event-stream' },
      body,
      signal
    });
    body = '';
    const response = await responsePromise;
    const onEvent = typeof document === 'undefined'
      ? () => {}
      : event => updateOAuthCredentialImportProgress(event, 'xaiCredentialImport');
    const result = await readXAICredentialImportStream(response, onEvent, {
      fetcher, delay, signal
    });
    completed = true;
    return result;
  } finally {
    if (button) {
      button.disabled = false;
      button.removeAttribute?.('aria-busy');
    }
    setXAICredentialImportView(completed ? 'result' : 'edit', textarea, button);
  }
}

async function submitCodexPersonalAccessToken(input, fetcher = fetchDataWithAuth, signal = undefined) {
  let accessToken = String(input?.value || '').trim();
  if (!accessToken) {
    input?.setAttribute?.('aria-invalid', 'true');
    input?.focus?.();
    throw new Error(window.t('channels.codex.personalAccessTokenRequired'));
  }
  if (!accessToken.startsWith('at-')) {
    input?.setAttribute?.('aria-invalid', 'true');
    input?.focus?.();
    throw new Error(window.t('channels.codex.personalAccessTokenInvalid'));
  }
  let body = JSON.stringify({ access_token: accessToken });
  accessToken = '';
  clearCodexPersonalAccessToken(input);
  try {
    return await fetcher('/admin/codex/personal-access-token', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body,
      signal
    });
  } finally {
    body = '';
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

async function pollAnthropicOAuthStatus(state, options = {}) {
  return pollOAuthStatus('anthropic', state, options);
}

async function pollZAIOAuthStatus(state, options = {}) {
  return pollOAuthStatus('zai', state, options);
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
    const unavailable = config.provider === 'zai' && /unavailable/i.test(error?.message);
    const message = unavailable
      ? window.t(`${config.i18n}.oauthUnavailable`)
      : (error?.message || window.t(`${config.i18n}.oauthFailed`));
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
    stopActiveCodexPersonalAccessToken(),
    stopActiveXAIImport({ closeDialog: false }),
    stopActiveAnthropicCookieAuth(),
    stopActiveZAIKeyImport(),
    stopActiveCursorImport()
  ]);
  if (options.closeDialog !== false) {
    closeOAuthLoginDialogElement();
    setCodexAuthStatus('');
    setCodexOAuthDialogStatus('');
  }
}

function stopActiveCursorImport() {
  const flow = activeCursorImportFlow;
  if (flow) {
    flow.cancelling = true;
    flow.controller?.abort?.();
    if (activeCursorImportFlow === flow) activeCursorImportFlow = null;
    if (flow.button) {
      flow.button.disabled = false;
      flow.button.removeAttribute?.('aria-busy');
    }
  }
  clearCursorSecret(flow?.input);
}

function stopActiveZAIKeyImport() {
  const flow = activeZAIKeyFlow;
  if (flow) {
    flow.cancelling = true;
    flow.controller?.abort?.();
    if (activeZAIKeyFlow === flow) activeZAIKeyFlow = null;
    if (flow.button) {
      flow.button.disabled = false;
      flow.button.removeAttribute?.('aria-busy');
    }
  }
  clearZAICodingPlanKey(flow?.input);
  const method = document.getElementById('zaiOAuthMethod');
  if (method) method.disabled = false;
}

function stopActiveCodexPersonalAccessToken() {
  const flow = activeCodexPersonalAccessTokenFlow;
  if (flow) {
    flow.cancelling = true;
    flow.controller?.abort?.();
    if (activeCodexPersonalAccessTokenFlow === flow) activeCodexPersonalAccessTokenFlow = null;
    if (flow.button) {
      flow.button.disabled = false;
      flow.button.removeAttribute?.('aria-busy');
    }
  }
  clearCodexPersonalAccessToken(flow?.input);
  const method = document.getElementById('codexOAuthMethod');
  if (method) method.disabled = false;
}

function stopActiveAnthropicCookieAuth() {
  const flow = activeAnthropicCookieFlow;
  if (flow) {
    flow.cancelling = true;
    flow.controller?.abort?.();
    if (activeAnthropicCookieFlow === flow) activeAnthropicCookieFlow = null;
    if (flow.button) {
      flow.button.disabled = false;
      flow.button.removeAttribute?.('aria-busy');
    }
  }
  clearAnthropicCookieSecret();
  const method = document.getElementById('anthropicOAuthMethod');
  if (method) method.disabled = false;
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
  return parseAdminJobResponse(response, 'channels.oauth.importFailed');
}

async function parseAdminJobResponse(response, fallbackKey) {
  let payload;
  try {
    payload = JSON.parse(await response.text());
  } catch (_) {
    throw new OAuthCredentialImportResponseError(
      window.t(fallbackKey),
      Number(response?.status) || 0
    );
  }
  if (!response?.ok || !payload?.success) {
    throw new OAuthCredentialImportResponseError(
      payload?.error || window.t(fallbackKey),
      Number(response?.status) || 0
    );
  }
  return payload.data;
}

async function pollOAuthCredentialImportJob(jobID, total, fetcher, onEvent, delay, recovery = {}) {
  let cursor = Math.max(0, Number(recovery.cursor) || 0);
  let created = Math.max(0, Number(recovery.created) || 0);
  let skipped = Math.max(0, Number(recovery.skipped) || 0);
  let failed = Math.max(0, Number(recovery.failed) || 0);
  let consecutiveNetworkErrors = 0;
  const results = Array.isArray(recovery.results) ? [...recovery.results] : [];
  const signal = recovery.signal;

  for (;;) {
    let view;
    try {
      const response = await fetcher(
        `/admin/oauth/credentials/import/jobs/${encodeURIComponent(jobID)}?after=${cursor}`,
        { method: 'GET', signal }
      );
      view = await parseOAuthCredentialImportResponse(response);
      consecutiveNetworkErrors = 0;
    } catch (error) {
      if (signal?.aborted) throw error;
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
      ), signal);
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
    await delay(OAUTH_CREDENTIAL_IMPORT_POLL_INTERVAL_MS, signal);
  }
}

async function readXAICredentialImportStream(response, onEvent, recovery) {
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

  const state = {
    jobID: '', total: 0, cursor: 0, created: 0, skipped: 0, failed: 0, results: []
  };
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
    if (event.job_id) state.jobID = event.job_id;
    state.total = Math.max(state.total, Number(event.total) || 0);
    if (event.event === 'progress' && event.result) {
      state.results.push(event.result);
      state.cursor = Math.max(state.cursor + 1, Number(event.processed) || 0);
      state.created = Math.max(0, Number(event.created) || 0);
      state.skipped = Math.max(0, Number(event.skipped) || 0);
      state.failed = Math.max(0, Number(event.failed) || 0);
    }
    if (event.event === 'complete') complete = event;
    onEvent(event);
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
  const resume = async error => {
    if (!state.jobID || recovery.signal?.aborted) throw error;
    onEvent({
      event: 'reconnecting', processed: state.cursor, total: state.total,
      created: state.created, skipped: state.skipped, failed: state.failed
    });
    return pollOAuthCredentialImportJob(
      state.jobID,
      state.total,
      recovery.fetcher,
      onEvent,
      recovery.delay,
      { ...state, signal: recovery.signal }
    );
  };

  try {
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
  } catch (error) {
    return resume(error);
  }
  if (!complete) {
    return resume(new Error(window.t('channels.oauth.importStreamIncomplete')));
  }
  return {
    created: Number(complete.created) || 0,
    skipped: Number(complete.skipped) || 0,
    failed: Number(complete.failed) || 0,
    results: state.results
  };
}

function oauthCredentialCleanupProviderLabel(authType) {
  return ({
    codex_oauth: 'Codex',
    antigravity_oauth: 'Antigravity',
    xai_oauth: 'xAI',
    anthropic_oauth: 'Anthropic',
    zai_oauth: 'Z.ai',
    cursor_oauth: 'Cursor'
  })[authType] || authType;
}

function openOAuthCredentialCleanupDialog(trigger = null) {
  const dialog = document.getElementById('oauthCredentialCleanupDialog');
  const authType = document.getElementById('oauthCredentialCleanupAuthType');
  if (!dialog) return false;
  oauthCredentialCleanupDialogTrigger = trigger;
  if (!dialog.open && typeof dialog.showModal === 'function') dialog.showModal();
  authType?.focus?.();
  return true;
}

function closeOAuthCredentialCleanupDialog() {
  const dialog = document.getElementById('oauthCredentialCleanupDialog');
  if (dialog?.open) dialog.close();
  const trigger = oauthCredentialCleanupDialogTrigger;
  oauthCredentialCleanupDialogTrigger = null;
  trigger?.focus?.();
}

function setOAuthCredentialCleanupButtonState(button, state) {
  if (!button) return;
  const label = button.querySelector?.('span') || button;
  button.dataset.state = state;
  button.removeAttribute?.('aria-busy');
  button.removeAttribute?.('aria-disabled');
  if (state === 'running') {
    if (label.dataset) label.dataset.i18n = 'channels.oauth.cleanupStop';
    if (button.dataset) button.dataset.i18nTitle = 'channels.oauth.cleanupStopTitle';
    label.textContent = window.t('channels.oauth.cleanupStop');
    button.title = window.t('channels.oauth.cleanupStopTitle');
    button.disabled = false;
    return;
  }
  if (state === 'stopping') {
    if (label.dataset) label.dataset.i18n = 'channels.oauth.cleanupStopping';
    if (button.dataset) button.dataset.i18nTitle = 'channels.oauth.cleanupStopTitle';
    label.textContent = window.t('channels.oauth.cleanupStopping');
    button.title = window.t('channels.oauth.cleanupStopTitle');
    button.disabled = false;
    button.setAttribute?.('aria-disabled', 'true');
    button.setAttribute?.('aria-busy', 'true');
    return;
  }
  if (label.dataset) label.dataset.i18n = 'channels.oauth.cleanupStart';
  if (button.dataset) button.dataset.i18nTitle = 'channels.oauth.cleanupTitle';
  label.textContent = window.t('channels.oauth.cleanupStart');
  button.title = window.t('channels.oauth.cleanupTitle');
  button.disabled = false;
}

function replaceOAuthCredentialCleanupModelOptions(select, models, placeholderKey) {
  if (!select) return;
  const placeholder = document.createElement('option');
  placeholder.value = '';
  placeholder.textContent = window.t(placeholderKey);
  const options = [placeholder];
  for (const modelName of models) {
    const option = document.createElement('option');
    option.value = modelName;
    option.textContent = modelName;
    options.push(option);
  }
  select.replaceChildren(...options);
  select.value = '';
}

async function loadOAuthCredentialCleanupModels(
  authType,
  modelSelect,
  cleanupButton,
  fetcher = fetchDataWithAuth
) {
  const selectedModel = String(modelSelect?.value || '').trim();
  const sequence = ++oauthCredentialCleanupModelLoadSequence;
  replaceOAuthCredentialCleanupModelOptions(modelSelect, [], 'channels.oauth.cleanupModelsLoading');
  modelSelect.disabled = true;
  if (!activeOAuthCredentialCleanup) cleanupButton.disabled = true;
  try {
    const result = await fetcher(
      `/admin/oauth/credentials/cleanup/options?auth_type=${encodeURIComponent(authType)}`
    );
    if (sequence !== oauthCredentialCleanupModelLoadSequence) return null;
    const models = [...new Set(Array.isArray(result?.models)
      ? result.models.map(value => String(value).trim()).filter(Boolean)
      : [])];
    oauthCredentialCleanupOptions = { models };
    replaceOAuthCredentialCleanupModelOptions(
      modelSelect,
      models,
      models.length > 0
        ? 'channels.oauth.cleanupSelectModel'
        : 'channels.oauth.cleanupNoModelsForType'
    );
    if (models.includes(selectedModel)) modelSelect.value = selectedModel;
    modelSelect.disabled = models.length === 0 || Boolean(activeOAuthCredentialCleanup);
    if (!activeOAuthCredentialCleanup) cleanupButton.disabled = modelSelect.value.trim() === '';
    return result;
  } catch (error) {
    if (sequence !== oauthCredentialCleanupModelLoadSequence) return null;
    oauthCredentialCleanupOptions = { models: [] };
    replaceOAuthCredentialCleanupModelOptions(modelSelect, [], 'channels.oauth.cleanupModelsLoadFailed');
    modelSelect.disabled = true;
    if (!activeOAuthCredentialCleanup) cleanupButton.disabled = true;
    throw error;
  }
}

function getOAuthCredentialCleanupProgressElements() {
  return {
    container: document.getElementById('oauthCredentialCleanupProgress'),
    progress: document.getElementById('oauthCredentialCleanupProgressBar'),
    counter: document.getElementById('oauthCredentialCleanupProgressCounter'),
    detail: document.getElementById('oauthCredentialCleanupProgressDetail'),
    counts: document.getElementById('oauthCredentialCleanupProgressCounts'),
    results: document.getElementById('oauthCredentialCleanupResults')
  };
}

function resetOAuthCredentialCleanupProgress() {
  const { container, progress, counter, detail, counts, results } = getOAuthCredentialCleanupProgressElements();
  if (container) {
    container.hidden = false;
    delete container.dataset.kind;
  }
  if (progress) {
    progress.max = 1;
    progress.value = 0;
  }
  if (counter) counter.textContent = window.t('channels.oauth.cleanupCounter', { processed: 0, total: 0 });
  if (detail) detail.textContent = window.t('channels.oauth.cleanupStarting');
  if (counts) counts.textContent = window.t('channels.oauth.cleanupCounts', {
    healthy: 0, refreshed: 0, disabled: 0, deleted: 0, failed: 0, skipped: 0
  });
  if (results) results.replaceChildren();
}

function appendOAuthCredentialCleanupResult(event, results) {
  if (!results || event.event !== 'progress') return;
  const item = document.createElement('li');
  const status = window.t(`channels.oauth.cleanupStatus.${event.status || 'failed'}`);
  item.dataset.status = event.status || 'failed';
  item.textContent = window.t('channels.oauth.cleanupResult', {
    channel: event.channel_name || `#${event.channel_id || ''}`,
    status
  });
  if (event.error) item.textContent += ` · ${event.error}`;
  results.append(item);
}

function updateOAuthCredentialCleanupProgress(event) {
  if (!event || typeof event !== 'object') return;
  const { container, progress, counter, detail, counts, results } = getOAuthCredentialCleanupProgressElements();
  const total = Math.max(0, Number(event.total) || 0);
  const processed = Math.min(total, Math.max(0, Number(event.processed) || 0));
  const summary = {
    healthy: Math.max(0, Number(event.healthy) || 0),
    refreshed: Math.max(0, Number(event.refreshed) || 0),
    disabled: Math.max(0, Number(event.disabled) || 0),
    deleted: Math.max(0, Number(event.deleted) || 0),
    failed: Math.max(0, Number(event.failed) || 0),
    skipped: Math.max(0, Number(event.skipped) || 0)
  };
  if (container) container.hidden = false;
  if (progress) {
    progress.max = Math.max(1, total);
    progress.value = processed;
  }
  if (counter) counter.textContent = window.t('channels.oauth.cleanupCounter', { processed, total });
  if (counts) counts.textContent = window.t('channels.oauth.cleanupCounts', summary);
  appendOAuthCredentialCleanupResult(event, results);
  if (!detail) return;

  const stageData = {
    channel: event.channel_name || `#${event.channel_id || ''}`,
    model: event.model || window.t('channels.oauth.cleanupNoModelSelected')
  };
  switch (event.event) {
    case 'start':
      detail.textContent = window.t('channels.oauth.cleanupStarted', { total });
      break;
    case 'testing':
    case 'refreshing':
    case 'disabling':
    case 'deleting':
    case 'retesting':
      detail.textContent = window.t(`channels.oauth.cleanupStage.${event.event}`, stageData);
      break;
    case 'progress':
      detail.textContent = window.t('channels.oauth.cleanupProcessed', {
        channel: stageData.channel,
        status: window.t(`channels.oauth.cleanupStatus.${event.status || 'failed'}`)
      });
      break;
    case 'reconnecting':
      detail.textContent = window.t('channels.oauth.cleanupReconnecting');
      break;
    case 'complete':
      if (event.status === 'cancelled') {
        detail.textContent = window.t('channels.oauth.cleanupStopped');
        if (container) container.dataset.kind = 'warning';
      } else {
        detail.textContent = event.status === 'succeeded'
          ? window.t('channels.oauth.cleanupComplete')
          : (event.error || window.t('channels.oauth.cleanupFailed'));
        if (container) container.dataset.kind = event.status === 'succeeded' ? 'success' : 'error';
      }
      break;
    default:
      break;
  }
}

async function readAdminSSEStream(response, onEvent, failedKey, incompleteKey, eventErrorStatus = 400) {
  if (!response?.ok) {
    await parseAdminJobResponse(response, failedKey);
    throw new Error(window.t(failedKey));
  }

  let buffer = '';
  let complete = null;
  const consumeBlock = block => {
    const data = block
      .split(/\r?\n/)
      .filter(line => line.startsWith('data:'))
      .map(line => line.slice(5).trimStart())
      .join('\n');
    if (!data) return;
    const event = JSON.parse(data);
    if (event.event === 'error') {
      throw new OAuthCredentialImportResponseError(
        event.error || window.t(failedKey),
        eventErrorStatus
      );
    }
    if (event.event === 'complete') complete = event;
    onEvent(event);
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
      if (complete) {
        try { await reader.cancel?.(); } catch (_) {}
        return complete;
      }
    }
    buffer += decoder.decode();
  } else {
    buffer = await response.text();
  }
  drain(true);
  if (!complete) throw new Error(window.t(incompleteKey));
  return complete;
}

async function readOAuthCredentialCleanupStream(response, onEvent) {
  return readAdminSSEStream(
    response,
    onEvent,
    'channels.oauth.cleanupFailed',
    'channels.oauth.cleanupStreamIncomplete',
    404
  );
}

async function followOAuthCredentialCleanupJob(jobID, total, fetcher, onEvent, delay = codexOAuthDelay) {
  let cursor = 0;
  let consecutiveNetworkErrors = 0;
  let latest = { processed: 0, total, healthy: 0, refreshed: 0, disabled: 0, deleted: 0, failed: 0, skipped: 0 };
  for (;;) {
    try {
      const response = await fetcher(
        `/admin/oauth/credentials/cleanup/jobs/${encodeURIComponent(jobID)}/stream?after=${cursor}`,
        { method: 'GET', headers: { Accept: 'text/event-stream' } }
      );
      const complete = await readOAuthCredentialCleanupStream(response, event => {
        cursor = Math.max(cursor, Number(event.sequence) || 0);
        latest = { ...latest, ...event };
        consecutiveNetworkErrors = 0;
        onEvent(event);
      });
      if (complete.status !== 'succeeded' && complete.status !== 'cancelled') {
        throw new OAuthCredentialImportResponseError(
          complete.error || window.t('channels.oauth.cleanupFailed'),
          400
        );
      }
      const summary = {
        healthy: Number(complete.healthy) || 0,
        refreshed: Number(complete.refreshed) || 0,
        disabled: Number(complete.disabled) || 0,
        deleted: Number(complete.deleted) || 0,
        failed: Number(complete.failed) || 0,
        skipped: Number(complete.skipped) || 0,
        total: Number(complete.total) || total
      };
      if (complete.status === 'cancelled') summary.cancelled = true;
      return summary;
    } catch (error) {
      if (error instanceof OAuthCredentialImportResponseError && error.status < 500) {
        throw error;
      }
      consecutiveNetworkErrors++;
      onEvent({ ...latest, event: 'reconnecting' });
      await delay(Math.min(OAUTH_CREDENTIAL_IMPORT_POLL_INTERVAL_MS * consecutiveNetworkErrors, 5000));
    }
  }
}

async function cleanupOAuthCredentials(
  authType,
  modelName,
  action = 'disable',
  fetcher = fetchWithAuth,
  onEvent = updateOAuthCredentialCleanupProgress,
  delay = codexOAuthDelay,
  onStarted = null
) {
  action = action === 'delete' ? 'delete' : 'disable';
  const requestID = typeof globalThis.crypto?.randomUUID === 'function'
    ? globalThis.crypto.randomUUID()
    : `cleanup-${Date.now()}-${Math.random().toString(36).slice(2)}`;
  let attempts = 0;
  for (;;) {
    try {
      const response = await fetcher('/admin/oauth/credentials/cleanup/jobs', {
        method: 'POST',
        headers: {
          Accept: 'application/json',
          'Content-Type': 'application/json',
          'Idempotency-Key': requestID
        },
        body: JSON.stringify({ auth_type: authType, model: modelName, action })
      });
      const started = await parseAdminJobResponse(response, 'channels.oauth.cleanupFailed');
      if (!started?.job_id) {
        throw new OAuthCredentialImportResponseError(window.t('channels.oauth.cleanupFailed'), 400);
      }
      if (typeof onStarted === 'function') onStarted(started);
      return followOAuthCredentialCleanupJob(started.job_id, Number(started.total) || 0, fetcher, onEvent, delay);
    } catch (error) {
      const status = error instanceof OAuthCredentialImportResponseError ? error.status : 0;
      const retryable = status === 0 || status === 408 ||
        (status >= 200 && status < 300) || status >= 500;
      if (!retryable) {
        throw error;
      }
      attempts++;
      onEvent({ event: 'reconnecting', processed: 0, total: 0 });
      await delay(Math.min(OAUTH_CREDENTIAL_IMPORT_POLL_INTERVAL_MS * attempts, 5000));
    }
  }
}

async function cancelOAuthCredentialCleanup(
  jobID,
  fetcher = fetchWithAuth,
  delay = codexOAuthDelay,
  signal = undefined
) {
  let attempts = 0;
  for (;;) {
    if (signal?.aborted) throw signal.reason || new Error('Credential cleanup cancellation was aborted');
    try {
      const response = await fetcher(
        `/admin/oauth/credentials/cleanup/jobs/${encodeURIComponent(jobID)}/cancel`,
        { method: 'POST', headers: { Accept: 'application/json' }, signal }
      );
      return await parseAdminJobResponse(response, 'channels.oauth.cleanupStopFailed');
    } catch (error) {
      if (signal?.aborted) throw signal.reason || error;
      const status = error instanceof OAuthCredentialImportResponseError ? error.status : 0;
      const retryable = status === 0 || status === 408 ||
        (status >= 200 && status < 300) || status >= 500;
      if (!retryable) throw error;
      attempts++;
      await delay(Math.min(OAUTH_CREDENTIAL_IMPORT_POLL_INTERVAL_MS * attempts, 5000), signal);
    }
  }
}

async function refreshOAuthCredential(channelID, fetcher = fetchDataWithAuth, authType = 'codex_oauth') {
  const numericID = Number(channelID);
  const antigravity = authType === 'antigravity_oauth';
  const anthropic = authType === 'anthropic_oauth';
  if (!Number.isInteger(numericID) || numericID <= 0) {
    throw new Error(`A saved ${anthropic ? 'Anthropic' : (antigravity ? 'Antigravity' : 'Codex')} channel is required`);
  }
  const resource = anthropic ? 'anthropic-credential' : (antigravity ? 'antigravity-credential' : 'codex-credential');
  return fetcher(`/admin/channels/${numericID}/${resource}/refresh`, { method: 'POST' });
}

async function updateCodexQuotaOverdraft(channelID, enabled, fetcher = fetchDataWithAuth) {
  const numericID = Number(channelID);
  if (!Number.isInteger(numericID) || numericID <= 0) {
    throw new Error('A saved Codex channel is required');
  }
  return fetcher(`/admin/channels/${numericID}/codex-quota-overdraft`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ enabled: enabled === true })
  });
}

function resetCodexQuotaOverdraftDraft() {
  renderCodexQuotaOverdraft(currentOAuthCredential, editingChannelAuthType === 'codex_oauth');
}

async function saveCodexQuotaOverdraftFromAdvancedSettings(
  channelID = editingChannelId,
  fetcher = fetchDataWithAuth
) {
  const settings = document.getElementById('codexQuotaOverdraftSettings');
  const checkbox = document.getElementById('codexQuotaOverdraftEnabled');
  if (!settings || settings.hidden || !checkbox) return null;

  const enabled = checkbox.checked === true;
  const persisted = currentOAuthCredential?.quota_overdraft || {};
  if (enabled === (persisted.enabled === true)) return persisted;

  try {
    const result = await updateCodexQuotaOverdraft(channelID, enabled, fetcher);
    if (!result?.quota_overdraft) {
      throw new Error(window.t('channels.codex.quotaOverdraftSaveFailed'));
    }
    currentOAuthCredential = {
      ...(currentOAuthCredential || {}),
      quota_overdraft: result.quota_overdraft
    };
    renderCurrentOAuthCredential();
    renderCodexQuotaOverdraft(currentOAuthCredential, true);
    return result.quota_overdraft;
  } catch (error) {
    renderCodexQuotaOverdraft(currentOAuthCredential, true);
    throw error;
  }
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
  const operationID = ++oauthUsageOperationSequence;
  oauthUsageOperationByChannelID.set(numericID, operationID);
  oauthUsageStateByChannelID.set(numericID, { status: 'loading' });
  rerenderOAuthUsage();
  try {
    const result = await fetcher(`/admin/channels/${numericID}/oauth-usage`, { method: 'POST' });
    if (!result || !Array.isArray(result.windows)) {
      throw new Error(window.t('channels.oauth.usageInvalid'));
    }
    if (oauthUsageOperationByChannelID.get(numericID) !== operationID) return result;
    oauthUsageOperationByChannelID.delete(numericID);
    oauthUsageStateByChannelID.set(numericID, { status: 'ready', data: result });
    if (options.reload !== false && typeof loadChannels === 'function') {
      await loadChannels();
    } else {
      rerenderOAuthUsage();
    }
    return result;
  } catch (error) {
    const message = error?.message || window.t('channels.oauth.usageFailed');
    if (oauthUsageOperationByChannelID.get(numericID) === operationID) {
      oauthUsageOperationByChannelID.delete(numericID);
      oauthUsageStateByChannelID.set(numericID, { status: 'error', error: message });
      rerenderOAuthUsage();
    }
    throw error;
  }
}

async function resetCodexQuota(channelID, fetcher = fetchDataWithAuth, options = {}) {
  const numericID = Number(channelID);
  if (!Number.isInteger(numericID) || numericID <= 0) {
    throw new Error('A saved Codex channel is required');
  }
  const channelList = typeof channels !== 'undefined' && Array.isArray(channels) ? channels : [];
  const persistedUsage = channelList.find(channel => Number(channel?.id) === numericID)?.oauth_usage;
  const previous = oauthUsageStateByChannelID.get(numericID) ||
    (persistedUsage ? { status: 'ready', data: persistedUsage } : null);
  const operationID = ++oauthUsageOperationSequence;
  oauthUsageOperationByChannelID.set(numericID, operationID);
  oauthUsageStateByChannelID.set(numericID, {
    ...(previous || {}),
    status: previous?.data ? 'ready' : (previous?.status || 'loading'),
    reset_status: 'loading',
    reset_error: ''
  });
  rerenderOAuthUsage();
  try {
    const result = await fetcher(`/admin/channels/${numericID}/codex-quota-reset`, { method: 'POST' });
    if (!result || result.reset !== true) {
      throw new Error(window.t('channels.oauth.resetInvalid'));
    }
    if (oauthUsageOperationByChannelID.get(numericID) !== operationID) return result;
    oauthUsageOperationByChannelID.delete(numericID);
    const usage = result.usage;
    if (usage && Array.isArray(usage.windows)) {
      oauthUsageStateByChannelID.set(numericID, { status: 'ready', data: usage, reset_status: 'ready' });
    } else {
      oauthUsageStateByChannelID.set(numericID, {
        ...(previous || {}),
        status: previous?.data ? 'ready' : 'error',
        error: previous?.data ? previous.error : window.t('channels.oauth.resetNeedsRefresh'),
        reset_status: 'stale',
        reset_error: window.t('channels.oauth.resetNeedsRefresh')
      });
    }
    if (options.reload !== false && typeof loadChannels === 'function') {
      await loadChannels();
    } else {
      rerenderOAuthUsage();
    }
    return result;
  } catch (error) {
    const message = error?.message || window.t('channels.oauth.resetFailed');
    if (oauthUsageOperationByChannelID.get(numericID) === operationID) {
      oauthUsageOperationByChannelID.delete(numericID);
      if (previous?.data) {
        oauthUsageStateByChannelID.set(numericID, {
          ...previous,
          status: 'ready',
          reset_status: 'error',
          reset_error: message
        });
      } else {
        oauthUsageStateByChannelID.set(numericID, { status: 'error', error: message });
      }
      rerenderOAuthUsage();
    }
    throw error;
  }
}

async function refreshOAuthUsageBatch(channelIDs, fetcher = fetchWithAuth) {
  const ids = Array.from(new Set((channelIDs || [])
    .map(id => Number(id))
    .filter(id => Number.isInteger(id) && id > 0)));
  const summary = { total: ids.length, succeeded: 0, failed: 0 };
  if (ids.length === 0) return summary;

  const idSet = new Set(ids);
  const operationID = ++oauthUsageOperationSequence;
  ids.forEach(id => {
    oauthUsageOperationByChannelID.set(id, operationID);
    oauthUsageStateByChannelID.set(id, { status: 'loading' });
  });
  rerenderOAuthUsage();
  try {
    const response = await fetcher('/admin/channels/oauth-usage/batch/stream', {
      method: 'POST',
      headers: {
        Accept: 'text/event-stream',
        'Content-Type': 'application/json'
      },
      body: JSON.stringify({ channel_ids: ids })
    });
    const complete = await readAdminSSEStream(
      response,
      event => {
        if (event.event !== 'progress' || !event.result) return;
        const result = event.result;
        const channelID = Number(result.channel_id);
        if (!idSet.has(channelID) || oauthUsageOperationByChannelID.get(channelID) !== operationID) return;
        if (result.status === 'succeeded') {
          if (!result.usage || !Array.isArray(result.usage.windows)) {
            throw new Error(window.t('channels.oauth.usageInvalid'));
          }
          oauthUsageStateByChannelID.set(channelID, { status: 'ready', data: result.usage });
        } else {
          oauthUsageStateByChannelID.set(channelID, {
            status: 'error',
            error: result.error || window.t('channels.oauth.usageFailed')
          });
        }
        oauthUsageOperationByChannelID.delete(channelID);
        rerenderOAuthUsage();
      },
      'channels.batchOAuthUsageFailed',
      'channels.batchOAuthUsageIncomplete'
    );

    summary.succeeded = Number(complete.succeeded) || 0;
    summary.failed = Number(complete.failed) || 0;
    if (Number(complete.processed) !== ids.length || summary.succeeded + summary.failed !== ids.length) {
      throw new Error(window.t('channels.batchOAuthUsageIncomplete'));
    }

    if (typeof loadChannels === 'function') {
      await loadChannels();
    } else {
      rerenderOAuthUsage();
    }
    return summary;
  } catch (error) {
    const message = error?.message || window.t('channels.batchOAuthUsageIncomplete');
    ids.forEach(id => {
      if (oauthUsageOperationByChannelID.get(id) === operationID) {
        oauthUsageOperationByChannelID.delete(id);
        oauthUsageStateByChannelID.set(id, { status: 'error', error: message });
      }
    });
    rerenderOAuthUsage();
    throw error;
  }
}

async function batchRefreshSelectedOAuthUsage(fetcher = fetchWithAuth) {
  const selectedIDs = typeof getSelectedChannelIDs === 'function' ? getSelectedChannelIDs() : [];
  if (selectedIDs.length === 0) {
    if (window.showWarning) window.showWarning(window.t('channels.batchNoSelection'));
    return null;
  }

  const channelList = typeof channels !== 'undefined' && Array.isArray(channels) ? channels : [];
  const eligibleIDs = selectedIDs.filter(id => {
    const channel = channelList.find(item => Number(item.id) === id);
    return channel && ['codex_oauth', 'antigravity_oauth', 'xai_oauth', 'anthropic_oauth', 'zai_oauth', 'cursor_oauth'].includes(channel.auth_type);
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
  const codexMethod = document.getElementById('codexOAuthMethod');
  const codexPersonalAccessToken = document.getElementById('codexPersonalAccessToken');
  const xaiMethod = document.getElementById('xaiOAuthMethod');
  const xaiCredentialValues = document.getElementById('xaiCredentialValues');
  const anthropicMethod = document.getElementById('anthropicOAuthMethod');
  const anthropicSessionKey = document.getElementById('anthropicSessionKey');
  const zaiMethod = document.getElementById('zaiOAuthMethod');
  const zaiCodingPlanKey = document.getElementById('zaiCodingPlanKey');
  const cursorUserAPIKey = document.getElementById('cursorUserAPIKey');
  const authorizeButton = document.getElementById('oauthAuthorizeButton');
  const sessionFields = document.getElementById('oauthSessionFields');
  const copyButton = document.getElementById('oauthCopyLink');
  const restartButton = document.getElementById('oauthRestart');
  const authorizationURL = document.getElementById('oauthAuthorizationURL');
  const callbackForm = document.getElementById('oauthCallbackForm');
  const callbackURL = document.getElementById('oauthCallbackURL');
  const callbackButton = document.getElementById('oauthSubmitCallback');
  const importButton = document.getElementById('oauthCredentialImportBtn');
  const cleanupOpenButton = document.getElementById('oauthCredentialCleanupOpenBtn');
  const cleanupDialog = document.getElementById('oauthCredentialCleanupDialog');
  const cleanupForm = document.getElementById('oauthCredentialCleanupForm');
  const cleanupButton = document.getElementById('oauthCredentialCleanupBtn');
  const cleanupAuthType = document.getElementById('oauthCredentialCleanupAuthType');
  const cleanupModel = document.getElementById('oauthCredentialCleanupModel');
  const cleanupAction = document.getElementById('oauthCredentialCleanupAction');
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
  if (providerSelect && typeof providerSelect.addEventListener === 'function' && !providerSelect.dataset?.oauthBound) {
    providerSelect.addEventListener('change', syncOAuthProviderFields);
    if (providerSelect.dataset) providerSelect.dataset.oauthBound = '1';
  }
  if (xaiMethod && !xaiMethod.dataset.bound) {
    xaiMethod.addEventListener('change', syncOAuthProviderFields);
    xaiMethod.dataset.bound = '1';
  }
  if (codexMethod && !codexMethod.dataset.bound) {
    codexMethod.addEventListener('change', syncOAuthProviderFields);
    codexMethod.dataset.bound = '1';
  }
  if (anthropicMethod && !anthropicMethod.dataset.bound) {
    anthropicMethod.addEventListener('change', syncOAuthProviderFields);
    anthropicMethod.dataset.bound = '1';
  }
  if (zaiMethod && !zaiMethod.dataset.bound) {
    zaiMethod.addEventListener('change', syncOAuthProviderFields);
    zaiMethod.dataset.bound = '1';
  }
  if (loginForm && providerSelect && authorizeButton && !loginForm.dataset.bound) {
    loginForm.addEventListener('submit', async event => {
      event.preventDefault();
      if (activeCodexOAuthFlow || activeCodexPersonalAccessTokenFlow || activeXAIImportFlow ||
        activeAnthropicCookieFlow || activeZAIKeyFlow || activeCursorImportFlow) return;
      providerSelect.disabled = true;
      if (providerSelect.value === 'codex' && codexMethod?.value === 'personalAccessToken') {
        const controller = typeof AbortController === 'function' ? new AbortController() : null;
        const flow = {
          button: authorizeButton, input: codexPersonalAccessToken, cancelling: false, controller
        };
        activeCodexPersonalAccessTokenFlow = flow;
        codexMethod.disabled = true;
        authorizeButton.disabled = true;
        authorizeButton.setAttribute?.('aria-busy', 'true');
        try {
          setCodexOAuthDialogStatus(window.t('channels.codex.personalAccessTokenValidating'));
          await submitCodexPersonalAccessToken(codexPersonalAccessToken, fetchDataWithAuth, controller?.signal);
          if (flow.cancelling || activeCodexPersonalAccessTokenFlow !== flow) return;
          closeOAuthLoginDialogElement();
          setCodexAuthStatus(window.t('channels.codex.personalAccessTokenComplete'), 'success');
          if (window.showSuccess) window.showSuccess(window.t('channels.codex.personalAccessTokenComplete'));
          try {
            await reloadChannelsList({ throwOnError: true });
          } catch {
            if (flow.cancelling || activeCodexPersonalAccessTokenFlow !== flow) return;
            const message = window.t('channels.codex.personalAccessTokenReloadFailed');
            setCodexAuthStatus(message, 'error');
            if (window.showError) window.showError(message);
          }
        } catch (error) {
          if (flow.cancelling || activeCodexPersonalAccessTokenFlow !== flow) return;
          codexPersonalAccessToken?.setAttribute?.('aria-invalid', 'true');
          codexPersonalAccessToken?.focus?.();
          const message = error?.message || window.t('channels.codex.personalAccessTokenFailed');
          setCodexOAuthDialogStatus(message, 'error');
          if (window.showError) window.showError(message);
        } finally {
          if (activeCodexPersonalAccessTokenFlow === flow) activeCodexPersonalAccessTokenFlow = null;
          codexMethod.disabled = false;
          authorizeButton.disabled = false;
          authorizeButton.removeAttribute?.('aria-busy');
        }
      } else if (providerSelect.value === 'xai') {
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
      } else if (providerSelect.value === 'cursor') {
        const input = cursorUserAPIKey;
        const controller = typeof AbortController === 'function' ? new AbortController() : null;
        const flow = { button: authorizeButton, input, cancelling: false, controller };
        activeCursorImportFlow = flow;
        authorizeButton.disabled = true;
        authorizeButton.setAttribute?.('aria-busy', 'true');
        try {
          setCodexOAuthDialogStatus(window.t('channels.cursor.apiKeyValidating'));
          const result = await submitCursorCredential(input, fetchDataWithAuth, controller?.signal);
          if (flow.cancelling || activeCursorImportFlow !== flow) return;
          closeOAuthLoginDialogElement();
          const message = window.t('channels.cursor.importComplete', { channel: result?.channel_name || '' });
          setCodexAuthStatus(message, 'success');
          if (window.showSuccess) window.showSuccess(message);
          await reloadChannelsList();
        } catch (error) {
          if (flow.cancelling || activeCursorImportFlow !== flow) return;
          input?.setAttribute?.('aria-invalid', 'true');
          input?.focus?.();
          const message = error?.message || window.t('channels.cursor.importFailed');
          setCodexOAuthDialogStatus(message, 'error');
          if (window.showError) window.showError(message);
        } finally {
          if (activeCursorImportFlow === flow) activeCursorImportFlow = null;
          authorizeButton.disabled = false;
          authorizeButton.removeAttribute?.('aria-busy');
        }
      } else if (providerSelect.value === 'zai' && zaiMethod?.value === 'api_key') {
        const controller = typeof AbortController === 'function' ? new AbortController() : null;
        const flow = { button: authorizeButton, input: zaiCodingPlanKey, cancelling: false, controller };
        activeZAIKeyFlow = flow;
        zaiMethod.disabled = true;
        authorizeButton.disabled = true;
        authorizeButton.setAttribute?.('aria-busy', 'true');
        try {
          setCodexOAuthDialogStatus(window.t('channels.zai.apiKeyValidating'));
          const result = await submitZAICodingPlanKey(zaiCodingPlanKey, fetchDataWithAuth, controller?.signal);
          if (flow.cancelling || activeZAIKeyFlow !== flow) return;
          closeOAuthLoginDialogElement();
          const message = window.t('channels.zai.apiKeyComplete', { channel: result?.channel_name || '' });
          setCodexAuthStatus(message, 'success');
          if (window.showSuccess) window.showSuccess(message);
          await reloadChannelsList();
        } catch (error) {
          if (flow.cancelling || activeZAIKeyFlow !== flow) return;
          zaiCodingPlanKey?.setAttribute?.('aria-invalid', 'true');
          zaiCodingPlanKey?.focus?.();
          const message = error?.message || window.t('channels.zai.apiKeyFailed');
          setCodexOAuthDialogStatus(message, 'error');
          if (window.showError) window.showError(message);
        } finally {
          if (activeZAIKeyFlow === flow) activeZAIKeyFlow = null;
          zaiMethod.disabled = false;
          authorizeButton.disabled = false;
          authorizeButton.removeAttribute?.('aria-busy');
        }
      } else if (providerSelect.value === 'anthropic' && anthropicMethod?.value === 'cookie') {
        const controller = typeof AbortController === 'function' ? new AbortController() : null;
        const flow = { button: authorizeButton, input: anthropicSessionKey, cancelling: false, controller };
        activeAnthropicCookieFlow = flow;
        anthropicMethod.disabled = true;
        authorizeButton.disabled = true;
        authorizeButton.setAttribute?.('aria-busy', 'true');
        try {
          const result = await submitAnthropicCookieAuth(
            anthropicSessionKey,
            fetchDataWithAuth,
            controller?.signal,
            progress => setCodexOAuthDialogStatus(
              window.t('channels.anthropic.cookieAuthorizing', progress)
            )
          );
          if (flow.cancelling || activeAnthropicCookieFlow !== flow) return;
          const successful = result.created + result.updated;
          let resultMessage = '';
          if (result.failed > 0) {
            const details = result.failedDetails
              .map(detail => window.t('channels.anthropic.cookieFailureDetail', detail))
              .join('\n');
            resultMessage = window.t('channels.anthropic.cookiePartial', {
              ...result,
              details
            });
            anthropicSessionKey?.setAttribute?.('aria-invalid', 'true');
            anthropicSessionKey?.focus?.();
            setCodexOAuthDialogStatus(resultMessage, 'error');
          } else {
            resultMessage = window.t('channels.anthropic.cookieComplete', result);
            setCodexOAuthDialogStatus(resultMessage, 'success');
          }
          if (successful > 0) {
            try {
              await reloadChannelsList({ throwOnError: true });
            } catch {
              if (flow.cancelling || activeAnthropicCookieFlow !== flow) return;
              setCodexOAuthDialogStatus(window.t('channels.anthropic.cookieReloadFailedWithResult', {
                result: resultMessage
              }), 'error');
            }
          }
          return result;
        } catch (error) {
          if (flow.cancelling || activeAnthropicCookieFlow !== flow) return;
          anthropicSessionKey?.setAttribute?.('aria-invalid', 'true');
          anthropicSessionKey?.focus?.();
          const message = error?.message || window.t('channels.anthropic.cookieFailed');
          setCodexOAuthDialogStatus(message, 'error');
        } finally {
          if (activeAnthropicCookieFlow === flow) activeAnthropicCookieFlow = null;
          anthropicMethod.disabled = false;
          authorizeButton.disabled = false;
          authorizeButton.removeAttribute?.('aria-busy');
          if (loginDialog?.open && sessionFields?.hidden) providerSelect.disabled = false;
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
      const provider = activeCodexOAuthFlow?.provider || oauthProviderConfig(providerSelect?.value).provider;
      const providerConfig = oauthProviderConfig(provider);
      if (!value) {
        callbackURL.setAttribute('aria-invalid', 'true');
        callbackURL.focus();
        setCodexOAuthDialogStatus(window.t(providerConfig.authorizationCode
          ? 'channels.anthropic.authorizationCodeRequired' : 'channels.oauth.callbackRequired'), 'error');
        return;
      }
      callbackURL.removeAttribute('aria-invalid');
      try {
        if (callbackButton) callbackButton.disabled = true;
        setCodexOAuthDialogStatus(window.t(providerConfig.authorizationCode
          ? 'channels.anthropic.authorizationCodeSubmitting' : 'channels.oauth.callbackSubmitting'));
        await submitOAuthCallback(provider, value, fetchDataWithAuth, activeCodexOAuthFlow?.state || '');
        setCodexOAuthDialogStatus(window.t(providerConfig.authorizationCode
          ? 'channels.anthropic.authorizationCodeAccepted' : 'channels.oauth.callbackAccepted'), 'success');
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
      clearCodexPersonalAccessToken();
      clearXAICredentialSecrets();
      void stopActiveOAuth({ closeDialog: false }).catch(() => {});
    });
    oauthPagehideBound = true;
  }
  if (importButton && !importButton.dataset.bound) {
    importButton.addEventListener('click', () => openOAuthCredentialImportDialog(importButton));
    importButton.dataset.bound = '1';
  }
  if (cleanupForm && cleanupButton && cleanupAuthType && cleanupModel && cleanupAction && !cleanupForm.dataset.bound) {
    const requestCleanupStop = async flow => {
      if (!flow?.jobID || flow.cancelPromise) return flow?.cancelPromise;
      flow.cancelPromise = cancelOAuthCredentialCleanup(
        flow.jobID,
        fetchWithAuth,
        codexOAuthDelay,
        flow.cancelController?.signal
      ).catch(error => {
        if (flow.cancelController?.signal.aborted || activeOAuthCredentialCleanup !== flow) return null;
        flow.stopRequested = false;
        flow.cancelPromise = null;
        setOAuthCredentialCleanupButtonState(cleanupButton, 'running');
        if (window.showError) {
          window.showError(error?.message || window.t('channels.oauth.cleanupStopFailed'));
        }
        return null;
      });
      return flow.cancelPromise;
    };
    const refreshCleanupModels = async () => {
      cleanupModel.removeAttribute?.('aria-invalid');
      try {
        await loadOAuthCredentialCleanupModels(
          cleanupAuthType.value,
          cleanupModel,
          cleanupButton
        );
      } catch (error) {
        console.warn('Failed to load OAuth credential cleanup models', error);
      }
    };
    if (cleanupOpenButton && !cleanupOpenButton.dataset.bound) {
      cleanupOpenButton.addEventListener('click', () => {
        if (!openOAuthCredentialCleanupDialog(cleanupOpenButton)) return;
        if (!activeOAuthCredentialCleanup && cleanupModel.options.length <= 1) {
          void refreshCleanupModels();
        }
      });
      cleanupOpenButton.dataset.bound = '1';
    }
    cleanupAuthType.addEventListener('change', () => { void refreshCleanupModels(); });
    const syncCleanupButtonForModel = () => {
      cleanupModel.removeAttribute?.('aria-invalid');
      if (!activeOAuthCredentialCleanup) cleanupButton.disabled = cleanupModel.value.trim() === '';
    };
    cleanupModel.addEventListener('change', syncCleanupButtonForModel);
    cleanupForm.addEventListener('submit', async event => {
      event.preventDefault();
      if (activeOAuthCredentialCleanup) {
        const flow = activeOAuthCredentialCleanup;
        if (flow.stopRequested) return;
        flow.stopRequested = true;
        setOAuthCredentialCleanupButtonState(cleanupButton, 'stopping');
        const { detail } = getOAuthCredentialCleanupProgressElements();
        if (detail) detail.textContent = window.t('channels.oauth.cleanupStopping');
        if (flow.jobID) void requestCleanupStop(flow);
        return;
      }

      const authType = cleanupAuthType.value;
      const modelName = cleanupModel.value.trim();
      const action = cleanupAction.value === 'delete' ? 'delete' : 'disable';
      if (!modelName) {
        cleanupModel.setAttribute?.('aria-invalid', 'true');
        cleanupModel.focus?.();
        return;
      }
      if (!oauthCredentialCleanupOptions.models.includes(modelName)) {
        cleanupModel.setAttribute?.('aria-invalid', 'true');
        cleanupModel.focus?.();
        return;
      }
      const provider = cleanupAuthType.selectedOptions?.[0]?.textContent?.trim() ||
        oauthCredentialCleanupProviderLabel(authType);
      const confirmKey = action === 'delete'
        ? 'channels.oauth.cleanupConfirmDelete'
        : 'channels.oauth.cleanupConfirmDisable';
      if (typeof window.confirm === 'function' && !window.confirm(window.t(confirmKey, {
        provider,
        model: modelName
      }))) return;

      const flow = {
        jobID: '',
        stopRequested: false,
        cancelPromise: null,
        cancelController: typeof AbortController === 'function' ? new AbortController() : null
      };
      activeOAuthCredentialCleanup = flow;
      cleanupAuthType.disabled = true;
      cleanupModel.disabled = true;
      cleanupAction.disabled = true;
      setOAuthCredentialCleanupButtonState(cleanupButton, 'running');
      resetOAuthCredentialCleanupProgress();
      try {
        const result = await cleanupOAuthCredentials(
          authType,
          modelName,
          action,
          fetchWithAuth,
          updateOAuthCredentialCleanupProgress,
          codexOAuthDelay,
          started => {
            if (activeOAuthCredentialCleanup !== flow) return;
            flow.jobID = started.job_id;
            if (flow.stopRequested) void requestCleanupStop(flow);
          }
        );
        const message = result.cancelled
          ? window.t('channels.oauth.cleanupStopped')
          : window.t('channels.oauth.cleanupSummary', result);
        let reloadFailed = false;
        if ((result.disabled > 0 || result.deleted > 0) && typeof reloadChannelsList === 'function') {
          try {
            await reloadChannelsList({ throwOnError: true });
          } catch (error) {
            reloadFailed = true;
            console.warn('OAuth credential cleanup finished, but the channel list could not be reloaded', error);
          }
        }
        const notification = reloadFailed
          ? `${message} ${window.t('channels.oauth.cleanupReloadFailed')}`
          : message;
        if ((result.cancelled || result.failed > 0 || reloadFailed) && window.showWarning) {
          window.showWarning(notification);
        } else if (window.showSuccess) {
          window.showSuccess(message);
        }
      } catch (error) {
        const message = error?.message || window.t('channels.oauth.cleanupFailed');
        const { container, detail } = getOAuthCredentialCleanupProgressElements();
        if (container) container.dataset.kind = 'error';
        if (detail) detail.textContent = message;
        if (window.showError) window.showError(message);
        if (typeof reloadChannelsList === 'function') {
          try { await reloadChannelsList(); } catch (_) {}
        }
      } finally {
        flow.cancelController?.abort();
        activeOAuthCredentialCleanup = null;
        cleanupAuthType.disabled = false;
        cleanupAction.disabled = false;
        setOAuthCredentialCleanupButtonState(cleanupButton, 'idle');
        await refreshCleanupModels();
      }
    });
    cleanupForm.dataset.bound = '1';
  }
  document.querySelectorAll('[data-action="close-oauth-cleanup"]').forEach(closeButton => {
    if (closeButton.dataset.bound) return;
    closeButton.addEventListener('click', () => closeOAuthCredentialCleanupDialog());
    closeButton.dataset.bound = '1';
  });
  if (cleanupDialog && !cleanupDialog.dataset.cancelBound) {
    cleanupDialog.addEventListener('cancel', event => {
      event.preventDefault();
      closeOAuthCredentialCleanupDialog();
    });
    cleanupDialog.dataset.cancelBound = '1';
  }
  if (cleanupDialog && !cleanupDialog.dataset.overlayBound) {
    cleanupDialog.addEventListener('click', event => {
      if (event.target === cleanupDialog) closeOAuthCredentialCleanupDialog();
    });
    cleanupDialog.dataset.overlayBound = '1';
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
        const authType = ['antigravity_oauth', 'anthropic_oauth'].includes(editingChannelAuthType)
          ? editingChannelAuthType : 'codex_oauth';
        const antigravity = authType === 'antigravity_oauth';
        const anthropic = authType === 'anthropic_oauth';
        const credentialI18n = anthropic ? 'channels.anthropic' : (antigravity ? 'channels.antigravity' : 'channels.codex');
        const result = await refreshOAuthCredential(editingChannelId, fetchDataWithAuth, authType);
        const credential = result?.oauth_credential;
        if (!credential?.access_token) throw new Error(window.t(`${credentialI18n}.credentialRefreshInvalid`));

        if (typeof setInlineKeyTableDataFromAPI === 'function' && typeof renderInlineKeyTable === 'function') {
          setInlineKeyTableDataFromAPI([{
            channel_id: editingChannelId,
            key_index: 0,
            api_key: credential.access_token,
            note: anthropic ? 'Anthropic OAuth AT' : (antigravity ? 'Antigravity OAuth AT' : 'Codex OAuth AT'),
            key_strategy: 'sequential'
          }]);
          inlineKeyVisible = true;
          renderInlineKeyTable();
        }
        applyChannelAuthEditorMode(authType, credential, result, result.oauth_credential_info, previousView);
        await reloadChannelsList();
        if (window.showSuccess) window.showSuccess(window.t(`${credentialI18n}.credentialRefreshed`));
      } catch (error) {
        const credentialI18n = editingChannelAuthType === 'anthropic_oauth'
          ? 'channels.anthropic'
          : (editingChannelAuthType === 'antigravity_oauth' ? 'channels.antigravity' : 'channels.codex');
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
    cancelAnthropicOAuth,
    cancelCodexOAuth,
    cancelOAuthCredentialCleanup,
    cancelXAIOAuth,
    cancelZAIOAuth,
    cleanupOAuthCredentials,
    copyOAuthCredential,
    copyCodexOAuthLink,
    formatCodexPlanBadgeText,
    getOAuthUsageState,
    importOAuthCredentials,
    loadOAuthCredentialCleanupModels,
    openOAuthCredentialImportDialog,
    openOAuthLoginDialog,
    pollAntigravityOAuthStatus,
    pollAnthropicOAuthStatus,
    pollCodexOAuthStatus,
    pollXAIOAuthStatus,
    refreshOAuthCredential,
    refreshOAuthUsage,
    refreshOAuthUsageBatch,
    resetCodexQuota,
    renderOAuthCredential,
    resetCodexQuotaOverdraftDraft,
    saveCodexQuotaOverdraftFromAdvancedSettings,
    updateCodexQuotaOverdraft,
    setOAuthCredentialView,
    setupOAuthActions,
    showOAuthSession,
    submitAntigravityOAuthCallback,
    submitAnthropicCookieAuth,
    submitAnthropicOAuthCode,
    submitCodexPersonalAccessToken,
    submitCursorCredential,
    submitCodexOAuthCallback,
    submitXAIOAuthCallback,
    submitXAICredentialBatch,
    submitZAICodingPlanKey,
    pollZAIOAuthStatus
  };
}
