(function initChannelProtocolConfig(global) {
  const ALL_PROTOCOLS = Object.freeze(['anthropic', 'codex', 'openai', 'gemini']);
  const PROTOCOL_TRANSFORM_MODES = Object.freeze(['upstream', 'local']);
  const CODEX_TO_OPENAI_CAPABILITIES = Object.freeze([
    'function_tools',
    'hosted_web_search',
    'tool_search',
    'reasoning',
    'prompt_cache'
  ]);
  const SUPPORTED_TRANSFORMS_BY_CHANNEL_TYPE = Object.freeze(
    Object.fromEntries(
      ALL_PROTOCOLS.map((protocol) => [
        protocol,
        Object.freeze(ALL_PROTOCOLS.filter((candidate) => candidate !== protocol))
      ])
    )
  );

  function normalizeProtocol(value) {
    return String(value || '').trim().toLowerCase();
  }

  function normalizeProtocolTransformMode(value) {
    return String(value || '').trim().toLowerCase() === 'local' ? 'local' : 'upstream';
  }

  function getSupportedProtocolTransforms(channelType) {
    const baseType = normalizeProtocol(channelType) || 'anthropic';
    return [...(SUPPORTED_TRANSFORMS_BY_CHANNEL_TYPE[baseType] || [])];
  }

  function getProtocolTransformRenderOptions(channelType) {
    return [...ALL_PROTOCOLS];
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

  function shouldShowCodexToOpenAICapabilities(channelType, selectedValues, transformMode) {
    return normalizeProtocol(channelType) === 'openai'
      && normalizeProtocolTransformMode(transformMode) === 'local'
      && normalizeProtocolTransformsForChannel(channelType, selectedValues).includes('codex');
  }

  function normalizeCodexToOpenAICapabilities(rawCapabilities) {
    const raw = rawCapabilities && rawCapabilities.codex;
    return Object.fromEntries(CODEX_TO_OPENAI_CAPABILITIES.map((capability) => [
      capability,
      !raw || typeof raw[capability] !== 'boolean' ? true : raw[capability]
    ]));
  }

  global.ChannelProtocolConfig = Object.freeze({
    ALL_PROTOCOLS: [...ALL_PROTOCOLS],
    PROTOCOL_TRANSFORM_MODES: [...PROTOCOL_TRANSFORM_MODES],
    CODEX_TO_OPENAI_CAPABILITIES: [...CODEX_TO_OPENAI_CAPABILITIES],
    SUPPORTED_TRANSFORMS_BY_CHANNEL_TYPE: Object.fromEntries(
      Object.entries(SUPPORTED_TRANSFORMS_BY_CHANNEL_TYPE).map(([key, values]) => [key, [...values]])
    ),
    normalizeProtocol,
    normalizeProtocolTransformMode,
    getSupportedProtocolTransforms,
    getProtocolTransformRenderOptions,
    normalizeProtocolTransformsForChannel,
    shouldShowCodexToOpenAICapabilities,
    normalizeCodexToOpenAICapabilities
  });
})(window);
