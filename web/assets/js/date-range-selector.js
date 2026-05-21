/**
 * 时间范围选择器 - 共享组件
 * 用于 logs/stats/trend 页面的统一时间范围选择
 *
 * 使用方式:
 * 1. 在HTML中引入: <script src="/web/assets/js/date-range-selector.js"></script>
 * 2. 调用 initDateRangeSelector(elementId, defaultRange, onChangeCallback)
 *
 * 后端API参数: range=today|yesterday|day_before_yesterday|this_week|last_week|this_month|last_month|custom
 * 自定义区间额外参数: start_time=<Unix毫秒>&end_time=<Unix毫秒>
 */

(function(window) {
  'use strict';

  const t = window.t;

  function text(key, fallback, params) {
    if (typeof window.i18nText === 'function') return window.i18nText(key, fallback, params);
    const translated = typeof t === 'function' ? t(key, params) : key;
    return translated === key ? fallback : translated;
  }

  // 时间范围预设 (key → i18n key)
  // key与后端GetTimeRange()支持的range参数一致
  const BASE_DATE_RANGE_PRESETS = [
    { value: 'today', i18nKey: 'index.timeRange.today', fallback: 'Today' },
    { value: 'yesterday', i18nKey: 'index.timeRange.yesterday', fallback: 'Yesterday' },
    { value: 'day_before_yesterday', i18nKey: 'index.timeRange.dayBeforeYesterday', fallback: 'Day Before' },
    { value: 'this_week', i18nKey: 'index.timeRange.thisWeek', fallback: 'This Week' },
    { value: 'last_week', i18nKey: 'index.timeRange.lastWeek', fallback: 'Last Week' },
    { value: 'this_month', i18nKey: 'index.timeRange.thisMonth', fallback: 'This Month' },
    { value: 'last_month', i18nKey: 'index.timeRange.lastMonth', fallback: 'Last Month' }
  ];

  const CUSTOM_DATE_RANGE_PRESET = { value: 'custom', i18nKey: 'index.timeRange.custom', fallback: 'Custom' };
  const ALL_DATE_RANGE_PRESET = { value: 'all', i18nKey: 'common.all', fallback: 'All' };
  const CUSTOM_RESELECT_SENTINEL = '__custom_reselect__';

  function buildPresetMap(options = {}) {
    const presets = [...BASE_DATE_RANGE_PRESETS];
    if (options.includeCustom === true) presets.push(CUSTOM_DATE_RANGE_PRESET);
    if (options.includeAll === true) presets.push(ALL_DATE_RANGE_PRESET);
    return new Map(presets.map((preset) => [preset.value, preset]));
  }

  function getDateRangePresets(options = {}) {
    const includeAll = options.includeAll === true;
    const includeCustom = options.includeCustom === true;
    const values = Array.isArray(options.values) && options.values.length > 0
      ? options.values
      : [
          ...BASE_DATE_RANGE_PRESETS.map((preset) => preset.value),
          ...(includeCustom ? [CUSTOM_DATE_RANGE_PRESET.value] : []),
          ...(includeAll ? [ALL_DATE_RANGE_PRESET.value] : [])
        ];
    const presetMap = buildPresetMap({
      includeAll: includeAll || values.includes(ALL_DATE_RANGE_PRESET.value),
      includeCustom: includeCustom || values.includes(CUSTOM_DATE_RANGE_PRESET.value)
    });

    return values
      .map((value) => presetMap.get(value))
      .filter(Boolean)
      .map((preset) => ({ ...preset }));
  }

  function renderDateRangeButtons(containerId, options = {}) {
    const container = document.getElementById(containerId);
    if (!container) {
      console.error(`Date range button render failed: element #${containerId} not found`);
      return;
    }

    const presets = getDateRangePresets({
      includeAll: options.includeAll === true,
      includeCustom: options.includeCustom === true,
      values: options.values
    });
    const activeValue = options.activeValue || 'today';

    container.innerHTML = presets.map((preset) => `
      <button type="button" class="time-range-btn${preset.value === activeValue ? ' active' : ''}" data-range="${preset.value}">${text(preset.i18nKey, preset.fallback)}</button>
    `).join('');
  }

  function normalizeCustomRange(customRange, nowMs = Date.now()) {
    if (!customRange || typeof customRange !== 'object') return null;

    const startMs = Number(customRange.startMs);
    const endMs = Number(customRange.endMs);
    const maxMs = Number.isFinite(Number(nowMs)) ? Number(nowMs) : Date.now();
    if (!Number.isFinite(startMs) || !Number.isFinite(endMs) || startMs >= maxMs) {
      return null;
    }
    const safeEndMs = Math.min(endMs, maxMs);
    if (safeEndMs <= startMs) return null;

    return {
      startMs: Math.trunc(startMs),
      endMs: Math.trunc(safeEndMs)
    };
  }

  function buildDateRangeQuery(rangeKey, customRange, nowMs) {
    if (rangeKey === 'custom') {
      const normalized = normalizeCustomRange(customRange, nowMs);
      if (!normalized) return 'range=today';
      return `range=custom&start_time=${normalized.startMs}&end_time=${normalized.endMs}`;
    }
    return `range=${encodeURIComponent(rangeKey || 'today')}`;
  }

  function pad2(value) {
    return String(value).padStart(2, '0');
  }

  function startOfLocalDay(date) {
    return new Date(date.getFullYear(), date.getMonth(), date.getDate(), 0, 0, 0, 0);
  }

  function endOfLocalDay(date) {
    return new Date(date.getFullYear(), date.getMonth(), date.getDate(), 23, 59, 59, 0);
  }

  function monthStart(date) {
    return new Date(date.getFullYear(), date.getMonth(), 1);
  }

  function addMonths(date, count) {
    return new Date(date.getFullYear(), date.getMonth() + count, 1);
  }

  function formatDayKey(date) {
    return `${date.getFullYear()}-${pad2(date.getMonth() + 1)}-${pad2(date.getDate())}`;
  }

  function dateFromDayKey(dayKey) {
    const [year, month, day] = dayKey.split('-').map(Number);
    return new Date(year, month - 1, day);
  }

  function formatTimeValue(date) {
    return `${pad2(date.getHours())}:${pad2(date.getMinutes())}:${pad2(date.getSeconds())}`;
  }

  function applyTimeValue(date, value, fallback) {
    const parts = String(value || '').split(':').map(Number);
    const source = Number.isFinite(parts[0]) ? parts : fallback.split(':').map(Number);
    const next = new Date(date.getTime());
    next.setHours(source[0] || 0, source[1] || 0, source[2] || 0, 0);
    return next;
  }

  function formatDisplayDateTime(date) {
    return `${date.getFullYear()}/${pad2(date.getMonth() + 1)}/${pad2(date.getDate())} ${formatTimeValue(date)}`;
  }

  function formatMonthTitle(date) {
    const locale = window.i18n && typeof window.i18n.getLocale === 'function'
      ? window.i18n.getLocale()
      : 'zh-CN';
    if (locale === 'en') {
      return date.toLocaleDateString('en-US', { year: 'numeric', month: 'long' });
    }
    return `${date.getFullYear()}年 ${date.getMonth() + 1}月`;
  }

  function getWeekdayLabels() {
    const locale = window.i18n && typeof window.i18n.getLocale === 'function'
      ? window.i18n.getLocale()
      : 'zh-CN';
    return locale === 'en'
      ? ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun']
      : ['一', '二', '三', '四', '五', '六', '日'];
  }

  function getInitialCustomRange(existingRange) {
    const normalized = normalizeCustomRange(existingRange);
    if (normalized) {
      return {
        start: new Date(normalized.startMs),
        end: new Date(normalized.endMs)
      };
    }

    const today = new Date();
    return {
      start: startOfLocalDay(today),
      end: endOfLocalDay(today)
    };
  }

  function buildCalendarDays(monthDate) {
    const first = monthStart(monthDate);
    const mondayOffset = (first.getDay() + 6) % 7;
    const cursor = new Date(first.getFullYear(), first.getMonth(), first.getDate() - mondayOffset);
    const days = [];

    for (let i = 0; i < 42; i += 1) {
      days.push(new Date(cursor.getFullYear(), cursor.getMonth(), cursor.getDate() + i));
    }
    return days;
  }

  function renderCustomRangePickerBody(picker, state) {
    const summary = picker.querySelector('[data-role="custom-range-summary"]');
    const startTimeInput = picker.querySelector('[data-role="start-time"]');
    const endTimeInput = picker.querySelector('[data-role="end-time"]');
    const calendars = picker.querySelector('[data-role="calendars"]');

    summary.textContent = `${formatDisplayDateTime(state.start)} ~ ${formatDisplayDateTime(state.end)}`;
    startTimeInput.value = formatTimeValue(state.start);
    endTimeInput.value = formatTimeValue(state.end);

    calendars.innerHTML = '';
    [0, 1].forEach((offset) => {
      const monthDate = addMonths(state.visibleMonth, offset);
      const panel = document.createElement('div');
      panel.className = 'custom-range-calendar';

      const title = document.createElement('div');
      title.className = 'custom-range-calendar-title';
      title.textContent = formatMonthTitle(monthDate);
      panel.appendChild(title);

      const weekdays = document.createElement('div');
      weekdays.className = 'custom-range-weekdays';
      getWeekdayLabels().forEach((label) => {
        const item = document.createElement('span');
        item.textContent = label;
        weekdays.appendChild(item);
      });
      panel.appendChild(weekdays);

      const grid = document.createElement('div');
      grid.className = 'custom-range-days';
      buildCalendarDays(monthDate).forEach((day) => {
        const dayStart = startOfLocalDay(day);
        const dayEnd = endOfLocalDay(day);
        const btn = document.createElement('button');
        btn.type = 'button';
        btn.className = 'custom-range-day';
        btn.dataset.date = formatDayKey(day);
        btn.textContent = String(day.getDate());

        if (day.getMonth() !== monthDate.getMonth()) btn.classList.add('muted');
        if (formatDayKey(day) === formatDayKey(state.start)) btn.classList.add('selected-start');
        if (formatDayKey(day) === formatDayKey(state.end)) btn.classList.add('selected-end');
        if (dayStart > state.start && dayEnd < state.end) btn.classList.add('in-range');
        if (dayStart > state.maxDate) {
          btn.disabled = true;
          btn.classList.add('disabled');
        }

        grid.appendChild(btn);
      });
      panel.appendChild(grid);
      calendars.appendChild(panel);
    });
  }

  function openCustomDateRangePicker(options = {}) {
    const container = options.container || document.getElementById(options.containerId);
    if (!container) return;

    const oldPicker = container.querySelector('.custom-range-picker');
    if (oldPicker) oldPicker.remove();

    const initialRange = getInitialCustomRange(options.range);
    const state = {
      start: initialRange.start,
      end: initialRange.end,
      visibleMonth: monthStart(initialRange.start),
      maxDate: startOfLocalDay(new Date()),
      awaitingEnd: false
    };

    const picker = document.createElement('div');
    picker.className = 'custom-range-picker';
    picker.innerHTML = `
      <div class="custom-range-picker-head">
        <button type="button" class="custom-range-nav-btn" data-action="prev-month" aria-label="${text('index.customRange.prevMonth', 'Previous month')}">‹</button>
        <div class="custom-range-summary" data-role="custom-range-summary"></div>
        <button type="button" class="custom-range-nav-btn" data-action="next-month" aria-label="${text('index.customRange.nextMonth', 'Next month')}">›</button>
      </div>
      <div class="custom-range-calendars" data-role="calendars"></div>
      <div class="custom-range-time-row">
        <label>
          <span>${text('index.customRange.startTime', 'Start time')}</span>
          <input type="time" step="1" data-role="start-time">
        </label>
        <label>
          <span>${text('index.customRange.endTime', 'End time')}</span>
          <input type="time" step="1" data-role="end-time">
        </label>
      </div>
      <div class="custom-range-picker-footer">
        <button type="button" class="custom-range-link-btn" data-action="cancel">${text('common.cancel', 'Cancel')}</button>
        <button type="button" class="custom-range-confirm-btn" data-action="confirm">${text('common.confirm', 'Confirm')}</button>
      </div>
    `;

    container.appendChild(picker);
    renderCustomRangePickerBody(picker, state);

    picker.addEventListener('click', (event) => {
      const actionTarget = event.target.closest('[data-action]');
      if (actionTarget) {
        const action = actionTarget.dataset.action;
        if (action === 'prev-month') {
          state.visibleMonth = addMonths(state.visibleMonth, -1);
          renderCustomRangePickerBody(picker, state);
          return;
        }
        if (action === 'next-month') {
          state.visibleMonth = addMonths(state.visibleMonth, 1);
          renderCustomRangePickerBody(picker, state);
          return;
        }
        if (action === 'cancel') {
          if (typeof options.onCancel === 'function') options.onCancel();
          picker.remove();
          return;
        }
        if (action === 'confirm') {
          const normalized = normalizeCustomRange({
            startMs: state.start.getTime(),
            endMs: state.end.getTime()
          });
          if (normalized && typeof options.onConfirm === 'function') {
            options.onConfirm({
              ...normalized,
              label: `${formatDisplayDateTime(state.start)} ~ ${formatDisplayDateTime(state.end)}`
            });
          }
          picker.remove();
          return;
        }
      }

      const dayTarget = event.target.closest('.custom-range-day');
      if (!dayTarget) return;
      if (dayTarget.disabled) return;

      const picked = dateFromDayKey(dayTarget.dataset.date);
      if (!state.awaitingEnd) {
        state.start = startOfLocalDay(picked);
        state.end = endOfLocalDay(picked);
        state.awaitingEnd = true;
      } else {
        const pickedEnd = endOfLocalDay(picked);
        if (pickedEnd < state.start) {
          state.end = endOfLocalDay(state.start);
          state.start = startOfLocalDay(picked);
        } else {
          state.end = pickedEnd;
        }
        state.awaitingEnd = false;
      }
      renderCustomRangePickerBody(picker, state);
    });

    picker.querySelector('[data-role="start-time"]').addEventListener('change', (event) => {
      state.start = applyTimeValue(state.start, event.target.value, '00:00:00');
      if (state.end <= state.start) state.end = new Date(state.start.getTime() + 1000);
      renderCustomRangePickerBody(picker, state);
    });

    picker.querySelector('[data-role="end-time"]').addEventListener('change', (event) => {
      state.end = applyTimeValue(state.end, event.target.value, '23:59:59');
      if (state.end <= state.start) state.start = new Date(state.end.getTime() - 1000);
      renderCustomRangePickerBody(picker, state);
    });
  }


  /**
   * 初始化时间范围选择器
   * @param {string} elementId - select元素的ID
   * @param {string} defaultRange - 默认选中的范围key (如'today')
   * @param {function} onChangeCallback - 值变化时的回调函数，接收range key参数
   */
  window.initDateRangeSelector = function(elementId, defaultRange, onChangeCallback, options = {}) {
    const selectEl = document.getElementById(elementId);
    if (!selectEl) {
      console.error(`Date range selector init failed: element #${elementId} not found`);
      return;
    }

    function getSelectPresets() {
      return getDateRangePresets({
        includeAll: options.includeAll === true,
        includeCustom: options.includeCustom === true,
        values: options.values
      });
    }

    function resolvePickerContainer() {
      if (options.customPickerContainerId) {
        return document.getElementById(options.customPickerContainerId);
      }
      return selectEl.parentElement;
    }

    let selectedCustomRange = normalizeCustomRange(options.customRange);
    let selectedValue = typeof options.restoredValue === 'string' && options.restoredValue
      ? options.restoredValue
      : defaultRange;
    let reselectingCustom = false;

    // 渲染选项
    function renderOptions() {
      const currentValue = selectEl.value;
      selectEl.innerHTML = '';
      getSelectPresets().forEach(range => {
        const option = document.createElement('option');
        option.value = range.value;
        option.textContent = t(range.i18nKey, range.fallback);
        selectEl.appendChild(option);
      });
      if (options.includeCustom === true) {
        const sentinel = document.createElement('option');
        sentinel.value = CUSTOM_RESELECT_SENTINEL;
        sentinel.textContent = text(CUSTOM_DATE_RANGE_PRESET.i18nKey, CUSTOM_DATE_RANGE_PRESET.fallback);
        sentinel.hidden = true;
        selectEl.appendChild(sentinel);
      }
      // 恢复之前的选择
      if (currentValue && getSelectPresets().some(r => r.value === currentValue)) {
        selectEl.value = currentValue;
      }
    }

    // 初次渲染
    renderOptions();

    // 监听语言切换事件
    window.i18n.onLocaleChange(renderOptions);

    // 设置默认值
    const validDefault = getSelectPresets().some(r => r.value === defaultRange) ? defaultRange : 'today';
    selectedValue = getSelectPresets().some(r => r.value === selectedValue) ? selectedValue : validDefault;
    selectEl.value = selectedValue;

    // 绑定change事件
    if (typeof onChangeCallback === 'function') {
      function canOpenCustomPicker() {
        return options.includeCustom === true && typeof window.openCustomDateRangePicker === 'function';
      }

      function openCustomPicker(previousValue) {
        window.openCustomDateRangePicker({
          container: resolvePickerContainer(),
          range: selectedCustomRange,
          onCancel: () => {
            selectEl.value = previousValue;
          },
          onConfirm: (confirmedRange) => {
            selectedValue = 'custom';
            selectedCustomRange = confirmedRange;
            selectEl.value = 'custom';
            if (confirmedRange.label) selectEl.title = confirmedRange.label;
            onChangeCallback('custom', confirmedRange);
          }
        });
      }

      selectEl.addEventListener('pointerdown', function() {
        if (this.value === 'custom' && selectedValue === 'custom' && options.includeCustom === true) {
          reselectingCustom = true;
          this.value = CUSTOM_RESELECT_SENTINEL;
        }
      });

      selectEl.addEventListener('blur', function() {
        if (reselectingCustom) {
          reselectingCustom = false;
          this.value = 'custom';
        }
      });

      selectEl.addEventListener('change', function() {
        const nextValue = this.value;
        const wasReselectingCustom = reselectingCustom;
        reselectingCustom = false;

        if (nextValue === CUSTOM_RESELECT_SENTINEL) {
          this.value = selectedValue || validDefault;
          return;
        }

        if (nextValue === 'custom' && canOpenCustomPicker()) {
          const previousValue = selectedValue || validDefault;
          openCustomPicker(wasReselectingCustom ? 'custom' : previousValue);
          return;
        }

        selectedValue = nextValue;
        if (nextValue !== 'custom') {
          this.title = '';
        }
        onChangeCallback(nextValue);
      });
    }
  };

  /**
   * 获取范围的显示标签
   * @param {string} rangeKey - 范围key
   * @returns {string} 显示标签
   */
  window.getRangeLabel = function(rangeKey) {
    const range = getDateRangePresets({ includeAll: true, includeCustom: true }).find(r => r.value === rangeKey);
    return range ? text(range.i18nKey, range.fallback) : text('index.timeRange.today', 'Today');
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
      'last_month': 720,
      'custom': 24,
      'all': 24
    };
    return hoursMap[rangeKey] || 24;
  };

  window.getDateRangePresets = getDateRangePresets;
  window.renderDateRangeButtons = renderDateRangeButtons;
  window.buildDateRangeQuery = buildDateRangeQuery;
  window.openCustomDateRangePicker = openCustomDateRangePicker;

})(window);
