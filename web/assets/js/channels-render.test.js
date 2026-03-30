const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function loadRenderSandbox(overrides = {}) {
  const source = fs.readFileSync(path.join(__dirname, 'channels-render.js'), 'utf8');
  const sandbox = {
    window: {
      t(key, params = {}) {
        if (key === 'channels.table.priority') return '优先级';
        if (key === 'channels.stats.healthScoreLabel') return '健康度';
        if (key === 'channels.stats.successRate') return `成功率 ${params.rate}`;
        if (key === 'channels.stats.firstByte') return '首字';
        if (key === 'channels.stats.calls') return '调用';
        if (key === 'stats.tooltipDuration') return '耗时';
        if (key === 'stats.unitTimes') return '次';
        if (key === 'common.success') return '成功';
        if (key === 'common.failed') return '失败';
        if (key === 'common.seconds') return '秒';
        return key;
      }
    },
    console
  };

  Object.assign(sandbox, overrides);

  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);
  return sandbox;
}

function loadRenderHelpers() {
  return loadRenderSandbox();
}

test('buildEffectivePriorityHtml 不渲染优先级和健康度标签', () => {
  const { buildEffectivePriorityHtml } = loadRenderHelpers();

  const html = buildEffectivePriorityHtml({
    priority: 110,
    effective_priority: 105,
    success_rate: 0.991
  });

  assert.ok(!html.includes('ch-priority-label'));
  assert.ok(html.includes('>110<'));
  assert.ok(html.includes('>105<'));
});

test('buildEffectivePriorityHtml 在健康度等于优先级时只显示一次优先级', () => {
  const { buildEffectivePriorityHtml } = loadRenderHelpers();

  const html = buildEffectivePriorityHtml({
    priority: 100,
    effective_priority: 100
  });

  assert.equal((html.match(/ch-priority-row/g) || []).length, 1);
  assert.equal((html.match(/>100</g) || []).length, 1);
  assert.ok(!html.includes('ch-priority-health'));
});

test('buildChannelTimingHtml 渲染耗时和带单位的调用汇总', () => {
  const { buildChannelTimingHtml } = loadRenderHelpers();

  const html = buildChannelTimingHtml({
    avgFirstByteTimeSeconds: 2.3,
    avgDurationSeconds: 22.23,
    success: 17,
    error: 3
  });

  assert.match(html, /首字/);
  assert.match(html, /耗时/);
  assert.match(html, /调用/);
  assert.match(html, />2\.30秒</);
  assert.match(html, />22\.23秒</);
  assert.match(html, /17<\/span>\/<span style="color: var\(--error-600\);">3<\/span>次/);
  assert.doesNotMatch(html, />成功</);
  assert.doesNotMatch(html, />失败</);
});

test('initChannelEventDelegation 允许表头全选 checkbox 触发可见渠道批量选择', () => {
  const listeners = {};
  const container = {
    dataset: {},
    addEventListener(type, handler) {
      listeners[type] = handler;
    }
  };
  let toggleCalls = 0;

  const { initChannelEventDelegation } = loadRenderSandbox({
    document: {
      getElementById(id) {
        return id === 'channels-container' ? container : null;
      }
    },
    toggleVisibleChannelsSelection() {
      toggleCalls += 1;
    },
    selectedChannelIds: new Set(),
    normalizeSelectedChannelID(value) {
      return value;
    }
  });

  initChannelEventDelegation();

  const headerCheckbox = {
    id: 'visibleSelectionCheckbox',
    closest(selector) {
      if (selector === '#visibleSelectionCheckbox') return this;
      return null;
    }
  };

  listeners.change({ target: headerCheckbox });

  assert.equal(toggleCalls, 1);
});
