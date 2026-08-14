const test = require('node:test');
const assert = require('node:assert/strict');

const TokenExpiry = require('./token-expiry.js');

test('custom token expiry retains its instant when reopened in UTC+8', () => {
  const previousTZ = process.env.TZ;
  process.env.TZ = 'Asia/Shanghai';

  try {
    const expiresAt = Date.UTC(2026, 7, 13, 16, 1);
    const controlValue = TokenExpiry.formatDateTimeLocal(expiresAt);

    assert.equal(controlValue, '2026-08-14T00:01');
    assert.equal(new Date(controlValue).getTime(), expiresAt);
  } finally {
    if (previousTZ === undefined) delete process.env.TZ;
    else process.env.TZ = previousTZ;
  }
});
