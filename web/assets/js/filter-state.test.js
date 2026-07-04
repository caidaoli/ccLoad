const test = require('node:test');
const assert = require('node:assert/strict');

const { load, save } = require('./filter-state.js');

function memoryStorage(initial = {}) {
  const data = new Map(Object.entries(initial));
  return {
    getItem: (key) => (data.has(key) ? data.get(key) : null),
    setItem: (key, value) => data.set(key, String(value)),
    dump: () => Object.fromEntries(data.entries())
  };
}

test('load reads only the current filter storage key', () => {
  const storage = memoryStorage({
    'trend.range': 'yesterday',
    'trend.filters': JSON.stringify({ range: 'today', channelType: 'all' })
  });

  assert.deepEqual(load('trend.filters', storage), { range: 'today', channelType: 'all' });
});

test('load ignores legacy split filter keys', () => {
  const storage = memoryStorage({
    'trend.range': 'yesterday',
    'trend.channelType': 'openai'
  });

  assert.equal(load('trend.filters', storage), null);
});

test('save serializes filters to the current storage key', () => {
  const storage = memoryStorage();

  save('trend.filters', { range: 'this_week', channelType: 'gemini' }, storage);

  assert.deepEqual(JSON.parse(storage.dump()['trend.filters']), {
    range: 'this_week',
    channelType: 'gemini'
  });
});
