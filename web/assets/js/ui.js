// ============================================================
// Token认证工具（统一API调用，替代Cookie Session）
// ============================================================
(function() {
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
      window.location.href = '/web/login.html';
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
      window.location.href = '/web/login.html';
      throw new Error('Unauthorized');
    }

    return response;
  }

  // 导出到全局作用域
  window.fetchWithAuth = fetchWithAuth;
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

  function buildTopbar(active) {
    const bar = h('header', { class: 'topbar' });
    const left = h('div', { class: 'topbar-left' }, [
      h('div', { class: 'brand' }, [
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
    const right = h('div', { class: 'topbar-right' }, [
      h('button', {
        class: 'btn btn-secondary btn-sm',
        onclick: loggedIn ? onLogout : () => location.href = '/web/login.html'
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

  /**
   * 获取渠道类型配置（带缓存）
   */
  async function getChannelTypes() {
    if (channelTypesCache) {
      return channelTypesCache;
    }

    const res = await fetch('/public/channel-types');
    if (!res.ok) {
      throw new Error(`获取渠道类型配置失败: ${res.status}`);
    }
    const data = await res.json();
    channelTypesCache = data.data || [];
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
               value="${type.value}"
               ${type.value === selectedValue ? 'checked' : ''}
               style="margin-right: 5px;">
        <span title="${type.description}">${type.display_name}</span>
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
      <option value="${type.value}"
              ${type.value === selectedValue ? 'selected' : ''}
              title="${type.description}">
        ${type.display_name}
      </option>
    `).join('');
  }

  /**
   * 获取渠道类型的显示名称
   * @param {string} value - 渠道类型内部值
   * @returns {Promise<string>} 显示名称
   */
  async function getChannelTypeDisplayName(value) {
    const types = await getChannelTypes();
    const type = types.find(t => t.value === value);
    return type ? type.display_name : value;
  }

  /**
   * 渲染渠道类型过滤器下拉框（包含"所有类型"选项）
   * @param {string} selectId - select元素ID
   */
  async function renderChannelTypeFilter(selectId) {
    const select = document.getElementById(selectId);
    if (!select) {
      console.error('select元素不存在:', selectId);
      return;
    }

    const types = await getChannelTypes();

    select.innerHTML = '<option value="all">所有类型</option>' +
      types.map(type => `
        <option value="${type.value}" title="${type.description}">
          ${type.display_name}
        </option>
      `).join('');
  }

  /**
   * 渲染渠道类型Tab页（包含"全部"选项）
   * @param {string} containerId - 容器元素ID
   * @param {Function} onTabChange - tab切换回调函数
   */
  async function renderChannelTypeTabs(containerId, onTabChange) {
    const container = document.getElementById(containerId);
    if (!container) {
      console.error('容器元素不存在:', containerId);
      return;
    }

    const types = await getChannelTypes();

    // 添加"全部"选项到末尾
    const allTab = { value: 'all', display_name: '全部' };
    const allTypes = [...types, allTab];

    container.innerHTML = allTypes.map((type, index) => `
      <button class="channel-tab ${index === 0 ? 'active' : ''}"
              data-type="${type.value}"
              title="${type.description || '显示所有渠道类型'}">
        ${type.display_name}
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
    renderChannelTypeFilter,
    renderChannelTypeTabs,
    getChannelTypeDisplayName
  };
})();
