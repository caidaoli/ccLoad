(function () {
  const storageKey = 'ccload_theme';
  const modes = ['system', 'light', 'dark'];

  function getStoredTheme() {
    try {
      const saved = localStorage.getItem(storageKey);
      return modes.includes(saved) ? saved : 'system';
    } catch (_) {
      return 'system';
    }
  }

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
