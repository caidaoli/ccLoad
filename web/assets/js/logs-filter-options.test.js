const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const logsSource = fs.readFileSync(path.join(__dirname, 'logs.js'), 'utf8');

function extractFunction(source, name) {
  const signature = `function ${name}`;
  const start = source.indexOf(signature);
  assert.ok(start >= 0, `缺少函数 ${name}`);

  const braceStart = source.indexOf('{', start);
  assert.ok(braceStart >= 0, `函数 ${name} 缺少起始大括号`);

  let depth = 0;
  for (let i = braceStart; i < source.length; i++) {
    const char = source[i];
    if (char === '{') depth++;
    if (char === '}') depth--;
    if (depth === 0) {
      return source.slice(start, i + 1);
    }
  }

  assert.fail(`函数 ${name} 大括号未闭合`);
}

function makeSandbox(initialChannels = [], initialModels = []) {
  const sandbox = {
    refreshCount: 0,
    logsChannelNameCombobox: { refresh() { sandbox.refreshCount++; } },
    logsModelCombobox: { refresh() { sandbox.refreshCount++; } }
  };
  sandbox.window = sandbox;
  sandbox.logsChannels = initialChannels;
  sandbox.availableLogsModels = initialModels;

  vm.runInNewContext(extractFunction(logsSource, 'mergeLogsFilterOptions'), sandbox);
  return sandbox;
}

// 沙箱里 push 出的对象/数组属于另一个 realm，deepStrictEqual 会因原型不同而失败，
// 故统一用 JSON 序列化比较值。
function assertJSONEqual(actual, expected, msg) {
  assert.equal(JSON.stringify(actual), JSON.stringify(expected), msg);
}

test('日志数据中新出现的渠道/模型合并进下拉并去重', () => {
  const sandbox = makeSandbox([{ id: 1, name: 'old-channel' }], ['old-model']);

  sandbox.mergeLogsFilterOptions([
    { channel_id: 2, channel_name: 'new-channel', model: 'new-model' },
    { channel_id: 2, channel_name: 'new-channel', model: 'new-model' }, // 重复，应忽略
    { channel_id: 1, channel_name: 'old-channel', model: 'old-model' }  // 已存在，应忽略
  ]);

  assertJSONEqual(sandbox.logsChannels, [
    { id: 1, name: 'old-channel' },
    { id: 2, name: 'new-channel' }
  ]);
  assertJSONEqual(sandbox.availableLogsModels, ['old-model', 'new-model']);
  assert.ok(sandbox.refreshCount > 0, '有新增项时应刷新下拉');
});

test('actual_model（重定向后实际模型）也并入模型下拉', () => {
  const sandbox = makeSandbox([], []);

  sandbox.mergeLogsFilterOptions([
    { channel_id: 3, channel_name: 'ch', model: 'req-model', actual_model: 'real-model' }
  ]);

  assertJSONEqual(sandbox.availableLogsModels, ['req-model', 'real-model']);
});

test('无新增项时不触发下拉刷新', () => {
  const sandbox = makeSandbox([{ id: 1, name: 'ch' }], ['m']);

  sandbox.mergeLogsFilterOptions([{ channel_id: 1, channel_name: 'ch', model: 'm' }]);

  assert.equal(sandbox.refreshCount, 0);
});

test('空数据/缺字段安全跳过', () => {
  const sandbox = makeSandbox([], []);

  sandbox.mergeLogsFilterOptions([]);
  sandbox.mergeLogsFilterOptions(null);
  sandbox.mergeLogsFilterOptions([{ channel_name: '', model: '' }, {}]);

  assertJSONEqual(sandbox.logsChannels, []);
  assertJSONEqual(sandbox.availableLogsModels, []);
  assert.equal(sandbox.refreshCount, 0);
});
