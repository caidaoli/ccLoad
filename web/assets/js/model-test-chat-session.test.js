const test = require('node:test');
const assert = require('node:assert/strict');

const { createSessionState } = require('./model-test-chat-session.js');

test('chat session identity survives reload and rotates on clear', () => {
  const generated = ['session-one', 'session-two'];
  const firstPage = createSessionState(() => generated.shift());
  const firstID = firstPage.restore('');
  assert.equal(firstID, 'session-one');

  const reloadedPage = createSessionState(() => generated.shift());
  assert.equal(reloadedPage.restore(firstID), firstID);
  assert.equal(reloadedPage.current(), firstID);

  assert.equal(reloadedPage.rotate(), 'session-two');
  assert.equal(reloadedPage.current(), 'session-two');
});

test('chat session identity ignores invalid persisted values', () => {
  const state = createSessionState(() => 'fresh-session');
  assert.equal(state.restore({ leaked: true }), 'fresh-session');
});
