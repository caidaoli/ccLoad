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

document.addEventListener('DOMContentLoaded', async () => {
  if (window.initTopbar) initTopbar('channels');
  setupFilterListeners();
  setupImportExport();
  setupKeyImportPreview();

  await window.ChannelTypeManager.renderChannelTypeRadios('channelTypeRadios');

  const types = await window.ChannelTypeManager.getChannelTypes();
  const defaultType = types.length > 0 ? types[0].value : 'all';

  // 检查URL参数是否指定了目标渠道ID，如果是则获取其类型
  const targetChannelType = await getTargetChannelType();
  const initialType = targetChannelType || defaultType;
  filters.channelType = initialType;

  await window.ChannelTypeManager.renderChannelTypeTabs('channelTypeTabs', (type) => {
    filters.channelType = type;
    filters.model = 'all';
    document.getElementById('modelFilter').value = 'all';
    loadChannels(type);
  }, initialType); // 传递初始选中的类型

  await loadDefaultTestContent();
  await loadChannelStatsRange();

  await loadChannels(initialType);
  await loadChannelStats();
  highlightFromHash();
  window.addEventListener('hashchange', highlightFromHash);
});

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    closeModal();
    closeDeleteModal();
    closeTestModal();
    closeKeyImportModal();
  }
});
