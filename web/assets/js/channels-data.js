async function loadChannels(type = 'all') {
  try {
    if (channelsCache[type]) {
      channels = channelsCache[type];
      if (typeof syncSelectedChannelsWithLoadedChannels === 'function') {
        syncSelectedChannelsWithLoadedChannels();
      }
      updateModelOptions();
      filterChannels();
      return;
    }

    const url = type === 'all' ? '/admin/channels' : `/admin/channels?type=${encodeURIComponent(type)}`;
    const data = await fetchDataWithAuth(url);

    channelsCache[type] = data || [];
    channels = channelsCache[type];
    if (typeof syncSelectedChannelsWithLoadedChannels === 'function') {
      syncSelectedChannelsWithLoadedChannels();
    }

    updateModelOptions();
    filterChannels();
  } catch (e) {
    console.error('Failed to load channels', e);
    if (window.showError) window.showError(window.t('channels.loadChannelsFailed'));
  }
}

async function loadChannelStatsRange() {
  try {
    const setting = await fetchDataWithAuth('/admin/settings/channel_stats_range');
    if (setting && setting.value) {
      channelStatsRange = setting.value;
    }
  } catch (e) {
    console.error('Failed to load stats range setting', e);
  }
}

async function loadChannelStats(range = channelStatsRange) {
  try {
    const params = new URLSearchParams({ range, limit: '500', offset: '0' });
    const data = await fetchDataWithAuth(`/admin/stats?${params.toString()}`);
    channelStatsById = aggregateChannelStats((data && data.stats) || []);
    filterChannels();
  } catch (err) {
    console.error('Failed to load channel stats', err);
  }
}

function aggregateChannelStats(statsEntries = []) {
  const result = {};

  for (const entry of statsEntries) {
    const channelId = Number(entry.channel_id || entry.channelID);
    if (!Number.isFinite(channelId) || channelId <= 0) continue;

    if (!result[channelId]) {
      result[channelId] = {
        success: 0,
        error: 0,
        total: 0,
        totalInputTokens: 0,
        totalOutputTokens: 0,
        totalCacheReadInputTokens: 0,
        totalCacheCreationInputTokens: 0,
        totalCost: 0,
        _firstByteWeightedSum: 0,
        _firstByteWeight: 0,
        _durationWeightedSum: 0,
        _durationWeight: 0,
        _healthMap: {} // ts -> merged HealthPoint
      };
    }

    const stats = result[channelId];
    const success = toSafeNumber(entry.success);
    const error = toSafeNumber(entry.error);
    const total = toSafeNumber(entry.total);

    stats.success += success;
    stats.error += error;
    stats.total += total;

    const avgFirstByte = Number(entry.avg_first_byte_time_seconds);
    const weight = success || total || 0;
    if (Number.isFinite(avgFirstByte) && avgFirstByte > 0 && weight > 0) {
      stats._firstByteWeightedSum += avgFirstByte * weight;
      stats._firstByteWeight += weight;
    }

    const avgDuration = Number(entry.avg_duration_seconds);
    if (Number.isFinite(avgDuration) && avgDuration > 0 && weight > 0) {
      stats._durationWeightedSum += avgDuration * weight;
      stats._durationWeight += weight;
    }

    stats.totalInputTokens += toSafeNumber(entry.total_input_tokens);
    stats.totalOutputTokens += toSafeNumber(entry.total_output_tokens);
    stats.totalCacheReadInputTokens += toSafeNumber(entry.total_cache_read_input_tokens);
    stats.totalCacheCreationInputTokens += toSafeNumber(entry.total_cache_creation_input_tokens);
    stats.totalCost += toSafeNumber(entry.total_cost);

    // 合并 health_timeline 到渠道级别
    if (Array.isArray(entry.health_timeline)) {
      for (const point of entry.health_timeline) {
        const ts = point.ts;
        if (!stats._healthMap[ts]) {
          stats._healthMap[ts] = {
            ts: ts,
            success: 0,
            error: 0,
            _ftSum: 0, _ftWeight: 0,
            _durSum: 0, _durWeight: 0,
            input_tokens: 0,
            output_tokens: 0,
            cache_read_tokens: 0,
            cache_creation_tokens: 0,
            cost: 0
          };
        }
        const hp = stats._healthMap[ts];
        if (point.rate < 0) continue; // 无数据的时间桶跳过
        hp.success += (point.success || 0);
        hp.error += (point.error || 0);
        const ptTotal = (point.success || 0) + (point.error || 0);
        if (point.avg_first_byte_time > 0 && ptTotal > 0) {
          hp._ftSum += point.avg_first_byte_time * ptTotal;
          hp._ftWeight += ptTotal;
        }
        if (point.avg_duration > 0 && ptTotal > 0) {
          hp._durSum += point.avg_duration * ptTotal;
          hp._durWeight += ptTotal;
        }
        hp.input_tokens += (point.input_tokens || 0);
        hp.output_tokens += (point.output_tokens || 0);
        hp.cache_read_tokens += (point.cache_read_tokens || 0);
        hp.cache_creation_tokens += (point.cache_creation_tokens || 0);
        hp.cost += (point.cost || 0);
      }
    }
  }

  for (const id of Object.keys(result)) {
    const stats = result[id];
    if (stats._firstByteWeight > 0) {
      stats.avgFirstByteTimeSeconds = stats._firstByteWeightedSum / stats._firstByteWeight;
    }
    if (stats._durationWeight > 0) {
      stats.avgDurationSeconds = stats._durationWeightedSum / stats._durationWeight;
    }

    // 构建 healthTimeline 数组
    const healthMap = stats._healthMap;
    const keys = Object.keys(healthMap);
    if (keys.length > 0) {
      keys.sort(); // 按时间排序
      stats.healthTimeline = keys.map(ts => {
        const hp = healthMap[ts];
        const total = hp.success + hp.error;
        return {
          ts: hp.ts,
          rate: total > 0 ? hp.success / total : -1,
          success: hp.success,
          error: hp.error,
          avg_first_byte_time: hp._ftWeight > 0 ? hp._ftSum / hp._ftWeight : 0,
          avg_duration: hp._durWeight > 0 ? hp._durSum / hp._durWeight : 0,
          input_tokens: hp.input_tokens,
          output_tokens: hp.output_tokens,
          cache_read_tokens: hp.cache_read_tokens,
          cache_creation_tokens: hp.cache_creation_tokens,
          cost: hp.cost
        };
      });
    }

    delete stats._firstByteWeightedSum;
    delete stats._firstByteWeight;
    delete stats._durationWeightedSum;
    delete stats._durationWeight;
    delete stats._healthMap;
  }

  return result;
}

function toSafeNumber(value) {
  const num = Number(value);
  return Number.isFinite(num) ? num : 0;
}

// 加载默认测试内容（从系统设置）
async function loadDefaultTestContent() {
  try {
    const setting = await fetchDataWithAuth('/admin/settings/channel_test_content');
    if (setting && setting.value) {
      defaultTestContent = setting.value;
    }
  } catch (e) {
    console.warn('Failed to load default test content, using built-in default', e);
  }
}
