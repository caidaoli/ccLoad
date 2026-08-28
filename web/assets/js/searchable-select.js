(function (root, factory) {
  const api = factory(root);
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = api;
    return;
  }
  Object.assign(root, api);
  api.startSearchableSelectEnhancement();
})(typeof window !== 'undefined' ? window : globalThis, function (root) {
  const enhancedSelects = new WeakMap();
  let nextSelectID = 0;
  let documentObserver = null;

  function selectOptions(select) {
    return Array.from(select?.options || [])
      .filter(option => option.hidden !== true)
      .map((option) => ({
        value: String(option.value ?? ''),
        label: String(option.label || option.textContent || option.text || option.value || '').trim(),
        disabled: option.disabled === true
      }));
  }

  function selectedOption(select) {
    const value = String(select?.value ?? '');
    return selectOptions(select).find(option => option.value === value) || null;
  }

  function selectedLabel(select) {
    return selectedOption(select)?.label || '';
  }

  function findPropertyDescriptor(target, propertyName) {
    let prototype = Object.getPrototypeOf(target);
    while (prototype) {
      const descriptor = Object.getOwnPropertyDescriptor(prototype, propertyName);
      if (descriptor) return descriptor;
      prototype = Object.getPrototypeOf(prototype);
    }
    return null;
  }

  function observeProperty(target, propertyName, onSet) {
    const descriptor = findPropertyDescriptor(target, propertyName);
    if (!descriptor?.get || !descriptor?.set) return;
    Object.defineProperty(target, propertyName, {
      configurable: true,
      enumerable: descriptor.enumerable,
      get() {
        return descriptor.get.call(this);
      },
      set(value) {
        descriptor.set.call(this, value);
        onSet();
      }
    });
  }

  function associatedLabel(select) {
    if (select.labels?.length) return select.labels[0];
    if (!select.id || !select.ownerDocument?.querySelectorAll) return null;
    return Array.from(select.ownerDocument.querySelectorAll('label'))
      .find(label => label.htmlFor === select.id) || null;
  }

  function makeControlID(select) {
    nextSelectID += 1;
    const base = String(select.id || `select-${nextSelectID}`).replace(/[^a-zA-Z0-9_-]/g, '-');
    return `${base}-searchable`;
  }

  function dispatchSelectionEvents(select) {
    const EventConstructor = select.ownerDocument?.defaultView?.Event || root.Event;
    if (typeof EventConstructor !== 'function') return;
    select.dispatchEvent(new EventConstructor('input', { bubbles: true }));
    select.dispatchEvent(new EventConstructor('change', { bubbles: true }));
  }

  function dispatchPointerDown(select) {
    const EventConstructor = select.ownerDocument?.defaultView?.PointerEvent ||
      select.ownerDocument?.defaultView?.Event || root.PointerEvent || root.Event;
    if (typeof EventConstructor !== 'function') return;
    select.dispatchEvent(new EventConstructor('pointerdown', { bubbles: true, cancelable: true }));
  }

  function enhanceNativeSelect(select) {
    if (!select || select.multiple || enhancedSelects.has(select)) {
      return enhancedSelects.get(select) || null;
    }
    if (typeof root.createSearchableCombobox !== 'function') return null;
    // 仅增强仍在文档树中的 select:observer 回调派发时,插入方可能已同步摘除
    // 该节点(如第三方脚本的临时探测节点),此时 attach 模式找不到 input/dropdown
    // 且 wrapper 会挂在游离节点上;节点重新插入时的新 mutation 会再触发增强。
    if (!select.isConnected) return null;

    const document = select.ownerDocument || root.document;
    if (!document?.createElement || typeof select.after !== 'function') return null;

    const controlID = makeControlID(select);
    const wrapper = document.createElement('div');
    wrapper.className = 'filter-combobox-wrapper searchable-select-wrapper';
    for (const className of select.classList || []) {
      if (className.startsWith('filter-control--') || className === 'field-grow') {
        wrapper.classList.add(className);
      }
    }
    if (select.matches?.('.channel-batch-select, .modal-inline-select, .settings-input--select')) {
      wrapper.classList.add('searchable-select-wrapper--auto');
    }

    const input = document.createElement('input');
    input.id = `${controlID}-input`;
    input.type = 'text';
    input.className = `${select.className || 'form-input'} filter-combobox searchable-select-input`;
    input.autocomplete = 'off';
    input.spellcheck = false;

    const dropdown = document.createElement('div');
    dropdown.id = `${controlID}-dropdown`;
    dropdown.className = 'filter-dropdown searchable-select-dropdown';
    dropdown.setAttribute('role', 'listbox');

    const label = associatedLabel(select);
    const labelledBy = select.getAttribute?.('aria-labelledby');
    const ariaLabel = select.getAttribute?.('aria-label') || select.title || '';
    if (labelledBy) {
      input.setAttribute('aria-labelledby', labelledBy);
    } else if (label) {
      if (!label.id) label.id = `${controlID}-label`;
      input.setAttribute('aria-labelledby', label.id);
    } else if (ariaLabel) {
      input.setAttribute('aria-label', ariaLabel);
    }
    const describedBy = select.getAttribute?.('aria-describedby');
    if (describedBy) input.setAttribute('aria-describedby', describedBy);
    if (select.title) input.title = select.title;

    wrapper.append(input, dropdown);
    select.after(wrapper);
    select.classList?.add('searchable-select-native');
    select.setAttribute?.('aria-hidden', 'true');
    select.tabIndex = -1;
    if (label) label.htmlFor = input.id;

    let combobox = null;
    const syncFromSelect = () => {
      const option = selectedOption(select);
      combobox?.setValue(option?.value || '', option?.label || '');
      combobox?.refresh();
      input.disabled = select.disabled === true;
      input.setAttribute('aria-required', select.required ? 'true' : 'false');
      if (select.disabled) input.setAttribute('aria-disabled', 'true');
      else input.removeAttribute('aria-disabled');
      if (select.validity?.valid !== false) input.removeAttribute('aria-invalid');
    };

    combobox = root.createSearchableCombobox({
      attachMode: true,
      inputId: input.id,
      dropdownId: dropdown.id,
      initialValue: String(select.value ?? ''),
      initialLabel: selectedLabel(select),
      allowCustomInput: false,
      showAllOptionsOnOpen: true,
      getOptions: () => selectOptions(select),
      onSelect: (value) => {
        dispatchPointerDown(select);
        const previousValue = String(select.value ?? '');
        select.value = value;
        syncFromSelect();
        if (String(select.value ?? '') !== previousValue) dispatchSelectionEvents(select);
      }
    });
    if (!combobox) {
      wrapper.remove();
      select.classList?.remove('searchable-select-native');
      select.removeAttribute?.('aria-hidden');
      return null;
    }

    const instance = { select, input, dropdown, wrapper, combobox, sync: syncFromSelect };
    enhancedSelects.set(select, instance);

    observeProperty(select, 'value', syncFromSelect);
    observeProperty(select, 'selectedIndex', syncFromSelect);
    observeProperty(select, 'disabled', syncFromSelect);

    input.addEventListener('mousedown', syncFromSelect, true);
    select.addEventListener('change', syncFromSelect);
    select.addEventListener('invalid', () => {
      input.setAttribute('aria-invalid', 'true');
      root.setTimeout?.(() => input.focus(), 0);
    });
    select.form?.addEventListener('reset', () => root.queueMicrotask?.(syncFromSelect));

    if (typeof root.MutationObserver === 'function') {
      const observer = new root.MutationObserver(syncFromSelect);
      observer.observe(select, {
        attributes: true,
        attributeFilter: ['disabled', 'required', 'value', 'selected', 'label'],
        childList: true,
        subtree: true,
        characterData: true
      });
      instance.observer = observer;
    }

    syncFromSelect();
    return instance;
  }

  function enhanceNativeSelects(scope) {
    if (!scope) return [];
    const selects = [];
    if (scope.matches?.('select:not([multiple])')) selects.push(scope);
    if (scope.querySelectorAll) selects.push(...scope.querySelectorAll('select:not([multiple])'));
    return selects.map(enhanceNativeSelect).filter(Boolean);
  }

  function startSearchableSelectEnhancement() {
    const document = root.document;
    if (!document || documentObserver) return;
    enhanceNativeSelects(document);
    if (typeof root.MutationObserver !== 'function' || !document.documentElement) return;

    documentObserver = new root.MutationObserver((records) => {
      records.forEach((record) => {
        record.addedNodes.forEach(node => enhanceNativeSelects(node));
      });
    });
    documentObserver.observe(document.documentElement, { childList: true, subtree: true });
  }

  return {
    enhanceNativeSelect,
    enhanceNativeSelects,
    selectOptions,
    selectedLabel,
    startSearchableSelectEnhancement
  };
});
