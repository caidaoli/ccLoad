    // 统计数据管理
    let statsData = { by_client_protocol: {}, by_auth_type: {} };

    // 当前选中的时间范围
    let currentTimeRange = 'today';
    let currentCustomTimeRange = null;
    let serviceHealthModel = null;
    let dashboardLoadGeneration = 0;

    const AUTH_TYPE_CARD_CONFIG = Object.freeze({
      api_key: {
        labelKey: 'index.channels.api',
        iconClass: 'api',
        icon: '<path d="M15 7a5 5 0 1 0-9.9 1H3v4h2v2h2v2h3v-3.1A5 5 0 0 0 15 7Zm-5 0a1.5 1.5 0 1 1-3 0 1.5 1.5 0 0 1 3 0Z"/>'
      },
      codex_oauth: {
        labelKey: 'index.channels.codex',
        iconClass: 'codex',
        filled: true,
        icon: '<path d="M22.2819 9.8211a5.9847 5.9847 0 0 0-.5157-4.9108 6.0462 6.0462 0 0 0-6.5098-2.9A6.0651 6.0651 0 0 0 4.9807 4.1818a5.9847 5.9847 0 0 0-3.9977 2.9 6.0462 6.0462 0 0 0 .7427 7.0966 5.98 5.98 0 0 0 .511 4.9107 6.051 6.051 0 0 0 6.5146 2.9001A5.9847 5.9847 0 0 0 13.2599 24a6.0557 6.0557 0 0 0 5.7718-4.2058 5.9894 5.9894 0 0 0 3.9977-2.9001 6.0557 6.0557 0 0 0-.7475-7.0729zm-9.022 12.6081a4.4755 4.4755 0 0 1-2.8764-1.0408l.1419-.0804 4.7783-2.7582a.7948.7948 0 0 0 .3927-.6813v-6.7369l2.02 1.1686a.071.071 0 0 1 .038.052v5.5826a4.504 4.504 0 0 1-4.4945 4.4944zm-9.6607-4.1254a4.4708 4.4708 0 0 1-.5346-3.0137l.142.0852 4.783 2.7582a.7712.7712 0 0 0 .7806 0l5.8428-3.3685v2.3324a.0804.0804 0 0 1-.0332.0615L9.74 19.9502a4.4992 4.4992 0 0 1-6.1408-1.6464zM2.3408 7.8956a4.485 4.485 0 0 1 2.3655-1.9728V11.6a.7664.7664 0 0 0 .3879.6765l5.8144 3.3543-2.0201 1.1685a.0757.0757 0 0 1-.071 0l-4.8303-2.7865A4.504 4.504 0 0 1 2.3408 7.872zm16.5963 3.8558L13.1038 8.364 15.1192 7.2a.0757.0757 0 0 1 .071 0l4.8303 2.7913a4.4944 4.4944 0 0 1-.6765 8.1042v-5.6772a.79.79 0 0 0-.407-.667zm2.0107-3.0231l-.142-.0852-4.7735-2.7818a.7759.7759 0 0 0-.7854 0L9.409 9.2297V6.8974a.0662.0662 0 0 1 .0284-.0615l4.8303-2.7866a4.4992 4.4992 0 0 1 6.6802 4.66zM8.3065 12.863l-2.02-1.1638a.0804.0804 0 0 1-.038-.0567V6.0742a4.4992 4.4992 0 0 1 7.3757-3.4537l-.142.0805L8.704 5.459a.7948.7948 0 0 0-.3927.6813zm1.0976-2.3654l2.602-1.4998 2.6069 1.4998v2.9994l-2.5974 1.4997-2.6067-1.4997Z"/>'
      },
      antigravity_oauth: {
        labelKey: 'index.channels.antigravity',
        iconClass: 'antigravity',
        viewBox: '0 0 111 113',
        filled: true,
        icon: '<path d="M89.6992 93.695C94.3659 97.195 101.366 94.8617 94.9492 88.445 75.6992 69.7783 79.7825 18.445 55.8659 18.445S36.0325 69.7783 16.7825 88.445C9.78251 95.445 17.3658 97.195 22.0325 93.695 40.1159 81.445 38.9492 59.8617 55.8659 59.8617S71.6159 81.445 89.6992 93.695Z"/>'
      },
      xai_oauth: {
        labelKey: 'index.channels.xai',
        iconClass: 'xai',
        filled: true,
        icon: '<text x="12" y="15.5" text-anchor="middle" font-family="Arial, sans-serif" font-size="10.5" font-weight="700">xAI</text>'
      },
      anthropic_oauth: {
        labelKey: 'index.channels.anthropic',
        iconClass: 'anthropic',
        filled: true,
        icon: '<path d="M17.3041 3.541h-3.6718l6.696 16.918H24Zm-10.6082 0L0 20.459h3.7442l1.3693-3.5527h7.0052l1.3693 3.5528h3.7442L10.5363 3.5409Zm-.3712 10.2232 2.2914-5.9456 2.2914 5.9456Z"/>'
      },
      zai_oauth: {
        labelKey: 'index.channels.zai',
        iconClass: 'zai',
        viewBox: '0 0 30 30',
        filled: true,
        icon: '<path d="M15.47 7.1 14.17 8.95c-.2.29-.54.47-.9.47h-7.1V7.09h9.31Zm8.83 0L13.14 22.91H5.7L16.86 7.1h7.44Zm-9.77 15.81 1.31-1.86c.2-.29.54-.47.9-.47h7.09v2.33h-9.3Z"/>'
      },
      cursor_oauth: {
        labelKey: 'index.channels.cursor',
        iconClass: 'cursor',
        filled: true,
        icon: '<path d="M11.503.131 1.891 5.678a.84.84 0 0 0-.42.726v11.188c0 .3.162.575.42.724l9.609 5.55a1 1 0 0 0 .998 0l9.61-5.55a.84.84 0 0 0 .42-.724V6.404a.84.84 0 0 0-.42-.726L12.497.131a1.01 1.01 0 0 0-.996 0M2.657 6.338h18.55c.263 0 .43.287.297.515L12.23 22.918c-.062.107-.229.064-.229-.06V12.335a.59.59 0 0 0-.295-.51l-9.11-5.257c-.109-.063-.064-.23.061-.23"/>'
      },
      zed_oauth: {
        labelKey: 'index.channels.zed',
        iconClass: 'zed',
        filled: true,
        icon: '<path d="M2.25 1.5a.75.75 0 0 0-.75.75v16.5H0V2.25A2.25 2.25 0 0 1 2.25 0h20.095c1.002 0 1.504 1.212.795 1.92L10.764 14.298h3.486V12.75h1.5v1.922a1.125 1.125 0 0 1-1.125 1.125H9.264l-2.578 2.578h11.689V9h1.5v9.375a1.5 1.5 0 0 1-1.5 1.5H5.185L2.562 22.5H21.75a.75.75 0 0 0 .75-.75V5.25H24v16.5A2.25 2.25 0 0 1 21.75 24H1.655C.653 24 .151 22.788.86 22.08L13.19 9.75H9.75v1.5h-1.5V9.375A1.125 1.125 0 0 1 9.375 8.25h5.314l2.625-2.625H5.625V15h-1.5V5.625a1.5 1.5 0 0 1 1.5-1.5h13.19L21.438 1.5Z"/>'
      }
    });

    const AUTH_TYPE_CARD_ORDER = Object.keys(AUTH_TYPE_CARD_CONFIG);

    function createOverviewElement(tag, className, text) {
      const element = document.createElement(tag);
      if (className) element.className = className;
      if (text !== undefined) element.textContent = text;
      return element;
    }

    function translatedAuthTypeLabel(authType, config) {
      if (config && config.labelKey && typeof window.t === 'function') {
        const translated = window.t(config.labelKey);
        if (translated && translated !== config.labelKey) return translated;
      }
      return authType;
    }

    function createAuthTypeCard(authType) {
      const config = AUTH_TYPE_CARD_CONFIG[authType] || {
        labelKey: '',
        iconClass: 'api',
        icon: '<path d="M5 12h14M12 5v14"/>'
      };
      const card = createOverviewElement('div', 'channel-card');
      card.id = `type-${authType}-card`;

      const header = createOverviewElement('div', 'channel-card-header');
      const title = createOverviewElement('div', 'channel-card-title');
      const icon = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
      icon.setAttribute('width', '16');
      icon.setAttribute('height', '16');
      icon.setAttribute('viewBox', config.viewBox || '0 0 24 24');
      icon.setAttribute('fill', config.filled ? 'currentColor' : 'none');
      icon.setAttribute('stroke', config.filled ? 'none' : 'currentColor');
      icon.setAttribute('stroke-width', '2.2');
      icon.setAttribute('stroke-linecap', 'round');
      icon.setAttribute('stroke-linejoin', 'round');
      icon.setAttribute('aria-hidden', 'true');
      icon.setAttribute('focusable', 'false');
      icon.innerHTML = config.icon;
      const iconContainer = createOverviewElement('div', `channel-icon channel-icon--${config.iconClass}`);
      iconContainer.appendChild(icon);
      title.append(iconContainer, createOverviewElement('span', '', translatedAuthTypeLabel(authType, config)));

      const cost = createOverviewElement('div', 'channel-cost');
      cost.append(
        createOverviewElement('span', 'cost-label', typeof window.t === 'function' ? window.t('common.cost') : '成本'),
        createOverviewElement('span', 'cost-value')
      );
      cost.lastElementChild.id = `type-${authType}-cost`;
      header.append(title, cost);
      card.appendChild(header);

      const metrics = createOverviewElement('div', 'channel-metrics');
      [
        ['requests', 'index.metrics.totalRequests', '总请求', 'metric-total'],
        ['success', 'index.metrics.success', '成功', 'metric-success'],
        ['error', 'index.metrics.failed', '失败', 'metric-error'],
        ['rate', 'index.metrics.successRate', '成功率', 'metric-rate']
      ].forEach(([name, labelKey, fallback, valueClass]) => {
        const item = createOverviewElement('div', 'metric-item');
        const value = createOverviewElement('div', `metric-value ${valueClass}`, name === 'rate' ? '0.0%' : '0');
        value.id = `type-${authType}-${name}`;
        const label = createOverviewElement('div', 'metric-label', typeof window.t === 'function' ? window.t(labelKey) : fallback);
        label.dataset.i18n = labelKey;
        item.append(value, label);
        metrics.appendChild(item);
      });
      card.appendChild(metrics);

      const tokens = createOverviewElement('div', 'token-stats');
      [
        ['input', 'common.input', '输入', false],
        ['output', 'common.output', '输出', false],
        ['cache-read', 'common.cacheRead', '缓存读', true],
        ['cache-create', 'common.cacheCreate', '缓存创', true]
      ].forEach(([name, labelKey, fallback, isCache]) => {
        const item = createOverviewElement('div', 'token-item');
        const label = createOverviewElement('span', 'token-label', typeof window.t === 'function' ? window.t(labelKey) : fallback);
        label.dataset.i18n = labelKey;
        const value = createOverviewElement('span', `token-value${isCache ? ' token-cache' : ''}`, '0');
        value.id = `type-${authType}-${name}`;
        item.append(label, value);
        tokens.appendChild(item);
      });
      card.appendChild(tokens);
      return card;
    }

    function renderAuthTypeCards(authStats) {
      const section = document.getElementById('auth-type-section');
      const grid = document.getElementById('auth-type-cards');
      if (!section || !grid) return;

      const entries = Object.entries(authStats || {})
        .filter(([, stat]) => Number(stat && stat.total_requests) > 0)
        .sort(([left], [right]) => {
          const leftIndex = AUTH_TYPE_CARD_ORDER.indexOf(left);
          const rightIndex = AUTH_TYPE_CARD_ORDER.indexOf(right);
          return (leftIndex < 0 ? AUTH_TYPE_CARD_ORDER.length : leftIndex)
            - (rightIndex < 0 ? AUTH_TYPE_CARD_ORDER.length : rightIndex);
        });

      grid.replaceChildren();
      section.hidden = entries.length === 0;
      entries.forEach(([authType, stat]) => {
        const card = createAuthTypeCard(authType);
        grid.appendChild(card);
        updateOverviewCard(authType, stat);
      });
    }

    function buildCurrentDateRangeQuery() {
      return typeof window.buildDateRangeQuery === 'function'
        ? window.buildDateRangeQuery(currentTimeRange, currentCustomTimeRange)
        : `range=${encodeURIComponent(currentTimeRange)}`;
    }

    function currentRangeHours() {
      if (currentTimeRange === 'custom' && currentCustomTimeRange) {
        const startMs = Number(currentCustomTimeRange.startMs);
        const endMs = Number(currentCustomTimeRange.endMs);
        if (Number.isFinite(startMs) && Number.isFinite(endMs) && endMs > startMs) {
          return Math.max((endMs - startMs) / 3600000, 1 / 60);
        }
      }
      return typeof window.getRangeHours === 'function'
        ? window.getRangeHours(currentTimeRange)
        : 24;
    }

    function serviceHealthText(key, fallback, params) {
      if (typeof window.i18nText === 'function') return window.i18nText(key, fallback, params);
      const translated = typeof window.t === 'function' ? window.t(key, params) : key;
      return translated === key ? fallback : translated;
    }

    function serviceHealthLocale() {
      return window.i18n && typeof window.i18n.getLocale === 'function' && window.i18n.getLocale() === 'en'
        ? 'en-US'
        : 'zh-CN';
    }

    function serviceHealthTimeFormatter() {
      return new Intl.DateTimeFormat(serviceHealthLocale(), {
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
        hourCycle: 'h23'
      });
    }

    function serviceHealthPeriodText() {
      return typeof window.getRangeLabel === 'function'
        ? window.getRangeLabel(currentTimeRange)
        : currentTimeRange;
    }

    function hideServiceHealthTooltip() {
      const tooltip = document.getElementById('service-health-tooltip');
      if (tooltip) tooltip.hidden = true;
    }

    function showServiceHealthTooltip(cell, point, formatter, bucketMs) {
      const plot = cell.closest('.service-health-plot');
      const card = plot && plot.closest('.service-health-card');
      const tooltip = document.getElementById('service-health-tooltip');
      const timeElement = document.getElementById('service-health-tooltip-time');
      const successElement = document.getElementById('service-health-tooltip-success');
      const errorElement = document.getElementById('service-health-tooltip-error');
      const rateElement = document.getElementById('service-health-tooltip-rate');
      if (!plot || !card || !tooltip || !timeElement || !successElement || !errorElement || !rateElement) return;

      const intervalMs = bucketMs || 15 * 60 * 1000;
      timeElement.textContent = `${formatter.format(new Date(point.ts))} – ${formatter.format(new Date(point.ts + intervalMs))}`;
      successElement.textContent = formatNumber(point.success);
      errorElement.textContent = formatNumber(point.error);
      rateElement.textContent = point.rate === null ? '--' : `(${(point.rate * 100).toFixed(1)}%)`;

      tooltip.hidden = false;
      tooltip.dataset.placement = 'top';

      const plotRect = plot.getBoundingClientRect();
      const cellRect = cell.getBoundingClientRect();
      const tooltipRect = tooltip.getBoundingClientRect();
      const cellCenter = cellRect.left - plotRect.left + cellRect.width / 2;
      const inset = 8;
      const maxLeft = Math.max(inset, plotRect.width - tooltipRect.width - inset);
      const left = Math.min(Math.max(cellCenter - tooltipRect.width / 2, inset), maxLeft);
      const roomAbove = cellRect.top - card.getBoundingClientRect().top;
      let top = cellRect.top - plotRect.top - tooltipRect.height - 12;

      if (roomAbove < tooltipRect.height + 16) {
        top = cellRect.bottom - plotRect.top + 12;
        tooltip.dataset.placement = 'bottom';
      }

      tooltip.style.left = `${left}px`;
      tooltip.style.top = `${top}px`;
      const arrowX = Math.min(Math.max(cellCenter - left, 12), tooltipRect.width - 12);
      tooltip.style.setProperty('--service-health-tooltip-arrow-x', `${arrowX}px`);
    }

    function renderServiceHealth(model) {
      const grid = document.getElementById('service-health-grid');
      const rateElement = document.getElementById('service-health-rate');
      const message = document.getElementById('service-health-message');
      if (!grid || !rateElement || !message || !model) return;

      hideServiceHealthTooltip();
      const timeFormatter = serviceHealthTimeFormatter();
      const fragment = document.createDocumentFragment();
      for (const [index, point] of model.points.entries()) {
        const cell = document.createElement('span');
        cell.className = `service-health-cell ${point.state}`;
        cell.setAttribute('aria-hidden', 'true');
        cell.dataset.index = String(index);
        fragment.appendChild(cell);
      }
      grid.replaceChildren(fragment);
      grid.onmouseover = event => {
        const cell = event.target.closest('.service-health-cell');
        if (!cell || !grid.contains(cell)) return;
        showServiceHealthTooltip(cell, model.points[Number(cell.dataset.index)], timeFormatter, model.bucketMs);
      };
      grid.onmouseleave = hideServiceHealthTooltip;

      const hasData = model.rate !== null;
      const rate = hasData ? `${(model.rate * 100).toFixed(1)}%` : '--';
      const period = serviceHealthPeriodText();
      rateElement.textContent = rate;
      rateElement.dataset.state = model.state;
      const periodElement = document.getElementById('service-health-period');
      if (periodElement) periodElement.textContent = period;
      const earlierElement = document.getElementById('service-health-earlier');
      const latestElement = document.getElementById('service-health-latest');
      if (earlierElement) {
        earlierElement.textContent = model.points.length > 0
          ? timeFormatter.format(new Date(model.points[0].ts))
          : '--';
      }
      if (latestElement) {
        latestElement.textContent = model.points.length > 0
          ? timeFormatter.format(new Date(model.points.at(-1).ts))
          : '--';
      }
      grid.setAttribute('aria-label', hasData
        ? serviceHealthText(
          'index.health.summary',
          `${period}服务成功率 ${rate}，成功 ${model.success} 次，失败 ${model.error} 次`,
          {
            period,
            rate,
            success: formatNumber(model.success),
            error: formatNumber(model.error)
          }
        )
        : serviceHealthText('index.health.noData', `${period}暂无请求数据`, { period }));
      message.hidden = true;
      message.textContent = '';
    }

    function renderServiceHealthUnavailable() {
      const message = document.getElementById('service-health-message');
      const rateElement = document.getElementById('service-health-rate');
      if (rateElement) {
        rateElement.textContent = '--';
        rateElement.dataset.state = 'unknown';
      }
      if (message) {
        message.hidden = false;
        message.textContent = serviceHealthText(
          'index.health.unavailable',
          '健康数据暂时无法加载，将在下次刷新时重试。'
        );
      }
    }

    async function loadDashboard() {
      const generation = ++dashboardLoadGeneration;
      const dateRangeQuery = buildCurrentDateRangeQuery();
      const grid = document.getElementById('service-health-grid');
      const loadingElements = document.querySelectorAll('.metric-number');
      loadingElements.forEach(element => element.classList.add('animate-pulse'));
      if (grid) grid.setAttribute('aria-busy', 'true');

      const healthRequest = window.ServiceHealth
        ? window.ServiceHealth.buildRequest(dateRangeQuery, currentRangeHours())
        : null;
      const [statsResult, healthResult] = await Promise.allSettled([
        fetchDataWithAuth(`/dashboard/summary?${dateRangeQuery}`),
        healthRequest
          ? fetchDataWithAuth(`/dashboard/metrics?${healthRequest.query}`)
          : Promise.reject(new Error('ServiceHealth unavailable'))
      ]);

      if (generation !== dashboardLoadGeneration) return;

      if (statsResult.status === 'fulfilled') {
        statsData = statsResult.value || statsData;
        updateStatsDisplay();
      } else {
        console.error('Failed to load stats:', statsResult.reason);
        showError('无法加载统计数据');
      }

      if (healthResult.status === 'fulfilled') {
        serviceHealthModel = window.ServiceHealth.buildModel(
          healthResult.value,
          healthRequest.bucketMinutes
        );
        renderServiceHealth(serviceHealthModel);
      } else {
        console.error('Failed to load service health:', healthResult.reason);
        renderServiceHealthUnavailable();
      }

      loadingElements.forEach(element => element.classList.remove('animate-pulse'));
      if (grid) grid.setAttribute('aria-busy', 'false');
    }

    // 更新统计显示
    function updateStatsDisplay() {
      // 更新按客户端入口协议统计
      const protocolStats = statsData.by_client_protocol || {};
      updateOverviewCard('anthropic', protocolStats.anthropic);
      updateOverviewCard('codex', protocolStats.codex);
      updateOverviewCard('openai', protocolStats.openai);
      updateOverviewCard('gemini', protocolStats.gemini);

      renderAuthTypeCards(statsData.by_auth_type || {});
    }

    // 更新单个概览卡片的统计
    function updateOverviewCard(type, data) {
      const card = document.getElementById(`type-${type}-card`);
      if (!card) return;

      // 如果没有数据，显示默认值
      const totalRequests = data ? (data.total_requests || 0) : 0;
      const successRequests = data ? (data.success_requests || 0) : 0;
      const errorRequests = data ? (data.error_requests || 0) : 0;

      const successRate = totalRequests > 0
        ? ((successRequests / totalRequests) * 100).toFixed(1)
        : '0.0';

      // 更新基础统计（总请求、成功、失败、成功率）
      document.getElementById(`type-${type}-requests`).textContent = formatNumber(totalRequests);
      document.getElementById(`type-${type}-success`).textContent = formatNumber(successRequests);
      document.getElementById(`type-${type}-error`).textContent = formatNumber(errorRequests);
      document.getElementById(`type-${type}-rate`).textContent = successRate + '%';

      const inputTokens = data ? (data.total_input_tokens || 0) : 0;
      const outputTokens = data ? (data.total_output_tokens || 0) : 0;
      const totalCost = data ? (data.total_cost || 0) : 0;
      const effectiveCost = data && data.effective_cost !== undefined && data.effective_cost !== null
        ? Number(data.effective_cost) || 0
        : totalCost;

      document.getElementById(`type-${type}-input`).textContent = formatNumber(inputTokens);
      document.getElementById(`type-${type}-output`).textContent = formatNumber(outputTokens);
      document.getElementById(`type-${type}-cost`).innerHTML = buildCostStackHtml(totalCost, effectiveCost, { tone: 'warning', inline: true });

      const cacheReadTokens = data ? (data.total_cache_read_tokens || 0) : 0;
      const cacheReadEl = document.getElementById(`type-${type}-cache-read`);
      if (cacheReadEl) cacheReadEl.textContent = formatNumber(cacheReadTokens);

      const cacheCreateEl = document.getElementById(`type-${type}-cache-create`);
      if (cacheCreateEl) {
        const cacheCreateTokens = data ? (data.total_cache_creation_tokens || 0) : 0;
        cacheCreateEl.textContent = formatNumber(cacheCreateTokens);
      }
    }

    // 通知系统统一由 ui.js 提供（showSuccess/showError/showNotification）

    // 注销功能（已由 ui.js 的 onLogout 统一处理）

    // 自动刷新由 createAutoRefresh 统一管理（system_settings.auto_refresh_interval_seconds）

    // 页面初始化
    window.initPageBootstrap({
      topbarKey: 'index',
      run: () => {
      window.bindTimeRangeSelector({
        containerId: 'index-time-range',
        values: ['today', 'yesterday', 'day_before_yesterday', 'this_week', 'last_week', 'this_month', 'last_month', 'custom'],
        initialValue: currentTimeRange,
        customRange: currentCustomTimeRange,
        onChange: (range, customRange) => {
          currentTimeRange = range;
          if (range === 'custom') currentCustomTimeRange = customRange;
          loadDashboard();
        }
      });

      // 费用与服务健康检测共用同一日期范围快照。
      loadDashboard();

      if (window.i18n && typeof window.i18n.onLocaleChange === 'function') {
        window.i18n.onLocaleChange(() => {
          updateStatsDisplay();
          if (serviceHealthModel) renderServiceHealth(serviceHealthModel);
        });
      }

      // 自动刷新（system_settings.auto_refresh_interval_seconds，0=禁用）
      if (typeof window.createAutoRefresh === 'function') {
        window.createAutoRefresh({ load: loadDashboard }).init();
      }

      // 添加页面动画
      document.querySelectorAll('.animate-slide-up').forEach((el, index) => {
        el.style.animationDelay = `${index * 0.1}s`;
      });
      }
    });
