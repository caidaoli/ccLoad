const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const tokensSource = fs.readFileSync(path.join(__dirname, 'tokens.js'), 'utf8');
const zhLocaleSource = fs.readFileSync(path.join(__dirname, '..', 'locales', 'zh-CN.js'), 'utf8');
const enLocaleSource = fs.readFileSync(path.join(__dirname, '..', 'locales', 'en.js'), 'utf8');

function duplicateLocaleKeys(source) {
  const counts = new Map();
  for (const match of source.matchAll(/^\s*'([^']+)'\s*:/gm)) {
    counts.set(match[1], (counts.get(match[1]) || 0) + 1);
  }
  return [...counts.entries()]
    .filter(([, count]) => count > 1)
    .map(([key]) => key);
}

test('locale 文件不能重复定义同一个 key', () => {
  assert.deepEqual(duplicateLocaleKeys(zhLocaleSource), []);
  assert.deepEqual(duplicateLocaleKeys(enLocaleSource), []);
});

test('tokens 页时间范围支持自定义区间查询参数', () => {
  assert.match(tokensSource, /let\s+currentCustomTimeRange\s*=\s*null;/);
  assert.match(tokensSource, /values:\s*\[[^\]]*'custom'[^\]]*\]/);
  assert.match(tokensSource, /window\.buildDateRangeQuery\(currentTimeRange,\s*currentCustomTimeRange\)/);
  assert.doesNotMatch(tokensSource, /url\s*\+=\s*`\?range=\$\{currentTimeRange\}`/);
});
