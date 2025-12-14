    let currentLogsPage = 1;
    let logsPageSize = 15;
    let totalLogsPages = 1;
    let totalLogs = 0;
    let currentChannelType = 'all'; // 当前选中的渠道类型
    let authTokens = []; // 令牌列表
    let defaultTestContent = 'sonnet 4.0的发布日期是什么'; // 默认测试内容（从设置加载）

    // 加载默认测试内容（从系统设置）
    async function loadDefaultTestContent() {
      try {
        const resp = await fetchWithAuth('/admin/settings/channel_test_content');
        const data = await resp.json();
        if (data.success && data.data?.value) {
          defaultTestContent = data.data.value;
        }
      } catch (e) {
        console.warn('加载默认测试内容失败，使用内置默认值', e);
      }
    }

    async function load() {
      try {
        showLoading();

        // 从表单元素获取筛选条件（支持下拉框切换后立即生效）
        const range = document.getElementById('f_hours')?.value || 'today';
        const channelId = document.getElementById('f_id')?.value?.trim() || '';
        const channelName = document.getElementById('f_name')?.value?.trim() || '';
        const model = document.getElementById('f_model')?.value?.trim() || '';
        const statusCode = document.getElementById('f_status')?.value?.trim() || '';
        const authTokenId = document.getElementById('f_auth_token')?.value?.trim() || '';

        const params = new URLSearchParams({
          range,
          limit: logsPageSize.toString(),
          offset: ((currentLogsPage - 1) * logsPageSize).toString()
        });

        if (channelId) params.set('channel_id', channelId);
        if (channelName) params.set('channel_name_like', channelName);
        if (model) params.set('model_like', model);
        if (statusCode) params.set('status_code', statusCode);
        if (authTokenId) params.set('auth_token_id', authTokenId);

        // 添加渠道类型筛选
        if (currentChannelType && currentChannelType !== 'all') {
          params.set('channel_type', currentChannelType);
        }
        
        const res = await fetchWithAuth('/admin/errors?' + params.toString());
        if (!res.ok) throw new Error(`HTTP ${res.status}`);

        const response = await res.json();
        const result = response.success ? response.data : response;
        const data = result.data || result || [];

        // 精确计算总页数（基于后端返回的total字段）
        if (result.total !== undefined) {
          totalLogs = result.total;
          totalLogsPages = Math.ceil(totalLogs / logsPageSize) || 1;
        } else {
          // 降级方案：后端未返回total时使用旧逻辑
          if (data.length === logsPageSize) {
            totalLogsPages = Math.max(currentLogsPage + 1, totalLogsPages);
          } else if (data.length < logsPageSize && currentLogsPage === 1) {
            totalLogsPages = 1;
          } else if (data.length < logsPageSize) {
            totalLogsPages = currentLogsPage;
          }
        }

        updatePagination();
        renderLogs(data);
        updateStats(data);

      } catch (error) {
        console.error('加载日志失败:', error);
        try { if (window.showError) window.showError('无法加载请求日志'); } catch(_){}
        showError();
      }
    }

    // ✅ 动态计算列数（避免硬编码维护成本）
    function getTableColspan() {
      const headerCells = document.querySelectorAll('thead th');
      return headerCells.length || 13; // fallback到13列（向后兼容）
    }

    function showLoading() {
      const tbody = document.getElementById('tbody');
      const colspan = getTableColspan();
      const loadingRow = TemplateEngine.render('tpl-log-loading', { colspan });
      tbody.innerHTML = '';
      if (loadingRow) tbody.appendChild(loadingRow);
    }

    function showError() {
      const tbody = document.getElementById('tbody');
      const colspan = getTableColspan();
      const errorRow = TemplateEngine.render('tpl-log-error', { colspan });
      tbody.innerHTML = '';
      if (errorRow) tbody.appendChild(errorRow);
    }

    function renderLogs(data) {
      const tbody = document.getElementById('tbody');
      const colspan = getTableColspan();

      if (data.length === 0) {
        const emptyRow = TemplateEngine.render('tpl-log-empty', { colspan });
        tbody.innerHTML = '';
        if (emptyRow) tbody.appendChild(emptyRow);
        return;
      }

      tbody.innerHTML = '';

      for (const entry of data) {
        // === 预处理数据：构建复杂HTML片段 ===

        // 0. 客户端IP显示
        const clientIPDisplay = entry.client_ip ?
          escapeHtml(entry.client_ip) :
          '<span style="color: var(--neutral-400);">-</span>';

        // 1. 渠道信息显示
        const configInfo = entry.channel_name ||
          (entry.channel_id ? `渠道 #${entry.channel_id}` :
           (entry.message === 'exhausted backends' ? '系统（所有渠道失败）' :
            entry.message === 'no available upstream (all cooled or none)' ? '系统（无可用渠道）' : '系统'));
        const configDisplay = entry.channel_id ?
          `<a class="channel-link" href="/web/channels.html?id=${entry.channel_id}#channel-${entry.channel_id}">${escapeHtml(entry.channel_name||'')} <small>(#${entry.channel_id})</small></a>` :
          `<span style="color: var(--neutral-500);">${escapeHtml(configInfo)}</span>`;

        // 2. 状态码样式
        const statusClass = (entry.status_code >= 200 && entry.status_code < 300) ?
          'status-success' : 'status-error';
        const statusCode = entry.status_code;

        // 3. 模型显示
        const modelDisplay = entry.model ?
          `<span class="model-tag">${escapeHtml(entry.model)}</span>` :
          '<span style="color: var(--neutral-500);">-</span>';

        // 4. 响应时间显示(流式/非流式)
        const hasDuration = entry.duration !== undefined && entry.duration !== null;
        const durationDisplay = hasDuration ?
          `<span style="color: var(--neutral-700);">${entry.duration.toFixed(3)}</span>` :
          '<span style="color: var(--neutral-500);">-</span>';

        const streamFlag = entry.is_streaming ?
          '<span class="stream-flag">流</span>' :
          '<span class="stream-flag placeholder">流</span>';

        let responseTimingDisplay;
        if (entry.is_streaming) {
          const hasFirstByte = entry.first_byte_time !== undefined && entry.first_byte_time !== null;
          const firstByteDisplay = hasFirstByte ?
            `<span style="color: var(--success-600);">${entry.first_byte_time.toFixed(3)}</span>` :
            '<span style="color: var(--neutral-500);">-</span>';
          responseTimingDisplay = `
            <span style="display: inline-flex; align-items: center; justify-content: flex-end; gap: 4px; white-space: nowrap;">
              ${firstByteDisplay}
              <span style="color: var(--neutral-400);">/</span>
              ${durationDisplay}
            </span>
            ${streamFlag}
          `;
        } else {
          responseTimingDisplay = `
            <span style="display: inline-flex; align-items: center; justify-content: flex-end; gap: 4px; white-space: nowrap;">
              ${durationDisplay}
            </span>
            ${streamFlag}
          `;
        }

        // 5. API Key显示(含按钮组)
        let apiKeyDisplay = '';
        if (entry.api_key_used && entry.channel_id && entry.model) {
          const statusCode = entry.status_code || 0;
          const showTestBtn = statusCode !== 200;
          const showDeleteBtn = statusCode === 403;

          let buttons = '';
          if (showTestBtn) {
            buttons += `
              <button
                class="test-key-btn"
                data-action="test"
                data-channel-id="${entry.channel_id}"
                data-channel-name="${escapeHtml(entry.channel_name || '').replace(/"/g, '&quot;')}"
                data-api-key="${escapeHtml(entry.api_key_used).replace(/"/g, '&quot;')}"
                data-model="${escapeHtml(entry.model).replace(/"/g, '&quot;')}"
                title="测试此 API Key">
                ⚡
              </button>
            `;
          }
          if (showDeleteBtn) {
            buttons += `
              <button
                class="test-key-btn"
                style="color: var(--error-600);"
                data-action="delete"
                data-channel-id="${entry.channel_id}"
                data-channel-name="${escapeHtml(entry.channel_name || '').replace(/"/g, '&quot;')}"
                data-api-key="${escapeHtml(entry.api_key_used).replace(/"/g, '&quot;')}"
                title="删除此 API Key">
                🗑
              </button>
            `;
          }

          apiKeyDisplay = `
            <div style="display: flex; align-items: center; gap: 6px; justify-content: center;">
              <code style="font-size: 0.9em; color: var(--neutral-600);">${escapeHtml(entry.api_key_used)}</code>
              ${buttons}
            </div>
          `;
        } else if (entry.api_key_used) {
          apiKeyDisplay = `<code style="font-size: 0.9em; color: var(--neutral-600);">${escapeHtml(entry.api_key_used)}</code>`;
        } else {
          apiKeyDisplay = '<span style="color: var(--neutral-500);">-</span>';
        }

        // 6. Token统计显示(0值为空)
        const tokenValue = (value, color) => {
          if (value === undefined || value === null || value === 0) {
            return '';
          }
          return `<span class="token-metric-value" style="color: ${color};">${value.toLocaleString()}</span>`;
        };
        const inputTokensDisplay = tokenValue(entry.input_tokens, 'var(--neutral-700)');
        const outputTokensDisplay = tokenValue(entry.output_tokens, 'var(--neutral-700)');
        const cacheReadDisplay = tokenValue(entry.cache_read_input_tokens, 'var(--success-600)');
        const cacheCreationDisplay = tokenValue(entry.cache_creation_input_tokens, 'var(--primary-600)');

        // 7. 成本显示(0值为空)
        const costDisplay = entry.cost ?
          `<span style="color: var(--warning-600); font-weight: 500;">${formatCost(entry.cost)}</span>` :
          '';

        // === 渲染行 ===
        const rowEl = TemplateEngine.render('tpl-log-row', {
          time: formatTime(entry.time),
          clientIPDisplay,
          modelDisplay,
          configDisplay,
          apiKeyDisplay,
          statusClass,
          statusCode,
          responseTimingDisplay,
          inputTokensDisplay,
          outputTokensDisplay,
          cacheReadDisplay,
          cacheCreationDisplay,
          costDisplay,
          message: entry.message || ''
        });
        if (rowEl) tbody.appendChild(rowEl);
      }
    }

    function updatePagination() {
      // 更新页码显示（只更新底部分页）
      const currentPage2El = document.getElementById('logs_current_page2');
      const totalPages2El = document.getElementById('logs_total_pages2');
      const prev2El = document.getElementById('logs_prev2');
      const next2El = document.getElementById('logs_next2');
      const jumpPageInput = document.getElementById('logs_jump_page');

      if (currentPage2El) currentPage2El.textContent = currentLogsPage;
      if (totalPages2El) totalPages2El.textContent = totalLogsPages;

      // 更新跳转输入框的max属性
      if (jumpPageInput) {
        jumpPageInput.max = totalLogsPages;
        jumpPageInput.placeholder = `1-${totalLogsPages}`;
      }

      // 更新按钮状态（只更新底部分页）
      const prevDisabled = currentLogsPage <= 1;
      const nextDisabled = currentLogsPage >= totalLogsPages;

      if (prev2El) prev2El.disabled = prevDisabled;
      if (next2El) next2El.disabled = nextDisabled;
    }

    function updateStats(data) {
      // 更新筛选器统计信息
      const displayedCountEl = document.getElementById('displayedCount');
      const totalCountEl = document.getElementById('totalCount');

      if (displayedCountEl) displayedCountEl.textContent = data.length;
      if (totalCountEl) totalCountEl.textContent = totalLogs || data.length;
    }

    function prevLogsPage() {
      if (currentLogsPage > 1) {
        currentLogsPage--;
        load();
      }
    }

    function nextLogsPage() {
      if (currentLogsPage < totalLogsPages) {
        currentLogsPage++;
        load();
      }
    }

    function jumpToPage() {
      const jumpPageInput = document.getElementById('logs_jump_page');
      if (!jumpPageInput) return;

      const targetPage = parseInt(jumpPageInput.value);

      // 输入验证
      if (isNaN(targetPage) || targetPage < 1 || targetPage > totalLogsPages) {
        jumpPageInput.value = ''; // 清空无效输入
        if (window.showError) {
          try {
            window.showError(`请输入有效的页码 (1-${totalLogsPages})`);
          } catch(_) {}
        }
        return;
      }

      // 跳转到目标页
      if (targetPage !== currentLogsPage) {
        currentLogsPage = targetPage;
        load();
      }

      // 清空输入框
      jumpPageInput.value = '';
    }

    function changePageSize() {
      const newPageSize = parseInt(document.getElementById('page_size').value);
      if (newPageSize !== logsPageSize) {
        logsPageSize = newPageSize;
        currentLogsPage = 1;
        totalLogsPages = 1;
        load();
      }
    }

    function applyFilter() {
      currentLogsPage = 1;
      totalLogsPages = 1;

      const range = document.getElementById('f_hours').value.trim();
      const id = document.getElementById('f_id').value.trim();
      const name = document.getElementById('f_name').value.trim();
      const model = document.getElementById('f_model').value.trim();
      const status = document.getElementById('f_status') ? document.getElementById('f_status').value.trim() : '';
      const authToken = document.getElementById('f_auth_token').value.trim();
      const channelType = document.getElementById('f_channel_type').value.trim();

      // 保存筛选条件到 localStorage
      saveLogsFilters();

      const q = new URLSearchParams(location.search);

      if (range) q.set('range', range); else q.delete('range');
      if (id) q.set('channel_id', id); else q.delete('channel_id');
      if (name) { q.set('channel_name_like', name); q.delete('channel_name'); }
      else { q.delete('channel_name_like'); }
      if (model) { q.set('model_like', model); q.delete('model'); }
      else { q.delete('model_like'); q.delete('model'); }
      if (status) { q.set('status_code', status); }
      else { q.delete('status_code'); }
      if (authToken) q.set('auth_token_id', authToken); else q.delete('auth_token_id');
      if (channelType) q.set('channel_type', channelType); else q.set('channel_type', 'all');

      location.search = '?' + q.toString();
    }

    function initFilters() {
      const u = new URLSearchParams(location.search);
      const saved = loadLogsFilters();
      // URL 参数优先，否则从 localStorage 恢复
      const hasUrlParams = u.toString().length > 0;

      const id = u.get('channel_id') || (!hasUrlParams && saved?.channelId) || '';
      const name = u.get('channel_name_like') || u.get('channel_name') || (!hasUrlParams && saved?.channelName) || '';
      const range = u.get('range') || (!hasUrlParams && saved?.range) || 'today';
      const model = u.get('model_like') || u.get('model') || (!hasUrlParams && saved?.model) || '';
      const status = u.get('status_code') || (!hasUrlParams && saved?.status) || '';
      const authToken = u.get('auth_token_id') || (!hasUrlParams && saved?.authToken) || '';
      const channelType = u.get('channel_type') || (!hasUrlParams && saved?.channelType) || 'all';

      // 初始化时间范围选择器 (默认"本日")，切换后立即筛选
      if (window.initDateRangeSelector) {
        initDateRangeSelector('f_hours', 'today', () => {
          saveLogsFilters();
          currentLogsPage = 1;
          load();
        });
        // 设置URL中的值
        document.getElementById('f_hours').value = range;
      }

      document.getElementById('f_id').value = id;
      document.getElementById('f_name').value = name;
      document.getElementById('f_model').value = model;
      const statusEl = document.getElementById('f_status');
      if (statusEl) statusEl.value = status;

      // 设置渠道类型
      currentChannelType = channelType;
      const channelTypeEl = document.getElementById('f_channel_type');
      if (channelTypeEl) channelTypeEl.value = channelType;

      // 加载令牌列表
      loadAuthTokens().then(() => {
        document.getElementById('f_auth_token').value = authToken;
      });

      // 令牌选择器切换后立即筛选
      document.getElementById('f_auth_token').addEventListener('change', () => {
        saveLogsFilters();
        currentLogsPage = 1;
        load();
      });

      // 事件监听
      document.getElementById('btn_filter').addEventListener('click', applyFilter);

      // 输入框自动筛选（防抖）
      const debouncedFilter = debounce(applyFilter, 500);
      ['f_id', 'f_name', 'f_model', 'f_status'].forEach(id => {
        const el = document.getElementById(id);
        if (el) {
          el.addEventListener('input', debouncedFilter);
        }
      });

      // 回车键筛选
      ['f_hours', 'f_id', 'f_name', 'f_model', 'f_status', 'f_auth_token', 'f_channel_type'].forEach(id => {
        const el = document.getElementById(id);
        if (el) {
          el.addEventListener('keydown', e => {
            if (e.key === 'Enter') applyFilter();
          });
        }
      });
    }

    function formatTime(timeStr) {
      try {
        // 处理Unix timestamp（秒）或ISO字符串
        let timestamp = timeStr;
        if (typeof timeStr === 'number' || /^\d+$/.test(timeStr)) {
          // Unix timestamp（秒）转换为毫秒
          timestamp = parseInt(timeStr) * 1000;
        }

        const date = new Date(timestamp);
        if (isNaN(date.getTime()) || date.getFullYear() < 2020) {
          return '-';
        }
        return date.toLocaleString('zh-CN', {
          year: 'numeric',
          month: '2-digit',
          day: '2-digit',
          hour: '2-digit',
          minute: '2-digit',
          second: '2-digit'
        });
      } catch (e) {
        return '-';
      }
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

    function parseApiKeysFromChannel(channel) {
      if (!channel) return [];
      // 优先支持新结构：api_keys 为对象数组
      if (Array.isArray(channel.api_keys)) {
        return channel.api_keys
          .map(k => (k && (k.api_key || k.key)) || '')
          .map(k => k.trim())
          .filter(k => k);
      }
      // 向后兼容：api_key 为逗号分隔的字符串
      if (typeof channel.api_key === 'string') {
        return channel.api_key
          .split(',')
          .map(k => k.trim())
          .filter(k => k);
      }
      return [];
    }

    function maskKeyForCompare(key) {
      if (!key) return '';
      if (key.length <= 8) return key;
      return `${key.slice(0, 4)}...${key.slice(-4)}`;
    }

    function findKeyIndexByMaskedKey(keys, maskedKey) {
      if (!maskedKey || !keys || !keys.length) return null;
      const target = maskedKey.trim();
      for (let i = 0; i < keys.length; i++) {
        if (maskKeyForCompare(keys[i]) === target) return i;
      }
      return null;
    }

    function updateTestKeyIndexInfo(text) {
      const el = document.getElementById('testKeyIndexInfo');
      if (el) el.textContent = text || '';
    }

    // 注销功能（已由 ui.js 的 onLogout 统一处理）

    // localStorage key for logs page filters
    const LOGS_FILTER_KEY = 'logs.filters';

    function saveLogsFilters() {
      try {
        const filters = {
          channelType: document.getElementById('f_channel_type')?.value || 'all',
          range: document.getElementById('f_hours')?.value || 'today',
          channelId: document.getElementById('f_id')?.value || '',
          channelName: document.getElementById('f_name')?.value || '',
          model: document.getElementById('f_model')?.value || '',
          status: document.getElementById('f_status')?.value || '',
          authToken: document.getElementById('f_auth_token')?.value || ''
        };
        localStorage.setItem(LOGS_FILTER_KEY, JSON.stringify(filters));
      } catch (_) {}
    }

    function loadLogsFilters() {
      try {
        const saved = localStorage.getItem(LOGS_FILTER_KEY);
        if (saved) return JSON.parse(saved);
      } catch (_) {}
      return null;
    }

    // 页面初始化
    document.addEventListener('DOMContentLoaded', async function() {
      if (window.initTopbar) initTopbar('logs');

      // 优先从 URL 读取，其次从 localStorage 恢复，默认 all
      const u = new URLSearchParams(location.search);
      const hasUrlParams = u.toString().length > 0;
      const savedFilters = loadLogsFilters();
      currentChannelType = u.get('channel_type') || (!hasUrlParams && savedFilters?.channelType) || 'all';

      await initChannelTypeFilter(currentChannelType);

      initFilters();
      await loadDefaultTestContent();

      // ✅ 修复：如果没有 URL 参数但有保存的筛选条件，先同步 URL 再加载数据
      if (!hasUrlParams && savedFilters) {
        const q = new URLSearchParams();
        if (savedFilters.range) q.set('range', savedFilters.range);
        if (savedFilters.channelId) q.set('channel_id', savedFilters.channelId);
        if (savedFilters.channelName) q.set('channel_name_like', savedFilters.channelName);
        if (savedFilters.model) q.set('model_like', savedFilters.model);
        if (savedFilters.status) q.set('status_code', savedFilters.status);
        if (savedFilters.authToken) q.set('auth_token_id', savedFilters.authToken);
        if (savedFilters.channelType && savedFilters.channelType !== 'all') {
          q.set('channel_type', savedFilters.channelType);
        }
        // 使用 replaceState 更新 URL，不触发页面刷新
        if (q.toString()) {
          history.replaceState(null, '', '?' + q.toString());
        }
      }

      load();

      // ESC键关闭测试模态框
      document.addEventListener('keydown', (e) => {
        if (e.key === 'Escape') {
          closeTestKeyModal();
        }
      });

      // 事件委托：处理日志表格中的按钮点击
      const tbody = document.getElementById('tbody');
      if (tbody) {
        tbody.addEventListener('click', (e) => {
          const btn = e.target.closest('.test-key-btn[data-action]');
          if (!btn) return;

          const action = btn.dataset.action;
          const channelId = parseInt(btn.dataset.channelId);
          const channelName = btn.dataset.channelName || '';
          const apiKey = btn.dataset.apiKey || '';
          const model = btn.dataset.model || '';

          if (action === 'test') {
            testKey(channelId, channelName, apiKey, model);
          } else if (action === 'delete') {
            deleteKeyFromLog(channelId, channelName, apiKey);
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
      const allOption = document.createElement('option');
      allOption.value = 'all';
      allOption.textContent = '全部';
      if (!initialType || initialType === 'all') {
        allOption.selected = true;
      }
      select.innerHTML = '';
      select.appendChild(allOption);

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
        saveLogsFilters();
        // 切换渠道类型时重置到第一页并重新加载
        currentLogsPage = 1;
        load();
      });
    }

    // ========== API Key 测试功能 ==========
    let testingKeyData = null;

    async function testKey(channelId, channelName, apiKey, model) {
      testingKeyData = {
        channelId,
        channelName,
        maskedApiKey: apiKey,
        originalModel: model,
        channelType: null, // 将在异步加载渠道配置后填充
        keyIndex: null
      };

      // 填充模态框基本信息
      document.getElementById('testKeyChannelName').textContent = channelName;
      document.getElementById('testKeyDisplay').textContent = apiKey;
      document.getElementById('testKeyOriginalModel').textContent = model;

      // 重置状态
      resetTestKeyModal();
      updateTestKeyIndexInfo('');

      // 显示模态框
      document.getElementById('testKeyModal').classList.add('show');

      // 异步加载渠道配置以获取支持的模型列表
      try {
        const res = await fetchWithAuth(`/admin/channels/${channelId}`);
        if (!res.ok) throw new Error('HTTP ' + res.status);

        const response = await res.json();
        const channel = response.success ? response.data : response;

        // ✅ 保存渠道类型,用于后续测试请求
        testingKeyData.channelType = channel.channel_type || 'anthropic';
        const apiKeys = parseApiKeysFromChannel(channel);
        const matchedIndex = findKeyIndexByMaskedKey(apiKeys, apiKey);
        testingKeyData.keyIndex = matchedIndex;
        if (apiKeys.length > 0) {
          updateTestKeyIndexInfo(
            matchedIndex !== null
              ? `匹配到 Key #${matchedIndex + 1}，按日志所用Key测试`
              : '未匹配到日志中的 Key，将按默认顺序测试'
          );
        } else {
          updateTestKeyIndexInfo('未获取到渠道 Key，将按默认顺序测试');
        }

        // 填充模型下拉列表
        const modelSelect = document.getElementById('testKeyModel');
        modelSelect.innerHTML = '';

        if (channel.models && channel.models.length > 0) {
          channel.models.forEach(m => {
            const option = document.createElement('option');
            option.value = m;
            option.textContent = m;
            modelSelect.appendChild(option);
          });

          // 如果日志中的模型在支持列表中，则预选；否则选择第一个
          if (channel.models.includes(model)) {
            modelSelect.value = model;
          } else {
            modelSelect.value = channel.models[0];
          }
        } else {
          // 没有配置模型，使用日志中的模型
          const option = document.createElement('option');
          option.value = model;
          option.textContent = model;
          modelSelect.appendChild(option);
          modelSelect.value = model;
        }
      } catch (e) {
        console.error('加载渠道配置失败', e);
        // 降级方案：使用日志中的模型
        const modelSelect = document.getElementById('testKeyModel');
        modelSelect.innerHTML = '';
        const option = document.createElement('option');
        option.value = model;
        option.textContent = model;
        modelSelect.appendChild(option);
        modelSelect.value = model;
        updateTestKeyIndexInfo('渠道配置加载失败，将按默认顺序测试');
      }
    }

    function closeTestKeyModal() {
      document.getElementById('testKeyModal').classList.remove('show');
      testingKeyData = null;
    }

    function resetTestKeyModal() {
      document.getElementById('testKeyProgress').classList.remove('show');
      document.getElementById('testKeyResult').classList.remove('show', 'success', 'error');
      document.getElementById('runKeyTestBtn').disabled = false;
      document.getElementById('testKeyContent').value = defaultTestContent;
      document.getElementById('testKeyStream').checked = true;
      updateTestKeyIndexInfo('');
      // 重置模型选择框
      const modelSelect = document.getElementById('testKeyModel');
      modelSelect.innerHTML = '<option value="">加载中...</option>';
    }

    async function runKeyTest() {
      if (!testingKeyData) return;

      const modelSelect = document.getElementById('testKeyModel');
      const contentInput = document.getElementById('testKeyContent');
      const streamCheckbox = document.getElementById('testKeyStream');
      const selectedModel = modelSelect.value;
      const testContent = contentInput.value.trim() || defaultTestContent;
      const streamEnabled = streamCheckbox.checked;

      if (!selectedModel) {
        if (window.showError) showError('请选择一个测试模型');
        return;
      }

      // 显示进度
      document.getElementById('testKeyProgress').classList.add('show');
      document.getElementById('testKeyResult').classList.remove('show');
      document.getElementById('runKeyTestBtn').disabled = true;

      try {
        // 构建测试请求（使用用户选择的模型）
        const testRequest = {
          model: selectedModel,
          max_tokens: 512,
          stream: streamEnabled,
          content: testContent,
          channel_type: testingKeyData.channelType || 'anthropic' // ✅ 添加渠道类型
        };
        if (testingKeyData && testingKeyData.keyIndex !== null && testingKeyData.keyIndex !== undefined) {
          testRequest.key_index = testingKeyData.keyIndex;
        }

        const res = await fetchWithAuth(`/admin/channels/${testingKeyData.channelId}/test`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(testRequest)
        });

        if (!res.ok) {
          throw new Error('HTTP ' + res.status);
        }

        const result = await res.json();
        const testResult = result.data || result;

        displayKeyTestResult(testResult);
      } catch (e) {
        console.error('测试失败', e);
        displayKeyTestResult({
          success: false,
          error: '测试请求失败: ' + e.message
        });
      } finally {
        document.getElementById('testKeyProgress').classList.remove('show');
        document.getElementById('runKeyTestBtn').disabled = false;
      }
    }

    function displayKeyTestResult(result) {
      const testResultDiv = document.getElementById('testKeyResult');
      const contentDiv = document.getElementById('testKeyResultContent');
      const detailsDiv = document.getElementById('testKeyResultDetails');

      testResultDiv.classList.remove('success', 'error');
      testResultDiv.classList.add('show');

      if (result.success) {
        testResultDiv.classList.add('success');
        contentDiv.innerHTML = `
          <div style="display: flex; align-items: center; gap: 8px;">
            <span style="font-size: 18px;">✅</span>
            <strong>${escapeHtml(result.message || 'API测试成功')}</strong>
          </div>
        `;

        let details = `响应时间: ${result.duration_ms}ms`;
        if (result.status_code) {
          details += ` | 状态码: ${result.status_code}`;
        }

        // 显示响应文本
        if (result.response_text) {
          details += `
            <div style="margin-top: 12px;">
              <h4 style="margin-bottom: 8px; color: var(--neutral-700);">API 响应内容</h4>
              <div style="padding: 12px; background: var(--neutral-50); border-radius: 4px; border: 1px solid var(--neutral-200); color: var(--neutral-700); white-space: pre-wrap; font-family: monospace; font-size: 0.9em; max-height: 300px; overflow-y: auto;">${escapeHtml(result.response_text)}</div>
            </div>
          `;
        }

        // 显示完整API响应
        if (result.api_response) {
          const responseId = 'api-response-' + Date.now();
          details += `
            <div style="margin-top: 12px;">
              <h4 style="margin-bottom: 8px; color: var(--neutral-700);">完整 API 响应</h4>
              <button class="btn btn-secondary btn-sm" onclick="toggleResponse('${responseId}')" style="margin-bottom: 8px;">显示/隐藏 JSON</button>
              <div id="${responseId}" style="display: none; padding: 12px; background: var(--neutral-50); border-radius: 4px; border: 1px solid var(--neutral-200); color: var(--neutral-700); white-space: pre-wrap; font-family: monospace; font-size: 0.85em; max-height: 400px; overflow-y: auto;">${escapeHtml(JSON.stringify(result.api_response, null, 2))}</div>
            </div>
          `;
        }

        detailsDiv.innerHTML = details;
      } else {
        testResultDiv.classList.add('error');
        contentDiv.innerHTML = `
          <div style="display: flex; align-items: center; gap: 8px;">
            <span style="font-size: 18px;">❌</span>
            <strong>测试失败</strong>
          </div>
        `;

        let details = `<p style="color: var(--error-600); margin-top: 8px;">${escapeHtml(result.error || '未知错误')}</p>`;

        if (result.status_code) {
          details += `<p style="margin-top: 8px;">状态码: ${result.status_code}</p>`;
        }

        if (result.raw_response) {
          const rawId = 'raw-response-' + Date.now();
          details += `
            <div style="margin-top: 12px;">
              <h4 style="margin-bottom: 8px; color: var(--neutral-700);">原始响应</h4>
              <button class="btn btn-secondary btn-sm" onclick="toggleResponse('${rawId}')" style="margin-bottom: 8px;">显示/隐藏</button>
              <div id="${rawId}" style="display: none; padding: 12px; background: var(--neutral-50); border-radius: 4px; border: 1px solid var(--neutral-200); color: var(--error-700); white-space: pre-wrap; font-family: monospace; font-size: 0.85em; max-height: 400px; overflow-y: auto;">${escapeHtml(result.raw_response)}</div>
            </div>
          `;
        }

        detailsDiv.innerHTML = details;
      }
    }

    function toggleResponse(id) {
      const el = document.getElementById(id);
      if (el) {
        el.style.display = el.style.display === 'none' ? 'block' : 'none';
      }
    }

    // ========== 删除 Key（从日志列表入口） ==========
    async function deleteKeyFromLog(channelId, channelName, maskedApiKey) {
      if (!channelId || !maskedApiKey) return;

      const confirmDel = confirm(`确定删除渠道“${channelName || ('#' + channelId)}”中的此Key (${maskedApiKey}) 吗？`);
      if (!confirmDel) return;

      try {
        // 获取渠道详情，匹配掩码对应的 key_index
        const res = await fetchWithAuth(`/admin/channels/${channelId}`);
        if (!res.ok) throw new Error('加载渠道失败: HTTP ' + res.status);
        const respJson = await res.json();
        const channel = respJson.success ? respJson.data : respJson;

        const apiKeys = parseApiKeysFromChannel(channel);
        const keyIndex = findKeyIndexByMaskedKey(apiKeys, maskedApiKey);
        if (keyIndex === null) {
          alert('未能匹配到该Key，请检查渠道配置。');
          return;
        }

        // 删除Key
        const delRes = await fetchWithAuth(`/admin/channels/${channelId}/keys/${keyIndex}`, { method: 'DELETE' });
        if (!delRes.ok) throw new Error('删除失败: HTTP ' + delRes.status);
        const delResult = await delRes.json();

        alert(`已删除 Key #${keyIndex + 1} (${maskedApiKey})`);

        // 如果没有剩余Key，询问是否删除渠道
        if (delResult.remaining_keys === 0) {
          const delChannel = confirm('该渠道已无可用Key，是否删除整个渠道？');
          if (delChannel) {
            const chRes = await fetchWithAuth(`/admin/channels/${channelId}`, { method: 'DELETE' });
            if (!chRes.ok) throw new Error('删除渠道失败: HTTP ' + chRes.status);
            alert('渠道已删除');
          }
        }

        // 刷新日志列表
        load();
      } catch (e) {
        console.error('删除Key失败', e);
        alert(e.message || '删除Key失败');
      }
    }
