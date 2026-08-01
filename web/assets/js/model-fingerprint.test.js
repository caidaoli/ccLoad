const test = require('node:test');
const assert = require('node:assert/strict');

test('指纹表单根据模型名称选择默认请求协议', () => {
  const previousWindow = global.window;
  global.window = {};
  const modulePath = require.resolve('./model-fingerprint.js');
  delete require.cache[modulePath];

  try {
    const { inferClientProtocolForModel } = require(modulePath);
    const cases = [
      ['gpt-5', 'codex'],
      [' GPT-5.6-sol ', 'codex'],
      ['claude-opus-4-8', 'anthropic'],
      ['gemini-2.5-pro', 'gemini'],
      ['gpt-4o', 'openai'],
      ['deepseek-chat', 'openai']
    ];

    for (const [model, expected] of cases) {
      assert.equal(inferClientProtocolForModel(model), expected, model);
    }
  } finally {
    delete require.cache[modulePath];
    if (previousWindow === undefined) delete global.window;
    else global.window = previousWindow;
  }
});
