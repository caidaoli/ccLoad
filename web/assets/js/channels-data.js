async function loadChannels(type = 'all') {
  try {
    if (channelsCache[type]) {
      channels = channelsCache[type];
      updateModelOptions();
      filterChannels();
      return;
    }

    const url = type === 'all' ? '/admin/channels' : `/admin/channels?type=${encodeURIComponent(type)}`;
    const res = await fetchWithAuth(url);
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const response = await res.json();
    const data = response.success ? (response.data || []) : (response || []);

    channelsCache[type] = data;
    channels = data;

    updateModelOptions();
    filterChannels();
  } catch (e) {
    console.error('加载渠道失败', e);
    if (window.showError) showError('加载渠道失败');
  }
}

async function loadChannelStatsRange() {
  try {
    const resp = await fetchWithAuth('/admin/settings/channel_stats_range');
    const data = await resp.json();
    if (data.success && data.data?.value) {
      channelStatsRange = data.data.value;
    }
  } catch (e) {
    console.error('加载统计范围设置失败', e);
  }
}

async function loadChannelStats(range = channelStatsRange) {
  try {
    const params = new URLSearchParams({ range, limit: '500', offset: '0' });
    const res = await fetchWithAuth(`/admin/stats?${params.toString()}`);
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const response = await res.json();
    const statsArray = extractStatsEntries(response);
    channelStatsById = aggregateChannelStats(statsArray);
    filterChannels();
  } catch (err) {
    console.error('加载渠道统计数据失败', err);
  }
}

function extractStatsEntries(response) {
  if (!response) return [];
  if (Array.isArray(response)) return response;
  if (Array.isArray(response.data?.stats)) return response.data.stats;
  if (Array.isArray(response.stats)) return response.stats;
  if (Array.isArray(response.data)) return response.data;
  return [];
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
    const resp = await fetchWithAuth('/admin/settings/channel_test_content');
    const data = await resp.json();
    if (data.success && data.data?.value) {
      defaultTestContent = data.data.value;
    }
  } catch (e) {
    console.warn('加载默认测试内容失败，使用内置默认值', e);
  }
}
