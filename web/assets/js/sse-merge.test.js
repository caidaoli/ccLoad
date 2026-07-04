const test = require('node:test');
const assert = require('node:assert/strict');

function loadSSEMerge() {
  global.window = {};
  delete require.cache[require.resolve('./sse-merge.js')];
  require('./sse-merge.js');
  return global.window.SSEMerge;
}

function mergeVisibleText(api, payloads) {
  const state = api.createState();
  payloads.forEach(payload => api.collectPayload(payload, state));
  return api.formatState(state);
}

test('formatState excludes reasoning summary from visible merged response', () => {
  const api = loadSSEMerge();
  const raw = [
    'event: response.reasoning_summary_text.delta',
    'data: {"type":"response.reasoning_summary_text.delta","delta":"**Explaining API authentication**\\n\\n"}',
    '',
    'event: response.output_text.delta',
    'data: {"type":"response.output_text.delta","delta":"AI 模型本身不会通过 header 获取 key。"}',
    '',
    'data: [DONE]',
    ''
  ].join('\n');

  const state = api.createState();
  api.parsePayloads(raw).forEach(payload => api.collectPayload(payload, state));

  assert.equal(api.formatState(state), 'AI 模型本身不会通过 header 获取 key。');
  assert.equal(
    api.formatState(state, { includeReasoning: true }),
    '**Explaining API authentication**\n\nAI 模型本身不会通过 header 获取 key。'
  );
});

test('formatParts separates reasoning from visible content for chat-style rendering', () => {
  const api = loadSSEMerge();
  const state = api.createState();
  const raw = [
    'event: response.reasoning_summary_text.delta',
    'data: {"type":"response.reasoning_summary_text.delta","delta":"检查上游返回"}',
    '',
    'event: response.output_text.delta',
    'data: {"type":"response.output_text.delta","delta":"最终回答"}',
    '',
    'data: [DONE]',
    ''
  ].join('\n');

  api.parsePayloads(raw).forEach(payload => api.collectPayload(payload, state));

  assert.deepEqual(api.formatParts(state), {
    reasoning: '检查上游返回',
    content: '最终回答'
  });
});

test('formatState keeps Codex output_text and drops reasoning items', () => {
  const api = loadSSEMerge();

  const text = mergeVisibleText(api, [{
    output: [
      { type: 'reasoning', summary: [{ type: 'summary_text', text: 'step by step' }] },
      { type: 'message', role: 'assistant', content: [{ type: 'output_text', text: 'final answer' }] }
    ]
  }]);

  assert.equal(text, 'final answer');
});

test('formatState keeps OpenAI content delta and drops reasoning delta', () => {
  const api = loadSSEMerge();

  const text = mergeVisibleText(api, [
    { choices: [{ delta: { reasoning_content: 'hidden thought' } }] },
    { choices: [{ delta: { content: 'visible answer' } }] }
  ]);

  assert.equal(text, 'visible answer');
});
