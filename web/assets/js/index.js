    // 首页摘要：全局计数与入口协议/认证类型/模型/渠道各维度汇总
    let statsData = { by_client_protocol: {}, by_auth_type: {}, by_model: [], by_channel: [] };
    let usageRankMetric = 'requests';
    // Token 构成悬停/点击选中的分段（tone，空为未选中）
    let tokenMixActive = '';
    // 迷你趋势当前序列与悬停的数据点（metric:index，空为未悬停）
    let sparkSeries = null;
    let sparkHover = '';
    // 最近一次渲染的全局指标，Token 构成提示框取其用量
    let overviewTotals = null;

    // 当前选中的时间范围
    let currentTimeRange = 'today';
    let currentCustomTimeRange = null;
    let serviceHealthModel = null;
    let dashboardLoadGeneration = 0;
    let dashboardUpdatedAt = null;

    const SPARK_MAX_POINTS = 48;
    const USAGE_RANK_LIMIT = 5;

    // 时间范围同步到 URL（range/start_time/end_time）与 localStorage，刷新/分享后保持一致
    const INDEX_FILTER_KEY = 'index.filters';
    const INDEX_RANGE_VALUES = ['today', 'yesterday', 'day_before_yesterday', 'this_week', 'last_week', 'this_month', 'last_month', 'custom'];
    const INDEX_FILTER_FIELDS = [
      {
        key: 'range',
        queryKeys: ['range'],
        defaultValue: 'today',
        includeInQuery(value) {
          return Boolean(value) && value !== 'today';
        }
      },
      {
        key: 'customStartTime',
        queryKeys: ['start_time'],
        defaultValue: '',
        includeInQuery(value, values) {
          return values?.range === 'custom' && Boolean(value);
        }
      },
      {
        key: 'customEndTime',
        queryKeys: ['end_time'],
        defaultValue: '',
        includeInQuery(value, values) {
          return values?.range === 'custom' && Boolean(value);
        }
      }
    ];

    // 入口协议的显示名与图标
    const PROTOCOL_USAGE_ROWS = Object.freeze([
      { key: 'anthropic', label: 'Claude Code', icon: '<path d="M12 2L2 22h20L12 2zm0 4.5L18.5 20h-13L12 6.5z"/>' },
      { key: 'codex', label: 'Codex', icon: '<path d="M22.2819 9.8211a5.9847 5.9847 0 0 0-.5157-4.9108 6.0462 6.0462 0 0 0-6.5098-2.9A6.0651 6.0651 0 0 0 4.9807 4.1818a5.9847 5.9847 0 0 0-3.9977 2.9 6.0462 6.0462 0 0 0 .7427 7.0966 5.98 5.98 0 0 0 .511 4.9107 6.051 6.051 0 0 0 6.5146 2.9001A5.9847 5.9847 0 0 0 13.2599 24a6.0557 6.0557 0 0 0 5.7718-4.2058 5.9894 5.9894 0 0 0 3.9977-2.9001 6.0557 6.0557 0 0 0-.7475-7.0729zm-9.022 12.6081a4.4755 4.4755 0 0 1-2.8764-1.0408l.1419-.0804 4.7783-2.7582a.7948.7948 0 0 0 .3927-.6813v-6.7369l2.02 1.1686a.071.071 0 0 1 .038.052v5.5826a4.504 4.504 0 0 1-4.4945 4.4944zm-9.6607-4.1254a4.4708 4.4708 0 0 1-.5346-3.0137l.142.0852 4.783 2.7582a.7712.7712 0 0 0 .7806 0l5.8428-3.3685v2.3324a.0804.0804 0 0 1-.0332.0615L9.74 19.9502a4.4992 4.4992 0 0 1-6.1408-1.6464zM2.3408 7.8956a4.485 4.485 0 0 1 2.3655-1.9728V11.6a.7664.7664 0 0 0 .3879.6765l5.8144 3.3543-2.0201 1.1685a.0757.0757 0 0 1-.071 0l-4.8303-2.7865A4.504 4.504 0 0 1 2.3408 7.872zm16.5963 3.8558L13.1038 8.364 15.1192 7.2a.0757.0757 0 0 1 .071 0l4.8303 2.7913a4.4944 4.4944 0 0 1-.6765 8.1042v-5.6772a.79.79 0 0 0-.407-.667zm2.0107-3.0231l-.142-.0852-4.7735-2.7818a.7759.7759 0 0 0-.7854 0L9.409 9.2297V6.8974a.0662.0662 0 0 1 .0284-.0615l4.8303-2.7866a4.4992 4.4992 0 0 1 6.6802 4.66zM8.3065 12.863l-2.02-1.1638a.0804.0804 0 0 1-.038-.0567V6.0742a4.4992 4.4992 0 0 1 7.3757-3.4537l-.142.0805L8.704 5.459a.7948.7948 0 0 0-.3927.6813zm1.0976-2.3654l2.602-1.4998 2.6069 1.4998v2.9994l-2.5974 1.4997-2.6067-1.4997Z"/>' },
      { key: 'openai', label: 'OpenAI', icon: '<path d="M22.2819 9.8211a5.9847 5.9847 0 0 0-.5157-4.9108 6.0462 6.0462 0 0 0-6.5098-2.9A6.0651 6.0651 0 0 0 4.9807 4.1818a5.9847 5.9847 0 0 0-3.9977 2.9 6.0462 6.0462 0 0 0 .7427 7.0966 5.98 5.98 0 0 0 .511 4.9107 6.051 6.051 0 0 0 6.5146 2.9001A5.9847 5.9847 0 0 0 13.2599 24a6.0557 6.0557 0 0 0 5.7718-4.2058 5.9894 5.9894 0 0 0 3.9977-2.9001 6.0557 6.0557 0 0 0-.7475-7.0729zm-9.022 12.6081a4.4755 4.4755 0 0 1-2.8764-1.0408l.1419-.0804 4.7783-2.7582a.7948.7948 0 0 0 .3927-.6813v-6.7369l2.02 1.1686a.071.071 0 0 1 .038.052v5.5826a4.504 4.504 0 0 1-4.4945 4.4944zm-9.6607-4.1254a4.4708 4.4708 0 0 1-.5346-3.0137l.142.0852 4.783 2.7582a.7712.7712 0 0 0 .7806 0l5.8428-3.3685v2.3324a.0804.0804 0 0 1-.0332.0615L9.74 19.9502a4.4992 4.4992 0 0 1-6.1408-1.6464zM2.3408 7.8956a4.485 4.485 0 0 1 2.3655-1.9728V11.6a.7664.7664 0 0 0 .3879.6765l5.8144 3.3543-2.0201 1.1685a.0757.0757 0 0 1-.071 0l-4.8303-2.7865A4.504 4.504 0 0 1 2.3408 7.872zm16.5963 3.8558L13.1038 8.364 15.1192 7.2a.0757.0757 0 0 1 .071 0l4.8303 2.7913a4.4944 4.4944 0 0 1-.6765 8.1042v-5.6772a.79.79 0 0 0-.407-.667zm2.0107-3.0231l-.142-.0852-4.7735-2.7818a.7759.7759 0 0 0-.7854 0L9.409 9.2297V6.8974a.0662.0662 0 0 1 .0284-.0615l4.8303-2.7866a4.4992 4.4992 0 0 1 6.6802 4.66zM8.3065 12.863l-2.02-1.1638a.0804.0804 0 0 1-.038-.0567V6.0742a4.4992 4.4992 0 0 1 7.3757-3.4537l-.142.0805L8.704 5.459a.7948.7948 0 0 0-.3927.6813zm1.0976-2.3654l2.602-1.4998 2.6069 1.4998v2.9994l-2.5974 1.4997-2.6067-1.4997Z"/>' },
      { key: 'gemini', label: 'Google Gemini', icon: '<path d="M12 2L2 7v10l10 5 10-5V7L12 2zm0 2.18L19.82 8 12 11.82 4.18 8 12 4.18zM4 9.48l7 3.5v7.84l-7-3.5V9.48zm16 0v7.84l-7 3.5v-7.84l7-3.5z"/>' },
      { key: 'system_one', label: 'System One', icon: '<g fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M16 3h5v5"/><path d="M8 3H3v5"/><path d="M12 22v-8.3a4 4 0 0 0-1.172-2.872L3 3"/><path d="m15 9 6-6"/></g>' }
    ]);

    // 渠道认证类型的显示名与渠道管理页筛选一致；图标共用 channel-provider-icons.js
    const AUTH_TYPE_LABEL_KEYS = Object.freeze({
      api_key: 'channels.authTypeAPI',
      codex_oauth: 'channels.authTypeCodex',
      antigravity_oauth: 'channels.authTypeAntigravity',
      codebuddy_oauth: 'channels.authTypeCodeBuddy',
      xai_oauth: 'channels.authTypeXAI',
      anthropic_oauth: 'channels.authTypeAnthropic',
      zai_oauth: 'channels.authTypeZAI',
      system_one: 'channels.authTypeSystemOne',
      cursor_oauth: 'channels.authTypeCursor',
      zed_oauth: 'channels.authTypeZed',
      vertex_oauth: 'channels.authTypeVertex'
    });

    // Token 构成的分段顺序即横条绘制顺序
    const TOKEN_MIX_PARTS = Object.freeze([
      { key: 'inputTokens', labelKey: 'index.tokenMix.input', fallback: '输入', tone: 'input' },
      { key: 'outputTokens', labelKey: 'index.tokenMix.output', fallback: '输出', tone: 'output' },
      { key: 'cacheReadTokens', labelKey: 'index.tokenMix.cacheRead', fallback: '缓存读取', tone: 'cache-read' },
      { key: 'cacheCreationTokens', labelKey: 'index.tokenMix.cacheCreation', fallback: '缓存创建', tone: 'cache-write' }
    ]);

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

    // 全局指标：请求计数取响应顶层；费用与 Tokens 按入口协议加总（每个请求只属于一个入口协议）。
    // 入库的 input_tokens 已扣除缓存读写，四类 Token 互不重叠，相加即总量。
    function buildOverviewTotals(summary) {
      const totals = {
        requests: toNumber(summary && summary.total_requests),
        success: toNumber(summary && summary.success_requests),
        error: toNumber(summary && summary.error_requests),
        rate: null,
        cost: 0,
        effectiveCost: 0,
        inputTokens: 0,
        outputTokens: 0,
        cacheReadTokens: 0,
        cacheCreationTokens: 0,
        promptTokens: 0,
        totalTokens: 0,
        cacheRate: null
      };
      for (const stat of Object.values((summary && summary.by_client_protocol) || {})) {
        totals.cost += toNumber(stat && stat.total_cost);
        totals.effectiveCost += effectiveCostOf(stat);
        totals.inputTokens += toNumber(stat && stat.total_input_tokens);
        totals.outputTokens += toNumber(stat && stat.total_output_tokens);
        totals.cacheReadTokens += toNumber(stat && stat.total_cache_read_tokens);
        totals.cacheCreationTokens += toNumber(stat && stat.total_cache_creation_tokens);
      }
      totals.promptTokens = totals.inputTokens + totals.cacheReadTokens + totals.cacheCreationTokens;
      totals.totalTokens = totals.promptTokens + totals.outputTokens;
      if (totals.requests > 0) totals.rate = totals.success / totals.requests;
      if (totals.promptTokens > 0) totals.cacheRate = totals.cacheReadTokens / totals.promptTokens;
      return totals;
    }

    // 费用 KPI 不需要三位小数，满 $1 取两位
    function formatOverviewCost(cost) {
      return formatCost(cost, cost >= 1 ? 2 : undefined);
    }

    // 指标分桶 → 迷你趋势序列；桶数过多时合并相邻桶，保持折线可读。
    // starts/ends 为每个点覆盖的时间段（毫秒），桶缺少时间戳时为 null。
    function buildSparkSeries(metrics, bucketMinutes) {
      const points = Array.isArray(metrics) ? metrics : [];
      const chunk = Math.max(1, Math.ceil(points.length / SPARK_MAX_POINTS));
      const bucketMs = toNumber(bucketMinutes) * 60 * 1000;
      const series = { requests: [], errors: [], cost: [], starts: [], ends: [] };
      for (let start = 0; start < points.length; start += chunk) {
        const group = points.slice(start, start + chunk);
        let requests = 0;
        let errors = 0;
        let cost = 0;
        for (const point of group) {
          if (!point) continue;
          requests += toNumber(point.success) + toNumber(point.error);
          errors += toNumber(point.error);
          cost += effectiveCostOf(point);
        }
        const startMs = Date.parse(group[0] && group[0].ts);
        series.requests.push(requests);
        series.errors.push(errors);
        series.cost.push(cost);
        series.starts.push(Number.isFinite(startMs) ? startMs : null);
        series.ends.push(Number.isFinite(startMs) && bucketMs > 0 ? startMs + group.length * bucketMs : null);
      }
      return series;
    }

    // 序列各点在 viewBox(100×32) 中的坐标，按最大值归一；全零或单点时贴底
    function sparkCoords(values) {
      const max = Math.max(0, ...values);
      const flat = values.length < 2 || max <= 0;
      return values.map((value, index) => ({
        x: values.length < 2 ? 50 : index * 100 / (values.length - 1),
        y: flat ? 31 : 31 - value / max * 28
      }));
    }

    function sparkPath(points) {
      return points.map((point, index) => `${index === 0 ? 'M' : 'L'}${point.x.toFixed(2)} ${point.y.toFixed(2)}`).join('');
    }

    function renderSparkline(id, values) {
      const svg = document.getElementById(id);
      if (!svg) return;
      if (!values) {
        svg.innerHTML = '';
        return;
      }
      if (values.length < 2 || Math.max(0, ...values) <= 0) {
        svg.innerHTML = '<path class="overview-spark-line overview-spark-line--flat" d="M0 31H100"/>';
        return;
      }
      const line = sparkPath(sparkCoords(values));
      svg.innerHTML = `<path class="overview-spark-area" d="${line}L100 32L0 32Z"/><path class="overview-spark-line" d="${line}"/>`;
    }

    function renderSparklines(series) {
      sparkSeries = series;
      hideSparkTooltip();
      renderSparkline('overview-requests-spark', series && series.requests);
      renderSparkline('overview-cost-spark', series && series.cost);
    }

    function hideSparkTooltip() {
      sparkHover = '';
      const tooltip = document.getElementById('overview-spark-tooltip');
      const dot = document.getElementById('overview-spark-dot');
      if (tooltip) tooltip.hidden = true;
      if (dot) dot.hidden = true;
    }

    // 悬停或点击（触屏）迷你趋势：圆点标出最近的数据点，提示框显示其时间段与数值
    function showSparkTooltip(svg, clientX) {
      const metric = svg.dataset.metric;
      const values = sparkSeries && sparkSeries[metric];
      const container = svg.closest('.overview-kpis');
      const tooltip = document.getElementById('overview-spark-tooltip');
      const dot = document.getElementById('overview-spark-dot');
      const timeElement = document.getElementById('overview-spark-tooltip-time');
      const valueElement = document.getElementById('overview-spark-tooltip-value');
      if (!values || values.length === 0 || !container || !tooltip || !dot || !timeElement || !valueElement) {
        hideSparkTooltip();
        return;
      }
      const rect = svg.getBoundingClientRect();
      const ratio = rect.width > 0 ? (clientX - rect.left) / rect.width : 0;
      const index = Math.min(Math.max(Math.round(ratio * (values.length - 1)), 0), values.length - 1);
      const hoverKey = `${metric}:${index}`;
      if (hoverKey === sparkHover) return;
      sparkHover = hoverKey;

      const errors = metric === 'requests' ? sparkSeries.errors : null;
      const point = sparkCoords(values)[index];
      const x = rect.left + point.x / 100 * rect.width;
      const y = rect.top + point.y / 32 * rect.height;
      const containerRect = container.getBoundingClientRect();
      dot.hidden = false;
      dot.style.left = `${x - containerRect.left}px`;
      dot.style.top = `${y - containerRect.top}px`;
      const failedCount = errors ? errors[index] : 0;
      dot.style.setProperty('--spark-color', getComputedStyle(svg).getPropertyValue('--spark-color'));

      const startMs = sparkSeries.starts[index];
      const endMs = sparkSeries.ends[index];
      const hasTime = startMs !== null && endMs !== null;
      const formatter = serviceHealthTimeFormatter();
      timeElement.hidden = !hasTime;
      timeElement.textContent = hasTime
        ? `${formatter.format(new Date(startMs))} – ${formatter.format(new Date(endMs))}`
        : '';
      const value = values[index];
      if (metric === 'cost') valueElement.textContent = formatCost(value);
      else if (failedCount > 0) {
        const failed = overviewText('index.overview.failedCount', `${failedCount}\u00a0失败`, { count: formatNumber(failedCount) });
        const rate = failedCount / value;
        valueElement.textContent = `${formatNumber(value)} · ${failed} (${rate < 0.001 ? '<0.1%' : formatPercent(rate)})`;
      } else valueElement.textContent = formatNumber(value);
      tooltip.hidden = false;
      placeOverviewTooltip(tooltip, container, { left: x, width: 0, top: y - 4, bottom: y + 4 });
    }

    function bindSparklines() {
      for (const svg of document.querySelectorAll('.overview-spark[data-metric]')) {
        const show = event => showSparkTooltip(svg, event.clientX);
        svg.addEventListener('mousemove', show);
        svg.addEventListener('click', show);
        svg.addEventListener('mouseleave', hideSparkTooltip);
      }
    }

    function renderOverviewKpis(totals) {
      document.getElementById('overview-requests').textContent = formatNumber(totals.requests);
      document.getElementById('overview-counts').textContent = overviewText(
        'index.overview.successFailed',
        `${totals.success} 成功 · ${totals.error} 失败`,
        { success: formatNumber(totals.success), error: formatNumber(totals.error) }
      );
      const rateElement = document.getElementById('overview-rate');
      rateElement.textContent = formatPercent(totals.rate);
      rateElement.dataset.state = window.ServiceHealth
        ? window.ServiceHealth.classifyRate(totals.rate)
        : 'unknown';
      document.getElementById('overview-tokens').textContent = formatNumber(totals.totalTokens);
      document.getElementById('overview-tokens-detail').textContent = overviewText(
        'index.overview.cacheRateDetail',
        `缓存率\u00a0${formatPercent(totals.cacheRate)}`,
        { rate: formatPercent(totals.cacheRate) }
      );
      // 主值为倍率后的实付费用；有倍率时副行给出原价
      const cost = getCostDisplayInfo(totals.cost, totals.effectiveCost);
      document.getElementById('overview-cost').textContent = formatOverviewCost(cost.effectiveCost);
      document.getElementById('overview-cost-detail').textContent = cost.hasMultiplier
        ? overviewText(
          'index.overview.standardCost',
          `原价\u00a0${formatOverviewCost(cost.standardCost)}`,
          { cost: formatOverviewCost(cost.standardCost) }
        )
        : '';
    }

    function tokenMixShare(totals, value) {
      return totals.totalTokens > 0 ? value / totals.totalTokens : 0;
    }

    // 图例只列名称与占比，用量在悬停/点击的提示框里给出
    function renderTokenMix(totals) {
      const bar = document.getElementById('token-mix-bar');
      const legend = document.getElementById('token-mix-legend');
      if (!bar || !legend) return;
      overviewTotals = totals;
      const segments = [];
      legend.innerHTML = TOKEN_MIX_PARTS.map(part => {
        const value = totals[part.key];
        const share = tokenMixShare(totals, value);
        if (share > 0) {
          segments.push(`<span class="token-mix-segment token-mix-segment--${part.tone}" data-part="${part.tone}" style="flex:${share.toFixed(4)} 1 0;"></span>`);
        }
        return `<li class="token-mix-item" data-part="${part.tone}">
          <span class="token-mix-dot token-mix-dot--${part.tone}" aria-hidden="true"></span>
          <span class="token-mix-label">${escapeHtml(overviewText(part.labelKey, part.fallback))}</span>
          <span class="token-mix-share">${formatPercent(share)}</span>
        </li>`;
      }).join('');
      bar.innerHTML = segments.join('');
      renderTokenMixActive();
    }

    // 选中分段时横条与图例同时高亮该分段，其余淡化
    function renderTokenMixActive() {
      const container = document.getElementById('token-mix');
      if (!container) return;
      container.toggleAttribute('data-active', Boolean(tokenMixActive));
      for (const element of container.querySelectorAll('[data-part]')) {
        element.classList.toggle('is-active', element.dataset.part === tokenMixActive);
      }
    }

    // 选中分段的名称、用量与占比，复用迷你趋势的提示框
    function showTokenMixTooltip(target) {
      const part = TOKEN_MIX_PARTS.find(item => item.tone === target.dataset.part);
      const container = target.closest('.overview-kpis');
      const tooltip = document.getElementById('overview-spark-tooltip');
      const timeElement = document.getElementById('overview-spark-tooltip-time');
      const valueElement = document.getElementById('overview-spark-tooltip-value');
      if (!part || !overviewTotals || !container || !tooltip || !timeElement || !valueElement) return;
      hideSparkTooltip();
      const value = overviewTotals[part.key];
      timeElement.hidden = false;
      timeElement.textContent = overviewText(part.labelKey, part.fallback);
      valueElement.textContent = `${formatNumber(value)} · ${formatPercent(tokenMixShare(overviewTotals, value))}`;
      tooltip.hidden = false;
      placeOverviewTooltip(tooltip, container, target.getBoundingClientRect());
    }

    // 悬停或点击（触屏）色块/图例行选中分段，移出横条与图例取消选中
    function bindTokenMix() {
      const container = document.getElementById('token-mix');
      if (!container) return;
      const selectPart = event => {
        const target = event.target.closest('[data-part]');
        const next = target ? target.dataset.part : '';
        if (next === tokenMixActive) return;
        tokenMixActive = next;
        renderTokenMixActive();
        if (target) showTokenMixTooltip(target);
        else hideSparkTooltip();
      };
      container.addEventListener('mouseover', selectPart);
      container.addEventListener('click', selectPart);
      container.addEventListener('mouseleave', () => {
        tokenMixActive = '';
        renderTokenMixActive();
        hideSparkTooltip();
      });
    }

    function protocolIconHtml(row) {
      return `<span class="usage-avatar usage-avatar--${row.key}" aria-hidden="true"><svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor">${row.icon}</svg></span>`;
    }

    // 无提供商标志时显示名称首字母，统一用中性底色
    function authTypeIconHtml(authType, label) {
      const icon = typeof window.channelProviderIconSVG === 'function' ? window.channelProviderIconSVG(authType) : '';
      const content = icon || escapeHtml((label.trim().charAt(0) || '?').toUpperCase());
      return `<span class="usage-avatar channel-provider-avatar${icon ? '' : ' usage-avatar--letter'}" data-auth-type="${escapeHtml(authType)}" aria-hidden="true">${content}</span>`;
    }

    function summaryUsageRow(row, label, stat) {
      return {
        ...row,
        label,
        requests: toNumber(stat && stat.total_requests),
        tokens: toNumber(stat && stat.total_input_tokens) + toNumber(stat && stat.total_output_tokens)
          + toNumber(stat && stat.total_cache_read_tokens) + toNumber(stat && stat.total_cache_creation_tokens),
        cost: effectiveCostOf(stat)
      };
    }

    // 摘要的模型/渠道汇总 → 排行行；已删除的渠道没有名称，以 #ID 显示。
    // 隐藏渠道的 API 令牌会话不返回 by_channel，渠道排行为空。
    function buildSummaryUsageRows(summary) {
      const models = Array.isArray(summary && summary.by_model) ? summary.by_model : [];
      const channels = Array.isArray(summary && summary.by_channel) ? summary.by_channel : [];
      return {
        model: models.map(stat => summaryUsageRow({ key: String(stat.model) }, String(stat.model), stat)),
        channel: channels.map(stat => summaryUsageRow(
          { key: String(stat.channel_id), authType: stat.auth_type || '' },
          stat.channel_name || `#${stat.channel_id}`,
          stat
        ))
      };
    }

    // 按指标取前 N 名；占比以该维度全部行之和为分母
    function rankUsageRows(rows, metric, limit = USAGE_RANK_LIMIT) {
      const total = rows.reduce((sum, row) => sum + row[metric], 0);
      return rows
        .filter(row => row[metric] > 0)
        .sort((left, right) => right[metric] - left[metric])
        .slice(0, limit)
        .map(row => ({ ...row, share: row[metric] / total }));
    }

    function formatUsageMetric(value) {
      return usageRankMetric === 'cost' ? formatCost(value) : formatNumber(value);
    }

    function renderUsageRank(listId, rows, iconOf) {
      const list = document.getElementById(listId);
      if (!list) return;
      const ranked = rankUsageRows(rows, usageRankMetric);
      if (ranked.length === 0) {
        list.innerHTML = `<li class="usage-rank-empty">${escapeHtml(overviewText('index.rank.empty', '暂无数据'))}</li>`;
        return;
      }
      // 行底色按占比填充
      list.innerHTML = ranked.map((row, index) => `<li class="usage-rank-item" style="--usage-share:${(row.share * 100).toFixed(1)}%;">
          <span class="usage-rank-index">${index + 1}</span>
          <span class="usage-rank-name" title="${escapeHtml(row.label)}">${iconOf ? iconOf(row) : ''}<span class="usage-rank-label">${escapeHtml(row.label)}</span></span>
          <span class="usage-rank-value">${escapeHtml(formatUsageMetric(row[usageRankMetric]))}</span>
          <span class="usage-rank-share">${formatPercent(row.share)}</span>
        </li>`).join('');
    }

    function renderUsageRanks() {
      const protocolStats = statsData.by_client_protocol || {};
      const usageRows = buildSummaryUsageRows(statsData);
      renderUsageRank('usage-rank-model', usageRows.model);
      renderUsageRank('usage-rank-channel', usageRows.channel,
        row => authTypeIconHtml(row.authType, row.label));
      renderUsageRank('usage-rank-protocol',
        PROTOCOL_USAGE_ROWS.map(row => summaryUsageRow(row, row.label, protocolStats[row.key])),
        protocolIconHtml);
      renderUsageRank('usage-rank-auth-type',
        Object.entries(statsData.by_auth_type || {}).map(([authType, stat]) => {
          const labelKey = AUTH_TYPE_LABEL_KEYS[authType];
          return summaryUsageRow({ key: authType }, labelKey ? overviewText(labelKey, authType) : authType, stat);
        }),
        row => authTypeIconHtml(row.key, row.label));
    }

    function bindUsageRankMetric() {
      const group = document.getElementById('usage-rank-metric');
      if (!group) return;
      group.addEventListener('click', event => {
        const button = event.target.closest('.toggle-btn[data-metric]');
        if (!button || button.dataset.metric === usageRankMetric) return;
        usageRankMetric = button.dataset.metric;
        for (const item of group.querySelectorAll('.toggle-btn[data-metric]')) {
          const active = item === button;
          item.classList.toggle('active', active);
          item.setAttribute('aria-pressed', String(active));
        }
        renderUsageRanks();
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

    function serviceHealthRateLimitedText(count) {
      return overviewText('index.health.rateLimited', `其中限流\u00a0${count}`, { count: formatNumber(count) });
    }

    function hideServiceHealthTooltip() {
      const tooltip = document.getElementById('service-health-tooltip');
      if (tooltip) tooltip.hidden = true;
    }

    function showServiceHealthTooltip(cell, point, formatter, bucketMs) {
      const plot = cell.closest('.service-health-plot');
      const tooltip = document.getElementById('service-health-tooltip');
      const timeElement = document.getElementById('service-health-tooltip-time');
      const successElement = document.getElementById('service-health-tooltip-success');
      const errorElement = document.getElementById('service-health-tooltip-error');
      const rateElement = document.getElementById('service-health-tooltip-rate');
      if (!plot || !tooltip || !timeElement || !successElement || !errorElement || !rateElement) return;

      const intervalMs = bucketMs || 15 * 60 * 1000;
      timeElement.textContent = `${formatter.format(new Date(point.ts))} – ${formatter.format(new Date(point.ts + intervalMs))}`;
      const pendingElement = document.getElementById('service-health-tooltip-pending');
      const statsElement = document.getElementById('service-health-tooltip-stats');
      if (pendingElement && statsElement) {
        pendingElement.hidden = !point.pending;
        pendingElement.textContent = point.pending ? overviewText('index.health.pending', '未到') : '';
        statsElement.hidden = Boolean(point.pending);
      }
      successElement.textContent = formatNumber(point.success);
      errorElement.textContent = formatNumber(point.error);
      rateElement.textContent = `(${formatPercent(point.rate)})`;
      const rateLimitedElement = document.getElementById('service-health-tooltip-rate-limited');
      if (rateLimitedElement) {
        rateLimitedElement.hidden = !(point.rateLimited > 0);
        rateLimitedElement.textContent = point.rateLimited > 0 ? serviceHealthRateLimitedText(point.rateLimited) : '';
      }

      tooltip.hidden = false;
      placeOverviewTooltip(tooltip, plot, cell.getBoundingClientRect());
    }

    // 提示框放在锚点（视口坐标）上方，空间不足时放下方；水平居中并限制在定位容器内
    function placeOverviewTooltip(tooltip, container, anchor) {
      tooltip.dataset.placement = 'top';
      const containerRect = container.getBoundingClientRect();
      const tooltipRect = tooltip.getBoundingClientRect();
      const anchorCenter = anchor.left - containerRect.left + anchor.width / 2;
      const inset = 8;
      const maxLeft = Math.max(inset, containerRect.width - tooltipRect.width - inset);
      const left = Math.min(Math.max(anchorCenter - tooltipRect.width / 2, inset), maxLeft);
      // 上方空间按视口计算，扣除固定顶栏
      const topbar = document.querySelector('.topbar');
      const roomAbove = anchor.top - (topbar ? topbar.getBoundingClientRect().bottom : 0);
      let top = anchor.top - containerRect.top - tooltipRect.height - 12;

      if (roomAbove < tooltipRect.height + 16) {
        top = anchor.bottom - containerRect.top + 12;
        tooltip.dataset.placement = 'bottom';
      }

      tooltip.style.left = `${left}px`;
      tooltip.style.top = `${top}px`;
      const arrowX = Math.min(Math.max(anchorCenter - left, 12), tooltipRect.width - 12);
      tooltip.style.setProperty('--service-health-tooltip-arrow-x', `${arrowX}px`);
    }

    function renderServiceHealth(model) {
      const grid = document.getElementById('service-health-grid');
      const message = document.getElementById('service-health-message');
      if (!grid || !message || !model) return;

      hideServiceHealthTooltip();
      const timeFormatter = serviceHealthTimeFormatter();
      const fragment = document.createDocumentFragment();
      for (const [index, point] of model.points.entries()) {
        const cell = document.createElement('span');
        cell.className = `service-health-cell ${point.state}${point.pending ? ' pending' : ''}`;
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
      const rate = formatPercent(model.rate);
      const period = serviceHealthPeriodText();
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
      // 请求卡副行在成功/失败后补限流次数（取自分桶数据）
      const rateDetail = document.getElementById('overview-rate-detail');
      if (rateDetail) {
        rateDetail.textContent = model.rateLimited > 0
          ? ` · ${overviewText('index.overview.rateLimited', `限流\u00a0${model.rateLimited}`, { count: formatNumber(model.rateLimited) })}`
          : '';
      }
      message.hidden = true;
      message.textContent = '';
    }

    function renderServiceHealthUnavailable() {
      const message = document.getElementById('service-health-message');
      const rateDetail = document.getElementById('overview-rate-detail');
      if (rateDetail) rateDetail.textContent = '';
      if (message) {
        message.hidden = false;
        message.textContent = overviewText(
          'index.health.unavailable',
          '健康数据暂时无法加载，将在下次刷新时重试。'
        );
        const retryButton = document.createElement('button');
        retryButton.type = 'button';
        retryButton.className = 'btn btn-secondary btn-sm load-retry-btn';
        retryButton.textContent = overviewText('common.retry', '重试');
        retryButton.addEventListener('click', loadDashboard);
        message.appendChild(retryButton);
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
      const [summaryResult, healthResult] = await Promise.allSettled([
        fetchDataWithAuth(`/dashboard/summary?${dateRangeQuery}`),
        healthRequest
          ? fetchDataWithAuth(`/dashboard/metrics?${healthRequest.query}`)
          : Promise.reject(new Error('ServiceHealth unavailable'))
      ]);

      if (generation !== dashboardLoadGeneration) return;
      dashboardUpdatedAt = new Date();

      if (summaryResult.status === 'fulfilled') {
        statsData = summaryResult.value || statsData;
      } else {
        console.error('Failed to load stats:', summaryResult.reason);
        showError(overviewText('index.overview.loadFailed', '无法加载统计数据'));
      }
      updateStatsDisplay();

      if (healthResult.status === 'fulfilled') {
        renderSparklines(buildSparkSeries(healthResult.value, healthRequest.bucketMinutes));
        serviceHealthModel = window.ServiceHealth.buildModel(
          healthResult.value,
          healthRequest.bucketMinutes,
          window.ServiceHealth.rangeEndMs(currentTimeRange, currentCustomTimeRange)
        );
        renderServiceHealth(serviceHealthModel);
      } else {
        console.error('Failed to load service health:', healthResult.reason);
        renderSparklines(null);
        renderServiceHealthUnavailable();
      }

      renderUpdatedAt();
      if (refreshButton) refreshButton.setAttribute('aria-busy', 'false');
      if (grid) grid.setAttribute('aria-busy', 'false');
    }

    // 页头显示最近一次加载完成的时刻，自动刷新时可确认数据新鲜度
    function renderUpdatedAt() {
      const element = document.getElementById('overview-updated');
      if (!element) return;
      const time = new Intl.DateTimeFormat(serviceHealthLocale(), {
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
        hourCycle: 'h23'
      }).format(dashboardUpdatedAt);
      element.textContent = overviewText('index.overview.updatedAt', `${time} 更新`, { time });
    }

    // 更新统计显示
    function updateStatsDisplay() {
      const totals = buildOverviewTotals(statsData);
      renderOverviewKpis(totals);
      renderTokenMix(totals);
      renderUsageRanks();
    }

    // 通知系统统一由 ui.js 提供（showSuccess/showError/showNotification）

    // 注销功能（已由 ui.js 的 onLogout 统一处理）

    // 自动刷新由 createAutoRefresh 统一管理（system_settings.auto_refresh_interval_seconds）

    function getIndexFilters() {
      const hasCustomRange = currentTimeRange === 'custom' && currentCustomTimeRange;
      return {
        range: currentTimeRange,
        customStartTime: hasCustomRange ? String(currentCustomTimeRange.startMs) : '',
        customEndTime: hasCustomRange ? String(currentCustomTimeRange.endMs) : ''
      };
    }

    // 解析 URL/本地保存的时间范围；非法值或残缺的自定义区间回落到"本日"
    function resolveIndexTimeRange(values) {
      const range = INDEX_RANGE_VALUES.includes(values?.range) ? values.range : 'today';
      if (range !== 'custom') return { range, customRange: null };
      const startMs = Number(values.customStartTime);
      const endMs = Number(values.customEndTime);
      if (!Number.isFinite(startMs) || !Number.isFinite(endMs) || endMs <= startMs) {
        return { range: 'today', customRange: null };
      }
      return { range, customRange: { startMs: Math.trunc(startMs), endMs: Math.trunc(endMs) } };
    }

    function restoreIndexTimeRange() {
      if (!window.FilterState) return;
      const restored = resolveIndexTimeRange(window.FilterState.restore({
        search: location.search,
        savedFilters: window.FilterState.load(INDEX_FILTER_KEY),
        fields: INDEX_FILTER_FIELDS
      }));
      currentTimeRange = restored.range;
      currentCustomTimeRange = restored.customRange;
    }

    function persistIndexTimeRange() {
      window.persistFilterState({
        key: INDEX_FILTER_KEY,
        values: getIndexFilters(),
        pathname: location.pathname,
        fields: INDEX_FILTER_FIELDS,
        historyMethod: 'replaceState'
      });
    }

    if (typeof module !== 'undefined' && module.exports) {
      module.exports = { buildOverviewTotals, buildSparkSeries, buildSummaryUsageRows, rankUsageRows, resolveIndexTimeRange };
    }

    // 页面初始化
    if (typeof window !== 'undefined') window.initPageBootstrap({
      topbarKey: 'index',
      run: () => {
      restoreIndexTimeRange();
      window.bindTimeRangeSelector({
        containerId: 'index-time-range',
        values: INDEX_RANGE_VALUES,
        initialValue: currentTimeRange,
        customRange: currentCustomTimeRange,
        onChange: (range, customRange) => {
          currentTimeRange = range;
          if (range === 'custom') currentCustomTimeRange = customRange;
          persistIndexTimeRange();
          loadDashboard();
        }
      });

      bindUsageRankMetric();
      bindTokenMix();
      bindSparklines();

      // 摘要（含排行）与服务健康检测共用同一日期范围快照。
      loadDashboard();
      document.getElementById('overview-refresh')?.addEventListener('click', loadDashboard);

      if (window.i18n && typeof window.i18n.onLocaleChange === 'function') {
        window.i18n.onLocaleChange(() => {
          updateStatsDisplay();
          if (serviceHealthModel) renderServiceHealth(serviceHealthModel);
          if (dashboardUpdatedAt) renderUpdatedAt();
        });
      }

      // 自动刷新（system_settings.auto_refresh_interval_seconds，0=禁用）
      if (typeof window.createAutoRefresh === 'function') {
        window.createAutoRefresh({ load: loadDashboard }).init();
      }
      }
    });
