    // 常量定义
    const STATS_TABLE_COLUMNS = 12; // 统计表列数（增加了5列：输入Token、输出Token、缓存读取、缓存创建、成本）

    let statsData = null;
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
        // 后端返回格式: {"success":true,"data":{"stats":[...]}}
        statsData = response.data || {stats: []};

        // 🎯 新增: 初始化时应用默认排序(渠道名称→模型名称)
        applyDefaultSorting();

        renderStatsTable();
        updateStatsCount();

      } catch (error) {
        console.error('加载统计数据失败:', error);
        if (window.showError) try { window.showError('无法加载统计数据'); } catch(_){}
        showError();
      }
    }

    function showLoading() {
      const tbody = document.getElementById('stats_tbody');
      tbody.innerHTML = `
        <tr>
          <td colspan="${STATS_TABLE_COLUMNS}" class="loading-state">
            <div class="loading-spinner" style="margin: 0 auto var(--space-2)"></div>
            正在加载统计数据...
          </td>
        </tr>
      `;
    }

    function showError() {
      const tbody = document.getElementById('stats_tbody');
      tbody.innerHTML = `
        <tr>
          <td colspan="${STATS_TABLE_COLUMNS}" class="empty-state">
            <svg class="w-12 h-12 mx-auto mb-4" style="color: var(--error-400);" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-2.5L13.732 4c-.77-.833-1.864-.833-2.634 0L4.18 16.5c-.77.833.192 2.5 1.732 2.5z"/>
            </svg>
            <div style="color: var(--error-400); font-weight: var(--font-medium); margin-bottom: var(--space-1);">加载失败</div>
            <div>请检查网络连接或重试</div>
          </td>
        </tr>
      `;
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
          case 'total':
            valueA = a.total || 0;
            valueB = b.total || 0;
            break;
          case 'success_rate':
            valueA = a.total > 0 ? (a.success / a.total) : 0;
            valueB = b.total > 0 ? (b.success / b.total) : 0;
            break;
          case 'avg_first_byte_time':
            valueA = a.avg_first_byte_time_seconds || 0;
            valueB = b.avg_first_byte_time_seconds || 0;
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
        tbody.innerHTML = `
          <tr>
            <td colspan="${STATS_TABLE_COLUMNS}" class="empty-state">
              <svg class="w-12 h-12 mx-auto mb-4" style="color: var(--neutral-400);" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1" d="M9 19v-6a2 2 0 00-2-2H5a2 2 0 00-2 2v6a2 2 0 002 2h2a2 2 0 002-2zm0 0V9a2 2 0 012-2h2a2 2 0 012 2v10m-6 0a2 2 0 002 2h2a2 2 0 002-2m0 0V5a2 2 0 012-2h2a2 2 0 012 2v14a2 2 0 01-2 2h-2a2 2 0 01-2-2z"/>
              </svg>
              <div style="font-weight: var(--font-medium); margin-bottom: var(--space-1); color: var(--neutral-700);">暂无统计数据</div>
              <div>请调整筛选条件或检查时间范围</div>
            </td>
          </tr>
        `;
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

      for (const entry of statsData.stats) {
        const tr = document.createElement('tr');

        const successRate = entry.total > 0 ? ((entry.success / entry.total) * 100) : 0;
        const successRateText = successRate.toFixed(1) + '%';

        // 根据成功率设置颜色类
        let successRateClass = 'success-rate';
        if (successRate >= 95) successRateClass += ' high';
        else if (successRate < 80) successRateClass += ' low';

        const modelDisplay = entry.model ?
          `<span class="model-tag">${escapeHtml(entry.model)}</span>` :
          '<span style="color: var(--neutral-500);">未知模型</span>';

        // 格式化平均首字响应时间（保留2位小数，显示"秒"）
        const avgFirstByteTime = entry.avg_first_byte_time_seconds || 0;
        const avgFirstByteTimeText = avgFirstByteTime > 0 ?
          `${avgFirstByteTime.toFixed(2)}秒` :
          '<span style="color: var(--neutral-400);">--</span>';

        // 格式化Token数据
        const inputTokensText = entry.total_input_tokens ? formatNumber(entry.total_input_tokens) : '<span style="color: var(--neutral-400);">--</span>';
        const outputTokensText = entry.total_output_tokens ? formatNumber(entry.total_output_tokens) : '<span style="color: var(--neutral-400);">--</span>';
        const cacheReadTokensText = entry.total_cache_read_input_tokens ?
          `<span style="color: var(--success-600);">${formatNumber(entry.total_cache_read_input_tokens)}</span>` :
          '<span style="color: var(--neutral-400);">--</span>';
        const cacheCreationTokensText = entry.total_cache_creation_input_tokens ?
          `<span style="color: var(--primary-600);">${formatNumber(entry.total_cache_creation_input_tokens)}</span>` :
          '<span style="color: var(--neutral-400);">--</span>';
        const costText = entry.total_cost ?
          `<span style="color: var(--warning-600); font-weight: 500;">${formatCost(entry.total_cost)}</span>` :
          '<span style="color: var(--neutral-400);">--</span>';

        tr.innerHTML = `
          <td>
            <a href="/web/channels.html?id=${entry.channel_id}#channel-${entry.channel_id}" class="config-name channel-link" title="跳转到渠道管理">
              ${escapeHtml(entry.channel_name)}
            </a>
            ${entry.channel_id ? `<span class="channel-id">(ID: ${entry.channel_id})</span>` : ''}
          </td>
          <td>${modelDisplay}</td>
          <td><span class="success-count">${formatNumber(entry.success || 0)}</span></td>
          <td><span class="error-count">${formatNumber(entry.error || 0)}</span></td>
          <td><strong>${formatNumber(entry.total || 0)}</strong></td>
          <td>
            <div class="${successRateClass}">${successRateText}</div>
            <div class="progress-bar">
              <div class="progress-fill" style="width: ${successRate}%"></div>
            </div>
          </td>
          <td style="text-align: center;">
            ${avgFirstByteTimeText}
          </td>
          <td style="text-align: right;">${inputTokensText}</td>
          <td style="text-align: right;">${outputTokensText}</td>
          <td style="text-align: right;">${cacheReadTokensText}</td>
          <td style="text-align: right;">${cacheCreationTokensText}</td>
          <td style="text-align: right;">${costText}</td>
        `;
        tbody.appendChild(tr);

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

      // 追加合计行
      const totalRow = document.createElement('tr');
      totalRow.style.backgroundColor = 'var(--primary-50)';
      totalRow.style.fontWeight = 'bold';
      totalRow.style.borderTop = '2px solid var(--primary-200)';

      const totalSuccessRate = totalRequests > 0 ? ((totalSuccess / totalRequests) * 100).toFixed(1) + '%' : '0.0%';

      totalRow.innerHTML = `
        <td colspan="2" style="text-align: center; font-size: 15px; color: var(--primary-700);">合计</td>
        <td><span class="success-count">${formatNumber(totalSuccess)}</span></td>
        <td><span class="error-count">${formatNumber(totalError)}</span></td>
        <td><strong>${formatNumber(totalRequests)}</strong></td>
        <td style="text-align: center; font-size: 14px;">${totalSuccessRate}</td>
        <td style="text-align: center; color: var(--neutral-400);">--</td>
        <td style="text-align: right;">${formatNumber(totalInputTokens)}</td>
        <td style="text-align: right;">${formatNumber(totalOutputTokens)}</td>
        <td style="text-align: right;"><span style="color: var(--success-600);">${formatNumber(totalCacheRead)}</span></td>
        <td style="text-align: right;"><span style="color: var(--primary-600);">${formatNumber(totalCacheCreation)}</span></td>
        <td style="text-align: right;"><span style="color: var(--warning-600); font-weight: 600;">${formatCost(totalCost)}</span></td>
      `;
      tbody.appendChild(totalRow);
    }

    function applyFilter() {
      const range = document.getElementById('f_hours').value.trim();
      const id = document.getElementById('f_id').value.trim();
      const name = document.getElementById('f_name').value.trim();
      const model = document.getElementById('f_model').value.trim();
      const authToken = document.getElementById('f_auth_token').value.trim();

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
      const id = u.get('channel_id') || '';
      const name = u.get('channel_name_like') || u.get('channel_name') || '';
      const range = u.get('range') || 'today';
      const model = u.get('model_like') || u.get('model') || '';
      const authToken = u.get('auth_token_id') || '';

      // 初始化时间范围选择器 (默认"本日")
      if (window.initDateRangeSelector) {
        initDateRangeSelector('f_hours', 'today', null);
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

      // 事件监听
      document.getElementById('btn_filter').addEventListener('click', applyFilter);

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

    // 格式化成本（美元）- 复用logs.html的逻辑
    function formatCost(cost) {
      if (cost === 0) return '$0.00';
      if (cost < 0.001) {
        // 小额成本：使用更多小数位
        if (cost < 0.000001) {
          return '$' + cost.toExponential(2); // 科学计数法
        }
        return '$' + cost.toFixed(6).replace(/\.?0+$/, ''); // 最多6位小数，去除尾随0
      }
      if (cost >= 1.0) {
        return '$' + cost.toFixed(2); // 大于$1显示2位小数
      }
      return '$' + cost.toFixed(4).replace(/\.?0+$/, ''); // 否则显示4位小数，去除尾随0
    }

    function escapeHtml(str) {
      if (!str) return '';
      return str.replace(/[&<>"']/g, c => ({
        '&': '&amp;',
        '<': '&lt;',
        '>': '&gt;',
        '"': '&quot;',
        "'": '&#39;'
      }[c]));
    }


    // 注销功能（已由 ui.js 的 onLogout 统一处理）

    // 顶栏布局下，无需侧栏响应逻辑
    function handleResize() {}

    // localStorage key for stats page filters
    const STATS_FILTER_KEY = 'stats.filters';

    function saveStatsFilters() {
      try {
        localStorage.setItem(STATS_FILTER_KEY, JSON.stringify({ channelType: currentChannelType }));
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

      // 优先从 localStorage 恢复，默认 all
      const savedFilters = loadStatsFilters();
      currentChannelType = savedFilters?.channelType || 'all';

      await initChannelTypeFilter(currentChannelType);

      initFilters();
      loadStats();

      // 响应式处理
      handleResize();
      window.addEventListener('resize', handleResize);
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
