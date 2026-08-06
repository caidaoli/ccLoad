(function (root, factory) {
  const api = factory();
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  if (root) root.ModelTestChatSession = api;
})(typeof window !== 'undefined' ? window : null, function () {
  function normalizeSessionID(value) {
    return typeof value === 'string' ? value.trim() : '';
  }

  function createSessionState(generateID) {
    if (typeof generateID !== 'function') throw new TypeError('generateID must be a function');
    let sessionID = '';

    function generate() {
      const next = normalizeSessionID(generateID());
      if (!next) throw new Error('generateID returned an empty session id');
      return next;
    }

    function restore(persistedID) {
      const persisted = normalizeSessionID(persistedID);
      if (persisted) sessionID = persisted;
      if (!sessionID) sessionID = generate();
      return sessionID;
    }

    function current() {
      return restore('');
    }

    function rotate() {
      sessionID = generate();
      return sessionID;
    }

    return { restore, current, rotate };
  }

  return { createSessionState };
});
