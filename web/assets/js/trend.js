    // 全局变量
    const t = window.t;

    window.trendData = null;
    window.currentRange = 'today'; // 默认"本日"
    const DEFAULT_TREND_TYPE = 'count'; // count/rpm/first_byte/duration/tokens/cost
    window.currentTrendType = DEFAULT_TREND_TYPE;
    window.currentTrendChartType = 'line'; // 默认使用折线图，可切换为柱状图
    window.currentModel = ''; // 当前模型筛选（空字符串表示全部模型）
    let trendExactModelValue = ''; // 从 URL(model=) / 本地存储恢复的精确模型值，选项加载前据此判定精确匹配
    window.currentAuthToken = ''; // 当前选中的令牌（空字符串表示全部令牌）
    window.currentClientProtocol = ''; // 当前选中的客户端入口协议
    window.currentChannelName = ''; // 当前选中的渠道名称
    let currentTrendCustomTimeRange = null;
    window.chartInstance = null;
    window.channels = [];
    window.visibleChannels = new Set(); // 可见渠道集合
    let trendChannelNameCombobox = null; // 渠道名筛选组合框
    let trendModelCombobox = null; // 模型筛选组合框（与统计/日志一致：选中选项精确匹配，自由输入模糊匹配）
    window.availableModels = []; // 可用模型列表
    window.authTokens = []; // 令牌列表

    function getTrendChartTheme() {
      return typeof window.getChartTheme === 'function'
        ? window.getChartTheme()
        : {
          text: '#374151',
          mutedText: '#6b7280',
          strongText: '#111827',
          axisLine: '#e5e7eb',
          splitLine: 'rgba(148, 163, 184, 0.25)',
          surface: '#ffffff',
          surfaceMuted: 'rgba(148, 163, 184, 0.10)',
          tooltipBg: 'rgba(255, 255, 255, 0.98)',
          tooltipBorder: 'rgba(17, 24, 39, 0.16)',
          tooltipText: '#111827'
        };
    }

    const TREND_FILTER_KEY = 'trend.filters';
    const TREND_FILTER_FIELDS = [
      {
        key: 'range',
        queryKeys: ['range'],
        defaultValue: 'today',
        includeInQuery(value) {
          return Boolean(value) && value !== 'today';
        }
      },
      {
        key: 'customStartTime',
        queryKeys: ['start_time'],
        defaultValue: '',
        includeInQuery(value, values) {
          return values?.range === 'custom' && Boolean(value);
        },
        includeInRequest() {
          return false;
        }
      },
      {
        key: 'customEndTime',
        queryKeys: ['end_time'],
        defaultValue: '',
        includeInQuery(value, values) {
          return values?.range === 'custom' && Boolean(value);
        },
        includeInRequest() {
          return false;
        }
      },
      {
        key: 'trendType',
        queryKeys: ['type'],
        defaultValue: DEFAULT_TREND_TYPE,
        includeInQuery(value) {
          return Boolean(value) && value !== DEFAULT_TREND_TYPE;
        },
        includeInRequest() {
          return false;
        }
      },
      { key: 'clientProtocol', queryKeys: ['client_protocol'], defaultValue: '' },
      {
        key: 'model',
        queryKeys: ['model', 'model_like'],
        paramKey: getTrendModelFilterKey,
        requestKey: getTrendModelFilterKey,
        defaultValue: ''
      },
      { key: 'authToken', queryKeys: ['auth_token_id'], defaultValue: '' },
      {
        key: 'channelName',
        queryKeys: ['channel_name_like'],
        defaultValue: '',
        includeInQuery() {
          return false;
        }
      }
    ];
    const TREND_MODELS_REQUEST_FIELDS = TREND_FILTER_FIELDS.filter((field) => field.key === 'range');

    function getTrendFilters() {
      const range = window.currentRange || 'today';
      const hasCustomRange = range === 'custom' && currentTrendCustomTimeRange;
      return {
        range,
        customStartTime: hasCustomRange ? String(currentTrendCustomTimeRange.startMs) : '',
        customEndTime: hasCustomRange ? String(currentTrendCustomTimeRange.endMs) : '',
        trendType: window.currentTrendType || DEFAULT_TREND_TYPE,
        clientProtocol: window.currentClientProtocol || '',
        model: window.currentModel || '',
        modelExact: isExactTrendModelFilter(window.currentModel),
        authToken: window.currentAuthToken || '',
        channelName: window.currentChannelName || ''
      };
    }

    function normalizeTrendModelValue(value) {
      return String(value || '').trim().toLowerCase();
    }

    // 命中模型选项或恢复的精确值 → model=；否则视为自由输入 → model_like=
    function isExactTrendModelFilter(value) {
      const normalized = normalizeTrendModelValue(value);
      if (!normalized) return false;
      if (normalized === normalizeTrendModelValue(trendExactModelValue)) return true;
      return (window.availableModels || []).some((model) => normalizeTrendModelValue(model) === normalized);
    }

    function getTrendModelFilterKey(value, values) {
      return (values && values.modelExact) || isExactTrendModelFilter(value) ? 'model' : 'model_like';
    }

    function loadSavedTrendFilters(storage = window.localStorage) {
      return window.FilterState.load(TREND_FILTER_KEY, storage);
    }

    function normalizeTrendCustomTimeRange(range) {
      if (!range || typeof range !== 'object') return null;

      const startMs = Number(range.startMs ?? range.customStartTime);
      const endMs = Number(range.endMs ?? range.customEndTime);
      if (!Number.isFinite(startMs) || !Number.isFinite(endMs) || endMs <= startMs) {
        return null;
      }
      return {
        startMs: Math.trunc(startMs),
        endMs: Math.trunc(endMs),
        label: range.label || ''
      };
    }

    function appendTrendTimeRangeParams(params, filters) {
      const range = filters?.range || 'today';
      const query = typeof window.buildDateRangeQuery === 'function'
        ? window.buildDateRangeQuery(range, currentTrendCustomTimeRange)
        : `range=${encodeURIComponent(range)}`;
      new URLSearchParams(query).forEach((value, key) => {
        params.set(key, value);
      });
      return params;
    }

    function getTrendRangeHours(range) {
      if (range === 'custom' && currentTrendCustomTimeRange) {
        return Math.max((currentTrendCustomTimeRange.endMs - currentTrendCustomTimeRange.startMs) / 3600000, 1 / 60);
      }
      return window.getRangeHours ? getRangeHours(range) : 24;
    }

    function buildTrendRequestParams(baseParams = {}) {
      const params = window.FilterQuery.buildRequestParams(getTrendFilters(), TREND_FILTER_FIELDS, {
        baseParams
      });
      appendTrendTimeRangeParams(params, getTrendFilters());
      return params;
    }

    // 加载当前时间范围内的可用模型和渠道列表（仅用于筛选下拉选项）
    async function loadModels(range) {
      try {
        const filters = {
          ...getTrendFilters(),
          range: range || window.currentRange || 'today'
        };
        const params = window.FilterQuery.buildRequestParams(filters, TREND_MODELS_REQUEST_FIELDS);
        appendTrendTimeRangeParams(params, filters);
        const url = `/dashboard/models?${params.toString()}`;

        const resp = await fetchDataWithAuth(url) || {};
        const rawModels = Array.isArray(resp.models) ? resp.models : [];
        const rawChannels = Array.isArray(resp.channels) ? resp.channels : [];

        // 去重：使用 Set 确保模型名称唯一
        window.availableModels = [...new Set(rawModels)];

        // 更新渠道列表（仅有日志数据的渠道）
        window.channels = rawChannels;
        if (trendChannelNameCombobox) trendChannelNameCombobox.refresh();
        if (trendModelCombobox) trendModelCombobox.refresh();
      } catch (error) {
        console.error('[Trend] load models failed:', error);
      }
    }

    // 模型/渠道选项与指标互不依赖，并行拉取
    async function loadModelsAndData(range) {
      await Promise.all([loadData(), loadModels(range)]);
    }

    let trendLoadSeq = 0;
    let trendBucketInfo = null; // 最近一次成功加载的分桶信息，语言切换时重渲染提示文本

    // background=true 为自动刷新：不显示加载态、失败时保留现有图表，图表原地合并更新
    async function loadData(options = {}) {
      const background = options.background === true && Boolean(window.chartInstance) && Boolean(window.trendData);
      const seq = ++trendLoadSeq;
      try {
        if (!background) renderTrendLoading();

        // 从 DOM 元素读取当前选择的时间范围和模型
        const rangeSelect = document.getElementById('f_hours');
        const currentRange = rangeSelect ? rangeSelect.value : (window.currentRange || 'today');
        window.currentRange = currentRange; // 同步到全局变量
        if (currentRange !== 'custom') {
          currentTrendCustomTimeRange = null;
        }

        // 模型/令牌以 window 状态为准：两者的下拉选项异步填充，填充前读 DOM 会把已恢复的筛选冲掉
        const clientProtocolSelect = document.getElementById('f_client_protocol');
        if (clientProtocolSelect) {
          window.currentClientProtocol = clientProtocolSelect.value || '';
        }

        // 读取渠道名/模型筛选（combobox）
        if (trendChannelNameCombobox) window.currentChannelName = trendChannelNameCombobox.getValue();
        if (trendModelCombobox) window.currentModel = trendModelCombobox.getValue();

        const hours = getTrendRangeHours(currentRange);
        window.currentHours = hours; // 同步到全局变量，供 renderChart 使用
        const bucketMin = computeBucketMin(hours);

        const metricsParams = buildTrendRequestParams({
          bucket_min: bucketMin,
          by_channel: '1'
        });
        const metrics = await fetchAPIWithAuthRaw('/dashboard/metrics?' + metricsParams.toString());
        // 筛选连续变化时丢弃过期响应，避免旧数据覆盖新数据
        if (seq !== trendLoadSeq) return;

        if (!metrics.payload.success) {
          throw new Error(metrics.payload.error || t('trend.fetchDataFailed'));
        }

        window.trendData = metrics.payload.data || [];

        // 构建渠道数据缓存（一次遍历，供后续 hasChannelData 使用）
        buildChannelDataCache(window.trendData);

        // 清理本地存储中已无数据的渠道选择；为空时只显示总数
        if (window.visibleChannels.size > 0) {
          const validChannels = new Set();
          window.visibleChannels.forEach(channelName => {
            if (hasChannelData(channelName, window.trendData)) {
              validChannels.add(channelName);
            }
          });
          window.visibleChannels = validChannels;
          persistChannelState();
        }

        updateChannelFilter();
        renderChart({ merge: background });

        trendBucketInfo = {
          bucketMin,
          points: window.trendData.length,
          total: sumTrendRequests(window.trendData)
        };
        renderTrendInfo();

      } catch (error) {
        if (seq !== trendLoadSeq) return;
        console.error('[Trend] load data failed:', error);
        // 后台刷新失败保留已有图表，等待下一轮
        if (background) return;
        try { if (window.showError) window.showError(t('trend.loadDataFailed')); } catch(_){}
        renderTrendError();
      }
    }

    function renderTrendInfo() {
      const iv = document.getElementById('bucket-interval');
      if (iv) {
        iv.textContent = trendBucketInfo
          ? t('trend.dataInterval', {
            interval: formatInterval(trendBucketInfo.bucketMin),
            points: trendBucketInfo.points,
            total: formatNumber(trendBucketInfo.total)
          })
          : '--';
      }
      const label = document.getElementById('data-timerange');
      if (label) {
        const range = window.currentRange || 'today';
        const rangeLabel = window.getRangeLabel ? getRangeLabel(range) : range;
        label.textContent = t('trend.dataDisplay', { range: rangeLabel });
      }
    }

    function sumTrendRequests(points) {
      return (points || []).reduce((sum, point) => sum + (point.success || 0) + (point.error || 0), 0);
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

    // merge=true（后台刷新）时原地合并配置，保留缩放区间，避免整图重绘闪烁
    function renderChart(options = {}) {
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

      // 准备时间数据：类目值统一为 MM-DD HH:mm（tooltip 直接使用），≤24h 时轴标签只显示 HH:mm
      const trendData = window.trendData;
      const dataLen = trendData.length;
      const timestamps = new Array(dataLen);
      const useShortFormat = window.currentHours <= 24;

      for (let i = 0; i < dataLen; i++) {
        const point = trendData[i];
        timestamps[i] = window.formatMonthDayTime(point.ts || point.Ts);
      }

      const noRequestRanges = computeNoRequestRanges(trendData);
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
          name: t('trend.totalSuccess'),
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
          name: t('trend.totalFailed'),
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
	          name: t('trend.avgFirstByteTime'),
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
          name: t('trend.avgDuration'),
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
          name: t('trend.inputTokens'),
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
          name: t('trend.outputTokens'),
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
          name: t('trend.cacheRead'),
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
          name: t('trend.cacheCreate'),
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
          name: t('trend.totalCost'),
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
      // 优化：使用 for 循环替代 forEach，预分配数组
      const visibleChannelsArray = Array.from(window.visibleChannels);
      const visibleCount = visibleChannelsArray.length;

      for (let ci = 0; ci < visibleCount; ci++) {
        const channelName = visibleChannelsArray[ci];
        const color = channelColors[channelName];

        if (trendType === 'count') {
          // 调用次数趋势：渠道成功/失败线
          // 优化：单次遍历同时提取 success 和 error 数据
          const successData = new Array(dataLen);
          const errorData = new Array(dataLen);
          let successTotal = 0;
          let errorTotal = 0;

          for (let i = 0; i < dataLen; i++) {
            const channels = trendData[i].channels;
            const channelData = channels ? channels[channelName] : null;
            const success = channelData ? (channelData.success || 0) : 0;
            const error = channelData ? (channelData.error || 0) : 0;
            successData[i] = success;
            errorData[i] = error;
            successTotal += success;
            errorTotal += error;
          }

          // 成功线
          if (successTotal > 0) {
            series.push({
              name: t('trend.channelSuccess', { channel: channelName }),
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
              name: t('trend.channelFailed', { channel: channelName }),
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
          const fbtData = new Array(dataLen);
          let hasData = false;

          for (let i = 0; i < dataLen; i++) {
            const channels = trendData[i].channels;
            const channelData = channels ? channels[channelName] : null;
            const fbt = channelData ? channelData.avg_first_byte_time_seconds : null;
            if (fbt != null && fbt > 0) {
              fbtData[i] = fbt;
              hasData = true;
            } else {
              fbtData[i] = null;
            }
          }

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
          const durData = new Array(dataLen);
          let hasData = false;

          for (let i = 0; i < dataLen; i++) {
            const channels = trendData[i].channels;
            const channelData = channels ? channels[channelName] : null;
            const dur = channelData ? channelData.avg_duration_seconds : null;
            if (dur != null && dur > 0) {
              durData[i] = dur;
              hasData = true;
            } else {
              durData[i] = null;
            }
          }

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
          const tokenData = new Array(dataLen);
          let hasData = false;

          for (let i = 0; i < dataLen; i++) {
            const channels = trendData[i].channels;
            const channelData = channels ? channels[channelName] : null;
            const total = channelData ? ((channelData.input_tokens || 0) + (channelData.output_tokens || 0)) : 0;
            if (total > 0) {
              tokenData[i] = total;
              hasData = true;
            } else {
              tokenData[i] = null;
            }
          }

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
          const costData = new Array(dataLen);
          let hasData = false;

          for (let i = 0; i < dataLen; i++) {
            const channels = trendData[i].channels;
            const channelData = channels ? channels[channelName] : null;
            const cost = channelData ? channelData.total_cost : null;
            if (cost != null && cost > 0) {
              costData[i] = cost;
              hasData = true;
            } else {
              costData[i] = null;
            }
          }

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
          const rpmData = new Array(dataLen);
          let hasData = false;

          for (let i = 0; i < dataLen; i++) {
            const channels = trendData[i].channels;
            const channelData = channels ? channels[channelName] : null;
            const total = channelData ? ((channelData.success || 0) + (channelData.error || 0)) : 0;
            if (total > 0) {
              rpmData[i] = total / bucketMin;
              hasData = true;
            } else {
              rpmData[i] = null;
            }
          }

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
      }

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
      const chartTheme = getTrendChartTheme();

      const chartType = window.currentTrendChartType === 'bar' ? 'bar' : 'line';
      const option = {
        backgroundColor: 'transparent',
        title: {
          show: false
        },
        tooltip: {
          trigger: 'axis',
          confine: true,
          backgroundColor: chartTheme.tooltipBg,
          borderColor: chartTheme.tooltipBorder,
          borderWidth: 1,
          textStyle: {
            color: chartTheme.tooltipText,
            fontSize: 12
          },
          axisPointer: {
            type: chartType === 'bar' ? 'shadow' : 'cross',
            crossStyle: {
              color: chartTheme.mutedText,
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
                ? `<span style="color: ${chartTheme.mutedText};">${t('trend.noRequestInPeriod')}</span>`
                : '';
              html += `<div style="margin-bottom: 8px; color: ${chartTheme.text}; font-size: 12px;">${t('trend.requestCount')}: ${totalReq}${hint}</div>`;
            }
            params.forEach(param => {
              const color = param.color;
              const value = param.value;
              let formattedValue;

              // 根据当前趋势类型格式化数值
              if (value == null) {
                formattedValue = 'N/A';
              } else if (window.currentTrendType === 'first_byte' || window.currentTrendType === 'duration') {
                // 首块响应体时间/总耗时：秒
                formattedValue = value.toFixed(1) + 's';
              } else if (window.currentTrendType === 'cost') {
                formattedValue = window.formatCost(value);
              } else if (window.currentTrendType === 'tokens') {
                formattedValue = window.formatNumber(value);
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
            color: chartTheme.mutedText,
            fontSize: 11
          },
          itemWidth: 20,
          itemHeight: 8,
          itemGap: 12,
          type: 'scroll',
          pageIconColor: chartTheme.mutedText,
          pageIconInactiveColor: chartTheme.axisLine,
          pageIconSize: 12,
          pageTextStyle: {
            color: chartTheme.mutedText,
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
          boundaryGap: chartType === 'bar',
          data: timestamps,
          axisLine: {
            lineStyle: {
              color: chartTheme.axisLine
            }
          },
          axisTick: {
            alignWithLabel: true,
            lineStyle: { color: chartTheme.axisLine }
          },
          axisLabel: {
            color: chartTheme.mutedText,
            fontSize: 11,
            rotate: xAxisRotate,
            hideOverlap: true,
            interval: xAxisLabelInterval,
            formatter: useShortFormat ? (value) => String(value).slice(6) : null
          },
          splitLine: {
            show: true,
            lineStyle: {
              color: chartTheme.splitLine,
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
              color: chartTheme.axisLine
            }
          },
          axisLabel: {
            color: chartTheme.mutedText,
            fontSize: 11,
            formatter: function(value) {
              if (trendType === 'first_byte' || trendType === 'duration') {
                // 首块响应体时间/总耗时：秒格式
                return value.toFixed(1) + 's';
              } else if (trendType === 'cost') {
                return window.formatCost(value);
              } else if (trendType === 'tokens') {
                return window.formatNumber(value);
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
              color: chartTheme.splitLine,
              type: 'dashed'
            }
          }
        },
        graphic: buildTrendEmptyGraphic(series, trendType, chartTheme),
        series: applyNoRequestMarkArea(applyTrendChartType(series, chartType, trendType), markAreaData),
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
            borderColor: chartTheme.axisLine,
            backgroundColor: chartTheme.surfaceMuted,
            fillerColor: 'rgba(59, 130, 246, 0.16)',
            handleStyle: {
              color: '#3b82f6',
              borderColor: '#3b82f6'
            },
            textStyle: {
              color: chartTheme.mutedText,
              fontSize: 10
            }
          }
        ] : [],
        animationDuration: 300,
        animationEasing: 'cubicInOut'
      };

      // 系列以稳定 id 匹配：合并更新时增删渠道不会残留旧系列
      option.series.forEach((item, index) => {
        item.id = `${index}:${item.name}`;
      });

      if (options.merge === true) {
        // 沿用用户当前的缩放区间
        const prevZoom = window.chartInstance.getOption()?.dataZoom || [];
        option.dataZoom.forEach((zoom, index) => {
          if (!prevZoom[index]) return;
          zoom.start = prevZoom[index].start;
          zoom.end = prevZoom[index].end;
        });
        window.chartInstance.setOption(option, { replaceMerge: ['series', 'dataZoom'] });
        return;
      }
      window.chartInstance.setOption(option, true); // 显式加载/切换类型：全量更新
    }

    // 平滑曲线只用于耗时这类连续均值；计数/Token/成本的样条插值会过冲出不存在的峰谷
    function applyTrendChartType(series, chartType, trendType) {
      if (chartType !== 'bar') {
        if (isLatencyTrendType(trendType)) return series;
        return series.map(item => ({ ...item, smooth: false }));
      }

      return series.map(item => {
        const isDashedLine = item.lineStyle && item.lineStyle.type === 'dashed';
        const next = {
          ...item,
          type: 'bar',
          barMaxWidth: 18,
          itemStyle: {
            ...(item.itemStyle || {}),
            opacity: isDashedLine ? 0.48 : 0.78,
            borderRadius: [3, 3, 0, 0]
          }
        };

        delete next.smooth;
        delete next.symbol;
        delete next.symbolSize;
        delete next.showSymbol;
        delete next.sampling;
        delete next.connectNulls;
        delete next.lineStyle;
        delete next.areaStyle;
        return next;
      });
    }

    // 所选指标整段无样本时在图上说明原因，避免空坐标系被误认为加载失败
    function buildTrendEmptyGraphic(series, trendType, chartTheme) {
      const hasSample = series.some(item => (item.data || []).some(value => {
        const num = Number(value && typeof value === 'object' ? value.value : value);
        return Number.isFinite(num) && num !== 0;
      }));
      // 固定 id + invisible 切换：合并更新时也能正确隐藏/显示提示
      return [{
        id: 'trend-empty-hint',
        type: 'text',
        invisible: hasSample,
        left: 'center',
        top: 'middle',
        silent: true,
        style: {
          text: t(trendType === 'first_byte' ? 'trend.emptyFirstByte' : 'trend.emptyMetric'),
          fill: chartTheme.mutedText,
          fontSize: 13
        }
      }];
    }

    function isLatencyTrendType(trendType) {
      return trendType === 'first_byte' || trendType === 'duration';
    }

    function setTrendChartType(chartType) {
      if (chartType !== 'line' && chartType !== 'bar') return;
      if (window.currentTrendChartType === chartType) return;

      window.currentTrendChartType = chartType;
      updateTrendChartTypeButtons();
      renderChart();
    }

    function updateTrendChartTypeButtons() {
      document.querySelectorAll('.trend-chart-type-btn').forEach(button => {
        const active = button.dataset.chartType === window.currentTrendChartType;
        button.classList.toggle('active', active);
        button.setAttribute('aria-pressed', active ? 'true' : 'false');
      });
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
      const chartTheme = getTrendChartTheme();

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
          color: chartTheme.strongText,
          fontSize: 11,
          position: 'insideEndTop',
          padding: [2, 6],
          borderRadius: 4,
          backgroundColor: chartTheme.surface,
          borderColor: chartTheme.axisLine,
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
          color: chartTheme.strongText,
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
      return min >= 60 ? (min/60) + t('trend.hour') : min + t('trend.minute');
    }

    // ===== 渠道数据缓存（避免重复遍历 trendData）=====
    // 缓存结构: { channelName: { success, error, hasData } }
    window._channelDataCache = null;
    window._channelDataCacheVersion = 0;

    // 构建渠道数据缓存：一次遍历 trendData，统计所有渠道
    function buildChannelDataCache(trendData) {
      const cache = {};
      if (!trendData || !trendData.length) {
        window._channelDataCache = cache;
        window._channelDataCacheVersion++;
        return cache;
      }

      // 单次遍历：收集所有渠道的统计数据
      for (let i = 0, len = trendData.length; i < len; i++) {
        const channels = trendData[i].channels;
        if (!channels) continue;

        const names = Object.keys(channels);
        for (let j = 0, nLen = names.length; j < nLen; j++) {
          const name = names[j];
          const chData = channels[name];
          if (!cache[name]) {
            cache[name] = { success: 0, error: 0 };
          }
          cache[name].success += chData.success || 0;
          cache[name].error += chData.error || 0;
        }
      }

      // 计算 hasData 标记
      const cacheNames = Object.keys(cache);
      for (let i = 0, len = cacheNames.length; i < len; i++) {
        const name = cacheNames[i];
        cache[name].hasData = (cache[name].success + cache[name].error) > 0;
      }

      window._channelDataCache = cache;
      window._channelDataCacheVersion++;
      return cache;
    }

    // 检查渠道是否有数据（使用缓存）
    function hasChannelData(channelName, trendData) {
      // 如果缓存不存在或为空，先构建缓存
      if (!window._channelDataCache) {
        buildChannelDataCache(trendData);
      }

      const cached = window._channelDataCache[channelName];
      return cached ? cached.hasData : false;
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
      const channelArray = Array.from(channels);
      const colorsLen = colors.length;

      for (let i = 0, len = channelArray.length; i < len; i++) {
        channelColors[channelArray[i]] = colors[i % colorsLen];
      }

      return channelColors;
    }
    
    // 更新渠道筛选器 - 显示所有有数据的渠道（包括未配置的渠道）
    // 优化：直接使用缓存获取有数据的渠道，避免重复遍历 trendData
    function updateChannelFilter() {
      const filterList = document.getElementById('channel-filter-list');
      if (!filterList) return;

      // 直接从缓存获取所有有数据的渠道名称
      const allChannelNames = new Set();

      // 使用缓存：O(1) 查找
      if (window._channelDataCache) {
        const cachedNames = Object.keys(window._channelDataCache);
        for (let i = 0, len = cachedNames.length; i < len; i++) {
          const name = cachedNames[i];
          if (window._channelDataCache[name].hasData) {
            allChannelNames.add(name);
          }
        }
      }

      // 生成颜色映射
      const channelColors = generateChannelColors(allChannelNames);

      // 使用 DocumentFragment 批量插入 DOM
      const fragment = document.createDocumentFragment();
      const sortedNames = Array.from(allChannelNames).sort();

      for (let i = 0, len = sortedNames.length; i < len; i++) {
        const channelName = sortedNames[i];
        const isVisible = window.visibleChannels.has(channelName);

        // Add special marker for "Unknown Channel"
        const unknownChannelName = t('trend.unknownChannel');
        const displayName = channelName === 'Unknown Channel' || channelName === unknownChannelName
          ? `${unknownChannelName} ⚠️`
          : channelName;

        const item = TemplateEngine.render('tpl-channel-filter-item', {
          checkedClass: isVisible ? 'checked' : '',
          color: channelColors[channelName],
          displayName: displayName
        });
        if (item) {
          item.addEventListener('click', () => {
            toggleChannel(channelName);
          });
          fragment.appendChild(item);
        }
      }

      filterList.innerHTML = '';
      filterList.appendChild(fragment);
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
    // 优化：直接使用缓存获取有数据的渠道
    function selectAllChannels() {
      if (window._channelDataCache) {
        const names = Object.keys(window._channelDataCache);
        for (let i = 0, len = names.length; i < len; i++) {
          const name = names[i];
          if (window._channelDataCache[name].hasData) {
            window.visibleChannels.add(name);
          }
        }
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

    function bindChannelFilterControls() {
      const channelFilterToggle = document.getElementById('btn-channel-filter-toggle');
      if (channelFilterToggle) {
        channelFilterToggle.addEventListener('click', () => {
          toggleChannelFilter();
        });
      }

      const selectAllBtn = document.getElementById('btn-select-all-channels');
      if (selectAllBtn) {
        selectAllBtn.addEventListener('click', () => {
          selectAllChannels();
        });
      }

      const clearAllBtn = document.getElementById('btn-clear-all-channels');
      if (clearAllBtn) {
        clearAllBtn.addEventListener('click', () => {
          clearAllChannels();
        });
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

    function initTrendChannelNameCombobox(initialValue) {
      if (typeof window.createSearchableCombobox !== 'function') return;
      if (!document.getElementById('f_name')) return;
      trendChannelNameCombobox = window.createSearchableCombobox({
        inputId: 'f_name',
        dropdownId: 'f_name_dropdown',
        attachMode: true,
        initialValue: initialValue || '',
        initialLabel: initialValue || t('stats.allChannels'),
        getOptions: () => [
          { value: '', label: t('stats.allChannels') },
          ...(window.channels || []).map(ch => ({ value: ch.name, label: ch.name }))
        ],
        onSelect: () => {
          window.currentChannelName = trendChannelNameCombobox.getValue();
          persistState();
          loadData();
        }
      });
    }

    function initTrendModelCombobox(initialValue) {
      if (typeof window.createSearchableCombobox !== 'function') return;
      if (!document.getElementById('f_model')) return;
      trendModelCombobox = window.createSearchableCombobox({
        inputId: 'f_model',
        dropdownId: 'f_model_dropdown',
        attachMode: true,
        allowCustomInput: true,
        commitEmptyAsFirst: true,
        initialValue: initialValue || '',
        initialLabel: initialValue || t('trend.allModels'),
        getOptions: () => [
          { value: '', label: t('trend.allModels') },
          ...(window.availableModels || []).map(model => ({ value: model, label: model }))
        ],
        onSelect: () => {
          // 用户重新选择后不再沿用恢复的精确值，精确/模糊由当前选项列表判定
          trendExactModelValue = '';
          window.currentModel = trendModelCombobox.getValue();
          persistState();
          loadData();
        }
      });
    }

    // 页面初始化
    window.initPageBootstrap({
      topbarKey: 'trend',
      run: async () => {
      restoreState();
      restoreChannelState();
      applyRangeUI();

      bindToggles();
      bindChannelFilterControls();

      // 初始化渠道名/模型 combobox
      initTrendChannelNameCombobox(window.currentChannelName);
      initTrendModelCombobox(window.currentModel);

      // 模型/渠道选项、令牌选项与指标数据互不依赖，并行加载
      const [, authTokens] = await Promise.all([
        loadModelsAndData(),
        window.initAuthTokenFilter({
          selectId: 'f_auth_token',
          value: window.currentAuthToken,
          loadOptions: {
            tokenPrefix: t('trend.tokenPrefix'),
            restoreValue: window.currentAuthToken
          }
        })
      ]);
      window.authTokens = authTokens;

      // 修复：全局注册resize监听器（仅一次，避免内存泄漏）
      window.addEventListener('resize', () => {
        if (window.chartInstance) {
          window.chartInstance.resize();
        }
      });

      window.addEventListener('ccload:themechange', () => {
        if (window.chartInstance && window.trendData && window.trendData.length) {
          renderChart();
        }
      });

      // 自动刷新（system_settings.auto_refresh_interval_seconds，0=禁用；页面隐藏时暂停）
      if (typeof window.createAutoRefresh === 'function') {
        window.createAutoRefresh({ load: () => loadData({ background: true }) }).init();
      }
      }
    });

    function bindToggles() {
      const trendChartTypeGroup = document.getElementById('trend-chart-type-group');
      updateTrendChartTypeButtons();
      trendChartTypeGroup?.addEventListener('click', (event) => {
        const button = event.target.closest('.trend-chart-type-btn');
        if (!button || !trendChartTypeGroup.contains(button)) return;
        setTrendChartType(button.dataset.chartType);
      });

      // 趋势类型切换
      const trendTypeGroup = document.getElementById('trend-type-group');
      trendTypeGroup.addEventListener('click', (e) => {
        const button = e.target.closest('.toggle-btn');
        if (!button) return;
        window.currentTrendType = button.getAttribute('data-type') || DEFAULT_TREND_TYPE;
        updateTrendTypeButtons();
        persistState();
        renderChart();
      });

      const clientProtocolSelect = document.getElementById('f_client_protocol');
      if (clientProtocolSelect) {
        clientProtocolSelect.addEventListener('change', (e) => {
          window.currentClientProtocol = e.target.value || '';
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

      document.getElementById('btn_trend_retry')?.addEventListener('click', () => {
        loadData();
      });

      // 渠道ID和渠道名已改为 combobox，onSelect 回调自动触发 persistState + loadData
      document.getElementById('btn_clear_filters')?.addEventListener('click', resetTrendFilters);
    }

    async function resetTrendFilters() {
      window.currentModel = '';
      trendExactModelValue = '';
      window.currentClientProtocol = '';
      window.currentAuthToken = '';
      window.currentChannelName = '';
      window.applyFilterControlValues({ range: 'today' }, {
        range: 'f_hours',
        clientProtocol: 'f_client_protocol',
        authToken: 'f_auth_token'
      });
      trendChannelNameCombobox?.setValue('', t('stats.allChannels'));
      trendModelCombobox?.setValue('', t('trend.allModels'));
      await handleTrendRangeChange('today');
    }

    async function handleTrendRangeChange(nextRange, customRange) {
      const range = nextRange || 'today';
      window.currentRange = range;
      if (range === 'custom') {
        currentTrendCustomTimeRange = normalizeTrendCustomTimeRange(customRange);
      } else {
        currentTrendCustomTimeRange = null;
      }
      renderTrendInfo();
      persistState();
      await loadModelsAndData(range);
    }

    function persistState() {
      try {
        window.persistFilterState({
          key: TREND_FILTER_KEY,
          values: getTrendFilters(),
          pathname: location.pathname,
          fields: TREND_FILTER_FIELDS,
          historyMethod: 'replaceState'
        });
      } catch (_) {}
    }

    function restoreState() {
      try {
        const savedFilters = loadSavedTrendFilters();
        const restoredFilters = window.FilterState.restore({
          search: location.search,
          savedFilters,
          fields: TREND_FILTER_FIELDS
        });

        // 恢复时间范围 (默认"本日")
        const validRanges = window.getDateRangePresets
          ? window.getDateRangePresets({ includeCustom: true }).map((range) => range.value)
          : ['today'];
        window.currentRange = validRanges.includes(restoredFilters.range) ? restoredFilters.range : 'today';
        currentTrendCustomTimeRange = window.currentRange === 'custom'
          ? normalizeTrendCustomTimeRange(restoredFilters)
          : null;
        if (window.currentRange === 'custom' && !currentTrendCustomTimeRange) {
          window.currentRange = 'today';
        }

        renderTrendInfo();

        // 恢复趋势类型
        window.currentTrendType = DEFAULT_TREND_TYPE;
        if (['count', 'rpm', 'first_byte', 'duration', 'tokens', 'cost'].includes(restoredFilters.trendType)) {
          window.currentTrendType = restoredFilters.trendType;
        }

        // 恢复模型筛选：URL 带 model= 或本地存储标记精确时按精确匹配，否则 model_like 模糊匹配
        window.currentModel = restoredFilters.model || '';
        const urlParams = new URLSearchParams(location.search);
        const modelExact = urlParams.toString()
          ? urlParams.has('model')
          : savedFilters?.modelExact === true;
        trendExactModelValue = modelExact ? window.currentModel : '';

        // 恢复客户端入口协议
        window.currentClientProtocol = restoredFilters.clientProtocol || '';
        const clientProtocolSelect = document.getElementById('f_client_protocol');
        if (clientProtocolSelect) {
          clientProtocolSelect.value = window.currentClientProtocol;
        }

        // 恢复令牌选择
        window.currentAuthToken = restoredFilters.authToken || '';

        // 恢复渠道名（combobox 初始化时通过 initialValue 恢复）
        window.currentChannelName = restoredFilters.channelName || '';
      } catch (_) {}
    }

    function applyRangeUI() {
      window.initSavedDateRangeFilter({
        selectId: 'f_hours',
        defaultValue: 'today',
        restoredValue: window.currentRange,
        includeCustom: true,
        customRange: currentTrendCustomTimeRange,
        customPickerContainerId: 'f_hours_custom_range_host',
        onChange: handleTrendRangeChange
      });

      updateTrendTypeButtons();
    }

    function updateTrendTypeButtons() {
      document.querySelectorAll('#trend-type-group .toggle-btn').forEach(btn => {
        const active = (btn.getAttribute('data-type') || DEFAULT_TREND_TYPE) === window.currentTrendType;
        btn.classList.toggle('active', active);
        btn.setAttribute('aria-pressed', active ? 'true' : 'false');
      });
    }

    window.i18n?.onLocaleChange?.(() => {
      renderTrendInfo();
      if (window.chartInstance && document.getElementById('chart').style.display !== 'none') renderChart();
    });

    // 注销功能（已由 ui.js 的 onLogout 统一处理）
