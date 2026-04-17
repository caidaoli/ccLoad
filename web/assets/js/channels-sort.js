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
    window.showError(window.t('channels.loadChannelsFailed'));
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
    container.innerHTML = `<p class="sort-list-empty">${window.t('channels.noChannelsForSort')}</p>`;
    return;
  }

  sortChannels.forEach((channel, index) => {
    const item = createSortItem(channel, index);
    container.appendChild(item);
  });

  // 添加拖拽事件监听
  attachDragListeners();

  // Translate dynamically rendered elements
  if (window.i18n && window.i18n.translatePage) {
    window.i18n.translatePage();
  }
}

// 创建排序卡片
function createSortItem(channel, index) {
  const template = document.getElementById('tpl-sort-item');
  if (!template) return document.createElement('div');

  // 状态徽章
  let statusBadge = '';
  if (!channel.enabled) {
    statusBadge = `<span class="sort-item-status-badge sort-item-status-badge--disabled">${window.t('channels.statusDisabled')}</span>`;
  } else if (channel.cooldown_until && new Date(channel.cooldown_until) > new Date()) {
    statusBadge = `<span class="sort-item-status-badge sort-item-status-badge--cooldown">${window.t('channels.cooldownStatus')}</span>`;
  } else {
    statusBadge = `<span class="sort-item-status-badge sort-item-status-badge--normal">${window.t('channels.statusNormal')}</span>`;
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

// 添加拖拽事件监听：采用 dragover 实时 DOM 重排，避免 drop 命中率低的问题
function attachDragListeners() {
  const container = document.getElementById('sortListContainer');
  if (!container) return;

  container.querySelectorAll('.sort-item').forEach(item => {
    item.addEventListener('dragstart', handleDragStart);
    item.addEventListener('dragend', handleDragEnd);
  });

  // 容器级 dragover：无论释放在卡片还是间隙，都能捕获
  container.addEventListener('dragover', handleContainerDragOver);
}

// 拖拽开始
function handleDragStart(e) {
  draggedItem = this;
  this.classList.add('is-dragging');
  e.dataTransfer.effectAllowed = 'move';
  // Firefox 要求必须 setData 才会触发后续拖拽事件
  try { e.dataTransfer.setData('text/plain', this.dataset.channelId || ''); } catch (_) { /* ignore */ }
}

// 拖拽结束：从当前 DOM 顺序同步回 sortChannels，然后重渲染刷新序号
function handleDragEnd() {
  this.classList.remove('is-dragging');

  const container = document.getElementById('sortListContainer');
  if (container) {
    const byId = new Map(sortChannels.map(c => [String(c.id), c]));
    const newOrder = [];
    container.querySelectorAll('.sort-item').forEach(el => {
      const ch = byId.get(el.dataset.channelId);
      if (ch) newOrder.push(ch);
    });
    if (newOrder.length === sortChannels.length) {
      sortChannels = newOrder;
    }
  }

  draggedItem = null;
  renderSortList();
}

// 容器级 dragover：按鼠标 Y 坐标实时插入到最近的兄弟节点前后
function handleContainerDragOver(e) {
  e.preventDefault();
  if (!draggedItem) return;
  e.dataTransfer.dropEffect = 'move';

  const container = e.currentTarget;
  const afterElement = getDragAfterElement(container, e.clientY);

  if (afterElement == null) {
    if (container.lastElementChild !== draggedItem) {
      container.appendChild(draggedItem);
    }
  } else if (afterElement !== draggedItem && afterElement !== draggedItem.nextElementSibling) {
    container.insertBefore(draggedItem, afterElement);
  }
}

// 找到鼠标 Y 坐标上方最接近的 sort-item，作为插入锚点
function getDragAfterElement(container, y) {
  const siblings = container.querySelectorAll('.sort-item:not(.is-dragging)');
  let closest = null;
  let closestOffset = Number.NEGATIVE_INFINITY;
  siblings.forEach(child => {
    const box = child.getBoundingClientRect();
    const offset = y - box.top - box.height / 2;
    if (offset < 0 && offset > closestOffset) {
      closestOffset = offset;
      closest = child;
    }
  });
  return closest;
}

// 保存排序
async function saveSortOrder() {
  if (sortChannels.length === 0) {
    window.showNotification(window.t('channels.sortNoChanges'), 'warning');
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

    window.showSuccess(window.t('channels.sortSaveSuccess'));
    closeSortModal();
    if (typeof clearChannelsCache === 'function') clearChannelsCache();
    const currentType = (filters && filters.channelType) ? filters.channelType : 'all';
    if (typeof loadChannels === 'function') await loadChannels(currentType);
  } catch (error) {
    console.error('Save sort order failed:', error);
    window.showError(error.message || window.t('channels.sortSaveFailed'));
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
