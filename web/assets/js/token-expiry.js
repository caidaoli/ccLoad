(function (root, factory) {
  const api = factory();
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  if (root) root.TokenExpiry = api;
})(typeof window !== 'undefined' ? window : globalThis, function () {
  function pad(value) {
    return String(value).padStart(2, '0');
  }

  function formatDateTimeLocal(value) {
    const date = value instanceof Date ? value : new Date(value);
    if (Number.isNaN(date.getTime())) return '';

    return [
      date.getFullYear(),
      pad(date.getMonth() + 1),
      pad(date.getDate())
    ].join('-') + 'T' + [
      pad(date.getHours()),
      pad(date.getMinutes())
    ].join(':');
  }

  function buildUpdatePayload(initial, current, expiresAt) {
    const unchanged = initial.type === current.type &&
      (current.type !== 'custom' || initial.value === current.value);

    return unchanged ? {} : { expires_at: expiresAt };
  }

  return { formatDateTimeLocal, buildUpdatePayload };
});
