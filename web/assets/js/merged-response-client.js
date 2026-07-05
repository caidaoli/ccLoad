(function () {
  'use strict';

  const MERGE_ENDPOINT = '/admin/debug-logs/merged-response';

  async function gzipBytes(bytes) {
    if (typeof CompressionStream !== 'function') return null;
    const stream = new Blob([bytes]).stream().pipeThrough(new CompressionStream('gzip'));
    const compressed = await new Response(stream).arrayBuffer();
    return new Uint8Array(compressed);
  }

  async function buildMergeRequestBody(respBody) {
    const json = JSON.stringify({ resp_body: String(respBody || '') });
    const bytes = new TextEncoder().encode(json);
    try {
      const gzipped = await gzipBytes(bytes);
      if (gzipped && gzipped.byteLength > 0 && gzipped.byteLength < bytes.byteLength) {
        return {
          body: gzipped,
          headers: {
            'Content-Type': 'application/json',
            'Content-Encoding': 'gzip',
          },
        };
      }
    } catch (_) {
      // Fall through to plain JSON. Compression is an optimization, not a contract.
    }
    return {
      body: json,
      headers: { 'Content-Type': 'application/json' },
    };
  }

  async function mergeUpstreamResponse(respBody) {
    const request = await buildMergeRequestBody(respBody);
    const payload = await fetchAPIWithAuth(MERGE_ENDPOINT, {
      method: 'POST',
      headers: request.headers,
      body: request.body,
    });
    if (!payload.success) {
      throw new Error(payload.error || 'merge response failed');
    }
    return payload.data || { reasoning: '', content: '', tools: '' };
  }

  window.MergedResponseClient = {
    mergeUpstreamResponse,
  };
})();
