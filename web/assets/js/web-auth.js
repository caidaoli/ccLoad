(function (root, factory) {
  const api = factory();
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  if (root) root.WebAuth = api;
})(typeof window !== 'undefined' ? window : globalThis, function () {
  const TOKEN_KEY = 'ccload_token';
  const EXPIRY_KEY = 'ccload_token_expiry';
  const ROLE_KEY = 'ccload_web_role';
  const API_TOKEN_ROLE = 'api_token';
  const API_TOKEN_NAV = new Set(['index', 'channels', 'stats', 'trend', 'logs', 'model-test']);

  function buildLoginPayload(mode, credential) {
    if (mode === API_TOKEN_ROLE) return { mode, token: credential };
    return { mode: 'admin', password: credential };
  }

  function storeWebSession(storage, data, now = Date.now()) {
    clearWebSession(storage);
    storage.setItem(TOKEN_KEY, data.token);
    storage.setItem(EXPIRY_KEY, now + Number(data.expiresIn || 0) * 1000);
    storage.setItem(ROLE_KEY, data.role || 'admin');
  }

  function clearWebSession(storage) {
    storage.removeItem(TOKEN_KEY);
    storage.removeItem(EXPIRY_KEY);
    storage.removeItem(ROLE_KEY);
    storage.removeItem('ccload_api_token');
  }

  function getWebRole(storage) {
    return storage.getItem(ROLE_KEY) || 'admin';
  }

  function isAPITokenRole(storage) {
    return getWebRole(storage) === API_TOKEN_ROLE;
  }

  function filterNavigation(navKeys, role) {
    if (role !== API_TOKEN_ROLE) return [...navKeys];
    return navKeys.filter((key) => API_TOKEN_NAV.has(key));
  }

  function getSafeRedirectPath(redirect, origin) {
    if (!redirect || typeof redirect !== 'string') return '/web/index.html';
    const candidate = redirect.trim();
    if (!candidate.startsWith('/') || candidate.startsWith('//')) return '/web/index.html';
    try {
      const url = new URL(candidate, origin);
      if (url.origin !== origin) return '/web/index.html';
      return `${url.pathname}${url.search}${url.hash}`;
    } catch (_) {
      return '/web/index.html';
    }
  }

  return {
    TOKEN_KEY,
    EXPIRY_KEY,
    ROLE_KEY,
    buildLoginPayload,
    storeWebSession,
    clearWebSession,
    getWebRole,
    isAPITokenRole,
    filterNavigation,
    getSafeRedirectPath
  };
});
