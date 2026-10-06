    // 统计数据管理
    let statsData = { by_client_protocol: {}, by_auth_type: {} };

    // 当前选中的时间范围
    let currentTimeRange = 'today';
    let currentCustomTimeRange = null;
    let serviceHealthModel = null;
    let dashboardLoadGeneration = 0;

    // 入口协议固定展示，顺序即表格行序
    const PROTOCOL_USAGE_ROWS = Object.freeze([
      { key: 'anthropic', label: 'Claude Code', icon: '<path d="M12 2L2 22h20L12 2zm0 4.5L18.5 20h-13L12 6.5z"/>' },
      { key: 'codex', label: 'Codex', icon: '<path d="M22.2819 9.8211a5.9847 5.9847 0 0 0-.5157-4.9108 6.0462 6.0462 0 0 0-6.5098-2.9A6.0651 6.0651 0 0 0 4.9807 4.1818a5.9847 5.9847 0 0 0-3.9977 2.9 6.0462 6.0462 0 0 0 .7427 7.0966 5.98 5.98 0 0 0 .511 4.9107 6.051 6.051 0 0 0 6.5146 2.9001A5.9847 5.9847 0 0 0 13.2599 24a6.0557 6.0557 0 0 0 5.7718-4.2058 5.9894 5.9894 0 0 0 3.9977-2.9001 6.0557 6.0557 0 0 0-.7475-7.0729zm-9.022 12.6081a4.4755 4.4755 0 0 1-2.8764-1.0408l.1419-.0804 4.7783-2.7582a.7948.7948 0 0 0 .3927-.6813v-6.7369l2.02 1.1686a.071.071 0 0 1 .038.052v5.5826a4.504 4.504 0 0 1-4.4945 4.4944zm-9.6607-4.1254a4.4708 4.4708 0 0 1-.5346-3.0137l.142.0852 4.783 2.7582a.7712.7712 0 0 0 .7806 0l5.8428-3.3685v2.3324a.0804.0804 0 0 1-.0332.0615L9.74 19.9502a4.4992 4.4992 0 0 1-6.1408-1.6464zM2.3408 7.8956a4.485 4.485 0 0 1 2.3655-1.9728V11.6a.7664.7664 0 0 0 .3879.6765l5.8144 3.3543-2.0201 1.1685a.0757.0757 0 0 1-.071 0l-4.8303-2.7865A4.504 4.504 0 0 1 2.3408 7.872zm16.5963 3.8558L13.1038 8.364 15.1192 7.2a.0757.0757 0 0 1 .071 0l4.8303 2.7913a4.4944 4.4944 0 0 1-.6765 8.1042v-5.6772a.79.79 0 0 0-.407-.667zm2.0107-3.0231l-.142-.0852-4.7735-2.7818a.7759.7759 0 0 0-.7854 0L9.409 9.2297V6.8974a.0662.0662 0 0 1 .0284-.0615l4.8303-2.7866a4.4992 4.4992 0 0 1 6.6802 4.66zM8.3065 12.863l-2.02-1.1638a.0804.0804 0 0 1-.038-.0567V6.0742a4.4992 4.4992 0 0 1 7.3757-3.4537l-.142.0805L8.704 5.459a.7948.7948 0 0 0-.3927.6813zm1.0976-2.3654l2.602-1.4998 2.6069 1.4998v2.9994l-2.5974 1.4997-2.6067-1.4997Z"/>' },
      { key: 'openai', label: 'OpenAI', icon: '<path d="M22.2819 9.8211a5.9847 5.9847 0 0 0-.5157-4.9108 6.0462 6.0462 0 0 0-6.5098-2.9A6.0651 6.0651 0 0 0 4.9807 4.1818a5.9847 5.9847 0 0 0-3.9977 2.9 6.0462 6.0462 0 0 0 .7427 7.0966 5.98 5.98 0 0 0 .511 4.9107 6.051 6.051 0 0 0 6.5146 2.9001A5.9847 5.9847 0 0 0 13.2599 24a6.0557 6.0557 0 0 0 5.7718-4.2058 5.9894 5.9894 0 0 0 3.9977-2.9001 6.0557 6.0557 0 0 0-.7475-7.0729zm-9.022 12.6081a4.4755 4.4755 0 0 1-2.8764-1.0408l.1419-.0804 4.7783-2.7582a.7948.7948 0 0 0 .3927-.6813v-6.7369l2.02 1.1686a.071.071 0 0 1 .038.052v5.5826a4.504 4.504 0 0 1-4.4945 4.4944zm-9.6607-4.1254a4.4708 4.4708 0 0 1-.5346-3.0137l.142.0852 4.783 2.7582a.7712.7712 0 0 0 .7806 0l5.8428-3.3685v2.3324a.0804.0804 0 0 1-.0332.0615L9.74 19.9502a4.4992 4.4992 0 0 1-6.1408-1.6464zM2.3408 7.8956a4.485 4.485 0 0 1 2.3655-1.9728V11.6a.7664.7664 0 0 0 .3879.6765l5.8144 3.3543-2.0201 1.1685a.0757.0757 0 0 1-.071 0l-4.8303-2.7865A4.504 4.504 0 0 1 2.3408 7.872zm16.5963 3.8558L13.1038 8.364 15.1192 7.2a.0757.0757 0 0 1 .071 0l4.8303 2.7913a4.4944 4.4944 0 0 1-.6765 8.1042v-5.6772a.79.79 0 0 0-.407-.667zm2.0107-3.0231l-.142-.0852-4.7735-2.7818a.7759.7759 0 0 0-.7854 0L9.409 9.2297V6.8974a.0662.0662 0 0 1 .0284-.0615l4.8303-2.7866a4.4992 4.4992 0 0 1 6.6802 4.66zM8.3065 12.863l-2.02-1.1638a.0804.0804 0 0 1-.038-.0567V6.0742a4.4992 4.4992 0 0 1 7.3757-3.4537l-.142.0805L8.704 5.459a.7948.7948 0 0 0-.3927.6813zm1.0976-2.3654l2.602-1.4998 2.6069 1.4998v2.9994l-2.5974 1.4997-2.6067-1.4997Z"/>' },
      { key: 'gemini', label: 'Google Gemini', icon: '<path d="M12 2L2 7v10l10 5 10-5V7L12 2zm0 2.18L19.82 8 12 11.82 4.18 8 12 4.18zM4 9.48l7 3.5v7.84l-7-3.5V9.48zm16 0v7.84l-7 3.5v-7.84l7-3.5z"/>' }
    ]);

    // 渠道认证类型的显示名与行序；图标与渠道管理页共用 channel-provider-icons.js
    const AUTH_TYPE_LABEL_KEYS = Object.freeze({
      api_key: 'index.channels.api',
      codex_oauth: 'index.channels.codex',
      antigravity_oauth: 'index.channels.antigravity',
      xai_oauth: 'index.channels.xai',
      anthropic_oauth: 'index.channels.anthropic',
      zai_oauth: 'index.channels.zai',
      cursor_oauth: 'index.channels.cursor',
      zed_oauth: 'index.channels.zed'
    });

    const AUTH_TYPE_ORDER = Object.keys(AUTH_TYPE_LABEL_KEYS);

    function overviewText(key, fallback, params) {
      if (typeof window.i18nText === 'function') return window.i18nText(key, fallback, params);
      const translated = typeof window.t === 'function' ? window.t(key, params) : key;
      return translated === key ? fallback : translated;
    }

    function toNumber(value) {
      const n = Number(value);
      return Number.isFinite(n) ? n : 0;
    }

    // effective_cost 缺省时视为未打折，等于 total_cost
    function effectiveCostOf(stat) {
      return stat && stat.effective_cost !== undefined && stat.effective_cost !== null
        ? toNumber(stat.effective_cost)
        : toNumber(stat && stat.total_cost);
    }

    // 全局指标：请求计数取响应顶层；费用与输出 Tokens 按入口协议加总（每个请求只属于一个入口协议）
    function buildOverviewTotals(summary) {
      const totals = {
        requests: toNumber(summary && summary.total_requests),
        success: toNumber(summary && summary.success_requests),
        error: toNumber(summary && summary.error_requests),
        rate: null,
        cost: 0,
        effectiveCost: 0,
        outputTokens: 0
      };
      for (const stat of Object.values((summary && summary.by_client_protocol) || {})) {
        totals.cost += toNumber(stat && stat.total_cost);
        totals.effectiveCost += effectiveCostOf(stat);
        totals.outputTokens += toNumber(stat && stat.total_output_tokens);
      }
      if (totals.requests > 0) totals.rate = totals.success / totals.requests;
      return totals;
    }

    function formatRate(rate) {
      return rate === null ? '--' : `${(rate * 100).toFixed(1)}%`;
    }

    function formatOverviewCost(stat) {
      const total = toNumber(stat && stat.total_cost);
      return buildCostStackHtml(total, effectiveCostOf(stat), { tone: 'warning', inline: true })
        || escapeHtml(formatCost(0, 3));
    }

    function protocolIconHtml(row) {
      return `<span class="usage-avatar usage-avatar--${row.key}" aria-hidden="true"><svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor">${row.icon}</svg></span>`;
    }

    function authTypeIconHtml(authType, label) {
      const content = typeof window.channelAvatarContentHTML === 'function'
        ? window.channelAvatarContentHTML(authType, label)
        : escapeHtml(label.charAt(0));
      return `<span class="usage-avatar channel-provider-avatar" data-auth-type="${escapeHtml(authType)}" aria-hidden="true">${content}</span>`;
    }

    // 协议表与渠道表共用：rows 为 { label, iconHtml, stat }
    // 只列有请求的行；没有任何行时隐藏整个分区。
    function renderUsageSection(sectionId, tbodyId, rows) {
      const section = document.getElementById(sectionId);
      const tbody = document.getElementById(tbodyId);
      if (!section || !tbody) return;
      const activeRows = rows.filter(row => toNumber(row.stat && row.stat.total_requests) > 0);
      section.hidden = activeRows.length === 0;
      tbody.innerHTML = activeRows.map(({ label, iconHtml, stat }) => {
        const requests = toNumber(stat.total_requests);
        const rate = toNumber(stat.success_requests) / requests;
        const rateState = window.ServiceHealth ? window.ServiceHealth.classifyRate(rate) : 'unknown';
        const rateText = `${Number((rate * 100).toFixed(1))}%`;
        return `<tr>
          <th scope="row"><span class="usage-name">${iconHtml}<span>${escapeHtml(label)}</span></span></th>
          <td>${formatNumber(requests)}</td>
          <td><span class="usage-rate" data-state="${rateState}">${rateText}</span></td>
          <td>${formatOverviewCost(stat)}</td>
          <td>${formatNumber(toNumber(stat && stat.total_input_tokens))}</td>
          <td>${formatNumber(toNumber(stat && stat.total_output_tokens))}</td>
          <td>${formatNumber(toNumber(stat && stat.total_cache_read_tokens))}</td>
          <td>${formatNumber(toNumber(stat && stat.total_cache_creation_tokens))}</td>
        </tr>`;
      }).join('');
    }

    function renderProtocolUsage(protocolStats) {
      renderUsageSection('protocol-usage-section', 'protocol-usage-body', PROTOCOL_USAGE_ROWS.map(row => ({
        label: row.label,
        iconHtml: protocolIconHtml(row),
        stat: protocolStats[row.key]
      })));
    }

    function renderAuthTypeUsage(authStats) {
      const orderOf = authType => {
        const index = AUTH_TYPE_ORDER.indexOf(authType);
        return index < 0 ? AUTH_TYPE_ORDER.length : index;
      };
      const entries = Object.entries(authStats || {})
        .sort(([left], [right]) => orderOf(left) - orderOf(right));
      renderUsageSection('auth-type-section', 'auth-type-usage-body', entries.map(([authType, stat]) => {
        const labelKey = AUTH_TYPE_LABEL_KEYS[authType];
        const label = labelKey ? overviewText(labelKey, authType) : authType;
        return { label, iconHtml: authTypeIconHtml(authType, label), stat };
      }));
    }

    function renderOverviewKpis(totals) {
      document.getElementById('overview-requests').textContent = formatNumber(totals.requests);
      document.getElementById('overview-rate').textContent = formatRate(totals.rate);
      document.getElementById('overview-counts').textContent = overviewText(
        'index.overview.successFailed',
        `${totals.success} 成功 · ${totals.error} 失败`,
        { success: formatNumber(totals.success), error: formatNumber(totals.error) }
      );
      document.getElementById('overview-cost').innerHTML = formatOverviewCost({
        total_cost: totals.cost,
        effective_cost: totals.effectiveCost
      });
      document.getElementById('overview-output-tokens').textContent = formatNumber(totals.outputTokens);
    }

    function renderOverviewStatus(state) {
      const badge = document.getElementById('overview-status');
      const text = document.getElementById('overview-status-text');
      if (!badge || !text) return;
      const fallbacks = { healthy: '服务正常', warning: '服务波动', critical: '服务异常', unknown: '暂无数据' };
      const safeState = Object.hasOwn(fallbacks, state) ? state : 'unknown';
      const key = `index.overview.status.${safeState}`;
      badge.dataset.state = safeState;
      text.dataset.i18n = key;
      text.textContent = overviewText(key, fallbacks[safeState]);
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
        ? overviewText(
          'index.health.summary',
          `${period}服务成功率 ${rate}，成功 ${model.success} 次，失败 ${model.error} 次`,
          {
            period,
            rate,
            success: formatNumber(model.success),
            error: formatNumber(model.error)
          }
        )
        : overviewText('index.health.noData', `${period}暂无请求数据`, { period }));
      const countsElement = document.getElementById('service-health-counts');
      if (countsElement) {
        countsElement.textContent = hasData
          ? overviewText(
            'index.overview.successFailed',
            `${model.success} 成功 · ${model.error} 失败`,
            { success: formatNumber(model.success), error: formatNumber(model.error) }
          )
          : '';
      }
      renderOverviewStatus(model.state);
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
      renderOverviewStatus('unknown');
      if (message) {
        message.hidden = false;
        message.textContent = overviewText(
          'index.health.unavailable',
          '健康数据暂时无法加载，将在下次刷新时重试。'
        );
      }
    }

    async function loadDashboard() {
      const generation = ++dashboardLoadGeneration;
      const dateRangeQuery = buildCurrentDateRangeQuery();
      const grid = document.getElementById('service-health-grid');
      const refreshButton = document.getElementById('overview-refresh');
      if (refreshButton) refreshButton.setAttribute('aria-busy', 'true');
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

      if (refreshButton) refreshButton.setAttribute('aria-busy', 'false');
      if (grid) grid.setAttribute('aria-busy', 'false');
    }

    // 更新统计显示
    function updateStatsDisplay() {
      renderOverviewKpis(buildOverviewTotals(statsData));
      renderProtocolUsage(statsData.by_client_protocol || {});
      renderAuthTypeUsage(statsData.by_auth_type || {});
    }

    // 通知系统统一由 ui.js 提供（showSuccess/showError/showNotification）

    // 注销功能（已由 ui.js 的 onLogout 统一处理）

    // 自动刷新由 createAutoRefresh 统一管理（system_settings.auto_refresh_interval_seconds）

    if (typeof module !== 'undefined' && module.exports) {
      module.exports = { buildOverviewTotals };
    }

    // 页面初始化
    if (typeof window !== 'undefined') window.initPageBootstrap({
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
      document.getElementById('overview-refresh')?.addEventListener('click', loadDashboard);

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
