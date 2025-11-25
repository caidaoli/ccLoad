    const API_BASE = '/admin';
    let allTokens = [];
    let selectedTokenIds = new Set();

    document.addEventListener('DOMContentLoaded', () => {
      loadTokens();
      document.getElementById('tokenExpiry').addEventListener('change', (e) => {
        document.getElementById('customExpiryContainer').style.display =
          e.target.value === 'custom' ? 'block' : 'none';
      });
      document.getElementById('editTokenExpiry').addEventListener('change', (e) => {
        document.getElementById('editCustomExpiryContainer').style.display =
          e.target.value === 'custom' ? 'block' : 'none';
      });
    });

    async function loadTokens() {
      try {
        const response = await fetchWithAuth(`${API_BASE}/auth-tokens`);
        if (!response.ok) throw new Error('加载令牌失败');
        const data = await response.json();
        allTokens = data.data || [];
        renderTokens();
      } catch (error) {
        console.error('加载令牌失败:', error);
        showToast('加载令牌失败: ' + error.message, 'error');
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

      // 根据选中状态决定操作列显示内容
      const hasSelection = selectedTokenIds.size > 0;
      const operationHeader = hasSelection
        ? `<div style="display: flex; align-items: center; gap: 8px;">
             <span style="color: var(--primary-700); font-weight: 600; font-size: 12px;">已选择 ${selectedTokenIds.size} 项</span>
             <button onclick="batchDeleteTokens()" class="btn btn-danger" style="padding: 4px 12px; font-size: 12px;">批量删除</button>
           </div>`
        : '操作';

      const tableHTML = `
        <table>
          <thead>
            <tr>
              <th style="width: 40px;">
                <input type="checkbox" id="select-all" onchange="toggleSelectAll(this.checked)" style="width: 18px; height: 18px; cursor: pointer;">
              </th>
              <th>描述</th>
              <th>令牌</th>
              <th>状态</th>
              <th style="text-align: center;">调用次数</th>
              <th style="text-align: center;">成功率</th>
              <th style="text-align: center;">Token用量</th>
              <th style="text-align: center;">总费用</th>
              <th style="text-align: center;">流式首字平均响应</th>
              <th style="text-align: center;">非流式平均响应</th>
              <th>创建时间</th>
              <th>最后使用</th>
              <th>过期时间</th>
              <th style="width: 200px;">${operationHeader}</th>
            </tr>
          </thead>
          <tbody>
            ${allTokens.map(token => createTokenRow(token)).join('')}
          </tbody>
        </table>
      `;

      container.innerHTML = tableHTML;
      updateSelectAllCheckbox();
    }

    function createTokenRow(token) {
      const status = getTokenStatus(token);
      const createdAt = new Date(token.created_at).toLocaleString('zh-CN');
      const lastUsed = token.last_used_at ? new Date(token.last_used_at).toLocaleString('zh-CN') : '从未使用';
      const expiresAt = token.expires_at ? new Date(token.expires_at).toLocaleString('zh-CN') : '永不过期';
      const isSelected = selectedTokenIds.has(token.id);

      // 计算统计信息
      const successCount = token.success_count || 0;
      const failureCount = token.failure_count || 0;
      const totalCount = successCount + failureCount;
      const successRate = totalCount > 0 ? ((successCount / totalCount) * 100).toFixed(1) : 0;

      // 成功率样式
      let successRateClass = 'stats-badge';
      if (totalCount > 0) {
        if (successRate >= 95) successRateClass += ' success-rate-high';
        else if (successRate >= 80) successRateClass += ' success-rate-medium';
        else successRateClass += ' success-rate-low';
      }

      // 平均响应时间
      const streamAvgTTFB = token.stream_avg_ttfb || 0;
      const nonStreamAvgRT = token.non_stream_avg_rt || 0;
      const streamCount = token.stream_count || 0;
      const nonStreamCount = token.non_stream_count || 0;

      // 调用次数样式（根据调用量）
      let countFontWeight = '500';
      let successBgOpacity = '100';
      let errorBgOpacity = '100';
      let successColorOpacity = '700';
      let errorColorOpacity = '700';

      if (totalCount >= 100) {
        countFontWeight = '700';
        successBgOpacity = '200';
        errorBgOpacity = '200';
        successColorOpacity = '800';
        errorColorOpacity = '800';
      } else if (totalCount >= 10) {
        countFontWeight = '600';
        successBgOpacity = '100';
        errorBgOpacity = '100';
        successColorOpacity = '700';
        errorColorOpacity = '700';
      } else {
        countFontWeight = '500';
        successBgOpacity = '50';
        errorBgOpacity = '50';
        successColorOpacity = '600';
        errorColorOpacity = '600';
      }

      // 响应时间颜色等级
      const getResponseClass = (time) => {
        const num = Number(time);
        if (!Number.isFinite(num) || num <= 0) return '';
        if (num < 3) return 'response-fast';
        if (num < 6) return 'response-medium';
        return 'response-slow';
      };

      return `
        <tr>
          <td>
            <input type="checkbox"
              class="token-checkbox"
              data-token-id="${token.id}"
              onchange="toggleTokenSelection(${token.id}, this.checked)"
              ${isSelected ? 'checked' : ''}
              style="width: 18px; height: 18px; cursor: pointer;">
          </td>
          <td style="font-weight: 500;">${escapeHtml(token.description)}</td>
          <td><span class="token-display">${escapeHtml(token.token)}</span></td>
          <td><span class="status-badge status-${status.class}">${status.text}</span></td>
          <td style="text-align: center;">
            ${totalCount > 0 ? `
              <div style="display: inline-flex; gap: 8px;">
                <span class="stats-badge" style="background: var(--success-${successBgOpacity}); color: var(--success-${successColorOpacity}); font-weight: ${countFontWeight};" title="成功调用">
                  ✓ ${successCount.toLocaleString()}
                </span>
                <span class="stats-badge" style="background: var(--error-${errorBgOpacity}); color: var(--error-${errorColorOpacity}); font-weight: ${countFontWeight};" title="失败调用">
                  ✗ ${failureCount.toLocaleString()}
                </span>
              </div>
            ` : '<span style="color: var(--neutral-500); font-size: 13px;">-</span>'}
          </td>
          <td style="text-align: center;">
            ${totalCount > 0
              ? `<span class="${successRateClass}">${successRate}%</span>`
              : '<span style="color: var(--neutral-500); font-size: 13px;">-</span>'}
          </td>
          <td style="text-align: center;">
            ${(token.prompt_tokens_total > 0 || token.completion_tokens_total > 0) ? `
              <div style="display: flex; flex-direction: column; align-items: center; gap: 4px;">
                <div style="display: inline-flex; gap: 6px; font-size: 12px;">
                  <span class="stats-badge" style="background: var(--primary-50); color: var(--primary-700);" title="输入Tokens">
                    ${(token.prompt_tokens_total || 0).toLocaleString()}
                  </span>
                  <span class="stats-badge" style="background: var(--secondary-50); color: var(--secondary-700);" title="输出Tokens">
                    ${(token.completion_tokens_total || 0).toLocaleString()}
                  </span>
                </div>
             
              </div>
            ` : '<span style="color: var(--neutral-500); font-size: 13px;">-</span>'}
          </td>
          <td style="text-align: center;">
            ${(token.total_cost_usd > 0) ? `
              <div style="display: flex; flex-direction: column; align-items: center; gap: 2px;">
                <span class="metric-value" style="color: var(--success-700); font-size: 15px; font-weight: 700;">
                  $${token.total_cost_usd.toFixed(4)}
                </span>
              
              </div>
            ` : '<span style="color: var(--neutral-500); font-size: 13px;">$0.00</span>'}
          </td>
          <td style="text-align: center;">
            ${streamCount > 0
              ? `<span class="metric-value ${getResponseClass(streamAvgTTFB)}">${streamAvgTTFB.toFixed(2)}s</span>`
              : '<span style="color: var(--neutral-500); font-size: 13px;">-</span>'}
          </td>
          <td style="text-align: center;">
            ${nonStreamCount > 0
              ? `<span class="metric-value ${getResponseClass(nonStreamAvgRT)}">${nonStreamAvgRT.toFixed(2)}s</span>`
              : '<span style="color: var(--neutral-500); font-size: 13px;">-</span>'}
          </td>
          <td style="color: var(--neutral-600);">${createdAt}</td>
          <td style="color: var(--neutral-600);">${lastUsed}</td>
          <td style="color: var(--neutral-600);">${expiresAt}</td>
          <td>
            <button onclick="editToken(${token.id})" class="btn btn-secondary" style="padding: 4px 12px; font-size: 13px; margin-right: 4px;">编辑</button>
            <button onclick="deleteToken(${token.id})" class="btn btn-danger" style="padding: 4px 12px; font-size: 13px;">删除</button>
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
        showToast('请输入描述', 'error');
        return;
      }
      const expiryType = document.getElementById('tokenExpiry').value;
      let expiresAt = null;
      if (expiryType !== 'never') {
        if (expiryType === 'custom') {
          const customDate = document.getElementById('customExpiry').value;
          if (!customDate) {
            showToast('请选择过期时间', 'error');
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
        const response = await fetchWithAuth(`${API_BASE}/auth-tokens`, {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json'
          },
          body: JSON.stringify({ description, expires_at: expiresAt, is_active: isActive })
        });
        if (!response.ok) throw new Error('创建失败');
        const data = await response.json();

        closeCreateModal();
        document.getElementById('newTokenValue').value = data.data.token;
        document.getElementById('tokenResultModal').style.display = 'block';
        loadTokens();
        showToast('令牌创建成功', 'success');
      } catch (error) {
        console.error('创建令牌失败:', error);
        showToast('创建失败: ' + error.message, 'error');
      }
    }

    function copyToken() {
      const textarea = document.getElementById('newTokenValue');
      textarea.select();
      document.execCommand('copy');
      showToast('已复制到剪贴板', 'success');
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
            showToast('请选择过期时间', 'error');
            return;
          }
          expiresAt = new Date(customDate).getTime();
        } else {
          const days = parseInt(expiryType);
          expiresAt = Date.now() + days * 24 * 60 * 60 * 1000;
        }
      }
      try {
        const response = await fetchWithAuth(`${API_BASE}/auth-tokens/${id}`, {
          method: 'PUT',
          headers: {
            'Content-Type': 'application/json'
          },
          body: JSON.stringify({ description, is_active: isActive, expires_at: expiresAt })
        });
        if (!response.ok) throw new Error('更新失败');
        closeEditModal();
        loadTokens();
        showToast('更新成功', 'success');
      } catch (error) {
        console.error('更新失败:', error);
        showToast('更新失败: ' + error.message, 'error');
      }
    }

    async function deleteToken(id) {
      if (!confirm('确定要删除此令牌吗?删除后无法恢复。')) return;
      try {
        const response = await fetchWithAuth(`${API_BASE}/auth-tokens/${id}`, {
          method: 'DELETE'
        });
        if (!response.ok) throw new Error('删除失败');
        selectedTokenIds.delete(id);
        loadTokens();
        showToast('删除成功', 'success');
      } catch (error) {
        console.error('删除失败:', error);
        showToast('删除失败: ' + error.message, 'error');
      }
    }

    // 批量操作相关函数
    function toggleTokenSelection(tokenId, checked) {
      if (checked) {
        selectedTokenIds.add(tokenId);
      } else {
        selectedTokenIds.delete(tokenId);
      }
      renderTokens(); // 重新渲染以更新表头
    }

    function toggleSelectAll(checked) {
      if (checked) {
        allTokens.forEach(token => selectedTokenIds.add(token.id));
      } else {
        selectedTokenIds.clear();
      }
      renderTokens();
    }

    function updateSelectAllCheckbox() {
      const selectAllCheckbox = document.getElementById('select-all');
      if (selectAllCheckbox) {
        selectAllCheckbox.checked = allTokens.length > 0 && selectedTokenIds.size === allTokens.length;
        selectAllCheckbox.indeterminate = selectedTokenIds.size > 0 && selectedTokenIds.size < allTokens.length;
      }
    }



    async function batchDeleteTokens() {
      if (selectedTokenIds.size === 0) {
        showToast('请先选择要删除的令牌', 'error');
        return;
      }

      if (!confirm(`确定要删除选中的 ${selectedTokenIds.size} 个令牌吗?删除后无法恢复。`)) {
        return;
      }

      const idsToDelete = Array.from(selectedTokenIds);
      let successCount = 0;
      let failCount = 0;

      for (const id of idsToDelete) {
        try {
          const response = await fetchWithAuth(`${API_BASE}/auth-tokens/${id}`, {
            method: 'DELETE'
          });
          if (response.ok) {
            successCount++;
            selectedTokenIds.delete(id);
          } else {
            failCount++;
          }
        } catch (error) {
          console.error(`删除令牌 ${id} 失败:`, error);
          failCount++;
        }
      }

      loadTokens();

      if (failCount === 0) {
        showToast(`成功删除 ${successCount} 个令牌`, 'success');
      } else {
        showToast(`删除完成: 成功 ${successCount} 个, 失败 ${failCount} 个`, 'error');
      }
    }

    function showToast(message, type = 'info') {
      const toast = document.createElement('div');
      toast.className = `toast toast-${type}`;
      toast.textContent = message;
      toast.style.cssText = `
        position: fixed; top: 20px; right: 20px; padding: 12px 20px;
        background: ${type === 'success' ? 'var(--success-500)' : type === 'error' ? 'var(--error-500)' : 'var(--primary-500)'};
        color: white; border-radius: 8px; box-shadow: 0 4px 12px rgba(0,0,0,0.15);
        z-index: 10000; animation: slideIn 0.3s ease-out;
      `;
      document.body.appendChild(toast);
      setTimeout(() => {
        toast.style.animation = 'slideOut 0.3s ease-out';
        setTimeout(() => toast.remove(), 300);
      }, 3000);
    }

    function escapeHtml(text) {
      const div = document.createElement('div');
      div.textContent = text;
      return div.innerHTML;
    }

    // 初始化顶部导航栏
    document.addEventListener('DOMContentLoaded', () => {
      initTopbar('tokens');
    });
