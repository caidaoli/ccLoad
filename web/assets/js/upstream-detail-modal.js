(function (root, factory) {
  'use strict';

  const api = factory(root || globalThis);
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
  }
})(typeof globalThis !== 'undefined' ? globalThis : this, function (root) {
  'use strict';

  let upstreamMergedVisible = false;
  let currentUpstreamDetailData = null;
  let upstreamMergedSourceBody = null;
  let upstreamMergedLoading = false;
  let upstreamWrapEnabled = true;
  let eventsBound = false;

  function text(key, fallback, params) {
    const fn = root.i18nText || root.t;
    if (typeof fn !== 'function') return fallback;
    try {
      return fn(key, fallback, params) || fallback;
    } catch (_) {
      return fallback;
    }
  }

  function tryFormatJSON(value) {
    if (value === null || value === undefined || value === '') return '';
    if (typeof value !== 'string') {
      try {
        return JSON.stringify(value, null, 2);
      } catch (_) {
        return String(value);
      }
    }
    try {
      return JSON.stringify(JSON.parse(value), null, 2);
    } catch (_) {
      return value;
    }
  }

  function normalizeHeaders(headers) {
    if (!headers) return null;
    if (typeof headers === 'string') {
      const trimmed = headers.trim();
      if (!trimmed) return null;
      try {
        return JSON.parse(trimmed);
      } catch (_) {
        return trimmed;
      }
    }
    return headers;
  }

  function formatHeaderLines(headers) {
    const normalized = normalizeHeaders(headers);
    if (!normalized) return '';
    if (typeof normalized === 'string') return normalized;
    if (typeof normalized !== 'object') return String(normalized);

    const safeHeaders = typeof root.maskSensitiveHeaders === 'function'
      ? root.maskSensitiveHeaders(normalized)
      : normalized;
    const lines = [];
    for (const [key, value] of Object.entries(safeHeaders)) {
      if (Array.isArray(value)) {
        value.forEach(item => lines.push(`${key}: ${item}`));
      } else {
        lines.push(`${key}: ${value}`);
      }
    }
    return lines.join('\n');
  }

  function composeRawRequest(data) {
    const parts = [];
    const method = data?.method || data?.requestMethod || data?.req_method || 'POST';
    const url = data?.url || data?.requestUrl || data?.req_url || '';
    if (url) parts.push(`${method} ${url}`);

    const headers = formatHeaderLines(data?.requestHeaders || data?.req_headers);
    if (headers) parts.push(headers);

    const body = tryFormatJSON(data?.requestBody ?? data?.req_body);
    if (body) {
      parts.push('');
      parts.push(body);
    }
    return parts.join('\n');
  }

  function composeRawResponse(data) {
    const parts = [];
    const statusCode = data?.statusCode ?? data?.resp_status;
    if (statusCode !== null && statusCode !== undefined && statusCode !== '') {
      parts.push('HTTP ' + statusCode);
    }

    const headers = formatHeaderLines(data?.responseHeaders || data?.resp_headers);
    if (headers) parts.push(headers);

    const body = tryFormatJSON(data?.responseBody ?? data?.resp_body);
    if (body) {
      parts.push('');
      parts.push(body);
    }
    return parts.join('\n');
  }

  function setCodeContent(id, content, mode) {
    if (typeof root.setHighlightedCodeContent === 'function') {
      root.setHighlightedCodeContent(id, content, mode);
      return;
    }

    const el = root.document?.getElementById(id);
    if (!el) return;
    el.textContent = String(content || '');
    el._rawText = String(content || '');
  }

  function renderMergedResponse(targetId, response) {
    if (root.MarkdownRenderer && typeof root.MarkdownRenderer.renderResponse === 'function') {
      root.MarkdownRenderer.renderResponse(targetId, response || { reasoning: '', content: '' });
      return;
    }

    const el = root.document?.getElementById(targetId);
    if (!el) return;
    const content = typeof response === 'string'
      ? response
      : String(response?.content || response?.text || '');
    el.textContent = content;
    el._rawText = content;
  }

  function updateUpstreamResponseActionButtons() {
    const document = root.document;
    const responseActive = !!document?.getElementById('upstreamTabResponse')?.classList.contains('active');
    const copyBtn = document?.querySelector('#upstreamDetailModal .upstream-copy-btn--tabs');
    if (copyBtn) {
      copyBtn.dataset.copyTarget = responseActive
        ? (upstreamMergedVisible ? 'upstreamRespMerged' : 'upstreamRespRaw')
        : 'upstreamReqRaw';
    }

    const mergeBtn = document?.getElementById('upstreamMergeBtn');
    if (mergeBtn) {
      mergeBtn.hidden = !responseActive;
    }
  }

  function updateUpstreamWrapButton() {
    const wrapBtn = root.document?.getElementById('upstreamWrapBtn');
    if (!wrapBtn) return;
    wrapBtn.classList.toggle('active', upstreamWrapEnabled);
    wrapBtn.setAttribute('aria-pressed', upstreamWrapEnabled ? 'true' : 'false');
    wrapBtn.dataset.i18n = upstreamWrapEnabled ? 'logs.debugWrap' : 'logs.debugNoWrap';
    wrapBtn.textContent = text(
      wrapBtn.dataset.i18n,
      upstreamWrapEnabled ? 'Wrap' : 'No wrap'
    );
  }

  function applyUpstreamWrapMode() {
    root.document?.querySelectorAll('#upstreamDetailModal .upstream-pre').forEach(pre => {
      pre.classList.toggle('upstream-pre--nowrap', !upstreamWrapEnabled);
    });
    updateUpstreamWrapButton();
  }

  function setUpstreamWrapEnabled(enabled) {
    upstreamWrapEnabled = !!enabled;
    applyUpstreamWrapMode();
  }

  function setUpstreamMergedVisible(visible) {
    upstreamMergedVisible = !!visible;

    const raw = root.document?.getElementById('upstreamRespRaw');
    const merged = root.document?.getElementById('upstreamRespMerged');
    if (raw) raw.hidden = upstreamMergedVisible;
    if (merged) merged.hidden = !upstreamMergedVisible;

    const mergeBtn = root.document?.getElementById('upstreamMergeBtn');
    if (mergeBtn) {
      const key = upstreamMergedVisible ? 'logs.debugRaw' : 'logs.debugMerge';
      mergeBtn.classList.toggle('active', upstreamMergedVisible);
      mergeBtn.setAttribute('aria-pressed', upstreamMergedVisible ? 'true' : 'false');
      mergeBtn.dataset.i18n = key;
      mergeBtn.textContent = text(key, upstreamMergedVisible ? 'Raw' : 'Merge');
    }

    updateUpstreamResponseActionButtons();

    if (upstreamMergedVisible) {
      void refreshUpstreamMergedResponse(currentUpstreamDetailData);
    }
  }

  function resetUpstreamMergedResponse() {
    upstreamMergedSourceBody = null;
    upstreamMergedLoading = false;
    renderMergedResponse('upstreamRespMerged', { reasoning: '', content: '' });
  }

  function show(data) {
    if (!data) return;
    const document = root.document;
    const modal = document?.getElementById('upstreamDetailModal');
    if (!modal) return;

    currentUpstreamDetailData = data;
    upstreamMergedSourceBody = null;
    upstreamMergedLoading = false;

    setCodeContent('upstreamReqRaw', composeRawRequest(data), 'request');
    setCodeContent('upstreamRespRaw', composeRawResponse(data), 'response');
    resetUpstreamMergedResponse();

    modal.querySelectorAll('.upstream-tab').forEach(tab => {
      tab.classList.toggle('active', tab.dataset.tab === 'request');
    });
    document.getElementById('upstreamTabRequest')?.classList.add('active');
    document.getElementById('upstreamTabResponse')?.classList.remove('active');
    setUpstreamMergedVisible(false);
    applyUpstreamWrapMode();
    updateUpstreamResponseActionButtons();

    modal.classList.add('show');
  }

  function close() {
    currentUpstreamDetailData = null;
    upstreamMergedSourceBody = null;
    upstreamMergedLoading = false;
    root.document?.getElementById('upstreamDetailModal')?.classList.remove('show');
  }

  async function refreshUpstreamMergedResponse(data) {
    if (!data || upstreamMergedLoading) return;
    if (!root.MergedResponseClient || typeof root.MergedResponseClient.mergeUpstreamResponse !== 'function') {
      renderMergedResponse('upstreamRespMerged', {
        reasoning: '',
        content: 'MergedResponseClient is unavailable',
      });
      return;
    }

    const sourceBody = String(data.responseBody ?? data.resp_body ?? '');
    if (upstreamMergedSourceBody === sourceBody) return;
    upstreamMergedLoading = true;
    renderMergedResponse('upstreamRespMerged', {
      reasoning: '',
      content: text('common.loading', 'Loading...'),
    });
    try {
      const merged = await root.MergedResponseClient.mergeUpstreamResponse(sourceBody);
      upstreamMergedSourceBody = sourceBody;
      renderMergedResponse('upstreamRespMerged', merged || { reasoning: '', content: '' });
    } catch (e) {
      renderMergedResponse('upstreamRespMerged', {
        reasoning: '',
        content: e?.message || 'Merge response failed',
      });
    } finally {
      upstreamMergedLoading = false;
    }
  }

  function copyText(textToCopy) {
    if (typeof root.copyToClipboard === 'function') {
      return root.copyToClipboard(textToCopy);
    }
    const clipboard = root.navigator && root.navigator.clipboard;
    if (clipboard && typeof clipboard.writeText === 'function') {
      return clipboard.writeText(textToCopy);
    }
    return Promise.reject(new Error('clipboard unavailable'));
  }

  function markCopied(button) {
    const originalText = button.textContent;
    button.textContent = '\u2713';
    button.classList.add('copied');
    root.setTimeout?.(() => {
      button.textContent = originalText;
      button.classList.remove('copied');
    }, 1500);
  }

  function bindEvents() {
    const document = root.document;
    if (!document || eventsBound || typeof document.addEventListener !== 'function') return;
    eventsBound = true;

    document.addEventListener('keydown', event => {
      if (event.key === 'Escape' && document.getElementById('upstreamDetailModal')?.classList.contains('show')) {
        event.preventDefault();
        event.stopImmediatePropagation();
        close();
      }
    });

    document.addEventListener('click', event => {
      const closeBtn = event.target.closest('#upstreamDetailModal [data-action="close-upstream-detail"]');
      if (closeBtn) {
        close();
        return;
      }

      const tab = event.target.closest('#upstreamDetailModal .upstream-tab');
      if (tab) {
        const target = tab.dataset.tab;
        document.querySelectorAll('#upstreamDetailModal .upstream-tab').forEach(item => {
          item.classList.toggle('active', item === tab);
        });
        document.getElementById('upstreamTabRequest')?.classList.toggle('active', target === 'request');
        document.getElementById('upstreamTabResponse')?.classList.toggle('active', target === 'response');
        updateUpstreamResponseActionButtons();
        return;
      }

      const mergeBtn = event.target.closest('#upstreamDetailModal [data-action="merge-upstream-response"]');
      if (mergeBtn) {
        setUpstreamMergedVisible(!upstreamMergedVisible);
        return;
      }

      const wrapBtn = event.target.closest('#upstreamDetailModal [data-action="toggle-upstream-wrap"]');
      if (wrapBtn) {
        setUpstreamWrapEnabled(!upstreamWrapEnabled);
        return;
      }

      const copyBtn = event.target.closest('#upstreamDetailModal .upstream-copy-btn');
      if (copyBtn) {
        const targetId = copyBtn.dataset.copyTarget;
        const pre = document.getElementById(targetId);
        if (!pre) return;
        const textToCopy = pre._rawText || pre.textContent || '';
        copyText(textToCopy).then(() => markCopied(copyBtn)).catch(() => {});
      }
    });
  }

  const api = {
    show,
    close,
    composeRawRequest,
    composeRawResponse,
    formatHeaderLines,
    tryFormatJSON,
    setUpstreamWrapEnabled,
  };

  if (root) {
    root.UpstreamDetailModal = api;
    root.showUpstreamDetailModal = show;
    root.closeUpstreamDetailModal = close;
    bindEvents();
  }

  return api;
});
