    const t = window.t;

    let currentLogsPage = 1;
    let logsPageSize = 15;
    let totalLogsPages = 1;
    let totalLogs = 0;
    let currentChannelType = 'all'; // 当前选中的渠道类型
    let authTokens = []; // 令牌列表
    let defaultTestContent = 'sonnet 4.0的发布日期是什么'; // 默认测试内容（从设置加载）

    const ACTIVE_REQUESTS_POLL_INTERVAL_MS = 2000;
    let activeRequestsPollTimer = null;
    let activeRequestsFetchInFlight = false;
    let lastActiveRequestIDs = null; // 上次活跃请求ID集合（后端原始数据，用于检测完成）
    let logsLoadInFlight = false;
    let logsLoadPending = false;
    let logsLoadScheduled = false;

    function scheduleLoad() {
      if (logsLoadScheduled) return;
      logsLoadScheduled = true;
      setTimeout(() => {
        logsLoadScheduled = false;
        load(true); // 自动刷新时跳过 loading 状态，避免闪烁
      }, 0);
    }

    function toUnixMs(value) {
      if (value === undefined || value === null) return null;

      if (typeof value === 'number' && Number.isFinite(value)) {
        // 兼容：秒(10位) / 毫秒(13位)
        if (value > 1e12) return value;
        if (value > 1e9) return value * 1000;
        return value;
      }

      if (typeof value === 'string') {
        if (/^\d+$/.test(value)) {
          const n = parseInt(value, 10);
          if (!Number.isFinite(n)) return null;
          return n > 1e12 ? n : n * 1000;
        }
        const parsed = Date.parse(value);
        return Number.isNaN(parsed) ? null : parsed;
      }

      return null;
    }

    // 格式化字节数为可读形式（K/M/G）- 使用对数优化
    function formatBytes(bytes) {
      if (bytes == null || bytes <= 0) return '';
      const UNITS = ['B', 'K', 'M', 'G'];
      const FACTOR = 1024;
      const i = Math.min(Math.floor(Math.log(bytes) / Math.log(FACTOR)), UNITS.length - 1);
      const value = bytes / Math.pow(FACTOR, i);
      return value.toFixed(i > 0 ? 1 : 0) + ' ' + UNITS[i];
    }

    // IP 地址掩码处理（隐藏最后两段）
    function maskIP(ip) {
      if (!ip) return '';
      // IPv4: 192.168.1.100 -> 192.168.*.*
      if (ip.includes('.')) {
        const parts = ip.split('.');
        if (parts.length === 4) {
          return `${parts[0]}.${parts[1]}.*.*`;
        }
      }
      // IPv6: 简化处理，保留前两段
      if (ip.includes(':')) {
        const parts = ip.split(':');
        if (parts.length >= 2) {
          return `${parts[0]}:${parts[1]}::*`;
        }
      }
      return ip;
    }

    function clearActiveRequestsRows() {
      document.querySelectorAll('tr.pending-row').forEach(el => el.remove());
    }

    function ensureActiveRequestsPollingStarted() {
      if (activeRequestsPollTimer) return;
      activeRequestsPollTimer = setInterval(async () => {
        if (currentLogsPage !== 1) return;
        await fetchActiveRequests();
      }, ACTIVE_REQUESTS_POLL_INTERVAL_MS);
    }
    // 生成流式标志HTML（公共函数，避免重复）
    function getStreamFlagHtml(isStreaming) {
      return isStreaming
        ? '<span class="stream-flag">流</span>'
        : '<span class="stream-flag placeholder">流</span>';
    }

    // 加载默认测试内容（从系统设置）
    async function loadDefaultTestContent() {
      try {
        const setting = await fetchDataWithAuth('/admin/settings/channel_test_content');
        if (setting && setting.value) {
          defaultTestContent = setting.value;
        }
      } catch (e) {
        console.warn('加载默认测试内容失败，使用内置默认值', e);
      }
    }

    async function load(skipLoading = false) {
      if (logsLoadInFlight) {
        logsLoadPending = true;
        return;
      }
      logsLoadInFlight = true;
      try {
        if (!skipLoading) {
          renderLogsLoading();
        }

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
        
        const response = await fetchAPIWithAuth('/admin/logs?' + params.toString());
        if (!response.success) throw new Error(response.error || '无法加载请求日志');

        const data = response.data || [];

        // 精确计算总页数（基于后端返回的count字段）
        if (typeof response.count === 'number') {
          totalLogs = response.count;
          totalLogsPages = Math.ceil(totalLogs / logsPageSize) || 1;
        } else if (Array.isArray(data)) {
          // 降级方案：后端未返回count时使用旧逻辑
          if (data.length === logsPageSize) {
            totalLogsPages = Math.max(currentLogsPage + 1, totalLogsPages);
          } else if (data.length < logsPageSize && currentLogsPage === 1) {
            totalLogsPages = 1;
          } else if (data.length < logsPageSize) {
            totalLogsPages = currentLogsPage;
          }
        }

	        updatePagination();

	        // 自动刷新时，保存现有 pending 行以避免闪烁
	        const pendingRows = skipLoading ? Array.from(document.querySelectorAll('tr.pending-row')) : [];

	        renderLogs(data);

	        // 立即恢复 pending 行（后续 fetchActiveRequests 会再更新）
	        if (skipLoading && pendingRows.length > 0) {
	          const tbody = document.getElementById('tbody');
	          const firstRow = tbody.firstChild;
	          const fragment = document.createDocumentFragment();
	          pendingRows.forEach(row => fragment.appendChild(row));
	          tbody.insertBefore(fragment, firstRow);
	        }

	        updateStats(data);

	        // 第一页时获取并显示进行中的请求（并开启轮询，做到真正“实时”）
		        if (currentLogsPage === 1) {
		          ensureActiveRequestsPollingStarted();
		          await fetchActiveRequests();
		        } else {
		          lastActiveRequestIDs = null;
		          clearActiveRequestsRows();
		        }

		      } catch (error) {
		        console.error('加载日志失败:', error);
		        try { if (window.showError) window.showError('无法加载请求日志'); } catch(_){}
	        renderLogsError();
	      } finally {
          logsLoadInFlight = false;
          if (logsLoadPending) {
            logsLoadPending = false;
            scheduleLoad();
          }
        }
	    }

    // 根据当前筛选条件过滤活跃请求
    function filterActiveRequests(requests) {
      const channelId = (document.getElementById('f_id')?.value || '').trim();
      const channelName = (document.getElementById('f_name')?.value || '').trim().toLowerCase();
      const model = (document.getElementById('f_model')?.value || '').trim().toLowerCase();
      const channelType = (document.getElementById('f_channel_type')?.value || '').trim();
      const tokenId = (document.getElementById('f_auth_token')?.value || '').trim();

      return requests.filter(req => {
        // 渠道ID精确匹配
        if (channelId) {
          if (req.channel_id === undefined || req.channel_id === null) return false;
          if (String(req.channel_id) !== channelId) return false;
        }
        // 渠道名称模糊匹配（包含）
        if (channelName) {
          const name = (typeof req.channel_name === 'string' ? req.channel_name : '').toLowerCase();
          if (!name.includes(channelName)) return false;
        }
        // 模型名称模糊匹配（包含）
        if (model) {
          const reqModel = (typeof req.model === 'string' ? req.model : '').toLowerCase();
          if (!reqModel.includes(model)) return false;
        }
        // 渠道类型精确匹配（'all' 表示全部，不过滤）
        if (channelType && channelType !== 'all') {
          const reqType = (typeof req.channel_type === 'string' ? req.channel_type : '').toLowerCase();
          if (reqType !== channelType.toLowerCase()) return false;
        }
        // 令牌ID精确匹配
        if (tokenId) {
          if (req.token_id === undefined || req.token_id === null || req.token_id === 0) return false;
          if (String(req.token_id) !== tokenId) return false;
        }
        return true;
      });
    }

    // 获取进行中的请求
    async function fetchActiveRequests() {
      if (activeRequestsFetchInFlight) return;

      // 优化：当筛选条件不可能匹配进行中请求时，跳过请求
      const hours = (document.getElementById('f_hours')?.value || '').trim();
      const status = (document.getElementById('f_status')?.value || '').trim();
      // 进行中的请求只存在于"本日"，且没有状态码
      if ((hours && hours !== 'today') || status) {
        clearActiveRequestsRows();
        lastActiveRequestIDs = null;
        return;
      }

      activeRequestsFetchInFlight = true;
      try {
        const response = await fetchAPIWithAuth('/admin/active-requests');
        const rawActiveRequests = (response.success && Array.isArray(response.data)) ? response.data : [];

        // 检测请求完成：用后端原始ID集合判断“消失的ID”，避免筛选条件变化导致误判
        const currentIDs = new Set();
        for (const req of rawActiveRequests) {
          if (req && (req.id !== undefined && req.id !== null)) {
            currentIDs.add(String(req.id));
          }
        }
        if (lastActiveRequestIDs !== null) {
          let hasCompleted = false;
          for (const id of lastActiveRequestIDs) {
            if (!currentIDs.has(id)) {
              hasCompleted = true;
              break;
            }
          }
          if (hasCompleted && currentLogsPage === 1) {
            scheduleLoad();
          }
        }
        lastActiveRequestIDs = currentIDs;

        // 根据当前筛选条件过滤（只影响展示，不影响完成检测）
        const activeRequests = filterActiveRequests(rawActiveRequests);

        renderActiveRequests(activeRequests);
      } catch (e) {
        // 静默失败，不影响主日志显示
      } finally {
        activeRequestsFetchInFlight = false;
      }
    }

    // 渲染进行中的请求（插入到表格顶部）
    function renderActiveRequests(activeRequests) {
      // 移除旧的进行中行
      clearActiveRequestsRows();

      if (!activeRequests || activeRequests.length === 0) return;

      const tbody = document.getElementById('tbody');
      const firstRow = tbody.firstChild;
      const totalCols = getTableColspan();

      // 使用 DocumentFragment 批量构建，减少 DOM 操作
      const fragment = document.createDocumentFragment();

      for (const req of activeRequests) {
        const startMs = toUnixMs(req.start_time);
        const elapsedRaw = startMs ? Math.max(0, (Date.now() - startMs) / 1000) : null;
        const elapsed = elapsedRaw !== null ? elapsedRaw.toFixed(1) : '-';
        const streamFlag = getStreamFlagHtml(req.is_streaming);

        // 耗时显示：流式请求有首字时间则显示 "首字/总耗时" 格式
        let durationDisplay = startMs ? `${elapsed}s...` : '-';
        if (req.is_streaming && req.client_first_byte_time > 0 && startMs) {
          durationDisplay = `${req.client_first_byte_time.toFixed(2)}s/${elapsed}s...`;
        }

        // 渠道显示
        let channelDisplay = '<span style="color: var(--neutral-500);">选择中...</span>';
        if (req.channel_id && req.channel_name) {
          channelDisplay = `<a class="channel-link" href="/web/channels.html?id=${req.channel_id}#channel-${req.channel_id}">${escapeHtml(req.channel_name)} <small>(#${req.channel_id})</small></a>`;
        }

        // Key显示
        let keyDisplay = '<span style="color: var(--neutral-500);">-</span>';
        if (req.api_key_used) {
          keyDisplay = `<span style="font-family: monospace; font-size: 0.85em;">${escapeHtml(req.api_key_used)}</span>`;
        }

        const bytesInfo = formatBytes(req.bytes_received);
        const hasBytes = !!bytesInfo;
        const infoDisplay = hasBytes ? `已接收 ${bytesInfo}` : '请求处理中...';
        const infoColor = hasBytes ? 'var(--success-600)' : 'var(--neutral-500)';

        const row = document.createElement('tr');
        row.className = 'pending-row';
        if (totalCols < 8) {
          row.innerHTML = `
            <td colspan="${totalCols}">
              <span class="status-pending">进行中</span>
              <span style="margin-left: 8px;">${formatTime(req.start_time)}</span>
              <span style="margin-left: 8px; color: var(--neutral-600);">${escapeHtml(maskIP(req.client_ip) || '-')}</span>
              <span style="margin-left: 8px;">${escapeHtml(req.model || '-')}</span>
              <span style="margin-left: 8px;">${durationDisplay} ${streamFlag}</span>
              <span style="margin-left: 8px; color: ${infoColor};">${escapeHtml(infoDisplay)}</span>
            </td>
          `;
        } else {
          const emptyCols = Math.max(0, totalCols - 8); // 7列固定信息 + 末尾消息列
          const emptyCells = '<td></td>'.repeat(emptyCols);
          row.innerHTML = `
            <td style="white-space: nowrap;">${formatTime(req.start_time)}</td>
            <td class="config-info" style="white-space: nowrap; font-family: monospace; font-size: 0.85em; color: var(--neutral-600);">${escapeHtml(maskIP(req.client_ip) || '-')}</td>
            <td style="text-align: center;">${keyDisplay}</td>
            <td class="config-info">${channelDisplay}</td>
            <td><span class="model-tag">${escapeHtml(req.model)}</span></td>
            <td><span class="status-pending">进行中</span></td>
            <td style="text-align: right;">${durationDisplay} ${streamFlag}</td>
            ${emptyCells}
            <td><span style="color: ${infoColor};">${escapeHtml(infoDisplay)}</span></td>
          `;
        }
        fragment.appendChild(row);
      }

      // 一次性插入所有 pending 行
      tbody.insertBefore(fragment, firstRow);
    }

    // ✅ 动态计算列数（避免硬编码维护成本）
    function getTableColspan() {
      const headerCells = document.querySelectorAll('thead th');
      return headerCells.length || 13; // fallback到13列（向后兼容）
    }

    function renderLogsLoading() {
      const tbody = document.getElementById('tbody');
      const colspan = getTableColspan();
      const loadingRow = TemplateEngine.render('tpl-log-loading', { colspan });
      tbody.innerHTML = '';
      if (loadingRow) tbody.appendChild(loadingRow);
    }

    function renderLogsError() {
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

      // 性能优化：直接拼接 HTML 字符串，避免逐行调用 TemplateEngine.render
      const htmlParts = new Array(data.length);

      for (let i = 0; i < data.length; i++) {
        const entry = data[i];
        // === 预处理数据：构建复杂HTML片段 ===

        // 0. 客户端IP显示（掩码处理）
        const clientIPDisplay = entry.client_ip ?
          escapeHtml(maskIP(entry.client_ip)) :
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

        // 3. 模型显示（支持重定向角标）
        let modelDisplay;
        if (entry.model) {
          if (entry.actual_model && entry.actual_model !== entry.model) {
            // 有重定向：显示角标 + tooltip
            modelDisplay = `<span class="model-tag model-redirected" title="请求模型: ${escapeHtml(entry.model)}&#10;实际模型: ${escapeHtml(entry.actual_model)}">
              <span class="model-text">${escapeHtml(entry.model)}</span>
              <sup class="redirect-badge">↪</sup>
            </span>`;
          } else {
            modelDisplay = `<span class="model-tag">${escapeHtml(entry.model)}</span>`;
          }
        } else {
          modelDisplay = '<span style="color: var(--neutral-500);">-</span>';
        }

        // 4. 响应时间显示(流式/非流式)
        const hasDuration = entry.duration !== undefined && entry.duration !== null;
        const durationDisplay = hasDuration ?
          `<span style="color: var(--neutral-700);">${entry.duration.toFixed(3)}</span>` :
          '<span style="color: var(--neutral-500);">-</span>';

        const streamFlag = getStreamFlagHtml(entry.is_streaming);

        let responseTimingDisplay;
        if (entry.is_streaming) {
          const hasFirstByte = entry.first_byte_time !== undefined && entry.first_byte_time !== null;
          const firstByteDisplay = hasFirstByte ?
            `<span style="color: var(--success-600);">${entry.first_byte_time.toFixed(3)}</span>` :
            '<span style="color: var(--neutral-500);">-</span>';
          responseTimingDisplay = `<span style="display: inline-flex; align-items: center; justify-content: flex-end; gap: 4px; white-space: nowrap;">${firstByteDisplay}<span style="color: var(--neutral-400);">/</span>${durationDisplay}</span>${streamFlag}`;
        } else {
          responseTimingDisplay = `<span style="display: inline-flex; align-items: center; justify-content: flex-end; gap: 4px; white-space: nowrap;">${durationDisplay}</span>${streamFlag}`;
        }

        // 5. API Key显示(含按钮组)
        let apiKeyDisplay = '';
        if (entry.api_key_used && entry.channel_id && entry.model) {
          const sc = entry.status_code || 0;
          const showTestBtn = sc !== 200;
          const showDeleteBtn = sc === 401 || sc === 403;

          const testBtnIcon = `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" xmlns="http://www.w3.org/2000/svg" aria-hidden="true" focusable="false"><path d="M13 2L4 14H11L9 22L20 10H13L13 2Z" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/></svg>`;
          const deleteBtnIcon = `<svg width="14" height="14" viewBox="0 0 24 24" fill="none" xmlns="http://www.w3.org/2000/svg" aria-hidden="true" focusable="false"><path d="M3 6H21" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/><path d="M8 6V4H16V6" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/><path d="M19 6L18 20H6L5 6" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/><path d="M10 11V17" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/><path d="M14 11V17" stroke="currentColor" stroke-width="1.8" stroke-linecap="round"/></svg>`;
          let buttons = '';
          if (showTestBtn) {
            buttons += `<button class="test-key-btn" data-action="test" data-channel-id="${entry.channel_id}" data-channel-name="${escapeHtml(entry.channel_name || '').replace(/"/g, '&quot;')}" data-api-key="${escapeHtml(entry.api_key_used).replace(/"/g, '&quot;')}" data-model="${escapeHtml(entry.model).replace(/"/g, '&quot;')}" title="测试此 API Key">${testBtnIcon}</button>`;
          }
          if (showDeleteBtn) {
            buttons += `<button class="test-key-btn" style="color: var(--error-600);" data-action="delete" data-channel-id="${entry.channel_id}" data-channel-name="${escapeHtml(entry.channel_name || '').replace(/"/g, '&quot;')}" data-api-key="${escapeHtml(entry.api_key_used).replace(/"/g, '&quot;')}" title="删除此 API Key">${deleteBtnIcon}</button>`;
          }

          apiKeyDisplay = `<div style="display: flex; align-items: center; gap: 4px; justify-content: center;"><code style="font-size: 0.9em; color: var(--neutral-600);">${escapeHtml(entry.api_key_used)}</code><span style="display: inline-flex; align-items: center; gap: 1px;">${buttons}</span></div>`;
        } else if (entry.api_key_used) {
          apiKeyDisplay = `<code style="font-size: 0.9em; color: var(--neutral-600);">${escapeHtml(entry.api_key_used)}</code>`;
        } else {
          apiKeyDisplay = '<span style="color: var(--neutral-500);">-</span>';
        }

        // 6. Token统计显示(0值为空)
        const tokenValue = (value, color) => {
          if (value === undefined || value === null || value === 0) return '';
          return `<span class="token-metric-value" style="color: ${color};">${value.toLocaleString()}</span>`;
        };
        const inputTokensDisplay = tokenValue(entry.input_tokens, 'var(--neutral-700)');
        const outputTokensDisplay = tokenValue(entry.output_tokens, 'var(--neutral-700)');
        const cacheReadDisplay = tokenValue(entry.cache_read_input_tokens, 'var(--success-600)');

        // 缓存建列
        let cacheCreationDisplay = '';
        const total = entry.cache_creation_input_tokens || 0;
        const cache5m = entry.cache_5m_input_tokens || 0;
        const cache1h = entry.cache_1h_input_tokens || 0;

        if (total > 0) {
          const model = (entry.model || '').toLowerCase();
          const isClaudeOrCodex = model.includes('claude') || model.includes('codex');

          let badge = '';
          if (isClaudeOrCodex && (cache5m > 0 || cache1h > 0)) {
            if (cache5m > 0 && cache1h === 0) {
              badge = ' <sup style="color: var(--primary-500); font-size: 0.75em; font-weight: 600;">5m</sup>';
            } else if (cache1h > 0 && cache5m === 0) {
              badge = ' <sup style="color: var(--warning-600); font-size: 0.75em; font-weight: 600;">1h</sup>';
            } else if (cache5m > 0 && cache1h > 0) {
              badge = ' <sup style="color: var(--primary-500); font-size: 0.75em; font-weight: 600;">5m</sup><sup style="color: var(--warning-600); font-size: 0.75em; font-weight: 600;">+1h</sup>';
            }
          }
          cacheCreationDisplay = `<span class="token-metric-value" style="color: var(--primary-600);">${total.toLocaleString()}${badge}</span>`;
        }

        // 7. 成本显示
        const costDisplay = entry.cost ?
          `<span style="color: var(--warning-600); font-weight: 500;">${formatCost(entry.cost)}</span>` : '';

        // === 直接拼接行 HTML ===
        htmlParts[i] = `<tr>
          <td style="white-space: nowrap;">${formatTime(entry.time)}</td>
          <td class="config-info" style="white-space: nowrap; font-family: monospace; font-size: 0.85em; color: var(--neutral-600);">${clientIPDisplay}</td>
          <td style="text-align: center; white-space: nowrap;">${apiKeyDisplay}</td>
          <td class="config-info">${configDisplay}</td>
          <td>${modelDisplay}</td>
          <td><span class="${statusClass}">${statusCode}</span></td>
          <td style="text-align: right; white-space: nowrap;">${responseTimingDisplay}</td>
          <td style="text-align: right; white-space: nowrap;">${inputTokensDisplay}</td>
          <td style="text-align: right; white-space: nowrap;">${outputTokensDisplay}</td>
          <td style="text-align: right; white-space: nowrap;">${cacheReadDisplay}</td>
          <td style="text-align: right; white-space: nowrap;">${cacheCreationDisplay}</td>
          <td style="text-align: right; white-space: nowrap;">${costDisplay}</td>
          <td style="max-width: 300px; word-break: break-word;">${escapeHtml(entry.message || '')}</td>
        </tr>`;
      }

      // 一次性替换 tbody 内容
      tbody.innerHTML = htmlParts.join('');
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

      // 使用 pushState 更新 URL，避免页面重新加载
      history.pushState(null, '', '?' + q.toString());
      load();
    }

    async function initFilters() {
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

      // 加载令牌列表（返回 Promise 以便等待完成）
      await loadAuthTokens();
      document.getElementById('f_auth_token').value = authToken;

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

    // 性能优化：避免 toLocaleString 的开销，使用手动格式化
    function formatTime(timeStr) {
      try {
        const ts = toUnixMs(timeStr);
        if (!ts) return '-';

        const d = new Date(ts);
        if (isNaN(d.getTime()) || d.getFullYear() < 2020) {
          return '-';
        }

        // 手动格式化：YYYY/MM/DD HH:mm:ss
        const Y = d.getFullYear();
        const M = String(d.getMonth() + 1).padStart(2, '0');
        const D = String(d.getDate()).padStart(2, '0');
        const h = String(d.getHours()).padStart(2, '0');
        const m = String(d.getMinutes()).padStart(2, '0');
        const s = String(d.getSeconds()).padStart(2, '0');
        return `${Y}/${M}/${D} ${h}:${m}:${s}`;
      } catch (e) {
        return '-';
      }
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
          const allTokensText = t('stats.allTokens');
          tokenSelect.innerHTML = `<option value="" data-i18n="stats.allTokens">${allTokensText}</option>`;
          authTokens.forEach(token => {
            const option = document.createElement('option');
            option.value = token.id;
            option.textContent = token.description || `Token #${token.id}`;
            tokenSelect.appendChild(option);
          });
        }
      } catch (error) {
        console.error('Failed to load auth tokens:', error);
      }
    }

    function maskKeyForCompare(key) {
      if (!key) return '';
      if (key.length <= 8) return key;
      return `${key.slice(0, 4)}...${key.slice(-4)}`;
    }

    function findKeyIndexByMaskedKey(apiKeys, maskedKey) {
      if (!maskedKey || !apiKeys || !apiKeys.length) return null;
      const target = maskedKey.trim();
      for (const k of apiKeys) {
        const rawKey = (k && (k.api_key || k.key)) || '';
        if (maskKeyForCompare(rawKey) !== target) continue;
        if (k && typeof k.key_index === 'number') return k.key_index;
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

      await initFilters();
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

    // 处理 bfcache（后退/前进缓存）：页面从缓存恢复时重新加载筛选条件
    window.addEventListener('pageshow', async function(event) {
      if (event.persisted) {
        // 页面从 bfcache 恢复，重新同步筛选器状态
        const savedFilters = loadLogsFilters();
        if (savedFilters) {
          // 重新加载令牌列表并设置值
          await loadAuthTokens();
          if (savedFilters.authToken) {
            document.getElementById('f_auth_token').value = savedFilters.authToken;
          }
          // 同步其他筛选器
          if (savedFilters.channelType) {
            document.getElementById('f_channel_type').value = savedFilters.channelType;
            currentChannelType = savedFilters.channelType;
          }
          if (savedFilters.range) {
            document.getElementById('f_hours').value = savedFilters.range;
          }
          if (savedFilters.channelId) {
            document.getElementById('f_id').value = savedFilters.channelId;
          }
          if (savedFilters.channelName) {
            document.getElementById('f_name').value = savedFilters.channelName;
          }
          if (savedFilters.model) {
            document.getElementById('f_model').value = savedFilters.model;
          }
          if (savedFilters.status) {
            document.getElementById('f_status').value = savedFilters.status;
          }
          // 重新加载数据
          currentLogsPage = 1;
          load();
        }
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
      allOption.textContent = t('common.all');
      allOption.setAttribute('data-i18n', 'common.all');
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

      // 异步加载渠道配置以获取支持的模型列表 + Keys 用于 key_index 匹配
      try {
        const [channel, apiKeysRaw] = await Promise.all([
          fetchDataWithAuth(`/admin/channels/${channelId}`),
          fetchDataWithAuth(`/admin/channels/${channelId}/keys`)
        ]);
        const apiKeys = apiKeysRaw || [];

        // ✅ 保存渠道类型,用于后续测试请求
        testingKeyData.channelType = channel.channel_type || 'anthropic';
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
          // channel.models 是 ModelEntry 对象数组，需访问 .model 属性
          channel.models.forEach(m => {
            const modelName = m.model || m; // 兼容字符串和对象
            const option = document.createElement('option');
            option.value = modelName;
            option.textContent = modelName;
            modelSelect.appendChild(option);
          });

          // 如果日志中的模型在支持列表中，则预选；否则选择第一个
          const modelNames = channel.models.map(m => m.model || m);
          if (modelNames.includes(model)) {
            modelSelect.value = model;
          } else {
            modelSelect.value = modelNames[0];
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
        if (window.showError) window.showError('请选择一个测试模型');
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
          stream: streamEnabled,
          content: testContent,
          channel_type: testingKeyData.channelType || 'anthropic' // ✅ 添加渠道类型
        };
        if (testingKeyData && testingKeyData.keyIndex !== null && testingKeyData.keyIndex !== undefined) {
          testRequest.key_index = testingKeyData.keyIndex;
        }

        const testResult = await fetchDataWithAuth(`/admin/channels/${testingKeyData.channelId}/test`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(testRequest)
        });

        displayKeyTestResult(testResult || { success: false, error: '空响应' });
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

    // ========== 删除 Key（从日志列表入口） ==========
    async function deleteKeyFromLog(channelId, channelName, maskedApiKey) {
      if (!channelId || !maskedApiKey) return;

      const confirmDel = confirm(`确定删除渠道“${channelName || ('#' + channelId)}”中的此Key (${maskedApiKey}) 吗？`);
      if (!confirmDel) return;

      try {
        // 通过 Keys 列表匹配掩码对应的 key_index（渠道详情不再返回明文Key）
        const apiKeys = await fetchDataWithAuth(`/admin/channels/${channelId}/keys`);
        const keyIndex = findKeyIndexByMaskedKey(apiKeys, maskedApiKey);
        if (keyIndex === null) {
          alert('未能匹配到该Key，请检查渠道配置。');
          return;
        }

        // 删除Key
        const delResult = await fetchDataWithAuth(`/admin/channels/${channelId}/keys/${keyIndex}`, { method: 'DELETE' });

        alert(`已删除 Key #${keyIndex + 1} (${maskedApiKey})`);

        // 如果没有剩余Key，询问是否删除渠道
        if (delResult && delResult.remaining_keys === 0) {
          const delChannel = confirm('该渠道已无可用Key，是否删除整个渠道？');
          if (delChannel) {
            const chResp = await fetchAPIWithAuth(`/admin/channels/${channelId}`, { method: 'DELETE' });
            if (!chResp.success) throw new Error(chResp.error || '删除渠道失败');
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
