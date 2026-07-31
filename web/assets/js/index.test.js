const test = require('node:test');
const assert = require('node:assert/strict');

const ServiceHealth = require('./service-health.js');

test('service health normalizes the latest seven days into fixed fifteen-minute buckets', () => {
  const nowMs = Date.UTC(2026, 6, 31, 5, 17, 42);
  const range = ServiceHealth.buildRange(nowMs);

  assert.equal(range.bucketMs, 15 * 60 * 1000);
  assert.equal(range.pointCount, 7 * 24 * 4);
  assert.equal(range.endBucketMs, Date.UTC(2026, 6, 31, 5, 15));
  assert.equal(range.startBucketMs, range.endBucketMs - (range.pointCount - 1) * range.bucketMs);

  const metrics = [
    { ts: new Date(range.startBucketMs).toISOString(), success: 19, error: 1 },
    { ts: new Date(range.startBucketMs + range.bucketMs).toISOString(), success: 4, error: 1 },
    { ts: new Date(range.startBucketMs + 2 * range.bucketMs).toISOString(), success: 1, error: 4 },
    { ts: new Date(range.startBucketMs - range.bucketMs).toISOString(), success: 100, error: 0 },
    { ts: 'invalid', success: 100, error: 0 }
  ];

  const model = ServiceHealth.buildModel(metrics, range);

  assert.equal(model.points.length, range.pointCount);
  assert.deepEqual(model.points.slice(0, 4).map(point => point.state), [
    'healthy',
    'warning',
    'critical',
    'unknown'
  ]);
  assert.deepEqual(model.points.slice(0, 3).map(point => point.rate), [0.95, 0.8, 0.2]);
  assert.equal(model.success, 24);
  assert.equal(model.error, 6);
  assert.equal(model.rate, 0.8);
  assert.equal(model.state, 'warning');
});

test('service health treats empty and malformed metrics as unknown instead of healthy', () => {
  const range = ServiceHealth.buildRange(Date.UTC(2026, 6, 31, 5, 17));
  const model = ServiceHealth.buildModel([
    { ts: new Date(range.endBucketMs).toISOString(), success: -1, error: 'bad' }
  ], range);

  assert.equal(model.points.at(-1).state, 'unknown');
  assert.equal(model.rate, null);
  assert.equal(model.state, 'unknown');
});
