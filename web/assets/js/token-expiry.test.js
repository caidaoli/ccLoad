const test = require('node:test');
const assert = require('node:assert/strict');

const TokenExpiry = require('./token-expiry.js');

function withTimezone(timezone, fn) {
  const previousTZ = process.env.TZ;
  process.env.TZ = timezone;

  try {
    fn();
  } finally {
    if (previousTZ === undefined) delete process.env.TZ;
    else process.env.TZ = previousTZ;
  }
}

test('stored token expiry is displayed in the browser local timezone', () => {
  withTimezone('Asia/Shanghai', () => {
    const expiresAt = Date.UTC(2026, 7, 13, 16, 1, 37, 456);
    const controlValue = TokenExpiry.formatDateTimeLocal(expiresAt);

    assert.equal(controlValue, '2026-08-14T00:01');
  });
});

test('unchanged custom expiry is omitted from the update payload', () => {
  withTimezone('Asia/Shanghai', () => {
    const expiresAt = Date.UTC(2026, 7, 13, 16, 1, 37, 456);
    const controlValue = TokenExpiry.formatDateTimeLocal(expiresAt);
    const parsedValue = new Date(controlValue).getTime();

    assert.notEqual(parsedValue, expiresAt);
    assert.deepEqual(TokenExpiry.buildUpdatePayload(
      { type: 'custom', value: controlValue },
      { type: 'custom', value: controlValue },
      parsedValue
    ), {});
  });
});

test('unchanged expiry in a repeated DST hour is omitted from the update payload', () => {
  withTimezone('America/New_York', () => {
    const expiresAt = Date.UTC(2026, 10, 1, 6, 30);
    const controlValue = TokenExpiry.formatDateTimeLocal(expiresAt);
    const parsedValue = new Date(controlValue).getTime();

    assert.equal(controlValue, '2026-11-01T01:30');
    assert.equal(parsedValue, expiresAt - 60 * 60 * 1000);
    assert.deepEqual(TokenExpiry.buildUpdatePayload(
      { type: 'custom', value: controlValue },
      { type: 'custom', value: controlValue },
      parsedValue
    ), {});
  });
});

test('changed expiry is included in the update payload', () => {
  const expiresAt = Date.UTC(2026, 7, 15, 0, 0);

  assert.deepEqual(TokenExpiry.buildUpdatePayload(
    { type: 'custom', value: '2026-08-14T00:01' },
    { type: 'custom', value: '2026-08-15T00:00' },
    expiresAt
  ), { expires_at: expiresAt });
  assert.deepEqual(TokenExpiry.buildUpdatePayload(
    { type: 'custom', value: '2026-08-14T00:01' },
    { type: 'never', value: '' },
    null
  ), { expires_at: null });
});
