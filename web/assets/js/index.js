    // 统计数据管理
    let statsData = {
      total_requests: 0,
      success_requests: 0,
      error_requests: 0,
      active_channels: 0,
      active_models: 0,
      duration_seconds: 1,
      rpm_stats: null,
      is_today: true
    };

    // 当前选中的时间范围
    let currentTimeRange = 'today';

    // 加载统计数据
    async function loadStats() {
      try {
        // 添加加载状态
        document.querySelectorAll('.metric-number').forEach(el => {
          el.classList.add('animate-pulse');
        });

        const response = await fetch(`/public/summary?range=${currentTimeRange}`);
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

      // 更新总体数字显示（成功/失败合并显示）
      document.getElementById('success-requests').textContent = formatNumber(statsData.success_requests || 0);
      document.getElementById('error-requests').textContent = formatNumber(statsData.error_requests || 0);
      document.getElementById('success-rate').textContent = successRate + '%';

      // 更新 RPM 和 QPS（使用峰值/平均/最近格式）
      const rpmStats = statsData.rpm_stats || null;
      const isToday = statsData.is_today !== false;
      updateGlobalRpmDisplay('total-rpm', rpmStats, isToday);
      updateGlobalQpsDisplay('total-qps', rpmStats, isToday);

      // 更新按渠道类型统计
      if (statsData.by_type) {
        updateTypeStats('anthropic', statsData.by_type.anthropic);
        updateTypeStats('codex', statsData.by_type.codex);
        updateTypeStats('openai', statsData.by_type.openai);
        updateTypeStats('gemini', statsData.by_type.gemini);
      }
    }

    // 更新全局 RPM 显示（格式：数值 数值 数值）
    function updateGlobalRpmDisplay(elementId, stats, showRecent) {
      const el = document.getElementById(elementId);
      if (!el) return;

      if (!stats || (stats.peak_rpm < 0.01 && stats.avg_rpm < 0.01)) {
        el.innerHTML = '--';
        return;
      }

      const fmt = v => v >= 1000 ? (v / 1000).toFixed(1) + 'K' : v.toFixed(1);
      const parts = [];

      if (stats.peak_rpm >= 0.01) {
        parts.push(`<span style="color:${getRpmColor(stats.peak_rpm)}">${fmt(stats.peak_rpm)}</span>`);
      }
      if (stats.avg_rpm >= 0.01) {
        parts.push(`<span style="color:${getRpmColor(stats.avg_rpm)}">${fmt(stats.avg_rpm)}</span>`);
      }
      if (showRecent && stats.recent_rpm >= 0.01) {
        parts.push(`<span style="color:${getRpmColor(stats.recent_rpm)}">${fmt(stats.recent_rpm)}</span>`);
      }

      el.innerHTML = parts.length > 0 ? parts.join(' ') : '--';
    }

    // 更新全局 QPS 显示（格式：数值 数值 数值）
    function updateGlobalQpsDisplay(elementId, stats, showRecent) {
      const el = document.getElementById(elementId);
      if (!el) return;

      if (!stats || (stats.peak_qps < 0.01 && stats.avg_qps < 0.01)) {
        el.innerHTML = '--';
        return;
      }

      const fmt = v => v >= 1000 ? (v / 1000).toFixed(1) + 'K' : v.toFixed(1);
      const parts = [];

      if (stats.peak_qps >= 0.01) {
        parts.push(`<span style="color:${getQpsColor(stats.peak_qps)}">${fmt(stats.peak_qps)}</span>`);
      }
      if (stats.avg_qps >= 0.01) {
        parts.push(`<span style="color:${getQpsColor(stats.avg_qps)}">${fmt(stats.avg_qps)}</span>`);
      }
      if (showRecent && stats.recent_qps >= 0.01) {
        parts.push(`<span style="color:${getQpsColor(stats.recent_qps)}">${fmt(stats.recent_qps)}</span>`);
      }

      el.innerHTML = parts.length > 0 ? parts.join(' ') : '--';
    }

    // 格式化RPM数值
    function formatRpmValue(rpm) {
      if (rpm >= 1000) return (rpm / 1000).toFixed(1) + 'K';
      if (rpm >= 1) return rpm.toFixed(1);
      return rpm.toFixed(2);
    }

    // 格式化QPS数值
    function formatQpsValue(qps) {
      if (qps >= 1000) return (qps / 1000).toFixed(1) + 'K';
      if (qps >= 1) return qps.toFixed(2);
      return qps.toFixed(3);
    }

    // RPM 颜色
    function getRpmColor(rpm) {
      if (rpm < 10) return 'var(--success-600)';
      if (rpm < 100) return 'var(--warning-600)';
      return 'var(--error-600)';
    }

    // QPS 颜色
    function getQpsColor(qps) {
      if (qps < 1) return 'var(--success-600)';
      if (qps < 10) return 'var(--warning-600)';
      return 'var(--error-600)';
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
          loadStats();
        });
      });
    }

    // 页面初始化
    document.addEventListener('DOMContentLoaded', function() {
      if (window.initTopbar) initTopbar('index');

      // 初始化时间范围选择器
      initTimeRangeSelector();

      // 加载统计数据
      loadStats();

      // 设置自动刷新（每30秒，仅在页面可见时）
      startStatsPolling();

      // 添加页面动画
      document.querySelectorAll('.animate-slide-up').forEach((el, index) => {
        el.style.animationDelay = `${index * 0.1}s`;
      });
    });
