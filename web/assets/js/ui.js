// ============================================================
// Token认证工具（统一API调用，替代Cookie Session）
// ============================================================
(function() {
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
(function() {
  async function parseAPIResponse(res) {
    const text = await res.text();
    if (!text) {
      throw new Error(`空响应 (HTTP ${res.status})`);
    }

    let payload;
    try {
      payload = JSON.parse(text);
    } catch (e) {
      throw new Error(`响应不是JSON (HTTP ${res.status})`);
    }

    if (!payload || typeof payload !== 'object' || typeof payload.success !== 'boolean') {
      throw new Error(`响应格式不符合APIResponse (HTTP ${res.status})`);
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
    if (!resp.success) throw new Error(resp.error || '请求失败');
    return resp.data;
  }

  async function fetchDataWithAuth(url, options = {}) {
    const resp = await fetchAPIWithAuth(url, options);
    if (!resp.success) throw new Error(resp.error || '请求失败');
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
    { key: 'index', label: '概览', href: '/web/index.html', icon: iconHome },
    { key: 'channels', label: '渠道管理', href: '/web/channels.html', icon: iconSettings },
    { key: 'tokens', label: 'API令牌', href: '/web/tokens.html', icon: iconKey },
    { key: 'stats', label: '调用统计', href: '/web/stats.html', icon: iconBars },
    { key: 'trend', label: '请求趋势', href: '/web/trend.html', icon: iconTrend },
    { key: 'logs', label: '日志', href: '/web/logs.html', icon: iconAlert },
    { key: 'model-test', label: '模型测试', href: '/web/model-test.html', icon: iconTest },
    { key: 'settings', label: '设置', href: '/web/settings.html', icon: iconCog },
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
  function iconCog() {
    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M10.325 4.317c.426-1.756 2.924-1.756 3.35 0a1.724 1.724 0 002.573 1.066c1.543-.94 3.31.826 2.37 2.37a1.724 1.724 0 001.065 2.572c1.756.426 1.756 2.924 0 3.35a1.724 1.724 0 00-1.066 2.573c.94 1.543-.826 3.31-2.37 2.37a1.724 1.724 0 00-2.572 1.065c-.426 1.756-2.924 1.756-3.35 0a1.724 1.724 0 00-2.573-1.066c-1.543.94-3.31-.826-2.37-2.37a1.724 1.724 0 00-1.065-2.572c-1.756-.426-1.756-2.924 0-3.35a1.724 1.724 0 001.066-2.573c-.94-1.543.826-3.31 2.37-2.37.996.608 2.296.07 2.572-1.065z"/><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 12a3 3 0 11-6 0 3 3 0 016 0z"/>`);
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
        badgeEl.title = `点击查看新版本: ${versionInfo.latest_version}`;
        badgeEl.classList.add('has-update');
      } else {
        badgeEl.title = '点击查看发布页面';
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
        title: 'GitHub仓库'
      }, [
        h('img', { class: 'brand-icon', src: '/web/favicon.svg', alt: 'Logo' }),
        h('div', { class: 'brand-text' }, 'Claude Code & Codex Proxy')
      ])
    ]);
    const nav = h('nav', { class: 'topnav' }, [
      ...NAVS.map(n => h('a', {
        class: `topnav-link ${n.key === active ? 'active' : ''}`,
        href: n.href
      }, [n.icon(), h('span', {}, n.label)]) )
    ]);
    const loggedIn = isLoggedIn();

    // 版本信息组件（点击跳转到GitHub releases页面）
    const versionBadge = h('a', {
      id: 'version-badge',
      class: 'version-badge',
      href: GITHUB_RELEASES_URL,
      target: '_blank',
      rel: 'noopener noreferrer',
      title: '点击查看发布页面'
    }, [
      h('span', { id: 'version-display' }, 'v...')
    ]);

    // GitHub链接
    const githubLink = h('a', {
      href: GITHUB_REPO_URL,
      target: '_blank',
      rel: 'noopener noreferrer',
      class: 'github-link',
      title: 'GitHub仓库'
    }, [iconGitHub()]);

    // 版本+GitHub组合成一个视觉组
    const versionGroup = h('div', { class: 'version-group' }, [versionBadge, githubLink]);

    const right = h('div', { class: 'topbar-right' }, [
      versionGroup,
      h('button', {
        class: 'btn btn-secondary btn-sm',
        onclick: loggedIn ? onLogout : () => location.href = window.getLoginUrl()
      }, loggedIn ? '注销' : '登录')
    ]);
    bar.appendChild(left); bar.appendChild(nav); bar.appendChild(right);
    return bar;
  }

  async function onLogout() {
    if (!confirm('确定要注销吗？')) return;

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
  window.pauseBackgroundAnimation = function() {
    if (bgAnimElement) {
      bgAnimElement.style.animationPlayState = 'paused';
    }
  }

  window.resumeBackgroundAnimation = function() {
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

  window.showNotification = function(message, type = 'info') {
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
})();

// ============================================================
// 渠道类型管理模块（动态加载配置，单一数据源）
// ============================================================
(function() {
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
      console.error('容器元素不存在:', containerId);
      return;
    }

    const types = await getChannelTypes();

    container.innerHTML = types.map(type => `
      <label style="margin-right: 15px; cursor: pointer; display: inline-flex; align-items: center;">
        <input type="radio"
               name="channelType"
               value="${escapeHtml(type.value)}"
               ${type.value === selectedValue ? 'checked' : ''}
               style="margin-right: 5px;">
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
      console.error('select元素不存在:', selectId);
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

  /**
   * 渲染可搜索的渠道类型选择框
   * @param {string} containerId - 容器元素ID
   * @param {string} selectedValue - 选中的值（默认'anthropic'）
   */
  async function renderSearchableChannelTypeSelect(containerId, selectedValue = 'anthropic') {
    const container = document.getElementById(containerId);
    if (!container) {
      console.error('容器元素不存在:', containerId);
      return;
    }

    const types = await getChannelTypes();
    const selectedType = types.find(t => t.value === selectedValue) || types[0];

    // 创建可搜索下拉框结构
    container.innerHTML = `
      <div class="searchable-select" style="position: relative; width: 150px;">
        <input type="text" class="searchable-select-input"
               value="${escapeHtml(selectedType?.display_name || '')}"
               data-value="${escapeHtml(selectedType?.value || '')}"
               placeholder="搜索类型..."
               autocomplete="off"
               style="width: 100%; padding: 6px 8px; border: 1px solid var(--color-border); border-radius: 6px; background: var(--color-bg-secondary); color: var(--color-text); font-size: 13px;">
        <div class="searchable-select-dropdown" style="display: none; position: absolute; top: 100%; left: 0; right: 0; max-height: 200px; overflow-y: auto; background: #fff; border: 1px solid var(--color-border); border-radius: 6px; margin-top: 2px; z-index: 100; box-shadow: 0 4px 12px rgba(0,0,0,0.15);"></div>
      </div>
    `;

    const input = container.querySelector('.searchable-select-input');
    const dropdown = container.querySelector('.searchable-select-dropdown');

    // 渲染下拉选项
    function renderOptions(filter = '') {
      const filterLower = filter.toLowerCase();
      const filtered = types.filter(t =>
        t.display_name.toLowerCase().includes(filterLower) ||
        t.value.toLowerCase().includes(filterLower)
      );

      dropdown.innerHTML = filtered.map(type => `
        <div class="searchable-select-option"
             data-value="${escapeHtml(type.value)}"
             data-display="${escapeHtml(type.display_name)}"
             title="${escapeHtml(type.description)}"
             style="padding: 8px 10px; cursor: pointer; font-size: 13px; ${type.value === input.dataset.value ? 'background: var(--color-bg-tertiary);' : ''}">
          ${escapeHtml(type.display_name)}
        </div>
      `).join('');

      // 绑定选项点击事件
      dropdown.querySelectorAll('.searchable-select-option').forEach(opt => {
        opt.addEventListener('click', () => {
          input.value = opt.dataset.display;
          input.dataset.value = opt.dataset.value;
          dropdown.style.display = 'none';
        });
        opt.addEventListener('mouseenter', () => {
          opt.style.background = 'var(--color-bg-tertiary)';
        });
        opt.addEventListener('mouseleave', () => {
          opt.style.background = opt.dataset.value === input.dataset.value ? 'var(--color-bg-tertiary)' : '';
        });
      });
    }

    // 输入框事件
    input.addEventListener('focus', () => {
      renderOptions(input.value);
      dropdown.style.display = 'block';
    });

    input.addEventListener('input', () => {
      renderOptions(input.value);
      dropdown.style.display = 'block';
    });

    // 点击外部关闭
    document.addEventListener('click', (e) => {
      if (!container.contains(e.target)) {
        dropdown.style.display = 'none';
        // 恢复显示已选择的值
        const selected = types.find(t => t.value === input.dataset.value);
        if (selected) input.value = selected.display_name;
      }
    });
  }

  /**
   * 获取可搜索选择框的当前值
   * @param {string} containerId - 容器元素ID
   * @returns {string} 当前选中的值
   */
  function getSearchableSelectValue(containerId) {
    const container = document.getElementById(containerId);
    const input = container?.querySelector('.searchable-select-input');
    return input?.dataset.value || '';
  }

  /**
   * 渲染渠道类型Tab页（包含"全部"选项）
   * @param {string} containerId - 容器元素ID
   * @param {Function} onTabChange - tab切换回调函数
   * @param {string} initialType - 初始选中的类型（可选，默认选中第一个）
   */
  async function renderChannelTypeTabs(containerId, onTabChange, initialType = null) {
    const container = document.getElementById(containerId);
    if (!container) {
      console.error('容器元素不存在:', containerId);
      return;
    }

    const types = await getChannelTypes();

    // 添加"全部"选项到末尾
    const allTab = { value: 'all', display_name: '全部' };
    const allTypes = [...types, allTab];

    // 确定初始选中的类型
    const activeType = initialType || (types.length > 0 ? types[0].value : 'all');

    container.innerHTML = allTypes.map((type) => `
      <button class="channel-tab ${type.value === activeType ? 'active' : ''}"
              data-type="${escapeHtml(type.value)}"
              title="${escapeHtml(type.description || '显示所有渠道类型')}">
        ${escapeHtml(type.display_name)}
      </button>
    `).join('');

    // 绑定点击事件
    container.querySelectorAll('.channel-tab').forEach(tab => {
      tab.addEventListener('click', () => {
        const type = tab.dataset.type;
        container.querySelectorAll('.channel-tab').forEach(t => t.classList.remove('active'));
        tab.classList.add('active');
        if (onTabChange) onTabChange(type);
      });
    });
  }

  // 导出到全局作用域
  window.ChannelTypeManager = {
    getChannelTypes,
    renderChannelTypeRadios,
    renderChannelTypeSelect,
    renderSearchableChannelTypeSelect,
    getSearchableSelectValue,
    renderChannelTypeTabs
  };
})();

// ============================================================
// 公共工具函数（DRY原则：消除重复代码）
// ============================================================
(function() {
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

  /**
   * 格式化成本（美元）
   * @param {number} cost - 成本值
   * @returns {string} 格式化后的字符串
   */
  function formatCost(cost) {
    if (cost === 0) return '$0.00';
    if (cost < 0.001) {
      if (cost < 0.000001) {
        return '$' + cost.toExponential(2);
      }
      return '$' + cost.toFixed(6).replace(/\.0+$/, '');
    }
    if (cost >= 1.0) {
      return '$' + cost.toFixed(2);
    }
    return '$' + cost.toFixed(4).replace(/\.0+$/, '');
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
  window.formatCost = formatCost;
  window.formatNumber = formatNumber;
  window.getRpmColor = getRpmColor;
  window.escapeHtml = escapeHtml;
  window.toggleResponse = toggleResponse;
})();
