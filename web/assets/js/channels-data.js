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
        _firstByteWeight: 0
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

    stats.totalInputTokens += toSafeNumber(entry.total_input_tokens);
    stats.totalOutputTokens += toSafeNumber(entry.total_output_tokens);
    stats.totalCacheReadInputTokens += toSafeNumber(entry.total_cache_read_input_tokens);
    stats.totalCacheCreationInputTokens += toSafeNumber(entry.total_cache_creation_input_tokens);
    stats.totalCost += toSafeNumber(entry.total_cost);
  }

  for (const id of Object.keys(result)) {
    const stats = result[id];
    if (stats._firstByteWeight > 0) {
      stats.avgFirstByteTimeSeconds = stats._firstByteWeightedSum / stats._firstByteWeight;
    }
    delete stats._firstByteWeightedSum;
    delete stats._firstByteWeight;
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
