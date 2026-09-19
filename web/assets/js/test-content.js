(function (root, factory) {
  const api = factory();
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
    return;
  }
  root.TestContent = api;
})(typeof window !== 'undefined' ? window : globalThis, function () {
  function splitPipeSeparatedTestContents(value) {
    return [...new Set(String(value ?? '')
      .split('|')
      .map((content) => content.trim())
      .filter(Boolean))];
  }

  function firstPipeSeparatedTestContent(value, fallback = '') {
    return splitPipeSeparatedTestContents(value)[0] || String(fallback ?? '').trim();
  }

  return { splitPipeSeparatedTestContents, firstPipeSeparatedTestContent };
});
