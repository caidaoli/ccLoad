(function (root, factory) {
  const api = factory(root);
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  if (root) root.TokenModelTest = api;
})(typeof window !== 'undefined' ? window : null, function (root) {
  let allowedModels = [];

  function normalizeProtocol(protocol) {
    return ['anthropic', 'openai', 'codex', 'gemini'].includes(protocol) ? protocol : 'openai';
  }

  function endpointFor(protocol, model, stream) {
    switch (normalizeProtocol(protocol)) {
      case 'anthropic': return '/dashboard/v1/messages';
      case 'codex': return '/dashboard/v1/responses';
      case 'gemini':
        return `/dashboard/v1beta/models/${encodeURIComponent(model)}:${stream ? 'streamGenerateContent' : 'generateContent'}`;
      default: return '/dashboard/v1/chat/completions';
    }
  }

  function buildPayload(protocol, model, content, stream) {
    switch (normalizeProtocol(protocol)) {
      case 'anthropic':
        return { model, max_tokens: 1024, messages: [{ role: 'user', content }], stream };
      case 'codex':
        return { model, input: content, stream };
      case 'gemini':
        return { contents: [{ role: 'user', parts: [{ text: content }] }] };
      default:
        return { model, messages: [{ role: 'user', content }], stream };
    }
  }

  function isModelAllowed(model, models) {
    if (!Array.isArray(models) || models.length === 0) return true;
    const normalized = String(model || '').toLowerCase();
    return models.some((item) => String(item || '').toLowerCase() === normalized);
  }

  function setRunning(running) {
    const button = document.getElementById('tokenModelTestRun');
    if (!button) return;
    button.disabled = running;
    button.textContent = running ? root.t('modelTest.testing') : root.t('modelTest.startTest');
  }

  async function runTest() {
    const protocol = document.getElementById('tokenModelTestProtocol').value;
    const model = document.getElementById('tokenModelTestModel').value.trim();
    const content = document.getElementById('tokenModelTestContent').value.trim();
    const stream = document.getElementById('tokenModelTestStream').checked;
    const output = document.getElementById('tokenModelTestOutput');
    if (!model || !content) {
      root.showError(root.t('error.invalidInput'));
      return;
    }
	if (!isModelAllowed(model, allowedModels)) {
	  root.showError(root.t('modelTest.modelNotAllowed'));
	  return;
	}

    setRunning(true);
    output.textContent = root.t('common.loading');
    try {
      const response = await root.fetchWithAuth(endpointFor(protocol, model, stream), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(buildPayload(protocol, model, content, stream))
      });
      const raw = await response.text();
      if (!response.ok) throw new Error(raw || `HTTP ${response.status}`);
      try {
        output.textContent = JSON.stringify(JSON.parse(raw), null, 2);
      } catch (_) {
        output.textContent = raw;
      }
    } catch (error) {
      output.textContent = error.message || String(error);
      root.showError(error.message || root.t('error.requestFailed'));
    } finally {
      setRunning(false);
    }
  }

  async function init() {
    document.getElementById('modelTestCard').hidden = true;
    const card = document.getElementById('tokenModelTestCard');
    card.hidden = false;

    const session = await root.fetchDataWithAuth('/dashboard/session');
    const models = Array.isArray(session.allowed_models) ? session.allowed_models : [];
	allowedModels = models;
    const input = document.getElementById('tokenModelTestModel');
    const datalist = document.getElementById('tokenModelTestModels');
    let choices = models;
    if (choices.length === 0) {
      const modelData = await root.fetchDataWithAuth('/dashboard/models?range=this_month');
      choices = Array.isArray(modelData.models) ? modelData.models : [];
    }
    datalist.innerHTML = '';
    choices.forEach((model) => {
      const option = document.createElement('option');
      option.value = model;
      datalist.appendChild(option);
    });
    if (choices.length > 0) input.value = choices[0];
    document.getElementById('tokenModelTestIdentity').textContent = session.description || '';
    document.getElementById('tokenModelTestRun').addEventListener('click', runTest);
  }

  return { endpointFor, buildPayload, isModelAllowed, init };
});
