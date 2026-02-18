// 全局状态与通用工具函数
let channels = [];
let channelStatsById = {};
let editingChannelId = null;
let deletingChannelId = null;
let testingChannelId = null;
let currentChannelKeyCooldowns = []; // 当前编辑渠道的Key冷却信息
let redirectTableData = []; // 模型重定向表格数据: [{from: '', to: ''}]
let selectedModelIndices = new Set(); // 选中的模型索引集合
let currentModelFilter = ''; // 模型名称筛选关键字
let defaultTestContent = 'When was Claude 3.5 Sonnet released?'; // Default test content (loaded from settings)
let channelStatsRange = 'today'; // 渠道统计时间范围（从设置加载）
let channelsCache = {}; // 按类型缓存渠道数据: {type: channels[]}
let selectedChannelIds = new Set(); // 选中的渠道ID（字符串，避免数字/字符串混用）

function normalizeSelectedChannelID(id) {
  const numericID = Number(id);
  if (!Number.isFinite(numericID) || numericID <= 0) {
    return '';
  }
  return String(Math.trunc(numericID));
}

// Filter state
let filters = {
  search: '',
  id: '',
  channelType: 'all',
  status: 'all',
  model: 'all'
};

// 内联Key表格状态
let inlineKeyTableData = [];
let inlineKeyVisible = false; // 密码可见性状态
let selectedKeyIndices = new Set(); // 选中的Key索引集合
let currentKeyStatusFilter = 'all'; // 当前状态筛选：all/normal/cooldown
let channelFormDirty = false; // 表单是否有未保存的更改

// 虚拟滚动实现：优化大量Key时的渲染性能
const VIRTUAL_SCROLL_CONFIG = {
  ROW_HEIGHT: 40,           // 每行高度（像素）
  BUFFER_SIZE: 5,           // 上下缓冲区行数（减少滚动时的闪烁）
  ENABLE_THRESHOLD: 50,     // 启用虚拟滚动的阈值（Key数量）
  CONTAINER_HEIGHT: 250     // 容器固定高度（像素）
};

let virtualScrollState = {
  enabled: false,
  scrollTop: 0,
  visibleStart: 0,
  visibleEnd: 0,
  rafId: null,
  filteredIndices: [] // 存储筛选后的索引列表（支持状态筛选）
};

// 清除渠道缓存（在增删改操作后调用）
function clearChannelsCache() {
  channelsCache = {};
}

function humanizeMS(ms) {
  let s = Math.ceil(ms / 1000);
  const h = Math.floor(s / 3600);
  s = s % 3600;
  const m = Math.floor(s / 60);
  s = s % 60;

  if (h > 0) return window.t('common.timeHM', { h, m });
  if (m > 0) return window.t('common.timeMS', { m, s });
  return window.t('common.timeS', { s });
}

function formatMetricNumber(value) {
  if (value === null || value === undefined) return '--';
  const num = Number(value);
  if (!Number.isFinite(num)) return '--';
  return formatCompactNumber(num);
}

function formatCompactNumber(num) {
  const abs = Math.abs(num);
  if (abs >= 1_000_000) return (num / 1_000_000).toFixed(1).replace(/\.0$/, '') + 'M';
  if (abs >= 1_000) return (num / 1_000).toFixed(1).replace(/\.0$/, '') + 'K';
  return num.toString();
}

function formatSuccessRate(success, total) {
  if (success === null || success === undefined || total === null || total === undefined) return '--';
  const succ = Number(success);
  const ttl = Number(total);
  if (!Number.isFinite(succ) || !Number.isFinite(ttl) || ttl <= 0) return '--';
  return ((succ / ttl) * 100).toFixed(1) + '%';
}

function formatAvgFirstByte(value) {
  if (value === null || value === undefined) return '--';
  const num = Number(value);
  if (!Number.isFinite(num) || num <= 0) return '--';
  return num.toFixed(2) + window.t('common.seconds');
}

function formatCostValue(cost) {
  if (cost === null || cost === undefined) return '--';
  const num = Number(cost);
  if (!Number.isFinite(num)) return '--';
  if (num === 0) return '$0.00';
  if (num < 0) return '--';
  return formatCost(num);
}

function getStatsRangeLabel(range) {
  const keyMap = {
    'today': 'index.timeRange.today',
    'this_week': 'index.timeRange.thisWeek',
    'this_month': 'index.timeRange.thisMonth',
    'all': 'common.all'
  };
  const key = keyMap[range] || 'index.timeRange.today';
  return window.t(key);
}

function formatTimestampForFilename() {
  const pad = (n) => String(n).padStart(2, '0');
  const now = new Date();
  return `${now.getFullYear()}${pad(now.getMonth() + 1)}${pad(now.getDate())}-${pad(now.getHours())}${pad(now.getMinutes())}${pad(now.getSeconds())}`;
}

// 遮罩Key显示（保留前后各4个字符）
function maskKey(key) {
  if (key.length <= 8) return '***';
  return key.slice(0, 4) + '***' + key.slice(-4);
}

// Mark form as having unsaved changes
function markChannelFormDirty() {
  channelFormDirty = true;
  const saveBtn = document.getElementById('channelSaveBtn');
  if (saveBtn && !saveBtn.classList.contains('btn-warning')) {
    saveBtn.classList.remove('btn-primary');
    saveBtn.classList.add('btn-warning');
    saveBtn.textContent = window.t('common.save') + ' *';
  }
}

// Reset form dirty state
function resetChannelFormDirty() {
  channelFormDirty = false;
  const saveBtn = document.getElementById('channelSaveBtn');
  if (saveBtn) {
    saveBtn.classList.remove('btn-warning');
    saveBtn.classList.add('btn-primary');
    saveBtn.textContent = window.t('common.save');
  }
}

// 初始化表单变更追踪（覆盖输入类改动，非输入改动由调用方手动 mark）
function initChannelFormDirtyTracking() {
  const form = document.getElementById('channelForm');
  if (!form || form.dataset.dirtyTracking === '1') return;
  form.dataset.dirtyTracking = '1';

  const uiOnlyIDs = new Set([
    'channelApiKey',
    'selectAllKeys',
    'keyStatusFilter',
    'selectAllModels',
    'modelFilterInput'
  ]);

  const uiOnlyClasses = ['key-checkbox', 'model-checkbox'];

  const shouldTrackTarget = (target) => {
    if (!(target instanceof HTMLElement)) return false;
    if (!target.closest('#channelForm')) return false;

    if (uiOnlyIDs.has(target.id)) return false;
    if (uiOnlyClasses.some(cls => target.classList.contains(cls))) return false;

    const tag = target.tagName;
    return tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT';
  };

  const markDirtyOnEdit = (event) => {
    if (!shouldTrackTarget(event.target)) return;
    markChannelFormDirty();
  };

  form.addEventListener('input', markDirtyOnEdit);
  form.addEventListener('change', markDirtyOnEdit);
}

// 通知系统统一由 ui.js 提供（showNotification/showSuccess/showError）
