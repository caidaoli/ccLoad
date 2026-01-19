/**
 * 时间范围选择器 - 共享组件
 * 用于 logs/stats/trend 页面的统一时间范围选择
 *
 * 使用方式:
 * 1. 在HTML中引入: <script src="/web/assets/js/date-range-selector.js"></script>
 * 2. 调用 initDateRangeSelector(elementId, defaultRange, onChangeCallback)
 *
 * 后端API参数: range=today|yesterday|day_before_yesterday|this_week|last_week|this_month|last_month
 */

(function(window) {
  'use strict';

  const t = window.t;

  // 时间范围预设 (key → i18n key)
  // key与后端GetTimeRange()支持的range参数一致
  const DATE_RANGE_KEYS = [
    { value: 'today', i18nKey: 'index.timeRange.today', fallback: 'Today' },
    { value: 'yesterday', i18nKey: 'index.timeRange.yesterday', fallback: 'Yesterday' },
    { value: 'day_before_yesterday', i18nKey: 'index.timeRange.dayBeforeYesterday', fallback: 'Day Before' },
    { value: 'this_week', i18nKey: 'index.timeRange.thisWeek', fallback: 'This Week' },
    { value: 'last_week', i18nKey: 'index.timeRange.lastWeek', fallback: 'Last Week' },
    { value: 'this_month', i18nKey: 'index.timeRange.thisMonth', fallback: 'This Month' },
    { value: 'last_month', i18nKey: 'index.timeRange.lastMonth', fallback: 'Last Month' }
  ];


  /**
   * 初始化时间范围选择器
   * @param {string} elementId - select元素的ID
   * @param {string} defaultRange - 默认选中的范围key (如'today')
   * @param {function} onChangeCallback - 值变化时的回调函数，接收range key参数
   */
  window.initDateRangeSelector = function(elementId, defaultRange, onChangeCallback) {
    const selectEl = document.getElementById(elementId);
    if (!selectEl) {
      console.error(`Date range selector init failed: element #${elementId} not found`);
      return;
    }

    // 渲染选项
    function renderOptions() {
      const currentValue = selectEl.value;
      selectEl.innerHTML = '';
      DATE_RANGE_KEYS.forEach(range => {
        const option = document.createElement('option');
        option.value = range.value;
        option.textContent = t(range.i18nKey, range.fallback);
        selectEl.appendChild(option);
      });
      // 恢复之前的选择
      if (currentValue && DATE_RANGE_KEYS.some(r => r.value === currentValue)) {
        selectEl.value = currentValue;
      }
    }

    // 初次渲染
    renderOptions();

    // 监听语言切换事件
    window.i18n.onLocaleChange(renderOptions);

    // 设置默认值
    const validDefault = DATE_RANGE_KEYS.some(r => r.value === defaultRange) ? defaultRange : 'today';
    selectEl.value = validDefault;

    // 绑定change事件
    if (typeof onChangeCallback === 'function') {
      selectEl.addEventListener('change', function() {
        onChangeCallback(this.value);
      });
    }
  };

  /**
   * 获取范围的显示标签
   * @param {string} rangeKey - 范围key
   * @returns {string} 显示标签
   */
  window.getRangeLabel = function(rangeKey) {
    const range = DATE_RANGE_KEYS.find(r => r.value === rangeKey);
    return range ? t(range.i18nKey, range.fallback) : t('index.timeRange.today', 'Today');
  };

  /**
   * 获取范围对应的大致小时数（用于metrics API的分桶计算）
   * @param {string} rangeKey - 范围key
   * @returns {number} 小时数
   */
  window.getRangeHours = function(rangeKey) {
    const hoursMap = {
      'today': 24,
      'yesterday': 24,
      'day_before_yesterday': 24,
      'this_week': 168,
      'last_week': 168,
      'this_month': 720,
      'last_month': 720
    };
    return hoursMap[rangeKey] || 24;
  };

})(window);
