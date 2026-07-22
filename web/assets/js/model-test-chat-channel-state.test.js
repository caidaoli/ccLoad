const test = require('node:test');
const assert = require('node:assert/strict');

const { createSelection } = require('./model-test-chat-channel-state.js');

function memoryStorage(initial = {}) {
  const values = new Map(Object.entries(initial));
  let writes = 0;
  return {
    getItem(key) {
      return values.has(key) ? values.get(key) : null;
    },
    setItem(key, value) {
      writes += 1;
      values.set(key, String(value));
    },
    removeItem(key) {
      writes += 1;
      values.delete(key);
    },
    get writes() {
      return writes;
    }
  };
}

test('chat channel selection survives candidate refresh without rewriting the preference', () => {
  const storage = memoryStorage({ chat_channel: '2' });
  const selection = createSelection(storage, 'chat_channel');
  const anyrouter = { id: 1, name: 'anyrouter' };
  const cliProxyGrok = { id: 2, name: 'cliProxy-grok' };

  assert.equal(selection.resolve([anyrouter, cliProxyGrok]), cliProxyGrok);
  assert.equal(storage.writes, 0);

  assert.equal(selection.resolve([anyrouter]), anyrouter);
  assert.equal(storage.getItem('chat_channel'), '2');
  assert.equal(storage.writes, 0);

  assert.equal(selection.resolve([anyrouter, cliProxyGrok]), cliProxyGrok);
  assert.equal(storage.writes, 0);
});

test('only an explicit channel selection replaces the persisted preference', () => {
  const storage = memoryStorage({ chat_channel: '2' });
  const selection = createSelection(storage, 'chat_channel');
  const anyrouter = { id: 1, name: 'anyrouter' };

  selection.select(anyrouter);

  assert.equal(storage.getItem('chat_channel'), '1');
  assert.equal(storage.writes, 1);
  assert.equal(selection.resolve([{ id: '1', name: 'reloaded-anyrouter' }]).name, 'reloaded-anyrouter');
});
