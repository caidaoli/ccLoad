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

  function modelEntryParseError(code, index) {
    const error = new Error(code);
    error.code = code;
    if (Number.isInteger(index)) error.index = index;
    return error;
  }

  function parseJSONModelEntries(value) {
    let entries;
    try {
      entries = JSON.parse(String(value || ''));
    } catch (_) {
      throw modelEntryParseError('invalid_json');
    }

    if (!Array.isArray(entries)) {
      if (entries !== null && typeof entries === 'object' && Array.isArray(entries.data)) {
        entries = entries.data;
      } else {
        throw modelEntryParseError('array_required');
      }
    }

    const seen = new Set();
    const result = [];
    entries.forEach((item, index) => {
      const isString = typeof item === 'string';
      const isObject = item !== null && typeof item === 'object' && !Array.isArray(item);
      if (!isString && !isObject) {
        throw modelEntryParseError('invalid_entry', index);
      }

      const isGatewayEntry = isObject && (
        item.MODEL_SERIES_ID !== undefined || item.MODEL_ID !== undefined
      );
      if (
        isGatewayEntry &&
        (typeof item.MODEL_ID !== 'string' || !item.MODEL_ID.trim())
      ) {
        throw modelEntryParseError('gateway_model_required', index);
      }
      if (isObject && !isGatewayEntry && typeof item.model !== 'string') {
        throw modelEntryParseError('model_required', index);
      }
      if (isObject && !isGatewayEntry && item.redirect_model !== undefined && typeof item.redirect_model !== 'string') {
        throw modelEntryParseError('invalid_redirect_model', index);
      }
      if (isObject && !isGatewayEntry && item.disabled !== undefined && typeof item.disabled !== 'boolean') {
        throw modelEntryParseError('invalid_disabled', index);
      }

      const model = String(
        isString ? item : (isGatewayEntry ? item.MODEL_ID : item.model)
      ).trim();
      if (!model) {
        throw modelEntryParseError('model_required', index);
      }
      if (/\x00|[\r\n]/.test(model)) {
        throw modelEntryParseError('invalid_model', index);
      }

      const redirectModel = isObject && !isGatewayEntry
        ? String(item.redirect_model || '').trim()
        : '';
      if (/\x00|[\r\n]/.test(redirectModel)) {
        throw modelEntryParseError('invalid_redirect_model', index);
      }

      const key = model.toLowerCase();
      if (seen.has(key)) return;

      seen.add(key);
      result.push({
        model,
        redirect_model: redirectModel,
        disabled: isObject && !isGatewayEntry && item.disabled === true
      });
    });

    return result;
  }

  function normalizedModelCandidate(entry, options = {}) {
    const model = String(entry?.model || '').trim();
    if (!model) return null;

    const upstreamModel = String(entry?.redirect_model || '').trim() || model;
    let alias = model;
    let sourcePrefixed = false;
    if (options.strip_model_source_prefix === true) {
      const separator = alias.lastIndexOf('/');
      if (separator >= 0 && separator + 1 < alias.length) {
        alias = alias.slice(separator + 1);
        sourcePrefixed = true;
      }
    }
    if (options.lowercase_models === true) alias = alias.toLowerCase();

    return {
      entry: {
        ...entry,
        model: alias,
        redirect_model: upstreamModel === alias ? '' : upstreamModel
      },
      upstreamModel,
      sourcePrefixed,
      exactAliasMatch: upstreamModel === alias
    };
  }

  function preferNormalizedCandidate(candidate, current) {
    if (candidate.exactAliasMatch !== current.exactAliasMatch) {
      return candidate.exactAliasMatch;
    }
    if (candidate.sourcePrefixed !== current.sourcePrefixed) {
      return !candidate.sourcePrefixed;
    }
    return candidate.upstreamModel < current.upstreamModel;
  }

  function normalizeModelEntry(entry, options = {}) {
    return normalizedModelCandidate(entry, options)?.entry || null;
  }

  function normalizeModelEntries(entries, options = {}) {
    const seen = new Map();
    const candidates = [];
    const result = [];

    for (const entry of entries || []) {
      const candidate = normalizedModelCandidate(entry, options);
      if (!candidate) continue;

      const key = candidate.entry.model.toLowerCase();
      if (seen.has(key)) {
        const index = seen.get(key);
        if (preferNormalizedCandidate(candidate, candidates[index])) {
          candidates[index] = candidate;
          result[index] = candidate.entry;
        }
        continue;
      }

      seen.set(key, result.length);
      candidates.push(candidate);
      result.push(candidate.entry);
    }
    return result;
  }

  function serializeModelEntries(entries) {
    return (entries || [])
      .map(entry => {
        const model = String(entry?.model || '').trim();
        if (!model) return '';
        const redirectModel = String(entry?.redirect_model || '').trim() || model;
        return `${model}|${redirectModel}`;
      })
      .filter(Boolean)
      .join('\n');
  }

  return {
    normalizeModelEntries,
    normalizeModelEntry,
    parseJSONModelEntries,
    parseModelEntries,
    serializeModelEntries
  };
});
