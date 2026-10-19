    // 常量定义
    const t = window.t;
    const STATS_TABLE_COLUMNS = 11; // 统计表可见列数：渠道/模型层级列（隐藏渠道时为模型列）+ 10 个指标列
    const STATS_CHEVRON_SVG = '<svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" aria-hidden="true"><path stroke-linecap="round" stroke-linejoin="round" d="M6 9l6 6 6-6"/></svg>';

    let statsData = null;
    let rpmStats = null; // 全局RPM统计（峰值、平均、最近一分钟）
    let isToday = true;  // 是否为本日（本日才显示最近一分钟）
    let durationSeconds = 0; // 时间跨度（秒），用于计算RPM
    let currentStatsCustomTimeRange = null;
    let authTokens = []; // 令牌列表
    let hideZeroSuccess = true; // 是否隐藏0成功的模型（默认开启）
    let statsChannelNameOptions = []; // 从统计数据中提取的渠道名列表
    let statsModelOptions = []; // 从统计数据中提取的模型列表
    let statsChannelNameCombobox = null; // 渠道名筛选组合框实例
    let statsModelCombobox = null; // 模型筛选组合框实例
    let statsExactChannelNameValue = '';
    let statsExactModelValue = '';
    let sortState = {
      column: null,
      order: null // null, 'asc', 'desc'
    };
    const collapsedStatsChannels = new Set(); // 已折叠的渠道 ID（字符串），跨刷新/排序保留

    function normalizeStatsFilterValue(value) {
      return String(value || '').trim().toLowerCase();
    }

    function statsFilterMatchesOption(value, options) {
      const normalizedValue = normalizeStatsFilterValue(value);
      if (!normalizedValue) return false;

      return (Array.isArray(options) ? options : []).some((option) => {
        const candidates = option && typeof option === 'object'
          ? [option.value, option.label]
          : [option];
        return candidates.some((candidate) => normalizeStatsFilterValue(candidate) === normalizedValue);
      });
    }

    function statsFilterMatchesExactValue(value, exactValue) {
      const normalizedValue = normalizeStatsFilterValue(value);
      return Boolean(normalizedValue) && normalizedValue === normalizeStatsFilterValue(exactValue);
    }

    function isExactStatsChannelNameFilter(value) {
      return statsFilterMatchesOption(value, statsChannelNameOptions) ||
        statsFilterMatchesExactValue(value, statsExactChannelNameValue);
    }

    function isExactStatsModelFilter(value) {
      return statsFilterMatchesOption(value, statsModelOptions) ||
        statsFilterMatchesExactValue(value, statsExactModelValue);
    }

    function getStatsChannelNameFilterKey(value, values) {
      return (values && values.channelNameExact) || isExactStatsChannelNameFilter(value)
        ? 'channel_name'
        : 'channel_name_like';
    }

    function getStatsModelFilterKey(value, values) {
      return (values && values.modelExact) || isExactStatsModelFilter(value) ? 'model' : 'model_like';
    }

    function rememberExactStatsFilters(filters = {}, urlParams = null) {
      const hasExactChannelName = urlParams
        ? urlParams.has('channel_name')
        : filters.channelNameExact === true;
      const hasExactModel = urlParams
        ? urlParams.has('model')
        : filters.modelExact === true;

      statsExactChannelNameValue = hasExactChannelName ? (filters.channelName || '') : '';
      statsExactModelValue = hasExactModel ? (filters.model || '') : '';
    }

    function normalizeStatsCustomTimeRange(range) {
      if (!range || typeof range !== 'object') return null;

      const startMs = Number(range.startMs ?? range.customStartTime);
      const endMs = Number(range.endMs ?? range.customEndTime);
      if (!Number.isFinite(startMs) || !Number.isFinite(endMs) || endMs <= startMs) {
        return null;
      }
      return {
        startMs: Math.trunc(startMs),
        endMs: Math.trunc(endMs),
        label: range.label || ''
      };
    }

    function appendStatsTimeRangeParams(params, filters) {
      const range = filters?.range || 'today';
      const query = typeof window.buildDateRangeQuery === 'function'
        ? window.buildDateRangeQuery(range, currentStatsCustomTimeRange)
        : `range=${encodeURIComponent(range)}`;
      new URLSearchParams(query).forEach((value, key) => {
        params.set(key, value);
      });
      return params;
    }

    function buildStatsLogLinkParams(baseParams = {}) {
      const params = new URLSearchParams();
      Object.entries(baseParams).forEach(([key, value]) => {
        if (value !== undefined && value !== null && value !== '') {
          params.set(key, value);
        }
      });
      return appendStatsTimeRangeParams(params, getStatsFilters());
    }

    let statsLoadSeq = 0;

    // background=true 为自动刷新：不显示加载态、失败时保留现有内容，并恢复滚动位置
    async function loadStats(options = {}) {
      const background = options.background === true && Boolean(statsData);
      const seq = ++statsLoadSeq;
      try {
        if (!background) renderStatsLoading();

        const params = buildStatsRequestParams();
        params.set('health_timeline', '1');
        // 后端返回格式: {"success":true,"data":{"stats":[...],"duration_seconds":...,"rpm_stats":{...},"is_today":...}}
        const nextStatsData = (await fetchDataWithAuth('/dashboard/stats?' + params.toString())) || { stats: [] };
        // 筛选连续变化时丢弃过期响应
        if (seq !== statsLoadSeq) return;
        statsData = nextStatsData;
        durationSeconds = statsData.duration_seconds || 1; // 防止除零
        rpmStats = statsData.rpm_stats || null;
        isToday = statsData.is_today !== false;
        populateStatsComboboxOptions();

        const scrollState = background ? captureStatsScroll() : null;
        renderStatsTable();
        updateRpmHeader(); // 更新表头标题

        // 如果当前是图表视图，同步更新图表
        if (currentView === 'chart') {
          renderCharts();
        }
        if (scrollState) restoreStatsScroll(scrollState);

      } catch (error) {
        if (seq !== statsLoadSeq) return;
        console.error('Failed to load stats:', error);
        // 后台刷新失败保留已有数据，等待下一轮
        if (background) return;
        if (window.showError) try { window.showError(t('stats.loadFailed')); } catch(_){}
        renderStatsError();
      }
    }

    // 表格容器可横向滚动，整体重渲染前后保存/恢复窗口与容器的滚动位置
    function captureStatsScroll() {
      const container = document.getElementById('stats-table-view');
      return {
        x: window.scrollX,
        y: window.scrollY,
        left: container ? container.scrollLeft : 0,
        top: container ? container.scrollTop : 0
      };
    }

    function restoreStatsScroll(state) {
      const container = document.getElementById('stats-table-view');
      if (container) {
        container.scrollLeft = state.left;
        container.scrollTop = state.top;
      }
      if (window.scrollX !== state.x || window.scrollY !== state.y) {
        window.scrollTo(state.x, state.y);
      }
    }

    // 模板不在文档树内，translatePage 覆盖不到，文本需在渲染时传入
    function renderStatsStateRow(templateId, data) {
      const tbody = document.getElementById('stats_tbody');
      tbody.innerHTML = '';
      const row = TemplateEngine.render(templateId, { colspan: STATS_TABLE_COLUMNS, ...data });
      if (row) tbody.appendChild(row);
      syncStatsToggleAll();
    }

    function renderStatsLoading() {
      renderStatsStateRow('tpl-stats-loading', { loadingText: t('stats.loading') });
    }

    function renderStatsError() {
      renderStatsStateRow('tpl-stats-error', {
        title: t('stats.loadFailed'),
        message: t('stats.checkNetwork'),
        retryText: t('common.retry')
      });
    }

    function renderStatsEmpty() {
      renderStatsStateRow('tpl-stats-empty', {
        title: t('stats.noData'),
        message: t('stats.adjustFilter')
      });
    }

    // 表格排序功能
    function sortTable(column) {
      if (!statsData || !statsData.stats || statsData.stats.length === 0) return;
      
      // 确定排序状态：null -> desc -> asc -> null (三态循环)
      let newOrder;
      if (sortState.column !== column) {
        // 切换到新列，从desc开始
        newOrder = 'desc';
      } else {
        // 同一列循环：null -> desc -> asc -> null
        if (sortState.order === null) {
          newOrder = 'desc';
        } else if (sortState.order === 'desc') {
          newOrder = 'asc';
        } else {
          newOrder = null;
        }
      }
      
      // 更新排序状态
      sortState.column = newOrder ? column : null;
      sortState.order = newOrder;
      
      // 更新表头样式
      updateSortHeaders();
      
      // 排序在渲染时进行（分组视图先排渠道，再排组内模型）
      renderStatsTable();
    }

    function updateSortHeaders() {
      // 清除所有列的排序样式
      document.querySelectorAll('.sortable').forEach(th => {
        th.classList.remove('sorted');
        th.removeAttribute('data-sort-order');
      });
      
      // 如果有排序状态，设置当前列的样式
      if (sortState.column && sortState.order) {
        const currentHeader = document.querySelector(`[data-column="${sortState.column}"]`);
        if (currentHeader) {
          currentHeader.classList.add('sorted');
          currentHeader.setAttribute('data-sort-order', sortState.order);
        }
      }
    }

    // 默认排序：渠道优先级降序 → 渠道名称升序 → 模型名称升序
    function compareStatsDefault(a, b) {
      const priorityA = a.channel_priority ?? 0;
      const priorityB = b.channel_priority ?? 0;
      if (priorityA !== priorityB) return priorityB - priorityA;

      const channelA = (a.channel_name || '').toLowerCase();
      const channelB = (b.channel_name || '').toLowerCase();
      const channelCompare = channelA.localeCompare(channelB, 'zh-CN');
      if (channelCompare !== 0) return channelCompare;

      const modelA = (a.model || '').toLowerCase();
      const modelB = (b.model || '').toLowerCase();
      return modelA.localeCompare(modelB, 'zh-CN');
    }

    function getStatsSortValue(entry, column) {
      switch (column) {
        case 'channel_name':
          return (entry.channel_name || '').toLowerCase();
        case 'model':
          return (entry.model || '').toLowerCase();
        case 'success':
          return entry.success || 0;
        case 'rpm':
          // 使用后端计算的峰值RPM排序
          return entry.peak_rpm || 0;
        case 'success_rate':
          return entry.total > 0 ? (entry.success / entry.total) : 0;
        case 'avg_first_byte_time':
          // 优先按平均耗时排序，其次按平均首字时间
          return entry.avg_duration_seconds || entry.avg_first_byte_time_seconds || 0;
        case 'avg_speed':
          return getStatsEntrySpeed(entry) || 0;
        case 'total_input_tokens':
          return entry.total_input_tokens || 0;
        case 'total_output_tokens':
          return entry.total_output_tokens || 0;
        case 'total_cache_read':
          return entry.total_cache_read_input_tokens || 0;
        case 'total_cache_creation':
          return entry.total_cache_creation_input_tokens || 0;
        case 'total_cost':
          return entry.total_cost || 0;
        default:
          return 0;
      }
    }

    // 渠道行与模型行共用：按当前表头排序，未选排序或值相同时回落默认排序
    function compareStatsEntries(a, b) {
      if (!sortState.column || !sortState.order) return compareStatsDefault(a, b);

      const valueA = getStatsSortValue(a, sortState.column);
      const valueB = getStatsSortValue(b, sortState.column);
      const result = typeof valueA === 'string'
        ? valueA.localeCompare(valueB, 'zh-CN')
        : valueA - valueB;
      return (sortState.order === 'asc' ? result : -result) || compareStatsDefault(a, b);
    }

    function calculateAverageSpeed(entry) {
      const successCount = Number(entry?.success);
      if (!Number.isFinite(successCount) || successCount <= 0) {
        return null;
      }

      return calculateTokenSpeed(
        Number(entry?.total_output_tokens) / successCount,
        Number(entry?.avg_duration_seconds),
        Number(entry?.avg_first_byte_time_seconds)
      );
    }

    // 渠道行/合计行由 StatsGrouping 按总输出/总生成秒数算好；单条统计按均值推算
    function getStatsEntrySpeed(entry) {
      return entry.aggregate_speed !== undefined ? entry.aggregate_speed : calculateAverageSpeed(entry);
    }

    function buildStatsTimingValue(seconds, color) {
      return `<span class="stats-value-dynamic" style="--stats-accent:${color};">${seconds.toFixed(2)}</span>`;
    }

    function buildStatsTimingText(firstByteSeconds, durationSeconds) {
      const firstByte = Number(firstByteSeconds) || 0;
      const duration = Number(durationSeconds) || 0;
      const parts = [];

      if (firstByte <= 0 && duration <= 0) return '';

      // 两段始终同时出现，缺失的一段用占位符，避免单个数值分不清是首字还是耗时
      const placeholder = '<span class="stats-value-muted">—</span>';
      parts.push(firstByte > 0
        ? buildStatsTimingValue(firstByte, window.getFirstByteTimingColor(firstByte))
        : placeholder);
      parts.push('<span class="stats-timing-separator">/</span>');
      parts.push(duration > 0
        ? buildStatsTimingValue(duration, window.getDurationTimingColor(duration))
        : placeholder);

      return `<span class="stats-timing-inline">${parts.join('')}</span>`;
    }

    function buildCacheUtilRate(inputTokens, cacheReadTokens, cacheCreationTokens) {
      const rate = window.calculateCacheHitRate(inputTokens, cacheReadTokens, cacheCreationTokens);
      if (rate === null) return '';
      return `<span class="stats-value-dynamic" style="--stats-accent:${window.getCacheHitRateColor(rate)};">${window.formatPercent(rate)}</span>`;
    }

    function buildStatsModelLink(entry, className) {
      if (!entry.model) {
        return `<span class="stats-value-muted">${escapeHtml(t('stats.unknownModel'))}</span>`;
      }
      return `<a href="#" class="${className} model-link" data-model="${escapeHtml(entry.model)}" data-channel-name="${escapeHtml(entry.channel_name)}" title="${escapeHtml(t('stats.viewLogsTitle'))}">${escapeHtml(entry.model)}</a>`;
    }

    // 隐藏渠道时的平铺模型列
    function buildStatsModelDisplay(entry) {
      const modelLink = buildStatsModelLink(entry, 'model-tag');
      if (!entry.model) return modelLink;
      return `<span class="stats-model-cell">${modelLink}${buildCornerMultiplierBadge(entry.cost_multiplier_min, entry.cost_multiplier_max)}</span>`;
    }

    // 渠道行首列：折叠按钮 + 头像 + 名称/倍率 + 渠道健康条；单模型渠道同样展开子行，结构统一
    function buildStatsChannelCell(group, collapsed) {
      const name = group.channel_name || '';
      const authType = group.channel_auth_type || 'api_key';
      const toggle = `<button type="button" class="stats-tree-toggle" aria-expanded="${!collapsed}" aria-label="${escapeHtml(t('stats.toggleChannelModels', { name }))}">${STATS_CHEVRON_SVG}</button>`;
      const multiplier = buildCornerMultiplierBadge(group.cost_multiplier_min, group.cost_multiplier_max);
      const successRate = group.total > 0 ? group.success / group.total : 0;

      return `<div class="stats-tree">${toggle}` +
        `<span class="stats-channel-avatar channel-provider-avatar" data-auth-type="${escapeHtml(authType)}" aria-hidden="true">${window.channelAvatarContentHTML(authType, name)}</span>` +
        '<div class="stats-tree-body"><div class="stats-tree-title">' +
        `<a href="#" class="config-name channel-link" data-channel-name="${escapeHtml(name)}" title="${escapeHtml(t('stats.viewChannelLogsTitle'))}">${escapeHtml(name)}</a>` +
        `${multiplier ? `<span class="stats-tree-meta">${multiplier}</span>` : ''}</div>` +
        `${buildHealthIndicator(group.health_timeline, successRate)}</div></div>`;
    }

    // 指标列（成功/耗时/Tok/s/RPM/Token/缓存/费用）的模板字段，渠道行、模型行、合计行共用
    function buildStatsMetricFields(entry, rpmHtml, inlineCost) {
      const successRate = entry.total > 0 ? (entry.success / entry.total) * 100 : 0;
      const speed = getStatsEntrySpeed(entry);
      const timingText = buildStatsTimingText(entry.avg_first_byte_time_seconds, entry.avg_duration_seconds);
      const speedText = speed === null
        ? ''
        : `<span class="stats-value-dynamic" style="--stats-accent:${window.getSpeedColor(speed)};">${speed >= 100 ? speed.toFixed(0) : speed.toFixed(1)}</span>`;
      const tokenText = (value, type) => (value ? `<span class="token-type--${type}">${formatNumber(value)}</span>` : '');
      const inputText = entry.total_input_tokens ? formatNumber(entry.total_input_tokens) : '';
      const outputText = entry.total_output_tokens ? formatNumber(entry.total_output_tokens) : '';
      const cacheReadText = tokenText(entry.total_cache_read_input_tokens, 'cache-read');
      const cacheCreateText = tokenText(entry.total_cache_creation_input_tokens, 'cache-create');
      const cacheUtilText = buildCacheUtilRate(
        entry.total_input_tokens,
        entry.total_cache_read_input_tokens,
        entry.total_cache_creation_input_tokens
      );
      const costText = buildCostStackHtml(entry.total_cost, entry.effective_cost, { tone: 'warning', inline: inlineCost });
      const emptyClass = text => (text ? '' : 'mobile-empty-cell');

      return {
        successDisplay: buildSuccessDisplay(
          formatNumber(entry.success || 0),
          formatSuccessRateText(successRate, entry.total || 0),
          window.getSuccessRateColor(successRate / 100)
        ),
        rpm: rpmHtml,
        avgFirstByteTime: timingText,
        timingCellClass: emptyClass(timingText),
        avgSpeed: speedText,
        speedCellClass: emptyClass(speedText),
        inputTokens: inputText,
        inputCellClass: emptyClass(inputText),
        outputTokens: outputText,
        outputCellClass: emptyClass(outputText),
        cacheReadTokens: cacheReadText,
        cacheReadCellClass: emptyClass(cacheReadText),
        cacheCreationTokens: cacheCreateText,
        cacheCreateCellClass: emptyClass(cacheCreateText),
        cacheUtilText,
        cacheUtilCellClass: emptyClass(cacheUtilText),
        costText,
        costCellClass: emptyClass(costText),
        mobileLabelSuccess: t('common.success'),
        mobileLabelTiming: t('stats.avgFirstByte'),
        mobileLabelSpeed: t('stats.avgSpeed'),
        mobileLabelRpm: t('stats.rpm'),
        mobileLabelInput: t('stats.inputTokens'),
        mobileLabelOutput: t('stats.outputTokens'),
        mobileLabelCacheRead: t('stats.cacheRead'),
        mobileLabelCacheCreate: t('stats.cacheCreation'),
        mobileLabelCacheUtil: t('stats.cacheUtil'),
        mobileLabelCost: t('stats.costUsd')
      };
    }

    // 渠道可见时按渠道分组（渠道行汇总 + 可折叠的模型子行）；隐藏渠道时退回按模型平铺
    function renderStatsTable() {
      const tbody = document.getElementById('stats_tbody');

      if (!statsData || !statsData.stats || statsData.stats.length === 0) {
        renderStatsEmpty();
        return;
      }

      // 根据 hideZeroSuccess 过滤数据
      const filteredStats = hideZeroSuccess
        ? statsData.stats.filter(entry => (entry.success || 0) > 0)
        : statsData.stats;

      if (filteredStats.length === 0) {
        renderStatsEmpty();
        return;
      }

      const grouped = !window.shouldHideChannels?.();
      const fragment = document.createDocumentFragment();
      const appendRow = (data) => {
        const row = TemplateEngine.render('tpl-stats-row', data);
        if (row) fragment.appendChild(row);
        return row;
      };

      if (grouped) {
        const groups = window.StatsGrouping.groupStatsByChannel(filteredStats).sort(compareStatsEntries);
        for (const group of groups) {
          const channelKey = String(group.channel_id ?? '');
          const collapsed = collapsedStatsChannels.has(channelKey);

          appendRow({
            rowClass: 'stats-data-row stats-channel-row',
            channelId: channelKey,
            labelCellClass: 'stats-col-tree mobile-card-no-label',
            labelCellMobileLabel: t('stats.channelName'),
            labelCell: buildStatsChannelCell(group, collapsed),
            ...buildStatsMetricFields(group, formatEntryRpm(group, isToday), true)
          });

          const models = [...group.models].sort(compareStatsEntries);
          models.forEach((entry, index) => {
            const row = appendRow({
              rowClass: index === models.length - 1 ? 'stats-data-row stats-model-row is-last' : 'stats-data-row stats-model-row',
              channelId: channelKey,
              labelCellClass: 'stats-col-tree mobile-card-no-label',
              labelCellMobileLabel: t('common.model'),
              labelCell: `<div class="stats-tree-model">${buildStatsModelLink(entry, 'stats-tree-model-link')}</div>`,
              ...buildStatsMetricFields(entry, formatEntryRpm(entry, isToday), true)
            });
            if (row) row.hidden = collapsed;
          });
        }
      } else {
        for (const entry of [...filteredStats].sort(compareStatsEntries)) {
          appendRow({
            rowClass: 'stats-data-row',
            channelId: '',
            labelCellClass: 'stats-col-model',
            labelCellMobileLabel: t('common.model'),
            labelCell: buildStatsModelDisplay(entry),
            ...buildStatsMetricFields(entry, formatEntryRpm(entry, isToday), false)
          });
        }
      }

      // 合计行使用全局 rpm_stats（峰值不可由各行相加）
      appendRow({
        rowClass: 'stats-total-row',
        channelId: '',
        labelCellClass: 'stats-col-total-label',
        labelCellMobileLabel: t('stats.total'),
        labelCell: escapeHtml(t('stats.total')),
        ...buildStatsMetricFields(
          window.StatsGrouping.aggregateStatsEntries(filteredStats),
          formatGlobalRpm(rpmStats, isToday),
          grouped
        )
      });

      tbody.innerHTML = '';
      tbody.appendChild(fragment);
      syncStatsToggleAll();
    }

    // 折叠/展开渠道下的模型子行：只切换现有行的 hidden，不整表重渲染
    function setStatsChannelCollapsed(channelRow, collapsed) {
      const channelKey = channelRow.dataset.channelId || '';
      if (collapsed) {
        collapsedStatsChannels.add(channelKey);
      } else {
        collapsedStatsChannels.delete(channelKey);
      }

      channelRow.querySelector('.stats-tree-toggle')?.setAttribute('aria-expanded', String(!collapsed));
      let row = channelRow.nextElementSibling;
      while (row && row.classList.contains('stats-model-row')) {
        row.hidden = collapsed;
        row = row.nextElementSibling;
      }
    }

    function getStatsChannelRows() {
      return [...document.querySelectorAll('#stats_tbody .stats-channel-row')];
    }

    function isStatsChannelExpanded(channelRow) {
      return !collapsedStatsChannels.has(channelRow.dataset.channelId || '');
    }

    function toggleStatsChannel(channelRow) {
      setStatsChannelCollapsed(channelRow, isStatsChannelExpanded(channelRow));
      syncStatsToggleAll();
    }

    // 表头总开关：任一渠道展开时全部收起，否则全部展开
    function toggleAllStatsChannels() {
      const rows = getStatsChannelRows();
      const collapse = rows.some(isStatsChannelExpanded);
      rows.forEach(row => setStatsChannelCollapsed(row, collapse));
      syncStatsToggleAll();
    }

    function syncStatsToggleAll() {
      const button = document.querySelector('.stats-tree-toggle-all');
      if (!button) return;
      const rows = getStatsChannelRows();
      const anyExpanded = rows.some(isStatsChannelExpanded);
      const label = t(anyExpanded ? 'stats.collapseAllChannels' : 'stats.expandAllChannels');
      button.disabled = rows.length === 0;
      button.setAttribute('aria-expanded', String(anyExpanded));
      button.setAttribute('aria-label', label);
      button.title = label;
    }

    function formatSuccessRateText(successRate, totalRequests) {
      if (!(totalRequests > 0)) return '';
      return window.formatPercent(successRate / 100);
    }

    function buildSuccessDisplay(successCountText, successRateText, successRateColor) {
      if (!successRateText) {
        return `<span class="success-count">${successCountText}</span>`;
      }

      return `<span class="stats-success-inline"><span class="success-count">${successCountText}</span><span class="stats-success-separator">/</span><span class="success-rate stats-value-dynamic" style="--stats-accent:${successRateColor};">${successRateText}</span></span>`;
    }

    function applyFilter() {
      window.persistFilterState({
        key: STATS_FILTER_KEY,
        values: getStatsFilters(),
        search: location.search,
        pathname: location.pathname,
        fields: STATS_FILTER_FIELDS,
        preserveExistingParams: true
      });
      loadStats();
    }

    function getDefaultStatsFilters() {
      if (window.FilterState && typeof window.FilterState.restore === 'function') {
        return window.FilterState.restore({
          search: '',
          savedFilters: null,
          fields: STATS_FILTER_FIELDS
        });
      }

      return STATS_FILTER_FIELDS.reduce((values, field) => {
        values[field.key] = Object.prototype.hasOwnProperty.call(field, 'defaultValue')
          ? field.defaultValue
          : '';
        return values;
      }, {});
    }

    function resetStatsFilters() {
      const defaults = getDefaultStatsFilters();

      currentStatsCustomTimeRange = null;
      hideZeroSuccess = true;
      rememberExactStatsFilters({
        ...defaults,
        channelNameExact: false,
        modelExact: false
      });

      window.applyFilterControlValues(defaults, {
        range: 'f_hours',
        clientProtocol: 'f_client_protocol',
        authToken: 'f_auth_token'
      });
      statsChannelNameCombobox?.setValue('', t('stats.allChannels'));
      statsModelCombobox?.setValue('', t('trend.allModels'));

      const hideZeroCheckbox = document.getElementById('f_hide_zero_success');
      if (hideZeroCheckbox) hideZeroCheckbox.checked = true;

      window.persistFilterState({
        key: STATS_FILTER_KEY,
        values: getStatsFilters(),
        search: location.search,
        pathname: location.pathname,
        fields: STATS_FILTER_FIELDS,
        preserveExistingParams: true,
        historyMethod: 'replaceState'
      });
      loadStats();
    }

    function initStatsChannelNameCombobox(initialValue) {
      statsChannelNameCombobox = window.createSearchableCombobox({
        inputId: 'f_name',
        dropdownId: 'f_name_dropdown',
        attachMode: true,
        allowCustomInput: true,
        commitEmptyAsFirst: true,
        initialValue: initialValue || '',
        initialLabel: initialValue || t('stats.allChannels'),
        getOptions: () => [
          { value: '', label: t('stats.allChannels') },
          ...statsChannelNameOptions.map(n => ({ value: n, label: n }))
        ],
        onSelect: () => {
          window.persistFilterState({ key: STATS_FILTER_KEY, getValues: getStatsFilters });
          applyFilter();
        }
      });
    }

    function initStatsModelCombobox(initialValue) {
      statsModelCombobox = window.createSearchableCombobox({
        inputId: 'f_model',
        dropdownId: 'f_model_dropdown',
        attachMode: true,
        allowCustomInput: true,
        commitEmptyAsFirst: true,
        initialValue: initialValue || '',
        initialLabel: initialValue || t('trend.allModels'),
        getOptions: () => [
          { value: '', label: t('trend.allModels') },
          ...statsModelOptions.map(m => ({ value: m, label: m }))
        ],
        onSelect: () => {
          window.persistFilterState({
            key: STATS_FILTER_KEY,
            getValues: getStatsFilters
          });
          applyFilter();
        }
      });
    }

    async function loadStatsFilterOptions() {
      try {
        const params = new URLSearchParams();
        appendStatsTimeRangeParams(params, getStatsFilters());
        const data = await fetchDataWithAuth('/dashboard/stats/filter-options?' + params.toString());
        if (data) {
          statsChannelNameOptions = data.channel_names || [];
          statsModelOptions = data.models || [];
          if (statsChannelNameCombobox) {
            statsChannelNameCombobox.refresh();
          }
          if (statsModelCombobox) {
            statsModelCombobox.refresh();
          }
        }
      } catch (error) {
        console.error('[Stats] 加载筛选选项失败:', error);
      }
    }

    function populateStatsComboboxOptions() {
      loadStatsFilterOptions();
    }

    function initFilters(restoredFilters) {
      const name = restoredFilters.channelName || '';
      const range = restoredFilters.range || 'today';
      const model = restoredFilters.model || '';
      const clientProtocol = restoredFilters.clientProtocol || '';
      const authToken = restoredFilters.authToken || '';

      window.initSavedDateRangeFilter({
        selectId: 'f_hours',
        defaultValue: 'today',
        restoredValue: range,
        includeCustom: true,
        customRange: currentStatsCustomTimeRange,
        customPickerContainerId: 'f_hours_custom_range_host',
        onChange: (nextRange, customRange) => {
          if (nextRange === 'custom') {
            currentStatsCustomTimeRange = normalizeStatsCustomTimeRange(customRange);
          }
          window.persistFilterState({
            key: STATS_FILTER_KEY,
            getValues: getStatsFilters
          });
          loadStatsFilterOptions();
          applyFilter();
        }
      });

      initStatsChannelNameCombobox(name);
      initStatsModelCombobox(model);

      const clientProtocolSelect = document.getElementById('f_client_protocol');
      if (clientProtocolSelect) {
        clientProtocolSelect.value = clientProtocol;
        clientProtocolSelect.addEventListener('change', applyFilter);
      }

      window.initAuthTokenFilter({
        selectId: 'f_auth_token',
        value: authToken,
        loadOptions: { tokenPrefix: t('stats.tokenPrefix') },
        onChange: () => {
          window.persistFilterState({
            key: STATS_FILTER_KEY,
            getValues: getStatsFilters
          });
          applyFilter();
        }
      }).then((tokens) => {
        authTokens = tokens;
      });

      // 事件监听
      document.getElementById('btn_filter').addEventListener('click', applyFilter);
      document.getElementById('btn_clear_filters')?.addEventListener('click', resetStatsFilters);

      window.bindFilterApplyInputs({
        apply: applyFilter,
        debounceInputIds: [],
        enterInputIds: ['f_hours', 'f_client_protocol', 'f_auth_token']
      });
    }

    // 根据是否本日更新RPM表头标题
    function updateRpmHeader() {
      const rpmHeader = document.querySelector('[data-column="rpm"]');
      if (rpmHeader) {
        const span = rpmHeader.querySelector('span[data-i18n]');
        if (span) {
          const key = isToday ? 'stats.rpm' : 'stats.rpmNoRecent';
          const titleKey = isToday ? 'stats.rpmTitle' : 'stats.rpmNoRecentTitle';
          span.textContent = t(key);
          span.setAttribute('data-i18n', key);
          rpmHeader.title = t(titleKey);
          rpmHeader.setAttribute('data-i18n-title', titleKey);
        }
      }
    }

    // 渲染令牌选择器（支持语言切换时重新渲染）
    function renderTokenSelect() {
      const tokenSelect = document.getElementById('f_auth_token');
      if (!tokenSelect) return;

      const currentValue = tokenSelect.value;
      tokenSelect.innerHTML = `<option value="">${t('stats.allTokens')}</option>`;
      authTokens.forEach(token => {
        const option = document.createElement('option');
        option.value = token.id;
        option.textContent = token.description || `${t('stats.tokenPrefix')}${token.id}`;
        tokenSelect.appendChild(option);
      });
      // 恢复之前的选择
      if (currentValue) {
        tokenSelect.value = currentValue;
      }
    }

    // 格式化全局RPM（峰值/平均/最近），固定格式，0显示为-
    function formatGlobalRpm(stats, showRecent) {
      if (!stats) return '-/-' + (showRecent ? '/-' : '');
      return formatEntryRpm(stats, showRecent);
    }

    // 格式化每行的RPM（峰值/平均/最近），固定格式，0显示为-
    // 只有峰值反映渠道吞吐、按越高越好分档；平均值被所选时间范围稀释，最近值只是瞬时快照，保持中性色
    function formatEntryRpm(entry, showRecent) {
      const values = [entry.peak_rpm, entry.avg_rpm];
      if (showRecent) values.push(entry.recent_rpm);

      const html = values.map((v, index) => {
        const text = (v || 0).toFixed(1);
        const separator = index === 0 ? '' : '<span class="stats-rpm-separator">/</span>';
        if (text === '0.0') return `${separator}<span class="stats-rpm-value">-</span>`;
        const style = index === 0 ? ` style="--stats-rpm-color:${window.getRpmColor(v)};"` : '';
        return `${separator}<span class="stats-rpm-value"${style}>${text}</span>`;
      }).join('');

      return `<span class="stats-rpm-inline">${html}</span>`;
    }

    // 构建健康状态指示器 HTML（固定48个方块 + 当前成功率）
    // 性能优化：使用快速时间格式化，避免 toLocaleString 开销
    function buildHealthIndicator(timeline, currentRate) {
      if (!timeline || timeline.length === 0) {
        // 无健康数据时不显示指示器
        return '';
      }

      const fixedBucketCount = 48;
      const normalizedTimeline = timeline.length >= fixedBucketCount
        ? timeline.slice(-fixedBucketCount)
        : [...Array(fixedBucketCount - timeline.length).fill(null), ...timeline];
      const blocks = new Array(fixedBucketCount);

      for (let i = 0; i < fixedBucketCount; i++) {
        const point = normalizedTimeline[i];
        if (!point || point.rate < 0) {
          blocks[i] = `<span class="health-block unknown" title="${t('stats.healthNoData')}"></span>`;
          continue;
        }

        const rate = point.rate;
        const rateLimited = point.rate_limited || 0;
        const realErrors = (point.error || 0) - rateLimited;

        // 配色：所有失败都是限流(429) → 警告色；有真实错误 → 按成功率分级(绿/橙/红)
        const className = (realErrors === 0 && rateLimited > 0)
          ? 'rate-limited'
          : rate >= 0.95 ? 'healthy' : rate >= 0.80 ? 'warning' : 'critical';

        // 快速时间格式化（避免 toLocaleString 的性能开销），与趋势图统一 MM-DD HH:mm
        const timeStr = window.formatMonthDayTime(point.ts);

        // 构建 tooltip - 使用条件拼接减少数组操作
        let title = `${timeStr}
${t('stats.tooltipSuccess')}: ${point.success || 0} / ${t('stats.tooltipFailed')}: ${point.error || 0}`;
        if (rateLimited > 0) title += ` (${t('stats.tooltipRateLimited')}: ${rateLimited})`;
        if (point.avg_first_byte_time > 0) title += `
${t('stats.tooltipTTFT')}: ${point.avg_first_byte_time.toFixed(2)}s`;
        if (point.avg_duration > 0) title += `
${t('stats.tooltipDuration')}: ${point.avg_duration.toFixed(2)}s`;
        if (point.input_tokens > 0) title += `
${t('stats.tooltipInput')}: ${formatNumber(point.input_tokens)}`;
        if (point.output_tokens > 0) title += `
${t('stats.tooltipOutput')}: ${formatNumber(point.output_tokens)}`;
        if (point.cache_read_tokens > 0) title += `
${t('stats.tooltipCacheRead')}: ${formatNumber(point.cache_read_tokens)}`;
        if (point.cache_creation_tokens > 0) title += `
${t('stats.tooltipCacheWrite')}: ${formatNumber(point.cache_creation_tokens)}`;
        if (point.cost > 0) title += `
${t('stats.tooltipCost')}: ${formatCost(point.cost)}`;

        blocks[i] = `<span class="health-block ${className}" title="${escapeHtml(title)}"></span>`;
      }

      // 构建完整 HTML - 成功率数字按统一四档着色
      const ratePercent = window.formatPercent(currentRate);
      return `<div class="health-indicator"><span class="health-track">${blocks.join('')}</span><span class="health-rate" style="--health-rate-color:${window.getSuccessRateColor(currentRate)};">${ratePercent}</span></div>`;
    }

    // 注销功能（已由 ui.js 的 onLogout 统一处理）

    // localStorage key for stats page filters
    const STATS_FILTER_KEY = 'stats.filters';
    const STATS_FILTER_FIELDS = [
      { key: 'range', queryKeys: ['range'], defaultValue: 'today' },
      {
        key: 'customStartTime',
        queryKeys: ['start_time'],
        defaultValue: '',
        includeInQuery(value, values) {
          return values?.range === 'custom' && Boolean(value);
        },
        includeInRequest() {
          return false;
        }
      },
      {
        key: 'customEndTime',
        queryKeys: ['end_time'],
        defaultValue: '',
        includeInQuery(value, values) {
          return values?.range === 'custom' && Boolean(value);
        },
        includeInRequest() {
          return false;
        }
      },
      { key: 'channelId', queryKeys: ['channel_id'], defaultValue: '' },
      { key: 'clientProtocol', queryKeys: ['client_protocol'], defaultValue: '' },
      {
        key: 'channelName',
        queryKeys: ['channel_name', 'channel_name_like'],
        paramKey: getStatsChannelNameFilterKey,
        requestKey: getStatsChannelNameFilterKey,
        defaultValue: ''
      },
      {
        key: 'model',
        queryKeys: ['model', 'model_like'],
        paramKey: getStatsModelFilterKey,
        requestKey: getStatsModelFilterKey,
        defaultValue: ''
      },
      { key: 'authToken', queryKeys: ['auth_token_id'], defaultValue: '' },
    ];

    function getStatsFilters() {
      const channelName = statsChannelNameCombobox ? statsChannelNameCombobox.getValue() : '';
      const model = statsModelCombobox ? statsModelCombobox.getValue() : '';
      const baseValues = window.readFilterControlValues({
        range: { id: 'f_hours', defaultValue: 'today', trim: true },
        clientProtocol: { id: 'f_client_protocol', trim: true },
        authToken: { id: 'f_auth_token', trim: true }
      });
      const hasCustomRange = baseValues.range === 'custom' && currentStatsCustomTimeRange;
      return {
        ...baseValues,
        customStartTime: hasCustomRange ? String(currentStatsCustomTimeRange.startMs) : '',
        customEndTime: hasCustomRange ? String(currentStatsCustomTimeRange.endMs) : '',
        channelName,
        channelNameExact: isExactStatsChannelNameFilter(channelName),
        model,
        modelExact: isExactStatsModelFilter(model),
        hideZeroSuccess: hideZeroSuccess
      };
    }

    function buildStatsRequestParams() {
      const params = window.FilterQuery.buildRequestParams(getStatsFilters(), STATS_FILTER_FIELDS);
      appendStatsTimeRangeParams(params, getStatsFilters());
      return params;
    }

    function bindStatsStaticControls() {
      const viewToggleGroup = document.getElementById('view-toggle-group');
      if (viewToggleGroup && !viewToggleGroup.dataset.bound) {
        viewToggleGroup.addEventListener('click', (e) => {
          const viewBtn = e.target.closest('.view-toggle-btn[data-view]');
          if (!viewBtn) return;

          switchView(viewBtn.dataset.view);
        });
        viewToggleGroup.dataset.bound = '1';
      }

      const thead = document.querySelector('.stats-table thead');
      if (thead && !thead.dataset.bound) {
        thead.addEventListener('click', (e) => {
          if (e.target.closest('.stats-tree-toggle-all')) {
            toggleAllStatsChannels();
            return;
          }

          const sortable = e.target.closest('.sortable[data-column]');
          if (!sortable) return;

          sortTable(sortable.dataset.column);
        });
        thead.dataset.bound = '1';
      }
    }

    // 页面初始化
    window.initPageBootstrap({
      topbarKey: 'stats',
      run: async () => {
      bindStatsStaticControls();

      // 优先从 URL 读取，其次从 localStorage 恢复，默认 all
      const u = new URLSearchParams(location.search);
      const hasUrlParams = u.toString().length > 0;
      const savedFilters = window.FilterState.load(STATS_FILTER_KEY);
      const restoredFilters = window.FilterState.restore({
        search: location.search,
        savedFilters,
        fields: STATS_FILTER_FIELDS
      });
      currentStatsCustomTimeRange = restoredFilters.range === 'custom'
        ? normalizeStatsCustomTimeRange(restoredFilters)
        : null;
      if (restoredFilters.range === 'custom' && !currentStatsCustomTimeRange) {
        restoredFilters.range = 'today';
      }
      rememberExactStatsFilters({
        ...restoredFilters,
        channelNameExact: !hasUrlParams && savedFilters?.channelNameExact === true,
        modelExact: !hasUrlParams && savedFilters?.modelExact === true
      }, hasUrlParams ? u : null);
      // 恢复隐藏0成功选项状态（从 localStorage 读取，默认 true）
      hideZeroSuccess = savedFilters?.hideZeroSuccess !== false;
      const hideZeroCheckbox = document.getElementById('f_hide_zero_success');
      if (hideZeroCheckbox) {
        hideZeroCheckbox.checked = hideZeroSuccess;
        hideZeroCheckbox.addEventListener('change', (e) => {
          hideZeroSuccess = e.target.checked;
          window.persistFilterState({
            key: STATS_FILTER_KEY,
            getValues: getStatsFilters
          });
          renderStatsTable();
        });
      }

      initFilters(restoredFilters);

      if (!hasUrlParams && savedFilters) {
        window.persistFilterState({
          values: savedFilters,
          pathname: location.pathname,
          fields: STATS_FILTER_FIELDS,
          historyMethod: 'replaceState'
        });
      }

      await loadStats();
      restoreViewState();

      // 注册语言切换回调，重新渲染动态内容
      window.i18n.onLocaleChange(() => {
        renderTokenSelect();
        renderStatsTable();
        updateRpmHeader();
        if (currentView === 'chart') {
          renderCharts();
        }
      });

      // 事件委托：处理统计表格中的渠道名称和模型名称点击
      const statsTableBody = document.getElementById('stats_tbody');
      if (statsTableBody) {
        statsTableBody.addEventListener('click', (e) => {
          if (e.target.closest('[data-stats-retry]')) {
            loadStats();
            return;
          }

          // 处理渠道名称点击
          const channelLink = e.target.closest('.channel-link[data-channel-name]');
          if (channelLink) {
            e.preventDefault();
            const channelName = channelLink.dataset.channelName;
            if (channelName) {
              const params = buildStatsLogLinkParams({ channel_name: channelName });
              window.location.href = `/web/logs.html?${params.toString()}`;
            }
            return;
          }

          // 处理模型名称点击
          const modelLink = e.target.closest('.model-link[data-model]');
          if (modelLink) {
            e.preventDefault();
            const model = modelLink.dataset.model;
            const channelName = modelLink.dataset.channelName;
            if (model) {
              const params = buildStatsLogLinkParams({
                channel_name: channelName,
                model
              });
              window.location.href = `/web/logs.html?${params.toString()}`;
            }
            return;
          }

          // 点击渠道行（链接以外区域或折叠按钮）折叠/展开其模型
          const channelRow = e.target.closest('.stats-channel-row');
          if (channelRow && !e.target.closest('a')) {
            toggleStatsChannel(channelRow);
          }
        });
      }

      // 自动刷新（system_settings.auto_refresh_interval_seconds，0=禁用）
      if (typeof window.createAutoRefresh === 'function') {
        window.createAutoRefresh({ load: () => loadStats({ background: true }) }).init();
      }
      }
    });

    // ========== 图表视图功能 ==========
    let currentView = 'table'; // 当前视图: 'table' | 'chart'
    let chartInstances = {}; // ECharts 实例缓存

    function getStatsChartTheme() {
      return typeof window.getChartTheme === 'function'
        ? window.getChartTheme()
        : {
          mutedText: '#6b7280',
          strongText: '#111827',
          tooltipBg: 'rgba(255, 255, 255, 0.98)',
          tooltipBorder: 'rgba(17, 24, 39, 0.16)',
          tooltipText: '#111827',
          axisLine: 'rgba(17, 24, 39, 0.16)',
          surface: '#ffffff'
        };
    }

    // 切换视图
    function switchView(view) {
      currentView = view;
      document.documentElement.classList.toggle('stats-view-init-chart', view === 'chart');

      // 持久化视图状态
      try {
        localStorage.setItem('stats.view', view);
      } catch (_) {}

      // 更新按钮状态
      document.querySelectorAll('.view-toggle-btn').forEach(btn => {
        btn.classList.toggle('active', btn.dataset.view === view);
      });

      // 切换显示
      const tableView = document.getElementById('stats-table-view');
      const chartView = document.getElementById('stats-chart-view');

      if (view === 'table') {
        tableView.style.display = 'block';
        chartView.style.display = 'none';
      } else {
        tableView.style.display = 'none';
        chartView.style.display = 'block';
        // 渲染图表
        renderCharts();
      }
    }

    // 恢复视图状态
    function restoreViewState() {
      try {
        const savedView = localStorage.getItem('stats.view');
        if (savedView === 'chart' || savedView === 'table') {
          // 只在需要切换时才调用 switchView，避免不必要的重绘
          if (savedView !== currentView) {
            switchView(savedView);
          }
        }
      } catch (_) {}
    }

    // 渲染所有饼图
    function renderCharts() {
      // 空数据也要走 renderPieChart，否则旧图残留
      const stats = (statsData && statsData.stats) || [];

      // 聚合数据（只统计成功调用）
      const channelCallsMap = {}; // 渠道 -> 成功调用次数
      const channelTokensMap = {}; // 渠道 -> Token用量
      const modelCallsMap = {}; // 模型 -> 成功调用次数
      const modelTokensMap = {}; // 模型 -> Token用量
      const channelCostMap = {}; // 渠道 -> 成本（美元）
      const modelCostMap = {}; // 模型 -> 成本（美元）

      for (const entry of stats) {
        const channelName = entry.channel_name || t('stats.unknownChannel');
        const modelName = entry.model || t('stats.unknownModel');
        const successCount = entry.success || 0;
        const totalTokens = (entry.total_input_tokens || 0) + (entry.total_output_tokens || 0) + (entry.total_cache_read_input_tokens || 0) + (entry.total_cache_creation_input_tokens || 0);

        // 只统计成功调用
        if (successCount > 0) {
          // 渠道调用次数
          channelCallsMap[channelName] = (channelCallsMap[channelName] || 0) + successCount;
          // 渠道Token用量
          channelTokensMap[channelName] = (channelTokensMap[channelName] || 0) + totalTokens;
          // 模型调用次数
          modelCallsMap[modelName] = (modelCallsMap[modelName] || 0) + successCount;
          // 模型Token用量
          modelTokensMap[modelName] = (modelTokensMap[modelName] || 0) + totalTokens;
        }

        // 成本聚合（不依赖 successCount，因为成本可能来自失败请求的部分消耗）
        const cost = entry.total_cost || 0;
        const effectiveCost = (entry.effective_cost !== undefined && entry.effective_cost !== null)
          ? Number(entry.effective_cost) || 0
          : cost;
        if (cost > 0 || effectiveCost > 0) {
          if (!channelCostMap[channelName]) channelCostMap[channelName] = { standard: 0, effective: 0 };
          channelCostMap[channelName].standard += cost;
          channelCostMap[channelName].effective += effectiveCost;
          if (!modelCostMap[modelName]) modelCostMap[modelName] = { standard: 0, effective: 0 };
          modelCostMap[modelName].standard += cost;
          modelCostMap[modelName].effective += effectiveCost;
        }
      }

      // 渲染6个饼图
      const unitTimes = t('stats.unitTimes');
      renderPieChart('chart-channel-calls', channelCallsMap, unitTimes);
      renderPieChart('chart-channel-tokens', channelTokensMap, '');
      renderPieChart('chart-model-calls', modelCallsMap, unitTimes);
      renderPieChart('chart-model-tokens', modelTokensMap, '');
      renderPieChart('chart-channel-cost', channelCostMap, '$');
      renderPieChart('chart-model-cost', modelCostMap, '$');
    }

    // 渲染单个饼图
    function renderPieChart(containerId, dataMap, unit) {
      const container = document.getElementById(containerId);
      if (!container) return;

      // 获取或创建 ECharts 实例
      if (!chartInstances[containerId]) {
        chartInstances[containerId] = echarts.init(container);
      }
      const chart = chartInstances[containerId];
      const chartTheme = getStatsChartTheme();

      // 转换数据格式并排序（成本场景的值为 {standard, effective}，其他场景为数字）
      const data = Object.entries(dataMap)
        .map(([name, value]) => {
          if (value && typeof value === 'object') {
            const eff = Number(value.effective) || 0;
            const std = Number(value.standard) || 0;
            return { name, value: eff || std, standard: std };
          }
          return { name, value };
        })
        .sort((a, b) => b.value - a.value);

      // 如果没有数据，显示空状态
      if (data.length === 0) {
        chart.setOption({
          title: {
            text: t('stats.chartNoData'),
            left: 'center',
            top: 'center',
            textStyle: {
              color: chartTheme.mutedText,
              fontSize: 14
            }
          }
        }, true);
        return;
      }

      // 颜色方案
      const colors = [
        '#3b82f6', '#10b981', '#f59e0b', '#ef4444', '#8b5cf6',
        '#06b6d4', '#ec4899', '#84cc16', '#f97316', '#6366f1',
        '#14b8a6', '#a855f7', '#eab308', '#22c55e', '#0ea5e9'
      ];

      // 计算总值用于百分比
      const total = data.reduce((sum, item) => sum + item.value, 0);

      const option = {
        // 合并刷新时隐藏可能残留的空状态标题
        title: { show: false },
        tooltip: {
          trigger: 'item',
          backgroundColor: chartTheme.tooltipBg,
          borderColor: chartTheme.tooltipBorder,
          textStyle: { color: chartTheme.tooltipText, fontSize: 12 },
          formatter: function(params) {
            const value = params.value;
            let formattedValue;
            // 成本特殊处理
            if (unit === '$') {
              const std = params.data && typeof params.data.standard === 'number' ? params.data.standard : value;
              formattedValue = formatCostPair(std, value);
              return `${params.name}<br/>${formattedValue} (${params.percent}%)`;
            }
            formattedValue = window.formatNumber(value);
            return `${params.name}<br/>${formattedValue}${unit} (${params.percent}%)`;
          }
        },
        legend: {
          type: 'scroll',
          orient: 'vertical',
          right: 10,
          top: 20,
          bottom: 20,
          textStyle: { fontSize: 11, color: chartTheme.mutedText },
          pageIconColor: chartTheme.mutedText,
          pageIconInactiveColor: chartTheme.axisLine,
          pageTextStyle: { color: chartTheme.mutedText },
          formatter: function(name) {
            const item = data.find(d => d.name === name);
            if (item && total > 0) {
              return `${name} (${window.formatPercent(item.value / total)})`;
            }
            return name;
          }
        },
        color: colors,
        series: [{
          type: 'pie',
          radius: ['40%', '70%'],
          center: ['35%', '50%'],
          avoidLabelOverlap: true,
          itemStyle: {
            borderRadius: 4,
            borderColor: chartTheme.surface,
            borderWidth: 2
          },
          label: {
            show: false
          },
          emphasis: {
            label: {
              show: true,
              fontSize: 12,
              fontWeight: 'bold',
              formatter: function(params) {
                return params.percent.toFixed(1) + '%';
              }
            },
            itemStyle: {
              shadowBlur: 10,
              shadowOffsetX: 0,
              shadowColor: 'rgba(0, 0, 0, 0.3)'
            }
          },
          data: data
        }]
      };

      // 合并更新：自动刷新时保留图表实例与图例状态，避免闪烁
      chart.setOption(option);
    }

    // 窗口大小变化时重新调整图表
    window.addEventListener('resize', function() {
      Object.values(chartInstances).forEach(chart => {
        if (chart) chart.resize();
      });
    });

    window.addEventListener('ccload:themechange', function() {
      if (currentView === 'chart') {
        renderCharts();
      }
    });
