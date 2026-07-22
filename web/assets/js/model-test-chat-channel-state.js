(function (root, factory) {
  const api = factory();
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  if (root) root.ModelTestChatChannelState = api;
})(typeof window !== 'undefined' ? window : null, function () {
  function normalizeChannelId(value) {
    if (value === null || value === undefined || value === '') return '';
    return String(value);
  }

  function createSelection(storage, storageKey) {
    let preferredChannelId = '';
    try {
      preferredChannelId = normalizeChannelId(storage.getItem(storageKey));
    } catch (_) { /* ignore unavailable storage */ }

    function resolve(candidates) {
      const list = Array.isArray(candidates) ? candidates : [];
      return list.find(channel => normalizeChannelId(channel?.id) === preferredChannelId) || list[0] || null;
    }

    function select(channel) {
      preferredChannelId = normalizeChannelId(channel?.id);
      try {
        if (preferredChannelId) {
          storage.setItem(storageKey, preferredChannelId);
        } else {
          storage.removeItem(storageKey);
        }
      } catch (_) { /* ignore unavailable storage */ }
    }

    return { resolve, select };
  }

  return { createSelection };
});
