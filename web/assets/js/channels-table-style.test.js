const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const css = fs.readFileSync(path.join(__dirname, '..', 'css', 'channels.css'), 'utf8');

test('冷却中的渠道行使用整行渐变，避免每个单元格重复起始导致颜色分段', () => {
  const cooldownRow = css.match(/\.channel-table tbody tr\.channel-card-cooldown\s*\{[^}]+\}/);
  assert.ok(cooldownRow, '缺少冷却渠道行的基础背景样式');
  assert.match(cooldownRow[0], /background:\s*linear-gradient\(/);
  assert.match(cooldownRow[0], /var\(--channel-cooldown-row-bg-start\)/);
  assert.match(cooldownRow[0], /var\(--channel-cooldown-row-bg-end\)/);

  const cooldownHoverRow = css.match(/\.channel-table tbody tr\.channel-card-cooldown:hover\s*\{[^}]+\}/);
  assert.ok(cooldownHoverRow, '缺少冷却渠道行的 hover 背景样式');
  assert.match(cooldownHoverRow[0], /background:\s*linear-gradient\(/);
  assert.match(cooldownHoverRow[0], /var\(--channel-cooldown-row-hover-bg-start\)/);
  assert.match(cooldownHoverRow[0], /var\(--channel-cooldown-row-hover-bg-end\)/);

  const cooldownCells = css.match(/\.channel-table-row\.channel-card-cooldown\s*>\s*td\s*\{[^}]+\}/);
  assert.ok(cooldownCells, '缺少冷却渠道单元格背景兜底');
  assert.match(cooldownCells[0], /background:\s*transparent/);

  const cooldownHoverCells = css.match(/\.channel-table-row\.channel-card-cooldown:hover\s*>\s*td\s*\{[^}]+\}/);
  assert.ok(cooldownHoverCells, '缺少冷却渠道单元格 hover 背景兜底');
  assert.match(cooldownHoverCells[0], /background:\s*transparent/);
});

test('暗色主题覆盖冷却渠道行颜色变量，避免亮色底破坏可读性', () => {
  const darkBlock = css.match(/html\[data-theme="dark"\]\s*\{[^}]+\}/);
  const systemDarkBlock = css.match(/html\[data-theme="system"\]\[data-resolved-theme="dark"\]\s*\{[^}]+\}/);

  assert.ok(darkBlock, '缺少暗色主题冷却变量覆盖');
  assert.ok(systemDarkBlock, '缺少系统暗色主题冷却变量覆盖');

  for (const block of [darkBlock[0], systemDarkBlock[0]]) {
    assert.match(block, /--channel-cooldown-row-bg-start:\s*rgba\(127,\s*29,\s*29,/);
    assert.match(block, /--channel-cooldown-row-bg-end:\s*rgba\(120,\s*53,\s*15,/);
    assert.match(block, /--channel-cooldown-row-hover-bg-start:\s*rgba\(127,\s*29,\s*29,/);
    assert.match(block, /--channel-cooldown-row-hover-bg-end:\s*rgba\(120,\s*53,\s*15,/);
    assert.doesNotMatch(block, /#fff7ed|#fffbeb|#ffedd5|#fef9c3/);
  }
});

test('渠道桌面表格保留最小总宽度，避免低分辨率下表头文字重叠', () => {
  const tableRule = css.match(/\.channel-table\s*\{[^}]+\}/);
  assert.ok(tableRule, '缺少渠道表格基础样式');
  assert.match(tableRule[0], /min-width:\s*1360px;/);

  const nameColRule = css.match(/\.ch-col-name\s*\{[^}]+\}/);
  assert.ok(nameColRule, '缺少渠道名称列宽度样式');
  assert.match(nameColRule[0], /width:\s*360px;/);
  assert.match(nameColRule[0], /min-width:\s*360px;/);
  assert.match(nameColRule[0], /max-width:\s*360px;/);

  const modelsColRule = css.match(/\.ch-col-models\s*\{[^}]+\}/);
  assert.ok(modelsColRule, '缺少模型列宽度样式');
  assert.match(modelsColRule[0], /width:\s*160px;/);
  assert.match(modelsColRule[0], /min-width:\s*160px;/);
  assert.match(modelsColRule[0], /max-width:\s*160px;/);

  const durationColRule = css.match(/\.ch-col-duration\s*\{[^}]+\}/);
  assert.ok(durationColRule, '缺少耗时列宽度样式');
  assert.match(durationColRule[0], /width:\s*160px;/);
  assert.match(durationColRule[0], /min-width:\s*160px;/);
  assert.match(durationColRule[0], /max-width:\s*160px;/);

  const usageColRule = css.match(/\.ch-col-usage\s*\{[^}]+\}/);
  assert.ok(usageColRule, '缺少消耗列宽度样式');
  assert.match(usageColRule[0], /width:\s*100px;/);
  assert.match(usageColRule[0], /min-width:\s*100px;/);
  assert.match(usageColRule[0], /max-width:\s*100px;/);
});

test('渠道低分辨率桌面隐藏模型列，优先保留关键操作列', () => {
  const lowWidthRule = css.match(/@media\s*\(min-width:\s*769px\)\s*and\s*\(max-width:\s*1360px\)\s*\{[\s\S]*?\.channel-table\s*\{([^}]+)\}[\s\S]*?\.channel-table\s+\.ch-col-models\s*\{([^}]+)\}[\s\S]*?\}/);
  assert.ok(lowWidthRule, '缺少渠道表格低分辨率桌面规则');
  assert.match(lowWidthRule[1], /min-width:\s*1200px;/);
  assert.match(lowWidthRule[2], /display:\s*none;/);
});
