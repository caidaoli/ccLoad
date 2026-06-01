const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const stateSource = fs.readFileSync(path.join(__dirname, 'channels-state.js'), 'utf8');
const protocolSource = fs.readFileSync(path.join(__dirname, 'channels-protocols.js'), 'utf8');
const modalsSource = fs.readFileSync(path.join(__dirname, 'channels-modals.js'), 'utf8');

function createHarness() {
  const sandbox = {
    console,
    document: {
      querySelector() {
        return null;
      },
      getElementById() {
        return null;
      }
    },
    localStorage: {
      getItem() {
        return null;
      }
    },
    window: {}
  };
  sandbox.window = sandbox.window || {};

  vm.createContext(sandbox);
  vm.runInContext(`${stateSource}
${protocolSource}
${modalsSource}
this.__fetchModelsTest = {
  mergeModelRowsWithFetchedModels
};`, sandbox);

  return sandbox.__fetchModelsTest;
}

test('获取模型合并新模型时保留已有模型和重定向配置', () => {
  const { mergeModelRowsWithFetchedModels } = createHarness();

  const currentRows = [
    { model: 'kiro-opus-4-8', redirect_model: 'claude-opus-4.8-thinking' },
    { model: 'gemini-3.5-flash', redirect_model: 'claude-haiku-4.5' }
  ];
  const fetchedModels = [
    'claude-opus-4.8-thinking',
    'kiro-opus-4-8',
    { model: 'claude-sonnet-4.6', redirect_model: 'claude-sonnet-4.6-thinking' }
  ];

  const result = mergeModelRowsWithFetchedModels(currentRows, fetchedModels);

  assert.deepEqual(JSON.parse(JSON.stringify(result.rows)), [
    { model: 'kiro-opus-4-8', redirect_model: 'claude-opus-4.8-thinking' },
    { model: 'gemini-3.5-flash', redirect_model: 'claude-haiku-4.5' },
    { model: 'claude-opus-4.8-thinking', redirect_model: 'claude-opus-4.8-thinking' },
    { model: 'claude-sonnet-4.6', redirect_model: 'claude-sonnet-4.6-thinking' }
  ]);
  assert.equal(result.added, 2);
  assert.equal(result.removed, 0);
});
