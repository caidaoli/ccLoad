const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const uiSource = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');
const statsSource = fs.readFileSync(path.join(__dirname, 'stats.js'), 'utf8');
const channelsSource = fs.readFileSync(path.join(__dirname, 'channels-render.js'), 'utf8');
const tokensSource = fs.readFileSync(path.join(__dirname, 'tokens.js'), 'utf8');
const statsHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'stats.html'), 'utf8');
const channelsHtml = fs.readFileSync(path.join(__dirname, '..', '..', 'channels.html'), 'utf8');
const stylesCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'styles.css'), 'utf8');
const channelsCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'channels.css'), 'utf8');
const tokensCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'tokens.css'), 'utf8');

function extractFunction(source, name) {
  const signature = `function ${name}`;
  const start = source.indexOf(signature);
  assert.ok(start >= 0, `缺少函数 ${name}`);

  const braceStart = source.indexOf('{', start);
  assert.ok(braceStart >= 0, `函数 ${name} 缺少起始大括号`);

  let depth = 0;
  for (let i = braceStart; i < source.length; i++) {
    const char = source[i];
    if (char === '{') depth++;
    if (char === '}') depth--;
    if (depth === 0) {
      return source.slice(start, i + 1);
    }
  }

  assert.fail(`函数 ${name} 大括号未闭合`);
}

function extractBlock(source, startName, endName) {
  const start = source.indexOf(`function ${startName}`);
  assert.ok(start >= 0, `缺少函数 ${startName}`);

  const end = source.indexOf(`function ${endName}`, start);
  assert.ok(end >= 0, `缺少函数 ${endName}`);

  return source.slice(start, end);
}

function createHelperSandbox() {
  return {
    escapeHtml(value) {
      return String(value ?? '')
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#39;');
    },
    t(key) {
      if (key === 'stats.unknownModel') return '未知模型';
      return key;
    }
  };
}

test('共享成本 helper 生成倍率角标和两行成本 DOM', () => {
  const sandbox = createHelperSandbox();
  vm.runInNewContext(
    extractBlock(uiSource, 'formatCost', 'formatNumber'),
    sandbox
  );

  const costHtml = sandbox.buildCostStackHtml(0.02, 0.017, { tone: 'warning' });
  const badgeHtml = sandbox.buildCornerMultiplierBadge(0.85);

  assert.match(costHtml, /class="cost-stack cost-stack--warning cost-stack--with-multiplier"/);
  assert.match(costHtml, /class="cost-stack-standard">\$0\.020<\/span>/);
  assert.match(costHtml, /class="cost-stack-effective">\$0\.017<\/span>/);
  assert.match(badgeHtml, /class="cell-multiplier-badge">0\.85x<\/sup>/);
});

test('共享成本 helper 在免费渠道上保留 0x 和 $0 的倍率后成本', () => {
  const sandbox = createHelperSandbox();
  vm.runInNewContext(
    extractBlock(uiSource, 'formatCost', 'formatNumber'),
    sandbox
  );

  const costHtml = sandbox.buildCostStackHtml(0.02, 0, { tone: 'warning' });
  const badgeHtml = sandbox.buildCornerMultiplierBadge(0);

  assert.match(costHtml, /class="cost-stack cost-stack--warning cost-stack--with-multiplier"/);
  assert.match(costHtml, /class="cost-stack-standard">\$0\.020<\/span>/);
  assert.match(costHtml, /class="cost-stack-effective">\$0<\/span>/);
  assert.match(badgeHtml, /class="cell-multiplier-badge">0x<\/sup>/);
});

test('stats 页模型单元格右上角显示倍率，成本列输出结构化两行成本', () => {
  const sandbox = createHelperSandbox();
  vm.runInNewContext(
    `${extractBlock(uiSource, 'formatCost', 'formatNumber')}
${extractBlock(statsSource, 'buildStatsModelDisplay', 'renderStatsTable')}`,
    sandbox
  );

  const modelHtml = sandbox.buildStatsModelDisplay({
    model: 'gpt-5.4',
    channel_name: '88code-codex1',
    cost_multiplier: 0.85,
    total_cost: 0.02,
    effective_cost: 0.017
  });
  const costHtml = sandbox.buildStatsCostDisplay(0.02, 0.017);

  assert.match(modelHtml, /class="stats-model-cell"/);
  assert.match(modelHtml, /class="model-tag model-link" data-model="gpt-5\.4" data-channel-name="88code-codex1"/);
  assert.match(modelHtml, /class="cell-multiplier-badge">0\.85x<\/sup>/);
  assert.doesNotMatch(modelHtml, /0\.91x/);
  assert.match(costHtml, /class="cost-stack cost-stack--warning cost-stack--with-multiplier"/);
  assert.match(statsHtml, /<td class="stats-col-cost \{\{costCellClass\}\}" data-mobile-label="\{\{mobileLabelCost\}\}">\{\{\{costText\}\}\}<\/td>/);
  assert.match(statsHtml, /<td class="stats-col-cost" data-mobile-label="\{\{mobileLabelCost\}\}">\{\{\{costText\}\}\}<\/td>/);
  assert.match(statsSource, /buildCornerMultiplierBadge\(entry\.cost_multiplier\)/);
});

test('stats 与 channels 的倍率角标锚定在单元格容器右上角', () => {
  assert.match(stylesCss, /\.cell-multiplier-badge\s*\{[\s\S]*?position:\s*absolute;[\s\S]*?top:\s*[^;]+;[\s\S]*?right:\s*[^;]+;/);
  assert.match(stylesCss, /\.stats-model-cell\s*\{[\s\S]*?position:\s*relative;[\s\S]*?display:\s*flex;[\s\S]*?width:\s*100%;/);
  assert.match(channelsCss, /\.ch-name-cell\s*\{[\s\S]*?position:\s*relative;[\s\S]*?display:\s*block;[\s\S]*?width:\s*100%;/);
});

test('channels 页在名称单元格右上角挂倍率角标，成本列改为两行成本组件', () => {
  const templateMatch = channelsHtml.match(/<template id="tpl-channel-card">[\s\S]*?<\/template>/);
  assert.ok(templateMatch, '缺少 tpl-channel-card 模板');

  const template = templateMatch[0];
  assert.match(template, /<div class="ch-name-cell">[\s\S]*?\{\{\{nameMultiplierBadge\}\}\}/);
  assert.match(channelsSource, /nameMultiplierBadge:/);
  assert.match(channelsSource, /buildCornerMultiplierBadge\(/);
  assert.match(channelsSource, /buildCornerMultiplierBadge\(channel\.cost_multiplier\)/);
  assert.match(channelsSource, /costHtml = buildCostStackHtml\(/);
});

test('channels 页倍率角标只读渠道配置倍率，不依赖统计聚合结果', () => {
  const context = createHelperSandbox();
  context.channelStatsById = {};
  context.formatMetricNumber = (value) => String(value ?? 0);
  context.getCostDisplayInfo = (standard, effective) => {
    const standardCost = Number(standard) || 0;
    const effectiveCost = effective === undefined || effective === null ? standardCost : (Number(effective) || 0);
    return {
      standardCost,
      effectiveCost,
      hasMultiplier: Math.abs(effectiveCost - standardCost) >= 1e-9,
      multiplier: standardCost > 0 ? effectiveCost / standardCost : 1
    };
  };
  context.buildChannelTimingHtml = () => '';
  context.buildChannelHealthIndicator = () => '';
  context.buildChannelTypeBadge = () => '';
  context.buildProtocolTransformBadges = () => '';
  context.buildEffectivePriorityHtml = () => '';
  context.inlineCooldownBadge = () => '';
  context.TemplateEngine = {
    render(_id, data) {
      return data;
    }
  };
  context.window = {
    t(key) {
      return key;
    }
  };

  vm.runInNewContext(
    `${extractBlock(uiSource, 'formatCost', 'formatNumber')}
${extractFunction(channelsSource, 'createChannelCard')}`,
    context
  );

  const card = context.createChannelCard({
    id: 7,
    name: '88code-codex1',
    channel_type: 'codex',
    cost_multiplier: 0.85,
    models: ['gpt-5.4'],
    protocol_transforms: []
  });

  assert.equal(card.nameMultiplierBadge, '<sup class="cell-multiplier-badge">0.85x</sup>');
});

test('渠道卡片倍率角标：0 倍率（免费渠道）显示 0x', () => {
  const uiSource = fs.readFileSync('web/assets/js/ui.js', 'utf8');
  const channelsSource = fs.readFileSync('web/assets/js/channels-render.js', 'utf8');

  const context = createHelperSandbox();
  context.channelStatsById = {};
  context.parseCostInfo = (standardCost, effectiveCost) => {
    return {
      standardCost,
      effectiveCost,
      hasMultiplier: Math.abs(effectiveCost - standardCost) >= 1e-9,
      multiplier: standardCost > 0 ? effectiveCost / standardCost : 1
    };
  };
  context.buildChannelTimingHtml = () => '';
  context.buildChannelHealthIndicator = () => '';
  context.buildChannelTypeBadge = () => '';
  context.buildProtocolTransformBadges = () => '';
  context.buildEffectivePriorityHtml = () => '';
  context.inlineCooldownBadge = () => '';
  context.TemplateEngine = {
    render(_id, data) {
      return data;
    }
  };
  context.window = {
    t(key) {
      return key;
    }
  };

  vm.runInNewContext(
    `${extractBlock(uiSource, 'formatCost', 'formatNumber')}
${extractFunction(channelsSource, 'createChannelCard')}`,
    context
  );

  const card = context.createChannelCard({
    id: 8,
    name: 'free-channel',
    channel_type: 'anthropic',
    cost_multiplier: 0,
    models: ['claude-3-5-sonnet-20241022'],
    protocol_transforms: []
  });

  assert.equal(card.nameMultiplierBadge, '<sup class="cell-multiplier-badge">0x</sup>');
});

test('tokens 页总费用改为调用统计同款 warning 两行成本', () => {
  const sandbox = createHelperSandbox();
  vm.runInNewContext(
    `${extractBlock(uiSource, 'formatCost', 'formatNumber')}
${extractBlock(tokensSource, 'buildCostHtml', 'buildResponseTimeHtml')}`,
    sandbox
  );

  const costHtml = sandbox.buildCostHtml(0.02, 0.017);
  const emptyHtml = sandbox.buildCostHtml(0, 0);

  assert.match(costHtml, /class="cost-stack cost-stack--warning cost-stack--with-multiplier"/);
  assert.match(costHtml, /class="cost-stack-standard">\$0\.020<\/span>/);
  assert.match(costHtml, /class="cost-stack-effective">\$0\.017<\/span>/);
  assert.match(emptyHtml, /token-value-muted/);
  assert.match(tokensCss, /\.token-cost\s+\.cost-stack\s*\{[\s\S]*?align-items:\s*center;[\s\S]*?text-align:\s*center;/);
});

test('共享成本样式把标准成本置灰，并按页面语义区分现价颜色', () => {
  assert.match(stylesCss, /\.cost-stack\s*\{[\s\S]*?display:\s*inline-flex;[\s\S]*?flex-direction:\s*column;/);
  assert.match(stylesCss, /\.cost-stack-standard\s*\{[\s\S]*?color:\s*var\(--neutral-500\);/);
  assert.match(stylesCss, /\.cost-stack--warning\s+\.cost-stack-effective\s*\{[\s\S]*?color:\s*var\(--warning-600\);/);
  assert.match(stylesCss, /\.cost-stack--success\s+\.cost-stack-effective\s*\{[\s\S]*?color:\s*var\(--success-600\);/);
});
