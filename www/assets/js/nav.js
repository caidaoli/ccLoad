/**
 * 导航组件
 * 功能：动态生成导航菜单、语言切换、主题切换、移动端汉堡菜单
 */
(function() {
  'use strict';

  // 导航菜单项配置
  const NAV_ITEMS = [
    { key: 'home', href: 'index.html', label: 'www.nav.home' },
    { key: 'install', href: 'install.html', label: 'www.nav.install' },
    { key: 'config', href: 'config.html', label: 'www.nav.config' },
    { key: 'usage', href: 'usage.html', label: 'www.nav.usage' },
    { key: 'feedback', href: 'feedback.html', label: 'www.nav.feedback' },
    { key: 'admin', href: '/web/index.html', label: 'www.nav.admin', external: true }
  ];

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
             data-i18n="${item.label}"
             ${externalAttr}>
            ${item.label}
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
            <button id="www-lang-switch" class="www-btn-secondary" style="padding: 0.5rem 1rem; font-size: 0.875rem;">
              <span id="www-lang-label">EN</span>
            </button>
            <button id="www-theme-switch" class="www-btn-secondary" style="padding: 0.5rem 1rem;">
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

    if (langSwitch && window.setLocale) {
      // 更新语言标签
      function updateLangLabel() {
        const currentLocale = localStorage.getItem('ccload_locale') || 'zh-CN';
        langLabel.textContent = currentLocale === 'zh-CN' ? '中文' : 'EN';
      }

      updateLangLabel();

      langSwitch.addEventListener('click', () => {
        const currentLocale = localStorage.getItem('ccload_locale') || 'zh-CN';
        const newLocale = currentLocale === 'zh-CN' ? 'en' : 'zh-CN';
        window.setLocale(newLocale);
        updateLangLabel();
      });

      // 监听语言变化
      window.addEventListener('localechange', updateLangLabel);
    }

    // 主题切换
    const themeSwitch = document.getElementById('www-theme-switch');

    if (themeSwitch) {
      themeSwitch.addEventListener('click', () => {
        const html = document.documentElement;
        const currentTheme = html.getAttribute('data-theme') || 'system';
        const themes = ['system', 'light', 'dark'];
        const currentIndex = themes.indexOf(currentTheme);
        const nextTheme = themes[(currentIndex + 1) % themes.length];

        html.setAttribute('data-theme', nextTheme);
        localStorage.setItem('theme', nextTheme);

        // 更新图标
        updateThemeIcon(nextTheme);
      });
    }
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
