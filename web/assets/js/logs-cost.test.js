const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');
const logsCss = fs.readFileSync(path.join(__dirname, '..', 'css', 'logs.css'), 'utf8');

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

test('日志页倍率角标挂在渠道单元格容器右上角，成本列只保留价格对', () => {
  const sandbox = {
    escapeHtml(value) {
      return String(value ?? '')
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;');
    },
    buildChannelTrigger(channelId, channelName, baseURL = '') {
      const title = baseURL ? ` title="${sandbox.escapeHtml(baseURL)}"` : '';
      return `<button type="button" class="channel-link" data-channel-id="${channelId}"${title}>${sandbox.escapeHtml(channelName)}</button>`;
    },
    formatCost(cost) {
      if (!cost) return '';
      return '$' + Number(cost).toFixed(3);
    }
  };

  vm.runInNewContext(
    `${extractFunction(logsSource, 'buildLogChannelDisplay')}
${extractFunction(logsSource, 'buildLogCostDisplay')}`,
    sandbox
  );

  const channelHtml = sandbox.buildLogChannelDisplay({
    channel_id: 7,
    channel_name: '88code-codex1',
    base_url: 'https://example.com',
    cost_multiplier: 1.2
  });
  const costHtml = sandbox.buildLogCostDisplay({
    cost: 0.019,
    cost_multiplier: 1.2
  });

  assert.match(channelHtml, /class="log-channel-cell"/);
  assert.match(channelHtml, /class="channel-link" data-channel-id="7" title="https:\/\/example\.com">88code-codex1<\/button>/);
  assert.match(channelHtml, /class="log-channel-multiplier-badge">1\.2x<\/sup>/);
  assert.match(costHtml, /class="[^"]*log-cost[^"]*log-cost--with-multiplier[^"]*"/);
  assert.match(costHtml, /class="log-cost-standard">\$0\.019<\/span>/);
  assert.match(costHtml, /class="log-cost-effective">\$0\.023<\/span>/);
  assert.doesNotMatch(costHtml, /log-cost-badge--multiplier/);
});

test('日志页普通成本保持单值显示且不追加原价删除线', () => {
  const sandbox = {
    formatCost(cost) {
      if (!cost) return '';
      return '$' + Number(cost).toFixed(3);
    }
  };

  vm.runInNewContext(
    `${extractFunction(logsSource, 'buildLogCostDisplay')}`,
    sandbox
  );

  const html = sandbox.buildLogCostDisplay({
    cost: 0.019,
    cost_multiplier: 1
  });

  assert.match(html, /class="log-cost"/);
  assert.doesNotMatch(html, /log-cost-standard/);
  assert.match(html, /class="log-cost-effective">\$0\.019<\/span>/);
  assert.doesNotMatch(html, /log-cost-badge--multiplier/);
});

test('日志页倍率成本样式把原价变灰删除线并把角标放到右上角', () => {
  assert.match(logsCss, /\.log-channel-cell\s*\{[\s\S]*?position:\s*relative;[\s\S]*?display:\s*flex;[\s\S]*?width:\s*100%;/);
  assert.match(logsCss, /\.log-channel-multiplier-badge\s*\{[\s\S]*?position:\s*absolute;[\s\S]*?top:\s*[^;]+;[\s\S]*?right:\s*[^;]+;/);
  assert.match(logsCss, /\.log-cost-standard\s*\{[\s\S]*?color:\s*var\(--neutral-500\);[\s\S]*?text-decoration:\s*line-through;/);
});

test('日志页表格渲染实际使用倍率成本 helper', () => {
  assert.match(logsSource, /function renderLogs\(data\)\s*\{[\s\S]*?const configDisplay = buildLogChannelDisplay\(entry\);/);
  assert.match(logsSource, /function renderLogs\(data\)\s*\{[\s\S]*?const costDisplay = buildLogCostDisplay\(entry\);/);
});
