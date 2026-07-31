const test = require('node:test');
const assert = require('node:assert/strict');
const { matchModelGlob, isModelPattern } = require('./model-glob.js');

test('isModelPattern 与后端 IsModelPattern 一致', () => {
  assert.equal(isModelPattern('gpt-4.1'), false);
  assert.equal(isModelPattern('gpt-*'), true);
  assert.equal(isModelPattern('gpt-?-mini'), true);
  assert.equal(isModelPattern('*'), true);
  assert.equal(isModelPattern('?'), true);
  assert.equal(isModelPattern(''), false);
  assert.equal(isModelPattern(null), false);
  assert.equal(isModelPattern(undefined), false);
  assert.equal(isModelPattern(123), false);
});

test('matchModelGlob 基本语义', () => {
  assert.equal(matchModelGlob('gpt-*', 'gpt-4.1'), true);
  assert.equal(matchModelGlob('gpt-*', 'gpt-'), true);   // '*' 匹配空
  assert.equal(matchModelGlob('gpt-*', 'gpt'), false);    // 字面 '-' 必须存在
  assert.equal(matchModelGlob('gpt-*', 'claude-4'), false);
  assert.equal(matchModelGlob('gpt-?', 'gpt-4'), true);
  assert.equal(matchModelGlob('gpt-?', 'gpt-41'), false); // '?' 仅匹配单个字符
  assert.equal(matchModelGlob('gpt-?', 'gpt-'), false);    // '?' 必须吃一个字符
  assert.equal(matchModelGlob('gpt-?-mini', 'gpt-7-mini'), true);
  assert.equal(matchModelGlob('gpt-4', 'gpt-4'), true);    // 精确
  assert.equal(matchModelGlob('*', 'anything'), true);
  assert.equal(matchModelGlob('GPT-*', 'gpt-4'), false);  // 大小写敏感
});

test('matchModelGlob 尾部连续 *', () => {
  assert.equal(matchModelGlob('gpt-4**', 'gpt-4'), true);
  assert.equal(matchModelGlob('gpt-4*', 'gpt-41'), true);
  assert.equal(matchModelGlob('gpt-4*', 'gpt-4'), true);
});

test('matchModelGlob ? 必须吃一个字符', () => {
  assert.equal(matchModelGlob('?', ''), false);
  assert.equal(matchModelGlob('a?c', 'abc'), true);
  assert.equal(matchModelGlob('a?c', 'ac'), false);
});

test('matchModelGlob 类型守卫', () => {
  assert.equal(matchModelGlob(null, 'x'), false);
  assert.equal(matchModelGlob('x', null), false);
  assert.equal(matchModelGlob(123, 'x'), false);
});

// 关键契约：按 Unicode code point（[]rune）匹配，而非 UTF-16 code unit。
// 代理对字符（如 😀 = U+1F600，JS 中占 2 个 code unit）必须被 '?' 当作单个字符匹配，
// 与后端 internal/model/config.go 的 []rune 实现一致；旧 UTF-16 实现会在本用例失败。
test('matchModelGlob 按 Unicode code point 匹配（与后端 []rune 一致）', () => {
  assert.equal(matchModelGlob('model-?', 'model-😀'), true);
  assert.equal(matchModelGlob('model-?-x', 'model-😀-x'), true);
  assert.equal(matchModelGlob('模型-?', '模型-😀'), true);
  assert.equal(matchModelGlob('模型-*', '模型-😀-pro'), true);
  assert.equal(matchModelGlob('😀-?', '😀-x'), true);
  assert.equal(matchModelGlob('😀-*', '😀-😀-😀'), true);
  // 多 code point 表情序列：每个 '?' 匹配一个 code point
  assert.equal(matchModelGlob('?-?', '😀-🚀'), true);
  assert.equal(matchModelGlob('?-?', '🙂-🙃'), true);
});
