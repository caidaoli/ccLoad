const assert = require('node:assert/strict');
const test = require('node:test');

const { splitPipeSeparatedTestContents, firstPipeSeparatedTestContent } = require('./test-content.js');

test('拆分竖线分隔的测试内容时会去除空白、空项并保持顺序去重', () => {
  assert.deepEqual(
    splitPipeSeparatedTestContents(' first |second|| first|  third '),
    ['first', 'second', 'third']
  );
});

test('没有有效配置时取非空回退内容', () => {
  assert.equal(firstPipeSeparatedTestContent(' || ', 'fallback'), 'fallback');
  assert.equal(firstPipeSeparatedTestContent(' first |second ', 'fallback'), 'first');
});
