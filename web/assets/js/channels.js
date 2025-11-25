    // 渠道统计时间窗口（默认30天，避免全量查询拖垮DB）
    const CHANNEL_STATS_LOOKBACK_HOURS = 24 * 30;
    let channels = [];
    let channelStatsById = {};
    let editingChannelId = null;
    let deletingChannelId = null;
    let testingChannelId = null;
    let currentChannelKeyCooldowns = []; // 当前编辑渠道的Key冷却信息
    let redirectTableData = []; // 模型重定向表格数据: [{from: '', to: ''}]
    
    // Filter state
    let filters = {
      search: '',
      id: '',
      channelType: 'all',
      status: 'all',
      model: 'all'
    };
    
    // Debounce function for search input
    function debounce(func, wait) {
      let timeout;
      return function executedFunction(...args) {
        const later = () => {
          clearTimeout(timeout);
          func(...args);
        };
        clearTimeout(timeout);
        timeout = setTimeout(later, wait);
      };
    }
    
    // Filter channels based on current filters
    function filterChannels() {
      const filtered = channels.filter(channel => {
        // Name search
        if (filters.search && !channel.name.toLowerCase().includes(filters.search.toLowerCase())) {
          return false;
        }

        // ID filter (支持精确ID或逗号分隔的多个ID)
        if (filters.id) {
          const idStr = filters.id.trim();
          if (idStr) {
            // 支持逗号分隔的多个ID
            const ids = idStr.split(',').map(id => id.trim()).filter(id => id);
            if (ids.length > 0 && !ids.includes(String(channel.id))) {
              return false;
            }
          }
        }

        // Channel type filter
        if (filters.channelType !== 'all') {
          const channelType = channel.channel_type || 'anthropic';
          if (channelType !== filters.channelType) {
            return false;
          }
        }

        // Status filter
        if (filters.status !== 'all') {
          if (filters.status === 'enabled' && !channel.enabled) return false;
          if (filters.status === 'disabled' && channel.enabled) return false;
          if (filters.status === 'cooldown' && !(channel.cooldown_remaining_ms > 0)) return false;
        }

        // Model filter
        if (filters.model !== 'all' && !channel.models.includes(filters.model)) {
          return false;
        }

        return true;
      });

      renderChannels(filtered);
      updateFilterInfo(filtered.length, channels.length);
    }
    
    // Update filter info display
    function updateFilterInfo(filtered, total) {
      document.getElementById('filteredCount').textContent = filtered;
      document.getElementById('totalCount').textContent = total;
    }
    
    // Update model filter options
    function updateModelOptions() {
      const modelSet = new Set();
      channels.forEach(channel => {
        if (Array.isArray(channel.models)) {
          channel.models.forEach(model => modelSet.add(model));
        }
      });
      
      const modelFilter = document.getElementById('modelFilter');
      const currentValue = modelFilter.value;
      
      // Clear existing options (keep "All Models")
      modelFilter.innerHTML = '<option value="all">所有模型</option>';
      
      // Add model options
      Array.from(modelSet).sort().forEach(model => {
        const option = document.createElement('option');
        option.value = model;
        option.textContent = model;
        modelFilter.appendChild(option);
      });
      
      // Restore selection
      modelFilter.value = currentValue;
    }
    
    // Setup filter event listeners
    function setupFilterListeners() {
      // Search input with debounce
      const searchInput = document.getElementById('searchInput');
      const clearSearchBtn = document.getElementById('clearSearchBtn');

      const debouncedFilter = debounce(() => {
        filters.search = searchInput.value;
        filterChannels();
        updateClearButton();
      }, 300);

      searchInput.addEventListener('input', debouncedFilter);

      // Clear search button
      clearSearchBtn.addEventListener('click', () => {
        searchInput.value = '';
        filters.search = '';
        filterChannels();
        updateClearButton();
        searchInput.focus();
      });

      // Update clear button visibility
      function updateClearButton() {
        clearSearchBtn.style.opacity = searchInput.value ? '1' : '0';
      }

      // ID filter with debounce
      const idFilter = document.getElementById('idFilter');
      const debouncedIdFilter = debounce(() => {
        filters.id = idFilter.value;
        filterChannels();
      }, 300);
      idFilter.addEventListener('input', debouncedIdFilter);

      // Channel type filter
      document.getElementById('channelTypeFilter').addEventListener('change', (e) => {
        filters.channelType = e.target.value;
        filterChannels();
      });

      // Status filter
      document.getElementById('statusFilter').addEventListener('change', (e) => {
        filters.status = e.target.value;
        filterChannels();
      });
      
      // Model filter
      document.getElementById('modelFilter').addEventListener('change', (e) => {
        filters.model = e.target.value;
        filterChannels();
      });
      
      // Reset filters button
      document.getElementById('resetFiltersBtn').addEventListener('click', () => {
        // Reset filter values
        filters = {
          search: '',
          id: '',
          channelType: 'all',
          status: 'all',
          model: 'all'
        };

        // Reset form elements
        searchInput.value = '';
        document.getElementById('idFilter').value = '';
        document.getElementById('channelTypeFilter').value = 'all';
        document.getElementById('statusFilter').value = 'all';
        document.getElementById('modelFilter').value = 'all';

        // Update display
        filterChannels();
        updateClearButton();
        searchInput.focus();
      });
    }

    // Toggle API Key visibility
    function toggleApiKeyVisibility() {
      const apiKeyInput = document.getElementById('channelApiKey');
      const eyeIcon = document.getElementById('eyeIcon');
      const eyeOffIcon = document.getElementById('eyeOffIcon');

      if (apiKeyInput.type === 'password') {
        apiKeyInput.type = 'text';
        eyeIcon.style.display = 'none';
        eyeOffIcon.style.display = 'block';
      } else {
        apiKeyInput.type = 'password';
        eyeIcon.style.display = 'block';
        eyeOffIcon.style.display = 'none';
      }
    }

    async function loadChannels() {
      try {
        const res = await fetchWithAuth('/admin/channels');
        if (!res.ok) throw new Error('HTTP ' + res.status);
        const response = await res.json();
        // 处理新的API响应格式：{ success: true, data: [...] }
        channels = response.success ? (response.data || []) : (response || []);
        updateModelOptions();
        filterChannels(); // Use filterChannels instead of direct render
      } catch (e) {
        console.error('加载渠道失败', e);
        if (window.showError) showError('加载渠道失败');
      }
    }

    async function loadChannelStats(hours = CHANNEL_STATS_LOOKBACK_HOURS) {
      try {
        const params = new URLSearchParams({ hours: String(hours), limit: '500', offset: '0' });
        const res = await fetchWithAuth(`/admin/stats?${params.toString()}`);
        if (!res.ok) throw new Error('HTTP ' + res.status);
        const response = await res.json();
        const statsArray = extractStatsEntries(response);
        channelStatsById = aggregateChannelStats(statsArray);
        filterChannels(); // 重新渲染以显示最新统计
      } catch (err) {
        console.error('加载渠道统计数据失败', err);
      }
    }

    function extractStatsEntries(response) {
      if (!response) return [];
      if (Array.isArray(response)) return response;
      if (Array.isArray(response.data?.stats)) return response.data.stats;
      if (Array.isArray(response.stats)) return response.stats;
      if (Array.isArray(response.data)) return response.data;
      return [];
    }

    function aggregateChannelStats(statsEntries = []) {
      const result = {};

      for (const entry of statsEntries) {
        const channelId = Number(entry.channel_id || entry.channelID);
        if (!Number.isFinite(channelId) || channelId <= 0) continue;

        if (!result[channelId]) {
          result[channelId] = {
            success: 0,
            error: 0,
            total: 0,
            totalInputTokens: 0,
            totalOutputTokens: 0,
            totalCacheReadInputTokens: 0,
            totalCacheCreationInputTokens: 0,
            totalCost: 0,
            _firstByteWeightedSum: 0,
            _firstByteWeight: 0
          };
        }

        const stats = result[channelId];
        const success = toSafeNumber(entry.success);
        const error = toSafeNumber(entry.error);
        const total = toSafeNumber(entry.total);

        stats.success += success;
        stats.error += error;
        stats.total += total;

        const avgFirstByte = Number(entry.avg_first_byte_time_seconds);
        const weight = success || total || 0;
        if (Number.isFinite(avgFirstByte) && avgFirstByte > 0 && weight > 0) {
          stats._firstByteWeightedSum += avgFirstByte * weight;
          stats._firstByteWeight += weight;
        }

        stats.totalInputTokens += toSafeNumber(entry.total_input_tokens);
        stats.totalOutputTokens += toSafeNumber(entry.total_output_tokens);
        stats.totalCacheReadInputTokens += toSafeNumber(entry.total_cache_read_input_tokens);
        stats.totalCacheCreationInputTokens += toSafeNumber(entry.total_cache_creation_input_tokens);
        stats.totalCost += toSafeNumber(entry.total_cost);
      }

      for (const id of Object.keys(result)) {
        const stats = result[id];
        if (stats._firstByteWeight > 0) {
          stats.avgFirstByteTimeSeconds = stats._firstByteWeightedSum / stats._firstByteWeight;
        }
        delete stats._firstByteWeightedSum;
        delete stats._firstByteWeight;
      }

      return result;
    }

    function toSafeNumber(value) {
      const num = Number(value);
      return Number.isFinite(num) ? num : 0;
    }

    function renderChannels(channelsToRender = channels) {
      const el = document.getElementById('channels-container');
      if (!channelsToRender || channelsToRender.length === 0) {
        el.innerHTML = '<div class="glass-card">暂无符合条件的渠道</div>';
        return;
      }
      el.innerHTML = channelsToRender.map(c => {
        const isCooldown = c.cooldown_remaining_ms > 0;
        const cardClasses = ['glass-card'];

        if (isCooldown) {
          cardClasses.push('channel-card-cooldown');
        }
        if (!c.enabled) {
          cardClasses.push('channel-disabled');
        }

        // 渠道类型显示标签（优化版：更鲜明的颜色和边框）
        const channelTypeLabels = {
          'anthropic': {
            text: 'Claude',
            color: '#8b5cf6',      // 紫色 - Anthropic品牌色
            bgColor: '#f3e8ff',    // 浅紫背景
            borderColor: '#c4b5fd' // 紫色边框
          },
          'codex': {
            text: 'Codex',
            color: '#059669',      // 绿色 - Codex品牌色
            bgColor: '#d1fae5',    // 浅绿背景
            borderColor: '#6ee7b7' // 绿色边框
          },
          'openai': {
            text: 'OpenAI',
            color: '#10b981',      // 绿色 - OpenAI品牌色
            bgColor: '#d1fae5',    // 浅绿背景
            borderColor: '#6ee7b7' // 绿色边框
          },
          'gemini': {
            text: 'Gemini',
            color: '#2563eb',      // 蓝色 - Google品牌色
            bgColor: '#dbeafe',    // 浅蓝背景
            borderColor: '#93c5fd' // 蓝色边框
          }
        };
        // 防御性编程：如果类型未定义，使用默认值（KISS原则）
        const channelTypeRaw = (c.channel_type || '').toLowerCase();
        const channelTypeInfo = channelTypeLabels[channelTypeRaw || 'anthropic'] || channelTypeLabels['anthropic'];
        const channelTypeBadge = `<span style="background: ${channelTypeInfo.bgColor}; color: ${channelTypeInfo.color}; padding: 3px 10px; border-radius: 6px; font-size: 0.75rem; font-weight: 700; margin-left: 8px; border: 1.5px solid ${channelTypeInfo.borderColor}; letter-spacing: 0.025em; text-transform: uppercase;">${channelTypeInfo.text}</span>`;

        const stats = channelStatsById[c.id] || null;
        const successCount = stats ? stats.success : null;
        const errorCount = stats ? stats.error : null;
        const totalCount = stats ? stats.total : null;
        const successRateText = formatSuccessRate(successCount, totalCount);
        const avgFirstByteText = formatAvgFirstByte(stats ? stats.avgFirstByteTimeSeconds : null);
        const inputTokensText = formatMetricNumber(stats ? stats.totalInputTokens : null);
        const outputTokensText = formatMetricNumber(stats ? stats.totalOutputTokens : null);
        const cacheReadText = formatMetricNumber(stats ? stats.totalCacheReadInputTokens : null);
        const cacheCreationText = formatMetricNumber(stats ? stats.totalCacheCreationInputTokens : null);
        const costDisplay = formatCostValue(stats ? stats.totalCost : null);
        // 所有渠道类型都显示统计信息（包括 Gemini 和 OpenAI）
        const showStatsInline = true;
        const statsInline = showStatsInline && stats
          ? renderChannelStatsInline(stats, {
              successRateText,
              avgFirstByteText,
              inputTokensText,
              outputTokensText,
              cacheReadText,
              cacheCreationText,
              costDisplay
            }, channelTypeRaw)
          : '';

        return `
          <div class="${cardClasses.join(' ')}" id="channel-${c.id}">
            <div class="flex justify-between items-center">
              <div style="flex: 1;">
                <div class="section-title">${escapeHtml(c.name)} ${channelTypeBadge} <span style="color: var(--neutral-500); font-size: 0.875rem; font-weight: 400;">(ID: ${c.id})</span> <span style="color: var(--neutral-600); font-size: 1rem; font-weight: 400;">模型: ${Array.isArray(c.models) ? c.models.join(', ') : ''}</span></div>
                <div class="text-sm" style="color: var(--neutral-600); margin-top: 4px;">
                  <div class="channel-meta-line">
                    <span>URL: ${escapeHtml(c.url)} | 优先级: ${c.priority} | ${c.enabled ? '已启用' : '已禁用'}${inlineCooldownBadge(c)}</span>
                    ${statsInline ? `<span class="channel-stats-inline">${statsInline}</span>` : ''}
                  </div>
                </div>
              </div>
              <div class="channel-actions">
                <button class="btn-icon" onclick="editChannel(${c.id})" title="编辑">编辑</button>
                <button class="btn-icon" onclick="testChannel(${c.id}, '${escapeHtml(c.name)}')" title="测试API Key">测试</button>
                <button class="btn-icon" onclick="toggleChannel(${c.id}, ${!c.enabled})">${c.enabled ? '禁用' : '启用'}</button>
                <button class="btn-icon" onclick="copyChannel(${c.id}, '${escapeHtml(c.name)}')" title="复制渠道">复制</button>
                <button class="btn-icon btn-danger" onclick="deleteChannel(${c.id}, '${escapeHtml(c.name)}')" title="删除">删除</button>
              </div>
            </div>
          </div>
        `;
      }).join('');
    }

    function cooldownBadge(c) {
      const ms = c.cooldown_remaining_ms || 0;
      if (!ms || ms <= 0) return '';
      const text = humanizeMS(ms);
      return `<div class="cooldown-badge">
        <span class="cooldown-icon">⚠️</span>
        <span>冷却中 · 剩余 ${text}</span>
      </div>`;
    }

    function inlineCooldownBadge(c) {
      const ms = c.cooldown_remaining_ms || 0;
      if (!ms || ms <= 0) return '';
      const text = humanizeMS(ms);
      return ` <span style="color: #dc2626; font-size: 0.875rem; font-weight: 500; background: linear-gradient(135deg, #fee2e2 0%, #fecaca 100%); padding: 2px 8px; border-radius: 4px; border: 1px solid #fca5a5;">⚠️ 冷却中·${text}</span>`;
    }

    function humanizeMS(ms) {
      let s = Math.ceil(ms / 1000);
      const h = Math.floor(s / 3600);
      s = s % 3600;
      const m = Math.floor(s / 60);
      s = s % 60;
      
      if (h > 0) return `${h}小时${m}分`;
      if (m > 0) return `${m}分${s}秒`;
      return `${s}秒`;
    }

    function showAddModal() {
      editingChannelId = null;
      currentChannelKeyCooldowns = []; // 清空冷却信息
      
      document.getElementById('modalTitle').textContent = '添加渠道';
      document.getElementById('channelForm').reset();
      document.getElementById('channelEnabled').checked = true;
      // 设置默认选中的单选框
      document.querySelector('input[name="channelType"][value="anthropic"]').checked = true;
      document.querySelector('input[name="keyStrategy"][value="sequential"]').checked = true;

      // 初始化模型重定向表格（添加模式默认为空）
      redirectTableData = [];
      renderRedirectTable();

      // 初始化内联Key表格（添加模式默认一个空Key）
      inlineKeyTableData = [''];
      inlineKeyVisible = true; // 新增时默认显示明文,方便核对
      document.getElementById('inlineEyeIcon').style.display = 'none';
      document.getElementById('inlineEyeOffIcon').style.display = 'block';
      renderInlineKeyTable();

      document.getElementById('channelModal').classList.add('show');
    }

    async function editChannel(id) {
      const channel = channels.find(c => c.id === id);
      if (!channel) return;

      editingChannelId = id;

      document.getElementById('modalTitle').textContent = '编辑渠道';
      document.getElementById('channelName').value = channel.name;
      document.getElementById('channelUrl').value = channel.url;

      // ✅ 修复：异步从后端获取 API Keys（2025-10 新架构：api_keys表独立存储）
      let apiKeys = [];
      try {
        const res = await fetchWithAuth(`/admin/channels/${id}/keys`);
        if (res.ok) {
          const data = await res.json();
          apiKeys = (data.success ? data.data : data) || [];
          console.log('🔍 [DEBUG] API响应:', data);
          console.log('🔍 [DEBUG] 提取的apiKeys:', apiKeys);
        }
      } catch (e) {
        console.error('获取API Keys失败', e);
      }

      // ✅ 修复(2025-11): 从 APIKey 对象提取冷却信息
      // APIKey对象包含: api_key, cooldown_until(Unix秒), cooldown_duration_ms
      const now = Date.now();
      console.log('🔍 [DEBUG] 当前时间戳(ms):', now, '| Unix秒:', Math.floor(now / 1000));
      currentChannelKeyCooldowns = apiKeys.map((apiKey, index) => {
        const cooldownUntilMs = (apiKey.cooldown_until || 0) * 1000; // Unix秒→毫秒
        const remainingMs = Math.max(0, cooldownUntilMs - now);
        console.log(`🔍 [DEBUG] Key #${index + 1}:`, {
          api_key_preview: (apiKey.api_key || '').substring(0, 10) + '...',
          cooldown_until: apiKey.cooldown_until,
          cooldown_until_ms: cooldownUntilMs,
          remaining_ms: remainingMs,
          is_cooling: remainingMs > 0
        });
        return {
          key_index: index,
          cooldown_remaining_ms: remainingMs
        };
      });
      console.log('🔍 [DEBUG] 生成的冷却数组:', currentChannelKeyCooldowns);

      // 提取 API Key 字符串用于表格显示
      inlineKeyTableData = apiKeys.map(k => k.api_key || k);
      if (inlineKeyTableData.length === 0) {
        inlineKeyTableData = [''];
        currentChannelKeyCooldowns = [];
      }

      // 编辑时默认显示Key以便核对
      inlineKeyVisible = true;
      document.getElementById('inlineEyeIcon').style.display = 'none';
      document.getElementById('inlineEyeOffIcon').style.display = 'block';
      renderInlineKeyTable();

      // 动态渲染渠道类型单选框（使用当前渠道的类型）
      const channelType = channel.channel_type || 'anthropic';
      await window.ChannelTypeManager.renderChannelTypeRadios('channelTypeRadios', channelType);
      // 设置Key策略单选框
      const keyStrategy = channel.key_strategy || 'sequential';
      const strategyRadio = document.querySelector(`input[name="keyStrategy"][value="${keyStrategy}"]`);
      if (strategyRadio) {
        strategyRadio.checked = true;
      }
      document.getElementById('channelPriority').value = channel.priority;
      document.getElementById('channelModels').value = channel.models.join(',');
      document.getElementById('channelEnabled').checked = channel.enabled;

      // 设置模型重定向表格
      const modelRedirects = channel.model_redirects || {};
      redirectTableData = jsonToRedirectTable(modelRedirects);
      renderRedirectTable();

      document.getElementById('channelModal').classList.add('show');
    }

    function closeModal() {
      document.getElementById('channelModal').classList.remove('show');
      editingChannelId = null;
    }

    async function saveChannel(event) {
      event.preventDefault();

      // 验证内联Key表格（过滤空Key）
      const validKeys = inlineKeyTableData.filter(k => k && k.trim());
      if (validKeys.length === 0) {
        alert('请至少添加一个有效的API Key');
        return;
      }

      // 更新隐藏input（确保最新数据）
      document.getElementById('channelApiKey').value = validKeys.join(',');

      // 从表格获取模型重定向数据
      const modelRedirects = redirectTableToJSON();

      // 获取选中的单选框的值
      const channelType = document.querySelector('input[name="channelType"]:checked')?.value || 'anthropic';
      const keyStrategy = document.querySelector('input[name="keyStrategy"]:checked')?.value || 'sequential';

      const formData = {
        name: document.getElementById('channelName').value.trim(),
        url: document.getElementById('channelUrl').value.trim(),
        api_key: validKeys.join(','),
        channel_type: channelType,
        key_strategy: keyStrategy,
        priority: parseInt(document.getElementById('channelPriority').value) || 0,
        models: document.getElementById('channelModels').value.split(',').map(m => m.trim()).filter(m => m),
        model_redirects: modelRedirects,
        enabled: document.getElementById('channelEnabled').checked
      };

      if (!formData.name || !formData.url || !formData.api_key || formData.models.length === 0) {
        if (window.showError) showError('请填写所有必填字段');
        return;
      }

      try {
        let res;
        if (editingChannelId) {
          // 编辑现有渠道
          res = await fetchWithAuth(`/admin/channels/${editingChannelId}`, {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(formData)
          });
        } else {
          // 添加新渠道
          res = await fetchWithAuth('/admin/channels', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(formData)
          });
        }

        if (!res.ok) {
          const text = await res.text();
          throw new Error(text || 'HTTP ' + res.status);
        }

        closeModal();
        await loadChannels();
        if (window.showSuccess) showSuccess(editingChannelId ? '渠道已更新' : '渠道已添加');
      } catch (e) {
        console.error('保存渠道失败', e);
        if (window.showError) showError('保存失败: ' + e.message);
      }
    }

    function deleteChannel(id, name) {
      deletingChannelId = id;
      document.getElementById('deleteChannelName').textContent = name;
      document.getElementById('deleteModal').classList.add('show');
    }

    function closeDeleteModal() {
      document.getElementById('deleteModal').classList.remove('show');
      deletingChannelId = null;
    }

    async function confirmDelete() {
      if (!deletingChannelId) return;

      try {
        const res = await fetchWithAuth(`/admin/channels/${deletingChannelId}`, {
          method: 'DELETE'
        });

        if (!res.ok) {
          const text = await res.text();
          throw new Error(text || 'HTTP ' + res.status);
        }

        closeDeleteModal();
        await loadChannels();
        if (window.showSuccess) showSuccess('渠道已删除');
      } catch (e) {
        console.error('删除渠道失败', e);
        if (window.showError) showError('删除失败: ' + e.message);
      }
    }

    async function toggleChannel(id, enabled) {
      try {
        const res = await fetchWithAuth(`/admin/channels/${id}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ enabled })
        });
        if (!res.ok) throw new Error('HTTP ' + res.status);
        await loadChannels();
        if (window.showSuccess) showSuccess(enabled ? '渠道已启用' : '渠道已禁用');
      } catch (e) {
        console.error('切换失败', e);
        if (window.showError) showError('操作失败');
      }
    }

    function copyChannel(id, name) {
      const channel = channels.find(c => c.id === id);
      if (!channel) return;

      // 生成复制的渠道名称，添加"复制"字样
      const copiedName = generateCopyName(name);

      // 填充表单数据
      editingChannelId = null; // 确保是新建模式
      currentChannelKeyCooldowns = []; // 清空冷却信息（复制的渠道不继承冷却状态）
      document.getElementById('modalTitle').textContent = '复制渠道';
      document.getElementById('channelName').value = copiedName;
      document.getElementById('channelUrl').value = channel.url;

      // 加载API Keys到内联表格
      inlineKeyTableData = parseKeys(channel.api_key);
      if (inlineKeyTableData.length === 0) {
        inlineKeyTableData = [''];
      }

      // 复制时默认显示Key以便用户核对
      inlineKeyVisible = true;
      document.getElementById('inlineEyeIcon').style.display = 'none';
      document.getElementById('inlineEyeOffIcon').style.display = 'block';
      renderInlineKeyTable();

      // 设置渠道类型单选框
      const channelType = channel.channel_type || 'anthropic';
      const radioButton = document.querySelector(`input[name="channelType"][value="${channelType}"]`);
      if (radioButton) {
        radioButton.checked = true;
      }
      // 设置Key策略单选框
      const keyStrategy = channel.key_strategy || 'sequential';
      const strategyRadio = document.querySelector(`input[name="keyStrategy"][value="${keyStrategy}"]`);
      if (strategyRadio) {
        strategyRadio.checked = true;
      }
      document.getElementById('channelPriority').value = channel.priority;
      document.getElementById('channelModels').value = channel.models.join(',');
      document.getElementById('channelEnabled').checked = true; // 复制的渠道默认启用

      // 复制模型重定向表格
      const modelRedirects = channel.model_redirects || {};
      redirectTableData = jsonToRedirectTable(modelRedirects);
      renderRedirectTable();

      document.getElementById('channelModal').classList.add('show');
    }

    function generateCopyName(originalName) {
      // 生成复制的渠道名称
      const copyPattern = /^(.+?)(?:\s*-\s*复制(?:\s*(\d+))?)?$/;
      const match = originalName.match(copyPattern);

      if (!match) {
        return originalName + ' - 复制';
      }

      const baseName = match[1];
      const copyNumber = match[2] ? parseInt(match[2]) + 1 : 1;

      // 检查是否存在重名
      const proposedName = copyNumber === 1 ? `${baseName} - 复制` : `${baseName} - 复制 ${copyNumber}`;

      // 检查是否与现有渠道重名
      const existingNames = channels.map(c => c.name.toLowerCase());
      if (existingNames.includes(proposedName.toLowerCase())) {
        // 如果重名，递归生成新名称
        return generateCopyName(proposedName);
      }

      return proposedName;
    }

    function formatMetricNumber(value) {
      if (value === null || value === undefined) return '--';
      const num = Number(value);
      if (!Number.isFinite(num)) return '--';
      return formatCompactNumber(num);
    }

    function formatCompactNumber(num) {
      const abs = Math.abs(num);
      if (abs >= 1_000_000) return (num / 1_000_000).toFixed(1).replace(/\\.0$/, '') + 'M';
      if (abs >= 1_000) return (num / 1_000).toFixed(1).replace(/\\.0$/, '') + 'K';
      return num.toString();
    }

    function formatSuccessRate(success, total) {
      if (success === null || success === undefined || total === null || total === undefined) return '--';
      const succ = Number(success);
      const ttl = Number(total);
      if (!Number.isFinite(succ) || !Number.isFinite(ttl) || ttl <= 0) return '--';
      return ((succ / ttl) * 100).toFixed(1) + '%';
    }

    function formatAvgFirstByte(value) {
      if (value === null || value === undefined) return '--';
      const num = Number(value);
      if (!Number.isFinite(num) || num <= 0) return '--';
      return num.toFixed(2) + '秒';
    }

    function formatCostValue(cost) {
      if (cost === null || cost === undefined) return '--';
      const num = Number(cost);
      if (!Number.isFinite(num)) return '--';
      if (num === 0) return '$0.00';
      if (num < 0) return '--';
      return formatCost(num);
    }

    function renderChannelStatsInline(stats, cache, channelType) {
      if (!stats) {
        return `<span class="channel-stat-badge" style="margin-left: 6px; color: var(--neutral-500);">统计: --</span>`;
      }

      const successRateText = cache?.successRateText || formatSuccessRate(stats.success, stats.total);
      const avgFirstByteText = cache?.avgFirstByteText || formatAvgFirstByte(stats.avgFirstByteTimeSeconds);
      const inputTokensText = cache?.inputTokensText || formatMetricNumber(stats.totalInputTokens);
      const outputTokensText = cache?.outputTokensText || formatMetricNumber(stats.totalOutputTokens);
      const cacheReadText = cache?.cacheReadText || formatMetricNumber(stats.totalCacheReadInputTokens);
      const cacheCreationText = cache?.cacheCreationText || formatMetricNumber(stats.totalCacheCreationInputTokens);
      const costDisplay = cache?.costDisplay || formatCostValue(stats.totalCost);

      const successRateColor = (() => {
        const rateNum = Number(successRateText.replace('%', ''));
        if (!Number.isFinite(rateNum)) return 'var(--neutral-600)';
        if (rateNum >= 95) return 'var(--success-600)';
        if (rateNum < 80) return 'var(--error-500)';
        return 'var(--warning-600)';
      })();

      const callText = `${formatMetricNumber(stats.success)}/${formatMetricNumber(stats.error)}`;

      // 基础统计（所有渠道）
      const parts = [
        `<span class="channel-stat-badge" style="color: var(--neutral-800);"><strong>调用</strong> ${callText}</span>`,
        `<span class="channel-stat-badge" style="color: ${successRateColor};"><strong>率</strong> ${successRateText}</span>`,
        `<span class="channel-stat-badge" style="color: var(--primary-700);"><strong>首字</strong> ${avgFirstByteText}</span>`,
        `<span class="channel-stat-badge" style="color: var(--neutral-800);"><strong>In</strong> ${inputTokensText}</span>`,
        `<span class="channel-stat-badge" style="color: var(--neutral-800);"><strong>Out</strong> ${outputTokensText}</span>`
      ];

      // 缓存统计（仅 Claude/Codex 支持）
      const supportsCaching = channelType === 'anthropic' || channelType === 'codex';
      if (supportsCaching) {
        parts.push(
          `<span class="channel-stat-badge" style="color: var(--success-600); background: var(--success-50); border-color: var(--success-100);"><strong>缓存读</strong> ${cacheReadText}</span>`,
          `<span class="channel-stat-badge" style="color: var(--primary-700); background: var(--primary-50); border-color: var(--primary-100);"><strong>缓存建</strong> ${cacheCreationText}</span>`
        );
      }

      // 成本统计（所有渠道）
      parts.push(
        `<span class="channel-stat-badge" style="color: var(--warning-700); background: var(--warning-50); border-color: var(--warning-100);"><strong>成本</strong> ${costDisplay}</span>`
      );

      return parts.join(' ');
    }

    // 成本格式化（美元）
    function formatCost(cost) {
      if (cost === 0) return '$0.00';
      if (cost < 0.001) {
        if (cost < 0.000001) {
          return '$' + cost.toExponential(2);
        }
        return '$' + cost.toFixed(6).replace(/\.?0+$/, '');
      }
      if (cost >= 1.0) {
        return '$' + cost.toFixed(2);
      }
      return '$' + cost.toFixed(4).replace(/\.?0+$/, '');
    }

    function escapeHtml(s) {
      return (s || '').replace(/[&<>"']/g, c => ({
        "&": "&amp;",
        "<": "&lt;",
        ">": "&gt;",
        "\"": "&quot;",
        "'": "&#39;"
      }[c]));
    }

    function formatTimestampForFilename() {
      const pad = (n) => String(n).padStart(2, '0');
      const now = new Date();
      return `${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}-${pad(now.getHours())}${pad(now.getMinutes())}${pad(now.getSeconds())}`;
    }

    function setupImportExport() {
      const exportBtn = document.getElementById('exportCsvBtn');
      const importBtn = document.getElementById('importCsvBtn');
      const importInput = document.getElementById('importCsvInput');

      if (exportBtn) {
        exportBtn.addEventListener('click', () => exportChannelsCSV(exportBtn));
      }

      if (importBtn && importInput) {
        importBtn.addEventListener('click', () => {
          // 暂停背景动画以减少文件选择器打开时的CPU占用
          if (window.pauseBackgroundAnimation) window.pauseBackgroundAnimation();
          importInput.click();
        });

        importInput.addEventListener('change', (event) => {
          // 恢复背景动画
          if (window.resumeBackgroundAnimation) window.resumeBackgroundAnimation();
          handleImportCSV(event, importBtn);
        });

        // 监听文件选择器的取消操作（用户未选择文件时也要恢复动画）
        importInput.addEventListener('cancel', () => {
          if (window.resumeBackgroundAnimation) window.resumeBackgroundAnimation();
        });
      }
    }

    async function exportChannelsCSV(buttonEl) {
      try {
        if (buttonEl) buttonEl.disabled = true;
        const res = await fetchWithAuth('/admin/channels/export');
        if (!res.ok) {
          const errorText = await res.text();
          throw new Error(errorText || `导出失败 (HTTP ${res.status})`);
        }

        const blob = await res.blob();
        const url = URL.createObjectURL(blob);
        const link = document.createElement('a');
        link.href = url;
        link.download = `channels-${formatTimestampForFilename()}.csv`;
        document.body.appendChild(link);
        link.click();
        document.body.removeChild(link);
        URL.revokeObjectURL(url);

        if (window.showSuccess) showSuccess('导出成功');
      } catch (err) {
        console.error('导出CSV失败', err);
        if (window.showError) showError(err.message || '导出失败');
      } finally {
        if (buttonEl) buttonEl.disabled = false;
      }
    }

    async function handleImportCSV(event, importBtn) {
      const input = event.target;
      if (!input.files || input.files.length === 0) {
        return;
      }

      const file = input.files[0];
      const formData = new FormData();
      formData.append('file', file);

      if (importBtn) importBtn.disabled = true;

      try {
        const res = await fetchWithAuth('/admin/channels/import', {
          method: 'POST',
          body: formData
        });

        const responseText = await res.text();
        let payload = null;
        if (responseText) {
          try {
            payload = JSON.parse(responseText);
          } catch (e) {
            payload = null;
          }
        }

        if (!res.ok) {
          const message = (payload && payload.error) || responseText || `导入失败 (HTTP ${res.status})`;
          throw new Error(message);
        }

        const summary = payload && payload.data ? payload.data : payload;
        if (summary) {
          // 基础导入信息
          let msg = `导入完成：新增 ${summary.created || 0}，更新 ${summary.updated || 0}，跳过 ${summary.skipped || 0}`;

          // Redis同步状态信息 (Integration: 新功能无缝集成)
          if (summary.redis_sync_enabled) {
            if (summary.redis_sync_success) {
              msg += `，已同步 ${summary.redis_synced_channels || 0} 个渠道到Redis`;
            } else {
              msg += '，Redis同步失败';
            }
          }

          if (window.showSuccess) showSuccess(msg);

          // 显示导入错误（如果有）
          if (summary.errors && summary.errors.length) {
            const preview = summary.errors.slice(0, 3).join('；');
            const extra = summary.errors.length > 3 ? ` 等${summary.errors.length}条记录` : '';
            if (window.showError) showError(`部分记录导入失败：${preview}${extra}`);
          }

          // 显示Redis同步错误（如果有）
          if (summary.redis_sync_enabled && !summary.redis_sync_success && summary.redis_sync_error) {
            if (window.showError) showError(`Redis同步失败：${summary.redis_sync_error}`);
          }
        } else if (window.showSuccess) {
          showSuccess('导入完成');
        }

        await loadChannels();
      } catch (err) {
        console.error('导入CSV失败', err);
        if (window.showError) showError(err.message || '导入失败');
      } finally {
        if (importBtn) importBtn.disabled = false;
        input.value = '';
      }
    }

    document.addEventListener('DOMContentLoaded', async () => {
      if (window.initTopbar) initTopbar('channels');
      setupFilterListeners();
      setupImportExport();
      setupKeyImportPreview(); // DRY原则：统一初始化所有功能模块

      // 初始化渠道类型（动态加载配置）
      await window.ChannelTypeManager.renderChannelTypeRadios('channelTypeRadios');
      await window.ChannelTypeManager.renderChannelTypeFilter('channelTypeFilter');

      await loadChannels();
      await loadChannelStats();
      highlightFromHash();
      window.addEventListener('hashchange', highlightFromHash);
    });

    function highlightFromHash() {
      const m = (location.hash || '').match(/^#channel-(\d+)$/);
      if (!m) return;
      const el = document.getElementById(`channel-${m[1]}`);
      if (!el) return;
      el.scrollIntoView({ behavior: 'smooth', block: 'center' });
      const prev = el.style.boxShadow;
      el.style.transition = 'box-shadow 0.3s ease, background 0.3s ease';
      el.style.boxShadow = '0 0 0 3px rgba(59,130,246,0.35), 0 10px 25px rgba(59,130,246,0.20)';
      el.style.background = 'rgba(59,130,246,0.06)';
      setTimeout(() => {
        el.style.boxShadow = prev || '';
        el.style.background = '';
      }, 1600);
    }

    // ESC键关闭模态框
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') {
        closeModal();
        closeDeleteModal();
        closeTestModal();
        closeKeyImportModal();
      }
    });

    // 测试渠道相关函数
    async function testChannel(id, name) {
      const channel = channels.find(c => c.id === id);
      if (!channel) return;

      testingChannelId = id;
      document.getElementById('testChannelName').textContent = name;

      // 填充模型选择下拉框
      const modelSelect = document.getElementById('testModelSelect');
      modelSelect.innerHTML = '';
      channel.models.forEach(model => {
        const option = document.createElement('option');
        option.value = model;
        option.textContent = model;
        modelSelect.appendChild(option);
      });

      // ✅ 修复：异步从后端获取 API Keys（2025-10 新架构：api_keys表独立存储）
      let apiKeys = [];
      try {
        const res = await fetchWithAuth(`/admin/channels/${id}/keys`);
        if (res.ok) {
          const data = await res.json();
          apiKeys = (data.success ? data.data : data) || [];
        }
      } catch (e) {
        console.error('获取API Keys失败', e);
      }

      // ✅ 修复：从 APIKey 对象数组提取实际的 key 字符串
      const keys = apiKeys.map(k => k.api_key || k);
      const keySelect = document.getElementById('testKeySelect');
      const keySelectGroup = document.getElementById('testKeySelectGroup');
      const batchTestBtn = document.getElementById('batchTestBtn');

      if (keys.length > 1) {
        // 多个 Key 时显示选择框和批量测试按钮
        keySelectGroup.style.display = 'block';
        batchTestBtn.style.display = 'inline-block';
        
        keySelect.innerHTML = '';
        const maxKeys = Math.min(keys.length, 10); // 限制显示前10个
        for (let i = 0; i < maxKeys; i++) {
          const option = document.createElement('option');
          option.value = i;
          option.textContent = `Key ${i + 1}: ${maskKey(keys[i])}`;
          keySelect.appendChild(option);
        }
        
        // 如果Key总数超过10个，添加提示
        if (keys.length > 10) {
          const hintOption = document.createElement('option');
          hintOption.disabled = true;
          hintOption.textContent = `... 还有 ${keys.length - 10} 个Key（使用批量测试）`;
          keySelect.appendChild(hintOption);
        }
      } else {
        // 单个 Key 时隐藏选择框和批量测试按钮
        keySelectGroup.style.display = 'none';
        batchTestBtn.style.display = 'none';
      }

      // 重置状态
      resetTestModal();

      // 动态渲染渠道类型下拉框（使用当前渠道的类型）
      const channelType = channel.channel_type || 'anthropic';
      await window.ChannelTypeManager.renderChannelTypeSelect('testChannelType', channelType);

      // 按用户选择是否启用流式请求（不对特定渠道强制）

      document.getElementById('testModal').classList.add('show');
    }

    function closeTestModal() {
      document.getElementById('testModal').classList.remove('show');
      testingChannelId = null;
    }

    function resetTestModal() {
      document.getElementById('testProgress').classList.remove('show');
      document.getElementById('batchTestProgress').style.display = 'none';
      document.getElementById('testResult').classList.remove('show', 'success', 'error');
      document.getElementById('runTestBtn').disabled = false;
      document.getElementById('batchTestBtn').disabled = false;
      // 重置内容输入框为默认值
      document.getElementById('testContentInput').value = 'test';
      // 重置渠道类型为默认值
      document.getElementById('testChannelType').value = 'anthropic';
      // 重置冷却时间为默认值
      document.getElementById('testCooldownMinutes').value = '5';
      // 重置并发数为默认值
      document.getElementById('testConcurrency').value = '10';
    }

    function updateTestURL() {
      // 当渠道类型改变时，可以在这里添加额外的逻辑
      // 目前只是为了保持接口一致性
    }

    async function runChannelTest() {
      if (!testingChannelId) return;

      const modelSelect = document.getElementById('testModelSelect');
      const contentInput = document.getElementById('testContentInput');
      const channelTypeSelect = document.getElementById('testChannelType');
      const keySelect = document.getElementById('testKeySelect');
      const streamCheckbox = document.getElementById('testStreamEnabled');
      const selectedModel = modelSelect.value;
      const testContent = contentInput.value.trim() || 'test'; // 默认为"test"
      const channelType = channelTypeSelect.value;
      // 由用户选择是否启用流式
      const streamEnabled = streamCheckbox.checked;

      if (!selectedModel) {
        if (window.showError) showError('请选择一个模型');
        return;
      }

      // 显示进度
      document.getElementById('testProgress').classList.add('show');
      document.getElementById('testResult').classList.remove('show');
      document.getElementById('runTestBtn').disabled = true;

      try {
        // 使用新的参数结构，支持渠道类型、自定义内容、流式选项和 key_index
        const testRequest = {
          model: selectedModel,
          max_tokens: 512,
          stream: streamEnabled,
          content: testContent,
          channel_type: channelType
        };

        // 如果显示了 Key 选择框，则添加 key_index 参数
        if (keySelect && keySelect.parentElement.style.display !== 'none') {
          testRequest.key_index = parseInt(keySelect.value) || 0;
        }

        const res = await fetchWithAuth(`/admin/channels/${testingChannelId}/test`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(testRequest)
        });

        if (!res.ok) {
          throw new Error('HTTP ' + res.status);
        }

        const result = await res.json();
        // 检查是否有嵌套的data字段（标准API响应格式）
        const testResult = result.data || result;
        
        // 如果测试失败，自动冷却对应的Key
        if (!testResult.success) {
          const cooldownMinutes = parseInt(document.getElementById('testCooldownMinutes').value) || 5;
          const keyIndex = (typeof testRequest !== 'undefined' && testRequest && testRequest.key_index !== undefined) ? testRequest.key_index : null;
          await setCooldownForKey(testingChannelId, keyIndex, cooldownMinutes);
        }
        
        displayTestResult(testResult);
      } catch (e) {
        console.error('测试失败', e);
        
        // 测试失败也自动冷却
        const cooldownMinutes = parseInt(document.getElementById('testCooldownMinutes').value) || 5;
        const keyIndex = (typeof testRequest !== 'undefined' && testRequest && testRequest.key_index !== undefined) ? testRequest.key_index : null;
        await setCooldownForKey(testingChannelId, keyIndex, cooldownMinutes);
        
        displayTestResult({
          success: false,
          error: '测试请求失败: ' + e.message
        });
      } finally {
        document.getElementById('testProgress').classList.remove('show');
        document.getElementById('runTestBtn').disabled = false;

        // 刷新渠道列表（更新冷却状态）
        await loadChannels();
      }
    }

    // 为指定Key设置冷却时间
    async function setCooldownForKey(channelId, keyIndex, minutes) {
      try {
        const durationMs = minutes * 60 * 1000;
        
        // 如果指定了keyIndex，则冷却特定Key；否则冷却整个渠道
        const endpoint = keyIndex !== null 
          ? `/admin/channels/${channelId}/keys/${keyIndex}/cooldown`
          : `/admin/channels/${channelId}/cooldown`;
        
        const res = await fetchWithAuth(endpoint, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ duration_ms: durationMs })
        });
        
        if (res.ok) {
          const msg = keyIndex !== null 
            ? `Key #${keyIndex + 1} 已冷却 ${minutes} 分钟`
            : `渠道已冷却 ${minutes} 分钟`;
          console.log(msg);
        } else {
          console.warn('设置冷却失败:', await res.text());
        }
      } catch (e) {
        console.error('设置冷却时出错:', e);
      }
    }

    // 批量测试所有Key（并发版本）
    async function runBatchTest() {
      if (!testingChannelId) return;

      const channel = channels.find(c => c.id === testingChannelId);
      if (!channel) return;

      // ✅ 修复：异步从后端获取 API Keys（2025-10 新架构：api_keys表独立存储）
      let apiKeys = [];
      try {
        const res = await fetchWithAuth(`/admin/channels/${testingChannelId}/keys`);
        if (res.ok) {
          const data = await res.json();
          apiKeys = (data.success ? data.data : data) || [];
        }
      } catch (e) {
        console.error('获取API Keys失败', e);
      }

      // ✅ 修复：从 APIKey 对象数组提取实际的 key 字符串
      const keys = apiKeys.map(k => k.api_key || k);
      if (keys.length === 0) {
        if (window.showError) showError('没有可用的API Key');
        return;
      }

      const modelSelect = document.getElementById('testModelSelect');
      const contentInput = document.getElementById('testContentInput');
      const channelTypeSelect = document.getElementById('testChannelType');
      const streamCheckbox = document.getElementById('testStreamEnabled');
      const cooldownInput = document.getElementById('testCooldownMinutes');
      const concurrencyInput = document.getElementById('testConcurrency');
      
      const selectedModel = modelSelect.value;
      const testContent = contentInput.value.trim() || 'test';
      const channelType = channelTypeSelect.value;
      // 由用户选择是否启用流式
      const streamEnabled = streamCheckbox.checked;
      const cooldownMinutes = parseInt(cooldownInput.value) || 5;
      const concurrency = Math.max(1, Math.min(50, parseInt(concurrencyInput.value) || 10)); // 限制1-50

      if (!selectedModel) {
        if (window.showError) showError('请选择一个模型');
        return;
      }

      // 禁用按钮
      document.getElementById('runTestBtn').disabled = true;
      document.getElementById('batchTestBtn').disabled = true;

      // 显示批量测试进度
      const progressDiv = document.getElementById('batchTestProgress');
      const counterSpan = document.getElementById('batchTestCounter');
      const progressBar = document.getElementById('batchTestProgressBar');
      const statusDiv = document.getElementById('batchTestStatus');
      
      progressDiv.style.display = 'block';
      document.getElementById('testResult').classList.remove('show');

      let successCount = 0;
      let failedCount = 0;
      const failedKeys = [];
      let completedCount = 0;

      // 更新进度的辅助函数
      const updateProgress = () => {
        const progress = (completedCount / keys.length * 100).toFixed(0);
        counterSpan.textContent = `${completedCount} / ${keys.length}`;
        progressBar.style.width = `${progress}%`;
        statusDiv.textContent = `已完成 ${completedCount} / ${keys.length}（并发数: ${concurrency}）`;
      };

      // 测试单个Key的函数
      const testSingleKey = async (keyIndex) => {
        try {
          const testRequest = {
            model: selectedModel,
            max_tokens: 512,
            stream: streamEnabled,
            content: testContent,
            channel_type: channelType,
            key_index: keyIndex
          };

          const res = await fetchWithAuth(`/admin/channels/${testingChannelId}/test`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(testRequest)
          });

          const result = await res.json();
          const testResult = result.data || result;

          if (testResult.success) {
            successCount++;
          } else {
            failedCount++;
            failedKeys.push({ index: keyIndex, key: maskKey(keys[keyIndex]), error: testResult.error });
            
            // 失败时自动冷却
            await setCooldownForKey(testingChannelId, keyIndex, cooldownMinutes);
          }
        } catch (e) {
          failedCount++;
          failedKeys.push({ index: keyIndex, key: maskKey(keys[keyIndex]), error: e.message });
          
          // 异常时也冷却
          await setCooldownForKey(testingChannelId, keyIndex, cooldownMinutes);
        } finally {
          completedCount++;
          updateProgress();
        }
      };

      // 分批并发测试
      const batches = [];
      for (let i = 0; i < keys.length; i += concurrency) {
        const batchIndexes = [];
        for (let j = i; j < Math.min(i + concurrency, keys.length); j++) {
          batchIndexes.push(j);
        }
        batches.push(batchIndexes);
      }

      // 初始化进度
      updateProgress();

      // 逐批执行（每批内并发）
      for (const batch of batches) {
        const batchPromises = batch.map(keyIndex => testSingleKey(keyIndex));
        await Promise.all(batchPromises);
      }

      // 显示批量测试结果
      displayBatchTestResult(successCount, failedCount, keys.length, failedKeys);

      // 重新启用按钮
      document.getElementById('runTestBtn').disabled = false;
      document.getElementById('batchTestBtn').disabled = false;
      
      // 刷新渠道列表（更新冷却状态）
      await loadChannels();
    }

    // 显示批量测试结果
    function displayBatchTestResult(successCount, failedCount, totalCount, failedKeys) {
      const testResultDiv = document.getElementById('testResult');
      const contentDiv = document.getElementById('testResultContent');
      const detailsDiv = document.getElementById('testResultDetails');
      const statusDiv = document.getElementById('batchTestStatus');

      testResultDiv.classList.remove('success', 'error');
      testResultDiv.classList.add('show');

      // 更新状态文本
      statusDiv.textContent = `完成！成功: ${successCount}, 失败: ${failedCount}`;

      if (failedCount === 0) {
        // 全部成功
        testResultDiv.classList.add('success');
        contentDiv.innerHTML = `
          <div style="display: flex; align-items: center; gap: 8px;">
            <span style="font-size: 18px;">✅</span>
            <strong>批量测试完成：全部 ${totalCount} 个Key测试成功</strong>
          </div>
        `;
        detailsDiv.innerHTML = '';
      } else if (successCount === 0) {
        // 全部失败
        testResultDiv.classList.add('error');
        contentDiv.innerHTML = `
          <div style="display: flex; align-items: center; gap: 8px;">
            <span style="font-size: 18px;">❌</span>
            <strong>批量测试完成：全部 ${totalCount} 个Key测试失败</strong>
          </div>
        `;
        
        let details = '<h4 style="margin-top: 12px; color: var(--error-600);">失败详情：</h4><ul style="margin: 8px 0; padding-left: 20px;">';
        failedKeys.forEach(({ index, key, error }) => {
          details += `<li style="margin: 4px 0;"><strong>Key #${index + 1}</strong> (${key}): ${escapeHtml(error)}</li>`;
        });
        details += '</ul><p style="color: var(--error-600); margin-top: 8px;">失败的Key已自动冷却</p>';
        detailsDiv.innerHTML = details;
      } else {
        // 部分成功
        testResultDiv.classList.add('success');
        contentDiv.innerHTML = `
          <div style="display: flex; align-items: center; gap: 8px;">
            <span style="font-size: 18px;">⚠️</span>
            <strong>批量测试完成：${successCount} 个成功，${failedCount} 个失败</strong>
          </div>
        `;
        
        let details = `<p style="color: var(--success-600);">✅ ${successCount} 个Key可用</p>`;
        details += '<h4 style="margin-top: 12px; color: var(--error-600);">失败详情：</h4><ul style="margin: 8px 0; padding-left: 20px;">';
        failedKeys.forEach(({ index, key, error }) => {
          details += `<li style="margin: 4px 0;"><strong>Key #${index + 1}</strong> (${key}): ${escapeHtml(error)}</li>`;
        });
        details += '</ul><p style="color: var(--error-600); margin-top: 8px;">失败的Key已自动冷却</p>';
        detailsDiv.innerHTML = details;
      }
    }

    function displayTestResult(result) {
      const testResultDiv = document.getElementById('testResult');
      const contentDiv = document.getElementById('testResultContent');
      const detailsDiv = document.getElementById('testResultDetails');

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
        
        // 显示响应文本（如果有的话）
        if (result.response_text) {
          details += `
            <div class="response-section">
              <h4>API 响应内容</h4>
              <div class="response-content">${escapeHtml(result.response_text)}</div>
            </div>
          `;
        }
        
        // 显示完整API响应
        if (result.api_response) {
          const responseId = 'api-response-' + Date.now();
          details += `
            <div class="response-section">
              <h4>完整 API 响应</h4>
              <button class="toggle-btn" onclick="toggleResponse('${responseId}')">显示/隐藏 JSON</button>
              <div id="${responseId}" class="response-content" style="display: none;">${escapeHtml(JSON.stringify(result.api_response, null, 2))}</div>
            </div>
          `;
        } else if (result.raw_response) {
          const rawId = 'raw-response-' + Date.now();
          details += `
            <div class="response-section">
              <h4>原始响应</h4>
              <button class="toggle-btn" onclick="toggleResponse('${rawId}')">显示/隐藏</button>
              <div id="${rawId}" class="response-content" style="display: none;">${escapeHtml(result.raw_response)}</div>
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
        
        let details = result.error || '未知错误';
        if (result.duration_ms) {
          details += `<br>响应时间: ${result.duration_ms}ms`;
        }
        if (result.status_code) {
          details += ` | 状态码: ${result.status_code}`;
        }
        
        // 显示完整错误响应
        if (result.api_error) {
          const errorId = 'api-error-' + Date.now();
          details += `
            <div class="response-section">
              <h4>完整错误响应</h4>
              <button class="toggle-btn" onclick="toggleResponse('${errorId}')">显示/隐藏 JSON</button>
              <div id="${errorId}" class="response-content" style="display: block;">${escapeHtml(JSON.stringify(result.api_error, null, 2))}</div>
            </div>
          `;
        }
        if (typeof result.raw_response !== 'undefined') {
          const rawId = 'raw-error-' + Date.now();
          details += `
            <div class="response-section">
              <h4>原始错误响应</h4>
              <button class="toggle-btn" onclick="toggleResponse('${rawId}')">显示/隐藏</button>
              <div id="${rawId}" class="response-content" style="display: block;">${escapeHtml(result.raw_response || '(无响应体)')}</div>
            </div>
          `;
        }
        if (result.response_headers) {
          const headersId = 'resp-headers-' + Date.now();
          details += `
            <div class="response-section">
              <h4>响应头</h4>
              <button class="toggle-btn" onclick="toggleResponse('${headersId}')">显示/隐藏</button>
              <div id="${headersId}" class="response-content" style="display: block;">${escapeHtml(JSON.stringify(result.response_headers, null, 2))}</div>
            </div>
          `;
        }
        
        detailsDiv.innerHTML = details;
      }
    }

    function toggleResponse(elementId) {
      const element = document.getElementById(elementId);
      if (element) {
        element.style.display = element.style.display === 'none' ? 'block' : 'none';
      }
    }

    // 内联Key表格管理（主表单内的表格显示）
    let inlineKeyTableData = [];
    let inlineKeyVisible = false; // 密码可见性状态
    let selectedKeyIndices = new Set(); // 选中的Key索引集合
    let currentKeyStatusFilter = 'all'; // 当前状态筛选：all/normal/cooldown

    // 统一Key解析函数（DRY原则）
    function parseKeys(input) {
      if (!input || !input.trim()) return [];

      // 支持逗号和换行分割
      const keys = input
        .split(/[,\n]/)
        .map(k => k.trim())
        .filter(k => k);

      // 去重
      return [...new Set(keys)];
    }

    // ============================================================
    // 虚拟滚动实现：优化大量Key时的渲染性能
    // ============================================================
    const VIRTUAL_SCROLL_CONFIG = {
      ROW_HEIGHT: 40,           // 每行高度（像素）
      BUFFER_SIZE: 5,           // 上下缓冲区行数（减少滚动时的闪烁）
      ENABLE_THRESHOLD: 50,     // 启用虚拟滚动的阈值（Key数量）
      CONTAINER_HEIGHT: 250     // 容器固定高度（像素）
    };

    let virtualScrollState = {
      enabled: false,
      scrollTop: 0,
      visibleStart: 0,
      visibleEnd: 0,
      rafId: null,
      filteredIndices: [] // 存储筛选后的索引列表（支持状态筛选）
    };

    // 虚拟滚动：计算可见范围（支持筛选）
    function calculateVisibleRange(totalItems) {
      const { ROW_HEIGHT, BUFFER_SIZE, CONTAINER_HEIGHT } = VIRTUAL_SCROLL_CONFIG;
      const { scrollTop } = virtualScrollState;

      const visibleRowCount = Math.ceil(CONTAINER_HEIGHT / ROW_HEIGHT);
      const startIndex = Math.floor(scrollTop / ROW_HEIGHT);

      // 添加上下缓冲区
      const visibleStart = Math.max(0, startIndex - BUFFER_SIZE);
      const visibleEnd = Math.min(
        totalItems,
        startIndex + visibleRowCount + BUFFER_SIZE
      );

      return { visibleStart, visibleEnd };
    }

    // 虚拟滚动：渲染可见行（支持筛选）
    function renderVirtualRows(tbody, visibleStart, visibleEnd, filteredIndices) {
      const { ROW_HEIGHT } = VIRTUAL_SCROLL_CONFIG;
      const totalHeight = filteredIndices.length * ROW_HEIGHT;

      // 清空tbody
      tbody.innerHTML = '';

      // 添加顶部占位元素（保持滚动条位置）
      if (visibleStart > 0) {
        const topSpacer = document.createElement('tr');
        topSpacer.innerHTML = `<td colspan="4" style="height: ${visibleStart * ROW_HEIGHT}px; padding: 0; border: none;"></td>`;
        tbody.appendChild(topSpacer);
      }

      // 渲染可见行（使用筛选后的索引）
      for (let i = visibleStart; i < visibleEnd; i++) {
        const actualIndex = filteredIndices[i]; // 获取实际Key索引
        const row = createKeyRow(actualIndex);
        tbody.appendChild(row);
      }

      // 添加底部占位元素
      if (visibleEnd < filteredIndices.length) {
        const bottomSpacer = document.createElement('tr');
        const bottomHeight = (filteredIndices.length - visibleEnd) * ROW_HEIGHT;
        bottomSpacer.innerHTML = `<td colspan="4" style="height: ${bottomHeight}px; padding: 0; border: none;"></td>`;
        tbody.appendChild(bottomSpacer);
      }
    }

    // 创建单行Key元素（提取公共逻辑，DRY原则）
    function createKeyRow(index) {
      const key = inlineKeyTableData[index];
      const row = document.createElement('tr');
      row.style.borderBottom = '1px solid var(--neutral-200)';
      row.style.height = VIRTUAL_SCROLL_CONFIG.ROW_HEIGHT + 'px';

      // 查找当前Key的冷却信息
      const keyCooldown = currentChannelKeyCooldowns.find(kc => kc.key_index === index);
      let cooldownHtml = '<span style="color: var(--success-600); font-size: 12px;">✓ 正常</span>';
      
      if (keyCooldown && keyCooldown.cooldown_remaining_ms > 0) {
        const cooldownText = humanizeMS(keyCooldown.cooldown_remaining_ms);
        cooldownHtml = `<span style="color: #dc2626; font-size: 12px; font-weight: 500; background: linear-gradient(135deg, #fee2e2 0%, #fecaca 100%); padding: 2px 8px; border-radius: 4px; border: 1px solid #fca5a5; white-space: nowrap;">⚠️ 冷却中·${cooldownText}</span>`;
      }

      const isSelected = selectedKeyIndices.has(index);

      row.innerHTML = `
        <td style="padding: 6px 10px;">
          <div style="display: flex; align-items: center; gap: 8px;">
            <input
              type="checkbox"
              class="key-checkbox"
              data-index="${index}"
              ${isSelected ? 'checked' : ''}
              onchange="toggleKeySelection(${index}, this.checked)"
              style="width: 16px; height: 16px; cursor: pointer; accent-color: var(--primary-500);"
            >
            <span style="color: var(--neutral-600); font-weight: 500; font-size: 13px;">${index + 1}</span>
          </div>
        </td>
        <td style="padding: 6px 10px;">
          <input
            type="${inlineKeyVisible ? 'text' : 'password'}"
            value="${escapeHtml(key)}"
            onchange="updateInlineKey(${index}, this.value)"
            class="inline-key-input"
            data-index="${index}"
            style="width: 100%; padding: 5px 8px; border: 1px solid var(--neutral-300); border-radius: 6px; font-family: 'Monaco', 'Menlo', 'Courier New', monospace; font-size: 13px; transition: all 0.2s;"
            onfocus="this.style.borderColor='var(--primary-500)'; this.style.boxShadow='0 0 0 3px rgba(59,130,246,0.1)'"
            onblur="this.style.borderColor='var(--neutral-300)'; this.style.boxShadow='none'"
          >
        </td>
        <td style="padding: 6px 10px;">
          ${cooldownHtml}
        </td>
        <td style="padding: 6px 10px; text-align: center;">
          <div style="display: flex; gap: 6px; justify-content: center;">
            <button
              type="button"
              onclick="testSingleKey(${index})"
              title="测试此Key"
              style="width: 28px; height: 28px; border-radius: 6px; border: 1px solid var(--neutral-200); background: white; color: var(--neutral-500); cursor: pointer; transition: all 0.2s; display: inline-flex; align-items: center; justify-content: center; padding: 0;"
              onmouseover="this.style.background='#eff6ff'; this.style.borderColor='#93c5fd'; this.style.color='#3b82f6'"
              onmouseout="this.style.background='white'; this.style.borderColor='var(--neutral-200)'; this.style.color='var(--neutral-500)'"
            >
              <svg width="12" height="12" viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg">
                <path d="M4 2L12 8L4 14V2Z" fill="currentColor"/>
              </svg>
            </button>
            <button
              type="button"
              onclick="deleteInlineKey(${index})"
              title="删除此Key"
              style="width: 28px; height: 28px; border-radius: 6px; border: 1px solid var(--neutral-200); background: white; color: var(--neutral-500); cursor: pointer; transition: all 0.2s; display: inline-flex; align-items: center; justify-content: center; padding: 0;"
              onmouseover="this.style.background='#fef2f2'; this.style.borderColor='#fca5a5'; this.style.color='#dc2626'"
              onmouseout="this.style.background='white'; this.style.borderColor='var(--neutral-200)'; this.style.color='var(--neutral-500)'"
            >
              <svg width="12" height="12" viewBox="0 0 14 14" fill="none" xmlns="http://www.w3.org/2000/svg">
                <path d="M5.5 2.5V1.5C5.5 1.22386 5.72386 1 6 1H8C8.27614 1 8.5 1.22386 8.5 1.5V2.5M2 3.5H12M3 3.5V11.5C3 12.0523 3.44772 12.5 4 12.5H10C10.5523 12.5 11 12.0523 11 11.5V3.5M5.5 6.5V9.5M8.5 6.5V9.5" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" stroke-linejoin="round"/>
              </svg>
            </button>
          </div>
        </td>
      `;

      return row;
    }

    // 虚拟滚动：处理滚动事件（使用requestAnimationFrame节流）
    function handleVirtualScroll(event) {
      const container = event.target;
      virtualScrollState.scrollTop = container.scrollTop;

      // 取消之前的渲染请求
      if (virtualScrollState.rafId) {
        cancelAnimationFrame(virtualScrollState.rafId);
      }

      // 使用requestAnimationFrame节流，优化性能
      virtualScrollState.rafId = requestAnimationFrame(() => {
        const { visibleStart, visibleEnd } = calculateVisibleRange(virtualScrollState.filteredIndices.length);

        // 仅当可见范围变化时才重新渲染
        if (visibleStart !== virtualScrollState.visibleStart ||
            visibleEnd !== virtualScrollState.visibleEnd) {
          virtualScrollState.visibleStart = visibleStart;
          virtualScrollState.visibleEnd = visibleEnd;

          const tbody = document.getElementById('inlineKeyTableBody');
          renderVirtualRows(tbody, visibleStart, visibleEnd, virtualScrollState.filteredIndices);
        }
      });
    }

    // 初始化虚拟滚动（绑定事件监听器）
    function initVirtualScroll() {
      const tableContainer = document.querySelector('#inlineKeyTableBody').closest('div[style*="max-height"]');
      if (tableContainer) {
        // 移除旧的监听器（避免重复绑定）
        tableContainer.removeEventListener('scroll', handleVirtualScroll);
        tableContainer.addEventListener('scroll', handleVirtualScroll, { passive: true });
      }
    }

    // 清理虚拟滚动（禁用时）
    function cleanupVirtualScroll() {
      const tableContainer = document.querySelector('#inlineKeyTableBody').closest('div[style*="max-height"]');
      if (tableContainer) {
        tableContainer.removeEventListener('scroll', handleVirtualScroll);
      }
      if (virtualScrollState.rafId) {
        cancelAnimationFrame(virtualScrollState.rafId);
        virtualScrollState.rafId = null;
      }
    }

    // 渲染内联Key表格（主入口函数）
    function renderInlineKeyTable() {
      const tbody = document.getElementById('inlineKeyTableBody');
      const keyCount = document.getElementById('inlineKeyCount');
      const virtualScrollHint = document.getElementById('virtualScrollHint');

      tbody.innerHTML = '';
      keyCount.textContent = inlineKeyTableData.length;

      // 同步到隐藏input（用于表单验证）
      const hiddenInput = document.getElementById('channelApiKey');
      hiddenInput.value = inlineKeyTableData.join(',');

      // 空状态
      if (inlineKeyTableData.length === 0) {
        tbody.innerHTML = `
          <tr>
            <td colspan="4" style="padding: 30px; text-align: center; color: var(--neutral-500); font-size: 14px;">
              暂无API Key，点击"添加"或"导入"按钮添加
            </td>
          </tr>
        `;
        cleanupVirtualScroll();
        virtualScrollState.enabled = false;
        if (virtualScrollHint) virtualScrollHint.style.display = 'none';
        return;
      }

      // 获取要显示的Keys（考虑状态筛选）
      const visibleIndices = getVisibleKeyIndices();

      // 筛选后为空
      if (visibleIndices.length === 0) {
        tbody.innerHTML = `
          <tr>
            <td colspan="4" style="padding: 30px; text-align: center; color: var(--neutral-500); font-size: 14px;">
              ${currentKeyStatusFilter === 'normal' ? '当前无正常状态的Key' : '当前无冷却中的Key'}
            </td>
          </tr>
        `;
        cleanupVirtualScroll();
        virtualScrollState.enabled = false;
        if (virtualScrollHint) virtualScrollHint.style.display = 'none';
        return;
      }

      // 统一使用虚拟滚动（支持少量和大量Keys）
      virtualScrollState.enabled = true;
      // ✅ 修复：不要重置scrollTop，保持当前滚动位置
      // 只在第一次启用虚拟滚动或筛选变化时重置
      if (!virtualScrollState.filteredIndices || 
          virtualScrollState.filteredIndices.length !== visibleIndices.length) {
        virtualScrollState.scrollTop = 0;
      }
      virtualScrollState.filteredIndices = visibleIndices; // 保存筛选后的索引

      const { visibleStart, visibleEnd } = calculateVisibleRange(visibleIndices.length);
      virtualScrollState.visibleStart = visibleStart;
      virtualScrollState.visibleEnd = visibleEnd;

      // 渲染可见行
      renderVirtualRows(tbody, visibleStart, visibleEnd, visibleIndices);

      // 初始化虚拟滚动事件监听
      initVirtualScroll();

      // 更新虚拟滚动提示（显示总Key数）
      if (virtualScrollHint) {
        const showHint = visibleIndices.length >= VIRTUAL_SCROLL_CONFIG.ENABLE_THRESHOLD;
        virtualScrollHint.style.display = showHint ? 'inline' : 'none';
      }

      // 更新全选checkbox和删除按钮状态
      updateSelectAllCheckbox();
      updateBatchDeleteButton();
    }

    // 遮罩Key显示（保留前后各4个字符）
    function maskKey(key) {
      if (key.length <= 8) return '***';
      return key.slice(0, 4) + '***' + key.slice(-4);
    }

    // 切换密码可见性（虚拟滚动优化：直接重新渲染）
    function toggleInlineKeyVisibility() {
      inlineKeyVisible = !inlineKeyVisible;
      const eyeIcon = document.getElementById('inlineEyeIcon');
      const eyeOffIcon = document.getElementById('inlineEyeOffIcon');

      if (inlineKeyVisible) {
        eyeIcon.style.display = 'none';
        eyeOffIcon.style.display = 'block';
      } else {
        eyeIcon.style.display = 'block';
        eyeOffIcon.style.display = 'none';
      }

      // 重新渲染表格以应用可见性变化
      renderInlineKeyTable();
    }

  
    // 更新Key值（虚拟滚动优化：只更新数据，不重新渲染）
    function updateInlineKey(index, value) {
      inlineKeyTableData[index] = value.trim();
      
      // 同步到隐藏input（用于表单验证）
      const hiddenInput = document.getElementById('channelApiKey');
      if (hiddenInput) {
        hiddenInput.value = inlineKeyTableData.join(',');
      }
      
      // 无需重新渲染整个表格，输入框已经更新了值
    }

    // 删除Key（虚拟滚动优化：保持滚动位置）
    // 测试单个Key
    async function testSingleKey(keyIndex) {
      if (!editingChannelId) {
        alert('无法获取渠道ID');
        return;
      }

      // 获取模型列表
      const modelsInput = document.getElementById('channelModels');
      if (!modelsInput || !modelsInput.value.trim()) {
        alert('请先配置支持的模型列表');
        return;
      }

      const models = modelsInput.value.split(',').map(m => m.trim()).filter(m => m);
      if (models.length === 0) {
        alert('模型列表为空，请先配置支持的模型');
        return;
      }

      const firstModel = models[0];
      const apiKey = inlineKeyTableData[keyIndex];

      if (!apiKey || !apiKey.trim()) {
        alert('API Key为空，无法测试');
        return;
      }

      // 获取渠道类型
      const channelTypeRadios = document.querySelectorAll('input[name="channelType"]');
      let channelType = 'anthropic';
      for (const radio of channelTypeRadios) {
        if (radio.checked) {
          channelType = radio.value.toLowerCase();
          break;
        }
      }

      // 显示测试中状态
      const testButton = event.target.closest('button');
      const originalHTML = testButton.innerHTML;
      testButton.disabled = true;
      testButton.innerHTML = '<span style="font-size: 10px;">⏳</span>';

      try {
        const res = await fetchWithAuth(`/admin/channels/${editingChannelId}/test`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            model: firstModel,
            max_tokens: 512,
            stream: true,
            content: 'test',
            channel_type: channelType,
            key_index: keyIndex
          })
        });

        if (!res.ok) {
          throw new Error('HTTP ' + res.status);
        }

        const result = await res.json();
        const testResult = result.data || result;

        // 刷新冷却状态
        await refreshKeyCooldownStatus();

        // 显示测试结果
        if (testResult.success) {
          showToast(`✅ Key #${keyIndex + 1} 测试成功`, 'success');
        } else {
          const errorMsg = testResult.error || '测试失败';
          showToast(`❌ Key #${keyIndex + 1} 测试失败: ${errorMsg}`, 'error');
        }
      } catch (e) {
        console.error('测试失败', e);
        showToast(`❌ Key #${keyIndex + 1} 测试请求失败: ${e.message}`, 'error');
      } finally {
        testButton.disabled = false;
        testButton.innerHTML = originalHTML;
      }
    }

    // 刷新Key冷却状态
    async function refreshKeyCooldownStatus() {
      if (!editingChannelId) return;

      try {
        const res = await fetchWithAuth(`/admin/channels/${editingChannelId}/keys`);
        if (res.ok) {
          const data = await res.json();
          const apiKeys = (data.success ? data.data : data) || [];

          // ✅ 修复：同步更新Key数据
          inlineKeyTableData = apiKeys.map(k => k.api_key || k);
          if (inlineKeyTableData.length === 0) {
            inlineKeyTableData = [''];
          }

          // 更新冷却状态
          const now = Date.now();
          currentChannelKeyCooldowns = apiKeys.map((apiKey, index) => {
            const cooldownUntilMs = (apiKey.cooldown_until || 0) * 1000;
            const remainingMs = Math.max(0, cooldownUntilMs - now);
            return {
              key_index: index,
              cooldown_remaining_ms: remainingMs
            };
          });

          // ✅ 修复：保存虚拟滚动位置
          const tableContainer = document.querySelector('#inlineKeyTableBody').closest('div[style*="max-height"]');
          const savedScrollTop = tableContainer ? tableContainer.scrollTop : 0;

          // 重新渲染表格以显示最新的冷却状态
          renderInlineKeyTable();

          // ✅ 修复：恢复虚拟滚动位置
          if (tableContainer && virtualScrollState.enabled) {
            // 使用setTimeout确保DOM更新完成后再恢复滚动位置
            setTimeout(() => {
              tableContainer.scrollTop = savedScrollTop;
              virtualScrollState.scrollTop = savedScrollTop;
              // 触发一次滚动事件以更新虚拟滚动的可见范围
              handleVirtualScroll({ target: tableContainer });
            }, 0);
          }
        }
      } catch (e) {
        console.error('刷新冷却状态失败', e);
      }
    }

    // 显示Toast提示
    function showToast(message, type = 'info') {
      // 创建toast元素
      const toast = document.createElement('div');
      toast.textContent = message;

      // ✅ 修复：检测是否在编辑对话框中
      const channelModal = document.getElementById('channelModal');
      const isInChannelModal = channelModal && channelModal.classList.contains('show');

      if (isInChannelModal) {
        // 在对话框底部显示toast
        toast.style.cssText = `
          position: absolute;
          bottom: 20px;
          left: 50%;
          transform: translateX(-50%);
          padding: 12px 20px;
          border-radius: 8px;
          font-size: 14px;
          font-weight: 500;
          z-index: 10000;
          animation: slideIn 0.3s ease-out;
          box-shadow: 0 4px 12px rgba(0,0,0,0.15);
          max-width: 400px;
          word-wrap: break-word;
        `;
      } else {
        // 页面固定位置显示toast
        toast.style.cssText = `
          position: fixed;
          top: 80px;
          right: 20px;
          padding: 12px 20px;
          border-radius: 8px;
          font-size: 14px;
          font-weight: 500;
          z-index: 10000;
          animation: slideIn 0.3s ease-out;
          box-shadow: 0 4px 12px rgba(0,0,0,0.15);
          max-width: 400px;
          word-wrap: break-word;
        `;
      }

      if (type === 'success') {
        toast.style.background = 'linear-gradient(135deg, #10b981 0%, #059669 100%)';
        toast.style.color = 'white';
      } else if (type === 'error') {
        toast.style.background = 'linear-gradient(135deg, #ef4444 0%, #dc2626 100%)';
        toast.style.color = 'white';
      } else {
        toast.style.background = 'linear-gradient(135deg, #3b82f6 0%, #2563eb 100%)';
        toast.style.color = 'white';
      }

      if (isInChannelModal) {
        const modalContent = channelModal.querySelector('.modal-content');
        // 确保modal-content支持absolute定位
        if (modalContent.style.position !== 'relative') {
          modalContent.style.position = 'relative';
        }
        modalContent.appendChild(toast);

        // 3秒后自动移除
        setTimeout(() => {
          toast.style.animation = 'slideOut 0.3s ease-in';
          setTimeout(() => {
            if (toast.parentNode === modalContent) {
              modalContent.removeChild(toast);
            }
          }, 300);
        }, 3000);
      } else {
        document.body.appendChild(toast);

        // 3秒后自动移除
        setTimeout(() => {
          toast.style.animation = 'slideOut 0.3s ease-in';
          setTimeout(() => {
            if (toast.parentNode === document.body) {
              document.body.removeChild(toast);
            }
          }, 300);
        }, 3000);
      }
    }

    function deleteInlineKey(index) {
      if (inlineKeyTableData.length === 1) {
        alert('至少需要保留一个API Key');
        return;
      }

      if (confirm(`确定要删除第 ${index + 1} 个Key吗？`)) {
        // 如果启用了虚拟滚动，先保存滚动位置
        const tableContainer = document.querySelector('#inlineKeyTableBody').closest('div[style*="max-height"]');
        const scrollTop = tableContainer ? tableContainer.scrollTop : 0;

        inlineKeyTableData.splice(index, 1);

        // 更新冷却信息的索引（因为删除了一个Key）
        currentChannelKeyCooldowns = currentChannelKeyCooldowns
          .filter(kc => kc.key_index !== index)
          .map(kc => kc.key_index > index ? { ...kc, key_index: kc.key_index - 1 } : kc);

        // 清除选中状态（删除后索引会变化）
        selectedKeyIndices.clear();
        updateBatchDeleteButton();

        renderInlineKeyTable();

        // 恢复滚动位置
        setTimeout(() => {
          if (tableContainer) {
            tableContainer.scrollTop = Math.min(scrollTop, tableContainer.scrollHeight - tableContainer.clientHeight);
          }
        }, 50);
      }
    }

    // ============================================================
    // 批量选择和删除功能
    // ============================================================

    // 切换单个Key的选中状态
    function toggleKeySelection(index, checked) {
      if (checked) {
        selectedKeyIndices.add(index);
      } else {
        selectedKeyIndices.delete(index);
      }
      updateBatchDeleteButton();
      updateSelectAllCheckbox();
    }

    // 全选/取消全选
    function toggleSelectAllKeys(checked) {
      selectedKeyIndices.clear();

      if (checked) {
        // 获取当前可见的Keys（考虑筛选）
        const visibleIndices = getVisibleKeyIndices();
        visibleIndices.forEach(index => selectedKeyIndices.add(index));
      }

      updateBatchDeleteButton();
      renderInlineKeyTable(); // 重新渲染以更新checkbox状态
    }

    // 更新批量删除按钮状态
    function updateBatchDeleteButton() {
      const btn = document.getElementById('batchDeleteKeysBtn');
      const count = selectedKeyIndices.size;

      if (count > 0) {
        btn.disabled = false;
        btn.textContent = `删除选中 (${count})`;
        btn.style.cursor = 'pointer';
        btn.style.background = 'linear-gradient(135deg, #fef2f2 0%, #fecaca 100%)';
        btn.style.borderColor = '#fca5a5';
        btn.style.color = '#dc2626';
        btn.style.fontWeight = '600';
      } else {
        btn.disabled = true;
        btn.textContent = '删除选中';
        btn.style.cursor = 'not-allowed';
        btn.style.background = 'white';
        btn.style.borderColor = 'var(--neutral-300)';
        btn.style.color = 'var(--neutral-500)';
        btn.style.fontWeight = '500';
      }
    }

    // 更新全选checkbox状态
    function updateSelectAllCheckbox() {
      const checkbox = document.getElementById('selectAllKeys');
      if (!checkbox) return;

      const visibleIndices = getVisibleKeyIndices();
      const allSelected = visibleIndices.length > 0 &&
                         visibleIndices.every(index => selectedKeyIndices.has(index));

      checkbox.checked = allSelected;
      checkbox.indeterminate = !allSelected &&
                               visibleIndices.some(index => selectedKeyIndices.has(index));
    }

    // 批量删除选中的Keys
    function batchDeleteSelectedKeys() {
      const count = selectedKeyIndices.size;
      if (count === 0) return;

      if (inlineKeyTableData.length - count < 1) {
        alert('至少需要保留一个API Key');
        return;
      }

      if (!confirm(`确定要删除选中的 ${count} 个Key吗？`)) {
        return;
      }

      // 保存滚动位置
      const tableContainer = document.querySelector('#inlineKeyTableBody').closest('div[style*="max-height"]');
      const scrollTop = tableContainer ? tableContainer.scrollTop : 0;

      // 获取要删除的索引（降序排序，从后往前删除避免索引变化问题）
      const indicesToDelete = Array.from(selectedKeyIndices).sort((a, b) => b - a);

      // 删除Keys
      indicesToDelete.forEach(index => {
        inlineKeyTableData.splice(index, 1);

        // 更新冷却信息
        currentChannelKeyCooldowns = currentChannelKeyCooldowns
          .filter(kc => kc.key_index !== index)
          .map(kc => kc.key_index > index ? { ...kc, key_index: kc.key_index - 1 } : kc);
      });

      // 清空选中状态
      selectedKeyIndices.clear();
      updateBatchDeleteButton();

      renderInlineKeyTable();

      // 恢复滚动位置
      setTimeout(() => {
        if (tableContainer) {
          tableContainer.scrollTop = Math.min(scrollTop, tableContainer.scrollHeight - tableContainer.clientHeight);
        }
      }, 50);
    }

    // ============================================================
    // 状态筛选功能
    // ============================================================

    // 根据状态筛选Keys
    function filterKeysByStatus(status) {
      currentKeyStatusFilter = status;
      renderInlineKeyTable();

      // 重置全选checkbox
      updateSelectAllCheckbox();
    }

    // 获取当前可见的Key索引（考虑筛选）
    function getVisibleKeyIndices() {
      if (currentKeyStatusFilter === 'all') {
        return inlineKeyTableData.map((_, index) => index);
      }

      return inlineKeyTableData
        .map((_, index) => {
          const keyCooldown = currentChannelKeyCooldowns.find(kc => kc.key_index === index);
          const isCoolingDown = keyCooldown && keyCooldown.cooldown_remaining_ms > 0;

          if (currentKeyStatusFilter === 'normal' && !isCoolingDown) {
            return index;
          }
          if (currentKeyStatusFilter === 'cooldown' && isCoolingDown) {
            return index;
          }
          return null;
        })
        .filter(index => index !== null);
    }

    // 检查Key是否应该显示（根据筛选条件）
    function shouldShowKey(index) {
      if (currentKeyStatusFilter === 'all') {
        return true;
      }

      const keyCooldown = currentChannelKeyCooldowns.find(kc => kc.key_index === index);
      const isCoolingDown = keyCooldown && keyCooldown.cooldown_remaining_ms > 0;

      if (currentKeyStatusFilter === 'normal') {
        return !isCoolingDown;
      }
      if (currentKeyStatusFilter === 'cooldown') {
        return isCoolingDown;
      }

      return true;
    }

    // 打开内联导入模态框
    function openInlineKeyImport() {
      // 复用原有的导入模态框
      openKeyImportModal();
    }

    // 内联导入确认（替换原confirmKeyImport）
    function confirmInlineKeyImport() {
      const textarea = document.getElementById('keyImportTextarea');
      const input = textarea.value.trim();

      if (!input) {
        alert('请输入至少一个API Key');
        return;
      }

      const newKeys = parseKeys(input);

      if (newKeys.length === 0) {
        alert('未能解析到有效的API Key，请检查格式');
        return;
      }

      // 添加到内联表格（去重）
      const existingKeys = new Set(inlineKeyTableData);
      let addedCount = 0;

      newKeys.forEach(key => {
        if (!existingKeys.has(key)) {
          inlineKeyTableData.push(key);
          existingKeys.add(key);
          addedCount++;
        }
      });

      closeKeyImportModal();
      renderInlineKeyTable();

      showToast(`成功导入 ${addedCount} 个新Key${newKeys.length - addedCount > 0 ? `，${newKeys.length - addedCount} 个重复已忽略` : ''}`);
    }

    // Key导入模态框函数
    function openKeyImportModal() {
      // 重置输入框
      document.getElementById('keyImportTextarea').value = '';
      document.getElementById('keyImportPreview').style.display = 'none';

      // 显示模态框
      document.getElementById('keyImportModal').classList.add('show');

      // 聚焦到文本框
      setTimeout(() => document.getElementById('keyImportTextarea').focus(), 100);
    }

    function closeKeyImportModal() {
      document.getElementById('keyImportModal').classList.remove('show');
    }

    // 实时预览导入的Key数量（DRY原则：提取为独立函数，由统一的DOMContentLoaded调用）
    function setupKeyImportPreview() {
      const textarea = document.getElementById('keyImportTextarea');
      if (!textarea) return;

      textarea.addEventListener('input', () => {
        const input = textarea.value.trim();
        const preview = document.getElementById('keyImportPreview');
        const countSpan = document.getElementById('keyImportCount');

        if (input) {
          const keys = parseKeys(input);
          if (keys.length > 0) {
            countSpan.textContent = keys.length;
            preview.style.display = 'block';
          } else {
            preview.style.display = 'none';
          }
        } else {
          preview.style.display = 'none';
        }
      });
    }

    // ===================== 模型重定向表格管理 =====================

    // 添加重定向行
    function addRedirectRow() {
      redirectTableData.push({ from: '', to: '' });
      renderRedirectTable();
      
      // 聚焦到最后一行的请求模型输入框
      setTimeout(() => {
        const tbody = document.getElementById('redirectTableBody');
        const lastRow = tbody.lastElementChild;
        if (lastRow) {
          const firstInput = lastRow.querySelector('input');
          if (firstInput) firstInput.focus();
        }
      }, 50);
    }

    // 删除重定向行
    function deleteRedirectRow(index) {
      redirectTableData.splice(index, 1);
      renderRedirectTable();
    }

    // 更新重定向行数据
    function updateRedirectRow(index, field, value) {
      if (redirectTableData[index]) {
        redirectTableData[index][field] = value.trim();
      }
    }

    // 渲染重定向表格
    function renderRedirectTable() {
      const tbody = document.getElementById('redirectTableBody');
      const countSpan = document.getElementById('redirectCount');
      
      // 更新计数
      const validCount = redirectTableData.filter(r => r.from && r.to).length;
      countSpan.textContent = validCount;
      
      if (redirectTableData.length === 0) {
        tbody.innerHTML = '<tr><td colspan="3" style="padding: 20px; text-align: center; color: var(--neutral-500);">暂无重定向规则，点击"添加"按钮创建</td></tr>';
        return;
      }
      
      tbody.innerHTML = redirectTableData.map((redirect, index) => `
        <tr style="border-bottom: 1px solid var(--neutral-200);">
          <td style="padding: 8px 12px;">
            <input
              type="text"
              value="${escapeHtml(redirect.from || '')}"
              placeholder="claude-3-opus-20240229"
              onchange="updateRedirectRow(${index}, 'from', this.value)"
              style="width: 100%; padding: 6px 10px; border: 1px solid var(--neutral-300); border-radius: 6px; font-size: 13px; font-family: 'Monaco', 'Menlo', 'Courier New', monospace;"
            >
          </td>
          <td style="padding: 8px 12px;">
            <input
              type="text"
              value="${escapeHtml(redirect.to || '')}"
              placeholder="claude-3-5-sonnet-20241022"
              onchange="updateRedirectRow(${index}, 'to', this.value)"
              style="width: 100%; padding: 6px 10px; border: 1px solid var(--neutral-300); border-radius: 6px; font-size: 13px; font-family: 'Monaco', 'Menlo', 'Courier New', monospace;"
            >
          </td>
          <td style="padding: 8px 12px; text-align: center;">
            <button
              type="button"
              onclick="deleteRedirectRow(${index})"
              style="padding: 4px 8px; border-radius: 6px; border: 1px solid var(--error-300); background: white; color: var(--error-600); cursor: pointer; font-size: 12px; transition: all 0.2s;"
              onmouseover="this.style.background='var(--error-50)'; this.style.borderColor='var(--error-500)';"
              onmouseout="this.style.background='white'; this.style.borderColor='var(--error-300)';"
              title="删除此规则"
            >
              删除
            </button>
          </td>
        </tr>
      `).join('');
    }

    // 将重定向表格数据转换为JSON对象
    function redirectTableToJSON() {
      const result = {};
      redirectTableData.forEach(redirect => {
        if (redirect.from && redirect.to) {
          result[redirect.from] = redirect.to;
        }
      });
      return result;
    }

    // 将JSON对象转换为重定向表格数据
    function jsonToRedirectTable(json) {
      if (!json || typeof json !== 'object') return [];
      return Object.entries(json).map(([from, to]) => ({ from, to }));
    }

    // ===================== 结束：模型重定向表格管理 =====================

    // ===================== 模型获取与清除功能 =====================

    // 从API获取模型列表（根据渠道类型调用标准接口）
    async function fetchModelsFromAPI() {
      const isExistingChannel = Boolean(editingChannelId);
      let endpoint = '';
      let fetchOptions;

      if (isExistingChannel) {
        endpoint = `/admin/channels/${editingChannelId}/models/fetch`;
      } else {
        const channelUrl = document.getElementById('channelUrl').value.trim();
        const channelType = document.querySelector('input[name="channelType"]:checked')?.value || 'anthropic';
        const firstValidKey = inlineKeyTableData
          .map(key => (key || '').trim())
          .filter(Boolean)[0];

        if (!channelUrl) {
          if (window.showError) {
            showError('请先填写API URL');
          } else {
            alert('请先填写API URL');
          }
          return;
        }

        if (!firstValidKey) {
          if (window.showError) {
            showError('请至少添加一个API Key');
          } else {
            alert('请至少添加一个API Key');
          }
          return;
        }

        endpoint = '/admin/channels/models/fetch';
        fetchOptions = {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            channel_type: channelType,
            url: channelUrl,
            api_key: firstValidKey
          })
        };
      }

      // 显示加载状态
      const modelsTextarea = document.getElementById('channelModels');
      const originalValue = modelsTextarea.value;
      const originalPlaceholder = modelsTextarea.placeholder;

      modelsTextarea.disabled = true;
      modelsTextarea.placeholder = '正在获取模型列表...';

      try {
        const res = fetchOptions
          ? await fetchWithAuth(endpoint, fetchOptions)
          : await fetchWithAuth(endpoint);

        if (!res.ok) {
          const errorData = await res.json().catch(() => ({}));
          throw new Error(errorData.error || `HTTP ${res.status}`);
        }

        const response = await res.json();
        const data = response.data || response;

        if (!data.models || data.models.length === 0) {
          throw new Error('未获取到任何模型');
        }

        // 合并现有模型和新获取的模型（去重）
        const existingModels = originalValue.split(',').map(m => m.trim()).filter(m => m);
        const allModels = [...new Set([...existingModels, ...data.models])];

        // 更新textarea
        modelsTextarea.value = allModels.join(',');

        // 显示成功提示
        const source = data.source === 'api' ? '从API获取' : '预定义列表';
        if (window.showSuccess) {
          showSuccess(`成功获取 ${data.models.length} 个模型 (${source})`);
        } else {
          alert(`成功获取 ${data.models.length} 个模型 (${source})`);
        }

      } catch (error) {
        console.error('获取模型列表失败', error);

        // 恢复原值
        modelsTextarea.value = originalValue;

        if (window.showError) {
          showError('获取模型列表失败: ' + error.message);
        } else {
          alert('获取模型列表失败: ' + error.message);
        }
      } finally {
        modelsTextarea.disabled = false;
        modelsTextarea.placeholder = originalPlaceholder;
      }
    }

    // 清除所有模型
    function clearAllModels() {
      if (confirm('确定要清除所有模型吗？此操作不可恢复！')) {
        const modelsTextarea = document.getElementById('channelModels');
        modelsTextarea.value = '';
        modelsTextarea.focus();
      }
    }

    // ===================== 结束：模型获取与清除功能 =====================

    function escapeHtml(text) {
      const div = document.createElement('div');
      div.textContent = text;
      return div.innerHTML;
    }

    function showToast(message) {
      // 简单的提示框实现
      const toast = document.createElement('div');
      toast.textContent = message;
      toast.style.cssText = `
        position: fixed;
        top: 20px;
        right: 20px;
        background: var(--success-color);
        color: white;
        padding: 12px 20px;
        border-radius: 6px;
        box-shadow: 0 4px 12px rgba(0,0,0,0.15);
        z-index: 10000;
        animation: slideIn 0.3s ease-out;
      `;
      document.body.appendChild(toast);

      setTimeout(() => {
        toast.style.opacity = '0';
        toast.style.transition = 'opacity 0.3s';
        setTimeout(() => toast.remove(), 300);
      }, 2000);
    }
