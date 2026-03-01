// URL 表格管理（与 API Key 表格一致的交互模式）
function parseChannelURLs(input) {
  if (!input || !input.trim()) return [];

  return input
    .split('\n')
    .map(url => url.trim())
    .filter(Boolean);
}

function getValidInlineURLs() {
  return inlineURLTableData
    .map(url => (url || '').trim())
    .filter(Boolean);
}

function syncInlineURLInput() {
  const hiddenInput = document.getElementById('channelUrl');
  if (!hiddenInput) return;
  hiddenInput.value = getValidInlineURLs().join('\n');
}

function updateInlineURLCount() {
  const countEl = document.getElementById('inlineUrlCount');
  if (!countEl) return;
  countEl.textContent = inlineURLTableData.length;
}

function updateURLBatchDeleteButton() {
  const btn = document.getElementById('batchDeleteUrlsBtn');
  if (!btn) return;

  const count = selectedURLIndices.size;
  btn.disabled = count === 0;
  btn.style.opacity = count === 0 ? '0.5' : '1';

  const textEl = btn.querySelector('span');
  if (textEl) {
    textEl.textContent = count > 0
      ? window.t('channels.deleteSelectedCount', { count })
      : window.t('channels.deleteSelected');
  }
}

function updateSelectAllURLsCheckbox() {
  const checkbox = document.getElementById('selectAllURLs');
  if (!checkbox) return;

  const total = inlineURLTableData.length;
  const selected = selectedURLIndices.size;

  if (total === 0 || selected === 0) {
    checkbox.checked = false;
    checkbox.indeterminate = false;
    return;
  }

  if (selected === total) {
    checkbox.checked = true;
    checkbox.indeterminate = false;
    return;
  }

  checkbox.checked = false;
  checkbox.indeterminate = true;
}

function createURLRow(index) {
  const tplData = {
    index: index,
    displayIndex: index + 1,
    url: inlineURLTableData[index] || ''
  };

  const row = TemplateEngine.render('tpl-url-row', tplData);
  if (!row) return null;

  const checkbox = row.querySelector('.url-checkbox');
  if (checkbox && selectedURLIndices.has(index)) {
    checkbox.checked = true;
  }

  return row;
}

function renderInlineURLTable() {
  const tbody = document.getElementById('inlineUrlTableBody');
  if (!tbody) return;

  if (inlineURLTableData.length === 0) {
    inlineURLTableData = [''];
  }

  updateInlineURLCount();
  syncInlineURLInput();

  tbody.innerHTML = '';
  inlineURLTableData.forEach((_, index) => {
    const row = createURLRow(index);
    if (row) tbody.appendChild(row);
  });

  updateSelectAllURLsCheckbox();
  updateURLBatchDeleteButton();
}

function setInlineURLTableData(rawURL) {
  inlineURLTableData = parseChannelURLs(rawURL);
  if (inlineURLTableData.length === 0) {
    inlineURLTableData = [''];
  }
  selectedURLIndices.clear();
  renderInlineURLTable();
}

function addInlineURL() {
  const newIndex = inlineURLTableData.length;
  inlineURLTableData.push('');
  renderInlineURLTable();
  markChannelFormDirty();

  setTimeout(() => {
    const input = document.querySelector(`.inline-url-input[data-index="${newIndex}"]`);
    if (input) input.focus();
  }, 0);
}

function updateInlineURL(index, value) {
  const nextValue = (value || '').trim();
  if (inlineURLTableData[index] === nextValue) return;

  inlineURLTableData[index] = nextValue;
  syncInlineURLInput();
  markChannelFormDirty();
}

function toggleURLSelection(index, checked) {
  if (checked) {
    selectedURLIndices.add(index);
  } else {
    selectedURLIndices.delete(index);
  }

  updateSelectAllURLsCheckbox();
  updateURLBatchDeleteButton();
}

function toggleSelectAllURLs(checked) {
  if (checked) {
    inlineURLTableData.forEach((_, index) => selectedURLIndices.add(index));
  } else {
    selectedURLIndices.clear();
  }

  renderInlineURLTable();
}

function deleteInlineURL(index) {
  if (index < 0 || index >= inlineURLTableData.length) return;

  if (inlineURLTableData.length === 1) {
    inlineURLTableData[0] = '';
    selectedURLIndices.clear();
    renderInlineURLTable();
    markChannelFormDirty();
    return;
  }

  inlineURLTableData.splice(index, 1);

  const nextSelected = new Set();
  selectedURLIndices.forEach(i => {
    if (i < index) {
      nextSelected.add(i);
    } else if (i > index) {
      nextSelected.add(i - 1);
    }
  });
  selectedURLIndices = nextSelected;

  renderInlineURLTable();
  markChannelFormDirty();
}

function batchDeleteSelectedURLs() {
  const count = selectedURLIndices.size;
  if (count === 0) return;

  if (!confirm(window.t('channels.confirmBatchDeleteUrls', { count }))) {
    return;
  }

  const indices = Array.from(selectedURLIndices).sort((a, b) => b - a);
  indices.forEach(index => {
    inlineURLTableData.splice(index, 1);
  });

  if (inlineURLTableData.length === 0) {
    inlineURLTableData = [''];
  }

  selectedURLIndices.clear();
  renderInlineURLTable();
  markChannelFormDirty();
}

async function testInlineURL(index, buttonElement) {
  if (!editingChannelId) {
    alert(window.t('channels.cannotGetChannelId'));
    return;
  }

  const models = redirectTableData
    .map(r => r.model)
    .filter(m => m && m.trim());
  if (models.length === 0) {
    alert(window.t('channels.configModelsFirst'));
    return;
  }

  const firstModel = models[0];
  const url = (inlineURLTableData[index] || '').trim();
  if (!url) {
    alert(window.t('channels.fillApiUrlFirst'));
    return;
  }

  const firstKey = (inlineKeyTableData[0] || '').trim();
  if (!firstKey) {
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

  if (!buttonElement) return;
  const originalHTML = buttonElement.innerHTML;
  buttonElement.disabled = true;
  buttonElement.innerHTML = '<span style="font-size: 10px;">⏳</span>';

  try {
    const testResult = await fetchDataWithAuth(`/admin/channels/${editingChannelId}/test`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        model: firstModel,
        stream: true,
        content: 'test',
        channel_type: channelType,
        key_index: 0
      })
    });

    await refreshKeyCooldownStatus();

    if (testResult.success) {
      window.showNotification(window.t('channels.urlTestSuccess', { index: index + 1 }), 'success');
    } else {
      const errorMsg = testResult.error || window.t('common.failed');
      window.showNotification(window.t('channels.urlTestFailed', { index: index + 1, error: errorMsg }), 'error');
    }
  } catch (error) {
    console.error('URL test failed', error);
    window.showNotification(window.t('channels.urlTestRequestFailed', { index: index + 1, error: error.message }), 'error');
  } finally {
    buttonElement.disabled = false;
    buttonElement.innerHTML = originalHTML;
  }
}
