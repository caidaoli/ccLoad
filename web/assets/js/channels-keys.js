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
    tbody.appendChild(row);
  }

  if (visibleEnd < filteredIndices.length) {
    const bottomSpacer = document.createElement('tr');
    const bottomHeight = (filteredIndices.length - visibleEnd) * ROW_HEIGHT;
    bottomSpacer.innerHTML = `<td colspan="4" style="height: ${bottomHeight}px; padding: 0; border: none;"></td>`;
    tbody.appendChild(bottomSpacer);
  }
}

function createKeyRow(index) {
  const key = inlineKeyTableData[index];
  const row = document.createElement('tr');
  row.style.borderBottom = '1px solid var(--neutral-200)';
  row.style.height = VIRTUAL_SCROLL_CONFIG.ROW_HEIGHT + 'px';

  const keyCooldown = currentChannelKeyCooldowns.find(kc => kc.key_index === index);
  let cooldownHtml = '<span style="color: var(--success-600); font-size: 12px;">✓ 正常</span>';
  
  if (keyCooldown && keyCooldown.cooldown_remaining_ms > 0) {
    const cooldownText = humanizeMS(keyCooldown.cooldown_remaining_ms);
    cooldownHtml = `<span style="color: #dc2626; font-size: 12px; font-weight: 500; background: linear-gradient(135deg, #fee2e2 0%, #fecaca 100%); padding: 2px 8px; border-radius: 4px; border: 1px solid #fca5a5; white-space: nowrap;">⚠️ 冷却中·${cooldownText}</span>`;
  }

  const isSelected = selectedKeyIndices.has(index);

  row.innerHTML = `
    <td style="padding: 6px 10px;">
      <div style="display: flex; align-items: center; gap: 8px;">
        <input
          type="checkbox"
          class="key-checkbox"
          data-index="${index}"
          ${isSelected ? 'checked' : ''}
          onchange="toggleKeySelection(${index}, this.checked)"
          style="width: 16px; height: 16px; cursor: pointer; accent-color: var(--primary-500);"
        >
        <span style="color: var(--neutral-600); font-weight: 500; font-size: 13px;">${index + 1}</span>
      </div>
    </td>
    <td style="padding: 6px 10px;">
      <input
        type="${inlineKeyVisible ? 'text' : 'password'}"
        value="${escapeHtml(key)}"
        onchange="updateInlineKey(${index}, this.value)"
        class="inline-key-input"
        data-index="${index}"
        style="width: 100%; padding: 5px 8px; border: 1px solid var(--neutral-300); border-radius: 6px; font-family: 'Monaco', 'Menlo', 'Courier New', monospace; font-size: 13px; transition: all 0.2s;"
        onfocus="this.style.borderColor='var(--primary-500)'; this.style.boxShadow='0 0 0 3px rgba(59,130,246,0.1)'"
        onblur="this.style.borderColor='var(--neutral-300)'; this.style.boxShadow='none'"
      >
    </td>
    <td style="padding: 6px 10px;">
      ${cooldownHtml}
    </td>
    <td style="padding: 6px 10px; text-align: center;">
      <div style="display: flex; gap: 6px; justify-content: center;">
        <button
          type="button"
          onclick="testSingleKey(${index})"
          title="测试此Key"
          style="width: 28px; height: 28px; border-radius: 6px; border: 1px solid var(--neutral-200); background: white; color: var(--neutral-500); cursor: pointer; transition: all 0.2s; display: inline-flex; align-items: center; justify-content: center; padding: 0;"
          onmouseover="this.style.background='#eff6ff'; this.style.borderColor='#93c5fd'; this.style.color='#3b82f6'"
          onmouseout="this.style.background='white'; this.style.borderColor='var(--neutral-200)'; this.style.color='var(--neutral-500)'"
        >
          <svg width="12" height="12" viewBox="0 0 16 16" fill="none" xmlns="http://www.w3.org/2000/svg">
            <path d="M4 2L12 8L4 14V2Z" fill="currentColor"/>
          </svg>
        </button>
        <button
          type="button"
          onclick="deleteInlineKey(${index})"
          title="删除此Key"
          style="width: 28px; height: 28px; border-radius: 6px; border: 1px solid var(--neutral-200); background: white; color: var(--neutral-500); cursor: pointer; transition: all 0.2s; display: inline-flex; align-items: center; justify-content: center; padding: 0;"
          onmouseover="this.style.background='#fef2f2'; this.style.borderColor='#fca5a5'; this.style.color='#dc2626'"
          onmouseout="this.style.background='white'; this.style.borderColor='var(--neutral-200)'; this.style.color='var(--neutral-500)'"
        >
          <svg width="12" height="12" viewBox="0 0 14 14" fill="none" xmlns="http://www.w3.org/2000/svg">
            <path d="M5.5 2.5V1.5C5.5 1.22386 5.72386 1 6 1H8C8.27614 1 8.5 1.22386 8.5 1.5V2.5M2 3.5H12M3 3.5V11.5C3 12.0523 3.44772 12.5 4 12.5H10C10.5523 12.5 11 12.0523 11 11.5V3.5M5.5 6.5V9.5M8.5 6.5V9.5" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" stroke-linejoin="round"/>
          </svg>
        </button>
      </div>
    </td>
  `;

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
  const tableContainer = document.querySelector('#inlineKeyTableBody').closest('div[style*="max-height"]');
  if (tableContainer) {
    tableContainer.removeEventListener('scroll', handleVirtualScroll);
    tableContainer.addEventListener('scroll', handleVirtualScroll, { passive: true });
  }
}

function cleanupVirtualScroll() {
  const tableContainer = document.querySelector('#inlineKeyTableBody').closest('div[style*="max-height"]');
  if (tableContainer) {
    tableContainer.removeEventListener('scroll', handleVirtualScroll);
  }
  if (virtualScrollState.rafId) {
    cancelAnimationFrame(virtualScrollState.rafId);
    virtualScrollState.rafId = null;
  }
}

function renderInlineKeyTable() {
  const tbody = document.getElementById('inlineKeyTableBody');
  const keyCount = document.getElementById('inlineKeyCount');
  const virtualScrollHint = document.getElementById('virtualScrollHint');

  tbody.innerHTML = '';
  keyCount.textContent = inlineKeyTableData.length;

  const hiddenInput = document.getElementById('channelApiKey');
  hiddenInput.value = inlineKeyTableData.join(',');

  if (inlineKeyTableData.length === 0) {
    tbody.innerHTML = `
      <tr>
        <td colspan="4" style="padding: 30px; text-align: center; color: var(--neutral-500); font-size: 14px;">
          暂无API Key，点击"添加"或"导入"按钮添加
        </td>
      </tr>
    `;
    cleanupVirtualScroll();
    virtualScrollState.enabled = false;
    if (virtualScrollHint) virtualScrollHint.style.display = 'none';
    return;
  }

  const visibleIndices = getVisibleKeyIndices();

  if (visibleIndices.length === 0) {
    tbody.innerHTML = `
      <tr>
        <td colspan="4" style="padding: 30px; text-align: center; color: var(--neutral-500); font-size: 14px;">
          ${currentKeyStatusFilter === 'normal' ? '当前无正常状态的Key' : '当前无冷却中的Key'}
        </td>
      </tr>
    `;
    cleanupVirtualScroll();
    virtualScrollState.enabled = false;
    if (virtualScrollHint) virtualScrollHint.style.display = 'none';
    return;
  }

  virtualScrollState.enabled = true;
  if (!virtualScrollState.filteredIndices || 
      virtualScrollState.filteredIndices.length !== visibleIndices.length) {
    virtualScrollState.scrollTop = 0;
  }
  virtualScrollState.filteredIndices = visibleIndices;

  const { visibleStart, visibleEnd } = calculateVisibleRange(visibleIndices.length);
  virtualScrollState.visibleStart = visibleStart;
  virtualScrollState.visibleEnd = visibleEnd;

  renderVirtualRows(tbody, visibleStart, visibleEnd, visibleIndices);
  initVirtualScroll();

  if (virtualScrollHint) {
    const showHint = visibleIndices.length >= VIRTUAL_SCROLL_CONFIG.ENABLE_THRESHOLD;
    virtualScrollHint.style.display = showHint ? 'inline' : 'none';
  }

  updateSelectAllCheckbox();
  updateBatchDeleteButton();
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
  inlineKeyTableData[index] = value.trim();
  
  const hiddenInput = document.getElementById('channelApiKey');
  if (hiddenInput) {
    hiddenInput.value = inlineKeyTableData.join(',');
  }
}

async function testSingleKey(keyIndex) {
  if (!editingChannelId) {
    alert('无法获取渠道ID');
    return;
  }

  const modelsInput = document.getElementById('channelModels');
  if (!modelsInput || !modelsInput.value.trim()) {
    alert('请先配置支持的模型列表');
    return;
  }

  const models = modelsInput.value.split(',').map(m => m.trim()).filter(m => m);
  if (models.length === 0) {
    alert('模型列表为空，请先配置支持的模型');
    return;
  }

  const firstModel = models[0];
  const apiKey = inlineKeyTableData[keyIndex];

  if (!apiKey || !apiKey.trim()) {
    alert('API Key为空，无法测试');
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

  const testButton = event.target.closest('button');
  const originalHTML = testButton.innerHTML;
  testButton.disabled = true;
  testButton.innerHTML = '<span style="font-size: 10px;">⏳</span>';

  try {
    const res = await fetchWithAuth(`/admin/channels/${editingChannelId}/test`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        model: firstModel,
        max_tokens: 512,
        stream: true,
        content: 'test',
        channel_type: channelType,
        key_index: keyIndex
      })
    });

    if (!res.ok) {
      throw new Error('HTTP ' + res.status);
    }

    const result = await res.json();
    const testResult = result.data || result;

    await refreshKeyCooldownStatus();

    if (testResult.success) {
      showToast(`✅ Key #${keyIndex + 1} 测试成功`, 'success');
    } else {
      const errorMsg = testResult.error || '测试失败';
      showToast(`❌ Key #${keyIndex + 1} 测试失败: ${errorMsg}`, 'error');
    }
  } catch (e) {
    console.error('测试失败', e);
    showToast(`❌ Key #${keyIndex + 1} 测试请求失败: ${e.message}`, 'error');
  } finally {
    testButton.disabled = false;
    testButton.innerHTML = originalHTML;
  }
}

async function refreshKeyCooldownStatus() {
  if (!editingChannelId) return;

  try {
    const res = await fetchWithAuth(`/admin/channels/${editingChannelId}/keys`);
    if (res.ok) {
      const data = await res.json();
      const apiKeys = (data.success ? data.data : data) || [];

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

      const tableContainer = document.querySelector('#inlineKeyTableBody').closest('div[style*="max-height"]');
      const savedScrollTop = tableContainer ? tableContainer.scrollTop : 0;

      renderInlineKeyTable();

      if (tableContainer && virtualScrollState.enabled) {
        setTimeout(() => {
          tableContainer.scrollTop = savedScrollTop;
          virtualScrollState.scrollTop = savedScrollTop;
          handleVirtualScroll({ target: tableContainer });
        }, 0);
      }
    }
  } catch (e) {
    console.error('刷新冷却状态失败', e);
  }
}

function deleteInlineKey(index) {
  if (inlineKeyTableData.length === 1) {
    alert('至少需要保留一个API Key');
    return;
  }

  if (confirm(`确定要删除第 ${index + 1} 个Key吗？`)) {
    const tableContainer = document.querySelector('#inlineKeyTableBody').closest('div[style*="max-height"]');
    const scrollTop = tableContainer ? tableContainer.scrollTop : 0;

    inlineKeyTableData.splice(index, 1);

    currentChannelKeyCooldowns = currentChannelKeyCooldowns
      .filter(kc => kc.key_index !== index)
      .map(kc => kc.key_index > index ? { ...kc, key_index: kc.key_index - 1 } : kc);

    selectedKeyIndices.clear();
    updateBatchDeleteButton();

    renderInlineKeyTable();

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
  const count = selectedKeyIndices.size;

  if (count > 0) {
    btn.disabled = false;
    btn.textContent = `删除选中 (${count})`;
    btn.style.cursor = 'pointer';
    btn.style.background = 'linear-gradient(135deg, #fef2f2 0%, #fecaca 100%)';
    btn.style.borderColor = '#fca5a5';
    btn.style.color = '#dc2626';
    btn.style.fontWeight = '600';
  } else {
    btn.disabled = true;
    btn.textContent = '删除选中';
    btn.style.cursor = 'not-allowed';
    btn.style.background = 'white';
    btn.style.borderColor = 'var(--neutral-300)';
    btn.style.color = 'var(--neutral-500)';
    btn.style.fontWeight = '500';
  }
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
    alert('至少需要保留一个API Key');
    return;
  }

  if (!confirm(`确定要删除选中的 ${count} 个Key吗？`)) {
    return;
  }

  const tableContainer = document.querySelector('#inlineKeyTableBody').closest('div[style*="max-height"]');
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

function shouldShowKey(index) {
  if (currentKeyStatusFilter === 'all') {
    return true;
  }

  const keyCooldown = currentChannelKeyCooldowns.find(kc => kc.key_index === index);
  const isCoolingDown = keyCooldown && keyCooldown.cooldown_remaining_ms > 0;

  if (currentKeyStatusFilter === 'normal') {
    return !isCoolingDown;
  }
  if (currentKeyStatusFilter === 'cooldown') {
    return isCoolingDown;
  }

  return true;
}

function openInlineKeyImport() {
  openKeyImportModal();
}

function confirmInlineKeyImport() {
  const textarea = document.getElementById('keyImportTextarea');
  const input = textarea.value.trim();

  if (!input) {
    alert('请输入至少一个API Key');
    return;
  }

  const newKeys = parseKeys(input);

  if (newKeys.length === 0) {
    alert('未能解析到有效的API Key，请检查格式');
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

  showToast(`成功导入 ${addedCount} 个新Key${newKeys.length - addedCount > 0 ? `，${newKeys.length - addedCount} 个重复已忽略` : ''}`);
}

function openKeyImportModal() {
  document.getElementById('keyImportTextarea').value = '';
  document.getElementById('keyImportPreview').style.display = 'none';
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
    const preview = document.getElementById('keyImportPreview');
    const countSpan = document.getElementById('keyImportCount');

    if (input) {
      const keys = parseKeys(input);
      if (keys.length > 0) {
        countSpan.textContent = keys.length;
        preview.style.display = 'block';
      } else {
        preview.style.display = 'none';
      }
    } else {
      preview.style.display = 'none';
    }
  });
}
