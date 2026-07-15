(function () {
  'use strict';

  const STORAGE_KEY = 'ccload_model_test_chat_advanced_options';
  const DEFAULT_OPTIONS = Object.freeze({
    systemPrompt: '',
    temperature: null,
    topP: null,
    contextMessages: null,
    maxTokens: null
  });

  function normalizeFloat(value, min, max) {
    if (value === '' || value === null || value === undefined) return null;
    const num = Number(value);
    if (!Number.isFinite(num) || num < min || num > max) return null;
    return num;
  }

  function normalizeInteger(value, min, allowZero) {
    if (value === '' || value === null || value === undefined) return null;
    const num = Number(value);
    if (!Number.isFinite(num) || !Number.isInteger(num)) return null;
    if (num < min) return null;
    if (!allowZero && num === 0) return null;
    return num;
  }

  function normalizeOptions(source) {
    const safe = source && typeof source === 'object' ? source : {};
    return {
      systemPrompt: String(safe.systemPrompt || '').trim(),
      temperature: normalizeFloat(safe.temperature, 0, 2),
      topP: normalizeFloat(safe.topP, 0, 1),
      contextMessages: normalizeInteger(safe.contextMessages, 0, true),
      maxTokens: normalizeInteger(safe.maxTokens, 1, false)
    };
  }

  function loadOptions(storage) {
    try {
      const raw = storage.getItem(STORAGE_KEY);
      if (!raw) return { ...DEFAULT_OPTIONS };
      return normalizeOptions(JSON.parse(raw));
    } catch (_) {
      return { ...DEFAULT_OPTIONS };
    }
  }

  function saveOptions(storage, options) {
    const normalized = normalizeOptions(options);
    try {
      storage.setItem(STORAGE_KEY, JSON.stringify(normalized));
    } catch (_) { /* ignore */ }
    return normalized;
  }

  function cloneMessage(message) {
    if (!message || typeof message !== 'object') return message;
    try {
      // 仅转发 API 字段；thinking 等 UI 元数据留在本地持久化，不进上游请求体。
      const cloned = JSON.parse(JSON.stringify(message));
      return { role: cloned.role, content: cloned.content };
    } catch (_) {
      return { role: message.role, content: message.content };
    }
  }

  function limitMessages(messages, contextMessages) {
    const list = Array.isArray(messages) ? messages.map(cloneMessage) : [];
    const limit = normalizeInteger(contextMessages, 0, true);
    if (!limit || limit <= 0) return list;
    return list.slice(-limit);
  }

  function buildChatRequestPayload(basePayload, messages, options) {
    const normalized = normalizeOptions(options);
    const payload = { ...(basePayload && typeof basePayload === 'object' ? basePayload : {}) };
    payload.messages = limitMessages(messages, normalized.contextMessages);

    if (normalized.systemPrompt) {
      payload.system_prompt = normalized.systemPrompt;
    }
    if (normalized.temperature !== null) {
      payload.temperature = normalized.temperature;
    }
    if (normalized.topP !== null) {
      payload.top_p = normalized.topP;
    }
    if (normalized.maxTokens !== null) {
      payload.max_tokens = normalized.maxTokens;
    }
    return payload;
  }

  const api = {
    STORAGE_KEY,
    DEFAULT_OPTIONS,
    normalizeOptions,
    loadOptions,
    saveOptions,
    buildChatRequestPayload
  };

  if (typeof window !== 'undefined') {
    window.ModelTestAdvancedOptions = api;
  }

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
  }
})();
