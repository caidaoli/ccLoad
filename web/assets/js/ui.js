// ============================================================
// Token认证工具（统一API调用，替代Cookie Session）
// ============================================================
(function () {
  /**
   * 生成带redirect参数的登录页URL
   * @returns {string}
   */
  function getLoginUrl() {
    const currentPath = window.location.pathname + window.location.search;
    // 排除登录页本身
    if (currentPath.includes('/web/login.html')) {
      return '/web/login.html';
    }
    return '/web/login.html?redirect=' + encodeURIComponent(currentPath);
  }

  // 导出到全局作用域
  window.getLoginUrl = getLoginUrl;

  /**
   * 带Token认证的fetch封装
   * @param {string} url - 请求URL
   * @param {Object} options - fetch选项
   * @returns {Promise<Response>}
   */
  async function fetchWithAuth(url, options = {}) {
    const token = localStorage.getItem('ccload_token');
    const expiry = localStorage.getItem('ccload_token_expiry');

    // 检查Token过期（静默跳转，不显示错误提示）
    if (!token || (expiry && Date.now() > parseInt(expiry))) {
      localStorage.removeItem('ccload_token');
      localStorage.removeItem('ccload_token_expiry');
      window.location.href = getLoginUrl();
      throw new Error('Token expired');
    }

    // 合并Authorization头
    const headers = {
      ...options.headers,
      'Authorization': `Bearer ${token}`,
    };

    const response = await fetch(url, { ...options, headers });

    // 处理401未授权（静默跳转，不显示错误提示）
    if (response.status === 401) {
      localStorage.removeItem('ccload_token');
      localStorage.removeItem('ccload_token_expiry');
      window.location.href = getLoginUrl();
      throw new Error('Unauthorized');
    }

    return response;
  }

  // 导出到全局作用域
  window.fetchWithAuth = fetchWithAuth;
})();

// ============================================================
// API响应解析（统一后端返回格式：{success,data,error,count}）
// ============================================================
(function () {
  async function parseAPIResponse(res) {
    const text = await res.text();
    if (!text) {
      throw new Error(t('error.emptyResponse') + ` (HTTP ${res.status})`);
    }

    let payload;
    try {
      payload = JSON.parse(text);
    } catch (e) {
      throw new Error(t('error.invalidJson') + ` (HTTP ${res.status})`);
    }

    if (!payload || typeof payload !== 'object' || typeof payload.success !== 'boolean') {
      throw new Error(t('error.invalidFormat') + ` (HTTP ${res.status})`);
    }

    return payload;
  }

  async function fetchAPI(url, options = {}) {
    const res = await fetch(url, options);
    return parseAPIResponse(res);
  }

  async function fetchAPIWithAuth(url, options = {}) {
    const res = await fetchWithAuth(url, options);
    return parseAPIResponse(res);
  }

  // 需要同时读取响应头（如 X-Debug-*）的场景：返回 { res, payload }
  async function fetchAPIWithAuthRaw(url, options = {}) {
    const res = await fetchWithAuth(url, options);
    const payload = await parseAPIResponse(res);
    return { res, payload };
  }

  async function fetchData(url, options = {}) {
    const resp = await fetchAPI(url, options);
    if (!resp.success) throw new Error(resp.error || t('error.requestFailed'));
    return resp.data;
  }

  async function fetchDataWithAuth(url, options = {}) {
    const resp = await fetchAPIWithAuth(url, options);
    if (!resp.success) throw new Error(resp.error || t('error.requestFailed'));
    return resp.data;
  }

  window.fetchAPI = fetchAPI;
  window.fetchAPIWithAuth = fetchAPIWithAuth;
  window.fetchAPIWithAuthRaw = fetchAPIWithAuthRaw;
  window.fetchData = fetchData;
  window.fetchDataWithAuth = fetchDataWithAuth;
})();

// ============================================================
// 共享UI：顶部导航与背景动画（KISS/DRY）
// 使用方式：在页面底部引入本文件，并调用 initTopbar('index'|'configs'|'stats'|'trend'|'errors')
// ============================================================
(function () {
  const NAVS = [
    { key: 'index', labelKey: 'nav.overview', href: '/web/index.html', icon: iconHome },
    { key: 'channels', labelKey: 'nav.channels', href: '/web/channels.html', icon: iconSettings },
    { key: 'tokens', labelKey: 'nav.tokens', href: '/web/tokens.html', icon: iconKey },
    { key: 'stats', labelKey: 'nav.stats', href: '/web/stats.html', icon: iconBars },
    { key: 'trend', labelKey: 'nav.trend', href: '/web/trend.html', icon: iconTrend },
    { key: 'logs', labelKey: 'nav.logs', href: '/web/logs.html', icon: iconAlert },
    { key: 'model-test', labelKey: 'nav.modelTest', href: '/web/model-test.html', icon: iconTest },
    { key: 'settings', labelKey: 'nav.settings', href: '/web/settings.html', icon: iconSettings },
  ];

  function h(tag, attrs = {}, children = []) {
    const el = document.createElement(tag);
    Object.entries(attrs).forEach(([k, v]) => {
      if (k === 'class') el.className = v;
      else if (k === 'style') el.style.cssText = v;
      else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
      else el.setAttribute(k, v);
    });
    (Array.isArray(children) ? children : [children]).forEach((c) => {
      if (c == null) return;
      if (typeof c === 'string') el.appendChild(document.createTextNode(c));
      else el.appendChild(c);
    });
    return el;
  }

  function iconHome() {
    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 7v10a2 2 0 002 2h14a2 2 0 002-2V9a2 2 0 00-2-2H5a2 2 0 00-2-2z"/><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 5a2 2 0 012-2h4a2 2 0 012 2v0a2 2 0 01-2 2H10a2 2 0 01-2-2v0z"/>`);
  }
  function iconSettings() {
    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M10.325 4.317c.426-1.756 2.924-1.756 3.35 0a1.724 1.724 0 002.573 1.066c1.543-.94 3.31.826 2.37 2.37a1.724 1.724 0 001.065 2.572c1.756.426 1.756 2.924 0 3.35a1.724 1.724 0 00-1.066 2.573c.94 1.543-.826 3.31-2.37 2.37a1.724 1.724 0 00-2.572 1.065c-.426 1.756-2.924 1.756-3.35 0a1.724 1.724 0 00-2.573-1.066c-1.543.94-3.31-.826-2.37-2.37a1.724 1.724 0 00-1.065-2.572c-1.756-.426-1.756-2.924 0-3.35a1.724 1.724 0 001.066-2.573c-.94-1.543.826-3.31 2.37-2.37.996.608 2.296.07 2.572-1.065z"/><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 12a3 3 0 11-6 0 3 3 0 016 0z"/>`);
  }
  function iconBars() {
    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 19v-6a2 2 0 00-2-2H5a2 2 0 00-2 2v6a2 2 0 002 2h2a2 2 0 002-2zm0 0V9a2 2 0 012-2h2a2 2 0 012 2v10m-6 0a2 2 0 002 2h2a2 2 0 002-2m0 0V5a2 2 0 012-2h2a2 2 0 012 2v14a2 2 0 01-2 2h-2a2 2 0 01-2-2z"/>`);
  }
  function iconTrend() {
    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M7 12l3-3 3 3 4-4"/><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 21l4-4 4 4"/><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 4h18"/><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4h16v12a1 1 0 01-1 1H5a1 1 0 01-1-1V4z"/>`);
  }
  function iconAlert() {
    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-2.5L13.732 4c-.77-.833-1.864-.833-2.634 0L4.18 16.5c-.77.833.192 2.5 1.732 2.5z"/>`);
  }
  function iconKey() {
    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 7a2 2 0 012 2m4 0a6 6 0 01-7.743 5.743L11 17H9v2H7v2H4a1 1 0 01-1-1v-2.586a1 1 0 01.293-.707l5.964-5.964A6 6 0 1121 9z"/>`);
  }
  function iconTest() {
    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 12l2 2 4-4m6 2a9 9 0 11-18 0 9 9 0 0118 0z"/>`);
  }
  function svg(inner) {
    const el = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    el.setAttribute('fill', 'none');
    el.setAttribute('stroke', 'currentColor');
    el.setAttribute('viewBox', '0 0 24 24');
    el.classList.add('w-5', 'h-5');
    el.innerHTML = inner;
    return el;
  }

  function isLoggedIn() {
    const token = localStorage.getItem('ccload_token');
    const expiry = localStorage.getItem('ccload_token_expiry');
    return token && (!expiry || Date.now() <= parseInt(expiry));
  }

  // GitHub仓库地址
  const GITHUB_REPO_URL = 'https://github.com/caidaoli/ccLoad';
  const GITHUB_RELEASES_URL = 'https://github.com/caidaoli/ccLoad/releases';

  // 版本信息
  let versionInfo = null;

  // 获取版本信息（后端已包含新版本检测结果）
  async function fetchVersionInfo() {
    try {
      const res = await fetch('/public/version');
      const resp = await res.json();
      versionInfo = resp.data;
      return versionInfo;
    } catch (e) {
      console.error('Failed to fetch version info:', e);
      return null;
    }
  }

  // 更新版本显示
  function updateVersionDisplay() {
    const versionEl = document.getElementById('version-display');
    const badgeEl = document.getElementById('version-badge');
    if (!versionInfo) return;

    if (versionEl) {
      versionEl.textContent = versionInfo.version;
    }
    if (badgeEl) {
      if (versionInfo.has_update && versionInfo.latest_version) {
        badgeEl.title = t('version.hasUpdate', { version: versionInfo.latest_version });
        badgeEl.classList.add('has-update');
      } else {
        badgeEl.title = t('version.checkUpdate');
        badgeEl.classList.remove('has-update');
      }
    }
  }

  // 初始化版本显示
  function initVersionDisplay() {
    fetchVersionInfo().then(() => updateVersionDisplay());
  }

  // GitHub图标
  function iconGitHub() {
    const el = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    el.setAttribute('fill', 'currentColor');
    el.setAttribute('viewBox', '0 0 24 24');
    el.classList.add('w-5', 'h-5');
    el.innerHTML = '<path d="M12 0C5.374 0 0 5.373 0 12c0 5.302 3.438 9.8 8.207 11.387.599.111.793-.261.793-.577v-2.234c-3.338.726-4.033-1.416-4.033-1.416-.546-1.387-1.333-1.756-1.333-1.756-1.089-.745.083-.729.083-.729 1.205.084 1.839 1.237 1.839 1.237 1.07 1.834 2.807 1.304 3.492.997.107-.775.418-1.305.762-1.604-2.665-.305-5.467-1.334-5.467-5.931 0-1.311.469-2.381 1.236-3.221-.124-.303-.535-1.524.117-3.176 0 0 1.008-.322 3.301 1.23A11.509 11.509 0 0112 5.803c1.02.005 2.047.138 3.006.404 2.291-1.552 3.297-1.23 3.297-1.23.653 1.653.242 2.874.118 3.176.77.84 1.235 1.911 1.235 3.221 0 4.609-2.807 5.624-5.479 5.921.43.372.823 1.102.823 2.222v3.293c0 .319.192.694.801.576C20.566 21.797 24 17.3 24 12c0-6.627-5.373-12-12-12z"/>';
    return el;
  }

  // 新版本图标（小圆点）
  function iconNewVersion() {
    const el = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    el.setAttribute('viewBox', '0 0 8 8');
    el.setAttribute('fill', 'var(--success-500)');
    el.style.cssText = 'width: 8px; height: 8px; margin-left: 4px;';
    el.innerHTML = '<circle cx="4" cy="4" r="4"/>';
    return el;
  }

  function buildTopbar(active) {
    const bar = h('header', { class: 'topbar' });
    const left = h('div', { class: 'topbar-left' }, [
      h('a', {
        class: 'brand',
        href: GITHUB_REPO_URL,
        target: '_blank',
        rel: 'noopener noreferrer',
        title: t('nav.githubRepo')
      }, [
        h('img', { class: 'brand-icon', src: '/web/favicon.svg', alt: 'Logo' }),
        h('div', { class: 'brand-text' }, 'Claude Code & Codex Proxy')
      ])
    ]);
    const nav = h('nav', { class: 'topnav' }, [
      ...NAVS.map(n => h('a', {
        class: `topnav-link ${n.key === active ? 'active' : ''}`,
        href: n.href,
        'data-nav-key': n.key
      }, [n.icon(), h('span', { 'data-i18n': n.labelKey }, t(n.labelKey))]))
    ]);
    const loggedIn = isLoggedIn();

    // 版本信息组件（点击跳转到GitHub releases页面）
    const versionBadge = h('a', {
      id: 'version-badge',
      class: 'version-badge',
      href: GITHUB_RELEASES_URL,
      target: '_blank',
      rel: 'noopener noreferrer',
      title: t('version.checkUpdate')
    }, [
      h('span', { id: 'version-display' }, 'v...')
    ]);

    // GitHub链接
    const githubLink = h('a', {
      href: GITHUB_REPO_URL,
      target: '_blank',
      rel: 'noopener noreferrer',
      class: 'github-link',
      title: t('nav.githubRepo')
    }, [iconGitHub()]);

    // 版本+GitHub组合成一个视觉组
    const versionGroup = h('div', { class: 'version-group' }, [versionBadge, githubLink]);

    // 语言切换器
    const langSwitcher = window.i18n ? window.i18n.createLanguageSwitcher() : null;

    const right = h('div', { class: 'topbar-right' }, [
      versionGroup,
      langSwitcher,
      h('button', {
        id: 'auth-btn',
        class: 'btn btn-secondary btn-sm',
        'data-i18n': loggedIn ? 'common.logout' : 'common.login',
        onclick: loggedIn ? onLogout : () => location.href = window.getLoginUrl()
      }, t(loggedIn ? 'common.logout' : 'common.login'))
    ].filter(Boolean));
    bar.appendChild(left); bar.appendChild(nav); bar.appendChild(right);
    return bar;
  }

  async function onLogout() {
    if (!confirm(t('confirm.logout'))) return;

    // 先清理本地Token，避免后续请求触发token检查
    const token = localStorage.getItem('ccload_token');
    localStorage.removeItem('ccload_token');
    localStorage.removeItem('ccload_token_expiry');

    // 如果有token，尝试调用后端登出接口（使用普通fetch，不触发token检查）
    if (token) {
      try {
        await fetch('/logout', {
          method: 'POST',
          headers: { 'Authorization': `Bearer ${token}` }
        });
      } catch (error) {
        console.error('Logout error:', error);
      }
    }

    // 跳转到登录页
    location.href = '/web/login.html';
  }

  let bgAnimElement = null;

  function injectBackground() {
    if (document.querySelector('.bg-anim')) return;
    bgAnimElement = h('div', { class: 'bg-anim' });
    document.body.appendChild(bgAnimElement);
  }

  // 暂停/恢复背景动画（性能优化：减少文件选择器打开时的CPU占用）
  window.pauseBackgroundAnimation = function () {
    if (bgAnimElement) {
      bgAnimElement.style.animationPlayState = 'paused';
    }
  }

  window.resumeBackgroundAnimation = function () {
    if (bgAnimElement) {
      bgAnimElement.style.animationPlayState = 'running';
    }
  }

  window.initTopbar = function initTopbar(activeKey) {
    document.body.classList.add('top-layout');
    const app = document.querySelector('.app-container') || document.body;
    // 隐藏侧边栏与移动按钮
    const sidebar = document.getElementById('sidebar');
    if (sidebar) sidebar.style.display = 'none';
    const mobileBtn = document.getElementById('mobile-menu-btn');
    if (mobileBtn) mobileBtn.style.display = 'none';

    // 插入顶部条
    const topbar = buildTopbar(activeKey);
    document.body.appendChild(topbar);

    // 背景动效
    injectBackground();

    // 初始化版本显示
    initVersionDisplay();
  }

  // 通知系统（全局复用，DRY）
  function ensureNotifyHost() {
    let host = document.getElementById('notify-host');
    if (!host) {
      host = document.createElement('div');
      host.id = 'notify-host';
      host.style.cssText = `position: fixed; top: var(--space-6); right: var(--space-6); display: flex; flex-direction: column; gap: var(--space-2); z-index: 9999; pointer-events: none;`;
      document.body.appendChild(host);
    }
    return host;
  }

  window.ensureNotifyHost = ensureNotifyHost;

  window.showNotification = function (message, type = 'info') {
    const el = document.createElement('div');
    el.className = `notification notification-${type}`;
    el.style.cssText = `
      background: var(--glass-bg);
      backdrop-filter: blur(16px);
      border: 1px solid var(--glass-border);
      border-radius: var(--radius-lg);
      padding: var(--space-4) var(--space-6);
      color: var(--neutral-900);
      font-weight: var(--font-medium);
      opacity: 0;
      transform: translateX(20px);
      transition: all var(--duration-normal) var(--timing-function);
      max-width: 360px;
      box-shadow: 0 10px 25px rgba(0,0,0,0.12);
      overflow: hidden;
      isolation: isolate;
      pointer-events: auto;
    `;
    if (type === 'success') {
      // 高可读：浅底深字
      el.style.background = 'var(--success-50)';
      el.style.color = 'var(--success-600)';
      el.style.borderColor = 'var(--success-500)';
      el.style.boxShadow = '0 6px 28px rgba(16,185,129,0.18)';
    } else if (type === 'error') {
      el.style.background = 'var(--error-50)';
      el.style.color = 'var(--error-600)';
      el.style.borderColor = 'var(--error-500)';
      el.style.boxShadow = '0 6px 28px rgba(239,68,68,0.18)';
    } else if (type === 'warning') {
      el.style.background = 'var(--warning-50)';
      el.style.color = 'var(--warning-700)';
      el.style.borderColor = 'var(--warning-500)';
      el.style.boxShadow = '0 6px 28px rgba(245,158,11,0.18)';
    } else if (type === 'info') {
      el.style.background = 'var(--info-50)';
      el.style.color = 'var(--neutral-800)';
      el.style.borderColor = 'rgba(0,0,0,0.08)';
    }
    el.textContent = message;
    const host = ensureNotifyHost();
    host.appendChild(el);
    requestAnimationFrame(() => { el.style.opacity = '1'; el.style.transform = 'translateX(0)'; });
    setTimeout(() => {
      el.style.opacity = '0'; el.style.transform = 'translateX(20px)';
      setTimeout(() => { if (el.parentNode) el.parentNode.removeChild(el); }, 320);
    }, 3600);
  }
  window.showSuccess = (msg) => window.showNotification(msg, 'success');
  window.showError = (msg) => window.showNotification(msg, 'error');
  window.showWarning = (msg) => window.showNotification(msg, 'warning');
})();

// ============================================================
// 渠道类型管理模块（动态加载配置，单一数据源）
// ============================================================
(function () {
  let channelTypesCache = null;

  // 复用公共工具（DRY）：真实实现由下方公共工具模块导出到 window.escapeHtml
  const escapeHtml = (str) => window.escapeHtml(str);

  /**
   * 获取渠道类型配置（带缓存）
   */
  async function getChannelTypes() {
    if (channelTypesCache) {
      return channelTypesCache;
    }

    const types = await fetchData('/public/channel-types');
    channelTypesCache = types || [];
    return channelTypesCache;
  }

  /**
   * 渲染渠道类型单选按钮组（用于编辑渠道界面）
   * @param {string} containerId - 容器元素ID
   * @param {string} selectedValue - 选中的值（默认'anthropic'）
   */
  async function renderChannelTypeRadios(containerId, selectedValue = 'anthropic') {
    const container = document.getElementById(containerId);
    if (!container) {
      console.error('Container element not found:', containerId);
      return;
    }

    const types = await getChannelTypes();

    container.innerHTML = types.map(type => `
      <label class="channel-editor-radio-option">
        <input type="radio"
               name="channelType"
               value="${escapeHtml(type.value)}"
               ${type.value === selectedValue ? 'checked' : ''}>
        <span title="${escapeHtml(type.description)}">${escapeHtml(type.display_name)}</span>
      </label>
    `).join('');
  }

  /**
   * 渲染渠道类型下拉选择框（用于测试渠道界面）
   * @param {string} selectId - select元素ID
   * @param {string} selectedValue - 选中的值（默认'anthropic'）
   */
  async function renderChannelTypeSelect(selectId, selectedValue = 'anthropic') {
    const select = document.getElementById(selectId);
    if (!select) {
      console.error('select element not found:', selectId);
      return;
    }

    const types = await getChannelTypes();

    select.innerHTML = types.map(type => `
      <option value="${escapeHtml(type.value)}"
              ${type.value === selectedValue ? 'selected' : ''}
              title="${escapeHtml(type.description)}">
        ${escapeHtml(type.display_name)}
      </option>
    `).join('');
  }

  // 导出到全局作用域
  window.ChannelTypeManager = {
    getChannelTypes,
    renderChannelTypeRadios,
    renderChannelTypeSelect
  };
})();

// ============================================================
// 公共工具函数（DRY原则：消除重复代码）
// ============================================================
(function () {
  /**
   * 防抖函数
   * @param {Function} func - 要防抖的函数
   * @param {number} wait - 等待时间(ms)
   * @returns {Function} 防抖后的函数
   */
  function debounce(func, wait) {
    let timeout;
    return function executedFunction(...args) {
      const later = () => {
        clearTimeout(timeout);
        func(...args);
      };
      clearTimeout(timeout);
      timeout = setTimeout(later, wait);
    };
  }

  function bindFilterApplyInputs(options = {}) {
    const apply = typeof options.apply === 'function' ? options.apply : null;
    if (!apply) return;

    const debounceMs = Number.isFinite(options.debounceMs) ? options.debounceMs : 500;
    const debouncedApply = debounce(apply, debounceMs);

    (Array.isArray(options.debounceInputIds) ? options.debounceInputIds : []).forEach((id) => {
      const el = document.getElementById(id);
      if (!el) return;
      el.addEventListener('input', debouncedApply);
    });

    (Array.isArray(options.enterInputIds) ? options.enterInputIds : []).forEach((id) => {
      const el = document.getElementById(id);
      if (!el) return;
      el.addEventListener('keydown', (e) => {
        if (e.key === 'Enter') {
          apply();
        }
      });
    });
  }

  const delegatedActionConfig = {
    click: {
      selector: '[data-action]',
      datasetKey: 'action'
    },
    change: {
      selector: '[data-change-action]',
      datasetKey: 'changeAction'
    },
    input: {
      selector: '[data-input-action]',
      datasetKey: 'inputAction'
    }
  };

  function initDelegatedActions(options = {}) {
    const root = options.root || document;
    const boundElement = options.boundElement || document.body;
    const boundKey = options.boundKey;

    if (!root || !boundElement || !boundElement.dataset || !boundKey) {
      return false;
    }

    if (boundElement.dataset[boundKey]) {
      return false;
    }

    Object.entries(delegatedActionConfig).forEach(([eventType, config]) => {
      const handlers = options[eventType];
      if (!handlers || typeof handlers !== 'object') return;

      root.addEventListener(eventType, (event) => {
        const eventTarget = event.target;
        if (!eventTarget || typeof eventTarget.closest !== 'function') return;

        const actionTarget = eventTarget.closest(config.selector);
        if (!actionTarget) return;

        const actionName = actionTarget.dataset[config.datasetKey];
        const handler = handlers[actionName];
        if (typeof handler === 'function') {
          handler(actionTarget, event);
        }
      });
    });

    boundElement.dataset[boundKey] = '1';
    return true;
  }

  function initPageBootstrap(options = {}) {
    const run = typeof options.run === 'function' ? options.run : () => {};

    const execute = async () => {
      if (options.translate !== false && window.i18n && typeof window.i18n.translatePage === 'function') {
        window.i18n.translatePage();
      }

      if (options.topbarKey && typeof window.initTopbar === 'function') {
        window.initTopbar(options.topbarKey);
      }

      await run();
    };

    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', () => {
        void execute();
      }, { once: true });
      return;
    }

    void execute();
  }

  function getFilterControlConfig(config) {
    if (typeof config === 'string') {
      return { id: config, defaultValue: '', trim: false };
    }
    return {
      id: config && config.id ? config.id : '',
      defaultValue: config && config.defaultValue !== undefined ? config.defaultValue : '',
      trim: Boolean(config && config.trim)
    };
  }

  function readFilterControlValues(fieldMap = {}) {
    const values = {};
    Object.entries(fieldMap).forEach(([key, config]) => {
      const { id, defaultValue, trim } = getFilterControlConfig(config);
      const rawValue = document.getElementById(id)?.value;
      const normalizedValue = typeof rawValue === 'string' && trim ? rawValue.trim() : rawValue;
      values[key] = normalizedValue || defaultValue;
    });
    return values;
  }

  function applyFilterControlValues(values = {}, fieldMap = {}) {
    Object.entries(fieldMap).forEach(([key, config]) => {
      const { id, defaultValue } = getFilterControlConfig(config);
      const el = document.getElementById(id);
      if (!el) return;
      el.value = values[key] || defaultValue;
    });
  }

  function persistFilterState(options = {}) {
    const values = options.values !== undefined
      ? options.values
      : (typeof options.getValues === 'function' ? options.getValues() : {});

    if (!window.FilterState) {
      return values;
    }

    if (options.key) {
      window.FilterState.save(options.key, values);
    }

    if (options.fields) {
      const historyOptions = {
        values,
        fields: options.fields
      };

      ['search', 'pathname', 'preserveExistingParams', 'historyMethod'].forEach((key) => {
        if (options[key] !== undefined) {
          historyOptions[key] = options[key];
        }
      });

      window.FilterState.writeHistory(historyOptions);
    }

    return values;
  }

  function initSavedDateRangeFilter(options = {}) {
    if (typeof window.initDateRangeSelector !== 'function') return null;

    const selectId = options.selectId;
    if (!selectId) return null;

    const defaultValue = typeof options.defaultValue === 'string' && options.defaultValue
      ? options.defaultValue
      : 'today';
    const restoredValue = typeof options.restoredValue === 'string' && options.restoredValue
      ? options.restoredValue
      : defaultValue;
    const onChange = typeof options.onChange === 'function' ? options.onChange : () => {};

    window.initDateRangeSelector(selectId, defaultValue, onChange);

    const el = document.getElementById(selectId);
    if (el) {
      el.value = restoredValue;
    }
    return el;
  }

  async function initAuthTokenFilter(options = {}) {
    if (typeof window.loadAuthTokensIntoSelect !== 'function') return [];

    const selectId = options.selectId;
    if (!selectId) return [];

    const tokens = await window.loadAuthTokensIntoSelect(selectId, options.loadOptions);
    const el = document.getElementById(selectId);
    if (!el) return tokens;

    el.value = options.value || '';
    if (typeof options.onChange === 'function') {
      el.addEventListener('change', options.onChange);
    }

    return tokens;
  }

  function calculateTokenSpeed(outputTokens, durationSeconds, firstByteSeconds) {
    const output = Number(outputTokens);
    const duration = Number(durationSeconds);
    if (!Number.isFinite(output) || output <= 0 || !Number.isFinite(duration) || duration <= 0) {
      return null;
    }

    let tokenDuration = duration;
    const firstByte = Number(firstByteSeconds);
    if (Number.isFinite(firstByte) && firstByte > 0 && firstByte < duration) {
      const generationDuration = duration - firstByte;
      if (generationDuration >= 1) {
        tokenDuration = generationDuration;
      }
    }

    return output / tokenDuration;
  }

  /**
   * 格式化成本（美元）
   * @param {number} cost - 成本值
   * @returns {string} 格式化后的字符串
   */
  function formatCost(cost) {
    const value = Number(cost);
    if (!Number.isFinite(value)) return '';
    if (value === 0) return '$0';
    return '$' + value.toFixed(3);
  }

  /**
   * 格式化标准成本/倍率后成本对
   * 倍率为 1 或两值相等时仅显示标准成本，否则显示 "标准/倍率后"
   * @param {number} standard - 标准成本
   * @param {number|null|undefined} effective - 倍率后成本
   * @returns {string}
   */
  function formatCostPair(standard, effective) {
    const s = Number(standard) || 0;
    const e = (effective === undefined || effective === null) ? s : (Number(effective) || 0);
    if (Math.abs(e - s) < 1e-9) return formatCost(s);
    return formatCost(s) + '/' + formatCost(e);
  }

  /**
   * 格式化倍率文本
   * @param {number} multiplier - 倍率
   * @returns {string}
   */
  function formatCostMultiplier(multiplier) {
    const value = Number(multiplier);
    if (!Number.isFinite(value) || value < 0 || Math.abs(value - 1) < 1e-9) return '';
    // 0 倍率（免费渠道）显示为 "0x"
    return `${Number(value.toFixed(2)).toString()}x`;
  }

  /**
   * 解析标准成本/倍率后成本显示信息
   * @param {number} standard - 标准成本
   * @param {number|null|undefined} effective - 倍率后成本
   * @returns {{standardCost:number,effectiveCost:number,hasMultiplier:boolean,multiplier:number,multiplierText:string}}
   */
  function getCostDisplayInfo(standard, effective) {
    const standardCost = Number(standard) || 0;
    if (!(standardCost > 0)) {
      return {
        standardCost: 0,
        effectiveCost: 0,
        hasMultiplier: false,
        multiplier: 1,
        multiplierText: ''
      };
    }

    const hasExplicitEffectiveCost = effective !== undefined && effective !== null;
    const effectiveValue = hasExplicitEffectiveCost ? (Number(effective) || 0) : standardCost;
    const effectiveCost = hasExplicitEffectiveCost ? effectiveValue : standardCost;
    const hasMultiplier = Math.abs(effectiveCost - standardCost) >= 1e-9;
    const multiplier = hasMultiplier ? (effectiveCost / standardCost) : 1;

    return {
      standardCost,
      effectiveCost,
      hasMultiplier,
      multiplier,
      multiplierText: formatCostMultiplier(multiplier)
    };
  }

  /**
   * 构建两行成本显示HTML
   * @param {number} standard - 标准成本
   * @param {number|null|undefined} effective - 倍率后成本
   * @param {{tone?: 'warning'|'success'}} options - 样式配置
   * @returns {string}
   */
  function buildCostStackHtml(standard, effective, options = {}) {
    const info = getCostDisplayInfo(standard, effective);
    if (!(info.standardCost > 0)) return '';

    const tone = options.tone === 'success' ? 'success' : 'warning';
    const inline = options.inline === true;
    const classes = ['cost-stack', `cost-stack--${tone}`];
    if (info.hasMultiplier) {
      classes.push('cost-stack--with-multiplier');
    }
    if (inline) {
      classes.push('cost-stack--inline');
    }

    if (!info.hasMultiplier) {
      return `<span class="${classes.join(' ')}"><span class="cost-stack-effective">${formatCost(info.effectiveCost)}</span></span>`;
    }

    if (inline) {
      return `<span class="${classes.join(' ')}"><span class="cost-stack-standard">${formatCost(info.standardCost)}</span><span class="cost-stack-effective">${formatCost(info.effectiveCost)}</span></span>`;
    }

    return `<span class="${classes.join(' ')}"><span class="cost-stack-standard">${formatCost(info.standardCost)}</span><span class="cost-stack-effective">${formatCost(info.effectiveCost)}</span></span>`;
  }

  /**
   * 构建单元格右上角倍率角标
   * @param {number} multiplier - 倍率
   * @returns {string}
   */
  function buildCornerMultiplierBadge(multiplier) {
    const text = formatCostMultiplier(multiplier);
    if (!text) return '';
    return `<sup class="cell-multiplier-badge">${text}</sup>`;
  }

  // 格式化数字显示（通用：K/M缩写）
  function formatNumber(num) {
    const n = Number(num);
    if (!Number.isFinite(n)) return '0';
    if (n >= 1000000) return (n / 1000000).toFixed(1) + 'M';
    if (n >= 1000) return (n / 1000).toFixed(1) + 'K';
    return n.toString();
  }

  // RPM 颜色：低流量绿色，中等橙色，高流量红色
  function getRpmColor(rpm) {
    const n = Number(rpm);
    if (!Number.isFinite(n)) return 'var(--neutral-600)';
    if (n < 10) return 'var(--success-600)';
    if (n < 100) return 'var(--warning-600)';
    return 'var(--error-600)';
  }

  /**
   * HTML转义（防XSS）
   * @param {string} str - 需要转义的字符串
   * @returns {string} 转义后的安全字符串
   */
  function escapeHtml(str) {
    if (str == null) return '';
    return String(str)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  // 简单显示/隐藏切换（用于日志/测试响应块等）
  function toggleResponse(elementId) {
    const el = document.getElementById(elementId);
    if (!el) return;
    el.style.display = el.style.display === 'none' ? 'block' : 'none';
  }

  // 导出到全局作用域
  window.debounce = debounce;
  window.bindFilterApplyInputs = bindFilterApplyInputs;
  window.initDelegatedActions = initDelegatedActions;
  window.initPageBootstrap = initPageBootstrap;
  window.readFilterControlValues = readFilterControlValues;
  window.applyFilterControlValues = applyFilterControlValues;
  window.persistFilterState = persistFilterState;
  window.initSavedDateRangeFilter = initSavedDateRangeFilter;
  window.initAuthTokenFilter = initAuthTokenFilter;
  window.calculateTokenSpeed = calculateTokenSpeed;
  window.formatCost = formatCost;
  window.formatCostPair = formatCostPair;
  window.getCostDisplayInfo = getCostDisplayInfo;
  window.buildCostStackHtml = buildCostStackHtml;
  window.buildCornerMultiplierBadge = buildCornerMultiplierBadge;
  window.formatNumber = formatNumber;
  window.getRpmColor = getRpmColor;
  window.escapeHtml = escapeHtml;
  window.toggleResponse = toggleResponse;

  // 页面自动刷新（基于 system_settings.auto_refresh_interval_seconds）
  // 用法：const ar = window.createAutoRefresh({ load: () => loadStats() }); ar.init();
  // 行为：间隔>0 启动 setInterval；tick 时若 document.hidden 或 .modal.show 存在则跳过；
  //       visibilitychange 隐藏时 stop，恢复时立即刷新一次并重启。
  const AUTO_REFRESH_CACHE_KEY = '__autoRefreshIntervalSec';
  const AUTO_REFRESH_CACHE_TTL_MS = 60 * 1000;

  async function fetchAutoRefreshIntervalSec() {
    try {
      const cached = window.sessionStorage?.getItem(AUTO_REFRESH_CACHE_KEY);
      if (cached) {
        const parsed = JSON.parse(cached);
        if (parsed && typeof parsed.value === 'number' && Date.now() - parsed.ts < AUTO_REFRESH_CACHE_TTL_MS) {
          return parsed.value;
        }
      }
    } catch (_) { /* 忽略 sessionStorage 异常 */ }

    let seconds = 0;
    try {
      const fetcher = window.fetchDataWithAuth || window.fetchData;
      if (typeof fetcher !== 'function') return 0;
      const data = await fetcher('/admin/settings');
      if (Array.isArray(data)) {
        const item = data.find(s => s && s.key === 'auto_refresh_interval_seconds');
        const n = item ? Number(item.value) : 0;
        if (Number.isFinite(n) && n > 0) seconds = Math.floor(n);
      }
    } catch (_) { /* 拉取失败：不刷新 */ }

    try {
      window.sessionStorage?.setItem(AUTO_REFRESH_CACHE_KEY, JSON.stringify({ value: seconds, ts: Date.now() }));
    } catch (_) { /* 忽略 */ }
    return seconds;
  }

  function createAutoRefresh(options = {}) {
    const load = typeof options.load === 'function' ? options.load : null;
    if (!load) {
      return { init: async () => {}, stop: () => {} };
    }

    let intervalId = null;
    let intervalMs = 0;
    let visibilityHandler = null;

    function shouldSkip() {
      if (typeof document === 'undefined') return true;
      if (document.hidden) return true;
      if (document.querySelector('.modal.show')) return true;
      return false;
    }

    function tick() {
      if (shouldSkip()) return;
      try {
        const result = load();
        if (result && typeof result.catch === 'function') {
          result.catch(() => { /* 单次失败不影响后续轮询 */ });
        }
      } catch (_) { /* 同步异常吞掉 */ }
    }

    function startTimer() {
      if (intervalId !== null || intervalMs <= 0) return;
      intervalId = setInterval(tick, intervalMs);
    }

    function stopTimer() {
      if (intervalId !== null) {
        clearInterval(intervalId);
        intervalId = null;
      }
    }

    function onVisibilityChange() {
      if (intervalMs <= 0) return;
      if (document.hidden) {
        stopTimer();
      } else {
        tick();
        startTimer();
      }
    }

    async function init() {
      const seconds = await fetchAutoRefreshIntervalSec();
      if (!seconds || seconds <= 0) return;
      intervalMs = seconds * 1000;
      visibilityHandler = onVisibilityChange;
      document.addEventListener('visibilitychange', visibilityHandler);
      startTimer();
    }

    function stop() {
      stopTimer();
      if (visibilityHandler) {
        document.removeEventListener('visibilitychange', visibilityHandler);
        visibilityHandler = null;
      }
      intervalMs = 0;
    }

    return { init, stop };
  }

  window.createAutoRefresh = createAutoRefresh;
})();

// ============================================================
// 通用可搜索下拉选择框组件 (SearchableCombobox)
// ============================================================
(function () {
  /**
   * 创建可搜索下拉选择框
   * @param {Object} config - 配置对象
   * @param {HTMLElement|string} [config.container] - 容器元素或ID（生成模式必需）
   * @param {string} config.inputId - input 元素 ID
   * @param {string} config.dropdownId - 下拉框元素 ID
   * @param {Function} config.getOptions - 获取选项列表的函数，返回 [{value, label}]
   * @param {Function} config.onSelect - 选中回调 (value, label) => void
   * @param {Function} [config.onCancel] - 取消选择回调
   * @param {string} [config.placeholder] - placeholder 文本
   * @param {string} [config.initialValue] - 初始值
   * @param {string} [config.initialLabel] - 初始显示文本
   * @param {number} [config.minWidth] - 最小宽度 (px)
   * @param {boolean} [config.attachMode] - 附着模式，使用已存在的 HTML 元素
   * @param {boolean} [config.allowCustomInput] - 允许提交非下拉选项的自定义输入
   * @param {boolean} [config.commitEmptyAsFirst] - 输入为空回车/失焦时提交第一项（通常为“全部”），覆盖默认的取消/恢复行为
   * @returns {Object} 组件实例
   */
  function createSearchableCombobox(config) {
    const {
      container: containerArg,
      inputId,
      dropdownId,
      getOptions,
      onSelect,
      onCancel,
      placeholder = '',
      initialValue = '',
      initialLabel = '',
      minWidth = 150,
      attachMode = false,
      allowCustomInput = false,
      commitEmptyAsFirst = false
    } = config;

    let input, dropdown, wrapper, dropdownHome, container = null;

    if (attachMode) {
      // 附着模式：使用已存在的 HTML 元素
      input = document.getElementById(inputId);
      dropdown = document.getElementById(dropdownId);
      if (!input || !dropdown) {
        console.error('SearchableCombobox: input or dropdown not found in attach mode');
        return null;
      }
      wrapper = input.closest('.filter-combobox-wrapper');
      dropdownHome = dropdown.parentElement;
      if (initialLabel) input.value = initialLabel;
    } else {
      // 生成模式：创建新的 HTML 结构
      container = typeof containerArg === 'string'
        ? document.getElementById(containerArg)
        : containerArg;

      if (!container) {
        console.error('SearchableCombobox: container not found');
        return null;
      }

      container.innerHTML = `
        <div class="filter-combobox-wrapper" style="min-width: ${minWidth}px;">
          <input
            id="${inputId}"
            class="filter-select filter-combobox"
            type="text"
            autocomplete="off"
            spellcheck="false"
            placeholder="${escapeHtml(placeholder)}"
            value="${escapeHtml(initialLabel)}"
          />
          <div id="${dropdownId}" class="filter-dropdown" role="listbox"></div>
        </div>
      `;

      input = document.getElementById(inputId);
      dropdown = document.getElementById(dropdownId);
      wrapper = input.closest('.filter-combobox-wrapper');
      dropdownHome = dropdown.parentElement;
    }

    let activeIndex = -1;
    let outsideHandler = null;
    let repositionHandler = null;
    let currentValue = initialValue;

    function clearOutsideHandler() {
      if (!outsideHandler) return;
      document.removeEventListener('mousedown', outsideHandler, true);
      outsideHandler = null;
    }

    function clearRepositionHandler() {
      if (!repositionHandler) return;
      window.removeEventListener('resize', repositionHandler, true);
      window.removeEventListener('scroll', repositionHandler, true);
      repositionHandler = null;
    }

    function closeDropdown() {
      dropdown.style.display = 'none';
      dropdown.dataset.open = '0';
      activeIndex = -1;
      clearOutsideHandler();
      clearRepositionHandler();
      if (dropdownHome && dropdown.parentElement !== dropdownHome) {
        dropdownHome.appendChild(dropdown);
      }
    }

    function beginPick() {
      if (input.dataset.pickActive === '1') return;
      input.dataset.pickActive = '1';
      input.dataset.prevInputValue = input.value;
      input.dataset.prevValue = currentValue;
      // 非自定义输入模式始终清空；自定义输入模式下：
      // - 当前值为空（全量态）→ 清空，避免把“所有渠道”这类占位标签当成过滤关键字
      // - 当前值精确命中下拉选项（用户已从下拉选中而非输入自定义词）→ 清空，便于再次浏览全部选项
      // - 其余情况（自定义搜索词）→ 保留以便继续编辑
      let shouldClear = !allowCustomInput;
      if (allowCustomInput) {
        const trimmedCurrent = String(currentValue || '').trim();
        if (!trimmedCurrent) {
          shouldClear = true;
        } else {
          const trimmedLower = trimmedCurrent.toLowerCase();
          const matchesOption = getOptions().some((opt) => {
            const v = String(opt.value || '').trim().toLowerCase();
            const l = String(opt.label || '').trim().toLowerCase();
            return v === trimmedLower || l === trimmedLower;
          });
          if (matchesOption) shouldClear = true;
        }
      }
      if (shouldClear) {
        input.value = '';
      }
      activeIndex = -1;
    }

    function cancelPick() {
      if (input.dataset.pickActive !== '1') {
        closeDropdown();
        return;
      }

      const prevInputValue = input.dataset.prevInputValue ?? '';
      const prevValue = input.dataset.prevValue ?? '';

      input.value = prevInputValue;
      currentValue = prevValue;

      delete input.dataset.pickActive;
      delete input.dataset.prevInputValue;
      delete input.dataset.prevValue;

      closeDropdown();
      if (onCancel) onCancel();
    }

    function commitValue(value, label) {
      currentValue = value;
      input.value = label;

      delete input.dataset.pickActive;
      delete input.dataset.prevInputValue;
      delete input.dataset.prevValue;

      closeDropdown();
      if (onSelect) onSelect(value, label);
    }

    function commitFirstMatchedOrCancel() {
      const keyword = input.value.trim();
      if (!keyword) {
        if (commitEmptyAsFirst) {
          // 空输入回车/失焦时提交第一项（约定为“全部”），无论之前是否有选中值。
          const opts = getOptions();
          if (opts.length > 0) {
            commitValue(opts[0].value, opts[0].label);
            return;
          }
        }
        if (allowCustomInput) {
          // 自定义输入模式下，若打开下拉前已存在选中值（即本次仅是浏览/清空显示），
          // 视为取消并恢复之前的选择；只有从空态主动确认空值时才清除筛选。
          const prevInputValue = String(input.dataset.prevInputValue ?? '').trim();
          const prevValue = String(input.dataset.prevValue ?? '').trim();
          if (prevInputValue || prevValue) {
            cancelPick();
            return;
          }
          commitValue('', '');
          return;
        }
        cancelPick();
        return;
      }
      if (allowCustomInput) {
        const normalizedKeyword = keyword.toLowerCase();
        const exactOption = getOptions().find((opt) => {
          const label = String(opt.label || '').trim().toLowerCase();
          const value = String(opt.value || '').trim().toLowerCase();
          return label === normalizedKeyword || value === normalizedKeyword;
        });
        if (exactOption) {
          commitValue(exactOption.value, exactOption.label);
          return;
        }
        commitValue(keyword, keyword);
        return;
      }
      const items = getDropdownItems();
      if (items.length > 0) {
        commitValue(items[0].value, items[0].label);
        return;
      }
      cancelPick();
    }

    function getDropdownItems() {
      const keyword = input.value.trim().toLowerCase();
      const allOptions = getOptions();
      if (!keyword) return allOptions;
      return allOptions.filter(opt =>
        String(opt.label).toLowerCase().includes(keyword) ||
        String(opt.value).toLowerCase().includes(keyword)
      );
    }

    function renderDropdown() {
      if (dropdown.dataset.open !== '1') return;

      const items = getDropdownItems();
      dropdown.innerHTML = '';

      if (activeIndex >= items.length) activeIndex = items.length - 1;
      if (activeIndex < -1) activeIndex = -1;

      items.forEach((item, idx) => {
        const row = document.createElement('div');
        row.className = 'filter-dropdown-item';
        row.setAttribute('role', 'option');
        row.dataset.value = item.value;
        row.dataset.index = String(idx);
        row.textContent = item.label;

        if (item.value === currentValue) row.classList.add('selected');
        if (idx === activeIndex) row.classList.add('active');

        row.addEventListener('mousedown', (e) => {
          e.preventDefault();
          e.stopPropagation();
          commitValue(item.value, item.label);
        });

        dropdown.appendChild(row);
      });
    }

    function positionDropdown() {
      if (dropdown.dataset.open !== '1') return;
      const rect = input.getBoundingClientRect();
      const margin = 6;

      dropdown.style.left = `${Math.round(rect.left)}px`;
      dropdown.style.width = `${Math.round(rect.width)}px`;
      dropdown.style.top = `${Math.round(rect.bottom + margin)}px`;

      const dropdownHeight = dropdown.offsetHeight || 0;
      const viewportBottom = window.innerHeight || 0;
      if (dropdownHeight && rect.bottom + margin + dropdownHeight > viewportBottom && rect.top - margin - dropdownHeight >= 0) {
        dropdown.style.top = `${Math.round(rect.top - margin - dropdownHeight)}px`;
      }
    }

    function openDropdown() {
      if (dropdownHome && dropdown.parentElement !== document.body) {
        document.body.appendChild(dropdown);
      }
      dropdown.style.display = 'block';
      dropdown.dataset.open = '1';
      renderDropdown();
      positionDropdown();

      clearOutsideHandler();
      outsideHandler = (e) => {
        if (!wrapper.contains(e.target) && !dropdown.contains(e.target)) {
          commitFirstMatchedOrCancel();
        }
      };
      document.addEventListener('mousedown', outsideHandler, true);

      clearRepositionHandler();
      repositionHandler = () => positionDropdown();
      window.addEventListener('resize', repositionHandler, true);
      window.addEventListener('scroll', repositionHandler, true);
    }

    function moveActive(delta) {
      const items = getDropdownItems();
      if (items.length <= 0) return;
      if (activeIndex === -1) {
        activeIndex = 0;
      } else {
        activeIndex = Math.max(0, Math.min(items.length - 1, activeIndex + delta));
      }
      renderDropdown();
    }

    // 事件绑定
    input.addEventListener('mousedown', () => {
      beginPick();
      openDropdown();
    });

    input.addEventListener('input', () => {
      if (dropdown.dataset.open !== '1') {
        beginPick();
        openDropdown();
      }
      activeIndex = -1;
      renderDropdown();
    });

    input.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') {
        if (dropdown.dataset.open === '1') {
          e.preventDefault();
          cancelPick();
        }
        return;
      }

      if (e.key === 'ArrowDown') {
        e.preventDefault();
        if (dropdown.dataset.open !== '1') {
          beginPick();
          openDropdown();
          return;
        }
        moveActive(1);
        return;
      }

      if (e.key === 'ArrowUp') {
        e.preventDefault();
        if (dropdown.dataset.open !== '1') {
          beginPick();
          openDropdown();
          return;
        }
        moveActive(-1);
        return;
      }

      if (e.key === 'Enter') {
        e.preventDefault();
        if (dropdown.dataset.open === '1') {
          const items = getDropdownItems();
          if (activeIndex >= 0 && activeIndex < items.length) {
            commitValue(items[activeIndex].value, items[activeIndex].label);
            return;
          }
          commitFirstMatchedOrCancel();
          return;
        }
        if (input.dataset.pickActive === '1') {
          commitFirstMatchedOrCancel();
        }
      }
    });

    input.addEventListener('blur', () => {
      if (dropdown.dataset.open !== '1') return;
      commitFirstMatchedOrCancel();
    });

    // 返回组件实例，提供外部控制接口
    return {
      getValue: () => currentValue,
      setValue: (value, label) => {
        currentValue = value;
        input.value = label;
      },
      refresh: () => {
        if (dropdown.dataset.open === '1') {
          renderDropdown();
        }
      },
      getInput: () => input,
      getDropdown: () => dropdown,
      destroy: () => {
        closeDropdown();
        clearOutsideHandler();
        clearRepositionHandler();
        if (!attachMode && container) {
          container.innerHTML = '';
        }
      }
    };
  }

  // 导出到全局作用域
  window.createSearchableCombobox = createSearchableCombobox;
})();

// ============================================================
// 跨页面共享工具函数
// ============================================================
(function () {
  /**
   * 复制文本到剪贴板（带降级处理）
   * @param {string} text - 要复制的文本
   * @returns {Promise<void>}
   */
  function fallbackCopyToClipboard(text) {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.left = '-9999px';
    document.body.appendChild(ta);
    ta.select();

    try {
      const copied = typeof document.execCommand === 'function' && document.execCommand('copy');
      if (!copied) {
        throw new Error('copy failed');
      }
    } catch {
      document.body.removeChild(ta);
      return Promise.reject(new Error('copy failed'));
    }

    document.body.removeChild(ta);
    return Promise.resolve();
  }

  function copyToClipboard(text) {
    const clipboard = globalThis.navigator && globalThis.navigator.clipboard;
    if (clipboard && typeof clipboard.writeText === 'function') {
      return clipboard.writeText(text).catch(() => fallbackCopyToClipboard(text));
    }
    return fallbackCopyToClipboard(text);
  }

  function escapeCodeHtml(str) {
    return String(str)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;');
  }

  function wrapHighlightedToken(text, modifier) {
    return `<span class="upstream-token upstream-token--${modifier}">${escapeCodeHtml(text)}</span>`;
  }

  function classifyStatusModifier(statusCode) {
    const code = Number.parseInt(statusCode, 10);
    if (!Number.isFinite(code)) return 'status-unknown';
    if (code >= 200 && code < 300) return 'status-success';
    if (code >= 400 && code < 500) return 'status-client-error';
    if (code >= 500) return 'status-server-error';
    return 'status-neutral';
  }

  function renderJsonLine(line) {
    const tokenRe = /"(?:\\.|[^"\\])*"(?=\s*:)|"(?:\\.|[^"\\])*"|true|false|null|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?/g;
    let out = '';
    let lastIndex = 0;

    for (const match of line.matchAll(tokenRe)) {
      const index = match.index || 0;
      const token = match[0];
      out += escapeCodeHtml(line.slice(lastIndex, index));

      let modifier = 'json-string';
      if (token === 'true' || token === 'false') {
        modifier = 'json-boolean';
      } else if (token === 'null') {
        modifier = 'json-null';
      } else if (token[0] !== '"') {
        modifier = 'json-number';
      } else {
        const nextChar = line.slice(index + token.length).match(/^\s*:/);
        modifier = nextChar ? 'json-key' : 'json-string';
      }

      out += wrapHighlightedToken(token, modifier);
      lastIndex = index + token.length;
    }

    out += escapeCodeHtml(line.slice(lastIndex));
    return out;
  }

  function renderHeaderLine(line) {
    const match = line.match(/^([^:]+)(:\s*)(.*)$/);
    if (!match) return escapeCodeHtml(line);

    const [, key, separator, value] = match;
    return `${wrapHighlightedToken(key, 'header-key')}${escapeCodeHtml(separator)}${value ? wrapHighlightedToken(value, 'header-value') : ''}`;
  }

  function renderRequestLine(line) {
    const requestMatch = line.match(/^(\s*)([A-Z]+)(\s+)(\S.*)$/);
    if (requestMatch && /^[a-z]+:\/\//i.test(requestMatch[4])) {
      const [, indent, method, gap, url] = requestMatch;
      return `${escapeCodeHtml(indent)}${wrapHighlightedToken(method, 'method')}${escapeCodeHtml(gap)}${wrapHighlightedToken(url, 'url')}`;
    }

    const urlMatch = line.match(/^(\s*)([a-z]+:\/\/\S.*)$/i);
    if (urlMatch) {
      const [, indent, url] = urlMatch;
      return `${escapeCodeHtml(indent)}${wrapHighlightedToken(url, 'url')}`;
    }

    return escapeCodeHtml(line);
  }

  function renderStatusLine(line) {
    const responseMatch = line.match(/^(\s*)(HTTP)(\s+)(\d{3})(.*)$/i);
    if (responseMatch) {
      const [, indent, protocol, gap, statusCode, rest] = responseMatch;
      const modifier = classifyStatusModifier(statusCode);
      return `${escapeCodeHtml(indent)}${wrapHighlightedToken(protocol, 'protocol')}${escapeCodeHtml(gap)}${wrapHighlightedToken(statusCode, modifier)}${escapeCodeHtml(rest)}`;
    }

    const statusMatch = line.match(/^(\s*)(\d{3})(.*)$/);
    if (statusMatch) {
      const [, indent, statusCode, rest] = statusMatch;
      const modifier = classifyStatusModifier(statusCode);
      return `${escapeCodeHtml(indent)}${wrapHighlightedToken(statusCode, modifier)}${escapeCodeHtml(rest)}`;
    }

    return escapeCodeHtml(line);
  }

  function looksLikeJSONBlock(text) {
    const trimmed = text.trim();
    if (!trimmed) return false;
    const startsLikeJSON = (trimmed.startsWith('{') && trimmed.endsWith('}'))
      || (trimmed.startsWith('[') && trimmed.endsWith(']'));
    if (!startsLikeJSON) return false;

    try {
      JSON.parse(trimmed);
      return true;
    } catch {
      return false;
    }
  }

  function looksLikeSSE(text) {
    let hits = 0;
    for (const line of text.split('\n')) {
      if (/^(event|data|id|retry):/.test(line) && ++hits >= 2) return true;
    }
    return false;
  }

  function renderSSELine(line) {
    const fieldMatch = line.match(/^(event|data|id|retry)(:)(.*)/);
    if (fieldMatch) {
      const [, field, colon, value] = fieldMatch;
      const renderedField = wrapHighlightedToken(field + colon, 'sse-field');
      if (field === 'event') return renderedField + wrapHighlightedToken(value, 'sse-event-name');
      if (field === 'data' && value.trim()) {
        const trimmed = value.trim();
        const jsonLike = (trimmed[0] === '{' || trimmed[0] === '[');
        if (jsonLike) return renderedField + renderJsonLine(value);
      }
      return renderedField + escapeCodeHtml(value);
    }
    if (line.startsWith(':')) return wrapHighlightedToken(line, 'sse-comment');
    return escapeCodeHtml(line);
  }

  function leadingSpaceCount(line) {
    let i = 0;
    while (i < line.length && (line[i] === ' ' || line[i] === '\t')) i++;
    return i;
  }

  // 基于缩进配对识别可折叠区间。
  // rawLines: 字符串数组（折叠分析针对的"逻辑"行，索引与最终渲染行索引一一对应）
  // 返回 Map<startIndex, { endIndex, count }>，startIndex 指向打开 { 或 [ 的行；
  // endIndex 指向对应的 } 或 ] 行；count 为可折叠行数（不含起止行）。
  function computeFoldRegions(rawLines) {
    const regions = new Map();
    const stack = [];
    for (let i = 0; i < rawLines.length; i++) {
      const raw = rawLines[i] || '';
      const trimmedRight = raw.replace(/[,\s]+$/, '');
      const lastChar = trimmedRight.slice(-1);
      const isOpen = lastChar === '{' || lastChar === '[';
      const trimmedLeft = raw.trimStart();
      const firstChar = trimmedLeft[0];
      const isClose = firstChar === '}' || firstChar === ']';
      const indent = leadingSpaceCount(raw);

      if (isClose && stack.length) {
        // 找到匹配的同缩进 open
        for (let s = stack.length - 1; s >= 0; s--) {
          if (stack[s].indent === indent) {
            const opener = stack[s];
            const span = i - opener.index - 1;
            if (span >= 1) {
              regions.set(opener.index, { endIndex: i, count: span });
            }
            stack.length = s;
            break;
          }
        }
      }
      if (isOpen) {
        stack.push({ index: i, indent });
      }
    }
    return regions;
  }

  let foldIdCounter = 0;
  function nextFoldId() {
    foldIdCounter += 1;
    return `f${foldIdCounter}`;
  }

  function renderCodeLines(lines, foldRegions) {
    if (!foldRegions || foldRegions.size === 0) {
      return lines.map(line => `<span class="code-line">${line || ''}</span>`).join('');
    }
    // 为每个区间生成 id；保留每行的 ancestor region ids 列表（开区间 s < i < e）。
    const startToId = new Map();
    const regionList = []; // {id, start, end, count}
    for (const [startIdx, info] of foldRegions.entries()) {
      const id = nextFoldId();
      startToId.set(startIdx, { id, count: info.count });
      regionList.push({ id, start: startIdx, end: info.endIndex, count: info.count });
    }
    const ancestorIdsAt = (i) => {
      const ids = [];
      for (const r of regionList) {
        if (r.start < i && i < r.end) ids.push(r.id);
      }
      return ids;
    };

    const out = [];
    for (let i = 0; i < lines.length; i++) {
      const content = lines[i] || '';
      const ancestors = ancestorIdsAt(i);
      const regionAttr = ancestors.length ? ` data-fold-region="${ancestors.join(' ')}"` : '';
      const startMeta = startToId.get(i);
      if (startMeta) {
        const { id, count } = startMeta;
        const summary = `<span class="code-fold-summary" data-fold-summary-for="${id}">…${count} lines</span>`;
        const toggle = `<button type="button" class="code-fold-toggle" data-fold-toggle="${id}" aria-expanded="true" aria-label="toggle code fold">▼</button>`;
        out.push(`<span class="code-line code-line--foldable" data-fold-id="${id}"${regionAttr}>${toggle}${content}${summary}</span>`);
        continue;
      }
      out.push(`<span class="code-line"${regionAttr}>${content}</span>`);
    }
    return out.join('');
  }

  function renderUpstreamRequestOrResponse(text, mode) {
    const lines = String(text || '').split('\n');
    if (lines.length === 0) return '';

    const separatorIndex = lines.findIndex(line => line === '');
    const headerEnd = separatorIndex === -1 ? lines.length : separatorIndex;
    const renderedLines = [];
    const rawForFold = []; // 与 renderedLines 同索引，仅用于折叠分析；header 区填空字符串避免参与配对

    renderedLines.push(mode === 'response' ? renderStatusLine(lines[0]) : renderRequestLine(lines[0]));
    rawForFold.push('');

    for (let i = 1; i < headerEnd; i++) {
      renderedLines.push(renderHeaderLine(lines[i]));
      rawForFold.push('');
    }

    if (separatorIndex !== -1) {
      renderedLines.push('');
      rawForFold.push('');
      const bodyLines = lines.slice(separatorIndex + 1);
      const bodyText = bodyLines.join('\n');
      const renderBodyLine = looksLikeJSONBlock(bodyText) ? renderJsonLine
        : looksLikeSSE(bodyText) ? renderSSELine
        : escapeCodeHtml;
      bodyLines.forEach(line => {
        renderedLines.push(renderBodyLine(line));
        rawForFold.push(line);
      });
    }

    return renderCodeLines(renderedLines, computeFoldRegions(rawForFold));
  }

  function renderUpstreamCodeBlock(text, mode = 'text') {
    const value = String(text || '');
    if (!value) return '';

    switch (mode) {
      case 'request':
      case 'response':
        return renderUpstreamRequestOrResponse(value, mode);
      case 'json': {
        const rawLines = value.split('\n');
        return renderCodeLines(rawLines.map(renderJsonLine), computeFoldRegions(rawLines));
      }
      case 'url':
        return renderCodeLines(value.split('\n').map(renderRequestLine));
      case 'status':
        return renderCodeLines(value.split('\n').map(renderStatusLine));
      default:
        return renderCodeLines(value.split('\n').map(escapeCodeHtml));
    }
  }

  function setHighlightedCodeContent(target, text, mode = 'text') {
    const el = typeof target === 'string' ? document.getElementById(target) : target;
    if (!el) return;
    el._rawText = text || '';
    el.innerHTML = renderUpstreamCodeBlock(text || '', mode);
  }

  // 全局折叠按钮事件委托（仅绑定一次）。
  // 任何使用 setHighlightedCodeContent 渲染的 pre 都自动支持折叠。
  if (typeof document !== 'undefined' && typeof document.addEventListener === 'function'
      && !document.__codeFoldDelegated) {
    document.__codeFoldDelegated = true;
    document.addEventListener('click', (e) => {
      const foldBtn = e.target.closest('.code-fold-toggle');
      if (!foldBtn) return;
      e.preventDefault();
      e.stopPropagation();
      const id = foldBtn.dataset.foldToggle;
      if (!id) return;
      const startLine = foldBtn.closest('.code-line--foldable');
      if (!startLine) return;
      const collapsed = startLine.classList.toggle('code-line--collapsed');
      foldBtn.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
      foldBtn.textContent = collapsed ? '▶' : '▼';
      const pre = startLine.closest('pre');
      const root = pre || document;
      root.querySelectorAll(`[data-fold-region~="${id}"]`).forEach(el => {
        el.classList.toggle('code-line--hidden', collapsed);
      });
    });
  }

  /**
   * 初始化渠道类型筛选下拉框
   * @param {string} selectId - select 元素 ID
   * @param {string} initialType - 初始选中的类型
   * @param {function(string)} onChange - 选中值变更回调
   */
  async function initChannelTypeFilter(selectId, initialType, onChange) {
    const select = document.getElementById(selectId);
    if (!select) return;

    const types = await window.ChannelTypeManager.getChannelTypes();
    select.innerHTML = `<option value="all">${window.t('common.all')}</option>`;
    types.forEach(type => {
      const option = document.createElement('option');
      option.value = type.value;
      option.textContent = type.display_name;
      if (type.value === initialType) option.selected = true;
      select.appendChild(option);
    });

    select.addEventListener('change', (e) => onChange(e.target.value));
  }

  /**
   * 加载令牌列表并填充下拉框
   * @param {string} selectId - select 元素 ID
   * @param {Object} [opts] - 选项
   * @param {string} [opts.tokenPrefix] - 令牌显示前缀（默认 'Token #'）
   * @param {string} [opts.restoreValue] - 恢复选中值
   * @returns {Promise<Array>} 令牌数组
   */
  async function loadAuthTokensIntoSelect(selectId, opts) {
    const o = opts || {};
    try {
      const data = await fetchDataWithAuth('/admin/auth-tokens');
      const tokens = (data && data.tokens) || [];

      const select = document.getElementById(selectId);
      if (select && tokens.length > 0) {
        select.innerHTML = `<option value="">${window.t('stats.allTokens')}</option>`;
        tokens.forEach(token => {
          const option = document.createElement('option');
          option.value = token.id;
          option.textContent = token.description || `${o.tokenPrefix || 'Token #'}${token.id}`;
          select.appendChild(option);
        });
        if (o.restoreValue) select.value = o.restoreValue;
      }
      return tokens;
    } catch (error) {
      console.error('Failed to load auth tokens:', error);
      return [];
    }
  }

  /**
   * 初始化时间范围按钮选择器
   * @param {function(string)} onRangeChange - 范围变更回调，参数为 range 值
   */
  function initTimeRangeSelector(onRangeChange) {
    const buttons = document.querySelectorAll('.time-range-btn');
    buttons.forEach(btn => {
      if (typeof btn.__timeRangeClickHandler === 'function') {
        btn.removeEventListener('click', btn.__timeRangeClickHandler);
      }

      const handleClick = function () {
        buttons.forEach(b => b.classList.remove('active'));
        this.classList.add('active');
        onRangeChange(this.dataset.range);
      };

      btn.__timeRangeClickHandler = handleClick;
      btn.addEventListener('click', handleClick);
    });
  }

  // 渲染日期按钮 + 绑定切换回调 + 监听 i18n 重渲染
  function bindTimeRangeSelector(options = {}) {
    const { containerId, values, includeAll = false, initialValue, onChange } = options;
    let currentValue = initialValue;

    const render = () => {
      if (typeof window.renderDateRangeButtons !== 'function') return;
      const cfg = { values, activeValue: currentValue };
      if (includeAll) cfg.includeAll = true;
      window.renderDateRangeButtons(containerId, cfg);
    };

    const bind = () => {
      initTimeRangeSelector(range => {
        currentValue = range;
        if (typeof onChange === 'function') onChange(range);
      });
    };

    render();
    bind();

    if (window.i18n && typeof window.i18n.onLocaleChange === 'function') {
      window.i18n.onLocaleChange(() => {
        render();
        bind();
      });
    }
  }

  const SENSITIVE_HEADER_RE = /^(authorization|x-api-key|api-key|x-goog-api-key|proxy-authorization)$/i;

  function maskHeaderValue(v) {
    if (typeof v !== 'string' || v.length <= 8) return '******';
    return v.slice(0, 4) + '******' + v.slice(-4);
  }

  function maskSensitiveHeaders(headers) {
    if (!headers || typeof headers !== 'object') return headers;
    const out = {};
    for (const [key, value] of Object.entries(headers)) {
      if (SENSITIVE_HEADER_RE.test(key)) {
        out[key] = Array.isArray(value) ? value.map(maskHeaderValue) : maskHeaderValue(value);
      } else {
        out[key] = value;
      }
    }
    return out;
  }

  window.maskSensitiveHeaders = maskSensitiveHeaders;
  window.copyToClipboard = copyToClipboard;
  window.renderUpstreamCodeBlock = renderUpstreamCodeBlock;
  window.setHighlightedCodeContent = setHighlightedCodeContent;
  window.initChannelTypeFilter = initChannelTypeFilter;
  window.loadAuthTokensIntoSelect = loadAuthTokensIntoSelect;
  window.initTimeRangeSelector = initTimeRangeSelector;
  window.bindTimeRangeSelector = bindTimeRangeSelector;
})();
