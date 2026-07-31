/**
 * 弹出一个让用户选择/输入具体测试模型的轻量对话框（combobox + allowCustomInput）。
 * 用于仅配置通配规则（且无重定向目标）的渠道在测试 Key/URL 时指定一个具体请求模型。
 * 提交时拦截通配字面值（* / ?），与后端 SupportsModel 一致：具体模型合法性交后端校验。
 */
(function () {
  'use strict';

  /**
   * @param {object} cfg
   * @param {string} [cfg.title] 对话框标题
   * @param {string} [cfg.placeholder] 输入框占位
   * @param {string[]} [cfg.options] 下拉候选（具体模型）
   * @param {function} [cfg.onConfirm] (model:string)=>void
   * @param {function} [cfg.onCancel] ()=>void
   */
  function pickConcreteModelDialog(cfg) {
    const c = cfg || {};
    const tt = (typeof window.t === 'function') ? window.t : (k => k);

    const overlay = document.createElement('div');
    overlay.className = 'modal-overlay';
    overlay.style.cssText = 'display:flex;align-items:center;justify-content:center;position:fixed;inset:0;background:rgba(0,0,0,.5);z-index:10000;';

    const box = document.createElement('div');
    box.style.cssText = 'background:var(--bg-card,#fff);color:var(--text-main,#111);border-radius:10px;padding:18px;min-width:320px;max-width:440px;box-shadow:0 8px 28px rgba(0,0,0,.25);';

    const heading = document.createElement('div');
    heading.textContent = c.title || tt('channels.test.pickModel') || '请输入一个具体测试模型';
    heading.style.cssText = 'font-weight:600;margin-bottom:12px;';
    box.appendChild(heading);

    const container = document.createElement('div');
    container.id = 'pickConcreteModelCombo';
    box.appendChild(container);

    const btnRow = document.createElement('div');
    btnRow.style.cssText = 'display:flex;justify-content:flex-end;gap:8px;margin-top:14px;';
    const cancelBtn = document.createElement('button');
    cancelBtn.textContent = tt('common.cancel') || '取消';
    cancelBtn.className = 'btn btn-secondary';
    const confirmBtn = document.createElement('button');
    confirmBtn.textContent = tt('common.confirm') || '确定';
    confirmBtn.className = 'btn btn-primary';
    btnRow.append(cancelBtn, confirmBtn);
    box.appendChild(btnRow);
    overlay.appendChild(box);
    document.body.appendChild(overlay);

    let combobox = null;
    if (typeof window.createSearchableCombobox === 'function') {
      combobox = window.createSearchableCombobox({
        container: container,
        inputId: 'pickConcreteModelInput',
        dropdownId: 'pickConcreteModelDropdown',
        placeholder: c.placeholder || tt('channels.test.modelInputPlaceholder') || '输入或选择模型',
        minWidth: 280,
        allowCustomInput: true,
        getOptions: () => (c.options || []).map(name => ({ value: name, label: name })),
        onSelect: () => {}
      });
    }
    const input = document.getElementById('pickConcreteModelInput');
    setTimeout(() => { try { input && input.focus(); } catch (_) {} }, 0);

    function close() {
      if (combobox && typeof combobox.destroy === 'function') combobox.destroy();
      if (overlay.parentNode) overlay.parentNode.removeChild(overlay);
    }
    function confirm() {
      const v = (input ? input.value : '').trim();
      if (!v) { try { input && input.focus(); } catch (_) {} return; }
      if (typeof window.isModelPattern === 'function' && window.isModelPattern(v)) {
        if (typeof window.showError === 'function') {
          window.showError(tt('channels.test.modelNameNoWildcard') || '模型名不能包含通配符 * 或 ?，请输入具体模型名');
        }
        return;
      }
      close();
      if (typeof c.onConfirm === 'function') c.onConfirm(v);
    }
    confirmBtn.addEventListener('click', confirm);
    cancelBtn.addEventListener('click', () => { close(); if (typeof c.onCancel === 'function') c.onCancel(); });
    overlay.addEventListener('mousedown', (e) => { if (e.target === overlay) { close(); if (typeof c.onCancel === 'function') c.onCancel(); } });
  }

  window.pickConcreteModelDialog = pickConcreteModelDialog;
})();
