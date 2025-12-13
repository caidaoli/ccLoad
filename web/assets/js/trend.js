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
        const res = await fetchWithAuth('/admin/models?range=this_month');
        if (!res.ok) {
          console.error('加载模型列表失败');
          return;
        }
        const data = await res.json();
        window.availableModels = data.data || [];

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
        const res = await fetchWithAuth('/admin/auth-tokens');
        if (!res.ok) {
          console.error('加载令牌列表失败');
          return;
        }
        const response = await res.json();
        window.authTokens = response.success ? (response.data || []) : (response || []);

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
        showLoading();

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

        const [metricsRes, channelsRes] = await Promise.all([
          fetchWithAuth(metricsUrl + channelTypeParam + modelParam + tokenParam + channelIdParam + channelNameParam),
          fetchWithAuth(channelsUrl + (channelTypeParamForList ? '?' + channelTypeParamForList.slice(1) : ''))
        ]);
        
        if (!metricsRes.ok) throw new Error(`HTTP ${metricsRes.status}`);
        if (!channelsRes.ok) throw new Error(`获取渠道列表失败: ${channelsRes.status}`);
        
        const metricsResponse = await metricsRes.json();
        const channelsResponse = await channelsRes.json();
        
        window.trendData = metricsResponse.success ? (metricsResponse.data || []) : (metricsResponse || []);
        window.channels = channelsResponse.success ? (channelsResponse.data || []) : (channelsResponse || []);
        
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
        const debugSince = metricsRes.headers.get('X-Debug-Since');
        const debugPoints = metricsRes.headers.get('X-Debug-Points');
        const debugTotal = metricsRes.headers.get('X-Debug-Total');

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
        showError();
      }
    }

    function computeBucketMin(hours) {
      if (hours <= 1) return 1; // 1分钟
      if (hours <= 6) return 2; // 2分钟
      if (hours <= 24) return 5; // 5分钟
      if (hours <= 72) return 15; // 15分钟
      return 60; // 1小时
    }

    function showLoading() {
      document.getElementById('chart-loading').style.display = 'flex';
      document.getElementById('chart-error').style.display = 'none';
      document.getElementById('chart').style.display = 'none';
    }

    function showError() {
      document.getElementById('chart-loading').style.display = 'none';
      document.getElementById('chart-error').style.display = 'flex';
      document.getElementById('chart').style.display = 'none';
    }

    function renderChart() {
      if (!window.trendData || !window.trendData.length) {
        showError();
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

      // 为每个可见渠道生成颜色
      const channelColors = generateChannelColors(window.visibleChannels);

      // 准备series数据
      const series = [];
      const trendType = window.currentTrendType;

      // 根据趋势类型准备不同的总体数据
      if (trendType === 'count') {
        // 调用次数趋势：添加总体成功/失败线
        series.push({
          name: '总成功请求',
          type: 'line',
          smooth: true,
          symbol: 'circle',
          symbolSize: 4,
          sampling: 'lttb',
          connectNulls: true,
          itemStyle: {
            color: '#10b981'
          },
          lineStyle: {
            width: 2,
            color: '#10b981'
          },
          data: window.trendData.map(point => point.success || 0)
        });

        series.push({
          name: '总失败请求',
          type: 'line',
          smooth: true,
          symbol: 'circle',
          symbolSize: 4,
          sampling: 'lttb',
          connectNulls: true,
          itemStyle: {
            color: '#ef4444'
          },
          lineStyle: {
            width: 2,
            color: '#ef4444'
          },
          data: window.trendData.map(point => point.error || 0)
        });
      } else if (trendType === 'first_byte') {
        // 首字响应时间趋势：添加总体平均首字响应时间线
        series.push({
          name: '平均首字响应时间',
          type: 'line',
          smooth: true,
          symbol: 'circle',
          symbolSize: 4,
          sampling: 'lttb',
          connectNulls: true,
          itemStyle: {
            color: '#0ea5e9'
          },
          lineStyle: {
            width: 2,
            color: '#0ea5e9'
          },
          data: window.trendData.map(point => {
            const fbt = point.avg_first_byte_time_seconds;
            return (fbt != null && fbt > 0) ? (fbt * 1000) : null; // 转换为毫秒
          })
        });
      } else if (trendType === 'duration') {
        // 总耗时趋势：添加总体平均总耗时线
        series.push({
          name: '平均总耗时',
          type: 'line',
          smooth: true,
          symbol: 'circle',
          symbolSize: 4,
          sampling: 'lttb',
          connectNulls: true,
          itemStyle: {
            color: '#a855f7'
          },
          lineStyle: {
            width: 2,
            color: '#a855f7'
          },
          data: window.trendData.map(point => {
            const dur = point.avg_duration_seconds;
            return (dur != null && dur > 0) ? (dur * 1000) : null; // 转换为毫秒
          })
        });
      } else if (trendType === 'tokens') {
        // Token用量趋势：添加输入、输出、缓存读、缓存建四条线
        series.push({
          name: '输入Token',
          type: 'line',
          smooth: true,
          symbol: 'circle',
          symbolSize: 4,
          sampling: 'lttb',
          connectNulls: true,
          itemStyle: { color: '#3b82f6' },
          lineStyle: { width: 2, color: '#3b82f6' },
          data: window.trendData.map(point => point.input_tokens > 0 ? point.input_tokens : null)
        });
        series.push({
          name: '输出Token',
          type: 'line',
          smooth: true,
          symbol: 'circle',
          symbolSize: 4,
          sampling: 'lttb',
          connectNulls: true,
          itemStyle: { color: '#10b981' },
          lineStyle: { width: 2, color: '#10b981' },
          data: window.trendData.map(point => point.output_tokens > 0 ? point.output_tokens : null)
        });
        series.push({
          name: '缓存读取',
          type: 'line',
          smooth: true,
          symbol: 'circle',
          symbolSize: 4,
          sampling: 'lttb',
          connectNulls: true,
          itemStyle: { color: '#f97316' },
          lineStyle: { width: 2, color: '#f97316' },
          data: window.trendData.map(point => point.cache_read_tokens > 0 ? point.cache_read_tokens : null)
        });
        series.push({
          name: '缓存创建',
          type: 'line',
          smooth: true,
          symbol: 'circle',
          symbolSize: 4,
          sampling: 'lttb',
          connectNulls: true,
          itemStyle: { color: '#a855f7' },
          lineStyle: { width: 2, color: '#a855f7' },
          data: window.trendData.map(point => point.cache_creation_tokens > 0 ? point.cache_creation_tokens : null)
        });
      } else if (trendType === 'cost') {
        // 费用消耗趋势：添加总体费用线
        series.push({
          name: '总费用',
          type: 'line',
          smooth: true,
          symbol: 'circle',
          symbolSize: 4,
          sampling: 'lttb',
          connectNulls: true,
          itemStyle: {
            color: '#f97316'
          },
          lineStyle: {
            width: 2,
            color: '#f97316'
          },
          data: window.trendData.map(point => {
            const cost = point.total_cost;
            return (cost != null && cost > 0) ? cost : null;
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
              smooth: true,
              symbol: 'none',
              sampling: 'lttb',
              connectNulls: true,
              itemStyle: { color: color },
              lineStyle: { width: 1.5, color: color, type: 'solid' },
              data: successData
            });
          }

          // 失败线
          if (errorTotal > 0) {
            series.push({
              name: `${channelName}(失败)`,
              type: 'line',
              smooth: true,
              symbol: 'none',
              sampling: 'lttb',
              connectNulls: true,
              itemStyle: { color: color },
              lineStyle: { width: 1.5, color: color, type: 'dashed' },
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
              return fbt * 1000; // 转换为毫秒
            }
            return null;
          });

          if (hasData) {
            series.push({
              name: channelName,
              type: 'line',
              smooth: true,
              symbol: 'none',
              sampling: 'lttb',
              connectNulls: true,
              itemStyle: { color: color },
              lineStyle: { width: 1.5, color: color },
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
              return dur * 1000; // 转换为毫秒
            }
            return null;
          });

          if (hasData) {
            series.push({
              name: channelName,
              type: 'line',
              smooth: true,
              symbol: 'none',
              sampling: 'lttb',
              connectNulls: true,
              itemStyle: { color: color },
              lineStyle: { width: 1.5, color: color },
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
              smooth: true,
              symbol: 'none',
              sampling: 'lttb',
              connectNulls: true,
              itemStyle: { color: color },
              lineStyle: { width: 1.5, color: color },
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
              smooth: true,
              symbol: 'none',
              sampling: 'lttb',
              connectNulls: true,
              itemStyle: { color: color },
              lineStyle: { width: 1.5, color: color },
              data: costData
            });
          }
        }
      });

      // ECharts 配置
      const option = {
        backgroundColor: 'transparent',
        title: {
          show: false
        },
        tooltip: {
          trigger: 'axis',
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
            let html = `<div style="font-weight: 600; margin-bottom: 8px;">${params[0].axisValue}</div>`;
            params.forEach(param => {
              const color = param.color;
              const value = param.value;
              let formattedValue;

              // 根据当前趋势类型格式化数值
              if (value == null) {
                formattedValue = 'N/A';
              } else if (window.currentTrendType === 'first_byte' || window.currentTrendType === 'duration') {
                // 首字响应时间/总耗时：已转换为毫秒
                formattedValue = value.toFixed(0) + 'ms';
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
          right: 20,
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
          left: '3%',
          right: '3%',
          bottom: '12%',
          top: '20%',
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
          axisLabel: {
            color: '#6b7280',
            fontSize: 11,
            rotate: window.currentHours > 24 ? 45 : 0,
            interval: Math.floor(timestamps.length / 10) // 动态间隔
          },
          splitLine: {
            show: true,
            lineStyle: {
              color: '#f3f4f6',
              type: 'dashed'
            }
          }
        },
        yAxis: {
          type: 'value',
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
                // 首字响应时间/总耗时：毫秒格式
                return value.toFixed(0) + 'ms';
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
              color: '#f3f4f6',
              type: 'dashed'
            }
          }
        },
        series: series,
        dataZoom: window.currentHours > 24 ? [
          {
            type: 'inside',
            start: 0,
            end: 100,
            minValueSpan: 10
          },
          {
            show: true,
            type: 'slider',
            bottom: '2%',
            start: 0,
            end: 100,
            height: 20,
            borderColor: '#e5e7eb',
            fillerColor: 'rgba(59, 130, 246, 0.15)',
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

    function formatNumber(num) {
      if (num >= 1000000) return (num / 1000000).toFixed(1) + 'M';
      if (num >= 1000) return (num / 1000).toFixed(1) + 'K';
      return num.toString();
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
        const item = document.createElement('div');
        item.className = 'channel-filter-item';
        item.onclick = () => toggleChannel(channelName);

        const isVisible = window.visibleChannels.has(channelName);

        // 为"未知渠道"添加特殊标识
        const displayName = channelName === '未知渠道'
          ? `${channelName} ⚠️`
          : channelName;

        const content = TemplateEngine.render('tpl-channel-filter-item', {
          checkedClass: isVisible ? 'checked' : '',
          color: channelColors[channelName],
          displayName: displayName
        });
        if (content) {
          item.innerHTML = '';
          item.appendChild(content);
        }

        filterList.appendChild(item);
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
      const debouncedFilter = debounce(loadData, 500);
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
            if (e.key === 'Enter') loadData();
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
        if (['count', 'first_byte', 'duration', 'tokens', 'cost'].includes(savedType)) {
          window.currentTrendType = savedType;
        }

        // 恢复模型选择
        window.currentModel = urlParams.get('model') || (!hasUrlParams && localStorage.getItem('trend.model')) || '';

        // 恢复令牌选择
        window.currentAuthToken = urlParams.get('token') || (!hasUrlParams && localStorage.getItem('trend.authToken')) || '';
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
