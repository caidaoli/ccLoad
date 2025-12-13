// 全局状态与通用工具函数
let channels = [];
let channelStatsById = {};
let editingChannelId = null;
let deletingChannelId = null;
let testingChannelId = null;
let currentChannelKeyCooldowns = []; // 当前编辑渠道的Key冷却信息
let redirectTableData = []; // 模型重定向表格数据: [{from: '', to: ''}]
let defaultTestContent = 'sonnet 4.0的发布日期是什么'; // 默认测试内容（从设置加载）
let channelStatsRange = 'today'; // 渠道统计时间范围（从设置加载）
let channelsCache = {}; // 按类型缓存渠道数据: {type: channels[]}

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
  
  if (h > 0) return `${h}小时${m}分`;
  if (m > 0) return `${m}分${s}秒`;
  return `${s}秒`;
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
  return num.toFixed(2) + '秒';
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
  const labels = {
    'today': '本日',
    'this_week': '本周',
    'this_month': '本月',
    'all': '全部'
  };
  return labels[range] || '本日';
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

function toggleResponse(elementId) {
  const element = document.getElementById(elementId);
  if (element) {
    element.style.display = element.style.display === 'none' ? 'block' : 'none';
  }
}

// 显示Toast提示
function showToast(message, type = 'info') {
  const toast = document.createElement('div');
  toast.textContent = message;

  const channelModal = document.getElementById('channelModal');
  const isInChannelModal = channelModal && channelModal.classList.contains('show');

  if (isInChannelModal) {
    toast.style.cssText = `
      position: absolute;
      bottom: 20px;
      left: 50%;
      transform: translateX(-50%);
      padding: 12px 20px;
      border-radius: 8px;
      font-size: 14px;
      font-weight: 500;
      z-index: 10000;
      animation: slideIn 0.3s ease-out;
      box-shadow: 0 4px 12px rgba(0,0,0,0.15);
      max-width: 400px;
      word-wrap: break-word;
    `;
  } else {
    toast.style.cssText = `
      position: fixed;
      top: 80px;
      right: 20px;
      padding: 12px 20px;
      border-radius: 8px;
      font-size: 14px;
      font-weight: 500;
      z-index: 10000;
      animation: slideIn 0.3s ease-out;
      box-shadow: 0 4px 12px rgba(0,0,0,0.15);
      max-width: 400px;
      word-wrap: break-word;
    `;
  }

  if (type === 'success') {
    toast.style.background = 'linear-gradient(135deg, #10b981 0%, #059669 100%)';
    toast.style.color = 'white';
  } else if (type === 'error') {
    toast.style.background = 'linear-gradient(135deg, #ef4444 0%, #dc2626 100%)';
    toast.style.color = 'white';
  } else {
    toast.style.background = 'linear-gradient(135deg, #3b82f6 0%, #2563eb 100%)';
    toast.style.color = 'white';
  }

  if (isInChannelModal) {
    const modalContent = channelModal.querySelector('.modal-content');
    if (modalContent.style.position !== 'relative') {
      modalContent.style.position = 'relative';
    }
    modalContent.appendChild(toast);

    setTimeout(() => {
      toast.style.animation = 'slideOut 0.3s ease-in';
      setTimeout(() => {
        if (toast.parentNode === modalContent) {
          modalContent.removeChild(toast);
        }
      }, 300);
    }, 3000);
  } else {
    document.body.appendChild(toast);

    setTimeout(() => {
      toast.style.animation = 'slideOut 0.3s ease-in';
      setTimeout(() => {
        if (toast.parentNode === document.body) {
          document.body.removeChild(toast);
        }
      }, 300);
    }, 3000);
  }
}
