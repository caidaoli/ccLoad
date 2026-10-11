(function (root, factory) {
  const api = factory(root);
  if (typeof module !== 'undefined' && module.exports) module.exports = api;
  if (root) root.ModelTestRoutePreview = api;
})(typeof window !== 'undefined' ? window : null, function (root) {
  'use strict';

  const PROTOCOLS = ['anthropic', 'codex', 'openai', 'gemini'];
  let initialized = false;
  let tokenLoadID = 0;
  let lastResult = null;
  let running = false;
  let tokensReady = false;
  let settings = {};

  function text(key, fallback, params) {
    if (typeof settings.t === 'function') return settings.t(key, fallback, params);
    return root.i18nText(key, fallback, params);
  }

  function element(id) { return root.document.getElementById(id); }

  function buildRequestPayload(values) {
    const model = String(values.model || '').trim();
    if (!model) throw new Error('model_required');
    if (!PROTOCOLS.includes(values.clientProtocol)) throw new Error('invalid_protocol');
    const payload = { model, client_protocol: values.clientProtocol, multimodal: Boolean(values.multimodal) };
    if (values.authTokenID !== '' && values.authTokenID !== undefined && values.authTokenID !== null) {
      const id = Number(values.authTokenID);
      if (!Number.isSafeInteger(id) || id <= 0) throw new Error('invalid_token');
      payload.auth_token_id = id;
    }
    return payload;
  }

  function append(parent, tag, content) {
    const node = root.document.createElement(tag);
    if (content !== undefined) node.textContent = String(content);
    parent.appendChild(node);
    return node;
  }

  const COLUMNS = [
    ['route-preview-col-channel', 'modelTest.channel', '渠道'],
    ['route-preview-col-priority', 'channels.table.priority', '优先级'],
    ['route-preview-col-keys', 'modelTest.routePreview.availableKeys', '可用 Key 数'],
    ['route-preview-col-status', 'modelTest.routePreview.status', '状态'],
    ['route-preview-col-reason', 'modelTest.routePreview.reason', '原因'],
    ['route-preview-col-until', 'modelTest.routePreview.until', '冷却截止']
  ];

  function cell(row, className, content) {
    const node = append(row, 'td', content);
    node.className = className;
    return node;
  }

  function renderTable(fill) {
    const container = element('routePreviewResults');
    container.replaceChildren();
    const wrapper = append(container, 'div');
    wrapper.className = 'table-container model-test-table-container';
    const node = append(wrapper, 'table');
    node.className = 'modern-table model-test-table route-preview-table';
    const head = append(node, 'thead');
    head.className = 'table-head-sticky';
    const row = append(head, 'tr');
    for (const [className, key, fallback] of COLUMNS) {
      const header = append(row, 'th', text(key, fallback));
      header.className = className;
      header.setAttribute('scope', 'col');
    }
    fill(append(node, 'tbody'), container);
  }

  function renderMessage(message) {
    renderTable(body => {
      const row = append(body, 'tr');
      row.className = 'model-test-empty-row';
      append(row, 'td', message).setAttribute('colspan', String(COLUMNS.length));
    });
  }

  function channelCell(row, entry) {
    const name = append(cell(row, 'route-preview-col-channel'), 'span');
    name.className = 'model-test-channel-name';
    const link = append(name, 'button', entry.name);
    link.type = 'button';
    link.className = 'channel-link';
    link.title = text('modelTest.routePreview.editChannel', '编辑渠道');
    link.addEventListener('click', () => root.openLogChannelEditor?.(entry.id));
  }

  function formatPriority(value) {
    const formatted = value.toFixed(1);
    return formatted.endsWith('.0') ? formatted.slice(0, -2) : formatted;
  }

  // 与渠道页一致：动态优先级（健康度模式）与静态值偏差不足 0.1 时不重复显示
  function priorityCell(row, entry) {
    const node = cell(row, 'route-preview-col-priority');
    const base = Number(entry.priority);
    if (entry.priority === undefined || entry.priority === null || !Number.isFinite(base)) {
      node.textContent = '—';
      return;
    }
    const stack = append(node, 'div');
    stack.className = 'ch-priority-stack';
    const baseRow = append(stack, 'div');
    baseRow.className = 'ch-priority-row';
    append(baseRow, 'span', base).className = 'ch-priority-value ch-priority-base-value';
    const titles = [`${text('channels.table.priority', '优先级')}: ${base}`];
    const effective = Number(entry.effective_priority);
    if (entry.effective_priority !== undefined && entry.effective_priority !== null && Number.isFinite(effective) && Math.abs(effective - base) >= 0.1) {
      const value = formatPriority(effective);
      const dynamicRow = append(stack, 'div');
      dynamicRow.className = 'ch-priority-row ch-priority-health';
      append(dynamicRow, 'span', value).className = `ch-priority-value ch-priority-health-${effective < base ? 'bad' : 'good'}`;
      titles.push(`${text('modelTest.dynamicPriority', '动态优先级')}: ${value}`);
    }
    stack.title = titles.join(' | ');
  }

  function statusCell(row, kind, label) {
    append(cell(row, 'route-preview-col-status'), 'span', label).className = `route-preview-status route-preview-status--${kind}`;
  }

  function renderResult(data) {
    const candidates = data.candidates || [];
    const excluded = data.excluded || [];
    if (!candidates.length && !excluded.length) {
      renderMessage(data.model_denied
        ? text('modelTest.routePreview.modelDenied', '此令牌无权访问该模型。')
        : text('modelTest.routePreview.noCandidates', '没有可用候选渠道。'));
    } else {
      renderTable(body => {
        for (const candidate of candidates) {
          const row = append(body, 'tr');
          channelCell(row, candidate);
          priorityCell(row, candidate);
          cell(row, 'route-preview-col-keys', candidate.available_key_count);
          if (candidate.cooldown_fallback) statusCell(row, 'fallback', text('modelTest.routePreview.cooldownFallback', '冷却兜底'));
          else statusCell(row, 'candidate', text('modelTest.routePreview.candidate', '候选'));
          cell(row, 'route-preview-col-reason', candidate.same_tier_rotation ? text('modelTest.routePreview.sameTierRotation', '同档轮转') : '—');
          cell(row, 'route-preview-col-until', '—');
        }
        for (const entry of excluded) {
          const row = append(body, 'tr');
          row.className = 'route-preview-row--excluded';
          channelCell(row, entry);
          priorityCell(row, entry);
          cell(row, 'route-preview-col-keys', '—');
          statusCell(row, 'excluded', text('modelTest.routePreview.excludedStatus', '已排除'));
          cell(row, 'route-preview-col-reason', text(`modelTest.routePreview.reason.${entry.reason}`, entry.reason));
          cell(row, 'route-preview-col-until', entry.until ? new Date(entry.until).toLocaleString(undefined, { hour12: false }) : '—');
        }
      });
    }
    if ((data.limitations || []).length) {
      const details = append(element('routePreviewResults'), 'details');
      details.className = 'route-preview-limitations';
      append(details, 'summary', text('modelTest.routePreview.limitations', '结果说明'));
      const limitations = append(details, 'ul');
      for (const limitation of data.limitations) {
        append(limitations, 'li', text(`modelTest.routePreview.limitation.${limitation}`, limitation));
      }
    }
  }

  function renderCurrent() {
    if (running) renderMessage(text('modelTest.routePreview.loading', '正在预演…'));
    else if (lastResult) renderResult(lastResult);
    else renderMessage(text('modelTest.routePreview.idle', '选择模型后点击“开始预演”查看候选渠道'));
  }

  function refreshModels() {
    if (!initialized) return;
    const select = element('routePreviewModel');
    const selected = select.value;
    const models = settings.getModelOptions();
    select.replaceChildren();
    if (!models.length) {
      const option = append(select, 'option', text('modelTest.noModelsAvailable', '没有可用模型'));
      option.value = '';
    }
    for (const model of models) {
      const option = append(select, 'option', model);
      option.value = model;
    }
    select.value = models.includes(selected) ? selected : (models[0] || '');
    select.disabled = !models.length;
    element('routePreviewRun').disabled = running || !tokensReady || select.disabled;
  }

  async function run(event) {
    event?.preventDefault();
    const button = element('routePreviewRun');
    if (button.disabled) return;
    const status = element('routePreviewStatus');
    let payload;
    try {
      payload = buildRequestPayload({
        model: element('routePreviewModel').value,
        clientProtocol: element('routePreviewProtocol').value,
        authTokenID: element('routePreviewToken').value,
        multimodal: element('routePreviewMultimodal').checked
      });
    } catch (error) {
      status.textContent = text(`modelTest.routePreview.error.${error.message}`, '请检查模型、令牌和协议。');
      if (error.message === 'model_required') element('routePreviewModel').focus();
      return;
    }
    running = true;
    button.disabled = true;
    lastResult = null;
    status.textContent = '';
    renderCurrent();
    try {
      lastResult = await root.fetchDataWithAuth('/admin/route-preview', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload)
      });
    } catch (error) {
      status.textContent = text('modelTest.routePreview.failed', '预演失败') + ': ' + error.message;
    } finally {
      running = false;
      button.disabled = !tokensReady || element('routePreviewModel').disabled;
      renderCurrent();
    }
  }

  async function open() {
    const loadID = ++tokenLoadID;
    const select = element('routePreviewToken');
    const selected = select.value;
    const button = element('routePreviewRun');
    tokensReady = false;
    select.disabled = true;
    button.disabled = true;
    refreshModels();
    try {
      const data = await root.fetchDataWithAuth('/admin/auth-tokens');
      if (loadID !== tokenLoadID) return;
      select.replaceChildren();
      const unrestricted = append(select, 'option', text('modelTest.routePreview.noToken', '不限令牌'));
      unrestricted.value = '';
      for (const token of data.tokens || []) {
        const option = append(select, 'option', `${token.description} (#${token.id})`);
        option.value = String(token.id);
        option.disabled = !token.is_active || token.is_expired;
      }
      select.value = Array.from(select.options).some(option => option.value === selected && !option.disabled) ? selected : '';
      element('routePreviewStatus').textContent = '';
      tokensReady = true;
      button.disabled = running || element('routePreviewModel').disabled;
    } catch (error) {
      if (loadID !== tokenLoadID) return;
      element('routePreviewStatus').textContent = text('modelTest.routePreview.tokenLoadFailed', '加载令牌失败') + ': ' + error.message;
    } finally {
      if (loadID === tokenLoadID) select.disabled = false;
    }
  }

  function init(options = {}) {
    settings = options;
    if (initialized) return;
    initialized = true;
    const protocolSelect = element('routePreviewProtocol');
    protocolSelect.replaceChildren();
    for (const protocol of options.protocols || PROTOCOLS) {
      const option = append(protocolSelect, 'option', options.protocolLabel?.(protocol) || protocol);
      option.value = protocol;
    }
    refreshModels();
    renderCurrent();
    root.enhanceNativeSelects(element('routePreviewPanel'));
    element('routePreviewForm').addEventListener('submit', run);
    root.i18n?.onLocaleChange?.(() => {
      for (const option of protocolSelect.options) option.textContent = settings.protocolLabel?.(option.value) || option.value;
      const unrestricted = Array.from(element('routePreviewToken').options).find(option => option.value === '');
      if (unrestricted) unrestricted.textContent = text('modelTest.routePreview.noToken', '不限令牌');
      refreshModels();
      renderCurrent();
    });
  }

  return { buildRequestPayload, init, open, run, refreshModels };
});
