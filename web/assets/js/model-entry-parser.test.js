const test = require('node:test');
const assert = require('node:assert/strict');

const { parseModelEntries } = require('./model-entry-parser.js');

test('批量模型输入支持用竖线分隔请求模型和重定向模型', () => {
  assert.deepEqual(
    parseModelEntries(`
      gpt-4o | gpt-4.1,
      claude-3-5-sonnet
      GPT-4O | ignored-duplicate
      | missing-request
      gemini-2.5-pro |
    `),
    [
      { model: 'gpt-4o', redirect_model: 'gpt-4.1' },
      { model: 'claude-3-5-sonnet', redirect_model: '' },
      { model: 'gemini-2.5-pro', redirect_model: '' }
    ]
  );
});

test('批量模型输入同时接受全角竖线', () => {
  assert.deepEqual(
    parseModelEntries('请求模型｜重定向模型'),
    [{ model: '请求模型', redirect_model: '重定向模型' }]
  );
});
