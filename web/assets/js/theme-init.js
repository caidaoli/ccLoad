(function () {
  const storageKey = 'ccload_theme';
  const modes = ['system', 'light', 'dark'];
  const cookieMaxAge = 60 * 60 * 24 * 365;

  function normalizeTheme(value) {
    return modes.includes(value) ? value : null;
  }

  function parseStoredTheme(value) {
    const legacyMode = normalizeTheme(value);
    if (legacyMode) return { mode: legacyMode, updatedAt: 0 };
    if (typeof value !== 'string') return null;

    const separator = value.lastIndexOf(':');
    const mode = normalizeTheme(value.slice(0, separator));
    const updatedAt = Number(value.slice(separator + 1));
    if (!mode || !Number.isSafeInteger(updatedAt) || updatedAt <= 0) return null;
    return { mode, updatedAt };
  }

  function getCookieTheme() {
    try {
      const prefix = `${storageKey}=`;
      const entry = document.cookie.split(';').map((part) => part.trim()).find((part) => part.startsWith(prefix));
      return entry ? parseStoredTheme(decodeURIComponent(entry.slice(prefix.length))) : null;
    } catch (_) {
      return null;
    }
  }

  function getLocalTheme() {
    try {
      return parseStoredTheme(localStorage.getItem(storageKey));
    } catch (_) {
      return null;
    }
  }

  function getStoredTheme() {
    const localTheme = getLocalTheme();
    const cookieTheme = getCookieTheme();
    if (!localTheme) return cookieTheme ? cookieTheme.mode : 'system';
    if (!cookieTheme) return localTheme.mode;
    return cookieTheme.updatedAt > localTheme.updatedAt ? cookieTheme.mode : localTheme.mode;
  }

  function setStoredTheme(mode) {
    if (!normalizeTheme(mode)) return false;
    const localTheme = getLocalTheme();
    const cookieTheme = getCookieTheme();
    const updatedAt = Math.max(
      Date.now(),
      (localTheme ? localTheme.updatedAt : 0) + 1,
      (cookieTheme ? cookieTheme.updatedAt : 0) + 1
    );
    const storedValue = `${mode}:${updatedAt}`;
    let persisted = false;

    try {
      localStorage.setItem(storageKey, storedValue);
      const savedTheme = getLocalTheme();
      persisted = Boolean(savedTheme && savedTheme.mode === mode && savedTheme.updatedAt === updatedAt);
    } catch (_) { /* Cookie remains available when browser storage is blocked. */ }

    try {
      document.cookie = `${storageKey}=${encodeURIComponent(storedValue)}; Path=/; Max-Age=${cookieMaxAge}; SameSite=Lax`;
      const savedTheme = getCookieTheme();
      persisted = Boolean(savedTheme && savedTheme.mode === mode && savedTheme.updatedAt === updatedAt) || persisted;
    } catch (_) { /* The selected theme still applies to the current page. */ }

    return persisted;
  }

  window.ccLoadTheme = { getStoredTheme, setStoredTheme };

  function resolveTheme(mode) {
    if (mode !== 'system') return mode;
    return window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  }

  const theme = getStoredTheme();
  const resolvedTheme = resolveTheme(theme);
  document.documentElement.dataset.theme = theme;
  document.documentElement.dataset.resolvedTheme = resolvedTheme;
  document.documentElement.style.colorScheme = resolvedTheme;
  document.documentElement.style.backgroundColor = resolvedTheme === 'dark' ? '#0f172a' : '#fcfbf9';
  document.documentElement.style.color = resolvedTheme === 'dark' ? '#e5e7eb' : '#111827';

  const meta = document.querySelector('meta[name="theme-color"]');
  if (meta) meta.setAttribute('content', resolvedTheme === 'dark' ? '#0f172a' : '#3b82f6');

  function clearInitialPaintStyle() {
    document.documentElement.style.removeProperty('background-color');
    document.documentElement.style.removeProperty('color');
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', clearInitialPaintStyle, { once: true });
  } else {
    requestAnimationFrame(clearInitialPaintStyle);
  }
})();
