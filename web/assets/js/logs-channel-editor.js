(function () {
  const CHANNEL_MODAL_IDS = [
    'channelModal',
    'quickAddChannelModal',
    'commonModelsModal',
    'keyModelScopeModal',
    'keyImportModal',
    'keyExportModal',
    'modelImportModal',
    'customRulesModal',
    'testModal',
    'upstreamDetailModal'
  ];

  const CHANNEL_TEMPLATE_IDS = [
    'tpl-key-row',
    'tpl-key-empty',
    'tpl-cooldown-badge',
    'tpl-key-normal-status',
    'tpl-key-actions',
    'tpl-url-row',
    'tpl-url-empty',
    'tpl-redirect-row',
    'tpl-redirect-empty',
    'tpl-test-result-header',
    'tpl-response-section',
    'tpl-batch-fail-item'
  ];

  const CHANNEL_EDITOR_SCRIPTS = [
    '/web/assets/js/channels-state.js',
    '/web/assets/js/channels-codex-auth.js',
    '/web/assets/js/channels-keys.js',
    '/web/assets/js/channels-urls.js',
    '/web/assets/js/channels-custom-rules.js',
    '/web/assets/js/channels-cooldown-detection.js',
    '/web/assets/js/model-entry-parser.js',
    '/web/assets/js/channels-modals.js',
    '/web/assets/js/channels-management.js',
    '/web/assets/js/channels-test.js',
    '/web/assets/js/upstream-detail-modal.js'
  ];

  const loadedScriptPromises = new Map();
  let channelEditorReadyPromise = null;
  let escapeHandlerBound = false;
  let localeHandlerBound = false;

  function getAssetVersion() {
    const scripts = Array.from(document.scripts);
    const candidates = [
      '/web/assets/js/logs-channel-editor.js',
      '/web/assets/js/logs.js'
    ];

    for (const candidate of candidates) {
      const matched = scripts.find((script) => {
        try {
          return script.src && new URL(script.src, window.location.origin).pathname === candidate;
        } catch (_) {
          return false;
        }
      });

      if (!matched) continue;

      try {
        return new URL(matched.src, window.location.origin).searchParams.get('v') || '';
      } catch (_) {
        return '';
      }
    }

    return '';
  }

  function getVersionedAssetURL(path) {
    const version = getAssetVersion();
    if (!version) return path;
    const separator = path.includes('?') ? '&' : '?';
    return `${path}${separator}v=${encodeURIComponent(version)}`;
  }

  function normalizeAssetPath(path) {
    try {
      return new URL(path, window.location.origin).pathname;
    } catch (_) {
      return path;
    }
  }

  function loadScriptOnce(path) {
    const normalizedPath = normalizeAssetPath(path);
    if (loadedScriptPromises.has(normalizedPath)) {
      return loadedScriptPromises.get(normalizedPath);
    }

    const existingScript = Array.from(document.scripts).find((script) => {
      return script.src && normalizeAssetPath(script.src) === normalizedPath;
    });
    if (existingScript) {
      const resolved = Promise.resolve();
      loadedScriptPromises.set(normalizedPath, resolved);
      return resolved;
    }

    const script = document.createElement('script');
    const promise = new Promise((resolve, reject) => {
      script.src = getVersionedAssetURL(path);
      script.defer = true;
      script.onload = () => resolve();
      script.onerror = () => reject(new Error(`Failed to load script: ${path}`));
      document.head.appendChild(script);
    }).catch((error) => {
      loadedScriptPromises.delete(normalizedPath);
      script.remove();
      throw error;
    });

    loadedScriptPromises.set(normalizedPath, promise);
    return promise;
  }

  async function fetchChannelsDocument() {
    const response = await fetch(getVersionedAssetURL('/web/channels.html'), {
      credentials: 'same-origin'
    });
    if (!response.ok) {
      throw new Error(`Failed to fetch channels editor markup: HTTP ${response.status}`);
    }

    const html = await response.text();
    return new DOMParser().parseFromString(html, 'text/html');
  }

  function appendNodeByID(sourceDocument, id) {
    if (document.getElementById(id)) return;

    const sourceNode = sourceDocument.getElementById(id);
    if (!sourceNode) {
      throw new Error(`Missing required channels markup: ${id}`);
    }

    document.body.appendChild(document.importNode(sourceNode, true));
  }

  async function ensureChannelEditorMarkup() {
    const hasMarkup = CHANNEL_MODAL_IDS.every((id) => document.getElementById(id))
      && CHANNEL_TEMPLATE_IDS.every((id) => document.getElementById(id));
    if (hasMarkup) return;

    const sourceDocument = await fetchChannelsDocument();
    CHANNEL_MODAL_IDS.forEach((id) => appendNodeByID(sourceDocument, id));
    CHANNEL_TEMPLATE_IDS.forEach((id) => appendNodeByID(sourceDocument, id));
  }

  function bindEscapeHandlerOnce() {
    if (escapeHandlerBound) return;
    escapeHandlerBound = true;

    document.addEventListener('keydown', (event) => {
      if (event.key !== 'Escape') return;

      const customRulesModal = document.getElementById('customRulesModal');
      const modelImportModal = document.getElementById('modelImportModal');
      const keyImportModal = document.getElementById('keyImportModal');
      const keyExportModal = document.getElementById('keyExportModal');
      const testModal = document.getElementById('testModal');
      const channelModal = document.getElementById('channelModal');

      if (customRulesModal && customRulesModal.classList.contains('show')) {
        closeCustomRulesModal();
      } else if (modelImportModal && modelImportModal.classList.contains('show')) {
        closeModelImportModal();
      } else if (keyImportModal && keyImportModal.classList.contains('show')) {
        closeKeyImportModal();
      } else if (keyExportModal && keyExportModal.classList.contains('show')) {
        closeKeyExportModal();
      } else if (testModal && testModal.classList.contains('show')) {
        closeTestModal();
      } else if (channelModal && channelModal.classList.contains('show')) {
        closeModal();
      }
    });
  }

  function bindLocaleHandlerOnce() {
    if (localeHandlerBound || !window.i18n || typeof window.i18n.onLocaleChange !== 'function') return;
    localeHandlerBound = true;

    window.i18n.onLocaleChange(() => {
      if (document.getElementById('channelModal') && typeof window.i18n.translatePage === 'function') {
        window.i18n.translatePage();
      }
    });
  }

  function installChannelModalHooks() {
    if (window.ChannelModalHooks) return;
    window.ChannelModalHooks = {
      afterUpdate: async () => {
        if (typeof load === 'function') {
          await load(true);
        }
      }
    };
  }

  async function initializeChannelEditorFeatures() {
    installChannelModalHooks();

    if (typeof initChannelEditorActions === 'function') {
      initChannelEditorActions();
    }
    if (typeof setupOAuthActions === 'function') {
      setupOAuthActions();
    }
    if (typeof initChannelFormDirtyTracking === 'function') {
      initChannelFormDirtyTracking();
    }
    if (typeof setupKeyImportPreview === 'function') {
      setupKeyImportPreview();
    }
    if (typeof setupModelImportPreview === 'function') {
      setupModelImportPreview();
    }
    if (window.i18n && typeof window.i18n.translatePage === 'function') {
      window.i18n.translatePage();
    }

    bindEscapeHandlerOnce();
    bindLocaleHandlerOnce();
    if (typeof loadDefaultTestContent === 'function') {
      await loadDefaultTestContent();
    }
  }

  async function ensureLogChannelEditorReady() {
    if (channelEditorReadyPromise) {
      return channelEditorReadyPromise;
    }

    channelEditorReadyPromise = (async () => {
      await ensureChannelEditorMarkup();

      for (const scriptPath of CHANNEL_EDITOR_SCRIPTS) {
        await loadScriptOnce(scriptPath);
      }

      await initializeChannelEditorFeatures();
    })();

    try {
      await channelEditorReadyPromise;
    } catch (error) {
      channelEditorReadyPromise = null;
      throw error;
    }
  }

  async function openLogChannelEditor(channelId) {
    const numericID = Number(channelId);
    if (!Number.isFinite(numericID) || numericID <= 0) {
      return;
    }

    try {
      await ensureLogChannelEditorReady();
      await editChannel(Math.trunc(numericID));
    } catch (error) {
      console.error('Failed to open log channel editor', error);
      if (window.showError) {
        window.showError(window.t ? window.t('channels.loadChannelsFailed') : '无法加载渠道编辑器');
      }
    }
  }

  window.openLogChannelEditor = openLogChannelEditor;
})();
