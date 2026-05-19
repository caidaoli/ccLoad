const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const uiSource = fs.readFileSync(path.join(__dirname, 'ui.js'), 'utf8');

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

test('共享 token speed helper 使用首字后生成阶段且小于 1 秒时回退总耗时', () => {
  const calculateTokenSpeed = vm.runInNewContext(
    `(${extractFunction(uiSource, 'calculateTokenSpeed')})`,
    {}
  );

  assert.equal(calculateTokenSpeed(111, 5.9, 4.1), 61.66666666666664);
  assert.equal(calculateTokenSpeed(100, 3, 2.2), 33.333333333333336);
  assert.equal(calculateTokenSpeed(100, 3, 3), 33.333333333333336);
  assert.equal(calculateTokenSpeed(100, 3, 0), 33.333333333333336);
  assert.equal(calculateTokenSpeed(0, 3, 1), null);
  assert.equal(calculateTokenSpeed(100, 0, 0), null);
});

test('共享 token speed helper 暴露到 window 供页面复用', () => {
  assert.match(uiSource, /window\.calculateTokenSpeed\s*=\s*calculateTokenSpeed/);
});
