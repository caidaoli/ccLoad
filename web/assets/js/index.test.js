const test = require('node:test');
const assert = require('node:assert/strict');

const ServiceHealth = require('./service-health.js');

test('service health request preserves the dashboard date range exactly', () => {
  const request = ServiceHealth.buildRequest(
    'range=custom&start_time=1785456000000&end_time=1785542399000',
    24
  );
  const params = new URLSearchParams(request.query);

  assert.equal(params.get('range'), 'custom');
  assert.equal(params.get('start_time'), '1785456000000');
  assert.equal(params.get('end_time'), '1785542399000');
  assert.equal(params.get('bucket_min'), '15');
  assert.equal(request.bucketMinutes, 15);

  const monthRequest = ServiceHealth.buildRequest('range=last_month', 31 * 24);
  assert.equal(monthRequest.bucketMinutes, 120);
});

test('service health normalizes only the metric buckets returned for the selected range', () => {
  const startMs = Date.UTC(2026, 6, 31, 0, 0);
  const bucketMs = 15 * 60 * 1000;
  const metrics = [
    { ts: new Date(startMs).toISOString(), success: 19, error: 1 },
    { ts: new Date(startMs + bucketMs).toISOString(), success: 4, error: 1 },
    { ts: new Date(startMs + 2 * bucketMs).toISOString(), success: 1, error: 4 },
    { ts: 'invalid', success: 100, error: 0 }
  ];

  const model = ServiceHealth.buildModel(metrics, 15);

  assert.equal(model.bucketMs, bucketMs);
  assert.equal(model.points.length, 3);
  assert.deepEqual(model.points.map(point => point.state), [
    'healthy',
    'warning',
    'critical'
  ]);
  assert.deepEqual(model.points.map(point => point.rate), [0.95, 0.8, 0.2]);
  assert.equal(model.success, 24);
  assert.equal(model.error, 6);
  assert.equal(model.rate, 0.8);
  assert.equal(model.state, 'warning');
});

test('service health treats empty and malformed metrics as unknown instead of healthy', () => {
  const model = ServiceHealth.buildModel([
    { ts: 'invalid', success: -1, error: 'bad' }
  ], 15);

  assert.deepEqual(model.points, []);
  assert.equal(model.rate, null);
  assert.equal(model.state, 'unknown');
});
