// 渠道模型价格：挂在渠道模型行上的 8 项价目（美元/百万 Token），整份替换该模型的全局价格。
(function initChannelModelPricing(root, factory) {
  const api = factory(root);
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
  }
  if (root) {
    root.ChannelModelPricing = api;
    root.openChannelModelPricingModal = api.openChannelModelPricingModal;
  }
})(typeof window !== 'undefined' ? window : globalThis, function createChannelModelPricing(root) {
  const BASIC_FIELDS = ['input_price', 'output_price', 'cache_read_price', 'cache_write_price'];
  const HIGH_CONTEXT_FIELDS = ['input_price_high', 'output_price_high', 'cache_read_price_high', 'cache_write_price_high'];
  const PRICING_FIELDS = [...BASIC_FIELDS, ...HIGH_CONTEXT_FIELDS];

  const t = (key, params) => (typeof root?.t === 'function' ? root.t(key, params) : key);
  const isValidPrice = value => typeof value === 'number' && Number.isFinite(value) && value >= 0;

  /** 只保留契约字段中的合法价格（显式 0 合法）；没有任何价格时返回 null，即沿用全局价格。 */
  function normalizeChannelModelPricing(pricing) {
    if (!pricing || typeof pricing !== 'object') return null;
    const normalized = {};
    for (const field of PRICING_FIELDS) {
      if (isValidPrice(pricing[field])) normalized[field] = pricing[field];
    }
    return Object.keys(normalized).length > 0 ? normalized : null;
  }

  /** 状态列摘要：输入/输出单价。 */
  function formatChannelModelPricingSummary(pricing) {
    const normalized = normalizeChannelModelPricing(pricing);
    if (!normalized) return '';
    const format = value => (isValidPrice(value) ? `$${value}` : '—');
    return t('channels.modelPricing.summary', {
      input: format(normalized.input_price),
      output: format(normalized.output_price)
    });
  }

  /** 表单字符串 → { pricing, error, field }；pricing 为 null 表示沿用全局价格。 */
  function parseChannelModelPricingForm(values) {
    const pricing = {};
    for (const field of PRICING_FIELDS) {
      const raw = String(values?.[field] ?? '').trim();
      if (raw === '') continue;
      const value = Number(raw);
      if (!isValidPrice(value)) return { pricing: null, error: 'channels.modelPricing.invalidNumber', field };
      pricing[field] = value;
    }
    if (Object.keys(pricing).length === 0) return { pricing: null, error: '', field: '' };
    // 输入/输出价留空会按 0 计费，几乎总是漏填；高上下文价同理需成对填写。
    const required = HIGH_CONTEXT_FIELDS.some(field => field in pricing)
      ? ['input_price', 'output_price', 'input_price_high', 'output_price_high']
      : ['input_price', 'output_price'];
    const missing = required.find(field => !(field in pricing));
    if (missing) return { pricing: null, error: 'channels.modelPricing.requiredMissing', field: missing };
    return { pricing, error: '', field: '' };
  }

  /** 预填候选：计费优先用重定向目标，没有目录价格时回退渠道模型名。 */
  function channelModelPricingLookupCandidates(row) {
    const names = [row?.redirect_model, row?.model].map(name => String(name || '').trim());
    return [...new Set(names)].filter(name => name && name !== '*');
  }

  // ==================== 对话框 ====================

  let editingIndex = -1;
  let dialogTrigger = null;
  let defaultsRequestSeq = 0;

  const getModal = () => document.getElementById('channelModelPricingModal');
  const getFieldInputs = modal => Array.from(modal.querySelectorAll('input[data-pricing-field]'));

  function setStatus(modal, message, state = '') {
    const status = modal.querySelector('#channelModelPricingStatus');
    status.textContent = message;
    status.dataset.state = state;
  }

  function fillForm(modal, pricing) {
    for (const input of getFieldInputs(modal)) {
      const value = pricing?.[input.dataset.pricingField];
      input.value = isValidPrice(value) ? String(value) : '';
      input.removeAttribute('aria-invalid');
    }
  }

  // 字段标签复用全局自定义价格的文案键：input_price_high → settings.customPricing.inputPriceHigh。
  function renderFields(modal) {
    for (const [group, fields] of [['basic', BASIC_FIELDS], ['high', HIGH_CONTEXT_FIELDS]]) {
      const grid = modal.querySelector(`[data-pricing-group="${group}"]`);
      if (!grid || grid.childElementCount > 0) continue;
      for (const field of fields) {
        const labelKey = 'settings.customPricing.' + field.replace(/_([a-z])/g, (_, letter) => letter.toUpperCase());
        const label = document.createElement('label');
        label.className = 'form-label';
        const text = document.createElement('span');
        text.dataset.i18n = labelKey;
        text.textContent = t(labelKey);
        const input = document.createElement('input');
        input.type = 'number';
        input.min = '0';
        input.step = 'any';
        input.inputMode = 'decimal';
        input.className = 'form-input custom-pricing-number';
        input.dataset.pricingField = field;
        label.append(text, input);
        grid.appendChild(label);
      }
    }
  }

  function openChannelModelPricingModal(index, trigger) {
    const modal = getModal();
    const row = redirectTableData[index];
    const modelName = String(row?.model || '').trim();
    // 空模型与通配行的价格按钮已禁用。
    if (!modal || !modelName || modelName === '*') return false;

    bindModalEvents(modal);
    renderFields(modal);
    editingIndex = index;
    dialogTrigger = trigger || document.activeElement;
    defaultsRequestSeq++;
    modal.querySelector('#channelModelPricingModelName').textContent = modelName;
    fillForm(modal, normalizeChannelModelPricing(row.pricing));
    setStatus(modal, '');

    document.getElementById('channelModal')?.setAttribute('inert', '');
    modal.classList.add('show');
    modal.setAttribute('aria-hidden', 'false');
    getFieldInputs(modal)[0]?.focus();
    return true;
  }

  function closeChannelModelPricingModal() {
    const modal = getModal();
    defaultsRequestSeq++;
    modal.classList.remove('show');
    modal.setAttribute('aria-hidden', 'true');
    document.getElementById('channelModal')?.removeAttribute('inert');
    dialogTrigger?.focus?.();
    dialogTrigger = null;
    editingIndex = -1;
  }

  function applyChannelModelPricing(modal) {
    const row = redirectTableData[editingIndex];
    if (!row) return;
    const values = Object.fromEntries(getFieldInputs(modal).map(input => [input.dataset.pricingField, input.value]));
    const { pricing, error, field } = parseChannelModelPricingForm(values);
    if (error) {
      setStatus(modal, t(error), 'error');
      const input = modal.querySelector(`input[data-pricing-field="${field}"]`);
      input?.setAttribute('aria-invalid', 'true');
      input?.focus();
      return;
    }
    const index = editingIndex;
    const changed = JSON.stringify(normalizeChannelModelPricing(row.pricing)) !== JSON.stringify(pricing);
    row.pricing = pricing;
    closeChannelModelPricingModal();
    if (!changed) return;
    renderRedirectTable();
    markChannelFormDirty();
    document.querySelector(`#redirectTableBody .redirect-model-price-btn[data-index="${index}"]`)?.focus();
  }

  async function loadChannelModelPricingDefaults(modal) {
    const row = redirectTableData[editingIndex];
    if (!row) return;
    const requestSeq = ++defaultsRequestSeq;
    try {
      for (const candidate of channelModelPricingLookupCandidates(row)) {
        const result = await root.fetchDataWithAuth(
          `/admin/model-pricing?model=${encodeURIComponent(candidate)}&effective=1`
        );
        if (requestSeq !== defaultsRequestSeq) return;
        if (!result?.found) continue;
        if (result.pricing?.token_pricing_tiers?.length > 0) {
          setStatus(modal, t('channels.modelPricing.tieredNoDefaults', { model: candidate }), 'error');
          return;
        }
        fillForm(modal, normalizeChannelModelPricing(result.pricing));
        setStatus(modal, t('channels.modelPricing.defaultLoaded', { model: candidate }), 'success');
        return;
      }
      setStatus(modal, t('channels.modelPricing.defaultNotFound'), 'error');
    } catch (error) {
      if (requestSeq !== defaultsRequestSeq) return;
      console.error('Load channel model pricing defaults failed', error);
      setStatus(modal, t('channels.modelPricing.defaultLoadFailed'), 'error');
    }
  }

  function bindModalEvents(modal) {
    if (modal.dataset.bound) return;
    modal.dataset.bound = '1';
    modal.addEventListener('click', event => {
      const action = event.target === modal
        ? 'close'
        : event.target.closest('[data-pricing-action]')?.dataset.pricingAction;
      if (action === 'close') closeChannelModelPricingModal();
      else if (action === 'apply') applyChannelModelPricing(modal);
      else if (action === 'clear') fillForm(modal, null);
      else if (action === 'load-defaults') void loadChannelModelPricingDefaults(modal);
    });
    // 捕获阶段处理 Escape，避免全局 Escape 链把渠道编辑器一起关掉。
    document.addEventListener('keydown', event => {
      if (event.key !== 'Escape' || !modal.classList.contains('show')) return;
      event.preventDefault();
      event.stopPropagation();
      closeChannelModelPricingModal();
    }, true);
  }

  return {
    channelModelPricingLookupCandidates,
    formatChannelModelPricingSummary,
    normalizeChannelModelPricing,
    openChannelModelPricingModal,
    parseChannelModelPricingForm
  };
});
