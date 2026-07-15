const test = require('node:test');
const assert = require('node:assert/strict');

const {
  normalizeOptions,
  loadOptions,
  saveOptions,
  buildChatRequestPayload,
  DEFAULT_OPTIONS,
  STORAGE_KEY
} = require('./model-test-advanced-options.js');

function memoryStorage(initial = {}) {
  const data = new Map(Object.entries(initial));
  return {
    getItem: (key) => data.has(key) ? data.get(key) : null,
    setItem: (key, value) => data.set(key, String(value)),
    removeItem: (key) => data.delete(key),
    dump: () => Object.fromEntries(data.entries())
  };
}

test('normalizeOptions keeps valid values and drops invalid numeric values', () => {
  const options = normalizeOptions({
    systemPrompt: '  keep it short  ',
    temperature: '0.7',
    topP: '0.9',
    contextMessages: '3',
    maxTokens: '2048'
  });

  assert.deepEqual(options, {
    systemPrompt: 'keep it short',
    temperature: 0.7,
    topP: 0.9,
    contextMessages: 3,
    maxTokens: 2048
  });

  assert.deepEqual(normalizeOptions({
    temperature: 'hot',
    topP: '2',
    contextMessages: '-1',
    maxTokens: '0'
  }), DEFAULT_OPTIONS);
});

test('loadOptions and saveOptions serialize only to browser storage', () => {
  const storage = memoryStorage();

  saveOptions(storage, {
    systemPrompt: 'answer in Chinese',
    temperature: 0,
    topP: 1,
    contextMessages: 0,
    maxTokens: 1000
  });

  const raw = JSON.parse(storage.dump()[STORAGE_KEY]);
  assert.deepEqual(raw, {
    systemPrompt: 'answer in Chinese',
    temperature: 0,
    topP: 1,
    contextMessages: 0,
    maxTokens: 1000
  });

  assert.deepEqual(loadOptions(storage), {
    systemPrompt: 'answer in Chinese',
    temperature: 0,
    topP: 1,
    contextMessages: 0,
    maxTokens: 1000
  });
});

test('buildChatRequestPayload applies sampling options and limits recent messages', () => {
  const messages = [
    { role: 'user', content: 'one' },
    { role: 'assistant', content: 'two' },
    { role: 'user', content: 'three' }
  ];

  const payload = buildChatRequestPayload({
    model: 'gpt-test',
    stream: true,
    thinking_effort: 'medium',
    builtin_search: false
  }, messages, {
    systemPrompt: 'answer tersely',
    temperature: 0.4,
    topP: 0.8,
    contextMessages: 2,
    maxTokens: 512
  });

  assert.deepEqual(payload, {
    model: 'gpt-test',
    stream: true,
    thinking_effort: 'medium',
    builtin_search: false,
    messages: [
      { role: 'assistant', content: 'two' },
      { role: 'user', content: 'three' }
    ],
    system_prompt: 'answer tersely',
    temperature: 0.4,
    top_p: 0.8,
    max_tokens: 512
  });
});

test('buildChatRequestPayload treats empty or zero context as unlimited', () => {
  const messages = [
    { role: 'user', content: 'one' },
    { role: 'assistant', content: 'two' }
  ];

  assert.deepEqual(
    buildChatRequestPayload({ model: 'gpt-test' }, messages, { contextMessages: 0 }).messages,
    messages
  );
  assert.deepEqual(
    buildChatRequestPayload({ model: 'gpt-test' }, messages, {}).messages,
    messages
  );
});

test('buildChatRequestPayload strips UI-only thinking field from messages', () => {
  const messages = [
    { role: 'user', content: 'why?' },
    {
      role: 'assistant',
      content: 'because',
      thinking: 'internal chain of thought that must not leave the browser'
    }
  ];

  assert.deepEqual(
    buildChatRequestPayload({ model: 'gpt-test' }, messages, {}).messages,
    [
      { role: 'user', content: 'why?' },
      { role: 'assistant', content: 'because' }
    ]
  );
});
