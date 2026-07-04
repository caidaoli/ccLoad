(function(root) {
  function getQueryKeys(field) {
    if (Array.isArray(field.queryKeys) && field.queryKeys.length > 0) {
      return field.queryKeys;
    }
    if (typeof field.queryKey === 'string' && field.queryKey) {
      return [field.queryKey];
    }
    return [];
  }

  function getPrimaryQueryKey(field, value, values) {
    if (typeof field.paramKey === 'function') {
      const queryKey = field.paramKey(value, values);
      if (typeof queryKey === 'string' && queryKey) {
        return queryKey;
      }
    }
    if (typeof field.paramKey === 'string' && field.paramKey) {
      return field.paramKey;
    }
    return getQueryKeys(field)[0] || '';
  }

  function load(storageKey, storage = root.localStorage) {
    try {
      const saved = storage.getItem(storageKey);
      if (saved) {
        return JSON.parse(saved);
      }
    } catch (_) {}
    return null;
  }

  function save(storageKey, filters, storage = root.localStorage) {
    try {
      storage.setItem(storageKey, JSON.stringify(filters));
    } catch (_) {}
  }

  function restore(options = {}) {
    const search = options.search || '';
    const savedFilters = options.savedFilters || null;
    const fields = Array.isArray(options.fields) ? options.fields : [];
    const urlParams = new URLSearchParams(search);
    const hasURLParams = urlParams.toString().length > 0;
    const values = {};

    fields.forEach((field) => {
      let value = null;
      getQueryKeys(field).some((queryKey) => {
        const queryValue = urlParams.get(queryKey);
        if (queryValue !== null) {
          value = queryValue;
          return true;
        }
        return false;
      });

      if (value === null && !hasURLParams && savedFilters && Object.prototype.hasOwnProperty.call(savedFilters, field.key)) {
        value = savedFilters[field.key];
      }

      if ((value === null || value === undefined || value === '') && Object.prototype.hasOwnProperty.call(field, 'defaultValue')) {
        value = field.defaultValue;
      }

      values[field.key] = value;
    });

    return values;
  }

  function buildParams(values, fields) {
    const params = new URLSearchParams();

    (Array.isArray(fields) ? fields : []).forEach((field) => {
      const value = values ? values[field.key] : undefined;
      const include = typeof field.includeInQuery === 'function'
        ? field.includeInQuery(value, values)
        : value !== undefined && value !== null && value !== '';

      if (!include) {
        return;
      }

      const queryKey = getPrimaryQueryKey(field, value, values);
      if (!queryKey) {
        return;
      }

      const serializedValue = typeof field.serialize === 'function'
        ? field.serialize(value, values)
        : value;
      params.set(queryKey, String(serializedValue));
    });

    return params;
  }

  function mergeParams(search, values, fields) {
    const params = new URLSearchParams(search || '');

    (Array.isArray(fields) ? fields : []).forEach((field) => {
      getQueryKeys(field).forEach((queryKey) => {
        params.delete(queryKey);
      });
    });

    const nextParams = buildParams(values, fields);
    nextParams.forEach((value, key) => {
      params.set(key, value);
    });

    return params;
  }

  function buildURL(options = {}) {
    const pathname = options.pathname || (root.location && root.location.pathname) || '';
    const params = options.preserveExistingParams
      ? mergeParams(options.search, options.values, options.fields)
      : buildParams(options.values, options.fields);
    const nextSearch = params.toString();
    return nextSearch ? `?${nextSearch}` : pathname;
  }

  function writeHistory(options = {}) {
    const historyMethod = options.historyMethod === 'replaceState' ? 'replaceState' : 'pushState';
    const historyObject = options.history || root.history;
    const url = buildURL(options);

    if (historyObject && typeof historyObject[historyMethod] === 'function') {
      historyObject[historyMethod](null, '', url);
    }

    return url;
  }

  function buildRestoreSearch(search, savedFilters, fields) {
    const currentSearch = new URLSearchParams(search || '');
    if (currentSearch.toString() || !savedFilters) {
      return '';
    }

    return buildURL({
      pathname: '',
      values: savedFilters,
      fields
    });
  }

  const api = {
    load,
    save,
    restore,
    buildParams,
    mergeParams,
    buildRestoreSearch,
    buildURL,
    writeHistory
  };

  if (typeof window !== 'undefined') {
    window.FilterState = api;
  }

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
  }
})(typeof window !== 'undefined' ? window : globalThis);
