const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const uiSource = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');

function extractCountFormatter(name) {
  const match = uiSource.match(new RegExp(`function\\s+${name}\\s*\\(count\\)\\s*\\{\\n([\\s\\S]*?)\\n  \\}`));
  assert.ok(match, `缺少 ${name}`);
  return vm.runInNewContext(`(function ${name}(count) {\n${match[1]}\n})`);
}

function extractFunctionBlock(name) {
  const match = uiSource.match(new RegExp(`function\\s+${name}\\s*\\([^)]*\\)\\s*\\{[\\s\\S]*?\\n  \\}`));
  assert.ok(match, `缺少 ${name}`);
  return match[0];
}

test('brand icon 活动请求角标显示 1 到 999', () => {
  const label = extractCountFormatter('brandBadgeLabel');

  assert.equal(label(1), '1');
  assert.equal(label(9), '9');
  assert.equal(label(10), '10');
  assert.equal(label(999), '999');
  assert.equal(label(1000), '999+');
});

test('favicon 活动请求角标保留 1 到 9', () => {
  const label = extractCountFormatter('faviconBadgeLabel');

  assert.equal(label(1), '1');
  assert.equal(label(9), '9');
  assert.equal(label(10), '9+');
  assert.equal(label(999), '9+');
});

test('brand icon 和 favicon 使用独立角标格式化逻辑', () => {
  const drawFaviconBadge = extractFunctionBlock('drawFaviconBadge');
  const updateActiveIndicator = extractFunctionBlock('updateActiveIndicator');

  assert.match(drawFaviconBadge, /faviconBadgeLabel\(count\)/);
  assert.match(updateActiveIndicator, /_activeBadge\.textContent\s*=\s*brandBadgeLabel\(count\)/);
  assert.doesNotMatch(uiSource, /function\s+badgeLabel\s*\(/);
});
