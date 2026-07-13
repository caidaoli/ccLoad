const test = require('node:test');
const assert = require('node:assert/strict');

const TokenModelTest = require('./token-model-test.js');

test('token model test maps protocols to dashboard proxy paths', () => {
  assert.equal(TokenModelTest.endpointFor('openai', 'gpt-5.6', false), '/dashboard/v1/chat/completions');
  assert.equal(TokenModelTest.endpointFor('anthropic', 'claude-sonnet-4-6', true), '/dashboard/v1/messages');
  assert.equal(TokenModelTest.endpointFor('codex', 'gpt-5.6-codex', true), '/dashboard/v1/responses');
  assert.equal(
    TokenModelTest.endpointFor('gemini', 'gemini-2.5-pro', true),
    '/dashboard/v1beta/models/gemini-2.5-pro:streamGenerateContent'
  );
});

test('token model test builds native protocol payloads without channel fields', () => {
  const anthropic = TokenModelTest.buildPayload('anthropic', 'claude-sonnet-4-6', 'hello', true);
  assert.deepEqual(anthropic, {
    model: 'claude-sonnet-4-6',
    max_tokens: 1024,
    messages: [{ role: 'user', content: 'hello' }],
    stream: true
  });
  assert.equal('channel_id' in anthropic, false);

  assert.deepEqual(TokenModelTest.buildPayload('codex', 'gpt-5.6-codex', 'hello', false), {
    model: 'gpt-5.6-codex',
    input: 'hello',
    stream: false
  });
});

test('token model test enforces an explicit allowed-model list', () => {
  assert.equal(TokenModelTest.isModelAllowed('gpt-5.6', ['gpt-5.6']), true);
  assert.equal(TokenModelTest.isModelAllowed('foreign-model', ['gpt-5.6']), false);
  assert.equal(TokenModelTest.isModelAllowed('any-model', []), true);
});
