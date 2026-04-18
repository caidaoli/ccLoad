/**
 * 渠道自定义请求规则模态框
 *
 * 暴露全局函数：
 * - openCustomRulesModal / closeCustomRulesModal
 * - resetCustomRulesState(rules|null)
 * - collectCustomRulesForSubmit()
 * - applyCustomRulesFromForm() / addCustomRule / removeCustomRule / closeCustomRulesHelp
 *
 * 状态：模块内 `_state`（{ headers: [], body: [] }）与 `_draft`（仅模态打开期间）
 */
(function () {
  'use strict';

  const MAX_RULES = 32;
  const MAX_VALUE_BYTES = 8 * 1024;
  const MAX_NAME = 256;
  const PATH_REGEX = /^[A-Za-z0-9_.\-]+$/;
  const AUTH_BLACKLIST = new Set(['authorization', 'x-api-key', 'x-goog-api-key']);

  const HEADER_ACTIONS = ['override', 'append', 'remove'];
  const BODY_ACTIONS = ['override', 'remove'];

  const hasWindow = typeof window !== 'undefined';
  const hasDocument = typeof document !== 'undefined';

  let _state = { headers: [], body: [] };
  let _draft = null;

  function t(key, fallback) {
    if (hasWindow && typeof window.t === 'function') {
      const v = window.t(key);
      if (v && v !== key) return v;
    }
    return fallback;
  }

  function cloneRules(source) {
    const safe = source && typeof source === 'object' ? source : {};
    const headers = Array.isArray(safe.headers) ? safe.headers : [];
    const body = Array.isArray(safe.body) ? safe.body : [];
    return {
      headers: headers.map((r) => ({
        action: String(r && r.action || 'override').toLowerCase(),
        name: String(r && r.name || ''),
        value: typeof (r && r.value) === 'string' ? r.value : (r && r.value == null ? '' : String(r.value))
      })),
      body: body.map((r) => ({
        action: String(r && r.action || 'override').toLowerCase(),
        path: String(r && r.path || ''),
        value: normalizeBodyValue(r && r.value)
      }))
    };
  }

  function normalizeBodyValue(v) {
    if (v == null) return '';
    if (typeof v === 'string') return v;
    try {
      return JSON.stringify(v);
    } catch (_) {
      return '';
    }
  }

  function getState() {
    if (!_state || typeof _state !== 'object') _state = { headers: [], body: [] };
    if (!Array.isArray(_state.headers)) _state.headers = [];
    if (!Array.isArray(_state.body)) _state.body = [];
    return _state;
  }

  function resetCustomRulesState(rules) {
    _state = rules == null ? { headers: [], body: [] } : cloneRules(rules);
    if (hasWindow) window.channelCustomRulesState = _state;
    updateTabCounts(_state);
  }

  function updateTabCounts(src) {
    if (!hasDocument) return;
    const headersEl = document.getElementById('customRulesHeadersCount');
    const bodyEl = document.getElementById('customRulesBodyCount');
    if (headersEl) headersEl.textContent = `(${(src && src.headers ? src.headers.length : 0)})`;
    if (bodyEl) bodyEl.textContent = `(${(src && src.body ? src.body.length : 0)})`;
  }

  function byteLength(str) {
    if (typeof str !== 'string') return 0;
    if (typeof TextEncoder !== 'undefined') {
      return new TextEncoder().encode(str).length;
    }
    return str.length;
  }

  function validateRulesLocally(rules) {
    const errors = [];
    if (!rules) return errors;
    const headers = Array.isArray(rules.headers) ? rules.headers : [];
    const body = Array.isArray(rules.body) ? rules.body : [];

    if (headers.length > MAX_RULES) {
      errors.push(t('channels.customRules.errMaxHeaders', `Too many header rules (max ${MAX_RULES})`));
    }
    if (body.length > MAX_RULES) {
      errors.push(t('channels.customRules.errMaxBody', `Too many body rules (max ${MAX_RULES})`));
    }

    headers.forEach((rule, idx) => {
      const label = `[${t('channels.customRules.tabHeaders', 'Headers')} #${idx + 1}]`;
      if (!rule || typeof rule !== 'object') {
        errors.push(`${label} ${t('channels.customRules.errInvalid', 'Invalid rule')}`);
        return;
      }
      if (!['remove', 'override', 'append'].includes(rule.action)) {
        errors.push(`${label} ${t('channels.customRules.errAction', 'Invalid action')}`);
        return;
      }
      const name = (rule.name || '').trim();
      if (!name) {
        errors.push(`${label} ${t('channels.customRules.errHeaderName', 'Header name required')}`);
        return;
      }
      if (name.length > MAX_NAME) {
        errors.push(`${label} ${t('channels.customRules.errNameTooLong', 'Header name too long')}`);
      }
      if (/[\r\n\0]/.test(name)) {
        errors.push(`${label} ${t('channels.customRules.errNameCRLF', 'Header name contains illegal characters')}`);
      }
      if (AUTH_BLACKLIST.has(name.toLowerCase())) {
        errors.push(`${label} ${t('channels.customRules.errAuthHeader', 'Auth header cannot be customized')}`);
      }
      if (rule.action !== 'remove') {
        const val = typeof rule.value === 'string' ? rule.value : '';
        if (byteLength(val) > MAX_VALUE_BYTES) {
          errors.push(`${label} ${t('channels.customRules.errValueTooLong', 'Value too long')}`);
        }
        if (/[\r\n\0]/.test(val)) {
          errors.push(`${label} ${t('channels.customRules.errValueCRLF', 'Value contains illegal characters')}`);
        }
      }
    });

    body.forEach((rule, idx) => {
      const label = `[${t('channels.customRules.tabBody', 'Body')} #${idx + 1}]`;
      if (!rule || typeof rule !== 'object') {
        errors.push(`${label} ${t('channels.customRules.errInvalid', 'Invalid rule')}`);
        return;
      }
      if (!['remove', 'override'].includes(rule.action)) {
        errors.push(`${label} ${t('channels.customRules.errAction', 'Invalid action')}`);
        return;
      }
      const path = (rule.path || '').trim();
      if (!path) {
        errors.push(`${label} ${t('channels.customRules.errPath', 'Path required')}`);
        return;
      }
      if (path.length > MAX_NAME) {
        errors.push(`${label} ${t('channels.customRules.errPathTooLong', 'Path too long')}`);
      }
      if (!PATH_REGEX.test(path)) {
        errors.push(`${label} ${t('channels.customRules.errPathChars', 'Invalid path characters')}`);
      }
      if (rule.action !== 'remove') {
        const val = typeof rule.value === 'string' ? rule.value : '';
        if (!val) {
          errors.push(`${label} ${t('channels.customRules.errBodyValueEmpty', 'Value required')}`);
          return;
        }
        if (byteLength(val) > MAX_VALUE_BYTES) {
          errors.push(`${label} ${t('channels.customRules.errValueTooLong', 'Value too long')}`);
        }
        try {
          JSON.parse(val);
        } catch (_) {
          errors.push(`${label} ${t('channels.customRules.errBodyValueJSON', 'Value must be a valid JSON literal')}`);
        }
      }
    });

    return errors;
  }

  function collectCustomRulesForSubmit() {
    const state = getState();
    const headers = (state.headers || [])
      .map((r) => ({
        action: r.action,
        name: (r.name || '').trim(),
        value: r.value || ''
      }))
      .filter((r) => r.name);
    const body = (state.body || [])
      .map((r) => {
        const action = r.action;
        const path = (r.path || '').trim();
        if (!path) return null;
        if (action === 'remove') return { action, path };
        try {
          return { action, path, value: JSON.parse(r.value || '') };
        } catch (_) {
          return null;
        }
      })
      .filter((r) => r);
    if (headers.length === 0 && body.length === 0) return null;
    const payload = {};
    if (headers.length > 0) {
      payload.headers = headers.map((r) => {
        const entry = { action: r.action, name: r.name };
        if (r.action === 'remove') {
          // remove + 空值=整头删除（省略 value）；非空值=按逗号 token 精确移除
          if (r.value) entry.value = r.value;
        } else {
          // override/append 始终保留 value（允许空字符串）
          entry.value = r.value;
        }
        return entry;
      });
    }
    if (body.length > 0) {
      payload.body = body;
    }
    return payload;
  }

  // ===== 以下函数依赖 DOM，仅在浏览器中生效 =====

  function openCustomRulesModal() {
    if (!hasDocument) return;
    _draft = cloneRules(getState());
    renderRuleList('headers');
    renderRuleList('body');
    switchTab('headers');
    hideError();
    const modal = document.getElementById('customRulesModal');
    if (modal) modal.classList.add('show');
  }

  function closeCustomRulesModal() {
    if (!hasDocument) return;
    const modal = document.getElementById('customRulesModal');
    if (modal) modal.classList.remove('show');
    _draft = null;
    closeCustomRulesHelp();
  }

  function switchTab(tab) {
    if (!hasDocument) return;
    const panels = {
      headers: document.getElementById('customRulesPanelHeaders'),
      body: document.getElementById('customRulesPanelBody')
    };
    const buttons = document.querySelectorAll('[data-custom-rules-tab]');
    buttons.forEach((btn) => {
      const active = btn.dataset.customRulesTab === tab;
      btn.classList.toggle('active', active);
      btn.setAttribute('aria-selected', active ? 'true' : 'false');
    });
    Object.entries(panels).forEach(([k, el]) => {
      if (!el) return;
      el.classList.toggle('hidden', k !== tab);
    });
  }

  function addCustomRule(target) {
    if (!_draft) return;
    if (target === 'headers') {
      if (_draft.headers.length >= MAX_RULES) {
        showError(t('channels.customRules.errMaxHeaders', `Too many header rules (max ${MAX_RULES})`));
        return;
      }
      _draft.headers.push({ action: 'override', name: '', value: '' });
    } else if (target === 'body') {
      if (_draft.body.length >= MAX_RULES) {
        showError(t('channels.customRules.errMaxBody', `Too many body rules (max ${MAX_RULES})`));
        return;
      }
      _draft.body.push({ action: 'override', path: '', value: '' });
    } else {
      return;
    }
    hideError();
    renderRuleList(target);
  }

  function removeCustomRule(target, index) {
    if (!_draft) return;
    if (typeof index !== 'number' || Number.isNaN(index) || index < 0) return;
    const list = _draft[target];
    if (!Array.isArray(list) || index >= list.length) return;
    list.splice(index, 1);
    renderRuleList(target);
  }

  function renderRuleList(target) {
    if (!hasDocument || !_draft) return;
    const listId = target === 'headers' ? 'customRulesListHeaders' : 'customRulesListBody';
    const list = document.getElementById(listId);
    if (!list) return;
    const rules = _draft[target] || [];
    list.innerHTML = '';
    if (rules.length === 0) {
      const empty = document.createElement('div');
      empty.className = 'custom-rules-empty';
      empty.textContent = t('channels.customRules.empty', 'No rules yet.');
      list.appendChild(empty);
    }
    rules.forEach((rule, idx) => {
      list.appendChild(buildRuleRow(target, rule, idx));
    });
    updateTabCounts(_draft);
  }

  function buildRuleRow(target, rule, idx) {
    const row = document.createElement('div');
    row.className = 'custom-rules-row';

    const actionSelect = document.createElement('select');
    actionSelect.className = 'form-input custom-rules-action';
    (target === 'headers' ? HEADER_ACTIONS : BODY_ACTIONS).forEach((action) => {
      const opt = document.createElement('option');
      opt.value = action;
      opt.textContent = t(`channels.customRules.action_${action}`, action);
      if (action === rule.action) opt.selected = true;
      actionSelect.appendChild(opt);
    });
    actionSelect.addEventListener('change', () => {
      rule.action = actionSelect.value;
      updateValueDisabled(row, rule, target);
    });
    row.appendChild(actionSelect);

    const primary = document.createElement('input');
    primary.className = 'form-input custom-rules-primary';
    primary.type = 'text';
    primary.maxLength = MAX_NAME;
    if (target === 'headers') {
      primary.value = rule.name || '';
      primary.placeholder = t('channels.customRules.placeholderHeaderName', 'X-Api-Version');
      primary.addEventListener('input', () => { rule.name = primary.value; });
    } else {
      primary.value = rule.path || '';
      primary.placeholder = t('channels.customRules.placeholderPath', 'thinking.budget_tokens');
      primary.addEventListener('input', () => { rule.path = primary.value; });
    }
    row.appendChild(primary);

    const valueInput = document.createElement('input');
    valueInput.className = 'form-input custom-rules-value';
    valueInput.type = 'text';
    valueInput.value = rule.value || '';
    valueInput.placeholder = target === 'headers'
      ? t('channels.customRules.placeholderHeaderValue', '2025-08-07')
      : t('channels.customRules.placeholderBodyValue', '"text" / 8192 / {"a":1}');
    valueInput.addEventListener('input', () => { rule.value = valueInput.value; });
    row.appendChild(valueInput);

    const removeBtn = document.createElement('button');
    removeBtn.type = 'button';
    removeBtn.className = 'btn btn-secondary custom-rules-remove-btn';
    removeBtn.setAttribute('data-action', 'remove-custom-rule');
    removeBtn.dataset.customRulesTarget = target;
    removeBtn.dataset.customRulesIndex = String(idx);
    removeBtn.textContent = '×';
    removeBtn.title = t('channels.customRules.deleteRule', 'Delete');
    row.appendChild(removeBtn);

    updateValueDisabled(row, rule, target);
    return row;
  }

  function updateValueDisabled(row, rule, target) {
    const valueInput = row.querySelector('.custom-rules-value');
    if (!valueInput) return;
    const isRemove = rule.action === 'remove';
    // headers 的 remove：留 value 输入框，空=删整条，填值=按逗号 token 精确移除
    const shouldDisable = isRemove && target !== 'headers';
    valueInput.disabled = shouldDisable;
    valueInput.classList.toggle('custom-rules-value-disabled', shouldDisable);
    if (shouldDisable) {
      valueInput.value = '';
      rule.value = '';
    }
  }

  function showError(msg) {
    if (!hasDocument) return;
    const el = document.getElementById('customRulesError');
    if (!el) return;
    el.textContent = msg;
    el.hidden = false;
  }

  function hideError() {
    if (!hasDocument) return;
    const el = document.getElementById('customRulesError');
    if (!el) return;
    el.textContent = '';
    el.hidden = true;
  }

  function applyCustomRulesFromForm() {
    if (!_draft) {
      closeCustomRulesModal();
      return;
    }
    const normalized = {
      headers: _draft.headers.map((r) => ({
        action: r.action,
        name: (r.name || '').trim(),
        value: r.value || ''
      })),
      body: _draft.body.map((r) => ({
        action: r.action,
        path: (r.path || '').trim(),
        value: r.action === 'remove' ? '' : (r.value || '')
      }))
    };
    const errors = validateRulesLocally(normalized);
    if (errors.length > 0) {
      showError(errors.join(' · '));
      return;
    }
    _state = normalized;
    if (hasWindow) {
      window.channelCustomRulesState = _state;
      if (typeof window.markChannelFormDirty === 'function') {
        window.markChannelFormDirty();
      }
    }
    updateTabCounts(_state);
    closeCustomRulesModal();
  }

  function showCustomRulesHelp(target) {
    if (!hasDocument) return;
    const popup = document.getElementById('customRulesHelpPopup');
    const content = document.getElementById('customRulesHelpContent');
    if (!popup || !content) return;
    const key = target === 'body' ? 'channels.customRules.helpBody' : 'channels.customRules.helpHeaders';
    content.textContent = t(key, target === 'body' ? defaultHelpBody() : defaultHelpHeaders());
    popup.hidden = false;
  }

  function closeCustomRulesHelp() {
    if (!hasDocument) return;
    const popup = document.getElementById('customRulesHelpPopup');
    if (popup) popup.hidden = true;
  }

  function defaultHelpHeaders() {
    return 'Rewrite HTTP headers sent to upstream.\nActions: remove / override / append.\nremove: empty value deletes the header; non-empty value removes only that comma-separated token (e.g. remove "context-1m-2025-08-07" from Anthropic-Beta).\nAuth headers (Authorization / x-api-key / x-goog-api-key) are protected.';
  }
  function defaultHelpBody() {
    return 'Rewrite JSON body fields.\nActions: remove / override.\nPath uses dots + integer indices (messages.0.role).\nValues are JSON literals — strings need quotes.';
  }

  function bindTabDelegation() {
    if (!hasDocument) return;
    const tabs = document.querySelectorAll('[data-custom-rules-tab]');
    tabs.forEach((btn) => {
      if (btn.dataset.customRulesTabBound === '1') return;
      btn.addEventListener('click', (ev) => {
        if (ev.target && ev.target.classList && ev.target.classList.contains('custom-rules-help-icon')) {
          return;
        }
        switchTab(btn.dataset.customRulesTab);
      });
      btn.dataset.customRulesTabBound = '1';
    });

    const helpIcons = document.querySelectorAll('[data-custom-rules-help]');
    helpIcons.forEach((icon) => {
      if (icon.dataset.customRulesHelpBound === '1') return;
      icon.addEventListener('click', (ev) => {
        ev.stopPropagation();
        showCustomRulesHelp(icon.dataset.customRulesHelp);
      });
      icon.dataset.customRulesHelpBound = '1';
    });
  }

  function init() {
    bindTabDelegation();
    resetCustomRulesState(null);
  }

  if (hasDocument) {
    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', init);
    } else {
      init();
    }
  }

  if (hasWindow) {
    window.openCustomRulesModal = openCustomRulesModal;
    window.closeCustomRulesModal = closeCustomRulesModal;
    window.applyCustomRulesFromForm = applyCustomRulesFromForm;
    window.addCustomRule = addCustomRule;
    window.removeCustomRule = removeCustomRule;
    window.closeCustomRulesHelp = closeCustomRulesHelp;
    window.resetCustomRulesState = resetCustomRulesState;
    window.collectCustomRulesForSubmit = collectCustomRulesForSubmit;
    window.validateCustomRulesLocally = validateRulesLocally;
  }

  if (typeof module !== 'undefined' && module.exports) {
    module.exports = {
      validateRulesLocally,
      collectCustomRulesForSubmit,
      resetCustomRulesState,
      cloneRules,
      normalizeBodyValue,
      byteLength,
      getState,
      MAX_RULES,
      MAX_VALUE_BYTES,
      MAX_NAME
    };
  }
})();
