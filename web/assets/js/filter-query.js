(function(window) {
  function getQueryKeys(field) {
    if (Array.isArray(field.queryKeys) && field.queryKeys.length > 0) {
      return field.queryKeys;
    }
    if (typeof field.queryKey === 'string' && field.queryKey) {
      return [field.queryKey];
    }
    return [];
  }

  function getRequestKey(field, value, values) {
    if (typeof field.requestKey === 'function') {
      const requestKey = field.requestKey(value, values);
      if (typeof requestKey === 'string' && requestKey) {
        return requestKey;
      }
    }
    if (typeof field.requestKey === 'string' && field.requestKey) {
      return field.requestKey;
    }
    return getQueryKeys(field)[0] || '';
  }

  function appendBaseParams(params, baseParams) {
    if (!baseParams || typeof baseParams !== 'object') {
      return;
    }

    Object.entries(baseParams).forEach(([key, value]) => {
      if (value === undefined || value === null || value === '') {
        return;
      }
      params.set(key, String(value));
    });
  }

  function buildRequestParams(values, fields, options = {}) {
    const params = new URLSearchParams();
    appendBaseParams(params, options.baseParams);

    (Array.isArray(fields) ? fields : []).forEach((field) => {
      const value = values ? values[field.key] : undefined;
      const include = typeof field.includeInRequest === 'function'
        ? field.includeInRequest(value, values)
        : value !== undefined && value !== null && value !== '';

      if (!include) {
        return;
      }

      const requestKey = getRequestKey(field, value, values);
      if (!requestKey) {
        return;
      }

      const serializedValue = typeof field.serializeForRequest === 'function'
        ? field.serializeForRequest(value, values)
        : value;
      params.set(requestKey, String(serializedValue));
    });

    return params;
  }

  window.FilterQuery = {
    buildRequestParams
  };
})(window);
