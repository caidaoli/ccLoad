(function initStatsGrouping(root, factory) {
  const api = factory();
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
  }
  if (root) {
    root.StatsGrouping = api;
  }
})(typeof window !== 'undefined' ? window : globalThis, function createStatsGrouping() {
  // 合并多条统计为同结构条目（渠道行、合计行共用）：
  // 计数/Token/费用直接相加；首字/耗时按后端返回的各自样本数加权；Tok/s 用 总输出 / 总生成秒数，
  // 生成秒数口径与 calculateTokenSpeed 一致（耗时减首字不足 1 秒时退回总耗时）。
  function aggregateStatsEntries(entries) {
    const aggregate = {
      success: 0,
      error: 0,
      total: 0,
      total_input_tokens: 0,
      total_output_tokens: 0,
      total_cache_read_input_tokens: 0,
      total_cache_creation_input_tokens: 0,
      total_cost: 0,
      effective_cost: 0
    };
    let firstByteWeighted = 0;
    let firstByteSamples = 0;
    let durationWeighted = 0;
    let durationSamples = 0;
    let speedOutputTokens = 0;
    let speedGenerationSeconds = 0;

    for (const entry of entries) {
      aggregate.success += entry.success || 0;
      aggregate.error += entry.error || 0;
      aggregate.total += entry.total || 0;
      aggregate.total_input_tokens += entry.total_input_tokens || 0;
      aggregate.total_output_tokens += entry.total_output_tokens || 0;
      aggregate.total_cache_read_input_tokens += entry.total_cache_read_input_tokens || 0;
      aggregate.total_cache_creation_input_tokens += entry.total_cache_creation_input_tokens || 0;
      aggregate.total_cost += entry.total_cost || 0;
      aggregate.effective_cost += (entry.effective_cost !== undefined && entry.effective_cost !== null)
        ? Number(entry.effective_cost) || 0
        : (entry.total_cost || 0);

      const firstByte = Number(entry.avg_first_byte_time_seconds) || 0;
      const firstByteCount = Number(entry.first_byte_count) || 0;
      if (firstByte > 0 && firstByteCount > 0) {
        firstByteWeighted += firstByte * firstByteCount;
        firstByteSamples += firstByteCount;
      }

      const duration = Number(entry.avg_duration_seconds) || 0;
      const durationCount = Number(entry.duration_count) || 0;
      if (duration > 0 && durationCount > 0) {
        durationWeighted += duration * durationCount;
        durationSamples += durationCount;
      }

      const success = Number(entry.success) || 0;
      if (success <= 0 || duration <= 0) continue;

      const output = Number(entry.total_output_tokens) || 0;
      if (output > 0) {
        let generationSeconds = duration;
        if (firstByte > 0 && firstByte < duration && duration - firstByte >= 1) {
          generationSeconds = duration - firstByte;
        }
        speedOutputTokens += output;
        speedGenerationSeconds += generationSeconds * success;
      }
    }

    aggregate.avg_first_byte_time_seconds = firstByteSamples > 0 ? firstByteWeighted / firstByteSamples : 0;
    aggregate.avg_duration_seconds = durationSamples > 0 ? durationWeighted / durationSamples : 0;
    aggregate.aggregate_speed = speedOutputTokens > 0 && speedGenerationSeconds > 0
      ? speedOutputTokens / speedGenerationSeconds
      : null;
    return aggregate;
  }

  // 同一桶索引的 ts 由后端统一生成，按索引合并；无数据的桶保持 rate=-1
  function mergeHealthTimelines(timelines) {
    const sources = timelines.filter(timeline => Array.isArray(timeline) && timeline.length > 0);
    if (sources.length <= 1) return sources[0] || null;

    return sources[0].map((_, index) => {
      const merged = {
        ts: sources[0][index]?.ts,
        rate: -1,
        success: 0,
        error: 0,
        rate_limited: 0,
        input_tokens: 0,
        output_tokens: 0,
        cache_read_tokens: 0,
        cache_creation_tokens: 0,
        cost: 0
      };
      let hasData = false;
      let firstByteWeighted = 0;
      let firstByteSuccess = 0;
      let durationWeighted = 0;
      let durationSuccess = 0;

      for (const timeline of sources) {
        const point = timeline[index];
        if (!point || !(point.rate >= 0)) continue;
        hasData = true;

        const success = point.success || 0;
        merged.success += success;
        merged.error += point.error || 0;
        merged.rate_limited += point.rate_limited || 0;
        merged.input_tokens += point.input_tokens || 0;
        merged.output_tokens += point.output_tokens || 0;
        merged.cache_read_tokens += point.cache_read_tokens || 0;
        merged.cache_creation_tokens += point.cache_creation_tokens || 0;
        merged.cost += point.cost || 0;
        if (success > 0 && point.avg_first_byte_time > 0) {
          firstByteWeighted += point.avg_first_byte_time * success;
          firstByteSuccess += success;
        }
        if (success > 0 && point.avg_duration > 0) {
          durationWeighted += point.avg_duration * success;
          durationSuccess += success;
        }
      }

      if (hasData) {
        const total = merged.success + merged.error;
        merged.rate = total > 0 ? merged.success / total : 0;
        merged.avg_first_byte_time = firstByteSuccess > 0 ? firstByteWeighted / firstByteSuccess : 0;
        merged.avg_duration = durationSuccess > 0 ? durationWeighted / durationSuccess : 0;
      }
      return merged;
    });
  }

  // 按 channel_id 分组（保持首次出现顺序），渠道行字段与单条统计同结构，可复用同一套单元格渲染。
  // 峰值 RPM 不可按模型相加，取后端按渠道合并分钟桶后的 channel_peak_rpm；平均与最近 RPM 可相加。
  // 渠道名、优先级、倍率是渠道级属性，同组各条目一致。
  function groupStatsByChannel(entries) {
    const groups = new Map();
    for (const entry of entries) {
      const key = entry.channel_id ?? null;
      if (!groups.has(key)) groups.set(key, []);
      groups.get(key).push(entry);
    }

    return [...groups.values()].map(models => {
      const first = models[0];
      return {
        ...aggregateStatsEntries(models),
        channel_id: first.channel_id,
        channel_name: first.channel_name,
        channel_auth_type: first.channel_auth_type,
        channel_priority: first.channel_priority,
        cost_multiplier_min: first.cost_multiplier_min,
        cost_multiplier_max: first.cost_multiplier_max,
        peak_rpm: first.channel_peak_rpm || 0,
        avg_rpm: models.reduce((sum, entry) => sum + (entry.avg_rpm || 0), 0),
        recent_rpm: models.reduce((sum, entry) => sum + (entry.recent_rpm || 0), 0),
        health_timeline: mergeHealthTimelines(models.map(entry => entry.health_timeline)),
        models
      };
    });
  }

  return {
    aggregateStatsEntries,
    groupStatsByChannel,
    mergeHealthTimelines
  };
});
