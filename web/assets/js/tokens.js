    const t = window.t;
    const API_BASE = '/admin';
    let allTokens = [];
    let isToday = true;      // 是否为本日（本日才显示最近一分钟）

    // 当前选中的时间范围(默认为本日)
    let currentTimeRange = 'today';

    // 模型限制相关状态（2026-01新增）
    let editAllowedModels = [];              // 编辑模态框中当前的模型限制列表
    let selectedAllowedModelIndices = new Set(); // 已选中的模型索引（批量删除用）
    let allChannels = [];                    // 渠道数据缓存
    let availableModelsCache = [];           // 可用模型缓存
    let selectedModelsForAdd = new Set();    // 模型选择对话框中已选的模型
    let currentVisibleModels = [];            // 当前可见的模型列表（用于全选功能）

    // 对话框栈，用于 ESC 键层级关闭
    const modalStack = [];

    /** 注册全局 ESC 键处理 */
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && modalStack.length > 0) {
        const topModal = modalStack[modalStack.length - 1];
        topModal.close();
      }
    });

    /** 压入对话框栈 */
    function pushModal(closeFunc) {
      modalStack.push({ close: closeFunc });
    }

    /** 弹出对话框栈 */
    function popModal() {
      modalStack.pop();
    }

    document.addEventListener('DOMContentLoaded', () => {
      // 初始化时间范围选择器
      initTimeRangeSelector();

      // 加载令牌列表(默认显示本日统计)
      loadTokens();

      // 预加载渠道数据（用于模型选择）
      loadChannelsData();

      // 初始化事件委托
      initEventDelegation();

      document.getElementById('tokenExpiry').addEventListener('change', (e) => {
        document.getElementById('customExpiryContainer').style.display =
          e.target.value === 'custom' ? 'block' : 'none';
      });
      document.getElementById('editTokenExpiry').addEventListener('change', (e) => {
        document.getElementById('editCustomExpiryContainer').style.display =
          e.target.value === 'custom' ? 'block' : 'none';
      });

      // 监听语言切换事件，重新渲染令牌列表
      window.i18n.onLocaleChange(() => {
        renderTokens();
      });
    });

    // 时间范围选择器事件处理
    function initTimeRangeSelector() {
      const buttons = document.querySelectorAll('.time-range-btn');
      buttons.forEach(btn => {
        btn.addEventListener('click', function() {
          // 更新按钮激活状态
          buttons.forEach(b => b.classList.remove('active'));
          this.classList.add('active');

          // 更新当前时间范围并重新加载数据
          currentTimeRange = this.dataset.range;
          loadTokens();
        });
      });
    }

    /**
     * 初始化事件委托(统一处理表格内按钮点击)
     */
    function initEventDelegation() {
      const container = document.getElementById('tokens-container');
      if (!container) return;

      container.addEventListener('click', (e) => {
        const target = e.target;

        // 处理复制令牌按钮
        if (target.classList.contains('btn-copy-token')) {
          const tokenHash = target.dataset.token;
          if (tokenHash) copyTokenToClipboard(tokenHash);
          return;
        }

        // 处理编辑按钮
        if (target.classList.contains('btn-edit')) {
          const row = target.closest('tr');
          const tokenId = row ? parseInt(row.dataset.tokenId) : null;
          if (tokenId) editToken(tokenId);
          return;
        }

        // 处理删除按钮
        if (target.classList.contains('btn-delete')) {
          const row = target.closest('tr');
          const tokenId = row ? parseInt(row.dataset.tokenId) : null;
          if (tokenId) deleteToken(tokenId);
          return;
        }
      });
    }

    async function loadTokens() {
      try {
        // 根据currentTimeRange决定是否添加range参数
        let url = `${API_BASE}/auth-tokens`;
        if (currentTimeRange !== 'all') {
          url += `?range=${currentTimeRange}`;
        }

        const data = await fetchDataWithAuth(url);
        allTokens = (data && data.tokens) || [];
        isToday = !!(data && data.is_today);
        renderTokens();
      } catch (error) {
        
        console.error('Failed to load tokens:', error);
        window.showNotification(t('tokens.msg.loadFailed') + ': ' + error.message, 'error');
      }
    }

    function renderTokens() {
      const container = document.getElementById('tokens-container');
      const emptyState = document.getElementById('empty-state');

      if (allTokens.length === 0) {
        container.innerHTML = '';
        emptyState.style.display = 'block';
        return;
      }

      emptyState.style.display = 'none';

      // 构建表格结构
      const table = document.createElement('table');
      
      table.innerHTML = `
        <thead>
          <tr>
            <th>${t('tokens.table.description')}</th>
            <th>${t('tokens.table.token')}</th>
            <th style="text-align: center;">${t('tokens.table.callCount')}</th>
            <th style="text-align: center;">${t('tokens.table.successRate')}</th>
            <th style="text-align: center;" title="${t('tokens.table.rpmTitle')}">${t('tokens.table.rpm')}</th>
            <th style="text-align: center;">${t('tokens.table.tokenUsage')}</th>
            <th style="text-align: center;">${t('tokens.table.totalCost')}</th>
            <th style="text-align: center;">${t('tokens.table.streamAvg')}</th>
            <th style="text-align: center;">${t('tokens.table.nonStreamAvg')}</th>
            <th>${t('tokens.table.lastUsed')}</th>
            <th style="width: 260px;">${t('tokens.table.actions')}</th>
          </tr>
        </thead>
      `;

      const tbody = document.createElement('tbody');

      // 使用模板引擎渲染行，降级处理
      if (typeof TemplateEngine !== 'undefined') {
        allTokens.forEach(token => {
          const row = createTokenRowWithTemplate(token);
          if (row) tbody.appendChild(row);
        });
      } else {
        // 降级：模板引擎不可用时使用原有方式
        console.warn('[Tokens] TemplateEngine not available, using fallback rendering');
        tbody.innerHTML = allTokens.map(token => createTokenRowFallback(token)).join('');
      }

      table.appendChild(tbody);
      container.innerHTML = '';
      container.appendChild(table);

      // 翻译动态渲染的内容中的 data-i18n 属性
      if (window.i18n.translatePage) {
        window.i18n.translatePage();
      }
    }

    // 格式化 Token 数量为 M 单位
    function formatTokenCount(count) {
      if (!count || count === 0) return '0M';
      const millions = count / 1000000;
      return millions.toFixed(2) + 'M';
    }

    /**
     * 使用模板引擎渲染令牌行
     */
    function createTokenRowWithTemplate(token) {
      
      const locale = window.i18n?.getLocale?.() || 'en';
      const status = getTokenStatus(token);
      const createdAt = new Date(token.created_at).toLocaleString(locale);
      const lastUsed = token.last_used_at ? new Date(token.last_used_at).toLocaleString(locale) : t('tokens.neverUsed');
      const expiresAt = token.expires_at ? new Date(token.expires_at).toLocaleString(locale) : t('tokens.expiryNever');

      // 计算统计信息
      const successCount = token.success_count || 0;
      const failureCount = token.failure_count || 0;
      const totalCount = successCount + failureCount;
      const successRate = totalCount > 0 ? ((successCount / totalCount) * 100).toFixed(1) : 0;

      // 预构建各个HTML片段(保留条件逻辑在JS中)
      const callsHtml = buildCallsHtml(successCount, failureCount, totalCount);
      const successRateHtml = buildSuccessRateHtml(successRate, totalCount);
      const rpmHtml = buildRpmHtml(token);
      const tokensHtml = buildTokensHtml(token);
      const costHtml = buildCostHtml(token.total_cost_usd);
      const streamAvgHtml = buildResponseTimeHtml(token.stream_avg_ttfb, token.stream_count);
      const nonStreamAvgHtml = buildResponseTimeHtml(token.non_stream_avg_rt, token.non_stream_count);

      // 使用模板引擎渲染
      const maskedToken = token.token.length > 8
        ? token.token.substring(0, 4) + '****' + token.token.slice(-4)
        : token.token;

      return TemplateEngine.render('tpl-token-row', {
        id: token.id,
        description: token.description,
        token: token.token,
        maskedToken: maskedToken,
        statusClass: status.class,
        createdAt: createdAt,
        createdLabel: t('tokens.createdSuffix'),
        expiresAt: expiresAt,
        callsHtml: callsHtml,
        rpmHtml: rpmHtml,
        successRateHtml: successRateHtml,
        tokensHtml: tokensHtml,
        costHtml: costHtml,
        streamAvgHtml: streamAvgHtml,
        nonStreamAvgHtml: nonStreamAvgHtml,
        lastUsed: lastUsed
      });
    }

    /**
     * 构建调用次数HTML
     */
    function buildCallsHtml(successCount, failureCount, totalCount) {
      if (totalCount === 0) {
        return '<span style="color: var(--neutral-500); font-size: 13px;">-</span>';
      }

      
      let html = '<div style="display: flex; flex-direction: column; gap: 4px; align-items: center;">';
      html += `<span class="stats-badge" style="background: var(--success-50); color: var(--success-700); font-weight: 600; border: 1px solid var(--success-200);" title="${t('tokens.successCall')}">`;
      html += `<span style="color: var(--success-600); font-size: 14px; font-weight: 700;">✓</span> ${successCount.toLocaleString()}`;
      html += `</span>`;

      if (failureCount > 0) {
        html += `<span class="stats-badge" style="background: var(--error-50); color: var(--error-700); font-weight: 600; border: 1px solid var(--error-200);" title="${t('tokens.failedCall')}">`;
        html += `<span style="color: var(--error-600); font-size: 14px; font-weight: 700;">✗</span> ${failureCount.toLocaleString()}`;
        html += `</span>`;
      }

      html += '</div>';
      return html;
    }

    /**
     * 构建RPM HTML（峰/均/近格式）
     */
    function buildRpmHtml(token) {
      const peakRPM = token.peak_rpm || 0;
      const avgRPM = token.avg_rpm || 0;
      const recentRPM = token.recent_rpm || 0;

      // 如果都是0，返回空
      if (peakRPM < 0.01 && avgRPM < 0.01 && recentRPM < 0.01) {
        return '<span style="color: var(--neutral-500); font-size: 13px;">-</span>';
      }

      // 格式化RPM值
      const formatRpm = (rpm) => {
        if (rpm < 0.01) return '-';
        if (rpm >= 1000) return (rpm / 1000).toFixed(1) + 'K';
        if (rpm >= 1) return rpm.toFixed(1);
        return rpm.toFixed(2);
      };

      const peakText = formatRpm(peakRPM);
      const avgText = formatRpm(avgRPM);
      const recentText = isToday ? formatRpm(recentRPM) : '-';

      // 颜色：峰值决定整体颜色
      const color = getRpmColor(peakRPM);

      return `<span style="color: ${color}; font-weight: 500;">${peakText}/${avgText}/${recentText}</span>`;
    }

    /**
     * RPM 颜色：低流量绿色，中等橙色，高流量红色
     */
    /**
     * 构建成功率HTML
     */
    function buildSuccessRateHtml(successRate, totalCount) {
      if (totalCount === 0) {
        return '<span style="color: var(--neutral-500); font-size: 13px;">-</span>';
      }

      let className = 'stats-badge';
      if (successRate >= 95) className += ' success-rate-high';
      else if (successRate >= 80) className += ' success-rate-medium';
      else className += ' success-rate-low';

      return `<span class="${className}">${successRate}%</span>`;
    }

    /**
     * 构建Token用量HTML
     */
    function buildTokensHtml(token) {
      const hasTokens = token.prompt_tokens_total > 0 ||
                        token.completion_tokens_total > 0 ||
                        token.cache_read_tokens_total > 0 ||
                        token.cache_creation_tokens_total > 0;

      if (!hasTokens) {
        return '<span style="color: var(--neutral-500); font-size: 13px;">-</span>';
      }

      
      let html = '<div style="display: flex; flex-direction: column; align-items: center; gap: 4px;">';

      // 输入/输出
      html += '<div style="display: inline-flex; gap: 4px; font-size: 12px;">';
      html += `<span class="stats-badge" style="background: var(--primary-50); color: var(--primary-700);" title="${t('tokens.inputTokens')}">`;
      html += `${t('tokens.input')} ${formatTokenCount(token.prompt_tokens_total || 0)}`;
      html += `</span>`;
      html += `<span class="stats-badge" style="background: var(--secondary-50); color: var(--secondary-700);" title="${t('tokens.outputTokens')}">`;
      html += `${t('tokens.output')} ${formatTokenCount(token.completion_tokens_total || 0)}`;
      html += `</span>`;
      html += '</div>';

      // 缓存
      if (token.cache_read_tokens_total > 0 || token.cache_creation_tokens_total > 0) {
        html += '<div style="display: inline-flex; gap: 4px; font-size: 12px;">';

        if (token.cache_read_tokens_total > 0) {
          html += `<span class="stats-badge" style="background: var(--success-50); color: var(--success-700);" title="${t('tokens.cacheReadTokens')}">`;
          html += `${t('tokens.cacheRead')} ${formatTokenCount(token.cache_read_tokens_total || 0)}`;
          html += `</span>`;
        }

        if (token.cache_creation_tokens_total > 0) {
          html += `<span class="stats-badge" style="background: var(--warning-50); color: var(--warning-700);" title="${t('tokens.cacheCreateTokens')}">`;
          html += `${t('tokens.cacheCreate')} ${formatTokenCount(token.cache_creation_tokens_total || 0)}`;
          html += `</span>`;
        }

        html += '</div>';
      }

      html += '</div>';
      return html;
    }

    /**
     * 构建总费用HTML
     */
    function buildCostHtml(totalCostUsd) {
      if (!totalCostUsd || totalCostUsd <= 0) {
        return '<span style="color: var(--neutral-500); font-size: 13px;">-</span>';
      }

      return `
        <div style="display: flex; flex-direction: column; align-items: center; gap: 2px;">
          <span class="metric-value" style="color: var(--success-700); font-size: 15px; font-weight: 700;">
            $${totalCostUsd.toFixed(4)}
          </span>
        </div>
      `;
    }

    /**
     * 构建响应时间HTML
     */
    function buildResponseTimeHtml(time, count) {
      if (!count || count === 0) {
        return '<span style="color: var(--neutral-500); font-size: 13px;">-</span>';
      }

      const responseClass = getResponseClass(time);
      return `<span class="metric-value ${responseClass}">${time.toFixed(2)}s</span>`;
    }

    /**
     * 获取响应时间颜色等级
     */
    function getResponseClass(time) {
      const num = Number(time);
      if (!Number.isFinite(num) || num <= 0) return '';
      if (num < 3) return 'response-fast';
      if (num < 6) return 'response-medium';
      return 'response-slow';
    }

    /**
     * 降级：模板引擎不可用时的渲染方式
     */
    function createTokenRowFallback(token) {
      
      const locale = window.i18n?.getLocale?.() || 'en';
      const status = getTokenStatus(token);
      const createdAt = new Date(token.created_at).toLocaleString(locale);
      const lastUsed = token.last_used_at ? new Date(token.last_used_at).toLocaleString(locale) : t('tokens.neverUsed');
      const expiresAt = token.expires_at ? new Date(token.expires_at).toLocaleString(locale) : t('tokens.expiryNever');

      // 计算统计信息
      const successCount = token.success_count || 0;
      const failureCount = token.failure_count || 0;
      const totalCount = successCount + failureCount;

      // 预构建HTML片段
      const callsHtml = buildCallsHtml(successCount, failureCount, totalCount);
      const successRate = totalCount > 0 ? ((successCount / totalCount) * 100).toFixed(1) : 0;
      const successRateHtml = buildSuccessRateHtml(successRate, totalCount);
      const rpmHtml = buildRpmHtml(token);
      const tokensHtml = buildTokensHtml(token);
      const costHtml = buildCostHtml(token.total_cost_usd);
      const streamAvgHtml = buildResponseTimeHtml(token.stream_avg_ttfb, token.stream_count);
      const nonStreamAvgHtml = buildResponseTimeHtml(token.non_stream_avg_rt, token.non_stream_count);

      const maskedToken = token.token.length > 8
        ? token.token.substring(0, 4) + '****' + token.token.slice(-4)
        : token.token;

      return `
        <tr data-token-id="${token.id}">
          <td style="font-weight: 500;">${escapeHtml(token.description)}</td>
          <td>
            <div><span class="token-display token-display-${status.class}">${escapeHtml(maskedToken)}</span></div>
            <div style="font-size: 12px; color: var(--neutral-500); margin-top: 4px;">${createdAt}${t('tokens.createdSuffix')} · ${expiresAt}</div>
          </td>
          <td style="text-align: center;">${callsHtml}</td>
          <td style="text-align: center;">${successRateHtml}</td>
          <td style="text-align: center;">${rpmHtml}</td>
          <td style="text-align: center;">${tokensHtml}</td>
          <td style="text-align: center;">${costHtml}</td>
          <td style="text-align: center;">${streamAvgHtml}</td>
          <td style="text-align: center;">${nonStreamAvgHtml}</td>
          <td style="color: var(--neutral-600);">${lastUsed}</td>
          <td style="white-space: nowrap;">
            <button class="btn-copy-token btn btn-secondary" style="padding: 4px 12px; font-size: 13px; margin-right: 4px;" data-token="${escapeHtml(token.token)}">${t('common.copy')}</button>
            <button class="btn btn-secondary btn-edit" style="padding: 4px 12px; font-size: 13px; margin-right: 4px;">${t('common.edit')}</button>
            <button class="btn btn-danger btn-delete" style="padding: 4px 12px; font-size: 13px;">${t('common.delete')}</button>
          </td>
        </tr>
      `;
    }

    function getTokenStatus(token) {
      
      if (token.is_expired) return { class: 'expired', text: t('tokens.status.expired') };
      if (!token.is_active) return { class: 'inactive', text: t('tokens.status.inactive') };
      return { class: 'active', text: t('tokens.status.active') };
    }

    function showCreateModal() {
      document.getElementById('tokenDescription').value = '';
      document.getElementById('tokenExpiry').value = 'never';
      document.getElementById('tokenCostLimitUSD').value = 0;
      document.getElementById('tokenActive').checked = true;
      document.getElementById('customExpiryContainer').style.display = 'none';
      document.getElementById('createModal').style.display = 'block';
    }

    function closeCreateModal() {
      document.getElementById('createModal').style.display = 'none';
    }

    async function createToken() {
      
      const description = document.getElementById('tokenDescription').value.trim();
      if (!description) {
        window.showNotification(t('tokens.msg.enterDescription'), 'error');
        return;
      }
      const expiryType = document.getElementById('tokenExpiry').value;
      let expiresAt = null;
      if (expiryType !== 'never') {
        if (expiryType === 'custom') {
          const customDate = document.getElementById('customExpiry').value;
          if (!customDate) {
            window.showNotification(t('tokens.msg.selectExpiry'), 'error');
            return;
          }
          expiresAt = new Date(customDate).getTime();
        } else {
          const days = parseInt(expiryType);
          expiresAt = Date.now() + days * 24 * 60 * 60 * 1000;
        }
      }
      const isActive = document.getElementById('tokenActive').checked;
      const costLimitUSD = parseFloat(document.getElementById('tokenCostLimitUSD').value) || 0;
      if (costLimitUSD < 0) {
        window.showNotification(t('tokens.msg.costLimitNegative'), 'error');
        return;
      }
      try {
        const data = await fetchDataWithAuth(`${API_BASE}/auth-tokens`, {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json'
          },
          body: JSON.stringify({ description, expires_at: expiresAt, is_active: isActive, cost_limit_usd: costLimitUSD })
        });

        closeCreateModal();
        document.getElementById('newTokenValue').value = data.token;
        document.getElementById('tokenResultModal').style.display = 'block';
        loadTokens();
        window.showNotification(t('tokens.msg.createSuccess'), 'success');
      } catch (error) {
        console.error('Failed to create token:', error);
        window.showNotification(t('tokens.msg.createFailed') + ': ' + error.message, 'error');
      }
    }

    function copyToken() {
      const textarea = document.getElementById('newTokenValue');
      textarea.select();
      document.execCommand('copy');

      window.showNotification(t('tokens.msg.copySuccess'), 'success');
    }

    function copyTokenToClipboard(hash) {
      navigator.clipboard.writeText(hash).then(() => {
        window.showNotification(t('tokens.msg.copySuccess'), 'success');
      }).catch(() => {
        // fallback
        const textarea = document.createElement('textarea');
        textarea.value = hash;
        textarea.style.position = 'fixed';
        textarea.style.opacity = '0';
        document.body.appendChild(textarea);
        textarea.select();
        document.execCommand('copy');
        document.body.removeChild(textarea);
        window.showNotification(t('tokens.msg.copySuccess'), 'success');
      });
    }

    function closeTokenResultModal() {
      document.getElementById('tokenResultModal').style.display = 'none';
      document.getElementById('newTokenValue').value = '';
    }

    function editToken(id) {
      const token = allTokens.find(t => t.id === id);
      if (!token) return;
      document.getElementById('editTokenId').value = id;
      document.getElementById('editTokenDescription').value = token.description;
      document.getElementById('editTokenActive').checked = token.is_active;
      if (!token.expires_at) {
        document.getElementById('editTokenExpiry').value = 'never';
      } else {
        document.getElementById('editTokenExpiry').value = 'custom';
        document.getElementById('editCustomExpiryContainer').style.display = 'block';
        const date = new Date(token.expires_at);
        document.getElementById('editCustomExpiry').value = date.toISOString().slice(0, 16);
      }

      // 初始化费用限额状态（2026-01新增）
      const costLimitInput = document.getElementById('editCostLimitUSD');
      const costUsedDisplay = document.getElementById('editCostUsedDisplay');
      costLimitInput.value = token.cost_limit_usd || 0;

      // 显示已消耗费用
      const costUsed = token.cost_used_usd || 0;
      
      costUsedDisplay.textContent = costUsed > 0 ? `${t('tokens.costUsedPrefix')}: $${costUsed.toFixed(4)}` : '';

      // 初始化模型限制状态（2026-01新增）
      editAllowedModels = (token.allowed_models || []).slice();
      selectedAllowedModelIndices.clear();
      renderAllowedModelsTable();

      document.getElementById('editModal').style.display = 'block';
      pushModal(closeEditModal);
    }

    function closeEditModal() {
      document.getElementById('editModal').style.display = 'none';
      // 清理模型限制状态
      editAllowedModels = [];
      selectedAllowedModelIndices.clear();
      popModal();
    }

    async function updateToken() {
      
      const id = document.getElementById('editTokenId').value;
      const description = document.getElementById('editTokenDescription').value.trim();
      const isActive = document.getElementById('editTokenActive').checked;
      const expiryType = document.getElementById('editTokenExpiry').value;
      const costLimitUSD = parseFloat(document.getElementById('editCostLimitUSD').value) || 0;
      let expiresAt = null;
      if (expiryType !== 'never') {
        if (expiryType === 'custom') {
          const customDate = document.getElementById('editCustomExpiry').value;
          if (!customDate) {
            window.showNotification(t('tokens.msg.selectExpiry'), 'error');
            return;
          }
          expiresAt = new Date(customDate).getTime();
        } else {
          const days = parseInt(expiryType);
          expiresAt = Date.now() + days * 24 * 60 * 60 * 1000;
        }
      }
      try {
        await fetchDataWithAuth(`${API_BASE}/auth-tokens/${id}`, {
          method: 'PUT',
          headers: {
            'Content-Type': 'application/json'
          },
          body: JSON.stringify({
            description,
            is_active: isActive,
            expires_at: expiresAt,
            allowed_models: editAllowedModels,  // 2026-01新增：模型限制
            cost_limit_usd: costLimitUSD         // 2026-01新增：费用上限
          })
        });
        closeEditModal();
        loadTokens();
        window.showNotification(t('tokens.msg.updateSuccess'), 'success');
      } catch (error) {
        console.error('Failed to update token:', error);
        window.showNotification(t('tokens.msg.updateFailed') + ': ' + error.message, 'error');
      }
    }

    async function deleteToken(id) {
      
      if (!confirm(t('tokens.msg.deleteConfirm'))) return;
      try {
        await fetchDataWithAuth(`${API_BASE}/auth-tokens/${id}`, {
          method: 'DELETE'
        });
        loadTokens();
        window.showNotification(t('tokens.msg.deleteSuccess'), 'success');
      } catch (error) {
        console.error('Failed to delete token:', error);
        window.showNotification(t('tokens.msg.deleteFailed') + ': ' + error.message, 'error');
      }
    }

    // 初始化顶部导航栏
    document.addEventListener('DOMContentLoaded', () => {
      initTopbar('tokens');
    });

    // ============================================================================
    // 模型限制功能（2026-01新增）
    // ============================================================================

    /**
     * 加载渠道数据（用于模型选择）
     */
    async function loadChannelsData() {
      try {
        const data = await fetchDataWithAuth(`${API_BASE}/channels`);
        // API 直接返回渠道数组
        allChannels = Array.isArray(data) ? data : (data && data.channels) || [];
        // 聚合可用模型
        availableModelsCache = getAvailableModels();
      } catch (error) {
        console.error('Failed to load channels data:', error);
      }
    }

    /**
     * 从渠道数据聚合所有模型（去重+排序）
     */
    function getAvailableModels() {
      const modelSet = new Set();
      allChannels.forEach(ch => {
        (ch.models || []).forEach(m => {
          if (m.model) modelSet.add(m.model);
        });
      });
      return Array.from(modelSet).sort();
    }

    /**
     * 渲染模型限制表格
     */
    function renderAllowedModelsTable() {
      const tbody = document.getElementById('allowedModelsTableBody');
      const countSpan = document.getElementById('editAllowedModelsCount');
      const batchDeleteBtn = document.getElementById('batchDeleteAllowedModelsBtn');
      const selectAllCheckbox = document.getElementById('selectAllAllowedModels');

      if (!tbody) return;

      // 更新计数
      if (countSpan) countSpan.textContent = editAllowedModels.length;

      // 更新批量删除按钮状态
      updateBatchDeleteBtn();

      // 更新全选复选框状态
      if (selectAllCheckbox) {
        selectAllCheckbox.checked = editAllowedModels.length > 0 &&
          selectedAllowedModelIndices.size === editAllowedModels.length;
      }

      if (editAllowedModels.length === 0) {
        
        tbody.innerHTML = `
          <tr>
            <td colspan="3" style="text-align: center; color: var(--neutral-500); padding: 16px;">
              ${t('tokens.noModelRestriction')}
            </td>
          </tr>
        `;
        return;
      }

      tbody.innerHTML = editAllowedModels.map((model, index) => {
        
        return `
        <tr>
          <td style="text-align: center; padding: 8px;">
            <input type="checkbox" class="allowed-model-checkbox" data-index="${index}"
              ${selectedAllowedModelIndices.has(index) ? 'checked' : ''}
              onchange="toggleAllowedModelSelection(${index}, this.checked)">
          </td>
          <td style="padding: 8px; font-family: monospace; font-size: 13px;">${escapeHtml(model)}</td>
          <td style="text-align: center; padding: 8px;">
            <button type="button" class="btn btn-secondary btn-sm" onclick="removeAllowedModel(${index})"
              style="padding: 2px 8px; font-size: 12px;">${t('common.delete')}</button>
          </td>
        </tr>
      `}).join('');
    }

    /**
     * 切换单个模型的选中状态
     */
    function toggleAllowedModelSelection(index, checked) {
      if (checked) {
        selectedAllowedModelIndices.add(index);
      } else {
        selectedAllowedModelIndices.delete(index);
      }
      updateBatchDeleteBtn();
      updateSelectAllCheckbox();
    }

    /**
     * 全选/取消全选模型
     */
    function toggleSelectAllAllowedModels(checked) {
      if (checked) {
        editAllowedModels.forEach((_, index) => selectedAllowedModelIndices.add(index));
      } else {
        selectedAllowedModelIndices.clear();
      }
      renderAllowedModelsTable();
    }

    /**
     * 更新批量删除按钮状态
     */
    function updateBatchDeleteBtn() {
      const btn = document.getElementById('batchDeleteAllowedModelsBtn');
      if (btn) {
        const hasSelection = selectedAllowedModelIndices.size > 0;
        btn.disabled = !hasSelection;
        btn.style.opacity = hasSelection ? '1' : '0.5';
      }
    }

    /**
     * 更新全选复选框状态
     */
    function updateSelectAllCheckbox() {
      const checkbox = document.getElementById('selectAllAllowedModels');
      if (checkbox) {
        checkbox.checked = editAllowedModels.length > 0 &&
          selectedAllowedModelIndices.size === editAllowedModels.length;
      }
    }

    /**
     * 删除单个模型
     */
    function removeAllowedModel(index) {
      editAllowedModels.splice(index, 1);
      // 重建选中索引（删除后索引会变化）
      const newIndices = new Set();
      selectedAllowedModelIndices.forEach(i => {
        if (i < index) newIndices.add(i);
        else if (i > index) newIndices.add(i - 1);
      });
      selectedAllowedModelIndices = newIndices;
      renderAllowedModelsTable();
    }

    /**
     * 批量删除选中的模型
     */
    function batchDeleteSelectedAllowedModels() {
      if (selectedAllowedModelIndices.size === 0) return;

      // 从大到小排序，避免删除时索引偏移问题
      const indices = Array.from(selectedAllowedModelIndices).sort((a, b) => b - a);
      indices.forEach(index => {
        editAllowedModels.splice(index, 1);
      });
      selectedAllowedModelIndices.clear();
      renderAllowedModelsTable();
    }

    /**
     * 显示模型选择对话框
     */
    function showModelSelectModal() {
      selectedModelsForAdd.clear();
      document.getElementById('modelSearchInput').value = '';
      renderAvailableModels('');
      document.getElementById('modelSelectModal').style.display = 'block';
      pushModal(closeModelSelectModal);
    }

    /**
     * 关闭模型选择对话框
     */
    function closeModelSelectModal() {
      document.getElementById('modelSelectModal').style.display = 'none';
      selectedModelsForAdd.clear();
      popModal();
    }

    /**
     * 搜索过滤可用模型
     */
    function filterAvailableModels(searchText) {
      renderAvailableModels(searchText);
    }

    /**
     * 渲染可用模型列表
     */
    function renderAvailableModels(searchText) {
      const container = document.getElementById('availableModelsContainer');
      const countSpan = document.getElementById('selectedModelsCount');
      const selectAllContainer = document.getElementById('selectAllContainer');
      const selectAllCheckbox = document.getElementById('selectAllModelsCheckbox');
      const visibleModelsCount = document.getElementById('visibleModelsCount');
      if (!container) return;

      // 过滤已添加的模型
      const existingModels = new Set(editAllowedModels.map(m => m.toLowerCase()));
      let models = availableModelsCache.filter(m => !existingModels.has(m.toLowerCase()));

      // 搜索过滤
      if (searchText) {
        const search = searchText.toLowerCase();
        models = models.filter(m => m.toLowerCase().includes(search));
      }

      // 保存当前可见模型列表（用于全选功能）
      currentVisibleModels = models;

      // 更新选中计数
      if (countSpan) countSpan.textContent = selectedModelsForAdd.size;

      
      if (models.length === 0) {
        const isEmptyCache = availableModelsCache.length === 0;
        const message = searchText
          ? t('tokens.noMatchingModel')
          : isEmptyCache
            ? t('tokens.channelNoModel')
            : t('tokens.allModelsAdded');
        container.innerHTML = `
          <div style="text-align: center; color: var(--neutral-500); padding: 24px;">
            ${message}
          </div>
        `;
        // 隐藏全选容器，恢复列表圆角
        if (selectAllContainer) selectAllContainer.style.display = 'none';
        container.style.borderRadius = '6px';
        return;
      }

      // 显示全选容器，调整列表圆角
      if (selectAllContainer) {
        selectAllContainer.style.display = 'block';
        container.style.borderRadius = '0 0 6px 6px';
      }

      // 更新全选复选框状态
      if (selectAllCheckbox) {
        const allSelected = models.every(m => selectedModelsForAdd.has(m));
        selectAllCheckbox.checked = allSelected;
        selectAllCheckbox.indeterminate = !allSelected && models.some(m => selectedModelsForAdd.has(m));
      }
      if (visibleModelsCount) {
        
        visibleModelsCount.textContent = t('tokens.visibleModelsCount', { count: models.length });
      }

      container.innerHTML = models.map(model => `
        <label class="model-option-item" data-model="${escapeHtml(model)}"
          style="display: flex; align-items: center; padding: 8px 12px; cursor: pointer; border-bottom: 1px solid var(--neutral-100);">
          <input type="checkbox" class="model-option-checkbox" data-model="${escapeHtml(model)}" style="margin-right: 8px;"
            ${selectedModelsForAdd.has(model) ? 'checked' : ''}>
          <span style="font-family: monospace; font-size: 13px;">${escapeHtml(model)}</span>
        </label>
      `).join('');

      // Event delegation: attach once on container
      if (!container.dataset.delegated) {
        container.addEventListener('change', (e) => {
          const checkbox = e.target.closest('.model-option-checkbox');
          if (checkbox) {
            toggleModelForAdd(checkbox.dataset.model || '', checkbox.checked);
          }
        });
        container.dataset.delegated = '1';
      }
    }

    /**
     * 切换待添加模型的选中状态
     */
    function toggleModelForAdd(model, checked) {
      if (checked) {
        selectedModelsForAdd.add(model);
      } else {
        selectedModelsForAdd.delete(model);
      }
      document.getElementById('selectedModelsCount').textContent = selectedModelsForAdd.size;
      updateSelectAllCheckboxState();
    }

    /**
     * 更新全选复选框状态
     */
    function updateSelectAllCheckboxState() {
      const selectAllCheckbox = document.getElementById('selectAllModelsCheckbox');
      if (!selectAllCheckbox || currentVisibleModels.length === 0) return;

      const allSelected = currentVisibleModels.every(m => selectedModelsForAdd.has(m));
      selectAllCheckbox.checked = allSelected;
      selectAllCheckbox.indeterminate = !allSelected && currentVisibleModels.some(m => selectedModelsForAdd.has(m));
    }

    /**
     * 全选/取消全选当前可见模型
     */
    function toggleSelectAllModels(checked) {
      currentVisibleModels.forEach(model => {
        if (checked) {
          selectedModelsForAdd.add(model);
        } else {
          selectedModelsForAdd.delete(model);
        }
      });
      document.getElementById('selectedModelsCount').textContent = selectedModelsForAdd.size;
      // 重新渲染以更新复选框状态
      const searchText = document.getElementById('modelSearchInput')?.value || '';
      renderAvailableModels(searchText);
    }

    /**
     * 确认添加选中的模型
     */
    function confirmModelSelection() {
      
      if (selectedModelsForAdd.size === 0) {
        window.showNotification(t('tokens.msg.selectAtLeastOne'), 'warning');
        return;
      }

      // 添加到模型限制列表
      selectedModelsForAdd.forEach(model => {
        if (!editAllowedModels.includes(model)) {
          editAllowedModels.push(model);
        }
      });

      // 排序
      editAllowedModels.sort();

      closeModelSelectModal();
      renderAllowedModelsTable();
      window.showNotification(t('tokens.msg.modelsAdded', { count: selectedModelsForAdd.size }), 'success');
    }

    // ==================== 模型手动输入 ====================

    /**
     * 解析模型输入，支持逗号和换行分隔
     */
    function parseModelInput(input) {
      return input
        .split(/[,\n]/)
        .map(m => m.trim())
        .filter(m => m);
    }

    /**
     * 显示模型导入对话框
     */
    function showModelImportModal() {
      document.getElementById('tokenModelImportTextarea').value = '';
      document.getElementById('tokenModelImportPreview').style.display = 'none';
      document.getElementById('modelImportModal').style.display = 'block';
      setTimeout(() => document.getElementById('tokenModelImportTextarea').focus(), 100);
      pushModal(closeModelImportModal);
    }

    /**
     * 关闭模型导入对话框
     */
    function closeModelImportModal() {
      document.getElementById('modelImportModal').style.display = 'none';
      popModal();
    }

    /**
     * 更新模型导入预览
     */
    function updateModelImportPreview() {
      const textarea = document.getElementById('tokenModelImportTextarea');
      const preview = document.getElementById('tokenModelImportPreview');
      const countSpan = document.getElementById('tokenModelImportCount');
      const input = textarea.value.trim();

      if (!input) {
        preview.style.display = 'none';
        return;
      }

      const models = parseModelInput(input);
      // 去重并排除已存在的模型
      const existingModels = new Set(editAllowedModels.map(m => m.toLowerCase()));
      const newModels = [...new Set(models)].filter(m => !existingModels.has(m.toLowerCase()));

      if (newModels.length > 0) {
        countSpan.textContent = newModels.length;
        preview.style.display = 'block';
      } else {
        preview.style.display = 'none';
      }
    }

    /**
     * 确认模型导入
     */
    function confirmModelImport() {
      
      const textarea = document.getElementById('tokenModelImportTextarea');
      const input = textarea.value.trim();

      if (!input) {
        window.showNotification(t('tokens.msg.enterModelName'), 'warning');
        return;
      }

      const models = parseModelInput(input);
      if (models.length === 0) {
        window.showNotification(t('tokens.msg.noValidModel'), 'warning');
        return;
      }

      // 去重并排除已存在的模型
      const existingModels = new Set(editAllowedModels.map(m => m.toLowerCase()));
      const newModels = [...new Set(models)].filter(m => !existingModels.has(m.toLowerCase()));

      if (newModels.length === 0) {
        window.showNotification(t('tokens.msg.allModelsExist'), 'info');
        closeModelImportModal();
        return;
      }

      // 添加新模型
      newModels.forEach(model => editAllowedModels.push(model));
      editAllowedModels.sort();

      closeModelImportModal();
      renderAllowedModelsTable();

      const duplicateCount = models.length - newModels.length;
      const msg = duplicateCount > 0
        ? t('tokens.msg.importSuccessWithDuplicates', { added: newModels.length, duplicates: duplicateCount })
        : t('tokens.msg.importSuccess', { count: newModels.length });
      window.showNotification(msg, 'success');
    }
