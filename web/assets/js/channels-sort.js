// ==================== 渠道排序功能 ====================
// 拖拽排序实现,优先级相差10

let sortChannels = []; // 存储排序中的渠道列表
let draggedItem = null; // 当前拖拽的元素

// 打开排序模态框
function showSortModal() {
  const modal = document.getElementById('sortModal');
  if (!modal) return;

  // 获取当前渠道列表(使用筛选后的渠道)
  const sourceChannels = filteredChannels.length > 0 ? filteredChannels : channels;

  if (!sourceChannels || sourceChannels.length === 0) {
    window.showError('无法加载渠道列表');
    return;
  }

  // 复制渠道列表并按优先级排序(从高到低)
  sortChannels = [...sourceChannels].sort((a, b) => {
    // 优先级从高到低
    if (b.priority !== a.priority) {
      return b.priority - a.priority;
    }
    // 优先级相同时按ID排序
    return a.id - b.id;
  });

  // 渲染排序列表
  renderSortList();

  // 显示模态框(使用show类实现居中)
  modal.classList.add('show');
}

// 关闭排序模态框
function closeSortModal() {
  const modal = document.getElementById('sortModal');
  if (modal) {
    modal.classList.remove('show');
  }
  sortChannels = [];
  draggedItem = null;
}

// 渲染排序列表
function renderSortList() {
  const container = document.getElementById('sortListContainer');
  if (!container) return;

  container.innerHTML = '';

  if (sortChannels.length === 0) {
    container.innerHTML = '<p style="text-align: center; color: var(--neutral-500); padding: 40px;">暂无渠道</p>';
    return;
  }

  sortChannels.forEach((channel, index) => {
    const item = createSortItem(channel, index);
    container.appendChild(item);
  });

  // 添加拖拽事件监听
  attachDragListeners();
}

// 创建排序卡片
function createSortItem(channel, index) {
  const template = document.getElementById('tpl-sort-item');
  if (!template) return document.createElement('div');

  // 渠道类型徽章
  const typeColor = getChannelTypeColor(channel.type || 'anthropic');

  // 状态徽章
  let statusBadge = '';
  if (!channel.enabled) {
    statusBadge = '<span style="background: var(--neutral-200); color: var(--neutral-600); padding: 2px 8px; border-radius: 4px; font-size: 12px; font-weight: 500;">已禁用</span>';
  } else if (channel.cooldown_until && new Date(channel.cooldown_until) > new Date()) {
    statusBadge = '<span style="background: var(--error-100); color: var(--error-600); padding: 2px 8px; border-radius: 4px; font-size: 12px; font-weight: 500;">冷却中</span>';
  } else {
    statusBadge = '<span style="background: var(--success-100); color: var(--success-600); padding: 2px 8px; border-radius: 4px; font-size: 12px; font-weight: 500;">正常</span>';
  }

  const html = template.innerHTML
    .replace(/\{\{id\}\}/g, channel.id)
    .replace(/\{\{name\}\}/g, escapeHtml(channel.name))
    .replace(/\{\{priority\}\}/g, channel.priority)
    .replace(/\{\{\{statusBadge\}\}\}/g, statusBadge);

  const div = document.createElement('div');
  div.innerHTML = html;
  const item = div.firstElementChild;

  // 设置索引属性用于拖拽
  item.dataset.index = index;

  return item;
}

// 获取渠道类型颜色
function getChannelTypeColor(type) {
  const colors = {
    anthropic: '#3b82f6',
    openai: '#10b981',
    azure: '#0ea5e9',
    bedrock: '#f59e0b',
    vertex: '#8b5cf6',
    openrouter: '#ec4899',
    cohere: '#06b6d4',
    groq: '#f97316',
    deepseek: '#6366f1',
    qwen: '#14b8a6',
    zhipu: '#a855f7',
    baidu: '#3b82f6',
    ollama: '#84cc16',
    custom: '#6b7280'
  };
  return colors[type.toLowerCase()] || colors.custom;
}

// 添加拖拽事件监听
function attachDragListeners() {
  const items = document.querySelectorAll('.sort-item');

  items.forEach(item => {
    item.addEventListener('dragstart', handleDragStart);
    item.addEventListener('dragend', handleDragEnd);
    item.addEventListener('dragover', handleDragOver);
    item.addEventListener('dragenter', handleDragEnter);
    item.addEventListener('dragleave', handleDragLeave);
    item.addEventListener('drop', handleDrop);
  });
}

// 拖拽开始
function handleDragStart(e) {
  draggedItem = this;
  this.style.opacity = '0.5';
  e.dataTransfer.effectAllowed = 'move';
}

// 拖拽结束
function handleDragEnd(e) {
  this.style.opacity = '1';

  // 移除所有拖拽样式
  document.querySelectorAll('.sort-item').forEach(item => {
    item.style.borderTop = '';
    item.style.borderBottom = '';
  });

  draggedItem = null;
}

// 拖拽经过
function handleDragOver(e) {
  if (e.preventDefault) {
    e.preventDefault();
  }
  e.dataTransfer.dropEffect = 'move';
  return false;
}

// 拖拽进入
function handleDragEnter(e) {
  if (this === draggedItem) return;

  // 显示插入位置提示
  const rect = this.getBoundingClientRect();
  const midpoint = rect.top + rect.height / 2;

  if (e.clientY < midpoint) {
    this.style.borderTop = '2px solid var(--primary-500)';
    this.style.borderBottom = '';
  } else {
    this.style.borderTop = '';
    this.style.borderBottom = '2px solid var(--primary-500)';
  }
}

// 拖拽离开
function handleDragLeave(e) {
  this.style.borderTop = '';
  this.style.borderBottom = '';
}

// 放置
function handleDrop(e) {
  if (e.stopPropagation) {
    e.stopPropagation();
  }

  if (this === draggedItem) return false;

  const draggedIndex = parseInt(draggedItem.dataset.index);
  const targetIndex = parseInt(this.dataset.index);

  if (draggedIndex === targetIndex) return false;

  // 计算插入位置
  const rect = this.getBoundingClientRect();
  const midpoint = rect.top + rect.height / 2;
  const insertBefore = e.clientY < midpoint;

  // 更新数组顺序
  const draggedChannel = sortChannels[draggedIndex];
  sortChannels.splice(draggedIndex, 1);

  let newIndex = targetIndex;
  if (draggedIndex < targetIndex && !insertBefore) {
    newIndex = targetIndex;
  } else if (draggedIndex < targetIndex && insertBefore) {
    newIndex = targetIndex - 1;
  } else if (draggedIndex > targetIndex && insertBefore) {
    newIndex = targetIndex;
  } else if (draggedIndex > targetIndex && !insertBefore) {
    newIndex = targetIndex + 1;
  }

  sortChannels.splice(newIndex, 0, draggedChannel);

  // 重新渲染
  renderSortList();

  return false;
}

// 保存排序
async function saveSortOrder() {
  if (sortChannels.length === 0) {
    window.showNotification('没有需要保存的排序', 'warning');
    return;
  }

  // 计算新的优先级(从高到低,相差10)
  const updates = sortChannels.map((channel, index) => {
    const newPriority = (sortChannels.length - index) * 10;
    return {
      id: channel.id,
      priority: newPriority
    };
  });

  try {
    const result = await fetchDataWithAuth('/admin/channels/batch-priority', {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json'
      },
      body: JSON.stringify({ updates })
    });

    window.showSuccess('排序已保存');
    closeSortModal();
    if (typeof clearChannelsCache === 'function') clearChannelsCache();
    const currentType = (filters && filters.channelType) ? filters.channelType : 'all';
    if (typeof loadChannels === 'function') await loadChannels(currentType);
  } catch (error) {
    console.error('保存排序失败:', error);
    window.showError(error.message || '保存排序失败');
  }
}

// 初始化排序按钮事件
document.addEventListener('DOMContentLoaded', function() {
  const sortBtn = document.getElementById('btn_sort');
  if (sortBtn) {
    sortBtn.addEventListener('click', showSortModal);
  }

  // 点击模态框背景关闭
  const sortModal = document.getElementById('sortModal');
  if (sortModal) {
    sortModal.addEventListener('click', function(e) {
      if (e.target === sortModal) {
        closeSortModal();
      }
    });
  }
});
