const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const trendSource = fs.readFileSync(path.join(__dirname, 'trend.js'), 'utf8');
const channelsStateSource = fs.readFileSync(path.join(__dirname, 'channels-state.js'), 'utf8');
const channelsHtmlSource = fs.readFileSync(path.join(__dirname, '..', '..', 'channels.html'), 'utf8');

function loadDateRangeModule() {
  const source = fs.readFileSync(path.join(__dirname, 'date-range-selector.js'), 'utf8');
  const containers = new Map();
  const sandbox = {
    console,
    document: {
      getElementById(id) {
        if (!containers.has(id)) {
          containers.set(id, {
            innerHTML: '',
            value: '',
            children: [],
            appendChild(node) {
              this.children.push(node);
            },
            addEventListener() {}
          });
        }
        return containers.get(id);
      },
      createElement(tagName) {
        return {
          tagName,
          value: '',
          textContent: ''
        };
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

test('时间范围模块可以从共享预设渲染按钮模式', () => {
  const { window, containers } = loadDateRangeModule();

  assert.equal(typeof window.renderDateRangeButtons, 'function');
  window.renderDateRangeButtons('range-buttons', {
    values: ['today', 'last_month', 'all'],
    activeValue: 'last_month'
  });

  const html = containers.get('range-buttons').innerHTML;
  assert.match(html, /data-range="today"/);
  assert.match(html, /data-range="last_month"/);
  assert.match(html, /data-range="all"/);
  assert.match(html, /time-range-btn active" data-range="last_month"/);
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
