(function initModelEntryParser(root, factory) {
  const api = factory();
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
  }
  if (root) {
    root.ModelEntryParser = api;
  }
})(typeof window !== 'undefined' ? window : globalThis, function createModelEntryParser() {
  function parseModelEntries(value) {
    const seen = new Set();
    const result = [];
    const entries = String(value || '')
      .split(/[,\n]+/)
      .map(item => item.trim())
      .filter(Boolean);

    for (const entry of entries) {
      const separatorIndex = entry.search(/[|｜]/);
      const model = (separatorIndex < 0 ? entry : entry.slice(0, separatorIndex)).trim();
      if (!model) continue;

      const key = model.toLowerCase();
      if (seen.has(key)) continue;

      const redirectModel = separatorIndex < 0
        ? ''
        : entry.slice(separatorIndex + 1).trim();
      seen.add(key);
      result.push({ model, redirect_model: redirectModel });
    }

    return result;
  }

  return { parseModelEntries };
});
