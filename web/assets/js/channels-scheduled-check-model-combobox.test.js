const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const protocolSource = fs.readFileSync(path.join(__dirname, 'channels-protocols.js'), 'utf8');
const modalsSource = fs.readFileSync(path.join(__dirname, 'channels-modals.js'), 'utf8');

function createHarness({ modelValue = '', checked = true, hidden = false, models = [] } = {}) {
  const wrapper = { hidden };
  const hiddenInput = { value: modelValue };
  const visibleInput = { value: '', disabled: false, dataset: {}, addEventListener() {} };
  const dropdown = { dataset: {}, style: {}, parentElement: null };
  const checkbox = { checked, dataset: {}, addEventListener() {} };
  const hint = {
    textContent: '',
    setAttribute() {}
  };

  const calls = [];
  let comboboxInstance = null;

  const sandbox = {
    console,
    redirectTableData: models.map((model) => ({ model, redirect_model: '' })),
    createSearchableCombobox(config) {
      calls.push(config);
      comboboxInstance = {
        setValue(value, label) {
          hiddenInput.value = value;
          visibleInput.value = label;
        },
        refresh() {},
        getInput() {
          return visibleInput;
        }
      };
      return comboboxInstance;
    },
    document: {
      body: {},
      getElementById(id) {
        switch (id) {
          case 'channelScheduledCheckModelWrapper':
            return wrapper;
          case 'channelScheduledCheckModel':
            return hiddenInput;
          case 'channelScheduledCheckModelInput':
            return visibleInput;
          case 'channelScheduledCheckModelDropdown':
            return dropdown;
          case 'channelScheduledCheckEnabled':
            return checkbox;
          case 'channelScheduledCheckModelHint':
            return hint;
          default:
            return null;
        }
      }
    },
    window: {
      t(key) {
        const messages = {
          'channels.scheduledCheckModelDefault': '默认首个模型',
          'channels.scheduledCheckModelHint': '仅用于定时检测，留空表示默认首个模型',
          'channels.scheduledCheckModelFallback': '当前检测模型已失效，已回退为默认首个模型'
        };
        return messages[key] || key;
      }
    }
  };

  vm.createContext(sandbox);
  vm.runInContext(`${protocolSource}
${modalsSource}
this.__scheduledCheckModelTest = {
  syncScheduledCheckModelState,
  getScheduledCheckModelOptions
};`, sandbox);

  return {
    api: sandbox.__scheduledCheckModelTest,
    calls,
    wrapper,
    hiddenInput,
    visibleInput,
    checkbox,
    hint,
    getCombobox: () => comboboxInstance
  };
}

test('syncScheduledCheckModelState 复用共享组合框并在空值时显示默认首个模型', () => {
  const harness = createHarness({
    modelValue: '',
    checked: true,
    hidden: false,
    models: ['claude-3-7-sonnet', 'gpt-4.1']
  });

  harness.api.syncScheduledCheckModelState();

  assert.equal(harness.calls.length, 1);
  const [config] = harness.calls;
  assert.equal(config.attachMode, true);
  assert.equal(config.inputId, 'channelScheduledCheckModelInput');
  assert.equal(config.dropdownId, 'channelScheduledCheckModelDropdown');
  assert.deepEqual(
    JSON.parse(JSON.stringify(config.getOptions())),
    [
      { value: '', label: '默认首个模型' },
      { value: 'claude-3-7-sonnet', label: 'claude-3-7-sonnet' },
      { value: 'gpt-4.1', label: 'gpt-4.1' }
    ]
  );
  assert.equal(harness.hiddenInput.value, '');
  assert.equal(harness.visibleInput.value, '默认首个模型');
  assert.equal(harness.visibleInput.disabled, false);
  assert.equal(harness.hint.textContent, '仅用于定时检测，留空表示默认首个模型');
});

test('syncScheduledCheckModelState 遇到失效模型时回退为空值并禁用隐藏控件', () => {
  const harness = createHarness({
    modelValue: 'missing-model',
    checked: false,
    hidden: true,
    models: ['claude-3-7-sonnet']
  });

  harness.api.syncScheduledCheckModelState();

  assert.equal(harness.hiddenInput.value, '');
  assert.equal(harness.visibleInput.value, '默认首个模型');
  assert.equal(harness.visibleInput.disabled, true);
  assert.equal(harness.hint.textContent, '当前检测模型已失效，已回退为默认首个模型');
});
