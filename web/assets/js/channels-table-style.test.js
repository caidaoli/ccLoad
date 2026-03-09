const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const css = fs.readFileSync(path.join(__dirname, '..', 'css', 'channels.css'), 'utf8');

test('冷却中的渠道行使用整行渐变，避免每个单元格重复起始导致颜色分段', () => {
  const cooldownRow = css.match(/\.channel-table tbody tr\.channel-card-cooldown\s*\{[^}]+\}/);
  assert.ok(cooldownRow, '缺少冷却渠道行的基础背景样式');
  assert.match(cooldownRow[0], /background:\s*linear-gradient\(/);

  const cooldownHoverRow = css.match(/\.channel-table tbody tr\.channel-card-cooldown:hover\s*\{[^}]+\}/);
  assert.ok(cooldownHoverRow, '缺少冷却渠道行的 hover 背景样式');
  assert.match(cooldownHoverRow[0], /background:\s*linear-gradient\(/);

  const cooldownCells = css.match(/\.channel-table-row\.channel-card-cooldown\s*>\s*td\s*\{[^}]+\}/);
  assert.ok(cooldownCells, '缺少冷却渠道单元格背景兜底');
  assert.match(cooldownCells[0], /background:\s*transparent/);

  const cooldownHoverCells = css.match(/\.channel-table-row\.channel-card-cooldown:hover\s*>\s*td\s*\{[^}]+\}/);
  assert.ok(cooldownHoverCells, '缺少冷却渠道单元格 hover 背景兜底');
  assert.match(cooldownHoverCells[0], /background:\s*transparent/);
});
