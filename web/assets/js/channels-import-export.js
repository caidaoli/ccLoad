function setupImportExport() {
  const exportBtn = document.getElementById('exportCsvBtn');
  const importBtn = document.getElementById('importCsvBtn');
  const importInput = document.getElementById('importCsvInput');

  if (exportBtn) {
    exportBtn.addEventListener('click', () => exportChannelsCSV(exportBtn));
  }

  if (importBtn && importInput) {
    importBtn.addEventListener('click', () => {
      if (window.pauseBackgroundAnimation) window.pauseBackgroundAnimation();
      importInput.click();
    });

    importInput.addEventListener('change', (event) => {
      if (window.resumeBackgroundAnimation) window.resumeBackgroundAnimation();
      handleImportCSV(event, importBtn);
    });

    importInput.addEventListener('cancel', () => {
      if (window.resumeBackgroundAnimation) window.resumeBackgroundAnimation();
    });
  }
}

async function exportChannelsCSV(buttonEl) {
  try {
    if (buttonEl) buttonEl.disabled = true;
    const res = await fetchWithAuth('/admin/channels/export');
    if (!res.ok) {
      const errorText = await res.text();
      throw new Error(errorText || `导出失败 (HTTP ${res.status})`);
    }

    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const link = document.createElement('a');
    link.href = url;
    link.download = `channels-${formatTimestampForFilename()}.csv`;
    document.body.appendChild(link);
    link.click();
    document.body.removeChild(link);
    URL.revokeObjectURL(url);

    if (window.showSuccess) showSuccess('导出成功');
  } catch (err) {
    console.error('导出CSV失败', err);
    if (window.showError) showError(err.message || '导出失败');
  } finally {
    if (buttonEl) buttonEl.disabled = false;
  }
}

async function handleImportCSV(event, importBtn) {
  const input = event.target;
  if (!input.files || input.files.length === 0) {
    return;
  }

  const file = input.files[0];
  const formData = new FormData();
  formData.append('file', file);

  if (importBtn) importBtn.disabled = true;

  try {
    const res = await fetchWithAuth('/admin/channels/import', {
      method: 'POST',
      body: formData
    });

    const responseText = await res.text();
    let payload = null;
    if (responseText) {
      try {
        payload = JSON.parse(responseText);
      } catch (e) {
        payload = null;
      }
    }

    if (!res.ok) {
      const message = (payload && payload.error) || responseText || `导入失败 (HTTP ${res.status})`;
      throw new Error(message);
    }

    const summary = payload && payload.data ? payload.data : payload;
    if (summary) {
      let msg = `导入完成：新增 ${summary.created || 0}，更新 ${summary.updated || 0}，跳过 ${summary.skipped || 0}`;

      if (summary.redis_sync_enabled) {
        if (summary.redis_sync_success) {
          msg += `，已同步 ${summary.redis_synced_channels || 0} 个渠道到Redis`;
        } else {
          msg += '，Redis同步失败';
        }
      }

      if (window.showSuccess) showSuccess(msg);

      if (summary.errors && summary.errors.length) {
        const preview = summary.errors.slice(0, 3).join('；');
        const extra = summary.errors.length > 3 ? ` 等${summary.errors.length}条记录` : '';
        if (window.showError) showError(`部分记录导入失败：${preview}${extra}`);
      }

      if (summary.redis_sync_enabled && !summary.redis_sync_success && summary.redis_sync_error) {
        if (window.showError) showError(`Redis同步失败：${summary.redis_sync_error}`);
      }
    } else if (window.showSuccess) {
      showSuccess('导入完成');
    }

    clearChannelsCache();
    await loadChannels(filters.channelType);
  } catch (err) {
    console.error('导入CSV失败', err);
    if (window.showError) showError(err.message || '导入失败');
  } finally {
    if (importBtn) importBtn.disabled = false;
    input.value = '';
  }
}
