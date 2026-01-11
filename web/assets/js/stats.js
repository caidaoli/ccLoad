    // 常量定义
    const STATS_TABLE_COLUMNS = 12; // 统计表列数

    let statsData = null;
    let rpmStats = null; // 全局RPM统计（峰值、平均、最近一分钟）
    let isToday = true;  // 是否为本日（本日才显示最近一分钟）
    let durationSeconds = 0; // 时间跨度（秒），用于计算RPM
    let currentChannelType = 'all'; // 当前选中的渠道类型
    let authTokens = []; // 令牌列表
    let hideZeroSuccess = true; // 是否隐藏0成功的模型（默认开启）
    let sortState = {
      column: null,
      order: null // null, 'asc', 'desc'
    };

    async function loadStats() {
      try {
        renderStatsLoading();

        const u = new URLSearchParams(location.search);
        const params = new URLSearchParams({
          range: (u.get('range')||'today')
        });

        // 复用筛选条件
        if (u.get('channel_id')) params.set('channel_id', u.get('channel_id'));
        if (u.get('channel_name')) params.set('channel_name', u.get('channel_name'));
        if (u.get('channel_name_like')) params.set('channel_name_like', u.get('channel_name_like'));
        if (u.get('model')) params.set('model', u.get('model'));
        if (u.get('model_like')) params.set('model_like', u.get('model_like'));
        if (u.get('auth_token_id')) params.set('auth_token_id', u.get('auth_token_id'));

        // 添加渠道类型筛选
        if (currentChannelType && currentChannelType !== 'all') {
          params.set('channel_type', currentChannelType);
        }

        // 后端返回格式: {"success":true,"data":{"stats":[...],"duration_seconds":...,"rpm_stats":{...},"is_today":...}}
        statsData = (await fetchDataWithAuth('/admin/stats?' + params.toString())) || { stats: [] };
        durationSeconds = statsData.duration_seconds || 1; // 防止除零
        rpmStats = statsData.rpm_stats || null;
        isToday = statsData.is_today !== false;

        // 🎯 新增: 初始化时应用默认排序(渠道名称→模型名称)
        applyDefaultSorting();

        renderStatsTable();
        updateStatsCount();
        updateRpmHeader(); // 更新表头标题

        // 如果当前是图表视图，同步更新图表
        if (currentView === 'chart') {
          renderCharts();
        }

      } catch (error) {
        console.error('加载统计数据失败:', error);
        if (window.showError) try { window.showError('无法加载统计数据'); } catch(_){}
        renderStatsError();
      }
    }

    function renderStatsLoading() {
      const tbody = document.getElementById('stats_tbody');
      tbody.innerHTML = '';
      const row = TemplateEngine.render('tpl-stats-loading', { colspan: STATS_TABLE_COLUMNS });
      if (row) tbody.appendChild(row);
    }

    function renderStatsError() {
      const tbody = document.getElementById('stats_tbody');
      tbody.innerHTML = '';
      const row = TemplateEngine.render('tpl-stats-error', { colspan: STATS_TABLE_COLUMNS });
      if (row) tbody.appendChild(row);
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
      
      // 执行排序并重新渲染
      applySorting();
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

    function applySorting() {
      // 如果没有排序状态,从原始数据恢复默认排序(渠道名称→模型名称)
      if (!sortState.column || !sortState.order) {
        if (statsData && statsData.originalStats) {
          statsData.stats = [...statsData.originalStats];
        }
        return;
      }

      // 保存原始数据（如果还没有保存）
      if (!statsData.originalStats) {
        statsData.originalStats = [...statsData.stats];
      }

      const column = sortState.column;
      const isAsc = sortState.order === 'asc';

      statsData.stats.sort((a, b) => {
        let valueA, valueB;

        switch (column) {
          case 'channel_name':
            valueA = (a.channel_name || '').toLowerCase();
            valueB = (b.channel_name || '').toLowerCase();
            break;
          case 'model':
            valueA = (a.model || '').toLowerCase();
            valueB = (b.model || '').toLowerCase();
            break;
          case 'success':
            valueA = a.success || 0;
            valueB = b.success || 0;
            break;
          case 'error':
            valueA = a.error || 0;
            valueB = b.error || 0;
            break;
          case 'rpm':
            // 使用后端计算的峰值RPM排序
            valueA = a.peak_rpm || 0;
            valueB = b.peak_rpm || 0;
            break;
          case 'success_rate':
            valueA = a.total > 0 ? (a.success / a.total) : 0;
            valueB = b.total > 0 ? (b.success / b.total) : 0;
            break;
          case 'avg_first_byte_time':
            // 优先按平均耗时排序，其次按平均首字时间
            valueA = a.avg_duration_seconds || a.avg_first_byte_time_seconds || 0;
            valueB = b.avg_duration_seconds || b.avg_first_byte_time_seconds || 0;
            break;
          case 'total_input_tokens':
            valueA = a.total_input_tokens || 0;
            valueB = b.total_input_tokens || 0;
            break;
          case 'total_output_tokens':
            valueA = a.total_output_tokens || 0;
            valueB = b.total_output_tokens || 0;
            break;
          case 'total_cache_read':
            valueA = a.total_cache_read_input_tokens || 0;
            valueB = b.total_cache_read_input_tokens || 0;
            break;
          case 'total_cache_creation':
            valueA = a.total_cache_creation_input_tokens || 0;
            valueB = b.total_cache_creation_input_tokens || 0;
            break;
          case 'total_cost':
            valueA = a.total_cost || 0;
            valueB = b.total_cost || 0;
            break;
          default:
            return 0;
        }

        let result;
        if (typeof valueA === 'string') {
          result = valueA.localeCompare(valueB, 'zh-CN');
        } else {
          result = valueA - valueB;
        }

        return isAsc ? result : -result;
      });
    }

    function renderStatsTable() {
      const tbody = document.getElementById('stats_tbody');

      if (!statsData || !statsData.stats || statsData.stats.length === 0) {
        tbody.innerHTML = '';
        const emptyRow = TemplateEngine.render('tpl-stats-empty', { colspan: STATS_TABLE_COLUMNS });
        if (emptyRow) tbody.appendChild(emptyRow);
        return;
      }

      // 根据 hideZeroSuccess 过滤数据
      const filteredStats = hideZeroSuccess
        ? statsData.stats.filter(entry => (entry.success || 0) > 0)
        : statsData.stats;

      if (filteredStats.length === 0) {
        tbody.innerHTML = '';
        const emptyRow = TemplateEngine.render('tpl-stats-empty', { colspan: STATS_TABLE_COLUMNS });
        if (emptyRow) tbody.appendChild(emptyRow);
        return;
      }

      tbody.innerHTML = '';

      // 初始化合计变量
      let totalSuccess = 0;
      let totalError = 0;
      let totalRequests = 0;
      let totalInputTokens = 0;
      let totalOutputTokens = 0;
      let totalCacheRead = 0;
      let totalCacheCreation = 0;
      let totalCost = 0;

      const fragment = document.createDocumentFragment();

      for (const entry of filteredStats) {
        const successRate = entry.total > 0 ? ((entry.success / entry.total) * 100) : 0;
        const successRateText = successRate > 0 ? successRate.toFixed(1) + '%' : '';

        // 使用后端返回的 RPM 数据（峰值/平均/最近）
        const rpmHtml = formatEntryRpm(entry, isToday);

        // 根据成功率设置颜色类
        let successRateClass = 'success-rate';
        if (successRate >= 95) successRateClass += ' high';
        else if (successRate > 0 && successRate < 80) successRateClass += ' low';

        const modelDisplay = entry.model ?
          `<a href="#" class="model-tag model-link" data-model="${escapeHtml(entry.model)}" data-channel-id="${entry.channel_id || ''}" title="查看该渠道此模型的日志">${escapeHtml(entry.model)}</a>` :
          '<span style="color: var(--neutral-500);">未知模型</span>';

        // 格式化平均首字响应时间/平均耗时
        const avgFirstByteTime = entry.avg_first_byte_time_seconds || 0;
        const avgDuration = entry.avg_duration_seconds || 0;
        let avgTimeText = '';

        if (avgFirstByteTime > 0 && avgDuration > 0) {
          // 流式请求：显示首字/耗时
          const durationColor = getDurationColor(avgDuration);
          avgTimeText = `<span style="color: ${durationColor};">${avgFirstByteTime.toFixed(2)}/${avgDuration.toFixed(2)}</span>`;
        } else if (avgDuration > 0) {
          // 非流式请求：只显示耗时
          const durationColor = getDurationColor(avgDuration);
          avgTimeText = `<span style="color: ${durationColor};">${avgDuration.toFixed(2)}</span>`;
        } else if (avgFirstByteTime > 0) {
          // 仅有首字时间（理论上不应出现）
          const durationColor = getDurationColor(avgFirstByteTime);
          avgTimeText = `<span style="color: ${durationColor};">${avgFirstByteTime.toFixed(2)}</span>`;
        }

        // 格式化Token数据
        const inputTokensText = entry.total_input_tokens ? formatNumber(entry.total_input_tokens) : '';
        const outputTokensText = entry.total_output_tokens ? formatNumber(entry.total_output_tokens) : '';
        const cacheReadTokensText = entry.total_cache_read_input_tokens ?
          `<span style="color: var(--success-600);">${formatNumber(entry.total_cache_read_input_tokens)}</span>` : '';
        const cacheCreationTokensText = entry.total_cache_creation_input_tokens ?
          `<span style="color: var(--primary-600);">${formatNumber(entry.total_cache_creation_input_tokens)}</span>` : '';
        const costText = entry.total_cost ?
          `<span style="color: var(--warning-600); font-weight: 500;">${formatCost(entry.total_cost)}</span>` : '';

        // 构建健康状态指示器
        const healthIndicator = buildHealthIndicator(entry.health_timeline, successRate / 100);

        const row = TemplateEngine.render('tpl-stats-row', {
          channelId: entry.channel_id,
          channelName: escapeHtml(entry.channel_name),
          channelIdBadge: entry.channel_id ? `<span class="channel-id">(ID: ${entry.channel_id})</span>` : '',
          healthIndicator: healthIndicator,
          modelDisplay: modelDisplay,
          successCount: formatNumber(entry.success || 0),
          errorCount: formatNumber(entry.error || 0),
          rpm: rpmHtml,
          successRateClass: successRateClass,
          successRateText: successRateText,
          successRate: successRate,
          avgFirstByteTime: avgTimeText,
          inputTokens: inputTokensText,
          outputTokens: outputTokensText,
          cacheReadTokens: cacheReadTokensText,
          cacheCreationTokens: cacheCreationTokensText,
          costText: costText
        });
        if (row) fragment.appendChild(row);

        // 累加合计数据
        totalSuccess += entry.success || 0;
        totalError += entry.error || 0;
        totalRequests += entry.total || 0;
        totalInputTokens += entry.total_input_tokens || 0;
        totalOutputTokens += entry.total_output_tokens || 0;
        totalCacheRead += entry.total_cache_read_input_tokens || 0;
        totalCacheCreation += entry.total_cache_creation_input_tokens || 0;
        totalCost += entry.total_cost || 0;
      }

      tbody.appendChild(fragment);

      // 追加合计行（使用全局rpm_stats显示峰值/平均/最近）
      const totalSuccessRateVal = totalRequests > 0 ? (totalSuccess / totalRequests) * 100 : 0;
      const totalSuccessRate = totalSuccessRateVal > 0 ? totalSuccessRateVal.toFixed(1) + '%' : '';

      // 使用全局rpm_stats格式化RPM
      const totalRpmHtml = formatGlobalRpm(rpmStats, isToday);

      const totalRow = TemplateEngine.render('tpl-stats-total', {
        successCount: formatNumber(totalSuccess),
        errorCount: formatNumber(totalError),
        rpm: totalRpmHtml,
        successRateText: totalSuccessRate,
        inputTokens: formatNumber(totalInputTokens),
        outputTokens: formatNumber(totalOutputTokens),
        cacheReadTokens: formatNumber(totalCacheRead),
        cacheCreationTokens: formatNumber(totalCacheCreation),
        costText: formatCost(totalCost)
      });
      if (totalRow) tbody.appendChild(totalRow);
    }

    function applyFilter() {
      const range = document.getElementById('f_hours').value.trim();
      const id = document.getElementById('f_id').value.trim();
      const name = document.getElementById('f_name').value.trim();
      const model = document.getElementById('f_model').value.trim();
      const authToken = document.getElementById('f_auth_token').value.trim();

      // 保存筛选条件到 localStorage
      saveStatsFilters();

      const q = new URLSearchParams(location.search);
      if (range) q.set('range', range); else q.delete('range');
      if (id) q.set('channel_id', id); else q.delete('channel_id');
      if (name) { q.set('channel_name_like', name); q.delete('channel_name'); }
      else { q.delete('channel_name_like'); }
      if (model) { q.set('model_like', model); q.delete('model'); }
      else { q.delete('model_like'); q.delete('model'); }
      if (authToken) q.set('auth_token_id', authToken); else q.delete('auth_token_id');

      // 使用 pushState 更新 URL，避免页面重新加载
      history.pushState(null, '', '?' + q.toString());
      loadStats();
    }

    function initFilters() {
      const u = new URLSearchParams(location.search);
      const saved = loadStatsFilters();
      // URL 参数优先，否则从 localStorage 恢复
      const hasUrlParams = u.toString().length > 0;

      const id = u.get('channel_id') || (!hasUrlParams && saved?.channelId) || '';
      const name = u.get('channel_name_like') || u.get('channel_name') || (!hasUrlParams && saved?.channelName) || '';
      const range = u.get('range') || (!hasUrlParams && saved?.range) || 'today';
      const model = u.get('model_like') || u.get('model') || (!hasUrlParams && saved?.model) || '';
      const authToken = u.get('auth_token_id') || (!hasUrlParams && saved?.authToken) || '';

      // 初始化时间范围选择器 (默认"本日")，切换后立即筛选
      if (window.initDateRangeSelector) {
        initDateRangeSelector('f_hours', 'today', () => {
          saveStatsFilters();
          applyFilter();
        });
        // 设置URL中的值
        document.getElementById('f_hours').value = range;
      }

      document.getElementById('f_id').value = id;
      document.getElementById('f_name').value = name;
      document.getElementById('f_model').value = model;

      // 加载令牌列表
      loadAuthTokens().then(() => {
        document.getElementById('f_auth_token').value = authToken;
      });

      // 令牌选择器切换后立即筛选
      document.getElementById('f_auth_token').addEventListener('change', () => {
        saveStatsFilters();
        applyFilter();
      });

      // 事件监听
      document.getElementById('btn_filter').addEventListener('click', applyFilter);

      // 输入框自动筛选（防抖）
      const debouncedFilter = debounce(applyFilter, 500);
      ['f_id', 'f_name', 'f_model'].forEach(id => {
        const el = document.getElementById(id);
        if (el) {
          el.addEventListener('input', debouncedFilter);
        }
      });

      // 回车键筛选
      ['f_hours', 'f_id', 'f_name', 'f_model', 'f_auth_token'].forEach(id => {
        const el = document.getElementById(id);
        if (el) {
          el.addEventListener('keydown', e => {
            if (e.key === 'Enter') applyFilter();
          });
        }
      });
    }

    function updateStatsCount() {
      // 更新筛选器统计信息（显示过滤后的记录数）
      const statsCountEl = document.getElementById('statsCount');
      if (statsCountEl && statsData && statsData.stats) {
        const count = hideZeroSuccess
          ? statsData.stats.filter(entry => (entry.success || 0) > 0).length
          : statsData.stats.length;
        statsCountEl.textContent = count;
      }
    }

    // 根据是否本日更新RPM表头标题
    function updateRpmHeader() {
      const rpmHeader = document.querySelector('[data-column="rpm"]');
      if (rpmHeader) {
        rpmHeader.childNodes[0].textContent = isToday ? 'RPM(峰/均/近)' : 'RPM(峰/均)';
      }
    }

    // 应用默认排序:按渠道优先级降序,相同优先级按渠道名称升序,相同渠道按模型名称升序
    // 如果用户已选择自定义排序，则保持用户的排序
    function applyDefaultSorting() {
      if (!statsData || !statsData.stats || statsData.stats.length === 0) return;

      // 保存原始数据副本(仅首次)
      if (!statsData.originalStats) {
        statsData.originalStats = [...statsData.stats];
      }

      // 如果用户已选择自定义排序，应用用户的排序而非默认排序
      if (sortState.column && sortState.order) {
        applySorting();
        updateSortHeaders();
        return;
      }

      // 按渠道优先级降序(高优先级在前),相同优先级按渠道名称升序,相同渠道按模型名称升序
      statsData.stats.sort((a, b) => {
        // 首先按优先级降序(数值大的在前)
        const priorityA = a.channel_priority ?? 0;
        const priorityB = b.channel_priority ?? 0;
        if (priorityA !== priorityB) return priorityB - priorityA;

        // 优先级相同时,按渠道名称升序
        const channelA = (a.channel_name || '').toLowerCase();
        const channelB = (b.channel_name || '').toLowerCase();
        const channelCompare = channelA.localeCompare(channelB, 'zh-CN');
        if (channelCompare !== 0) return channelCompare;

        // 渠道名称相同时,按模型名称升序
        const modelA = (a.model || '').toLowerCase();
        const modelB = (b.model || '').toLowerCase();
        return modelA.localeCompare(modelB, 'zh-CN');
      });
    }

    // 加载令牌列表
    async function loadAuthTokens() {
      try {
        const data = await fetchDataWithAuth('/admin/auth-tokens');
        authTokens = (data && data.tokens) || [];

        // 填充令牌选择器
        const tokenSelect = document.getElementById('f_auth_token');
        if (tokenSelect && authTokens.length > 0) {
          // 保留"全部令牌"选项
          tokenSelect.innerHTML = '<option value="">全部令牌</option>';
          authTokens.forEach(token => {
            const option = document.createElement('option');
            option.value = token.id;
            option.textContent = token.description || `令牌 #${token.id}`;
            tokenSelect.appendChild(option);
          });
        }
      } catch (error) {
        console.error('加载令牌列表失败:', error);
      }
    }

    // 格式化 RPM（每分钟请求数）带颜色
    function formatRpm(rpm) {
      if (rpm < 0.01) return '';
      const color = getRpmColor(rpm);
      const text = rpm >= 1000 ? (rpm / 1000).toFixed(1) + 'K' : rpm >= 1 ? rpm.toFixed(1) : rpm.toFixed(2);
      return `<span style="color: ${color}; font-weight: 500;">${text}</span>`;
    }

    // 格式化全局RPM（峰值/平均/最近），固定格式，0显示为-
    function formatGlobalRpm(stats, showRecent) {
      if (!stats) return '-/-' + (showRecent ? '/-' : '');

      const formatVal = (v) => {
        const text = (v || 0).toFixed(1);
        return text === '0.0' ? '-' : text;
      };
      const peakText = formatVal(stats.peak_rpm);
      const avgText = formatVal(stats.avg_rpm);

      const peakColor = peakText !== '-' ? getRpmColor(stats.peak_rpm) : 'inherit';
      const avgColor = avgText !== '-' ? getRpmColor(stats.avg_rpm) : 'inherit';

      let result = `<span style="color: ${peakColor};">${peakText}</span>/<span style="color: ${avgColor};">${avgText}</span>`;

      if (showRecent) {
        const recentText = formatVal(stats.recent_rpm);
        const recentColor = recentText !== '-' ? getRpmColor(stats.recent_rpm) : 'inherit';
        result += `/<span style="color: ${recentColor};">${recentText}</span>`;
      }

      return result;
    }

    // 格式化每行的RPM（峰值/平均/最近），固定格式，0显示为-
    function formatEntryRpm(entry, showRecent) {
      const formatVal = (v) => {
        const text = (v || 0).toFixed(1);
        return text === '0.0' ? '-' : text;
      };

      const peakText = formatVal(entry.peak_rpm);
      const avgText = formatVal(entry.avg_rpm);

      const peakColor = peakText !== '-' ? getRpmColor(entry.peak_rpm) : 'inherit';
      const avgColor = avgText !== '-' ? getRpmColor(entry.avg_rpm) : 'inherit';

      let result = `<span style="color: ${peakColor};">${peakText}</span>/<span style="color: ${avgColor};">${avgText}</span>`;

      if (showRecent) {
        const recentText = formatVal(entry.recent_rpm);
        const recentColor = recentText !== '-' ? getRpmColor(entry.recent_rpm) : 'inherit';
        result += `/<span style="color: ${recentColor};">${recentText}</span>`;
      }

      return result;
    }

    // 根据耗时返回颜色
    function getDurationColor(seconds) {
      if (seconds <= 5) {
        return 'var(--success-600)'; // 绿色：快速
      } else if (seconds <= 30) {
        return 'var(--warning-600)'; // 橙色：中等
      } else {
        return 'var(--error-600)'; // 红色：慢速
      }
    }

    // 构建健康状态指示器 HTML（固定48个方块 + 当前成功率）
    // 性能优化：使用快速时间格式化，避免 toLocaleString 开销
    function buildHealthIndicator(timeline, currentRate) {
      if (!timeline || timeline.length === 0) {
        // 无健康数据时不显示指示器
        return '';
      }

      // 后端已返回固定48个时间点，rate=-1 表示无数据
      // 使用数组预分配 + 直接拼接，减少内存分配
      const len = timeline.length;
      const blocks = new Array(len);

      for (let i = 0; i < len; i++) {
        const point = timeline[i];
        const rate = point.rate;

        // rate < 0 表示该时间桶无数据
        if (rate < 0) {
          blocks[i] = '<span class="health-block unknown" title="无数据"></span>';
          continue;
        }

        const className = rate >= 0.95 ? 'healthy' : rate >= 0.80 ? 'warning' : 'critical';

        // 快速时间格式化（避免 toLocaleString 的性能开销）
        const d = new Date(point.ts);
        const timeStr = `${String(d.getMonth() + 1).padStart(2, '0')}/${String(d.getDate()).padStart(2, '0')} ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;

        // 构建 tooltip - 使用条件拼接减少数组操作
        let title = `${timeStr}\n成功: ${point.success || 0} / 失败: ${point.error || 0}`;
        if (point.avg_first_byte_time > 0) title += `\n首字: ${point.avg_first_byte_time.toFixed(2)}s`;
        if (point.avg_duration > 0) title += `\n耗时: ${point.avg_duration.toFixed(2)}s`;
        if (point.input_tokens > 0) title += `\n输入: ${formatNumber(point.input_tokens)}`;
        if (point.output_tokens > 0) title += `\n输出: ${formatNumber(point.output_tokens)}`;
        if (point.cache_read_tokens > 0) title += `\n缓存读: ${formatNumber(point.cache_read_tokens)}`;
        if (point.cache_creation_tokens > 0) title += `\n缓存写: ${formatNumber(point.cache_creation_tokens)}`;
        if (point.cost > 0) title += `\n成本: $${point.cost.toFixed(4)}`;

        blocks[i] = `<span class="health-block ${className}" title="${escapeHtml(title)}"></span>`;
      }

      // 构建完整 HTML - 成功率颜色：>=95%绿色, >=80%橙色, <80%红色
      const ratePercent = (currentRate * 100).toFixed(1);
      const rateColor = currentRate >= 0.95 ? 'var(--success-600)' :
                        currentRate >= 0.80 ? 'var(--warning-600)' : 'var(--error-600)';
      return `<div class="health-indicator">${blocks.join('')}<span class="health-rate" style="color: ${rateColor}">${ratePercent}%</span></div>`;
    }

    // 注销功能（已由 ui.js 的 onLogout 统一处理）

    // localStorage key for stats page filters
    const STATS_FILTER_KEY = 'stats.filters';

    function saveStatsFilters() {
      try {
        const filters = {
          channelType: currentChannelType,
          range: document.getElementById('f_hours')?.value || 'today',
          channelId: document.getElementById('f_id')?.value || '',
          channelName: document.getElementById('f_name')?.value || '',
          model: document.getElementById('f_model')?.value || '',
          authToken: document.getElementById('f_auth_token')?.value || '',
          hideZeroSuccess: hideZeroSuccess
        };
        localStorage.setItem(STATS_FILTER_KEY, JSON.stringify(filters));
      } catch (_) {}
    }

    function loadStatsFilters() {
      try {
        const saved = localStorage.getItem(STATS_FILTER_KEY);
        if (saved) return JSON.parse(saved);
      } catch (_) {}
      return null;
    }

    // 页面初始化
    document.addEventListener('DOMContentLoaded', async function() {
      if (window.initTopbar) initTopbar('stats');

      // 优先从 URL 读取，其次从 localStorage 恢复，默认 all
      const u = new URLSearchParams(location.search);
      const hasUrlParams = u.toString().length > 0;
      const savedFilters = loadStatsFilters();
      currentChannelType = u.get('channel_type') || (!hasUrlParams && savedFilters?.channelType) || 'all';

      // 恢复隐藏0成功选项状态（从 localStorage 读取，默认 true）
      hideZeroSuccess = savedFilters?.hideZeroSuccess !== false;
      const hideZeroCheckbox = document.getElementById('f_hide_zero_success');
      if (hideZeroCheckbox) {
        hideZeroCheckbox.checked = hideZeroSuccess;
        hideZeroCheckbox.addEventListener('change', (e) => {
          hideZeroSuccess = e.target.checked;
          saveStatsFilters();
          renderStatsTable();
          updateStatsCount();
        });
      }

      await initChannelTypeFilter(currentChannelType);

      initFilters();

      // ✅ 修复：如果没有 URL 参数但有保存的筛选条件，先同步 URL 再加载数据
      if (!hasUrlParams && savedFilters) {
        const q = new URLSearchParams();
        if (savedFilters.range) q.set('range', savedFilters.range);
        if (savedFilters.channelId) q.set('channel_id', savedFilters.channelId);
        if (savedFilters.channelName) q.set('channel_name_like', savedFilters.channelName);
        if (savedFilters.model) q.set('model_like', savedFilters.model);
        if (savedFilters.authToken) q.set('auth_token_id', savedFilters.authToken);
        if (savedFilters.channelType && savedFilters.channelType !== 'all') {
          q.set('channel_type', savedFilters.channelType);
        }
        // 使用 replaceState 更新 URL，不触发页面刷新
        if (q.toString()) {
          history.replaceState(null, '', '?' + q.toString());
        }
      }

      loadStats().then(() => {
        // 数据加载完成后恢复视图状态
        restoreViewState();
      });

      // 事件委托：处理统计表格中的渠道名称和模型名称点击
      const statsTableBody = document.getElementById('stats_tbody');
      if (statsTableBody) {
        statsTableBody.addEventListener('click', (e) => {
          // 获取当前时间范围参数
          const currentRange = document.getElementById('f_hours')?.value || 'today';

          // 处理渠道名称点击
          const channelLink = e.target.closest('.channel-link[data-channel-id]');
          if (channelLink) {
            e.preventDefault();
            const channelId = channelLink.dataset.channelId;
            if (channelId) {
              const logsUrl = `/web/logs.html?channel_id=${channelId}&range=${encodeURIComponent(currentRange)}`;
              window.location.href = logsUrl;
            }
            return;
          }

          // 处理模型名称点击
          const modelLink = e.target.closest('.model-link[data-model]');
          if (modelLink) {
            e.preventDefault();
            const model = modelLink.dataset.model;
            const channelId = modelLink.dataset.channelId;
            if (model) {
              const params = new URLSearchParams();
              if (channelId) params.set('channel_id', channelId);
              params.set('model_like', model);
              params.set('range', currentRange);
              window.location.href = `/web/logs.html?${params.toString()}`;
            }
            return;
          }
        });
      }
    });

    // 初始化渠道类型筛选器
    async function initChannelTypeFilter(initialType) {
      const select = document.getElementById('f_channel_type');
      if (!select) return;

      const types = await window.ChannelTypeManager.getChannelTypes();

      // 添加"全部"选项
      select.innerHTML = '<option value="all">全部</option>';
      types.forEach(type => {
        const option = document.createElement('option');
        option.value = type.value;
        option.textContent = type.display_name;
        if (type.value === initialType) {
          option.selected = true;
        }
        select.appendChild(option);
      });

      // 绑定change事件
      select.addEventListener('change', (e) => {
        currentChannelType = e.target.value;
        saveStatsFilters();
        loadStats();
      });
    }

    // ========== 图表视图功能 ==========
    let currentView = 'table'; // 当前视图: 'table' | 'chart'
    let chartInstances = {}; // ECharts 实例缓存

    // 切换视图
    function switchView(view) {
      currentView = view;

      // 移除初始化时注入的样式（避免与动态切换冲突）
      const initStyle = document.getElementById('stats-view-init-style');
      if (initStyle) {
        initStyle.remove();
      }

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
      if (!statsData || !statsData.stats || statsData.stats.length === 0) {
        return;
      }

      // 聚合数据（只统计成功调用）
      const channelCallsMap = {}; // 渠道 -> 成功调用次数
      const channelTokensMap = {}; // 渠道 -> Token用量
      const modelCallsMap = {}; // 模型 -> 成功调用次数
      const modelTokensMap = {}; // 模型 -> Token用量
      const channelCostMap = {}; // 渠道 -> 成本（美元）
      const modelCostMap = {}; // 模型 -> 成本（美元）

      for (const entry of statsData.stats) {
        const channelName = entry.channel_name || '未知渠道';
        const modelName = entry.model || '未知模型';
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
        if (cost > 0) {
          channelCostMap[channelName] = (channelCostMap[channelName] || 0) + cost;
          modelCostMap[modelName] = (modelCostMap[modelName] || 0) + cost;
        }
      }

      // 渲染6个饼图
      renderPieChart('chart-channel-calls', channelCallsMap, '次');
      renderPieChart('chart-channel-tokens', channelTokensMap, '');
      renderPieChart('chart-model-calls', modelCallsMap, '次');
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

      // 转换数据格式并排序
      const data = Object.entries(dataMap)
        .map(([name, value]) => ({ name, value }))
        .sort((a, b) => b.value - a.value);

      // 如果没有数据，显示空状态
      if (data.length === 0) {
        chart.setOption({
          title: {
            text: '暂无数据',
            left: 'center',
            top: 'center',
            textStyle: {
              color: '#999',
              fontSize: 14
            }
          }
        });
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
        tooltip: {
          trigger: 'item',
          backgroundColor: 'rgba(0, 0, 0, 0.85)',
          borderColor: 'rgba(255, 255, 255, 0.1)',
          textStyle: { color: '#fff', fontSize: 12 },
          formatter: function(params) {
            const value = params.value;
            let formattedValue;
            // 成本特殊处理
            if (unit === '$') {
              formattedValue = formatCost(value);
              return `${params.name}<br/>${formattedValue} (${params.percent}%)`;
            }
            // 原有逻辑：大数值缩写
            if (value >= 1000000) {
              formattedValue = (value / 1000000).toFixed(2) + 'M';
            } else if (value >= 1000) {
              formattedValue = (value / 1000).toFixed(2) + 'K';
            } else {
              formattedValue = value.toLocaleString();
            }
            return `${params.name}<br/>${formattedValue}${unit} (${params.percent}%)`;
          }
        },
        legend: {
          type: 'scroll',
          orient: 'vertical',
          right: 10,
          top: 20,
          bottom: 20,
          textStyle: { fontSize: 11, color: '#666' },
          pageIconColor: '#666',
          pageIconInactiveColor: '#ccc',
          pageTextStyle: { color: '#666' },
          formatter: function(name) {
            const item = data.find(d => d.name === name);
            if (item && total > 0) {
              const percent = ((item.value / total) * 100).toFixed(1);
              return `${name} (${percent}%)`;
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
            borderColor: '#fff',
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

      chart.setOption(option, true);
    }

    // 窗口大小变化时重新调整图表
    window.addEventListener('resize', function() {
      Object.values(chartInstances).forEach(chart => {
        if (chart) chart.resize();
      });
    });
