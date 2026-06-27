const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const script = fs.readFileSync(path.join(__dirname, 'tokens.js'), 'utf8');

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

test('tokens.js 并发上限输入只接受非负整数且创建更新共用同一解析逻辑', () => {
  const sandbox = {
    t(key) {
      return key;
    }
  };
  vm.runInNewContext(extractFunction(script, 'parseMaxConcurrencyInput'), sandbox);
  const normalize = (value) => JSON.parse(JSON.stringify(value));

  assert.deepEqual(normalize(sandbox.parseMaxConcurrencyInput('')), { value: 0 });
  assert.deepEqual(normalize(sandbox.parseMaxConcurrencyInput('0')), { value: 0 });
  assert.deepEqual(normalize(sandbox.parseMaxConcurrencyInput(' 1e2 ')), { value: 100 });
  assert.deepEqual(normalize(sandbox.parseMaxConcurrencyInput('3')), { value: 3 });
  assert.deepEqual(normalize(sandbox.parseMaxConcurrencyInput('1.9')), { error: 'tokens.msg.maxConcurrencyInteger' });
  assert.deepEqual(normalize(sandbox.parseMaxConcurrencyInput('-0.5')), { error: 'tokens.msg.maxConcurrencyInteger' });
  assert.deepEqual(normalize(sandbox.parseMaxConcurrencyInput('-1')), { error: 'tokens.msg.maxConcurrencyInteger' });
});
