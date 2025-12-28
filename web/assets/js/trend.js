    // 全局变量
    window.trendData = null;
    window.currentRange = 'today'; // 默认"本日"
    window.currentTrendType = 'first_byte'; // 默认显示首字响应时间趋势 (count/first_byte/cost)
    window.currentChannelType = 'all'; // 当前选中的渠道类型
    window.currentModel = ''; // 当前选中的模型（空字符串表示全部模型）
    window.currentAuthToken = ''; // 当前选中的令牌（空字符串表示全部令牌）
    window.currentChannelId = ''; // 当前选中的渠道ID
    window.currentChannelName = ''; // 当前选中的渠道名称
    window.chartInstance = null;
    window.channels = [];
    window.visibleChannels = new Set(); // 可见渠道集合
    window.availableModels = []; // 可用模型列表
    window.authTokens = []; // 令牌列表

    // 加载可用模型列表
    async function loadModels() {
      try {
        window.availableModels = (await fetchDataWithAuth('/admin/models?range=this_month')) || [];

        // 填充模型选择器
        const modelSelect = document.getElementById('f_model');
        if (modelSelect && window.availableModels.length > 0) {
          // 保留"全部模型"选项
          modelSelect.innerHTML = '<option value="">全部模型</option>';
          window.availableModels.forEach(model => {
            const option = document.createElement('option');
            option.value = model;
            option.textContent = model;
            modelSelect.appendChild(option);
          });

          // 恢复之前选择的模型
          if (window.currentModel) {
            modelSelect.value = window.currentModel;
          }
        }
      } catch (error) {
        console.error('加载模型列表失败:', error);
      }
    }

    // 加载令牌列表
    async function loadAuthTokens() {
      try {
        const data = await fetchDataWithAuth('/admin/auth-tokens');
        window.authTokens = (data && data.tokens) || [];

        // 填充令牌选择器
        const tokenSelect = document.getElementById('f_auth_token');
        if (tokenSelect && window.authTokens.length > 0) {
          // 保留"全部令牌"选项
          tokenSelect.innerHTML = '<option value="">全部令牌</option>';
          window.authTokens.forEach(token => {
            const option = document.createElement('option');
            option.value = token.id;
            option.textContent = token.description || `令牌 #${token.id}`;
            tokenSelect.appendChild(option);
          });

          // 恢复之前选择的令牌
          if (window.currentAuthToken) {
            tokenSelect.value = window.currentAuthToken;
          }
        }
      } catch (error) {
        console.error('加载令牌列表失败:', error);
      }
    }

    async function loadData() {
      try {
        renderTrendLoading();

        // 从 DOM 元素读取当前选择的时间范围和模型
        const rangeSelect = document.getElementById('f_hours');
        const currentRange = rangeSelect ? rangeSelect.value : (window.currentRange || 'today');
        window.currentRange = currentRange; // 同步到全局变量

        const modelSelect = document.getElementById('f_model');
        if (modelSelect) {
          window.currentModel = modelSelect.value || '';
        }

        const tokenSelect = document.getElementById('f_auth_token');
        if (tokenSelect) {
          window.currentAuthToken = tokenSelect.value || '';
        }

        // 读取渠道ID和渠道名筛选
        const idInput = document.getElementById('f_id');
        if (idInput) {
          window.currentChannelId = idInput.value.trim() || '';
        }
        const nameInput = document.getElementById('f_name');
        if (nameInput) {
          window.currentChannelName = nameInput.value.trim() || '';
        }

        const hours = window.getRangeHours ? getRangeHours(currentRange) : 24;
        window.currentHours = hours; // 同步到全局变量，供 renderChart 使用
        const bucketMin = computeBucketMin(hours);

        // 并行加载趋势数据和渠道列表
        // metrics API使用range参数获取精确时间范围
        const metricsUrl = `/admin/metrics?range=${currentRange}&bucket_min=${bucketMin}`;
        const channelsUrl = '/admin/channels';

        // 添加渠道类型筛选
        const channelTypeParam = (window.currentChannelType && window.currentChannelType !== 'all') ?
          `&channel_type=${window.currentChannelType}` : '';
        const channelTypeParamForList = (window.currentChannelType && window.currentChannelType !== 'all') ?
          `&type=${window.currentChannelType}` : '';

        // 添加模型筛选参数
        const modelParam = window.currentModel ? `&model=${encodeURIComponent(window.currentModel)}` : '';

        // 添加令牌筛选参数
        const tokenParam = window.currentAuthToken ? `&auth_token_id=${encodeURIComponent(window.currentAuthToken)}` : '';

        // 添加渠道ID和渠道名筛选参数
        const channelIdParam = window.currentChannelId ? `&channel_id=${encodeURIComponent(window.currentChannelId)}` : '';
        const channelNameParam = window.currentChannelName ? `&channel_name_like=${encodeURIComponent(window.currentChannelName)}` : '';

        const [metrics, channels] = await Promise.all([
          fetchAPIWithAuthRaw(metricsUrl + channelTypeParam + modelParam + tokenParam + channelIdParam + channelNameParam),
          fetchDataWithAuth(channelsUrl + (channelTypeParamForList ? '?' + channelTypeParamForList.slice(1) : ''))
        ]);

        if (!metrics.payload.success) throw new Error(metrics.payload.error || '获取趋势数据失败');

        window.trendData = metrics.payload.data || [];
        window.channels = channels || [];
        
        // 修复：智能初始化渠道显示状态（处理localStorage过时数据）
        // 默认不显示任何渠道，只显示总数
        if (window.visibleChannels.size === 0) {
          // 首次访问：不默认显示任何渠道
          console.log('初始化渠道显示状态（首次访问）- 默认仅显示总数');
          // 不添加任何渠道到 visibleChannels，保持为空集合
        } else {
          // 修复：验证并清理localStorage中过时的渠道选择
          console.log('验证现有渠道选择状态...', Array.from(window.visibleChannels));
          const validChannels = new Set();

          // 检查每个已保存渠道是否在当前数据中存在
          window.visibleChannels.forEach(channelName => {
            if (hasChannelData(channelName, window.trendData)) {
              validChannels.add(channelName);
            } else {
              console.log(`清理过时渠道: ${channelName}（数据中不存在）`);
            }
          });

          // 更新visibleChannels为验证后的集合
          window.visibleChannels = validChannels;
          persistChannelState();
          console.log('更新后的可见渠道:', Array.from(window.visibleChannels));
        }
        
        // 添加调试信息显示
        const debugSince = metrics.res.headers.get('X-Debug-Since');
        const debugPoints = metrics.res.headers.get('X-Debug-Points');
        const debugTotal = metrics.res.headers.get('X-Debug-Total');

        console.log('趋势数据调试信息:', {
          since: debugSince,
          points: debugPoints,
          total: debugTotal,
          dataLength: trendData.length,
          channelsCount: window.channels.length
        });

        updateChannelFilter();
        renderChart();

        // 更新分桶提示
        const iv = document.getElementById('bucket-interval');
        if (iv) iv.textContent = `数据更新间隔：${formatInterval(bucketMin)} | 数据点：${trendData.length} | 总请求：${debugTotal || '未知'}`;

      } catch (error) {
        console.error('加载趋势数据失败:', error);
        try { if (window.showError) window.showError('无法加载趋势数据'); } catch(_){}
        renderTrendError();
      }
    }

    function computeBucketMin(hours) {
      if (hours <= 1) return 1; // 1分钟
      if (hours <= 6) return 2; // 2分钟
      if (hours <= 24) return 5; // 5分钟
      if (hours <= 72) return 15; // 15分钟
      return 60; // 1小时
    }

    function renderTrendLoading() {
      document.getElementById('chart-loading').style.display = 'flex';
      document.getElementById('chart-error').style.display = 'none';
      document.getElementById('chart').style.display = 'none';
    }

    function renderTrendError() {
      document.getElementById('chart-loading').style.display = 'none';
      document.getElementById('chart-error').style.display = 'flex';
      document.getElementById('chart').style.display = 'none';
    }

    function renderChart() {
      if (!window.trendData || !window.trendData.length) {
        renderTrendError();
        return;
      }

      // 显示图表容器
      document.getElementById('chart-loading').style.display = 'none';
      document.getElementById('chart-error').style.display = 'none';
      document.getElementById('chart').style.display = 'block';

      // 初始化或获取 ECharts 实例
      const chartDom = document.getElementById('chart');
      if (!window.chartInstance) {
        window.chartInstance = echarts.init(chartDom, null, {
          renderer: 'canvas'
        });
        attachChartResizeObserver(chartDom);
      }

      // 准备时间数据
      const timestamps = window.trendData.map(point => {
        const date = new Date(point.ts || point.Ts);
        if (window.currentHours > 24) {
          return `${date.getMonth()+1}/${date.getDate()} ${pad(date.getHours())}:00`;
        } else {
          return `${pad(date.getHours())}:${pad(date.getMinutes())}`;
        }
      });

      const noRequestRanges = computeNoRequestRanges(window.trendData);
      const markAreaData = noRequestRanges
        .filter(([start, end]) => (end - start + 1) >= 3) // 太短的空窗不要标，避免噪音
        .map(([start, end]) => ([
          { xAxis: timestamps[start] },
          { xAxis: timestamps[end] }
        ]));

      // 为每个可见渠道生成颜色
      const channelColors = generateChannelColors(window.visibleChannels);

      // 准备series数据
      const series = [];
      const trendType = window.currentTrendType;
      const showZoom = shouldShowZoom(timestamps.length, window.currentHours, trendType);

      // 根据趋势类型准备不同的总体数据
      if (trendType === 'count') {
        // 调用次数趋势：添加总体成功/失败线
        series.push({
          name: '总成功请求',
          type: 'line',
          smooth: 0.25,
          symbol: 'circle',
          symbolSize: 4,
          showSymbol: false,
          sampling: 'lttb',
          connectNulls: false,
          emphasis: { focus: 'series', showSymbol: true },
          itemStyle: {
            color: '#10b981'
          },
          lineStyle: {
            width: 2,
            color: '#10b981',
            cap: 'round',
            join: 'round'
          },
          areaStyle: {
            color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
              { offset: 0, color: 'rgba(16, 185, 129, 0.22)' },
              { offset: 1, color: 'rgba(16, 185, 129, 0.00)' }
            ])
          },
          data: window.trendData.map(point => {
            const val = point.success || 0;
            return val; // 0值显示为基线，避免大段空白
          })
        });

        series.push({
          name: '总失败请求',
          type: 'line',
          smooth: 0.25,
          symbol: 'circle',
          symbolSize: 4,
          showSymbol: false,
          sampling: 'lttb',
          connectNulls: false,
          emphasis: { focus: 'series', showSymbol: true },
          itemStyle: {
            color: '#ef4444'
          },
          lineStyle: {
            width: 2,
            color: '#ef4444',
            cap: 'round',
            join: 'round'
          },
          areaStyle: {
            color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
              { offset: 0, color: 'rgba(239, 68, 68, 0.12)' },
              { offset: 1, color: 'rgba(239, 68, 68, 0.00)' }
            ])
          },
          data: window.trendData.map(point => {
            const val = point.error || 0;
            return val; // 0值显示为基线，避免大段空白
          })
        });
      } else if (trendType === 'first_byte') {
        // 首字响应时间趋势：添加总体平均首字响应时间线
        series.push({
          name: '平均首字响应时间',
          type: 'line',
          smooth: 0.25,
          symbol: 'circle',
          symbolSize: 4,
          showSymbol: false,
          sampling: 'lttb',
          connectNulls: false,
          emphasis: { focus: 'series', showSymbol: true },
          itemStyle: {
            color: '#0ea5e9'
          },
          lineStyle: {
            width: 2,
            color: '#0ea5e9',
            cap: 'round',
            join: 'round'
          },
          areaStyle: {
            color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
              { offset: 0, color: 'rgba(14, 165, 233, 0.18)' },
              { offset: 1, color: 'rgba(14, 165, 233, 0.00)' }
            ])
          },
          data: window.trendData.map(point => {
            const fbt = point.avg_first_byte_time_seconds;
            return (fbt != null && fbt > 0) ? fbt : null; // 秒
          })
        });
      } else if (trendType === 'duration') {
        // 总耗时趋势：添加总体平均总耗时线
        series.push({
          name: '平均总耗时',
          type: 'line',
          smooth: 0.25,
          symbol: 'circle',
          symbolSize: 4,
          showSymbol: false,
          sampling: 'lttb',
          connectNulls: false,
          emphasis: { focus: 'series', showSymbol: true },
          itemStyle: {
            color: '#a855f7'
          },
          lineStyle: {
            width: 2,
            color: '#a855f7',
            cap: 'round',
            join: 'round'
          },
          areaStyle: {
            color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
              { offset: 0, color: 'rgba(168, 85, 247, 0.16)' },
              { offset: 1, color: 'rgba(168, 85, 247, 0.00)' }
            ])
          },
          data: window.trendData.map(point => {
            const dur = point.avg_duration_seconds;
            return (dur != null && dur > 0) ? dur : null; // 秒
          })
        });
      } else if (trendType === 'tokens') {
        // Token用量趋势：添加输入、输出、缓存读、缓存建四条线
        series.push({
          name: '输入Token',
          type: 'line',
          smooth: 0.25,
          symbol: 'circle',
          symbolSize: 4,
          showSymbol: false,
          sampling: 'lttb',
          connectNulls: false,
          emphasis: { focus: 'series', showSymbol: true },
          itemStyle: { color: '#3b82f6' },
          lineStyle: { width: 2, color: '#3b82f6', cap: 'round', join: 'round' },
          data: window.trendData.map(point => point.input_tokens || 0)
        });
        series.push({
          name: '输出Token',
          type: 'line',
          smooth: 0.25,
          symbol: 'circle',
          symbolSize: 4,
          showSymbol: false,
          sampling: 'lttb',
          connectNulls: false,
          emphasis: { focus: 'series', showSymbol: true },
          itemStyle: { color: '#10b981' },
          lineStyle: { width: 2, color: '#10b981', cap: 'round', join: 'round' },
          data: window.trendData.map(point => point.output_tokens || 0)
        });
        series.push({
          name: '缓存读取',
          type: 'line',
          smooth: 0.25,
          symbol: 'circle',
          symbolSize: 4,
          showSymbol: false,
          sampling: 'lttb',
          connectNulls: false,
          emphasis: { focus: 'series', showSymbol: true },
          itemStyle: { color: '#f97316' },
          lineStyle: { width: 2, color: '#f97316', cap: 'round', join: 'round' },
          data: window.trendData.map(point => point.cache_read_tokens || 0)
        });
        series.push({
          name: '缓存创建',
          type: 'line',
          smooth: 0.25,
          symbol: 'circle',
          symbolSize: 4,
          showSymbol: false,
          sampling: 'lttb',
          connectNulls: false,
          emphasis: { focus: 'series', showSymbol: true },
          itemStyle: { color: '#a855f7' },
          lineStyle: { width: 2, color: '#a855f7', cap: 'round', join: 'round' },
          data: window.trendData.map(point => point.cache_creation_tokens || 0)
        });
      } else if (trendType === 'cost') {
        // 费用消耗趋势：添加总体费用线
        series.push({
          name: '总费用',
          type: 'line',
          smooth: 0.25,
          symbol: 'circle',
          symbolSize: 4,
          showSymbol: false,
          sampling: 'lttb',
          connectNulls: false,
          emphasis: { focus: 'series', showSymbol: true },
          itemStyle: {
            color: '#f97316'
          },
          lineStyle: {
            width: 2,
            color: '#f97316',
            cap: 'round',
            join: 'round'
          },
          areaStyle: {
            color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
              { offset: 0, color: 'rgba(249, 115, 22, 0.16)' },
              { offset: 1, color: 'rgba(249, 115, 22, 0.00)' }
            ])
          },
          data: window.trendData.map(point => {
            const cost = point.total_cost;
            return cost || 0;
          })
        });
      } else if (trendType === 'rpm') {
        // RPM趋势：每分钟请求数 = (success + error) / bucketMin
        const bucketMin = window.currentHours ? computeBucketMin(window.currentHours) : 5;
        series.push({
          name: 'RPM',
          type: 'line',
          smooth: 0.25,
          symbol: 'circle',
          symbolSize: 4,
          showSymbol: false,
          sampling: 'lttb',
          connectNulls: false,
          emphasis: { focus: 'series', showSymbol: true },
          itemStyle: { color: '#3b82f6' },
          lineStyle: { width: 2, color: '#3b82f6', cap: 'round', join: 'round' },
          areaStyle: {
            color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
              { offset: 0, color: 'rgba(59, 130, 246, 0.16)' },
              { offset: 1, color: 'rgba(59, 130, 246, 0.00)' }
            ])
          },
          data: window.trendData.map(point => {
            const total = (point.success || 0) + (point.error || 0);
            return total > 0 ? total / bucketMin : 0;
          })
        });
      }

      // 为每个可见渠道添加对应趋势线
      console.log('开始渲染渠道数据，可见渠道:', Array.from(window.visibleChannels));

      Array.from(window.visibleChannels).forEach(channelName => {
        const color = channelColors[channelName];

        if (trendType === 'count') {
          // 调用次数趋势：渠道成功/失败线
          let successTotal = 0;
          let errorTotal = 0;
          const successData = window.trendData.map(point => {
            const channels = point.channels || {};
            const channelData = channels[channelName] || { success: 0, error: 0 };
            const success = channelData.success || 0;
            successTotal += success;
            return success;
          });

          const errorData = window.trendData.map(point => {
            const channels = point.channels || {};
            const channelData = channels[channelName] || { success: 0, error: 0 };
            const error = channelData.error || 0;
            errorTotal += error;
            return error;
          });

          console.log(`渠道 ${channelName} 数据统计: 成功总数=${successTotal}, 错误总数=${errorTotal}`);

          // 成功线
          if (successTotal > 0) {
            series.push({
              name: `${channelName}(成功)`,
              type: 'line',
              smooth: 0.25,
              symbol: 'none',
              sampling: 'lttb',
              connectNulls: false,
              emphasis: { focus: 'series' },
              itemStyle: { color: color },
              lineStyle: { width: 1.5, color: color, type: 'solid', cap: 'round', join: 'round' },
              data: successData
            });
          }

          // 失败线
          if (errorTotal > 0) {
            series.push({
              name: `${channelName}(失败)`,
              type: 'line',
              smooth: 0.25,
              symbol: 'none',
              sampling: 'lttb',
              connectNulls: false,
              emphasis: { focus: 'series' },
              itemStyle: { color: color },
              lineStyle: { width: 1.5, color: color, type: 'dashed', cap: 'round', join: 'round' },
              data: errorData
            });
          }
        } else if (trendType === 'first_byte') {
          // 首字响应时间趋势：渠道平均首字响应时间线
          let hasData = false;
          const fbtData = window.trendData.map(point => {
            const channels = point.channels || {};
            const channelData = channels[channelName] || {};
            const fbt = channelData.avg_first_byte_time_seconds;
            if (fbt != null && fbt > 0) {
              hasData = true;
              return fbt; // 秒
            }
            return null;
          });

          if (hasData) {
            series.push({
              name: channelName,
              type: 'line',
              smooth: 0.25,
              symbol: 'none',
              sampling: 'lttb',
              connectNulls: false,
              emphasis: { focus: 'series' },
              itemStyle: { color: color },
              lineStyle: { width: 1.5, color: color, cap: 'round', join: 'round' },
              data: fbtData
            });
          }
        } else if (trendType === 'duration') {
          // 总耗时趋势：渠道平均总耗时线
          let hasData = false;
          const durData = window.trendData.map(point => {
            const channels = point.channels || {};
            const channelData = channels[channelName] || {};
            const dur = channelData.avg_duration_seconds;
            if (dur != null && dur > 0) {
              hasData = true;
              return dur; // 秒
            }
            return null;
          });

          if (hasData) {
            series.push({
              name: channelName,
              type: 'line',
              smooth: 0.25,
              symbol: 'none',
              sampling: 'lttb',
              connectNulls: false,
              emphasis: { focus: 'series' },
              itemStyle: { color: color },
              lineStyle: { width: 1.5, color: color, cap: 'round', join: 'round' },
              data: durData
            });
          }
        } else if (trendType === 'tokens') {
          // Token用量趋势：渠道Token线（输入+输出合计）
          let hasData = false;
          const tokenData = window.trendData.map(point => {
            const channels = point.channels || {};
            const channelData = channels[channelName] || {};
            const total = (channelData.input_tokens || 0) + (channelData.output_tokens || 0);
            if (total > 0) {
              hasData = true;
              return total;
            }
            return null;
          });

          if (hasData) {
            series.push({
              name: channelName,
              type: 'line',
              smooth: 0.25,
              symbol: 'none',
              sampling: 'lttb',
              connectNulls: false,
              emphasis: { focus: 'series' },
              itemStyle: { color: color },
              lineStyle: { width: 1.5, color: color, cap: 'round', join: 'round' },
              data: tokenData
            });
          }
        } else if (trendType === 'cost') {
          // 费用消耗趋势：渠道费用线
          let hasData = false;
          const costData = window.trendData.map(point => {
            const channels = point.channels || {};
            const channelData = channels[channelName] || {};
            const cost = channelData.total_cost;
            if (cost != null && cost > 0) {
              hasData = true;
              return cost;
            }
            return null;
          });

          if (hasData) {
            series.push({
              name: channelName,
              type: 'line',
              smooth: 0.25,
              symbol: 'none',
              sampling: 'lttb',
              connectNulls: false,
              emphasis: { focus: 'series' },
              itemStyle: { color: color },
              lineStyle: { width: 1.5, color: color, cap: 'round', join: 'round' },
              data: costData
            });
          }
        } else if (trendType === 'rpm') {
          // RPM趋势：渠道每分钟请求数
          const bucketMin = window.currentHours ? computeBucketMin(window.currentHours) : 5;
          let hasData = false;
          const rpmData = window.trendData.map(point => {
            const channels = point.channels || {};
            const channelData = channels[channelName] || {};
            const total = (channelData.success || 0) + (channelData.error || 0);
            if (total > 0) {
              hasData = true;
              return total / bucketMin;
            }
            return null;
          });

          if (hasData) {
            series.push({
              name: channelName,
              type: 'line',
              smooth: 0.25,
              symbol: 'none',
              sampling: 'lttb',
              connectNulls: false,
              emphasis: { focus: 'series' },
              itemStyle: { color: color },
              lineStyle: { width: 1.5, color: color, cap: 'round', join: 'round' },
              data: rpmData
            });
          }
        }
      });

      // 首字响应/总耗时：加参考线（P50/P90）和极值标记，便于读趋势/看尖峰
      if (trendType === 'first_byte' || trendType === 'duration') {
        enhanceLatencySeries(series);
      }

      // ECharts 配置
      const legendHeight = 28;
      const gridTopPx = legendHeight + 18;
      const gridBottomPx = showZoom ? 70 : 48;
      const gridRightPx = (trendType === 'first_byte' || trendType === 'duration') ? 44 : 28;
      const xAxisLabelInterval = computeXAxisLabelInterval(timestamps.length, 10);
      const xAxisRotate = (window.currentHours > 24 || window.innerWidth < 640) ? 45 : 0;
      const yAxisScale = (trendType === 'first_byte' || trendType === 'duration');
      const useLatencyAxis = (trendType === 'first_byte' || trendType === 'duration');
      const yAxisMin = useLatencyAxis ? latencyAxisMin : 0;
      const yAxisMax = useLatencyAxis ? latencyAxisMax : null;

      const option = {
        backgroundColor: 'transparent',
        title: {
          show: false
        },
        tooltip: {
          trigger: 'axis',
          confine: true,
          backgroundColor: 'rgba(0, 0, 0, 0.85)',
          borderColor: 'rgba(255, 255, 255, 0.1)',
          borderWidth: 1,
          textStyle: {
            color: '#fff',
            fontSize: 12
          },
          axisPointer: {
            type: 'cross',
            crossStyle: {
              color: '#999',
              width: 1,
              type: 'dashed'
            }
          },
          formatter: function(params) {
            const dataIndex = params && params.length ? params[0].dataIndex : null;
            const point = (dataIndex != null && window.trendData && window.trendData[dataIndex]) ? window.trendData[dataIndex] : null;
            const totalReq = point ? ((point.success || 0) + (point.error || 0)) : null;

            let html = `<div style="font-weight: 600; margin-bottom: 6px;">${params[0].axisValue}</div>`;
            if (totalReq != null) {
              const hint = totalReq === 0
                ? `<span style="color: rgba(203, 213, 225, 0.95);">（该时间段无请求）</span>`
                : '';
              html += `<div style="margin-bottom: 8px; color: rgba(226, 232, 240, 0.95); font-size: 12px;">请求数: ${totalReq}${hint}</div>`;
            }
            params.forEach(param => {
              const color = param.color;
              const value = param.value;
              let formattedValue;

              // 根据当前趋势类型格式化数值
              if (value == null) {
                formattedValue = 'N/A';
              } else if (window.currentTrendType === 'first_byte' || window.currentTrendType === 'duration') {
                // 首字响应时间/总耗时：秒
                formattedValue = value.toFixed(1) + 's';
              } else if (window.currentTrendType === 'cost') {
                // 费用消耗：美元格式
                if (value >= 1) {
                  formattedValue = '$' + value.toFixed(2);
                } else if (value >= 0.01) {
                  formattedValue = '$' + value.toFixed(4);
                } else if (value > 0) {
                  formattedValue = '$' + value.toFixed(6);
                } else {
                  formattedValue = '$0.00';
                }
              } else if (window.currentTrendType === 'tokens') {
                // Token用量：K/M格式
                if (value >= 1000000) {
                  formattedValue = (value / 1000000).toFixed(1) + 'M';
                } else if (value >= 1000) {
                  formattedValue = (value / 1000).toFixed(1) + 'K';
                } else {
                  formattedValue = value.toString();
                }
              } else if (window.currentTrendType === 'rpm') {
                // RPM：保留1位小数
                formattedValue = value.toFixed(1) + '/min';
              } else {
                // 调用次数：整数
                formattedValue = Math.round(value).toString();
              }

              html += `
                <div style="display: flex; align-items: center; gap: 8px; margin: 4px 0;">
                  <span style="display: inline-block; width: 10px; height: 10px; background: ${color}; border-radius: 50%;"></span>
                  <span>${param.seriesName}: ${formattedValue}</span>
                </div>
              `;
            });
            return html;
          }
        },
        legend: {
          data: series.map(s => s.name),
          top: 10,
          left: 16,
          right: 16,
          textStyle: {
            color: '#666',
            fontSize: 11
          },
          itemWidth: 20,
          itemHeight: 8,
          itemGap: 12,
          type: 'scroll',
          pageIconColor: '#666',
          pageIconInactiveColor: '#ccc',
          pageIconSize: 12,
          pageTextStyle: {
            color: '#666',
            fontSize: 10
          }
        },
        grid: {
          left: 16,
          right: gridRightPx,
          bottom: gridBottomPx,
          top: gridTopPx,
          containLabel: true
        },
        xAxis: {
          type: 'category',
          boundaryGap: false,
          data: timestamps,
          axisLine: {
            lineStyle: {
              color: '#e5e7eb'
            }
          },
          axisTick: {
            alignWithLabel: true,
            lineStyle: { color: '#e5e7eb' }
          },
          axisLabel: {
            color: '#6b7280',
            fontSize: 11,
            rotate: xAxisRotate,
            hideOverlap: true,
            interval: xAxisLabelInterval
          },
          splitLine: {
            show: true,
            lineStyle: {
              color: 'rgba(148, 163, 184, 0.25)',
              type: 'dashed'
            }
          }
        },
        yAxis: {
          type: 'value',
          scale: yAxisScale,
          min: yAxisMin,
          max: yAxisMax,
          axisLine: {
            lineStyle: {
              color: '#e5e7eb'
            }
          },
          axisLabel: {
            color: '#6b7280',
            fontSize: 11,
            formatter: function(value) {
              if (trendType === 'first_byte' || trendType === 'duration') {
                // 首字响应时间/总耗时：秒格式
                return value.toFixed(1) + 's';
              } else if (trendType === 'cost') {
                // 费用消耗：美元格式
                if (value >= 1) return '$' + value.toFixed(2);
                if (value >= 0.01) return '$' + value.toFixed(4);
                return '$' + value.toFixed(6);
              } else if (trendType === 'tokens') {
                // Token用量：K/M格式
                if (value >= 1000000) return (value / 1000000).toFixed(1) + 'M';
                if (value >= 1000) return (value / 1000).toFixed(1) + 'K';
                return value;
              } else if (trendType === 'rpm') {
                // RPM：保留1位小数
                return value.toFixed(1);
              } else {
                // 调用次数：K/M格式
                if (value >= 1000000) return (value / 1000000) + 'M';
                if (value >= 1000) return (value / 1000) + 'K';
                return value;
              }
            }
          },
          splitLine: {
            lineStyle: {
              color: 'rgba(148, 163, 184, 0.25)',
              type: 'dashed'
            }
          }
        },
        series: applyNoRequestMarkArea(series, markAreaData),
        dataZoom: showZoom ? [
          {
            type: 'inside',
            start: 0,
            end: 100,
            minValueSpan: 10
          },
          {
            show: true,
            type: 'slider',
            bottom: 18,
            start: 0,
            end: 100,
            height: 20,
            borderColor: '#e5e7eb',
            backgroundColor: 'rgba(148, 163, 184, 0.10)',
            fillerColor: 'rgba(59, 130, 246, 0.16)',
            handleStyle: {
              color: '#3b82f6',
              borderColor: '#3b82f6'
            },
            textStyle: {
              color: '#6b7280',
              fontSize: 10
            }
          }
        ] : [],
        animationDuration: 1000,
        animationEasing: 'cubicInOut'
      };

      // 设置配置并渲染
      window.chartInstance.setOption(option, true); // true 表示不合并，全量更新
    }

    function attachChartResizeObserver(chartDom) {
      if (!chartDom) return;
      if (window.chartResizeObserver) return;
      if (typeof ResizeObserver === 'undefined') return;

      let raf = 0;
      window.chartResizeObserver = new ResizeObserver(() => {
        if (!window.chartInstance) return;
        if (raf) cancelAnimationFrame(raf);
        raf = requestAnimationFrame(() => {
          try { window.chartInstance.resize(); } catch (_) {}
        });
      });

      window.chartResizeObserver.observe(chartDom);
    }

    function shouldShowZoom(points, hours, trendType) {
      if (hours > 24) return true;
      if (trendType === 'first_byte' || trendType === 'duration') return points >= 60;
      return points >= 120;
    }

    function computeXAxisLabelInterval(points, maxLabels) {
      if (!points || points <= maxLabels) return 0;
      return Math.max(0, Math.ceil(points / maxLabels) - 1);
    }

    // 标注无请求区间：视觉上解释“断线/空窗”，同时不篡改数据语义
    function computeNoRequestRanges(trendData) {
      const ranges = [];
      let start = -1;
      for (let i = 0; i < trendData.length; i++) {
        const p = trendData[i] || {};
        const total = (p.success || 0) + (p.error || 0);
        if (total === 0) {
          if (start === -1) start = i;
        } else if (start !== -1) {
          ranges.push([start, i - 1]);
          start = -1;
        }
      }
      if (start !== -1) ranges.push([start, trendData.length - 1]);
      return ranges;
    }

    function applyNoRequestMarkArea(series, markAreaData) {
      if (!markAreaData || markAreaData.length === 0) return series;
      if (!series || series.length === 0) return series;

      // 只挂在第一条 series 上，避免重复渲染造成性能和视觉噪音
      const first = { ...series[0] };
      first.markArea = {
        silent: true,
        itemStyle: {
          color: 'rgba(148, 163, 184, 0.08)'
        },
        label: {
          show: false
        },
        data: markAreaData
      };
      return [first, ...series.slice(1)];
    }

    function latencyAxisMin(value) {
      if (!value) return 0;
      const min = Number.isFinite(value.min) ? value.min : 0;
      const max = Number.isFinite(value.max) ? value.max : 0;
      const range = Math.max(0, max - min);
      const pad = range > 0 ? range * 0.08 : max * 0.08;
      return Math.max(0, min - pad);
    }

    function latencyAxisMax(value) {
      if (!value) return null;
      const min = Number.isFinite(value.min) ? value.min : 0;
      const max = Number.isFinite(value.max) ? value.max : 0;
      const range = Math.max(0, max - min);
      const pad = range > 0 ? range * 0.08 : Math.max(10, max * 0.08);
      return max + pad;
    }

    function enhanceLatencySeries(series) {
      if (!series || series.length === 0) return;
      const base = series[0];
      if (!base || !Array.isArray(base.data)) return;

      const values = base.data.filter(v => typeof v === 'number' && Number.isFinite(v) && v > 0);
      if (values.length < 5) return;

      const p50 = percentile(values, 0.50);
      const p90 = percentile(values, 0.90);

      base.markLine = {
        silent: true,
        symbol: 'none',
        lineStyle: {
          width: 1,
          type: 'dashed',
          color: 'rgba(100, 116, 139, 0.55)'
        },
        label: {
          color: '#334155',
          fontSize: 11,
          position: 'insideEndTop',
          padding: [2, 6],
          borderRadius: 4,
          backgroundColor: 'rgba(255, 255, 255, 0.85)',
          borderColor: 'rgba(148, 163, 184, 0.55)',
          borderWidth: 1,
          formatter: (p) => {
            const v = p && p.value != null ? p.value : null;
            if (v == null) return '';
            return `${p.name}: ${Number(v).toFixed(1)}s`;
          }
        },
        data: [
          { name: 'P50', yAxis: p50 },
          { name: 'P90', yAxis: p90 }
        ]
      };

      base.markPoint = {
        symbol: 'pin',
        symbolSize: 34,
        label: {
          color: '#0f172a',
          fontSize: 10,
          formatter: (p) => (p && p.value != null ? `${Number(p.value).toFixed(1)}s` : '')
        },
        itemStyle: {
          color: 'rgba(14, 165, 233, 0.85)'
        },
        data: [
          { type: 'max', name: 'MAX' }
        ]
      };
    }

    function percentile(values, p) {
      if (!values || values.length === 0) return 0;
      const sorted = values.slice().sort((a, b) => a - b);
      const clamped = Math.min(1, Math.max(0, p));
      const idx = (sorted.length - 1) * clamped;
      const lo = Math.floor(idx);
      const hi = Math.ceil(idx);
      if (lo === hi) return sorted[lo];
      const w = idx - lo;
      return sorted[lo] * (1 - w) + sorted[hi] * w;
    }

    function formatInterval(min) { 
      return min >= 60 ? (min/60) + '小时' : min + '分钟';
    }

    // 工具函数
    function pad(n) {
      return (n < 10 ? '0' : '') + n;
    }
    
    // 检查渠道是否有数据的函数
    function hasChannelData(channelName, trendData) {
      if (!trendData || !trendData.length) {
        console.log(`hasChannelData: 没有趋势数据 for ${channelName}`);
        return false;
      }
      
      let totalSuccess = 0;
      let totalError = 0;
      
      trendData.forEach(point => {
        const channels = point.channels || {};
        const channelData = channels[channelName] || { success: 0, error: 0 };
        totalSuccess += channelData.success || 0;
        totalError += channelData.error || 0;
      });
      
      const hasData = (totalSuccess + totalError) > 0;
      console.log(`hasChannelData: ${channelName} - success=${totalSuccess}, error=${totalError}, hasData=${hasData}`);
      return hasData;
    }
    
    // 生成渠道颜色（避免与总体趋势线颜色冲突）
    // 总体趋势线保留颜色: #10b981(绿), #ef4444(红), #0ea5e9(天蓝), #a855f7(紫), #f97316(橙)
    function generateChannelColors(channels) {
      const colors = [
        '#3b82f6', // 蓝色
        '#06b6d4', // 青色
        '#14b8a6', // 绿松色
        '#84cc16', // 黄绿色
        '#eab308', // 黄色
        '#fb923c', // 浅橙色
        '#ec4899', // 粉色
        '#6366f1', // 靛蓝色
        '#8b5cf6', // 淡紫色
        '#22c55e', // 亮绿色
        '#f43f5e', // 玫红色
        '#0891b2', // 深青色
        '#65a30d', // 橄榄绿
        '#ca8a04', // 金黄色
        '#dc2626'  // 深红色
      ];

      const channelColors = {};
      let colorIndex = 0;
      Array.from(channels).forEach(channelName => {
        channelColors[channelName] = colors[colorIndex % colors.length];
        colorIndex++;
      });

      return channelColors;
    }
    
    // 更新渠道筛选器 - 显示所有有数据的渠道（包括未配置的渠道）
    function updateChannelFilter() {
      const filterList = document.getElementById('channel-filter-list');
      if (!filterList) return;
      
      // 收集所有有数据的渠道名称
      const allChannelNames = new Set();
      
      // 添加已配置的启用渠道
      if (window.channels) {
        window.channels.forEach(ch => {
          if (ch.enabled && hasChannelData(ch.name, window.trendData)) {
            allChannelNames.add(ch.name);
          }
        });
      }
      
      // 添加趋势数据中存在但未配置的渠道（如"未知渠道"）
      if (window.trendData) {
        window.trendData.forEach(point => {
          if (point.channels) {
            Object.keys(point.channels).forEach(name => {
              const chData = point.channels[name];
              if ((chData.success || 0) + (chData.error || 0) > 0) {
                allChannelNames.add(name);
              }
            });
          }
        });
      }
      
      console.log('筛选器中的所有渠道:', Array.from(allChannelNames));
      
      // 生成颜色映射
      const channelColors = generateChannelColors(allChannelNames);
      
      filterList.innerHTML = '';
      
      // 渲染渠道列表
      Array.from(allChannelNames).sort().forEach(channelName => {
        const isVisible = window.visibleChannels.has(channelName);

        // 为"未知渠道"添加特殊标识
        const displayName = channelName === '未知渠道'
          ? `${channelName} ⚠️`
          : channelName;

        const item = TemplateEngine.render('tpl-channel-filter-item', {
          checkedClass: isVisible ? 'checked' : '',
          color: channelColors[channelName],
          displayName: displayName
        });
        if (item) {
          item.onclick = () => toggleChannel(channelName);
          filterList.appendChild(item);
        }
      });
    }
    
    // 切换渠道显示/隐藏
    function toggleChannel(channelName) {
      if (window.visibleChannels.has(channelName)) {
        window.visibleChannels.delete(channelName);
      } else {
        window.visibleChannels.add(channelName);
      }
      
      updateChannelFilter();
      renderChart();
      persistChannelState();
    }
    
    // 全选渠道 - 选择所有有数据的渠道（包括未配置的渠道）
    function selectAllChannels() {
      // 添加已配置的启用渠道
      if (window.channels) {
        window.channels.forEach(ch => {
          if (ch.enabled && hasChannelData(ch.name, window.trendData)) {
            window.visibleChannels.add(ch.name);
          }
        });
      }
      
      // 添加趋势数据中存在但未配置的渠道
      if (window.trendData) {
        window.trendData.forEach(point => {
          if (point.channels) {
            Object.keys(point.channels).forEach(name => {
              const chData = point.channels[name];
              if ((chData.success || 0) + (chData.error || 0) > 0) {
                window.visibleChannels.add(name);
              }
            });
          }
        });
      }
      
      updateChannelFilter();
      renderChart();
      persistChannelState();
    }
    
    // 清空选择
    function clearAllChannels() {
      window.visibleChannels.clear();
      
      updateChannelFilter();
      renderChart();
      persistChannelState();
    }
    
    // 切换渠道筛选器显示/隐藏
    function toggleChannelFilter() {
      const dropdown = document.getElementById('channel-filter-dropdown');
      if (!dropdown) return;
      
      const isVisible = dropdown.style.display === 'block';
      dropdown.style.display = isVisible ? 'none' : 'block';
      
      if (!isVisible) {
        // 点击外部关闭
        setTimeout(() => {
          document.addEventListener('click', closeChannelFilter, true);
        }, 10);
      }
    }
    
    function closeChannelFilter(event) {
      const dropdown = document.getElementById('channel-filter-dropdown');
      const container = document.querySelector('.channel-filter-container');
      
      if (!dropdown || !container) return;
      
      if (!container.contains(event.target)) {
        dropdown.style.display = 'none';
        document.removeEventListener('click', closeChannelFilter, true);
      }
    }
    
    // 持久化渠道状态
    function persistChannelState() {
      try {
        const visibleArray = Array.from(window.visibleChannels);
        localStorage.setItem('trend.visibleChannels', JSON.stringify(visibleArray));
      } catch (_) {}
    }
    
    // 恢复渠道状态
    function restoreChannelState() {
      try {
        const saved = localStorage.getItem('trend.visibleChannels');
        if (saved) {
          const visibleArray = JSON.parse(saved);
          window.visibleChannels = new Set(visibleArray);
        }
      } catch (_) {}
    }

    // 页面初始化
    document.addEventListener('DOMContentLoaded', async function() {
      if (window.initTopbar) initTopbar('trend');

      // ✅ 优先从 URL 参数恢复渠道类型，否则从 localStorage，默认 all
      const urlParams = new URLSearchParams(location.search);
      const hasUrlParams = urlParams.toString().length > 0;
      const savedChannelType = urlParams.get('channel_type') || (!hasUrlParams && localStorage.getItem('trend.channelType')) || 'all';
      window.currentChannelType = savedChannelType;

      await initChannelTypeFilter(window.currentChannelType);

      restoreState();
      restoreChannelState();
      applyRangeUI();
      bindToggles();

      // 加载模型列表
      await loadModels();

      // 加载令牌列表
      await loadAuthTokens();

      loadData();

      // 修复：全局注册resize监听器（仅一次，避免内存泄漏）
      window.addEventListener('resize', () => {
        if (window.chartInstance) {
          window.chartInstance.resize();
        }
      });

      // 定期刷新数据（每5分钟）
      setInterval(loadData, 5 * 60 * 1000);
    });

    // 初始化渠道类型筛选器
    async function initChannelTypeFilter(initialType) {
      const select = document.getElementById('f_channel_type');
      if (!select) return;

      const types = await window.ChannelTypeManager.getChannelTypes();

      // 添加"全部"选项
      select.innerHTML = '<option value="all">全部</option>';
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
        window.currentChannelType = e.target.value;
        try {
          localStorage.setItem('trend.channelType', e.target.value);
          updateURLParams(); // ✅ 同步到 URL 参数
        } catch (_) {}
        // 切换渠道类型时重新加载数据并清除渠道选择状态
        window.visibleChannels.clear();
        loadData();
      });
    }

    function bindToggles() {
      // 趋势类型切换
      const trendTypeGroup = document.getElementById('trend-type-group');
      trendTypeGroup.addEventListener('click', (e) => {
        const t = e.target.closest('.toggle-btn');
        if (!t) return;
        trendTypeGroup.querySelectorAll('.toggle-btn').forEach(btn => btn.classList.remove('active'));
        t.classList.add('active');
        const trendType = t.getAttribute('data-type') || 'first_byte';
        window.currentTrendType = trendType;
        persistState();
        renderChart();
      });

      // 时间范围选择 - 使用 f_hours 元素
      const rangeSelect = document.getElementById('f_hours');
      if (rangeSelect) {
        rangeSelect.addEventListener('change', (e) => {
          const range = e.target.value;
          window.currentRange = range;
          const label = document.getElementById('data-timerange');
          if (label) {
            const rangeLabel = window.getRangeLabel ? getRangeLabel(range) : range;
            label.textContent = `${rangeLabel}数据展示`;
          }
          persistState();
          loadData();
        });
      }

      // 模型选择器
      const modelSelect = document.getElementById('f_model');
      if (modelSelect) {
        modelSelect.addEventListener('change', (e) => {
          window.currentModel = e.target.value || '';
          persistState();
          loadData();
        });
      }

      // 令牌选择器
      const tokenSelect = document.getElementById('f_auth_token');
      if (tokenSelect) {
        tokenSelect.addEventListener('change', (e) => {
          window.currentAuthToken = e.target.value || '';
          persistState();
          loadData();
        });
      }

      // 筛选按钮
      const btnFilter = document.getElementById('btn_filter');
      if (btnFilter) {
        btnFilter.addEventListener('click', () => {
          loadData();
        });
      }

      // 输入框自动筛选（防抖）
      const debouncedFilter = debounce(() => {
        // 读取输入框值到全局变量
        const idInput = document.getElementById('f_id');
        if (idInput) window.currentChannelId = idInput.value.trim() || '';
        const nameInput = document.getElementById('f_name');
        if (nameInput) window.currentChannelName = nameInput.value.trim() || '';

        persistState();
        loadData();
      }, 500);
      ['f_id', 'f_name'].forEach(id => {
        const el = document.getElementById(id);
        if (el) {
          el.addEventListener('input', debouncedFilter);
        }
      });

      // 回车键筛选
      ['f_id', 'f_name'].forEach(id => {
        const el = document.getElementById(id);
        if (el) {
          el.addEventListener('keydown', e => {
            if (e.key === 'Enter') {
              // 读取输入框值到全局变量
              const idInput = document.getElementById('f_id');
              if (idInput) window.currentChannelId = idInput.value.trim() || '';
              const nameInput = document.getElementById('f_name');
              if (nameInput) window.currentChannelName = nameInput.value.trim() || '';

              persistState();
              loadData();
            }
          });
        }
      });
    }

    function persistState() {
      try {
        localStorage.setItem('trend.range', window.currentRange);
        localStorage.setItem('trend.trendType', window.currentTrendType);
        localStorage.setItem('trend.model', window.currentModel);
        localStorage.setItem('trend.authToken', window.currentAuthToken);
        localStorage.setItem('trend.channelId', window.currentChannelId || '');
        localStorage.setItem('trend.channelName', window.currentChannelName || '');

        // ✅ 新增：同步到 URL 参数（不刷新页面）
        updateURLParams();
      } catch (_) {}
    }

    function updateURLParams() {
      try {
        const params = new URLSearchParams();
        if (window.currentRange && window.currentRange !== 'today') {
          params.set('range', window.currentRange);
        }
        if (window.currentTrendType && window.currentTrendType !== 'first_byte') {
          params.set('type', window.currentTrendType);
        }
        if (window.currentModel) {
          params.set('model', window.currentModel);
        }
        if (window.currentAuthToken) {
          params.set('token', window.currentAuthToken);
        }
        if (window.currentChannelType && window.currentChannelType !== 'all') {
          params.set('channel_type', window.currentChannelType);
        }

        const newSearch = params.toString();
        const newUrl = newSearch ? `?${newSearch}` : location.pathname;
        history.replaceState(null, '', newUrl);
      } catch (_) {}
    }

    function restoreState() {
      try {
        // ✅ 优先从 URL 参数恢复，否则从 localStorage 恢复
        const urlParams = new URLSearchParams(location.search);
        const hasUrlParams = urlParams.toString().length > 0;

        // 恢复时间范围 (默认"本日")
        let savedRange = urlParams.get('range') || (!hasUrlParams && localStorage.getItem('trend.range')) || 'today';
        const validRanges = ['today', 'yesterday', 'day_before_yesterday', 'this_week', 'last_week', 'this_month', 'last_month'];
        window.currentRange = validRanges.includes(savedRange) ? savedRange : 'today';

        const label = document.getElementById('data-timerange');
        if (label) {
          const rangeLabel = window.getRangeLabel ? getRangeLabel(window.currentRange) : window.currentRange;
          label.textContent = `${rangeLabel}数据展示`;
        }

        // 恢复趋势类型
        let savedType = urlParams.get('type') || (!hasUrlParams && localStorage.getItem('trend.trendType')) || 'first_byte';
        if (['count', 'rpm', 'first_byte', 'duration', 'tokens', 'cost'].includes(savedType)) {
          window.currentTrendType = savedType;
        }

        // 恢复模型选择
        window.currentModel = urlParams.get('model') || (!hasUrlParams && localStorage.getItem('trend.model')) || '';

        // 恢复令牌选择
        window.currentAuthToken = urlParams.get('token') || (!hasUrlParams && localStorage.getItem('trend.authToken')) || '';

        // 恢复渠道ID和渠道名
        window.currentChannelId = urlParams.get('channel_id') || (!hasUrlParams && localStorage.getItem('trend.channelId')) || '';
        window.currentChannelName = urlParams.get('channel_name_like') || (!hasUrlParams && localStorage.getItem('trend.channelName')) || '';

        // 同步到输入框
        const idInput = document.getElementById('f_id');
        if (idInput && window.currentChannelId) {
          idInput.value = window.currentChannelId;
        }
        const nameInput = document.getElementById('f_name');
        if (nameInput && window.currentChannelName) {
          nameInput.value = window.currentChannelName;
        }
      } catch (_) {}
    }

    function applyRangeUI() {
      // 初始化时间范围选择器 (默认"本日")
      if (window.initDateRangeSelector) {
        initDateRangeSelector('f_hours', 'today', null);
        // 设置已保存的值
        document.getElementById('f_hours').value = window.currentRange;
      }

      // 应用趋势类型UI
      const trendTypeGroup = document.getElementById('trend-type-group');
      if (trendTypeGroup) {
        trendTypeGroup.querySelectorAll('.toggle-btn').forEach(btn => {
          const type = btn.getAttribute('data-type') || 'first_byte';
          btn.classList.toggle('active', type === window.currentTrendType);
        });
      }
    }

    // 注销功能（已由 ui.js 的 onLogout 统一处理）
