// 统一Key解析函数（DRY原则）
function parseKeys(input) {
  if (!input || !input.trim()) return [];

  const keys = input
    .split(/[,\n]/)
    .map(k => k.trim())
    .filter(k => k);

  return [...new Set(keys)];
}

function calculateVisibleRange(totalItems) {
  const { ROW_HEIGHT, BUFFER_SIZE, CONTAINER_HEIGHT } = VIRTUAL_SCROLL_CONFIG;
  const { scrollTop } = virtualScrollState;

  const visibleRowCount = Math.ceil(CONTAINER_HEIGHT / ROW_HEIGHT);
  const startIndex = Math.floor(scrollTop / ROW_HEIGHT);

  const visibleStart = Math.max(0, startIndex - BUFFER_SIZE);
  const visibleEnd = Math.min(
    totalItems,
    startIndex + visibleRowCount + BUFFER_SIZE
  );

  return { visibleStart, visibleEnd };
}

function renderVirtualRows(tbody, visibleStart, visibleEnd, filteredIndices) {
  const { ROW_HEIGHT } = VIRTUAL_SCROLL_CONFIG;

  tbody.innerHTML = '';

  if (visibleStart > 0) {
    const topSpacer = document.createElement('tr');
    topSpacer.innerHTML = `<td colspan="4" style="height: ${visibleStart * ROW_HEIGHT}px; padding: 0; border: none;"></td>`;
    tbody.appendChild(topSpacer);
  }

  for (let i = visibleStart; i < visibleEnd; i++) {
    const actualIndex = filteredIndices[i];
    const row = createKeyRow(actualIndex);
    if (row) tbody.appendChild(row);
  }

  if (visibleEnd < filteredIndices.length) {
    const bottomSpacer = document.createElement('tr');
    const bottomHeight = (filteredIndices.length - visibleEnd) * ROW_HEIGHT;
    bottomSpacer.innerHTML = `<td colspan="4" style="height: ${bottomHeight}px; padding: 0; border: none;"></td>`;
    tbody.appendChild(bottomSpacer);
  }
}

/**
 * 构建Key行的冷却状态HTML
 * @param {number} index - Key索引
 * @returns {string} 冷却状态HTML
 */
function buildCooldownHtml(index) {
  const keyCooldown = currentChannelKeyCooldowns.find(kc => kc.key_index === index);
  if (keyCooldown && keyCooldown.cooldown_remaining_ms > 0) {
    const cooldownText = humanizeMS(keyCooldown.cooldown_remaining_ms);
    const tpl = document.getElementById('tpl-cooldown-badge');
    return tpl ? tpl.innerHTML.replaceAll('{{text}}', cooldownText) : window.t('channels.cooldownBadge', { time: cooldownText });
  }
  const normalTpl = document.getElementById('tpl-key-normal-status');
  return normalTpl ? normalTpl.innerHTML : `<span style="color: var(--success-600); font-size: 12px;">✓ ${window.t('channels.statusNormal')}</span>`;
}

/**
 * 构建Key行的操作按钮HTML
 * @param {number} index - Key索引
 * @returns {string} 操作按钮HTML
 */
function buildActionsHtml(index) {
  const tpl = document.getElementById('tpl-key-actions');
  if (tpl) {
    return tpl.innerHTML.replace(/\{\{index\}\}/g, String(index));
  }
  // 降级：无模板时返回简单按钮
  return `<button type="button" data-action="test" data-index="${index}">${window.t('common.test')}</button>
          <button type="button" data-action="delete" data-index="${index}">${window.t('common.delete')}</button>`;
}

/**
 * 使用模板引擎创建Key行元素
 * @param {number} index - Key在数据数组中的索引
 * @returns {HTMLElement} 表格行元素
 */
function createKeyRow(index) {
  const key = inlineKeyTableData[index];
  const isSelected = selectedKeyIndices.has(index);

  // 准备模板数据
  const rowData = {
    index: index,
    displayIndex: index + 1,
    key: key || '',
    inputType: inlineKeyVisible ? 'text' : 'password',
    cooldownHtml: buildCooldownHtml(index),
    actionsHtml: buildActionsHtml(index)
  };

  // 使用模板引擎渲染
  const row = TemplateEngine.render('tpl-key-row', rowData);
  if (!row) return null;

  // 设置选中状态
  const checkbox = row.querySelector('.key-checkbox');
  if (checkbox && isSelected) {
    checkbox.checked = true;
  }

  return row;
}

function handleVirtualScroll(event) {
  const container = event.target;
  virtualScrollState.scrollTop = container.scrollTop;

  if (virtualScrollState.rafId) {
    cancelAnimationFrame(virtualScrollState.rafId);
  }

  virtualScrollState.rafId = requestAnimationFrame(() => {
    const { visibleStart, visibleEnd } = calculateVisibleRange(virtualScrollState.filteredIndices.length);

    if (visibleStart !== virtualScrollState.visibleStart ||
      visibleEnd !== virtualScrollState.visibleEnd) {
      virtualScrollState.visibleStart = visibleStart;
      virtualScrollState.visibleEnd = visibleEnd;

      const tbody = document.getElementById('inlineKeyTableBody');
      renderVirtualRows(tbody, visibleStart, visibleEnd, virtualScrollState.filteredIndices);
    }
  });
}

function initVirtualScroll() {
  const tableContainer = document.querySelector('#inlineKeyTableBody').closest('.inline-table-container');
  if (tableContainer) {
    tableContainer.removeEventListener('scroll', handleVirtualScroll);
    tableContainer.addEventListener('scroll', handleVirtualScroll, { passive: true });
  }
}

function cleanupVirtualScroll() {
  const tableContainer = document.querySelector('#inlineKeyTableBody').closest('.inline-table-container');
  if (tableContainer) {
    tableContainer.removeEventListener('scroll', handleVirtualScroll);
  }
  if (virtualScrollState.rafId) {
    cancelAnimationFrame(virtualScrollState.rafId);
    virtualScrollState.rafId = null;
  }
}

/**
 * 初始化Key表格事件委托 (替代inline onclick)
 */
function initKeyTableEventDelegation() {
  const tbody = document.getElementById('inlineKeyTableBody');
  if (!tbody || tbody.dataset.delegated) return;

  tbody.dataset.delegated = 'true';
  let dragSrcIndex = null;

  // Drag and drop listeners
  tbody.addEventListener('dragstart', (e) => {
    // Prevent dragging when interacting with inputs or buttons
    if (['INPUT', 'BUTTON', 'A'].includes(e.target.tagName)) return;

    const row = e.target.closest('tr');
    if (row && row.classList.contains('draggable-key-row')) {
      dragSrcIndex = parseInt(row.dataset.index);
      row.classList.add('dragging');
      e.dataTransfer.effectAllowed = 'move';
      e.dataTransfer.setData('text/plain', dragSrcIndex);

      // Improve visual feedback
      // e.dataTransfer.setDragImage(row, 0, 0); // Optional
    }
  });

  tbody.addEventListener('dragend', (e) => {
    const row = e.target.closest('tr');
    if (row) row.classList.remove('dragging');
    tbody.querySelectorAll('.draggable-key-row.drag-over').forEach(r => r.classList.remove('drag-over'));
    dragSrcIndex = null;
  });

  tbody.addEventListener('dragover', (e) => {
    e.preventDefault(); // Necessary to allow dropping
    const row = e.target.closest('tr');

    // Clear other drag-overs
    tbody.querySelectorAll('.draggable-key-row.drag-over').forEach(r => {
      if (r !== row) r.classList.remove('drag-over');
    });

    if (row && row.classList.contains('draggable-key-row')) {
      const targetIndex = parseInt(row.dataset.index);
      if (targetIndex !== dragSrcIndex) {
        row.classList.add('drag-over');
      }
    }
  });

  tbody.addEventListener('drop', (e) => {
    e.stopPropagation();
    e.preventDefault();

    const targetRow = e.target.closest('tr');
    if (!targetRow || !targetRow.classList.contains('draggable-key-row')) return;

    const targetIndex = parseInt(targetRow.dataset.index);

    if (dragSrcIndex !== null && dragSrcIndex !== targetIndex) {
      // Perform Swap
      const movedKey = inlineKeyTableData[dragSrcIndex];

      inlineKeyTableData.splice(dragSrcIndex, 1);
      inlineKeyTableData.splice(targetIndex, 0, movedKey);

      // Update Cooldowns: Key Indices Shift
      if (currentChannelKeyCooldowns && currentChannelKeyCooldowns.length > 0) {
        currentChannelKeyCooldowns.forEach(kc => {
          if (kc.key_index === dragSrcIndex) {
            kc.key_index = targetIndex;
          } else if (dragSrcIndex < targetIndex) {
            // Moved down: Items between src and target shift UP (-1)
            if (kc.key_index > dragSrcIndex && kc.key_index <= targetIndex) {
              kc.key_index -= 1;
            }
          } else {
            // Moved up: Items between target and src shift DOWN (+1)
            if (kc.key_index >= targetIndex && kc.key_index < dragSrcIndex) {
              kc.key_index += 1;
            }
          }
        });
      }

      selectedKeyIndices.clear();
      renderInlineKeyTable();

      // 标记表单有未保存的更改
      markChannelFormDirty();

      // Update hidden input
      const hiddenInput = document.getElementById('channelApiKey');
      if (hiddenInput) {
        hiddenInput.value = inlineKeyTableData.join(',');
      }
    }
  });

  // 事件委托：处理所有按钮和输入事件
  tbody.addEventListener('click', (e) => {
    // 处理操作按钮点击
    const actionBtn = e.target.closest('.key-action-btn');
    if (actionBtn) {
      const action = actionBtn.dataset.action;
      const index = parseInt(actionBtn.dataset.index);
      if (action === 'test') testSingleKey(index, actionBtn);
      else if (action === 'copy') copyKeyToClipboard(index);
      else if (action === 'delete') deleteInlineKey(index);
      return;
    }

    // 处理复选框点击
    const checkbox = e.target.closest('.key-checkbox');
    if (checkbox) {
      const index = parseInt(checkbox.dataset.index);
      toggleKeySelection(index, checkbox.checked);
    }
  });

  // 处理输入框变更
  tbody.addEventListener('change', (e) => {
    const input = e.target.closest('.inline-key-input');
    if (input) {
      const index = parseInt(input.dataset.index);
      updateInlineKey(index, input.value);
    }
  });

  // 处理输入框焦点样式
  tbody.addEventListener('focusin', (e) => {
    const input = e.target.closest('.inline-key-input');
    if (input) {
      input.style.borderColor = 'var(--primary-500)';
      input.style.boxShadow = '0 0 0 3px rgba(59,130,246,0.1)';
      // Ensure drag doesn't interfere with typing
      input.closest('tr').setAttribute('draggable', 'false');
    }
  });

  tbody.addEventListener('focusout', (e) => {
    const input = e.target.closest('.inline-key-input');
    if (input) {
      input.style.borderColor = 'var(--neutral-300)';
      input.style.boxShadow = 'none';
      input.closest('tr').setAttribute('draggable', 'true');
    }
  });

  // 处理按钮悬停样式
  tbody.addEventListener('mouseover', (e) => {
    const btn = e.target.closest('.key-action-btn');
    if (btn) {
      const action = btn.dataset.action;
      if (action === 'test') {
        btn.style.background = '#eff6ff';
        btn.style.borderColor = '#93c5fd';
        btn.style.color = '#3b82f6';
      } else if (action === 'copy') {
        btn.style.background = '#f0fdf4';
        btn.style.borderColor = '#86efac';
        btn.style.color = '#16a34a';
      } else if (action === 'delete') {
        btn.style.background = '#fef2f2';
        btn.style.borderColor = '#fca5a5';
        btn.style.color = '#dc2626';
      }
    }
  });

  tbody.addEventListener('mouseout', (e) => {
    const btn = e.target.closest('.key-action-btn');
    if (btn) {
      btn.style.background = 'white';
      btn.style.borderColor = 'var(--neutral-200)';
      btn.style.color = 'var(--neutral-500)';
    }
  });
}

function renderInlineKeyTable() {
  const tbody = document.getElementById('inlineKeyTableBody');
  const keyCount = document.getElementById('inlineKeyCount');
  const virtualScrollHint = document.getElementById('virtualScrollHint');

  tbody.innerHTML = '';
  keyCount.textContent = inlineKeyTableData.length;

  const hiddenInput = document.getElementById('channelApiKey');
  hiddenInput.value = inlineKeyTableData.join(',');

  // 初始化事件委托
  initKeyTableEventDelegation();

  if (inlineKeyTableData.length === 0) {
    const emptyRow = TemplateEngine.render('tpl-key-empty', {
      message: window.t('channels.noApiKey')
    });
    if (emptyRow) tbody.appendChild(emptyRow);
    cleanupVirtualScroll();
    virtualScrollState.enabled = false;
    if (virtualScrollHint) virtualScrollHint.style.display = 'none';
    return;
  }

  const visibleIndices = getVisibleKeyIndices();

  if (visibleIndices.length === 0) {
    const filterMessage = currentKeyStatusFilter === 'normal'
      ? window.t('channels.noNormalKeys')
      : window.t('channels.noCooldownKeys');
    const emptyRow = TemplateEngine.render('tpl-key-empty', { message: filterMessage });
    if (emptyRow) tbody.appendChild(emptyRow);
    cleanupVirtualScroll();
    virtualScrollState.enabled = false;
    if (virtualScrollHint) virtualScrollHint.style.display = 'none';
    return;
  }

  virtualScrollState.enabled = true;
  const shouldResetScroll = !virtualScrollState.filteredIndices ||
    virtualScrollState.filteredIndices.length !== visibleIndices.length;
  if (shouldResetScroll) {
    virtualScrollState.scrollTop = 0;
  }
  virtualScrollState.filteredIndices = visibleIndices;

  const { visibleStart, visibleEnd } = calculateVisibleRange(visibleIndices.length);
  virtualScrollState.visibleStart = visibleStart;
  virtualScrollState.visibleEnd = visibleEnd;

  renderVirtualRows(tbody, visibleStart, visibleEnd, visibleIndices);
  initVirtualScroll();

  // 同步容器滚动位置
  if (shouldResetScroll) {
    const tableContainer = tbody.closest('.inline-table-container');
    if (tableContainer) {
      tableContainer.scrollTop = 0;
    }
  }

  if (virtualScrollHint) {
    const showHint = visibleIndices.length >= VIRTUAL_SCROLL_CONFIG.ENABLE_THRESHOLD;
    virtualScrollHint.style.display = showHint ? 'inline' : 'none';
  }

  updateSelectAllCheckbox();
  updateBatchDeleteButton();

  // Translate dynamically rendered elements
  if (window.i18n && window.i18n.translatePage) {
    window.i18n.translatePage();
  }
}

function toggleInlineKeyVisibility() {
  inlineKeyVisible = !inlineKeyVisible;
  const eyeIcon = document.getElementById('inlineEyeIcon');
  const eyeOffIcon = document.getElementById('inlineEyeOffIcon');

  if (inlineKeyVisible) {
    eyeIcon.style.display = 'none';
    eyeOffIcon.style.display = 'block';
  } else {
    eyeIcon.style.display = 'block';
    eyeOffIcon.style.display = 'none';
  }

  renderInlineKeyTable();
}

function updateInlineKey(index, value) {
  const nextValue = value.trim();
  if (inlineKeyTableData[index] === nextValue) return;

  inlineKeyTableData[index] = nextValue;
  markChannelFormDirty();

  const hiddenInput = document.getElementById('channelApiKey');
  if (hiddenInput) {
    hiddenInput.value = inlineKeyTableData.join(',');
  }
}

async function testSingleKey(keyIndex, testButton) {
  if (!editingChannelId) {
    alert(window.t('channels.cannotGetChannelId'));
    return;
  }

  // 从 redirectTableData 获取模型列表（定义在 channels-state.js）
  const models = redirectTableData
    .map(r => r.model)
    .filter(m => m && m.trim());
  if (models.length === 0) {
    alert(window.t('channels.configModelsFirst'));
    return;
  }

  const firstModel = models[0];
  const apiKey = inlineKeyTableData[keyIndex];

  if (!apiKey || !apiKey.trim()) {
    alert(window.t('channels.emptyKeyCannotTest'));
    return;
  }

  const channelTypeRadios = document.querySelectorAll('input[name="channelType"]');
  let channelType = 'anthropic';
  for (const radio of channelTypeRadios) {
    if (radio.checked) {
      channelType = radio.value.toLowerCase();
      break;
    }
  }

  if (!testButton) return;
  const originalHTML = testButton.innerHTML;
  testButton.disabled = true;
  testButton.innerHTML = '<span style="font-size: 10px;">⏳</span>';

  try {
    const testResult = await fetchDataWithAuth(`/admin/channels/${editingChannelId}/test`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        model: firstModel,
        stream: true,
        content: 'test',
        channel_type: channelType,
        key_index: keyIndex
      })
    });

    await refreshKeyCooldownStatus();

    if (testResult.success) {
      window.showNotification(window.t('channels.testKeySuccess', { index: keyIndex + 1 }), 'success');
    } else {
      const errorMsg = testResult.error || window.t('common.failed');
      window.showNotification(window.t('channels.testKeyFailed', { index: keyIndex + 1, error: errorMsg }), 'error');
    }
  } catch (e) {
    console.error('Test failed', e);
    window.showNotification(window.t('channels.testRequestFailed', { index: keyIndex + 1, error: e.message }), 'error');
  } finally {
    testButton.disabled = false;
    testButton.innerHTML = originalHTML;
  }
}

async function refreshKeyCooldownStatus() {
  if (!editingChannelId) return;

  try {
    const apiKeys = (await fetchDataWithAuth(`/admin/channels/${editingChannelId}/keys`)) || [];

    inlineKeyTableData = apiKeys.map(k => k.api_key || k);
    if (inlineKeyTableData.length === 0) {
      inlineKeyTableData = [''];
    }

    const now = Date.now();
    currentChannelKeyCooldowns = apiKeys.map((apiKey, index) => {
      const cooldownUntilMs = (apiKey.cooldown_until || 0) * 1000;
      const remainingMs = Math.max(0, cooldownUntilMs - now);
      return {
        key_index: index,
        cooldown_remaining_ms: remainingMs
      };
    });

    const tableContainer = document.querySelector('#inlineKeyTableBody').closest('.inline-table-container');
    const savedScrollTop = tableContainer ? tableContainer.scrollTop : 0;

    renderInlineKeyTable();

    if (tableContainer && virtualScrollState.enabled) {
      setTimeout(() => {
        tableContainer.scrollTop = savedScrollTop;
        virtualScrollState.scrollTop = savedScrollTop;
        handleVirtualScroll({ target: tableContainer });
      }, 0);
    }
  } catch (e) {
    console.error('Refresh cooldown status failed', e);
  }
}

/**
 * 复制Key到剪贴板
 * @param {number} index - Key在数据数组中的索引
 */
function copyKeyToClipboard(index) {
  const keyText = inlineKeyTableData[index];
  if (!keyText) return;

  window.copyToClipboard(keyText).then(() => {
    showToast(window.t('channels.keyCopied'), 'success');
  }).catch(() => {
    showToast(window.t('channels.keyCopyFailed'), 'error');
  });
}

function deleteInlineKey(index) {
  if (inlineKeyTableData.length === 1) {
    alert(window.t('channels.keepOneKey'));
    return;
  }

  if (confirm(window.t('channels.confirmDeleteKey', { index: index + 1 }))) {
    const tableContainer = document.querySelector('#inlineKeyTableBody').closest('.inline-table-container');
    const scrollTop = tableContainer ? tableContainer.scrollTop : 0;

    inlineKeyTableData.splice(index, 1);

    currentChannelKeyCooldowns = currentChannelKeyCooldowns
      .filter(kc => kc.key_index !== index)
      .map(kc => kc.key_index > index ? { ...kc, key_index: kc.key_index - 1 } : kc);

    selectedKeyIndices.clear();
    updateBatchDeleteButton();

    renderInlineKeyTable();
    markChannelFormDirty();

    setTimeout(() => {
      if (tableContainer) {
        tableContainer.scrollTop = Math.min(scrollTop, tableContainer.scrollHeight - tableContainer.clientHeight);
      }
    }, 50);
  }
}

function toggleKeySelection(index, checked) {
  if (checked) {
    selectedKeyIndices.add(index);
  } else {
    selectedKeyIndices.delete(index);
  }
  updateBatchDeleteButton();
  updateSelectAllCheckbox();
}

function toggleSelectAllKeys(checked) {
  selectedKeyIndices.clear();

  if (checked) {
    const visibleIndices = getVisibleKeyIndices();
    visibleIndices.forEach(index => selectedKeyIndices.add(index));
  }

  updateBatchDeleteButton();
  renderInlineKeyTable();
}

function updateBatchDeleteButton() {
  const btn = document.getElementById('batchDeleteKeysBtn');
  if (!btn) return;

  const count = selectedKeyIndices.size;
  const textSpan = btn.querySelector('span');

  if (count > 0) {
    btn.disabled = false;
    if (textSpan) textSpan.textContent = window.t('channels.deleteSelectedCount', { count });
    btn.style.cursor = 'pointer';
    btn.style.opacity = '1';
    btn.style.background = 'linear-gradient(135deg, #fef2f2 0%, #fecaca 100%)';
    btn.style.borderColor = '#fca5a5';
    btn.style.color = '#dc2626';
  } else {
    btn.disabled = true;
    if (textSpan) textSpan.textContent = window.t('channels.deleteSelected');
    btn.style.cursor = 'not-allowed';
    btn.style.opacity = '0.5';
    btn.style.background = '';
    btn.style.borderColor = '';
    btn.style.color = '';
  }

  // 同步更新导出按钮状态
  updateExportButton(count);
}

function updateSelectAllCheckbox() {
  const checkbox = document.getElementById('selectAllKeys');
  if (!checkbox) return;

  const visibleIndices = getVisibleKeyIndices();
  const allSelected = visibleIndices.length > 0 &&
    visibleIndices.every(index => selectedKeyIndices.has(index));

  checkbox.checked = allSelected;
  checkbox.indeterminate = !allSelected &&
    visibleIndices.some(index => selectedKeyIndices.has(index));
}

function batchDeleteSelectedKeys() {
  const count = selectedKeyIndices.size;
  if (count === 0) return;

  if (inlineKeyTableData.length - count < 1) {
    alert(window.t('channels.keepOneKey'));
    return;
  }

  if (!confirm(window.t('channels.confirmBatchDeleteKeys', { count }))) {
    return;
  }

  const tableContainer = document.querySelector('#inlineKeyTableBody').closest('.inline-table-container');
  const scrollTop = tableContainer ? tableContainer.scrollTop : 0;

  const indicesToDelete = Array.from(selectedKeyIndices).sort((a, b) => b - a);

  indicesToDelete.forEach(index => {
    inlineKeyTableData.splice(index, 1);

    currentChannelKeyCooldowns = currentChannelKeyCooldowns
      .filter(kc => kc.key_index !== index)
      .map(kc => kc.key_index > index ? { ...kc, key_index: kc.key_index - 1 } : kc);
  });

  selectedKeyIndices.clear();
  updateBatchDeleteButton();

  renderInlineKeyTable();
  markChannelFormDirty();

  setTimeout(() => {
    if (tableContainer) {
      tableContainer.scrollTop = Math.min(scrollTop, tableContainer.scrollHeight - tableContainer.clientHeight);
    }
  }, 50);
}

function filterKeysByStatus(status) {
  currentKeyStatusFilter = status;
  renderInlineKeyTable();
  updateSelectAllCheckbox();
}

function getVisibleKeyIndices() {
  if (currentKeyStatusFilter === 'all') {
    return inlineKeyTableData.map((_, index) => index);
  }

  return inlineKeyTableData
    .map((_, index) => {
      const keyCooldown = currentChannelKeyCooldowns.find(kc => kc.key_index === index);
      const isCoolingDown = keyCooldown && keyCooldown.cooldown_remaining_ms > 0;

      if (currentKeyStatusFilter === 'normal' && !isCoolingDown) {
        return index;
      }
      if (currentKeyStatusFilter === 'cooldown' && isCoolingDown) {
        return index;
      }
      return null;
    })
    .filter(index => index !== null);
}

function confirmInlineKeyImport() {
  const textarea = document.getElementById('keyImportTextarea');
  const input = textarea.value.trim();

  if (!input) {
    alert(window.t('channels.enterAtLeastOneKey'));
    return;
  }

  const newKeys = parseKeys(input);

  if (newKeys.length === 0) {
    alert(window.t('channels.noValidKeyParsed'));
    return;
  }

  const existingKeys = new Set(inlineKeyTableData);
  let addedCount = 0;

  newKeys.forEach(key => {
    if (!existingKeys.has(key)) {
      inlineKeyTableData.push(key);
      existingKeys.add(key);
      addedCount++;
    }
  });

  closeKeyImportModal();
  renderInlineKeyTable();
  if (addedCount > 0) markChannelFormDirty();

  const duplicates = newKeys.length - addedCount;
  const msg = duplicates > 0
    ? window.t('channels.keyImportDuplicates', { added: addedCount, duplicates })
    : window.t('channels.keyImportSuccess', { added: addedCount });
  window.showNotification(msg, 'success');
}

function openKeyImportModal() {
  document.getElementById('keyImportTextarea').value = '';
  document.getElementById('keyImportPreviewContent').style.display = 'none';
  document.getElementById('keyImportModal').classList.add('show');
  setTimeout(() => document.getElementById('keyImportTextarea').focus(), 100);
}

function closeKeyImportModal() {
  document.getElementById('keyImportModal').classList.remove('show');
}

function setupKeyImportPreview() {
  const textarea = document.getElementById('keyImportTextarea');
  if (!textarea) return;

  textarea.addEventListener('input', () => {
    const input = textarea.value.trim();
    const previewContent = document.getElementById('keyImportPreviewContent');
    const countSpan = document.getElementById('keyImportCount');

    if (input) {
      const keys = parseKeys(input);
      if (keys.length > 0) {
        countSpan.textContent = keys.length;
        previewContent.style.display = 'block';
      } else {
        previewContent.style.display = 'none';
      }
    } else {
      previewContent.style.display = 'none';
    }
  });
}

// ============================================================
// Key 导出功能
// ============================================================

/**
 * 更新导出按钮状态
 * @param {number} count - 选中的 Key 数量
 */
function updateExportButton(count) {
  const btn = document.getElementById('exportKeysBtn');
  if (!btn) return;

  if (count > 0) {
    btn.disabled = false;
    btn.style.opacity = '1';
    btn.style.cursor = 'pointer';
  } else {
    btn.disabled = true;
    btn.style.opacity = '0.5';
    btn.style.cursor = 'not-allowed';
  }
}

/**
 * 打开导出对话框
 */
function openKeyExportModal() {
  if (selectedKeyIndices.size === 0) return;
  document.getElementById('keyExportModal').classList.add('show');
  updateExportPreview();
}

/**
 * 关闭导出对话框
 */
function closeKeyExportModal() {
  document.getElementById('keyExportModal').classList.remove('show');
}

/**
 * 更新预览内容
 */
function updateExportPreview() {
  const separator = document.querySelector('input[name="exportSeparator"]:checked').value;
  const keys = getSelectedKeys();
  const text = separator === 'newline' ? keys.join('\n') : keys.join(',');
  document.getElementById('keyExportPreview').value = text;
}

/**
 * 获取选中的 Keys
 * @returns {string[]} 选中的 Key 数组
 */
function getSelectedKeys() {
  return Array.from(selectedKeyIndices)
    .sort((a, b) => a - b)
    .map(index => inlineKeyTableData[index])
    .filter(key => key); // 过滤掉空值
}

/**
 * 复制导出内容到剪贴板
 */
function copyExportKeys() {
  const text = document.getElementById('keyExportPreview').value;
  const count = selectedKeyIndices.size;

  window.copyToClipboard(text).then(() => {
    showToast(window.t('channels.keysCopied', { count }), 'success');
    closeKeyExportModal();
  }).catch(() => {
    showToast(window.t('channels.keyCopyFailed'), 'error');
  });
}

/**
 * 导出为文件下载
 */
function downloadExportKeys() {
  const text = document.getElementById('keyExportPreview').value;
  const blob = new Blob([text], { type: 'text/plain;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = 'api-keys.txt';
  document.body.appendChild(a);
  a.click();
  document.body.removeChild(a);
  URL.revokeObjectURL(url);
  closeKeyExportModal();
}
