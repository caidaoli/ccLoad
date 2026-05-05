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
        if (key === 'channels.priorityUpdateSuccess') return '优先级已更新';
        if (key === 'channels.priorityUpdateFailed') return '优先级更新失败';
        if (key === 'channels.stats.firstByte') return '首字';
        if (key === 'channels.stats.calls') return '调用';
        if (key === 'channels.table.lastSuccess') return '最后成功';
        if (key === 'channels.lastSuccess.noRequests') return '暂无请求';
        if (key === 'channels.lastSuccess.never') return '从未成功';
        if (key === 'channels.lastSuccess.secondsAgo') return `${params.count}秒前`;
        if (key === 'channels.lastSuccess.minutesAgo') return `${params.count}分钟前`;
        if (key === 'channels.lastSuccess.hoursAgo') return `${params.count}小时前`;
        if (key === 'channels.lastSuccess.daysAgo') return `${params.count}天前`;
        if (key === 'channels.lastSuccess.failedStatus') return `失败代码:${params.status}`;
        if (key === 'channels.lastSuccess.failedAt') return `失败于:${params.time}`;
        if (key === 'channels.lastSuccess.failedNoMessage') return '无失败日志';
        if (key === 'channels.lastSuccess.lastSuccessPrefix') return `最后成功 ${params.time}`;
        if (key === 'channels.lastSuccess.detail') return '详情';
        if (key === 'stats.tooltipDuration') return '耗时';
        if (key === 'stats.unitTimes') return '次';
        if (key === 'common.success') return '成功';
        if (key === 'common.failed') return '失败';
        if (key === 'common.copy') return '复制';
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
    setTimeout(fn) {
      fn();
      return 1;
    },
    clearTimeout() {},
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
  assert.match(html, /class="ch-priority-input"[^>]*value="110"/);
  assert.ok(html.includes('>105<'));
});

test('buildEffectivePriorityHtml 在健康度等于优先级时只显示一次优先级', () => {
  const { buildEffectivePriorityHtml } = loadRenderHelpers();

  const html = buildEffectivePriorityHtml({
    priority: 100,
    effective_priority: 100
  });

  assert.equal((html.match(/ch-priority-row/g) || []).length, 1);
  assert.match(html, /class="ch-priority-input"[^>]*value="100"/);
  assert.ok(!html.includes('ch-priority-health'));
});

test('buildEffectivePriorityHtml 渲染可直接编辑的优先级控件', () => {
  const { buildEffectivePriorityHtml } = loadRenderHelpers();

  const html = buildEffectivePriorityHtml({
    id: 42,
    priority: 7
  });

  assert.match(html, /ch-priority-editor-wrap/);
  assert.match(html, /ch-priority-editor/);
  assert.match(html, /class="ch-priority-input"[^>]*data-channel-id="42"[^>]*data-original-priority="7"/);
  assert.doesNotMatch(html, /ch-priority-controls/);
  assert.doesNotMatch(html, /data-action="priority-step"/);
});

test('saveInlineChannelPriority 只调用优先级接口并更新本地状态，不重新拉 channels 或 filter-options', async () => {
  const fetchCalls = [];
  let filterCalls = 0;
  let reloadCalls = 0;
  const channels = [{ id: 7, priority: 10, effective_priority: 11 }];
  const filteredChannels = [{ id: 7, priority: 10, effective_priority: 11 }];
  const { saveInlineChannelPriority } = loadRenderSandbox({
    channels,
    filteredChannels,
    fetchDataWithAuth: async (url, options = {}) => {
      fetchCalls.push({ url, body: JSON.parse(options.body) });
      return {};
    },
    clearChannelsCache() {},
    filterChannels() {
      filterCalls += 1;
    },
    reloadChannelsList() {
      reloadCalls += 1;
      throw new Error('priority update must not reload channel list');
    },
    window: {
      t(key) {
        if (key === 'channels.priorityUpdateSuccess') return '优先级已更新';
        if (key === 'channels.priorityUpdateFailed') return '优先级更新失败';
        return key;
      },
      showSuccess() {},
      showError(error) {
        throw new Error(error);
      }
    }
  });
  const input = {
    value: '20',
    dataset: { channelId: '7', originalPriority: '10' },
    classList: { remove() {} },
    closest() {
      return {
        classList: { toggle() {} },
        querySelectorAll() { return []; }
      };
    }
  };

  await saveInlineChannelPriority(input);

  assert.deepEqual(fetchCalls, [{
    url: '/admin/channels/batch-priority',
    body: { updates: [{ id: 7, priority: 20 }] }
  }]);
  assert.equal(channels[0].priority, 20);
  assert.equal(channels[0].effective_priority, 21);
  assert.equal(filteredChannels[0].priority, 20);
  assert.equal(filteredChannels[0].effective_priority, 21);
  assert.equal(filterCalls, 1);
  assert.equal(reloadCalls, 0);
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

test('buildChannelLastSuccessHtml 显示最近成功的相对时间', () => {
  const { buildChannelLastSuccessHtml } = loadRenderHelpers();
  const now = Date.now();

  const html = buildChannelLastSuccessHtml({
    lastSuccessAt: now - 3 * 60 * 1000,
    lastRequestAt: now - 3 * 60 * 1000,
    lastRequestStatus: 200
  });

  assert.match(html, /ch-last-status--ok/);
  assert.match(html, />3分钟前</);
});

test('buildChannelLastRequestFailureHtml 在最后一次请求失败时生成紧凑详情并转义内容', () => {
  const { buildChannelLastRequestFailureHtml } = loadRenderHelpers();
  const now = Date.now();

  const html = buildChannelLastRequestFailureHtml({
    lastSuccessAt: now - 2 * 60 * 60 * 1000,
    lastRequestAt: now - 45 * 1000,
    lastRequestStatus: 429,
    lastRequestMessage: '第一行\n<script>alert(1)</script>'
  });

  assert.match(html, /ch-last-request/);
  assert.match(html, /失败代码:429/);
  assert.match(html, /失败于:45秒前/);
  assert.match(html, /<summary>详情<\/summary>/);
  assert.match(html, /ch-last-request__panel/);
  assert.match(html, /data-action="copy-last-request-failure"/);
  assert.match(html, />复制<\/button>/);
  assert.doesNotMatch(html, /ch-last-request__message/);
  assert.doesNotMatch(html, /ch-last-request__main/);
  assert.doesNotMatch(html, /ch-last-request__actions/);
  assert.doesNotMatch(html, /最后成功 2小时前/);
  assert.doesNotMatch(html, /从未成功/);
  assert.match(html, /&lt;script&gt;alert\(1\)&lt;\/script&gt;/);
  assert.doesNotMatch(html, /<script>/);
});

test('copyChannelLastRequestFailure 复制详情里的完整失败日志', async () => {
  let copiedText = '';
  const { copyChannelLastRequestFailure } = loadRenderSandbox({
    window: {
      t(key) {
        if (key === 'channels.batchRefreshCopied') return '已复制';
        if (key === 'channels.keyCopyFailed') return '复制失败';
        return key;
      },
      copyToClipboard(text) {
        copiedText = text;
        return Promise.resolve();
      }
    },
    setTimeout(fn) {
      fn();
    }
  });
  const pre = { textContent: '第一行\n第二行完整日志' };
  const lastRequest = {
    querySelector(selector) {
      return selector === '.ch-last-request__detail pre' ? pre : null;
    }
  };
  const btn = {
    textContent: '复制',
    closest(selector) {
      return selector === '.ch-last-request' ? lastRequest : null;
    }
  };

  await copyChannelLastRequestFailure(btn);

  assert.equal(copiedText, '第一行\n第二行完整日志');
  assert.equal(btn.textContent, '复制');
});

test('buildChannelLastSuccessHtml 在没有成功时间时显示占位文字', () => {
  const { buildChannelLastSuccessHtml } = loadRenderHelpers();

  const html = buildChannelLastSuccessHtml({
    lastSuccessAt: 0,
    lastRequestAt: Date.now() - 45 * 1000,
    lastRequestStatus: 429,
    lastRequestMessage: 'rate limit'
  });

  assert.match(html, /ch-last-status--empty/);
  assert.match(html, />从未成功</);
  assert.doesNotMatch(html, /rate limit/);
  assert.doesNotMatch(html, /失败 429/);
});

test('buildChannelLastSuccessHtml 在没有任何请求时返回空字符串', () => {
  const { buildChannelLastSuccessHtml } = loadRenderHelpers();

  const html = buildChannelLastSuccessHtml({
    lastSuccessAt: 0,
    lastRequestAt: 0,
    lastRequestStatus: null
  });

  assert.strictEqual(html, '');
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
      },
      addEventListener() {
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

test('initChannelEventDelegation 不会在 change 事件里保存行内优先级', () => {
  const listeners = {};
  const container = {
    dataset: {},
    addEventListener(type, handler) {
      listeners[type] = handler;
    }
  };
  let flushCalls = 0;
  const input = {
    closest(selector) {
      return selector === '.ch-priority-input' ? this : null;
    }
  };

  const { initChannelEventDelegation } = loadRenderSandbox({
    document: {
      getElementById(id) {
        return id === 'channels-container' ? container : null;
      },
      addEventListener() {
      }
    },
    toggleVisibleChannelsSelection() {
    },
    selectedChannelIds: new Set(),
    normalizeSelectedChannelID(value) {
      return value;
    },
    flushInlineChannelPrioritySave() {
      flushCalls += 1;
    }
  });

  initChannelEventDelegation();
  listeners.change({ target: input });

  assert.equal(flushCalls, 0);
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

test('createChannelCard 会把最后成功状态传给渠道行模板', () => {
  const now = Date.now();
  const { createChannelCard } = loadRenderSandbox({
    channelStatsById: {
      17: {
        success: 1,
        error: 0,
        total: 1,
        lastSuccessAt: now - 60 * 1000,
        lastRequestAt: now - 60 * 1000,
        lastRequestStatus: 200
      }
    }
  });

  const cardData = createChannelCard({
    id: 17,
    name: '成功渠道',
    channel_type: 'openai',
    protocol_transforms: [],
    url: 'https://success.example.com',
    models: [{ model: 'gpt-4o' }],
    priority: 100,
    enabled: true
  });

  assert.match(cardData.lastSuccessHtml, /1分钟前/);
  assert.equal(cardData.mobileLabelLastSuccess, '最后成功');
});

test('createChannelCard 会把失败详情放到渠道行内', () => {
  const now = Date.now();
  const { createChannelCard } = loadRenderSandbox({
    channelStatsById: {
      18: {
        success: 1,
        error: 1,
        total: 2,
        lastSuccessAt: now - 2 * 60 * 1000,
        lastRequestAt: now - 30 * 1000,
        lastRequestStatus: 500,
        lastRequestMessage: 'upstream failed'
      }
    }
  });

  const channel = {
    id: 18,
    name: '失败渠道',
    channel_type: 'openai',
    protocol_transforms: [],
    url: 'https://failed.example.com',
    models: [{ model: 'gpt-4o' }],
    priority: 100,
    enabled: true
  };

  const cardData = createChannelCard(channel);

  assert.doesNotMatch(cardData.rowClasses, /channel-row-has-last-request/);
  assert.doesNotMatch(cardData.lastSuccessHtml, /upstream failed/);
  assert.match(cardData.lastRequestFailureHtml, /ch-last-request/);
  assert.match(cardData.lastRequestFailureHtml, /失败代码:500/);
  assert.match(cardData.lastRequestFailureHtml, /upstream failed/);
});

test('禁用渠道不再把已禁用徽章渲染到优先级列', () => {
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

  assert.doesNotMatch(cardData.effectivePriorityHtml, /已禁用/);
  assert.equal(cardData.toggleSwitchClass, 'channel-enable-switch--off');
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

test('未知批量模型刷新状态不会中断行内渲染', () => {
  const { buildBatchRefreshStatusHtml } = loadRenderHelpers();

  const html = buildBatchRefreshStatusHtml({
    channelID: '23',
    status: 'queued'
  });

  assert.match(html, /channel-refresh-result--queued/);
  assert.match(html, /<span class="channel-refresh-result__summary" title=""><\/span>/);
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
