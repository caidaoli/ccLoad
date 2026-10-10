// ============================================================
// i18n 国际化模块
// ============================================================
(function() {
  'use strict';

  // 语言包存储
  window.I18N_LOCALES = window.I18N_LOCALES || {};

  // 当前语言
  let currentLocale = 'zh-CN';

  // 支持的语言列表
  const SUPPORTED_LOCALES = ['zh-CN', 'en'];

  // 语言显示名称
  const LOCALE_NAMES = {
    'zh-CN': '中文',
    'en': 'English'
  };

  // 已注册的刷新回调
  const refreshCallbacks = [];

  /**
   * 检测浏览器语言
   * @returns {string} 语言代码
   */
  function detectBrowserLocale() {
    const nav = navigator.language || navigator.userLanguage || 'zh-CN';
    if (nav.startsWith('en')) return 'en';
    return 'zh-CN';
  }

  /**
   * 初始化 i18n
   */
  function init() {
    const saved = localStorage.getItem('ccload_locale');
    if (saved && SUPPORTED_LOCALES.includes(saved)) {
      currentLocale = saved;
    } else {
      currentLocale = detectBrowserLocale();
    }
    document.documentElement.lang = currentLocale;
  }

  /**
   * 获取当前语言
   * @returns {string}
   */
  function getLocale() {
    return currentLocale;
  }

  /**
   * 设置语言
   * @param {string} locale
   */
  function setLocale(locale) {
    if (!SUPPORTED_LOCALES.includes(locale)) {
      console.warn('[i18n] Unsupported locale:', locale);
      return;
    }
    currentLocale = locale;
    localStorage.setItem('ccload_locale', locale);
    document.documentElement.lang = locale;

    // 翻译静态页面元素
    translatePage();

    // 执行所有已注册的刷新回调
    refreshCallbacks.forEach(cb => {
      try { cb(locale); } catch (e) { console.error('[i18n] Refresh callback error:', e); }
    });

    // 触发自定义事件（兼容旧代码）
    window.dispatchEvent(new CustomEvent('localechange', { detail: { locale } }));
  }

  /**
   * 注册语言切换时的刷新回调
   * 用于需要重新渲染动态内容的模块
   * @param {Function} callback - 回调函数，接收新 locale 作为参数
   * @returns {Function} 取消注册的函数
   */
  function onLocaleChange(callback) {
    if (typeof callback !== 'function') return () => {};
    refreshCallbacks.push(callback);
    return () => {
      const idx = refreshCallbacks.indexOf(callback);
      if (idx > -1) refreshCallbacks.splice(idx, 1);
    };
  }

  /**
   * 占位符替换：{name} -> params.name
   * @param {string} text
   * @param {Object} [params]
   * @returns {string}
   */
  function interpolate(text, params) {
    if (!params || typeof text !== 'string') return text;
    let result = text;
    for (const k in params) {
      if (Object.prototype.hasOwnProperty.call(params, k)) {
        const safeKey = k.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
        result = result.replace(new RegExp('\\{' + safeKey + '\\}', 'g'), () => String(params[k]));
      }
    }
    return result;
  }

  /**
   * 翻译函数
   * @param {string} key - 翻译键，如 'nav.overview'
   * @param {Object} [params] - 插值参数，如 { count: 5 }
   * @returns {string} 翻译后的文本
   */
  function t(key, params) {
    if (!key) return '';

    const localeData = window.I18N_LOCALES[currentLocale] || {};
    let text = localeData[key];

    if (text === undefined) {
      // 回退到中文
      text = (window.I18N_LOCALES['zh-CN'] || {})[key];
      if (text === undefined) {
        // 生产环境不打印警告，避免日志污染
        if (typeof console !== 'undefined' && console.warn) {
          console.warn('[i18n] Missing key:', key);
        }
        return key;
      }
    }

    return interpolate(text, params);
  }

  /**
   * 翻译函数（带 fallback）
   * 与 t() 的差异：找不到 key 时返回 fallback（也会做插值）而非 key 本身，且不打 warn。
   * @param {string} key - 翻译键
   * @param {string} fallback - 找不到 key 时的回退文本
   * @param {Object} [params] - 插值参数
   * @returns {string}
   */
  function i18nText(key, fallback, params) {
    if (!key) return interpolate(fallback, params);

    const localeData = window.I18N_LOCALES[currentLocale] || {};
    let text = localeData[key];
    if (text === undefined) text = (window.I18N_LOCALES['zh-CN'] || {})[key];
    if (text === undefined) text = fallback;

    return interpolate(text, params);
  }

  // data-i18n-* 属性 → 写入目标；不支持 data-i18n-html 以避免 XSS 风险
  const I18N_BINDINGS = [
    ['data-i18n', el => el.textContent, (el, v) => { el.textContent = v; }],
    ['data-i18n-placeholder', el => el.placeholder, (el, v) => { el.placeholder = v; }],
    ['data-i18n-title', el => el.title, (el, v) => { el.title = v; }],
    ['data-i18n-value', el => el.value, (el, v) => { el.value = v; }],
    ['data-i18n-description', el => el.dataset.description, (el, v) => { el.dataset.description = v; }],
    ['data-i18n-content', el => el.getAttribute('content'), (el, v) => { el.setAttribute('content', v); }],
    ['data-i18n-alt', el => el.getAttribute('alt'), (el, v) => { el.setAttribute('alt', v); }],
    ['data-i18n-aria-label', el => el.getAttribute('aria-label'), (el, v) => { el.setAttribute('aria-label', v); }]
  ];

  /**
   * 翻译 root（默认整页）内带 data-i18n-* 属性的元素。
   * 文本未变化时不写 DOM：动态渲染后频繁调用，无谓写入会使整页布局失效并触发 MutationObserver。
   * @param {ParentNode} [root=document]
   */
  function translatePage(root = document) {
    for (const [attr, read, write] of I18N_BINDINGS) {
      const nodes = Array.from(root.querySelectorAll(`[${attr}]`));
      if (root !== document && root.hasAttribute?.(attr)) nodes.push(root);
      for (const el of nodes) {
        const key = el.getAttribute(attr);
        if (!key) continue;
        const text = t(key);
        if (read(el) !== text) write(el, text);
      }
    }
  }

  /**
   * 获取支持的语言列表
   * @returns {Array<{code: string, name: string}>}
   */
  function getSupportedLocales() {
    return SUPPORTED_LOCALES.map(code => ({
      code,
      name: LOCALE_NAMES[code]
    }));
  }

  /**
   * 创建语言切换器下拉菜单（图标样式）
   * @returns {HTMLElement}
   */
  function createLanguageSwitcher() {
    const wrapper = document.createElement('div');
    wrapper.className = 'lang-dropdown';

    const trigger = document.createElement('button');
    trigger.className = 'lang-dropdown-trigger';
    trigger.setAttribute('aria-label', 'Select language');
    trigger.setAttribute('aria-haspopup', 'true');
    trigger.setAttribute('aria-expanded', 'false');
    trigger.innerHTML = `
      <svg class="lang-icon" viewBox="0 0 24 24" fill="currentColor">
        <path d="M12.87 15.07l-2.54-2.51.03-.03A17.52 17.52 0 0014.07 6H17V4h-7V2H8v2H1v2h11.17C11.5 7.92 10.44 9.75 9 11.35 8.07 10.32 7.3 9.19 6.69 8h-2c.73 1.63 1.73 3.17 2.98 4.56l-5.09 5.02L4 19l5-5 3.11 3.11.76-2.04zM18.5 10h-2L12 22h2l1.12-3h4.75L21 22h2l-4.5-12zm-2.62 7l1.62-4.33L19.12 17h-3.24z"/>
      </svg>
      <svg class="lang-arrow" viewBox="0 0 12 12" fill="currentColor">
        <path d="M2.5 4.5L6 8L9.5 4.5" stroke="currentColor" stroke-width="1.5" fill="none" stroke-linecap="round" stroke-linejoin="round"/>
      </svg>
    `;

    const menu = document.createElement('div');
    menu.className = 'lang-dropdown-menu';
    menu.setAttribute('role', 'menu');

    getSupportedLocales().forEach(({ code, name }) => {
      const item = document.createElement('button');
      item.className = 'lang-dropdown-item';
      item.setAttribute('role', 'menuitem');
      item.setAttribute('data-locale', code);
      item.textContent = name;
      if (code === currentLocale) {
        item.classList.add('active');
      }
      item.addEventListener('click', () => {
        setLocale(code);
        menu.querySelectorAll('.lang-dropdown-item').forEach(el => el.classList.remove('active'));
        item.classList.add('active');
        closeMenu();
      });
      menu.appendChild(item);
    });

    wrapper.appendChild(trigger);
    wrapper.appendChild(menu);

    function toggleMenu() {
      const isOpen = wrapper.classList.toggle('open');
      trigger.setAttribute('aria-expanded', isOpen);
    }

    function closeMenu() {
      wrapper.classList.remove('open');
      trigger.setAttribute('aria-expanded', 'false');
    }

    trigger.addEventListener('click', (e) => {
      e.stopPropagation();
      toggleMenu();
    });

    document.addEventListener('click', (e) => {
      if (!wrapper.contains(e.target)) {
        closeMenu();
      }
    });

    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') {
        closeMenu();
      }
    });

    return wrapper;
  }

  // 初始化
  init();

  // 导出到全局
  window.i18n = {
    t,
    getLocale,
    setLocale,
    translatePage,
    getSupportedLocales,
    createLanguageSwitcher,
    onLocaleChange
  };

  // 简写形式 - 保证 t() 和 i18nText() 永远可用
  window.t = t;
  window.i18nText = i18nText;
})();
