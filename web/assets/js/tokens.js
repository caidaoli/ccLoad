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
    let editAllowedChannelIDs = [];           // 编辑模态框中当前的渠道限制列表
    let selectedAllowedChannelIDs = new Set(); // 已选中的渠道ID（批量删除用）
    let selectedChannelsForAdd = new Set();   // 渠道选择对话框中已选的渠道ID
    let currentVisibleChannels = [];          // 当前可见的渠道列表（用于全选功能）

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

    function initExpirySelects() {
      const template = document.getElementById('tpl-token-expiry-options');
      if (!template) return;

      const optionsHtml = template.innerHTML.trim();
      document.querySelectorAll('[data-expiry-select]').forEach((select) => {
        const currentValue = select.value;
        select.innerHTML = optionsHtml;
        if (currentValue) {
          select.value = currentValue;
        }
      });
    }

    window.initPageBootstrap({
      topbarKey: 'tokens',
      run: () => {
      initExpirySelects();

      const renderTimeRangeSelector = () => {
        if (typeof window.renderDateRangeButtons === 'function') {
          window.renderDateRangeButtons('tokens-time-range', {
            values: ['today', 'yesterday', 'day_before_yesterday', 'this_week', 'this_month', 'last_month', 'all'],
            includeAll: true,
            activeValue: currentTimeRange
          });
        }
      };

      renderTimeRangeSelector();

      // 初始化时间范围选择器
      window.initTimeRangeSelector((range) => {
        currentTimeRange = range;
        loadTokens();
      });

      // 加载令牌列表(默认显示本日统计)
      loadTokens();

      // 预加载渠道数据（用于模型选择）
      loadChannelsData();

      initPageActionDelegation();

      // 初始化事件委托
      initEventDelegation();

      // 监听语言切换事件，重新渲染令牌列表
      window.i18n.onLocaleChange(() => {
        renderTimeRangeSelector();
        window.initTimeRangeSelector((range) => {
          currentTimeRange = range;
          loadTokens();
        });
        renderAllowedChannelsTable();
        renderTokens();
      });
      }
    });

    function initPageActionDelegation() {
      if (typeof window.initDelegatedActions !== 'function') return;

      window.initDelegatedActions({
        boundKey: 'tokensPageActionsBound',
        click: {
          'show-create-modal': () => showCreateModal(),
          'close-create-modal': () => closeCreateModal(),
          'create-token': () => createToken(),
          'close-token-result-modal': () => closeTokenResultModal(),
          'copy-token-result': () => copyToken(),
          'close-edit-modal': () => closeEditModal(),
          'update-token': () => updateToken(),
          'show-model-select-modal': () => showModelSelectModal(),
          'show-model-import-modal': () => showModelImportModal(),
          'batch-delete-allowed-models': () => batchDeleteSelectedAllowedModels(),
          'show-channel-select-modal': () => showChannelSelectModal(),
          'batch-delete-allowed-channels': () => batchDeleteSelectedAllowedChannels(),
          'close-channel-select-modal': () => closeChannelSelectModal(),
          'confirm-channel-selection': () => confirmChannelSelection(),
          'close-model-select-modal': () => closeModelSelectModal(),
          'confirm-model-selection': () => confirmModelSelection(),
          'close-model-import-modal': () => closeModelImportModal(),
          'confirm-model-import': () => confirmModelImport(),
          'remove-allowed-model': (actionTarget) => {
            const index = Number(actionTarget.dataset.index);
            if (!Number.isNaN(index)) {
              removeAllowedModel(index);
            }
          },
          'remove-allowed-channel': (actionTarget) => {
            const channelID = Number(actionTarget.dataset.channelId);
            if (!Number.isNaN(channelID)) {
              removeAllowedChannel(channelID);
            }
          }
        },
        change: {
          'toggle-custom-expiry': (actionTarget) => {
            document.getElementById('customExpiryContainer').style.display =
              actionTarget.value === 'custom' ? 'block' : 'none';
          },
          'toggle-edit-custom-expiry': (actionTarget) => {
            document.getElementById('editCustomExpiryContainer').style.display =
              actionTarget.value === 'custom' ? 'block' : 'none';
          },
          'toggle-select-all-allowed-channels': (actionTarget) => toggleSelectAllAllowedChannels(actionTarget.checked),
          'toggle-select-all-channels': (actionTarget) => toggleSelectAllChannels(actionTarget.checked),
          'toggle-select-all-allowed-models': (actionTarget) => toggleSelectAllAllowedModels(actionTarget.checked),
          'toggle-select-all-models': (actionTarget) => toggleSelectAllModels(actionTarget.checked),
          'toggle-allowed-channel': (actionTarget) => {
            const channelID = Number(actionTarget.dataset.channelId);
            if (!Number.isNaN(channelID)) {
              toggleAllowedChannelSelection(channelID, actionTarget.checked);
            }
          },
          'toggle-allowed-model': (actionTarget) => {
            const index = Number(actionTarget.dataset.index);
            if (!Number.isNaN(index)) {
              toggleAllowedModelSelection(index, actionTarget.checked);
            }
          }
        },
        input: {
          'filter-available-channels': (actionTarget) => filterAvailableChannels(actionTarget.value),
          'filter-available-models': (actionTarget) => filterAvailableModels(actionTarget.value),
          'update-model-import-preview': () => updateModelImportPreview()
        }
      });
    }

    /**
     * 初始化事件委托(统一处理表格内按钮点击)
     */
    function initEventDelegation() {
      const container = document.getElementById('tokens-container');
      if (!container) return;

      container.addEventListener('click', (e) => {
        const target = e.target.closest('.btn-copy-token, .btn-edit, .btn-delete');
        if (!target) return;

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
      table.className = 'mobile-card-table tokens-table';
      
      table.innerHTML = `
        <thead>
          <tr>
            <th>${t('tokens.table.description')}</th>
            <th>${t('tokens.table.token')}</th>
            <th class="tokens-table-head-center">${t('tokens.table.callCount')}</th>
            <th class="tokens-table-head-center">${t('tokens.table.successRate')}</th>
            <th class="tokens-table-head-center" title="${t('tokens.table.rpmTitle')}">${t('tokens.table.rpm')}</th>
            <th class="tokens-table-head-center">${t('tokens.table.tokenUsage')}</th>
            <th class="tokens-table-head-center">${t('tokens.table.totalCost')}</th>
            <th class="tokens-table-head-center">${t('tokens.table.concurrency')}</th>
            <th class="tokens-table-head-center">${t('tokens.table.streamAvg')}</th>
            <th class="tokens-table-head-center">${t('tokens.table.nonStreamAvg')}</th>
            <th>${t('tokens.table.lastUsed')}</th>
            <th class="tokens-actions-col">${t('tokens.table.actions')}</th>
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
      const costHtml = buildCostHtml(token.total_cost_usd, token.effective_cost_usd);
      const concurrencyHtml = buildConcurrencyHtml(token.max_concurrency);
      const streamAvgHtml = buildResponseTimeHtml(token.stream_avg_ttfb, token.stream_count);
      const nonStreamAvgHtml = buildResponseTimeHtml(token.non_stream_avg_rt, token.non_stream_count);
      const costCellClass = token.total_cost_usd > 0 ? '' : 'mobile-empty-cell';
      const streamCellClass = token.stream_count ? '' : 'mobile-empty-cell';
      const nonStreamCellClass = token.non_stream_count ? '' : 'mobile-empty-cell';

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
        costCellClass: costCellClass,
        concurrencyHtml: concurrencyHtml,
        streamAvgHtml: streamAvgHtml,
        streamCellClass: streamCellClass,
        nonStreamAvgHtml: nonStreamAvgHtml,
        nonStreamCellClass: nonStreamCellClass,
        lastUsed: lastUsed,
        mobileLabelDescription: t('tokens.table.description'),
        mobileLabelToken: t('tokens.table.token'),
        mobileLabelCalls: t('tokens.table.callCount'),
        mobileLabelSuccessRate: t('tokens.table.successRate'),
        mobileLabelRpm: t('tokens.table.rpm'),
        mobileLabelTokenUsage: t('tokens.table.tokenUsage'),
        mobileLabelCost: t('tokens.table.totalCost'),
        mobileLabelConcurrency: t('tokens.table.concurrency'),
        mobileLabelStream: t('tokens.table.streamAvg'),
        mobileLabelNonStream: t('tokens.table.nonStreamAvg'),
        mobileLabelLastUsed: t('tokens.table.lastUsed'),
        mobileLabelActions: t('tokens.table.actions')
      });
    }

    /**
     * 构建调用次数HTML
     */
    function buildCallsHtml(successCount, failureCount, totalCount) {
      if (totalCount === 0) {
        return '<span class="token-value-muted">-</span>';
      }

      let html = '<div class="token-call-stats">';
      html += `<span class="stats-badge token-call-badge token-call-badge--success" title="${t('tokens.successCall')}">`;
      html += `<span class="token-call-icon token-call-icon--success">✓</span> ${successCount.toLocaleString()}`;
      html += `</span>`;

      if (failureCount > 0) {
        html += `<span class="stats-badge token-call-badge token-call-badge--failure" title="${t('tokens.failedCall')}">`;
        html += `<span class="token-call-icon token-call-icon--failure">✗</span> ${failureCount.toLocaleString()}`;
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
        return '<span class="token-value-muted">-</span>';
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

      let rpmClass = 'token-rpm token-rpm--high';
      if (peakRPM < 10) rpmClass = 'token-rpm token-rpm--low';
      else if (peakRPM < 100) rpmClass = 'token-rpm token-rpm--medium';

      return `<span class="${rpmClass}">${peakText}/${avgText}/${recentText}</span>`;
    }

    /**
     * RPM 颜色：低流量绿色，中等橙色，高流量红色
     */
    /**
     * 构建成功率HTML
     */
    function buildSuccessRateHtml(successRate, totalCount) {
      if (totalCount === 0) {
        return '<span class="token-value-muted">-</span>';
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
        return '<span class="token-value-muted">-</span>';
      }

      const items = [];
      const pushUsageItem = (variant, label, title, count) => {
        if (!count || count <= 0) return;
        items.push(
          `<span class="token-usage-item token-usage-item--${variant}" title="${title}">` +
            `<span class="token-usage-label">${label}</span>` +
            `<span class="token-usage-value">${formatTokenCount(count)}</span>` +
          `</span>`
        );
      };

      pushUsageItem('input', t('tokens.input'), t('tokens.inputTokens'), token.prompt_tokens_total || 0);
      pushUsageItem('output', t('tokens.output'), t('tokens.outputTokens'), token.completion_tokens_total || 0);
      pushUsageItem('cache-read', t('tokens.cacheRead'), t('tokens.cacheReadTokens'), token.cache_read_tokens_total || 0);
      pushUsageItem('cache-create', t('tokens.cacheCreate'), t('tokens.cacheCreateTokens'), token.cache_creation_tokens_total || 0);

      return `<div class="token-usage-metrics">${items.join('')}</div>`;
    }

    /**
     * 构建总费用HTML
     */
    function buildCostHtml(totalCostUsd, effectiveCostUsd) {
      if (!totalCostUsd || totalCostUsd <= 0) {
        return '<span class="token-value-muted">-</span>';
      }

      const costStack = buildCostStackHtml(totalCostUsd, effectiveCostUsd, { tone: 'warning' });
      return `
        <div class="token-cost">
          ${costStack}
        </div>
      `;
    }

    function buildConcurrencyHtml(maxConcurrency) {
      const limit = Number(maxConcurrency) || 0;
      if (limit <= 0) {
        return '<span class="token-value-muted">∞</span>';
      }
      return `<span class="metric-value">${limit.toLocaleString()}</span>`;
    }

    /**
     * 构建响应时间HTML
     */
    function buildResponseTimeHtml(time, count) {
      if (!count || count === 0) {
        return '<span class="token-value-muted">-</span>';
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
      const costHtml = buildCostHtml(token.total_cost_usd, token.effective_cost_usd);
      const concurrencyHtml = buildConcurrencyHtml(token.max_concurrency);
      const streamAvgHtml = buildResponseTimeHtml(token.stream_avg_ttfb, token.stream_count);
      const nonStreamAvgHtml = buildResponseTimeHtml(token.non_stream_avg_rt, token.non_stream_count);
      const costCellClass = token.total_cost_usd > 0 ? '' : ' mobile-empty-cell';
      const streamCellClass = token.stream_count ? '' : ' mobile-empty-cell';
      const nonStreamCellClass = token.non_stream_count ? '' : ' mobile-empty-cell';

      const maskedToken = token.token.length > 8
        ? token.token.substring(0, 4) + '****' + token.token.slice(-4)
        : token.token;

      return `
        <tr class="mobile-card-row token-card-row" data-token-id="${token.id}">
          <td class="tokens-col-description" data-mobile-label="${t('tokens.table.description')}">${escapeHtml(token.description)}</td>
          <td class="tokens-col-token" data-mobile-label="${t('tokens.table.token')}">
            <div><span class="token-display token-display-${status.class}">${escapeHtml(maskedToken)}</span></div>
            <div class="token-row-meta">${createdAt}${t('tokens.createdSuffix')} · ${expiresAt}</div>
          </td>
          <td class="tokens-col-calls" data-mobile-label="${t('tokens.table.callCount')}">${callsHtml}</td>
          <td class="tokens-col-success-rate" data-mobile-label="${t('tokens.table.successRate')}">${successRateHtml}</td>
          <td class="tokens-col-rpm" data-mobile-label="${t('tokens.table.rpm')}">${rpmHtml}</td>
          <td class="tokens-col-token-usage" data-mobile-label="${t('tokens.table.tokenUsage')}">${tokensHtml}</td>
          <td class="tokens-col-cost${costCellClass}" data-mobile-label="${t('tokens.table.totalCost')}">${costHtml}</td>
          <td class="tokens-col-concurrency" data-mobile-label="${t('tokens.table.concurrency')}">${concurrencyHtml}</td>
          <td class="tokens-col-stream${streamCellClass}" data-mobile-label="${t('tokens.table.streamAvg')}">${streamAvgHtml}</td>
          <td class="tokens-col-non-stream${nonStreamCellClass}" data-mobile-label="${t('tokens.table.nonStreamAvg')}">${nonStreamAvgHtml}</td>
          <td class="tokens-col-last-used" data-mobile-label="${t('tokens.table.lastUsed')}">${lastUsed}</td>
          <td class="tokens-col-actions" data-mobile-label="${t('tokens.table.actions')}">
            <div class="token-row-actions">
              <button class="btn-copy-token btn btn-secondary token-row-action-btn" data-token="${escapeHtml(token.token)}">${t('common.copy')}</button>
              <button class="btn btn-secondary btn-edit token-row-action-btn">${t('common.edit')}</button>
              <button class="btn btn-danger btn-delete token-row-action-btn">${t('common.delete')}</button>
            </div>
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
      document.getElementById('tokenMaxConcurrency').value = 0;
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
      const maxConcurrency = parseInt(document.getElementById('tokenMaxConcurrency').value, 10) || 0;
      if (costLimitUSD < 0) {
        window.showNotification(t('tokens.msg.costLimitNegative'), 'error');
        return;
      }
      if (maxConcurrency < 0) {
        window.showNotification(t('tokens.msg.maxConcurrencyNegative'), 'error');
        return;
      }
      try {
        const data = await fetchDataWithAuth(`${API_BASE}/auth-tokens`, {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json'
          },
          body: JSON.stringify({ description, expires_at: expiresAt, is_active: isActive, cost_limit_usd: costLimitUSD, max_concurrency: maxConcurrency })
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
      window.copyToClipboard(textarea.value).then(() => {
        window.showNotification(t('tokens.msg.copySuccess'), 'success');
      });
    }

    function copyTokenToClipboard(hash) {
      window.copyToClipboard(hash).then(() => {
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
      document.getElementById('editTokenValue').value = token.token || '';
      document.getElementById('editTokenDescription').value = token.description;
      document.getElementById('editTokenActive').checked = token.is_active;
      if (!token.expires_at) {
        document.getElementById('editTokenExpiry').value = 'never';
        document.getElementById('editCustomExpiryContainer').style.display = 'none';
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

      const maxConcurrencyInput = document.getElementById('editMaxConcurrency');
      maxConcurrencyInput.value = token.max_concurrency || 0;

      // 初始化模型限制状态（2026-01新增）
      editAllowedModels = (token.allowed_models || []).slice();
      selectedAllowedModelIndices.clear();
      renderAllowedModelsTable();

      // 初始化渠道限制状态（2026-04新增）
      editAllowedChannelIDs = (token.allowed_channel_ids || []).slice();
      selectedAllowedChannelIDs.clear();
      renderAllowedChannelsTable();
      if (allChannels.length === 0) {
        loadChannelsData().then(() => renderAllowedChannelsTable());
      }

      document.getElementById('editModal').style.display = 'block';
      pushModal(closeEditModal);
    }

    function closeEditModal() {
      document.getElementById('editModal').style.display = 'none';
      document.getElementById('editTokenValue').value = '';
      document.getElementById('editCustomExpiryContainer').style.display = 'none';
      // 清理模型限制状态
      editAllowedModels = [];
      selectedAllowedModelIndices.clear();
      editAllowedChannelIDs = [];
      selectedAllowedChannelIDs.clear();
      popModal();
    }

    async function updateToken() {
      
      const id = document.getElementById('editTokenId').value;
      const description = document.getElementById('editTokenDescription').value.trim();
      const isActive = document.getElementById('editTokenActive').checked;
      const expiryType = document.getElementById('editTokenExpiry').value;
      const costLimitUSD = parseFloat(document.getElementById('editCostLimitUSD').value) || 0;
      const maxConcurrency = parseInt(document.getElementById('editMaxConcurrency').value, 10) || 0;
      if (costLimitUSD < 0) {
        window.showNotification(t('tokens.msg.costLimitNegative'), 'error');
        return;
      }
      if (maxConcurrency < 0) {
        window.showNotification(t('tokens.msg.maxConcurrencyNegative'), 'error');
        return;
      }
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
            allowed_channel_ids: editAllowedChannelIDs,
            allowed_models: editAllowedModels,  // 2026-01新增：模型限制
            cost_limit_usd: costLimitUSD,        // 2026-01新增：费用上限
            max_concurrency: maxConcurrency      // 2026-04新增：并发上限
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

    function getAvailableModelsForCurrentChannelRestriction() {
      if (editAllowedChannelIDs.length === 0) {
        return availableModelsCache;
      }

      const allowedChannelIDs = new Set(editAllowedChannelIDs);
      const modelSet = new Set();
      allChannels.forEach(ch => {
        if (!allowedChannelIDs.has(normalizeChannelID(ch.id))) return;
        (ch.models || []).forEach(m => {
          if (m.model) modelSet.add(m.model);
        });
      });
      return Array.from(modelSet).sort();
    }

    function normalizeChannelID(value) {
      const id = Number(value);
      return Number.isFinite(id) ? id : 0;
    }

    function getChannelByID(channelID) {
      return allChannels.find(ch => normalizeChannelID(ch.id) === channelID) || null;
    }

    function getChannelDisplayName(channelID) {
      const channel = getChannelByID(channelID);
      if (!channel) return `${t('common.unknown')} #${channelID}`;
      return `${channel.name || t('common.unknown')} #${channel.id}`;
    }

    function getChannelTypeText(channelID) {
      const channel = getChannelByID(channelID);
      return channel ? (channel.channel_type || '-') : '-';
    }

    function sortAllowedChannelIDs() {
      editAllowedChannelIDs.sort((a, b) => {
        const nameA = getChannelDisplayName(a).toLowerCase();
        const nameB = getChannelDisplayName(b).toLowerCase();
        if (nameA < nameB) return -1;
        if (nameA > nameB) return 1;
        return a - b;
      });
    }

    function renderAllowedChannelsTable() {
      const tbody = document.getElementById('allowedChannelsTableBody');
      const countSpan = document.getElementById('editAllowedChannelsCount');
      const selectAllCheckbox = document.getElementById('selectAllAllowedChannels');
      const mobileLabelChannelName = t('tokens.channelName');
      const mobileLabelChannelType = t('tokens.channelType');
      const mobileLabelActions = t('tokens.table.actions');

      if (!tbody) return;

      if (countSpan) countSpan.textContent = editAllowedChannelIDs.length;
      updateBatchDeleteChannelsBtn();

      if (selectAllCheckbox) {
        selectAllCheckbox.checked = editAllowedChannelIDs.length > 0 &&
          selectedAllowedChannelIDs.size === editAllowedChannelIDs.length;
      }

      if (editAllowedChannelIDs.length === 0) {
        tbody.innerHTML = `
          <tr class="allowed-channels-empty-row">
            <td colspan="4" class="allowed-channels-empty-cell">
              ${t('tokens.noChannelRestriction')}
            </td>
          </tr>
        `;
        return;
      }

      tbody.innerHTML = editAllowedChannelIDs.map((channelID) => `
        <tr class="mobile-inline-row allowed-channel-row">
          <td class="allowed-channel-col-select mobile-inline-no-label">
            <input type="checkbox" class="allowed-channel-checkbox" data-channel-id="${channelID}"
              data-change-action="toggle-allowed-channel"
              ${selectedAllowedChannelIDs.has(channelID) ? 'checked' : ''}
            >
          </td>
          <td class="allowed-channel-col-name" data-mobile-label="${mobileLabelChannelName}">${escapeHtml(getChannelDisplayName(channelID))}</td>
          <td class="allowed-channel-col-type" data-mobile-label="${mobileLabelChannelType}">${escapeHtml(getChannelTypeText(channelID))}</td>
          <td class="allowed-channel-col-actions" data-mobile-label="${mobileLabelActions}">
            <button type="button" class="allowed-channel-remove-btn btn btn-secondary btn-sm" data-action="remove-allowed-channel" data-channel-id="${channelID}">${t('common.delete')}</button>
          </td>
        </tr>
      `).join('');
    }

    function toggleAllowedChannelSelection(channelID, checked) {
      if (checked) {
        selectedAllowedChannelIDs.add(channelID);
      } else {
        selectedAllowedChannelIDs.delete(channelID);
      }
      updateBatchDeleteChannelsBtn();
      updateSelectAllAllowedChannelsCheckbox();
    }

    function toggleSelectAllAllowedChannels(checked) {
      if (checked) {
        editAllowedChannelIDs.forEach(channelID => selectedAllowedChannelIDs.add(channelID));
      } else {
        selectedAllowedChannelIDs.clear();
      }
      renderAllowedChannelsTable();
    }

    function updateBatchDeleteChannelsBtn() {
      const btn = document.getElementById('batchDeleteAllowedChannelsBtn');
      if (btn) {
        btn.disabled = selectedAllowedChannelIDs.size === 0;
      }
    }

    function updateSelectAllAllowedChannelsCheckbox() {
      const checkbox = document.getElementById('selectAllAllowedChannels');
      if (checkbox) {
        checkbox.checked = editAllowedChannelIDs.length > 0 &&
          selectedAllowedChannelIDs.size === editAllowedChannelIDs.length;
      }
    }

    function removeAllowedChannel(channelID) {
      editAllowedChannelIDs = editAllowedChannelIDs.filter(id => id !== channelID);
      selectedAllowedChannelIDs.delete(channelID);
      renderAllowedChannelsTable();
    }

    function batchDeleteSelectedAllowedChannels() {
      if (selectedAllowedChannelIDs.size === 0) return;

      editAllowedChannelIDs = editAllowedChannelIDs.filter(id => !selectedAllowedChannelIDs.has(id));
      selectedAllowedChannelIDs.clear();
      renderAllowedChannelsTable();
    }

    async function showChannelSelectModal() {
      if (allChannels.length === 0) {
        await loadChannelsData();
      }
      selectedChannelsForAdd.clear();
      document.getElementById('channelSearchInput').value = '';
      renderAvailableChannels('');
      document.getElementById('channelSelectModal').style.display = 'block';
      pushModal(closeChannelSelectModal);
    }

    function closeChannelSelectModal() {
      document.getElementById('channelSelectModal').style.display = 'none';
      selectedChannelsForAdd.clear();
      popModal();
    }

    function filterAvailableChannels(searchText) {
      renderAvailableChannels(searchText);
    }

    function renderAvailableChannels(searchText) {
      const container = document.getElementById('availableChannelsContainer');
      const countSpan = document.getElementById('selectedChannelsCount');
      const selectAllContainer = document.getElementById('selectAllChannelsContainer');
      const selectAllCheckbox = document.getElementById('selectAllChannelsCheckbox');
      const visibleChannelsCount = document.getElementById('visibleChannelsCount');
      if (!container) return;

      const existingChannelIDs = new Set(editAllowedChannelIDs);
      let channels = allChannels.filter(ch => !existingChannelIDs.has(normalizeChannelID(ch.id)));

      if (searchText) {
        const search = searchText.toLowerCase();
        channels = channels.filter(ch => {
          const name = String(ch.name || '').toLowerCase();
          const type = String(ch.channel_type || '').toLowerCase();
          const id = String(ch.id || '');
          return name.includes(search) || type.includes(search) || id.includes(search);
        });
      }

      currentVisibleChannels = channels;
      if (countSpan) countSpan.textContent = selectedChannelsForAdd.size;

      if (channels.length === 0) {
        const message = searchText
          ? t('tokens.noMatchingChannel')
          : allChannels.length === 0
            ? t('tokens.noChannelsConfigured')
            : t('tokens.allChannelsAdded');
        container.innerHTML = `<div class="available-channels-empty">${message}</div>`;
        if (selectAllContainer) selectAllContainer.style.display = 'none';
        container.classList.add('available-channels-container--standalone');
        container.classList.remove('available-channels-container--stacked');
        return;
      }

      if (selectAllContainer) {
        selectAllContainer.style.display = 'block';
      }
      container.classList.add('available-channels-container--stacked');
      container.classList.remove('available-channels-container--standalone');

      if (selectAllCheckbox) {
        const allSelected = channels.every(ch => selectedChannelsForAdd.has(normalizeChannelID(ch.id)));
        selectAllCheckbox.checked = allSelected;
        selectAllCheckbox.indeterminate = !allSelected && channels.some(ch => selectedChannelsForAdd.has(normalizeChannelID(ch.id)));
      }
      if (visibleChannelsCount) {
        visibleChannelsCount.textContent = t('tokens.visibleChannelsCount', { count: channels.length });
      }

      container.innerHTML = channels.map(ch => {
        const channelID = normalizeChannelID(ch.id);
        return `
          <label class="channel-option-item" data-channel-id="${channelID}">
            <input type="checkbox" class="channel-option-checkbox" data-channel-id="${channelID}"
              ${selectedChannelsForAdd.has(channelID) ? 'checked' : ''}>
            <span class="channel-option-label">${escapeHtml(ch.name || t('common.unknown'))}</span>
            <span class="channel-option-meta">#${channelID} · ${escapeHtml(ch.channel_type || '-')}</span>
          </label>
        `;
      }).join('');

      if (!container.dataset.delegated) {
        container.addEventListener('change', (e) => {
          const checkbox = e.target.closest('.channel-option-checkbox');
          if (checkbox) {
            toggleChannelForAdd(normalizeChannelID(checkbox.dataset.channelId), checkbox.checked);
          }
        });
        container.dataset.delegated = '1';
      }
    }

    function toggleChannelForAdd(channelID, checked) {
      if (checked) {
        selectedChannelsForAdd.add(channelID);
      } else {
        selectedChannelsForAdd.delete(channelID);
      }
      document.getElementById('selectedChannelsCount').textContent = selectedChannelsForAdd.size;
      updateSelectAllChannelsCheckboxState();
    }

    function updateSelectAllChannelsCheckboxState() {
      const selectAllCheckbox = document.getElementById('selectAllChannelsCheckbox');
      if (!selectAllCheckbox || currentVisibleChannels.length === 0) return;

      const allSelected = currentVisibleChannels.every(ch => selectedChannelsForAdd.has(normalizeChannelID(ch.id)));
      selectAllCheckbox.checked = allSelected;
      selectAllCheckbox.indeterminate = !allSelected && currentVisibleChannels.some(ch => selectedChannelsForAdd.has(normalizeChannelID(ch.id)));
    }

    function toggleSelectAllChannels(checked) {
      currentVisibleChannels.forEach(ch => {
        const channelID = normalizeChannelID(ch.id);
        if (checked) {
          selectedChannelsForAdd.add(channelID);
        } else {
          selectedChannelsForAdd.delete(channelID);
        }
      });
      document.getElementById('selectedChannelsCount').textContent = selectedChannelsForAdd.size;
      const searchText = document.getElementById('channelSearchInput')?.value || '';
      renderAvailableChannels(searchText);
    }

    function confirmChannelSelection() {
      if (selectedChannelsForAdd.size === 0) {
        window.showNotification(t('tokens.msg.selectAtLeastOneChannel'), 'warning');
        return;
      }

      const existingChannelIDs = new Set(editAllowedChannelIDs);
      selectedChannelsForAdd.forEach(channelID => {
        if (!existingChannelIDs.has(channelID)) {
          editAllowedChannelIDs.push(channelID);
        }
      });

      sortAllowedChannelIDs();
      closeChannelSelectModal();
      renderAllowedChannelsTable();
      window.showNotification(t('tokens.msg.channelsAdded', { count: selectedChannelsForAdd.size }), 'success');
    }

    /**
     * 渲染模型限制表格
     */
    function renderAllowedModelsTable() {
      const tbody = document.getElementById('allowedModelsTableBody');
      const countSpan = document.getElementById('editAllowedModelsCount');
      const selectAllCheckbox = document.getElementById('selectAllAllowedModels');
      const mobileLabelModelName = t('tokens.modelName');
      const mobileLabelActions = t('tokens.table.actions');

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
          <tr class="allowed-models-empty-row">
            <td colspan="3" class="allowed-models-empty-cell">
              ${t('tokens.noModelRestriction')}
            </td>
          </tr>
        `;
        return;
      }

      tbody.innerHTML = editAllowedModels.map((model, index) => {
        return `
        <tr class="mobile-inline-row allowed-model-row">
          <td class="allowed-model-col-select mobile-inline-no-label">
            <input type="checkbox" class="allowed-model-checkbox" data-index="${index}"
              data-change-action="toggle-allowed-model"
              ${selectedAllowedModelIndices.has(index) ? 'checked' : ''}
            >
          </td>
          <td class="allowed-model-col-name" data-mobile-label="${mobileLabelModelName}">${escapeHtml(model)}</td>
          <td class="allowed-model-col-actions" data-mobile-label="${mobileLabelActions}">
            <button type="button" class="allowed-model-remove-btn btn btn-secondary btn-sm" data-action="remove-allowed-model" data-index="${index}">${t('common.delete')}</button>
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
        btn.disabled = selectedAllowedModelIndices.size === 0;
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
    async function showModelSelectModal() {
      if (allChannels.length === 0) {
        await loadChannelsData();
      }
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
      const sourceModels = getAvailableModelsForCurrentChannelRestriction();
      let models = sourceModels.filter(m => !existingModels.has(m.toLowerCase()));

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
        const isEmptyCache = sourceModels.length === 0;
        const message = searchText
          ? t('tokens.noMatchingModel')
          : isEmptyCache
            ? t('tokens.channelNoModel')
            : t('tokens.allModelsAdded');
        container.innerHTML = `
          <div class="available-models-empty">
            ${message}
          </div>
        `;
        // 隐藏全选容器，恢复列表圆角
        if (selectAllContainer) selectAllContainer.style.display = 'none';
        container.classList.add('available-models-container--standalone');
        container.classList.remove('available-models-container--stacked');
        return;
      }

      // 显示全选容器，调整列表圆角
      if (selectAllContainer) {
        selectAllContainer.style.display = 'block';
      }
      container.classList.add('available-models-container--stacked');
      container.classList.remove('available-models-container--standalone');

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
        <label class="model-option-item" data-model="${escapeHtml(model)}">
          <input type="checkbox" class="model-option-checkbox" data-model="${escapeHtml(model)}"
            ${selectedModelsForAdd.has(model) ? 'checked' : ''}>
          <span class="model-option-label">${escapeHtml(model)}</span>
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
