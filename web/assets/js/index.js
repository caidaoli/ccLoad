    // 统计数据管理
    let statsData = {
      total_requests: 0,
      success_requests: 0,
      error_requests: 0,
      active_channels: 0,
      active_models: 0
    };

    // 加载统计数据
    async function loadStats() {
      try {
        // 添加加载状态
        document.querySelectorAll('.metric-number').forEach(el => {
          el.classList.add('animate-pulse');
        });

        const response = await fetch('/public/summary?hours=24');
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        
        const responseData = await response.json();
        statsData = responseData.success ? (responseData.data || responseData) : responseData;
        updateStatsDisplay();
        
      } catch (error) {
        console.error('Failed to load stats:', error);
        showError('无法加载统计数据');
      } finally {
        // 移除加载状态
        document.querySelectorAll('.metric-number').forEach(el => {
          el.classList.remove('animate-pulse');
        });
      }
    }

    // 更新统计显示
    function updateStatsDisplay() {
      const successRate = statsData.total_requests > 0
        ? ((statsData.success_requests / statsData.total_requests) * 100).toFixed(1)
        : '0.0';

      // 更新总体数字显示
      document.getElementById('total-requests').textContent = formatNumber(statsData.total_requests || 0);
      document.getElementById('success-requests').textContent = formatNumber(statsData.success_requests || 0);
      document.getElementById('error-requests').textContent = formatNumber(statsData.error_requests || 0);
      document.getElementById('success-rate').textContent = successRate + '%';

      // 更新按渠道类型统计
      if (statsData.by_type) {
        updateTypeStats('anthropic', statsData.by_type.anthropic);
        updateTypeStats('codex', statsData.by_type.codex);
        updateTypeStats('openai', statsData.by_type.openai);
        updateTypeStats('gemini', statsData.by_type.gemini);
      }
    }

    // 更新单个渠道类型的统计
    function updateTypeStats(type, data) {
      // 始终显示所有卡片，保持界面完整性
      const card = document.getElementById(`type-${type}-card`);
      if (card) card.style.display = 'block';

      // 如果没有数据，显示默认值
      const totalRequests = data ? (data.total_requests || 0) : 0;
      const successRequests = data ? (data.success_requests || 0) : 0;
      const errorRequests = data ? (data.error_requests || 0) : 0;

      const successRate = totalRequests > 0
        ? ((successRequests / totalRequests) * 100).toFixed(1)
        : '0.0';

      // 更新基础统计（总请求、成功、失败、成功率）
      document.getElementById(`type-${type}-requests`).textContent = formatNumber(totalRequests);
      document.getElementById(`type-${type}-success`).textContent = formatNumber(successRequests);
      document.getElementById(`type-${type}-error`).textContent = formatNumber(errorRequests);
      document.getElementById(`type-${type}-rate`).textContent = successRate + '%';

      // 所有渠道类型的Token和成本统计
      const inputTokens = data ? (data.total_input_tokens || 0) : 0;
      const outputTokens = data ? (data.total_output_tokens || 0) : 0;
      const totalCost = data ? (data.total_cost || 0) : 0;

      document.getElementById(`type-${type}-input`).textContent = formatNumber(inputTokens);
      document.getElementById(`type-${type}-output`).textContent = formatNumber(outputTokens);
      document.getElementById(`type-${type}-cost`).textContent = formatCost(totalCost);

      // Claude和Codex类型的缓存统计
      if (type === 'anthropic' || type === 'codex') {
        const cacheReadTokens = data ? (data.total_cache_read_tokens || 0) : 0;
        const cacheCreateTokens = data ? (data.total_cache_creation_tokens || 0) : 0;
        document.getElementById(`type-${type}-cache-read`).textContent = formatNumber(cacheReadTokens);
        document.getElementById(`type-${type}-cache-create`).textContent = formatNumber(cacheCreateTokens);
      }
    }

    // 格式化成本（复用stats.html的逻辑）
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

    // 格式化数字显示
    function formatNumber(num) {
      if (num >= 1000000) return (num / 1000000).toFixed(1) + 'M';
      if (num >= 1000) return (num / 1000).toFixed(1) + 'K';
      return num.toString();
    }

    // 数字滚动动画
    function animateCountUp(elementId, target, duration = 1000) {
      const element = document.getElementById(elementId);
      const start = 0;
      const startTime = performance.now();

      function updateCount(currentTime) {
        const elapsed = currentTime - startTime;
        const progress = Math.min(elapsed / duration, 1);
        
        const current = Math.floor(start + (target - start) * progress);
        element.textContent = formatNumber(current);
        
        if (progress < 1) {
          requestAnimationFrame(updateCount);
        } else {
          element.textContent = formatNumber(target);
        }
      }
      
      requestAnimationFrame(updateCount);
    }

    // 刷新统计数据
    function refreshStats() {
      loadStats();
      showSuccess('数据已刷新');
    }

    // 通知系统统一由 ui.js 提供（showSuccess/showError/showNotification）

    // 注销功能（已由 ui.js 的 onLogout 统一处理）

    // 轮询控制（性能优化：页面不可见时暂停）
    let statsInterval = null;

    function startStatsPolling() {
      if (statsInterval) return; // 防止重复启动
      statsInterval = setInterval(loadStats, 30000);
    }

    function stopStatsPolling() {
      if (statsInterval) {
        clearInterval(statsInterval);
        statsInterval = null;
      }
    }

    // 页面可见性监听（后台标签页暂停轮询，节省CPU）
    document.addEventListener('visibilitychange', function() {
      if (document.hidden) {
        stopStatsPolling();
        console.log('[性能优化] 页面不可见，已暂停数据轮询');
      } else {
        loadStats(); // 页面重新可见时立即刷新一次
        startStatsPolling();
        console.log('[性能优化] 页面可见，已恢复数据轮询');
      }
    });

    // 页面初始化
    document.addEventListener('DOMContentLoaded', function() {
      if (window.initTopbar) initTopbar('index');
      // 加载统计数据
      loadStats();

      // 设置自动刷新（每30秒，仅在页面可见时）
      startStatsPolling();

      // 添加页面动画
      document.querySelectorAll('.animate-slide-up').forEach((el, index) => {
        el.style.animationDelay = `${index * 0.1}s`;
      });
    });
