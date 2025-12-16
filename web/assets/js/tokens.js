    const API_BASE = '/admin';
    let allTokens = [];
    let isToday = true;      // 是否为本日（本日才显示最近一分钟）

    // 当前选中的时间范围(默认为本日)
    let currentTimeRange = 'today';

    document.addEventListener('DOMContentLoaded', () => {
      // 初始化时间范围选择器
      initTimeRangeSelector();

      // 加载令牌列表(默认显示本日统计)
      loadTokens();

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
        console.error('加载令牌失败:', error);
        window.showNotification('加载令牌失败: ' + error.message, 'error');
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
            <th>描述</th>
            <th>令牌</th>
            <th style="text-align: center;">调用次数</th>
            <th style="text-align: center;">成功率</th>
            <th style="text-align: center;" title="每分钟请求数(峰值/平均/最近)">RPM(峰/均/近)</th>
            <th style="text-align: center;">Token用量</th>
            <th style="text-align: center;">总费用</th>
            <th style="text-align: center;">流首字平均</th>
            <th style="text-align: center;">非流平均</th>
            <th>最后使用</th>
            <th style="width: 200px;">操作</th>
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
      const status = getTokenStatus(token);
      const createdAt = new Date(token.created_at).toLocaleString('zh-CN');
      const lastUsed = token.last_used_at ? new Date(token.last_used_at).toLocaleString('zh-CN') : '从未使用';
      const expiresAt = token.expires_at ? new Date(token.expires_at).toLocaleString('zh-CN') : '永不过期';

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
      return TemplateEngine.render('tpl-token-row', {
        id: token.id,
        description: token.description,
        token: token.token,
        statusClass: status.class,
        createdAt: createdAt,
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
      html += `<span class="stats-badge" style="background: var(--success-50); color: var(--success-700); font-weight: 600; border: 1px solid var(--success-200);" title="成功调用">`;
      html += `<span style="color: var(--success-600); font-size: 14px; font-weight: 700;">✓</span> ${successCount.toLocaleString()}`;
      html += `</span>`;

      if (failureCount > 0) {
        html += `<span class="stats-badge" style="background: var(--error-50); color: var(--error-700); font-weight: 600; border: 1px solid var(--error-200);" title="失败调用">`;
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
      html += `<span class="stats-badge" style="background: var(--primary-50); color: var(--primary-700);" title="输入Tokens">`;
      html += `输入 ${formatTokenCount(token.prompt_tokens_total || 0)}`;
      html += `</span>`;
      html += `<span class="stats-badge" style="background: var(--secondary-50); color: var(--secondary-700);" title="输出Tokens">`;
      html += `输出 ${formatTokenCount(token.completion_tokens_total || 0)}`;
      html += `</span>`;
      html += '</div>';

      // 缓存
      if (token.cache_read_tokens_total > 0 || token.cache_creation_tokens_total > 0) {
        html += '<div style="display: inline-flex; gap: 4px; font-size: 12px;">';

        if (token.cache_read_tokens_total > 0) {
          html += `<span class="stats-badge" style="background: var(--success-50); color: var(--success-700);" title="缓存读Tokens">`;
          html += `缓存读 ${formatTokenCount(token.cache_read_tokens_total || 0)}`;
          html += `</span>`;
        }

        if (token.cache_creation_tokens_total > 0) {
          html += `<span class="stats-badge" style="background: var(--warning-50); color: var(--warning-700);" title="缓存建Tokens">`;
          html += `缓存建 ${formatTokenCount(token.cache_creation_tokens_total || 0)}`;
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
      const status = getTokenStatus(token);
      const createdAt = new Date(token.created_at).toLocaleString('zh-CN');
      const lastUsed = token.last_used_at ? new Date(token.last_used_at).toLocaleString('zh-CN') : '从未使用';
      const expiresAt = token.expires_at ? new Date(token.expires_at).toLocaleString('zh-CN') : '永不过期';

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

      return `
        <tr data-token-id="${token.id}">
          <td style="font-weight: 500;">${escapeHtml(token.description)}</td>
          <td>
            <div><span class="token-display token-display-${status.class}">${escapeHtml(token.token)}</span></div>
            <div style="font-size: 12px; color: var(--neutral-500); margin-top: 4px;">${createdAt}创建 · ${expiresAt}</div>
          </td>
          <td style="text-align: center;">${callsHtml}</td>
          <td style="text-align: center;">${successRateHtml}</td>
          <td style="text-align: center;">${rpmHtml}</td>
          <td style="text-align: center;">${tokensHtml}</td>
          <td style="text-align: center;">${costHtml}</td>
          <td style="text-align: center;">${streamAvgHtml}</td>
          <td style="text-align: center;">${nonStreamAvgHtml}</td>
          <td style="color: var(--neutral-600);">${lastUsed}</td>
          <td>
            <button class="btn btn-secondary btn-edit" style="padding: 4px 12px; font-size: 13px; margin-right: 4px;">编辑</button>
            <button class="btn btn-danger btn-delete" style="padding: 4px 12px; font-size: 13px;">删除</button>
          </td>
        </tr>
      `;
    }

    function getTokenStatus(token) {
      if (token.is_expired) return { class: 'expired', text: '已过期' };
      if (!token.is_active) return { class: 'inactive', text: '未启用' };
      return { class: 'active', text: '正常' };
    }

    function showCreateModal() {
      document.getElementById('tokenDescription').value = '';
      document.getElementById('tokenExpiry').value = 'never';
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
        window.showNotification('请输入描述', 'error');
        return;
      }
      const expiryType = document.getElementById('tokenExpiry').value;
      let expiresAt = null;
      if (expiryType !== 'never') {
        if (expiryType === 'custom') {
          const customDate = document.getElementById('customExpiry').value;
          if (!customDate) {
            window.showNotification('请选择过期时间', 'error');
            return;
          }
          expiresAt = new Date(customDate).getTime();
        } else {
          const days = parseInt(expiryType);
          expiresAt = Date.now() + days * 24 * 60 * 60 * 1000;
        }
      }
      const isActive = document.getElementById('tokenActive').checked;
      try {
        const data = await fetchDataWithAuth(`${API_BASE}/auth-tokens`, {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json'
          },
          body: JSON.stringify({ description, expires_at: expiresAt, is_active: isActive })
        });

        closeCreateModal();
        document.getElementById('newTokenValue').value = data.token;
        document.getElementById('tokenResultModal').style.display = 'block';
        loadTokens();
        window.showNotification('令牌创建成功', 'success');
      } catch (error) {
        console.error('创建令牌失败:', error);
        window.showNotification('创建失败: ' + error.message, 'error');
      }
    }

    function copyToken() {
      const textarea = document.getElementById('newTokenValue');
      textarea.select();
      document.execCommand('copy');
      window.showNotification('已复制到剪贴板', 'success');
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
      document.getElementById('editModal').style.display = 'block';
    }

    function closeEditModal() {
      document.getElementById('editModal').style.display = 'none';
    }

    async function updateToken() {
      const id = document.getElementById('editTokenId').value;
      const description = document.getElementById('editTokenDescription').value.trim();
      const isActive = document.getElementById('editTokenActive').checked;
      const expiryType = document.getElementById('editTokenExpiry').value;
      let expiresAt = null;
      if (expiryType !== 'never') {
        if (expiryType === 'custom') {
          const customDate = document.getElementById('editCustomExpiry').value;
          if (!customDate) {
            window.showNotification('请选择过期时间', 'error');
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
          body: JSON.stringify({ description, is_active: isActive, expires_at: expiresAt })
        });
        closeEditModal();
        loadTokens();
        window.showNotification('更新成功', 'success');
      } catch (error) {
        console.error('更新失败:', error);
        window.showNotification('更新失败: ' + error.message, 'error');
      }
    }

    async function deleteToken(id) {
      if (!confirm('确定要删除此令牌吗?删除后无法恢复。')) return;
      try {
        await fetchDataWithAuth(`${API_BASE}/auth-tokens/${id}`, {
          method: 'DELETE'
        });
        loadTokens();
        window.showNotification('删除成功', 'success');
      } catch (error) {
        console.error('删除失败:', error);
        window.showNotification('删除失败: ' + error.message, 'error');
      }
    }

    // 初始化顶部导航栏
    document.addEventListener('DOMContentLoaded', () => {
      initTopbar('tokens');
    });
