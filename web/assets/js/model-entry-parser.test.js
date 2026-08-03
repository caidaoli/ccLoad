const test = require('node:test');
const assert = require('node:assert/strict');

const {
  normalizeModelEntries,
  parseJSONModelEntries,
  parseModelEntries
} = require('./model-entry-parser.js');

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

test('JSON 批量模型输入接受模型名和完整模型条目', () => {
  assert.deepEqual(
    parseJSONModelEntries(`[
      " gpt-4o ",
      {"model":"claude-3-5-sonnet","redirect_model":" claude-3-7-sonnet ","disabled":true},
      {"model":"GPT-4O","redirect_model":"ignored-duplicate"}
    ]`),
    [
      { model: 'gpt-4o', redirect_model: '', disabled: false },
      { model: 'claude-3-5-sonnet', redirect_model: 'claude-3-7-sonnet', disabled: true }
    ]
  );
});

test('JSON 批量模型输入接受模型网关 API 响应', () => {
  assert.deepEqual(
    parseJSONModelEntries(JSON.stringify({
      success: true,
      data: [
        {
          MODEL_PROVIDER: 'openai',
          MODEL_ID: 'minimax/MiniMax-M3',
          MODEL_SERIES_ID: 'minimax-m3'
        },
        {
          MODEL_PROVIDER: 'openai',
          MODEL_ID: 'gpt-5.6-sol',
          MODEL_SERIES_ID: 'gpt-pro'
        },
        {
          MODEL_PROVIDER: 'openai',
          MODEL_ID: 'gpt-5.6-sol',
          MODEL_SERIES_ID: 'gpt-5.6-sol'
        }
      ]
    })),
    [
      { model: 'minimax-m3', redirect_model: 'minimax/MiniMax-M3', disabled: false },
      { model: 'gpt-pro', redirect_model: 'gpt-5.6-sol', disabled: false },
      { model: 'gpt-5.6-sol', redirect_model: '', disabled: false }
    ]
  );
});

test('JSON 批量模型输入拒绝无效 JSON 和错误条目结构', () => {
  assert.throws(
    () => parseJSONModelEntries('{'),
    error => error.code === 'invalid_json'
  );
  assert.throws(
    () => parseJSONModelEntries('{"model":"gpt-4o"}'),
    error => error.code === 'array_required'
  );
  assert.throws(
    () => parseJSONModelEntries('[{"redirect_model":"gpt-4.1"}]'),
    error => error.code === 'model_required' && error.index === 0
  );
  assert.throws(
    () => parseJSONModelEntries('[{"model":"gpt-4o","disabled":"yes"}]'),
    error => error.code === 'invalid_disabled' && error.index === 0
  );
  assert.throws(
    () => parseJSONModelEntries('{"data":[{"MODEL_ID":"gpt-5.6-sol"}]}'),
    error => error.code === 'gateway_series_required' && error.index === 0
  );
  assert.throws(
    () => parseJSONModelEntries('{"data":[{"MODEL_SERIES_ID":"gpt-pro"}]}'),
    error => error.code === 'gateway_model_required' && error.index === 0
  );
  assert.throws(
    () => parseJSONModelEntries('{"data":[{"MODEL_SERIES_ID":"   ","MODEL_ID":"gpt-5.6-sol"}]}'),
    error => error.code === 'gateway_series_required' && error.index === 0
  );
  assert.throws(
    () => parseJSONModelEntries('{"data":[{"MODEL_SERIES_ID":"gpt-pro","MODEL_ID":"   "}]}'),
    error => error.code === 'gateway_model_required' && error.index === 0
  );
});

test('模型规范化只改别名并保留原始上游模型名', () => {
  assert.deepEqual(
    normalizeModelEntries([
      { model: 'source/OpenAI/GPT-4O', redirect_model: '' },
      { model: 'vendor/Claude-SONNET', redirect_model: 'custom/Claude-SONNET' }
    ], {
      lowercase_models: true,
      strip_model_source_prefix: true
    }),
    [
      { model: 'gpt-4o', redirect_model: 'source/OpenAI/GPT-4O' },
      { model: 'claude-sonnet', redirect_model: 'custom/Claude-SONNET' }
    ]
  );
});

test('模型规范化发生别名冲突时优先保留无需重定向的精确模型', () => {
  assert.deepEqual(
    normalizeModelEntries([
      { model: 'source/GPT-4O', redirect_model: '' },
      { model: 'vendor/Claude-SONNET', redirect_model: '' },
      { model: 'gpt-4o', redirect_model: '' }
    ], {
      lowercase_models: true,
      strip_model_source_prefix: true
    }),
    [
      { model: 'gpt-4o', redirect_model: '' },
      { model: 'claude-sonnet', redirect_model: 'vendor/Claude-SONNET' }
    ]
  );
});
