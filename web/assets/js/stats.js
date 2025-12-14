    // 常量定义
    const STATS_TABLE_COLUMNS = 12; // 统计表列数

    let statsData = null;
    let rpmStats = null; // 全局RPM统计（峰值、平均、最近一分钟）
    let isToday = true;  // 是否为本日（本日才显示最近一分钟）
    let durationSeconds = 0; // 时间跨度（秒），用于计算RPM
    let currentChannelType = 'all'; // 当前选中的渠道类型
    let authTokens = []; // 令牌列表
    let sortState = {
      column: null,
      order: null // null, 'asc', 'desc'
    };

    async function loadStats() {
      try {
        showLoading();

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

        const res = await fetchWithAuth('/admin/stats?' + params.toString());
        if (!res.ok) throw new Error(`HTTP ${res.status}`);

        const response = await res.json();
        // 后端返回格式: {"success":true,"data":{"stats":[...],"duration_seconds":...,"rpm_stats":{...},"is_today":...}}
        statsData = response.data || {stats: []};
        durationSeconds = statsData.duration_seconds || 1; // 防止除零
        rpmStats = statsData.rpm_stats || null;
        isToday = statsData.is_today !== false;

        // 🎯 新增: 初始化时应用默认排序(渠道名称→模型名称)
        applyDefaultSorting();

        renderStatsTable();
        updateStatsCount();
        updateRpmHeader(); // 更新表头标题

      } catch (error) {
        console.error('加载统计数据失败:', error);
        if (window.showError) try { window.showError('无法加载统计数据'); } catch(_){}
        showError();
      }
    }

    function showLoading() {
      const tbody = document.getElementById('stats_tbody');
      tbody.innerHTML = '';
      const row = TemplateEngine.render('tpl-stats-loading', { colspan: STATS_TABLE_COLUMNS });
      if (row) tbody.appendChild(row);
    }

    function showError() {
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

      for (const entry of statsData.stats) {
        const successRate = entry.total > 0 ? ((entry.success / entry.total) * 100) : 0;
        const successRateText = successRate > 0 ? successRate.toFixed(1) + '%' : '';

        // 使用后端返回的 RPM 数据（峰值/平均/最近）
        const rpmHtml = formatEntryRpm(entry, isToday);

        // 根据成功率设置颜色类
        let successRateClass = 'success-rate';
        if (successRate >= 95) successRateClass += ' high';
        else if (successRate > 0 && successRate < 80) successRateClass += ' low';

        const modelDisplay = entry.model ?
          `<span class="model-tag">${escapeHtml(entry.model)}</span>` :
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

        const row = TemplateEngine.render('tpl-stats-row', {
          channelId: entry.channel_id,
          channelName: escapeHtml(entry.channel_name),
          channelIdBadge: entry.channel_id ? `<span class="channel-id">(ID: ${entry.channel_id})</span>` : '',
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
      location.search = '?' + q.toString();
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
      // 更新筛选器统计信息
      const statsCountEl = document.getElementById('statsCount');
      if (statsCountEl && statsData && statsData.stats) {
        statsCountEl.textContent = statsData.stats.length;
      }
    }

    // 根据是否本日更新RPM表头标题
    function updateRpmHeader() {
      const rpmHeader = document.querySelector('[data-column="rpm"]');
      if (rpmHeader) {
        rpmHeader.childNodes[0].textContent = isToday ? 'RPM(峰/均/近)' : 'RPM(峰/均)';
      }
    }

    // 应用默认排序:按渠道名称升序,相同渠道按模型名称升序
    function applyDefaultSorting() {
      if (!statsData || !statsData.stats || statsData.stats.length === 0) return;

      // 保存原始数据副本(仅首次)
      if (!statsData.originalStats) {
        statsData.originalStats = [...statsData.stats];
      }

      // 按渠道名称升序,相同渠道按模型名称升序
      statsData.stats.sort((a, b) => {
        const channelA = (a.channel_name || '').toLowerCase();
        const channelB = (b.channel_name || '').toLowerCase();

        // 首先按渠道名称排序
        const channelCompare = channelA.localeCompare(channelB, 'zh-CN');
        if (channelCompare !== 0) return channelCompare;

        // 渠道名称相同时,按模型名称排序
        const modelA = (a.model || '').toLowerCase();
        const modelB = (b.model || '').toLowerCase();
        return modelA.localeCompare(modelB, 'zh-CN');
      });

      // 重置排序状态(保持无排序指示器显示)
      sortState.column = null;
      sortState.order = null;
    }

    // 加载令牌列表
    async function loadAuthTokens() {
      try {
        const res = await fetchWithAuth('/admin/auth-tokens');
        if (!res.ok) {
          console.error('加载令牌列表失败');
          return;
        }
        const response = await res.json();
        authTokens = response.success ? (response.data || []) : (response || []);

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

    function formatNumber(num) {
      if (num >= 1000000) return (num / 1000000).toFixed(1) + 'M';
      if (num >= 1000) return (num / 1000).toFixed(1) + 'K';
      return num.toString();
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

    // 格式化RPM数值（不带颜色）
    function formatRpmValue(rpm) {
      if (rpm >= 1000) return (rpm / 1000).toFixed(1) + 'K';
      if (rpm >= 1) return rpm.toFixed(1);
      return rpm.toFixed(2);
    }

    // RPM 颜色：低流量绿色，中等橙色，高流量红色
    function getRpmColor(rpm) {
      if (rpm < 10) return 'var(--success-600)';   // 绿色：低流量
      if (rpm < 100) return 'var(--warning-600)';  // 橙色：中等流量
      return 'var(--error-600)';                   // 红色：高流量
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
          authToken: document.getElementById('f_auth_token')?.value || ''
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

      loadStats();
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
