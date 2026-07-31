(function (root, factory) {
  const api = factory();
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  if (root) root.ServiceHealth = api;
})(typeof window !== 'undefined' ? window : globalThis, function () {
  'use strict';

  const BUCKET_MINUTES = 15;
  const MAX_POINTS = 7 * 24 * (60 / BUCKET_MINUTES);
  const BUCKET_CHOICES = [15, 30, 60, 120, 240, 480, 720, 1440];

  function buildRequest(dateRangeQuery, rangeHours) {
    const safeHours = Number.isFinite(Number(rangeHours)) && Number(rangeHours) > 0
      ? Number(rangeHours)
      : 24;
    const requiredBucketMinutes = Math.max(BUCKET_MINUTES, Math.ceil(safeHours * 60 / MAX_POINTS));
    const bucketMinutes = BUCKET_CHOICES.find(value => value >= requiredBucketMinutes)
      || BUCKET_CHOICES.at(-1);
    const params = new URLSearchParams(dateRangeQuery || 'range=today');
    params.set('bucket_min', String(bucketMinutes));
    return {
      query: params.toString(),
      bucketMinutes
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

  function buildModel(metrics, bucketMinutes = BUCKET_MINUTES) {
    const safeBucketMinutes = Number.isFinite(Number(bucketMinutes)) && Number(bucketMinutes) > 0
      ? Math.trunc(Number(bucketMinutes))
      : BUCKET_MINUTES;
    const bucketMs = safeBucketMinutes * 60 * 1000;
    const totalsByBucket = new Map();
    let startBucketMs = Infinity;
    let endBucketMs = -Infinity;
    for (const metric of Array.isArray(metrics) ? metrics : []) {
      const timestamp = parseTimestamp(metric && metric.ts);
      if (timestamp === null) continue;
      const bucketTs = Math.floor(timestamp / bucketMs) * bucketMs;

      const current = totalsByBucket.get(bucketTs) || { success: 0, error: 0 };
      current.success += toCount(metric.success);
      current.error += toCount(metric.error);
      totalsByBucket.set(bucketTs, current);
      startBucketMs = Math.min(startBucketMs, bucketTs);
      endBucketMs = Math.max(endBucketMs, bucketTs);
    }

    let success = 0;
    let error = 0;
    const pointCount = totalsByBucket.size === 0
      ? 0
      : Math.floor((endBucketMs - startBucketMs) / bucketMs) + 1;
    const points = new Array(pointCount);
    for (let index = 0; index < pointCount; index += 1) {
      const ts = startBucketMs + index * bucketMs;
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
      state: classifyRate(rate),
      bucketMs,
      bucketMinutes: safeBucketMinutes
    };
  }

  return {
    BUCKET_MINUTES,
    buildRequest,
    buildModel,
    classifyRate
  };
});
