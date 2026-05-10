const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const script = fs.readFileSync(path.join(__dirname, 'model-test.js'), 'utf8');

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

function createCell() {
  return {
    textContent: '',
    innerHTML: '',
    title: '',
    dataset: {},
    classList: {
      add() {}
    }
  };
}

function createResultRow(costMultiplier = '0.85') {
  const cells = {
    '.first-byte-duration': createCell(),
    '.duration': createCell(),
    '.input-tokens': createCell(),
    '.output-tokens': createCell(),
    '.speed': createCell(),
    '.cache-read': createCell(),
    '.cache-create': createCell(),
    '.cost': createCell(),
    '.response': createCell()
  };

  return {
    cells,
    style: { background: '' },
    dataset: { costMultiplier },
    querySelector(selector) {
      return cells[selector] || null;
    }
  };
}

function loadCostHelpers(extraSandbox = {}) {
  const sandbox = {
    buildCostStackHtml(standard, effective, options) {
      return `<span class="cost-stack cost-stack--${options.tone}"><span class="cost-stack-standard">$${standard.toFixed(3)}</span><span class="cost-stack-effective">$${effective.toFixed(3)}</span></span>`;
    },
    formatCost(value) {
      return `$${Number(value).toFixed(3)}`;
    },
    channelsList: [],
    ...extraSandbox
  };

  vm.runInNewContext(
    `${extractFunction(script, 'normalizeModelTestCostMultiplier')}
${extractFunction(script, 'buildModelTestCostDisplay')}
${extractFunction(script, 'getRowCostMultiplier')}`,
    sandbox
  );

  return sandbox;
}

test('model-test 成本 helper 输出标准成本和渠道倍率后的实际成本', () => {
  const sandbox = loadCostHelpers();

  const discounted = sandbox.buildModelTestCostDisplay(0.02, 0.85);
  assert.equal(
    discounted.html,
    '<span class="cost-stack cost-stack--warning"><span class="cost-stack-standard">$0.020</span><span class="cost-stack-effective">$0.017</span></span>'
  );
  assert.equal(discounted.effectiveCost, 0.017);

  const free = sandbox.buildModelTestCostDisplay(0.02, 0);
  assert.equal(
    free.html,
    '<span class="cost-stack cost-stack--warning"><span class="cost-stack-standard">$0.020</span><span class="cost-stack-effective">$0.000</span></span>'
  );
  assert.equal(free.effectiveCost, 0);

  assert.equal(sandbox.buildModelTestCostDisplay(0, 0.85), null);
  assert.equal(sandbox.normalizeModelTestCostMultiplier(-1), 1);
});

test('model-test 测试结果把成本列渲染为实际成本组件并按实际成本排序', () => {
  const sandbox = loadCostHelpers({
    i18nText(_key, fallback) {
      return fallback;
    },
    formatDurationMs(value) {
      return `${value}ms`;
    },
    pickPositiveTokenCount(...values) {
      return values.find(value => Number(value) > 0) ?? null;
    },
    calculateTestSpeed() {
      return null;
    }
  });

  vm.runInNewContext(
    `${extractFunction(script, 'applyTestResultToRow')}
${extractFunction(script, 'parseNumericCellValue')}
${extractFunction(script, 'getRowSortValue')}`,
    sandbox
  );

  const row = createResultRow('0.85');
  sandbox.applyTestResultToRow(row, {
    success: true,
    first_byte_duration_ms: 1200,
    duration_ms: 2300,
    cost_usd: 0.02,
    usage: { input_tokens: 10, output_tokens: 5 },
    response_text: 'ok'
  });

  assert.match(row.cells['.cost'].innerHTML, /cost-stack--warning/);
  assert.match(row.cells['.cost'].innerHTML, /cost-stack-standard">\$0\.020/);
  assert.match(row.cells['.cost'].innerHTML, /cost-stack-effective">\$0\.017/);
  assert.equal(row.cells['.cost'].dataset.sortValue, '0.017');
  assert.equal(sandbox.getRowSortValue(row, 'cost'), 0.017);
});
