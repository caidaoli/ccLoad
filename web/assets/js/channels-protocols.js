(function initChannelProtocolConfig(global) {
  const ALL_PROTOCOLS = Object.freeze(['anthropic', 'codex', 'openai', 'gemini']);
  const SUPPORTED_TRANSFORMS_BY_CHANNEL_TYPE = Object.freeze({
    anthropic: Object.freeze(['codex', 'openai']),
    gemini: Object.freeze(['anthropic', 'codex', 'openai'])
  });

  function normalizeProtocol(value) {
    return String(value || '').trim().toLowerCase();
  }

  function getSupportedProtocolTransforms(channelType) {
    const baseType = normalizeProtocol(channelType) || 'anthropic';
    return [...(SUPPORTED_TRANSFORMS_BY_CHANNEL_TYPE[baseType] || [])];
  }

  function getProtocolTransformRenderOptions(channelType) {
    const baseType = normalizeProtocol(channelType) || 'anthropic';
    return [baseType, ...getSupportedProtocolTransforms(baseType)];
  }

  function normalizeProtocolTransformsForChannel(channelType, selectedValues) {
    const baseType = normalizeProtocol(channelType) || 'anthropic';
    const allowed = new Set(getSupportedProtocolTransforms(baseType));
    const selected = new Set();

    for (const raw of selectedValues || []) {
      const value = normalizeProtocol(raw);
      if (!value || value === baseType || !allowed.has(value)) continue;
      selected.add(value);
    }

    return getSupportedProtocolTransforms(baseType).filter((protocol) => selected.has(protocol));
  }

  global.ChannelProtocolConfig = Object.freeze({
    ALL_PROTOCOLS: [...ALL_PROTOCOLS],
    SUPPORTED_TRANSFORMS_BY_CHANNEL_TYPE: Object.fromEntries(
      Object.entries(SUPPORTED_TRANSFORMS_BY_CHANNEL_TYPE).map(([key, values]) => [key, [...values]])
    ),
    normalizeProtocol,
    getSupportedProtocolTransforms,
    getProtocolTransformRenderOptions,
    normalizeProtocolTransformsForChannel
  });
})(window);
