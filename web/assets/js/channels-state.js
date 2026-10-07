// 全局状态与通用工具函数
let channels = [];
let channelStatsById = {};
let editingChannelId = null;
let editingChannelAuthType = 'api_key';
let deletingChannelRequest = null;
let testingChannelId = null;
let testingClientProtocol = 'anthropic';
let currentChannelKeyCooldowns = []; // 当前编辑渠道的Key冷却信息
let redirectTableData = []; // 模型重定向表格数据: [{from: '', to: ''}]
let selectedModelIndices = new Set(); // 选中的模型索引集合
let currentModelFilter = ''; // 模型名称筛选关键字
let defaultTestContent = 'When was Claude 3.5 Sonnet released?'; // Default test content (loaded from settings)
let channelStatsRange = 'today'; // 渠道统计时间范围（从设置加载）
let selectedChannelIds = new Set(); // 选中的渠道ID（字符串，避免数字/字符串混用）
let channelsCurrentPage = 1;
const DEFAULT_CHANNELS_PAGE_SIZE = 20;
const MAX_CHANNELS_PAGE_SIZE = 1000;

function normalizeChannelsPageSize(value) {
  const size = Number(String(value ?? '').trim());
  if (!Number.isInteger(size) || size < 1 || size > MAX_CHANNELS_PAGE_SIZE) {
    return DEFAULT_CHANNELS_PAGE_SIZE;
  }
  return size;
}

let channelsPageSize = normalizeChannelsPageSize(localStorage.getItem('channels.pageSize'));

// 表头排序：取值与服务端 parseChannelListSort 一致，省略方向时名称升序、其余降序。
const CHANNEL_SORT_KEYS = ['name', 'priority', 'enabled'];

function defaultChannelSortOrder(key) {
  return key === 'name' ? 'asc' : 'desc';
}

function normalizeChannelsSort(value) {
  const key = CHANNEL_SORT_KEYS.includes(value?.key) ? value.key : 'priority';
  const order = value?.order === 'asc' || value?.order === 'desc' ? value.order : defaultChannelSortOrder(key);
  return { key, order };
}

let channelsSort = normalizeChannelsSort(null);
let channelsTotalPages = 1;
let channelsTotalCount = 0;
let channelsLoadFailed = false; // 最近一次列表加载失败：空列表时渲染就地错误与重试
let allAvailableModels = [];
let allAvailableChannelNames = [];
let batchRefreshResultsByChannelId = new Map();

// 认证类型选项：渠道筛选与编辑抽屉头部共用（日志页复用抽屉时不加载 channels-filters.js）
function getChannelAuthTypeOptions() {
  return [
    { value: 'all', label: window.t('channels.authTypeAll') },
    { value: 'api_key', label: window.t('channels.authTypeAPI') },
    { value: 'codex_oauth', label: window.t('channels.authTypeCodex') },
    { value: 'anthropic_oauth', label: window.t('channels.authTypeAnthropic') },
    { value: 'antigravity_oauth', label: window.t('channels.authTypeAntigravity') },
    { value: 'codebuddy_oauth', label: window.t('channels.authTypeCodeBuddy') },
    { value: 'xai_oauth', label: window.t('channels.authTypeXAI') },
    { value: 'cursor_oauth', label: window.t('channels.authTypeCursor') },
    { value: 'zed_oauth', label: window.t('channels.authTypeZed') },
    { value: 'zai_oauth', label: window.t('channels.authTypeZAI') }
  ];
}

function channelAuthTypeFilterLabel(value) {
  const options = getChannelAuthTypeOptions();
  const option = options.find(item => item.value === value);
  return option ? option.label : options[0].label;
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = { normalizeChannelsPageSize, normalizeChannelsSort, markChannelFormDirty, resetChannelFormDirty, syncChannelSaveButtonLabel };
}

function isTokenChannelsReadOnly() {
  return Boolean(window.WebAuth && window.WebAuth.isAPITokenRole(localStorage));
}

function channelsReadURL(adminPath, dashboardPath) {
  return isTokenChannelsReadOnly() ? dashboardPath : adminPath;
}

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
  searchExact: false,
  status: 'all',
  authType: 'all',
  model: 'all',
  modelExact: false
};

// 内联Key表格状态
let inlineKeyTableData = [];
let inlineKeyVisible = false; // 密码可见性状态
let selectedKeyIndices = new Set(); // 选中的Key索引集合
let currentKeyStatusFilter = 'all'; // 当前状态筛选：all/normal/cooldown/disabled
let inlineURLTableData = []; // API URL 表格数据: [{url: '', exact: false, protocols: []}]
let selectedURLIndices = new Set(); // 选中的 URL 索引集合
let urlStatsMap = {}; // URL实时状态：{ url: { latency_ms, cooled_down, cooldown_remain_ms } }
let channelFormDirty = false; // 表单是否有未保存的更改

// 虚拟滚动实现：优化大量Key时的渲染性能
const VIRTUAL_SCROLL_CONFIG = {
  ROW_HEIGHT: 40,           // 每行高度（像素）
  BUFFER_SIZE: 5,           // 上下缓冲区行数（减少滚动时的闪烁）
  ENABLE_THRESHOLD: 50,     // 启用虚拟滚动的阈值（Key数量）
  CONTAINER_HEIGHT: 250     // 容器高度兜底值（像素）
};

let virtualScrollState = {
  enabled: false,
  scrollTop: 0,
  visibleStart: 0,
  visibleEnd: 0,
  rafId: null,
  resizeObserver: null,
  filteredIndices: [] // 存储筛选后的索引列表（支持状态筛选）
};

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
function syncChannelSaveButtonLabel() {
  const saveLabel = document.getElementById('channelSaveLabel');
  if (!saveLabel) return;
  saveLabel.setAttribute('data-i18n', 'common.save');
  saveLabel.textContent = window.t('common.save') + (channelFormDirty ? ' *' : '');
}

function markChannelFormDirty() {
  channelFormDirty = true;
  const saveBtn = document.getElementById('channelSaveBtn');
  if (saveBtn && !saveBtn.classList.contains('btn-warning')) {
    saveBtn.classList.remove('btn-primary');
    saveBtn.classList.add('btn-warning');
  }
  syncChannelSaveButtonLabel();
}

// Reset form dirty state
function resetChannelFormDirty() {
  channelFormDirty = false;
  const saveBtn = document.getElementById('channelSaveBtn');
  if (saveBtn) {
    saveBtn.classList.remove('btn-warning');
    saveBtn.classList.add('btn-primary');
  }
  syncChannelSaveButtonLabel();
}

// 初始化表单变更追踪（覆盖输入类改动，非输入改动由调用方手动 mark）
function initChannelFormDirtyTracking() {
  const form = document.getElementById('channelForm');
  if (!form || form.dataset.dirtyTracking === '1') return;
  form.dataset.dirtyTracking = '1';

  const uiOnlyIDs = new Set([
    'channelApiKey',
    'selectAllURLs',
    'selectAllKeys',
    'keyStatusFilter',
    'selectAllModels',
    'modelFilterInput',
    'cooldownDetectionTestStatus',
    'cooldownDetectionTestBody'
  ]);

  const uiOnlyClasses = ['url-checkbox', 'key-checkbox', 'model-checkbox'];

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
