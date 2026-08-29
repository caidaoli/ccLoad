/**
 * 渠道管理账户（New API / 标准 Sub2API / Sub2API Pro）前端交互。
 *
 * 与 OAuth 凭据体系完全隔离：编辑器只提交 `management_account`，绝不触碰
 * `oauth_credential`；管理账户凭据只在已鉴权的编辑器接口中返回并回填。
 * 额度刷新与签到各自持有独立的 operation sequence、loading 与 error 状态，
 * 互不禁用，陈旧响应也不会覆盖新响应。
 */

// ============================================================
// Profile 字段矩阵
// ============================================================

const MANAGEMENT_PROFILE_FIELDS = Object.freeze({
  '': Object.freeze({
    baseURL: false, token: false, login: false, userID: false, checkin: false,
    notice: '', tokenHelp: ''
  }),
  new_api: Object.freeze({
    baseURL: true, token: true, login: false, userID: true, checkin: true,
    notice: '', tokenHelp: 'channels.management.tokenHelpNewAPI'
  }),
  sub2api: Object.freeze({
    baseURL: true, token: false, login: true, userID: false, checkin: false,
    notice: 'channels.management.noticeSub2API', tokenHelp: ''
  }),
  sub2api_pro: Object.freeze({
    baseURL: true, token: false, login: true, userID: false, checkin: true,
    notice: 'channels.management.noticeSub2APIPro', tokenHelp: ''
  })
});

const MANAGEMENT_CHECKIN_PROFILES = Object.freeze(['new_api', 'sub2api_pro']);
const MANAGEMENT_RATE_PROFILES = Object.freeze(['new_api', 'sub2api', 'sub2api_pro']);
const MANAGEMENT_CHECKIN_TIME_PATTERN = /^([01]\d|2[0-3]):[0-5]\d$/;

// ============================================================
// 列表动作状态（余额与签到各自独立）
// ============================================================

const managementBalanceStateByChannelID = new Map();
const managementCheckinStateByChannelID = new Map();
const managementBalanceOperationByChannelID = new Map();
const managementBalanceLastOperationByChannelID = new Map();
const managementCheckinOperationByChannelID = new Map();

let managementBalanceOperationSequence = 0;
let managementCheckinOperationSequence = 0;

// ============================================================
// 编辑器草稿状态
// ============================================================

let managementAccountAuthType = 'api_key';
let managementAccountSavedProfile = '';
let managementAccountSavedBaseURL = '';
let managementAccountUserIDConfigured = false;
let managementAccountCredentialConfigured = false;
let managementAccountState = null;
let managementAccountPendingSession = null;

function emptyManagementAccountDraft() {
  return {
    profile: '',
    base_url: '',
    access_token: '',
    refresh_token: '',
    email: '',
    password: '',
    totp_code: '',
    user_id: '',
    daily_checkin_enabled: false,
    daily_checkin_time: '',
    session: null
  };
}

function managementBaseURLKey(value) {
  return String(value || '').trim().replace(/\/+$/, '');
}

function clearManagementPendingSession() {
  managementAccountPendingSession = null;
}

function managementElement(id) {
  return typeof document !== 'undefined' ? document.getElementById(id) : null;
}

function managementText(key, values) {
  if (typeof window !== 'undefined' && typeof window.t === 'function') return window.t(key, values);
  return key;
}

function normalizeManagementProfile(value) {
  const profile = String(value === null || value === undefined ? '' : value).trim();
  return Object.prototype.hasOwnProperty.call(MANAGEMENT_PROFILE_FIELDS, profile) ? profile : '';
}

function hasPendingManagementSession() {
  const session = managementAccountPendingSession || (managementAccountState && managementAccountState.session);
  return Boolean(session && session.refresh_token && session.access_token && session.expires_at && session.account_id);
}

function syncManagementSaveAction() {
  if (typeof window !== 'undefined' && typeof window.syncChannelSaveButtonLabel === 'function') {
    window.syncChannelSaveButtonLabel();
  }
}

function managementProfileFields(profile) {
  return MANAGEMENT_PROFILE_FIELDS[normalizeManagementProfile(profile)];
}

/** 面板根地址：与后端 Validate 同规则，拒绝路径、查询、片段和 userinfo。 */
function isManagementBaseURLValid(raw) {
  const value = String(raw || '').trim();
  if (!value || value.includes('#')) return false;
  let parsed;
  try {
    parsed = new URL(value);
  } catch (_) {
    return false;
  }
  if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') return false;
  if (!parsed.hostname || parsed.username || parsed.password) return false;
  if (parsed.search || parsed.hash) return false;
  return parsed.pathname === '' || parsed.pathname === '/';
}

/** 把上游 URL 收敿到根地址，仅用于新渠道的初始默认值。 */
function managementBaseURLRoot(raw) {
  const value = String(raw || '').trim();
  if (!value) return '';
  try {
    const parsed = new URL(value);
    if (parsed.protocol !== 'http:' && parsed.protocol !== 'https:') return '';
    if (!parsed.hostname) return '';
    return `${parsed.protocol}//${parsed.host}`;
  } catch (_) {
    return '';
  }
}

function firstManagementBaseURL(channelURLs) {
  const entries = Array.isArray(channelURLs) ? channelURLs : [];
  for (const entry of entries) {
    const root = managementBaseURLRoot(typeof entry === 'string' ? entry : entry && entry.url);
    if (root) return root;
  }
  return '';
}

// ============================================================
// 表单渲染与校验
// ============================================================

function setManagementFieldVisibility(id, visible) {
  const field = managementElement(id);
  if (field) field.hidden = !visible;
}

function setManagementFieldValue(id, value) {
  const input = managementElement(id);
  if (input) input.value = value === null || value === undefined ? '' : String(value);
}

function setManagementFieldError(input, errorNode, messageKey) {
  if (input) {
    if (messageKey) input.setAttribute('aria-invalid', 'true');
    else input.removeAttribute('aria-invalid');
  }
  if (errorNode) {
    errorNode.textContent = messageKey ? managementText(messageKey) : '';
    errorNode.hidden = !messageKey;
  }
}

function clearManagementAccountErrors() {
  setManagementFieldError(
    managementElement('channelManagementBaseURL'),
    managementElement('channelManagementBaseURLError'),
    null
  );
  setManagementFieldError(
    managementElement('channelManagementToken'),
    managementElement('channelManagementTokenError'),
    null
  );
  setManagementFieldError(
    managementElement('channelManagementEmail'),
    managementElement('channelManagementEmailError'),
    null
  );
  setManagementFieldError(
    managementElement('channelManagementPassword'),
    managementElement('channelManagementPasswordError'),
    null
  );
  setManagementFieldError(
    managementElement('channelManagementTOTP'),
    managementElement('channelManagementTOTPError'),
    null
  );
  setManagementFieldError(
    managementElement('channelManagementRefreshToken'),
    managementElement('channelManagementRefreshTokenError'),
    null
  );
  setManagementFieldError(
    managementElement('channelManagementUserID'),
    managementElement('channelManagementUserIDError'),
    null
  );
  setManagementFieldError(
    managementElement('channelManagementDailyCheckinTime'),
    managementElement('channelManagementDailyCheckinTimeError'),
    null
  );
}

function bindManagementProfileChange(select) {
  if (!select || select.dataset.managementProfileBound === '1') return;
  select.dataset.managementProfileBound = '1';
  select.addEventListener('change', () => {
    const draft = readManagementAccountForm();
    if (draft.profile !== managementAccountSavedProfile) {
      // Credentials belong to the saved profile and must not carry across profiles.
      draft.access_token = '';
      draft.refresh_token = '';
      draft.email = '';
      draft.password = '';
      draft.totp_code = '';
      draft.session = null;
      clearManagementPendingSession();
    }
    renderManagementAccountFields(draft);
    if (typeof window !== 'undefined' && typeof window.markChannelFormDirty === 'function') {
      window.markChannelFormDirty();
    }
  });
}

/** 读取表单原始值；profile 无关的字段一律保留，提交时再按矩阵裁剪。 */
function readManagementAccountForm() {
  const profileSelect = managementElement('channelManagementProfile');
  const checkbox = managementElement('channelManagementDailyCheckinEnabled');
  return {
    profile: normalizeManagementProfile(profileSelect ? profileSelect.value : ''),
    base_url: String((managementElement('channelManagementBaseURL') || {}).value || '').trim(),
    access_token: String((managementElement('channelManagementToken') || {}).value || '').trim(),
    refresh_token: String((managementElement('channelManagementRefreshToken') || {}).value || '').trim(),
    email: String((managementElement('channelManagementEmail') || {}).value || '').trim(),
    password: String((managementElement('channelManagementPassword') || {}).value || ''),
    totp_code: String((managementElement('channelManagementTOTP') || {}).value || '').trim(),
    user_id: String((managementElement('channelManagementUserID') || {}).value || '').trim(),
    daily_checkin_enabled: checkbox ? checkbox.checked === true : false,
    daily_checkin_time: String((managementElement('channelManagementDailyCheckinTime') || {}).value || '').trim()
  };
}

function renderManagementAccountFields(draft) {
  const group = managementElement('channelManagementGroup');
  const isAPIKey = managementAccountAuthType === 'api_key';
  if (group) group.hidden = !isAPIKey;
  if (!isAPIKey) {
    clearManagementAccountErrors();
    return;
  }

  const state = draft || managementAccountState || emptyManagementAccountDraft();
  const profile = normalizeManagementProfile(state.profile);
  const fields = MANAGEMENT_PROFILE_FIELDS[profile];

  const profileSelect = managementElement('channelManagementProfile');
  if (profileSelect) {
    if (profileSelect.value !== profile) profileSelect.value = profile;
    bindManagementProfileChange(profileSelect);
  }

  setManagementFieldVisibility('channelManagementBaseURLField', fields.baseURL);
  setManagementFieldVisibility('channelManagementTokenField', fields.token);
  setManagementFieldVisibility('channelManagementLoginField', fields.login);
  setManagementFieldVisibility('channelManagementRefreshTokenField', fields.login);
  setManagementFieldVisibility('channelManagementUserIDField', fields.userID);
  setManagementFieldVisibility('channelManagementCheckinField', fields.checkin);

  setManagementFieldValue('channelManagementBaseURL', state.base_url);
  setManagementFieldValue('channelManagementToken', state.access_token);
  setManagementFieldValue('channelManagementRefreshToken', state.refresh_token);
  setManagementFieldValue('channelManagementEmail', state.email);
  setManagementFieldValue('channelManagementPassword', state.password);
  setManagementFieldValue('channelManagementTOTP', state.totp_code);
  setManagementFieldValue('channelManagementUserID', state.user_id);
  setManagementFieldValue('channelManagementDailyCheckinTime', state.daily_checkin_time);
  const checkbox = managementElement('channelManagementDailyCheckinEnabled');
  if (checkbox) checkbox.checked = state.daily_checkin_enabled === true;

  const tokenHelp = managementElement('channelManagementTokenHelp');
  if (tokenHelp && fields.tokenHelp) tokenHelp.textContent = managementText(fields.tokenHelp);

  // 已保存的凭据留空即保留；换 profile 后后端必须收到新凭据，提示随之消失。
  const hint = managementElement('channelManagementTokenHint');
  if (hint) {
    hint.hidden = !(fields.token && managementAccountCredentialConfigured && profile === managementAccountSavedProfile);
  }

  // 编辑器接口会返回已保存的 user_id；保留配置标记用于兼容旧响应。
  const userIDHint = managementElement('channelManagementUserIDHint');
  if (userIDHint) {
    userIDHint.hidden = !(
      fields.userID &&
      managementAccountUserIDConfigured &&
      profile === managementAccountSavedProfile
    );
  }

  const notice = managementElement('channelManagementNotice');
  if (notice) {
    notice.textContent = fields.notice ? managementText(fields.notice) : '';
    notice.hidden = !fields.notice;
  }

  clearManagementAccountErrors();
}

function resetManagementAccountDraft(view, channelURLs, authType) {
  managementAccountAuthType = String(authType || 'api_key').trim().toLowerCase() || 'api_key';
  const account = managementAccountAuthType === 'api_key' && view ? view : null;
  managementAccountSavedProfile = normalizeManagementProfile(account && account.profile);
  managementAccountSavedBaseURL = String((account && account.base_url) || '').trim();
  managementAccountUserIDConfigured = Boolean(account && account.user_id_configured === true);
  managementAccountCredentialConfigured = Boolean(account && account.credential_configured === true);
  clearManagementPendingSession();

  // 首个渠道 URL 只作为初始默认；已保存的显式面板地址永远优先。
  const savedBaseURL = String((account && account.base_url) || '').trim();
  managementAccountState = {
    profile: managementAccountSavedProfile,
    base_url: savedBaseURL || firstManagementBaseURL(channelURLs),
    access_token: String((account && account.access_token) || '').trim(),
    refresh_token: String((account && account.refresh_token) || '').trim(),
    email: String((account && account.email) || '').trim(),
    password: String((account && account.password) || ''),
    totp_code: '',
    user_id: account && account.user_id !== null && account.user_id !== undefined
      ? String(account.user_id)
      : '',
    daily_checkin_enabled: Boolean(account && account.daily_checkin_enabled === true),
    daily_checkin_time: String((account && account.daily_checkin_time) || '').trim(),
    session: null
  };
  renderManagementAccountFields(managementAccountState);
  syncManagementSaveAction();
  return managementAccountState;
}

/** 重开高级设置时丢弃未确认的编辑，回到最近一次 commit 的草稿。 */
function beginManagementAccountDraft() {
  clearManagementPendingSession();
  renderManagementAccountFields(managementAccountState);
}

function validateManagementAccountDraft() {
  if (managementAccountAuthType !== 'api_key') {
    clearManagementAccountErrors();
    return true;
  }
  const draft = readManagementAccountForm();
  const fields = managementProfileFields(draft.profile);
  const invalidControls = [];

  let baseURLError = null;
  if (fields.baseURL) {
    if (!draft.base_url) baseURLError = 'channels.management.errBaseURLRequired';
    else if (!isManagementBaseURLValid(draft.base_url)) baseURLError = 'channels.management.errBaseURLInvalid';
  }
  const baseURLInput = managementElement('channelManagementBaseURL');
  setManagementFieldError(baseURLInput, managementElement('channelManagementBaseURLError'), baseURLError);
  if (baseURLError && baseURLInput) invalidControls.push(baseURLInput);

  const tokenRequired = fields.token && !(
    managementAccountCredentialConfigured &&
    normalizeManagementProfile(draft.profile) === managementAccountSavedProfile
  );
  const tokenError = tokenRequired && !draft.access_token ? 'channels.management.errTokenRequired' : null;
  const tokenInput = managementElement('channelManagementToken');
  setManagementFieldError(tokenInput, managementElement('channelManagementTokenError'), tokenError);
  if (tokenError && tokenInput) invalidControls.push(tokenInput);

  const sessionError = managementSessionError(draft, fields);
  const refreshTokenInput = managementElement('channelManagementRefreshToken');
  setManagementFieldError(
    refreshTokenInput,
    managementElement('channelManagementRefreshTokenError'),
    sessionError
  );
  if (sessionError && refreshTokenInput) invalidControls.push(refreshTokenInput);

  setManagementFieldError(
    managementElement('channelManagementEmail'),
    managementElement('channelManagementEmailError'),
    null
  );
  setManagementFieldError(
    managementElement('channelManagementPassword'),
    managementElement('channelManagementPasswordError'),
    null
  );

  const totpError = fields.login && draft.totp_code && !/^\d{6}$/.test(draft.totp_code)
    ? 'channels.management.errTOTP'
    : null;
  const totpInput = managementElement('channelManagementTOTP');
  setManagementFieldError(totpInput, managementElement('channelManagementTOTPError'), totpError);
  if (totpError && totpInput) invalidControls.push(totpInput);

  let userIDError = null;
  if (fields.userID && draft.user_id !== '') {
    const userID = Number(draft.user_id);
    if (!Number.isInteger(userID) || userID <= 0) userIDError = 'channels.management.errUserID';
  }
  const userIDInput = managementElement('channelManagementUserID');
  setManagementFieldError(userIDInput, managementElement('channelManagementUserIDError'), userIDError);
  if (userIDError && userIDInput) invalidControls.push(userIDInput);

  let checkinTimeError = null;
  if (fields.checkin) {
    const hasTime = draft.daily_checkin_time !== '';
    if (draft.daily_checkin_enabled && !hasTime) checkinTimeError = 'channels.management.errCheckinTime';
    else if (hasTime && !MANAGEMENT_CHECKIN_TIME_PATTERN.test(draft.daily_checkin_time)) {
      checkinTimeError = 'channels.management.errCheckinTime';
    }
  }
  const checkinTimeInput = managementElement('channelManagementDailyCheckinTime');
  setManagementFieldError(
    checkinTimeInput,
    managementElement('channelManagementDailyCheckinTimeError'),
    checkinTimeError
  );
  if (checkinTimeError && checkinTimeInput) invalidControls.push(checkinTimeInput);

  if (invalidControls.length === 0) return true;
  if (typeof invalidControls[0].focus === 'function') invalidControls[0].focus();
  return false;
}

function matchingManagementPendingSession(draft) {
  const session = managementAccountPendingSession || (draft && draft.session) ||
    (managementAccountState && managementAccountState.session) || null;
  if (!session || !draft) return null;
  if (session.refresh_token !== draft.refresh_token) return null;
  if (managementBaseURLKey(session.base_url) !== managementBaseURLKey(draft.base_url)) return null;
  if (!session.access_token || !session.expires_at || !session.account_id) return null;
  return session;
}

function managementSessionError(draft, fields) {
  if (!fields.login) return null;
  if (matchingManagementPendingSession(draft)) return null;
  const savedSession = managementAccountCredentialConfigured &&
    normalizeManagementProfile(draft.profile) === managementAccountSavedProfile &&
    managementBaseURLKey(draft.base_url) === managementBaseURLKey(managementAccountSavedBaseURL) &&
    Boolean(draft.refresh_token);
  return savedSession ? null : 'channels.management.errSessionRequired';
}

function validateManagementLoginDraft() {
  if (managementAccountAuthType !== 'api_key') return false;
  const draft = readManagementAccountForm();
  const fields = managementProfileFields(draft.profile);
  const invalidControls = [];

  let baseURLError = null;
  if (!draft.base_url) baseURLError = 'channels.management.errBaseURLRequired';
  else if (!isManagementBaseURLValid(draft.base_url)) baseURLError = 'channels.management.errBaseURLInvalid';
  const baseURLInput = managementElement('channelManagementBaseURL');
  setManagementFieldError(baseURLInput, managementElement('channelManagementBaseURLError'), baseURLError);
  if (baseURLError && baseURLInput) invalidControls.push(baseURLInput);

  setManagementFieldError(
    managementElement('channelManagementRefreshToken'),
    managementElement('channelManagementRefreshTokenError'),
    null
  );

  const emailError = !draft.email ? 'channels.management.errEmailRequired' : null;
  const emailInput = managementElement('channelManagementEmail');
  setManagementFieldError(emailInput, managementElement('channelManagementEmailError'), emailError);
  if (emailError && emailInput) invalidControls.push(emailInput);

  const passwordError = !draft.password ? 'channels.management.errPasswordRequired' : null;
  const passwordInput = managementElement('channelManagementPassword');
  setManagementFieldError(passwordInput, managementElement('channelManagementPasswordError'), passwordError);
  if (passwordError && passwordInput) invalidControls.push(passwordInput);

  const totpError = draft.totp_code && !/^\d{6}$/.test(draft.totp_code)
    ? 'channels.management.errTOTP'
    : null;
  const totpInput = managementElement('channelManagementTOTP');
  setManagementFieldError(totpInput, managementElement('channelManagementTOTPError'), totpError);
  if (totpError && totpInput) invalidControls.push(totpInput);

  if (!fields.login) return false;
  if (invalidControls.length === 0) return true;
  if (typeof invalidControls[0].focus === 'function') invalidControls[0].focus();
  return false;
}

function commitManagementAccountDraft() {
  if (managementAccountAuthType !== 'api_key') {
    managementAccountState = null;
    syncManagementSaveAction();
    return true;
  }
  if (!validateManagementAccountDraft()) return false;
  const draft = readManagementAccountForm();
  const session = matchingManagementPendingSession(draft);
  managementAccountState = session ? { ...draft, session } : { ...draft, session: null };
  syncManagementSaveAction();
  if (typeof renderInlineKeyTable === 'function') renderInlineKeyTable();
  if (typeof window !== 'undefined' && typeof window.markChannelFormDirty === 'function') {
    window.markChannelFormDirty();
  }
  return true;
}

/** 渠道保存成功后，一次性 TOTP 离开草稿；邮箱、密码和 refresh token 留作下次登录。 */
function completeManagementAccountSave() {
  if (!managementAccountState) return;
  const fields = managementProfileFields(managementAccountState.profile);
  if (!fields.login && !hasPendingManagementSession()) return;
  managementAccountSavedProfile = normalizeManagementProfile(managementAccountState.profile);
  managementAccountSavedBaseURL = String(managementAccountState.base_url || '').trim();
  managementAccountCredentialConfigured = Boolean(managementAccountState.refresh_token) ||
    managementAccountCredentialConfigured;
  managementAccountState.totp_code = '';
  managementAccountState.session = null;
  clearManagementPendingSession();
  renderManagementAccountFields(managementAccountState);
  syncManagementSaveAction();
}

/**
 * 倍率查询只认已提交到渠道编辑器草稿的管理账户配置。
 * New API 需要管理 PAT 查询 Key 分组；Sub2API 只需要面板地址和渠道 Key。
 */
function getManagementAccountRateConfig() {
  if (managementAccountAuthType !== 'api_key') return null;
  const state = managementAccountState || emptyManagementAccountDraft();
  const profile = normalizeManagementProfile(state.profile);
  if (!MANAGEMENT_RATE_PROFILES.includes(profile)) return null;

  const payload = {
    profile,
    base_url: String(state.base_url || '').trim()
  };
  if (profile === 'new_api') {
    payload.access_token = String(state.access_token || '').trim();
    if (state.user_id !== '') {
      const userID = Number(state.user_id);
      if (Number.isInteger(userID) && userID > 0) payload.user_id = userID;
    }
  }
  return payload;
}

function collectManagementAccountForSubmit() {
  if (managementAccountAuthType !== 'api_key') return null;
  const state = managementAccountState || emptyManagementAccountDraft();
  const profile = normalizeManagementProfile(state.profile);
  if (!profile) return { profile: '' };

  const fields = MANAGEMENT_PROFILE_FIELDS[profile];
  const payload = { profile, base_url: state.base_url };
  // 空凭据表示保留服务端已保存的凭据。
  if (fields.token && state.access_token) payload.access_token = state.access_token;
  if (fields.login) {
    if (state.email) payload.email = state.email;
    if (state.password) payload.password = state.password;
  }
  const session = matchingManagementPendingSession(state);
  if (fields.login && session) {
    payload.access_token = session.access_token;
    payload.refresh_token = session.refresh_token;
    payload.expires_at = session.expires_at;
    payload.account_id = Number(session.account_id);
  }
  if (fields.userID && state.user_id !== '') {
    const userID = Number(state.user_id);
    if (Number.isInteger(userID) && userID > 0) payload.user_id = userID;
  }
  if (fields.checkin) {
    payload.daily_checkin_enabled = state.daily_checkin_enabled === true;
    if (state.daily_checkin_time) payload.daily_checkin_time = state.daily_checkin_time;
  }
  return payload;
}

function applyManagementLoginSession(draft, session) {
  const refreshToken = String(session.refresh_token || '').trim();
  managementAccountPendingSession = {
    base_url: String(draft.base_url || '').trim(),
    access_token: String(session.access_token || '').trim(),
    refresh_token: refreshToken,
    expires_at: session.expires_at,
    account_id: session.account_id
  };
  setManagementFieldValue('channelManagementRefreshToken', refreshToken);
  setManagementFieldValue('channelManagementTOTP', '');
  setManagementFieldError(
    managementElement('channelManagementRefreshToken'),
    managementElement('channelManagementRefreshTokenError'),
    null
  );
}

function isManagementTOTPRequired(error) {
  const message = String((error && error.message) || error || '');
  return message === 'totp_required' || message.includes('totp_required');
}

async function loginManagementAccount(fetcher) {
  const request = fetcher || (typeof fetchDataWithAuth === 'function' ? fetchDataWithAuth : null);
  if (!request) return false;
  if (!validateManagementLoginDraft()) return false;
  const draft = readManagementAccountForm();
  const button = managementElement('channelManagementLoginBtn');
  if (button) {
    button.disabled = true;
    button.setAttribute('aria-busy', 'true');
  }
  try {
    const session = await request('/admin/channel-management/sub2api-login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        profile: draft.profile,
        base_url: draft.base_url,
        email: draft.email,
        password: draft.password,
        totp_code: draft.totp_code || undefined,
        proxy_url: String((managementElement('channelProxyURL') || {}).value || '').trim()
      })
    });
    if (!session || !String(session.refresh_token || '').trim() || !String(session.access_token || '').trim() ||
        !session.expires_at || !session.account_id) {
      throw new Error(managementText('channels.management.loginInvalid'));
    }
    applyManagementLoginSession(draft, session);
    return true;
  } catch (error) {
    if (isManagementTOTPRequired(error)) {
      const totpInput = managementElement('channelManagementTOTP');
      setManagementFieldError(totpInput, managementElement('channelManagementTOTPError'), 'channels.management.errTOTPRequired');
      if (totpInput && typeof totpInput.focus === 'function') totpInput.focus();
      return false;
    }
    if (typeof window !== 'undefined' && typeof window.showError === 'function') {
      window.showError((error && error.message) || managementText('channels.management.loginFailed'));
    }
    return false;
  } finally {
    if (button) {
      button.disabled = false;
      button.removeAttribute('aria-busy');
    }
  }
}

// ============================================================
// 列表动作
// ============================================================

function managementChannelID(channelID) {
  const numericID = Number(channelID);
  if (!Number.isInteger(numericID) || numericID <= 0) return 0;
  return numericID;
}

function getManagementBalanceState(channelID) {
  const numericID = managementChannelID(channelID);
  if (!numericID) return null;
  return managementBalanceStateByChannelID.get(numericID) || null;
}

function getManagementBalanceOperationSequence() {
  return managementBalanceOperationSequence;
}

function applyManagementBalanceBatchResult(channelID, result, operationFloor = managementBalanceOperationSequence) {
  const numericID = managementChannelID(channelID);
  if (!numericID) return false;
  if (managementBalanceOperationByChannelID.has(numericID) ||
      (managementBalanceLastOperationByChannelID.get(numericID) || 0) > operationFloor) return false;
  if (result?.status === 'succeeded' && isManagementBalancePayload(result.management)) {
    managementBalanceStateByChannelID.set(numericID, { status: 'ready', data: result.management });
  } else {
    managementBalanceStateByChannelID.set(numericID, {
      status: 'error',
      error: result?.error || managementText('channels.management.balanceFailed')
    });
  }
  rerenderManagementAccounts();
  return true;
}

function getManagementCheckinState(channelID) {
  const numericID = managementChannelID(channelID);
  if (!numericID) return null;
  return managementCheckinStateByChannelID.get(numericID) || null;
}

function managementSupportsCheckin(profile) {
  return MANAGEMENT_CHECKIN_PROFILES.includes(normalizeManagementProfile(profile));
}

function isManagementBalancePayload(result) {
  return Boolean(result) && Boolean(result.balance) && Number.isFinite(Number(result.balance.remaining));
}

function rerenderManagementAccounts() {
  if (typeof filterChannels === 'function') filterChannels();
}

async function reloadManagementChannels(options) {
  if (options && options.reload === false) {
    rerenderManagementAccounts();
    return;
  }
  if (typeof loadChannels === 'function') await loadChannels();
  else rerenderManagementAccounts();
}

async function refreshManagementBalance(channelID, fetcher = fetchDataWithAuth, options = {}) {
  const numericID = managementChannelID(channelID);
  if (!numericID) throw new Error('A saved API Key channel is required');

  const operationID = ++managementBalanceOperationSequence;
  managementBalanceLastOperationByChannelID.set(numericID, operationID);
  managementBalanceOperationByChannelID.set(numericID, operationID);
  managementBalanceStateByChannelID.set(numericID, { status: 'loading' });
  rerenderManagementAccounts();
  try {
    const result = await fetcher(
      `/admin/channels/${numericID}/management-account/balance`,
      { method: 'POST' }
    );
    if (!isManagementBalancePayload(result)) {
      throw new Error(managementText('channels.management.balanceInvalid'));
    }
    if (managementBalanceOperationByChannelID.get(numericID) !== operationID) return result;
    managementBalanceOperationByChannelID.delete(numericID);
    managementBalanceStateByChannelID.set(numericID, { status: 'ready', data: result });
    await reloadManagementChannels(options);
    return result;
  } catch (error) {
    const message = (error && error.message) || managementText('channels.management.balanceFailed');
    if (managementBalanceOperationByChannelID.get(numericID) === operationID) {
      managementBalanceOperationByChannelID.delete(numericID);
      managementBalanceStateByChannelID.set(numericID, { status: 'error', error: message });
      rerenderManagementAccounts();
    }
    throw error;
  }
}

async function runManagementCheckin(channelID, fetcher = fetchDataWithAuth, options = {}) {
  const numericID = managementChannelID(channelID);
  if (!numericID) throw new Error('A saved API Key channel is required');

  managementBalanceLastOperationByChannelID.set(numericID, ++managementBalanceOperationSequence);
  const operationID = ++managementCheckinOperationSequence;
  managementCheckinOperationByChannelID.set(numericID, operationID);
  managementCheckinStateByChannelID.set(numericID, { status: 'loading' });
  rerenderManagementAccounts();
  try {
    const result = await fetcher(
      `/admin/channels/${numericID}/management-account/checkin`,
      { method: 'POST' }
    );
    if (!result || typeof result.status !== 'string' || result.status.trim() === '') {
      throw new Error(managementText('channels.management.checkinInvalid'));
    }
    if (managementCheckinOperationByChannelID.get(numericID) !== operationID) return result;
    managementCheckinOperationByChannelID.delete(numericID);
    managementCheckinStateByChannelID.set(numericID, { status: 'ready', data: result });
    // 签到附带的余额只在没有并发额度刷新时接管展示，避免覆盖更新的响应。
    if (isManagementBalancePayload(result) && !managementBalanceOperationByChannelID.has(numericID)) {
      managementBalanceStateByChannelID.set(numericID, {
        status: 'ready',
        data: { balance: result.balance }
      });
    }
    await reloadManagementChannels(options);
    return result;
  } catch (error) {
    const message = (error && error.message) || managementText('channels.management.checkinFailed');
    if (managementCheckinOperationByChannelID.get(numericID) === operationID) {
      managementCheckinOperationByChannelID.delete(numericID);
      managementCheckinStateByChannelID.set(numericID, { status: 'error', error: message });
      rerenderManagementAccounts();
    }
    throw error;
  }
}

if (typeof window !== 'undefined') {
  window.resetManagementAccountDraft = resetManagementAccountDraft;
  window.beginManagementAccountDraft = beginManagementAccountDraft;
  window.validateManagementAccountDraft = validateManagementAccountDraft;
  window.commitManagementAccountDraft = commitManagementAccountDraft;
  window.collectManagementAccountForSubmit = collectManagementAccountForSubmit;
  window.loginManagementAccount = loginManagementAccount;
  window.completeManagementAccountSave = completeManagementAccountSave;
  window.getManagementAccountRateConfig = getManagementAccountRateConfig;
  window.refreshManagementBalance = refreshManagementBalance;
  window.getManagementBalanceOperationSequence = getManagementBalanceOperationSequence;
  window.applyManagementBalanceBatchResult = applyManagementBalanceBatchResult;
  window.runManagementCheckin = runManagementCheckin;
  window.getManagementBalanceState = getManagementBalanceState;
  window.getManagementCheckinState = getManagementCheckinState;
  window.managementSupportsCheckin = managementSupportsCheckin;
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    MANAGEMENT_PROFILE_FIELDS,
    MANAGEMENT_CHECKIN_PROFILES,
    MANAGEMENT_RATE_PROFILES,
    firstManagementBaseURL,
    isManagementBaseURLValid,
    managementSupportsCheckin,
    readManagementAccountForm,
    renderManagementAccountFields,
    resetManagementAccountDraft,
    beginManagementAccountDraft,
    validateManagementAccountDraft,
    commitManagementAccountDraft,
    collectManagementAccountForSubmit,
    loginManagementAccount,
    completeManagementAccountSave,
    getManagementAccountRateConfig,
    refreshManagementBalance,
    getManagementBalanceOperationSequence,
    applyManagementBalanceBatchResult,
    runManagementCheckin,
    getManagementBalanceState,
    getManagementCheckinState
  };
}
