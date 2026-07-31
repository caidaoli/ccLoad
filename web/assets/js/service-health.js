(function (root, factory) {
  const api = factory();
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  if (root) root.ServiceHealth = api;
})(typeof window !== 'undefined' ? window : globalThis, function () {
  'use strict';

  const DAYS = 7;
  const BUCKET_MINUTES = 15;
  const BUCKET_MS = BUCKET_MINUTES * 60 * 1000;
  const POINT_COUNT = DAYS * 24 * (60 / BUCKET_MINUTES);

  function buildRange(nowMs = Date.now()) {
    const safeNowMs = Number.isFinite(Number(nowMs)) ? Number(nowMs) : Date.now();
    const endBucketMs = Math.floor(safeNowMs / BUCKET_MS) * BUCKET_MS;
    return {
      startBucketMs: endBucketMs - (POINT_COUNT - 1) * BUCKET_MS,
      endBucketMs,
      endMs: safeNowMs,
      bucketMs: BUCKET_MS,
      bucketMinutes: BUCKET_MINUTES,
      pointCount: POINT_COUNT
    };
  }

  function toCount(value) {
    const count = Number(value);
    return Number.isFinite(count) && count > 0 ? count : 0;
  }

  function parseTimestamp(value) {
    if (typeof value === 'number' && Number.isFinite(value)) {
      return value < 1e12 ? value * 1000 : value;
    }
    const parsed = Date.parse(value);
    return Number.isFinite(parsed) ? parsed : null;
  }

  function classifyRate(rate) {
    if (!Number.isFinite(rate)) return 'unknown';
    if (rate >= 0.95) return 'healthy';
    if (rate >= 0.80) return 'warning';
    return 'critical';
  }

  function buildModel(metrics, range = buildRange()) {
    const totalsByBucket = new Map();
    for (const metric of Array.isArray(metrics) ? metrics : []) {
      const timestamp = parseTimestamp(metric && metric.ts);
      if (timestamp === null) continue;
      const bucketTs = Math.floor(timestamp / range.bucketMs) * range.bucketMs;
      if (bucketTs < range.startBucketMs || bucketTs > range.endBucketMs) continue;

      const current = totalsByBucket.get(bucketTs) || { success: 0, error: 0 };
      current.success += toCount(metric.success);
      current.error += toCount(metric.error);
      totalsByBucket.set(bucketTs, current);
    }

    let success = 0;
    let error = 0;
    const points = new Array(range.pointCount);
    for (let index = 0; index < range.pointCount; index += 1) {
      const ts = range.startBucketMs + index * range.bucketMs;
      const counts = totalsByBucket.get(ts) || { success: 0, error: 0 };
      const total = counts.success + counts.error;
      const rate = total > 0 ? counts.success / total : null;
      success += counts.success;
      error += counts.error;
      points[index] = {
        ts,
        success: counts.success,
        error: counts.error,
        rate,
        state: classifyRate(rate)
      };
    }

    const total = success + error;
    const rate = total > 0 ? success / total : null;
    return {
      points,
      success,
      error,
      rate,
      state: classifyRate(rate)
    };
  }

  return {
    DAYS,
    BUCKET_MINUTES,
    buildRange,
    buildModel,
    classifyRate
  };
});
