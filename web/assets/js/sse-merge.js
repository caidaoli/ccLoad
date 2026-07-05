/**
 * SSE Merge Utilities — shared streaming response merge logic.
 * Extracts and assembles text content from SSE/non-SSE LLM responses
 * across Anthropic, OpenAI, Gemini, and Codex protocols.
 *
 * Exposed as window.SSEMerge = {
 *   appendText, parsePayloads, createState, collectPayload, formatState, formatParts,
 *   formatDisplayParts, hasParts
 * }
 */
(function () {
  'use strict';

  function appendMergedText(bucket, value) {
    if (!bucket || value == null) return;
    if (Array.isArray(value)) {
      value.forEach(item => appendMergedText(bucket, item));
      return;
    }
    if (typeof value === 'object') {
      if (typeof value.text === 'string') {
        appendMergedText(bucket, value.text);
        return;
      }
      if (typeof value.content === 'string') {
        appendMergedText(bucket, value.content);
        return;
      }
      try {
        bucket.push(JSON.stringify(value));
      } catch {
        // ignore values that cannot be rendered
      }
      return;
    }
    const text = String(value);
    if (text) bucket.push(text);
  }

  function codeFence(language, value) {
    const text = String(value ?? '');
    let fence = '```';
    (text.match(/`{3,}/g) || []).forEach(match => {
      if (match.length >= fence.length) fence = '`'.repeat(match.length + 1);
    });
    return `${fence}${language || ''}\n${text}\n${fence}`;
  }

  function parseToolArguments(value) {
    if (value == null) return null;
    if (typeof value === 'object') return value;
    if (typeof value !== 'string') return value;
    try {
      return JSON.parse(value);
    } catch {
      return value;
    }
  }

  function isShellToolCall(name) {
    return /(^|[._-])exec_command$/i.test(String(name || ''));
  }

  function formatToolValue(value) {
    const parsed = parseToolArguments(value);
    if (typeof parsed === 'string') return codeFence('', parsed);
    try {
      return codeFence('json', JSON.stringify(parsed, null, 2));
    } catch {
      return codeFence('', String(value ?? ''));
    }
  }

  function formatToolCall(name, value) {
    const toolName = String(name || 'tool_call');
    const parsed = parseToolArguments(value);
    if (parsed && typeof parsed === 'object' && typeof parsed.cmd === 'string') {
      const displayName = isShellToolCall(toolName) ? toolName : 'exec_command';
      return `### ${displayName}\n\n${codeFence('bash', parsed.cmd)}`;
    }
    return `### ${toolName}\n\n${formatToolValue(parsed)}`;
  }

  function parseSSEDataPayloads(body) {
    const payloads = [];
    let dataLines = [];

    const flush = () => {
      if (dataLines.length === 0) return;
      const raw = dataLines.join('\n').trim();
      dataLines = [];
      if (!raw || raw === '[DONE]') return;
      try {
        payloads.push(JSON.parse(raw));
      } catch {
        // Non-JSON SSE data is not useful for merged LLM content.
      }
    };

    String(body || '').replace(/\r\n/g, '\n').split('\n').forEach(line => {
      if (line.startsWith('data:')) {
        const value = line.slice(5);
        dataLines.push(value.startsWith(' ') ? value.slice(1) : value);
        return;
      }
      if (line === '') flush();
    });
    flush();

    return payloads;
  }

  function createMergeState() {
    return {
      reasoning: [],
      text: [],
      functionCalls: [],
      hasReasoningDelta: false,
      hasTextDelta: false,
      hasFunctionCallDelta: false,
      lastFunctionCallIndex: null,
      functionCallDeltaIndexes: new Set()
    };
  }

  function mergedBucketText(bucket) {
    return (bucket || []).join('').trim();
  }

  function formatMergedResponseState(state, options = {}) {
    if (!state || typeof state !== 'object') return '';
    const buckets = options.includeReasoning === true
      ? [state.reasoning, state.text]
      : [state.text];
    if (options.includeFunctionCalls === true) buckets.push(state.functionCalls);

    const sections = [];
    buckets.forEach(bucket => {
      const text = mergedBucketText(bucket);
      if (text) sections.push(text);
    });
    return sections.join('\n\n');
  }

  function formatMergedToolCalls(state) {
    if (!state || typeof state !== 'object') return '';
    const text = mergedBucketText(state.functionCalls);
    if (!text || /^###\s/m.test(text)) return text;
    return formatToolCall('tool_call', text);
  }

  function formatMergedResponseParts(state) {
    if (!state || typeof state !== 'object') {
      return { reasoning: '', content: '', tools: '' };
    }
    return {
      reasoning: mergedBucketText(state.reasoning),
      content: formatMergedResponseState(state),
      tools: formatMergedToolCalls(state)
    };
  }

  function formatJSONContentForMarkdown(text) {
    const raw = String(text || '').trim();
    if (!raw || !/^[\[{]/.test(raw)) return text;
    try {
      const parsed = JSON.parse(raw);
      if (!parsed || typeof parsed !== 'object') return text;
      return codeFence('json', JSON.stringify(parsed, null, 2));
    } catch {
      return text;
    }
  }

  function formatMergedResponseDisplayParts(parts) {
    if (!parts || typeof parts !== 'object') return { reasoning: '', content: '', tools: '' };
    return {
      reasoning: String(parts.reasoning || parts.thinking || ''),
      content: formatJSONContentForMarkdown(parts.content ?? parts.text ?? ''),
      tools: String(parts.tools ?? parts.toolCalls ?? parts.functionCalls ?? '')
    };
  }

  function hasMergedResponseParts(parts) {
    if (!parts || typeof parts !== 'object') return false;
    return Boolean(
      String(parts.reasoning || '').trim()
      || String(parts.content || parts.text || '').trim()
      || String(parts.tools || parts.toolCalls || parts.functionCalls || '').trim()
    );
  }

  function collectMergedResponsePayload(payload, state) {
    if (!payload || typeof payload !== 'object' || !state) return;

    const collectContentParts = (content) => {
      if (!Array.isArray(content)) {
        appendMergedText(state.text, content);
        return;
      }
      content.forEach(part => {
        if (!part || typeof part !== 'object') {
          appendMergedText(state.text, part);
          return;
        }
        appendMergedText(state.text, part.text ?? part.content);
      });
    };

    const collectMessage = (message) => {
      if (!message || typeof message !== 'object') return;
      appendMergedText(state.reasoning, message.reasoning_content);
      appendMergedText(state.reasoning, message.reasoning);
      appendMergedText(state.text, message.content);
      appendMergedText(state.text, message.refusal);
    };

    const collectAnthropicDelta = (payload) => {
      const delta = payload.delta;
      if (!delta || typeof delta !== 'object') return;

      appendMergedText(state.reasoning, delta.thinking);
      appendMergedText(state.text, delta.text);

      if (delta.partial_json != null) {
        if (
          payload.index != null
          && state.lastFunctionCallIndex != null
          && state.lastFunctionCallIndex !== payload.index
        ) {
          state.functionCalls.push('\n\n');
        }
        if (payload.index != null) state.lastFunctionCallIndex = payload.index;
        appendMergedText(state.functionCalls, delta.partial_json);
        state.hasFunctionCallDelta = true;
      }

      if (delta.thinking != null) state.hasReasoningDelta = true;
      if (delta.text != null) state.hasTextDelta = true;
    };

    const hasFunctionCallDeltaFor = (index) => {
      return index != null && state.functionCallDeltaIndexes?.has(index);
    };

    const appendFunctionCallText = (index, text, fromDelta = false) => {
      if (
        index != null
        && state.lastFunctionCallIndex != null
        && state.lastFunctionCallIndex !== index
      ) {
        state.functionCalls.push('\n\n');
      }
      if (index != null) state.lastFunctionCallIndex = index;
      appendMergedText(state.functionCalls, text);
      if (fromDelta) {
        state.hasFunctionCallDelta = true;
        if (index != null) state.functionCallDeltaIndexes.add(index);
      }
    };

    const appendToolCall = (index, name, value) => {
      if (
        index != null
        && state.lastFunctionCallIndex != null
        && state.lastFunctionCallIndex !== index
      ) {
        state.functionCalls.push('\n\n');
      } else if (state.functionCalls.length > 0) {
        state.functionCalls.push('\n\n');
      }
      if (index != null) state.lastFunctionCallIndex = index;
      appendMergedText(state.functionCalls, formatToolCall(name, value));
    };

    const collectOutputItem = (item, fallbackIndex = null) => {
      if (!item || typeof item !== 'object') return;
      const outputIndex = item.output_index ?? fallbackIndex;
      if (item.type === 'message') {
        if (state.hasTextDelta) return;
        collectContentParts(item.content);
        return;
      }
      if (item.type === 'function_call') {
        if (hasFunctionCallDeltaFor(outputIndex) || (outputIndex == null && state.hasFunctionCallDelta)) return;
        appendToolCall(outputIndex, item.name || 'function_call', item.arguments);
        return;
      }
      if (item.type === 'custom_tool_call') {
        if (hasFunctionCallDeltaFor(outputIndex) || (outputIndex == null && state.hasFunctionCallDelta)) return;
        appendToolCall(outputIndex, item.name || 'custom_tool_call', item.input);
        return;
      }
      if (item.type === 'reasoning') {
        if (state.hasReasoningDelta) return;
        appendMergedText(state.reasoning, item.summary || item.content);
      }
    };

    const collectGeminiCandidate = (candidate) => {
      if (!candidate || typeof candidate !== 'object') return;
      const parts = candidate.content?.parts;
      if (!Array.isArray(parts)) {
        appendMergedText(state.text, candidate.content?.text ?? candidate.content);
        return;
      }
      parts.forEach(part => {
        if (!part || typeof part !== 'object') {
          appendMergedText(state.text, part);
          return;
        }
        const target = part.thought === true ? state.reasoning : state.text;
        appendMergedText(target, part.text ?? part.content);
      });
    };

    if (Array.isArray(payload.choices)) {
      payload.choices.forEach(choice => {
        if (!choice || typeof choice !== 'object') return;
        const delta = choice.delta || null;
        if (delta && typeof delta === 'object') {
          appendMergedText(state.reasoning, delta.reasoning_content);
          appendMergedText(state.reasoning, delta.reasoning);
          appendMergedText(state.text, delta.content);
          if (delta.reasoning_content != null || delta.reasoning != null) state.hasReasoningDelta = true;
          if (delta.content != null) state.hasTextDelta = true;
        }
        collectMessage(choice.message);
      });
    }

    if (Array.isArray(payload.candidates)) {
      payload.candidates.forEach(collectGeminiCandidate);
    }

    switch (payload.type) {
      case 'content_block_delta':
        collectAnthropicDelta(payload);
        break;
      case 'response.output_text.delta':
      case 'response.refusal.delta':
        appendMergedText(state.text, payload.delta);
        state.hasTextDelta = true;
        break;
      case 'response.reasoning_text.delta':
      case 'response.reasoning_summary_text.delta':
      case 'response.reasoning.delta':
        appendMergedText(state.reasoning, payload.delta);
        state.hasReasoningDelta = true;
        break;
      case 'response.function_call_arguments.delta':
        appendFunctionCallText(payload.output_index, payload.delta, true);
        break;
      case 'response.function_call_arguments.done':
        if (!hasFunctionCallDeltaFor(payload.output_index)) {
          appendToolCall(payload.output_index, payload.name || 'function_call', payload.arguments);
        }
        break;
      case 'response.custom_tool_call_input.delta':
        appendFunctionCallText(payload.output_index, payload.delta, true);
        break;
      case 'response.custom_tool_call_input.done':
        if (!hasFunctionCallDeltaFor(payload.output_index)) {
          appendToolCall(payload.output_index, payload.name || 'custom_tool_call', payload.input);
        }
        break;
      case 'response.output_item.done':
        collectOutputItem(payload.item, payload.output_index);
        break;
      default:
        break;
    }

    if (Array.isArray(payload.output)) {
      payload.output.forEach((item, index) => collectOutputItem(item, index));
    }
    if (payload.response && Array.isArray(payload.response.output)) {
      payload.response.output.forEach((item, index) => collectOutputItem(item, index));
    }
  }

  window.SSEMerge = {
    appendText: appendMergedText,
    parsePayloads: parseSSEDataPayloads,
    createState: createMergeState,
    collectPayload: collectMergedResponsePayload,
    formatState: formatMergedResponseState,
    formatParts: formatMergedResponseParts,
    formatDisplayParts: formatMergedResponseDisplayParts,
    hasParts: hasMergedResponseParts
  };
})();
