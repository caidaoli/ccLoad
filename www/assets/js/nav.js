/**
 * Navigation interactions
 * The navigation HTML is pre-rendered by build.mjs. This script only handles the mobile menu, theme switching, and remembering the language choice.
 * Theme storage comes from theme-init.js (window.ccLoadTheme), which every page loads before this script.
 */
(function() {
  'use strict';

  const LOCALE_STORAGE_KEY = 'ccload_locale';
  const THEMES = ['system', 'light', 'dark'];
  const THEME_ICONS = {
    light: '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><circle cx="12" cy="12" r="5"/><line x1="12" y1="1" x2="12" y2="3"/><line x1="12" y1="21" x2="12" y2="23"/><line x1="4.22" y1="4.22" x2="5.64" y2="5.64"/><line x1="18.36" y1="18.36" x2="19.78" y2="19.78"/><line x1="1" y1="12" x2="3" y2="12"/><line x1="21" y1="12" x2="23" y2="12"/><line x1="4.22" y1="19.78" x2="5.64" y2="18.36"/><line x1="18.36" y1="5.64" x2="19.78" y2="4.22"/></svg>',
    dark: '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/></svg>',
    system: '<svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><rect x="2" y="3" width="20" height="14" rx="2" ry="2"/><line x1="8" y1="21" x2="16" y2="21"/><line x1="12" y1="17" x2="12" y2="21"/></svg>'
  };

  function initMenu() {
    const navToggle = document.getElementById('www-nav-toggle');
    const navMenu = document.getElementById('www-nav-menu');
    if (!navToggle || !navMenu) return;

    const close = () => {
      navMenu.classList.remove('open');
      navToggle.setAttribute('aria-expanded', 'false');
    };
    navToggle.addEventListener('click', () => {
      navToggle.setAttribute('aria-expanded', String(navMenu.classList.toggle('open')));
    });
    navMenu.querySelectorAll('.www-nav-link').forEach(link => link.addEventListener('click', close));
    // Esc 收起并把焦点还给开关；点菜单外任意处收起
    document.addEventListener('keydown', event => {
      if (event.key !== 'Escape' || !navMenu.classList.contains('open')) return;
      close();
      navToggle.focus();
    });
    document.addEventListener('click', event => {
      if (navMenu.classList.contains('open') && !navMenu.contains(event.target) && !navToggle.contains(event.target)) close();
    });
  }

  // An explicit language switch is remembered; the inline script in the page head redirects on the next visit.
  function initLanguageSwitch() {
    const langSwitch = document.getElementById('www-lang-switch');
    if (!langSwitch) return;
    langSwitch.addEventListener('click', () => {
      try {
        localStorage.setItem(LOCALE_STORAGE_KEY, langSwitch.dataset.locale);
      } catch (_) { /* Even if storage fails, the link still navigates to the target language. */ }
      langSwitch.href = langSwitch.getAttribute('href').split('#')[0] + location.hash;
    });
  }

  function initThemeSwitch() {
    const themeSwitch = document.getElementById('www-theme-switch');
    if (!themeSwitch || !window.ccLoadTheme) return;

    // 图标与可读标签都反映当前模式；标签文案由构建期按语言写在 data-label-<mode> 上
    const render = mode => {
      const label = themeSwitch.getAttribute(`data-label-${mode}`);
      themeSwitch.innerHTML = THEME_ICONS[mode];
      themeSwitch.setAttribute('aria-label', label);
      themeSwitch.title = label;
    };
    // 当前模式以 theme-init.js 写到 <html> 上的为准，存储被禁用时也能正常轮换
    render(document.documentElement.dataset.theme);

    themeSwitch.addEventListener('click', () => {
      const next = THEMES[(THEMES.indexOf(document.documentElement.dataset.theme) + 1) % THEMES.length];
      applyTheme(next);
      window.ccLoadTheme.setStoredTheme(next);
      render(next);
    });
  }

  function applyTheme(theme) {
    const resolved = theme === 'system'
      ? (window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light')
      : theme;
    const html = document.documentElement;
    html.dataset.theme = theme;
    html.dataset.resolvedTheme = resolved;
    html.style.colorScheme = resolved;
  }

  function initNav() {
    initMenu();
    initLanguageSwitch();
    initThemeSwitch();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', initNav);
  } else {
    initNav();
  }
})();
