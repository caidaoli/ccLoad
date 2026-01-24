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
      throw new Error(errorText || window.t('channels.import.exportHttpFailed', { status: res.status }));
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

    if (window.showSuccess) window.showSuccess(window.t('channels.msg.exportSuccess'));
  } catch (err) {
    console.error('Export CSV failed', err);
    if (window.showError) window.showError(err.message || window.t('channels.msg.exportFailed'));
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
    const resp = await fetchAPIWithAuth('/admin/channels/import', {
      method: 'POST',
      body: formData
    });

    const summary = resp.data;
    if (!resp.success) {
      throw new Error(resp.error || window.t('channels.msg.importFailed'));
    }
    if (summary) {
      let msg = window.t('channels.import.summary', {
        created: summary.created || 0,
        updated: summary.updated || 0,
        skipped: summary.skipped || 0
      });

      if (window.showSuccess) window.showSuccess(msg);

      if (summary.errors && summary.errors.length) {
        const preview = summary.errors.slice(0, 3).join('；');
        const extra = summary.errors.length > 3 ? window.t('channels.import.moreErrors', { count: summary.errors.length }) : '';
        if (window.showError) window.showError(window.t('channels.import.partialFailed', { preview, extra }));
      }
    } else if (window.showSuccess) {
      window.showSuccess(window.t('channels.msg.importSuccess'));
    }

    clearChannelsCache();
    await loadChannels(filters.channelType);
  } catch (err) {
    console.error('Import CSV failed', err);
    if (window.showError) window.showError(err.message || window.t('channels.msg.importFailed'));
  } finally {
    if (importBtn) importBtn.disabled = false;
    input.value = '';
  }
}
