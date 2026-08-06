const CODEX_OAUTH_POLL_INTERVAL_MS = 1000;
const CODEX_OAUTH_MAX_POLLS = 300;
let activeCodexOAuthFlow = null;
let codexOAuthStopPromise = null;
let currentCodexCredentialJSON = '';
let currentCodexCredential = null;
let currentCodexCredentialInfo = null;
let currentCodexCredentialView = 'decoded';
const codexUsageStateByChannelID = new Map();

function formatCodexPlanBadgeText(planType, subscriptionActiveUntil) {
  const plan = String(planType || '').trim();
  if (!plan) return '';
  const date = String(subscriptionActiveUntil || '').trim().match(/^(\d{4}-\d{2}-\d{2})/);
  return date ? `${plan} · ${date[1]}` : plan;
}

function buildCodexCredentialView() {
  if (!currentCodexCredential) return null;
  if (currentCodexCredentialView !== 'decoded' || !currentCodexCredentialInfo) {
    return currentCodexCredential;
  }
  return { ...currentCodexCredential, id_token: currentCodexCredentialInfo };
}

function updateCodexCredentialViewControls() {
  document.querySelectorAll('[data-codex-credential-view]').forEach(button => {
    const active = button.dataset.codexCredentialView === currentCodexCredentialView;
    button.classList.toggle('active', active);
    button.setAttribute('aria-pressed', String(active));
  });
}

function renderCurrentCodexCredential() {
  const content = document.getElementById('codexCredentialContent');
  const displayedCredential = buildCodexCredentialView();
  currentCodexCredentialJSON = displayedCredential ? JSON.stringify(displayedCredential, null, 2) : '';
  updateCodexCredentialViewControls();
  if (!content) return;

  content.removeAttribute?.('data-highlighted');
  content.classList?.remove('hljs');
  content.textContent = currentCodexCredentialJSON;
  if (!currentCodexCredentialJSON || typeof window === 'undefined' || !window.hljs?.highlightElement) return;

  try {
    content.classList?.add('language-json');
    window.hljs.highlightElement(content);
  } catch (error) {
    console.warn('Failed to highlight Codex credential JSON', error);
  }
}

function renderCodexCredential(credential, credentialInfo = null, view = 'decoded') {
  currentCodexCredential = credential || null;
  currentCodexCredentialInfo = credentialInfo || null;
  currentCodexCredentialView = view === 'raw' ? 'raw' : 'decoded';
  renderCurrentCodexCredential();
}

function setCodexCredentialView(view) {
  currentCodexCredentialView = view === 'raw' ? 'raw' : 'decoded';
  renderCurrentCodexCredential();
}

async function copyCodexCredential(copier = window.copyToClipboard) {
  if (!currentCodexCredentialJSON) throw new Error('Codex credential is empty');
  if (typeof copier !== 'function') throw new Error('Clipboard is unavailable');
  await copier(currentCodexCredentialJSON);
}

function applyChannelAuthEditorMode(
  authType,
  credential = null,
  channel = null,
  credentialInfo = null,
  credentialView = 'decoded'
) {
  const codexOAuth = authType === 'codex_oauth';
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
  if (notice) notice.hidden = !codexOAuth;
  if (planBadge) {
    planBadge.textContent = planBadgeText;
    planBadge.hidden = !planBadgeText;
  }
  if (keyHeader) keyHeader.hidden = false;
  if (keyTable) keyTable.hidden = false;
  if (hiddenKey) {
    hiddenKey.required = !codexOAuth;
    if (codexOAuth) hiddenKey.value = '';
  }
  if (importButton) importButton.disabled = codexOAuth;
  if (batchDeleteButton && codexOAuth) batchDeleteButton.disabled = true;
  if (selectAll) selectAll.disabled = codexOAuth;
  if (credentialTab) credentialTab.hidden = !codexOAuth;
  renderCodexCredential(
    codexOAuth ? credential : null,
    codexOAuth ? credentialInfo : null,
    credentialView
  );

  document.querySelectorAll('input[name="keyStrategy"]').forEach(input => {
    input.disabled = codexOAuth;
  });
  document.querySelectorAll('#inlineKeyTableBody .inline-key-input').forEach(input => {
    input.readOnly = codexOAuth;
  });
  document.querySelectorAll('#inlineKeyTableBody .inline-key-note-input').forEach(input => {
    input.readOnly = codexOAuth;
  });
  document.querySelectorAll('#inlineKeyTableBody [data-action="delete"], #inlineKeyTableBody [data-action="toggle-disabled"]').forEach(button => {
    button.hidden = false;
    button.disabled = codexOAuth;
  });
  document.querySelectorAll('#inlineKeyTableBody .inline-key-row').forEach(row => {
    row.draggable = !codexOAuth;
  });
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
  const status = document.getElementById('codexOAuthDialogStatus');
  if (!status) return;
  status.textContent = message || '';
  status.hidden = !message;
  status.dataset.kind = kind;
}

function showCodexOAuthSession(session) {
  if (!session?.url || !session?.state) return false;
  const dialog = document.getElementById('codexOAuthDialog');
  const authorizationURL = document.getElementById('codexOAuthAuthorizationURL');
  const openLink = document.getElementById('codexOAuthOpenLink');
  const callbackURL = document.getElementById('codexOAuthCallbackURL');
  if (!dialog || !authorizationURL || !openLink || !callbackURL) return false;

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
  if (!authorizationURL) throw new Error('Codex OAuth authorization URL is empty');
  if (typeof copier !== 'function') throw new Error('Clipboard is unavailable');
  await copier(authorizationURL);
}

async function submitCodexOAuthCallback(callbackURL, fetcher = fetchDataWithAuth) {
  const normalizedURL = String(callbackURL || '').trim();
  if (!normalizedURL) throw new Error('Codex OAuth callback URL is required');
  return fetcher('/admin/codex/oauth/callback', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ callback_url: normalizedURL })
  });
}

async function cancelCodexOAuth(state, fetcher = fetchDataWithAuth) {
  const normalizedState = String(state || '').trim();
  if (!normalizedState) throw new Error('Codex OAuth state is required');
  return fetcher('/admin/codex/oauth/cancel', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ state: normalizedState })
  });
}

async function pollCodexOAuthStatus(state, options = {}) {
  const fetchStatus = options.fetchStatus || (url => fetchDataWithAuth(url));
  const delay = options.delay || codexOAuthDelay;
  const maxPolls = options.maxPolls || CODEX_OAUTH_MAX_POLLS;
  const interval = options.interval ?? CODEX_OAUTH_POLL_INTERVAL_MS;
  for (let attempt = 0; attempt < maxPolls; attempt++) {
    const status = await fetchStatus(`/admin/codex/oauth/status?state=${encodeURIComponent(state)}`);
    if (status?.status === 'complete') return status;
    if (status?.status === 'cancelled') throw new Error(window.t('channels.codex.oauthCancelled'));
    if (status?.status === 'error') throw new Error(status.error || window.t('channels.codex.oauthFailed'));
    await delay(interval);
  }
  throw new Error(window.t('channels.codex.oauthTimedOut'));
}

async function startCodexOAuth(button) {
  let resolveReady;
  let rejectReady;
  const ready = new Promise((resolve, reject) => {
    resolveReady = resolve;
    rejectReady = reject;
  });
  ready.catch(() => {});
  const flow = { state: '', button, cancelling: false, ready, readySettled: false };
  activeCodexOAuthFlow = flow;
  try {
    if (button) button.disabled = true;
    setCodexAuthStatus(window.t('channels.codex.oauthStarting'));
    const session = await fetchDataWithAuth('/admin/codex/oauth/start', { method: 'POST' });
    if (!session?.url || !session?.state) throw new Error(window.t('channels.codex.oauthFailed'));
    flow.state = session.state;
    flow.readySettled = true;
    resolveReady(session.state);
    if (flow.cancelling) return null;
    if (!showCodexOAuthSession(session)) throw new Error(window.t('channels.codex.oauthFailed'));
    setCodexAuthStatus(window.t('channels.codex.oauthWaiting'));
    setCodexOAuthDialogStatus(window.t('channels.codex.oauthWaiting'));
    const result = await pollCodexOAuthStatus(session.state);
    if (flow.cancelling || activeCodexOAuthFlow !== flow) return null;
    const dialog = document.getElementById('codexOAuthDialog');
    if (dialog?.open) dialog.close();
    setCodexAuthStatus(window.t('channels.codex.oauthComplete'), 'success');
    if (window.showSuccess) window.showSuccess(window.t('channels.codex.oauthComplete'));
    await reloadChannelsList();
    return result;
  } catch (error) {
    if (!flow.readySettled) {
      flow.readySettled = true;
      rejectReady(error);
    }
    if (flow.cancelling) return null;
    const message = error?.message || window.t('channels.codex.oauthFailed');
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
      setCodexOAuthDialogStatus(window.t('channels.codex.oauthCancelling'));
      if (!flow.state && flow.ready) {
        try {
          await flow.ready;
        } catch {
          if (closeDialog) {
            const dialog = document.getElementById('codexOAuthDialog');
            if (dialog?.open) dialog.close();
          }
          return;
        }
      }
      try {
        await cancelCodexOAuth(flow.state);
      } catch (error) {
        flow.cancelling = false;
        throw error;
      }
      if (activeCodexOAuthFlow === flow) activeCodexOAuthFlow = null;
      if (flow.button) flow.button.disabled = false;
    }
    if (closeDialog) {
      const dialog = document.getElementById('codexOAuthDialog');
      if (dialog?.open) dialog.close();
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

async function closeCodexOAuthDialog() {
  try {
    await stopActiveCodexOAuth({ closeDialog: true });
  } catch (error) {
    setCodexOAuthDialogStatus(error?.message || window.t('channels.codex.oauthCancelFailed'), 'error');
  }
}

async function restartCodexOAuth(button) {
  try {
    if (button) button.disabled = true;
    await stopActiveCodexOAuth({ closeDialog: false });
    setCodexOAuthDialogStatus(window.t('channels.codex.oauthRestarting'));
    const loginButton = document.getElementById('codexOAuthBtn');
    const completion = startCodexOAuth(loginButton);
    const newFlow = activeCodexOAuthFlow;
    if (newFlow?.ready) await newFlow.ready;
    void completion;
  } catch (error) {
    setCodexOAuthDialogStatus(error?.message || window.t('channels.codex.oauthCancelFailed'), 'error');
  } finally {
    if (button) button.disabled = false;
  }
}

async function importCodexCredentials(files, button, fetcher = fetchDataWithAuth) {
  const selectedFiles = Array.from(files || []).filter(Boolean);
  if (selectedFiles.length === 0) return null;
  const formData = new FormData();
  selectedFiles.forEach(file => formData.append('files', file));
  try {
    if (button) button.disabled = true;
    setCodexAuthStatus(window.t('channels.codex.importing', { count: selectedFiles.length }));
    const result = await fetcher('/admin/codex/credentials/import', {
      method: 'POST',
      body: formData
    });
    const created = Number(result?.created) || 0;
    const skipped = Number(result?.skipped) || 0;
    const failed = Number(result?.failed) || 0;
    const message = window.t('channels.codex.importSummary', { created, skipped, failed });
    const kind = failed > 0 ? 'error' : 'success';
    setCodexAuthStatus(message, kind);
    if (failed > 0) {
      if (window.showError) window.showError(message);
    } else if (window.showSuccess) {
      window.showSuccess(message);
    }
    if (created > 0) await reloadChannelsList();
    return result;
  } catch (error) {
    const message = error?.message || window.t('channels.codex.importFailed');
    setCodexAuthStatus(message, 'error');
    if (window.showError) window.showError(message);
    return null;
  } finally {
    if (button) button.disabled = false;
  }
}

async function refreshCodexCredential(channelID, fetcher = fetchDataWithAuth) {
  const numericID = Number(channelID);
  if (!Number.isInteger(numericID) || numericID <= 0) {
    throw new Error('A saved Codex channel is required');
  }
  return fetcher(`/admin/channels/${numericID}/codex-credential/refresh`, { method: 'POST' });
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

function setupCodexAuthActions() {
  const oauthButton = document.getElementById('codexOAuthBtn');
  const importButton = document.getElementById('importCodexCredentialBtn');
  const importInput = document.getElementById('importCodexCredentialInput');
  const copyButton = document.getElementById('codexOAuthCopyLink');
  const restartButton = document.getElementById('codexOAuthRestart');
  const dialog = document.getElementById('codexOAuthDialog');
  const authorizationURL = document.getElementById('codexOAuthAuthorizationURL');
  const callbackForm = document.getElementById('codexOAuthCallbackForm');
  const callbackURL = document.getElementById('codexOAuthCallbackURL');
  const callbackButton = document.getElementById('codexOAuthSubmitCallback');
  const credentialCopyButton = document.getElementById('codexCredentialCopyButton');
  const credentialRefreshButton = document.getElementById('codexCredentialRefreshButton');
  if (oauthButton && !oauthButton.dataset.bound) {
    oauthButton.addEventListener('click', () => startCodexOAuth(oauthButton));
    oauthButton.dataset.bound = '1';
  }
  if (copyButton && authorizationURL && !copyButton.dataset.bound) {
    copyButton.addEventListener('click', async () => {
      try {
        await copyCodexOAuthLink(authorizationURL.value);
        setCodexOAuthDialogStatus(window.t('channels.codex.oauthLinkCopied'), 'success');
      } catch (error) {
        setCodexOAuthDialogStatus(error?.message || window.t('channels.codex.oauthCopyFailed'), 'error');
      }
    });
    copyButton.dataset.bound = '1';
  }
  if (restartButton && !restartButton.dataset.bound) {
    restartButton.addEventListener('click', () => restartCodexOAuth(restartButton));
    restartButton.dataset.bound = '1';
  }
  if (callbackForm && callbackURL && !callbackForm.dataset.bound) {
    callbackForm.addEventListener('submit', async event => {
      event.preventDefault();
      const value = callbackURL.value.trim();
      if (!value) {
        callbackURL.setAttribute('aria-invalid', 'true');
        callbackURL.focus();
        setCodexOAuthDialogStatus(window.t('channels.codex.oauthCallbackRequired'), 'error');
        return;
      }
      callbackURL.removeAttribute('aria-invalid');
      try {
        if (callbackButton) callbackButton.disabled = true;
        setCodexOAuthDialogStatus(window.t('channels.codex.oauthCallbackSubmitting'));
        await submitCodexOAuthCallback(value);
        setCodexOAuthDialogStatus(window.t('channels.codex.oauthCallbackAccepted'), 'success');
      } catch (error) {
        callbackURL.setAttribute('aria-invalid', 'true');
        callbackURL.focus();
        setCodexOAuthDialogStatus(error?.message || window.t('channels.codex.oauthFailed'), 'error');
      } finally {
        if (callbackButton) callbackButton.disabled = false;
      }
    });
    callbackForm.dataset.bound = '1';
  }
  document.querySelectorAll('[data-action="close-codex-oauth"]').forEach(closeButton => {
    if (closeButton.dataset.bound) return;
    closeButton.addEventListener('click', () => closeCodexOAuthDialog());
    closeButton.dataset.bound = '1';
  });
  if (dialog && !dialog.dataset.cancelBound) {
    dialog.addEventListener('cancel', event => {
      event.preventDefault();
      void closeCodexOAuthDialog();
    });
    dialog.dataset.cancelBound = '1';
  }
  if (importButton && importInput && !importButton.dataset.bound) {
    importButton.addEventListener('click', () => importInput.click());
    importInput.addEventListener('change', async () => {
      await importCodexCredentials(importInput.files, importButton);
      importInput.value = '';
    });
    importButton.dataset.bound = '1';
  }
  if (credentialCopyButton && !credentialCopyButton.dataset.bound) {
    credentialCopyButton.addEventListener('click', async () => {
      try {
        await copyCodexCredential();
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
    viewButton.addEventListener('click', () => setCodexCredentialView(viewButton.dataset.codexCredentialView));
    viewButton.dataset.bound = '1';
  });
  if (credentialRefreshButton && !credentialRefreshButton.dataset.bound) {
    credentialRefreshButton.addEventListener('click', async () => {
      const previousView = currentCodexCredentialView;
      try {
        credentialRefreshButton.disabled = true;
        const result = await refreshCodexCredential(editingChannelId);
        const credential = result?.codex_credential;
        if (!credential?.access_token) throw new Error(window.t('channels.codex.credentialRefreshInvalid'));

        if (typeof setInlineKeyTableDataFromAPI === 'function' && typeof renderInlineKeyTable === 'function') {
          setInlineKeyTableDataFromAPI([{
            channel_id: editingChannelId,
            key_index: 0,
            api_key: credential.access_token,
            note: 'Codex OAuth AT',
            key_strategy: 'sequential'
          }]);
          inlineKeyVisible = true;
          renderInlineKeyTable();
        }
        applyChannelAuthEditorMode('codex_oauth', credential, result, result.codex_credential_info, previousView);
        await reloadChannelsList();
        if (window.showSuccess) window.showSuccess(window.t('channels.codex.credentialRefreshed'));
      } catch (error) {
        const message = error?.message || window.t('channels.codex.credentialRefreshFailed');
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
    cancelCodexOAuth,
    copyCodexCredential,
    copyCodexOAuthLink,
    formatCodexPlanBadgeText,
    getCodexUsageState,
    importCodexCredentials,
    pollCodexOAuthStatus,
    refreshCodexCredential,
    refreshCodexUsage,
    renderCodexCredential,
    setCodexCredentialView,
    showCodexOAuthSession,
    submitCodexOAuthCallback
  };
}
