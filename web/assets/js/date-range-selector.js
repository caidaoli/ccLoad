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

  // 时间范围预设 (key → 显示标签)
  // key与后端GetTimeRange()支持的range参数一致
  const DATE_RANGES = {
    'today': { label: '本日' },
    'yesterday': { label: '昨日' },
    'day_before_yesterday': { label: '前日' },
    'this_week': { label: '本周' },
    'last_week': { label: '上周' },
    'this_month': { label: '本月' },
    'last_month': { label: '上月' }
  };

  /**
   * 初始化时间范围选择器
   * @param {string} elementId - select元素的ID
   * @param {string} defaultRange - 默认选中的范围key (如'today')
   * @param {function} onChangeCallback - 值变化时的回调函数，接收range key参数
   */
  window.initDateRangeSelector = function(elementId, defaultRange, onChangeCallback) {
    const selectEl = document.getElementById(elementId);
    if (!selectEl) {
      console.error(`时间范围选择器初始化失败: 未找到元素 #${elementId}`);
      return;
    }

    // 清空并重新生成选项
    selectEl.innerHTML = '';
    Object.keys(DATE_RANGES).forEach(key => {
      const range = DATE_RANGES[key];
      const option = document.createElement('option');
      option.value = key; // 使用range key作为value
      option.textContent = range.label;
      selectEl.appendChild(option);
    });

    // 设置默认值
    const validDefault = DATE_RANGES[defaultRange] ? defaultRange : 'today';
    selectEl.value = validDefault;

    // 绑定change事件
    if (typeof onChangeCallback === 'function') {
      selectEl.addEventListener('change', function() {
        onChangeCallback(this.value);
      });
    }
  };

  /**
   * 从URL参数获取时间范围
   * @param {URLSearchParams} urlParams - URL参数对象
   * @param {string} defaultRange - 默认范围key
   * @returns {string} 范围key
   */
  window.getRangeFromUrlOrDefault = function(urlParams, defaultRange) {
    const rangeParam = urlParams.get('range');
    if (rangeParam && DATE_RANGES[rangeParam]) {
      return rangeParam;
    }
    return DATE_RANGES[defaultRange] ? defaultRange : 'today';
  };

  /**
   * 获取所有可用的时间范围配置
   * @returns {Object} DATE_RANGES对象
   */
  window.getDateRanges = function() {
    return DATE_RANGES;
  };

  /**
   * 获取范围的显示标签
   * @param {string} rangeKey - 范围key
   * @returns {string} 显示标签
   */
  window.getRangeLabel = function(rangeKey) {
    return DATE_RANGES[rangeKey]?.label || '本日';
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
