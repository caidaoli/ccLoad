    // 统计数据管理
    let statsData = { by_client_protocol: {}, by_auth_type: {} };

    // 当前选中的时间范围
    let currentTimeRange = 'today';
    let currentCustomTimeRange = null;
    let serviceHealthModel = null;
    let dashboardLoadGeneration = 0;

    const AUTH_TYPE_CARD_CONFIG = Object.freeze({
      api_key: { labelKey: 'channels.authTypeAPI', iconClass: 'api', icon: '<path d="M15 7a5 5 0 1 0-9.9 1H3v4h2v2h2v2h3v-3.1A5 5 0 0 0 15 7Zm-5 0a1.5 1.5 0 1 1-3 0 1.5 1.5 0 0 1 3 0Z"/>' },
      codex_oauth: { labelKey: 'channels.authTypeCodex', iconClass: 'codex', icon: '<path d="M12 3a9 9 0 1 0 8.7 11.3l-2.5-.7A6.5 6.5 0 1 1 12 5.5c1.8 0 3.4.7 4.6 1.9l1.8-1.8A9 9 0 0 0 12 3Z"/>' },
      antigravity_oauth: { labelKey: 'channels.authTypeAntigravity', iconClass: 'antigravity', icon: '<path d="m12 3 2.2 5.6L20 11l-5.8 2.2L12 19l-2.2-5.8L4 11l5.8-2.4L12 3Z"/>' },
      xai_oauth: { labelKey: 'channels.authTypeXAI', iconClass: 'xai', icon: '<path d="m5 5 14 14M19 5 5 19"/>' },
      anthropic_oauth: { labelKey: 'channels.authTypeAnthropic', iconClass: 'anthropic', icon: '<path d="M12 2 2 22h20L12 2Zm0 4.5L18.5 20h-13L12 6.5Z"/>' },
      zai_oauth: { labelKey: 'channels.authTypeZAI', iconClass: 'zai', icon: '<path d="M5 6h14L5 18h14"/>' },
      cursor_oauth: { labelKey: 'channels.authTypeCursor', iconClass: 'cursor', icon: '<path d="m5 3 15 10-7 1.5 4 7-2.7 1.5-4-7-5.3 3.7V3Z"/>' },
      zed_oauth: { labelKey: 'channels.authTypeZed', iconClass: 'zed', icon: '<path d="m13 2-8 12h6l-1 8 8-12h-6l1-8Z"/>' }
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
      icon.setAttribute('viewBox', '0 0 24 24');
      icon.setAttribute('fill', 'none');
      icon.setAttribute('stroke', 'currentColor');
      icon.setAttribute('stroke-width', '2.2');
      icon.setAttribute('stroke-linecap', 'round');
      icon.setAttribute('stroke-linejoin', 'round');
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
