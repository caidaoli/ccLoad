/**
 * Navigation interactions
 * The navigation HTML is pre-rendered by build.mjs. This script only handles the mobile menu, theme switching, and remembering the language choice.
 */
(function() {
  'use strict';

  const THEME_STORAGE_KEY = 'ccload_theme';
  const LOCALE_STORAGE_KEY = 'ccload_locale';
  const THEMES = ['system', 'light', 'dark'];
  const THEME_ICONS = {
    light: '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><circle cx="12" cy="12" r="5"/><line x1="12" y1="1" x2="12" y2="3"/><line x1="12" y1="21" x2="12" y2="23"/><line x1="4.22" y1="4.22" x2="5.64" y2="5.64"/><line x1="18.36" y1="18.36" x2="19.78" y2="19.78"/><line x1="1" y1="12" x2="3" y2="12"/><line x1="21" y1="12" x2="23" y2="12"/><line x1="4.22" y1="19.78" x2="5.64" y2="18.36"/><line x1="18.36" y1="5.64" x2="19.78" y2="4.22"/></svg>',
    dark: '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/></svg>',
    system: '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><rect x="2" y="3" width="20" height="14" rx="2" ry="2"/><line x1="8" y1="21" x2="16" y2="21"/><line x1="12" y1="17" x2="12" y2="21"/></svg>'
  };

  function initNav() {
    const navToggle = document.getElementById('www-nav-toggle');
    const navMenu = document.getElementById('www-nav-menu');
    if (navToggle && navMenu) {
      navToggle.addEventListener('click', () => {
        navToggle.setAttribute('aria-expanded', String(navMenu.classList.toggle('open')));
      });
      navMenu.querySelectorAll('.www-nav-link').forEach(link => {
        link.addEventListener('click', () => {
          navMenu.classList.remove('open');
          navToggle.setAttribute('aria-expanded', 'false');
        });
      });
    }

    // An explicit language switch is remembered; the inline script in the page head redirects on the next visit.
    const langSwitch = document.getElementById('www-lang-switch');
    if (langSwitch) {
      langSwitch.addEventListener('click', () => {
        try {
          localStorage.setItem(LOCALE_STORAGE_KEY, langSwitch.dataset.locale);
        } catch (_) { /* Even if storage fails, the link still navigates to the target language. */ }
        langSwitch.href = langSwitch.getAttribute('href').split('#')[0] + location.hash;
      });
    }

    const themeSwitch = document.getElementById('www-theme-switch');
    if (themeSwitch) {
      const initialTheme = getStoredTheme();
      applyTheme(initialTheme);
      themeSwitch.innerHTML = THEME_ICONS[initialTheme];

      themeSwitch.addEventListener('click', () => {
        const nextTheme = THEMES[(THEMES.indexOf(getStoredTheme()) + 1) % THEMES.length];
        applyTheme(nextTheme);
        setStoredTheme(nextTheme);
        themeSwitch.innerHTML = THEME_ICONS[nextTheme];
      });
    }
  }

  function getStoredTheme() {
    if (window.ccLoadTheme && typeof window.ccLoadTheme.getStoredTheme === 'function') {
      return window.ccLoadTheme.getStoredTheme();
    }
    try {
      const savedTheme = localStorage.getItem(THEME_STORAGE_KEY);
      const mode = typeof savedTheme === 'string' ? savedTheme.split(':', 1)[0] : null;
      return THEMES.includes(mode) ? mode : 'system';
    } catch (_) {
      return 'system';
    }
  }

  function setStoredTheme(theme) {
    if (window.ccLoadTheme && typeof window.ccLoadTheme.setStoredTheme === 'function') {
      window.ccLoadTheme.setStoredTheme(theme);
      return;
    }
    try {
      localStorage.setItem(THEME_STORAGE_KEY, `${theme}:${Date.now()}`);
    } catch (_) { /* The theme still applies to the current page. */ }
  }

  function resolveTheme(theme) {
    if (theme !== 'system') return theme;
    return window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  }

  function applyTheme(theme) {
    const resolvedTheme = resolveTheme(theme);
    const html = document.documentElement;
    html.setAttribute('data-theme', theme);
    html.setAttribute('data-resolved-theme', resolvedTheme);
    html.style.colorScheme = resolvedTheme;
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', initNav);
  } else {
    initNav();
  }
})();
