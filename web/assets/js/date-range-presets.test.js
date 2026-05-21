const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const dateRangeSource = fs.readFileSync(path.join(__dirname, 'date-range-selector.js'), 'utf8');
const trendSource = fs.readFileSync(path.join(__dirname, 'trend.js'), 'utf8');
const channelsStateSource = fs.readFileSync(path.join(__dirname, 'channels-state.js'), 'utf8');
const channelsHtmlSource = fs.readFileSync(path.join(__dirname, '..', '..', 'channels.html'), 'utf8');

function loadDateRangeModule() {
  const source = fs.readFileSync(path.join(__dirname, 'date-range-selector.js'), 'utf8');
  const containers = new Map();
  function createFakeElement(tagName = 'div') {
    const listeners = new Map();
    return {
      tagName,
      listeners,
      innerHTML: '',
      value: '',
      title: '',
      hidden: false,
      disabled: false,
      children: [],
      parentElement: null,
      appendChild(node) {
        node.parentElement = this;
        this.children.push(node);
      },
      addEventListener(type, handler) {
        listeners.set(type, handler);
      },
      setAttribute(name, value) {
        this[name] = value;
      },
      querySelector() {
        return null;
      }
    };
  }

  const sandbox = {
    console,
    document: {
      getElementById(id) {
        if (!containers.has(id)) {
          containers.set(id, createFakeElement());
        }
        return containers.get(id);
      },
      createElement(tagName) {
        return createFakeElement(tagName);
      }
    },
    window: {
      t(key, fallback) {
        return fallback || key;
      },
      i18n: {
        onLocaleChange() {}
      }
    }
  };

  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);
  return { window: sandbox.window, containers };
}

test('时间范围模块暴露共享预设读取接口', () => {
  const { window } = loadDateRangeModule();

  assert.equal(typeof window.getDateRangePresets, 'function');
  const presets = window.getDateRangePresets();
  assert.ok(Array.isArray(presets));
  assert.deepEqual(
    Array.from(presets, (item) => item.value),
    ['today', 'yesterday', 'day_before_yesterday', 'this_week', 'last_week', 'this_month', 'last_month']
  );
});

test('时间范围模块按需追加自定义预设', () => {
  const { window } = loadDateRangeModule();

  assert.deepEqual(
    JSON.parse(JSON.stringify(window.getDateRangePresets({ includeCustom: true }).map((item) => item.value))),
    ['today', 'yesterday', 'day_before_yesterday', 'this_week', 'last_week', 'this_month', 'last_month', 'custom']
  );
});

test('时间范围模块可以从共享预设渲染按钮模式', () => {
  const { window, containers } = loadDateRangeModule();

  assert.equal(typeof window.renderDateRangeButtons, 'function');
  window.renderDateRangeButtons('range-buttons', {
    values: ['today', 'last_month', 'custom', 'all'],
    activeValue: 'last_month'
  });

  const html = containers.get('range-buttons').innerHTML;
  assert.match(html, /data-range="today"/);
  assert.match(html, /data-range="last_month"/);
  assert.match(html, /data-range="custom"/);
  assert.match(html, /data-range="all"/);
  assert.match(html, /time-range-btn active" data-range="last_month"/);
});

test('时间范围选择器在当前已是自定义时可再次选择自定义打开弹层', () => {
  const { window, containers } = loadDateRangeModule();
  const pickerCalls = [];
  const changes = [];

  window.openCustomDateRangePicker = (opts) => {
    pickerCalls.push(opts);
  };

  window.initDateRangeSelector('f_hours', 'today', (range, customRange) => {
    changes.push({ range, customRange });
  }, {
    includeCustom: true,
    customRange: { startMs: 1779321600000, endMs: 1779407999000 },
    customPickerContainerId: 'f_hours_custom_range_host'
  });

  const select = containers.get('f_hours');
  select.value = 'custom';
  select.listeners.get('change').call(select);
  pickerCalls[0].onConfirm({ startMs: 1779321600000, endMs: 1779407999000, label: 'first' });

  assert.equal(select.value, 'custom');
  assert.equal(pickerCalls.length, 1);
  assert.equal(typeof select.listeners.get('pointerdown'), 'function');

  select.listeners.get('pointerdown').call(select);
  select.value = 'custom';
  select.listeners.get('change').call(select);

  assert.equal(pickerCalls.length, 2);
  pickerCalls[1].onConfirm({ startMs: 1779321600000, endMs: 1779407999000, label: 'second' });
  assert.deepEqual(JSON.parse(JSON.stringify(changes)), [
    { range: 'custom', customRange: { startMs: 1779321600000, endMs: 1779407999000, label: 'first' } },
    { range: 'custom', customRange: { startMs: 1779321600000, endMs: 1779407999000, label: 'second' } }
  ]);
});

test('时间范围模块为自定义区间生成毫秒级查询参数', () => {
  const { window } = loadDateRangeModule();

  assert.equal(typeof window.buildDateRangeQuery, 'function');
  assert.equal(
    window.buildDateRangeQuery('custom', { startMs: 1779321600000, endMs: 1779407999000 }, 1779494400000),
    'range=custom&start_time=1779321600000&end_time=1779407999000'
  );
  assert.equal(window.buildDateRangeQuery('today'), 'range=today');
  assert.equal(
    window.buildDateRangeQuery('custom', { startMs: 1779407999000, endMs: 1779321600000 }),
    'range=today'
  );
});

test('时间范围模块不会为未来自定义区间生成无数据查询', () => {
  const { window } = loadDateRangeModule();
  const nowMs = 1779360000000;

  assert.equal(
    window.buildDateRangeQuery('custom', { startMs: 1781625600000, endMs: 1782057599000 }, nowMs),
    'range=today'
  );
  assert.equal(
    window.buildDateRangeQuery('custom', { startMs: 1779321600000, endMs: 1782057599000 }, nowMs),
    'range=custom&start_time=1779321600000&end_time=1779360000000'
  );
});

test('自定义时间弹层禁用未来日期按钮', () => {
  assert.match(dateRangeSource, /maxDate:\s*startOfLocalDay\(new Date\(\)\)/);
  assert.match(dateRangeSource, /dayStart\s*>\s*state\.maxDate/);
  assert.match(dateRangeSource, /btn\.disabled\s*=\s*true/);
});

test('trend.js 使用共享预设校验时间范围', () => {
  assert.match(trendSource, /getDateRangePresets/);
  assert.doesNotMatch(trendSource, /const validRanges = \[/);
});

test('channels-state.js 使用共享标签 helper 获取统计时间范围文案', () => {
  assert.match(channelsStateSource, /getRangeLabel/);
  assert.doesNotMatch(channelsStateSource, /const keyMap = \{/);
});

test('channels.html 在 channels-state.js 之前加载共享时间范围脚本', () => {
  assert.match(
    channelsHtmlSource,
    /date-range-selector\.js\?v=__VERSION__[\s\S]*channels-state\.js\?v=__VERSION__/
  );
});
