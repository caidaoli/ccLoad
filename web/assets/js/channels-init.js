function highlightFromHash() {
  const m = (location.hash || '').match(/^#channel-(\d+)$/);
  if (!m) return;
  const el = document.getElementById(`channel-${m[1]}`);
  if (!el) return;
  el.scrollIntoView({ behavior: 'smooth', block: 'center' });
  const prev = el.style.boxShadow;
  el.style.transition = 'box-shadow 0.3s ease, background 0.3s ease';
  el.style.boxShadow = '0 0 0 3px rgba(59,130,246,0.35), 0 10px 25px rgba(59,130,246,0.20)';
  el.style.background = 'rgba(59,130,246,0.06)';
  setTimeout(() => {
    el.style.boxShadow = prev || '';
    el.style.background = '';
  }, 1600);
}

// 从URL参数获取目标渠道ID，查询其类型并返回
async function getTargetChannelType() {
  const params = new URLSearchParams(location.search);
  const channelId = params.get('id');
  if (!channelId) return null;

  try {
    const res = await fetchWithAuth(`/admin/channels/${channelId}`);
    if (!res.ok) return null;
    const response = await res.json();
    const channel = response.success ? response.data : response;
    return channel.channel_type || 'anthropic';
  } catch (e) {
    console.error('获取渠道类型失败:', e);
    return null;
  }
}

// localStorage key for channels page filters
const CHANNELS_FILTER_KEY = 'channels.filters';

function saveChannelsFilters() {
  try {
    localStorage.setItem(CHANNELS_FILTER_KEY, JSON.stringify({
      channelType: filters.channelType,
      status: filters.status,
      model: filters.model
    }));
  } catch (_) {}
}

function loadChannelsFilters() {
  try {
    const saved = localStorage.getItem(CHANNELS_FILTER_KEY);
    if (saved) return JSON.parse(saved);
  } catch (_) {}
  return null;
}

document.addEventListener('DOMContentLoaded', async () => {
  if (window.initTopbar) initTopbar('channels');
  setupFilterListeners();
  setupImportExport();
  setupKeyImportPreview();

  await window.ChannelTypeManager.renderChannelTypeRadios('channelTypeRadios');

  // 优先从 localStorage 恢复，其次检查 URL 参数，最后默认 all
  const savedFilters = loadChannelsFilters();
  const targetChannelType = await getTargetChannelType();
  const initialType = targetChannelType || (savedFilters?.channelType) || 'all';

  filters.channelType = initialType;
  if (savedFilters) {
    filters.status = savedFilters.status || 'all';
    filters.model = savedFilters.model || 'all';
    document.getElementById('statusFilter').value = filters.status;
    document.getElementById('modelFilter').value = filters.model;
  }

  // 初始化渠道类型筛选器（替换原Tab逻辑）
  await initChannelTypeFilter(initialType);

  await loadDefaultTestContent();
  await loadChannelStatsRange();

  await loadChannels(initialType);
  await loadChannelStats();
  highlightFromHash();
  window.addEventListener('hashchange', highlightFromHash);
});

// 初始化渠道类型筛选器
async function initChannelTypeFilter(initialType) {
  const select = document.getElementById('channelTypeFilter');
  if (!select) return;

  const types = await window.ChannelTypeManager.getChannelTypes();

  // 添加"全部"选项
  select.innerHTML = '<option value="all">全部</option>';
  types.forEach(type => {
    const option = document.createElement('option');
    option.value = type.value;
    option.textContent = type.display_name;
    if (type.value === initialType) {
      option.selected = true;
    }
    select.appendChild(option);
  });

  // 绑定change事件
  select.addEventListener('change', (e) => {
    const type = e.target.value;
    filters.channelType = type;
    filters.model = 'all';
    document.getElementById('modelFilter').value = 'all';
    saveChannelsFilters();
    loadChannels(type);
  });
}

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    closeModal();
    closeDeleteModal();
    closeTestModal();
    closeKeyImportModal();
  }
});
