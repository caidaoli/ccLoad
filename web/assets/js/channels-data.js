function buildChannelsListParams(type = 'all') {
  const params = new URLSearchParams();
  if (type && type !== 'all') {
    params.set('type', type);
  }
  if (filters.search) {
    params.set('search', filters.search);
  }
  if (filters.status && filters.status !== 'all') {
    params.set('status', filters.status);
  }
  if (filters.model && filters.model !== 'all') {
    params.set('model', filters.model);
  }
  params.set('limit', String(channelsPageSize));
  params.set('offset', String((channelsCurrentPage - 1) * channelsPageSize));
  return params;
}

async function loadChannels(type = 'all') {
  try {
    const params = buildChannelsListParams(type);
    const url = '/admin/channels?' + params.toString();
    const resp = await fetchAPIWithAuth(url);
    if (!resp.success) {
      throw new Error(resp.error || window.t('channels.loadChannelsFailed'));
    }

    channels = Array.isArray(resp.data) ? resp.data : [];
    channelsTotalCount = Number.isFinite(resp.count) ? resp.count : channels.length;
    channelsTotalPages = Math.max(1, Math.ceil(channelsTotalCount / channelsPageSize));

    if (channelsCurrentPage > channelsTotalPages) {
      channelsCurrentPage = channelsTotalPages;
      return loadChannels(type);
    }

    if (typeof syncSelectedChannelsWithLoadedChannels === 'function') {
      syncSelectedChannelsWithLoadedChannels();
    }

    filterChannels();
    if (typeof updateChannelsPagination === 'function') {
      updateChannelsPagination();
    }
  } catch (e) {
    console.error('Failed to load channels', e);
    if (window.showError) window.showError(window.t('channels.loadChannelsFailed'));
  }
}

// CRUD 操作后同时刷新列表分页与筛选下拉全集
async function reloadChannelsList(type = filters.channelType, status = filters.status) {
  await Promise.all([
    loadChannelsFilterOptions(type, status),
    loadChannels(type)
  ]);
}

// 加载渠道筛选下拉的全集（按 type/status 联动），与列表分页/搜索/模型筛选解耦
async function loadChannelsFilterOptions(type = 'all', status = 'all') {
  try {
    const params = new URLSearchParams();
    if (type && type !== 'all') params.set('type', type);
    if (status && status !== 'all') params.set('status', status);
    const url = '/admin/channels/filter-options' + (params.toString() ? '?' + params.toString() : '');
    const data = await fetchDataWithAuth(url);
    allAvailableChannelNames = Array.isArray(data && data.channel_names) ? data.channel_names : [];
    allAvailableModels = Array.isArray(data && data.models) ? data.models : [];
  } catch (e) {
    console.error('Failed to load filter options', e);
    allAvailableChannelNames = [];
    allAvailableModels = [];
  }
  if (typeof updateModelOptions === 'function') updateModelOptions();
  if (typeof updateChannelNameOptions === 'function') updateChannelNameOptions();
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
    channelStatsById = aggregateChannelStats((data && data.stats) || [], data && data.channel_health);
    filterChannels();
  } catch (err) {
    console.error('Failed to load channel stats', err);
  }
}

function aggregateChannelStats(statsEntries = [], channelHealth = null) {
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
        effectiveCost: 0,
        _firstByteWeightedSum: 0,
        _firstByteWeight: 0,
        _durationWeightedSum: 0,
        _durationWeight: 0
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
    stats.effectiveCost += (entry.effective_cost !== undefined && entry.effective_cost !== null)
      ? toSafeNumber(entry.effective_cost)
      : toSafeNumber(entry.total_cost);
  }

  for (const id of Object.keys(result)) {
    const stats = result[id];
    if (stats._firstByteWeight > 0) {
      stats.avgFirstByteTimeSeconds = stats._firstByteWeightedSum / stats._firstByteWeight;
    }
    if (stats._durationWeight > 0) {
      stats.avgDurationSeconds = stats._durationWeightedSum / stats._durationWeight;
    }

    // 使用后端按渠道聚合的健康时间线（无需前端 merge）
    // 保留 rate=-1 的空桶，buildChannelHealthIndicator 会渲染为灰色
    if (channelHealth && channelHealth[id]) {
      stats.healthTimeline = channelHealth[id];
    }

    delete stats._firstByteWeightedSum;
    delete stats._firstByteWeight;
    delete stats._durationWeightedSum;
    delete stats._durationWeight;
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
