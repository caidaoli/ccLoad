const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function loadRenderSandbox(overrides = {}) {
  const protocolSource = fs.readFileSync(path.join(__dirname, 'channels-protocols.js'), 'utf8');
  const source = fs.readFileSync(path.join(__dirname, 'channels-render.js'), 'utf8');
  const sandbox = {
    window: {
      t(key, params = {}) {
        if (key === 'channels.table.priority') return '优先级';
        if (key === 'channels.stats.healthScoreLabel') return '健康度';
        if (key === 'channels.stats.successRate') return `成功率 ${params.rate}`;
        if (key === 'channels.statusDisabled') return '已禁用';
        if (key === 'channels.stats.firstByte') return '首字';
        if (key === 'channels.stats.calls') return '调用';
        if (key === 'stats.tooltipDuration') return '耗时';
        if (key === 'stats.unitTimes') return '次';
        if (key === 'common.success') return '成功';
        if (key === 'common.failed') return '失败';
        if (key === 'common.seconds') return '秒';
        if (key === 'channels.batchRefreshStatus.processing') return '刷新中';
        if (key === 'channels.batchRefreshStatus.updated') return '已更新';
        if (key === 'channels.batchRefreshStatus.unchanged') return '未变化';
        if (key === 'channels.batchRefreshStatus.failed') return '失败';
        if (key === 'channels.batchRefreshRowProcessing') return '正在获取模型列表...';
        if (key === 'channels.batchRefreshRowUpdatedMerge') return `获取 ${params.fetched}，新增 ${params.added}，总计 ${params.total}`;
        if (key === 'channels.batchRefreshRowUpdatedReplace') return `获取 ${params.fetched}，移除 ${params.removed}，总计 ${params.total}`;
        if (key === 'channels.batchRefreshRowUnchanged') return `获取 ${params.fetched}，总计 ${params.total}`;
        if (key === 'channels.batchRefreshRowFailed') return `${params.error}`;
        if (key === 'channels.batchRefreshDetail') return '展开详情';
        if (key === 'channels.batchRefreshClear') return '清除';
        if (key === 'channels.batchRefreshCopied') return '已复制';
        return key;
      }
    },
    TemplateEngine: {
      render(_templateId, data = {}) {
        return data;
      }
    },
    channelStatsById: {},
    formatMetricNumber(value) {
      return String(value ?? '');
    },
    buildCostStackHtml(standard, effective) {
      return `${standard ?? ''}/${effective ?? ''}`;
    },
    buildCornerMultiplierBadge(multiplier) {
      if (!multiplier || Math.abs(Number(multiplier) - 1) < 1e-9) return '';
      return `<sup class="cell-multiplier-badge">${multiplier}x</sup>`;
    },
    getCostDisplayInfo(standard, effective) {
      const standardCost = Number(standard) || 0;
      const effectiveCost = effective === undefined || effective === null ? standardCost : (Number(effective) || 0);
      return {
        standardCost,
        effectiveCost,
        hasMultiplier: Math.abs(effectiveCost - standardCost) >= 1e-9,
        multiplier: standardCost > 0 ? effectiveCost / standardCost : 1
      };
    },
    humanizeMS(ms) {
      return `${ms}ms`;
    },
    console
  };

  Object.assign(sandbox, overrides);

  vm.createContext(sandbox);
  vm.runInContext(`${protocolSource}\n${source}`, sandbox);
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

test('buildProtocolTransformBadges 按完整协议集合渲染额外协议并去重', () => {
  const { buildProtocolTransformBadges } = loadRenderHelpers();

  const html = buildProtocolTransformBadges('anthropic', ['gemini', 'openai', 'anthropic', 'codex', 'openai', 'unknown']);

  assert.match(html, />OpenAI</);
  assert.match(html, />Codex</);
  assert.match(html, />Gemini</);
  assert.doesNotMatch(html, />Anthropic</);
  assert.equal((html.match(/>OpenAI</g) || []).length, 1);
  assert.match(html, /Protocol Transforms: OpenAI/);
});

test('buildChannelTypeBadge 与协议标签使用一致的字号和字重', () => {
  const { buildChannelTypeBadge } = loadRenderHelpers();

  const html = buildChannelTypeBadge('codex');

  assert.match(html, /font-size:\s*0\.68rem/);
  assert.match(html, /font-weight:\s*600/);
  assert.match(html, /padding:\s*2px 6px/);
  assert.match(html, /line-height:\s*1/);
  assert.doesNotMatch(html, /text-transform:\s*uppercase/);
  assert.doesNotMatch(html, /letter-spacing:/);
});

test('createChannelCard 会把额外协议标签传给渠道卡片模板且保留上游协议徽章', () => {
  const { createChannelCard } = loadRenderHelpers();

  const cardData = createChannelCard({
    id: 7,
    name: '协议转换渠道',
    channel_type: 'gemini',
    protocol_transforms: ['openai', 'anthropic', 'gemini'],
    url: 'https://api.example.com',
    models: [{ model: 'gpt-5.4' }],
    priority: 1,
    enabled: true
  });

  assert.match(cardData.typeBadge, />Gemini</);
  assert.match(cardData.protocolTransformBadges, />Claude</);
  assert.match(cardData.protocolTransformBadges, />OpenAI</);
  assert.doesNotMatch(cardData.protocolTransformBadges, />Gemini</);
});

test('禁用渠道会把已禁用徽章渲染到优先级列而不是标题行', () => {
  const { createChannelCard } = loadRenderHelpers();

  const cardData = createChannelCard({
    id: 11,
    name: '禁用渠道',
    channel_type: 'anthropic',
    protocol_transforms: [],
    url: 'https://disabled.example.com',
    models: [{ model: 'claude-4' }],
    priority: 160,
    enabled: false
  });

  assert.equal(cardData.disabledBadge, '');
  assert.match(cardData.effectivePriorityHtml, /已禁用/);
});

test('createChannelCard 会把批量模型刷新结果渲染到渠道行状态槽', () => {
  const { createChannelCard } = loadRenderSandbox({
    batchRefreshResultsByChannelId: new Map([
      ['23', {
        channelID: '23',
        status: 'updated',
        mode: 'replace',
        fetched: 12,
        removed: 3,
        total: 12
      }]
    ])
  });

  const cardData = createChannelCard({
    id: 23,
    name: '刷新渠道',
    channel_type: 'openai',
    protocol_transforms: [],
    url: 'https://refresh.example.com',
    models: [{ model: 'gpt-5.4' }],
    priority: 100,
    enabled: true
  });

  assert.match(cardData.rowClasses, /channel-row-refresh-updated/);
  assert.match(cardData.batchRefreshStatusHtml, /channel-refresh-result--updated/);
  assert.match(cardData.batchRefreshStatusHtml, /已更新/);
  assert.match(cardData.batchRefreshStatusHtml, /获取 12，移除 3，总计 12/);
  assert.doesNotMatch(cardData.batchRefreshStatusHtml, /已更新：获取 12，移除 3，总计 12/);
});

test('clearAllBatchRefreshResults 会清空所有批量模型刷新结果并同步移除行内展示', () => {
  const resultMap = new Map();
  const row = {
    slot: { innerHTML: '旧内容' },
    classList: {
      removed: [],
      added: [],
      remove(...tokens) {
        this.removed.push(...tokens);
      },
      add(token) {
        this.added.push(token);
      }
    },
    querySelector(selector) {
      if (selector === '.ch-refresh-result-slot') return this.slot;
      return null;
    }
  };
  const { clearAllBatchRefreshResults } = loadRenderSandbox({
    batchRefreshResultsByChannelId: resultMap,
    document: {
      getElementById(id) {
        if (id === 'channel-23') return row;
        return null;
      }
    }
  });

  resultMap.set('23', { channelID: '23', status: 'updated', mode: 'merge' });
  clearAllBatchRefreshResults();

  assert.equal(resultMap.size, 0);
  assert.equal(row.slot.innerHTML, '');
  assert.deepEqual(row.classList.added, []);
});

test('失败的批量模型刷新结果行内显示摘要并折叠完整错误', () => {
  const { buildBatchRefreshStatusHtml } = loadRenderHelpers();

  const html = buildBatchRefreshStatusHtml({
    channelID: '7',
    status: 'failed',
    summary: 'HTTP 401 <bad>',
    detail: '完整错误 <script>alert(1)</script>'
  });

  assert.match(html, /channel-refresh-result--failed/);
  assert.match(html, /<span class="channel-refresh-result__status">失败<\/span>/);
  assert.match(html, /HTTP 401 &lt;bad&gt;/);
  assert.doesNotMatch(html, /失败：HTTP 401 &lt;bad&gt;/);
  assert.match(html, /<summary>展开详情<\/summary>/);
  assert.match(html, /完整错误 &lt;script&gt;alert\(1\)&lt;\/script&gt;/);
  assert.doesNotMatch(html, /data-action="copy-batch-refresh-error"/);
  assert.doesNotMatch(html, /复制错误/);
  assert.match(html, /data-action="clear-batch-refresh-result"/);
  assert.doesNotMatch(html, /<script>/);
});
