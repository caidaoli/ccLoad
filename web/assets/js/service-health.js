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

  // 进行中的范围后端只返回到当前时间的桶；返回所选范围的结束时刻（含），
  // 供健康条把尚未到达的时间段补成灰格。历史范围已由后端补齐，返回 null。
  function rangeEndMs(rangeKey, customRange, nowMs = Date.now()) {
    const now = new Date(nowMs);
    switch (rangeKey || 'today') {
      case 'today':
        return new Date(now.getFullYear(), now.getMonth(), now.getDate() + 1).getTime() - 1;
      case 'this_week': {
        // 周一为一周起点，与后端 beginningOfWeek 一致
        const daysUntilNextMonday = 7 - ((now.getDay() + 6) % 7);
        return new Date(now.getFullYear(), now.getMonth(), now.getDate() + daysUntilNextMonday).getTime() - 1;
      }
      case 'this_month':
        return new Date(now.getFullYear(), now.getMonth() + 1, 1).getTime() - 1;
      case 'custom': {
        const endMs = Number(customRange && customRange.endMs);
        return Number.isFinite(endMs) ? endMs : null;
      }
      default:
        return null;
    }
  }

  function buildModel(metrics, bucketMinutes = BUCKET_MINUTES, untilMs = null) {
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

      const current = totalsByBucket.get(bucketTs) || { success: 0, error: 0, rateLimited: 0 };
      current.success += toCount(metric.success);
      current.error += toCount(metric.error);
      current.rateLimited += toCount(metric.rate_limited);
      totalsByBucket.set(bucketTs, current);
      startBucketMs = Math.min(startBucketMs, bucketTs);
      endBucketMs = Math.max(endBucketMs, bucketTs);
    }

    // 后端补齐到当前时间，之后补出的桶尚未到达
    const lastReturnedBucketMs = endBucketMs;
    if (totalsByBucket.size > 0 && untilMs !== null && Number.isFinite(Number(untilMs))) {
      endBucketMs = Math.max(endBucketMs, Math.floor(Number(untilMs) / bucketMs) * bucketMs);
    }

    let success = 0;
    let error = 0;
    let rateLimited = 0;
    const pointCount = totalsByBucket.size === 0
      ? 0
      : Math.floor((endBucketMs - startBucketMs) / bucketMs) + 1;
    const points = new Array(pointCount);
    for (let index = 0; index < pointCount; index += 1) {
      const ts = startBucketMs + index * bucketMs;
      const counts = totalsByBucket.get(ts) || { success: 0, error: 0, rateLimited: 0 };
      const total = counts.success + counts.error;
      const rate = total > 0 ? counts.success / total : null;
      success += counts.success;
      error += counts.error;
      rateLimited += counts.rateLimited;
      points[index] = {
        ts,
        success: counts.success,
        error: counts.error,
        rateLimited: counts.rateLimited,
        rate,
        state: classifyRate(rate),
        pending: ts > lastReturnedBucketMs
      };
    }

    const total = success + error;
    const rate = total > 0 ? success / total : null;
    return {
      points,
      success,
      error,
      rateLimited,
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
    classifyRate,
    rangeEndMs
  };
});
