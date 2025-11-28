    let currentLogsPage = 1;
    let logsPageSize = 20;
    let totalLogsPages = 1;
    let totalLogs = 0;

    async function load() {
      try {
        showLoading();

        const u = new URLSearchParams(location.search);
        const params = new URLSearchParams({
          range: (u.get('range')||'today'),
          limit: logsPageSize.toString(),
          offset: ((currentLogsPage - 1) * logsPageSize).toString()
        });

        if (u.get('channel_id')) params.set('channel_id', u.get('channel_id'));
        if (u.get('channel_name')) params.set('channel_name', u.get('channel_name'));
        if (u.get('channel_name_like')) params.set('channel_name_like', u.get('channel_name_like'));
        if (u.get('model')) params.set('model', u.get('model'));
        if (u.get('model_like')) params.set('model_like', u.get('model_like'));
        if (u.get('status_code')) params.set('status_code', u.get('status_code'));
        
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
      tbody.innerHTML = `
        <tr>
          <td colspan="${colspan}" class="loading-state">
            <div class="loading-spinner" style="margin: 0 auto var(--space-2)"></div>
            正在加载日志...
          </td>
        </tr>
      `;
    }

    function showError() {
      const tbody = document.getElementById('tbody');
      const colspan = getTableColspan();
      tbody.innerHTML = `
        <tr>
          <td colspan="${colspan}" class="empty-state">
            <svg class="w-12 h-12 mx-auto mb-4" style="color: var(--error-400);" fill="none" stroke="currentColor" viewBox="0 0 24 24">
              <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-2.5L13.732 4c-.77-.833-1.864-.833-2.634 0L4.18 16.5c-.77.833.192 2.5 1.732 2.5z"/>
            </svg>
            <div style="color: var(--error-400); font-weight: var(--font-medium); margin-bottom: var(--space-1);">加载失败</div>
            <div>请检查网络连接或重试</div>
          </td>
        </tr>
      `;
    }

    function renderLogs(data) {
      const tbody = document.getElementById('tbody');
      const colspan = getTableColspan();

      if (data.length === 0) {
        tbody.innerHTML = `
          <tr>
            <td colspan="${colspan}" class="empty-state">
              <svg class="w-12 h-12 mx-auto mb-4" style="color: var(--neutral-400);" fill="none" stroke="currentColor" viewBox="0 0 24 24">
                <path stroke-linecap="round" stroke-linejoin="round" stroke-width="1" d="M9 12h6m-6 4h6m2 5H7a2 2 0 01-2-2V5a2 2 0 012-2h5.586a1 1 0 01.707.293l5.414 5.414a1 1 0 01.293.707V19a2 2 0 01-2 2z"/>
              </svg>
              <div style="font-weight: var(--font-medium); margin-bottom: var(--space-1); color: var(--neutral-700);">暂无日志数据</div>
              <div>请调整筛选条件或检查时间范围</div>
            </td>
          </tr>
        `;
        return;
      }

      tbody.innerHTML = '';
      for (const entry of data) {
        const tr = document.createElement('tr');
        
        const configInfo = entry.channel_name || 
          (entry.channel_id ? `渠道 #${entry.channel_id}` : 
           (entry.message === 'exhausted backends' ? '系统（所有渠道失败）' : 
            entry.message === 'no available upstream (all cooled or none)' ? '系统（无可用渠道）' : '系统'));
        const configDisplay = entry.channel_id ? 
          `<a class="channel-link" href="/web/channels.html#channel-${entry.channel_id}">${escapeHtml(entry.channel_name||'')} <small>(#${entry.channel_id})</small></a>` : 
          `<span style="color: var(--neutral-500);">${escapeHtml(configInfo)}</span>`;
        
        const statusClass = (entry.status_code >= 200 && entry.status_code < 300) ? 
          'status-success' : 'status-error';
          
        const modelDisplay = entry.model ? 
          `<span class="model-tag">${escapeHtml(entry.model)}</span>` : 
          '<span style="color: var(--neutral-500);">-</span>';
        
        // 格式化耗时显示
        const hasDuration = entry.duration !== undefined && entry.duration !== null;
        const durationDisplay = hasDuration ? 
          `<span style="color: var(--neutral-700);">${entry.duration.toFixed(3)}</span>` : 
          '<span style="color: var(--neutral-500);">-</span>';
          
        // 格式化首字节时间显示（仅流式请求）
        const hasFirstByte = entry.is_streaming && entry.first_byte_time !== undefined && entry.first_byte_time !== null;
        const firstByteDisplay = hasFirstByte ?
          `<span style="color: var(--success-600);">${entry.first_byte_time.toFixed(3)}</span>` :
          '<span style="color: var(--neutral-500);">-</span>';
        const streamFlag = entry.is_streaming ?
          '<span class="stream-flag">流</span>' :
          '<span class="stream-flag placeholder">流</span>';
        const responseTimingDisplay = `
          <span style="display: inline-flex; align-items: center; justify-content: flex-end; gap: 4px; white-space: nowrap;">
            ${firstByteDisplay}
            <span style="color: var(--neutral-400);">/</span>
            ${durationDisplay}
          </span>
          ${streamFlag}
        `;

        // 格式化API Key显示（已在后端掩码处理）
        let apiKeyDisplay = '';
        if (entry.api_key_used && entry.channel_id && entry.model) {
          // ✅ 修复：按钮显示条件优化
          // - 测试按钮：仅状态码非200时显示（故障Key才需要测试）
          // - 删除按钮：仅状态码403时显示（鉴权失败说明Key失效）
          const statusCode = entry.status_code || 0;
          const showTestBtn = statusCode !== 200;
          const showDeleteBtn = statusCode === 403;

          // 构建按钮组（按需显示）
          let buttons = '';
          if (showTestBtn) {
            buttons += `
              <button
                class="test-key-btn"
                onclick="testKey(${entry.channel_id}, '${escapeHtml(entry.channel_name || '').replace(/'/g, "\\'")}', '${escapeHtml(entry.api_key_used)}', '${escapeHtml(entry.model)}')"
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
                onclick="deleteKeyFromLog(${entry.channel_id}, '${escapeHtml(entry.channel_name || '').replace(/'/g, "\\'")}', '${escapeHtml(entry.api_key_used)}')"
                title="删除此 API Key">
                🗑
              </button>
            `;
          }

          // 有完整信息，显示API Key和按钮（按需）
          apiKeyDisplay = `
            <div style="display: flex; align-items: center; gap: 6px; justify-content: center;">
              <code style="font-size: 0.9em; color: var(--neutral-600);">${escapeHtml(entry.api_key_used)}</code>
              ${buttons}
            </div>
          `;
        } else if (entry.api_key_used) {
          // 只有API Key，无法测试
          apiKeyDisplay = `<code style="font-size: 0.9em; color: var(--neutral-600);">${escapeHtml(entry.api_key_used)}</code>`;
        } else {
          apiKeyDisplay = '<span style="color: var(--neutral-500);">-</span>';
        }

        // Token统计显示（2025-11新增）
        const tokenValue = (value, color) => {
          if (value === undefined || value === null) {
            return '<span class="token-metric-value token-empty">-</span>';
          }
          return `<span class="token-metric-value" style="color: ${color};">${value.toLocaleString()}</span>`;
        };
        const inputTokensDisplay = tokenValue(entry.input_tokens, 'var(--neutral-700)');
        const outputTokensDisplay = tokenValue(entry.output_tokens, 'var(--neutral-700)');
        const cacheReadDisplay = tokenValue(entry.cache_read_input_tokens, 'var(--success-600)');
        const cacheCreationDisplay = tokenValue(entry.cache_creation_input_tokens, 'var(--primary-600)');

        // 成本显示（2025-11新增）
        const costDisplay = entry.cost !== undefined && entry.cost !== null ?
          `<span style="color: var(--warning-600); font-weight: 500;">${formatCost(entry.cost)}</span>` :
          '<span style="color: var(--neutral-500);">-</span>';

        tr.innerHTML = `
          <td style="white-space: nowrap;">${formatTime(entry.time)}</td>
          <td>${modelDisplay}</td>
          <td class="config-info">${configDisplay}</td>
          <td style="text-align: center; white-space: nowrap;">${apiKeyDisplay}</td>
          <td><span class="${statusClass}">${entry.status_code}</span></td>
          <td style="text-align: right; white-space: nowrap;">${responseTimingDisplay}</td>
          <td style="text-align: right; white-space: nowrap;">${inputTokensDisplay}</td>
          <td style="text-align: right; white-space: nowrap;">${outputTokensDisplay}</td>
          <td style="text-align: right; white-space: nowrap;">${cacheReadDisplay}</td>
          <td style="text-align: right; white-space: nowrap;">${cacheCreationDisplay}</td>
          <td style="text-align: right; white-space: nowrap;">${costDisplay}</td>
          <td style="max-width: 300px; word-break: break-word;">${escapeHtml(entry.message || '')}</td>
        `;
        tbody.appendChild(tr);
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
      const q = new URLSearchParams(location.search);

      if (range) q.set('range', range); else q.delete('range');
      q.delete('hours'); // 清理旧参数
      if (id) q.set('channel_id', id); else q.delete('channel_id');
      if (name) { q.set('channel_name_like', name); q.delete('channel_name'); }
      else { q.delete('channel_name_like'); }
      if (model) { q.set('model_like', model); q.delete('model'); }
      else { q.delete('model_like'); q.delete('model'); }
      if (status) { q.set('status_code', status); }
      else { q.delete('status_code'); }

      location.search = '?' + q.toString();
    }

    function initFilters() {
      const u = new URLSearchParams(location.search);
      const id = u.get('channel_id') || '';
      const name = u.get('channel_name_like') || u.get('channel_name') || '';
      const range = u.get('range') || 'today';
      const model = u.get('model_like') || u.get('model') || '';
      const status = u.get('status_code') || '';

      // 初始化时间范围选择器 (默认"本日")
      if (window.initDateRangeSelector) {
        initDateRangeSelector('f_hours', 'today', null);
        // 设置URL中的值
        document.getElementById('f_hours').value = range;
      }

      document.getElementById('f_id').value = id;
      document.getElementById('f_name').value = name;
      document.getElementById('f_model').value = model;
      const statusEl = document.getElementById('f_status');
      if (statusEl) statusEl.value = status;

      // 事件监听
      document.getElementById('btn_filter').addEventListener('click', applyFilter);

      // 回车键筛选
      ['f_hours', 'f_id', 'f_name', 'f_model', 'f_status'].forEach(id => {
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

    // 格式化成本（美元）
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

    // 顶栏布局下，无需侧栏响应逻辑
    function handleResize() {}

    // 页面初始化
    document.addEventListener('DOMContentLoaded', function() {
      if (window.initTopbar) initTopbar('logs');
      initFilters();
      load();

      // 响应式处理
      handleResize();
      window.addEventListener('resize', handleResize);

      // ESC键关闭测试模态框
      document.addEventListener('keydown', (e) => {
        if (e.key === 'Escape') {
          closeTestKeyModal();
        }
      });
    });

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
      document.getElementById('testKeyContent').value = 'test';
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
      const testContent = contentInput.value.trim() || 'test';
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
            <strong>${result.message || 'API测试成功'}</strong>
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
