    // 统计数据管理
    let statsData = {
      total_requests: 0,
      success_requests: 0,
      error_requests: 0,
      active_channels: 0,
      active_models: 0,
      duration_seconds: 1,
      rpm_stats: null,
      is_today: true
    };

    // 当前选中的时间范围
    let currentTimeRange = 'today';
    let currentCustomTimeRange = null;
    let serviceHealthRange = null;
    let serviceHealthModel = null;

    function buildSummaryURL() {
      const query = typeof window.buildDateRangeQuery === 'function'
        ? window.buildDateRangeQuery(currentTimeRange, currentCustomTimeRange)
        : `range=${encodeURIComponent(currentTimeRange)}`;
      return `/dashboard/summary?${query}`;
    }

    function buildServiceHealthURL(range) {
      const params = new URLSearchParams({
        range: 'custom',
        start_time: String(range.startBucketMs),
        end_time: String(range.endMs),
        bucket_min: String(range.bucketMinutes)
      });
      return `/dashboard/metrics?${params.toString()}`;
    }

    function serviceHealthText(key, fallback, params) {
      if (typeof window.i18nText === 'function') return window.i18nText(key, fallback, params);
      const translated = typeof window.t === 'function' ? window.t(key, params) : key;
      return translated === key ? fallback : translated;
    }

    function serviceHealthStateText(state) {
      const labels = {
        healthy: ['index.health.healthy', '健康'],
        warning: ['index.health.warning', '波动'],
        critical: ['index.health.critical', '异常'],
        unknown: ['index.health.noRequests', '无请求']
      };
      const [key, fallback] = labels[state] || labels.unknown;
      return serviceHealthText(key, fallback);
    }

    function serviceHealthTimeFormatter() {
      const locale = window.i18n && typeof window.i18n.getLocale === 'function' && window.i18n.getLocale() === 'en'
        ? 'en-US'
        : 'zh-CN';
      return new Intl.DateTimeFormat(locale, {
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
        hourCycle: 'h23'
      });
    }

    function renderServiceHealth(model) {
      const grid = document.getElementById('service-health-grid');
      const rateElement = document.getElementById('service-health-rate');
      const message = document.getElementById('service-health-message');
      if (!grid || !rateElement || !message || !model) return;

      const formatter = serviceHealthTimeFormatter();
      const fragment = document.createDocumentFragment();
      for (const point of model.points) {
        const cell = document.createElement('span');
        cell.className = `service-health-cell ${point.state}`;
        cell.setAttribute('aria-hidden', 'true');
        const time = formatter.format(new Date(point.ts));
        if (point.rate === null) {
          cell.title = serviceHealthText('index.health.tooltipNoData', `${time} · 无请求`, { time });
        } else {
          const rate = `${(point.rate * 100).toFixed(1)}%`;
          cell.title = serviceHealthText(
            'index.health.tooltip',
            `${time} · ${serviceHealthStateText(point.state)}\n成功 ${point.success} · 失败 ${point.error} · 成功率 ${rate}`,
            {
              time,
              state: serviceHealthStateText(point.state),
              success: formatNumber(point.success),
              error: formatNumber(point.error),
              rate
            }
          );
        }
        fragment.appendChild(cell);
      }
      grid.replaceChildren(fragment);

      const hasData = model.rate !== null;
      const rate = hasData ? `${(model.rate * 100).toFixed(1)}%` : '--';
      rateElement.textContent = rate;
      rateElement.dataset.state = model.state;
      grid.setAttribute('aria-label', hasData
        ? serviceHealthText(
          'index.health.summary',
          `最近 7 天服务成功率 ${rate}，成功 ${model.success} 次，失败 ${model.error} 次`,
          {
            rate,
            success: formatNumber(model.success),
            error: formatNumber(model.error)
          }
        )
        : serviceHealthText('index.health.noData', '最近 7 天暂无请求数据'));
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

    async function loadServiceHealth() {
      const grid = document.getElementById('service-health-grid');
      if (!window.ServiceHealth) {
        renderServiceHealthUnavailable();
        return;
      }

      serviceHealthRange = window.ServiceHealth.buildRange();
      if (!serviceHealthModel) {
        serviceHealthModel = window.ServiceHealth.buildModel([], serviceHealthRange);
        renderServiceHealth(serviceHealthModel);
      }
      if (grid) grid.setAttribute('aria-busy', 'true');

      try {
        const metrics = await fetchDataWithAuth(buildServiceHealthURL(serviceHealthRange));
        serviceHealthModel = window.ServiceHealth.buildModel(metrics, serviceHealthRange);
        renderServiceHealth(serviceHealthModel);
      } catch (error) {
        console.error('Failed to load service health:', error);
        renderServiceHealthUnavailable();
      } finally {
        if (grid) grid.setAttribute('aria-busy', 'false');
      }
    }

    function loadDashboard() {
      return Promise.all([loadStats(), loadServiceHealth()]);
    }

    // 加载统计数据
    async function loadStats() {
      try {
        // 添加加载状态
        document.querySelectorAll('.metric-number').forEach(el => {
          el.classList.add('animate-pulse');
        });

        const data = await fetchDataWithAuth(buildSummaryURL());
        statsData = data || statsData;
        updateStatsDisplay();

      } catch (error) {
        console.error('Failed to load stats:', error);
        showError('无法加载统计数据');
      } finally {
        // 移除加载状态
        document.querySelectorAll('.metric-number').forEach(el => {
          el.classList.remove('animate-pulse');
        });
      }
    }

    // 更新统计显示
    function updateStatsDisplay() {
      const successRate = statsData.total_requests > 0
        ? ((statsData.success_requests / statsData.total_requests) * 100).toFixed(1)
        : '0.0';

      // 更新总体数字显示（成功/失败合并显示）
      document.getElementById('success-requests').textContent = formatNumber(statsData.success_requests || 0);
      document.getElementById('error-requests').textContent = formatNumber(statsData.error_requests || 0);
      document.getElementById('success-rate').textContent = successRate + '%';

      // 更新 RPM（使用峰值/平均/最近格式）
      const rpmStats = statsData.rpm_stats || null;
      const isToday = statsData.is_today !== false;
      updateGlobalRpmDisplay('total-rpm', rpmStats, isToday);

      // 更新按客户端入口协议统计
      const protocolStats = statsData.by_client_protocol || {};
      updateClientProtocolStats('anthropic', protocolStats.anthropic);
      updateClientProtocolStats('codex', protocolStats.codex);
      updateClientProtocolStats('openai', protocolStats.openai);
      updateClientProtocolStats('gemini', protocolStats.gemini);
    }

    // 更新全局 RPM 显示（格式：数值 数值 数值）
    function updateGlobalRpmDisplay(elementId, stats, showRecent) {
      const el = document.getElementById(elementId);
      if (!el) return;

      if (!stats || (stats.peak_rpm < 0.01 && stats.avg_rpm < 0.01)) {
        el.innerHTML = '--';
        return;
      }

      const fmt = v => v >= 1000 ? (v / 1000).toFixed(1) + 'K' : v.toFixed(1);
      const parts = [];

      if (stats.peak_rpm >= 0.01) {
        parts.push(`<span style="color:${getRpmColor(stats.peak_rpm)}">${fmt(stats.peak_rpm)}</span>`);
      }
      if (stats.avg_rpm >= 0.01) {
        parts.push(`<span style="color:${getRpmColor(stats.avg_rpm)}">${fmt(stats.avg_rpm)}</span>`);
      }
      if (showRecent && stats.recent_rpm >= 0.01) {
        parts.push(`<span style="color:${getRpmColor(stats.recent_rpm)}">${fmt(stats.recent_rpm)}</span>`);
      }

      el.innerHTML = parts.length > 0 ? parts.join(' ') : '--';
    }

    // 更新单个客户端入口协议的统计
    function updateClientProtocolStats(type, data) {
      // 始终显示所有卡片，保持界面完整性
      const card = document.getElementById(`type-${type}-card`);
      if (card) card.style.display = 'block';

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

      // 所有客户端协议的Token和成本统计
      const inputTokens = data ? (data.total_input_tokens || 0) : 0;
      const outputTokens = data ? (data.total_output_tokens || 0) : 0;
      const totalCost = data ? (data.total_cost || 0) : 0;
      const effectiveCost = data && data.effective_cost !== undefined && data.effective_cost !== null
        ? Number(data.effective_cost) || 0
        : totalCost;

      document.getElementById(`type-${type}-input`).textContent = formatNumber(inputTokens);
      document.getElementById(`type-${type}-output`).textContent = formatNumber(outputTokens);
      document.getElementById(`type-${type}-cost`).innerHTML = buildCostStackHtml(totalCost, effectiveCost, { tone: 'warning', inline: true });

      // Claude和Codex类型的缓存统计（缓存读+缓存创建）
      if (type === 'anthropic' || type === 'codex') {
        const cacheReadTokens = data ? (data.total_cache_read_tokens || 0) : 0;
        const cacheCreateTokens = data ? (data.total_cache_creation_tokens || 0) : 0;
        document.getElementById(`type-${type}-cache-read`).textContent = formatNumber(cacheReadTokens);
        document.getElementById(`type-${type}-cache-create`).textContent = formatNumber(cacheCreateTokens);
      }

      // OpenAI和Gemini类型的缓存统计（仅缓存读）
      if (type === 'openai' || type === 'gemini') {
        const cacheReadTokens = data ? (data.total_cache_read_tokens || 0) : 0;
        document.getElementById(`type-${type}-cache-read`).textContent = formatNumber(cacheReadTokens);
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
          loadStats();
        }
      });

      // 加载统计和最近七天服务健康数据
      loadDashboard();

      if (window.i18n && typeof window.i18n.onLocaleChange === 'function') {
        window.i18n.onLocaleChange(() => {
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
