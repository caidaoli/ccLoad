    const t = window.t;
    const API_BASE = '/admin';
    let allTokens = [];
    let isToday = true;      // 是否为本日（本日才显示最近一分钟）

    // 当前选中的时间范围(默认为本日)
    let currentTimeRange = 'today';
    let currentCustomTimeRange = null;
    // 列表客户端筛选：状态 all|active|inactive|expired + 描述/令牌关键字
    const tokenFilters = { status: 'all', search: '' };

    // 模型限制相关状态（2026-01新增）
    let editAllowedModels = [];              // 编辑模态框中当前的模型限制列表
    let allChannels = [];                    // 渠道数据缓存
    let availableModelsCache = [];           // 可用模型缓存
    let protocolDisplayNameMap = new Map(); // 协议显示名缓存
    let protocolDisplayNamesPromise = null; // 协议显示名加载中的 Promise
    let selectedModelsForAdd = new Set();    // 模型选择对话框中已选的模型
    let currentVisibleModels = [];            // 当前可见的模型列表（用于全选功能）
    let editAllowedChannelIDs = [];           // 编辑模态框中当前的渠道限制列表
    let editChannelRestrictionMode = 'allow'; // allow|deny
    let currentAllowedChannelFilter = '';
    let currentAllowedModelFilter = '';
    let selectedChannelsForAdd = new Set();   // 渠道选择对话框中已选的渠道ID
    let currentVisibleChannels = [];          // 当前可见的渠道列表（用于全选功能）
    let initialEditExpiryState = { type: 'never', value: '' };
    let releaseFormGuard = null;             // 创建/编辑对话框的未保存改动保护
    let editingTokenId = null;               // 编辑对话框当前令牌（语言切换时重绘状态徽标）

    // 对话框栈，用于 ESC 键层级关闭
    const modalStack = [];

    /** 注册全局 ESC 键处理 */
    document.addEventListener('keydown', (e) => {
      if (e.key !== 'Escape' || modalStack.length === 0 || e.defaultPrevented) return;
      // showConfirm 等原生 <dialog> 自行处理 Esc，不连带关闭底层对话框
      if (document.querySelector('dialog[open]')) return;
      // 有内容的搜索框：Esc 只清空关键字
      const target = e.target;
      if (target instanceof HTMLInputElement && target.type === 'search' && target.value) {
        target.value = '';
        target.dispatchEvent(new Event('input', { bubbles: true }));
        return;
      }
      modalStack[modalStack.length - 1].close();
    });

    /** 显示对话框并压栈 */
    function openModal(id, closeFunc) {
      document.getElementById(id).classList.add('show');
      modalStack.push({ id, close: closeFunc });
    }

    /** 隐藏对话框并出栈 */
    function hideModal(id) {
      document.getElementById(id).classList.remove('show');
      const index = modalStack.findIndex(entry => entry.id === id);
      if (index !== -1) modalStack.splice(index, 1);
    }

    /** 表单值快照不同于打开时即视为未保存 */
    function guardFormChanges(snapshot) {
      clearFormGuard();
      const initial = snapshot();
      releaseFormGuard = window.guardUnsavedChanges(() => snapshot() !== initial);
    }

    function clearFormGuard() {
      if (releaseFormGuard) releaseFormGuard();
      releaseFormGuard = null;
    }

    function snapshotFields(ids) {
      return ids.map((id) => {
        const el = document.getElementById(id);
        return el.type === 'checkbox' ? el.checked : el.value;
      });
    }

    const CREATE_FORM_FIELDS = ['tokenDescription', 'tokenExpiry', 'customExpiry', 'tokenDailyCostLimitUSD',
      'tokenMonthlyCostLimitUSD', 'tokenCostLimitUSD', 'tokenMaxConcurrency', 'tokenActive'];
    const EDIT_FORM_FIELDS = ['editTokenDescription', 'editTokenExpiry', 'editCustomExpiry', 'editDailyCostLimitUSD',
      'editMonthlyCostLimitUSD', 'editCostLimitUSD', 'editMaxConcurrency', 'editTokenActive'];

    function snapshotCreateForm() {
      return JSON.stringify(snapshotFields(CREATE_FORM_FIELDS));
    }

    function snapshotEditForm() {
      return JSON.stringify([
        snapshotFields(EDIT_FORM_FIELDS),
        editAllowedModels,
        editAllowedChannelIDs,
        editChannelRestrictionMode
      ]);
    }

    /** 标记字段校验失败并聚焦 */
    function rejectField(inputId, message) {
      const input = document.getElementById(inputId);
      if (input) {
        input.classList.add('is-invalid');
        input.setAttribute('aria-invalid', 'true');
        input.focus();
      }
      window.showNotification(message, 'error');
    }

    function clearInvalidFields(modalId) {
      document.querySelectorAll(`#${modalId} .is-invalid`).forEach((el) => {
        el.classList.remove('is-invalid');
        el.removeAttribute('aria-invalid');
      });
    }

    // 用户修改字段后撤销错误标记
    ['input', 'change'].forEach((type) => {
      document.addEventListener(type, (e) => {
        const el = e.target;
        if (el && el.classList && el.classList.contains('is-invalid')) {
          el.classList.remove('is-invalid');
          el.removeAttribute('aria-invalid');
        }
      });
    });

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

    function setCustomExpiryVisible(containerId, visible) {
      document.getElementById(containerId).hidden = !visible;
    }

    window.initPageBootstrap({
      topbarKey: 'tokens',
      run: () => {
      initExpirySelects();

      window.initSavedDateRangeFilter({
        selectId: 'tokensRange',
        defaultValue: currentTimeRange,
        values: ['today', 'yesterday', 'day_before_yesterday', 'this_week', 'this_month', 'last_month', 'custom', 'all'],
        includeAll: true,
        includeCustom: true,
        customRange: currentCustomTimeRange,
        customPickerContainerId: 'tokensRangeHost',
        onChange: (range, customRange) => {
          currentTimeRange = range;
          if (range === 'custom') currentCustomTimeRange = customRange;
          loadTokens();
        }
      });

      // 加载令牌列表(默认显示本日统计)
      loadTokens();

      // 预加载渠道数据（用于模型选择）
      loadChannelsData();
      ensureProtocolDisplayNameMap();

      initPageActionDelegation();

      // 初始化事件委托
      initEventDelegation();

      // 监听语言切换事件，重新渲染令牌相关动态内容
      window.i18n.onLocaleChange(() => {
        renderAllowedChannels();
        renderAllowedModels();
        renderEditQuotaUsage();
        renderEditStatusPill();
        renderTokens();
      });

      // 自动刷新（system_settings.auto_refresh_interval_seconds，0=禁用）
      if (typeof window.createAutoRefresh === 'function') {
        window.createAutoRefresh({ load: loadTokens }).init();
      }
      }
    });

    function initPageActionDelegation() {
      if (typeof window.initDelegatedActions !== 'function') return;

      window.initDelegatedActions({
        boundKey: 'tokensPageActionsBound',
        click: {
          'show-create-modal': () => showCreateModal(),
          'reload-tokens': () => loadTokens(),
          'clear-token-filters': () => clearTokenFilters(),
          'toggle-token-active': (actionTarget) => toggleTokenActive(actionTarget),
          'close-create-modal': () => closeCreateModal(),
          'create-token': () => createToken(),
          'close-token-result-modal': () => closeTokenResultModal(),
          'copy-token-result': () => copyToken(),
          'close-edit-modal': () => closeEditModal(),
          'update-token': () => updateToken(),
          'copy-edit-token': () => copyTokenToClipboard(document.getElementById('editTokenValue').value),
          'switch-restriction-tab': (actionTarget) => setRestrictionTab(actionTarget.dataset.tab),
          'set-channel-restriction-mode': (actionTarget) => {
            editChannelRestrictionMode = normalizeChannelRestrictionMode(actionTarget.dataset.mode);
            updateChannelRestrictionModeUI();
            renderAllowedChannels();
          },
          'clear-allowed-channels': () => {
            editAllowedChannelIDs = [];
            renderAllowedChannels();
          },
          'clear-allowed-models': () => {
            editAllowedModels = [];
            renderAllowedModels();
          },
          'show-model-select-modal': () => showModelSelectModal(),
          'show-model-import-modal': () => showModelImportModal(),
          'show-channel-select-modal': () => showChannelSelectModal(),
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
          'filter-tokens': () => applyTokenFilters(),
          'toggle-custom-expiry': (actionTarget) => {
            setCustomExpiryVisible('customExpiryContainer', actionTarget.value === 'custom');
          },
          'toggle-edit-custom-expiry': (actionTarget) => {
            setCustomExpiryVisible('editCustomExpiryContainer', actionTarget.value === 'custom');
          },
          'toggle-select-all-channels': (actionTarget) => toggleSelectAllChannels(actionTarget.checked),
          'toggle-select-all-models': (actionTarget) => toggleSelectAllModels(actionTarget.checked)
        },
        input: {
          'filter-tokens': () => applyTokenFilters(),
          'update-quota-usage': () => renderEditQuotaUsage(),
          'filter-available-channels': (actionTarget) => filterAvailableChannels(actionTarget.value),
          'filter-available-models': (actionTarget) => filterAvailableModels(actionTarget.value),
          'filter-allowed-channels': (actionTarget) => filterAllowedChannels(actionTarget.value),
          'filter-allowed-models': (actionTarget) => filterAllowedModels(actionTarget.value),
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
        const query = typeof window.buildDateRangeQuery === 'function'
          ? window.buildDateRangeQuery(currentTimeRange, currentCustomTimeRange)
          : `range=${encodeURIComponent(currentTimeRange)}`;
        const url = `${API_BASE}/auth-tokens?${query}`;

        const data = await fetchDataWithAuth(url);
        allTokens = (data && data.tokens) || [];
        isToday = !!(data && data.is_today);
        renderTokens();
      } catch (error) {
        
        console.error('Failed to load tokens:', error);
        window.showNotification(t('tokens.msg.loadFailed') + ': ' + error.message, 'error');
        if (allTokens.length === 0) renderLoadError(error);
      }
    }

    function renderLoadError(error) {
      document.getElementById('empty-state').style.display = 'none';
      const container = document.getElementById('tokens-container');
      container.hidden = false;
      container.innerHTML = `
        <div class="glass-card tokens-load-error" role="alert">
          <p>${escapeHtml(t('tokens.msg.loadFailed') + ': ' + (error?.message || ''))}</p>
          <button type="button" class="btn btn-secondary" data-action="reload-tokens">${escapeHtml(t('common.retry'))}</button>
        </div>
      `;
    }

    function getFilteredTokens() {
      const search = tokenFilters.search.trim().toLowerCase();
      return allTokens.filter((token) => {
        if (tokenFilters.status !== 'all' && getTokenStatus(token).class !== tokenFilters.status) return false;
        if (!search) return true;
        return String(token.description || '').toLowerCase().includes(search) ||
          String(token.token || '').toLowerCase().includes(search);
      });
    }

    function applyTokenFilters() {
      tokenFilters.status = document.getElementById('tokensStatusFilter').value || 'all';
      tokenFilters.search = document.getElementById('tokensSearchInput').value || '';
      renderTokens();
    }

    function clearTokenFilters() {
      const statusSelect = document.getElementById('tokensStatusFilter');
      statusSelect.value = 'all';
      statusSelect.dispatchEvent(new Event('change', { bubbles: true }));
      document.getElementById('tokensSearchInput').value = '';
      applyTokenFilters();
    }

    function renderTokens() {
      const container = document.getElementById('tokens-container');
      const emptyState = document.getElementById('empty-state');
      const tokens = getFilteredTokens();

      document.getElementById('tokensFilteredCount').textContent = tokens.length;
      document.getElementById('tokensTotalCount').textContent = allTokens.length;

      if (allTokens.length === 0) {
        container.innerHTML = '';
        container.hidden = true;
        emptyState.style.display = 'block';
        return;
      }

      container.hidden = false;
      emptyState.style.display = 'none';

      const table = document.createElement('table');
      table.className = 'modern-table mobile-card-table tokens-table';
      table.innerHTML = `
        <thead>
          <tr>
            <th>${t('tokens.table.token')}</th>
            <th class="tokens-col-num">${t('tokens.table.callsSuccess')}</th>
            <th class="tokens-col-num" title="${t('tokens.table.rpmTitle')}">${t('tokens.table.rpm')}</th>
            <th class="tokens-col-num">${t('tokens.table.tokenUsage')}</th>
            <th>${t('tokens.table.costLimit')}</th>
            <th class="tokens-col-num">${t('tokens.table.concurrency')}</th>
            <th class="tokens-col-num" title="${t('tokens.table.latencyTitle')}">${t('tokens.table.latency')}</th>
            <th>${t('tokens.table.lastUsed')}</th>
            <th class="tokens-col-actions">${t('tokens.table.actions')}</th>
          </tr>
        </thead>
      `;

      const tbody = document.createElement('tbody');
      if (tokens.length === 0) {
        tbody.innerHTML = `<tr class="tokens-no-match-row"><td colspan="9">${escapeHtml(t('tokens.noMatchingTokens'))}</td></tr>`;
      } else {
        tokens.forEach((token) => {
          const row = createTokenRow(token);
          if (row) tbody.appendChild(row);
        });
      }

      table.appendChild(tbody);
      container.innerHTML = '';
      container.appendChild(table);

      // 翻译动态渲染的内容中的 data-i18n 属性
      if (window.i18n.translatePage) {
        window.i18n.translatePage();
      }
    }

    function maskToken(value) {
      const token = String(value || '');
      return token.length > 8 ? token.substring(0, 4) + '****' + token.slice(-4) : token;
    }

    function createTokenRow(token) {
      const locale = window.i18n?.getLocale?.() || 'en';
      const status = getTokenStatus(token);
      const createdAt = new Date(token.created_at).toLocaleString(locale);
      const expiryText = token.expires_at
        ? t('tokens.expiresAt', { time: new Date(token.expires_at).toLocaleString(locale) })
        : t('tokens.expiryNever');
      const successCount = token.success_count || 0;
      const failureCount = token.failure_count || 0;
      const hasLatency = Boolean(token.stream_count || token.non_stream_count);

      return TemplateEngine.render('tpl-token-row', {
        id: token.id,
        description: token.description,
        token: token.token,
        maskedToken: maskToken(token.token),
        statusClass: status.class,
        statusText: status.text,
        expiryText,
        createdTitle: t('tokens.createdAt', { time: createdAt }),
        callsHtml: buildCallsHtml(successCount, failureCount),
        rpmHtml: buildRpmHtml(token),
        tokensHtml: buildTokensHtml(token),
        tokensCellClass: hasTokenUsage(token) ? '' : 'mobile-empty-cell',
        costHtml: buildCostHtml(token),
        concurrencyHtml: buildConcurrencyHtml(token.max_concurrency),
        latencyHtml: buildLatencyHtml(token),
        latencyCellClass: hasLatency ? '' : 'mobile-empty-cell',
        lastUsedHtml: formatLastUsedHtml(token.last_used_at, locale),
        toggleHtml: buildToggleHtml(token),
        mobileLabelCalls: t('tokens.table.callsSuccess'),
        mobileLabelRpm: t('tokens.table.rpm'),
        mobileLabelTokenUsage: t('tokens.table.tokenUsage'),
        mobileLabelCost: t('tokens.table.costLimit'),
        mobileLabelConcurrency: t('tokens.table.concurrency'),
        mobileLabelLatency: t('tokens.table.latency'),
        mobileLabelLastUsed: t('tokens.table.lastUsed')
      });
    }

    const MUTED_DASH_HTML = '<span class="token-value-muted">-</span>';

    function formatLastUsedHtml(value, locale) {
      if (!value) {
        return `<span class="token-value-muted">${t('tokens.neverUsed')}</span>`;
      }

      const usedAt = new Date(value);
      if (Number.isNaN(usedAt.getTime())) {
        return MUTED_DASH_HTML;
      }
      return `<span class="token-last-used">${usedAt.toLocaleString(locale, { hour12: false })}</span>`;
    }

    /**
     * 构建调用次数与成功率HTML（失败次数放在悬浮提示）
     */
    function buildCallsHtml(successCount, failureCount) {
      const totalCount = successCount + failureCount;
      if (totalCount === 0) {
        return MUTED_DASH_HTML;
      }

      const ratio = successCount / totalCount;
      let rateClass = 'success-rate-low';
      if (ratio >= 0.95) rateClass = 'success-rate-high';
      else if (ratio >= 0.8) rateClass = 'success-rate-medium';

      const title = `${t('tokens.successCall')}: ${successCount.toLocaleString()} · ${t('tokens.failedCall')}: ${failureCount.toLocaleString()}`;
      return `
        <div class="token-metric-stack" title="${escapeHtml(title)}">
          <span class="metric-value">${totalCount.toLocaleString()}</span>
          <span class="token-success-rate ${rateClass}">${window.formatPercent(ratio)}</span>
        </div>
      `;
    }

    /**
     * 构建RPM HTML（峰/均/近格式）
     */
    function buildRpmHtml(token) {
      const peakRPM = token.peak_rpm || 0;
      const avgRPM = token.avg_rpm || 0;
      const recentRPM = token.recent_rpm || 0;

      if (peakRPM < 0.01 && avgRPM < 0.01 && recentRPM < 0.01) {
        return MUTED_DASH_HTML;
      }

      const formatRpm = (rpm) => {
        if (rpm < 0.01) return '-';
        if (rpm >= 1000) return (rpm / 1000).toFixed(1) + 'K';
        if (rpm >= 1) return rpm.toFixed(1);
        return rpm.toFixed(2);
      };

      const peakText = formatRpm(peakRPM);
      const avgText = formatRpm(avgRPM);
      const recentText = isToday ? formatRpm(recentRPM) : '-';

      // 低流量绿色，中等橙色，高流量红色
      let rpmClass = 'token-rpm token-rpm--high';
      if (peakRPM < 10) rpmClass = 'token-rpm token-rpm--low';
      else if (peakRPM < 100) rpmClass = 'token-rpm token-rpm--medium';

      return `<span class="${rpmClass}">${peakText}/${avgText}/${recentText}</span>`;
    }

    function hasTokenUsage(token) {
      return token.prompt_tokens_total > 0 ||
        token.completion_tokens_total > 0 ||
        token.cache_read_tokens_total > 0 ||
        token.cache_creation_tokens_total > 0;
    }

    /**
     * 构建Token用量HTML
     */
    function buildTokensHtml(token) {
      if (!hasTokenUsage(token)) {
        return MUTED_DASH_HTML;
      }

      const items = [];
      const pushUsageItem = (variant, label, title, count) => {
        if (!count || count <= 0) return;
        items.push(
          `<span class="token-usage-item token-usage-item--${variant}" title="${title}">` +
            `<span class="token-usage-label">${label}</span>` +
            `<span class="token-usage-value">${window.formatNumber(count)}</span>` +
          `</span>`
        );
      };

      pushUsageItem('input', t('tokens.input'), t('tokens.inputTokens'), token.prompt_tokens_total || 0);
      pushUsageItem('output', t('tokens.output'), t('tokens.outputTokens'), token.completion_tokens_total || 0);
      pushUsageItem('cache-read', t('tokens.cacheRead'), t('tokens.cacheReadTokens'), token.cache_read_tokens_total || 0);
      pushUsageItem('cache-create', t('tokens.cacheCreate'), t('tokens.cacheCreateTokens'), token.cache_creation_tokens_total || 0);

      return `<div class="token-usage-metrics">${items.join('')}</div>`;
    }

    const COST_LIMIT_FIELDS = [
      { labelKey: 'tokens.limitShortDaily', limit: 'cost_daily_limit_usd', used: 'cost_daily_used_usd' },
      { labelKey: 'tokens.limitShortMonthly', limit: 'cost_monthly_limit_usd', used: 'cost_monthly_used_usd' },
      { labelKey: 'tokens.limitShortTotal', limit: 'cost_limit_usd', used: 'cost_used_usd' }
    ];

    function toNonNegativeNumber(value) {
      const num = Number(value);
      return Number.isFinite(num) && num > 0 ? num : 0;
    }

    /** 已设置的限额中消耗占比最高的一项；全部未设置时返回 null */
    function getTightestCostLimit(token) {
      let tightest = null;
      COST_LIMIT_FIELDS.forEach((field) => {
        const limit = toNonNegativeNumber(token[field.limit]);
        if (limit <= 0) return;
        const used = toNonNegativeNumber(token[field.used]);
        const ratio = used / limit;
        if (!tightest || ratio > tightest.ratio) {
          tightest = { labelKey: field.labelKey, limit, used, ratio };
        }
      });
      return tightest;
    }

    function buildProgressHtml(ratio) {
      let tone = '';
      if (ratio >= 1) tone = ' token-progress--danger';
      else if (ratio >= 0.8) tone = ' token-progress--warn';
      const percent = Math.min(100, Math.max(0, ratio * 100));
      return `<div class="token-progress${tone}"><span class="token-progress__bar" style="width: ${percent.toFixed(1)}%;"></span></div>`;
    }

    /**
     * 构建费用与最紧限额HTML
     */
    function buildCostHtml(token) {
      const totalCost = Number(token.total_cost_usd) || 0;
      const costHtml = totalCost > 0
        ? `<div class="token-cost">${buildCostStackHtml(totalCost, token.effective_cost_usd, { tone: 'warning' })}</div>`
        : MUTED_DASH_HTML;

      const limit = getTightestCostLimit(token);
      if (!limit) {
        return `${costHtml}<div class="token-limit token-limit--none">${t('tokens.noCostLimit')}</div>`;
      }
      return `
        ${costHtml}
        <div class="token-limit">
          <div class="token-limit__text">
            <span class="token-limit__label">${t(limit.labelKey)}</span>
            <span>${window.formatCost(limit.used, 2)} / ${window.formatCost(limit.limit, 2)}</span>
          </div>
          ${buildProgressHtml(limit.ratio)}
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

    function buildToggleHtml(token) {
      if (token.is_expired) return '';
      const on = Boolean(token.is_active);
      const title = t(on ? 'tokens.disableTokenTitle' : 'tokens.enableTokenTitle');
      return `
        <button type="button" class="channel-enable-switch ${on ? 'channel-enable-switch--on' : 'channel-enable-switch--off'}"
          role="switch" aria-checked="${on}" data-action="toggle-token-active" data-token-id="${token.id}"
          title="${escapeHtml(title)}" aria-label="${escapeHtml(title)}">
          <span class="channel-enable-switch__knob" aria-hidden="true"></span>
        </button>
        <span class="token-actions-divider" aria-hidden="true"></span>
      `;
    }

    function parseMaxConcurrencyInput(rawValue) {
      const normalized = String(rawValue ?? '').trim();
      if (normalized === '') {
        return { value: 0 };
      }

      const parsed = Number(normalized);
      if (!Number.isFinite(parsed) || !Number.isInteger(parsed) || parsed < 0) {
        return { error: t('tokens.msg.maxConcurrencyInteger') };
      }

      return { value: parsed };
    }

    // 编辑对话框中的限额输入及其已消耗展示
    const EDIT_QUOTA_FIELDS = [
      { input: 'editDailyCostLimitUSD', used: 'editDailyCostUsedDisplay', limitKey: 'cost_daily_limit_usd', usedKey: 'cost_daily_used_usd' },
      { input: 'editMonthlyCostLimitUSD', used: 'editMonthlyCostUsedDisplay', limitKey: 'cost_monthly_limit_usd', usedKey: 'cost_monthly_used_usd' },
      { input: 'editCostLimitUSD', used: 'editCostUsedDisplay', limitKey: 'cost_limit_usd', usedKey: 'cost_used_usd' }
    ];

    function fillEditQuotaFields(token) {
      EDIT_QUOTA_FIELDS.forEach((field) => {
        const limit = toNonNegativeNumber(token[field.limitKey]);
        document.getElementById(field.input).value = limit > 0 ? limit : '';
        document.getElementById(field.used).dataset.used = String(toNonNegativeNumber(token[field.usedKey]));
      });
      renderEditQuotaUsage();
    }

    /** 按当前输入的上限实时重算已消耗进度 */
    function renderEditQuotaUsage() {
      EDIT_QUOTA_FIELDS.forEach((field) => {
        const usedDisplay = document.getElementById(field.used);
        if (!usedDisplay || usedDisplay.dataset.used === undefined) return;
        const used = Number(usedDisplay.dataset.used) || 0;
        const limit = toNonNegativeNumber(document.getElementById(field.input).value);
        const usedText = window.formatCost(used, 4);
        usedDisplay.innerHTML = limit > 0
          ? `<span class="token-quota-used__text">${usedText}</span>${buildProgressHtml(used / limit)}`
          : `<span class="token-quota-used__text">${usedText}</span>`;
      });
    }

    function buildTimingValueHtml(time, count, colorFn) {
      const num = Number(time);
      if (!count || !Number.isFinite(num) || num <= 0) {
        return '<span class="token-value-muted">-</span>';
      }
      return `<span class="metric-value" style="color: ${colorFn(num)};">${num.toFixed(2)}s</span>`;
    }

    /**
     * 构建首字/非流耗时HTML
     */
    function buildLatencyHtml(token) {
      if (!token.stream_count && !token.non_stream_count) {
        return MUTED_DASH_HTML;
      }
      const ttfb = buildTimingValueHtml(token.stream_avg_ttfb, token.stream_count, window.getFirstByteTimingColor);
      const nonStream = buildTimingValueHtml(token.non_stream_avg_rt, token.non_stream_count, window.getDurationTimingColor);
      return `<span class="token-latency">${ttfb}<span class="token-latency__sep">/</span>${nonStream}</span>`;
    }

    async function toggleTokenActive(actionTarget) {
      const id = Number(actionTarget.dataset.tokenId);
      const token = allTokens.find(item => item.id === id);
      if (!token || token.is_expired) return;

      const nextActive = !token.is_active;
      actionTarget.disabled = true;
      try {
        await fetchDataWithAuth(`${API_BASE}/auth-tokens/${id}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ is_active: nextActive })
        });
        token.is_active = nextActive;
        renderTokens();
        window.showNotification(t(nextActive ? 'tokens.msg.enabled' : 'tokens.msg.disabled'), 'success');
      } catch (error) {
        actionTarget.disabled = false;
        console.error('Failed to toggle token:', error);
        window.showNotification(t('tokens.msg.toggleFailed') + ': ' + error.message, 'error');
      }
    }


    function getTokenStatus(token) {
      
      if (token.is_expired) return { class: 'expired', text: t('tokens.status.expired') };
      if (!token.is_active) return { class: 'inactive', text: t('tokens.status.inactive') };
      return { class: 'active', text: t('tokens.status.active') };
    }

    function showCreateModal() {
      document.getElementById('tokenDescription').value = '';
      document.getElementById('tokenExpiry').value = 'never';
      ['tokenDailyCostLimitUSD', 'tokenMonthlyCostLimitUSD', 'tokenCostLimitUSD', 'tokenMaxConcurrency'].forEach((id) => {
        document.getElementById(id).value = '';
      });
      document.getElementById('tokenActive').checked = true;
      document.getElementById('customExpiry').value = '';
      setCustomExpiryVisible('customExpiryContainer', false);
      clearInvalidFields('createModal');
      openModal('createModal', closeCreateModal);
      guardFormChanges(snapshotCreateForm);
      document.getElementById('tokenDescription').focus();
    }

    function closeCreateModal() {
      hideModal('createModal');
      clearFormGuard();
    }

    /**
     * 读取并校验限额字段；失败时返回出错字段 ID 与提示
     */
    function readLimitFields(ids) {
      const daily = parseFloat(document.getElementById(ids.daily).value) || 0;
      const monthly = parseFloat(document.getElementById(ids.monthly).value) || 0;
      const total = parseFloat(document.getElementById(ids.total).value) || 0;
      const negative = [[ids.daily, daily], [ids.monthly, monthly], [ids.total, total]].find(([, value]) => value < 0);
      if (negative) {
        return { field: negative[0], error: t('tokens.msg.costLimitNegative') };
      }
      const maxConcurrencyResult = parseMaxConcurrencyInput(document.getElementById(ids.concurrency).value);
      if (maxConcurrencyResult.error) {
        return { field: ids.concurrency, error: maxConcurrencyResult.error };
      }
      const maxConcurrency = maxConcurrencyResult.value;
      if ((daily > 0 || monthly > 0 || total > 0) && maxConcurrency <= 0) {
        return { field: ids.concurrency, error: t('tokens.msg.costLimitRequiresConcurrency') };
      }
      return { daily, monthly, total, maxConcurrency };
    }

    async function createToken() {
      clearInvalidFields('createModal');
      const description = document.getElementById('tokenDescription').value.trim();
      if (!description) {
        rejectField('tokenDescription', t('tokens.msg.enterDescription'));
        return;
      }
      const expiryType = document.getElementById('tokenExpiry').value;
      let expiresAt = null;
      if (expiryType !== 'never') {
        if (expiryType === 'custom') {
          const customDate = document.getElementById('customExpiry').value;
          if (!customDate) {
            rejectField('customExpiry', t('tokens.msg.selectExpiry'));
            return;
          }
          expiresAt = new Date(customDate).getTime();
        } else {
          const days = parseInt(expiryType);
          expiresAt = Date.now() + days * 24 * 60 * 60 * 1000;
        }
      }
      const isActive = document.getElementById('tokenActive').checked;
      const limits = readLimitFields({
        daily: 'tokenDailyCostLimitUSD',
        monthly: 'tokenMonthlyCostLimitUSD',
        total: 'tokenCostLimitUSD',
        concurrency: 'tokenMaxConcurrency'
      });
      if (limits.error) {
        rejectField(limits.field, limits.error);
        return;
      }
      try {
        const data = await fetchDataWithAuth(`${API_BASE}/auth-tokens`, {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json'
          },
          body: JSON.stringify({
            description,
            expires_at: expiresAt,
            is_active: isActive,
            cost_daily_limit_usd: limits.daily,
            cost_monthly_limit_usd: limits.monthly,
            cost_limit_usd: limits.total,
            max_concurrency: limits.maxConcurrency
          })
        });

        closeCreateModal();
        document.getElementById('newTokenValue').value = data.token;
        openModal('tokenResultModal', closeTokenResultModal);
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
      hideModal('tokenResultModal');
      document.getElementById('newTokenValue').value = '';
    }

    function editToken(id) {
      const token = allTokens.find(t => t.id === id);
      if (!token) return;
      editingTokenId = id;
      document.getElementById('editTokenId').value = id;
      document.getElementById('editTokenValue').value = token.token || '';
      document.getElementById('editTokenDescription').value = token.description;
      document.getElementById('editTokenActive').checked = token.is_active;
      renderEditStatusPill();
      const expiryTypeInput = document.getElementById('editTokenExpiry');
      const customExpiryInput = document.getElementById('editCustomExpiry');
      if (!token.expires_at) {
        expiryTypeInput.value = 'never';
        customExpiryInput.value = '';
      } else {
        expiryTypeInput.value = 'custom';
        customExpiryInput.value = TokenExpiry.formatDateTimeLocal(token.expires_at);
      }
      setCustomExpiryVisible('editCustomExpiryContainer', expiryTypeInput.value === 'custom');
      initialEditExpiryState = { type: expiryTypeInput.value, value: customExpiryInput.value };

      fillEditQuotaFields(token);
      document.getElementById('editMaxConcurrency').value = token.max_concurrency || '';

      // 初始化模型限制状态（2026-01新增）
      editAllowedModels = (token.allowed_models || []).slice();
      currentAllowedModelFilter = '';
      document.getElementById('allowedModelFilterInput').value = '';
      renderAllowedModels();

      // 初始化渠道限制状态（2026-04新增）
      editAllowedChannelIDs = (token.allowed_channel_ids || []).slice();
      editChannelRestrictionMode = normalizeChannelRestrictionMode(token.channel_restriction_mode);
      currentAllowedChannelFilter = '';
      document.getElementById('allowedChannelFilterInput').value = '';
      updateChannelRestrictionModeUI();
      renderAllowedChannels();
      if (allChannels.length === 0) {
        loadChannelsData().then(() => renderAllowedChannels());
      }
      setRestrictionTab('channels');

      clearInvalidFields('editModal');
      openModal('editModal', closeEditModal);
      guardFormChanges(snapshotEditForm);
    }

    function renderEditStatusPill() {
      const pill = document.getElementById('editTokenStatusPill');
      const token = allTokens.find(item => item.id === editingTokenId);
      if (!pill || !token) return;
      const status = getTokenStatus(token);
      pill.className = `token-status-pill token-status-pill--${status.class}`;
      pill.textContent = status.text;
    }

    function setRestrictionTab(tab) {
      const active = tab === 'models' ? 'models' : 'channels';
      [['channels', 'tokenRestrictionTabChannels', 'tokenRestrictionPaneChannels'],
        ['models', 'tokenRestrictionTabModels', 'tokenRestrictionPaneModels']].forEach(([name, tabId, paneId]) => {
        const selected = name === active;
        const tabEl = document.getElementById(tabId);
        tabEl.classList.toggle('active', selected);
        tabEl.setAttribute('aria-selected', String(selected));
        document.getElementById(paneId).classList.toggle('active', selected);
      });
    }

    function closeEditModal() {
      hideModal('editModal');
      clearFormGuard();
      editingTokenId = null;
      document.getElementById('editTokenValue').value = '';
      document.getElementById('editCustomExpiry').value = '';
      setCustomExpiryVisible('editCustomExpiryContainer', false);
      initialEditExpiryState = { type: 'never', value: '' };
      // 清理模型/渠道限制状态
      editAllowedModels = [];
      currentAllowedModelFilter = '';
      editAllowedChannelIDs = [];
      editChannelRestrictionMode = 'allow';
      currentAllowedChannelFilter = '';
      updateChannelRestrictionModeUI();
    }


    async function updateToken() {
      clearInvalidFields('editModal');
      const id = document.getElementById('editTokenId').value;
      const description = document.getElementById('editTokenDescription').value.trim();
      const isActive = document.getElementById('editTokenActive').checked;
      const expiryType = document.getElementById('editTokenExpiry').value;
      const limits = readLimitFields({
        daily: 'editDailyCostLimitUSD',
        monthly: 'editMonthlyCostLimitUSD',
        total: 'editCostLimitUSD',
        concurrency: 'editMaxConcurrency'
      });
      if (limits.error) {
        rejectField(limits.field, limits.error);
        return;
      }
      let customDate = '';
      let expiresAt = null;
      if (expiryType !== 'never') {
        if (expiryType === 'custom') {
          customDate = document.getElementById('editCustomExpiry').value;
          if (!customDate) {
            rejectField('editCustomExpiry', t('tokens.msg.selectExpiry'));
            return;
          }
          expiresAt = new Date(customDate).getTime();
        } else {
          const days = parseInt(expiryType);
          expiresAt = Date.now() + days * 24 * 60 * 60 * 1000;
        }
      }
      const expiryUpdate = TokenExpiry.buildUpdatePayload(
        initialEditExpiryState,
        { type: expiryType, value: customDate },
        expiresAt
      );
      try {
        await fetchDataWithAuth(`${API_BASE}/auth-tokens/${id}`, {
          method: 'PUT',
          headers: {
            'Content-Type': 'application/json'
          },
          body: JSON.stringify({
            description,
            is_active: isActive,
            ...expiryUpdate,
            allowed_channel_ids: editAllowedChannelIDs,
            channel_restriction_mode: normalizeChannelRestrictionMode(editChannelRestrictionMode),
            allowed_models: editAllowedModels,  // 2026-01新增：模型限制
            cost_daily_limit_usd: limits.daily,
            cost_monthly_limit_usd: limits.monthly,
            cost_limit_usd: limits.total,               // 总限额
            max_concurrency: limits.maxConcurrency      // 2026-04新增：并发上限
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
      const token = allTokens.find(item => item.id === id);
      const confirmed = await window.showConfirm({
        title: t('tokens.deleteConfirmTitle'),
        message: t('tokens.msg.deleteConfirm'),
        detail: token ? token.description : '',
        confirmText: t('common.delete'),
        danger: true
      });
      if (!confirmed) return;
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

    function normalizeChannelRestrictionMode(mode) {
      return String(mode || '').toLowerCase() === 'deny' ? 'deny' : 'allow';
    }

    function updateChannelRestrictionModeUI() {
      document.querySelectorAll('[data-action="set-channel-restriction-mode"]').forEach((btn) => {
        const selected = btn.dataset.mode === editChannelRestrictionMode;
        btn.classList.toggle('active', selected);
        btn.setAttribute('aria-pressed', String(selected));
      });
    }


    function getAvailableModelsForCurrentChannelRestriction() {
      if (editAllowedChannelIDs.length === 0) {
        return availableModelsCache;
      }

      const restrictedChannelIDs = new Set(editAllowedChannelIDs);
      const deny = editChannelRestrictionMode === 'deny';
      const modelSet = new Set();
      allChannels.forEach(ch => {
        const id = normalizeChannelID(ch.id);
        const inList = restrictedChannelIDs.has(id);
        if (deny) {
          if (inList) return;
        } else if (!inList) {
          return;
        }
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
      return channel?.name || t('common.unknown');
    }

    function normalizeRestrictionFilter(value) {
      return String(value || '').trim().toLowerCase();
    }

    function getVisibleAllowedChannelIDs() {
      const filter = normalizeRestrictionFilter(currentAllowedChannelFilter);
      if (!filter) return editAllowedChannelIDs;
      return editAllowedChannelIDs.filter((channelID) =>
        getChannelDisplayName(channelID).toLowerCase().includes(filter)
      );
    }

    function getVisibleAllowedModelEntries() {
      const entries = editAllowedModels.map((model, index) => ({ model, index }));
      const filter = normalizeRestrictionFilter(currentAllowedModelFilter);
      if (!filter) return entries;
      return entries.filter(({ model }) => String(model).toLowerCase().includes(filter));
    }

    function filterAllowedChannels(searchText) {
      currentAllowedChannelFilter = searchText;
      renderAllowedChannels();
    }

    function filterAllowedModels(searchText) {
      currentAllowedModelFilter = searchText;
      renderAllowedModels();
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

    function buildChipHtml({ label, meta, removeAttrs, deny }) {
      const removeLabel = escapeHtml(t('tokens.removeItem', { name: label }));
      return `
        <span class="token-chip${deny ? ' token-chip--deny' : ''}">
          <span class="token-chip__label" title="${escapeHtml(label)}">${escapeHtml(label)}</span>
          ${meta ? `<span class="token-chip__meta">${escapeHtml(meta)}</span>` : ''}
          <button type="button" class="token-chip__remove" ${removeAttrs} title="${removeLabel}" aria-label="${removeLabel}">&times;</button>
        </span>
      `;
    }

    function buildRestrictionEmptyHtml(message, actionsHtml) {
      return `
        <div class="token-restriction-empty">
          <p>${escapeHtml(message)}</p>
          <div class="token-restriction-empty__actions">${actionsHtml}</div>
        </div>
      `;
    }

    /** 同步限制面板的计数、提示与工具栏可用状态 */
    function updateRestrictionPane({ countId, hintId, clearId, filterId, count, hint }) {
      document.getElementById(countId).textContent = count;
      const hintEl = document.getElementById(hintId);
      hintEl.textContent = hint;
      hintEl.hidden = count === 0;
      document.getElementById(clearId).disabled = count === 0;
      document.getElementById(filterId).disabled = count === 0;
    }

    function renderAllowedChannels() {
      const list = document.getElementById('allowedChannelsList');
      if (!list) return;

      const deny = editChannelRestrictionMode === 'deny';
      const count = editAllowedChannelIDs.length;
      updateRestrictionPane({
        countId: 'editAllowedChannelsCount',
        hintId: 'allowedChannelsHint',
        clearId: 'clearAllowedChannelsBtn',
        filterId: 'allowedChannelFilterInput',
        count,
        hint: t(deny ? 'tokens.channelHintDeny' : 'tokens.channelHintAllow', { count })
      });

      if (count === 0) {
        list.innerHTML = buildRestrictionEmptyHtml(
          t(deny ? 'tokens.channelEmptyDeny' : 'tokens.channelEmptyAllow'),
          `<button type="button" class="btn btn-primary btn-sm" data-action="show-channel-select-modal">${escapeHtml(t('tokens.selectChannel'))}</button>`
        );
        return;
      }

      const visibleChannelIDs = getVisibleAllowedChannelIDs();
      if (visibleChannelIDs.length === 0) {
        list.innerHTML = `<div class="token-chip-list__empty">${escapeHtml(t('tokens.noMatchingChannel'))}</div>`;
        return;
      }

      list.innerHTML = visibleChannelIDs.map((channelID) => buildChipHtml({
        label: getChannelDisplayName(channelID),
        meta: `#${channelID}`,
        removeAttrs: `data-action="remove-allowed-channel" data-channel-id="${channelID}"`,
        deny
      })).join('');
    }

    function removeAllowedChannel(channelID) {
      editAllowedChannelIDs = editAllowedChannelIDs.filter(id => id !== channelID);
      renderAllowedChannels();
    }


    async function showChannelSelectModal() {
      if (allChannels.length === 0) {
        await loadChannelsData();
      }
      await ensureProtocolDisplayNameMap();
      selectedChannelsForAdd.clear();
      document.getElementById('channelSearchInput').value = '';
      renderAvailableChannels('');
      openModal('channelSelectModal', closeChannelSelectModal);
    }

    function closeChannelSelectModal() {
      hideModal('channelSelectModal');
      selectedChannelsForAdd.clear();
    }

    function filterAvailableChannels(searchText) {
      renderAvailableChannels(searchText);
    }

    function normalizeProtocolValue(value) {
      return String(value || '').trim().toLowerCase();
    }

    function buildProtocolDisplayNameMap(protocols) {
      const map = new Map();
      (Array.isArray(protocols) ? protocols : []).forEach((protocol) => {
        const protocolKey = normalizeProtocolValue(protocol && protocol.value);
        const displayName = String(protocol && protocol.display_name || '').trim();
        if (!displayName) return;
        map.set(protocolKey, displayName);
      });
      return map;
    }

    async function ensureProtocolDisplayNameMap() {
      if (protocolDisplayNameMap.size > 0) {
        return protocolDisplayNameMap;
      }
      if (protocolDisplayNamesPromise) {
        return protocolDisplayNamesPromise;
      }

      protocolDisplayNamesPromise = (async () => {
        try {
          if (window.ProtocolManager && typeof window.ProtocolManager.getProtocols === 'function') {
            const protocols = await window.ProtocolManager.getProtocols();
            protocolDisplayNameMap = buildProtocolDisplayNameMap(protocols);
          }
        } catch (error) {
          console.error('Failed to load protocol display names:', error);
        } finally {
          protocolDisplayNamesPromise = null;
        }
        return protocolDisplayNameMap;
      })();

      return protocolDisplayNamesPromise;
    }

    function getChannelProtocols(channel) {
      return Array.from(new Set(
        (Array.isArray(channel?.urls) ? channel.urls : [])
          .flatMap(entry => Array.isArray(entry?.protocols) ? entry.protocols : [])
          .map(normalizeProtocolValue)
          .filter(Boolean)
      ));
    }

    function getProtocolLabel(protocol) {
      const normalizedProtocol = normalizeProtocolValue(protocol);
      return protocolDisplayNameMap.get(normalizedProtocol) || normalizedProtocol;
    }

    function matchesChannelSearchText(channel, searchText) {
      const search = String(searchText || '').trim().toLowerCase();
      if (!search) return true;

      const protocols = getChannelProtocols(channel);
      const protocolText = protocols.map(getProtocolLabel).join(' ').toLowerCase();
      const name = String(channel?.name || '').toLowerCase();
      const id = String(channel?.id || '');

      return name.includes(search) ||
        protocols.some(protocol => protocol.includes(search)) ||
        protocolText.includes(search) ||
        id.includes(search);
    }

    function renderAvailableChannels(searchText) {
      const container = document.getElementById('availableChannelsContainer');
      const countSpan = document.getElementById('selectedChannelsCount');
      const selectAllContainer = document.getElementById('selectAllChannelsContainer');
      const selectAllCheckbox = document.getElementById('selectAllChannelsCheckbox');
      const visibleChannelsCount = document.getElementById('visibleChannelsCount');
      if (!container) return;

      const existingChannelIDs = new Set(editAllowedChannelIDs);
      const availableChannels = allChannels.filter(ch => !existingChannelIDs.has(normalizeChannelID(ch.id)));
      let channels = availableChannels;

      if (searchText) {
        channels = channels.filter(ch => matchesChannelSearchText(ch, searchText));
      }

      currentVisibleChannels = channels;
      if (countSpan) countSpan.textContent = selectedChannelsForAdd.size;

      if (channels.length === 0) {
        const hasFilter = Boolean(searchText);
        const message = hasFilter
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

      container.innerHTML = `<div class="channel-option-list">${channels.map(ch => {
        const channelID = normalizeChannelID(ch.id);
        const protocols = getChannelProtocols(ch);
        const protocolText = protocols.length > 0
          ? protocols.map(getProtocolLabel).join(', ')
          : t('channels.urlProtocolAuto');
        return `
          <label class="channel-option-item" data-channel-id="${channelID}">
            <input type="checkbox" class="channel-option-checkbox" data-channel-id="${channelID}"
              ${selectedChannelsForAdd.has(channelID) ? 'checked' : ''}>
            <span class="channel-option-label">${escapeHtml(ch.name || t('common.unknown'))}</span>
            <span class="channel-option-meta">#${channelID} · ${escapeHtml(protocolText)}</span>
          </label>
        `;
      }).join('')}</div>`;

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

      const addedCount = selectedChannelsForAdd.size;
      const existingChannelIDs = new Set(editAllowedChannelIDs);
      selectedChannelsForAdd.forEach(channelID => {
        if (!existingChannelIDs.has(channelID)) {
          editAllowedChannelIDs.push(channelID);
        }
      });

      sortAllowedChannelIDs();
      closeChannelSelectModal();
      renderAllowedChannels();
      window.showNotification(t('tokens.msg.channelsAdded', { count: addedCount }), 'success');
    }

    /**
     * 渲染模型限制列表
     */
    function renderAllowedModels() {
      const list = document.getElementById('allowedModelsList');
      if (!list) return;

      const count = editAllowedModels.length;
      updateRestrictionPane({
        countId: 'editAllowedModelsCount',
        hintId: 'allowedModelsHint',
        clearId: 'clearAllowedModelsBtn',
        filterId: 'allowedModelFilterInput',
        count,
        hint: t('tokens.modelHint', { count })
      });

      if (count === 0) {
        list.innerHTML = buildRestrictionEmptyHtml(
          t('tokens.modelEmpty'),
          `<button type="button" class="btn btn-secondary btn-sm" data-action="show-model-import-modal">${escapeHtml(t('tokens.manualInput'))}</button>` +
          `<button type="button" class="btn btn-primary btn-sm" data-action="show-model-select-modal">${escapeHtml(t('tokens.selectFromList'))}</button>`
        );
        return;
      }

      const visibleModelEntries = getVisibleAllowedModelEntries();
      if (visibleModelEntries.length === 0) {
        list.innerHTML = `<div class="token-chip-list__empty">${escapeHtml(t('tokens.noMatchingModel'))}</div>`;
        return;
      }

      list.innerHTML = visibleModelEntries.map(({ model, index }) => buildChipHtml({
        label: model,
        removeAttrs: `data-action="remove-allowed-model" data-index="${index}"`
      })).join('');
    }

    /**
     * 删除单个模型
     */
    function removeAllowedModel(index) {
      editAllowedModels.splice(index, 1);
      renderAllowedModels();
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
      openModal('modelSelectModal', closeModelSelectModal);
    }

    /**
     * 关闭模型选择对话框
     */
    function closeModelSelectModal() {
      hideModal('modelSelectModal');
      selectedModelsForAdd.clear();
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

      const addedCount = selectedModelsForAdd.size;
      // 添加到模型限制列表
      selectedModelsForAdd.forEach(model => {
        if (!editAllowedModels.includes(model)) {
          editAllowedModels.push(model);
        }
      });

      // 排序
      editAllowedModels.sort();

      closeModelSelectModal();
      renderAllowedModels();
      window.showNotification(t('tokens.msg.modelsAdded', { count: addedCount }), 'success');
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
      openModal('modelImportModal', closeModelImportModal);
      setTimeout(() => document.getElementById('tokenModelImportTextarea').focus(), 100);
    }

    /**
     * 关闭模型导入对话框
     */
    function closeModelImportModal() {
      hideModal('modelImportModal');
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
      renderAllowedModels();

      const duplicateCount = models.length - newModels.length;
      const msg = duplicateCount > 0
        ? t('tokens.msg.importSuccessWithDuplicates', { added: newModels.length, duplicates: duplicateCount })
        : t('tokens.msg.importSuccess', { count: newModels.length });
      window.showNotification(msg, 'success');
    }
