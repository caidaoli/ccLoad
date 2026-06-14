/**
 * 导航组件
 * 功能：动态生成导航菜单、语言切换、主题切换、移动端汉堡菜单
 */
(function() {
  'use strict';

  // 导航菜单项配置
  const NAV_ITEMS = [
    { key: 'home', href: 'index.html', label: 'www.nav.home', fallback: '产品概览', icon: 'home' },
    { key: 'install', href: 'install.html', label: 'www.nav.install', fallback: '部署安装', icon: 'download' },
    { key: 'config', href: 'config.html', label: 'www.nav.config', fallback: '配置手册', icon: 'settings' },
    { key: 'usage', href: 'usage.html', label: 'www.nav.usage', fallback: 'API 使用', icon: 'terminal' },
    { key: 'feedback', href: 'feedback.html', label: 'www.nav.feedback', fallback: '反馈支持', icon: 'message' }
  ];

  const ICONS = {
    home: '<svg class="www-nav-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M3 11.5 12 4l9 7.5"/><path d="M5 10.5V20h14v-9.5"/><path d="M9 20v-6h6v6"/></svg>',
    download: '<svg class="www-nav-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3v12"/><path d="m7 10 5 5 5-5"/><path d="M5 21h14"/></svg>',
    settings: '<svg class="www-nav-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 15.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7Z"/><path d="M19.4 15a1.8 1.8 0 0 0 .36 1.98l.05.05a2.1 2.1 0 0 1-2.97 2.97l-.05-.05a1.8 1.8 0 0 0-1.98-.36 1.8 1.8 0 0 0-1.08 1.65V21a2.1 2.1 0 0 1-4.2 0v-.07a1.8 1.8 0 0 0-1.08-1.65 1.8 1.8 0 0 0-1.98.36l-.05.05a2.1 2.1 0 0 1-2.97-2.97l.05-.05A1.8 1.8 0 0 0 4.6 15a1.8 1.8 0 0 0-1.65-1.08H3a2.1 2.1 0 0 1 0-4.2h.07A1.8 1.8 0 0 0 4.72 8.65a1.8 1.8 0 0 0-.36-1.98l-.05-.05a2.1 2.1 0 0 1 2.97-2.97l.05.05a1.8 1.8 0 0 0 1.98.36A1.8 1.8 0 0 0 10.4 2.4V2.1a2.1 2.1 0 0 1 4.2 0v.3a1.8 1.8 0 0 0 1.08 1.65 1.8 1.8 0 0 0 1.98-.36l.05-.05a2.1 2.1 0 0 1 2.97 2.97l-.05.05a1.8 1.8 0 0 0-.36 1.98 1.8 1.8 0 0 0 1.65 1.08H22a2.1 2.1 0 0 1 0 4.2h-.07A1.8 1.8 0 0 0 19.4 15Z"/></svg>',
    terminal: '<svg class="www-nav-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="m4 17 6-6-6-6"/><path d="M12 19h8"/></svg>',
    message: '<svg class="www-nav-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M21 12a8 8 0 0 1-8 8H6l-3 3v-7a8 8 0 1 1 18-4Z"/></svg>'
  };

  const THEME_STORAGE_KEY = 'ccload_theme';
  const THEMES = ['system', 'light', 'dark'];

  /**
   * 获取当前页面的 key
   */
  function getCurrentPageKey() {
    const path = window.location.pathname;
    if (path.includes('index.html') || path.endsWith('/www/') || path.endsWith('/www')) {
      return 'home';
    }
    for (const item of NAV_ITEMS) {
      if (path.includes(item.key)) {
        return item.key;
      }
    }
    return '';
  }

  /**
   * 创建导航栏 HTML
   */
  function createNavHTML() {
    const currentKey = getCurrentPageKey();

    const menuItemsHTML = NAV_ITEMS.map(item => {
      const activeClass = currentKey === item.key ? 'active' : '';
      const externalAttr = item.external ? 'target="_blank" rel="noopener"' : '';
      return `
        <li>
          <a href="${item.href}"
             class="www-nav-link ${activeClass}"
             ${externalAttr}>
            ${ICONS[item.icon] || ''}
            <span data-i18n="${item.label}">${item.fallback}</span>
          </a>
        </li>
      `;
    }).join('');

    return `
      <nav class="www-nav">
        <div class="www-nav-container">
          <a href="index.html" class="www-nav-logo">
            <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
              <path d="M12 2L2 7l10 5 10-5-10-5z"/>
              <path d="M2 17l10 5 10-5"/>
              <path d="M2 12l10 5 10-5"/>
            </svg>
            <span>ccLoad</span>
          </a>

          <ul class="www-nav-menu" id="www-nav-menu">
            ${menuItemsHTML}
          </ul>

          <div class="www-nav-actions">
            <a href="https://github.com/caidaoli/ccLoad" target="_blank" rel="noopener" class="www-btn-secondary www-icon-button" data-i18n-title="www.nav.github" aria-label="GitHub">
              <svg class="www-action-icon" viewBox="0 0 24 24" aria-hidden="true">
                <path d="M12 0c-6.626 0-12 5.373-12 12 0 5.302 3.438 9.8 8.207 11.387.599.111.793-.261.793-.577v-2.234c-3.338.726-4.033-1.416-4.033-1.416-.546-1.387-1.333-1.756-1.333-1.756-1.089-.745.083-.729.083-.729 1.205.084 1.839 1.237 1.839 1.237 1.07 1.834 2.807 1.304 3.492.997.107-.775.418-1.305.762-1.604-2.665-.305-5.467-1.334-5.467-5.931 0-1.311.469-2.381 1.236-3.221-.124-.303-.535-1.524.117-3.176 0 0 1.008-.322 3.301 1.23.957-.266 1.983-.399 3.003-.404 1.02.005 2.047.138 3.006.404 2.291-1.552 3.297-1.23 3.297-1.23.653 1.653.242 2.874.118 3.176.77.84 1.235 1.911 1.235 3.221 0 4.609-2.807 5.624-5.479 5.921.43.372.823 1.102.823 2.222v3.293c0 .319.192.694.801.576 4.765-1.589 8.199-6.086 8.199-11.386 0-6.627-5.373-12-12-12z"/>
              </svg>
            </a>
            <button id="www-lang-switch" class="www-btn-secondary www-icon-button www-lang-button" data-i18n-title="www.nav.switchLanguage" aria-label="Switch language">
              <svg class="www-action-icon" viewBox="0 0 24 24" aria-hidden="true">
                <path d="M12.87 15.07 10.33 12.56l.03-.03A17.52 17.52 0 0 0 14.07 6H17V4h-7V2H8v2H1v2h11.17C11.5 7.92 10.44 9.75 9 11.35 8.07 10.32 7.3 9.19 6.69 8h-2c.73 1.63 1.73 3.17 2.98 4.56L2.58 17.58 4 19l5-5 3.11 3.11.76-2.04ZM18.5 10h-2L12 22h2l1.12-3h4.75L21 22h2l-4.5-12Zm-2.62 7 1.62-4.33L19.12 17h-3.24Z"/>
              </svg>
              <span id="www-lang-label">EN</span>
            </button>
            <button id="www-theme-switch" class="www-btn-secondary www-icon-button" data-i18n-title="www.nav.switchTheme" aria-label="Switch theme">
              <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                <circle cx="12" cy="12" r="5"/>
                <line x1="12" y1="1" x2="12" y2="3"/>
                <line x1="12" y1="21" x2="12" y2="23"/>
                <line x1="4.22" y1="4.22" x2="5.64" y2="5.64"/>
                <line x1="18.36" y1="18.36" x2="19.78" y2="19.78"/>
                <line x1="1" y1="12" x2="3" y2="12"/>
                <line x1="21" y1="12" x2="23" y2="12"/>
                <line x1="4.22" y1="19.78" x2="5.64" y2="18.36"/>
                <line x1="18.36" y1="5.64" x2="19.78" y2="4.22"/>
              </svg>
            </button>
            <button class="www-nav-toggle" id="www-nav-toggle" aria-label="Toggle menu">
              <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
                <line x1="3" y1="12" x2="21" y2="12"/>
                <line x1="3" y1="6" x2="21" y2="6"/>
                <line x1="3" y1="18" x2="21" y2="18"/>
              </svg>
            </button>
          </div>
        </div>
      </nav>
    `;
  }

  /**
   * 初始化导航栏
   */
  function initNav() {
    // 插入导航栏 HTML
    const body = document.body;
    const navHTML = createNavHTML();
    body.insertAdjacentHTML('afterbegin', navHTML);

    if (window.i18n && typeof window.i18n.translatePage === 'function') {
      window.i18n.translatePage();
    }

    // 移动端菜单切换
    const navToggle = document.getElementById('www-nav-toggle');
    const navMenu = document.getElementById('www-nav-menu');

    if (navToggle && navMenu) {
      navToggle.addEventListener('click', () => {
        navMenu.classList.toggle('open');
      });

      // 点击菜单项后关闭移动端菜单
      navMenu.querySelectorAll('.www-nav-link').forEach(link => {
        link.addEventListener('click', () => {
          navMenu.classList.remove('open');
        });
      });
    }

    // 语言切换
    const langSwitch = document.getElementById('www-lang-switch');
    const langLabel = document.getElementById('www-lang-label');

    if (langSwitch && window.i18n && typeof window.i18n.setLocale === 'function') {
      // 更新语言标签
      function updateLangLabel() {
        const currentLocale = window.i18n.getLocale ? window.i18n.getLocale() : (localStorage.getItem('ccload_locale') || 'zh-CN');
        langLabel.textContent = currentLocale === 'zh-CN' ? '中文' : 'EN';
      }

      updateLangLabel();

      langSwitch.addEventListener('click', () => {
        const currentLocale = window.i18n.getLocale ? window.i18n.getLocale() : (localStorage.getItem('ccload_locale') || 'zh-CN');
        const newLocale = currentLocale === 'zh-CN' ? 'en' : 'zh-CN';
        window.i18n.setLocale(newLocale);
        updateLangLabel();
      });

      // 监听语言变化
      window.addEventListener('localechange', updateLangLabel);
    }

    // 主题切换
    const themeSwitch = document.getElementById('www-theme-switch');

    if (themeSwitch) {
      const initialTheme = getStoredTheme();
      applyTheme(initialTheme);
      updateThemeIcon(initialTheme);

      themeSwitch.addEventListener('click', () => {
        const currentTheme = getStoredTheme();
        const currentIndex = THEMES.indexOf(currentTheme);
        const nextTheme = THEMES[(currentIndex + 1) % THEMES.length];

        applyTheme(nextTheme);
        localStorage.setItem(THEME_STORAGE_KEY, nextTheme);
        updateThemeIcon(nextTheme);
      });
    }
  }

  function getStoredTheme() {
    const savedTheme = localStorage.getItem(THEME_STORAGE_KEY);
    return THEMES.includes(savedTheme) ? savedTheme : 'system';
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

  /**
   * 更新主题图标
   */
  function updateThemeIcon(theme) {
    const themeSwitch = document.getElementById('www-theme-switch');
    if (!themeSwitch) return;

    let iconHTML = '';
    if (theme === 'light') {
      iconHTML = `
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <circle cx="12" cy="12" r="5"/>
          <line x1="12" y1="1" x2="12" y2="3"/>
          <line x1="12" y1="21" x2="12" y2="23"/>
          <line x1="4.22" y1="4.22" x2="5.64" y2="5.64"/>
          <line x1="18.36" y1="18.36" x2="19.78" y2="19.78"/>
          <line x1="1" y1="12" x2="3" y2="12"/>
          <line x1="21" y1="12" x2="23" y2="12"/>
          <line x1="4.22" y1="19.78" x2="5.64" y2="18.36"/>
          <line x1="18.36" y1="5.64" x2="19.78" y2="4.22"/>
        </svg>
      `;
    } else if (theme === 'dark') {
      iconHTML = `
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <path d="M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79z"/>
        </svg>
      `;
    } else {
      iconHTML = `
        <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <rect x="2" y="3" width="20" height="14" rx="2" ry="2"/>
          <line x1="8" y1="21" x2="16" y2="21"/>
          <line x1="12" y1="17" x2="12" y2="21"/>
        </svg>
      `;
    }

    themeSwitch.innerHTML = iconHTML;
  }

  // DOM 加载完成后初始化
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', initNav);
  } else {
    initNav();
  }
})();
