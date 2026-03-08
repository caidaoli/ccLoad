const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function loadRenderHelpers() {
  const source = fs.readFileSync(path.join(__dirname, 'channels-render.js'), 'utf8');
  const sandbox = {
    window: {
      t(key, params = {}) {
        if (key === 'channels.table.priority') return '优先级';
        if (key === 'channels.stats.healthScoreLabel') return '健康度';
        if (key === 'channels.stats.successRate') return `成功率 ${params.rate}`;
        return key;
      }
    },
    console
  };

  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);
  return sandbox;
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
